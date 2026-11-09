package store

// Access approvals and leases: the storage behind peer_auth.locked's unlocks
// (migration 031). An owner session asks for one locked project; the request
// waits here as a pending approval with a one-time challenge; the owner's
// approval signs that challenge; ApproveAccess turns the approval into a lease
// bound to the requesting connection's process. The access layer judges every
// lease again on every request (access.View.WithLeases), so a row here is never
// trusted on its own: this file only keeps the rows honest -- single use, never
// past their deadline, never extended in place.

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

// Approval kinds.
const (
	// AccessKindRequest is a first unlock.
	AccessKindRequest = "request"
	// AccessKindRefresh is a request from a session that already holds a lease
	// on the project; approving it retires that lease.
	AccessKindRefresh = "refresh"
	// AccessKindGrant is a request for a read grant of a confidential project
	// (isolation.requests; grants.go). It is filed by CreateGrantRequest and
	// approved by ApproveAccessGrant only: it never becomes a lease.
	AccessKindGrant = "grant"
)

// AccessKinds is the canonical kind set of a LOCK's requests, the ones
// CreateAccessApproval files and ApproveAccess turns into a lease. A grant
// request (AccessKindGrant) is deliberately not in it.
var AccessKinds = []string{AccessKindRequest, AccessKindRefresh}

// Approval statuses.
const (
	ApprovalPending  = "pending"
	ApprovalApproved = "approved"
	ApprovalDenied   = "denied"
	ApprovalExpired  = "expired"
)

// Why a lease ended before its expiry.
const (
	LeaseRevokedRefreshed    = "refreshed"
	LeaseRevokedSessionEnded = "session_ended"
	LeaseRevokedOwner        = "revoked"
	LeaseRevokedReset        = "reset"
)

// LeaseRevokeReasons is the canonical revoke-reason set.
var LeaseRevokeReasons = []string{LeaseRevokedRefreshed, LeaseRevokedSessionEnded, LeaseRevokedOwner, LeaseRevokedReset}

const (
	// AccessChallengeBytes is the size of an approval's one-time challenge.
	AccessChallengeBytes = 32
	// MaxAccessReasonRunes caps the agent's reason. It is shown on the approval
	// page, and a long free text there is a channel out of a session that
	// already read a locked project (a refresh).
	MaxAccessReasonRunes = 200
	// maxAccessFieldBytes caps every other descriptive field (program, cwd,
	// repo, model, session name).
	maxAccessFieldBytes = 1024
)

var (
	// ErrApprovalNotFound is an approval id with no row.
	ErrApprovalNotFound = errors.New("access approval not found")
	// ErrApprovalNotPending is a decision on an approval that was already
	// decided or expired: approvals are single use.
	ErrApprovalNotPending = errors.New("access approval is not pending")
	// ErrApprovalExpired is an approval decided after its deadline.
	ErrApprovalExpired = errors.New("access approval expired")
	// ErrApprovalChallenge is an approval whose challenge is not the one the
	// passkey signed.
	ErrApprovalChallenge = errors.New("access approval challenge does not match")
	// ErrInvalidAccessRequest is a request or decision this store refuses to
	// record as given.
	ErrInvalidAccessRequest = errors.New("invalid access request")
	// ErrLeaseNotLive is a revoke of a lease that already ended (or never was).
	ErrLeaseNotLive = errors.New("access lease is not live")
)

// AccessRequest is what CreateAccessApproval records: the project asked for,
// the kernel's facts about the caller, and what the request said about itself.
type AccessRequest struct {
	Project string
	Kind    string
	// Lease is how long the lease lasts once approved: a whole number of
	// seconds, at least one.
	Lease time.Duration
	// TTL is how long the request waits for a decision.
	TTL time.Duration

	PeerUID uint32
	PeerPID int32
	// ProcessKey is access.ProcessToken.Key of the requesting connection: the
	// only fact here a lease is bound to.
	ProcessKey string
	Program    string

	SessionID, SessionName, CWD, Repo, Model, Reason string
}

// AccessApproval is one row of access_approvals.
type AccessApproval struct {
	ID          string
	Project     string
	Kind        string
	Challenge   []byte
	Status      string
	RequestedAt time.Time
	ExpiresAt   time.Time
	Lease       time.Duration

	PeerUID    uint32
	PeerPID    int32
	ProcessKey string
	Program    string

	SessionID, SessionName, CWD, Repo, Model, Reason string

	// FromProject is a grant request's asking project (migration 034): the
	// grant opens Project to sessions working there. "" for a lock's request.
	FromProject string

	DecidedAt *time.Time
	DecidedBy string
	LeaseID   string
}

// AccessLease is one row of access_leases.
type AccessLease struct {
	ID         string
	Project    string
	ProcessKey string
	PeerUID    uint32
	PeerPID    int32
	SessionID  string
	ApprovalID string
	GrantedAt  time.Time
	ExpiresAt  time.Time

	RevokedAt     *time.Time
	RevokedReason string
}

// Lease is the row as the access layer judges it.
func (l AccessLease) Lease() access.Lease {
	return access.Lease{ID: l.ID, Project: l.Project, Process: l.ProcessKey, Expires: l.ExpiresAt}
}

// AccessLeases converts rows for access.View.WithLeases.
func AccessLeases(rows []AccessLease) []access.Lease {
	out := make([]access.Lease, len(rows))
	for i, r := range rows {
		out[i] = r.Lease()
	}
	return out
}

func validAccessRequest(r AccessRequest) error {
	switch {
	case strings.TrimSpace(r.Project) == "" || r.Project != strings.TrimSpace(r.Project):
		return fmt.Errorf("%w: project %q", ErrInvalidAccessRequest, r.Project)
	case !slices.Contains(AccessKinds, r.Kind):
		return fmt.Errorf("%w: kind %q: valid values are %s", ErrInvalidAccessRequest, r.Kind, strings.Join(AccessKinds, ", "))
	case r.Lease < time.Second || r.Lease%time.Second != 0:
		return fmt.Errorf("%w: lease %s is not a whole number of seconds, at least one", ErrInvalidAccessRequest, r.Lease)
	case r.TTL <= 0:
		return fmt.Errorf("%w: ttl %s must be positive", ErrInvalidAccessRequest, r.TTL)
	case r.ProcessKey == "":
		return fmt.Errorf("%w: no process: a lease is bound to the requesting connection's process", ErrInvalidAccessRequest)
	case r.PeerPID <= 0:
		return fmt.Errorf("%w: pid %d", ErrInvalidAccessRequest, r.PeerPID)
	case utf8.RuneCountInString(r.Reason) > MaxAccessReasonRunes || !utf8.ValidString(r.Reason):
		return fmt.Errorf("%w: the reason is limited to %d characters of valid UTF-8", ErrInvalidAccessRequest, MaxAccessReasonRunes)
	}
	for _, f := range []struct{ name, v string }{{"program", r.Program}, {"session_id", r.SessionID},
		{"session_name", r.SessionName}, {"cwd", r.CWD}, {"repo", r.Repo}, {"model", r.Model}} {
		if len(f.v) > maxAccessFieldBytes || !utf8.ValidString(f.v) {
			return fmt.Errorf("%w: %s is limited to %d bytes of valid UTF-8", ErrInvalidAccessRequest, f.name, maxAccessFieldBytes)
		}
	}
	return nil
}

// CreateAccessApproval records a pending approval with a fresh one-time
// challenge, waiting until now + req.TTL for a decision.
func CreateAccessApproval(ctx context.Context, db *sql.DB, req AccessRequest, now time.Time) (AccessApproval, error) {
	if err := validAccessRequest(req); err != nil {
		return AccessApproval{}, fmt.Errorf("store.CreateAccessApproval: %w", err)
	}
	id, err := core.NewID()
	if err != nil {
		return AccessApproval{}, fmt.Errorf("store.CreateAccessApproval: %w", err)
	}
	challenge := make([]byte, AccessChallengeBytes)
	if _, err := rand.Read(challenge); err != nil {
		return AccessApproval{}, fmt.Errorf("store.CreateAccessApproval: challenge: %w", err)
	}
	now = now.UTC()
	a := AccessApproval{
		ID: id, Project: req.Project, Kind: req.Kind, Challenge: challenge, Status: ApprovalPending,
		RequestedAt: now, ExpiresAt: now.Add(req.TTL), Lease: req.Lease,
		PeerUID: req.PeerUID, PeerPID: req.PeerPID, ProcessKey: req.ProcessKey, Program: req.Program,
		SessionID: req.SessionID, SessionName: req.SessionName, CWD: req.CWD, Repo: req.Repo, Model: req.Model, Reason: req.Reason,
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO access_approvals (id, project_slug, kind, challenge, status,
		requested_at, expires_at, lease_seconds, peer_uid, peer_pid, process_key, program,
		session_id, session_name, cwd, repo, model, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Project, a.Kind, a.Challenge, a.Status, core.FormatTime(a.RequestedAt), core.FormatTime(a.ExpiresAt),
		int64(a.Lease/time.Second), int64(a.PeerUID), int64(a.PeerPID), a.ProcessKey, a.Program,
		a.SessionID, a.SessionName, a.CWD, a.Repo, a.Model, a.Reason); err != nil {
		return AccessApproval{}, fmt.Errorf("store.CreateAccessApproval: %w", err)
	}
	// Read back at the column's precision, so the caller holds what a later read
	// returns.
	stored, err := AccessApprovalByID(ctx, db, a.ID)
	if err != nil {
		return AccessApproval{}, fmt.Errorf("store.CreateAccessApproval: %w", err)
	}
	return stored, nil
}

const approvalCols = `id, project_slug, kind, challenge, status, requested_at, expires_at, lease_seconds,
	peer_uid, peer_pid, process_key, program, session_id, session_name, cwd, repo, model, reason,
	decided_at, decided_by, lease_id, from_project`

func scanApproval(row interface{ Scan(...any) error }) (AccessApproval, error) {
	var (
		a                  AccessApproval
		requested, expires string
		decided            sql.NullString
		leaseSecs          int64
		uid, pid           int64
	)
	if err := row.Scan(&a.ID, &a.Project, &a.Kind, &a.Challenge, &a.Status, &requested, &expires, &leaseSecs,
		&uid, &pid, &a.ProcessKey, &a.Program, &a.SessionID, &a.SessionName, &a.CWD, &a.Repo, &a.Model, &a.Reason,
		&decided, &a.DecidedBy, &a.LeaseID, &a.FromProject); err != nil {
		return AccessApproval{}, err
	}
	var err error
	if a.RequestedAt, err = core.ParseTime(requested); err != nil {
		return AccessApproval{}, fmt.Errorf("approval %s requested_at: %w", a.ID, err)
	}
	if a.ExpiresAt, err = core.ParseTime(expires); err != nil {
		return AccessApproval{}, fmt.Errorf("approval %s expires_at: %w", a.ID, err)
	}
	if decided.Valid {
		t, err := core.ParseTime(decided.String)
		if err != nil {
			return AccessApproval{}, fmt.Errorf("approval %s decided_at: %w", a.ID, err)
		}
		a.DecidedAt = &t
	}
	a.Lease = time.Duration(leaseSecs) * time.Second
	a.PeerUID, a.PeerPID = uint32(uid), int32(pid)
	return a, nil
}

// AccessApprovalByID loads one approval.
func AccessApprovalByID(ctx context.Context, db *sql.DB, id string) (AccessApproval, error) {
	a, err := scanApproval(db.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM access_approvals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AccessApproval{}, fmt.Errorf("store.AccessApprovalByID: %w: %q", ErrApprovalNotFound, id)
	}
	if err != nil {
		return AccessApproval{}, fmt.Errorf("store.AccessApprovalByID: %w", err)
	}
	return a, nil
}

// PendingAccessApproval returns the newest lock request still waiting for a
// decision at now for this process and project, so a repeated request can
// answer with the link it already has instead of piling up challenges. A
// grant request is never one (PendingGrantRequest finds those).
func PendingAccessApproval(ctx context.Context, db *sql.DB, processKey, project string, now time.Time) (AccessApproval, bool, error) {
	a, err := scanApproval(db.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM access_approvals
		WHERE process_key = ? AND project_slug = ? AND status = ? AND expires_at > ? AND kind <> ?
		ORDER BY requested_at DESC, id DESC LIMIT 1`, processKey, project, ApprovalPending, core.FormatTime(now), AccessKindGrant))
	if errors.Is(err, sql.ErrNoRows) {
		return AccessApproval{}, false, nil
	}
	if err != nil {
		return AccessApproval{}, false, fmt.Errorf("store.PendingAccessApproval: %w", err)
	}
	return a, true, nil
}

// LastAccessApproval returns the newest lock request for this process and
// project, whatever its status: a refusal reads it to tell a request the owner
// denied -- which it then names, filing nothing -- from one that only lapsed.
// A grant request is never one (LastGrantRequest finds those).
func LastAccessApproval(ctx context.Context, db *sql.DB, processKey, project string) (AccessApproval, bool, error) {
	a, err := scanApproval(db.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM access_approvals
		WHERE process_key = ? AND project_slug = ? AND kind <> ?
		ORDER BY requested_at DESC, id DESC LIMIT 1`, processKey, project, AccessKindGrant))
	if errors.Is(err, sql.ErrNoRows) {
		return AccessApproval{}, false, nil
	}
	if err != nil {
		return AccessApproval{}, false, fmt.Errorf("store.LastAccessApproval: %w", err)
	}
	return a, true, nil
}

// FillAccessReason records reason on approval id, pending at now, when it
// carries none: a request a refusal filed (a refusal has no reason to give)
// that the agent then asks for in its own words. A request that already has a
// reason, a refresh (which carries the earlier reason, never new text), and
// one no longer pending are left as they are. The row is returned as it
// stands either way. The reason is one of the facts the owner approves over,
// so filling it changes their digest: an owner deciding over the facts as
// they stood is asked to look again.
func FillAccessReason(ctx context.Context, db *sql.DB, id, reason string, now time.Time) (AccessApproval, error) {
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > MaxAccessReasonRunes || !utf8.ValidString(reason) {
		return AccessApproval{}, fmt.Errorf("store.FillAccessReason: %w: the reason is limited to %d characters of valid UTF-8",
			ErrInvalidAccessRequest, MaxAccessReasonRunes)
	}
	if reason != "" {
		res, err := db.ExecContext(ctx, `UPDATE access_approvals SET reason = ?
			WHERE id = ? AND status = ? AND reason = '' AND kind <> ? AND expires_at > ?`,
			reason, id, ApprovalPending, AccessKindRefresh, core.FormatTime(now))
		if err != nil {
			return AccessApproval{}, fmt.Errorf("store.FillAccessReason: %w", err)
		}
		if _, err := res.RowsAffected(); err != nil {
			return AccessApproval{}, fmt.Errorf("store.FillAccessReason: %w", err)
		}
	}
	a, err := AccessApprovalByID(ctx, db, id)
	if err != nil {
		return AccessApproval{}, fmt.Errorf("store.FillAccessReason: %w", err)
	}
	return a, nil
}

// AccessApprovalsSince returns every approval requested at or after since,
// whatever its status, newest first: the requests a short id the owner was
// shown may still name once they are decided or expired.
func AccessApprovalsSince(ctx context.Context, db *sql.DB, since time.Time) ([]AccessApproval, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+approvalCols+` FROM access_approvals
		WHERE requested_at >= ? ORDER BY requested_at DESC, id DESC`, core.FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("store.AccessApprovalsSince: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AccessApproval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, fmt.Errorf("store.AccessApprovalsSince: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.AccessApprovalsSince: %w", err)
	}
	return out, nil
}

// PendingAccessApprovals returns every approval still waiting for a decision
// at now, oldest first: what `seam approve` and the console list.
func PendingAccessApprovals(ctx context.Context, db *sql.DB, now time.Time) ([]AccessApproval, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+approvalCols+` FROM access_approvals
		WHERE status = ? AND expires_at > ? ORDER BY requested_at, id`, ApprovalPending, core.FormatTime(now))
	if err != nil {
		return nil, fmt.Errorf("store.PendingAccessApprovals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AccessApproval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, fmt.Errorf("store.PendingAccessApprovals: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.PendingAccessApprovals: %w", err)
	}
	return out, nil
}

// LastAccessReason returns the reason of the newest lock request for this
// process and project, approved or not: what a refresh reuses, since a
// refresh carries no new free text.
func LastAccessReason(ctx context.Context, db *sql.DB, processKey, project string) (string, error) {
	var reason string
	err := db.QueryRowContext(ctx, `SELECT reason FROM access_approvals WHERE process_key = ? AND project_slug = ? AND kind <> ?
		ORDER BY requested_at DESC, id DESC LIMIT 1`, processKey, project, AccessKindGrant).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store.LastAccessReason: %w", err)
	}
	return reason, nil
}

// ApproveAccess turns a pending approval into a lease, in one transaction:
// the approval must be pending, before its deadline, and carry exactly the
// challenge the owner's passkey signed (checked here too, so an assertion over
// one approval can never approve another). Single use: a second approve of the
// same id is ErrApprovalNotPending. Any live lease the same process holds on
// the same project is revoked as refreshed -- a lease is replaced, never
// extended. credentialID names the passkey that decided.
//
// An approval found past its deadline is marked expired, and the call returns
// ErrApprovalExpired.
func ApproveAccess(ctx context.Context, db *sql.DB, id string, challenge []byte, credentialID string, now time.Time) (AccessLease, error) {
	return ApproveAccessFor(ctx, db, id, challenge, credentialID, 0, now)
}

// ApproveAccessFor is ApproveAccess with the lease length the owner chose at
// approval: lease, a whole number of seconds, or 0 for the length the request
// carried. decidedBy names the proof that decided (an approver credential id,
// or the daemon account's).
func ApproveAccessFor(ctx context.Context, db *sql.DB, id string, challenge []byte, credentialID string, length time.Duration, now time.Time) (AccessLease, error) {
	if credentialID == "" {
		return AccessLease{}, fmt.Errorf("store.ApproveAccess: %w: no deciding proof named", ErrInvalidAccessRequest)
	}
	if length < 0 || length%time.Second != 0 {
		return AccessLease{}, fmt.Errorf("store.ApproveAccess: %w: lease %s is not a whole number of seconds", ErrInvalidAccessRequest, length)
	}
	now = now.UTC()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return AccessLease{}, fmt.Errorf("store.ApproveAccess: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	a, err := scanApproval(tx.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM access_approvals WHERE id = ?`, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AccessLease{}, fmt.Errorf("store.ApproveAccess: %w: %q", ErrApprovalNotFound, id)
	case err != nil:
		return AccessLease{}, fmt.Errorf("store.ApproveAccess: %w", err)
	case a.Status != ApprovalPending:
		return AccessLease{}, fmt.Errorf("store.ApproveAccess: %w: %s is %s", ErrApprovalNotPending, id, a.Status)
	case a.Kind == AccessKindGrant:
		// A read grant is never a lease: ApproveAccessGrant is its one writer.
		return AccessLease{}, fmt.Errorf("store.ApproveAccess: %w: %s is a read grant request, not a lock's", ErrInvalidAccessRequest, id)
	}
	if !a.ExpiresAt.After(now) {
		if err := expireApprovalTx(ctx, tx, id, now); err != nil {
			return AccessLease{}, fmt.Errorf("store.ApproveAccess: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return AccessLease{}, fmt.Errorf("store.ApproveAccess: commit: %w", err)
		}
		return AccessLease{}, fmt.Errorf("store.ApproveAccess: %w: %s ended at %s", ErrApprovalExpired, id, core.FormatTime(a.ExpiresAt))
	}
	if len(challenge) != AccessChallengeBytes || subtle.ConstantTimeCompare(challenge, a.Challenge) != 1 {
		return AccessLease{}, fmt.Errorf("store.ApproveAccess: %w", ErrApprovalChallenge)
	}
	leaseID, err := core.NewID()
	if err != nil {
		return AccessLease{}, fmt.Errorf("store.ApproveAccess: %w", err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE access_approvals SET status = ?, decided_at = ?, decided_by = ?, lease_id = ?
		WHERE id = ? AND status = ?`, ApprovalApproved, core.FormatTime(now), credentialID, leaseID, id, ApprovalPending)
	if err != nil {
		return AccessLease{}, fmt.Errorf("store.ApproveAccess: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return AccessLease{}, fmt.Errorf("store.ApproveAccess: %w", err)
	}
	if n != 1 {
		return AccessLease{}, fmt.Errorf("store.ApproveAccess: %w: %s", ErrApprovalNotPending, id)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE access_leases SET revoked_at = ?, revoked_reason = ?
		WHERE process_key = ? AND project_slug = ? AND revoked_at IS NULL AND expires_at > ?`,
		core.FormatTime(now), LeaseRevokedRefreshed, a.ProcessKey, a.Project, core.FormatTime(now)); err != nil {
		return AccessLease{}, fmt.Errorf("store.ApproveAccess: retire the previous lease: %w", err)
	}
	lease := AccessLease{
		ID: leaseID, Project: a.Project, ProcessKey: a.ProcessKey, PeerUID: a.PeerUID, PeerPID: a.PeerPID,
		SessionID: a.SessionID, ApprovalID: a.ID, GrantedAt: now, ExpiresAt: now.Add(a.Lease),
	}
	if length > 0 {
		lease.ExpiresAt = now.Add(length)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO access_leases (id, project_slug, process_key, peer_uid, peer_pid,
		session_id, approval_id, granted_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		lease.ID, lease.Project, lease.ProcessKey, int64(lease.PeerUID), int64(lease.PeerPID), lease.SessionID,
		lease.ApprovalID, core.FormatTime(lease.GrantedAt), core.FormatTime(lease.ExpiresAt)); err != nil {
		return AccessLease{}, fmt.Errorf("store.ApproveAccess: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AccessLease{}, fmt.Errorf("store.ApproveAccess: commit: %w", err)
	}
	return lease, nil
}

func expireApprovalTx(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE access_approvals SET status = ?, decided_at = ? WHERE id = ? AND status = ?`,
		ApprovalExpired, core.FormatTime(now), id, ApprovalPending)
	if err != nil {
		return err
	}
	if _, err := res.RowsAffected(); err != nil {
		return err
	}
	return nil
}

// DenyAccess records the owner's refusal of a pending approval. Single use, as
// approving is.
func DenyAccess(ctx context.Context, db *sql.DB, id, credentialID string, now time.Time) error {
	res, err := db.ExecContext(ctx, `UPDATE access_approvals SET status = ?, decided_at = ?, decided_by = ?
		WHERE id = ? AND status = ?`, ApprovalDenied, core.FormatTime(now), credentialID, id, ApprovalPending)
	if err != nil {
		return fmt.Errorf("store.DenyAccess: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.DenyAccess: %w", err)
	}
	if n == 1 {
		return nil
	}
	if _, err := AccessApprovalByID(ctx, db, id); err != nil {
		return fmt.Errorf("store.DenyAccess: %w", err)
	}
	return fmt.Errorf("store.DenyAccess: %w: %s", ErrApprovalNotPending, id)
}

// ExpireAccessApprovals marks every pending approval past its deadline at now
// as expired, returning how many.
func ExpireAccessApprovals(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `UPDATE access_approvals SET status = ?, decided_at = ?
		WHERE status = ? AND expires_at <= ?`, ApprovalExpired, core.FormatTime(now), ApprovalPending, core.FormatTime(now))
	if err != nil {
		return 0, fmt.Errorf("store.ExpireAccessApprovals: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.ExpireAccessApprovals: %w", err)
	}
	return n, nil
}

const leaseCols = `id, project_slug, process_key, peer_uid, peer_pid, session_id, approval_id,
	granted_at, expires_at, revoked_at, revoked_reason`

func scanLeases(rows *sql.Rows) ([]AccessLease, error) {
	var out []AccessLease
	for rows.Next() {
		var (
			l                AccessLease
			granted, expires string
			revoked          sql.NullString
			uid, pid         int64
		)
		if err := rows.Scan(&l.ID, &l.Project, &l.ProcessKey, &uid, &pid, &l.SessionID, &l.ApprovalID,
			&granted, &expires, &revoked, &l.RevokedReason); err != nil {
			return nil, err
		}
		var err error
		if l.GrantedAt, err = core.ParseTime(granted); err != nil {
			return nil, fmt.Errorf("lease %s granted_at: %w", l.ID, err)
		}
		if l.ExpiresAt, err = core.ParseTime(expires); err != nil {
			return nil, fmt.Errorf("lease %s expires_at: %w", l.ID, err)
		}
		if revoked.Valid {
			t, err := core.ParseTime(revoked.String)
			if err != nil {
				return nil, fmt.Errorf("lease %s revoked_at: %w", l.ID, err)
			}
			l.RevokedAt = &t
		}
		l.PeerUID, l.PeerPID = uint32(uid), int32(pid)
		out = append(out, l)
	}
	return out, rows.Err()
}

// ClaimAccessLeaseBriefing reports whether this caller is the first to brief
// lease id, and records that it is at now. The row is the claim, so it holds
// across a daemon restart, and two concurrent calls cannot both win. A failure
// claims nothing.
func ClaimAccessLeaseBriefing(ctx context.Context, db *sql.DB, id string, now time.Time) (bool, error) {
	res, err := db.ExecContext(ctx, `UPDATE access_leases SET briefed_at = ? WHERE id = ? AND briefed_at IS NULL`,
		core.FormatTime(now), id)
	if err != nil {
		return false, fmt.Errorf("store.ClaimAccessLeaseBriefing: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store.ClaimAccessLeaseBriefing: %w", err)
	}
	return n == 1, nil
}

// ForgetAccessLeaseBriefing undoes a claim whose briefing could not be built,
// so the next result tries again rather than a briefing nobody received
// counting as sent.
func ForgetAccessLeaseBriefing(ctx context.Context, db *sql.DB, id string) error {
	if _, err := db.ExecContext(ctx, `UPDATE access_leases SET briefed_at = NULL WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store.ForgetAccessLeaseBriefing: %w", err)
	}
	return nil
}

// ActiveAccessLeases returns the leases live at now for one process: not
// revoked, expiring after now. An empty key -- a connection whose process the
// kernel did not name -- holds none.
func ActiveAccessLeases(ctx context.Context, db *sql.DB, processKey string, now time.Time) ([]AccessLease, error) {
	return ActiveAccessLeasesFor(ctx, db, []string{processKey}, now)
}

// ActiveAccessLeasesFor returns the leases live at now for any of the given
// processes: a socket connection's process chain (access.Peer.Chain), whose
// agent process is the one a lease names. These are what the transport hands
// the access layer, which judges them again (access.View.WithLeases). Empty
// keys are ignored, and more than access.MaxChainHops keys are refused rather
// than truncated: a longer list did not come from a chain.
func ActiveAccessLeasesFor(ctx context.Context, db *sql.DB, processKeys []string, now time.Time) ([]AccessLease, error) {
	keys := make([]any, 0, len(processKeys)+1)
	for _, k := range processKeys {
		if k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	if len(keys) > access.MaxChainHops {
		return nil, fmt.Errorf("store.ActiveAccessLeasesFor: %w: %d process keys, at most %d", ErrInvalidAccessRequest, len(keys), access.MaxChainHops)
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	rows, err := db.QueryContext(ctx, `SELECT `+leaseCols+` FROM access_leases
		WHERE process_key IN (`+marks+`) AND revoked_at IS NULL AND expires_at > ?
		ORDER BY project_slug, expires_at DESC, id`, append(keys, core.FormatTime(now))...)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveAccessLeasesFor: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out, err := scanLeases(rows)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveAccessLeasesFor: %w", err)
	}
	return out, nil
}

// AccessLeasesForSession returns the leases live at now that a session's
// requests were granted, for the expiry warnings its hook injections carry. An
// empty session id is refused: requests that named no session all share it,
// and answering for them together would hand one session another's leases.
func AccessLeasesForSession(ctx context.Context, db *sql.DB, sessionID string, now time.Time) ([]AccessLease, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("store.AccessLeasesForSession: %w: empty session id is ambiguous", ErrInvalidAccessRequest)
	}
	rows, err := db.QueryContext(ctx, `SELECT `+leaseCols+` FROM access_leases
		WHERE session_id = ? AND revoked_at IS NULL AND expires_at > ?
		ORDER BY project_slug, expires_at DESC, id`, sessionID, core.FormatTime(now))
	if err != nil {
		return nil, fmt.Errorf("store.AccessLeasesForSession: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out, err := scanLeases(rows)
	if err != nil {
		return nil, fmt.Errorf("store.AccessLeasesForSession: %w", err)
	}
	return out, nil
}

func validRevokeReason(reason string) error {
	if !slices.Contains(LeaseRevokeReasons, reason) {
		return fmt.Errorf("%w: revoke reason %q: valid values are %s", ErrInvalidAccessRequest, reason, strings.Join(LeaseRevokeReasons, ", "))
	}
	return nil
}

// RevokeAccessLease ends one live lease at now. A lease that already ended --
// revoked, expired, or never granted -- is ErrLeaseNotLive.
func RevokeAccessLease(ctx context.Context, db *sql.DB, id, reason string, now time.Time) error {
	if err := validRevokeReason(reason); err != nil {
		return fmt.Errorf("store.RevokeAccessLease: %w", err)
	}
	res, err := db.ExecContext(ctx, `UPDATE access_leases SET revoked_at = ?, revoked_reason = ?
		WHERE id = ? AND revoked_at IS NULL AND expires_at > ?`, core.FormatTime(now), reason, id, core.FormatTime(now))
	if err != nil {
		return fmt.Errorf("store.RevokeAccessLease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.RevokeAccessLease: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store.RevokeAccessLease: %w: %q", ErrLeaseNotLive, id)
	}
	return nil
}

// RevokeAccessLeasesForSession ends every live lease a session holds (its
// session_end), returning how many. An empty session id is refused, as in
// AccessLeasesForSession.
func RevokeAccessLeasesForSession(ctx context.Context, db *sql.DB, sessionID, reason string, now time.Time) (int64, error) {
	if sessionID == "" {
		return 0, fmt.Errorf("store.RevokeAccessLeasesForSession: %w: empty session id is ambiguous", ErrInvalidAccessRequest)
	}
	if err := validRevokeReason(reason); err != nil {
		return 0, fmt.Errorf("store.RevokeAccessLeasesForSession: %w", err)
	}
	res, err := db.ExecContext(ctx, `UPDATE access_leases SET revoked_at = ?, revoked_reason = ?
		WHERE session_id = ? AND revoked_at IS NULL AND expires_at > ?`, core.FormatTime(now), reason, sessionID, core.FormatTime(now))
	if err != nil {
		return 0, fmt.Errorf("store.RevokeAccessLeasesForSession: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.RevokeAccessLeasesForSession: %w", err)
	}
	return n, nil
}

// RevokeAllAccessLeases ends every live lease (a passkey reset), returning how
// many.
func RevokeAllAccessLeases(ctx context.Context, db *sql.DB, reason string, now time.Time) (int64, error) {
	if err := validRevokeReason(reason); err != nil {
		return 0, fmt.Errorf("store.RevokeAllAccessLeases: %w", err)
	}
	res, err := db.ExecContext(ctx, `UPDATE access_leases SET revoked_at = ?, revoked_reason = ?
		WHERE revoked_at IS NULL AND expires_at > ?`, core.FormatTime(now), reason, core.FormatTime(now))
	if err != nil {
		return 0, fmt.Errorf("store.RevokeAllAccessLeases: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.RevokeAllAccessLeases: %w", err)
	}
	return n, nil
}
