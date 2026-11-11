package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// stateName is the scheduler's record in LocalDir. It is a dot file, so no
// listing shows it, and it lives beside the backups rather than in the
// database, so a restore that replaces the database keeps the schedule's
// history (and its failure streak) as it is.
const stateName = ".schedule.json"

// stateVersion is the record's shape; a record of another version is read as
// none.
const stateVersion = 1

// maxWritten caps state.Written: enough to remember every automatic backup
// MaxBackupKeep keeps, across a few folders.
const maxWritten = 512

// The outcomes of a backup run.
const (
	// OutcomeCreated: a backup was written and read back.
	OutcomeCreated = "created"
	// OutcomeUnchanged: a scheduled run found nothing changed since this
	// install's newest backup in the folder, and wrote none.
	OutcomeUnchanged = "unchanged"
	// OutcomeEmpty: a scheduled run found an empty instance, and wrote none.
	OutcomeEmpty = "empty"
	// OutcomeFailed: the run did not complete; Reason and Error say why.
	OutcomeFailed = "failed"
	// OutcomeCancelled: the daemon stopped mid-run. Not a failure, and not
	// recorded: the next start runs it again.
	OutcomeCancelled = "cancelled"
)

// Record is one backup the service wrote.
type Record struct {
	Name        string    `json:"name"`
	Dir         string    `json:"dir"`
	Kind        string    `json:"kind"`
	At          time.Time `json:"at"`
	Bytes       int64     `json:"bytes"`
	Memories    int       `json:"memories"`
	Notes       int       `json:"notes"`
	Projects    int       `json:"projects"`
	Fingerprint string    `json:"fingerprint,omitempty"`
}

// written is the trace of one backup the service wrote, kept so it can still
// recognize and prune its own automatic backups after a cloud folder has
// turned them into placeholders it will not open.
type written struct {
	Name        string    `json:"name"`
	Dir         string    `json:"dir"`
	At          time.Time `json:"at"`
	Fingerprint string    `json:"fingerprint,omitempty"`
}

// state is .schedule.json.
type state struct {
	Version int `json:"version"`
	// LastAttempt is when the last scheduled run started: the schedule counts
	// Every from it, success or not.
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	// LastConfirmed is the last time a run showed the backups current: a
	// backup written (scheduled or the owner's), or a scheduled run that found
	// nothing changed.
	LastConfirmed time.Time `json:"last_confirmed,omitzero"`
	// LastSuccess is when the last backup was written, and LastBackup what it
	// was.
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastBackup  *Record   `json:"last_backup,omitempty"`
	// LastOutcome is the last scheduled run's outcome.
	LastOutcome string `json:"last_outcome,omitempty"`
	// Failures counts the scheduled runs that failed in a row, the first of
	// them at FailingSince; LastReason (a fixed word) and LastError (the
	// owner-facing sentence) describe the newest.
	Failures     int       `json:"failures,omitempty"`
	FailingSince time.Time `json:"failing_since,omitzero"`
	LastReason   string    `json:"last_reason,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	// Since is when this install first ran with automatic backups on: the
	// baseline of the overdue alert before any run has confirmed.
	Since time.Time `json:"since,omitzero"`
	// Written traces the backups this service wrote, oldest first.
	Written []written `json:"written,omitempty"`
	// Folder is the folder in force at the last save, and InstallID this
	// install's id: what `seamlessd backups` lists without opening the
	// database (the console's chosen folder lives there).
	Folder    string `json:"folder,omitempty"`
	InstallID string `json:"install_id,omitempty"`
}

// ScheduleSummary is what doctor reads of the schedule's record.
type ScheduleSummary struct {
	LastConfirmed time.Time
	LastSuccess   time.Time
	// LastBackup is the newest backup's name.
	LastBackup   string
	Failures     int
	FailingSince time.Time
	LastError    string
}

// ReadSchedule reads the schedule's record for doctor; a missing record is the
// zero summary.
func ReadSchedule(dataDir string) (ScheduleSummary, error) {
	data, err := os.ReadFile(statePath(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return ScheduleSummary{}, nil
	}
	if err != nil {
		return ScheduleSummary{}, err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return ScheduleSummary{}, fmt.Errorf("%s: %w", statePath(dataDir), err)
	}
	sum := ScheduleSummary{
		LastConfirmed: st.LastConfirmed, LastSuccess: st.LastSuccess,
		Failures: st.Failures, FailingSince: st.FailingSince, LastError: st.LastError,
	}
	if st.LastBackup != nil {
		sum.LastBackup = st.LastBackup.Name
	}
	return sum, nil
}

// ReadFolderRecord is the folder the daemon last recorded backing up into and
// this install's id, from .schedule.json; both "" when it never recorded them.
func ReadFolderRecord(dataDir string) (folder, installID string) {
	data, err := os.ReadFile(statePath(dataDir))
	if err != nil {
		return "", ""
	}
	var st state
	if json.Unmarshal(data, &st) != nil || st.Version != stateVersion {
		return "", ""
	}
	return st.Folder, st.InstallID
}

func statePath(dataDir string) string { return filepath.Join(LocalDir(dataDir), stateName) }

// loadState reads the record. A missing one is a fresh start. A corrupt one is
// moved aside (and named) so the next save does not overwrite the evidence.
func loadState(dataDir string, now time.Time) (st state, quarantined string, err error) {
	path := statePath(dataDir)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return state{Version: stateVersion}, "", nil
	}
	if err != nil {
		return state{Version: stateVersion}, "", err
	}
	if jerr := json.Unmarshal(data, &st); jerr != nil || st.Version != stateVersion {
		moved := fmt.Sprintf("%s.corrupt-%s", path, now.UTC().Format(nameStamp))
		if rerr := os.Rename(path, moved); rerr != nil {
			return state{Version: stateVersion}, "", rerr
		}
		return state{Version: stateVersion}, moved, nil
	}
	return st, "", nil
}

// save writes the record through a temp file renamed into place, 0600.
func (st state) save(dataDir string) error {
	dir := LocalDir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, stateName+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, statePath(dataDir)); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// clamp pulls times a clock that went back left in the future to now, so the
// schedule is never stalled until the clock catches up.
func (st *state) clamp(now time.Time) bool {
	changed := false
	for _, t := range []*time.Time{&st.LastAttempt, &st.LastConfirmed, &st.LastSuccess, &st.FailingSince, &st.Since} {
		if t.After(now) {
			*t, changed = now, true
		}
	}
	return changed
}

// wrote reports whether the service recorded writing name into dir.
func (st state) wrote(dir, name string) bool {
	for _, w := range st.Written {
		if w.Name == name && sameDir(w.Dir, dir) {
			return true
		}
	}
	return false
}

// heldBackups reports whether the service ever wrote a backup into dir.
func (st state) heldBackups(dir string) bool {
	for _, w := range st.Written {
		if sameDir(w.Dir, dir) {
			return true
		}
	}
	return false
}

// writtenFingerprint is the fingerprint recorded for name in dir.
func (st state) writtenFingerprint(dir, name string) string {
	for _, w := range st.Written {
		if w.Name == name && sameDir(w.Dir, dir) {
			return w.Fingerprint
		}
	}
	return ""
}

// addWritten traces a new backup, keeping the newest maxWritten.
func (st *state) addWritten(w written) {
	st.Written = append(st.Written, w)
	if n := len(st.Written) - maxWritten; n > 0 {
		st.Written = append([]written(nil), st.Written[n:]...)
	}
}

// forgetWritten drops the trace of a backup that is gone.
func (st *state) forgetWritten(dir, name string) {
	out := st.Written[:0]
	for _, w := range st.Written {
		if !(w.Name == name && sameDir(w.Dir, dir)) {
			out = append(out, w)
		}
	}
	st.Written = out
}

// clone deep-copies the record, for a job that reads it off the loop.
func (st state) clone() state {
	c := st
	c.Written = append([]written(nil), st.Written...)
	if st.LastBackup != nil {
		b := *st.LastBackup
		c.LastBackup = &b
	}
	return c
}
