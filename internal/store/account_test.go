package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
)

// restrictedCtx is a request context whose View is a restricted local
// account's, as the peer_auth transport builds it.
func restrictedCtx(t *testing.T, open ...string) context.Context {
	t.Helper()
	p, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", Restricted: []string{"_example_agent"}},
		func(name string) (access.Account, error) {
			if name == "owner" {
				return access.Account{UID: 501, Name: "owner"}, nil
			}
			return access.Account{UID: 502, Name: "_example_agent"}, nil
		})
	require.NoError(t, err)
	v, ok := p.PeerView(502)
	require.True(t, ok)
	return access.WithView(context.Background(), v.WithOpenProjects(open))
}

// Sessions, tasks and trials record the restricted account a request speaks
// for, from the request itself: no surface has to remember to stamp it.
func TestAccountColumns_StampedFromTheRequest(t *testing.T) {
	db := openTestDB(t)
	ctx := restrictedCtx(t, "example-open")
	owner := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

	require.NoError(t, CreateSession(ctx, db, core.Session{ID: "01K0SESS0000000000000000A1", Name: "cc/agent",
		ProjectSlug: "example-open", Status: core.SessionActive, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, CreateSession(owner, db, core.Session{ID: "01K0SESS0000000000000000A2", Name: "cc/owner",
		ProjectSlug: "example-open", Status: core.SessionActive, CreatedAt: now, UpdatedAt: now}))
	// The owner names the account's session by its qualified name.
	s, ok, err := SessionByName(owner, db, "_example_agent:cc/agent")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "_example_agent", s.Account)
	s, ok, err = SessionByName(owner, db, "cc/owner")
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, s.Account)
	// An update never moves a session to another account.
	s.Findings = "done"
	s.UpdatedAt = now
	require.NoError(t, UpdateSession(ctx, db, s))
	s, _, err = SessionByName(owner, db, "cc/owner")
	require.NoError(t, err)
	require.Empty(t, s.Account)

	require.NoError(t, CreateTask(ctx, db, core.Task{ID: "01K0TASK0000000000000000A1", ProjectSlug: "example-open",
		Title: "agent task", Status: core.TaskOpen, CreatedBy: "cc/agent", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, CreateTask(owner, db, core.Task{ID: "01K0TASK0000000000000000A2", ProjectSlug: "example-open",
		Title: "owner task", Status: core.TaskOpen, CreatedBy: "cc/owner", CreatedAt: now, UpdatedAt: now}))
	tk, err := TaskByID(owner, db, "01K0TASK0000000000000000A1")
	require.NoError(t, err)
	require.Equal(t, "_example_agent", tk.Account)
	require.Empty(t, tk.UpdatedAccount)
	tk, err = TaskByID(owner, db, "01K0TASK0000000000000000A2")
	require.NoError(t, err)
	require.Empty(t, tk.Account)

	// The restricted account claims, then the owner changes, the owner's task.
	_, err = ClaimTask(ctx, db, "01K0TASK0000000000000000A2", "cc/agent", time.Hour, now)
	require.NoError(t, err)
	tk, err = TaskByID(owner, db, "01K0TASK0000000000000000A2")
	require.NoError(t, err)
	require.Empty(t, tk.UpdatedAccount, "a claim writes no text, so it adds no label")
	require.Empty(t, tk.Account, "a claim does not change who created it")
	_, err = ReleaseTask(ctx, db, "01K0TASK0000000000000000A2", "cc/agent", now)
	require.NoError(t, err)
	title := "owner task, renamed"
	_, err = UpdateTask(owner, db, "01K0TASK0000000000000000A2", TaskPatch{Title: &title}, "cc/owner", now)
	require.NoError(t, err)
	tk, err = TaskByID(owner, db, "01K0TASK0000000000000000A2")
	require.NoError(t, err)
	require.Empty(t, tk.UpdatedAccount, "the owner's change is the owner's")

	require.NoError(t, CreateTrial(ctx, db, core.Trial{ID: "01K0TRIAL000000000000000A1", Lab: "lab", Title: "t",
		ProjectSlug: "example-open", SessionID: "01K0SESS0000000000000000A1", CreatedAt: now}))
	trials, err := QueryTrials(owner, db, TrialFilter{Lab: "lab"})
	require.NoError(t, err)
	require.Len(t, trials, 1)
	require.Equal(t, "_example_agent", trials[0].Account)
}

// A restricted account's sessions live in a namespace of their own (SEAL A2):
// the same name, or agent session id, the owner holds is a different session
// for it, and nothing it names reaches the owner's.
func TestSessionNames_RestrictedNamespace(t *testing.T) {
	db := openTestDB(t)
	acct := restrictedCtx(t, "example-open")
	owner := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	mk := func(ctx context.Context, id, name, ext string) error {
		return CreateSession(ctx, db, core.Session{ID: id, Name: name, ProjectSlug: "example-open", Status: core.SessionActive,
			Ambient: ext != "", ExternalClient: map[bool]string{true: "claude-code"}[ext != ""], ExternalSessionID: ext,
			CreatedAt: now, UpdatedAt: now})
	}
	require.NoError(t, mk(owner, "01K0SESS0000000000000000B1", "sess/shared", "ext-shared"))
	require.NoError(t, mk(owner, "01K0SESS0000000000000000B2", "sess/owner-only", ""))
	// The owner's name and agent id are taken; the account's own are not.
	require.NoError(t, mk(acct, "01K0SESS0000000000000000B3", "sess/shared", "ext-shared"))
	// Already qualified: stored once, never twice.
	require.NoError(t, mk(acct, "01K0SESS0000000000000000B4", "_example_agent:sess/second", ""))

	got := func(ctx context.Context, name string) (string, bool) {
		t.Helper()
		s, ok, err := SessionByName(ctx, db, name)
		require.NoError(t, err)
		return s.ID, ok
	}
	id, ok := got(acct, "sess/shared")
	require.True(t, ok)
	require.Equal(t, "01K0SESS0000000000000000B3", id, "the account's own, never the owner's")
	id, ok = got(acct, "_example_agent:sess/shared")
	require.True(t, ok, "its qualified name names it too")
	require.Equal(t, "01K0SESS0000000000000000B3", id)
	id, ok = got(acct, "sess/second")
	require.True(t, ok)
	require.Equal(t, "01K0SESS0000000000000000B4", id)
	_, ok = got(acct, "sess/owner-only")
	require.False(t, ok, "a name only the owner holds reads like a free one")

	id, ok = got(owner, "sess/shared")
	require.True(t, ok)
	require.Equal(t, "01K0SESS0000000000000000B1", id, "the owner's names are read as given")
	id, ok = got(owner, "_example_agent:sess/shared")
	require.True(t, ok, "the owner names the account's by its qualified name")
	require.Equal(t, "01K0SESS0000000000000000B3", id)

	s, ok, err := SessionByID(owner, db, "01K0SESS0000000000000000B3")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "_example_agent:sess/shared", s.Name)
	require.Equal(t, "_example_agent:ext-shared", s.ExternalSessionID, "the agent's session id is namespaced too")
	s, ok, err = AmbientSessionByExternalIdentity(owner, db, "claude-code", "ext-shared")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "01K0SESS0000000000000000B1", s.ID)
	require.Equal(t, "sess/shared", s.Name)
	require.Equal(t, "ext-shared", s.ExternalSessionID)

	// A collision inside the account's namespace is still a collision.
	require.ErrorIs(t, mk(acct, "01K0SESS0000000000000000B5", "sess/shared", ""), ErrSessionNameExists)
}

func TestOpenProjectSlugs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for _, slug := range []string{"example-open", "example-conf", "example-sealed", "example-b"} {
		_, err := EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, SetProjectIsolation(ctx, db, "example-conf", core.IsolationConfidential))
	require.NoError(t, SetProjectIsolation(ctx, db, "example-sealed", core.IsolationSealed))
	got, err := OpenProjectSlugs(ctx, db)
	require.NoError(t, err)
	require.Contains(t, got, "example-open")
	require.Contains(t, got, "example-b")
	require.NotContains(t, got, "example-conf")
	require.NotContains(t, got, "example-sealed")
}
