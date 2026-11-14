package main

// seamlessd setup -- the front door's questions (plan networked-relay).
//
// The installers download and verify a release, then hand the decision of how
// this computer uses Seamless to this command: Standalone, or Networked, and
// then the Server or a Client of it. It decides from the environment's answers
// (SEAMLESS_SETUP and the installers' own client knobs), else from what is
// installed, else by asking a person at the terminal, and hands the decision
// back to the installer (--state) for the steps the installer still owns: the
// binaries, the hooks, the per-user service, the health check.
//
// Each kind's own steps -- the Server's address and TLS, importing a backup,
// locked projects, pairing a Client -- are tasks of their own. They plug in as
// setupSteps entries (a run func), and the question tree, the state file and
// the installers' hand-off stay as they are when one lands. Until then a step
// says in one line that setup does not do it yet, and how to do it by hand.
//
// Unattended means unchanged. Setup asks only a person at a terminal who set
// no SEAMLESS_* variable at all (setupRun.asks); with any knob set, or no
// terminal, the environment is the whole answer and an absent one is today's
// install. The installers apply the same rule before they run it: a run with
// no setup knob (SEAMLESS_SETUP, SEAMLESS_PAIR_CODE, SEAMLESS_IMPORT) that a
// script drives or that has no terminal never starts setup at all. Every run
// the updater makes is one: it always sets SEAMLESS_VERSION and
// SEAMLESS_INSTALL_DIR, and an unattended one carries no SEAMLESS_* but the
// installerKnobs, which list no setup knob.

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/arctop/seamless/internal/config"
)

// setupKind is how a computer uses Seamless.
type setupKind int

const (
	// setupNone: nothing is installed on this computer yet.
	setupNone setupKind = iota
	// setupStandalone: everything on this computer, today's default install.
	setupStandalone
	// setupServer: Networked, and this computer keeps the data.
	setupServer
	// setupClient: Networked, and this computer dials the Server.
	setupClient
)

// setupWord is one kind as the owner meets it: its SEAMLESS_SETUP spelling,
// the name the questions use, and the line that explains it.
type setupWord struct{ env, name, line string }

// setupWords holds every word the owner sees for the kinds, and nothing else
// in the code spells one: the names are the plan's proposal (its open decision
// D1), so renaming a kind is an edit of its line here. Messages name the kinds
// through the {standalone}, {networked}, {server} and {client} tokens of
// setupText, the lines below included.
var setupWords = map[setupKind]setupWord{
	setupStandalone: {env: "standalone", name: "Standalone", line: "everything on this computer: the daemon, the data, and the agents that use them"},
	setupServer:     {env: "server", name: "Server", line: "this computer keeps the data, and the other computers connect to it"},
	setupClient:     {env: "client", name: "Client", line: "this computer connects to the {server} and keeps no data of its own"},
}

// setupNetworkedWord is the choice Server and Client belong to.
var setupNetworkedWord = setupWord{name: "Networked", line: "one computer keeps the data, and the others connect to it"}

// setupText spells the kinds in s with their names from setupWords.
func setupText(s string) string {
	return strings.NewReplacer(
		"{standalone}", setupWords[setupStandalone].name,
		"{networked}", setupNetworkedWord.name,
		"{server}", setupWords[setupServer].name,
		"{client}", setupWords[setupClient].name,
	).Replace(s)
}

// setupTextf is fmt.Sprintf over setupText(format).
func setupTextf(format string, args ...any) string {
	return fmt.Sprintf(setupText(format), args...)
}

// setupOffered is the kinds setup offers on goos. Windows is offered
// Standalone and Client only (setupWindowsWhy).
func setupOffered(goos string) []setupKind {
	if goos == "windows" {
		return []setupKind{setupStandalone, setupClient}
	}
	return []setupKind{setupStandalone, setupServer, setupClient}
}

// setupWindowsWhy is why a Windows computer is never offered as the Server
// (the plan's owner decision D3): it has no peer_auth, so no locked projects
// and no relay, and it stays a key Client of a Server elsewhere.
const setupWindowsWhy = "Windows has no peer_auth and no relay, so a Windows computer can be a {client} but not the {server}"

// setupEnvValues is SEAMLESS_SETUP's accepted values on goos, for messages.
func setupEnvValues(goos, sep string) string {
	var out []string
	for _, k := range setupOffered(goos) {
		out = append(out, setupWords[k].env)
	}
	return strings.Join(out, sep)
}

// setupDescribe names a kind for a summary row: Standalone, or Networked:
// Server, or Networked: Client of its server.
func setupDescribe(k setupKind, serverURL string, locked bool) string {
	switch k {
	case setupServer:
		return setupText("{networked}: {server}")
	case setupClient:
		return setupText("{networked}: {client} of ") + serverURL
	case setupStandalone:
		if locked {
			return setupText("{standalone}, with locked projects")
		}
		return setupText("{standalone}")
	}
	return "nothing set up yet"
}

// The setup knobs: the environment's answers to the setup questions, for a
// run a script drives. SEAMLESS_SERVER_URL and SEAMLESS_MCP_API_KEY are the
// installers' own client knobs, with the meaning they always had.
const (
	setupKnobKind     = "SEAMLESS_SETUP"
	setupKnobServer   = "SEAMLESS_SERVER_URL"
	setupKnobKey      = "SEAMLESS_MCP_API_KEY"
	setupKnobPairCode = "SEAMLESS_PAIR_CODE"
	setupKnobImport   = "SEAMLESS_IMPORT"
)

// setupDocs is the docs site every by-hand pointer links into.
const setupDocs = "https://thereisnospoon.org/docs/"

// setupProtocolLine is the line `seamlessd setup -h` prints when it speaks
// the installers' hand-off below; the installers look for it (exactly) before
// they run setup, and install a release without it as before.
const setupProtocolLine = "installer protocol: 1"

// setupStateHeader opens the state file --state names: the hand-off between
// setup's first half, the installer, and setup's --finish. The installer
// reads only its role= and server_url= lines; the rest is setup's own.
const setupStateHeader = "seamless-setup 1"

// setupAnswer says how a run's kind was decided.
type setupAnswer string

const (
	setupAsked   setupAnswer = "asked"     // a person answered at the terminal
	setupFromEnv setupAnswer = "env"       // SEAMLESS_SETUP or SEAMLESS_SERVER_URL
	setupKept    setupAnswer = "installed" // what is installed, unchanged
	setupDefault setupAnswer = "default"   // nothing installed, nothing asked
)

// setupState is what a run decided, and the hand-off of it: to the installer
// (role and serverURL, for the steps it owns), and to setup's --finish (the
// rest, for the steps that come after the installer's).
type setupState struct {
	// kind is how this computer uses Seamless once the run is done.
	kind setupKind
	// role is what the installer acts on: config.RoleServer runs the daemon
	// here (today's default install), config.RoleClient dials serverURL. It is
	// not kind: the owner's half of a peer_auth install is a socket client of
	// a daemon that runs on this computer.
	role      string
	serverURL string
	// installed is the kind that was there before, and locked whether locked
	// projects (peer_auth) were already on.
	installed setupKind
	locked    bool
	answer    setupAnswer
	// change is the setupChange chosen on a re-run, "" for none.
	change string
}

// setupStep is one step of a kind's setup. Each is a task of its own; run is
// nil until it is built, and setup says so with manual, the way to do it by
// hand. A built step runs in its phase: before the installer's own steps (the
// binaries, the hooks, the service), or after them (after).
type setupStep struct {
	id string
	// label is the step's row label (at most 12 characters, the installer's).
	label string
	// what the step does, in one line.
	what string
	// manual is how to do it by hand today, or what happens instead.
	manual string
	// knob is the step's answer in the environment ("" for none). Set while
	// the step is not built, it stops the run: setup cannot do what it asks.
	knob string
	// fresh: a first install's step only (a re-run already has its data).
	fresh bool
	// peerAuth: macOS and Linux only (peer_auth), and only while no project
	// is locked on this computer.
	peerAuth bool
	after    bool
	run      func(*setupRun, *setupState) error
}

// setupSteps are the branch steps setup hands off to, by id. Their tasks:
// address, the Server's address and TLS; import, starting from a backup;
// locked-projects, the peer_auth setup engine (seamlessd peer-auth setup);
// clients, the Server's closing (what a Client needs); pairing, a Client's
// one-time code and its relay.
var setupSteps = map[string]setupStep{
	"address": {id: "address", label: "address",
		what:   "the address the other computers reach this one at, and its TLS certificate",
		manual: setupDocs + "guides/network-install/#step-1-widen-the-server"},
	"import": {id: "import", label: "import", knob: setupKnobImport, fresh: true,
		what:   "start from a backup instead of empty",
		manual: "seamlessd restore <backup>, once the install is up (" + setupDocs + "guides/data/#moving-machines)"},
	"locked-projects": {id: "locked-projects", label: "locked", peerAuth: true, after: true,
		what:   "locked projects (peer_auth): a project only the agents you approve can open",
		manual: setupDocs + "guides/peer-auth/#move-an-existing-install-over"},
	"clients": {id: "clients", label: "clients", after: true,
		what:   "what each {client} needs: this computer's address and key",
		manual: "seamlessd client-config prints it, once the address is set up (" + setupDocs + "guides/network-install/#step-3-pair-a-client)"},
	"pairing": {id: "pairing", label: "pairing", knob: setupKnobPairCode,
		what:   "pairing with a one-time code instead of the {server}'s own key",
		manual: "this {client} uses its {server}'s key, which seamlessd client-config prints on the {server}"},
}

// setupPlans is each kind's steps, in the order its questions come: the
// Server's address first, a Client's pairing.
var setupPlans = map[setupKind][]string{
	setupStandalone: {"import", "locked-projects"},
	setupServer:     {"address", "locked-projects", "import", "clients"},
	setupClient:     {"pairing"},
}

// setupStepsFor is kind's steps that apply: on a first install (fresh) or a
// re-run, on goos, with locked projects on or off.
func setupStepsFor(kind setupKind, fresh, locked bool, goos string) []setupStep {
	var out []setupStep
	for _, id := range setupPlans[kind] {
		s := setupSteps[id]
		if s.fresh && !fresh {
			continue
		}
		if s.peerAuth && (locked || !setupPeerAuthOS(goos)) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// setupPeerAuthOS reports whether peer_auth, and so locked projects, runs on
// goos.
func setupPeerAuthOS(goos string) bool { return goos == "darwin" || goos == "linux" }

// setupChange is a change setup offers on a re-run: to another kind (a
// Standalone install becoming the Server), or a step of the same kind.
type setupChange struct {
	id string
	// label is the change in the re-run's question, and doing in its summary.
	label, doing string
	to           setupKind
	// steps the change runs; nil means the steps of a re-run of kind to.
	steps []string
}

// setupChanges is what setup can change on an installed computer: a
// Standalone install can become the Server (not on Windows), and a computer
// that runs the daemon can turn locked projects on (macOS and Linux). A
// Client has nothing to change yet: its relay is not built.
func setupChanges(in setupInstall, goos string) []setupChange {
	var out []setupChange
	if in.kind == setupStandalone && goos != "windows" {
		out = append(out, setupChange{id: "server", to: setupServer,
			label: setupText("Make it the {server}, so other computers connect to it"),
			doing: setupText("becoming the {server}")})
	}
	if (in.kind == setupStandalone || in.kind == setupServer) && !in.locked && setupPeerAuthOS(goos) {
		out = append(out, setupChange{id: "locked-projects", to: in.kind, steps: []string{"locked-projects"},
			label: "Turn on locked projects (peer_auth)", doing: "turning on locked projects"})
	}
	return out
}

// setupChangeSteps is the steps change runs.
func setupChangeSteps(c setupChange, locked bool, goos string) []setupStep {
	if c.steps == nil {
		return setupStepsFor(c.to, false, locked, goos)
	}
	var out []setupStep
	for _, id := range c.steps {
		out = append(out, setupSteps[id])
	}
	return out
}

// setupChangeByID finds a change setupChanges offers.
func setupChangeByID(in setupInstall, goos, id string) (setupChange, bool) {
	for _, c := range setupChanges(in, goos) {
		if c.id == id {
			return c, true
		}
	}
	return setupChange{}, false
}

// setupRun is one run of setup: its environment, what is installed, and the
// terminal it may ask on.
type setupRun struct {
	goos    string
	environ []string
	facts   setupFacts
	// interactive: stdin is a terminal. Setup still asks only when asks().
	interactive bool
	in          *bufio.Reader
	out         io.Writer
	// secret reads one answer without echoing it (the Server's key).
	secret func() (string, error)
	// ensureClient writes this computer's client config, as install-hooks
	// --server-url does (config.EnsureClientConfig), and returns the path it
	// wrote ("" when the same config was already there).
	ensureClient func(serverURL, key string) (string, error)
}

// realSetupRun is a run on this process's terminal and machine.
func realSetupRun() *setupRun {
	r := &setupRun{
		goos:        runtime.GOOS,
		environ:     os.Environ(),
		facts:       setupFactsOf(realSetupHost()),
		interactive: stdinIsTerminal(),
		in:          bufio.NewReader(os.Stdin),
		out:         os.Stdout,
		ensureClient: func(serverURL, key string) (string, error) {
			_, wrote, err := config.EnsureClientConfig(serverURL, key)
			return wrote, err
		},
	}
	r.secret = func() (string, error) {
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(r.out)
		return string(raw), err
	}
	return r
}

// env is the value of the environment variable name, "" when it is unset or
// empty -- the installers' ${NAME:-} -- matched case-insensitively on Windows.
func (r *setupRun) env(name string) string {
	val := ""
	for _, kv := range r.environ {
		if k, v, ok := strings.Cut(kv, "="); ok && sameEnvName(r.goos, k, name) {
			val = v
		}
	}
	return val
}

// serverKnob is SEAMLESS_SERVER_URL as this OS's installer reads it: docs/install
// drops one trailing slash (${SERVER_URL%/}), install.ps1 every one
// (TrimEnd('/')). The installers keep their own reading; this one only has to
// agree with it on whether there is a server at all.
func (r *setupRun) serverKnob() string {
	v := r.env(setupKnobServer)
	if r.goos == "windows" {
		return strings.TrimRight(v, "/")
	}
	return strings.TrimSuffix(v, "/")
}

// knobs is every SEAMLESS_* variable set but SEAMLESS_CONFIG, the one the
// installers pin themselves.
func (r *setupRun) knobs() []string { return foreignSeamlessEnv(r.goos, r.environ) }

// asks reports whether setup may ask a question: a person is at the terminal
// and set no SEAMLESS_* variable. Anything set means a script is driving the
// run, and then the environment is the whole answer -- an absent one is
// today's install, a missing one an error naming its knob.
func (r *setupRun) asks() bool { return r.interactive && len(r.knobs()) == 0 }

// errSetupRefused is a setup that cannot do what was asked of it.
var errSetupRefused = errors.New("setup refused")

// setupRefusal is a refusal in the owner's words: main prints the line alone.
func setupRefusal(format string, args ...any) error {
	return &ownerError{msg: "seamlessd setup: " + setupTextf(format, args...), cause: errSetupRefused}
}

// setupDecide decides how this computer uses Seamless, in this order: the
// environment's answers, then what is installed (a re-run keeps it, or makes a
// change it asks for), then -- a first install with a person at the terminal --
// the questions. It writes nothing but the client config a Client answers at
// the terminal, which is install-hooks --server-url's own first step.
func setupDecide(r *setupRun) (setupState, error) {
	in := setupInstalledOf(r.facts)
	st := setupState{installed: in.kind, locked: in.locked, role: config.RoleServer}
	want, err := setupKindKnob(r)
	if err != nil {
		return st, err
	}
	if err := setupUnbuiltKnobs(r); err != nil {
		return st, err
	}
	if url := r.serverKnob(); url != "" {
		return setupFromServerKnob(r, st, want, url)
	}
	if in.kind != setupNone {
		return setupOnInstalled(r, st, in, want)
	}
	return setupOnFresh(r, st, want)
}

// setupKindKnob parses SEAMLESS_SETUP: setupNone when unset, else one of the
// kinds offered on this OS (case and surrounding space aside), else an error
// naming the accepted values.
func setupKindKnob(r *setupRun) (setupKind, error) {
	raw := r.env(setupKnobKind)
	if raw == "" {
		return setupNone, nil
	}
	v := strings.ToLower(strings.TrimSpace(raw))
	for _, k := range setupOffered(r.goos) {
		if setupWords[k].env == v {
			return k, nil
		}
	}
	if r.goos == "windows" && v == setupWords[setupServer].env {
		return setupNone, setupRefusal("%s=%s: "+setupWindowsWhy+"; set up the {server} on a macOS or Linux computer", setupKnobKind, raw)
	}
	return setupNone, setupRefusal("invalid %s=%q: valid values are %s", setupKnobKind, raw, setupEnvValues(r.goos, ", "))
}

// setupUnbuiltKnobs refuses a knob whose step is not built: setup cannot do
// what it asks, and quietly doing something else instead would leave the owner
// believing it was done. It runs before anything is decided, so the installers
// stop before they install anything.
func setupUnbuiltKnobs(r *setupRun) error {
	for _, id := range []string{"import", "pairing"} {
		s := setupSteps[id]
		if s.run != nil || r.env(s.knob) == "" {
			continue
		}
		if id == "pairing" {
			return setupRefusal("%s: pairing with a one-time code is not built into setup yet; pair this {client} with its {server}'s key: "+
				"%s and %s (seamlessd client-config on the {server} prints both)", s.knob, setupKnobServer, setupKnobKey)
		}
		return setupRefusal("%s: setup cannot %s yet; install without it, then by hand: %s", s.knob, setupText(s.what), setupText(s.manual))
	}
	return nil
}

// setupFromServerKnob is SEAMLESS_SERVER_URL, the installers' client knob: a
// Client of that server, whatever is installed (install-hooks keeps its own
// refusals), with the key from SEAMLESS_MCP_API_KEY. The installers check the
// key before they download anything; this repeats it for a run by hand.
func setupFromServerKnob(r *setupRun, st setupState, want setupKind, url string) (setupState, error) {
	if want != setupNone && want != setupClient {
		return st, setupRefusal("%s=%s and %s disagree: %s makes this computer a {client} of that {server}; drop one",
			setupKnobKind, setupWords[want].env, setupKnobServer, setupKnobServer)
	}
	if r.env(setupKnobKey) == "" {
		return st, setupRefusal("%s is set but %s is not: a {client} needs its {server}'s key as well as its URL. "+
			"Run  seamlessd client-config  on the {server} and use the line it prints", setupKnobServer, setupKnobKey)
	}
	if strings.ContainsFunc(url, setupControl) {
		return st, setupRefusal("%s holds a control character; set it to the {server}'s base URL (https://host:8081)", setupKnobServer)
	}
	st.kind, st.role, st.serverURL, st.answer = setupClient, config.RoleClient, url, setupFromEnv
	return st, nil
}

// setupOnInstalled is a re-run: the installed kind stays, or becomes what
// SEAMLESS_SETUP or the owner's answer changes it to.
func setupOnInstalled(r *setupRun, st setupState, in setupInstall, want setupKind) (setupState, error) {
	st.role, st.serverURL = in.role, in.serverURL
	if in.role == config.RoleClient && in.serverURL == "" {
		return st, setupRefusal("%s says role: client but sets no server_url; add the URL of the server this machine dials (server_url: https://host:8081), then re-run",
			tildePath(r.facts.configPath))
	}
	if strings.ContainsFunc(in.serverURL, setupControl) {
		return st, setupRefusal("%s's server_url holds a control character", tildePath(r.facts.configPath))
	}
	switch want {
	case setupNone:
		st.kind, st.answer = in.kind, setupKept
		changes := setupChanges(in, r.goos)
		if len(changes) == 0 || !r.asks() {
			return st, nil
		}
		c, ok, err := setupAskChange(r, in, changes)
		if err != nil {
			return st, err
		}
		if ok {
			st.kind, st.answer, st.change = c.to, setupAsked, c.id
		}
		return st, nil
	case in.kind:
		st.kind, st.answer = in.kind, setupFromEnv
		return st, nil
	}
	if c, ok := setupChangeByID(in, r.goos, "server"); ok && want == c.to {
		st.kind, st.answer, st.change = c.to, setupFromEnv, c.id
		return st, nil
	}
	return st, setupRefusal("%s", setupCannotChange(in, want))
}

// setupCannotChange says why setup does not make an installed kind another.
func setupCannotChange(in setupInstall, want setupKind) string {
	from := setupDescribe(in.kind, in.serverURL, in.locked)
	switch {
	case want == setupClient:
		return setupTextf("this computer is set up as %s and keeps its own data; setup does not make it a {client}. "+
			"Move its data to the {server} first, then pair it by hand: %sguides/data/#merging-two-instances", from, setupDocs)
	case in.kind == setupClient:
		return setupTextf("this computer is set up as %s: its data lives on that {server}, and setup does not change a {client} into %s. "+
			"To keep data here, take role: client and server_url out of its config by hand, then re-run", from, setupWords[want].name)
	default:
		return setupTextf("this computer is set up as %s, and setup does not make it %s; %s=%s keeps it as it is",
			from, setupWords[want].name, setupKnobKind, setupWords[in.kind].env)
	}
}

// setupOnFresh is a first install: SEAMLESS_SETUP's answer, else the
// questions for a person at the terminal, else today's default install.
func setupOnFresh(r *setupRun, st setupState, want setupKind) (setupState, error) {
	switch want {
	case setupStandalone, setupServer:
		st.kind, st.answer = want, setupFromEnv
		return st, nil
	case setupClient:
		return st, setupRefusal("%s=%s needs the {server} to connect to: set %s and %s too (seamlessd client-config on the {server} prints both)",
			setupKnobKind, setupWords[setupClient].env, setupKnobServer, setupKnobKey)
	}
	if !r.asks() {
		st.kind, st.answer = setupStandalone, setupDefault
		return st, nil
	}
	kind, err := setupAskKind(r)
	if err != nil {
		return st, err
	}
	st.kind, st.answer = kind, setupAsked
	if kind == setupClient {
		url, err := setupAskClient(r)
		if err != nil {
			return st, err
		}
		st.role, st.serverURL = config.RoleClient, url
	}
	return st, nil
}

// setupAskKind is the first install's question: Standalone or Networked, and
// then the Server or a Client. Enter takes Standalone, today's default
// install; Networked has no default, since a wrong guess between its two
// halves costs more than the question.
func setupAskKind(r *setupRun) (setupKind, error) {
	sa, sv, cl := setupWords[setupStandalone], setupWords[setupServer], setupWords[setupClient]
	fmt.Fprintf(r.out, "\n%s\n", bold("How will you use Seamless?"))
	setupOption(r.out, 1, sa.name, setupText(sa.line))
	if r.goos == "windows" {
		setupOption(r.out, 2, setupNetworkedWord.name, setupText("connect to a {server} on another computer, as its {client}"))
		fmt.Fprintf(r.out, "      %s\n", dim(setupText(setupWindowsWhy)+"."))
	} else {
		setupOption(r.out, 2, setupNetworkedWord.name, setupText(setupNetworkedWord.line))
	}
	n, err := setupChoose(r, "Enter 1 or 2 [1]: ", 1, [][]string{
		{sa.env, strings.ToLower(sa.name)},
		{strings.ToLower(setupNetworkedWord.name)},
	})
	if err != nil {
		return setupNone, err
	}
	if n == 1 {
		return setupStandalone, nil
	}
	if r.goos == "windows" {
		return setupClient, nil
	}
	fmt.Fprintf(r.out, "\n%s\n", bold(setupText("Set up the {server} first: each {client} needs its address and key.")))
	setupOption(r.out, 1, sv.name, setupText(sv.line))
	setupOption(r.out, 2, cl.name, setupText(cl.line))
	n, err = setupChoose(r, "Enter 1 or 2: ", 0, [][]string{
		{sv.env, strings.ToLower(sv.name)},
		{cl.env, strings.ToLower(cl.name)},
	})
	if err != nil {
		return setupNone, err
	}
	if n == 1 {
		return setupServer, nil
	}
	return setupClient, nil
}

// setupOption prints one numbered choice.
func setupOption(w io.Writer, n int, name, line string) {
	fmt.Fprintf(w, "  %s %-11s %s\n", dim("["+strconv.Itoa(n)+"]"), name, dim(line))
}

// setupChoose reads a numbered choice: a number, or one of the words for it.
// An empty answer takes def (0: no default, asked again); end of input takes
// def too, or stops the run when there is none, so a closed terminal cannot
// loop forever.
func setupChoose(r *setupRun, prompt string, def int, words [][]string) (int, error) {
	for {
		fmt.Fprint(r.out, prompt)
		line, err := r.in.ReadString('\n')
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer == "" && def > 0 {
			return def, nil
		}
		if n, ok := setupChoice(answer, words); ok {
			return n, nil
		}
		if err != nil {
			if def > 0 {
				return def, nil
			}
			return 0, setupRefusal("no answer: set %s=%s for a run without questions", setupKnobKind, setupEnvValues(r.goos, "|"))
		}
		fmt.Fprintf(r.out, "  please enter a number from 1 to %d\n", len(words))
	}
}

// setupChoice maps an answer to its choice number.
func setupChoice(answer string, words [][]string) (int, bool) {
	if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(words) {
		return n, true
	}
	for i, ws := range words {
		if answer != "" && slices.Contains(ws, answer) {
			return i + 1, true
		}
	}
	return 0, false
}

// setupAskClient asks a Client's two answers -- its Server's address and the
// key -- and writes the client config with them, as install-hooks
// --server-url would have from SEAMLESS_SERVER_URL and SEAMLESS_MCP_API_KEY:
// the key goes from the terminal to the 0600 config and nowhere else.
func setupAskClient(r *setupRun) (string, error) {
	fmt.Fprintf(r.out, "\n%s\n", dim(setupText("The {server}'s address and key: run  seamlessd client-config  on the {server}.")))
	var url string
	for {
		fmt.Fprint(r.out, setupText("{server} address (https://host:8081): "))
		line, err := r.in.ReadString('\n')
		url = strings.TrimSuffix(strings.TrimSpace(line), "/")
		if url != "" {
			problem := setupServerURLProblem(url)
			if problem == "" {
				break
			}
			fmt.Fprintf(r.out, "  %s\n", problem)
		}
		if err != nil {
			return "", setupRefusal("no {server} address: set %s=%s, %s and %s for a run without questions",
				setupKnobKind, setupWords[setupClient].env, setupKnobServer, setupKnobKey)
		}
	}
	if strings.HasPrefix(url, "http://") {
		fmt.Fprintf(r.out, "  %s %s\n", yellow("warning:"),
			dim("plain http: the key and every memory cross the network unencrypted; keep it inside a trusted LAN"))
	}
	var key string
	for key == "" {
		fmt.Fprint(r.out, setupText("{server} key (the input is hidden): "))
		raw, err := r.secret()
		key = strings.TrimSpace(raw)
		if key == "" && err != nil {
			return "", setupRefusal("no key: set %s=%s, %s and %s for a run without questions",
				setupKnobKind, setupWords[setupClient].env, setupKnobServer, setupKnobKey)
		}
		if strings.ContainsFunc(key, setupControl) {
			fmt.Fprintln(r.out, "  the key holds a control character; paste it again")
			key = ""
		}
	}
	wrote, err := r.ensureClient(url, key)
	if err != nil {
		return "", fmt.Errorf("seamlessd.setup: %w", err)
	}
	if wrote != "" {
		setupRow(r.out, "config", "wrote the client config "+dim("-> "+tildePath(wrote)))
	}
	return url, nil
}

// setupServerURLProblem says what is wrong with a Server address typed at the
// terminal, "" when nothing is: an http(s) base URL the config accepts.
func setupServerURLProblem(url string) string {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "the address starts with https:// (or http://), like https://host:8081"
	}
	if strings.ContainsFunc(url, setupControl) {
		return "the address holds a control character"
	}
	cfg := config.Defaults()
	cfg.Role, cfg.AdvertisedURL = config.RoleClient, url
	if err := cfg.Validate(); err != nil {
		return strings.TrimPrefix(err.Error(), "config: ")
	}
	return ""
}

// setupControl reports a control character, which no URL or key carries and
// which the state file's lines could not.
func setupControl(c rune) bool { return c < 0x20 || c == 0x7f }

// setupAskChange is a re-run's question: keep the install as it is (Enter),
// or make one of the changes setup offers.
func setupAskChange(r *setupRun, in setupInstall, changes []setupChange) (setupChange, bool, error) {
	fmt.Fprintf(r.out, "\n%s\n", bold("This computer is set up as "+setupDescribe(in.kind, in.serverURL, in.locked)+"."))
	words := [][]string{{"keep"}}
	fmt.Fprintf(r.out, "  %s Keep it as it is\n", dim("[1]"))
	for i, c := range changes {
		fmt.Fprintf(r.out, "  %s %s\n", dim("["+strconv.Itoa(i+2)+"]"), c.label)
		words = append(words, []string{c.id})
	}
	n, err := setupChoose(r, fmt.Sprintf("Enter 1-%d [1]: ", len(words)), 1, words)
	if err != nil || n == 1 {
		return setupChange{}, false, err
	}
	return changes[n-2], true, nil
}

// setupRow prints one row the way the installers print their steps: a label
// in 12 columns, then the value.
func setupRow(w io.Writer, label, value string) {
	fmt.Fprintf(w, "  %-12s %s\n", label, value)
}

// setupRowCont is the indent of a row's continuation line.
const setupRowCont = "               "

// setupSummary is the row a decision ends with.
func setupSummary(r *setupRun, st setupState) {
	desc := setupDescribe(st.kind, st.serverURL, st.locked)
	switch {
	case st.change != "":
		desc = setupDescribe(st.installed, st.serverURL, st.locked)
		if c, ok := setupChangeByID(setupInstall{kind: st.installed, locked: st.locked}, r.goos, st.change); ok {
			desc += dim(", " + c.doing)
		}
	case st.answer == setupKept:
		desc += dim(", as installed")
	case st.answer == setupFromEnv:
		knob := setupKnobKind
		if st.kind == setupClient && r.env(setupKnobKind) == "" {
			knob = setupKnobServer
		}
		desc += dim(" (" + knob + ")")
	}
	setupRow(r.out, "setup", desc)
}

// setupStepsLeft is the steps a run's decision leaves for after the
// installer's own: a first install's choice brings its kind's steps, a change
// its own, and keeping what is installed nothing.
func setupStepsLeft(st setupState, goos string) []setupStep {
	switch {
	case st.change != "":
		c, ok := setupChangeByID(setupInstall{kind: st.installed, locked: st.locked}, goos, st.change)
		if !ok {
			return nil
		}
		return setupChangeSteps(c, st.locked, goos)
	case st.installed == setupNone && (st.answer == setupAsked || st.answer == setupFromEnv):
		return setupStepsFor(st.kind, true, st.locked, goos)
	}
	return nil
}

// setupHandOff runs the built steps of phase after (true) or before (false)
// and, after, says of each step not built yet that setup does not do it and
// how to do it by hand: one row each.
func setupHandOff(r *setupRun, st *setupState, steps []setupStep, after bool) error {
	var manual []setupStep
	for _, s := range steps {
		switch {
		case s.run != nil && s.after == after:
			if err := s.run(r, st); err != nil {
				return fmt.Errorf("seamlessd.setup: %s: %w", s.id, err)
			}
		case s.run == nil && after:
			manual = append(manual, s)
		}
	}
	if len(manual) == 0 {
		return nil
	}
	which := "these " + setupWords[st.kind].name + " steps"
	if len(manual) == 1 {
		which = "this " + setupWords[st.kind].name + " step"
	}
	fmt.Fprintln(r.out)
	setupRow(r.out, "by hand", "setup does not do "+which+" yet:")
	for _, s := range manual {
		setupRow(r.out, s.label, setupText(s.what))
		fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim(setupText(s.manual)))
	}
	return nil
}

// runSetup is `seamlessd setup`.
func runSetup(args []string) error { return runSetupWith(args, realSetupRun) }

// setupFlags is setup's flag set: --state and --finish. Its usage goes to
// out, which for the command is stdout, where the installers' probes read it
// plainly.
func setupFlags(out io.Writer, goos string) (*flag.FlagSet, *string, *bool) {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(out)
	state := fs.String("state", "", "the installers' hand-off: decide, and write the answer to `FILE` for the installer")
	finish := fs.Bool("finish", false, "with --state: the installer's own steps are done; finish setup's")
	fs.Usage = func() { setupUsage(fs.Output(), fs, goos) }
	return fs, state, finish
}

// runSetupWith is runSetup over a run newRun builds, once the flags parsed.
func runSetupWith(args []string, newRun func() *setupRun) error {
	fs, state, finish := setupFlags(os.Stdout, runtime.GOOS)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("seamlessd.setup: unexpected argument %q", fs.Arg(0))
	}
	if *finish && *state == "" {
		return errors.New("seamlessd.setup: --finish needs the --state the installer passed before")
	}
	r := newRun()
	switch {
	case *finish:
		return setupFinish(r, *state)
	case *state != "":
		return setupForInstaller(r, *state)
	default:
		return setupDirect(r)
	}
}

// setupUsage is `seamlessd setup -h`. Its last line is setupProtocolLine,
// which the installers look for.
func setupUsage(w io.Writer, fs *flag.FlagSet, goos string) {
	fmt.Fprint(w, setupTextf(`usage: seamlessd setup

How this computer uses Seamless: {standalone}, or {networked} -- the {server},
or a {client} of it. Run by itself, setup says how this computer is set up and
offers the changes it can make. The installers run it to ask how a new install
will be used, and a script answers it in the environment:

  %s=%s
  %s, %s   a {client}'s {server}, and its key
  %s, %s     not built into setup yet

`, setupKnobKind, setupEnvValues(goos, "|"), setupKnobServer, setupKnobKey, setupKnobPairCode, setupKnobImport))
	fs.PrintDefaults()
	fmt.Fprintf(w, "\n%s\n", setupProtocolLine)
}

// setupForInstaller is setup's first half under an installer (--state): the
// decision, the built steps that come before the installer's own, and the
// state file the installer acts on.
func setupForInstaller(r *setupRun, path string) error {
	st, err := setupDecide(r)
	if err != nil {
		return err
	}
	setupSummary(r, st)
	if err := setupHandOff(r, &st, setupStepsLeft(st, r.goos), false); err != nil {
		return err
	}
	return writeSetupState(path, st)
}

// setupFinish is setup's second half (--finish), once the installer's steps
// are done: the steps the state leaves.
func setupFinish(r *setupRun, path string) error {
	st, err := readSetupState(path)
	if err != nil {
		return err
	}
	return setupHandOff(r, &st, setupStepsLeft(st, r.goos), true)
}

// setupDirect is `seamlessd setup` run by the owner: how this computer is set
// up, and the changes setup offers, which it hands off at once. A computer
// with nothing installed is the installer's to set up.
func setupDirect(r *setupRun) error {
	if setupInstalledOf(r.facts).kind == setupNone {
		if _, err := setupKindKnob(r); err != nil {
			return err
		}
		if err := setupUnbuiltKnobs(r); err != nil {
			return err
		}
		setupRow(r.out, "setup", "nothing is set up on this computer yet")
		fmt.Fprintf(r.out, "%s%s\n%s%s\n", setupRowCont, dim("the installer sets it up, and asks how you will use Seamless:"),
			setupRowCont, green(setupInstallCommand(r.goos)))
		return nil
	}
	st, err := setupDecide(r)
	if err != nil {
		return err
	}
	setupSummary(r, st)
	if st.answer == setupKept && !r.asks() {
		if changes := setupChanges(setupInstalledOf(r.facts), r.goos); len(changes) > 0 {
			why := "at a terminal"
			if knobs := r.knobs(); len(knobs) > 0 {
				why = "with no SEAMLESS_* set (" + strings.Join(knobs, ", ") + " is)"
			}
			fmt.Fprintf(r.out, "%s%s\n", setupRowCont, dim("to change it, run seamlessd setup "+why))
		}
	}
	steps := setupStepsLeft(st, r.goos)
	if err := setupHandOff(r, &st, steps, false); err != nil {
		return err
	}
	return setupHandOff(r, &st, steps, true)
}

// setupInstallCommand is the installer one-liner for goos, as every page
// teaches it.
func setupInstallCommand(goos string) string {
	if goos == "windows" {
		return "irm https://thereisnospoon.org/install.ps1 | iex"
	}
	return "curl -fsSL https://thereisnospoon.org/install | sh"
}
