package gardener

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// Regression: the daemon must be able to drain the gardener goroutine before it
// closes the DB. Start tracks the goroutine and Wait blocks until ctx cancel
// stops it; before this contract existed the loop was fire-and-forget and a
// mid-shutdown pass could race the DB close.
func TestStartWait_DrainsOnCancel(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	g := New(db, nil, nil, nil, nil, Config{}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	g.Start(ctx)
	cancel()

	done := make(chan struct{})
	go func() {
		g.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after ctx cancel")
	}
}

// Wait on a service whose Start never ran must return immediately (the daemon
// calls Wait unconditionally; the gardener may be disabled).
func TestWait_NoStartIsNoOp(t *testing.T) {
	g := New(nil, nil, nil, nil, nil, Config{}, slog.Default())
	done := make(chan struct{})
	go func() {
		g.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait without Start must not block")
	}
}

// gdSchedule is a started service over a fresh store, with a stale memory a
// staleness pass would propose archiving and an active session quiet for two
// hours that the reaper expires. first is the wait before the first tick;
// hook, when set, runs at the start of every proposal pass.
type gdSchedule struct {
	g       *Service
	ctx     context.Context
	session string
}

func gdNewSchedule(t *testing.T, propose bool, first time.Duration, hook func(context.Context)) *gdSchedule {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	now := time.Now().UTC()
	writeMem(t, mgr, "ancient", "", "4", core.KindGotcha, now.Add(-120*24*time.Hour), "long unread")
	sess := mustID(t)
	ctx := context.Background()
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: sess, Name: "cc/quiet", ProjectSlug: "example-ops", Status: core.SessionActive,
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
	}))
	g := New(db, mgr, nil, nil, events.NewRecorder(db),
		Config{Propose: propose, StalenessDays: 90, Interval: time.Hour}, slog.Default())
	g.firstDelay, g.beforePasses = first, hook
	runCtx, cancel := context.WithCancel(ctx)
	g.Start(runCtx)
	t.Cleanup(func() { cancel(); g.Wait() })
	return &gdSchedule{g: g, ctx: ctx, session: sess}
}

func (d *gdSchedule) pending(t *testing.T) []store.Proposal {
	t.Helper()
	ps, err := store.PendingProposals(d.ctx, d.g.db, "")
	require.NoError(t, err)
	return ps
}

// With gardener.enabled false the schedule still does the upkeep: the idle
// session is reaped. No proposal pass runs, on the schedule or when asked.
func TestSchedule_SuggestionsOffStillReaps(t *testing.T) {
	d := gdNewSchedule(t, false, 0, nil)
	require.Eventually(t, func() bool { return !d.g.Status().Housekept.IsZero() }, 10*time.Second, 10*time.Millisecond,
		"the first tick's upkeep runs")
	got, _, err := store.SessionByID(d.ctx, d.g.db, d.session)
	require.NoError(t, err)
	require.Equal(t, core.SessionExpired, got.Status, "the idle session is reaped with suggestions off")
	require.Empty(t, d.pending(t), "no proposal: the stale memory is not proposed")
	st := d.g.Status()
	require.False(t, st.Proposing)
	require.True(t, st.Scheduled)
	require.Nil(t, st.Last, "no pass ran")
	require.False(t, st.Next.IsZero(), "the next tick is known")
	require.ErrorIs(t, d.g.RunNow(WithActor(d.ctx, ByOwner)), ErrProposingOff)
}

// The control for the test above: the same store with suggestions on gets
// both the reap and the proposal, and the pass is recorded as the schedule's.
func TestSchedule_SuggestionsOnReapsAndProposes(t *testing.T) {
	d := gdNewSchedule(t, true, 0, nil)
	require.Eventually(t, func() bool { return d.g.Status().Last != nil }, 10*time.Second, 10*time.Millisecond)
	got, _, err := store.SessionByID(d.ctx, d.g.db, d.session)
	require.NoError(t, err)
	require.Equal(t, core.SessionExpired, got.Status)
	ps := d.pending(t)
	require.Len(t, ps, 1)
	require.Equal(t, "ancient", ps[0].Payload["name"])
	last := d.g.Status().Last
	require.Equal(t, TriggerSchedule, last.Trigger)
	require.Empty(t, last.By)
	require.True(t, last.OK())
	require.Equal(t, 1, last.Result.Archives)
	require.False(t, last.Started.IsZero())
	require.False(t, last.Finished.Before(last.Started))
}

// RunNow queues a proposal pass between ticks and refuses a second while the
// first runs: the overlap refusal. Once the pass finishes it is the last run,
// in the asker's name, its event says so, and a new RunNow is accepted.
func TestRunNow_RefusesWhileAPassRuns(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	hold := true
	// The hook is read by the loop after it receives each RunNow, so the
	// write below that turns it off happens before the read that sees it.
	d := gdNewSchedule(t, true, time.Hour /* no tick during the test */, func(context.Context) {
		if hold {
			entered <- struct{}{}
			<-release
		}
	})

	owner := WithActor(d.ctx, ByOwner)
	require.NoError(t, d.g.RunNow(owner))
	require.ErrorIs(t, d.g.RunNow(owner), ErrPassRunning, "queued counts as running")
	<-entered
	st := d.g.Status()
	require.NotNil(t, st.Running)
	require.Equal(t, TriggerOwner, st.Running.Trigger)
	require.Equal(t, ByOwner, st.Running.By)
	require.False(t, st.Running.Started.IsZero(), "the pass has started")
	require.ErrorIs(t, d.g.RunNow(WithActor(d.ctx, "cc/agent")), ErrPassRunning, "the overlap is refused")
	close(release)

	require.Eventually(t, func() bool { return d.g.Status().Last != nil }, 10*time.Second, 10*time.Millisecond)
	st = d.g.Status()
	require.Nil(t, st.Running)
	require.Equal(t, TriggerOwner, st.Last.Trigger)
	require.Equal(t, ByOwner, st.Last.By)
	require.Equal(t, 1, st.Last.Result.Archives, "the pass did run: the stale memory is proposed")
	require.Len(t, d.pending(t), 1)

	evs, err := d.g.events.ByKinds(d.ctx, []core.EventKind{core.EventGardenerAction}, "", "", 50)
	require.NoError(t, err)
	var runs []map[string]any
	for _, e := range evs {
		if e.Payload["action"] == "run_now" {
			runs = append(runs, e.Payload)
		}
	}
	require.Len(t, runs, 1, "one event for the one run")
	require.Equal(t, "console", runs[0]["by"], "the owner's run is recorded as a console write")
	require.NotContains(t, runs[0], "archives", "no counts: they span every project")

	first := st.Last.Queued
	hold = false
	require.NoError(t, d.g.RunNow(owner), "after the pass, a new one is accepted")
	require.Eventually(t, func() bool {
		st := d.g.Status()
		return st.Running == nil && st.Last != nil && st.Last.Queued.After(first)
	}, 10*time.Second, 10*time.Millisecond)
}

// RunNow refuses on a service whose schedule is not running: never started,
// or stopped. Nothing would run the pass, and nothing is left queued.
func TestRunNow_RefusesWithoutTheSchedule(t *testing.T) {
	g := New(nil, nil, nil, nil, nil, Config{Propose: true}, slog.Default())
	require.ErrorIs(t, g.RunNow(context.Background()), ErrNotScheduled)
	require.False(t, g.Status().Scheduled)

	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	g = New(db, nil, nil, nil, nil, Config{Propose: true}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	g.Start(ctx)
	require.True(t, g.Status().Scheduled)
	cancel()
	g.Wait()
	require.ErrorIs(t, g.RunNow(context.Background()), ErrNotScheduled)
	st := g.Status()
	require.False(t, st.Scheduled)
	require.True(t, st.Next.IsZero())
	require.Nil(t, st.Running)
}
