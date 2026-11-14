package main

// setup's address step (plan networked-relay): where the other computers
// reach the Server. It decides the name, the TLS certificate for that name,
// and the bind that holds every address the name resolves to, and writes them
// into a first install's config as addr, server_url, allowed_hosts and tls.
//
// The name. Tailscale first: this computer's MagicDNS name, when the tailnet
// runs here and issues certificates for that name (tailscale cert). Else a
// domain of the owner's, with its certificate and key files. Else a name on
// the LAN, with the commands that make a certificate for it (mkcert or
// openssl, as the network-install guide makes one). A script answers with
// SEAMLESS_SERVER_NAME, and with SEAMLESS_TLS_CERT and SEAMLESS_TLS_KEY unless
// the name is this computer's tailnet name.
//
// The bind. An address of the name the daemon does not hold is one another
// local process can listen on and serve the name's exact origin from (task
// 01M73MX5T07BC741Z6DXM6HHCP's addendum), so setup resolves the name, A and
// AAAA, and picks the narrowest addr that holds every address, judged by the
// function doctor judges the listener with (boundAddrs): one IPv4 address of
// the name, with the IPv6 addresses serve listens on beside it
// (listen_pair.go), else the wildcard. serve binds one addr, and pairing an
// IPv4 bind with its own interface's IPv6 addresses already holds a tailnet
// name's two; a name whose addresses that cannot hold gets the wildcard, as
// the peer-auth guide says, rather than serve growing a list of binds. A name
// that resolves to nothing, to this computer's loopback, or to an address
// this computer does not hold is refused.
//
// The config. A first install has none yet, and setup writes it once every
// step of its first half has run (setupWriteServer): after the import, so an
// import that stops leaves no config behind to make the next run a re-run. A
// config that exists is the owner's, never edited (config.EnsureServerConfig):
// setup prints the lines instead. With locked projects (peer_auth) on, the
// daemon's config and its TLS pair are its own account's, and the key must be
// readable by that account alone; setup runs no root step, so it prints them.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
)

// The guide's peer_auth layout (docs-src/guides/peer-auth.md, "Deploy it"):
// the daemon's config, and the directory its TLS pair lives in.
const (
	setupPeerAuthConfig = "/opt/seamless/etc/seamless.yaml"
	setupPeerAuthTLSDir = "/opt/seamless/etc/tls"
)

// setupAddrSource is where the Server's certificate comes from.
type setupAddrSource int

const (
	// setupAddrFiles: a certificate and key the owner has -- a domain's, or
	// one made for a name on the LAN.
	setupAddrFiles setupAddrSource = iota
	// setupAddrTailscale: tailscale cert, for this computer's tailnet name.
	setupAddrTailscale
)

// setupAddrPlan is what the address step decided.
type setupAddrPlan struct {
	name, port string
	source     setupAddrSource
	// cert and key are the TLS pair's paths as the owner's account reads
	// them (under peer_auth: where they come from, not where they go).
	cert, key string
	// leaf is the certificate, nil when setup has none to read yet (a tailnet
	// certificate root mints under peer_auth).
	leaf *x509.Certificate
	// addrs are what name resolves to on this computer; bind holds them all,
	// with paired beside a concrete IPv4 bind, or wildcard.
	addrs    []netip.Addr
	bind     string
	paired   []netip.Addr
	wildcard bool
	// issues: the tailnet issues certificates for name (tailscale cert);
	// minted: this account just got one.
	issues, minted bool
}

func (p setupAddrPlan) serverURL() string {
	if p.port == "443" {
		return "https://" + p.name
	}
	return "https://" + net.JoinHostPort(p.name, p.port)
}

// address is the plan as config keys, with the TLS pair at certFile and keyFile.
func (p setupAddrPlan) address(certFile, keyFile string) config.ServerAddress {
	return config.ServerAddress{Addr: p.bind, ServerURL: p.serverURL(), CertFile: certFile, KeyFile: keyFile}
}

// setupPeerAuthPair is the peer_auth layout's TLS pair for name.
func setupPeerAuthPair(name string) (string, string) {
	return setupPeerAuthTLSDir + "/" + name + ".crt", setupPeerAuthTLSDir + "/" + name + ".key"
}

// setupAddressKnobs refuses SEAMLESS_SERVER_NAME, SEAMLESS_TLS_CERT and
// SEAMLESS_TLS_KEY where setup cannot do what they ask, before anything is
// written: on Windows, on a run whose steps have no address step, half a
// certificate, a certificate with no name, and a name or a path setup cannot
// take as one. Whether the name resolves, and the pair is one for it, is the
// step's to check.
func setupAddressKnobs(r *setupRun, st setupState) error {
	name, cert, key := r.env(setupKnobServerName), r.env(setupKnobTLSCert), r.env(setupKnobTLSKey)
	var set string
	for _, k := range []struct{ knob, v string }{{setupKnobServerName, name}, {setupKnobTLSCert, cert}, {setupKnobTLSKey, key}} {
		if k.v != "" && set == "" {
			set = k.knob
		}
	}
	if set == "" {
		return nil
	}
	if r.goos == "windows" {
		return setupRefusal("%s: "+setupWindowsWhy+"; set up the {server} on a macOS or Linux computer", set)
	}
	if !slices.ContainsFunc(setupStepsLeft(st, r.goos), func(s setupStep) bool { return s.id == "address" }) {
		why := setupTextf("set %s=%s too, to make this computer the {server}", setupKnobKind, setupWords[setupServer].env)
		switch st.kind {
		case setupClient:
			why = setupTextf("a {client} has no address of its own (its {server}'s is %s)", setupKnobServer)
		case setupServer:
			why = setupTextf("this computer is the {server} already, and setup does not change its address: edit addr, server_url and tls in %s "+
				"by hand (%sguides/network-install/#step-1-widen-the-server)", tildePath(r.facts.configPath), setupDocs)
		}
		return setupRefusal("%s answers the {server}'s address, which this run does not set up: %s", set, why)
	}
	switch {
	case (cert == "") != (key == ""):
		return setupRefusal("%s and %s are a certificate and its key: set both, or neither (for this computer's Tailscale name "+
			"setup gets both from the tailnet)", setupKnobTLSCert, setupKnobTLSKey)
	case name == "":
		return setupRefusal("%s and %s are the certificate of the name %s gives: set it too", setupKnobTLSCert, setupKnobTLSKey, setupKnobServerName)
	}
	if p := setupServerNameProblem(name); p != "" {
		return setupRefusal("%s=%s: %s", setupKnobServerName, name, p)
	}
	for _, f := range []struct{ knob, v string }{{setupKnobTLSCert, cert}, {setupKnobTLSKey, key}} {
		if f.v == "" {
			continue
		}
		if _, p := setupAddrFilePath(r, f.v); p != "" {
			return setupRefusal("%s=%s: %s", f.knob, f.v, p)
		}
	}
	return nil
}

// setupNormalName is a name as setup takes it: lower-case, without the final
// dot a fully qualified name may carry.
func setupNormalName(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// setupServerNameProblem says what is wrong with a Server name, "" when
// nothing is: a host name the passkeys can take as their relying party
// (config.ServedRelyingParty, the one place that rule lives).
func setupServerNameProblem(raw string) string {
	n := setupNormalName(raw)
	switch {
	case n == "":
		return "no name"
	case strings.Contains(n, "://") || strings.ContainsAny(n, "/?#@"):
		return "a name only, like seamless.example.com: no scheme and no path"
	case strings.ContainsFunc(n, setupControl) || strings.ContainsAny(n, " \t"):
		return "the name holds a space or a control character"
	}
	if _, err := netip.ParseAddr(strings.Trim(n, "[]")); err == nil {
		return "an IP address: give the name it answers to -- a certificate and the passkeys need a name"
	}
	switch {
	case strings.Contains(n, ":"):
		return "a name only, without a port"
	case n == "localhost" || strings.HasSuffix(n, ".localhost"):
		return "localhost names this computer only: give the name the other computers dial"
	}
	cfg := config.Defaults()
	cfg.AdvertisedURL = "https://" + net.JoinHostPort(n, "8081")
	cfg.TLS.CertFile, cfg.TLS.KeyFile = "cert", "key"
	if _, _, ok := cfg.ServedRelyingParty(); !ok || cfg.Validate() != nil {
		return "not a host name (letters, digits, dots and dashes)"
	}
	return ""
}

// setupAddrFilePath resolves a certificate's or a key's path as setup takes
// one: ~ is the home, and anything else must be a full path, since setup runs
// in a directory of its own. It returns the path, or what is wrong with it.
func setupAddrFilePath(r *setupRun, raw string) (string, string) {
	p := raw
	if strings.ContainsFunc(p, setupControl) {
		return "", "the path holds a control character"
	}
	if rest, ok := setupHomeRelative(r.goos, p); ok {
		if r.home == "" {
			return "", "there is no home directory to read ~ from; give the file's full path"
		}
		p = filepath.Join(r.home, rest)
	}
	if !setupFullPath(r.goos, p) {
		return "", "not a full path; give the file's full path (from / or ~/)"
	}
	return filepath.Clean(p), ""
}

// setupAddrPort is the port a Server keeps: its config's addr's, else the
// default install's.
func setupAddrPort(addr string) string {
	for _, a := range []string{addr, config.Defaults().Addr} {
		if _, port, err := net.SplitHostPort(a); err == nil && port != "" && port != "0" {
			return port
		}
	}
	return "8081"
}

// setupTLSDir is where setup keeps a certificate the tailnet issues: beside
// the config the installers manage (~/.config/seamless/tls).
func setupTLSDir(r *setupRun) (string, error) {
	switch {
	case r.facts.configPath != "":
		return filepath.Join(filepath.Dir(r.facts.configPath), "tls"), nil
	case r.home != "":
		return filepath.Join(r.home, ".config", "seamless", "tls"), nil
	}
	return "", errors.New("no home directory to keep the certificate in")
}

// setupAddressStep is the address step (setupSteps["address"]).
func setupAddressStep(r *setupRun, st *setupState) error {
	ctx := context.Background()
	ts, tsErr := r.tailscale.Status(ctx)
	port := setupAddrPort(r.facts.addr)
	var (
		plan setupAddrPlan
		err  error
	)
	if r.asks() {
		plan, err = setupAskAddress(ctx, r, st, ts, tsErr, port)
	} else {
		plan, err = setupAddressFromEnv(ctx, r, st, ts, tsErr, port)
	}
	if err != nil {
		return err
	}
	setupExplainAddress(r, st, plan)
	return setupApplyAddress(r, st, plan)
}

// setupAddressFromEnv is the step's answer from the environment: the name,
// and the certificate's files or the tailnet's certificate.
func setupAddressFromEnv(ctx context.Context, r *setupRun, st *setupState, ts tailscaleStatus, tsErr error, port string) (setupAddrPlan, error) {
	raw := r.env(setupKnobServerName)
	if raw == "" {
		hint := ""
		if tsErr == nil && ts.usable() == "" {
			hint = fmt.Sprintf("; %s=%s alone takes this computer's Tailscale name, with a certificate from the tailnet", setupKnobServerName, ts.name)
		}
		return setupAddrPlan{}, setupRefusal("%s=%s needs the address the other computers reach this one at: set %s to its name, "+
			"and %s and %s to its certificate and key%s", setupKnobKind, setupWords[setupServer].env, setupKnobServerName, setupKnobTLSCert, setupKnobTLSKey, hint)
	}
	plan := setupAddrPlan{name: setupNormalName(raw), port: port}
	if p := setupResolveAddress(ctx, r, &plan); p != "" {
		return plan, setupRefusal("%s=%s: %s", setupKnobServerName, raw, p)
	}
	plan.issues = tsErr == nil && ts.issues(plan.name)
	if cert := r.env(setupKnobTLSCert); cert != "" {
		plan.source = setupAddrFiles
		var p string
		plan.cert, _ = setupAddrFilePath(r, cert)
		plan.key, _ = setupAddrFilePath(r, r.env(setupKnobTLSKey))
		if plan.leaf, p = setupCheckPair(plan.name, plan.cert, plan.key, r.now()); p != "" {
			return plan, setupRefusal("%s and %s: %s", setupKnobTLSCert, setupKnobTLSKey, p)
		}
		return plan, nil
	}
	if tsErr == nil && ts.name == plan.name && ts.usable() == "" {
		plan.source = setupAddrTailscale
		return plan, setupMint(ctx, r, st, &plan)
	}
	return plan, setupRefusal("setup cannot get a certificate for %s itself: %s, and set %s and %s to it and its key. %s",
		plan.name, setupNoCertWhy(plan.name, ts, tsErr), setupKnobTLSCert, setupKnobTLSKey, setupLANCertHint(r, plan.name))
}

// setupNoCertWhy says why the tailnet gives no certificate for name.
func setupNoCertWhy(name string, ts tailscaleStatus, tsErr error) string {
	switch {
	case tsErr != nil:
		return "it is not this computer's Tailscale name (" + tsErr.Error() + "): give its own certificate"
	case ts.name != name && ts.name != "":
		return "it is not this computer's Tailscale name (" + ts.name + "): give its own certificate"
	case ts.usable() != "":
		return ts.usable() + "; or give its own certificate"
	}
	return "give its own certificate"
}

// setupLANCertHint is how to make a certificate for a name on the LAN, as the
// network-install guide makes one, written into the directory setup keeps
// the tailnet's in.
func setupLANCertHint(r *setupRun, name string) string {
	cert, key := setupLANPair(r, name)
	return "To make one for a name on your LAN: " + strings.Join(setupLANCertCommands(name, cert, key), "; or ")
}

// setupLANPair is where the commands setup prints write a LAN name's pair.
func setupLANPair(r *setupRun, name string) (string, string) {
	dir, err := setupTLSDir(r)
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
}

// setupLANCertCommands makes a certificate for name at cert and key: mkcert,
// which also makes the CA every other computer trusts, or openssl, whose
// certificate is its own root (CA:TRUE). Both keep the key this account's
// alone.
func setupLANCertCommands(name, cert, key string) []string {
	dir := tildePath(filepath.Dir(cert))
	c, k := tildePath(cert), tildePath(key)
	return []string{
		fmt.Sprintf("mkdir -p %s && mkcert -install && mkcert -cert-file %s -key-file %s %s", dir, c, k, name),
		fmt.Sprintf(`mkdir -p %s && openssl req -x509 -newkey rsa:2048 -sha256 -days 825 -nodes -keyout %s -out %s -subj "/CN=%s" `+
			`-addext "subjectAltName=DNS:%s" -addext "basicConstraints=critical,CA:TRUE" && chmod 600 %s`, dir, k, c, name, name, k),
	}
}

// setupResolveAddress resolves plan.name on this computer and fills in the
// bind that holds every address of it, or says why none can.
func setupResolveAddress(ctx context.Context, r *setupRun, plan *setupAddrPlan) string {
	found, err := r.network.lookup(ctx, plan.name)
	if err != nil || len(found) == 0 {
		why := "it resolves to no address"
		if err != nil {
			why = err.Error()
		}
		return fmt.Sprintf("%s does not resolve on this computer (%s), so no computer can be told to dial it -- check the name, "+
			"or give it an address (its DNS entry, or /etc/hosts)", plan.name, why)
	}
	var addrs, foreign []netip.Addr
	for _, a := range found {
		a = a.WithZone("").Unmap()
		if !slices.Contains(addrs, a) {
			addrs = append(addrs, a)
		}
	}
	plan.addrs = addrs
	for _, a := range addrs {
		switch {
		case a.IsLoopback():
			return fmt.Sprintf("%s resolves to %s here, this computer's loopback address, and a name the other computers dial must "+
				"resolve to an address they reach: correct its entry (/etc/hosts, or its DNS)", plan.name, a)
		case !a.IsGlobalUnicast() && !a.IsLinkLocalUnicast():
			return fmt.Sprintf("%s resolves to %s, which no computer can dial", plan.name, a)
		case !r.network.holds(a):
			foreign = append(foreign, a)
		}
	}
	if len(foreign) > 0 {
		return fmt.Sprintf("%s resolves to %s, and this computer does not hold %s: a computer that dials that address reaches "+
			"another computer, or nothing -- give the name only this computer's addresses", plan.name, joinAddrs(addrs), joinAddrs(foreign))
	}
	plan.bind, plan.paired, plan.wildcard = setupBindFor(ctx, addrs, plan.port, r.network)
	return ""
}

// setupBindFor is the narrowest addr that holds every one of addrs on port:
// one of them (an IPv4 address first), with what serve listens on beside it
// (boundAddrs, which reads pairedIPv6 as serve does), else the wildcard.
func setupBindFor(ctx context.Context, addrs []netip.Addr, port string, hn hostNet) (bind string, paired []netip.Addr, wildcard bool) {
	ordered := slices.Clone(addrs)
	slices.SortStableFunc(ordered, func(a, b netip.Addr) int {
		switch {
		case a.Is4() == b.Is4():
			return 0
		case a.Is4():
			return -1
		}
		return 1
	})
	for _, a := range ordered {
		if a.IsLinkLocalUnicast() {
			continue // a zone-less link-local bind names no interface
		}
		cand := net.JoinHostPort(a.String(), port)
		held, err := boundAddrs(ctx, cand, hn)
		if err != nil {
			continue
		}
		if !slices.ContainsFunc(addrs, func(x netip.Addr) bool { return !held.covers(x) }) {
			if len(held.addrs) > 1 {
				paired = held.addrs[1:]
			}
			return cand, paired, false
		}
	}
	return net.JoinHostPort("0.0.0.0", port), nil, true
}

// setupCheckPair checks a TLS pair the Server would serve: it loads as a pair,
// its certificate names name and is valid at now, and no account but this one
// can read or rewrite the key. It returns the certificate, or what is wrong.
func setupCheckPair(name, certFile, keyFile string, now time.Time) (*x509.Certificate, string) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, "the certificate and key do not load as a pair: " + err.Error()
	}
	leaf := pair.Leaf
	if leaf == nil {
		if leaf, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
			return nil, "the certificate does not parse: " + err.Error()
		}
	}
	switch {
	case leaf.VerifyHostname(name) != nil:
		return leaf, fmt.Sprintf("%s does not name %s (it names %s): every computer that dials the name would refuse it",
			tildePath(certFile), name, strings.Join(certNames(leaf), ", "))
	case now.After(leaf.NotAfter):
		return leaf, fmt.Sprintf("%s expired on %s", tildePath(certFile), leaf.NotAfter.UTC().Format(time.DateOnly))
	case now.Before(leaf.NotBefore):
		return leaf, fmt.Sprintf("%s is not valid until %s", tildePath(certFile), leaf.NotBefore.UTC().Format(time.DateOnly))
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(keyFile)
		if err != nil {
			return leaf, err.Error()
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return leaf, fmt.Sprintf("%s is mode %04o: another account on this computer can read or replace the private key, and with it "+
				"serve %s -- chmod 600 %s", tildePath(keyFile), perm, name, tildePath(keyFile))
		}
	}
	return leaf, ""
}

// setupMint gets the tailnet's certificate for plan.name into setup's TLS
// directory and checks it. With locked projects on it gets none: the key must
// be the daemon's account's alone, so root mints it where only that account
// reads it, and setup prints how (setupPeerAuthAddress).
func setupMint(ctx context.Context, r *setupRun, st *setupState, plan *setupAddrPlan) error {
	plan.issues = true
	if st.locked {
		plan.cert, plan.key = setupPeerAuthPair(plan.name)
		return nil
	}
	dir, err := setupTLSDir(r)
	if err != nil {
		return fmt.Errorf("seamlessd.setup: address: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("seamlessd.setup: address: %w", err)
	}
	plan.cert, plan.key = filepath.Join(dir, plan.name+".crt"), filepath.Join(dir, plan.name+".key")
	setupRow(r.out, "certificate", "asking the tailnet for one: tailscale cert "+plan.name)
	if err := r.tailscale.Cert(ctx, plan.name, plan.cert, plan.key); err != nil {
		return setupRefusal("the tailnet gave no certificate for %s (%v): run  tailscale cert %s  to see why, or give a certificate "+
			"of your own with %s and %s", plan.name, err, plan.name, setupKnobTLSCert, setupKnobTLSKey)
	}
	plan.minted = true
	leaf, p := setupCheckPair(plan.name, plan.cert, plan.key, r.now())
	if p != "" {
		return setupRefusal("the tailnet's certificate for %s: %s", plan.name, p)
	}
	plan.leaf = leaf
	return nil
}

// setupAskAddress is the step's questions for a person at the terminal:
// where the other computers reach this one, then its certificate.
func setupAskAddress(ctx context.Context, r *setupRun, st *setupState, ts tailscaleStatus, tsErr error, port string) (setupAddrPlan, error) {
	tsOK := tsErr == nil && ts.usable() == ""
	fmt.Fprintf(r.out, "\n%s\n", bold("Where will the other computers reach this one?"))
	var words [][]string
	if tsOK {
		setupOption(r.out, 1, "Tailscale", ts.name+": this computer's name on your tailnet, with a certificate from the tailnet")
		words = append(words, []string{"tailscale"})
	} else {
		why := ts.usable()
		if tsErr != nil {
			why = tsErr.Error()
		}
		fmt.Fprintf(r.out, "      %s\n", dim("Tailscale: "+why+"."))
	}
	setupOption(r.out, len(words)+1, "Domain", "a name of yours, with its certificate and key")
	words = append(words, []string{"domain"})
	setupOption(r.out, len(words)+1, "LAN", "a name on your local network: setup shows how to make its certificate")
	words = append(words, []string{"lan"})
	prompt, def := "Enter 1 or 2: ", 0
	if tsOK {
		prompt, def = "Enter 1-3 [1]: ", 1
	}
	n, err := setupChooseOr(r, prompt, def, words, setupRefusal("no answer: set %s=%s, %s, %s and %s for a run without questions",
		setupKnobKind, setupWords[setupServer].env, setupKnobServerName, setupKnobTLSCert, setupKnobTLSKey))
	if err != nil {
		return setupAddrPlan{}, err
	}
	switch words[n-1][0] {
	case "tailscale":
		plan := setupAddrPlan{name: ts.name, port: port, source: setupAddrTailscale}
		if p := setupResolveAddress(ctx, r, &plan); p != "" {
			return plan, setupRefusal("%s", p)
		}
		return plan, setupMint(ctx, r, st, &plan)
	case "domain":
		plan, err := setupAskName(ctx, r, port, "Name (the host the other computers dial, like seamless.example.com): ", "")
		if err != nil {
			return plan, err
		}
		plan.issues = tsErr == nil && ts.issues(plan.name)
		return plan, setupAskPair(r, &plan, "", "")
	}
	plan, err := setupAskName(ctx, r, port, "", setupLANDefault(r.host))
	if err != nil {
		return plan, err
	}
	plan.issues = tsErr == nil && ts.issues(plan.name)
	cert, key := setupLANPair(r, plan.name)
	fmt.Fprintf(r.out, "  %s\n", dim("Make its certificate in another terminal with one of these, then come back here:"))
	for _, c := range setupLANCertCommands(plan.name, cert, key) {
		fmt.Fprintf(r.out, "    %s\n", c)
	}
	fmt.Fprintf(r.out, "  %s\n", dim("Each other computer then trusts it: mkcert's rootCA.pem (in mkcert -CAROOT), or the openssl certificate itself."))
	return plan, setupAskPair(r, &plan, cert, key)
}

// setupLANDefault is the name a LAN name question offers: this computer's
// own, with .local when it has no domain.
func setupLANDefault(host string) string {
	h := setupNormalName(host)
	if h == "" {
		return ""
	}
	if !strings.Contains(h, ".") {
		h += ".local"
	}
	if setupServerNameProblem(h) != "" {
		return ""
	}
	return h
}

// setupAskName asks for the Server's name until it is one setup can take and
// that resolves to this computer's addresses alone.
func setupAskName(ctx context.Context, r *setupRun, port, prompt, def string) (setupAddrPlan, error) {
	if prompt == "" {
		prompt = "Name (this computer's name on your LAN): "
		if def != "" {
			prompt = "Name (this computer's name on your LAN) [" + def + "]: "
		}
	}
	for {
		fmt.Fprint(r.out, prompt)
		line, err := r.in.ReadString('\n')
		answer := strings.TrimSpace(line)
		if answer == "" {
			answer = def
		}
		if answer != "" {
			plan := setupAddrPlan{name: setupNormalName(answer), port: port}
			problem := setupServerNameProblem(answer)
			if problem == "" {
				problem = setupResolveAddress(ctx, r, &plan)
			}
			if problem == "" {
				return plan, nil
			}
			fmt.Fprintf(r.out, "  %s\n", problem)
		}
		if err != nil {
			return setupAddrPlan{}, setupRefusal("no name: set %s=%s, %s, %s and %s for a run without questions",
				setupKnobKind, setupWords[setupServer].env, setupKnobServerName, setupKnobTLSCert, setupKnobTLSKey)
		}
	}
}

// setupAskPair asks for the certificate and key files until they are a pair
// for plan.name; Enter takes certDef and keyDef when there are any. The end
// of input ends the question: a default it would take again cannot change.
func setupAskPair(r *setupRun, plan *setupAddrPlan, certDef, keyDef string) error {
	knobs := fmt.Sprintf("set %s and %s for a run without questions", setupKnobTLSCert, setupKnobTLSKey)
	// ask reads one path, "" at the end of input with none to take; ended
	// reports that the input has ended.
	ask := func(what, def string) (path string, ended bool) {
		prompt := what + ": "
		if def != "" {
			prompt = what + " [" + tildePath(def) + "]: "
		}
		for {
			fmt.Fprint(r.out, prompt)
			line, err := r.in.ReadString('\n')
			answer := setupShellWord(r.goos, strings.TrimSpace(line))
			if answer == "" {
				answer = def
			}
			if answer != "" {
				p, problem := setupAddrFilePath(r, answer)
				if problem == "" {
					return p, err != nil
				}
				fmt.Fprintf(r.out, "  %s\n", problem)
			}
			if err != nil {
				return "", true
			}
		}
	}
	for {
		cert, certEnded := ask("Certificate file", certDef)
		if cert == "" {
			return setupRefusal("no certificate: %s", knobs)
		}
		key, keyEnded := ask("Key file", keyDef)
		if key == "" {
			return setupRefusal("no key: %s", knobs)
		}
		leaf, problem := setupCheckPair(plan.name, cert, key, r.now())
		if problem == "" {
			plan.source, plan.cert, plan.key, plan.leaf = setupAddrFiles, cert, key, leaf
			return nil
		}
		fmt.Fprintf(r.out, "  %s\n", problem)
		if certEnded || keyEnded {
			return setupRefusal("%s; %s", problem, knobs)
		}
	}
}

// setupExplainAddress says each choice the step made, one line each, as
// doctor reports them afterwards.
func setupExplainAddress(r *setupRun, st *setupState, p setupAddrPlan) {
	why := func(s string) { fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim(s)) }
	fmt.Fprintln(r.out)
	setupRow(r.out, "address", p.serverURL())
	if p.source == setupAddrTailscale {
		why("this computer's Tailscale name (MagicDNS): every computer on the tailnet resolves it")
	} else {
		why("the name the other computers dial: the certificate names it, and so do the passkeys")
	}
	switch {
	case p.leaf == nil:
		cert, _ := setupPeerAuthPair(p.name)
		setupRow(r.out, "certificate", cert+", which root gets from the tailnet (below)")
		why("tailscale cert, from Let's Encrypt for 90 days: root renews it the same way, then restarts the daemon")
	case p.source == setupAddrTailscale:
		setupRow(r.out, "certificate", tildePath(p.cert)+", valid until "+p.leaf.NotAfter.UTC().Format(time.DateOnly))
		why(fmt.Sprintf("from your tailnet (Let's Encrypt): renew it before then, and doctor warns 30 days ahead: "+
			"tailscale cert --cert-file %s --key-file %s %s, then seamlessd restart", tildePath(p.cert), tildePath(p.key), p.name))
	default:
		setupRow(r.out, "certificate", tildePath(p.cert)+", valid until "+p.leaf.NotAfter.UTC().Format(time.DateOnly))
		why("yours: renew it before then, then seamlessd restart (doctor warns 30 days ahead); a computer that does not trust " +
			"its issuer trusts it by tls.ca_file")
	}
	bind := p.bind
	if _, port, err := net.SplitHostPort(p.bind); err == nil {
		bind += pairedSuffix(port, p.paired)
	}
	setupRow(r.out, "bind", bind)
	if p.wildcard {
		why(fmt.Sprintf("every interface: %s resolves to %s, and no one address with the IPv6 addresses serve pairs with it "+
			"holds them all; the wildcard does, and listens on every network this computer joins, where TLS and the key are the guard",
			p.name, joinAddrs(p.addrs)))
	} else {
		why(fmt.Sprintf("every address %s resolves to here (%s), and no other interface: no other process can answer for the name",
			p.name, joinAddrs(p.addrs)))
	}
	probe := config.Defaults()
	probe.AdvertisedURL, probe.TLS.CertFile, probe.TLS.KeyFile = p.serverURL(), "cert", "key"
	if _, origin, ok := probe.ServedRelyingParty(); ok {
		setupRow(r.out, "passkeys", origin)
		why("the passkey origin, derived from server_url: a passkey made there works only under " + p.name)
	}
	setupRow(r.out, "allowed", "no other name")
	why("the daemon answers " + p.name + " and its own addresses, and refuses (421) a request that names any other")
	switch {
	case p.minted:
		setupRow(r.out, "key", "this account can get a certificate for "+p.name+" itself (tailscale cert)")
		why("so the TLS key is not what keeps a page of this account's off the console's origin: the bind holding every address of the name is")
	case p.issues:
		setupRow(r.out, "key", "the tailnet issues certificates for "+p.name+" (tailscale cert)")
		why("an account that may run it can serve the name with a valid certificate, so the TLS key does not keep its pages off " +
			"the console's origin: the bind holding every address of the name does")
	case st.locked:
		_, key := setupPeerAuthPair(p.name)
		setupRow(r.out, "key", key+": the daemon's account reads it, and no other")
		why("whoever reads the key serves " + p.name + " with its certificate, on any port")
	default:
		setupRow(r.out, "key", tildePath(p.key)+": this account's alone")
		why("the daemon runs as this account, so every process of it can read the key and serve the name with it; " +
			"locked projects (peer_auth) put it out of their reach")
	}
}

// setupApplyAddress puts the plan where the daemon reads it: a first
// install's config, written once the first half's steps have run; the lines
// for any other computer's; or, with locked projects on, the root steps.
// Only a first install, with nothing installed, gets a config written: every
// run of seamlessd update is on a computer set up already, whose update
// confirms the daemon by the config it loaded before the install.
func setupApplyAddress(r *setupRun, st *setupState, p setupAddrPlan) error {
	a := p.address(p.cert, p.key)
	switch {
	case st.locked:
		setupPeerAuthAddress(r, p)
		return nil
	case st.installed == setupNone && !r.facts.configFound:
		st.server = &a
		return nil
	}
	path := tildePath(r.facts.configPath)
	if r.facts.configFound {
		if cfg, err := r.loadConfig(); err == nil && cfg.HasServerAddress(a) {
			setupRow(r.out, "config", "already says this "+dim("-> "+path))
			return nil
		}
		setupRow(r.out, "config", "setup does not edit "+path+", which is yours: put these in it, replacing any it has,")
	} else {
		setupRow(r.out, "config", "setup writes a config only on a first install, and this computer has Seamless already: put these in "+path+",")
	}
	fmt.Fprint(r.out, a.Lines(setupRowCont))
	fmt.Fprintf(r.out, "%s%s\n%s%s\n", setupRowCont, dim("then run the installer again: it restarts the daemon and wires the hooks for https"),
		setupRowCont, green(setupInstallCommand(r.goos)))
	return nil
}

// setupPeerAuthAddress prints what root does on a computer whose daemon runs
// as its own account: the TLS pair into the guide's directory, readable by
// that account alone, the daemon's config, and the restart.
func setupPeerAuthAddress(r *setupRun, p setupAddrPlan) {
	acct := r.daemonAccount()
	cert, key := setupPeerAuthPair(p.name)
	setupRow(r.out, "root steps", "locked projects are on: the daemon's config and its TLS pair are its own account's, and setup")
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, "runs no root step. As an administrator:")
	cmds := []string{fmt.Sprintf("sudo install -d -o root -g %s -m 0750 %s", acct, setupPeerAuthTLSDir)}
	if p.source == setupAddrTailscale {
		cmds = append(cmds,
			fmt.Sprintf("sudo tailscale cert --cert-file %s --key-file %s %s", cert, key, p.name),
			fmt.Sprintf("sudo chown root:%s %s %s && sudo chmod 0644 %s && sudo chmod 0640 %s", acct, cert, key, cert, key))
	} else {
		cmds = append(cmds,
			fmt.Sprintf("sudo install -o root -g %s -m 0644 %s %s", acct, p.cert, cert),
			fmt.Sprintf("sudo install -o root -g %s -m 0640 %s %s", acct, p.key, key))
	}
	for _, c := range cmds {
		fmt.Fprintf(r.out, "%s  %s\n", setupRowCont, c)
	}
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, "then put these in the daemon's config ("+setupPeerAuthConfig+"), replacing any it has:")
	fmt.Fprint(r.out, p.address(cert, key).Lines(setupRowCont+"  "))
	restart := "sudo systemctl restart " + strings.TrimSuffix(systemDaemonUnit, ".service")
	if r.goos == "darwin" {
		restart = "sudo launchctl kickstart -k system/" + systemDaemonLabel
	}
	fmt.Fprintf(r.out, "%s%s\n%s  %s\n", setupRowCont, "then restart the daemon:", setupRowCont, restart)
	if p.source == setupAddrFiles {
		fmt.Fprintf(r.out, "%s%s\n", setupRowCont, "and delete "+tildePath(p.key)+" once it serves: every process of yours can read it there")
	}
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, `and in the console's config (~/.config/seamless/console.yaml), server_url: "`+p.serverURL()+`"`)
}

// setupWriteServer writes the config a first install's address step decided
// on, once every step of setup's first half has run. A config that appeared
// meanwhile is never edited: EnsureServerConfig adopts one that says the
// same, and refuses any other.
func setupWriteServer(r *setupRun, st *setupState) error {
	if st.server == nil {
		return nil
	}
	wrote, err := r.ensureServer(*st.server)
	if err != nil {
		return fmt.Errorf("seamlessd.setup: address: %w", err)
	}
	if wrote == "" {
		setupRow(r.out, "config", "already says this")
		return nil
	}
	setupRow(r.out, "config", "wrote addr, server_url, allowed_hosts and tls "+dim("-> "+tildePath(wrote)))
	return nil
}

// setupClientsStep is the Server's closing (setupSteps["clients"]): what each
// Client needs from this computer -- its address, its certificate's
// fingerprint, how to get a pairing code and the one-liners -- as
// `seamlessd devices code` prints it with a code.
func setupClientsStep(r *setupRun, _ *setupState) error {
	cfg, err := r.loadConfig()
	if err != nil {
		return err
	}
	if cfg.SocketPath() != "" {
		// The owner's half of a peer_auth install: the daemon's config, and the
		// certificate under it, are its own account's to read.
		fmt.Fprintln(r.out)
		fmt.Fprintln(r.out, setupText("The {server}'s daemon runs as its own account, which keeps its config: as that account, "+
			"seamlessd devices code prints what each {client} needs, with a pairing code:"))
		fmt.Fprintf(r.out, "  sudo -u %s env SEAMLESS_CONFIG=%s /opt/seamless/bin/seamlessd devices code\n", r.daemonAccount(), setupPeerAuthConfig)
		return nil
	}
	if !pairingOn(cfg) {
		fmt.Fprintln(r.out)
		fmt.Fprintln(r.out, setupText("This {server} serves no other computer yet: once its config has its address and TLS, "+
			"seamlessd devices code prints what each {client} needs."))
		return nil
	}
	facts, err := pairingFacts(cfg)
	if err != nil {
		return err
	}
	fmt.Fprintln(r.out)
	serverPairingClosing(r.out, facts, "", cfg.PeerAuth.Enabled)
	return nil
}

// setupDaemonAccount is the account a peer_auth daemon runs as, for the root
// steps setup prints: the one root pinned (access.DaemonAccount), else the
// guide's.
func setupDaemonAccount() string {
	if pin, ok, err := access.DaemonAccount(); err == nil && ok && access.ValidDaemonAccountName(pin.Name) {
		return pin.Name
	}
	return "_seamless"
}
