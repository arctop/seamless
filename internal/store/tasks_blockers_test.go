package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// The claim's "task blocked" refusal carries its blockers, so the tool surface
// can judge each against its caller's read fence; its text with nothing
// withheld is the text it always had.
func TestClaimTask_BlockedRefusalCarriesBlockers(t *testing.T) {
	db := newTaskDB(t)
	a := addTask(t, db, "example-alpha", "A", 1)
	b := addTask(t, db, "example-beta", "B", 2)
	c := addTask(t, db, "example-alpha", "C", 3, a, b)
	now := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)

	_, err := ClaimTask(context.Background(), db, c, "sessA", time.Minute, now)
	require.ErrorIs(t, err, ErrTaskBlocked)
	var blocked *TaskBlockedError
	require.True(t, errors.As(err, &blocked))
	require.Equal(t, c, blocked.TaskID)
	ids := []string{}
	for _, bl := range blocked.Blockers {
		ids = append(ids, bl.ID+"@"+bl.ProjectSlug)
	}
	require.Equal(t, []string{a + "@example-alpha", b + "@example-beta"}, ids, "oldest first, each with its project")
	require.Equal(t, `store.ClaimTask: task blocked: task "`+c+`" waits on `+a+` (open), `+b+` (open)`, err.Error())

	// A surface keeps the readable blockers and says what it left out.
	trimmed := &TaskBlockedError{TaskID: c, Blockers: blocked.Blockers[:1], Withheld: "one more elsewhere"}
	require.ErrorIs(t, trimmed, ErrTaskBlocked)
	require.Equal(t, `store.ClaimTask: task blocked: task "`+c+`" waits on `+a+` (open), one more elsewhere`, trimmed.Error())
	none := &TaskBlockedError{TaskID: c, Withheld: "two tasks elsewhere"}
	require.Equal(t, `store.ClaimTask: task blocked: task "`+c+`" waits on two tasks elsewhere`, none.Error())

	// A remedy rides on a second line, the way a lock refusal ends; without
	// one the text is exactly as before.
	remedied := &TaskBlockedError{TaskID: c, Withheld: "a task in locked project example-finance",
		Remedy: "bind to example-finance, or approve a request for it."}
	require.ErrorIs(t, remedied, ErrTaskBlocked)
	require.Equal(t, `store.ClaimTask: task blocked: task "`+c+`" waits on a task in locked project example-finance`+
		"\nRemedy: bind to example-finance, or approve a request for it.", remedied.Error())
}

func TestTaskProjects(t *testing.T) {
	db := newTaskDB(t)
	ctx := context.Background()
	a := addTask(t, db, "example-alpha", "A", 1)
	b := addTask(t, db, "example-beta", "B", 2)
	g := addTask(t, db, "", "G", 3)
	missing, err := core.NewID()
	require.NoError(t, err)

	got, err := TaskProjects(ctx, db, []string{a, b, a, g, missing, ""})
	require.NoError(t, err)
	require.Equal(t, map[string]string{a: "example-alpha", b: "example-beta", g: ""}, got,
		"duplicates collapse, unknown ids are absent, the global scope is \"\"")

	got, err = TaskProjects(ctx, db, nil)
	require.NoError(t, err)
	require.Empty(t, got)
}

// OpenBlockerProjects reads, for the tasks asked about, the projects of their
// unfinished blockers among the ones named: once per blocker, sorted, and
// nothing about a finished blocker or another project's.
func TestOpenBlockerProjects(t *testing.T) {
	db := newTaskDB(t)
	ctx := context.Background()
	fin1 := addTask(t, db, "example-finance", "F1", 1)
	fin2 := addTask(t, db, "example-finance", "F2", 2)
	legal := addTask(t, db, "example-legal", "L", 3)
	done := addTask(t, db, "example-finance", "F3", 4)
	closed := core.TaskDone
	_, err := UpdateTask(ctx, db, done, TaskPatch{Status: &closed}, "", time.Now())
	require.NoError(t, err)
	ops := addTask(t, db, "example-ops", "O", 5)
	a := addTask(t, db, "example-ops", "A", 6, fin1, fin2, legal, ops)
	b := addTask(t, db, "example-ops", "B", 7, done, ops)
	c := addTask(t, db, "example-ops", "C", 8)

	got, err := OpenBlockerProjects(ctx, db, []string{a, b, c, a}, []string{"example-finance", "example-legal"})
	require.NoError(t, err)
	require.Equal(t, map[string][]string{a: {"example-finance", "example-finance", "example-legal"}}, got,
		"one entry per open blocker; a finished one, and one outside the projects named, are not counted")

	got, err = OpenBlockerProjects(ctx, db, []string{a}, nil)
	require.NoError(t, err)
	require.Empty(t, got)
	got, err = OpenBlockerProjects(ctx, db, nil, []string{"example-finance"})
	require.NoError(t, err)
	require.Empty(t, got)
}

// Removing a blocker: the ids that were blockers stop being ones, the rest are
// named back as absent, and a task whose last unfinished blocker goes is
// ready. The row's updated_at moves; its updated_account does not.
func TestUpdateTaskDetailed_RemovesBlockers(t *testing.T) {
	db := newTaskDB(t)
	ctx := context.Background()
	open := addTask(t, db, "demo", "open blocker", 1)
	done := addTask(t, db, "demo", "done blocker", 2)
	setStatus(t, db, done, core.TaskDone)
	stranger := addTask(t, db, "demo", "not a blocker", 3)
	waits := addTask(t, db, "demo", "waits", 4, open, done)
	missing, err := core.NewID()
	require.NoError(t, err)
	require.Equal(t, []string{"open blocker", "not a blocker"}, readyTitles(t, db, "demo"))

	before, err := TaskByID(ctx, db, waits)
	require.NoError(t, err)
	at := before.UpdatedAt.Add(time.Hour)
	res, err := UpdateTaskDetailed(ctx, db, waits,
		TaskPatch{RemoveDependsOn: []string{stranger, open, "", missing, open}}, "", at)
	require.NoError(t, err)
	require.False(t, res.Unchanged)
	require.Equal(t, []string{open}, res.RemovedDependsOn)
	require.Equal(t, []string{stranger, missing}, res.AbsentDependsOn, "the patch's order, deduplicated, blanks dropped")
	require.Equal(t, core.TaskOpen, res.From)
	require.Equal(t, []string{done}, res.Task.DependsOn, "the finished blocker stays an edge")
	require.True(t, res.Task.UpdatedAt.Equal(at.UTC()), "a removal is a change of the row")
	require.Empty(t, res.Task.UpdatedAccount)

	require.Equal(t, []string{"open blocker", "not a blocker", "waits"}, readyTitles(t, db, "demo"),
		"its last unfinished blocker gone, the task is ready")
	blocked, err := BlockedTasks(ctx, db, "demo")
	require.NoError(t, err)
	require.Empty(t, blocked)

	// UpdateTask is the same update, answering the task alone.
	got, err := UpdateTask(ctx, db, waits, TaskPatch{RemoveDependsOn: []string{done}}, "", at)
	require.NoError(t, err)
	require.Empty(t, got.DependsOn)
}

// A patch of removals alone that names no blocker writes nothing: the row,
// its updated_at and the search mirror are as they were. With another field
// in the patch the update lands as always and the removal is still reported.
func TestUpdateTaskDetailed_RemovingANonBlockerIsANoOp(t *testing.T) {
	db := newTaskDB(t)
	ctx := context.Background()
	blocker := addTask(t, db, "demo", "blocker", 1)
	other := addTask(t, db, "demo", "other", 2)
	waits := addTask(t, db, "demo", "waits", 3, blocker)
	before, err := TaskByID(ctx, db, waits)
	require.NoError(t, err)

	res, err := UpdateTaskDetailed(ctx, db, waits, TaskPatch{RemoveDependsOn: []string{other}}, "", before.UpdatedAt.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, res.Unchanged)
	require.Empty(t, res.RemovedDependsOn)
	require.Equal(t, []string{other}, res.AbsentDependsOn)
	require.Equal(t, before, res.Task, "the answer is the row as it stands")
	after, err := TaskByID(ctx, db, waits)
	require.NoError(t, err)
	require.Equal(t, before, after, "nothing written, updated_at included")

	title := "waits, renamed"
	res, err = UpdateTaskDetailed(ctx, db, waits, TaskPatch{Title: &title, RemoveDependsOn: []string{other}}, "", before.UpdatedAt.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, res.Unchanged)
	require.Equal(t, []string{other}, res.AbsentDependsOn)
	require.Equal(t, title, res.Task.Title)
	require.Equal(t, []string{blocker}, res.Task.DependsOn)
}

// The holder lock comes first: a task another session holds refuses a
// removal, one that would be a no-op included, and keeps its edges. The holder
// removes freely.
func TestUpdateTaskDetailed_RemovalBehindTheHolderLock(t *testing.T) {
	db := newTaskDB(t)
	ctx := context.Background()
	blocker := addTask(t, db, "demo", "blocker", 1)
	setStatus(t, db, blocker, core.TaskDone)
	held := addTask(t, db, "demo", "held", 2, blocker)
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	_, err := ClaimTask(ctx, db, held, "cc/holder", time.Hour, now)
	require.NoError(t, err)

	for _, ids := range [][]string{{blocker}, {"01JZZZZZZZZZZZZZZZZZZZZZZZ"}} {
		_, err = UpdateTaskDetailed(ctx, db, held, TaskPatch{RemoveDependsOn: ids}, "cc/other", now)
		require.ErrorIs(t, err, ErrTaskClaimConflict, "%v", ids)
	}
	tk, err := TaskByID(ctx, db, held)
	require.NoError(t, err)
	require.Equal(t, []string{blocker}, tk.DependsOn)

	res, err := UpdateTaskDetailed(ctx, db, held, TaskPatch{RemoveDependsOn: []string{blocker}}, "cc/holder", now)
	require.NoError(t, err)
	require.Equal(t, []string{blocker}, res.RemovedDependsOn)
	require.Equal(t, core.TaskInProgress, res.Task.Status, "a removal moves no status, and keeps the claim")
	require.Equal(t, "cc/holder", res.Task.ClaimedBy)
}

// One patch may not both add and remove the same blocker: refused before
// anything is written.
func TestUpdateTaskDetailed_AddAndRemoveTheSameBlockerRefused(t *testing.T) {
	db := newTaskDB(t)
	ctx := context.Background()
	blocker := addTask(t, db, "demo", "blocker", 1)
	other := addTask(t, db, "demo", "other", 2)
	waits := addTask(t, db, "demo", "waits", 3, blocker)

	_, err := UpdateTaskDetailed(ctx, db, waits, TaskPatch{
		AddDependsOn: []string{other, blocker}, RemoveDependsOn: []string{blocker},
	}, "", time.Now())
	require.ErrorIs(t, err, ErrTaskDepConflict)
	require.Equal(t, `store.UpdateTask: dependency both added and removed: "`+blocker+`"`, err.Error())
	tk, err := TaskByID(ctx, db, waits)
	require.NoError(t, err)
	require.Equal(t, []string{blocker}, tk.DependsOn)

	// Adding one and removing another is one update.
	res, err := UpdateTaskDetailed(ctx, db, waits, TaskPatch{
		AddDependsOn: []string{other}, RemoveDependsOn: []string{blocker},
	}, "", time.Now())
	require.NoError(t, err)
	require.Equal(t, []string{other}, res.Task.DependsOn)
}

// Under peer_auth, a blocker in a project the request does not reach counts
// as not a blocker: kept, and reported exactly like an id that names nothing.
// A restricted account's removal of a blocker it reaches keeps the task's
// text label: the label follows the title and body, never the edges.
func TestUpdateTaskDetailed_RemovalJudgesTheBlockersReach(t *testing.T) {
	db := openTestDB(t)
	acct := restrictedCtx(t, "example-open")
	owner := context.Background()
	near := addTask(t, db, "example-open", "near", 1)
	far := addTask(t, db, "example-closed", "far", 2)
	waits := addTask(t, db, "example-open", "waits", 3, near, far)
	missing, err := core.NewID()
	require.NoError(t, err)
	at := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

	res, err := UpdateTaskDetailed(acct, db, waits, TaskPatch{RemoveDependsOn: []string{far, missing}}, "", at)
	require.NoError(t, err)
	require.True(t, res.Unchanged)
	require.Equal(t, []string{far, missing}, res.AbsentDependsOn, "the unreachable blocker reads like a missing id")

	res, err = UpdateTaskDetailed(acct, db, waits, TaskPatch{RemoveDependsOn: []string{near, far}}, "", at)
	require.NoError(t, err)
	require.Equal(t, []string{near}, res.RemovedDependsOn)
	require.Equal(t, []string{far}, res.AbsentDependsOn)
	require.Empty(t, res.Task.UpdatedAccount, "a removal writes no text, so it puts no label on")

	// The owner, with no View, reaches it.
	res, err = UpdateTaskDetailed(owner, db, waits, TaskPatch{RemoveDependsOn: []string{far}}, "", at)
	require.NoError(t, err)
	require.Equal(t, []string{far}, res.RemovedDependsOn)
	require.Empty(t, res.Task.DependsOn)
}
