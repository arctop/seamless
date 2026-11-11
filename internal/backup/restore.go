package backup

// Restore: putting a backup back, as a replacement of everything or as a
// merge of what is missing, with today's locks, approvers and settings kept.
//
// The heavy work runs while the daemon still serves (Stager.Stage), so a
// failure there changes nothing and is reported before any restart: the
// backup is verified and copied into <data_dir>/restore/<id>/, a replacement
// is imported fresh into restore/<id>/new/ and checked there, a merge takes a
// safety backup of today's data first. Then restore/pending.json says the
// restore is staged, and the daemon restarts.
//
// Startup (Applier.Apply, after the data dir lock and before anything opens
// the database) finishes it: a replacement writes today's kept state into the
// staged database, checks it again, records the swap ahead of it ("swapping",
// with every entry it moves), renames today's entries into
// restore/<id>/previous/ and the staged ones into the data dir, and commits by
// recording "applied". A merge folds the verified copy in, idempotently by id,
// at most maxMergeAttempts times. A start that finds a swap mid-way rolls it
// back entry by entry; one that finds it applied only finishes the
// bookkeeping. Nothing under restore/ is deleted before its data is verified
// elsewhere.
//
// After serving (ArchivePrevious) the replaced data in previous/ becomes a
// before-restore backup in LocalDir, read back before previous/ is removed:
// the owner's undo.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Restore modes.
const (
	// ModeReplace puts the backup in place of everything: today's data is set
	// aside, and becomes a before-restore backup.
	ModeReplace = "replace"
	// ModeMerge adds what the backup has and this install does not; nothing
	// already here changes.
	ModeMerge = "merge"
)

// The stages of restore/pending.json.
const (
	// StageStaged (StagePending): ready to apply at the next start.
	StageStaged = StagePending
	// StageSwapping: a replacement's swap began. Entries names what moves; a
	// start that finds this stage rolls the swap back.
	StageSwapping = "swapping"
	// StageApplied: the swap is complete; this record is the commit point,
	// and a start that finds it only finishes the bookkeeping.
	StageApplied = "applied"
	// StageMerging: a merge began; Attempts counts the starts that ran it.
	StageMerging = "merging"
)

// The outcomes a restore comes to (Result.Outcome).
const (
	RestoreDone    = "restored"
	RestoreFailed  = "failed"
	RestoreExpired = "expired"
	// RestorePartial is a merge that stopped after adding some of the backup:
	// nothing was removed, and what was added stays.
	RestorePartial = "partial"
)

// The fixed words a failed restore records (Result.Reason, events, doctor).
// A stage refusal's are Failure.Reason too.
const (
	RestoreReasonPending       = "restore-pending"   // another restore is staged
	RestoreReasonBusy          = "busy"              // a backup or an update is under way
	RestoreReasonUnreadable    = "unreadable"        // the backup did not verify
	RestoreReasonChanged       = "changed"           // the backup is not the one the owner approved
	RestoreReasonTooNew        = "too-new"           // a newer Seamless made it
	RestoreReasonNoDatabase    = "no-database"       // a knowledge-only backup cannot replace everything
	RestoreReasonDataless      = "dataless"          // an iCloud placeholder, not downloaded
	RestoreReasonImport        = "import"            // the backup could not be prepared
	RestoreReasonKept          = "kept-state"        // today's locks and settings could not be carried over
	RestoreReasonValidate      = "validation"        // the prepared data failed its checks
	RestoreReasonSafety        = "safety-backup"     // today's data could not be backed up first
	RestoreReasonMissing       = "staged-missing"    // the prepared data is gone
	RestoreReasonSwap          = "swap"              // the swap failed, and was rolled back
	RestoreReasonMerge         = "merge"             // the merge failed
	RestoreReasonExpired       = "expired"           // not applied within restoreExpiry
	RestoreReasonUnsupported   = "lock-unsupported"  // the data dir lock is unavailable, so no swap is safe
	RestoreReasonRecord        = "record-unreadable" // pending.json could not be read
	RestoreReasonInternal      = "internal"
	RestoreReasonNoSpace       = ReasonNoSpace
	RestoreReasonNotRestorable = "not-restorable" // damaged, unreadable or not a backup
)

// restoreExpiry is how long a staged restore may wait for the restart that
// applies it. A restore staged and then forgotten (Windows, where the owner
// restarts by hand) must not replace the data hours later, under work done
// since.
const restoreExpiry = time.Hour

// maxMergeAttempts is how many starts may run one merge: a merge is
// idempotent by id, so a crash re-runs it, but one that crashes the daemon
// every time must not keep it from serving.
const maxMergeAttempts = 3

// The names under <data_dir>/restore.
const (
	LastName        = "last.json"
	copyName        = "archive.tar.gz"
	newDirName      = "new"
	previousDirName = "previous"
	dryDirName      = "dry"
	recordVersion   = 1
)

// swapEntries are the data dir entries a replacement moves, in order: the
// database and its companions, then the two markdown trees. Everything else
// in the data dir (the lock, the backups, the update and restore records, the
// feed spool) stays.
var swapEntries = []string{"seam.db", "seam.db-wal", "seam.db-shm", "seam.db.import-tmp", "memory", "notes"}

// RestoreRoot is <dataDir>/restore.
func RestoreRoot(dataDir string) string { return filepath.Join(dataDir, RestoreDirName) }

// LastPath is <dataDir>/restore/last.json: the last restore's result.
func LastPath(dataDir string) string { return filepath.Join(dataDir, RestoreDirName, LastName) }

// workDir is <dataDir>/restore/<id>.
func workDir(dataDir, id string) string { return filepath.Join(RestoreRoot(dataDir), id) }

// RestorePending reports whether a restore is staged or under way: an update
// waits for it, and a second restore is refused.
func RestorePending(dataDir string) bool {
	_, err := os.Lstat(PendingPath(dataDir))
	return err == nil
}

// BackupRef describes the backup a restore puts back.
type BackupRef struct {
	// Name is the file's name; Where is WhereFolder or WhereLocal for a listed
	// backup, "" for a file the CLI named by path.
	Name      string    `json:"name"`
	Where     string    `json:"where,omitempty"`
	Path      string    `json:"path,omitempty"`
	Kind      string    `json:"kind,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	Host      string    `json:"host,omitempty"`
	Version   string    `json:"version,omitempty"`
	Memories  int       `json:"memories"`
	Notes     int       `json:"notes"`
	Sessions  int       `json:"sessions,omitempty"`
}

// SwapEntry is one data dir entry a replacement moves.
type SwapEntry struct {
	Name string `json:"name"`
	// Original is whether the data dir held it before the swap: it goes to
	// previous/, and a rollback brings it back.
	Original bool `json:"original"`
	// Staged is whether new/ held it: it goes into the data dir.
	Staged bool `json:"staged"`
}

// KeptWords is what carrying today's state over changed, beyond copying rows
// (store.KeptReport), for the result.
type KeptWords struct {
	Tightened  []string `json:"tightened,omitempty"`
	Detached   []string `json:"detached,omitempty"`
	Registered []string `json:"registered,omitempty"`
}

// Pending is restore/pending.json: a restore staged and waiting for the start
// that applies it, or under way.
type Pending struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Mode    string `json:"mode"`
	// SHA256 is the backup file's digest, as verified when it was staged.
	SHA256 string    `json:"sha256"`
	Backup BackupRef `json:"backup"`
	// Proof is how the owner approved it: "passkey" (Credential names it), or
	// "account" (the CLI, run as the daemon's account), or "owner" (the
	// console on an install with nothing locked).
	Proof       string    `json:"proof,omitempty"`
	Credential  string    `json:"credential,omitempty"`
	RequestedBy string    `json:"requested_by"`
	RequestedAt time.Time `json:"requested_at"`
	Stage       string    `json:"stage"`
	// Entries are what a replacement's swap moves, written with
	// StageSwapping.
	Entries []SwapEntry `json:"entries,omitempty"`
	// Attempts counts the starts that ran a merge.
	Attempts int `json:"attempts,omitempty"`
	// Safety is a merge's before-restore backup in LocalDir, taken while it
	// was staged.
	Safety string `json:"safety,omitempty"`
	// Kept is what staging found carrying today's state over would change.
	Kept KeptWords `json:"kept,omitzero"`
}

// expired reports whether the restore waited past restoreExpiry at now.
func (p Pending) expired(now time.Time) bool {
	return now.Sub(p.RequestedAt) > restoreExpiry
}

// Result is restore/last.json: what the last restore came to, until the owner
// dismisses it.
type Result struct {
	Version     int       `json:"version"`
	ID          string    `json:"id"`
	Mode        string    `json:"mode"`
	Outcome     string    `json:"outcome"`
	Backup      BackupRef `json:"backup"`
	Proof       string    `json:"proof,omitempty"`
	Credential  string    `json:"credential,omitempty"`
	RequestedBy string    `json:"requested_by,omitempty"`
	RequestedAt time.Time `json:"requested_at,omitzero"`
	At          time.Time `json:"at"`
	// Reason is a fixed word, Message the owner-facing sentence.
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Previous is the before-restore backup holding the data the restore
	// replaced or merged into, in LocalDir, once it is written; PreviousDir is
	// where a replacement's data waits for it (restore/<id>/previous), and
	// PreviousError why it could not be archived yet.
	Previous      string `json:"previous,omitempty"`
	PreviousDir   string `json:"previous_dir,omitempty"`
	PreviousError string `json:"previous_error,omitempty"`
	// Memories and Notes are what a replacement put back (the backup's), or
	// what a merge added of each.
	Memories int `json:"memories,omitempty"`
	Notes    int `json:"notes,omitempty"`
	// Added counts what a merge added: memories, notes and database rows.
	Added int       `json:"added,omitempty"`
	Kept  KeptWords `json:"kept,omitzero"`
	// Recorded is whether its event is in the event log.
	Recorded  bool `json:"recorded,omitempty"`
	Dismissed bool `json:"dismissed,omitempty"`
}

// ReadPending reads restore/pending.json, nil when there is none.
func ReadPending(dataDir string) (*Pending, error) {
	var p Pending
	ok, err := readRecord(PendingPath(dataDir), &p)
	if err != nil || !ok {
		return nil, err
	}
	return &p, nil
}

// ReadResult reads restore/last.json, nil when there is none.
func ReadResult(dataDir string) (*Result, error) {
	var r Result
	ok, err := readRecord(LastPath(dataDir), &r)
	if err != nil || !ok {
		return nil, err
	}
	return &r, nil
}

// readRecord reads a restore record into v, ok false when the file is absent.
func readRecord(path string, v any) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxPendingBytes+1))
	if err != nil {
		return false, err
	}
	if len(data) > maxPendingBytes {
		return false, fmt.Errorf("%s exceeds %d bytes", path, maxPendingBytes)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

// writeRecord replaces path with v's JSON through a synced temp file renamed
// into place.
func writeRecord(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := writeTemp(dir, filepath.Base(path), v)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	syncDir(dir)
	return nil
}

// createPending publishes p as restore/pending.json only if none is there:
// one restore at a time. The record is written whole first and linked into
// place, so a reader never sees it half written; where the file system has no
// hard links it falls back to an exclusive create.
func createPending(dataDir string, p *Pending) error {
	path := PendingPath(dataDir)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := writeTemp(dir, PendingName, p)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	err = os.Link(tmp, path)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrExist):
		return errRestorePending
	default:
		// No hard links here: an exclusive create. A crash part way leaves a
		// record that does not parse, which the start reports and sets aside.
		data, merr := json.MarshalIndent(p, "", "  ")
		if merr != nil {
			return merr
		}
		f, cerr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(cerr, fs.ErrExist) {
			return errRestorePending
		}
		if cerr != nil {
			return cerr
		}
		_, werr := f.Write(append(data, '\n'))
		if werr == nil {
			werr = f.Sync()
		}
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			_ = os.Remove(path)
			return werr
		}
	}
	syncDir(dir)
	return nil
}

// writeTemp writes v's JSON to a synced 0600 temp file in dir.
func writeTemp(dir, base string, v any) (string, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	_, err = tmp.Write(append(data, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// errRestorePending: a restore is already staged.
var errRestorePending = errors.New("another restore is already staged")

// ErrRestorePending is the stage refusal when another restore is staged.
var ErrRestorePending = &Failure{Reason: RestoreReasonPending,
	Message: "another restore is already waiting to finish; cancel it first, or restart Seamless to finish it"}

// exists reports whether path exists (without following a final link).
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
