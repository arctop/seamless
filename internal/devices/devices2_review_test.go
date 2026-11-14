package devices

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// Tests for the second security review's device findings (wave nt1113f):
// L5's server half (no pairing over plain http from another computer), I1
// (the revocation window between the edge's lookup and its registration of
// the request), and the one-liners that carry the certificate's fingerprint.

// devices2PairBody is a key device's pairing request named name with code,
// as a client sends it, and the secret it chose.
func devices2PairBody(t *testing.T, code, name string) (string, string) {
	t.Helper()
	secret := pairSecret(t)
	raw, err := json.Marshal(pairRequest{Code: code, Kind: KindKey, Name: name, Secret: secret})
	require.NoError(t, err)
	return string(raw), secret
}

// devices2Probe is what reached the route behind the edge: whether anything
// did, and with which credential.
type devices2Probe struct {
	reached bool
	cred    Credential
	paired  bool
	ctxErr  error
}

// devices2Serve sends r through svc's edge, in front of a route that records
// what reached it, and returns the answer. lookedUp is the edge's seam
// between its lookup and its registration of the request (nil: Edge's).
func devices2Serve(svc *Service, r *http.Request, lookedUp func(Credential)) (*httptest.ResponseRecorder, devices2Probe) {
	var p devices2Probe
	h := svc.edge(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.reached = true
		p.cred, p.paired = CredentialOf(r.Context())
		p.ctxErr = r.Context().Err()
		w.WriteHeader(http.StatusOK)
	}), lookedUp)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr, p
}

// devices2Refusal reads a refused pairing's answer.
func devices2Refusal(t *testing.T, rr *httptest.ResponseRecorder) pairError {
	t.Helper()
	var out pairError
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out), rr.Body.String())
	return out
}

// A pairing that arrives over plain http from another computer is refused
// before the pairing reads it (review L5): its code is not spent, and no key
// leaves the server. Over TLS it pairs, and loopback over plain http pairs as
// before (the tests, a proxy on this computer, a same-machine client).
func TestDevices2Edge_PlainHTTPPairsOnlyFromLoopback(t *testing.T) {
	f := pairNewFixture(t, false)
	code := f.pairCode(t)
	body, secret := devices2PairBody(t, code, "example-ci")
	for _, peer := range []string{"192.0.2.7:51000", "[2001:db8::7]:51000", "100.101.102.103:51000", "[fd7a:115c:a1e0::1]:51000",
		"[::ffff:192.0.2.7]:51000", "not-an-address"} {
		for path, b := range map[string]string{PairPath: body, PairStatusPath: `{"secret":"` + secret + `"}`} {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(b))
			req.RemoteAddr = peer
			rr, p := devices2Serve(f.svc, req, nil)
			require.Equal(t, http.StatusForbidden, rr.Code, "%s %s: %s", peer, path, rr.Body.String())
			require.False(t, p.reached)
			refused := devices2Refusal(t, rr)
			require.Equal(t, "tls", refused.Reason, peer)
			require.Contains(t, refused.Error, "plain http", "it says why")
			require.Contains(t, refused.Error, "seamlessd setup's address step", "and what to do")
			require.NotContains(t, rr.Body.String(), KeyPrefix, "no key leaves the server")
			require.Equal(t, "no-store", rr.Header().Get("Cache-Control"))
		}
	}
	devices, unused := f.pairCount(t)
	require.Zero(t, devices, "nothing paired")
	require.Equal(t, 1, unused, "refused before the pairing read it, the code is not spent")

	// Over TLS, from another computer: the same request pairs.
	req := httptest.NewRequest(http.MethodPost, PairPath, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.7:51000"
	req.TLS = &tls.ConnectionState{HandshakeComplete: true}
	rr, _ := devices2Serve(f.svc, req, nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var answer PairAnswer
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &answer))
	require.Equal(t, StatusPaired, answer.Status)
	require.True(t, IsKey(answer.Key))

	// Loopback over plain http pairs, IPv4, IPv6 and IPv4-mapped alike.
	for i, peer := range []string{"127.0.0.1:51000", "[::1]:51000", "[::ffff:127.0.0.1]:51000"} {
		body, _ := devices2PairBody(t, f.pairCode(t), []string{"example-one", "example-two", "example-three"}[i])
		req := httptest.NewRequest(http.MethodPost, PairPath, strings.NewReader(body))
		req.RemoteAddr = peer
		rr, _ := devices2Serve(f.svc, req, nil)
		require.Equal(t, http.StatusOK, rr.Code, "%s: %s", peer, rr.Body.String())
	}
}

// A server whose address is plain http to another host needs TLS before any
// computer can pair with it: the client refuses plain http off loopback, and
// the edge a pairing over it from another computer. Loopback, an https
// address (the server's TLS, or a proxy's in front of it) and TLS pair.
func TestDevices2_PairingNeedsTLSWhereNoComputerCanPair(t *testing.T) {
	for _, tc := range []struct {
		facts    ServerFacts
		needsTLS bool
	}{
		{ServerFacts{URL: "http://192.0.2.10:8081"}, true},
		{ServerFacts{URL: "http://example-server.example:8081"}, true},
		{ServerFacts{URL: " HTTP://[2001:db8::10]:8081/"}, true},
		{ServerFacts{URL: "http://127.0.0.1:8081"}, false},
		{ServerFacts{URL: "http://[::1]:8081"}, false},
		{ServerFacts{URL: "http://localhost:8081"}, false},
		{ServerFacts{URL: "https://example-server.example.ts.net"}, false},
		{ServerFacts{URL: "https://example-server.example.ts.net:8081", TLS: true, CertFingerprint: CertFingerprint([]byte("x"))}, false},
		{ServerFacts{}, false},
	} {
		require.Equal(t, tc.needsTLS, tc.facts.PairingNeedsTLS(), tc.facts.URL)
	}
	require.Contains(t, ErrPairingNeedsTLS.Error(), "seamlessd setup's address step", "it says what to do")
}

// A device revoked after the edge's lookup read it active, and before the
// edge registered the request, was revoked by a revocation that could not end
// a request it did not know of (review I1). The request is refused all the
// same, as one that arrived after the revocation: a relay with 401, a key
// device passed on unmarked for the routes to refuse as a wrong key. A
// revocation of another device in the same window changes nothing.
func TestDevices2Edge_RevokedBetweenLookupAndRegistration(t *testing.T) {
	f := pairNewFixture(t, false)
	keyDev, key := f.pairKeyDevice(t, "example-ci")
	other, otherKey := f.pairKeyDevice(t, "example-other")
	id := pairIdentity(t, "example-laptop")
	relayDev := f.pairRelayDevice(t, "example-laptop", id)

	var revoked []string
	victim := ""
	revokeInTheWindow := func(c Credential) {
		// The lookup read the device active, and this request is not
		// registered yet: a revocation now ends every request it knows of,
		// and this one is not among them.
		f.svc.mu.Lock()
		_, registered := f.svc.live[c.DeviceID]
		f.svc.mu.Unlock()
		require.False(t, registered, "the seam runs before the registration")
		target := victim
		if target == "" {
			target = c.DeviceID
		}
		_, err := f.svc.Revoke(context.Background(), target, store.DeviceByConsole)
		require.NoError(t, err)
		revoked = append(revoked, target)
	}

	keyReq := func(k string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/mcp", nil)
		r.Header.Set("Authorization", "Bearer "+k)
		return r
	}

	// Another device revoked in the window: this one is served as itself.
	victim = keyDev.ID
	rr, p := devices2Serve(f.svc, keyReq(otherKey), revokeInTheWindow)
	require.Equal(t, http.StatusOK, rr.Code)
	require.True(t, p.paired, "another device's revocation ends nothing of this one's")
	require.Equal(t, other.ID, p.cred.DeviceID)
	require.NoError(t, p.ctxErr)

	// Its own revocation in the window: a key device is no credential.
	victim = ""
	rr, p = devices2Serve(f.svc, keyReq(otherKey), revokeInTheWindow)
	require.Equal(t, []string{keyDev.ID, other.ID}, revoked)
	require.True(t, p.reached, "a revoked key passes on unmarked, as a wrong key does")
	require.False(t, p.paired, "a key revoked before its request was registered is no credential")
	require.Equal(t, http.StatusOK, rr.Code, "the probe answers; a real route refuses the key")

	// A relay revoked in the window is refused, its connection closed.
	relayReq := httptest.NewRequest(http.MethodGet, "/api/mcp", nil)
	relayReq.TLS = &tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{id.cert.Leaf}}
	rr, p = devices2Serve(f.svc, relayReq, revokeInTheWindow)
	require.Equal(t, []string{keyDev.ID, other.ID, relayDev.ID}, revoked)
	require.Equal(t, http.StatusUnauthorized, rr.Code)
	require.Contains(t, rr.Body.String(), ErrRelayRevoked.Error())
	require.Equal(t, "close", rr.Header().Get("Connection"))
	require.False(t, p.reached, "nothing reaches a route")
	require.Empty(t, f.svc.liveDevices(), "no request stays registered")

	// After the revocation the lookup itself refuses: the seam never runs.
	_, p = devices2Serve(f.svc, keyReq(key), func(Credential) { t.Fatal("a revoked device passed the lookup") })
	require.False(t, p.paired)

	// Without a revocation in the window a device is served as itself.
	live, liveKey := f.pairKeyDevice(t, "example-live")
	_, p = devices2Serve(f.svc, keyReq(liveKey), func(Credential) {})
	require.True(t, p.paired)
	require.Equal(t, live.ID, p.cred.DeviceID)
	require.NoError(t, p.ctxErr)
}

// The one-liners carry the server certificate's fingerprint whenever the
// server serves TLS, so an unattended Client compares it before it sends the
// code; without TLS (loopback only) they are as they were.
func TestDevices2_ClientCommandsCarryTheFingerprint(t *testing.T) {
	fp := CertFingerprint([]byte("example certificate"))
	require.True(t, ValidCertFingerprint(fp))
	unix, windows := ClientCommands(ServerFacts{URL: "https://example-server.example.ts.net:8081", TLS: true, CertFingerprint: fp},
		"ABCD-EFGH-JKMN-PQRS")
	require.Equal(t, "curl -fsSL https://thereisnospoon.org/install | SEAMLESS_SETUP=client "+
		"SEAMLESS_SERVER_URL=https://example-server.example.ts.net:8081 SEAMLESS_SERVER_FINGERPRINT="+fp+
		" SEAMLESS_PAIR_CODE=ABCD-EFGH-JKMN-PQRS sh", unix)
	require.Equal(t, "$env:SEAMLESS_SETUP='client'; $env:SEAMLESS_SERVER_URL='https://example-server.example.ts.net:8081'; "+
		"$env:SEAMLESS_SERVER_FINGERPRINT='"+fp+"'; $env:SEAMLESS_PAIR_CODE='ABCD-EFGH-JKMN-PQRS'; "+
		"irm https://thereisnospoon.org/install.ps1 | iex", windows)

	unix, windows = ClientCommands(ServerFacts{URL: "http://127.0.0.1:8081"}, "ABCD-EFGH-JKMN-PQRS")
	require.Equal(t, "curl -fsSL https://thereisnospoon.org/install | SEAMLESS_SETUP=client SEAMLESS_SERVER_URL=http://127.0.0.1:8081 "+
		"SEAMLESS_PAIR_CODE=ABCD-EFGH-JKMN-PQRS sh", unix)
	require.Equal(t, "$env:SEAMLESS_SETUP='client'; $env:SEAMLESS_SERVER_URL='http://127.0.0.1:8081'; "+
		"$env:SEAMLESS_PAIR_CODE='ABCD-EFGH-JKMN-PQRS'; irm https://thereisnospoon.org/install.ps1 | iex", windows)

	// A proxy's https address: the daemon knows no certificate to name.
	unix, _ = ClientCommands(ServerFacts{URL: "https://example-server.example.ts.net"}, "<code>")
	require.NotContains(t, unix, "FINGERPRINT")
}
