package main

// Doctor dials what it would otherwise infer (review M1). The addresses serve
// listens on beside an IPv4 bind (listen_pair.go) come from this machine's
// interfaces, and an interface listing an address says nothing about who
// listens on it: another process that held it when serve started answers
// there instead. So doctor's run carries a probe (withListenerProbe), and
// boundAddrs, which every row judging what serve holds reads (bind,
// server_url.addresses, the passkey rows of peer_auth and of console
// sign-in), keeps an address beside the bind only where the daemon answering
// at the bind answers too, by its /healthz instance id. bind.addresses says
// what the dial found.
//
// Only doctor dials. setup picks a bind for a daemon that has not started,
// and serve judges its origins against the listeners it holds (heldBy). A
// wildcard bind is not dialed: it holds the port on every address, and no
// other account can bind one of them while it does (EADDRINUSE).
//
// The instance id tells this daemon from another process, not from one that
// relays /healthz to it: serve's refusal to start without the addresses that
// matter (pairedNeed) is what keeps a process off them.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arctop/seamless/internal/config"
)

// listenerAnswer is what answers at one address of the listener: whether a
// process accepted the connection, and the instance id a Seamless daemon's
// /healthz named there ("" when it named none, or was not one).
type listenerAnswer struct {
	answered bool
	instance string
}

// listenerProbe dials one address of this daemon's listener.
type listenerProbe func(ctx context.Context, ap netip.AddrPort) listenerAnswer

type listenerProbeKey struct{}

// withListenerProbe is ctx carrying doctor's probe; nil takes one away.
func withListenerProbe(ctx context.Context, p listenerProbe) context.Context {
	return context.WithValue(ctx, listenerProbeKey{}, p)
}

// listenerProbeFrom is the probe ctx carries, nil when none does.
func listenerProbeFrom(ctx context.Context) listenerProbe {
	p, _ := ctx.Value(listenerProbeKey{}).(listenerProbe)
	return p
}

// doctorListenerProbe is doctor's probe for cfg's listener: GET /healthz at the
// address, over TLS when the daemon serves it (the certificate unverified: the
// answer's instance is what is read, and no credential is sent), each address
// dialed once per run.
func doctorListenerProbe(cfg config.Config, timeout time.Duration) listenerProbe {
	scheme := "http"
	if cfg.TLSEnabled() {
		scheme = "https"
	}
	var mu sync.Mutex
	seen := map[netip.AddrPort]listenerAnswer{}
	return func(ctx context.Context, ap netip.AddrPort) listenerAnswer {
		mu.Lock()
		defer mu.Unlock()
		if got, ok := seen[ap]; ok {
			return got
		}
		got := dialListener(ctx, scheme, cfg.Addr, ap, timeout)
		seen[ap] = got
		return got
	}
}

// dialListener asks ap for its /healthz, naming the bind in the Host header:
// serve's Host allowlist always admits it, an address serve did not pair
// only sometimes.
func dialListener(ctx context.Context, scheme, bind string, ap netip.AddrPort, timeout time.Duration) (got listenerAnswer) {
	var accepted atomic.Bool // set where the transport dials, read once Do returns
	defer func() { got.answered = accepted.Load() }()
	tr := reachabilityProbe().Transport.(*http.Transport)
	tr.DisableKeepAlives = true
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, network, addr)
		if err == nil {
			accepted.Store(true)
		}
		return c, err
	}
	client := &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: config.RefuseRedirect}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+ap.String()+"/healthz", nil)
	if err != nil {
		return got
	}
	req.Host = bind
	resp, err := client.Do(req)
	if err != nil {
		return got
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Instance string `json:"instance"`
	}
	if data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10)); err == nil && json.Unmarshal(data, &body) == nil {
		got.instance = body.Instance
	}
	return got
}

// confirmHeld dials the bind (held.addrs[0]) and each address beside it on
// port. The bind's answer names the daemon: an address beside it stays held
// where the same instance answers, is taken where another process answers,
// and silent where nothing does. With nothing answering at the bind (the
// daemon stopped), an address something answers at is taken, and one where
// nothing does stays: serve listens there when it starts, or refuses to.
// A daemon that names no instance (an older release) leaves the inference.
func confirmHeld(ctx context.Context, held heldAddrs, port uint16, probe listenerProbe) heldAddrs {
	out := heldAddrs{addrs: held.addrs[:1:1]}
	out.self = probe(ctx, netip.AddrPortFrom(held.addrs[0], port))
	for _, a := range held.addrs[1:] {
		got := probe(ctx, netip.AddrPortFrom(a, port))
		switch {
		case out.self.answered && out.self.instance == "":
			out.addrs = append(out.addrs, a)
		case out.self.answered && got.answered && got.instance == out.self.instance:
			out.addrs = append(out.addrs, a)
		case got.answered:
			out.taken = append(out.taken, a)
		case out.self.answered:
			out.silent = append(out.silent, a)
		default:
			out.addrs = append(out.addrs, a)
		}
	}
	return out
}

// bindAddressesCheck is the bind.addresses row: what answers at the bind and
// at each address serve listens on beside it, from doctor's dial. No row
// without the probe, for a wildcard bind or one with nothing beside it, or
// while nothing answers anywhere (the server_url row says the daemon is not
// answering).
func bindAddressesCheck(ctx context.Context, cfg config.Config, hn hostNet) (check, bool) {
	const name = "bind.addresses"
	if listenerProbeFrom(ctx) == nil {
		return check{}, false
	}
	_, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return check{}, false
	}
	held, err := boundAddrs(ctx, cfg.Addr, hn)
	paired := len(held.addrs) - 1 + len(held.taken) + len(held.silent)
	if err != nil || held.every || paired <= 0 {
		return check{}, false
	}
	at := func(addrs []netip.Addr) string {
		s := make([]string, len(addrs))
		for i, a := range addrs {
			s[i] = net.JoinHostPort(a.String(), port)
		}
		return strings.Join(s, ", ")
	}
	bind := at(held.addrs[:1])
	switch {
	case len(held.taken) > 0 && !held.self.answered:
		return check{statusFail, name, fmt.Sprintf("another process answers at %s, beside %s, where this daemon is not answering: a "+
			"browser or client that tries that address first reaches it, and serve refuses to start while it holds a loopback "+
			"address, one a name browsers and clients dial resolves to, or under peer_auth any -- stop it (%s names it)",
			at(held.taken), bind, listenHint(port))}, true
	case len(held.taken) > 0:
		return check{statusFail, name, fmt.Sprintf("another process answers at %s, not this daemon (instance %s at %s): a browser or "+
			"client that tries that address first reaches it instead -- stop it (%s names it), then restart the daemon so it "+
			"listens there", at(held.taken), held.self.instance, bind, listenHint(port))}, true
	case len(held.silent) > 0:
		return check{statusWarn, name, fmt.Sprintf("nothing answers at %s, beside %s where this daemon does: it could not listen "+
			"there when it started (its log says why), and another process can take the address -- restart the daemon",
			at(held.silent), bind)}, true
	case !held.self.answered:
		return check{}, false
	case held.self.instance == "":
		return check{statusInfo, name, fmt.Sprintf("the daemon at %s names no instance in /healthz (an older release), so doctor "+
			"cannot confirm it answers at %s too", bind, at(held.addrs[1:]))}, true
	}
	return check{statusOK, name, fmt.Sprintf("this daemon answers at %s and %s (instance %s)", bind, at(held.addrs[1:]),
		held.self.instance)}, true
}

// relaySocketEnv is what the client server_url row reads to tell who serves
// a socket here: the OS, and whether a file exists (the services' definitions).
type relaySocketEnv struct {
	goos   string
	exists func(path string) bool
}

// hostRelaySocketEnv is this computer's.
func hostRelaySocketEnv() relaySocketEnv {
	return relaySocketEnv{goos: runtime.GOOS, exists: func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}}
}

// relaySocketServer reports whether a socket server_url on this computer is
// a relay's, and whether the relay's service is installed. It is setup's
// reading (setupInstalledOf): a socket beside the peer_auth daemon's system
// service is that daemon's (the owner's half of a peer_auth install), and one
// without it a relay's -- installed when its LaunchDaemon or unit exists,
// else waiting for its root step. Windows has neither.
func relaySocketServer(env relaySocketEnv) (relay, installed bool) {
	var relaySvc, daemonSvc []string
	switch env.goos {
	case "darwin":
		relaySvc = []string{clientRelayPlistPath()}
		for _, label := range []string{systemDaemonLabel, legacySystemDaemonLabel} {
			daemonSvc = append(daemonSvc, systemDaemonPlistDir+"/"+label+".plist")
		}
	case "linux":
		for _, dir := range systemUnitDirs {
			relaySvc = append(relaySvc, dir+"/"+clientRelayUnit)
			daemonSvc = append(daemonSvc, dir+"/"+systemDaemonUnit)
		}
	default:
		return false, false
	}
	for _, p := range relaySvc {
		if env.exists(p) {
			return true, true
		}
	}
	for _, p := range daemonSvc {
		if env.exists(p) {
			return false, false
		}
	}
	return true, false
}

// relaySocketRepair is the client server_url row's repair for a relay's
// socket that does not answer: the relay's service, or its root step.
func relaySocketRepair(goos string, installed bool) string {
	start := "sudo systemctl restart " + strings.TrimSuffix(clientRelayUnit, ".service")
	svc := "the system unit " + clientRelayUnit
	if goos == "darwin" {
		start = "sudo launchctl kickstart -k system/" + clientRelayLabel
		svc = "the LaunchDaemon " + clientRelayLabel
	}
	if installed {
		return fmt.Sprintf("the relay serves it on this computer, as %s: start it (%s), and if it stops again, doctor run as the "+
			"relay's account says why (sudo -u %s -H /opt/seamless/bin/seamlessd doctor)", svc, start, clientRelayAccount)
	}
	return fmt.Sprintf("the relay serves it on this computer, and its root step has not run: seamlessd setup prints it, to run as "+
		"an administrator (%sguides/network-install/#the-relays-root-step); it installs the relay's service, %s", setupDocs, svc)
}
