package main

// Helpers the peer_auth matrix files share: an MCP client for the owner's TCP
// key or a socket principal's kernel identity, the peer a socket request is
// injected with, console routes read from source, and the small assertions
// every file uses. The matrix itself lives in TestPeerAuthMatrix_Leases,
// _Console, _Proofs, _Anchors, _CLI, _Grants, _Advisory and _Restricted.

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/passkey"
)

// paHost is the host the matrix daemons map repositories under.
const paHost = "example-host"

// paLogSink collects log output for assertions.
type paLogSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *paLogSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *paLogSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// closeServer drops any connection still open (a live-feed stream a failed
// assertion left behind) before Close, which would otherwise wait on it.
func closeServer(srv *httptest.Server) {
	srv.CloseClientConnections()
	srv.Close()
}

// injectPeer stands in for peerConnContext: the kernel's answer for the
// connection, here a fixed uid. Real-socket tests run the kernel path as the
// test's own uid (peerauth_unix_test.go).
func injectPeer(uid uint32, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), peerCredKey{}, peerCred{uid: uid})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// paClient is an MCP client against base: the owner presents the key, a socket
// principal presents nothing (the kernel is its credential).
func paClient(t *testing.T, base, key string) *mcpclient.Client {
	t.Helper()
	return paClientHeaders(t, base, key, nil)
}

// paClientHeaders is paClient with extra request headers (a forged identity).
func paClientHeaders(t *testing.T, base, key string, extra map[string]string) *mcpclient.Client {
	t.Helper()
	ctx := context.Background()
	headers := map[string]string{}
	if key != "" {
		headers["Authorization"] = "Bearer " + key
	}
	for k, v := range extra {
		headers[k] = v
	}
	cli, err := mcpclient.NewStreamableHttpClient(base+"/api/mcp", transport.WithHTTPHeaders(headers))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	require.NoError(t, cli.Start(ctx))
	var init mcpgo.InitializeRequest
	init.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcpgo.Implementation{Name: "matrix", Version: "0"}
	_, err = cli.Initialize(ctx, init)
	require.NoError(t, err)
	return cli
}

// paCall calls a tool and returns its text and whether it was an error result.
func paCall(t *testing.T, cli *mcpclient.Client, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cli.CallTool(context.Background(), mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: name, Arguments: args}})
	require.NoError(t, err, "%s transport error", name)
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcpgo.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func paCallOK(t *testing.T, cli *mcpclient.Client, name string, args map[string]any) map[string]any {
	t.Helper()
	text, isErr := paCall(t, cli, name, args)
	require.False(t, isErr, "%s failed: %s", name, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out), "%s: %s", name, text)
	return out
}

// paApprovals is a matrix daemon's approval service: the Approvers page and
// its passkey ceremony routes, live.
func paApprovals(t *testing.T, db *sql.DB) *approval.Service {
	t.Helper()
	v, err := passkey.New(passkey.Config{RPID: "localhost", Origins: []string{"http://localhost:8081"}})
	require.NoError(t, err)
	return approval.New(approval.Config{DB: db, Passkeys: v, Origins: []string{"http://localhost:8081"}})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}

// consoleRoutes reads the console's route table from its source, so the crawl
// below cannot fall behind a route added later.
func consoleRoutes(t *testing.T) (gets, posts []string) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "internal", "console", "console.go"))
	require.NoError(t, err)
	re := regexp.MustCompile(`(?:handle|post)\("(GET|POST) (/console/[^"]*)"`)
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		if m[1] == "GET" {
			gets = append(gets, m[2])
		} else {
			posts = append(posts, m[2])
		}
	}
	require.NotEmpty(t, gets)
	require.NotEmpty(t, posts)
	return gets, posts
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// paHook posts one hook event to base as the given key (none on the socket) and
// returns its body. An empty event posts to base itself: a full URL with its own
// query.
func paHook(t *testing.T, base, key, event string, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	target := base + "/api/hooks/" + event
	if event == "" {
		target = base // a full URL with its own query
	}
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(string(raw)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "hook %s: %s", event, out)
	return string(out)
}
