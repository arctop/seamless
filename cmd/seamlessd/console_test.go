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
	err = consoleSignIn(cfg, "ZZZZ-ZZZZ", "", tty, time.Now)
	require.ErrorContains(t, err, "no sign-in is waiting for code ZZZZ-ZZZZ")
	require.ErrorContains(t, err, "or crowded out by newer codes shown from the same address", "a displaced code is one reason")
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

// --path lands on a console page, by the rule the login form's next follows:
// anything the console would replace with its home, or a browser would
// rewrite into another path or site, is refused up front instead.
func TestConsoleOpenPath_OnlyAConsolePage(t *testing.T) {
	for _, ok := range []string{"", "/console/", "/console/approvers", "/console/settings?s=updates", "/console/projects/demo#isolation"} {
		got, err := consoleOpenPath(ok)
		require.NoError(t, err, ok)
		if ok == "" {
			require.Equal(t, "/console/", got)
		} else {
			require.Equal(t, ok, got)
		}
	}
	for _, bad := range []string{"approvers", "/console", "/other", "//evil.example/console/", "/console//evil.example",
		"https://evil.example/console/", "/console/../logout", "/console/./x", "/console/%2e%2e/logout", "/console/.%2E/x", "/console/%2E/x", "/console/a\\b", "/console/a b", "/console/a\nb"} {
		_, err := consoleOpenPath(bad)
		require.ErrorContains(t, err, "is not a console page", bad)
	}

	page, err := renderConsoleLoginPageAt("http://127.0.0.1:8081", "k", "/console/approvers")
	require.NoError(t, err)
	require.Contains(t, page, `name="next" value="/console/approvers"`)
	got, ok := consoleSignInPage(config.Config{Console: config.Console{SignIn: "passkey"}}, "http://127.0.0.1:8081", "/console/approvers",
		func(config.Config) bool { return false })
	require.True(t, ok)
	require.Equal(t, "http://127.0.0.1:8081/console/login?next=%2Fconsole%2Fapprovers", got)
}

// A socket config cannot drive a browser, so console-open opens with the
// console's own config beside it -- where seamlessd peer-auth setup writes
// console.yaml -- and refuses, naming that file, when the owner keeps none.
func TestSocketConsoleConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a socket server_url names a Unix path")
	}
	for _, k := range []string{"SEAMLESS_SERVER_URL", "SEAMLESS_MCP_API_KEY", "SEAMLESS_ROLE"} {
		t.Setenv(k, "")
		require.NoError(t, os.Unsetenv(k))
	}
	const sock = "unix:///nonexistent/seamless/d.sock"
	world := func(t *testing.T, console string, mode os.FileMode) (config.Config, string) {
		t.Helper()
		dir := t.TempDir()
		src := filepath.Join(dir, "seamless.yaml")
		require.NoError(t, os.WriteFile(src, []byte("role: client\nserver_url: \""+sock+"\"\n"), 0o600))
		beside := filepath.Join(dir, "console.yaml")
		if console != "" {
			require.NoError(t, os.WriteFile(beside, []byte(console), 0o600))
			require.NoError(t, os.Chmod(beside, mode))
		}
		cfg, err := config.LoadFrom(src)
		require.NoError(t, err)
		require.NotEmpty(t, cfg.SocketPath())
		return cfg, beside
	}
	const tcp = "role: client\nserver_url: \"https://seam.example:8443\"\nmcp:\n  api_key: \"console-key\"\n"

	t.Run("beside", func(t *testing.T) {
		cfg, beside := world(t, tcp, 0o600)
		var out, errOut bytes.Buffer
		got, err := socketConsoleConfig(cfg, &out, &errOut)
		require.NoError(t, err)
		require.Empty(t, got.SocketPath())
		require.Equal(t, "https://seam.example:8443", got.ServerURL())
		require.Equal(t, "console-key", got.MCP.APIKey)
		require.Equal(t, beside, got.SourcePath())
		require.Contains(t, out.String(), cfg.SourcePath()+" dials the daemon's socket")
		require.Contains(t, out.String(), "opening the console with "+beside)
		require.Empty(t, errOut.String())
	})
	t.Run("readable by others", func(t *testing.T) {
		cfg, beside := world(t, tcp, 0o644)
		var out, errOut bytes.Buffer
		_, err := socketConsoleConfig(cfg, &out, &errOut)
		require.NoError(t, err)
		require.Contains(t, errOut.String(), "other accounts can read it (mode 0644): chmod 600 "+beside)
	})
	t.Run("absent", func(t *testing.T) {
		cfg, beside := world(t, "", 0)
		var out, errOut bytes.Buffer
		_, err := socketConsoleConfig(cfg, &out, &errOut)
		require.ErrorContains(t, err, "which a browser cannot dial")
		require.ErrorContains(t, err, "keep the console's own config at "+beside)
		require.NotContains(t, err.Error(), "SEAMLESS_CONFIG")
		require.Empty(t, out.String())
	})
	t.Run("dials the socket too", func(t *testing.T) {
		cfg, beside := world(t, "role: client\nserver_url: \""+sock+"\"\n", 0o600)
		_, err := socketConsoleConfig(cfg, &bytes.Buffer{}, &bytes.Buffer{})
		require.ErrorContains(t, err, beside+", the console's config, dials the daemon's socket too")
	})
	t.Run("unreadable", func(t *testing.T) {
		cfg, _ := world(t, "role: client\nnot_a_key: 1\n", 0o600)
		_, err := socketConsoleConfig(cfg, &bytes.Buffer{}, &bytes.Buffer{})
		require.ErrorContains(t, err, "the console's config beside "+cfg.SourcePath())
	})
	t.Run("no file to look beside", func(t *testing.T) {
		envOnly := config.Config{AdvertisedURL: sock, Role: config.RoleClient}
		_, err := socketConsoleConfig(envOnly, &bytes.Buffer{}, &bytes.Buffer{})
		require.ErrorContains(t, err, "SEAMLESS_CONFIG")

		// console.yaml that dials the socket itself names no other file.
		dir := t.TempDir()
		self := filepath.Join(dir, "console.yaml")
		require.NoError(t, os.WriteFile(self, []byte("role: client\nserver_url: \""+sock+"\"\n"), 0o600))
		cfg, err := config.LoadFrom(self)
		require.NoError(t, err)
		require.Empty(t, consoleConfigBeside(cfg))
		_, err = socketConsoleConfig(cfg, &bytes.Buffer{}, &bytes.Buffer{})
		require.ErrorContains(t, err, "SEAMLESS_CONFIG")
	})
	// ./seamless.yaml sits in a working directory anyone may commit to: a
	// console.yaml beside it would choose the site the browser goes to.
	t.Run("working directory", func(t *testing.T) {
		repo := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(repo, config.WorkingDirConfig), []byte("role: client\nserver_url: \""+sock+"\"\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(repo, "console.yaml"), []byte(tcp), 0o600))
		t.Setenv("HOME", t.TempDir())
		t.Setenv("SEAMLESS_CONFIG", "")
		require.NoError(t, os.Unsetenv("SEAMLESS_CONFIG"))
		t.Chdir(repo)
		cfg, err := config.Load()
		require.NoError(t, err)
		require.True(t, cfg.FromWorkingDir())
		require.Empty(t, consoleConfigBeside(cfg))
		_, err = socketConsoleConfig(cfg, &bytes.Buffer{}, &bytes.Buffer{})
		require.ErrorContains(t, err, "SEAMLESS_CONFIG")
	})
}
