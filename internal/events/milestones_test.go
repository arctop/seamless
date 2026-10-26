package events

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

func TestRecord_ArmedRecorderMintsMilestoneOnce(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()
	r.SetFeatures(config.Features{Momentum: true})

	for i := range 100 {
		_, err := r.Record(ctx, core.Event{
			Kind: core.EventMemoryWritten, SessionID: "sess-1",
			ProjectSlug: "demo", ItemID: "mem-" + strconv.Itoa(i),
		})
		require.NoError(t, err)
	}
	got, err := r.ByKinds(ctx, []core.EventKind{store.EventMilestoneReached}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, got, 1, "the 100th write mints the milestone exactly once")
	m := got[0]
	require.Equal(t, "memories-100:demo", m.ItemID)
	require.Equal(t, "demo", m.ProjectSlug)
	require.Equal(t, "sess-1", m.SessionID)
	require.Equal(t, "100 memories written in demo", m.Payload["claim"])
	require.EqualValues(t, 100, m.Payload["count"])

	// The 101st write re-checks but the latch holds.
	_, err = r.Record(ctx, core.Event{Kind: core.EventMemoryWritten, ProjectSlug: "demo", ItemID: "mem-100"})
	require.NoError(t, err)
	got, err = r.ByKinds(ctx, []core.EventKind{store.EventMilestoneReached}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
}

// The whole plan-shipped chain through the real recorder: a task.transition
// recorded on any path (the MCP tasks_update tool records exactly this shape)
// mints the plan.shipped settlement once via the post-insert hook, and the
// settlement's own re-entrant pass latches the first-plan-shipped milestone --
// no caller anywhere calls a plan-shipped API explicitly.
func TestRecord_TaskTransitionMintsPlanShippedOnce(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	r := NewRecorder(db)
	r.SetFeatures(config.Features{Momentum: true})
	ctx := context.Background()

	seed := func(status core.TaskStatus) string {
		id, err := core.NewID()
		require.NoError(t, err)
		now := time.Now().UTC()
		require.NoError(t, store.CreateTask(ctx, db, core.Task{
			ID: id, ProjectSlug: "demo", Title: "step", Status: status,
			PlanSlug: "ship-it", CreatedAt: now, UpdatedAt: now,
		}))
		return id
	}
	seed(core.TaskDone)
	last := seed(core.TaskOpen)

	_, err = db.ExecContext(ctx, `UPDATE tasks SET status = 'done' WHERE id = ?`, last)
	require.NoError(t, err)
	transition := core.Event{Kind: core.EventTaskTransition, SessionID: "sess-1",
		ProjectSlug: "demo", ItemID: last, Payload: map[string]any{"to": "done"}}
	_, err = r.Record(ctx, transition)
	require.NoError(t, err)

	shipped, err := r.ByKinds(ctx, []core.EventKind{core.EventPlanShipped}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, shipped, 1, "the completing transition mints the settlement")
	require.Equal(t, "ship-it", shipped[0].ItemID)
	require.Equal(t, "demo", shipped[0].ProjectSlug)
	require.Equal(t, "sess-1", shipped[0].SessionID)
	require.Equal(t, "ship-it", shipped[0].Payload["plan"])
	require.EqualValues(t, 2, shipped[0].Payload["steps"])

	firsts, err := r.ByKinds(ctx, []core.EventKind{store.EventMilestoneReached}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, firsts, 1, "the settlement's re-entrant pass latched the milestone")
	require.Equal(t, "first-plan-shipped:demo", firsts[0].ItemID)
	require.EqualValues(t, 2, firsts[0].Payload["steps"])

	// A repeated rollup recompute -- the same transition recorded again -- finds
	// both latches set.
	_, err = r.Record(ctx, transition)
	require.NoError(t, err)
	shipped, err = r.ByKinds(ctx, []core.EventKind{core.EventPlanShipped}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, shipped, 1)
	firsts, err = r.ByKinds(ctx, []core.EventKind{store.EventMilestoneReached}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, firsts, 1)
}

func TestRecord_UnarmedRecorderMintsNothing(t *testing.T) {
	r := newRecorder(t) // SetFeatures never called: behavior as before
	ctx := context.Background()

	for i := range 100 {
		_, err := r.Record(ctx, core.Event{
			Kind: core.EventMemoryWritten, ProjectSlug: "demo", ItemID: "mem-" + strconv.Itoa(i),
		})
		require.NoError(t, err)
	}
	got, err := r.ByKinds(ctx, []core.EventKind{store.EventMilestoneReached}, "", "", 10)
	require.NoError(t, err)
	require.Empty(t, got)
}
