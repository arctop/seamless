package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// Every test here runs against a stand-in console and a fake program runner:
// never a real daemon, never a real sudo, browser or notifier.

const (
	reqA = "01KACCESSREQUEST0000007F3K"
	reqB = "01KACCESSREQUEST000000BBBB"
)

// fixedNow is the clock the facts are measured against: twelve minutes before
// the requests stop waiting.
var (
	fixedNow  = time.Date(2026, 11, 6, 13, 53, 0, 0, time.UTC)
	expiresAt = fixedNow.Add(12 * time.Minute)
)

var testTerminal = &terminalApprover{Account: "_seamless", Seamlessd: "/opt/seamless/bin/seamlessd", Config: "/opt/seamless/seamless.yaml"}

func factsOf(id, short, project string, warnings ...accessWarning) accessFacts {
	return accessFacts{
		ID: id, Short: short, Project: project, Isolation: "confidential", Access: "read-write", Kind: "request",
		Program: "/usr/local/bin/claude", Agent: "Claude Code 2.1.319", PID: 4242, UID: 501, Session: "cc/quiet-otter", SessionID: "01KSESSION",
		CWD: "/Users/owner/repos/finance", Repo: "finance", Model: "claude-opus-5-5",
		Reason: "reconcile the Q3 ledger", Warnings: warnings,
		RequestedAt: fixedNow.Add(-3 * time.Minute), ExpiresAt: expiresAt, Digest: "c0ffee" + strings.Repeat("0", 58),
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

// listJSON is GET /console/access?format=json.
func listJSON(t *testing.T, reqs ...accessFacts) string {
	if reqs == nil {
		reqs = []accessFacts{}
	}
	return mustJSON(t, accessList{Requests: reqs, Terminal: testTerminal, Durations: []string{"session", "1h", "4h"}, MaxLease: "8h0m0s"})
}

// oneJSON is GET /console/access/{id}?format=json.
func oneJSON(t *testing.T, f accessFacts, mutate ...func(*accessOne)) string {
	one := accessOne{Request: f, Status: "pending", Page: "https://seamless.example.org/console/access/" + f.ID,
		Terminal: testTerminal, Durations: []string{"session", "1h", "4h"}, MaxLease: "8h0m0s"}
	for _, m := range mutate {
		m(&one)
	}
	return mustJSON(t, one)
}

// byRef serves one request under its full id and its short id, as the console
// resolves either.
func byRef(t *testing.T, f accessFacts, mutate ...func(*accessOne)) map[string]string {
	body := oneJSON(t, f, mutate...)
	return map[string]string{"GET /console/access/" + f.ID: body, "GET /console/access/" + f.Short: body}
}

// routes answers the access routes from a fixed table: "GET /console/access" is
// the list, "GET /console/access/<ref>" one request.
func routes(t *testing.T, table map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, ok := table[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such access request"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}
}

// fakeRunner records every program seam asked to run and answers with code.
type fakeRunner struct {
	mu       sync.Mutex
	calls    [][]string
	attached []bool // whether the call was given the terminal's stdin
	code     func(argv []string) (int, error)
}

func (f *fakeRunner) exec(stdin io.Reader) execFunc {
	return func(_ context.Context, argv []string, in io.Reader, _, _ io.Writer) (int, error) {
		f.mu.Lock()
		f.calls = append(f.calls, slices.Clone(argv))
		f.attached = append(f.attached, in != nil && in == stdin)
		f.mu.Unlock()
		if f.code != nil {
			return f.code(argv)
		}
		// By default sudo -n is refused, as it is with no NOPASSWD rule.
		if len(argv) > 1 && argv[0] == "sudo" && argv[1] == "-n" {
			return 1, nil
		}
		return 0, nil
	}
}

func (f *fakeRunner) recorded() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// approveRig is a terminal env aimed at a stand-in console, with the owner's
// typed answers on stdin.
type approveRig struct {
	e      *env
	out    *strings.Builder
	errb   *strings.Builder
	runner *fakeRunner
	ac     *accessConsole
	vars   map[string]string
}

func newApproveRig(t *testing.T, h http.HandlerFunc, typed string) *approveRig {
	t.Helper()
	ac := newAccessConsole(t, h)
	e, out, errb := ac.env()
	e.stdin = strings.NewReader(typed)
	rig := &approveRig{e: e, out: out, errb: errb, runner: &fakeRunner{}, ac: ac, vars: map[string]string{"NO_COLOR": "1"}}
	e.terminal = func() bool { return true }
	e.exec = rig.runner.exec(e.stdin)
	// The stand-in console is on TCP: the check of who answers is faked here,
	// and the real one is exercised in daemon_peer_test.go.
	e.dialDaemon = fakeDaemonPeer(testDaemonUID)
	e.lookupAccount = fakeAccounts
	e.daemonAccount = noDaemonPin // this machine's real pin, if any, is not the stand-in's
	e.getenv = func(k string) string { return rig.vars[k] }
	e.now = func() time.Time { return fixedNow }
	e.goos = "darwin"
	return rig
}

func (r *approveRig) run(argv ...string) int {
	return dispatch(context.Background(), r.e, append([]string{"approve"}, argv...))
}

func approveArgvFor(id, duration, digest string) []string {
	return []string{"sudo", "-k", "-u", "_seamless", "env", "SEAMLESS_CONFIG=/opt/seamless/seamless.yaml",
		"/opt/seamless/bin/seamlessd", "approve", id, "--for", duration, "--digest", digest}
}

func denyArgvFor(id string) []string {
	return []string{"sudo", "-k", "-u", "_seamless", "env", "SEAMLESS_CONFIG=/opt/seamless/seamless.yaml",
		"/opt/seamless/bin/seamlessd", "approve", id, "--deny"}
}

var passwordlessProbe = [][]string{{"sudo", "-k"}, {"sudo", "-n", "-u", "_seamless", "true"}}

// --- the terminal check ---

func TestApprove_RefusesWithoutTheOwnersTerminal(t *testing.T) {
	for _, argv := range [][]string{{}, {"R-7F3K"}, {"--watch"}, {"--passkey", "R-7F3K"}} {
		e, out, errb := stubEnv()
		e.exec = func(context.Context, []string, io.Reader, io.Writer, io.Writer) (int, error) {
			t.Fatal("nothing may run without the owner's terminal")
			return 0, nil
		}
		e.loadConfig = func() (config.Config, error) {
			t.Fatal("nothing may be fetched without the owner's terminal")
			return config.Config{}, nil
		}
		require.Equal(t, 1, dispatch(context.Background(), e, append([]string{"approve"}, argv...)), "%v", argv)
		require.Equal(t, "error: seam approve is the owner's own terminal path: it needs a terminal on stdin and stdout, "+
			"so an agent cannot use it. An agent asks with `seam access request <project>`; the owner decides in their "+
			"own terminal, or with Touch ID or a passkey where one is enrolled\n", errb.String(), "one line")
		require.Empty(t, out.String())
	}
}

// --- listing and selection ---

func TestApprove_NothingWaiting(t *testing.T) {
	rig := newApproveRig(t, routes(t, map[string]string{"GET /console/access": listJSON(t)}), "")
	require.Equal(t, 0, rig.run())
	require.Equal(t, "Nothing is waiting: no access request is pending (seam approve --watch prints each new one).\n", rig.out.String())
	require.Empty(t, rig.runner.recorded())
}

func TestApprove_TheOnlyRequestIsSelected(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	rig := newApproveRig(t, routes(t, map[string]string{
		"GET /console/access":         listJSON(t, a),
		"GET /console/access/" + reqA: oneJSON(t, a),
	}), "\nn\n")
	require.Equal(t, 0, rig.run())
	out := rig.out.String()
	require.Contains(t, out, "Waiting for the owner:\n  1  R-7F3K   example-finance          cc/quiet-otter           no warnings  waits until "+clockTime(expiresAt)+"\n")
	require.Contains(t, out, "Access request R-7F3K")
	require.True(t, strings.HasSuffix(out, "Nothing changed.\n"), out)
	require.Empty(t, rig.runner.recorded(), "an answer other than y or d runs nothing")
}

func TestApprove_SeveralRequestsAskWhichOne(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	b := factsOf(reqB, "R-BBBB", "example-payroll", accessWarning{Code: "no_session", Text: "no session named this agent"})
	table := map[string]string{
		"GET /console/access":         listJSON(t, a, b),
		"GET /console/access/" + reqA: oneJSON(t, a),
		"GET /console/access/" + reqB: oneJSON(t, b),
	}
	for _, tc := range []struct {
		name, typed, want string
	}{
		{"by number", "2\n\nn\n", "R-BBBB"},
		{"by short id, any case", "r-bbbb\n\nn\n", "R-BBBB"},
		{"an unknown answer asks again", "9\nR-7F3K\n\nn\n", "R-7F3K"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newApproveRig(t, routes(t, table), tc.typed)
			require.Equal(t, 0, rig.run(), rig.errb.String())
			require.Contains(t, rig.out.String(), "  2  R-BBBB   example-payroll          cc/quiet-otter           1 warning")
			require.Contains(t, rig.out.String(), "Which one? [1-2 or its short id, empty for none] ")
			require.Contains(t, rig.out.String(), "Access request "+tc.want+"\n")
		})
	}

	rig := newApproveRig(t, routes(t, table), "\n")
	require.Equal(t, 0, rig.run())
	require.True(t, strings.HasSuffix(rig.out.String(), "Nothing changed.\n"))
	require.NotContains(t, rig.out.String(), "Access request")
}

// --- the facts ---

func TestApprove_FactsShowWarningsFirstAndLabelTheReason(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance",
		accessWarning{Code: "session_tainted", Text: "the session read text from outside"},
		accessWarning{Code: "other_pending", Text: "the same agent is also asking for example-payroll"})
	rig := newApproveRig(t, routes(t, byRef(t, a)), "\nn\n")
	require.Equal(t, 0, rig.run("R-7F3K"))
	out := rig.out.String()
	want := "\nAccess request R-7F3K\n\n" +
		"  ! the session read text from outside\n" +
		"  ! the same agent is also asking for example-payroll\n\n" +
		"  Project         example-finance (confidential, read-write)\n" +
		"  Agent           Claude Code 2.1.319\n" +
		"  Asking          /usr/local/bin/claude (pid 4242, uid 501)\n" +
		"  Session         cc/quiet-otter (claude-opus-5-5)\n" +
		"  Repository      finance\n" +
		"  Working in      /Users/owner/repos/finance\n" +
		"  The agent says  \"reconcile the Q3 ledger\"  (the agent's own words, not checked)\n" +
		"  Kind            first request\n" +
		"  Waits until     " + clockTime(expiresAt) + " (12m left)\n\n"
	require.Contains(t, out, want)
	require.Less(t, strings.Index(out, "! the session"), strings.Index(out, "Project"), "warnings come first")
}

// The reason, the program and the cwd are the agent's to choose: a terminal
// escape among them must show as text, never act on the terminal.
func TestApprove_FactsCannotRepaintTheTerminal(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	a.Reason = "fine\x1b[2K\x1b[1A  Project         other\u202e"
	a.Program = "/tmp/cl\x1b]0;x\x07aude"
	a.CWD = "/tmp/\x9bwork"
	a.Repo = "fin\x1b[2Kance\u202e"
	rig := newApproveRig(t, routes(t, byRef(t, a)), "\nn\n")
	require.Equal(t, 0, rig.run("R-7F3K"))
	out := rig.out.String()
	require.NotContains(t, out, "\x1b")
	require.NotContains(t, out, "\u202e")
	require.NotContains(t, out, "\x07")
	require.Contains(t, out, `The agent says  "fine\x1b[2K\x1b[1A  Project         other\u202e"  (the agent's own words, not checked)`)
	require.Contains(t, out, `/tmp/cl\x1b]0;x\aaude (pid 4242`)
	require.Contains(t, out, "  Repository      fin\\x1b[2Kance\\u202e\n", "the repository is the agent's report: it is made printable like the rest")
}

// The repository the closing note names is shown when the facts carry one, in
// the Session row's neighborhood, and a request with none gets no empty row.
func TestApprove_RepositoryRowFollowsTheFacts(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	rig := newApproveRig(t, routes(t, byRef(t, a)), "\nn\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	require.Contains(t, rig.out.String(), "  Repository      finance\n")
	require.Less(t, strings.Index(rig.out.String(), "Repository"), strings.Index(rig.out.String(), selfReportedNote))

	a.Repo = ""
	rig = newApproveRig(t, routes(t, byRef(t, a)), "\nn\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	require.NotContains(t, rig.out.String(), "Repository")
}

func TestApprove_ColorIsForWarningsOnlyAndHonorsNoColor(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance", accessWarning{Code: "no_session", Text: "no session named this agent"})
	table := byRef(t, a)

	rig := newApproveRig(t, routes(t, table), "\nn\n")
	rig.vars = map[string]string{}
	require.Equal(t, 0, rig.run("R-7F3K"))
	out := rig.out.String()
	require.Contains(t, out, "  \x1b[1;31m! no session named this agent\x1b[0m\n", "the indent is never colored")
	require.Equal(t, 1, strings.Count(out, "\x1b[1;31m"), "only the warning is colored")

	for _, vars := range []map[string]string{{"NO_COLOR": "1"}, {"TERM": "dumb"}} {
		rig = newApproveRig(t, routes(t, table), "\nn\n")
		rig.vars = vars
		require.Equal(t, 0, rig.run("R-7F3K"))
		require.NotContains(t, rig.out.String(), "\x1b", "%v", vars)
		require.Contains(t, rig.out.String(), "  ! no session named this agent\n")
	}
}

// --- duration ---

func TestApprove_ForIsValidatedAgainstTheDaemonsDurations(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	rig := newApproveRig(t, routes(t, byRef(t, a)), "y\n")
	require.Equal(t, 1, rig.run("R-7F3K", "--for", "2h"))
	require.Contains(t, rig.errb.String(), `invalid value "2h" for --for: valid values are session, 1h, 4h`)
	require.Empty(t, rig.runner.recorded())

	// Present but empty is not absent: it is refused, not answered with the question.
	rig = newApproveRig(t, routes(t, byRef(t, a)), "y\n")
	require.Equal(t, 1, rig.run("R-7F3K", "--for="))
	require.Contains(t, rig.errb.String(), `invalid value "" for --for: valid values are session, 1h, 4h`)
	require.NotContains(t, rig.out.String(), "For how long?")
	require.Empty(t, rig.runner.recorded())
}

func TestApprove_DurationPrompt(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	table := byRef(t, a)
	for _, tc := range []struct {
		name, typed, duration, words string
	}{
		{"empty is the session", "\ny\n", "session", "until the session ends, at most 8h"},
		{"a listed choice", "4h\ny\n", "4h", "for 4 hours, until " + clockTime(fixedNow.Add(4*time.Hour))},
		{"its start: 1 is 1h, never the first line", "1\ny\n", "1h", "for 1 hour, until " + clockTime(fixedNow.Add(time.Hour))},
		{"its start, any case", "S\ny\n", "session", "until the session ends, at most 8h"},
		{"a name, any case", "4H\ny\n", "4h", "for 4 hours, until " + clockTime(fixedNow.Add(4*time.Hour))},
		{"anything else asks again", "2h\n2\n0\n1h\ny\n", "1h", "for 1 hour, until " + clockTime(fixedNow.Add(time.Hour))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newApproveRig(t, routes(t, table), tc.typed)
			require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
			out := rig.out.String()
			require.Contains(t, out, "For how long?\n"+
				"  session  until the session ends, at most 8h (the default)\n"+
				"  1h       for 1 hour\n"+
				"  4h       for 4 hours\n"+
				"Duration [session, 1h, 4h; Enter for session; deny to deny it]: ")
			require.Contains(t, out, "If approved: Claude Code 2.1.319 (pid 4242) may read and write example-finance "+tc.words+".\n"+
				"Approve R-7F3K? [y/N/d=deny] ")
			calls := rig.runner.recorded()
			require.Equal(t, approveArgvFor(reqA, tc.duration, a.Digest), calls[len(calls)-1])
		})
	}
	rig := newApproveRig(t, routes(t, table), "2h\n")
	require.Equal(t, 0, rig.run("R-7F3K"))
	require.Contains(t, rig.errb.String(), "answer with one of session, 1h, 4h, or the start of one (or deny, in full, to deny it)\n")
	require.True(t, strings.HasSuffix(rig.out.String(), "Nothing changed.\n"), "end of input decides nothing")
	require.Empty(t, rig.runner.recorded())
}

// The answer names a duration, or starts one, in any case; a bare number is
// never an index into the list.
func TestMatchDuration(t *testing.T) {
	ds := []string{"session", "1h", "4h"}
	capped := []string{"session", "1h", "4h", "20m"}
	for _, tc := range []struct {
		ds      []string
		ans     string
		want    string
		matches int
	}{
		{ds, "session", "session", 1},
		{ds, "s", "session", 1},
		{ds, "SESS", "session", 1},
		{ds, "1", "1h", 1},
		{ds, "1H", "1h", 1},
		{ds, "4", "4h", 1},
		{ds, "2", "", 0},
		{ds, "3", "", 0},
		{ds, "1h30m", "", 0},
		{ds, "", "", 0},
		{capped, "2", "20m", 1},
		{[]string{"session", "1h", "12h"}, "1", "", 2},
		{[]string{"session", "1h", "12h"}, "1h", "1h", 1},
	} {
		got, n := matchDuration(tc.ds, tc.ans)
		require.Equal(t, tc.want, got, "%v / %q", tc.ds, tc.ans)
		require.Equal(t, tc.matches, n, "%v / %q", tc.ds, tc.ans)
	}
}

// The word deny, in full and in any case, denies straight from the duration
// question: no duration is chosen, no "If approved" is said, and the denial
// commits as d at the y/N question does. Nothing else typed at the question
// can deny, and no duration ever reads as a denial.
func TestApprove_DenyFromTheDurationPrompt(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	for _, typed := range []string{"deny\n", "DENY\n", "Deny\n"} {
		rig := newApproveRig(t, routes(t, byRef(t, a)), typed)
		require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
		out := rig.out.String()
		require.NotContains(t, out, "If approved", "%q", typed)
		require.NotContains(t, out, "Approve R-7F3K?", "%q", typed)
		require.Contains(t, out, "sudo asks for your Touch ID or password next: it commits the denial as _seamless.\n", "%q", typed)
		require.Equal(t, append(slices.Clone(passwordlessProbe), denyArgvFor(reqA)), rig.runner.recorded(), "%q", typed)
	}

	// Only the word in full: "d" and "den" are not denials, and "deny" never
	// starts a duration.
	for _, typed := range []string{"d\ndeny\n", "den\nDENY\n", "denied\ndeny\n"} {
		rig := newApproveRig(t, routes(t, byRef(t, a)), typed)
		require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
		require.Contains(t, rig.errb.String(), "answer with one of session, 1h, 4h, or the start of one (or deny, in full, to deny it)\n", "%q", typed)
		require.Equal(t, append(slices.Clone(passwordlessProbe), denyArgvFor(reqA)), rig.runner.recorded(), "%q", typed)
	}

	// A duration the daemon offers wins over the word: a typed duration can
	// never become a denial.
	odd := byRef(t, a, func(o *accessOne) { o.Durations = []string{"session", "deny", "1h"} })
	rig := newApproveRig(t, routes(t, odd), "deny\ny\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	require.Contains(t, rig.out.String(), "If approved:", "deny is a duration here: the approval still asks y/N")
	require.Equal(t, approveArgvFor(reqA, "deny", a.Digest), rig.runner.recorded()[len(rig.runner.recorded())-1])
	require.NotEqual(t, denyArgvFor(reqA), rig.runner.recorded()[len(rig.runner.recorded())-1])

	// Without the default: the same word on the shorter prompt.
	short := byRef(t, a, func(o *accessOne) { o.Durations = []string{"1h", "4h"} })
	rig = newApproveRig(t, routes(t, short), "deny\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	require.Contains(t, rig.out.String(), "Duration [1h, 4h; deny to deny it]: ")
	require.Equal(t, denyArgvFor(reqA), rig.runner.recorded()[len(rig.runner.recorded())-1])

	// With --for there is no duration question: d at the y/N question denies,
	// as it always did, and so does the word (it is the same answer).
	rig = newApproveRig(t, routes(t, byRef(t, a)), "deny\n")
	require.Equal(t, 0, rig.run("R-7F3K", "--for", "1h"), rig.errb.String())
	require.Contains(t, rig.out.String(), "Approve R-7F3K? [y/N/d=deny] ")
	require.Equal(t, denyArgvFor(reqA), rig.runner.recorded()[len(rig.runner.recorded())-1])
}

// An ambiguous start asks again, naming the choices; a max_lease cap's
// duration is listed and answered like the others.
func TestApprove_DurationPromptAmbiguousAndCapped(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	twelve := byRef(t, a, func(o *accessOne) { o.Durations = []string{"session", "1h", "12h"}; o.MaxLease = "24h0m0s" })
	rig := newApproveRig(t, routes(t, twelve), "1\n1h\ny\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	require.Contains(t, rig.errb.String(), `"1" starts more than one of session, 1h, 12h: type more of it`+"\n")
	require.Equal(t, approveArgvFor(reqA, "1h", a.Digest), rig.runner.recorded()[len(rig.runner.recorded())-1])

	capped := byRef(t, a, func(o *accessOne) { o.Durations = []string{"session", "20m"}; o.MaxLease = "20m0s" })
	rig = newApproveRig(t, routes(t, capped), "2\ny\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	require.Contains(t, rig.out.String(), "  session  until the session ends, at most 20m (the default)\n"+
		"  20m      for 20m\n"+
		"Duration [session, 20m; Enter for session; deny to deny it]: ")
	require.Contains(t, rig.out.String(), "for 20m, until "+clockTime(fixedNow.Add(20*time.Minute))+".\n")
	require.Equal(t, approveArgvFor(reqA, "20m", a.Digest), rig.runner.recorded()[len(rig.runner.recorded())-1])
}

// A fixed duration's end is said as a clock time, capped by max_lease; the
// session's is not a clock time.
func TestGrantLength(t *testing.T) {
	for _, tc := range []struct {
		choice, max string
		want        time.Duration
		ok          bool
	}{
		{"1h", "8h0m0s", time.Hour, true},
		{"4h", "2h0m0s", 2 * time.Hour, true},
		{"20m", "", 20 * time.Minute, true},
		{"session", "8h0m0s", 0, false},
		{"week", "8h0m0s", 0, false},
	} {
		got, ok := grantLength(tc.choice, tc.max)
		require.Equal(t, tc.ok, ok, tc.choice)
		require.Equal(t, tc.want, got, tc.choice)
	}
}

func TestDurationWords(t *testing.T) {
	for _, tc := range []struct{ choice, max, want string }{
		{"session", "8h0m0s", "until the session ends, at most 8h"},
		{"session", "", "until the session ends"},
		{"1h", "8h0m0s", "for 1 hour"},
		{"4h", "8h0m0s", "for 4 hours"},
		{"4h", "2h0m0s", "for 2h (the max_lease cap)"},
		{"90m", "8h0m0s", "for 1h30m"},
		{"week", "8h0m0s", "for week"},
	} {
		require.Equal(t, tc.want, durationWords(tc.choice, tc.max), "%s / %s", tc.choice, tc.max)
	}
}

// --- the sudo commit ---

func TestApprove_RunsSudoWithTheExactArgv(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	table := byRef(t, a)

	rig := newApproveRig(t, routes(t, table), "y\n")
	require.Equal(t, 0, rig.run("R-7F3K", "--for", "1h"), rig.errb.String())
	require.Equal(t, append(slices.Clone(passwordlessProbe), approveArgvFor(reqA, "1h", a.Digest)), rig.runner.recorded())
	require.Equal(t, []bool{false, false, true}, rig.runner.attached, "only the commit gets the terminal, for sudo's prompt")
	require.Contains(t, rig.out.String(), "If approved: Claude Code 2.1.319 (pid 4242) may read and write example-finance for 1 hour, until "+
		clockTime(fixedNow.Add(time.Hour))+".\n"+
		"Approve R-7F3K? [y/N/d=deny] ")
	require.NotContains(t, rig.out.String(), "For how long?", "--for skips the question")

	rig = newApproveRig(t, routes(t, table), "d\n")
	require.Equal(t, 0, rig.run("R-7F3K", "--for", "session"), rig.errb.String())
	require.Equal(t, append(slices.Clone(passwordlessProbe), denyArgvFor(reqA)), rig.runner.recorded())

	// Named by its full id, the request is fetched by it; the commit always names the full id.
	rig = newApproveRig(t, routes(t, map[string]string{"GET /console/access/" + reqA: oneJSON(t, a)}), "yes\n")
	require.Equal(t, 0, rig.run(reqA, "--for", "4h"), rig.errb.String())
	require.Equal(t, approveArgvFor(reqA, "4h", a.Digest), rig.runner.recorded()[2])
}

func TestApprove_AnythingButYesOrDenyChangesNothing(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	for _, typed := range []string{"n\n", "\n", "maybe\n", ""} {
		rig := newApproveRig(t, routes(t, byRef(t, a)), typed)
		require.Equal(t, 0, rig.run("R-7F3K", "--for", "1h"), "%q", typed)
		require.True(t, strings.HasSuffix(rig.out.String(), "Nothing changed.\n"), "%q", typed)
		require.Empty(t, rig.runner.recorded(), "%q", typed)
	}
}

func TestApprove_RelaysAFailedCommit(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	rig := newApproveRig(t, routes(t, byRef(t, a)), "y\n")
	rig.runner.code = func(argv []string) (int, error) {
		if slices.Contains(argv, "approve") {
			return 1, nil
		}
		return 1, nil
	}
	require.Equal(t, 1, rig.run("R-7F3K", "--for", "1h"))
	require.Equal(t, "error: the approval of R-7F3K did not complete: nothing was approved (sudo exited with status 1)\n", rig.errb.String())

	rig = newApproveRig(t, routes(t, byRef(t, a)), "d\n")
	rig.runner.code = func([]string) (int, error) { return 1, nil }
	require.Equal(t, 1, rig.run("R-7F3K", "--for", "1h"))
	require.Equal(t, "error: the denial of R-7F3K did not complete: nothing was denied (sudo exited with status 1)\n", rig.errb.String())

	rig = newApproveRig(t, routes(t, byRef(t, a)), "y\n")
	rig.runner.code = func([]string) (int, error) { return -1, errors.New(`exec: "sudo": executable file not found in $PATH`) }
	require.Equal(t, 1, rig.run("R-7F3K", "--for", "1h"))
	require.Contains(t, rig.errb.String(), "run sudo: exec: \"sudo\": executable file not found")
}

func TestApprove_WarnsWhenSudoAsksForNothing(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	rig := newApproveRig(t, routes(t, byRef(t, a)), "y\n")
	rig.runner.code = func([]string) (int, error) { return 0, nil } // sudo -n succeeds: a NOPASSWD rule
	require.Equal(t, 0, rig.run("R-7F3K", "--for", "1h"))
	require.Contains(t, rig.errb.String(), "! warning: sudo runs commands as _seamless here without asking for authentication")
	require.Contains(t, rig.errb.String(), "this approval proves nothing")
	require.Equal(t, approveArgvFor(reqA, "1h", a.Digest), rig.runner.recorded()[2], "a warning, not a refusal")
	require.NotContains(t, rig.out.String(), "sudo asks", "sudo will ask for nothing: no prompt is announced")

	rig = newApproveRig(t, routes(t, byRef(t, a)), "y\n")
	require.Equal(t, 0, rig.run("R-7F3K", "--for", "1h"))
	require.NotContains(t, rig.errb.String(), "warning", "sudo -n refused: nothing to warn about")
	require.Contains(t, rig.out.String(), "sudo asks for your Touch ID or password next: it commits the approval as _seamless.\n")
}

func TestApprove_WithoutATerminalApprover(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	for _, tc := range []struct {
		name   string
		mutate func(*accessOne)
		want   string
	}{
		{"absent", func(o *accessOne) { o.Terminal = nil }, "it named no terminal approver"},
		{"no account", func(o *accessOne) { o.Terminal = &terminalApprover{Seamlessd: "/x", Config: "/y"} }, "it does not know its own account"},
		{"no config", func(o *accessOne) { o.Terminal = &terminalApprover{Account: "_seamless", Seamlessd: "/x"} }, "it does not know its config file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newApproveRig(t, routes(t, byRef(t, a, tc.mutate)), "y\n")
			require.Equal(t, 1, rig.run("R-7F3K", "--for", "1h"))
			require.Contains(t, rig.errb.String(), "the daemon did not say how to reach the terminal path ("+tc.want+")")
			require.Contains(t, rig.errb.String(), "on the console's passkey page instead: https://seamless.example.org/console/access/"+reqA)
			require.NotContains(t, rig.out.String(), "Access request R-7F3K", "a daemon that names no account is not verified, so nothing is shown")
			require.Empty(t, rig.runner.recorded())
		})
	}
	rig := newApproveRig(t, routes(t, byRef(t, a, func(o *accessOne) { o.Terminal, o.Page = nil, "" })), "")
	require.Equal(t, 1, rig.run("R-7F3K"))
	require.Contains(t, rig.errb.String(), "/console/access/"+reqA+" (open it on the passkey origin)")
}

// What the daemon names becomes sudo's arguments: anything shaped to be read
// as a flag, or a relative program, is refused rather than run.
func TestApprove_RefusesAnUnusableTerminalApprover(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	for _, mutate := range []func(*accessOne){
		func(o *accessOne) { o.Terminal = &terminalApprover{Account: "-s", Seamlessd: "/x", Config: "/y"} },
		func(o *accessOne) {
			o.Terminal = &terminalApprover{Account: "_seamless", Seamlessd: "seamlessd", Config: "/y"}
		},
		func(o *accessOne) { o.Request.ID = "--deny" },
		func(o *accessOne) { o.Request.Digest = "--for" },
	} {
		rig := newApproveRig(t, routes(t, byRef(t, a, mutate)), "y\n")
		require.Equal(t, 1, rig.run("R-7F3K", "--for", "1h"))
		require.Empty(t, rig.runner.recorded())
	}
}

func TestApprove_OnlyAPendingRequestIsDecided(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	for _, tc := range []struct {
		mutate func(*accessOne)
		want   string
	}{
		{func(o *accessOne) { o.Status = "approved" }, "R-7F3K is already approved: nothing to decide"},
		{func(o *accessOne) { o.Status = "denied" },
			"R-7F3K was denied: nothing to decide (example-finance stays locked for that session until the agent asks again)"},
		{func(o *accessOne) { o.Status, o.Request.Kind = "denied", "grant" },
			"R-7F3K was denied: nothing to decide (example-finance stays closed to that session until the agent asks again)"},
		{func(o *accessOne) { o.Status = "expired" }, "R-7F3K expired at " + clockTime(expiresAt) +
			" without a decision: nothing to decide (the agent asks again with its next call to example-finance)"},
		{func(o *accessOne) { o.Request.ExpiresAt = fixedNow.Add(-time.Second) }, "R-7F3K expired at " +
			clockTime(fixedNow.Add(-time.Second)) + " without a decision"},
		{func(o *accessOne) { o.Status = "revoked" }, "R-7F3K is revoked, not pending: nothing to decide"},
	} {
		rig := newApproveRig(t, routes(t, byRef(t, a, tc.mutate)), "y\n")
		require.Equal(t, 1, rig.run("R-7F3K", "--for", "1h"))
		require.Contains(t, rig.errb.String(), tc.want)
		require.Equal(t, 1, strings.Count(rig.errb.String(), "\n"), "one line")
		require.Empty(t, rig.runner.recorded())
	}
}

func TestApprove_UnknownRequestSurfacesTheConsolesMessage(t *testing.T) {
	rig := newApproveRig(t, routes(t, map[string]string{}), "")
	require.Equal(t, 1, rig.run("R-0000"))
	require.Equal(t, "error: no such access request\n", rig.errb.String())
}

// --- --passkey ---

func TestApprove_PasskeyOpensThePage(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	page := "https://seamless.example.org/console/access/" + reqA
	table := byRef(t, a)
	for goos, want := range map[string][]string{
		"darwin":  {"open", page},
		"linux":   {"xdg-open", page},
		"windows": {"rundll32", "url.dll,FileProtocolHandler", page},
	} {
		rig := newApproveRig(t, routes(t, table), "")
		rig.e.goos = goos
		require.Equal(t, 0, rig.run("--passkey", reqA), goos)
		require.Equal(t, [][]string{want}, rig.runner.recorded(), goos)
		require.Equal(t, "Opened "+page+" in the browser: decide R-7F3K there with a passkey.\n", rig.out.String())
	}

	rig := newApproveRig(t, routes(t, table), "")
	rig.runner.code = func([]string) (int, error) { return -1, errors.New("no browser") }
	require.Equal(t, 0, rig.run("--passkey", reqA))
	require.Equal(t, "Could not open a browser. Open this page to decide R-7F3K with a passkey:\n  "+page+"\n", rig.out.String())

	rig = newApproveRig(t, routes(t, byRef(t, a, func(o *accessOne) { o.Page = "" })), "")
	require.Equal(t, 1, rig.run("--passkey", reqA))
	require.Contains(t, rig.errb.String(), "R-7F3K has no passkey page: this daemon offers no passkey approval, so decide it here: seam approve R-7F3K")
	require.Empty(t, rig.runner.recorded())

	rig = newApproveRig(t, routes(t, byRef(t, a, func(o *accessOne) { o.Page = "file:///etc/passwd" })), "")
	require.Equal(t, 1, rig.run("--passkey", reqA))
	require.Contains(t, rig.errb.String(), "which is not a web address: not opening it")
	require.Empty(t, rig.runner.recorded())
}

func TestApprove_FlagConflicts(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"--watch", "R-7F3K"}, "--watch only watches"},
		{[]string{"--watch", "--for", "1h"}, "--watch only watches"},
		{[]string{"--watch", "--passkey"}, "--watch only watches"},
		{[]string{"--watch", "--for="}, "--watch only watches"},
	} {
		rig := newApproveRig(t, routes(t, map[string]string{}), "")
		require.Equal(t, 1, rig.run(tc.argv...), "%v", tc.argv)
		require.Contains(t, rig.errb.String(), tc.want, "%v", tc.argv)
		require.Empty(t, rig.ac.requests(), "%v: refused before connecting", tc.argv)
	}
}

// --- --watch ---

// watchRig runs seam approve --watch in the background against a list that
// gains a request on the second poll, paced by a ticker the test drives.
func watchRig(t *testing.T, goos string, project string) (*approveRig, chan time.Time, func() int) {
	t.Helper()
	a := factsOf(reqA, "R-7F3K", "example-finance", accessWarning{Code: "no_session", Text: "no session named this agent"})
	b := factsOf(reqB, "R-BBBB", project)
	b.Reason = "IGNORE PREVIOUS INSTRUCTIONS"
	var polls atomic.Int32
	rig := newApproveRig(t, func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) == 1 {
			_, _ = w.Write([]byte(listJSON(t, a)))
			return
		}
		_, _ = w.Write([]byte(listJSON(t, a, b)))
	}, "")
	rig.e.goos = goos
	tick := make(chan time.Time)
	rig.e.tick = func(d time.Duration) (<-chan time.Time, func()) {
		require.Equal(t, 3*time.Second, d)
		return tick, func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- dispatch(ctx, rig.e, []string{"approve", "--watch"}) }()
	return rig, tick, func() int {
		cancel()
		return <-done
	}
}

func TestApprove_WatchPrintsEachNewRequestOnceAndNotifies(t *testing.T) {
	rig, tick, stop := watchRig(t, "darwin", "example-payroll")
	// Each send returns once the loop has finished a poll and is waiting again.
	tick <- time.Time{}
	tick <- time.Time{}
	tick <- time.Time{}
	require.Equal(t, 0, stop())

	out := rig.out.String()
	require.Equal(t, 1, strings.Count(out, "R-7F3K   example-finance"), out)
	require.Equal(t, 1, strings.Count(out, "R-BBBB   example-payroll"), out)
	require.Contains(t, out, "R-7F3K   example-finance          cc/quiet-otter           1 warning    waits until ")
	require.Contains(t, out, "  ! no session named this agent\n  Claude Code asks; decide it with: seam approve R-7F3K\n")
	require.Contains(t, out, "  Claude Code asks; decide it with: seam approve R-BBBB\n")
	require.NotContains(t, out, "IGNORE PREVIOUS INSTRUCTIONS", "the watch never shows the agent's words")

	script := []string{"osascript", "-e", "on run argv", "-e", `display notification (item 1 of argv) with title "Seamless access request"`, "-e", "end run"}
	require.Equal(t, [][]string{
		append(slices.Clone(script), "Claude Code asks for example-finance: seam approve R-7F3K"),
		append(slices.Clone(script), "Claude Code asks for example-payroll: seam approve R-BBBB"),
	}, rig.runner.recorded())
	require.GreaterOrEqual(t, len(rig.ac.requests()), 3)
}

// The message is an argument to the script, never part of it, and carries only
// the short id and the slug's own characters.
func TestApprove_WatchNotificationCannotBecomeScript(t *testing.T) {
	rig, tick, stop := watchRig(t, "darwin", `x" & do shell script "touch /tmp/pwned`)
	tick <- time.Time{}
	tick <- time.Time{}
	require.Equal(t, 0, stop())
	calls := rig.runner.recorded()
	require.Len(t, calls, 2)
	last := calls[1]
	require.Equal(t, `display notification (item 1 of argv) with title "Seamless access request"`, last[4], "the script is constant")
	require.Equal(t, "Claude Code asks for xdoshellscripttouchtmppwned: seam approve R-BBBB", last[len(last)-1])
}

func TestApprove_WatchRingsTheBellOffMacOS(t *testing.T) {
	rig, tick, stop := watchRig(t, "linux", "example-payroll")
	tick <- time.Time{}
	tick <- time.Time{}
	require.Equal(t, 0, stop())
	require.Empty(t, rig.runner.recorded(), "no notifier off macOS")
	require.Equal(t, 2, strings.Count(rig.out.String(), "\a"))
}

func TestApprove_WatchFailsFastOnTheFirstPoll(t *testing.T) {
	rig := newApproveRig(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no project is locked, so there are no access requests"}`))
	}, "")
	rig.e.tick = func(time.Duration) (<-chan time.Time, func()) { return make(chan time.Time), func() {} }
	require.Equal(t, 1, rig.run("--watch"))
	require.Contains(t, rig.errb.String(), "error: no project is locked")
}

// --- helpers ---

func TestPrintable(t *testing.T) {
	for in, want := range map[string]string{
		"plain words":            "plain words",
		"é and ü stay":           "é and ü stay",
		"esc\x1b[31m":            `esc\x1b[31m`,
		"rtl\u202eoverride":      `rtl\u202eoverride`,
		"bad\xffbyte":            `bad\xffbyte`,
		"tab\tand\nnewline":      `tab\tand\nnewline`,
		`a "quoted" \ backslash`: `a "quoted" \ backslash`,
	} {
		require.Equal(t, want, printable(in), "%q", in)
	}
}

func TestCompactDuration(t *testing.T) {
	require.Equal(t, "8h", compactDuration(8*time.Hour))
	require.Equal(t, "1h30m", compactDuration(90*time.Minute))
	require.Equal(t, "12m", compactDuration(12*time.Minute))
	require.Equal(t, "3s", compactDuration(3*time.Second))
}

// --- read grants without peer_auth (the advisory tier) ---

// grantFacts is a read grant request as an advisory daemon shows it: no
// process facts, read-only, the copy line among the warnings.
func grantFacts() accessFacts {
	f := factsOf(reqA, "R-7F3K", "example-finance",
		accessWarning{Code: "copy_into", Text: "content from example-finance may be copied into example-ops's memories"})
	f.Kind, f.Access, f.From, f.Program, f.Agent, f.PID, f.UID = "grant", "read-only", "example-ops", "", "", 0, 0
	return f
}

func advisoryOne(o *accessOne) { o.Advisory, o.Terminal, o.Page = true, nil, "" }

func TestApprove_AdvisoryGrantCommitsOverTheConsole(t *testing.T) {
	f := grantFacts()
	table := byRef(t, f, advisoryOne)
	table["POST /console/access/"+reqA+"/approve"] = `{"approved":"R-7F3K","project":"example-finance","from":"example-ops",` +
		`"grant":"01KGRANT","expires_at":"` + fixedNow.Add(time.Hour).Format(time.RFC3339) + `"}`
	rig := newApproveRig(t, routes(t, table), "1h\ny\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	out := rig.out.String()
	require.Contains(t, out, "  ! content from example-finance may be copied into example-ops's memories\n")
	require.Contains(t, out, "  Project         example-finance (confidential, read-only)\n")
	require.Contains(t, out, "  For             sessions working in example-ops\n")
	require.Contains(t, out, "  Asking          a session (without peer_auth the daemon knows no process)\n")
	require.Contains(t, out, "  Kind            a read-only grant of a confidential project\n")
	require.Contains(t, out, "  ! This approval is advisory: without peer_auth it is made with the bearer key, and any process holding the key could make it.\n")
	require.Contains(t, out, "If approved: the session cc/quiet-otter, working in example-ops, may read example-finance (read-only) for 1 hour, until "+
		clockTime(fixedNow.Add(time.Hour))+".\n"+
		"Approve R-7F3K? [y/N/d=deny] ")
	require.NotContains(t, out, "sudo asks", "no sudo: the bearer key commits it")
	require.Contains(t, out, "Approved R-7F3K: sessions working in example-ops may read example-finance (read-only) until "+clockTime(fixedNow.Add(time.Hour))+".\n")
	require.Empty(t, rig.runner.recorded(), "no sudo: the bearer key commits it")

	var posted *seenRequest
	for _, r := range rig.ac.requests() {
		if r.method == http.MethodPost {
			posted = &r
		}
	}
	require.NotNil(t, posted)
	require.Equal(t, "/console/access/"+reqA+"/approve?format=json", posted.uri)
	require.Equal(t, []string{"1h"}, posted.form["duration"])
	require.Equal(t, []string{f.Digest}, posted.form["digest"])
	require.Empty(t, posted.form["response"], "no passkey: the bearer key's decision")
	require.Equal(t, "Bearer fixture-key", posted.header.Get("Authorization"))
}

func TestApprove_AdvisoryGrantDenyAndNo(t *testing.T) {
	f := grantFacts()
	table := byRef(t, f, advisoryOne)
	table["POST /console/access/"+reqA+"/deny"] = `{"denied":"R-7F3K","project":"example-finance"}`
	rig := newApproveRig(t, routes(t, table), "\nd\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	require.Contains(t, rig.out.String(), "Denied R-7F3K: example-finance stays closed to that session.\n")

	rig = newApproveRig(t, routes(t, table), "\nn\n")
	require.Equal(t, 0, rig.run("R-7F3K"))
	require.True(t, strings.HasSuffix(rig.out.String(), "Nothing changed.\n"))
	for _, r := range rig.ac.requests() {
		require.Equal(t, http.MethodGet, r.method, "an answer other than y or d posts nothing")
	}

	// The word deny at the duration question commits the denial here too,
	// with no duration in the form.
	rig = newApproveRig(t, routes(t, table), "deny\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	require.Contains(t, rig.out.String(), "Denied R-7F3K: example-finance stays closed to that session.\n")
	require.NotContains(t, rig.out.String(), "If approved")
	var denied *seenRequest
	for _, r := range rig.ac.requests() {
		if r.method == http.MethodPost {
			denied = &r
		}
	}
	require.NotNil(t, denied)
	require.Equal(t, "/console/access/"+reqA+"/deny?format=json", denied.uri)
	require.Empty(t, denied.form["duration"], "a denial chooses no duration")
}

// A grant request on a daemon under peer_auth is not advisory: it goes
// through sudo exactly as a lock's request does.
func TestApprove_PeerGrantGoesThroughSudo(t *testing.T) {
	f := grantFacts()
	f.Program, f.Agent, f.PID, f.UID = "/usr/local/bin/claude", "Claude Code 2.1.319", 4242, 501
	rig := newApproveRig(t, routes(t, byRef(t, f)), "4h\ny\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	require.NotContains(t, rig.out.String(), "advisory")
	require.Equal(t, append(slices.Clone(passwordlessProbe), approveArgvFor(reqA, "4h", f.Digest)), rig.runner.recorded())
}

// --- the layout, byte for byte ---

// The whole transcript of one approval, with and without color: the warnings
// first and indented like the facts, the facts' values aligned past the
// widest label, the durations by name, what the approval grants, and the
// line announcing sudo's prompt -- all before sudo runs. Color is the
// warnings' alone, and never their indent.
func TestApprove_TranscriptGolden(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance",
		accessWarning{Code: "session_tainted", Text: "the session read text from outside"},
		accessWarning{Code: "other_pending", Text: "the same agent is also asking for example-payroll"})
	for _, tc := range []struct {
		name string
		vars map[string]string
		warn func(string) string
	}{
		{"NO_COLOR", map[string]string{"NO_COLOR": "1", "TERM": "xterm-256color"}, func(s string) string { return "! " + s }},
		{"TERM=dumb", map[string]string{"TERM": "dumb"}, func(s string) string { return "! " + s }},
		{"color", map[string]string{"TERM": "xterm-256color"}, func(s string) string { return "\x1b[1;31m! " + s + "\x1b[0m" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newApproveRig(t, routes(t, byRef(t, a)), "1\ny\n")
			rig.vars = tc.vars
			var atSudo string
			rig.runner.code = func(argv []string) (int, error) {
				if slices.Contains(argv, "approve") {
					atSudo = rig.out.String()
					rig.out.WriteString("Approved R-7F3K: Claude Code 2.1.319 (pid 4242) may read and write example-finance until 15:05.\n")
					return 0, nil
				}
				return 1, nil // sudo -n refused: no NOPASSWD rule
			}
			require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
			want := "\n" +
				"Access request R-7F3K\n" +
				"\n" +
				"  " + tc.warn("the session read text from outside") + "\n" +
				"  " + tc.warn("the same agent is also asking for example-payroll") + "\n" +
				"\n" +
				"  Project         example-finance (confidential, read-write)\n" +
				"  Agent           Claude Code 2.1.319\n" +
				"  Asking          /usr/local/bin/claude (pid 4242, uid 501)\n" +
				"  Session         cc/quiet-otter (claude-opus-5-5)\n" +
				"  Repository      finance\n" +
				"  Working in      /Users/owner/repos/finance\n" +
				"  The agent says  \"reconcile the Q3 ledger\"  (the agent's own words, not checked)\n" +
				"  Kind            first request\n" +
				"  Waits until     " + clockTime(expiresAt) + " (12m left)\n" +
				"\n" +
				selfReportedNote + "\n" +
				"\n" +
				"For how long?\n" +
				"  session  until the session ends, at most 8h (the default)\n" +
				"  1h       for 1 hour\n" +
				"  4h       for 4 hours\n" +
				// The owner's typed answers echo on the terminal, not here.
				"Duration [session, 1h, 4h; Enter for session; deny to deny it]: " +
				"If approved: Claude Code 2.1.319 (pid 4242) may read and write example-finance for 1 hour, until " +
				clockTime(fixedNow.Add(time.Hour)) + ".\n" +
				"Approve R-7F3K? [y/N/d=deny] " +
				"sudo asks for your Touch ID or password next: it commits the approval as _seamless.\n"
			require.Equal(t, want, atSudo, "everything is said before sudo runs")
			require.Equal(t, want+"Approved R-7F3K: Claude Code 2.1.319 (pid 4242) may read and write example-finance until 15:05.\n",
				rig.out.String())
			require.Empty(t, rig.errb.String())
		})
	}
}

// The facts' values share one column, the label column's width, widened for
// every row when a label would leave less than two spaces before its value.
func TestAlignFacts(t *testing.T) {
	require.Equal(t, []string{
		"  Project         example-finance",
		"  The agent says  \"x\"",
	}, alignFacts([][2]string{{"Project", "example-finance"}, {"The agent says", `"x"`}}))
	require.Equal(t, []string{
		"  Kind" + strings.Repeat(" ", 15) + "first request",
		"  A label this long  v",
	}, alignFacts([][2]string{{"Kind", "first request"}, {"A label this long", "v"}}))
}

// What an approval grants, in the words the confirmation shows.
func TestGrantWords(t *testing.T) {
	lock := factsOf(reqA, "R-7F3K", "example-finance")
	refresh := lock
	refresh.Kind = "refresh"
	unnamed := lock
	unnamed.Agent = ""
	bare := lock
	bare.Agent, bare.Program = "", ""
	odd := lock
	odd.Access = "write-only"
	peerGrant := grantFacts()
	peerGrant.Program, peerGrant.Agent, peerGrant.PID, peerGrant.UID = "/opt/homebrew/bin/codex", "Codex", 4300, 501
	advisory := grantFacts()
	advisory.Session = ""
	h1, h4 := clockTime(fixedNow.Add(time.Hour)), clockTime(fixedNow.Add(4*time.Hour))
	for _, tc := range []struct {
		name     string
		f        accessFacts
		duration string
		want     string
	}{
		{"a lock's request", lock, "session", "Claude Code 2.1.319 (pid 4242) may read and write example-finance until the session ends, at most 8h."},
		{"for an hour", lock, "1h", "Claude Code 2.1.319 (pid 4242) may read and write example-finance for 1 hour, until " + h1 + "."},
		{"a refresh", refresh, "4h", "Claude Code 2.1.319 (pid 4242) may read and write example-finance for 4 hours, until " + h4 + ", in place of the lease it holds now."},
		{"no agent named: the program", unnamed, "1h", "claude (pid 4242) may read and write example-finance for 1 hour, until " + h1 + "."},
		{"no program known: the pid", bare, "1h", "pid 4242 may read and write example-finance for 1 hour, until " + h1 + "."},
		{"an access it does not know", odd, "1h", "Claude Code 2.1.319 (pid 4242) may reach example-finance (write-only) for 1 hour, until " + h1 + "."},
		{"a read grant under peer_auth", peerGrant, "1h", "Codex (pid 4300), working in example-ops, may read example-finance (read-only) for 1 hour, until " + h1 + "."},
		{"an advisory read grant", grantFacts(), "session",
			"the session cc/quiet-otter, working in example-ops, may read example-finance (read-only) until the session ends, at most 8h."},
		{"an advisory read grant, no session named", advisory, "1h",
			"the asking session, working in example-ops, may read example-finance (read-only) for 1 hour, until " + h1 + "."},
	} {
		require.Equal(t, tc.want, grantWords(tc.f, tc.duration, "8h0m0s", fixedNow), tc.name)
	}
}

// A denial is announced like an approval: what sudo's prompt is for.
func TestApprove_DenialAnnouncesSudo(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	rig := newApproveRig(t, routes(t, byRef(t, a)), "d\n")
	var atSudo string
	rig.runner.code = func(argv []string) (int, error) {
		if slices.Contains(argv, "--deny") {
			atSudo = rig.out.String()
			return 0, nil
		}
		return 1, nil
	}
	require.Equal(t, 0, rig.run("R-7F3K", "--for", "1h"), rig.errb.String())
	require.True(t, strings.HasSuffix(atSudo, "Approve R-7F3K? [y/N/d=deny] sudo asks for your Touch ID or password next: "+
		"it commits the denial as _seamless.\n"), atSudo)
}

// --passkey --for opens the passkey page with that duration chosen (the
// page's for= query); --for is checked against the daemon's durations first.
func TestApprove_PasskeyCarriesTheDuration(t *testing.T) {
	a := factsOf(reqA, "R-7F3K", "example-finance")
	page := "https://seamless.example.org/console/access/" + reqA
	table := byRef(t, a)
	for _, tc := range []struct {
		duration, words string
	}{
		{"session", "until the session ends, at most 8h"},
		{"1h", "for 1 hour"},
		{"4h", "for 4 hours"},
	} {
		rig := newApproveRig(t, routes(t, table), "")
		require.Equal(t, 0, rig.run("--passkey", "--for", tc.duration, "R-7F3K"), rig.errb.String())
		require.Equal(t, [][]string{{"open", page + "?for=" + tc.duration}}, rig.runner.recorded(), tc.duration)
		require.Equal(t, "Opened "+page+"?for="+tc.duration+" in the browser: decide R-7F3K there with a passkey "+
			"(approving there grants it "+tc.words+").\n", rig.out.String())
	}

	rig := newApproveRig(t, routes(t, table), "")
	rig.runner.code = func([]string) (int, error) { return -1, errors.New("no browser") }
	require.Equal(t, 0, rig.run("--passkey", "--for", "1h", reqA))
	require.Equal(t, "Could not open a browser. Open this page to decide R-7F3K with a passkey (approving there grants it "+
		"for 1 hour):\n  "+page+"?for=1h\n", rig.out.String())

	for _, bad := range []string{"2h", ""} {
		rig = newApproveRig(t, routes(t, table), "")
		require.Equal(t, 1, rig.run("--passkey", "--for="+bad, reqA))
		require.Contains(t, rig.errb.String(), fmt.Sprintf("invalid value %q for --for: valid values are session, 1h, 4h", bad))
		require.Empty(t, rig.runner.recorded(), "%q: nothing opened", bad)
	}

	// A page that carries a query already keeps it.
	rig = newApproveRig(t, routes(t, byRef(t, a, func(o *accessOne) { o.Page = page + "?origin=1" })), "")
	require.Equal(t, 0, rig.run("--passkey", "--for", "4h", reqA))
	require.Equal(t, [][]string{{"open", page + "?for=4h&origin=1"}}, rig.runner.recorded())
}

// Who a notification names: the daemon's agent name without its version, and
// nothing that is not letters.
func TestAgentFamily(t *testing.T) {
	for _, tc := range []struct {
		agent string
		pid   int32
		want  string
	}{
		{"Claude Code 2.1.319", 4242, "Claude Code"},
		{"Claude Code", 4242, "Claude Code"},
		{"Codex 0.46.0", 4242, "Codex"},
		{"Claude app", 4242, "Claude app"},
		{"", 4242, "An agent"},
		{"", 0, "A session"},
		{"Claude\x1b[2J Code", 4242, "An agent"},
		{"One Two Three Four", 4242, "One Two Three"},
		{"Ünï Code", 4242, "An agent"},
	} {
		require.Equal(t, tc.want, agentFamily(accessFacts{Agent: tc.agent, PID: tc.pid}), "%q", tc.agent)
	}
}

// A fixed length the cap reaches is offered once, as the cap: a longer one
// would grant the same.
func TestOfferedDurations(t *testing.T) {
	all := []string{"session", "1h", "4h"}
	require.Equal(t, []string{"session", "1h"}, offeredDurations(all, "20m0s"))
	require.Equal(t, []string{"session", "1h"}, offeredDurations(all, "1h0m0s"))
	require.Equal(t, all, offeredDurations(all, "2h0m0s"))
	require.Equal(t, all, offeredDurations(all, "8h0m0s"))
	require.Equal(t, all, offeredDurations(all, ""), "no cap known: every duration")
}
