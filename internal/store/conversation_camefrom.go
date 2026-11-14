package store

// A project page's "came from <project>" filter (plan fenced-sessions P10),
// over the provenance index (item_provenance, migration 039): which of a
// project's memories, notes and tasks may contain content from another
// project. A stamp counts only with its item, an item of that kind that is in
// the stamp's project now: a row can outlive a task deleted outside Seamless,
// and a filter that counted it would promise an item it cannot show. Read
// through a filtered handle, an item the handle hides does not count either.

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/arctop/seamless/internal/core"
)

// cameFromMaxItems bounds the items one came-from filter lists.
const cameFromMaxItems = 1000

// CameFromKinds are the item kinds a project page filters by where they came
// from: the ones its tabs list.
var CameFromKinds = []string{core.ItemKindMemory, core.ItemKindNote, core.ItemKindTask}

// cameFromItemExists keeps a stamp row p whose item exists, of its kind, in
// the stamp's project.
const cameFromItemExists = `(
	(p.item_kind = '` + core.ItemKindMemory + `' AND EXISTS (SELECT 1 FROM memories_index m WHERE m.id = p.item_id AND m.project = p.project_slug))
	OR (p.item_kind = '` + core.ItemKindNote + `' AND EXISTS (SELECT 1 FROM notes_index n WHERE n.id = p.item_id AND n.project = p.project_slug))
	OR (p.item_kind = '` + core.ItemKindTask + `' AND EXISTS (SELECT 1 FROM tasks t WHERE t.id = p.item_id AND t.project_slug = p.project_slug)))`

// CameFromSource is one project whose content items of a project may
// contain, with how many of each kind (CameFromKinds) say so.
type CameFromSource struct {
	Project string
	// Counts is the number of items of each kind, by core.ItemKind*.
	Counts map[string]int
}

// CameFromSources returns the projects the memories, notes and tasks of
// project may contain content from, sorted by project, each with its counts:
// the choices of the project page's came-from filter. With no stamped item it
// returns none.
func CameFromSources(ctx context.Context, db *sql.DB, project string) ([]CameFromSource, error) {
	var out []CameFromSource
	err := eachRow(ctx, db, `SELECT p.source_project, p.item_kind, COUNT(*) FROM item_provenance p
		WHERE p.project_slug = ? AND `+cameFromItemExists+`
		GROUP BY p.source_project, p.item_kind ORDER BY p.source_project, p.item_kind`, []any{project},
		func(rows *sql.Rows) error {
			var (
				source, kind string
				n            int
			)
			if err := rows.Scan(&source, &kind, &n); err != nil {
				return err
			}
			if len(out) == 0 || out[len(out)-1].Project != source {
				out = append(out, CameFromSource{Project: source, Counts: map[string]int{}})
			}
			out[len(out)-1].Counts[kind] = n
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("store.CameFromSources: %w", err)
	}
	return out, nil
}

// CameFromItems returns the ids of the memories, notes and tasks of project
// that may contain content from source, by kind (CameFromKinds), each list
// newest stamp first; at most cameFromMaxItems in all. It is the project
// page's came-from filter: what a tab shows while it is on.
func CameFromItems(ctx context.Context, db *sql.DB, project, source string) (map[string][]string, error) {
	out := map[string][]string{}
	err := eachRow(ctx, db, `SELECT p.item_kind, p.item_id FROM item_provenance p
		WHERE p.project_slug = ? AND p.source_project = ? AND `+cameFromItemExists+`
		ORDER BY p.created_at DESC, p.item_kind, p.item_id LIMIT `+fmt.Sprint(cameFromMaxItems), []any{project, source},
		func(rows *sql.Rows) error {
			var kind, id string
			if err := rows.Scan(&kind, &id); err != nil {
				return err
			}
			if slices.Contains(CameFromKinds, kind) {
				out[kind] = append(out[kind], id)
			}
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("store.CameFromItems: %w", err)
	}
	return out, nil
}
