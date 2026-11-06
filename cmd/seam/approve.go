package main

// seam approve -- the owner's terminal path for a locked project's access
// requests (access.go has the routes and seam access request).
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

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	statusPending = "pending"
	statusExpired = "expired"
)

var approveCmd = spec("approve", groupAccess,
	"decide an access request in the owner's own terminal",
	between(0, 1, "request"), bindApprove, runApprove).withLong(
	`The owner's terminal path for a locked project (peer_auth.locked). With no
request it lists the pending ones and picks one (asking which when there are
several); a request is its short id (R-7F3K) or its full id.

It shows the request's facts, warnings first, with the agent's reason quoted
and labeled as the agent's own words; asks for a duration (default: until the
asking session ends, capped by the daemon's max_lease) unless --for names one;
and asks to confirm: y approves, d denies, anything else changes nothing.

The decision runs as the daemon's account through sudo -k, so the owner's
Touch ID or password is asked for every time; the daemon's answer names the
account, its seamlessd and its config. Before that it warns when a sudoers
rule lets this account skip the authentication, which would make the approval
prove nothing.

It needs a terminal on stdin and stdout: an agent cannot use it. --passkey
opens the request's passkey page in the browser instead; --watch polls for new
requests every 3 seconds, prints each once and raises a notification.`)

type approveOpts struct {
	duration *presentString
	passkey  *bool
	watch    *bool
}

func bindApprove(fs *flag.FlagSet) *approveOpts {
	o := &approveOpts{
		duration: &presentString{},
		passkey:  fs.Bool("passkey", false, "open the request's passkey page in the browser instead"),
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

// errNotTerminal is seam approve run without the owner's terminal.
var errNotTerminal = errors.New("seam approve is the owner's own terminal path: it needs a terminal on stdin and stdout, " +
	"and an agent cannot use it\n" + hintIndent +
	"(an agent asks with: seam access request <project>; the owner decides here, or on the console's passkey page)")

func runApprove(ctx context.Context, e *env, o *approveOpts, pos []string) error {
	if !e.isTerminal() {
		return errNotTerminal
	}
	switch {
	case *o.watch && (len(pos) > 0 || *o.passkey || o.duration.set):
		return errors.New("--watch only watches: it takes no request, --for or --passkey")
	case *o.passkey && o.duration.set:
		return errors.New("the duration is chosen on the passkey page: drop --for, or drop --passkey")
	}
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	if *o.watch {
		return watchAccess(ctx, e, cfg)
	}

	in := bufio.NewReader(e.stdin)
	ref := ""
	if len(pos) == 1 {
		ref = pos[0]
	} else if ref, err = pickRequest(e, cfg, in); err != nil || ref == "" {
		return err
	}
	var one accessOne
	if err := accessGet(cfg, accessOnePath(ref), &one); err != nil {
		return err
	}
	f := one.Request
	if err := decidable(e, one); err != nil {
		return err
	}
	if *o.passkey {
		return openPasskeyPage(ctx, e, one)
	}
	duration := o.duration.val
	if o.duration.set && !slices.Contains(one.Durations, duration) {
		return fmt.Errorf("invalid value %q for --for: valid values are %s", duration, strings.Join(one.Durations, ", "))
	}

	renderFacts(e, f)
	t, err := terminalOf(one)
	if err != nil {
		return err
	}
	if !o.duration.set {
		var ok bool
		if duration, ok, err = askDuration(e, in, one); err != nil {
			return err
		} else if !ok {
			fmt.Fprintln(e.stdout, "Nothing changed.")
			return nil
		}
	}

	answer, _ := prompt(e, in, fmt.Sprintf("Approve %s (%s, %s, %s)? [y/N/d=deny] ",
		printable(f.Short), printable(f.Project), printable(orDash(f.Access)), durationWords(duration, one.MaxLease)))
	var argv []string
	what := ""
	switch strings.ToLower(answer) {
	case "y", "yes":
		if !durationArg.MatchString(duration) {
			return fmt.Errorf("the daemon offered %q as a duration, which seamlessd approve could not take", duration)
		}
		argv, what = approveArgv(t, f, duration), "approval"
	case "d", "deny":
		argv, what = denyArgv(t, f), "denial"
	default:
		fmt.Fprintln(e.stdout, "Nothing changed.")
		return nil
	}
	if err := warnPasswordless(ctx, e, t); err != nil {
		return err
	}
	// The terminal's own stdio, so sudo's Touch ID or password prompt shows and
	// seamlessd's answer ("Approved R-7F3K: ...") reaches the owner as it said it.
	code, err := e.run(ctx, argv, e.stdin, e.stdout, e.stderr)
	if err != nil {
		return fmt.Errorf("run sudo: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("the %s of %s did not complete (sudo exited with status %d)", what, printable(f.Short), code)
	}
	return nil
}

// pickRequest lists the pending requests and returns the id of the one to
// decide: the only one, or the one the owner names. "" (with no error) is
// nothing to decide, already said.
func pickRequest(e *env, cfg config.Config, in *bufio.Reader) (string, error) {
	var list accessList
	if err := accessGet(cfg, accessListPath, &list); err != nil {
		return "", err
	}
	reqs := list.Requests
	if len(reqs) == 0 {
		fmt.Fprintln(e.stdout, "Nothing is waiting.")
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

// decidable refuses a request that is no longer waiting for a decision: the
// daemon's status, or a pending one whose wait has run out.
func decidable(e *env, one accessOne) error {
	short := printable(one.Request.Short)
	status := one.Status
	if status == statusPending && !one.Request.ExpiresAt.IsZero() && !e.clock().Before(one.Request.ExpiresAt) {
		status = statusExpired
	}
	switch status {
	case statusPending:
		return nil
	case statusExpired:
		return fmt.Errorf("%s expired: nothing to decide (the agent can ask again)", short)
	default:
		return fmt.Errorf("%s is already %s: nothing to decide", short, printable(status))
	}
}

// factLabelWidth aligns the facts' values.
const factLabelWidth = 16

// renderFacts prints what the owner decides on: the warnings first, then the
// facts. Every value is the daemon's except the reason, which is quoted and
// labeled as the agent's own words. Every value is also made printable: a
// program name, a cwd and above all the reason are the agent's to choose, and
// a terminal escape among them could repaint the lines above it.
func renderFacts(e *env, f accessFacts) {
	color := useColor(e)
	fmt.Fprintf(e.stdout, "\nAccess request %s\n\n", printable(f.Short))
	for _, w := range f.Warnings {
		line := "! " + printable(w.Text)
		if color {
			line = "\x1b[1;31m" + line + "\x1b[0m"
		}
		fmt.Fprintln(e.stdout, line)
	}
	if len(f.Warnings) > 0 {
		fmt.Fprintln(e.stdout)
	}
	fact := func(label, value string) {
		fmt.Fprintf(e.stdout, "  %-*s%s\n", factLabelWidth, label, value)
	}
	fact("Project", fmt.Sprintf("%s (%s, %s)", printable(f.Project), printable(orDash(f.Isolation)), printable(orDash(f.Access))))
	asking := fmt.Sprintf("pid %d, uid %d", f.PID, f.UID)
	if f.Program != "" {
		asking = fmt.Sprintf("%s (%s)", printable(f.Program), asking)
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
	fmt.Fprintln(e.stdout)
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

// askDuration asks how long to approve for. ok is false when the owner gave
// no answer at all (end of input): nothing is decided.
func askDuration(e *env, in *bufio.Reader, one accessOne) (string, bool, error) {
	if len(one.Durations) == 0 {
		return "", false, errors.New("the daemon offered no durations to approve for")
	}
	hasDefault := slices.Contains(one.Durations, durationSession)
	fmt.Fprintln(e.stdout, "For how long?")
	for _, d := range one.Durations {
		fmt.Fprintf(e.stdout, "  %-8s %s\n", printable(d), durationWords(d, one.MaxLease))
	}
	q := "Duration: "
	if hasDefault {
		q = "Duration [" + durationSession + "]: "
	}
	for {
		ans, ok := prompt(e, in, q)
		if !ok {
			return "", false, nil
		}
		if ans == "" && hasDefault {
			return durationSession, true, nil
		}
		if slices.Contains(one.Durations, ans) {
			return ans, true, nil
		}
		fmt.Fprintf(e.stderr, "valid values are %s\n", strings.Join(one.Durations, ", "))
	}
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
func warnPasswordless(ctx context.Context, e *env, t terminalApprover) error {
	if _, err := e.run(ctx, []string{"sudo", "-k"}, nil, io.Discard, io.Discard); err != nil {
		return fmt.Errorf("run sudo: %w", err)
	}
	code, err := e.run(ctx, []string{"sudo", "-n", "-u", t.Account, "true"}, nil, io.Discard, io.Discard)
	if err != nil {
		return fmt.Errorf("run sudo: %w", err)
	}
	if code == 0 {
		line := "! warning: sudo runs commands as " + t.Account + " here without asking for authentication " +
			"(a NOPASSWD rule in sudoers). While that rule stands this approval proves nothing: " +
			"a process running as you could run it unasked. Remove the rule."
		if useColor(e) {
			line = "\x1b[1;31m" + line + "\x1b[0m"
		}
		fmt.Fprintln(e.stderr, line)
	}
	return nil
}

// --- --passkey ---

// openPasskeyPage opens the request's passkey page in the browser, or prints
// it when no browser could be opened.
func openPasskeyPage(ctx context.Context, e *env, one accessOne) error {
	short := printable(one.Request.Short)
	if one.Page == "" {
		return fmt.Errorf("%s has no passkey page: this daemon offers no passkey approval, so decide it here: seam approve %s", short, short)
	}
	u, err := url.Parse(one.Page)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("the daemon named %q as %s's passkey page, which is not a web address: not opening it", one.Page, short)
	}
	page := u.String()
	if code, err := e.run(ctx, openerArgv(e.platform(), page), nil, io.Discard, io.Discard); err != nil || code != 0 {
		fmt.Fprintf(e.stdout, "Could not open a browser. Open this page to decide %s with a passkey:\n  %s\n", short, page)
		return nil
	}
	fmt.Fprintf(e.stdout, "Opened %s in the browser: decide %s there with a passkey.\n", page, short)
	return nil
}

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
	color := useColor(e)
	for _, w := range r.Warnings {
		line := "  ! " + printable(w.Text)
		if color {
			line = "\x1b[1;31m" + line + "\x1b[0m"
		}
		fmt.Fprintln(e.stdout, line)
	}
	fmt.Fprintf(e.stdout, "  decide it with: seam approve %s\n", printable(r.Short))
	notify(ctx, e, r)
}

// notifyScript displays its first argument as a notification. The message is
// passed as an argument, never spliced into the script, so nothing in it can
// become AppleScript.
var notifyScript = []string{"-e", "on run argv", "-e", `display notification (item 1 of argv) with title "Seamless"`, "-e", "end run"}

// notify raises a desktop notification on macOS, and rings the terminal bell
// elsewhere or when that fails. Its text is the short id and the project slug
// only -- never the agent's reason, which is the agent's to write.
func notify(ctx context.Context, e *env, r accessFacts) {
	if e.platform() == "darwin" {
		msg := safeToken(r.Short) + " wants " + safeToken(r.Project)
		argv := append(append([]string{"osascript"}, notifyScript...), msg)
		if code, err := e.run(ctx, argv, nil, io.Discard, io.Discard); err == nil && code == 0 {
			return
		}
	}
	fmt.Fprint(e.stdout, "\a")
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
