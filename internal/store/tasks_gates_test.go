package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// addGated inserts an open task of kind with a gate (who/what) into a plan
// ("" for the default queue), returning its id.
func addGated(t *testing.T, db *sql.DB, plan, title string, seq int, kind core.TaskKind, who, what string, deps ...string) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Minute)
	require.NoError(t, CreateTask(context.Background(), db, core.Task{
		ID: id, ProjectSlug: "demo", Title: title, Status: core.TaskOpen, PlanSlug: plan,
		Kind: kind, Gate: core.TaskGate{Who: who, What: what},
		DependsOn: deps, CreatedAt: base, UpdatedAt: base,
	}))
	return id
}

func blockedTitles(t *testing.T, list []BlockedTask) []string {
	t.Helper()
	out := make([]string, len(list))
	for i, b := range list {
		out[i] = b.Task.Title
	}
	return out
}

// TestQueueGroupsPartitionOpenTasks is the acceptance in one table: every open
// task lands in exactly one of ready, blocked, gated and gates, and a task
// without a gate reads exactly as before.
func TestQueueGroupsPartitionOpenTasks(t *testing.T) {
	ctx := context.Background()
	db := newTaskDB(t)
	free := addTask(t, db, "demo", "free", 1)
	addTask(t, db, "demo", "after-free", 2, free)
	gate := addGated(t, db, "", "GATE sign-off", 3, core.TaskKindGate, "owner", "sign the contract")
	addTask(t, db, "demo", "after-gate", 4, gate)
	addGated(t, db, "", "own-gate", 5, core.TaskKindStep, "sitting:security", "run the drill")
	addTask(t, db, "demo", "after-gate-and-free", 6, gate, free)
	addGated(t, db, "", "GATE hardware", 7, core.TaskKindGate, "hardware", "the board arrives")

	require.Equal(t, []string{"free"}, readyTitles(t, db, "demo"))

	blocked, err := BlockedTasks(ctx, db, "demo")
	require.NoError(t, err)
	require.Equal(t, []string{"after-free"}, blockedTitles(t, blocked))

	gated, err := GatedTasks(ctx, db, "demo")
	require.NoError(t, err)
	require.Equal(t, []string{"after-gate", "own-gate", "after-gate-and-free"}, blockedTitles(t, gated),
		"a step held by people is gated, whatever else blocks it")
	require.Len(t, gated[2].Blockers, 2, "a gated step still lists every unfinished blocker")

	gates, err := GateTasks(ctx, db, "demo")
	require.NoError(t, err)
	require.Equal(t, []string{"GATE hardware", "GATE sign-off"}, blockedTitles(t, gates), "the people's queue is ordered by who")
	require.True(t, gates[1].Task.IsGate())
	require.Equal(t, "sign the contract", gates[1].Task.Gate.What)

	all, err := AllGatedTasks(ctx, db)
	require.NoError(t, err)
	require.Len(t, all, 3)
	allGates, err := AllGateTasks(ctx, db)
	require.NoError(t, err)
	require.Len(t, allGates, 2)
	allReady, err := AllReadyTasks(ctx, db)
	require.NoError(t, err)
	require.Len(t, allReady, 1)
	allBlocked, err := AllBlockedTasks(ctx, db)
	require.NoError(t, err)
	require.Len(t, allBlocked, 1)
}

// TestClearGate_StepBecomesReadyWithoutStatusChange: clearing a step's own
// gate makes it ready and leaves it open, with the clearance on record.
func TestClearGate_StepBecomesReadyWithoutStatusChange(t *testing.T) {
	ctx := context.Background()
	db := newTaskDB(t)
	id := addGated(t, db, "", "own-gate", 1, core.TaskKindStep, "owner", "approve the copy")
	require.Empty(t, readyTitles(t, db, "demo"))

	now := time.Date(2026, 11, 14, 13, 0, 0, 0, time.UTC)
	res, err := UpdateTaskDetailed(ctx, db, id, TaskPatch{ClearGate: &TaskGateClear{By: "cc/one", Note: " approved "}}, "", now)
	require.NoError(t, err)
	require.Equal(t, core.TaskOpen, res.Task.Status, "clearing a step's gate never touches its status")
	require.Equal(t, core.TaskOpen, res.From)
	require.False(t, res.Task.Gate.Open())
	require.NotNil(t, res.Task.Gate.ClearedAt)
	require.True(t, now.Equal(*res.Task.Gate.ClearedAt))
	require.Equal(t, "cc/one", res.Task.Gate.ClearedBy)
	require.Equal(t, "approved", res.Task.Gate.Note)
	require.Equal(t, "approve the copy", res.Task.Gate.What, "the cleared gate stays readable")
	require.Equal(t, []string{"own-gate"}, readyTitles(t, db, "demo"))

	_, err = ClaimTask(ctx, db, id, "sessA", time.Minute, now)
	require.NoError(t, err, "a cleared step is claimable")

	_, err = UpdateTask(ctx, db, id, TaskPatch{ClearGate: &TaskGateClear{}}, "sessA", now)
	require.ErrorIs(t, err, ErrTaskGate, "a cleared gate has nothing left to clear")
}

// TestClearGate_GateTaskClosesAndUnblocks: clearing a gate closes it, and every
// step it held becomes ready.
func TestClearGate_GateTaskClosesAndUnblocks(t *testing.T) {
	ctx := context.Background()
	db := newTaskDB(t)
	gate := addGated(t, db, "", "GATE", 1, core.TaskKindGate, "owner", "decide the vendor")
	addTask(t, db, "demo", "a", 2, gate)
	addTask(t, db, "demo", "b", 3, gate)
	require.Empty(t, readyTitles(t, db, "demo"))

	dropped := core.TaskDropped
	_, err := UpdateTask(ctx, db, gate, TaskPatch{ClearGate: &TaskGateClear{}, Status: &dropped}, "", time.Now().UTC())
	require.ErrorIs(t, err, ErrTaskGate, "clearing a gate closes it as done, never as another status")

	res, err := UpdateTaskDetailed(ctx, db, gate, TaskPatch{ClearGate: &TaskGateClear{By: "owner"}}, "", time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, core.TaskDone, res.Task.Status)
	require.NotNil(t, res.Task.ClosedAt)
	require.Equal(t, []string{"a", "b"}, readyTitles(t, db, "demo"))

	// Reopened, the gate holds again.
	open := core.TaskOpen
	reopened, err := UpdateTask(ctx, db, gate, TaskPatch{Status: &open}, "", time.Now().UTC())
	require.NoError(t, err)
	require.True(t, reopened.Gate.Open(), "a reopened gate is open again")
	require.Empty(t, readyTitles(t, db, "demo"))
}

// TestGatePatch_SetEditReopen covers setting a gate on an existing step,
// editing it, and turning a step into a gate (the GATE-task workaround's
// conversion).
func TestGatePatch_SetEditReopen(t *testing.T) {
	ctx := context.Background()
	db := newTaskDB(t)
	id := addTask(t, db, "demo", "step", 1)
	now := time.Now().UTC()
	who, what, decision := " reviewer:dana ", "pick the name", "which of the three?"
	got, err := UpdateTask(ctx, db, id, TaskPatch{Gate: &TaskGatePatch{Who: &who, What: &what, Decision: &decision}}, "", now)
	require.NoError(t, err)
	require.Equal(t, "reviewer:dana", got.Gate.Who, "gate text is trimmed")
	require.True(t, got.Gate.Open())
	require.Empty(t, readyTitles(t, db, "demo"))

	// Cleared, then edited: an edit opens the gate as it now reads.
	_, err = UpdateTask(ctx, db, id, TaskPatch{ClearGate: &TaskGateClear{By: "owner"}}, "", now)
	require.NoError(t, err)
	readyWhen := "after the board meets"
	got, err = UpdateTask(ctx, db, id, TaskPatch{Gate: &TaskGatePatch{ReadyWhen: &readyWhen}}, "", now)
	require.NoError(t, err)
	require.True(t, got.Gate.Open())
	require.Empty(t, got.Gate.ClearedBy)
	require.Equal(t, "pick the name", got.Gate.What, "an edit keeps the fields it does not name")

	// One patch may not both edit and clear.
	_, err = UpdateTask(ctx, db, id, TaskPatch{Gate: &TaskGatePatch{What: &what}, ClearGate: &TaskGateClear{}}, "", now)
	require.ErrorIs(t, err, ErrTaskGate)

	// A step converted to a gate is listed with the gates.
	gate := core.TaskKindGate
	got, err = UpdateTask(ctx, db, id, TaskPatch{Kind: &gate}, "", now)
	require.NoError(t, err)
	require.True(t, got.IsGate())
	gates, err := GateTasks(ctx, db, "demo")
	require.NoError(t, err)
	require.Equal(t, []string{"step"}, blockedTitles(t, gates))

	// Only removals that name nothing still write nothing.
	res, err := UpdateTaskDetailed(ctx, db, id, TaskPatch{RemoveDependsOn: []string{"nope"}}, "", now)
	require.NoError(t, err)
	require.True(t, res.Unchanged)
}

// TestGateRules refuses gates that break the rules, at create and update.
func TestGateRules(t *testing.T) {
	ctx := context.Background()
	db := newTaskDB(t)
	long := make([]byte, gateLineMaxRunes+1)
	for i := range long {
		long[i] = 'x'
	}
	cases := []struct {
		name string
		kind core.TaskKind
		gate core.TaskGate
	}{
		{"who without what", core.TaskKindStep, core.TaskGate{Who: "owner"}},
		{"what without who", core.TaskKindStep, core.TaskGate{What: "sign"}},
		{"decision alone", core.TaskKindStep, core.TaskGate{Decision: "which?"}},
		{"gate kind without a gate", core.TaskKindGate, core.TaskGate{}},
		{"two-line what", core.TaskKindStep, core.TaskGate{Who: "owner", What: "a\nb"}},
		{"long what", core.TaskKindStep, core.TaskGate{Who: "owner", What: string(long)}},
		{"unknown kind", core.TaskKind("milestone"), core.TaskGate{}},
		{"cleared at birth", core.TaskKindStep, core.TaskGate{Who: "owner", What: "sign", ClearedBy: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := core.NewID()
			require.NoError(t, err)
			now := time.Now().UTC()
			err = CreateTask(ctx, db, core.Task{ID: id, ProjectSlug: "demo", Title: tc.name, Status: core.TaskOpen,
				Kind: tc.kind, Gate: tc.gate, CreatedAt: now, UpdatedAt: now})
			require.ErrorIs(t, err, ErrTaskGate)
		})
	}

	id := addTask(t, db, "demo", "plain", 1)
	gate := core.TaskKindGate
	_, err := UpdateTask(ctx, db, id, TaskPatch{Kind: &gate}, "", time.Now().UTC())
	require.ErrorIs(t, err, ErrTaskGate, "a step becomes a gate only with a gate")
	empty := ""
	withGate := addGated(t, db, "", "gated", 2, core.TaskKindStep, "owner", "sign")
	_, err = UpdateTask(ctx, db, withGate, TaskPatch{Gate: &TaskGatePatch{Who: &empty}}, "", time.Now().UTC())
	require.ErrorIs(t, err, ErrTaskGate, "a gate keeps naming who")
	_, err = UpdateTask(ctx, db, id, TaskPatch{ClearGate: &TaskGateClear{}}, "", time.Now().UTC())
	require.ErrorIs(t, err, ErrTaskGate, "no gate, nothing to clear")
}

// TestClaimTask_GatedRefusals: a gate is never claimed, a step with an open
// gate of its own is refused naming who and what, and a step behind a gate is
// refused as blocked by it.
func TestClaimTask_GatedRefusals(t *testing.T) {
	ctx := context.Background()
	db := newTaskDB(t)
	now := time.Now().UTC()
	gate := addGated(t, db, "", "GATE", 1, core.TaskKindGate, "owner", "sign the contract")
	own := addGated(t, db, "", "own", 2, core.TaskKindStep, "hardware", "the board arrives")
	behind := addTask(t, db, "demo", "behind", 3, gate)

	_, err := ClaimTask(ctx, db, gate, "sessA", time.Minute, now)
	var gated *TaskGatedError
	require.True(t, errors.As(err, &gated), "%v", err)
	require.True(t, gated.IsGate)
	require.ErrorIs(t, err, ErrTaskGated)
	require.Contains(t, err.Error(), "owner: sign the contract")

	_, err = ClaimTask(ctx, db, own, "sessA", time.Minute, now)
	require.True(t, errors.As(err, &gated), "%v", err)
	require.False(t, gated.IsGate)
	require.Contains(t, err.Error(), "waits on hardware: the board arrives")
	gated.Withhold = true
	require.NotContains(t, gated.Error(), "hardware", "a withheld refusal names no one")

	_, err = ClaimTask(ctx, db, behind, "sessA", time.Minute, now)
	var blocked *TaskBlockedError
	require.True(t, errors.As(err, &blocked), "%v", err)
	require.Equal(t, gate, blocked.Blockers[0].ID)

	// A gate marked in progress by hand is still never claimed.
	inProgress := core.TaskInProgress
	_, err = UpdateTask(ctx, db, gate, TaskPatch{Status: &inProgress}, "", now)
	require.NoError(t, err)
	_, err = ClaimTask(ctx, db, gate, "sessA", time.Minute, now)
	require.ErrorIs(t, err, ErrTaskGated)
	gates, err := GateTasks(ctx, db, "demo")
	require.NoError(t, err)
	require.Len(t, gates, 1, "an in-progress gate stays in the people's queue")
}

// TestPlanRollup_GatesOpen: gated steps are not claimable, and the rollup
// counts the gates still waiting on people.
func TestPlanRollup_GatesOpen(t *testing.T) {
	ctx := context.Background()
	db := newTaskDB(t)
	gate := addGated(t, db, "p", "GATE", 1, core.TaskKindGate, "owner", "approve")
	addPlanTask(t, db, "demo", "p", "behind", 2, gate)
	addGated(t, db, "p", "own", 3, core.TaskKindStep, "owner", "sign")
	addPlanTask(t, db, "demo", "p", "free", 4)

	plans, err := ActivePlans(ctx, db, "demo")
	require.NoError(t, err)
	require.Len(t, plans, 1)
	require.Equal(t, 4, plans[0].Total)
	require.Equal(t, 1, plans[0].Claimable)
	require.Equal(t, 2, plans[0].GatesOpen)

	ready, err := ReadyTasksForPlan(ctx, db, "demo", "p")
	require.NoError(t, err)
	require.Len(t, ready, 1)
	gated, err := GatedTasksForPlan(ctx, db, "demo", "p")
	require.NoError(t, err)
	require.Equal(t, []string{"behind", "own"}, blockedTitles(t, gated))
	gates, err := GateTasksForPlan(ctx, db, "demo", "p")
	require.NoError(t, err)
	require.Equal(t, []string{"GATE"}, blockedTitles(t, gates))
	plain, err := GateTasks(ctx, db, "demo")
	require.NoError(t, err)
	require.Empty(t, plain, "plan steps stay out of the default queue")
}

// TestProjectsWithCounts_GatedCountAsBlocked: the board's blocked count is the
// open tasks that are not ready, so work waiting on people needs attention too.
func TestProjectsWithCounts_GatedCountAsBlocked(t *testing.T) {
	ctx := context.Background()
	db := newTaskDB(t)
	gate := addGated(t, db, "", "GATE", 1, core.TaskKindGate, "owner", "approve")
	addTask(t, db, "demo", "behind", 2, gate)
	addTask(t, db, "demo", "free", 3)
	now := time.Now().UTC()
	rows, err := ProjectsWithCounts(ctx, db, ResolveRetrievalWindow("all", now), now, 0)
	require.NoError(t, err)
	for _, r := range rows {
		if r.Project == "demo" {
			require.Equal(t, 3, r.OpenTasks)
			require.Equal(t, 2, r.Blocked)
			return
		}
	}
	t.Fatal("no demo row")
}

// A gate's words are the task's text for updated_account: a restricted
// account's gate edit or clearing note labels the task, and the owner's
// rewrite of the title and body keeps the label while the gate still carries
// words that rewrite did not touch.
func TestTaskUpdatedAccount_FollowsTheGateWords(t *testing.T) {
	db := openTestDB(t)
	acct := restrictedCtx(t, "example-open")
	owner := context.Background()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	str := func(s string) *string { return &s }
	label := func(id string) string {
		t.Helper()
		tk, err := TaskByID(owner, db, id)
		require.NoError(t, err)
		return tk.UpdatedAccount
	}
	mk := func() string {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, CreateTask(owner, db, core.Task{ID: id, ProjectSlug: "example-open", Title: "owner task",
			Body: "owner body", Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}))
		return id
	}

	id := mk()
	_, err := UpdateTask(acct, db, id, TaskPatch{Gate: &TaskGatePatch{Who: str("owner"), What: str("sign")}}, "", now)
	require.NoError(t, err)
	require.Equal(t, "_example_agent", label(id), "an account's gate is its words")

	_, err = UpdateTask(owner, db, id, TaskPatch{Title: str("owner title"), Body: str("owner words")}, "", now)
	require.NoError(t, err)
	require.Equal(t, "_example_agent", label(id), "the account's gate words are still there")

	_, err = UpdateTask(owner, db, id, TaskPatch{Gate: &TaskGatePatch{Who: str("owner"), What: str("sign it")}}, "", now)
	require.NoError(t, err)
	require.Equal(t, "_example_agent", label(id), "an owner edit of the gate alone never clears the label")
	_, err = UpdateTask(owner, db, id, TaskPatch{Title: str("owner title 2"), Body: str("owner words 2"),
		Gate: &TaskGatePatch{What: str("sign it now")}}, "", now)
	require.NoError(t, err)
	require.Empty(t, label(id), "one owner edit that rewrites title, body and gate leaves only the owner's words")

	other := mk()
	_, err = UpdateTask(owner, db, other, TaskPatch{Gate: &TaskGatePatch{Who: str("owner"), What: str("sign")}}, "", now)
	require.NoError(t, err)
	_, err = UpdateTask(acct, db, other, TaskPatch{ClearGate: &TaskGateClear{By: "cc/agent", Note: "signed"}}, "", now)
	require.NoError(t, err)
	require.Equal(t, "_example_agent", label(other), "a clearing's note is the account's words")
}
