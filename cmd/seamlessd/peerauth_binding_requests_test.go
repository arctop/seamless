package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/store"
)

// TestPeerAuth_OwnerBinding_RequestsFileMoves is the sibling of
// TestPeerAuth_OwnerBinding_OnlyTheLockHolds with isolation.requests on (plan
// fenced-sessions P4, which lifts Addendum 10 item 2 of the locked-projects
// decisions for an identified conversation). The agent's own conversation --
// the session its SessionStart hook recorded, stamped with its process --
// can no longer read a confidential or sealed project by naming it: from its
// open home, session_start into either files a move request instead of
// binding, and so does a read. The lock still refuses, and asks for its
// lease. A client that names no recorded conversation binds as before: the
// documented limit.
func TestPeerAuth_OwnerBinding_RequestsFileMoves(t *testing.T) {
	r := newGMRig(t, true, true)
	r.plantAll()
	conf, sealed, fin := r.items[gmConf], r.items[gmSealed], r.items[gmFin]
	sess := r.stamp(gmOpen, "cc/limit-agent", gmAgent)
	cli := r.dial(r.socket(gmBridge, gmAgent).URL, "", gmAgent)

	open := gmOK(t, cli, "session_start", map[string]any{"project": gmOpen, "name": "limit-open"})
	require.Equal(t, gmOpen, open["project"], "its own open project binds as before")
	items, isErr := gmCall(t, cli, "memory_read", map[string]any{"name": conf.memName, "project": gmConf})
	require.True(t, isErr, "%v", items)
	gmNoSecretsBut(t, gmText(items), "a read of the confidential project")
	require.Contains(t, items[0], "\nRemedy: move request R-")

	for _, target := range []gmItems{conf, sealed} {
		items, isErr := gmCall(t, cli, "session_start", map[string]any{"project": target.project, "name": "limit-" + target.project})
		require.True(t, isErr, "session_start into %s files a move, never a binding: %v", target.project, items)
		ls := gmLines(t, items)
		require.Len(t, ls, 2, "%v", items)
		require.True(t, strings.HasPrefix(ls[0], "session_start: nothing was bound: project "+target.project+" is "), ls[0])
		require.True(t, strings.HasPrefix(ls[1], "Remedy: move request R-"), ls[1])
		require.Contains(t, ls[1], "Record what belongs in "+gmOpen+" now")
		require.NotContains(t, ls[1], "Touch ID")
		_, found, err := store.SessionByName(r.ctx, r.db, "limit-"+target.project)
		require.NoError(t, err)
		require.False(t, found, "no session was started in %s", target.project)
		items, isErr = gmCall(t, cli, "memory_read", map[string]any{"name": target.memName})
		require.True(t, isErr, "nothing was bound: the name is not found in its home: %v", items)
		gmNoSecretsBut(t, gmText(items), "after session_start "+target.project)
		require.Equal(t, 1, r.moveRows("project_slug = ? AND process_key = ? AND session_id = ? AND status = 'pending'",
			target.project, gmAgent.Key(), sess), "one waiting move per project, anchored to the agent")
	}

	// The lock still refuses, and asks for its lease.
	items, isErr = gmCall(t, cli, "session_start", map[string]any{"project": gmFin, "name": "limit-fin"})
	require.True(t, isErr, "the same move at a lock is refused: %v", items)
	require.Contains(t, items[0], "is locked")
	items, isErr = gmCall(t, cli, "memory_read", map[string]any{"name": fin.memName, "project": gmFin})
	require.True(t, isErr, "%v", items)
	gmNoSecretsBut(t, gmText(items), "a read of the locked project")
	require.Zero(t, r.moveRows("project_slug = ?", gmFin))
	require.Equal(t, 1, r.count(`SELECT COUNT(*) FROM access_approvals WHERE kind = 'request' AND project_slug = ?
		AND process_key = ? AND status = 'pending'`, gmFin, gmAgent.Key()), "the lock's own request, for its lease")

	// The documented limit: a connection whose agent has no recorded
	// conversation is held by what it binds, and binds as before.
	plain := r.dial(r.socket(gmBridge2, gmAgent2).URL, "", access.ProcessToken{})
	gmOK(t, plain, "session_start", map[string]any{"project": gmConf, "name": "limit-plain"})
	items, isErr = gmCall(t, plain, "memory_read", map[string]any{"name": conf.memName})
	require.False(t, isErr, "%v", items)
	require.Contains(t, items[0], conf.secret)
}
