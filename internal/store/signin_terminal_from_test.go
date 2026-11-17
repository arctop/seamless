package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/stretchr/testify/require"
)

// fstorePerAddress is how many terminal sign-ins may wait from one address,
// as the docs state it (maxWaitingTerminalSignInsFrom; pinned in
// fstore_signin_source_test.go).
const fstorePerAddress = 4

// fstoreBegun is one terminal sign-in fstoreBegin began.
type fstoreBegun struct {
	row    WaitingTerminalSignIn
	code   string
	cookie string
	key    string
}

// fstoreBegin begins a terminal sign-in from addr one second after the one
// before it (*at), for a browser of its own (a fresh cookie and key), so the
// rows' order is their beginning's. A begin never waits for room: every one
// begins.
func fstoreBegin(t *testing.T, db *sql.DB, at *time.Time, addr string) fstoreBegun {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	*at = at.Add(time.Second)
	cookie, key := "fstore-cookie-"+id, "fstore-key-"+id
	row, code, err := BeginTerminalSignIn(context.Background(), db, cookie, key,
		TerminalSignInBrowser{Addr: addr, UserAgent: "Mozilla/5.0 Example/1.0"}, *at)
	require.NoError(t, err, "a begin from %q never waits for room", addr)
	return fstoreBegun{row: row, code: code, cookie: cookie, key: key}
}

// fstoreFill begins n terminal sign-ins from addr, each a second after the
// one before it.
func fstoreFill(t *testing.T, db *sql.DB, at *time.Time, addr string, n int) []fstoreBegun {
	t.Helper()
	out := make([]fstoreBegun, 0, n)
	for range n {
		out = append(out, fstoreBegin(t, db, at, addr))
	}
	return out
}

// fstoreWaits reports whether b's code still waits at now, and asserts that
// the page that showed it sees the same.
func fstoreWaits(t *testing.T, db *sql.DB, b fstoreBegun, now time.Time) bool {
	t.Helper()
	ctx := context.Background()
	_, state, perr := TerminalSignInFor(ctx, db, b.row.ID, HashConsoleSignIn(b.cookie, b.key), now)
	w, err := TerminalSignInByCode(ctx, db, b.code, now)
	if err != nil {
		require.ErrorIs(t, err, ErrTerminalSignInNotFound)
		require.ErrorIs(t, perr, ErrTerminalSignInNotFound, "the page finds nothing waiting either")
		return false
	}
	require.NoError(t, perr)
	require.Equal(t, TerminalSignInWaiting, state)
	require.Equal(t, b.row.ID, w.ID)
	return true
}

// fstoreWaiting counts the terminal sign-ins waiting at now from source
// (peerSource), or, with source "", from every address but loopback.
func fstoreWaiting(t *testing.T, db *sql.DB, now time.Time, source string) int {
	t.Helper()
	rows, err := db.Query(`SELECT remote_addr FROM console_signins WHERE `+waitingTerminalSignIn,
		core.FormatTime(now.Add(-TerminalSignInTTL)))
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var addr string
		require.NoError(t, rows.Scan(&addr))
		s := peerSource(addr)
		if s == source || (source == "" && s != peerSourceLoopback) {
			n++
		}
	}
	require.NoError(t, rows.Err())
	return n
}

// One address holds at most a few waiting codes, and a begin past that
// displaces the address's oldest -- however the address is written -- whose
// code then approves nothing and whose page finds nothing waiting. Someone
// looping on the begin from one host, however long, never displaces the
// owner's code shown from another address, which the owner still approves.
// The control is a loop from the owner's own address -- a proxy in front of
// the console makes every request one -- which does.
func TestTerminalSignIn_PerAddressDisplacesItsOwn(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	at := apNow
	owners := fstoreBegin(t, db, &at, "198.51.100.7")
	loop := fstoreFill(t, db, &at, "192.0.2.10", fstorePerAddress)

	fstoreBegin(t, db, &at, "::ffff:192.0.2.10")
	require.False(t, fstoreWaits(t, db, loop[0], at), "the address's oldest made room, IPv4-mapped")
	_, err := ApproveTerminalSignIn(ctx, db, loop[0].row.ID, loop[0].code, time.Hour, at)
	require.ErrorIs(t, err, ErrTerminalSignInNotFound, "a displaced code approves nothing")
	fstoreBegin(t, db, &at, "192.0.2.10:51234")
	require.False(t, fstoreWaits(t, db, loop[1], at), "the address's next oldest made room, with a port")
	require.True(t, fstoreWaits(t, db, loop[2], at))
	require.Equal(t, fstorePerAddress, fstoreWaiting(t, db, at, "192.0.2.10"))

	fstoreFill(t, db, &at, "192.0.2.10", 3*maxWaitingTerminalSignIns)
	require.True(t, fstoreWaits(t, db, owners, at), "a loop from another address never displaces the owner's code")
	require.Equal(t, fstorePerAddress, fstoreWaiting(t, db, at, "192.0.2.10"))
	require.Equal(t, fstorePerAddress+1, signcapWaiting(t, db, at), "the table holds the loop's share and the owner's")
	_, err = ApproveTerminalSignIn(ctx, db, owners.row.ID, owners.code, time.Hour, at)
	require.NoError(t, err)

	// The control: from the owner's own address the loop does displace it.
	owners = fstoreBegin(t, db, &at, "198.51.100.7")
	fstoreFill(t, db, &at, "198.51.100.7", fstorePerAddress)
	require.False(t, fstoreWaits(t, db, owners, at), "a loop from the same address crowds it out")
}

// One host may hold a whole IPv6 /64 and pick a fresh address from it for
// every request, so an IPv6 address counts by its /64 and displaces its /64's
// oldest; the next /64 is another address.
func TestTerminalSignIn_PerIPv6Slash64DisplacesItsOwn(t *testing.T) {
	db := leaseDB(t)
	at := apNow
	var slash64 []fstoreBegun
	for i := range fstorePerAddress {
		slash64 = append(slash64, fstoreBegin(t, db, &at, fmt.Sprintf("2001:db8:1:2::%x", 0xa+i)))
	}
	next := fstoreBegin(t, db, &at, "2001:db8:1:3::a")
	for i, addr := range []string{"2001:db8:1:2:ffff:ffff:ffff:ffff", "2001:db8:1:2::1", "[2001:db8:1:2::77]:443"} {
		fstoreBegin(t, db, &at, addr)
		require.False(t, fstoreWaits(t, db, slash64[i], at), "from %s: the /64's oldest made room", addr)
		require.Equal(t, fstorePerAddress, fstoreWaiting(t, db, at, "2001:db8:1:2::/64"))
	}
	require.True(t, fstoreWaits(t, db, next, at), "the next /64 is another address")
	require.True(t, fstoreWaits(t, db, slash64[3], at))
}

// Every loopback address is this computer: one address, however many of
// 127.0.0.0/8 and ::1 a caller uses.
func TestTerminalSignIn_LoopbackIsOneAddress(t *testing.T) {
	db := leaseDB(t)
	at := apNow
	var server []fstoreBegun
	for _, addr := range []string{"127.0.0.1", "127.0.0.2", "::1", "127.9.9.9"} {
		server = append(server, fstoreBegin(t, db, &at, addr))
	}
	fstoreBegin(t, db, &at, "127.0.0.3")
	require.False(t, fstoreWaits(t, db, server[0], at))
	fstoreBegin(t, db, &at, "::1")
	require.False(t, fstoreWaits(t, db, server[1], at))
	require.True(t, fstoreWaits(t, db, server[2], at))
	require.Equal(t, fstorePerAddress, fstoreWaiting(t, db, at, peerSourceLoopback))
}

// The global cap still holds: codes from many addresses, each within its
// share, fill it, and then a begin from an address with none waiting
// displaces the oldest code of the address holding the most -- the one whose
// oldest is oldest on a tie -- never the owner's, the oldest of all but alone
// at its address. Past their deadline the codes make room.
func TestTerminalSignIn_GlobalCapDisplacesTheFullest(t *testing.T) {
	db := leaseDB(t)
	at := apNow
	owners := fstoreBegin(t, db, &at, "203.0.113.200")
	var fullest fstoreBegun
	for i := range maxWaitingTerminalSignIns - 1 {
		b := fstoreBegin(t, db, &at, fmt.Sprintf("198.51.100.%d", 1+i/fstorePerAddress))
		if i == 0 {
			fullest = b
		}
	}
	require.Equal(t, maxWaitingTerminalSignIns, fstoreWaiting(t, db, at, ""))

	fstoreBegin(t, db, &at, "203.0.113.201")
	require.False(t, fstoreWaits(t, db, fullest, at), "the fullest address with the oldest code gave one up")
	require.True(t, fstoreWaits(t, db, owners, at), "an address holding fewer keeps its code, however old")
	require.Equal(t, maxWaitingTerminalSignIns, fstoreWaiting(t, db, at, ""), "the cap holds")

	later := at.Add(TerminalSignInTTL)
	fstoreBegin(t, db, &later, "203.0.113.202")
	require.Equal(t, 1, signcapWaiting(t, db, later), "past their deadline the codes make room")
}

// Only codes still waiting count against an address: not one ended (its
// browser began again or signed out), and not one approved (a live sign-in
// now, which no begin displaces).
func TestTerminalSignIn_PerAddressCountsOnlyWaiting(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	at := apNow
	const addr = "192.0.2.20"
	codes := fstoreFill(t, db, &at, addr, fstorePerAddress)
	n, err := EndConsoleSignInByCookie(ctx, db, codes[0].cookie, at)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	_, err = ApproveTerminalSignIn(ctx, db, codes[1].row.ID, codes[1].code, time.Hour, at)
	require.NoError(t, err)

	more := fstoreFill(t, db, &at, addr, 2)
	for i, b := range []fstoreBegun{codes[2], codes[3], more[0], more[1]} {
		require.True(t, fstoreWaits(t, db, b, at), "code %d: the ended and the approved ones made room", i)
	}
	fstoreBegin(t, db, &at, addr)
	require.False(t, fstoreWaits(t, db, codes[2], at), "full again: the oldest still waiting makes room")
	_, err = LiveConsoleSignIn(ctx, db, HashConsoleSignIn(codes[1].cookie, codes[1].key), at)
	require.NoError(t, err, "the approved one is a sign-in, never displaced")
}
