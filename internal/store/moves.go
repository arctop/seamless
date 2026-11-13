package store

// Moves (isolation.requests, migration 039): a conversation working in an open
// project (or the global scope) that reaches a confidential project, or any
// conversation that reaches a sealed one, asks to move there for good. The
// request waits in access_approvals as a pending row of kind move, with its own
// one-time challenge and the carry the owner is shown; MoveApprove -- the one
// writer of a move, single use and bound to the challenge -- records the move
// and the flows its carry lists, in one transaction. A move is never a lease
// and never a grant, and nothing ever ends one: revocation, expiry and a
// restore leave access_moves as it is.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/core"
)

// moveMaxRows bounds the moves one holder's lookup returns: a conversation
// moves a handful of times at most.
const moveMaxRows = 200

// MoveRequest is what MoveRequestCreate records: the fenced project a
// conversation asks to move into, the home it would leave, the carry the owner
// is shown, who asks, and what the request said about itself.
type MoveRequest struct {
	// Project is the fenced project to move into; From the home the
	// conversation would leave ("" for the global scope).
	Project, From string
	// Carry is what the approval binds: Home is From, Holds names Project
	// (the conversation holds it once moved), and WriteTo is exactly Project
	// (after a move it writes nowhere else).
	Carry Carry
	// Lease is how long the target's lease lasts when the move also opens a
	// lock (a whole number of seconds, at least one); a move itself never
	// ends. TTL is how long the request waits.
	Lease, TTL time.Duration

	// Under peer_auth: the kernel's facts about the anchoring agent process,
	// ProcessKey its access.ProcessToken.Key -- the holder.
	PeerUID    uint32
	PeerPID    int32
	ProcessKey string
	Program    string
	// Without peer_auth: the asking conversation, both or neither -- the
	// holder when set.
	ConversationClient, ConversationID string

	// SessionID is the session the conversation would leave (required: a
	// move's child continues it). It is never the holder: one of the two
	// above must be set.
	SessionID, SessionName, CWD, Repo, Model, Reason string
}

func moveValidRequest(r MoveRequest) error {
	switch {
	case strings.TrimSpace(r.Project) == "" || r.Project != strings.TrimSpace(r.Project):
		return fmt.Errorf("%w: project %q", ErrInvalidAccessRequest, r.Project)
	case r.From != strings.TrimSpace(r.From):
		return fmt.Errorf("%w: from %q", ErrInvalidAccessRequest, r.From)
	case r.Project == r.From:
		return fmt.Errorf("%w: a conversation is already home in %q: nothing to move", ErrInvalidAccessRequest, r.Project)
	case r.Lease < time.Second || r.Lease%time.Second != 0:
		return fmt.Errorf("%w: lease %s is not a whole number of seconds, at least one", ErrInvalidAccessRequest, r.Lease)
	case r.TTL <= 0:
		return fmt.Errorf("%w: ttl %s must be positive", ErrInvalidAccessRequest, r.TTL)
	case r.ProcessKey != "" && r.PeerPID <= 0:
		return fmt.Errorf("%w: pid %d", ErrInvalidAccessRequest, r.PeerPID)
	case r.SessionID == "":
		return fmt.Errorf("%w: no session: a move's child continues the session it leaves", ErrInvalidAccessRequest)
	case utf8.RuneCountInString(r.Reason) > MaxAccessReasonRunes || !utf8.ValidString(r.Reason):
		return fmt.Errorf("%w: the reason is limited to %d characters of valid UTF-8", ErrInvalidAccessRequest, MaxAccessReasonRunes)
	}
	for _, f := range []struct{ name, v string }{{"program", r.Program}, {"session_id", r.SessionID},
		{"session_name", r.SessionName}, {"cwd", r.CWD}, {"repo", r.Repo}, {"model", r.Model}} {
		if len(f.v) > maxAccessFieldBytes || !utf8.ValidString(f.v) {
			return fmt.Errorf("%w: %s is limited to %d bytes of valid UTF-8", ErrInvalidAccessRequest, f.name, maxAccessFieldBytes)
		}
	}
	if r.ProcessKey != "" {
		if _, err := HolderOfProcess(r.ProcessKey); err != nil {
			return err
		}
	}
	if r.ConversationClient != "" || r.ConversationID != "" {
		if _, err := HolderOfConversation(r.ConversationClient, r.ConversationID); err != nil {
			return err
		}
	}
	if _, err := HolderOfSession(r.SessionID); err != nil {
		return err
	}
	if r.ProcessKey == "" && r.ConversationID == "" {
		return fmt.Errorf("%w: %s", ErrInvalidAccessRequest, moveSessionHolder)
	}
	return moveValidCarry(r.Carry, r.Project, r.From)
}

// moveSessionHolder is why a move is never a session's: its child is another
// session, so what it moved through would not follow it.
const moveSessionHolder = "a move belongs to an agent process or a conversation, never to a session alone: " +
	"its child is another session, and its label would not follow it"

// moveValidCarry checks that c states a move from home into target: a carry
// is required, its home is the one left, it holds the target, and it writes to
// the target alone -- after a move a conversation writes nowhere else, so a
// carry naming another destination would clear a flow out of the project it
// moved into.
func moveValidCarry(c Carry, target, home string) error {
	n, err := CarryOf(c.Home, c.Holds, c.WriteTo)
	switch {
	case err != nil:
		return err
	case n.IsZero():
		return fmt.Errorf("%w: a move states its carry", ErrInvalidAccessRequest)
	case n.Home != home:
		return fmt.Errorf("%w: the carry's home %q is not the home %q the move leaves", ErrInvalidAccessRequest, n.Home, home)
	case !slices.Contains(n.Holds, target):
		return fmt.Errorf("%w: the carry does not hold %q, the project it moves into", ErrInvalidAccessRequest, target)
	case !slices.Equal(n.WriteTo, []string{target}):
		return fmt.Errorf("%w: a move's carry writes to %q alone, not %q", ErrInvalidAccessRequest, target, n.WriteTo)
	}
	return nil
}

// MoveRequestCreate records a pending move request (an access_approvals row of
// kind move) with a fresh one-time challenge, waiting until now + req.TTL.
func MoveRequestCreate(ctx context.Context, db *sql.DB, req MoveRequest, now time.Time) (AccessApproval, error) {
	const op = "store.MoveRequestCreate"
	if err := moveValidRequest(req); err != nil {
		return AccessApproval{}, fmt.Errorf("%s: %w", op, err)
	}
	carry, err := carryJSON(req.Carry)
	if err != nil {
		return AccessApproval{}, fmt.Errorf("%s: %w", op, err)
	}
	id, err := core.NewID()
	if err != nil {
		return AccessApproval{}, fmt.Errorf("%s: %w", op, err)
	}
	challenge := make([]byte, AccessChallengeBytes)
	if _, err := rand.Read(challenge); err != nil {
		return AccessApproval{}, fmt.Errorf("%s: challenge: %w", op, err)
	}
	now = now.UTC()
	if _, err := db.ExecContext(ctx, `INSERT INTO access_approvals (id, project_slug, kind, challenge, status,
		requested_at, expires_at, lease_seconds, peer_uid, peer_pid, process_key, program,
		session_id, session_name, cwd, repo, model, reason, from_project,
		carry, conversation_client, conversation_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, req.Project, AccessKindMove, challenge, ApprovalPending, core.FormatTime(now), core.FormatTime(now.Add(req.TTL)),
		int64(req.Lease/time.Second), int64(req.PeerUID), int64(req.PeerPID), req.ProcessKey, req.Program,
		req.SessionID, req.SessionName, req.CWD, req.Repo, req.Model, req.Reason, req.From,
		carry, req.ConversationClient, req.ConversationID); err != nil {
		return AccessApproval{}, fmt.Errorf("%s: %w", op, err)
	}
	stored, err := AccessApprovalByID(ctx, db, id)
	if err != nil {
		return AccessApproval{}, fmt.Errorf("%s: %w", op, err)
	}
	return stored, nil
}

// Move is one row of access_moves: Holder left FromSessionID, homed in
// FromProject, for ToProject, where it continues as ToSessionID.
type Move struct {
	ID         string
	ApprovalID string
	Holder     Holder
	// FromSessionID is the session it left; ToSessionID the child it
	// continues in (whose parent is FromSessionID).
	FromSessionID, ToSessionID string
	// FromProject is the home it left ("" for the global scope); ToProject the
	// fenced project it moved into.
	FromProject, ToProject string
	DecidedBy              string
	MovedAt                time.Time
}

const moveCols = `id, approval_id, holder, from_session_id, to_session_id, from_project, to_project, decided_by, moved_at`

func moveScan(rows *sql.Rows) ([]Move, error) {
	var out []Move
	for rows.Next() {
		var (
			m      Move
			holder string
			moved  string
		)
		if err := rows.Scan(&m.ID, &m.ApprovalID, &holder, &m.FromSessionID, &m.ToSessionID,
			&m.FromProject, &m.ToProject, &m.DecidedBy, &moved); err != nil {
			return nil, err
		}
		var err error
		if m.Holder, err = HolderFromKey(holder); err != nil {
			return nil, fmt.Errorf("move %s holder: %w", m.ID, err)
		}
		if m.MovedAt, err = core.ParseTime(moved); err != nil {
			return nil, fmt.Errorf("move %s moved_at: %w", m.ID, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MoveApprove turns a pending move request into a move, in one transaction,
// and is the only writer of one: the request must be of kind move, pending,
// before its deadline, and carry exactly the challenge the decision was made
// over. Single use: a second approve of the same id is ErrApprovalNotPending,
// and the table holds one move per approval. In the same transaction it
// records the flows the request's carry lists: each held project into the
// project moved into (Carry.Flows). decidedBy names the proof that decided;
// toSessionID is the child session the conversation continues in, which the
// caller opens with ParentSessionID set to the request's session.
//
// A request found past its deadline is marked expired, and the call returns
// ErrApprovalExpired.
func MoveApprove(ctx context.Context, db *sql.DB, id string, challenge []byte, decidedBy, toSessionID string, now time.Time) (Move, error) {
	const op = "store.MoveApprove"
	if decidedBy == "" {
		return Move{}, fmt.Errorf("%s: %w: no deciding proof named", op, ErrInvalidAccessRequest)
	}
	if _, err := HolderOfSession(toSessionID); err != nil {
		return Move{}, fmt.Errorf("%s: the child session: %w", op, err)
	}
	now = now.UTC()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Move{}, fmt.Errorf("%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	a, err := scanApproval(tx.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM access_approvals WHERE id = ?`, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Move{}, fmt.Errorf("%s: %w: %q", op, ErrApprovalNotFound, id)
	case err != nil:
		return Move{}, fmt.Errorf("%s: %w", op, err)
	case a.Kind != AccessKindMove:
		return Move{}, fmt.Errorf("%s: %w: %s is a %s request, not a move's", op, ErrInvalidAccessRequest, id, a.Kind)
	case a.Status != ApprovalPending:
		return Move{}, fmt.Errorf("%s: %w: %s is %s", op, ErrApprovalNotPending, id, a.Status)
	}
	if !a.ExpiresAt.After(now) {
		if err := expireApprovalTx(ctx, tx, id, now); err != nil {
			return Move{}, fmt.Errorf("%s: %w", op, err)
		}
		if err := tx.Commit(); err != nil {
			return Move{}, fmt.Errorf("%s: commit: %w", op, err)
		}
		return Move{}, fmt.Errorf("%s: %w: %s ended at %s", op, ErrApprovalExpired, id, core.FormatTime(a.ExpiresAt))
	}
	if len(challenge) != AccessChallengeBytes || subtle.ConstantTimeCompare(challenge, a.Challenge) != 1 {
		return Move{}, fmt.Errorf("%s: %w", op, ErrApprovalChallenge)
	}
	holder := a.Holder()
	switch {
	case holder.IsZero() || a.SessionID == "":
		return Move{}, fmt.Errorf("%s: %w: %s names no holder or session to move", op, ErrInvalidAccessRequest, id)
	case holder.Kind() == HolderSession:
		return Move{}, fmt.Errorf("%s: %w: %s", op, ErrInvalidAccessRequest, moveSessionHolder)
	case toSessionID == a.SessionID:
		return Move{}, fmt.Errorf("%s: %w: the child session is the session it leaves", op, ErrInvalidAccessRequest)
	}
	if err := moveValidCarry(a.Carry, a.Project, a.FromProject); err != nil {
		return Move{}, fmt.Errorf("%s: %s: %w", op, id, err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE access_approvals SET status = ?, decided_at = ?, decided_by = ?
		WHERE id = ? AND status = ?`, ApprovalApproved, core.FormatTime(now), decidedBy, id, ApprovalPending)
	if err != nil {
		return Move{}, fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Move{}, fmt.Errorf("%s: %w", op, err)
	}
	if n != 1 {
		return Move{}, fmt.Errorf("%s: %w: %s", op, ErrApprovalNotPending, id)
	}
	moveID, err := core.NewID()
	if err != nil {
		return Move{}, fmt.Errorf("%s: %w", op, err)
	}
	m := Move{
		ID: moveID, ApprovalID: a.ID, Holder: holder, FromSessionID: a.SessionID, ToSessionID: toSessionID,
		FromProject: a.FromProject, ToProject: a.Project, DecidedBy: decidedBy, MovedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO access_moves (`+moveCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ApprovalID, m.Holder.Key(), m.FromSessionID, m.ToSessionID, m.FromProject, m.ToProject,
		m.DecidedBy, core.FormatTime(m.MovedAt)); err != nil {
		return Move{}, fmt.Errorf("%s: %w", op, err)
	}
	if _, err := flowRecordTx(ctx, tx, holder, a.ID, a.Carry.Flows(), now); err != nil {
		return Move{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return Move{}, fmt.Errorf("%s: commit: %w", op, err)
	}
	return m, nil
}

// MovesForHolder returns every move any of holders made, oldest first, at
// most moveMaxRows. With no holder it returns none.
func MovesForHolder(ctx context.Context, db *sql.DB, holders []Holder) ([]Move, error) {
	clause, args, ok, err := holderKeyClause("holder", holders)
	if err != nil {
		return nil, fmt.Errorf("store.MovesForHolder: %w", err)
	}
	if !ok {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+moveCols+` FROM access_moves WHERE `+clause+`
		ORDER BY moved_at, id LIMIT `+fmt.Sprint(moveMaxRows), args...)
	if err != nil {
		return nil, fmt.Errorf("store.MovesForHolder: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out, err := moveScan(rows)
	if err != nil {
		return nil, fmt.Errorf("store.MovesForHolder: %w", err)
	}
	return out, nil
}
