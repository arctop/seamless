package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

// Read grants (isolation.requests, migration 034): requests ride
// access_approvals as kind grant, ApproveAccessGrant is their one writer, and
// the fence opens a confidential target to the one project a live grant was
// approved from -- never a write, never a sealed project.

const (
	grOps = "example-ops"
	grFin = "example-finance"
)

func grantReq(sessionID string) GrantRequest {
	return GrantRequest{
		Project: grFin, From: grOps, Lease: 2 * time.Hour, TTL: 10 * time.Minute,
		SessionID: sessionID, SessionName: "cc/ops", CWD: "/work/example-ops", Repo: "example-ops",
		Model: "claude-opus-5-5", Reason: "check the invoice totals",
	}
}

func TestMigration034_GrantTables(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM access_grants`).Scan(&n))
	require.Zero(t, n, "access_grants starts empty")
	// The new column defaults to '' for every lock request.
	a, err := CreateAccessApproval(ctx, db, leaseReq(grFin, access.ProcessToken{PID: 9, Version: 9, Boot: "b"}), time.Now())
	require.NoError(t, err)
	require.Equal(t, "", a.FromProject)
	// A grant names a holder and two different projects, enforced by the table too.
	_, err = db.ExecContext(ctx, `INSERT INTO access_grants (id, project_slug, from_project, approval_id, decided_by, granted_at, expires_at)
		VALUES ('01G', 'a', 'b', ?, 'x', 'x', 'x')`, a.ID)
	require.Error(t, err, "a grant without a process or a session is refused")
	_, err = db.ExecContext(ctx, `INSERT INTO access_grants (id, project_slug, from_project, session_id, approval_id, decided_by, granted_at, expires_at)
		VALUES ('01G', 'a', 'a', 'S', ?, 'x', 'x', 'x')`, a.ID)
	require.Error(t, err, "a grant of a project to itself is refused")
}

func TestCreateGrantRequest_Validates(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	for name, mutate := range map[string]func(*GrantRequest){
		"no project":      func(r *GrantRequest) { r.Project = "" },
		"no from":         func(r *GrantRequest) { r.From = "" },
		"itself":          func(r *GrantRequest) { r.From = r.Project },
		"no holder":       func(r *GrantRequest) { r.SessionID = "" },
		"process, no pid": func(r *GrantRequest) { r.ProcessKey = "b:1:1" },
		"long reason":     func(r *GrantRequest) { r.Reason = string(make([]rune, MaxAccessReasonRunes+1)) },
		"zero lease":      func(r *GrantRequest) { r.Lease = 0 },
	} {
		req := grantReq("S1")
		mutate(&req)
		_, err := CreateGrantRequest(ctx, db, req, now)
		require.ErrorIs(t, err, ErrInvalidAccessRequest, name)
	}
	a, err := CreateGrantRequest(ctx, db, grantReq("S1"), now)
	require.NoError(t, err)
	require.Equal(t, AccessKindGrant, a.Kind)
	require.Equal(t, grOps, a.FromProject)
	require.Equal(t, ApprovalPending, a.Status)
	require.Len(t, a.Challenge, AccessChallengeBytes)

	// A lock's request is never filed as a grant, nor a grant as a lock's.
	bad := leaseReq(grFin, access.ProcessToken{PID: 9, Version: 9, Boot: "b"})
	bad.Kind = AccessKindGrant
	_, err = CreateAccessApproval(ctx, db, bad, now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest)

	// The pending lookups keep the two apart, and holders apart.
	got, ok, err := PendingGrantRequest(ctx, db, "", "S1", grFin, grOps, now)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, a.ID, got.ID)
	_, ok, err = PendingGrantRequest(ctx, db, "", "S2", grFin, grOps, now)
	require.NoError(t, err)
	require.False(t, ok, "another session's request is not this one's")
	_, ok, err = PendingGrantRequest(ctx, db, "b:1:1", "S1", grFin, grOps, now)
	require.NoError(t, err)
	require.False(t, ok, "a process never finds a session's request")
	_, ok, err = PendingAccessApproval(ctx, db, "", grFin, now)
	require.NoError(t, err)
	require.False(t, ok, "a lock's pending lookup never returns a grant request")
}

func TestApproveAccessGrant_SingleUseChallengeBoundNeverALease(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	a, err := CreateGrantRequest(ctx, db, grantReq("S1"), now)
	require.NoError(t, err)

	// A grant request never becomes a lease.
	_, err = ApproveAccessFor(ctx, db, a.ID, a.Challenge, "owner", 0, now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest)
	var leases int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM access_leases`).Scan(&leases))
	require.Zero(t, leases)

	// The challenge binds the decision to this request.
	other := make([]byte, AccessChallengeBytes)
	_, err = ApproveAccessGrant(ctx, db, a.ID, other, "owner", 0, now)
	require.ErrorIs(t, err, ErrApprovalChallenge)
	_, err = ApproveAccessGrant(ctx, db, a.ID, a.Challenge, "", 0, now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest, "a decision names its proof")

	g, err := ApproveAccessGrant(ctx, db, a.ID, a.Challenge, "bearer-key", time.Hour, now)
	require.NoError(t, err)
	require.Equal(t, grFin, g.Project)
	require.Equal(t, grOps, g.FromProject)
	require.Equal(t, "S1", g.SessionID)
	require.Equal(t, "", g.ProcessKey)
	require.Equal(t, now.Add(time.Hour), g.ExpiresAt)
	stored, err := AccessApprovalByID(ctx, db, a.ID)
	require.NoError(t, err)
	require.Equal(t, ApprovalApproved, stored.Status)
	require.Equal(t, "bearer-key", stored.DecidedBy)

	// Single use.
	_, err = ApproveAccessGrant(ctx, db, a.ID, a.Challenge, "bearer-key", 0, now)
	require.ErrorIs(t, err, ErrApprovalNotPending)

	// A lock's request is never approved as a grant.
	lock, err := CreateAccessApproval(ctx, db, leaseReq(grFin, access.ProcessToken{PID: 9, Version: 9, Boot: "b"}), now)
	require.NoError(t, err)
	_, err = ApproveAccessGrant(ctx, db, lock.ID, lock.Challenge, "owner", 0, now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest)

	// A second approval for the same holder replaces the first.
	b, err := CreateGrantRequest(ctx, db, grantReq("S1"), now.Add(time.Minute))
	require.NoError(t, err)
	g2, err := ApproveAccessGrant(ctx, db, b.ID, b.Challenge, "bearer-key", 0, now.Add(time.Minute))
	require.NoError(t, err)
	live, err := LiveAccessGrants(ctx, db, now.Add(2*time.Minute))
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, g2.ID, live[0].ID)
	first, err := AccessGrantByID(ctx, db, g.ID)
	require.NoError(t, err)
	require.Equal(t, LeaseRevokedRefreshed, first.RevokedReason)

	// Expired requests are never approved.
	c, err := CreateGrantRequest(ctx, db, grantReq("S2"), now)
	require.NoError(t, err)
	_, err = ApproveAccessGrant(ctx, db, c.ID, c.Challenge, "bearer-key", 0, now.Add(time.Hour))
	require.ErrorIs(t, err, ErrApprovalExpired)
}

func TestAccessGrants_HolderRevokeAndExpiry(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	approve := func(req GrantRequest) AccessGrant {
		a, err := CreateGrantRequest(ctx, db, req, now)
		require.NoError(t, err)
		g, err := ApproveAccessGrant(ctx, db, a.ID, a.Challenge, "owner", 0, now)
		require.NoError(t, err)
		return g
	}
	sess := approve(grantReq("S1"))
	proc := grantReq("S2")
	proc.ProcessKey, proc.PeerPID, proc.PeerUID = "boot:200:2000", 200, 501
	byProc := approve(proc)

	got, err := AccessGrantsForHolder(ctx, db, nil, "S1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, sess.ID, got[0].ID)
	got, err = AccessGrantsForHolder(ctx, db, nil, "S2")
	require.NoError(t, err)
	require.Empty(t, got, "a process's grant is never a session's, even the session that asked")
	got, err = AccessGrantsForHolder(ctx, db, []string{"boot:300:3000", "boot:200:2000"}, "S1")
	require.NoError(t, err)
	require.Len(t, got, 1, "under peer_auth the chain is the holder, never the session")
	require.Equal(t, byProc.ID, got[0].ID)
	got, err = AccessGrantsForHolder(ctx, db, []string{""}, "S1")
	require.NoError(t, err)
	require.Empty(t, got, "a chain the kernel did not name holds nothing, and never falls back to the session")
	got, err = AccessGrantsForHolder(ctx, db, nil, "")
	require.NoError(t, err)
	require.Empty(t, got)

	// Revoke one; the session end revokes the other.
	require.NoError(t, RevokeAccessGrant(ctx, db, sess.ID, LeaseRevokedOwner, now.Add(time.Minute)))
	require.ErrorIs(t, RevokeAccessGrant(ctx, db, sess.ID, LeaseRevokedOwner, now.Add(time.Minute)), ErrGrantNotLive)
	n, err := RevokeAccessGrantsForSession(ctx, db, "S2", LeaseRevokedSessionEnded, now.Add(time.Minute))
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	_, err = RevokeAccessGrantsForSession(ctx, db, "", LeaseRevokedSessionEnded, now)
	require.ErrorIs(t, err, ErrInvalidAccessRequest)
	live, err := LiveAccessGrants(ctx, db, now.Add(2*time.Minute))
	require.NoError(t, err)
	require.Empty(t, live)
	// An ended grant still names its holder: it confines.
	got, err = AccessGrantsForHolder(ctx, db, nil, "S1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.True(t, got[0].Grant().Ended)
	_, err = AccessGrantByID(ctx, db, "01NOSUCHGRANT")
	require.ErrorIs(t, err, ErrGrantNotFound)
}

// The fence with grants: off (no grants in ctx) is exactly the matrix; on, a
// live grant opens a confidential target to the one project it came from,
// read-only, and a holder's writes are confined from then on.
func TestIsolationPolicy_Grants(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	for slug, state := range map[string]core.Isolation{
		grOps: core.IsolationOpen, "example-hr": core.IsolationOpen, grFin: core.IsolationConfidential,
		"example-legal": core.IsolationConfidential, "example-vault": core.IsolationSealed,
	} {
		require.NoError(t, CreateProject(ctx, db, core.Project{ID: "01" + slug, Slug: slug, Name: slug, Isolation: state,
			CreatedAt: now, UpdatedAt: now}))
	}
	check := func(ctx context.Context, caller, target string, read, write bool) {
		t.Helper()
		got, err := CanRead(ctx, db, caller, target)
		require.NoError(t, err)
		require.Equal(t, read, got, "CanRead(%s->%s)", caller, target)
		got, err = CanWrite(ctx, db, caller, target)
		require.NoError(t, err)
		require.Equal(t, write, got, "CanWrite(%s->%s)", caller, target)
	}
	// Off: nothing in ctx.
	check(ctx, grOps, grFin, false, true)
	check(ctx, grOps, "", true, true)

	grants := []access.Grant{
		{ID: "g1", Project: grFin, From: grOps, Session: "S1", Expires: now.Add(time.Hour)},
		{ID: "g2", Project: "example-vault", From: grOps, Session: "S1", Expires: now.Add(time.Hour)},
		{ID: "g3", Project: grOps, From: "example-legal", Session: "S1", Expires: now.Add(time.Hour)},
	}
	on := access.WithGrants(ctx, access.JudgeGrants(ctx, "S1", now, grants))
	check(on, grOps, grFin, true, false)                    // the grant reads; the target is never written
	check(on, "example-hr", grFin, false, false)            // only from the project it was approved from
	check(on, grOps, "example-legal", false, true)          // only the target it names; legal is writable: a grant was approved from it
	check(on, grOps, "example-vault", false, false)         // never a sealed project, whatever a row says
	check(on, grOps, grOps, true, true)                     // its own project, as ever
	check(on, grOps, "", true, false)                       // confined: the global scope is refused
	check(on, grOps, "example-hr", true, false)             // and every other project
	check(on, "example-legal", "example-legal", true, true) // a confidential From is in the confined set
	check(on, "", grFin, false, false)                      // the global scope is never granted

	// Another session's grants are not this one's.
	other := access.WithGrants(ctx, access.JudgeGrants(ctx, "S2", now, grants))
	check(other, grOps, grFin, false, true)

	// Expired: no read, still confined.
	later := access.WithGrants(ctx, access.JudgeGrants(ctx, "S1", now.Add(2*time.Hour), grants))
	check(later, grOps, grFin, false, false)
	check(later, grOps, grOps, true, true)

	// A target tightened to sealed after its grant reads nothing.
	require.NoError(t, SetProjectIsolation(ctx, db, grFin, core.IsolationSealed))
	check(on, grOps, grFin, false, false)
}

// A passkey reset ends every live read grant, as it ends every lease.
func TestResetApprovers_RevokesReadGrants(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	a, err := CreateGrantRequest(ctx, db, grantReq("S1"), now)
	require.NoError(t, err)
	g, err := ApproveAccessGrant(ctx, db, a.ID, a.Challenge, "owner", 0, now)
	require.NoError(t, err)
	_, _, err = ResetApprovers(ctx, db, now.Add(time.Minute))
	require.NoError(t, err)
	got, err := AccessGrantByID(ctx, db, g.ID)
	require.NoError(t, err)
	require.Equal(t, LeaseRevokedReset, got.RevokedReason)
}
