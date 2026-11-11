package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/core"
	fileslayer "github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// exportTo writes a backup of dataDir into dir and returns its path.
func exportTo(t *testing.T, dataDir, dir, name string, noDB bool) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	require.NoError(t, err)
	_, err = archive.Export(context.Background(), archive.ExportOptions{
		DataDir: dataDir, Out: f, Host: "test-host", Kind: archive.KindManual, NoDB: noDB, TmpDir: t.TempDir(),
	})
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return path
}

// corpus maps every file under dataDir's memory/ and notes/ to its digest.
func corpus(t *testing.T, dataDir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, tree := range []string{archive.MemoryTree, archive.NotesTree} {
		root := filepath.Join(dataDir, tree)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil || d.IsDir() {
				return err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dataDir, p)
			sum := sha256.Sum256(data)
			out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
			return nil
		})
		require.NoError(t, err)
	}
	return out
}

func writeMemory(t *testing.T, dataDir, project, name, id, body string) {
	t.Helper()
	p := filepath.Join(dataDir, archive.MemoryTree, project, name+".md")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	require.NoError(t, os.WriteFile(p, []byte("---\nid: "+id+"\nkind: gotcha\nname: "+name+"\ndescription: "+body+
		"\nproject: "+project+"\ncreated: 2026-11-10T03:00:00Z\nupdated: 2026-11-10T03:00:00Z\nvalid_from: 2026-11-10T03:00:00Z\n---\n"+body+"\n"), 0o600))
}

func writeNote(t *testing.T, dataDir, project, slug, id, body string) {
	t.Helper()
	p := filepath.Join(dataDir, archive.NotesTree, project, slug+".md")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	require.NoError(t, os.WriteFile(p, []byte("---\nid: "+id+"\ntitle: "+slug+"\nslug: "+slug+"\nproject: "+project+
		"\ncreated: 2026-11-10T03:00:00Z\nupdated: 2026-11-10T03:00:00Z\n---\n"+body+"\n"), 0o600))
}

// reconcile indexes the data dir's corpus into db, as the daemon does.
func reconcile(t *testing.T, dataDir string, db *sql.DB) {
	t.Helper()
	mgr, err := fileslayer.NewManager(dataDir, db, quiet)
	require.NoError(t, err)
	require.NoError(t, mgr.Reconcile(context.Background()))
	require.NoError(t, mgr.Close())
}

// taskIDs lists the tasks in the data dir's database.
func taskIDs(t *testing.T, dataDir string) []string {
	t.Helper()
	db, err := store.OpenExisting(filepath.Join(dataDir, archive.DBName))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT id FROM tasks ORDER BY id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	return out
}

// lockAlpha fences alpha and locks it from the console, today.
func lockAlpha(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	_, err := store.TightenProjectIsolation(ctx, db, "alpha", core.IsolationConfidential, time.Now())
	require.NoError(t, err)
	require.NoError(t, store.AddConsoleLock(ctx, db, "alpha", "cred-today", time.Now()))
}

// scene is a data dir with a backup taken, then changed since: the backup
// holds task 01TASKSEED and memory boot-race; today adds 01TASKTODAY and
// memory today-only, and locks alpha.
type scene struct {
	dataDir string
	db      *sql.DB
	backup  string
	before  map[string]string // the backup's corpus
	today   map[string]string // today's corpus
}

func newScene(t *testing.T) *scene {
	t.Helper()
	dataDir, db := seedDataDir(t)
	sc := &scene{dataDir: dataDir, db: db}
	writeMemory(t, dataDir, "alpha", "boot-race", "01MEMSEED", "chroma races on boot")
	writeNote(t, dataDir, "alpha", "design", "01NOTESEED", "the design")
	reconcile(t, dataDir, db)
	sc.before = corpus(t, dataDir)
	sc.backup = exportTo(t, dataDir, filepath.Join(t.TempDir(), "folder"), "seamless-test-host-20261110T030000Z.tar.gz", false)
	addTask(t, db, "01TASKTODAY")
	writeMemory(t, dataDir, "alpha", "today-only", "01MEMTODAY", "written after the backup")
	writeNote(t, dataDir, "alpha", "today-note", "01NOTETODAY", "a note from today")
	reconcile(t, dataDir, db)
	lockAlpha(t, db)
	sc.today = corpus(t, dataDir)
	require.NotEqual(t, sc.before, sc.today)
	return sc
}

func (sc *scene) stager() Stager {
	return Stager{DataDir: sc.dataDir, DB: sc.db, Version: "0.8.1", Host: "test-host", Log: quiet,
		FreeBytes: func(string) (uint64, error) { return 1 << 40, nil }}
}

func (sc *scene) stage(t *testing.T, mode string) *Pending {
	t.Helper()
	p, err := sc.stager().Stage(context.Background(), StageRequest{
		Path: sc.backup, Backup: BackupRef{Name: filepath.Base(sc.backup), Where: WhereFolder}, Mode: mode,
		RequestedBy: "test", Proof: "owner",
	})
	require.NoError(t, err)
	return p
}

// restart closes the live handle, as the daemon's restart does.
func (sc *scene) restart(t *testing.T) {
	t.Helper()
	require.NoError(t, sc.db.Close())
}

func applier(dataDir string) Applier { return Applier{DataDir: dataDir, Log: quiet} }

func TestRestore_ReplacePutsTheBackupBackAndKeepsTodaysLock(t *testing.T) {
	sc := newScene(t)
	p := sc.stage(t, ModeReplace)
	require.Equal(t, StageStaged, p.Stage)
	require.NotEmpty(t, p.SHA256)
	require.Equal(t, 1, p.Backup.Memories)
	require.FileExists(t, PendingPath(sc.dataDir))
	require.Equal(t, sc.today, corpus(t, sc.dataDir), "staging changes nothing in the data dir")
	require.Empty(t, p.Kept.Tightened, "the fresh import already fenced the project locked today")

	sc.restart(t)
	out, err := applier(sc.dataDir).Apply(context.Background())
	require.NoError(t, err)
	require.Equal(t, RestoreDone, out.Result.Outcome)
	require.Equal(t, sc.before, corpus(t, sc.dataDir), "the corpus is the backup's, byte for byte")
	require.Equal(t, []string{"01TASKSEED"}, taskIDs(t, sc.dataDir))
	require.NoFileExists(t, PendingPath(sc.dataDir))

	db, err := store.OpenExisting(filepath.Join(sc.dataDir, archive.DBName))
	require.NoError(t, err)
	locks, err := store.ConsoleLockedSlugs(context.Background(), db)
	require.NoError(t, err)
	require.Equal(t, []string{"alpha"}, locks, "today's lock survives a restore from before it")
	iso, err := store.IsolationOf(context.Background(), db, "alpha")
	require.NoError(t, err)
	require.Equal(t, core.IsolationConfidential, iso)
	require.NoError(t, db.Close())

	res, err := ReadResult(sc.dataDir)
	require.NoError(t, err)
	require.True(t, res.NeedsArchive())
	require.Equal(t, sc.today, corpus(t, res.PreviousDir), "today's data waits in previous/")

	// After serving: the replaced data becomes a before-restore backup.
	ar := Archiver{DataDir: sc.dataDir, Version: "0.8.1", Host: "test-host", InstallID: "01INSTALL", Log: quiet}
	res, err = ar.ArchivePrevious(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, res.Previous)
	require.Empty(t, res.PreviousDir)
	require.NoDirExists(t, workDir(sc.dataDir, p.ID))
	safety := filepath.Join(LocalDir(sc.dataDir), res.Previous)
	m, err := readManifestFile(safety)
	require.NoError(t, err)
	require.Equal(t, archive.KindBeforeRestore, m.Kind)

	// Undo: restoring the safety backup brings today's data back, byte for byte.
	db, err = store.Open(filepath.Join(sc.dataDir, archive.DBName))
	require.NoError(t, err)
	st := Stager{DataDir: sc.dataDir, DB: db, Log: quiet}
	_, err = st.Stage(context.Background(), StageRequest{Path: safety, Mode: ModeReplace})
	require.NoError(t, err)
	require.NoError(t, db.Close())
	out, err = applier(sc.dataDir).Apply(context.Background())
	require.NoError(t, err)
	require.Equal(t, RestoreDone, out.Result.Outcome)
	require.Equal(t, sc.today, corpus(t, sc.dataDir))
	require.Equal(t, []string{"01TASKSEED", "01TASKTODAY"}, taskIDs(t, sc.dataDir))
}

// The steps a replacement's apply takes, in order, as the hook hears them.
func applySteps(t *testing.T) []string {
	t.Helper()
	sc := newScene(t)
	sc.stage(t, ModeReplace)
	sc.restart(t)
	var steps []string
	a := applier(sc.dataDir)
	a.hook = func(step string) error { steps = append(steps, step); return nil }
	_, err := a.Apply(context.Background())
	require.NoError(t, err)
	return steps
}

func TestRestore_AStopAtEveryStepOfTheSwapLeavesADataDirThatRecovers(t *testing.T) {
	steps := applySteps(t)
	require.Contains(t, steps, "move-out:memory")
	require.Contains(t, steps, "move-in:notes")
	for _, stop := range steps {
		t.Run(stop, func(t *testing.T) {
			sc := newScene(t)
			sc.stage(t, ModeReplace)
			sc.restart(t)
			a := applier(sc.dataDir)
			a.hook = func(step string) error {
				if step == stop {
					return errSimulatedStop
				}
				return nil
			}
			_, err := a.Apply(context.Background())
			require.ErrorIs(t, err, errSimulatedStop)
			p, err := ReadPending(sc.dataDir)
			require.NoError(t, err)
			require.NotNil(t, p, "the record outlives the stop")
			committed := p.Stage == StageApplied

			// The next start.
			out, err := applier(sc.dataDir).Apply(context.Background())
			require.NoError(t, err)
			require.NoFileExists(t, PendingPath(sc.dataDir))
			if committed {
				require.Equal(t, RestoreDone, out.Result.Outcome, "an applied swap is never rolled back")
				require.Equal(t, sc.before, corpus(t, sc.dataDir))
				require.Equal(t, []string{"01TASKSEED"}, taskIDs(t, sc.dataDir))
				return
			}
			if p.Stage == StageSwapping {
				require.Equal(t, RestoreFailed, out.Result.Outcome)
				require.Equal(t, RestoreReasonSwap, out.Result.Reason)
				require.Equal(t, sc.today, corpus(t, sc.dataDir), "rolled back: today's corpus, byte for byte")
				require.Equal(t, []string{"01TASKSEED", "01TASKTODAY"}, taskIDs(t, sc.dataDir))
				require.NoDirExists(t, workDir(sc.dataDir, p.ID))
				return
			}
			// Stopped before the swap began: the next start applies it.
			require.Equal(t, RestoreDone, out.Result.Outcome)
			require.Equal(t, sc.before, corpus(t, sc.dataDir))
		})
	}
}

func TestRestore_AStopDuringTheRollbackIsRolledBackAgain(t *testing.T) {
	for _, stop := range []string{"rollback-out:memory", "rollback-in:notes", "rollback-in:seam.db"} {
		t.Run(stop, func(t *testing.T) {
			sc := newScene(t)
			sc.stage(t, ModeReplace)
			sc.restart(t)
			a := applier(sc.dataDir)
			a.hook = func(step string) error {
				switch step {
				case "move-in:notes":
					return errors.New("disk said no") // a real failure: the swap rolls back
				case stop:
					return errSimulatedStop
				}
				return nil
			}
			_, err := a.Apply(context.Background())
			require.ErrorIs(t, err, errSimulatedStop)

			out, err := applier(sc.dataDir).Apply(context.Background())
			require.NoError(t, err)
			require.Equal(t, RestoreFailed, out.Result.Outcome)
			require.Equal(t, sc.today, corpus(t, sc.dataDir))
			require.Equal(t, []string{"01TASKSEED", "01TASKTODAY"}, taskIDs(t, sc.dataDir))
		})
	}
}

func TestRestore_ASwapThatFailsRollsBackAtOnce(t *testing.T) {
	sc := newScene(t)
	sc.stage(t, ModeReplace)
	sc.restart(t)
	a := applier(sc.dataDir)
	a.hook = func(step string) error {
		if step == "move-in:memory" {
			return errors.New("disk said no")
		}
		return nil
	}
	out, err := a.Apply(context.Background())
	require.NoError(t, err)
	require.Equal(t, RestoreFailed, out.Result.Outcome)
	require.Contains(t, out.Result.Message, "nothing changed")
	require.Equal(t, sc.today, corpus(t, sc.dataDir))
}

func TestRestore_ARollbackThatCannotCompleteRefusesToServe(t *testing.T) {
	sc := newScene(t)
	p := sc.stage(t, ModeReplace)
	sc.restart(t)
	a := applier(sc.dataDir)
	a.hook = func(step string) error {
		if step == "move-in:notes" {
			return errSimulatedStop
		}
		return nil
	}
	_, err := a.Apply(context.Background())
	require.ErrorIs(t, err, errSimulatedStop)
	// Something lands where today's notes must go back.
	prev := filepath.Join(workDir(sc.dataDir, p.ID), previousDirName)
	require.NoError(t, os.WriteFile(filepath.Join(sc.dataDir, "notes"), []byte("in the way"), 0o600))

	_, err = applier(sc.dataDir).Apply(context.Background())
	require.ErrorIs(t, err, ErrRollbackIncomplete)
	require.ErrorContains(t, err, prev)
	require.FileExists(t, PendingPath(sc.dataDir), "the record stays for the owner and doctor")
	require.DirExists(t, filepath.Join(prev, "notes"), "nothing of today's is deleted")
}

func TestRestore_AnExpiredRestoreIsDroppedUnapplied(t *testing.T) {
	sc := newScene(t)
	p := sc.stage(t, ModeReplace)
	sc.restart(t)
	a := applier(sc.dataDir)
	a.Now = func() time.Time { return p.RequestedAt.Add(restoreExpiry + time.Minute) }
	out, err := a.Apply(context.Background())
	require.NoError(t, err)
	require.Equal(t, RestoreExpired, out.Result.Outcome)
	require.Contains(t, out.Result.Message, "within an hour")
	require.Equal(t, sc.today, corpus(t, sc.dataDir))
	require.NoFileExists(t, PendingPath(sc.dataDir))
	require.NoDirExists(t, workDir(sc.dataDir, p.ID))
}

func TestRestore_AValidationFailureAtStartChangesNothing(t *testing.T) {
	sc := newScene(t)
	sc.stage(t, ModeReplace)
	sc.restart(t)
	a := applier(sc.dataDir)
	a.Validate = func(context.Context, *sql.DB) error { return errors.New("a locked project is not fenced") }
	out, err := a.Apply(context.Background())
	require.NoError(t, err)
	require.Equal(t, RestoreFailed, out.Result.Outcome)
	require.Equal(t, RestoreReasonValidate, out.Result.Reason)
	require.Equal(t, sc.today, corpus(t, sc.dataDir))
	require.Equal(t, []string{"01TASKSEED", "01TASKTODAY"}, taskIDs(t, sc.dataDir))
}

func TestRestore_MissingStagedDataIsReportedAndChangesNothing(t *testing.T) {
	sc := newScene(t)
	p := sc.stage(t, ModeReplace)
	sc.restart(t)
	require.NoError(t, os.RemoveAll(filepath.Join(workDir(sc.dataDir, p.ID), newDirName)))
	out, err := applier(sc.dataDir).Apply(context.Background())
	require.NoError(t, err)
	require.Equal(t, RestoreReasonMissing, out.Result.Reason)
	require.Equal(t, sc.today, corpus(t, sc.dataDir))
}

func TestRestore_AnUnreadableRecordIsSetAside(t *testing.T) {
	dataDir, db := seedDataDir(t)
	require.NoError(t, db.Close())
	require.NoError(t, os.MkdirAll(RestoreRoot(dataDir), 0o700))
	require.NoError(t, os.WriteFile(PendingPath(dataDir), []byte(`{"stage":`), 0o600))
	out, err := applier(dataDir).Apply(context.Background())
	require.NoError(t, err)
	require.Equal(t, RestoreReasonRecord, out.Result.Reason)
	require.NoFileExists(t, PendingPath(dataDir))
}

func TestStage_Refusals(t *testing.T) {
	sc := newScene(t)
	st := sc.stager()
	ctx := context.Background()

	t.Run("a knowledge-only backup cannot replace everything", func(t *testing.T) {
		noDB := exportTo(t, sc.dataDir, t.TempDir(), "knowledge.tar.gz", true)
		_, err := st.Stage(ctx, StageRequest{Path: noDB, Mode: ModeReplace})
		requireReason(t, err, RestoreReasonNoDatabase)
	})
	t.Run("a backup that changed since it was approved", func(t *testing.T) {
		_, err := st.Stage(ctx, StageRequest{Path: sc.backup, Mode: ModeReplace, ExpectSHA256: strings.Repeat("0", 64)})
		requireReason(t, err, RestoreReasonChanged)
	})
	t.Run("a damaged backup", func(t *testing.T) {
		data, err := os.ReadFile(sc.backup)
		require.NoError(t, err)
		bad := filepath.Join(t.TempDir(), "cut.tar.gz")
		require.NoError(t, os.WriteFile(bad, data[:len(data)/2], 0o600))
		_, err = st.Stage(ctx, StageRequest{Path: bad, Mode: ModeReplace})
		requireReason(t, err, RestoreReasonUnreadable)
	})
	t.Run("no room", func(t *testing.T) {
		tight := st
		tight.FreeBytes = func(string) (uint64, error) { return 1 << 20, nil }
		_, err := tight.Stage(ctx, StageRequest{Path: sc.backup, Mode: ModeReplace})
		requireReason(t, err, RestoreReasonNoSpace)
	})
	t.Run("a prepared database that fails the checks", func(t *testing.T) {
		strict := st
		strict.Validate = func(context.Context, *sql.DB) error { return errors.New("no") }
		_, err := strict.Stage(ctx, StageRequest{Path: sc.backup, Mode: ModeReplace})
		requireReason(t, err, RestoreReasonValidate)
	})
	t.Run("one restore at a time", func(t *testing.T) {
		_, err := st.Stage(ctx, StageRequest{Path: sc.backup, Mode: ModeReplace})
		require.NoError(t, err)
		_, err = st.Stage(ctx, StageRequest{Path: sc.backup, Mode: ModeMerge})
		requireReason(t, err, RestoreReasonPending)
	})

	// Every refusal before the last left nothing behind.
	des, err := os.ReadDir(RestoreRoot(sc.dataDir))
	require.NoError(t, err)
	var dirs []string
	for _, de := range des {
		if de.IsDir() {
			dirs = append(dirs, de.Name())
		}
	}
	require.Len(t, dirs, 1, "only the staged restore's work dir")
	require.Equal(t, sc.today, corpus(t, sc.dataDir))
}

func requireReason(t *testing.T, err error, reason string) {
	t.Helper()
	var f *Failure
	require.True(t, errors.As(err, &f), "want a *Failure, got %v", err)
	require.Equal(t, reason, f.Reason, f.Message)
}

func TestRestore_MergeBringsBackADeletedMemoryAndKeepsTodays(t *testing.T) {
	sc := newScene(t)
	gone := filepath.Join(sc.dataDir, archive.MemoryTree, "alpha", "boot-race.md")
	require.NoError(t, os.Remove(gone))
	reconcile(t, sc.dataDir, sc.db)
	p := sc.stage(t, ModeMerge)
	require.NotEmpty(t, p.Safety)
	safety := filepath.Join(LocalDir(sc.dataDir), p.Safety)
	m, err := readManifestFile(safety)
	require.NoError(t, err)
	require.Equal(t, archive.KindBeforeRestore, m.Kind)

	sc.restart(t)
	out, err := applier(sc.dataDir).Apply(context.Background())
	require.NoError(t, err)
	require.Equal(t, RestoreDone, out.Result.Outcome, out.Result.Message)
	require.Positive(t, out.Result.Added)
	require.Equal(t, p.Safety, out.Result.Previous)
	require.FileExists(t, gone, "the deleted memory is back")
	require.FileExists(t, filepath.Join(sc.dataDir, archive.MemoryTree, "alpha", "today-only.md"), "today's stays")
	require.Equal(t, []string{"01TASKSEED", "01TASKTODAY"}, taskIDs(t, sc.dataDir))
	require.NoDirExists(t, workDir(sc.dataDir, p.ID))
	require.FileExists(t, safety, "the safety backup stays")
}

func TestRestore_AMergeThatKeepsStoppingIsAbandoned(t *testing.T) {
	sc := newScene(t)
	sc.stage(t, ModeMerge)
	sc.restart(t)
	stopAtMerge := applier(sc.dataDir)
	stopAtMerge.hook = func(step string) error {
		if step == "merge" {
			return errSimulatedStop
		}
		return nil
	}
	for i := 1; i <= maxMergeAttempts; i++ {
		_, err := stopAtMerge.Apply(context.Background())
		require.ErrorIs(t, err, errSimulatedStop)
		p, err := ReadPending(sc.dataDir)
		require.NoError(t, err)
		require.Equal(t, i, p.Attempts)
		require.Equal(t, StageMerging, p.Stage)
	}
	out, err := applier(sc.dataDir).Apply(context.Background())
	require.NoError(t, err)
	require.Equal(t, RestorePartial, out.Result.Outcome)
	require.NoFileExists(t, PendingPath(sc.dataDir))
}

func TestDropPending_EndsAStagedRestoreUnapplied(t *testing.T) {
	sc := newScene(t)
	sc.stage(t, ModeReplace)
	res, err := DropPending(sc.dataDir, RestoreReasonUnsupported, "no lock here", time.Now())
	require.NoError(t, err)
	require.Equal(t, RestoreFailed, res.Outcome)
	require.False(t, RestorePending(sc.dataDir))
}

func TestService_StageRestoreStagesAndRestartsOnce(t *testing.T) {
	dataDir, db := seedDataDir(t)
	h := newHarness(t, dataDir, db)
	_, err := h.svc.BackUpNow(context.Background())
	require.NoError(t, err)
	h.idle()
	l, err := h.svc.List(context.Background())
	require.NoError(t, err)
	require.Len(t, l.Entries, 1)
	ref := l.Entries[0].Ref

	_, sum, err := h.svc.Digest(context.Background(), ref)
	require.NoError(t, err)
	_, err = h.svc.StageRestore(context.Background(), RestoreRequest{Ref: ref, Mode: ModeReplace, ExpectSHA256: sum, By: "console", Proof: "owner"})
	require.NoError(t, err)
	st := restoreIdle(t, h)
	require.NotNil(t, st.Restore.Pending, "%+v", st.Restore.Failed)
	require.Equal(t, sum, st.Restore.Pending.SHA256)
	require.EqualValues(t, 1, h.restarts.Load())
	h.poll()
	require.EqualValues(t, 1, h.restarts.Load(), "the poll does not ask again")

	_, err = h.svc.BackUpNow(context.Background())
	require.NoError(t, err, "a backup the owner asks for still runs while a restore waits")
	h.idle()

	_, err = h.svc.StageRestore(context.Background(), RestoreRequest{Ref: ref, Mode: ModeMerge})
	requireReason(t, err, RestoreReasonPending)

	st, err = h.svc.CancelRestore(context.Background())
	require.NoError(t, err)
	require.Nil(t, st.Restore.Pending)
	require.False(t, RestorePending(dataDir))
	des, err := os.ReadDir(RestoreRoot(dataDir))
	require.NoError(t, err)
	require.Empty(t, des)
}

func TestService_StageRestoreRefusals(t *testing.T) {
	dataDir, db := seedDataDir(t)
	busy := true
	h := newHarness(t, dataDir, db, func(d *Deps) { d.UpdateBusy = func() bool { return busy } })
	_, err := h.svc.BackUpNow(context.Background())
	require.NoError(t, err)
	h.idle()
	l, err := h.svc.List(context.Background())
	require.NoError(t, err)
	ref := l.Entries[0].Ref

	_, err = h.svc.StageRestore(context.Background(), RestoreRequest{Ref: ref, Mode: ModeReplace})
	requireReason(t, err, RestoreReasonBusy)
	busy = false

	_, err = h.svc.StageRestore(context.Background(), RestoreRequest{Ref: "folder:nope.tar.gz", Mode: ModeReplace})
	require.ErrorIs(t, err, ErrNoSuchBackup)
	_, err = h.svc.StageRestore(context.Background(), RestoreRequest{Ref: "folder:../x.tar.gz", Mode: ModeReplace})
	require.ErrorIs(t, err, ErrNoSuchBackup)

	// A staging that fails shows why, and records it.
	_, err = h.svc.StageRestore(context.Background(), RestoreRequest{Ref: ref, Mode: ModeReplace, ExpectSHA256: strings.Repeat("0", 64)})
	require.NoError(t, err)
	st := restoreIdle(t, h)
	require.NotNil(t, st.Restore.Failed)
	require.Equal(t, RestoreReasonChanged, st.Restore.Failed.Reason)
	require.Len(t, h.rec.of(core.EventBackupRestoreFailed), 1)
	require.Zero(t, h.restarts.Load())
	st, err = h.svc.DismissResult(context.Background())
	require.NoError(t, err)
	require.Nil(t, st.Restore.Failed)
}

func TestService_StartRecordsTheResultOnceAndSavesTheReplacedData(t *testing.T) {
	sc := newScene(t)
	sc.stage(t, ModeReplace)
	sc.restart(t)
	_, err := applier(sc.dataDir).Apply(context.Background())
	require.NoError(t, err)

	db, err := store.Open(filepath.Join(sc.dataDir, archive.DBName))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	h := newHarness(t, sc.dataDir, db)
	require.Eventually(t, func() bool { return !h.svc.Status().Restore.Archiving }, 30*time.Second, 2*time.Millisecond)
	st := h.sync()
	require.NotNil(t, st.Restore.Last)
	require.NotEmpty(t, st.Restore.Last.Previous, st.Restore.Last.PreviousError)
	require.Len(t, h.rec.of(core.EventBackupRestored), 1)
	require.Len(t, h.rec.of(core.EventBackupCreated), 1, "the safety backup")

	l, err := h.svc.List(context.Background())
	require.NoError(t, err)
	var kinds []string
	for _, e := range l.Entries {
		kinds = append(kinds, e.Kind)
	}
	require.True(t, slices.Contains(kinds, archive.KindBeforeRestore), "%v", kinds)

	h.restart()
	h.sync()
	require.Len(t, h.rec.of(core.EventBackupRestored), 1, "recorded once")

	st, err = h.svc.DismissResult(context.Background())
	require.NoError(t, err)
	require.Nil(t, st.Restore.Last)
	h.restart()
	require.Nil(t, h.sync().Restore.Last, "a dismissed result stays dismissed")
}

func TestReadFolderRecord_TheCLIFindsTheFolderInForce(t *testing.T) {
	dataDir, db := seedDataDir(t)
	h := newHarness(t, dataDir, db)
	h.runScheduled(firstRunDelay)
	folder, id := ReadFolderRecord(dataDir)
	require.Equal(t, h.folder, folder)
	require.NotEmpty(t, id)
	l := ListBackups(dataDir, folder, id)
	require.Len(t, l.Entries, 1)
	require.True(t, l.Entries[0].Ours)
	anon := ListBackups(dataDir, folder, "")
	require.False(t, anon.Entries[0].Ours)
	require.False(t, anon.Entries[0].FromAnother, "no id: neither ours nor another's")
}

// restoreIdle waits for the restore job to end.
func restoreIdle(t *testing.T, h *harness) Status {
	t.Helper()
	var st Status
	require.Eventually(t, func() bool {
		st = h.sync()
		return st.Restore.Staging == nil && !st.Restore.Archiving
	}, 30*time.Second, 2*time.Millisecond)
	return st
}
