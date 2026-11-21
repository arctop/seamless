package store

// The session flag and its transcript copy's record (migrations 051 and 052).
// A flag is metadata, like a star: no write here bumps updated_at, so flagging
// a session from an hour ago never reorders a recency sort or the briefing's
// recent findings. The copy itself lives on disk (internal/transcripts); the
// columns record what became of it, and how often a pending one was asked for.

import (
	"context"
	"database/sql"
	"errors"
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
// clearing is ClearSessionFlag. A flag asks for the transcript afresh: the
// asks start over, and a copy that was not made is attempted again (its word
// is cleared; a copy kept stays recorded). It returns the row as it now reads.
func SetSessionFlag(ctx context.Context, db *sql.DB, id string, verdict core.SessionVerdict, reason string, now time.Time) (core.Session, error) {
	if !verdict.Flags() {
		return core.Session{}, fmt.Errorf("store.SetSessionFlag: verdict %q does not flag a session", verdict)
	}
	if err := clearTranscriptAsks(ctx, db, id); err != nil {
		return core.Session{}, fmt.Errorf("store.SetSessionFlag: %w", err)
	}
	sess, found, err := sessionOne(ctx, db, `
		UPDATE sessions SET verdict = ?, verdict_reason = ?, flagged_at = ?,
		       transcript_copy = CASE WHEN transcript_copy = 'copied' THEN transcript_copy END
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
	if err := clearTranscriptAsks(ctx, db, id); err != nil {
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

// transcriptAskedSQL is core.TranscriptCopy.Asked in SQL, over the sessions
// row a statement reads, with its arguments: the copy word is one of
// core.TranscriptAsked ("" is NULL).
func transcriptAskedSQL() (string, []any) {
	var words []any
	for _, c := range core.TranscriptAsked {
		if c != "" {
			words = append(words, string(c))
		}
	}
	return `(transcript_copy IS NULL OR transcript_copy = '' OR transcript_copy IN (` + placeholders(len(words)) + `))`, words
}

// The asks of a pending transcript live in a table of their own (migration
// 052), not on the sessions row: bookkeeping only the pending logic reads,
// kept out of every session read and out of a merge. A Session's
// TranscriptAsks and TranscriptAskedAt are filled by PendingTranscriptSessions
// alone.

// sessionAsksCols are the asks of the sessions row a statement reads, as two
// columns after sessionCols (scanSessionAsks).
const sessionAsksCols = `(SELECT asks FROM session_transcript_asks a WHERE a.session_id = sessions.id),
	(SELECT asked_at FROM session_transcript_asks a WHERE a.session_id = sessions.id)`

// scanSessionAsks scans a sessionCols row followed by sessionAsksCols.
func scanSessionAsks(rows *sql.Rows) (core.Session, error) {
	var (
		asks    sql.NullInt64
		askedAt sql.NullString
	)
	s, err := scanSession(scanExtra{rows: rows, extra: []any{&asks, &askedAt}})
	if err != nil {
		return core.Session{}, err
	}
	s.TranscriptAsks = int(asks.Int64)
	if askedAt.Valid && askedAt.String != "" {
		at, err := core.ParseTime(askedAt.String)
		if err != nil {
			return core.Session{}, fmt.Errorf("transcript asked_at: %w", err)
		}
		s.TranscriptAskedAt = &at
	}
	return s, nil
}

// scanExtra scans a row into the destinations it is given, then extra.
type scanExtra struct {
	rows  *sql.Rows
	extra []any
}

func (s scanExtra) Scan(dest ...any) error { return s.rows.Scan(append(dest, s.extra...)...) }

// clearTranscriptAsks forgets session id's asks: a new flag, a cleared one, a
// copy recorded pending afresh.
func clearTranscriptAsks(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, id string) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM session_transcript_asks WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("clear transcript asks: %w", err)
	}
	return nil
}

// TranscriptAsksOf is how many times session id's transcript was asked for
// since it was last flagged, and when last (nil when never).
func TranscriptAsksOf(ctx context.Context, db *sql.DB, id string) (asks int, at *time.Time, err error) {
	var raw string
	err = db.QueryRowContext(ctx, `SELECT asks, asked_at FROM session_transcript_asks WHERE session_id = ?`, id).Scan(&asks, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("store.TranscriptAsksOf: %w", err)
	}
	t, err := core.ParseTime(raw)
	if err != nil {
		return 0, nil, fmt.Errorf("store.TranscriptAsksOf: asked_at: %w", err)
	}
	return asks, &t, nil
}

// PendingTranscriptSessions returns the flagged sessions of one caller whose
// transcript is still asked for (core.TranscriptCopy.Asked): those an agent
// client's hooks opened (externalClient, as hooks name it), started by
// account ("" for the owner), oldest flag first, each with its asks. The
// caller judges the rest -- the computer, the session in hand, a copy kept on
// disk.
func PendingTranscriptSessions(ctx context.Context, db *sql.DB, externalClient, account string) ([]core.Session, error) {
	if externalClient == "" {
		return nil, nil
	}
	asked, args := transcriptAskedSQL()
	rows, err := db.QueryContext(ctx, `SELECT `+sessionCols+`, `+sessionAsksCols+`
		FROM sessions
		WHERE verdict IS NOT NULL AND external_client = ? AND account = ? AND claude_session_id != ''
		  AND `+asked+`
		ORDER BY flagged_at, ambient DESC, id`, append([]any{externalClient, account}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("store.PendingTranscriptSessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Session
	for rows.Next() {
		s, err := scanSessionAsks(rows)
		if err != nil {
			return nil, fmt.Errorf("store.PendingTranscriptSessions: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.PendingTranscriptSessions: %w", err)
	}
	return out, nil
}

// EndedFlaggedSessions returns the flagged sessions that ended -- completed or
// expired, last updated before cutoff -- whose transcript is still asked for
// (core.TranscriptCopy.Asked), oldest flag first: the reaper's to settle,
// once a session's end signal has had its time to come.
func EndedFlaggedSessions(ctx context.Context, db *sql.DB, cutoff time.Time) ([]core.Session, error) {
	asked, args := transcriptAskedSQL()
	rows, err := db.QueryContext(ctx, `SELECT `+sessionCols+`
		FROM sessions
		WHERE verdict IS NOT NULL AND status != 'active' AND updated_at < ?
		  AND `+asked+`
		ORDER BY flagged_at, id`, append([]any{core.FormatTime(cutoff.UTC())}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("store.EndedFlaggedSessions: %w", err)
	}
	return collectSessions(rows, "store.EndedFlaggedSessions")
}

// collectSessions scans every row of rows and closes them.
func collectSessions(rows *sql.Rows, op string) ([]core.Session, error) {
	defer func() { _ = rows.Close() }()
	var out []core.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// SetTranscriptPending records that flagged session id's transcript is
// pending (core.TranscriptPending): its end signal sent none, so the agent's
// later hooks are asked for it, the asks counted from none. It writes only
// while the session is flagged and its copy still asked for; recorded is
// false otherwise (a copy kept, or a word that stands).
func SetTranscriptPending(ctx context.Context, db *sql.DB, id string) (recorded bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store.SetTranscriptPending: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	asked, args := transcriptAskedSQL()
	res, err := tx.ExecContext(ctx, `
		UPDATE sessions SET transcript_copy = ?
		 WHERE id = ? AND verdict IS NOT NULL AND `+asked,
		append([]any{string(core.TranscriptPending), id}, args...)...)
	if err != nil {
		return false, fmt.Errorf("store.SetTranscriptPending: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store.SetTranscriptPending: rows affected: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	if err := clearTranscriptAsks(ctx, tx, id); err != nil {
		return false, fmt.Errorf("store.SetTranscriptPending: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store.SetTranscriptPending: %w", err)
	}
	return true, nil
}

// RecordTranscriptAsk counts one more ask for flagged session id's
// transcript, at now, and records it pending. asks is the count it now
// holds; recorded is false, and nothing is written, when the session is no
// longer flagged or its copy no longer asked for.
func RecordTranscriptAsk(ctx context.Context, db *sql.DB, id string, now time.Time) (asks int, recorded bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("store.RecordTranscriptAsk: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	asked, args := transcriptAskedSQL()
	res, err := tx.ExecContext(ctx, `
		UPDATE sessions SET transcript_copy = ?
		 WHERE id = ? AND verdict IS NOT NULL AND `+asked,
		append([]any{string(core.TranscriptPending), id}, args...)...)
	if err != nil {
		return 0, false, fmt.Errorf("store.RecordTranscriptAsk: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("store.RecordTranscriptAsk: rows affected: %w", err)
	}
	if n == 0 {
		return 0, false, nil
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO session_transcript_asks (session_id, asks, asked_at) VALUES (?, 1, ?)
		ON CONFLICT (session_id) DO UPDATE SET asks = asks + 1, asked_at = excluded.asked_at
		RETURNING asks`, id, core.FormatTime(now.UTC())).Scan(&asks); err != nil {
		return 0, false, fmt.Errorf("store.RecordTranscriptAsk: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("store.RecordTranscriptAsk: %w", err)
	}
	return asks, true, nil
}

// EndTranscriptAsks records state -- why flagged session id holds no copy --
// only while its copy is still asked for: a copy recorded since, or a word
// that already stands, is left as it is, and recorded is false. The asks
// stop with it: the reaper's and the hooks' word on a pending copy (given up,
// refused, the file gone).
func EndTranscriptAsks(ctx context.Context, db *sql.DB, id string, state core.TranscriptCopy) (recorded bool, err error) {
	if !state.Valid() || state == core.TranscriptCopied {
		return false, fmt.Errorf("store.EndTranscriptAsks: invalid copy state %q", state)
	}
	asked, args := transcriptAskedSQL()
	res, err := db.ExecContext(ctx, `
		UPDATE sessions SET transcript_copy = ?
		 WHERE id = ? AND verdict IS NOT NULL AND `+asked,
		append([]any{string(state), id}, args...)...)
	if err != nil {
		return false, fmt.Errorf("store.EndTranscriptAsks: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store.EndTranscriptAsks: rows affected: %w", err)
	}
	return n > 0, nil
}
