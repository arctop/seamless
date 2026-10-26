package mcp_test

// Host-scoped MCP identity: which machine a connection speaks for, where
// session_start takes that answer from, and how the ambient fallback narrows to
// it before widening.

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const (
	mcpLocalHost  = "alpha"
	mcpRemoteHost = "beta"
)

// newHostServer is a server that has named its machine, so a caller from any
// other machine is remote.
func newHostServer(t *testing.T) (string, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap, `{"/work/demo":"demo"}`))
	require.NoError(t, store.AdoptLocalHost(ctx, db, mcpLocalHost))

	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })

	ret := retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	srv := mcpserver.New(mcpserver.Config{
		DB: db, Files: mgr, Retrieve: ret, Events: events.NewRecorder(db),
		APIKey: testKey, LocalHost: mcpLocalHost,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, db
}

// dialFrom is dialClient with the machine header a remote `seam` sends.
func dialFrom(t *testing.T, ctx context.Context, url, host string) *mcpclient.Client {
	t.Helper()
	headers := map[string]string{"Authorization": "Bearer " + testKey}
	if host != "" {
		headers[mcpserver.HostHeader] = host
	}
	cli, err := mcpclient.NewStreamableHttpClient(url, transport.WithHTTPHeaders(headers))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	require.NoError(t, cli.Start(ctx))
	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "host-test", Version: "0"}
	_, err = cli.Initialize(ctx, initReq)
	require.NoError(t, err)
	return cli
}

func sessionHost(t *testing.T, db *sql.DB, id string) core.Session {
	t.Helper()
	sess, ok, err := store.SessionByID(context.Background(), db, id)
	require.NoError(t, err)
	require.True(t, ok)
	return sess
}

// Explicit arg beats the connection header beats the daemon's own host. The arg
// is the only one an agent can correct when its transport strips headers, and
// the daemon's host is what keeps a loopback install unchanged.
func TestSessionStart_HostPrecedence(t *testing.T) {
	ctx := context.Background()
	url, db := newHostServer(t)

	// No header, no arg: the daemon's own machine.
	cli := dialFrom(t, ctx, url, "")
	out := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo", "name": "s-default"})
	require.Equal(t, mcpLocalHost, sessionHost(t, db, out["session_id"].(string)).Host)
	require.Equal(t, "demo", out["project"], "the local map still resolves")

	// Header only: the connection's machine.
	remote := dialFrom(t, ctx, url, mcpRemoteHost)
	out = callJSON(t, ctx, remote, "session_start", map[string]any{
		"cwd": "/srv/app", "name": "s-header", "repo_root": "/srv/app",
	})
	require.Equal(t, mcpRemoteHost, sessionHost(t, db, out["session_id"].(string)).Host)

	// Explicit arg wins over the header.
	out = callJSON(t, ctx, remote, "session_start", map[string]any{
		"cwd": "/srv/app", "name": "s-arg", "host": "gamma", "repo_root": "/srv/app",
	})
	require.Equal(t, "gamma", sessionHost(t, db, out["session_id"].(string)).Host)
}

// A remote cwd is placed from the roots the CLIENT resolved, and the project it
// registers belongs to that machine -- not to the daemon's map.
func TestSessionStart_RemoteRootsPlaceTheProject(t *testing.T) {
	ctx := context.Background()
	url, db := newHostServer(t)
	cli := dialFrom(t, ctx, url, mcpRemoteHost)

	out := callJSON(t, ctx, cli, "session_start", map[string]any{
		"cwd": `C:\repos\myapp\internal`, "repo_root": `C:\repos\myapp`,
		"main_worktree_root": `C:\repos\myapp`, "repo_origin": "https://github.com/acme/myapp.git",
	})
	require.Equal(t, "myapp", out["project"])
	require.Nil(t, out["warning"], "a placeable session carries no warning")

	local, err := store.ResolveProjectForCWD(ctx, db, mcpLocalHost, `C:\repos\myapp`)
	require.NoError(t, err)
	require.Empty(t, local, "the remote mapping is not the daemon's")
}

// A remote cwd with no roots cannot be placed. The call still SUCCEEDS -- the
// session is worth having -- but it is global and says why, rather than silently
// resolving against the daemon's own filesystem.
func TestSessionStart_RemoteWithoutRootWarnsAndStaysGlobal(t *testing.T) {
	ctx := context.Background()
	url, _ := newHostServer(t)
	cli := dialFrom(t, ctx, url, mcpRemoteHost)

	out := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	require.Empty(t, out["project"], "a remote cwd matching a LOCAL mapped path is not that project")
	warning, _ := out["warning"].(string)
	require.Contains(t, warning, "repo_root")
	require.Contains(t, warning, mcpRemoteHost)
}

// The ambient fallback asks the caller's own machine first: two agents in
// identically-named directories on two machines are not an ambiguous pair.
func TestAmbientFallback_NarrowsToTheCallersHost(t *testing.T) {
	ctx := context.Background()
	url, db := newHostServer(t)
	now := time.Now().UTC()

	mk := func(name, host, project string) {
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateSession(ctx, db, core.Session{
			ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive,
			Ambient: true, Host: host, CWD: "/work/demo", CreatedAt: now, UpdatedAt: now,
		}))
	}
	mk("cc/local", mcpLocalHost, "demo")
	mk("cc/remote", mcpRemoteHost, "other")

	// Unbound write from the local machine: two ambients across two projects
	// would be ambiguous without the host narrowing.
	cli := dialFrom(t, ctx, url, mcpLocalHost)
	out := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "local-inference", "kind": "reference",
		"description": "written without a binding", "body": "b",
	})
	require.Equal(t, "demo", out["project"], "the caller's own machine decides the scope")

	// And from the other machine, the other project.
	remote := dialFrom(t, ctx, url, mcpRemoteHost)
	out = callJSON(t, ctx, remote, "memory_write", map[string]any{
		"name": "remote-inference", "kind": "reference",
		"description": "written without a binding", "body": "b",
	})
	require.Equal(t, "other", out["project"])
}

// With nothing active on the caller's own machine, the search widens to every
// host -- which is what keeps a single-machine install (and a client that sends
// no host at all) behaving exactly as before.
func TestAmbientFallback_WidensWhenTheCallersHostIsIdle(t *testing.T) {
	ctx := context.Background()
	url, db := newHostServer(t)
	now := time.Now().UTC()

	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: id, Name: "cc/remote-only", ProjectSlug: "other", Status: core.SessionActive,
		Ambient: true, Host: mcpRemoteHost, CWD: "/srv/app", CreatedAt: now, UpdatedAt: now,
	}))

	cli := dialFrom(t, ctx, url, mcpLocalHost)
	out := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "widened-inference", "kind": "reference",
		"description": "written without a binding", "body": "b",
	})
	require.Equal(t, "other", out["project"],
		"nothing active here: the sole ambient anywhere is still an unambiguous answer")
}
