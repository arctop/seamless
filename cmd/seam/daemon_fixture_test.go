package main

// A stand-in daemon for the read commands' output tests: mcp-go's real
// streamable transport serving canned tool results at /api/mcp, plus canned
// console JSON and /healthz on the same listener. Every reply is a fixed string,
// so a command's rendering can be compared byte for byte and its --json output
// against exactly what "the daemon" sent.
//
// It never touches a real daemon, a real config file or the data dir: the env's
// loadConfig and dial are pointed at this listener and nothing else.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// toolReply is one canned tool result: its text, and whether the daemon marks
// it as a tool error.
type toolReply struct {
	text    string
	isError bool
}

// toolFunc answers one tool call from its arguments, so a fixture can reply
// differently to, say, an empty plan than to the default queue.
type toolFunc func(args map[string]any) toolReply

// fixedReply answers every call with the same successful text.
func fixedReply(text string) toolFunc {
	return func(map[string]any) toolReply { return toolReply{text: text} }
}

// recordedCall is one tools/call the fixture served.
type recordedCall struct {
	name   string
	args   map[string]any
	header http.Header
	params json.RawMessage // the frame's params as sent: arguments verbatim
}

// daemonFixture is the stand-in daemon.
type daemonFixture struct {
	srv *httptest.Server
	mux *http.ServeMux // the whole surface, for serving it on a second listener

	mu    sync.Mutex
	calls []recordedCall
	// console maps "METHOD /path?query" to a canned JSON body; anything else is
	// a 404, which is how the console answers an unknown slug.
	console map[string]string
	healthz string
}

// newDaemonFixture serves tools at /api/mcp, console at /console/..., and
// healthz at /healthz.
func newDaemonFixture(t *testing.T, tools map[string]toolFunc, console map[string]string, healthz string) *daemonFixture {
	t.Helper()
	fx := &daemonFixture{console: console, healthz: healthz}

	srv := mcpserver.NewMCPServer("t", "0", mcpserver.WithToolCapabilities(false))
	for name, fn := range tools {
		srv.AddTool(mcp.NewTool(name), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			fx.mu.Lock()
			for i := len(fx.calls) - 1; i >= 0; i-- {
				if fx.calls[i].name == name && fx.calls[i].args == nil {
					fx.calls[i].args = req.GetArguments()
					break
				}
			}
			fx.mu.Unlock()
			r := fn(req.GetArguments())
			if r.isError {
				return mcp.NewToolResultError(r.text), nil
			}
			return mcp.NewToolResultText(r.text), nil
		})
	}
	transport := mcpserver.NewStreamableHTTPServer(srv,
		mcpserver.WithStreamableHTTPProtocolVersions(mcp.LegacyProtocolVersions()...))

	mux := http.NewServeMux()
	mux.HandleFunc("/api/mcp", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var frame struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		var params struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(body, &frame) == nil && frame.Method == "tools/call" && json.Unmarshal(frame.Params, &params) == nil {
			fx.mu.Lock()
			fx.calls = append(fx.calls, recordedCall{name: params.Name, header: r.Header.Clone(), params: frame.Params})
			fx.mu.Unlock()
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		transport.ServeHTTP(w, r)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, fx.healthz)
	})
	mux.HandleFunc("/console/", func(w http.ResponseWriter, r *http.Request) {
		body, ok := fx.console[r.Method+" "+r.URL.RequestURI()]
		if !ok {
			http.Error(w, `{"error":"no such route in the fixture"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	})
	fx.mux = mux
	fx.srv = httptest.NewServer(mux)
	t.Cleanup(fx.srv.Close)
	return fx
}

// config returns a client config aimed at the fixture.
func (fx *daemonFixture) config() config.Config {
	cfg := config.Defaults()
	cfg.Addr = strings.TrimPrefix(fx.srv.URL, "http://")
	cfg.MCP.APIKey = "fixture-key"
	cfg.DataDir = "/data/seamless"
	return cfg
}

// env returns an env whose config and dial both reach the fixture.
func (fx *daemonFixture) env() (*env, *bytes.Buffer, *bytes.Buffer) {
	e, out, errb := stubEnv()
	cfg := fx.config()
	e.loadConfig = func() (config.Config, error) { return cfg, nil }
	e.dial = func(ctx context.Context) (*mcpclient.Client, config.Config, error) {
		return dialWith(ctx, cfg)
	}
	return e, out, errb
}

// run dispatches argv against the fixture and returns stdout, stderr and the
// exit code.
func (fx *daemonFixture) run(t *testing.T, argv ...string) (string, string, int) {
	t.Helper()
	e, out, errb := fx.env()
	code := dispatch(context.Background(), e, argv)
	return out.String(), errb.String(), code
}

// lastCall returns the most recent tools/call the fixture served.
func (fx *daemonFixture) lastCall(t *testing.T) recordedCall {
	t.Helper()
	fx.mu.Lock()
	defer fx.mu.Unlock()
	require.NotEmpty(t, fx.calls, "no tool was called")
	return fx.calls[len(fx.calls)-1]
}

// callCount reports how many tools/call requests reached the fixture.
func (fx *daemonFixture) callCount() int {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return len(fx.calls)
}
