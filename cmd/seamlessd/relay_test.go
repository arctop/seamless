//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/relay"
	"github.com/arctop/seamless/internal/store"
)

// relayCmdLayout is a relay layout in a short temp dir (macOS caps a socket
// path at 104 bytes): a 0755 run dir for the socket, the data dir not yet
// made, and a binary of its own.
func relayCmdLayout(t *testing.T) (relay.Layout, relay.Env) {
	t.Helper()
	base, err := os.MkdirTemp("", "rc")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base, err = filepath.EvalSymlinks(base)
	require.NoError(t, err)
	l := relay.Layout{DataDir: filepath.Join(base, "relay"), Socket: filepath.Join(base, "run", "s.sock")}
	exe := filepath.Join(base, "bin", "seamlessd")
	for _, d := range []string{filepath.Dir(l.Socket), filepath.Dir(exe)} {
		require.NoError(t, os.Mkdir(d, 0o755))
		require.NoError(t, os.Chmod(d, 0o755)) // past the umask
	}
	require.NoError(t, os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.Chmod(exe, 0o755))
	// The test's own uid plays the relay's account: no pin, and taken for a
	// system account, as the server's preflight tests take it.
	env := relay.HostEnv()
	env.Exe = func() (string, error) { return exe, nil }
	env.DaemonAccount = func() (access.DaemonPin, bool, error) { return access.DaemonPin{}, false, nil }
	env.SystemAccount = func(uint32) (bool, string) { return true, "the test's stand-in" }
	return l, env
}

// relayCmdServerPEM is the certificate the server at base presents, as PEM:
// what the owner copies to a client as its CA file.
func relayCmdServerPEM(t *testing.T, base string, pool *x509.CertPool) []byte {
	t.Helper()
	conn, err := tls.Dial("tcp", strings.TrimPrefix(base, "https://"), &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: conn.ConnectionState().PeerCertificates[0].Raw})
}

// relayCmdOtherUID is the account a test's relay refuses as a caller: not
// the test's own, since here the relay and its callers run as one uid, where
// a real relay runs as its own account.
const relayCmdOtherUID = ^uint32(0) - 1

// relayCmdStart runs `seamlessd relay serve` (relayServe) in the background
// until the test ends, and returns a client of its socket built as seam
// builds one, once the socket is bound. The relay refuses relayCmdOtherUID as
// its own account.
func relayCmdStart(t *testing.T, l relay.Layout, env relay.Env) *http.Client {
	t.Helper()
	other := relayCmdOtherUID
	return relayCmdStartAs(t, l, env, &other)
}

// relayCmdStartAs is relayCmdStart with the account the relay refuses as a
// caller: self, or nil for the one it runs as (env.EUID), as serve does.
func relayCmdStartAs(t *testing.T, l relay.Layout, env relay.Env, self *uint32) *http.Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- relayServe(ctx, relayServeOptions{layout: l, env: env, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			ready: func(a net.Addr) { ready <- a }, self: self})
	}()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("the relay did not start: %v", err)
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("the relay never bound its socket")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err, "a stop is a clean return")
		case <-time.After(20 * time.Second):
			t.Error("the relay did not stop")
		}
	})
	cfg := config.Defaults()
	cfg.AdvertisedURL = "unix://" + l.Socket
	client, err := cfg.HTTPClient(20 * time.Second)
	require.NoError(t, err)
	client.CheckRedirect = config.RefuseRedirect
	return client
}

// relayCmdPost sends one request to the socket and returns the status and
// body.
func relayCmdPost(t *testing.T, client *http.Client, path, body string, header http.Header) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, config.SocketBaseURL+path, strings.NewReader(body))
	require.NoError(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(out)
}

// `seamlessd relay` names its two commands and settles its arguments before
// it touches anything.
func TestRelayCmd_SettlesItsArguments(t *testing.T) {
	require.ErrorContains(t, runRelay(nil), "name a command")
	require.ErrorContains(t, runRelay([]string{"bogus"}), `unknown command "bogus": valid commands are serve, pair`)
	require.ErrorContains(t, runRelayServe([]string{"extra"}), `unexpected argument "extra"`)
	require.Error(t, runRelayServe([]string{"--bogus"}))
	require.ErrorContains(t, runRelayPair(nil), "--server and --code are required")
	require.ErrorContains(t, runRelayPair([]string{"--server", "https://example-server:8081"}), "--server and --code are required")
	require.ErrorContains(t, runRelayPair([]string{"--server", "https://example-server:8081", "--code", "AAAA", "extra"}), `unexpected argument "extra"`)
	require.ErrorContains(t, runRelayPair([]string{"--server", "https://example-server:8081", "--code", "AAAA", "--ca-file", filepath.Join(t.TempDir(), "none.pem")}),
		"--ca-file")
	require.Contains(t, relayUsageText(), relay.DefaultSocketPath)
	require.Contains(t, relayUsageText(), relay.DefaultDataDir)
}

// The relay refuses to start where the boundary would not hold, and before
// it is paired: it never serves a socket it cannot vouch for.
func TestRelayCmd_ServeRefusesAnUnsafeLayoutAndAnUnpairedRelay(t *testing.T) {
	l, env := relayCmdLayout(t)
	err := relayServe(context.Background(), relayServeOptions{layout: l, env: env})
	require.ErrorIs(t, err, relay.ErrNotPaired, "the preflight passed and made the data dir; nothing to forward to")
	info, serr := os.Stat(l.DataDir)
	require.NoError(t, serr)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	_, serr = os.Lstat(l.Socket)
	require.ErrorIs(t, serr, os.ErrNotExist, "an unpaired relay binds no socket")

	require.NoError(t, os.Chmod(l.DataDir, 0o755))
	err = relayServe(context.Background(), relayServeOptions{layout: l, env: env})
	require.ErrorIs(t, err, errRelayPreflight)
	require.ErrorContains(t, err, "must be 0700")

	notSystem := env
	notSystem.SystemAccount = func(uint32) (bool, string) { return false, "system accounts are below uid 500" }
	require.NoError(t, os.Chmod(l.DataDir, 0o700))
	err = relayServe(context.Background(), relayServeOptions{layout: l, env: notSystem})
	require.ErrorIs(t, err, errRelayPreflight)
	require.ErrorContains(t, err, "not a system account")
	err = relayPair(context.Background(), relayPairOptions{dataDir: l.DataDir, env: notSystem,
		pair: relay.PairOptions{Server: "https://127.0.0.1:1", Code: "AAAA-BBBB-CCCC-DDDD"}}, io.Discard, io.Discard)
	require.ErrorIs(t, err, errRelayPreflight, "a key never lands where the account rule fails")
}

// `relay pair` then `relay serve`, against the daemon's real TCP stack with
// pairing on: the relay pairs with a code, the server's edge then takes its
// forwarded hook as the paired device's -- speaking for the enrolled name, with
// no key on this computer -- and a stop is a clean return.
func TestRelayCmd_PairThenServe(t *testing.T) {
	d := deviceNewDaemon(t, false)
	l, env := relayCmdLayout(t)
	code, err := d.pairing.MintCode(context.Background(), store.DeviceByConsole)
	require.NoError(t, err)
	var out, notes bytes.Buffer
	ca := relayCmdServerPEM(t, d.srv.URL, d.pool)
	require.NoError(t, relayPair(context.Background(), relayPairOptions{dataDir: l.DataDir, env: env, json: true,
		pair: relay.PairOptions{Server: d.srv.URL, Code: code.Code, Name: "example-laptop", CA: ca}}, &out, &notes))
	var paired relayPairJSON
	require.NoError(t, json.Unmarshal(out.Bytes(), &paired))
	require.Equal(t, "example-laptop", paired.Name)
	require.NotContains(t, out.String(), "PRIVATE KEY", "--json prints the public facts alone")
	dev, err := store.DeviceByID(context.Background(), d.db, paired.DeviceID)
	require.NoError(t, err)
	require.Equal(t, dev.Fingerprint, paired.Fingerprint)

	client := relayCmdStart(t, l, env)
	status, body := relayCmdPost(t, client, "/api/hooks/session-start?client=claude-code&host=example-ci",
		`{"session_id":"relay-cmd-hook","cwd":"/tmp/relay-cmd","source":"startup"}`,
		http.Header{"Content-Type": {"application/json"}, "Authorization": {"Bearer " + deviceSharedKey + "-wrong"}})
	require.Equal(t, http.StatusOK, status, body)
	var host string
	require.NoError(t, d.db.QueryRow(`SELECT host FROM sessions WHERE claude_session_id = 'relay-cmd-hook'`).Scan(&host))
	require.Equal(t, "example-laptop", host, "the hook speaks for the relay's enrolled name, whatever it said")

	// Paired already: a second pairing is refused unless asked to replace.
	err = relayPair(context.Background(), relayPairOptions{dataDir: l.DataDir, env: env,
		pair: relay.PairOptions{Server: d.srv.URL, Code: code.Code, Name: "example-laptop", CA: ca}}, io.Discard, io.Discard)
	require.ErrorIs(t, err, relay.ErrPaired)

	out.Reset()
	_, err = d.pairing.Revoke(context.Background(), paired.DeviceID, store.DeviceByConsole)
	require.NoError(t, err)
	code, err = d.pairing.MintCode(context.Background(), store.DeviceByConsole)
	require.NoError(t, err)
	require.NoError(t, relayPair(context.Background(), relayPairOptions{dataDir: l.DataDir, env: env,
		pair: relay.PairOptions{Server: d.srv.URL, Code: code.Code, Name: "example-laptop", CA: ca, Replace: true}}, &out, &notes))
	for _, want := range []string{"Paired: this relay speaks for example-laptop", "The server's certificate: SHA-256 ",
		"This relay's key: ", "restart it"} {
		require.Contains(t, out.String(), want)
	}
}

// `relay pair` checks the binary it runs as serve does: a pairing writes the
// relay's key, so a binary another account could have replaced -- one a PATH
// lookup under sudo -u found in a person's home -- pairs nothing.
func TestRelayCmd_PairRefusesABinaryOthersCanReplace(t *testing.T) {
	l, env := relayCmdLayout(t)
	exe, err := env.Exe()
	require.NoError(t, err)
	require.Empty(t, relayBinaryProblem(env), "the layout's own binary passes")

	open := filepath.Join(filepath.Dir(filepath.Dir(exe)), "open")
	require.NoError(t, os.Mkdir(open, 0o755))
	require.NoError(t, os.Chmod(open, 0o777))
	other := filepath.Join(open, "seamlessd")
	require.NoError(t, os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755))
	env.Exe = func() (string, error) { return other, nil }
	err = relayPair(context.Background(), relayPairOptions{dataDir: l.DataDir, env: env,
		pair: relay.PairOptions{Server: "https://127.0.0.1:1", Code: "AAAA-BBBB-CCCC-DDDD"}}, io.Discard, io.Discard)
	require.ErrorIs(t, err, errRelayPreflight)
	require.ErrorContains(t, err, "writable by other accounts -- pair with "+relay.DefaultBinary)
	_, serr := os.Stat(filepath.Join(l.DataDir, relay.StateName))
	require.ErrorIs(t, serr, os.ErrNotExist, "nothing was paired")

	require.NoError(t, os.Chmod(open, 0o755))
	require.NoError(t, os.Chmod(other, 0o777))
	require.Contains(t, relayBinaryProblem(env), "is mode 0777")
}

// `relay serve` refuses a caller that runs as its own account -- the pinned
// one, which holds the relay's key -- before anything reaches the server: the
// relay vouches for other accounts' processes, never its own. Here the test's
// uid plays the relay's account and its caller both.
func TestRelayCmd_ServeRefusesItsOwnAccount(t *testing.T) {
	d := deviceNewDaemon(t, false)
	l, env := relayCmdLayout(t)
	code, err := d.pairing.MintCode(context.Background(), store.DeviceByConsole)
	require.NoError(t, err)
	require.NoError(t, relayPair(context.Background(), relayPairOptions{dataDir: l.DataDir, env: env,
		pair: relay.PairOptions{Server: d.srv.URL, Code: code.Code, Name: "example-laptop", CA: relayCmdServerPEM(t, d.srv.URL, d.pool)}},
		io.Discard, io.Discard))

	client := relayCmdStartAs(t, l, env, nil)
	status, body := relayCmdPost(t, client, "/api/hooks/session-start?client=claude-code&host=example-ci",
		`{"session_id":"relay-cmd-self","cwd":"/tmp/relay-cmd","source":"startup"}`, http.Header{"Content-Type": {"application/json"}})
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, "the caller runs as the relay's own account")
	var n int
	require.NoError(t, d.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE claude_session_id = 'relay-cmd-self'`).Scan(&n))
	require.Zero(t, n, "nothing reached the server")
}
