//go:build darwin || linux

package relay

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/devices"
)

// v2Recorder is a transport that keeps the last request it was handed and
// answers 200.
type v2Recorder struct{ got chan *http.Request }

func (r v2Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.got <- req
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok")),
		Request: req}, nil
}

// v2Forwarder is a forwarder to an example server over rec, naming each
// process by names (pid to program).
func v2Forwarder(t *testing.T, rec http.RoundTripper, names map[int32]string) *Forwarder {
	t.Helper()
	u, err := url.Parse("https://127.0.0.1:1")
	require.NoError(t, err)
	f := NewForwarder(u, "AB", rec, relayLogger())
	f.programOf = func(tok access.ProcessToken) string { return names[tok.PID] }
	return f
}

// v2Caller is a local caller of uid whose chain is the pids given, the peer
// first, of one boot.
func v2Caller(uid uint32, pids ...int32) func(context.Context) context.Context {
	return func(c context.Context) context.Context {
		hops := make([]access.ChainHop, len(pids))
		for i, pid := range pids {
			hops[i] = access.ChainHop{Token: access.ProcessToken{PID: pid, Version: uint64(pid) * 10, Boot: "example-boot"}}
		}
		return withPeer(c, peerConn{uid: uid, chain: func() ([]access.ChainHop, error) { return hops, nil }})
	}
}

// v2Serve sends one request through f as the caller in ctx and returns the
// status and what the server got (nil when nothing was forwarded).
func v2Serve(t *testing.T, f *Forwarder, rec v2Recorder, caller func(context.Context) context.Context, planted http.Header) (int, string, *http.Request) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://localhost/api/mcp", nil)
	for k, v := range planted {
		req.Header[k] = v
	}
	req = req.WithContext(caller(req.Context()))
	w := httptest.NewRecorder()
	f.ServeHTTP(w, req)
	select {
	case got := <-rec.got:
		return w.Code, w.Body.String(), got
	default:
		return w.Code, w.Body.String(), nil
	}
}

// Wire version 2: the relay names every process's program, in the chain's
// order and as many as the chain has, in the one encoding; a process its
// kernel does not name is an empty entry, never a guess; and nothing a local
// client says about programs reaches the server.
func TestRelayForwardV2_NamesEveryProcessOfTheChain(t *testing.T) {
	rec := v2Recorder{got: make(chan *http.Request, 1)}
	f := v2Forwarder(t, rec, map[int32]string{
		4210: "/Users/owner/.local/bin/seam",
		4200: "/Users/owner/.local/share/claude/versions/2.1.319",
		4100: "/Applications/iTerm.app/Contents/MacOS/iTerm2",
		// 4000 exited: the kernel names nothing.
	})
	planted := http.Header{"X-Seamless-Relay-Programs": {"%2Fbin%2Fclaude"}, "X-Seamless-Relay-Version": {"1"}}
	status, _, got := v2Serve(t, f, rec, v2Caller(501, 4210, 4200, 4100, 4000), planted)
	require.Equal(t, http.StatusOK, status)
	require.NotNil(t, got)
	require.Equal(t, "2", got.Header.Get(HeaderVersion))
	require.Equal(t, "example-boot:4210:42100,example-boot:4200:42000,example-boot:4100:41000,example-boot:4000:40000",
		got.Header.Get(HeaderChain))
	require.Equal(t, []string{"/Users/owner/.local/bin/seam,/Users/owner/.local/share/claude/versions/2.1.319," +
		"/Applications/iTerm.app/Contents/MacOS/iTerm2,"}, got.Header.Values(HeaderPrograms),
		"one entry per process, the unnamed one empty; the planted header is gone")

	attrs, present, err := devices.ParseRelayAttributes(got.Header)
	require.NoError(t, err, "what the relay sends, the server reads")
	require.True(t, present)
	require.True(t, attrs.Named())
	require.Equal(t, []string{"/Users/owner/.local/bin/seam", "/Users/owner/.local/share/claude/versions/2.1.319",
		"/Applications/iTerm.app/Contents/MacOS/iTerm2", ""}, attrs.Programs)

	// A one-process chain the kernel names nothing for: the header is there,
	// empty, and still one entry.
	rec2 := v2Recorder{got: make(chan *http.Request, 1)}
	f2 := v2Forwarder(t, rec2, nil)
	status, _, got = v2Serve(t, f2, rec2, v2Caller(501, 4300), nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, []string{""}, got.Header.Values(HeaderPrograms))
	attrs, _, err = devices.ParseRelayAttributes(got.Header)
	require.NoError(t, err)
	require.Equal(t, []string{""}, attrs.Programs)

	// A program with bytes the header cannot carry plain: encoded, and read
	// back exactly.
	odd := "/Applications/Visual Studio Code.app/Contents/Frameworks/Code Helper (Plugin).app/x,yé"
	rec3 := v2Recorder{got: make(chan *http.Request, 1)}
	f3 := v2Forwarder(t, rec3, map[int32]string{4400: odd})
	_, _, got = v2Serve(t, f3, rec3, v2Caller(501, 4400), nil)
	require.NotContains(t, got.Header.Get(HeaderPrograms), ",", "a comma in a program is encoded")
	attrs, _, err = devices.ParseRelayAttributes(got.Header)
	require.NoError(t, err)
	require.Equal(t, []string{odd}, attrs.Programs)
}

// The account pin rule on the client: the relay's own account -- the one root
// pins as the daemon's there, and the one that holds the relay's key -- is
// never a caller the relay vouches for, whatever the server maps its uid to.
// Every other account is forwarded as before.
func TestRelayForwardV2_RefusesItsOwnAccount(t *testing.T) {
	rec := v2Recorder{got: make(chan *http.Request, 1)}
	f := v2Forwarder(t, rec, map[int32]string{4200: "/usr/local/bin/claude"}).RefusingAccount(270)
	status, body, got := v2Serve(t, f, rec, v2Caller(270, 4200), nil)
	require.Equal(t, http.StatusForbidden, status)
	require.Nil(t, got, "nothing reaches the server")
	require.Contains(t, relayErr(t, body), "the relay's own account")

	status, _, got = v2Serve(t, f, rec, v2Caller(502, 4200), nil)
	require.Equal(t, http.StatusOK, status, "a restricted account's caller is forwarded, for the server to judge")
	require.Equal(t, "502", got.Header.Get(HeaderUID))

	// Without RefusingAccount nothing is refused for its uid.
	rec2 := v2Recorder{got: make(chan *http.Request, 1)}
	status, _, _ = v2Serve(t, v2Forwarder(t, rec2, nil), rec2, v2Caller(270, 4200), nil)
	require.Equal(t, http.StatusOK, status)
}

// The relay is never a tunnel (review L2): a local request asking for a
// protocol switch is refused before anything is sent, and a server that
// answers 101 anyway gets its connection closed -- the caller gets 502, and
// what the caller writes next is a new request the relay rewrites, never raw
// bytes on the relay's certificate.
func TestRelayForwardV2_NeverATunnel(t *testing.T) {
	cert := relayNewCert(t, "example-server")
	seen := make(chan http.Header, 4)
	tunneled := make(chan string, 1)
	ts := relayUpstream(t, cert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/switch" {
			seen <- r.Header.Clone()
			w.WriteHeader(http.StatusOK)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: example-proto\r\n\r\n")
		_ = rw.Flush()
		if next, err := http.ReadRequest(rw.Reader); err == nil {
			tunneled <- next.Header.Get(HeaderUID)
		}
	}))
	l := relayLayoutAt(t)
	_ = relayRun(t, l, relayWriteState(t, l, ts.URL, cert.fp, cert.pem))

	c, err := net.Dial("unix", l.Socket)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	require.NoError(t, c.SetDeadline(time.Now().Add(10*time.Second)))
	br := bufio.NewReader(c)
	send := func(raw string) *http.Response {
		_, err := io.WriteString(c, raw)
		require.NoError(t, err)
		resp, err := http.ReadResponse(br, nil)
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return resp
	}
	resp := send("GET /api/anything HTTP/1.1\r\nHost: seamless\r\nConnection: Upgrade\r\nUpgrade: example-proto\r\n\r\n")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "a protocol switch is refused at the relay")
	relayRequireNoUpstream(t, seen)

	resp = send("GET /switch HTTP/1.1\r\nHost: seamless\r\n\r\n")
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, "a server's 101 is never carried")

	resp = send("GET /api/mcp HTTP/1.1\r\nHost: seamless\r\nX-Seamless-Relay-Version: 1\r\nX-Seamless-Relay-Uid: 0\r\n" +
		"X-Seamless-Relay-Chain: forged-boot:1:1\r\n\r\n")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got := <-seen
	relayRequireWire(t, got)
	require.NotEqual(t, "0", got.Get(HeaderUID), "what the caller wrote next was rewritten as a request")
	relayRequireNoUpstream(t, tunneled)
}

// A failure to read the chain still refuses before anything is named or sent.
func TestRelayForwardV2_NamesNothingForAChainItCannotRead(t *testing.T) {
	named := false
	u, err := url.Parse("https://127.0.0.1:1")
	require.NoError(t, err)
	f := NewForwarder(u, "AB", relayNoServer{t}, relayLogger())
	f.programOf = func(access.ProcessToken) string { named = true; return "x" }
	req := httptest.NewRequest(http.MethodGet, "http://localhost/api/mcp", nil)
	req = req.WithContext(withPeer(req.Context(), peerConn{uid: 501, chain: func() ([]access.ChainHop, error) {
		return nil, errors.New("example: the process is gone")
	}}))
	w := httptest.NewRecorder()
	f.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.False(t, named)
}
