package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPeerAuth_OwnerBinding_OnlyTheLockHolds pins what the Privacy page and the
// guide promise a dedicated-account install. A session's project is the agent's
// own word, so an agent running as the owner can name a confidential or sealed
// project and read it; the fence keeps a careful agent out, not one set on it.
// The lock is the wall: it asks the kernel who is calling, and the same agent,
// the same way, is refused there.
func TestPeerAuth_OwnerBinding_OnlyTheLockHolds(t *testing.T) {
	d := newLMDaemon(t)
	w := d.setup(t)

	agent, bridge := lmTok(7000), lmTok(7001)
	srv := d.peer(t, lmUIDOwner, bridge, agent)
	cli := paClientHeaders(t, srv.URL, "", lmAgentHeaders(agent))

	lmCallOK(t, cli, "session_start", map[string]any{"project": lmOpen, "name": "limit-open"})
	text, isErr := paCall(t, cli, "memory_read", map[string]any{"name": w.conf.memName, "project": lmConf})
	require.True(t, isErr, "bound elsewhere, the fence refuses: %s", text)
	require.NotContains(t, text, lmConfSecret)

	lmCallOK(t, cli, "session_start", map[string]any{"project": lmConf, "name": "limit-conf"})
	text, isErr = paCall(t, cli, "memory_read", map[string]any{"name": w.conf.memName})
	require.False(t, isErr, "%s", text)
	require.Contains(t, text, lmConfSecret, "naming the project is all a confidential fence asks of an owner-account agent")

	lmCallOK(t, cli, "session_start", map[string]any{"project": lmSealed, "name": "limit-sealed"})
	text, isErr = paCall(t, cli, "memory_read", map[string]any{"name": w.sealed.memName})
	require.False(t, isErr, "%s", text)
	require.Contains(t, text, lmSealedSecret, "a sealed project's fence is the same self-reported binding")

	text, isErr = paCall(t, cli, "session_start", map[string]any{"project": lmFin, "name": "limit-fin"})
	require.True(t, isErr, "the same move at a lock is refused: %s", text)
	text, isErr = paCall(t, cli, "memory_read", map[string]any{"name": w.fin.memName, "project": lmFin})
	require.True(t, isErr, "%s", text)
	require.NotContains(t, text, lmFinSecret)
}
