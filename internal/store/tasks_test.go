package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func newTaskDB(t *testing.T) *sql.DB {
	t.Helper()
	return openTestDB(t)
}

// addTask inserts an open task and returns its id. createdAt is offset by seq so
// ordering is deterministic (oldest = smallest seq).
func addTask(t *testing.T, db *sql.DB, project, title string, seq int, deps ...string) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Minute)
	require.NoError(t, CreateTask(context.Background(), db, core.Task{
		ID: id, ProjectSlug: project, Title: title, Status: core.TaskOpen,
		DependsOn: deps, CreatedAt: base, UpdatedAt: base,
	}))
	return id
}

func readyTitles(t *testing.T, db *sql.DB, project string) []string {
	t.Helper()
	tasks, err := ReadyTasks(context.Background(), db, project)
	require.NoError(t, err)
	out := make([]string, len(tasks))
	for i, tk := range tasks {
		out[i] = tk.Title
	}
	return out
}

func setStatus(t *testing.T, db *sql.DB, id string, status core.TaskStatus) {
	t.Helper()
	_, err := UpdateTask(context.Background(), db, id, TaskPatch{Status: &status}, "", time.Now().UTC())
	require.NoError(t, err)
}

func TestReadyQueueBlocksUntilDependencyClosed(t *testing.T) {
	db := newTaskDB(t)
	a := addTask(t, db, "demo", "A", 1)
	addTask(t, db, "demo", "B", 2, a) // B depends on A

	// Only A is ready; B is blocked by open A.
	require.Equal(t, []string{"A"}, readyTitles(t, db, "demo"))

	// A in_progress: it is claimed (out of ready) and still blocks B.
	setStatus(t, db, a, core.TaskInProgress)
	require.Empty(t, readyTitles(t, db, "demo"))

	// A done: B becomes ready.
	setStatus(t, db, a, core.TaskDone)
	require.Equal(t, []string{"B"}, readyTitles(t, db, "demo"))
}

func TestReadyQueueDroppedDependencyUnblocks(t *testing.T) {
	db := newTaskDB(t)
	a := addTask(t, db, "demo", "A", 1)
	addTask(t, db, "demo", "B", 2, a)

	setStatus(t, db, a, core.TaskDropped) // dropping a blocker unblocks its dependents
	require.Equal(t, []string{"B"}, readyTitles(t, db, "demo"))
}

func TestReadyQueueOrderingOldestFirst(t *testing.T) {
	db := newTaskDB(t)
	addTask(t, db, "demo", "third", 3)
	addTask(t, db, "demo", "first", 1)
	addTask(t, db, "demo", "second", 2)
	require.Equal(t, []string{"first", "second", "third"}, readyTitles(t, db, "demo"))
}

func TestReadyQueueMultipleBlockers(t *testing.T) {
	db := newTaskDB(t)
	a := addTask(t, db, "demo", "A", 1)
	b := addTask(t, db, "demo", "B", 2)
	addTask(t, db, "demo", "C", 3, a, b) // C blocked by both A and B

	setStatus(t, db, a, core.TaskDone)
	require.NotContains(t, readyTitles(t, db, "demo"), "C", "C still blocked by B")
	setStatus(t, db, b, core.TaskDone)
	require.Contains(t, readyTitles(t, db, "demo"), "C", "C ready once both blockers close")
}

func TestBlockedTasksListsOpenBlockers(t *testing.T) {
	db := newTaskDB(t)
	a := addTask(t, db, "demo", "A", 1)
	b := addTask(t, db, "demo", "B", 2, a)

	blocked, err := BlockedTasks(context.Background(), db, "demo")
	require.NoError(t, err)
	require.Len(t, blocked, 1)
	require.Equal(t, b, blocked[0].Task.ID)
	require.Len(t, blocked[0].Blockers, 1)
	require.Equal(t, a, blocked[0].Blockers[0].ID)
}

// TestAllBlockedTasksBatchedBlockers exercises the batched blocker fetch: several
// blocked tasks with differing blocker counts, each paired with its own blockers
// in created_at order.
func TestAllBlockedTasksBatchedBlockers(t *testing.T) {
	db := newTaskDB(t)
	a := addTask(t, db, "demo", "A", 1)
	b := addTask(t, db, "demo", "B", 2)
	c := addTask(t, db, "demo", "C", 3, a, b) // blocked by A and B
	d := addTask(t, db, "demo", "D", 4, a)    // blocked by A

	blocked, err := AllBlockedTasks(context.Background(), db)
	require.NoError(t, err)
	byID := make(map[string]BlockedTask, len(blocked))
	for _, bt := range blocked {
		byID[bt.Task.ID] = bt
	}
	require.Len(t, blocked, 2, "only C and D are blocked; A and B are ready")

	require.Contains(t, byID, c)
	require.Equal(t, []string{a, b}, blockerIDs(byID[c]), "C's blockers in created_at order")

	require.Contains(t, byID, d)
	require.Equal(t, []string{a}, blockerIDs(byID[d]))
}

func blockerIDs(bt BlockedTask) []string {
	ids := make([]string, len(bt.Blockers))
	for i, blk := range bt.Blockers {
		ids[i] = blk.ID
	}
	return ids
}

func TestCreateTaskRejectsDanglingDependency(t *testing.T) {
	db := newTaskDB(t)
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	err = CreateTask(context.Background(), db, core.Task{
		ID: id, ProjectSlug: "demo", Title: "X", Status: core.TaskOpen,
		DependsOn: []string{"01NONEXISTENT"}, CreatedAt: now, UpdatedAt: now,
	})
	require.ErrorIs(t, err, ErrTaskNotFound)

	// The task row must not have been committed.
	_, err = TaskByID(context.Background(), db, id)
	require.ErrorIs(t, err, ErrTaskNotFound)
}

func TestDependencyCycleRejected(t *testing.T) {
	db := newTaskDB(t)
	a := addTask(t, db, "demo", "A", 1)
	b := addTask(t, db, "demo", "B", 2, a) // B -> A

	// Adding A -> B would close the cycle A -> B -> A.
	_, err := UpdateTask(context.Background(), db, a, TaskPatch{AddDependsOn: []string{b}}, "", time.Now().UTC())
	require.ErrorIs(t, err, ErrTaskCycle)

	// A self-dependency is rejected too.
	_, err = UpdateTask(context.Background(), db, a, TaskPatch{AddDependsOn: []string{a}}, "", time.Now().UTC())
	require.Error(t, err)
}

func TestUpdateTaskStampsAndClearsClosedAt(t *testing.T) {
	db := newTaskDB(t)
	a := addTask(t, db, "demo", "A", 1)

	done, err := UpdateTask(context.Background(), db, a,
		TaskPatch{Status: statusPtr(core.TaskDone)}, "", time.Now().UTC())
	require.NoError(t, err)
	require.NotNil(t, done.ClosedAt, "moving to done stamps closed_at")

	reopened, err := UpdateTask(context.Background(), db, a,
		TaskPatch{Status: statusPtr(core.TaskOpen)}, "", time.Now().UTC())
	require.NoError(t, err)
	require.Nil(t, reopened.ClosedAt, "reopening clears closed_at")
}

// TestListTasksPopulatesDependsOn pins the batched dependency loading in
// scanTasksWithDeps: every listed task carries its own depends-on ids (and only
// its own), dep-less tasks carry none.
func TestListTasksPopulatesDependsOn(t *testing.T) {
	db := newTaskDB(t)
	a := addTask(t, db, "demo", "A", 1)
	b := addTask(t, db, "demo", "B", 2)
	c := addTask(t, db, "demo", "C", 3, a, b)
	addTask(t, db, "demo", "D", 4)

	all, err := ListTasks(context.Background(), db, "demo", "")
	require.NoError(t, err)
	require.Len(t, all, 4)
	byID := map[string][]string{}
	for _, tk := range all {
		byID[tk.ID] = tk.DependsOn
	}
	require.ElementsMatch(t, []string{a, b}, byID[c], "C carries both dep ids")
	require.Empty(t, byID[a])
	require.Empty(t, byID[b])
}

func TestListTasksFiltersByStatus(t *testing.T) {
	db := newTaskDB(t)
	a := addTask(t, db, "demo", "A", 1)
	addTask(t, db, "demo", "B", 2)
	setStatus(t, db, a, core.TaskDone)

	open, err := ListTasks(context.Background(), db, "demo", core.TaskOpen)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Equal(t, "B", open[0].Title)

	all, err := ListTasks(context.Background(), db, "demo", "")
	require.NoError(t, err)
	require.Len(t, all, 2)
}

func statusPtr(s core.TaskStatus) *core.TaskStatus { return &s }
