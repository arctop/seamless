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
}

// fstoreBegin begins a terminal sign-in from addr at now, for a browser of its
// own (a fresh cookie and key), and returns what began and the begin's error.
func fstoreBegin(t *testing.T, db *sql.DB, addr string, now time.Time) (fstoreBegun, error) {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	cookie := "fstore-cookie-" + id
	row, code, err := BeginTerminalSignIn(context.Background(), db, cookie, "fstore-key-"+id,
		TerminalSignInBrowser{Addr: addr, UserAgent: "Mozilla/5.0 Example/1.0"}, now)
	return fstoreBegun{row: row, code: code, cookie: cookie}, err
}

// fstoreFill begins n terminal sign-ins from addr at now, each of which must
// begin.
func fstoreFill(t *testing.T, db *sql.DB, addr string, n int, now time.Time) []fstoreBegun {
	t.Helper()
	out := make([]fstoreBegun, 0, n)
	for i := range n {
		b, err := fstoreBegin(t, db, addr, now)
		require.NoError(t, err, "begin %d from %s", i+1, addr)
		out = append(out, b)
	}
	return out
}

// fstoreBusyFrom asserts err is the per-address refusal: a busy answer that
// says it is this address's, and what to do.
func fstoreBusyFrom(t *testing.T, err error, addr string) {
	t.Helper()
	require.ErrorIs(t, err, ErrTerminalSignInBusy, "from %s", addr)
	require.ErrorContains(t, err, "from this address", "from %s", addr)
	require.ErrorContains(t, err, "use a code a page already shows, or wait until one expires", "from %s", addr)
}

// One address holds a few waiting codes, not every one: the next begin from it
// is refused and says why, and a begin from another address -- the owner's --
// still finds room.
func TestTerminalSignIn_CappedPerAddress(t *testing.T) {
	db := leaseDB(t)
	fstoreFill(t, db, "192.0.2.10", fstorePerAddress, apNow)
	_, err := fstoreBegin(t, db, "192.0.2.10", apNow)
	fstoreBusyFrom(t, err, "192.0.2.10")
	_, err = fstoreBegin(t, db, "::ffff:192.0.2.10", apNow)
	fstoreBusyFrom(t, err, "the same address, IPv4-mapped")
	_, err = fstoreBegin(t, db, "192.0.2.10:51234", apNow)
	fstoreBusyFrom(t, err, "the same address with a port")

	_, err = fstoreBegin(t, db, "192.0.2.11", apNow)
	require.NoError(t, err, "another address still begins one")
	_, err = fstoreBegin(t, db, "2001:db8::10", apNow)
	require.NoError(t, err, "an IPv6 address is another address")

	var waiting int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM console_signins WHERE `+waitingTerminalSignIn,
		core.FormatTime(apNow.Add(-TerminalSignInTTL))).Scan(&waiting))
	require.Equal(t, fstorePerAddress+2, waiting, "a refused begin stores nothing")
}

// One host may hold a whole IPv6 /64 and pick a fresh address from it for
// every request, so an IPv6 address counts by its /64; the next /64 is
// another address.
func TestTerminalSignIn_CappedPerIPv6Slash64(t *testing.T) {
	db := leaseDB(t)
	for i := range fstorePerAddress {
		_, err := fstoreBegin(t, db, fmt.Sprintf("2001:db8:1:2::%x", 0xa+i), apNow)
		require.NoError(t, err, "begin %d", i+1)
	}
	for _, addr := range []string{"2001:db8:1:2:ffff:ffff:ffff:ffff", "2001:db8:1:2::1", "[2001:db8:1:2::77]:443"} {
		_, err := fstoreBegin(t, db, addr, apNow)
		fstoreBusyFrom(t, err, addr)
	}
	_, err := fstoreBegin(t, db, "2001:db8:1:3::a", apNow)
	require.NoError(t, err, "the next /64 is another address")
	_, err = fstoreBegin(t, db, "2001:db8:1:2::a", apNow.Add(TerminalSignInTTL))
	require.NoError(t, err, "the /64's codes expired")
}

// Every loopback address is this computer: one address, however many of
// 127.0.0.0/8 and ::1 a caller uses.
func TestTerminalSignIn_LoopbackIsOneAddress(t *testing.T) {
	db := leaseDB(t)
	for _, addr := range []string{"127.0.0.1", "127.0.0.2", "::1", "127.9.9.9"} {
		_, err := fstoreBegin(t, db, addr, apNow)
		require.NoError(t, err, addr)
	}
	_, err := fstoreBegin(t, db, "127.0.0.3", apNow)
	fstoreBusyFrom(t, err, "127.0.0.3")
	_, err = fstoreBegin(t, db, "::1", apNow)
	fstoreBusyFrom(t, err, "::1")
}

// The global cap still holds: codes from many addresses, each within its
// share, fill it, and then a begin from an address with none waiting is
// refused as busy, not as that address's.
func TestTerminalSignIn_GlobalCapStillHolds(t *testing.T) {
	db := leaseDB(t)
	for i := range maxWaitingTerminalSignIns {
		addr := fmt.Sprintf("198.51.100.%d", 1+i/fstorePerAddress)
		_, err := fstoreBegin(t, db, addr, apNow)
		require.NoError(t, err, "begin %d from %s", i+1, addr)
	}
	_, err := fstoreBegin(t, db, "203.0.113.200", apNow)
	require.ErrorIs(t, err, ErrTerminalSignInBusy)
	require.NotContains(t, err.Error(), "from this address", "an address with none waiting is refused for the whole queue")
	_, err = fstoreBegin(t, db, "203.0.113.200", apNow.Add(TerminalSignInTTL))
	require.NoError(t, err, "past their deadline the codes make room")
}

// Only codes still waiting count against an address: not one past its
// deadline (purged at the next begin), not one ended (its browser began again
// or signed out), and not one approved (a live sign-in now).
func TestTerminalSignIn_PerAddressCountsOnlyWaiting(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	const addr = "192.0.2.20"
	fstoreFill(t, db, addr, fstorePerAddress, apNow)
	_, err := fstoreBegin(t, db, addr, apNow.Add(TerminalSignInTTL-time.Second))
	fstoreBusyFrom(t, err, "a second before the deadline")
	fresh := fstoreFill(t, db, addr, fstorePerAddress, apNow.Add(TerminalSignInTTL))
	_, err = fstoreBegin(t, db, addr, apNow.Add(TerminalSignInTTL))
	fstoreBusyFrom(t, err, "full again")

	later := apNow.Add(TerminalSignInTTL + time.Minute)
	n, err := EndConsoleSignInByCookie(ctx, db, fresh[0].cookie, later)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	_, err = fstoreBegin(t, db, addr, later)
	require.NoError(t, err, "an ended code makes room")

	_, err = ApproveTerminalSignIn(ctx, db, fresh[1].row.ID, fresh[1].code, time.Hour, later)
	require.NoError(t, err)
	_, err = fstoreBegin(t, db, addr, later)
	require.NoError(t, err, "an approved code is a sign-in now, and makes room")
	_, err = fstoreBegin(t, db, addr, later)
	fstoreBusyFrom(t, err, "full once more")
}
