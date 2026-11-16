package mcp_test

import (
	"context"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/features"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/store"
)

// researchTools is the gated tool set, read from the registry rather than
// transcribed: the registry is the single source of truth for which tools a
// feature owns, and a hand-copied list here would be exactly the drift the
// registry exists to prevent.
func researchTools(t *testing.T) []string {
	t.Helper()
	f, ok := features.Get(features.Research)
	require.True(t, ok, "the research feature must be registered")
	require.NotEmpty(t, f.Tools)
	return f.Tools
}

// defaultFeatures builds a server with the shipped config -- every optional
// feature off -- rather than the research-enabled shared fixture.
func defaultFeatures(c *mcpserver.Config) { c.Features = config.Features{} }

// listedTools returns the tool names the live tools/list advertises.
func listedTools(t *testing.T, ctx context.Context, cli *mcpclient.Client) []string {
	t.Helper()
	listed, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// requireGated asserts each named tool is absent from tools/list AND rejected by
// tools/call with the mcp-go tool-not-found error. Both halves matter: a tool
// hidden from discovery but still callable would be a visibility hint rather
// than a gate, and an agent holding a cached tool list is exactly the caller that
// finds out.
func requireGated(t *testing.T, ctx context.Context, cli *mcpclient.Client, names []string) {
	t.Helper()
	listed := listedTools(t, ctx, cli)
	for _, name := range names {
		require.NotContains(t, listed, name, "%s must not be advertised while its feature is off", name)
		_, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name}})
		require.Error(t, err, "%s must not be callable while its feature is off", name)
		require.ErrorContains(t, err, "not found",
			"a gated call is tool-not-found: from this server the tool does not exist")
	}
}

// requireExposed asserts each named tool is advertised again.
func requireExposed(t *testing.T, ctx context.Context, cli *mcpclient.Client, names []string) {
	t.Helper()
	listed := listedTools(t, ctx, cli)
	for _, name := range names {
		require.Contains(t, listed, name, "%s must be advertised while its feature is on", name)
	}
}

// With the shipped config every optional feature is off, so the live surface is
// the registered surface minus the gated tools. ToolCount is unchanged: it counts
// registration, and registration is untouched by the filter.
func TestFeatureGate_DefaultConfigHidesResearchTools(t *testing.T) {
	ctx := context.Background()
	url, _ := newServerCfg(t, defaultFeatures)
	cli := dialClient(t, ctx, url, testKey)

	gated := researchTools(t)
	require.Len(t, listedTools(t, ctx, cli), mcpserver.ToolCount-len(features.HiddenTools(config.Features{})))
	requireGated(t, ctx, cli, gated)
}

// With the feature on, the live list is the full registered surface and the tools
// work end to end.
func TestFeatureGate_EnabledExposesFullSurface(t *testing.T) {
	ctx := context.Background()
	url, _ := newServerCfg(t, func(c *mcpserver.Config) { c.Features = config.Features{Research: true, RepoLocks: true} })
	cli := dialClient(t, ctx, url, testKey)

	require.Len(t, listedTools(t, ctx, cli), mcpserver.ToolCount)
	requireExposed(t, ctx, cli, researchTools(t))

	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	callJSON(t, ctx, cli, "lab_open", map[string]any{"lab": "gate-lab", "goal": "g"})
	rec := callJSON(t, ctx, cli, "trial_record", map[string]any{
		"title": "t", "changes": "c", "expected": "e", "actual": "a", "outcome": "pass",
	})
	require.NotEmpty(t, rec["id"])
	q := callJSON(t, ctx, cli, "trial_query", map[string]any{"lab": "gate-lab"})
	require.NotEmpty(t, q["trials"])
}

// The gate resolves the effective config PER REQUEST, not once at construction.
// The proof is that a single long-lived server and a single already-initialized
// connection change their answer when the stored override row changes underneath
// them -- no restart, no reconnect, no re-initialize. A construction-time snapshot
// would keep answering with the config the server was built with.
func TestFeatureGate_StoreOverrideAppliesLiveInBothDirections(t *testing.T) {
	ctx := context.Background()
	gated := researchTools(t)

	t.Run("base off, override on", func(t *testing.T) {
		url, db := newServerCfg(t, defaultFeatures)
		cli := dialClient(t, ctx, url, testKey)
		requireGated(t, ctx, cli, gated)

		require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Research: true, RepoLocks: true}))
		require.Len(t, listedTools(t, ctx, cli), mcpserver.ToolCount)
		requireExposed(t, ctx, cli, gated)
		callJSON(t, ctx, cli, "lab_open", map[string]any{"lab": "live-lab"})

		// And back: the same connection loses the tools again.
		require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Research: false}))
		requireGated(t, ctx, cli, gated)

		// Reset-by-deletion falls back to the file/env base, which is off here.
		require.NoError(t, store.ClearFeaturesConfig(ctx, db))
		requireGated(t, ctx, cli, gated)
	})

	t.Run("base on, override off", func(t *testing.T) {
		url, db := newServerCfg(t, func(c *mcpserver.Config) { c.Features = config.Features{Research: true, RepoLocks: true} })
		cli := dialClient(t, ctx, url, testKey)
		requireExposed(t, ctx, cli, gated)

		// The stored row wins over file/env until it is cleared.
		require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Research: false}))
		requireGated(t, ctx, cli, gated)

		require.NoError(t, store.ClearFeaturesConfig(ctx, db))
		requireExposed(t, ctx, cli, gated)
		require.Len(t, listedTools(t, ctx, cli), mcpserver.ToolCount)
	})
}

// A corrupt override row must not fail an agent's call: the gate is failure-soft
// and falls back to the file/env base.
func TestFeatureGate_CorruptOverrideFallsBackToBase(t *testing.T) {
	ctx := context.Background()
	url, db := newServerCfg(t, func(c *mcpserver.Config) { c.Features = config.Features{Research: true, RepoLocks: true} })
	cli := dialClient(t, ctx, url, testKey)

	require.NoError(t, store.SetSetting(ctx, db, store.SettingFeaturesConfig, `{not json`))
	require.Len(t, listedTools(t, ctx, cli), mcpserver.ToolCount,
		"an unreadable override row degrades to the base config, it does not gate everything")
	requireExposed(t, ctx, cli, researchTools(t))
}

// favorite_set stays exposed for its other kinds, so kind=trial carries its own
// in-handler gate. Starring a trial is refused while research is off and works
// again when it is back on -- the trial itself is never touched.
func TestFeatureGate_FavoriteSetTrialKind(t *testing.T) {
	ctx := context.Background()
	url, db := newServerCfg(t, func(c *mcpserver.Config) { c.Features = config.Features{Research: true} })
	cli := dialClient(t, ctx, url, testKey)

	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo", "name": "gate-sess"})
	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "gate-memory", "kind": "gotcha", "description": "d", "body": "b",
	})
	callJSON(t, ctx, cli, "lab_open", map[string]any{"lab": "fav-gate-lab", "goal": "g"})
	trial := callJSON(t, ctx, cli, "trial_record", map[string]any{
		"title": "t", "changes": "c", "expected": "e", "actual": "a", "outcome": "pass",
	})
	trialID, ok := trial["id"].(string)
	require.True(t, ok)

	star := map[string]any{"kind": "trial", "id": trialID, "favorite": true}
	out := callJSON(t, ctx, cli, "favorite_set", star)
	require.Equal(t, true, out["favorite"])

	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Research: false}))
	isErr, txt := callErr(t, ctx, cli, "favorite_set", star)
	require.True(t, isErr, "starring a trial must be refused while research is off")
	require.Contains(t, txt, "research feature is disabled")
	require.Contains(t, txt, "Settings", "the refusal says where to turn it back on")

	// Every other kind is unaffected: the gate is on the trial branch only.
	other := callJSON(t, ctx, cli, "favorite_set",
		map[string]any{"kind": "memory", "id": "gate-memory", "favorite": true})
	require.Equal(t, true, other["favorite"])

	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Research: true}))
	out = callJSON(t, ctx, cli, "favorite_set",
		map[string]any{"kind": "trial", "id": trialID, "favorite": false})
	require.Equal(t, false, out["favorite"], "the trial is still there and starrable again")
}
