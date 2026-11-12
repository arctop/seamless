package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/backup"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// fakeBackups is the console's BackupsView in tests. Refresh re-reads the
// stored override and merges it over base exactly as the service does
// (backup.Effective), so a save or a reset round-trips through the real store
// and the real precedence rule; the commands record what they were asked.
type fakeBackups struct {
	mu      sync.Mutex
	db      *sql.DB
	base    config.Backup
	release bool
	dataDir string

	status  backup.Status
	listing backup.Listing
	listErr error
	// checks answers CheckFolder and PrepareFolder by folder; any other is
	// ready, with 212 GB free.
	checks map[string]backup.FolderCheck
	sugg   []backup.Suggestion
	digest string

	nowErr, stageErr, cancelErr, dismissErr error

	staged   []backup.RestoreRequest
	deleted  []string
	prepared []string
	nows     int
	retries  int
	cancels  int
}

var _ BackupsView = (*fakeBackups)(nil)

func (f *fakeBackups) Status() backup.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeBackups) Refresh(ctx context.Context) (backup.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, _, err := store.BackupOverride(ctx, f.db)
	if err != nil {
		o = config.BackupOverride{}
	}
	f.status.Settings = backup.Effective(f.base, o, f.release, f.dataDir)
	return f.status, nil
}

func (f *fakeBackups) BackUpNow(context.Context) (backup.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nows++
	if f.nowErr != nil {
		return f.status, f.nowErr
	}
	f.status.Running = &backup.Running{Kind: archive.KindManual, StartedAt: time.Now()}
	return f.status, nil
}

func (f *fakeBackups) RetryNow(context.Context) (backup.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retries++
	if f.nowErr != nil {
		return f.status, f.nowErr
	}
	f.status.Running = &backup.Running{Kind: archive.KindScheduled, Scheduled: true, StartedAt: time.Now()}
	return f.status, nil
}

func (f *fakeBackups) List(context.Context) (backup.Listing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listing, f.listErr
}

func (f *fakeBackups) CheckFolder(dir string) backup.FolderCheck {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.checks[dir]; ok {
		return c
	}
	return backup.FolderCheck{Dir: dir, State: backup.FolderReady, Free: 212 << 30, FreeKnown: true}
}

func (f *fakeBackups) PrepareFolder(dir string) backup.FolderCheck {
	c := f.CheckFolder(dir)
	f.mu.Lock()
	f.prepared = append(f.prepared, dir)
	f.mu.Unlock()
	return c
}

func (f *fakeBackups) Suggestions() []backup.Suggestion { return f.sugg }

func (f *fakeBackups) Delete(_ context.Context, ref, by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.listing.Find(ref); !ok {
		return backup.ErrNoSuchBackup
	}
	f.deleted = append(f.deleted, ref+" by "+by)
	return nil
}

func (f *fakeBackups) StageRestore(_ context.Context, req backup.RestoreRequest) (backup.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stageErr != nil {
		return f.status, f.stageErr
	}
	e, ok := f.listing.Find(req.Ref)
	if !ok {
		return f.status, backup.ErrNoSuchBackup
	}
	f.staged = append(f.staged, req)
	f.status.Restore.Staging = &backup.Staging{Mode: req.Mode, Backup: backup.BackupRef{Name: e.Name, CreatedAt: e.CreatedAt},
		Step: backup.StepChecking, StartedAt: time.Now()}
	return f.status, nil
}

func (f *fakeBackups) CancelRestore(context.Context) (backup.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	if f.cancelErr != nil {
		return f.status, f.cancelErr
	}
	f.status.Restore.Pending = nil
	return f.status, nil
}

func (f *fakeBackups) DismissResult(context.Context) (backup.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dismissErr != nil {
		return f.status, f.dismissErr
	}
	f.status.Restore.Last, f.status.Restore.Failed = nil, nil
	return f.status, nil
}

func (f *fakeBackups) Digest(_ context.Context, ref string) (backup.Entry, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.listing.Find(ref)
	if !ok {
		return backup.Entry{}, "", backup.ErrNoSuchBackup
	}
	return e, f.digest, nil
}

// backupsConsole is a console with a fake backup service, at Basic, the
// level the section belongs to.
type backupsConsole struct {
	db  *sql.DB
	mux *http.ServeMux
	fb  *fakeBackups
	svc *Service
}

func newBackupsConsole(t *testing.T, fb *fakeBackups) *backupsConsole {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	dataDir := t.TempDir()
	fb.db, fb.dataDir = db, dataDir
	fb.status.Settings = backup.Effective(fb.base, config.BackupOverride{}, fb.release, dataDir)
	if fb.digest == "" {
		fb.digest = strings.Repeat("ab", 32)
	}
	svc, err := New(Config{DB: db, Events: events.NewRecorder(db), APIKey: testKey, Level: "basic", DataDir: dataDir, Backups: fb})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return &backupsConsole{db: db, mux: mux, fb: fb, svc: svc}
}

func (bc *backupsConsole) local() string { return filepath.Join(bc.fb.dataDir, backup.LocalDirName) }

// page renders Settings > Backups with query (after s=backups).
func (bc *backupsConsole) page(t *testing.T, query string) string {
	t.Helper()
	rr := getPeek(t, bc.mux, "/console/settings?s=backups"+query)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	return rr.Body.String()
}

// post submits a Backups form with the key, answering the redirect's flash.
func (bc *backupsConsole) post(t *testing.T, path string, form url.Values) (notice, flashErr string) {
	t.Helper()
	rr := postForm(bc.mux, path, form.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code, rr.Body.String())
	u, err := url.Parse(rr.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "backups", u.Query().Get("s"), "back to the section")
	require.Equal(t, "backups", u.Fragment)
	return u.Query().Get("notice"), u.Query().Get("error")
}

// backupEntry is a listed backup of kind made at, holding m.
func backupEntry(where, name, kind string, at time.Time, m *archive.Manifest) backup.Entry {
	e := backup.Entry{Ref: where + ":" + name, Where: where, Name: name, Path: "/backups/" + name, Size: 4 << 20,
		ModTime: at, Kind: kind, CreatedAt: at, Manifest: m}
	e.Parsed, e.Named = backup.ParseName(name)
	return e
}

// sampleManifest holds two projects and the global scope.
func sampleManifest(at time.Time, kind string) *archive.Manifest {
	return &archive.Manifest{
		FormatVersion: 1, SchemaVersion: 38, SeamlessdVersion: "0.8.1", CreatedAt: at, SourceHost: "studio",
		IncludesDB: true, Kind: kind, InstallID: "ours",
		Projects: []archive.ProjectCounts{
			{Slug: "app", Memories: 10, Notes: 4, Tasks: 3, Sessions: 1200},
			{Slug: "docs", Name: "Docs site", Memories: 2, Notes: 1, Sessions: 3},
			{Slug: "", Memories: 1},
		},
	}
}

// sectionOf cuts the Backups section out of a page.
func sectionOf(t *testing.T, page string) string {
	t.Helper()
	sec := regexp.MustCompile(`(?s)<section class="settings-zone settings-section bk-section" id="backups".*?\n</section>`).FindString(page)
	require.NotEmpty(t, sec, "the Backups section renders")
	return sec
}

// With no backup service in the process the section says so, and every POST
// answers with the same error flash.
func TestBackupsSection_NotRunning(t *testing.T) {
	_, mux := newConsoleLevel(t, config.Features{}, "basic")
	page := getPeek(t, mux, "/console/settings?s=backups").Body.String()
	require.Contains(t, page, `id="backups"`)
	require.Contains(t, page, "Backups are not running in this process")
	require.NotContains(t, page, `action="/console/settings/backups/now"`)
	for _, path := range []string{"/console/settings/backups", "/console/settings/backups/reset", "/console/settings/backups/now",
		"/console/settings/backups/delete", "/console/settings/backups/restore/cancel", "/console/settings/backups/restore/dismiss"} {
		rr := postForm(mux, path, "")
		require.Equal(t, http.StatusSeeOther, rr.Code, path)
		require.Contains(t, rr.Header().Get("Location"), "error="+url.QueryEscape(backupsNotRunning), path)
	}
	rr := getPeek(t, mux, "/console/settings/backups/folder?dir=/tmp")
	require.Equal(t, http.StatusNotFound, rr.Code)
	// No alert, no Your setup row without a service.
	setup := getPeek(t, mux, "/console/settings?s=setup").Body.String()
	require.NotContains(t, setup, "<dt>Backups</dt>")
	require.NotContains(t, setup, `id="backup-alert"`)
}

// The status card in each state: its badge is a word, the lines say what is
// on, the newest backup, what comes next, and what is wrong.
func TestBackupsSection_StatusCard(t *testing.T) {
	now := time.Now()
	on := true
	bc := newBackupsConsole(t, &fakeBackups{base: config.Backup{Auto: &on}})
	bc.fb.status.LastBackup = &backup.Record{Name: "seamless-studio-20261110T030000Z-auto.tar.gz", At: now.Add(-3 * time.Hour),
		Bytes: 4300000, Memories: 412, Notes: 96, Projects: 7}
	bc.fb.status.NextAt = now.Add(21 * time.Hour)
	sec := sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, "Automatic backups are on: every day, keeping the newest 7.")
	require.Contains(t, sec, `<span class="badge ok">On</span>`)
	require.Contains(t, sec, "Last backup 3h ago, 4.1 MB: 412 memories and 96 notes in 7 projects.")
	require.Contains(t, sec, "Next backup in 2")
	require.Contains(t, sec, "Backups go to")
	require.Contains(t, sec, bc.local())
	require.Contains(t, sec, "Seamless&rsquo;s data folder")
	require.Regexp(t, `<button class="btn primary" type="submit">.*Back up now</button>`, sec)

	bc.fb.status.UpToDate = true
	require.Contains(t, sectionOf(t, bc.page(t, "")), "Up to date: nothing has changed since the last backup. Next check in ")

	// Running: Back up now waits, and the section says so to its script.
	bc.fb.status.Running = &backup.Running{Kind: archive.KindManual, StartedAt: now}
	sec = sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, "Backing up… started just now")
	require.Contains(t, sec, `<span class="badge accent">Backing up</span>`)
	require.Contains(t, sec, `disabled title="A backup is running"`)
	require.Contains(t, sec, "data-backup-running")
	bc.fb.status.Running = nil

	// Failing: the streak, the daemon's own sentence, and Try again.
	bc.fb.status.Failures, bc.fb.status.FailingSince = 3, now.Add(-49*time.Hour)
	bc.fb.status.LastReason, bc.fb.status.LastError = "folder-disconnected", "/Volumes/T7 is not connected"
	sec = sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, `<span class="badge warn">Failing</span>`)
	require.Contains(t, sec, "The last 3 automatic backups failed, the first 2d ago")
	require.Contains(t, sec, "/Volumes/T7 is not connected")
	require.Contains(t, sec, `<input type="hidden" name="run" value="retry">`)

	// The owner's own backup that did not finish.
	bc.fb.status.Failures = 0
	bc.fb.status.LastRun = &backup.LastRun{Kind: archive.KindManual, Outcome: backup.OutcomeFailed, At: now, Error: "the disk is full"}
	require.Contains(t, sectionOf(t, bc.page(t, "")), "Your backup just now did not finish: the disk is full")

	// Stopped.
	bc.fb.status.Stopped = true
	sec = sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, "The backup service stopped after an internal error.")
	require.Contains(t, sec, `<span class="badge danger">Stopped</span>`)
}

// Off by default on a build from source, and the precedence bar says the
// build decided.
func TestBackupsSection_OffOnASourceBuild(t *testing.T) {
	bc := newBackupsConsole(t, &fakeBackups{})
	sec := sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, "Automatic backups are off: a Seamless built from source does not back up by itself.")
	require.Contains(t, sec, `<span class="badge">Off</span>`)
	require.Contains(t, sec, "Default for this build")
	require.Contains(t, sec, "No backups yet. Make one now, or turn automatic backups on below.")
	require.Contains(t, sec, `<input type="checkbox" name="auto" value="on">`, "the switch is off")
	require.NotContains(t, sec, "Next backup")
}

// The list: newest first as the service lists them, each with its kind in
// words, what it holds, where it is from, and whether it can be restored.
func TestBackupsSection_List(t *testing.T) {
	bc := newBackupsConsole(t, &fakeBackups{})
	ctx := context.Background()
	for _, slug := range []string{"app", "docs", "new"} {
		_, err := store.EnsureProject(ctx, bc.db, slug, slug)
		require.NoError(t, err)
	}
	mem := func(slug, name string) {
		id, err := core.NewID()
		require.NoError(t, err)
		now := core.FormatTime(time.Now())
		_, err = bc.db.Exec(`INSERT INTO memories_index (id, project, kind, name, description, file_path, content_hash, created_at, updated_at, valid_from)
			VALUES (?, ?, 'gotcha', ?, 'd', ?, 'h', ?, ?, ?)`, id, slug, name, slug+"/"+name+".md", now, now, now)
		require.NoError(t, err)
	}
	for i := range 13 {
		mem("app", "m"+string(rune('a'+i)))
	}
	mem("new", "fresh")

	at := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	other := sampleManifest(at.Add(-48*time.Hour), archive.KindManual)
	other.SourceHost = "laptop"
	noDB := sampleManifest(at.Add(-72*time.Hour), "")
	noDB.IncludesDB, noDB.SchemaVersion = false, 0
	tooNew := sampleManifest(at.Add(-96*time.Hour), archive.KindScheduled)
	tooNew.SeamlessdVersion = "0.9.0"
	bc.fb.listing = backup.Listing{Dir: bc.local(), Entries: []backup.Entry{
		backupEntry(backup.WhereFolder, "seamless-studio-20261110T030000Z-auto.tar.gz", archive.KindScheduled, at, sampleManifest(at, archive.KindScheduled)),
		func() backup.Entry {
			e := backupEntry(backup.WhereFolder, "seamless-laptop-20261108T030000Z.tar.gz", archive.KindManual, at.Add(-48*time.Hour), other)
			e.FromAnother = true
			return e
		}(),
		backupEntry(backup.WhereFolder, "notes-only.tar.gz", "", at.Add(-72*time.Hour), noDB),
		func() backup.Entry {
			e := backupEntry(backup.WhereFolder, "seamless-studio-20261106T030000Z-auto.tar.gz", archive.KindScheduled, at.Add(-96*time.Hour), tooNew)
			e.TooNew = true
			return e
		}(),
		func() backup.Entry {
			e := backupEntry(backup.WhereLocal, "seamless-studio-20261105T030000Z-auto.tar.gz", archive.KindScheduled, at.Add(-120*time.Hour), nil)
			e.Dataless = true
			return e
		}(),
		backupEntry(backup.WhereLocal, "pre-update-v0.8.0-20261104T030000Z.tar.gz", archive.KindPreUpdate, at.Add(-144*time.Hour), sampleManifest(at, archive.KindPreUpdate)),
	}}
	sec := sectionOf(t, bc.page(t, ""))

	rows := regexp.MustCompile(`(?s)<li class="bk-row[^"]*" id="(backup-[0-9a-f]{12})">.*?</li>`).FindAllStringSubmatch(sec, -1)
	require.Len(t, rows, 6, "one row per backup")
	ids := map[string]bool{}
	for _, r := range rows {
		ids[r[1]] = true
	}
	require.Len(t, ids, 6, "each row has its own id, for the in-place morph")
	auto, another, notes, newer, icloud, preUpdate := rows[0][0], rows[1][0], rows[2][0], rows[3][0], rows[4][0], rows[5][0]

	require.Contains(t, auto, `<span class="badge">Automatic</span>`)
	require.Contains(t, auto, "13 memories · 5 notes · 2 projects · 1,203 sessions")
	require.Contains(t, auto, "Made by Seamless v0.8.1 on studio, schema v38")
	require.Contains(t, auto, `href="/console/settings?s=backups&amp;restore=folder%3Aseamless-studio-20261110T030000Z-auto.tar.gz#backup-restore" data-seam-query`)
	require.Contains(t, auto, `action="/console/settings/backups/delete"`)
	require.Contains(t, auto, "return confirm(")
	// The details: the backup's counts beside how each project differs now.
	require.Regexp(t, `<th scope="row">app</th><td>10</td><td>4</td><td>3</td><td>1200</td><td class="bk-vs">&#43;3 memories · −4 notes</td>`, auto)
	require.Regexp(t, `<th scope="row">Docs site \(docs\)</th>.*?<td class="bk-vs">−2 memories · −1 note</td>`, auto)
	require.Contains(t, auto, `<th scope="row">Global (no project)</th>`)
	require.Regexp(t, `<tr class="is-now"><th scope="row">new</th>.*?&#43;1 memory \(not in this backup\)</td>`, auto)

	require.Contains(t, another, `<span class="badge brand">Made by you</span>`)
	require.Contains(t, another, "from another Seamless on laptop")

	require.Contains(t, notes, `<span class="badge">Exported</span>`)
	require.Contains(t, notes, "memories and notes only")
	require.Contains(t, notes, "restore=folder%3Anotes-only.tar.gz", "a knowledge-only backup can still bring back what is missing")

	require.Contains(t, newer, "Made by a newer Seamless (v0.9.0): update to restore it.")
	require.NotContains(t, newer, "Restore&hellip;")

	require.Contains(t, icloud, "In iCloud: download it in Finder first.")
	require.NotContains(t, icloud, "Restore&hellip;")
	require.Contains(t, icloud, "in Seamless&rsquo;s data folder")

	require.Contains(t, preUpdate, `<span class="badge accent">Before updating from v0.8.0</span>`)

	// A busy service holds every Restore… back, with the reason.
	bc.fb.status.Running = &backup.Running{Kind: archive.KindManual, StartedAt: time.Now()}
	sec = sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, `disabled title="A backup is running: restore once it finishes"`)
	require.NotContains(t, sec, `restore=folder%3Aseamless-studio-20261110T030000Z-auto.tar.gz#backup-restore" data-seam-query`)

	// A list that cannot be read says so.
	bc.fb.status.Running, bc.fb.listErr = nil, errors.New("permission denied")
	require.Contains(t, sectionOf(t, bc.page(t, "")), "The list of backups could not be read: permission denied")
}

// Saving stores only what differs from the settings in force, so a value the
// file sets keeps following the file; Reset hands everything back.
func TestBackupsSave_StoresOnlyWhatChanged(t *testing.T) {
	bc := newBackupsConsole(t, &fakeBackups{base: config.Backup{Every: config.Duration(12 * time.Hour)}})
	ctx := context.Background()

	sec := sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, `<option value="12h" selected>Every 12 hours (config file)</option>`)
	require.Contains(t, sec, "Following the config file")
	require.Contains(t, sec, `value="`+bc.local()+`"`, "the folder field holds the folder in force")

	notice, flashErr := bc.post(t, "/console/settings/backups", url.Values{"auto": {"on"}, "every": {"12h"}, "keep": {"14"}, "dir": {bc.local()}})
	require.Empty(t, flashErr)
	require.Contains(t, notice, "Saved. Automatic backups are on: every 12 hours, keeping the newest 14.")
	o, found, err := store.BackupOverride(ctx, bc.db)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, o.Auto)
	require.True(t, *o.Auto)
	require.NotNil(t, o.Keep)
	require.Equal(t, 14, *o.Keep)
	require.Nil(t, o.Every, "an unchanged interval keeps following the file")
	require.Nil(t, o.Dir, "an unchanged folder keeps following the default")
	require.Empty(t, bc.fb.prepared, "an unchanged folder is not probed")

	sec = sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, "Set in this console: the switch and how many to keep")
	require.Contains(t, sec, `form="backups-reset"`)

	notice, _ = bc.post(t, "/console/settings/backups", url.Values{"auto": {"on"}, "every": {"12h"}, "keep": {"14"}, "dir": {""}})
	require.Equal(t, "Nothing changed: these are the backup settings in force.", notice, "an empty folder is Seamless's own, which is in force")

	notice, _ = bc.post(t, "/console/settings/backups/reset", url.Values{})
	require.Contains(t, notice, "The backup settings follow the config file again. Automatic backups are off")
	_, found, err = store.BackupOverride(ctx, bc.db)
	require.NoError(t, err)
	require.False(t, found)
}

// A new folder is made and probed before anything is stored, and the notice
// says the old folder's backups stay where they are.
func TestBackupsSave_NewFolder(t *testing.T) {
	bc := newBackupsConsole(t, &fakeBackups{})
	ctx := context.Background()
	dest := filepath.Join(t.TempDir(), "Seamless Backups")

	notice, flashErr := bc.post(t, "/console/settings/backups", url.Values{"every": {"24h"}, "keep": {"7"}, "dir": {dest}})
	require.Empty(t, flashErr)
	require.Contains(t, notice, "New backups go to "+dest+". The backups already in the old folder stay there.")
	require.Equal(t, []string{dest}, bc.fb.prepared)
	o, _, err := store.BackupOverride(ctx, bc.db)
	require.NoError(t, err)
	require.NotNil(t, o.Dir)
	require.Equal(t, dest, *o.Dir)

	// Choosing Seamless's own folder again stores it as the empty folder,
	// which means LocalDir whatever the file names.
	notice, _ = bc.post(t, "/console/settings/backups", url.Values{"every": {"24h"}, "keep": {"7"}, "dir": {bc.local()}})
	require.Contains(t, notice, "New backups go to "+bc.local())
	o, _, err = store.BackupOverride(ctx, bc.db)
	require.NoError(t, err)
	require.NotNil(t, o.Dir)
	require.Empty(t, *o.Dir)

	// A folder that cannot be used is refused with nothing stored.
	bad := filepath.Join(t.TempDir(), "ro")
	bc.fb.checks = map[string]backup.FolderCheck{bad: {Dir: bad, State: backup.FolderNotWritable}}
	_, flashErr = bc.post(t, "/console/settings/backups", url.Values{"every": {"24h"}, "keep": {"7"}, "dir": {bad}})
	require.Equal(t, "The folder was not saved: "+bad+" is not writable.", flashErr)
	o, _, err = store.BackupOverride(ctx, bc.db)
	require.NoError(t, err)
	require.Empty(t, *o.Dir, "nothing stored")
}

// Each refusal names the field and what it takes; nothing is stored.
func TestBackupsSave_Refusals(t *testing.T) {
	bc := newBackupsConsole(t, &fakeBackups{})
	for _, tc := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"every": {"30m"}, "keep": {"7"}, "dir": {""}}, `Every "30m" is not an interval between 1h and 720h`},
		{url.Values{"every": {"6"}, "keep": {"7"}, "dir": {""}}, `Every "6" is not an interval`},
		{url.Values{"every": {"24h"}, "keep": {"0"}, "dir": {""}}, `Keep "0" is not a number from 1 to 100`},
		{url.Values{"every": {"24h"}, "keep": {"seven"}, "dir": {""}}, `Keep "seven" is not a number`},
		{url.Values{"every": {"24h"}, "keep": {"7"}, "dir": {"backups/here"}}, "The folder must be a full path"},
		{url.Values{"every": {"24h"}, "keep": {"7"}}, "Dir is required"},
		{url.Values{"keep": {"7"}, "dir": {""}}, "Every is required"},
		{url.Values{"auto": {"yes"}, "every": {"24h"}, "keep": {"7"}, "dir": {""}}, "Auto must be on, or left out for off"},
	} {
		_, flashErr := bc.post(t, "/console/settings/backups", tc.form)
		require.Contains(t, flashErr, tc.want, tc.form.Encode())
		require.Contains(t, flashErr, "nothing was saved", tc.form.Encode())
	}
	_, found, err := store.BackupOverride(context.Background(), bc.db)
	require.NoError(t, err)
	require.False(t, found)
}

// The folder field's live check: a look in words, never a write, and
// anything that is not a full path is said so.
func TestBackupsFolderCheck(t *testing.T) {
	bc := newBackupsConsole(t, &fakeBackups{})
	check := func(dir string) backupsFolderLine {
		t.Helper()
		var got backupsFolderLine
		getJSON(t, bc.mux, "/console/settings/backups/folder?dir="+url.QueryEscape(dir), &got)
		return got
	}
	require.Equal(t, backupsFolderLine{State: "ready", Usable: true, Text: "Ready · 212.0 GB free", Dir: "/Volumes/T7/Seamless Backups"},
		check("/Volumes/T7/Seamless Backups"))
	require.Equal(t, "Seamless's data folder · ready · 212.0 GB free", check("").Text, "empty is Seamless's own folder")
	got := check("relative/dir")
	require.False(t, got.Usable)
	require.Equal(t, "not-absolute", got.State)

	bc.fb.checks = map[string]backup.FolderCheck{
		"/x/new":   {Dir: "/x/new", State: backup.FolderWillCreate},
		"/x/ro":    {Dir: "/x/ro", State: backup.FolderNotWritable},
		"/x/tcc":   {Dir: "/x/tcc", State: backup.FolderPrivacy},
		"/x/inner": {Dir: "/x/inner", State: backup.FolderInsideDataDir},
	}
	require.Equal(t, "Will be created", check("/x/new").Text)
	require.Equal(t, "Not writable", check("/x/ro").Text)
	require.Contains(t, check("/x/tcc").Text, "macOS is keeping Seamless out of /x/tcc: allow it in System Settings > Privacy & Security")
	require.Equal(t, "Inside Seamless's data folder: choose a folder outside it", check("/x/inner").Text)
	require.Empty(t, bc.fb.prepared, "the check never makes or writes a folder")

	rr := getPeek(t, bc.mux, "/console/settings/backups/folder")
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

// The folder field's chips: the places the service suggests that this
// account can write to, Seamless's own folder always, the one in force marked.
func TestBackupsSection_Chips(t *testing.T) {
	bc := newBackupsConsole(t, &fakeBackups{})
	bc.fb.sugg = []backup.Suggestion{
		{Label: "iCloud Drive", Kind: backup.PlaceICloud, Dir: "/Users/o/iCloud/Seamless Backups"},
		{Label: "Recovery", Kind: backup.PlaceVolume, Dir: "/Volumes/Recovery/Seamless Backups"},
		{Label: "Seamless's data folder", Kind: backup.PlaceLocal, Dir: bc.local()},
	}
	bc.fb.checks = map[string]backup.FolderCheck{"/Volumes/Recovery/Seamless Backups": {State: backup.FolderNotWritable}}
	sec := sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, `data-backups-chip="/Users/o/iCloud/Seamless Backups" title="iCloud Drive: /Users/o/iCloud/Seamless Backups">`)
	require.NotContains(t, sec, "Recovery", "a read-only volume is no suggestion")
	require.Contains(t, sec, `class="bk-chip is-current" data-backups-chip="`+bc.local()+`"`)
}

// Back up now and Try again start their runs; a busy service and an unknown
// run are refused in words.
func TestBackupsNow(t *testing.T) {
	bc := newBackupsConsole(t, &fakeBackups{})
	notice, _ := bc.post(t, "/console/settings/backups/now", url.Values{})
	require.Equal(t, "Backing up now. This section shows when it is done.", notice)
	require.Equal(t, 1, bc.fb.nows)
	notice, _ = bc.post(t, "/console/settings/backups/now", url.Values{"run": {"retry"}})
	require.Contains(t, notice, "Trying the automatic backup again now.")
	require.Equal(t, 1, bc.fb.retries)
	_, flashErr := bc.post(t, "/console/settings/backups/now", url.Values{"run": {"all"}})
	require.Equal(t, `run "all" is not one of: manual, retry`, flashErr)

	bc.fb.nowErr = backup.ErrBusy
	bc.fb.status.Running = &backup.Running{Kind: archive.KindManual, StartedAt: time.Now()}
	_, flashErr = bc.post(t, "/console/settings/backups/now", url.Values{})
	require.Contains(t, flashErr, "A backup is already running.")
	bc.fb.status.Running = nil
	bc.fb.status.Restore.Staging = &backup.Staging{Mode: backup.ModeMerge}
	_, flashErr = bc.post(t, "/console/settings/backups/now", url.Values{})
	require.Equal(t, "A restore is being prepared. Back up once it is done.", flashErr)
}

// Delete removes the backup the list names, as the owner, and nothing else.
func TestBackupsDelete(t *testing.T) {
	bc := newBackupsConsole(t, &fakeBackups{})
	at := time.Now().Add(-time.Hour)
	name := "seamless-studio-20261110T030000Z-auto.tar.gz"
	bc.fb.listing = backup.Listing{Dir: bc.local(), Entries: []backup.Entry{
		backupEntry(backup.WhereFolder, name, archive.KindScheduled, at, sampleManifest(at, archive.KindScheduled))}}
	notice, _ := bc.post(t, "/console/settings/backups/delete", url.Values{"ref": {"folder:" + name}})
	require.Equal(t, "Deleted "+name+".", notice)
	require.Equal(t, []string{"folder:" + name + " by owner"}, bc.fb.deleted)
	_, flashErr := bc.post(t, "/console/settings/backups/delete", url.Values{"ref": {"folder:gone.tar.gz"}})
	require.Equal(t, "That backup is not in the list any more, so nothing was deleted.", flashErr)
	_, flashErr = bc.post(t, "/console/settings/backups/delete", url.Values{})
	require.Contains(t, flashErr, "ref is required")
}

// Restore… opens the confirm card in place: the two choices, the comparison
// with now, what a restore keeps, and a knowledge-only backup offering only
// the merge.
func TestBackupsRestore_ConfirmCard(t *testing.T) {
	bc := newBackupsConsole(t, &fakeBackups{})
	at := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	noDB := sampleManifest(at, "")
	noDB.IncludesDB = false
	bc.fb.listing = backup.Listing{Dir: bc.local(), Entries: []backup.Entry{
		backupEntry(backup.WhereFolder, "seamless-studio-20261110T030000Z-auto.tar.gz", archive.KindScheduled, at, sampleManifest(at, archive.KindScheduled)),
		backupEntry(backup.WhereFolder, "notes-only.tar.gz", "", at, noDB),
	}}

	sec := sectionOf(t, bc.page(t, "&restore="+url.QueryEscape("folder:seamless-studio-20261110T030000Z-auto.tar.gz")))
	card := regexp.MustCompile(`(?s)<article class="card bk-card bk-confirm" id="backup-restore".*?</article>`).FindString(sec)
	require.NotEmpty(t, card)
	require.Contains(t, card, "Restore the backup from <time")
	require.Contains(t, card, `<input type="radio" name="mode" value="replace" checked>`)
	require.Contains(t, card, `<input type="radio" name="mode" value="merge">`)
	require.Contains(t, card, "Replace all data with this backup")
	require.Contains(t, card, "Your current data is saved as a backup first, so you can undo this.")
	require.Contains(t, card, "Only bring back what&rsquo;s missing")
	require.Contains(t, card, "Compared with this backup, this install holds 13 fewer memories, 5 fewer notes and 1,203 fewer sessions now.")
	require.Contains(t, card, "Locks, passkeys, repo routes and console settings stay as they are now, and no project&rsquo;s privacy is loosened.")
	require.Contains(t, card, "Seamless restarts to finish; agents lose it for a few seconds.")
	require.Contains(t, card, `<input type="hidden" name="ref" value="folder:seamless-studio-20261110T030000Z-auto.tar.gz">`)
	require.Regexp(t, `<button class="btn primary" type="submit" data-backups-go>.*Restore</button>`, card)
	require.NotContains(t, card, "data-passkey", "no lock: no passkey")
	require.Contains(t, card, `href="/console/settings?s=backups#backups" data-seam-query>Cancel</a>`)
	require.Contains(t, sec, `class="bk-row is-open"`, "the row it is about is marked")

	card = sectionOf(t, bc.page(t, "&restore="+url.QueryEscape("folder:notes-only.tar.gz")))
	require.Contains(t, card, `<input type="radio" name="mode" value="replace" disabled>`)
	require.Contains(t, card, `<input type="radio" name="mode" value="merge" checked>`)
	require.Contains(t, card, "This backup holds memories and notes only, not the database, so it cannot replace everything")

	card = sectionOf(t, bc.page(t, "&restore="+url.QueryEscape("folder:gone.tar.gz")))
	require.Contains(t, card, "That backup is not in the list any more")

	// Windows: the footnote names the restart the owner runs.
	defer func(old string) { backupsGOOS = old }(backupsGOOS)
	backupsGOOS = "windows"
	card = sectionOf(t, bc.page(t, "&restore="+url.QueryEscape("folder:seamless-studio-20261110T030000Z-auto.tar.gz")))
	require.Contains(t, card, "Seamless then needs a restart to finish: run <span class=\"mono\">seamlessd restart</span> within an hour.")
}

// Without a lock the console's own word is the approval: the restore stages
// with proof "owner", asked for by the console, bound to no digest.
func TestBackupsRestore_StagesWithoutAGate(t *testing.T) {
	bc := newBackupsConsole(t, &fakeBackups{})
	at := time.Now().Add(-24 * time.Hour)
	ref := "folder:seamless-studio-20261110T030000Z-auto.tar.gz"
	bc.fb.listing = backup.Listing{Dir: bc.local(), Entries: []backup.Entry{
		backupEntry(backup.WhereFolder, "seamless-studio-20261110T030000Z-auto.tar.gz", archive.KindScheduled, at, sampleManifest(at, archive.KindScheduled))}}

	notice, flashErr := bc.post(t, "/console/settings/backups/restore", url.Values{"ref": {ref}, "mode": {"replace"}})
	require.Empty(t, flashErr)
	require.Equal(t, "Restoring the backup. This section shows each step; Seamless restarts to finish.", notice)
	require.Equal(t, []backup.RestoreRequest{{Ref: ref, Mode: backup.ModeReplace, By: "console", Proof: "owner"}}, bc.fb.staged)

	_, flashErr = bc.post(t, "/console/settings/backups/restore", url.Values{"ref": {ref}, "mode": {"everything"}})
	require.Equal(t, `Mode "everything" is not one of: replace, merge.`, flashErr)
	_, flashErr = bc.post(t, "/console/settings/backups/restore", url.Values{"mode": {"merge"}})
	require.Contains(t, flashErr, "Ref is required")

	bc.fb.stageErr = backup.ErrRestorePending
	_, flashErr = bc.post(t, "/console/settings/backups/restore", url.Values{"ref": {ref}, "mode": {"merge"}})
	require.Equal(t, "Another restore is already waiting to finish; cancel it first, or restart Seamless to finish it. Nothing changed.", flashErr)

	// The passkey's begin answers only under the gate.
	req := authedReq(httptest.NewRequest(http.MethodPost, "/console/settings/backups/restore/begin", strings.NewReader("ref="+url.QueryEscape(ref)+"&mode=merge")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := do(bc.mux, req)
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Contains(t, rr.Body.String(), "needs no passkey")
}

// A restore in flight: the progress card's steps follow the service, and the
// section tells its script what to wait for.
func TestBackupsRestore_Progress(t *testing.T) {
	bc := newBackupsConsole(t, &fakeBackups{})
	at := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	ref := backup.BackupRef{Name: "seamless-studio-20261110T030000Z-auto.tar.gz", CreatedAt: at}
	steps := func(sec string) []string {
		var out []string
		for _, m := range regexp.MustCompile(`<li class="bk-step is-(\w+)"`).FindAllStringSubmatch(sec, -1) {
			out = append(out, m[1])
		}
		return out
	}

	bc.fb.status.Restore.Staging = &backup.Staging{Mode: backup.ModeReplace, Backup: ref, Step: backup.StepChecking}
	sec := sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, `data-restore-state="staging"`)
	require.Equal(t, []string{"doing", "todo", "todo", "todo", "todo"}, steps(sec))
	require.Contains(t, sec, "Checking the backup")
	require.Contains(t, sec, "Saving your previous data as a backup")

	bc.fb.status.Restore.Staging = &backup.Staging{Mode: backup.ModeMerge, Backup: ref, Step: backup.StepSaving}
	sec = sectionOf(t, bc.page(t, ""))
	require.Equal(t, []string{"done", "doing", "todo", "todo"}, steps(sec), "a merge saves today's data and has no last step")
	require.Contains(t, sec, "Saving your current data")
	require.Contains(t, sec, `aria-current="step"`)

	bc.fb.status.Restore.Staging = nil
	bc.fb.status.Restore.Pending = &backup.Pending{ID: "r1", Mode: backup.ModeReplace, Backup: ref, Stage: backup.StageStaged, RequestedAt: time.Now()}
	bc.fb.listing = backup.Listing{Dir: bc.local(), Entries: []backup.Entry{
		backupEntry(backup.WhereFolder, ref.Name, archive.KindScheduled, at, sampleManifest(at, archive.KindScheduled))}}
	sec = sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, `data-restore-state="pending"`)
	require.Contains(t, sec, `data-restore-restart="auto"`)
	require.Equal(t, []string{"done", "done", "doing", "todo", "todo"}, steps(sec))
	require.Contains(t, sec, `action="/console/settings/backups/restore/cancel"`)
	require.Contains(t, sec, `disabled title="A restore is already under way"`, "no second restore meanwhile")

	notice, _ := bc.post(t, "/console/settings/backups/restore/cancel", url.Values{})
	require.Equal(t, "The restore was cancelled. Nothing changed.", notice)
	notice, _ = bc.post(t, "/console/settings/backups/restore/cancel", url.Values{})
	require.Equal(t, "No restore was waiting to finish.", notice)

	// Windows: the restart is the owner's to run.
	defer func(old string) { backupsGOOS = old }(backupsGOOS)
	backupsGOOS = "windows"
	bc.fb.status.Restore.Pending = &backup.Pending{ID: "r1", Mode: backup.ModeReplace, Backup: ref, Stage: backup.StageStaged, RequestedAt: time.Now()}
	sec = sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, `data-restore-restart="manual"`)
	require.Contains(t, sec, "Restart Seamless within an hour to finish")
	require.Contains(t, sec, `data-copy="seamlessd restart"`)
	backupsGOOS = "darwin"

	// Back up: saving the replaced data.
	bc.fb.status.Restore.Pending = nil
	bc.fb.status.Restore.Archiving = true
	bc.fb.status.Restore.Last = &backup.Result{ID: "r1", Mode: backup.ModeReplace, Outcome: backup.RestoreDone, Backup: ref,
		Memories: 398, Notes: 94, PreviousDir: "/data/restore/r1/previous"}
	sec = sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, `data-restore-state="archiving"`)
	require.Equal(t, []string{"done", "done", "done", "done", "doing"}, steps(sec))
	require.Contains(t, sec, "(398 memories and 94 notes).")
	require.Contains(t, sec, "Your previous data is being saved as a backup")
	require.NotContains(t, sec, `action="/console/settings/backups/restore/dismiss"`, "Dismiss waits for the undo")
}

// The last restore's banner, until dismissed: what came back, the undo, and a
// restore that did not finish in the service's own words.
func TestBackupsRestore_Result(t *testing.T) {
	bc := newBackupsConsole(t, &fakeBackups{})
	at := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	safety := "seamless-studio-20261111T140200Z-before-restore.tar.gz"
	bc.fb.listing = backup.Listing{Dir: bc.local(), Entries: []backup.Entry{
		backupEntry(backup.WhereFolder, safety, archive.KindBeforeRestore, at.Add(35*time.Hour), sampleManifest(at, archive.KindBeforeRestore))}}
	ref := backup.BackupRef{Name: "seamless-studio-20261110T030000Z-auto.tar.gz", CreatedAt: at}

	bc.fb.status.Restore.Last = &backup.Result{ID: "r1", Mode: backup.ModeReplace, Outcome: backup.RestoreDone, Backup: ref,
		Memories: 398, Notes: 94, Previous: safety, Kept: backup.KeptWords{Tightened: []string{"example-finance"}}}
	sec := sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, "<strong>Restored the backup from <time")
	require.Contains(t, sec, "(398 memories and 94 notes). Your previous data is saved as a backup in Seamless&rsquo;s data folder, <span class=\"mono\">"+safety+"</span>.")
	require.Contains(t, sec, `href="/console/settings?s=backups&amp;restore=folder%3A`+safety+`#backup-restore" data-seam-query>`)
	require.Contains(t, sec, "Undo this restore")
	require.Contains(t, sec, "Kept today&#39;s stricter privacy for example-finance.")
	require.Contains(t, sec, "the last restore&rsquo;s undo", "the list marks the undo")
	require.Contains(t, sec, `action="/console/settings/backups/restore/dismiss"`)

	// The undo's confirm card says what it is.
	card := sectionOf(t, bc.page(t, "&restore="+url.QueryEscape("folder:"+safety)))
	require.Contains(t, card, "Undo the last restore")

	bc.fb.status.Restore.Last = &backup.Result{ID: "r2", Mode: backup.ModeMerge, Outcome: backup.RestoreDone, Backup: ref,
		Added: 31, Memories: 5, Notes: 2, Previous: safety}
	sec = sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, "<strong>Brought back what was missing</strong> from the backup of <time")
	require.Contains(t, sec, "31 items added, among them 5 memories and 2 notes.")

	bc.fb.status.Restore.Last = &backup.Result{ID: "r2b", Mode: backup.ModeMerge, Outcome: backup.RestoreDone, Backup: ref, Previous: safety}
	require.Contains(t, sectionOf(t, bc.page(t, "")), "<strong>Nothing was missing:</strong> the backup of <time")

	bc.fb.status.Restore.Last = &backup.Result{ID: "r3", Mode: backup.ModeReplace, Outcome: backup.RestoreFailed, Backup: ref,
		Message: "the prepared backup was missing when Seamless restarted; nothing changed"}
	sec = sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, "<strong>The restore did not finish.</strong> The prepared backup was missing when Seamless restarted; nothing changed.")
	require.Contains(t, sec, `class="bk-result is-danger"`)

	bc.fb.status.Restore.Last = &backup.Result{ID: "r4", Mode: backup.ModeReplace, Outcome: backup.RestoreExpired, Backup: ref, RequestedAt: at}
	require.Contains(t, sectionOf(t, bc.page(t, "")), "was not applied because Seamless did not restart within an hour. Nothing changed.")

	bc.fb.status.Restore.Last = &backup.Result{ID: "r5", Mode: backup.ModeMerge, Outcome: backup.RestorePartial, Backup: ref,
		Message: "the merge stopped part way (disk full): 31 items were added before it stopped; nothing was removed"}
	require.Contains(t, sectionOf(t, bc.page(t, "")), "<strong>The merge stopped partway.</strong> The merge stopped part way (disk full): 31 items were added")

	bc.fb.status.Restore.Last = nil
	bc.fb.status.Restore.Failed = &backup.StageFailure{Mode: backup.ModeReplace, Backup: ref, Reason: backup.RestoreReasonNoSpace,
		Message: "the disk lacks room for this restore"}
	sec = sectionOf(t, bc.page(t, ""))
	require.Contains(t, sec, "<strong>The restore could not be prepared.</strong> The disk lacks room for this restore. Nothing changed.")

	rr := postForm(bc.mux, "/console/settings/backups/restore/dismiss", "")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/settings?s=backups#backups", rr.Header().Get("Location"))
	require.NotContains(t, sectionOf(t, bc.page(t, "")), "could not be prepared")

	bc.fb.dismissErr = &backup.Failure{Reason: backup.RestoreReasonBusy, Message: "the data the restore replaced is still being saved as a backup"}
	_, flashErr := bc.post(t, "/console/settings/backups/restore/dismiss", url.Values{})
	require.Equal(t, "The data the restore replaced is still being saved as a backup: dismiss it once that is done.", flashErr)
}

// Every page carries the alert while automatic backups are in trouble, in
// fixed words: never the run's error text.
func TestBackupsAlert_EveryPage(t *testing.T) {
	on := true
	bc := newBackupsConsole(t, &fakeBackups{base: config.Backup{Auto: &on}})
	since := time.Now().Add(-49 * time.Hour)
	bc.fb.status.Failures, bc.fb.status.FailingSince = 2, since
	bc.fb.status.LastReason, bc.fb.status.LastError = "folder-disconnected", "/Volumes/T7 private path"

	for _, path := range []string{"/console/", "/console/memories", "/console/settings?s=experience"} {
		body := getPeek(t, bc.mux, path).Body.String()
		alert := regexp.MustCompile(`(?s)<div class="section notice update-alert" id="backup-alert".*?</div>\s*</div>`).FindString(body)
		require.NotEmpty(t, alert, path)
		require.Contains(t, alert, `data-update-banner="backups-`)
		require.Contains(t, alert, "<strong>Automatic backups are failing.</strong> The last 2 did not finish, the first 2d ago: the backup folder&#39;s disk is not connected.")
		require.Contains(t, alert, `href="/console/settings?s=backups"`)
		require.Contains(t, alert, "data-update-banner-dismiss")
		require.NotContains(t, alert, "private path", "the alert never carries the run's error text")
	}

	// One failure is not yet an alert; nor is a service with backups off.
	bc.fb.status.Failures = 1
	require.NotContains(t, getPeek(t, bc.mux, "/console/").Body.String(), `id="backup-alert"`)

	// Overdue: no confirmation for three intervals.
	bc.fb.status.Failures = 0
	bc.fb.status.LastConfirmed = time.Now().Add(-4 * 24 * time.Hour)
	body := getPeek(t, bc.mux, "/console/").Body.String()
	require.Contains(t, body, "<strong>Automatic backups are overdue.</strong> The last one finished 4d ago, and they are set to run every day.")
}

// Your setup has a Backups row linking to the section.
func TestBackupsSetupRow(t *testing.T) {
	on := true
	bc := newBackupsConsole(t, &fakeBackups{base: config.Backup{Auto: &on}})
	bc.fb.status.LastBackup = &backup.Record{At: time.Now().Add(-3 * time.Hour)}
	page := getPeek(t, bc.mux, "/console/settings?s=setup").Body.String()
	require.Contains(t, page, `<div><dt>Backups</dt><dd><span class="badge ok">on</span><span>every day, keeping the newest 7 · last backup 3h ago</span><a href="/console/settings?s=backups" data-seam-query>Settings &rsaquo; Backups</a></dd></div>`)
}

// Every Backups route is the owner's own decision: a request from a process
// an agent started is refused on each, the folder field's look included.
func TestBackupsRoutes_RefusedFromAnAgent(t *testing.T) {
	lc := newLockedConsole(t)
	fb := &fakeBackups{db: lc.db, dataDir: filepath.Join(t.TempDir(), "data"), digest: strings.Repeat("ab", 32)}
	lc.cs.cfg.Backups = fb
	sock, ok := lc.policy.PeerView(501)
	require.True(t, ok)
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/console/settings/backups/folder?dir=/tmp"},
		{http.MethodPost, "/console/settings/backups"},
		{http.MethodPost, "/console/settings/backups/reset"},
		{http.MethodPost, "/console/settings/backups/now"},
		{http.MethodPost, "/console/settings/backups/delete"},
		{http.MethodPost, "/console/settings/backups/restore/begin"},
		{http.MethodPost, "/console/settings/backups/restore"},
		{http.MethodPost, "/console/settings/backups/restore/cancel"},
		{http.MethodPost, "/console/settings/backups/restore/dismiss"},
	} {
		req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(""))
		if rt.method == http.MethodPost {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		// The owner's uid on the socket, with no process chain the kernel
		// named: FromAgent holds.
		rr := lc.do(req.WithContext(access.WithView(req.Context(), sock)))
		require.Equal(t, http.StatusForbidden, rr.Code, rt.path)
		require.Contains(t, rr.Body.String(), "the owner's decision", rt.path)
	}
	require.Zero(t, fb.nows)
	require.Empty(t, fb.staged)
}

// Under peer_auth with a project locked a restore needs a passkey: only a
// browser begins the ceremony, a restore without its assertion is refused,
// and the one that passes stages with the passkey's proof, bound to the
// digest the server read from the file.
func TestBackupsRestore_PasskeyGate(t *testing.T) {
	lc := newLockedConsole(t)
	at := time.Now().Add(-24 * time.Hour)
	name := "seamless-studio-20261110T030000Z-auto.tar.gz"
	ref := "folder:" + name
	m := sampleManifest(at, archive.KindScheduled)
	m.Projects = append(m.Projects, archive.ProjectCounts{Slug: lockedSlug, Memories: 4, Notes: 1})
	fb := &fakeBackups{db: lc.db, digest: strings.Repeat("ab", 32)}
	fb.listing = backup.Listing{Entries: []backup.Entry{backupEntry(backup.WhereFolder, name, archive.KindScheduled, at, m)}}
	lc.cs.cfg.Backups = fb

	restore := func(form url.Values) *httptest.ResponseRecorder {
		req := lc.browser(http.MethodPost, "/console/settings/backups/restore", "", form)
		req.Header.Set("Accept", "application/json")
		return lc.do(req)
	}

	// No passkey yet: the card shows the terminal's way, as the daemon's
	// account, and the ceremony will not begin.
	_, page := lc.get(t, "/console/settings?s=backups&restore="+url.QueryEscape(ref), "")
	require.Contains(t, page, "No passkey is enrolled, so a restore runs from a terminal")
	require.Contains(t, page, "sudo -u _seamless env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd restore /backups/"+name)
	require.Contains(t, page, "seamlessd restore --merge /backups/"+name)
	require.NotContains(t, page, "Restore with passkey")
	// A locked project shows its name and the backup's counts, never now's.
	require.Regexp(t, `<th scope="row">`+lockedSlug+`</th><td>4</td><td>1</td><td>0</td><td>0</td><td class="bk-vs">locked</td>`, page)
	require.Contains(t, page, "Locked projects are not compared.")
	require.NotContains(t, page, lockedSecret)

	rr := restore(url.Values{"ref": {ref}, "mode": {"merge"}})
	require.Equal(t, http.StatusForbidden, rr.Code, "no assertion, no restore")
	require.Contains(t, rr.Body.String(), "needs your passkey")
	require.Empty(t, fb.staged)

	key, err := passkeytest.New(true)
	require.NoError(t, err)
	enrollPasskeyByCode(t, lc.db, lc.svc, key, "key")
	_, page = lc.get(t, "/console/settings?s=backups&restore="+url.QueryEscape(ref), "")
	require.Contains(t, page, "Restore with passkey")
	require.Contains(t, page, "data-passkey")

	// The bearer key cannot begin one, however it is signed in.
	bearer := authedReq(httptest.NewRequest(http.MethodPost, "/console/settings/backups/restore/begin",
		strings.NewReader(url.Values{"ref": {ref}, "mode": {"merge"}}.Encode())))
	bearer.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	require.Equal(t, http.StatusForbidden, lc.do(bearer).Code)
	bearer = authedReq(httptest.NewRequest(http.MethodPost, "/console/settings/backups/restore",
		strings.NewReader(url.Values{"ref": {ref}, "mode": {"merge"}, "id": {"x"}, "response": {"{}"}}.Encode())))
	bearer.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	bearer.Header.Set("Accept", "application/json")
	require.Equal(t, http.StatusForbidden, lc.do(bearer).Code)

	begin := func(mode string) approval.Begun {
		t.Helper()
		rr := lc.do(lc.browser(http.MethodPost, "/console/settings/backups/restore/begin", "", url.Values{"ref": {ref}, "mode": {mode}}))
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var begun approval.Begun
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &begun))
		return begun
	}

	// An approval to bring back what is missing never replaces everything.
	begun := begin("merge")
	assertion, err := key.Get(begun.Options, approverOrigin)
	require.NoError(t, err)
	rr = restore(url.Values{"ref": {ref}, "mode": {"replace"}, "id": {begun.ID}, "response": {string(assertion)}})
	require.Equal(t, http.StatusConflict, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "start again")
	require.Empty(t, fb.staged)

	begun = begin("merge")
	assertion, err = key.Get(begun.Options, approverOrigin)
	require.NoError(t, err)
	rr = restore(url.Values{"ref": {ref}, "mode": {"merge"}, "id": {begun.ID}, "response": {string(assertion)}})
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Len(t, fb.staged, 1)
	got := fb.staged[0]
	require.Equal(t, ref, got.Ref)
	require.Equal(t, backup.ModeMerge, got.Mode)
	require.Equal(t, approval.ProofPasskey, got.Proof)
	require.Equal(t, fb.digest, got.ExpectSHA256, "the stage expects the digest the ceremony approved")
	require.NotEmpty(t, got.Credential)
	require.Equal(t, "console", got.By)

	// The ceremony is single use.
	rr = restore(url.Values{"ref": {ref}, "mode": {"merge"}, "id": {begun.ID}, "response": {string(assertion)}})
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Len(t, fb.staged, 1)

	// A file that changed since the ceremony began is refused.
	begun = begin("merge")
	assertion, err = key.Get(begun.Options, approverOrigin)
	require.NoError(t, err)
	fb.mu.Lock()
	fb.digest = strings.Repeat("cd", 32)
	fb.mu.Unlock()
	rr = restore(url.Values{"ref": {ref}, "mode": {"merge"}, "id": {begun.ID}, "response": {string(assertion)}})
	require.Equal(t, http.StatusConflict, rr.Code)
	require.Len(t, fb.staged, 1)
}

// The section over the real backup service, with nothing locked: Back up now
// writes a backup the list shows, Restore… opens its card, the restore stages
// on the owner's word and waits for the restart, and Cancel restore drops it.
func TestBackupsSection_RealService(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	_, err = store.EnsureProject(ctx, db, "app", "app")
	require.NoError(t, err)
	writeMemory(t, mgr, core.KindGotcha, "app", "kept-rule", "a rule worth keeping")

	folder := filepath.Join(t.TempDir(), "Seamless Backups")
	rec := events.NewRecorder(db)
	bs := backup.New(backup.Deps{DataDir: dir, DB: db, Base: config.Backup{Dir: folder}, Version: "0.8.1", Host: "studio", Events: rec})
	bctx, stop := context.WithCancel(ctx)
	bs.Start(bctx)
	t.Cleanup(func() { stop(); bs.Wait() })
	cs, err := New(Config{DB: db, Files: mgr, Events: rec, APIKey: testKey, Level: "basic", DataDir: dir, Backups: bs})
	require.NoError(t, err)
	mux := http.NewServeMux()
	cs.Register(mux)
	page := func(query string) string {
		rr := getPeek(t, mux, "/console/settings?s=backups"+query)
		require.Equal(t, http.StatusOK, rr.Code)
		return sectionOf(t, rr.Body.String())
	}

	rr := postForm(mux, "/console/settings/backups/now", "")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Eventually(t, func() bool { st := bs.Status(); return st.Running == nil && st.LastBackup != nil }, 60*time.Second, 10*time.Millisecond)
	name := bs.Status().LastBackup.Name
	sec := page("")
	require.Contains(t, sec, "Last backup just now")
	require.Contains(t, sec, `<span class="badge brand">Made by you</span>`)
	require.Contains(t, sec, "1 memory · 0 notes · 1 project")
	require.Contains(t, sec, `<th scope="row">app</th><td>1</td><td>0</td><td>0</td><td>0</td><td class="bk-vs">same</td>`)
	require.Contains(t, sec, folder)

	ref := backup.WhereFolder + ":" + name
	card := page("&restore=" + url.QueryEscape(ref))
	require.Contains(t, card, "Compared with now: the same number of memories, notes and sessions as this backup.")

	rr = postForm(mux, "/console/settings/backups/restore", url.Values{"ref": {ref}, "mode": {"merge"}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")
	require.Eventually(t, func() bool { return bs.Status().Restore.Pending != nil }, 60*time.Second, 10*time.Millisecond)
	p, err := backup.ReadPending(dir)
	require.NoError(t, err)
	require.Equal(t, "owner", p.Proof)
	require.Equal(t, "console", p.RequestedBy)
	sec = page("")
	require.Contains(t, sec, `data-restore-state="pending"`)
	require.Contains(t, sec, `action="/console/settings/backups/restore/cancel"`)

	rr = postForm(mux, "/console/settings/backups/restore/cancel", "")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), url.QueryEscape("The restore was cancelled. Nothing changed."))
	require.False(t, backup.RestorePending(dir))
	require.NotContains(t, page(""), `data-restore-state="pending"`)
}
