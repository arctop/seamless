package mcp_test

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	mcpserver "github.com/arctop/seamless/internal/mcp"
)

// statelessServer serves the stateless MCP revision too, which production does
// not yet (Config.ServeStatelessMCP). The tests on it dial asking for that
// revision (mcp.LATEST_PROTOCOL_VERSION), as a client that speaks it would;
// Seamless's own clients ask for mcpversion.Client.
func statelessServer(t *testing.T) string {
	url, _ := newServerCfg(t, func(c *mcpserver.Config) { c.ServeStatelessMCP = true })
	return url
}

// On a stateless connection, session_start's binding lasts one call. The result
// says so, naming the handle to thread, and a call carrying session= is scoped
// to and credited to that session as the binding would have made it.
func TestSessionHandle_StatelessConnectionThreadsTheSession(t *testing.T) {
	ctx := context.Background()
	cli := dialClientAt(t, ctx, statelessServer(t), testKey, mcp.LATEST_PROTOCOL_VERSION)

	start := callJSON(t, ctx, cli, "session_start", map[string]any{"project": "alpha"})
	name, _ := start["name"].(string)
	require.NotEmpty(t, name)
	require.Contains(t, start["stateless"], "session="+name, "the result must name the handle to thread")

	// The binding did not outlive session_start: an unscoped write has nothing
	// to resolve its project from, and is refused rather than guessed.
	isErr, text := callErr(t, ctx, cli, "memory_write", map[string]any{
		"name": "unbound-write", "kind": "reference", "description": "d", "body": "b",
	})
	require.True(t, isErr, "an unbound stateless write must be refused, got: %s", text)

	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "handle-write", "kind": "reference", "description": "d", "body": "b",
		"session": name,
	})
	r := callJSON(t, ctx, cli, "memory_read", map[string]any{"name": "handle-write", "session": name})
	require.Equal(t, "alpha", r["project"], "the handle's session scopes the write")
	require.Equal(t, start["session_id"], r["source_session"], "the handle's session is credited")

	// A lab cannot bind to a stateless connection either: lab_open says to pass
	// lab=, and trial_record without one says the same rather than "call lab_open".
	lab := callJSON(t, ctx, cli, "lab_open", map[string]any{"lab": "probe", "session": name})
	require.Contains(t, lab["stateless"], "lab=probe")
	isErr, text = callErr(t, ctx, cli, "trial_record", map[string]any{"title": "t", "session": name})
	require.True(t, isErr)
	require.Contains(t, text, "pass lab")
	callJSON(t, ctx, cli, "trial_record", map[string]any{"title": "t", "lab": "probe", "session": name})
}

// On a session connection the handle is this call's explicit choice, so it wins
// over the connection's session_start binding -- for that call only.
func TestSessionHandle_WinsOverConnectionBinding(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	other := dialClient(t, ctx, url, testKey)

	alpha := callJSON(t, ctx, cli, "session_start", map[string]any{"project": "alpha"})
	require.NotContains(t, alpha, "stateless", "a session connection keeps its binding, so no handle is needed")
	beta := callJSON(t, ctx, other, "session_start", map[string]any{"project": "beta"})

	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "via-handle", "kind": "reference", "description": "d", "body": "b",
		"session": beta["name"],
	})
	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "via-binding", "kind": "reference", "description": "d", "body": "b",
	})

	r := callJSON(t, ctx, other, "memory_read", map[string]any{"name": "via-handle"})
	require.Equal(t, "beta", r["project"])
	require.Equal(t, beta["session_id"], r["source_session"])
	r = callJSON(t, ctx, cli, "memory_read", map[string]any{"name": "via-binding"})
	require.Equal(t, "alpha", r["project"], "the handle must not rebind the connection")
	require.Equal(t, alpha["session_id"], r["source_session"])
}

// A handle that names no session is refused, never treated as absent: falling
// through to the connection's binding would credit and scope the call to a
// session the caller did not name.
func TestSessionHandle_UnknownSessionRefused(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"project": "alpha"})

	isErr, text := callErr(t, ctx, cli, "memory_write", map[string]any{
		"name": "x", "kind": "reference", "description": "d", "body": "b", "session": "sess/nope",
	})
	require.True(t, isErr)
	require.Contains(t, text, `session "sess/nope" not found`)
}

// Every tool but session_start declares the handle, in the registered schemas
// and in the catalog the docs render alike.
func TestSessionHandle_DeclaredOnEveryToolButSessionStart(t *testing.T) {
	for _, tool := range mcpserver.Catalog() {
		_, declared := tool.InputSchema.Properties["session"]
		require.Equal(t, tool.Name != "session_start", declared, tool.Name)
	}
}

// A Claude Code or Codex agent needs no handle on a stateless connection: its
// process binding rides on every request, not on the connection, so its own
// session scopes and credits a bare call, and session_start adopting that
// session has nothing to tell it.
func TestSessionHandle_ProcessBindingNeedsNoHandleWhenStateless(t *testing.T) {
	ctx := context.Background()
	url, db := newServerCfg(t, func(c *mcpserver.Config) { c.ServeStatelessMCP = true })
	alpha := seedAgentAmbient(t, ctx, db, "cc/alpha", "demo", "/work/demo", "100.1")
	seedAgentAmbient(t, ctx, db, "cc/beta", "other", "/work/other", "200.2")

	a := dialAgentAt(t, ctx, url, "100.1", mcp.LATEST_PROTOCOL_VERSION)
	require.Equal(t, "demo", writeMemory(t, ctx, a, "alpha-note", nil)["project"])
	require.Equal(t, alpha, lastToolCall(t, db, "memory_write"))

	own := callJSON(t, ctx, a, "session_start", map[string]any{})
	require.Equal(t, alpha, own["session_id"])
	require.NotContains(t, own, "stateless")
}
