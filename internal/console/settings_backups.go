package console

// Settings > Backups: the schedule (on or off, how often, how many to keep,
// the folder), the backups the folder and Seamless's own folder hold, and
// restoring one of them -- replacing everything, or bringing back only what
// is missing -- with the undo that every restore leaves behind. The section
// reads and steers Config.Backups; with none (a process that runs no backup
// service) it says so and every POST answers with an error flash.
//
// Under peer_auth with any project locked, a restore needs a passkey
// ceremony bound to the backup's SHA-256 and the mode (approval.BeginRestore
// and FinishRestore): /restore/begin is the browser's only, the digest is
// computed here from the file both times, and /restore refuses without the
// ceremony's id and the browser's response. Without a usable passkey the
// section shows the terminal command instead, run as the daemon's account.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/backup"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/update"
)

// backupsNotRunning is the flash every Backups POST answers with when this
// process runs no backup service.
const backupsNotRunning = "Backups are not running in this process, so there is nothing to change here."

// backupsStopped is the flash for a command the stopped service cannot serve.
const backupsStopped = "The backup service stopped after an internal error. Restart Seamless to revive it."

// maxBackupDirLen bounds a folder path the section accepts.
const maxBackupDirLen = 4096

// backupsPanel is the Backups section's view of the backup service.
type backupsPanel struct {
	// Running is false when this process runs no backup service: the section
	// says so and offers nothing else. Stopped is a service whose loop died
	// after an internal error.
	Running bool
	Stopped bool

	Status backupsStatus
	Form   backupsForm
	List   backupsList

	// The restore side: the last restore's result until dismissed, a
	// preparation that failed, the steps of one under way, and the confirm
	// card ?restore= opens.
	Result   *restoreResult
	Failed   *restoreFailure
	Progress *restoreProgress
	Confirm  *restoreConfirm

	// State is what backups.js follows (the section's data-restore-state):
	// "staging", "pending" or "archiving" while a restore is in flight, ""
	// otherwise. Restart is "auto" where Seamless restarts by itself to
	// finish one, "manual" on Windows. BackingUp marks a backup run under way.
	State     string
	Restart   string
	BackingUp bool
}

// backupsStatus is the status card.
type backupsStatus struct {
	// Badge is the state chip's word and Tone its badge class ("ok", "warn",
	// "danger", "accent", "" for neutral): the chip is text, never colour
	// alone.
	Badge, Tone string
	// Lead says whether automatic backups run, how often and how many stay.
	Lead string
	// Last is the newest backup this install made, LastAt its time; None
	// stands in before there is one.
	Last   string
	LastAt time.Time
	None   string
	// Next is when the next automatic backup runs, or that nothing changed.
	Next   string
	NextAt time.Time
	// Running is the backup under way, RunningAt when it started.
	Running   string
	RunningAt time.Time
	// Failing is the automatic runs' failure streak; nil without one.
	Failing *backupsFailing
	// ManualFailed says the owner's own last backup did not finish.
	ManualFailed string
	// Folder is where backups go; FolderLocal marks Seamless's own folder,
	// and FolderProblem what is wrong with the folder when it cannot be read.
	Folder        string
	FolderLocal   bool
	FolderProblem string
	// BackUpNowBlocked says why Back up now cannot run, "" when it can.
	BackUpNowBlocked string
	// Notes are the service's own problems: a stored setting it could not
	// read, a record it could not save.
	Notes []string
}

// backupsFailing is the failing card.
type backupsFailing struct {
	Line  string
	Error string
	Since time.Time
	// RetryBlocked says why Try again cannot run, "" when it can.
	RetryBlocked string
}

// backupsForm is the schedule form.
type backupsForm struct {
	Auto      bool
	Every     string
	EveryOpts []backupsOption
	Keep      int
	MinKeep   int
	MaxKeep   int
	// Dir is the folder in force, LocalDir Seamless's own folder.
	Dir      string
	LocalDir string
	// Check is the folder in force's check, drawn here so the line reads
	// right before (and without) the script.
	Check backupsFolderLine
	Chips []backupsChip
	// Overridden reports a console choice in force, which Reset clears;
	// SourceLabel and SourceNote say who decided each value.
	Overridden  bool
	SourceLabel string
	SourceNote  string
}

// backupsOption is one How often choice.
type backupsOption struct {
	Value, Label string
	Selected     bool
}

// backupsChip is one suggested place for the folder.
type backupsChip struct {
	Label, Kind, Dir string
	Current          bool
}

// backupsFolderLine is a folder check in words, for the live line under the
// folder field (and GET /console/settings/backups/folder).
type backupsFolderLine struct {
	State  string `json:"state"`
	Usable bool   `json:"usable"`
	Text   string `json:"text"`
	Dir    string `json:"dir"`
}

// backupsList is the Available backups card.
type backupsList struct {
	Rows []backupRow
	// Error says why the list could not be read; Empty is the empty state's
	// sentence, "" when there are rows.
	Error string
	Empty string
	// Local is Seamless's own folder when it is listed beside the chosen one.
	Local string
	// Locked reports a locked project the comparisons leave out.
	Locked bool
}

// backupRow is one backup in the list.
type backupRow struct {
	// ID keys the row for the in-place morph.
	ID   string
	Ref  string
	Name string
	Path string
	When time.Time
	// Kind is the badge's words and KindTone its class.
	Kind, KindTone string
	// Local marks a backup in Seamless's own folder, beside a chosen folder
	// elsewhere.
	Local bool
	// From names another install's backup: "from another Seamless on <host>".
	From    string
	Size    string
	Summary string
	// Note says why it cannot be restored (in iCloud, too new, unreadable).
	Note string
	// Restore is the confirm card's link, "" when it cannot be restored, and
	// RestoreBlocked why the button waits (a backup or a restore under way).
	Restore        string
	RestoreBlocked string
	// Made says which Seamless made it, where, at which schema.
	Made     string
	Projects []backupProjectRow
	Omitted  int
	// Open marks the backup the confirm card is open for; Undo the safety
	// backup the last restore left.
	Open bool
	Undo bool
}

// backupProjectRow is one project in a backup's details.
type backupProjectRow struct {
	Name                             string
	Memories, Notes, Tasks, Sessions int
	// VsNow says how this project differs now: "+3 memories · −1 note",
	// "same", "not here now", "locked".
	VsNow string
	// OnlyNow marks a project this install holds that the backup does not.
	OnlyNow bool
}

// restoreConfirm is the confirm card ?restore=<ref> opens.
type restoreConfirm struct {
	Ref, Name string
	When      time.Time
	Kind      string
	// Missing: the ref names no backup in the list.
	Missing bool
	// NotRestorable says why neither mode can restore it.
	NotRestorable string
	Compare       string
	Replace       restoreChoice
	Merge         restoreChoice
	// Mode is the choice the card opens on.
	Mode string
	// Blocked says why the restore cannot start now (a backup or a restore
	// under way), "" when it can.
	Blocked string
	// Gated is a restore that needs a passkey; Passkey that this browser can
	// run the ceremony, NoPasskey why not, and Commands the terminal's way.
	Gated     bool
	Passkey   bool
	NoPasskey string
	Commands  []restoreCommandLine
	Restart   string
	Undo      bool
}

// restoreChoice is one mode in the confirm card.
type restoreChoice struct {
	OK  bool
	Why string
}

// restoreCommandLine is the terminal command for one mode.
type restoreCommandLine struct {
	Label, Command string
}

// restoreProgress is the progress card of a restore in flight.
type restoreProgress struct {
	State      string
	Mode       string
	BackupName string
	BackupAt   time.Time
	Steps      []restoreStep
	// Manual is a restart the owner runs (Windows), Command what to run, and
	// ExpiresAt when the staged restore lapses unapplied.
	Manual    bool
	Command   string
	ExpiresAt time.Time
	// Cancel offers Cancel restore: the restore is staged and not applying.
	Cancel bool
}

// restoreStep is one step of the progress card: "done", "doing" or "todo".
type restoreStep struct {
	Label, State string
}

// restoreResult is the last restore's banner, until dismissed.
type restoreResult struct {
	// Outcome is the backup package's word: restored, failed, partial or
	// expired. Tone is the banner's.
	Outcome, Tone string
	Mode          string
	BackupName    string
	BackupAt      time.Time
	RequestedAt   time.Time
	// Counts says what came back ("398 memories and 94 notes", or what a
	// merge added).
	Counts string
	// Message is the backup package's sentence for a restore that did not
	// finish.
	Message string
	// Previous is the safety backup holding the data from before, and Undo
	// the confirm card that restores it.
	Previous string
	Undo     string
	// Saving: the replaced data is still being saved as a backup (Dismiss
	// waits), or could not be (PreviousError, with PreviousDir where it is).
	Saving        bool
	PreviousError string
	PreviousDir   string
	// Kept lists what carrying today's privacy and locks over changed.
	Kept []string
}

// restoreFailure is a preparation that failed: nothing changed.
type restoreFailure struct {
	Mode       string
	BackupName string
	Message    string
	At         time.Time
}

// backupsPanelFor builds the Backups section for this request.
func (s *Service) backupsPanelFor(r *http.Request, now time.Time) backupsPanel {
	bv := s.cfg.Backups
	if bv == nil {
		return backupsPanel{}
	}
	ctx := r.Context()
	st := bv.Status()
	p := backupsPanel{Running: true, Stopped: st.Stopped, Restart: "auto", BackingUp: st.Running != nil}
	if !restartsInPlace() {
		p.Restart = "manual"
	}
	listing, lerr := bv.List(ctx)
	local := filepath.Clean(backup.LocalDir(s.cfg.DataDir))

	p.Status = backupsStatusFor(st, listing, lerr, now)
	p.Form = s.backupsFormFor(st, local)
	counts := s.nowCounts(ctx)
	busy := restoreBusyWords(st)
	p.List = s.backupsListFor(ctx, st, listing, lerr, counts, busy, r.URL.Query().Get("restore"))

	rs := st.Restore
	switch {
	case rs.Staging != nil:
		p.State = "staging"
	case rs.Pending != nil:
		p.State = "pending"
	case rs.Archiving || (rs.Last.NeedsArchive() && rs.Last.PreviousError == ""):
		p.State = "archiving"
	}
	if prog := restoreProgressFor(st, p.Restart == "manual"); prog != nil {
		p.Progress = prog
	}
	if rs.Last != nil {
		p.Result = restoreResultFor(*rs.Last, listing)
	}
	if f := rs.Failed; f != nil {
		p.Failed = &restoreFailure{Mode: f.Mode, BackupName: f.Backup.Name, Message: nothingChanged(f.Message), At: f.At}
	}
	if ref := r.URL.Query().Get("restore"); ref != "" {
		p.Confirm = s.restoreConfirmFor(r, st, listing, ref, counts, busy, p.Restart)
	}
	return p
}

// backupsLead is the status card's first line.
func backupsLead(set backup.Settings) string {
	if set.Auto {
		return "Automatic backups are on: " + everyWords(set.Every) + ", " + keepWords(set.Keep) + "."
	}
	switch set.AutoSource {
	case backup.SourceConsole:
		return "Automatic backups are off: they were turned off in this console."
	case backup.SourceConfig:
		return "Automatic backups are off: backup.auto: false is set in the config file or SEAMLESS_BACKUP_AUTO."
	}
	return "Automatic backups are off: a Seamless built from source does not back up by itself."
}

// backupsStatusFor builds the status card.
func backupsStatusFor(st backup.Status, listing backup.Listing, lerr error, now time.Time) backupsStatus {
	set := st.Settings
	c := backupsStatus{Lead: backupsLead(set), Folder: set.Dir, FolderLocal: set.DirIsLocal}
	switch {
	case st.Stopped:
		c.Badge, c.Tone = "Stopped", "danger"
	case st.Running != nil:
		c.Badge, c.Tone = "Backing up", "accent"
	case set.Auto && st.Failures > 0:
		c.Badge, c.Tone = "Failing", "warn"
	case set.Auto:
		c.Badge, c.Tone = "On", "ok"
	default:
		c.Badge = "Off"
	}
	if b := st.LastBackup; b != nil {
		c.LastAt = b.At
		c.Last = "Last backup " + agoPhrase(b.At) + ", " + humanBytes(b.Bytes)
		projects := b.Projects
		for _, e := range listing.Entries {
			// The list's count, which leaves the global scope out, so the
			// two say the same.
			if e.Name == b.Name && e.Manifest != nil {
				if _, _, _, n, ok := backupTotals(*e.Manifest, nil); ok {
					projects = n
				}
				break
			}
		}
		if b.Memories+b.Notes > 0 {
			c.Last += ": " + countOf(b.Memories, "memory", "memories") + " and " + countOf(b.Notes, "note", "notes")
			if projects > 0 {
				c.Last += " in " + countOf(projects, "project", "projects")
			}
		}
		c.Last += "."
	} else if lerr == nil && len(listing.Entries) > 0 {
		c.None = "This install has not made a backup yet. The list below shows the backups found in its folders."
	} else {
		c.None = "No backup yet."
	}
	if r := st.Running; r != nil {
		c.RunningAt = r.StartedAt
		c.Running = "Backing up… started " + agoPhrase(r.StartedAt)
		if r.Scheduled {
			c.Running = "Running the automatic backup… started " + agoPhrase(r.StartedAt)
		}
	} else if set.Auto && !st.Stopped && !st.NextAt.IsZero() {
		c.NextAt = st.NextAt
		if st.UpToDate {
			c.Next = "Up to date: nothing has changed since the last backup. Next check " + untilPhrase(st.NextAt, now) + "."
		} else {
			c.Next = "Next backup " + untilPhrase(st.NextAt, now) + "."
		}
	}
	busy := ""
	switch {
	case st.Stopped:
		busy = "The backup service is not running"
	case st.Running != nil:
		busy = "A backup is running"
	case st.Restore.Staging != nil:
		busy = "A restore is being prepared"
	case st.Restore.Archiving:
		busy = "The data a restore replaced is being saved"
	}
	c.BackUpNowBlocked = busy
	if set.Auto && st.Failures > 0 {
		f := &backupsFailing{Error: st.LastError, Since: st.FailingSince, RetryBlocked: busy}
		if st.Failures == 1 {
			f.Line = "The last automatic backup failed " + agoPhrase(st.FailingSince)
		} else {
			f.Line = fmt.Sprintf("The last %d automatic backups failed, the first %s", st.Failures, agoPhrase(st.FailingSince))
		}
		c.Failing = f
	}
	if lr := st.LastRun; lr != nil && !lr.Scheduled && lr.Outcome == backup.OutcomeFailed && st.Running == nil {
		c.ManualFailed = "Your backup " + agoPhrase(lr.At) + " did not finish: " + lr.Error
	}
	if lerr == nil && listing.DirCheck.State != "" && !listing.DirCheck.Usable() {
		c.FolderProblem = sentenceStart(listing.DirCheck.Message()) + "."
	}
	if st.SettingsError != "" {
		c.Notes = append(c.Notes, "The backup settings saved in this console could not be read, so the config file's apply: "+st.SettingsError)
	}
	if st.StateError != "" {
		c.Notes = append(c.Notes, "The schedule's record could not be saved, so a restart forgets when the last backup ran: "+st.StateError)
	}
	return c
}

// everyChoices are the How often menu's values.
var everyChoices = []struct {
	d     time.Duration
	label string
}{
	{time.Hour, "Every hour"},
	{6 * time.Hour, "Every 6 hours"},
	{24 * time.Hour, "Every day"},
	{7 * 24 * time.Hour, "Every week"},
}

// backupsFormFor builds the schedule form.
func (s *Service) backupsFormFor(st backup.Status, local string) backupsForm {
	set := st.Settings
	f := backupsForm{
		Auto: set.Auto, Every: config.Duration(set.Every).String(), Keep: set.Keep,
		MinKeep: config.MinBackupKeep, MaxKeep: config.MaxBackupKeep,
		Dir: set.Dir, LocalDir: local,
	}
	listed := false
	for _, c := range everyChoices {
		if c.d == set.Every {
			listed = true
		}
	}
	for _, c := range everyChoices {
		if !listed && set.Every < c.d {
			f.EveryOpts = append(f.EveryOpts, s.customEvery(set))
			listed = true
		}
		f.EveryOpts = append(f.EveryOpts, backupsOption{Value: config.Duration(c.d).String(), Label: c.label, Selected: c.d == set.Every})
	}
	if !listed {
		f.EveryOpts = append(f.EveryOpts, s.customEvery(set))
	}
	f.Check = folderLine(s.cfg.Backups.CheckFolder(set.Dir), local)
	for _, sg := range s.cfg.Backups.Suggestions() {
		// A place this account cannot write to (a read-only system volume, a
		// snapshot) is no suggestion: offered, it would only be refused.
		if sg.Kind != backup.PlaceLocal && !s.cfg.Backups.CheckFolder(sg.Dir).Usable() {
			continue
		}
		f.Chips = append(f.Chips, backupsChip{Label: sg.Label, Kind: placeWords(sg.Kind), Dir: sg.Dir, Current: filepath.Clean(sg.Dir) == set.Dir})
	}
	var console []string
	anyConfig := false
	for _, fld := range []struct{ source, words string }{
		{set.AutoSource, "the switch"}, {set.EverySource, "how often"}, {set.KeepSource, "how many to keep"}, {set.DirSource, "the folder"},
	} {
		switch fld.source {
		case backup.SourceConsole:
			console = append(console, fld.words)
		case backup.SourceConfig:
			anyConfig = true
		}
	}
	switch {
	case len(console) > 0:
		f.Overridden = true
		f.SourceLabel = "Set in this console: " + joinWords(console)
		f.SourceNote = "Your choices here win over the config file until you reset them."
	case anyConfig:
		f.SourceLabel = "Following the config file"
		f.SourceNote = "Saving here stores your choices, which win over the config file until you reset them."
	case set.Auto:
		f.SourceLabel = "Default for this build"
		f.SourceNote = "Release builds back up every day and keep the newest 7, in Seamless's data folder. Saving here stores your choices."
	default:
		f.SourceLabel = "Default for this build"
		f.SourceNote = "A Seamless built from source backs up only when you turn it on. Saving here stores your choices."
	}
	return f
}

// customEvery is the How often choice for an interval the menu does not list.
func (s *Service) customEvery(set backup.Settings) backupsOption {
	label := capitalize(everyWords(set.Every))
	switch set.EverySource {
	case backup.SourceConfig:
		label += " (config file)"
	case backup.SourceConsole:
		label += " (set in this console)"
	}
	return backupsOption{Value: config.Duration(set.Every).String(), Label: label, Selected: true}
}

// placeWords names a suggestion's kind, for its chip's title.
func placeWords(kind string) string {
	switch kind {
	case backup.PlaceICloud:
		return "iCloud Drive"
	case backup.PlaceCloud:
		return "a synced folder"
	case backup.PlaceVolume:
		return "an external or network disk"
	}
	return "on this machine's own disk"
}

// joinWords is "a", "a and b", "a, b and c".
func joinWords(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// folderLine says a folder check in the live line's words.
func folderLine(c backup.FolderCheck, local string) backupsFolderLine {
	l := backupsFolderLine{State: string(c.State), Usable: c.Usable(), Dir: c.Dir}
	free := ""
	if c.FreeKnown {
		free = " · " + humanBytes(int64(min(c.Free, uint64(1)<<62))) + " free"
	}
	switch c.State {
	case backup.FolderReady:
		l.Text = "Ready" + free
		if c.Dir == local {
			l.Text = "Seamless's data folder · ready" + free
		}
	case backup.FolderWillCreate:
		l.Text = "Will be created" + free
	case backup.FolderNotWritable:
		l.Text = "Not writable"
	case backup.FolderInsideDataDir:
		l.Text = "Inside Seamless's data folder: choose a folder outside it"
	case backup.FolderNotDirectory:
		l.Text = "Not a folder"
	case backup.FolderNotAbsolute:
		l.Text = "Use a full path, like /Volumes/Backup/Seamless Backups"
	default:
		l.Text = sentenceStart(c.Message())
	}
	return l
}

// sentenceStart upper-cases a sentence's first letter, except a word that is
// spelled lowercase-first ("macOS").
func sentenceStart(s string) string {
	if strings.HasPrefix(s, "macOS") {
		return s
	}
	return capitalize(s)
}

// nowCounts are what each project this request may read holds now, by slug.
// A failed read costs the comparisons, never the section.
func (s *Service) nowCounts(ctx context.Context) map[string]store.ProjectContent {
	rows, err := store.ProjectContentCounts(ctx, s.db(ctx))
	if err != nil {
		s.logger.Warn("console: backups: content counts", "error", err)
		return nil
	}
	out := make(map[string]store.ProjectContent, len(rows))
	for _, r := range rows {
		out[r.Slug] = r
	}
	return out
}

// restoreBusyWords says why no restore can start now, "" when one can.
func restoreBusyWords(st backup.Status) string {
	switch {
	case st.Stopped:
		return "The backup service is not running"
	case st.Running != nil:
		return "A backup is running: restore once it finishes"
	case st.Restore.Staging != nil, st.Restore.Pending != nil:
		return "A restore is already under way"
	case st.Restore.Archiving:
		return "The data the last restore replaced is still being saved"
	}
	return ""
}

// backupsListFor builds the Available backups card.
func (s *Service) backupsListFor(ctx context.Context, st backup.Status, listing backup.Listing, lerr error,
	now map[string]store.ProjectContent, busy, open string) backupsList {
	l := backupsList{Local: listing.Local}
	if lerr != nil {
		l.Error = "The list of backups could not be read: " + lerr.Error()
		return l
	}
	undo := ""
	if r := st.Restore.Last; r != nil {
		undo = r.Previous
	}
	for _, e := range listing.Entries {
		row := backupRow{
			ID: backupRowID(e.Ref), Ref: e.Ref, Name: e.Name, Path: e.Path, When: e.CreatedAt,
			Kind: backupKindWords(e), Local: e.Where == backup.WhereLocal, Size: humanBytes(e.Size),
			Open: e.Ref == open, Undo: undo != "" && e.Name == undo && e.Kind == archive.KindBeforeRestore,
		}
		switch e.Kind {
		case archive.KindManual:
			row.KindTone = "brand"
		case archive.KindBeforeRestore:
			row.KindTone = "warn"
		case archive.KindPreUpdate:
			row.KindTone = "accent"
		}
		if m := e.Manifest; m != nil {
			if e.FromAnother {
				row.From = "from another Seamless"
				if h := strings.TrimSpace(m.SourceHost); h != "" {
					row.From += " on " + h
				}
			}
			row.Summary = backupSummary(*m)
			row.Made = backupMade(*m)
			row.Projects, row.Omitted = s.backupProjects(ctx, *m, now)
		}
		replace, merge := backup.Restorable(e, backup.ModeReplace), backup.Restorable(e, backup.ModeMerge)
		switch {
		case e.Dataless:
			row.Note = "In iCloud: download it in Finder first."
		case e.TooNew:
			row.Note = "Made by a newer Seamless" + newerWords(e.Manifest) + ": update to restore it."
		case replace != nil && merge != nil:
			row.Note = sentenceStart(merge.Message) + "."
		default:
			row.Restore = backupsSectionHref + "&restore=" + url.QueryEscape(e.Ref) + "#backup-restore"
			row.RestoreBlocked = busy
		}
		l.Rows = append(l.Rows, row)
	}
	l.Locked = len(lockedProjects(ctx)) > 0
	if len(l.Rows) == 0 {
		switch {
		case st.Settings.Auto:
			l.Empty = "No backups yet. The first automatic one runs within the hour, or make one now."
		default:
			l.Empty = "No backups yet. Make one now, or turn automatic backups on below."
		}
	}
	return l
}

// backupRowID is a list row's element id: stable for a backup, valid in HTML.
func backupRowID(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return "backup-" + hex.EncodeToString(sum[:6])
}

// newerWords names the Seamless that made a backup too new to restore.
func newerWords(m *archive.Manifest) string {
	if m == nil {
		return ""
	}
	if v, ok := update.Parse(m.SeamlessdVersion); ok {
		return " (v" + v.String() + ")"
	}
	return ""
}

// backupTotals adds a manifest's per-project counts up, leaving out the
// projects skip names. ok is false for a manifest without them (written
// before they existed).
func backupTotals(m archive.Manifest, skip func(string) bool) (mem, notes, sessions, projects int, ok bool) {
	if len(m.Projects) == 0 {
		return 0, 0, 0, 0, false
	}
	for _, pc := range m.Projects {
		if skip != nil && skip(pc.Slug) {
			continue
		}
		mem += pc.Memories
		notes += pc.Notes
		sessions += pc.Sessions
		if pc.Slug != "" {
			projects++
		}
	}
	return mem, notes, sessions, projects + m.ProjectsOmitted, true
}

// backupSummary is a row's counts line: "412 memories · 96 notes · 7
// projects · 1,203 sessions".
func backupSummary(m archive.Manifest) string {
	mem, notes, sessions, projects, ok := backupTotals(m, nil)
	if !ok {
		mem, notes, sessions = m.Counts.MemoryFiles, m.Counts.NoteFiles, m.Counts.Tables["sessions"]
	}
	parts := []string{countOf(mem, "memory", "memories"), countOf(notes, "note", "notes")}
	if projects > 0 {
		parts = append(parts, countOf(projects, "project", "projects"))
	}
	if sessions > 0 {
		parts = append(parts, countOf(sessions, "session", "sessions"))
	}
	if !m.IncludesDB {
		parts = append(parts, "memories and notes only")
	}
	return strings.Join(parts, " · ")
}

// backupMade says which Seamless made a backup, where, at which schema.
func backupMade(m archive.Manifest) string {
	made := "Made by Seamless"
	if v, ok := update.Parse(m.SeamlessdVersion); ok {
		made += " v" + v.String()
	} else if v := strings.TrimSpace(m.SeamlessdVersion); v != "" && len(v) <= 40 {
		made += " " + v
	}
	if h := strings.TrimSpace(m.SourceHost); h != "" {
		made += " on " + h
	}
	if m.SchemaVersion > 0 {
		made += ", schema v" + strconv.Itoa(m.SchemaVersion)
	} else if !m.IncludesDB {
		made += ", memories and notes only"
	}
	return made
}

// backupProjects is a backup's per-project table, with how each project
// differs now. A project locked here shows its name and the backup's counts,
// "locked" in place of now; one this install holds that the backup does not
// closes the table.
func (s *Service) backupProjects(ctx context.Context, m archive.Manifest, now map[string]store.ProjectContent) ([]backupProjectRow, int) {
	if len(m.Projects) == 0 {
		return nil, 0
	}
	seen := map[string]bool{}
	var out []backupProjectRow
	for _, pc := range m.Projects {
		seen[pc.Slug] = true
		row := backupProjectRow{Name: projectWords(pc.Slug, pc.Name), Memories: pc.Memories, Notes: pc.Notes, Tasks: pc.Tasks, Sessions: pc.Sessions}
		cur, here := now[pc.Slug]
		switch {
		case lockedHere(ctx, pc.Slug):
			row.VsNow = "locked"
		case !here && now != nil:
			row.VsNow = "not here now"
		case here:
			row.VsNow = deltaWords(cur.Memories-pc.Memories, cur.Notes-pc.Notes)
		}
		out = append(out, row)
	}
	if m.ProjectsOmitted == 0 {
		var extra []string
		for slug, cur := range now {
			if !seen[slug] && (cur.Memories+cur.Notes+cur.Tasks+cur.Sessions) > 0 {
				extra = append(extra, slug)
			}
		}
		slices.Sort(extra)
		for _, slug := range extra {
			cur := now[slug]
			out = append(out, backupProjectRow{Name: projectWords(slug, cur.Name), OnlyNow: true,
				VsNow: deltaWords(cur.Memories, cur.Notes) + " (not in this backup)"})
		}
	}
	return out, m.ProjectsOmitted
}

// projectWords names a project in a backup's table.
func projectWords(slug, name string) string {
	switch {
	case slug == "":
		return "Global (no project)"
	case name != "" && name != slug:
		return name + " (" + slug + ")"
	}
	return slug
}

// deltaWords says how many more (+) or fewer (−) memories and notes there
// are now: "+3 memories · −1 note", or "same".
func deltaWords(mem, notes int) string {
	var parts []string
	signed := func(n int, one, many string) string {
		sign := "+"
		if n < 0 {
			sign, n = "−", -n
		}
		if n == 1 {
			return sign + "1 " + one
		}
		return sign + groupDigits(n) + " " + many
	}
	if mem != 0 {
		parts = append(parts, signed(mem, "memory", "memories"))
	}
	if notes != 0 {
		parts = append(parts, signed(notes, "note", "notes"))
	}
	if len(parts) == 0 {
		return "same"
	}
	return strings.Join(parts, " · ")
}

// compareWords is the confirm card's comparison with now, over the projects
// this request reads (a locked one is left out on both sides).
func compareWords(ctx context.Context, m archive.Manifest, now map[string]store.ProjectContent) string {
	locked := func(slug string) bool { return lockedHere(ctx, slug) }
	bMem, bNotes, bSess, _, ok := backupTotals(m, locked)
	if !ok || now == nil {
		return "This backup does not say what each project holds, so it cannot be compared with now."
	}
	var nMem, nNotes, nSess int
	for slug, c := range now {
		if locked(slug) {
			continue
		}
		nMem += c.Memories
		nNotes += c.Notes
		nSess += c.Sessions
	}
	var parts []string
	for _, d := range []struct {
		n         int
		one, many string
	}{{nMem - bMem, "memory", "memories"}, {nNotes - bNotes, "note", "notes"}, {nSess - bSess, "session", "sessions"}} {
		switch {
		case d.n > 0:
			parts = append(parts, countOf(d.n, "more "+d.one, "more "+d.many))
		case d.n < 0:
			parts = append(parts, countOf(-d.n, "fewer "+d.one, "fewer "+d.many))
		}
	}
	line := "Compared with now: the same number of memories, notes and sessions as this backup."
	if len(parts) > 0 {
		line = "Compared with this backup, this install holds " + joinWords(parts) + " now."
	}
	if len(lockedProjects(ctx)) > 0 {
		line += " Locked projects are not compared."
	}
	return line
}

// restoreConfirmFor builds the confirm card for ref.
func (s *Service) restoreConfirmFor(r *http.Request, st backup.Status, listing backup.Listing, ref string,
	now map[string]store.ProjectContent, busy, restart string) *restoreConfirm {
	ctx := r.Context()
	c := &restoreConfirm{Ref: ref, Blocked: busy, Restart: restart}
	e, ok := listing.Find(ref)
	if !ok {
		c.Missing = true
		return c
	}
	c.Name, c.When, c.Kind = e.Name, e.CreatedAt, backupKindWords(e)
	if last := st.Restore.Last; last != nil && last.Previous != "" && e.Name == last.Previous {
		c.Undo = true
	}
	replace, merge := backup.Restorable(e, backup.ModeReplace), backup.Restorable(e, backup.ModeMerge)
	c.Replace = restoreChoice{OK: replace == nil}
	if replace != nil {
		c.Replace.Why = sentenceStart(replace.Message) + "."
	}
	c.Merge = restoreChoice{OK: merge == nil}
	if merge != nil {
		c.Merge.Why = sentenceStart(merge.Message) + "."
	}
	switch {
	case c.Replace.OK:
		c.Mode = backup.ModeReplace
	case c.Merge.OK:
		c.Mode = backup.ModeMerge
	default:
		c.NotRestorable = c.Merge.Why
		return c
	}
	if e.Manifest != nil {
		c.Compare = compareWords(ctx, *e.Manifest, now)
	}
	if !s.restoreGated(ctx) {
		return c
	}
	c.Gated = true
	v := s.passkeyReach(r)
	switch {
	case !v.browser:
		c.NoPasskey = "A restore here needs a passkey, so it runs in the browser you sign in to the console with, or from a terminal:"
	case !v.passkeysOn:
		c.NoPasskey = "Passkeys are not configured on this daemon, so a restore runs from a terminal:"
	case !v.hasPasskey:
		c.NoPasskey = "No passkey is enrolled, so a restore runs from a terminal (or add a passkey on the Approvers page first):"
	case !v.onOrigin:
		c.NoPasskey = "A passkey works only on " + v.origin + ": open this page there, or restore from a terminal:"
	default:
		c.Passkey = true
	}
	if !c.Passkey {
		if c.Replace.OK {
			c.Commands = append(c.Commands, restoreCommandLine{Label: "To replace all data", Command: s.restoreCommand(e.Path, backup.ModeReplace)})
		}
		if c.Merge.OK {
			c.Commands = append(c.Commands, restoreCommandLine{Label: "To bring back only what is missing", Command: s.restoreCommand(e.Path, backup.ModeMerge)})
		}
	}
	return c
}

// passkeyFacts is whether this request can run a passkey ceremony.
type passkeyFacts struct {
	browser, passkeysOn, hasPasskey, onOrigin bool
	origin                                    string
}

// passkeyReach reads what a ceremony needs: a browser, passkeys configured, an
// enrolled one, and the page on the relying party's origin.
func (s *Service) passkeyReach(r *http.Request) passkeyFacts {
	f := passkeyFacts{browser: s.classify(r) == credCookie}
	if s.cfg.Approvals == nil {
		return f
	}
	f.passkeysOn = s.cfg.Approvals.PasskeysOn()
	if creds, err := store.ApproverCredentials(r.Context(), s.writeDB(), store.ApproverPasskey); err != nil {
		s.logger.Warn("console: backups: approver credentials", "error", err)
	} else {
		f.hasPasskey = len(creds) > 0
	}
	if origins := s.cfg.Approvals.Origins(); len(origins) > 0 {
		f.origin = origins[0]
		f.onOrigin = slices.Contains(origins, requestOrigin(r))
	}
	return f
}

// restoreCommand is the terminal's way to restore the backup at path in mode:
// seamlessd restore as the daemon's own account, the proof a passkey stands in
// for here.
func (s *Service) restoreCommand(path, mode string) string {
	account, seamlessd, env := "<daemon account>", "seamlessd", ""
	if t := s.cfg.Terminal; t != nil {
		if t.Account != "" {
			account = shellWord(t.Account)
		}
		if t.Seamlessd != "" {
			seamlessd = shellWord(t.Seamlessd)
		}
		if t.Config != "" {
			env = "env SEAMLESS_CONFIG=" + shellWord(t.Config) + " "
		}
	}
	flag := ""
	if mode == backup.ModeMerge {
		flag = "--merge "
	}
	return fmt.Sprintf("sudo -u %s %s%s restore %s%s", account, env, seamlessd, flag, shellWord(path))
}

// restoreProgressFor is the progress card of a restore in flight, nil without
// one.
func restoreProgressFor(st backup.Status, manual bool) *restoreProgress {
	rs := st.Restore
	var p *restoreProgress
	switch {
	case rs.Staging != nil:
		p = &restoreProgress{State: "staging", Mode: rs.Staging.Mode, BackupName: rs.Staging.Backup.Name, BackupAt: rs.Staging.Backup.CreatedAt}
	case rs.Pending != nil:
		pe := rs.Pending
		p = &restoreProgress{State: "pending", Mode: pe.Mode, BackupName: pe.Backup.Name, BackupAt: pe.Backup.CreatedAt,
			Cancel: pe.Stage == backup.StageStaged, Manual: manual}
		if manual {
			p.Command = "seamlessd restart"
			p.ExpiresAt = pe.RequestedAt.Add(time.Hour)
		}
	case rs.Archiving || (rs.Last.NeedsArchive() && rs.Last.PreviousError == ""):
		last := rs.Last
		p = &restoreProgress{State: "archiving", Mode: backup.ModeReplace}
		if last != nil {
			p.Mode, p.BackupName, p.BackupAt = last.Mode, last.Backup.Name, last.Backup.CreatedAt
		}
	default:
		return nil
	}
	second := "Preparing the restored data"
	if p.Mode == backup.ModeMerge {
		second = "Saving your current data"
	}
	labels := []string{"Checking the backup", second, "Restarting Seamless", "Done"}
	if p.Mode == backup.ModeReplace {
		labels = append(labels, "Saving your previous data as a backup")
	}
	at := 0 // the step in progress
	switch p.State {
	case "staging":
		if s := rs.Staging.Step; s == backup.StepPreparing || s == backup.StepSaving {
			at = 1
		}
	case "pending":
		at = 2
	case "archiving":
		at = 4
	}
	for i, l := range labels {
		state := "todo"
		switch {
		case i < at:
			state = "done"
		case i == at:
			state = "doing"
		}
		p.Steps = append(p.Steps, restoreStep{Label: l, State: state})
	}
	return p
}

// restoreResultFor is the last restore's banner.
func restoreResultFor(r backup.Result, listing backup.Listing) *restoreResult {
	res := &restoreResult{
		Outcome: r.Outcome, Mode: r.Mode, BackupName: r.Backup.Name, BackupAt: r.Backup.CreatedAt,
		RequestedAt: r.RequestedAt, Previous: r.Previous,
		Saving: r.NeedsArchive() && r.PreviousError == "", PreviousError: r.PreviousError, PreviousDir: r.PreviousDir,
	}
	switch r.Outcome {
	case backup.RestoreDone:
		res.Tone = "ok"
		if r.Mode == backup.ModeMerge {
			// Empty when the backup held nothing missing here.
			// Only the kinds it added are named: "1 memory added", not "1
			// item added, among them 1 memory and 0 notes".
			if r.Added > 0 {
				var kinds []string
				if r.Memories > 0 {
					kinds = append(kinds, countOf(r.Memories, "memory", "memories"))
				}
				if r.Notes > 0 {
					kinds = append(kinds, countOf(r.Notes, "note", "notes"))
				}
				switch named := strings.Join(kinds, " and "); {
				case named != "" && r.Memories+r.Notes == r.Added:
					res.Counts = named + " added"
				case named != "":
					res.Counts = countOf(r.Added, "item", "items") + " added, among them " + named
				default:
					res.Counts = countOf(r.Added, "item", "items") + " added"
				}
			}
		} else {
			res.Counts = countOf(r.Memories, "memory", "memories") + " and " + countOf(r.Notes, "note", "notes")
		}
	case backup.RestorePartial:
		res.Tone = "warn"
		res.Message = sentenceStart(r.Message) + "."
	case backup.RestoreExpired:
		res.Tone = "warn"
	default:
		res.Tone = "danger"
		res.Message = nothingChanged(r.Message)
	}
	if r.Previous != "" {
		for _, e := range listing.Entries {
			if e.Name == r.Previous && e.Kind == archive.KindBeforeRestore {
				res.Undo = backupsSectionHref + "&restore=" + url.QueryEscape(e.Ref) + "#backup-restore"
				break
			}
		}
	}
	k := r.Kept
	if len(k.Tightened) > 0 {
		res.Kept = append(res.Kept, "Kept today's stricter privacy for "+joinWords(k.Tightened)+".")
	}
	if len(k.Detached) > 0 {
		res.Kept = append(res.Kept, "Detached from their parent project, so their locks hold: "+joinWords(k.Detached)+".")
	}
	if len(k.Registered) > 0 {
		res.Kept = append(res.Kept, "Kept the locked projects the backup did not have: "+joinWords(k.Registered)+".")
	}
	return res
}

// nothingChanged ends a refusal's sentence with "Nothing changed." unless it
// already says so.
func nothingChanged(msg string) string {
	msg = strings.TrimRight(strings.TrimSpace(msg), ".")
	if msg == "" {
		return "Nothing changed."
	}
	out := sentenceStart(msg) + "."
	if !strings.Contains(strings.ToLower(msg), "nothing changed") {
		out += " Nothing changed."
	}
	return out
}
