package mcp

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// A tool call that waits past the server's ReadTimeout still returns its
// result: an approval dialog waits up to 110 seconds, and the daemon's
// ReadTimeout is 30s (cmd/seamlessd newHTTPServer). ReadTimeout is a deadline
// on the connection; it holds because net/http clears that deadline itself
// when the request body hits EOF and its background read (the disconnect
// watch) starts (connReader.startBackgroundRead, go1.25 through go1.27), and
// mcp-go reads a POST body in full before it dispatches. If either stopped
// being true, a waiting call would die at ReadTimeout with its context
// cancelled, and this test would fail.
func TestHandler_ToolCallOutlivesTheReadTimeout(t *testing.T) {
	const readTimeout = 150 * time.Millisecond
	srv, _ := newBindingsServer(t)
	// A tool that waits three read timeouts and says whether its context
	// survived them.
	srv.addTool(mcp.NewTool("test_wait", hintRead()), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		select {
		case <-time.After(3 * readTimeout):
			return mcp.NewToolResultText("waited"), nil
		case <-ctx.Done():
			return mcp.NewToolResultError("cancelled: " + ctx.Err().Error()), nil
		}
	})
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Config.ReadTimeout = readTimeout
	ts.Config.ReadHeaderTimeout = readTimeout
	ts.Start()
	t.Cleanup(ts.Close)

	cli, err := mcpclient.NewStreamableHttpClient(ts.URL, transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer test-key"}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	ctx := context.Background()
	require.NoError(t, cli.Start(ctx))
	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "deadline", Version: "0"}
	_, err = cli.Initialize(ctx, initReq)
	require.NoError(t, err)

	res, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "test_wait"}})
	require.NoError(t, err, "the call must not die with its connection's read deadline")
	require.False(t, res.IsError, "%v", res.Content)
	require.Equal(t, "waited", res.Content[0].(mcp.TextContent).Text)
}
