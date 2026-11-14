//go:build darwin || linux

package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
)

// relayCert is a self-signed server certificate for 127.0.0.1, usable as its
// own root (CA:TRUE), as the network-install guide's openssl one is.
type relayCert struct {
	tls tls.Certificate
	pem []byte
	fp  string
}

func relayNewCert(t *testing.T, cn string) relayCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return relayCert{
		tls: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf},
		pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		fp:  devices.CertFingerprint(der),
	}
}

// relayUpstream is h on a TLS listener that asks a relay for its certificate
// (devices.WithRelayClientAuth), as a paired server's does.
func relayUpstream(t *testing.T, cert relayCert, h http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(h)
	ts.TLS = devices.WithRelayClientAuth(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert.tls}})
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

// relayLayoutAt is a relay layout in a short temp dir (macOS caps a socket
// path at 104 bytes): a 0700 data dir and a 0755 run dir for the socket.
func relayLayoutAt(t *testing.T) Layout {
	t.Helper()
	base, err := os.MkdirTemp("", "rl")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base, err = filepath.EvalSymlinks(base)
	require.NoError(t, err)
	l := Layout{DataDir: filepath.Join(base, "relay"), Socket: filepath.Join(base, "run", "s.sock")}
	require.NoError(t, os.Mkdir(l.DataDir, 0o700))
	require.NoError(t, os.Mkdir(filepath.Dir(l.Socket), 0o755))
	require.NoError(t, os.Chmod(filepath.Dir(l.Socket), 0o755)) // past the umask
	return l
}

// relayTestEnv plays the relay's account as the test's own uid, on a machine
// with no pin, taking that uid for a system account (as the server's
// preflight tests do), with a binary of its own in the layout.
func relayTestEnv(t *testing.T, l Layout) Env {
	t.Helper()
	exe := filepath.Join(filepath.Dir(filepath.Dir(l.Socket)), "bin", "seamlessd")
	require.NoError(t, os.MkdirAll(filepath.Dir(exe), 0o755))
	require.NoError(t, os.Chmod(filepath.Dir(exe), 0o755))
	require.NoError(t, os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.Chmod(exe, 0o755))
	return Env{
		EUID:          os.Geteuid(),
		Exe:           func() (string, error) { return exe, nil },
		Inspect:       inspectPath,
		DaemonAccount: relayNoPin,
		SystemAccount: func(uint32) (bool, string) { return true, "the test's stand-in" },
	}
}

// relayNoPin is a machine with no pin of the daemon's account.
func relayNoPin() (access.DaemonPin, bool, error) { return access.DaemonPin{}, false, nil }

// relayWriteState saves a pairing with the server at serverURL in l's data
// dir, pinning the certificate fingerprint pin, with ca as the CA copy, and
// returns it.
func relayWriteState(t *testing.T, l Layout, serverURL, pin string, ca []byte) State {
	t.Helper()
	id, err := devices.NewRelayIdentity("example-laptop")
	require.NoError(t, err)
	certPEM, keyPEM, err := id.MarshalPEM()
	require.NoError(t, err)
	caName, err := writeCA(l.DataDir, ca)
	require.NoError(t, err)
	st := State{Version: stateVersion, ServerURL: serverURL, ServerFingerprint: pin, CAFile: caName, DeviceID: "01EXAMPLEDEVICE",
		Name: "example-laptop", Fingerprint: id.Fingerprint(), PairedAt: time.Now().UTC(), Certificate: string(certPEM), Key: string(keyPEM)}
	require.NoError(t, SaveState(l.DataDir, st))
	return st
}

// relayLogger discards the relay's log.
func relayLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// relayRun serves the relay of l (paired as st) on its socket until the test
// ends, and returns a client of the socket built as seam builds one for a
// socket server_url (config.Config.HTTPClient).
func relayRun(t *testing.T, l Layout, st State) *http.Client {
	t.Helper()
	env := relayTestEnv(t, l)
	require.Empty(t, env.Problems(l, false))
	fwd, err := st.Forwarder(l.DataDir, relayLogger())
	require.NoError(t, err)
	ln, err := env.Listen(l.Socket)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, fwd, relayLogger()) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Error("the relay did not stop")
		}
	})
	return relaySocketClient(t, l.Socket)
}

// relaySocketClient is a client of the socket at path, as seam's.
func relaySocketClient(t *testing.T, path string) *http.Client {
	t.Helper()
	cfg := config.Defaults()
	cfg.AdvertisedURL = "unix://" + path
	c, err := cfg.HTTPClient(20 * time.Second)
	require.NoError(t, err)
	c.CheckRedirect = config.RefuseRedirect
	return c
}

// relayDo sends one request through client and returns the status, the
// header and the body.
func relayDo(t *testing.T, client *http.Client, method, path string, header http.Header, body string) (int, http.Header, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, config.SocketBaseURL+path, rd)
	require.NoError(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, string(out)
}

// relayErr is the error text of a JSON refusal.
func relayErr(t *testing.T, body string) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &e), body)
	return e.Error
}

// relayRequireNoUpstream fails when the channel got a request.
func relayRequireNoUpstream[T any](t *testing.T, seen <-chan T) {
	t.Helper()
	select {
	case got := <-seen:
		t.Fatalf("the server got a request it must never have seen: %v", got)
	default:
	}
}
