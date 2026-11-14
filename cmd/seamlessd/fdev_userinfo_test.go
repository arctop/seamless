package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// A server_url that names a user loads (install-hooks --server-url wrote
// such client configs), and doctor warns instead of probing it: Go's HTTP
// client would send the user and password as Basic auth to whatever answers,
// and the row would print them. The public agent card never carries it.
func TestFdevHub_ServerURLUserIsWarnedNotProbed(t *testing.T) {
	var hits atomic.Int32
	var basic atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			basic.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	u.User = url.UserPassword("example-user", "example-password")

	// The premise: a request with no Authorization header carries it.
	resp, err := reachabilityProbe().Get(u.String() + "/healthz")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.True(t, basic.Load(), "Go's client sends the URL's user as Basic auth")
	hits.Store(0)

	for _, role := range []string{config.RoleServer, config.RoleClient} {
		cfg := config.Defaults()
		cfg.Role, cfg.AdvertisedURL = role, u.String()
		require.NoError(t, cfg.Validate(), "it loads")
		var c check
		if role == config.RoleClient {
			c = clientServerURLCheckWith(cfg, relaySocketEnv{})
		} else {
			c = serverURLCheck(cfg)
			require.NotContains(t, a2aEndpoint(cfg), "example-password", "the public card never carries it")
			require.Equal(t, srv.URL+"/api/a2a", a2aEndpoint(cfg))
		}
		require.Equal(t, "server_url", c.name, role)
		require.Equal(t, statusWarn, c.status, role)
		require.Contains(t, c.detail, "names a user", role)
		require.NotContains(t, c.detail, "example-password", role)
		require.Zero(t, hits.Load(), "doctor probed nothing (%s)", role)
	}

	cfg := config.Defaults()
	cfg.AdvertisedURL = srv.URL
	require.Equal(t, statusOK, serverURLCheck(cfg).status, "a bare address is probed as before")
	require.Equal(t, srv.URL+"/api/a2a", a2aEndpoint(cfg))
}
