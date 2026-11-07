package main

import (
	"bytes"
	"context"
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
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", Locked: []string{"example-finance"}}
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

	off := cfg
	off.PeerAuth.Locked = nil
	require.ErrorContains(t, approverAccountCheck(off, env(480, 480, 0o700)), "locks no project")
	disabled := cfg
	disabled.PeerAuth.Enabled = false
	require.ErrorContains(t, approverAccountCheck(disabled, env(480, 480, 0o700)), "locks no project")
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
	require.Contains(t, out.String(), "Approved "+short+": example-finance, read-only, for example-ops, until ")
	require.Contains(t, out.String(), "content from example-finance may be copied into example-ops's memories")
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
