package mcp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// missEvents reads the recall.miss rows straight off the event log.
func missEvents(t *testing.T, db *sql.DB) []map[string]any {
	t.Helper()
	rows, err := db.Query(`SELECT project_slug, session_id, payload FROM events WHERE kind = ?`,
		string(core.EventRecallMiss))
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var project, session, payload string
		require.NoError(t, rows.Scan(&project, &session, &payload))
		var p map[string]any
		require.NoError(t, json.Unmarshal([]byte(payload), &p))
		p["_project"] = project
		p["_session"] = session
		out = append(out, p)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestRecall_ZeroHitRecordsMiss(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	start := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	sessID := start["session_id"].(string)

	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "chroma-boot-race", "kind": "gotcha",
		"description": "chroma readiness boot race",
		"body":        "Add a readiness gate.\n",
	})

	// A query nothing matches: the zero-hit recall must leave a miss event
	// carrying the query and the session/project attribution.
	rec := callJSON(t, ctx, cli, "recall", map[string]any{"query": "quantum flux capacitor sizing", "limit": 3})
	require.Empty(t, rec["hits"])

	misses := missEvents(t, db)
	require.Len(t, misses, 1)
	require.Equal(t, "quantum flux capacitor sizing", misses[0]["query"])
	require.Equal(t, "recall", misses[0]["source"])
	require.Equal(t, float64(3), misses[0]["limit"])
	require.Equal(t, "demo", misses[0]["_project"])
	require.Equal(t, sessID, misses[0]["_session"])

	// A hit records only retrieval.injected -- no second miss row appears.
	rec = callJSON(t, ctx, cli, "recall", map[string]any{"query": "chroma readiness boot race"})
	require.NotEmpty(t, rec["hits"])
	require.Len(t, missEvents(t, db), 1)
}

// A kind-filtered recall keeps both filter contracts: a zero-hit call is still
// demand (the miss records, kind riding along), and the notes scope is a
// contradiction reported as an error, not an empty result.
func TestRecall_KindFilterMissAndScopeContradiction(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "boot-order", "kind": "gotcha",
		"description": "boot order matters",
		"body":        "b\n",
	})

	// The gotcha matches the query, but the convention filter excludes it: a
	// kind-filtered zero-hit is still a recorded miss (memory-wanted demand).
	rec := callJSON(t, ctx, cli, "recall", map[string]any{"query": "boot order", "kind": "convention"})
	require.Empty(t, rec["hits"])
	misses := missEvents(t, db)
	require.Len(t, misses, 1)
	require.Equal(t, "boot order", misses[0]["query"])
	require.Equal(t, "convention", misses[0]["kind"])

	// kind implies memories-only; combined with scope=notes it errors loudly.
	res, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name:      "recall",
		Arguments: map[string]any{"query": "boot order", "scope": "notes", "kind": "convention"},
	}})
	require.NoError(t, err)
	require.True(t, res.IsError)
	// The contradiction is not a miss: no second miss row appears.
	require.Len(t, missEvents(t, db), 1)
}

// injectedEvents reads the retrieval.injected rows straight off the event log.
func injectedEvents(t *testing.T, db *sql.DB) []map[string]any {
	t.Helper()
	rows, err := db.Query(`SELECT payload FROM events WHERE kind = ?`,
		string(core.EventInjected))
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var payload string
		require.NoError(t, rows.Scan(&payload))
		var p map[string]any
		require.NoError(t, json.Unmarshal([]byte(payload), &p))
		out = append(out, p)
	}
	require.NoError(t, rows.Err())
	return out
}

// A kind browse (no query) is a listing, not demand: its hits record as
// retrieval.injected under source "recall-browse" -- which the utility scorer
// classifies as passive exposure, weight 0 (closed-loop contract) -- and an
// empty browse records nothing at all: no miss row, since there is no query
// text for the memory-wanted pass to cluster and an empty kind is not a
// missing memory.
func TestRecall_BrowseRecordsExposureNotDemand(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "layout-fact", "kind": "convention",
		"description": "where things deploy",
		"body":        "b\n",
	})

	// An empty browse: no hits, no miss event, no injected event.
	rec := callJSON(t, ctx, cli, "recall", map[string]any{"kind": "stage"})
	require.Empty(t, rec["hits"])
	require.Empty(t, missEvents(t, db))

	// A browse with hits records injected under the browse source, never the
	// query-gated "recall" source.
	rec = callJSON(t, ctx, cli, "recall", map[string]any{"kind": "convention"})
	require.Len(t, rec["hits"], 1)
	var browse []map[string]any
	for _, e := range injectedEvents(t, db) {
		if e["source"] == "recall-browse" {
			browse = append(browse, e)
		}
	}
	require.Len(t, browse, 1)
	require.Equal(t, "", browse[0]["query"])
	require.Equal(t, []any{"layout-fact"}, func() []any {
		mems := callJSON(t, ctx, cli, "recall", map[string]any{"kind": "convention"})
		names := make([]any, 0)
		for _, h := range mems["hits"].([]any) {
			names = append(names, h.(map[string]any)["name"])
		}
		return names
	}())

	// Neither query nor kind is a loud error, not an empty result.
	res, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "recall", Arguments: map[string]any{},
	}})
	require.NoError(t, err)
	require.True(t, res.IsError)
}

// Every recall hit names its item type under "kind" (note, task, ...), so an
// agent sends kind=note to search notes. recall reads that as scope=notes instead
// of refusing it as an unknown memory kind (the gardener's recurring tool error
// 01M7X4PQXCZ0ZEKDARZJ9ZJ76V), and refuses it only where a scope would be a
// guess: against a contradicting scope, or with no query to search for.
func TestRecall_HitKindReadsAsItsScope(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "ledger-rule", "kind": "gotcha",
		"description": "ledger reconciliation order", "body": "ledger reconciliation\n",
	})
	callJSON(t, ctx, cli, "notes_create", map[string]any{"title": "ledger reconciliation design", "body": "ledger reconciliation\n"})
	callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "ledger reconciliation fix", "body": "ledger reconciliation\n"})

	kindsOf := func(rec map[string]any) []string {
		var out []string
		for _, h := range rec["hits"].([]any) {
			out = append(out, h.(map[string]any)["kind"].(string))
		}
		return out
	}
	all := kindsOf(callJSON(t, ctx, cli, "recall", map[string]any{"query": "ledger reconciliation"}))
	require.ElementsMatch(t, []string{"memory", "note", "task"}, all, "the unfiltered recall sees all three")

	for _, kind := range []string{"note", "task", "memory"} {
		got := kindsOf(callJSON(t, ctx, cli, "recall", map[string]any{"query": "ledger reconciliation", "kind": kind}))
		require.Equal(t, []string{kind}, got, "kind=%s searches only %s hits", kind, kind)
	}

	// A miss under the rewrite records the scope it searched, not a kind.
	before := len(missEvents(t, db))
	require.Empty(t, callJSON(t, ctx, cli, "recall", map[string]any{"query": "nothing like this", "kind": "session"})["hits"])
	misses := missEvents(t, db)
	require.Len(t, misses, before+1)
	require.Equal(t, "sessions", misses[len(misses)-1]["scope"])
	require.NotContains(t, misses[len(misses)-1], "kind")

	isErr, txt := callErr(t, ctx, cli, "recall", map[string]any{"query": "ledger", "kind": "note", "scope": "tasks"})
	require.True(t, isErr)
	require.Contains(t, txt, `kind "note" selects scope notes, but scope is tasks`)

	isErr, txt = callErr(t, ctx, cli, "recall", map[string]any{"kind": "note"})
	require.True(t, isErr)
	require.Contains(t, txt, "pass scope=notes with a query")

	// A genuinely unknown kind is still refused with the memory kinds named.
	isErr, txt = callErr(t, ctx, cli, "recall", map[string]any{"query": "ledger", "kind": "notez"})
	require.True(t, isErr)
	require.Contains(t, txt, "valid values are constraint, convention")
}

// A kind listing says when it is not the whole kind, and how to get the rest.
func TestRecall_KindListingCountsTheRest(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	for i := range 3 {
		callJSON(t, ctx, cli, "memory_write", map[string]any{
			"name": fmt.Sprintf("rule-%d", i), "kind": "constraint",
			"description": fmt.Sprintf("rule number %d", i), "body": "a rule\n",
		})
	}
	rec := callJSON(t, ctx, cli, "recall", map[string]any{"kind": "constraint", "limit": 2})
	require.Len(t, rec["hits"], 2)
	require.EqualValues(t, 1, rec["more"])
	require.Equal(t, "1 more constraint memories match past limit=2; pass limit=3 to list all of them", rec["note"])

	rec = callJSON(t, ctx, cli, "recall", map[string]any{"kind": "constraint", "limit": 3})
	require.Len(t, rec["hits"], 3)
	require.NotContains(t, rec, "more", "a whole listing carries no more")
	require.NotContains(t, rec, "note")

}
