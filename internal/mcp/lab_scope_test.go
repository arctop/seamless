package mcp_test

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// TestLabOpenDoesNotGlobalizeUnscopedWrites is the regression test for the
// lab_open zero-value-binding bug: on a stateful connection that never ran
// session_start, lab_open used to create a session-less bindings entry that
// getBinding reported as a real session binding, so resolveWriteScope returned
// its empty project and every later unscoped durable create silently landed in
// the global scope instead of being refused. After lab_open, each durable
// create with no project and nothing to inherit from must still fail with the
// ambiguous-scope error.
func TestLabOpenDoesNotGlobalizeUnscopedWrites(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	open := callJSON(t, ctx, cli, "lab_open", map[string]any{"lab": "codex-app-local"})
	require.Equal(t, "codex-app-local", open["lab"])

	calls := []struct {
		tool string
		args map[string]any
	}{
		{"memory_write", map[string]any{
			"name": "lab-scope-leak", "kind": "gotcha",
			"description": "must not land global", "body": "leak\n",
		}},
		{"notes_create", map[string]any{"title": "lab scope leak", "body": "leak\n"}},
		{"tasks_add", map[string]any{"title": "lab scope leak"}},
		{"capture_url", map[string]any{"url": "https://example.com/"}},
		{"trial_record", map[string]any{"title": "lab scope leak"}},
	}
	for _, c := range calls {
		res, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
			Name: c.tool, Arguments: c.args,
		}})
		require.NoError(t, err)
		require.True(t, res.IsError, "%s after a bare lab_open must refuse an unscoped write", c.tool)
		require.Contains(t, resultText(t, res), "ambiguous scope",
			"%s must fail on scope, not land in global", c.tool)
	}

	// Nothing may have leaked into the global scope.
	_, found, err := store.MemoryByName(ctx, db, "", "lab-scope-leak")
	require.NoError(t, err)
	require.False(t, found, "the refused memory_write must not exist in the global scope")
	trials, err := store.QueryTrials(ctx, db, store.TrialFilter{Lab: "codex-app-local"})
	require.NoError(t, err)
	require.Empty(t, trials, "the refused trial_record must not have been stored")
}

// TestLabOpenPreservesLabAffinityWithoutBinding pins the other half of the fix:
// refusing to treat the lab-only entry as a session binding must not cost the
// lab affinity itself. On a connection with no session, trial_record still
// inherits the lab from lab_open once the scope is named explicitly.
func TestLabOpenPreservesLabAffinityWithoutBinding(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	callJSON(t, ctx, cli, "lab_open", map[string]any{"lab": "codex-app-local"})

	tr := callJSON(t, ctx, cli, "trial_record", map[string]any{
		"title": "boot probe", "project": "demo", "outcome": "pass",
	})
	require.Equal(t, "codex-app-local", tr["lab"], "trial_record must inherit the opened lab")

	q := callJSON(t, ctx, cli, "trial_query", map[string]any{})
	require.Equal(t, "codex-app-local", q["lab"], "trial_query must inherit the opened lab")
	require.Len(t, q["trials"].([]any), 1)
}

// TestLabOpenDoesNotShadowAmbientFallback covers the inheriting case: with a
// single active ambient session, an unscoped durable create after a bare
// lab_open must fall through to the ambient project (as it would with no
// lab_open at all), not resolve to the lab-only entry's empty project.
func TestLabOpenDoesNotShadowAmbientFallback(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	id := seedAmbient(t, ctx, db, "cc/lab00000", "demo")

	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "lab_open", map[string]any{"lab": "codex-app-local"})

	w := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "lab-then-ambient", "kind": "reference",
		"description": "must inherit the ambient project", "body": "demo, not global\n",
	})
	require.Equal(t, "demo", w["project"], "the write must inherit the ambient project despite lab_open")

	r := callJSON(t, ctx, cli, "memory_read", map[string]any{"name": "lab-then-ambient", "project": "demo"})
	require.Equal(t, id, r["source_session"], "ambient provenance must survive lab_open")
}

// A trial a conversation records while it holds a grant's content is stamped
// with the grant (plan fenced-sessions P6), in the index, and recall's hit
// shows it.
func TestStampTrial_RecordCarriesTheConversationsStamps(t *testing.T) {
	r := newStampRig(t, true, nil)
	agent, amb := r.agent("cc/stamp-trial", stLegal, stProc)
	g := r.grant(amb, "cc/stamp-trial", stFin, stLegal, false, access.ProcessToken{})
	tr := stampOK(t, agent, "trial_record", map[string]any{"title": "ledger reconciliation trial", "lab": "close",
		"expected": "balances", "actual": "balanced"})
	rows := r.rows(core.ItemKindTrial, tr["id"].(string))
	require.Len(t, rows, 1)
	require.Equal(t, stFin, rows[0].Source)
	require.Equal(t, g.ApprovalID, rows[0].ApprovalID)
	hits := stampOK(t, agent, "recall", map[string]any{"query": "ledger reconciliation trial", "scope": "trials"})["hits"].([]any)
	require.Len(t, hits, 1)
	require.Equal(t, stampField(stFin, core.StampViaGrant, g.ApprovalID), hits[0].(map[string]any)["may_contain"])
}
