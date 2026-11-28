package mcp_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// session_end on a Claude Code conversation's ambient row leaves its usage
// owed: the conversation's SessionEnd harvest may come late or never, and the
// daemon reads the turns after this end itself. Any other session owes none.
func TestSessionEnd_LeavesAClaudeCodeConversationsUsageOwed(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	cc := seedAmbientCWD(t, ctx, db, "cc/owed0000", "cc-conv-1", "/work/demo", time.Now().UTC())
	plain := seedAmbient(t, ctx, db, "sess/plain0", "demo")
	for _, id := range []string{cc, plain} {
		end := callJSON(t, ctx, cli, "session_end", map[string]any{"findings": "done", "session_id": id})
		require.Equal(t, "completed", end["status"])
	}

	owed, err := store.OwedUsages(ctx, db)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	require.Equal(t, cc, owed[0].Session.ID)
	require.Equal(t, "cc-conv-1", owed[0].Session.ExternalSessionID)
}
