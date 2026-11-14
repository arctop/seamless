package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// stateSchema is the state.json layout version. A reader ignores fields it
// does not know, so additions keep the number; a change an older binary would
// misread bumps it.
const stateSchema = 1

// StateDir is <data_dir>/update, the directory the update check owns.
func StateDir(dataDir string) string { return filepath.Join(dataDir, "update") }

// StatePath is the daemon's state file, <data_dir>/update/state.json.
func StatePath(dataDir string) string { return filepath.Join(StateDir(dataDir), "state.json") }

// State is what the checker keeps across restarts. Only a release daemon
// writes it (constraint dev-and-fixture-daemons-never-self-update); doctor and
// `seamlessd update --check` only read it. Versions are typed, so a value that
// is not a clean X.Y.Z fails to load rather than reach a notice.
//
// A release older than the automatic-update fields below reads this file
// fine (unknown fields are ignored) but rewrites it at its own start with
// only the fields it knows, so a downgrade to one forgets them all: a pending
// spawn, the last attempt, blocks, the hold, the pause, the backoff and its
// counters, the Codex fingerprint, and every cached release's
// ChecksumsBundle. ReleasesRev then reads zero, which re-reads the release
// list in full at the next check.
type State struct {
	Schema int `json:"schema"`

	// The release list as of the last successful check.
	CheckedAt   time.Time `json:"checked_at,omitzero"`
	ServerDate  time.Time `json:"server_date,omitzero"` // GitHub's Date header at CheckedAt
	NextCheckAt time.Time `json:"next_check_at,omitzero"`
	ETag        string    `json:"etag,omitempty"`
	Releases    []Release `json:"releases,omitempty"`
	// ReleasesRev is the filterRevision that produced Releases.
	ReleasesRev int `json:"releases_rev,omitempty"`
	// Seen is the newest release this install has been told about, and when
	// it was first seen: the briefing mentions an available update for a
	// limited time per version.
	Seen *Seen `json:"seen,omitempty"`

	CheckError *CheckError `json:"check_error,omitempty"`

	// Running is the daemon that wrote this file: doctor and the CLI read it
	// as the daemon's own view of itself.
	Running Running `json:"running,omitzero"`
	// LastRunVersion is the version the previous daemon ran; a different one
	// at startup is a version change (Updated).
	LastRunVersion *Version `json:"last_run_version,omitempty"`
	Updated        *Updated `json:"updated,omitempty"`

	// Automatic updates (plan 2.4). The checker's loop is their only writer,
	// like everything else here.

	// RecheckAt is the earliest the next pre-update re-check may ask GitHub:
	// saved before each such request, so a decision that keeps falling
	// through, or a crash loop, cannot spend the anonymous rate limit.
	RecheckAt time.Time `json:"recheck_at,omitzero"`
	// Pending anchors the max_defer deadline of a pending automatic update.
	Pending *Pending `json:"pending,omitempty"`
	// Spawn is the attempt handed to the Spawner and not yet folded.
	Spawn *Spawn `json:"spawn,omitempty"`
	// LastAttempt is the newest attempt the checker folded: one it spawned,
	// or one it only saw (a manual `seamlessd update`).
	LastAttempt *AttemptResult `json:"last_attempt,omitempty"`
	// Blocks are releases automatic updates skip (Target); a block on a
	// version at or below the running one is dropped as moot.
	Blocks []Block `json:"blocks,omitempty"`
	// Hold is the skip-through a deliberate downgrade set.
	Hold *Hold `json:"hold,omitempty"`
	// Paused is the latch that turns automatic updates off after repeated
	// rollbacks or a failed one. Settings.Auto, the owner's permission, is
	// untouched: Status.Mode honors the latch, and Resume lifts it.
	Paused *Pause `json:"paused,omitempty"`
	// Backoff delays the next automatic attempt after one that failed
	// without a block.
	Backoff *Backoff `json:"backoff,omitempty"`
	// Rollbacks are the releases automatic updates rolled back in a row,
	// oldest first; a successful update or Resume clears them.
	Rollbacks []Version `json:"rollbacks,omitempty"`
	// InstallFailures counts installer failures in a row on one release.
	InstallFailures *InstallFailures `json:"install_failures,omitempty"`
	// CodexHooks is the fingerprint of Codex's hooks.json this daemon last
	// saw (Deps.CodexHooksHash), at its start and before each spawn; "" is
	// unknown. The next daemon compares it on a version change.
	CodexHooks string `json:"codex_hooks,omitempty"`
	// SignatureRefusal is the last release update.release_signers refused,
	// from any attempt -- automatic, Update now or a manual `seamlessd
	// update` -- for Settings > Updates to name. Like LastAttempt it is
	// shown, never acted on: the block on the release is what keeps
	// automatic updates off it.
	SignatureRefusal *SignatureRefusal `json:"signature_refusal,omitempty"`
}

// SignatureRefusal is one release update.release_signers refused: the
// attempt's versions and id, its Signature* word, and when the checker
// folded it.
type SignatureRefusal struct {
	Version   Version   `json:"version"`
	From      Version   `json:"from"`
	Signature string    `json:"signature"`
	Attempt   string    `json:"attempt,omitempty"`
	At        time.Time `json:"at"`
}

// Pending anchors the max_defer deadline. It is tied to the running version,
// not to the release it would install, so a stream of new releases never
// restarts the wait; it is cleared when the running version changes, when no
// release is a target, and while the daemon does not update itself.
type Pending struct {
	Running Version `json:"running"`
	// Since is when the update became pending, on GitHub's clock
	// (serverClock), like the soak: a local clock that jumps cannot make the
	// deadline fire early by more than one check interval.
	Since time.Time `json:"since"`
}

// Spawn is an attempt the checker handed to its Spawner and has not folded
// yet. It is saved before Spawn is called, so a daemon that dies right after
// still knows what it started.
type Spawn struct {
	ID   string  `json:"id"`
	From Version `json:"from"`
	To   Version `json:"to"`
	Why  string  `json:"why"` // WhyAuto or WhyNow
	// Path is how the decision got there: PathIdle, PathDeadline,
	// PathForced, PathOverdue or PathNow.
	Path string    `json:"path,omitempty"`
	At   time.Time `json:"spawned_at"`
	// CodexHooks is the Codex hooks.json fingerprint just before the spawn.
	CodexHooks string `json:"codex_hooks,omitempty"`
}

// AttemptResult is an attempt as the checker folded it. Every field but
// Error and LogPath is a parsed version, an id or a fixed word; Error is the
// updater's own summary, for the owner's eyes only (console, doctor), never
// a notice or an event payload, and no surface branches on its content:
// Refusal is the typed reason a surface may act on.
type AttemptResult struct {
	ID   string  `json:"id"`
	From Version `json:"from"`
	To   Version `json:"to"`
	Why  string  `json:"why"`
	// Outcome is one of the Outcome* words.
	Outcome string `json:"outcome"`
	// Stage is where the attempt stopped, or StageSpawn ("spawn") when the
	// updater never wrote a record.
	Stage      Stage `json:"stage,omitempty"`
	RolledBack bool  `json:"rolled_back,omitempty"`
	// Warnings marks an applied update whose record carries an error: the
	// new version serves, and some client wiring may be stale.
	Warnings bool   `json:"warnings,omitempty"`
	Error    string `json:"error,omitempty"`
	// Refusal is the record's Refusal, kept as is: a word this release does
	// not know is a newer updater's. Read it through Refused.
	Refusal string `json:"refusal,omitempty"`
	// Signature is the record's Signature, kept as is: a word this release
	// does not know is a newer updater's. Read it through SignatureRefused.
	Signature string `json:"signature,omitempty"`
	LogPath   string `json:"log_path,omitempty"`
	// SpawnedAt is when this daemon spawned it (zero for one it did not),
	// StartedAt and FinishedAt what the record says, FoldedAt when the
	// checker settled it.
	SpawnedAt  time.Time `json:"spawned_at,omitzero"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	FoldedAt   time.Time `json:"folded_at"`
}

// Block keeps one release out of automatic updates (Target). Update now and
// a manual `seamlessd update` ignore it.
type Block struct {
	Version Version `json:"version"`
	// Reason is one of the Block* words.
	Reason  string    `json:"reason"`
	Attempt string    `json:"attempt,omitempty"`
	At      time.Time `json:"at"`
}

// Hold is what a deliberate downgrade leaves behind -- the owner pinned an
// older release (SEAMLESS_VERSION, or a manual `seamlessd update`) -- so an
// automatic update does not undo it: Target skips every release up to and
// including Through. Through is the version the owner went back from, or the
// newest release known at the time when that is newer, so only a release
// published after the downgrade is a candidate again. Resume and Update now
// lift it, and so does running Through or newer.
type Hold struct {
	Through Version   `json:"skip_through"`
	From    Version   `json:"from"` // the version the owner went back from
	At      time.Time `json:"at"`
}

// Pause is the latch that turns automatic updates off: two releases in a row
// rolled back (PauseRollbacks), or an update that could not be rolled back
// cleanly (PauseBroken). Config files are never edited, so it lives here;
// Resume lifts it, and so does an update the daemon starts (Update now) that
// applies.
type Pause struct {
	// Reason is one of the Pause* words.
	Reason string `json:"reason"`
	// Versions are the releases that caused it.
	Versions []Version `json:"versions,omitempty"`
	At       time.Time `json:"at"`
}

// Backoff delays automatic attempts after one that failed without a block:
// before anything changed, with the installer leaving the install as it was,
// or interrupted. The wait doubles from backoffBase per failure in a row, up
// to backoffMax; a new target restarts the count, a successful update clears
// it.
type Backoff struct {
	Until   time.Time `json:"until"`
	Count   int       `json:"count"`
	Version Version   `json:"version"` // the release the failures were on
	// Reason is the Outcome* word of the failure that set it.
	Reason string `json:"reason"`
}

// InstallFailures counts the installer failing in a row on one release with
// the install left as it was; installFailuresToBlock of them block it.
type InstallFailures struct {
	Version Version `json:"version"`
	Count   int     `json:"count"`
}

// Seen records when a release was first seen as the newest.
type Seen struct {
	Version Version   `json:"version"`
	At      time.Time `json:"at"`
}

// CheckError is the current streak of failed checks.
type CheckError struct {
	Kind    string    `json:"kind"` // CheckErrorRateLimited or CheckErrorUnavailable
	Message string    `json:"message"`
	Since   time.Time `json:"since"` // first failure of the streak
	Count   int       `json:"count"` // consecutive failures
}

// Check error kinds.
const (
	CheckErrorRateLimited = "rate_limited"
	CheckErrorUnavailable = "unavailable"
)

// Running describes one daemon process.
type Running struct {
	Version      Version   `json:"version"`
	Distribution string    `json:"distribution"`
	Kind         Kind      `json:"kind"`
	Reason       string    `json:"reason,omitempty"`
	Instance     string    `json:"instance"`
	PID          int       `json:"pid"`
	StartedAt    time.Time `json:"started_at"`
	// CanApply reports whether the daemon has an updater wired (a Spawner):
	// doctor reads it to tell automatic updates from notices (Status.CanApply).
	CanApply bool `json:"can_apply,omitempty"`
}

// Updated is a version change the daemon observed at startup.
type Updated struct {
	From      Version   `json:"from"`
	To        Version   `json:"to"`
	At        time.Time `json:"at"`
	Direction string    `json:"direction"` // DirectionUpgrade or DirectionDowngrade
	// CodexHooks reports that Codex's hooks.json changed across the version
	// change (its fingerprint before the update differs from the one after).
	// Codex runs changed hooks only once they are re-approved in its /hooks,
	// so the console and the briefing say so.
	CodexHooks bool `json:"codex_hooks_changed,omitempty"`
}

// Version-change directions.
const (
	DirectionUpgrade   = "upgrade"
	DirectionDowngrade = "downgrade"
)

// ReadState reads the state file without changing anything: the read-only
// path doctor and the CLI use. A missing file is a zero State, not an error.
func ReadState(dataDir string) (State, error) {
	raw, err := os.ReadFile(StatePath(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("update.ReadState: %w", err)
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return State{}, fmt.Errorf("update.ReadState: decode %s: %w", StatePath(dataDir), err)
	}
	return s, nil
}

// LoadState is the daemon's read at startup. A missing file is a zero State.
// A file that does not decode is renamed aside to state.json.corrupt-<utc> --
// its contents are kept for a human, never trusted again -- and the daemon
// starts from a zero State; quarantined names where it went. The error is for
// a file that could be neither read nor moved aside.
func LoadState(dataDir string, now time.Time) (s State, quarantined string, err error) {
	path := StatePath(dataDir)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, "", nil
	}
	if err != nil {
		return State{}, "", fmt.Errorf("update.LoadState: %w", err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		aside := path + ".corrupt-" + now.UTC().Format("20060102T150405Z")
		if rerr := os.Rename(path, aside); rerr != nil {
			return State{}, "", fmt.Errorf("update.LoadState: quarantine corrupt %s: %w", path, rerr)
		}
		return State{}, aside, nil
	}
	return s, "", nil
}

// SaveState writes s atomically: a temp file in the same directory, fsync,
// then rename over state.json, so a crash leaves the old file or the new one
// and never half of either. The file is 0600 in a 0700 directory: it holds no
// secret, but nothing about this install is anyone else's business.
func SaveState(dataDir string, s State) error {
	dir := StateDir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("update.SaveState: %w", err)
	}
	s.Schema = stateSchema
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("update.SaveState: encode: %w", err)
	}
	raw = append(raw, '\n')
	if err := writeFileAtomic(StatePath(dataDir), raw, 0o600); err != nil {
		return fmt.Errorf("update.SaveState: %w", err)
	}
	return nil
}

// writeFileAtomic writes data to path through a temp file in path's directory:
// write, fsync, close, rename. The temp file is removed on any failure.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(perm); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// clampDeadlines is Clamp's runtime half, for a clock that went back while
// the daemon ran: a deadline further ahead than anything ever sets one -- the
// next check past a rate limit's longest wait plus a jittered interval, a
// re-check past the post-update wait, a backoff past its longest, a spawn in
// the future -- is pulled in, so the schedule never stalls for as long as the
// clock jumped. A legitimate deadline is never touched. It reports whether
// anything changed.
func (s *State) clampDeadlines(now time.Time, interval time.Duration) bool {
	changed := false
	if s.NextCheckAt.After(now.Add(maxRetryWait + scheduleSlack(interval))) {
		s.NextCheckAt, changed = now, true
	}
	if s.RecheckAt.After(now.Add(postUpdateCheckDelay + firstCheckSpread + recheckSpacing)) {
		s.RecheckAt, changed = time.Time{}, true
	}
	if s.Backoff != nil && s.Backoff.Until.After(now.Add(backoffMax)) {
		s.Backoff.Until, changed = now.Add(backoffMax), true
	}
	if s.Spawn != nil && s.Spawn.At.After(now) {
		s.Spawn.At, changed = now, true
	}
	return changed
}

// scheduleSlack is the longest a release daemon goes between two successful
// checks when nothing fails: one interval, its +10% jitter, and the
// first-check spread. A saved schedule further out than that came from a
// wrong clock, and GitHub's clock is never extrapolated further than that
// from its last Date header (serverClock).
func scheduleSlack(interval time.Duration) time.Duration {
	return interval + interval/10 + firstCheckSpread
}

// Clamp drops timestamps a wrong clock left in the future, so a clock that
// was briefly days ahead cannot silence the check, freeze a notice or stall
// an automatic update: a scheduled check or re-check further away than it
// could legitimately be is rescheduled by the caller, a spawn time is pulled
// back to now (so its start window can pass), a backoff is cut to its
// longest, and the "happened at" times are pulled back to now. Pending.Since
// is on GitHub's clock, not this one, and is left alone.
func (s *State) Clamp(now time.Time, interval time.Duration) {
	if s.NextCheckAt.After(now.Add(scheduleSlack(interval))) {
		s.NextCheckAt = time.Time{}
	}
	if s.RecheckAt.After(now.Add(postUpdateCheckDelay + firstCheckSpread + recheckSpacing)) {
		s.RecheckAt = time.Time{}
	}
	if s.CheckedAt.After(now) {
		s.CheckedAt = now
	}
	if s.Seen != nil && s.Seen.At.After(now) {
		s.Seen.At = now
	}
	if s.CheckError != nil && s.CheckError.Since.After(now) {
		s.CheckError.Since = now
	}
	if s.Updated != nil && s.Updated.At.After(now) {
		s.Updated.At = now
	}
	if s.Spawn != nil && s.Spawn.At.After(now) {
		s.Spawn.At = now
	}
	if s.Backoff != nil && s.Backoff.Until.After(now.Add(backoffMax)) {
		s.Backoff.Until = now.Add(backoffMax)
	}
	if s.LastAttempt != nil && s.LastAttempt.FoldedAt.After(now) {
		s.LastAttempt.FoldedAt = now
	}
	if s.Hold != nil && s.Hold.At.After(now) {
		s.Hold.At = now
	}
	if s.Paused != nil && s.Paused.At.After(now) {
		s.Paused.At = now
	}
	for i := range s.Blocks {
		if s.Blocks[i].At.After(now) {
			s.Blocks[i].At = now
		}
	}
}
