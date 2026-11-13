package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/store"
)

// access_request's write (isolation.requests): it asks for the kind the
// conversation's home and the project call for -- from an open project a move
// into the project, answered at once with what to record first -- and one
// waiting request per asker and project, whichever kind it is.

func TestAccessRequest_WriteFromAnOpenHomeAsksForAMove(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: dlgLong, grants: true})
	sess := d.session("cc/ops", lkOps)
	agent := d.dialOwner(d.agentSocket(), nil, nil)

	out, _ := d.ok(agent, mcpserver.AccessRequestTool, map[string]any{"project": dlgLegal, "write": true,
		"reason": "fix the contract template"})
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res), out)
	require.Equal(t, store.AccessKindMove, res["kind"])
	require.Equal(t, lkOps, res["from"])
	require.Equal(t, "pending", res["status"])
	require.Equal(t, true, res["filed"])
	require.Nil(t, res["access"], "a move asks no grant's access")
	require.True(t, strings.HasPrefix(res["next"].(string), "Record what belongs in example-ops now: once the owner approves the "+
		"move, this conversation can no longer write there. "), res["next"])
	pending := d.pending()
	require.Len(t, pending, 1)
	require.Equal(t, store.AccessKindMove, pending[0].Kind)
	require.Equal(t, sess, pending[0].SessionID)
	require.Equal(t, lkAgent.Key(), pending[0].ProcessKey)
	require.Equal(t, "fix the contract template", pending[0].Reason)

	// One waiting request per asker and project: asked again, with write or
	// without, the move is found.
	for _, args := range []map[string]any{{"project": dlgLegal, "write": true}, {"project": dlgLegal}} {
		out, _ = d.ok(agent, mcpserver.AccessRequestTool, args)
		res = map[string]any{}
		require.NoError(t, json.Unmarshal([]byte(out), &res), out)
		require.Equal(t, pending[0].ID, res["id"], "%v", args)
		require.Equal(t, false, res["filed"], "%v", args)
	}
	require.Len(t, d.pending(), 1)

	// The tool.call event keeps write and never the reason.
	ev := findToolCall(t, d.db, mcpserver.AccessRequestTool)
	require.NotContains(t, ev.Payload["args"], "fix the contract")
}

// write on the project a conversation already works in, under a lock, is the
// lock's lease to ask for: it reads and writes already.
func TestAccessRequest_WriteOnTheHomeIsTheLocksRequest(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: dlgLong, grants: true})
	d.session("cc/fin", lkFin)
	agent := d.dialOwner(d.agentSocket(), nil, nil)
	out, _ := d.ok(agent, mcpserver.AccessRequestTool, map[string]any{"project": lkFin, "write": true})
	require.Contains(t, out, `"kind":"request"`)
	require.Contains(t, d.refused(agent, mcpserver.AccessRequestTool, map[string]any{"project": lkOps, "write": true}),
		"that project is neither confidential nor sealed: nothing to request")

	// Without read grants write changes nothing: the lock's request.
	off := newDialogRig(t, dialogOpts{wait: dlgLong})
	off.session("cc/ops", lkOps)
	offAgent := off.dialOwner(off.agentSocket(), nil, nil)
	out, _ = off.ok(offAgent, mcpserver.AccessRequestTool, map[string]any{"project": lkFin, "write": true})
	require.Contains(t, out, `"kind":"request"`)
}

// The tool declares write as a boolean, and the validator holds it to one.
func TestAccessRequest_WriteIsDeclared(t *testing.T) {
	d := newDialogRig(t, dialogOpts{wait: dlgLong, grants: true})
	d.session("cc/ops", lkOps)
	agent := d.dialOwner(d.agentSocket(), nil, nil)
	tools, err := agent.ListTools(context.Background(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	var found bool
	for _, tool := range tools.Tools {
		if tool.Name != mcpserver.AccessRequestTool {
			continue
		}
		found = true
		prop, ok := tool.InputSchema.Properties["write"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "boolean", prop["type"])
	}
	require.True(t, found)
	require.Contains(t, d.refused(agent, mcpserver.AccessRequestTool, map[string]any{"project": dlgLegal, "write": "maybe"}), "write")
	require.Empty(t, d.pending())
}
