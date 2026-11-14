package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/config"
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
//
// So serve does not start without the ones that matter (review M1): an
// address it cannot listen on that another process may hold stops the start
// when it is ::1, when a name browsers or clients dial this daemon by
// resolves to it (server_url's host, the passkey origins'), and under
// peer_auth whatever it is (pairedNeed). The service manager starts serve
// again, and it runs once the address is free. One no process can hold now
// (not this machine's yet, or a family it lacks: addrUnheld) and, on an
// install without peer_auth, one no name points at are logged and skipped as
// before: nothing a browser or client is sent to answers there.

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
// listen on is logged and skipped when no process can hold it now (addrUnheld)
// or need does not require it: ln is the daemon's listener, and these close a
// gap beside it. One need requires is the error (a *pairedListenError), with
// every listener made here closed: another process holds it, or may take it,
// and would answer there for this daemon. Interfaces it cannot read pair
// nothing, which under peer_auth (need.all) is the error too.
func listenPaired(ctx context.Context, ln net.Listener, ifaces ifaceAddrs, listen func(network, address string) (net.Listener, error),
	need pairedNeed) ([]net.Listener, error) {
	ap, err := netip.ParseAddrPort(ln.Addr().String())
	if err != nil || !ap.Addr().Unmap().Is4() {
		return nil, nil
	}
	all, err := ifaces()
	if err != nil {
		if need.all {
			return nil, fmt.Errorf("cannot read this machine's interfaces to listen on the IPv6 addresses beside %s, which locked "+
				"projects (peer_auth) need held by this daemon: %w", ln.Addr(), err)
		}
		slog.Warn("seamlessd: cannot read this machine's interfaces, so it listens on no IPv6 address beside "+ln.Addr().String(),
			"err", err)
		return nil, nil
	}
	var out []net.Listener
	for _, a := range pairedIPv6(ap.Addr(), all) {
		addr := netip.AddrPortFrom(a, ap.Port()).String()
		l, err := listen("tcp", addr)
		if err == nil {
			out = append(out, l)
			continue
		}
		if !addrUnheld(err) {
			if must, why := need.must(ctx, a); must {
				for _, l := range out {
					_ = l.Close()
				}
				return nil, &pairedListenError{addr: addr, beside: ln.Addr().String(), port: ap.Port(), why: why, err: err}
			}
		}
		slog.Warn("seamlessd: not listening on "+addr+", the IPv6 address beside "+ln.Addr().String()+
			": a client that tries it first reaches whatever listens there instead, if anything does", "err", err)
	}
	return out, nil
}

// pairedNeed says which of the addresses serve pairs beside its bind it must
// hold to run (listenPaired). It is judged only for an address serve could
// not listen on, so resolving a name never delays a start that binds.
type pairedNeed struct {
	// all: every one of them -- locked projects (peer_auth) are on.
	all bool
	// hosts are the names browsers and clients are sent to this listener's
	// port by: server_url's host, and with passkeys on the origins' hosts.
	hosts []string
	// lookup resolves one of hosts; nil is the system resolver.
	lookup originLookup
}

// pairedNeedFor is serve's pairedNeed for cfg on the listener's port: a name
// that points at another port (a proxy forwards it) is not this listener's.
func pairedNeedFor(cfg config.Config, port string) pairedNeed {
	need := pairedNeed{all: cfg.PeerAuth.Enabled, lookup: lookupOriginHost}
	add := func(raw string) {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || originPort(u) != port {
			return
		}
		if h := strings.ToLower(u.Hostname()); !slices.Contains(need.hosts, h) {
			need.hosts = append(need.hosts, h)
		}
	}
	if strings.TrimSpace(cfg.AdvertisedURL) != "" && cfg.SocketPath() == "" {
		add(cfg.ServerURL())
	}
	if cfg.PeerAuth.Enabled || cfg.Console.PasskeySignIn() {
		_, origins := cfg.RelyingParty()
		for _, o := range origins {
			add(o)
		}
	}
	return need
}

// originPort is u's port, else its scheme's.
func originPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// must reports whether serve must hold a, and why, in the words of the
// refusal. A name that does not resolve now cannot be ruled out, so it is a
// reason too: the start waits for it, as it would for the address.
func (n pairedNeed) must(ctx context.Context, a netip.Addr) (bool, string) {
	a = a.WithZone("").Unmap()
	switch {
	case n.all:
		return true, "locked projects (peer_auth) are on, so every address it listens on must be this daemon's"
	case a.IsLoopback():
		return true, "localhost is both loopback addresses"
	}
	lookup := n.lookup
	if lookup == nil {
		lookup = lookupOriginHost
	}
	for _, h := range n.hosts {
		if h == "localhost" || strings.HasSuffix(h, ".localhost") {
			continue // its addresses are the loopback ones, judged above
		}
		if ip, err := netip.ParseAddr(h); err == nil {
			if ip.WithZone("").Unmap() == a {
				return true, h + " is a name browsers and clients dial this daemon by"
			}
			continue
		}
		addrs, err := lookup(ctx, h)
		if err != nil {
			return true, fmt.Sprintf("%s, a name browsers and clients dial this daemon by, does not resolve here now (%v), so it may "+
				"be one of its addresses", h, err)
		}
		if slices.ContainsFunc(addrs, func(x netip.Addr) bool { return x.WithZone("").Unmap() == a }) {
			return true, h + ", a name browsers and clients dial this daemon by, resolves to it"
		}
	}
	return false, ""
}

// pairedListenError is serve's refusal to run without an address it must
// hold beside its bind (listenPaired).
type pairedListenError struct {
	addr, beside string
	port         uint16
	why          string
	err          error
}

func (e *pairedListenError) Error() string {
	if addrInUse(e.err) {
		return fmt.Sprintf("%s is in use by another process, and serve must listen there beside %s (%s): a browser or client that "+
			"tries it first would reach that process instead of this daemon -- stop the process holding it (%s names it), or move "+
			"this daemon to another port (addr); the service starts serve again, and it runs once the address is free",
			e.addr, e.beside, e.why, listenHint(strconv.Itoa(int(e.port))))
	}
	return fmt.Sprintf("cannot listen on %s (%v), and serve must listen there beside %s (%s): until it does, another process may "+
		"take it and answer there instead of this daemon -- fix what refuses it, or move this daemon to another port (addr)",
		e.addr, e.err, e.beside, e.why)
}

func (e *pairedListenError) Unwrap() error { return e.err }

// listenHint is the command that names the process listening on port.
func listenHint(port string) string {
	if runtime.GOOS == "windows" {
		return "netstat -ano | findstr :" + port
	}
	return "sudo lsof -nP -iTCP:" + port + " -sTCP:LISTEN"
}

// heldBy is what serve's listeners hold: every address for a wildcard one,
// else each one's own.
func heldBy(lns []net.Listener) heldAddrs {
	var held heldAddrs
	for _, l := range lns {
		ap, err := netip.ParseAddrPort(l.Addr().String())
		if err != nil {
			continue
		}
		a := ap.Addr().WithZone("").Unmap()
		if a.IsUnspecified() {
			return heldAddrs{every: true}
		}
		held.addrs = append(held.addrs, a)
	}
	return held
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
// this machine's interfaces: doctor's copy of listenPaired's choice, the
// configuration the bind row names. Whether this daemon answers at each is
// the bind.addresses row's (doctor_net.go), so this one never dials.
func bindPaired(ctx context.Context, bind string, hn hostNet) []netip.Addr {
	held, err := boundAddrs(withListenerProbe(ctx, nil), bind, hn)
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
