package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/hooks"
)

// The whole reason hook is exempt from the usage exit code: Claude Code reads
// exit 2 from a hook as a BLOCKING error and feeds stderr back to the model, so
// a typo'd hook config would wedge the very session the hook exists to serve.
// Every way of getting the command line wrong must still fail open at 1.
//
// stubEnv's dial and loadConfig are nil, so any case reaching the network would
// panic rather than pass.
func TestDispatch_HookNeverExitsTwo(t *testing.T) {
	for _, tt := range []struct {
		name string
		argv []string
		want string
	}{
		{"no event", []string{"hook"}, "missing hook event"},
		{"unknown event", []string{"hook", "bogus"}, `unknown hook event "bogus"`},
		{"invalid client", []string{"hook", "session-start", "--client", "codxe"}, `valid values are claude-code, codex`},
		{"unknown flag", []string{"hook", "--bogus"}, "not defined"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, _, errb := stubEnv()
			require.Equal(t, 1, dispatch(context.Background(), e, tt.argv),
				"hook must fail open at 1: exit 2 blocks the session it serves")
			require.Contains(t, errb.String(), tt.want)
		})
	}
}

// Both halves of the exemption, at the layer that decides each. usageExit covers
// parse failures; hook's loose arity keeps event validation in runHook while the
// canonical client enum rejects a bad discriminator during parse.
func TestUsageExit_HookIsTheOnlyExemption(t *testing.T) {
	for _, c := range commands() {
		want := 2
		if c.name == "hook" {
			want = 1
		}
		require.Equal(t, want, c.usageExit(), "%s", c.name)
	}

	// A bad event reaches runHook (which fails open) rather than the parse layer.
	p, err := parse(commands(), []string{"hook", "bogus"})
	require.NoError(t, err, "hook must not enforce its event name at parse time")
	require.Equal(t, []string{"bogus"}, p.pos)

	p, err = parse(commands(), []string{"hook"})
	require.NoError(t, err, "hook must not enforce arity at parse time")
	require.Empty(t, p.pos)

	_, err = parse(commands(), []string{"hook", "session-start", "--client", "codxe"})
	require.EqualError(t, err,
		`invalid value "codxe" for flag -client: valid values are claude-code, codex`)
}

// A typo is an install/configuration error, not a recoverable daemon outage. It
// fails before stdin, config, or network work, names the canonical set, and uses
// exit 1 because hook exit 2 is blocking in supported agent clients.
func TestDispatch_HookInvalidClientExitContract(t *testing.T) {
	e, _, errb := stubEnv()
	code := dispatch(context.Background(), e,
		[]string{"hook", "session-start", "--client", "codxe"})
	require.Equal(t, 1, code)
	lines := strings.Split(strings.TrimSpace(errb.String()), "\n")
	require.Equal(t,
		`error: invalid value "codxe" for flag -client: valid values are claude-code, codex`,
		lines[0])
	require.Len(t, lines, 2)
	require.Equal(t, "usage: "+synopsis(hookCmd), lines[1])
}

// The error names the valid set rather than the old "(want a|b|c)" blob, and
// derives it from hookEvents so it cannot drift from what forwards.
func TestRunHook_ErrorsNameTheValidEvents(t *testing.T) {
	e, _, _ := stubEnv()
	err := runHook(context.Background(), e, &hookOpts{}, []string{"bogus"})
	require.ErrorContains(t, err, "valid values are session-start, user-prompt-submit")
	require.NotContains(t, err.Error(), "want ")
}

// captureHookServer stands in for seamlessd: it records the path+query and
// bearer of the one request seam hook forwards, and returns an empty 200 so the
// hook succeeds. loadConfig points seam hook at it via cfg.Addr.
func captureHookServer(t *testing.T, payload string) (*env, **http.Request) {
	t.Helper()
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	e, _, _ := stubEnv()
	e.stdin = strings.NewReader(payload)
	e.loadConfig = func() (config.Config, error) {
		cfg := config.Defaults()
		cfg.Addr = strings.TrimPrefix(srv.URL, "http://")
		cfg.MCP.APIKey = "k"
		return cfg, nil
	}
	return e, &got // caller reads *got after runHook returns
}

// --client threads the discriminator to the daemon as ?client=<value> on the
// forwarded request, so the server can pick the right per-client payload adapter.
func TestRunHook_ClientFlagForwardsQueryParam(t *testing.T) {
	for _, client := range hookClients {
		t.Run(client, func(t *testing.T) {
			e, got := captureHookServer(t, `{"session_id":"019f7291-x","cwd":"/w","prompt":"hi"}`)
			require.NoError(t, runHook(context.Background(), e, &hookOpts{client: client}, []string{"user-prompt-submit"}))
			require.NotNil(t, *got, "seam hook must forward to the daemon")
			require.Equal(t, "/api/hooks/user-prompt-submit", (*got).URL.Path)
			require.Equal(t, client, (*got).URL.Query().Get("client"))
		})
	}
}

// The default (no --client) sends no client param, so the daemon still resolves
// the request to Claude Code exactly as it did before the discriminator existed.
// The query string is no longer empty -- the machine identity always rides on it
// (see identityParams) -- but the client half is absent, which is the half that
// selects a payload adapter.
func TestRunHook_NoClientFlagOmitsQueryParam(t *testing.T) {
	e, got := captureHookServer(t, `{"session_id":"abc","cwd":"/w"}`)
	require.NoError(t, runHook(context.Background(), e, &hookOpts{}, []string{"session-start"}))
	require.NotNil(t, *got, "seam hook must forward to the daemon")
	require.Equal(t, "/api/hooks/session-start", (*got).URL.Path)
	require.False(t, (*got).URL.Query().Has("client"),
		"no --client => no client param, so the daemon defaults to Claude Code")
}

// anyEventPayload is a hook payload every event forwards: post-tool-use's local
// filter passes an ExitPlanMode call, and the other events forward any body.
// Its cwd is outside any repository, so session-start's git reads find nothing.
const anyEventPayload = `{"session_id":"s","cwd":"/w","prompt":"hi","tool_name":"ExitPlanMode"}`

// hookReply is a whole reply as the daemon gives one, for the tests that cut it
// short.
const hookReply = `{"continue":true,"suppressOutput":true}`

// hookEnvAt is a stub env whose config points seam hook at addr, with
// anyEventPayload on stdin.
func hookEnvAt(addr string) (*env, *bytes.Buffer, *bytes.Buffer) {
	e, out, errb := stubEnv()
	e.stdin = strings.NewReader(anyEventPayload)
	e.loadConfig = func() (config.Config, error) {
		cfg := config.Defaults()
		cfg.Addr = addr
		cfg.MCP.APIKey = "k"
		return cfg, nil
	}
	return e, out, errb
}

// answerHooks answers any hook with a fixed reply, or hangs up after reading it
// once hangUpAfterRead is set, counting what it read.
func answerHooks(hits *atomic.Int32, hangUpAfterRead bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits.Add(1)
		if hangUpAfterRead {
			hangUp(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"continue":true}`))
	})
}

// A hook that fires while an update restarts the daemon lands when the daemon
// is back within its budget: refused while the port is closed, delivered once
// the new daemon listens, and read by it exactly once. A full restart (about 2s
// on macOS) outlasts the short budget, so a hook on it fails open instead: the
// accepted cost is one prompt's recall injection or one heartbeat. The client
// matters where its hook does different work: Claude Code's subagent-stop
// caches a planning subagent's report and rides the restart out, Codex's only
// heartbeats and does not.
func TestRunHook_RidesOutARestartWithinItsBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		event  string
		client string // --client; "" is Claude Code
		// backAt is the retry wait during which the daemon listens again. The
		// production backoff ends wait 2 at 300ms (a blip) and wait 5 at 2.5s
		// (the next attempt after a 2s restart).
		backAt int
		waits  int
		lands  bool
	}{
		{"blip, full budget", "session-start", "", 2, 2, true},
		{"blip, short budget", "user-prompt-submit", "", 2, 2, true},
		{"restart, full budget", "session-start", "", 5, 5, true},
		{"restart, short budget", "user-prompt-submit", "", 5, 4, false},
		{"restart, claude code subagent-stop", "subagent-stop", "", 5, 5, true},
		{"restart, codex subagent-stop", "subagent-stop", "codex", 5, 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := deadAddr(t)
			var hits atomic.Int32
			clk := &fakeClock{onWait: func(n int) {
				if n == tc.backAt {
					serveAt(t, addr, answerHooks(&hits, false))
				}
			}}
			e, out, errb := hookEnvAt(addr)
			o := &hookOpts{client: tc.client, retry: clk.policy}
			require.NoError(t, runHook(context.Background(), e, o, []string{tc.event}))
			require.Len(t, clk.recorded(), tc.waits)
			if !tc.lands {
				require.Empty(t, out.String())
				require.Contains(t, errb.String(), "seam hook: request not sent after 5 attempts over 1s")
				require.Zero(t, hits.Load())
				return
			}
			require.Equal(t, `{"continue":true}`, out.String())
			require.Empty(t, errb.String())
			require.Equal(t, int32(1), hits.Load())
		})
	}
}

// A daemon that is down for good costs each hook exactly its budget for that
// event and client on the production backoff, and then the hook fails open --
// the reason on stderr, nothing on stdout, exit 0. The full budget is nine
// refused dials over 5s, the short one five over 1s. The retries run under the
// event's own deadline, counted from when runHook started. Every event runs
// under every --client value, an absent one included.
func TestRunHook_BudgetAndDeadlineFollowTheEventAndClient(t *testing.T) {
	ms := time.Millisecond
	spend := map[time.Duration]struct {
		waits []time.Duration
		msg   string
	}{
		hookFullDialBudget: {
			[]time.Duration{100 * ms, 200 * ms, 400 * ms, 800 * ms, time.Second, time.Second, time.Second, 500 * ms},
			"seam hook: request not sent after 9 attempts over 5s",
		},
		hookShortDialBudget: {
			[]time.Duration{100 * ms, 200 * ms, 400 * ms, 300 * ms},
			"seam hook: request not sent after 5 attempts over 1s",
		},
	}
	for _, client := range append([]string{""}, hookClients...) {
		for _, h := range hookEvents {
			name := client
			if name == "" {
				name = "no client"
			}
			t.Run(name+"/"+h.event, func(t *testing.T) {
				budget := h.dialBudgetFor(client)
				want, ok := spend[budget]
				require.True(t, ok, "no expectation for a %s budget", budget)
				clk := &fakeClock{}
				e, out, errb := hookEnvAt(deadAddr(t))
				o := &hookOpts{client: client, retry: clk.policy}
				start := time.Now()
				require.NoError(t, runHook(context.Background(), e, o, []string{h.event}))
				end := time.Now()
				require.Empty(t, out.String())
				require.Contains(t, errb.String(), want.msg)
				require.Equal(t, want.waits, clk.recorded())
				require.Equal(t, budget, clk.waited())

				deadline := clk.lastDeadline()
				require.False(t, deadline.IsZero(), "the retries ran without a deadline")
				require.False(t, deadline.Before(start.Add(h.deadline)) || deadline.After(end.Add(h.deadline)),
					"%s: the retries ran under a deadline %s after runHook started, not %s",
					h.event, deadline.Sub(start).Round(time.Millisecond), h.deadline)
			})
		}
	}
}

// A payload the daemon read before the connection dropped is never sent again:
// the hook fails open on the first failure, with nothing retried.
func TestRunHook_NeverResendsAfterSending(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(answerHooks(&hits, true))
	t.Cleanup(srv.Close)
	clk := &fakeClock{}
	e, out, errb := hookEnvAt(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, runHook(context.Background(), e, &hookOpts{retry: clk.policy}, []string{"user-prompt-submit"}))
	require.Empty(t, out.String())
	require.Contains(t, errb.String(), "seam hook: ")
	require.NotContains(t, errb.String(), errNotSent.Error(), "the payload was sent")
	require.Equal(t, int32(1), hits.Load())
	require.Empty(t, clk.recorded())
}

// The client parses the hook's stdout as its output, so a reply is written
// whole or not at all. One that ends short -- a daemon that dies halfway through
// it -- writes nothing, and the hook fails open with the reason on stderr. A
// reply cut off by the deadline is the same, pinned with the deadline below.
func TestRunHook_WritesNothingOfAReplyCutShort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		// Writing less than the declared length makes the server close the
		// connection once the handler returns.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(hookReply)))
		_, _ = io.WriteString(w, hookReply[:len(hookReply)/2])
	}))
	t.Cleanup(srv.Close)
	e, out, errb := hookEnvAt(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, runHook(context.Background(), e, &hookOpts{}, []string{"session-start"}))
	require.Empty(t, out.String())
	require.Equal(t, "seam hook: read reply: "+io.ErrUnexpectedEOF.Error()+"\n", errb.String())
}

// hangingDials is a hookOpts.httpClient: the production client with its dialer
// replaced. The first refused dials are refused as a closed port refuses them;
// every later one hangs, as a dial to a host that drops packets does, until the
// test ends -- longer than config.DialTimeout, so only the hook's deadline can
// end the request. dials counts them.
func hangingDials(t *testing.T, refused int32, dials *atomic.Int32) func(config.Config) (*http.Client, error) {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	return func(cfg config.Config) (*http.Client, error) {
		c, err := cfg.HTTPClient(0)
		if err != nil {
			return nil, err
		}
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("config.HTTPClient's transport is a %T", c.Transport)
		}
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			if dials.Add(1) <= refused {
				return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("connect: connection refused")}
			}
			// net/http dials under a context detached from the request's deadline:
			// at the deadline the request returns and the dial runs on. The test's
			// end is what releases it.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
				return nil, &net.OpError{Op: "dial", Net: network, Err: os.ErrDeadlineExceeded}
			}
		}
		return c, nil
	}
}

// Whatever the network does, a hook gives up at its deadline and fails open
// before its client kills it: Codex, and Claude Code on an https install, kill
// user-prompt-submit at 5s. Under the one 10s deadline every event had before,
// refused dials for the 1s budget followed by one that hung ran to about 6s,
// because the budget only stops a new attempt from starting and a hanging dial
// runs to config.DialTimeout. A daemon that accepts the request and never
// answers is the same failure one step later, and the payload it has is not
// sent again. One that sends half its reply and then stalls is the same failure
// a step later still, and the half it sent never reaches stdout: the client
// would parse it as the hook's output. On the real clock, because the deadline
// is a real context deadline; the cases run in parallel.
func TestRunHook_GivesUpAtItsDeadlineWhateverTheNetworkDoes(t *testing.T) {
	t.Parallel()
	const event = "user-prompt-submit"
	h, ok := lookupHookEvent(event)
	require.True(t, ok)
	kill, err := hooks.CommandHookTimeouts(hooks.ClientCodex)
	require.NoError(t, err)
	require.Equal(t, 5*time.Second, kill[event], "the timeout this test is about")

	for _, tc := range []struct {
		name    string
		refused int32 // dials refused before they start to hang; -1 dials for real
		dials   int32 // dials the hook makes, all told
		sent    bool  // whether the daemon read the payload
		half    bool  // whether the daemon sends half its reply before it stalls
		msg     string
	}{
		{"every dial hangs", 0, 1, false, false, "request not sent: Post "},
		{"refused, then a dial hangs", 4, 5, false, false, "request not sent after 5 attempts over "},
		{"the daemon accepts and never answers", -1, 0, true, false, "Post "},
		{"the daemon sends half its reply and stalls", -1, 0, true, true, "read reply: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var dials, hits atomic.Int32
			addr := "127.0.0.1:9" // never dialed: hangingDials replaces the dialer
			o := &hookOpts{client: hookClientCodex}
			if tc.refused < 0 {
				release := make(chan struct{})
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					hits.Add(1)
					if tc.half {
						// The headers promise the whole reply; the flush puts
						// them and half of it on the wire.
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("Content-Length", strconv.Itoa(len(hookReply)))
						_, _ = io.WriteString(w, hookReply[:len(hookReply)/2])
						if err := http.NewResponseController(w).Flush(); err != nil {
							t.Errorf("flush half the reply: %v", err)
						}
					}
					select {
					case <-r.Context().Done(): // the hook hung up at its deadline
					case <-release:
					}
				}))
				t.Cleanup(srv.Close)
				t.Cleanup(func() { close(release) }) // runs first: Close waits on the handler
				addr = strings.TrimPrefix(srv.URL, "http://")
			} else {
				o.httpClient = hangingDials(t, tc.refused, &dials)
			}
			e, out, errb := hookEnvAt(addr)

			start := time.Now()
			require.NoError(t, runHook(context.Background(), e, o, []string{event}))
			took := time.Since(start)

			require.GreaterOrEqual(t, took, h.deadline, "the hook gave up before its deadline")
			require.Less(t, took, kill[event], "the hook outlived the %s its client gives it", kill[event])
			require.Empty(t, out.String())
			require.Contains(t, errb.String(), "seam hook: "+event+" gave up at its "+h.deadline.String()+" deadline: ")
			require.Contains(t, errb.String(), tc.msg)
			require.Contains(t, errb.String(), context.DeadlineExceeded.Error())
			require.Equal(t, tc.dials, dials.Load())
			if tc.sent {
				require.NotContains(t, errb.String(), errNotSent.Error(), "the payload was sent")
				require.Equal(t, int32(1), hits.Load(), "the daemon must see the payload once")
			}
		})
	}
}

// The owner's split (2026-10-09): the full budget spans an update's restart for
// the hooks that are costly to lose, the short one only a blip for those whose
// loss is one prompt's recall or one heartbeat. subagent-stop is split by
// client: Claude Code's caches a planning subagent's report, Codex's only
// heartbeats. Pinned whole, every event under every client, so no event or
// client joins without a decision about its budget. Each budget also leaves at
// least 3s of the event's deadline for an attempt that connects at the end of
// it: more than the 2s the daemon gives itself for a briefing or a recall
// (internal/hooks handler.go hookTimeout).
func TestHookDialBudgets_ByClientAndEvent(t *testing.T) {
	const full, short = 5 * time.Second, time.Second
	want := map[string]map[string]time.Duration{
		"claude-code": {
			"session-start":      full,
			"user-prompt-submit": short,
			"session-end":        full,
			"post-tool-use":      full,
			"subagent-start":     full,
			"subagent-stop":      full,
			"permission-request": full,
			"stop":               short,
		},
		"codex": {
			"session-start":      full,
			"user-prompt-submit": short,
			"session-end":        full,
			"post-tool-use":      full,
			"subagent-start":     full,
			"subagent-stop":      short,
			"permission-request": full,
			"stop":               short,
		},
	}
	got := map[string]map[string]time.Duration{}
	for _, client := range hookClients {
		got[client] = map[string]time.Duration{}
		for _, h := range hookEvents {
			got[client][h.event] = h.dialBudgetFor(client)
			require.GreaterOrEqual(t, h.deadline-got[client][h.event], 3*time.Second,
				"%s/%s: an attempt that connects at the end of the budget must still have time to be served", client, h.event)
		}
	}
	require.Equal(t, want, got)

	// An absent --client is Claude Code, as it is to the daemon.
	for _, h := range hookEvents {
		require.Equal(t, h.dialBudgetFor(hookClientClaudeCode), h.dialBudgetFor(""), "%s", h.event)
	}
}

// Every override is live: it names an event in hookEvents and a client in
// hookClients, once, and gives that client a budget other than the event's
// own. dialBudgetFor skips a misspelled event or client in silence, and an
// override equal to the default says the clients differ when they do not.
func TestHookClientDialBudgets_AreLiveOverrides(t *testing.T) {
	seen := map[[2]string]bool{}
	for _, o := range hookClientDialBudgets {
		h, ok := lookupHookEvent(o.event)
		require.True(t, ok, "an override for unknown event %q", o.event)
		require.Contains(t, hookClients, o.client, "an override for unknown client %q", o.client)
		require.NotEqual(t, h.dialBudget, o.budget, "%s/%s overrides the event's budget with itself", o.event, o.client)
		key := [2]string{o.event, o.client}
		require.False(t, seen[key], "%s/%s is overridden twice", o.event, o.client)
		seen[key] = true
	}
}

// Every Claude Code hook is installed without --client, so an absent one must
// take a Claude Code override rather than fall through to the event's own
// budget. No such override exists today, so this plants one.
func TestHookEvent_AbsentClientTakesClaudeCodeOverrides(t *testing.T) {
	saved := hookClientDialBudgets
	t.Cleanup(func() { hookClientDialBudgets = saved })
	hookClientDialBudgets = append(slices.Clone(saved),
		hookClientDialBudget{"session-start", hookClientClaudeCode, hookShortDialBudget})

	h, ok := lookupHookEvent("session-start")
	require.True(t, ok)
	require.Equal(t, hookShortDialBudget, h.dialBudgetFor(""))
	require.Equal(t, hookShortDialBudget, h.dialBudgetFor(hookClientClaudeCode))
	require.Equal(t, hookFullDialBudget, h.dialBudgetFor(hookClientCodex))
}

// Every budget fits inside the timeout its client gives that hook with half to
// spare, for every client and every command hook its profile wires, on either
// transport. Past half, a daemon that is down for good would hold a hook for
// most of what its client allows, on every prompt in user-prompt-submit's case,
// and leave an attempt that connects at the end of the budget little time to be
// served. What keeps a hook inside its client's kill is the deadline, pinned
// next. A test-only import, like the event pin below.
func TestHookDialBudgets_FitInsideEveryClientTimeout(t *testing.T) {
	for _, client := range hooks.HookClients {
		timeouts, err := hooks.CommandHookTimeouts(client)
		require.NoError(t, err)
		require.NotEmpty(t, timeouts, "%s", client)
		for arg, timeout := range timeouts {
			h, ok := lookupHookEvent(arg)
			require.True(t, ok, "%s wires `seam hook %s`, which this CLI rejects", client, arg)
			budget := h.dialBudgetFor(string(client))
			require.LessOrEqual(t, budget, timeout/2,
				"%s kills `seam hook %s` at %s, so its %s budget must be at most half that", client, arg, timeout, budget)
		}
	}
	// An https install makes Claude Code's user-prompt-submit a command hook too
	// (profileForBaseURL), with the same 5s timeout Codex gives it.
	cc, err := hooks.CommandHookTimeouts(hooks.ClientClaudeCode)
	require.NoError(t, err)
	require.Equal(t, 5*time.Second, cc["user-prompt-submit"])
}

// Every deadline ends the hook at least hookDeadlineMargin before its client
// kills it, for every client and every command hook its profile wires, on
// either transport. The budget pin above holds only for dials that are
// refused; this one holds whatever the network does, because the deadline
// bounds the whole forward. Past it, a dial that hangs (a server whose host
// drops packets) or a daemon that never answers runs a hook into its client's
// kill: a timeout warning in place of a quiet fail-open, on every prompt in
// user-prompt-submit's case.
func TestHookDeadlines_EndBeforeEveryClientKillsTheHook(t *testing.T) {
	for _, client := range hooks.HookClients {
		timeouts, err := hooks.CommandHookTimeouts(client)
		require.NoError(t, err)
		require.NotEmpty(t, timeouts, "%s", client)
		for arg, timeout := range timeouts {
			h, ok := lookupHookEvent(arg)
			require.True(t, ok, "%s wires `seam hook %s`, which this CLI rejects", client, arg)
			require.LessOrEqual(t, h.deadline, timeout-hookDeadlineMargin,
				"%s kills `seam hook %s` at %s, so its %s deadline must end %s before that",
				client, arg, timeout, h.deadline, hookDeadlineMargin)
			require.Less(t, h.dialBudgetFor(string(client)), h.deadline,
				"%s/%s: a retry must be able to start before the hook gives up", client, arg)
		}
	}
}

// The deadlines by event, pinned whole so no event joins without one: a second
// under the 10s every client gives a command hook, and under the 5s Codex, and
// Claude Code on an https install, give user-prompt-submit.
func TestHookDeadlines_ByEvent(t *testing.T) {
	const s = time.Second
	want := map[string]time.Duration{
		"session-start":      9 * s,
		"user-prompt-submit": 4 * s,
		"session-end":        9 * s,
		"post-tool-use":      9 * s,
		"subagent-start":     9 * s,
		"subagent-stop":      9 * s,
		"permission-request": 9 * s,
		"stop":               9 * s,
	}
	got := map[string]time.Duration{}
	for _, h := range hookEvents {
		got[h.event] = h.deadline
	}
	require.Equal(t, want, got)
}

// The help names each budget and each deadline from the table, never by hand,
// and marks the event whose budget depends on the client with the clients that
// spend each.
func TestHookCmd_HelpDerivesTheDialBudgetsAndDeadlines(t *testing.T) {
	help := commandHelp(hookCmd)
	require.Contains(t, help, "\ndial retry: 5s for session-start, session-end, post-tool-use, "+
		"subagent-start, subagent-stop (claude-code), permission-request; "+
		"1s for user-prompt-submit, subagent-stop (codex), stop\n")
	require.Contains(t, help, "\ndeadline: 9s for session-start, session-end, post-tool-use, "+
		"subagent-start, subagent-stop, permission-request, stop; 4s for user-prompt-submit\n")
}

// The pin that keeps the CLI's copy of the event table honest against the
// installer's canonical one. Because a hook fails open, a mismatch here is a
// silent no-op: install-hooks writes `seam hook <arg>` lines, and an arg this CLI
// rejects (or forwards to the wrong route) shows up only as a briefing that
// stopped arriving. A test-only import, so the CLI binary stays thin.
func TestHookEvents_MatchTheInstaller(t *testing.T) {
	installed := hooks.CommandHookEndpoints()
	require.NotEmpty(t, installed)
	for arg, endpoint := range installed {
		h, ok := lookupHookEvent(arg)
		require.True(t, ok, "install-hooks writes `seam hook %s`, which this CLI rejects", arg)
		require.Equal(t, endpoint, h.endpoint, "seam hook %s forwards somewhere the installer does not expect", arg)
	}
}

// Like the event table pin above, this keeps the thin CLI's enum identical to
// the daemon/install package's canonical set without importing that dependency
// graph into the production seam binary.
func TestHookClients_MatchTheServerCanonicalSet(t *testing.T) {
	require.Equal(t, enumOf(hooks.HookClients), hookClients)
}
