//go:build darwin || linux

package main

// A Client's setup end to end: docs/install run for real, its every setup call
// served by the real setup (TestSetupHelperSeamlessd), against a real serve
// with TLS on a loopback port (relayE2EServe) -- the server whose setup printed
// the address, the fingerprint and the code. Nothing runs as root: the relay's
// root step is printed, and its pairing and service are played here by the
// relay command in a scratch layout, as its account would run them.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/relay"
	"github.com/arctop/seamless/internal/store"
)

// clientE2EServer is a real serve and what its setup printed for a Client:
// the address, the certificate fingerprint and a fresh pairing code.
func clientE2EServer(t *testing.T) (*relayE2EServer, string, string) {
	t.Helper()
	srv := relayE2EServe(t)
	fp, err := certFingerprintFile(filepath.Join(filepath.Dir(filepath.Dir(srv.dbPath)), "cert.pem"))
	require.NoError(t, err)
	return srv, fp, srv.relayCode(t)
}

// The Windows path, on a computer where the owner has no root: the one-liner
// with SEAMLESS_PAIR_AS=key pairs a key device of its own before anything is
// installed -- its key, never the server's, into a 0600 config the hooks then
// read -- and the closing check finds the server recording this computer
// under the name it paired as.
func TestSetupClientE2E_KeyDevicePairsAndTheServerSeesItsName(t *testing.T) {
	srv, fp, code := clientE2EServer(t)
	w := newSetupInstallerWorld(t, "/bin/sh", filepath.Join("..", "..", "docs", "install"))
	out, err := w.run(t, "SETUP_E2E_SETUP=real", "SEAMLESS_VERSION="+setupE2EVersion, "SEAMLESS_CLIENT=claude", "SEAMLESS_NO_SERVICE=1",
		"SETUP_E2E_HOST=example-laptop",
		"SEAMLESS_SETUP=client", "SEAMLESS_SERVER_URL="+srv.base, "SEAMLESS_PAIR_CODE="+code, "SEAMLESS_PAIR_AS=key",
		"SEAMLESS_SERVER_FINGERPRINT="+fp)
	require.NoError(t, err, out)

	cfgPath := filepath.Join(w.home, ".config", "seamless", "seamless.yaml")
	cfg, err := config.LoadFrom(cfgPath)
	require.NoError(t, err, out)
	require.True(t, cfg.IsClient())
	require.Equal(t, srv.base, cfg.ServerURL())
	require.True(t, strings.HasPrefix(cfg.MCP.APIKey, "seamless_dk_"), "a key of this computer's own")
	require.NotEqual(t, relayE2EKey, cfg.MCP.APIKey, "never the server's key")
	require.Equal(t, filepath.Join(w.home, ".config", "seamless", "server-ca.crt"), cfg.TLS.CAFile, "the compared certificate, trusted alone")
	info, err := os.Stat(cfgPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	require.Contains(t, w.calls(t), w.hooksCall("claude"), "the hooks read the config: no key rides in argv")
	require.Contains(t, out, "fingerprint  "+fp)
	require.Contains(t, out, "matches SEAMLESS_SERVER_FINGERPRINT")
	require.Contains(t, out, "a key device never opens a locked project")
	require.Contains(t, out, "skipped (client of "+srv.base+")")
	require.Contains(t, out, "the Server recorded the session seamless-setup-check under example-laptop")
	require.NotContains(t, out, cfg.MCP.APIKey, "the key is written, never printed")
	require.Less(t, strings.Index(out, "paired "), strings.Index(out, "installed "), "paired before anything is installed")

	db, err := store.OpenExisting(srv.dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var kind, status string
	require.NoError(t, db.QueryRow(`SELECT kind, status FROM devices WHERE name = 'example-laptop'`).Scan(&kind, &status))
	require.Equal(t, "key", kind)
	require.Equal(t, store.DeviceActive, status)
}

// The macOS and Linux path: the one-liner points the owner's config at the
// relay's socket with no key, and prints the relay's one root step -- whose
// pairing, run here as the relay command in a scratch layout with the printed
// server, fingerprint and certificate, pairs the relay; served, it carries
// the closing check, and the server records this computer under the relay's
// name.
func TestSetupClientE2E_RelayRootStepPairsAndServes(t *testing.T) {
	srv, fp, code := clientE2EServer(t)
	w := newSetupInstallerWorld(t, "/bin/sh", filepath.Join("..", "..", "docs", "install"))
	out, err := w.run(t, "SETUP_E2E_SETUP=real", "SEAMLESS_VERSION="+setupE2EVersion, "SEAMLESS_CLIENT=claude", "SEAMLESS_NO_SERVICE=1",
		"SETUP_E2E_HOST=example-laptop",
		"SEAMLESS_SETUP=client", "SEAMLESS_SERVER_URL="+srv.base, "SEAMLESS_PAIR_CODE="+code, "SEAMLESS_SERVER_FINGERPRINT="+fp)
	require.NoError(t, err, out)

	cfg, err := config.LoadFrom(filepath.Join(w.home, ".config", "seamless", "seamless.yaml"))
	require.NoError(t, err, out)
	require.Equal(t, relay.DefaultSocketPath, cfg.SocketPath(), "the hooks and MCP dial the relay's socket")
	require.Empty(t, cfg.MCP.APIKey, "a relay's Client holds no key")
	require.Contains(t, out, "skipped (client of "+config.RelaySocketURL+")")
	pair := "\n/usr/bin/sudo -u _seamless -H /opt/seamless/bin/seamlessd relay pair --server " + srv.base + " --fingerprint " + fp +
		" --ca-file " + clientRelayCAFile + " --code -\n"
	require.Contains(t, out, pair)
	require.Contains(t, out, "code         the one you set in SEAMLESS_PAIR_CODE")
	require.NotContains(t, out, code, "the code from the environment is not printed back")
	require.Contains(t, out, "once the relay serves: seam doctor")
	require.Contains(t, out, "peer_auth.relays: {example-laptop: {owner_uid: ")
	require.Less(t, strings.Index(out, "installed "), strings.Index(out, pair), "the root step closes the install")

	// The certificate root writes is the one the step prints, and the one the
	// owner compared: what setup kept, and no file of the owner's is read.
	kept, err := os.ReadFile(filepath.Join(w.home, ".config", "seamless", "server-ca.crt"))
	require.NoError(t, err)
	open := "\n/usr/bin/sudo /usr/bin/tee " + clientRelayCAFile + " >/dev/null <<'EOF' &&\n"
	require.Contains(t, out, open)
	ca, _, ok := strings.Cut(out[strings.Index(out, open)+len(open):], "EOF\n")
	require.True(t, ok, "the certificate's here-document ends")
	require.Equal(t, string(kept), ca)

	// The root step's pairing and service, as the relay's account runs them.
	l, env := relayCmdLayout(t)
	require.NoError(t, relayPair(context.Background(), relayPairOptions{dataDir: l.DataDir, env: env,
		pair: relay.PairOptions{Server: srv.base, Code: code, Name: "example-laptop", ServerFingerprint: fp, CA: []byte(ca)}},
		&strings.Builder{}, &strings.Builder{}))
	relayCmdStart(t, l, env)

	local := config.Defaults()
	local.Role, local.AdvertisedURL = config.RoleClient, "unix://"+l.Socket
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	host, err := clientSessionHost(ctx, local, "seamless-setup-check")
	require.NoError(t, err)
	require.Equal(t, "example-laptop", host, "the server sees this computer by the relay's enrolled name")
}
