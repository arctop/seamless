package mcp_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/store"
)

// The clearance rule at the tool surface (isolation.requests): every call
// carries the state of the conversation it speaks for -- found from the
// agent-process identity, never the connection's binding -- and every write
// must be cleared for everything that conversation holds.

const clAgent = "4242.99"

// clNative is the grant rig with a Claude Code conversation native to the
// confidential example-legal: its ambient row, stamped with its process, and
// a connection that names the process, as the agent's MCP bridge does.
func clNative(t *testing.T, on bool) (*grantRig, string) {
	t.Helper()
	r := newGrantRig(t, on)
	ambID := seedAgentAmbient(t, context.Background(), r.db, "cc/legal-agent", gLegal, "/work/legal", clAgent)
	return r, ambID
}

// Leak path 3: rebinding is free. A conversation native to a confidential
// project binds itself to an open one -- with session_start or by naming
// another session with session= -- and is refused writing there with the
// switch on, whatever the binding says; with it off, as before.
func TestClearance_ARebindDoesNotLiftTheLabel(t *testing.T) {
	for _, on := range []bool{true, false} {
		r, _ := clNative(t, on)
		ctx := context.Background()
		r.bind(gOps, "cc/ops")
		cli := dialAgent(t, ctx, r.url, clAgent)
		r.ok(cli, "memory_write", map[string]any{"name": "legal-own", "kind": "reference", "description": "d", "body": "b\n"})

		const rule = "this session holds content from example-legal, so it writes only into example-legal: a write into project example-ops is refused"
		start := r.ok(cli, "session_start", map[string]any{"project": gOps})
		require.Equal(t, gOps, start["project"])
		cases := []struct {
			tool string
			args map[string]any
		}{
			{"memory_write", map[string]any{"name": "rebound-implicit", "kind": "reference", "description": "d", "body": "b\n"}},
			{"memory_write", map[string]any{"name": "rebound-explicit", "kind": "reference", "description": "d", "body": "b\n", "project": gOps}},
			{"notes_create", map[string]any{"title": "rebound note", "body": "b"}},
			{"tasks_add", map[string]any{"title": "rebound task"}},
		}
		for _, c := range cases {
			items, isErr := r.call(cli, c.tool, c.args)
			if !on {
				require.False(t, isErr, "off: %s lands as before: %v", c.tool, items)
				continue
			}
			require.True(t, isErr, "%s: %v", c.tool, items)
			require.Equal(t, c.tool+": "+rule, items[0])
		}
		items, isErr := r.call(cli, "memory_write", map[string]any{"name": "named-other", "kind": "reference",
			"description": "d", "body": "b\n", "session": "cc/ops"})
		require.Equal(t, on, isErr, "session= naming another session: %v", items)
		if on {
			require.Equal(t, "memory_write: "+rule, items[0])
			require.Zero(t, r.count(`SELECT COUNT(*) FROM memories_index WHERE project = ?`, gOps))
			// The global scope: never while the label holds anything.
			items, isErr = r.call(cli, "memory_write", map[string]any{"name": "to-global", "kind": "reference",
				"description": "d", "body": "b\n", "project": "global"})
			require.True(t, isErr)
			require.Equal(t, "memory_write: this session holds content from example-legal, so it writes only into "+
				"example-legal: a write into the global scope is refused", items[0])
		}
		// Into its home, from anywhere it is bound, it writes.
		r.ok(cli, "memory_write", map[string]any{"name": "back-home", "kind": "reference", "description": "d", "body": "b\n",
			"project": gLegal})
	}
}

// A client that names no agent process is held by its bound session, as
// before migration 039: the documented limit.
func TestClearance_NoIdentityKeepsTheSessionHolder(t *testing.T) {
	r, _ := clNative(t, true)
	ops := r.bind(gOps, "cc/ops")
	r.ok(ops, "memory_write", map[string]any{"name": "ops-own", "kind": "reference", "description": "d", "body": "b\n"})
	r.ok(ops, "memory_write", map[string]any{"name": "ops-global", "kind": "reference", "description": "d", "body": "b\n",
		"project": "global"})
}

// A read-and-write grant between confidential projects: the conversation
// reads the target, writes it as the target's own sessions do, and writes
// nowhere its label is not cleared.
func TestClearance_WriteGrantAtTheSurface(t *testing.T) {
	r, ambID := clNative(t, true)
	ctx := context.Background()
	a, err := store.CreateGrantRequest(ctx, r.db, store.GrantRequest{Project: gFin, From: gLegal, Lease: time.Hour,
		TTL: 10 * time.Minute, SessionID: ambID, ConversationClient: "claude-code", ConversationID: "cc/legal-agent-full",
		Access: store.GrantAccessWrite}, r.clock())
	require.NoError(t, err)
	g, err := store.GrantAccessApprove(ctx, r.db, a.ID, a.Challenge, "owner", 0, store.GrantAccessWrite, r.clock())
	require.NoError(t, err)

	cli := dialAgent(t, ctx, r.url, clAgent)
	items, isErr := r.call(cli, "notes_read", map[string]any{"id": r.finNote})
	require.False(t, isErr, "a write grant reads: %v", items)
	require.Len(t, items, 2)
	require.Contains(t, items[0], "quarterly ledger reconciliation notes")
	require.Equal(t, "Read and write access to example-finance was approved until "+access.Clock(g.ExpiresAt)+
		". This session now writes only into example-finance, example-legal. When you document this work in example-legal "+
		"(task details, memories, notes, findings), refer to example-finance's items by reference -- project slug plus "+
		"the item's name or id -- and leave out example-finance's confidential details.", items[1])

	r.ok(cli, "memory_write", map[string]any{"name": "into-fin", "kind": "reference", "description": "d", "body": "b\n",
		"project": gFin})
	r.ok(cli, "notes_append", map[string]any{"id": r.finNote, "body": "reconciled from legal"})
	r.ok(cli, "memory_write", map[string]any{"name": "legal-own", "kind": "reference", "description": "d", "body": "b\n"})
	for where, project := range map[string]string{"project example-ops": gOps, "the global scope": "global",
		"project example-vault": gVault} {
		text := r.refused(cli, "memory_write", map[string]any{"name": "out", "kind": "reference", "description": "d",
			"body": "b\n", "project": project})
		require.True(t, strings.HasSuffix(text, "a write into "+where+" is refused"), text)
	}
}

// With isolation.requests off nothing in ctx changes: the matrix alone, word
// for word, for the same conversation.
func TestClearance_OffIsTheMatrix(t *testing.T) {
	r, _ := clNative(t, false)
	cli := dialAgent(t, context.Background(), r.url, clAgent)
	text := r.refused(cli, "memory_write", map[string]any{"name": "out", "kind": "reference", "description": "d",
		"body": "b\n", "project": gOps})
	require.Equal(t, "memory_write: this session is bound to confidential project example-legal: writes outside it are disabled", text)
}
