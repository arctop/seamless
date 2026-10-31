//go:build darwin || linux

package config

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHTTPClient_DialsTheSocket proves a socket server_url reaches the daemon
// through the one shared client: the request URL's host is never dialed, and
// an HTTP proxy in the environment does not move the dial off the socket.
func TestHTTPClient_DialsTheSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "hc")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "socket:"+r.URL.Path)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	cfg := Config{AdvertisedURL: "unix://" + sock, Role: RoleClient}
	client, err := cfg.HTTPClient(0)
	require.NoError(t, err)
	resp, err := client.Get(cfg.ServerURL() + "/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "socket:/healthz", string(body))
}
