package main

// The address step's root step under locked projects, as text: the staged
// copy rootstepCopyCommands prints for any file root copies from one of the
// owner's, the plan's own check of every value, and the pair setup reads once.
// The printed step's runs are in addrstep_review_unix_test.go.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A copy is six commands, each value quoted and each tool by its full path:
// root makes the staged copy empty and 0600, tee fills it from what the
// owner's shell opened, the check, then owner and mode, then the rename.
func TestAddrStepCopyCommands(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	for goos, tools := range map[string][2]string{
		"darwin": {"/usr/sbin/chown", "/usr/bin/shasum -a 256"},
		"linux":  {"/bin/chown", "/usr/bin/sha256sum"},
	} {
		got := rootstepCopyCommands(rootstepToolsFor(goos), rootstepCopy{source: "/h/my pair/it's.key", sha256: sum,
			staged: "/opt/x/.k.new", dest: "/opt/x/k", owner: "root", group: "_example", mode: "0640"})
		require.Equal(t, []string{
			"/usr/bin/sudo /usr/bin/install -m 0600 /dev/null /opt/x/.k.new",
			`/usr/bin/sudo /usr/bin/tee /opt/x/.k.new >/dev/null < '/h/my pair/it'\''s.key'`,
			"echo '" + sum + "  /opt/x/.k.new' | /usr/bin/sudo " + tools[1] + " -c",
			"/usr/bin/sudo " + tools[0] + " root:_example /opt/x/.k.new",
			"/usr/bin/sudo /bin/chmod 0640 /opt/x/.k.new",
			"/usr/bin/sudo /bin/mv -f /opt/x/.k.new /opt/x/k",
		}, got, goos)
	}
	// The tailnet's command is root's, where its own packages put it.
	require.Equal(t, "/Applications/Tailscale.app/Contents/MacOS/Tailscale", rootstepToolsFor("darwin").tailscale)
	require.Equal(t, "/usr/bin/tailscale", rootstepToolsFor("linux").tailscale)
}

// addrstepPlan is a TLS root plan from the owner's files whose every value
// holds its format.
func addrstepPlan(goos string) setupTLSRootPlan {
	return setupTLSRootPlan{goos: goos, acct: "_example", name: setupAddrDomain, source: setupAddrFiles,
		cert: "/h/tls/server.crt", key: "/h/tls/server.key", certSHA: strings.Repeat("0a", 32), keySHA: strings.Repeat("0b", 32)}
}

// The step prints only when every value holds its format.
func TestAddrStepTLSRootPlanProblem(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		require.Empty(t, addrstepPlan(goos).problem(), goos)
	}
	tailnet := setupTLSRootPlan{goos: "linux", acct: "_seamless", name: setupAddrTSName, source: setupAddrTailscale}
	require.Empty(t, tailnet.problem(), "the tailnet's pair has no files or hashes to judge")
	for name, change := range map[string]func(*setupTLSRootPlan){
		"windows":               func(p *setupTLSRootPlan) { p.goos = "windows" },
		"no account":            func(p *setupTLSRootPlan) { p.acct = "" },
		"an option as account":  func(p *setupTLSRootPlan) { p.acct = "-x" },
		"a hostile account":     func(p *setupTLSRootPlan) { p.acct = "_a;id" },
		"a hostile name":        func(p *setupTLSRootPlan) { p.name = "a$(id).example" },
		"a name not normalized": func(p *setupTLSRootPlan) { p.name = "Seamless.Example.com." },
		"a relative certificate": func(p *setupTLSRootPlan) {
			p.cert = "server.crt"
		},
		"a control in the key": func(p *setupTLSRootPlan) { p.key = "/h/tls/server\x1b.key" },
		"no hash of the key":   func(p *setupTLSRootPlan) { p.keySHA = "" },
		"a short hash":         func(p *setupTLSRootPlan) { p.certSHA = "00" },
		"an upper-case hash":   func(p *setupTLSRootPlan) { p.certSHA = strings.Repeat("0A", 32) },
	} {
		p := addrstepPlan("linux")
		change(&p)
		require.NotEmpty(t, p.problem(), name)
	}
}

// setup prints no root step from an account name that is not one: the
// commands quote it as well, but a wrong value is not printed at all.
func TestAddrStepRefusesToPrintFromBadValues(t *testing.T) {
	goos := setupAddrUnix(t)
	cert, key := setupAddrPair(t, setupAddrDomain)
	r, _, _ := setupTestRun(t, goos, []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=" + setupAddrDomain,
		"SEAMLESS_TLS_CERT=" + cert, "SEAMLESS_TLS_KEY=" + key}, setupFactsLocked, false, "")
	setupAddrArrange(t, r, map[string][]string{setupAddrDomain: {setupAddrLANv4}}, setupAddrIfaces, tailscaleStatus{}, errNoTailscale)
	r.facts.configFound = true
	r.daemonAccount = func() string { return "_seamless; id" }
	out, _, err := setupAddrFirstHalf(t, r)
	require.ErrorIs(t, err, errSetupRefused)
	require.ErrorContains(t, err, `setup prints no root step from these values: "_seamless; id" is not an account name`)
	require.NotContains(t, out, "/usr/bin/sudo")
}

// The pair setup checks is the pair it hashes: each file read once, whole,
// a chain included, and only a regular file.
func TestAddrStepCheckPairHashesWhatItRead(t *testing.T) {
	cert, key := setupAddrPair(t, setupAddrDomain)
	// A chain: the leaf, then another certificate after it.
	other, _ := setupAddrPair(t, "intermediate.example")
	leafPEM, err := os.ReadFile(cert)
	require.NoError(t, err)
	extra, err := os.ReadFile(other)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cert, append(leafPEM, extra...), 0o644))

	read, problem := setupCheckPair(setupAddrDomain, cert, key, time.Now())
	require.Empty(t, problem)
	require.NotNil(t, read.leaf)
	require.Equal(t, addrstepSHA256File(t, cert), read.certSHA, "the whole chain, as root's copy of it will be")
	require.Equal(t, addrstepSHA256File(t, key), read.keySHA)
	keyPEM, err := os.ReadFile(key)
	require.NoError(t, err)
	sum := sha256.Sum256(keyPEM)
	require.Equal(t, hex.EncodeToString(sum[:]), read.keySHA)

	_, problem = setupCheckPair(setupAddrDomain, filepath.Dir(cert), key, time.Now())
	require.Contains(t, problem, "is not a regular file")
	_, problem = setupCheckPair(setupAddrDomain, cert, filepath.Join(t.TempDir(), "gone.key"), time.Now())
	require.Contains(t, problem, "the certificate and key do not load as a pair")
}

// A path in a command the owner runs is quoted, and a ~/ in front stays the
// home.
func TestAddrStepHomeArg(t *testing.T) {
	for in, want := range map[string]string{
		"~/.config/seamless/tls":       "~/.config/seamless/tls",
		"~/my certs/x.crt":             "~/'my certs/x.crt'",
		"/srv/my certs/x.crt":          "'/srv/my certs/x.crt'",
		"/srv/tls/x.crt":               "/srv/tls/x.crt",
		"~/a'b":                        `~/'a'\''b'`,
		"~user/x":                      "'~user/x'",
		"/x/$(id)":                     "'/x/$(id)'",
		"~/.config/seamless/tls/a.crt": "~/.config/seamless/tls/a.crt",
	} {
		require.Equal(t, want, setupHomeArg(in), in)
	}
}
