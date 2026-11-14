//go:build darwin || linux

package main

// The relay end to end: a real serve in this process (TLS, so pairing is on),
// and the relay in this process too, paired with it through /api/pair. The
// agent side dials only the relay's socket, as seam does with a socket
// server_url, and sends no key.

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/relay"
	"github.com/arctop/seamless/internal/store"
)

const relayE2EKey = "relay-e2e-shared-key-0123456789ab"

// relayE2EServer is a real serve with TLS on a free loopback port.
type relayE2EServer struct {
	base    string
	pool    *x509.CertPool
	certPEM []byte
	dbPath  string
	done    chan error
	stopped bool
}

// relayE2EServe runs serve in this process until the test ends or stop.
func relayE2EServe(t *testing.T) *relayE2EServer {
	t.Helper()
	clearServeEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	certPath, keyPath := filepath.Join(root, "cert.pem"), filepath.Join(root, "key.pem")
	pool := pairProofCert(t, certPath, keyPath)
	certPEM, err := os.ReadFile(certPath)
	require.NoError(t, err)
	data := filepath.Join(root, "data")
	yaml := fmt.Sprintf("addr: '127.0.0.1:%d'\ndata_dir: '%s'\nmcp:\n  api_key: '%s'\nupdate:\n  check: false\ngardener:\n  enabled: false\n"+
		"tls:\n  cert_file: '%s'\n  key_file: '%s'\n", port, data, relayE2EKey, certPath, keyPath)
	cfgPath := filepath.Join(root, "seamless.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(yaml), 0o600))
	t.Setenv("SEAMLESS_CONFIG", cfgPath)
	s := &relayE2EServer{base: fmt.Sprintf("https://127.0.0.1:%d", port), pool: pool, certPEM: certPEM,
		dbPath: filepath.Join(data, "seam.db"), done: make(chan error, 1)}
	go func() { s.done <- runServe([]string{"--config", cfgPath}) }()
	client := pairProofClient(pool, nil)
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := client.Get(s.base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case err := <-s.done:
			t.Fatalf("serve stopped before it answered: %v", err)
		default:
		}
		require.True(t, time.Now().Before(deadline), "serve never answered /healthz")
		<-time.After(50 * time.Millisecond)
	}
	t.Cleanup(func() { s.relayStop(t) })
	return s
}

// relayStop stops serve (SIGTERM, which serve's signal context takes) and
// waits for it.
func (s *relayE2EServer) relayStop(t *testing.T) {
	t.Helper()
	if s.stopped {
		return
	}
	s.stopped = true
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))
	select {
	case err := <-s.done:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Error("serve did not stop")
	}
}

// relayCode mints a pairing code on the console, with the server's key, as
// the owner does on the Devices page.
func (s *relayE2EServer) relayCode(t *testing.T) string {
	t.Helper()
	status, body := pairProofDo(t, pairProofClient(s.pool, nil), http.MethodPost, s.base+"/console/devices/code", relayE2EKey,
		"application/x-www-form-urlencoded", "")
	require.Equal(t, http.StatusOK, status, "%s", body)
	var minted struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(body, &minted))
	require.NotEmpty(t, minted.Code)
	return minted.Code
}

// relayE2EPaired is a real serve and a relay paired with it, serving its
// socket.
type relayE2EPaired struct {
	srv    *relayE2EServer
	layout relay.Layout
	state  relay.State
	agent  *http.Client
}

func relayE2EPair(t *testing.T) relayE2EPaired {
	t.Helper()
	srv := relayE2EServe(t)
	l, env := relayCmdLayout(t)
	require.NoError(t, relayPair(context.Background(), relayPairOptions{dataDir: l.DataDir, env: env,
		pair: relay.PairOptions{Server: srv.base, Code: srv.relayCode(t), Name: "example-laptop", CA: srv.certPEM}},
		&strings.Builder{}, &strings.Builder{}))
	st, err := relay.LoadState(l.DataDir)
	require.NoError(t, err)
	return relayE2EPaired{srv: srv, layout: l, state: st, agent: relayCmdStart(t, l, env)}
}

// An agent that dials only the relay's socket and holds no key reaches the
// server: an MCP call and a hook go through, and the server took both as the
// paired relay's -- its edge matched the relay's certificate, so each speaks
// for the relay's enrolled name whatever host the agent named, and the device
// shows its use. The same hook without the relay is refused. With the server
// down, the relay refuses at once and answers nothing from memory.
func TestRelayE2E_AnAgentReachesTheServerThroughTheRelay(t *testing.T) {
	p := relayE2EPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	trans, err := transport.NewStreamableHTTP(config.SocketBaseURL+"/api/mcp", transport.WithHTTPBasicClient(p.agent),
		transport.WithHTTPHeaders(map[string]string{mcp.HostHeader: "example-ci", "Authorization": "Bearer "}))
	require.NoError(t, err)
	cli := mcpclient.NewClient(trans)
	t.Cleanup(func() { _ = cli.Close() })
	require.NoError(t, cli.Start(ctx))
	var initReq mcpgo.InitializeRequest
	initReq.Params.ProtocolVersion = mcpgo.LATEST_LEGACY_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "relay-e2e", Version: "0"}
	_, err = cli.Initialize(ctx, initReq)
	require.NoError(t, err)
	var call mcpgo.CallToolRequest
	call.Params.Name = "session_start"
	call.Params.Arguments = map[string]any{"name": "relay-e2e-mcp", "cwd": "/tmp/relay-e2e"}
	res, err := cli.CallTool(ctx, call)
	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)

	hook := "/api/hooks/session-start?client=claude-code&host=example-ci"
	hookBody := `{"session_id":"relay-e2e-hook","cwd":"/tmp/relay-e2e","source":"startup"}`
	status, body := relayCmdPost(t, p.agent, hook, hookBody, http.Header{"Content-Type": {"application/json"}})
	require.Equal(t, http.StatusOK, status, body)
	status, _ = pairProofDo(t, pairProofClient(p.srv.pool, nil), http.MethodPost, p.srv.base+hook, "", "application/json", hookBody)
	require.Equal(t, http.StatusUnauthorized, status, "without the relay's certificate the same hook carries no credential")

	db, err := store.OpenExisting(p.srv.dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var mcpHost string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT host FROM sessions WHERE name = 'relay-e2e-mcp'`).Scan(&mcpHost))
	require.Equal(t, "example-laptop", mcpHost, "the MCP call speaks for the relay's enrolled name")
	var hookHost string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT host FROM sessions WHERE claude_session_id = 'relay-e2e-hook'`).Scan(&hookHost))
	require.Equal(t, "example-laptop", hookHost, "so does the hook")
	var status2, approvedBy string
	var lastUsed *string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT status, approved_by, last_used_at FROM devices WHERE id = ?`, p.state.DeviceID).
		Scan(&status2, &approvedBy, &lastUsed))
	require.Equal(t, store.DeviceActive, status2)
	require.Empty(t, approvedBy, "without peer_auth the code approved it")
	require.NotNil(t, lastUsed, "the server's edge recorded the relay's use")

	p.srv.relayStop(t)
	for _, path := range []string{"/healthz", "/api/mcp", hook} {
		status, body := relayCmdPost(t, p.agent, path, hookBody, http.Header{"Content-Type": {"application/json"}})
		require.Equal(t, http.StatusBadGateway, status, path)
		require.Contains(t, body, "seamless relay: the server at "+p.srv.base+" could not be reached", path)
	}
}

// The seam CLI, unchanged, with a socket server_url and no key: through the
// relay it reads the server's health, the console's JSON and calls a tool,
// and the server takes each as the relay's.
func TestRelayE2E_TheSeamCLIDialsTheRelayUnchanged(t *testing.T) {
	// Built before the serve below gives this process a throwaway HOME: the
	// build reads the real module cache.
	cmSeam(t)
	p := relayE2EPair(t)
	seam := cmNewSeamRunner(t, p.layout.Socket)
	code, out, errOut := seam.run(t, "status", "--json")
	require.Equal(t, 0, code, "seam status: %s %s", out, errOut)
	code, out, errOut = seam.run(t, "call", "session_start", `{"name":"relay-e2e-seam","cwd":"/tmp/relay-e2e"}`)
	require.Equal(t, 0, code, "seam call: %s %s", out, errOut)
	code, out, errOut = seam.run(t, "sessions", "--json")
	require.Equal(t, 0, code, "seam sessions: %s %s", out, errOut)
	require.Contains(t, out, "relay-e2e-seam", "the console's JSON came back through the relay")

	db, err := store.OpenExisting(p.srv.dbPath)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var host string
	require.NoError(t, db.QueryRow(`SELECT host FROM sessions WHERE name = 'relay-e2e-seam'`).Scan(&host))
	require.Equal(t, "example-laptop", host)
}

// What the server's side of the wire contract adds (task
// 01M73RFZ9TMRQXWM6GBZPBZN6R, agent JUDGE): under a relay's certificate the
// edge reads the relay headers, and refuses a request whose headers are there
// but malformed (400) -- whatever sent them, the relay itself never does.
func TestRelayJudge_MalformedRelayHeadersAreRefusedAtTheEdge(t *testing.T) {
	p := relayE2EPair(t)
	rt, err := p.state.Transport(p.layout.DataDir)
	require.NoError(t, err)
	direct := &http.Client{Transport: rt, CheckRedirect: config.RefuseRedirect, Timeout: 20 * time.Second}
	good := http.Header{relay.HeaderVersion: {relay.WireVersion}, relay.HeaderUID: {"501"}, relay.HeaderChain: {"example-boot:4242:7"}}
	for name, edit := range map[string]func(http.Header){
		"another version":    func(h http.Header) { h.Set(relay.HeaderVersion, "2") },
		"a uid not decimal":  func(h http.Header) { h.Set(relay.HeaderUID, "-1") },
		"a uid past 2^32-1":  func(h http.Header) { h.Set(relay.HeaderUID, "4294967296") },
		"an empty chain":     func(h http.Header) { h.Set(relay.HeaderChain, "") },
		"an entry not a key": func(h http.Header) { h.Set(relay.HeaderChain, "example-boot:4242") },
		"too many hops": func(h http.Header) {
			h.Set(relay.HeaderChain, strings.Repeat("example-boot:4242:7,", 32)+"example-boot:4242:7")
		},
		"a header twice":      func(h http.Header) { h.Add(relay.HeaderUID, "502") },
		"another relay-ish":   func(h http.Header) { h.Set("X-Seamless-Relay-Extra", "x") },
		"no version":          func(h http.Header) { h.Del(relay.HeaderVersion) },
		"a chain with spaces": func(h http.Header) { h.Set(relay.HeaderChain, "example-boot:4242:7, example-boot:4243:8") },
	} {
		h := good.Clone()
		edit(h)
		req, err := http.NewRequest(http.MethodGet, p.state.ServerURL+"/healthz", nil)
		require.NoError(t, err)
		req.Header = h
		resp, err := direct.Do(req)
		require.NoError(t, err, name)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "%s: refused, never served as the owner", name)
	}
	// What the relay sends parses: its requests pass.
	status, body := relayCmdPost(t, p.agent, "/healthz", "", nil)
	require.Equal(t, http.StatusOK, status, body)
}
