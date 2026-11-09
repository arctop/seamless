package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// The console under a locked project, wired as serve wires it (a view pool,
// the console's base handle, peerHandler with the store's leases): a leased
// agent tree's `seam plan show` -- GET /console/plans/<slug> as JSON over the
// socket -- reads the locked project, while an unleased tree on the socket
// and the TCP key are refused.
func TestConsoleLock_LeasedTreeReadsThroughTheConsole(t *testing.T) {
	ctx := context.Background()
	const (
		boot   = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"
		secret = "LEDGER-PLAN-SECRET"
		plan   = "ledger-plan"
	)
	agent := access.ProcessToken{PID: 200, Version: 2000, Boot: boot}
	cli := access.ProcessToken{PID: 320, Version: 3200, Boot: boot}
	stranger := access.ProcessToken{PID: 400, Version: 4000, Boot: boot}

	dir := t.TempDir()
	path := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, slug := range []string{"example-finance", "example-ops"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, "example-finance", core.IsolationConfidential))
	mgr, err := files.NewManager(filepath.Join(dir, "data"), db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	noteID, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = mgr.WriteNote(ctx, core.Note{ID: noteID, Slug: "ledger-narrative", Title: "Ledger plan", Project: "example-finance",
		Body: "# Ledger\n\n" + secret, Tags: []string{plans.SlugTag(plan), "created-by:agent"}, Created: now, Updated: now})
	require.NoError(t, err)

	cfg := config.Defaults()
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", Locked: []string{"example-finance"}}
	policy, err := access.New(cfg.PeerAuth, lockOwnerResolve)
	require.NoError(t, err)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := events.NewRecorder(db)
	svc, err := newApprovals(cfg, db, rec, logger)
	require.NoError(t, err)

	// The agent's tree holds a lease, approved on the terminal path.
	a, err := store.CreateAccessApproval(ctx, db, store.AccessRequest{Project: "example-finance", Kind: store.AccessKindRequest,
		Lease: time.Hour, TTL: time.Minute, PeerUID: 501, PeerPID: agent.PID, ProcessKey: agent.Key()}, now)
	require.NoError(t, err)
	_, err = store.ApproveAccessFor(ctx, db, a.ID, a.Challenge, "daemon-account", time.Hour, now)
	require.NoError(t, err)

	views, err := store.OpenViews(path, policy.OwnerLocked())
	require.NoError(t, err)
	t.Cleanup(func() { _ = views.Close() })
	base, err := views.For(nil)
	require.NoError(t, err)
	cs, err := console.New(console.Config{
		DB: base, WriteDB: db, Files: mgr, Events: rec.ReadingFrom(base), APIKey: "k", DataDir: filepath.Join(dir, "data"),
		Approvals: svc, Views: views, Logger: logger,
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	cs.Register(mux)

	serveAs := func(chain ...access.ProcessToken) *httptest.Server {
		hops := make([]access.ChainHop, len(chain))
		for i, c := range chain {
			hops[i] = access.ChainHop{Token: c}
		}
		cred := peerCred{uid: 501, chain: func() ([]access.ChainHop, error) { return hops, nil }}
		h := peerHandler(policy, storeLeases(db), logger, mux)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerCredKey{}, cred)))
		}))
		t.Cleanup(func() { closeServer(srv) })
		return srv
	}
	get := func(base, key string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, base+"/console/plans/"+url.PathEscape(plan)+"?format=json", nil)
		require.NoError(t, err)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(body)
	}

	status, body := get(serveAs(cli, agent).URL, "")
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, secret, "the leased tree's seam plan show reads the locked project")

	status, body = get(serveAs(stranger).URL, "")
	require.Equal(t, http.StatusForbidden, status, "another tree on the socket is refused")
	require.Contains(t, body, "is locked", "and told the lock, not not-found")
	require.NotContains(t, body, secret)

	tcp := httptest.NewServer(ownerHandler(policy, mux))
	t.Cleanup(func() { closeServer(tcp) })
	status, body = get(tcp.URL, "k")
	require.Equal(t, http.StatusForbidden, status, "the TCP key never reaches a locked project")
	require.Contains(t, body, "the TCP connection and its bearer key never reach a locked project")
	require.NotContains(t, body, secret)
	require.LessOrEqual(t, views.Open(), 2, "the base handle and the leased one")
}

// console-open --unlock's core: the project must be locked, --for is the whole
// console lease when absent and refused when present but not a duration up to
// it, and the link it prints claims once into a browser's unlock.
func TestConsoleOpenUnlock_Link(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.Defaults()
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", Locked: []string{"example-finance"}}
	now := time.Now()

	_, err = consoleUnlockLink(ctx, cfg, db, 3*time.Hour, "example-ops", "", now)
	require.ErrorContains(t, err, "not locked")
	for _, bad := range []string{"soon", "0s", "30s", "4h"} {
		_, err = consoleUnlockLink(ctx, cfg, db, 3*time.Hour, "example-finance", bad, now)
		require.ErrorContains(t, err, "invalid --for", bad)
	}

	link, err := consoleUnlockLink(ctx, cfg, db, 3*time.Hour, "example-finance", "", now)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(link, consoleBase(cfg)+"/console/unlock?t="), link)
	u, err := url.Parse(link)
	require.NoError(t, err)
	secret := u.Query().Get("t")
	require.NotEmpty(t, secret)
	browser := store.HashConsoleToken("browser")
	got, err := store.ClaimConsoleUnlockLink(ctx, db, secret, browser, now)
	require.NoError(t, err)
	require.Equal(t, 3*time.Hour, got.Lease, "absent --for is the whole console lease")
	require.Equal(t, store.ConsoleUnlockByDaemon, got.Credential)
	_, err = store.ClaimConsoleUnlockLink(ctx, db, secret, browser, now)
	require.ErrorIs(t, err, store.ErrConsoleUnlockNotFound, "the link works once")

	link, err = consoleUnlockLink(ctx, cfg, db, 3*time.Hour, "example-finance", "90m", now)
	require.NoError(t, err)
	u, err = url.Parse(link)
	require.NoError(t, err)
	got, err = store.ClaimConsoleUnlockLink(ctx, db, u.Query().Get("t"), browser, now)
	require.NoError(t, err)
	require.Equal(t, 90*time.Minute, got.Lease)

	// It runs only as the daemon's account, with a project locked.
	off := config.Defaults()
	require.ErrorContains(t, runConsoleUnlock(off, "example-finance", "", ""), "locks no project")
}
