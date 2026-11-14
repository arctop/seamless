package devices

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// deviceGet sends one request through client and returns its status, body and
// what the probe behind the edge saw (ok false when nothing reached it).
func deviceGet(t *testing.T, client *http.Client, target string, header http.Header, seen <-chan deviceProbe) (int, string, deviceProbe, bool) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	require.NoError(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	select {
	case p := <-seen:
		return resp.StatusCode, string(body), p, true
	default:
		return resp.StatusCode, string(body), deviceProbe{}, false
	}
}

// A TLS connection with no client certificate is judged by the bearer key as
// before: the edge marks nothing, strips the relay attribute headers, and
// leaves the request's own host and key alone.
func TestEdge_NoClientCertificateIsJudgedByTheKeyAsBefore(t *testing.T) {
	f := pairNewFixture(t, false)
	seen := make(chan deviceProbe, 4)
	ts, pool, _ := f.deviceServer(t, seen, nil)
	client := deviceClient(pool, nil)

	status, _, p, ok := deviceGet(t, client, ts.URL+"/api/mcp", http.Header{
		"Authorization":          {"Bearer shared-key"},
		"X-Seamless-Host":        {"example-laptop"},
		"X-Seamless-Relay-Uid":   {"0"},
		"X-Seamless-Relay-Chain": {"1:100:boot"},
	}, seen)
	require.Equal(t, http.StatusOK, status)
	require.True(t, ok)
	require.False(t, p.paired, "a bearer request is no paired device")
	require.Empty(t, p.relayHdrs, "a bearer request carrying relay attributes is judged as a bearer request: they never reach a route")
	require.Equal(t, "example-laptop", p.host, "a bearer request names its own host, as before")
	require.Equal(t, "Bearer shared-key", p.auth, "the key is left for the routes to judge")
}

// A relay certificate whose key no device holds is refused, and the
// connection closed; once paired it is the relay, speaking for its enrolled
// name whatever host it names; revoked, refused again.
func TestEdge_RelayCertificateIsMatchedByItsKey(t *testing.T) {
	f := pairNewFixture(t, false)
	seen := make(chan deviceProbe, 8)
	ts, pool, _ := f.deviceServer(t, seen, nil)
	id := pairIdentity(t, "example-laptop")
	client := deviceClient(pool, id)

	status, body, _, reached := deviceGet(t, client, ts.URL+"/api/mcp", nil, seen)
	require.Equal(t, http.StatusUnauthorized, status, "a relay certificate presented without an enrollment")
	require.Contains(t, body, ErrRelayNotPaired.Error())
	require.False(t, reached, "nothing reaches a route")

	d := f.pairRelayDevice(t, "example-laptop", id)
	status, _, p, reached := deviceGet(t, client, ts.URL+"/api/hooks/session-start?client=claude&host=example-ci", http.Header{
		"X-Seamless-Host":          {"example-ci"},
		"X-Seamless-Relay-Version": {"1"},
		"X-Seamless-Relay-Uid":     {"501"},
		"X-Seamless-Relay-Chain":   {judgeChainHeader},
		"Authorization":            {"Bearer not-the-key"},
	}, seen)
	require.Equal(t, http.StatusOK, status)
	require.True(t, reached)
	require.True(t, p.paired)
	require.Equal(t, Credential{DeviceID: d.ID, Kind: KindRelay, Name: "example-laptop"}, p.cred)
	require.Equal(t, "example-laptop", p.host, "the enrolled name is the request's host")
	require.Equal(t, "example-laptop", p.hookHost, "a hook's host parameter too")
	require.Empty(t, p.relayHdrs, "a relay's forwarded headers never reach a route raw")
	require.True(t, p.forwarded, "what the relay forwarded rides the request's context")
	require.Equal(t, judgeAttrs, p.relay)
	used, err := store.DeviceByID(context.Background(), f.db, d.ID)
	require.NoError(t, err)
	require.NotNil(t, used.LastUsedAt, "the device's last use is recorded")

	_, err = f.svc.Revoke(context.Background(), d.ID, store.DeviceByConsole)
	require.NoError(t, err)
	status, body, _, reached = deviceGet(t, client, ts.URL+"/api/mcp", nil, seen)
	require.Equal(t, http.StatusUnauthorized, status, "a revoked relay is refused")
	require.Contains(t, body, ErrRelayRevoked.Error())
	require.False(t, reached)
}

// A relay that presents a certificate is judged by it alone: a valid shared
// key in the same request does not rescue an unpaired relay.
func TestEdge_UnpairedRelayIsNotRescuedByAKey(t *testing.T) {
	f := pairNewFixture(t, false)
	seen := make(chan deviceProbe, 2)
	ts, pool, _ := f.deviceServer(t, seen, nil)
	_, key := f.pairKeyDevice(t, "example-ci")
	client := deviceClient(pool, pairIdentity(t, "example-laptop"))
	status, _, _, reached := deviceGet(t, client, ts.URL+"/api/mcp", http.Header{"Authorization": {"Bearer " + key}}, seen)
	require.Equal(t, http.StatusUnauthorized, status)
	require.False(t, reached)
}

// A relay whose pairing waits for the owner is refused until approved.
func TestEdge_PendingRelayIsRefusedUntilApproved(t *testing.T) {
	f := pairNewFixture(t, true)
	seen := make(chan deviceProbe, 2)
	ts, pool, _ := f.deviceServer(t, seen, nil)
	id := pairIdentity(t, "example-laptop")
	answer, err := f.svc.Pair(context.Background(), PairInput{Code: f.pairCode(t), Kind: KindRelay, Name: "example-laptop",
		Secret: pairSecret(t), RelayKey: id.PublicKey()})
	require.NoError(t, err)
	require.Equal(t, StatusPending, answer.Status, "under peer_auth a code without approval enrolls nothing")
	status, body, _, reached := deviceGet(t, deviceClient(pool, id), ts.URL+"/api/mcp", nil, seen)
	require.Equal(t, http.StatusUnauthorized, status)
	require.Contains(t, body, ErrRelayPending.Error())
	require.False(t, reached)

	_, err = store.ApproveDevice(context.Background(), f.db, answer.Device.ID, answer.Device.Fingerprint, "01PASSKEY", f.pairNow())
	require.NoError(t, err)
	status, _, p, reached := deviceGet(t, deviceClient(pool, id), ts.URL+"/api/mcp", nil, seen)
	require.Equal(t, http.StatusOK, status)
	require.True(t, reached)
	require.Equal(t, answer.Device.ID, p.cred.DeviceID)
}

// A device key is the device: it speaks for its enrolled name, so one
// device's key never authenticates another device's name. Revoked, it is no
// credential at all, and the routes refuse it as a wrong key.
func TestEdge_DeviceKeySpeaksForItsOwnNameOnly(t *testing.T) {
	f := pairNewFixture(t, false)
	seen := make(chan deviceProbe, 8)
	ts, pool, _ := f.deviceServer(t, seen, nil)
	laptop, laptopKey := f.pairKeyDevice(t, "example-laptop")
	ci, ciKey := f.pairKeyDevice(t, "example-ci")
	client := deviceClient(pool, nil)

	_, _, p, ok := deviceGet(t, client, ts.URL+"/api/hooks/stop?client=codex&host=example-ci", http.Header{
		"Authorization":   {"Bearer " + laptopKey},
		"X-Seamless-Host": {"example-ci"},
	}, seen)
	require.True(t, ok)
	require.True(t, p.paired)
	require.Equal(t, laptop.ID, p.cred.DeviceID, "the key names its own device")
	require.Equal(t, "example-laptop", p.host, "one device's key does not authenticate another device's name")
	require.Equal(t, "example-laptop", p.hookHost)

	_, _, p, ok = deviceGet(t, client, ts.URL+"/api/mcp", http.Header{"Authorization": {"Bearer " + ciKey}}, seen)
	require.True(t, ok)
	require.Equal(t, Credential{DeviceID: ci.ID, Kind: KindKey, Name: "example-ci"}, p.cred)

	_, err := f.svc.Revoke(context.Background(), laptop.ID, store.DeviceByTerminal)
	require.NoError(t, err)
	_, _, p, ok = deviceGet(t, client, ts.URL+"/api/mcp", http.Header{
		"Authorization": {"Bearer " + laptopKey}, "X-Seamless-Host": {"example-laptop"},
	}, seen)
	require.True(t, ok)
	require.False(t, p.paired, "a revoked key is no credential: it reaches the routes unmarked, and they refuse it")
	require.False(t, Bearer(deviceRequestWith(t, laptopKey), "shared-key"))

	never, _, err := NewKey()
	require.NoError(t, err)
	_, _, p, ok = deviceGet(t, client, ts.URL+"/api/mcp", http.Header{"Authorization": {"Bearer " + never}}, seen)
	require.True(t, ok)
	require.False(t, p.paired, "a key no device holds is no credential")
}

func deviceRequestWith(t *testing.T, key string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/api/mcp", nil)
	require.NoError(t, err)
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}

// A hook query that does not parse is left for the hook to refuse.
func TestEdge_MalformedHookQueryIsLeftAlone(t *testing.T) {
	r, err := http.NewRequest(http.MethodPost, "http://127.0.0.1/api/hooks/stop?client=%zz", nil)
	require.NoError(t, err)
	got := speakFor(r, "example-laptop")
	require.Equal(t, "client=%zz", got.URL.RawQuery)
	require.Equal(t, "example-laptop", got.Header.Get(hostHeader))
}

// Revoking a device ends the requests it is making, at once.
func TestEdge_RevocationEndsLiveRequests(t *testing.T) {
	f := pairNewFixture(t, false)
	seen := make(chan deviceProbe, 2)
	hold := make(chan struct{})
	defer close(hold)
	ts, pool, _ := f.deviceServer(t, seen, hold)
	d, key := f.pairKeyDevice(t, "example-ci")
	client := deviceClient(pool, nil)
	done := make(chan error, 1)
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/hold", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	go func() {
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	var p deviceProbe
	select {
	case p = <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("the held request never reached the route")
	}
	require.NoError(t, p.ctxErr())
	_, err = f.svc.Revoke(context.Background(), d.ID, store.DeviceByConsole)
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("revoking the device did not end its request")
	}
	require.ErrorIs(t, p.ctxErr(), context.Canceled)
}

// The sweep ends the live requests of a device revoked elsewhere (the
// terminal), which this service did not see happen.
func TestSweep_EndsRequestsOfADeviceRevokedElsewhere(t *testing.T) {
	f := pairNewFixture(t, false)
	d, _ := f.pairKeyDevice(t, "example-ci")
	ctx, done := f.svc.track(context.Background(), d.ID)
	defer done()
	f.svc.sweep(context.Background())
	require.NoError(t, ctx.Err(), "an active device's request runs on")
	_, err := store.RevokeDevice(context.Background(), f.db, d.ID, store.DeviceByTerminal, f.pairNow())
	require.NoError(t, err)
	f.svc.sweep(context.Background())
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

// Only a client that offers the relay protocol is asked for a certificate:
// a browser, curl or seam with a key never is.
func TestWithRelayClientAuth_OnlyARelayIsAskedForACertificate(t *testing.T) {
	f := pairNewFixture(t, false)
	ts, pool, _ := f.deviceServer(t, nil, nil)
	id := pairIdentity(t, "example-laptop")
	asked := false
	plain := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			asked = true
			c := id.cert
			return &c, nil
		}}
	conn, err := tls.Dial("tcp", strings.TrimPrefix(ts.URL, "https://"), plain)
	require.NoError(t, err)
	require.NoError(t, conn.Handshake())
	require.False(t, asked, "a client without the relay protocol is never asked for a certificate")
	require.NotEqual(t, RelayALPN, conn.ConnectionState().NegotiatedProtocol)
	require.NoError(t, conn.Close())

	relay := id.ClientTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	conn, err = tls.Dial("tcp", strings.TrimPrefix(ts.URL, "https://"), relay)
	require.NoError(t, err)
	require.NoError(t, conn.Handshake())
	require.Equal(t, "http/1.1", conn.ConnectionState().NegotiatedProtocol, "the marker is never selected")
	require.NoError(t, conn.Close())
}

// A CA chain is no proof: a relay certificate a CA issued over a key no
// device holds is refused like any other, and a relay that sends a chain
// fails the handshake.
func TestWithRelayClientAuth_ACAChainIsNoProof(t *testing.T) {
	f := pairNewFixture(t, false)
	seen := make(chan deviceProbe, 2)
	ts, pool, _ := f.deviceServer(t, seen, nil)
	paired := pairIdentity(t, "example-laptop")
	f.pairRelayDevice(t, "example-laptop", paired)

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "example CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leafTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "example-laptop"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	require.NoError(t, err)

	issued := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1", RelayALPN},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}, nil
		}}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: issued}, Timeout: 10 * time.Second}
	status, _, _, reached := deviceGet(t, client, ts.URL+"/api/mcp", nil, seen)
	require.Equal(t, http.StatusUnauthorized, status, "a CA's signature over a key no device holds proves nothing")
	require.False(t, reached)

	chained := issued.Clone()
	chained.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		return &tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}, nil
	}
	chainClient := &http.Client{Transport: &http.Transport{TLSClientConfig: chained}, Timeout: 10 * time.Second}
	_, err = chainClient.Get(ts.URL + "/api/mcp")
	require.Error(t, err, "a relay presents its own certificate alone")
}
