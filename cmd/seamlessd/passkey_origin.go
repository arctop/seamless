package main

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// originLookup resolves a passkey origin's host to the addresses a browser on
// this machine may dial. Injected so tests never touch DNS.
type originLookup func(ctx context.Context, host string) ([]netip.Addr, error)

// originLookupTimeout bounds one resolution: doctor and serve's warning both
// wait on it, and a name that does not answer in this long is one a browser
// gives up on too.
const originLookupTimeout = 3 * time.Second

// lookupOriginHost is the system resolver, A and AAAA both.
func lookupOriginHost(ctx context.Context, host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, originLookupTimeout)
	defer cancel()
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// hostNet is what the origin checks read about this machine's network: the
// names its resolver answers, and the addresses its interfaces hold.
type hostNet struct {
	lookup originLookup
	ifaces ifaceAddrs
}

// systemNet is this machine's resolver and interfaces.
var systemNet = hostNet{lookup: lookupOriginHost, ifaces: hostIfaceAddrs}

// holds reports whether one of this machine's interfaces holds a.
func (n hostNet) holds(a netip.Addr) bool {
	all, err := n.ifaces()
	if err != nil {
		return true // unknown: judged as the worse case, an address a local process can take
	}
	for _, list := range all {
		if slices.Contains(list, a.Unmap()) {
			return true
		}
	}
	return false
}

// passkeyOriginChecks is what doctor reports, and serve logs, about the
// relying party the approver passkeys use under peer_auth:
//
//   - the rp_id and origins in effect, and where they come from;
//   - for each origin, whether this daemon serves it: its scheme against TLS,
//     its port against bind, the certificate naming its host, and the daemon
//     listening on EVERY address the host resolves to (bind, and the IPv6
//     addresses serve pairs with an IPv4 one: listen_pair.go). An address of
//     the name this machine holds and the daemon does not is one another local
//     process can listen on and serve the exact origin from, so a browser that
//     picks it would run a passkey ceremony for that process;
//   - with a database, the passkeys a changed rp_id strands (each one works only
//     under the rp_id it was enrolled for), the ones enrolled before that was
//     recorded, and the ones another passkey's approval enrolled without a code.
//
// bind is the listener's address (cfg.Addr, or serve's -addr).
func passkeyOriginChecks(ctx context.Context, cfg config.Config, bind string, db *sql.DB, hn hostNet) []check {
	const name = "peer_auth.passkeys"
	rpID, origins := cfg.RelyingParty()
	source := "the localhost default"
	switch {
	case strings.TrimSpace(cfg.PeerAuth.Passkeys.RPID) != "":
		source = "peer_auth.passkeys"
	default:
		if _, _, derived := cfg.ServedRelyingParty(); derived {
			source = "derived from server_url " + cfg.ServerURL()
		}
	}
	out := []check{{statusOK, name, fmt.Sprintf("relying party %s (%s), origins %s: a passkey approves only under the rp_id "+
		"it was enrolled for", rpID, source, strings.Join(origins, ", "))}}
	for _, o := range origins {
		out = append(out, originChecks(ctx, cfg, bind, o, hn)...)
	}
	if db != nil {
		out = append(out, enrolledPasskeyChecks(ctx, db, rpID)...)
	}
	return out
}

// originChecks judges one origin against this daemon's listener.
func originChecks(ctx context.Context, cfg config.Config, bind, origin string, hn hostNet) []check {
	const name = "peer_auth.passkeys"
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" {
		return []check{{statusFail, name, fmt.Sprintf("origin %q is not scheme://host[:port]", origin)}}
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	bindPort := "" // an addr that is not host:port names no port to compare
	if _, p, err := net.SplitHostPort(bind); err == nil {
		bindPort = p
	}
	var out []check
	tlsOn := cfg.TLSEnabled()
	if (u.Scheme == "https") != tlsOn {
		served := "http"
		if tlsOn {
			served = "https"
		}
		out = append(out, check{statusWarn, name, fmt.Sprintf("origin %s is %s, but this daemon serves %s: a browser reaches the "+
			"passkey pages there only through a proxy that serves it", origin, u.Scheme, served)})
	}
	if bindPort != "" && bindPort != port {
		out = append(out, check{statusWarn, name, fmt.Sprintf("origin %s names port %s, but this daemon listens on port %s: "+
			"a browser reaches the passkey pages there only through a proxy that forwards it", origin, port, bindPort)})
	}
	if tlsOn && u.Scheme == "https" {
		if err := certCoversHost(cfg.TLS.CertFile, host); err != nil {
			out = append(out, check{statusWarn, name, fmt.Sprintf("origin %s: %v -- a browser refuses the origin, and every "+
				"passkey ceremony with it", origin, err)})
		}
	}
	if bindPort != "" && bindPort != port {
		return out // the listener is not where the origin points; whose addresses it holds says nothing more
	}

	localhost := host == "localhost" || strings.HasSuffix(host, ".localhost")
	var addrs []netip.Addr
	if localhost {
		// Browsers answer localhost themselves, with both loopback addresses.
		addrs = []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.IPv6Loopback()}
	} else {
		addrs, err = hn.lookup(ctx, host)
		if err != nil {
			return append(out, check{statusWarn, name, fmt.Sprintf("origin %s: %s does not resolve here (%v), so a browser on "+
				"this machine cannot reach it", origin, host, err)})
		}
	}
	held, err := boundAddrs(ctx, bind, hn)
	if err != nil {
		return append(out, check{statusWarn, name, fmt.Sprintf("origin %s: cannot tell which addresses this daemon "+
			"listens on (%v)", origin, err)})
	}
	// An address of the name the daemon does not hold is a local process's to
	// take only when this machine holds it; one it does not is another
	// machine's, or nobody's.
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
	case len(missing)+len(elsewhere) == 0:
		if len(out) == 0 {
			out = append(out, check{statusOK, name, fmt.Sprintf("origin %s: %s, every address of it held by this daemon on port %s",
				origin, joinAddrs(addrs), port)})
		}
	case localhost && len(missing)+len(elsewhere) == len(addrs):
		out = append(out, check{statusWarn, name, fmt.Sprintf("origin %s: this daemon does not listen on localhost (addr %s), so no "+
			"browser can open the passkey pages: serve TLS under a host name in server_url, which the relying party then "+
			"follows, or set peer_auth.passkeys to an origin the daemon serves", origin, bind)})
	case localhost && len(missing) > 0:
		out = append(out, check{statusInfo, name, fmt.Sprintf("origin %s: this daemon does not listen on %s port %s: a browser "+
			"that tries that address first could reach another local process listening there", origin, joinAddrs(missing), port)})
	case localhost:
		// A loopback address this machine lacks is one no process can take, and
		// a browser that tries it falls back to the other.
	default:
		if len(missing) > 0 {
			out = append(out, check{statusWarn, name, fmt.Sprintf("origin %s: %s resolves to %s, and this daemon does not listen on %s "+
				"(addr %s): another local process can listen there and serve the origin, so a browser that picks that address "+
				"would run its passkey ceremonies -- serve pairs an IPv4 addr only with its own interface's IPv6 addresses that "+
				"reach no further; bind the wildcard (every interface) or give the name only the addresses the daemon holds",
				origin, host, joinAddrs(addrs), joinAddrs(missing), bind)})
		}
		if len(elsewhere) > 0 {
			out = append(out, check{statusWarn, name, fmt.Sprintf("origin %s: %s resolves to %s, and this machine does not hold %s: "+
				"a browser that picks that address reaches another machine, or nothing -- give the name only this machine's "+
				"addresses", origin, host, joinAddrs(addrs), joinAddrs(elsewhere))})
		}
	}
	return out
}

// heldAddrs is what serve's listeners hold: every address (a wildcard bind),
// or the ones listed.
type heldAddrs struct {
	every bool
	addrs []netip.Addr
}

func (h heldAddrs) covers(a netip.Addr) bool {
	return h.every || slices.Contains(h.addrs, a.Unmap())
}

// boundAddrs reads what serve listens on for bind: a wildcard or empty host is
// every address (Go listens dual-stack); an IP is itself; a name is the address
// net.Listen resolves it to, the first IPv4 one if any. Beside an IPv4 address
// come the IPv6 ones serve pairs with it (pairedIPv6), as far as this
// machine's interfaces can be read.
func boundAddrs(ctx context.Context, bind string, hn hostNet) (heldAddrs, error) {
	host := bindHost(bind)
	if config.IsWildcardHost(host) {
		return heldAddrs{every: true}, nil
	}
	primary, err := netip.ParseAddr(host)
	if err != nil {
		addrs, lerr := hn.lookup(ctx, host)
		if lerr != nil {
			return heldAddrs{}, lerr
		}
		if len(addrs) == 0 {
			return heldAddrs{}, fmt.Errorf("%s resolves to nothing", host)
		}
		i := slices.IndexFunc(addrs, func(a netip.Addr) bool { return a.Unmap().Is4() })
		if i < 0 {
			i = 0
		}
		primary = addrs[i]
	}
	held := heldAddrs{addrs: []netip.Addr{primary.Unmap()}}
	if all, ierr := hn.ifaces(); ierr == nil {
		held.addrs = append(held.addrs, pairedIPv6(primary, all)...)
	}
	return held, nil
}

func joinAddrs(addrs []netip.Addr) string {
	s := make([]string, len(addrs))
	for i, a := range addrs {
		s[i] = a.Unmap().String()
	}
	return strings.Join(s, ", ")
}

// errCertNoName is a certificate that does not cover the origin's host.
var errCertNoName = errors.New("the TLS certificate does not name the host")

// certCoversHost reports whether the leaf certificate in certFile covers host.
func certCoversHost(certFile, host string) error {
	data, err := os.ReadFile(certFile)
	if err != nil {
		return fmt.Errorf("cannot read the TLS certificate to check it names %s: %w", host, err)
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return fmt.Errorf("no certificate in %s", certFile)
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("the TLS certificate %s does not parse: %w", certFile, err)
		}
		if leaf.VerifyHostname(host) != nil {
			return fmt.Errorf("%w %s: %s names %s", errCertNoName, host, certFile, strings.Join(leaf.DNSNames, ", "))
		}
		return nil
	}
}

// enrolledPasskeyChecks reports the enrolled passkeys this relying party
// cannot use, and the ones whose enrollment says less than a code does.
func enrolledPasskeyChecks(ctx context.Context, db *sql.DB, rpID string) []check {
	const name = "peer_auth.passkeys"
	creds, err := store.ApproverCredentials(ctx, db, store.ApproverPasskey)
	if err != nil {
		return []check{{statusFail, name, "enrolled passkeys: " + err.Error()}}
	}
	var out []check
	if stranded := store.StrandedPasskeys(creds, rpID); len(stranded) > 0 {
		names := make([]string, len(stranded))
		for i, c := range stranded {
			names[i] = fmt.Sprintf("%q (%s, enrolled for %s)", c.Name, c.ID, c.RPID)
		}
		out = append(out, check{statusWarn, name, fmt.Sprintf("passkeys %s cannot approve under rp_id %s: set peer_auth.passkeys.rp_id "+
			"and origins back to what they were enrolled for, or remove them (seamlessd approvers remove <id>, as the "+
			"daemon's account) and enroll again", strings.Join(names, ", "), rpID)})
	}
	unknown, byApproval := 0, []string{}
	for _, c := range creds {
		if c.RPID == "" {
			unknown++
		}
		if c.AddedBy != store.AddedByCode {
			byApproval = append(byApproval, fmt.Sprintf("%q (%s)", c.Name, c.ID))
		}
	}
	if unknown > 0 {
		out = append(out, check{statusInfo, name, fmt.Sprintf("%d passkey(s) enrolled before Seamless recorded the rp_id a "+
			"passkey belongs to: one that stops approving was enrolled for another rp_id", unknown)})
	}
	if len(byApproval) > 0 {
		out = append(out, check{statusWarn, name, fmt.Sprintf("passkeys %s were enrolled by another passkey's approval, with no enrollment "+
			"code; every enrollment now needs a code minted as the daemon's account. One approval tricked out of you was "+
			"enough to enroll a key someone else holds: remove any you do not recognize (seamlessd approvers remove <id>, "+
			"as the daemon's account)", strings.Join(byApproval, ", "))})
	}
	return out
}

// warnPasskeyOrigin logs serve's copy of the doctor rows: the relying party in
// effect, then every warning and failure about it and the passkeys it strands.
// The database half runs now, while serve's handle is open; the origin half
// resolves names, so it runs beside startup, never in front of it, and says
// nothing once serve is shutting down.
func warnPasskeyOrigin(ctx context.Context, cfg config.Config, bind string, db *sql.DB, logger *slog.Logger) {
	rpID, _ := cfg.RelyingParty()
	logPasskeyRows(logger, enrolledPasskeyChecks(ctx, db, rpID))
	go func() {
		rows := passkeyOriginChecks(ctx, cfg, bind, nil, systemNet)
		if ctx.Err() != nil {
			return
		}
		logger.Info("peer_auth: passkeys: " + rows[0].detail)
		logPasskeyRows(logger, rows[1:])
	}()
}

func logPasskeyRows(logger *slog.Logger, rows []check) {
	for _, c := range rows {
		if c.status == statusWarn || c.status == statusFail {
			logger.Warn("peer_auth: passkeys: "+c.detail, "check", c.name)
		}
	}
}
