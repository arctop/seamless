package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// updated_account is the account whose words a task's title or body may still
// carry (taskTextAccount), never the last account to touch the row: a status
// flip, a claim, a release, a lease steal, a move, a dependency, a commit or a
// star moves nothing.
func TestTaskUpdatedAccount_FollowsTheText(t *testing.T) {
	db := openTestDB(t)
	acct := restrictedCtx(t, "example-open")
	owner := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	mk := func(title, body string) string {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, CreateTask(owner, db, core.Task{ID: id, ProjectSlug: "example-open", Title: title,
			Body: body, Status: core.TaskOpen, CreatedBy: "cc/owner", CreatedAt: now, UpdatedAt: now}))
		return id
	}
	label := func(id string) string {
		t.Helper()
		tk, err := TaskByID(owner, db, id)
		require.NoError(t, err)
		require.Empty(t, tk.Account, "the owner created every task here")
		return tk.UpdatedAccount
	}
	update := func(ctx context.Context, id string, p TaskPatch, actor string) {
		t.Helper()
		_, err := UpdateTask(ctx, db, id, p, actor, now)
		require.NoError(t, err)
	}
	str := func(s string) *string { return &s }
	status := func(s core.TaskStatus) *core.TaskStatus { return &s }
	const agent = "_example_agent"

	t.Run("a claim, release or close of the owner's task adds no label", func(t *testing.T) {
		id := mk("owner task", "owner body")
		_, err := ClaimTask(acct, db, id, "cc/agent", time.Hour, now)
		require.NoError(t, err)
		require.Empty(t, label(id), "claim")
		_, err = ClaimTask(acct, db, id, "cc/agent", time.Hour, now.Add(time.Minute))
		require.NoError(t, err)
		require.Empty(t, label(id), "heartbeat")
		_, err = ReleaseTask(acct, db, id, "cc/agent", now)
		require.NoError(t, err)
		require.Empty(t, label(id), "release")
		_, err = ClaimTask(acct, db, id, "cc/agent", time.Hour, now)
		require.NoError(t, err)
		_, err = ForceReleaseTask(acct, db, id, now)
		require.NoError(t, err)
		require.Empty(t, label(id), "force release")
		_, err = ClaimTask(acct, db, id, "cc/agent", time.Hour, now)
		require.NoError(t, err)
		n, err := ReleaseClaimsForSession(acct, db, "cc/agent", now)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		require.Empty(t, label(id), "session teardown")
		// A lapsed lease stolen by the account, then a close with a commit.
		_, err = ClaimTask(owner, db, id, "cc/owner", time.Minute, now)
		require.NoError(t, err)
		_, err = ClaimTask(acct, db, id, "cc/agent", time.Hour, now.Add(time.Hour))
		require.NoError(t, err)
		update(acct, id, TaskPatch{Status: status(core.TaskDone), AddCommits: []string{strings.Repeat("ab", 20)}}, "cc/agent")
		require.Empty(t, label(id), "a status change and a commit write no text")
		// Resending the text it already has is no change of it either.
		update(acct, id, TaskPatch{Title: str("owner task"), Body: str("owner body")}, "")
		require.Empty(t, label(id), "unchanged text")
	})

	t.Run("a restricted title or body rewrite labels it", func(t *testing.T) {
		byTitle := mk("owner task", "owner body")
		update(acct, byTitle, TaskPatch{Title: str("the account's title")}, "")
		require.Equal(t, agent, label(byTitle))
		byBody := mk("owner task", "owner body")
		update(acct, byBody, TaskPatch{Body: str("the account's body")}, "")
		require.Equal(t, agent, label(byBody))
	})

	t.Run("everything else the owner does keeps the label", func(t *testing.T) {
		id := mk("owner task", "owner body")
		update(acct, id, TaskPatch{Body: str("the account's body")}, "")
		blocker := mk("blocker", "")
		update(owner, id, TaskPatch{Status: status(core.TaskInProgress)}, "")
		require.Equal(t, agent, label(id), "status flip")
		update(owner, id, TaskPatch{ProjectSlug: str("example-other")}, "")
		require.Equal(t, agent, label(id), "project move")
		update(owner, id, TaskPatch{AddDependsOn: []string{blocker}}, "")
		require.Equal(t, agent, label(id), "dependency")
		update(owner, id, TaskPatch{AddCommits: []string{strings.Repeat("cd", 20)}}, "")
		require.Equal(t, agent, label(id), "commit")
		require.NoError(t, SetTaskFavorite(owner, db, id, true))
		require.Equal(t, agent, label(id), "star")
		_, err := ClaimTask(owner, db, id, "cc/owner", time.Hour, now)
		require.NoError(t, err)
		_, err = ReleaseTask(owner, db, id, "cc/owner", now)
		require.NoError(t, err)
		require.Equal(t, agent, label(id), "the owner's claim and release")
		update(owner, id, TaskPatch{Status: status(core.TaskDone)}, "")
		require.Equal(t, agent, label(id), "the owner closing it")
	})

	t.Run("the owner's rewrite clears it once no other text is left", func(t *testing.T) {
		id := mk("owner task", "owner body")
		update(acct, id, TaskPatch{Title: str("the account's title")}, "")
		// The body alone: the account's title is still the task's title.
		update(owner, id, TaskPatch{Body: str("the owner's new body")}, "")
		require.Equal(t, agent, label(id), "an owner's body edit leaves the account's title")
		update(owner, id, TaskPatch{Title: str("the owner's title"), Body: str("the owner's body")}, "")
		require.Empty(t, label(id), "title and body both the owner's")

		bare := mk("owner task", "")
		update(acct, bare, TaskPatch{Title: str("the account's title")}, "")
		update(owner, bare, TaskPatch{Title: str("the owner's title")}, "")
		require.Empty(t, label(bare), "a task with no body is its title")

		bodied := mk("owner task", "owner body")
		update(acct, bodied, TaskPatch{Body: str("the account's body")}, "")
		update(owner, bodied, TaskPatch{Title: str("the owner's title")}, "")
		require.Equal(t, agent, label(bodied), "an owner's title edit leaves the account's body")
	})
}

func TestTaskTextAccount(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		current, writer           string
		titleChanged, bodyChanged bool
		body                      string
		want                      string
	}{
		{"owner, no text change", "bob", "", false, false, "b", "bob"},
		{"account, no text change", "", "bob", false, false, "b", ""},
		{"account title", "", "bob", true, false, "b", "bob"},
		{"account body", "", "bob", false, true, "b", "bob"},
		{"another account's rewrite", "bob", "eve", false, true, "b", "eve"},
		{"owner title over a body", "bob", "", true, false, "b", "bob"},
		{"owner body under a title", "bob", "", false, true, "b", "bob"},
		{"owner title and body", "bob", "", true, true, "b", ""},
		{"owner title, empty body", "bob", "", true, false, "", ""},
		{"owner empties the body only", "bob", "", false, true, "", "bob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, taskTextAccount(tc.current, tc.writer, tc.titleChanged, tc.bodyChanged, tc.body))
		})
	}
}
