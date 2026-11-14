package main

import (
	"context"
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// tlsFileMode is a stat answer of one mode, for a key whose mode a test
// chooses on any OS.
type tlsFileMode struct{ mode fs.FileMode }

func (m tlsFileMode) Name() string       { return "key.pem" }
func (m tlsFileMode) Size() int64        { return 0 }
func (m tlsFileMode) Mode() fs.FileMode  { return m.mode }
func (m tlsFileMode) ModTime() time.Time { return time.Time{} }
func (m tlsFileMode) IsDir() bool        { return false }
func (m tlsFileMode) Sys() any           { return nil }

// tlsDoctorEnv is a machine for doctor's name rows: the tailnet's addresses,
// a stat that answers mode for every file, and no tailnet unless ts is set.
func tlsDoctorEnv(lookup map[string][]string, ifaces [][]netip.Addr, mode fs.FileMode, ts tailscaleCLI) doctorTLSEnv {
	if ts == nil {
		ts = setupAddrTailnet{err: errNoTailscale}
	}
	return doctorTLSEnv{
		net:       hostNet{lookup: fakeLookup(lookup), ifaces: func() ([][]netip.Addr, error) { return ifaces, nil }},
		tailscale: ts,
		goos:      "linux",
		stat:      func(string) (os.FileInfo, error) { return tlsFileMode{mode}, nil },
	}
}

// tlsServerConfig is a TLS Server reached at name on bind.
func tlsServerConfig(t *testing.T, name, bind string) config.Config {
	t.Helper()
	cert, key := writeCertPair(t, time.Now().Add(90*24*time.Hour), name)
	cfg := config.Defaults()
	cfg.Addr, cfg.AdvertisedURL, cfg.TLS.CertFile, cfg.TLS.KeyFile = bind, "https://"+name+":8081", cert, key
	return cfg
}

func tlsRowNamed(rows []check, name string) (check, bool) {
	for _, r := range rows {
		if r.name == name {
			return r, true
		}
	}
	return check{}, false
}

// Who can read the key of a daemon that runs as the owner: one account, the
// daemon's, is said as a fact; a group or every account is a warning.
func TestTLSKeyCheck(t *testing.T) {
	tailnet := map[string][]string{setupAddrTSName: {setupAddrTailv4, setupAddrTailv6}}
	cfg := tlsServerConfig(t, setupAddrTSName, setupAddrTailv4+":8081")
	for _, tc := range []struct {
		name   string
		mode   fs.FileMode
		status checkStatus
		says   string
	}{
		{"this account alone", 0o600, statusInfo, "readable by this account alone, which the daemon runs as: every process of this account can read it"},
		{"its group", 0o640, statusWarn, "the accounts of its group can read the TLS private key"},
		{"every account", 0o644, statusWarn, "every account on this computer can read the TLS private key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := serverNameChecks(context.Background(), cfg, tlsDoctorEnv(tailnet, setupAddrIfaces, tc.mode, nil))
			row, ok := tlsRowNamed(rows, "tls.key_file")
			require.True(t, ok, rowsText(rows))
			require.Equal(t, tc.status, row.status, row.detail)
			require.Contains(t, row.detail, tc.says)
			require.Contains(t, row.detail, setupAddrTSName)
		})
	}
	// Under peer_auth the preflight judges the key (its rows), and on Windows
	// a mode says nothing.
	pa := cfg
	pa.PeerAuth.Enabled = true
	_, ok := tlsKeyCheck(pa, tlsDoctorEnv(tailnet, setupAddrIfaces, 0o644, nil))
	require.False(t, ok)
	win := tlsDoctorEnv(tailnet, setupAddrIfaces, 0o666, nil)
	win.goos = "windows"
	_, ok = tlsKeyCheck(cfg, win)
	require.False(t, ok)
	// A key it cannot stat is said, not skipped.
	gone := tlsDoctorEnv(tailnet, setupAddrIfaces, 0o600, nil)
	gone.stat = func(string) (os.FileInfo, error) { return nil, errors.New("permission denied") }
	row, ok := tlsKeyCheck(cfg, gone)
	require.True(t, ok)
	require.Equal(t, statusWarn, row.status)
	if runtime.GOOS != "windows" {
		// The real stat reads the real mode.
		real := tlsDoctorEnv(tailnet, setupAddrIfaces, 0, nil)
		real.stat = os.Stat
		require.NoError(t, os.Chmod(cfg.TLS.KeyFile, 0o644))
		row, ok = tlsKeyCheck(cfg, real)
		require.True(t, ok)
		require.Equal(t, statusWarn, row.status, row.detail)
	}
}

// Every address server_url's host resolves to, judged against what the
// daemon listens on, as setup picks the bind and the passkey rows judge an
// origin.
func TestServerURLAddressesCheck(t *testing.T) {
	dualStack := addrs(setupAddrTailv4, setupAddrTailv6)
	split := [][]netip.Addr{addrs("127.0.0.1", "::1"), addrs(setupAddrTailv4), addrs(setupAddrTailv6)}
	tailnet := map[string][]string{setupAddrTSName: {setupAddrTailv4, setupAddrTailv6}}
	for _, tc := range []struct {
		name   string
		bind   string
		lookup map[string][]string
		ifaces [][]netip.Addr
		status checkStatus
		says   string
	}{
		{"the tailnet's IPv4 holds the IPv6 beside it", setupAddrTailv4 + ":8081", tailnet, [][]netip.Addr{addrs("127.0.0.1"), dualStack},
			statusOK, "every address of it held by this daemon on port 8081"},
		{"an IPv6 on another interface is another process's to take", setupAddrTailv4 + ":8081", tailnet, split,
			statusWarn, "this daemon does not listen on " + setupAddrTailv6},
		{"the wildcard holds them all", "0.0.0.0:8081", tailnet, split, statusOK, "every address of it held"},
		{"an address of another computer", setupAddrTailv4 + ":8081",
			map[string][]string{setupAddrTSName: {setupAddrTailv4, "203.0.113.50"}}, [][]netip.Addr{dualStack},
			statusWarn, "this computer does not hold 203.0.113.50"},
		{"a name that does not resolve", setupAddrTailv4 + ":8081", nil, [][]netip.Addr{dualStack}, statusWarn, "does not resolve here"},
		// A resolver names a link-local address's interface; the interface lists
		// the address alone. The zone is not a different address.
		{"a link-local address of the name, with its zone, on a wildcard", "0.0.0.0:8081",
			map[string][]string{setupAddrTSName: {setupAddrTailv4, "fe80::7%utun3"}}, [][]netip.Addr{addrs(setupAddrTailv4, "fe80::7")},
			statusOK, "every address of it held"},
		{"a link-local address of the name, with its zone, beside an IPv4 bind", setupAddrTailv4 + ":8081",
			map[string][]string{setupAddrTSName: {setupAddrTailv4, "fe80::7%utun3"}}, [][]netip.Addr{addrs(setupAddrTailv4, "fe80::7")},
			statusWarn, "this daemon does not listen on fe80::7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tlsServerConfig(t, setupAddrTSName, tc.bind)
			row, ok := serverURLAddressesCheck(context.Background(), cfg, hostNet{lookup: fakeLookup(tc.lookup),
				ifaces: func() ([][]netip.Addr, error) { return tc.ifaces, nil }})
			require.True(t, ok)
			require.Equal(t, tc.status, row.status, row.detail)
			require.Contains(t, row.detail, tc.says)
		})
	}

	// No row: a server_url derived from addr, a loopback one, a proxy on
	// another port, and a host the passkey rows judge already.
	none := hostNet{lookup: func(context.Context, string) ([]netip.Addr, error) {
		t.Fatal("no name should be resolved")
		return nil, nil
	}, ifaces: func() ([][]netip.Addr, error) { return nil, nil }}
	derived := config.Defaults()
	derived.Addr = setupAddrTailv4 + ":8081"
	loop := config.Defaults()
	loop.AdvertisedURL = "http://localhost:8081"
	proxied := tlsServerConfig(t, setupAddrTSName, "127.0.0.1:9000")
	passkeys := tlsServerConfig(t, setupAddrTSName, setupAddrTailv4+":8081")
	passkeys.PeerAuth.Enabled = true
	signIn := tlsServerConfig(t, setupAddrTSName, setupAddrTailv4+":8081")
	signIn.Console.SignIn = config.ConsoleSignInPasskey
	for name, cfg := range map[string]config.Config{"derived": derived, "loopback": loop, "a proxy": proxied, "peer_auth's passkeys": passkeys,
		"passkey sign-in": signIn} {
		_, ok := serverURLAddressesCheck(context.Background(), cfg, none)
		require.False(t, ok, name)
	}
}

// When the tailnet issues certificates for the name, a valid certificate is
// not the TLS key's to withhold: doctor says so.
func TestTailnetCertCheck(t *testing.T) {
	cfg := tlsServerConfig(t, setupAddrTSName, setupAddrTailv4+":8081")
	row, ok := tailnetCertCheck(context.Background(), cfg, setupAddrTailnet{st: setupAddrTailnetUp})
	require.True(t, ok)
	require.Equal(t, statusInfo, row.status)
	require.Contains(t, row.detail, "the tailnet issues certificates for "+setupAddrTSName+" (tailscale cert)")
	require.Contains(t, row.detail, "the TLS key is not what keeps its pages off the console's origin")

	noCerts := setupAddrTailnetUp
	noCerts.certDomains = nil
	for name, ts := range map[string]tailscaleCLI{"no tailscale": setupAddrTailnet{err: errNoTailscale},
		"no certificates on the tailnet": setupAddrTailnet{st: noCerts}} {
		_, ok := tailnetCertCheck(context.Background(), cfg, ts)
		require.False(t, ok, name)
	}
	plain := config.Defaults()
	plain.AdvertisedURL = "https://" + setupAddrTSName + ":8081"
	_, ok = tailnetCertCheck(context.Background(), plain, setupAddrTailnet{st: setupAddrTailnetUp})
	require.False(t, ok, "no TLS, no certificate to speak of")
}

// A default install serves no other computer: doctor's transport rows are
// the three they always were, and nothing asks the network or the tailnet.
// (Its addr is a closed loopback port: the probe of server_url dials it.)
func TestTransportChecks_DefaultInstallUnchanged(t *testing.T) {
	quiet := doctorTLSEnv{
		net: hostNet{lookup: func(context.Context, string) ([]netip.Addr, error) {
			t.Fatal("a default install resolves no name")
			return nil, nil
		}, ifaces: func() ([][]netip.Addr, error) { return [][]netip.Addr{addrs("127.0.0.1", "::1")}, nil }},
		tailscale: setupAddrTailnet{err: errors.New("a default install asks no tailnet")},
		goos:      "linux",
		stat: func(string) (os.FileInfo, error) {
			t.Fatal("a default install reads no key")
			return nil, nil
		},
	}
	def := config.Defaults()
	def.Addr = "127.0.0.1:0"
	rows := transportChecksWith(def, quiet)
	require.Len(t, rows, 3, rowsText(rows))
	require.Equal(t, []string{"bind", "server_url", "tls"}, []string{rows[0].name, rows[1].name, rows[2].name})
	require.Nil(t, serverNameChecks(context.Background(), config.Defaults(), quiet))

	// A Server setup made: the facts setup said, as doctor's rows.
	cfg := tlsServerConfig(t, setupAddrTSName, setupAddrTailv4+":8081")
	env := tlsDoctorEnv(map[string][]string{setupAddrTSName: {setupAddrTailv4, setupAddrTailv6}},
		[][]netip.Addr{addrs("127.0.0.1", "::1"), addrs(setupAddrTailv4, setupAddrTailv6)}, 0o600, setupAddrTailnet{st: setupAddrTailnetUp})
	rows = serverNameChecks(context.Background(), cfg, env)
	for _, name := range []string{"tls.key_file", "server_url.addresses", "tailscale"} {
		_, ok := tlsRowNamed(rows, name)
		require.True(t, ok, "%s in %s", name, rowsText(rows))
	}
	bind := bindCheck(cfg, bindPaired(context.Background(), cfg.Addr, env.net))
	require.Contains(t, bind.detail, "[fd7a:115c:a1e0::7]:8081 beside it")
}
