package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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

// The unlock link is the credential: on a launcher's argv ps would show it to
// any process of the account, and whoever claims it first gets the unlock. So
// the unlock path prints the link and runs nothing, even with a launcher of
// each name first on PATH.
func TestConsoleOpenUnlock_PrintsTheLinkAndLaunchesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("peer_auth is a POSIX deployment: the account check refuses first")
	}
	daemonDB(t, store.LatestSchemaVersion())
	cfg, err := config.Load()
	require.NoError(t, err)

	bin := t.TempDir()
	launched := filepath.Join(t.TempDir(), "launched")
	for _, name := range []string{"open", "xdg-open"} {
		script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + launched + "'\n"
		require.NoError(t, os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755))
	}
	t.Setenv("PATH", bin)

	out := captureStdout(t, func() error { return runConsoleUnlock(cfg, "example-finance", "", "") })
	require.Contains(t, out, "Unlock link for example-finance (works once, within 10 minutes):")
	require.Contains(t, out, consoleBase(cfg)+"/console/unlock?t=")

	_, err = os.Stat(launched)
	require.ErrorIs(t, err, os.ErrNotExist, "no launcher ran: the link never reached an argv")
}

// --browser names the application to launch, and --unlock launches none: a
// flag that is present and means nothing is refused, not quietly ignored. It
// is settled first, so it is the answer before the account check or any
// database is opened.
func TestConsoleOpenUnlock_RefusesBrowser(t *testing.T) {
	require.ErrorIs(t, runConsoleUnlock(config.Defaults(), "example-finance", "", "Google Chrome"), errUnlockBrowser,
		"ahead of the account check, which says peer_auth is off")

	writeDaemonConfig(t, t.TempDir())
	err := runConsoleOpen([]string{"--unlock", "example-finance", "--browser", "Safari"})
	require.ErrorIs(t, err, errUnlockBrowser)
	require.ErrorContains(t, err, "--browser does not combine with --unlock")
}
