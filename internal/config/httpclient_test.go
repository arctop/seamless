package config

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// writeCAFile writes a certificate as a PEM file and returns its path.
func writeCAFile(t *testing.T, der []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	return path
}

func TestHTTPClient_NoCAFileUsesTheSystemPool(t *testing.T) {
	c, err := Config{}.HTTPClient(3 * time.Second)
	require.NoError(t, err)
	require.Equal(t, 3*time.Second, c.Timeout)
	tr, ok := c.Transport.(*http.Transport)
	require.True(t, ok)
	require.Nil(t, tr.TLSClientConfig, "no extra root configured: leave Go's default trust alone")
	require.NotNil(t, tr.DialContext, "a down daemon must fail fast on the dial, not on the deadline")

	// 0 means no whole-request deadline (mcp-proxy, dial).
	c, err = Config{}.HTTPClient(0)
	require.NoError(t, err)
	require.Equal(t, time.Duration(0), c.Timeout)
}

// A configured-but-unusable ca_file is a LOCAL config error: surfacing it is
// what keeps it distinguishable from the server being down, which is exactly
// what it would look like if we fell back to the system pool.
func TestHTTPClient_BadCAFileIsAnError(t *testing.T) {
	_, err := Config{TLS: TLS{CAFile: "/nonexistent/ca.pem"}}.HTTPClient(time.Second)
	require.Error(t, err)
	require.ErrorContains(t, err, "tls.ca_file /nonexistent/ca.pem")

	junk := filepath.Join(t.TempDir(), "junk.pem")
	require.NoError(t, os.WriteFile(junk, []byte("not a certificate\n"), 0o600))
	_, err = Config{TLS: TLS{CAFile: junk}}.HTTPClient(time.Second)
	require.Error(t, err)
	require.ErrorContains(t, err, "no PEM certificate found")
}

// End to end: an https server whose certificate chains to nothing the system
// trusts is reachable through tls.ca_file and unreachable without it. This is
// the whole point of the key -- a mkcert/self-signed LAN daemon.
func TestHTTPClient_CAFileTrustsAPrivateServer(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := Config{TLS: TLS{CAFile: writeCAFile(t, srv.Certificate().Raw)}}
	c, err := cfg.HTTPClient(5 * time.Second)
	require.NoError(t, err)
	tr, ok := c.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, tr.TLSClientConfig)
	require.Equal(t, uint16(tls.VersionTLS12), tr.TLSClientConfig.MinVersion)
	require.False(t, tr.TLSClientConfig.InsecureSkipVerify, "trust is added, never switched off")

	resp, err := c.Get(srv.URL + "/healthz")
	require.NoError(t, err, "the configured root must verify this server")
	require.NoError(t, resp.Body.Close())

	plain, err := Config{}.HTTPClient(5 * time.Second)
	require.NoError(t, err)
	_, err = plain.Get(srv.URL + "/healthz")
	require.Error(t, err, "without the root it is an untrusted certificate, not a reachable server")
	var unknown x509.UnknownAuthorityError
	require.ErrorAs(t, err, &unknown)
}

// RefuseRedirect stops a redirect a default client follows: the call fails
// naming the target, and the target is never sent the request.
func TestRefuseRedirect(t *testing.T) {
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
	}))
	defer target.Close()
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer daemon.Close()

	resp, err := (&http.Client{}).Post(daemon.URL+"/api/mcp", "application/json", nil)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, 1, hits, "the fixture's redirect must be one a default client follows")

	_, err = (&http.Client{CheckRedirect: RefuseRedirect}).Post(daemon.URL+"/api/mcp", "application/json", nil)
	require.ErrorContains(t, err, "refusing a redirect to "+target.URL+"/elsewhere")
	require.Equal(t, 1, hits)
}
