package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// itRow writes a memory or note index row at a time; invalid retires a
// memory.
func itRow(t *testing.T, db *sql.DB, table, id, name, project, kind string, at time.Time, invalid bool) {
	t.Helper()
	ts := core.FormatTime(at)
	var err error
	if table == "memories_index" {
		var inv any
		if invalid {
			inv = ts
		}
		_, err = db.Exec(`INSERT INTO memories_index (id, kind, name, description, project, file_path, content_hash, created_at, updated_at, invalid_at)
			VALUES (?, ?, ?, '', ?, ?, 'h', ?, ?, ?)`, id, kind, name, project, "memory/"+project+"/"+name+".md", ts, ts, inv)
	} else {
		_, err = db.Exec(`INSERT INTO notes_index (id, title, slug, description, project, file_path, content_hash, created_at, updated_at)
			VALUES (?, ?, ?, '', ?, ?, 'h', ?, ?)`, id, name, name, project, "notes/"+project+"/"+name+".md", ts, ts)
	}
	require.NoError(t, err)
}

// The context column's reads: up to limit others, newest first, without the
// item itself, and the count of all of them; another project, another kind
// and a retired memory stay out.
func TestItContextColumnReads(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for i := range 4 {
		itRow(t, db, "memories_index", fmt.Sprintf("M%d", i), fmt.Sprintf("m%d", i), "example-pay", "gotcha", base.Add(time.Duration(i)*time.Hour), false)
		itRow(t, db, "notes_index", fmt.Sprintf("N%d", i), fmt.Sprintf("n%d", i), "example-pay", "", base.Add(time.Duration(i)*time.Hour), false)
	}
	itRow(t, db, "memories_index", "MX", "retired", "example-pay", "gotcha", base.Add(9*time.Hour), true)
	itRow(t, db, "memories_index", "MD", "decision", "example-pay", "decision", base.Add(9*time.Hour), false)
	itRow(t, db, "memories_index", "MW", "web", "example-web", "gotcha", base.Add(9*time.Hour), false)
	itRow(t, db, "notes_index", "NW", "web", "example-web", "", base.Add(9*time.Hour), false)

	mems, total, err := MemoriesOfKind(ctx, db, "example-pay", "gotcha", "M2", 2)
	require.NoError(t, err)
	require.Equal(t, 4, total)
	require.Equal(t, []string{"M3", "M1"}, []string{mems[0].ID, mems[1].ID})
	notes, total, err := NotesOfProject(ctx, db, "example-pay", "N3", 5)
	require.NoError(t, err)
	require.Equal(t, 4, total)
	require.Len(t, notes, 3)
	require.Equal(t, "N2", notes[0].ID)
	none, total, err := MemoriesOfKind(ctx, db, "example-empty", "gotcha", "", 5)
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, none)

	// An item the list does not hold has no place in it.
	_, found, err := MemoryPlace(ctx, db, MemoryList{ByProject: true, Project: "example-web", Sort: "recent"}, "M1")
	require.NoError(t, err)
	require.False(t, found)
	p, found, err := MemoryPlace(ctx, db, MemoryList{ByProject: true, Project: "example-pay", Kind: "gotcha", Inactive: true, Sort: "recent"}, "MX")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ItemPlace{Pos: 1, Total: 1}, p, "a retired memory's place is among the retired")
	p, found, err = NotePlace(ctx, db, NoteList{ByProject: true, Project: "example-pay", Sort: "recent"}, "N1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ItemPlace{Pos: 3, Total: 4, Prev: ItemStep{ID: "N2", Label: "n2"}, Next: ItemStep{ID: "N0", Label: "n0"}}, p)
}

// The places read through a view like every console read: a hidden
// project's rows are not in the window, so they neither count nor neighbour.
func TestItPlacesReadThroughTheView(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	itRow(t, db, "memories_index", "M1", "open-old", "example-open", "gotcha", base, false)
	itRow(t, db, "memories_index", "M2", "hidden", "example-locked", "gotcha", base.Add(time.Hour), false)
	itRow(t, db, "memories_index", "M3", "open-new", "example-open", "gotcha", base.Add(2*time.Hour), false)
	view, err := OpenView(path, []string{"example-locked"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	all := MemoryList{Kind: "gotcha", Sort: "recent"}
	p, found, err := MemoryPlace(context.Background(), view, all, "M3")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ItemPlace{Pos: 1, Total: 2, Next: ItemStep{ID: "M1", Label: "open-old"}}, p)
	_, found, err = MemoryPlace(context.Background(), view, all, "M2")
	require.NoError(t, err)
	require.False(t, found, "the hidden memory has no place at all")
	p, _, err = MemoryPlace(context.Background(), db, all, "M3")
	require.NoError(t, err)
	require.Equal(t, 3, p.Total, "without the view it counts")
}
