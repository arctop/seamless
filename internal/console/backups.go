package console

// The console's view of scheduled backups and restore (internal/backup):
// Settings > Backups (settings_backups.go and templates/settings_backups.html),
// the Backups row in Your setup, the every-page alert while automatic backups
// are in trouble, the System screen's Backups part, and the backup event
// kinds' words.
//
// The alert and the event lines are fixed words, counts and parsed times.
// The free text the section shows is the backup service's own: a run's or a
// restore's owner-facing sentence, a folder's problem and a file's path, all
// rendered escaped and only inside Settings > Backups.

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"time"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/backup"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// BackupsView is what the console needs of the backup service
// (*backup.Service in the daemon). Every command answers at once: a backup
// and a restore's preparation run as jobs, and the status follows them.
type BackupsView interface {
	Status() backup.Status
	Refresh(ctx context.Context) (backup.Status, error)
	BackUpNow(ctx context.Context) (backup.Status, error)
	RetryNow(ctx context.Context) (backup.Status, error)
	List(ctx context.Context) (backup.Listing, error)
	CheckFolder(dir string) backup.FolderCheck
	PrepareFolder(dir string) backup.FolderCheck
	Suggestions() []backup.Suggestion
	Delete(ctx context.Context, ref, by string) error
	StageRestore(ctx context.Context, req backup.RestoreRequest) (backup.Status, error)
	CancelRestore(ctx context.Context) (backup.Status, error)
	DismissResult(ctx context.Context) (backup.Status, error)
	Digest(ctx context.Context, ref string) (backup.Entry, string, error)
}

// The daemon's backup service is the one production BackupsView.
var _ BackupsView = (*backup.Service)(nil)

// backupsSectionHref is Settings > Backups, where every backup surface links.
const backupsSectionHref = "/console/settings?s=backups"

// backupsGOOS is the platform the restore words are for: Windows has no
// in-place restart, so a staged restore waits for `seamlessd restart`. A test
// sets it to see the other platform's words.
var backupsGOOS = runtime.GOOS

// restartsInPlace reports whether this daemon restarts by itself to finish a
// staged restore (everywhere but Windows).
func restartsInPlace() bool { return backupsGOOS != "windows" }

// restoreGated reports whether a console restore needs a passkey: under
// peer_auth with any project locked -- forced in the config file, locked when
// the daemon started, or locked from the console since. The bearer key is no
// proof of the owner then, and a restore could roll a locked project back. A
// failed read of the console's locks counts as locked: it fails closed.
func (s *Service) restoreGated(ctx context.Context) bool {
	if s.cfg.Approvals == nil {
		return false
	}
	if len(s.cfg.ForceLocked) > 0 || (s.cfg.Views != nil && len(s.cfg.Views.Lockable()) > 0) {
		return true
	}
	locks, err := store.ConsoleLocks(ctx, s.writeDB())
	if err != nil {
		s.logger.Warn("console: console locks unreadable; a restore needs a passkey", "error", err)
		return true
	}
	return len(locks) > 0
}

// everyWords says how often automatic backups run: "every hour", "every 6
// hours", "every day", "every week", "every 3 days".
func everyWords(d time.Duration) string {
	const day, week = 24 * time.Hour, 7 * 24 * time.Hour
	switch {
	case d == time.Hour:
		return "every hour"
	case d == day:
		return "every day"
	case d == week:
		return "every week"
	case d%week == 0:
		return fmt.Sprintf("every %d weeks", d/week)
	case d%day == 0:
		return fmt.Sprintf("every %d days", d/day)
	case d%time.Hour == 0:
		return fmt.Sprintf("every %d hours", d/time.Hour)
	}
	return "every " + config.Duration(d).String()
}

// keepWords says how many automatic backups stay: "keeping the newest 7".
func keepWords(n int) string {
	if n == 1 {
		return "keeping only the newest one"
	}
	return "keeping the newest " + strconv.Itoa(n)
}

// countOf is "1 memory", "1,203 sessions".
func countOf(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return groupDigits(n) + " " + many
}

// backupKindWords names why a backup was written, for its badge: the kind its
// manifest records, else the one its name implies.
func backupKindWords(e backup.Entry) string {
	switch e.Kind {
	case archive.KindScheduled:
		return "Automatic"
	case archive.KindManual:
		return "Made by you"
	case archive.KindBeforeRestore:
		return "Before restore"
	case archive.KindPreUpdate:
		if v := e.Parsed.FromVersion; v != "" {
			return "Before updating from v" + v
		}
		return "Before an update"
	}
	return "Exported"
}

// backupReasonWords is a failed run's fixed reason word in the owner's words,
// for the alert and the event lines. "" for a word with nothing to add.
func backupReasonWords(reason string) string {
	switch reason {
	case backup.ReasonNoSpace:
		return "the disk is too full"
	case backup.ReasonExport:
		return "the backup could not be written"
	case backup.ReasonReadBack:
		return "the backup did not read back as written"
	case backup.ReasonDatabase:
		return "the database could not be read"
	case backup.ReasonUnsafe:
		return "the disk does not keep backups private to Seamless"
	case "folder-" + string(backup.FolderDisconnected):
		return "the backup folder's disk is not connected"
	case "folder-" + string(backup.FolderMissing):
		return "the backup folder no longer exists"
	case "folder-" + string(backup.FolderNotWritable):
		return "the backup folder is not writable"
	case "folder-" + string(backup.FolderPrivacy):
		return "macOS is keeping Seamless out of the backup folder"
	case "folder-" + string(backup.FolderInsideDataDir):
		return "the backup folder is inside Seamless's data folder"
	case "folder-" + string(backup.FolderNotDirectory):
		return "the backup folder's path is not a folder"
	case "folder-" + string(backup.FolderUnsafe):
		return "the backup folder is not private to Seamless"
	case "folder-" + string(backup.FolderNotAbsolute), "folder-" + string(backup.FolderError):
		return "the backup folder cannot be used"
	}
	return ""
}

// restoreReasonWords is a failed restore's fixed reason word in the owner's
// words, for the event lines.
func restoreReasonWords(reason string) string {
	switch reason {
	case backup.RestoreReasonExpired:
		return "Seamless did not restart within an hour"
	case backup.RestoreReasonUnreadable:
		return "the backup did not verify"
	case backup.RestoreReasonChanged:
		return "the backup changed after it was approved"
	case backup.RestoreReasonTooNew:
		return "a newer Seamless made the backup"
	case backup.RestoreReasonNoSpace:
		return "the disk is too full"
	case backup.RestoreReasonSwap:
		return "the swap failed and was rolled back"
	case backup.RestoreReasonValidate:
		return "the restored data failed its checks"
	case backup.RestoreReasonMerge:
		return "the merge did not finish"
	}
	return ""
}

// backupAlert is the banner every page carries while automatic backups are in
// trouble: the last two automatic runs failed, or none has confirmed the
// backups current for three intervals (backup.Status.Alert). It points to
// Settings > Backups and is built from fixed words, counts and the reason's
// fixed word, never a run's error text.
type backupAlert struct {
	// Key names the trouble for the per-tab "Got it" (shell.js reads it as
	// data-update-banner): a new streak shows the alert again.
	Key        string
	Head, Line string
}

// backupAlertFor returns the alert for st at now, or nil.
func backupAlertFor(st backup.Status, now time.Time) *backupAlert {
	if !st.Alert(now) {
		return nil
	}
	since := st.AlertSince()
	a := &backupAlert{Key: "backups-" + strconv.FormatInt(since.Unix(), 10)}
	if st.Failures >= 2 {
		a.Head = "Automatic backups are failing."
		a.Line = fmt.Sprintf("The last %d did not finish, the first %s", st.Failures, agoPhrase(since))
		if why := backupReasonWords(st.LastReason); why != "" {
			a.Line += ": " + why
		}
		a.Line += "."
		return a
	}
	a.Head = "Automatic backups are overdue."
	if !st.LastConfirmed.IsZero() {
		a.Line = "The last one finished " + agoPhrase(st.LastConfirmed) + ", and they are set to run " + everyWords(st.Settings.Every) + "."
	} else {
		a.Line = "None has finished since they were turned on " + agoPhrase(since) + ", and they are set to run " + everyWords(st.Settings.Every) + "."
	}
	return a
}

// backupChrome is the alert for every page; nil when no backup service runs
// in this process.
func (s *Service) backupChrome(now time.Time) *backupAlert {
	if s.cfg.Backups == nil {
		return nil
	}
	return backupAlertFor(s.cfg.Backups.Status(), now)
}

// setupBackups is the Backups row in Settings > Your setup.
type setupBackups struct {
	On      bool
	Failing bool
	Line    string
}

// setupBackupsRow builds the Your setup row, nil without a backup service.
func (s *Service) setupBackupsRow(now time.Time) *setupBackups {
	if s.cfg.Backups == nil {
		return nil
	}
	st := s.cfg.Backups.Status()
	set := st.Settings
	row := &setupBackups{On: set.Auto, Failing: st.Alert(now)}
	if set.Auto {
		row.Line = everyWords(set.Every) + ", " + keepWords(set.Keep)
	} else {
		row.Line = "no automatic backups"
	}
	if b := st.LastBackup; b != nil {
		row.Line += " · last backup " + agoPhrase(b.At)
	}
	return row
}

// backupsPart is the System screen's Backups part: the backup service when
// one runs here, else the updater's pre-update archives (backupsArchivesPart).
func (s *Service) backupsPart() systemPart {
	if s.cfg.Backups == nil {
		return s.backupsArchivesPart()
	}
	st := s.cfg.Backups.Status()
	set := st.Settings
	p := systemPart{
		ID: "backups", Label: "Backups", Ring: ringCore, State: partIdle,
		Role:  "Backs everything up on a schedule, and restores a backup.",
		What:  "Automatic backups archive the database and every memory and note into the folder you choose, keeping the newest few. A restore saves today's data first, so it can be undone, and the updater takes its own backup before each update.",
		Hero:  systemStat{Value: "On", Label: everyWords(set.Every) + ", " + keepWords(set.Keep)},
		Facts: []systemFact{{"Folder", set.Dir}},
		Talks: []string{"updates", "database"},
		Href:  backupsSectionHref, HrefLabel: "Open Settings",
	}
	if b := st.LastBackup; b != nil {
		p.Facts = append(p.Facts, systemFact{"Last backup", agoPhrase(b.At)})
	}
	switch {
	case st.Stopped:
		p.State = partWarn
		p.Hero = systemStat{Value: "Stopped", Label: "the backup service stopped after an internal error"}
	case st.Alert(time.Now()) || st.Failures > 0:
		p.State = partWarn
		p.Hero.Label = countOf(st.Failures, "automatic backup failed", "automatic backups failed in a row")
		if st.Failures == 0 {
			p.Hero.Label = "automatic backups are overdue"
		}
	case !set.Auto:
		p.State = partOff
		p.Hero = systemStat{Value: "Off", Label: "automatic backups are off"}
		p.TurnOn = &systemHow{Text: "Turn them on in Settings, Backups."}
	case st.Running != nil:
		p.State = partOn
	}
	return p
}

// backupEventSummary is a backup.* event's ledger line, from its fixed words
// and counts.
func backupEventSummary(e core.Event) string {
	p := e.Payload
	switch e.Kind {
	case core.EventBackupCreated:
		line := "backup made"
		switch payloadStr(p, "kind") {
		case archive.KindScheduled:
			line = "automatic backup made"
		case archive.KindBeforeRestore:
			line = "safety backup made before a restore"
		}
		if m, n := payloadInt(p, "memories"), payloadInt(p, "notes"); m+n > 0 {
			line += " (" + countOf(m, "memory", "memories") + ", " + countOf(n, "note", "notes") + ")"
		}
		return line
	case core.EventBackupFailed:
		line := "a backup failed"
		if payloadStr(p, "kind") == archive.KindScheduled {
			line = "an automatic backup failed"
		}
		if why := backupReasonWords(payloadStr(p, "reason")); why != "" {
			line += ": " + why
		}
		return line
	case core.EventBackupDeleted:
		if payloadStr(p, "by") == "prune" {
			return "an old automatic backup was removed"
		}
		return "a backup was deleted"
	case core.EventBackupRestored:
		if payloadStr(p, "mode") == backup.ModeMerge {
			line := "brought back what was missing from a backup"
			if n := payloadInt(p, "added"); n > 0 {
				line += " (" + countOf(n, "item", "items") + " added)"
			}
			return line
		}
		return "restored a backup, replacing everything"
	case core.EventBackupRestoreFailed:
		line := "a restore did not finish"
		if payloadStr(p, "outcome") == backup.RestoreExpired {
			line = "a restore expired before Seamless restarted"
		} else if why := restoreReasonWords(payloadStr(p, "reason")); why != "" {
			line += ": " + why
		}
		return line
	}
	return string(e.Kind)
}

// What a console restore records about itself: who asked, and how it was
// approved without the gate (under it, approval.ProofPasskey and the passkey).
const (
	restoreBy         = "console"
	restoreProofOwner = "owner"
)
