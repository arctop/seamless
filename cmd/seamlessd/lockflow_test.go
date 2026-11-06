package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// The locked-project flow end to end, over MCP on an injected socket peer
// with a kernel chain: a call refused at the lock files a request and names
// it; the terminal path approves it (seamlessd approve's core); the agent's
// tree then reads and writes the project while another tree stays locked;
// session_end revokes.
func TestLockFlow_RefuseRequestApproveUseRevoke(t *testing.T) {
	ctx := context.Background()
	const boot = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"
	bridge := access.ProcessToken{PID: 300, Version: 3000, Boot: boot}
	agent := access.ProcessToken{PID: 200, Version: 2000, Boot: boot}
	stranger := access.ProcessToken{PID: 400, Version: 4000, Boot: boot}

	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, slug := range []string{"example-finance", "example-ops"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, "example-finance", core.IsolationConfidential))
	policy, err := access.New(config.PeerAuth{Owner: "owner", Locked: []string{"example-finance"}}, lockOwnerResolve)
	require.NoError(t, err)

	// The agent's session, stamped with its process by the SessionStart hook.
	sessID, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: sessID, Name: "cc/fin", ProjectSlug: "example-ops", Status: core.SessionActive, Ambient: true,
		ExternalSessionID: "cc-fin-x", ExternalClient: "claude-code", CWD: "/work/ops", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.SetSessionAgentProcess(ctx, db, sessID, agent.AgentID()))

	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	rec := events.NewRecorder(db)
	ret := retrieve.New(db, nil, config.Defaults().Budgets, nil)
	ret.SetBodyReader(mgr.Store())
	cfg := config.Defaults()
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", Locked: []string{"example-finance"}}
	svc, err := newApprovals(cfg, db, rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	mcpSrv := mcp.New(mcp.Config{DB: db, Files: mgr, Retrieve: ret, Events: rec, APIKey: "k", LocalHost: "laptop", Approvals: svc})

	serveAs := func(chain ...access.ProcessToken) *httptest.Server {
		hops := make([]access.ChainHop, len(chain))
		for i, c := range chain {
			hops[i] = access.ChainHop{Token: c}
		}
		cred := peerCred{uid: 501, chain: func() ([]access.ChainHop, error) { return hops, nil }}
		h := peerHandler(policy, storeLeases(db), slog.New(slog.NewTextHandler(io.Discard, nil)), mcpSrv.Handler())
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerCredKey{}, cred)))
		}))
		t.Cleanup(func() { closeServer(srv) })
		return srv
	}
	agentSrv := serveAs(bridge, agent)
	strangerSrv := serveAs(stranger)
	agentCli := paClientHeaders(t, agentSrv.URL, "", map[string]string{mcp.AgentProcessHeader: agent.AgentID(), mcp.HostHeader: "laptop"})
	strangerCli := paClient(t, strangerSrv.URL, "")

	call := func(cli interface {
		CallTool(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
	}, name string, args map[string]any) (string, bool) {
		res, err := cli.CallTool(ctx, mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: name, Arguments: args}})
		require.NoError(t, err)
		var b bytes.Buffer
		for _, c := range res.Content {
			if tc, ok := c.(mcpgo.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String(), res.IsError
	}

	// A tool that only filters a locked project out files nothing.
	_, isErr := call(agentCli, "usage_summary", map[string]any{})
	require.False(t, isErr)
	none, err := svc.Pending(ctx)
	require.NoError(t, err)
	require.Empty(t, none, "filtering is not asking")

	// Refused at the lock: the refusal names the request it filed.
	text, isErr := call(agentCli, "tasks_list", map[string]any{"project": "example-finance"})
	require.True(t, isErr)
	short := regexp.MustCompile(`R-[0-9A-Z]{4}`).FindString(text)
	require.NotEmpty(t, short, text)
	require.Contains(t, text, "seam approve "+short)
	text2, _ := call(agentCli, "tasks_list", map[string]any{"project": "example-finance"})
	require.Contains(t, text2, short, "the same request, not a second one")

	pending, err := svc.Pending(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "cc/fin", pending[0].Session, "the daemon found the agent's session")

	// The terminal path's commit.
	var out bytes.Buffer
	require.NoError(t, approve(ctx, svc, &out, []string{short, "--for", "1h", "--digest", pending[0].Digest}))
	require.Contains(t, out.String(), "Approved "+short)

	// The agent's tree writes, and -- bound to the project, as its confidential
	// isolation requires for reads -- reads; another tree stays locked.
	text, isErr = call(agentCli, "tasks_add", map[string]any{"project": "example-finance", "title": "reconcile"})
	require.False(t, isErr, text)
	text, isErr = call(agentCli, "session_start", map[string]any{"project": "example-finance", "cwd": "/work/finance"})
	require.False(t, isErr, "a leased session may be placed in the locked project: %s", text)
	text, isErr = call(agentCli, "tasks_list", map[string]any{"project": "example-finance"})
	require.False(t, isErr, text)
	require.Contains(t, text, "reconcile")
	text, isErr = call(strangerCli, "tasks_list", map[string]any{"project": "example-finance"})
	require.True(t, isErr)
	require.Contains(t, text, "locked")

	// session_end of the session the lease was granted to revokes it.
	_, isErr = call(agentCli, "session_end", map[string]any{"findings": "done", "session": sessID})
	require.False(t, isErr)
	text, isErr = call(agentCli, "tasks_list", map[string]any{"project": "example-finance"})
	require.True(t, isErr, "locked again after session_end")
	require.True(t, strings.Contains(text, "locked"), text)

	// The terminal path denies too; a denied request leaves the project
	// locked, and its arguments are checked.
	again := regexp.MustCompile(`R-[0-9A-Z]{4}`).FindString(text)
	require.NotEmpty(t, again, text)
	require.Error(t, approve(ctx, svc, &out, []string{again, "--deny", "--for", "1h"}), "--deny takes no duration")
	require.Error(t, approve(ctx, svc, &out, []string{again}), "an approval needs --for and --digest")
	require.Error(t, approve(ctx, svc, &out, []string{}), "a request must be named")
	out.Reset()
	require.NoError(t, approve(ctx, svc, &out, []string{again, "--deny"}))
	require.Contains(t, out.String(), "Denied "+again)
	_, isErr = call(agentCli, "tasks_list", map[string]any{"project": "example-finance"})
	require.True(t, isErr)
}
