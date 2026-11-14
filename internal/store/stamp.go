package store

// Provenance stamps at the write paths (plan fenced-sessions P6, owner
// decision N2 (a)). A write lands with the stamps the writing conversation's
// state calls for (access.Grants.Stamps, from the flows store.
// ConversationClearance read): one per fenced project in its label other than
// the destination, naming the record that cleared that project's content into
// it. Memories and notes carry them in frontmatter (may_contain), and the files
// layer mirrors the file into item_provenance (ProvenanceMirror); tasks and
// session findings carry them in item_provenance only (StampWorkItem). Nothing
// here reads an agent's input, and nothing here judges a write: the fence
// already did.

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

// provenanceLookupChunk bounds how many item ids one ProvenanceForItems
// statement names.
const provenanceLookupChunk = 500

// StampsOf returns the stamps a write into dest carries for the conversation
// ctx carries (access.GrantsFrom): nil when ctx carries none -- with
// isolation.requests off, for a request no conversation identifies, and for
// a conversation whose label is empty or is dest alone.
func StampsOf(ctx context.Context, dest string) []core.Stamp {
	g, ok := access.GrantsFrom(ctx)
	if !ok {
		return nil
	}
	return StampsOfGrants(g, dest)
}

// StampsOfGrants is StampsOf for a judged state the caller already holds (a
// hook that judged the conversation for its own clearance check).
func StampsOfGrants(g access.Grants, dest string) []core.Stamp {
	flows := g.Stamps(dest)
	if len(flows) == 0 {
		return nil
	}
	out := make([]core.Stamp, 0, len(flows))
	for _, f := range flows {
		out = append(out, core.Stamp{Project: f.Project, Via: f.Via, Ref: f.Ref})
	}
	return out
}

// ConversationStamps judges the conversation ref names for the holder ctx
// speaks for (ConversationClearance) and returns the stamps a write into dest
// carries. It is for a surface that has not judged the conversation already:
// the hooks' record of a conversation (the SessionEnd harvest, Codex Stop
// findings, plan and subagent captures, pushed captures).
func ConversationStamps(ctx context.Context, db *sql.DB, ref ConversationRef, dest string, now time.Time) ([]core.Stamp, error) {
	g, err := ConversationClearance(ctx, db, ref, now)
	if err != nil {
		return nil, fmt.Errorf("store.ConversationStamps: %w", err)
	}
	return StampsOfGrants(g, dest), nil
}

// StampConversationWrite records, on an item of the work record the surface
// is about to write for the conversation ref names -- a session's findings
// from the SessionEnd harvest, Codex Stop or a pushed capture, a task -- the
// stamps that write carries into project (ConversationStamps, then
// StampWorkItem). It is the hooks' one call before such a write; with
// isolation.requests off the hooks never make it. A failure is the caller's
// to report, and the write it precedes should not land unstamped.
func StampConversationWrite(ctx context.Context, db *sql.DB, ref ConversationRef, kind, id, project string, now time.Time) error {
	stamps, err := ConversationStamps(ctx, db, ref, project, now)
	if err != nil {
		return err
	}
	return StampWorkItem(ctx, db, kind, id, project, stamps, now)
}

// StampWorkItem records stamps on an item of the work record -- a task, a
// trial, or a session's findings -- in item_provenance, its only home: one row
// per source project, each keeping the approval it was first stamped with
// (ProvenanceUpsert). A stamp for the item's own project, the global scope or
// no reference is skipped. With no stamps it touches nothing.
func StampWorkItem(ctx context.Context, db *sql.DB, kind, id, project string, stamps []core.Stamp, now time.Time) error {
	const op = "store.StampWorkItem"
	if kind != core.ItemKindTask && kind != core.ItemKindTrial && kind != core.ItemKindSession {
		return fmt.Errorf("%s: %w: %q is not a work-record kind a stamp lives in the index for", op, ErrInvalidProvenance, kind)
	}
	keep := stampRows(kind, id, project, stamps)
	if len(keep) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, s := range keep {
		if err := ProvenanceUpsert(ctx, tx, s, now); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", op, err)
	}
	return nil
}

// stampRows is the index rows stamps make for one item: valid ones only, one
// per source, the first for a source winning. A source and a reference are
// each one token (holderToken): a hand-edited entry carrying a sentence stays
// in its file and out of every answer the index feeds.
func stampRows(kind, id, project string, stamps []core.Stamp) []ProvenanceStamp {
	var out []ProvenanceStamp
	for _, st := range stamps {
		row := ProvenanceStamp{ItemKind: kind, ItemID: id, Project: project, Source: st.Project, ApprovalID: st.Ref}
		if provenanceValid(row) != nil || holderToken("stamp project", st.Project, carryMaxSlugBytes) != nil ||
			holderToken("stamp reference", st.Ref, maxAccessFieldBytes) != nil {
			continue
		}
		if slices.ContainsFunc(out, func(o ProvenanceStamp) bool { return o.Source == row.Source }) {
			continue
		}
		out = append(out, row)
	}
	return out
}

// ProvenanceMirror makes item_provenance hold exactly the stamps a memory's or
// a note's file carries: the file is the source of truth, and the index its
// rebuildable mirror. It runs inside the transaction that indexes the file
// (files.Indexer), on every write and every reindex: a source the file no
// longer names leaves the index -- the owner's hand edit of the file is the
// only way that happens, since no Seamless write removes a stamp -- and one it
// names takes the file's approval and the item's project, keeping the time it
// was first indexed. An entry for the item's own project, one with no project
// or no reference, and one that does not validate stay in the file and out of
// the index.
func ProvenanceMirror(ctx context.Context, ex provenanceExecutor, kind, id, project string, stamps []core.Stamp, now time.Time) error {
	const op = "store.ProvenanceMirror"
	if kind != core.ItemKindMemory && kind != core.ItemKindNote {
		return fmt.Errorf("%s: %w: %q is not a file-backed kind", op, ErrInvalidProvenance, kind)
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%s: %w: no item id", op, ErrInvalidProvenance)
	}
	keep := stampRows(kind, id, project, stamps)
	del, args := `DELETE FROM item_provenance WHERE item_kind = ? AND item_id = ?`, []any{kind, id}
	if len(keep) > 0 {
		del += ` AND source_project NOT IN (` + placeholders(len(keep)) + `)`
		for _, s := range keep {
			args = append(args, s.Source)
		}
	}
	if _, err := ex.ExecContext(ctx, del, args...); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	for _, s := range keep {
		if _, err := ex.ExecContext(ctx, `INSERT INTO item_provenance
			(item_kind, item_id, project_slug, source_project, approval_id, created_at) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (item_kind, item_id, source_project) DO UPDATE SET
			    project_slug = excluded.project_slug, approval_id = excluded.approval_id`,
			s.ItemKind, s.ItemID, s.Project, s.Source, s.ApprovalID, core.FormatTime(now)); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
	return nil
}

// ProvenanceForget drops an item's stamps from the index when the item itself
// leaves it: a memory's or a note's file removed (files.Indexer). A
// revocation or an expiry never calls it.
func ProvenanceForget(ctx context.Context, ex provenanceExecutor, kind, id string) error {
	if _, err := ex.ExecContext(ctx, `DELETE FROM item_provenance WHERE item_kind = ? AND item_id = ?`, kind, id); err != nil {
		return fmt.Errorf("store.ProvenanceForget: %w", err)
	}
	return nil
}

// ProvenanceRelocate keeps a work-record item's stamps on the project it was
// moved to (a task reassigned to another project), so the came-from lists by
// project stay true. A stamp naming the new project as its source says
// nothing about an item now in it, and a row may not name an item's own
// project, so that one row leaves the index; the next write that carries the
// item out of that project stamps it again from the writer's label (a move
// out of a fenced project is a read of it). Memories and notes are mirrored
// from their files instead (ProvenanceMirror).
func ProvenanceRelocate(ctx context.Context, db *sql.DB, kind, id, project string) error {
	const op = "store.ProvenanceRelocate"
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM item_provenance WHERE item_kind = ? AND item_id = ? AND source_project = ?`,
		kind, id, project); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE item_provenance SET project_slug = ? WHERE item_kind = ? AND item_id = ?`,
		project, kind, id); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: commit: %w", op, err)
	}
	return nil
}

// ProvenanceForItems returns the stamps of the items of kind with ids, by id,
// each item's sorted by source, as an item shows them (a recall hit, a task
// answer): one lookup per chunk of ids, nothing for an item with none.
func ProvenanceForItems(ctx context.Context, db *sql.DB, kind string, ids []string) (map[string][]core.Stamp, error) {
	const op = "store.ProvenanceForItems"
	out := map[string][]core.Stamp{}
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	for chunk := range slices.Chunk(ids, provenanceLookupChunk) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, kind)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := provenanceQuery(ctx, db, op, `p.item_kind = ? AND p.item_id IN (`+placeholders(len(chunk))+`)
			ORDER BY p.item_id, p.source_project`, args...)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.ItemID] = append(out[r.ItemID], r.Stamp())
		}
	}
	return out, nil
}
