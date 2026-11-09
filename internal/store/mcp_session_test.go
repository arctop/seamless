package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

var mcpSessionNow = time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)

func TestMCPSession_SaveReadTouch(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()

	_, _, ok, err := MCPSessionCapabilities(ctx, db, "mcp-session-one")
	require.NoError(t, err)
	require.False(t, ok, "no row: the client declared nothing a restart must bring back")

	require.NoError(t, SaveMCPSession(ctx, db, "mcp-session-one", `{"elicitation":{"form":{}}}`, mcpSessionNow))
	caps, seen, ok, err := MCPSessionCapabilities(ctx, db, "mcp-session-one")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, `{"elicitation":{"form":{}}}`, caps)
	require.True(t, seen.Equal(mcpSessionNow))

	// Re-saving replaces what the client declared and refreshes the row; only
	// one row names the session.
	later := mcpSessionNow.Add(time.Hour)
	require.NoError(t, SaveMCPSession(ctx, db, "mcp-session-one", `{"elicitation":{"url":{}}}`, later))
	caps, seen, ok, err = MCPSessionCapabilities(ctx, db, "mcp-session-one")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, `{"elicitation":{"url":{}}}`, caps)
	require.True(t, seen.Equal(later))
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcp_sessions`).Scan(&n))
	require.Equal(t, 1, n)

	require.NoError(t, TouchMCPSession(ctx, db, "mcp-session-one", later.Add(time.Hour)))
	_, seen, _, err = MCPSessionCapabilities(ctx, db, "mcp-session-one")
	require.NoError(t, err)
	require.True(t, seen.Equal(later.Add(time.Hour)))
	require.NoError(t, TouchMCPSession(ctx, db, "mcp-session-gone", later), "a pruned row stays gone")

	require.Error(t, SaveMCPSession(ctx, db, "", "{}", mcpSessionNow), "an empty id names no connection")
}

// A row no call has used within the retention goes at the next save; a row
// that a call touched inside it stays, however old it is.
func TestMCPSession_PrunesRowsLongUnused(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	old := mcpSessionNow.Add(-MCPSessionRetention - time.Hour)
	require.NoError(t, SaveMCPSession(ctx, db, "mcp-session-stale", `{"elicitation":{}}`, old))
	require.NoError(t, SaveMCPSession(ctx, db, "mcp-session-used", `{"elicitation":{}}`, old))
	require.NoError(t, TouchMCPSession(ctx, db, "mcp-session-used", mcpSessionNow.Add(-time.Hour)))

	require.NoError(t, SaveMCPSession(ctx, db, "mcp-session-new", `{"elicitation":{}}`, mcpSessionNow))

	for id, want := range map[string]bool{"mcp-session-stale": false, "mcp-session-used": true, "mcp-session-new": true} {
		_, _, ok, err := MCPSessionCapabilities(ctx, db, id)
		require.NoError(t, err)
		require.Equal(t, want, ok, id)
	}
}

// Times round-trip at core.TimeFormat, the column's precision.
func TestMCPSession_TimesAreCanonical(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	require.NoError(t, SaveMCPSession(ctx, db, "mcp-session-x", "{}", mcpSessionNow))
	var stored string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT last_seen_at FROM mcp_sessions`).Scan(&stored))
	require.Equal(t, core.FormatTime(mcpSessionNow), stored)
}
