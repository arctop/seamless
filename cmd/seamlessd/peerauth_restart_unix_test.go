//go:build darwin || linux

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/store/storetest"
)

// raElicitor is a client's elicitation handler: its presence is what declares
// the capability.
type raElicitor struct{}

func (raElicitor) Elicit(context.Context, mcpgo.ElicitationRequest) (*mcpgo.ElicitationResult, error) {
	return &mcpgo.ElicitationResult{ElicitationResponse: mcpgo.ElicitationResponse{Action: mcpgo.ElicitationResponseActionDecline}}, nil
}

// raInitialize connects to url through hc and declares elicitation.
func raInitialize(t *testing.T, url string, hc *http.Client, headers map[string]string) {
	t.Helper()
	opts := []transport.StreamableHTTPCOption{transport.WithHTTPHeaders(headers)}
	if hc != nil {
		opts = append(opts, transport.WithHTTPBasicClient(hc))
	}
	trans, err := transport.NewStreamableHTTP(url, opts...)
	require.NoError(t, err)
	cli := mcpclient.NewClient(trans, mcpclient.WithElicitationHandler(raElicitor{}))
	t.Cleanup(func() { _ = cli.Close() })
	require.NoError(t, cli.Start(context.Background()))
	var req mcpgo.InitializeRequest
	req.Params.ProtocolVersion = mcpgo.LATEST_LEGACY_PROTOCOL_VERSION
	req.Params.ClientInfo = mcpgo.Implementation{Name: "restart", Version: "0"}
	_, err = cli.Initialize(context.Background(), req)
	require.NoError(t, err)
}

// What a client declared at initialize is kept only for a connection the
// kernel authenticated: over the real peer_auth socket (peerHandler's View
// must already be on initialize's context) a row is written, and over the
// bearer-key TCP listener of the same daemon none is, though the client there
// declared the same capability.
func TestPeerAuthSocket_InitializeKeepsWhatTheClientDeclared(t *testing.T) {
	l, cfg := newPALayout(t, "  locked: [example-finance]\n")
	env := paTestEnv(l, uint32(os.Geteuid()))
	policy, err := access.New(cfg.PeerAuth, env.resolve)
	require.NoError(t, err)
	rt := &peerAuthRuntime{policy: policy, cfg: cfg.PeerAuth, env: env}
	ln, err := rt.listenSocket()
	require.NoError(t, err)

	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc := approval.New(approval.Config{DB: db, Host: "example-host", LeaseCap: time.Hour})
	srv := mcp.New(mcp.Config{DB: db, APIKey: "k", Approvals: svc, OfferAccessRequest: true})
	handler := srv.Handler()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	socket := &http.Server{
		Handler: peerHandler(policy, storeLeases(db), logger, handler), ConnContext: peerConnContext,
	}
	go func() { _ = socket.Serve(ln) }()
	t.Cleanup(func() { _ = socket.Close() })

	kept := func() int {
		t.Helper()
		var n int
		require.NoError(t, db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM mcp_sessions`).Scan(&n))
		return n
	}

	// TCP first: nothing is kept for a connection only the key let in.
	tcp := httptest.NewServer(ownerHandler(policy, handler))
	t.Cleanup(tcp.Close)
	raInitialize(t, tcp.URL, nil, map[string]string{"Authorization": "Bearer k"})
	require.Zero(t, kept(), "the bearer-key listener keeps nothing")

	// The socket: the kernel named the caller before initialize ran.
	client := config.Config{Role: config.RoleClient, AdvertisedURL: "unix://" + l.sock}
	hc, err := client.HTTPClient(0)
	require.NoError(t, err)
	raInitialize(t, client.ServerURL()+"/api/mcp", hc, nil)
	require.Equal(t, 1, kept(), "the socket's initialize kept the declared capability")
}
