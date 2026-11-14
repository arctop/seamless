package main

// Doctor's rows about who can answer for this daemon's name: the facts setup's
// address step explains (setup_address.go), reported afterwards for every
// install that serves other computers.
//
//   - tls.key_file: who can read the TLS private key. Whoever can serves this
//     daemon's own name with its certificate, on any port. Under peer_auth the
//     preflight already judges the key (filesystemProblems, which the
//     peer_auth rows report), so this row is for a daemon that runs as the
//     owner, where every process of the owner's can read it by design.
//   - server_url.addresses: whether the daemon holds every address server_url's
//     host resolves to. An address of the name it does not hold is one another
//     local process can listen on and answer for the name from -- the bearer
//     key a client sends, a page on the console's origin. With passkeys on
//     (peer_auth, or passkey sign-in), the passkey rows judge the same host
//     the same way, and this row stays out of their way.
//   - tailscale: whether the tailnet issues certificates for that host
//     (tailscale cert). Then a valid certificate for the name is not the TLS
//     key's to withhold: the bind is what keeps another process off the
//     origin.

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"

	"github.com/arctop/seamless/internal/config"
)

// doctorTLSEnv is what these rows read of the machine: its network, its
// tailnet, its OS and its file modes. Injected so tests read no real machine.
type doctorTLSEnv struct {
	net       hostNet
	tailscale tailscaleCLI
	goos      string
	stat      func(string) (os.FileInfo, error)
}

// serverNameChecks is doctor's rows about who can answer for this daemon's
// name, nil on an install that serves no other computer.
func serverNameChecks(ctx context.Context, cfg config.Config, env doctorTLSEnv) []check {
	if cfg.IsClient() || !pairingOn(cfg) {
		return nil
	}
	var out []check
	if c, ok := tlsKeyCheck(cfg, env); ok {
		out = append(out, c)
	}
	if c, ok := serverURLAddressesCheck(ctx, cfg, env.net); ok {
		out = append(out, c)
	}
	if c, ok := tailnetCertCheck(ctx, cfg, env.tailscale); ok {
		out = append(out, c)
	}
	return out
}

// tlsKeyCheck reports who can read the TLS private key of a daemon that runs
// as the owner. Under peer_auth the preflight judges it instead.
func tlsKeyCheck(cfg config.Config, env doctorTLSEnv) (check, bool) {
	const name = "tls.key_file"
	if !cfg.TLSEnabled() || cfg.PeerAuth.Enabled || env.goos == "windows" {
		return check{}, false
	}
	key := cfg.TLS.KeyFile
	info, err := env.stat(key)
	if err != nil {
		return check{statusWarn, name, fmt.Sprintf("cannot read who may read %s: %v", key, err)}, true
	}
	host := cfg.ServerHost()
	switch perm := info.Mode().Perm(); {
	case perm&0o004 != 0:
		return check{statusWarn, name, fmt.Sprintf("%s is mode %04o: every account on this computer can read the TLS private key, "+
			"and with it serve %s with this daemon's certificate -- chmod 600 %s", key, perm, host, key)}, true
	case perm&0o040 != 0:
		return check{statusWarn, name, fmt.Sprintf("%s is mode %04o: the accounts of its group can read the TLS private key, and "+
			"with it serve %s with this daemon's certificate -- chmod 600 %s", key, perm, host, key)}, true
	}
	return check{statusInfo, name, fmt.Sprintf("%s is readable by this account alone, which the daemon runs as: every process of "+
		"this account can read it, and serve %s with this daemon's certificate on any port; locked projects (peer_auth) "+
		"put it where only the daemon's account reads it", key, host)}, true
}

// serverURLAddressesCheck judges every address server_url's host resolves to
// against what the daemon listens on (boundAddrs, which serve's pairing
// reads): held, or another local process's to take. A server_url derived
// from addr, a loopback one, a proxy's on another port, and a host the
// passkey rows already judge get no row.
func serverURLAddressesCheck(ctx context.Context, cfg config.Config, hn hostNet) (check, bool) {
	const name = "server_url.addresses"
	if strings.TrimSpace(cfg.AdvertisedURL) == "" || cfg.SocketPath() != "" {
		return check{}, false
	}
	u, err := url.Parse(cfg.ServerURL())
	if err != nil || u.Hostname() == "" {
		return check{}, false
	}
	host := strings.ToLower(u.Hostname())
	if isLoopbackServerHost(host) {
		return check{}, false
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	if _, bindPort, err := net.SplitHostPort(cfg.Addr); err != nil || bindPort != port {
		return check{}, false // a proxy forwards the name: whose addresses the listener holds says nothing about it
	}
	if passkeysJudge(cfg, host, port) {
		return check{}, false
	}
	var addrs []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		addrs = []netip.Addr{ip}
	} else if addrs, err = hn.lookup(ctx, host); err != nil || len(addrs) == 0 {
		why := "it resolves to no address"
		if err != nil {
			why = err.Error()
		}
		return check{statusWarn, name, fmt.Sprintf("%s does not resolve here (%s): a client here cannot reach this daemon by it", host, why)}, true
	}
	held, err := boundAddrs(ctx, cfg.Addr, hn)
	if err != nil {
		return check{statusWarn, name, fmt.Sprintf("cannot tell which addresses this daemon listens on (%v)", err)}, true
	}
	var missing, elsewhere []netip.Addr
	for _, a := range addrs {
		switch {
		case held.covers(a):
		case hn.holds(a):
			missing = append(missing, a)
		default:
			elsewhere = append(elsewhere, a)
		}
	}
	switch {
	case len(missing) > 0:
		return check{statusWarn, name, fmt.Sprintf("%s resolves to %s, and this daemon does not listen on %s (addr %s): another local "+
			"process can listen there and answer for %s, taking the key a client sends and serving the console's origin -- "+
			"bind the wildcard (every interface), or give the name only the addresses the daemon holds (seamlessd setup "+
			"picks the bind that holds them all)", host, joinAddrs(addrs), joinAddrs(missing), cfg.Addr, host)}, true
	case len(elsewhere) > 0:
		return check{statusWarn, name, fmt.Sprintf("%s resolves to %s, and this computer does not hold %s: a client that picks that "+
			"address reaches another computer, or nothing -- give the name only this computer's addresses",
			host, joinAddrs(addrs), joinAddrs(elsewhere))}, true
	}
	return check{statusOK, name, fmt.Sprintf("%s resolves to %s, every address of it held by this daemon on port %s",
		host, joinAddrs(addrs), port)}, true
}

// passkeysJudge reports whether the passkey rows judge host on port already:
// passkeys are on (peer_auth, or passkey sign-in, whose rows read
// passkeyOriginChecks) and one of their origins is that host and port.
func passkeysJudge(cfg config.Config, host, port string) bool {
	if !cfg.PeerAuth.Enabled && !cfg.Console.PasskeySignIn() {
		return false
	}
	_, origins := cfg.RelyingParty()
	for _, o := range origins {
		u, err := url.Parse(o)
		if err != nil || !strings.EqualFold(u.Hostname(), host) {
			continue
		}
		p := u.Port()
		if p == "" {
			p = "80"
			if u.Scheme == "https" {
				p = "443"
			}
		}
		if p == port {
			return true
		}
	}
	return false
}

// tailnetCertCheck says when the tailnet issues certificates for
// server_url's host: an account that may run tailscale cert can then serve
// the name with a valid certificate, whoever reads the daemon's own key.
func tailnetCertCheck(ctx context.Context, cfg config.Config, ts tailscaleCLI) (check, bool) {
	const name = "tailscale"
	if !cfg.TLSEnabled() || strings.TrimSpace(cfg.AdvertisedURL) == "" || ts == nil {
		return check{}, false
	}
	host := cfg.ServerHost()
	if _, err := netip.ParseAddr(host); err == nil || host == "" {
		return check{}, false
	}
	st, err := ts.Status(ctx)
	if err != nil || !st.issues(host) {
		return check{}, false
	}
	return check{statusInfo, name, fmt.Sprintf("the tailnet issues certificates for %s (tailscale cert): an account allowed to run it "+
		"here can serve %s with a valid certificate, so the TLS key is not what keeps its pages off the console's origin -- "+
		"this daemon holding every address of the name is (server_url.addresses, or the passkey rows)", host, host)}, true
}
