package main

// seam approve -- the owner's terminal path for a locked project's access
// requests (access.go has the routes and seam access request), and for a
// confidential project's read grant requests (isolation.requests).
//
// Without peer_auth a read grant request is decided over the console with the
// bearer key instead of through sudo: the daemon marks it advisory, this
// command says so, and the y/N commits it as POST
// /console/access/<id>/approve with the digest of the facts shown. Any process
// holding the key could make the same call, which is why it is advisory.
//
// It shows a request's facts -- the daemon's, with the agent's reason quoted
// and labeled as the agent's -- asks for a duration and a confirmation, and then
// commits through
//
//	sudo -k -u <daemon account> env SEAMLESS_CONFIG=<config> <seamlessd> approve <id> --for <d> --digest <x>
//
// The sudo is the proof, not this command: -k asks for the owner's Touch ID or
// password every time and leaves no cached ticket behind, and only the daemon's
// account can open the database at all. That is also why a sudoers rule that
// skips authentication is warned about: while it stands, any process running as
// the owner could make the same call unasked.
//
// The terminal check below is a courtesy, not the boundary: it stops an agent's
// shell from wandering into prompts it cannot answer, and says where to go.
//
// What the daemon says becomes that command line, and server_url is in a file an
// agent running as the owner can edit, so the daemon is verified first
// (daemon_peer.go): nothing it says is shown as fact until the kernel has named
// who answered. The advisory path may stay unverified: it never runs sudo, and
// only posts back to the daemon that asked.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/config"
)

// watchInterval is how often seam approve --watch polls the pending list.
const watchInterval = 3 * time.Second

// durationSession is the default duration: until the asking session ends,
// capped by the daemon's max_lease.
const durationSession = "session"

// The request statuses the daemon reports.
const (
	statusPending  = "pending"
	statusExpired  = "expired"
	statusApproved = "approved"
	statusDenied   = "denied"
)

var approveCmd = spec("approve", groupAccess,
	"decide an access request in the owner's own terminal",
	between(0, 1, "request"), bindApprove, runApprove).withLong(
	`The owner's terminal path for a locked project. With no
request it lists the pending ones and picks one (asking which when there are
several); a request is its short id (R-7F3K) or its full id.

It shows the request's facts, warnings first, with the agent's reason quoted
and labeled as the agent's own words; asks for a duration (default: until the
asking session ends, capped by the daemon's max_lease) unless --for names one
(the word deny, typed in full there, denies the request without choosing one);
says in one line what an approval grants (the agent, the project, the access
and the duration); and asks to confirm: y approves, d denies, anything else
changes nothing.

The decision runs as the daemon's account through sudo -k, so the owner's
Touch ID or password is asked for every time; the daemon's answer names the
account, its seamlessd and its config. Before that it warns when a sudoers
rule lets this account skip the authentication, which would make the approval
prove nothing. Only warnings are colored, and not with NO_COLOR set.

A read grant request (isolation.requests: an agent asking to read a
confidential project) is decided the same way. On a daemon without peer_auth
it is committed over the console with the bearer key instead of through sudo,
and seam approve says so: that approval is advisory, since any process holding
the key could make it.

Before anything is shown it asks the kernel who answers on server_url's socket,
and refuses a socket served by your own account or by root, or by an account
other than the one the daemon names. A server_url that is not a socket says
nothing of who answered, so only an advisory request may go on: otherwise point
it at the socket, or use --passkey.

It needs a terminal on stdin and stdout: an agent cannot use it. --passkey
opens the request's passkey page in the browser instead, with --for's duration
chosen there when given; --watch polls for new requests every 3 seconds,
prints each once and raises a notification.`)

type approveOpts struct {
	duration *presentString
	passkey  *bool
	watch    *bool
}

func bindApprove(fs *flag.FlagSet) *approveOpts {
	o := &approveOpts{
		duration: &presentString{},
		passkey:  fs.Bool("passkey", false, "open the request's passkey page in the browser instead (with --for's duration)"),
		watch:    fs.Bool("watch", false, "watch for new requests: print each once and raise a notification"),
	}
	fs.Var(o.duration, "for", "approve for `DURATION`: session (until the session ends), 1h or 4h; asked when absent")
	return o
}

// presentString is a string flag that knows whether it was given, so that a
// present but empty --for is refused like any other value the daemon does not
// offer, rather than read as absent and answered with the question.
type presentString struct {
	val string
	set bool
}

func (p *presentString) String() string {
	if p == nil {
		return ""
	}
	return p.val
}

func (p *presentString) Set(v string) error {
	p.val, p.set = v, true
	return nil
}

// errNotTerminal is seam approve run without the owner's terminal: one line,
// naming the ways that remain for an agent and for the owner. It is said
// before any config is read, so it names the ways in general.
var errNotTerminal = errors.New("seam approve is the owner's own terminal path: it needs a terminal on stdin and stdout, " +
	"so an agent cannot use it. An agent asks with `seam access request <project>`; the owner decides in their own " +
	"terminal, or with Touch ID or a passkey where one is enrolled")

func runApprove(ctx context.Context, e *env, o *approveOpts, pos []string) error {
	if !e.isTerminal() {
		return errNotTerminal
	}
	if *o.watch && (len(pos) > 0 || *o.passkey || o.duration.set) {
		return errors.New("--watch only watches: it takes no request, --for or --passkey")
	}
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	if *o.watch {
		return watchAccess(ctx, e, cfg)
	}

	// Who answers, before anything it says is read. A passkey run only opens a
	// page and builds no sudo command line. A TCP server_url cannot be verified,
	// which only an advisory request survives: unverified holds the refusal
	// until the daemon has said which this is, and nothing is shown before then.
	var daemon verifiedDaemon
	var unverified error
	if !*o.passkey {
		daemon, err = verifyDaemon(ctx, e, cfg)
		if errors.Is(err, errServerNotSocket) {
			unverified, err = err, nil
		}
		if err != nil {
			return err
		}
	}

	in := bufio.NewReader(e.stdin)
	ref := ""
	if len(pos) == 1 {
		ref = pos[0]
	} else if ref, err = pickRequest(e, cfg, daemon.client, unverified, in); err != nil || ref == "" {
		return err
	}
	var one accessOne
	if err := accessGetVia(daemon.client, cfg, accessOnePath(ref), &one); err != nil {
		return err
	}
	var t terminalApprover
	if !*o.passkey && !one.Advisory {
		if unverified != nil {
			return unverified
		}
		if t, err = daemon.terminal(e, one); err != nil {
			return err
		}
	}
	f := one.Request
	if err := decidable(e, one); err != nil {
		return err
	}
	duration := o.duration.val
	if o.duration.set && !slices.Contains(one.Durations, duration) {
		return fmt.Errorf("invalid value %q for --for: valid values are %s", duration, strings.Join(one.Durations, ", "))
	}
	if *o.passkey {
		return openPasskeyPage(ctx, e, one, duration)
	}

	renderFacts(e, f)
	if one.Advisory {
		warnAdvisory(e)
	}
	answer := ""
	if !o.duration.set {
		d, got, err := askDuration(e, in, one)
		switch {
		case err != nil:
			return err
		case got == durationNoAnswer:
			fmt.Fprintln(e.stdout, "Nothing changed.")
			return nil
		case got == durationDenied:
			// The picker's own word: the same decision as d at the question
			// below, and the same denial commit, without choosing a duration.
			answer = "deny"
		default:
			duration = d
		}
	}

	// What y grants, said before anything runs: the agent, the project, the
	// access and the duration. sudo's prompt follows only after the answer.
	if answer == "" {
		fmt.Fprintln(e.stdout, "If approved: "+grantWords(f, duration, one.MaxLease, e.clock()))
		answer, _ = prompt(e, in, "Approve "+printable(f.Short)+"? [y/N/d=deny] ")
	}
	if one.Advisory {
		return decideOverConsole(e, cfg, f, strings.ToLower(answer), duration)
	}
	var argv []string
	what, done := "", ""
	switch strings.ToLower(answer) {
	case "y", "yes":
		if !durationArg.MatchString(duration) {
			return fmt.Errorf("the daemon offered %q as a duration, which seamlessd approve could not take", duration)
		}
		argv, what, done = approveArgv(t, f, duration), "approval", "approved"
	case "d", "deny":
		argv, what, done = denyArgv(t, f), "denial", "denied"
	default:
		fmt.Fprintln(e.stdout, "Nothing changed.")
		return nil
	}
	passwordless, err := warnPasswordless(ctx, e, t)
	if err != nil {
		return err
	}
	if !passwordless {
		fmt.Fprintf(e.stdout, "sudo asks for your Touch ID or password next: it commits the %s as %s.\n", what, t.Account)
	}
	// The terminal's own stdio, so sudo's Touch ID or password prompt shows and
	// seamlessd's answer ("Approved R-7F3K: ...") reaches the owner as it said it.
	code, err := e.run(ctx, argv, e.stdin, e.stdout, e.stderr)
	if err != nil {
		return fmt.Errorf("run sudo: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("the %s of %s did not complete: nothing was %s (sudo exited with status %d)",
			what, printable(f.Short), done, code)
	}
	return nil
}

// grantWords is what approving f for duration grants, as one sentence: who
// (the agent and its pid; a grant's asking project), what access to which
// project, and until when -- for a fixed duration, the wall-clock end too,
// counted from now.
func grantWords(f accessFacts, duration, maxLease string, now time.Time) string {
	who := askerWords(f)
	if f.From != "" {
		who += ", working in " + printable(f.From) + ","
	}
	project := printable(f.Project)
	var reach string
	switch f.Access {
	case "read-write":
		reach = "read and write " + project
	case "read-only":
		reach = "read " + project + " (read-only)"
	default:
		reach = "reach " + project + " (" + printable(orDash(f.Access)) + ")"
	}
	s := who + " may " + reach + " " + durationWords(duration, maxLease)
	if length, ok := grantLength(duration, maxLease); ok {
		s += ", until " + clockTime(now.Add(length))
	}
	if f.Kind == "refresh" {
		s += ", in place of the lease it holds now"
	}
	return s + "."
}

// askerWords names who asks: the agent the daemon names and its pid, else
// the program's base name, else the pid alone; without peer_auth (no
// process known) the session.
func askerWords(f accessFacts) string {
	if f.PID == 0 {
		if s := printable(f.Session); s != "" {
			return "the session " + s
		}
		return "the asking session"
	}
	name := printable(f.Agent)
	if name == "" {
		name = programBase(f.Program)
	}
	if name == "" {
		return fmt.Sprintf("pid %d", f.PID)
	}
	return fmt.Sprintf("%s (pid %d)", name, f.PID)
}

// programBase is a program path's last element, printable: the daemon host's
// path, whatever this client runs on.
func programBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	return printable(p)
}

// warnAdvisory says what an approval without peer_auth is worth, before the
// owner is asked for it.
func warnAdvisory(e *env) {
	fmt.Fprintln(e.stdout, warningLine(e, "  ", "This approval is advisory: without peer_auth it is made with the bearer key, "+
		"and any process holding the key could make it."))
	fmt.Fprintln(e.stdout)
}

// warningLine is one warning as the owner reads it: indent, then "! " and the
// text, the two colored together when color is on (useColor) and the indent
// never.
func warningLine(e *env, indent, text string) string {
	line := "! " + text
	if useColor(e) {
		line = colorWarn + line + colorReset
	}
	return indent + line
}

// The warning color (bold red) and its reset: the only color seam approve
// prints.
const (
	colorWarn  = "\x1b[1;31m"
	colorReset = "\x1b[0m"
)

// accessDecided is the console's answer to an approval or a denial.
type accessDecided struct {
	Approved  string    `json:"approved"`
	Denied    string    `json:"denied"`
	Project   string    `json:"project"`
	From      string    `json:"from"`
	ExpiresAt time.Time `json:"expires_at"`
}

// decideOverConsole commits the owner's answer to an advisory read grant
// request with the bearer key: y approves it for duration over the digest of
// the facts shown, d denies it, anything else changes nothing.
func decideOverConsole(e *env, cfg config.Config, f accessFacts, answer, duration string) error {
	short := printable(f.Short)
	var path string
	form := url.Values{}
	switch answer {
	case "y", "yes":
		path = "/console/access/" + url.PathEscape(f.ID) + "/approve?format=json"
		form.Set("duration", duration)
		form.Set("digest", f.Digest)
	case "d", "deny":
		path = "/console/access/" + url.PathEscape(f.ID) + "/deny?format=json"
	default:
		fmt.Fprintln(e.stdout, "Nothing changed.")
		return nil
	}
	b, err := accessCall(cfg, http.MethodPost, path, form)
	if err != nil {
		return fmt.Errorf("the decision on %s did not complete: %w", short, err)
	}
	var got accessDecided
	if err := json.Unmarshal(b, &got); err != nil {
		return fmt.Errorf("unreadable console response from %s: %w", path, err)
	}
	if got.Denied != "" {
		fmt.Fprintf(e.stdout, "Denied %s: %s stays closed to that session.\n", short, printable(got.Project))
		return nil
	}
	fmt.Fprintf(e.stdout, "Approved %s: sessions working in %s may read %s (read-only) until %s.\n", short,
		printable(got.From), printable(got.Project), clockTime(got.ExpiresAt))
	return nil
}

// pickRequest lists the pending requests and returns the id of the one to
// decide: the only one, or the one the owner names. "" (with no error) is
// nothing to decide, already said. It reads through client (accessCallVia), and
// unverified, when set, is why this daemon's list may be shown only as the
// advisory tier's: it is the answer, before anything is printed, for any other.
func pickRequest(e *env, cfg config.Config, client *http.Client, unverified error, in *bufio.Reader) (string, error) {
	var list accessList
	if err := accessGetVia(client, cfg, accessListPath, &list); err != nil {
		return "", err
	}
	if unverified != nil && !list.Advisory {
		return "", unverified
	}
	reqs := list.Requests
	if len(reqs) == 0 {
		fmt.Fprintln(e.stdout, "Nothing is waiting: no access request is pending (seam approve --watch prints each new one).")
		return "", nil
	}
	fmt.Fprintln(e.stdout, "Waiting for the owner:")
	for i, r := range reqs {
		fmt.Fprintf(e.stdout, "  %d  %s\n", i+1, requestLine(r))
	}
	if len(reqs) == 1 {
		return reqs[0].ID, nil
	}
	for {
		ans, ok := prompt(e, in, fmt.Sprintf("Which one? [1-%d or its short id, empty for none] ", len(reqs)))
		if !ok || ans == "" {
			fmt.Fprintln(e.stdout, "Nothing changed.")
			return "", nil
		}
		if n, err := strconv.Atoi(ans); err == nil && n >= 1 && n <= len(reqs) {
			return reqs[n-1].ID, nil
		}
		var hits []accessFacts
		for _, r := range reqs {
			if strings.EqualFold(r.Short, ans) || r.ID == ans {
				hits = append(hits, r)
			}
		}
		switch len(hits) {
		case 1:
			return hits[0].ID, nil
		case 0:
			fmt.Fprintf(e.stderr, "%q is none of them: answer with its number or its short id\n", ans)
		default:
			fmt.Fprintf(e.stderr, "%q names %d of them: answer with its number\n", ans, len(hits))
		}
	}
}

// requestLine is one pending request in a list: short id, project, session,
// warning count and when it stops waiting.
func requestLine(r accessFacts) string {
	session := printable(r.Session)
	if session == "" {
		session = "no session"
	}
	return fmt.Sprintf("%-8s %-24s %-24s %-12s waits until %s",
		printable(r.Short), printable(r.Project), session, countWarnings(len(r.Warnings)), clockTime(r.ExpiresAt))
}

func countWarnings(n int) string {
	switch n {
	case 0:
		return "no warnings"
	case 1:
		return "1 warning"
	default:
		return fmt.Sprintf("%d warnings", n)
	}
}

// clockTime renders a time as the owner's wall clock.
func clockTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("15:04")
}

// decidable refuses a request that is no longer waiting for a decision, in
// one line that says what became of it: the daemon's status, or a pending
// one whose wait has run out.
func decidable(e *env, one accessOne) error {
	f := one.Request
	short, project := printable(f.Short), printable(f.Project)
	status := one.Status
	if status == statusPending && !f.ExpiresAt.IsZero() && !e.clock().Before(f.ExpiresAt) {
		status = statusExpired
	}
	switch status {
	case statusPending:
		return nil
	case statusExpired:
		return fmt.Errorf("%s expired at %s without a decision: nothing to decide (the agent asks again with its next call to %s)",
			short, clockTime(f.ExpiresAt), project)
	case statusDenied:
		return fmt.Errorf("%s was denied: nothing to decide (%s stays %s that session until the agent asks again)",
			short, project, closedWord(f.Kind))
	case statusApproved:
		return fmt.Errorf("%s is already approved: nothing to decide", short)
	default:
		return fmt.Errorf("%s is %s, not pending: nothing to decide", short, printable(status))
	}
}

// closedWord is what a denial leaves a project: a lock's stays locked for the
// session, a confidential project's fence (a read grant request) closed to it.
func closedWord(kind string) string {
	if kind == kindGrant {
		return "closed to"
	}
	return "locked for"
}

// selfReportedNote says which facts are the agent's own report
// (approval.SelfReportedNote, pinned by a test: this CLI must not import
// internal/approval).
const selfReportedNote = "The working directory, repository and model are what the agent reported. The daemon cannot check them."

// factLabelWidth is the facts' label column, label and gap together: seam
// proposals prints its facts in the same column. A label that would leave
// less than factGap before its value widens the column for every row.
const (
	factLabelWidth = 16
	factGap        = 2
)

// renderFacts prints what the owner decides on: the warnings first, then the
// facts, values aligned past the widest label. Every value is the daemon's
// except the reason, which is quoted and labeled as the agent's own words.
// Every value is also made printable: a program name, a cwd and above all the
// reason are the agent's to choose, and a terminal escape among them could
// repaint the lines above it. Only the warnings are colored (useColor). The
// working directory and the model are the agent's own report too, as the
// closing note says.
func renderFacts(e *env, f accessFacts) {
	fmt.Fprintf(e.stdout, "\nAccess request %s\n\n", printable(f.Short))
	for _, w := range f.Warnings {
		fmt.Fprintln(e.stdout, warningLine(e, "  ", printable(w.Text)))
	}
	if len(f.Warnings) > 0 {
		fmt.Fprintln(e.stdout)
	}
	var rows [][2]string
	fact := func(label, value string) { rows = append(rows, [2]string{label, value}) }
	fact("Project", fmt.Sprintf("%s (%s, %s)", printable(f.Project), printable(orDash(f.Isolation)), printable(orDash(f.Access))))
	if f.From != "" {
		fact("For", "sessions working in "+printable(f.From))
	}
	if f.Agent != "" {
		fact("Agent", printable(f.Agent))
	}
	asking := fmt.Sprintf("pid %d, uid %d", f.PID, f.UID)
	if f.Program != "" {
		asking = fmt.Sprintf("%s (%s)", printable(f.Program), asking)
	}
	if f.PID == 0 {
		asking = "a session (without peer_auth the daemon knows no process)"
	}
	fact("Asking", asking)
	session := printable(f.Session)
	if session == "" && f.SessionID != "" {
		session = printable(shortID(f.SessionID))
	}
	if session == "" {
		session = "none"
	}
	if f.Model != "" {
		session += " (" + printable(f.Model) + ")"
	}
	fact("Session", session)
	fact("Working in", orUnknown(printable(f.CWD)))
	says := "(no reason given)"
	if f.Reason != "" {
		says = strconv.Quote(f.Reason) + "  (the agent's own words, not checked)"
	}
	fact("The agent says", says)
	fact("Kind", kindWords(f.Kind))
	waits := clockTime(f.ExpiresAt)
	if left := f.ExpiresAt.Sub(e.clock()); !f.ExpiresAt.IsZero() && left > 0 {
		waits += " (" + compactDuration(left.Round(time.Minute)) + " left)"
	}
	fact("Waits until", waits)
	for _, line := range alignFacts(rows) {
		fmt.Fprintln(e.stdout, line)
	}
	fmt.Fprintln(e.stdout)
	fmt.Fprintln(e.stdout, selfReportedNote)
	fmt.Fprintln(e.stdout)
}

// alignFacts lays out label/value rows, indented two spaces, every value in
// one column: factLabelWidth, or wider when a label would leave less than
// factGap before its value.
func alignFacts(rows [][2]string) []string {
	width := factLabelWidth
	for _, r := range rows {
		width = max(width, len(r[0])+factGap)
	}
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = fmt.Sprintf("  %-*s%s", width, r[0], r[1])
	}
	return lines
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// kindWords names a request's kind for the owner.
func kindWords(kind string) string {
	switch kind {
	case "request":
		return "first request"
	case "refresh":
		return "refresh of a lease this agent already holds"
	case kindGrant:
		return "a read-only grant of a confidential project"
	default:
		return orDash(printable(kind))
	}
}

// useColor reports whether warnings may be colored: a terminal, no NO_COLOR
// (https://no-color.org: any non-empty value) and not a dumb terminal.
func useColor(e *env) bool {
	return e.isTerminal() && e.envVar("NO_COLOR") == "" && e.envVar("TERM") != "dumb"
}

// printable returns s as it may be written to the owner's terminal: unchanged
// when every rune prints, else with the non-printing ones escaped (Go quoting,
// without the quotes) -- a control sequence, a bidi override or an invalid byte
// shows as what it is instead of acting on the terminal.
func printable(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return s
	}
	q := strconv.Quote(s)
	return q[1 : len(q)-1]
}

// durationAnswer is how the owner answered the duration question.
type durationAnswer int

const (
	// durationNoAnswer: end of input, nothing is decided.
	durationNoAnswer durationAnswer = iota
	// durationChosen: a duration was named.
	durationChosen
	// durationDenied: the word deny, spelled out, instead of a duration.
	durationDenied
)

// denyWord denies a request from the duration question, in any case and only
// in full: it is the one answer that is not a duration, so nothing typed for
// a duration can turn into a denial.
const denyWord = "deny"

// askDuration asks how long to approve for: the daemon's durations, listed
// by name, each with what it grants. The answer is a duration's name or the
// start of one, in any case ("1" is 1h, "s" is session), and an empty one is
// the default (until the session ends) when the daemon offers it. The list
// is not numbered: a number that picked the first line would read as hours
// ("1" for 1h granting the session). denyWord, in full, denies instead (a
// duration match wins first, so a duration can never read as a denial); no
// other input decides anything, and an approval still waits for the y/N
// question. durationNoAnswer is the owner giving no answer at all (end of
// input): nothing is decided.
func askDuration(e *env, in *bufio.Reader, one accessOne) (string, durationAnswer, error) {
	ds := offeredDurations(one.Durations, one.MaxLease)
	if len(ds) == 0 {
		return "", durationNoAnswer, errors.New("the daemon offered no durations to approve for")
	}
	hasDefault := slices.Contains(ds, durationSession)
	width := 0
	names := make([]string, len(ds))
	for i, d := range ds {
		names[i] = printable(d)
		width = max(width, len(names[i]))
	}
	fmt.Fprintln(e.stdout, "For how long?")
	for i, d := range ds {
		words := durationWords(d, one.MaxLease)
		if d == durationSession {
			words += " (the default)"
		}
		fmt.Fprintf(e.stdout, "  %-*s  %s\n", width, names[i], words)
	}
	listed := strings.Join(names, ", ")
	q := "Duration [" + listed + "; deny to deny it]: "
	if hasDefault {
		q = "Duration [" + listed + "; Enter for " + durationSession + "; deny to deny it]: "
	}
	for {
		ans, ok := prompt(e, in, q)
		if !ok {
			return "", durationNoAnswer, nil
		}
		if ans == "" && hasDefault {
			return durationSession, durationChosen, nil
		}
		d, matches := matchDuration(ds, ans)
		switch {
		case matches == 1:
			return d, durationChosen, nil
		case matches > 1:
			fmt.Fprintf(e.stderr, "%q starts more than one of %s: type more of it\n", ans, listed)
		case strings.EqualFold(ans, denyWord):
			return "", durationDenied, nil
		default:
			fmt.Fprintf(e.stderr, "answer with one of %s, or the start of one (or %s, in full, to deny it)\n", listed, denyWord)
		}
	}
}

// offeredDurations are the daemon's durations as max_lease grants them:
// each one up to the first fixed length the cap reaches, which grants the
// cap. A longer one would grant the same, so it is not offered twice (the
// console's choices and the approval dialog's are the same).
func offeredDurations(ds []string, maxLease string) []string {
	limit, err := time.ParseDuration(maxLease)
	capped := err == nil && limit > 0
	var out []string
	for _, d := range ds {
		out = append(out, d)
		if v, err := time.ParseDuration(d); d != durationSession && err == nil && capped && v >= limit {
			break
		}
	}
	return out
}

// matchDuration finds the duration ans names: a name exactly, else the one
// name ans starts, in any case. matches is how many it could be; d is set
// only when that is one.
func matchDuration(ds []string, ans string) (d string, matches int) {
	if ans == "" {
		return "", 0
	}
	for _, c := range ds {
		if strings.EqualFold(c, ans) {
			return c, 1
		}
	}
	for _, c := range ds {
		if len(c) >= len(ans) && strings.EqualFold(c[:len(ans)], ans) {
			d = c
			matches++
		}
	}
	if matches != 1 {
		d = ""
	}
	return d, matches
}

// durationWords says what a duration choice grants, capped by max_lease.
func durationWords(choice, maxLease string) string {
	maxD, maxErr := time.ParseDuration(maxLease)
	if choice == durationSession {
		if maxErr == nil && maxD > 0 {
			return "until the session ends, at most " + compactDuration(maxD)
		}
		return "until the session ends"
	}
	d, err := time.ParseDuration(choice)
	switch {
	case err != nil:
		return "for " + printable(choice)
	case maxErr == nil && maxD > 0 && d > maxD:
		return "for " + compactDuration(maxD) + " (the max_lease cap)"
	case d == time.Hour:
		return "for 1 hour"
	case d%time.Hour == 0:
		return fmt.Sprintf("for %d hours", int(d.Hours()))
	default:
		return "for " + compactDuration(d)
	}
}

// grantLength is how long a fixed duration choice grants, capped by
// max_lease; ok is false for the session (its end is not a clock time) and
// for a choice that is not a duration.
func grantLength(choice, maxLease string) (time.Duration, bool) {
	if choice == durationSession {
		return 0, false
	}
	d, err := time.ParseDuration(choice)
	if err != nil || d <= 0 {
		return 0, false
	}
	if maxD, err := time.ParseDuration(maxLease); err == nil && maxD > 0 && d > maxD {
		d = maxD
	}
	return d, true
}

// compactDuration renders 8h0m0s as 8h and 1h30m0s as 1h30m.
func compactDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// prompt asks q and reads one answer line. ok is false at end of input with
// nothing typed.
func prompt(e *env, in *bufio.Reader, q string) (string, bool) {
	fmt.Fprint(e.stdout, q)
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(e.stdout)
		return "", false
	}
	return strings.TrimSpace(line), true
}

// The shapes the sudo argv's daemon-supplied parts must have. The daemon is the
// owner's own, but these become a program's arguments: an id or a duration that
// began with "-" would be read by seamlessd as a flag.
var (
	accountName = regexp.MustCompile(`^#?[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	requestID   = regexp.MustCompile(`^[0-9A-Za-z]+$`)
	hexDigest   = regexp.MustCompile(`^[0-9a-f]+$`)
	durationArg = regexp.MustCompile(`^[0-9A-Za-z]+$`)
)

// terminalOf returns how to reach the terminal path's commit, or says what the
// daemon left out and where the owner can approve instead.
func terminalOf(one accessOne) (terminalApprover, error) {
	t := one.Terminal
	missing := ""
	switch {
	case t == nil:
		missing = "it named no terminal approver"
	case t.Account == "":
		missing = "it does not know its own account"
	case t.Seamlessd == "":
		missing = "it does not know its own executable"
	case t.Config == "":
		missing = "it does not know its config file"
	}
	if missing != "" {
		return terminalApprover{}, fmt.Errorf("the daemon did not say how to reach the terminal path (%s): approve %s on the console's passkey page instead: %s",
			missing, printable(one.Request.Short), passkeyPlace(one))
	}
	switch {
	case !accountName.MatchString(t.Account):
		return terminalApprover{}, fmt.Errorf("the daemon named %q as its account, which is not an account name", t.Account)
	case isRootAccount(t.Account):
		return terminalApprover{}, errRootAccount(t.Account)
	case !absPath(t.Seamlessd):
		return terminalApprover{}, fmt.Errorf("the daemon named %q as its seamlessd, which is not an absolute path", t.Seamlessd)
	case !absPath(t.Config):
		return terminalApprover{}, fmt.Errorf("the daemon named %q as its config, which is not an absolute path", t.Config)
	case !requestID.MatchString(one.Request.ID):
		return terminalApprover{}, fmt.Errorf("the daemon named %q as the request's id, which is not an id", one.Request.ID)
	case !hexDigest.MatchString(one.Request.Digest):
		return terminalApprover{}, fmt.Errorf("the daemon sent no usable digest of %s's facts", printable(one.Request.Short))
	}
	return *t, nil
}

// absPath accepts the daemon host's absolute paths, which are POSIX under
// peer_auth whatever this client runs on.
func absPath(p string) bool {
	return strings.HasPrefix(p, "/") || filepath.IsAbs(p)
}

// passkeyPlace is where the owner approves a request with a passkey: the
// page the daemon named, or the console path to open on the passkey origin.
func passkeyPlace(one accessOne) string {
	if one.Page != "" {
		return printable(one.Page)
	}
	return "/console/access/" + url.PathEscape(one.Request.ID) + " (open it on the passkey origin)"
}

// sudoArgv is the terminal commit's command line: seamlessd approve, run as
// the daemon's account with the config the daemon loaded. -k makes sudo ask
// every time and keep no ticket an agent could reuse.
func sudoArgv(t terminalApprover, tail ...string) []string {
	argv := []string{"sudo", "-k", "-u", t.Account, "env", "SEAMLESS_CONFIG=" + t.Config, t.Seamlessd, "approve"}
	return append(argv, tail...)
}

// approveArgv approves f for duration over the digest of the facts shown.
func approveArgv(t terminalApprover, f accessFacts, duration string) []string {
	return sudoArgv(t, f.ID, "--for", duration, "--digest", f.Digest)
}

// denyArgv denies f.
func denyArgv(t terminalApprover, f accessFacts) []string {
	return sudoArgv(t, f.ID, "--deny")
}

// warnPasswordless warns when sudo would run a command as the daemon's account
// without authenticating: with the cached ticket dropped (sudo -k), a
// non-interactive sudo -n succeeds only under a rule that asks for nothing.
// While one stands the terminal approval proves nothing, so the owner is told;
// the approval still goes ahead, since refusing would not remove the rule.
// passwordless reports that rule, so the caller does not also announce a
// prompt sudo will not show.
func warnPasswordless(ctx context.Context, e *env, t terminalApprover) (passwordless bool, err error) {
	if _, err := e.run(ctx, []string{"sudo", "-k"}, nil, io.Discard, io.Discard); err != nil {
		return false, fmt.Errorf("run sudo: %w", err)
	}
	code, err := e.run(ctx, []string{"sudo", "-n", "-u", t.Account, "true"}, nil, io.Discard, io.Discard)
	if err != nil {
		return false, fmt.Errorf("run sudo: %w", err)
	}
	if code != 0 {
		return false, nil
	}
	fmt.Fprintln(e.stderr, warningLine(e, "", "warning: sudo runs commands as "+t.Account+" here without asking for "+
		"authentication (a NOPASSWD rule in sudoers). While that rule stands this approval proves nothing: "+
		"a process running as you could run it unasked. Remove the rule."))
	return true, nil
}

// --- --passkey ---

// openPasskeyPage opens the request's passkey page in the browser, or prints
// it when no browser could be opened. duration, when given (--for, already
// checked against the daemon's durations), rides as the page's for= query:
// the page opens with it chosen, and the passkey's approval binds it.
func openPasskeyPage(ctx context.Context, e *env, one accessOne, duration string) error {
	short := printable(one.Request.Short)
	if one.Page == "" {
		return fmt.Errorf("%s has no passkey page: this daemon offers no passkey approval, so decide it here: seam approve %s", short, short)
	}
	u, err := url.Parse(one.Page)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("the daemon named %q as %s's passkey page, which is not a web address: not opening it", one.Page, short)
	}
	chosen := ""
	if duration != "" {
		q := u.Query()
		q.Set(passkeyDurationParam, duration)
		u.RawQuery = q.Encode()
		chosen = " (approving there grants it " + durationWords(duration, one.MaxLease) + ")"
	}
	page := u.String()
	if code, err := e.run(ctx, openerArgv(e.platform(), page), nil, io.Discard, io.Discard); err != nil || code != 0 {
		fmt.Fprintf(e.stdout, "Could not open a browser. Open this page to decide %s with a passkey%s:\n  %s\n", short, chosen, page)
		return nil
	}
	fmt.Fprintf(e.stdout, "Opened %s in the browser: decide %s there with a passkey%s.\n", page, short, chosen)
	return nil
}

// passkeyDurationParam is the passkey page's duration query
// (/console/access/<id>?for=<session|1h|4h>): the page opens with it chosen.
const passkeyDurationParam = "for"

// openerArgv is the platform's command to open a URL in the default browser.
func openerArgv(goos, page string) []string {
	switch goos {
	case "darwin":
		return []string{"open", page}
	case "windows":
		return []string{"rundll32", "url.dll,FileProtocolHandler", page}
	default:
		return []string{"xdg-open", page}
	}
}

// --- --watch ---

// watchAccess polls the pending list every watchInterval, prints each request
// it has not seen once, and raises a notification for it. It ends, cleanly, on
// an interrupt or when ctx ends. A first poll that fails is the answer (no
// project locked, no daemon); a later one is reported once per distinct
// failure and the watch goes on, so a daemon restart does not end it.
func watchAccess(ctx context.Context, e *env, cfg config.Config) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	tick, stopTick := e.ticker(watchInterval)
	defer stopTick()
	fmt.Fprintf(e.stdout, "Watching for access requests every %s (Ctrl-C to stop).\n", compactDuration(watchInterval))

	seen := map[string]bool{}
	first := true
	lastErr := ""
	for {
		var list accessList
		err := accessGet(cfg, accessListPath, &list)
		switch {
		case err != nil && first:
			return err
		case err != nil:
			if msg := err.Error(); msg != lastErr {
				fmt.Fprintf(e.stderr, "warning: %s (still watching)\n", msg)
				lastErr = msg
			}
		default:
			lastErr = ""
			// Only the ids still pending are kept: a request that left the list
			// never comes back to it, so this stays bounded.
			current := make(map[string]bool, len(list.Requests))
			for _, r := range list.Requests {
				current[r.ID] = true
				if !seen[r.ID] {
					announce(ctx, e, r)
				}
			}
			seen = current
		}
		first = false
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
		}
	}
}

// announce prints one new request and raises a notification for it.
func announce(ctx context.Context, e *env, r accessFacts) {
	fmt.Fprintln(e.stdout, requestLine(r))
	for _, w := range r.Warnings {
		fmt.Fprintln(e.stdout, warningLine(e, "  ", printable(w.Text)))
	}
	fmt.Fprintf(e.stdout, "  %s asks; decide it with: seam approve %s\n", agentFamily(r), printable(r.Short))
	notify(ctx, e, r)
}

// notifyScript displays its first argument as a notification. The message is
// passed as an argument, never spliced into the script, so nothing in it can
// become AppleScript.
var notifyScript = []string{"-e", "on run argv", "-e",
	`display notification (item 1 of argv) with title "Seamless access request"`, "-e", "end run"}

// notify raises a desktop notification on macOS, and rings the terminal bell
// elsewhere or when that fails. Its text is who asks (agentFamily), the
// project slug and the command that decides it -- never the agent's reason,
// which is the agent's to write.
func notify(ctx context.Context, e *env, r accessFacts) {
	if e.platform() == "darwin" {
		short := safeToken(r.Short)
		msg := agentFamily(r) + " asks for " + safeToken(r.Project) + ": seam approve " + short
		argv := append(append([]string{"osascript"}, notifyScript...), msg)
		if code, err := e.run(ctx, argv, nil, io.Discard, io.Discard); err == nil && code == 0 {
			return
		}
	}
	fmt.Fprint(e.stdout, "\a")
}

// agentFamily names who asks in a notification: the leading words of the
// daemon's agent name that are letters only ("Claude Code" of "Claude Code
// 2.1.319"), at most three, so no version or other text rides along; "An
// agent" when the daemon names none, "A session" without peer_auth (no
// process known).
func agentFamily(r accessFacts) string {
	var words []string
	for w := range strings.FieldsSeq(r.Agent) {
		if len(words) == 3 || strings.ContainsFunc(w, func(c rune) bool { return c >= utf8.RuneSelf || !unicode.IsLetter(c) }) {
			break
		}
		words = append(words, w)
	}
	switch {
	case len(words) > 0:
		return strings.Join(words, " ")
	case r.PID == 0:
		return "A session"
	default:
		return "An agent"
	}
}

// safeToken keeps the characters a short id or a project slug is made of.
func safeToken(s string) string {
	return strings.Map(func(r rune) rune {
		if r < utf8.RuneSelf && (r == '-' || r == '_' || r == '.' || unicode.IsLetter(r) || unicode.IsDigit(r)) {
			return r
		}
		return -1
	}, s)
}
