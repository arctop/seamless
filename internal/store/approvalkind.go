package store

// The approvals of a move and of a read-and-write grant (isolation.requests,
// migration 039): the whole of one owner decision in one transaction.
//
//   - GrantAccessApproveWith is GrantAccessApprove with the target's lease when
//     the grant opens a locked project under peer_auth: the grant, the flows
//     its carry lists and the lease are one approval.
//   - MoveApproveWith is MoveApprove with everything a move does to the
//     conversation's sessions: the session it leaves ends (no longer ambient,
//     completed, a metadata-only pointer in its findings), the child session
//     opens in the target as the conversation's one ambient row (the same
//     external ids, stamped with the same agent process, parent_session_id
//     naming the session it left), and -- for a locked target under peer_auth
//     -- the target's lease, bound to the child. The flip comes before the
//     child's insert because idx_sessions_ambient_external_identity (migration
//     010) is unique over every ambient row of an identity, whatever its
//     status.
//
// MoveApprove and GrantAccessApprove keep their shape and meaning: a caller
// that opens its own child session (a test fixture) still uses MoveApprove.

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// GrantDecision is one grant request's approval (GrantAccessApproveWith).
type GrantDecision struct {
	ID        string
	Challenge []byte
	// DecidedBy names the proof that decided.
	DecidedBy string
	// Length is the grant's length, a whole number of seconds, or 0 for the
	// length the request carried.
	Length time.Duration
	// Access is what the owner chose: GrantAccessRead or GrantAccessWrite.
	Access string
	// Lease records the target's lease too, as long as the grant and bound to
	// the request's process: a locked target under peer_auth. A request that
	// names no process is refused.
	Lease bool
}

// GrantAccessApproveWith is GrantAccessApprove (the owner's access, the flows
// the carry lists) with d.Lease: the target's lease recorded in the same
// transaction, which it returns (nil without one).
func GrantAccessApproveWith(ctx context.Context, db *sql.DB, d GrantDecision, now time.Time) (AccessGrant, *AccessLease, error) {
	return grantAccessApprove(ctx, db, "store.GrantAccessApproveWith", d, true, now)
}

// MoveDecision is one move request's approval (MoveApproveWith).
type MoveDecision struct {
	ID        string
	Challenge []byte
	// DecidedBy names the proof that decided.
	DecidedBy string
	// Lease, when positive, records the target's lease for that long (a whole
	// number of seconds), bound to the request's process and the child
	// session: a locked target under peer_auth. Zero records none; a move
	// itself never ends.
	Lease time.Duration
}

// MoveApproved is what one approved move recorded.
type MoveApproved struct {
	Move Move
	// Child is the session the conversation continues in: its ambient row
	// from now on, in the project it moved into.
	Child core.Session
	// Left is the session it left, as the move ended it.
	Left core.Session
	// Lease is the target's lease, when the move carries one.
	Lease *AccessLease
}

// MovePointer is the metadata-only line a move leaves in the findings of the
// session it ends: where the conversation went, never what it did there.
func MovePointer(project, childName string) string {
	return "Moved to project " + project + "; this conversation continues there as session " + childName +
		", and its findings go there, not here."
}

// MoveApproveWith turns a pending move request into a move and does what the
// move means for the conversation's sessions, in one transaction (see the file
// comment). The session the request names is the one it leaves: it must still
// be the conversation's ambient row (for a conversation holder, of that very
// conversation), not completed, and working in the home the request leaves.
// Its findings keep what the agent wrote and gain the pointer; the hooks' own
// placeholders are replaced by it. Everything else MoveApprove checks is
// checked the same way, and the same errors come back.
func MoveApproveWith(ctx context.Context, db *sql.DB, d MoveDecision, now time.Time) (MoveApproved, error) {
	const op = "store.MoveApproveWith"
	if d.DecidedBy == "" {
		return MoveApproved{}, fmt.Errorf("%s: %w: no deciding proof named", op, ErrInvalidAccessRequest)
	}
	if d.Lease < 0 || d.Lease%time.Second != 0 {
		return MoveApproved{}, fmt.Errorf("%s: %w: lease %s is not a whole number of seconds", op, ErrInvalidAccessRequest, d.Lease)
	}
	now = now.UTC()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return MoveApproved{}, fmt.Errorf("%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	a, holder, expired, err := moveCheckTx(ctx, tx, d.ID, d.Challenge, now)
	if err != nil {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
	}
	if expired {
		if err := tx.Commit(); err != nil {
			return MoveApproved{}, fmt.Errorf("%s: commit: %w", op, err)
		}
		return MoveApproved{}, fmt.Errorf("%s: %w: %s ended at %s", op, ErrApprovalExpired, d.ID, core.FormatTime(a.ExpiresAt))
	}
	if d.Lease > 0 && a.ProcessKey == "" {
		return MoveApproved{}, fmt.Errorf("%s: %w: %s names no process to lease the project to", op, ErrInvalidAccessRequest, a.ID)
	}
	left, agentProcess, err := moveLeftSessionTx(ctx, tx, a, holder)
	if err != nil {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
	}
	childID, err := core.NewID()
	if err != nil {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
	}
	child := core.Session{
		ID: childID, Name: left.Name + "@" + a.Project, ProjectSlug: a.Project, Status: core.SessionActive,
		ExternalSessionID: left.ExternalSessionID, ExternalClient: left.ExternalClient, CWD: left.CWD, Host: left.Host,
		Source: left.Source, Model: left.Model, Ambient: true, Account: left.Account, ParentSessionID: left.ID,
		Metadata:  moveMetadata(left.Metadata, map[string]any{"moved_from": left.ID, "moved_from_project": a.FromProject, "move_approval": a.ID}),
		CreatedAt: now, UpdatedAt: now,
	}
	// The session it leaves first: the identity's unique ambient index admits
	// one ambient row, so the child can only be inserted once this one is not.
	left.Ambient, left.Status, left.UpdatedAt = false, core.SessionCompleted, now
	left.Findings = movedFindings(left.Findings, MovePointer(a.Project, child.Name))
	left.Metadata = moveMetadata(left.Metadata, map[string]any{"moved_to": child.ID, "moved_to_project": a.Project, "move_approval": a.ID})
	meta, err := marshalMetadata(left.Metadata)
	if err != nil {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE sessions
		   SET ambient = 0, status = ?, findings = ?, metadata = ?, agent_process = '', updated_at = ?
		 WHERE id = ? AND ambient = 1 AND status <> ?`,
		string(core.SessionCompleted), left.Findings, meta, core.FormatTime(now), left.ID, string(core.SessionCompleted))
	if err != nil {
		return MoveApproved{}, fmt.Errorf("%s: end the session it leaves: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return MoveApproved{}, fmt.Errorf("%s: end the session it leaves: %w", op, err)
	}
	if n != 1 {
		return MoveApproved{}, fmt.Errorf("%s: %w: session %s ended, or stopped being the conversation's own, meanwhile", op, ErrInvalidAccessRequest, left.ID)
	}
	if child, err = moveInsertChildTx(ctx, tx, child, agentProcess); err != nil {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
	}
	var lease *AccessLease
	leaseID := ""
	if d.Lease > 0 {
		if leaseID, err = core.NewID(); err != nil {
			return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
		}
		lease = &AccessLease{ID: leaseID, Project: a.Project, ProcessKey: a.ProcessKey, PeerUID: a.PeerUID, PeerPID: a.PeerPID,
			SessionID: child.ID, ApprovalID: a.ID, GrantedAt: now, ExpiresAt: now.Add(d.Lease)}
		if err := leaseInsertTx(ctx, tx, *lease); err != nil {
			return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
		}
	}
	m, err := moveRecordTx(ctx, tx, a, holder, d.DecidedBy, child.ID, leaseID, now)
	if err != nil {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
	}
	// The search mirror of the work record follows the findings the move wrote.
	if err := IndexSessionFTS(ctx, tx, left); err != nil {
		return MoveApproved{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return MoveApproved{}, fmt.Errorf("%s: commit: %w", op, err)
	}
	return MoveApproved{Move: m, Child: child, Left: left, Lease: lease}, nil
}

// moveLeftSessionTx reads the session move request a leaves, and its stamped
// agent process, and checks it may be left: the conversation's ambient row --
// of the holder's own conversation when a conversation holds the move --
// with a complete external identity (the child takes it over), not completed,
// working in the home the request leaves.
func moveLeftSessionTx(ctx context.Context, tx *sql.Tx, a AccessApproval, holder Holder) (core.Session, string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+sessionCols+`, agent_process FROM sessions WHERE id = ?`, a.SessionID)
	if err != nil {
		return core.Session{}, "", fmt.Errorf("the session it leaves: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return core.Session{}, "", fmt.Errorf("the session it leaves: %w", err)
		}
		return core.Session{}, "", fmt.Errorf("%w: session %s, which the move leaves, is gone", ErrInvalidAccessRequest, a.SessionID)
	}
	var (
		left    core.Session
		process string
	)
	if left, process, err = scanSessionProcess(rows); err != nil {
		return core.Session{}, "", fmt.Errorf("the session it leaves: %w", err)
	}
	if err := rows.Close(); err != nil {
		return core.Session{}, "", fmt.Errorf("the session it leaves: %w", err)
	}
	client, external := holder.Conversation()
	switch {
	case !left.Ambient || left.ExternalClient == "" || left.ExternalSessionID == "":
		return core.Session{}, "", fmt.Errorf("%w: session %s is not a conversation's own (ambient) session: nothing to move", ErrInvalidAccessRequest, left.ID)
	case holder.Kind() == HolderConversation && (left.ExternalClient != client || left.ExternalSessionID != external):
		return core.Session{}, "", fmt.Errorf("%w: session %s belongs to another conversation", ErrInvalidAccessRequest, left.ID)
	case left.Status == core.SessionCompleted:
		return core.Session{}, "", fmt.Errorf("%w: session %s has ended: nothing to move", ErrInvalidAccessRequest, left.ID)
	case left.ProjectSlug != a.FromProject:
		return core.Session{}, "", fmt.Errorf("%w: session %s works in %q now, not %q, the home the move leaves",
			ErrInvalidAccessRequest, left.ID, left.ProjectSlug, a.FromProject)
	}
	return left, process, nil
}

// scanSessionProcess scans a sessionCols row followed by agent_process.
func scanSessionProcess(rows *sql.Rows) (core.Session, string, error) {
	var process string
	s, err := scanSession(scanThenMore{row: rows, more: []any{&process}})
	if err != nil {
		return core.Session{}, "", err
	}
	return s, process, nil
}

// scanThenMore scans a row whose leading columns are what a scanner like
// scanSession asks for, then the columns after them into more.
type scanThenMore struct {
	row  interface{ Scan(...any) error }
	more []any
}

func (s scanThenMore) Scan(dest ...any) error { return s.row.Scan(append(dest, s.more...)...) }

// moveInsertChildTx inserts the move's child session inside tx, stamped with
// agentProcess. A child name another session holds (a session named like it
// by hand) takes the child id's last symbols as a suffix instead of failing
// the approval.
func moveInsertChildTx(ctx context.Context, tx *sql.Tx, child core.Session, agentProcess string) (core.Session, error) {
	meta, err := marshalMetadata(child.Metadata)
	if err != nil {
		return core.Session{}, err
	}
	insert := func(name string) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sessions
			    (id, name, project_slug, status, findings, claude_session_id,
			     external_client, cwd, host, source, model, ambient, metadata, created_at, updated_at, account,
			     parent_session_id, agent_process)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?)`,
			child.ID, name, child.ProjectSlug, string(child.Status), child.Findings, child.ExternalSessionID,
			child.ExternalClient, child.CWD, child.Host, child.Source, child.Model, meta,
			core.FormatTime(child.CreatedAt), core.FormatTime(child.UpdatedAt), child.Account, child.ParentSessionID, agentProcess)
		return err
	}
	err = insert(child.Name)
	if err != nil && isUniqueViolation(err) {
		child.Name += "-" + strings.ToLower(child.ID[len(child.ID)-6:])
		err = insert(child.Name)
	}
	if err != nil {
		return core.Session{}, fmt.Errorf("open the child session: %w", err)
	}
	return child, nil
}

// movedFindings is the findings of the session a move ends: the agent's own
// findings with the pointer after them, or the pointer alone where the
// session holds none of its own (blank, or the hooks' no-summary sentinel).
func movedFindings(findings, pointer string) string {
	if !SessionFindingsIndexable(findings) {
		return pointer
	}
	return strings.TrimRight(findings, "\n") + "\n\n" + pointer
}

// moveMetadata is base with add's keys set, base itself untouched.
func moveMetadata(base, add map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(add))
	maps.Copy(out, base)
	maps.Copy(out, add)
	return out
}

// RevokeAccessGrantsForHolder ends every live grant holder holds, returning
// how many: a conversation's end, which ends its grants whichever of its
// sessions asked for them. The zero holder is refused: it would name every
// row that names none.
func RevokeAccessGrantsForHolder(ctx context.Context, db *sql.DB, holder Holder, reason string, now time.Time) (int64, error) {
	const op = "store.RevokeAccessGrantsForHolder"
	if err := validRevokeReason(reason); err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	clause, args, ok, err := holderAccessClause([]Holder{holder})
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	if !ok {
		return 0, fmt.Errorf("%s: %w: no holder", op, ErrInvalidAccessRequest)
	}
	stamp := core.FormatTime(now)
	args = append([]any{stamp, reason}, args...)
	args = append(args, stamp)
	res, err := db.ExecContext(ctx, `UPDATE access_grants SET revoked_at = ?, revoked_reason = ?
		WHERE `+clause+` AND revoked_at IS NULL AND expires_at > ?`, args...)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	return n, nil
}
