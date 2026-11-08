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
