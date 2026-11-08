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
	s, ok, err := SessionByName(owner, db, "cc/agent")
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
	require.Equal(t, "_example_agent", tk.UpdatedAccount)
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
