//go:build darwin || linux

package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
)

// relaySeen is what the server got of one forwarded request.
type relaySeen struct {
	method, path, rawQuery, host, body string
	header                             http.Header
	// relayKey is the SubjectPublicKeyInfo of the client certificate the
	// relay's TLS connection presented.
	relayKey []byte
}

// relayProbe records every request on seen and answers status with answer.
func relayProbe(seen chan<- relaySeen, status int, answer string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s := relaySeen{method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery, host: r.Host, body: string(b), header: r.Header.Clone()}
		if r.TLS != nil && len(r.TLS.PeerCertificates) == 1 {
			s.relayKey = r.TLS.PeerCertificates[0].RawSubjectPublicKeyInfo
		}
		seen <- s
		w.Header().Set("X-Example-Answer", "kept")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	})
}

// relayChainEntry is one access.ProcessToken key: boot, pid, start.
var relayChainEntry = regexp.MustCompile(`^[A-Za-z0-9-]+:([1-9][0-9]*):[1-9][0-9]*$`)

// relayRequireWire checks the relay headers the server got: the contract's
// four (version 2), naming this test process as the caller and its program
// as this kernel names it, and no other.
func relayRequireWire(t *testing.T, h http.Header) {
	t.Helper()
	require.Equal(t, WireVersion, h.Get(HeaderVersion))
	require.Equal(t, "2", WireVersion, "a relay of this release names programs")
	require.Equal(t, strconv.Itoa(os.Geteuid()), h.Get(HeaderUID), "the uid the relay's kernel reports for its caller")
	chain := strings.Split(h.Get(HeaderChain), ",")
	require.NotEmpty(t, chain)
	require.LessOrEqual(t, len(chain), access.MaxChainHops)
	for _, k := range chain {
		require.Regexp(t, relayChainEntry, k, "each entry is a ProcessToken key")
	}
	require.Equal(t, strconv.Itoa(os.Getpid()), relayChainEntry.FindStringSubmatch(chain[0])[1], "the peer first: this process dialed the socket")
	programs := strings.Split(h.Get(HeaderPrograms), ",")
	require.Len(t, programs, len(chain), "one program per process of the chain")
	peer, err := access.ParseProcessKey(chain[0])
	require.NoError(t, err)
	got, err := devices.DecodeRelayProgram(programs[0])
	require.NoError(t, err, "every entry is in the one encoding")
	require.NotEmpty(t, got, "this live process has a program")
	require.Equal(t, access.ProgramOf(peer), got, "the peer's program, as this kernel names it")
	for _, p := range programs[1:] {
		_, err := devices.DecodeRelayProgram(p)
		require.NoError(t, err)
	}
	var names []string
	for name := range h {
		if strings.HasPrefix(strings.ToLower(name), "x-seamless-relay-") {
			names = append(names, name)
		}
	}
	require.ElementsMatch(t, []string{HeaderVersion, HeaderUID, HeaderChain, HeaderPrograms}, names,
		"the contract's four, and nothing a local client sent")
	require.Len(t, h.Values(HeaderUID), 1)
	require.Len(t, h.Values(HeaderChain), 1)
	require.Len(t, h.Values(HeaderPrograms), 1)
}

// The relay forwards a local request as it came, but for the wire contract:
// what a local client said about itself (relay headers, a bearer key) is
// removed, the kernel's account of the caller is added, and the answer comes
// back as the server gave it.
func TestRelayForward_SetsTheWireHeadersAndRemovesTheCallers(t *testing.T) {
	cert := relayNewCert(t, "example-server")
	seen := make(chan relaySeen, 4)
	ts := relayUpstream(t, cert, relayProbe(seen, http.StatusCreated, "the server's answer\n"))
	l := relayLayoutAt(t)
	st := relayWriteState(t, l, ts.URL, cert.fp, cert.pem)
	client := relayRun(t, l, st)

	header := http.Header{
		"Authorization":            {"Bearer example-planted-key"},
		"X-Seamless-Relay-Uid":     {"0"},
		"X-Seamless-Relay-Chain":   {"example-boot:1:1"},
		"X-Seamless-Relay-Version": {"9"},
		"x-seamless-relay-extra":   {"planted"},
		"X-Seamless-Agent-Process": {"4242.1234567"},
		"X-Seamless-Host":          {"example-laptop"},
		"Mcp-Session-Id":           {"example-mcp-session"},
		"X-Forwarded-For":          {"203.0.113.9"},
		"Content-Type":             {"application/json"},
	}
	status, answer, body := relayDo(t, client, http.MethodPost, "/api/hooks/session-start?client=claude-code&host=example-other",
		header, `{"session_id":"example-session"}`)
	require.Equal(t, http.StatusCreated, status)
	require.Equal(t, "the server's answer\n", body, "the answer comes back as the server gave it")
	require.Equal(t, "kept", answer.Get("X-Example-Answer"))

	got := <-seen
	relayRequireWire(t, got.header)
	require.Empty(t, got.header.Values("Authorization"), "the relay holds no bearer key and drops the caller's")
	require.Equal(t, "4242.1234567", got.header.Get("X-Seamless-Agent-Process"), "the agent process identity passes unchanged")
	require.Equal(t, "example-mcp-session", got.header.Get("Mcp-Session-Id"))
	require.Equal(t, "example-laptop", got.header.Get("X-Seamless-Host"))
	require.Empty(t, got.header.Values("X-Forwarded-For"), "a local client says nothing about where it is")
	require.Equal(t, http.MethodPost, got.method)
	require.Equal(t, "/api/hooks/session-start", got.path)
	require.Equal(t, "client=claude-code&host=example-other", got.rawQuery, "the query is the server's to judge")
	require.Equal(t, `{"session_id":"example-session"}`, got.body)
	u, err := url.Parse(ts.URL)
	require.NoError(t, err)
	require.Equal(t, u.Host, got.host, "the request names the server's host, not the socket's")
	id, err := st.Identity()
	require.NoError(t, err)
	require.Equal(t, id.PublicKey(), got.relayKey, "the relay's TLS connection presented its own certificate")
}

// relayElicitor answers the server's elicitation as the owner's client would.
type relayElicitor struct{ asked chan string }

func (e relayElicitor) Elicit(_ context.Context, req mcpgo.ElicitationRequest) (*mcpgo.ElicitationResult, error) {
	e.asked <- req.Params.Message
	return &mcpgo.ElicitationResult{ElicitationResponse: mcpgo.ElicitationResponse{
		Action: mcpgo.ElicitationResponseActionAccept, Content: map[string]any{"answer": "approved through the relay"}}}, nil
}

// relayRecorder notes the Content-Type a response was sent with.
type relayRecorder struct {
	http.ResponseWriter
	contentType string
}

func (r *relayRecorder) WriteHeader(code int) {
	r.contentType = r.Header().Get("Content-Type")
	r.ResponseWriter.WriteHeader(code)
}

func (r *relayRecorder) Write(b []byte) (int, error) {
	if r.contentType == "" {
		r.contentType = r.Header().Get("Content-Type")
	}
	return r.ResponseWriter.Write(b)
}

func (r *relayRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *relayRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// relayRPC is what the probe reads of one MCP POST.
type relayRPC struct {
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
}

// An MCP elicitation's round trip goes through the relay: the tool call's
// POST answers with an SSE stream that carries the server's question while
// the call stays open, the client's answer comes back as a second POST
// through the relay, and the call ends with the answer -- every request with
// the relay's headers, none of it buffered.
func TestRelayForward_ElicitationRoundTrip(t *testing.T) {
	cert := relayNewCert(t, "example-server")
	srv := mcpserver.NewMCPServer("relay-upstream", "0", mcpserver.WithElicitation())
	srv.AddTool(mcpgo.NewTool("relay_ask", mcpgo.WithDescription("asks the client one question")),
		func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			res, err := srv.RequestElicitation(ctx, mcpgo.ElicitationRequest{Params: mcpgo.ElicitationParams{
				Message: "May example-laptop's agent read example-finance?",
				RequestedSchema: map[string]any{"type": "object",
					"properties": map[string]any{"answer": map[string]any{"type": "string"}}},
			}})
			if err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			content, _ := res.Content.(map[string]any)
			return mcpgo.NewToolResultText(fmt.Sprintf("%s: %v", res.Action, content["answer"])), nil
		})
	mcpHandler := mcpserver.NewStreamableHTTPServer(srv, mcpserver.WithStreamableHTTPProtocolVersions(mcpgo.LegacyProtocolVersions()...))

	var (
		mu  sync.Mutex
		log []string
	)
	note := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		log = append(log, s)
	}
	var headers []http.Header
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		if r.Method != http.MethodPost {
			mcpHandler.ServeHTTP(w, r)
			return
		}
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		r.Body = io.NopCloser(bytes.NewReader(b))
		var rpc relayRPC
		require.NoError(t, json.Unmarshal(b, &rpc))
		what := rpc.Method
		if what == "" && len(rpc.Result) > 0 {
			what = "answer " + string(rpc.Result)
		}
		note("begin " + what)
		rec := &relayRecorder{ResponseWriter: w}
		mcpHandler.ServeHTTP(rec, r)
		note("end " + what + " as " + rec.contentType)
	})
	ts := relayUpstream(t, cert, probe)
	l := relayLayoutAt(t)
	st := relayWriteState(t, l, ts.URL, cert.fp, cert.pem)
	client := relayRun(t, l, st)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	trans, err := transport.NewStreamableHTTP(config.SocketBaseURL+"/api/mcp", transport.WithHTTPBasicClient(client),
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer example-planted-key"}))
	require.NoError(t, err)
	asked := make(chan string, 1)
	cli := mcpclient.NewClient(trans, mcpclient.WithElicitationHandler(relayElicitor{asked: asked}))
	t.Cleanup(func() { _ = cli.Close() })
	require.NoError(t, cli.Start(ctx))
	var initReq mcpgo.InitializeRequest
	initReq.Params.ProtocolVersion = mcpgo.LATEST_LEGACY_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "relay-test", Version: "0"}
	_, err = cli.Initialize(ctx, initReq)
	require.NoError(t, err)
	var call mcpgo.CallToolRequest
	call.Params.Name = "relay_ask"
	res, err := cli.CallTool(ctx, call)
	require.NoError(t, err)
	require.False(t, res.IsError, "%v", res.Content)
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(mcpgo.TextContent)
	require.True(t, ok)
	require.Equal(t, "accept: approved through the relay", text.Text, "the answer made the round trip")
	require.Equal(t, "May example-laptop's agent read example-finance?", <-asked)

	// The client has the call's result as soon as the last event is on the
	// wire, which can be before the upstream handler returns and notes the
	// call's end: wait for that note rather than race it.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range log {
			if strings.HasPrefix(s, "end tools/call") {
				return true
			}
		}
		return false
	}, 10*time.Second, 5*time.Millisecond, "the upstream handler noted the call's end")
	mu.Lock()
	got := append([]string(nil), log...)
	seenHeaders := append([]http.Header(nil), headers...)
	mu.Unlock()
	begin, answered, end := -1, -1, -1
	for i, s := range got {
		switch {
		case s == "begin tools/call":
			begin = i
		case strings.HasPrefix(s, "begin answer ") && strings.Contains(s, "approved through the relay"):
			answered = i
		case strings.HasPrefix(s, "end tools/call"):
			end = i
			require.Equal(t, "end tools/call as text/event-stream", s, "the call's answer streamed as SSE")
		}
	}
	require.True(t, begin >= 0 && answered > begin && end > answered,
		"the client's answer reached the server while the call's stream was still open: %v", got)
	for _, h := range seenHeaders {
		relayRequireWire(t, h)
		require.Empty(t, h.Values("Authorization"))
	}
	require.GreaterOrEqual(t, len(seenHeaders), 4, "initialize, initialized, the call and its answer, each through the relay")
}

// An SSE event reaches the caller when the server sends it, not when the
// stream ends: the relay flushes every write.
func TestRelayForward_StreamsEachEventAsItIsSent(t *testing.T) {
	cert := relayNewCert(t, "example-server")
	release := make(chan struct{})
	ts := relayUpstream(t, cert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: two\n\n")
	}))
	l := relayLayoutAt(t)
	client := relayRun(t, l, relayWriteState(t, l, ts.URL, cert.fp, cert.pem))

	req, err := http.NewRequest(http.MethodGet, config.SocketBaseURL+"/console/events", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	lines := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if s := sc.Text(); s != "" {
				lines <- s
			}
		}
		close(lines)
	}()
	select {
	case s := <-lines:
		require.Equal(t, "data: one", s)
	case <-time.After(10 * time.Second):
		t.Fatal("the first event did not come through before the stream ended: the relay buffers")
	}
	close(release)
	select {
	case s := <-lines:
		require.Equal(t, "data: two", s)
	case <-time.After(10 * time.Second):
		t.Fatal("the second event did not come through")
	}
}

// With the server down, every request answers the relay's own refusal at once:
// 502 and the reason, never anything from memory.
func TestRelayForward_FailsClosedWhenTheServerIsDown(t *testing.T) {
	cert := relayNewCert(t, "example-server")
	var n int
	var mu sync.Mutex
	ts := relayUpstream(t, cert, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n++
		c := n
		mu.Unlock()
		_, _ = fmt.Fprintf(w, "answer %d", c)
	}))
	l := relayLayoutAt(t)
	client := relayRun(t, l, relayWriteState(t, l, ts.URL, cert.fp, cert.pem))
	for want := 1; want <= 2; want++ {
		status, _, body := relayDo(t, client, http.MethodGet, "/console/sessions?format=json", nil, "")
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, fmt.Sprintf("answer %d", want), body, "each request reaches the server: nothing is cached")
	}
	ts.Close()
	for _, path := range []string{"/console/sessions?format=json", "/api/mcp", "/api/hooks/stop?client=claude-code", "/healthz"} {
		status, header, body := relayDo(t, client, http.MethodGet, path, nil, "")
		require.Equal(t, http.StatusBadGateway, status, path)
		require.Equal(t, "no-store", header.Get("Cache-Control"))
		require.Equal(t, "application/json; charset=utf-8", header.Get("Content-Type"))
		msg := relayErr(t, body)
		require.True(t, strings.HasPrefix(msg, "seamless relay: the server at "+ts.URL+" could not be reached"), msg)
		require.Contains(t, msg, "nothing is answered from memory")
		require.NotRegexp(t, `answer [0-9]`, body, "the earlier answer is never served again")
	}
}

// A server presenting another certificate than the pinned one -- even one the
// relay's roots trust -- gets nothing: the handshake fails before a request
// is sent, and the caller is told why.
func TestRelayForward_RefusesAnotherCertificateThanThePinnedOne(t *testing.T) {
	pinned := relayNewCert(t, "example-server")
	other := relayNewCert(t, "example-server")
	seen := make(chan relaySeen, 1)
	ts := relayUpstream(t, other, relayProbe(seen, http.StatusOK, "from the other certificate"))
	l := relayLayoutAt(t)
	// Both roots are trusted, so the chain verifies: only the pin refuses.
	st := relayWriteState(t, l, ts.URL, pinned.fp, append(append([]byte{}, pinned.pem...), other.pem...))
	client := relayRun(t, l, st)
	status, _, body := relayDo(t, client, http.MethodPost, "/api/mcp", http.Header{"Content-Type": {"application/json"}}, `{"jsonrpc":"2.0"}`)
	require.Equal(t, http.StatusBadGateway, status)
	msg := relayErr(t, body)
	require.Contains(t, msg, "a certificate other than the one this relay pinned at pairing (SHA-256 "+pinned.fp+")")
	require.Contains(t, msg, "nothing was sent")
	require.Contains(t, msg, "seamlessd relay pair --replace")
	relayRequireNoUpstream(t, seen)
}

// A certificate the relay's roots do not trust is refused the same way, with
// its own words.
func TestRelayForward_RefusesACertificateThatDoesNotVerify(t *testing.T) {
	served := relayNewCert(t, "example-server")
	trusted := relayNewCert(t, "example-server")
	seen := make(chan relaySeen, 1)
	ts := relayUpstream(t, served, relayProbe(seen, http.StatusOK, "unverified"))
	l := relayLayoutAt(t)
	client := relayRun(t, l, relayWriteState(t, l, ts.URL, served.fp, trusted.pem))
	status, _, body := relayDo(t, client, http.MethodGet, "/healthz", nil, "")
	require.Equal(t, http.StatusBadGateway, status)
	require.Contains(t, relayErr(t, body), "presented a certificate that does not verify")
	relayRequireNoUpstream(t, seen)
}

// A redirect goes back to the caller as the server sent it: the relay
// follows none, so nothing it carries goes anywhere but the pinned server.
func TestRelayForward_FollowsNoRedirect(t *testing.T) {
	cert := relayNewCert(t, "example-server")
	seen := make(chan relaySeen, 2)
	ts := relayUpstream(t, cert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- relaySeen{path: r.URL.Path}
		http.Redirect(w, r, "https://example-elsewhere.example/console/", http.StatusTemporaryRedirect)
	}))
	l := relayLayoutAt(t)
	client := relayRun(t, l, relayWriteState(t, l, ts.URL, cert.fp, cert.pem))
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	status, header, _ := relayDo(t, client, http.MethodGet, "/console", nil, "")
	require.Equal(t, http.StatusTemporaryRedirect, status)
	require.Equal(t, "https://example-elsewhere.example/console/", header.Get("Location"))
	<-seen
	relayRequireNoUpstream(t, seen)
}

// relayNoServer is a transport that fails the test if anything reaches it.
type relayNoServer struct{ t *testing.T }

func (r relayNoServer) RoundTrip(req *http.Request) (*http.Response, error) {
	r.t.Errorf("a request reached the server without the kernel's account of its caller: %s", req.URL)
	return nil, errors.New("no server")
}

// A request whose caller the kernel did not name is refused by the relay
// itself: the server never sees one without the uid and the chain.
func TestRelayForward_RefusesACallerTheKernelDidNotName(t *testing.T) {
	u, err := url.Parse("https://127.0.0.1:1")
	require.NoError(t, err)
	f := NewForwarder(u, "AB", relayNoServer{t}, relayLogger())
	goodHop := access.ChainHop{Token: access.ProcessToken{PID: 42, Version: 7, Boot: "example-boot"}, Name: "claude"}
	for name, tc := range map[string]struct {
		ctx  func(context.Context) context.Context
		want string
	}{
		"no kernel account": {func(c context.Context) context.Context { return c }, "no peer credential"},
		"no uid": {func(c context.Context) context.Context {
			return withPeer(c, peerConn{uidErr: errors.New("example: LOCAL_PEERCRED failed")})
		}, "no peer credential"},
		"no process": {func(c context.Context) context.Context {
			return withPeer(c, peerConn{uid: 501, chainErr: errors.New("example: LOCAL_PEERTOKEN failed")})
		}, "named no process"},
		"the process is gone": {func(c context.Context) context.Context {
			return withPeer(c, peerConn{uid: 501, chain: func() ([]access.ChainHop, error) { return nil, access.ErrPeerGone }})
		}, "could not be read"},
		"an empty chain": {func(c context.Context) context.Context {
			return withPeer(c, peerConn{uid: 501, chain: func() ([]access.ChainHop, error) { return nil, nil }})
		}, "named no process"},
		"an invalid token": {func(c context.Context) context.Context {
			return withPeer(c, peerConn{uid: 501, chain: func() ([]access.ChainHop, error) {
				return []access.ChainHop{goodHop, {Token: access.ProcessToken{PID: 0, Version: 1, Boot: "x"}}}, nil
			}})
		}, "no valid token"},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://localhost/api/mcp", nil)
		req = req.WithContext(tc.ctx(req.Context()))
		rec := httptest.NewRecorder()
		f.ServeHTTP(rec, req)
		require.Equal(t, http.StatusForbidden, rec.Code, name)
		require.Contains(t, relayErr(t, rec.Body.String()), tc.want, name)
	}
}

// The relay's local server sets no deadline a stream could hit: a ReadTimeout
// would cancel a long call's context once its body was read.
func TestRelayServer_NoDeadlineAStreamCouldHit(t *testing.T) {
	srv := NewServer(context.Background(), http.NotFoundHandler(), relayLogger())
	require.Zero(t, srv.ReadTimeout)
	require.Zero(t, srv.WriteTimeout)
	require.NotZero(t, srv.ReadHeaderTimeout, "headers are still bounded")
	require.NotNil(t, srv.ConnContext, "every connection's caller is read from the kernel")
}

// The relay's transport: the one trust decision (the CA copy beside the
// system's roots, never verification switched off), the relay's certificate
// offered with the relay protocol, the server's pinned on top, and a bound on
// the handshake.
func TestRelayTransport_TrustsPinsPresentsAndBoundsTheHandshake(t *testing.T) {
	cert := relayNewCert(t, "example-server")
	l := relayLayoutAt(t)
	st := relayWriteState(t, l, "https://127.0.0.1:8081", cert.fp, cert.pem)
	tr, err := st.Transport(l.DataDir)
	require.NoError(t, err)
	c := tr.TLSClientConfig
	require.NotNil(t, c)
	require.False(t, c.InsecureSkipVerify)
	require.NotNil(t, c.RootCAs, "the CA copied at pairing is trusted")
	require.NotNil(t, c.GetClientCertificate, "the relay presents its certificate")
	require.NotNil(t, c.VerifyConnection, "the server's certificate is pinned")
	require.Contains(t, c.NextProtos, "seamless-relay/1", "so the server asks for the certificate")
	require.Equal(t, tlsHandshakeTimeout, tr.TLSHandshakeTimeout)
	require.False(t, tr.ForceAttemptHTTP2)

	unpinned := st
	unpinned.ServerFingerprint = ""
	_, err = unpinned.Transport(l.DataDir)
	require.Error(t, err, "no transport without a pin")
}
