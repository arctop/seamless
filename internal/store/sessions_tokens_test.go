package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// A running total read again and again (Codex's cumulative counter, a pushed
// total) is credited by what it grew by, so the stored totals are always the
// latest total and never the sum of the reads: the resume/double-count rule
// at the store level.
func TestCreditConversationUsage_TheTotalsAreTheLatestNotTheSum(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	s := newSession("01TOK0000000000000000000AA", "cc/tok1")
	s.ExternalSessionID = "tok1-full"
	s.ExternalClient = "claude-code"
	s.Ambient = true
	require.NoError(t, CreateSession(ctx, db, s))

	now := time.Now().UTC()
	first := core.TokenUsage{Input: 100, Cached: 1000, CacheCreation: 200, Output: 50}
	first.Normalize()
	ok, err := CreditConversationUsage(ctx, db, "claude-code", "tok1-full", UsageCredit{Cumulative: first, At: now})
	require.NoError(t, err)
	require.True(t, ok)

	got, _, err := SessionByID(ctx, db, s.ID)
	require.NoError(t, err)
	require.Equal(t, first, got.Tokens)

	// The session resumes; the grown transcript yields a larger cumulative total.
	second := core.TokenUsage{Input: 180, Cached: 1800, CacheCreation: 360, Output: 90}
	second.Normalize()
	ok, err = CreditConversationUsage(ctx, db, "claude-code", "tok1-full", UsageCredit{Cumulative: second, At: now.Add(time.Minute)})
	require.NoError(t, err)
	require.True(t, ok)

	got, _, err = SessionByID(ctx, db, s.ID)
	require.NoError(t, err)
	require.Equal(t, second, got.Tokens, "the latest cumulative total, not the sum of harvests")
	requireTotalsAreTheLedger(t, db, s.ID)

	// The same total again: nothing grew, nothing is written.
	ok, err = CreditConversationUsage(ctx, db, "claude-code", "tok1-full", UsageCredit{Cumulative: second, At: now.Add(2 * time.Minute)})
	require.NoError(t, err)
	require.False(t, ok)
}

// The writers are no-ops on an incomplete identity, empty usage or a
// conversation with no ambient row. A session the agent already completed
// takes the counts as metadata: it stays completed, and its end time does not
// move; an active one's updated_at moves, as the heartbeat it always was.
func TestUsageWriters_GuardsAndNoOps(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)

	s := newSession("01TOK0000000000000000000BB", "cc/tok2")
	s.ExternalSessionID = "tok2-full"
	s.ExternalClient = "claude-code"
	s.Ambient = true
	require.NoError(t, CreateSession(ctx, db, s))
	now := time.Now().UTC()
	row := core.UsageBucket{Bucket: core.UsageBucketStart(now), Model: "claude-x", Thread: core.UsageThreadMain, Input: 10, Output: 5, Messages: 1}

	// Empty usage: no-op (a turn with no token record must not blank a prior harvest).
	ok, err := CreditConversationUsage(ctx, db, "claude-code", "tok2-full", UsageCredit{At: now})
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = ReplaceConversationUsage(ctx, db, "claude-code", "tok2-full", nil, now)
	require.NoError(t, err)
	require.False(t, ok)

	// Incomplete identity: no-op.
	ok, err = CreditConversationUsage(ctx, db, "", "tok2-full", UsageCredit{Cumulative: core.TokenUsage{Input: 1, Total: 1}, At: now})
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = ReplaceConversationUsage(ctx, db, "claude-code", "", []core.UsageBucket{row}, now)
	require.NoError(t, err)
	require.False(t, ok)

	// No ambient row (an explicit session holds the identity): no-op.
	explicit := newSession("01TOK0000000000000000000CC", "sess/explicit")
	explicit.ExternalSessionID = "tok3-full"
	explicit.ExternalClient = "claude-code"
	require.NoError(t, CreateSession(ctx, db, explicit))
	ok, err = ReplaceConversationUsage(ctx, db, "claude-code", "tok3-full", []core.UsageBucket{row}, now)
	require.NoError(t, err)
	require.False(t, ok, "an explicit session never takes the counts")

	// An active row's updated_at moves with a harvest.
	beat := now.Add(time.Minute)
	ok, err = ReplaceConversationUsage(ctx, db, "claude-code", "tok2-full", []core.UsageBucket{row}, beat)
	require.NoError(t, err)
	require.True(t, ok)
	got, _, err := SessionByID(ctx, db, s.ID)
	require.NoError(t, err)
	require.True(t, beat.Equal(got.UpdatedAt), "a harvest heartbeats an active row")

	// Completed: the counts land, the row stays completed at its end time.
	s.Status = core.SessionCompleted
	s.UpdatedAt = now
	require.NoError(t, UpdateSession(ctx, db, s))
	late := core.TokenUsage{Input: 999, Output: 999}
	late.Normalize()
	ok, err = CreditConversationUsage(ctx, db, "claude-code", "tok2-full", UsageCredit{Cumulative: late, At: now.Add(time.Hour)})
	require.NoError(t, err)
	require.True(t, ok, "a completed session takes a harvest")
	got, _, err = SessionByID(ctx, db, s.ID)
	require.NoError(t, err)
	require.Equal(t, late, got.Tokens)
	require.Equal(t, core.SessionCompleted, got.Status, "never revived")
	require.True(t, now.Equal(got.UpdatedAt), "the end time does not move")
	requireTotalsAreTheLedger(t, db, s.ID)
}

// Per-project token totals sum across a project's sessions on the board.
func TestProjectsWithCounts_SumsTokens(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Now().UTC()

	mk := func(id, name, project string, total int) {
		s := newSession(id, name)
		s.ProjectSlug = project
		s.ExternalSessionID = name + "-full"
		s.ExternalClient = "claude-code"
		s.Ambient = true
		require.NoError(t, CreateSession(ctx, db, s))
		u := core.TokenUsage{Input: total}
		u.Normalize()
		_, err := CreditConversationUsage(ctx, db, "claude-code", name+"-full", UsageCredit{Cumulative: u, At: now})
		require.NoError(t, err)
	}
	mk("01TOKP000000000000000000AA", "cc/p1a", "alpha", 100)
	mk("01TOKP000000000000000000BB", "cc/p1b", "alpha", 250)
	mk("01TOKP000000000000000000CC", "cc/p2a", "beta", 40)

	rows, err := ProjectsWithCounts(ctx, db, ResolveRetrievalWindow("all", now), now, 0)
	require.NoError(t, err)

	byProject := map[string]int{}
	for _, r := range rows {
		byProject[r.Project] = r.TokensTotal
	}
	require.Equal(t, 350, byProject["alpha"], "100 + 250")
	require.Equal(t, 40, byProject["beta"])
}
