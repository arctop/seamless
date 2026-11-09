package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/agentguide"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	seamlessmcp "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// jsonRPCID pulls the id out of one relayed frame, so a test can assert the
// bridge relayed the right reply for the right request.
func jsonRPCID(t *testing.T, frame string) float64 {
	t.Helper()
	var m struct {
		ID float64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(frame), &m), "frame is not JSON-RPC: %s", frame)
	return m.ID
}

func nonEmptyLines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// The headline contract: initialize/tools-list/tools-call round-trip, the
// Mcp-Session-Id minted on initialize replayed on every later POST (so the
// daemon's connection binding -- session_start inheritance -- survives), and a
// notification relayed as silence rather than an empty frame.
func TestBridge_RoundTripPersistsSession(t *testing.T) {
	var mu sync.Mutex
	var seenSessions []string // Mcp-Session-Id header per request, in arrival order

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/mcp", r.URL.Path)
		require.Equal(t, "Bearer testkey", r.Header.Get("Authorization"))
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.Contains(t, r.Header.Get("Accept"), "text/event-stream")

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		require.NoError(t, json.Unmarshal(body, &msg))

		mu.Lock()
		seenSessions = append(seenSessions, r.Header.Get(headerSessionID))
		mu.Unlock()

		// A notification carries no id: 202 with no body, exactly as the daemon's
		// streamable-HTTP server answers one.
		if len(msg.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if msg.Method == "initialize" {
			w.Header().Set(headerSessionID, "sess-123")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      msg.ID,
			"result": map[string]any{
				"method":       msg.Method,
				"instructions": agentguide.MCPInstructions,
			},
		}))
	}))
	defer srv.Close()

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"recall"}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	b := newBridge(srv.URL+"/api/mcp", "testkey", nil)
	require.NoError(t, b.run(context.Background(), strings.NewReader(in), &out))

	// Three replies; the notification relayed as silence.
	frames := nonEmptyLines(out.String())
	require.Len(t, frames, 3)
	require.Equal(t, float64(1), jsonRPCID(t, frames[0]))
	require.Equal(t, float64(2), jsonRPCID(t, frames[1]))
	require.Equal(t, float64(3), jsonRPCID(t, frames[2]))
	var initialized struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal([]byte(frames[0]), &initialized))
	require.Equal(t, agentguide.MCPInstructions, initialized.Result.Instructions)

	// No session id on the first POST; minted on initialize and replayed on all
	// three that follow.
	require.Equal(t, []string{"", "sess-123", "sess-123", "sess-123"}, seenSessions)
}

// A no-id frame gets a 202: the bridge must write nothing, not a blank frame that
// would desync the stdio parser downstream.
func TestBridge_NotificationRelaysNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	var out bytes.Buffer
	b := newBridge(srv.URL+"/api/mcp", "k", nil)
	require.NoError(t, b.run(context.Background(),
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"), &out))
	require.Empty(t, out.String())
}

// rpcReply is one relayed frame, decoded far enough to tell a result from an
// error and to read the id back exactly as the client wrote it.
type rpcReply struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeReply(t *testing.T, frame string) rpcReply {
	t.Helper()
	var r rpcReply
	require.NoError(t, json.Unmarshal([]byte(frame), &r), "frame is not JSON-RPC: %s", frame)
	return r
}

// fakeDaemon answers every JSON-RPC request with an empty result and every
// notification with a 202, hangs up (after reading the whole request) on any
// frame that mentions "hang-up", and counts what it read: served by method,
// hang-ups apart.
type fakeDaemon struct {
	mu      sync.Mutex
	served  []string
	hangUps int
}

func (d *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if bytes.Contains(body, []byte("hang-up")) {
		d.mu.Lock()
		d.hangUps++
		d.mu.Unlock()
		hangUp(w)
		return
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d.mu.Lock()
	d.served = append(d.served, msg.Method)
	d.mu.Unlock()
	if len(msg.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, msg.ID)
}

func (d *fakeDaemon) counts() ([]string, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.served...), d.hangUps
}

// The bridge rides out the restart an automatic update performs. The next call
// is refused while the port is closed, re-dialed until the new daemon listens,
// and served there exactly once -- under the session id the OLD daemon minted,
// which a fresh transport accepts because it checks the id's shape, not its
// origin. The transport is mcp-go's, configured as the daemon configures it.
func TestBridge_RidesOutADaemonRestart(t *testing.T) {
	addr := deadAddr(t)
	var mu sync.Mutex
	var calls []string // the Mcp-Session-Id each served tools/call carried
	daemon := func() http.Handler {
		srv := mcpserver.NewMCPServer("t", "0", mcpserver.WithToolCapabilities(false))
		srv.AddTool(mcp.NewTool("echo"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("echoed"), nil
		})
		transport := mcpserver.NewStreamableHTTPServer(srv,
			mcpserver.WithStreamableHTTPProtocolVersions(mcp.LegacyProtocolVersions()...))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if bytes.Contains(body, []byte(`"tools/call"`)) {
				mu.Lock()
				calls = append(calls, r.Header.Get(headerSessionID))
				mu.Unlock()
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			transport.ServeHTTP(w, r)
		})
	}

	old := serveAt(t, addr, daemon())
	clk := &fakeClock{onWait: func(n int) {
		if n == 2 {
			serveAt(t, addr, daemon()) // the new daemon binds the old address
		}
	}}
	b := newBridge("http://"+addr+"/api/mcp", "k", nil)
	b.retry = clk.policy(proxyDialBudget)
	ctx := context.Background()

	var out bytes.Buffer
	require.NoError(t, b.forward(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+
		mcp.LATEST_LEGACY_PROTOCOL_VERSION+`","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`), &out))
	minted := b.session()
	require.NotEmpty(t, minted, "initialize mints a session id: %s", out.String())
	require.NoError(t, b.forward(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), &out))

	// The update's restart. The old daemon's FIN makes the transport drop its
	// pooled connection well under a millisecond after it lands (dialretry.go);
	// the test drops it by hand rather than race that, since nothing in it waits
	// seconds the way a real restart does.
	old.Close()
	b.client.CloseIdleConnections()

	out.Reset()
	require.NoError(t, b.forward(ctx, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}`), &out))
	reply := decodeReply(t, out.String())
	require.Nil(t, reply.Error, out.String())
	require.Contains(t, string(reply.Result), "echoed")
	require.JSONEq(t, `2`, string(reply.ID))
	require.Len(t, clk.recorded(), 2, "refused twice while the port was closed")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{minted}, calls, "served once, by the new daemon, under the old session id")
}

// A daemon that stays down past the budget gets the request answered in-band
// with a JSON-RPC error naming the cause, and the bridge goes on: it serves the
// next frame once the daemon is back. Exiting instead would take the tools away
// for the rest of the session, since a stdio client does not restart its server.
func TestBridge_AnswersADownDaemonInBandAndKeepsServing(t *testing.T) {
	addr := deadAddr(t)
	d := &fakeDaemon{}
	clk := &fakeClock{}
	b := newBridge("http://"+addr+"/api/mcp", "k", nil)
	b.retry = clk.policy(proxyDialBudget)
	in, out := newFeedStdin(), newFrameSink()
	done := runBridge(b, in, out)

	in.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	failed := decodeReply(t, out.next(t))
	require.JSONEq(t, `1`, string(failed.ID))
	require.NotNil(t, failed.Error)
	require.Equal(t, mcp.INTERNAL_ERROR, failed.Error.Code)
	require.Contains(t, failed.Error.Message, "cannot reach seamlessd at http://"+addr+"/api/mcp")
	require.Contains(t, failed.Error.Message, "request not sent after 34 attempts over 30s")
	require.Equal(t, proxyDialBudget, clk.waited())

	serveAt(t, addr, d) // the daemon is back
	in.send(`{"jsonrpc":"2.0","id":"two","method":"tools/list"}`)
	served := decodeReply(t, out.next(t))
	require.Nil(t, served.Error)
	require.JSONEq(t, `"two"`, string(served.ID), "a string id comes back as the client wrote it")

	in.close()
	require.NoError(t, waitRun(t, done))
	require.Empty(t, out.rest())
	got, hangUps := d.counts()
	require.Equal(t, []string{"tools/list"}, got)
	require.Zero(t, hangUps)
}

// A request the daemon read before the connection dropped is never sent again --
// a tool call is not idempotent, and this one may already have run. It is
// answered in-band instead, and the bridge serves the next frame. A reused
// keep-alive connection is the case net/http has its own retry for.
func TestBridge_NeverResendsAfterSending(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames []string
	}{
		{"fresh connection", []string{
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hang-up"}}`,
			`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		}},
		{"reused keep-alive connection", []string{
			`{"jsonrpc":"2.0","id":0,"method":"ping"}`,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hang-up"}}`,
			`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeDaemon{}
			srv := httptest.NewServer(d)
			t.Cleanup(srv.Close)
			clk := &fakeClock{}
			b := newBridge(srv.URL+"/api/mcp", "k", nil)
			b.retry = clk.policy(proxyDialBudget)

			var out bytes.Buffer
			require.NoError(t, b.run(context.Background(), strings.NewReader(strings.Join(tc.frames, "\n")+"\n"), &out))
			frames := nonEmptyLines(out.String())
			require.Len(t, frames, len(tc.frames), "one reply per request")

			lost := decodeReply(t, frames[len(frames)-2])
			require.JSONEq(t, `1`, string(lost.ID))
			require.NotNil(t, lost.Error, frames[len(frames)-2])
			require.Equal(t, mcp.INTERNAL_ERROR, lost.Error.Code)
			require.Contains(t, lost.Error.Message, "after sending the request")
			require.Contains(t, lost.Error.Message, "may already have run")
			next := decodeReply(t, frames[len(frames)-1])
			require.JSONEq(t, `2`, string(next.ID))
			require.Nil(t, next.Error, frames[len(frames)-1])

			_, hangUps := d.counts()
			require.Equal(t, 1, hangUps, "the daemon must read the call once")
			require.Empty(t, clk.recorded(), "nothing was retried")
		})
	}
}

// A notification, or a response to a server-initiated request, has no reply for
// the bridge to give: its failure goes to stderr, nothing reaches stdout for it,
// and the bridge serves the next frame.
func TestBridge_UnanswerableFailuresGoToStderr(t *testing.T) {
	for _, tc := range []struct {
		name, frame, want string
	}{
		{"notification", `{"jsonrpc":"2.0","method":"notifications/hang-up"}`,
			"seam mcp-proxy: notifications/hang-up: lost the connection to seamlessd"},
		{"response to a server request", `{"jsonrpc":"2.0","id":"hang-up","result":{}}`,
			"seam mcp-proxy: lost the connection to seamlessd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeDaemon{}
			srv := httptest.NewServer(d)
			t.Cleanup(srv.Close)
			b := newBridge(srv.URL+"/api/mcp", "k", nil)
			var errb bytes.Buffer
			b.stderr = &errb

			var out bytes.Buffer
			require.NoError(t, b.run(context.Background(),
				strings.NewReader(tc.frame+"\n"+`{"jsonrpc":"2.0","id":2,"method":"ping"}`+"\n"), &out))
			frames := nonEmptyLines(out.String())
			require.Len(t, frames, 1, "only the request is answered")
			require.JSONEq(t, `2`, string(decodeReply(t, frames[0]).ID))
			require.Contains(t, errb.String(), tc.want)
			_, hangUps := d.counts()
			require.Equal(t, 1, hangUps, "sent once, never again")
		})
	}
}

// Cancelling the bridge's context ends a retry wait at once, and the bridge
// returns rather than answering: it is shutting down.
func TestBridge_CancelEndsARetryWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := newBridge("http://"+deadAddr(t)+"/api/mcp", "k", nil)
	b.retry.backoff = func(int) time.Duration { return time.Hour }
	waiting := make(chan struct{})
	var once sync.Once
	b.retry.wait = func(ctx context.Context, d time.Duration) error {
		once.Do(func() { close(waiting) })
		return waitCtx(ctx, d)
	}

	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- b.run(ctx, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n"), &out)
	}()
	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("returned before waiting to retry: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the refused dial was never retried")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
		require.Empty(t, out.String())
	case <-time.After(10 * time.Second):
		t.Fatal("cancel did not end the retry wait")
	}
}

// A non-2xx that is not a JSON-RPC error is a failed exchange like any other:
// answered in-band, and the bridge goes on. Exiting on it, as the bridge once
// did, would take the tools away over one oversized or refused call.
func TestBridge_Non2xxIsAnsweredInBand(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"server error", http.StatusInternalServerError, "boom", "seamlessd returned 500 Internal Server Error: boom"},
		{"a result is not an error", http.StatusBadRequest, `{"jsonrpc":"2.0","id":1,"result":{}}`, "seamlessd returned 400 Bad Request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				if hits.Add(1) == 1 {
					w.WriteHeader(tc.status)
					_, _ = fmt.Fprint(w, tc.body)
					return
				}
				_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":2,"result":{}}`)
			}))
			t.Cleanup(srv.Close)

			var out bytes.Buffer
			require.NoError(t, newBridge(srv.URL, "k", nil).run(context.Background(), strings.NewReader(
				`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n"+`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n"), &out))
			frames := nonEmptyLines(out.String())
			require.Len(t, frames, 2)
			failed := decodeReply(t, frames[0])
			require.JSONEq(t, `1`, string(failed.ID))
			require.NotNil(t, failed.Error, frames[0])
			require.Contains(t, failed.Error.Message, tc.want)
			require.Nil(t, decodeReply(t, frames[1]).Error, frames[1])
		})
	}
}

// An SSE reply that ends early is answered in-band only if the reply itself did
// not get through: before it, the client is still waiting on the id; after it,
// a second answer would send the id twice. The reply can also arrive in a final
// event with no blank line after it, which only the end-of-stream flush relays.
func TestBridge_SSEStreamThatEndsEarly(t *testing.T) {
	note := "event: message\ndata: " + `{"jsonrpc":"2.0","method":"notifications/progress","params":{}}` + "\n\n"
	reply := "event: message\ndata: " + `{"jsonrpc":"2.0","id":7,"result":{}}` + "\n\n"
	for _, tc := range []struct {
		name   string
		stream string
		hangUp bool     // drop the connection after the stream, mid-response
		want   []string // the method, or result/error and id, of each relayed frame
	}{
		{"connection lost before the reply", note, true, []string{"notifications/progress", "error 7"}},
		{"connection lost after the reply", note + reply, true, []string{"notifications/progress", "result 7"}},
		{"stream closed with no reply", note, false, []string{"notifications/progress", "error 7"}},
		{"reply in an unterminated final event", note + strings.TrimSuffix(reply, "\n\n"), false,
			[]string{"notifications/progress", "result 7"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.stream)
				if tc.hangUp {
					_ = http.NewResponseController(w).Flush() // a failure shows up as missing frames
					hangUp(w)
				}
			}))
			t.Cleanup(srv.Close)

			var out bytes.Buffer
			require.NoError(t, newBridge(srv.URL, "k", nil).run(context.Background(),
				strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/call"}`+"\n"), &out))
			var got []string
			for _, f := range nonEmptyLines(out.String()) {
				var m struct {
					Method string          `json:"method"`
					ID     json.RawMessage `json:"id"`
					Error  json.RawMessage `json:"error"`
				}
				require.NoError(t, json.Unmarshal([]byte(f), &m), f)
				switch {
				case m.Method != "":
					got = append(got, m.Method)
				case len(m.Error) > 0:
					got = append(got, "error "+string(m.ID))
				default:
					got = append(got, "result "+string(m.ID))
				}
			}
			require.Equal(t, tc.want, got, out.String())
		})
	}
}

// The daemon can answer a request with an SSE stream instead of a JSON body; the
// bridge must relay each event's data payload as one stdio frame.
func TestBridge_RelaysSSEReplies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{}}\n\n")
	}))
	defer srv.Close()

	var out bytes.Buffer
	b := newBridge(srv.URL+"/api/mcp", "k", nil)
	require.NoError(t, b.run(context.Background(),
		strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/call"}`+"\n"), &out))

	frames := nonEmptyLines(out.String())
	require.Len(t, frames, 1)
	require.Equal(t, float64(7), jsonRPCID(t, frames[0]))
}

// A frame with no trailing newline before EOF (a client that closes the pipe
// without a final delimiter) must still be forwarded, not dropped.
func TestBridge_ForwardsUnterminatedFinalFrame(t *testing.T) {
	var got int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		got++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()

	b := newBridge(srv.URL+"/api/mcp", "k", nil)
	require.NoError(t, b.run(context.Background(),
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`), io.Discard)) // no "\n"
	require.Equal(t, 1, got)
}

// mcp-proxy is machine-invoked but not a Claude Code hook, so it takes the normal
// usage exit (2), not hook's fail-open 1. It also parses --config and rejects
// stray positionals at parse time.
func TestMCPProxy_ParsesConfigAndTakesNoPositionals(t *testing.T) {
	p, err := parse(commands(), []string{"mcp-proxy", "--config", "/abs/seamless.yaml"})
	require.NoError(t, err)
	require.Equal(t, "/abs/seamless.yaml", p.opts.(*mcpProxyOpts).config)
	require.Empty(t, p.pos)

	_, err = parse(commands(), []string{"mcp-proxy", "extra"})
	require.ErrorContains(t, err, "takes no positional arguments")

	require.Equal(t, 2, mcpProxyCmd.usageExit())
}

// The bridge names the agent that spawned it on every request -- the identity
// that binds the connection to that agent's ambient session -- and sends no
// header at all when it could not resolve one, rather than an empty value.
func TestBridge_ForwardsTheAgentProcess(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get(agentProcessHeader)+"|"+strconv.FormatBool(len(r.Header.Values(agentProcessHeader)) > 0))
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	note := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	b := newBridge(srv.URL+"/api/mcp", "testkey", nil)
	b.agentProcess = "4242.17"
	require.NoError(t, b.run(context.Background(), strings.NewReader(note), io.Discard))

	anon := newBridge(srv.URL+"/api/mcp", "testkey", nil)
	require.NoError(t, anon.run(context.Background(), strings.NewReader(note), io.Discard))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"4242.17|true", "|false"}, seen)
}

// statelessToolCall is a tools/call frame on the stateless MCP revision: no
// handshake, the version declared in _meta. A stdio client writes exactly this,
// with no headers.
const statelessToolCall = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","arguments":{},` +
	`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",` +
	`"io.modelcontextprotocol/clientInfo":{"name":"t","version":"0"},"io.modelcontextprotocol/clientCapabilities":{}}}}`

// echoTransport is mcp-go's real streamable transport over one trivial tool, so
// the bridge is checked against the header validation the daemon actually runs.
func echoTransport(t *testing.T, opts ...mcpserver.StreamableHTTPOption) string {
	t.Helper()
	srv := mcpserver.NewMCPServer("t", "0", mcpserver.WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("echo"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("echoed"), nil
	})
	ts := httptest.NewServer(mcpserver.NewStreamableHTTPServer(srv, opts...))
	t.Cleanup(ts.Close)
	return ts.URL
}

// A stateless frame through the bridge must reach the version check with the
// headers that revision requires, and its refusal must come back as the reply:
// the daemon serves only the session revisions, and the refusal naming them is
// how the client learns to negotiate down. Without the headers it would be a
// header mismatch instead; relaying nothing (a fatal exit) would restart the
// client into the same refusal.
func TestBridge_StatelessRefusalIsRelayed(t *testing.T) {
	url := echoTransport(t, mcpserver.WithStreamableHTTPProtocolVersions(mcp.LegacyProtocolVersions()...))
	var out bytes.Buffer
	require.NoError(t, newBridge(url, "k", nil).run(context.Background(), strings.NewReader(statelessToolCall+"\n"), &out))

	var reply struct {
		ID    float64 `json:"id"`
		Error struct {
			Code int `json:"code"`
			Data struct {
				Supported []string `json:"supported"`
			} `json:"data"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &reply), out.String())
	require.Equal(t, float64(7), reply.ID)
	require.Equal(t, mcp.UNSUPPORTED_PROTOCOL_VERSION, reply.Error.Code, out.String())
	require.Contains(t, reply.Error.Data.Supported, mcp.LATEST_LEGACY_PROTOCOL_VERSION)
}

// Served the stateless revision, the same frame is answered: the bridge supplies
// every routing header a tools/call needs (version, method and name).
func TestBridge_StatelessFrameIsServed(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, newBridge(echoTransport(t), "k", nil).run(context.Background(),
		strings.NewReader(statelessToolCall+"\n"), &out))
	require.Contains(t, out.String(), "echoed")
	require.Equal(t, float64(7), jsonRPCID(t, out.String()))
}

// feedStdin is a stdin the test writes frames to while the bridge runs, and
// closes for EOF. Feeding never blocks the test, so a bridge that stops
// reading shows up as a missing frame on stdout rather than a hung test.
type feedStdin struct {
	lines chan string
	buf   []byte
}

func newFeedStdin() *feedStdin { return &feedStdin{lines: make(chan string, 16)} }

func (s *feedStdin) send(frame string) { s.lines <- frame }
func (s *feedStdin) close()            { close(s.lines) }

func (s *feedStdin) Read(p []byte) (int, error) {
	if len(s.buf) == 0 {
		l, ok := <-s.lines
		if !ok {
			return 0, io.EOF
		}
		s.buf = []byte(l + "\n")
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

// frameSink is a bridge's stdout the test reads frame by frame as the bridge
// writes them.
type frameSink struct{ frames chan string }

func newFrameSink() *frameSink { return &frameSink{frames: make(chan string, 64)} }

func (s *frameSink) Write(p []byte) (int, error) {
	for l := range strings.SplitSeq(strings.TrimSuffix(string(p), "\n"), "\n") {
		s.frames <- l
	}
	return len(p), nil
}

// next waits for the bridge's next frame.
func (s *frameSink) next(t *testing.T) string {
	t.Helper()
	select {
	case f := <-s.frames:
		return f
	case <-time.After(10 * time.Second):
		t.Fatal("no frame reached stdout: the bridge is stuck")
		return ""
	}
}

// rest drains what the bridge wrote after the frames a test read; call it
// once run has returned.
func (s *frameSink) rest() []string {
	var out []string
	for {
		select {
		case f := <-s.frames:
			out = append(out, f)
		default:
			return out
		}
	}
}

// runBridge runs b over in and out on its own goroutine and returns run's
// result channel.
func runBridge(b *bridge, in io.Reader, out io.Writer) <-chan error {
	done := make(chan error, 1)
	go func() { done <- b.run(context.Background(), in, out) }()
	return done
}

// waitRun waits for run to return.
func waitRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after stdin closed")
		return nil
	}
}

// elicitingDaemon is a daemon whose tool call asks the client a question
// mid-call, as the in-session approval dialog does: the call's POST is
// upgraded to SSE, carries an elicitation/create, and stays open until the
// client's answer arrives on a POST of its own; only then does the call end.
type elicitingDaemon struct {
	answered chan []byte   // the answer POST's body
	stop     chan struct{} // closed at cleanup, so a stuck call lets the server close
	open     atomic.Bool   // the tool call's POST is still open

	mu             sync.Mutex
	answerSession  string   // the Mcp-Session-Id the answer carried
	answerWhenOpen bool     // the answer arrived while the call was still open
	served         []string // every other request's method, and whether it came while the call was open
}

func newElicitingDaemon(t *testing.T) (*elicitingDaemon, string) {
	t.Helper()
	d := &elicitingDaemon{answered: make(chan []byte, 1), stop: make(chan struct{})}
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(d.stop) }) // runs first: cleanups are LIFO
	return d, srv.URL + "/api/mcp"
}

func (d *elicitingDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch {
	case msg.Method == "initialize":
		w.Header().Set(headerSessionID, "sess-e")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, msg.ID)
	case msg.Method == "tools/call":
		d.open.Store(true)
		defer d.open.Store(false)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message\ndata: "+
			`{"jsonrpc":"2.0","id":0,"method":"elicitation/create","params":{"message":"Approve?","requestedSchema":{"type":"object","properties":{}}}}`+"\n\n")
		_ = http.NewResponseController(w).Flush() // a failure shows up as a missing frame
		var answer []byte
		select {
		case answer = <-d.answered:
		case <-r.Context().Done():
			return
		case <-d.stop:
			return
		}
		var got struct {
			Result struct {
				Action string `json:"action"`
			} `json:"result"`
		}
		if err := json.Unmarshal(answer, &got); err != nil {
			got.Result.Action = "unreadable: " + err.Error() // fails the test's check of the reply
		}
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n",
			fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"answered %s"}]}}`, msg.ID, got.Result.Action))
	case msg.Method == "" && len(msg.ID) > 0: // the client's answer to the elicitation
		d.mu.Lock()
		d.answerSession = r.Header.Get(headerSessionID)
		d.answerWhenOpen = d.open.Load()
		d.mu.Unlock()
		select {
		case d.answered <- body:
		default:
		}
		w.WriteHeader(http.StatusAccepted)
	case len(msg.ID) > 0:
		d.mu.Lock()
		d.served = append(d.served, msg.Method+" open="+strconv.FormatBool(d.open.Load()))
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, msg.ID)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

// A server request inside a tool call (the approval dialog's elicitation) is
// relayed to the client, and the client's answer is carried back while the
// call is still open: the bridge reads stdin while a call is in flight. Read
// in turn, the answer would sit unread behind a call that waits on it. A
// request the client wrote in between still waits its turn, and its reply
// follows the call's on stdout.
func TestBridge_RelaysAnAnswerWhileTheCallWaitsOnIt(t *testing.T) {
	d, url := newElicitingDaemon(t)
	in, out := newFeedStdin(), newFrameSink()
	done := runBridge(newBridge(url, "k", nil), in, out)

	in.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	require.JSONEq(t, `1`, string(decodeReply(t, out.next(t)).ID))
	in.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"recall"}}`)

	var ask struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	frame := out.next(t)
	require.NoError(t, json.Unmarshal([]byte(frame), &ask), frame)
	require.Equal(t, "elicitation/create", ask.Method)
	require.JSONEq(t, `0`, string(ask.ID))

	in.send(`{"jsonrpc":"2.0","id":3,"method":"ping"}`) // behind the call: waits its turn
	in.send(`{"jsonrpc":"2.0","id":0,"result":{"action":"accept","content":{}}}`)
	result := decodeReply(t, out.next(t))
	require.JSONEq(t, `2`, string(result.ID))
	require.Nil(t, result.Error)
	require.Contains(t, string(result.Result), "answered accept")
	require.JSONEq(t, `3`, string(decodeReply(t, out.next(t)).ID))

	in.close()
	require.NoError(t, waitRun(t, done))
	require.Empty(t, out.rest(), "the answer's 202 relays nothing")
	d.mu.Lock()
	defer d.mu.Unlock()
	require.True(t, d.answerWhenOpen, "the answer reached the daemon while the call was open")
	require.Equal(t, "sess-e", d.answerSession, "the answer carries the session the call runs under")
	require.Equal(t, []string{"ping open=false"}, d.served, "the ping was sent once the call had ended")
}

// notifications/cancelled is about the call in flight, so it reaches the
// daemon while that call is still open: here the call ends only when its
// cancel arrives.
func TestBridge_SendsACancelWhileTheCallIsInFlight(t *testing.T) {
	cancelled, stop := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if bytes.Contains(body, []byte(`"notifications/cancelled"`)) {
			once.Do(func() { close(cancelled) })
			w.WriteHeader(http.StatusAccepted)
			return
		}
		select {
		case <-cancelled:
		case <-r.Context().Done():
			return
		case <-stop:
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32800,"message":"request cancelled"}}`)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stop) })

	in, out := newFeedStdin(), newFrameSink()
	done := runBridge(newBridge(srv.URL+"/api/mcp", "k", nil), in, out)
	in.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"recall"}}`)
	in.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"user"}}`)
	reply := decodeReply(t, out.next(t))
	require.JSONEq(t, `1`, string(reply.ID))
	require.NotNil(t, reply.Error)

	in.close()
	require.NoError(t, waitRun(t, done))
	require.Empty(t, out.rest(), "the cancel's 202 relays nothing")
}

// brokenStdout is a client that has gone away.
type brokenStdout struct{}

func (brokenStdout) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// A frame sent at once still ends the bridge when its relay finds stdout
// broken, as it did when every frame was sent in turn: here the daemon refuses
// an answer with a JSON-RPC error, which is relayed.
func TestBridge_BrokenStdoutUnderAnAnswerEndsTheBridge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":0,"error":{"code":-32600,"message":"no"}}`)
	}))
	t.Cleanup(srv.Close)

	err := newBridge(srv.URL+"/api/mcp", "k", nil).run(context.Background(),
		strings.NewReader(`{"jsonrpc":"2.0","id":0,"result":{"action":"accept"}}`+"\n"), brokenStdout{})
	require.ErrorIs(t, err, errWriteStdout)
	require.ErrorIs(t, err, io.ErrClosedPipe)
}

// ---------------------------------------------------------------------------
// A daemon restart under a live bridge (the real bridge, the real daemon MCP
// surface, the real approval service; only the socket's kernel facts and the
// Touch ID helper are played by the test)
// ---------------------------------------------------------------------------

const (
	rdProject = "example-vault"
	rdBoot    = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"
	rdOwner   = 501
)

// rdHost is this machine: the bridge names it on every request (X-Seamless-Host),
// and the agent's session is looked up on it.
var rdHost = config.Hostname()

var (
	rdBridge = access.ProcessToken{PID: 900, Version: 9000, Boot: rdBoot}
	rdAgent  = access.ProcessToken{PID: 800, Version: 8000, Boot: rdBoot}
)

// restartDaemon is a daemon's MCP surface whose process the test can replace
// under a live connection: restart() builds a new server over the same
// database and puts it behind the same listener, as the new process of a real
// restart does. Everything in memory -- the old transport's sessions and what
// their initialize left on them -- goes with the old server.
type restartDaemon struct {
	t       *testing.T
	db      *sql.DB
	mgr     *files.Manager
	ret     *retrieve.Service
	rec     *events.Recorder
	policy  *access.Policy
	svc     *approval.Service
	program string
	handler atomic.Pointer[http.Handler]
}

func newRestartDaemon(t *testing.T, program string) *restartDaemon {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	_, err = store.EnsureProject(ctx, db, rdProject, rdProject)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, db, rdProject, core.IsolationConfidential))
	policy, err := access.New(config.PeerAuth{Owner: "owner", Locked: []string{rdProject}}, func(n string) (access.Account, error) {
		if n == "owner" {
			return access.Account{UID: rdOwner, Name: "owner"}, nil
		}
		return access.Account{}, fmt.Errorf("no account %s", n)
	})
	require.NoError(t, err)
	d := &restartDaemon{t: t, db: db, mgr: mgr, rec: events.NewRecorder(db), policy: policy, program: program}
	d.ret = retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	d.ret.SetBodyReader(mgr.Store())
	d.svc = approval.New(approval.Config{DB: db, Events: d.rec, Host: rdHost, LeaseCap: 8 * time.Hour,
		Programs: func(tok access.ProcessToken) string {
			if tok == rdAgent {
				return program
			}
			return ""
		}})
	return d
}

func (d *restartDaemon) newServer() *seamlessmcp.Server {
	return seamlessmcp.New(seamlessmcp.Config{
		DB: d.db, Files: d.mgr, Retrieve: d.ret, Events: d.rec, APIKey: "k", LocalHost: rdHost,
		Approvals: d.svc, OfferAccessRequest: true, DialogWait: time.Minute, DialogPoll: 5 * time.Millisecond,
	})
}

// restart replaces the running daemon's in-memory state with a fresh server.
func (d *restartDaemon) restart() {
	h := d.newServer().Handler()
	d.handler.Store(&h)
}

// listen serves the daemon, attaching the owner's socket view of the agent's
// process chain to every request as peerHandler does.
func (d *restartDaemon) listen() string {
	d.restart()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v, ok := d.view(); ok {
			r = r.WithContext(access.WithView(r.Context(), v))
		}
		(*d.handler.Load()).ServeHTTP(w, r)
	}))
	d.t.Cleanup(ts.Close)
	d.t.Cleanup(ts.CloseClientConnections)
	return ts.URL
}

func (d *restartDaemon) view() (access.View, bool) {
	v, ok := d.policy.PeerChainView(rdOwner, []access.ProcessToken{rdBridge, rdAgent})
	if !ok {
		return access.View{}, false
	}
	now := time.Now()
	rows, err := store.ActiveAccessLeasesFor(context.Background(), d.db, []string{rdBridge.Key(), rdAgent.Key()}, now)
	if err != nil {
		return access.View{}, false
	}
	return v.WithLeases(now, store.AccessLeases(rows)), true
}

// seed writes a memory in the locked project, so its briefing has content.
func (d *restartDaemon) seed() {
	d.t.Helper()
	ts := httptest.NewServer(d.newServer().Handler())
	d.t.Cleanup(ts.Close)
	ctx := context.Background()
	cli, err := mcpclient.NewStreamableHttpClient(ts.URL, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer k"}))
	require.NoError(d.t, err)
	d.t.Cleanup(func() { _ = cli.Close() })
	require.NoError(d.t, cli.Start(ctx))
	var ini mcp.InitializeRequest
	ini.Params.ProtocolVersion = mcp.LATEST_LEGACY_PROTOCOL_VERSION
	ini.Params.ClientInfo = mcp.Implementation{Name: "seed", Version: "0"}
	_, err = cli.Initialize(ctx, ini)
	require.NoError(d.t, err)
	res, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "memory_write", Arguments: map[string]any{
		"project": rdProject, "name": "vault-rule", "kind": "constraint",
		"description": "the vault rule", "body": "Keep the vault closed.\n",
	}}})
	require.NoError(d.t, err)
	require.False(d.t, res.IsError, "%v", res.Content)
}

// session records the agent's session the way the SessionStart hook does in a
// locked repository, stamped with the agent process.
func (d *restartDaemon) session() {
	d.t.Helper()
	ctx := context.Background()
	id, err := core.NewID()
	require.NoError(d.t, err)
	now := time.Now()
	require.NoError(d.t, store.CreateSession(ctx, d.db, core.Session{
		ID: id, Name: "cc/vault", ProjectSlug: rdProject, Status: core.SessionActive, Ambient: true, Host: rdHost,
		ExternalSessionID: "cc-vault-x", ExternalClient: "claude-code", CWD: "/work/" + rdProject, CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(d.t, store.SetSessionAgentProcess(ctx, d.db, id, rdAgent.AgentID()))
}

// enrollTouchID enrolls a software key as the helper's Secure Enclave key.
func (d *restartDaemon) enrollTouchID() (*ecdsa.PrivateKey, store.ApproverCredential) {
	d.t.Helper()
	ctx := context.Background()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(d.t, err)
	pub, err := key.PublicKey.Bytes()
	require.NoError(d.t, err)
	code, _, err := store.MintEnrollCode(ctx, d.db, store.ApproverTouchID, time.Now())
	require.NoError(d.t, err)
	msg := sha256.Sum256(approval.TouchIDEnrollMessage(code, pub))
	sig, err := ecdsa.SignASN1(rand.Reader, key, msg[:])
	require.NoError(d.t, err)
	cred, err := d.svc.EnrollTouchID(ctx, "this Mac", code, pub, sig)
	require.NoError(d.t, err)
	return key, cred
}

// prove signs request id over duration and hands it to the approve core: what
// the helper the bridge launched does.
func (d *restartDaemon) prove(key *ecdsa.PrivateKey, cred store.ApproverCredential, id, duration string) error {
	ctx := context.Background()
	a, err := store.AccessApprovalByID(ctx, d.db, id)
	if err != nil {
		return err
	}
	facts, err := d.svc.Facts(ctx, id)
	if err != nil {
		return err
	}
	dgst := sha256.Sum256(approval.TouchIDApproveMessage(a.ID, a.Challenge, facts.Digest, facts.Access, duration))
	sig, err := ecdsa.SignASN1(rand.Reader, key, dgst[:])
	if err != nil {
		return err
	}
	_, err = d.svc.Approve(ctx, approval.Decision{ID: id, Duration: duration, Digest: facts.Digest,
		Proof: approval.Proof{Kind: approval.ProofTouchID, CredentialID: cred.ID, Signature: sig}})
	return err
}

// toolResultText is every text item of a tools/call reply, and whether it is
// an error.
func toolResultText(t *testing.T, reply rpcReply) ([]string, bool) {
	t.Helper()
	require.Nil(t, reply.Error, "tools/call answered with a JSON-RPC error: %s", reply.Result)
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	require.NoError(t, json.Unmarshal(reply.Result, &res))
	items := make([]string, len(res.Content))
	for i, c := range res.Content {
		items[i] = c.Text
	}
	return items, res.IsError
}

// briefingCount counts the one-time briefing's notices in a result.
func briefingCount(items []string) int {
	n := 0
	for _, it := range items {
		if strings.Contains(it, "This is its briefing, sent once:") {
			n++
		}
	}
	return n
}

// A live bridge that declared form elicitation keeps the approval dialog across
// a daemon restart: the next refused call shows the form, the approver is
// launched through the bridge when the owner picks Touch ID, and the access is
// approved. The one-time briefing goes out once, restart or no restart. Claude
// Code and Codex use the same bridge; what differs is the declared capability
// and the agent process the facts name.
//
// Not covered: the kernel's socket facts (the chain is played; peerauth_tree_unix_test
// covers those) and the helper app itself (mcpproxy_approver_test covers the
// launch; here the test plays its proof through the approve core).
func TestBridge_RestartKeepsTheApprovalDialog(t *testing.T) {
	for _, c := range []struct {
		name, caps, program, asker string
	}{
		{"claude code", `{"elicitation":{"form":{},"url":{}}}`, "/home/owner/.local/share/claude/versions/2.1.319", "Claude Code 2.1.319"},
		{"codex", `{"elicitation":{}}`, "/opt/homebrew/Caskroom/codex/0.46.0/codex-aarch64-apple-darwin", "Codex 0.46.0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newRestartDaemon(t, c.program)
			d.seed()
			d.session()
			key, cred := d.enrollTouchID()

			open := &fakeOpen{ran: make(chan struct{}, 1)}
			var errb strings.Builder
			b := approverBridge(d.listen(), open, &errb)
			b.agentProcess = rdAgent.AgentID()
			in, out := newFeedStdin(), newFrameSink()
			done := runBridge(b, in, out)

			in.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + mcp.LATEST_LEGACY_PROTOCOL_VERSION +
				`","capabilities":` + c.caps + `,"clientInfo":{"name":"t","version":"0"}}}`)
			require.Nil(t, decodeReply(t, out.next(t)).Error)
			in.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

			d.restart() // with the connection idle: the bridge sees no failed dial

			// The owner accepts with Touch ID; the helper the bridge launches
			// proves it, as the real one does over the socket.
			proved := make(chan error, 1)
			go func() {
				select {
				case <-open.ran:
				case <-time.After(20 * time.Second):
					proved <- fmt.Errorf("the bridge did not launch the approver")
					return
				}
				rows, err := store.PendingAccessApprovals(context.Background(), d.db, time.Now())
				if err != nil {
					proved <- fmt.Errorf("pending requests: %w", err)
					return
				}
				if len(rows) != 1 {
					proved <- fmt.Errorf("pending requests: %d, want 1", len(rows))
					return
				}
				proved <- d.prove(key, cred, rows[0].ID, approval.ForSession)
			}()
			in.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tasks_list","arguments":{}}}`)
			var ask struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					Message string `json:"message"`
				} `json:"params"`
			}
			frame := out.next(t)
			require.NoError(t, json.Unmarshal([]byte(frame), &ask), frame)
			require.Equal(t, string(mcp.MethodElicitationCreate), ask.Method, "the refused call raised the form dialog: %s", frame)
			require.Contains(t, ask.Params.Message, c.asker+" asks for read-write access to "+rdProject)
			in.send(`{"jsonrpc":"2.0","id":` + string(ask.ID) + `,"result":{"action":"accept","content":{"duration":"session","method":"touchid"}}}`)

			reply := decodeReply(t, out.next(t))
			require.JSONEq(t, "2", string(reply.ID))
			items, isErr := toolResultText(t, reply)
			require.True(t, isErr, "%v", items)
			require.Contains(t, items[0], "the owner approved access request")
			require.Contains(t, items[0], "is open to this agent until")
			select {
			case err := <-proved:
				require.NoError(t, err)
			case <-time.After(20 * time.Second):
				t.Fatal("the proof did not finish")
			}
			rows, err := store.LiveAccessLeases(context.Background(), d.db, time.Now())
			require.NoError(t, err)
			require.Len(t, rows, 1, "the approved lease")

			// The briefing comes on the first result under the lease, and
			// never again, across another restart.
			in.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"project_list","arguments":{}}}`)
			items, isErr = toolResultText(t, decodeReply(t, out.next(t)))
			require.False(t, isErr, "%v", items)
			require.Equal(t, 1, briefingCount(items), "%v", items)

			d.restart()
			in.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"project_list","arguments":{}}}`)
			items, isErr = toolResultText(t, decodeReply(t, out.next(t)))
			require.False(t, isErr, "%v", items)
			require.Zero(t, briefingCount(items), "%v", items)

			in.close()
			require.NoError(t, waitRun(t, done))
			require.Empty(t, out.rest())
			require.Empty(t, errb.String())
			approved, err := store.AccessApprovalByID(context.Background(), d.db, rows[0].ApprovalID)
			require.NoError(t, err)
			require.Equal(t, [][]string{openArgv(approved.ID, "session")}, open.recorded(),
				"the approver was launched for that request, for the duration the form chose, and only once")
		})
	}
}

// The control: a client that declared no elicitation shows no dialog after a
// restart either, so a dialog on the bridge above came from what was kept
// rather than from the restart having no effect.
func TestBridge_RestartWithNoDeclaredDialogStaysOnTheTextPath(t *testing.T) {
	d := newRestartDaemon(t, "/home/owner/.local/share/claude/versions/2.1.319")
	d.seed()
	d.session()
	d.enrollTouchID()

	open := &fakeOpen{}
	var errb strings.Builder
	b := approverBridge(d.listen(), open, &errb)
	b.agentProcess = rdAgent.AgentID()
	in, out := newFeedStdin(), newFrameSink()
	done := runBridge(b, in, out)

	in.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + mcp.LATEST_LEGACY_PROTOCOL_VERSION +
		`","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	require.Nil(t, decodeReply(t, out.next(t)).Error)
	in.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	d.restart()

	in.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tasks_list","arguments":{}}}`)
	reply := decodeReply(t, out.next(t))
	require.JSONEq(t, "2", string(reply.ID))
	items, isErr := toolResultText(t, reply)
	require.True(t, isErr, "%v", items)
	require.Contains(t, items[0], "seam approve", "the terminal way, named as ever")
	require.NotContains(t, items[0], "Touch ID")

	in.close()
	require.NoError(t, waitRun(t, done))
	require.Empty(t, out.rest(), "no dialog")
	require.Empty(t, open.recorded(), "no approver")
	require.Empty(t, errb.String())
}
