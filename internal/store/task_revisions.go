// A task's body history (migration 054): every body a tasks_update replaces,
// or appends to, is kept as a revision, written in the same transaction as the
// change, and a body append is applied inside that transaction too
// (UpdateTaskDetailed), so two appends cannot lose a line.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// TaskBodyRevisionsKept is how many revisions a task keeps. Bodies run to a
// few kilobytes, so twenty revisions of the busiest task cost about a hundred
// kilobytes, and recovering a wiped body needs the newest few. Without a cap a
// task appended to without end would grow its history with the square of its
// body, since each revision an append makes holds the whole body before it.
// Past the cap the write that adds a revision removes one, in its transaction:
// the oldest an append made, whose text the body after it still holds, else
// the oldest replaced. The newest revision -- the body just before the latest
// change -- is never removed.
const TaskBodyRevisionsKept = 20

// ErrTaskBodyConflict is a patch that both replaces the body and appends to
// it: the two orders mean different bodies, so neither is guessed.
var ErrTaskBodyConflict = errors.New("body replaced and appended in one patch")

// ErrTaskBodyAppendBlank is an append of nothing but white space.
var ErrTaskBodyAppendBlank = errors.New("nothing to append: the text is blank")

// checkBody refuses a patch whose body change cannot be applied: an append
// beside a replacement, or an append of nothing.
func (p TaskPatch) checkBody() error {
	if p.AppendBody == nil {
		return nil
	}
	if p.Body != nil {
		return ErrTaskBodyConflict
	}
	if strings.TrimSpace(*p.AppendBody) == "" {
		return ErrTaskBodyAppendBlank
	}
	return nil
}

// appendTaskBody is body with text appended at at: a blank line, then the
// UTC minute, " -- " and text, ending in a newline -- the line notes_append
// adds to a note, so an agent appends to either the same way. An empty body
// takes the line alone, with no blank line before it.
func appendTaskBody(body, text string, at time.Time) string {
	line := at.UTC().Format("2006-01-02 15:04") + " -- " + text + "\n"
	if kept := strings.TrimRight(body, "\n"); kept != "" {
		return kept + "\n\n" + line
	}
	return line
}

// keepBodyRevisionTx records rev as its task's newest revision, numbered one
// past the newest it has, then trims the task to TaskBodyRevisionsKept: the
// oldest revisions an append made go first, then the oldest replaced, and the
// one just written stays. It runs inside the transaction that changed the
// body, so the revision and the change land together or not at all.
func keepBodyRevisionTx(ctx context.Context, tx *sql.Tx, rev core.TaskBodyRevision) error {
	var newest int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision), 0) FROM task_body_revisions WHERE task_id = ?`,
		rev.TaskID).Scan(&newest); err != nil {
		return fmt.Errorf("store: task %q body revision: newest: %w", rev.TaskID, err)
	}
	rev.Revision = newest + 1
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO task_body_revisions (task_id, revision, body, change, replaced_at, replaced_by, updated_account)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rev.TaskID, rev.Revision, rev.Body, string(rev.Change), core.FormatTime(rev.ReplacedAt),
		rev.ReplacedBy, rev.UpdatedAccount); err != nil {
		return fmt.Errorf("store: task %q body revision: insert: %w", rev.TaskID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM task_body_revisions
		 WHERE task_id = ?1 AND revision IN (
			SELECT revision FROM task_body_revisions
			 WHERE task_id = ?1 AND revision < ?2
			 ORDER BY change = 'append' DESC, revision ASC
			 LIMIT max((SELECT COUNT(*) FROM task_body_revisions WHERE task_id = ?1) - ?3, 0))`,
		rev.TaskID, rev.Revision, TaskBodyRevisionsKept); err != nil {
		return fmt.Errorf("store: task %q body revision: trim: %w", rev.TaskID, err)
	}
	return nil
}

// TaskBodyRevisions returns the revisions a task keeps, newest first; none for
// a task that has none, or that names no task. It reads db as given, so on the
// console's filtered handle (OpenView) a task the handle hides has none: the
// revisions are hidden with their task.
func TaskBodyRevisions(ctx context.Context, db *sql.DB, taskID string) ([]core.TaskBodyRevision, error) {
	const op = "store.TaskBodyRevisions"
	var out []core.TaskBodyRevision
	err := eachRow(ctx, db, `SELECT task_id, revision, body, change, replaced_at, replaced_by, updated_account
		FROM task_body_revisions WHERE task_id = ? ORDER BY revision DESC`, []any{taskID}, func(rows *sql.Rows) error {
		var r core.TaskBodyRevision
		var change, at string
		if err := rows.Scan(&r.TaskID, &r.Revision, &r.Body, &change, &at, &r.ReplacedBy, &r.UpdatedAccount); err != nil {
			return err
		}
		r.Change = core.TaskBodyChange(change)
		var err error
		if r.ReplacedAt, err = core.ParseTime(at); err != nil {
			return fmt.Errorf("replaced_at: %w", err)
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s: task %q: %w", op, taskID, err)
	}
	return out, nil
}
