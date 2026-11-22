package store

// The Notes list's place query and a note page's other notes (an item's own
// page, store/itempage.go).

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/arctop/seamless/internal/core"
)

// NoteList is a Notes list's state as a place query reads it.
type NoteList struct {
	ByProject bool
	Project   string
	Fav       bool
	// Query is the text filter, lowercased: a substring of the title, the
	// description, a tag, or the project's slug or name ("global" for the
	// global scope).
	Query string
	// Tag keeps the notes that carry this tag, exactly; Plans the ones
	// tagged plan:<slug> (a slug of one character or more).
	Tag   string
	Plans bool
	// Without leaves out the notes that carry this tag, exactly ("" leaves
	// none out): the console's folded agent working copies.
	Without string
	// Sort is recent, name or favorites.
	Sort string
}

// NotePlace is where note id sits in list l. found is false when the list
// does not hold it.
func NotePlace(ctx context.Context, db *sql.DB, l NoteList, id string) (ItemPlace, bool, error) {
	where := []string{"1 = 1"}
	var args []any
	if l.ByProject {
		where = append(where, "n.project = ?")
		args = append(args, l.Project)
	}
	if l.Fav {
		where = append(where, "n.favorite = 1")
	}
	if q := strings.TrimSpace(l.Query); q != "" {
		needle := likeContains(q)
		where = append(where, `(n.title LIKE ? ESCAPE '\' OR n.description LIKE ? ESCAPE '\' OR n.tags LIKE ? ESCAPE '\'
			OR (n.project <> '' AND (n.project LIKE ? ESCAPE '\' OR COALESCE(p.name, '') LIKE ? ESCAPE '\'))
			OR (n.project = '' AND instr('global', ?) > 0))`)
		args = append(args, needle, needle, needle, needle, needle, q)
	}
	if l.Tag != "" {
		where = append(where, `EXISTS (SELECT 1 FROM json_each(n.tags) je WHERE je.value = ?)`)
		args = append(args, l.Tag)
	}
	if l.Plans {
		// GLOB is case-sensitive, as the console's prefix test is.
		where = append(where, `EXISTS (SELECT 1 FROM json_each(n.tags) je WHERE je.value GLOB 'plan:?*')`)
	}
	if l.Without != "" {
		where = append(where, `NOT EXISTS (SELECT 1 FROM json_each(n.tags) je WHERE je.value = ?)`)
		args = append(args, l.Without)
	}
	inner := `SELECT n.id AS id, n.title AS label, n.favorite AS favorite, n.updated_at AS updated_at
		  FROM notes_index n
		  LEFT JOIN projects p ON p.slug = n.project
		 WHERE ` + strings.Join(where, " AND ")
	var order []string
	switch l.Sort {
	case "name":
		order = append(order, "lower(label)")
	case "favorites":
		order = append(order, "favorite DESC")
	}
	order = append(order, "updated_at DESC", "id DESC")
	place, found, err := itemPlace(ctx, db, placeSQL(inner, strings.Join(order, ", ")), append(args, id), id)
	if err != nil {
		return ItemPlace{}, false, fmt.Errorf("store.NotePlace: %w", err)
	}
	return place, found, nil
}

// NotesOfProject is up to limit of project's notes but the one at except,
// newest-updated first, and how many notes the project holds in all (except
// included). A note carrying the tag without names ("" for none) is left out
// of both, as the Notes list leaves the agent working copies out.
func NotesOfProject(ctx context.Context, db *sql.DB, project, except, without string, limit int) ([]core.Note, int, error) {
	const notWithout = `(? = '' OR NOT EXISTS (SELECT 1 FROM json_each(notes_index.tags) je WHERE je.value = ?))`
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes_index WHERE project = ? AND `+notWithout,
		project, without, without).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store.NotesOfProject: count: %w", err)
	}
	if limit <= 0 || total == 0 {
		return nil, total, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+noteCols+` FROM notes_index
		WHERE project = ? AND id <> ? AND `+notWithout+`
		ORDER BY updated_at DESC, id DESC LIMIT ?`, project, except, without, without, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("store.NotesOfProject: %w", err)
	}
	defer func() { _ = rows.Close() }()
	notes, err := scanNotes(rows)
	if err != nil {
		return nil, 0, fmt.Errorf("store.NotesOfProject: %w", err)
	}
	return notes, total, nil
}
