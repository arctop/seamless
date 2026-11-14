package devices

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Tests for wave nt1113g's pairing follow-ups (task 01M7CZJ3F7TEHDPAAZJJ5BZ2MA,
// items 4 and 6): one-liners that name this computer's loopback, and the
// client's reading of the edge's tls refusal.

// fdevForwarded is h behind a forwarded port: every request arrives from
// peer, another computer's address, though the client dialed this computer's
// loopback (a container's published port, a TCP forwarder).
func fdevForwarded(peer string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = peer
		h.ServeHTTP(w, r)
	})
}

// An address of this computer's loopback, http or https, is one only a
// client on this computer can dial: the one-liners that carry it are of no
// use to another computer. Any other address, a socket and nothing at all
// are not.
func TestFdev_PairingLoopbackOnly(t *testing.T) {
	for _, tc := range []struct {
		facts    ServerFacts
		loopback bool
	}{
		{ServerFacts{URL: "http://127.0.0.1:8081"}, true},
		{ServerFacts{URL: "https://127.0.0.1:8081", TLS: true, CertFingerprint: CertFingerprint([]byte("x"))}, true},
		{ServerFacts{URL: "http://[::1]:8081"}, true},
		{ServerFacts{URL: "http://localhost:8081"}, true},
		{ServerFacts{URL: " HTTPS://LOCALHOST.:8081/"}, true},
		{ServerFacts{URL: "http://example.localhost:8081"}, true},
		{ServerFacts{URL: "http://127.0.0.2:8081"}, true},
		{ServerFacts{URL: "http://[::ffff:127.0.0.1]:8081"}, true},
		{ServerFacts{URL: "https://example-server.example.ts.net:8081", TLS: true}, false},
		{ServerFacts{URL: "https://example-server.example.ts.net"}, false},
		{ServerFacts{URL: "http://192.0.2.10:8081"}, false},
		{ServerFacts{URL: "http://[2001:db8::10]:8081"}, false},
		{ServerFacts{URL: "unix:///opt/seamless/run/seamlessd.sock"}, false},
		{ServerFacts{URL: "%"}, false},
		{ServerFacts{}, false},
	} {
		require.Equal(t, tc.loopback, tc.facts.PairingLoopbackOnly(), tc.facts.URL)
		if tc.loopback {
			require.False(t, tc.facts.PairingNeedsTLS(), "a loopback address pairs on this computer: %s", tc.facts.URL)
		}
	}
}

// The edge's refusal of a pairing over plain http from another computer
// (reason tls) reads once and names the fix. A client that dialed this
// computer's loopback through a forwarded port passes its own check, and the
// edge, which sees another computer, refuses it unread: the client then says
// what it says of its own refusal (ErrPlainHTTP), not "refused ... refuses".
// Nothing paired, and the code is not spent.
func TestFdev_ClientReadsTheEdgesTLSRefusalOnce(t *testing.T) {
	f := pairNewFixture(t, false)
	code := f.pairCode(t)
	srv := httptest.NewServer(fdevForwarded("192.0.2.7:51000", f.svc.Edge(http.NotFoundHandler())))
	t.Cleanup(srv.Close)
	_, err := Pair(context.Background(), &http.Client{}, srv.URL, PairRequest{Code: code, Name: "example-ci"})
	require.ErrorIs(t, err, ErrPlainHTTP)
	msg := err.Error()
	require.LessOrEqual(t, strings.Count(msg, "refus"), 1, msg)
	require.NotContains(t, msg, "(tls)", msg)
	require.Contains(t, msg, "plain http", msg)
	require.Contains(t, msg, "Give the server TLS, then pair with its https address", msg)
	devices, unused := f.pairCount(t)
	require.Zero(t, devices, "nothing paired")
	require.Equal(t, 1, unused, "the code is not spent")

	// The client's own refusal reads the same, and names the same fix.
	_, err = Pair(context.Background(), &http.Client{Transport: clientNoDial{t}}, "http://192.0.2.7:8081",
		PairRequest{Code: code, Name: "example-ci"})
	require.ErrorIs(t, err, ErrPlainHTTP)
	require.Contains(t, err.Error(), "Give the server TLS, then pair with its https address")
}
