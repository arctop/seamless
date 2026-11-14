package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// redactUser is the user and password every server_url below names: what
// `seamlessd install-hooks --server-url` can write into a config, and what
// no line printed, logged or shown may carry, nor any half of it.
const redactUser = "alice:s3cret@"

// redactRequireNoUser holds out to carrying neither the user nor its password.
func redactRequireNoUser(t *testing.T, out string) {
	t.Helper()
	require.NotContains(t, out, "s3cret")
	require.NotContains(t, out, "alice")
}

// review3 LOW-3, on a Client: doctor's role row names the address it dials
// without the user (the server_url row beside it warns that there is one),
// and so do serve's and client-config's refusals of a client install.
func TestRedact_ClientSurfacesNameNoUser(t *testing.T) {
	cli := config.Defaults()
	cli.Role = config.RoleClient
	cli.AdvertisedURL = "https://" + redactUser + "box.example:8081"
	cli.MCP.APIKey = "example-key"
	require.NoError(t, cli.Validate(), "it loads")

	rows := transportChecksIn(context.Background(), cli)
	require.Len(t, rows, 2)
	require.Equal(t, "role", rows[0].name)
	require.Equal(t, "client: this install runs no daemon and dials https://box.example:8081", rows[0].detail)
	require.Equal(t, "server_url", rows[1].name)
	require.Equal(t, statusWarn, rows[1].status)
	require.Contains(t, rows[1].detail, "names a user")
	for _, r := range rows {
		redactRequireNoUser(t, r.detail)
	}

	var out bytes.Buffer
	err := clientConfigReport(&out, cli, false)
	require.ErrorContains(t, err, "it dials https://box.example:8081;")
	redactRequireNoUser(t, err.Error())
	require.Empty(t, out.String())

	path := filepath.Join(t.TempDir(), "seamless.yaml")
	require.NoError(t, os.WriteFile(path, []byte("role: client\nserver_url: 'https://"+redactUser+"box.example:8081'\n"+
		"mcp:\n  api_key: 'example-key'\n"), 0o600))
	t.Setenv("SEAMLESS_CONFIG", path)
	err = runServe([]string{"--config", path})
	require.ErrorContains(t, err, "role: client runs no daemon -- this install dials https://box.example:8081;")
	redactRequireNoUser(t, err.Error())
}

// On a Server, `seamlessd client-config` refuses a server_url that names a
// user, --redact or not: every line it prints is a command for another
// machine, which would carry the password into its shell history. An
// address with no user hands out its commands as before.
func TestRedact_ClientConfigRefusesAUserInTheAddress(t *testing.T) {
	cfg := config.Defaults()
	cfg.Addr = "0.0.0.0:8081"
	cfg.AdvertisedURL = "https://" + redactUser + "box.example:8081"
	cfg.MCP.APIKey = "example-key-0123456789"
	for _, redact := range []bool{false, true} {
		var out bytes.Buffer
		err := clientConfigReport(&out, cfg, redact)
		require.ErrorContains(t, err, "server_url names a user: remove it, Seamless uses none")
		require.ErrorContains(t, err, "leave scheme://host:port in server_url")
		redactRequireNoUser(t, err.Error())
		require.Empty(t, out.String(), "no command is printed")
	}
	cfg.AdvertisedURL = "https://box.example:8081"
	var out bytes.Buffer
	require.NoError(t, clientConfigReport(&out, cfg, true))
	require.Contains(t, out.String(), "seamlessd install-hooks --server-url https://box.example:8081 --api-key "+redactedKey)
}

// The passkey row (doctor's, and serve's log under peer_auth) names the
// server_url its relying party is derived from without the user.
func TestRedact_PasskeyRowNamesNoUser(t *testing.T) {
	cfg := config.Defaults()
	cfg.AdvertisedURL = "https://" + redactUser + "box.example:8081"
	cfg.TLS.CertFile, cfg.TLS.KeyFile = filepath.Join(t.TempDir(), "c"), filepath.Join(t.TempDir(), "k")
	cfg.PeerAuth.Enabled = true
	rows := passkeyOriginChecks(context.Background(), cfg, "127.0.0.1:8081", nil,
		hostNet{lookup: func(context.Context, string) ([]netip.Addr, error) { return nil, fmt.Errorf("no resolver here") },
			ifaces: func() ([][]netip.Addr, error) { return nil, nil }})
	require.NotEmpty(t, rows)
	require.Contains(t, rows[0].detail, "relying party box.example (derived from server_url https://box.example:8081)")
	for _, r := range rows {
		redactRequireNoUser(t, r.detail)
	}
}

// setup's import refuses to restore while a daemon answers, and names where
// without the user.
func TestRedact_SetupImportRefusalNamesNoUser(t *testing.T) {
	r, _, _ := setupTestRun(t, "linux", nil, setupFactsFresh, false, "")
	r.daemonUp = func(config.Config) bool { return true }
	cfg := config.Defaults()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfg.AdvertisedURL = "http://" + redactUser + "127.0.0.1:8081"
	err := setupImportDestination(r, cfg)
	require.ErrorContains(t, err, "a Seamless daemon already answers at http://127.0.0.1:8081,")
	redactRequireNoUser(t, err.Error())
}

// review3 LOW-3, pairing: a Server whose server_url names a user prints no
// one-liner and mints no code -- setup's closing, `seamlessd devices code`
// (text and --json) -- and says to take the user out; the address it shows
// has none. The other computer's setup would refuse such an address anyway
// (setupServerURLProblem). Doctor's device pairing section says so too. The
// list still works, for the devices paired before.
func TestRedact_PairingRefusesAUserInTheAddress(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	cfg, der := devices2CodeConfig(t, dir)
	cfg.AdvertisedURL = "https://" + redactUser + "example-server.example.ts.net:8081"
	require.True(t, pairingOn(cfg))
	require.NotEmpty(t, setupServerURLProblem(cfg.ServerURL()), "the other computer's setup refuses the address")

	facts, err := pairingFacts(cfg)
	require.NoError(t, err)
	var b bytes.Buffer
	serverPairingClosing(&b, facts, "", false)
	closing := b.String()
	redactRequireNoUser(t, closing)
	require.Contains(t, closing, "Address:                 https://example-server.example.ts.net:8081\n")
	require.Contains(t, closing, devices.CertFingerprint(der))
	require.Contains(t, closing, "server_url names a user: remove it, Seamless uses none.")
	require.Contains(t, closing, "restart the daemon, then run `seamlessd devices code`")
	require.NotContains(t, closing, "SEAMLESS_PAIR_CODE", "no one-liner")
	require.NotContains(t, closing, "Pairing code:")

	for _, args := range [][]string{{"code"}, {"code", "--json"}} {
		var out bytes.Buffer
		cmd, err := parseDevices(args)
		require.NoError(t, err)
		err = cmd.run(ctx, db, cfg, &out)
		require.ErrorIs(t, err, devices.ErrPairingNamesUser, args)
		require.NotErrorIs(t, err, devices.ErrPairingNeedsTLS, "the address is https")
		redactRequireNoUser(t, err.Error())
		require.Empty(t, out.String())
	}
	open, err := store.CountOpenPairingCodes(ctx, db, time.Now())
	require.NoError(t, err)
	require.Zero(t, open, "no code was minted")
	var out bytes.Buffer
	cmd, err := parseDevices([]string{"list", "--json"})
	require.NoError(t, err)
	require.NoError(t, cmd.run(ctx, db, cfg, &out), "the devices paired before stay listed")

	rows := devicePairingChecks(ctx, cfg, db)
	found := false
	for _, r := range rows {
		redactRequireNoUser(t, r.detail)
		if strings.Contains(r.detail, "server_url names a user") {
			require.Equal(t, statusWarn, r.status)
			require.Contains(t, r.detail, "no code is minted and no one-liner printed")
			found = true
		}
	}
	require.True(t, found, "doctor says no code is minted here")

	// Both refusals where both hold, and the loopback note gives way: no
	// one-liner is printed for it to be about.
	plain := config.Defaults()
	plain.Addr = "0.0.0.0:8081"
	plain.AdvertisedURL = "http://" + redactUser + "192.168.1.20:8081"
	cmd, err = parseDevices([]string{"code"})
	require.NoError(t, err)
	err = cmd.run(ctx, db, plain, &out)
	require.ErrorIs(t, err, devices.ErrPairingNamesUser)
	require.ErrorIs(t, err, devices.ErrPairingNeedsTLS)
	b.Reset()
	serverPairingClosing(&b, devices.ServerFacts{URL: "http://" + redactUser + "127.0.0.1:8081"}, "", false)
	redactRequireNoUser(t, b.String())
	require.Contains(t, b.String(), "server_url names a user")
	require.NotContains(t, b.String(), "loopback")
	require.NotContains(t, b.String(), "SEAMLESS_PAIR_CODE")
	plain.AdvertisedURL = "http://" + redactUser + "127.0.0.1:8081"
	for _, r := range devicePairingChecks(ctx, plain, nil) {
		redactRequireNoUser(t, r.detail)
		require.NotContains(t, r.detail, "loopback", "no one-liner carries it")
	}
}

// With no user in server_url, `seamlessd devices code --json` is as it
// was: the server's address as configured, and the one-liners carrying it.
func TestRedact_DevicesCodeWithoutAUserIsAsItWas(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	cfg, _ := devices2CodeConfig(t, dir)
	var out bytes.Buffer
	cmd, err := parseDevices([]string{"code", "--json"})
	require.NoError(t, err)
	require.NoError(t, cmd.run(ctx, db, cfg, &out))
	var minted struct {
		Server devices.ServerFacts
		Unix   string
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &minted), out.String())
	require.Equal(t, "https://example-server.example.ts.net:8081", minted.Server.URL)
	require.Contains(t, minted.Unix, "SEAMLESS_SERVER_URL=https://example-server.example.ts.net:8081 ")
}
