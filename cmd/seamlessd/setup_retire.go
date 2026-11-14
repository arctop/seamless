package main

// setup's "Sign in with passkeys and retire the key" (plan networked-relay):
// on a Standalone install with locked projects (peer_auth), the agents use the
// socket and hold no key, and the key's only remaining use is the console's
// config (~/.config/seamless/console.yaml), which every process of the owner
// can read. This re-run turns on passkey sign-in, waits for the owner to prove
// a passkey signs in, checks nothing else still dials TCP with the key, then
// has root take the key out of the daemon's config and drops it from
// console.yaml. TCP then serves the console's sign-in, its pages and /healthz.
//
// Setup runs no root step (the address step's rule): it prints each one and
// then waits for its effect, read from the daemon itself over the owner's
// socket (GET /console/signin/state), since the daemon's config is not the
// owner's to read. Every step is judged from what is there, so a run that
// stops anywhere -- a crash, a timeout, the owner's Ctrl-C -- continues from
// where things are when setup runs again. The one thing a run never inherits
// is the proof: until the key is gone, each run asks for a fresh passkey
// sign-in before it removes anything.
//
// No path leaves the owner without a way in. Nothing is removed before a
// passkey sign-in is recorded; the key goes from console.yaml only once the
// daemon no longer takes it; and the terminal sign-in (console-open --sign-in
// --code, as the daemon's account) signs a browser in when every passkey is
// lost. Bringing a key back mints a fresh one: the old one is never revived.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
)

// retireConsoleConfig is the console's own config the peer-auth guide has the
// owner keep beside the socket config: TCP server_url and the key.
const retireConsoleConfig = "console.yaml"

// retirePatience is how long setup waits for a root step's effect or for the
// owner's sign-in before it stops; a run after that continues from there.
const retirePatience = 15 * time.Minute

// retireEvery is how often it asks the daemon while it waits.
const retireEvery = 2 * time.Second

// retireWired is one thing still wired to TCP with the key.
type retireWired struct{ where, what string }

// retireEnv is the machine as the step sees it, injected so a test stands any
// state up and dials nothing.
type retireEnv struct {
	// state asks the daemon how its console signs a browser in, counting the
	// passkey sign-ins made at or after since (zero: none counted).
	state func(since time.Time) (console.SignInState, error)
	// pin reads the daemon-account pin (access.DaemonAccount).
	pin func() (access.DaemonPin, bool, error)
	// healthz is the instance id a daemon answers /healthz with at base, ""
	// when nothing answers.
	healthz func(base string) string
	// consolePath is the console's config; inventory lists what else holds
	// key or dials with the console's config.
	consolePath string
	inventory   func(key string) []retireWired
	// keyAccepted reports whether the daemon at base takes key over TCP.
	keyAccepted func(base, key string) bool
	newKey      func() (string, error)
	now         func() time.Time
	sleep       func(time.Duration)
	patience    time.Duration
}

// realRetireEnv is the step's view of this machine, from the run's own.
func realRetireEnv(r *setupRun) retireEnv {
	return retireEnv{
		state: func(since time.Time) (console.SignInState, error) {
			cfg, err := r.loadConfig()
			if err != nil {
				return console.SignInState{}, err
			}
			return daemonSignInState(cfg, since)
		},
		pin:         access.DaemonAccount,
		healthz:     healthzInstance,
		consolePath: filepath.Join(r.home, ".config", "seamless", retireConsoleConfig),
		inventory:   func(key string) []retireWired { return retireScanHome(r.home, r.goos, key) },
		keyAccepted: keyAcceptedAt(filepath.Join(r.home, ".config", "seamless", retireConsoleConfig)),
		newKey:      newRetireKey,
		now:         time.Now,
		//nolint:forbidigo // polling an external process (the daemon, root's edit) that gives no completion signal
		sleep:    time.Sleep,
		patience: retirePatience,
	}
}

// setupRetireStep is setupSteps["retire-key"]'s run.
func setupRetireStep(r *setupRun, st *setupState) error { return retireRun(r, st, realRetireEnv(r)) }

// errRetireStopped is a run that stopped before the end; setup run again
// continues from where things are.
var errRetireStopped = errors.New("retiring the key stopped")

// retireStop is a stop in the owner's words: what is missing, and that a run
// of seamlessd setup continues from here.
func retireStop(format string, args ...any) error {
	return &ownerError{msg: "seamlessd setup: " + fmt.Sprintf(format, args...) + ". Nothing else changed; run seamlessd setup again to continue from here.",
		cause: errRetireStopped}
}

// retireRun is the step: every stage judged from what is there.
func retireRun(r *setupRun, _ *setupState, env retireEnv) error {
	cfg, err := r.loadConfig()
	if err != nil {
		return err
	}
	if cfg.SocketPath() == "" {
		return retireStop("%s does not name the daemon's socket, so your agents still dial TCP with the key: "+
			"connect them through the socket first (%sguides/peer-auth/#connect-the-owner)", tildePath(r.facts.configPath), setupDocs)
	}
	st, err := env.state(time.Time{})
	if err != nil {
		return retireStop("the daemon did not answer on its socket (%s): %v", cfg.DaemonAddress(), err)
	}
	fmt.Fprintln(r.out)
	if !st.Key {
		return retireRetired(r, env, st)
	}
	acct := r.daemonAccount()
	if err := retirePreconditions(r, env, st, acct); err != nil {
		return err
	}
	if st, err = retireSignInOn(r, env, st, acct); err != nil {
		return err
	}
	if st, err = retirePasskeyReady(r, env, st, acct); err != nil {
		return err
	}
	if err := retireProof(r, env, st); err != nil {
		return err
	}
	key, err := retireConsoleKey(env.consolePath)
	if err != nil {
		return err
	}
	if err := retireInventory(r, env, key); err != nil {
		return err
	}
	if err := retireDaemonKey(r, env, acct); err != nil {
		return err
	}
	return retireFinish(r, env, st)
}

// retirePreconditions are what must hold before anything changes: the
// daemon-account pin, the localhost origin held on both loopback addresses
// (F1), and a daemon whose sign-in binds the cookie to the browser's key
// (the state route answering is that release's).
func retirePreconditions(r *setupRun, env retireEnv, st console.SignInState, acct string) error {
	pin, ok, err := env.pin()
	switch {
	case err != nil:
		return retireStop("the daemon-account pin is there but not believed (%v): root rewrites it, as the peer-auth guide shows", err)
	case !ok:
		setupRow(r.out, "pin", "missing: setup runs no root step. As an administrator, paste this whole:")
		for _, line := range strings.Split(rootstepChain(rootstepPinCommands(rootstepToolsFor(r.goos), acct)), "\n") {
			fmt.Fprintf(r.out, "%s  %s\n", setupRowCont, line)
		}
		return retireStop("the daemon's account is not pinned (%s)", access.DaemonAccountPath)
	}
	setupRow(r.out, "pin", pin.String()+dim(" ("+pin.Path+")"))
	u, err := url.Parse(st.Origin)
	if err != nil || st.Origin == "" {
		return retireStop("the daemon names no passkey origin: turn on locked projects (peer_auth) first")
	}
	if strings.EqualFold(u.Hostname(), "localhost") {
		port := u.Port()
		if port == "" {
			port = "80"
		}
		v4 := env.healthz("http://" + net.JoinHostPort("127.0.0.1", port))
		v6 := env.healthz("http://" + net.JoinHostPort("::1", port))
		if v4 == "" || v4 != v6 {
			return retireStop("%s is not this daemon on both loopback addresses (127.0.0.1 answers %s, ::1 answers %s): "+
				"another process could serve the passkey origin on the other one; restart the daemon on this release, which holds both",
				st.Origin, retireAnswer(v4), retireAnswer(v6))
		}
		setupRow(r.out, "loopbacks", "127.0.0.1 and ::1 are both this daemon "+dim("("+st.Origin+")"))
	}
	setupRow(r.out, "cookie", "a sign-in binds its cookie to the browser's key: a cookie read on another port opens nothing")
	return nil
}

// retireAnswer words a /healthz instance for a refusal.
func retireAnswer(instance string) string {
	if instance == "" {
		return "nothing"
	}
	return "instance " + instance
}

// retireRootEdit prints one root step: an edit of the daemon's config, then
// the restart. Like every command setup prints for root, each names its tools
// by their absolute paths (rootstepTools).
func retireRootEdit(r *setupRun, label, what string, lines ...string) {
	setupRow(r.out, label, what+": setup runs no root step. As an administrator:")
	fmt.Fprintf(r.out, "%s  %s -e %s\n", setupRowCont, rootstepToolsFor(r.goos).sudo, setupPeerAuthConfig)
	for _, l := range lines {
		fmt.Fprintf(r.out, "%s    %s\n", setupRowCont, dim(l))
	}
	fmt.Fprintf(r.out, "%s  %s\n", setupRowCont, retireRestartCommand(r.goos))
}

// retireRestartCommand restarts the peer_auth daemon.
func retireRestartCommand(goos string) string {
	t := rootstepToolsFor(goos)
	if goos == "darwin" {
		return t.asRoot(t.launchctl, "kickstart", "-k", "system/"+systemDaemonLabel)
	}
	return t.asRoot(t.systemctl, "restart", strings.TrimSuffix(systemDaemonUnit, ".service"))
}

// retireWait asks the daemon until done says so, or the patience runs out.
func retireWait(r *setupRun, env retireEnv, waiting string, since time.Time, done func(console.SignInState) bool) (console.SignInState, error) {
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim("waiting for "+waiting+"..."))
	deadline := env.now().Add(env.patience)
	for {
		st, err := env.state(since)
		if err == nil && done(st) {
			return st, nil
		}
		if !env.now().Before(deadline) {
			why := waiting
			if err != nil {
				why += fmt.Sprintf(" (the daemon did not answer on its socket: %v)", err)
			}
			return st, retireStop("setup stopped waiting for %s", why)
		}
		env.sleep(retireEvery)
	}
}

// retireSignInOn turns on passkey sign-in, with the key still accepted.
func retireSignInOn(r *setupRun, env retireEnv, st console.SignInState, _ string) (console.SignInState, error) {
	if st.SignIn == config.ConsoleSignInPasskey {
		setupRow(r.out, "sign-in", "passkey: the key signs no browser in, and scripts still use it")
		return st, nil
	}
	retireRootEdit(r, "sign-in", "turn on passkey sign-in", "under console: add  sign_in: passkey  (keep mcp.api_key for now)")
	st, err := retireWait(r, env, "the daemon to sign browsers in with a passkey", time.Time{}, func(s console.SignInState) bool {
		return s.SignIn == config.ConsoleSignInPasskey
	})
	if err != nil {
		return st, err
	}
	setupRow(r.out, "sign-in", "passkey: on")
	return st, nil
}

// retirePasskeyReady makes sure a passkey can sign in: one enrolled for this
// install's relying party, else one added now on the Add a passkey page and
// approved on the server as the daemon's account.
func retirePasskeyReady(r *setupRun, env retireEnv, st console.SignInState, acct string) (console.SignInState, error) {
	if st.Passkeys+st.UnknownRP > 0 {
		setupRow(r.out, "passkeys", fmt.Sprintf("%d for %s", st.Passkeys+st.UnknownRP, st.RPID))
		return st, nil
	}
	setupRow(r.out, "passkeys", "none for "+st.RPID+" yet: add one now. Open this on the device that will hold it:")
	fmt.Fprintf(r.out, "%s  %s\n", setupRowCont, st.Origin+"/console/passkeys/add")
	fmt.Fprintf(r.out, "%s%s\n%s  %s\n", setupRowCont, "then approve the code it shows, in your own terminal:",
		setupRowCont, retireDaemonCommand(acct, "approvers approve --code <code>"))
	return retireWait(r, env, "a passkey to be added", time.Time{}, func(s console.SignInState) bool { return s.Passkeys+s.UnknownRP > 0 })
}

// retireDaemonCommand is a seamlessd command run as the daemon's account:
// sudo and env by their absolute paths, which are the same on macOS and
// Linux, so neither is found on the owner's PATH.
func retireDaemonCommand(acct, verb string) string {
	t := rootstepToolsFor("")
	return fmt.Sprintf("%s -k -u %s %s SEAMLESS_CONFIG=%s /opt/seamless/bin/seamlessd %s", t.sudo, posixShellArg(acct), t.env, setupPeerAuthConfig, verb)
}

// retireProof waits for the owner to sign in with a passkey now: nothing is
// removed before the owner has shown they can get in.
func retireProof(r *setupRun, env retireEnv, st console.SignInState) error {
	since := env.now().Add(-time.Second)
	setupRow(r.out, "proof", "sign in to the console with your passkey now:")
	fmt.Fprintf(r.out, "%s  %s\n", setupRowCont, st.Origin+"/console/login")
	if _, err := retireWait(r, env, "a passkey sign-in", since, func(s console.SignInState) bool { return s.SignIns > 0 }); err != nil {
		return err
	}
	setupRow(r.out, "proof", "a passkey signed in: the console no longer needs the key")
	return nil
}

// retireInventory refuses to go on while something still dials TCP with the
// key, unless the owner, shown each one, says to.
func retireInventory(r *setupRun, env retireEnv, key string) error {
	if key == "" {
		setupRow(r.out, "inventory", "not checked: "+tildePath(env.consolePath)+" holds no key to look for")
		return nil
	}
	wired := env.inventory(key)
	if len(wired) == 0 {
		setupRow(r.out, "inventory", "nothing else here dials TCP with the key "+
			dim("(scripts on other computers and A2A clients are not seen here: MCP over TCP will name the socket to them)"))
		return nil
	}
	setupRow(r.out, "inventory", fmt.Sprintf("%d still dial TCP with the key, and stop working once it is retired:", len(wired)))
	for _, w := range wired {
		fmt.Fprintf(r.out, "%s  %s %s\n", setupRowCont, w.where, dim(w.what))
	}
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim("move each to the socket (seamlessd install-hooks rewires the hooks and MCP), or go on without them"))
	ok, err := retireAsk(r, "Retire the key anyway? [y/N]: ")
	if err != nil {
		return err
	}
	if !ok {
		return retireStop("the key is kept while %d still use it", len(wired))
	}
	return nil
}

// retireAsk reads a yes or no; anything but yes is no, and so is no terminal.
func retireAsk(r *setupRun, prompt string) (bool, error) {
	if !r.interactive {
		return false, nil
	}
	fmt.Fprintf(r.out, "%s%s", setupRowCont, prompt)
	line, err := r.in.ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes", nil
}

// retireDaemonKey has root take mcp.api_key out of the daemon's config, and
// waits for the daemon to serve without it.
func retireDaemonKey(r *setupRun, env retireEnv, _ string) error {
	retireRootEdit(r, "key", "retire the key", "under mcp: delete the api_key line (keep console: sign_in: passkey)")
	if _, err := retireWait(r, env, "the daemon to serve without the key", time.Time{}, func(s console.SignInState) bool {
		return !s.Key && s.SignIn == config.ConsoleSignInPasskey
	}); err != nil {
		return err
	}
	setupRow(r.out, "key", "retired: TCP serves the console's sign-in, its pages and /healthz, and the agents use the socket")
	return nil
}

// retireFinish drops the key from console.yaml, so console-open opens the
// sign-in page, and says how the owner gets in from now on.
func retireFinish(r *setupRun, env retireEnv, st console.SignInState) error {
	changed, err := retireDropConsoleKey(env.consolePath, st.Origin)
	if err != nil {
		return err
	}
	if changed {
		setupRow(r.out, "console", "took the key out of "+tildePath(env.consolePath)+dim(": console-open opens the sign-in page, at "+st.Origin))
	}
	setupRow(r.out, "way in", "your passkey, at "+st.Origin+"/console/login")
	fmt.Fprintf(r.out, "%s%s\n%s  %s\n", setupRowCont, dim("every passkey lost: choose Show a code on that page, then in your own terminal"),
		setupRowCont, retireDaemonCommand(r.daemonAccount(), "console-open --sign-in --code <code>"))
	return nil
}

// retireRetired is a run on an install whose daemon holds no key: it finishes
// a run that stopped before console.yaml, or offers to bring a key back.
func retireRetired(r *setupRun, env retireEnv, st console.SignInState) error {
	key, err := retireConsoleKey(env.consolePath)
	if err != nil {
		return err
	}
	if key != "" && !env.keyAccepted(retireConsoleBase(env.consolePath, st), key) {
		if retireUndoWaiting(env.consolePath) {
			// A fresh key setup wrote, which root has not given the daemon yet.
			setupRow(r.out, "key", "a fresh key waits in "+tildePath(env.consolePath)+" for the daemon's config")
			return retireUndoWait(r, env, retireConsoleBase(env.consolePath, st), key)
		}
		setupRow(r.out, "key", "retired: the daemon holds no key")
		return retireFinish(r, env, st)
	}
	setupRow(r.out, "key", "retired: the console signs a browser in with a passkey, and the agents use the socket")
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim("bringing a key back mints a fresh one, for scripts and console-open; the old one stays retired"))
	ok, err := retireAsk(r, "Bring a key back? [y/N]: ")
	if err != nil || !ok {
		return err
	}
	return retireUndo(r, env, st)
}

// retireUndo mints a fresh key, writes it into console.yaml (0600, the
// owner's, as the guide has it), and has root put the same key in the
// daemon's config: setup never prints it, so root copies it from that file.
func retireUndo(r *setupRun, env retireEnv, st console.SignInState) error {
	key, err := env.newKey()
	if err != nil {
		return err
	}
	base := retireConsoleBase(env.consolePath, st)
	if err := retireWriteConsoleKey(env.consolePath, base, key); err != nil {
		return err
	}
	setupRow(r.out, "console", "wrote a fresh key into "+tildePath(env.consolePath))
	return retireUndoWait(r, env, base, key)
}

// retireUndoWait has root give the daemon the key console.yaml holds, and
// waits until the daemon takes it.
func retireUndoWait(r *setupRun, env retireEnv, base, key string) error {
	retireRootEdit(r, "key", "give the daemon the same key",
		"under mcp: set  api_key:  to the mcp.api_key in "+env.consolePath+" (the key is not shown here)")
	deadline := env.now().Add(env.patience)
	fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim("waiting for the daemon to take the new key..."))
	for !env.keyAccepted(base, key) {
		if !env.now().Before(deadline) {
			return retireStop("setup stopped waiting for the daemon to take the new key in %s", tildePath(env.consolePath))
		}
		env.sleep(retireEvery)
	}
	setupRow(r.out, "key", "a fresh key: scripts and console-open use it; passkey sign-in stays on until you take sign_in out of the daemon's config")
	return nil
}

// retireUndoMark is the line a fresh key's console.yaml carries, so a run
// that stopped before the daemon took the key goes on waiting for it rather
// than take it out again as a retired key's copy.
const retireUndoMark = "# seamlessd setup brought a fresh key back: the daemon's config takes the same one"

// retireUndoWaiting reports whether console.yaml's key is a fresh one setup
// wrote.
func retireUndoWaiting(path string) bool {
	data, err := os.ReadFile(path)
	return err == nil && bytes.Contains(data, []byte(retireUndoMark))
}

// retireConsoleKey is the key console.yaml holds, "" when it holds none or is
// not there.
func retireConsoleKey(path string) (string, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	cfg, err := config.LoadFrom(path)
	if err != nil {
		return "", retireStop("%s does not load (%v): fix it, or remove it", tildePath(path), err)
	}
	return strings.TrimSpace(cfg.MCP.APIKey), nil
}

// retireConsoleBase is the TCP address console.yaml names, else the
// daemon's passkey origin.
func retireConsoleBase(path string, st console.SignInState) string {
	if cfg, err := config.LoadFrom(path); err == nil && cfg.SocketPath() == "" && strings.TrimSpace(cfg.AdvertisedURL) != "" {
		return cfg.ServerURL()
	}
	return st.Origin
}

// retireDropConsoleKey takes the api_key line out of console.yaml's mcp block
// (and the block, once empty), keeping every other line as the owner wrote
// it, and writes the file atomically. A server_url on a loopback address
// becomes origin, the passkey origin on the same port (localhost), since a
// browser refuses a passkey on an IP address and console-open now opens the
// sign-in page there. It refuses, changing nothing, when the result still
// carries a key or no longer loads (a key written another way than the
// guide's: the owner takes it out by hand).
func retireDropConsoleKey(path, origin string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	key, err := retireConsoleKey(path)
	if err != nil || key == "" {
		return false, err
	}
	out := retireWithoutKey(data)
	wantURL := ""
	if cfg, lerr := config.LoadFrom(path); lerr == nil {
		wantURL = cfg.ServerURL()
		if to := retireLoopbackOrigin(wantURL, origin); to != "" {
			out, wantURL = retireWithServerURL(out, to), to
		}
	}
	if err := retireWriteChecked(path, out, func(cfg config.Config) bool {
		return strings.TrimSpace(cfg.MCP.APIKey) == "" && (wantURL == "" || cfg.ServerURL() == wantURL)
	}); err != nil {
		return false, retireStop("%s: setup could not take the key out (%v): delete its mcp.api_key by hand", tildePath(path), err)
	}
	return true, nil
}

// retireLoopbackOrigin is origin when serverURL dials a loopback IP address
// on origin's port and origin is localhost: the same daemon, under the name a
// passkey works on. "" otherwise.
func retireLoopbackOrigin(serverURL, origin string) string {
	s, err1 := url.Parse(serverURL)
	o, err2 := url.Parse(origin)
	if err1 != nil || err2 != nil || !strings.EqualFold(o.Hostname(), "localhost") || o.Scheme != s.Scheme || o.Port() != s.Port() {
		return ""
	}
	if ip := net.ParseIP(s.Hostname()); ip == nil || !ip.IsLoopback() {
		return ""
	}
	return strings.TrimSuffix(origin, "/")
}

// retireWithServerURL is data with its top-level server_url line saying url.
func retireWithServerURL(data []byte, url string) []byte {
	lines := strings.SplitAfter(string(data), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "server_url:") {
			lines[i] = "server_url: " + strconv.Quote(url) + "\n"
			break
		}
	}
	return []byte(strings.Join(lines, ""))
}

// retireWithoutKey is data without the api_key lines of its top-level mcp
// block, and without the block -- its comments included -- when nothing else
// is left in it.
func retireWithoutKey(data []byte) []byte {
	var out, block []string
	inMCP, kept := false, 0
	flush := func() {
		if inMCP && kept > 0 {
			out = append(out, block...)
		}
		block, inMCP, kept = nil, false, 0
	}
	for _, l := range strings.SplitAfter(string(data), "\n") {
		trimmed := strings.TrimSpace(l)
		// A line at the margin -- a key, or a comment about what follows --
		// ends the block before it.
		if l != "" && l[0] != ' ' && l[0] != '\t' && trimmed != "" {
			flush()
			if strings.HasPrefix(trimmed, "mcp:") && strings.TrimSpace(strings.TrimPrefix(trimmed, "mcp:")) == "" {
				inMCP, block = true, []string{l}
				continue
			}
		}
		switch {
		case !inMCP:
			out = append(out, l)
		case strings.HasPrefix(trimmed, "api_key:"):
		default:
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				kept++
			}
			block = append(block, l)
		}
	}
	flush()
	return []byte(strings.Join(out, ""))
}

// retireWriteConsoleKey writes key into console.yaml, adding the file when
// there is none: role client, base, and the key, 0600.
func retireWriteConsoleKey(path, base, key string) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var b bytes.Buffer
	if len(data) == 0 {
		fmt.Fprintf(&b, "role: client\nserver_url: %s\n", strconv.Quote(base))
	} else {
		b.WriteString(strings.ReplaceAll(string(retireWithoutKey(data)), retireUndoMark+"\n", ""))
		if b.Len() > 0 && !bytes.HasSuffix(b.Bytes(), []byte("\n")) {
			b.WriteString("\n")
		}
	}
	fmt.Fprintf(&b, "%s\nmcp:\n  api_key: %s\n", retireUndoMark, strconv.Quote(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := retireWriteChecked(path, b.Bytes(), func(cfg config.Config) bool { return cfg.MCP.APIKey == key }); err != nil {
		return retireStop("%s: setup could not write the new key (%v): nothing was written", tildePath(path), err)
	}
	return nil
}

// retireWriteChecked writes data over path, 0600, through a temporary file in
// the same directory, once the temporary file loads and ok says the result
// is right: a crash leaves either the old file or the new one.
func retireWriteChecked(path string, data []byte, ok func(config.Config) bool) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	cfg, err := config.LoadFrom(name)
	if err != nil {
		return err
	}
	if !ok(cfg) {
		return errors.New("the result does not say what it should")
	}
	return os.Rename(name, path)
}

// newRetireKey mints a fresh bearer key, in the shape serve generates one.
func newRetireKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mint a key: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// daemonSignInState asks the daemon cfg dials, the owner's socket, for its
// console's sign-in state. It follows no redirect.
func daemonSignInState(cfg config.Config, since time.Time) (console.SignInState, error) {
	client, err := cfg.HTTPClient(5 * time.Second)
	if err != nil {
		return console.SignInState{}, err
	}
	client.CheckRedirect = config.RefuseRedirect
	target := cfg.ServerURL() + "/console/signin/state"
	if !since.IsZero() {
		target += "?since=" + strconv.FormatInt(since.Unix(), 10)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		return console.SignInState{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return console.SignInState{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return console.SignInState{}, errors.New("this daemon predates passkey sign-in's setup: update it first")
	}
	if resp.StatusCode != http.StatusOK {
		return console.SignInState{}, fmt.Errorf("the daemon answered %s", resp.Status)
	}
	var st console.SignInState
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&st); err != nil {
		return console.SignInState{}, err
	}
	return st, nil
}

// healthzInstance is the instance id the daemon at base answers /healthz
// with, "" when nothing answers.
func healthzInstance(base string) string {
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: config.RefuseRedirect}
	resp, err := client.Get(base + "/healthz")
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var h struct {
		Instance string `json:"instance"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&h) != nil {
		return ""
	}
	return h.Instance
}

// keyAcceptedAt reports whether the daemon at base takes key: the console's
// sign-in state, asked with it as a bearer, trusting what the console's config
// at consolePath trusts (its tls.ca_file on a TLS install).
func keyAcceptedAt(consolePath string) func(base, key string) bool {
	return func(base, key string) bool {
		cfg := config.Defaults()
		if loaded, err := config.LoadFrom(consolePath); err == nil {
			cfg = loaded
		}
		client, err := cfg.HTTPClient(5 * time.Second)
		if err != nil {
			return false
		}
		client.CheckRedirect = config.RefuseRedirect
		return keyAcceptedWith(client, base, key)
	}
}

// keyAcceptedWith is keyAcceptedAt over client.
func keyAcceptedWith(client *http.Client, base, key string) bool {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/console/signin/state", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// retireScanHome lists what in the owner's home still dials TCP with key:
// an agent client's settings or a Seamless config that holds it, or that
// runs with the console's config. Scripts elsewhere and A2A clients are not
// seen.
func retireScanHome(home, goos, key string) []retireWired {
	if home == "" || key == "" {
		return nil
	}
	files := []struct{ path, what string }{
		{filepath.Join(home, ".claude", "settings.json"), "Claude Code's hooks"},
		{filepath.Join(home, ".claude.json"), "Claude Code's MCP registration"},
		{filepath.Join(home, ".codex", "config.toml"), "Codex's MCP registration"},
		{filepath.Join(home, ".codex", "hooks.json"), "Codex's hooks"},
	}
	if p, err := claudeDesktopConfigPathFor(goos, home, os.Getenv("APPDATA")); err == nil {
		files = append(files, struct{ path, what string }{p, "the Claude app's MCP bridge"})
	}
	if matches, err := filepath.Glob(filepath.Join(home, ".config", "seamless", "*.y*ml")); err == nil {
		for _, m := range matches {
			if filepath.Base(m) != retireConsoleConfig {
				files = append(files, struct{ path, what string }{m, "a Seamless config"})
			}
		}
	}
	var out []retireWired
	for _, f := range files {
		data, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		switch {
		case bytes.Contains(data, []byte(key)):
			out = append(out, retireWired{tildePath(f.path), f.what + " holds the key"})
		case bytes.Contains(data, []byte(retireConsoleConfig)):
			out = append(out, retireWired{tildePath(f.path), f.what + " runs with " + retireConsoleConfig + ", which holds the key"})
		}
	}
	return out
}

// retireDoctorChecks is doctor's rows for the owner's half of a peer_auth
// install (a socket config) whose console.yaml still keeps the daemon's key:
// that every process of the owner can read it, how setup retires it, and
// what else in home still dials TCP with it. Nothing anywhere else, and
// nothing once the key is retired. It only reads.
func retireDoctorChecks(cfg config.Config, home, goos string) []check {
	if !cfg.IsClient() || cfg.SocketPath() == "" || home == "" {
		return nil
	}
	path := filepath.Join(home, ".config", "seamless", retireConsoleConfig)
	key, err := retireConsoleKey(path)
	if err != nil || key == "" {
		return nil
	}
	out := []check{{statusInfo, "console key", tildePath(path) + " keeps the daemon's key, which every process of yours can read: " +
		"seamlessd setup retires it (Sign in with passkeys and retire the key)"}}
	for _, w := range retireScanHome(home, goos, key) {
		out = append(out, check{statusInfo, "console key", w.where + ": " + w.what})
	}
	return out
}
