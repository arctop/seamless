package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// Only a Claude Code conversation's ambient row is owed usage: a Codex
// conversation's Stops credit it as it grows, and an explicit session or one
// with no agent session id carries none. Marking again moves owed_at and
// keeps what the reads and asks recorded.
func TestMarkUsageOwed_OnlyAClaudeCodeConversationsAmbientRow(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	t0 := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	cc := ledgerAmbient(t, db, "01OWEDCC0000000000000000000", "claude-code", "cc-1", "demo", t0)
	codex := ledgerAmbient(t, db, "01OWEDCODEX0000000000000000", "codex", "cx-1", "demo", t0)
	explicit := core.Session{ID: "01OWEDEXPLICIT0000000000000", Name: "explicit", ProjectSlug: "demo",
		Status: core.SessionActive, CreatedAt: t0, UpdatedAt: t0}
	require.NoError(t, CreateSession(ctx, db, explicit))
	noID := core.Session{ID: "01OWEDNOID00000000000000000", Name: "claude-code/none", ProjectSlug: "demo",
		Status: core.SessionActive, Ambient: true, ExternalClient: "claude-code", CreatedAt: t0, UpdatedAt: t0}
	require.NoError(t, CreateSession(ctx, db, noID))

	for _, id := range []string{codex.ID, explicit.ID, noID.ID, "01NOSUCHSESSION000000000000"} {
		marked, err := MarkUsageOwed(ctx, db, id, t0)
		require.NoError(t, err)
		require.False(t, marked, id)
	}
	marked, err := MarkUsageOwed(ctx, db, cc.ID, t0)
	require.NoError(t, err)
	require.True(t, marked)

	owed, err := OwedUsages(ctx, db)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	require.Equal(t, cc.ID, owed[0].Session.ID)
	require.Equal(t, "cc-1", owed[0].Session.ExternalSessionID)
	require.Equal(t, t0, owed[0].OwedAt)
	require.Nil(t, owed[0].ChangedAt)
	require.Nil(t, owed[0].AskedAt)
	require.Equal(t, t0, owed[0].LastChange())

	// An ask and a read are kept across a second mark; owed_at moves.
	asks, recorded, err := RecordUsageOwedAsk(ctx, db, cc.ID, t0.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, recorded)
	require.Equal(t, 1, asks)
	asks, _, err = RecordUsageOwedAsk(ctx, db, cc.ID, t0.Add(2*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 2, asks)
	changed := t0.Add(90 * time.Minute)
	recorded, err = RecordUsageOwedRead(ctx, db, cc.ID, changed)
	require.NoError(t, err)
	require.True(t, recorded)
	_, err = MarkUsageOwed(ctx, db, cc.ID, t0.Add(time.Hour))
	require.NoError(t, err)

	o, found, err := OwedUsageOf(ctx, db, "claude-code", "cc-1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, t0.Add(time.Hour), o.OwedAt)
	require.Equal(t, changed, *o.ChangedAt)
	require.Equal(t, 0, o.Asks, "a read answers the asks")
	require.Equal(t, t0.Add(2*time.Minute), *o.AskedAt)
	require.Equal(t, changed, o.LastChange())

	_, found, err = OwedUsageOf(ctx, db, "codex", "cx-1")
	require.NoError(t, err)
	require.False(t, found)
}

// The caller's list is its client's and account's; a SessionEnd's clear is
// its conversation's alone; and a cleared row records nothing more.
func TestOwedUsage_ForACallerAndCleared(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	t0 := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	mine := ledgerAmbient(t, db, "01OWEDMINE00000000000000000", "claude-code", "mine", "demo", t0)
	theirs := core.Session{ID: "01OWEDTHEIRS000000000000000", Name: "claude-code/theirs", ProjectSlug: "demo",
		Status: core.SessionActive, Ambient: true, ExternalClient: "claude-code", ExternalSessionID: "theirs",
		Account: "ci", CreatedAt: t0, UpdatedAt: t0}
	require.NoError(t, CreateSession(ctx, db, theirs))
	later := ledgerAmbient(t, db, "01OWEDLATER0000000000000000", "claude-code", "later", "demo", t0)
	for i, id := range []string{mine.ID, theirs.ID, later.ID} {
		_, err := MarkUsageOwed(ctx, db, id, t0.Add(time.Duration(i)*time.Minute))
		require.NoError(t, err)
	}

	owner, err := OwedUsagesFor(ctx, db, "claude-code", "")
	require.NoError(t, err)
	require.Len(t, owner, 2)
	require.Equal(t, mine.ID, owner[0].Session.ID, "the longest owed first")
	require.Equal(t, later.ID, owner[1].Session.ID)
	ci, err := OwedUsagesFor(ctx, db, "claude-code", "ci")
	require.NoError(t, err)
	require.Len(t, ci, 1)
	require.Equal(t, theirs.ID, ci[0].Session.ID)
	none, err := OwedUsagesFor(ctx, db, "codex", "")
	require.NoError(t, err)
	require.Empty(t, none)

	require.NoError(t, ClearConversationUsageOwed(ctx, db, "claude-code", "mine"))
	require.NoError(t, ClearUsageOwed(ctx, db, later.ID))
	all, err := OwedUsages(ctx, db)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, theirs.ID, all[0].Session.ID)

	_, recorded, err := RecordUsageOwedAsk(ctx, db, mine.ID, t0)
	require.NoError(t, err)
	require.False(t, recorded)
	recorded, err = RecordUsageOwedRead(ctx, db, later.ID, t0)
	require.NoError(t, err)
	require.False(t, recorded)
}
