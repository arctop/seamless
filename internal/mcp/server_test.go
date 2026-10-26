package mcp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/agentguide"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/gardener"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const testKey = "test-bearer-key"

// newServer builds a full MCP server over a temp store and returns its base URL
// and the backing DB (for tests that seed rows directly).
func newServer(t *testing.T) (string, *sql.DB) {
	return newServerCfg(t, nil)
}

// newServerCfg is newServer with a hook to tune the Config before construction
// (e.g. ToolEventMaxChars), so tests can exercise non-default wiring.
func newServerCfg(t *testing.T, tune func(*mcpserver.Config)) (string, *sql.DB) {
	url, db, _, _ := newServerFiles(t, tune)
	return url, db
}

// newServerFiles is newServerCfg plus the files layer behind the server: the
// data dir and its Manager. Tests that must assert against the markdown file
// (the source of truth for durable knowledge) rather than the index row, or that
// need to seed frontmatter the tool surface cannot express, use this.
func newServerFiles(t *testing.T, tune func(*mcpserver.Config)) (string, *sql.DB, *files.Manager, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	require.NoError(t, store.SetSetting(context.Background(), db,
		store.SettingRepoProjectMap, `{"/work/demo":"demo"}`))

	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })

	ret := retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	rec := events.NewRecorder(db)
	garden := gardener.New(db, mgr, nil, nil, rec, gardener.Config{}, nil)
	cfg := mcpserver.Config{
		DB: db, Files: mgr, Retrieve: ret, Events: rec, Gardener: garden, APIKey: testKey,
		// The shared fixture stands in for an instance with every optional feature
		// ON, so the whole tool surface is exercised: research is off by default,
		// and without this the research tools would vanish from every test that
		// uses this helper. The feature-gate tests construct their own servers
		// (tune can set Features back to the default) precisely so the two
		// concerns stay separable.
		Features: config.Features{Research: true},
	}
	if tune != nil {
		tune(&cfg)
	}
	srv := mcpserver.New(cfg)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, db, mgr, dir
}

// dialClient starts and initializes an MCP client against url with the given key.
func dialClient(t *testing.T, ctx context.Context, url, key string) *mcpclient.Client {
	t.Helper()
	cli, err := mcpclient.NewStreamableHttpClient(url,
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + key}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	require.NoError(t, cli.Start(ctx))

	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test", Version: "0"}
	_, err = cli.Initialize(ctx, initReq)
	require.NoError(t, err)
	return cli
}

func rawMCPRequest(t *testing.T, url, method, key, body string) (int, http.Header, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, string(raw)
}

func TestServer_InitializeIncludesAgentGuidance(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli, err := mcpclient.NewStreamableHttpClient(url,
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + testKey}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	require.NoError(t, cli.Start(ctx))

	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "instructions-test", Version: "0"}
	result, err := cli.Initialize(ctx, initReq)
	require.NoError(t, err)
	require.Equal(t, agentguide.MCPInstructions, result.Instructions)

	listed, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)
	require.Len(t, listed.Tools, mcpserver.ToolCount)
}

// The session_start binding is keyed by Mcp-Session-Id, which the stateless
// 2026-07-28 revision drops. The transport must keep every client on a revision
// that still has sessions: an initialize asking for the stateless one is
// negotiated down and still gets an id, and a stateless request (no handshake)
// is refused with the versions to fall back to.
func TestServer_ServesOnlySessionProtocolVersions(t *testing.T) {
	url, _ := newServer(t)

	code, hdr, body := rawMCPRequest(t, url, http.MethodPost, testKey,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+mcp.ProtocolVersion20260728+
			`","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	require.Equal(t, http.StatusOK, code, body)
	require.NotEmpty(t, hdr.Get("Mcp-Session-Id"))
	var initResp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &initResp))
	require.Equal(t, mcp.LATEST_LEGACY_PROTOCOL_VERSION, initResp.Result.ProtocolVersion)

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":{"`+mcp.MetaKeyProtocolVersion+`":"`+
			mcp.ProtocolVersion20260728+`","`+mcp.MetaKeyClientInfo+`":{"name":"t","version":"0"},"`+
			mcp.MetaKeyClientCapabilities+`":{}}}}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set(mcp.HeaderProtocolVersion, mcp.ProtocolVersion20260728)
	req.Header.Set(mcp.HeaderMethod, string(mcp.MethodToolsList))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
	var refused struct {
		Error struct {
			Data struct {
				Supported []string `json:"supported"`
			} `json:"data"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &refused))
	require.Contains(t, refused.Error.Data.Supported, mcp.LATEST_LEGACY_PROTOCOL_VERSION)
	require.NotContains(t, refused.Error.Data.Supported, mcp.ProtocolVersion20260728)
}

func callJSON(t *testing.T, ctx context.Context, cli *mcpclient.Client, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
	require.NoError(t, err)
	require.False(t, res.IsError, "tool %s errored: %s", name, resultText(t, res))
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &out))
	return out
}

// projectSlugs extracts the slugs from a project_list result.
func projectSlugs(pl map[string]any) []string {
	ps, _ := pl["projects"].([]any)
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if m, ok := p.(map[string]any); ok {
			if slug, ok := m["slug"].(string); ok {
				out = append(out, slug)
			}
		}
	}
	return out
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.NotNil(t, res)
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatalf("no text content in result: %+v", res)
	return ""
}

func TestMCPLoopWithBinding(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	// session_start binds the connection to project "demo" via the cwd map.
	start := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo/sub"})
	require.Equal(t, "demo", start["project"])
	require.NotEmpty(t, start["session_id"])

	// memory_write with NO project inherits "demo" from the binding.
	w := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "chroma-boot-race", "kind": "gotcha",
		"description": "chroma answers health checks before it can serve",
		"body":        "Add a readiness gate.\n",
	})
	require.Equal(t, false, w["updated"])
	memID := w["id"].(string)
	require.NotEmpty(t, memID)

	// memory_read with NO project resolves via the binding and returns the body.
	r := callJSON(t, ctx, cli, "memory_read", map[string]any{"name": "chroma-boot-race"})
	require.Equal(t, "demo", r["project"])
	require.Equal(t, memID, r["id"])
	require.Contains(t, r["body"], "readiness gate")

	// Writing the same name again updates in place: the id is stable.
	w2 := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "chroma-boot-race", "kind": "gotcha",
		"description": "chroma readiness race, revised",
		"body":        "Add a readiness gate and a healthcheck.\n",
	})
	require.Equal(t, true, w2["updated"])
	require.Equal(t, memID, w2["id"], "same name must reuse the same ULID")

	// recall (FTS-only; no embedder) finds it, inheriting the bound project.
	rec := callJSON(t, ctx, cli, "recall", map[string]any{"query": "chroma health check readiness"})
	hits := rec["hits"].([]any)
	require.NotEmpty(t, hits)
	require.Equal(t, "chroma-boot-race", hits[0].(map[string]any)["name"])

	// notes roundtrip.
	nc := callJSON(t, ctx, cli, "notes_create", map[string]any{
		"title": "Boot race writeup", "body": "The fix was a readiness gate.",
	})
	noteID := nc["id"].(string)
	nr := callJSON(t, ctx, cli, "notes_read", map[string]any{"id": noteID})
	require.Contains(t, nr["body"], "readiness gate")

	// projects: session_start auto-registered "demo" from the cwd map, so it
	// already appears in project_list without an explicit project_create.
	pl := callJSON(t, ctx, cli, "project_list", nil)
	require.Contains(t, projectSlugs(pl), "demo")

	// A distinct project_create adds another; both are then listed.
	pc := callJSON(t, ctx, cli, "project_create", map[string]any{"name": "Other Project", "slug": "other"})
	require.Equal(t, "other", pc["slug"])
	pl = callJSON(t, ctx, cli, "project_list", nil)
	require.Subset(t, projectSlugs(pl), []string{"demo", "other"})

	// session_end persists findings.
	end := callJSON(t, ctx, cli, "session_end", map[string]any{"findings": "readiness gate fixes the boot race"})
	require.Equal(t, "completed", end["status"])
}

// TestConventionKindRoundTrips pins the convention kind end-to-end: written via
// memory_write, read back with the kind intact, and reachable through recall's
// index without special-casing.
func TestConventionKindRoundTrips(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})

	w := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "wordmark-markup", "kind": "convention",
		"description": "the wordmark markup must stay in sync across the three files",
		"body":        "index.html, docsgen layout, og-source.\n",
	})
	require.NotEmpty(t, w["id"])

	r := callJSON(t, ctx, cli, "memory_read", map[string]any{"name": "wordmark-markup"})
	require.Equal(t, "convention", r["kind"])

	rec := callJSON(t, ctx, cli, "recall", map[string]any{"query": "wordmark markup sync"})
	hits := rec["hits"].([]any)
	require.NotEmpty(t, hits)
	require.Equal(t, "wordmark-markup", hits[0].(map[string]any)["name"])
}

func TestMemorySupersession(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	start := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	sessID := start["session_id"].(string)

	// Write the original memory, then a replacement that supersedes it.
	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "chroma-boot-race", "kind": "gotcha",
		"description": "chroma readiness boot race",
		"body":        "The old understanding of the readiness race.\n",
	})
	w := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "chroma-readiness-gate", "kind": "gotcha",
		"description": "readiness gate fixes the chroma race",
		"body":        "Add a readiness gate and healthcheck.\n",
		"supersedes":  "chroma-boot-race",
	})
	require.Equal(t, "demo/chroma-boot-race", w["superseded"])
	require.Nil(t, w["supersede_error"])

	// recall no longer surfaces the superseded memory, only its replacement.
	rec := callJSON(t, ctx, cli, "recall", map[string]any{"query": "chroma readiness race gate"})
	for _, h := range rec["hits"].([]any) {
		require.NotEqual(t, "chroma-boot-race", h.(map[string]any)["name"],
			"superseded memory must not appear in recall")
	}

	// memory_read of the superseded memory still returns it, with a warning and
	// provenance (source_session) attached.
	r := callJSON(t, ctx, cli, "memory_read", map[string]any{"name": "chroma-boot-race"})
	require.Contains(t, r["body"], "old understanding")
	require.Contains(t, r["warning"], "superseded by demo/chroma-readiness-gate")
	require.Equal(t, sessID, r["source_session"], "memory_read must carry write-time provenance")

	// The replacement reads back active (no warning).
	r2 := callJSON(t, ctx, cli, "memory_read", map[string]any{"name": "chroma-readiness-gate"})
	require.Nil(t, r2["warning"])
}

func TestTasksReadyQueue(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})

	a := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "build ready queue"})
	aID := a["id"].(string)
	b := callJSON(t, ctx, cli, "tasks_add", map[string]any{
		"title": "wire briefing line", "depends_on": aID,
	})
	bID := b["id"].(string)

	// Only A is ready; B is blocked with A listed as its blocker.
	ready := callJSON(t, ctx, cli, "tasks_ready", nil)
	readyList := ready["ready"].([]any)
	require.Len(t, readyList, 1)
	require.Equal(t, aID, readyList[0].(map[string]any)["id"])
	blocked := ready["blocked"].([]any)
	require.Len(t, blocked, 1)
	require.Equal(t, bID, blocked[0].(map[string]any)["id"])

	// Completing A unblocks B.
	callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": aID, "status": "done"})
	ready = callJSON(t, ctx, cli, "tasks_ready", nil)
	readyList = ready["ready"].([]any)
	require.Len(t, readyList, 1)
	require.Equal(t, bID, readyList[0].(map[string]any)["id"])

	// A cycle is rejected (B already depends on A).
	res, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "tasks_update", Arguments: map[string]any{"id": aID, "add_depends_on": bID},
	}})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "cycle")

	// tasks_list filters by status.
	list := callJSON(t, ctx, cli, "tasks_list", map[string]any{"status": "open"})
	require.Len(t, list["tasks"].([]any), 1) // only B is open

	// tasks_list id=<id> loads exactly that task by its globally-unique id,
	// regardless of status (A is done, not open) and with no scope needed.
	byID := callJSON(t, ctx, cli, "tasks_list", map[string]any{"id": aID})
	got := byID["tasks"].([]any)
	require.Len(t, got, 1)
	require.Equal(t, aID, got[0].(map[string]any)["id"])
	require.Equal(t, "done", got[0].(map[string]any)["status"])

	// An unknown id is a not-found error, not an empty list.
	miss, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "tasks_list", Arguments: map[string]any{"id": "01JZZZZZZZZZZZZZZZZZZZZZZZ"},
	}})
	require.NoError(t, err)
	require.True(t, miss.IsError)
	require.Contains(t, resultText(t, miss), "not found")

	// The next session's briefing surfaces the ready-tasks line.
	brief := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	require.Contains(t, brief["briefing"], "Ready tasks (1):\n- wire briefing line")
}

func TestResearchTrials(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})

	// Opening a fresh lab returns empty history and binds the lab.
	open := callJSON(t, ctx, cli, "lab_open", map[string]any{"lab": "demo-dfu"})
	require.EqualValues(t, 0, open["trial_count"])

	// trial_record inherits the bound lab (no lab arg) and stores metrics.
	callJSON(t, ctx, cli, "trial_record", map[string]any{
		"title": "baseline", "outcome": "fail", "metrics": `{"fw":"2.0.3","hz":497}`,
	})
	callJSON(t, ctx, cli, "trial_record", map[string]any{
		"title": "retry", "outcome": "pass", "metrics": `{"fw":"2.0.4","hz":500}`,
	})

	// Re-opening the lab now shows both trials as context.
	open = callJSON(t, ctx, cli, "lab_open", map[string]any{"lab": "demo-dfu"})
	require.EqualValues(t, 2, open["trial_count"])

	// Native metrics query: only the trial with hz=497 comes back.
	q := callJSON(t, ctx, cli, "trial_query", map[string]any{"metrics_filter": `{"hz":497}`})
	trials := q["trials"].([]any)
	require.Len(t, trials, 1)
	require.Equal(t, "baseline", trials[0].(map[string]any)["title"])

	// Outcome filter.
	q = callJSON(t, ctx, cli, "trial_query", map[string]any{"outcome": "pass"})
	require.Len(t, q["trials"].([]any), 1)

	// A bad metrics argument is a tool error, not a panic.
	res, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "trial_record", Arguments: map[string]any{"title": "x", "metrics": "not json"},
	}})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "JSON object")
}

// seedAmbient inserts an active ambient (cc/*) session in the given project, as
// the session-start hook would, and returns its id. name must be unique.
func seedAmbient(t *testing.T, ctx context.Context, db *sql.DB, name, project string) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive,
		Ambient: true, CreatedAt: now, UpdatedAt: now,
	}))
	return id
}

func TestWriteScopeFallbackToAmbient(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)

	// Seed an active ambient session in "demo" (as the session-start hook would).
	id := seedAmbient(t, ctx, db, "cc/amb00000", "demo")

	// A connection that never calls session_start still attributes its writes to
	// the ambient session's project and provenance.
	cli := dialClient(t, ctx, url, testKey)
	w := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "unbound-write", "kind": "reference",
		"description": "written without session_start",
		"body":        "lands in the ambient project.\n",
	})
	require.Equal(t, "demo", w["project"], "unbound write inherits the ambient project")

	r := callJSON(t, ctx, cli, "memory_read", map[string]any{"name": "unbound-write", "project": "demo"})
	require.Equal(t, id, r["source_session"], "unbound write inherits ambient provenance")
}

func TestSamePrefixAmbientBindingsKeepKnowledgeProvenanceIsolated(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	now := time.Now().UTC()
	seed := func(id, name, project, externalID string) {
		t.Helper()
		require.NoError(t, store.CreateSession(ctx, db, core.Session{
			ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive,
			ExternalSessionID: externalID, ExternalClient: "codex", Ambient: true,
			CreatedAt: now, UpdatedAt: now,
		}))
	}
	idA, err := core.NewID()
	require.NoError(t, err)
	idB, err := core.NewID()
	require.NoError(t, err)
	const (
		nameA = "cx/019f7291-aaaaaaaaaaaaaaaa"
		nameB = "cx/019f7291-bbbbbbbbbbbbbbbb"
	)
	seed(idA, nameA, "alpha", "019f7291-alpha-full")
	seed(idB, nameB, "beta", "019f7291-beta-full")

	write := func(name, memoryName string) map[string]any {
		t.Helper()
		cli := dialClient(t, ctx, url, testKey)
		start := callJSON(t, ctx, cli, "session_start", map[string]any{"name": name})
		require.Equal(t, name, start["name"])
		return callJSON(t, ctx, cli, "memory_write", map[string]any{
			"name": memoryName, "kind": "reference",
			"description": "same-prefix provenance regression",
			"body":        "The bound ambient must remain the source.\n",
		})
	}
	alphaWrite := write(nameA, "alpha-provenance")
	betaWrite := write(nameB, "beta-provenance")
	require.Equal(t, "alpha", alphaWrite["project"])
	require.Equal(t, "beta", betaWrite["project"])

	alpha, found, err := store.MemoryByName(ctx, db, "alpha", "alpha-provenance")
	require.NoError(t, err)
	require.True(t, found)
	beta, found, err := store.MemoryByName(ctx, db, "beta", "beta-provenance")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, idA, alpha.SourceSession)
	require.Equal(t, idB, beta.SourceSession)
	require.NotEqual(t, alpha.SourceSession, beta.SourceSession)
}

// TestConcurrentAmbientDoesNotBleed is the regression test for the cross-agent
// session-bleed bug: when two agents run in different repos, both leaving an
// active ambient session, an unbound durable create must NOT silently attribute
// to whichever ambient session was touched last (machine-global fallback).
// Instead it must refuse and force an explicit project=.
func TestConcurrentAmbientDoesNotBleed(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)

	// Two concurrent agents: an ambient session in "demo" and one in "other".
	seedAmbient(t, ctx, db, "cc/ambdemo0", "demo")
	seedAmbient(t, ctx, db, "cc/ambother", "other")

	cli := dialClient(t, ctx, url, testKey)

	// Every unbound durable create is rejected rather than guessing a project.
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"memory_write", map[string]any{"name": "x", "kind": "reference", "description": "d", "body": "b"}},
		{"notes_create", map[string]any{"title": "a note", "body": "b"}},
		{"tasks_add", map[string]any{"title": "a task"}},
	} {
		isErr, txt := callErr(t, ctx, cli, tc.tool, tc.args)
		require.True(t, isErr, "%s must be rejected under concurrent ambient sessions", tc.tool)
		require.Contains(t, txt, "ambiguous scope", "%s: %s", tc.tool, txt)
		require.Contains(t, txt, "multiple", "%s error should name the multi-project cause: %s", tc.tool, txt)
	}

	// The write nothing bled into "other": passing project= resolves cleanly and
	// lands exactly where asked, never in the sibling agent's project.
	w := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "scoped-write", "kind": "reference", "description": "d",
		"body": "explicitly scoped.\n", "project": "demo",
	})
	require.Equal(t, "demo", w["project"])

	_, foundOther, err := store.MemoryByName(ctx, db, "other", "scoped-write")
	require.NoError(t, err)
	require.False(t, foundOther, "the write must not leak into the sibling agent's project")
	_, foundDemo, err := store.MemoryByName(ctx, db, "demo", "scoped-write")
	require.NoError(t, err)
	require.True(t, foundDemo, "the explicitly scoped write lands in its own project")
}

// TestConcurrentAmbientRejectsReads is the read-side twin of the bleed
// regression: under concurrent agents in different repos, a read or by-name
// lookup must not silently narrow to the global scope (hiding the intended
// project's items). It must refuse, exactly as the durable writes do. The same
// guard covers capture_url and trial_record, which create durable artifacts but
// used to resolve scope through the read path (a latent instance of the bug).
func TestConcurrentAmbientRejectsReads(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)

	seedAmbient(t, ctx, db, "cc/ambdemo0", "demo")
	seedAmbient(t, ctx, db, "cc/ambother", "other")

	cli := dialClient(t, ctx, url, testKey)

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"recall", map[string]any{"query": "anything"}},
		{"memory_read", map[string]any{"name": "x"}},
		{"memory_append", map[string]any{"name": "x", "body": "b"}},
		{"memory_delete", map[string]any{"name": "x"}},
		{"tasks_list", map[string]any{}},
		{"tasks_ready", map[string]any{}},
		{"capture_url", map[string]any{"url": "https://example.com/x"}},
		{"trial_record", map[string]any{"title": "t", "lab": "L", "changes": "c"}},
	} {
		isErr, txt := callErr(t, ctx, cli, tc.tool, tc.args)
		require.True(t, isErr, "%s must refuse under concurrent multi-project ambient sessions", tc.tool)
		require.Contains(t, txt, "ambiguous scope", "%s: %s", tc.tool, txt)
	}
}

// TestReadResolvesGlobalWithoutAmbient verifies the read path is only tightened
// for the *ambiguous* case: with genuinely nothing to infer from (no binding, no
// ambient), a read still legitimately targets the global scope rather than
// erroring -- reading a global memory by name keeps working.
func TestReadResolvesGlobalWithoutAmbient(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)

	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "g", "kind": "reference", "description": "d",
		"body": "a global memory.\n", "project": "global",
	})
	r := callJSON(t, ctx, cli, "memory_read", map[string]any{"name": "g"})
	require.Equal(t, "g", r["name"], "a read with no scope signal resolves to global, not an error")
}

// TestSingleAmbientInfersProjectForReads verifies the single-agent ergonomic is
// preserved for reads: one active ambient project is unambiguous, so an unbound
// read inherits it instead of falling to global.
func TestSingleAmbientInfersProjectForReads(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	seedAmbient(t, ctx, db, "cc/ambonly0", "demo")
	cli := dialClient(t, ctx, url, testKey)

	// Write (inherits demo), then read back with no project -- the read must infer
	// demo too, otherwise it would look in global and miss it.
	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "in-demo", "kind": "reference", "description": "d", "body": "b\n",
	})
	r := callJSON(t, ctx, cli, "memory_read", map[string]any{"name": "in-demo"})
	require.Equal(t, "in-demo", r["name"], "single-project ambient is inferred for reads")
}

// TestSessionTargetAmbiguousSameProject closes the same-project session gap: two
// agents in the SAME repo both leave an active ambient session, so a session_end
// / session_update with no explicit target must refuse rather than complete a
// sibling agent's session. An explicit session_id is the escape hatch.
func TestSessionTargetAmbiguousSameProject(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)

	a := seedAmbient(t, ctx, db, "cc/samea000", "demo")
	b := seedAmbient(t, ctx, db, "cc/sameb000", "demo")

	cli := dialClient(t, ctx, url, testKey)

	for _, tool := range []string{"session_end", "session_update"} {
		isErr, txt := callErr(t, ctx, cli, tool, map[string]any{"findings": "x"})
		require.True(t, isErr, "%s must refuse when two same-project ambients could be the target", tool)
		require.Contains(t, txt, "ambiguous session", "%s: %s", tool, txt)
	}

	// Explicit session_id resolves cleanly and completes only that session.
	end := callJSON(t, ctx, cli, "session_end", map[string]any{"findings": "done", "session_id": a})
	require.Equal(t, a, end["session_id"])
	require.Equal(t, "completed", end["status"])

	other, ok, err := store.SessionByID(ctx, db, b)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionActive, other.Status, "the sibling session must stay active")
}

// TestSessionTargetSoloAmbientResolves verifies the solo ergonomic survives: with
// exactly one active ambient session, session_end with no explicit target still
// completes it.
func TestSessionTargetSoloAmbientResolves(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	solo := seedAmbient(t, ctx, db, "cc/solo0000", "demo")

	cli := dialClient(t, ctx, url, testKey)
	end := callJSON(t, ctx, cli, "session_end", map[string]any{"findings": "done"})
	require.Equal(t, solo, end["session_id"], "a lone ambient is unambiguous and resolves")
	require.Equal(t, "completed", end["status"])
}

// TestSessionTargetBindingWinsOverAmbient verifies a bound agent (called
// session_start) targets its own session even when a sibling ambient exists in
// the same project -- the binding short-circuits the ambient ambiguity guard.
func TestSessionTargetBindingWinsOverAmbient(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)

	cli := dialClient(t, ctx, url, testKey)
	start := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	bound, _ := start["session_id"].(string)
	require.NotEmpty(t, bound)

	sib := seedAmbient(t, ctx, db, "cc/sibling0", "demo")

	end := callJSON(t, ctx, cli, "session_end", map[string]any{"findings": "done"})
	require.Equal(t, bound, end["session_id"], "session_end targets the bound session, not the sibling ambient")

	s, ok, err := store.SessionByID(ctx, db, sib)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionActive, s.Status, "the sibling ambient must stay active")
}

// TestSessionEndTargetsExplicitID verifies session_end/session_update honor an
// explicit session_id -- so a call meant for one session can no longer be
// silently redirected to the ambient fallback and overwrite another agent's.
func TestSessionEndTargetsExplicitID(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)

	// A bystander ambient session in another project that must be left untouched.
	bystander := seedAmbient(t, ctx, db, "cc/bystandr", "other")

	// A real, non-ambient session created out of band (no connection binding).
	target := seedAmbient(t, ctx, db, "sess/target0", "demo")

	cli := dialClient(t, ctx, url, testKey)
	end := callJSON(t, ctx, cli, "session_end", map[string]any{
		"findings": "done", "session_id": target,
	})
	require.Equal(t, target, end["session_id"])
	require.Equal(t, "completed", end["status"])

	// The bystander is still active: the id-targeted end did not touch it.
	bs, ok, err := store.SessionByID(ctx, db, bystander)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionActive, bs.Status, "an id-targeted session_end must not complete a bystander session")
}

func TestMCPAuthRejectsBadKey(t *testing.T) {
	url, _ := newServer(t)
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	status, header, body := rawMCPRequest(t, url, http.MethodPost, "wrong-key", initialize)
	require.Equal(t, http.StatusUnauthorized, status)
	require.Equal(t, "Bearer", header.Get("WWW-Authenticate"))
	require.Contains(t, body, "unauthorized")
}

// seedAmbientCWD inserts an active ambient (cc/*) session with a Claude session id
// and cwd, as the session-start hook would, and returns its id.
func seedAmbientCWD(t *testing.T, ctx context.Context, db *sql.DB, name, claudeID, cwd string, updated time.Time) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: id, Name: name, ProjectSlug: "demo", Status: core.SessionActive,
		Ambient: true, ExternalSessionID: claudeID, ExternalClient: "claude-code", CWD: cwd,
		CreatedAt: updated, UpdatedAt: updated,
	}))
	return id
}

// TestSessionStart_LinksClaudeSessionID checks that session_start stamps a new
// NAMED explicit session with the Claude session id of the sole active ambient
// sharing its cwd, so a graceful SessionEnd closes it too. (An unnamed start adopts
// the ambient instead -- see TestSessionStart_AdoptsAmbient.) Two same-cwd ambients
// make the link ambiguous, so it is skipped and the idle reaper handles that
// session instead.
func TestSessionStart_LinksClaudeSessionID(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	now := time.Now().UTC()

	// Sole same-cwd ambient -> the new named explicit session links to it.
	seedAmbientCWD(t, ctx, db, "cc/claude99", "claude99-full", "/work/demo", now)
	cli := dialClient(t, ctx, url, testKey)
	start := callJSON(t, ctx, cli, "session_start", map[string]any{
		"name": "agent-a", "cwd": "/work/demo",
	})
	expl, ok, err := store.SessionByID(ctx, db, start["session_id"].(string))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "agent-a", expl.Name, "a named start creates its own session")
	require.Equal(t, "claude99-full", expl.ExternalSessionID, "linked to the sole same-cwd ambient")
	require.Equal(t, "claude-code", expl.ExternalClient, "the linked client disambiguates its external id")

	// A second same-cwd ambient makes the link ambiguous -> no link.
	seedAmbientCWD(t, ctx, db, "cc/claudeaa", "claudeaa-full", "/work/demo", now)
	cli2 := dialClient(t, ctx, url, testKey)
	start2 := callJSON(t, ctx, cli2, "session_start", map[string]any{
		"name": "agent-b", "cwd": "/work/demo",
	})
	expl2, _, err := store.SessionByID(ctx, db, start2["session_id"].(string))
	require.NoError(t, err)
	require.Empty(t, expl2.ExternalSessionID, "ambiguous same-cwd ambients -> no link")
	require.Empty(t, expl2.ExternalClient, "ambiguous same-cwd ambients -> no client link")
}

// TestSessionStart_AdoptsAmbient checks that an unnamed session_start with exactly
// one active ambient (cc/*) session sharing its cwd resumes that session -- binding
// the connection to it, bumping its recency, returning its id/name -- instead of
// creating a second sess/* row (the double-session).
func TestSessionStart_AdoptsAmbient(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	stale := time.Now().UTC().Add(-time.Hour)

	amb := seedAmbientCWD(t, ctx, db, "cc/claude99", "claude99-full", "/work/demo", stale)

	cli := dialClient(t, ctx, url, testKey)
	start := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	require.Equal(t, amb, start["session_id"], "adopted the ambient, no new session")
	require.Equal(t, "cc/claude99", start["name"], "the adopted session keeps its ambient name")
	require.Equal(t, true, start["resumed"])

	all, err := store.ListSessions(ctx, db, "", time.Time{}, 0)
	require.NoError(t, err)
	require.Len(t, all, 1, "adoption must not create a second session row")

	sess, ok, err := store.SessionByID(ctx, db, amb)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionActive, sess.Status)
	require.True(t, sess.UpdatedAt.After(stale), "adoption bumps recency for the idle reaper")

	// The connection is bound to the adopted session: session_end targets it.
	end := callJSON(t, ctx, cli, "session_end", map[string]any{"findings": "done"})
	require.Equal(t, amb, end["session_id"], "the binding targets the adopted ambient")
	require.Equal(t, "completed", end["status"])
}

// TestSessionStart_NoAmbientCreatesFresh pins the fallback: with no same-cwd
// ambient (the hook never ran, or another cwd), an unnamed session_start still
// creates a fresh sess/* session.
func TestSessionStart_NoAmbientCreatesFresh(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	seedAmbientCWD(t, ctx, db, "cc/elsewher", "elsewhere-full", "/work/other", time.Now().UTC())

	cli := dialClient(t, ctx, url, testKey)
	start := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	name, _ := start["name"].(string)
	require.True(t, strings.HasPrefix(name, "sess/"), "no same-cwd ambient -> fresh sess/* session, got %q", name)

	sess, ok, err := store.SessionByID(ctx, db, start["session_id"].(string))
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, sess.Ambient)
	require.Empty(t, sess.ExternalSessionID, "different-cwd ambient must not link")
	require.Empty(t, sess.ExternalClient, "different-cwd ambient must not link a client")
}
