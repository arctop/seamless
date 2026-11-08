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
