package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// signcapOtherHosts are two ways hosts other than this computer fill the cap
// they share, at their per-address share each: eight IPv4 addresses, and eight
// IPv6 /64s of one /56 -- an ordinary home or cloud delegation.
var signcapOtherHosts = []struct {
	name string
	addr func(i int) string
}{
	{"8 IPv4 addresses", func(i int) string { return fmt.Sprintf("192.0.2.%d", 10+i) }},
	{"8 /64s of one /56", func(i int) string { return fmt.Sprintf("2001:db8:0:%x::1", i) }},
}

// signcapFillOtherHosts begins maxWaitingTerminalSignIns terminal sign-ins at
// now, fstorePerAddress from each of addr(0), addr(1), ..., each of which must
// begin.
func signcapFillOtherHosts(t *testing.T, db *sql.DB, addr func(i int) string, now time.Time) {
	t.Helper()
	for i := range maxWaitingTerminalSignIns {
		from := addr(i / fstorePerAddress)
		_, err := fstoreBegin(t, db, from, now)
		require.NoError(t, err, "begin %d, from %s", i+1, from)
	}
}

// signcapWaiting counts the terminal sign-ins waiting at now.
func signcapWaiting(t *testing.T, db *sql.DB, now time.Time) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM console_signins WHERE `+waitingTerminalSignIn,
		core.FormatTime(now.Add(-TerminalSignInTTL))).Scan(&n))
	return n
}

// signcapBusyForAll asserts err is the refusal of the cap the addresses other
// than loopback share, not the one of an address's own share.
func signcapBusyForAll(t *testing.T, err error, from string) {
	t.Helper()
	require.ErrorIs(t, err, ErrTerminalSignInBusy, "from %s", from)
	require.NotErrorIs(t, err, ErrTerminalSignInBusyFrom, "from %s: refused for the whole queue, not for its own share", from)
}

// Hosts other than this computer that fill the cap they share -- eight IPv4
// addresses, or eight /64s of one /56, at their share each -- never hold off a
// begin from loopback: a browser on the server, the owner's way back in when
// every passkey is lost, still gets a code while loopback holds fewer than its
// share, and loopback holds no more than that.
func TestTerminalSignIn_OtherHostsLeaveLoopbackItsShare(t *testing.T) {
	for _, hosts := range signcapOtherHosts {
		t.Run(hosts.name, func(t *testing.T) {
			db := leaseDB(t)
			signcapFillOtherHosts(t, db, hosts.addr, apNow)
			_, err := fstoreBegin(t, db, "203.0.113.9", apNow)
			signcapBusyForAll(t, err, "another host")

			for i, addr := range []string{"127.0.0.1", "::1", "127.0.0.2", "::ffff:127.0.0.1"} {
				_, err := fstoreBegin(t, db, addr, apNow)
				require.NoError(t, err, "loopback begin %d, from %s, while loopback holds %d", i+1, addr, i)
			}
			_, err = fstoreBegin(t, db, "127.0.0.1", apNow)
			fstoreBusyFrom(t, err, "loopback, its share taken")
			_, err = fstoreBegin(t, db, "203.0.113.9", apNow)
			signcapBusyForAll(t, err, "another host, still")
			require.Equal(t, maxWaitingTerminalSignIns+fstorePerAddress, signcapWaiting(t, db, apNow),
				"the queue holds the other hosts' cap and loopback's share, no more")
		})
	}
}

// Loopback's codes take no room from the other hosts: with loopback's share
// waiting, they still fill their whole cap, and past their deadline every code
// makes room again.
func TestTerminalSignIn_LoopbackTakesNoRoomFromOtherHosts(t *testing.T) {
	db := leaseDB(t)
	fstoreFill(t, db, "127.0.0.1", fstorePerAddress, apNow)
	signcapFillOtherHosts(t, db, func(i int) string { return fmt.Sprintf("198.51.100.%d", 1+i) }, apNow)
	_, err := fstoreBegin(t, db, "203.0.113.9", apNow)
	signcapBusyForAll(t, err, "another host")
	_, err = fstoreBegin(t, db, "::1", apNow)
	fstoreBusyFrom(t, err, "loopback, its share taken")
	require.Equal(t, maxWaitingTerminalSignIns+fstorePerAddress, signcapWaiting(t, db, apNow))

	later := apNow.Add(TerminalSignInTTL)
	_, err = fstoreBegin(t, db, "::1", later)
	require.NoError(t, err, "loopback's codes expired")
	_, err = fstoreBegin(t, db, "203.0.113.9", later)
	require.NoError(t, err, "the other hosts' codes expired")
}

// Only a loopback address begins outside the shared cap: no address recorded,
// one that does not parse, the text of loopback's own key, and an unspecified
// address are other hosts, held to it.
func TestTerminalSignIn_OnlyLoopbackBeginsOutsideTheCap(t *testing.T) {
	db := leaseDB(t)
	signcapFillOtherHosts(t, db, func(i int) string { return fmt.Sprintf("198.51.100.%d", 1+i) }, apNow)
	for _, addr := range []string{"", "loopback", "localhost", "127.0.0.1.example", "::2", "0.0.0.0", "::"} {
		_, err := fstoreBegin(t, db, addr, apNow)
		signcapBusyForAll(t, err, fmt.Sprintf("%q", addr))
	}
	_, err := fstoreBegin(t, db, "[::1]:50000", apNow)
	require.NoError(t, err, "loopback with a port is loopback")
}

// signcapBrowserLine is the line console-open --sign-in prints a User-Agent on
// (cmd/seamlessd/signin.go, writeTerminalSignIn), below its "began" and "from"
// lines. Each of those starts with two spaces and pads its label with more.
func signcapBrowserLine(agent string) string { return "  browser  " + agent }

// signcapWrap is line as a terminal cols wide shows it: a row for every cols
// characters (printable ASCII takes a column a character).
func signcapWrap(line string, cols int) []string {
	var rows []string
	for len(line) > cols {
		rows = append(rows, line[:cols])
		line = line[cols:]
	}
	return append(rows, line)
}

// A User-Agent padded with runs of spaces cannot wrap into a row of its own
// that reads as one of the lines printed with it -- here a second "from" line
// that claims this computer. A cleaned User-Agent keeps each run of spaces as
// one, so no row it wraps into starts with the two spaces those lines start
// with, at any terminal width. The begin's answer, the lookup the terminal
// reads, and the row itself all keep it so, and a row the begin did not write
// reads back the same way.
func TestTerminalSignIn_UserAgentCannotPoseAsALine(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	const head, tail = "Mozilla/5.0 (X11; Linux x86_64) Example/1.0", "  from     127.0.0.1 (this computer)"
	forged := head + strings.Repeat(" ", 80-len(signcapBrowserLine(head))) + tail
	require.Equal(t, tail, signcapWrap(signcapBrowserLine(forged), 80)[1], "as sent, it wraps into a from line of its own")

	row, code, err := BeginTerminalSignIn(ctx, db, "cookie-pad", "key-pad",
		TerminalSignInBrowser{Addr: "203.0.113.9", UserAgent: forged}, apNow)
	require.NoError(t, err)
	var stored string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT user_agent FROM console_signins WHERE id = ?`, row.ID).Scan(&stored))

	_, raw, err := BeginTerminalSignIn(ctx, db, "cookie-raw", "key-raw", TerminalSignInBrowser{}, apNow)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE console_signins SET remote_addr = ?, user_agent = ? WHERE remote_addr = '' AND user_agent = ''`,
		"192.0.2.44"+strings.Repeat(" ", 60)+"(this computer)", forged)
	require.NoError(t, err)
	rawBrowser := signin3ByCode(t, db, raw)

	const want = "Mozilla/5.0 (X11; Linux x86_64) Example/1.0 from 127.0.0.1 (this computer)"
	for _, got := range []struct{ what, agent string }{
		{"the begin's answer", row.Browser.UserAgent},
		{"the lookup by code", signin3ByCode(t, db, code).UserAgent},
		{"the row", stored},
		{"a row the begin did not write", rawBrowser.UserAgent},
	} {
		require.NotContains(t, signcapWrap(signcapBrowserLine(got.agent), 80), tail, "%s: no row of it at 80 columns is the from line", got.what)
		for cols := len(signcapBrowserLine("")) + 1; cols <= 2*MaxTerminalSignInUserAgent; cols++ {
			for i, r := range signcapWrap(signcapBrowserLine(got.agent), cols)[1:] {
				require.False(t, strings.HasPrefix(r, "  "), "%s, %d columns: row %d is %q", got.what, cols, i+2, r)
			}
		}
		require.Equal(t, want, got.agent, got.what)
	}
	require.Equal(t, "192.0.2.44 (this computer)", rawBrowser.Addr, "the address is cleaned the same way")
}

// A run of spaces is kept as one space wherever it is, after the fields are
// trimmed and before they are cut: a control character in a run is a '?' that
// ends it, and a run kept as one counts toward the maximum as one.
func TestTerminalSignIn_UserAgentSpacesKeptAsOne(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"single spaces", "Mozilla/5.0 (Macintosh) Example/17.0", "Mozilla/5.0 (Macintosh) Example/17.0"},
		{"runs inside, trimmed outside", "  Example   /   1.0  ", "Example / 1.0"},
		{"a tab ends a run", "Example/1.0 \t\t  Next", "Example/1.0 ?? Next"},
		{"no-break spaces are not ASCII", "Example\u00a0\u00a0 Next", "Example?? Next"},
		{"a megabyte of spaces between", "A" + strings.Repeat(" ", 1<<20) + "B", "A B"},
		{"nothing but spaces", strings.Repeat(" ", 1<<10), ""},
	} {
		require.Equal(t, tc.want, TerminalSignInBrowser{UserAgent: tc.in}.Clean().UserAgent, tc.name)
	}

	long := TerminalSignInBrowser{UserAgent: strings.Repeat("A     ", MaxTerminalSignInUserAgent)}.Clean()
	require.Equal(t, strings.Repeat("A ", MaxTerminalSignInUserAgent)[:MaxTerminalSignInUserAgent-3]+"...", long.UserAgent,
		"cut at the maximum of what is kept, not of what was sent")
	whole := strings.Repeat("A ", MaxTerminalSignInUserAgent/2-1) + "AB"
	require.Len(t, whole, MaxTerminalSignInUserAgent)
	require.Equal(t, whole, TerminalSignInBrowser{UserAgent: strings.ReplaceAll(whole, " ", "    ")}.Clean().UserAgent,
		"what fits once its runs are one is kept whole")
}
