package gardener

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/lifecycle"
	"github.com/arctop/seamless/internal/store"
)

// archiveByHand retires a memory the way an agent or the owner would, outside
// any proposal.
func archiveByHand(t *testing.T, g *Service, mgr *files.Manager, id string) {
	t.Helper()
	ctx := context.Background()
	idx, ok, err := store.MemoryByID(ctx, g.db, id)
	require.NoError(t, err)
	require.True(t, ok)
	mem, err := mgr.Store().ReadMemory(idx.FilePath)
	require.NoError(t, err)
	_, err = lifecycle.Archive(ctx, mgr, mem, "by hand", time.Now().UTC())
	require.NoError(t, err)
}

func memoryID(t *testing.T, g *Service, project, name string) string {
	t.Helper()
	m, ok, err := store.MemoryByName(context.Background(), g.db, project, name)
	require.NoError(t, err)
	require.True(t, ok)
	return m.ID
}

// The gardener pass settles a proposal whose memory was archived by another
// route since it was raised, as a dismissal carrying the reason, and leaves a
// proposal that still stands pending.
func TestRunOnce_SettlesMootProposals(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	writeMem(t, mgr, "retired-by-hand", "web", "1", core.KindGotcha, now, "body")
	writeMem(t, mgr, "still-here", "web", "2", core.KindGotcha, now, "body")
	writeMem(t, mgr, "kept", "web", "3", core.KindGotcha, now, "body")
	gone, here, kept := memoryID(t, g, "web", "retired-by-hand"), memoryID(t, g, "web", "still-here"), memoryID(t, g, "web", "kept")

	archive, err := store.CreateProposal(ctx, g.db, store.ProposalArchive, map[string]any{
		"key": "archive:" + gone, "id": gone, "name": "retired-by-hand",
	})
	require.NoError(t, err)
	merge, err := store.CreateProposal(ctx, g.db, store.ProposalMerge, map[string]any{
		"key":  mergeKey(kept, gone),
		"keep": map[string]any{"id": kept, "name": "kept"},
		"drop": map[string]any{"id": gone, "name": "retired-by-hand"},
	})
	require.NoError(t, err)
	standing, err := store.CreateProposal(ctx, g.db, store.ProposalArchive, map[string]any{
		"key": "archive:" + here, "id": here, "name": "still-here",
	})
	require.NoError(t, err)

	archiveByHand(t, g, mgr, gone)
	_, err = g.RunOnce(ctx)
	require.NoError(t, err)

	for _, id := range []string{archive.ID, merge.ID} {
		got, _, err := store.ProposalByID(ctx, g.db, id)
		require.NoError(t, err)
		require.Equal(t, store.ProposalDismissed, got.Status, got.Kind)
		require.Equal(t, "memory web/retired-by-hand is already inactive", got.Result["settled"])
	}
	got, _, err := store.ProposalByID(ctx, g.db, standing.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalPending, got.Status)

	evs, err := g.events.ByKinds(ctx, []core.EventKind{core.EventGardenerAction}, "", "", 50)
	require.NoError(t, err)
	settles := 0
	for _, e := range evs {
		if e.Payload["action"] == "settle" {
			settles++
		}
	}
	require.Equal(t, 2, settles)

	// Undo reopens a settled proposal like any dismissal.
	require.NoError(t, g.Undo(ctx, archive.ID))
	got, _, err = store.ProposalByID(ctx, g.db, archive.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalPending, got.Status)
}

// An apply that finds the memory already inactive settles the proposal and
// says so, rather than erroring and leaving it in the queue to be clicked
// again. A memory missing from the index altogether still errors: that absence
// may be a reindex in flight, not a deletion.
func TestApply_MootProposalSettles(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	writeMem(t, mgr, "retired-by-hand", "web", "1", core.KindGotcha, time.Now().UTC(), "body")
	gone := memoryID(t, g, "web", "retired-by-hand")

	p, err := store.CreateProposal(ctx, g.db, store.ProposalArchive, map[string]any{
		"key": "archive:" + gone, "id": gone, "name": "retired-by-hand",
	})
	require.NoError(t, err)
	archiveByHand(t, g, mgr, gone)

	res, err := g.Apply(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalDismissed, res["status"])
	require.Equal(t, "memory web/retired-by-hand is already inactive -- nothing left to apply", res["settled"])
	got, _, err := store.ProposalByID(ctx, g.db, p.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalDismissed, got.Status)

	_, err = g.Apply(ctx, p.ID)
	require.ErrorContains(t, err, "already dismissed")
}
