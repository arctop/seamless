package mcp_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/mcp/mcpversion"
	"github.com/arctop/seamless/internal/store"
)

// clNoticeCall calls a tool and returns its whole result, every content item.
func clNoticeCall(t *testing.T, ctx context.Context, cli *mcpclient.Client, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
	require.NoError(t, err)
	return res
}

// clNoticeTexts is every text item of a result after the first (the handler's
// own answer): the notices the middlewares appended.
func clNoticeTexts(res *mcp.CallToolResult) []string {
	var out []string
	for i, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok && i > 0 {
			out = append(out, tc.Text)
		}
	}
	return out
}

// clLapsedTask creates a task in project and claims it for session two hours
// ago with a one-minute lease, so the claim has lapsed; it returns the task id.
func clLapsedTask(t *testing.T, ctx context.Context, db *sql.DB, project, session string) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	then := time.Now().UTC().Add(-2 * time.Hour)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: id, ProjectSlug: project, Title: "held work",
		Status: core.TaskOpen, CreatedAt: then, UpdatedAt: then}))
	_, err = store.ClaimTask(ctx, db, id, session, time.Minute, then)
	require.NoError(t, err)
	return id
}

// lease_seconds above the ceiling is refused, and the refusal names the
// ceiling and how to hold a task longer; the ceiling itself is accepted. The
// tool's words say how long a claim lasts and that it keeps its session live.
func TestTasksClaim_LeaseCeiling(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	id, _ := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "long gate"})["id"].(string)
	require.NotEmpty(t, id)

	for _, over := range []any{14401, "14401", 86400} {
		isErr, txt := callErr(t, ctx, cli, "tasks_claim", map[string]any{"id": id, "lease_seconds": over})
		require.True(t, isErr, "lease_seconds %v is above the ceiling", over)
		require.Contains(t, txt, "invalid lease_seconds")
		require.Contains(t, txt, "at most 14400 seconds (4 hours)")
		require.Contains(t, txt, "re-claim it (tasks_claim again) before the lease lapses")
	}

	claimed := callJSON(t, ctx, cli, "tasks_claim", map[string]any{"id": id, "lease_seconds": 14400})
	require.Equal(t, "in_progress", claimed["status"])
	exp, err := time.Parse(time.RFC3339Nano, claimed["lease_expires_at"].(string))
	require.NoError(t, err)
	require.InDelta(t, core.MaxClaimLease.Seconds(), time.Until(exp).Seconds(), 60, "the ceiling itself is allowed")

	var tool mcp.Tool
	for _, c := range mcpserver.Catalog() {
		if c.Name == "tasks_claim" {
			tool = c
		}
	}
	require.Contains(t, tool.Description, "at most 14400 seconds (4 hours)")
	require.Contains(t, tool.Description, "a claim keeps its session live")
	require.Contains(t, tool.Description, "is told once, in its next Seamless tool result or prompt")
	lease, ok := tool.InputSchema.Properties["lease_seconds"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, lease["description"], "at most 14400")
}

// A claim another session takes after its lease lapsed is news to the session
// that held it: its next tool result -- any tool -- carries one notice naming
// the task, the time and the session that took it, and the result after that
// carries none. The session that took it is told nothing.
func TestClaimNotice_ReclaimIsToldOnceInTheNextToolResult(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	holder := seedAgentAmbient(t, ctx, db, "cc/holder", "demo", "/work/demo", "100.1")
	seedAgentAmbient(t, ctx, db, "cc/taker", "demo", "/work/demo", "200.2")
	taskID := clLapsedTask(t, ctx, db, "demo", holder)

	taker := dialAgent(t, ctx, url, "200.2")
	res := clNoticeCall(t, ctx, taker, "tasks_claim", map[string]any{"id": taskID})
	require.False(t, res.IsError, resultText(t, res))
	require.Empty(t, clNoticeTexts(res), "the taker lost nothing")

	pending, err := store.PendingClaimNotices(ctx, db, holder)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	cli := dialAgent(t, ctx, url, "100.1")
	res = clNoticeCall(t, ctx, cli, "tasks_ready", map[string]any{})
	require.False(t, res.IsError)
	notices := clNoticeTexts(res)
	require.Len(t, notices, 1)
	require.True(t, strings.HasPrefix(notices[0], "Claim lost: your claim on task "+taskID+" in project demo lapsed at "), notices[0])
	require.Contains(t, notices[0], "and session cc/taker claimed it at ")
	require.Contains(t, notices[0], "Pick other work, or claim it again with tasks_claim once it is free.")

	res = clNoticeCall(t, ctx, cli, "tasks_ready", map[string]any{})
	require.Empty(t, clNoticeTexts(res), "told once")
}

// The owner's force release is news too, told the same way, once.
func TestClaimNotice_ForceReleaseIsToldOnce(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	seedAgentAmbient(t, ctx, db, "cc/holder", "demo", "/work/demo", "100.1")
	cli := dialAgent(t, ctx, url, "100.1")
	id, _ := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "held work"})["id"].(string)
	callJSON(t, ctx, cli, "tasks_claim", map[string]any{"id": id})

	_, err := store.ForceReleaseTask(ctx, db, id, time.Now().UTC())
	require.NoError(t, err)

	res := clNoticeCall(t, ctx, cli, "tasks_list", map[string]any{"id": id})
	require.False(t, res.IsError)
	notices := clNoticeTexts(res)
	require.Len(t, notices, 1)
	require.True(t, strings.HasPrefix(notices[0], "Claim lost: the owner released your claim on task "+id+" in project demo at "), notices[0])
	require.Empty(t, clNoticeTexts(clNoticeCall(t, ctx, cli, "tasks_list", map[string]any{"id": id})))
}

// The seam CLI shows only a result's first text item, so its calls carry no
// notice: the notice stays pending for the agent's own next result.
func TestClaimNotice_TheSeamCLIIsNotToldAndLeavesItPending(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	holder := seedAgentAmbient(t, ctx, db, "cc/holder", "demo", "/work/demo", "100.1")
	taskID := clLapsedTask(t, ctx, db, "demo", holder)
	_, err := store.ForceReleaseTask(ctx, db, taskID, time.Now().UTC())
	require.NoError(t, err)

	headers := map[string]string{"Authorization": "Bearer " + testKey, mcpserver.AgentProcessHeader: "100.1"}
	cliTr, err := mcpclient.NewStreamableHttpClient(url, transport.WithHTTPHeaders(headers))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cliTr.Close() })
	require.NoError(t, cliTr.Start(ctx))
	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcpversion.Client
	initReq.Params.ClientInfo = mcp.Implementation{Name: "seam-cli", Version: "0"}
	_, err = cliTr.Initialize(ctx, initReq)
	require.NoError(t, err)

	res := clNoticeCall(t, ctx, cliTr, "tasks_ready", map[string]any{})
	require.False(t, res.IsError)
	require.Empty(t, clNoticeTexts(res), "the CLI would drop it unseen")
	pending, err := store.PendingClaimNotices(ctx, db, holder)
	require.NoError(t, err)
	require.Len(t, pending, 1, "still waiting for the agent")

	agent := dialAgent(t, ctx, url, "100.1")
	require.Len(t, clNoticeTexts(clNoticeCall(t, ctx, agent, "tasks_ready", map[string]any{})), 1)
}
