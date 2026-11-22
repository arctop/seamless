// The console's item pages (plan:console-ux-pass UX 26): where one memory,
// note or task sits in the list a link came from, and the few rows its
// context column names. Each place is one scoped query: a window over the
// rows that list holds, which answers the item's position, the list's length
// and its two neighbours, never the list itself. Read on the request's handle,
// so a locked project's rows are absent from the window as from every list.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// ItemPlace is an item's place in an ordered list: its position from 1, the
// list's length, and the items before and after it (zero at an end).
type ItemPlace struct {
	Pos   int
	Total int
	Prev  ItemStep
	Next  ItemStep
}

// ItemStep is a neighbour in a list: its id and what the list calls it (a
// memory's name, a note's or a task's title).
type ItemStep struct {
	ID    string
	Label string
}

// itemPlace runs a place query: rows is a SELECT of (id, label, pos, total)
// over the scoped list, ordered by pos, holding the item at id and its
// neighbours. found is false when the list does not hold the item.
func itemPlace(ctx context.Context, db *sql.DB, query string, args []any, id string) (ItemPlace, bool, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return ItemPlace{}, false, err
	}
	defer func() { _ = rows.Close() }()
	var (
		out   ItemPlace
		found bool
		steps []struct {
			step ItemStep
			pos  int
		}
	)
	for rows.Next() {
		var (
			step       ItemStep
			pos, total int
		)
		if err := rows.Scan(&step.ID, &step.Label, &pos, &total); err != nil {
			return ItemPlace{}, false, err
		}
		out.Total = total
		if step.ID == id {
			out.Pos, found = pos, true
		}
		steps = append(steps, struct {
			step ItemStep
			pos  int
		}{step, pos})
	}
	if err := rows.Err(); err != nil {
		return ItemPlace{}, false, err
	}
	if !found {
		return ItemPlace{}, false, nil
	}
	for _, s := range steps {
		switch s.pos {
		case out.Pos - 1:
			out.Prev = s.step
		case out.Pos + 1:
			out.Next = s.step
		}
	}
	return out, true, nil
}

// placeSQL wraps a list's rows -- inner, a SELECT of id, label and the
// columns order reads -- in the window that numbers them, and keeps the item
// (bound last) and the rows either side of it.
func placeSQL(inner, order string) string {
	return `WITH s AS (
		SELECT id, label, ROW_NUMBER() OVER (ORDER BY ` + order + `) AS pos
		  FROM (` + inner + `)
	), n AS (SELECT COUNT(*) AS total FROM s)
	SELECT s.id, s.label, s.pos, n.total
	  FROM s, n, (SELECT pos AS at FROM s WHERE id = ?) me
	 WHERE s.pos BETWEEN me.at - 1 AND me.at + 1
	 ORDER BY s.pos`
}

// MemoryList is a Memories list's state as a place query reads it: the
// filters the list applies, which of its two lists the item is in, and its
// order.
type MemoryList struct {
	// ByProject narrows to Project's memories ("" is then the global scope).
	ByProject bool
	Project   string
	// Kind is a core memory kind, "" for every kind.
	Kind string
	// Fav keeps the starred ones, Never the ones that never surfaced.
	Fav   bool
	Never bool
	// Query is the text filter, lowercased: a substring of the name, the
	// description, the kind, a tag, or the project's slug or name ("global"
	// for the global scope).
	Query string
	// Inactive is the archived and superseded list instead of the active one.
	Inactive bool
	// Sort is the list's sort key (recent, name, reach, utility, favorites);
	// Tree orders by project, global first, then by kind in kinds' order
	// before the sort.
	Sort  string
	Tree  bool
	Kinds []string
}

// MemoryPlace is where memory id sits in list l. found is false when the
// list does not hold it.
func MemoryPlace(ctx context.Context, db *sql.DB, l MemoryList, id string) (ItemPlace, bool, error) {
	var (
		where []string
		args  []any
	)
	if l.Inactive {
		where = append(where, "m.invalid_at IS NOT NULL")
	} else {
		where = append(where, "m.invalid_at IS NULL")
	}
	if l.ByProject {
		where = append(where, "m.project = ?")
		args = append(args, l.Project)
	}
	if l.Kind != "" {
		where = append(where, "m.kind = ?")
		args = append(args, l.Kind)
	}
	if l.Fav {
		where = append(where, "m.favorite = 1")
	}
	if l.Never {
		where = append(where, "COALESCE(r.inject_count, 0) = 0")
	}
	if q := strings.TrimSpace(l.Query); q != "" {
		needle := likeContains(q)
		where = append(where, `(m.name LIKE ? ESCAPE '\' OR m.description LIKE ? ESCAPE '\' OR m.kind LIKE ? ESCAPE '\'
			OR m.tags LIKE ? ESCAPE '\'
			OR (m.project <> '' AND (m.project LIKE ? ESCAPE '\' OR COALESCE(p.name, '') LIKE ? ESCAPE '\'))
			OR (m.project = '' AND instr('global', ?) > 0))`)
		args = append(args, needle, needle, needle, needle, needle, needle, q)
	}
	inner := `SELECT m.id AS id, m.name AS label, m.project AS project, m.kind AS kind, m.name AS name,
		m.favorite AS favorite, m.updated_at AS updated_at,
		COALESCE(r.inject_count, 0) AS injects, COALESCE(r.utility, 0) AS utility
		  FROM memories_index m
		  LEFT JOIN retrieval_stats r ON r.item_id = m.id
		  LEFT JOIN projects p ON p.slug = m.project
		 WHERE ` + strings.Join(where, " AND ")
	var order []string
	if l.Tree {
		order = append(order, "(project <> '')", "project", kindRank("kind", l.Kinds))
	}
	switch l.Sort {
	case "name":
		order = append(order, "name")
	case "favorites":
		order = append(order, "favorite DESC", "name")
	case "reach":
		order = append(order, "injects DESC", "name")
	case "utility":
		order = append(order, "utility DESC", "name")
	default:
		order = append(order, "updated_at DESC", "name")
	}
	// Ties keep the store's newest-first read, as the list's stable sorts do.
	order = append(order, "updated_at DESC", "id DESC")
	place, found, err := itemPlace(ctx, db, placeSQL(inner, strings.Join(order, ", ")), append(args, id), id)
	if err != nil {
		return ItemPlace{}, false, fmt.Errorf("store.MemoryPlace: %w", err)
	}
	return place, found, nil
}

// kindRank orders col by kinds' order, a kind it does not name last.
func kindRank(col string, kinds []string) string {
	if len(kinds) == 0 {
		return col
	}
	var b strings.Builder
	b.WriteString("CASE " + col)
	for i, k := range kinds {
		// The kinds are core's own names, never request text.
		fmt.Fprintf(&b, " WHEN '%s' THEN %d", strings.ReplaceAll(k, "'", "''"), i)
	}
	fmt.Fprintf(&b, " ELSE %d END", len(kinds))
	return b.String()
}

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

// TaskPlace is where task id sits in the Tasks list under f: its groups in
// the list's order -- in progress under a live claim, in progress with none,
// ready, blocked, the gates, the steps waiting on a gate, then the closed
// tasks newest-closed first -- each open group oldest-created first (newest
// first with newest, but the gates, which go by who must act). now is the
// instant a claim is judged live at. found is false when the list does not
// hold the task.
func TaskPlace(ctx context.Context, db *sql.DB, f TaskFilter, newest bool, now time.Time, id string) (ItemPlace, bool, error) {
	at := core.FormatTime(now.UTC())
	started := "t.status = 'in_progress' AND t.kind <> 'gate'"
	rank := `CASE
		WHEN ` + started + ` AND ` + sqlClaimLive("t") + ` THEN 0
		WHEN ` + started + ` THEN 1
		WHEN ` + sqlReady("t") + ` THEN 2
		WHEN ` + sqlBlocked("t") + ` THEN 3
		WHEN ` + sqlGates("t") + ` THEN 4
		WHEN ` + sqlGated("t") + ` THEN 5
		WHEN t.status IN ('done','dropped') THEN 6
	END`
	where, args := f.where("t")
	inner := `SELECT id, label, rk, created_at, closed, who FROM (
		SELECT t.id AS id, t.title AS label, ` + rank + ` AS rk, t.created_at AS created_at,
		       COALESCE(t.closed_at, t.updated_at) AS closed, lower(t.gate_who) AS who
		  FROM tasks t
		 WHERE 1 = 1` + where + `
	) WHERE rk IS NOT NULL`
	dir := "ASC"
	if newest {
		dir = "DESC"
	}
	order := `rk,
		CASE WHEN rk = 4 THEN who END,
		CASE WHEN rk = 6 THEN closed END DESC,
		CASE WHEN rk = 6 THEN id END DESC,
		CASE WHEN rk = 4 THEN created_at END,
		CASE WHEN rk = 4 THEN id END,
		created_at ` + dir + `, id ` + dir
	all := append([]any{at}, args...)
	place, found, err := itemPlace(ctx, db, placeSQL(inner, order), append(all, id), id)
	if err != nil {
		return ItemPlace{}, false, fmt.Errorf("store.TaskPlace: %w", err)
	}
	return place, found, nil
}

// MemoriesOfKind is up to limit of project's active memories of kind but
// the one at except, newest-updated first, and how many active memories of
// that kind the project holds in all (except included).
func MemoriesOfKind(ctx context.Context, db *sql.DB, project, kind, except string, limit int) ([]core.Memory, int, error) {
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories_index
		WHERE invalid_at IS NULL AND project = ? AND kind = ?`, project, kind).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store.MemoriesOfKind: count: %w", err)
	}
	if limit <= 0 || total == 0 {
		return nil, total, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+memoryCols+` FROM memories_index
		WHERE invalid_at IS NULL AND project = ? AND kind = ? AND id <> ?
		ORDER BY updated_at DESC, id DESC LIMIT ?`, project, kind, except, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("store.MemoriesOfKind: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, 0, fmt.Errorf("store.MemoriesOfKind: %w", err)
	}
	return mems, total, nil
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
