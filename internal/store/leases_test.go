package store

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

func leaseDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func leaseReq(project string, proc access.ProcessToken) AccessRequest {
	return AccessRequest{
		Project: project, Kind: AccessKindRequest, Lease: 3 * time.Hour, TTL: 10 * time.Minute,
		PeerUID: 501, PeerPID: proc.PID, ProcessKey: proc.Key(), Program: "/opt/seamless/bin/seam",
		SessionID: "01SESSIONFIN", SessionName: "cc/fin", CWD: "/work/finance-os", Repo: "finance-os",
		Model: "claude-opus-5-5", Reason: "reconcile the ledger notes",
	}
}

func TestMigration031_AccessTables(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	var v int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v))
	require.GreaterOrEqual(t, v, 31)
	for _, table := range []string{"access_approvals", "access_leases"} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n))
		require.Equal(t, 1, n, table)
		require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n))
		require.Zero(t, n, "%s starts empty", table)
	}
	insert := func(id string, challenge []byte, leaseSecs int) error {
		_, err := db.ExecContext(ctx, `INSERT INTO access_approvals (id, project_slug, kind, challenge, status, requested_at,
			expires_at, lease_seconds, peer_uid, peer_pid, process_key) VALUES (?, 'p', 'request', ?, 'pending', 'x', 'x', ?, 501, 1, 'k')`,
			id, challenge, leaseSecs)
		return err
	}
	require.Error(t, insert("01SHORT", make([]byte, 31), 60), "the challenge is exactly 32 bytes")
	require.Error(t, insert("01NOLEASE", make([]byte, 32), 0), "a lease lasts at least a second")
	require.NoError(t, insert("01OK", make([]byte, 32), 60))
	_, err := db.ExecContext(ctx, `INSERT INTO access_leases (id, project_slug, process_key, peer_uid, peer_pid, approval_id,
		granted_at, expires_at) VALUES ('01L', 'p', 'k', 501, 1, 'no-such-approval', 'x', 'x')`)
	require.Error(t, err, "a lease names a real approval")
}

func TestAccessApproval_CreateAndPending(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	a, err := CreateAccessApproval(ctx, db, leaseReq("example-finance", fenceProc), fenceNow)
	require.NoError(t, err)
	require.Equal(t, ApprovalPending, a.Status)
	require.Len(t, a.Challenge, AccessChallengeBytes)
	require.False(t, bytes.Equal(a.Challenge, make([]byte, AccessChallengeBytes)))
	require.Equal(t, fenceNow.Add(10*time.Minute), a.ExpiresAt)
	require.Equal(t, 3*time.Hour, a.Lease)
	require.Equal(t, fenceProc.Key(), a.ProcessKey)
	require.Equal(t, int32(fenceProc.PID), a.PeerPID)
	require.Equal(t, "reconcile the ledger notes", a.Reason)
	require.Nil(t, a.DecidedAt)

	// A second request a second later (ULIDs within one millisecond are not
	// ordered, so "newest" is by requested_at).
	b, err := CreateAccessApproval(ctx, db, leaseReq("example-finance", fenceProc), fenceNow.Add(time.Second))
	require.NoError(t, err)
	require.NotEqual(t, a.ID, b.ID)
	require.False(t, bytes.Equal(a.Challenge, b.Challenge), "every approval has its own challenge")

	got, ok, err := PendingAccessApproval(ctx, db, fenceProc.Key(), "example-finance", fenceNow.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, b.ID, got.ID, "the newest pending request")
	for name, probe := range map[string]struct {
		key, project string
		at           time.Time
	}{
		"another process":         {fenceOther.Key(), "example-finance", fenceNow},
		"another project":         {fenceProc.Key(), "example-legal", fenceNow},
		"after the deadline":      {fenceProc.Key(), "example-finance", fenceNow.Add(10*time.Minute + time.Second)},
		"long after the deadline": {fenceProc.Key(), "example-finance", fenceNow.Add(time.Hour)},
	} {
		_, ok, err := PendingAccessApproval(ctx, db, probe.key, probe.project, probe.at)
		require.NoError(t, err)
		require.False(t, ok, name)
	}
	_, err = AccessApprovalByID(ctx, db, "01NOPE")
	require.ErrorIs(t, err, ErrApprovalNotFound)
}

func TestAccessApproval_Refusals(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	for name, mutate := range map[string]func(*AccessRequest){
		"no project":                 func(r *AccessRequest) { r.Project = "" },
		"a padded project":           func(r *AccessRequest) { r.Project = " example-finance" },
		"an unknown kind":            func(r *AccessRequest) { r.Kind = "forever" },
		"a lease under a second":     func(r *AccessRequest) { r.Lease = time.Millisecond },
		"a fractional-second lease":  func(r *AccessRequest) { r.Lease = 1500 * time.Millisecond },
		"no ttl":                     func(r *AccessRequest) { r.TTL = 0 },
		"no process":                 func(r *AccessRequest) { r.ProcessKey = "" },
		"no pid":                     func(r *AccessRequest) { r.PeerPID = 0 },
		"an over-long reason":        func(r *AccessRequest) { r.Reason = strings.Repeat("x", MaxAccessReasonRunes+1) },
		"a reason that is not UTF-8": func(r *AccessRequest) { r.Reason = "\xff" },
		"an over-long cwd":           func(r *AccessRequest) { r.CWD = "/" + strings.Repeat("d", maxAccessFieldBytes) },
	} {
		t.Run(name, func(t *testing.T) {
			req := leaseReq("example-finance", fenceProc)
			mutate(&req)
			_, err := CreateAccessApproval(ctx, db, req, fenceNow)
			require.ErrorIs(t, err, ErrInvalidAccessRequest)
		})
	}
	req := leaseReq("example-finance", fenceProc)
	req.Reason = strings.Repeat("\xc3\xa9", MaxAccessReasonRunes)
	_, err := CreateAccessApproval(ctx, db, req, fenceNow)
	require.NoError(t, err, "the cap counts characters, not bytes")
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM access_approvals`).Scan(&n))
	require.Equal(t, 1, n, "a refused request records nothing")
}

func TestApproveAccess_SingleUseAndChallengeBound(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	a, err := CreateAccessApproval(ctx, db, leaseReq("example-finance", fenceProc), fenceNow)
	require.NoError(t, err)
	other, err := CreateAccessApproval(ctx, db, leaseReq("example-legal", fenceProc), fenceNow)
	require.NoError(t, err)

	at := fenceNow.Add(2 * time.Minute)
	_, err = ApproveAccess(ctx, db, a.ID, other.Challenge, "cred-1", at)
	require.ErrorIs(t, err, ErrApprovalChallenge, "an assertion over another approval's challenge approves nothing")
	_, err = ApproveAccess(ctx, db, a.ID, nil, "cred-1", at)
	require.ErrorIs(t, err, ErrApprovalChallenge)
	_, err = ApproveAccess(ctx, db, a.ID, a.Challenge, "", at)
	require.ErrorIs(t, err, ErrInvalidAccessRequest, "a decision names its passkey")
	still, err := AccessApprovalByID(ctx, db, a.ID)
	require.NoError(t, err)
	require.Equal(t, ApprovalPending, still.Status, "a refused approve changes nothing")

	lease, err := ApproveAccess(ctx, db, a.ID, a.Challenge, "cred-1", at)
	require.NoError(t, err)
	require.Equal(t, "example-finance", lease.Project)
	require.Equal(t, fenceProc.Key(), lease.ProcessKey)
	require.Equal(t, at.Add(3*time.Hour), lease.ExpiresAt)
	require.Equal(t, a.SessionID, lease.SessionID)
	require.Equal(t, a.ID, lease.ApprovalID)

	decided, err := AccessApprovalByID(ctx, db, a.ID)
	require.NoError(t, err)
	require.Equal(t, ApprovalApproved, decided.Status)
	require.Equal(t, "cred-1", decided.DecidedBy)
	require.Equal(t, lease.ID, decided.LeaseID)
	require.NotNil(t, decided.DecidedAt)

	_, err = ApproveAccess(ctx, db, a.ID, a.Challenge, "cred-1", at)
	require.ErrorIs(t, err, ErrApprovalNotPending, "a replayed approval grants nothing")
	require.ErrorIs(t, DenyAccess(ctx, db, a.ID, "cred-1", at), ErrApprovalNotPending)
	_, err = ApproveAccess(ctx, db, "01NOPE", a.Challenge, "cred-1", at)
	require.ErrorIs(t, err, ErrApprovalNotFound)

	live, err := ActiveAccessLeases(ctx, db, fenceProc.Key(), at)
	require.NoError(t, err)
	require.Len(t, live, 1, "one approval, one lease")
}

func TestApproveAccess_ConcurrentApprovesGrantOnce(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	a, err := CreateAccessApproval(ctx, db, leaseReq("example-finance", fenceProc), fenceNow)
	require.NoError(t, err)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
		errs []error
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ApproveAccess(ctx, db, a.ID, a.Challenge, "cred-1", fenceNow.Add(time.Minute))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else {
				errs = append(errs, err)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, wins)
	for _, err := range errs {
		require.ErrorIs(t, err, ErrApprovalNotPending)
	}
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM access_leases`).Scan(&n))
	require.Equal(t, 1, n)
}

func TestApproveAccess_ExpiredAndDenied(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	late, err := CreateAccessApproval(ctx, db, leaseReq("example-finance", fenceProc), fenceNow)
	require.NoError(t, err)
	_, err = ApproveAccess(ctx, db, late.ID, late.Challenge, "cred-1", late.ExpiresAt)
	require.ErrorIs(t, err, ErrApprovalExpired, "the deadline itself is too late")
	got, err := AccessApprovalByID(ctx, db, late.ID)
	require.NoError(t, err)
	require.Equal(t, ApprovalExpired, got.Status, "found past its deadline, it is marked expired")
	_, err = ApproveAccess(ctx, db, late.ID, late.Challenge, "cred-1", fenceNow)
	require.ErrorIs(t, err, ErrApprovalNotPending, "an expired approval never comes back")

	denied, err := CreateAccessApproval(ctx, db, leaseReq("example-finance", fenceProc), fenceNow)
	require.NoError(t, err)
	require.NoError(t, DenyAccess(ctx, db, denied.ID, "cred-1", fenceNow))
	_, err = ApproveAccess(ctx, db, denied.ID, denied.Challenge, "cred-1", fenceNow)
	require.ErrorIs(t, err, ErrApprovalNotPending)
	require.ErrorIs(t, DenyAccess(ctx, db, "01NOPE", "", fenceNow), ErrApprovalNotFound)

	stale, err := CreateAccessApproval(ctx, db, leaseReq("example-legal", fenceProc), fenceNow)
	require.NoError(t, err)
	n, err := ExpireAccessApprovals(ctx, db, fenceNow.Add(9*time.Minute))
	require.NoError(t, err)
	require.Zero(t, n, "nothing is past its deadline yet")
	n, err = ExpireAccessApprovals(ctx, db, fenceNow.Add(10*time.Minute))
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	got, err = AccessApprovalByID(ctx, db, stale.ID)
	require.NoError(t, err)
	require.Equal(t, ApprovalExpired, got.Status)

	leases, err := ActiveAccessLeases(ctx, db, fenceProc.Key(), fenceNow)
	require.NoError(t, err)
	require.Empty(t, leases, "no expired or denied approval granted a lease")
}

func TestAccessLeases_RefreshRevokeExpire(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	grant := func(project string, proc access.ProcessToken, kind string, at time.Time) AccessLease {
		t.Helper()
		req := leaseReq(project, proc)
		req.Kind = kind
		a, err := CreateAccessApproval(ctx, db, req, at)
		require.NoError(t, err)
		l, err := ApproveAccess(ctx, db, a.ID, a.Challenge, "cred-1", at)
		require.NoError(t, err)
		return l
	}
	first := grant("example-finance", fenceProc, AccessKindRequest, fenceNow)
	legal := grant("example-legal", fenceProc, AccessKindRequest, fenceNow)
	elsewhere := grant("example-finance", fenceOther, AccessKindRequest, fenceNow)

	// A refresh is a new approval and a new 3h lease; the old one retires.
	refreshAt := fenceNow.Add(2*time.Hour + 40*time.Minute)
	second := grant("example-finance", fenceProc, AccessKindRefresh, refreshAt)
	require.Equal(t, refreshAt.Add(3*time.Hour), second.ExpiresAt, "a fresh 3h from the approval, not an extension")
	live, err := ActiveAccessLeases(ctx, db, fenceProc.Key(), refreshAt)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{second.ID, legal.ID}, leaseIDs(live))
	var reason string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT revoked_reason FROM access_leases WHERE id = ?`, first.ID).Scan(&reason))
	require.Equal(t, LeaseRevokedRefreshed, reason)
	other, err := ActiveAccessLeases(ctx, db, fenceOther.Key(), refreshAt)
	require.NoError(t, err)
	require.Equal(t, []string{elsewhere.ID}, leaseIDs(other), "another process's lease is its own")

	// Expiry: legal's lease ends 3h after fenceNow.
	live, err = ActiveAccessLeases(ctx, db, fenceProc.Key(), fenceNow.Add(3*time.Hour))
	require.NoError(t, err)
	require.Equal(t, []string{second.ID}, leaseIDs(live), "a lease is gone at its expiry instant")
	require.ErrorIs(t, RevokeAccessLease(ctx, db, legal.ID, LeaseRevokedOwner, fenceNow.Add(3*time.Hour)), ErrLeaseNotLive)

	none, err := ActiveAccessLeases(ctx, db, "", refreshAt)
	require.NoError(t, err)
	require.Empty(t, none, "a connection with no process holds nothing")

	// By session: the warnings' lookup, and session_end's revoke.
	bySession, err := AccessLeasesForSession(ctx, db, "01SESSIONFIN", refreshAt)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{second.ID, legal.ID, elsewhere.ID}, leaseIDs(bySession))
	_, err = AccessLeasesForSession(ctx, db, "", refreshAt)
	require.ErrorIs(t, err, ErrInvalidAccessRequest)
	_, err = RevokeAccessLeasesForSession(ctx, db, "", LeaseRevokedSessionEnded, refreshAt)
	require.ErrorIs(t, err, ErrInvalidAccessRequest)
	_, err = RevokeAccessLeasesForSession(ctx, db, "01SESSIONFIN", "because", refreshAt)
	require.ErrorIs(t, err, ErrInvalidAccessRequest, "a revoke names a known reason")
	n, err := RevokeAccessLeasesForSession(ctx, db, "01SESSIONFIN", LeaseRevokedSessionEnded, refreshAt)
	require.NoError(t, err)
	require.Equal(t, int64(3), n)
	live, err = ActiveAccessLeases(ctx, db, fenceProc.Key(), refreshAt)
	require.NoError(t, err)
	require.Empty(t, live)

	// One lease by id, and everything at once.
	third := grant("example-finance", fenceProc, AccessKindRequest, refreshAt)
	require.NoError(t, RevokeAccessLease(ctx, db, third.ID, LeaseRevokedOwner, refreshAt))
	require.ErrorIs(t, RevokeAccessLease(ctx, db, third.ID, LeaseRevokedOwner, refreshAt), ErrLeaseNotLive)
	grant("example-finance", fenceProc, AccessKindRequest, refreshAt)
	grant("example-legal", fenceOther, AccessKindRequest, refreshAt)
	n, err = RevokeAllAccessLeases(ctx, db, LeaseRevokedReset, refreshAt)
	require.NoError(t, err)
	require.Equal(t, int64(2), n)
}

// The loop closed: leases loaded from rows, handed to the owner's socket view,
// drive the store fence -- leased, expired, revoked, and another process.
func TestAccessLeases_DriveTheLockedView(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	for _, slug := range []string{"example-finance", "example-legal"} {
		_, err := EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, SetProjectIsolation(ctx, db, slug, core.IsolationConfidential))
	}
	p := lockedPolicy(t)
	a, err := CreateAccessApproval(ctx, db, leaseReq("example-finance", fenceProc), fenceNow)
	require.NoError(t, err)
	lease, err := ApproveAccess(ctx, db, a.ID, a.Challenge, "cred-1", fenceNow)
	require.NoError(t, err)

	// request is what the transport does per request: load the process's live
	// leases at now and hand them to its view.
	request := func(proc access.ProcessToken, now time.Time) context.Context {
		rows, err := ActiveAccessLeases(ctx, db, proc.Key(), now)
		require.NoError(t, err)
		v, ok := p.PeerProcessView(501, proc)
		require.True(t, ok)
		return access.WithView(ctx, v.WithLeases(now, AccessLeases(rows)))
	}
	reach := func(rctx context.Context, project string) (bool, bool) {
		r, err := CanRead(rctx, db, project, project)
		require.NoError(t, err)
		w, err := CanWrite(rctx, db, project, project)
		require.NoError(t, err)
		return r, w
	}
	for _, tc := range []struct {
		name        string
		ctx         context.Context
		project     string
		read, write bool
	}{
		{"leased, at the grant", request(fenceProc, fenceNow), "example-finance", true, true},
		{"leased, a minute before expiry", request(fenceProc, lease.ExpiresAt.Add(-time.Minute)), "example-finance", true, true},
		{"the lease opens only its project", request(fenceProc, fenceNow), "example-legal", false, false},
		{"lease expired", request(fenceProc, lease.ExpiresAt), "example-finance", false, false},
		{"another process of the owner", request(fenceOther, fenceNow), "example-finance", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, w := reach(tc.ctx, tc.project)
			require.Equal(t, tc.read, r, "read")
			require.Equal(t, tc.write, w, "write")
		})
	}
	// Revoked (session_end): the very next request is locked again.
	_, err = RevokeAccessLeasesForSession(ctx, db, a.SessionID, LeaseRevokedSessionEnded, fenceNow.Add(time.Minute))
	require.NoError(t, err)
	r, w := reach(request(fenceProc, fenceNow.Add(2*time.Minute)), "example-finance")
	require.False(t, r)
	require.False(t, w)

	// A row that names the process but is handed to the TCP view (the bearer
	// key) opens nothing: the access layer, not the row, decides.
	rows := []AccessLease{{ID: "forged", Project: "example-finance", ProcessKey: fenceProc.Key(), ExpiresAt: fenceNow.Add(time.Hour)}}
	tcp := access.WithView(ctx, p.OwnerView().WithLeases(fenceNow, AccessLeases(rows)))
	r, w = reach(tcp, "example-finance")
	require.False(t, r)
	require.False(t, w)
}

func leaseIDs(ls []AccessLease) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.ID
	}
	return out
}

// A socket request's leases are looked up for its whole process chain: the
// lease names the agent process, and every process below it carries the agent
// in its chain.
func TestActiveAccessLeasesFor_Chain(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	agent := fenceProc
	child := access.ProcessToken{PID: 6001, Version: 9, Boot: fenceProc.Boot}
	a, err := CreateAccessApproval(ctx, db, leaseReq("example-finance", agent), fenceNow)
	require.NoError(t, err)
	lease, err := ApproveAccess(ctx, db, a.ID, a.Challenge, "cred-1", fenceNow)
	require.NoError(t, err)
	b, err := CreateAccessApproval(ctx, db, leaseReq("example-legal", fenceOther), fenceNow)
	require.NoError(t, err)
	_, err = ApproveAccess(ctx, db, b.ID, b.Challenge, "cred-1", fenceNow)
	require.NoError(t, err)

	got, err := ActiveAccessLeasesFor(ctx, db, []string{child.Key(), agent.Key()}, fenceNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, []string{lease.ID}, leaseIDs(got), "the agent's lease, found from its child's chain; another process's is not")

	got, err = ActiveAccessLeasesFor(ctx, db, []string{child.Key()}, fenceNow.Add(time.Minute))
	require.NoError(t, err)
	require.Empty(t, got, "a chain without the agent holds nothing")

	got, err = ActiveAccessLeasesFor(ctx, db, []string{"", ""}, fenceNow)
	require.NoError(t, err)
	require.Empty(t, got)

	long := make([]string, access.MaxChainHops+1)
	for i := range long {
		long[i] = access.ProcessToken{PID: int32(i + 2), Version: 1, Boot: fenceProc.Boot}.Key()
	}
	_, err = ActiveAccessLeasesFor(ctx, db, long, fenceNow)
	require.ErrorIs(t, err, ErrInvalidAccessRequest, "a list longer than any chain is refused, not truncated")
}

// FillAccessReason writes the reason a refusal's request lacks, once: never
// over a reason, never on a refresh (which carries the earlier one), and
// never once the request is decided or past its deadline.
func TestFillAccessReason_OnlyAMissingOne(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	bare := leaseReq("example-finance", fenceProc)
	bare.Reason = ""
	a, err := CreateAccessApproval(ctx, db, bare, fenceNow)
	require.NoError(t, err)

	got, err := FillAccessReason(ctx, db, a.ID, "  the ledger  ", fenceNow)
	require.NoError(t, err)
	require.Equal(t, "the ledger", got.Reason)
	got, err = FillAccessReason(ctx, db, a.ID, "something else", fenceNow)
	require.NoError(t, err)
	require.Equal(t, "the ledger", got.Reason, "a reason is written once")
	_, err = FillAccessReason(ctx, db, a.ID, strings.Repeat("x", MaxAccessReasonRunes+1), fenceNow)
	require.ErrorIs(t, err, ErrInvalidAccessRequest)

	refresh := bare
	refresh.Kind = AccessKindRefresh
	r, err := CreateAccessApproval(ctx, db, refresh, fenceNow)
	require.NoError(t, err)
	got, err = FillAccessReason(ctx, db, r.ID, "new words", fenceNow)
	require.NoError(t, err)
	require.Empty(t, got.Reason, "a refresh carries no new free text")

	late, err := CreateAccessApproval(ctx, db, bare, fenceNow)
	require.NoError(t, err)
	got, err = FillAccessReason(ctx, db, late.ID, "too late", late.ExpiresAt)
	require.NoError(t, err)
	require.Empty(t, got.Reason, "past its deadline")

	// Requested a second later, so it is the newest (ids minted in the same
	// millisecond do not sort by time).
	denied, err := CreateAccessApproval(ctx, db, bare, fenceNow.Add(time.Second))
	require.NoError(t, err)
	require.NoError(t, DenyAccess(ctx, db, denied.ID, "cred-1", fenceNow.Add(time.Second)))
	got, err = FillAccessReason(ctx, db, denied.ID, "after the fact", fenceNow.Add(time.Second))
	require.NoError(t, err)
	require.Empty(t, got.Reason, "decided")

	last, ok, err := LastAccessApproval(ctx, db, fenceProc.Key(), "example-finance")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, denied.ID, last.ID, "the newest, whatever its status")
	_, ok, err = LastAccessApproval(ctx, db, fenceProc.Key(), "example-legal")
	require.NoError(t, err)
	require.False(t, ok)
}
