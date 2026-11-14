package main

// setup's Client steps (plan networked-relay; task 01M73RGB4PQVSXCN4V8VT5XNBX):
// a Client points at its Server, shows the Server's certificate fingerprint
// for the owner to compare with what the Server's setup printed, and pairs
// with a one-time code. It never holds the Server's own key.
//
//   - macOS and Linux pair a relay (setupDeviceRelay): `seamlessd relay`, run
//     as a system account of its own, serves the socket agents dial and names
//     each caller to the Server by its uid. Its install is one root step, which
//     setup prints and does not run (relay_install.go): setup never runs a
//     command as root, so an unattended run and a run at a terminal alike end
//     with the same commands for the owner to read and run, and doctor's relay
//     rows check what they did. The owner's config points at the relay's socket
//     (config.EnsureSocketClientConfig), with no key.
//   - Windows, and a computer where the owner has no root (a container, CI),
//     pair a key device (setupDeviceKey): the pairing's answer is a key of this
//     computer's own, which the Server's Devices page lists and revokes, written
//     with role: client and server_url at 0600 (config.EnsureDeviceClientConfig).
//     A key device never opens a locked project, and setup says so.
//
// A Client never imports a backup: its data is its Server's (setupImportKnobs).
// SEAMLESS_SERVER_URL with SEAMLESS_MCP_API_KEY keeps the meaning it always
// had: a Client of the Server's shared key, which pairs nothing.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcptransport "github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/relay"
)

// The pairing's knobs, beside SEAMLESS_PAIR_CODE: the Server certificate's
// fingerprint, as its setup printed it (compared before the code is sent, and
// the certificate this computer then trusts the Server by when its own roots
// do not), and what a macOS or Linux Client pairs as (relay, the default, or
// key, for a computer where the owner has no root).
const (
	setupKnobFingerprint = "SEAMLESS_SERVER_FINGERPRINT"
	setupKnobPairAs      = "SEAMLESS_PAIR_AS"
)

// What a Client pairs as.
const (
	setupDeviceRelay = "relay"
	setupDeviceKey   = "key"
)

// setupPairTimeout bounds a key device's pairing: the Server holds a pairing
// that waits for the owner's passkey ten minutes, then expires it.
const setupPairTimeout = 12 * time.Minute

// clientServerCert is the certificate a Server presented, as setup shows it.
type clientServerCert struct {
	// tls is false for a plain http Server: no certificate to compare.
	tls bool
	// fingerprint is the leaf's devices.CertFingerprint, pem the leaf.
	fingerprint string
	pem         []byte
	// trusted: this computer's own roots verify it for the address's host;
	// why says why not.
	trusted bool
	why     string
}

// clientHost is the slice of the machine a Client's setup reads and writes,
// injected so tests stand none of it up for real.
type clientHost struct {
	// serverCert reads the certificate the Server at url presents.
	serverCert func(ctx context.Context, url string) (clientServerCert, error)
	// pair pairs a key device with the server at url (devices.Pair), trusting
	// the certificate copy caFile beside this computer's roots when it is set.
	pair func(ctx context.Context, url, caFile string, req devices.PairRequest) (devices.Paired, error)
	// configTarget is config.ClientConfigTarget: the config the search finds
	// today, and where a new one goes.
	configTarget func() (existing, path string, err error)
	// ensureDevice and ensureSocket write the two Client configs
	// (config.EnsureDeviceClientConfig, config.EnsureSocketClientConfig).
	ensureDevice func(serverURL, key, caFile string) (string, error)
	ensureSocket func() (string, error)
	// writeFile writes a certificate this computer trusts the Server by, and
	// readFile reads it back for the relay's root step (rootstepReadFile).
	writeFile func(path string, data []byte) error
	readFile  func(path string) ([]byte, error)
	// exe is the seamlessd setup runs as, which the relay's root step copies.
	exe func() (string, error)
	// owner is the account this runs as, the agents', and ownerUID its uid.
	owner    func() string
	ownerUID func() int
	// accounts reads what the relay's root step finds: its account and group
	// there already, the ids a macOS one is made with, and root's pin naming
	// its account.
	accounts func() clientAccounts
	// check is the closing check, against the Client config at cfgPath.
	check func(ctx context.Context, w io.Writer, cfgPath string) error
	// relayServing reports whether the relay's socket answers a dial.
	relayServing func() bool
}

// clientAccounts is what the relay's root step finds of its accounts.
type clientAccounts struct {
	accountExists, groupExists bool
	uid, gid                   int
	pinned                     bool
}

// realClientHost is this machine.
func realClientHost() clientHost {
	return clientHost{
		serverCert:   clientFetchServerCert,
		pair:         clientPair,
		configTarget: config.ClientConfigTarget,
		ensureDevice: config.EnsureDeviceClientConfig,
		ensureSocket: config.EnsureSocketClientConfig,
		writeFile: func(path string, data []byte) error {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			return os.WriteFile(path, data, 0o600)
		},
		readFile: rootstepReadFile,
		exe:      os.Executable,
		owner: func() string {
			if u, err := user.Current(); err == nil {
				return u.Username
			}
			return os.Getenv("USER")
		},
		ownerUID:     os.Getuid,
		accounts:     clientRealAccounts,
		check:        clientClosingCheck(""),
		relayServing: func() bool { return clientSocketAnswers(relay.DefaultSocketPath) },
	}
}

// setupPairingKnobs refuses the pairing's knobs where setup cannot do what
// they ask, before anything is decided: a code with the Server's own key, or
// with no Server; a fingerprint or a kind with no code; a fingerprint that is
// not one; a kind that is not one, or a relay on Windows.
func setupPairingKnobs(r *setupRun, want setupKind) error {
	code, fp, as := r.env(setupKnobPairCode), r.env(setupKnobFingerprint), r.env(setupKnobPairAs)
	if code == "" {
		for _, k := range []struct{ knob, v string }{{setupKnobFingerprint, fp}, {setupKnobPairAs, as}} {
			if k.v != "" {
				return setupRefusal("%s answers a pairing, and %s names no code to pair with: set it too (the {server}'s Devices page, "+
					"or seamlessd devices code there, gives one)", k.knob, setupKnobPairCode)
			}
		}
		return nil
	}
	switch {
	case r.env(setupKnobKey) != "":
		return setupRefusal("%s and %s are both set: a pairing code gives this computer a key of its own, in place of the {server}'s; drop one",
			setupKnobPairCode, setupKnobKey)
	case want != setupNone && want != setupClient:
		return setupRefusal("%s=%s and %s disagree: a pairing code makes this computer a {client}; drop one",
			setupKnobKind, setupWords[want].env, setupKnobPairCode)
	case r.serverKnob() == "":
		return setupRefusal("%s pairs this computer with a {server}: set %s to its address too (https://host:8081)", setupKnobPairCode, setupKnobServer)
	case strings.ContainsFunc(code, setupControl):
		return setupRefusal("%s holds a control character", setupKnobPairCode)
	case fp != "" && !devices.ValidCertFingerprint(fp):
		return setupRefusal("%s=%s is not a certificate's SHA-256 fingerprint (AB:CD:..., 32 bytes, as the {server}'s setup prints it)",
			setupKnobFingerprint, fp)
	}
	switch v := strings.ToLower(strings.TrimSpace(as)); v {
	case "", setupDeviceKey:
	case setupDeviceRelay:
		if !setupPeerAuthOS(r.goos) {
			return setupRefusal("%s=%s: the relay runs on macOS and Linux only; this computer pairs as a key device of its own", setupKnobPairAs, as)
		}
	default:
		return setupRefusal("invalid %s=%q: valid values are relay, key", setupKnobPairAs, as)
	}
	return nil
}

// setupFromPairCode is SEAMLESS_SERVER_URL with SEAMLESS_PAIR_CODE: a Client
// of that Server, paired by the pairing step. A computer that runs a daemon of
// its own is refused, as SEAMLESS_SETUP=client refuses it: a relay would serve
// the very socket that daemon serves, and a key device's config would have to
// replace the daemon's.
func setupFromPairCode(r *setupRun, st setupState, in setupInstall, url string) (setupState, error) {
	if in.kind == setupStandalone || in.kind == setupServer {
		return st, setupRefusal("%s: %s", setupKnobPairCode, setupCannotChange(in, setupClient))
	}
	if p := setupServerURLProblem(url); p != "" {
		return st, setupRefusal("%s=%s: %s", setupKnobServer, url, p)
	}
	st.kind, st.role, st.serverURL, st.answer, st.pairing = setupClient, config.RoleClient, url, setupFromEnv, true
	return st, nil
}

// setupPairingStep is a Client's pairing (setupSteps["pairing"]), before the
// installer's own steps, which wire the hooks against the config it writes:
// the Server's certificate, compared; then a key device paired and its config
// written, or the relay's socket config written for the root step to come.
func setupPairingStep(r *setupRun, st *setupState) error {
	if st.kind != setupClient || !st.pairing {
		return nil // the Server's own key, as before: nothing to pair
	}
	ctx := context.Background()
	server := st.serverURL
	cert, err := r.client.serverCert(ctx, server)
	if err != nil {
		return setupRefusal("cannot reach the {server} at %s to read its certificate: %v -- check the address, and that it serves there", server, err)
	}
	if u, perr := url.Parse(server); !cert.tls && (perr != nil || !isLoopbackServerHost(u.Hostname())) {
		return setupRefusal("%s is plain http, and a pairing crosses the network: the code, and a key of this computer's, would travel in "+
			"the clear. Give the {server} TLS first (its setup's address step), then pair with its https address", server)
	}
	ca, err := setupCompareCert(r, server, cert)
	if err != nil {
		return err
	}
	device, err := setupDeviceKind(r, cert)
	if err != nil {
		return err
	}
	if device == setupDeviceKey {
		return setupPairKey(ctx, r, st, cert, ca)
	}
	return setupPrepareRelay(r, st, cert, ca)
}

// setupCompareCert shows the Server's certificate fingerprint and has it
// compared: by the person at the terminal, or with SEAMLESS_SERVER_FINGERPRINT.
// A certificate this computer's roots do not verify is trusted only once it
// was compared, and then as that certificate alone: the path of the copy it
// is trusted by comes back ("" when the roots verify it).
func setupCompareCert(r *setupRun, server string, cert clientServerCert) (string, error) {
	fmt.Fprintln(r.out)
	if !cert.tls {
		setupRow(r.out, "certificate", "none: "+server+" is plain http")
		fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim("the code, this computer's key and every memory cross the network unencrypted; keep it inside a trusted LAN"))
		return "", nil
	}
	setupRow(r.out, "fingerprint", cert.fingerprint)
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim(setupText("the {server}'s certificate, SHA-256: its setup printed it (Certificate fingerprint), "+
		"and so does seamlessd devices code there")))
	if !cert.trusted {
		fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim("this computer's own roots do not vouch for it ("+cert.why+"): once compared, it trusts this certificate alone"))
	}
	confirmed := false
	switch want := r.env(setupKnobFingerprint); {
	case want != "":
		if !clientSameFingerprint(want, cert.fingerprint) {
			return "", setupRefusal("%s presents %s, not the %s you gave: nothing was sent to it -- check the address, and the fingerprint the {server} printed",
				server, cert.fingerprint, setupKnobFingerprint)
		}
		setupRow(r.out, "", green("matches "+setupKnobFingerprint))
		confirmed = true
	case r.asks():
		n, err := setupChooseOr(r, "Does it match what the "+setupWords[setupServer].name+" printed? [y/N]: ", 2, [][]string{{"y", "yes"}, {"n", "no"}},
			setupRefusal("no answer"))
		if err != nil {
			return "", err
		}
		if n != 1 {
			return "", setupRefusal("the fingerprints differ: nothing was sent to %s. Check the address; a certificate that is not the {server}'s "+
				"is another computer answering for it", server)
		}
		confirmed = true
	default:
		fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim(setupTextf("compare it with what the {server} printed; set %s to have setup compare it", setupKnobFingerprint)))
	}
	if cert.trusted {
		return "", nil
	}
	if !confirmed {
		return "", setupRefusal("this computer does not trust %s's certificate (%s), and nothing compared it: set %s=%s once it matches what the "+
			"{server}'s setup printed, and setup trusts that certificate alone", server, cert.why, setupKnobFingerprint, cert.fingerprint)
	}
	_, path, err := r.client.configTarget()
	if err != nil {
		return "", fmt.Errorf("seamlessd.setup: pairing: %w", err)
	}
	ca := filepath.Join(filepath.Dir(path), "server-ca.crt")
	if err := r.client.writeFile(ca, cert.pem); err != nil {
		return "", fmt.Errorf("seamlessd.setup: pairing: keep the server's certificate: %w", err)
	}
	setupRow(r.out, "trust", "this certificate alone "+dim("-> "+tildePath(ca)))
	return ca, nil
}

// clientSameFingerprint compares two certificate fingerprints as a person
// types them: case and separators aside.
func clientSameFingerprint(a, b string) bool {
	norm := func(s string) string {
		return strings.NewReplacer(":", "", " ", "", "-", "").Replace(strings.ToUpper(strings.TrimSpace(s)))
	}
	return norm(a) != "" && norm(a) == norm(b)
}

// setupDeviceKind is what this Client pairs as: a key device on Windows, or
// over plain http (a relay needs TLS); SEAMLESS_PAIR_AS; at the terminal, the
// person's answer; else a relay.
func setupDeviceKind(r *setupRun, cert clientServerCert) (string, error) {
	as := strings.ToLower(strings.TrimSpace(r.env(setupKnobPairAs)))
	switch {
	case !setupPeerAuthOS(r.goos):
		return setupDeviceKey, nil
	case !cert.tls:
		if as == setupDeviceRelay {
			return "", setupRefusal("%s=relay: a relay pairs over https only, with its own certificate, and this {server} serves plain http", setupKnobPairAs)
		}
		return setupDeviceKey, nil
	case as != "":
		return as, nil
	case !r.asks():
		return setupDeviceRelay, nil
	}
	fmt.Fprintf(r.out, "\n%s\n", bold(setupText("How will this computer reach the {server}?")))
	setupOption(r.out, 1, "Relay", "a small service of its own account: your agents need no key, and locked projects can open to them (one root step)")
	setupOption(r.out, 2, "Key", "a key of this computer's own, for a computer where you have no root (a container, CI); it never opens a locked project")
	n, err := setupChooseOr(r, "Enter 1 or 2 [1]: ", 1, [][]string{{setupDeviceRelay}, {setupDeviceKey}}, setupRefusal("no answer"))
	if err != nil {
		return "", err
	}
	if n == 2 {
		return setupDeviceKey, nil
	}
	return setupDeviceRelay, nil
}

// setupPairCode is the pairing code: SEAMLESS_PAIR_CODE, else the person's
// answer at the terminal, else "".
func setupPairCode(r *setupRun) (string, error) {
	if code := strings.TrimSpace(r.env(setupKnobPairCode)); code != "" || !r.asks() {
		return code, nil
	}
	fmt.Fprintf(r.out, "%s\n", dim(setupText("A pairing code: the {server}'s Devices page gives one, or seamlessd devices code there. It pairs one computer, within 10 minutes.")))
	for {
		fmt.Fprint(r.out, "Pairing code: ")
		line, err := r.in.ReadString('\n')
		code := strings.TrimSpace(line)
		if code != "" && !strings.ContainsFunc(code, setupControl) {
			return code, nil
		}
		if err != nil {
			return "", setupRefusal("no pairing code: set %s=%s, %s and %s for a run without questions",
				setupKnobKind, setupWords[setupClient].env, setupKnobServer, setupKnobPairCode)
		}
	}
}

// setupPairKey pairs this computer as a key device and writes its config: the
// key the pairing answers with goes into the 0600 config and nowhere else.
// Where no config can be written, nothing is paired: a key the Server minted
// and no file took would be a key lost.
func setupPairKey(ctx context.Context, r *setupRun, st *setupState, cert clientServerCert, ca string) error {
	existing, _, err := r.client.configTarget()
	if err != nil {
		return fmt.Errorf("seamlessd.setup: pairing: %w", err)
	}
	if existing != "" {
		return setupRefusal("%s is here already, and setup never edits a config: a key device's key goes into a config of its own. "+
			"Move it aside to pair this computer, or keep it as it is", tildePath(existing))
	}
	code, err := setupPairCode(r)
	if err != nil {
		return err
	}
	if code == "" {
		return setupRefusal("a key device pairs with a code: set %s", setupKnobPairCode)
	}
	setupRow(r.out, "pairing", "as a key device of this computer's own, named "+r.host)
	ctx, cancel := context.WithTimeout(ctx, setupPairTimeout)
	defer cancel()
	paired, err := r.client.pair(ctx, st.serverURL, ca, devices.PairRequest{Code: code, Name: r.host, ServerFingerprint: cert.fingerprint,
		Waiting: func(fp string) {
			setupRow(r.out, "approval", setupText("waiting: approve this computer on the {server}'s Devices page, with your passkey"))
			fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim("the page shows it as "+fp+": approve it only if it shows that"))
		}})
	if err != nil {
		return setupRefusal("the pairing did not finish: %v", err)
	}
	wrote, err := r.client.ensureDevice(st.serverURL, paired.Key, ca)
	if err != nil {
		return fmt.Errorf("seamlessd.setup: the server paired this computer as %s (device %s), but its config could not be written (%w): "+
			"revoke it on the server's Devices page and pair again", paired.Name, paired.DeviceID, err)
	}
	setupRow(r.out, "paired", fmt.Sprintf("%s (device %s): its own key, which the %s's Devices page lists and revokes",
		paired.Name, paired.DeviceID, setupWords[setupServer].name))
	setupRow(r.out, "config", "wrote role, server_url and this computer's key "+dim("-> "+tildePath(wrote)+" (0600)"))
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim("a key device never opens a locked project: work in one from a computer with a relay, or the "+
		setupWords[setupServer].name+"'s own"))
	st.device, st.pairServer, st.fingerprint, st.caFile = setupDeviceKey, st.serverURL, cert.fingerprint, ca
	return nil
}

// setupPrepareRelay readies a relay Client: the owner's config points at the
// relay's socket, with no key, so the installer wires the hooks and MCP to it;
// the pairing itself is made in the root step, as the relay's account, after
// the installer's own steps (setupRelayStep).
func setupPrepareRelay(r *setupRun, st *setupState, cert clientServerCert, ca string) error {
	if !cert.tls {
		return setupRefusal("a relay pairs over https only, with its own certificate, and %s is plain http", st.serverURL)
	}
	wrote, err := r.client.ensureSocket()
	if err != nil {
		if errors.Is(err, config.ErrConfigExists) {
			return setupRefusal("%v: a relay's {client} dials the relay's socket (server_url: %s), with no key -- move the config aside, "+
				"or write that in it by hand", err, config.RelaySocketURL)
		}
		return fmt.Errorf("seamlessd.setup: pairing: %w", err)
	}
	sum, err := clientExeSHA256(r.client.exe)
	if err != nil {
		return fmt.Errorf("seamlessd.setup: pairing: %w", err)
	}
	setupRow(r.out, "relay", setupText("this computer reaches the {server} through a relay of its own"))
	if wrote != "" {
		setupRow(r.out, "config", "wrote role and server_url, the relay's socket "+dim("-> "+tildePath(wrote)))
	} else {
		setupRow(r.out, "config", "already says this: the relay's socket")
	}
	st.device, st.pairServer, st.fingerprint, st.caFile, st.binarySHA = setupDeviceRelay, st.serverURL, cert.fingerprint, ca, sum
	st.serverURL = config.RelaySocketURL
	return nil
}

// clientPair is devices.Pair over config.Config.HTTPClient -- the one trust
// decision every surface makes -- with caFile as its tls.ca_file.
func clientPair(ctx context.Context, url, caFile string, req devices.PairRequest) (devices.Paired, error) {
	cfg := config.Defaults()
	cfg.TLS.CAFile = caFile
	client, err := cfg.HTTPClient(30 * time.Second)
	if err != nil {
		return devices.Paired{}, err
	}
	return devices.Pair(ctx, client, url, req)
}

// clientExeSHA256 is the hex SHA-256 of the binary exe names.
func clientExeSHA256(exe func() (string, error)) (string, error) {
	path, err := exe()
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// setupInstallDir is where the installer puts the binaries: what root's copy
// of seamlessd is copied from.
func setupInstallDir(r *setupRun) string {
	if dir := r.env("SEAMLESS_INSTALL_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(r.home, ".local", "bin")
}

// setupRelayStep is a relay Client's root step (setupSteps["relay"]), after
// the installer's own: what the relay is and why it needs root, then the
// commands, then where approvals happen. It prints them only from values that
// hold their formats (clientRelayPlan.problem) and a certificate that is the
// one compared, whatever the state file says.
func setupRelayStep(r *setupRun, st *setupState) error {
	if st.device != setupDeviceRelay {
		return nil
	}
	codeSet := strings.TrimSpace(r.env(setupKnobPairCode)) != ""
	code, err := setupPairCode(r)
	if err != nil {
		return err
	}
	var caPEM string
	if st.caFile != "" {
		if caPEM, err = rootstepCAPEM(r.client.readFile, st.caFile, st.fingerprint); err != nil {
			return setupRefusal("the {server}'s certificate setup kept, %s, is not the one compared (%v): setup prints no root step "+
				"from it. Run the installer again", tildePath(st.caFile), err)
		}
	}
	acc := r.client.accounts()
	plan := clientRelayPlan{goos: r.goos, owner: r.client.owner(), server: st.pairServer, fingerprint: st.fingerprint, caPEM: caPEM,
		source: filepath.Join(setupInstallDir(r), "seamlessd"), sha256: st.binarySHA,
		accountExists: acc.accountExists, groupExists: acc.groupExists, uid: acc.uid, gid: acc.gid, pinned: acc.pinned}
	if why := plan.problem(); why != "" {
		return setupRefusal("setup prints no root step from these values: %s. Run the installer again", why)
	}
	fmt.Fprintln(r.out)
	setupRow(r.out, "relay", setupText("the relay forwards your agents' requests to the {server}, naming each caller by the uid this computer's"))
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, setupText("kernel reports: it runs as an account of its own, so no process of yours can change what it says,"))
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, "and making that account is its one root step. Setup runs nothing as root: as an administrator, run")
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, "these, reading them first. Paste each block whole: it stops at its first failure.")
	// The commands start at the margin: a here-document's lines, and the line
	// that ends it, must reach the shell as printed.
	for i, b := range clientRelayRootStep(plan) {
		fmt.Fprintf(r.out, "\n%s%s\n", setupRowCont, dim(fmt.Sprintf("%d. %s", i+1, b[0])))
		fmt.Fprintln(r.out, rootstepChain(b[1:]))
	}
	fmt.Fprintln(r.out)
	switch {
	case codeSet:
		// The environment's code stays out of this output, which a log or a
		// transcript may keep where the environment was never seen.
		setupRow(r.out, "code", dim("the one you set in "+setupKnobPairCode+": paste it when step 3 waits for it, within 10 minutes of its minting"))
	case code != "":
		setupRow(r.out, "code", code+dim(" -- paste it when step 3 waits for it: it pairs one computer, within 10 minutes of its minting"))
	default:
		setupRow(r.out, "code", dim(setupText("step 3 reads it from your paste: the {server}'s Devices page gives one, for one computer, within 10 minutes")))
	}
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim("it never rides in a command line, where other accounts and your shell history would keep it"))
	setupRow(r.out, "mapping", setupTextf("under locked projects (peer_auth), the {server} judges each caller this relay names once you map it, "+
		"and refuses it until then: in its config (%s), then restart it:", setupPeerAuthConfig))
	fmt.Fprintf(r.out, "%s  peer_auth.relays: {%s: {owner_uid: %d}}\n", setupRowCont, r.host, r.client.ownerUID())
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim("an account here that runs restricted agents joins "+clientRelayGroup+
		" and gets a line of its own there, restricted: {<its name>: <its uid here>}; the relay's own account is refused as a caller"))
	if r.goos == "linux" {
		fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim("log out and back in once: your agents join "+clientRelayGroup+" at your next login"))
	}
	setupRow(r.out, "approvals", setupText("on the {server}'s console, with a passkey, from any browser: a pairing under locked projects, access"))
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, setupText("requests and unlocks. seamlessd approve runs on the {server} alone: from here, sudo would be the"))
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, "relay vouching for you, which it never does")
	return nil
}

// setupClientCheck is a paired Client's closing check (setupSteps["check"]),
// after the installer's own steps: seam doctor through the relay or with the
// device key, and a request whose session the Server records under the name
// it sees for this computer. A relay that is not serving yet is its root
// step's to start: the check says how to run it then.
func setupClientCheck(r *setupRun, st *setupState) error {
	if st.device == "" {
		return nil
	}
	_, path, err := r.client.configTarget()
	if err != nil {
		return fmt.Errorf("seamlessd.setup: check: %w", err)
	}
	fmt.Fprintln(r.out)
	if st.device == setupDeviceRelay && !r.client.relayServing() {
		setupRow(r.out, "check", "once the relay serves: seam doctor (and seamlessd doctor reports the relay)")
		return nil
	}
	setupRow(r.out, "check", "seam doctor, and a session the "+setupWords[setupServer].name+" records under this computer's name:")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := r.client.check(ctx, r.out, path); err != nil {
		setupRow(r.out, "", yellow("the check failed: ")+err.Error())
		fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim("this computer is set up; run seam doctor once the cause is fixed"))
	}
	return nil
}

// clientSocketAnswers reports whether something answers a dial of the socket
// at path.
func clientSocketAnswers(path string) bool {
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close() //nolint:errcheck // a probe: the connection carried nothing
	return true
}

// clientClosingCheck is the closing check with the seam at seamBin ("" is the
// one beside this seamlessd): seam doctor against the Client config at
// cfgPath, then a session named seamless-setup-check started through that
// config, and the host the Server recorded it under read back.
func clientClosingCheck(seamBin string) func(ctx context.Context, w io.Writer, cfgPath string) error {
	return func(ctx context.Context, w io.Writer, cfgPath string) error {
		return clientRunCheck(ctx, w, resolveSeamBin(seamBin), cfgPath)
	}
}

// clientRunCheck is clientClosingCheck's run.
func clientRunCheck(ctx context.Context, w io.Writer, seam, cfgPath string) error {
	cmd := exec.CommandContext(ctx, seam, "doctor")
	cmd.Env = append(clientEnvWithout(os.Environ(), "SEAMLESS_"), "SEAMLESS_CONFIG="+cfgPath)
	cmd.Stdout, cmd.Stderr = w, w
	doctorErr := cmd.Run()
	cfg, err := config.LoadFrom(cfgPath)
	if err != nil {
		return err
	}
	host, err := clientSessionHost(ctx, cfg, "seamless-setup-check")
	if err != nil {
		return errors.Join(doctorErr, err)
	}
	setupRow(w, "host", setupTextf("the {server} recorded the session seamless-setup-check under %s: that is this computer's name there", host))
	return doctorErr
}

// clientEnvWithout is environ without the variables starting with prefix:
// the installer's own knobs must not stand in for the config's values.
func clientEnvWithout(environ []string, prefix string) []string {
	var out []string
	for _, kv := range environ {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return out
}

// clientSessionHost starts (or resumes) the session name through cfg, as
// every agent's MCP connection does, then reads the host the Server recorded
// it under from the console's session list.
func clientSessionHost(ctx context.Context, cfg config.Config, name string) (string, error) {
	hc, err := cfg.HTTPClient(30 * time.Second)
	if err != nil {
		return "", err
	}
	hc.CheckRedirect = config.RefuseRedirect
	headers := map[string]string{}
	if key := strings.TrimSpace(cfg.MCP.APIKey); key != "" {
		headers["Authorization"] = "Bearer " + key
	}
	if host := config.Hostname(); host != "" {
		headers[mcp.HostHeader] = host
	}
	cli, err := mcpclient.NewStreamableHttpClient(cfg.ServerURL()+"/api/mcp", mcptransport.WithHTTPHeaders(headers),
		mcptransport.WithHTTPBasicClient(hc))
	if err != nil {
		return "", err
	}
	defer func() { _ = cli.Close() }()
	if err := cli.Start(ctx); err != nil {
		return "", err
	}
	var initReq mcpgo.InitializeRequest
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "seamlessd-setup", Version: version}
	if _, err := cli.Initialize(ctx, initReq); err != nil {
		return "", err
	}
	var call mcpgo.CallToolRequest
	call.Params.Name = "session_start"
	call.Params.Arguments = map[string]any{"name": name}
	res, err := cli.CallTool(ctx, call)
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("session_start refused: %v", res.Content)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.ServerURL()+"/console/sessions?format=json&w=all&q="+url.QueryEscape(name), nil)
	if err != nil {
		return "", err
	}
	if key := strings.TrimSpace(cfg.MCP.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the session list answered %s", resp.Status)
	}
	var list struct {
		Sessions []struct {
			Name string `json:"name"`
			Host string `json:"host"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", fmt.Errorf("the session list: %w", err)
	}
	for _, s := range list.Sessions {
		if s.Name == name && s.Host != "" {
			return s.Host, nil
		}
	}
	return "", fmt.Errorf("the session list holds no %s with a host", name)
}

// clientFetchServerCert reads the certificate the Server at raw presents: it
// completes a handshake and sends nothing, then judges the chain against this
// computer's own roots for the address's host.
func clientFetchServerCert(ctx context.Context, raw string) (clientServerCert, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return clientServerCert{}, err
	}
	if u.Scheme != "https" {
		return clientServerCert{}, nil
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "443"
	}
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second},
		//nolint:gosec // reads the certificate to show it: nothing is sent, and the chain is judged below
		Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, ServerName: host}}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return clientServerCert{}, err
	}
	defer func() { _ = conn.Close() }()
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return clientServerCert{}, errors.New("not a TLS connection")
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return clientServerCert{}, errors.New("the server presented no certificate")
	}
	out := clientServerCert{tls: true, fingerprint: devices.CertFingerprint(certs[0].Raw),
		pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certs[0].Raw}), trusted: true}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	if _, err := certs[0].Verify(x509.VerifyOptions{DNSName: host, Intermediates: inter}); err != nil {
		out.trusted, out.why = false, err.Error()
	}
	return out, nil
}

// clientRealAccounts reads this machine's accounts for the relay's root step:
// macOS through dscl's lists, Linux through the account database.
func clientRealAccounts() clientAccounts {
	var a clientAccounts
	if pin, ok, err := access.DaemonAccount(); err == nil && ok && pin.Name == clientRelayAccount {
		a.pinned = true
	}
	if _, err := user.Lookup(clientRelayAccount); err == nil {
		a.accountExists = true
	}
	if _, err := user.LookupGroup(clientRelayGroup); err == nil {
		a.groupExists = true
	}
	users, uerr := exec.Command("dscl", ".", "-list", "/Users", "UniqueID").Output()
	groups, gerr := exec.Command("dscl", ".", "-list", "/Groups", "PrimaryGroupID").Output()
	if uerr != nil || gerr != nil {
		a.uid, a.gid = clientRelayFreeIDs(func(int) bool { return false })
		return a
	}
	taken := map[int]bool{}
	for _, list := range [][]byte{users, groups} {
		for line := range bytes.SplitSeq(list, []byte("\n")) {
			f := strings.Fields(string(line))
			if len(f) != 2 {
				continue
			}
			if id, err := strconv.Atoi(f[1]); err == nil {
				taken[id] = true
			}
			switch f[0] {
			case clientRelayAccount:
				a.accountExists = true
			case clientRelayGroup:
				a.groupExists = true
			}
		}
	}
	a.uid, a.gid = clientRelayFreeIDs(func(id int) bool { return taken[id] })
	return a
}
