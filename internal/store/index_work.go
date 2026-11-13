// FTS mirroring for the work record: tasks, trials, and session findings.
//
// The knowledge half of the corpus (memories, notes) is file-backed, so
// internal/files owns its fts rows and refreshes them from the watcher +
// startup reconcile. Tasks, trials, and sessions have no file behind them --
// the DB row IS the item -- so their mirror is maintained here, next to the
// writers, and every writer of an indexed field calls one of these.
//
// Two deliberate asymmetries with the knowledge kinds:
//
//   - No embeddings. There is no content_hash reconcile loop for a DB-native
//     row, so vectorizing one would mean a provider round-trip inside
//     tasks_add/trial_record/session_end and no way to backfill the rows that
//     predate it. These kinds are lexical-only; recall's cosine leg simply
//     finds no rows of their kind, which also means a work-record hit can never
//     out-rank a fused memory at equal lexical rank.
//   - No validity filter. A done or dropped task is exactly the row an agent
//     needs to find ("why was this not done?"), so closed work stays indexed.
//     The hit carries its status instead.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/arctop/seamless/internal/core"
)

// ftsExecutor is the write subset shared by *sql.DB and *sql.Tx, so a work-record
// index refresh runs either on the pool or inside the caller's transaction --
// CreateTask indexes inside its own tx, UpdateAmbientFindings on the pool.
type ftsExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// IndexTaskFTS refreshes the FTS row for a task. Title and body are the searched
// text; the plan slug rides in the `name` column so a plan-step task is findable
// by its composition, which is the half of a plan that notes do not cover.
func IndexTaskFTS(ctx context.Context, ex ftsExecutor, t core.Task) error {
	return ftsUpsertWork(ctx, ex, t.ID, core.ItemKindTask, t.ProjectSlug,
		t.Title, t.PlanSlug, "", t.Body)
}

// IndexTrialFTS refreshes the FTS row for a trial. The lab rides in `name` and
// the outcome in `description`, so "the boot-race lab" and "what failed" both
// reach the trial without reading every body; changes/expected/actual are joined
// into the body, which is the expected-vs-actual prose worth searching.
func IndexTrialFTS(ctx context.Context, ex ftsExecutor, tr core.Trial) error {
	body := strings.Join(nonEmpty(tr.Changes, tr.Expected, tr.Actual), "\n")
	return ftsUpsertWork(ctx, ex, tr.ID, core.ItemKindTrial, tr.ProjectSlug,
		tr.Title, tr.Lab, string(tr.Outcome), body)
}

// IndexSessionFTS refreshes the FTS row for a session's findings, and removes it
// when there are none worth searching. A session is indexed for its findings
// alone -- the handoff prose the next agent is meant to inherit -- so a session
// with none (or only the no-summary sentinel) carries no row at all, which keeps
// the corpus to sessions that actually said something rather than one row per
// process start.
func IndexSessionFTS(ctx context.Context, ex ftsExecutor, s core.Session) error {
	if !SessionFindingsIndexable(s.Findings) {
		return DeleteWorkFTS(ctx, ex, s.ID)
	}
	return ftsUpsertWork(ctx, ex, s.ID, core.ItemKindSession, s.ProjectSlug,
		s.Name, "", "", s.Findings)
}

// SessionFindingsIndexable reports whether findings text is worth indexing:
// non-blank and not the auto-harvest sentinel. Migration 024's backfill applies
// the same predicate in SQL.
func SessionFindingsIndexable(findings string) bool {
	trimmed := strings.TrimSpace(findings)
	return trimmed != "" && trimmed != core.FindingNoSummary
}

// DeleteWorkFTS drops the FTS row for an item id. It is a no-op when there is
// none, so callers do not have to know whether the item was ever indexed.
func DeleteWorkFTS(ctx context.Context, ex ftsExecutor, itemID string) error {
	if _, err := ex.ExecContext(ctx, `DELETE FROM fts WHERE item_id = ?`, itemID); err != nil {
		return fmt.Errorf("store: work fts delete: %w", err)
	}
	return nil
}

// ftsUpsertWork replaces an item's FTS row. The unified fts table is
// self-contained (no content triggers, not external-content), so delete +
// insert is the upsert -- the same shape files.ftsUpsert uses for the knowledge
// kinds.
func ftsUpsertWork(ctx context.Context, ex ftsExecutor, itemID, kind, project, title, name, description, body string) error {
	if itemID == "" {
		return fmt.Errorf("store: work fts upsert: %s has no id", kind)
	}
	if err := DeleteWorkFTS(ctx, ex, itemID); err != nil {
		return err
	}
	if _, err := ex.ExecContext(ctx, `
		INSERT INTO fts (item_id, kind, project, title, name, description, body)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		itemID, kind, project, title, name, description, body); err != nil {
		return fmt.Errorf("store: work fts insert: %w", err)
	}
	return nil
}

// nonEmpty returns the non-blank arguments, so joining them cannot produce runs
// of blank lines for a trial that recorded only some of its fields.
func nonEmpty(parts ...string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// reindexSessionFTSByExternal refreshes the FTS row for the ambient session
// identified by an external client + session id. UpdateAmbientFindings writes
// by that identity rather than by id, so the mirror has to look the id up; the
// FTS table is a rebuildable mirror, so this deliberately runs after (not
// inside) the targeted write rather than turning it into a transaction. Only
// the ambient row qualifies: an explicit session that session_start linked to
// the conversation shares the identity, and reindexing it instead would leave
// the row just written stale in search.
func reindexSessionFTSByExternal(ctx context.Context, db *sql.DB, externalClient, externalSessionID string) error {
	var s core.Session
	err := db.QueryRowContext(ctx, `
		SELECT id, name, project_slug, findings FROM sessions
		 WHERE external_client = ? AND claude_session_id = ? AND ambient = 1
		 ORDER BY updated_at DESC LIMIT 1`,
		externalClient, externalSessionID).Scan(&s.ID, &s.Name, &s.ProjectSlug, &s.Findings)
	if err != nil {
		return fmt.Errorf("store: reindex session fts: %w", err)
	}
	return IndexSessionFTS(ctx, db, s)
}

// workFTSChunk bounds one by-id load; the by-id loaders build an IN (...) list,
// so an unbounded merge could otherwise blow past SQLite's bound-parameter
// limit. 500 matches the chunk size ActiveSessionsByIDs already uses.
const workFTSChunk = 500

// ReindexWorkFTS refreshes the unified fts rows for the named work-record ids.
//
// It exists for bulk row writes that bypass the normal writers -- the archive
// merge import INSERTs task/trial/session rows straight into the tables, so
// nothing on that path calls IndexTaskFTS/IndexTrialFTS/IndexSessionFTS and the
// imported work record would be present but unsearchable. Keeping the reindex
// here rather than in the importer keeps the column mapping next to its writers
// (AGENTS.md: each half of the fts table is maintained next to its own writer).
//
// Ids with no row are skipped, not an error: the caller passes the ids it tried
// to insert, and INSERT OR IGNORE means some of them are legitimately already
// present under another row or were never inserted at all. Rows that do exist
// are reindexed unconditionally -- an fts row is a rebuildable mirror, so
// rewriting one that was already correct costs a delete + insert and nothing
// else.
//
// The loads are batched (TasksByIDs/TrialsByIDs/SessionsByIDs) rather than one
// query per id: a merge reindexes every imported id at once.
func ReindexWorkFTS(ctx context.Context, db *sql.DB, taskIDs, trialIDs, sessionIDs []string) error {
	for chunk := range slices.Chunk(taskIDs, workFTSChunk) {
		byID, err := TasksByIDs(ctx, db, chunk)
		if err != nil {
			return fmt.Errorf("store.ReindexWorkFTS: %w", err)
		}
		for _, id := range chunk {
			t, ok := byID[id]
			if !ok {
				continue
			}
			if err := IndexTaskFTS(ctx, db, t); err != nil {
				return fmt.Errorf("store.ReindexWorkFTS: task %s: %w", id, err)
			}
		}
	}

	for chunk := range slices.Chunk(trialIDs, workFTSChunk) {
		byID, err := TrialsByIDs(ctx, db, chunk)
		if err != nil {
			return fmt.Errorf("store.ReindexWorkFTS: %w", err)
		}
		for _, id := range chunk {
			tr, ok := byID[id]
			if !ok {
				continue
			}
			if err := IndexTrialFTS(ctx, db, tr); err != nil {
				return fmt.Errorf("store.ReindexWorkFTS: trial %s: %w", id, err)
			}
		}
	}

	for chunk := range slices.Chunk(sessionIDs, workFTSChunk) {
		byID, err := SessionsByIDs(ctx, db, chunk)
		if err != nil {
			return fmt.Errorf("store.ReindexWorkFTS: %w", err)
		}
		for _, id := range chunk {
			s, ok := byID[id]
			if !ok {
				continue
			}
			// IndexSessionFTS removes the row when the findings are not worth
			// indexing, which is the right outcome for an imported session that
			// carries none.
			if err := IndexSessionFTS(ctx, db, s); err != nil {
				return fmt.Errorf("store.ReindexWorkFTS: session %s: %w", id, err)
			}
		}
	}
	return nil
}
