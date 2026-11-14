package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// consoleLoginTmpl is a one-shot, self-submitting login page. Opened from a
// local file:// URL, it POSTs the static key to the console's /console/login
// endpoint, which sets the HttpOnly session cookie and 303-redirects into the
// console -- so `make console` lands on an authenticated page with no paste.
// The cookie is SameSite=Lax; setting it on this cross-site POST is allowed,
// and the follow-up top-level GET to /console/ carries it. Rendered with
// html/template so the key is contextually escaped inside the attribute.
var consoleLoginTmpl = template.Must(template.New("console-login").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="robots" content="noindex">
<title>Signing in to Seamless...</title>
</head>
<body>
<p>Signing in to the Seamless console...</p>
<form id="login" method="post" action="{{.Base}}/console/login">
<input type="hidden" name="key" value="{{.Key}}">
<input type="hidden" name="next" value="/console/">
<noscript><button type="submit">Continue to console</button></noscript>
</form>
<script>document.getElementById("login").submit();</script>
</body>
</html>
`))

// runConsoleOpen renders a self-submitting console login page, writes it to a
// 0600 temp file, and opens it in a browser -- pre-authenticating the session
// so the console loads without a manual key paste. By default it opens the OS
// default browser; --browser <app> targets a specific one (e.g. "Google
// Chrome", so an agent driving Chrome gets the auth cookie even when another
// browser is the default). It refuses to run if the key is unset or the server
// is unreachable, since the page POSTs to it.
//
// Under passkey sign-in (console.sign_in: passkey) the key signs no browser
// in, so it opens the console's sign-in page instead and writes no page at
// all (openSignInPage): there is no local path that signs a browser in
// without the passkey, on the server or anywhere else. The way in without a
// passkey begins in the browser too: the sign-in page's terminal sign-in shows
// a code, which --sign-in --code approves as the daemon's account once it has
// shown the owner that browser and the owner said yes (runConsoleSignIn).
func runConsoleOpen(args []string) error {
	fs := flag.NewFlagSet("console-open", flag.ContinueOnError)
	browser := fs.String("browser", "", `browser application to open instead of the default (e.g. "Google Chrome"; macOS only; not with --unlock or --sign-in, which open no browser)`)
	unlock := fs.String("unlock", "", "approve a locked project's unlock in the browser that began it on its unlock page (run as the daemon's account, through sudo -k)")
	signIn := fs.Bool("sign-in", false, "approve the terminal sign-in a browser began on the console's sign-in page, under passkey sign-in, after it shows that browser and asks (at a terminal; run as the daemon's account, through sudo -k)")
	code := fs.String("code", "", "the code that browser's unlock or sign-in page shows (with --unlock or --sign-in; asked for when absent and stdin is a terminal)")
	forFlag := fs.String("for", "", "how long the unlock lasts once approved (with --unlock; at most and by default 3h, capped by peer_auth.max_lease)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("seamlessd.console-open: %w", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.console-open: %w", err)
	}
	if *signIn {
		if *unlock != "" || *forFlag != "" {
			return errors.New("seamlessd.console-open: --sign-in takes only --code: a sign-in lasts as long as every console sign-in, and opens no locked project")
		}
		if err := runConsoleSignIn(cfg, *code, *browser); err != nil {
			return fmt.Errorf("seamlessd.console-open: %w", err)
		}
		return nil
	}
	if *unlock != "" || *forFlag != "" || *code != "" {
		if *unlock == "" {
			return errors.New("seamlessd.console-open: --for and --code need --unlock <project>")
		}
		if err := runConsoleUnlock(cfg, *unlock, *code, *forFlag, *browser); err != nil {
			return fmt.Errorf("seamlessd.console-open: %w", err)
		}
		return nil
	}
	if cfg.SocketPath() != "" {
		return fmt.Errorf("seamlessd.console-open: %w", socketConsoleRefusal(cfg))
	}
	// Passkey sign-in: the key signs no browser in, so there is no page to
	// pre-sign. console-open opens the console's sign-in page and writes
	// nothing anywhere.
	if handled, err := openSignInPage(cfg, *browser, systemSignInOpen); handled {
		if err != nil {
			return fmt.Errorf("seamlessd.console-open: %w", err)
		}
		return nil
	}
	if strings.TrimSpace(cfg.MCP.APIKey) == "" {
		return fmt.Errorf("seamlessd.console-open: mcp.api_key is empty; run `seamlessd serve` once to generate it, or set it in seamless.yaml")
	}
	base := consoleOpenBase(cfg)
	if !serverReachable(urlHostPort(base)) {
		return fmt.Errorf("seamlessd.console-open: console not reachable at %s -- start it with `make run` or `make start`", base)
	}

	page, err := renderConsoleLoginPage(base, cfg.MCP.APIKey)
	if err != nil {
		return fmt.Errorf("seamlessd.console-open: %w", err)
	}
	// Anything an earlier run left behind (a crash, a kill between launch and
	// cleanup) is a copy of the bearer key sitting in a shared /tmp, so sweep
	// before adding another one.
	sweepStaleLoginPages()

	f, err := os.CreateTemp("", consoleLoginPattern)
	if err != nil {
		return fmt.Errorf("seamlessd.console-open: temp file: %w", err)
	}
	// CreateTemp makes the file 0600, so the embedded key stays owner-readable.
	if _, err := f.WriteString(page); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return fmt.Errorf("seamlessd.console-open: write login page: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("seamlessd.console-open: close login page: %w", err)
	}
	// From here the file exists on disk with the key in it, so every exit path
	// has to erase it -- including the browser-launch failure below.
	defer removeAfterBrowserRead(f.Name())

	if err := openInBrowser(f.Name(), *browser); err != nil {
		return fmt.Errorf("seamlessd.console-open: open browser: %w", err)
	}
	where := "default browser"
	if *browser != "" {
		where = *browser
	}
	fmt.Printf("opened pre-authenticated console at %s/console/ in %s\n", base, where)
	return nil
}

// signInOpen is what console-open's passkey path reads and does: whether the
// daemon signs browsers in with a passkey, whether its console answers, and
// how a browser is opened. Injected so a test opens no browser.
type signInOpen struct {
	signsIn   func(config.Config) bool
	reachable func(host string) bool
	open      func(target, app string) error
	out       io.Writer
}

// systemSignInOpen is the real machine's.
var systemSignInOpen = signInOpen{signsIn: daemonSignsInWithPasskey, reachable: serverReachable, open: openInBrowser, out: os.Stdout}

// openSignInPage is console-open under passkey sign-in (console.sign_in:
// passkey, in this config or in the daemon's answer): it opens the console's
// own sign-in page in the browser and says so. Nothing it writes, opens or
// prints is a credential -- no temp page, no key, no link that signs a
// browser in -- since any process of the owner's could read one, and a page
// that signs a browser in without the passkey would be one more way in
// (constraint console-unlock-begun-by-the-browser-it-opens). handled is false
// while the key signs browsers in: the pre-signed key page, as before.
func openSignInPage(cfg config.Config, browser string, d signInOpen) (handled bool, err error) {
	base := consoleOpenBase(cfg)
	page, ok := consoleSignInPage(cfg, base, d.signsIn)
	if !ok {
		return false, nil
	}
	if !d.reachable(urlHostPort(base)) {
		return true, fmt.Errorf("console not reachable at %s -- start it with `make run` or `make start`", base)
	}
	if err := d.open(page, browser); err != nil {
		return true, fmt.Errorf("open browser: %w", err)
	}
	where := "the default browser"
	if browser != "" {
		where = browser
	}
	fmt.Fprintf(d.out, "opened the console's sign-in page at %s in %s: sign in there with your passkey.\n", page, where)
	fmt.Fprintf(d.out, "A device with no passkey yet adds one at %s; approve the code it shows with `seamlessd approvers approve --code <code>` on the server.\n",
		passkeyAddAddress(cfg))
	return true, nil
}

// consoleLoginPattern names the temp login pages, and is also what the sweep
// matches -- one constant so the two cannot drift apart.
const consoleLoginPattern = "seamless-console-*.html"

// loginPageGrace is how long the page stays on disk after the browser is
// launched. The browser reads the file asynchronously (`open`/`xdg-open`
// returns as soon as it has handed the URL over), so deleting immediately would
// race a cold browser start. Long enough for one, short enough that the key is
// not sitting in /tmp for the rest of the session.
const loginPageGrace = 8 * time.Second

// removeAfterBrowserRead deletes the login page once the browser has had time to
// read it. It blocks, deliberately: this runs as the last thing `console-open`
// does, after its success line is printed, and a background goroutine would die
// with the process before it ever fired.
func removeAfterBrowserRead(path string) {
	// Not synchronization: nothing here is waiting on a goroutine or a channel.
	// This is a wall-clock grace period for an external process (the browser)
	// that gives us no completion signal at all -- `open`/`xdg-open` return
	// immediately and never report when the page was actually loaded.
	//nolint:forbidigo // wall-clock grace for an external process, not inter-goroutine sync
	time.Sleep(loginPageGrace)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "warning: could not remove %s (it contains your API key): %v\n", path, err)
	}
}

// sweepStaleLoginPages removes login pages left by earlier runs. Best-effort:
// on a shared /tmp another user's file of the same shape is not ours to delete,
// and the Remove simply fails, so errors are ignored. The age floor keeps a
// concurrent console-open's live page out of the sweep.
func sweepStaleLoginPages() {
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), consoleLoginPattern))
	if err != nil {
		return
	}
	for _, m := range matches {
		info, serr := os.Stat(m)
		if serr != nil || time.Since(info.ModTime()) < 2*loginPageGrace {
			continue
		}
		_ = os.Remove(m)
	}
}

// renderConsoleLoginPage returns the self-submitting login HTML posting key to
// the console at base (a config.ServerURL).
func renderConsoleLoginPage(base, key string) (string, error) {
	var b strings.Builder
	if err := consoleLoginTmpl.Execute(&b, struct{ Base, Key string }{Base: base, Key: key}); err != nil {
		return "", err
	}
	return b.String(), nil
}

// a2aEndpoint returns the usable discovery URL for the daemon's A2A endpoint.
//
// It takes the LOADED config, not a bind address: the card must carry the
// scheme tls.* decides and the host server_url names, and synthesizing a
// config.Config{Addr: bind} threw both away -- a TLS daemon advertised http://
// and a wildcard bind advertised loopback even when server_url said otherwise.
// `serve --addr` still wins, because runServe writes the override back onto
// cfg.Addr before anything derives a URL from it.
func a2aEndpoint(cfg config.Config) string {
	// The card is public (a2a.Server.CardHandler): a user in server_url
	// (config.Config.ServerURLNamesUser) never goes into it.
	return config.RedactedURL(cfg.ServerURL()) + "/api/a2a"
}

// urlHostPort is the authority (host:port) of a base URL, for the callers that
// still take a bare host rather than a URL. "" when base does not parse, which
// config.ServerURL cannot produce -- it builds the URL from its parts.
func urlHostPort(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Host
}

// serverReachable reports whether a daemon answers /healthz on host (host:port)
// within 2s. Any HTTP response (200 or a 503 degraded) counts; only a transport
// error is down.
//
// Plaintext first, then TLS: once tls.cert_file/tls.key_file are set the
// listener refuses http entirely, and a probe that only spoke http would report
// a perfectly healthy daemon as absent -- which is how `import` would decide it
// is safe to overwrite a live data directory. The TLS leg deliberately skips
// verification: this asks "is something serving here", nothing is read from the
// response and no credential is sent, and requiring a trusted chain would turn
// a self-signed LAN daemon (the documented setup) back into "down".
func serverReachable(host string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + host + "/healthz")
	if err == nil {
		_ = resp.Body.Close()
		return true
	}
	tlsClient := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			//nolint:gosec // liveness probe only: no body read, no credential sent
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12},
		},
	}
	resp, err = tlsClient.Get("https://" + host + "/healthz")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

// openInBrowser opens target (a file path or URL) in a browser: the OS default
// when app is empty, or the named browser application (macOS only).
func openInBrowser(target, app string) error {
	cmd, err := browserCommand(runtime.GOOS, target, app)
	if err != nil {
		return err
	}
	return cmd.Run()
}

// browserCommand builds the OS launch command for target. A non-empty app
// names a specific browser application; that is only expressible on macOS
// (`open -a`), so other platforms reject it rather than silently opening the
// default.
func browserCommand(goos, target, app string) (*exec.Cmd, error) {
	if app != "" && goos != "darwin" {
		return nil, fmt.Errorf("--browser is only supported on macOS (got GOOS=%s)", goos)
	}
	switch goos {
	case "darwin":
		if app != "" {
			return exec.Command("open", "-a", app, target), nil
		}
		return exec.Command("open", target), nil
	case "windows":
		return exec.Command("cmd", "/c", "start", "", target), nil
	default:
		return exec.Command("xdg-open", target), nil
	}
}

// consoleBase is where console-open signs in. Under peer_auth it is
// the passkey relying party's origin (localhost by default) rather than the
// server URL: the console cookie is per host, and a passkey ceremony on an IP
// address such as 127.0.0.1 is refused by every browser. Otherwise it is the
// server URL, as it always was.
func consoleBase(cfg config.Config) string {
	if cfg.PeerAuth.Enabled {
		if _, origins := cfg.RelyingParty(); len(origins) > 0 {
			return origins[0]
		}
	}
	// A browser opens this, and console-open prints it: never with a user.
	return config.RedactedURL(cfg.ServerURL())
}

// consoleOpenBase is where console-open signs in, for this config: consoleBase
// when the config is the daemon's own, else whatever the daemon it dials says
// its passkey origin is. A peer_auth owner's config is a client's -- its
// server_url and key, no peer_auth block -- so consoleBase alone would sign it
// in on 127.0.0.1, where passkeys cannot be created. A daemon that locks
// nothing answers no origin (the access routes are 404 without peer_auth), so a default install
// signs in exactly where it always did.
func consoleOpenBase(cfg config.Config) string {
	base := consoleBase(cfg)
	if cfg.PeerAuth.Enabled {
		return base
	}
	if origin := daemonOrigin(cfg); usableOrigin(origin, cfg) && serverReachable(urlHostPort(origin)) {
		return origin
	}
	return base
}

// daemonOrigin asks the daemon cfg dials for its passkey relying party's
// origin: the origin field of GET /console/access?format=json. "" means no
// answer: a daemon that locks nothing, one that predates the field, or any
// failure, for which the config's own URL stands. The request carries the
// console's key and, like every dial of a Seamless daemon, follows no
// redirect that could carry it elsewhere.
func daemonOrigin(cfg config.Config) string {
	client, err := cfg.HTTPClient(2 * time.Second)
	if err != nil {
		return ""
	}
	client.CheckRedirect = config.RefuseRedirect
	req, err := http.NewRequest(http.MethodGet, cfg.ServerURL()+"/console/access?format=json", nil)
	if err != nil {
		return ""
	}
	if key := strings.TrimSpace(cfg.MCP.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var got struct {
		Origin string `json:"origin"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&got); err != nil {
		return ""
	}
	return got.Origin
}

// usableOrigin reports whether the daemon's answer may be where the browser
// signs in: scheme://host[:port] and nothing more, https or http on localhost
// (the shape peer_auth.passkeys.origins has), and localhost only when this
// config dials the daemon on this machine -- a client elsewhere has its own
// localhost, which is not this daemon.
func usableOrigin(origin string, cfg config.Config) bool {
	u, err := url.Parse(origin)
	if err != nil || origin == "" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return false
	}
	local := strings.EqualFold(u.Hostname(), "localhost")
	switch {
	case u.Scheme == "https", u.Scheme == "http" && local:
		return !local || isLoopbackServerHost(cfg.ServerHost())
	}
	return false
}

// socketConsoleRefusal is console-open on a config whose server_url is the
// daemon's socket: a browser cannot dial a socket, so there is nothing to
// open from this config. It names where the daemon says its console is, when
// the daemon can be asked.
func socketConsoleRefusal(cfg config.Config) error {
	where := "the daemon's TCP address"
	if origin := daemonOrigin(cfg); usableOrigin(origin, cfg) {
		where = origin + "/console/"
	}
	return fmt.Errorf("this config's server_url is the daemon's socket (%s), which a browser cannot dial: "+
		"open %s and sign in with the daemon's key, or run console-open with a config that names the TCP server_url and "+
		"mcp.api_key (set SEAMLESS_CONFIG)", cfg.DaemonAddress(), where)
}

// errUnlockBrowser refuses --browser beside --unlock. --browser names the
// application to launch, and --unlock launches none, so it is present and means
// nothing: refused rather than quietly ignored.
var errUnlockBrowser = errors.New("--browser does not combine with --unlock: an unlock begins on the console's unlock page, " +
	"in the browser you sign in to the console with, and console-open --unlock only approves the code that page shows")

// unlockTerminal is the terminal console-open --unlock talks to: where a
// missing code is asked for and the outcome is said. interactive is whether
// in is a terminal the owner types at.
type unlockTerminal struct {
	in          io.Reader
	out         io.Writer
	interactive bool
}

// stdioUnlockTerminal is the process's own terminal.
func stdioUnlockTerminal() unlockTerminal {
	return unlockTerminal{in: os.Stdin, out: os.Stdout, interactive: term.IsTerminal(int(os.Stdin.Fd()))}
}

// unlockPage is where an unlock of project begins: the console's unlock page,
// on the origin the browser signs in on. It is no secret.
func unlockPage(cfg config.Config, project string) string {
	return consoleBase(cfg) + "/console/unlock/" + url.PathEscape(project)
}

// runConsoleUnlock is console-open --unlock: the terminal's half of a
// terminal unlock. The owner's browser begins it on the project's unlock page,
// which shows a pairing code; this approves that code as the daemon's own
// account (the same check as seamlessd approve: the sudo is the proof), and
// the browser that showed it reads the project. The unlock never reaches an
// agent.
//
// Nothing it reads or prints is a credential. The code only names a row the
// browser began, bound to that browser's cookie, so someone reading this
// terminal (a shared tmux server, scrollback) learns nothing that opens
// another browser. It launches nothing either: the daemon's account usually
// has no desktop session to open a browser in.
func runConsoleUnlock(cfg config.Config, project, code, forFlag, browser string) error {
	return consoleUnlock(cfg, project, code, forFlag, browser, stdioUnlockTerminal())
}

// consoleUnlock is runConsoleUnlock with its terminal given.
func consoleUnlock(cfg config.Config, project, code, forFlag, browser string, tty unlockTerminal) error {
	if browser != "" {
		return errUnlockBrowser
	}
	if err := approverAccountCheck(cfg, defaultPeerAuthEnv()); err != nil {
		return err
	}
	// Settled before the database is opened: nothing to ask the owner with.
	if strings.TrimSpace(code) == "" && !tty.interactive {
		return fmt.Errorf("console-open --unlock needs --code: open %s in the browser you sign in to the console with, "+
			"choose Approve from a terminal, and run the command it shows", unlockPage(cfg, project))
	}
	db, err := openServedDB(cfg, os.Stderr)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc, err := newApprovals(cfg, db, events.NewRecorder(db), logger)
	if err != nil {
		return err
	}
	return approveConsoleUnlock(context.Background(), cfg, db, svc, tty, project, code, forFlag)
}

// approveConsoleUnlock is console-open --unlock's testable core: it checks
// project is locked and the duration (absent: the whole console lease;
// present: one no longer than it), asks for the code when none was given,
// approves it, and says what it opened. A code that is malformed, mistyped,
// used, expired or another project's changes nothing.
func approveConsoleUnlock(ctx context.Context, cfg config.Config, db *sql.DB, svc *approval.Service, tty unlockTerminal,
	project, code, forFlag string) error {
	locked, err := lockedProjects(ctx, cfg, db)
	if err != nil {
		return err
	}
	if !slices.Contains(locked, project) {
		return fmt.Errorf("project %q is not locked: the locked projects are %s", project, strings.Join(locked, ", "))
	}
	maxLease := svc.ConsoleLeaseLength()
	lease := maxLease
	if forFlag != "" {
		d, err := time.ParseDuration(forFlag)
		if err != nil || d < time.Minute || d > maxLease {
			return fmt.Errorf("invalid --for %q: a duration from 1m to %s (the console lease)", forFlag, maxLease)
		}
		lease = d
	}
	if strings.TrimSpace(code) == "" {
		if !tty.interactive {
			return fmt.Errorf("console-open --unlock needs --code: open %s and choose Approve from a terminal", unlockPage(cfg, project))
		}
		fmt.Fprintf(tty.out, "Open %s in the browser you sign in to the console with, and choose Approve from a terminal.\n"+
			"Code it shows: ", unlockPage(cfg, project))
		line, err := bufio.NewReader(tty.in).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("no code was typed: nothing changed (%w)", err)
		}
		if code = strings.TrimSpace(line); code == "" {
			return errors.New("no code was typed: nothing changed")
		}
	}
	shown := store.FormatUnlockCode(code)
	u, err := svc.ApproveTerminalUnlock(ctx, project, code, lease)
	switch {
	case errors.Is(err, store.ErrInvalidConsoleUnlock) && shown == "":
		return &ownerError{msg: fmt.Sprintf("%q is not an unlock code: the unlock page shows one as XXXX-XXXX, 8 letters and digits. Nothing changed.", code), cause: err}
	case errors.Is(err, store.ErrConsoleUnlockNotFound):
		return &ownerError{msg: fmt.Sprintf("no unlock of %s is waiting for code %s: it may be mistyped, already used, expired (a code lasts %s), "+
			"or shown for another project. Nothing changed. Start again on %s.",
			project, shown, minutesWords(approval.ConsoleTerminalUnlockTTL), unlockPage(cfg, project)), cause: err}
	case err != nil:
		return err
	}
	fmt.Fprintf(tty.out, "Unlocked %s for %s, until %s, in the browser that showed code %s.\n",
		u.Project, leaseWords(u.Lease), access.Clock(*u.ExpiresAt), shown)
	return nil
}

// leaseWords says an unlock's length as the owner reads it: "3 hours", "90
// minutes".
func leaseWords(d time.Duration) string {
	switch h := int(d / time.Hour); {
	case d%time.Hour != 0:
		return minutesWords(d)
	case h == 1:
		return "1 hour"
	default:
		return fmt.Sprintf("%d hours", h)
	}
}

// minutesWords says a whole-minute duration as the owner reads it: "10
// minutes", not Go's "10m0s".
func minutesWords(d time.Duration) string {
	if m := int(d / time.Minute); m == 1 {
		return "1 minute"
	} else if d%time.Minute == 0 {
		return fmt.Sprintf("%d minutes", m)
	}
	return d.String()
}
