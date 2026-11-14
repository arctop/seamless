package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// signin3Browser is a browser's begin request as the console records it.
var signin3Browser = TerminalSignInBrowser{Addr: "192.0.2.44", UserAgent: "Mozilla/5.0 (X11; Linux x86_64) Example/1.0"}

// signin3RequirePrintable fails unless s holds printable ASCII only.
func signin3RequirePrintable(t *testing.T, s string) {
	t.Helper()
	for i := range len(s) {
		require.True(t, s[i] >= ' ' && s[i] <= '~', "byte %d of %q is not printable ASCII", i, s)
	}
}

// The begin records the browser that began a terminal sign-in, and the lookup
// by code -- what the owner's terminal reads before asking -- returns it with
// when it began, while the code waits.
func TestTerminalSignIn_RecordsTheBrowserAtBegin(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	row, code, err := BeginTerminalSignIn(ctx, db, "cookie-a", "key-a", signin3Browser, apNow)
	require.NoError(t, err)
	require.Equal(t, signin3Browser, row.Browser)

	w, err := TerminalSignInByCode(ctx, db, code, apNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, row.ID, w.ID)
	require.Equal(t, apNow, w.CreatedAt, "when it began")
	require.Equal(t, signin3Browser, w.Browser)
	w, err = TerminalSignInByCode(ctx, db, strings.ToLower(strings.ReplaceAll(code, "-", "")), apNow)
	require.NoError(t, err, "the code as the owner may type it")
	require.Equal(t, row.ID, w.ID)

	none, _, err := BeginTerminalSignIn(ctx, db, "cookie-b", "key-b", TerminalSignInBrowser{}, apNow)
	require.NoError(t, err)
	require.Equal(t, TerminalSignInBrowser{}, none.Browser, "nothing recorded is nothing")
}

// The lookup refuses, as the approval does, a code that is malformed, names
// nothing, was approved, is past its deadline or ended.
func TestTerminalSignIn_ByCodeRefusals(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	_, err := TerminalSignInByCode(ctx, db, "bad!", apNow)
	require.ErrorIs(t, err, ErrInvalidSignIn)
	_, err = TerminalSignInByCode(ctx, db, "ZZZZ-ZZZZ", apNow)
	require.ErrorIs(t, err, ErrTerminalSignInNotFound)

	used, code, err := BeginTerminalSignIn(ctx, db, "cookie-used", "key", signin3Browser, apNow)
	require.NoError(t, err)
	_, err = ApproveTerminalSignIn(ctx, db, used.ID, code, time.Hour, apNow)
	require.NoError(t, err)
	_, err = TerminalSignInByCode(ctx, db, code, apNow)
	require.ErrorIs(t, err, ErrTerminalSignInNotFound, "already used")

	_, late, err := BeginTerminalSignIn(ctx, db, "cookie-late", "key", signin3Browser, apNow)
	require.NoError(t, err)
	_, err = TerminalSignInByCode(ctx, db, late, apNow.Add(TerminalSignInTTL))
	require.ErrorIs(t, err, ErrTerminalSignInNotFound, "past its deadline")

	_, out, err := BeginTerminalSignIn(ctx, db, "cookie-out", "key", signin3Browser, apNow)
	require.NoError(t, err)
	_, err = EndConsoleSignInByCookie(ctx, db, "cookie-out", apNow)
	require.NoError(t, err)
	_, err = TerminalSignInByCode(ctx, db, out, apNow)
	require.ErrorIs(t, err, ErrTerminalSignInNotFound, "its browser signed out")
}

// The approval signs in the row the owner was shown, or none: a code with
// another row's id approves nothing, and changes nothing.
func TestTerminalSignIn_ApprovesOnlyTheRowShown(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	a, codeA, err := BeginTerminalSignIn(ctx, db, "cookie-a", "key-a", signin3Browser, apNow)
	require.NoError(t, err)
	b, codeB, err := BeginTerminalSignIn(ctx, db, "cookie-b", "key-b", signin3Browser, apNow)
	require.NoError(t, err)

	_, err = ApproveTerminalSignIn(ctx, db, b.ID, codeA, time.Hour, apNow)
	require.ErrorIs(t, err, ErrTerminalSignInNotFound)
	_, err = ApproveTerminalSignIn(ctx, db, "", codeA, time.Hour, apNow)
	require.ErrorIs(t, err, ErrTerminalSignInNotFound)
	n, err := CountLiveConsoleSignIns(ctx, db, "", apNow)
	require.NoError(t, err)
	require.Zero(t, n, "a mismatched approval signs nothing in")
	for _, code := range []string{codeA, codeB} {
		_, err = TerminalSignInByCode(ctx, db, code, apNow)
		require.NoError(t, err, "both still wait")
	}

	s, err := ApproveTerminalSignIn(ctx, db, a.ID, codeA, time.Hour, apNow)
	require.NoError(t, err)
	require.Equal(t, a.ID, s.ID)
	_, err = LiveConsoleSignIn(ctx, db, HashConsoleSignIn("cookie-a", "key-a"), apNow)
	require.NoError(t, err)
	_, err = LiveConsoleSignIn(ctx, db, HashConsoleSignIn("cookie-b", "key-b"), apNow)
	require.ErrorIs(t, err, ErrSignInNotFound)
}

// A hostile User-Agent -- terminal escapes, newlines, a bidi override, invalid
// bytes, megabytes of it -- is kept as bounded printable ASCII, and a row
// written by anything else is cleaned on the way out as well, so nothing the
// browser sent can act on the owner's terminal.
func TestTerminalSignIn_BrowserIsPrintableASCII(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	hostile := TerminalSignInBrowser{
		Addr:      "192.0.2.44\x1b[2J\n",
		UserAgent: "\x1b[2J\x1b[31mExample\x07\r\nSign in this browser? [y/N]: y\t\u202e\u009b31m\xff" + strings.Repeat("A", 1<<20),
	}
	row, code, err := BeginTerminalSignIn(ctx, db, "cookie-h", "key-h", hostile, apNow)
	require.NoError(t, err)
	for _, b := range []TerminalSignInBrowser{row.Browser, signin3ByCode(t, db, code)} {
		signin3RequirePrintable(t, b.Addr)
		signin3RequirePrintable(t, b.UserAgent)
		require.Equal(t, "192.0.2.44?[2J", b.Addr)
		require.Len(t, b.UserAgent, MaxTerminalSignInUserAgent)
		require.True(t, strings.HasPrefix(b.UserAgent, "?[2J?[31mExample???Sign in this browser? [y/N]: y???31m?AAA"), b.UserAgent)
		require.True(t, strings.HasSuffix(b.UserAgent, "A..."), "a cut one says so")
	}
	var stored string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT user_agent FROM console_signins WHERE id = ?`, row.ID).Scan(&stored))
	require.Len(t, stored, MaxTerminalSignInUserAgent, "the row keeps the bounded text, never the megabyte")

	// A row the begin did not write -- an older release's, or a database
	// edited by hand -- reads back clean too.
	_, raw, err := BeginTerminalSignIn(ctx, db, "cookie-raw", "key-raw", TerminalSignInBrowser{}, apNow)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE console_signins SET remote_addr = ?, user_agent = ? WHERE user_agent = '' AND remote_addr = ''`,
		"\x1b]0;owned\x07", "\x1b[1A\x1b[2KExample\n"+strings.Repeat("B", 500))
	require.NoError(t, err)
	b := signin3ByCode(t, db, raw)
	signin3RequirePrintable(t, b.Addr)
	signin3RequirePrintable(t, b.UserAgent)
	require.Equal(t, "?]0;owned?", b.Addr)
	require.True(t, strings.HasPrefix(b.UserAgent, "?[1A?[2KExample?BBB"), b.UserAgent)
	require.Len(t, b.UserAgent, MaxTerminalSignInUserAgent)

	short := TerminalSignInBrowser{Addr: " 2001:db8::7 ", UserAgent: strings.Repeat("C", MaxTerminalSignInUserAgent)}.Clean()
	require.Equal(t, "2001:db8::7", short.Addr, "trimmed")
	require.Equal(t, strings.Repeat("C", MaxTerminalSignInUserAgent), short.UserAgent, "exactly the maximum is kept whole")
}

// signin3ByCode is the browser the waiting sign-in code names records.
func signin3ByCode(t *testing.T, db *sql.DB, code string) TerminalSignInBrowser {
	t.Helper()
	w, err := TerminalSignInByCode(context.Background(), db, code, apNow)
	require.NoError(t, err)
	return w.Browser
}

// A terminal sign-in the previous release began (after a rollback) recorded
// nothing: its lookup says so with an empty browser, never a guess.
func TestTerminalSignIn_APreviousReleaseRowRecordsNothing(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	at := core.FormatTime(apNow)
	_, err := db.ExecContext(ctx, `INSERT INTO console_signins (id, token_hash, cookie_hash, credential, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, "01OLDWAITING", HashConsoleSignIn("cookie-wait", "key-wait"), hashSignInCookie("cookie-wait"),
		terminalSignInWaiting+terminalSignInCodeHash("ABCD-EFGH"), at, at)
	require.NoError(t, err)
	w, err := TerminalSignInByCode(ctx, db, "ABCD-EFGH", apNow.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, "01OLDWAITING", w.ID)
	require.Equal(t, TerminalSignInBrowser{}, w.Browser)
	_, err = ApproveTerminalSignIn(ctx, db, w.ID, "abcd efgh", time.Hour, apNow.Add(time.Minute))
	require.NoError(t, err, "and it approves as before")
}
