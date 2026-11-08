package main

// seam mcp-proxy -- a transport-thin stdio<->streamable-HTTP bridge, so an MCP
// client that can only speak stdio reaches the same daemon /api/mcp endpoint the
// HTTP-native clients (Claude Code, the seam CLI) already use. The local Codex
// host is the first such client: `codex mcp add seamless -- <abs seam>
// mcp-proxy --config ...` writes the config shared by its app, CLI, and IDE.
//
// Why a bridge rather than pointing Codex straight at the HTTP endpoint (design
// decision D6):
//   - no secret duplicated into ~/.codex/config.toml;
//   - we never hand-edit or serialize another tool's live TOML -- registration
//     goes through `codex mcp add`, symmetric with the CC installer shelling to
//     `claude mcp add`;
//   - `bearer_token_env_var` needs the key exported in whatever shell launches
//     codex, which is fragile (upstream codex issue #30125);
//   - the bridge makes ANY stdio-only MCP client a supported Seamless client.
//
// It is deliberately dumb: it knows the MCP wire framing (newline-delimited
// JSON-RPC on stdio, streamable HTTP on the wire) and nothing about tools,
// arguments, or results. The one stateful thing it carries is the Mcp-Session-Id
// the daemon mints on initialize -- resending it on every later POST is what keeps
// the connection binding alive, so session_start inheritance works across calls.
// The one Seamless message it knows is the daemon's ask to launch the Touch ID
// approver (core.ApproverNotification), which it swallows and acts on, and
// the one it sends of its own is its word that the approver it launched has
// exited (approverEndedMethod; mcpproxy_approver.go).
//
// Requests and notifications are served in turn, in the order the client wrote
// them, but stdin is read all the while: a tool call can wait on the client's
// answer to a server request it sent mid-call (the in-session approval dialog's
// elicitation), and that answer is behind the call on stdin. Answers and
// notifications/cancelled therefore go to the daemon at once (see sentAtOnce).
//
// It outlives a daemon restart (an automatic update performs one): a failed
// dial is retried (dialretry.go), and the old Mcp-Session-Id is still served
// afterwards, because the session-id manager the daemon's transport keeps
// (mcp-go's default; internal/mcp Handler sets no other) checks the id's shape,
// not whether this process minted it. What the restart does lose is the
// daemon's in-memory connection state: a session_start binding (calls fall back
// to the agent-process binding, which lives in the session row) and a lab_open
// lab.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/agentproc"
	"github.com/arctop/seamless/internal/config"
)

// headerSessionID is the streamable-HTTP session header (mcp-go's
// server.HeaderKeySessionID). The daemon mints it on initialize and requires it
// on every subsequent request; the bridge captures and replays it.
const headerSessionID = "Mcp-Session-Id"

// proxyDialBudget is how long the bridge re-dials a daemon it cannot dial
// before it answers the request with an error. It spans an update's restart
// (about 2s on macOS) with room to spare; past it the daemon is down rather
// than restarting, and the client is better served by an error it can act on.
const proxyDialBudget = 30 * time.Second

// mcpProxyOpts carries the flags for `seam mcp-proxy`.
type mcpProxyOpts struct {
	config string // --config: abs seamless.yaml the installer bakes into the codex registration
}

// bindMCPProxy registers --config, the same escape from cwd-relative config
// search that `seam hook` uses: `codex mcp add` records an argv with no
// environment, so the path is a flag rather than the SEAMLESS_CONFIG env prefix.
func bindMCPProxy(fs *flag.FlagSet) *mcpProxyOpts {
	o := &mcpProxyOpts{}
	fs.StringVar(&o.config, "config", "", "path to seamless.yaml, so the proxy resolves config from any cwd")
	return o
}

var mcpProxyCmd = spec("mcp-proxy", groupBridge, "bridge a stdio MCP client to seamlessd over HTTP",
	noArgs(), bindMCPProxy, runMCPProxy).
	withLong(`An MCP client spawns this; it is not run by hand. It reads newline-delimited
JSON-RPC from stdin, forwards each message to the daemon's /api/mcp streamable
HTTP endpoint with the bearer key from config, and relays the reply back to
stdout, preserving Mcp-Session-Id so a session started over the bridge stays
bound across calls.

Register it with a stdio client, e.g. the local Codex host:

  codex mcp add seamless -- <abs seam> mcp-proxy --config <abs seamless.yaml>

It rides out a daemon restart: while seamlessd cannot be dialed, a request is
retried for up to 30s and then answered with a JSON-RPC error. A request that
may have reached the daemon is never sent twice, since a tool call may already
have run; it is answered with an error instead. Either way the bridge keeps
serving, because a stdio client does not restart a server that exits. Once
serving, it exits only when stdin closes or stdout breaks.`)

func runMCPProxy(ctx context.Context, e *env, o *mcpProxyOpts, _ []string) error {
	// --config is config.Load's documented $SEAMLESS_CONFIG override, moved out of
	// the shell because `codex mcp add` records an argv with no environment. Same
	// path as runHook; setting it in this process is safe and keeps loadConfig's
	// search order the single code path.
	if o.config != "" {
		if err := os.Setenv("SEAMLESS_CONFIG", o.config); err != nil {
			return fmt.Errorf("set config path: %w", err)
		}
	}
	cfg, err := e.loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// No whole-request deadline (see config.Config.HTTPClient): a tool call may
	// be LLM-backed and legitimately slow, and codex has its own
	// tool_timeout_sec for that.
	client, err := cfg.HTTPClient(0)
	if err != nil {
		return err
	}
	b := newBridge(cfg.ServerURL()+"/api/mcp", cfg.MCP.APIKey, client)
	b.stderr = e.stderr
	// The daemon may ask this bridge to launch the Touch ID approver for an
	// access request (mcpproxy_approver.go). The helper talks to the daemon only
	// through the peer_auth socket, so it is told the one this bridge dials.
	b.approver = approverHost{run: e.run, goos: e.platform(), home: os.UserHomeDir, socket: cfg.SocketPath()}
	// The client that spawned this bridge is the agent whose SessionStart hook
	// created its ambient session; naming it on every request is what binds the
	// connection to that session. Resolved once: the parent does not change for
	// the life of a stdio server.
	if proc, ok := agentproc.Anchor(); ok {
		b.agentProcess = proc
	}
	return b.run(ctx, e.stdin, e.stdout)
}

// bridge forwards stdio MCP frames to a streamable-HTTP endpoint and back.
// Requests and ordinary notifications are served by one worker, in turn: each
// is forwarded and its reply relayed before the next is sent, which preserves
// request/response ordering without any per-request bookkeeping. The frames
// sent at once (sentAtOnce) and the approver launches run on side goroutines,
// which write no reply of their own in practice; stdout and stderr are
// serialized so no two frames or lines interleave.
type bridge struct {
	endpoint     string
	apiKey       string
	client       *http.Client
	retry        *dialRetry   // dial-phase retry policy (dialretry.go); tests swap its clock
	stderr       io.Writer    // where a failure nothing waits on is reported; MCP clients log it
	agentProcess string       // agentproc identity of the client that spawned the bridge; "" = unknown
	approver     approverHost // how to launch the Touch ID approver when the daemon asks

	mu        sync.Mutex
	sessionID string              // Mcp-Session-Id from initialize, replayed on later POSTs; guarded by mu
	launching map[string]struct{} // request ids whose approver launch is running; guarded by mu
	stopped   bool                // run is returning: no new side work starts; guarded by mu
	side      sync.WaitGroup      // the side goroutines: frames sent at once, approver launches and their watches
	quit      chan struct{}       // closed when run returns: an approver's watch ends unreported
	quitOnce  sync.Once

	logMu sync.Mutex // serializes writes to stderr
}

// newBridge takes its HTTP client rather than building one: the TLS trust the
// CLI applies (tls.ca_file) has to be identical here and on every other surface,
// and a bridge with its own client was how it would drift. A nil client falls
// back to the shared shape with no extra roots, for the tests that only exercise
// framing.
func newBridge(endpoint, apiKey string, client *http.Client) *bridge {
	if client == nil {
		// A dial timeout so a down daemon fails fast, but no response timeout:
		// a tool call may be LLM-backed (recall embeddings, gardener_request)
		// and legitimately slow.
		client = &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{Timeout: config.DialTimeout}).DialContext,
			},
		}
	}
	// A copy, so the caller's client keeps its own policy.
	own := *client
	own.CheckRedirect = config.RefuseRedirect
	client = &own
	return &bridge{
		endpoint: endpoint, apiKey: apiKey, client: client,
		retry: newDialRetry(proxyDialBudget), stderr: io.Discard, quit: make(chan struct{}),
	}
}

// run reads newline-delimited JSON-RPC from r and relays each frame's reply to w
// until stdin closes (EOF -> nil), and then waits for the side goroutines still
// running. A failure reaching the daemon is answered in-band and the bridge
// goes on (see forward); only a broken stdin or stdout, or a cancelled ctx, ends
// it with an error.
func (b *bridge) run(ctx context.Context, r io.Reader, w io.Writer) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	out := &syncWriter{w: w}
	q := newFrameQueue()
	go b.read(ctx, r, q, out, cancel)

	err := b.serve(ctx, q, out)
	if errors.Is(err, errWriteStdout) {
		// The client is gone: nothing a side goroutine relays can reach it.
		cancel(err)
	}
	b.stop()
	if cause := context.Cause(ctx); errors.Is(cause, errWriteStdout) {
		// Stdout broke, perhaps under a side goroutine, whose cancel then ended
		// the worker's forward with a bare "context canceled".
		err = cause
	}
	return err
}

// serve is the worker: it forwards the queued frames in turn until stdin has
// ended and the queue is empty, or a forward returns an error.
func (b *bridge) serve(ctx context.Context, q *frameQueue, w io.Writer) error {
	for {
		frame, ok, err := q.next(ctx)
		if !ok {
			return err
		}
		if err := b.forward(ctx, frame, w); err != nil {
			return err
		}
	}
}

// read is stdin's one reader. It queues the frames served in turn and starts
// the ones sent at once, and closes the queue when stdin ends. It never waits
// on the worker: an answer behind a queued request would then sit unread while
// the call in flight waits on it.
func (b *bridge) read(ctx context.Context, r io.Reader, q *frameQueue, w io.Writer, fail context.CancelCauseFunc) {
	// ReadBytes rather than a Scanner: a tool-call frame (e.g. memory_write with a
	// long body) can exceed a Scanner's default token cap, and ReadBytes has none.
	// It also returns a fresh slice each time, which a side goroutine may keep.
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if frame := bytes.TrimSpace(line); len(frame) > 0 {
			if sentAtOnce(frame) {
				b.goSide(func() {
					if ferr := b.forward(ctx, frame, w); ferr != nil {
						fail(ferr) // stdout is broken or ctx is done: end the bridge
					}
				})
			} else {
				q.push(frame)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			} else {
				err = fmt.Errorf("read stdin: %w", err)
			}
			q.close(err)
			return
		}
	}
}

// sentAtOnce reports whether frame goes to the daemon as soon as it is read,
// rather than in turn behind a call in flight: a response (an id and no
// method), which answers a server request that call may be waiting on, and
// notifications/cancelled, which is about that call. The daemon answers both
// with a 202, so neither has a reply to order against the others.
func sentAtOnce(frame []byte) bool {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(frame, &m) != nil {
		return false
	}
	return (m.Method == "" && len(m.ID) > 0) || m.Method == string(mcp.MethodNotificationCancelled)
}

// goSide runs f on a side goroutine that stop waits for, unless run is
// already returning.
func (b *bridge) goSide(f func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	b.side.Add(1)
	go func() {
		defer b.side.Done()
		f()
	}()
}

// stop lets no new side goroutine start, ends the approvers' watches (the
// client they would report to is gone) and waits for the running ones.
func (b *bridge) stop() {
	b.mu.Lock()
	b.stopped = true
	b.mu.Unlock()
	b.quitOnce.Do(func() { close(b.quit) })
	b.side.Wait()
}

// session is the Mcp-Session-Id to send, "" before initialize.
func (b *bridge) session() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

// logf writes one line to stderr.
func (b *bridge) logf(format string, args ...any) {
	b.logMu.Lock()
	defer b.logMu.Unlock()
	fmt.Fprintf(b.stderr, format+"\n", args...)
}

// syncWriter serializes writes to the client's stdout: the worker and a side
// goroutine may both relay a frame, and writeFrame writes each one whole in a
// single Write.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// frameQueue holds the frames the worker serves in turn, in the order the
// client wrote them. It is unbounded on purpose: the reader must never wait on
// the worker (see read).
type frameQueue struct {
	mu     sync.Mutex
	frames [][]byte
	ended  bool          // stdin ended
	err    error         // why: nil at EOF
	ready  chan struct{} // signalled on every push and on close
}

func newFrameQueue() *frameQueue { return &frameQueue{ready: make(chan struct{}, 1)} }

func (q *frameQueue) push(frame []byte) {
	q.mu.Lock()
	q.frames = append(q.frames, frame)
	q.mu.Unlock()
	q.signal()
}

// close marks the end of stdin, err saying why (nil at EOF).
func (q *frameQueue) close(err error) {
	q.mu.Lock()
	q.ended, q.err = true, err
	q.mu.Unlock()
	q.signal()
}

func (q *frameQueue) signal() {
	select {
	case q.ready <- struct{}{}:
	default: // a wake-up is already pending
	}
}

// next returns the next frame. Once stdin has ended and the queue is empty, or
// ctx is done, it returns ok=false and why: stdin's error (nil at EOF), or
// ctx's cause.
func (q *frameQueue) next(ctx context.Context) (frame []byte, ok bool, err error) {
	for {
		q.mu.Lock()
		if len(q.frames) > 0 {
			frame = q.frames[0]
			q.frames[0] = nil
			q.frames = q.frames[1:]
			q.mu.Unlock()
			return frame, true, nil
		}
		ended, err := q.ended, q.err
		q.mu.Unlock()
		if ended {
			return nil, false, err
		}
		select {
		case <-q.ready:
		case <-ctx.Done():
			return nil, false, context.Cause(ctx)
		}
	}
}

// setStatelessHeaders adds the headers the stateless MCP revision (2026-07-28,
// SEP-2243) requires on every POST, read from the frame itself: a stdio client
// writes a bare JSON-RPC message and has no headers of its own to set. Such a
// request declares its version in params._meta, and the server refuses one whose
// Mcp-Protocol-Version header is missing or disagrees -- before it even checks
// whether it serves that version, so without these a client could not be told
// to negotiate down. A frame on an earlier revision (no _meta version) is left
// alone, as is anything that does not parse: the daemon reports that better.
func setStatelessHeaders(h http.Header, frame []byte) {
	var msg struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(frame, &msg) != nil || msg.Method == "" {
		return
	}
	var params struct {
		Meta *mcp.Meta `json:"_meta"`
	}
	if len(msg.Params) > 0 && json.Unmarshal(msg.Params, &params) != nil {
		return
	}
	version := params.Meta.ProtocolVersion()
	if !mcp.RequiresStandardHeaders(version) {
		return
	}
	h.Set(mcp.HeaderProtocolVersion, version)
	h.Set(mcp.HeaderMethod, msg.Method)
	method := mcp.MCPMethod(msg.Method)
	if !mcp.MethodRequiresNameHeader(method) {
		return
	}
	if name, ok := mcp.ExtractHeaderName(method, msg.Params); ok {
		if v, ok := mcp.EncodeHeaderValue(name); ok {
			h.Set(mcp.HeaderName, v)
		}
	}
}

// forward relays one JSON-RPC frame to the daemon and the daemon's reply to w.
//
// A failure between the bridge and the daemon does not end the bridge: an MCP
// client does not restart a stdio server that exits, so exiting would take the
// tools away for the rest of the client's session. A request is answered with a
// JSON-RPC error naming the cause instead; a notification or a response, which
// nothing waits on, is reported on stderr. Only a failed write to w (the client
// is gone) or a cancelled ctx is returned.
func (b *bridge) forward(ctx context.Context, frame []byte, w io.Writer) error {
	err := b.exchange(ctx, frame, w)
	if err == nil || errors.Is(err, errWriteStdout) || ctx.Err() != nil {
		return err
	}
	return b.answerFailure(w, frame, err)
}

// exchange POSTs one frame and relays the reply to w. An error means no reply
// was relayed (or, wrapping errWriteStdout, that relaying it failed); forward
// decides who hears about it.
func (b *bridge) exchange(ctx context.Context, frame []byte, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(frame))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+b.apiKey)
	// Which machine this bridge runs on. The daemon may be another one, and
	// without this every remote agent would be attributed to the daemon's host.
	if host := config.Hostname(); host != "" {
		req.Header.Set(hostHeader, host)
	}
	if b.agentProcess != "" {
		req.Header.Set(agentProcessHeader, b.agentProcess)
	}
	if id := b.session(); id != "" {
		req.Header.Set(headerSessionID, id)
	}
	setStatelessHeaders(req.Header, frame)

	resp, err := b.retry.do(b.client, req)
	if err != nil {
		if errors.Is(err, errNotSent) {
			return fmt.Errorf("cannot reach seamlessd at %s (is the daemon running?): %w", b.endpoint, err)
		}
		return fmt.Errorf("lost the connection to seamlessd at %s after sending the request; "+
			"it was not re-sent, because it may already have run: %w", b.endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Capture the session id the daemon mints on initialize; replaying it on later
	// POSTs is what keeps the connection binding (session_start inheritance) alive.
	if id := resp.Header.Get(headerSessionID); id != "" {
		b.mu.Lock()
		b.sessionID = id
		b.mu.Unlock()
	}

	switch {
	case resp.StatusCode == http.StatusAccepted:
		// A notification (or a response with no reply): 202 with no body. Nothing
		// to relay -- writing an empty line would corrupt the stdio framing.
		return nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		// A JSON-RPC error answering the request is the reply: the stateless
		// revision (2026-07-28) sends its refusals that way -- an unsupported
		// protocol version is a 400 naming the versions to fall back to -- and
		// the client must read it to negotiate down. Any other non-2xx (bad
		// content type, unauthorized, oversized body, server error) is a failed
		// exchange, which forward answers in-band.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody)) //nolint:errcheck // best-effort read of an error body for the message; a partial read still helps
		if isJSONRPCError(body) {
			return writeFrame(w, body)
		}
		return fmt.Errorf("seamlessd returned %s: %s", resp.Status, bytes.TrimSpace(body))
	}

	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")) //nolint:errcheck // an absent or unparseable content type just falls through to the JSON path
	if mediaType == "text/event-stream" {
		replied, err := b.relaySSE(ctx, resp.Body, w)
		switch {
		case errors.Is(err, errWriteStdout):
			return err
		case replied:
			// The reply reached the client, so the exchange is complete even if
			// the stream broke after it; answering again would send its id twice.
			return nil
		case err == nil:
			// A clean end with no reply still leaves the client waiting on it.
			err = io.ErrUnexpectedEOF
		}
		return b.lostReply(err)
	}
	// application/json: a single JSON-RPC reply object.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return b.lostReply(err)
	}
	return writeFrame(w, body)
}

// lostReply words a reply that broke off after the daemon had the request.
func (b *bridge) lostReply(err error) error {
	return fmt.Errorf("lost the reply from seamlessd at %s; the request may already have run: %w", b.endpoint, err)
}

// answerFailure reports a failed exchange to the client that sent frame. A
// request gets a JSON-RPC error carrying its id, since the client is waiting on
// that id. A notification or a response (to a server-initiated request) has no
// reply to give, so its failure goes to stderr.
func (b *bridge) answerFailure(w io.Writer, frame []byte, cause error) error {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	isRequest := json.Unmarshal(frame, &msg) == nil && msg.Method != "" &&
		len(msg.ID) > 0 && string(msg.ID) != "null"
	if !isRequest {
		prefix := "seam mcp-proxy"
		if msg.Method != "" {
			prefix += ": " + msg.Method
		}
		b.logf("%s: %v", prefix, cause)
		return nil
	}
	reply, err := json.Marshal(jsonRPCFailure{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      msg.ID,
		Error:   mcp.NewJSONRPCErrorDetails(mcp.INTERNAL_ERROR, cause.Error(), nil),
	})
	if err != nil {
		return fmt.Errorf("encode error reply: %w", err)
	}
	return writeFrame(w, reply)
}

// jsonRPCFailure is the error reply the bridge writes for a request the daemon
// did not answer. The id is the request's own bytes, so it matches whatever type
// the client chose.
type jsonRPCFailure struct {
	JSONRPC string                  `json:"jsonrpc"`
	ID      json.RawMessage         `json:"id"`
	Error   mcp.JSONRPCErrorDetails `json:"error"`
}

// maxErrorBody caps how much of a non-2xx reply the bridge reads: enough for
// any JSON-RPC error the daemon sends, and for the message of one it does not.
const maxErrorBody = 64 << 10

// isJSONRPCError reports whether body is a JSON-RPC error response.
func isJSONRPCError(body []byte) bool {
	var msg struct {
		JSONRPC string          `json:"jsonrpc"`
		Error   json.RawMessage `json:"error"`
	}
	return json.Unmarshal(body, &msg) == nil && msg.JSONRPC == "2.0" && len(msg.Error) > 0
}

// errWriteStdout marks a failed write to the client: the one failure the bridge
// cannot answer in-band, because the client is gone.
var errWriteStdout = errors.New("write stdout")

// writeFrame relays one JSON-RPC message as a single stdio frame: trimmed to one
// line, newline-terminated, per the stdio transport contract (no embedded
// newlines). An empty message writes nothing.
func writeFrame(w io.Writer, msg []byte) error {
	msg = bytes.TrimSpace(msg)
	if len(msg) == 0 {
		return nil
	}
	if _, err := w.Write(append(msg, '\n')); err != nil {
		return fmt.Errorf("%w: %w", errWriteStdout, err)
	}
	return nil
}

// relaySSE relays the JSON-RPC messages carried in a text/event-stream reply,
// and reports whether one of them was the reply itself -- the message with no
// method; the others are notifications or server requests riding the same
// stream. A tool call that waits on the owner (the in-session approval dialog)
// upgrades to SSE to carry its elicitation and the daemon's ask to launch the
// Touch ID approver, which is swallowed here (takeApproverAsk); the
// synchronous tools do not, but the streamable-HTTP server can still choose
// it. Each event's data payload is one JSON-RPC message.
func (b *bridge) relaySSE(ctx context.Context, body io.Reader, w io.Writer) (replied bool, err error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var data []string
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		msg := []byte(strings.Join(data, "\n"))
		data = data[:0]
		if b.takeApproverAsk(ctx, msg) {
			return nil
		}
		if err := writeFrame(w, msg); err != nil {
			return err
		}
		replied = replied || isReply(msg)
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "": // event boundary
			if err := flush(); err != nil {
				return replied, err
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[len("data:"):], " "))
		default: // event:, id:, retry:, comments -- not part of the payload
		}
	}
	if err := sc.Err(); err != nil {
		return replied, fmt.Errorf("read sse: %w", err)
	}
	// Not `return replied, flush()`: Go leaves unspecified whether replied is
	// read before or after the call that may set it, and a reply in the final,
	// unterminated event would then be answered a second time.
	err = flush()
	return replied, err
}

// isReply reports whether msg is a JSON-RPC response: an id and no method.
func isReply(msg []byte) bool {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	return json.Unmarshal(msg, &m) == nil && m.Method == "" && len(m.ID) > 0
}
