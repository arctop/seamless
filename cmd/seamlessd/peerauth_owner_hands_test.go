package main

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The owner's own decisions are refused on the socket from a process an agent
// started, and from one whose chain cannot be read. The same calls still work
// from the owner's plain terminal and over TCP with the key.
func TestPeerAuth_OwnerDecisions_RefusedFromAnAgent(t *testing.T) {
	ctx := context.Background()
	d := newLMDaemon(t)
	d.setup(t)

	post := func(srv, path string) (int, string) {
		resp, err := http.Post(srv+path, "application/x-www-form-urlencoded", nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(body)
	}
	state := func() core.Isolation {
		s, err := store.IsolationOf(ctx, d.db, lmConf)
		require.NoError(t, err)
		return s
	}
	const loosen = "/console/projects/" + lmConf + "/isolation?format=json&state=open"
	const release = "/console/held/no-such-hold/release?format=json"

	refused := []struct {
		who   string
		chain []access.ProcessToken
	}{
		{"an agent's bridge", []access.ProcessToken{lmBridgeA, lmAgentA}},
		{"a script under an agent's shell", []access.ProcessToken{lmScriptA, lmShellA, lmAgentA}},
		{"a connection whose chain the kernel did not give", nil},
	}
	for _, c := range refused {
		t.Run(c.who, func(t *testing.T) {
			srv := d.peer(t, lmUIDOwner, c.chain...)
			for _, path := range []string{loosen, release} {
				status, body := post(srv.URL, path)
				require.Equal(t, http.StatusForbidden, status, "%s: %s", path, body)
				require.Contains(t, body, "the owner's decision")
			}
			require.Equal(t, core.IsolationConfidential, state(), "the fence stays up")
		})
	}

	// The owner's own terminal: seam under a shell under the terminal, no agent.
	t.Run("the owner's terminal", func(t *testing.T) {
		srv := d.peer(t, lmUIDOwner, lmBridgeA, lmShellA, lmTerm)
		status, body := post(srv.URL, release)
		require.NotEqual(t, http.StatusForbidden, status, "%s", body)
		status, body = post(srv.URL, loosen)
		require.Equal(t, http.StatusOK, status, "%s", body)
		require.Equal(t, core.IsolationOpen, state())
	})
}

// A process outside a session's tree cannot end the session while it holds a
// read grant, which would revoke the grant. Its own tree still can.
func TestPeerAuth_SessionEndHook_LeavesAnotherTreesGrant(t *testing.T) {
	r := newGMRig(t, true, true)
	r.plantAll()
	conf := r.items[gmConf]
	r.stamp(gmOpen, "cc/open-agent", gmAgent)
	agent := r.dial(r.socket(gmBridge, gmAgent).URL, "", gmAgent)
	items, _ := gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
	short := gmShort.FindString(gmText(items))
	require.NotEmpty(t, short)
	r.approveDaemon(short)
	_, isErr := gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
	require.False(t, isErr, "the grant reads")

	end := map[string]any{"session_id": "cc/open-agent-ext", "cwd": "/work/" + gmOpen, "hook_event_name": "SessionEnd", "reason": "other"}
	const hook = "session-end?client=claude-code&host=" + gmHost
	sibling := r.socket(gmSibling, gmSibShell)
	status, body := r.hook(sibling.URL, "", hook, end)
	require.Equal(t, http.StatusOK, status, "the hook still answers: %s", body)
	_, isErr = gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
	require.False(t, isErr, "a process outside the tree did not end the grant")

	status, body = r.hook(r.socket(gmBridge, gmAgent).URL, "", hook, end)
	require.Equal(t, http.StatusOK, status, "%s", body)
	_, isErr = gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
	require.True(t, isErr, "its own tree ends the session, and the grant with it")
}
