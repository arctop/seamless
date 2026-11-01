package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// The write-screening tables (migration 028): the held-writes queue, owner
// confirmations, and session taint marks. internal/screening owns the policy;
// this file is only the rows.

// Held-write statuses.
const (
	HeldStatusHeld      = "held"
	HeldStatusReleased  = "released"
	HeldStatusDiscarded = "discarded"
)

// Held-write operations.
const (
	HeldOpWrite    = "write"    // a memory or note lands at Detail["path"]
	HeldOpDelete   = "delete"   // a memory leaves the disk
	HeldOpGardener = "gardener" // a gardener proposal is applied
)

// ErrHoldNotPending is returned when a held write is resolved twice, or does
// not exist: the queue row is no longer (or never was) waiting for the owner.
var ErrHoldNotPending = errors.New("store: held write is not pending")

// HeldWrite is one row of the owner's held-writes queue.
type HeldWrite struct {
	ID         string
	CreatedAt  time.Time
	Status     string
	ResolvedAt *time.Time
	Op         string
	Tool       string
	ItemKind   string // core.ItemKindMemory or core.ItemKindNote
	ItemID     string
	Project    string
	Name       string // memory name or note slug
	MemoryKind core.MemoryKind
	SessionID  string
	Reasons    []string
	BaseHash   string
	Content    string
	Detail     map[string]string
}

// DetailValue returns one op-specific detail value, or "".
func (h HeldWrite) DetailValue(key string) string { return h.Detail[key] }

const heldCols = `id, created_at, status, resolved_at, op, tool, item_kind, item_id,
	project_slug, name, memory_kind, session_id, reasons, base_hash, content, detail`

// InsertHeldWrite records a held write. The caller mints the id.
func InsertHeldWrite(ctx context.Context, db *sql.DB, h HeldWrite) error {
	reasons, err := json.Marshal(nonNilStrings(h.Reasons))
	if err != nil {
		return fmt.Errorf("store.InsertHeldWrite: marshal reasons: %w", err)
	}
	detail := h.Detail
	if detail == nil {
		detail = map[string]string{}
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("store.InsertHeldWrite: marshal detail: %w", err)
	}
	if h.CreatedAt.IsZero() {
		h.CreatedAt = time.Now().UTC()
	}
	_, err = db.ExecContext(ctx, `INSERT INTO held_writes (`+heldCols+`)
		VALUES (?, ?, 'held', '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.ID, core.FormatTime(h.CreatedAt), h.Op, h.Tool, h.ItemKind, h.ItemID,
		h.Project, h.Name, string(h.MemoryKind), h.SessionID, string(reasons),
		h.BaseHash, h.Content, string(detailJSON))
	if err != nil {
		return fmt.Errorf("store.InsertHeldWrite: %w", err)
	}
	return nil
}

// HeldWriteByID returns one held write in any status.
func HeldWriteByID(ctx context.Context, db *sql.DB, id string) (HeldWrite, bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+heldCols+` FROM held_writes WHERE id = ?`, id)
	if err != nil {
		return HeldWrite{}, false, fmt.Errorf("store.HeldWriteByID: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list, err := scanHeldWrites(rows)
	if err != nil {
		return HeldWrite{}, false, fmt.Errorf("store.HeldWriteByID: %w", err)
	}
	if len(list) == 0 {
		return HeldWrite{}, false, nil
	}
	return list[0], true, nil
}

// HeldWrites lists held writes in one status, newest first. limit <= 0 means
// no limit.
func HeldWrites(ctx context.Context, db *sql.DB, status string, limit int) ([]HeldWrite, error) {
	query := `SELECT ` + heldCols + ` FROM held_writes WHERE status = ? ORDER BY created_at DESC, id DESC`
	args := []any{status}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store.HeldWrites: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list, err := scanHeldWrites(rows)
	if err != nil {
		return nil, fmt.Errorf("store.HeldWrites: %w", err)
	}
	return list, nil
}

// PendingHoldsFor lists the pending held writes that land on one item (a
// memory or note id), newest first.
func PendingHoldsFor(ctx context.Context, db *sql.DB, itemID string) ([]HeldWrite, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+heldCols+` FROM held_writes
		WHERE item_id = ? AND status = 'held' ORDER BY created_at DESC, id DESC`, itemID)
	if err != nil {
		return nil, fmt.Errorf("store.PendingHoldsFor: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list, err := scanHeldWrites(rows)
	if err != nil {
		return nil, fmt.Errorf("store.PendingHoldsFor: %w", err)
	}
	return list, nil
}

// PendingHoldsNamed lists the pending held writes of one memory name (or note
// slug) in one project, newest first: what a by-name read miss asks about.
func PendingHoldsNamed(ctx context.Context, db *sql.DB, itemKind, project, name string) ([]HeldWrite, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+heldCols+` FROM held_writes
		WHERE item_kind = ? AND project_slug = ? AND name = ? AND status = 'held'
		ORDER BY created_at DESC, id DESC`, itemKind, project, name)
	if err != nil {
		return nil, fmt.Errorf("store.PendingHoldsNamed: %w", err)
	}
	defer func() { _ = rows.Close() }()
	list, err := scanHeldWrites(rows)
	if err != nil {
		return nil, fmt.Errorf("store.PendingHoldsNamed: %w", err)
	}
	return list, nil
}

// CountPendingHolds counts the held writes waiting for the owner.
func CountPendingHolds(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM held_writes WHERE status = 'held'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store.CountPendingHolds: %w", err)
	}
	return n, nil
}

// ResolveHeldWrite moves a pending held write to released or discarded and
// erases its held text: after a release the text is in the corpus, and after a
// discard it must not survive anywhere. A row that is not pending (already
// resolved, or no such id) is ErrHoldNotPending, so two owners resolving one
// hold at once cannot both win.
func ResolveHeldWrite(ctx context.Context, db *sql.DB, id, status string, at time.Time) error {
	if status != HeldStatusReleased && status != HeldStatusDiscarded {
		return fmt.Errorf("store.ResolveHeldWrite: invalid status %q: valid values are %s, %s",
			status, HeldStatusReleased, HeldStatusDiscarded)
	}
	res, err := db.ExecContext(ctx, `UPDATE held_writes SET status = ?, resolved_at = ?, content = ''
		WHERE id = ? AND status = 'held'`, status, core.FormatTime(at), id)
	if err != nil {
		return fmt.Errorf("store.ResolveHeldWrite: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.ResolveHeldWrite: rows affected: %w", err)
	}
	if n == 0 {
		return ErrHoldNotPending
	}
	return nil
}

func scanHeldWrites(rows *sql.Rows) ([]HeldWrite, error) {
	var out []HeldWrite
	for rows.Next() {
		var h HeldWrite
		var created, resolved, kind, reasons, detail string
		if err := rows.Scan(&h.ID, &created, &h.Status, &resolved, &h.Op, &h.Tool, &h.ItemKind,
			&h.ItemID, &h.Project, &h.Name, &kind, &h.SessionID, &reasons, &h.BaseHash,
			&h.Content, &detail); err != nil {
			return nil, fmt.Errorf("scan held write: %w", err)
		}
		t, err := core.ParseTime(created)
		if err != nil {
			return nil, fmt.Errorf("held write %s: created_at: %w", h.ID, err)
		}
		h.CreatedAt = t
		if resolved != "" {
			rt, err := core.ParseTime(resolved)
			if err != nil {
				return nil, fmt.Errorf("held write %s: resolved_at: %w", h.ID, err)
			}
			h.ResolvedAt = &rt
		}
		h.MemoryKind = core.MemoryKind(kind)
		if err := json.Unmarshal([]byte(reasons), &h.Reasons); err != nil {
			return nil, fmt.Errorf("held write %s: reasons: %w", h.ID, err)
		}
		if err := json.Unmarshal([]byte(detail), &h.Detail); err != nil {
			return nil, fmt.Errorf("held write %s: detail: %w", h.ID, err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("held write rows: %w", err)
	}
	return out, nil
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---------------------------------------------------------------------------
// Owner confirmations
// ---------------------------------------------------------------------------

// ConfirmedViaRelease is owner_confirmations.via for a confirmation the owner
// gave by releasing a held write of the content -- today the only way one is
// given; the column leaves room for others.
const ConfirmedViaRelease = "release"

// UpsertOwnerConfirmation records that the owner confirmed an item's content,
// stamped with its core.MemoryDigest.
func UpsertOwnerConfirmation(ctx context.Context, db *sql.DB, itemID, digest, via string, at time.Time) error {
	_, err := db.ExecContext(ctx, `INSERT INTO owner_confirmations (item_id, digest, confirmed_at, via)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(item_id) DO UPDATE SET digest = excluded.digest,
			confirmed_at = excluded.confirmed_at, via = excluded.via`,
		itemID, digest, core.FormatTime(at), via)
	if err != nil {
		return fmt.Errorf("store.UpsertOwnerConfirmation: %w", err)
	}
	return nil
}

// OwnerConfirmations returns the confirmed digest of each of ids that has
// one. An id with no confirmation is absent from the map.
func OwnerConfirmations(ctx context.Context, db *sql.DB, ids []string) (map[string]string, error) {
	out := make(map[string]string)
	for chunk := range chunked(ids, sqlInChunk) {
		err := eachRow(ctx, db, `SELECT item_id, digest FROM owner_confirmations
			WHERE item_id IN (`+placeholders(len(chunk))+`)`, anySlice(chunk), func(rows *sql.Rows) error {
			var id, digest string
			if err := rows.Scan(&id, &digest); err != nil {
				return err
			}
			out[id] = digest
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("store.OwnerConfirmations: %w", err)
		}
	}
	return out, nil
}

// ContentWriters returns, for each of ids, the session that last changed the
// item's content through Seamless: the session of its newest memory.written
// or note.written event, "" when that write ran in no session (the gardener,
// an unbound connection). An id with no such event is absent from the map.
func ContentWriters(ctx context.Context, db *sql.DB, ids []string) (map[string]string, error) {
	out := make(map[string]string)
	for chunk := range chunked(ids, sqlInChunk) {
		args := append([]any{string(core.EventMemoryWritten), string(core.EventNoteWritten)}, anySlice(chunk)...)
		err := eachRow(ctx, db, `SELECT e.item_id, e.session_id FROM events e
			WHERE e.kind IN (?, ?) AND e.item_id IN (`+placeholders(len(chunk))+`)
			ORDER BY e.item_id, e.ts ASC, e.id ASC`, args, func(rows *sql.Rows) error {
			var id, session string
			if err := rows.Scan(&id, &session); err != nil {
				return err
			}
			out[id] = session // rows arrive oldest-first per item, so the last wins
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("store.ContentWriters: %w", err)
		}
	}
	return out, nil
}

// SessionRef is a session's display name and the project it ran in.
type SessionRef struct {
	Name    string
	Project string
}

// SessionRefs maps session ids to their display names (cc/..., cx/..., or an
// explicit session's name) and projects. Unknown ids are absent.
func SessionRefs(ctx context.Context, db *sql.DB, ids []string) (map[string]SessionRef, error) {
	out := make(map[string]SessionRef)
	for chunk := range chunked(ids, sqlInChunk) {
		err := eachRow(ctx, db, `SELECT id, name, project_slug FROM sessions
			WHERE id IN (`+placeholders(len(chunk))+`)`, anySlice(chunk), func(rows *sql.Rows) error {
			var id string
			var ref SessionRef
			if err := rows.Scan(&id, &ref.Name, &ref.Project); err != nil {
				return err
			}
			out[id] = ref
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("store.SessionRefs: %w", err)
		}
	}
	return out, nil
}

// eachRow runs query and hands each row to fn, closing the rows on every path.
func eachRow(ctx context.Context, db *sql.DB, query string, args []any, fn func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Session taint marks
// ---------------------------------------------------------------------------

// SessionTaint is a session's outside-text mark.
type SessionTaint struct {
	SessionID string
	TaintedAt time.Time
	Reason    string
	Via       string
}

// TaintedSession is a taint mark joined to the session it marks, for the
// owner's list.
type TaintedSession struct {
	SessionTaint
	Name    string
	Project string
	Status  core.SessionStatus
}

// SetSessionTaint marks a session as having read outside text. Marking is
// idempotent and keeps the FIRST mark: a session is tainted from the moment it
// first read outside text, and a later report changes nothing. first is true
// only when this call set the mark.
func SetSessionTaint(ctx context.Context, db *sql.DB, sessionID, reason, via string, at time.Time) (first bool, err error) {
	res, err := db.ExecContext(ctx, `INSERT INTO session_taints (session_id, tainted_at, reason, via)
		VALUES (?, ?, ?, ?) ON CONFLICT(session_id) DO NOTHING`,
		sessionID, core.FormatTime(at), reason, via)
	if err != nil {
		return false, fmt.Errorf("store.SetSessionTaint: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store.SetSessionTaint: rows affected: %w", err)
	}
	return n == 1, nil
}

// ClearSessionTaint removes a session's mark. cleared is false when the
// session carried none.
func ClearSessionTaint(ctx context.Context, db *sql.DB, sessionID string) (cleared bool, err error) {
	res, err := db.ExecContext(ctx, `DELETE FROM session_taints WHERE session_id = ?`, sessionID)
	if err != nil {
		return false, fmt.Errorf("store.ClearSessionTaint: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store.ClearSessionTaint: rows affected: %w", err)
	}
	return n == 1, nil
}

// SessionTaintOf returns a session's mark, if it carries one.
func SessionTaintOf(ctx context.Context, db *sql.DB, sessionID string) (SessionTaint, bool, error) {
	var t SessionTaint
	var at string
	err := db.QueryRowContext(ctx, `SELECT session_id, tainted_at, reason, via FROM session_taints
		WHERE session_id = ?`, sessionID).Scan(&t.SessionID, &at, &t.Reason, &t.Via)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionTaint{}, false, nil
	}
	if err != nil {
		return SessionTaint{}, false, fmt.Errorf("store.SessionTaintOf: %w", err)
	}
	ts, err := core.ParseTime(at)
	if err != nil {
		return SessionTaint{}, false, fmt.Errorf("store.SessionTaintOf: tainted_at: %w", err)
	}
	t.TaintedAt = ts
	return t, true, nil
}

// TaintedSessions lists every marked session that still exists, newest mark
// first.
func TaintedSessions(ctx context.Context, db *sql.DB) ([]TaintedSession, error) {
	rows, err := db.QueryContext(ctx, `SELECT t.session_id, t.tainted_at, t.reason, t.via,
			s.name, s.project_slug, s.status
		FROM session_taints t JOIN sessions s ON s.id = t.session_id
		ORDER BY t.tainted_at DESC, t.session_id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store.TaintedSessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TaintedSession
	for rows.Next() {
		var t TaintedSession
		var at, status string
		if err := rows.Scan(&t.SessionID, &at, &t.Reason, &t.Via, &t.Name, &t.Project, &status); err != nil {
			return nil, fmt.Errorf("store.TaintedSessions: scan: %w", err)
		}
		ts, err := core.ParseTime(at)
		if err != nil {
			return nil, fmt.Errorf("store.TaintedSessions: tainted_at: %w", err)
		}
		t.TaintedAt, t.Status = ts, core.SessionStatus(status)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.TaintedSessions: rows: %w", err)
	}
	return out, nil
}

// sqlInChunk bounds an IN list well under SQLite's variable limit.
const sqlInChunk = 500

// chunked yields ids in slices of at most n, skipping nothing.
func chunked(ids []string, n int) func(func([]string) bool) {
	return func(yield func([]string) bool) {
		for start := 0; start < len(ids); start += n {
			end := min(start+n, len(ids))
			if !yield(ids[start:end]) {
				return
			}
		}
	}
}

func anySlice(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
