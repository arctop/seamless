package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// A task moved to another project by the store's own update -- no tool in
// front of it to remember the stamps -- takes its provenance stamps along:
// item_provenance names the project the task is in now, so the came-from
// lists by project stay true, and the stamp naming the new project as its
// source leaves the index (ProvenanceRelocate).
func TestUpdateTask_AMoveTakesTheTasksStampsAlong(t *testing.T) {
	ctx := context.Background()
	db := crDB(t)
	task := core.Task{ID: crID(t), ProjectSlug: crData, Title: "reconcile the ledger", Status: core.TaskOpen,
		CreatedAt: crNow, UpdatedAt: crNow}
	require.NoError(t, CreateTask(ctx, db, task))
	require.NoError(t, StampWorkItem(ctx, db, core.ItemKindTask, task.ID, crData, []core.Stamp{
		{Project: crFin, Via: core.StampViaGrant, Ref: "01A"},
		{Project: crLegal, Via: core.StampViaGrant, Ref: "01B"},
	}, crNow))

	to := crLegal
	moved, err := UpdateTask(ctx, db, task.ID, TaskPatch{ProjectSlug: &to}, "", crNow)
	require.NoError(t, err)
	require.Equal(t, crLegal, moved.ProjectSlug)

	in, err := ProvenanceForProject(ctx, db, crLegal, "", 0)
	require.NoError(t, err)
	require.Len(t, in, 1, "the stamps moved with the task: %+v", in)
	require.Equal(t, task.ID, in[0].ItemID)
	require.Equal(t, crFin, in[0].Source)
	stale, err := ProvenanceForProject(ctx, db, crData, "", 0)
	require.NoError(t, err)
	require.Empty(t, stale, "the project it left lists none of them")

	// An update that moves nothing leaves the stamps where they are.
	title := "reconcile every ledger"
	_, err = UpdateTask(ctx, db, task.ID, TaskPatch{Title: &title}, "", crNow)
	require.NoError(t, err)
	again, err := ProvenanceForProject(ctx, db, crLegal, "", 0)
	require.NoError(t, err)
	require.Equal(t, in, again)
}
