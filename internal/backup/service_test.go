package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

func autoNames(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, n := range files(t, dir) {
		if strings.HasSuffix(n, "-auto.tar.gz") {
			out = append(out, n)
		}
	}
	return out
}

func TestScheduler_FirstRunWaitsTenMinutesThenDaily(t *testing.T) {
	dataDir, db := seedDataDir(t)
	h := newHarness(t, dataDir, db)

	st := h.svc.Status()
	require.True(t, st.Settings.Auto)
	require.Equal(t, t0.Add(firstRunDelay), st.NextAt)
	require.Equal(t, t0, st.Since)

	h.clock.Add(9 * time.Minute)
	st = h.tick()
	require.Nil(t, st.Running, "never in the daemon's first ten minutes, however overdue")
	require.NoDirExists(t, h.folder)

	h.clock.Add(time.Minute)
	st = h.tick()
	require.NotNil(t, st.Running)
	require.True(t, st.Running.Scheduled)
	st = h.idle()
	require.Equal(t, OutcomeCreated, st.LastRun.Outcome, st.LastRun.Error)
	first := t0.Add(firstRunDelay)
	require.Equal(t, first, st.LastAttempt)
	require.Equal(t, first, st.LastConfirmed)
	require.Equal(t, first.Add(24*time.Hour), st.NextAt)
	require.Equal(t, []string{"seamless-test-host-20261111T031000Z-auto.tar.gz"}, files(t, h.folder))
	require.NotNil(t, st.LastBackup)
	require.Equal(t, archive.KindScheduled, st.LastBackup.Kind)
	require.Equal(t, 1, st.LastBackup.Projects)

	created := h.rec.of(core.EventBackupCreated)
	require.Len(t, created, 1)
	require.Equal(t, "seamless-test-host-20261111T031000Z-auto.tar.gz", created[0].ItemID)
	require.Equal(t, archive.KindScheduled, created[0].Payload["kind"])

	// Not due again until a day after the attempt.
	h.clock.Add(23 * time.Hour)
	require.Nil(t, h.tick().Running)
}

// A restore that changed the data ends "Up to date: nothing has changed since
// the last backup" (the last run's skip), and that survives a restart. One
// that changed nothing leaves it.
func TestScheduler_ARestoreThatChangedTheDataEndsUpToDate(t *testing.T) {
	for _, tc := range []struct {
		outcome  string
		upToDate bool
	}{
		{RestoreDone, false}, {RestorePartial, false}, {RestoreFailed, true}, {RestoreExpired, true},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			dataDir, db := seedDataDir(t)
			h := newHarness(t, dataDir, db)
			require.Equal(t, OutcomeCreated, h.runScheduled(firstRunDelay).LastRun.Outcome)
			require.True(t, h.runScheduled(24*time.Hour).UpToDate)

			// What a startup finds once a restore has been applied (or not).
			require.NoError(t, writeRecord(LastPath(dataDir), &Result{
				Version: 1, ID: "01RESTORE", Mode: ModeReplace, Outcome: tc.outcome, At: h.clock.Now(),
			}))
			h.restart()
			require.Equal(t, tc.upToDate, h.sync().UpToDate)
			h.restart()
			require.Equal(t, tc.upToDate, h.sync().UpToDate, "the record keeps it across a restart")
		})
	}
}

func TestScheduler_SkipsWhenUnchangedAndRunsWhenChanged(t *testing.T) {
	dataDir, db := seedDataDir(t)
	h := newHarness(t, dataDir, db)
	require.Equal(t, OutcomeCreated, h.runScheduled(firstRunDelay).LastRun.Outcome)

	st := h.runScheduled(24 * time.Hour)
	require.Equal(t, OutcomeUnchanged, st.LastRun.Outcome)
	require.True(t, st.UpToDate)
	require.Equal(t, h.clock.Now(), st.LastConfirmed, "a skip confirms the backups current")
	require.Equal(t, h.clock.Now(), st.LastAttempt)
	require.Len(t, autoNames(t, h.folder), 1, "nothing new is written")
	require.Len(t, h.rec.of(core.EventBackupCreated), 1)

	addTask(t, db, "01TASKNEW")
	st = h.runScheduled(24 * time.Hour)
	require.Equal(t, OutcomeCreated, st.LastRun.Outcome)
	require.False(t, st.UpToDate)
	require.Len(t, autoNames(t, h.folder), 2)

	// Deleting the newest backup means the next run has nothing to match.
	l, err := h.svc.List(context.Background())
	require.NoError(t, err)
	require.NoError(t, h.svc.Delete(context.Background(), l.Entries[0].Ref, "owner"))
	st = h.runScheduled(24 * time.Hour)
	require.Equal(t, OutcomeCreated, st.LastRun.Outcome, "the owner deleted the backup that matched")
}

func TestScheduler_AnEmptyInstanceIsSkipped(t *testing.T) {
	dataDir, db := emptyDataDir(t)
	h := newHarness(t, dataDir, db)
	st := h.runScheduled(firstRunDelay)
	require.Equal(t, OutcomeEmpty, st.LastRun.Outcome)
	require.Equal(t, h.clock.Now(), st.LastConfirmed)
	require.NoDirExists(t, h.folder)
}

func TestScheduler_OffWritesNothing(t *testing.T) {
	dataDir, db := seedDataDir(t)
	h := newHarness(t, dataDir, db, func(d *Deps) {
		d.Base.Auto = nil
		d.Release = false
	})
	st := h.runScheduled(48 * time.Hour)
	require.False(t, st.Settings.Auto, "a source build does not back up unless configured")
	require.Nil(t, st.LastRun)
	require.True(t, st.NextAt.IsZero())
	require.False(t, st.Alert(h.clock.Now()))
	require.NoDirExists(t, h.folder)
	require.NoDirExists(t, LocalDir(dataDir), "not even the schedule's record")
}

func TestScheduler_FailureStreakSurvivesARestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mount points")
	}
	dataDir, db := seedDataDir(t)
	disk := "/mnt/seamless-test-absent-disk-7f3a"
	h := newHarness(t, dataDir, db, func(d *Deps) {
		d.GOOS = "linux"
		d.Base.Dir = disk + "/Seamless Backups"
	})

	st := h.runScheduled(firstRunDelay)
	require.Equal(t, OutcomeFailed, st.LastRun.Outcome)
	require.Equal(t, 1, st.Failures)
	firstFail := h.clock.Now()
	require.Equal(t, firstFail, st.FailingSince)
	require.Equal(t, "folder-disconnected", st.LastReason)
	require.Equal(t, disk+" is not connected", st.LastError)
	require.False(t, st.Alert(h.clock.Now()), "one failure is not yet an alert")

	failedEvents := h.rec.of(core.EventBackupFailed)
	require.Len(t, failedEvents, 1)
	require.Equal(t, "folder-disconnected", failedEvents[0].Payload["reason"])
	require.NotContains(t, failedEvents[0].Payload, "error", "an event carries a fixed word, never error text")

	h.restart()
	st = h.svc.Status()
	require.Equal(t, 1, st.Failures, "the streak is read back at start")
	require.Equal(t, firstFail, st.FailingSince)

	st = h.runScheduled(24*time.Hour + firstRunDelay)
	require.Equal(t, 2, st.Failures)
	require.Equal(t, firstFail, st.FailingSince)
	require.True(t, st.Alert(h.clock.Now()))
	require.Equal(t, firstFail, st.AlertSince())
	failedEvents = h.rec.of(core.EventBackupFailed)
	require.Len(t, failedEvents, 2)
	require.Equal(t, 2, failedEvents[1].Payload["failures"])

	// The disk comes back (here: the folder moves to one that exists); the
	// next success ends the streak.
	h.deps.GOOS = runtime.GOOS
	h.deps.Base.Dir = h.folder
	h.restart()
	st = h.runScheduled(24*time.Hour + firstRunDelay)
	require.Equal(t, OutcomeCreated, st.LastRun.Outcome, st.LastRun.Error)
	require.Zero(t, st.Failures)
	require.True(t, st.FailingSince.IsZero())
	require.Empty(t, st.LastError)
	require.False(t, st.Alert(h.clock.Now()))
}

func TestScheduler_OverdueAlert(t *testing.T) {
	st := Status{Settings: Settings{Auto: true, Every: 24 * time.Hour}, Since: t0}
	require.False(t, st.Alert(t0.Add(72*time.Hour)))
	require.True(t, st.Alert(t0.Add(72*time.Hour+time.Second)), "three intervals with no confirmation")
	require.Equal(t, t0, st.AlertSince())
	st.LastConfirmed = t0.Add(48 * time.Hour)
	require.False(t, st.Alert(t0.Add(72*time.Hour+time.Second)))
	require.Equal(t, t0.Add(48*time.Hour), st.AlertSince())
	st.Settings.Auto = false
	require.False(t, st.Alert(t0.Add(1000*time.Hour)), "no alert while off")
}

func TestScheduler_NoSpace(t *testing.T) {
	dataDir, db := seedDataDir(t)
	h := newHarness(t, dataDir, db, func(d *Deps) {
		d.FreeBytes = func(string) (uint64, error) { return 900 << 20, nil }
	})
	st := h.runScheduled(firstRunDelay)
	require.Equal(t, OutcomeFailed, st.LastRun.Outcome)
	require.Equal(t, ReasonNoSpace, st.LastReason)
	require.Contains(t, st.LastError, "not enough free space")
	require.Empty(t, autoNames(t, h.folder), "the reserve is never spent")
}

func TestScheduler_AFolderThatHeldBackupsAndVanishedIsNotRecreated(t *testing.T) {
	dataDir, db := seedDataDir(t)
	h := newHarness(t, dataDir, db)
	require.Equal(t, OutcomeCreated, h.runScheduled(firstRunDelay).LastRun.Outcome)

	require.NoError(t, os.RemoveAll(h.folder))
	addTask(t, db, "01TASKNEW")
	st := h.runScheduled(24 * time.Hour)
	require.Equal(t, OutcomeFailed, st.LastRun.Outcome)
	require.Equal(t, "folder-missing", st.LastReason)
	require.NoDirExists(t, h.folder)

	// The data folder's own backups folder is always made again.
	h.deps.Base.Dir = ""
	h.restart()
	require.NoError(t, os.RemoveAll(LocalDir(dataDir)))
	st = h.runScheduled(24*time.Hour + firstRunDelay)
	require.Equal(t, OutcomeCreated, st.LastRun.Outcome, st.LastRun.Error)
	require.Len(t, autoNames(t, LocalDir(dataDir)), 1)
}

func TestScheduler_ClockThatWentBack(t *testing.T) {
	dataDir, db := seedDataDir(t)
	h := newHarness(t, dataDir, db)
	require.Equal(t, OutcomeCreated, h.runScheduled(firstRunDelay).LastRun.Outcome)
	addTask(t, db, "01TASKNEW")

	h.clock.Add(-30 * 24 * time.Hour)
	st := h.tick()
	require.Equal(t, h.clock.Now(), st.LastAttempt, "a future attempt is pulled back to now")
	st = h.runScheduled(24 * time.Hour)
	require.Equal(t, OutcomeCreated, st.LastRun.Outcome, "a day later it runs, not a month later")
}

func TestBackUpNow_ManualIsNeverSkippedOrPruned(t *testing.T) {
	dataDir, db := seedDataDir(t)
	h := newHarness(t, dataDir, db, func(d *Deps) { d.Base.Keep = 1 })
	ctx := context.Background()
	for range 2 {
		_, err := h.svc.BackUpNow(ctx)
		require.NoError(t, err)
		st := h.idle()
		require.Equal(t, OutcomeCreated, st.LastRun.Outcome, st.LastRun.Error)
		require.False(t, st.LastRun.Scheduled)
		require.True(t, st.LastAttempt.IsZero(), "the owner's backup does not move the schedule")
	}
	// Same clock second: the second name moves on a second.
	require.Equal(t, []string{
		"seamless-test-host-20261111T030000Z.tar.gz",
		"seamless-test-host-20261111T030001Z.tar.gz",
	}, files(t, h.folder))
	require.Equal(t, OutcomeUnchanged, h.runScheduled(firstRunDelay).LastRun.Outcome,
		"a scheduled run matches the owner's newest backup")
	require.Len(t, files(t, h.folder), 2)
}

// blockingExport stands in for archive.Export: it signals that it started,
// then writes through Out until the run is stopped (respect) or until
// released, ignoring ctx (a write stalled in the kernel).
type blockingExport struct {
	started chan struct{}
	release chan struct{}
	respect bool
	once    sync.Once
}

func (b *blockingExport) export(ctx context.Context, opts archive.ExportOptions) (archive.Manifest, error) {
	b.once.Do(func() { close(b.started) })
	if !b.respect {
		<-b.release
		return archive.Manifest{}, errors.New("released")
	}
	buf := make([]byte, 1024)
	for {
		if _, err := opts.Out.Write(buf); err != nil {
			return archive.Manifest{}, err
		}
		select {
		case <-b.release:
			return archive.Manifest{}, errors.New("released")
		default:
		}
	}
}

func TestBackUpNow_BusyWhileRunning(t *testing.T) {
	dataDir, db := seedDataDir(t)
	b := &blockingExport{started: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, dataDir, db, func(d *Deps) { d.export = b.export })
	_, err := h.svc.BackUpNow(context.Background())
	require.NoError(t, err)
	<-b.started
	st, err := h.svc.BackUpNow(context.Background())
	require.ErrorIs(t, err, ErrBusy)
	require.NotNil(t, st.Running)
	_, err = h.svc.RetryNow(context.Background())
	require.ErrorIs(t, err, ErrBusy)
	close(b.release)
	st = h.idle()
	require.Equal(t, OutcomeFailed, st.LastRun.Outcome)
	require.Equal(t, ReasonExport, st.LastRun.Reason)
	require.Empty(t, files(t, h.folder), "a failed run leaves no temp file")
}

func TestWait_AbandonsAStalledWriteWithinItsBound(t *testing.T) {
	dataDir, db := seedDataDir(t)
	b := &blockingExport{started: make(chan struct{}), release: make(chan struct{})}
	h := newHarness(t, dataDir, db, func(d *Deps) {
		d.export = b.export
		d.WaitBound = 50 * time.Millisecond
	})
	_, err := h.svc.BackUpNow(context.Background())
	require.NoError(t, err)
	<-b.started

	h.cancel()
	require.False(t, h.svc.Wait(), "the stalled run is abandoned, not waited for")

	close(b.release)
	h.svc.jobs.Wait()
	partials := files(t, h.folder)
	require.Empty(t, partials, "once it does return, the run removes its temp file")
}

func TestWait_AStoppingDaemonCancelsARun(t *testing.T) {
	dataDir, db := seedDataDir(t)
	b := &blockingExport{started: make(chan struct{}), release: make(chan struct{}), respect: true}
	h := newHarness(t, dataDir, db, func(d *Deps) { d.export = b.export })
	h.clock.Add(firstRunDelay)
	h.tick()
	<-b.started
	h.cancel()
	require.True(t, h.svc.Wait(), "the run stops at its next write")
	require.Empty(t, files(t, h.folder))
	st, _, err := loadState(dataDir, h.clock.Now())
	require.NoError(t, err)
	require.Zero(t, st.Failures, "a stop is not a failure")
	require.True(t, st.LastAttempt.IsZero(), "and the next start runs it again")
	require.Empty(t, h.rec.of(core.EventBackupFailed))
}

func TestStart_SweepsOnlyItsOwnLeftovers(t *testing.T) {
	dataDir, db := seedDataDir(t)
	local := LocalDir(dataDir)
	require.NoError(t, os.MkdirAll(filepath.Join(local, ".staging-01OLD"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(local, ".export-01UPDATER"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(local, ".pre-update-1.partial"), nil, 0o600))

	h := newHarness(t, dataDir, db)
	id, err := store.InstallID(context.Background(), db)
	require.NoError(t, err)
	require.NoDirExists(t, filepath.Join(local, ".staging-01OLD"))
	require.DirExists(t, filepath.Join(local, ".export-01UPDATER"), "the updater's staging is not ours")
	require.FileExists(t, filepath.Join(local, ".pre-update-1.partial"))

	require.NoError(t, os.MkdirAll(h.folder, 0o700))
	mine := filepath.Join(h.folder, partialName(id, "01OLDJOB"))
	theirs := filepath.Join(h.folder, partialName("01ANOTHERINSTALLAAAAAAAAAA", "01JOB"))
	require.NoError(t, os.WriteFile(mine, nil, 0o600))
	require.NoError(t, os.WriteFile(theirs, nil, 0o600))
	require.Equal(t, OutcomeCreated, h.runScheduled(firstRunDelay).LastRun.Outcome)
	require.NoFileExists(t, mine, "a run sweeps this install's abandoned temp files")
	require.FileExists(t, theirs, "another install's may still be writing")
}

func TestPrune_OnlyThisInstallsAutomaticBackups(t *testing.T) {
	ctx := context.Background()
	shared := filepath.Join(t.TempDir(), "Shared Backups")

	dirA, dbA := seedDataDir(t)
	a := newHarness(t, dirA, dbA, func(d *Deps) { d.Base.Keep = 2; d.Base.Dir = shared })
	dirB, dbB := seedDataDir(t)
	b := newHarness(t, dirB, dbB, func(d *Deps) { d.Base.Keep = 7; d.Base.Dir = shared })

	// The other install, with the same hostname, writes two automatic
	// backups; this one makes one by hand.
	require.Equal(t, OutcomeCreated, b.runScheduled(firstRunDelay).LastRun.Outcome)
	addTask(t, dbB, "01TASKB2")
	require.Equal(t, OutcomeCreated, b.runScheduled(24*time.Hour).LastRun.Outcome)
	_, err := a.svc.BackUpNow(ctx)
	require.NoError(t, err)
	require.Equal(t, OutcomeCreated, a.idle().LastRun.Outcome)

	// Three automatic backups here, keeping two.
	var firstAuto string
	for i, step := range []time.Duration{firstRunDelay, 24 * time.Hour, 24 * time.Hour} {
		addTask(t, dbA, "01TASKA"+string(rune('0'+i)))
		st := a.runScheduled(step)
		require.Equal(t, OutcomeCreated, st.LastRun.Outcome, st.LastRun.Error)
		if i == 0 {
			firstAuto = st.LastRun.Name
		}
	}
	require.Equal(t, "seamless-test-host-20261111T031001Z-auto.tar.gz", firstAuto,
		"the other install, same hostname, took the second first: this name moved on one")

	l, err := a.svc.List(ctx)
	require.NoError(t, err)
	var mineAuto, theirsAuto, manual int
	for _, e := range l.Entries {
		if e.Where != WhereFolder {
			continue
		}
		switch {
		case e.Kind == archive.KindManual:
			manual++
		case e.Ours:
			mineAuto++
		case e.FromAnother:
			theirsAuto++
		}
	}
	require.Equal(t, 2, mineAuto, "this install keeps its newest two")
	require.Equal(t, 2, theirsAuto, "the other install's are not ours to prune")
	require.Equal(t, 1, manual, "a backup made by hand is never pruned")

	deleted := a.rec.of(core.EventBackupDeleted)
	require.Len(t, deleted, 1)
	require.Equal(t, "prune", deleted[0].Payload["by"])
	require.Equal(t, firstAuto, deleted[0].ItemID, "the oldest goes")
}

func TestPrune_APlaceholderTheServiceWroteIsStillItsOwn(t *testing.T) {
	// A dataless file cannot be faked portably, so the rule is checked on
	// describe: an unread placeholder is ours only when the record says so.
	e := Entry{Named: true, Dataless: true, Name: "seamless-h-20261101T000000Z-auto.tar.gz", Path: "/f/seamless-h-20261101T000000Z-auto.tar.gz"}
	e.Parsed, _ = ParseName(e.Name)
	st := state{Written: []written{{Name: e.Name, Dir: "/f", Fingerprint: "ffff"}}}
	describe(&e, "01ME", st.wrote)
	require.True(t, e.Ours)
	require.Equal(t, archive.KindScheduled, e.Kind)
	require.Equal(t, "ffff", st.writtenFingerprint("/f", e.Name))

	other := Entry{Named: true, Dataless: true, Name: "seamless-h-20261102T000000Z-auto.tar.gz", Path: "/f/seamless-h-20261102T000000Z-auto.tar.gz"}
	other.Parsed, _ = ParseName(other.Name)
	describe(&other, "01ME", st.wrote)
	require.False(t, other.Ours)
}

func TestDelete(t *testing.T) {
	dataDir, db := seedDataDir(t)
	h := newHarness(t, dataDir, db)
	ctx := context.Background()
	require.Equal(t, OutcomeCreated, h.runScheduled(firstRunDelay).LastRun.Outcome)
	name := h.svc.Status().LastBackup.Name

	require.ErrorIs(t, h.svc.Delete(ctx, "folder:"+filepath.Join("..", name), "owner"), ErrNoSuchBackup)
	require.ErrorIs(t, h.svc.Delete(ctx, "folder:seamless-x-20200101T000000Z.tar.gz", "owner"), ErrNoSuchBackup)
	require.ErrorIs(t, h.svc.Delete(ctx, filepath.Join(h.folder, name), "owner"), ErrNoSuchBackup, "never a raw path")

	require.NoError(t, h.svc.Delete(ctx, "folder:"+name, "owner"))
	require.NoFileExists(t, filepath.Join(h.folder, name))
	ev := h.rec.of(core.EventBackupDeleted)
	require.Len(t, ev, 1)
	require.Equal(t, name, ev[0].ItemID)
	require.Equal(t, "owner", ev[0].Payload["by"])
	st, _, err := loadState(dataDir, h.clock.Now())
	require.NoError(t, err)
	require.False(t, st.wrote(h.folder, name), "the record forgets it")
}

func TestPoll_AStagedRestoreAsksForARestart(t *testing.T) {
	dataDir, db := seedDataDir(t)
	pending := PendingPath(dataDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(pending), 0o700))
	require.NoError(t, os.WriteFile(pending, []byte(`{"stage":"staged"}`), 0o600))

	h := newHarness(t, dataDir, db)
	h.poll()
	require.Zero(t, h.restarts.Load(), "one there at start was the startup step's")

	require.NoError(t, os.Remove(pending))
	h.poll()
	require.NoError(t, os.WriteFile(pending, []byte(`{"stage":`), 0o600))
	h.poll()
	require.Zero(t, h.restarts.Load(), "not until it is complete")
	require.NoError(t, os.WriteFile(pending, []byte(`{"stage":"staged","id":"01X"}`), 0o600))
	h.poll()
	require.EqualValues(t, 1, h.restarts.Load())
	h.poll()
	require.EqualValues(t, 1, h.restarts.Load(), "once per file")

	require.NoError(t, os.WriteFile(pending, []byte(`{"stage":"swapping","id":"01X","entries":[]}`), 0o600))
	h.poll()
	require.EqualValues(t, 1, h.restarts.Load(), "only a staged restore asks")
}

func TestService_OverrideErrorFallsBackToTheFile(t *testing.T) {
	dataDir, db := seedDataDir(t)
	h := newHarness(t, dataDir, db, func(d *Deps) {
		d.Override = func(context.Context) (config.BackupOverride, error) {
			return config.BackupOverride{}, errors.New("bad row")
		}
	})
	st := h.sync()
	require.Equal(t, "bad row", st.SettingsError)
	require.True(t, st.Settings.Auto)
	require.Equal(t, SourceConfig, st.Settings.DirSource)
}

func TestService_CorruptRecordIsMovedAside(t *testing.T) {
	dataDir, db := seedDataDir(t)
	require.NoError(t, os.MkdirAll(LocalDir(dataDir), 0o700))
	require.NoError(t, os.WriteFile(statePath(dataDir), []byte("{not json"), 0o600))
	h := newHarness(t, dataDir, db)
	require.Zero(t, h.svc.Status().Failures)
	var aside []string
	for _, n := range files(t, LocalDir(dataDir)) {
		if strings.HasPrefix(n, stateName+".corrupt-") {
			aside = append(aside, n)
		}
	}
	require.Len(t, aside, 1)
}

func TestService_NotStarted(t *testing.T) {
	s := New(Deps{DataDir: t.TempDir()})
	_, err := s.BackUpNow(context.Background())
	require.ErrorIs(t, err, ErrNotRunning)
	require.True(t, s.Wait())
}

func TestService_SuggestionsUnderTheGuardAreOnlyUsableOnes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("peer_auth runs on macOS and Linux")
	}
	dataDir, db := seedDataDir(t)
	home := t.TempDir()
	mkdirs(t, home, "Dropbox")
	s := New(Deps{DataDir: dataDir, DB: db, GOOS: "linux", Home: home, User: "nobody-here",
		Guard: &Guard{EUID: 260, Inspect: func(p string) (FileFacts, error) {
			if _, err := os.Lstat(p); err != nil {
				return FileFacts{}, err
			}
			if strings.HasPrefix(p, home) {
				return FileFacts{UID: 501, Mode: 0o755 | os.ModeDir, IsDir: true}, nil
			}
			return FileFacts{UID: 260, Mode: 0o700 | os.ModeDir, IsDir: true}, nil
		}}})
	got := s.Suggestions()
	require.Len(t, got, 1, "a person's Dropbox is no place for the daemon's backups")
	require.Equal(t, PlaceLocal, got[0].Kind)
}
