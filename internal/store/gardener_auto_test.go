package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// The count is the applied rows resolved_by names, windowed by when they were
// decided: a dismissal, another decider's apply, and an undone apply are not
// counted.
func TestAaCountProposalsAppliedSince(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	resolve := func(by, status string, at time.Time) string {
		p, err := CreateProposal(ctx, db, ProposalMemoryWanted, map[string]any{"project": "example-ops", "key": core.FormatTime(at) + by + status})
		require.NoError(t, err)
		require.NoError(t, ResolveProposal(ctx, db, p.ID, status, by, at))
		return p.ID
	}
	resolve("auto", ProposalApplied, base)
	resolve("auto", ProposalApplied, base.Add(2*time.Hour))
	undone := resolve("auto", ProposalApplied, base.Add(3*time.Hour))
	resolve("auto", ProposalDismissed, base.Add(4*time.Hour))
	resolve("owner", ProposalApplied, base.Add(5*time.Hour))
	require.NoError(t, ReopenProposal(ctx, db, undone))

	n, err := CountProposalsAppliedSince(ctx, db, "auto", time.Time{})
	require.NoError(t, err)
	require.Equal(t, 2, n, "every automatic apply still standing")
	n, err = CountProposalsAppliedSince(ctx, db, "auto", base.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n, "only those decided after since")
	n, err = CountProposalsAppliedSince(ctx, db, "auto", base.Add(2*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, n, "since is exclusive")
}

// The newest gardener.action by a decider, leaving out the named actions; an
// event with a payload that is not JSON is skipped rather than failing the read.
func TestAaLastGardenerActionBy(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	_, found, err := LastGardenerActionBy(ctx, db, "console", nil)
	require.NoError(t, err)
	require.False(t, found)

	base := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	insert := func(at time.Time, payload string) {
		id, err := core.NewID()
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO events (id, ts, kind, payload) VALUES (?, ?, ?, ?)`,
			id, core.FormatTime(at), string(core.EventGardenerAction), payload)
		require.NoError(t, err)
	}
	insert(base, `{"action":"dismiss","by":"console"}`)
	insert(base.Add(time.Hour), `{"action":"apply","by":"auto"}`)
	insert(base.Add(2*time.Hour), `{"action":"switches","by":"console"}`)
	insert(base.Add(3*time.Hour), `not json`)

	at, found, err := LastGardenerActionBy(ctx, db, "console", []string{"switches", "switches_reset"})
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, base, at)
	at, found, err = LastGardenerActionBy(ctx, db, "console", nil)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, base.Add(2*time.Hour), at)
}
