package main

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
)

// Beside a concrete IPv4 addr, serve also listens on the IPv6 addresses of
// the interface that holds it, on the same port, where that interface has
// them -- but only those that reach no further than the IPv4 address does:
//
//   - 127.0.0.1 gets ::1, so localhost is this daemon whichever loopback
//     address a client tries first;
//   - a private or tailnet address (10/8, 172.16/12, 192.168/16, 100.64/10)
//     gets the interface's unique local addresses (fc00::/7), such as a
//     tailnet's fd7a:115c:a1e0::/48 one, never its global ones: a LAN bind
//     would otherwise widen to an address the internet may reach;
//   - a public address gets the interface's global and unique local ones.
//
// A dual-stack name resolves to both families, and an address of it the daemon
// does not hold is one another local process can listen on, so a client that
// tries it first reaches that process instead. A wildcard addr already holds
// every address; an IPv6, link-local or name-less bind gets nothing beside it.

// ifaceAddrs lists the addresses of each interface that is up, one slice per
// interface. Injected so tests never read this machine's interfaces.
type ifaceAddrs func() ([][]netip.Addr, error)

// hostIfaceAddrs is this machine's interfaces.
func hostIfaceAddrs() ([][]netip.Addr, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out [][]netip.Addr
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue // an interface that cannot be read holds nothing to pair with
		}
		var list []netip.Addr
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(n.IP); ok {
					list = append(list, ip.Unmap())
				}
			}
		}
		out = append(out, list)
	}
	return out, nil
}

// reach orders how far away an address can be reached from.
type reach int

const (
	reachNone     reach = iota // link-local, multicast, unspecified: nothing pairs with it
	reachLoopback              // this machine
	reachPrivate               // a private network, a tailnet
	reachPublic                // anywhere
)

// sharedAddressSpace is RFC 6598's 100.64.0.0/10: carrier-grade NAT, and the
// range every tailnet numbers its machines from. netip's IsPrivate leaves it
// out, and a tailnet address is as private as a LAN one.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

func reachOf(a netip.Addr) reach {
	a = a.Unmap()
	switch {
	case a.IsLoopback():
		return reachLoopback
	case !a.IsGlobalUnicast(): // link-local, multicast, unspecified, broadcast
		return reachNone
	case a.IsPrivate() || sharedAddressSpace.Contains(a):
		return reachPrivate
	default:
		return reachPublic
	}
}

// pairedIPv6 is the IPv6 addresses serve listens on beside v4: those of every
// interface holding v4 that reach no further than v4 does. Nothing for an
// address that is not IPv4, or that nothing pairs with.
func pairedIPv6(v4 netip.Addr, ifaces [][]netip.Addr) []netip.Addr {
	v4 = v4.Unmap()
	limit := reachOf(v4)
	if !v4.Is4() || limit == reachNone {
		return nil
	}
	var out []netip.Addr
	for _, list := range ifaces {
		if !slices.Contains(list, v4) {
			continue
		}
		for _, a := range list {
			if r := reachOf(a); a.Is6() && r != reachNone && r <= limit && !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
}

// listenPaired listens on the IPv6 addresses paired with the IPv4 address ln
// holds, on ln's port (its real one, so a ":0" bind pairs too). One it cannot
// listen on is logged and skipped: ln is the daemon's listener, and these only
// close a gap beside it.
func listenPaired(ln net.Listener, ifaces ifaceAddrs, listen func(network, address string) (net.Listener, error)) []net.Listener {
	ap, err := netip.ParseAddrPort(ln.Addr().String())
	if err != nil || !ap.Addr().Unmap().Is4() {
		return nil
	}
	all, err := ifaces()
	if err != nil {
		slog.Warn("seamlessd: cannot read this machine's interfaces, so it listens on no IPv6 address beside "+ln.Addr().String(),
			"err", err)
		return nil
	}
	var out []net.Listener
	for _, a := range pairedIPv6(ap.Addr(), all) {
		addr := netip.AddrPortFrom(a, ap.Port()).String()
		l, err := listen("tcp", addr)
		if err != nil {
			slog.Warn("seamlessd: not listening on "+addr+", the IPv6 address beside "+ln.Addr().String()+
				": a client that tries it first reaches whatever listens there instead, if anything does", "err", err)
			continue
		}
		out = append(out, l)
	}
	return out
}

// listenerHosts is the bare hosts of lns, the form the Host allowlist reads.
func listenerHosts(lns []net.Listener) []string {
	out := make([]string, 0, len(lns))
	for _, l := range lns {
		if ap, err := netip.ParseAddrPort(l.Addr().String()); err == nil {
			out = append(out, ap.Addr().String())
		}
	}
	return out
}

// bindPaired is the IPv6 addresses serve listens on beside bind, read from
// this machine's interfaces: doctor's copy of listenPaired's choice, which
// cannot see an address another process already holds.
func bindPaired(ctx context.Context, bind string, hn hostNet) []netip.Addr {
	held, err := boundAddrs(ctx, bind, hn)
	if err != nil || held.every || len(held.addrs) < 2 {
		return nil
	}
	return held.addrs[1:]
}

// pairedSuffix is how doctor names the addresses beside a bind: "" for none.
func pairedSuffix(port string, paired []netip.Addr) string {
	if len(paired) == 0 {
		return ""
	}
	s := make([]string, len(paired))
	for i, a := range paired {
		s[i] = net.JoinHostPort(a.String(), port)
	}
	return " (and " + strings.Join(s, ", ") + " beside it, on the same interface)"
}
