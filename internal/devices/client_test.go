package devices

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// pairCount counts the device rows and the unused codes.
func (f *pairFixture) pairCount(t *testing.T) (devices, unused int) {
	t.Helper()
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&devices))
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM device_pairing_codes WHERE used_at IS NULL`).Scan(&unused))
	return devices, unused
}

// Without peer_auth the code is the approval: a key device pairs at once and
// gets a key of its own, which the edge then accepts as that device.
func TestClientPair_KeyDeviceWithoutPeerAuth(t *testing.T) {
	f := pairNewFixture(t, false)
	seen := make(chan deviceProbe, 2)
	ts, pool, certFP := f.deviceServer(t, seen, nil)
	code := f.pairCode(t)

	paired, err := Pair(context.Background(), deviceClient(pool, nil), ts.URL, PairRequest{Code: code, Name: "Example-CI",
		ServerFingerprint: certFP})
	require.NoError(t, err)
	require.Equal(t, KindKey, paired.Kind)
	require.Equal(t, "example-ci", paired.Name)
	require.True(t, IsKey(paired.Key))
	require.Equal(t, certFP, paired.ServerFingerprint)
	d, err := store.DeviceByID(context.Background(), f.db, paired.DeviceID)
	require.NoError(t, err)
	require.Equal(t, store.DeviceActive, d.Status)
	require.Empty(t, d.ApprovedBy, "the code approved it")
	require.Equal(t, paired.Key[:len(KeyPrefix)+4], d.KeyPrefix)
	var stored string
	require.NoError(t, f.db.QueryRow(`SELECT key_hash FROM devices WHERE id = ?`, d.ID).Scan(&stored))
	require.Equal(t, KeyHash(paired.Key), stored, "the server keeps only the key's hash")

	_, _, p, ok := deviceGet(t, deviceClient(pool, nil), ts.URL+"/api/mcp", http.Header{"Authorization": {"Bearer " + paired.Key}}, seen)
	require.True(t, ok)
	require.Equal(t, paired.DeviceID, p.cred.DeviceID)

	_, err = Pair(context.Background(), deviceClient(pool, nil), ts.URL, PairRequest{Code: code, Name: "example-other"})
	require.ErrorIs(t, err, ErrCodeRefused, "a used code fails")
}

// A relay pairs over TLS with its certificate, and is then accepted.
func TestClientPair_RelayWithoutPeerAuth(t *testing.T) {
	f := pairNewFixture(t, false)
	seen := make(chan deviceProbe, 2)
	ts, pool, certFP := f.deviceServer(t, seen, nil)
	id := pairIdentity(t, "example-laptop")
	paired, err := Pair(context.Background(), deviceClient(pool, nil), ts.URL, PairRequest{Code: f.pairCode(t),
		Name: "example-laptop", Relay: id, ServerFingerprint: certFP})
	require.NoError(t, err)
	require.Equal(t, KindRelay, paired.Kind)
	require.Equal(t, id.Fingerprint(), paired.Fingerprint, "both ends show the key's fingerprint")
	require.Empty(t, paired.Key, "a relay holds no bearer key")
	status, _, p, ok := deviceGet(t, deviceClient(pool, id), ts.URL+"/api/mcp", nil, seen)
	require.Equal(t, http.StatusOK, status)
	require.True(t, ok)
	require.Equal(t, paired.DeviceID, p.cred.DeviceID)
}

// A relay cannot pair without presenting its certificate, nor a key device
// with one.
func TestClientPair_KindsAndCertificates(t *testing.T) {
	f := pairNewFixture(t, false)
	ts, pool, _ := f.deviceServer(t, nil, nil)
	_, err := f.svc.Pair(context.Background(), PairInput{Code: f.pairCode(t), Kind: KindRelay, Name: "example-laptop", Secret: pairSecret(t)})
	require.ErrorIs(t, err, ErrRelayNeedsCertificate)
	id := pairIdentity(t, "example-laptop")
	_, err = f.svc.Pair(context.Background(), PairInput{Code: f.pairCode(t), Kind: KindKey, Name: "example-ci", Secret: pairSecret(t),
		RelayKey: id.PublicKey()})
	require.ErrorIs(t, err, ErrKeyWithCertificate)
	_, err = f.svc.Pair(context.Background(), PairInput{Code: f.pairCode(t), Kind: "printer", Name: "example-ci", Secret: pairSecret(t)})
	require.ErrorIs(t, err, ErrKind)
	_, err = f.svc.Pair(context.Background(), PairInput{Code: f.pairCode(t), Kind: KindKey, Name: "Example-Server", Secret: pairSecret(t)})
	require.ErrorIs(t, err, ErrServerName, "no device pairs under the server's own name")
	_, err = Pair(context.Background(), deviceClient(pool, nil), "http"+ts.URL[len("https"):], PairRequest{Code: f.pairCode(t),
		Name: "example-laptop", Relay: id})
	require.ErrorIs(t, err, ErrRelayNeedsCertificate, "a relay pairs over https only")
	devices, _ := f.pairCount(t)
	require.Zero(t, devices)
}

// The code goes only to the server whose certificate the owner compared:
// with another fingerprint, nothing is sent and nothing pairs.
func TestClientPair_ServerFingerprintMismatchSendsNothing(t *testing.T) {
	f := pairNewFixture(t, false)
	ts, pool, _ := f.deviceServer(t, nil, nil)
	code := f.pairCode(t)
	_, err := Pair(context.Background(), deviceClient(pool, nil), ts.URL, PairRequest{Code: code, Name: "example-ci",
		ServerFingerprint: "00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF"})
	require.ErrorIs(t, err, ErrServerFingerprint)
	devices, unused := f.pairCount(t)
	require.Zero(t, devices)
	require.Equal(t, 1, unused, "the code was never sent, so it is still unused")
}

// Under peer_auth a code without approval enrolls nothing: the device waits,
// told the fingerprint to compare, and gets its key only once the owner
// approves; a second status gives no key again.
func TestClientPair_PeerAuthWaitsForTheOwner(t *testing.T) {
	f := pairNewFixture(t, true)
	seen := make(chan deviceProbe, 4)
	ts, pool, _ := f.deviceServer(t, seen, nil)
	waiting := make(chan string, 1)
	result := make(chan error, 1)
	var paired Paired
	go func() {
		var err error
		paired, err = Pair(context.Background(), deviceClient(pool, nil), ts.URL, PairRequest{Code: f.pairCode(t), Name: "example-ci",
			Poll: 20 * time.Millisecond, Waiting: func(fp string) { waiting <- fp }})
		result <- err
	}()
	var fp string
	select {
	case fp = <-waiting:
	case err := <-result:
		t.Fatalf("pairing finished without waiting: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the pairing never said it waits")
	}
	list, err := f.svc.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, store.DevicePending, list[0].Status)
	require.Equal(t, fp, list[0].Fingerprint, "the page shows the fingerprint the device shows")
	require.False(t, list[0].HasKey, "nothing that authenticates exists yet")

	_, err = store.ApproveDevice(context.Background(), f.db, list[0].ID, list[0].Fingerprint, "01PASSKEY", f.pairNow())
	require.NoError(t, err)
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the pairing did not finish after the approval")
	}
	require.True(t, IsKey(paired.Key))
	_, _, p, ok := deviceGet(t, deviceClient(pool, nil), ts.URL+"/api/mcp", http.Header{"Authorization": {"Bearer " + paired.Key}}, seen)
	require.True(t, ok)
	require.Equal(t, list[0].ID, p.cred.DeviceID)
}

// Status answers only the secret that paired: a status that collected the key
// gives no key again.
func TestStatus_KeyIsCollectedOnce(t *testing.T) {
	f := pairNewFixture(t, true)
	secret := pairSecret(t)
	answer, err := f.svc.Pair(context.Background(), PairInput{Code: f.pairCode(t), Kind: KindKey, Name: "example-ci", Secret: secret})
	require.NoError(t, err)
	require.Equal(t, StatusPending, answer.Status)
	require.Empty(t, answer.Key)
	st, err := f.svc.Status(context.Background(), secret)
	require.NoError(t, err)
	require.Equal(t, StatusPending, st.Status)
	require.Empty(t, st.Key)
	_, err = f.svc.Status(context.Background(), pairSecret(t))
	require.ErrorIs(t, err, ErrUnknownPairing, "another secret asks after nothing")

	_, err = store.ApproveDevice(context.Background(), f.db, answer.Device.ID, answer.Device.Fingerprint, "01PASSKEY", f.pairNow())
	require.NoError(t, err)
	first, err := f.svc.Status(context.Background(), secret)
	require.NoError(t, err)
	require.Equal(t, StatusPaired, first.Status)
	require.True(t, IsKey(first.Key))
	again, err := f.svc.Status(context.Background(), secret)
	require.NoError(t, err)
	require.Equal(t, StatusPaired, again.Status)
	require.Empty(t, again.Key, "the key is given once")
}

// The owner's denial and the pairing's deadline end the wait.
func TestClientPair_DeniedAndExpired(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(f *pairFixture, d store.Device) error
		want error
	}{
		{"denied", func(f *pairFixture, d store.Device) error {
			_, err := f.svc.Revoke(context.Background(), d.ID, store.DeviceByConsole)
			return err
		}, ErrPairingDenied},
		{"expired", func(f *pairFixture, _ store.Device) error {
			f.pairAdvance(store.DevicePendingTTL)
			return nil
		}, ErrPairingExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := pairNewFixture(t, true)
			ts, pool, _ := f.deviceServer(t, nil, nil)
			waiting := make(chan struct{}, 1)
			result := make(chan error, 1)
			go func() {
				_, err := Pair(context.Background(), deviceClient(pool, nil), ts.URL, PairRequest{Code: f.pairCode(t), Name: "example-ci",
					Poll: 20 * time.Millisecond, Waiting: func(string) { waiting <- struct{}{} }})
				result <- err
			}()
			<-waiting
			list, err := f.svc.List(context.Background())
			require.NoError(t, err)
			require.Len(t, list, 1)
			require.NoError(t, tc.end(f, list[0]))
			select {
			case err := <-result:
				require.ErrorIs(t, err, tc.want)
			case <-time.After(5 * time.Second):
				t.Fatal("the wait did not end")
			}
		})
	}
}

// A waiting pairing gives up when its context ends.
func TestClientPair_StopsWithItsContext(t *testing.T) {
	f := pairNewFixture(t, true)
	ts, pool, _ := f.deviceServer(t, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	_, err := Pair(ctx, deviceClient(pool, nil), ts.URL, PairRequest{Code: f.pairCode(t), Name: "example-ci",
		Poll: 10 * time.Millisecond, Waiting: func(string) { cancel() }})
	require.True(t, errors.Is(err, context.Canceled), "%v", err)
}

// The pairing endpoint refuses what it cannot read, field by field.
func TestPairEndpoint_RefusesMalformedRequests(t *testing.T) {
	f := pairNewFixture(t, false)
	ts, pool, _ := f.deviceServer(t, nil, nil)
	client := deviceClient(pool, nil)
	post := func(path, body string) (int, string) {
		resp, err := client.Post(ts.URL+path, "application/json", strings.NewReader(body))
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
		var out pairError
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		return resp.StatusCode, out.Reason
	}
	secret := pairSecret(t)
	for _, tc := range []struct {
		path, body string
		status     int
		reason     string
	}{
		{PairPath, `not json`, http.StatusBadRequest, "input"},
		{PairPath, `{"code":"x","kind":"key","name":"example-ci","secret":"` + secret + `","extra":1}`, http.StatusBadRequest, "input"},
		{PairPath, `{"code":"x"} {"code":"y"}`, http.StatusBadRequest, "input"},
		{PairPath, `{"code":"AAAA-BBBB-CCCC-DDDD","kind":"key","name":"example-ci","secret":"` + secret + `"}`, http.StatusForbidden, "code"},
		{PairPath, `{"code":"AAAA-BBBB-CCCC-DDDD","kind":"key","name":"Bad Name","secret":"` + secret + `"}`, http.StatusBadRequest, "input"},
		{PairPath, `{"code":"AAAA-BBBB-CCCC-DDDD","kind":"key","name":"example-ci","secret":"short"}`, http.StatusBadRequest, "input"},
		{PairStatusPath, `{"secret":"` + secret + `"}`, http.StatusNotFound, "pairing"},
		{PairStatusPath, `{"secret":"` + secret + `","code":"AAAA"}`, http.StatusBadRequest, "input"},
	} {
		status, reason := post(tc.path, tc.body)
		require.Equal(t, tc.status, status, tc.body)
		require.Equal(t, tc.reason, reason, tc.body)
	}
	resp, err := client.Get(ts.URL + PairPath)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	devices, _ := f.pairCount(t)
	require.Zero(t, devices)
}

// A relay's transport presents its certificate and forwards only to the
// server certificate pinned at pairing; there is none without a pin.
func TestRelayTransport_PresentsTheRelayAndPinsTheServer(t *testing.T) {
	f := pairNewFixture(t, false)
	seen := make(chan deviceProbe, 2)
	ts, pool, certFP := f.deviceServer(t, seen, nil)
	id := pairIdentity(t, "example-laptop")
	dev := f.pairRelayDevice(t, "example-laptop", id)
	base := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}

	tr, err := RelayTransport(base, id, strings.ToLower(certFP))
	require.NoError(t, err)
	status, _, p, ok := deviceGet(t, &http.Client{Transport: tr}, ts.URL+"/api/mcp", nil, seen)
	require.Equal(t, http.StatusOK, status)
	require.True(t, ok)
	require.Equal(t, dev.ID, p.cred.DeviceID, "the edge matched the relay's certificate")

	wrong, err := RelayTransport(base, id, strings.TrimSuffix(strings.Repeat("AB:", 32), ":"))
	require.NoError(t, err)
	_, err = (&http.Client{Transport: wrong}).Get(ts.URL + "/api/mcp")
	require.ErrorIs(t, err, ErrServerFingerprint)
	select {
	case got := <-seen:
		t.Fatalf("a request reached the server through another pin: %+v", got)
	default:
	}

	_, err = RelayTransport(base, id, " ")
	require.ErrorIs(t, err, ErrNoServerPin)
	_, err = RelayTransport(base, nil, certFP)
	require.ErrorIs(t, err, ErrRelayIdentity)
	_, err = RelayTransport(nil, id, certFP)
	require.Error(t, err)
	require.Nil(t, base.Transport.(*http.Transport).TLSClientConfig.GetClientCertificate, "the client it copies is untouched")
	require.Nil(t, base.Transport.(*http.Transport).TLSClientConfig.VerifyConnection)
}

func TestValidCertFingerprint(t *testing.T) {
	fp := CertFingerprint([]byte("example certificate"))
	for s, ok := range map[string]bool{
		fp:                              true,
		strings.ToLower(fp):             true,
		strings.ReplaceAll(fp, ":", ""): true,
		"AB:CD":                         false,
		"":                              false,
		fp + ":00":                      false,
		"ZZ" + fp[2:]:                   false,
	} {
		require.Equal(t, ok, ValidCertFingerprint(s), s)
	}
}

// clientNoDial is a transport no pairing may use.
type clientNoDial struct{ t *testing.T }

func (c clientNoDial) RoundTrip(*http.Request) (*http.Response, error) {
	c.t.Helper()
	c.t.Fatal("a refused pairing dialed the server")
	return nil, errors.New("unreachable")
}

// Over plain http only loopback pairs: to another computer the code, and a key
// device's key in the answer, would cross the network in the clear, so the
// pairing is refused before anything is sent.
func TestClientPair_PlainHTTPPairsOnlyOnLoopback(t *testing.T) {
	noDial := &http.Client{Transport: clientNoDial{t}}
	for _, server := range []string{"http://example-server:8081", "http://192.0.2.7:8081", "http://[2001:db8::7]:8081"} {
		_, err := Pair(context.Background(), noDial, server, PairRequest{Code: "ABCD-EFGH-JKMN-PQRS", Name: "example-ci"})
		require.ErrorIs(t, err, ErrPlainHTTP, server)
	}
	for _, host := range []string{"localhost", "127.0.0.1", "[::1]", "LOCALHOST.", "app.localhost"} {
		require.True(t, loopbackHost(strings.Trim(host, "[]")), host)
	}

	f := pairNewFixture(t, false)
	code := f.pairCode(t)
	srv := httptest.NewServer(f.svc.Edge(http.NotFoundHandler()))
	t.Cleanup(srv.Close)
	paired, err := Pair(context.Background(), &http.Client{}, srv.URL, PairRequest{Code: code, Name: "example-ci"})
	require.NoError(t, err, "loopback over http pairs as before")
	require.True(t, IsKey(paired.Key))
}
