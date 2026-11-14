package store

// Read grants (isolation.requests, migration 034): the storage behind a
// confidential project's access requests. A session refused at the fence asks
// for one project; the request waits in access_approvals as a pending row of
// kind grant, with its own one-time challenge; the owner's approval turns it
// into an access_grants row through ApproveAccessGrant -- the one writer of a
// grant, single use and bound to the challenge, exactly the discipline
// ApproveAccess keeps for a lease, and never a lease. The access layer judges
// every grant again on every request (access.JudgeGrants), so a row here is
// never trusted on its own.

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

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

// maxGrantRows bounds the grants one holder's lookup returns. A holder is one
// session or one agent process: it never comes near this many, and a lookup
// that did would be a table growing without anyone approving it.
const maxGrantRows = 200

// The access a grant opens (migration 039): what the owner approved.
const (
	// GrantAccessRead reads the target, and never writes it: every grant from
	// before migration 039, and the default.
	GrantAccessRead = "read"
	// GrantAccessWrite reads and writes the target, between confidential
	// projects, as the owner chose on the approval.
	GrantAccessWrite = "write"
)

// GrantAccesses is the canonical access set of a grant.
var GrantAccesses = []string{GrantAccessRead, GrantAccessWrite}

var (
	// ErrGrantNotFound is a grant id with no row.
	ErrGrantNotFound = errors.New("access grant not found")
	// ErrGrantNotLive is a revoke of a grant that already ended (or never was).
	ErrGrantNotLive = errors.New("access grant is not live")
)

// GrantRequest is what CreateGrantRequest records: the confidential project
// asked for, the project the asking session works in, who asks, and what the
// request said about itself.
type GrantRequest struct {
	// Project is the confidential project asked for; From the asking
	// session's own project, which the grant would open it to.
	Project, From string
	// Lease is how long the grant lasts once approved: a whole number of
	// seconds, at least one. TTL is how long the request waits.
	Lease, TTL time.Duration

	// Under peer_auth: the kernel's facts about the anchoring agent process,
	// ProcessKey its access.ProcessToken.Key -- the holder. Without peer_auth
	// they are zero and SessionID is the holder.
	PeerUID    uint32
	PeerPID    int32
	ProcessKey string
	Program    string

	SessionID, SessionName, CWD, Repo, Model, Reason string

	// Access is what the request asks for (migration 039): GrantAccessRead,
	// GrantAccessWrite, or "" for read. The owner may approve another.
	Access string
	// Carry is what the request states (migration 039): Home is From, Holds
	// names Project, and a write request's WriteTo names Project too. The zero
	// Carry states nothing, as every request did before.
	Carry Carry
	// ConversationClient and ConversationID name the asking conversation
	// without peer_auth (migration 039), both or neither: then the
	// conversation, not SessionID, is the holder. SessionID still names the
	// session that asked.
	ConversationClient, ConversationID string
}

func validGrantRequest(r GrantRequest) error {
	for _, p := range []struct{ name, v string }{{"project", r.Project}, {"from", r.From}} {
		if strings.TrimSpace(p.v) == "" || p.v != strings.TrimSpace(p.v) {
			return fmt.Errorf("%w: %s %q", ErrInvalidAccessRequest, p.name, p.v)
		}
	}
	switch {
	case r.Project == r.From:
		return fmt.Errorf("%w: a project always reads itself: nothing to grant", ErrInvalidAccessRequest)
	case r.Lease < time.Second || r.Lease%time.Second != 0:
		return fmt.Errorf("%w: lease %s is not a whole number of seconds, at least one", ErrInvalidAccessRequest, r.Lease)
	case r.TTL <= 0:
		return fmt.Errorf("%w: ttl %s must be positive", ErrInvalidAccessRequest, r.TTL)
	case r.ProcessKey != "" && r.PeerPID <= 0:
		return fmt.Errorf("%w: pid %d", ErrInvalidAccessRequest, r.PeerPID)
	case r.ProcessKey == "" && r.SessionID == "":
		return fmt.Errorf("%w: no holder: a grant is bound to the asking process or session", ErrInvalidAccessRequest)
	case utf8.RuneCountInString(r.Reason) > MaxAccessReasonRunes || !utf8.ValidString(r.Reason):
		return fmt.Errorf("%w: the reason is limited to %d characters of valid UTF-8", ErrInvalidAccessRequest, MaxAccessReasonRunes)
	}
	for _, f := range []struct{ name, v string }{{"program", r.Program}, {"session_id", r.SessionID},
		{"session_name", r.SessionName}, {"cwd", r.CWD}, {"repo", r.Repo}, {"model", r.Model}} {
		if len(f.v) > maxAccessFieldBytes || !utf8.ValidString(f.v) {
			return fmt.Errorf("%w: %s is limited to %d bytes of valid UTF-8", ErrInvalidAccessRequest, f.name, maxAccessFieldBytes)
		}
	}
	if r.Access != "" && !slices.Contains(GrantAccesses, r.Access) {
		return fmt.Errorf("%w: access %q: valid values are %s", ErrInvalidAccessRequest, r.Access, strings.Join(GrantAccesses, ", "))
	}
	if r.ConversationClient != "" || r.ConversationID != "" {
		if _, err := HolderOfConversation(r.ConversationClient, r.ConversationID); err != nil {
			return err
		}
	}
	if !r.Carry.IsZero() {
		c, err := CarryOf(r.Carry.Home, r.Carry.Holds, r.Carry.WriteTo)
		switch {
		case err != nil:
			return err
		case c.Home != r.From:
			return fmt.Errorf("%w: the carry's home %q is not the asking project %q", ErrInvalidAccessRequest, c.Home, r.From)
		case !slices.Contains(c.Holds, r.Project):
			return fmt.Errorf("%w: the carry does not hold %q, the project it asks for", ErrInvalidAccessRequest, r.Project)
		case r.Access == GrantAccessWrite && !slices.Contains(c.WriteTo, r.Project):
			return fmt.Errorf("%w: a write request's carry does not write to %q", ErrInvalidAccessRequest, r.Project)
		}
	}
	return nil
}

// CreateGrantRequest records a pending grant request (an access_approvals row
// of kind grant) with a fresh one-time challenge, waiting until now + req.TTL.
func CreateGrantRequest(ctx context.Context, db *sql.DB, req GrantRequest, now time.Time) (AccessApproval, error) {
	if err := validGrantRequest(req); err != nil {
		return AccessApproval{}, fmt.Errorf("store.CreateGrantRequest: %w", err)
	}
	id, err := core.NewID()
	if err != nil {
		return AccessApproval{}, fmt.Errorf("store.CreateGrantRequest: %w", err)
	}
	challenge := make([]byte, AccessChallengeBytes)
	if _, err := rand.Read(challenge); err != nil {
		return AccessApproval{}, fmt.Errorf("store.CreateGrantRequest: challenge: %w", err)
	}
	now = now.UTC()
	asked := req.Access
	if asked == "" {
		asked = GrantAccessRead
	}
	carry, err := carryJSON(req.Carry)
	if err != nil {
		return AccessApproval{}, fmt.Errorf("store.CreateGrantRequest: %w", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO access_approvals (id, project_slug, kind, challenge, status,
		requested_at, expires_at, lease_seconds, peer_uid, peer_pid, process_key, program,
		session_id, session_name, cwd, repo, model, reason, from_project,
		requested_access, carry, conversation_client, conversation_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, req.Project, AccessKindGrant, challenge, ApprovalPending, core.FormatTime(now), core.FormatTime(now.Add(req.TTL)),
		int64(req.Lease/time.Second), int64(req.PeerUID), int64(req.PeerPID), req.ProcessKey, req.Program,
		req.SessionID, req.SessionName, req.CWD, req.Repo, req.Model, req.Reason, req.From,
		asked, carry, req.ConversationClient, req.ConversationID); err != nil {
		return AccessApproval{}, fmt.Errorf("store.CreateGrantRequest: %w", err)
	}
	stored, err := AccessApprovalByID(ctx, db, id)
	if err != nil {
		return AccessApproval{}, fmt.Errorf("store.CreateGrantRequest: %w", err)
	}
	return stored, nil
}

// holderClause matches a grant (or grant request) row to one holder: the
// process keys under peer_auth -- never a session-keyed row -- else the
// session, and then never a process-keyed row. ok is false when there is no
// holder at all, which matches nothing.
func holderClause(processKeys []string, sessionID string) (string, []any, bool, error) {
	keys := make([]any, 0, len(processKeys))
	for _, k := range processKeys {
		if k != "" {
			keys = append(keys, k)
		}
	}
	switch {
	case len(keys) > access.MaxChainHops:
		return "", nil, false, fmt.Errorf("%w: %d process keys, at most %d", ErrInvalidAccessRequest, len(keys), access.MaxChainHops)
	case len(keys) > 0:
		return `process_key IN (` + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + `)`, keys, true, nil
	case len(processKeys) > 0:
		// Keys were named and every one was empty: a connection whose process
		// the kernel did not name holds nothing, and never falls back to a
		// session.
		return "", nil, false, nil
	case sessionID != "":
		return `process_key = '' AND session_id = ?`, []any{sessionID}, true, nil
	}
	return "", nil, false, nil
}

// PendingGrantRequest returns the newest grant request still waiting at now
// from this holder (processKey under peer_auth, else sessionID) for project
// from from, so a repeated refusal answers with the request it already filed.
func PendingGrantRequest(ctx context.Context, db *sql.DB, processKey, sessionID, project, from string, now time.Time) (AccessApproval, bool, error) {
	var keys []string
	if processKey != "" {
		keys = []string{processKey}
	}
	clause, args, ok, err := holderClause(keys, sessionID)
	if err != nil {
		return AccessApproval{}, false, fmt.Errorf("store.PendingGrantRequest: %w", err)
	}
	if !ok {
		return AccessApproval{}, false, nil
	}
	args = append(args, project, from, ApprovalPending, core.FormatTime(now), AccessKindGrant)
	a, err := scanApproval(db.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM access_approvals
		WHERE `+clause+` AND project_slug = ? AND from_project = ? AND status = ? AND expires_at > ? AND kind = ?
		ORDER BY requested_at DESC, id DESC LIMIT 1`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return AccessApproval{}, false, nil
	}
	if err != nil {
		return AccessApproval{}, false, fmt.Errorf("store.PendingGrantRequest: %w", err)
	}
	return a, true, nil
}

// LastGrantRequest returns the newest grant request from this holder
// (processKey under peer_auth, else sessionID) for project from from, whatever
// its status: a refusal reads it to tell a request the owner denied -- which it
// then names, filing nothing -- from one that only lapsed.
func LastGrantRequest(ctx context.Context, db *sql.DB, processKey, sessionID, project, from string) (AccessApproval, bool, error) {
	var keys []string
	if processKey != "" {
		keys = []string{processKey}
	}
	clause, args, ok, err := holderClause(keys, sessionID)
	if err != nil {
		return AccessApproval{}, false, fmt.Errorf("store.LastGrantRequest: %w", err)
	}
	if !ok {
		return AccessApproval{}, false, nil
	}
	args = append(args, project, from, AccessKindGrant)
	a, err := scanApproval(db.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM access_approvals
		WHERE `+clause+` AND project_slug = ? AND from_project = ? AND kind = ?
		ORDER BY requested_at DESC, id DESC LIMIT 1`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return AccessApproval{}, false, nil
	}
	if err != nil {
		return AccessApproval{}, false, fmt.Errorf("store.LastGrantRequest: %w", err)
	}
	return a, true, nil
}

// AccessGrant is one row of access_grants.
type AccessGrant struct {
	ID          string
	Project     string
	FromProject string
	ProcessKey  string
	SessionID   string
	PeerUID     uint32
	PeerPID     int32
	ApprovalID  string
	DecidedBy   string
	GrantedAt   time.Time
	ExpiresAt   time.Time

	RevokedAt     *time.Time
	RevokedReason string

	// Access is what the owner approved (migration 039): GrantAccessRead, or
	// GrantAccessWrite.
	Access string
	// ConversationClient and ConversationID name the conversation it was
	// granted to without peer_auth (migration 039), both "" otherwise.
	ConversationClient, ConversationID string
	// RelayID is the relay whose process holds the grant (migration 043): the
	// device id ProcessKey names (access.RelayOfKey), "" for this machine's
	// processes and for every grant without peer_auth. The approve
	// transaction writes it from the key.
	RelayID string
}

// Holder is who the grant belongs to: its process under peer_auth, else its
// conversation, else its session.
func (g AccessGrant) Holder() Holder {
	return holderOfRow(g.ProcessKey, g.ConversationClient, g.ConversationID, g.SessionID)
}

// Grant is the row as the access layer judges it.
func (g AccessGrant) Grant() access.Grant {
	return access.Grant{ID: g.ID, Project: g.Project, From: g.FromProject, Process: g.ProcessKey,
		Session: sessionIfNoProcess(g), Expires: g.ExpiresAt, Ended: g.RevokedAt != nil}
}

// sessionIfNoProcess is the session a grant is keyed on: only a grant with no
// process is a session's.
func sessionIfNoProcess(g AccessGrant) string {
	if g.ProcessKey != "" {
		return ""
	}
	return g.SessionID
}

// AccessGrants converts rows for access.JudgeGrants.
func AccessGrants(rows []AccessGrant) []access.Grant {
	out := make([]access.Grant, len(rows))
	for i, r := range rows {
		out[i] = r.Grant()
	}
	return out
}

const grantCols = `id, project_slug, from_project, process_key, session_id, peer_uid, peer_pid, approval_id,
	decided_by, granted_at, expires_at, revoked_at, revoked_reason, access, conversation_client, conversation_id, relay_id`

func scanGrants(rows *sql.Rows) ([]AccessGrant, error) {
	var out []AccessGrant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func scanGrant(row interface{ Scan(...any) error }) (AccessGrant, error) {
	var (
		g                AccessGrant
		granted, expires string
		revoked          sql.NullString
		uid, pid         int64
	)
	if err := row.Scan(&g.ID, &g.Project, &g.FromProject, &g.ProcessKey, &g.SessionID, &uid, &pid, &g.ApprovalID,
		&g.DecidedBy, &granted, &expires, &revoked, &g.RevokedReason, &g.Access,
		&g.ConversationClient, &g.ConversationID, &g.RelayID); err != nil {
		return AccessGrant{}, err
	}
	var err error
	if g.GrantedAt, err = core.ParseTime(granted); err != nil {
		return AccessGrant{}, fmt.Errorf("grant %s granted_at: %w", g.ID, err)
	}
	if g.ExpiresAt, err = core.ParseTime(expires); err != nil {
		return AccessGrant{}, fmt.Errorf("grant %s expires_at: %w", g.ID, err)
	}
	if revoked.Valid {
		t, err := core.ParseTime(revoked.String)
		if err != nil {
			return AccessGrant{}, fmt.Errorf("grant %s revoked_at: %w", g.ID, err)
		}
		g.RevokedAt = &t
	}
	g.PeerUID, g.PeerPID = uint32(uid), int32(pid)
	return g, nil
}

// ApproveAccessGrant turns a pending grant request into a grant, in one
// transaction, and is (with GrantAccessApprove, through the same transaction)
// the only writer of one: the request must be of kind
// grant, pending, before its deadline, and carry exactly the challenge the
// decision was made over (so a proof over one request can never approve
// another). Single use: a second approve of the same id is
// ErrApprovalNotPending. A live grant the same holder holds for the same
// project from the same project is revoked as refreshed -- replaced, never
// extended. decidedBy names the proof that decided; length is the grant's
// length, a whole number of seconds, or 0 for the length the request carried.
//
// The grant reads only (GrantAccessRead), whatever the request asked: the
// decision it records binds no access. It records no flow either; the grant
// row implies its own (HolderFlows). GrantAccessApprove is the approval that
// binds the owner's chosen access and the request's carry.
//
// A request found past its deadline is marked expired, and the call returns
// ErrApprovalExpired.
func ApproveAccessGrant(ctx context.Context, db *sql.DB, id string, challenge []byte, decidedBy string, length time.Duration, now time.Time) (AccessGrant, error) {
	g, _, err := grantAccessApprove(ctx, db, "store.ApproveAccessGrant", GrantDecision{ID: id, Challenge: challenge,
		DecidedBy: decidedBy, Length: length, Access: GrantAccessRead}, false, now)
	return g, err
}

// GrantAccessApprove is ApproveAccessGrant with the access the owner chose on
// the approval (GrantAccessRead or GrantAccessWrite), bound by the decision's
// proof, and the request's carry (isolation.requests, migration 039). In the
// same transaction it records the flows the carry lists as approved: the
// target held, and a write destination exactly when the owner chose write
// (whatever the request asked) -- each held project into each write
// destination (Carry.Flows). A request without a carry records the grant
// alone, whose own flows HolderFlows implies.
func GrantAccessApprove(ctx context.Context, db *sql.DB, id string, challenge []byte, decidedBy string, length time.Duration, grantAccess string, now time.Time) (AccessGrant, error) {
	g, _, err := grantAccessApprove(ctx, db, "store.GrantAccessApprove", GrantDecision{ID: id, Challenge: challenge,
		DecidedBy: decidedBy, Length: length, Access: grantAccess}, true, now)
	return g, err
}

// grantAccessApprove is the one approve transaction of a grant request. With
// d.Lease it also records the target's lease (approvalkind.go), bound to the
// request's process for the grant's length, and names it on the request.
func grantAccessApprove(ctx context.Context, db *sql.DB, op string, d GrantDecision, withFlows bool, now time.Time) (AccessGrant, *AccessLease, error) {
	id, challenge, decidedBy, length, grantAccess := d.ID, d.Challenge, d.DecidedBy, d.Length, d.Access
	if decidedBy == "" {
		return AccessGrant{}, nil, fmt.Errorf("%s: %w: no deciding proof named", op, ErrInvalidAccessRequest)
	}
	if length < 0 || length%time.Second != 0 {
		return AccessGrant{}, nil, fmt.Errorf("%s: %w: grant %s is not a whole number of seconds", op, ErrInvalidAccessRequest, length)
	}
	if !slices.Contains(GrantAccesses, grantAccess) {
		return AccessGrant{}, nil, fmt.Errorf("%s: %w: access %q: valid values are %s", op, ErrInvalidAccessRequest, grantAccess, strings.Join(GrantAccesses, ", "))
	}
	now = now.UTC()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return AccessGrant{}, nil, fmt.Errorf("%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	a, err := scanApproval(tx.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM access_approvals WHERE id = ?`, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AccessGrant{}, nil, fmt.Errorf("%s: %w: %q", op, ErrApprovalNotFound, id)
	case err != nil:
		return AccessGrant{}, nil, fmt.Errorf("%s: %w", op, err)
	case a.Kind != AccessKindGrant:
		return AccessGrant{}, nil, fmt.Errorf("%s: %w: %s is a %s request, not a read grant's", op, ErrInvalidAccessRequest, id, a.Kind)
	case a.Status != ApprovalPending:
		return AccessGrant{}, nil, fmt.Errorf("%s: %w: %s is %s", op, ErrApprovalNotPending, id, a.Status)
	}
	if !a.ExpiresAt.After(now) {
		if err := expireApprovalTx(ctx, tx, id, now); err != nil {
			return AccessGrant{}, nil, fmt.Errorf("%s: %w", op, err)
		}
		if err := tx.Commit(); err != nil {
			return AccessGrant{}, nil, fmt.Errorf("%s: commit: %w", op, err)
		}
		return AccessGrant{}, nil, fmt.Errorf("%s: %w: %s ended at %s", op, ErrApprovalExpired, id, core.FormatTime(a.ExpiresAt))
	}
	if len(challenge) != AccessChallengeBytes || subtle.ConstantTimeCompare(challenge, a.Challenge) != 1 {
		return AccessGrant{}, nil, fmt.Errorf("%s: %w", op, ErrApprovalChallenge)
	}
	holder := a.Holder()
	if a.FromProject == "" || a.FromProject == a.Project || (a.ProcessKey == "" && a.SessionID == "") || holder.IsZero() {
		return AccessGrant{}, nil, fmt.Errorf("%s: %w: %s names no holder or project to open it to", op, ErrInvalidAccessRequest, id)
	}
	if d.Lease && a.ProcessKey == "" {
		return AccessGrant{}, nil, fmt.Errorf("%s: %w: %s names no process to lease the project to", op, ErrInvalidAccessRequest, id)
	}
	var flows []FlowPair
	if withFlows && !a.Carry.IsZero() {
		if a.Carry.Home != a.FromProject {
			return AccessGrant{}, nil, fmt.Errorf("%s: %w: %s states the home %q, not its asking project %q",
				op, ErrInvalidAccessRequest, id, a.Carry.Home, a.FromProject)
		}
		flows = a.Carry.with(a.Project, grantAccess == GrantAccessWrite).Flows()
	}
	grantID, err := core.NewID()
	if err != nil {
		return AccessGrant{}, nil, fmt.Errorf("%s: %w", op, err)
	}
	leaseID := ""
	if d.Lease {
		if leaseID, err = core.NewID(); err != nil {
			return AccessGrant{}, nil, fmt.Errorf("%s: %w", op, err)
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE access_approvals SET status = ?, decided_at = ?, decided_by = ?, lease_id = ?
		WHERE id = ? AND status = ?`, ApprovalApproved, core.FormatTime(now), decidedBy, leaseID, id, ApprovalPending)
	if err != nil {
		return AccessGrant{}, nil, fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return AccessGrant{}, nil, fmt.Errorf("%s: %w", op, err)
	}
	if n != 1 {
		return AccessGrant{}, nil, fmt.Errorf("%s: %w: %s", op, ErrApprovalNotPending, id)
	}
	clause, args, _, err := holderAccessClause([]Holder{holder})
	if err != nil {
		return AccessGrant{}, nil, fmt.Errorf("%s: %w", op, err)
	}
	args = append([]any{core.FormatTime(now), LeaseRevokedRefreshed}, args...)
	args = append(args, a.Project, a.FromProject, core.FormatTime(now))
	if _, err := tx.ExecContext(ctx, `UPDATE access_grants SET revoked_at = ?, revoked_reason = ?
		WHERE `+clause+` AND project_slug = ? AND from_project = ? AND revoked_at IS NULL AND expires_at > ?`, args...); err != nil {
		return AccessGrant{}, nil, fmt.Errorf("%s: retire the previous grant: %w", op, err)
	}
	g := AccessGrant{
		ID: grantID, Project: a.Project, FromProject: a.FromProject, ProcessKey: a.ProcessKey, SessionID: a.SessionID,
		PeerUID: a.PeerUID, PeerPID: a.PeerPID, ApprovalID: a.ID, DecidedBy: decidedBy, GrantedAt: now, ExpiresAt: now.Add(a.Lease),
		Access: grantAccess, ConversationClient: a.ConversationClient, ConversationID: a.ConversationID,
		RelayID: access.RelayOfKey(a.ProcessKey),
	}
	if length > 0 {
		g.ExpiresAt = now.Add(length)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO access_grants (id, project_slug, from_project, process_key, session_id,
		peer_uid, peer_pid, approval_id, decided_by, granted_at, expires_at, access, conversation_client, conversation_id, relay_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.ID, g.Project, g.FromProject, g.ProcessKey, g.SessionID, int64(g.PeerUID), int64(g.PeerPID), g.ApprovalID,
		g.DecidedBy, core.FormatTime(g.GrantedAt), core.FormatTime(g.ExpiresAt), g.Access,
		g.ConversationClient, g.ConversationID, g.RelayID); err != nil {
		return AccessGrant{}, nil, fmt.Errorf("%s: %w", op, err)
	}
	if len(flows) > 0 {
		if _, err := flowRecordTx(ctx, tx, holder, a.ID, flows, now); err != nil {
			return AccessGrant{}, nil, fmt.Errorf("%s: %w", op, err)
		}
	}
	var lease *AccessLease
	if d.Lease {
		// The lock's half of the same approval: the target's lease, as long as
		// the grant, bound to the process the grant is.
		lease = &AccessLease{ID: leaseID, Project: a.Project, ProcessKey: a.ProcessKey, PeerUID: a.PeerUID, PeerPID: a.PeerPID,
			SessionID: a.SessionID, ApprovalID: a.ID, GrantedAt: now, ExpiresAt: g.ExpiresAt, RelayID: g.RelayID}
		if err := leaseInsertTx(ctx, tx, *lease); err != nil {
			return AccessGrant{}, nil, fmt.Errorf("%s: %w", op, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return AccessGrant{}, nil, fmt.Errorf("%s: commit: %w", op, err)
	}
	return g, lease, nil
}

// AccessGrantsForHolder returns every grant one holder was ever approved --
// live, expired and revoked alike, newest first, at most maxGrantRows: the
// live ones open reads, and every one confines writes (access.JudgeGrants).
// The holder is processKeys under peer_auth (a connection's verified chain),
// else sessionID; with neither it holds none.
func AccessGrantsForHolder(ctx context.Context, db *sql.DB, processKeys []string, sessionID string) ([]AccessGrant, error) {
	clause, args, ok, err := holderClause(processKeys, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store.AccessGrantsForHolder: %w", err)
	}
	if !ok {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+grantCols+` FROM access_grants WHERE `+clause+`
		ORDER BY granted_at DESC, id DESC LIMIT `+fmt.Sprint(maxGrantRows), args...)
	if err != nil {
		return nil, fmt.Errorf("store.AccessGrantsForHolder: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out, err := scanGrants(rows)
	if err != nil {
		return nil, fmt.Errorf("store.AccessGrantsForHolder: %w", err)
	}
	return out, nil
}

// HolderAccessGrants returns every grant any of holders was ever approved --
// live, expired and revoked alike, newest first, at most maxGrantRows -- by the
// holder each row names (Holder): a conversation's grants never answer for the
// session that asked, nor a session's for a conversation. With no holder it
// returns none.
func HolderAccessGrants(ctx context.Context, db *sql.DB, holders []Holder) ([]AccessGrant, error) {
	clause, args, ok, err := holderAccessClause(holders)
	if err != nil {
		return nil, fmt.Errorf("store.HolderAccessGrants: %w", err)
	}
	if !ok {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+grantCols+` FROM access_grants WHERE `+clause+`
		ORDER BY granted_at DESC, id DESC LIMIT `+fmt.Sprint(maxGrantRows), args...)
	if err != nil {
		return nil, fmt.Errorf("store.HolderAccessGrants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out, err := scanGrants(rows)
	if err != nil {
		return nil, fmt.Errorf("store.HolderAccessGrants: %w", err)
	}
	return out, nil
}

// HolderGrants judges the read grants of the holder ctx speaks for: the
// owner's socket process chain under peer_auth (a TCP request or a restricted
// account holds none), else sessionID. Every surface that confines or opens by
// a grant reads it here, so they cannot disagree about who holds one.
func HolderGrants(ctx context.Context, db *sql.DB, sessionID string, now time.Time) (access.Grants, error) {
	var keys []string
	if v, hasView := access.FromContext(ctx); hasView {
		p := v.Principal()
		if p.Kind != access.KindOwner || !p.HasProcess {
			return access.Grants{}, nil
		}
		if keys = p.ChainKeys(); len(keys) == 0 {
			return access.Grants{}, nil
		}
		sessionID = ""
	}
	rows, err := AccessGrantsForHolder(ctx, db, keys, sessionID)
	if err != nil {
		return access.Grants{}, err
	}
	if len(rows) == 0 {
		return access.Grants{}, nil
	}
	return access.JudgeGrants(ctx, sessionID, now, AccessGrants(rows)), nil
}

// LiveAccessGrants returns every grant live at now, soonest to end first: the
// access page's list, each with Revoke.
func LiveAccessGrants(ctx context.Context, db *sql.DB, now time.Time) ([]AccessGrant, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+grantCols+` FROM access_grants
		WHERE revoked_at IS NULL AND expires_at > ? ORDER BY expires_at, id`, core.FormatTime(now))
	if err != nil {
		return nil, fmt.Errorf("store.LiveAccessGrants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out, err := scanGrants(rows)
	if err != nil {
		return nil, fmt.Errorf("store.LiveAccessGrants: %w", err)
	}
	return out, nil
}

// AccessGrantByID loads one grant.
func AccessGrantByID(ctx context.Context, db *sql.DB, id string) (AccessGrant, error) {
	g, err := scanGrant(db.QueryRowContext(ctx, `SELECT `+grantCols+` FROM access_grants WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AccessGrant{}, fmt.Errorf("store.AccessGrantByID: %w: %q", ErrGrantNotFound, id)
	}
	if err != nil {
		return AccessGrant{}, fmt.Errorf("store.AccessGrantByID: %w", err)
	}
	return g, nil
}

// RevokeAccessGrant ends one live grant at now. A grant that already ended --
// revoked, expired, or never approved -- is ErrGrantNotLive. Its holder stays
// confined: ending a grant does not unread what it read.
func RevokeAccessGrant(ctx context.Context, db *sql.DB, id, reason string, now time.Time) error {
	if err := validRevokeReason(reason); err != nil {
		return fmt.Errorf("store.RevokeAccessGrant: %w", err)
	}
	res, err := db.ExecContext(ctx, `UPDATE access_grants SET revoked_at = ?, revoked_reason = ?
		WHERE id = ? AND revoked_at IS NULL AND expires_at > ?`, core.FormatTime(now), reason, id, core.FormatTime(now))
	if err != nil {
		return fmt.Errorf("store.RevokeAccessGrant: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.RevokeAccessGrant: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store.RevokeAccessGrant: %w: %q", ErrGrantNotLive, id)
	}
	return nil
}

// RevokeAccessGrantsForSession ends every live grant a session asked for (its
// end: "until the session ends" means this), returning how many. An empty
// session id is refused: it would name every request that named none.
func RevokeAccessGrantsForSession(ctx context.Context, db *sql.DB, sessionID, reason string, now time.Time) (int64, error) {
	if sessionID == "" {
		return 0, fmt.Errorf("store.RevokeAccessGrantsForSession: %w: empty session id is ambiguous", ErrInvalidAccessRequest)
	}
	if err := validRevokeReason(reason); err != nil {
		return 0, fmt.Errorf("store.RevokeAccessGrantsForSession: %w", err)
	}
	res, err := db.ExecContext(ctx, `UPDATE access_grants SET revoked_at = ?, revoked_reason = ?
		WHERE session_id = ? AND revoked_at IS NULL AND expires_at > ?`, core.FormatTime(now), reason, sessionID, core.FormatTime(now))
	if err != nil {
		return 0, fmt.Errorf("store.RevokeAccessGrantsForSession: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.RevokeAccessGrantsForSession: %w", err)
	}
	return n, nil
}

// RevokeAllAccessGrants ends every live grant (a passkey reset), returning
// how many.
func RevokeAllAccessGrants(ctx context.Context, db *sql.DB, reason string, now time.Time) (int64, error) {
	if err := validRevokeReason(reason); err != nil {
		return 0, fmt.Errorf("store.RevokeAllAccessGrants: %w", err)
	}
	res, err := db.ExecContext(ctx, `UPDATE access_grants SET revoked_at = ?, revoked_reason = ?
		WHERE revoked_at IS NULL AND expires_at > ?`, core.FormatTime(now), reason, core.FormatTime(now))
	if err != nil {
		return 0, fmt.Errorf("store.RevokeAllAccessGrants: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.RevokeAllAccessGrants: %w", err)
	}
	return n, nil
}
