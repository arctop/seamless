package main

// Helpers for the relay matrix (peerauth_relay_matrix_test.go): serve's TCP
// stack under peer_auth with pairing on -- the owner's view, the device edge,
// the relay judge, the real routes -- on a TLS listener that asks a relay for
// its certificate, plus this machine's socket for comparison; relays paired
// through the real pairing flow; and a fake relay: a TLS client with the
// relay's certificate that sets the wire's headers for one local caller, as
// the relay does after reading that caller from its own kernel.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/a2a"
	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/hooks"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const (
	relayvFin  = "example-finance" // confidential and locked
	relayvOpen = "example-open"    // open

	// relayvSecret is in every content field planted in the locked project,
	// and in no probe; relayvOpenToken is the open project's.
	relayvSecret    = "rv7secret"
	relayvOpenToken = "rv2open"

	relayvKey      = "relay-matrix-key"
	relayvHost     = "example-server"
	relayvOwnerUID = 501
	// relayvBoot is the client computer's boot: every process of its chains.
	relayvBoot = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	// relayvLocalBoot is this machine's.
	relayvLocalBoot = "4b1d0c2e-7a3f-4e59-8c61-0d2e4f6a8b9c"

	relayvClaude = "/Users/owner/.local/share/claude/versions/2.1.319"
	relayvSeam   = "/Users/owner/.local/bin/seam"
)

// The relays peer_auth.relays maps (the owner is uid 501 on each), and one it
// does not.
const (
	relayvLaptop = "example-laptop"
	relayvDesk   = "example-desk"
	relayvOld    = "example-old" // mapped, but paired with a code alone
	relayvStudio = "example-studio"
)

// relayvTok is one process of the client computer, as its relay renders it.
func relayvTok(pid int32) access.ProcessToken {
	return access.ProcessToken{PID: pid, Version: uint64(pid) * 1000, Boot: relayvBoot}
}

// relayvLocalTok is one process of this machine.
func relayvLocalTok(pid int32) access.ProcessToken {
	return access.ProcessToken{PID: pid, Version: uint64(pid) * 1000, Boot: relayvLocalBoot}
}

// The client computer's processes: two agents under one terminal, each with
// a bridge; agent A also a shell running a script.
var (
	relayvTerm    = relayvTok(2100)
	relayvAgentA  = relayvTok(2200)
	relayvBridgeA = relayvTok(2210)
	relayvShellA  = relayvTok(2220)
	relayvScriptA = relayvTok(2221)
	relayvAgentB  = relayvTok(2300)
	relayvBridgeB = relayvTok(2310)
)

// relayvCaller is one local caller of a relay: its uid and its chain, the
// peer first, as the relay's kernel reports them.
type relayvCaller struct {
	uid   uint32
	chain []access.ProcessToken
}

// relayvChain is c's chain as the wire carries it.
func (c relayvCaller) relayvChain() string {
	keys := make([]string, len(c.chain))
	for i, t := range c.chain {
		keys[i] = t.Key()
	}
	return strings.Join(keys, ",")
}

// relayvHeaders are the headers the relay sets for c, plus the agent-process
// identity the caller's seam sends (which the relay passes through): the
// agent is the first claude of the chain.
func (c relayvCaller) relayvHeaders() map[string]string {
	h := map[string]string{
		devices.RelayVersionHeader: devices.RelayWireVersion,
		devices.RelayUIDHeader:     strconv.FormatUint(uint64(c.uid), 10),
		devices.RelayChainHeader:   c.relayvChain(),
	}
	for _, t := range c.chain {
		if t.PID == relayvAgentA.PID || t.PID == relayvAgentB.PID {
			h[mcp.AgentProcessHeader] = t.AgentID()
			break
		}
	}
	return h
}

var (
	relayvCallerA      = relayvCaller{uid: relayvOwnerUID, chain: []access.ProcessToken{relayvBridgeA, relayvAgentA, relayvTerm}}
	relayvCallerScript = relayvCaller{uid: relayvOwnerUID, chain: []access.ProcessToken{relayvScriptA, relayvShellA, relayvAgentA, relayvTerm}}
	relayvCallerB      = relayvCaller{uid: relayvOwnerUID, chain: []access.ProcessToken{relayvBridgeB, relayvAgentB, relayvTerm}}
)

// relay2Named is the client computer's kernel's name for each of its
// processes, by pid, as a relay of wire version 2 forwards it.
var relay2Named = map[int32]string{
	relayvTerm.PID:    "/Applications/iTerm.app/Contents/MacOS/iTerm2",
	relayvAgentA.PID:  relayvClaude,
	relayvBridgeA.PID: relayvSeam,
	relayvShellA.PID:  "/bin/zsh",
	relayvScriptA.PID: "/usr/bin/python3",
	relayvAgentB.PID:  relayvClaude,
	relayvBridgeB.PID: relayvSeam,
}

// relay2Headers are the headers a relay of wire version 2 sets for c,
// naming each process of its chain from names ("" for a pid names lacks), and
// the agent-process identity c's seam sends.
func (c relayvCaller) relay2Headers(names map[int32]string) map[string]string {
	h := c.relayvHeaders()
	h[devices.RelayVersionHeader] = devices.RelayWireVersion2
	programs := make([]string, len(c.chain))
	for i, t := range c.chain {
		programs[i] = names[t.PID]
	}
	h[devices.RelayProgramsHeader] = devices.EncodeRelayPrograms(programs)
	return h
}

// relay2RestrictedUID is the restricted account peer_auth.relays maps on
// the laptop, example-agent; its label is relay2RestrictedLabel.
const (
	relay2RestrictedUID   = 502
	relay2RestrictedLabel = "example-agent@" + relayvLaptop
)

// relayvPrograms names this machine's processes for the anchor rule. The
// client computer's are never named here: no kernel of this machine could.
func relayvPrograms(t access.ProcessToken) string {
	if t.Relay != "" || t.Boot != relayvLocalBoot {
		return ""
	}
	switch t.PID {
	case 5200:
		return relayvClaude
	case 5210:
		return relayvSeam
	}
	return ""
}

// relayvDaemon is the matrix's daemon.
type relayvDaemon struct {
	db      *sql.DB
	mgr     *files.Manager
	policy  *access.Policy
	svc     *approval.Service
	pairing *devices.Service
	mux     http.Handler
	tcp     *httptest.Server
	pool    *x509.CertPool
	logger  *slog.Logger

	finMem, openMem string // the planted memories' names
}

func newRelayvDaemon(t *testing.T) *relayvDaemon {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(ctx, db, relayvHost))
	for _, slug := range []string{relayvFin, relayvOpen} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, relayvFin, core.IsolationConfidential))

	cfg := config.Defaults()
	cfg.PeerAuth = config.PeerAuth{
		Enabled: true, Owner: "owner",
		Socket:      config.PeerSocket{Path: "/opt/seamless/run/seamlessd.sock"},
		ForceLocked: []string{relayvFin},
		Relays: map[string]config.PeerRelay{
			relayvLaptop: {OwnerUID: relayvOwnerUID, Restricted: map[string]uint32{"example-agent": relay2RestrictedUID}},
			relayvDesk:   {OwnerUID: relayvOwnerUID}, relayvOld: {OwnerUID: relayvOwnerUID}},
	}
	require.NoError(t, cfg.Validate())
	policy, err := access.New(cfg.PeerAuth, func(name string) (access.Account, error) {
		if name == "owner" {
			return access.Account{UID: relayvOwnerUID, Name: "owner", Home: "/Users/owner"}, nil
		}
		return access.Account{}, fmt.Errorf("no such account %q", name)
	})
	require.NoError(t, err)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	ret := retrieve.New(db, nil, config.Defaults().Budgets, nil)
	ret.SetBodyReader(mgr.Store())
	rec := events.NewRecorder(db)
	garden := gardener.New(db, mgr, nil, nil, rec, gardener.Config{}, nil)
	screen := screening.New(screening.Config{DB: db, Files: mgr, Events: rec, Gardener: garden,
		Enabled: func(context.Context) bool { return false }})
	rpID, origins := cfg.RelyingParty()
	verifier, err := passkey.New(passkey.Config{RPID: rpID, Origins: origins})
	require.NoError(t, err)
	svc := approval.New(approval.Config{
		DB: db, Passkeys: verifier, Origins: origins, UserName: "owner", UserDisplayName: "Seamless owner on " + relayvHost,
		Events: rec, Host: relayvHost, LeaseCap: cfg.PeerAuth.LeaseCap(), Programs: relayvPrograms, Logger: logger,
	})
	mcpSrv := mcp.New(mcp.Config{
		DB: db, Files: mgr, Retrieve: ret, Events: rec, Gardener: garden, Screening: screen,
		APIKey: relayvKey, LocalHost: relayvHost, Logger: logger, Approvals: svc, OfferAccessRequest: policy.HasLocked(),
	})
	a2aSrv, err := a2a.New(a2a.Config{Retrieve: ret, Events: rec, APIKey: relayvKey, Version: "test",
		Endpoint: "https://127.0.0.1/api/a2a", Logger: logger})
	require.NoError(t, err)
	hooksH := hooks.NewHandler(hooks.Config{DB: db, Retrieve: ret, Events: rec, Files: mgr, APIKey: relayvKey,
		LocalHost: relayvHost, Logger: logger, Screening: screen, RevokeSessionLeases: svc.RevokeSession, Approvals: svc})
	views, err := store.OpenViews(dbPath, policy.OwnerLocked())
	require.NoError(t, err)
	t.Cleanup(func() { _ = views.Close() })
	base, err := views.For(nil)
	require.NoError(t, err)
	pairing := devices.New(devices.Config{DB: db, Events: rec, PeerAuth: true, LocalHost: relayvHost,
		Server: devices.ServerFacts{URL: "https://127.0.0.1", TLS: true}, Logger: logger})
	consoleSrv, err := console.New(console.Config{
		DB: base, WriteDB: db, Files: mgr, Gardener: garden, Events: rec.ReadingFrom(base), Screening: screen,
		Retrieve: ret, APIKey: relayvKey, DataDir: dir, Approvals: svc, Views: views, Devices: pairing, Logger: logger,
	})
	require.NoError(t, err)
	mux := daemonRoutes{
		healthz: healthzHandler(db, "instance", nil), mcp: mcpSrv.Handler(), a2a: a2aSrv.Handler(),
		agentCard: a2aSrv.CardHandler(), hooks: hooksH, console: consoleSrv,
	}.mux()

	// runServe's TCP order: the owner's view, the device edge, the relay
	// judge, the routes; the refusal watch outside, the Host guard and the
	// tracker outside that.
	routes := refusalWatch(screen, ownerHandler(policy, pairing.Edge(relayHandler(policy, storeLeases(db), logger, mux))))
	cert, pool := deviceServerCert(t)
	ts := httptest.NewUnstartedServer(daemonHandler("127.0.0.1:8081", nil, newActivityTracker(time.Now), routes))
	ts.TLS = devices.WithRelayClientAuth(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}})
	ts.StartTLS()
	t.Cleanup(func() { closeServer(ts) })

	d := &relayvDaemon{db: db, mgr: mgr, policy: policy, svc: svc, pairing: pairing, mux: mux, tcp: ts, pool: pool, logger: logger}
	d.finMem = d.plant(t, relayvFin, relayvSecret)
	d.openMem = d.plant(t, relayvOpen, relayvOpenToken)
	return d
}

// plant writes a memory into project with word in its every content field,
// as background work: no View, no fence.
func (d *relayvDaemon) plant(t *testing.T, project, word string) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	name := strings.TrimPrefix(project, "example-") + "-ledger-rule"
	_, err = d.mgr.WriteMemory(context.Background(), core.Memory{ID: id, Kind: core.KindGotcha, Name: name,
		Description: "the ledger " + word, Project: project, Body: "body " + word, Created: now, Updated: now, ValidFrom: now})
	require.NoError(t, err)
	return name
}

// httpClient trusts the daemon's certificate and presents id's, when id is
// set, as a relay's transport does.
func (d *relayvDaemon) httpClient(id *devices.RelayIdentity) *http.Client {
	base := &tls.Config{RootCAs: d.pool, MinVersion: tls.VersionTLS12}
	if id != nil {
		base = id.ClientTLS(base)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: base}, Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// pair pairs a relay named name through the real flow: a code minted on the
// console, the relay's pairing over TLS with its certificate, waiting, the
// owner's approval of the fingerprint both ends show (here in the store, as
// the Devices page's passkey ceremony writes it), done.
func (d *relayvDaemon) pair(t *testing.T, name string) (store.Device, *devices.RelayIdentity) {
	t.Helper()
	ctx := context.Background()
	id, err := devices.NewRelayIdentity(name)
	require.NoError(t, err)
	code, err := d.pairing.MintCode(ctx, store.DeviceByConsole)
	require.NoError(t, err)
	waiting := make(chan string, 1)
	done := make(chan error, 1)
	var paired devices.Paired
	go func() {
		var err error
		paired, err = devices.Pair(ctx, d.httpClient(nil), d.tcp.URL, devices.PairRequest{Code: code.Code, Name: name, Relay: id,
			Poll: 10 * time.Millisecond, Waiting: func(fp string) { waiting <- fp }})
		done <- err
	}()
	var fp string
	select {
	case fp = <-waiting:
	case err := <-done:
		t.Fatalf("the pairing did not wait for the owner: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the pairing never said it waits")
	}
	require.Equal(t, id.Fingerprint(), fp)
	list, err := d.pairing.List(ctx)
	require.NoError(t, err)
	var pending store.Device
	for _, dev := range list {
		if dev.Name == name && dev.Status == store.DevicePending {
			pending = dev
		}
	}
	require.NotEmpty(t, pending.ID, "the pairing waits on the Devices page")
	_, err = store.ApproveDevice(ctx, d.db, pending.ID, fp, "01PASSKEYEXAMPLE", time.Now())
	require.NoError(t, err)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the pairing did not finish after the approval")
	}
	dev, err := store.DeviceByID(ctx, d.db, paired.DeviceID)
	require.NoError(t, err)
	require.Equal(t, store.DeviceActive, dev.Status)
	require.NotEmpty(t, dev.ApprovedBy)
	return dev, id
}

// pairCodeOnly records a relay paired the way an install without peer_auth
// pairs one: its code alone approved it (store.PairDevice, not pending).
func (d *relayvDaemon) pairCodeOnly(t *testing.T, name string) (store.Device, *devices.RelayIdentity) {
	t.Helper()
	ctx := context.Background()
	id, err := devices.NewRelayIdentity(name)
	require.NoError(t, err)
	code, _, err := store.MintPairingCode(ctx, d.db, store.DeviceByConsole, time.Now())
	require.NoError(t, err)
	secret := sha256.Sum256([]byte("secret:" + name))
	spki := id.PublicKey()
	dev, err := store.PairDevice(ctx, d.db, code, store.NewDevice{Kind: store.DeviceRelayKind, Name: name,
		Fingerprint: devices.PublicKeyFingerprint(spki), PublicKey: spki, KeyHash: devices.PublicKeyHash(spki),
		SecretHash: hex.EncodeToString(secret[:])}, time.Now())
	require.NoError(t, err)
	require.Equal(t, store.DeviceActive, dev.Status)
	require.Empty(t, dev.ApprovedBy, "the code alone approved it")
	return dev, id
}

// pairKey pairs a key device named name and returns its key: under peer_auth
// it waits for the owner, then collects its key with its secret.
func (d *relayvDaemon) pairKey(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()
	code, err := d.pairing.MintCode(ctx, store.DeviceByConsole)
	require.NoError(t, err)
	secret, err := devices.NewSecret()
	require.NoError(t, err)
	pending, err := d.pairing.Pair(ctx, devices.PairInput{Code: code.Code, Kind: devices.KindKey, Name: name, Secret: secret})
	require.NoError(t, err)
	_, err = store.ApproveDevice(ctx, d.db, pending.Device.ID, pending.Device.Fingerprint, "01PASSKEYEXAMPLE", time.Now())
	require.NoError(t, err)
	got, err := d.pairing.Status(ctx, secret)
	require.NoError(t, err)
	require.True(t, devices.IsKey(got.Key))
	return got.Key
}

// mcpVia is an MCP client over the daemon's TLS listener: as a relay when id
// is set (its certificate, the headers it forwards), else as a bearer
// request with key; extra headers ride along either way.
func (d *relayvDaemon) mcpVia(t *testing.T, id *devices.RelayIdentity, key string, extra map[string]string) *mcpclient.Client {
	t.Helper()
	ctx := context.Background()
	headers := map[string]string{mcp.HostHeader: "example-claimed-host"}
	if key != "" {
		headers["Authorization"] = "Bearer " + key
	}
	for k, v := range extra {
		headers[k] = v
	}
	trans, err := transport.NewStreamableHTTP(d.tcp.URL+"/api/mcp", transport.WithHTTPBasicClient(d.httpClient(id)),
		transport.WithHTTPHeaders(headers))
	require.NoError(t, err)
	cli := mcpclient.NewClient(trans)
	t.Cleanup(func() { _ = cli.Close() })
	require.NoError(t, cli.Start(ctx))
	var init mcpgo.InitializeRequest
	init.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcpgo.Implementation{Name: "claude-code", Version: "0"}
	_, err = cli.Initialize(ctx, init)
	require.NoError(t, err)
	return cli
}

// socket is this machine's socket for one connection whose kernel peer is
// uid with chain: the real peerHandler over the store's leases.
func (d *relayvDaemon) socket(t *testing.T, uid uint32, chain ...access.ProcessToken) *httptest.Server {
	t.Helper()
	hops := make([]access.ChainHop, len(chain))
	for i, c := range chain {
		hops[i] = access.ChainHop{Token: c, Name: filepath.Base(relayvPrograms(c))}
	}
	cred := peerCred{uid: uid, chain: func() ([]access.ChainHop, error) { return hops, nil }}
	h := peerHandler(d.policy, storeLeases(d.db), d.logger, d.mux)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerCredKey{}, cred)))
	}))
	t.Cleanup(func() { closeServer(srv) })
	return srv
}

// leaseFor plants a live lease on project for agent of the relay relay ("" for
// this machine), as the approval of that agent's request leaves one.
func (d *relayvDaemon) leaseFor(t *testing.T, relay string, agent access.ProcessToken, project string) store.AccessLease {
	t.Helper()
	ctx := context.Background()
	agent.Relay = relay
	a, err := store.CreateAccessApproval(ctx, d.db, store.AccessRequest{Project: project, Kind: store.AccessKindRequest,
		Lease: time.Hour, TTL: 10 * time.Minute, PeerUID: relayvOwnerUID, PeerPID: agent.PID, ProcessKey: agent.Key(),
		Program: relayvClaude}, time.Now())
	require.NoError(t, err)
	lease, err := store.ApproveAccess(ctx, d.db, a.ID, a.Challenge, "daemon-account", time.Now())
	require.NoError(t, err)
	require.Equal(t, relay, lease.RelayID)
	return lease
}

// do sends one request to the daemon's TLS listener through client, with
// headers (and a form, POSTed), and returns its status and body.
func (d *relayvDaemon) do(t *testing.T, client *http.Client, method, target string, form url.Values, headers map[string]string) (int, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, d.tcp.URL+target, body)
	require.NoError(t, err)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(out)
}

// relayvInitialize is one MCP initialize request, the first a client sends.
func relayvInitialize() string {
	return `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + mcpgo.LATEST_PROTOCOL_VERSION +
		`","capabilities":{},"clientInfo":{"name":"relay-matrix","version":"0"}}}`
}

// initStatus posts an MCP initialize through client with headers and returns
// the status.
func (d *relayvDaemon) initStatus(t *testing.T, client *http.Client, headers map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, d.tcp.URL+"/api/mcp", bytes.NewReader([]byte(relayvInitialize())))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode
}

// relayvRead reads the locked project's memory through cli, and reports the
// text and whether it was refused.
func relayvRead(t *testing.T, cli *mcpclient.Client, project, name string) (string, bool) {
	t.Helper()
	return paCall(t, cli, "memory_read", map[string]any{"name": name, "project": project})
}

// relayvJSON decodes a JSON body.
func relayvJSON(t *testing.T, body string, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal([]byte(body), v), body)
}
