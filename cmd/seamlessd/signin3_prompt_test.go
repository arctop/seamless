package main

import (
	"bytes"
	"context"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// console-open --sign-in --code shows the owner the browser a code would sign
// in and asks before it signs it in (REVIEW2 L3): anyone who reaches the
// console can show a code, so a code someone sent the owner names their
// browser. These tests drive the command as the owner runs it, against the
// daemon's own database.

// signin3Daemon is the daemon's database with passkey sign-in on in the config
// console-open loads; it returns the config and the database's path.
func signin3Daemon(t *testing.T) (config.Config, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("peer_auth is a POSIX deployment: the account check refuses first")
	}
	dbPath := daemonDB(t, store.LatestSchemaVersion())
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Console.SignIn = config.ConsoleSignInPasskey
	return cfg, dbPath
}

// signin3Begin begins a terminal sign-in as the console's begin route does,
// for the browser holding browserKey, and returns its row's id and its code.
func signin3Begin(t *testing.T, cfg config.Config, dbPath, browserKey string) (string, string) {
	t.Helper()
	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	view, err := newConsoleSignIn(cfg, db, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	begun, err := view.BeginTerminalSignIn(context.Background(), browserKey)
	require.NoError(t, err)
	return begun.ID, begun.Code
}

// signin3SetBrowser writes what a begin records of its browser straight into
// row id: the address and the User-Agent, as anything that writes the row
// could.
func signin3SetBrowser(t *testing.T, dbPath, id, addr, agent string) {
	t.Helper()
	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	_, err = db.Exec(`UPDATE console_signins SET remote_addr = ?, user_agent = ? WHERE id = ?`, addr, agent, id)
	require.NoError(t, err)
}

// signin3Live counts the console sign-ins live now.
func signin3Live(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	n, err := store.CountLiveConsoleSignIns(context.Background(), db, "", time.Now())
	require.NoError(t, err)
	return n
}

// signin3Run runs console-open --sign-in --code code at a terminal where the
// owner types typed, and returns what it said and its error.
func signin3Run(cfg config.Config, code, typed string) (string, error) {
	var said bytes.Buffer
	err := consoleSignIn(cfg, code, "", unlockTerminal{in: strings.NewReader(typed), out: &said, interactive: true}, time.Now)
	return said.String(), err
}

// Any answer but yes -- no, Enter, anything else, the end of input -- signs
// nothing in and leaves the code waiting; yes signs in exactly that browser,
// once.
func TestConsoleOpenSignIn_AsksBeforeItSignsIn(t *testing.T) {
	cfg, dbPath := signin3Daemon(t)
	_, code := signin3Begin(t, cfg, dbPath, "signin3-browser-key")
	for _, typed := range []string{"n\n", "N\n", "no\n", "\n", "   \n", "nope\n", "yy\n", "y es\n", ""} {
		said, err := signin3Run(cfg, code, typed)
		require.Contains(t, said, "Sign in this browser? [y/N]: ", "%q", typed)
		require.NotContains(t, said, "Signed in", "%q", typed)
		if typed == "" {
			require.ErrorContains(t, err, "no answer: nothing changed", "the end of input")
		} else {
			require.NoError(t, err, "%q", typed)
			require.Contains(t, said, "Not signed in. Nothing changed.", "%q", typed)
		}
		require.Zero(t, signin3Live(t, dbPath), "%q signs nothing in", typed)
	}

	said, err := signin3Run(cfg, code, "y\n")
	require.NoError(t, err, "the code still waited after every refusal")
	require.Contains(t, said, "Signed in the browser that showed code "+code)
	require.Less(t, strings.Index(said, "Sign in this browser? [y/N]: "), strings.Index(said, "Signed in the browser"), "asked first")
	require.Equal(t, 1, signin3Live(t, dbPath))

	_, other := signin3Begin(t, cfg, dbPath, "signin3-other-key")
	said, err = signin3Run(cfg, other, " YES \n")
	require.NoError(t, err)
	require.Contains(t, said, "Signed in the browser that showed code "+other)
	require.Equal(t, 2, signin3Live(t, dbPath))
}

// The code may be typed at the prompt instead of given with --code: the same
// terminal then answers the question, and nothing it buffered is lost.
func TestConsoleOpenSignIn_TypedCodeThenAsks(t *testing.T) {
	cfg, dbPath := signin3Daemon(t)
	_, code := signin3Begin(t, cfg, dbPath, "signin3-browser-key")
	said, err := signin3Run(cfg, "", code+"\nn\n")
	require.NoError(t, err)
	require.Contains(t, said, "Code it shows: ")
	require.Contains(t, said, "Not signed in. Nothing changed.")
	require.Zero(t, signin3Live(t, dbPath))

	said, err = signin3Run(cfg, "", strings.ToLower(code)+"\ny\n")
	require.NoError(t, err)
	require.Contains(t, said, "Signed in the browser that showed code "+code)
	require.Equal(t, 1, signin3Live(t, dbPath))
}

// With no terminal there is no one to ask, so nothing is signed in: a script
// cannot approve a code it was handed, and no flag skips the question.
func TestConsoleOpenSignIn_NoTerminalSignsNothingIn(t *testing.T) {
	cfg, dbPath := signin3Daemon(t)
	_, code := signin3Begin(t, cfg, dbPath, "signin3-browser-key")
	var said bytes.Buffer
	err := consoleSignIn(cfg, code, "", unlockTerminal{in: strings.NewReader("y\n"), out: &said}, time.Now)
	require.ErrorContains(t, err, "runs only at a terminal")
	require.ErrorContains(t, err, "Nothing changed")
	require.Empty(t, said.String())
	require.Zero(t, signin3Live(t, dbPath))

	said.Reset()
	require.NoError(t, consoleSignIn(cfg, code, "", unlockTerminal{in: strings.NewReader("y\n"), out: &said, interactive: true}, time.Now),
		"the code still waits for a terminal")
	require.Equal(t, 1, signin3Live(t, dbPath))
}

// Before it asks, the approval shows what the begin recorded of the browser:
// when it began, the address it came from and whose that is, and its
// User-Agent. A row that recorded nothing says so.
func TestConsoleOpenSignIn_ShowsTheBrowserBeforeAsking(t *testing.T) {
	cfg, dbPath := signin3Daemon(t)
	for _, tc := range []struct {
		addr, agent string
		want        []string
	}{
		{"192.0.2.44", "Mozilla/5.0 (X11; Linux x86_64) Example/1.0",
			[]string{"  from     192.0.2.44 (another computer)\n", "  browser  Mozilla/5.0 (X11; Linux x86_64) Example/1.0\n"}},
		{"127.0.0.1", "Mozilla/5.0 (Macintosh) Example/17.0",
			[]string{"  from     127.0.0.1 (this computer, or a proxy in front of the console)\n", "  browser  Mozilla/5.0 (Macintosh) Example/17.0\n"}},
		{"::1", "", []string{"  from     ::1 (this computer, or a proxy in front of the console)\n", "  browser  none sent\n"}},
		{"", "", []string{"  from     not recorded\n", "  browser  not recorded\n"}},
	} {
		id, code := signin3Begin(t, cfg, dbPath, "signin3-key-"+tc.addr)
		signin3SetBrowser(t, dbPath, id, tc.addr, tc.agent)
		said, err := signin3Run(cfg, code, "n\n")
		require.NoError(t, err)
		ask := strings.Index(said, "Sign in this browser? [y/N]: ")
		require.Positive(t, ask)
		facts := said[:ask]
		require.Contains(t, facts, "Code "+code+" names a browser waiting to sign in to this console:\n")
		require.Contains(t, facts, "  began    ")
		require.Contains(t, facts, "less than a minute ago\n")
		for _, w := range tc.want {
			require.Contains(t, facts, w, tc.addr)
		}
		require.Contains(t, facts, "never to one someone sent you")
	}
	require.Zero(t, signin3Live(t, dbPath))
}

// A hostile User-Agent or address -- terminal escapes, a forged prompt on a
// new line, a bell, megabytes of text -- is shown as one bounded line of
// printable ASCII: nothing in it acts on the owner's terminal.
func TestConsoleOpenSignIn_HostileBrowserPrintsSafely(t *testing.T) {
	cfg, dbPath := signin3Daemon(t)
	id, code := signin3Begin(t, cfg, dbPath, "signin3-browser-key")
	signin3SetBrowser(t, dbPath, id, "192.0.2.44\x1b]0;owned\x07",
		"\x1b[2J\x1b[1;1HExample\r\nSigned in the browser. Sign in this browser? [y/N]: y\n\x9b31m\u202e"+strings.Repeat("A", 1<<20))
	said, err := signin3Run(cfg, code, "n\n")
	require.NoError(t, err)
	for i := range len(said) {
		c := said[i]
		require.True(t, c == '\n' || (c >= ' ' && c <= '~'), "byte %d (%#x) of the output is not printable ASCII", i, c)
	}
	var browserLine string
	for line := range strings.SplitSeq(said, "\n") {
		if strings.HasPrefix(line, "  browser  ") {
			browserLine = line
		}
	}
	require.True(t, strings.HasPrefix(browserLine, "  browser  ?[2J?[1;1HExample??Signed in the browser. Sign in this browser? [y/N]: y??31m?AAA"),
		browserLine)
	require.LessOrEqual(t, len(browserLine), len("  browser  ")+256, "cut to 256 characters")
	require.True(t, strings.HasSuffix(browserLine, "..."), "a cut one says so")
	require.Contains(t, said, "  from     192.0.2.44?]0;owned?\n")
	require.Equal(t, 1, strings.Count(said, "Sign in this browser? [y/N]: y"), "the forged prompt stays inside its line")
	require.Zero(t, signin3Live(t, dbPath))
}
