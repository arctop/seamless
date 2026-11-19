//go:build linux

package main

import (
	"net"
	"net/netip"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// fbNeedIPv6 skips where this machine cannot listen on IPv6 at all (a CI
// sandbox without ::1).
func fbNeedIPv6(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		if addrUnheld(err) {
			t.Skipf("no IPv6 here: %v", err)
		}
		t.Fatalf("listen on [::1]:0: %v", err)
	}
	_ = l.Close()
}

// fbSockoptInt reads an int socket option of l's socket.
func fbSockoptInt(t *testing.T, l net.Listener, level, opt int) (int, error) {
	t.Helper()
	tl, ok := l.(*net.TCPListener)
	require.True(t, ok, "%T", l)
	rc, err := tl.SyscallConn()
	require.NoError(t, err)
	var v int
	var gerr error
	require.NoError(t, rc.Control(func(fd uintptr) { v, gerr = unix.GetsockoptInt(int(fd), level, opt) }))
	return v, gerr
}

// serve's paired listen sets IP_FREEBIND, which on an AF_INET6 socket is the
// flag IPV6_FREEBIND reads too; a plain listen sets neither.
func TestFBPairedListen_SetsFreebind(t *testing.T) {
	fbNeedIPv6(t)
	l, err := pairedListen("tcp", "[::1]:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	v, err := fbSockoptInt(t, l, unix.SOL_IP, unix.IP_FREEBIND)
	require.NoError(t, err)
	require.Equal(t, 1, v, "IP_FREEBIND on serve's paired listener")
	if v6, err := fbSockoptInt(t, l, unix.SOL_IPV6, unix.IPV6_FREEBIND); err == nil {
		require.Equal(t, 1, v6, "IPV6_FREEBIND reads the same flag")
	}

	plain, err := net.Listen("tcp", "[::1]:0")
	require.NoError(t, err)
	defer func() { _ = plain.Close() }()
	v, err = fbSockoptInt(t, plain, unix.SOL_IP, unix.IP_FREEBIND)
	require.NoError(t, err)
	require.Zero(t, v, "a plain listen sets no IP_FREEBIND")
}

// Under peer_auth an address the interfaces list but a plain listen refuses
// (tentative, here one this machine does not have) is held by serve's paired
// listen, so no other process can take it first.
func TestFBListenPaired_HoldsAnAddressNotUpYet(t *testing.T) {
	fbNeedIPv6(t)
	ifaces := func() ([][]netip.Addr, error) {
		return [][]netip.Addr{addrs("10.255.255.1", "fd00:dead:beef::1")}, nil
	}
	ln4 := tcpListener("10.255.255.1:0")
	got, err := listenPaired(t.Context(), ln4, ifaces, net.Listen, pairedNeed{all: true})
	require.NoError(t, err)
	require.Empty(t, got, "a plain listen skips it")

	got, err = listenPaired(t.Context(), ln4, ifaces, pairedListen, pairedNeed{all: true})
	require.NoError(t, err)
	require.Equal(t, []string{"fd00:dead:beef::1"}, listenerHosts(got))
	for _, l := range got {
		_ = l.Close()
	}
}

// The race this closes: a process that bound the address first, with
// IP_FREEBIND as any process may, makes serve's paired listen fail with
// EADDRINUSE, which stops the start under peer_auth with nothing serve bound
// left open, and is skipped without it as before. Once the squatter is gone
// the same start holds the address.
func TestFBListenPaired_FreebindSquatterStopsTheStart(t *testing.T) {
	fbNeedIPv6(t)
	squat, err := pairedListen("tcp", "[fd00:dead:beef::7]:0")
	require.NoError(t, err)
	defer func() { _ = squat.Close() }()
	port := squat.Addr().(*net.TCPAddr).Port
	p := strconv.Itoa(port)
	ifaces := func() ([][]netip.Addr, error) {
		return [][]netip.Addr{addrs("10.255.255.1", "fd00:dead:beef::6", "fd00:dead:beef::7")}, nil
	}
	ln4 := tcpListener("10.255.255.1:" + p)

	got, err := listenPaired(t.Context(), ln4, ifaces, pairedListen, pairedNeed{all: true})
	require.Empty(t, got)
	var refusal *pairedListenError
	require.ErrorAs(t, err, &refusal)
	require.True(t, addrInUse(err), "the cause is the kernel's: %v", err)
	require.Contains(t, err.Error(), "[fd00:dead:beef::7]:"+p+" is in use by another process, and serve must listen there beside 10.255.255.1:"+p)
	require.Contains(t, err.Error(), "locked projects (peer_auth) are on")
	again, err := pairedListen("tcp", "[fd00:dead:beef::6]:"+p)
	require.NoError(t, err, "what serve bound before the refusal is closed")
	_ = again.Close()

	got, err = listenPaired(t.Context(), ln4, ifaces, pairedListen, pairedNeed{})
	require.NoError(t, err, "without peer_auth an address no name points at is skipped")
	require.Equal(t, []string{"fd00:dead:beef::6"}, listenerHosts(got))
	for _, l := range got {
		_ = l.Close()
	}

	require.NoError(t, squat.Close())
	got, err = listenPaired(t.Context(), ln4, ifaces, pairedListen, pairedNeed{all: true})
	require.NoError(t, err)
	require.Equal(t, []string{"fd00:dead:beef::6", "fd00:dead:beef::7"}, listenerHosts(got))
	for _, l := range got {
		_ = l.Close()
	}
}

// A socket that refuses IP_FREEBIND binds without it, with a warning: the
// listen goes on, as on other systems.
func TestFBFreebindControl_FallsBackWhereRefused(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer func() { _ = r.Close(); _ = w.Close() }()
	rc, err := r.SyscallConn()
	require.NoError(t, err)
	var cerr error
	logs := warnLines(t, func() { cerr = freebindControl("tcp6", "[fd00:dead:beef::1]:43117", rc) })
	require.NoError(t, cerr)
	require.Contains(t, logs, "cannot set IP_FREEBIND to listen on [fd00:dead:beef::1]:43117, so it listens without it")
	require.Contains(t, logs, "socket operation on non-socket")
}
