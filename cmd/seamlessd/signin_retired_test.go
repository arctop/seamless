package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// retireDaemonConfig is a peer_auth daemon's config with passkey sign-in on
// and the key retired.
func retireDaemonConfig() config.Config {
	cfg := config.Defaults()
	cfg.PeerAuth.Enabled = true
	cfg.PeerAuth.Socket.Path = "/opt/seamless/run/seamlessd.sock"
	cfg.Console.SignIn = config.ConsoleSignInPasskey
	cfg.MCP.APIKey = ""
	return cfg
}

// A retired key is meant: only a peer_auth daemon that signs browsers in with
// a passkey and holds no key says so; every other daemon answers a missing
// key as it always did.
func TestRetiredKey_OnlyThePasskeyPeerAuthDaemonWithNoKey(t *testing.T) {
	cfg := retireDaemonConfig()
	require.True(t, retiredKey(cfg))
	require.Equal(t, "unix:///opt/seamless/run/seamlessd.sock", retiredKeySocketURL(cfg))
	c, ok := retiredKeyCheck(cfg)
	require.True(t, ok)
	require.Equal(t, statusOK, c.status)
	require.Contains(t, c.detail, "retired")

	for name, mutate := range map[string]func(*config.Config){
		"a key":          func(c *config.Config) { c.MCP.APIKey = "k" },
		"no peer_auth":   func(c *config.Config) { c.PeerAuth.Enabled = false },
		"the key signs":  func(c *config.Config) { c.Console.SignIn = config.ConsoleSignInKey },
		"a client":       func(c *config.Config) { c.Role = config.RoleClient },
		"default config": func(c *config.Config) { *c = config.Defaults() },
	} {
		cfg := retireDaemonConfig()
		mutate(&cfg)
		require.False(t, retiredKey(cfg), name)
		require.Empty(t, retiredKeySocketURL(cfg), name)
		_, ok := retiredKeyCheck(cfg)
		require.False(t, ok, name)
	}
}

// MCP over TCP on a daemon whose key is retired is refused with a pointer to
// the socket and no Bearer challenge; with a key, a request without it gets
// the 401 it always did.
func TestRetiredKey_MCPOverTCPPointsToTheSocket(t *testing.T) {
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(context.Background(), db, "example-server"))
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	serve := func(cfg config.Config) *httptest.ResponseRecorder {
		srv := mcp.New(mcp.Config{DB: db, Files: mgr, Retrieve: retrieve.New(db, nil, config.Defaults().Budgets, nil),
			Events: events.NewRecorder(db), APIKey: cfg.MCP.APIKey, SocketURL: retiredKeySocketURL(cfg), Version: "test",
			LocalHost: "example-server", Logger: logger})
		req := httptest.NewRequest(http.MethodPost, "/api/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
		req.Header.Set("Authorization", "Bearer old-key")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		return rr
	}
	rr := serve(retireDaemonConfig())
	require.Equal(t, http.StatusForbidden, rr.Code)
	require.Contains(t, rr.Body.String(), "unix:///opt/seamless/run/seamlessd.sock")
	require.Empty(t, rr.Header().Get("WWW-Authenticate"))

	keyed := retireDaemonConfig()
	keyed.MCP.APIKey = "the-key"
	rr = serve(keyed)
	require.Equal(t, http.StatusUnauthorized, rr.Code, "a wrong key on a keyed daemon is the old answer")
	require.Equal(t, "Bearer", rr.Header().Get("WWW-Authenticate"))
}
