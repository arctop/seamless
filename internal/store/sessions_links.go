package store

// Linked sessions: the explicit sessions session_start opened for a
// conversation that already had an ambient row, stamped with that
// conversation's external identity (internal/mcp linkedExternalIdentity) so a
// graceful SessionEnd closes them together. RecordDestinationOf (isolation.go)
// reads them to decide where the conversation's own record goes; the hooks
// write that record with LinkUpdateFindings.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// LinkedSessionsByExternalIdentity returns every explicit session (ambient = 0)
// stamped with a conversation's full external id and client discriminator, in
// any status, newest first (created_at, then id). Unlike
// ActiveSessionsByExternalIdentity it keeps the ended ones: a link the agent
// ended with session_end, or one the idle reaper expired, still says where the
// conversation went, and its findings may be the agent's own. An incomplete
// identity matches nothing.
func LinkedSessionsByExternalIdentity(ctx context.Context, db *sql.DB, externalClient, externalSessionID string) ([]core.Session, error) {
	if externalClient == "" || externalSessionID == "" {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+sessionCols+`
		FROM sessions
		WHERE ambient = 0 AND external_client = ? AND claude_session_id = ?
		ORDER BY created_at DESC, id DESC`, externalClient, externalSessionID)
	if err != nil {
		return nil, fmt.Errorf("store.LinkedSessionsByExternalIdentity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Session
	for rows.Next() {
		s, serr := scanSession(rows)
		if serr != nil {
			return nil, fmt.Errorf("store.LinkedSessionsByExternalIdentity: %w", serr)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.LinkedSessionsByExternalIdentity: %w", err)
	}
	return out, nil
}

// LinkUpdateFindings writes findings onto a linked (explicit) session, but only
// while its findings still read prior: a compare-and-set, so findings the agent
// wrote with session_update or session_end after the caller read the row are
// never overwritten, and a racing writer of the same value is a no-op.
//
// complete also ends the session (status completed, updated_at now), the way a
// graceful SessionEnd ends one. Without it the status never changes, and
// updated_at moves only while the session is active (a heartbeat, as
// UpdateAmbientFindings does): an expired or completed session keeps the time
// it was last alive.
//
// It returns the row as written, and found is false when nothing was: no such
// explicit session, or its findings no longer read prior. The search mirror
// follows the write, as UpdateSession's does.
func LinkUpdateFindings(ctx context.Context, db *sql.DB, id, prior, findings string, complete bool, now time.Time) (core.Session, bool, error) {
	if id == "" || findings == "" {
		return core.Session{}, false, nil
	}
	sess, found, err := sessionOne(ctx, db, `
		UPDATE sessions
		   SET findings = ?,
		       status = CASE WHEN ? THEN 'completed' ELSE status END,
		       updated_at = CASE WHEN ? OR status = 'active' THEN ? ELSE updated_at END
		 WHERE id = ? AND ambient = 0 AND findings = ?
		 RETURNING `+sessionCols,
		findings, boolToInt(complete), boolToInt(complete), core.FormatTime(now.UTC()), id, prior)
	if err != nil {
		return core.Session{}, false, fmt.Errorf("store.LinkUpdateFindings: %w", err)
	}
	if !found {
		return core.Session{}, false, nil
	}
	if err := IndexSessionFTS(ctx, db, sess); err != nil {
		return sess, true, fmt.Errorf("store.LinkUpdateFindings: %w", err)
	}
	return sess, true, nil
}
