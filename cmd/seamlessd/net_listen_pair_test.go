package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// netDualLoopback listens on 127.0.0.1 and [::1] on one free port, so a test
// owns both halves of the pair; it skips where this machine has no ::1.
func netDualLoopback(t *testing.T) (net.Listener, net.Listener) {
	t.Helper()
	for range 20 {
		ln4, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		_, port, _ := net.SplitHostPort(ln4.Addr().String())
		ln6, err := net.Listen("tcp", net.JoinHostPort("::1", port))
		if err == nil {
			t.Cleanup(func() { _ = ln4.Close(); _ = ln6.Close() })
			return ln4, ln6
		}
		_ = ln4.Close()
		if addrUnheld(err) {
			t.Skipf("no ::1 here: %v", err)
		}
	}
	t.Fatal("no port free on both loopback addresses")
	return nil, nil
}

// netRealListenErr is the error this machine's kernel gives a listen on addr.
func netRealListenErr(t *testing.T, addr string) error {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err == nil {
		_ = l.Close()
		t.Fatalf("listening on %s succeeded", addr)
	}
	return err
}

// The PoC of review M1, turned around: another account holding [::1]:P before
// serve stops the start, with an error naming the address, the cause and the
// fix -- and nothing serve bound stays open.
func TestListenPaired_SquattedLoopbackStopsTheStart(t *testing.T) {
	ln4, squat := netDualLoopback(t)
	port := ln4.Addr().(*net.TCPAddr).Port
	ifaces := func() ([][]netip.Addr, error) { return [][]netip.Addr{addrs("127.0.0.1", "::1")}, nil }
	paired, err := listenPaired(t.Context(), ln4, ifaces, net.Listen, pairedNeedFor(config.Defaults(), strconv.Itoa(port)))
	require.Empty(t, paired)
	var refusal *pairedListenError
	require.ErrorAs(t, err, &refusal)
	require.True(t, addrInUse(err), "the cause is the kernel's: %v", err)
	addr := net.JoinHostPort("::1", strconv.Itoa(port))
	require.Contains(t, err.Error(), addr+" is in use by another process, and serve must listen there beside 127.0.0.1:"+strconv.Itoa(port))
	require.Contains(t, err.Error(), "localhost is both loopback addresses")
	require.Contains(t, err.Error(), "the service starts serve again, and it runs once the address is free")
	if runtime.GOOS != "windows" {
		require.Contains(t, err.Error(), "sudo lsof -nP -iTCP:"+strconv.Itoa(port)+" -sTCP:LISTEN names it")
	}
	require.NoError(t, squat.Close())

	// Once the address is free, the same start holds it.
	paired, err = listenPaired(t.Context(), ln4, ifaces, net.Listen, pairedNeed{})
	require.NoError(t, err)
	require.Equal(t, []string{"::1"}, listenerHosts(paired))
	for _, l := range paired {
		_ = l.Close()
	}
}

// Which failures stop the start: an address another process may hold that
// serve must hold (::1, an address of a name browsers and clients dial, any
// under peer_auth). An address no process can hold now never does, and on an
// install without peer_auth neither does one no name points at.
func TestListenPaired_WhichFailuresStopTheStart(t *testing.T) {
	// Real errors from this machine's kernel: a port held, and an address it
	// does not have (EADDRNOTAVAIL, or EAFNOSUPPORT where IPv6 is off).
	holder, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = holder.Close() }()
	inUse := netRealListenErr(t, holder.Addr().String())
	require.True(t, addrInUse(inUse), "%v", inUse)
	notHere := netRealListenErr(t, "[fd00:dead:beef::1]:0")
	require.True(t, addrUnheld(notHere), "an address this machine does not have is not in use: %v", notHere)
	refused := errors.New("bind: permission denied")

	tailnet := fakeLookup(map[string][]string{"studio.example.ts.net": {"100.64.0.7", "fd7a:115c:a1e0::7"}})
	for _, tc := range []struct {
		name   string
		bind   string
		fails  map[string]error
		need   pairedNeed
		fatal  string // a substring of the refusal; "" = the start goes on
		listed []string
	}{
		{"::1 in use", "127.0.0.1:43117", map[string]error{"[::1]:43117": inUse}, pairedNeed{},
			"[::1]:43117 is in use by another process", nil},
		{"::1 refused otherwise", "127.0.0.1:43117", map[string]error{"[::1]:43117": refused}, pairedNeed{},
			"cannot listen on [::1]:43117 (bind: permission denied)", nil},
		{"::1 not this machine's", "127.0.0.1:43117", map[string]error{"[::1]:43117": notHere}, pairedNeed{all: true}, "", nil},
		{"a tailnet IPv6 no name points at, without peer_auth", "100.64.0.7:43117",
			map[string]error{"[fd7a:115c:a1e0::7]:43117": inUse}, pairedNeed{}, "", nil},
		{"a tailnet IPv6 server_url's host resolves to", "100.64.0.7:43117",
			map[string]error{"[fd7a:115c:a1e0::7]:43117": inUse},
			pairedNeed{hosts: []string{"studio.example.ts.net"}, lookup: tailnet},
			"studio.example.ts.net, a name browsers and clients dial this daemon by, resolves to it", nil},
		{"a name that does not resolve now cannot be ruled out", "100.64.0.7:43117",
			map[string]error{"[fd7a:115c:a1e0::7]:43117": inUse},
			pairedNeed{hosts: []string{"elsewhere.example.ts.net"}, lookup: tailnet}, "does not resolve here now", nil},
		{"an IP host that is another address", "100.64.0.7:43117",
			map[string]error{"[fd7a:115c:a1e0::7]:43117": inUse}, pairedNeed{hosts: []string{"100.64.0.7"}}, "", nil},
		{"under peer_auth any address in use", "100.64.0.7:43117",
			map[string]error{"[fd7a:115c:a1e0::7]:43117": inUse}, pairedNeed{all: true}, "locked projects (peer_auth) are on", nil},
		{"under peer_auth an address not this machine's", "100.64.0.7:43117",
			map[string]error{"[fd7a:115c:a1e0::7]:43117": notHere}, pairedNeed{all: true}, "", nil},
		{"the second of two in use under peer_auth closes the first", "203.0.113.5:43117",
			map[string]error{"[fd00:5::5]:43117": inUse}, pairedNeed{all: true}, "[fd00:5::5]:43117 is in use", nil},
		{"the second of two in use without a reason", "203.0.113.5:43117",
			map[string]error{"[fd00:5::5]:43117": inUse}, pairedNeed{}, "", []string{"2001:db8:5::5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var closed []string
			listen := func(_, address string) (net.Listener, error) {
				if err := tc.fails[address]; err != nil {
					return nil, err
				}
				return netClosingListener{tcpListener(address), &closed}, nil
			}
			ifaces := func() ([][]netip.Addr, error) { return studioIfaces, nil }
			got, err := listenPaired(t.Context(), tcpListener(tc.bind), ifaces, listen, tc.need)
			if tc.fatal == "" {
				require.NoError(t, err)
				require.Equal(t, tc.listed, netNilIfEmpty(listenerHosts(got)))
				return
			}
			require.ErrorContains(t, err, tc.fatal)
			require.Empty(t, got)
			if tc.bind == "203.0.113.5:43117" {
				require.Equal(t, []string{"[2001:db8:5::5]:43117"}, closed, "what serve bound before the refusal is closed")
			}
		})
	}

	// No ::1 on any interface: nothing is listened on, so nothing can fail.
	got, err := listenPaired(t.Context(), tcpListener("127.0.0.1:43117"),
		func() ([][]netip.Addr, error) { return [][]netip.Addr{addrs("127.0.0.1")}, nil },
		func(string, string) (net.Listener, error) { t.Fatal("no address to listen on"); return nil, nil }, pairedNeed{all: true})
	require.NoError(t, err)
	require.Empty(t, got)
	// Interfaces that cannot be read: today's warning, except under peer_auth.
	broken := func() ([][]netip.Addr, error) { return nil, errors.New("no interfaces") }
	_, err = listenPaired(t.Context(), tcpListener("127.0.0.1:43117"), broken, net.Listen, pairedNeed{})
	require.NoError(t, err)
	_, err = listenPaired(t.Context(), tcpListener("127.0.0.1:43117"), broken, net.Listen, pairedNeed{all: true})
	require.ErrorContains(t, err, "cannot read this machine's interfaces")
}

// A real listen on an address this machine's kernel does not have fails as
// no process can hold it, so even under peer_auth the start goes on: the CI
// sandboxes without IPv6 loopback, and a tailnet address not up yet.
func TestListenPaired_AnAddressNoOneCanHoldIsNotFatal(t *testing.T) {
	ifaces := func() ([][]netip.Addr, error) {
		return [][]netip.Addr{addrs("10.255.255.1", "fd00:dead:beef::1")}, nil
	}
	got, err := listenPaired(context.Background(), tcpListener("10.255.255.1:43117"), ifaces, net.Listen, pairedNeed{all: true})
	require.NoError(t, err)
	require.Empty(t, got)
}

// pairedNeedFor names the hosts browsers and clients are sent to this port by.
func TestPairedNeedFor(t *testing.T) {
	tlsCfg := func(c config.Config) config.Config {
		c.TLS.CertFile, c.TLS.KeyFile = "cert.pem", "key.pem"
		return c
	}
	def := config.Defaults()
	require.Equal(t, pairedNeed{lookup: nil}, netNoLookup(pairedNeedFor(def, "8081")), "a default install names no host")

	tail := tlsCfg(def)
	tail.Addr, tail.AdvertisedURL = "100.64.0.7:8081", "https://Studio.example.ts.net:8081"
	require.Equal(t, []string{"studio.example.ts.net"}, pairedNeedFor(tail, "8081").hosts)
	require.Empty(t, pairedNeedFor(tail, "9443").hosts, "a name on another port is a proxy's")

	proxy := tail
	proxy.AdvertisedURL = "https://studio.example.ts.net"
	require.Empty(t, pairedNeedFor(proxy, "8081").hosts)

	signIn := tail
	signIn.Console.SignIn = config.ConsoleSignInPasskey
	signIn.PeerAuth.Passkeys.RPID = "example.ts.net"
	signIn.PeerAuth.Passkeys.Origins = []string{"https://desk.example.ts.net:8081"}
	require.Equal(t, []string{"studio.example.ts.net", "desk.example.ts.net"}, pairedNeedFor(signIn, "8081").hosts)

	peer := def
	peer.PeerAuth.Enabled = true
	require.True(t, pairedNeedFor(peer, "8081").all)

	sock := def
	sock.AdvertisedURL = "unix:///tmp/example.sock"
	require.Empty(t, pairedNeedFor(sock, "8081").hosts)
}

// netNoLookup drops the resolver, for comparing the rest.
func netNoLookup(n pairedNeed) pairedNeed {
	n.lookup = nil
	return n
}

// netClosingListener records its own Close.
type netClosingListener struct {
	net.Listener
	closed *[]string
}

func (l netClosingListener) Close() error {
	*l.closed = append(*l.closed, l.Addr().String())
	return nil
}

func netNilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
