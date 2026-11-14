package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// netServeHealthz answers /healthz on ln as a daemon named instance would
// ("" answers as something that is not a Seamless daemon).
func netServeHealthz(t *testing.T, ln net.Listener, instance string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if instance == "" {
			_, _ = fmt.Fprint(w, "squat!")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"status":"ok","instance":%q}`, instance)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// netLoopbackNet is a machine whose loopback interface holds both addresses,
// and whose resolver answers one name with them.
var netLoopbackNet = hostNet{
	lookup: fakeLookup(map[string][]string{"studio.example.test": {"127.0.0.1", "::1"}}),
	ifaces: func() ([][]netip.Addr, error) { return [][]netip.Addr{addrs("127.0.0.1", "::1")}, nil },
}

// netDoctorCtx is doctor's context for a daemon at bind: its probe, which
// dials only the scratch listeners the test made.
func netDoctorCtx(cfg config.Config) context.Context {
	return withListenerProbe(context.Background(), doctorListenerProbe(cfg, 2*time.Second))
}

// Doctor dials the addresses beside the bind: another process answering at
// [::1] is a FAIL, on the bind.addresses row and on the passkey origin it
// would serve, and the server_url name no longer reads as held.
func TestDoctorNet_SquattedPairIsAFail(t *testing.T) {
	ln4, ln6 := netDualLoopback(t)
	netServeHealthz(t, ln4, "01M7DAEMON")
	netServeHealthz(t, ln6, "")
	port := strconv.Itoa(ln4.Addr().(*net.TCPAddr).Port)
	cfg := config.Defaults()
	cfg.Addr = "127.0.0.1:" + port
	ctx := netDoctorCtx(cfg)

	held, err := boundAddrs(ctx, cfg.Addr, netLoopbackNet)
	require.NoError(t, err)
	require.False(t, held.covers(netip.MustParseAddr("::1")), "the squatted address is not held")
	require.Equal(t, addrs("::1"), held.taken)

	row, ok := bindAddressesCheck(ctx, cfg, netLoopbackNet)
	require.True(t, ok)
	require.Equal(t, statusFail, row.status, row.detail)
	require.Contains(t, row.detail, "another process answers at [::1]:"+port+", not this daemon (instance 01M7DAEMON at 127.0.0.1:"+port+")")

	rows := passkeyOriginChecks(ctx, cfg, cfg.Addr, nil, netLoopbackNet)
	text := rowsText(rows)
	require.Equal(t, statusFail, worst(rows), text)
	require.Contains(t, text, "origin http://localhost:"+port+": another process answers at ::1 port "+port)

	named := cfg
	named.AdvertisedURL = "http://studio.example.test:" + port
	c, ok := serverURLAddressesCheck(ctx, named, netLoopbackNet)
	require.True(t, ok)
	require.Equal(t, statusWarn, c.status, c.detail)
	require.Contains(t, c.detail, "this daemon does not listen on ::1")

	// The bind row names the configuration, as before; without the probe
	// nothing is dialed and the rows infer as they did.
	require.Contains(t, bindCheck(cfg, bindPaired(ctx, cfg.Addr, netLoopbackNet)).detail, "(and [::1]:"+port+" beside it")
	_, ok = bindAddressesCheck(context.Background(), cfg, netLoopbackNet)
	require.False(t, ok)
	held, err = boundAddrs(context.Background(), cfg.Addr, netLoopbackNet)
	require.NoError(t, err)
	require.True(t, held.covers(netip.MustParseAddr("::1")))
}

// The other answers: one daemon at both is OK; nothing beside a daemon that
// answers is a WARN; something beside a daemon that does not is a FAIL;
// nothing anywhere is no row.
func TestDoctorNet_BindAddresses(t *testing.T) {
	t.Run("one daemon at both", func(t *testing.T) {
		ln4, ln6 := netDualLoopback(t)
		netServeHealthz(t, ln4, "01M7DAEMON")
		netServeHealthz(t, ln6, "01M7DAEMON")
		cfg := config.Defaults()
		cfg.Addr = ln4.Addr().String()
		row, ok := bindAddressesCheck(netDoctorCtx(cfg), cfg, netLoopbackNet)
		require.True(t, ok)
		require.Equal(t, statusOK, row.status, row.detail)
		require.Contains(t, row.detail, "this daemon answers at "+cfg.Addr+" and [::1]:")
		rows := passkeyOriginChecks(netDoctorCtx(cfg), cfg, cfg.Addr, nil, netLoopbackNet)
		require.Equal(t, statusOK, worst(rows), rowsText(rows))
	})
	t.Run("nothing beside a daemon that answers", func(t *testing.T) {
		ln4, ln6 := netDualLoopback(t)
		require.NoError(t, ln6.Close())
		netServeHealthz(t, ln4, "01M7DAEMON")
		cfg := config.Defaults()
		cfg.Addr = ln4.Addr().String()
		row, ok := bindAddressesCheck(netDoctorCtx(cfg), cfg, netLoopbackNet)
		require.True(t, ok)
		require.Equal(t, statusWarn, row.status, row.detail)
		require.Contains(t, row.detail, "nothing answers at [::1]:")
	})
	t.Run("something beside a daemon that does not answer", func(t *testing.T) {
		ln4, ln6 := netDualLoopback(t)
		require.NoError(t, ln4.Close())
		netServeHealthz(t, ln6, "01M7OTHER")
		cfg := config.Defaults()
		cfg.Addr = ln4.Addr().String()
		row, ok := bindAddressesCheck(netDoctorCtx(cfg), cfg, netLoopbackNet)
		require.True(t, ok)
		require.Equal(t, statusFail, row.status, row.detail)
		require.Contains(t, row.detail, "where this daemon is not answering")
	})
	t.Run("nothing anywhere", func(t *testing.T) {
		ln4, ln6 := netDualLoopback(t)
		require.NoError(t, ln4.Close())
		require.NoError(t, ln6.Close())
		cfg := config.Defaults()
		cfg.Addr = ln4.Addr().String()
		_, ok := bindAddressesCheck(netDoctorCtx(cfg), cfg, netLoopbackNet)
		require.False(t, ok, "the server_url row says the daemon is not answering")
		held, err := boundAddrs(netDoctorCtx(cfg), cfg.Addr, netLoopbackNet)
		require.NoError(t, err)
		require.True(t, held.covers(netip.MustParseAddr("::1")), "serve listens there when it starts, or refuses to")
	})
	t.Run("a wildcard bind is not dialed", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Addr = "0.0.0.0:1"
		_, ok := bindAddressesCheck(withListenerProbe(context.Background(), func(context.Context, netip.AddrPort) listenerAnswer {
			t.Fatal("dialed")
			return listenerAnswer{}
		}), cfg, netLoopbackNet)
		require.False(t, ok)
	})
}

// A client whose socket does not answer is told what serves it here: the
// relay's service when it is installed, its root step when not, and the
// daemon beside a peer_auth system service.
func TestDoctorNet_ClientSocketNamesTheRelay(t *testing.T) {
	cfg := config.Defaults()
	cfg.Role = config.RoleClient
	cfg.AdvertisedURL = "unix://" + filepath.Join(t.TempDir(), "seamlessd.sock")
	files := func(present ...string) func(string) bool {
		return func(p string) bool {
			for _, f := range present {
				if p == f {
					return true
				}
			}
			return false
		}
	}
	for _, tc := range []struct {
		name string
		env  relaySocketEnv
		says []string
	}{
		{"macOS, the relay installed", relaySocketEnv{"darwin", files(clientRelayPlistPath())},
			[]string{"the relay serves it on this computer, as the LaunchDaemon com.arctop.seamless.relay",
				"sudo launchctl kickstart -k system/com.arctop.seamless.relay"}},
		{"Linux, before the root step", relaySocketEnv{"linux", files()},
			[]string{"its root step has not run", "guides/network-install/#the-relays-root-step", "the system unit seamless-relay.service"}},
		{"Linux, the relay installed", relaySocketEnv{"linux", files("/etc/systemd/system/seamless-relay.service")},
			[]string{"sudo systemctl restart seamless-relay"}},
		{"macOS, a peer_auth daemon here", relaySocketEnv{"darwin", files("/Library/LaunchDaemons/com.arctop.seamless.daemon.plist")},
			[]string{"start seamlessd there, or correct server_url"}},
		{"Windows", relaySocketEnv{"windows", files()}, []string{"start seamlessd there"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := clientServerURLCheckWith(cfg, tc.env)
			require.Equal(t, statusFail, c.status, c.detail)
			for _, s := range tc.says {
				require.Contains(t, c.detail, s)
			}
		})
	}
	tcp := config.Config{Role: config.RoleClient, AdvertisedURL: "http://127.0.0.1:1"}
	c := clientServerURLCheckWith(tcp, relaySocketEnv{"darwin", files(clientRelayPlistPath())})
	require.Contains(t, c.detail, "start seamlessd there, or correct server_url", "a TCP server_url is the server's")
}
