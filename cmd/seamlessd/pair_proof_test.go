//go:build darwin || linux

package main

// Black-box proofs of device pairing against a real serve: plain HTTP and the
// standard library alone, so the same file runs against a build without
// pairing and shows what it lacks.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const pairProofKey = "pair-proof-shared-key-0123456789"

// pairProofServe runs serve in this process on a port free on both loopback
// addresses (frelayDualLoopbackPort) with the config yaml (extra lines
// appended), and returns its base URL and a client that trusts its
// certificate. It stops when the test ends.
func pairProofServe(t *testing.T, withTLS bool) (string, *x509.CertPool) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := t.TempDir()
	port, release := frelayDualLoopbackPort(t)
	yaml := fmt.Sprintf("addr: '127.0.0.1:%d'\ndata_dir: '%s'\nmcp:\n  api_key: '%s'\nupdate:\n  check: false\ngardener:\n  enabled: false\n",
		port, filepath.Join(root, "data"), pairProofKey)
	scheme := "http"
	var pool *x509.CertPool
	if withTLS {
		scheme = "https"
		certPath, keyPath := filepath.Join(root, "cert.pem"), filepath.Join(root, "key.pem")
		pool = pairProofCert(t, certPath, keyPath)
		yaml += fmt.Sprintf("tls:\n  cert_file: '%s'\n  key_file: '%s'\n", certPath, keyPath)
	}
	cfgPath := filepath.Join(root, "seamless.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(yaml), 0o600))
	t.Setenv("SEAMLESS_CONFIG", cfgPath)
	done := make(chan error, 1)
	release() // serve binds both halves itself
	go func() { done <- runServe([]string{"--config", cfgPath}) }()
	base := fmt.Sprintf("%s://127.0.0.1:%d", scheme, port)
	client := pairProofClient(pool, nil)
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case err := <-done:
			t.Fatalf("serve stopped before it answered: %v", err)
		default:
		}
		require.True(t, time.Now().Before(deadline), "serve never answered /healthz")
		<-time.After(50 * time.Millisecond)
	}
	t.Cleanup(func() {
		require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(20 * time.Second):
			t.Error("serve did not stop")
		}
	})
	return base, pool
}

// pairProofCert writes a self-signed certificate for 127.0.0.1 and its key,
// and returns a pool that trusts it.
func pairProofCert(t *testing.T, certPath, keyPath string) *x509.CertPool {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(9), Subject: pkix.Name{CommonName: "example-server"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return pool
}

func pairProofClient(pool *x509.CertPool, tlsCfg *tls.Config) *http.Client {
	if tlsCfg == nil {
		tlsCfg = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}, Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// pairProofDo sends one request and returns its status and body.
func pairProofDo(t *testing.T, client *http.Client, method, target, auth, contentType, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, target, strings.NewReader(body))
	require.NoError(t, err)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, out
}

const pairProofInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"pair-proof","version":"0"}}}`

// On a networked server (TLS here) a computer pairs with a one-time code the
// owner mints on the console, gets a key of its own that MCP accepts as the
// owner, cannot reuse the code, and is refused at once when revoked.
func TestPAIRProof_NetworkedServerPairsADevice(t *testing.T) {
	base, pool := pairProofServe(t, true)
	client := pairProofClient(pool, nil)

	status, body := pairProofDo(t, client, http.MethodPost, base+"/console/devices/code", pairProofKey, "application/x-www-form-urlencoded", "")
	require.Equal(t, http.StatusOK, status, "the owner mints a pairing code on the console: %s", body)
	var minted struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(body, &minted))
	require.NotEmpty(t, minted.Code)

	secretRaw := make([]byte, 32)
	_, err := rand.Read(secretRaw)
	require.NoError(t, err)
	secret := base64.RawURLEncoding.EncodeToString(secretRaw)
	pair := fmt.Sprintf(`{"code":%q,"kind":"key","name":"example-ci","secret":%q}`, minted.Code, secret)
	status, body = pairProofDo(t, client, http.MethodPost, base+"/api/pair", "", "application/json", pair)
	require.Equal(t, http.StatusOK, status, "%s", body)
	var paired struct {
		Status string `json:"status"`
		Key    string `json:"key"`
		Device struct {
			ID string `json:"id"`
		} `json:"device"`
	}
	require.NoError(t, json.Unmarshal(body, &paired))
	require.Equal(t, "paired", paired.Status, "without peer_auth the code is the approval")
	require.NotEmpty(t, paired.Key)
	require.NotEqual(t, pairProofKey, paired.Key, "a key of its own, never the server's")

	status, _ = pairProofDo(t, client, http.MethodPost, base+"/api/mcp", paired.Key, "application/json", pairProofInitialize)
	require.Equal(t, http.StatusOK, status, "the device key is the owner where the bearer key is")

	again := fmt.Sprintf(`{"code":%q,"kind":"key","name":"example-other","secret":%q}`, minted.Code, base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	status, _ = pairProofDo(t, client, http.MethodPost, base+"/api/pair", "", "application/json", again)
	require.Equal(t, http.StatusForbidden, status, "a used code fails")

	status, _ = pairProofDo(t, client, http.MethodPost, base+"/console/devices/"+paired.Device.ID+"/revoke", pairProofKey,
		"application/x-www-form-urlencoded", "")
	require.Equal(t, http.StatusOK, status)
	status, _ = pairProofDo(t, client, http.MethodPost, base+"/api/mcp", paired.Key, "application/json", pairProofInitialize)
	require.Equal(t, http.StatusUnauthorized, status, "a revoked device key is refused")
	status, _ = pairProofDo(t, client, http.MethodPost, base+"/api/mcp", pairProofKey, "application/json", pairProofInitialize)
	require.Equal(t, http.StatusOK, status, "the shared key, as before")
}

// On a networked server with TLS a relay -- a client offering the relay
// protocol -- is asked for its certificate; a browser-like client never is.
func TestPAIRProof_OnlyARelayIsAskedForACertificate(t *testing.T) {
	base, pool := pairProofServe(t, true)
	for _, tc := range []struct {
		protos []string
		asked  bool
	}{{[]string{"h2", "http/1.1"}, false}, {[]string{"http/1.1", "seamless-relay/1"}, true}} {
		asked := false
		cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, NextProtos: tc.protos,
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				asked = true
				return &tls.Certificate{}, nil
			}}
		conn, err := tls.Dial("tcp", strings.TrimPrefix(base, "https://"), cfg)
		require.NoError(t, err)
		require.NoError(t, conn.HandshakeContext(context.Background()))
		require.NoError(t, conn.Close())
		require.Equal(t, tc.asked, asked, "%v", tc.protos)
	}
}

// A default install -- loopback, no TLS, no server_url -- pairs nothing: no
// Devices page, no pairing endpoint, no certificate asked of anyone.
func TestPAIRProof_DefaultInstallPairsNothing(t *testing.T) {
	base, _ := pairProofServe(t, false)
	client := pairProofClient(nil, nil)
	status, _ := pairProofDo(t, client, http.MethodGet, base+"/console/devices", pairProofKey, "", "")
	require.Equal(t, http.StatusNotFound, status)
	status, _ = pairProofDo(t, client, http.MethodPost, base+"/console/devices/code", pairProofKey, "application/x-www-form-urlencoded", "")
	require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, status, "no code is minted")
	status, _ = pairProofDo(t, client, http.MethodPost, base+"/api/pair", "", "application/json", `{}`)
	require.Equal(t, http.StatusNotFound, status)
	status, _ = pairProofDo(t, client, http.MethodPost, base+"/api/mcp", pairProofKey, "application/json", pairProofInitialize)
	require.Equal(t, http.StatusOK, status)
}
