package main

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The address a terminal sign-in came from is placed honestly: loopback may
// be a proxy in front of the console, one of this computer's own addresses is
// this computer, any other is another computer, and when this computer's
// addresses cannot be read the address stands alone.
func TestSignInAddrWords(t *testing.T) {
	own := func() ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("198.51.100.7"), netip.MustParseAddr("2001:db8::7")}, nil
	}
	for addr, want := range map[string]string{
		"":                    "not recorded",
		"127.0.0.1":           "127.0.0.1 (this computer, or a proxy in front of the console)",
		"::1":                 "::1 (this computer, or a proxy in front of the console)",
		"198.51.100.7":        "198.51.100.7 (this computer)",
		"::ffff:198.51.100.7": "::ffff:198.51.100.7 (this computer)",
		"2001:db8::7":         "2001:db8::7 (this computer)",
		"192.0.2.44":          "192.0.2.44 (another computer)",
		"not-an-address?":     "not-an-address?",
	} {
		require.Equal(t, want, signInAddrWords(addr, own), addr)
	}
	unreadable := func() ([]netip.Addr, error) { return nil, errors.New("no interfaces") }
	require.Equal(t, "192.0.2.44", signInAddrWords("192.0.2.44", unreadable))
	require.Equal(t, "127.0.0.1 (this computer, or a proxy in front of the console)", signInAddrWords("127.0.0.1", unreadable))
	_, err := thisComputersAddrs()
	require.NoError(t, err, "this computer's own addresses read")
}

func TestAgoWords(t *testing.T) {
	require.Equal(t, "less than a minute ago", agoWords(59*time.Second))
	require.Equal(t, "less than a minute ago", agoWords(-time.Second))
	require.Equal(t, "1 minute ago", agoWords(time.Minute+30*time.Second))
	require.Equal(t, "9 minutes ago", agoWords(9*time.Minute+59*time.Second))
}
