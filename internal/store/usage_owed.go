package store

// Owed usage (migration 057): a Claude Code conversation whose ambient row
// ended without its SessionEnd hook's harvest -- the agent closed it with
// session_end, or the idle reaper expired it -- so the turns since the
// ledger's last write are on its transcript alone, and SessionEnd may come
// late or never (the Claude app runs none for a session it stops). The row
// says so until the transcript is read: internal/gardener reads it where the
// daemon may read the file (owed_usage.go), and under peer_auth the same
// caller's later hooks are asked for it (internal/hooks usage_owed.go). A
// SessionEnd harvest settles it (ClearConversationUsageOwed), and so does the
// row going active again: its own next end marks it again.
//
// It is bookkeeping, like the transcript asks (migration 052): a table of its
// own rather than session columns every read would scan, and left out of a
// merge.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// UsageOwedClient is the one client whose usage can be owed: Claude Code
// reports a conversation's whole usage at its SessionEnd, while each Codex
// Stop credits the conversation's counter as it grows.
const UsageOwedClient = "claude-code"

// UsageOwedHorizon is how long an owed conversation may go without a change
// before its owed usage is dropped: the last read holds everything in its
// transcript, and a conversation resumed after it starts with a SessionStart,
// which reactivates its row. The change is the transcript's latest at the
// last read (OwedUsage.LastChange), or the end when none was read.
const UsageOwedHorizon = 7 * 24 * time.Hour

// OwedUsage is one conversation's owed usage: its ambient row, and what its
// reads and asks recorded.
type OwedUsage struct {
	Session core.Session
	// OwedAt is when its latest end without a SessionEnd harvest came.
	OwedAt time.Time
	// ChangedAt is the transcript's latest modification at the last read;
	// nil before the first.
	ChangedAt *time.Time
	// Asks is how many times it was asked for under peer_auth since the last
	// answer, and AskedAt when last (nil before the first).
	Asks    int
	AskedAt *time.Time
}

// LastChange is the latest the conversation is known to have changed: its
// transcript's last change at the last read, or its end when none was read.
func (o OwedUsage) LastChange() time.Time {
	if o.ChangedAt != nil && o.ChangedAt.After(o.OwedAt) {
		return *o.ChangedAt
	}
	return o.OwedAt
}

// MarkUsageOwed records that session id's usage is owed from now, when it is
// the ambient row of a Claude Code conversation (UsageOwedClient, with the
// agent client's session id); any other session is not marked, and that is
// no error. marked reports whether a row was written. A conversation owed
// already has its owed_at moved to now and keeps what its reads and asks
// recorded.
func MarkUsageOwed(ctx context.Context, db *sql.DB, id string, now time.Time) (marked bool, err error) {
	res, err := db.ExecContext(ctx, `INSERT INTO session_usage_owed (session_id, owed_at)
		SELECT id, ? FROM sessions
		 WHERE id = ? AND ambient = 1 AND external_client = ? AND claude_session_id != ''
		ON CONFLICT (session_id) DO UPDATE SET owed_at = excluded.owed_at`,
		core.FormatTime(now), id, UsageOwedClient)
	if err != nil {
		return false, fmt.Errorf("store.MarkUsageOwed: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store.MarkUsageOwed: rows affected: %w", err)
	}
	return n > 0, nil
}

// ClearUsageOwed forgets session id's owed usage: it was read, or can no
// longer be.
func ClearUsageOwed(ctx context.Context, db *sql.DB, id string) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM session_usage_owed WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("store.ClearUsageOwed: %w", err)
	}
	return nil
}

// ClearConversationUsageOwed forgets the owed usage of the conversation the
// client's external session id names (its ambient row): its SessionEnd
// harvested the whole transcript.
func ClearConversationUsageOwed(ctx context.Context, db *sql.DB, externalClient, externalSessionID string) error {
	if externalClient == "" || externalSessionID == "" {
		return nil
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM session_usage_owed WHERE session_id IN (
		SELECT id FROM sessions WHERE ambient = 1 AND external_client = ? AND claude_session_id = ?)`,
		externalClient, externalSessionID); err != nil {
		return fmt.Errorf("store.ClearConversationUsageOwed: %w", err)
	}
	return nil
}

// owedCols are the owed row's columns after sessionCols (scanOwedUsage).
const owedCols = `o.owed_at, o.changed_at, o.asks, o.asked_at`

// OwedUsages returns every owed conversation, the longest owed first: the
// gardener's upkeep settles them.
func OwedUsages(ctx context.Context, db *sql.DB) ([]OwedUsage, error) {
	return queryOwedUsage(ctx, db, "store.OwedUsages", ``)
}

// OwedUsagesFor returns the owed conversations of one agent client and
// account (the owner is ""), the longest owed first: the ones a hook of that
// caller may be asked about.
func OwedUsagesFor(ctx context.Context, db *sql.DB, externalClient, account string) ([]OwedUsage, error) {
	if externalClient == "" {
		return nil, nil
	}
	return queryOwedUsage(ctx, db, "store.OwedUsagesFor",
		`WHERE sessions.external_client = ? AND sessions.account = ?`, externalClient, account)
}

// OwedUsageOf returns the owed usage of the conversation the client's external
// session id names; found is false when none is owed.
func OwedUsageOf(ctx context.Context, db *sql.DB, externalClient, externalSessionID string) (OwedUsage, bool, error) {
	if externalClient == "" || externalSessionID == "" {
		return OwedUsage{}, false, nil
	}
	out, err := queryOwedUsage(ctx, db, "store.OwedUsageOf",
		`WHERE sessions.ambient = 1 AND sessions.external_client = ? AND sessions.claude_session_id = ?`,
		externalClient, externalSessionID)
	if err != nil || len(out) == 0 {
		return OwedUsage{}, false, err
	}
	return out[0], true, nil
}

// queryOwedUsage reads the owed rows, joined to their sessions, that where
// (a WHERE clause over sessions, or "") selects.
func queryOwedUsage(ctx context.Context, db *sql.DB, op, where string, args ...any) ([]OwedUsage, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+sessionCols+`, `+owedCols+`
		FROM sessions JOIN session_usage_owed o ON o.session_id = sessions.id
		`+where+`
		ORDER BY o.owed_at, sessions.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = rows.Close() }()
	var out []OwedUsage
	for rows.Next() {
		o, err := scanOwedUsage(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// scanOwedUsage scans a sessionCols row followed by owedCols.
func scanOwedUsage(rows *sql.Rows) (OwedUsage, error) {
	var (
		owedAt            string
		changedAt, asked  sql.NullString
		asks              int
		o                 OwedUsage
		err               error
		parseOptionalTime = func(field string, v sql.NullString) (*time.Time, error) {
			if !v.Valid || v.String == "" {
				return nil, nil
			}
			at, err := core.ParseTime(v.String)
			if err != nil {
				return nil, fmt.Errorf("owed usage %s: %w", field, err)
			}
			return &at, nil
		}
	)
	if o.Session, err = scanSession(scanExtra{rows: rows, extra: []any{&owedAt, &changedAt, &asks, &asked}}); err != nil {
		return OwedUsage{}, err
	}
	if o.OwedAt, err = core.ParseTime(owedAt); err != nil {
		return OwedUsage{}, fmt.Errorf("owed usage owed_at: %w", err)
	}
	if o.ChangedAt, err = parseOptionalTime("changed_at", changedAt); err != nil {
		return OwedUsage{}, err
	}
	if o.AskedAt, err = parseOptionalTime("asked_at", asked); err != nil {
		return OwedUsage{}, err
	}
	o.Asks = asks
	return o, nil
}

// RecordUsageOwedRead records a read of session id's owed transcript: the
// transcript's latest change as it was read, and an answer to any ask, so the
// asks start counting again. recorded is false when the row is gone (a
// SessionEnd settled it meanwhile).
func RecordUsageOwedRead(ctx context.Context, db *sql.DB, id string, changedAt time.Time) (recorded bool, err error) {
	res, err := db.ExecContext(ctx, `UPDATE session_usage_owed SET changed_at = ?, asks = 0 WHERE session_id = ?`,
		core.FormatTime(changedAt), id)
	if err != nil {
		return false, fmt.Errorf("store.RecordUsageOwedRead: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store.RecordUsageOwedRead: rows affected: %w", err)
	}
	return n > 0, nil
}

// RecordUsageOwedAsk counts one more ask for session id's owed usage, at now,
// and returns the count. recorded is false when the row is gone.
func RecordUsageOwedAsk(ctx context.Context, db *sql.DB, id string, now time.Time) (asks int, recorded bool, err error) {
	err = db.QueryRowContext(ctx, `UPDATE session_usage_owed SET asks = asks + 1, asked_at = ?
		WHERE session_id = ? RETURNING asks`, core.FormatTime(now), id).Scan(&asks)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("store.RecordUsageOwedAsk: %w", err)
	}
	return asks, true, nil
}
