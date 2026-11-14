package devices

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// pairFixture is a pairing service over a fresh database, on a clock the
// test moves.
type pairFixture struct {
	db  *sql.DB
	svc *Service
	mu  sync.Mutex
	at  time.Time
}

func pairNewFixture(t *testing.T, peerAuth bool) *pairFixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	f := &pairFixture{db: db, at: time.Now().UTC()}
	f.svc = New(Config{DB: db, Events: events.NewRecorder(db), PeerAuth: peerAuth, LocalHost: "example-server",
		Server: ServerFacts{URL: "https://example-server.example.ts.net:8081", TLS: true},
		Now:    f.pairNow})
	return f
}

// pairNow is the fixture's clock.
func (f *pairFixture) pairNow() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.at
}

// pairAdvance moves the fixture's clock.
func (f *pairFixture) pairAdvance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.at = f.at.Add(d)
}

// pairCode mints a code from the console.
func (f *pairFixture) pairCode(t *testing.T) string {
	t.Helper()
	c, err := f.svc.MintCode(context.Background(), store.DeviceByConsole)
	require.NoError(t, err)
	return c.Code
}

// pairSecret draws a pairing secret.
func pairSecret(t *testing.T) string {
	t.Helper()
	s, err := NewSecret()
	require.NoError(t, err)
	return s
}

// pairKeyDevice pairs a key device named name without peer_auth, and
// returns its device and its key.
func (f *pairFixture) pairKeyDevice(t *testing.T, name string) (store.Device, string) {
	t.Helper()
	answer, err := f.svc.Pair(context.Background(), PairInput{Code: f.pairCode(t), Kind: KindKey, Name: name, Secret: pairSecret(t)})
	require.NoError(t, err)
	require.Equal(t, StatusPaired, answer.Status)
	require.True(t, IsKey(answer.Key))
	return answer.Device, answer.Key
}

// pairRelayDevice pairs a relay named name with identity id, without
// peer_auth.
func (f *pairFixture) pairRelayDevice(t *testing.T, name string, id *RelayIdentity) store.Device {
	t.Helper()
	answer, err := f.svc.Pair(context.Background(), PairInput{Code: f.pairCode(t), Kind: KindRelay, Name: name,
		Secret: pairSecret(t), RelayKey: id.PublicKey()})
	require.NoError(t, err)
	require.Equal(t, StatusPaired, answer.Status)
	return answer.Device
}

func pairIdentity(t *testing.T, name string) *RelayIdentity {
	t.Helper()
	id, err := NewRelayIdentity(name)
	require.NoError(t, err)
	return id
}

// deviceServerCert is a self-signed server certificate for 127.0.0.1, and a
// pool that trusts it.
func deviceServerCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "example-server"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// deviceProbe is what a route behind the edge saw of one request.
type deviceProbe struct {
	cred      Credential
	paired    bool
	host      string
	hookHost  string
	relayHdrs []string
	auth      string
	ctxErr    func() error
	// relay is what the edge read of a relay's forwarded attributes, and
	// forwarded whether it read any (RelayAttributesOf).
	relay     RelayAttributes
	forwarded bool
}

// deviceServer is the edge in front of a probe route, on a TLS listener that
// asks relays for their certificate (WithRelayClientAuth). The probe reports
// what reached it on seen.
func (f *pairFixture) deviceServer(t *testing.T, seen chan<- deviceProbe, hold <-chan struct{}) (*httptest.Server, *x509.CertPool, string) {
	t.Helper()
	cert, pool := deviceServerCert(t)
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := deviceProbe{host: r.Header.Get(hostHeader), hookHost: r.URL.Query().Get("host"), auth: r.Header.Get("Authorization"),
			ctxErr: r.Context().Err}
		p.cred, p.paired = CredentialOf(r.Context())
		p.relay, p.forwarded = RelayAttributesOf(r.Context())
		for name := range r.Header {
			if strings.HasPrefix(strings.ToLower(name), strings.ToLower(RelayAttributePrefix)) {
				p.relayHdrs = append(p.relayHdrs, name)
			}
		}
		if seen != nil {
			seen <- p
		}
		if hold != nil && r.URL.Path == "/hold" {
			select {
			case <-hold:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewUnstartedServer(f.svc.Edge(probe))
	ts.TLS = WithRelayClientAuth(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}})
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts, pool, CertFingerprint(cert.Certificate[0])
}

// deviceClient is an http.Client trusting pool, presenting id's certificate
// when id is set.
func deviceClient(pool *x509.CertPool, id *RelayIdentity) *http.Client {
	base := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if id != nil {
		base = id.ClientTLS(base)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: base}, Timeout: 10 * time.Second}
}

func TestBearer_SharedKeyOrAPairedCredential(t *testing.T) {
	req := func(auth string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/mcp", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		return r
	}
	require.True(t, Bearer(req("Bearer shared-key"), "shared-key"))
	require.True(t, Bearer(req("bearer shared-key"), "shared-key"))
	require.False(t, Bearer(req("Bearer other"), "shared-key"))
	require.False(t, Bearer(req(""), "shared-key"))
	require.False(t, Bearer(req("Bearer "), ""), "an empty shared key matches nothing")
	require.False(t, Bearer(req("Basic shared-key"), "shared-key"))

	// A device key the edge did not match is just a wrong key.
	key, _, err := NewKey()
	require.NoError(t, err)
	require.False(t, Bearer(req("Bearer "+key), "shared-key"))

	// The edge's mark is the only way a device credential counts: it holds
	// with an empty shared key too (a server that retired it).
	marked := req("")
	marked = marked.WithContext(WithCredential(marked.Context(), Credential{DeviceID: "01DEV", Kind: KindKey, Name: "example-ci"}))
	require.True(t, Bearer(marked, ""))
	require.True(t, Bearer(marked, "shared-key"))
	empty := req("")
	empty = empty.WithContext(WithCredential(empty.Context(), Credential{}))
	require.False(t, Bearer(empty, ""), "a credential naming no device is none")
}

func TestKeysSecretsAndFingerprints(t *testing.T) {
	key, shown, err := NewKey()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(key, KeyPrefix))
	require.True(t, IsKey(key))
	require.Equal(t, key[:len(KeyPrefix)+4], shown)
	require.Len(t, KeyHash(key), 64)
	require.NotContains(t, KeyHash(key), strings.TrimPrefix(key, KeyPrefix))
	for _, bad := range []string{"", KeyPrefix, KeyPrefix + "short", "shared-key", key + "x", strings.TrimPrefix(key, KeyPrefix)} {
		require.False(t, IsKey(bad), bad)
	}

	secret := pairSecret(t)
	h, err := SecretHash(secret)
	require.NoError(t, err)
	fp, err := SecretFingerprint(secret)
	require.NoError(t, err)
	require.Equal(t, fp, store.FormatKeyFingerprint(fp))
	require.NotContains(t, h, strings.ReplaceAll(strings.ToLower(fp), "-", ""), "the fingerprint shows nothing that asks after the pairing")
	for _, bad := range []string{"", "abc", secret + "A", strings.Repeat("A", 44)} {
		_, err := SecretHash(bad)
		require.ErrorIs(t, err, ErrSecret, bad)
	}

	require.True(t, sameCertFingerprint("AB:CD", "ab cd"))
	require.False(t, sameCertFingerprint("", ""))
	require.False(t, sameCertFingerprint("AB:CD", "AB:CE"))
}

func TestStripRelayAttributes(t *testing.T) {
	h := http.Header{}
	h.Set("X-Seamless-Relay-Uid", "0")
	h.Set("X-Seamless-Relay-Chain", "1:2:3")
	h["x-seamless-relay-odd"] = []string{"set by hand"}
	h.Set("X-Seamless-Host", "example-laptop")
	h.Set("Authorization", "Bearer k")
	require.Equal(t, 3, StripRelayAttributes(h))
	require.Equal(t, http.Header{"X-Seamless-Host": {"example-laptop"}, "Authorization": {"Bearer k"}}, h)
}

func TestRelayIdentity_RoundTripAndClientTLS(t *testing.T) {
	id := pairIdentity(t, "Example-Laptop")
	require.Equal(t, "example-laptop", id.cert.Leaf.Subject.CommonName)
	certPEM, keyPEM, err := id.MarshalPEM()
	require.NoError(t, err)
	back, err := LoadRelayIdentity(certPEM, keyPEM)
	require.NoError(t, err)
	require.Equal(t, id.PublicKey(), back.PublicKey())
	require.Equal(t, id.Fingerprint(), back.Fingerprint())
	other := pairIdentity(t, "example-ci")
	_, otherKey, err := other.MarshalPEM()
	require.NoError(t, err)
	_, err = LoadRelayIdentity(certPEM, otherKey)
	require.ErrorIs(t, err, ErrRelayIdentity, "a key that is not the certificate's does not load")
	_, err = LoadRelayIdentity(keyPEM, certPEM)
	require.ErrorIs(t, err, ErrRelayIdentity)

	c := id.ClientTLS(&tls.Config{NextProtos: []string{"h2", RelayALPN}, MinVersion: tls.VersionTLS13})
	require.Equal(t, []string{"h2", RelayALPN}, c.NextProtos)
	require.Equal(t, uint16(tls.VersionTLS13), c.MinVersion, "the base's verification settings stay")
	require.False(t, c.InsecureSkipVerify)
	got, err := c.GetClientCertificate(&tls.CertificateRequestInfo{})
	require.NoError(t, err)
	require.Equal(t, id.cert.Certificate, got.Certificate)
	require.Equal(t, []string{"http/1.1", RelayALPN}, id.ClientTLS(nil).NextProtos)
}
