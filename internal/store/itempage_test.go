package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
	notes, total, err := NotesOfProject(ctx, db, "example-pay", "N3", "", 5)
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

// nlTags sets a note index row's tags.
func nlTags(t *testing.T, db *sql.DB, id string, tags ...string) {
	t.Helper()
	raw, err := json.Marshal(tags)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE notes_index SET tags = ? WHERE id = ?`, string(raw), id)
	require.NoError(t, err)
}

// A Notes list's place honours its tag chip, its plan chip and its fold:
// a tag matches whole and exactly, a plan chip wants plan:<slug> with a
// slug, and the folded tag leaves its notes out of the window, the count and
// the neighbours. NotesOfProject folds the same way.
func TestNlNotePlaceTagsPlansAndFold(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for i, tags := range [][]string{
		{"research"}, {"research-notes", "plan:ops"}, {"agent-cache", "plan:ops", "research"}, {"plan:"}, {"PLAN:ops", "Research"}, nil,
	} {
		id := fmt.Sprintf("N%d", i)
		itRow(t, db, "notes_index", id, fmt.Sprintf("n%d", i), "example-ops", "", base.Add(time.Duration(i)*time.Hour), false)
		if tags != nil {
			nlTags(t, db, id, tags...)
		}
	}
	ids := func(l NoteList) []string {
		t.Helper()
		var out []string
		for _, id := range []string{"N5", "N4", "N3", "N2", "N1", "N0"} {
			if _, found, err := NotePlace(ctx, db, l, id); err != nil {
				t.Fatal(err)
			} else if found {
				out = append(out, id)
			}
		}
		return out
	}
	require.Equal(t, []string{"N2", "N0"}, ids(NoteList{Tag: "research", Sort: "recent"}), "a tag matches whole, with its case")
	require.Equal(t, []string{"N2", "N1"}, ids(NoteList{Plans: true, Sort: "recent"}), "plan:<slug> with a slug, lower case")
	require.Equal(t, []string{"N5", "N4", "N3", "N1", "N0"}, ids(NoteList{Without: "agent-cache", Sort: "recent"}))
	require.Equal(t, []string{"N1"}, ids(NoteList{Plans: true, Without: "agent-cache", Sort: "recent"}))
	p, found, err := NotePlace(ctx, db, NoteList{Without: "agent-cache", Sort: "recent"}, "N3")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ItemPlace{Pos: 3, Total: 5, Prev: ItemStep{ID: "N4", Label: "n4"}, Next: ItemStep{ID: "N1", Label: "n1"}}, p,
		"the folded note is no neighbour")

	notes, total, err := NotesOfProject(ctx, db, "example-ops", "N5", "agent-cache", 3)
	require.NoError(t, err)
	require.Equal(t, 5, total, "the folded note is not counted")
	require.Equal(t, []string{"N4", "N3", "N1"}, []string{notes[0].ID, notes[1].ID, notes[2].ID})
	_, total, err = NotesOfProject(ctx, db, "example-ops", "", "", 0)
	require.NoError(t, err)
	require.Equal(t, 6, total)
}

// The new filters read through a view like the rest: a hidden project's
// tagged notes are neither counted nor neighbours.
func TestNlNotePlaceReadsThroughTheView(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	itRow(t, db, "notes_index", "N1", "open-old", "example-open", "", base, false)
	itRow(t, db, "notes_index", "N2", "hidden", "example-locked", "", base.Add(time.Hour), false)
	itRow(t, db, "notes_index", "N3", "open-new", "example-open", "", base.Add(2*time.Hour), false)
	for _, id := range []string{"N1", "N2", "N3"} {
		nlTags(t, db, id, "research", "plan:shared")
	}
	view, err := OpenView(path, []string{"example-locked"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	for _, l := range []NoteList{{Tag: "research", Sort: "recent"}, {Plans: true, Without: "agent-cache", Sort: "name"}} {
		p, found, err := NotePlace(context.Background(), view, l, "N3")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, 2, p.Total, "%+v", l)
		_, found, err = NotePlace(context.Background(), view, l, "N2")
		require.NoError(t, err)
		require.False(t, found, "%+v: the hidden note has no place at all", l)
	}
}
