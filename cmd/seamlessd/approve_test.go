package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// seamlessd approve commits only as the daemon's own account: the data dir
// is its and 0700, and it is not the owner.
func TestApproverAccountCheck(t *testing.T) {
	cfg := config.Defaults()
	cfg.DataDir = "/opt/seamless/data"
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", ForceLocked: []string{"example-finance"}}
	env := func(euid int, dirUID uint32, mode os.FileMode) peerAuthEnv {
		return peerAuthEnv{
			euid: euid,
			resolve: func(name string) (access.Account, error) {
				return access.Account{UID: 501, Name: name}, nil
			},
			inspect: func(string) (fileFacts, error) {
				return fileFacts{uid: dirUID, mode: os.ModeDir | mode, isDir: true}, nil
			},
		}
	}
	require.NoError(t, approverAccountCheck(cfg, env(480, 480, 0o700)), "the daemon's account")
	require.ErrorContains(t, approverAccountCheck(cfg, env(501, 480, 0o700)), "not the daemon's account", "the owner without sudo")
	require.ErrorContains(t, approverAccountCheck(cfg, env(480, 480, 0o755)), "not the daemon's account", "a data dir others can read is no proof")
	require.ErrorContains(t, approverAccountCheck(cfg, env(501, 501, 0o700)), "owner's account", "the owner's own data dir")

	nothingLocked := cfg
	nothingLocked.PeerAuth.ForceLocked = nil
	require.NoError(t, approverAccountCheck(nothingLocked, env(480, 480, 0o700)), "the console can lock a project the file does not list")
	disabled := cfg
	disabled.PeerAuth.Enabled = false
	require.ErrorContains(t, approverAccountCheck(disabled, env(480, 480, 0o700)), "peer_auth is off")
}

// With read grants on (isolation.requests), the daemon's account approves
// them as well, peer_auth locking nothing or not; without peer_auth there is
// no daemon-account commit at all, and the refusal says where to go.
func TestApproverAccountCheck_ReadGrants(t *testing.T) {
	cfg := config.Defaults()
	cfg.DataDir = "/opt/seamless/data"
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner"}
	cfg.Isolation.Requests = true
	env := peerAuthEnv{
		euid:    480,
		resolve: func(name string) (access.Account, error) { return access.Account{UID: 501, Name: name}, nil },
		inspect: func(string) (fileFacts, error) {
			return fileFacts{uid: 480, mode: os.ModeDir | 0o700, isDir: true}, nil
		},
	}
	require.NoError(t, approverAccountCheck(cfg, env), "read grants under peer_auth: the daemon's account approves them")
	advisory := cfg
	advisory.PeerAuth.Enabled = false
	require.ErrorContains(t, approverAccountCheck(advisory, env), "approved over the console with the bearer key")
}

// seamlessd approve's core decides a read grant request with the daemon
// account's proof, and never as a lease; with read grants off it refuses one.
func TestApprove_ReadGrantRequest(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for slug, state := range map[string]core.Isolation{"example-ops": core.IsolationOpen, "example-finance": core.IsolationConfidential} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, state))
	}
	a, err := store.CreateGrantRequest(ctx, db, store.GrantRequest{Project: "example-finance", From: "example-ops",
		Lease: 8 * time.Hour, TTL: 10 * time.Minute, PeerUID: 501, PeerPID: 200, ProcessKey: "boot:200:2000",
		SessionID: "01KSESSION", SessionName: "cc/ops"}, time.Now())
	require.NoError(t, err)
	svc := approval.New(approval.Config{DB: db, LeaseCap: 8 * time.Hour})
	facts, err := svc.Facts(ctx, a.ID)
	require.NoError(t, err)
	short := approval.ShortID(a.ID)
	args := []string{short, "--for", "1h", "--digest", facts.Digest}

	var out bytes.Buffer
	require.ErrorContains(t, approve(ctx, svc, &out, args), "read grants are off here")
	grants := approval.NewGrants(approval.GrantsConfig{Approvals: svc})
	require.NoError(t, approveWith(ctx, svc, grants, &out, args))
	require.Contains(t, out.String(), "Approved "+short+": pid 200, working in example-ops, may read example-finance (read-only) until ")
	require.Contains(t, out.String(), "; content from example-finance may be copied into example-ops's memories.\n")
	live, err := store.LiveAccessGrants(ctx, db, time.Now())
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, "daemon-account", live[0].DecidedBy)
	leases, err := store.LiveAccessLeases(ctx, db, time.Now())
	require.NoError(t, err)
	require.Empty(t, leases, "a read grant is never a lease")
}

// The terminal path (seam approve, through sudo to seamlessd approve) never
// approves a request an older daemon filed for a broad ancestor: the approve
// core refuses it, and the owner reads why.
func TestApprove_TerminalRefusesABroadAnchor(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = store.EnsureProject(ctx, db, "example-finance", "example-finance")
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, db, "example-finance", core.IsolationConfidential))
	a, err := store.CreateAccessApproval(ctx, db, store.AccessRequest{Project: "example-finance", Kind: store.AccessKindRequest,
		Lease: 8 * time.Hour, TTL: 10 * time.Minute, PeerUID: 501, PeerPID: 3698, ProcessKey: "boot:3698:36980",
		Program: "/opt/homebrew/Cellar/tmux/3.8/bin/tmux"}, time.Now())
	require.NoError(t, err)
	svc := approval.New(approval.Config{DB: db, LeaseCap: 8 * time.Hour})
	facts, err := svc.Facts(ctx, a.ID)
	require.NoError(t, err)
	var out bytes.Buffer
	err = approve(ctx, svc, &out, []string{approval.ShortID(a.ID), "--for", "1h", "--digest", facts.Digest})
	require.ErrorIs(t, err, approval.ErrBroadAnchor)
	require.ErrorContains(t, err, "tmux, a terminal multiplexer")
	require.Empty(t, out.String())
	leases, err := store.LiveAccessLeases(ctx, db, time.Now())
	require.NoError(t, err)
	require.Empty(t, leases)
}

// lockRig is one lock's request on a fresh database, an approval service on
// a clock the test moves, and the facts' digest as seam approve would show it.
type lockRig struct {
	db  *sql.DB
	svc *approval.Service
	now time.Time
	a   store.AccessApproval
}

func newLockRig(t *testing.T, program string) *lockRig {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = store.EnsureProject(ctx, db, "example-finance", "example-finance")
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, db, "example-finance", core.IsolationConfidential))
	r := &lockRig{db: db, now: time.Date(2026, 11, 8, 13, 53, 0, 0, time.UTC)}
	r.a, err = store.CreateAccessApproval(ctx, db, store.AccessRequest{Project: "example-finance", Kind: store.AccessKindRequest,
		Lease: 8 * time.Hour, TTL: 10 * time.Minute, PeerUID: 501, PeerPID: 4242, ProcessKey: "boot:4242:42420",
		Program: program}, r.now)
	require.NoError(t, err)
	r.svc = approval.New(approval.Config{DB: db, LeaseCap: 8 * time.Hour, Now: func() time.Time { return r.now }})
	return r
}

func (r *lockRig) digest(t *testing.T) string {
	t.Helper()
	f, err := r.svc.Facts(context.Background(), r.a.ID)
	require.NoError(t, err)
	return f.Digest
}

// What seamlessd approve tells the owner after sudo, one line each: the
// approval (who, what, until when), the denial, and the refusals the owner
// can act on -- facts changed, expired, decided meanwhile -- each still
// wrapping its sentinel.
func TestApprove_Outcomes(t *testing.T) {
	ctx := context.Background()
	const claude = "/Users/owner/.local/share/claude/versions/2.1.319"

	r := newLockRig(t, claude)
	short := approval.ShortID(r.a.ID)
	var out bytes.Buffer
	require.NoError(t, approve(ctx, r.svc, &out, []string{short, "--for", "session", "--digest", r.digest(t)}))
	require.Equal(t, "Approved "+short+": Claude Code 2.1.319 (pid 4242) may read and write example-finance until the session ends, "+
		"at the latest "+access.Clock(r.now.Add(8*time.Hour))+".\n", out.String())

	r = newLockRig(t, claude)
	out.Reset()
	require.NoError(t, approve(ctx, r.svc, &out, []string{r.a.ID, "--for", "1h", "--digest", r.digest(t)}))
	require.Equal(t, "Approved "+approval.ShortID(r.a.ID)+": Claude Code 2.1.319 (pid 4242) may read and write example-finance until "+
		access.Clock(r.now.Add(time.Hour))+".\n", out.String())

	// An agent the daemon does not name is its pid; its path is not printed.
	r = newLockRig(t, "/tmp/\x1b[2Jfake")
	out.Reset()
	require.NoError(t, approve(ctx, r.svc, &out, []string{r.a.ID, "--for", "1h", "--digest", r.digest(t)}))
	require.Contains(t, out.String(), ": pid 4242 may read and write example-finance until ")
	require.NotContains(t, out.String(), "\x1b")

	r = newLockRig(t, claude)
	out.Reset()
	require.NoError(t, approve(ctx, r.svc, &out, []string{r.a.ID, "--deny"}))
	require.Equal(t, "Denied "+approval.ShortID(r.a.ID)+": example-finance stays locked for that session.\n", out.String())

	for _, tc := range []struct {
		name  string
		setup func(r *lockRig) []string
		is    error
		want  func(r *lockRig) string
	}{
		{"facts changed", func(r *lockRig) []string { return []string{r.a.ID, "--for", "1h", "--digest", "00"} }, approval.ErrFactsChanged,
			func(r *lockRig) string {
				return "the facts of " + approval.ShortID(r.a.ID) + " changed since they were shown: run seam approve again and look at them first"
			}},
		{"expired before the approval", func(r *lockRig) []string {
			d := r.digest(t)
			r.now = r.a.ExpiresAt.Add(time.Second)
			return []string{r.a.ID, "--for", "1h", "--digest", d}
		}, store.ErrApprovalExpired, func(r *lockRig) string {
			return approval.ShortID(r.a.ID) + " expired at " + access.Clock(r.a.ExpiresAt) +
				", before this decision: nothing changed (the agent asks again with its next call to example-finance)"
		}},
		{"expired, then denied", func(r *lockRig) []string {
			_, err := store.ExpireAccessApprovals(ctx, r.db, r.a.ExpiresAt)
			require.NoError(t, err)
			return []string{r.a.ID, "--deny"}
		}, store.ErrApprovalNotPending, func(r *lockRig) string {
			return approval.ShortID(r.a.ID) + " expired at " + access.Clock(r.a.ExpiresAt) + ", before this decision: nothing changed " +
				"(the agent asks again with its next call to example-finance)"
		}},
		{"denied meanwhile", func(r *lockRig) []string {
			d := r.digest(t)
			require.NoError(t, r.svc.Deny(ctx, r.a.ID, "touchid-key"))
			return []string{r.a.ID, "--for", "1h", "--digest", d}
		}, store.ErrApprovalNotPending, func(r *lockRig) string {
			return approval.ShortID(r.a.ID) + " was already denied: nothing changed"
		}},
		{"approved meanwhile, then denied", func(r *lockRig) []string {
			_, err := r.svc.Approve(ctx, approval.Decision{ID: r.a.ID, Duration: "1h", Digest: r.digest(t), Proof: approval.Proof{Kind: approval.ProofDaemon}})
			require.NoError(t, err)
			return []string{r.a.ID, "--deny"}
		}, store.ErrApprovalNotPending, func(r *lockRig) string {
			return approval.ShortID(r.a.ID) + " was already approved: nothing changed"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLockRig(t, claude)
			args := tc.setup(r)
			var out bytes.Buffer
			err := approve(ctx, r.svc, &out, args)
			require.ErrorIs(t, err, tc.is)
			require.EqualError(t, err, tc.want(r))
			require.Empty(t, out.String())
		})
	}
}
