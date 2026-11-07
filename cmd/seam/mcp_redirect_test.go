package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// redirectingDaemon is a daemon whose every answer is a 307 to a second server
// on another port of the same host: the one redirect Go follows with the
// bearer key still attached. hits counts the requests that reach the second
// server.
func redirectingDaemon(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	hits := new(atomic.Int32)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(daemon.Close)
	return daemon.URL, hits
}

// seam's MCP client follows no redirect (GO-2026-6911): the dial fails and the
// redirect's target never sees the request or its key.
func TestDialWith_RefusesARedirect(t *testing.T) {
	daemonURL, hits := redirectingDaemon(t)
	cfg := config.Defaults()
	cfg.AdvertisedURL = daemonURL
	cfg.MCP.APIKey = "the-bearer-key"

	cli, _, err := dialWith(context.Background(), cfg)
	if cli != nil {
		_ = cli.Close()
	}
	require.Error(t, err)
	require.Zero(t, hits.Load(), "the redirect's target received a request")
}

// The mcp-proxy bridge sends the same headers, so it refuses the same way and
// answers the frame in-band.
func TestBridge_RefusesARedirect(t *testing.T) {
	daemonURL, hits := redirectingDaemon(t)
	var out bytes.Buffer
	b := newBridge(daemonURL+"/api/mcp", "the-bearer-key", nil)
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"
	require.NoError(t, b.run(context.Background(), strings.NewReader(in), &out))

	require.Zero(t, hits.Load(), "the redirect's target received a request")
	frames := nonEmptyLines(out.String())
	require.Len(t, frames, 1)
	require.Equal(t, float64(1), jsonRPCID(t, frames[0]))
	require.Contains(t, frames[0], "refusing a redirect")
}

// newBridge sets its policy on a copy: the caller's client is left as it was.
func TestNewBridge_LeavesTheCallersClientAlone(t *testing.T) {
	hc := &http.Client{}
	b := newBridge("http://127.0.0.1:1/api/mcp", "k", hc)
	require.Nil(t, hc.CheckRedirect)
	require.NotNil(t, b.client.CheckRedirect)
}
