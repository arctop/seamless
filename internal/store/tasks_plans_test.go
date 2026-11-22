package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// TestPlanRollupsForProject_KeepsCompletePlansActivePlansDrops pins the one
// difference between the two rollup views: the console needs the finished plans
// (it renders them as a collapsed group), the briefing contract does not.
func TestPlanRollupsForProject_KeepsCompletePlansActivePlansDrops(t *testing.T) {
	db := newTaskDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	seed := func(plan, title string, status core.TaskStatus, at time.Time) {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, CreateTask(ctx, db, core.Task{
			ID: id, ProjectSlug: "demo", Title: title, Status: status,
			PlanSlug: plan, CreatedAt: at, UpdatedAt: at,
		}))
	}
	// "shipped" is fully closed; "running" still has an open step.
	seed("shipped", "wire it up", core.TaskDone, now.Add(-2*time.Hour))
	seed("shipped", "ship it", core.TaskDropped, now.Add(-time.Hour))
	seed("running", "keep going", core.TaskOpen, now)

	all, err := PlanRollupsForProject(ctx, db, "demo")
	require.NoError(t, err)
	bySlug := map[string]PlanRollup{}
	for _, p := range all {
		bySlug[p.Slug] = p
	}
	require.Len(t, all, 2, "the full set carries every plan")
	require.Equal(t, 2, bySlug["shipped"].Total)
	require.Equal(t, 2, bySlug["shipped"].Done, "dropped counts as closed")
	require.False(t, bySlug["shipped"].LastActivity.IsZero())

	active, err := ActivePlans(ctx, db, "demo")
	require.NoError(t, err)
	require.Len(t, active, 1, "ActivePlans still drops the complete plan")
	require.Equal(t, "running", active[0].Slug)
	require.Equal(t, 1, active[0].Claimable, "an unblocked open step is claimable")
}

// TestPlanRollupsForProject_EmptyProject keeps the no-plans case a nil slice
// rather than an error, so the console can range over it directly.
func TestPlanRollupsForProject_EmptyProject(t *testing.T) {
	db := newTaskDB(t)
	rollups, err := PlanRollupsForProject(context.Background(), db, "nobody")
	require.NoError(t, err)
	require.Empty(t, rollups)
}

// TestPlanRollup_AtFinishLine pins the qualification both momentum surfaces
// derive from: integer math against PlanFinishLinePct, complete plans excluded.
func TestPlanRollup_AtFinishLine(t *testing.T) {
	for _, tc := range []struct {
		done, total int
		want        bool
	}{
		{4, 5, true},  // exactly 80%
		{8, 9, true},  // the "one step from shipped" shape
		{3, 4, false}, // 75% is under the line, never rounded up
		{1, 2, false}, // small plans do not qualify early
		{5, 5, false}, // complete is shipped, not at the finish line
		{0, 0, false}, // no steps, no line
		{9, 10, true},
	} {
		p := PlanRollup{Done: tc.done, Total: tc.total}
		require.Equal(t, tc.want, p.AtFinishLine(), "%d/%d", tc.done, tc.total)
	}
	require.Equal(t, "one step from shipped", PlanRollup{Done: 8, Total: 9}.FinishLinePhrase())
	require.Equal(t, "2 steps from shipped", PlanRollup{Done: 8, Total: 10}.FinishLinePhrase())
}

// TestFinishLinePlans covers the cross-project card query: only qualifying
// active plans return, each with its exact remaining step titles oldest-first.
func TestFinishLinePlans(t *testing.T) {
	db := newTaskDB(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)

	seed := func(project, plan, title string, status core.TaskStatus, at time.Time) {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, CreateTask(ctx, db, core.Task{
			ID: id, ProjectSlug: project, Title: title, Status: status,
			PlanSlug: plan, CreatedAt: at, UpdatedAt: at,
		}))
	}
	// "nearly" in demo: 4/5 done -- qualifies, one step left.
	for i, st := range []core.TaskStatus{core.TaskDone, core.TaskDone, core.TaskDone, core.TaskDropped} {
		seed("demo", "nearly", "closed step", st, now.Add(time.Duration(i)*time.Minute))
	}
	seed("demo", "nearly", "write the docs", core.TaskInProgress, now.Add(10*time.Minute))
	// "halfway" in demo: 1/2 done -- does not qualify.
	seed("demo", "halfway", "done half", core.TaskDone, now)
	seed("demo", "halfway", "open half", core.TaskOpen, now)
	// "done" in other: complete -- shipped plans get no card.
	seed("other", "done", "all closed", core.TaskDone, now)
	// "almost" in other: 4/5 with two orderings -- remaining titles oldest-first.
	for i := range 4 {
		seed("other", "almost", "closed step", core.TaskDone, now.Add(time.Duration(i)*time.Minute))
	}
	seed("other", "almost", "final review", core.TaskOpen, now.Add(20*time.Minute))

	plans, err := FinishLinePlans(ctx, db)
	require.NoError(t, err)
	require.Len(t, plans, 2)
	byKey := map[string]FinishLinePlan{}
	for _, p := range plans {
		byKey[p.Project+"/"+p.Slug] = p
	}
	nearly := byKey["demo/nearly"]
	require.Equal(t, 5, nearly.Total)
	require.Equal(t, 4, nearly.Done)
	require.Equal(t, []string{"write the docs"}, nearly.Remaining)
	require.Equal(t, "one step from shipped", nearly.FinishLinePhrase())
	almost := byKey["other/almost"]
	require.Equal(t, []string{"final review"}, almost.Remaining)

	// The empty case is a nil slice, not an error: no card is the empty state.
	empty, err := FinishLinePlans(ctx, newTaskDB(t))
	require.NoError(t, err)
	require.Empty(t, empty)
}

// A slug's steps name their projects once each, and the dropped steps are
// counted per plan, keyed by project and slug: one slug in two projects is
// two plans (plan:console-ux-pass UX 07).
func TestPvbPlanStepProjectsAndDroppedCounts(t *testing.T) {
	db := newTaskDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	seed := func(project, plan string, status core.TaskStatus) {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, CreateTask(ctx, db, core.Task{
			ID: id, ProjectSlug: project, Title: "step", Status: status,
			PlanSlug: plan, CreatedAt: now, UpdatedAt: now,
		}))
	}
	seed("example-web", "shared", core.TaskOpen)
	seed("example-ops", "shared", core.TaskDropped)
	seed("example-ops", "shared", core.TaskDone)
	seed("example-ops", "shared", core.TaskDropped)
	seed("", "shared", core.TaskOpen)
	seed("example-ops", "other", core.TaskDone)
	seed("example-ops", "", core.TaskDropped) // off-plan: no plan's drop

	projects, err := PlanStepProjects(ctx, db, "shared")
	require.NoError(t, err)
	require.Equal(t, []string{"", "example-ops", "example-web"}, projects)
	none, err := PlanStepProjects(ctx, db, "no-such-plan")
	require.NoError(t, err)
	require.Empty(t, none)
	offPlan, err := PlanStepProjects(ctx, db, "")
	require.NoError(t, err)
	require.Empty(t, offPlan, "the empty slug is no plan")

	dropped, err := PlanDroppedCounts(ctx, db)
	require.NoError(t, err)
	require.Equal(t, map[PlanRef]int{{Project: "example-ops", Slug: "shared"}: 2}, dropped)
}
