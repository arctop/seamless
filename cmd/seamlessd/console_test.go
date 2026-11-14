package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// The health probe and the login page's form action are two views of one
// address: the probe wants a bare authority, the form wants the URL.
func TestURLHostPort(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8081": "127.0.0.1:8081",
		"http://localhost:9000": "localhost:9000",
		"http://[::1]:8081":     "[::1]:8081",
		"https://seam.lan:8443": "seam.lan:8443",
	}
	for in, want := range cases {
		require.Equal(t, want, urlHostPort(in), "urlHostPort(%q)", in)
	}
	for _, addr := range []string{"127.0.0.1:8081", "0.0.0.0:8081", ":8081", "[::]:8081", "localhost:9000"} {
		base := config.Config{Addr: addr}.ServerURL()
		require.Equal(t, strings.TrimPrefix(base, "http://"), urlHostPort(base), "addr %q", addr)
	}
}

func TestA2AEndpointUsesEffectiveBind(t *testing.T) {
	tests := map[string]string{
		"127.0.0.1:8081": "http://127.0.0.1:8081/api/a2a",
		"127.0.0.1:9099": "http://127.0.0.1:9099/api/a2a", // concrete --addr override
		"[::1]:8081":     "http://[::1]:8081/api/a2a",
		"0.0.0.0:8081":   "http://127.0.0.1:8081/api/a2a",
		":8081":          "http://127.0.0.1:8081/api/a2a",
		"[::]:8081":      "http://127.0.0.1:8081/api/a2a",
	}
	for bind, want := range tests {
		require.Equal(t, want, a2aEndpoint(config.Config{Addr: bind}), "bind %q", bind)
	}
}

// The card is discovery metadata: an agent dials exactly what it says. Both
// transport keys must reach it, which a synthesized config.Config{Addr: bind}
// could not express -- it advertised http:// on a TLS daemon and loopback on a
// wildcard bind that server_url had already named.
func TestA2AEndpointCarriesTLSAndServerURL(t *testing.T) {
	require.Equal(t, "https://127.0.0.1:8081/api/a2a",
		a2aEndpoint(config.Config{Addr: "0.0.0.0:8081", TLS: config.TLS{CertFile: "c.pem", KeyFile: "k.pem"}}))
	require.Equal(t, "https://seam.lan:8443/api/a2a",
		a2aEndpoint(config.Config{Addr: "0.0.0.0:8081", AdvertisedURL: "https://seam.lan:8443"}))
}

func TestRenderConsoleLoginPage(t *testing.T) {
	page, err := renderConsoleLoginPage("http://127.0.0.1:8081", "deadbeefKEY")
	require.NoError(t, err)
	// POSTs the key to the login endpoint and auto-submits.
	require.Contains(t, page, `action="http://127.0.0.1:8081/console/login"`)
	require.Contains(t, page, `name="key" value="deadbeefKEY"`)
	require.Contains(t, page, `name="next" value="/console/"`)
	require.Contains(t, page, `.submit();`)
}

func TestBrowserCommand(t *testing.T) {
	tests := []struct {
		name     string
		goos     string
		app      string
		wantArgs []string
		wantErr  bool
	}{
		{"darwin default", "darwin", "", []string{"open", "/tmp/x.html"}, false},
		{"darwin named app", "darwin", "Google Chrome", []string{"open", "-a", "Google Chrome", "/tmp/x.html"}, false},
		{"linux default", "linux", "", []string{"xdg-open", "/tmp/x.html"}, false},
		{"windows default", "windows", "", []string{"cmd", "/c", "start", "", "/tmp/x.html"}, false},
		{"linux named app rejected", "linux", "firefox", nil, true},
		{"windows named app rejected", "windows", "chrome", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := browserCommand(tt.goos, "/tmp/x.html", tt.app)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantArgs, cmd.Args)
		})
	}
}

func TestRenderConsoleLoginPage_EscapesKey(t *testing.T) {
	// A key with attribute-breaking characters must be contextually escaped so
	// it cannot terminate the value="" attribute or inject markup.
	page, err := renderConsoleLoginPage("127.0.0.1:8081", `a"><script>x`)
	require.NoError(t, err)
	require.NotContains(t, page, `<script>x`)
	require.NotContains(t, page, `value="a">`)
	require.True(t, strings.Contains(page, "&#34;") || strings.Contains(page, "&quot;"),
		"quote in key must be escaped, got: %s", page)
}

// console-open --unlock --code, end to end as the daemon's account: it
// approves the code a browser's page shows, says what it opened, prints
// nothing secret, and launches nothing, even with a launcher of each name
// first on PATH.
func TestConsoleOpenUnlock_ApprovesTheCodeAndLaunchesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("peer_auth is a POSIX deployment: the account check refuses first")
	}
	dbPath := daemonDB(t, store.LatestSchemaVersion())
	cfg, err := config.Load()
	require.NoError(t, err)
	cookie, err := store.NewConsoleToken()
	require.NoError(t, err)
	u, code := beginTerminalIn(t, dbPath, "example-finance", store.HashConsoleToken(cookie))

	bin := t.TempDir()
	launched := filepath.Join(t.TempDir(), "launched")
	for _, name := range []string{"open", "xdg-open"} {
		script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + launched + "'\n"
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755))
	}
	t.Setenv("PATH", bin)

	out := captureStdout(t, func() error { return runConsoleUnlock(cfg, "example-finance", code, "", "") })
	require.Contains(t, out, "Unlocked example-finance for 3 hours, until ")
	require.Contains(t, out, "in the browser that showed code "+code+".")
	require.NotContains(t, out, cookie)
	require.NotContains(t, out, "?t=", "no link: nothing the terminal shows opens a browser")
	_, err = os.Stat(launched)
	require.ErrorIs(t, err, os.ErrNotExist, "no launcher ran")

	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	live, err := store.LiveConsoleUnlocks(context.Background(), db, store.HashConsoleToken(cookie), time.Now())
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, u.ID, live[0].ID, "the browser that began it reads the project")
}

// beginTerminalIn begins a terminal unlock of project in the database at
// dbPath, as the browser whose cookie hashes to tokenHash does, and returns
// the row and its code.
func beginTerminalIn(t *testing.T, dbPath, project, tokenHash string) (store.ConsoleUnlock, string) {
	t.Helper()
	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	u, code, err := store.BeginTerminalUnlock(context.Background(), db, project, tokenHash, 3*time.Hour, 10*time.Minute, time.Now())
	require.NoError(t, err)
	return u, code
}

// Without --code it asks for one in a terminal, and refuses -- naming --code
// and the page where the unlock begins -- anywhere else, before any database
// is opened.
func TestConsoleOpenUnlock_WithoutACode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("peer_auth is a POSIX deployment: the account check refuses first")
	}
	dbPath := daemonDB(t, store.LatestSchemaVersion())
	cfg, err := config.Load()
	require.NoError(t, err)
	page := consoleBase(cfg) + "/console/unlock/example-finance"

	var said bytes.Buffer
	err = consoleUnlock(cfg, "example-finance", "", "", "", unlockTerminal{in: strings.NewReader(""), out: &said})
	require.ErrorContains(t, err, "needs --code")
	require.ErrorContains(t, err, page)
	require.Empty(t, said.String(), "nothing asked where nobody can answer")

	cookie, err := store.NewConsoleToken()
	require.NoError(t, err)
	_, code := beginTerminalIn(t, dbPath, "example-finance", store.HashConsoleToken(cookie))
	typed := strings.ToLower(strings.ReplaceAll(code, "-", " ")) + "\n"
	err = consoleUnlock(cfg, "example-finance", "", "", "", unlockTerminal{in: strings.NewReader(typed), out: &said, interactive: true})
	require.NoError(t, err)
	require.Contains(t, said.String(), "Open "+page+" in the browser you sign in to the console with")
	require.Contains(t, said.String(), "Code it shows: ")
	require.Contains(t, said.String(), "Unlocked example-finance for 3 hours")

	said.Reset()
	err = consoleUnlock(cfg, "example-finance", "", "", "", unlockTerminal{in: strings.NewReader("\n"), out: &said, interactive: true})
	require.ErrorContains(t, err, "no code was typed: nothing changed")
}

// --browser names the application to launch, and --unlock launches none: a
// flag that is present and means nothing is refused, not quietly ignored. It
// is settled first, so it is the answer before the account check or any
// database is opened. --code and --for mean nothing without --unlock.
func TestConsoleOpenUnlock_RefusesBrowser(t *testing.T) {
	require.ErrorIs(t, runConsoleUnlock(config.Defaults(), "example-finance", "ABCD-EFGH", "", "Google Chrome"), errUnlockBrowser,
		"ahead of the account check, which says peer_auth is off")

	writeDaemonConfig(t, t.TempDir())
	err := runConsoleOpen([]string{"--unlock", "example-finance", "--browser", "Safari"})
	require.ErrorIs(t, err, errUnlockBrowser)
	require.ErrorContains(t, err, "--browser does not combine with --unlock")
	for _, args := range [][]string{{"--code", "ABCD-EFGH"}, {"--for", "1h"}} {
		require.ErrorContains(t, runConsoleOpen(args), "--for and --code need --unlock <project>")
	}
}

// retireSignInDB is the daemon's database with passkey sign-in on in the
// config console-open loads, and a terminal sign-in begun by the browser
// holding cookie and key, from 192.0.2.44 with an example User-Agent; it
// returns the config, the database's path and the code that browser's page
// shows.
func retireSignInDB(t *testing.T, cookie, key string) (config.Config, string, string) {
	t.Helper()
	dbPath := daemonDB(t, store.LatestSchemaVersion())
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Console.SignIn = config.ConsoleSignInPasskey
	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	_, code, err := store.BeginTerminalSignIn(context.Background(), db, cookie, key,
		store.TerminalSignInBrowser{Addr: "192.0.2.44", UserAgent: "Mozilla/5.0 (X11; Linux x86_64) Example/1.0"}, time.Now())
	require.NoError(t, err)
	return cfg, dbPath, code
}

// console-open --sign-in --code, as the daemon's account: it shows the
// browser the code names and, once the owner says yes, signs in the browser
// whose page showed the code -- that browser's cookie with its own key, and
// nothing else -- prints nothing secret, and launches nothing.
func TestConsoleOpenSignIn_SignsInTheBrowserThatShowedTheCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("peer_auth is a POSIX deployment: the account check refuses first")
	}
	cookie, err := store.NewConsoleToken()
	require.NoError(t, err)
	cfg, dbPath, code := retireSignInDB(t, cookie, "browser-key-a")

	var said bytes.Buffer
	require.NoError(t, consoleSignIn(cfg, code, "", unlockTerminal{in: strings.NewReader("y\n"), out: &said, interactive: true}, time.Now))
	require.Contains(t, said.String(), "192.0.2.44 (another computer)")
	require.Contains(t, said.String(), "Mozilla/5.0 (X11; Linux x86_64) Example/1.0")
	require.Contains(t, said.String(), "Signed in the browser that showed code "+code+", until ")
	require.NotContains(t, said.String(), cookie)

	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	ctx := context.Background()
	s, err := store.LiveConsoleSignIn(ctx, db, store.HashConsoleSignIn(cookie, "browser-key-a"), time.Now())
	require.NoError(t, err)
	require.Equal(t, store.SignInByDaemon, s.Credential)
	_, err = store.LiveConsoleSignIn(ctx, db, store.HashConsoleSignIn(cookie, "browser-key-b"), time.Now())
	require.ErrorIs(t, err, store.ErrSignInNotFound, "the cookie in another browser signs nothing in")

	said.Reset()
	err = consoleSignIn(cfg, code, "", unlockTerminal{in: strings.NewReader("y\n"), out: &said, interactive: true}, time.Now)
	require.ErrorContains(t, err, "no sign-in is waiting for code "+code, "a code works once")
	require.ErrorContains(t, err, "Nothing changed.")
	require.NotContains(t, said.String(), "Sign in this browser?", "refused before anything is asked")
}

// Every refusal changes nothing: --browser, a key-signs-in config, no code
// with no one to ask, a malformed or unknown code, and --sign-in beside
// --unlock or --for.
func TestConsoleOpenSignIn_Refusals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("peer_auth is a POSIX deployment: the account check refuses first")
	}
	require.ErrorIs(t, consoleSignIn(config.Defaults(), "ABCD-EFGH", "Safari", unlockTerminal{}, time.Now), errSignInBrowser)
	require.ErrorContains(t, consoleSignIn(config.Defaults(), "ABCD-EFGH", "", unlockTerminal{}, time.Now), "console.sign_in is key")

	cookie, err := store.NewConsoleToken()
	require.NoError(t, err)
	cfg, dbPath, _ := retireSignInDB(t, cookie, "browser-key-a")
	var said bytes.Buffer
	err = consoleSignIn(cfg, "", "", unlockTerminal{in: strings.NewReader(""), out: &said}, time.Now)
	require.ErrorContains(t, err, "needs --code")
	require.ErrorContains(t, err, "/console/login")
	require.Empty(t, said.String(), "nothing asked where nobody can answer")

	tty := unlockTerminal{in: strings.NewReader("y\ny\n"), out: &said, interactive: true}
	require.ErrorContains(t, consoleSignIn(cfg, "bad!", "", tty, time.Now), "is not a sign-in code")
	require.ErrorContains(t, consoleSignIn(cfg, "ZZZZ-ZZZZ", "", tty, time.Now), "no sign-in is waiting for code ZZZZ-ZZZZ")
	require.Empty(t, said.String(), "a bad code is refused before anything is asked")

	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	n, err := store.CountLiveConsoleSignIns(context.Background(), db, "", time.Now())
	require.NoError(t, db.Close())
	require.NoError(t, err)
	require.Zero(t, n, "no refusal signed anything in")

	for _, args := range [][]string{{"--sign-in", "--unlock", "example-finance"}, {"--sign-in", "--for", "1h"}} {
		require.ErrorContains(t, runConsoleOpen(args), "--sign-in takes only --code")
	}
}
