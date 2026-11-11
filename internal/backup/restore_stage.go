package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The steps a staging restore reports (Stager.Step), as the console's
// progress card names them.
const (
	StepChecking  = "checking"  // verifying the backup and copying it
	StepPreparing = "preparing" // a replacement: importing it beside today's data
	StepSaving    = "saving"    // a merge: backing up today's data first
)

// Stager prepares a restore while the daemon still serves: nothing in the
// live data dir changes, so a failure changes nothing. The service runs it as
// a job; the CLI runs it in the foreground.
type Stager struct {
	// DataDir is the instance root; DB is today's database (the live handle,
	// read for the kept state and the install id).
	DataDir string
	DB      *sql.DB
	// ForceLocked are the config's force locks, set only while peer_auth is
	// on; PasskeyRPID goes into a safety backup's manifest.
	ForceLocked []string
	PasskeyRPID string
	// Validate checks a prepared database the way serve's preflight would
	// (peer_auth's projectProblems); nil checks nothing more.
	Validate func(ctx context.Context, db *sql.DB) error
	// FreeBytes reads a disk's free space; nil skips the check.
	FreeBytes func(dir string) (uint64, error)
	// Version and Host go into a merge's safety backup.
	Version string
	Host    string
	Now     func() time.Time
	Log     *slog.Logger
	// Step, when set, hears each step as it begins.
	Step func(step string)

	export func(ctx context.Context, opts archive.ExportOptions) (archive.Manifest, error)
}

// StageRequest is one restore to stage.
type StageRequest struct {
	// Path is the backup file, Backup what the listing (or the CLI) knows of
	// it.
	Path   string
	Backup BackupRef
	Mode   string
	// ExpectSHA256, when set, is the digest the owner approved (the passkey
	// ceremony's): a file that changed since is refused.
	ExpectSHA256 string
	RequestedBy  string
	Proof        string
	Credential   string
}

func (st *Stager) defaults() {
	if st.Now == nil {
		st.Now = time.Now
	}
	if st.Log == nil {
		st.Log = slog.Default()
	}
	if st.export == nil {
		st.export = archive.Export
	}
	if st.Step == nil {
		st.Step = func(string) {}
	}
}

// Stage verifies the backup, prepares it under restore/<id>/, and records
// restore/pending.json as staged. It returns the record, or a *Failure with
// the owner-facing words. On failure everything it made is removed.
func (st Stager) Stage(ctx context.Context, req StageRequest) (*Pending, error) {
	st.defaults()
	if req.Mode != ModeReplace && req.Mode != ModeMerge {
		return nil, failf(RestoreReasonInternal, "unknown restore mode %q", req.Mode)
	}
	if RestorePending(st.DataDir) {
		return nil, ErrRestorePending
	}
	fi, err := os.Stat(req.Path)
	if err != nil {
		return nil, failf(RestoreReasonNotRestorable, "the backup could not be opened: %v", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, failf(RestoreReasonNotRestorable, "%s is not a backup file", req.Path)
	}
	if isDataless(fi) {
		return nil, failf(RestoreReasonDataless, "this backup is in iCloud and not on this Mac: download it in Finder first")
	}
	if f := st.checkSpace(uint64(fi.Size()), 0); f != nil {
		return nil, f
	}

	id, err := core.NewID()
	if err != nil {
		return nil, failf(RestoreReasonInternal, "could not mint a restore id: %v", err)
	}
	work := workDir(st.DataDir, id)
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, failf(RestoreReasonInternal, "could not prepare %s: %v", work, err)
	}
	staged := false
	var safety string
	defer func() {
		if staged {
			return
		}
		_ = os.RemoveAll(work)
		if safety != "" {
			_ = os.Remove(filepath.Join(LocalDir(st.DataDir), safety))
		}
	}()

	st.Step(StepChecking)
	v, err := st.verifyCopy(ctx, req.Path, filepath.Join(work, copyName))
	if err != nil {
		return nil, err
	}
	if req.ExpectSHA256 != "" && v.SHA256 != req.ExpectSHA256 {
		return nil, failf(RestoreReasonChanged, "the backup file changed since the restore was approved; approve it again")
	}
	if req.Mode == ModeReplace && !v.Manifest.IncludesDB {
		return nil, failf(RestoreReasonNoDatabase,
			"this backup holds memories and notes only, not the database, so it cannot replace everything: bring back what is missing instead")
	}
	if f := st.checkSpace(uint64(max(v.Unpacked, 0)), 1); f != nil {
		return nil, f
	}

	p := &Pending{
		Version: recordVersion, ID: id, Mode: req.Mode, SHA256: v.SHA256, Backup: completeRef(req.Backup, req.Path, v.Manifest),
		Proof: req.Proof, Credential: req.Credential, RequestedBy: req.RequestedBy, RequestedAt: st.Now().UTC(),
		Stage: StageStaged,
	}
	switch req.Mode {
	case ModeReplace:
		st.Step(StepPreparing)
		kept, err := st.prepareReplace(ctx, work)
		if err != nil {
			return nil, err
		}
		p.Kept = kept
	case ModeMerge:
		st.Step(StepSaving)
		installID, err := store.InstallID(ctx, st.DB)
		if err != nil {
			return nil, failf(RestoreReasonSafety, "the database could not be read: %v", err)
		}
		name, err := writeLocalBackup(ctx, localBackupSpec{
			export: st.export, dataDir: st.DataDir, from: st.DataDir, version: st.Version, host: st.Host,
			installID: installID, forceLocked: st.ForceLocked, passkeyRPID: st.PasskeyRPID, at: st.Now(), id: id,
		})
		if err != nil {
			return nil, failf(RestoreReasonSafety, "today's data could not be backed up before the merge: %v", err)
		}
		safety, p.Safety = name, name
	}
	if err := ctx.Err(); err != nil {
		return nil, failf(RestoreReasonInternal, "the restore was stopped: %v", err)
	}
	if err := createPending(st.DataDir, p); err != nil {
		if errors.Is(err, errRestorePending) {
			return nil, ErrRestorePending
		}
		return nil, failf(RestoreReasonInternal, "could not record the restore: %v", err)
	}
	staged = true
	st.Log.Info("backup: a restore is staged", "id", id, "mode", req.Mode, "backup", p.Backup.Name)
	return p, nil
}

// completeRef fills what the manifest says about the backup into ref.
func completeRef(ref BackupRef, path string, m archive.Manifest) BackupRef {
	if ref.Name == "" {
		ref.Name = filepath.Base(path)
	}
	ref.Path = path
	if ref.Kind == "" {
		ref.Kind = m.Kind
	}
	ref.CreatedAt = m.CreatedAt
	ref.Host = m.SourceHost
	ref.Version = m.SeamlessdVersion
	ref.Memories, ref.Notes, ref.Sessions = 0, 0, 0
	for _, pc := range m.Projects {
		ref.Memories += pc.Memories
		ref.Notes += pc.Notes
		ref.Sessions += pc.Sessions
	}
	if len(m.Projects) == 0 {
		ref.Memories, ref.Notes = m.Counts.MemoryFiles, m.Counts.NoteFiles
	}
	return ref
}

// checkSpace refuses a stage the data dir's disk lacks room for. phase 0 is
// before the backup is read (the copy: its size); phase 1 after (what it
// unpacks to, plus today's data, which a replacement sets aside and later
// archives and a merge backs up first), each with the reserve.
func (st Stager) checkSpace(n uint64, phase int) *Failure {
	if st.FreeBytes == nil {
		return nil
	}
	need := n
	if phase == 1 {
		today, err := estimateNeed(st.DataDir)
		if err != nil {
			return failf(RestoreReasonInternal, "could not size the data folder: %v", err)
		}
		need += today
	}
	want := need + max(minReserve, need)
	free, err := st.FreeBytes(st.DataDir)
	if err != nil {
		return failf(RestoreReasonNoSpace, "could not read the free space on %s: %v", st.DataDir, err)
	}
	if free < want {
		return failf(RestoreReasonNoSpace, "not enough free space on %s for the restore: %s free, %s needed",
			st.DataDir, humanBytes(free), humanBytes(want))
	}
	return nil
}

// verifyCopy reads the backup once through archive.Verify, teeing it into a
// synced local copy, and refuses what cannot be restored here.
func (st Stager) verifyCopy(ctx context.Context, src, dst string) (archive.Verified, error) {
	in, err := os.Open(src)
	if err != nil {
		return archive.Verified{}, failf(RestoreReasonNotRestorable, "the backup could not be opened: %v", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return archive.Verified{}, failf(RestoreReasonInternal, "could not copy the backup: %v", err)
	}
	v, verr := archive.Verify(ctx, in, &ctxWriter{ctx: ctx, w: out})
	serr := out.Sync()
	cerr := out.Close()
	if verr != nil {
		switch {
		case errors.Is(verr, archive.ErrSchemaTooNew):
			return v, failf(RestoreReasonTooNew, "a newer Seamless made this backup: update Seamless to restore it")
		case ctx.Err() != nil:
			return v, failf(RestoreReasonInternal, "the restore was stopped: %v", ctx.Err())
		}
		return v, failf(RestoreReasonUnreadable, "the backup did not check out, so nothing changed: %v", verr)
	}
	if err := errors.Join(serr, cerr); err != nil {
		return v, failf(RestoreReasonInternal, "could not copy the backup: %v", err)
	}
	return v, nil
}

// prepareReplace imports the copy fresh into new/, keeping today's locks on
// top of the backup's, then carries today's kept state into a throwaway copy
// of the result and checks it, so a restore the start would refuse is
// refused now, while nothing has changed.
func (st Stager) prepareReplace(ctx context.Context, work string) (KeptWords, error) {
	kept, err := store.ReadKeptState(ctx, st.DB)
	if err != nil {
		return KeptWords{}, failf(RestoreReasonKept, "today's locks and settings could not be read: %v", err)
	}
	newDir := filepath.Join(work, newDirName)
	if err := importFresh(ctx, filepath.Join(work, copyName), newDir, kept, st.ForceLocked, work, st.Log); err != nil {
		return KeptWords{}, err
	}

	dry := filepath.Join(work, dryDirName)
	defer func() { _ = os.RemoveAll(dry) }()
	if err := os.Mkdir(dry, 0o700); err != nil {
		return KeptWords{}, failf(RestoreReasonInternal, "could not check the prepared data: %v", err)
	}
	for _, name := range []string{archive.DBName, archive.DBName + "-wal"} {
		if err := copyFile(filepath.Join(newDir, name), filepath.Join(dry, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return KeptWords{}, failf(RestoreReasonInternal, "could not check the prepared data: %v", err)
		}
	}
	return applyKept(ctx, filepath.Join(dry, archive.DBName), kept, st.ForceLocked, st.Validate, st.Now())
}

// importFresh restores the archive at src into the empty dir dst: a fresh
// import that keeps today's locked projects locked whatever the backup held.
func importFresh(ctx context.Context, src, dst string, kept store.KeptState, forceLocked []string, tmp string, log *slog.Logger) error {
	f, err := os.Open(src)
	if err != nil {
		return failf(RestoreReasonImport, "the backup copy could not be opened: %v", err)
	}
	defer func() { _ = f.Close() }()
	var keep []archive.LockedProject
	for _, p := range kept.Locked(forceLocked) {
		keep = append(keep, archive.LockedProject{Slug: p.Slug, Isolation: string(p.Isolation)})
	}
	rep, err := archive.Import(ctx, archive.ImportOptions{
		DataDir: dst, Src: f, Logger: log, ForceLocked: forceLocked, KeepLocked: keep, TmpDir: tmp,
	})
	if err != nil {
		return failf(RestoreReasonImport, "the backup could not be prepared, so nothing changed: %v", err)
	}
	if rep.Mode != archive.ModeFresh {
		return failf(RestoreReasonInternal, "the restore's staging folder %s was not empty", dst)
	}
	return nil
}

// applyKept writes kept into the database at dbPath, blanks the hashes of what
// has no vector (so the daemon's reconcile embeds it), and checks the result:
// every lock today holds is there and fenced, and Validate passes.
func applyKept(ctx context.Context, dbPath string, kept store.KeptState, forceLocked []string,
	validate func(context.Context, *sql.DB) error, now time.Time) (words KeptWords, err error) {
	db, err := store.Open(dbPath)
	if err != nil {
		return words, failf(RestoreReasonKept, "the prepared database could not be opened: %v", err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil && err == nil {
			err = failf(RestoreReasonKept, "the prepared database could not be closed: %v", cerr)
		}
	}()
	rep, err := store.ApplyKeptState(ctx, db, kept, store.ApplyKeptOptions{ForceLocked: forceLocked, Now: now})
	if err != nil {
		return words, failf(RestoreReasonKept, "today's locks and settings could not be carried over: %v", err)
	}
	words = KeptWords{Tightened: rep.Tightened, Detached: rep.Detached, Registered: rep.Registered}
	if _, err := store.ClearHashesWithoutVectors(ctx, db); err != nil {
		return words, failf(RestoreReasonKept, "the prepared database could not be readied: %v", err)
	}
	if err := checkLocksKept(ctx, db, kept, forceLocked); err != nil {
		return words, failf(RestoreReasonValidate, "%v", err)
	}
	if validate != nil {
		if err := validate(ctx, db); err != nil {
			return words, failf(RestoreReasonValidate, "the restored data would not pass Seamless's own checks: %v", err)
		}
	}
	return words, nil
}

// checkLocksKept checks a prepared database keeps every lock today holds: each
// console lock as a lock row, and each locked project (the force locks too)
// registered and fenced.
func checkLocksKept(ctx context.Context, db *sql.DB, kept store.KeptState, forceLocked []string) error {
	got, err := store.ConsoleLockedSlugs(ctx, db)
	if err != nil {
		return fmt.Errorf("the prepared database's locks could not be read: %w", err)
	}
	var missing []string
	for _, p := range kept.Locked(nil) {
		if !slices.Contains(got, p.Slug) {
			missing = append(missing, p.Slug)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the prepared data does not keep the lock on %v", missing)
	}
	for _, p := range kept.Locked(forceLocked) {
		iso, err := store.IsolationOf(ctx, db, p.Slug)
		if err != nil {
			return fmt.Errorf("the prepared database's fence on %s could not be read: %w", p.Slug, err)
		}
		if iso == core.IsolationOpen {
			return fmt.Errorf("the prepared data leaves the locked project %s unfenced", p.Slug)
		}
	}
	return nil
}

// localBackupSpec is one safety backup into LocalDir.
type localBackupSpec struct {
	export      func(ctx context.Context, opts archive.ExportOptions) (archive.Manifest, error)
	dataDir     string // the instance root, whose LocalDir receives the backup
	from        string // the data dir to back up: dataDir, or a replacement's previous/
	version     string
	host        string
	installID   string
	forceLocked []string
	passkeyRPID string
	at          time.Time
	id          string
}

// writeLocalBackup writes a before-restore backup of spec.from into LocalDir
// through a temp file, synced, renamed and read back, and returns its name.
func writeLocalBackup(ctx context.Context, spec localBackupSpec) (string, error) {
	local := LocalDir(spec.dataDir)
	if err := os.MkdirAll(local, 0o700); err != nil {
		return "", err
	}
	staging := filepath.Join(local, stagingPrefix+spec.id)
	if err := os.Mkdir(staging, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	tmpPath := filepath.Join(local, partialName(spec.installID, spec.id))
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	published := false
	defer func() {
		if !published {
			_ = f.Close()
			_ = os.Remove(tmpPath)
		}
	}()
	m, err := spec.export(ctx, archive.ExportOptions{
		DataDir: spec.from, Out: &ctxWriter{ctx: ctx, w: f}, SeamlessdVersion: spec.version, Host: spec.host,
		ForceLocked: spec.forceLocked, PasskeyRPID: spec.passkeyRPID,
		Kind: archive.KindBeforeRestore, InstallID: spec.installID, TmpDir: staging,
	})
	if err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	name, err := freeName(local, spec.host, spec.at, archive.KindBeforeRestore)
	if err != nil {
		return "", err
	}
	final := filepath.Join(local, name)
	if err := os.Rename(tmpPath, final); err != nil {
		return "", err
	}
	published = true
	syncDir(local)
	back, err := readManifestFile(final)
	if err == nil && (back.Fingerprint != m.Fingerprint || !back.CreatedAt.Equal(m.CreatedAt) ||
		back.Counts.MemoryFiles != m.Counts.MemoryFiles || back.Counts.NoteFiles != m.Counts.NoteFiles) {
		err = errors.New("its manifest is not the one just written")
	}
	if err != nil {
		_ = os.Remove(final)
		return "", fmt.Errorf("the backup did not read back: %w", err)
	}
	return name, nil
}

// copyFile copies src to the new file dst, synced.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
