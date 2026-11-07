package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"html/template"
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
func runConsoleOpen(args []string) error {
	fs := flag.NewFlagSet("console-open", flag.ContinueOnError)
	browser := fs.String("browser", "", `browser application to open instead of the default (e.g. "Google Chrome"; macOS only)`)
	unlock := fs.String("unlock", "", "a locked project to open in one browser's console (run as the daemon's account, through sudo -k)")
	forFlag := fs.String("for", "", "how long the unlock lasts once opened (with --unlock; at most and by default 3h, capped by peer_auth.max_lease)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("seamlessd.console-open: %w", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("seamlessd.console-open: %w", err)
	}
	if *unlock != "" || *forFlag != "" {
		if *unlock == "" {
			return errors.New("seamlessd.console-open: --for needs --unlock <project>")
		}
		if err := runConsoleUnlock(cfg, *unlock, *forFlag, *browser); err != nil {
			return fmt.Errorf("seamlessd.console-open: %w", err)
		}
		return nil
	}
	if strings.TrimSpace(cfg.MCP.APIKey) == "" {
		return fmt.Errorf("seamlessd.console-open: mcp.api_key is empty; run `seamlessd serve` once to generate it, or set it in seamless.yaml")
	}
	base := consoleBase(cfg)
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
	return cfg.ServerURL() + "/api/a2a"
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

// consoleBase is where console-open signs in. While a project is locked it is
// the passkey relying party's origin (localhost by default) rather than the
// server URL: the console cookie is per host, and a passkey ceremony on an IP
// address such as 127.0.0.1 is refused by every browser. Otherwise it is the
// server URL, as it always was.
func consoleBase(cfg config.Config) string {
	if cfg.PeerAuth.Enabled && len(cfg.PeerAuth.Locked) > 0 {
		if _, origins := cfg.RelyingParty(); len(origins) > 0 {
			return origins[0]
		}
	}
	return cfg.ServerURL()
}

// consoleUnlockLinkTTL is how long an unlock link waits to be opened. Once
// opened it is used up; the unlock it starts lasts --for.
const consoleUnlockLinkTTL = 10 * time.Minute

// runConsoleUnlock is console-open --unlock: the terminal way to open a
// locked project in one browser's console. It runs as the daemon's own
// account (the same check as seamlessd approve: the sudo is the proof), writes
// a one-time unlock link straight into the database, prints it, and tries to
// open it. A browser that opens it gets the seamless_unlock cookie; the
// unlock never reaches an agent.
func runConsoleUnlock(cfg config.Config, project, forFlag, browser string) error {
	if err := approverAccountCheck(cfg, defaultPeerAuthEnv()); err != nil {
		return err
	}
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	svc, err := newApprovals(cfg, db, events.NewRecorder(db), logger)
	if err != nil {
		return err
	}
	link, err := consoleUnlockLink(context.Background(), cfg, db, svc.ConsoleLeaseLength(), project, forFlag, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "Unlock link for %s (works once, within %s):\n\n  %s\n\nOpen it in the browser you sign in to the console with.\n",
		project, consoleUnlockLinkTTL, link)
	// The daemon's account usually has no desktop session to open a browser
	// in; the printed link is the way, and a launch failure says so quietly.
	if err := openInBrowser(link, browser); err != nil {
		fmt.Fprintln(os.Stderr, "(could not open a browser from this account; open the link above yourself)")
	}
	return nil
}

// consoleUnlockLink is runConsoleUnlock's testable core: it checks project is
// locked and the duration (absent: the whole console lease; present: a
// duration no longer than it), writes the link, and returns its URL.
func consoleUnlockLink(ctx context.Context, cfg config.Config, db *sql.DB, maxLease time.Duration, project, forFlag string, now time.Time) (string, error) {
	if !slices.Contains(cfg.PeerAuth.Locked, project) {
		return "", fmt.Errorf("project %q is not locked: peer_auth.locked lists %s", project, strings.Join(cfg.PeerAuth.Locked, ", "))
	}
	lease := maxLease
	if forFlag != "" {
		d, err := time.ParseDuration(forFlag)
		if err != nil || d < time.Minute || d > maxLease {
			return "", fmt.Errorf("invalid --for %q: a duration from 1m to %s (the console lease)", forFlag, maxLease)
		}
		lease = d
	}
	secret, _, err := store.CreateConsoleUnlockLink(ctx, db, project, lease, consoleUnlockLinkTTL, now)
	if err != nil {
		return "", err
	}
	return consoleBase(cfg) + "/console/unlock?t=" + url.QueryEscape(secret), nil
}
