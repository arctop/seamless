package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// Commit records (the commits.record option, off by default): the full commit
// ids a session named at session_end and a task at tasks_update, one row per
// (owner, commit), read back by the commit-matching snapshot
// (internal/commitfeed). Nothing here runs while the option is off: the MCP
// argument that reaches it is not even offered then.

// CommitOwner is what a commit record belongs to.
type CommitOwner string

// The two owners.
const (
	CommitOwnerSession CommitOwner = "session"
	CommitOwnerTask    CommitOwner = "task"
)

// MaxCommitsPerCall bounds the distinct commit ids one session_end or
// tasks_update call may record. An honest session or task names its own few
// commits; a call naming hundreds is an agent pasting a whole history, which
// would vouch for every commit in it.
const MaxCommitsPerCall = 256

// MaxRecordedCommits bounds the commits one session or one task may hold in
// all. It is the snapshot contract's bound on one entry's commits: an owner
// past it would make the whole snapshot unreadable to its consumer, so the
// record refuses to grow past it instead.
const MaxRecordedCommits = 10000

// ErrTooManyCommits is returned when a record would take a session or a task
// past MaxRecordedCommits. Nothing is recorded.
var ErrTooManyCommits = errors.New("store: too many recorded commits")

// ErrSessionNotFound is returned when a session id names no session.
var ErrSessionNotFound = errors.New("store: session not found")

// ErrInvalidCommitID is returned for a commit id that is not a full, lowercase
// one (core.IsCommitID). Callers normalize input with validate.CommitIDs; this
// is the store's own guard behind it.
var ErrInvalidCommitID = errors.New("store: not a full lowercase commit id")

// RecordSessionCommits records ids on a session: full lowercase commit ids,
// each a no-op when the session already holds it. recordedBy is the session
// ULID of the call that records them. It returns how many were new, and
// refuses -- recording nothing -- an unknown session, an invalid id, or a
// record that would hold more than MaxRecordedCommits.
func RecordSessionCommits(ctx context.Context, db *sql.DB, sessionID, recordedBy string, ids []string, now time.Time) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store.RecordSessionCommits: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("store.RecordSessionCommits: %w: %q", ErrSessionNotFound, sessionID)
	case err != nil:
		return 0, fmt.Errorf("store.RecordSessionCommits: %w", err)
	}
	added, err := recordCommitsTx(ctx, tx, CommitOwnerSession, sessionID, recordedBy, ids, now)
	if err != nil {
		return 0, fmt.Errorf("store.RecordSessionCommits: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store.RecordSessionCommits: commit: %w", err)
	}
	return added, nil
}

// recordCommitsTx records ids on one owner inside tx, returning how many were
// new. The bound is checked against what the owner already holds, in the same
// transaction as the inserts, so two racing calls cannot both slip under it.
func recordCommitsTx(ctx context.Context, tx *sql.Tx, owner CommitOwner, ownerID, recordedBy string, ids []string, now time.Time) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	for _, id := range ids {
		if !core.IsCommitID(id) {
			return 0, fmt.Errorf("%w: %q", ErrInvalidCommitID, id)
		}
	}
	held := map[string]bool{}
	var total int
	err := func() error {
		rows, err := tx.QueryContext(ctx, `SELECT commit_id FROM commit_records WHERE owner_kind = ? AND owner_id = ?`,
			string(owner), ownerID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			held[id] = true
			total++
		}
		return rows.Err()
	}()
	if err != nil {
		return 0, fmt.Errorf("read recorded commits: %w", err)
	}
	var fresh []string
	for _, id := range ids {
		if !held[id] {
			held[id] = true
			fresh = append(fresh, id)
		}
	}
	if total+len(fresh) > MaxRecordedCommits {
		return 0, fmt.Errorf("%w: the %s already holds %d and this records %d more, past the %d one %s may hold",
			ErrTooManyCommits, owner, total, len(fresh), MaxRecordedCommits, owner)
	}
	at := core.FormatTime(now)
	for _, id := range fresh {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO commit_records (owner_kind, owner_id, commit_id, recorded_by, recorded_at)
			VALUES (?, ?, ?, ?, ?)`, string(owner), ownerID, id, recordedBy, at); err != nil {
			return 0, fmt.Errorf("insert commit record: %w", err)
		}
	}
	return len(fresh), nil
}

// RecordedCommits returns the commits recorded on each of ids (sessions or
// tasks, by owner), oldest record first. An id with none is absent.
func RecordedCommits(ctx context.Context, db *sql.DB, owner CommitOwner, ids []string) (map[string][]string, error) {
	out := map[string][]string{}
	const chunkSize = 500 // stay well under SQLite's bound-parameter limit
	for start := 0; start < len(ids); start += chunkSize {
		chunk := ids[start:min(start+chunkSize, len(ids))]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, string(owner))
		for _, id := range chunk {
			args = append(args, id)
		}
		err := eachRow(ctx, db, `SELECT owner_id, commit_id FROM commit_records
			WHERE owner_kind = ? AND owner_id IN (`+placeholders(len(chunk))+`)
			ORDER BY owner_id, recorded_at, commit_id`, args, func(rows *sql.Rows) error {
			var ownerID, commitID string
			if err := rows.Scan(&ownerID, &commitID); err != nil {
				return err
			}
			out[ownerID] = append(out[ownerID], commitID)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("store.RecordedCommits: %w", err)
		}
	}
	return out, nil
}

// FeedSession is a session as the commit-matching snapshot reads it.
type FeedSession struct {
	ID          string
	Name        string
	ProjectSlug string
	// Account is the restricted account that started the session
	// (core.Session.Account), "" for the owner's.
	Account   string
	Active    bool
	CWD       string
	Host      string
	CreatedAt time.Time
	// UpdatedAt is the session's last activity. For a session that is no
	// longer active it is when it ended: session_end and the SessionEnd hook
	// stamp it as they complete the row, and the idle reaper leaves it at the
	// last sign of life when it expires one.
	UpdatedAt time.Time
}

// FeedSessions returns every session that was live at some point between
// since and until: started by until, and still active or last active at or
// after since. Oldest first.
func FeedSessions(ctx context.Context, db *sql.DB, since, until time.Time) ([]FeedSession, error) {
	var out []FeedSession
	err := eachRow(ctx, db, `SELECT id, name, project_slug, account, status, cwd, host, created_at, updated_at
		FROM sessions
		WHERE created_at <= ? AND (status = 'active' OR updated_at >= ?)
		ORDER BY created_at, id`, []any{core.FormatTime(until), core.FormatTime(since)}, func(rows *sql.Rows) error {
		var s FeedSession
		var status, created, updated string
		if err := rows.Scan(&s.ID, &s.Name, &s.ProjectSlug, &s.Account, &status, &s.CWD, &s.Host, &created, &updated); err != nil {
			return err
		}
		s.Active = core.SessionStatus(status) == core.SessionActive
		var err error
		if s.CreatedAt, err = core.ParseTime(created); err != nil {
			return fmt.Errorf("session %s created_at: %w", s.ID, err)
		}
		if s.UpdatedAt, err = core.ParseTime(updated); err != nil {
			return fmt.Errorf("session %s updated_at: %w", s.ID, err)
		}
		out = append(out, s)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store.FeedSessions: %w", err)
	}
	return out, nil
}

// FeedTask is a task as the commit-matching snapshot reads it.
type FeedTask struct {
	ID          string
	ProjectSlug string
	// Account is the restricted account that created the task and
	// UpdatedAccount the one whose words its text may still carry
	// (core.Task.Account, UpdatedAccount); "" for the owner.
	Account        string
	UpdatedAccount string
	UpdatedAt      time.Time
}

// FeedTasks returns the tasks a commit seen at or after since may name: every
// open or in-progress task, and every task changed at or after since (closing
// a task and recording its commits both change it). Most recently changed
// first, at most limit (limit <= 0 is no bound).
func FeedTasks(ctx context.Context, db *sql.DB, since time.Time, limit int) ([]FeedTask, error) {
	query := `SELECT id, project_slug, account, updated_account, updated_at FROM tasks
		WHERE status IN ('open', 'in_progress') OR updated_at >= ?
		ORDER BY updated_at DESC, id`
	args := []any{core.FormatTime(since)}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	var out []FeedTask
	err := eachRow(ctx, db, query, args, func(rows *sql.Rows) error {
		var t FeedTask
		var updated string
		if err := rows.Scan(&t.ID, &t.ProjectSlug, &t.Account, &t.UpdatedAccount, &updated); err != nil {
			return err
		}
		var err error
		if t.UpdatedAt, err = core.ParseTime(updated); err != nil {
			return fmt.Errorf("task %s updated_at: %w", t.ID, err)
		}
		out = append(out, t)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store.FeedTasks: %w", err)
	}
	return out, nil
}

// TaskCommitAccounts maps each of ids (tasks) that a restricted account
// recorded commits on to that account: the account of the session that made
// the task's earliest such record (commit_records.recorded_by names the
// recording call's session). A task whose commits only the owner recorded, or
// a call with no session, is absent. A session's own commits need no lookup:
// only the principal that started a session may end it, so they are always
// its account's.
func TaskCommitAccounts(ctx context.Context, db *sql.DB, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for chunk := range chunked(ids, sqlInChunk) {
		err := eachRow(ctx, db, `SELECT c.owner_id, s.account FROM commit_records c
			JOIN sessions s ON s.id = c.recorded_by
			WHERE c.owner_kind = ? AND c.owner_id IN (`+placeholders(len(chunk))+`) AND s.account <> ''
			ORDER BY c.owner_id, c.recorded_at, c.commit_id`, append([]any{string(CommitOwnerTask)}, anySlice(chunk)...),
			func(rows *sql.Rows) error {
				var task, account string
				if err := rows.Scan(&task, &account); err != nil {
					return err
				}
				if _, seen := out[task]; !seen {
					out[task] = account
				}
				return nil
			})
		if err != nil {
			return nil, fmt.Errorf("store.TaskCommitAccounts: %w", err)
		}
	}
	return out, nil
}

// EarliestSessionStart returns when the oldest session started: where the
// record of sessions begins. ok is false when there is no session at all.
func EarliestSessionStart(ctx context.Context, db *sql.DB) (time.Time, bool, error) {
	var first sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT MIN(created_at) FROM sessions`).Scan(&first); err != nil {
		return time.Time{}, false, fmt.Errorf("store.EarliestSessionStart: %w", err)
	}
	if !first.Valid || first.String == "" {
		return time.Time{}, false, nil
	}
	t, err := core.ParseTime(first.String)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store.EarliestSessionStart: %w", err)
	}
	return t, true, nil
}
