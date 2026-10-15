package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func TestGetUsageSummary(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedMemoryRow(t, db, "m1", "alpha", now)
	seedMemoryRow(t, db, "m2", "beta", now)
	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: "s1", Name: "cc/a", Status: core.SessionCompleted, Findings: "x", CreatedAt: now, UpdatedAt: now,
	}))
	_, err := CreateProposal(ctx, db, ProposalArchive, map[string]any{"key": "archive:m1"})
	require.NoError(t, err)

	// Injection + read events -> retrieval stats.
	insertEvent(t, db, core.EventInjected, "", `{"item_ids":["m1","m2"]}`, now)
	insertEvent(t, db, core.EventInjected, "", `{"item_ids":["m1"]}`, now)
	insertEvent(t, db, core.EventMemoryRead, "m1", "{}", now)
	require.NoError(t, RebuildRetrievalStats(ctx, db))

	u, err := GetUsageSummary(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 2, u.Memories.Active)
	require.Equal(t, 2, u.Memories.ByKind["gotcha"])
	require.Equal(t, 1, u.Sessions["completed"])
	require.Equal(t, 1, u.GardenerPending["archive"])
	require.Equal(t, 3, u.Retrieval.Injections) // m1 x2 + m2 x1
	require.Equal(t, 1, u.Retrieval.Reads)
	require.NotEmpty(t, u.Retrieval.TopInjected)
	require.Equal(t, "m1", u.Retrieval.TopInjected[0].ID) // most-injected first
	require.Equal(t, 2, u.Retrieval.TopInjected[0].Count)
	require.Equal(t, 2, u.EventsByKind["retrieval.injected"])
}

func TestGetUsageSummary_FunnelBySurface(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	insertEvent(t, db, core.EventInjected, "", `{"hook":"session-start","item_ids":["m1","m2"]}`, base)
	insertEvent(t, db, core.EventInjected, "", `{"hook":"subagent-start","item_ids":["m2"]}`, base)
	insertEvent(t, db, core.EventMemoryRead, "m2", "{}", base.Add(time.Hour))

	u, err := GetUsageSummary(ctx, db)
	require.NoError(t, err)
	require.Len(t, u.Retrieval.FunnelBySurface, 2)

	ss := u.Retrieval.FunnelBySurface[0]
	require.Equal(t, "session-start", ss.Surface)
	require.Equal(t, 2, ss.Injections)
	require.Equal(t, 2, ss.Items)
	require.Equal(t, 1, ss.ItemsRead)
	require.Equal(t, 50, ss.ReadRate)

	sub := u.Retrieval.FunnelBySurface[1]
	require.Equal(t, "subagent-start", sub.Surface)
	require.Equal(t, 1, sub.Injections)
	require.Equal(t, 1, sub.Items)
	require.Equal(t, 1, sub.ItemsRead)
	require.Equal(t, 100, sub.ReadRate)
}

// The counts are the summary minus its event-log aggregates: the console
// Overview re-renders on every live event and shows none of those, the
// all-time funnel least of all.
func TestGetUsageCounts_IsTheSummaryWithoutTheEventAggregates(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedMemoryRow(t, db, "m1", "alpha", now)
	seedMemoryRow(t, db, "m2", "beta", now)
	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: "s1", Name: "cc/a", Status: core.SessionCompleted, Findings: "x", CreatedAt: now, UpdatedAt: now,
	}))
	_, err := CreateProposal(ctx, db, ProposalArchive, map[string]any{"key": "archive:m1"})
	require.NoError(t, err)
	insertEvent(t, db, core.EventInjected, "", `{"hook":"session-start","item_ids":["m1","m2"]}`, now)
	insertEvent(t, db, core.EventMemoryRead, "m1", "{}", now)
	require.NoError(t, RebuildRetrievalStats(ctx, db))

	sum, err := GetUsageSummary(ctx, db)
	require.NoError(t, err)
	counts, err := GetUsageCounts(ctx, db)
	require.NoError(t, err)

	require.Equal(t, sum.Memories, counts.Memories)
	require.Equal(t, sum.Notes, counts.Notes)
	require.Equal(t, sum.Sessions, counts.Sessions)
	require.Equal(t, sum.Tasks, counts.Tasks)
	require.Equal(t, sum.GardenerPending, counts.GardenerPending)
	require.Equal(t, sum.Retrieval.TopInjected, counts.Retrieval.TopInjected)

	require.NotEmpty(t, sum.Retrieval.FunnelBySurface)
	require.Empty(t, counts.Retrieval.FunnelBySurface)
	require.Empty(t, counts.EventsByKind)
	require.Empty(t, counts.Retrieval.TopUtility)
	require.Zero(t, counts.Retrieval.Injections)
}
