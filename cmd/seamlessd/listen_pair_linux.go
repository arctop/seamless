//go:build linux

package main

import (
	"context"
	"log/slog"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// pairedListen is how serve listens on an IPv6 address paired beside its bind
// (listenPaired). Linux binds it with IP_FREEBIND set. A tentative address
// (duplicate address detection still running, or its link without carrier)
// refuses a plain bind with EADDRNOTAVAIL, but not a bind with IP_FREEBIND,
// which any process may set: without it serve would skip the address, and a
// process that bound it first would answer there once the address is up. With
// it serve holds the address from the start, and a process that got there
// first makes this bind fail with EADDRINUSE, which stops the start where
// serve must hold the address (pairedNeed).
//
// IP_FREEBIND also lets a bind to an address this machine does not have
// succeed. serve binds only the addresses its interfaces list (pairedIPv6),
// so such a listener is on one that failed duplicate address detection, or
// was removed after the read: it answers nothing until the address is this
// machine's again, and then it holds it for this daemon.
func pairedListen(network, address string) (net.Listener, error) {
	lc := net.ListenConfig{Control: freebindControl}
	return lc.Listen(context.Background(), network, address)
}

// freebindControl sets IP_FREEBIND on a socket before its bind. IP_FREEBIND
// at SOL_IP, not IPV6_FREEBIND: on an AF_INET6 socket both set the same flag,
// and IPV6_FREEBIND needs Linux 4.15. A socket that refuses it binds without
// it, as on other systems: a tentative address is then refused and skipped,
// as it was before.
func freebindControl(_, address string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_FREEBIND, 1)
	}); err != nil {
		return err
	}
	if serr != nil {
		slog.Warn("seamlessd: cannot set IP_FREEBIND to listen on "+address+", so it listens without it: an address still "+
			"tentative there is refused and skipped", "err", serr)
	}
	return nil
}
