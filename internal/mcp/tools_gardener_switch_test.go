package mcp_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/gardener"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/store"
)

// aaSeedPending seeds one pending archive proposal about a real memory and
// returns its id.
func aaSeedPending(t *testing.T, ctx context.Context, db *sql.DB, memID string) string {
	t.Helper()
	p, err := store.CreateProposal(ctx, db, store.ProposalArchive, map[string]any{
		"key": "archive:" + memID, "id": memID, "name": "stale-thing",
	})
	require.NoError(t, err)
	return p.ID
}

// aaRequireRefused checks that each of gardener_apply's three decisions is
// refused, in the owner-curation words, and leaves the proposal pending.
func aaRequireRefused(t *testing.T, ctx context.Context, cli interface {
	CallTool(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error)
}, db *sql.DB, id string) {
	t.Helper()
	for _, action := range gardener.Decisions {
		isErr, text := callErr(t, ctx, cli, "gardener_apply", map[string]any{"id": id, "action": action})
		require.True(t, isErr, action)
		require.Contains(t, text, "not open to agents: the owner has switched agents' applying off (gardener.agents_may_apply)", action)
		require.Contains(t, text, "the owner raises and resolves them on the console's Gardener screen", action)
	}
	// An id that names nothing is refused the same way: the answer says
	// nothing about the id.
	isErr, text := callErr(t, ctx, cli, "gardener_apply", map[string]any{"id": "01NOSUCHPROPOSAL"})
	require.True(t, isErr)
	require.Contains(t, text, "not open to agents")
	p, ok, err := store.ProposalByID(ctx, db, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, store.ProposalPending, p.Status)
}

// Turned off from the console, with no restart: every decision is refused and
// the listing says the switch is off. Reset, and an agent applies again.
func TestAaGardenerSwitch_ConsoleOffRefusesAgents(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	wrote := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "stale-thing", "kind": "gotcha", "description": "an old memory", "body": "body text", "project": "global",
	})
	memID, _ := wrote["id"].(string)
	id := aaSeedPending(t, ctx, db, memID)

	require.NoError(t, store.SetGardenerOverride(ctx, db, config.GardenerOverride{AgentsMayApply: new(false)}))
	aaRequireRefused(t, ctx, cli, db, id)
	listed := callJSON(t, ctx, cli, "gardener_proposals", nil)
	require.Equal(t, float64(1), listed["count"], "the queue stays readable")
	require.Equal(t, false, listed["agentsMayApply"])
	require.Contains(t, listed["note"], "gardener_apply refuses apply, dismiss and hide")

	require.NoError(t, store.ClearGardenerOverride(ctx, db))
	applied := callJSON(t, ctx, cli, "gardener_apply", map[string]any{"id": id})
	require.Equal(t, "applied", applied["status"])
}

// agents_may_apply: false in the file is final: a console override of true
// does not reopen it.
func TestAaGardenerSwitch_FileOffIsFinal(t *testing.T) {
	ctx := context.Background()
	url, db, _, _ := newServerFiles(t, func(c *mcpserver.Config) {
		c.Gardener = gardener.New(c.DB, c.Files, nil, nil, c.Events, gardener.Config{AgentsMayApply: new(false)}, nil)
	})
	cli := dialClient(t, ctx, url, testKey)
	wrote := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "stale-thing", "kind": "gotcha", "description": "an old memory", "body": "body text", "project": "global",
	})
	memID, _ := wrote["id"].(string)
	id := aaSeedPending(t, ctx, db, memID)
	aaRequireRefused(t, ctx, cli, db, id)
	require.NoError(t, store.SetGardenerOverride(ctx, db, config.GardenerOverride{AgentsMayApply: new(true)}))
	aaRequireRefused(t, ctx, cli, db, id)
}

// On -- the default -- the listing is exactly what it was before the switch:
// no new keys. The apply tool's description names the switch.
func TestAaGardenerSwitch_DefaultOnAndDescribed(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	listed := callJSON(t, ctx, cli, "gardener_proposals", nil)
	require.Len(t, listed, 2, "count and proposals only: %v", listed)
	require.Contains(t, listed, "count")
	require.Contains(t, listed, "proposals")

	tools, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)
	var desc string
	for _, tool := range tools.Tools {
		if tool.Name == "gardener_apply" {
			desc = tool.Description
		}
	}
	require.Contains(t, desc, "The owner can switch agents' applying off (gardener.agents_may_apply)")
	require.Contains(t, desc, "this tool refuses apply, dismiss and hide alike")
}
