package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// convIndexRows writes index rows for the came-from fixture: memories and a
// note straight into the index (the files layer is not under test here), and
// tasks through the store.
func convIndexRows(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	ts := core.FormatTime(convNow)
	for _, m := range []struct{ id, project string }{{"m-legal", "example-ops"}, {"m-fin", "example-ops"}, {"m-plain", "example-ops"}} {
		_, err := db.ExecContext(ctx, `INSERT INTO memories_index (id, kind, name, description, project, file_path, content_hash, created_at, updated_at)
			VALUES (?, 'decision', ?, 'd', ?, ?, 'h', ?, ?)`, m.id, m.id, m.project, "memory/"+m.project+"/"+m.id+".md", ts, ts)
		require.NoError(t, err)
	}
	_, err := db.ExecContext(ctx, `INSERT INTO notes_index (id, title, slug, project, file_path, content_hash, created_at, updated_at)
		VALUES ('n-legal', 'review', 'review', 'example-ops', 'notes/example-ops/review.md', 'h', ?, ?)`, ts, ts)
	require.NoError(t, err)
	for _, tk := range []struct{ id, project string }{{"t-legal", "example-ops"}, {"t-moved", "example-legal"}, {"t-fin", "example-finance"}} {
		require.NoError(t, CreateTask(ctx, db, core.Task{ID: tk.id, ProjectSlug: tk.project, Title: tk.id,
			Status: core.TaskOpen, CreatedAt: convNow, UpdatedAt: convNow}))
	}
	for i, s := range []ProvenanceStamp{
		{ItemKind: core.ItemKindMemory, ItemID: "m-legal", Project: "example-ops", Source: "example-legal", ApprovalID: "01G"},
		{ItemKind: core.ItemKindMemory, ItemID: "m-fin", Project: "example-ops", Source: "example-finance", ApprovalID: "01M"},
		{ItemKind: core.ItemKindNote, ItemID: "n-legal", Project: "example-ops", Source: "example-legal", ApprovalID: "01G"},
		{ItemKind: core.ItemKindTask, ItemID: "t-legal", Project: "example-ops", Source: "example-legal", ApprovalID: "01G"},
		// A task deleted outside Seamless: its row outlived it.
		{ItemKind: core.ItemKindTask, ItemID: "t-gone", Project: "example-ops", Source: "example-legal", ApprovalID: "01G"},
		// A row whose task now sits in another project.
		{ItemKind: core.ItemKindTask, ItemID: "t-moved", Project: "example-ops", Source: "example-finance", ApprovalID: "01M"},
		// Findings and trials are no tab's.
		{ItemKind: core.ItemKindSession, ItemID: "s-1", Project: "example-ops", Source: "example-legal", ApprovalID: "01G"},
		{ItemKind: core.ItemKindTask, ItemID: "t-fin", Project: "example-finance", Source: "example-legal", ApprovalID: "01G"},
	} {
		require.NoError(t, ProvenanceUpsert(ctx, db, s, convNow.Add(-time.Duration(i)*time.Second)))
	}
}

func TestCameFrom_OnlyItemsThatAreThereCount(t *testing.T) {
	db, path := convDB(t)
	ctx := context.Background()
	convIndexRows(t, db)

	got, err := CameFromSources(ctx, db, "example-ops")
	require.NoError(t, err)
	require.Equal(t, []CameFromSource{
		{Project: "example-finance", Counts: map[string]int{core.ItemKindMemory: 1}},
		{Project: "example-legal", Counts: map[string]int{core.ItemKindMemory: 1, core.ItemKindNote: 1, core.ItemKindTask: 1}},
	}, got, "the orphan, the moved task and the findings stamp count nowhere")

	items, err := CameFromItems(ctx, db, "example-ops", "example-legal")
	require.NoError(t, err)
	require.Equal(t, map[string][]string{core.ItemKindMemory: {"m-legal"}, core.ItemKindNote: {"n-legal"}, core.ItemKindTask: {"t-legal"}}, items)
	items, err = CameFromItems(ctx, db, "example-ops", "example-finance")
	require.NoError(t, err)
	require.Equal(t, map[string][]string{core.ItemKindMemory: {"m-fin"}}, items)
	items, err = CameFromItems(ctx, db, "example-ops", "example-hr")
	require.NoError(t, err)
	require.Empty(t, items, "a source no stamp names")

	none, err := CameFromSources(ctx, db, "example-hr")
	require.NoError(t, err)
	require.Empty(t, none)

	// Through a handle that hides example-finance: its own items' stamps are
	// gone, and a stamp that only names it as the source stays (a slug is no
	// content).
	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	hidden, err := CameFromSources(ctx, view, "example-finance")
	require.NoError(t, err)
	require.Empty(t, hidden)
	shown, err := CameFromSources(ctx, view, "example-ops")
	require.NoError(t, err)
	require.Len(t, shown, 2)
	require.Equal(t, "example-finance", shown[0].Project)
}
