package store

// The session flag and its transcript copy's record (migration 051). A flag
// is metadata, like a star: no write here bumps updated_at, so flagging a
// session from an hour ago never reorders a recency sort or the briefing's
// recent findings. The copy itself lives on disk (internal/transcripts); the
// columns record what became of it.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// sessionFlagCols are the migration-051 columns every session read scans, in
// sessionFlagScan's order.
const sessionFlagCols = `transcript_path, verdict, verdict_reason, flagged_at,
	transcript_copy, transcript_bytes, transcript_copied_at`

// sessionFlagScan holds the nullable flag columns of one scanned row.
type sessionFlagScan struct {
	path, verdict, reason, flaggedAt, copy, copiedAt sql.NullString
	bytes                                            sql.NullInt64
}

// apply copies the scanned flag columns onto s.
func (f sessionFlagScan) apply(s *core.Session) error {
	s.TranscriptPath = f.path.String
	s.Verdict = core.SessionVerdict(f.verdict.String)
	s.VerdictReason = f.reason.String
	s.Transcript = core.TranscriptCopy(f.copy.String)
	s.TranscriptBytes = f.bytes.Int64
	for _, t := range []struct {
		raw  sql.NullString
		dst  **time.Time
		name string
	}{{f.flaggedAt, &s.FlaggedAt, "flagged_at"}, {f.copiedAt, &s.TranscriptCopiedAt, "transcript_copied_at"}} {
		if !t.raw.Valid || t.raw.String == "" {
			continue
		}
		at, err := core.ParseTime(t.raw.String)
		if err != nil {
			return fmt.Errorf("%s: %w", t.name, err)
		}
		*t.dst = &at
	}
	return nil
}

// nullIfEmpty is s as a nullable column value: NULL for "".
func nullIfEmpty(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// SetSessionTranscriptPath records the transcript file a session's hooks name.
// It runs at every SessionStart, so a resumed conversation's path stays
// current; an empty path (a client that sent none) keeps what is recorded, and
// an unchanged one writes nothing. Targeted, like the other single-column
// hook writes, and it never bumps updated_at.
func SetSessionTranscriptPath(ctx context.Context, db *sql.DB, id, path string) error {
	if id == "" || path == "" {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE sessions SET transcript_path = ? WHERE id = ? AND transcript_path IS NOT ?`,
		path, id, path); err != nil {
		return fmt.Errorf("store.SetSessionTranscriptPath: %w", err)
	}
	return nil
}

// SetSessionFlag flags session id with verdict and reason ("" for none) at
// now: one flag per session, so a second flag replaces the first. A verdict
// that does not flag (none, or anything not in core.FlagVerdicts) is refused:
// clearing is ClearSessionFlag. It returns the row as it now reads.
func SetSessionFlag(ctx context.Context, db *sql.DB, id string, verdict core.SessionVerdict, reason string, now time.Time) (core.Session, error) {
	if !verdict.Flags() {
		return core.Session{}, fmt.Errorf("store.SetSessionFlag: verdict %q does not flag a session", verdict)
	}
	sess, found, err := sessionOne(ctx, db, `
		UPDATE sessions SET verdict = ?, verdict_reason = ?, flagged_at = ?
		 WHERE id = ?
		 RETURNING `+sessionCols,
		string(verdict), nullIfEmpty(reason), core.FormatTime(now.UTC()), id)
	if err != nil {
		return core.Session{}, fmt.Errorf("store.SetSessionFlag: %w", err)
	}
	if !found {
		return core.Session{}, fmt.Errorf("store.SetSessionFlag: %w: %q", ErrSessionNotFound, id)
	}
	return sess, nil
}

// ClearSessionFlag clears session id's flag and the record of its transcript
// copy (the caller removes the file). cleared is false, and nothing is
// written, when the session carries no flag; prior is the row as it read
// before the clear (its verdict is the one cleared). A missing session is
// ErrSessionNotFound.
func ClearSessionFlag(ctx context.Context, db *sql.DB, id string) (prior core.Session, cleared bool, err error) {
	prior, found, err := SessionByID(ctx, db, id)
	if err != nil {
		return core.Session{}, false, fmt.Errorf("store.ClearSessionFlag: %w", err)
	}
	if !found {
		return core.Session{}, false, fmt.Errorf("store.ClearSessionFlag: %w: %q", ErrSessionNotFound, id)
	}
	res, err := db.ExecContext(ctx, `
		UPDATE sessions SET verdict = NULL, verdict_reason = NULL, flagged_at = NULL,
		       transcript_copy = NULL, transcript_bytes = NULL, transcript_copied_at = NULL
		 WHERE id = ? AND verdict IS NOT NULL`, id)
	if err != nil {
		return core.Session{}, false, fmt.Errorf("store.ClearSessionFlag: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return core.Session{}, false, fmt.Errorf("store.ClearSessionFlag: rows affected: %w", err)
	}
	return prior, n > 0, nil
}

// SetTranscriptCopy records what became of a flagged session's transcript
// copy: copied, with the transcript's size (bytes) and the time, or the word
// saying why none was made (which leaves the size and time alone). It writes
// only while the session is still flagged: a copy that finishes after the
// flag was cleared records nothing, and recorded is false. A caller whose
// re-copy fails while an earlier copy is still on disk records nothing here:
// the earlier copy is the one kept, and the row says so.
func SetTranscriptCopy(ctx context.Context, db *sql.DB, id string, state core.TranscriptCopy, bytes int64, now time.Time) (recorded bool, err error) {
	if !state.Valid() {
		return false, fmt.Errorf("store.SetTranscriptCopy: invalid copy state %q", state)
	}
	var res sql.Result
	if state == core.TranscriptCopied {
		res, err = db.ExecContext(ctx, `
			UPDATE sessions SET transcript_copy = ?, transcript_bytes = ?, transcript_copied_at = ?
			 WHERE id = ? AND verdict IS NOT NULL`,
			string(state), bytes, core.FormatTime(now.UTC()), id)
	} else {
		res, err = db.ExecContext(ctx, `
			UPDATE sessions SET transcript_copy = ? WHERE id = ? AND verdict IS NOT NULL`,
			string(state), id)
	}
	if err != nil {
		return false, fmt.Errorf("store.SetTranscriptCopy: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store.SetTranscriptCopy: rows affected: %w", err)
	}
	return n > 0, nil
}

// FlaggedSessionsByExternalIdentity returns every flagged session a client's
// session id names, whatever its status -- the ambient row and any explicit
// session linked to it -- ambient first. A SessionEnd or Stop snapshots each
// of them: an agent that closed its session with the session_end tool has
// already completed the row the hook's end still snapshots.
func FlaggedSessionsByExternalIdentity(ctx context.Context, db *sql.DB, externalClient, externalSessionID string) ([]core.Session, error) {
	if externalClient == "" || externalSessionID == "" {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+sessionCols+`
		FROM sessions
		WHERE verdict IS NOT NULL AND external_client = ? AND claude_session_id = ?
		ORDER BY ambient DESC, updated_at DESC, id DESC`, externalClient, externalSessionID)
	if err != nil {
		return nil, fmt.Errorf("store.FlaggedSessionsByExternalIdentity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Session
	for rows.Next() {
		s, serr := scanSession(rows)
		if serr != nil {
			return nil, fmt.Errorf("store.FlaggedSessionsByExternalIdentity: %w", serr)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.FlaggedSessionsByExternalIdentity: %w", err)
	}
	return out, nil
}

// TranscriptPathOf is the transcript a session's copy is made from: its own
// recorded path, or -- for an explicit session linked to an agent client's
// session id, which no SessionStart names -- the path recorded on that id's
// ambient row. "" when neither names one.
func TranscriptPathOf(ctx context.Context, db *sql.DB, sess core.Session) (string, error) {
	if sess.TranscriptPath != "" || sess.Ambient || sess.ExternalClient == "" || sess.ExternalSessionID == "" {
		return sess.TranscriptPath, nil
	}
	amb, found, err := AmbientSessionByExternalIdentity(ctx, db, sess.ExternalClient, sess.ExternalSessionID)
	if err != nil {
		return "", fmt.Errorf("store.TranscriptPathOf: %w", err)
	}
	if !found {
		return "", nil
	}
	return amb.TranscriptPath, nil
}

// CountFlaggedSessions counts the flagged sessions: the console shows its flag
// filter only once there is one.
func CountFlaggedSessions(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE verdict IS NOT NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store.CountFlaggedSessions: %w", err)
	}
	return n, nil
}
