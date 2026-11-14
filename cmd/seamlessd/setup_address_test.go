package main

import (
	"bufio"
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// setupAddrTailnet is a tailnet for the address step's tests: Status answers
// st (or err), and Cert records the name it was asked for and runs mint.
type setupAddrTailnet struct {
	st    tailscaleStatus
	err   error
	mint  func(name, certFile, keyFile string) error
	calls *[]string
}

func (f setupAddrTailnet) Status(context.Context) (tailscaleStatus, error) { return f.st, f.err }

func (f setupAddrTailnet) Cert(_ context.Context, name, certFile, keyFile string) error {
	if f.calls != nil {
		*f.calls = append(*f.calls, name)
	}
	if f.mint == nil {
		return errors.New("this test's tailnet issues no certificate")
	}
	return f.mint(name, certFile, keyFile)
}

// The names and machines the address tests stand up.
const (
	setupAddrTSName  = "studio.example.ts.net"
	setupAddrDomain  = "seamless.example.com"
	setupAddrTailv4  = "100.64.0.7"
	setupAddrTailv6  = "fd7a:115c:a1e0::7"
	setupAddrLANv4   = "192.168.1.9"
	setupAddrLANula  = "fd00:1::9"
	setupAddrLANglob = "2001:db8::9"
)

// setupAddrIfaces is a machine with a loopback, a tailnet and a LAN interface.
var setupAddrIfaces = [][]netip.Addr{
	addrs("127.0.0.1", "::1"),
	addrs(setupAddrTailv4, setupAddrTailv6),
	addrs(setupAddrLANv4, "fe80::aa", setupAddrLANglob, setupAddrLANula),
}

// setupAddrTailnetUp is a tailnet that runs here and issues this computer's
// name a certificate.
var setupAddrTailnetUp = tailscaleStatus{running: true, name: setupAddrTSName,
	addrs: addrs(setupAddrTailv4, setupAddrTailv6), certDomains: []string{setupAddrTSName}}

// setupAddrWorld is what an arranged run records: the configs setup wrote
// (ensureServer's calls), the certificates it asked the tailnet for, and the
// scratch directory its config and certificates live in.
type setupAddrWorld struct {
	dir    string
	wrote  []config.ServerAddress
	minted []string
}

// setupAddrArrange points r's address step at a scratch machine: hosts is
// what each name resolves to, ifaces the interfaces, ts the tailnet (err for
// none). The config the installers manage moves into the scratch directory,
// and the tailnet's certificates are real self-signed pairs for the name.
func setupAddrArrange(t *testing.T, r *setupRun, hosts map[string][]string, ifaces [][]netip.Addr, ts tailscaleStatus, tsErr error) *setupAddrWorld {
	t.Helper()
	w := &setupAddrWorld{dir: t.TempDir()}
	r.facts.configPath = filepath.Join(w.dir, "seamless.yaml")
	r.home = w.dir
	r.network = hostNet{lookup: fakeLookup(hosts), ifaces: func() ([][]netip.Addr, error) { return ifaces, nil }}
	r.tailscale = setupAddrTailnet{st: ts, err: tsErr, calls: &w.minted, mint: func(name, certFile, keyFile string) error {
		setupAddrWritePair(t, certFile, keyFile, time.Now().Add(90*24*time.Hour), name)
		return nil
	}}
	r.ensureServer = func(a config.ServerAddress) (string, error) {
		w.wrote = append(w.wrote, a)
		return r.facts.configPath, nil
	}
	return w
}

// setupAddrWritePair writes a self-signed pair for hosts at certFile and
// keyFile, the key this account's alone.
func setupAddrWritePair(t *testing.T, certFile, keyFile string, notAfter time.Time, hosts ...string) {
	t.Helper()
	c, k := writeCertPair(t, notAfter, hosts...)
	for _, f := range []struct{ from, to string }{{c, certFile}, {k, keyFile}} {
		raw, err := os.ReadFile(f.from)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(f.to), 0o700))
		require.NoError(t, os.WriteFile(f.to, raw, 0o600))
	}
}

// setupAddrPair is a pair for hosts in a directory of its own.
func setupAddrPair(t *testing.T, hosts ...string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	setupAddrWritePair(t, cert, key, time.Now().Add(365*24*time.Hour), hosts...)
	return cert, key
}

// setupAddrUnix skips where the Server is never set up: Windows, which is
// offered Standalone and Client only.
func setupAddrUnix(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows is never the Server")
	}
	return runtime.GOOS
}

// setupAddrFirstHalf runs setup's first half under the installer and returns
// what it printed, the state it wrote and its error.
func setupAddrFirstHalf(t *testing.T, r *setupRun) (string, setupState, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "setup.state")
	err := setupForInstaller(r, path)
	out := r.out.(*strings.Builder).String()
	if err != nil {
		require.NoFileExists(t, path, "a stopped setup writes no state")
		return out, setupState{}, err
	}
	st, rerr := readSetupState(path)
	require.NoError(t, rerr)
	return out, st, nil
}

// The tailnet's name: setup asks the tailnet for the certificate, binds the
// name's IPv4 address with its interface's IPv6 beside it, and writes the
// config once the first half is done -- saying each choice, and that this
// account can mint a certificate for the name itself.
func TestSetupAddress_TailnetName(t *testing.T) {
	goos := setupAddrUnix(t)
	r, _, _ := setupTestRun(t, goos, []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=" + strings.ToUpper(setupAddrTSName) + "."},
		setupFactsFresh, false, "")
	w := setupAddrArrange(t, r, map[string][]string{setupAddrTSName: {setupAddrTailv4, setupAddrTailv6}}, setupAddrIfaces, setupAddrTailnetUp, nil)
	out, st, err := setupAddrFirstHalf(t, r)
	require.NoError(t, err, out)
	require.Equal(t, config.RoleServer, st.role)
	require.Equal(t, []string{setupAddrTSName}, w.minted)
	tls := filepath.Join(w.dir, "tls")
	require.Equal(t, []config.ServerAddress{{Addr: setupAddrTailv4 + ":8081", ServerURL: "https://" + setupAddrTSName + ":8081",
		CertFile: filepath.Join(tls, setupAddrTSName+".crt"), KeyFile: filepath.Join(tls, setupAddrTSName+".key")}}, w.wrote)
	for _, want := range []string{
		"  address      https://" + setupAddrTSName + ":8081\n",
		"this computer's Tailscale name (MagicDNS)",
		"from your tailnet (Let's Encrypt): renew it before then, and doctor warns 30 days ahead: tailscale cert --cert-file " +
			filepath.Join(tls, setupAddrTSName+".crt") + " --key-file " + filepath.Join(tls, setupAddrTSName+".key") + " " + setupAddrTSName,
		"  bind         " + setupAddrTailv4 + ":8081 (and [" + setupAddrTailv6 + "]:8081 beside it, on the same interface)\n",
		"every address " + setupAddrTSName + " resolves to here (" + setupAddrTailv4 + ", " + setupAddrTailv6 + "), and no other interface",
		"  passkeys     https://" + setupAddrTSName + ":8081\n",
		"  allowed      no other name\n",
		"  key          this account can get a certificate for " + setupAddrTSName + " itself (tailscale cert)\n",
		"so the TLS key is not what keeps a page of this account's off the console's origin: the bind holding every address of the name is",
		"  config       wrote addr, server_url, allowed_hosts and tls",
	} {
		require.Contains(t, out, want)
	}
	require.NotContains(t, out, "by hand", "the steps by hand come after the installer's own")
	require.Less(t, strings.Index(out, "  bind "), strings.Index(out, "  config "), "the config is written last")
}

// A domain's own certificate: the files are checked as a pair for the name,
// and the bind is the one address the name has with the unique local address
// serve pairs with it -- never the interface's global IPv6 address, which the
// name does not resolve to.
func TestSetupAddress_OwnCertificate(t *testing.T) {
	goos := setupAddrUnix(t)
	cert, key := setupAddrPair(t, setupAddrDomain)
	r, _, _ := setupTestRun(t, goos, []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=" + setupAddrDomain,
		"SEAMLESS_TLS_CERT=" + cert, "SEAMLESS_TLS_KEY=" + key}, setupFactsFresh, false, "")
	w := setupAddrArrange(t, r, map[string][]string{setupAddrDomain: {setupAddrLANv4, setupAddrLANula}}, setupAddrIfaces, tailscaleStatus{}, errNoTailscale)
	out, _, err := setupAddrFirstHalf(t, r)
	require.NoError(t, err, out)
	require.Empty(t, w.minted)
	require.Equal(t, []config.ServerAddress{{Addr: setupAddrLANv4 + ":8081", ServerURL: "https://" + setupAddrDomain + ":8081",
		CertFile: cert, KeyFile: key}}, w.wrote)
	require.Contains(t, out, "  bind         "+setupAddrLANv4+":8081 (and ["+setupAddrLANula+"]:8081 beside it, on the same interface)\n")
	require.Contains(t, out, "the name the other computers dial: the certificate names it, and so do the passkeys")
	require.Contains(t, out, "  key          "+tildePath(key)+": this account's alone\n")
	require.Contains(t, out, "the daemon runs as this account, so every process of it can read the key")
	require.NotContains(t, out, "tailscale cert")
}

// A name whose addresses no IPv4 bind and its pairing hold gets the wildcard,
// and says what that costs.
func TestSetupAddress_Wildcard(t *testing.T) {
	goos := setupAddrUnix(t)
	cert, key := setupAddrPair(t, setupAddrDomain)
	r, _, _ := setupTestRun(t, goos, []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=" + setupAddrDomain,
		"SEAMLESS_TLS_CERT=" + cert, "SEAMLESS_TLS_KEY=" + key}, setupFactsFresh, false, "")
	w := setupAddrArrange(t, r, map[string][]string{setupAddrDomain: {setupAddrLANv4, setupAddrLANglob}}, setupAddrIfaces, tailscaleStatus{}, errNoTailscale)
	out, _, err := setupAddrFirstHalf(t, r)
	require.NoError(t, err, out)
	require.Len(t, w.wrote, 1)
	require.Equal(t, "0.0.0.0:8081", w.wrote[0].Addr)
	require.Contains(t, out, "  bind         0.0.0.0:8081\n")
	require.Contains(t, out, "every interface: "+setupAddrDomain+" resolves to "+setupAddrLANv4+", "+setupAddrLANglob)
	require.Contains(t, out, "listens on every network this computer joins")
}

// setupBindFor picks the narrowest bind that holds every address, judged as
// doctor judges the listener (boundAddrs).
func TestSetupBindFor(t *testing.T) {
	hn := hostNet{lookup: fakeLookup(nil), ifaces: func() ([][]netip.Addr, error) { return setupAddrIfaces, nil }}
	split := hostNet{lookup: fakeLookup(nil), ifaces: func() ([][]netip.Addr, error) {
		return [][]netip.Addr{addrs(setupAddrTailv4), addrs(setupAddrTailv6)}, nil
	}}
	for _, tc := range []struct {
		name     string
		hn       hostNet
		addrs    []netip.Addr
		bind     string
		paired   []netip.Addr
		wildcard bool
	}{
		{"a tailnet name: its IPv4, with the IPv6 beside it", hn, addrs(setupAddrTailv6, setupAddrTailv4), setupAddrTailv4 + ":8081", addrs(setupAddrTailv6), false},
		{"one IPv4 address alone", hn, addrs(setupAddrLANv4), setupAddrLANv4 + ":8081", addrs(setupAddrLANula), false},
		{"one IPv6 address alone", hn, addrs(setupAddrLANula), "[" + setupAddrLANula + "]:8081", nil, false},
		{"a global IPv6 beside a private IPv4: the wildcard", hn, addrs(setupAddrLANv4, setupAddrLANglob), "0.0.0.0:8081", nil, true},
		{"two interfaces: the wildcard", hn, addrs(setupAddrLANv4, setupAddrTailv4), "0.0.0.0:8081", nil, true},
		{"the IPv6 on an interface of its own: the wildcard", split, addrs(setupAddrTailv4, setupAddrTailv6), "0.0.0.0:8081", nil, true},
		{"a link-local address of the name: the wildcard", hn, addrs(setupAddrLANv4, "fe80::aa"), "0.0.0.0:8081", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bind, paired, wildcard := setupBindFor(context.Background(), tc.addrs, "8081", tc.hn)
			require.Equal(t, tc.bind, bind)
			require.Equal(t, tc.paired, paired)
			require.Equal(t, tc.wildcard, wildcard)
			held, err := boundAddrs(context.Background(), bind, tc.hn)
			require.NoError(t, err)
			for _, a := range tc.addrs {
				require.True(t, held.covers(a), "the bind setup picked holds %s, as doctor reads it", a)
			}
		})
	}
}

// Every refusal of the address step comes before anything is written: no
// config, no state for the installer.
func TestSetupAddress_Refusals(t *testing.T) {
	goos := setupAddrUnix(t)
	cert, key := setupAddrPair(t, setupAddrDomain)
	otherCert, otherKey := setupAddrPair(t, "elsewhere.example.com")
	expiredDir := t.TempDir()
	expiredCert, expiredKey := filepath.Join(expiredDir, "c.crt"), filepath.Join(expiredDir, "c.key")
	setupAddrWritePair(t, expiredCert, expiredKey, time.Now().Add(-time.Hour), setupAddrDomain)
	openDir := t.TempDir()
	openCert, openKey := filepath.Join(openDir, "c.crt"), filepath.Join(openDir, "c.key")
	setupAddrWritePair(t, openCert, openKey, time.Now().Add(24*time.Hour), setupAddrDomain)
	require.NoError(t, os.Chmod(openKey, 0o644))
	files := func(c, k string) []string {
		return []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=" + setupAddrDomain, "SEAMLESS_TLS_CERT=" + c, "SEAMLESS_TLS_KEY=" + k}
	}
	domain := map[string][]string{setupAddrDomain: {setupAddrLANv4}, setupAddrTSName: {setupAddrTailv4, setupAddrTailv6},
		"lan.example": {setupAddrLANv4}, "far.example": {setupAddrLANv4, "203.0.113.50"}, "self.example": {"127.0.1.1"}}
	noTLSCerts := setupAddrTailnetUp
	noTLSCerts.certDomains = nil
	for _, tt := range []struct {
		name  string
		env   []string
		ts    tailscaleStatus
		tsErr error
		mint  bool // the tailnet issues the certificate it is asked for
		want  []string
	}{
		{name: "the Server with no name", env: []string{"SEAMLESS_SETUP=server"}, tsErr: errNoTailscale,
			want: []string{"SEAMLESS_SETUP=server needs the address the other computers reach this one at: set SEAMLESS_SERVER_NAME"}},
		{name: "the Server with no name names the tailnet's", env: []string{"SEAMLESS_SETUP=server"}, ts: setupAddrTailnetUp,
			want: []string{"SEAMLESS_SERVER_NAME=" + setupAddrTSName + " alone takes this computer's Tailscale name"}},
		{name: "a name that does not resolve", env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=gone.example"}, tsErr: errNoTailscale,
			want: []string{"SEAMLESS_SERVER_NAME=gone.example: gone.example does not resolve on this computer"}},
		{name: "a name another computer holds an address of", env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=far.example",
			"SEAMLESS_TLS_CERT=" + cert, "SEAMLESS_TLS_KEY=" + key}, tsErr: errNoTailscale,
			want: []string{"far.example resolves to " + setupAddrLANv4 + ", 203.0.113.50, and this computer does not hold 203.0.113.50"}},
		{name: "a name of this computer's loopback", env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=self.example"}, tsErr: errNoTailscale,
			want: []string{"self.example resolves to 127.0.1.1 here, this computer's loopback address"}},
		{name: "a certificate for another name", env: files(otherCert, otherKey), tsErr: errNoTailscale,
			want: []string{"does not name " + setupAddrDomain + " (it names elsewhere.example.com)"}},
		{name: "a key that is not the certificate's", env: files(cert, otherKey), tsErr: errNoTailscale,
			want: []string{"the certificate and key do not load as a pair"}},
		{name: "an expired certificate", env: files(expiredCert, expiredKey), tsErr: errNoTailscale, want: []string{"expired on"}},
		{name: "a key another account can read", env: files(openCert, openKey), tsErr: errNoTailscale,
			want: []string{"is mode 0644: another account on this computer can read or replace the private key", "chmod 600"}},
		{name: "no certificate, and not the tailnet's name", env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=lan.example"},
			tsErr: errNoTailscale, want: []string{"setup cannot get a certificate for lan.example itself", "mkcert -install && mkcert -cert-file",
				`openssl req -x509`, `-addext "subjectAltName=DNS:lan.example"`, "chmod 600"}},
		{name: "the tailnet's name, and no certificates on the tailnet", env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=" + setupAddrTSName},
			ts: noTLSCerts, want: []string{"turn on HTTPS certificates in the tailnet's admin console"}},
		{name: "the tailnet gives no certificate", env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=" + setupAddrTSName},
			ts: setupAddrTailnetUp, mint: false, want: []string{"the tailnet gave no certificate for " + setupAddrTSName, "tailscale cert " + setupAddrTSName}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "a key another account can read" && runtime.GOOS == "windows" {
				t.Skip("file modes")
			}
			r, _, _ := setupTestRun(t, goos, tt.env, setupFactsFresh, false, "")
			w := setupAddrArrange(t, r, domain, setupAddrIfaces, tt.ts, tt.tsErr)
			if !tt.mint {
				r.tailscale = setupAddrTailnet{st: tt.ts, err: tt.tsErr, calls: &w.minted}
			}
			out, _, err := setupAddrFirstHalf(t, r)
			require.Error(t, err, out)
			require.ErrorIs(t, err, errSetupRefused)
			for _, want := range tt.want {
				require.Contains(t, err.Error(), want)
			}
			require.Empty(t, w.wrote, "a refused address writes no config")
		})
	}
}

// The address knobs are refused where setup cannot do what they ask, before
// anything is decided or written.
func TestSetupAddressKnobs(t *testing.T) {
	cert, key := "/h/c.crt", "/h/c.key"
	for _, tt := range []struct {
		name  string
		goos  string
		env   []string
		facts setupFacts
		want  string
	}{
		{name: "half a pair", env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=a.example", "SEAMLESS_TLS_CERT=" + cert},
			want: "SEAMLESS_TLS_CERT and SEAMLESS_TLS_KEY are a certificate and its key: set both, or neither"},
		{name: "a pair with no name", env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_TLS_CERT=" + cert, "SEAMLESS_TLS_KEY=" + key},
			want: "are the certificate of the name SEAMLESS_SERVER_NAME gives: set it too"},
		{name: "a name without SEAMLESS_SETUP=server", env: []string{"SEAMLESS_SERVER_NAME=a.example"},
			want: "SEAMLESS_SERVER_NAME answers the Server's address, which this run does not set up: set SEAMLESS_SETUP=server too"},
		{name: "a name for a Standalone", env: []string{"SEAMLESS_SETUP=standalone", "SEAMLESS_SERVER_NAME=a.example"},
			want: "set SEAMLESS_SETUP=server too"},
		{name: "a name for a Client", env: []string{"SEAMLESS_SERVER_URL=https://box.example:8081", "SEAMLESS_MCP_API_KEY=k", "SEAMLESS_SERVER_NAME=a.example"},
			want: "a Client has no address of its own (its Server's is SEAMLESS_SERVER_URL)"},
		{name: "a name for the Server already set up", facts: setupFactsServer, env: []string{"SEAMLESS_SERVER_NAME=a.example"},
			want: "this computer is the Server already, and setup does not change its address"},
		{name: "a name on a re-run that keeps a Standalone", facts: setupFactsStandalone, env: []string{"SEAMLESS_SERVER_NAME=a.example"},
			want: "set SEAMLESS_SETUP=server too"},
		{name: "Windows", goos: "windows", env: []string{"SEAMLESS_SERVER_NAME=a.example"}, want: "Windows has no peer_auth and no relay"},
		{name: "an IP address", env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=192.168.1.9"}, want: "an IP address: give the name it answers to"},
		{name: "localhost", env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=localhost"}, want: "localhost names this computer only"},
		{name: "a URL", env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=https://a.example"}, want: "a name only, like seamless.example.com"},
		{name: "a port", env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=a.example:8081"}, want: "a name only, without a port"},
		{name: "not a host name", env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=a_b.example"}, want: "not a host name"},
		{name: "a relative certificate", env: []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=a.example", "SEAMLESS_TLS_CERT=c.crt", "SEAMLESS_TLS_KEY=" + key},
			want: "SEAMLESS_TLS_CERT=c.crt: not a full path"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			goos := tt.goos
			if goos == "" {
				goos = "linux"
			}
			facts := tt.facts
			if facts.configPath == "" {
				facts = setupFactsFresh
			}
			r, _, wrote := setupTestRun(t, goos, tt.env, facts, false, "")
			_, err := setupDecide(r)
			require.ErrorIs(t, err, errSetupRefused)
			require.ErrorContains(t, err, tt.want)
			require.Empty(t, *wrote)
		})
	}
	// Each one the installer's run would refuse, setup run by hand on a
	// computer with nothing installed refuses too.
	r, _, _ := setupTestRun(t, "linux", []string{"SEAMLESS_SERVER_NAME=a.example"}, setupFactsFresh, false, "")
	require.ErrorContains(t, setupDirect(r), "set SEAMLESS_SETUP=server too")
	r, out, _ := setupTestRun(t, "linux", []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=a.example"}, setupFactsFresh, false, "")
	require.NoError(t, setupDirect(r))
	require.Contains(t, out.String(), "nothing is set up on this computer yet")
}

// A computer already set up keeps its config as the owner wrote it: setup
// prints the lines for it, and writes none.
func TestSetupAddress_NeverEditsAConfig(t *testing.T) {
	goos := setupAddrUnix(t)
	cert, key := setupAddrPair(t, setupAddrDomain)
	env := []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=" + setupAddrDomain, "SEAMLESS_TLS_CERT=" + cert, "SEAMLESS_TLS_KEY=" + key}
	r, _, _ := setupTestRun(t, goos, env, setupFactsStandalone, false, "")
	w := setupAddrArrange(t, r, map[string][]string{setupAddrDomain: {setupAddrLANv4}}, setupAddrIfaces, tailscaleStatus{}, errNoTailscale)
	r.facts.configFound = true
	r.ensureServer = func(config.ServerAddress) (string, error) {
		t.Fatal("setup wrote over a config that exists")
		return "", nil
	}
	out, st, err := setupAddrFirstHalf(t, r)
	require.NoError(t, err, out)
	require.Equal(t, "server", st.change)
	require.Empty(t, w.wrote)
	require.Contains(t, out, "Standalone, becoming the Server")
	require.Contains(t, out, "  config       setup does not edit "+tildePath(r.facts.configPath)+", which is yours")
	require.Contains(t, out, setupRowCont+`addr: "`+setupAddrLANv4+`:8081"`+"\n")
	require.Contains(t, out, setupRowCont+`server_url: "https://`+setupAddrDomain+`:8081"`+"\n")
	require.Contains(t, out, setupRowCont+`  cert_file: "`+cert+`"`+"\n")
	require.Contains(t, out, "then run the installer again: it restarts the daemon and wires the hooks for https")
	require.Contains(t, out, setupInstallCommand(goos))

	// One that already says it is left alone, and said so.
	r, _, _ = setupTestRun(t, goos, env, setupFactsStandalone, false, "")
	setupAddrArrange(t, r, map[string][]string{setupAddrDomain: {setupAddrLANv4}}, setupAddrIfaces, tailscaleStatus{}, errNoTailscale)
	r.facts.configFound = true
	r.loadConfig = func() (config.Config, error) {
		cfg := config.Defaults()
		cfg.Addr, cfg.AdvertisedURL, cfg.TLS.CertFile, cfg.TLS.KeyFile = setupAddrLANv4+":8081", "https://"+setupAddrDomain+":8081", cert, key
		return cfg, nil
	}
	out, _, err = setupAddrFirstHalf(t, r)
	require.NoError(t, err, out)
	require.Contains(t, out, "  config       already says this")
	require.NotContains(t, out, "setup does not edit")

	// A computer set up already whose config is gone -- its service, or its
	// data, is there -- gets no config written either: every run of
	// seamlessd update is on such a computer.
	for _, facts := range []setupFacts{{userService: true}, {localData: true}} {
		r, _, _ = setupTestRun(t, goos, env, facts, false, "")
		w = setupAddrArrange(t, r, map[string][]string{setupAddrDomain: {setupAddrLANv4}}, setupAddrIfaces, tailscaleStatus{}, errNoTailscale)
		r.ensureServer = func(config.ServerAddress) (string, error) {
			t.Fatal("setup wrote a config on a computer set up already")
			return "", nil
		}
		out, _, err = setupAddrFirstHalf(t, r)
		require.NoError(t, err, out)
		require.Contains(t, out, "setup writes a config only on a first install, and this computer has Seamless already")
		require.Contains(t, out, setupRowCont+`addr: "`+setupAddrLANv4+`:8081"`+"\n")
		require.Empty(t, w.wrote)
	}
}

// With locked projects on, the daemon's config and its TLS pair are its own
// account's: setup runs no root step and writes nothing, and says exactly
// what root does -- the pair into the guide's directory, the key readable by
// the daemon's account alone, the daemon's config, the restart.
func TestSetupAddress_PeerAuth(t *testing.T) {
	goos := setupAddrUnix(t)
	t.Run("the tailnet's name: root mints the certificate where only the daemon's account reads it", func(t *testing.T) {
		r, _, _ := setupTestRun(t, goos, []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=" + setupAddrTSName}, setupFactsLocked, false, "")
		w := setupAddrArrange(t, r, map[string][]string{setupAddrTSName: {setupAddrTailv4, setupAddrTailv6}}, setupAddrIfaces, setupAddrTailnetUp, nil)
		r.facts.configFound = true
		r.daemonAccount = func() string { return "_example" }
		out, _, err := setupAddrFirstHalf(t, r)
		require.NoError(t, err, out)
		require.Empty(t, w.minted, "the owner's account mints no key the daemon would serve")
		require.Empty(t, w.wrote)
		crt, k := "/opt/seamless/etc/tls/"+setupAddrTSName+".crt", "/opt/seamless/etc/tls/"+setupAddrTSName+".key"
		for _, want := range []string{
			"  root steps   locked projects are on",
			"sudo install -d -o root -g _example -m 0750 /opt/seamless/etc/tls",
			"sudo tailscale cert --cert-file " + crt + " --key-file " + k + " " + setupAddrTSName,
			"sudo chown root:_example " + crt + " " + k + " && sudo chmod 0644 " + crt + " && sudo chmod 0640 " + k,
			"then put these in the daemon's config (/opt/seamless/etc/seamless.yaml), replacing any it has:",
			`addr: "` + setupAddrTailv4 + `:8081"`,
			`cert_file: "` + crt + `"`,
			`and in the console's config (~/.config/seamless/console.yaml), server_url: "https://` + setupAddrTSName + `:8081"`,
			"the tailnet issues certificates for " + setupAddrTSName + " (tailscale cert)",
		} {
			require.Contains(t, out, want)
		}
		restart := "sudo systemctl restart seamless-daemon"
		if goos == "darwin" {
			restart = "sudo launchctl kickstart -k system/com.arctop.seamless.daemon"
		}
		require.Contains(t, out, restart)
	})
	t.Run("files of the owner's: root installs them, and the owner deletes the key from the home", func(t *testing.T) {
		cert, key := setupAddrPair(t, setupAddrDomain)
		r, _, _ := setupTestRun(t, goos, []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=" + setupAddrDomain,
			"SEAMLESS_TLS_CERT=" + cert, "SEAMLESS_TLS_KEY=" + key}, setupFactsLocked, false, "")
		w := setupAddrArrange(t, r, map[string][]string{setupAddrDomain: {setupAddrLANv4}}, setupAddrIfaces, tailscaleStatus{}, errNoTailscale)
		r.facts.configFound = true
		out, _, err := setupAddrFirstHalf(t, r)
		require.NoError(t, err, out)
		require.Empty(t, w.wrote)
		require.Contains(t, out, "sudo install -o root -g _seamless -m 0644 "+cert+" /opt/seamless/etc/tls/"+setupAddrDomain+".crt")
		require.Contains(t, out, "sudo install -o root -g _seamless -m 0640 "+key+" /opt/seamless/etc/tls/"+setupAddrDomain+".key")
		require.Contains(t, out, "and delete "+tildePath(key)+" once it serves")
		require.Contains(t, out, "  key          /opt/seamless/etc/tls/"+setupAddrDomain+".key: the daemon's account reads it, and no other")
	})
}

// At the terminal: the tailnet's name is the default when the tailnet can
// issue its certificate; a domain asks for the name and the files again until
// they are a pair for it; a name on the LAN shows how to make the
// certificate, and takes the paths those commands write.
func TestSetupAddress_AtTheTerminal(t *testing.T) {
	goos := setupAddrUnix(t)
	hosts := map[string][]string{setupAddrTSName: {setupAddrTailv4, setupAddrTailv6}, setupAddrDomain: {setupAddrLANv4},
		"here.local": {setupAddrLANv4}}

	t.Run("Enter takes the tailnet's name", func(t *testing.T) {
		r, out, _ := setupTestRun(t, goos, nil, setupFactsFresh, true, "2\n1\n\n")
		w := setupAddrArrange(t, r, hosts, setupAddrIfaces, setupAddrTailnetUp, nil)
		r.loadConfig = func() (config.Config, error) { // the import step's question: Enter starts fresh
			cfg := config.Defaults()
			cfg.DataDir = filepath.Join(w.dir, "data")
			return cfg, nil
		}
		text, _, err := setupAddrFirstHalf(t, r)
		require.NoError(t, err, text)
		require.Contains(t, out.String(), "Where will the other computers reach this one?")
		require.Contains(t, out.String(), "[1] Tailscale   "+setupAddrTSName)
		require.Contains(t, out.String(), "Enter 1-3 [1]: ")
		require.Equal(t, []string{setupAddrTSName}, w.minted)
		require.Len(t, w.wrote, 1)
	})

	t.Run("a domain, asked again until it resolves and its files are its pair", func(t *testing.T) {
		cert, key := setupAddrPair(t, setupAddrDomain)
		other, _ := setupAddrPair(t, "elsewhere.example.com")
		input := "2\n1\n2\n" + "gone.example\n" + setupAddrDomain + "\n" + other + "\n" + key + "\n" + "relative.crt\n" + cert + "\n" + key + "\n" + "\n"
		r, out, _ := setupTestRun(t, goos, nil, setupFactsFresh, true, input)
		w := setupAddrArrange(t, r, hosts, setupAddrIfaces, setupAddrTailnetUp, nil)
		text, _, err := setupAddrFirstHalf(t, r)
		require.NoError(t, err, text)
		said := out.String()
		require.Contains(t, said, "gone.example does not resolve on this computer")
		require.Contains(t, said, "the certificate and key do not load as a pair")
		require.Contains(t, said, "not a full path")
		require.Empty(t, w.minted)
		require.Equal(t, []config.ServerAddress{{Addr: setupAddrLANv4 + ":8081", ServerURL: "https://" + setupAddrDomain + ":8081",
			CertFile: cert, KeyFile: key}}, w.wrote)
	})

	t.Run("a name on the LAN, with no tailnet here: the commands, then their paths", func(t *testing.T) {
		r, out, _ := setupTestRun(t, goos, nil, setupFactsFresh, true, "")
		w := setupAddrArrange(t, r, hosts, setupAddrIfaces, tailscaleStatus{}, errNoTailscale)
		cert, key := filepath.Join(w.dir, "tls", "here.local.crt"), filepath.Join(w.dir, "tls", "here.local.key")
		setupAddrWritePair(t, cert, key, time.Now().Add(825*24*time.Hour), "here.local")
		r.in = bufio.NewReader(strings.NewReader("2\n1\n2\n\n\n\n\n"))
		text, _, err := setupAddrFirstHalf(t, r)
		require.NoError(t, err, text)
		said := out.String()
		require.Contains(t, said, "Tailscale: "+errNoTailscale.Error()+".")
		require.Contains(t, said, "[1] Domain")
		require.Contains(t, said, "[2] LAN")
		require.Contains(t, said, "Enter 1 or 2: ")
		require.Contains(t, said, "Name (this computer's name on your LAN) [here.local]: ")
		require.Contains(t, said, "mkcert -cert-file "+cert+" -key-file "+key+" here.local")
		require.Contains(t, said, `-subj "/CN=here.local"`)
		require.Contains(t, said, "Certificate file ["+cert+"]: ")
		require.Equal(t, []config.ServerAddress{{Addr: setupAddrLANv4 + ":8081", ServerURL: "https://here.local:8081",
			CertFile: cert, KeyFile: key}}, w.wrote)
	})

	t.Run("the end of input ends the files' question, its defaults no pair", func(t *testing.T) {
		r, _, _ := setupTestRun(t, goos, nil, setupFactsFresh, true, "2\n1\n2\n\n")
		w := setupAddrArrange(t, r, hosts, setupAddrIfaces, tailscaleStatus{}, errNoTailscale)
		_, _, err := setupAddrFirstHalf(t, r)
		require.ErrorIs(t, err, errSetupRefused)
		require.ErrorContains(t, err, "the certificate and key do not load as a pair")
		require.ErrorContains(t, err, "set SEAMLESS_TLS_CERT and SEAMLESS_TLS_KEY for a run without questions")
		require.Empty(t, w.wrote)
	})

	t.Run("no answer names the knobs", func(t *testing.T) {
		r, _, _ := setupTestRun(t, goos, nil, setupFactsFresh, true, "2\n1\n")
		w := setupAddrArrange(t, r, hosts, setupAddrIfaces, tailscaleStatus{}, errNoTailscale)
		_, _, err := setupAddrFirstHalf(t, r)
		require.ErrorIs(t, err, errSetupRefused)
		require.ErrorContains(t, err, "no answer: set SEAMLESS_SETUP=server, SEAMLESS_SERVER_NAME, SEAMLESS_TLS_CERT and SEAMLESS_TLS_KEY")
		require.Empty(t, w.wrote)
	})
}

// The config is written once every step of the first half has run: an import
// that stops leaves none behind, so the next run is still a first install,
// and an address that cannot be set up stops the run before the import.
func TestSetupAddress_TheConfigComesAfterTheImport(t *testing.T) {
	setupAddrUnix(t)
	cert, key := setupAddrPair(t, setupAddrDomain)
	locked := impBackup(t, impSpec{locked: true})
	address := []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=" + setupAddrDomain, "SEAMLESS_TLS_CERT=" + cert, "SEAMLESS_TLS_KEY=" + key}
	hosts := map[string][]string{setupAddrDomain: {setupAddrLANv4}}

	t.Run("an import that stops leaves no config", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "data")
		r, _ := setupImportTestRun(t, append(address, "SEAMLESS_IMPORT="+locked), false, "", dest)
		w := setupAddrArrange(t, r, hosts, setupAddrIfaces, tailscaleStatus{}, errNoTailscale)
		r.facts = setupFactsFresh
		r.facts.configPath = filepath.Join(w.dir, "seamless.yaml")
		_, _, err := setupAddrFirstHalf(t, r)
		require.ErrorIs(t, err, errSetupRefused)
		require.Empty(t, w.wrote, "the import stopped, so the run is still a first install's")
		require.NoDirExists(t, dest)
	})
	t.Run("an import that lands, then the config", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "data")
		r, _ := setupImportTestRun(t, append(address, "SEAMLESS_IMPORT="+locked, "SEAMLESS_IMPORT_LOCKS=keep"), false, "", dest)
		w := setupAddrArrange(t, r, hosts, setupAddrIfaces, tailscaleStatus{}, errNoTailscale)
		r.facts = setupFactsFresh
		r.facts.configPath = filepath.Join(w.dir, "seamless.yaml")
		out, _, err := setupAddrFirstHalf(t, r)
		require.NoError(t, err, out)
		impRestored(t, dest)
		require.Len(t, w.wrote, 1)
		require.Less(t, strings.Index(out, "  imported "), strings.Index(out, "  config       wrote"))
	})
	t.Run("an address that cannot be set up stops the run before the import", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "data")
		r, _ := setupImportTestRun(t, []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=gone.example", "SEAMLESS_TLS_CERT=" + cert,
			"SEAMLESS_TLS_KEY=" + key, "SEAMLESS_IMPORT=" + locked, "SEAMLESS_IMPORT_LOCKS=keep"}, false, "", dest)
		w := setupAddrArrange(t, r, hosts, setupAddrIfaces, tailscaleStatus{}, errNoTailscale)
		r.facts = setupFactsFresh
		r.facts.configPath = filepath.Join(w.dir, "seamless.yaml")
		_, _, err := setupAddrFirstHalf(t, r)
		require.ErrorContains(t, err, "gone.example does not resolve on this computer")
		require.NoDirExists(t, dest, "nothing was imported")
	})
}

// The Server's closing: what each Client needs, from the config the
// installer's own steps run with.
func TestSetupClientsStep(t *testing.T) {
	r, out, _ := setupTestRun(t, "linux", nil, setupFactsFresh, false, "")
	require.NoError(t, setupClientsStep(r, &setupState{}))
	require.Contains(t, out.String(), "This Server serves no other computer yet")

	cert, key := writeCertPair(t, time.Now().Add(24*time.Hour), setupAddrDomain)
	r, out, _ = setupTestRun(t, "linux", nil, setupFactsFresh, false, "")
	r.loadConfig = func() (config.Config, error) {
		cfg := config.Defaults()
		cfg.Addr, cfg.AdvertisedURL, cfg.TLS.CertFile, cfg.TLS.KeyFile = setupAddrLANv4+":8081", "https://"+setupAddrDomain+":8081", cert, key
		return cfg, nil
	}
	require.NoError(t, setupClientsStep(r, &setupState{}))
	fp, err := certFingerprintFile(cert)
	require.NoError(t, err)
	for _, want := range []string{"To connect another computer to this server:", "Address:                 https://" + setupAddrDomain + ":8081",
		"Certificate fingerprint: " + fp, "seamlessd devices code", "macOS and Linux (pairs a relay):", "The code is the approval"} {
		require.Contains(t, out.String(), want)
	}

	r, out, _ = setupTestRun(t, "linux", nil, setupFactsLocked, false, "")
	r.daemonAccount = func() string { return "_example" }
	r.loadConfig = func() (config.Config, error) {
		cfg := config.Defaults()
		cfg.Role, cfg.AdvertisedURL = config.RoleClient, "unix:///opt/seamless/run/seamlessd.sock"
		return cfg, nil
	}
	require.NoError(t, setupClientsStep(r, &setupState{}))
	require.Contains(t, out.String(), "sudo -u _example env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd devices code")
}

func TestSetupServerNameProblem(t *testing.T) {
	for name, want := range map[string]string{
		"seamless.example.com":   "",
		"Studio.Example.TS.NET.": "",
		"studio":                 "",
		"":                       "no name",
		"10.0.0.5":               "an IP address",
		"[fd00::1]":              "an IP address",
		"host:8081":              "without a port",
		"https://a.example":      "no scheme",
		"a.example/path":         "no scheme",
		"localhost":              "localhost names this computer only",
		"app.localhost":          "localhost names this computer only",
		"a b.example":            "a space",
		"-a.example":             "not a host name",
		"a_b.example":            "not a host name",
	} {
		got := setupServerNameProblem(name)
		if want == "" {
			require.Empty(t, got, name)
			continue
		}
		require.Contains(t, got, want, name)
	}
	require.Equal(t, "studio.example.ts.net", setupNormalName(" Studio.Example.TS.NET. "))
	require.Equal(t, "here.local", setupLANDefault("HERE"))
	require.Equal(t, "box.example.com", setupLANDefault("box.example.com"))
	require.Empty(t, setupLANDefault(""))
	require.Equal(t, "8081", setupAddrPort(""))
	require.Equal(t, "9443", setupAddrPort("127.0.0.1:9443"))
	require.Equal(t, "8081", setupAddrPort("127.0.0.1:0"))
}

func TestParseTailscaleStatus(t *testing.T) {
	st, err := parseTailscaleStatus([]byte(`{"BackendState":"Running","Self":{"DNSName":"Studio.Example.ts.net.",` +
		`"TailscaleIPs":["100.64.0.7","fd7a:115c:a1e0::7","bogus"]},"CertDomains":["studio.example.ts.net"]}`))
	require.NoError(t, err)
	require.Equal(t, tailscaleStatus{running: true, name: setupAddrTSName, addrs: addrs(setupAddrTailv4, setupAddrTailv6),
		certDomains: []string{setupAddrTSName}}, st)
	require.True(t, st.issues("STUDIO.example.ts.net."))
	require.False(t, st.issues("other.example.ts.net"))
	require.Empty(t, st.usable())

	stopped, err := parseTailscaleStatus([]byte(`{"BackendState":"Stopped","Self":{"DNSName":""}}`))
	require.NoError(t, err)
	require.Contains(t, stopped.usable(), "not running")
	noName := tailscaleStatus{running: true}
	require.Contains(t, noName.usable(), "turn on MagicDNS")
	noCerts := tailscaleStatus{running: true, name: setupAddrTSName}
	require.Contains(t, noCerts.usable(), "turn on HTTPS certificates")
	_, err = parseTailscaleStatus([]byte("not json"))
	require.Error(t, err)
}
