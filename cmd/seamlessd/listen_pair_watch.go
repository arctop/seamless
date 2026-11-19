package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// pairWatchInterval is how often a running serve reads this machine's
// interfaces again for an IPv6 address to listen on beside its bind. A read
// costs a fraction of a millisecond (about 0.25 ms on a laptop with twenty
// addresses), so every two seconds is nothing to the machine, and an address
// that appears (tailscaled's IPv6 a moment after its IPv4, a later router
// advertisement) waits at most that long. A poll, not a netlink or route
// socket watch: it needs nothing per system, and a process that races for the
// address wins against a watch too, so what bounds the harm is what serve
// does when it finds the address taken, not how soon it looks.
const pairWatchInterval = 2 * time.Second

// pairWatch is the IPv6 addresses serve listens on beside its IPv4 bind: the
// ones it holds, from the start (start, listenPaired) and those it takes
// while it runs (watch), and the failures it has logged for the rest.
//
// While serve runs, each read pairs by the start's rules (pairedIPv6), and a
// paired address it does not hold yet is listened on:
//
//   - one it now holds is served at once and admitted by the Host allowlist
//     (pairedHostSet) before its first request;
//   - one no process can hold now (addrUnheld: not up yet, tentative on
//     macOS) is tried again at the next read;
//   - one another process holds (addrTaken) that serve must hold (pairedNeed:
//     ::1, an address of a name browsers and clients dial, any under
//     peer_auth) stops serve with the start's refusal, so the daemon never
//     runs beside a process answering for it where the start would not have
//     let it; the service starts it again, and the start then waits for the
//     address as it always has;
//   - any other failure is logged once (again when it changes) and tried
//     again at the next read.
//
// A listener, once held, stays until serve stops, though its address leaves
// the interfaces: closing it would free the port there for another process the
// moment the address came back (tailscaled restarting), and a listener on an
// address the machine no longer has answers nothing. Its host stays admitted
// for the same reason.
type pairWatch struct {
	ln     net.Listener // the bind
	v4     netip.Addr
	port   uint16
	ifaces ifaceAddrs
	listen func(network, address string) (net.Listener, error)
	need   pairedNeed

	// The rest belongs to the one goroutine that runs start, then watch.
	held     []net.Listener
	heldAt   map[netip.Addr]bool
	warned   map[netip.Addr]string // the failure last logged for an address not held
	ifaceErr string                // the interface read's failure last logged while running
}

// newPairWatch is the watch beside ln, nil when nothing can pair with it: an
// IPv6, wildcard or link-local bind.
func newPairWatch(ln net.Listener, ifaces ifaceAddrs, listen func(network, address string) (net.Listener, error), need pairedNeed) *pairWatch {
	ap, err := netip.ParseAddrPort(ln.Addr().String())
	if err != nil {
		return nil
	}
	v4 := ap.Addr().Unmap()
	if !v4.Is4() || reachOf(v4) == reachNone {
		return nil
	}
	return &pairWatch{ln: ln, v4: v4, port: ap.Port(), ifaces: ifaces, listen: listen, need: need,
		heldAt: map[netip.Addr]bool{}, warned: map[netip.Addr]string{}}
}

// start is serve's first read, before it serves (listenPaired's rules).
func (w *pairWatch) start(ctx context.Context) ([]net.Listener, error) {
	return w.pass(ctx, false)
}

// watch reads the interfaces again at each tick until ctx ends, hands each
// listener it takes to serve (which must admit its host before it serves), and
// then calls changed, when set, with what serve's listeners hold now. The
// error is the reason serve stops (a *pairedListenError); nil once ctx ends,
// or once nothing more can pair (complete).
func (w *pairWatch) watch(ctx context.Context, tick <-chan time.Time, serve func(net.Listener), changed func(heldAddrs)) error {
	for !w.complete() {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
		}
		added, err := w.pass(ctx, true)
		if ctx.Err() != nil {
			// Stopping: a lookup cut short is no reason, and serve is not
			// serving what this read took.
			closeAll(added)
			return nil
		}
		if err != nil {
			return err
		}
		for _, l := range added {
			serve(l)
		}
		if len(added) > 0 && changed != nil {
			changed(w.heldAddrs())
		}
	}
	return nil
}

// watchPaired runs w.watch beside a running serve, reading every interval,
// until ctx ends: each listener it takes is admitted by hosts first, then
// served (serveOn); judge, when set, gets what serve holds after; the reason
// to stop, if any, goes to fail. The channel closes once the watch has ended.
func watchPaired(ctx context.Context, w *pairWatch, interval time.Duration, hosts *pairedHostSet, serveOn func(net.Listener),
	judge func(heldAddrs), fail func(error)) <-chan struct{} {
	done := make(chan struct{})
	ticker := time.NewTicker(interval)
	go func() {
		defer close(done)
		defer ticker.Stop()
		err := w.watch(ctx, ticker.C, func(l net.Listener) {
			hosts.add(l)
			serveOn(l)
		}, judge)
		if err != nil {
			fail(err)
		}
	}()
	return done
}

// complete reports whether no address can pair that serve does not hold: a
// loopback bind pairs with ::1 alone, so once that is held there is nothing
// to watch for.
func (w *pairWatch) complete() bool {
	return reachOf(w.v4) == reachLoopback && len(w.held) > 0
}

// heldAddrs is what serve's TCP listeners hold now: the bind and every
// address beside it (heldBy).
func (w *pairWatch) heldAddrs() heldAddrs {
	return heldBy(append([]net.Listener{w.ln}, w.held...))
}

// pass reads the interfaces once and listens on each paired address not held
// yet, returning the listeners it made. At the start (running false) a
// failure stops serve wherever another process may hold an address serve must
// hold, and an unreadable interface list does under peer_auth; running, only
// another process holding such an address does (addrTaken). On an error the
// listeners this pass made are closed.
func (w *pairWatch) pass(ctx context.Context, running bool) ([]net.Listener, error) {
	beside := w.ln.Addr().String()
	all, err := w.ifaces()
	if err != nil {
		switch {
		case !running && w.need.all:
			return nil, fmt.Errorf("cannot read this machine's interfaces to listen on the IPv6 addresses beside %s, which locked "+
				"projects (peer_auth) need held by this daemon: %w", beside, err)
		case !running:
			slog.Warn("seamlessd: cannot read this machine's interfaces, so it listens on no IPv6 address beside "+beside+
				" for now; it reads them again every "+pairWatchInterval.String(), "err", err)
		case err.Error() != w.ifaceErr:
			slog.Warn("seamlessd: cannot read this machine's interfaces again, so it takes no new IPv6 address beside "+beside+
				" for now; it reads them again every "+pairWatchInterval.String(), "err", err)
		}
		w.ifaceErr = err.Error()
		return nil, nil
	}
	w.ifaceErr = ""
	var added []net.Listener
	for _, a := range pairedIPv6(w.v4, all) {
		if w.heldAt[a] {
			continue
		}
		addr := netip.AddrPortFrom(a, w.port).String()
		l, err := w.listen("tcp", addr)
		if err == nil {
			added = append(added, l)
			w.held = append(w.held, l)
			w.heldAt[a] = true
			delete(w.warned, a)
			if running {
				slog.Info("seamlessd: listening on " + addr + " too, the IPv6 address beside " + beside + ": it was not this " +
					"daemon's when serve started")
			}
			continue
		}
		// A failure already logged was judged then; the next read tries it
		// again without a word.
		if w.warned[a] == err.Error() {
			continue
		}
		stops := !addrUnheld(err)
		if running {
			stops = addrTaken(err)
		}
		if stops {
			if must, why := w.need.must(ctx, a); must {
				w.drop(added)
				return nil, &pairedListenError{addr: addr, beside: beside, port: w.port, why: why, err: err, running: running}
			}
		}
		w.warned[a] = err.Error()
		slog.Warn("seamlessd: not listening on "+addr+", the IPv6 address beside "+beside+
			": a client that tries it first reaches whatever listens there instead, if anything does; serve tries it again every "+
			pairWatchInterval.String(), "err", err)
	}
	return added, nil
}

// drop closes ls, made by this pass, and forgets them.
func (w *pairWatch) drop(ls []net.Listener) {
	closeAll(ls)
	w.held = w.held[:len(w.held)-len(ls)]
	for _, l := range ls {
		if ap, err := netip.ParseAddrPort(l.Addr().String()); err == nil {
			delete(w.heldAt, ap.Addr())
		}
	}
}

func closeAll(ls []net.Listener) {
	for _, l := range ls {
		_ = l.Close()
	}
}

// pairedHostSet is the hosts of the listeners beside serve's bind, which the
// Host allowlist admits (daemonHandlerPaired): read by every request, added to
// by pairWatch while serve runs. A host is never taken out, as its listener
// never is (pairWatch).
type pairedHostSet struct {
	mu    sync.RWMutex
	hosts map[string]bool
}

func newPairedHostSet(ls []net.Listener) *pairedHostSet {
	s := &pairedHostSet{hosts: map[string]bool{}}
	for _, l := range ls {
		s.add(l)
	}
	return s
}

// add admits l's host, in the bare lowercase form requestHost reads.
func (s *pairedHostSet) add(l net.Listener) {
	for _, h := range listenerHosts([]net.Listener{l}) {
		s.mu.Lock()
		s.hosts[strings.ToLower(h)] = true
		s.mu.Unlock()
	}
}

func (s *pairedHostSet) has(host string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hosts[host]
}
