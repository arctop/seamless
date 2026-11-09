package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// testRequestID is a well-formed access request id (a ULID).
const testRequestID = "01M6SBMC73SF2XSND2VAZ33QZJ"

// testSocket and testHome stand in for the peer_auth socket the bridge dials
// and the owner's home.
const (
	testSocket = "/var/run/seamless/seamless.sock"
	testHome   = "/Users/owner"
)

// approverAsk is the daemon's ask as it rides an SSE reply, with params as
// given ("" for none).
func approverAsk(params string) string {
	if params == "" {
		return `{"jsonrpc":"2.0","method":"` + core.ApproverNotification + `"}`
	}
	return `{"jsonrpc":"2.0","method":"` + core.ApproverNotification + `","params":` + params + `}`
}

// fakeOpen stands in for the program runner the bridge launches the approver
// with: it records each command line (the --stderr path as stderrMark),
// signals ran (when set) once per launch, and, when release is set, holds
// every launch until release closes. helper, when set, plays the helper the
// launch opened, given the path open --stderr names.
type fakeOpen struct {
	code    int
	err     error
	ran     chan struct{}
	release chan struct{}
	helper  func(stderr string)
	// sig answers the codesign calls of the check before open, which are not
	// launches and are not recorded; nil is a good signature.
	sig *touchRunner

	mu    sync.Mutex
	calls [][]string
}

// stderrMark stands for the FIFO path a launch hands the helper as stderr.
const stderrMark = "<stderr>"

func (f *fakeOpen) run(ctx context.Context, argv []string, in io.Reader, out, errw io.Writer) (int, error) {
	if argv[0] == "codesign" {
		sig := f.sig
		if sig == nil {
			sig = newTouchRunner()
		}
		return sig.exec(ctx, argv, in, out, errw)
	}
	recorded := slices.Clone(argv)
	stderr := ""
	if i := slices.Index(recorded, "--stderr"); i >= 0 && i+1 < len(recorded) {
		stderr, recorded[i+1] = recorded[i+1], stderrMark
	}
	f.mu.Lock()
	f.calls = append(f.calls, recorded)
	f.mu.Unlock()
	if f.helper != nil && stderr != "" {
		f.helper(stderr)
	}
	if f.ran != nil {
		f.ran <- struct{}{}
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return -1, ctx.Err()
		}
	}
	return f.code, f.err
}

func (f *fakeOpen) recorded() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// openArgv is the command line a launch for id and duration runs: the
// helper's stderr goes to a FIFO where the bridge can make one.
func openArgv(id, duration string) []string {
	argv := []string{"open", "-n", "-a", approverAppPath(testHome)}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		argv = append(argv, "--stderr", stderrMark)
	}
	return append(argv, "--args", "approve", "--socket", testSocket, "--request", id, "--for", duration)
}

// sseCallDaemon answers every request with an SSE stream: the given events,
// then the request's reply.
func sseCallDaemon(t *testing.T, events ...string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, e := range events {
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", e)
		}
		_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{}}\n\n", msg.ID)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/api/mcp"
}

// approverBridge is a bridge to url whose approver launches go to open.
func approverBridge(url string, open *fakeOpen, errb io.Writer) *bridge {
	b := newBridge(url, "k", nil)
	b.stderr = errb
	b.approver = approverHost{
		run:    open.run,
		goos:   "darwin",
		home:   func() (string, error) { return testHome, nil },
		socket: testSocket,
		self:   func() (string, error) { return fakeSelf, nil },
	}
	return b
}

// The daemon's ask to launch the Touch ID approver is acted on and never
// reaches the client: the reply that follows it on the same stream is the only
// frame on stdout. A launch it cannot make, or an ask it cannot read, is one
// line on stderr and launches nothing; the call is served either way.
func TestBridge_ApproverAsk(t *testing.T) {
	for _, tc := range []struct {
		name    string
		params  string
		mutate  func(*bridge, *fakeOpen)
		want    [][]string // the launches
		wantErr string     // the one stderr line, "" for none
	}{
		{name: "launches the approver",
			params: `{"request_id":"` + testRequestID + `","duration":"1h"}`,
			want:   [][]string{openArgv(testRequestID, "1h")}},
		{name: "an unknown field is ignored",
			params: `{"request_id":"` + testRequestID + `","duration":"session","extra":"x"}`,
			want:   [][]string{openArgv(testRequestID, "session")}},
		{name: "params that are not an object", params: `"approve"`,
			wantErr: "seam mcp-proxy: " + core.ApproverNotification + ": malformed params"},
		{name: "a request id that is not a string", params: `{"request_id":7,"duration":"1h"}`,
			wantErr: "malformed params"},
		{name: "no params", params: "",
			wantErr: "malformed params"},
		{name: "null params", params: `null`,
			wantErr: "not a request id"},
		{name: "a request id that is a flag", params: `{"request_id":"-n","duration":"1h"}`,
			wantErr: "not a request id"},
		{name: "a bad duration", params: `{"request_id":"` + testRequestID + `","duration":"1h --for 99h"}`,
			wantErr: "refusing to launch the Touch ID approver for duration"},
		{name: "no socket configured",
			params:  `{"request_id":"` + testRequestID + `","duration":"1h"}`,
			mutate:  func(b *bridge, _ *fakeOpen) { b.approver.socket = "" },
			wantErr: errNoApproverSocket.Error()},
		{name: "not macOS",
			params:  `{"request_id":"` + testRequestID + `","duration":"1h"}`,
			mutate:  func(b *bridge, _ *fakeOpen) { b.approver.goos = "linux" },
			wantErr: "exists only on macOS"},
		{name: "no home",
			params: `{"request_id":"` + testRequestID + `","duration":"1h"}`,
			mutate: func(b *bridge, _ *fakeOpen) {
				b.approver.home = func() (string, error) { return "", io.ErrUnexpectedEOF }
			},
			wantErr: "find the home the Touch ID approver is installed under"},
		{name: "a substitute app is refused and never opened",
			params: `{"request_id":"` + testRequestID + `","duration":"1h"}`,
			mutate: func(_ *bridge, o *fakeOpen) {
				o.sig = newTouchRunner()
				o.sig.appTeam = otherTeam
			},
			wantErr: "run seam touchid enroll to install it again"},
		{name: "open fails",
			params:  `{"request_id":"` + testRequestID + `","duration":"1h"}`,
			mutate:  func(_ *bridge, o *fakeOpen) { o.code = 1 },
			want:    [][]string{openArgv(testRequestID, "1h")},
			wantErr: "exit status 1 (is it installed? seam touchid enroll)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			open := &fakeOpen{}
			var errb strings.Builder
			b := approverBridge(sseCallDaemon(t, approverAsk(tc.params)), open, &errb)
			if tc.mutate != nil {
				tc.mutate(b, open)
			}
			var out strings.Builder
			require.NoError(t, b.run(context.Background(),
				strings.NewReader(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"recall"}}`+"\n"), &out))

			frames := nonEmptyLines(out.String())
			require.Len(t, frames, 1, "only the reply reaches stdout: %s", out.String())
			require.JSONEq(t, `4`, string(decodeReply(t, frames[0]).ID))
			require.Equal(t, tc.want, open.recorded())
			if tc.wantErr == "" {
				require.Empty(t, errb.String())
				return
			}
			lines := nonEmptyLines(errb.String())
			require.Len(t, lines, 1, errb.String())
			require.Contains(t, lines[0], tc.wantErr)
		})
	}
}

// A daemon that asks again for a request whose launch is still running gets
// one launch, so the owner sees one prompt; another request launches on its
// own.
func TestBridge_ApproverAskLaunchesOncePerRequest(t *testing.T) {
	const other = "01M6SBKTYM04X9QS4P6YQPBRMG"
	ask := approverAsk(`{"request_id":"` + testRequestID + `","duration":"1h"}`)
	open := &fakeOpen{release: make(chan struct{})}
	var errb strings.Builder
	b := approverBridge(sseCallDaemon(t, ask, ask, approverAsk(`{"request_id":"`+other+`","duration":"4h"}`)), open, &errb)

	in, out := newFeedStdin(), newFrameSink()
	done := runBridge(b, in, out)
	in.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"recall"}}`)
	// The reply follows all three asks on the stream, and the first launch is
	// held until release closes: every ask was taken while it ran.
	require.JSONEq(t, `4`, string(decodeReply(t, out.next(t)).ID))
	close(open.release)
	in.close()
	require.NoError(t, waitRun(t, done), "run waits for the launches")
	require.Empty(t, out.rest())
	require.Empty(t, errb.String())

	got := open.recorded()
	slices.SortFunc(got, func(a, b []string) int { return strings.Compare(a[len(a)-3], b[len(b)-3]) })
	require.Equal(t, [][]string{openArgv(other, "4h"), openArgv(testRequestID, "1h")}, got)
}

// Against mcp-go's own streamable transport, configured as the daemon serves
// it: a tool that asks the owner through an elicitation, waits for the
// answer, then asks the bridge to launch the approver and waits for the
// approval, as the daemon does. The elicitation reaches the client, the
// answer gets back while the call waits on it, the ask is taken and never
// relayed, and the call's result ends the exchange.
//
// The tool waits after the ask on purpose. mcp-go v1.1.1 can drop a
// notification sent just before a handler returns: its forwarder takes it off
// the session's channel, loses the response path's lock, finds the request
// done, and discards it (server/streamable_http.go, the notificationChannel
// case). A daemon that asks and then returns at once would lose the ask.
func TestBridge_ApprovalDialogOverTheRealTransport(t *testing.T) {
	open := &fakeOpen{ran: make(chan struct{}, 1)}
	srv := mcpserver.NewMCPServer("t", "0", mcpserver.WithToolCapabilities(false), mcpserver.WithElicitation())
	srv.AddTool(mcp.NewTool("access_request"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := srv.RequestElicitation(ctx, mcp.ElicitationRequest{Params: mcp.ElicitationParams{
			Message:         "Seamless access request",
			RequestedSchema: map[string]any{"type": "object"},
		}})
		if err != nil {
			return nil, err
		}
		if res.Action != mcp.ElicitationResponseActionAccept {
			return mcp.NewToolResultText("declined"), nil
		}
		if err := srv.SendNotificationToClient(ctx, core.ApproverNotification,
			map[string]any{"request_id": testRequestID, "duration": "session"}); err != nil {
			return nil, err
		}
		select { // the approval: here, the helper's launch
		case <-open.ran:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return mcp.NewToolResultText("approved"), nil
	})
	ts := httptest.NewServer(mcpserver.NewStreamableHTTPServer(srv,
		mcpserver.WithStreamableHTTPProtocolVersions(mcp.LegacyProtocolVersions()...)))
	t.Cleanup(ts.Close)
	t.Cleanup(ts.CloseClientConnections) // runs first: a call stuck on a failed test ends with its connection

	var errb strings.Builder
	b := approverBridge(ts.URL, open, &errb)
	in, out := newFeedStdin(), newFrameSink()
	done := runBridge(b, in, out)

	in.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + mcp.LATEST_LEGACY_PROTOCOL_VERSION +
		`","capabilities":{"elicitation":{"form":{}}},"clientInfo":{"name":"t","version":"0"}}}`)
	init := decodeReply(t, out.next(t))
	require.Nil(t, init.Error)
	in.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	in.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"access_request","arguments":{}}}`)

	var ask struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	frame := out.next(t)
	require.NoError(t, json.Unmarshal([]byte(frame), &ask), frame)
	require.Equal(t, string(mcp.MethodElicitationCreate), ask.Method, frame)
	in.send(`{"jsonrpc":"2.0","id":` + string(ask.ID) + `,"result":{"action":"accept","content":{}}}`)

	frame = out.next(t)
	result := decodeReply(t, frame)
	require.JSONEq(t, `2`, string(result.ID), frame)
	require.Nil(t, result.Error, frame)
	require.Contains(t, string(result.Result), "approved")

	in.close()
	require.NoError(t, waitRun(t, done))
	require.Empty(t, out.rest(), "the ask never reaches the client")
	require.Empty(t, errb.String())
	require.Equal(t, [][]string{openArgv(testRequestID, "session")}, open.recorded())
}

// The bridge's word that the approver ended is pinned as the daemon pins it
// (internal/mcp/elicit_test.go).
func TestBridge_ApproverEndedMethodIsPinned(t *testing.T) {
	require.Equal(t, "notifications/seamless/approver_ended", core.ApproverEndedNotification)
}

// How a helper that ended ended, read from its stderr: its last "SeamApprove: "
// line, a cancel's beginning "Not approved:". One killed before it said
// anything is closed: the bridge asks this only of a helper it saw end.
func TestApproverOutcome(t *testing.T) {
	for _, c := range []struct {
		name, stderr, want string
	}{
		{"the Touch ID prompt cancelled", "SeamApprove: Not approved: the Touch ID prompt was cancelled.\n", core.ApproverEndedCancelled},
		{"framework noise first", "2026-11-08 SeamApprove[812:4410] warning\nSeamApprove: Not approved: the Touch ID prompt was cancelled.\n", core.ApproverEndedCancelled},
		{"a failure, over two lines", "SeamApprove: The daemon refused it: this request was already decided.\n  seam approve R-7F3K\n", core.ApproverEndedClosed},
		{"approved", "SeamApprove: Approved access to acme-finance until 15:04.\n", core.ApproverEndedClosed},
		{"no newline at the end", "SeamApprove: Not approved: the Touch ID prompt was cancelled.", core.ApproverEndedCancelled},
		{"nothing", "", core.ApproverEndedClosed},
		{"nothing of the helper's", "objc[812]: Class X is implemented in both\n", core.ApproverEndedClosed},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, approverOutcome([]byte(c.stderr)))
		})
	}
}

// endedDaemon answers a tools/call with an SSE stream carrying the approver
// ask, then the call's reply, and records every approver_ended notification
// the bridge posts (202, no body), with the session it named.
type endedDaemon struct {
	url   string
	ended chan map[string]any
}

func newEndedDaemon(t *testing.T, ask string) *endedDaemon {
	t.Helper()
	d := &endedDaemon{ended: make(chan map[string]any, 4)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if msg.Method == core.ApproverEndedNotification {
			msg.Params["session"] = r.Header.Get(headerSessionID)
			d.ended <- msg.Params
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set(headerSessionID, "sess-1")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", ask)
		_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{}}\n\n", msg.ID)
	}))
	t.Cleanup(srv.Close)
	d.url = srv.URL + "/api/mcp"
	return d
}

// When the helper the bridge launched exits, the bridge tells the daemon how,
// on the connection's own MCP session, and the client sees nothing of it.
func TestBridge_ApproverEndIsReported(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("the helper's stderr is a FIFO, made on darwin and linux only")
	}
	for _, c := range []struct {
		name, line, want string
	}{
		{"cancelled", "SeamApprove: Not approved: the Touch ID prompt was cancelled.\n", core.ApproverEndedCancelled},
		{"closed", "SeamApprove: Couldn't approve: the daemon refused it.\n", core.ApproverEndedClosed},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newEndedDaemon(t, approverAsk(`{"request_id":"`+testRequestID+`","duration":"1h"}`))
			exited := make(chan error, 1)
			open := &fakeOpen{helper: func(stderr string) {
				// The helper, opened by LaunchServices: it holds its stderr
				// past open's return, then writes its outcome and exits.
				go func() {
					f, err := os.OpenFile(stderr, os.O_WRONLY, 0)
					if err != nil {
						exited <- err
						return
					}
					_, err = f.WriteString(c.line)
					exited <- errors.Join(err, f.Close())
				}()
			}}
			var errb strings.Builder
			b := approverBridge(d.url, open, &errb)
			in, out := newFeedStdin(), newFrameSink()
			done := runBridge(b, in, out)
			in.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"access_request"}}`)
			require.JSONEq(t, `4`, string(decodeReply(t, out.next(t)).ID))
			require.NoError(t, <-exited)

			select {
			case got := <-d.ended:
				require.Equal(t, map[string]any{"request_id": testRequestID, "outcome": c.want, "session": "sess-1"}, got)
			case <-time.After(10 * time.Second):
				t.Fatal("the bridge did not report the helper's end")
			}
			in.close()
			require.NoError(t, waitRun(t, done))
			require.Empty(t, out.rest(), "the word never reaches the client")
			require.Empty(t, errb.String())
			require.Equal(t, [][]string{openArgv(testRequestID, "1h")}, open.recorded())
		})
	}
}
