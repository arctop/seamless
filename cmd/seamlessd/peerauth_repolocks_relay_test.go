//go:build darwin || linux

package main

// The repo locks' relay row of the peer_auth matrix (plan repo-locks, step
// RL3), apart from the other rows because the relay rig builds on darwin and
// linux only (peerauth_relay_helpers_test.go).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// A relay's restricted account (peer_auth.relays.<relay>.restricted) is
// judged as a local one: it holds a lock listing a project open to it, and a
// lock listing only a fenced project is missing to it -- its reach is the open
// projects alone (a plain entry), here over the real TLS path of a paired relay.
func TestPeerAuthMatrix_RepoLocksRelay(t *testing.T) {
	ctx := context.Background()
	d := newRelayvDaemon(t)
	require.NoError(t, store.SetFeaturesConfig(ctx, d.db, config.Features{RepoLocks: true}))
	_, laptopID := d.pair(t, relayvLaptop)
	owner := d.mcpVia(t, nil, relayvKey, nil)
	open := lkLockOf(t, paCallOK(t, owner, "locks_declare", map[string]any{"repo_root": "/work/app",
		"repo_origin": "https://example.com/team/app.git", "projects": []any{relayvOpen}}))
	// A lock listing only the confidential project, as the owner declares it
	// under a lease: here straight in the store.
	finAliases, err := store.RepoLockAliasesFor(store.CWDIdentity{Host: relayvHost, RepoRoot: "/work/fin",
		Origin: "https://example.com/team/fin.git"}, relayvHost)
	require.NoError(t, err)
	fin, err := store.DeclareRepoLock(ctx, d.db, store.RepoLockDeclaration{Name: core.DefaultRepoLockName, Aliases: finAliases,
		Projects: []string{relayvFin}, State: core.RepoLockActive, RecordAliases: true}, time.Now().UTC())
	require.NoError(t, err)

	caller := relayvCaller{uid: relay2RestrictedUID, chain: []access.ProcessToken{relayvTok(2710), relayvTok(2700)}}
	agent := d.mcpVia(t, laptopID, "", caller.relay2Headers(map[int32]string{2700: relayvClaude}))
	paCallOK(t, agent, "session_start", map[string]any{"project": relayvOpen, "name": "lk-relay-agent"})

	seen := lkLocksOf(t, paCallOK(t, agent, "locks_list", map[string]any{}))
	require.Len(t, seen, 1)
	require.Contains(t, seen, open["id"].(string))
	require.Equal(t, true, paCallOK(t, agent, "locks_list", map[string]any{})["caller_bound"])
	real, refused := paCall(t, agent, "locks_acquire", map[string]any{"id": fin.Lock.ID})
	require.True(t, refused)
	ghost, _ := paCall(t, agent, "locks_acquire", map[string]any{"id": lkGhostID})
	require.Equal(t, strings.ReplaceAll(ghost, lkGhostID, fin.Lock.ID), real, "a fenced project's lock is missing to it")

	got := paCallOK(t, agent, "locks_acquire", map[string]any{"id": open["id"]})
	require.Equal(t, true, got["acquired"])
	require.Equal(t, true, lkLockOf(t, got)["held_by_caller"])
	var account string
	require.NoError(t, d.db.QueryRowContext(ctx, `SELECT json_extract(payload, '$.account') FROM events WHERE kind = ? AND item_id = ?
		AND json_extract(payload, '$.action') = 'acquired'`, string(core.EventRepoLockChanged), open["id"]).Scan(&account))
	require.Equal(t, relay2RestrictedLabel, account, "the event names the relay's account")
	require.Equal(t, true, paCallOK(t, agent, "locks_release", map[string]any{"id": open["id"]})["released"])
}
