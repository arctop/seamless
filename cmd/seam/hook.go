package main

// seam hook -- forward an agent-client hook payload (stdin) to seamlessd and
// copy the JSON response back (stdout), so a `command` hook can drive the same
// server logic an `http` hook would. Claude Code requires command/mcp_tool hooks
// for SessionStart; Codex uses command hooks for its entire Seamless profile.
//
// Agent clients invoke this command; the owner does not. That one fact shapes
// every decision in this file: a hook must never block the session it serves.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/agentproc"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/gitread"
)

// hookEvent is one event seam forwards: the `seam hook` argument, the endpoint
// it posts to, how long it re-dials a daemon it cannot dial, and how long the
// whole hook may take.
type hookEvent struct {
	event    string
	endpoint string
	// dialBudget is hookFullDialBudget or hookShortDialBudget: every client's,
	// unless hookClientDialBudgets overrides it for one (see dialBudgetFor).
	dialBudget time.Duration
	// deadline is hookDeadline or hookPromptDeadline, for every client: each
	// gives an event the same timeout.
	deadline time.Duration
}

// hookEvents is every event seam forwards. A slice rather than a map: it is the
// canonical set behind the validation and the help text, and a map would order
// the events differently on every run.
//
// internal/hooks' seamlessHooks and codexHooks tables are what this mirrors. The
// CLI cannot import that package -- it would drag the store, the retriever, and
// SQLite into a binary whose job is one HTTP POST -- so hook_test.go pins this
// copy against them instead: each endpoint against the one the installer wires,
// and each client's dial budget and each deadline against the timeout that
// client gives the hook. That pin is load-bearing: because a hook fails open,
// drift here is a silent no-op rather than an error.
//
// user-prompt-submit is the one event whose shape follows the transport: on an
// http base URL the installer wires it as an http hook (reliable mid-turn), and
// on an https one profileForBaseURL turns it into a command hook so no bearer key
// lands in settings.json. seam has always accepted the command form, so it stays
// either way.
var hookEvents = []hookEvent{
	{"session-start", "/api/hooks/session-start", hookFullDialBudget, hookDeadline},
	{"user-prompt-submit", "/api/hooks/user-prompt-submit", hookShortDialBudget, hookPromptDeadline},
	{"session-end", "/api/hooks/session-end", hookFullDialBudget, hookDeadline},
	{"post-tool-use", "/api/hooks/post-tool-use", hookFullDialBudget, hookDeadline},
	{"subagent-start", "/api/hooks/subagent-start", hookFullDialBudget, hookDeadline},
	{"subagent-stop", "/api/hooks/subagent-stop", hookFullDialBudget, hookDeadline},
	{"permission-request", "/api/hooks/permission-request", hookFullDialBudget, hookDeadline},
	// Codex-only: its per-turn end signal (heartbeat + provisional harvest). Codex
	// has no SessionEnd, so the codex install profile wires `seam hook stop`.
	{"stop", "/api/hooks/stop", hookShortDialBudget, hookDeadline},
}

// hookClientDialBudget gives one client its own dial budget for one event.
type hookClientDialBudget struct {
	event  string // a hookEvents event
	client string // a hookClients value
	budget time.Duration
}

// hookClientDialBudgets overrides an event's dial budget for one client, where
// that client's hook does different work under the same event name. Every
// (event, client) pair not listed here spends the event's own dialBudget.
var hookClientDialBudgets = []hookClientDialBudget{
	// Claude Code's SubagentStop caches a planning subagent's report, which
	// nothing redoes; Codex's only heartbeats the parent session.
	{"subagent-stop", hookClientCodex, hookShortDialBudget},
}

// dialBudgetFor is the dial budget the event spends for a --client value: the
// client's override in hookClientDialBudgets, or else the event's own. An absent
// --client ("") is Claude Code, as it is to the daemon: the Claude Code profile
// installs every hook without one.
func (h hookEvent) dialBudgetFor(client string) time.Duration {
	if client == "" {
		client = hookClientClaudeCode
	}
	for _, o := range hookClientDialBudgets {
		if o.event == h.event && o.client == client {
			return o.budget
		}
	}
	return h.dialBudget
}

// lookupHookEvent returns the table entry for an event.
func lookupHookEvent(event string) (hookEvent, bool) {
	for _, h := range hookEvents {
		if h.event == event {
			return h, true
		}
	}
	return hookEvent{}, false
}

// hookEventNames lists the events for help and error text, in table order.
func hookEventNames() string {
	names := make([]string, len(hookEvents))
	for i, h := range hookEvents {
		names[i] = h.event
	}
	return strings.Join(names, ", ")
}

// clientQueryParam is the query key that carries the client discriminator to the
// daemon. It mirrors internal/hooks' unexported constant of the same name (this
// binary must not import that package -- see the file header). Both are the
// literal "client", and the forwarding tests pin the resulting query contract.
const clientQueryParam = "client"

// The --client values, as internal/hooks names its Client constants.
const (
	hookClientClaudeCode = "claude-code"
	hookClientCodex      = "codex"
)

// hookClients is the CLI's thin-binary copy of internal/hooks.HookClients. The
// test-only hooks import pins the two sets exactly; production seam avoids the
// SQLite dependency graph that importing internal/hooks would add.
var hookClients = []string{hookClientClaudeCode, hookClientCodex}

// The identity query keys, mirroring internal/hooks' unexported constants (same
// reason as clientQueryParam: this binary must not import that package). The
// order is the one hooks.IdentityQueryParams returns, and the pin test compares
// the two lists.
//
// They carry what only THIS process can see: which machine the hook fired on,
// and the git identity of the directory it fired in. A daemon on another machine
// cannot derive either -- its own disk answers a different question -- so an
// absent value makes it fall back to treating the hook as local, which is
// exactly right for the single-machine install every one of these was written
// for.
var hookIdentityParams = []string{"host", "repo_root", "main_root", "origin", "agent_process"}

// The deadlines: how long a hook may take in all, whatever the network does,
// before it gives up and fails open. A client kills a hook that outlives the
// timeout it gives it -- a timeout warning in place of a quiet fail-open, on
// every prompt in user-prompt-submit's case -- so each deadline sits
// hookDeadlineMargin under the timeout every client gives that event
// (hook_test.go pins it against internal/hooks): 10s for every command hook
// but user-prompt-submit, which Codex, and Claude Code on an https install,
// kill at 5s.
//
// A deadline covers everything the hook does from the moment it knows its
// event: reading the payload, loading config, every attempt and the waits
// between them, and relaying the reply. The retry spends from it rather than
// adding to it. The dial budgets below cannot stand in for it, because a budget
// bounds when the last attempt may START. A dial to a host that drops packets
// -- a client machine away from the LAN its server is on -- hangs until
// config.DialTimeout (5s) unless the deadline ends the request first, and a
// daemon that accepts and then says nothing hangs for as long as it likes. At
// the deadline the request returns; net/http lets a dial still in flight run on
// in the background, detached from the request, and this process's exit ends
// it.
//
// The margin is the part of the client's clock the deadline cannot see:
// starting this process before runHook runs, and exiting it after. Measured on
// macOS, that is about 10ms, and 0.2s more the first time a new binary runs (as
// it does after an update), so a second leaves room for a loaded machine and
// for a Codex hook's shell.
const (
	hookDeadlineMargin = time.Second
	hookDeadline       = 10*time.Second - hookDeadlineMargin
	hookPromptDeadline = 5*time.Second - hookDeadlineMargin
)

// The dial budgets: how long a hook re-dials a daemon that cannot be dialed
// (dialretry.go) before it fails open. Each event takes one in hookEvents, and
// a client whose hook does different work under that name may take the other
// (hookClientDialBudgets), by what losing the payload to an update's restart
// (about 2s on macOS) would cost, weighed against the delay: a daemon that is
// down for good costs every hook its whole budget.
//
// hookFullDialBudget spans the restart, for the hooks whose loss outlives the
// turn: the session-start briefing, the session-end harvest, a plan capture or
// approval, a subagent's constraints briefing, and Claude Code's subagent-stop
// (a planning subagent's cached report, which nothing redoes).
//
// hookShortDialBudget rides out only a blip, for the hooks that fire often
// enough that the delay would be paid most, and whose loss is small:
// user-prompt-submit (one prompt's recall injection), stop (Codex's per-turn
// heartbeat and provisional findings harvest, which the next turn's stop
// supersedes) and Codex's subagent-stop (a heartbeat of the parent session). A
// restart may drop one of these.
//
// Each budget is at most half the timeout its client gives that hook
// (hook_test.go pins it against internal/hooks), so a hook whose dials are all
// refused -- a daemon that is down on a host that answers -- fails open well
// before its deadline. A dial that hangs is the deadline's to end, not the
// budget's. An attempt that connects at the end of its budget still has the
// rest of its deadline, at least 3s, to be served: more than the 2s the daemon
// gives itself to assemble a briefing or a recall.
const (
	hookFullDialBudget  = 5 * time.Second
	hookShortDialBudget = time.Second
)

// hookOpts carries the flags for `seam hook`.
type hookOpts struct {
	config string // --config: abs seamless.yaml the installer bakes in (see bindHook)
	client string // --client: canonical agent CLI discriminator; "" => absent/Claude Code

	// retry builds the dial-retry policy for the event's budget. Not a flag: nil
	// is newDialRetry; tests pass a fake clock's policy.
	retry func(budget time.Duration) *dialRetry
	// httpClient builds the client the payload is sent through. Not a flag: nil
	// is cfg.HTTPClient(0), the one constructor for a dial that carries the
	// bearer key (constraint seamless-http-client-is-config-httpclient); a test
	// replaces that client's dialer with one that hangs.
	httpClient func(cfg config.Config) (*http.Client, error)
}

// bindHook registers --config and --client. install-hooks writes --config into
// every command hook so the hook resolves config from any cwd: exec-form command
// hooks carry no environment, so this flag replaces the old SEAMLESS_CONFIG env
// prefix. runHook exports it back to SEAMLESS_CONFIG (config.Load's documented
// override) before loading, keeping every command's cwd-relative search otherwise
// unchanged. --client rides on the forwarded request as ?client=<value> so the
// daemon can pick the right per-client payload adapter; the codex install profile
// sets it, and an omitted flag keeps every Claude Code hook request unchanged.
func bindHook(fs *flag.FlagSet) *hookOpts {
	o := &hookOpts{}
	fs.StringVar(&o.config, "config", "", "path to seamless.yaml, so the hook resolves config from any cwd")
	fs.Var(&enumValue{val: &o.client, valid: hookClients}, "client",
		"agent CLI this hook fires for (`CLIENT`); valid: "+strings.Join(hookClients, ", ")+"; default Claude Code")
	return o
}

// hookCmd declares loose positional arity so event misuse lands in runHook, which
// fails open. --client uses the shared enum flag machinery and usageExit keeps
// that parse error at 1 too: exit 2 would block the agent session.
var hookCmd = spec("hook", groupHooks, "forward the stdin hook payload to seamlessd",
	atLeast(0, "EVENT"), bindHook, runHook).
	withLong("events: " + hookEventNames() +
		"\ndial retry: " + hookDurationsHelp(hookEvent.dialBudgetFor) +
		"\ndeadline: " + hookDurationsHelp(func(h hookEvent, _ string) time.Duration { return h.deadline }) + `

Your agent client (Claude Code or Codex) invokes this; it is not run by hand.
post-tool-use is Claude Code only and fires machine-wide
on every Write/Edit, so it is pre-filtered locally: a non-plan file never reaches
the network.

A runtime failure (unreadable stdin, no config, server down) is reported on
stderr and exits 0 -- a hook must never block the session it serves. A daemon
that cannot be dialed (restarting after an update) is retried first, for the
event's dial-retry budget above, which depends on --client where the line
names one; a payload that may have reached it is never sent twice. Whatever
the network does (a dial that hangs, a daemon that never answers), the hook
gives up at the event's deadline above, which sits inside the timeout its
client gives it, so it fails open before its client kills it. An unknown
event or client is an install/configuration bug and exits 1, never 2.`)

// hookDurationsHelp renders one per-event, per-client duration for the help
// text (of is dialBudgetFor, or the deadline), longest first, each followed by
// the events that take it in table order. An event whose duration depends on
// the client appears under each of its durations, naming the clients that take
// it there. It asks of about every event and client, so the page cannot drift
// from what runs.
func hookDurationsHelp(of func(h hookEvent, client string) time.Duration) string {
	events := map[time.Duration][]string{}
	for _, h := range hookEvents {
		clients := map[time.Duration][]string{}
		for _, c := range hookClients {
			d := of(h, c)
			clients[d] = append(clients[d], c)
		}
		// One entry per duration for this event, so each list stays in table
		// order whatever order the map yields its keys in.
		for d, cs := range clients {
			name := h.event
			if len(cs) < len(hookClients) {
				name += " (" + strings.Join(cs, ", ") + ")"
			}
			events[d] = append(events[d], name)
		}
	}
	durations := slices.SortedFunc(maps.Keys(events), func(a, b time.Duration) int { return cmp.Compare(b, a) })
	parts := make([]string, len(durations))
	for i, d := range durations {
		parts[i] = d.String() + " for " + strings.Join(events[d], ", ")
	}
	return strings.Join(parts, "; ")
}

func runHook(ctx context.Context, e *env, o *hookOpts, pos []string) error {
	// Arity is not enforced by the spec (see hookCmd), so the handler owns the
	// empty case. Both messages name the valid set rather than a "want" blob.
	if len(pos) == 0 {
		return fmt.Errorf("missing hook event: valid values are %s", hookEventNames())
	}
	event := pos[0]
	h, ok := lookupHookEvent(event)
	if !ok {
		return fmt.Errorf("unknown hook event %q: valid values are %s", event, hookEventNames())
	}
	ep := h.endpoint

	// One deadline over everything from here on (see hookDeadline). It starts
	// now rather than at the forward because the client's clock started when it
	// spawned this process: the payload read, the config load and
	// session-start's git reads spend from it too. Cancelling the parent ctx
	// still ends a retry wait at once.
	ctx, cancel := context.WithTimeout(ctx, h.deadline)
	defer cancel()

	// A stdin failure leaves nothing to forward. Report it and exit 0: a hook
	// must never block the agent (the same contract as every failure below).
	payload, err := io.ReadAll(e.stdin)
	if err != nil {
		fmt.Fprintln(e.stderr, "seam hook: read stdin:", err)
		return nil
	}

	// PostToolUse fires machine-wide on every Write/Edit; drop non-plan events
	// here, before any config load or network round-trip.
	if event == "post-tool-use" && !shouldForwardPostToolUse(payload, defaultPlansDir()) {
		return nil
	}

	// --config is config.Load's documented $SEAMLESS_CONFIG override, moved out of
	// the shell (exec-form hooks carry no env). Setting it in this short-lived hook
	// process is safe and keeps loadConfig's search order the single code path.
	if o.config != "" {
		if err := os.Setenv("SEAMLESS_CONFIG", o.config); err != nil {
			fmt.Fprintln(e.stderr, "seam hook: set config path:", err)
			return nil
		}
	}

	cfg, err := e.loadConfig()
	if err != nil {
		fmt.Fprintln(e.stderr, "seam hook: load config:", err)
		return nil
	}
	// --client rides as ?client=<value> so the daemon selects the per-client
	// payload adapter; the machine identity rides beside it. Both are out of band
	// because the body is the agent client's schema, not ours.
	q := url.Values{}
	if o.client != "" {
		q.Set(clientQueryParam, o.client)
	}
	for k, v := range identityParams(event, payload) {
		q.Set(k, v)
	}
	if len(q) > 0 {
		ep += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.ServerURL()+ep, bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintln(e.stderr, "seam hook:", err)
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+cfg.MCP.APIKey)
	req.Header.Set("Content-Type", "application/json")

	// No per-request timeout on the client: ctx carries the hook's deadline, and
	// a client timeout would start over on every retry.
	newClient := o.httpClient
	if newClient == nil {
		newClient = func(cfg config.Config) (*http.Client, error) { return cfg.HTTPClient(0) }
	}
	client, err := newClient(cfg)
	if err != nil {
		// Same degradation as a transport failure: a hook must never block the
		// agent, so the reason goes to stderr and the turn continues.
		fmt.Fprintln(e.stderr, "seam hook:", err)
		return nil
	}
	retry := o.retry
	if retry == nil {
		retry = newDialRetry
	}
	// Only a dial-phase failure is retried, and only for the event's budget for
	// this client. A payload that may have reached the daemon is never sent
	// again: session-start, a plan capture, a prompt record would each be applied
	// twice.
	resp, err := retry(h.dialBudgetFor(o.client)).do(client, req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// The error says only that a deadline passed; name which one, so a
			// hook that keeps hitting it reads as a network that hangs, not a bug.
			fmt.Fprintf(e.stderr, "seam hook: %s gave up at its %s deadline: %v\n", event, h.deadline, err)
			return nil
		}
		fmt.Fprintln(e.stderr, "seam hook:", err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	// Relay whatever arrived; a copy failure means stdout is gone, and a hook
	// must never block the agent by failing here.
	_, _ = io.Copy(e.stdout, resp.Body) //nolint:errcheck // a hook must not fail on a broken stdout
	return nil
}

// identityParams resolves the machine identity to append to a forwarded hook.
//
// The host goes on every hook: it is what tells the daemon whether the paths in
// this payload are on its own disk. The repository roots go on session-start
// only, because that is the single hook that PLACES a working directory in a
// project; every other hook resolves through the map that placement already
// grew. Resolving them is a handful of Lstats plus one config read, and a hook
// fires on every turn -- so the cheap half runs always and the rest runs once.
//
// Every value is best-effort: an unreadable hostname, an unparseable body, or a
// cwd outside any repo simply omits the param, and the daemon reads the absence
// as "an older client on my own machine" (see internal/hooks/adapter.go).
func identityParams(event string, payload []byte) map[string]string {
	out := map[string]string{}
	if host := config.Hostname(); host != "" {
		out["host"] = host
	}
	if event != "session-start" {
		return out
	}
	// The agent that ran this hook. The daemon stamps it on the ambient session,
	// and the same agent's MCP connection names it on every request, which is
	// what binds the two without a session_start call. Before the cwd checks: a
	// session outside any repository is still that agent's session.
	if proc, ok := agentproc.Anchor(); ok {
		out["agent_process"] = proc
	}
	var body struct {
		CWD string `json:"cwd"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || strings.TrimSpace(body.CWD) == "" {
		return out
	}
	root := gitread.RepoRoot(body.CWD)
	if root == "" {
		return out // not inside a repo: nothing to place, nothing to send
	}
	// A linked worktree is a checkout of the main repository, not a repository of
	// its own, and project identity keys on the main checkout (store.
	// RegisterProjectForCWD). Resolving that here is what lets the daemon skip
	// reading a filesystem it may not own.
	main := gitread.MainWorktreeRoot(root)
	out["repo_root"] = root
	out["main_root"] = main
	if origin := gitread.OriginURL(main); origin != "" {
		out["origin"] = origin
	}
	return out
}
