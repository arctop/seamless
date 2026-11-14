//go:build darwin || linux

package relay

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// relayPairServer is a server's pairing service over a fresh database, behind
// its edge (devices.Service.Edge) on a TLS listener that asks relays for their
// certificate, in front of a route that reports the credential the edge
// matched.
type relayPairServer struct {
	db   *sql.DB
	svc  *devices.Service
	ts   *httptest.Server
	cert relayCert
	mu   sync.Mutex
	// creds are the credentials the route saw, in order.
	creds []devices.Credential
}

func relayNewPairServer(t *testing.T, peerAuth bool) *relayPairServer {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	s := &relayPairServer{db: db, cert: relayNewCert(t, "example-server")}
	s.svc = devices.New(devices.Config{DB: db, Events: events.NewRecorder(db), PeerAuth: peerAuth, LocalHost: "example-server",
		Server: devices.ServerFacts{URL: "https://127.0.0.1", TLS: true}, Logger: relayLogger()})
	route := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, _ := devices.CredentialOf(r.Context())
		s.mu.Lock()
		s.creds = append(s.creds, cred)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	s.ts = relayUpstream(t, s.cert, s.svc.Edge(route))
	return s
}

// relayCode mints a pairing code, as the console does.
func (s *relayPairServer) relayCode(t *testing.T) string {
	t.Helper()
	c, err := s.svc.MintCode(context.Background(), store.DeviceByConsole)
	require.NoError(t, err)
	return c.Code
}

// relayLastCred is the credential the route saw last.
func (s *relayPairServer) relayLastCred(t *testing.T) devices.Credential {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotEmpty(t, s.creds)
	return s.creds[len(s.creds)-1]
}

// relayUnusedCodes counts the codes no pairing used.
func (s *relayPairServer) relayUnusedCodes(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM device_pairing_codes WHERE used_at IS NULL`).Scan(&n))
	return n
}

// relayMode is path's permission bits.
func relayMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	require.NoError(t, err)
	return info.Mode().Perm()
}

// A relay pairs with a code and the certificate the owner compared, keeps
// its key and the server's pinned certificate in its own data directory, and
// from then on the server's edge takes its requests as the paired device's.
func TestRelayPair_PairsPinsAndForwardsAsThePairedDevice(t *testing.T) {
	s := relayNewPairServer(t, false)
	l := relayLayoutAt(t)
	st, err := Pair(context.Background(), l.DataDir, PairOptions{Server: s.ts.URL + "/", Code: s.relayCode(t), Name: "Example-Laptop",
		ServerFingerprint: s.cert.fp, CA: s.cert.pem})
	require.NoError(t, err)
	require.Equal(t, s.ts.URL, st.ServerURL)
	require.Equal(t, s.cert.fp, st.ServerFingerprint, "the certificate the server presented is the one pinned")
	require.Equal(t, "example-laptop", st.Name)
	d, err := store.DeviceByID(context.Background(), s.db, st.DeviceID)
	require.NoError(t, err)
	require.Equal(t, store.DeviceActive, d.Status, "without peer_auth the code is the approval")
	require.Equal(t, devices.KindRelay, d.Kind)
	require.Equal(t, d.Fingerprint, st.Fingerprint, "both ends show the relay key's fingerprint")

	require.Equal(t, os.FileMode(0o600), relayMode(t, filepath.Join(l.DataDir, StateName)), "the pairing holds the private key")
	require.NotEmpty(t, st.CAFile)
	require.Equal(t, os.FileMode(0o600), relayMode(t, st.CAPath(l.DataDir)))
	require.Equal(t, os.FileMode(0o700), relayMode(t, l.DataDir))
	back, err := LoadState(l.DataDir)
	require.NoError(t, err)
	require.Equal(t, st.DeviceID, back.DeviceID)
	require.Equal(t, st.Key, back.Key)

	client := relayRun(t, l, back)
	status, _, _ := relayDo(t, client, http.MethodGet, "/api/mcp", http.Header{"Authorization": {"Bearer example-planted-key"}}, "")
	require.Equal(t, http.StatusOK, status)
	cred := s.relayLastCred(t)
	require.Equal(t, st.DeviceID, cred.DeviceID, "the server's edge matched the relay's certificate")
	require.Equal(t, "example-laptop", cred.Name)

	// Revoked on the server, the relay's requests are refused there, and the
	// relay passes the server's refusal on as it came.
	_, err = s.svc.Revoke(context.Background(), st.DeviceID, store.DeviceByConsole)
	require.NoError(t, err)
	status, _, body := relayDo(t, client, http.MethodGet, "/api/mcp", nil, "")
	require.Equal(t, http.StatusUnauthorized, status)
	require.Contains(t, body, devices.ErrRelayRevoked.Error())
}

// Under peer_auth the pairing waits for the owner's passkey, showing the
// fingerprint the Devices page shows; the relay saves nothing until then.
func TestRelayPair_WaitsForTheOwnersApproval(t *testing.T) {
	s := relayNewPairServer(t, true)
	l := relayLayoutAt(t)
	waiting := make(chan string, 1)
	done := make(chan error, 1)
	var st State
	go func() {
		var err error
		st, err = Pair(context.Background(), l.DataDir, PairOptions{Server: s.ts.URL, Code: s.relayCode(t), Name: "example-laptop",
			CA: s.cert.pem, Poll: 20 * time.Millisecond, Waiting: func(fp string) { waiting <- fp }})
		done <- err
	}()
	var fp string
	select {
	case fp = <-waiting:
	case err := <-done:
		t.Fatalf("the pairing did not wait: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the pairing never said it waits")
	}
	_, err := LoadState(l.DataDir)
	require.ErrorIs(t, err, ErrNotPaired, "nothing is saved while the owner has not approved")
	list, err := s.svc.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, fp, list[0].Fingerprint, "the page shows the fingerprint the relay showed")
	_, err = store.ApproveDevice(context.Background(), s.db, list[0].ID, list[0].Fingerprint, "01PASSKEYEXAMPLE", time.Now())
	require.NoError(t, err)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the pairing did not finish after the approval")
	}
	require.Equal(t, list[0].ID, st.DeviceID)
	_, err = LoadState(l.DataDir)
	require.NoError(t, err)
}

// The code goes only to the certificate the owner compared: with another
// fingerprint nothing is sent, nothing is saved, and the code stays unused.
func TestRelayPair_AnotherFingerprintSendsNothing(t *testing.T) {
	s := relayNewPairServer(t, false)
	l := relayLayoutAt(t)
	other := relayNewCert(t, "example-server")
	_, err := Pair(context.Background(), l.DataDir, PairOptions{Server: s.ts.URL, Code: s.relayCode(t), Name: "example-laptop",
		ServerFingerprint: other.fp, CA: s.cert.pem})
	require.ErrorIs(t, err, devices.ErrServerFingerprint)
	_, err = LoadState(l.DataDir)
	require.ErrorIs(t, err, ErrNotPaired)
	cas, err := caCopies(l.DataDir)
	require.NoError(t, err)
	require.Empty(t, cas, "a pairing that did not finish leaves no CA copy behind")
	require.Equal(t, 1, s.relayUnusedCodes(t))
}

// A relay that is paired stays paired unless the pairing is replaced on
// purpose; a replacement is a new key, and only its CA copy stays.
func TestRelayPair_ReplacesOnlyWhenAsked(t *testing.T) {
	s := relayNewPairServer(t, false)
	l := relayLayoutAt(t)
	first, err := Pair(context.Background(), l.DataDir, PairOptions{Server: s.ts.URL, Code: s.relayCode(t), Name: "example-laptop", CA: s.cert.pem})
	require.NoError(t, err)

	code := s.relayCode(t)
	_, err = Pair(context.Background(), l.DataDir, PairOptions{Server: s.ts.URL, Code: code, Name: "example-laptop", CA: s.cert.pem})
	require.ErrorIs(t, err, ErrPaired)
	require.Contains(t, err.Error(), first.DeviceID, "the refusal names the pairing that stands")
	require.Equal(t, 1, s.relayUnusedCodes(t), "a refused pairing sends nothing")

	// The server refuses a second device of the same name while the first is
	// live, and the pairing that stands is untouched.
	_, err = Pair(context.Background(), l.DataDir, PairOptions{Server: s.ts.URL, Code: code, Name: "example-laptop", CA: s.cert.pem, Replace: true})
	require.ErrorIs(t, err, devices.ErrNameTaken)
	kept, err := LoadState(l.DataDir)
	require.NoError(t, err)
	require.Equal(t, first.DeviceID, kept.DeviceID)

	_, err = s.svc.Revoke(context.Background(), first.DeviceID, store.DeviceByConsole)
	require.NoError(t, err)
	// The same root, written another way, is another copy.
	second, err := Pair(context.Background(), l.DataDir, PairOptions{Server: s.ts.URL, Code: s.relayCode(t), Name: "example-laptop",
		CA: append(append([]byte{}, s.cert.pem...), '\n'), Replace: true})
	require.NoError(t, err)
	require.NotEqual(t, first.DeviceID, second.DeviceID)
	require.NotEqual(t, first.Fingerprint, second.Fingerprint, "a new pairing is a new key")
	require.NotEqual(t, first.CAFile, second.CAFile)
	cas, err := caCopies(l.DataDir)
	require.NoError(t, err)
	require.Equal(t, []string{second.CAFile}, cas, "the replaced pairing's CA copy is gone with it")
}

// What a relay cannot pair with is refused before anything is sent.
func TestRelayPair_RefusesWhatItCannotUse(t *testing.T) {
	s := relayNewPairServer(t, false)
	l := relayLayoutAt(t)
	for name, o := range map[string]PairOptions{
		"plain http":     {Server: "http" + strings.TrimPrefix(s.ts.URL, "https"), Code: s.relayCode(t)},
		"a path":         {Server: s.ts.URL + "/seamless", Code: s.relayCode(t)},
		"no host":        {Server: "https://", Code: s.relayCode(t)},
		"a fingerprint":  {Server: s.ts.URL, Code: s.relayCode(t), ServerFingerprint: "AB:CD"},
		"a key as a CA":  {Server: s.ts.URL, Code: s.relayCode(t), CA: []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")},
		"no certificate": {Server: s.ts.URL, Code: s.relayCode(t), CA: []byte("not a certificate")},
		"a wrong code":   {Server: s.ts.URL, Code: "AAAA-BBBB-CCCC-DDDD", CA: s.cert.pem},
	} {
		o.Name = "example-laptop"
		_, err := Pair(context.Background(), l.DataDir, o)
		require.Error(t, err, name)
		_, lerr := LoadState(l.DataDir)
		require.ErrorIs(t, lerr, ErrNotPaired, name)
		cas, cerr := caCopies(l.DataDir)
		require.NoError(t, cerr)
		require.Empty(t, cas, name)
	}
	_, err := Pair(context.Background(), l.DataDir, PairOptions{Server: s.ts.URL, Code: "AAAA-BBBB-CCCC-DDDD", Name: "example-laptop", CA: s.cert.pem})
	require.True(t, errors.Is(err, devices.ErrCodeRefused), "%v", err)
	// Without the server's root the system's roots do not trust it, and the
	// code is never sent.
	before := s.relayUnusedCodes(t)
	_, err = Pair(context.Background(), l.DataDir, PairOptions{Server: s.ts.URL, Code: s.relayCode(t), Name: "example-laptop"})
	require.Error(t, err)
	require.Equal(t, before+1, s.relayUnusedCodes(t))
}
