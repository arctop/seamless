package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// lcOrigin is the origin every test checkout shares.
const lcOrigin = "git@github.com:example/example-app.git"

// Object names for the tests' ref updates.
const (
	lcOidA    = "1111111111111111111111111111111111111111"
	lcOidB    = "2222222222222222222222222222222222222222"
	lcOidZero = "0000000000000000000000000000000000000000"
)

// lcFakeRepo makes a checkout at a fresh folder whose origin is origin, with
// refs/heads/main at lcOidA, and returns its real root.
func lcFakeRepo(t *testing.T, origin string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "example-app")
	git := filepath.Join(root, ".git")
	require.NoError(t, os.MkdirAll(filepath.Join(git, "refs", "heads"), 0o755))
	cfg := "[core]\n\tbare = false\n"
	if origin != "" {
		cfg += "[remote \"origin\"]\n\turl = " + origin + "\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(git, "config"), []byte(cfg), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(git, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(git, "refs", "heads", "main"), []byte(lcOidA+"\n"), 0o644))
	real, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	return real
}

// lcLockOpts shapes one lock in a locks_list answer.
type lcLockOpts struct {
	refs         []string
	held, mine   bool
	lapsed       bool
	holder       string // holder_session; "" leaves it out, as for a hidden holder
	state        string // "" = active
	label        string // "" = example-app main
	omitHeldByMe bool   // an older server: no held_by_caller at all
}

// lcLock renders one lock as F3's JSON.
func lcLock(o lcLockOpts) string {
	if o.state == "" {
		o.state = "active"
	}
	if o.label == "" {
		o.label = "example-app main"
	}
	if o.refs == nil {
		o.refs = []string{"refs/heads/main"}
	}
	refs := `"` + strings.Join(o.refs, `","`) + `"`
	j := fmt.Sprintf(`{"id":"01KLCLOCKLCLOCKLCLOCKLCLOC","label":%q,"repo":"example-app","name":"main","refs":[%s],"state":%q,"held":%t`,
		o.label, refs, o.state, o.held)
	if !o.omitHeldByMe {
		j += fmt.Sprintf(`,"held_by_caller":%t`, o.mine)
	}
	if o.lapsed {
		j += `,"lapsed":true`
	}
	if o.held || o.lapsed {
		j += `,"since":"2026-11-15T18:00:00.000000000Z","until":"2026-11-15T18:15:00.000000000Z"`
	}
	if o.holder != "" {
		j += fmt.Sprintf(`,"holder":"01KLCHOLDER","holder_session":%q`, o.holder)
	}
	return j + `,"aliases":[],"projects":[]}`
}

// lcAnswer renders a locks_list answer: the locks, and caller_bound when
// bound is not nil (nil leaves it out, as a server that predates it does).
func lcAnswer(bound *bool, locks ...string) string {
	out := `{"locks":[` + strings.Join(locks, ",") + `]`
	if bound != nil {
		out += fmt.Sprintf(`,"caller_bound":%t`, *bound)
	}
	return out + "}"
}

var (
	lcAgent    = new(true)
	lcTerminal = new(false)
)

// lcStubGit stands in for the process table: the git command running the hook.
func lcStubGit(t *testing.T, cmd string, known bool) {
	t.Helper()
	prev := lcHookGit
	lcHookGit = func() (string, bool) { return cmd, known }
	t.Cleanup(func() { lcHookGit = prev })
}

// lcRun parses and runs argv against the fixture with stdin, returning the
// handler's error (dispatch would print it and pick the exit code) and stderr.
func lcRun(t *testing.T, fx *daemonFixture, stdin string, argv ...string) (string, error) {
	t.Helper()
	e, _, errb := fx.env()
	e.stdin = strings.NewReader(stdin)
	p, err := parse(commands(), argv)
	require.NoError(t, err)
	err = p.cmd.run(context.Background(), e, p.opts, p.pos)
	return errb.String(), err
}

// lcRefusedCode is the exit code an error carries for dispatch, 1 for none.
func lcRefusedCode(err error) int {
	var coded lcExitCoder
	if errors.As(err, &coded) {
		return coded.seamExitCode()
	}
	if err != nil {
		return 1
	}
	return 0
}

// lcCommitMain is a reference-transaction's stdin for a commit on main.
const lcCommitMain = lcOidA + " " + lcOidB + " refs/heads/main\n"

func lcRefTx(repo string) []string {
	return []string{"lock", "check", "--hook", "reference-transaction", "--cwd", repo, "--", "prepared"}
}

// The rule, row by row: who may move a guarded ref, for a lock held by another
// session, held by the caller, and free, as an agent session and as a caller
// the daemon binds to no session (the owner at a terminal).
func TestLockCheck_TheRule(t *testing.T) {
	lcStubGit(t, "commit", true)
	repo := lcFakeRepo(t, lcOrigin)
	for _, tc := range []struct {
		name    string
		bound   *bool
		lock    lcLockOpts
		refused bool
		want    []string // in the refusal
	}{
		{name: "held by another, agent", bound: lcAgent, refused: true,
			lock: lcLockOpts{held: true, holder: "cc/lc-holder"},
			want: []string{"LOCK example-app main", "refs/heads/main", "session cc/lc-holder holds the lock",
				"since 2026-11-15T18:00:00Z", "until 2026-11-15T18:15:00Z", "seam lock acquire --wait",
				"--no-verify does not skip", "seam lock release --force"}},
		{name: "held by another, terminal", bound: lcTerminal, refused: true,
			lock: lcLockOpts{held: true, holder: "cc/lc-holder"},
			want: []string{"session cc/lc-holder holds the lock", "since 2026-11-15T18:00:00Z"}},
		{name: "held by a session this caller may not see", bound: lcAgent, refused: true,
			lock: lcLockOpts{held: true}, want: []string{"another session holds the lock", "until 2026-11-15T18:15:00Z"}},
		{name: "held by the caller", bound: lcAgent, lock: lcLockOpts{held: true, mine: true, holder: "cc/lc-me"}},
		{name: "free, agent", bound: lcAgent, refused: true, lock: lcLockOpts{},
			want: []string{"the lock is free", "this session does not hold it", "Take it first: seam lock acquire"}},
		{name: "lapsed, agent", bound: lcAgent, refused: true, lock: lcLockOpts{lapsed: true, holder: "cc/lc-me"},
			want: []string{"its last hold lapsed at 2026-11-15T18:15:00Z"}},
		{name: "free, terminal", bound: lcTerminal, lock: lcLockOpts{}},
		{name: "free, a server that names no caller_bound", bound: nil, lock: lcLockOpts{}},
		{name: "held by another, a server without held_by_caller", bound: lcAgent, refused: true,
			lock: lcLockOpts{held: true, holder: "cc/lc-holder", omitHeldByMe: true}},
		{name: "a declaration waiting for the owner guards nothing", bound: lcAgent,
			lock: lcLockOpts{state: "requested"}},
		{name: "another ref's lock", bound: lcAgent, lock: lcLockOpts{held: true, refs: []string{"refs/heads/release"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newDaemonFixture(t, map[string]toolFunc{
				"locks_list": fixedReply(lcAnswer(tc.bound, lcLock(tc.lock))),
			}, nil, "")
			stderr, err := lcRun(t, fx, lcCommitMain, lcRefTx(repo)...)
			require.Empty(t, stderr)
			if !tc.refused {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Equal(t, lcExitRefused, lcRefusedCode(err))
			for _, w := range tc.want {
				require.Contains(t, err.Error(), w)
			}
			args := fx.lastCall(t).args
			require.Equal(t, lcOrigin, args["repo_origin"], "the lock is found by the checkout's origin")
			require.Equal(t, repo, args["repo_root"])
		})
	}
}

// The unit tests run as a caller with no agent markers, whatever runs them: CI
// sets none, and an agent running make check carries its own (lcAgentMarker).
// A row that needs one sets it with lcStubEnv.
func init() { lcGetenv = func(string) string { return "" } }

// lcStubEnv stands in for the caller's environment: kv is name, value pairs.
func lcStubEnv(t *testing.T, kv ...string) {
	t.Helper()
	require.Zero(t, len(kv)%2, "name, value pairs")
	env := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		env[kv[i]] = kv[i+1]
	}
	prev := lcGetenv
	lcGetenv = func(name string) string { return env[name] }
	t.Cleanup(func() { lcGetenv = prev })
}

// lcStubChain stands in for the process table: the processes the check may
// ask as, nearest first (agentproc.Chain).
func lcStubChain(t *testing.T, chain ...string) {
	t.Helper()
	prev := lcAgentChain
	lcAgentChain = func() []string { return chain }
	t.Cleanup(func() { lcAgentChain = prev })
}

// An agent that runs git through a program of its own (fanout-land's node,
// make) is not the nearest process above git that is not a shell: the wrapper
// is, and it binds no session. The check asks again as each process further
// up and keeps the first answer that binds (lcList), so the agent's own hold
// lets it through and a lock it does not hold refuses it, as when it runs git
// itself. A chain that binds nowhere keeps the terminal rule.
func TestLockCheck_AnAgentBehindAWrapper(t *testing.T) {
	lcStubGit(t, "update-ref", true)
	repo := lcFakeRepo(t, lcOrigin)
	const wrapper, agent, tmux = "270.27", "200.20", "50.5"
	const login, terminal = "60.6", "55.5"

	// A stand-in daemon that binds agent alone, the process a session-start
	// hook stamped, and answers the lock as the daemon would for each asker.
	type holder int
	const (
		free holder = iota
		heldByAgent
		heldByAnother
	)
	world := func(t *testing.T, h holder, older bool) (*daemonFixture, *[]string) {
		var fx *daemonFixture
		asked := []string{}
		fx = newDaemonFixture(t, map[string]toolFunc{
			"locks_list": func(map[string]any) toolReply {
				proc := fx.lastHeader("locks_list").Get(agentProcessHeader)
				asked = append(asked, proc)
				bound := proc == agent
				var lock lcLockOpts
				switch h {
				case heldByAgent:
					lock = lcLockOpts{held: true, mine: bound, holder: "cc/lc-agent"}
				case heldByAnother:
					lock = lcLockOpts{held: true, holder: "cc/lc-holder"}
				}
				if older {
					return toolReply{text: lcAnswer(nil, lcLock(lock))}
				}
				return toolReply{text: lcAnswer(&bound, lcLock(lock))}
			},
		}, nil, "")
		return fx, &asked
	}

	for _, tc := range []struct {
		name    string
		chain   []string
		holder  holder
		older   bool
		refused string // in the refusal; "" passes
		asked   []string
	}{
		{name: "the agent holds the lock", chain: []string{wrapper, agent, tmux}, holder: heldByAgent,
			asked: []string{wrapper, agent}},
		{name: "the lock is free", chain: []string{wrapper, agent, tmux}, holder: free,
			refused: "this session does not hold it", asked: []string{wrapper, agent}},
		{name: "another session holds the lock", chain: []string{wrapper, agent, tmux}, holder: heldByAnother,
			refused: "session cc/lc-holder holds the lock", asked: []string{wrapper, agent}},
		{name: "the agent runs git itself", chain: []string{agent, tmux}, holder: heldByAgent,
			asked: []string{agent}},
		// The control: a check that stopped at the wrapper read the agent's own
		// hold as another session's.
		{name: "a chain that ends at the wrapper", chain: []string{wrapper}, holder: heldByAgent,
			refused: "session cc/lc-agent holds the lock", asked: []string{wrapper}},
		{name: "a terminal, the lock free", chain: []string{login, terminal}, holder: free,
			asked: []string{login, terminal}},
		{name: "a terminal, the lock held", chain: []string{login, terminal}, holder: heldByAnother,
			refused: "session cc/lc-holder holds the lock", asked: []string{login, terminal}},
		{name: "a server that names no caller_bound is asked once", chain: []string{wrapper, agent}, holder: free,
			older: true, asked: []string{wrapper}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lcStubChain(t, tc.chain...)
			fx, asked := world(t, tc.holder, tc.older)
			stderr, err := lcRun(t, fx, lcCommitMain, lcRefTx(repo)...)
			require.Empty(t, stderr)
			require.Equal(t, tc.asked, *asked, "who the check asked as, in order")
			if tc.refused == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Equal(t, lcExitRefused, lcRefusedCode(err))
			require.Contains(t, err.Error(), tc.refused)
		})
	}

	t.Run("no chain asks once as the connection", func(t *testing.T) {
		lcStubChain(t)
		fx, asked := world(t, free, false)
		_, err := lcRun(t, fx, lcCommitMain, lcRefTx(repo)...)
		require.NoError(t, err, "nothing binds, so the terminal rule lets a free lock through")
		require.Len(t, *asked, 1)
	})

	t.Run("a failure partway warns and allows", func(t *testing.T) {
		lcStubChain(t, wrapper, agent)
		var fx *daemonFixture
		fx = newDaemonFixture(t, map[string]toolFunc{
			"locks_list": func(map[string]any) toolReply {
				if fx.lastHeader("locks_list").Get(agentProcessHeader) == agent {
					return toolReply{text: "the store is busy", isError: true}
				}
				return toolReply{text: lcAnswer(lcTerminal, lcLock(lcLockOpts{held: true, holder: "cc/lc-agent"}))}
			},
		}, nil, "")
		stderr, err := lcRun(t, fx, lcCommitMain, lcRefTx(repo)...)
		require.NoError(t, err)
		require.Contains(t, stderr, "locks_list failed (the store is busy)")
	})
}

// An agent the daemon binds to no session -- its session ended, or no
// session-start hook ever stamped it -- reads to the daemon as the owner at a
// terminal does (caller_bound false). Its environment says it is an agent, so
// it gets the agent's rule, and its refusal says to resume or start a session
// before taking the lock. The owner's shape, no marker, keeps the terminal
// rule; so does a marker that outlived its agent, and a server too old to say
// caller_bound.
func TestLockCheck_AnAgentWithNoSession(t *testing.T) {
	lcStubGit(t, "commit", true)
	repo := lcFakeRepo(t, lcOrigin)
	const agentPid = "4242"
	chain := []string{"270.27", agentPid + ".42", "50.5"}
	sessionRemedy := []string{"no live Seamless session is bound to", "Resume your session, or start one: session_start",
		"name=<the cc/... or cx/... name your briefing printed>", "Then take the lock: seam lock acquire",
		"--no-verify does not skip", "seam lock release --force"}
	for _, tc := range []struct {
		name    string
		env     []string
		chain   []string
		bound   *bool
		lock    lcLockOpts
		refused []string // in the refusal; nil passes
		absent  []string // not in the refusal
	}{
		{name: "Claude Code, the lock free", env: []string{"CLAUDECODE", "1"}, bound: lcTerminal,
			refused: append([]string{"LOCK example-app main", "the lock is free", "an agent (CLAUDECODE is set)"}, sessionRemedy...),
			absent:  []string{"this session does not hold it"}},
		{name: "Claude Code, its own process named and an ancestor", env: []string{"CLAUDECODE", "1", "CLAUDE_PID", agentPid},
			chain: chain, bound: lcTerminal, refused: []string{"CLAUDECODE is set"}},
		{name: "Claude Code, its process named but no process table to read", env: []string{"CLAUDECODE", "1", "CLAUDE_PID", "999"},
			bound: lcTerminal, refused: []string{"CLAUDECODE is set"}},
		{name: "Codex", env: []string{"CODEX_THREAD_ID", "019a0000-0000-7000-8000-000000000000"}, bound: lcTerminal,
			refused: append([]string{"CODEX_THREAD_ID is set"}, sessionRemedy...)},
		{name: "Codex in its sandbox", env: []string{"CODEX_SANDBOX", "seatbelt"}, bound: lcTerminal,
			refused: []string{"CODEX_SANDBOX is set"}},
		{name: "a lapsed hold", env: []string{"CLAUDECODE", "1"}, bound: lcTerminal,
			lock:    lcLockOpts{lapsed: true, holder: "cc/lc-me"},
			refused: []string{"its last hold lapsed at 2026-11-15T18:15:00Z", "CLAUDECODE is set"}},
		{name: "held by another session", env: []string{"CLAUDECODE", "1"}, bound: lcTerminal,
			lock: lcLockOpts{held: true, holder: "cc/lc-holder"},
			refused: append([]string{"session cc/lc-holder holds the lock (since 2026-11-15T18:00:00Z, until 2026-11-15T18:15:00Z)",
				"CLAUDECODE is set"}, sessionRemedy...)},

		// The owner's shape: no marker, the terminal rule.
		{name: "no marker, the lock free", bound: lcTerminal},
		{name: "no marker, the lock held", bound: lcTerminal, lock: lcLockOpts{held: true, holder: "cc/lc-holder"},
			refused: []string{"session cc/lc-holder holds the lock", "Wait for its release"},
			absent:  []string{"is set", "session_start"}},
		// A marker that outlived its agent (a tmux server an agent started):
		// the process CLAUDE_PID names is not above this one.
		{name: "Claude Code's marker, its process not an ancestor", env: []string{"CLAUDECODE", "1", "CLAUDE_PID", "777"},
			chain: chain, bound: lcTerminal},
		{name: "a marker under a server that names no caller_bound", env: []string{"CLAUDECODE", "1"}, bound: nil},
		{name: "a bound agent's own hold", env: []string{"CLAUDECODE", "1"}, bound: lcAgent,
			lock: lcLockOpts{held: true, mine: true, holder: "cc/lc-me"}},
		{name: "a bound agent, the lock free", env: []string{"CLAUDECODE", "1"}, bound: lcAgent,
			refused: []string{"this session does not hold it"}, absent: []string{"is set", "session_start"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lcStubEnv(t, tc.env...)
			lcStubChain(t, tc.chain...)
			fx := newDaemonFixture(t, map[string]toolFunc{
				"locks_list": fixedReply(lcAnswer(tc.bound, lcLock(tc.lock))),
			}, nil, "")
			stderr, err := lcRun(t, fx, lcCommitMain, lcRefTx(repo)...)
			require.Empty(t, stderr)
			if tc.refused == nil {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Equal(t, lcExitRefused, lcRefusedCode(err))
			for _, w := range tc.refused {
				require.Contains(t, err.Error(), w)
			}
			for _, w := range tc.absent {
				require.NotContains(t, err.Error(), w)
			}
		})
	}
}

// lcAgentMarker on its own: which variable counts, and when CLAUDE_PID vetoes
// CLAUDECODE.
func TestLockCheck_AgentMarker(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(name string) string { return m[name] }
	}
	chain := []string{"270.27", "4242.42"}
	for _, tc := range []struct {
		name  string
		env   func(string) string
		chain []string
		want  string
	}{
		{name: "nothing", env: env(), chain: chain},
		{name: "CLAUDECODE alone", env: env("CLAUDECODE", "1"), chain: chain, want: "CLAUDECODE"},
		{name: "CLAUDE_PID an ancestor", env: env("CLAUDECODE", "1", "CLAUDE_PID", "4242"), chain: chain, want: "CLAUDECODE"},
		{name: "CLAUDE_PID a prefix of an ancestor's pid is not one", env: env("CLAUDECODE", "1", "CLAUDE_PID", "424"), chain: chain},
		{name: "CLAUDE_PID not an ancestor", env: env("CLAUDECODE", "1", "CLAUDE_PID", "777"), chain: chain},
		{name: "CLAUDE_PID with no chain", env: env("CLAUDECODE", "1", "CLAUDE_PID", "777"), want: "CLAUDECODE"},
		{name: "CLAUDE_PID without CLAUDECODE", env: env("CLAUDE_PID", "4242"), chain: chain},
		{name: "a leaked CLAUDECODE beside Codex's own", env: env("CLAUDECODE", "1", "CLAUDE_PID", "777", "CODEX_THREAD_ID", "t"),
			chain: chain, want: "CODEX_THREAD_ID"},
		{name: "CODEX_SANDBOX", env: env("CODEX_SANDBOX", "seatbelt"), chain: chain, want: "CODEX_SANDBOX"},
		{name: "another CODEX_ variable is not a marker", env: env("CODEX_HOME", "/x"), chain: chain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, lcAgentMarker(tc.env, tc.chain))
		})
	}
}

// The negative control the acceptance asks for: a refusal that says only
// "refused" is not enough. Every refusal names the lock, the holder, since and
// until, and the remedy, and an assertion on those words fails on a bare one.
func TestLockCheck_ARefusalNamesTheHolder(t *testing.T) {
	bare := &lcRefusedError{msg: "refused"}
	for _, w := range []string{"LOCK example-app main", "cc/lc-holder", "since ", "until ", "seam lock acquire --wait"} {
		require.NotContains(t, bare.Error(), w, "the control: a bare refusal names nothing")
	}
	got := lcRefusal(lcHookRefTx, "", "", map[string]any{
		"label": "example-app main", "held": true, "holder_session": "cc/lc-holder",
		"since": "2026-11-15T18:00:00Z", "until": "2026-11-15T18:15:00Z",
	}, []string{"refs/heads/main"})
	require.Equal(t, "refused: this update moves refs/heads/main, which LOCK example-app main guards, and session "+
		"cc/lc-holder holds the lock (since 2026-11-15T18:00:00Z, until 2026-11-15T18:15:00Z).\n"+
		hintIndent+"Wait for its release, or queue for the lock: seam lock acquire --wait", got)

	push := lcRefusal(lcHookPrePush, "origin", "", map[string]any{"label": "example-app main"}, []string{"refs/tags/v1.2"})
	require.Contains(t, push, "this push moves refs/tags/v1.2 on origin, which LOCK example-app main guards")
}

// A transaction that touches no ref a lock could guard exits before the
// daemon is dialed: a commit on a detached HEAD, a fetch's remote-tracking
// refs, the stash, git's working refs, and the states other than prepared.
func TestLockCheck_TheFastPathDialsNothing(t *testing.T) {
	lcStubGit(t, "commit", true)
	repo := lcFakeRepo(t, lcOrigin)
	e, _, errb := stubEnv()
	dialed := 0
	e.dial = func(context.Context) (*mcpclient.Client, config.Config, error) {
		dialed++
		return nil, config.Config{}, errors.New("the fixture refuses every dial")
	}
	run := func(stdin string, argv ...string) error {
		e.stdin = strings.NewReader(stdin)
		p, err := parse(commands(), argv)
		require.NoError(t, err)
		return p.cmd.run(context.Background(), e, p.opts, p.pos)
	}
	for name, stdin := range map[string]string{
		"a detached-HEAD commit":   lcOidA + " " + lcOidB + " HEAD\n",
		"remote-tracking refs":     lcOidA + " " + lcOidB + " refs/remotes/origin/main\n",
		"the stash":                lcOidZero + " " + lcOidB + " refs/stash\n",
		"git's working refs":       lcOidZero + " " + lcOidB + " refs/bisect/bad\n" + lcOidZero + " " + lcOidB + " refs/rewritten/onto\n",
		"a worktree's own HEAD":    lcOidA + " " + lcOidB + " worktrees/wt/HEAD\n",
		"ORIG_HEAD and AUTO_MERGE": lcOidZero + " " + lcOidB + " ORIG_HEAD\n" + lcOidZero + " " + lcOidZero + " AUTO_MERGE\n",
		"main to where it is":      lcOidZero + " " + lcOidA + " refs/heads/main\n",
		"nothing at all":           "",
	} {
		require.NoError(t, run(stdin, lcRefTx(repo)...), name)
	}
	for _, state := range []string{"preparing", "committed", "aborted"} {
		require.NoError(t, run(lcCommitMain, "lock", "check", "--hook", "reference-transaction", "--cwd", repo, "--", state), state)
	}
	require.NoError(t, run("refs/heads/main "+lcOidA+" refs/heads/main "+lcOidA+"\n", "lock", "check", "--hook", "pre-push", "--cwd", repo),
		"a push that leaves the remote ref where it is")
	require.Empty(t, errb.String())
	require.Zero(t, dialed)

	// The control: a commit on main does reach the dial.
	require.NoError(t, run(lcCommitMain, lcRefTx(repo)...))
	require.Equal(t, 1, dialed, "a commit on main dials the daemon")
}

func TestLockCheck_Candidates(t *testing.T) {
	for ref, want := range map[string]bool{
		"refs/heads/main": true, "refs/heads/feature/x": true, "refs/tags/v1.2": true, "refs/notes/commits": true,
		"HEAD": false, "ORIG_HEAD": false, "FETCH_HEAD": false, "AUTO_MERGE": false, "main-worktree/HEAD": false,
		"refs/remotes/origin/main": false, "refs/stash": false, "refs/bisect/bad": false, "refs/rewritten/onto": false,
		"refs/worktree/x": false, "refs/prefetch/remotes/origin/main": false, "refs/stashes": true,
	} {
		require.Equal(t, want, lcCandidate(ref), ref)
	}
}

func TestLockCheck_Guards(t *testing.T) {
	for _, tc := range []struct {
		pattern, ref string
		want         bool
	}{
		{"refs/heads/main", "refs/heads/main", true},
		{"refs/heads/main", "refs/heads/main2", false},
		{"refs/tags/v*", "refs/tags/v1.2.3", true},
		{"refs/tags/v*", "refs/tags/v", true},
		{"refs/tags/v*", "refs/tags/release-1", false},
		{"refs/heads/*", "refs/heads/team/topic", true},
		{"refs/heads/release-*-final", "refs/heads/release-2-final", true},
		{"refs/heads/release-*-final", "refs/heads/release-final", false},
		{"refs/tags/*", "refs/heads/main", false},
	} {
		require.Equal(t, tc.want, lcGuards(tc.pattern, tc.ref), "%s ~ %s", tc.pattern, tc.ref)
	}
}

// A fetch brings the remote's refs here and lands nothing: its tags pass with
// no daemon call. A fetch that moves a local branch by an explicit refspec is
// still checked. clone, pack-refs, gc and maintenance are never checked.
func TestLockCheck_ExemptCommands(t *testing.T) {
	repo := lcFakeRepo(t, lcOrigin)
	tagAndRemote := lcOidZero + " " + lcOidB + " refs/tags/v1.2\n" + lcOidA + " " + lcOidB + " refs/remotes/origin/main\n"
	fx := newDaemonFixture(t, map[string]toolFunc{
		"locks_list": fixedReply(lcAnswer(lcAgent, lcLock(lcLockOpts{held: true, holder: "cc/lc-holder",
			refs: []string{"refs/heads/main", "refs/tags/v*"}}))),
	}, nil, "")
	for _, tc := range []struct {
		name    string
		cmd     string
		known   bool
		stdin   string
		refused bool
	}{
		{name: "a fetch's new tag", cmd: "fetch", known: true, stdin: tagAndRemote},
		{name: "a fetch's tag alone", cmd: "fetch", known: true, stdin: lcOidZero + " " + lcOidB + " refs/tags/v1.2\n"},
		{name: "fetch origin main:main", cmd: "fetch", known: true, stdin: lcCommitMain, refused: true},
		{name: "a tag made here", cmd: "tag", known: true, stdin: lcOidZero + " " + lcOidB + " refs/tags/v1.2\n", refused: true},
		{name: "clone", cmd: "clone", known: true, stdin: lcCommitMain},
		{name: "pack-refs", cmd: "pack-refs", known: true, stdin: lcOidB + " " + lcOidZero + " refs/heads/main\n"},
		{name: "gc", cmd: "gc", known: true, stdin: lcOidB + " " + lcOidZero + " refs/heads/main\n"},
		{name: "maintenance", cmd: "maintenance", known: true, stdin: lcOidB + " " + lcOidZero + " refs/heads/main\n"},
		{name: "unreadable: remote-tracking refs mark a fetch", known: false, stdin: tagAndRemote},
		{name: "unreadable: a tag alone is checked", known: false, stdin: lcOidZero + " " + lcOidB + " refs/tags/v1.2\n", refused: true},
		{name: "unreadable: main is checked beside remote refs", known: false,
			stdin: lcCommitMain + lcOidA + " " + lcOidB + " refs/remotes/origin/main\n", refused: true},
		{name: "a check run by hand", cmd: "", known: true, stdin: tagAndRemote, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lcStubGit(t, tc.cmd, tc.known)
			before := fx.callCount()
			_, err := lcRun(t, fx, tc.stdin, lcRefTx(repo)...)
			if tc.refused {
				require.Equal(t, lcExitRefused, lcRefusedCode(err))
				return
			}
			require.NoError(t, err)
			require.Equal(t, before, fx.callCount(), "an exempt transaction asks the daemon nothing")
		})
	}
}

func TestLockCheck_GitSubcommand(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"git", "commit", "-m", "fetch the thing"}, "commit"},
		{[]string{"/usr/bin/git", "fetch", "-q", "origin"}, "fetch"},
		{[]string{"git", "-C", "/work/repo", "-c", "user.name=x", "--no-pager", "fetch"}, "fetch"},
		{[]string{"git", "--git-dir=/x/.git", "--work-tree", "/x", "pack-refs", "--all"}, "pack-refs"},
		{[]string{"/usr/libexec/git-core/git", "merge", "--ff-only", "FETCH_HEAD"}, "merge"},
		{[]string{"/usr/libexec/git-core/git-receive-pack", "/srv/repo.git"}, "receive-pack"},
		{[]string{`C:\Program Files\Git\cmd\git.exe`, "gc", "--auto"}, "gc"},
		{[]string{"git"}, ""},
		{nil, ""},
	} {
		require.Equal(t, tc.want, lcSubcommand(tc.argv), "%q", tc.argv)
	}
}

// A push is checked by the ref it moves on the remote, from the remote's
// value to the local one; a deletion moves it too.
func TestLockCheck_PrePush(t *testing.T) {
	lcStubGit(t, "", true) // pre-push is never exempt: the git command is not asked
	repo := lcFakeRepo(t, lcOrigin)
	fx := newDaemonFixture(t, map[string]toolFunc{
		"locks_list": fixedReply(lcAnswer(lcAgent, lcLock(lcLockOpts{held: true, holder: "cc/lc-holder",
			refs: []string{"refs/heads/main", "refs/tags/v*"}}))),
	}, nil, "")
	push := func(stdin string) error {
		_, err := lcRun(t, fx, stdin, "lock", "check", "--hook", "pre-push", "--cwd", repo, "--", "origin", "git@github.com:example/example-app.git")
		return err
	}
	err := push("refs/heads/topic " + lcOidB + " refs/heads/main " + lcOidA + "\n")
	require.Equal(t, lcExitRefused, lcRefusedCode(err))
	require.Contains(t, err.Error(), "this push moves refs/heads/main on origin")
	require.Equal(t, lcExitRefused, lcRefusedCode(push("(delete) "+lcOidZero+" refs/tags/v1.2 "+lcOidA+"\n")))
	require.NoError(t, push("refs/heads/topic "+lcOidB+" refs/heads/topic "+lcOidA+"\n"), "an unguarded branch")
	require.NoError(t, push("refs/remotes/x "+lcOidB+" refs/remotes/x "+lcOidA+"\n"))
}

// Everything that is not a refusal lets the update through: the daemon down
// warns; a server with repo locks off, and a repository with no lock, say
// nothing; a checkout that is no repository warns.
func TestLockCheck_FailOpen(t *testing.T) {
	lcStubGit(t, "commit", true)
	repo := lcFakeRepo(t, lcOrigin)

	t.Run("the daemon down", func(t *testing.T) {
		fx := newDaemonFixture(t, map[string]toolFunc{}, nil, "")
		fx.srv.Close()
		stderr, err := lcRun(t, fx, lcCommitMain, lcRefTx(repo)...)
		require.NoError(t, err)
		require.Contains(t, stderr, "seam lock check: warning: seamlessd is unreachable")
		require.Contains(t, stderr, "the update goes ahead")
	})
	t.Run("repo locks off on the server", func(t *testing.T) {
		fx := newDaemonFixture(t, map[string]toolFunc{"tasks_list": fixedReply(`{"tasks":[]}`)}, nil, "")
		stderr, err := lcRun(t, fx, lcCommitMain, lcRefTx(repo)...)
		require.NoError(t, err)
		require.Empty(t, stderr, "the feature off is silent")
	})
	t.Run("no lock declared", func(t *testing.T) {
		fx := newDaemonFixture(t, map[string]toolFunc{"locks_list": fixedReply(lcAnswer(lcAgent))}, nil, "")
		stderr, err := lcRun(t, fx, lcCommitMain, lcRefTx(repo)...)
		require.NoError(t, err)
		require.Empty(t, stderr)
	})
	t.Run("locks_list refused", func(t *testing.T) {
		fx := newDaemonFixture(t, map[string]toolFunc{"locks_list": func(map[string]any) toolReply {
			return toolReply{text: "locks_list: boom", isError: true}
		}}, nil, "")
		stderr, err := lcRun(t, fx, lcCommitMain, lcRefTx(repo)...)
		require.NoError(t, err)
		require.Contains(t, stderr, "locks_list failed (locks_list: boom)")
	})
	t.Run("outside a checkout", func(t *testing.T) {
		fx := newDaemonFixture(t, map[string]toolFunc{"locks_list": fixedReply(lcAnswer(lcAgent))}, nil, "")
		stderr, err := lcRun(t, fx, lcCommitMain, "lock", "check", "--hook", "reference-transaction", "--cwd", t.TempDir())
		require.NoError(t, err)
		require.Contains(t, stderr, "is not inside a git repository")
		require.Zero(t, fx.callCount())
	})
}

func TestLockCheck_Parse(t *testing.T) {
	require.Equal(t, "seam lock check [--cwd DIR] [--hook HOOK] [<hook-arg>] [<hook-arg>]", synopsis(lockCheckCmd))
	e, _, errb := stubEnv()
	require.Equal(t, 2, dispatch(context.Background(), e, []string{"lock", "check", "--hook", "post-commit"}))
	require.Contains(t, errb.String(), "valid values are reference-transaction, pre-push")

	e, _, errb = stubEnv()
	require.Equal(t, 1, dispatch(context.Background(), e, []string{"lock", "check"}))
	require.Contains(t, errb.String(), "pass --hook reference-transaction or --hook pre-push")

	e, _, errb = stubEnv()
	require.Equal(t, 2, dispatch(context.Background(), e, []string{"lock"}))
	require.Contains(t, errb.String(), "lock release, lock check")
}

// dispatch exits with the refusal's own code, and only a refusal's: the hook
// scripts refuse on 77 alone.
func TestLockCheck_DispatchExitsWithTheRefusalCode(t *testing.T) {
	lcStubGit(t, "commit", true)
	repo := lcFakeRepo(t, lcOrigin)
	fx := newDaemonFixture(t, map[string]toolFunc{
		"locks_list": fixedReply(lcAnswer(lcAgent, lcLock(lcLockOpts{held: true, holder: "cc/lc-holder"}))),
	}, nil, "")
	e, _, errb := fx.env()
	e.stdin = strings.NewReader(lcCommitMain)
	require.Equal(t, lcExitRefused, dispatch(context.Background(), e, lcRefTx(repo)))
	require.Contains(t, errb.String(), "error: refused: this update moves refs/heads/main")

	// Any other handler error keeps exit 1.
	e, _, _ = stubEnv()
	require.Equal(t, 1, dispatch(context.Background(), e, []string{"lock", "check"}))
}

// seam doctor's repo_lock_hooks row: none without a declared lock, ok when the
// checkout runs the hooks, "hooks off" with the remedy when it does not.
func TestLockCheck_DoctorRow(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "none"))
	repo := lcFakeRepo(t, lcOrigin)
	ctx := context.Background()
	row := func(answer string, dir string) (string, string) {
		fx := newDaemonFixture(t, map[string]toolFunc{"locks_list": fixedReply(answer)}, nil, "")
		cli, _, err := dialWith(ctx, fx.config())
		require.NoError(t, err)
		defer func() { _ = cli.Close() }()
		return lockHooksCheck(ctx, cli, dir)
	}
	declared := lcAnswer(lcTerminal, lcLock(lcLockOpts{}))

	label, detail := row(declared, repo)
	require.Equal(t, "note", label)
	require.Contains(t, detail, "hooks off: LOCK example-app main is declared for this repository")
	require.Contains(t, detail, "#commit-time-lock-checks", "the remedy")

	hook := filepath.Join(repo, ".git", "hooks", "reference-transaction")
	require.NoError(t, os.MkdirAll(filepath.Dir(hook), 0o755))
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nseam lock check --hook reference-transaction -- \"$@\"\n"), 0o755))
	label, detail = row(declared, repo)
	require.Equal(t, "ok", label)
	require.Contains(t, detail, "LOCK example-app main: checked at commit and push time")

	for name, answer := range map[string]string{
		"no lock":                lcAnswer(lcTerminal),
		"only a waiting request": lcAnswer(lcTerminal, lcLock(lcLockOpts{state: "requested"})),
	} {
		label, _ = row(answer, repo)
		require.Empty(t, label, name)
	}
	label, _ = row(declared, t.TempDir())
	require.Empty(t, label, "outside a checkout")
}

// The whole doctor run prints the row only while the server serves the lock
// tools.
func TestLockCheck_DoctorRowNeedsTheFeature(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "none"))
	repo := lcFakeRepo(t, lcOrigin)
	t.Chdir(repo)
	run := func(tools map[string]toolFunc) string {
		tools["project_list"] = fixedReply(`{"projects":[]}`)
		fx := newDaemonFixture(t, tools, map[string]string{
			"GET /console/settings?format=json": `{"featuresConfig":{"research":true,"repo_locks":true}}`,
		}, `{"status":"ok"}`)
		e, out, _ := fx.env()
		_ = dispatch(context.Background(), e, []string{"doctor"})
		return out.String()
	}
	out := run(map[string]toolFunc{"locks_list": fixedReply(lcAnswer(lcTerminal, lcLock(lcLockOpts{})))})
	require.Contains(t, out, "  [note] repo_lock_hooks: hooks off: LOCK example-app main")
	out = run(map[string]toolFunc{})
	require.NotContains(t, out, "repo_lock_hooks", "repo locks off: no row")
}

// The guide's reference-transaction script is the committed one, comments
// aside, so the hook lines a repository copies are the ones these tests run.
func TestLockCheck_TheGuideShowsTheCommittedHook(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs-src", "guides", "coordinate-agents.md"))
	require.NoError(t, err)
	section := string(guide)[strings.Index(string(guide), "### Add the hooks to a repository"):]
	start := strings.Index(section, "```sh\n") + len("```sh\n")
	shown := section[start : start+strings.Index(section[start:], "```")]

	script, err := os.ReadFile(filepath.Join("..", "..", ".githooks", "reference-transaction"))
	require.NoError(t, err)
	var kept []string
	for i, line := range strings.Split(string(script), "\n") {
		if i > 0 && strings.HasPrefix(line, "#") {
			continue
		}
		if line == "" && len(kept) > 0 && kept[len(kept)-1] == "" {
			continue
		}
		kept = append(kept, line)
	}
	require.Equal(t, strings.TrimSpace(strings.Join(kept, "\n")), strings.TrimSpace(shown))
}
