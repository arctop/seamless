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
// another session with session= -- and with the switch on is refused both
// (plan fenced-sessions P4): session_start into an open project is no way
// back, a session= handle acts as a session only where the home may read and
// write, and a write into the open project is refused whatever the call
// names. With the switch off, as before.
func TestClearance_ARebindDoesNotLiftTheLabel(t *testing.T) {
	for _, on := range []bool{true, false} {
		r, _ := clNative(t, on)
		ctx := context.Background()
		r.bind(gOps, "cc/ops")
		cli := dialAgent(t, ctx, r.url, clAgent)
		r.ok(cli, "memory_write", map[string]any{"name": "legal-own", "kind": "reference", "description": "d", "body": "b\n"})

		if !on {
			start := r.ok(cli, "session_start", map[string]any{"project": gOps})
			require.Equal(t, gOps, start["project"])
			for _, args := range []map[string]any{
				{"name": "rebound-implicit", "kind": "reference", "description": "d", "body": "b\n"},
				{"name": "rebound-explicit", "kind": "reference", "description": "d", "body": "b\n", "project": gOps},
				{"name": "named-other", "kind": "reference", "description": "d", "body": "b\n", "session": "cc/ops"},
			} {
				r.ok(cli, "memory_write", args)
			}
			continue
		}

		require.Equal(t, "session_start: nothing was bound: this conversation holds content from example-legal, so it does "+
			"not work in project example-ops: a conversation holding a fenced project's content never goes back to an open "+
			"project or the global scope\nRemedy: keep working in project example-legal, your own session cc/legal-agent; "+
			"record there what you meant to record in project example-ops, citing other projects' items by reference, "+
			"each written <project>:<id> with the item's full id", r.refused(cli, "session_start", map[string]any{"project": gOps}))
		require.Equal(t, "session_start: nothing was bound: this conversation holds content from example-legal, so it does "+
			"not work in the global scope: a conversation holding a fenced project's content never goes back to an open "+
			"project or the global scope\nRemedy: keep working in project example-legal, your own session cc/legal-agent; "+
			"record there what you meant to record in the global scope, citing other projects' items by reference, "+
			"each written <project>:<id> with the item's full id", r.refused(cli, "session_start", map[string]any{"project": "global"}))
		// Resuming a session in the open project is the same rebind.
		require.Contains(t, r.refused(cli, "session_start", map[string]any{"name": "cc/ops"}), "does not work in project example-ops")

		// Nothing was bound: an unscoped write lands in the home.
		mem := r.ok(cli, "memory_write", map[string]any{"name": "rebound-implicit", "kind": "reference", "description": "d", "body": "b\n"})
		require.Equal(t, gLegal, mem["project"])
		const rule = "this session holds content from example-legal, so it writes only into example-legal: a write into project example-ops is refused"
		require.Equal(t, "memory_write: "+rule, r.refused(cli, "memory_write", map[string]any{"name": "rebound-explicit",
			"kind": "reference", "description": "d", "body": "b\n", "project": gOps}))

		// session= naming a session in the open project: refused at the handle.
		require.Equal(t, "memory_write: session cc/ops works in project example-ops, and this conversation works in project "+
			"example-legal: it holds content from example-legal, which may not be written into project example-ops, so it "+
			"may not act as that session\nRemedy: leave session= off to act as your own session cc/legal-agent in project "+
			"example-legal", r.refused(cli, "memory_write", map[string]any{"name": "named-other", "kind": "reference",
			"description": "d", "body": "b\n", "session": "cc/ops"}))
		require.Contains(t, r.refused(cli, "notes_create", map[string]any{"title": "rebound note", "body": "b", "session": "cc/ops"}),
			"so it may not act as that session")
		require.Zero(t, r.count(`SELECT COUNT(*) FROM memories_index WHERE project = ?`, gOps))
		require.Zero(t, r.count(`SELECT COUNT(*) FROM notes_index WHERE project = ?`, gOps))
		// The global scope: never while the label holds anything.
		require.Equal(t, "memory_write: this session holds content from example-legal, so it writes only into "+
			"example-legal: a write into the global scope is refused", r.refused(cli, "memory_write", map[string]any{
			"name": "to-global", "kind": "reference", "description": "d", "body": "b\n", "project": "global"}))
		// Its own home session, named by handle, is itself.
		r.ok(cli, "memory_write", map[string]any{"name": "back-home", "kind": "reference", "description": "d", "body": "b\n",
			"session": "cc/legal-agent"})
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
		"(task details, memories, notes, findings), refer to example-finance's items by reference, each written "+
		"example-finance:<id> with the item's full id, and leave out example-finance's confidential details.", items[1])

	r.ok(cli, "memory_write", map[string]any{"name": "into-fin", "kind": "reference", "description": "d", "body": "b\n",
		"project": gFin})
	r.ok(cli, "notes_append", map[string]any{"id": r.finNote, "body": "reconciled from legal"})
	r.ok(cli, "memory_write", map[string]any{"name": "legal-own", "kind": "reference", "description": "d", "body": "b\n"})
	for where, project := range map[string]string{"project example-ops": gOps, "the global scope": "global",
		"project example-vault": gVault} {
		text := r.refused(cli, "memory_write", map[string]any{"name": "out", "kind": "reference", "description": "d",
			"body": "b\n", "project": project})
		rule, remedy, _ := strings.Cut(text, "\n")
		require.True(t, strings.HasSuffix(rule, "a write into "+where+" is refused"), text)
		if project == gVault {
			// A sealed project is reached only by moving in: the refused write
			// asks for that move (P4), and the move's carry names what it holds.
			require.True(t, strings.HasPrefix(remedy, "Remedy: move request R-"), remedy)
			continue
		}
		require.Empty(t, remedy, "into an open project or the global scope there is no way back: nothing to ask")
	}
	pending, err := store.PendingAccessApprovals(context.Background(), r.db, r.clock())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, store.AccessKindMove, pending[0].Kind)
	require.Equal(t, gVault, pending[0].Project)
	require.Equal(t, []string{gFin, gLegal, gVault}, pending[0].Carry.Holds)
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
