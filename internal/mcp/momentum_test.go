package mcp_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// firstReuseEvents counts the memory.first_reuse marks for one memory id.
func firstReuseEvents(t *testing.T, db *sql.DB, itemID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE kind = ? AND item_id = ?`,
		string(core.EventMemoryFirstReuse), itemID).Scan(&n))
	return n
}

// The first-reuse moment: latched exactly once per memory ever, and only for a
// reader that is not the writer. It needs no optional feature.
func TestMemoryRead_FirstReuseMark(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)

	writer := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, writer, "session_start", map[string]any{"cwd": "/work/demo"})
	w := callJSON(t, ctx, writer, "memory_write", map[string]any{
		"name": "payoff-mem", "kind": "gotcha", "description": "d", "body": "the lesson",
	})
	memID, _ := w["id"].(string)
	require.NotEmpty(t, memID)

	// The writer re-reading its own memory is not reuse.
	callJSON(t, ctx, writer, "memory_read", map[string]any{"name": "payoff-mem"})
	require.Zero(t, firstReuseEvents(t, db, memID))

	// A different session reading it is -- once, ever.
	reader := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, reader, "session_start", map[string]any{"cwd": "/work/demo", "name": "sess/reader-one"})
	callJSON(t, ctx, reader, "memory_read", map[string]any{"name": "payoff-mem"})
	require.Equal(t, 1, firstReuseEvents(t, db, memID))

	callJSON(t, ctx, reader, "memory_read", map[string]any{"name": "payoff-mem"})
	third := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, third, "session_start", map[string]any{"cwd": "/work/demo", "name": "sess/reader-two"})
	callJSON(t, ctx, third, "memory_read", map[string]any{"name": "payoff-mem"})
	require.Equal(t, 1, firstReuseEvents(t, db, memID), "the moment is minted once, ever")

	// The payload carries what the ledger line and the toast need.
	var payload string
	require.NoError(t, db.QueryRow(
		`SELECT payload FROM events WHERE kind = ? AND item_id = ?`,
		string(core.EventMemoryFirstReuse), memID).Scan(&payload))
	require.Contains(t, payload, `"name":"payoff-mem"`)
	require.Contains(t, payload, `"kind":"gotcha"`)
	require.Contains(t, payload, `"reader":"sess/reader-one"`)
}

// A memory another session read before first reuse was recorded at all (an
// install from before momentum was core: the read is in the log, no mark is)
// was reused then, so the next foreign read is not its first and mints
// nothing -- the claim "paid off for the first time" stays true.
func TestMemoryRead_FirstReuseJudgedFromTheReadHistory(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)

	writer := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, writer, "session_start", map[string]any{"cwd": "/work/demo"})
	w := callJSON(t, ctx, writer, "memory_write", map[string]any{
		"name": "old-mem", "kind": "gotcha", "description": "d", "body": "the lesson",
	})
	memID, _ := w["id"].(string)
	require.NotEmpty(t, memID)

	// The writer's own read earlier is no reuse; another session's is.
	callJSON(t, ctx, writer, "memory_read", map[string]any{"name": "old-mem"})
	_, err := db.ExecContext(ctx, `
		INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES ('01OLDREAD0000000000000000A', '2026-01-02T03:04:05Z', ?, '01EARLIERREADERSESSION0000', 'demo', ?, '{}')`,
		string(core.EventMemoryRead), memID)
	require.NoError(t, err)

	reader := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, reader, "session_start", map[string]any{"cwd": "/work/demo", "name": "sess/late-reader"})
	callJSON(t, ctx, reader, "memory_read", map[string]any{"name": "old-mem"})
	require.Zero(t, firstReuseEvents(t, db, memID), "the reuse happened before: no first-reuse claim now")

	var reads int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND item_id = ?`,
		string(core.EventMemoryRead), memID).Scan(&reads))
	require.Equal(t, 3, reads, "every read is still recorded")
}
