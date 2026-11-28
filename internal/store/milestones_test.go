package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// fakeOnceRecorder mimics events.Recorder.RecordOnce's kind+item_id latch
// in memory, capturing what was minted for verbatim payload assertions.
type fakeOnceRecorder struct {
	minted []core.Event
	seen   map[string]struct{}
}

func newFakeOnceRecorder() *fakeOnceRecorder {
	return &fakeOnceRecorder{seen: map[string]struct{}{}}
}

func (f *fakeOnceRecorder) RecordOnce(_ context.Context, e core.Event) (string, bool, error) {
	if e.Kind == "" || e.ItemID == "" {
		return "", false, fmt.Errorf("fake RecordOnce: empty kind or item id")
	}
	key := string(e.Kind) + "\x00" + e.ItemID
	if _, dup := f.seen[key]; dup {
		return "", false, nil
	}
	f.seen[key] = struct{}{}
	f.minted = append(f.minted, e)
	id, err := core.NewID()
	if err != nil {
		return "", false, err
	}
	return id, true, nil
}

// seedPlanTask inserts a task (optionally a plan step) and returns its id.
func seedPlanTask(t *testing.T, db *sql.DB, project, plan string, status core.TaskStatus) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, CreateTask(context.Background(), db, core.Task{
		ID: id, ProjectSlug: project, Title: "step", Status: status,
		PlanSlug: plan, CreatedAt: now, UpdatedAt: now,
	}))
	return id
}

func TestCheckMilestones_MemoryWrittenCrossingMintsOnce(t *testing.T) {
	db := openTestDB(t)
	rec := newFakeOnceRecorder()
	ctx := context.Background()
	ts := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	for i := 1; i <= 110; i++ {
		insertProjectEvent(t, db, core.EventMemoryWritten, "sess-1", "demo", "", "{}", ts)
		e := core.Event{Kind: core.EventMemoryWritten, SessionID: "sess-1", ProjectSlug: "demo", TS: ts}
		require.NoError(t, CheckMilestones(ctx, db, rec, e))
		if i < 100 {
			require.Emptyf(t, rec.minted, "no mint before the 100th write (write %d)", i)
		}
	}
	require.Len(t, rec.minted, 1, "the crossing mints exactly once; re-checks do not re-mint")

	m := rec.minted[0]
	require.Equal(t, EventMilestoneReached, m.Kind)
	require.Equal(t, "memories-100:demo", m.ItemID)
	require.Equal(t, "demo", m.ProjectSlug)
	require.Equal(t, "sess-1", m.SessionID)
	require.Equal(t, "100 memories written in demo", m.Payload["claim"])
	require.Equal(t, 100, m.Payload["count"])
	require.Equal(t, 100, m.Payload["threshold"])

	// A projectless event moves no per-project count.
	require.NoError(t, CheckMilestones(ctx, db, rec,
		core.Event{Kind: core.EventMemoryWritten, TS: ts}))
	require.Len(t, rec.minted, 1)
}

// A count first checked far past several thresholds -- writes recorded before
// milestones were -- mints only the highest it reached, with the real count,
// and the next threshold still mints on the write that reaches it.
func TestCheckMilestones_CatchUpMintsOnlyTheHighestThreshold(t *testing.T) {
	db := openTestDB(t)
	rec := newFakeOnceRecorder()
	ctx := context.Background()
	ts := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	for range 501 {
		insertProjectEvent(t, db, core.EventMemoryWritten, "", "demo", "", "{}", ts)
	}
	e := core.Event{Kind: core.EventMemoryWritten, ProjectSlug: "demo", TS: ts}
	require.NoError(t, CheckMilestones(ctx, db, rec, e))
	require.Len(t, rec.minted, 1, "one row for the catch-up, not one per threshold passed")
	require.Equal(t, "memories-500:demo", rec.minted[0].ItemID)
	require.Equal(t, "500 memories written in demo", rec.minted[0].Payload["claim"])
	require.Equal(t, 501, rec.minted[0].Payload["count"])
	require.Equal(t, 500, rec.minted[0].Payload["threshold"])

	// Further writes below the next threshold mint nothing more.
	insertProjectEvent(t, db, core.EventMemoryWritten, "", "demo", "", "{}", ts)
	require.NoError(t, CheckMilestones(ctx, db, rec, e))
	require.Len(t, rec.minted, 1)

	// The write that reaches 1000 mints it, on that write.
	for range 497 {
		insertProjectEvent(t, db, core.EventMemoryWritten, "", "demo", "", "{}", ts)
	}
	require.NoError(t, CheckMilestones(ctx, db, rec, e))
	require.Len(t, rec.minted, 1, "999 writes is not the next milestone")
	insertProjectEvent(t, db, core.EventMemoryWritten, "", "demo", "", "{}", ts)
	require.NoError(t, CheckMilestones(ctx, db, rec, e))
	require.Len(t, rec.minted, 2)
	require.Equal(t, "memories-1000:demo", rec.minted[1].ItemID)
	require.Equal(t, 1000, rec.minted[1].Payload["count"])
}

// A milestone already in the ledger stops its check at the latch: no count and
// no mint attempt, though the counts are past every threshold (the fake
// recorder has not seen these keys, so a check that went on would mint).
func TestCheckMilestones_LatchedMilestoneStopsItsCheck(t *testing.T) {
	db := openTestDB(t)
	rec := newFakeOnceRecorder()
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	for range 1000 {
		insertProjectEvent(t, db, core.EventMemoryWritten, "", "demo", "", "{}", now)
		insertProjectEvent(t, db, core.EventInjected, "", "demo", "", `{"source":"recall"}`, now)
	}
	seedRegisteredProject(t, db, "demo", now.AddDate(-1, 0, -1))
	for _, key := range []string{"memories-1000:demo", "recalls-1000:demo", "birthday-1:demo"} {
		insertProjectEvent(t, db, EventMilestoneReached, "", "demo", key, "{}", now)
	}

	require.NoError(t, CheckMilestones(ctx, db, rec,
		core.Event{Kind: core.EventMemoryWritten, ProjectSlug: "demo", TS: now}))
	require.NoError(t, CheckMilestones(ctx, db, rec,
		core.Event{Kind: core.EventInjected, ProjectSlug: "demo", Payload: map[string]any{"source": "recall"}, TS: now}))
	require.NoError(t, CheckMilestones(ctx, db, rec,
		core.Event{Kind: core.EventSessionStarted, ProjectSlug: "demo", TS: now}))
	require.Empty(t, rec.minted)
}

// The milestones are always on: an unwatched kind runs nothing, and a stored
// features override from when momentum was a switch -- momentum off --
// changes nothing.
func TestCheckMilestones_UnwatchedKindsAndLegacyOverride(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	ts := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	for range 100 {
		insertProjectEvent(t, db, core.EventMemoryWritten, "", "demo", "", "{}", ts)
	}
	require.NoError(t, SetSetting(ctx, db, SettingFeaturesConfig, `{"research":false,"momentum":false,"gamification":false}`))

	rec := newFakeOnceRecorder()
	require.NoError(t, CheckMilestones(ctx, db, rec,
		core.Event{Kind: core.EventNoteWritten, ProjectSlug: "demo", TS: ts}))
	require.Empty(t, rec.minted, "an unwatched kind runs no check")

	require.NoError(t, CheckMilestones(ctx, db, rec,
		core.Event{Kind: core.EventMemoryWritten, ProjectSlug: "demo", TS: ts}))
	require.Len(t, rec.minted, 1)
	require.Equal(t, "memories-100:demo", rec.minted[0].ItemID)
}

func TestCheckMilestones_RecallAnsweredThousandth(t *testing.T) {
	db := openTestDB(t)
	rec := newFakeOnceRecorder()
	ctx := context.Background()
	ts := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	for range 999 {
		insertProjectEvent(t, db, core.EventInjected, "", "demo", "", `{"source":"recall"}`, ts)
	}
	// Passive exposure never counts: kind-browse hits and briefing injections.
	for range 30 {
		insertProjectEvent(t, db, core.EventInjected, "", "demo", "", `{"source":"recall-browse"}`, ts)
	}
	for range 20 {
		insertProjectEvent(t, db, core.EventInjected, "", "demo", "", `{"hook":"session-start"}`, ts)
	}

	recall := core.Event{Kind: core.EventInjected, SessionID: "sess-9", ProjectSlug: "demo",
		Payload: map[string]any{"source": "recall"}, TS: ts}
	require.NoError(t, CheckMilestones(ctx, db, rec, recall))
	require.Empty(t, rec.minted, "999 answered recalls is not the milestone")

	insertProjectEvent(t, db, core.EventInjected, "", "demo", "", `{"source":"recall"}`, ts)
	require.NoError(t, CheckMilestones(ctx, db, rec, recall))
	require.Len(t, rec.minted, 1)
	m := rec.minted[0]
	require.Equal(t, "recalls-1000:demo", m.ItemID)
	require.Equal(t, "1000 recalls answered in demo", m.Payload["claim"])
	require.Equal(t, 1000, m.Payload["count"])
	require.Equal(t, 1000, m.Payload["threshold"])

	// Re-checks past the threshold do not re-mint.
	insertProjectEvent(t, db, core.EventInjected, "", "demo", "", `{"source":"recall"}`, ts)
	require.NoError(t, CheckMilestones(ctx, db, rec, recall))
	require.Len(t, rec.minted, 1)

	// A browse injection is not an answered recall and triggers no check.
	browse := core.Event{Kind: core.EventInjected, ProjectSlug: "demo",
		Payload: map[string]any{"source": "recall-browse"}, TS: ts}
	require.NoError(t, CheckMilestones(ctx, db, rec, browse))
	require.Len(t, rec.minted, 1)
}

func TestCheckMilestones_FirstSupersession(t *testing.T) {
	db := openTestDB(t)
	rec := newFakeOnceRecorder()
	ctx := context.Background()
	ts := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	e := core.Event{Kind: core.EventMemorySuperseded, SessionID: "sess-2",
		ProjectSlug: "demo", ItemID: "01OLDMEMID", TS: ts}
	require.NoError(t, CheckMilestones(ctx, db, rec, e))
	require.Len(t, rec.minted, 1)
	m := rec.minted[0]
	require.Equal(t, "first-supersession:demo", m.ItemID)
	require.Equal(t, "first supersession in demo", m.Payload["claim"])
	require.Equal(t, 1, m.Payload["count"])
	require.Equal(t, "01OLDMEMID", m.Payload["memory"])

	// The second supersession is not a first.
	e.ItemID = "01OTHERMEM"
	require.NoError(t, CheckMilestones(ctx, db, rec, e))
	require.Len(t, rec.minted, 1)
}

func TestCheckMilestones_PlanShippedOnFinalStepDone(t *testing.T) {
	db := openTestDB(t)
	rec := newFakeOnceRecorder()
	ctx := context.Background()
	ts := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	done1 := seedPlanTask(t, db, "demo", "ship-it", core.TaskDone)
	seedPlanTask(t, db, "demo", "ship-it", core.TaskDone)
	last := seedPlanTask(t, db, "demo", "ship-it", core.TaskOpen)
	loose := seedPlanTask(t, db, "demo", "", core.TaskDone)

	transition := func(taskID, to string) core.Event {
		return core.Event{Kind: core.EventTaskTransition, SessionID: "sess-3", ProjectSlug: "demo",
			ItemID: taskID, Payload: map[string]any{"to": to}, TS: ts}
	}

	// A step done while another stays open ships nothing.
	require.NoError(t, CheckMilestones(ctx, db, rec, transition(done1, "done")))
	require.Empty(t, rec.minted)
	// Claiming is not shipping.
	require.NoError(t, CheckMilestones(ctx, db, rec, transition(last, "in_progress")))
	require.Empty(t, rec.minted)
	// A non-plan task closing is not a plan.
	require.NoError(t, CheckMilestones(ctx, db, rec, transition(loose, "done")))
	require.Empty(t, rec.minted)

	// The final step closing as done mints the settlement, once.
	_, err := db.ExecContext(ctx, `UPDATE tasks SET status = 'done' WHERE id = ?`, last)
	require.NoError(t, err)
	require.NoError(t, CheckMilestones(ctx, db, rec, transition(last, "done")))
	require.Len(t, rec.minted, 1)
	m := rec.minted[0]
	require.Equal(t, core.EventPlanShipped, m.Kind)
	require.Equal(t, "ship-it", m.ItemID, "the latch key is the plan slug")
	require.Equal(t, "demo", m.ProjectSlug)
	require.Equal(t, "sess-3", m.SessionID)
	require.Equal(t, "ship-it", m.Payload["plan"])
	require.Equal(t, "demo", m.Payload["project"])
	require.Equal(t, 3, m.Payload["steps"])

	// Repeated rollup recomputes do not re-mint the settlement.
	require.NoError(t, CheckMilestones(ctx, db, rec, transition(last, "done")))
	require.Len(t, rec.minted, 1)

	// The recorder feeds the minted settlement back through CheckMilestones,
	// which latches the first-plan-shipped milestone with the steps verbatim.
	require.NoError(t, CheckMilestones(ctx, db, rec, m))
	require.Len(t, rec.minted, 2)
	first := rec.minted[1]
	require.Equal(t, EventMilestoneReached, first.Kind)
	require.Equal(t, "first-plan-shipped:demo", first.ItemID)
	require.Equal(t, "first plan shipped in demo: ship-it", first.Payload["claim"])
	require.Equal(t, "ship-it", first.Payload["plan"])
	require.Equal(t, 3, first.Payload["steps"])
	require.Equal(t, 1, first.Payload["count"])

	// A later plan mints its own settlement, but the first-milestone latch holds.
	encore := seedPlanTask(t, db, "demo", "encore", core.TaskDone)
	require.NoError(t, CheckMilestones(ctx, db, rec, transition(encore, "done")))
	require.Len(t, rec.minted, 3)
	require.Equal(t, core.EventPlanShipped, rec.minted[2].Kind)
	require.Equal(t, "encore", rec.minted[2].ItemID)
	require.NoError(t, CheckMilestones(ctx, db, rec, rec.minted[2]))
	require.Len(t, rec.minted, 3, "the second shipped plan finds the milestone latch set")
}

func TestCheckMilestones_PlanShippedEventLatchesTheFirstMilestone(t *testing.T) {
	db := openTestDB(t)
	rec := newFakeOnceRecorder()
	ctx := context.Background()
	ts := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	// A plan.shipped event latches the project's first-plan milestone.
	shipped := core.Event{Kind: core.EventPlanShipped, SessionID: "sess-4", ProjectSlug: "demo",
		ItemID: "from-l2", Payload: map[string]any{"slug": "from-l2"}, TS: ts}
	require.NoError(t, CheckMilestones(ctx, db, rec, shipped))
	require.Len(t, rec.minted, 1)
	require.Equal(t, "first-plan-shipped:demo", rec.minted[0].ItemID)
	require.Equal(t, "first plan shipped in demo: from-l2", rec.minted[0].Payload["claim"])

	// A later plan's settlement finds the milestone latch set: no double mint.
	only := seedPlanTask(t, db, "demo", "by-tasks", core.TaskDone)
	require.NoError(t, CheckMilestones(ctx, db, rec,
		core.Event{Kind: core.EventTaskTransition, ProjectSlug: "demo", ItemID: only,
			Payload: map[string]any{"to": "done"}, TS: ts}))
	require.Len(t, rec.minted, 2, "the completion still mints its own settlement")
	require.Equal(t, core.EventPlanShipped, rec.minted[1].Kind)
	require.Equal(t, "by-tasks", rec.minted[1].ItemID)
	require.NoError(t, CheckMilestones(ctx, db, rec, rec.minted[1]))
	require.Len(t, rec.minted, 2, "feeding it back mints no second first-plan milestone")
}

func TestCheckMilestones_ProjectBirthdayLatchesPerYear(t *testing.T) {
	db := openTestDB(t)
	rec := newFakeOnceRecorder()
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	seedRegisteredProject(t, db, "elder", now.AddDate(-2, 0, -1))
	seedRegisteredProject(t, db, "yearling", now.AddDate(-1, 0, 0))
	seedRegisteredProject(t, db, "young", now.AddDate(0, -6, 0))

	started := func(project string) core.Event {
		return core.Event{Kind: core.EventSessionStarted, SessionID: "sess-5", ProjectSlug: project, TS: now}
	}

	// Only the current anniversary mints -- no backfill of earlier years.
	require.NoError(t, CheckMilestones(ctx, db, rec, started("elder")))
	require.Len(t, rec.minted, 1)
	m := rec.minted[0]
	require.Equal(t, "birthday-2:elder", m.ItemID)
	require.Equal(t, "2 years of elder", m.Payload["claim"])
	require.Equal(t, 2, m.Payload["years"])
	require.Equal(t, core.FormatTime(now.AddDate(-2, 0, -1)), m.Payload["registered"])

	// The same year never re-mints.
	require.NoError(t, CheckMilestones(ctx, db, rec, started("elder")))
	require.Len(t, rec.minted, 1)

	// Exactly one year old, singular phrasing.
	require.NoError(t, CheckMilestones(ctx, db, rec, started("yearling")))
	require.Len(t, rec.minted, 2)
	require.Equal(t, "birthday-1:yearling", rec.minted[1].ItemID)
	require.Equal(t, "one year of yearling", rec.minted[1].Payload["claim"])

	// Under a year, and unregistered, earn nothing.
	require.NoError(t, CheckMilestones(ctx, db, rec, started("young")))
	require.NoError(t, CheckMilestones(ctx, db, rec, started("ghost")))
	require.Len(t, rec.minted, 2)
}
