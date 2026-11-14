package store

// The one-time briefing after an approved move (isolation.requests, plan
// fenced-sessions P4; migration 040): the first tool result the conversation
// gets in its new home says it moved and carries that home's briefing, once.
// The claim is access_moves.briefed_at, as a lease's is access_leases
// .briefed_at (ClaimAccessLeaseBriefing): atomic, and kept across a restart.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// MoveInto is the move that opened a session, as its briefing reads it.
type MoveInto struct {
	Move Move
	// LeaseID is the lease the move's approval opened with it (a locked
	// target under peer_auth), "" for none.
	LeaseID string
	// Briefed reports that a tool result already carried the briefing.
	Briefed bool
}

// MoveIntoSession returns the move that opened session sessionID -- the
// child a move continues the conversation in -- and found false for every
// other session.
func MoveIntoSession(ctx context.Context, db *sql.DB, sessionID string) (MoveInto, bool, error) {
	const op = "store.MoveIntoSession"
	if sessionID == "" {
		return MoveInto{}, false, nil
	}
	var (
		m              Move
		holder, moved  string
		briefed, lease sql.NullString
	)
	err := db.QueryRowContext(ctx, `SELECT m.id, m.approval_id, m.holder, m.from_session_id, m.to_session_id,
		       m.from_project, m.to_project, m.decided_by, m.moved_at, m.briefed_at, a.lease_id
		  FROM access_moves m JOIN access_approvals a ON a.id = m.approval_id
		 WHERE m.to_session_id = ?
		 ORDER BY m.moved_at DESC, m.id DESC LIMIT 1`, sessionID).Scan(
		&m.ID, &m.ApprovalID, &holder, &m.FromSessionID, &m.ToSessionID, &m.FromProject, &m.ToProject,
		&m.DecidedBy, &moved, &briefed, &lease)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return MoveInto{}, false, nil
	case err != nil:
		return MoveInto{}, false, fmt.Errorf("%s: %w", op, err)
	}
	if m.Holder, err = HolderFromKey(holder); err != nil {
		return MoveInto{}, false, fmt.Errorf("%s: move %s holder: %w", op, m.ID, err)
	}
	if m.MovedAt, err = core.ParseTime(moved); err != nil {
		return MoveInto{}, false, fmt.Errorf("%s: move %s moved_at: %w", op, m.ID, err)
	}
	return MoveInto{Move: m, LeaseID: lease.String, Briefed: briefed.Valid}, true, nil
}

// ClaimMoveBriefing reports whether this caller is the first to brief move
// id, and records that it is at now. The row is the claim, so it holds across
// a daemon restart, and two concurrent calls cannot both win. A failure claims
// nothing.
func ClaimMoveBriefing(ctx context.Context, db *sql.DB, id string, now time.Time) (bool, error) {
	res, err := db.ExecContext(ctx, `UPDATE access_moves SET briefed_at = ? WHERE id = ? AND briefed_at IS NULL`,
		core.FormatTime(now), id)
	if err != nil {
		return false, fmt.Errorf("store.ClaimMoveBriefing: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store.ClaimMoveBriefing: %w", err)
	}
	return n == 1, nil
}

// ForgetMoveBriefing undoes a claim whose briefing could not be built, so the
// next result tries again rather than a briefing nobody received counting as
// sent.
func ForgetMoveBriefing(ctx context.Context, db *sql.DB, id string) error {
	if _, err := db.ExecContext(ctx, `UPDATE access_moves SET briefed_at = NULL WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store.ForgetMoveBriefing: %w", err)
	}
	return nil
}
