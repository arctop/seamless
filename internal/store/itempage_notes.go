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
	// Sort is recent, name or favorites; Tree orders by project, global
	// first, before it.
	Sort string
	Tree bool
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
	inner := `SELECT n.id AS id, n.title AS label, n.project AS project, n.favorite AS favorite,
		n.updated_at AS updated_at
		  FROM notes_index n
		  LEFT JOIN projects p ON p.slug = n.project
		 WHERE ` + strings.Join(where, " AND ")
	var order []string
	if l.Tree {
		order = append(order, "(project <> '')", "project")
	}
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
// included).
func NotesOfProject(ctx context.Context, db *sql.DB, project, except string, limit int) ([]core.Note, int, error) {
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes_index WHERE project = ?`, project).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store.NotesOfProject: count: %w", err)
	}
	if limit <= 0 || total == 0 {
		return nil, total, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+noteCols+` FROM notes_index
		WHERE project = ? AND id <> ?
		ORDER BY updated_at DESC, id DESC LIMIT ?`, project, except, limit)
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
