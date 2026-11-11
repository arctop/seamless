package main

import (
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, len(s))
	for i, a := range s {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

// A machine with a loopback, a tailnet and a LAN interface, each dual-stack.
var studioIfaces = [][]netip.Addr{
	addrs("127.0.0.1", "::1", "fe80::1"),
	addrs("100.64.0.7", "fd7a:115c:a1e0::7"),
	addrs("192.168.1.9", "fe80::aa", "2001:db8::9", "fd00:1::9"),
	addrs("203.0.113.5", "2001:db8:5::5", "fd00:5::5"),
}

// Beside an IPv4 address, serve takes its own interface's IPv6 addresses that
// reach no further than it does: never a link-local one, and never a global one
// beside a private address.
func TestPairedIPv6(t *testing.T) {
	for _, tc := range []struct {
		name string
		v4   string
		want []netip.Addr
	}{
		{"loopback gets ::1", "127.0.0.1", addrs("::1")},
		{"a tailnet address gets the tailnet's IPv6", "100.64.0.7", addrs("fd7a:115c:a1e0::7")},
		{"a LAN address gets the unique local address, not the global one", "192.168.1.9", addrs("fd00:1::9")},
		{"a public address gets the global and the unique local ones", "203.0.113.5", addrs("2001:db8:5::5", "fd00:5::5")},
		{"an address no interface holds gets nothing", "10.9.9.9", nil},
		{"an IPv6 bind gets nothing", "fd7a:115c:a1e0::7", nil},
		{"a link-local IPv4 gets nothing", "169.254.1.1", nil},
		{"a v4-mapped spelling is the IPv4 address", "::ffff:100.64.0.7", addrs("fd7a:115c:a1e0::7")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ifaces := studioIfaces
			if tc.v4 == "169.254.1.1" {
				ifaces = append(ifaces, addrs("169.254.1.1", "fe80::9"))
			}
			require.Equal(t, tc.want, pairedIPv6(netip.MustParseAddr(tc.v4), ifaces))
		})
	}
	require.Nil(t, pairedIPv6(netip.MustParseAddr("192.168.1.9"), [][]netip.Addr{addrs("192.168.1.9", "fe80::aa", "2001:db8::9")}),
		"a LAN with only global IPv6 addresses pairs with none of them")
}

func TestReachOf(t *testing.T) {
	for a, want := range map[string]reach{
		"127.0.0.1":         reachLoopback,
		"::1":               reachLoopback,
		"10.0.0.1":          reachPrivate,
		"172.16.0.1":        reachPrivate,
		"192.168.0.1":       reachPrivate,
		"100.64.0.1":        reachPrivate,
		"100.127.255.254":   reachPrivate,
		"fd7a:115c:a1e0::7": reachPrivate,
		"100.128.0.1":       reachPublic,
		"203.0.113.5":       reachPublic,
		"2001:db8::9":       reachPublic,
		"fe80::1":           reachNone,
		"169.254.1.1":       reachNone,
		"0.0.0.0":           reachNone,
		"::":                reachNone,
		"ff02::1":           reachNone,
	} {
		require.Equal(t, want, reachOf(netip.MustParseAddr(a)), a)
	}
}

// fakeListener stands in for a bound TCP listener.
type fakeListener struct{ addr net.Addr }

func (f fakeListener) Accept() (net.Conn, error) { return nil, errors.New("fake") }
func (f fakeListener) Close() error              { return nil }
func (f fakeListener) Addr() net.Addr            { return f.addr }

func tcpListener(ap string) net.Listener {
	return fakeListener{net.TCPAddrFromAddrPort(netip.MustParseAddrPort(ap))}
}

// listenPaired binds the paired addresses on the listener's real port, skips
// one it cannot bind, and pairs nothing with an IPv6 or wildcard listener.
func TestListenPaired(t *testing.T) {
	ifaces := func() ([][]netip.Addr, error) { return studioIfaces, nil }
	var dialed []string
	listen := func(network, address string) (net.Listener, error) {
		require.Equal(t, "tcp", network)
		dialed = append(dialed, address)
		if address == "[fd00:5::5]:43117" {
			return nil, errors.New("address already in use")
		}
		return tcpListener(address), nil
	}

	got := listenPaired(tcpListener("100.64.0.7:43117"), ifaces, listen)
	require.Equal(t, []string{"[fd7a:115c:a1e0::7]:43117"}, dialed)
	require.Equal(t, []string{"fd7a:115c:a1e0::7"}, listenerHosts(got))

	dialed = nil
	got = listenPaired(tcpListener("203.0.113.5:43117"), ifaces, listen)
	require.Equal(t, []string{"[2001:db8:5::5]:43117", "[fd00:5::5]:43117"}, dialed)
	require.Equal(t, []string{"2001:db8:5::5"}, listenerHosts(got), "the address in use is skipped")

	dialed = nil
	require.Empty(t, listenPaired(tcpListener("[::]:43117"), ifaces, listen))
	require.Empty(t, listenPaired(tcpListener("[fd7a:115c:a1e0::7]:43117"), ifaces, listen))
	require.Empty(t, listenPaired(tcpListener("127.0.0.1:43117"),
		func() ([][]netip.Addr, error) { return nil, errors.New("no interfaces") }, listen))
	require.Empty(t, dialed)
}

// The pair really serves: a loopback listener's ::1 partner accepts on the
// same port, where this machine has ::1 at all.
func TestListenPaired_Loopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	got := listenPaired(ln, func() ([][]netip.Addr, error) { return [][]netip.Addr{addrs("127.0.0.1", "::1")}, nil }, net.Listen)
	if len(got) == 0 {
		t.Skip("no ::1 here, or its port is taken")
	}
	t.Cleanup(func() { _ = got[0].Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	require.Equal(t, net.JoinHostPort("::1", port), got[0].Addr().String())
	go func() {
		if c, err := got[0].Accept(); err == nil {
			_ = c.Close()
		}
	}()
	c, err := net.Dial("tcp", got[0].Addr().String())
	require.NoError(t, err)
	_ = c.Close()
}

func TestBindPairedAndSuffix(t *testing.T) {
	hn := hostNet{lookup: fakeLookup(nil), ifaces: func() ([][]netip.Addr, error) { return studioIfaces, nil }}
	require.Equal(t, addrs("fd7a:115c:a1e0::7"), bindPaired(t.Context(), "100.64.0.7:8081", hn))
	require.Nil(t, bindPaired(t.Context(), "0.0.0.0:8081", hn))
	require.Nil(t, bindPaired(t.Context(), "nowhere.local:8081", hn))
	require.Equal(t, " (and [fd7a:115c:a1e0::7]:8081 beside it, on the same interface)",
		pairedSuffix("8081", addrs("fd7a:115c:a1e0::7")))
	require.Empty(t, pairedSuffix("8081", nil))
}
