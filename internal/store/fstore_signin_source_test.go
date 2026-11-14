package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// The per-address share is the one the docs state, and the behavior tests
// (signin_terminal_from_test.go) count with.
func TestTerminalSignIn_PerAddressShare(t *testing.T) {
	require.Equal(t, fstorePerAddress, maxWaitingTerminalSignInsFrom)
	require.Less(t, maxWaitingTerminalSignInsFrom, maxWaitingTerminalSignIns, "one address never holds the whole queue")
}

// The address a begin counts against: an IPv4 address by itself, an IPv6
// address by its /64, every loopback address as one, and anything else by its
// text, never equal to an address's.
func TestTerminalSignInSource(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"192.0.2.1", "192.0.2.1"},
		{"::ffff:192.0.2.1", "192.0.2.1"},
		{"192.0.2.1:443", "192.0.2.1"},
		{"[::ffff:192.0.2.1]:443", "192.0.2.1"},
		{"192.0.2.2", "192.0.2.2"},
		{"2001:db8:1:2::1", "2001:db8:1:2::/64"},
		{"2001:db8:1:2:ffff:ffff:ffff:ffff", "2001:db8:1:2::/64"},
		{"[2001:db8:1:2::9]:8443", "2001:db8:1:2::/64"},
		{"2001:db8:1:3::1", "2001:db8:1:3::/64"},
		{"fe80::1%en0", "fe80::/64"},
		{"fe80::2%eth1", "fe80::/64"},
		{"127.0.0.1", "loopback"},
		{"127.255.0.9", "loopback"},
		{"::1", "loopback"},
		{"[::1]:50000", "loopback"},
		{"", "text "},
		{"loopback", "text loopback"},
		{"2001:db8:1:2::/64", "text 2001:db8:1:2::/64"},
		{"192.0.2.44?]0;owned?", "text 192.0.2.44?]0;owned?"},
	} {
		require.Equal(t, tc.want, terminalSignInSource(tc.addr), "%q", tc.addr)
	}
}

// The per-address refusal is its own error, and a busy one: a surface that
// words only ErrTerminalSignInBusy still answers it as busy. The global
// refusal is not the per-address one.
func TestTerminalSignIn_BusyFromIsBusy(t *testing.T) {
	require.ErrorIs(t, ErrTerminalSignInBusyFrom, ErrTerminalSignInBusy)
	require.NotErrorIs(t, ErrTerminalSignInBusy, ErrTerminalSignInBusyFrom)
	require.Equal(t, "too many terminal sign-ins are waiting from this address: use a code a page already shows, "+
		"or wait until one expires", ErrTerminalSignInBusyFrom.Error())

	db := leaseDB(t)
	fstoreFill(t, db, "192.0.2.30", fstorePerAddress, apNow)
	_, err := fstoreBegin(t, db, "192.0.2.30", apNow)
	require.ErrorIs(t, err, ErrTerminalSignInBusyFrom)
	for i := range maxWaitingTerminalSignIns - fstorePerAddress {
		_, err := fstoreBegin(t, db, fmt.Sprintf("198.51.100.%d", 1+i/fstorePerAddress), apNow)
		require.NoError(t, err)
	}
	_, err = fstoreBegin(t, db, "203.0.113.1", apNow)
	require.ErrorIs(t, err, ErrTerminalSignInBusy)
	require.NotErrorIs(t, err, ErrTerminalSignInBusyFrom)
}
