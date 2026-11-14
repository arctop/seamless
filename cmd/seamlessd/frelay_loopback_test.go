package main

// A port for a serve a test starts, free on both loopback addresses. serve
// listens on [::1] beside 127.0.0.1, on the same port, and refuses to start
// when another process holds it there (listenPaired, review M1); the kernel
// hands out a 127.0.0.1 port with no regard for [::1] (on macOS one counter
// numbers both families' ports, and another socket on [::1] at the next
// number does not stop the IPv4 pick). The harnesses that start serve on a
// port of their choosing (pair_proof_test.go, relay_e2e_test.go,
// fenced_example_unix_test.go) take it from here. Portable, and built on
// every platform: addrInUse and addrUnheld read Winsock's codes on Windows.

import (
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// frelayPortTries bounds the picks frelayDualLoopbackPort makes.
const frelayPortTries = 20

// frelayDualLoopbackPort is a port free on 127.0.0.1 and [::1], both halves
// held until release, which the harness calls just before serve binds them
// itself (the test's end releases them too). A pick whose [::1] half another
// socket holds stays held until the search ends, so the kernel hands out
// another. Where this machine has no ::1 (an IPv4-only sandbox: addrUnheld),
// it is the IPv4 pick alone, which serve pairs with nothing either.
func frelayDualLoopbackPort(t testing.TB) (int, func()) {
	t.Helper()
	return frelayDualLoopbackPortWith(t, net.Listen)
}

// frelayDualLoopbackPortWith is frelayDualLoopbackPort over listen.
func frelayDualLoopbackPortWith(t testing.TB, listen func(network, address string) (net.Listener, error)) (int, func()) {
	t.Helper()
	var taken []net.Listener
	defer func() {
		for _, l := range taken {
			_ = l.Close()
		}
	}()
	for range frelayPortTries {
		ln4, err := listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := ln4.Addr().(*net.TCPAddr).Port
		ln6, err := listen("tcp", net.JoinHostPort("::1", strconv.Itoa(port)))
		switch {
		case err == nil:
			return port, frelayReleaseOnce(t, ln4, ln6)
		case addrUnheld(err):
			return port, frelayReleaseOnce(t, ln4)
		}
		taken = append(taken, ln4)
	}
	t.Fatalf("no port free on both loopback addresses in %d picks", frelayPortTries)
	return 0, nil
}

// frelayReleaseOnce closes lns once: when the harness releases the port for
// serve, or when the test ends first.
func frelayReleaseOnce(t testing.TB, lns ...net.Listener) func() {
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, l := range lns {
				_ = l.Close()
			}
		})
	}
	t.Cleanup(release)
	return release
}

// frelayHasIPv6Loopback reports whether this machine can listen on ::1.
func frelayHasIPv6Loopback() bool {
	return frelayListenErr("[::1]:0") == nil
}

// frelayListenErr is the error a listen on addr gives now, nil when it
// succeeds (the listener closed at once).
func frelayListenErr(addr string) error {
	l, err := net.Listen("tcp", addr)
	if err == nil {
		_ = l.Close()
	}
	return err
}

// The port holds both halves until it is released, and both are free for
// serve after.
func TestFrelayDualLoopbackPort_HoldsBothHalvesUntilReleased(t *testing.T) {
	port, release := frelayDualLoopbackPort(t)
	hosts := []string{"127.0.0.1"}
	if frelayHasIPv6Loopback() {
		hosts = append(hosts, "::1")
	}
	for _, h := range hosts {
		err := frelayListenErr(net.JoinHostPort(h, strconv.Itoa(port)))
		require.True(t, addrInUse(err), "%s is held until the harness releases it: %v", h, err)
	}
	release()
	release() // once is enough; the test's end calls it again
	for _, h := range hosts {
		require.NoError(t, frelayListenErr(net.JoinHostPort(h, strconv.Itoa(port))), "%s is serve's once released", h)
	}
}

// A 127.0.0.1 port whose [::1] half another socket holds is the one serve
// refuses to start on: the search passes it by, and lets it go when done.
func TestFrelayDualLoopbackPort_PassesByAPortWhoseIPv6HalfIsHeld(t *testing.T) {
	if !frelayHasIPv6Loopback() {
		t.Skip("no ::1 here")
	}
	// The squat: [::1]:Q held by another socket, 127.0.0.1:Q free.
	var squat net.Listener
	for range frelayPortTries {
		l, err := net.Listen("tcp", "[::1]:0")
		require.NoError(t, err)
		_, q, _ := net.SplitHostPort(l.Addr().String())
		if frelayListenErr(net.JoinHostPort("127.0.0.1", q)) == nil {
			squat = l
			break
		}
		_ = l.Close()
	}
	require.NotNil(t, squat, "no [::1] port with its 127.0.0.1 half free")
	t.Cleanup(func() { _ = squat.Close() })
	q := squat.Addr().(*net.TCPAddr).Port

	// The kernel's first IPv4 pick is Q, as it may be when another process
	// holds [::1]:Q alone.
	var asked []string
	first := true
	listen := func(network, address string) (net.Listener, error) {
		asked = append(asked, address)
		if address == "127.0.0.1:0" && first {
			first = false
			return net.Listen(network, net.JoinHostPort("127.0.0.1", strconv.Itoa(q)))
		}
		return net.Listen(network, address)
	}
	port, release := frelayDualLoopbackPortWith(t, listen)
	require.NotEqual(t, q, port, "a port serve would refuse to start on")
	require.Contains(t, asked, net.JoinHostPort("::1", strconv.Itoa(q)), "the [::1] half was tried")
	require.NoError(t, frelayListenErr(net.JoinHostPort("127.0.0.1", strconv.Itoa(q))), "the pick passed by is let go")
	require.True(t, addrInUse(frelayListenErr(net.JoinHostPort("::1", strconv.Itoa(port)))), "both halves of the port are held")
	release()
}

// Where this machine has no ::1, the IPv4 pick alone: serve skips ::1 there
// too (addrUnheld).
func TestFrelayDualLoopbackPort_IPv4AloneWithoutIPv6(t *testing.T) {
	notHere := netRealListenErr(t, "[fd00:dead:beef::1]:0")
	require.True(t, addrUnheld(notHere), "%v", notHere)
	var asked []string
	listen := func(network, address string) (net.Listener, error) {
		asked = append(asked, address)
		if address != "127.0.0.1:0" {
			return nil, notHere
		}
		return net.Listen(network, address)
	}
	port, release := frelayDualLoopbackPortWith(t, listen)
	require.Len(t, asked, 2, "one pick, its [::1] half tried once: %v", asked)
	require.True(t, addrInUse(frelayListenErr(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))), "the IPv4 half is held")
	release()
	require.NoError(t, frelayListenErr(net.JoinHostPort("127.0.0.1", strconv.Itoa(port))))
}
