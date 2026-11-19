//go:build linux

package main

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/arctop/seamless/internal/config"
)

// An address that appears while serve runs is bound the way the start binds,
// with IP_FREEBIND, so a tentative one is held too; and it holds the port
// there against a later bind of any process, IP_FREEBIND or not.
func TestPWLinuxLateAddressIsHeldWithFreebind(t *testing.T) {
	fbNeedIPv6(t)
	ln4, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln4.Close() }()
	port := strconv.Itoa(ln4.Addr().(*net.TCPAddr).Port)
	if l6, err := net.Listen("tcp", net.JoinHostPort("::1", port)); err != nil {
		t.Skipf("[::1]:%s is taken: %v", port, err)
	} else {
		_ = l6.Close()
	}
	ifs := &pwIfaces{}
	ifs.set([][]netip.Addr{addrs("127.0.0.1")}, nil)
	w := newPairWatch(ln4, ifs.read, pairedListen, pairedNeedFor(config.Defaults(), port))
	got, err := w.start(t.Context())
	require.NoError(t, err)
	require.Empty(t, got)

	ifs.set([][]netip.Addr{addrs("127.0.0.1", "::1")}, nil)
	tick := make(chan time.Time, 1)
	tick <- time.Now()
	var late []net.Listener
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, w.watch(ctx, tick, func(l net.Listener) { late = append(late, l) }, nil))
	require.NoError(t, ctx.Err(), "the watch ended once ::1 was held")
	require.Len(t, late, 1)
	defer func() { _ = late[0].Close() }()
	require.Equal(t, net.JoinHostPort("::1", port), late[0].Addr().String())
	v, err := fbSockoptInt(t, late[0], unix.SOL_IP, unix.IP_FREEBIND)
	require.NoError(t, err)
	require.Equal(t, 1, v, "IP_FREEBIND on the listener taken while running")

	_, err = pairedListen("tcp", net.JoinHostPort("::1", port))
	require.True(t, addrInUse(err), "another bind of the address is refused: %v", err)
}
