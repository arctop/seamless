package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/store"
)

// Applier finishes a staged restore at startup: after serve has taken the data
// dir lock, before anything opens the database.
type Applier struct {
	DataDir string
	// ForceLocked are the config's force locks, set only while peer_auth is
	// on.
	ForceLocked []string
	// Validate is serve's peer_auth preflight over a prepared database; nil
	// checks nothing more.
	Validate func(ctx context.Context, db *sql.DB) error
	Now      func() time.Time
	Log      *slog.Logger

	// hook is the test seam for a crash: called before each step that changes
	// the disk, a non-nil error stops Apply there, as a process that died
	// would, with nothing cleaned up.
	hook func(step string) error
}

// Applied is what Apply found and did.
type Applied struct {
	// Pending is the record it found, nil when none was there.
	Pending *Pending
	// Result is what the restore came to, nil when nothing was pending.
	Result *Result
}

// ErrRollbackIncomplete is a swap that could not be rolled back: serve must
// not start on a data dir that holds part of two instances.
var ErrRollbackIncomplete = errors.New("a restore was interrupted and could not be undone")

func (a *Applier) defaults() {
	if a.Now == nil {
		a.Now = time.Now
	}
	if a.Log == nil {
		a.Log = slog.Default()
	}
}

func (a *Applier) step(name string) error {
	if a.hook == nil {
		return nil
	}
	return a.hook(name)
}

// Apply finishes the restore restore/pending.json records, if there is one,
// and writes restore/last.json. It returns an error only when serve must not
// start: a swap that could not be rolled back (ErrRollbackIncomplete, naming
// the folder that holds today's data), or a record it cannot act on safely.
func (a Applier) Apply(ctx context.Context) (Applied, error) {
	a.defaults()
	p, err := ReadPending(a.DataDir)
	if err != nil {
		return a.unreadable(err)
	}
	if p == nil {
		return Applied{}, nil
	}
	out := Applied{Pending: p}
	switch p.Stage {
	case StageApplied:
		res, err := a.finishSwap(p)
		out.Result = res
		return out, err
	case StageSwapping:
		res, err := a.rollback(p, RestoreReasonSwap, "Seamless stopped part way through the restore; it was undone, and nothing changed")
		out.Result = res
		return out, err
	case StageStaged, StageMerging:
	default:
		res, err := a.drop(p, RestoreFailed, RestoreReasonRecord, fmt.Sprintf("the restore record names a stage this Seamless does not know (%q); nothing changed", p.Stage))
		out.Result = res
		return out, err
	}
	if p.Stage == StageStaged && p.expired(a.Now()) {
		res, err := a.drop(p, RestoreExpired, RestoreReasonExpired, fmt.Sprintf(
			"the restore started at %s was not applied because Seamless did not restart within an hour; nothing changed",
			p.RequestedAt.Local().Format("15:04")))
		out.Result = res
		return out, err
	}
	var res *Result
	if p.Mode == ModeMerge {
		res, err = a.merge(ctx, p)
	} else {
		res, err = a.replace(ctx, p)
	}
	out.Result = res
	return out, err
}

// unreadable handles a pending.json that does not parse. Unless a swap may be
// mid-way (some restore/<id>/previous holds data), it is set aside and serve
// starts; otherwise serve refuses, naming the folder.
func (a Applier) unreadable(cause error) (Applied, error) {
	root := RestoreRoot(a.DataDir)
	des, err := os.ReadDir(root)
	if err != nil {
		// Nothing can rule out a swap mid-way.
		return Applied{}, fmt.Errorf("%w: its record (%s) cannot be read (%w), nor %s: %w",
			ErrRollbackIncomplete, PendingPath(a.DataDir), cause, root, err)
	}
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		prev := filepath.Join(root, de.Name(), previousDirName)
		held, err := os.ReadDir(prev)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || len(held) > 0 {
			return Applied{}, fmt.Errorf("%w: its record (%s) cannot be read (%w), and %s may hold data a swap moved: put each entry back into %s by hand, then remove %s",
				ErrRollbackIncomplete, PendingPath(a.DataDir), cause, prev, a.DataDir, PendingPath(a.DataDir))
		}
	}
	moved := PendingPath(a.DataDir) + ".unreadable-" + a.Now().UTC().Format(nameStamp)
	if err := os.Rename(PendingPath(a.DataDir), moved); err != nil {
		return Applied{}, fmt.Errorf("the restore record %s cannot be read (%w) or set aside: %w", PendingPath(a.DataDir), cause, err)
	}
	a.Log.Warn("backup: the restore record could not be read; it was set aside and nothing changed", "moved_to", moved, "err", cause)
	res := &Result{Version: recordVersion, Outcome: RestoreFailed, At: a.Now().UTC(), Reason: RestoreReasonRecord,
		Message: "the restore's record could not be read, so it was not applied; nothing changed"}
	if err := writeRecord(LastPath(a.DataDir), res); err != nil {
		a.Log.Warn("backup: could not record the restore's result", "err", err)
	}
	return Applied{Result: res}, nil
}

// replace applies a staged replacement: today's kept state into new/, checked
// again, then the recorded swap.
func (a Applier) replace(ctx context.Context, p *Pending) (*Result, error) {
	work := workDir(a.DataDir, p.ID)
	newDir := filepath.Join(work, newDirName)
	if !exists(filepath.Join(newDir, archive.DBName)) {
		return a.drop(p, RestoreFailed, RestoreReasonMissing, "the prepared data was missing when Seamless restarted; nothing changed")
	}
	if err := a.step("kept"); err != nil {
		return nil, err
	}
	kept, err := readTodayKept(ctx, a.DataDir)
	if err != nil {
		return a.drop(p, RestoreFailed, RestoreReasonKept, fmt.Sprintf("today's locks and settings could not be read (%v); nothing changed", err))
	}
	words, err := applyKept(ctx, filepath.Join(newDir, archive.DBName), kept, a.ForceLocked, a.Validate, a.Now())
	if err != nil {
		reason := RestoreReasonKept
		var f *Failure
		if errors.As(err, &f) {
			reason = f.Reason
		}
		return a.drop(p, RestoreFailed, reason, fmt.Sprintf("%v; nothing changed", err))
	}
	p.Kept = words

	// The write-ahead record: every entry the swap moves, and where each is
	// now, before the first rename.
	p.Entries = p.Entries[:0]
	for _, name := range swapEntries {
		p.Entries = append(p.Entries, SwapEntry{
			Name: name, Original: exists(filepath.Join(a.DataDir, name)), Staged: exists(filepath.Join(newDir, name)),
		})
	}
	if err := a.step("record-swapping"); err != nil {
		return nil, err
	}
	p.Stage = StageSwapping
	if err := writeRecord(PendingPath(a.DataDir), p); err != nil {
		return a.drop(p, RestoreFailed, RestoreReasonSwap, fmt.Sprintf("the restore could not be recorded (%v); nothing changed", err))
	}

	if err := a.swap(p); err != nil {
		if errors.Is(err, errSimulatedStop) {
			return nil, err
		}
		a.Log.Warn("backup: the restore's swap failed; undoing it", "id", p.ID, "err", err)
		return a.rollback(p, RestoreReasonSwap, fmt.Sprintf("the restore could not swap the data in (%v); it was undone, and nothing changed", err))
	}

	if err := a.step("record-applied"); err != nil {
		return nil, err
	}
	p.Stage = StageApplied
	if err := writeRecord(PendingPath(a.DataDir), p); err != nil {
		// The swap is complete on disk; without the record a later start
		// would roll it back. Undo it now instead, while the entries are known.
		return a.rollback(p, RestoreReasonSwap, fmt.Sprintf("the restore could not record its completion (%v); it was undone, and nothing changed", err))
	}
	return a.finishSwap(p)
}

// errSimulatedStop is what the test hook returns to stand in for a process
// that died.
var errSimulatedStop = errors.New("backup: simulated stop")

// swap moves each original entry into previous/, then each staged one into
// the data dir.
func (a Applier) swap(p *Pending) error {
	work := workDir(a.DataDir, p.ID)
	prev := filepath.Join(work, previousDirName)
	newDir := filepath.Join(work, newDirName)
	if err := os.MkdirAll(prev, 0o700); err != nil {
		return err
	}
	for _, e := range p.Entries {
		if !e.Original {
			continue
		}
		if err := a.step("move-out:" + e.Name); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(a.DataDir, e.Name), filepath.Join(prev, e.Name)); err != nil {
			return fmt.Errorf("set aside %s: %w", e.Name, err)
		}
	}
	syncDir(prev)
	for _, e := range p.Entries {
		if !e.Staged {
			continue
		}
		if err := a.step("move-in:" + e.Name); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(newDir, e.Name), filepath.Join(a.DataDir, e.Name)); err != nil {
			return fmt.Errorf("move in %s: %w", e.Name, err)
		}
	}
	syncDir(a.DataDir)
	return nil
}

// rollback undoes a swap entry by entry, from wherever it stopped: a staged
// entry already in the data dir goes back to new/, and an original in
// previous/ comes back. It is idempotent, so a start that stops part way
// through it runs it again. new/ is discarded only once every original is
// verified in place.
func (a Applier) rollback(p *Pending, reason, message string) (*Result, error) {
	work := workDir(a.DataDir, p.ID)
	prev := filepath.Join(work, previousDirName)
	newDir := filepath.Join(work, newDirName)
	for i := len(p.Entries) - 1; i >= 0; i-- {
		e := p.Entries[i]
		live := filepath.Join(a.DataDir, e.Name)
		if e.Staged && !exists(filepath.Join(newDir, e.Name)) && exists(live) {
			if err := a.step("rollback-out:" + e.Name); err != nil {
				return nil, err
			}
			if err := os.Rename(live, filepath.Join(newDir, e.Name)); err != nil {
				return nil, a.stuck(prev, fmt.Errorf("move the restored %s back out: %w", e.Name, err))
			}
		}
		if e.Original && exists(filepath.Join(prev, e.Name)) {
			if err := a.step("rollback-in:" + e.Name); err != nil {
				return nil, err
			}
			if exists(live) {
				return nil, a.stuck(prev, fmt.Errorf("%s is in the way of today's %s", live, e.Name))
			}
			if err := os.Rename(filepath.Join(prev, e.Name), live); err != nil {
				return nil, a.stuck(prev, fmt.Errorf("put today's %s back: %w", e.Name, err))
			}
		}
	}
	syncDir(a.DataDir)
	for _, e := range p.Entries {
		if e.Original && (!exists(filepath.Join(a.DataDir, e.Name)) || exists(filepath.Join(prev, e.Name))) {
			return nil, a.stuck(prev, fmt.Errorf("today's %s is not back in place", e.Name))
		}
	}
	return a.drop(p, RestoreFailed, reason, message)
}

// stuck is a rollback that could not complete.
func (a Applier) stuck(prev string, cause error) error {
	return fmt.Errorf("%w (%w): today's data is in %s and %s, recorded in %s; move each entry of %s back into %s, then remove %s",
		ErrRollbackIncomplete, cause, a.DataDir, prev, PendingPath(a.DataDir), prev, a.DataDir, PendingPath(a.DataDir))
}

// finishSwap is the bookkeeping after the commit point: the result, the
// pending record gone, and what is no longer needed of the work dir. It never
// rolls back.
func (a Applier) finishSwap(p *Pending) (*Result, error) {
	work := workDir(a.DataDir, p.ID)
	res := a.result(p, RestoreDone, "", "")
	res.Memories, res.Notes = p.Backup.Memories, p.Backup.Notes
	prev := filepath.Join(work, previousDirName)
	if exists(prev) {
		res.PreviousDir = prev
	}
	if old, err := ReadResult(a.DataDir); err == nil && old != nil && old.ID == p.ID {
		// A start that stopped after writing it: keep what it said.
		res = old
	}
	if err := a.step("result"); err != nil {
		return nil, err
	}
	if err := writeRecord(LastPath(a.DataDir), res); err != nil {
		return res, fmt.Errorf("the restore is applied, but its result could not be recorded: %w", err)
	}
	if err := os.Remove(PendingPath(a.DataDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("the restore is applied, but %s could not be removed: %w", PendingPath(a.DataDir), err)
	}
	syncDir(RestoreRoot(a.DataDir))
	// The copy and new/'s leftovers are in the data dir now, or the folder
	// the backup came from still holds them; previous/ waits for its archive.
	_ = os.Remove(filepath.Join(work, copyName))
	_ = os.RemoveAll(filepath.Join(work, newDirName))
	a.Log.Info("backup: restored a backup", "id", p.ID, "backup", p.Backup.Name, "previous", res.PreviousDir)
	return res, nil
}

// merge folds the verified copy into today's data. A merge is idempotent by
// id, so a start that stopped part way runs it again, at most
// maxMergeAttempts times.
func (a Applier) merge(ctx context.Context, p *Pending) (*Result, error) {
	work := workDir(a.DataDir, p.ID)
	src := filepath.Join(work, copyName)
	if !exists(src) {
		return a.drop(p, RestoreFailed, RestoreReasonMissing, "the prepared backup was missing when Seamless restarted; nothing changed")
	}
	if p.Attempts >= maxMergeAttempts {
		return a.drop(p, RestorePartial, RestoreReasonMerge, fmt.Sprintf(
			"the merge stopped part way %d times, so it was abandoned: what it added stays, and nothing was removed", p.Attempts))
	}
	p.Attempts++
	p.Stage = StageMerging
	if err := writeRecord(PendingPath(a.DataDir), p); err != nil {
		return a.drop(p, RestoreFailed, RestoreReasonMerge, fmt.Sprintf("the merge could not be recorded (%v); nothing changed", err))
	}
	if err := a.step("merge"); err != nil {
		return nil, err
	}
	rep, err := mergeInto(ctx, src, a.DataDir, work, a.ForceLocked, a.Log)
	if err != nil {
		if rep != nil && added(rep) > 0 {
			res, derr := a.drop(p, RestorePartial, RestoreReasonMerge, fmt.Sprintf(
				"the merge stopped part way (%v): %d items were added before it stopped; nothing was removed", err, added(rep)))
			if res != nil {
				res.Added = added(rep)
				if err := writeRecord(LastPath(a.DataDir), res); err != nil {
					a.Log.Warn("backup: could not record what the merge added", "err", err)
				}
			}
			return res, derr
		}
		return a.drop(p, RestoreFailed, RestoreReasonMerge, fmt.Sprintf("the merge did not go ahead (%v); nothing changed", err))
	}
	if err := clearHashes(ctx, a.DataDir); err != nil {
		a.Log.Warn("backup: merged items without vectors will not be embedded until a re-embed", "err", err)
	}
	if err := a.step("result"); err != nil {
		return nil, err
	}
	res := a.result(p, RestoreDone, "", "")
	res.Added, res.Memories, res.Notes = added(rep), rep.Memories, rep.Notes
	res.Previous = p.Safety
	if err := writeRecord(LastPath(a.DataDir), res); err != nil {
		return res, fmt.Errorf("the merge is done, but its result could not be recorded: %w", err)
	}
	if err := os.Remove(PendingPath(a.DataDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("the merge is done, but %s could not be removed: %w", PendingPath(a.DataDir), err)
	}
	_ = os.RemoveAll(work)
	a.Log.Info("backup: merged a backup", "id", p.ID, "backup", p.Backup.Name, "added", res.Added)
	return res, nil
}

// mergeInto runs a merge import of src into dataDir, staging under tmp.
func mergeInto(ctx context.Context, src, dataDir, tmp string, forceLocked []string, log *slog.Logger) (*archive.ImportReport, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return archive.Import(ctx, archive.ImportOptions{
		DataDir: dataDir, Src: f, Logger: log, ForceLocked: forceLocked, TmpDir: tmp,
	})
}

// added counts what an import wrote.
func added(rep *archive.ImportReport) int {
	n := rep.Memories + rep.Notes
	for _, c := range rep.Rows {
		n += c
	}
	return n
}

// clearHashes blanks, in the data dir's database, the hashes of what has no
// vector, so the daemon's startup reconcile embeds it.
func clearHashes(ctx context.Context, dataDir string) error {
	db, err := store.Open(filepath.Join(dataDir, archive.DBName))
	if err != nil {
		return err
	}
	_, err = store.ClearHashesWithoutVectors(ctx, db)
	return errors.Join(err, db.Close())
}

// readTodayKept reads the kept state from the data dir's database.
func readTodayKept(ctx context.Context, dataDir string) (store.KeptState, error) {
	db, err := store.Open(filepath.Join(dataDir, archive.DBName))
	if err != nil {
		return store.KeptState{}, err
	}
	kept, err := store.ReadKeptState(ctx, db)
	return kept, errors.Join(err, db.Close())
}

// result is the Result for p.
func (a Applier) result(p *Pending, outcome, reason, message string) *Result {
	return &Result{
		Version: recordVersion, ID: p.ID, Mode: p.Mode, Outcome: outcome, Backup: p.Backup,
		Proof: p.Proof, Credential: p.Credential, RequestedBy: p.RequestedBy, RequestedAt: p.RequestedAt,
		At: a.Now().UTC(), Reason: reason, Message: message, Kept: p.Kept,
	}
}

// drop ends a restore that did not apply: the result recorded, the pending
// record removed, and the work dir with it (it holds only the copy and the
// prepared data, whose source is still where it came from). A merge's safety
// backup stays, as any backup does.
func (a Applier) drop(p *Pending, outcome, reason, message string) (*Result, error) {
	res := a.result(p, outcome, reason, message)
	if p.Mode == ModeMerge && p.Safety != "" {
		res.Previous = p.Safety
	}
	if err := writeRecord(LastPath(a.DataDir), res); err != nil {
		a.Log.Warn("backup: could not record the restore's result", "err", err)
	}
	if err := os.Remove(PendingPath(a.DataDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, fmt.Errorf("the restore was not applied, but %s could not be removed: %w", PendingPath(a.DataDir), err)
	}
	work := workDir(a.DataDir, p.ID)
	// Never a work dir holding replaced data (a rollback that left some).
	if held, err := os.ReadDir(filepath.Join(work, previousDirName)); errors.Is(err, fs.ErrNotExist) || (err == nil && len(held) == 0) {
		_ = os.RemoveAll(work)
	}
	a.Log.Warn("backup: a restore was not applied", "id", p.ID, "outcome", outcome, "reason", reason, "detail", message)
	return res, nil
}

// DropPending ends a staged restore unapplied, recording reason: serve calls it
// when the data dir lock is unavailable, since no swap is safe without it.
func DropPending(dataDir, reason, message string, now time.Time) (*Result, error) {
	p, err := ReadPending(dataDir)
	if err != nil || p == nil {
		return nil, err
	}
	if p.Stage == StageSwapping || p.Stage == StageApplied {
		return nil, fmt.Errorf("a restore is mid-way (%s) and needs the data dir lock to finish: %s", p.Stage, message)
	}
	a := Applier{DataDir: dataDir, Now: func() time.Time { return now }}
	a.defaults()
	return a.drop(p, RestoreFailed, reason, message)
}

// Archiver turns a replacement's previous/ into a before-restore backup in
// LocalDir once the daemon serves, and records it in last.json: the undo.
type Archiver struct {
	DataDir     string
	Version     string
	Host        string
	InstallID   string
	ForceLocked []string
	PasskeyRPID string
	Now         func() time.Time
	Log         *slog.Logger

	export func(ctx context.Context, opts archive.ExportOptions) (archive.Manifest, error)
}

// NeedsArchive reports whether r's replaced data still waits for its backup.
func (r *Result) NeedsArchive() bool {
	return r != nil && r.Outcome == RestoreDone && r.Mode == ModeReplace && r.PreviousDir != "" && r.Previous == ""
}

// ArchivePrevious archives the replaced data last.json names, reads the backup
// back, and only then removes previous/. It returns the updated result; on
// failure previous/ stays, and the result says why.
func (ar Archiver) ArchivePrevious(ctx context.Context) (*Result, error) {
	if ar.Now == nil {
		ar.Now = time.Now
	}
	if ar.Log == nil {
		ar.Log = slog.Default()
	}
	if ar.export == nil {
		ar.export = archive.Export
	}
	res, err := ReadResult(ar.DataDir)
	if err != nil || !res.NeedsArchive() {
		return res, err
	}
	prev := res.PreviousDir
	if !insideRestore(ar.DataDir, prev) || !exists(prev) {
		res.PreviousError = "the replaced data was not where the restore left it"
		return res, writeRecord(LastPath(ar.DataDir), res)
	}
	name, err := writeLocalBackup(ctx, localBackupSpec{
		export: ar.export, dataDir: ar.DataDir, from: prev, version: ar.Version, host: ar.Host,
		installID: ar.InstallID, forceLocked: ar.ForceLocked, passkeyRPID: ar.PasskeyRPID,
		at: ar.Now(), id: res.ID,
	})
	if err != nil {
		res.PreviousError = err.Error()
		ar.Log.Warn("backup: the data a restore replaced could not be backed up; it stays where it is", "dir", prev, "err", err)
		return res, errors.Join(err, writeRecord(LastPath(ar.DataDir), res))
	}
	res.Previous, res.PreviousError = name, ""
	if err := writeRecord(LastPath(ar.DataDir), res); err != nil {
		return res, err
	}
	res.PreviousDir = ""
	if err := os.RemoveAll(filepath.Dir(prev)); err != nil {
		ar.Log.Warn("backup: could not remove the replaced data after backing it up", "dir", prev, "err", err)
		res.PreviousDir = prev
	}
	if err := writeRecord(LastPath(ar.DataDir), res); err != nil {
		return res, err
	}
	ar.Log.Info("backup: the data a restore replaced is saved as a backup", "name", name)
	return res, nil
}

// insideRestore reports whether dir is restore/<id>/previous in dataDir.
func insideRestore(dataDir, dir string) bool {
	rel, err := filepath.Rel(RestoreRoot(dataDir), dir)
	if err != nil {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	return len(parts) == 2 && parts[0] != ".." && parts[0] != "." && parts[1] == previousDirName
}
