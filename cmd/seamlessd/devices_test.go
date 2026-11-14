package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/a2a"
	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/hooks"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const deviceSharedKey = "device-test-shared-key"

// deviceDaemon is serve's TCP stack with pairing on: the real routes (MCP,
// the hooks, A2A, the console) behind the pairing edge, the Host guard and
// the activity tracker, on a TLS listener that asks a relay for its
// certificate. peerAuth puts the owner's View on every request, as serve's
// ownerHandler does.
type deviceDaemon struct {
	db      *sql.DB
	pairing *devices.Service
	srv     *httptest.Server
	pool    *x509.CertPool
}

func deviceNewDaemon(t *testing.T, peerAuth bool) *deviceDaemon {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	host := "example-server"
	require.NoError(t, store.AdoptLocalHost(context.Background(), db, host))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	ret := retrieve.New(db, nil, config.Defaults().Budgets, nil)
	ret.SetBodyReader(mgr.Store())
	rec := events.NewRecorder(db)
	garden := gardener.New(db, mgr, nil, nil, rec, gardener.Config{}, nil)
	// Under peer_auth a pairing waits for the owner; these tests approve it in
	// the store, as the passkey ceremony's write does (internal/approval).
	pairing := devices.New(devices.Config{DB: db, Events: rec, LocalHost: host, PeerAuth: peerAuth,
		Server: devices.ServerFacts{URL: "https://127.0.0.1", TLS: true}, Logger: logger})

	mcpSrv := mcp.New(mcp.Config{DB: db, Files: mgr, Retrieve: ret, Events: rec, Gardener: garden,
		APIKey: deviceSharedKey, Version: "test", LocalHost: host, Logger: logger})
	a2aSrv, err := a2a.New(a2a.Config{Retrieve: ret, Events: rec, APIKey: deviceSharedKey, Version: "test",
		Endpoint: "https://127.0.0.1/api/a2a", Logger: logger})
	require.NoError(t, err)
	hooksH := hooks.NewHandler(hooks.Config{DB: db, Retrieve: ret, Events: rec, Files: mgr, APIKey: deviceSharedKey,
		PlansDir: filepath.Join(dir, "plans"), LocalHost: host, Logger: logger})
	consoleSrv, err := console.New(console.Config{DB: db, Files: mgr, Gardener: garden, Events: rec, Retrieve: ret,
		APIKey: deviceSharedKey, DataDir: dir, Devices: pairing, Logger: logger})
	require.NoError(t, err)
	mux := daemonRoutes{
		healthz: healthzHandler(db, "device-instance", nil), mcp: mcpSrv.Handler(), a2a: a2aSrv.Handler(),
		agentCard: a2aSrv.CardHandler(), hooks: hooksH, console: consoleSrv,
	}.mux()
	// runServe's order: the pairing edge around the routes, the owner's View
	// around that under peer_auth, the Host guard and the tracker outside.
	routes := pairing.Edge(mux)
	if peerAuth {
		policy, err := access.New(config.PeerAuth{Enabled: true, Owner: "example-owner",
			Socket: config.PeerSocket{Path: "/opt/seamless/run/seamlessd.sock"}}, func(name string) (access.Account, error) {
			return access.Account{UID: 501, Name: name, Home: "/Users/" + name}, nil
		})
		require.NoError(t, err)
		routes = ownerHandler(policy, routes)
	}
	cert, pool := deviceServerCert(t)
	ts := httptest.NewUnstartedServer(daemonHandler("127.0.0.1:8081", nil, newActivityTracker(time.Now), routes))
	ts.TLS = devices.WithRelayClientAuth(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}})
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return &deviceDaemon{db: db, pairing: pairing, srv: ts, pool: pool}
}

// deviceServerCert is a self-signed server certificate for 127.0.0.1 and a
// pool that trusts it.
func deviceServerCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "example-server"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// deviceHTTP is a client trusting the daemon, presenting id's certificate
// when id is set.
func (d *deviceDaemon) deviceHTTP(id *devices.RelayIdentity) *http.Client {
	base := &tls.Config{RootCAs: d.pool, MinVersion: tls.VersionTLS12}
	if id != nil {
		base = id.ClientTLS(base)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: base}, Timeout: 20 * time.Second}
}

// devicePairKey pairs a key device named name (peer_auth off) and returns it
// with its key.
func (d *deviceDaemon) devicePairKey(t *testing.T, name string) (store.Device, string) {
	t.Helper()
	ctx := context.Background()
	code, err := d.pairing.MintCode(ctx, store.DeviceByConsole)
	require.NoError(t, err)
	paired, err := devices.Pair(ctx, d.deviceHTTP(nil), d.srv.URL, devices.PairRequest{Code: code.Code, Name: name})
	require.NoError(t, err)
	dev, err := store.DeviceByID(ctx, d.db, paired.DeviceID)
	require.NoError(t, err)
	return dev, paired.Key
}

// devicePairRelay pairs a relay named name (peer_auth off) and returns it with
// its identity.
func (d *deviceDaemon) devicePairRelay(t *testing.T, name string) (store.Device, *devices.RelayIdentity) {
	t.Helper()
	ctx := context.Background()
	id, err := devices.NewRelayIdentity(name)
	require.NoError(t, err)
	code, err := d.pairing.MintCode(ctx, store.DeviceByConsole)
	require.NoError(t, err)
	paired, err := devices.Pair(ctx, d.deviceHTTP(nil), d.srv.URL, devices.PairRequest{Code: code.Code, Name: name, Relay: id})
	require.NoError(t, err)
	dev, err := store.DeviceByID(ctx, d.db, paired.DeviceID)
	require.NoError(t, err)
	return dev, id
}

// devicePlace is one of the places that check the bearer key.
type devicePlace struct {
	name, method, path, body string
	header                   http.Header
}

// devicePlaces are every place the bearer key is checked: MCP
// (internal/mcp verifyBearer), the hooks (handler.go's session-start,
// user-prompt-submit, session-end and stop; plans.go's post-tool-use and
// permission-request; the subagent and taint routes), A2A, and the console
// (classify).
func devicePlaces() []devicePlace {
	jsonHdr := http.Header{"Content-Type": {"application/json"}}
	mcpHdr := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + mcpgo.LATEST_PROTOCOL_VERSION +
		`","capabilities":{},"clientInfo":{"name":"device-test","version":"0"}}}`
	out := []devicePlace{
		{"mcp", http.MethodPost, "/api/mcp", initialize, mcpHdr},
		{"a2a", http.MethodPost, "/api/a2a", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"example"}]}}}`, jsonHdr},
		{"console", http.MethodGet, "/console/?format=json", "", http.Header{"Accept": {"application/json"}}},
	}
	for _, route := range []string{"session-start", "user-prompt-submit", "session-end", "stop", "post-tool-use",
		"permission-request", "subagent-start", "subagent-stop", "session-taint"} {
		out = append(out, devicePlace{"hooks " + route, http.MethodPost, "/api/hooks/" + route + "?client=claude-code",
			`{"session_id":"device-test-session","cwd":"/tmp/device-test"}`, jsonHdr})
	}
	return out
}

// deviceDo sends one request to the place with auth as its Authorization
// header ("" for none) and returns the status.
func (d *deviceDaemon) deviceDo(t *testing.T, client *http.Client, p devicePlace, auth string) int {
	t.Helper()
	req, err := http.NewRequest(p.method, d.srv.URL+p.path, strings.NewReader(p.body))
	require.NoError(t, err)
	for k, v := range p.header {
		req.Header[k] = v
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode
}

// A device key is the owner in every place the bearer key is checked; a
// revoked one, or one never paired, is refused in each of them, as a wrong
// key is.
func TestPairDeviceKey_EveryBearerPlace(t *testing.T) {
	d := deviceNewDaemon(t, false)
	dev, key := d.devicePairKey(t, "example-ci")
	never, _, err := devices.NewKey()
	require.NoError(t, err)
	client := d.deviceHTTP(nil)
	for _, p := range devicePlaces() {
		require.Equal(t, http.StatusOK, d.deviceDo(t, client, p, "Bearer "+deviceSharedKey), "%s: the shared key, as before", p.name)
		require.Equal(t, http.StatusOK, d.deviceDo(t, client, p, "Bearer "+key), "%s: a device key is the owner", p.name)
		require.Equal(t, http.StatusUnauthorized, d.deviceDo(t, client, p, "Bearer "+never), "%s: a key never paired", p.name)
		require.Equal(t, http.StatusUnauthorized, d.deviceDo(t, client, p, ""), "%s: no key", p.name)
	}
	_, err = d.pairing.Revoke(context.Background(), dev.ID, store.DeviceByConsole)
	require.NoError(t, err)
	for _, p := range devicePlaces() {
		require.Equal(t, http.StatusUnauthorized, d.deviceDo(t, client, p, "Bearer "+key), "%s: a revoked device key is refused", p.name)
		require.Equal(t, http.StatusOK, d.deviceDo(t, client, p, "Bearer "+deviceSharedKey), "%s: the shared key still works", p.name)
	}
}

// A paired relay is the owner in every place, with no key at all; revoked,
// it is refused in every place, its connection closed, and an unpaired relay
// certificate is refused before any route.
func TestPairRelay_EveryBearerPlace(t *testing.T) {
	d := deviceNewDaemon(t, false)
	dev, id := d.devicePairRelay(t, "example-laptop")
	relay := d.deviceHTTP(id)
	for _, p := range devicePlaces() {
		require.Equal(t, http.StatusOK, d.deviceDo(t, relay, p, ""), "%s: a paired relay needs no key", p.name)
	}
	stranger, err := devices.NewRelayIdentity("example-laptop")
	require.NoError(t, err)
	for _, p := range devicePlaces() {
		require.Equal(t, http.StatusUnauthorized, d.deviceDo(t, d.deviceHTTP(stranger), p, "Bearer "+deviceSharedKey),
			"%s: a relay certificate without an enrollment is refused, key or no key", p.name)
	}
	_, err = d.pairing.Revoke(context.Background(), dev.ID, store.DeviceByConsole)
	require.NoError(t, err)
	for _, p := range devicePlaces() {
		require.Equal(t, http.StatusUnauthorized, d.deviceDo(t, relay, p, ""), "%s: a revoked relay is refused", p.name)
	}
	// A TLS connection with no client certificate is still judged by the
	// bearer key, as today.
	for _, p := range devicePlaces() {
		require.Equal(t, http.StatusOK, d.deviceDo(t, d.deviceHTTP(nil), p, "Bearer "+deviceSharedKey), p.name)
		require.Equal(t, http.StatusUnauthorized, d.deviceDo(t, d.deviceHTTP(nil), p, ""), p.name)
	}
}

// The host a paired device's requests speak for is its enrolled name: one
// device's key does not authenticate another device's name, through the hooks
// or through MCP.
func TestPairDeviceKey_HostIsTheEnrolledName(t *testing.T) {
	d := deviceNewDaemon(t, false)
	ctx := context.Background()
	_, key := d.devicePairKey(t, "example-laptop")
	_, _ = d.devicePairKey(t, "example-ci")
	client := d.deviceHTTP(nil)

	// A SessionStart hook claiming another device's host.
	hook := devicePlace{"hooks session-start", http.MethodPost, "/api/hooks/session-start?client=claude-code&host=example-ci",
		`{"session_id":"device-host-hook","cwd":"/tmp/device-host","source":"startup"}`, http.Header{"Content-Type": {"application/json"}}}
	require.Equal(t, http.StatusOK, d.deviceDo(t, client, hook, "Bearer "+key))

	// session_start over MCP, with the host header naming the other device.
	trans, err := transport.NewStreamableHTTP(d.srv.URL+"/api/mcp", transport.WithHTTPBasicClient(client),
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + key, mcp.HostHeader: "example-ci"}))
	require.NoError(t, err)
	cli := mcpclient.NewClient(trans)
	t.Cleanup(func() { _ = cli.Close() })
	require.NoError(t, cli.Start(ctx))
	var initReq mcpgo.InitializeRequest
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "device-host", Version: "0"}
	_, err = cli.Initialize(ctx, initReq)
	require.NoError(t, err)
	var call mcpgo.CallToolRequest
	call.Params.Name = "session_start"
	call.Params.Arguments = map[string]any{"name": "device-host-mcp", "cwd": "/tmp/device-host"}
	res, err := cli.CallTool(ctx, call)
	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)

	rows, err := d.db.QueryContext(ctx, `SELECT name, host FROM sessions ORDER BY name`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	hosts := map[string]string{}
	for rows.Next() {
		var name, host string
		require.NoError(t, rows.Scan(&name, &host))
		hosts[name] = host
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, hosts)
	for name, host := range hosts {
		require.Equal(t, "example-laptop", host, "session %s speaks for the key's own device", name)
	}
}

// Under peer_auth a code without approval enrolls nothing: a waiting relay is
// refused in every place, and passes once the owner approved it. A paired
// device is then no more than a bearer request: the owner's TCP view, holding
// no lease, and a bearer request carrying relay attributes is judged as the
// bearer request it is.
func TestPairPeerAuth_ACodeAloneOpensNothing(t *testing.T) {
	d := deviceNewDaemon(t, true)
	ctx := context.Background()
	code, err := d.pairing.MintCode(ctx, store.DeviceByConsole)
	require.NoError(t, err)
	require.Equal(t, devices.ProofPasskey, code.Approval)
	id, err := devices.NewRelayIdentity("example-laptop")
	require.NoError(t, err)
	waiting := make(chan string, 1)
	result := make(chan error, 1)
	go func() {
		_, err := devices.Pair(ctx, d.deviceHTTP(nil), d.srv.URL, devices.PairRequest{Code: code.Code, Name: "example-laptop",
			Relay: id, Poll: 20 * time.Millisecond, Waiting: func(fp string) { waiting <- fp }})
		result <- err
	}()
	var fp string
	select {
	case fp = <-waiting:
	case err := <-result:
		t.Fatalf("the pairing did not wait: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the pairing never said it waits")
	}
	require.Equal(t, id.Fingerprint(), fp, "both ends show the key's fingerprint")
	relay := d.deviceHTTP(id)
	for _, p := range devicePlaces() {
		require.Equal(t, http.StatusUnauthorized, d.deviceDo(t, relay, p, ""), "%s: a relay waiting for approval opens nothing", p.name)
	}
	list, err := d.pairing.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	_, err = store.ApproveDevice(ctx, d.db, list[0].ID, list[0].Fingerprint, "01PASSKEYEXAMPLE", time.Now())
	require.NoError(t, err)
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the pairing did not finish after the approval")
	}
	for _, p := range devicePlaces() {
		require.Equal(t, http.StatusOK, d.deviceDo(t, relay, p, ""), "%s: approved, the relay passes", p.name)
	}

	// What a route sees: the owner's TCP view, no lease, no relay attribute,
	// for a bearer request and for a paired device alike.
	type seenReq struct {
		view   access.View
		ok     bool
		paired bool
		relay  []string
	}
	var seen []seenReq
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, ok := access.FromContext(r.Context())
		_, paired := devices.CredentialOf(r.Context())
		var relayHdrs []string
		for name := range r.Header {
			if strings.HasPrefix(name, devices.RelayAttributePrefix) {
				relayHdrs = append(relayHdrs, name)
			}
		}
		seen = append(seen, seenReq{v, ok, paired, relayHdrs})
		w.WriteHeader(http.StatusOK)
	})
	policy, err := access.New(config.PeerAuth{Enabled: true, Owner: "example-owner",
		Socket: config.PeerSocket{Path: "/opt/seamless/run/seamlessd.sock"}}, func(name string) (access.Account, error) {
		return access.Account{UID: 501, Name: name, Home: "/Users/" + name}, nil
	})
	require.NoError(t, err)
	keyCode, err := d.pairing.MintCode(ctx, store.DeviceByConsole)
	require.NoError(t, err)
	secret, err := devices.NewSecret()
	require.NoError(t, err)
	pending, err := d.pairing.Pair(ctx, devices.PairInput{Code: keyCode.Code, Kind: devices.KindKey, Name: "example-ci", Secret: secret})
	require.NoError(t, err)
	require.Empty(t, pending.Key, "a key device that waits holds no key")
	_, err = store.ApproveDevice(ctx, d.db, pending.Device.ID, pending.Device.Fingerprint, "01PASSKEYEXAMPLE", time.Now())
	require.NoError(t, err)
	collected, err := d.pairing.Status(ctx, secret)
	require.NoError(t, err)
	require.True(t, devices.IsKey(collected.Key))

	h := ownerHandler(policy, d.pairing.Edge(probe))
	for _, auth := range []string{"Bearer " + deviceSharedKey, "Bearer " + collected.Key} {
		req := httptest.NewRequest(http.MethodPost, "https://127.0.0.1/api/mcp", nil)
		req.Header.Set("Authorization", auth)
		req.Header.Set("X-Seamless-Relay-Uid", "0")
		req.Header.Set("X-Seamless-Relay-Chain", "1:100:boot")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
	}
	require.Len(t, seen, 2)
	require.False(t, seen[0].paired, "the shared key is no paired device")
	require.True(t, seen[1].paired)
	for _, s := range seen {
		require.True(t, s.ok)
		require.Empty(t, s.relay, "relay attributes never reach a route")
		require.Empty(t, s.view.Leases(), "a TCP request holds no lease, so it opens no locked project")
		require.True(t, access.OwnerOnly(access.WithView(ctx, s.view)), "the owner's view, as a bearer request's")
		require.False(t, access.PeerAuthenticated(access.WithView(ctx, s.view)), "never the socket's")
	}
}

// pairingOn follows what the install serves, and nothing else.
func TestPairingOn_OnlyForAServerThatServesOthers(t *testing.T) {
	base := config.Defaults()
	require.False(t, pairingOn(base), "a default install pairs nothing")
	for name, tc := range map[string]struct {
		edit func(*config.Config)
		want bool
	}{
		"wildcard bind":           {func(c *config.Config) { c.Addr = "0.0.0.0:8081" }, true},
		"lan bind":                {func(c *config.Config) { c.Addr = "192.168.1.20:8081" }, true},
		"loopback ipv6":           {func(c *config.Config) { c.Addr = "[::1]:8081" }, false},
		"tls":                     {func(c *config.Config) { c.TLS.CertFile, c.TLS.KeyFile = "c.pem", "k.pem" }, true},
		"proxy server_url":        {func(c *config.Config) { c.AdvertisedURL = "https://example-server.example.ts.net" }, true},
		"loopback server_url":     {func(c *config.Config) { c.AdvertisedURL = "http://localhost:8081" }, false},
		"a client pairs no one":   {func(c *config.Config) { c.Role, c.AdvertisedURL = config.RoleClient, "https://example-server:8081" }, false},
		"a socket client neither": {func(c *config.Config) { c.AdvertisedURL = "unix:///opt/seamless/run/seamlessd.sock" }, false},
	} {
		c := config.Defaults()
		tc.edit(&c)
		require.Equal(t, tc.want, pairingOn(c), name)
	}
}

// The closing output names the address, the certificate, how to get a code
// and the client one-liners, which carry the certificate's fingerprint, and
// says the code works once; with a code, the code. Where no other computer
// can pair (plain http to another host) it says why, and offers no code.
func TestServerPairingClosing(t *testing.T) {
	var b bytes.Buffer
	facts := devices.ServerFacts{URL: "https://example-server.example.ts.net:8081", CertFingerprint: "AB:CD", TLS: true}
	serverPairingClosing(&b, facts, "", false)
	out := b.String()
	for _, want := range []string{"https://example-server.example.ts.net:8081", "AB:CD", "seamlessd devices code",
		"The code is the approval", "SEAMLESS_SERVER_FINGERPRINT=AB:CD SEAMLESS_PAIR_CODE=<code>",
		"$env:SEAMLESS_SERVER_FINGERPRINT='AB:CD'; $env:SEAMLESS_PAIR_CODE='<code>'",
		"The code works once, within 10 minutes: the line it leaves in that computer's shell history is dead after use."} {
		require.Contains(t, out, want)
	}
	b.Reset()
	serverPairingClosing(&b, devices.ServerFacts{URL: "http://127.0.0.1:8081"}, "ABCD-EFGH-JKMN-PQRS", true)
	out = b.String()
	require.Contains(t, out, "ABCD-EFGH-JKMN-PQRS")
	require.Contains(t, out, "approve the computer with your passkey")
	require.Contains(t, out, "No TLS")
	require.NotContains(t, out, "FINGERPRINT", "without TLS the one-liners are as they were")
	b.Reset()
	serverPairingClosing(&b, devices.ServerFacts{URL: "http://192.168.1.20:8081"}, "", false)
	out = b.String()
	require.Contains(t, out, "No TLS")
	require.Contains(t, out, "Pairing needs TLS here")
	require.Contains(t, out, "seamlessd setup's address step")
	require.NotContains(t, out, "SEAMLESS_PAIR_CODE", "no one-liner that could only fail")
}

// seamlessd devices: code, list and revoke against the served database,
// with its arguments settled first.
func TestDevicesCommand(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	certPath := filepath.Join(dir, "cert.pem")
	cert, _ := deviceServerCert(t)
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600))
	cfg := config.Defaults()
	cfg.Addr = "0.0.0.0:8081"
	cfg.AdvertisedURL = "https://example-server.example.ts.net:8081"
	cfg.TLS.CertFile, cfg.TLS.KeyFile = certPath, filepath.Join(dir, "key.pem")

	for _, bad := range [][]string{{"nope"}, {"code", "extra"}, {"revoke"}, {"revoke", "a", "b"}, {"list", "--bogus"}} {
		_, err := parseDevices(bad)
		require.Error(t, err, "%v", bad)
	}

	var out bytes.Buffer
	cmd, err := parseDevices([]string{"code", "--json"})
	require.NoError(t, err)
	require.NoError(t, cmd.run(ctx, db, cfg, &out))
	var minted struct {
		Code   string
		Server devices.ServerFacts
		Unix   string
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &minted))
	require.Equal(t, minted.Code, store.FormatPairingCode(minted.Code))
	require.Equal(t, devices.CertFingerprint(cert.Certificate[0]), minted.Server.CertFingerprint)
	require.Contains(t, minted.Unix, minted.Code)
	var by string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT minted_by FROM device_pairing_codes`).Scan(&by))
	require.Equal(t, store.DeviceByTerminal, by)

	svc := devices.New(devices.Config{DB: db, LocalHost: "example-server"})
	secret, err := devices.NewSecret()
	require.NoError(t, err)
	answer, err := svc.Pair(ctx, devices.PairInput{Code: minted.Code, Kind: devices.KindKey, Name: "example-ci", Secret: secret})
	require.NoError(t, err)

	out.Reset()
	cmd, err = parseDevices([]string{"list"})
	require.NoError(t, err)
	require.NoError(t, cmd.run(ctx, db, cfg, &out))
	require.Contains(t, out.String(), "example-ci")
	require.Contains(t, out.String(), answer.Device.KeyPrefix+"...")
	require.NotContains(t, out.String(), answer.Key)

	out.Reset()
	cmd, err = parseDevices([]string{"revoke", "example-ci"})
	require.NoError(t, err)
	require.NoError(t, cmd.run(ctx, db, cfg, &out))
	require.Contains(t, out.String(), "Revoked example-ci")
	got, err := store.DeviceByID(ctx, db, answer.Device.ID)
	require.NoError(t, err)
	require.Equal(t, store.DeviceRevoked, got.Status)
	require.Equal(t, store.DeviceByTerminal, got.RevokedBy)
	cmd, err = parseDevices([]string{"revoke", "example-ci"})
	require.NoError(t, err)
	require.Error(t, cmd.run(ctx, db, cfg, &out), "a revoked device is not live: its name names nothing")
	cmd, err = parseDevices([]string{"revoke", answer.Device.ID})
	require.NoError(t, err)
	require.ErrorIs(t, cmd.run(ctx, db, cfg, &out), store.ErrDeviceEnded)
}

// Doctor's device pairing rows appear only with pairing on.
func TestDevicePairingChecks(t *testing.T) {
	require.Empty(t, devicePairingChecks(context.Background(), config.Defaults(), nil), "a default install has no such rows")
	cfg := config.Defaults()
	cfg.Addr = "0.0.0.0:8081"
	warning := func(cfg config.Config) string {
		t.Helper()
		rows := devicePairingChecks(context.Background(), cfg, nil)
		require.NotEmpty(t, rows)
		warned := ""
		for _, r := range rows {
			require.Equal(t, "device pairing", r.name)
			if r.status == statusWarn && strings.Contains(r.detail, "no TLS") {
				warned = r.detail
			}
		}
		require.NotEmpty(t, warned, "without TLS a relay cannot pair: doctor says so")
		return warned
	}
	line := warning(cfg)
	require.Contains(t, line, "a pairing over plain http from another computer is refused")
	require.Contains(t, line, "seamlessd setup's address step")
	require.NotContains(t, line, "no other computer can pair here", "server_url names loopback: this computer still pairs")

	cfg.Addr = "192.168.1.20:8081"
	require.Contains(t, warning(cfg), "server_url is plain http, so no other computer can pair here, and no code is minted")
}

// devices2CodeConfig is a Server's config with TLS from a fresh certificate
// in dir, and that certificate's DER.
func devices2CodeConfig(t *testing.T, dir string) (config.Config, []byte) {
	t.Helper()
	cert, _ := deviceServerCert(t)
	certPath := filepath.Join(dir, "cert.pem")
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600))
	cfg := config.Defaults()
	cfg.Addr = "0.0.0.0:8081"
	cfg.AdvertisedURL = "https://example-server.example.ts.net:8081"
	cfg.TLS.CertFile, cfg.TLS.KeyFile = certPath, filepath.Join(dir, "key.pem")
	return cfg, cert.Certificate[0]
}

// devices2UnixEnv is the environment a macOS and Linux one-liner gives the
// installer: its SEAMLESS_* assignments, in order.
func devices2UnixEnv(t *testing.T, line string) []string {
	t.Helper()
	pipe, rest, ok := strings.Cut(line, " | ")
	require.True(t, ok, line)
	require.Equal(t, "curl -fsSL https://thereisnospoon.org/install", pipe)
	fields := strings.Fields(rest)
	require.Equal(t, "sh", fields[len(fields)-1], line)
	env := fields[:len(fields)-1]
	for _, f := range env {
		require.Regexp(t, `^SEAMLESS_[A-Z_]+=[^\s'"$;|&]+$`, f, "a plain assignment the shell passes as it is")
	}
	return env
}

// devices2WindowsEnv is the environment a Windows one-liner gives the
// installer: its $env: assignments, in order.
func devices2WindowsEnv(t *testing.T, line string) []string {
	t.Helper()
	const tail = "irm https://thereisnospoon.org/install.ps1 | iex"
	require.True(t, strings.HasSuffix(line, tail), line)
	var env []string
	for _, stmt := range strings.Split(strings.TrimSuffix(line, tail), "; ") {
		if stmt = strings.TrimSpace(stmt); stmt == "" {
			continue
		}
		m := regexp.MustCompile(`^\$env:(SEAMLESS_[A-Z_]+)='([^']*)'$`).FindStringSubmatch(stmt)
		require.Len(t, m, 3, "%q is one quoted assignment", stmt)
		env = append(env, m[1]+"="+m[2])
	}
	return env
}

// seamlessd devices code: the one-liners carry this server's certificate
// fingerprint, in the form setup on the other computer reads
// (SEAMLESS_SERVER_FINGERPRINT), and the code is said to work once. Each
// one-liner, run as it is, is accepted by setup's own knobs: the certificate
// it is shown is compared with no one at the terminal, the code goes only to
// it, and another certificate answering for the address gets nothing.
func TestDevices2_OneLinersCarryTheFingerprintSetupReads(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	cfg, der := devices2CodeConfig(t, dir)
	fp := devices.CertFingerprint(der)

	var out bytes.Buffer
	cmd, err := parseDevices([]string{"code"})
	require.NoError(t, err)
	require.NoError(t, cmd.run(ctx, db, cfg, &out))
	text := out.String()
	require.Contains(t, text, "Certificate fingerprint: "+fp)
	require.Contains(t, text, "SEAMLESS_SERVER_URL=https://example-server.example.ts.net:8081 SEAMLESS_SERVER_FINGERPRINT="+fp+" SEAMLESS_PAIR_CODE=")
	require.Contains(t, text, "$env:SEAMLESS_SERVER_FINGERPRINT='"+fp+"'; $env:SEAMLESS_PAIR_CODE='")
	require.Contains(t, text, "The code works once, within 10 minutes: the line it leaves in that computer's shell history is dead after use.")

	out.Reset()
	cmd, err = parseDevices([]string{"code", "--json"})
	require.NoError(t, err)
	require.NoError(t, cmd.run(ctx, db, cfg, &out))
	var minted struct {
		Code, Unix, Windows string
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &minted))
	unixEnv, windowsEnv := devices2UnixEnv(t, minted.Unix), devices2WindowsEnv(t, minted.Windows)
	want := []string{"SEAMLESS_SETUP=client", "SEAMLESS_SERVER_URL=https://example-server.example.ts.net:8081",
		"SEAMLESS_SERVER_FINGERPRINT=" + fp, "SEAMLESS_PAIR_CODE=" + minted.Code}
	require.Equal(t, want, unixEnv)
	require.Equal(t, want, windowsEnv)

	// The certificate the other computer is shown: the server's, which its
	// own roots do not vouch for, so setup trusts it only once compared.
	shown := clientServerCert{tls: true, fingerprint: fp, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		why: "x509: certificate signed by unknown authority"}

	// macOS and Linux: compared unattended, and the relay's step goes on.
	r, rout, _ := setupTestRun(t, "linux", unixEnv, setupFactsFresh, false, "")
	clientArrange(t, r, shown)
	st, err := clientFirstHalf(t, r)
	require.NoError(t, err, rout.String())
	require.Contains(t, rout.String(), "matches SEAMLESS_SERVER_FINGERPRINT")
	require.Equal(t, setupDeviceRelay, st.device)
	require.Equal(t, fp, st.fingerprint, "the relay pins the certificate compared")

	// Windows: the key device pairs with the code, only to that certificate.
	r, wout, wrote := setupTestRun(t, "windows", windowsEnv, setupFactsFresh, false, "")
	w := clientArrange(t, r, shown)
	_, err = clientFirstHalf(t, r)
	require.NoError(t, err, wout.String())
	require.Contains(t, wout.String(), "matches SEAMLESS_SERVER_FINGERPRINT")
	require.Len(t, w.pairs, 1)
	require.Equal(t, minted.Code, w.pairs[0].Code)
	require.Equal(t, fp, w.pairs[0].ServerFingerprint, "the code goes only to the certificate compared")
	require.Len(t, *wrote, 1)

	// Another certificate answering for the address gets nothing.
	other := shown
	other.fingerprint = devices.CertFingerprint([]byte("another certificate"))
	for goos, env := range map[string][]string{"linux": unixEnv, "windows": windowsEnv} {
		r, _, wrote := setupTestRun(t, goos, env, setupFactsFresh, false, "")
		w := clientArrange(t, r, other)
		_, err := clientFirstHalf(t, r)
		require.ErrorIs(t, err, errSetupRefused, goos)
		require.ErrorContains(t, err, "nothing was sent to it", goos)
		require.Empty(t, w.pairs, goos)
		require.Empty(t, *wrote, goos)
	}
}

// A Server whose address is plain http to another host mints no code: no
// other computer could pair with it (the client refuses plain http off
// loopback, and the edge a pairing over it from another computer). The list
// and revoke still work there, for the devices paired before. A wildcard
// bind with no server_url names loopback, which pairs on this computer as
// before.
func TestDevices2_CodeNeedsTLSWhereNoComputerCanPair(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, edit := range []func(*config.Config){
		func(c *config.Config) { c.Addr = "192.168.1.20:8081" },
		func(c *config.Config) { c.Addr, c.AdvertisedURL = "0.0.0.0:8081", "http://example-server.example:8081" },
	} {
		cfg := config.Defaults()
		edit(&cfg)
		require.True(t, pairingOn(cfg))
		for _, args := range [][]string{{"code"}, {"code", "--json"}} {
			var out bytes.Buffer
			cmd, err := parseDevices(args)
			require.NoError(t, err)
			err = cmd.run(ctx, db, cfg, &out)
			require.ErrorIs(t, err, devices.ErrPairingNeedsTLS, cfg.ServerURL())
			require.ErrorContains(t, err, "seamlessd setup's address step")
			require.Empty(t, out.String())
		}
		var out bytes.Buffer
		cmd, err := parseDevices([]string{"list"})
		require.NoError(t, err)
		require.NoError(t, cmd.run(ctx, db, cfg, &out), "the devices paired before stay listed")
	}
	open, err := store.CountOpenPairingCodes(ctx, db, time.Now())
	require.NoError(t, err)
	require.Zero(t, open, "no code was minted")

	cfg := config.Defaults()
	cfg.Addr = "0.0.0.0:8081"
	cmd, err := parseDevices([]string{"code"})
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, cmd.run(ctx, db, cfg, &out), "loopback pairs on this computer, as before")
	require.Contains(t, out.String(), "SEAMLESS_SERVER_URL=http://127.0.0.1:8081 SEAMLESS_PAIR_CODE=")
}
