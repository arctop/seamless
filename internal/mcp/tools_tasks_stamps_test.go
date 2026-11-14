package mcp_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// tasks_update moving a task to another project takes the task's provenance
// stamps along: the store relocates them inside the update (store.
// UpdateTaskDetailed), so the tool only adds the call's own stamps.
func TestTasksUpdate_AMoveTakesTheTasksStampsAlong(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	for _, p := range []string{"demo", "other"} {
		_, err := store.EnsureProject(ctx, db, p, p)
		require.NoError(t, err)
	}
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	id := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "reconcile the ledger"})["id"].(string)
	require.NoError(t, store.StampWorkItem(ctx, db, core.ItemKindTask, id, "demo",
		[]core.Stamp{{Project: "example-finance", Via: core.StampViaGrant, Ref: "01A"}}, time.Now().UTC()))

	moved := callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": id, "project": "other"})
	require.Equal(t, "other", moved["project"])
	in, err := store.ProvenanceForProject(ctx, db, "other", "", 0)
	require.NoError(t, err)
	require.Len(t, in, 1, "%+v", in)
	require.Equal(t, id, in[0].ItemID)
	require.Equal(t, "example-finance", in[0].Source)
	left, err := store.ProvenanceForProject(ctx, db, "demo", "", 0)
	require.NoError(t, err)
	require.Empty(t, left)
}
