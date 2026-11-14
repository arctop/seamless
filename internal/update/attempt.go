package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// Attempt timing. The checker classifies a spawned attempt by these (plan
// 2.4); the updater heartbeats well inside them.
const (
	// AttemptHeartbeatInterval is the longest a running updater goes without
	// re-writing attempt.json. It heartbeats from a timer, not between steps:
	// one step (the installer, the wait to confirm) can take minutes.
	AttemptHeartbeatInterval = 30 * time.Second
	// AttemptHeartbeatStale is how old a heartbeat may get before its attempt
	// stops being Live: ten missed heartbeats. An unfinished attempt that is
	// not Live was interrupted -- killed, crashed, or asleep with its machine.
	AttemptHeartbeatStale = 5 * time.Minute
	// AttemptStartWindow is how long after a spawn the checker waits for
	// attempt.json to name the new attempt. A spawn that has not shown up by
	// then was interrupted before it began (plan 2.4: "a spawn with no
	// attempt file after 5 min").
	AttemptStartWindow = 5 * time.Minute
)

// History bounds. AppendAttempt compacts attempts.jsonl once it passes
// historyCompactAt, and a reader never reads more than historyReadMax of its
// end, so neither the file nor a read of it grows without bound.
const (
	historyKeep      = 100       // lines a compaction keeps, the newest
	historyCompactAt = 64 << 10  // bytes past which an append compacts
	historyReadMax   = 256 << 10 // bytes a reader reads, from the end
	attemptErrorMax  = 1000      // runes of Attempt.Error a writer keeps
)

// ErrInvalidAttempt is a spawn request or attempt record this release would
// not act on or write: SpawnRequest.Validate, WriteAttempt and AppendAttempt
// refuse with it.
var ErrInvalidAttempt = errors.New("update: invalid update attempt")

// AttemptPath is the newest attempt, <data_dir>/update/attempt.json.
func AttemptPath(dataDir string) string { return filepath.Join(StateDir(dataDir), "attempt.json") }

// AttemptHistoryPath is the attempt history, <data_dir>/update/attempts.jsonl.
func AttemptHistoryPath(dataDir string) string {
	return filepath.Join(StateDir(dataDir), "attempts.jsonl")
}

// LockPath is the updater's lock, <data_dir>/update/update.lock. The updater
// holds an exclusive OS lock on it (flock, LockFileEx) for its whole run, so
// the OS releases it however the updater dies, and only the holder writes
// attempt.json and attempts.jsonl. The file is never deleted: removing a lock
// file while one process holds it lets a second lock a new file of the same
// name.
func LockPath(dataDir string) string { return filepath.Join(StateDir(dataDir), "update.lock") }

// Why an attempt runs. A why is a fixed word, never free text, so it may sit
// in an event payload as is.
const (
	// WhyAuto is an attempt the daemon's checker started to install a newer
	// release. It only ever moves forward.
	WhyAuto = "auto"
	// WhyManual is an attended `seamlessd update` the owner ran by hand. The
	// daemon never spawns one, so no pending spawn ever matches it, and it may
	// move in either direction: an owner may pin an older release.
	WhyManual = "manual"
	// WhyNow is an attempt the daemon's checker started because the owner
	// asked for it from the daemon (the console's "Update now",
	// Checker.ApplyNow): the newest release, at once, without the soak, the
	// idle wait or blocks, and whatever update.auto says. The daemon spawns
	// it like WhyAuto, and like WhyAuto it only moves forward.
	WhyNow = "now"
)

// whys lists every why this release writes.
var whys = []string{WhyAuto, WhyManual, WhyNow}

// Stage is the step of plan 2.2 an attempt is in, or stopped in. A finished
// attempt's stage is where it stopped: StageDone for a success and only then,
// the failing step for a failure before StageInstall, and StageRollback for a
// failure at or after StageInstall, once the rollback has begun.
type Stage string

const (
	// StageLock: the updater holds LockPath and runs its self-checks.
	StageLock Stage = "lock"
	// StageGates: checking that an unattended install is still allowed now.
	StageGates Stage = "gates"
	// StageFetch: downloading the installer script and its Sigstore bundle.
	// A failure here is the network's, not the release's.
	StageFetch Stage = "fetch"
	// StageVerify: checking what was fetched against its signature and pins.
	// A failure here is a signature failure or a pin mismatch.
	StageVerify Stage = "verify"
	// StagePreflight: proving a rollback would work before anything changes.
	StagePreflight Stage = "preflight"
	// StageBackup: copying the running install aside, to BackupPath.
	StageBackup Stage = "backup"
	// StageInstall: running the installer, the first step that changes the
	// install.
	StageInstall Stage = "install"
	// StageConfirm: waiting to observe the restarted daemon running To.
	StageConfirm Stage = "confirm"
	// StageRollback: restoring From after a failure at or after StageInstall.
	StageRollback Stage = "rollback"
	// StageDone: To is installed and confirmed running.
	StageDone Stage = "done"
)

// Stages lists every stage in the order an attempt moves through them.
// StageRollback runs only after a failure at or after StageInstall, and
// StageDone follows StageConfirm only.
var Stages = []Stage{
	StageLock, StageGates, StageFetch, StageVerify, StagePreflight,
	StageBackup, StageInstall, StageConfirm, StageRollback, StageDone,
}

// Valid reports whether this release knows s. A reader keeps a stage it does
// not know as is: a newer release may have added it.
func (s Stage) Valid() bool { return slices.Contains(Stages, s) }

// Why the updater's gates refused an attempt the daemon started (Refusal). A
// refusal is a fixed word, never free text, so it may sit in an event payload
// as is, and a surface keys its own fixed words on it (RefusalWords,
// RefusalAction) -- never on the record's Error. The attended `seamlessd
// update` runs no gates and never records one. A refusal a reader does not
// know (a newer updater's) reads as a plain failure at StageGates.
const (
	// RefusalNotInstaller: Detect, re-run by the updater, no longer finds an
	// install the installer manages -- it changed since the daemon started.
	RefusalNotInstaller = "not_installer"
	// RefusalStaleBinary: the installed seamlessd is not the release the
	// daemon runs (From), so the service runs a binary the disk no longer
	// holds -- a release installed without a restart.
	RefusalStaleBinary = "stale_binary"
	// RefusalSelfCheck: the updater could not make sure it runs outside the
	// service's process tree, which the installer restarts.
	RefusalSelfCheck = "self_check"
	// RefusalAutoOff: the owner turned automatic updates off after the daemon
	// decided to start this one (WhyAuto only).
	RefusalAutoOff = "auto_off"
	// RefusalRestorePending: a restore is staged or under way
	// (<data_dir>/restore/pending.json), and an update must not replace the
	// binary or back up the data under it.
	RefusalRestorePending = "restore_pending"
	// RefusalReleaseSigners: update.release_signers names a file the updater
	// cannot use -- missing, unreadable, open to another account's writes,
	// or naming no key for the release namespace -- so no release could be
	// held to the owner's keys. Nothing is fetched; the release is not
	// blocked, since the fault is the file's.
	RefusalReleaseSigners = "release_signers"
)

// refusals lists every refusal this release writes.
var refusals = []string{RefusalNotInstaller, RefusalStaleBinary, RefusalSelfCheck, RefusalAutoOff, RefusalRestorePending,
	RefusalReleaseSigners}

// Why update.release_signers -- the opt-in pin to the owner's release keys
// -- refused a release (Attempt.Signature): a fixed word, like a refusal,
// for an event payload and the surfaces' own words (SignatureWords). It is
// set on a finished attempt that stopped at StageVerify, attended or not.
const (
	// SignatureMissing: the release carries no checksums.txt.sshsig
	// (OwnerSignatureAsset), so the owner never signed it.
	SignatureMissing = "missing"
	// SignatureUnlisted: its checksums.txt.sshsig was made by a key the pin
	// does not list.
	SignatureUnlisted = "unlisted"
	// SignatureInvalid: its checksums.txt.sshsig does not verify -- not a
	// signature, made for another namespace or another file, or by a
	// security key that was not touched or did not verify its owner.
	SignatureInvalid = "invalid"
)

// signatureWords lists every Signature word this release writes.
var signatureWords = []string{SignatureMissing, SignatureUnlisted, SignatureInvalid}

// KnownSignature reports whether this release knows the Signature word w.
func KnownSignature(w string) bool { return slices.Contains(signatureWords, w) }

// KnownRefusal reports whether this release knows refusal.
func KnownRefusal(refusal string) bool { return slices.Contains(refusals, refusal) }

// PreSwap reports whether s comes before StageInstall, so an attempt that
// stopped there left the install untouched. A stage this release does not
// know is not pre-swap: nothing vouches that the install was not touched.
func (s Stage) PreSwap() bool {
	i := slices.Index(Stages, s)
	return i >= 0 && i < slices.Index(Stages, StageInstall)
}

// Attempt is one run of the updater, `seamlessd update --auto`, as it records
// itself in two files under StateDir, both 0600:
//
//   - attempt.json (AttemptPath) is the newest attempt. The updater writes it
//     once it holds the lock, again at every stage change and at least every
//     AttemptHeartbeatInterval (each write stamping HeartbeatAt), and a last
//     time with the outcome. Every write replaces the file atomically.
//   - attempts.jsonl (AttemptHistoryPath) is the history: after that last
//     write the updater appends the same final record as one line. A line is
//     never changed; compaction only drops the oldest.
//
// Only the process holding the lock at LockPath writes either file; the
// daemon only reads them. The daemon's checker matches attempt.json to the
// attempt it spawned by ID and reads the outcome from the fields: Live while
// the updater runs; once Finished, OK is an applied update, RolledBack a
// failure the updater undid, and otherwise Stage is where it failed
// (Stage.PreSwap: before anything was installed). An unfinished attempt that
// is not Live was interrupted.
//
// Every release from the floor on reads every other one's files, in both
// directions, so the layout is frozen: a release may add a field, never
// rename, retype or repurpose one, and there is no schema number to bump.
// Readers ignore fields they do not know, read missing ones as zero, and keep
// a stage or why they do not know as is. Writers are strict, readers are not.
type Attempt struct {
	// ID is the attempt's ULID: the checker's SpawnRequest.AttemptID for an
	// attempt it spawned, minted by `seamlessd update` itself for a manual one.
	ID string `json:"id"`
	// From is the version the daemon ran when the attempt began; To is the
	// release it installs.
	From Version `json:"from"`
	To   Version `json:"to"`
	// Why the attempt runs: WhyAuto, WhyNow or WhyManual.
	Why string `json:"why"`
	// StartedAt is when the updater took the lock, HeartbeatAt its latest
	// write, and FinishedAt when it recorded the outcome (zero while it runs).
	StartedAt   time.Time `json:"started_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	FinishedAt  time.Time `json:"finished_at,omitzero"`
	// OK means To is installed and confirmed running; it is set exactly when
	// Stage is StageDone. RolledBack means the attempt failed at or after
	// StageInstall and the updater restored From.
	OK         bool `json:"ok"`
	RolledBack bool `json:"rolled_back"`
	// Stage is the step the attempt is in, or where it stopped.
	Stage Stage `json:"stage"`
	// Error says why the attempt failed: the updater's own summary, capped at
	// attemptErrorMax runes on write, with the full output in LogPath. It is
	// for the owner's eyes (console, doctor); notices and event payloads name
	// versions, stages and refusals only, and no surface branches on its
	// content (Refusal is the typed reason).
	Error string `json:"error,omitempty"`
	// Refusal is set when the gates refused the attempt: a Refusal* word, on a
	// finished record that stopped at StageGates, for an attempt the daemon
	// started. Empty for every other outcome, and in every record a release
	// before it wrote.
	Refusal string `json:"refusal,omitempty"`
	// Signature is set when update.release_signers refused the release: a
	// Signature* word, on a finished record that stopped at StageVerify.
	// Empty for every other outcome, on an install with no pin, and in
	// every record a release before it wrote.
	Signature string `json:"signature,omitempty"`
	// LogPath is the updater's log of this attempt, and BackupPath where
	// StageBackup put the install it replaces: absolute paths the updater
	// chose, empty until it has them.
	LogPath    string `json:"log_path,omitempty"`
	BackupPath string `json:"backup_path,omitempty"`
}

// Finished reports whether the updater recorded an outcome: OK, RolledBack,
// Stage, Error and Refusal are then final.
func (a Attempt) Finished() bool { return !a.FinishedAt.IsZero() }

// Live reports whether the attempt is still running at now: not Finished,
// with a heartbeat less than AttemptHeartbeatStale old. A heartbeat that far
// in the future is stale too -- the clock went back after it was written, and
// a heartbeat that cannot be placed in time vouches for nothing -- so a clock
// change cannot keep a dead attempt live. The zero Attempt is never live.
func (a Attempt) Live(now time.Time) bool {
	if a.Finished() || a.HeartbeatAt.IsZero() {
		return false
	}
	age := now.Sub(a.HeartbeatAt)
	return age < AttemptHeartbeatStale && age > -AttemptHeartbeatStale
}

// ReadAttempt reads attempt.json. A missing file is the zero Attempt (no
// attempt has run here), not an error. Fields this release does not know are
// ignored and missing ones read as zero. A file that does not decode is an
// error; nothing repairs it but the next attempt's first write, since the
// daemon never writes it.
func ReadAttempt(dataDir string) (Attempt, error) {
	path := AttemptPath(dataDir)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Attempt{}, nil
	}
	if err != nil {
		return Attempt{}, fmt.Errorf("update.ReadAttempt: %w", err)
	}
	var a Attempt
	if err := json.Unmarshal(raw, &a); err != nil {
		return Attempt{}, fmt.Errorf("update.ReadAttempt: decode %s: %w", path, err)
	}
	return a, nil
}

// WriteAttempt replaces attempt.json with a, atomically (temp file, fsync,
// rename), in a 0700 StateDir and 0600 like the state file. It refuses a
// record this release would not write (ErrInvalidAttempt) and caps Error. The
// caller holds the lock at LockPath and stamps HeartbeatAt.
//
// On Windows the rename fails while another process holds the file open
// without delete sharing, as os.Open and os.ReadFile open it, so the updater
// leaves a failed heartbeat to the next one and retries the final write.
func WriteAttempt(dataDir string, a Attempt) error {
	raw, err := a.encode(true)
	if err != nil {
		return fmt.Errorf("update.WriteAttempt: %w", err)
	}
	if err := os.MkdirAll(StateDir(dataDir), 0o700); err != nil {
		return fmt.Errorf("update.WriteAttempt: %w", err)
	}
	if err := writeFileAtomic(AttemptPath(dataDir), append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("update.WriteAttempt: %w", err)
	}
	return nil
}

// AppendAttempt adds a's final record to attempts.jsonl as one line, written
// with O_APPEND in a single write and synced, in a 0700 StateDir and 0600. It
// refuses what WriteAttempt refuses. A file that does not end in a newline
// lost the end of its last line to a crash; the record then starts on a line
// of its own, so the torn one stays a single skipped line. Past
// historyCompactAt the file is rewritten to its newest historyKeep lines,
// byte for byte -- never decoded and re-encoded, so an older release cannot
// strip a newer one's fields -- and a compaction that fails is left to the
// next append. The caller holds the lock at LockPath.
func AppendAttempt(dataDir string, a Attempt) error {
	rec, err := a.encode(false)
	if err != nil {
		return fmt.Errorf("update.AppendAttempt: %w", err)
	}
	if err := os.MkdirAll(StateDir(dataDir), 0o700); err != nil {
		return fmt.Errorf("update.AppendAttempt: %w", err)
	}
	path := AttemptHistoryPath(dataDir)
	size, err := appendLine(path, rec)
	if err != nil {
		return fmt.Errorf("update.AppendAttempt: %w", err)
	}
	if size > historyCompactAt {
		// The record is on disk either way: a failed compaction leaves a longer
		// file, and readers stop at historyReadMax regardless.
		_ = compactHistory(path) //nolint:errcheck // the record is appended; the next append compacts again
	}
	return nil
}

// ReadAttemptHistory reads attempts.jsonl, oldest first. A missing file is no
// history. A line that does not decode -- torn by a crash mid-append, corrupt,
// a from or to that is not X.Y.Z, no id -- is skipped and counted in skipped,
// never fatal; the caller reports the count. Blank lines are ignored. An id
// can appear twice (an append retried after an error); the later line is the
// newer. Only the newest historyReadMax bytes are read, from the first whole
// line in them.
func ReadAttemptHistory(dataDir string) (attempts []Attempt, skipped int, err error) {
	raw, err := readTail(AttemptHistoryPath(dataDir), historyReadMax)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("update.ReadAttemptHistory: %w", err)
	}
	for line := range bytes.Lines(raw) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var a Attempt
		if json.Unmarshal(line, &a) != nil || a.ID == "" {
			skipped++
			continue
		}
		attempts = append(attempts, a)
	}
	return attempts, skipped, nil
}

// encode checks a and renders it for a writer, Error capped: indented for
// attempt.json, one line for the history.
func (a Attempt) encode(indent bool) ([]byte, error) {
	if err := a.check(); err != nil {
		return nil, err
	}
	a.Error = core.TruncateWords(a.Error, attemptErrorMax)
	if indent {
		return json.MarshalIndent(a, "", "  ")
	}
	return json.Marshal(a)
}

// check refuses a record this release would not write. Readers never call
// it: a newer release's record may well fail it.
func (a Attempt) check() error {
	if err := (SpawnRequest{AttemptID: a.ID, From: a.From, To: a.To, Why: a.Why}).Validate(); err != nil {
		return err
	}
	var problem string
	switch {
	case !a.Stage.Valid():
		problem = fmt.Sprintf("stage %q: valid values are %s", a.Stage, stageNames())
	case a.StartedAt.IsZero() || a.HeartbeatAt.IsZero():
		problem = "started_at and heartbeat_at are required"
	case a.OK != (a.Stage == StageDone):
		problem = "ok is set exactly when the stage is done"
	case a.OK && !a.Finished():
		problem = "a successful attempt is finished"
	case a.RolledBack && a.Stage != StageRollback:
		problem = "rolled_back is set only in the rollback stage"
	case a.Refusal != "" && !KnownRefusal(a.Refusal):
		problem = fmt.Sprintf("refusal %q: valid values are %s", a.Refusal, strings.Join(refusals, ", "))
	case a.Refusal != "" && (a.Stage != StageGates || !a.Finished()):
		problem = "refusal is set only on a finished attempt that stopped at the gates"
	case a.Refusal != "" && !spawnedByDaemon(a.Why):
		problem = "refusal is set only on an attempt the daemon started: an attended update runs no gates"
	case a.Signature != "" && !KnownSignature(a.Signature):
		problem = fmt.Sprintf("signature %q: valid values are %s", a.Signature, strings.Join(signatureWords, ", "))
	case a.Signature != "" && (a.Stage != StageVerify || !a.Finished()):
		problem = "signature is set only on a finished attempt that stopped at verify"
	default:
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalidAttempt, problem)
}

// stageNames lists Stages for an error message.
func stageNames() string {
	names := make([]string, len(Stages))
	for i, s := range Stages {
		names[i] = string(s)
	}
	return strings.Join(names, ", ")
}

// appendLine appends rec and a newline to path in one write -- after a
// newline of its own when the file does not end in one -- syncs, and returns
// the file's new size.
func appendLine(path string, rec []byte) (int64, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return 0, err
	}
	line := make([]byte, 0, len(rec)+2)
	if fi.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], fi.Size()-1); err != nil {
			_ = f.Close()
			return 0, err
		}
		if last[0] != '\n' {
			line = append(line, '\n')
		}
	}
	line = append(append(line, rec...), '\n')
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return 0, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	return fi.Size() + int64(len(line)), nil
}

// compactHistory rewrites the history at path to its newest historyKeep
// non-blank lines, byte for byte, atomically.
func compactHistory(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var lines [][]byte
	for line := range bytes.Lines(raw) {
		if len(bytes.TrimSpace(line)) > 0 {
			lines = append(lines, line)
		}
	}
	if len(lines) <= historyKeep {
		return nil
	}
	out := bytes.Join(lines[len(lines)-historyKeep:], nil)
	if !bytes.HasSuffix(out, []byte{'\n'}) {
		out = append(out, '\n')
	}
	return writeFileAtomic(path, out, 0o600)
}

// readTail returns the file at path, or when it is longer than limit only
// its last limit bytes, from the first whole line in them.
func readTail(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() <= limit {
		return io.ReadAll(f)
	}
	// Read one byte before the window too: when it is a newline, the window
	// starts on a line boundary and its first line is whole.
	buf := make([]byte, limit+1)
	n, err := f.ReadAt(buf, fi.Size()-limit-1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	buf = buf[:n]
	i := bytes.IndexByte(buf, '\n')
	if i < 0 {
		return nil, nil // one line longer than the window: nothing whole in it
	}
	return buf[i+1:], nil
}

// crockford is the alphabet of a ULID's text form, as core.NewID writes it.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ValidAttemptID reports whether id has the exact shape core.NewID mints: 26
// characters of upper-case Crockford base32, the first at most '7' (a ULID is
// 128 bits). An attempt id travels on a command line and names files, so
// nothing looser passes.
func ValidAttemptID(id string) bool {
	if len(id) != 26 || id[0] > '7' {
		return false
	}
	for i := range len(id) {
		if strings.IndexByte(crockford, id[i]) < 0 {
			return false
		}
	}
	return true
}

// SpawnRequest names the attempt a Spawner starts. The checker fills it once
// it has re-checked the target, minted AttemptID with core.NewID and recorded
// update.started; the spawner hands all of it to the updater, which copies it
// into every Attempt it writes.
type SpawnRequest struct {
	AttemptID string  // ValidAttemptID
	From      Version // the version the daemon runs
	To        Version // the release to install
	Why       string  // a why this release knows; the checker spawns WhyAuto, or WhyNow for Update now
}

// Validate refuses a request no updater should start for. The attempt id
// must have the shape core.NewID mints, since a spawner puts it on a command
// line (on Windows inside a scheduled task's one argument string) and the
// updater names files after it; both versions must be set; Why must be a word
// this release knows; and an attempt the daemon starts (WhyAuto, WhyNow) only
// moves forward, because an update never auto-downgrades.
func (r SpawnRequest) Validate() error {
	var problem string
	switch {
	case !ValidAttemptID(r.AttemptID):
		problem = fmt.Sprintf("attempt id %q is not a ULID", r.AttemptID)
	case r.From.IsZero() || r.To.IsZero():
		problem = "from and to are required"
	case !slices.Contains(whys, r.Why):
		problem = fmt.Sprintf("why %q: valid values are %s", r.Why, strings.Join(whys, ", "))
	case (r.Why == WhyAuto || r.Why == WhyNow) && r.To.Compare(r.From) <= 0:
		problem = fmt.Sprintf("an update the daemon starts only moves forward, not from %s to %s", r.From, r.To)
	default:
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalidAttempt, problem)
}

// Spawner starts the updater for one attempt, `seamlessd update --auto
// --attempt <id>`, as a process that outlives the daemon: the installer it
// runs restarts that daemon, so it runs in its own session, transient unit or
// scheduled task, never as a child that dies with the daemon or with ctx.
// cmd/seamlessd wires one per OS.
//
// Spawn returns once the OS has the updater, not when it finishes. nil means
// it was started, not that it ran: the evidence of that is an attempt.json
// naming req.AttemptID, which the checker waits AttemptStartWindow for. An
// error means it was not started as far as the spawner can tell. ctx bounds
// the hand-off, never the updater's life. An implementation refuses a request
// that fails Validate before it builds anything from it.
type Spawner interface {
	Spawn(ctx context.Context, req SpawnRequest) error
}
