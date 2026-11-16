package mcp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	mcpserver "github.com/arctop/seamless/internal/mcp"
)

func TestUsageSummaryTool(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	// Write a memory and recall it, so the summary has something to report.
	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "a-fact", "kind": "reference", "description": "a durable fact", "body": "body", "project": "global",
	})
	callJSON(t, ctx, cli, "recall", map[string]any{"query": "durable fact"})

	out := callJSON(t, ctx, cli, "usage_summary", nil)
	mems, ok := out["memories"].(map[string]any)
	require.True(t, ok, "summary has a memories section")
	require.Equal(t, float64(1), mems["active"])
	ret, ok := out["retrieval"].(map[string]any)
	require.True(t, ok)
	_, ok = out["eventsByKind"].(map[string]any)
	require.True(t, ok)

	// The read-after-inject funnel split rides on the retrieval section: one
	// entry per injection surface, in a stable order.
	funnel, ok := ret["funnelBySurface"].([]any)
	require.True(t, ok, "retrieval carries funnelBySurface")
	require.Len(t, funnel, 2)
	first, ok := funnel[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "session-start", first["surface"])
	second, ok := funnel[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "subagent-start", second["surface"])
}

// seedInjections records n recall-sourced injections of one memory. Recall's
// source is what carries utility weight (a passive briefing injection is
// exposure, not demand), so this is the one shape that populates BOTH
// topInjected and topUtility. Each injection gets its own session id because
// utility credits a (item, session, class) triple once.
func seedInjections(t *testing.T, ctx context.Context, db *sql.DB, memID string, n int) {
	t.Helper()
	rec := events.NewRecorder(db)
	for i := range n {
		_, err := rec.Record(ctx, core.Event{
			Kind: core.EventInjected, ItemID: memID, SessionID: memID + "/s" + strconv.Itoa(i),
			Payload: map[string]any{"source": "recall"},
		})
		require.NoError(t, err)
	}
}

// topNames pulls the memory names out of one of the summary's ranked lists. A
// list that came back null (nothing readable, or nothing injected at all) reads
// as empty -- the two must stay indistinguishable.
func topNames(t *testing.T, out map[string]any, key string) []string {
	t.Helper()
	ret, ok := out["retrieval"].(map[string]any)
	require.True(t, ok, "summary has a retrieval section")
	rows, _ := ret[key].([]any)
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		row, ok := r.(map[string]any)
		require.True(t, ok, "%s row is an object", key)
		name, ok := row["name"].(string)
		require.True(t, ok, "%s row carries a name", key)
		names = append(names, name)
	}
	return names
}

// seedRankedFleet stands up three memories with descending injection counts --
// one per project -- and returns their ids by project. The fenced projects
// deliberately OUTRANK the open one, so a caller that sees only the open memory
// proves rows were dropped rather than merely never ranked.
func seedRankedFleet(t *testing.T, ctx context.Context, db *sql.DB, clients map[string]*mcpclient.Client) map[string]string {
	t.Helper()
	ids := map[string]string{}
	for _, seed := range []struct {
		project string
		injects int
	}{
		{"secret", 5}, {"vault", 4}, {"demo", 2},
	} {
		cli, ok := clients[seed.project]
		require.True(t, ok, "no client bound to %s", seed.project)
		id := callJSON(t, ctx, cli, "memory_write", map[string]any{
			"name": seed.project + "-topology", "kind": "reference",
			"description": "the client's deployment topology", "body": "b\n",
		})["id"].(string)
		ids[seed.project] = id
		seedInjections(t, ctx, db, id, seed.injects)
	}
	return ids
}

// TestUsageSummaryFenceDropsIsolatedMemoryNames is the leak itself: the ranked
// retrieval lists are computed machine-wide and the rows carry no project, so a
// confidential or sealed project's most-injected MEMORY NAMES reached every
// caller. A ranked list of names is a table of contents, which is content.
//
// The same test pins the totals decision: the counts around those lists stay
// machine-wide, so the outside caller's injection total is strictly larger than
// what its own visible rows sum to, and its active-memory count includes the
// memories it was not shown.
func TestUsageSummaryFenceDropsIsolatedMemoryNames(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	isolate(t, ctx, db, "secret", core.IsolationConfidential)
	isolate(t, ctx, db, "vault", core.IsolationSealed)

	// Every client binds BEFORE any memory exists, so no session-start briefing
	// injection perturbs the ranking these assertions read.
	clients := map[string]*mcpclient.Client{
		"secret": bindClient(t, ctx, db, url, "secret"),
		"vault":  bindClient(t, ctx, db, url, "vault"),
		"demo":   bindClient(t, ctx, db, url, "demo"),
	}
	outside := bindClient(t, ctx, db, url, "elsewhere")
	ids := seedRankedFleet(t, ctx, db, clients)

	out := callJSON(t, ctx, outside, "usage_summary", nil)
	raw, err := json.Marshal(out)
	require.NoError(t, err)

	require.Equal(t, []string{"demo-topology"}, topNames(t, out, "topInjected"),
		"the fenced projects' names must not rank for an outside caller: %s", raw)
	require.Equal(t, []string{"demo-topology"}, topNames(t, out, "topUtility"),
		"the fenced projects' names must not rank for an outside caller: %s", raw)
	for _, project := range []string{"secret", "vault"} {
		require.NotContains(t, string(raw), project+"-topology",
			"%s's memory name leaked into the summary: %s", project, raw)
		require.NotContains(t, string(raw), ids[project],
			"%s's memory id leaked into the summary: %s", project, raw)
	}

	// No count of what was removed, anywhere. A single entity's write response
	// carries a "withheld" marker so an absent field never reads as an empty one;
	// beside an aggregate the same marker would be a direct readout of hidden
	// volume, which is what the ranking must never report.
	require.NotContains(t, string(raw), "withheld", "an aggregate must not count what it dropped: %s", raw)

	ret, ok := out["retrieval"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(11), ret["injections"],
		"the retrieval total stays machine-wide (5+4+2), not the visible rows' 2: %s", raw)
	mems, ok := out["memories"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(3), mems["active"],
		"the memory count stays machine-wide, fenced projects included: %s", raw)
}

// TestUsageSummaryFenceBoundCallerSeesItsOwn is the other side: the fence must
// cost a fenced project's own agent nothing. Confidential fences outbound only,
// so its agent keeps the open project's rows too; sealed collapses the ranking
// to the caller's own project.
func TestUsageSummaryFenceBoundCallerSeesItsOwn(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	isolate(t, ctx, db, "secret", core.IsolationConfidential)
	isolate(t, ctx, db, "vault", core.IsolationSealed)

	clients := map[string]*mcpclient.Client{
		"secret": bindClient(t, ctx, db, url, "secret"),
		"vault":  bindClient(t, ctx, db, url, "vault"),
		"demo":   bindClient(t, ctx, db, url, "demo"),
	}
	seedRankedFleet(t, ctx, db, clients)

	conf := callJSON(t, ctx, clients["secret"], "usage_summary", nil)
	require.Equal(t, []string{"secret-topology", "demo-topology"}, topNames(t, conf, "topInjected"),
		"a confidential caller keeps its own rows and the open ones, and loses the sealed project's")
	require.Equal(t, []string{"secret-topology", "demo-topology"}, topNames(t, conf, "topUtility"))

	sealed := callJSON(t, ctx, clients["vault"], "usage_summary", nil)
	require.Equal(t, []string{"vault-topology"}, topNames(t, sealed, "topInjected"),
		"a sealed caller reads nothing outside itself, its own rows included")
	require.Equal(t, []string{"vault-topology"}, topNames(t, sealed, "topUtility"))
}

// TestUsageSummaryFenceUnboundCallerSeesNoFencedNames covers the fence's honest
// limit: a connection whose scope cannot be pinned is judged as GLOBAL, which
// owns nothing an isolated project fences, so it fails closed here rather than
// inheriting the access of whichever agent happened to be nearby.
func TestUsageSummaryFenceUnboundCallerSeesNoFencedNames(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	isolate(t, ctx, db, "secret", core.IsolationConfidential)
	isolate(t, ctx, db, "vault", core.IsolationSealed)

	clients := map[string]*mcpclient.Client{
		"secret": bindClient(t, ctx, db, url, "secret"),
		"vault":  bindClient(t, ctx, db, url, "vault"),
		"demo":   bindClient(t, ctx, db, url, "demo"),
	}
	seedRankedFleet(t, ctx, db, clients)

	unbound := dialClient(t, ctx, url, testKey)
	out := callJSON(t, ctx, unbound, "usage_summary", nil)
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	require.Equal(t, []string{"demo-topology"}, topNames(t, out, "topInjected"),
		"an unbound caller is judged global and must not see fenced names: %s", raw)
	require.Equal(t, []string{"demo-topology"}, topNames(t, out, "topUtility"))
	require.NotContains(t, string(raw), "secret-topology", raw)
	require.NotContains(t, string(raw), "vault-topology", raw)
}

// TestUsageSummaryOpenProjectsUnchanged pins the side a regression would hurt
// most: with nothing isolated, every caller gets the identical ranked lists it
// got before the fence existed -- full length, same order, and null (never [])
// when there is nothing to rank.
func TestUsageSummaryOpenProjectsUnchanged(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)

	clients := map[string]*mcpclient.Client{
		"secret": bindClient(t, ctx, db, url, "secret"),
		"vault":  bindClient(t, ctx, db, url, "vault"),
		"demo":   bindClient(t, ctx, db, url, "demo"),
	}
	outside := bindClient(t, ctx, db, url, "elsewhere")

	// Before any memory exists the lists are absent, not empty: an all-withheld
	// list returns to that same shape, so the two can never be told apart.
	empty := callJSON(t, ctx, outside, "usage_summary", nil)
	ret, ok := empty["retrieval"].(map[string]any)
	require.True(t, ok)
	require.Nil(t, ret["topInjected"])
	require.Nil(t, ret["topUtility"])

	seedRankedFleet(t, ctx, db, clients)

	want := []string{"secret-topology", "vault-topology", "demo-topology"}
	for _, who := range []string{"secret", "vault", "demo"} {
		out := callJSON(t, ctx, clients[who], "usage_summary", nil)
		require.Equal(t, want, topNames(t, out, "topInjected"), "open ranking, seen from %s", who)
		require.Equal(t, want, topNames(t, out, "topUtility"), "open ranking, seen from %s", who)
	}
	out := callJSON(t, ctx, outside, "usage_summary", nil)
	require.Equal(t, want, topNames(t, out, "topInjected"))
	require.Equal(t, want, topNames(t, out, "topUtility"))
}

// TestUsageSummaryDescriptionRecordsTotalsDecision keeps the contract honest:
// the payload now has two kinds of number in it, and an agent reading a
// machine-wide total as the total of what it was shown would draw exactly the
// wrong conclusion. Guidance and behavior are one fix, so the description is
// asserted, not assumed.
func TestUsageSummaryDescriptionRecordsTotalsDecision(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	listed, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)
	var desc string
	for _, tool := range listed.Tools {
		if tool.Name == "usage_summary" {
			desc = tool.Description
		}
	}
	require.NotEmpty(t, desc, "usage_summary is registered")
	require.Contains(t, desc, "Every count is machine-wide and identical for every caller")
	require.Contains(t, desc, "topInjected, topUtility")
	require.Contains(t, desc, "project isolation")
}

// TestToolCountMatchesRegistered mirrors the doctor assertion: the server must
// register exactly ToolCount tools (26 at P4; 28 with tasks_claim + tasks_release;
// 29 with gardener_request; 30 with gardener_split; 31 with favorite_set;
// 33 with memory_edit + notes_edit).
func TestToolCountMatchesRegistered(t *testing.T) {
	srv := mcpserver.New(mcpserver.Config{})
	require.Equal(t, mcpserver.ToolCount, srv.NumTools())
	require.Equal(t, 37, mcpserver.ToolCount)
}
