package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// pwIfaces is an interface list a test changes while a watch reads it.
type pwIfaces struct {
	mu   sync.Mutex
	list [][]netip.Addr
	err  error
}

func (f *pwIfaces) set(list [][]netip.Addr, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list, f.err = list, err
}

func (f *pwIfaces) read() ([][]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list, f.err
}

// pwListen is a listen whose failures a test sets per address, and which
// records each attempt and each Close.
type pwListen struct {
	mu       sync.Mutex
	fails    map[string]error
	attempts []string
	closed   []string
}

func (f *pwListen) fail(addr string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails == nil {
		f.fails = map[string]error{}
	}
	if err == nil {
		delete(f.fails, addr)
		return
	}
	f.fails[addr] = err
}

func (f *pwListen) listen(_, address string) (net.Listener, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, address)
	if err := f.fails[address]; err != nil {
		return nil, err
	}
	return pwClosing{tcpListener(address), f}, nil
}

func (f *pwListen) tried() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.attempts...)
}

type pwClosing struct {
	net.Listener
	f *pwListen
}

func (l pwClosing) Close() error {
	l.f.mu.Lock()
	defer l.f.mu.Unlock()
	l.f.closed = append(l.f.closed, l.Addr().String())
	return nil
}

// pwRun runs w.watch on a tick channel the test drives, one read per pwTick.
type pwRun struct {
	tick    chan time.Time
	served  chan net.Listener
	changed chan heldAddrs
	done    chan error
}

func pwStart(ctx context.Context, w *pairWatch) *pwRun {
	r := &pwRun{tick: make(chan time.Time), served: make(chan net.Listener, 16), changed: make(chan heldAddrs, 16),
		done: make(chan error, 1)}
	go func() {
		r.done <- w.watch(ctx, r.tick, func(l net.Listener) { r.served <- l }, func(h heldAddrs) { r.changed <- h })
	}()
	return r
}

// pwTick lets the watch read once; it returns once the read before it ended.
func (r *pwRun) pwTick(t *testing.T) {
	t.Helper()
	select {
	case r.tick <- time.Now():
	case err := <-r.done:
		t.Fatalf("the watch ended: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not read")
	}
}

// pwSettle waits for the read a pwTick let go to end: the next tick is only
// taken once it has.
func (r *pwRun) pwSettle(t *testing.T) {
	t.Helper()
	r.pwTick(t)
}

func (r *pwRun) pwEnd(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not end")
		return nil
	}
}

func pwHosts(ls []net.Listener) []string { return listenerHosts(ls) }

// The task's case: tailscaled adds the node's IPv6 address a moment after its
// IPv4 one, so serve's start sees only the IPv4 address. The next read takes
// the IPv6 one, hands it to serve once, and says what serve now holds; a read
// after that takes nothing again.
func TestPairWatch_TakesAnAddressThatAppears(t *testing.T) {
	ifs := &pwIfaces{}
	ifs.set([][]netip.Addr{addrs("127.0.0.1", "::1"), addrs("100.64.0.7")}, nil)
	ls := &pwListen{}
	w := newPairWatch(tcpListener("100.64.0.7:43117"), ifs.read, ls.listen, pairedNeed{all: true})
	require.NotNil(t, w)
	got, err := w.start(t.Context())
	require.NoError(t, err)
	require.Empty(t, got, "nothing beside the tailnet address yet")

	ctx, cancel := context.WithCancel(t.Context())
	run := pwStart(ctx, w)
	run.pwTick(t)
	require.Empty(t, run.served, "the interfaces have not changed")

	ifs.set([][]netip.Addr{addrs("127.0.0.1", "::1"), addrs("100.64.0.7", "fd7a:115c:a1e0::7", "fe80::7")}, nil)
	run.pwTick(t)
	run.pwSettle(t)
	require.Len(t, run.served, 1)
	l := <-run.served
	require.Equal(t, "[fd7a:115c:a1e0::7]:43117", l.Addr().String(), "on the bind's port, never the link-local one")
	held := <-run.changed
	require.True(t, held.covers(netip.MustParseAddr("fd7a:115c:a1e0::7")))
	require.True(t, held.covers(netip.MustParseAddr("100.64.0.7")))
	require.Equal(t, []string{"[fd7a:115c:a1e0::7]:43117"}, ls.tried(), "held once, never listened on again")
	require.False(t, w.complete(), "a tailnet bind may gain another address")

	cancel()
	require.NoError(t, run.pwEnd(t))
	require.Empty(t, ls.closed, "a listener serve took is serve's to close at shutdown")
}

// A failure while running: one no process can hold now (tentative on macOS,
// not up yet) is tried at every read until it binds; one another process
// holds that serve need not hold is too; one of another kind does not stop
// serve even where serve must hold the address. Each failure is logged once.
func TestPairWatch_RetriesWhatItCannotHoldYet(t *testing.T) {
	notHere := netRealListenErr(t, "[fd00:dead:beef::1]:0")
	require.True(t, addrUnheld(notHere), "%v", notHere)
	inUse := pwInUse(t)
	refused := errors.New("bind: too many open files")

	for _, tc := range []struct {
		name string
		err  error
		need pairedNeed
	}{
		{"an address no process can hold now, under peer_auth", notHere, pairedNeed{all: true}},
		{"an address in use no name points at, without peer_auth", inUse, pairedNeed{}},
		{"a refusal that names no other process, under peer_auth", refused, pairedNeed{all: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ifs := &pwIfaces{}
			ifs.set([][]netip.Addr{addrs("100.64.0.7")}, nil)
			ls := &pwListen{}
			w := newPairWatch(tcpListener("100.64.0.7:43117"), ifs.read, ls.listen, tc.need)
			_, err := w.start(t.Context())
			require.NoError(t, err)
			ifs.set([][]netip.Addr{addrs("100.64.0.7", "fd7a:115c:a1e0::7")}, nil)
			ls.fail("[fd7a:115c:a1e0::7]:43117", tc.err)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var run *pwRun
			logs := warnLines(t, func() {
				run = pwStart(ctx, w)
				for range 3 {
					run.pwTick(t)
				}
				run.pwSettle(t)
			})
			require.Equal(t, 1, strings.Count(logs, "not listening on [fd7a:115c:a1e0::7]:43117"), "logged once:\n%s", logs)
			require.Contains(t, logs, "serve tries it again every 2s")
			require.Empty(t, run.served)

			ls.fail("[fd7a:115c:a1e0::7]:43117", nil)
			run.pwTick(t)
			run.pwSettle(t)
			require.Len(t, run.served, 1, "taken once it binds")
			// The read the last pwSettle of the logging let go may have run
			// before or after the failure was cleared: 4 or 5 tries, one per
			// read until it held.
			tries := len(ls.tried())
			require.GreaterOrEqual(t, tries, 4)
			require.LessOrEqual(t, tries, 5)
			run.pwTick(t)
			run.pwSettle(t)
			require.Len(t, ls.tried(), tries, "none once held")
			cancel()
			require.NoError(t, run.pwEnd(t))
		})
	}
}

// An address that appears while serve runs and that another process holds,
// where serve must hold it, stops serve with the start's refusal, said as a
// running serve's: under peer_auth any address, and without it one a name
// browsers and clients dial resolves to. What that read took is closed.
func TestPairWatch_ATakenAddressItMustHoldStopsServe(t *testing.T) {
	inUse := pwInUse(t)
	tailnet := fakeLookup(map[string][]string{"studio.example.ts.net": {"203.0.113.5", "fd00:5::5"}})
	for _, tc := range []struct {
		name string
		need pairedNeed
		why  string
	}{
		{"under peer_auth", pairedNeed{all: true}, "locked projects (peer_auth) are on, so every address it listens on must be this daemon's"},
		{"a name clients dial", pairedNeed{hosts: []string{"studio.example.ts.net"}, lookup: tailnet},
			"studio.example.ts.net, a name browsers and clients dial this daemon by, resolves to it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ifs := &pwIfaces{}
			ifs.set([][]netip.Addr{addrs("203.0.113.5")}, nil)
			ls := &pwListen{}
			w := newPairWatch(tcpListener("203.0.113.5:43117"), ifs.read, ls.listen, tc.need)
			_, err := w.start(t.Context())
			require.NoError(t, err)
			ifs.set([][]netip.Addr{addrs("203.0.113.5", "2001:db8:5::5", "fd00:5::5")}, nil)
			ls.fail("[fd00:5::5]:43117", inUse)

			run := pwStart(t.Context(), w)
			run.pwTick(t)
			err = run.pwEnd(t)
			var refusal *pairedListenError
			require.ErrorAs(t, err, &refusal)
			require.True(t, refusal.running)
			require.Contains(t, err.Error(), "[fd00:5::5]:43117 is in use by another process, and serve must listen there beside "+
				"203.0.113.5:43117 ("+tc.why+"): serve did not hold it when it started")
			require.Contains(t, err.Error(), "so serve stops")
			require.Contains(t, err.Error(), "the service starts serve again, and it runs once the address is free")
			require.Empty(t, run.served, "nothing of that read is served")
			require.Equal(t, []string{"[2001:db8:5::5]:43117"}, ls.closed, "what that read took is closed")
		})
	}
}

// pwInUse is this machine's kernel's error for a port another socket holds.
func pwInUse(t *testing.T) error {
	t.Helper()
	holder, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = holder.Close() }()
	err = netRealListenErr(t, holder.Addr().String())
	require.True(t, addrTaken(err), "%v", err)
	return err
}

// Interfaces that cannot be read while serve runs stop nothing, even under
// peer_auth (the start's refusal is the start's): the watch logs it once and
// reads again.
func TestPairWatch_UnreadableInterfacesWhileRunning(t *testing.T) {
	ifs := &pwIfaces{}
	ifs.set([][]netip.Addr{addrs("100.64.0.7")}, nil)
	ls := &pwListen{}
	w := newPairWatch(tcpListener("100.64.0.7:43117"), ifs.read, ls.listen, pairedNeed{all: true})
	_, err := w.start(t.Context())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ifs.set(nil, errors.New("route socket gone"))
	var run *pwRun
	logs := warnLines(t, func() {
		run = pwStart(ctx, w)
		run.pwTick(t)
		run.pwTick(t)
		run.pwSettle(t)
	})
	require.Equal(t, 1, strings.Count(logs, "cannot read this machine's interfaces again"), logs)
	ifs.set([][]netip.Addr{addrs("100.64.0.7", "fd7a:115c:a1e0::7")}, nil)
	run.pwTick(t)
	run.pwSettle(t)
	require.Len(t, run.served, 1)
	cancel()
	require.NoError(t, run.pwEnd(t))
}

// A loopback bind pairs with ::1 alone: once serve holds it the watch ends at
// once, so the default install reads nothing more. Without ::1 it keeps
// reading.
func TestPairWatch_LoopbackEndsOnceHeld(t *testing.T) {
	ls := &pwListen{}
	w := newPairWatch(tcpListener("127.0.0.1:43117"), func() ([][]netip.Addr, error) {
		return [][]netip.Addr{addrs("127.0.0.1", "::1")}, nil
	}, ls.listen, pairedNeed{})
	got, err := w.start(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"::1"}, pwHosts(got))
	require.True(t, w.complete())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, w.watch(ctx, nil, func(net.Listener) { t.Fatal("nothing to serve") }, nil))
	require.NoError(t, ctx.Err(), "the watch ended by itself, not at the deadline")

	w = newPairWatch(tcpListener("127.0.0.1:43117"), func() ([][]netip.Addr, error) {
		return [][]netip.Addr{addrs("127.0.0.1")}, nil
	}, ls.listen, pairedNeed{})
	_, err = w.start(t.Context())
	require.NoError(t, err)
	require.False(t, w.complete(), "::1 may come")
}

// Nothing pairs beside a wildcard, IPv6 or link-local bind, so there is no
// watch, and no interface read: a wildcard bind under peer_auth no longer
// fails its start on interfaces it cannot read, since it already holds every
// address.
func TestPairWatch_NothingBesideABindThatCannotPair(t *testing.T) {
	broken := func() ([][]netip.Addr, error) { return nil, errors.New("no interfaces") }
	for _, bind := range []string{"0.0.0.0:43117", "[::]:43117", "[fd7a:115c:a1e0::7]:43117", "169.254.1.1:43117"} {
		require.Nil(t, newPairWatch(tcpListener(bind), broken, net.Listen, pairedNeed{all: true}), bind)
		got, err := listenPaired(t.Context(), tcpListener(bind), broken, net.Listen, pairedNeed{all: true})
		require.NoError(t, err, bind)
		require.Empty(t, got, bind)
	}
}

// A read cut short by serve stopping returns no reason, and closes what it
// took instead of handing it to serve.
func TestPairWatch_StoppingClosesWhatTheLastReadTook(t *testing.T) {
	ifs := &pwIfaces{}
	ifs.set([][]netip.Addr{addrs("100.64.0.7")}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	ls := &pwListen{}
	listen := func(network, address string) (net.Listener, error) {
		cancel() // serve stops while this listen runs
		return ls.listen(network, address)
	}
	w := newPairWatch(tcpListener("100.64.0.7:43117"), ifs.read, listen, pairedNeed{all: true})
	_, err := w.start(t.Context())
	require.NoError(t, err)
	ifs.set([][]netip.Addr{addrs("100.64.0.7", "fd7a:115c:a1e0::7")}, nil)
	tick := make(chan time.Time, 1)
	tick <- time.Now()
	require.NoError(t, w.watch(ctx, tick, func(net.Listener) { t.Fatal("served while stopping") }, nil))
	require.Equal(t, []string{"[fd7a:115c:a1e0::7]:43117"}, ls.closed)
}

// The Host allowlist admits a paired address from the moment the watch adds
// it, and still refuses a rebound name.
func TestDaemonHandlerPaired_AdmitsWhatTheWatchAdds(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	hosts := newPairedHostSet([]net.Listener{tcpListener("[fd00:1::9]:8081")})
	h := daemonHandlerPaired("192.168.1.9:8081", nil, hosts, newActivityTracker(time.Now), ok)
	code := func(host string) int {
		r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		r.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	require.Equal(t, http.StatusNoContent, code("192.168.1.9:8081"), "the bind")
	require.Equal(t, http.StatusNoContent, code("[fd00:1::9]:8081"), "an address held from the start")
	require.Equal(t, http.StatusNoContent, code("localhost:8081"))
	require.Equal(t, http.StatusMisdirectedRequest, code("[FD7A:115C:A1E0::7]:8081"), "not held yet")
	hosts.add(tcpListener("[fd7a:115c:a1e0::7]:8081"))
	require.Equal(t, http.StatusNoContent, code("[FD7A:115C:A1E0::7]:8081"), "held now, in any case")
	require.Equal(t, http.StatusMisdirectedRequest, code("evil.example.com:8081"))

	plain := daemonHandler("192.168.1.9:8081", nil, newActivityTracker(time.Now), ok)
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.Host = "[fd00:1::9]:8081"
	rec := httptest.NewRecorder()
	plain.ServeHTTP(rec, r)
	require.Equal(t, http.StatusMisdirectedRequest, rec.Code, "daemonHandler admits no paired host")
}

// The pair really serves when it comes late: a loopback bind whose ::1 appears
// after the start is listened on by serve's own listen, served by the running
// server, and closed by its Shutdown.
func TestPairWatch_LateLoopbackServes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	if l6, err := net.Listen("tcp", net.JoinHostPort("::1", port)); err != nil {
		t.Skipf("no ::1 here, or its port is taken: %v", err)
	} else {
		_ = l6.Close()
	}
	ifs := &pwIfaces{}
	ifs.set([][]netip.Addr{addrs("127.0.0.1")}, nil)
	w := newPairWatch(ln, ifs.read, pairedListen, pairedNeedFor(config.Defaults(), port))
	paired, err := w.start(t.Context())
	require.NoError(t, err)
	require.Empty(t, paired)

	hosts := newPairedHostSet(paired)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := newHTTPServer(t.Context(), ln.Addr().String(), daemonHandlerPaired(ln.Addr().String(), nil, hosts,
		newActivityTracker(time.Now), ok))
	var served sync.WaitGroup
	var admitted []bool // whether each listener's host was admitted before it was served
	serveOn := func(l net.Listener) {
		admitted = append(admitted, hosts.has(listenerHosts([]net.Listener{l})[0]))
		served.Go(func() { _ = srv.Serve(l) })
	}
	serveOn(ln)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var held []heldAddrs
	fail := func(err error) { t.Errorf("serve would stop: %v", err) }
	ifs.set([][]netip.Addr{addrs("127.0.0.1", "::1")}, nil)
	done := watchPaired(ctx, w, 10*time.Millisecond, hosts, serveOn, func(h heldAddrs) { held = append(held, h) }, fail)
	select {
	case <-done: // a loopback bind's watch ends once ::1 is held
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not take ::1")
	}
	require.Equal(t, []bool{false, true}, admitted, "::1 admitted before it was served (the bind is the guard's own)")
	require.Len(t, held, 1)
	require.True(t, held[0].covers(netip.IPv6Loopback()))

	at := "http://" + net.JoinHostPort("::1", port) + "/"
	resp, err := http.Get(at)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.NoError(t, srv.Shutdown(context.Background()))
	served.Wait()
	_, err = net.DialTimeout("tcp", net.JoinHostPort("::1", port), time.Second)
	require.Error(t, err, "Shutdown closed the listener the watch added")
}

// Another process on ::1 beside a loopback bind, which appeared after the
// start: the running serve stops, with the kernel's in-use as the cause.
func TestPairWatch_LateLoopbackSquatterStopsServe(t *testing.T) {
	ln4, _ := netDualLoopback(t) // the second is the other process, on ::1
	port := strconv.Itoa(ln4.Addr().(*net.TCPAddr).Port)
	ifs := &pwIfaces{}
	ifs.set([][]netip.Addr{addrs("127.0.0.1")}, nil)
	w := newPairWatch(ln4, ifs.read, pairedListen, pairedNeedFor(config.Defaults(), port))
	_, err := w.start(t.Context())
	require.NoError(t, err)
	ifs.set([][]netip.Addr{addrs("127.0.0.1", "::1")}, nil)
	tick := make(chan time.Time, 1)
	tick <- time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second) // a watch that does not stop fails, not hangs
	defer cancel()
	err = w.watch(ctx, tick, func(net.Listener) { t.Fatal("nothing to serve") }, nil)
	var refusal *pairedListenError
	require.ErrorAs(t, err, &refusal)
	require.True(t, addrTaken(err), "the cause is the kernel's: %v", err)
	require.Contains(t, err.Error(), net.JoinHostPort("::1", port)+" is in use by another process")
	require.Contains(t, err.Error(), "localhost is both loopback addresses")
}
