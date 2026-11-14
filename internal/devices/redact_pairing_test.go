package devices

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// An address that names a user is judged by what the shown form would take
// out of it: a password, a user alone (a token is often given that way), an
// empty user, and a value url.Parse refuses whose authority holds an "@".
// A bare address, a socket and no address name none.
func TestRedact_PairingNamesUser(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://alice:s3cret@example-server.example:8081":  true,
		"http://alice@192.0.2.10:8081":                      true,
		"https://example-token-0123@example-server.example": true,
		"https://@example-server.example:8081":              true,
		" HTTPS://alice:s3cret@example-server.example/":     true,
		"http://alice:s3 cret@example-server.example:8081":  true,
		"https://example-server.example.ts.net:8081":        false,
		"http://127.0.0.1:8081":                             false,
		"unix:///opt/seamless/run/seamlessd.sock":           false,
		"%": false,
		"":  false,
	} {
		require.Equal(t, want, ServerFacts{URL: raw}.PairingNamesUser(), "%q", raw)
	}
}

// PairingRefusal names every reason this server mints no code, joined: a
// user in the address, plain http to another host, or both. An https
// address, and loopback over plain http, are refused nothing.
func TestRedact_PairingRefusal(t *testing.T) {
	user := ServerFacts{URL: "https://alice:s3cret@example-server.example:8081", TLS: true, CertFingerprint: CertFingerprint([]byte("x"))}
	err := user.PairingRefusal()
	require.ErrorIs(t, err, ErrPairingNamesUser)
	require.NotErrorIs(t, err, ErrPairingNeedsTLS)
	require.ErrorContains(t, err, "server_url names a user: remove it, Seamless uses none")
	require.NotContains(t, err.Error(), "s3cret")

	both := ServerFacts{URL: "http://alice:s3cret@192.0.2.10:8081"}
	err = both.PairingRefusal()
	require.ErrorIs(t, err, ErrPairingNamesUser)
	require.ErrorIs(t, err, ErrPairingNeedsTLS, "both reasons, so one pass fixes both")

	require.ErrorIs(t, ServerFacts{URL: "http://alice@127.0.0.1:8081"}.PairingRefusal(), ErrPairingNamesUser,
		"loopback pairs on this computer, but not with a user in its address")

	tlsOnly := ServerFacts{URL: "http://192.0.2.10:8081"}.PairingRefusal()
	require.ErrorIs(t, tlsOnly, ErrPairingNeedsTLS)
	require.Equal(t, ErrPairingNeedsTLS.Error(), tlsOnly.Error(), "the TLS refusal reads as before")

	for _, ok := range []ServerFacts{
		{URL: "https://example-server.example.ts.net:8081", TLS: true, CertFingerprint: CertFingerprint([]byte("x"))},
		{URL: "https://example-server.example.ts.net"},
		{URL: "http://127.0.0.1:8081"},
		{},
	} {
		require.NoError(t, ok.PairingRefusal(), ok.URL)
	}
}

// Shown takes the user out of the address and nothing else, and the
// one-liners never carry a user, even from a caller that skipped the
// refusal.
func TestRedact_ShownAndClientCommandsCarryNoUser(t *testing.T) {
	fp := CertFingerprint([]byte("example certificate"))
	facts := ServerFacts{URL: "https://alice:s3cret@example-server.example:8081", TLS: true, CertFingerprint: fp}
	require.Equal(t, ServerFacts{URL: "https://example-server.example:8081", TLS: true, CertFingerprint: fp}, facts.Shown())
	require.Equal(t, "https://alice:s3cret@example-server.example:8081", facts.URL, "Shown copies")

	unix, windows := ClientCommands(facts, "ABCD-EFGH-JKMN-PQRS")
	for _, line := range []string{unix, windows} {
		require.NotContains(t, line, "s3cret")
		require.NotContains(t, line, "alice")
	}
	require.Contains(t, unix, "SEAMLESS_SERVER_URL=https://example-server.example:8081 SEAMLESS_SERVER_FINGERPRINT="+fp+" ")
	require.Contains(t, windows, "$env:SEAMLESS_SERVER_URL='https://example-server.example:8081'; ")

	bare := ServerFacts{URL: "https://example-server.example.ts.net:8081", TLS: true, CertFingerprint: fp}
	require.Equal(t, bare, bare.Shown(), "an address with no user is shown as configured")
	unix, _ = ClientCommands(bare, "<code>")
	require.Contains(t, unix, "SEAMLESS_SERVER_URL=https://example-server.example.ts.net:8081 SEAMLESS_SERVER_FINGERPRINT=")
}
