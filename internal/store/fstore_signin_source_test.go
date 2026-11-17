package store

import (
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
		require.Equal(t, tc.want, peerSource(tc.addr), "%q", tc.addr)
	}
}
