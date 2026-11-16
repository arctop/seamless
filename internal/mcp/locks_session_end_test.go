package mcp_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Ending a session frees the repo locks it holds, as it returns its claims;
// the answer counts them only when there were some.
func TestSessionEnd_ReleasesTheSessionsRepoLocks(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	start := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	sessID, _ := start["session_id"].(string)
	require.NotEmpty(t, sessID)

	aliases, err := store.RepoLockAliasesFor(store.CWDIdentity{Host: "h", RepoRoot: "/work/demo"}, "h")
	require.NoError(t, err)
	now := time.Now().UTC()
	decl, err := store.DeclareRepoLock(ctx, db, store.RepoLockDeclaration{
		Name: "main", Aliases: aliases, State: core.RepoLockActive, RecordAliases: true,
	}, now)
	require.NoError(t, err)
	_, err = store.AcquireRepoLock(ctx, db, decl.Lock.ID, sessID, time.Hour, now)
	require.NoError(t, err)

	end := callJSON(t, ctx, cli, "session_end", map[string]any{"findings": "landed"})
	require.EqualValues(t, 1, end["locks_released"])
	require.EqualValues(t, 0, end["claims_released"])
	lock, _, err := store.RepoLockByID(ctx, db, decl.Lock.ID)
	require.NoError(t, err)
	require.Empty(t, lock.Holder, "the lock is free again")
	pending, err := store.PendingClaimNotices(ctx, db, sessID)
	require.NoError(t, err)
	require.Empty(t, pending, "a session that ends itself loses nothing it does not know about")

	// A session that held no lock answers as before: no locks_released key.
	cli2 := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli2, "session_start", map[string]any{"cwd": "/work/demo"})
	end2 := callJSON(t, ctx, cli2, "session_end", map[string]any{"findings": "nothing held"})
	_, has := end2["locks_released"]
	require.False(t, has)
}
