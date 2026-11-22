package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// OwnerQueueTasks reads exactly the three sets the Overview's needs model
// judges -- the gates still open or in progress, the open steps a gate holds,
// and every task in progress -- with only the columns that judge them, and a
// project the handle hides is absent from the read itself.
func TestOwnerQueueTasks_ReadsTheThreeSetsAndNoContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	mk := func(task core.Task) string {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		task.ID, task.CreatedBy, task.CreatedAt, task.UpdatedAt = id, "test", now, now
		task.Body = "body of " + task.Title
		if task.Status == "" {
			task.Status = core.TaskOpen
		}
		require.NoError(t, CreateTask(ctx, db, task))
		return id
	}
	lease := now.Add(-time.Minute)
	gate := mk(core.Task{ProjectSlug: "example-ops", Title: "sign", Kind: core.TaskKindGate, Gate: core.TaskGate{Who: "owner", What: "sign"}})
	held := mk(core.Task{ProjectSlug: "example-ops", Title: "after the gate", DependsOn: []string{gate}})
	own := mk(core.Task{ProjectSlug: "example-ops", Title: "own gate", Gate: core.TaskGate{Who: "owner", What: "pick"}})
	inProg := mk(core.Task{ProjectSlug: "example-ops", Title: "lapsed", Status: core.TaskInProgress, ClaimedBy: "01SBHOLDER", LeaseExpiresAt: &lease})
	mk(core.Task{ProjectSlug: "example-ops", Title: "ready"})
	mk(core.Task{ProjectSlug: "example-ops", Title: "closed gate", Kind: core.TaskKindGate, Status: core.TaskDone,
		Gate: core.TaskGate{Who: "owner", What: "done"}})
	hidden := mk(core.Task{ProjectSlug: "example-finance", Title: "hidden gate", Kind: core.TaskKindGate,
		Gate: core.TaskGate{Who: "owner", What: "sign"}})

	got, err := OwnerQueueTasks(ctx, db)
	require.NoError(t, err)
	byID := map[string]core.Task{}
	for _, tk := range got {
		byID[tk.ID] = tk
		require.Empty(t, tk.Title, "no content is read")
		require.Empty(t, tk.Body, "no content is read")
	}
	require.Len(t, got, 5)
	for _, id := range []string{gate, held, own, inProg, hidden} {
		require.Contains(t, byID, id)
	}
	require.Equal(t, core.TaskKindGate, byID[gate].Kind)
	require.Equal(t, "owner", byID[own].Gate.Who)
	require.Nil(t, byID[own].Gate.ClearedAt)
	require.Equal(t, core.TaskInProgress, byID[inProg].Status)
	require.Equal(t, "01SBHOLDER", byID[inProg].ClaimedBy)
	require.NotNil(t, byID[inProg].LeaseExpiresAt)
	require.True(t, byID[inProg].LeaseExpiresAt.Equal(lease.Truncate(time.Nanosecond)))
	require.Equal(t, "example-ops", byID[inProg].ProjectSlug)

	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	seen, err := OwnerQueueTasks(ctx, view)
	require.NoError(t, err)
	require.Len(t, seen, 4)
	for _, tk := range seen {
		require.NotEqual(t, hidden, tk.ID, "a hidden project's gate is absent from the read")
	}
}

// CountPendingProposals counts the pending proposals alone: a resolved one is
// not waiting.
func TestCountPendingProposals_CountsWhatWaits(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	n, err := CountPendingProposals(ctx, db)
	require.NoError(t, err)
	require.Zero(t, n)
	p, err := CreateProposal(ctx, db, ProposalArchive, map[string]any{"key": "sb-1"})
	require.NoError(t, err)
	_, err = CreateProposal(ctx, db, ProposalArchive, map[string]any{"key": "sb-2"})
	require.NoError(t, err)
	n, err = CountPendingProposals(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.NoError(t, ResolveProposal(ctx, db, p.ID, ProposalDismissed, "owner", time.Now().UTC()))
	n, err = CountPendingProposals(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}
