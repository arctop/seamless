package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// AmbientSessionsOf lists one principal's conversations -- their ambient rows,
// oldest first -- and never a session another principal started, nor an
// explicit session linked to a conversation.
func TestAmbientSessionsOf_OnePrincipalsConversations(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	day := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	later := ledgerAmbient(t, db, "01BACKFILL0000000000000002", "claude-code", "bf-2", "example-ops", day.Add(time.Hour))
	earlier := ledgerAmbient(t, db, "01BACKFILL0000000000000001", "codex", "bf-1", "example-ops", day)
	require.NoError(t, CreateSession(ctx, db, core.Session{ID: "01BACKFILL0000000000000003", Name: "explicit-bf",
		ProjectSlug: "example-ops", Status: core.SessionActive, ExternalClient: "claude-code", ExternalSessionID: "bf-2",
		CreatedAt: day, UpdatedAt: day}))
	require.NoError(t, CreateSession(ctx, db, core.Session{ID: "01BACKFILL0000000000000004", Name: "cc/bf-agent",
		ProjectSlug: "example-ops", Status: core.SessionCompleted, Ambient: true, ExternalClient: "claude-code",
		ExternalSessionID: "bf-agent", Account: "example-agent", CreatedAt: day, UpdatedAt: day}))

	owners, err := AmbientSessionsOf(ctx, db, "")
	require.NoError(t, err)
	require.Len(t, owners, 2)
	require.Equal(t, []string{earlier.ID, later.ID}, []string{owners[0].ID, owners[1].ID}, "oldest first, ambient rows only")

	agents, err := AmbientSessionsOf(ctx, db, "example-agent")
	require.NoError(t, err)
	require.Len(t, agents, 1)
	require.Equal(t, "example-agent", agents[0].Account)
	require.Equal(t, "example-agent:bf-agent", agents[0].ExternalSessionID)
}

// ConversationUsageOf says whether the ledger already holds exactly the rows a
// backfill would write: after ReplaceConversationUsage wrote them it does, and
// any difference -- a count, a minute, a missing or extra row, totals from
// before the ledger -- says it does not. It writes nothing.
func TestConversationUsageOf_HoldsExactlyTheRows(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	day := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	s := ledgerAmbient(t, db, "01BACKFILL0000000000000011", "claude-code", "bf-hold", "example-ops", day)
	rows := []core.UsageBucket{
		ledgerRow(day, "claude-x", core.UsageThreadMain, 100, 10, 2, 0b11),
		ledgerRow(day, "claude-x", core.UsageThreadSubagent, 40, 4, 1, 0b100),
		ledgerRow(day.Add(15*time.Minute), "claude-y", core.UsageThreadMain, 7, 3, 1, 1<<14),
	}

	_, found, err := ConversationUsageOf(ctx, db, "claude-code", "bf-nobody", rows)
	require.NoError(t, err)
	require.False(t, found, "no conversation of that id")
	_, found, err = ConversationUsageOf(ctx, db, "", "bf-hold", rows)
	require.NoError(t, err)
	require.False(t, found, "an incomplete identity names none")

	got, found, err := ConversationUsageOf(ctx, db, "claude-code", "bf-hold", rows)
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, got.Holds, "an empty ledger does not hold the rows")
	require.True(t, got.Totals.Empty())

	_, err = ReplaceConversationUsage(ctx, db, "claude-code", "bf-hold", rows, day.Add(time.Hour))
	require.NoError(t, err)
	got, found, err = ConversationUsageOf(ctx, db, "claude-code", "bf-hold", rows)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, got.Holds, "the rows it wrote, in any order")
	require.Equal(t, core.SumUsage(rows), got.Totals)
	reversed := []core.UsageBucket{rows[2], rows[1], rows[0]}
	got, _, err = ConversationUsageOf(ctx, db, "claude-code", "bf-hold", reversed)
	require.NoError(t, err)
	require.True(t, got.Holds, "order does not matter")

	for name, other := range map[string][]core.UsageBucket{
		"a count":     {rows[0], rows[1], ledgerRow(day.Add(15*time.Minute), "claude-y", core.UsageThreadMain, 7, 4, 1, 1<<14)},
		"a minute":    {rows[0], rows[1], ledgerRow(day.Add(15*time.Minute), "claude-y", core.UsageThreadMain, 7, 3, 1, 1<<13)},
		"a row fewer": {rows[0], rows[1]},
		"a row more":  append([]core.UsageBucket{ledgerRow(day.Add(time.Hour), "claude-y", core.UsageThreadMain, 1, 1, 1, 1)}, rows...),
	} {
		got, _, err := ConversationUsageOf(ctx, db, "claude-code", "bf-hold", other)
		require.NoError(t, err, name)
		require.False(t, got.Holds, name)
		require.Equal(t, core.SumUsage(rows), got.Totals, "%s: the totals as they stand", name)
	}
	after, err := SessionUsage(ctx, db, s.ID)
	require.NoError(t, err)
	require.Equal(t, rows, ledgerBuckets(after), "reading wrote nothing")

	// Totals from before the ledger, and no rows: not held, whatever the rows.
	old := ledgerAmbient(t, db, "01BACKFILL0000000000000012", "claude-code", "bf-old", "example-ops", day)
	_, err = db.ExecContext(ctx, `UPDATE sessions SET input_tokens = 107, total_tokens = 107 WHERE id = ?`, old.ID)
	require.NoError(t, err)
	got, _, err = ConversationUsageOf(ctx, db, "claude-code", "bf-old", []core.UsageBucket{ledgerRow(day, "claude-x", core.UsageThreadMain, 107, 0, 1, 1)})
	require.NoError(t, err)
	require.False(t, got.Holds, "the totals are not the ledger's sum")
	require.Equal(t, 107, got.Totals.Total)

	_, _, err = ConversationUsageOf(ctx, db, "claude-code", "bf-hold", []core.UsageBucket{rows[0], rows[0]})
	require.ErrorIs(t, err, ErrInvalidUsage)
}

// A moved conversation's rows are held only as the writer splits them: the
// buckets before the move on the session the move ended, the rest on the
// child. Its totals are both sessions'.
func TestConversationUsageOf_AMovedConversation(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	start := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	move := time.Date(2026, 11, 20, 10, 7, 0, 0, time.UTC)
	parent := core.Session{ID: "01BACKFILLPARENT0000000000", Name: "cc/bf-move", ProjectSlug: "example-ops",
		Status: core.SessionCompleted, ExternalClient: "claude-code", ExternalSessionID: "bf-move",
		CreatedAt: start, UpdatedAt: move}
	require.NoError(t, CreateSession(ctx, db, parent))
	child := core.Session{ID: "01BACKFILLCHILD00000000000", Name: "cc/bf-move@example-finance", ProjectSlug: "example-finance",
		Status: core.SessionCompleted, Ambient: true, ExternalClient: "claude-code", ExternalSessionID: "bf-move",
		ParentSessionID: parent.ID, CreatedAt: move, UpdatedAt: move}
	require.NoError(t, CreateSession(ctx, db, child))
	rows := []core.UsageBucket{
		ledgerRow(start, "claude-x", core.UsageThreadMain, 100, 10, 3, 0b111),
		ledgerRow(move.Truncate(15*time.Minute), "claude-x", core.UsageThreadMain, 20, 2, 2, 1),
	}
	_, err := ReplaceConversationUsage(ctx, db, "claude-code", "bf-move", rows, move.Add(time.Hour))
	require.NoError(t, err)
	got, found, err := ConversationUsageOf(ctx, db, "claude-code", "bf-move", rows)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, got.Holds)
	require.Equal(t, core.SumUsage(rows), got.Totals, "both sessions' totals")

	// The same rows, all on the child: not how the writer splits them.
	_, err = db.ExecContext(ctx, `UPDATE session_usage SET session_id = ? WHERE session_id = ?`, child.ID, parent.ID)
	require.NoError(t, err)
	got, _, err = ConversationUsageOf(ctx, db, "claude-code", "bf-move", rows)
	require.NoError(t, err)
	require.False(t, got.Holds)
}
