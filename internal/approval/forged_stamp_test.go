package approval

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// A restricted account's SessionStart hook may stamp its own live session
// with any agent-process identity, the owner's agent's included. The owner's
// requests find the session the claimed process stands for among the
// sessions the owner's own agents started, as the MCP process binding does:
// such a row neither makes the owner's own session ambiguous (two rows, no
// session, and for an agent run by node no anchor either) nor, as the sole
// row, files the owner's request under the account's session.

// forgedSession records a live ambient session a restricted account's
// hook started and stamped with proc.
func forgedSession(t *testing.T, f *reqFixture, project string, proc access.ProcessToken) string {
	t.Helper()
	ctx := context.Background()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(ctx, f.db, core.Session{
		ID: id, Name: "cc/forged", ProjectSlug: project, Status: core.SessionActive, Ambient: true,
		ExternalSessionID: "forged-x", ExternalClient: "claude-code", CWD: "/work/" + project,
		Account: "_example_agent", CreatedAt: f.now, UpdatedAt: f.now,
	}))
	require.NoError(t, store.SetSessionAgentProcess(ctx, f.db, id, proc.AgentID()))
	return id
}

// nodeAgent names the agent the way the kernel names Claude Code installed
// with npm: node, which anchors only when a session is stamped with it.
func nodeAgent(f *reqFixture) {
	f.svc.cfg.Programs = func(t access.ProcessToken) string {
		if t == agent {
			return "/opt/homebrew/bin/node"
		}
		return ""
	}
}

func TestOwnerRequests_CountOnlyTheOwnersStampedSessions(t *testing.T) {
	t.Run("a lock's request keeps its session and its anchor", func(t *testing.T) {
		f := newReqFixture(t)
		nodeAgent(f)
		mine := f.session(t, "cc/fin", "/work/ops", agent)
		forgedSession(t, f, openOps, agent)
		a, err := f.svc.Request(context.Background(), RequestInput{Project: lockFin,
			View: f.view(t, bridge, agent, shell), AgentProcess: agent.AgentID()})
		require.NoError(t, err, "the owner's own stamped session anchors node")
		require.Equal(t, mine, a.SessionID)
		require.Equal(t, "cc/fin", a.SessionName)
		require.Equal(t, agent.Key(), a.ProcessKey)
	})

	t.Run("a lock's request is never filed under the account's session", func(t *testing.T) {
		f := newReqFixture(t)
		forgedSession(t, f, openOps, agent)
		a, err := f.svc.Request(context.Background(), RequestInput{Project: lockFin,
			View: f.view(t, bridge, agent, shell), AgentProcess: agent.AgentID()})
		require.NoError(t, err, "the claimed process still anchors the request")
		require.Equal(t, agent.Key(), a.ProcessKey)
		require.Empty(t, a.SessionID, "no session of the owner's stands for the process")
		require.Empty(t, a.SessionName)
	})

	t.Run("a grant request keeps its session", func(t *testing.T) {
		f := newGrantFixture(t)
		g := NewGrants(GrantsConfig{Approvals: f.svc})
		mine := f.session(t, "cc/ops", "/work/ops", agent)
		forgedSession(t, f, openOps, agent)
		a, err := g.RequestGrant(access.WithView(context.Background(), f.view(t, bridge, agent, shell)),
			GrantInput{Project: confHR, AgentProcess: agent.AgentID()})
		require.NoError(t, err)
		require.Equal(t, mine, a.SessionID)
		require.Equal(t, openOps, a.FromProject)
	})

	t.Run("an advisory grant request keeps its session", func(t *testing.T) {
		f := newGrantFixture(t)
		// A row a restricted account left from when peer_auth was on.
		mine := f.session(t, "cc/ops", "/work/ops", agent)
		forgedSession(t, f, openOps, agent)
		a, err := f.advisory().RequestGrant(context.Background(), GrantInput{Project: lockFin, AgentProcess: agent.AgentID()})
		require.NoError(t, err)
		require.Equal(t, mine, a.SessionID)
	})
}
