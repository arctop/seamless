package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

func lockOwnerResolve(name string) (access.Account, error) {
	if name == "owner" {
		return access.Account{UID: 501, Name: "owner"}, nil
	}
	return access.Account{}, fmt.Errorf("no such account %q", name)
}

// serve's project preflight names the lock as "locked", never "sealed": a
// locked project whose isolation is open is refused with the remedy, and a
// locked slug with no project behind it is refused as a typo that protects
// nothing.
func TestProjectProblems_LockedWording(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, slug := range []string{"example-finance", "example-legal"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, "example-legal", core.IsolationConfidential))

	policy, err := access.New(config.PeerAuth{Owner: "owner", ForceLocked: []string{"example-finance", "example-legal", "example-typo"}}, lockOwnerResolve)
	require.NoError(t, err)
	problems, err := projectProblems(ctx, db, policy)
	require.NoError(t, err)
	require.Equal(t, []string{
		`project "example-finance" is locked (peer_auth) but its isolation is open: make it confidential first (seam project isolation example-finance confidential --yes)`,
		`peer_auth names project "example-typo", which does not exist: if that is a typo, the project it meant is NOT protected`,
	}, problems)
}

// doctor names the locked list under its own key, and a filled-in list with
// peer_auth off is a warning rather than silence.
func TestPeerAuthChecks_LockedRows(t *testing.T) {
	cfg := config.Defaults()
	cfg.PeerAuth.ForceLocked = []string{"example-finance"}
	rows := peerAuthChecks(context.Background(), cfg, nil, peerAuthEnv{resolve: lockOwnerResolve})
	require.Len(t, rows, 1)
	require.Equal(t, statusWarn, rows[0].status)
	require.Contains(t, rows[0].detail, "peer_auth.force_locked is filled in")
	require.NotContains(t, rows[0].detail, "sealed")
}

// peerHandler on the owner's socket view, with the kernel's chain injected:
// it installs exactly the leases that name a process of the chain, on a
// locked project, live at now -- and fails closed on every lookup error.
func TestPeerHandler_InstallsOnlyMatchingLeases(t *testing.T) {
	const boot = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	bridge := access.ProcessToken{PID: 300, Version: 3000, Boot: boot}
	agent := access.ProcessToken{PID: 200, Version: 2000, Boot: boot}
	shell := access.ProcessToken{PID: 100, Version: 1000, Boot: boot}
	sibling := access.ProcessToken{PID: 210, Version: 2100, Boot: boot}
	chain := []access.ChainHop{{Token: bridge, Name: "seam"}, {Token: agent, Name: "claude"}, {Token: shell, Name: "zsh"}}

	policy, err := access.New(config.PeerAuth{Owner: "owner", ForceLocked: []string{"example-finance", "example-legal"}}, lockOwnerResolve)
	require.NoError(t, err)
	rows := []access.Lease{
		{ID: "agent-fin", Project: "example-finance", Process: agent.Key(), Expires: now.Add(time.Hour)},
		{ID: "sibling-legal", Project: "example-legal", Process: sibling.Key(), Expires: now.Add(time.Hour)},
		{ID: "agent-open", Project: "example-ops", Process: agent.Key(), Expires: now.Add(time.Hour)},
		{ID: "agent-legal-ended", Project: "example-legal", Process: agent.Key(), Expires: now},
	}
	var asked []string
	loader := peerLeases{
		load: func(_ context.Context, keys []string, at time.Time) ([]access.Lease, error) {
			require.Equal(t, now, at)
			asked = keys
			return rows, nil
		},
		now: func() time.Time { return now },
	}
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	serve := func(t *testing.T, policy *access.Policy, cred peerCred, leases peerLeases) access.View {
		t.Helper()
		var got access.View
		h := peerHandler(policy, leases, discard, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			v, ok := access.FromContext(r.Context())
			require.True(t, ok)
			got = v
		}))
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req = req.WithContext(context.WithValue(req.Context(), peerCredKey{}, cred))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		return got
	}
	withChain := func(hops []access.ChainHop, err error) peerCred {
		return peerCred{uid: 501, chain: func() ([]access.ChainHop, error) { return hops, err }}
	}

	v := serve(t, policy, withChain(chain, nil), loader)
	require.Equal(t, []string{bridge.Key(), agent.Key(), shell.Key()}, asked, "every process of the chain is looked up")
	require.Equal(t, access.Tokens(chain), v.Principal().Chain)
	require.True(t, v.Visible("example-finance"), "the agent's lease opens its project for the bridge below it")
	require.False(t, v.Visible("example-legal"), "a sibling's lease and an ended one open nothing")
	require.Len(t, v.Leases(), 1)
	require.Equal(t, "agent-fin", v.Leases()[0].ID)

	for name, cred := range map[string]peerCred{
		"no process for the connection": {uid: 501, chainErr: errors.New("LOCAL_PEERTOKEN failed")},
		"the chain cannot be walked":    withChain(nil, access.ErrPeerGone),
		"a chain the policy refuses":    withChain([]access.ChainHop{{Token: access.ProcessToken{PID: 1}}}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			asked = nil
			v := serve(t, policy, cred, loader)
			require.True(t, v.Locked("example-finance"), "fails closed")
			require.Empty(t, v.Leases())
			require.Nil(t, asked, "no lease is looked up without a chain")
		})
	}
	t.Run("the lease lookup fails", func(t *testing.T) {
		failing := peerLeases{load: func(context.Context, []string, time.Time) ([]access.Lease, error) {
			return nil, errors.New("database is locked")
		}}
		v := serve(t, policy, withChain(chain, nil), failing)
		require.True(t, v.Locked("example-finance"), "fails closed")
	})
	t.Run("no loader", func(t *testing.T) {
		require.True(t, serve(t, policy, withChain(chain, nil), peerLeases{}).Locked("example-finance"))
	})
	t.Run("nothing locked: no chain is walked", func(t *testing.T) {
		open, err := access.New(config.PeerAuth{Owner: "owner"}, lockOwnerResolve)
		require.NoError(t, err)
		walked := false
		cred := peerCred{uid: 501, chain: func() ([]access.ChainHop, error) { walked = true; return chain, nil }}
		v := serve(t, open, cred, loader)
		require.False(t, walked)
		require.False(t, v.Principal().HasProcess)

		// Read grants under peer_auth key on the chain: with chains on it is
		// walked and attached although nothing is locked.
		withChains := loader
		withChains.chains = true
		v = serve(t, open, cred, withChains)
		require.True(t, walked)
		require.True(t, v.Principal().HasProcess)
		require.Equal(t, access.Tokens(chain), v.Principal().Chain)
	})
}

// Both lists count for every one-shot command that asks what is locked.
func TestLockedProjects_FileAndConsole(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = store.EnsureProject(ctx, db, "example-conf", "example-conf")
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, db, "example-conf", core.IsolationConfidential))
	require.NoError(t, store.AddConsoleLock(ctx, db, "example-conf", "cred", time.Now()))

	cfg := config.Defaults()
	cfg.PeerAuth.ForceLocked = []string{"example-finance", "example-conf"}
	got, err := lockedProjects(ctx, cfg, db)
	require.NoError(t, err)
	require.Equal(t, []string{"example-conf", "example-finance"}, got)

	got, err = lockedProjects(ctx, cfg, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"example-conf", "example-finance"}, got, "no database: the file's list")

	require.False(t, locksProjects(ctx, cfg, db), "peer_auth off locks nothing")
	cfg.PeerAuth.Enabled, cfg.PeerAuth.ForceLocked = true, nil
	require.True(t, locksProjects(ctx, cfg, db), "a console lock registers access_request")
}
