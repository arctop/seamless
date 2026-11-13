//go:build defaultsgolden

// Step 10's defaults row (plan seamless-sealed-unlock): with peer_auth off and
// isolation.requests off -- the defaults -- every agent surface answers exactly
// as it did on main before the plan. The golden files under testdata/defaults/
// were written by THIS test running against `git archive main`; the branch
// must reproduce them after normalization, less the few differences the owner
// accepted as changes for every install (dgAccepted).
//
// The file compiles against main's API as well as the branch's: it names only
// mcp.Config, hooks.Config and console.Config fields both sides have, and only
// store functions main already exports. Every top-level identifier carries the
// dg prefix (package main is shared with the other matrix files).
//
// It runs only under the defaultsgolden build tag (make defaults-golden): it
// is the plan's merge gate against main, not a standing snapshot. Every
// legitimate change to a tool description, the briefing or a console page
// moves it, so make check does not run it; run it before locked-projects
// merges, and regenerate it from main whenever main itself moves.
//
// Regenerate (on a main extract, never on the branch):
//
//	go test -tags defaultsgolden ./cmd/seamlessd -run TestDefaults -count=1 -dg.update
//	DG_UPDATE=1 go test -tags defaultsgolden ./cmd/seamlessd -run TestDefaults -count=1
//
// DG_OUT=<dir> also writes the actual (normalized) files there, for an
// external diff against testdata/defaults/.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/a2a"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/hooks"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

var dgUpdateFlag = flag.Bool("dg.update", false, "rewrite the defaults golden files under testdata/defaults (run on a main extract only)")

// dgUpdating reports whether this run writes the golden files instead of
// comparing against them.
func dgUpdating() bool { return *dgUpdateFlag || os.Getenv("DG_UPDATE") == "1" }

const (
	dgKey     = "dg-bearer-key-0123456789abcdef"
	dgHost    = "dg-host.example"
	dgVersion = "0.0.0-dg"

	dgOpenA  = "example-open-alpha"
	dgOpenB  = "example-open-beta"
	dgOpenC  = "example-open-gamma"
	dgConf   = "example-confidential"
	dgSealed = "example-sealed"

	dgRepoA    = "/work/example-open-alpha"
	dgRepoB    = "/work/example-open-beta"
	dgRepoConf = "/work/example-confidential"

	// dgProcA is a well-formed agent process identity (agentproc.Valid): the
	// hook stamps it on the ambient session and one MCP connection names it,
	// so the process binding is part of the script.
	dgProcA = "4242.1700000000"

	dgGoldenDir = "testdata/defaults"
)

// dgPlanTables are the tables plan seamless-sealed-unlock added (migrations
// 031-034 and 036). They do not exist on main; on the branch, with the
// defaults, they must hold no row after the whole script.
var dgPlanTables = []string{
	"access_approvals", "access_leases", "approver_credentials",
	"approver_enroll_codes", "console_unlocks", "access_grants",
	"mcp_sessions",
}

// dgForbiddenKeys are the response keys SEAL A (restricted accounts, commit
// 4989ebee) added; none may appear in any response with the defaults.
// project_memory and global_memory are memory_read's pointers between a
// restricted account's project memory and the global rule of its name, so
// they need a memory carrying an account, which the defaults never write.
var dgForbiddenKeys = []string{"account", "updated_account", "proposed_by", "project_memory", "global_memory"}

// ---------------------------------------------------------------------------
// The daemon
// ---------------------------------------------------------------------------

// dgElicitLog is the client half of elicitation: every connection declares
// the capability (a handler is set) and records any request it is shown. With
// the defaults the server must never send one.
type dgElicitLog struct {
	mu   sync.Mutex
	reqs []mcpgo.ElicitationRequest
}

func (l *dgElicitLog) Elicit(_ context.Context, req mcpgo.ElicitationRequest) (*mcpgo.ElicitationResult, error) {
	l.mu.Lock()
	l.reqs = append(l.reqs, req)
	l.mu.Unlock()
	return &mcpgo.ElicitationResult{ElicitationResponse: mcpgo.ElicitationResponse{Action: mcpgo.ElicitationResponseActionDecline}}, nil
}

func (l *dgElicitLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.reqs)
}

// dgNoteLog records the method of every server notification any connection
// receives.
type dgNoteLog struct {
	mu      sync.Mutex
	methods []string
}

func (l *dgNoteLog) add(m string) {
	l.mu.Lock()
	l.methods = append(l.methods, m)
	l.mu.Unlock()
}

func (l *dgNoteLog) sorted() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := slices.Clone(l.methods)
	sort.Strings(out)
	return out
}

// dgDaemon is a default-config daemon wired the way runServe wires one with
// peer_auth off and isolation.requests off: no ownerHandler, no socket, no
// approval or grant service.
type dgDaemon struct {
	db     *sql.DB
	dir    string
	home   string
	base   string
	elicit *dgElicitLog
	notes  *dgNoteLog

	rawMu sync.Mutex
	raw   []string // every response text, before normalization (the SEAL A scan reads it)
	prime []string // the same texts as the golden files carry them, for placeholder numbering
}

func dgNewDaemon(t *testing.T) *dgDaemon {
	t.Helper()
	ctx := context.Background()
	// A throwaway home: nothing below may read or write the owner's.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := t.TempDir()

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.MCP.APIKey = dgKey
	// The research feature is on so the trial tools are part of the script;
	// every other optional feature keeps its default (off), write screening
	// included.
	feats := cfg.Features
	feats.Research = true
	cfg.Features = feats
	require.False(t, cfg.PeerAuth.Enabled, "peer_auth is off by default")
	require.False(t, cfg.Features.Screening, "write screening is off by default")

	dbPath := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(ctx, db, dgHost))
	for _, slug := range []string{dgOpenA, dgOpenB, dgOpenC, dgConf, dgSealed} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, dgConf, core.IsolationConfidential))
	require.NoError(t, store.SetProjectIsolation(ctx, db, dgSealed, core.IsolationSealed))
	require.NoError(t, store.AddRepoMapping(ctx, db, dgRepoA, dgOpenA))
	require.NoError(t, store.AddRepoMapping(ctx, db, dgRepoB, dgOpenB))
	require.NoError(t, store.AddRepoMapping(ctx, db, dgRepoConf, dgConf))
	require.NoError(t, store.AdoptLocalHost(ctx, db, dgHost))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The files manager is not Started: the watcher's 300ms debounce re-index
	// would race the script. Every write in the script goes through the
	// Manager, which indexes synchronously.
	mgr, err := files.NewManager(dir, db, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })

	ret := retrieve.New(db, nil, cfg.Budgets, logger)
	ret.SetBodyReader(mgr.Store())
	ret.SetBriefingConfig(cfg.Briefing)
	ret.SetSearchConfig(cfg.Search)
	ret.SetFeaturesConfig(cfg.Features)
	rec := events.NewRecorder(db)
	rec.SetFeatures(cfg.Features)
	garden := gardener.New(db, mgr, nil, nil, rec, gardener.FromConfig(cfg.Gardener), logger)
	screeningOn := func(ctx context.Context) bool {
		fc, _, ferr := store.FeaturesConfig(ctx, db, cfg.Features)
		if ferr != nil {
			fc = cfg.Features
		}
		return features.Enabled(fc, features.Screening)
	}
	screen := screening.New(screening.Config{
		DB: db, Files: mgr, Events: rec, Gardener: garden, Logger: logger,
		Rules:   screening.Rules{URLAllowlist: cfg.Screening.URLAllowlist, BlobMinLength: cfg.Screening.BlobMinLength},
		Enabled: screeningOn, LockdownFile: cfg.Screening.LockdownFile,
		DriftInterval: time.Duration(cfg.Screening.DriftInterval),
	})
	ret.SetLockdown(screen.LockdownOn)
	// screen.Run (the corpus watch) and the gardener's ticker are not started:
	// both are inert or off by default, and a background pass would race the
	// row counts.

	mcpSrv := mcp.New(mcp.Config{
		DB: db, Files: mgr, Retrieve: ret, Events: rec, Gardener: garden, Screening: screen,
		APIKey: dgKey, Version: dgVersion, LocalHost: dgHost,
		ToolEventMaxChars:   cfg.Budgets.ToolEventMaxChars,
		CaptureAllowedPorts: cfg.Capture.AllowedPorts, Logger: logger,
		RecordCommits: cfg.Commits.Record,
		Features:      cfg.Features,
	})
	a2aSrv, err := a2a.New(a2a.Config{Retrieve: ret, Events: rec, APIKey: dgKey, Version: dgVersion,
		Endpoint: "http://127.0.0.1:8081/api/a2a", Logger: logger})
	require.NoError(t, err)
	hooksH := hooks.NewHandler(hooks.Config{
		DB: db, Retrieve: ret, Events: rec, Files: mgr,
		APIKey: dgKey, MaxEventChars: cfg.Budgets.ToolEventMaxChars,
		PlanCapture: cfg.PlanCapture, PlansDir: filepath.Join(home, ".claude", "plans"),
		LocalHost: dgHost, Logger: logger,
		Features: cfg.Features, Screening: screen,
	})
	consoleSrv, err := console.New(console.Config{
		DB: db, Files: mgr, Gardener: garden, Events: rec, Retrieve: ret,
		Screening: screen,
		APIKey:    dgKey, DataDir: dir, DBPath: dbPath,
		Budgets: cfg.Budgets, GardenerCfg: cfg.Gardener, BriefingCfg: cfg.Briefing,
		Features: cfg.Features, Level: cfg.Console.Level, Version: dgVersion,
		SessionIdleTTL: time.Duration(cfg.Gardener.SessionIdleMinutes) * time.Minute,
		Logger:         logger,
	})
	require.NoError(t, err)
	mux := daemonRoutes{
		healthz:   healthzHandler(db, "dg-instance", nil),
		mcp:       mcpSrv.Handler(),
		a2a:       a2aSrv.Handler(),
		agentCard: a2aSrv.CardHandler(),
		hooks:     hooksH,
		console:   consoleSrv,
	}.mux()
	// runServe with peer_auth off: refusalWatch around the routes, the Host
	// guard and the activity tracker outside it.
	routes := refusalWatch(screen, mux)
	srv := httptest.NewServer(daemonHandler("127.0.0.1:8081", cfg.AllowedHostsEffective(), newActivityTracker(time.Now), routes))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return &dgDaemon{db: db, dir: dir, home: home, base: srv.URL, elicit: &dgElicitLog{}, notes: &dgNoteLog{}}
}

// dgConn is one MCP connection and what its initialize returned.
type dgConn struct {
	cli  *mcpclient.Client
	init *mcpgo.InitializeResult
}

// dial opens an MCP connection over the bearer key, declaring elicitation.
func (d *dgDaemon) dial(t *testing.T, label string, extra map[string]string) dgConn {
	t.Helper()
	headers := map[string]string{"Authorization": "Bearer " + dgKey, mcp.HostHeader: dgHost}
	for k, v := range extra {
		headers[k] = v
	}
	trans, err := transport.NewStreamableHTTP(d.base+"/api/mcp", transport.WithHTTPHeaders(headers))
	require.NoError(t, err)
	cli := mcpclient.NewClient(trans, mcpclient.WithElicitationHandler(d.elicit))
	t.Cleanup(func() { _ = cli.Close() })
	cli.OnNotification(func(n mcpgo.JSONRPCNotification) { d.notes.add(n.Method) })
	ctx := context.Background()
	require.NoError(t, cli.Start(ctx))
	var initReq mcpgo.InitializeRequest
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "dg-" + label, Version: "0"}
	res, err := cli.Initialize(ctx, initReq)
	require.NoError(t, err)
	return dgConn{cli: cli, init: res}
}

func (d *dgDaemon) keepRaw(s string) { d.keep(s, s) }

// keep records raw for the scan and kept -- the part a golden file carries --
// for the placeholder numbering.
func (d *dgDaemon) keep(raw, kept string) {
	d.rawMu.Lock()
	d.raw = append(d.raw, raw)
	d.prime = append(d.prime, kept)
	d.rawMu.Unlock()
}

// ---------------------------------------------------------------------------
// The transcript
// ---------------------------------------------------------------------------

// dgStep is one entry of the transcript: what was asked and what came back.
type dgStep struct {
	Step    string `json:"step"`
	Surface string `json:"surface"`
	Request any    `json:"request,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	Status  int    `json:"status,omitempty"`
	Result  any    `json:"result"`
}

// dgScript runs the scripted sequence and collects the transcript.
type dgScript struct {
	t     *testing.T
	d     *dgDaemon
	steps []dgStep
	n     int
}

func (s *dgScript) name(label string) string {
	s.n++
	return fmt.Sprintf("%03d-%s", s.n, label)
}

// dgDecode keeps a JSON text as its decoded value, so a golden diff points at
// the field that changed; any other text stays a string.
func dgDecode(text string) any {
	var v any
	if json.Unmarshal([]byte(text), &v) == nil {
		switch v.(type) {
		case map[string]any, []any:
			return v
		}
	}
	return text
}

// call runs one tool and records it. It returns the raw text and, when the
// text is a JSON object, the decoded object.
func (s *dgScript) call(c dgConn, label, tool string, args map[string]any) (string, map[string]any, bool) {
	s.t.Helper()
	res, err := c.cli.CallTool(context.Background(), mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: tool, Arguments: args}})
	require.NoError(s.t, err, "%s: transport error", tool)
	return s.record(label, tool, args, res)
}

// record appends one tool result to the transcript.
func (s *dgScript) record(label, tool string, args map[string]any, res *mcpgo.CallToolResult) (string, map[string]any, bool) {
	s.t.Helper()
	var b strings.Builder
	var items []any
	for _, ct := range res.Content {
		if tc, ok := ct.(mcpgo.TextContent); ok {
			b.WriteString(tc.Text)
			s.d.keepRaw(tc.Text)
			items = append(items, map[string]any{"text": dgDecode(tc.Text)})
			continue
		}
		raw, merr := json.Marshal(ct)
		require.NoError(s.t, merr)
		s.d.keepRaw(string(raw))
		items = append(items, map[string]any{"content": dgDecode(string(raw))})
	}
	if res.StructuredContent != nil {
		raw, merr := json.Marshal(res.StructuredContent)
		require.NoError(s.t, merr)
		s.d.keepRaw(string(raw))
		items = append(items, map[string]any{"structured": dgDecode(string(raw))})
	}
	text := b.String()
	s.steps = append(s.steps, dgStep{Step: s.name(label), Surface: "mcp " + tool, Request: args, IsError: res.IsError, Result: items})
	var obj map[string]any
	if json.Unmarshal([]byte(text), &obj) != nil {
		obj = nil
	}
	return text, obj, res.IsError
}

// rpcRefused calls a tool the server does not register: mcp-go answers a
// JSON-RPC error rather than a tool result, and the error text is the surface.
func (s *dgScript) rpcRefused(c dgConn, label, tool string, args map[string]any) {
	s.t.Helper()
	res, err := c.cli.CallTool(context.Background(), mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: tool, Arguments: args}})
	if err == nil {
		// Not fatal: the script goes on, and the goldens show what changed.
		s.t.Errorf("%s (%s) was expected to be an unknown tool, and a handler answered", tool, label)
		s.record(label, tool, args, res)
		return
	}
	s.d.keepRaw(err.Error())
	s.steps = append(s.steps, dgStep{Step: s.name(label), Surface: "mcp " + tool, Request: args, IsError: true,
		Result: map[string]any{"rpc_error": err.Error()}})
}

// http records one plain HTTP exchange (A2A, healthz, the agent card).
func (s *dgScript) http(label, method, path, body string) string {
	s.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, s.d.base+path, rd)
	require.NoError(s.t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+dgKey)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(s.t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)
	s.d.keepRaw(string(out))
	request := map[string]any{"method": method, "path": path}
	if body != "" {
		request["body"] = dgDecode(body)
	}
	s.steps = append(s.steps, dgStep{Step: s.name(label), Surface: "http " + path, Request: request, Status: resp.StatusCode, Result: dgDecode(string(out))})
	return string(out)
}

// ok is call with the requirement that the tool succeeded.
func (s *dgScript) ok(c dgConn, label, tool string, args map[string]any) map[string]any {
	s.t.Helper()
	text, obj, isErr := s.call(c, label, tool, args)
	require.False(s.t, isErr, "%s (%s) failed: %s", tool, label, text)
	return obj
}

// refused is call with the expectation that the tool answered an error. It
// is not fatal (no later step depends on a refusal), so a refusal that turned
// into a success still lets the goldens show everything else that changed.
func (s *dgScript) refused(c dgConn, label, tool string, args map[string]any) string {
	s.t.Helper()
	text, _, isErr := s.call(c, label, tool, args)
	if !isErr {
		s.t.Errorf("%s (%s) was expected to be refused: %s", tool, label, text)
	}
	return text
}

// hook posts one hook event and records the response.
func (s *dgScript) hook(label, event string, query url.Values, body map[string]any) string {
	s.t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(s.t, err)
	target := s.d.base + "/api/hooks/" + event
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(string(raw)))
	require.NoError(s.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+dgKey)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(s.t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(s.t, err)
	s.d.keepRaw(string(out))
	request := map[string]any{"body": body}
	if len(query) > 0 {
		request["query"] = query
	}
	s.steps = append(s.steps, dgStep{Step: s.name(label), Surface: "hook " + event, Request: request, Status: resp.StatusCode, Result: dgDecode(string(out))})
	return string(out)
}

func dgStr(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	require.True(t, ok, "result has no string %q: %v", key, m)
	return v
}

// dgRunScript is the scripted sequence: hooks, a broad set of tool calls with
// their full results, and the isolation refusals that exist on main.
// Dependencies join OPEN projects only: the plan withholds a blocker's title
// across a lock or a confidential fence on purpose, and this row is not about
// that change.
func dgRunScript(t *testing.T, s *dgScript) {
	d := s.d
	procQuery := url.Values{"agent_process": {dgProcA}}

	// ---- the agent's first SessionStart: an empty corpus -----------------
	s.hook("hook-session-start-empty", "session-start", procQuery, map[string]any{
		"session_id": "dg-claude-0001", "cwd": dgRepoA, "source": "startup",
	})

	// ---- a connection bound to the open project --------------------------
	a := d.dial(t, "alpha", nil)
	sessA := s.ok(a, "session-start-alpha", "session_start", map[string]any{"project": dgOpenA, "model": "example-model-1"})
	sessAName := dgStr(t, sessA, "name")

	constraint := s.ok(a, "memory-write-constraint", "memory_write", map[string]any{
		"name": "example-deploy-gate", "kind": "constraint",
		"description": "Every example deploy goes through the staging gate first",
		"body":        "Never deploy the example service straight to production; stage it first.",
		"tags":        []any{"deploy", "example"},
	})
	s.ok(a, "memory-read-by-id", "memory_read", map[string]any{"id": dgStr(t, constraint, "id")})
	s.ok(a, "memory-write-convention", "memory_write", map[string]any{
		"name": "example-layout-convention", "kind": "convention",
		"description": "Example services keep their configs under deploy/example",
		"body":        "Configs for the example services live in deploy/example, one file per service.",
	})
	s.ok(a, "memory-write-stage", "memory_write", map[string]any{
		"name": "example-migration-stage", "kind": "stage",
		"description": "The example data migration waits on the owner's sign-off",
		"body":        "Status: blocked\nGate: human\n\nThe example data migration is staged and waits on the owner's sign-off.",
	})
	s.ok(a, "memory-write-gotcha", "memory_write", map[string]any{
		"name": "example-cache-gotcha", "kind": "gotcha",
		"description": "The example cache keeps stale entries for ten minutes after a deploy",
		"body":        "After a deploy the example cache serves stale entries for ten minutes; flush it by hand.",
	})
	s.ok(a, "memory-write-global-reference", "memory_write", map[string]any{
		"name": "example-style-reference", "kind": "reference", "project": "global",
		"description": "Where the shared example style guide lives",
		"body":        "The shared example style guide is in docs/example-style.md.",
	})
	s.ok(a, "memory-read-constraint", "memory_read", map[string]any{"name": "example-deploy-gate"})
	s.ok(a, "memory-edit-stage", "memory_edit", map[string]any{
		"name":     "example-migration-stage",
		"edits":    []any{map[string]any{"old_string": "Status: blocked", "new_string": "Status: in_progress"}},
		"tags_add": []any{"migration"},
	})
	s.ok(a, "memory-append-gotcha", "memory_append", map[string]any{
		"name": "example-cache-gotcha", "body": "The flush command is example-cache flush --all.",
	})
	s.ok(a, "memory-write-supersede", "memory_write", map[string]any{
		"name": "example-cache-gotcha-v2", "kind": "gotcha",
		"description": "The example cache keeps stale entries for five minutes after a deploy",
		"body":        "After a deploy the example cache serves stale entries for five minutes.",
		"supersedes":  "example-cache-gotcha",
	})
	s.ok(a, "memory-read-superseded", "memory_read", map[string]any{"name": "example-cache-gotcha"})
	s.ok(a, "memory-write-duplicate-name", "memory_write", map[string]any{
		"name": "example-layout-convention", "kind": "convention",
		"description": "Example services keep their configs under deploy/example",
		"body":        "Configs for the example services live in deploy/example, one file per service, named after it.",
	})

	note := s.ok(a, "notes-create", "notes_create", map[string]any{
		"title": "Example rollout narrative", "description": "How the example rollout goes",
		"body": "The example rollout runs in three waves.\n\nWave one is the staging gate.",
		"tags": []any{"rollout"}, "plan": "example-rollout",
	})
	noteID := dgStr(t, note, "id")
	s.ok(a, "notes-read-id", "notes_read", map[string]any{"id": noteID})
	s.ok(a, "notes-update", "notes_update", map[string]any{
		"id": noteID, "body": "The example rollout runs in three waves.\n\nWave one is the staging gate.\n\nWave two widens it.",
		"tags_add": []any{"waves"},
	})
	s.ok(a, "notes-append", "notes_append", map[string]any{"id": noteID, "body": "Wave three is everyone."})
	s.ok(a, "notes-edit", "notes_edit", map[string]any{
		"id": noteID, "edits": []any{map[string]any{"old_string": "Wave two widens it.", "new_string": "Wave two widens it to half."}},
	})
	s.ok(a, "notes-read-slug", "notes_read", map[string]any{"slug": dgStr(t, note, "slug")})

	// ---- tasks: dependencies between OPEN projects only ------------------
	t1 := s.ok(a, "tasks-add-first", "tasks_add", map[string]any{"title": "Example: stage the deploy", "body": "Stage the example deploy."})
	t1ID := dgStr(t, t1, "id")
	t2 := s.ok(a, "tasks-add-second", "tasks_add", map[string]any{"title": "Example: promote the deploy", "depends_on": []any{t1ID}})
	t2ID := dgStr(t, t2, "id")
	t3 := s.ok(a, "tasks-add-beta", "tasks_add", map[string]any{
		"title": "Example: announce the deploy", "project": dgOpenB, "depends_on": []any{t2ID},
	})
	t3ID := dgStr(t, t3, "id")
	s.ok(a, "tasks-add-plan-step", "tasks_add", map[string]any{"title": "Example: wave one", "plan": "example-rollout"})
	s.ok(a, "tasks-ready-alpha", "tasks_ready", map[string]any{})
	s.ok(a, "tasks-list-alpha", "tasks_list", map[string]any{})
	s.ok(a, "tasks-list-blocked-id", "tasks_list", map[string]any{"id": t2ID})
	s.ok(a, "tasks-list-beta", "tasks_list", map[string]any{"project": dgOpenB})
	s.ok(a, "tasks-list-plan", "tasks_list", map[string]any{"plan": "example-rollout"})
	s.ok(a, "tasks-claim", "tasks_claim", map[string]any{"id": t1ID, "lease_seconds": 600})
	s.ok(a, "tasks-release", "tasks_release", map[string]any{"id": t1ID})
	s.ok(a, "tasks-claim-again", "tasks_claim", map[string]any{"id": t1ID})
	s.ok(a, "tasks-update-done", "tasks_update", map[string]any{"id": t1ID, "status": "done", "body": "Staged."})
	s.ok(a, "tasks-ready-after", "tasks_ready", map[string]any{})
	s.ok(a, "tasks-list-unblocked-id", "tasks_list", map[string]any{"id": t2ID})
	s.ok(a, "tasks-update-retitle", "tasks_update", map[string]any{"id": t3ID, "title": "Example: announce the promoted deploy"})
	s.refused(a, "tasks-claim-blocked", "tasks_claim", map[string]any{"id": t3ID})

	// ---- sessions, recall, projects --------------------------------------
	s.ok(a, "session-update", "session_update", map[string]any{"findings": "Staged the example deploy; promotion is next."})
	s.ok(a, "recall-all", "recall", map[string]any{"query": "example deploy staging gate"})
	s.ok(a, "recall-memories", "recall", map[string]any{"query": "example cache stale", "scope": "memories"})
	s.ok(a, "recall-tasks", "recall", map[string]any{"query": "promote deploy", "scope": "tasks"})
	s.ok(a, "recall-notes-kind", "recall", map[string]any{"query": "rollout waves", "scope": "notes"})
	s.ok(a, "project-list", "project_list", map[string]any{})
	s.ok(a, "project-create", "project_create", map[string]any{"name": "Example Open Delta", "slug": "example-open-delta", "description": "A fourth open example project"})

	// ---- research lab ------------------------------------------------------
	s.ok(a, "lab-open", "lab_open", map[string]any{"lab": "example-cache-lab", "goal": "Find the cache flush window"})
	s.ok(a, "trial-record", "trial_record", map[string]any{
		"title": "Flush after five minutes", "changes": "flush delay 5m", "expected": "no stale entries",
		"actual": "no stale entries", "outcome": "pass", "metrics": map[string]any{"stale_entries": 0},
	})
	s.ok(a, "trial-query", "trial_query", map[string]any{"lab": "example-cache-lab"})

	// ---- favorites, gardener, usage, capture -----------------------------
	s.ok(a, "favorite-set", "favorite_set", map[string]any{"kind": "memory", "id": "example-deploy-gate", "favorite": true})
	s.ok(a, "gardener-proposals", "gardener_proposals", map[string]any{})
	s.ok(a, "usage-summary", "usage_summary", map[string]any{})
	// capture_url needs the network for a real page; a loopback URL is refused
	// by the SSRF guard before any dial, so its refusal is the surface here.
	s.refused(a, "capture-url-loopback", "capture_url", map[string]any{"url": "http://127.0.0.1:9/example"})

	// ---- the confidential project's own session --------------------------
	conf := d.dial(t, "confidential", nil)
	s.ok(conf, "session-start-confidential", "session_start", map[string]any{"project": dgConf})
	s.ok(conf, "memory-write-confidential", "memory_write", map[string]any{
		"name": "example-confidential-gotcha", "kind": "gotcha",
		"description": "The confidential example ledger closes on the fifth",
		"body":        "The confidential example ledger closes on the fifth of each month.",
	})
	confNote := s.ok(conf, "notes-create-confidential", "notes_create", map[string]any{
		"title": "Confidential example ledger notes", "body": "The confidential ledger notes.",
	})
	confNoteID := dgStr(t, confNote, "id")
	confTask := s.ok(conf, "tasks-add-confidential", "tasks_add", map[string]any{"title": "Example: close the confidential ledger"})
	confTaskID := dgStr(t, confTask, "id")
	s.ok(conf, "recall-confidential-own", "recall", map[string]any{"query": "confidential ledger"})

	// ---- refusals that exist on main: a session bound elsewhere ----------
	s.refused(a, "refuse-read-confidential-memory", "memory_read", map[string]any{"name": "example-confidential-gotcha", "project": dgConf})
	s.refused(a, "refuse-read-confidential-note", "notes_read", map[string]any{"id": confNoteID})
	s.refused(a, "refuse-list-confidential-task", "tasks_list", map[string]any{"id": confTaskID})
	s.refused(a, "refuse-recall-confidential", "recall", map[string]any{"query": "confidential ledger", "project": dgConf})
	s.ok(a, "recall-unscoped-filters-confidential", "recall", map[string]any{"query": "confidential ledger"})
	// An inbound write into a confidential project lands, with the content
	// withheld from the answer.
	s.ok(a, "tasks-add-into-confidential", "tasks_add", map[string]any{"title": "Example: inbound confidential task", "project": dgConf})
	s.ok(a, "notes-update-confidential-withheld", "notes_update", map[string]any{"id": confNoteID, "description": "Inbound description"})

	// ---- a confidential session writing out ------------------------------
	s.refused(conf, "refuse-confidential-write-out", "memory_write", map[string]any{
		"name": "example-leak", "kind": "gotcha", "description": "leak", "body": "leak", "project": dgOpenA,
	})
	s.refused(conf, "refuse-confidential-write-global", "memory_write", map[string]any{
		"name": "example-leak-global", "kind": "gotcha", "description": "leak", "body": "leak", "project": "global",
	})
	s.refused(conf, "refuse-confidential-task-out", "tasks_add", map[string]any{"title": "leak", "project": dgOpenB})
	s.refused(conf, "refuse-confidential-update-out", "tasks_update", map[string]any{"id": t2ID, "title": "leak"})

	// ---- the sealed project ----------------------------------------------
	sealed := d.dial(t, "sealed", nil)
	s.ok(sealed, "session-start-sealed", "session_start", map[string]any{"project": dgSealed})
	s.ok(sealed, "memory-write-sealed", "memory_write", map[string]any{
		"name": "example-sealed-rule", "kind": "gotcha",
		"description": "The sealed example vault rotates keys weekly",
		"body":        "The sealed example vault rotates its keys every week.",
	})
	s.refused(sealed, "refuse-sealed-read-out", "memory_read", map[string]any{"name": "example-deploy-gate", "project": dgOpenA})
	s.refused(sealed, "refuse-sealed-read-global", "memory_read", map[string]any{"name": "example-style-reference", "project": "global"})
	s.refused(sealed, "refuse-sealed-write-out", "memory_write", map[string]any{
		"name": "example-sealed-leak", "kind": "gotcha", "description": "leak", "body": "leak", "project": dgOpenA,
	})
	s.refused(a, "refuse-write-into-sealed", "memory_write", map[string]any{
		"name": "example-sealed-inbound", "kind": "gotcha", "description": "inbound", "body": "inbound", "project": dgSealed,
	})
	s.refused(a, "refuse-read-sealed", "memory_read", map[string]any{"name": "example-sealed-rule", "project": dgSealed})
	s.refused(a, "refuse-recall-sealed", "recall", map[string]any{"query": "vault keys", "project": dgSealed})

	// ---- the agent's hooks with content in place -------------------------
	s.hook("hook-user-prompt-submit", "user-prompt-submit", nil, map[string]any{
		"session_id": "dg-claude-0001", "cwd": dgRepoA,
		"user_prompt":     "Before I deploy to production: does the staging gate apply, and when do stale cache entries clear?",
		"hook_event_name": "UserPromptSubmit",
	})
	s.hook("hook-session-start-resume", "session-start", procQuery, map[string]any{
		"session_id": "dg-claude-0001", "cwd": dgRepoA, "source": "resume",
	})
	s.hook("hook-session-start-confidential", "session-start", nil, map[string]any{
		"session_id": "dg-claude-0002", "cwd": dgRepoConf, "source": "startup",
	})
	s.hook("hook-user-prompt-confidential", "user-prompt-submit", nil, map[string]any{
		"session_id": "dg-claude-0002", "cwd": dgRepoConf, "user_prompt": "Which day of the month the confidential ledger closes on?",
		"hook_event_name": "UserPromptSubmit",
	})

	// ---- the agent's own connection, bound by its process ----------------
	proc := d.dial(t, "process", map[string]string{mcp.AgentProcessHeader: dgProcA})
	s.ok(proc, "process-memory-read", "memory_read", map[string]any{"name": "example-deploy-gate"})
	s.ok(proc, "process-session-update", "session_update", map[string]any{"findings": "Read the deploy gate."})
	s.ok(proc, "process-tasks-ready", "tasks_ready", map[string]any{})
	s.refused(proc, "process-refuse-read-confidential", "memory_read", map[string]any{"name": "example-confidential-gotcha", "project": dgConf})

	// ---- an unbound connection resumes a session by name ----------------
	other := d.dial(t, "resume", nil)
	s.ok(other, "session-start-resume-name", "session_start", map[string]any{"name": sessAName})
	s.ok(other, "memory-read-after-resume", "memory_read", map[string]any{"name": "example-layout-convention"})

	// ---- clean-up writes and the ends ------------------------------------
	s.ok(a, "memory-delete", "memory_delete", map[string]any{"name": "example-cache-gotcha-v2"})
	scratch := s.ok(a, "notes-create-scratch", "notes_create", map[string]any{"title": "Example scratch", "body": "scratch"})
	s.ok(a, "notes-delete", "notes_delete", map[string]any{"id": dgStr(t, scratch, "id")})
	s.ok(a, "favorite-unset", "favorite_set", map[string]any{"kind": "memory", "id": "example-deploy-gate", "favorite": false})
	// ---- plan mode: a plan file written, presented, approved -------------
	plansDir := filepath.Join(d.home, ".claude", "plans")
	require.NoError(t, os.MkdirAll(plansDir, 0o755))
	planPath := filepath.Join(plansDir, "example-deploy-plan.md")
	require.NoError(t, os.WriteFile(planPath, []byte("# Example deploy plan\n\n1. Stage the example deploy.\n2. Promote it.\n"), 0o644))
	s.hook("hook-post-tool-use-plan-write", "post-tool-use", nil, map[string]any{
		"session_id": "dg-claude-0001", "cwd": dgRepoA, "tool_name": "Write",
		"tool_input": map[string]any{"file_path": planPath, "content": "# Example deploy plan"},
	})
	s.hook("hook-permission-request-plan", "permission-request", nil, map[string]any{
		"session_id": "dg-claude-0001", "cwd": dgRepoA, "tool_name": "ExitPlanMode",
		"tool_input": map[string]any{"plan": "# Example deploy plan"},
	})
	s.hook("hook-post-tool-use-plan-approve", "post-tool-use", nil, map[string]any{
		"session_id": "dg-claude-0001", "cwd": dgRepoA, "tool_name": "ExitPlanMode",
		"tool_input":    map[string]any{"plan": "# Example deploy plan"},
		"tool_response": map[string]any{"plan": "# Example deploy plan", "filePath": planPath},
	})
	s.hook("hook-post-tool-use-read", "post-tool-use", nil, map[string]any{
		"session_id": "dg-claude-0001", "cwd": dgRepoA, "tool_name": "Read",
		"tool_input": map[string]any{"file_path": "/work/example-open-alpha/README.md"},
	})
	s.ok(a, "tasks-list-captured-plan", "tasks_list", map[string]any{"plan": "example-deploy-plan"})

	// ---- subagents and the screening hook (screening off) ---------------
	s.hook("hook-subagent-start", "subagent-start", nil, map[string]any{
		"session_id": "dg-claude-0001", "cwd": dgRepoA, "agent_id": "dg-agent-01", "agent_type": "general-purpose",
	})
	s.hook("hook-subagent-stop", "subagent-stop", nil, map[string]any{
		"session_id": "dg-claude-0001", "cwd": dgRepoA, "agent_id": "dg-agent-01", "agent_type": "general-purpose",
		"agent_transcript_path": filepath.Join(d.home, "no-such-transcript.jsonl"),
	})
	s.hook("hook-session-taint", "session-taint", nil, map[string]any{"session_id": "dg-claude-0001", "reason": "web_fetch"})

	// ---- argument and tool surfaces that options gate ------------------
	// commits.record is off: tasks_update has no commits argument, and the
	// unknown-parameter refusal is the old one word for word.
	s.refused(a, "refuse-commits-argument", "tasks_update", map[string]any{"id": t3ID, "commits": []any{strings.Repeat("ab", 20)}})
	// Nothing is locked: access_request is not a tool.
	s.rpcRefused(a, "refuse-access-request-tool", "access_request", map[string]any{"project": dgConf, "reason": "example"})
	s.refused(a, "refuse-memory-read-missing", "memory_read", map[string]any{"name": "example-no-such-memory"})
	s.refused(a, "refuse-gardener-request-no-llm", "gardener_request", map[string]any{"request": "tidy the example cache memories"})
	s.ok(a, "session-start-cwd", "session_start", map[string]any{"cwd": dgRepoA})

	// ---- A2A, healthz, the agent card ---------------------------------
	s.http("a2a-recall", http.MethodPost, "/api/a2a", `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"role":"user","kind":"message","messageId":"m1","parts":[{"kind":"text","text":"example style guide"}]}}}`)
	s.http("a2a-recall-project", http.MethodPost, "/api/a2a", `{"jsonrpc":"2.0","id":2,"method":"message/send","params":{"message":{"role":"user","kind":"message","messageId":"m2","parts":[{"kind":"text","text":"example deploy staging"}],"metadata":{"project":"example-open-alpha"}}}}`)
	s.http("a2a-recall-confidential", http.MethodPost, "/api/a2a", `{"jsonrpc":"2.0","id":3,"method":"message/send","params":{"message":{"role":"user","kind":"message","messageId":"m3","parts":[{"kind":"text","text":"confidential ledger"}],"metadata":{"project":"example-confidential"}}}}`)
	s.http("agent-card", http.MethodGet, "/.well-known/agent-card.json", "")
	s.http("healthz", http.MethodGet, "/healthz", "")

	// ---- an ambiguous ambient scope: agents in two open projects -------
	s.hook("hook-session-start-beta", "session-start", nil, map[string]any{
		"session_id": "dg-claude-0003", "cwd": dgRepoB, "source": "startup",
	})
	loose := d.dial(t, "loose", nil)
	s.refused(loose, "refuse-ambiguous-scope", "memory_write", map[string]any{
		"name": "example-ambiguous", "kind": "gotcha", "description": "ambiguous", "body": "ambiguous",
	})

	s.hook("hook-stop", "stop", nil, map[string]any{"session_id": "dg-claude-0001", "cwd": dgRepoA})
	s.hook("hook-session-end", "session-end", nil, map[string]any{"session_id": "dg-claude-0001", "cwd": dgRepoA, "reason": "other"})
	s.ok(a, "session-end-alpha", "session_end", map[string]any{
		"findings": "Staged the example deploy and recorded the cache trial.", "mishaps": []any{"none worth noting"},
	})
	s.ok(conf, "session-end-confidential", "session_end", map[string]any{"findings": "Closed the confidential ledger."})
	s.ok(sealed, "session-end-sealed", "session_end", map[string]any{"findings": "Rotated the sealed vault keys."})
	// After its session ended, the connection is unbound and its scope falls
	// to the ambient sessions still active: the confidential project's and
	// the open beta project's, which is ambiguous.
	s.refused(a, "refuse-ambient-after-end", "memory_write", map[string]any{
		"name": "example-after-end", "kind": "gotcha", "description": "after", "body": "after",
	})
	s.hook("hook-session-end-beta", "session-end", nil, map[string]any{"session_id": "dg-claude-0003", "cwd": dgRepoB, "reason": "other"})
	// The sole active ambient session is the confidential project's: never
	// inferred into.
	s.refused(loose, "refuse-ambient-confidential", "memory_write", map[string]any{
		"name": "example-ambient-fenced", "kind": "gotcha", "description": "fenced", "body": "fenced",
	})
	s.hook("hook-session-end-confidential", "session-end", nil, map[string]any{"session_id": "dg-claude-0002", "cwd": dgRepoConf, "reason": "other"})
	// Every agent gone: no scope to infer, and the scope error lists the
	// projects (knownProjects).
	s.refused(loose, "refuse-no-scope", "memory_write", map[string]any{
		"name": "example-no-scope", "kind": "gotcha", "description": "none", "body": "none",
	})
	s.ok(loose, "recall-unbound-global", "recall", map[string]any{"query": "example style guide"})
	s.ok(loose, "project-list-unbound", "project_list", map[string]any{})
}

// dgConsolePages are the console pages captured after the script, read with
// the bearer key the way `make console` opens them.
func dgConsolePages(t *testing.T, d *dgDaemon) []string {
	t.Helper()
	// The memory, note and task detail routes render their list page with the
	// item opened client-side, so the list pages stand for them.
	return []string{
		"/console/",
		"/console/projects",
		"/console/projects/" + dgOpenA,
		"/console/projects/" + dgConf,
		"/console/projects/" + dgSealed,
		"/console/memories",
		"/console/notes",
		"/console/tasks",
		// Not a session's own page: its interaction timeline draws each call's
		// duration as a bar width and orders same-millisecond events by id.
		"/console/sessions",
		"/console/plans",
		"/console/held",
		"/console/gardener",
		"/console/settings",
		"/console/search?q=example+deploy",
		"/console/trials",
		"/console/labs",
		"/console/retrieval",
		"/console/context",
	}
}

// dgConsole reads each console page and renders them as one plain-text
// document (console.txt), verbatim line for line. The shell around <main>
// repeats on every page, so the home page's head is written once, with the
// most common part after </main>; every page then carries its status, its
// <title> line, its <main> element, and its part after </main> only when that
// is not the common one. Nothing is trimmed or reflowed: a whitespace-only
// change still shows.
func dgConsole(t *testing.T, d *dgDaemon) string {
	t.Helper()
	type page struct {
		path             string
		status           int
		title            string
		head, main, tail []string
	}
	var pages []page
	tailCount := map[string]int{}
	for _, path := range dgConsolePages(t, d) {
		req, err := http.NewRequest(http.MethodGet, d.base+path, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+dgKey)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		lines := dgDropInteractions(strings.Split(string(body), "\n"))
		d.keep(string(body), strings.Join(lines, "\n"))
		start := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "<main") })
		end := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, "</main>") })
		require.True(t, start >= 0 && end >= start, "%s has no <main> element", path)
		p := page{path: path, status: resp.StatusCode, head: lines[:start], main: lines[start : end+1], tail: lines[end+1:]}
		if ti := slices.IndexFunc(p.head, func(l string) bool { return strings.Contains(l, "<title>") }); ti >= 0 {
			p.title = p.head[ti]
		}
		tailCount[strings.Join(p.tail, "\n")]++
		pages = append(pages, p)
	}
	// The common tail: the most frequent, ties broken by the text itself.
	var common string
	for key, n := range tailCount {
		if n > tailCount[common] || (n == tailCount[common] && key < common) {
			common = key
		}
	}
	var b strings.Builder
	section := func(title string, lines []string) {
		fmt.Fprintf(&b, "==== %s ====\n", title)
		for _, l := range lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	section("shell head (from "+pages[0].path+")", pages[0].head)
	section("shell common tail", strings.Split(common, "\n"))
	// Any other tail is written once, by the first page that carries it.
	written := map[string]string{common: "the common tail"}
	for _, p := range pages {
		fmt.Fprintf(&b, "==== GET %s -> %d ====\n%s\n", p.path, p.status, p.title)
		section(p.path+" main", p.main)
		key := strings.Join(p.tail, "\n")
		if name, ok := written[key]; ok {
			fmt.Fprintf(&b, "==== %s tail: %s ====\n", p.path, name)
			continue
		}
		written[key] = "the tail of " + p.path
		section(p.path+" tail", p.tail)
	}
	return b.String()
}

// dgDropInteractions removes a project page's Interactions tab panel: a
// per-minute volume chart and an event timeline whose buckets, and the order
// of events within one millisecond, depend on how long the script took (a
// -race run moves them). The rest of the page is kept verbatim.
func dgDropInteractions(lines []string) []string {
	out := make([]string, 0, len(lines))
	skipping := false
	for _, l := range lines {
		if strings.HasPrefix(l, `<div class="tabpanel"`) {
			skipping = strings.Contains(l, `data-panel="interactions"`)
			if skipping {
				out = append(out, "<dropped: the Interactions tab panel>")
				continue
			}
		}
		if !skipping {
			out = append(out, l)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// State after the script
// ---------------------------------------------------------------------------

// dgTables lists every table in the database, sorted.
func dgTables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		out = append(out, n)
	}
	require.NoError(t, rows.Err())
	return out
}

func dgCount(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM "`+table+`"`).Scan(&n))
	return n
}

// dgState is the database after the script: per-table row counts, the event
// kinds (with the project each was filed under) and the payload keys each
// kind carried, and the held-writes count.
func dgState(t *testing.T, d *dgDaemon) map[string]any {
	t.Helper()
	counts := map[string]int{}
	for _, table := range dgTables(t, d.db) {
		// schema_migrations is the migration ledger: the branch has more
		// migrations by construction, and the row-count rule is about rows a
		// surface writes. sqlite_sequence is SQLite's own bookkeeping.
		if table == "schema_migrations" || table == "sqlite_sequence" {
			continue
		}
		counts[table] = dgCount(t, d.db, table)
	}
	rows, err := d.db.Query(`SELECT kind, project_slug, payload FROM events ORDER BY kind, project_slug, rowid`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	kinds := map[string]int{}
	keys := map[string]map[string]bool{}
	for rows.Next() {
		var kind, project, payload string
		require.NoError(t, rows.Scan(&kind, &project, &payload))
		kinds[kind+" @"+project]++
		var obj map[string]any
		require.NoError(t, json.Unmarshal([]byte(payload), &obj), "event %s payload", kind)
		if keys[kind] == nil {
			keys[kind] = map[string]bool{}
		}
		for k := range obj {
			keys[kind][k] = true
		}
		d.keepRaw(payload)
	}
	require.NoError(t, rows.Err())
	payloadKeys := map[string][]string{}
	for kind, set := range keys {
		ks := make([]string, 0, len(set))
		for k := range set {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		payloadKeys[kind] = ks
	}
	return map[string]any{
		"row_counts":         counts,
		"event_kinds":        kinds,
		"event_payload_keys": payloadKeys,
		"held_writes":        dgCount(t, d.db, "held_writes"),
		"elicitations":       d.elicit.count(),
		"notifications":      d.notes.sorted(),
	}
}

// dgCorpus is every file under memory/ and notes/, by relative path, so a
// new frontmatter key (account) shows up as a golden difference.
func dgCorpus(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, tree := range []string{"memory", "notes"} {
		root := filepath.Join(dir, tree)
		err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() || !strings.HasSuffix(p, ".md") {
				return nil
			}
			rel, rerr := filepath.Rel(dir, p)
			if rerr != nil {
				return rerr
			}
			raw, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			out[filepath.ToSlash(rel)] = string(raw)
			return nil
		})
		if os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err)
	}
	return out
}

// ---------------------------------------------------------------------------
// Normalization
// ---------------------------------------------------------------------------

// dgNormalizer rewrites only what is genuinely nondeterministic between two
// runs of the same script; everything else stays verbatim.
//
//   - ULIDs (core.NewID: time plus crypto randomness) become <id:N>, numbered
//     in order of first appearance, so two places naming the same item still
//     name the same placeholder.
//   - MCP session names (sess/<last eight ULID characters, lowercased>) become
//     <sess:N> the same way.
//   - RFC 3339 timestamps become <time>, and bare dates <date>: wall-clock.
//   - 64-hex digests become <hash:N>: content hashes of files whose
//     frontmatter carries ids and timestamps.
//   - The data dir, the throwaway home and the test server's base URL (its
//     port) become <data-dir>, <home> and <base>.
//   - Relative ages are wall-clock too: a recall hit's "age" field and any
//     "just now" / "N <unit> ago" phrase in text become <age> (dgAgeRe).
//   - The console's 24-hour charts label each bucket with a local hour
//     ("17:00"), which depends on when and where the test runs: <hh>:00.
//   - A "score" number is rounded to four decimals: usage_summary's utility
//     score decays with the seconds since each read, so its trailing digits
//     move between runs (observed at the 8th decimal). Recall's RRF scores
//     are exact fractions and survive the rounding unchanged in meaning.
//
// The console's HTML adds its own wall-clock and random renderings:
//
//   - an item's short id (the last eight characters of its ULID) becomes
//     <id:N>~8, through the same numbering as the full id;
//   - data-ts / data-t / data-c attributes are unix times: <unix>;
//   - "took Nms" chips and "Nms" meta labels are request durations; the chip
//     renders only above a threshold, so it is dropped and the label becomes
//     <ms>;
//   - data-live-v is a digest of a live panel's data (which carries times);
//   - the knowledge sky places each star by its id and animates it with a
//     random phase: translate(...), data-x/data-y and --d become placeholders;
//   - HTML-escaped "score" numbers round as above.
type dgNormalizer struct {
	literal []string // old, new pairs
	ids     map[string]string
	shorts  map[string]string // last eight characters of a known ULID -> its placeholder~8
	sess    map[string]string
	hashes  map[string]string
}

var (
	dgShortRe    = regexp.MustCompile(`\b[0-9A-HJKMNP-TV-Z]{8}\b`)
	dgTsAttrRe   = regexp.MustCompile(`data-(ts|t|c)="\d{10,13}"`)
	dgTookRe     = regexp.MustCompile(`<span class="ix-chip"><span class="ix-chip-k">took</span><span class="ix-chip-v">\d+ms</span></span>`)
	dgMsRe       = regexp.MustCompile(`>\d+(?:\.\d+)?ms<`)
	dgLiveVRe    = regexp.MustCompile(`data-live-v="[0-9a-f]+"`)
	dgSkyPosRe   = regexp.MustCompile(`translate\([0-9.]+px,[0-9.]+px\)`)
	dgSkyXYRe    = regexp.MustCompile(`data-(x|y)="[0-9.]+"`)
	dgSkyPhaseRe = regexp.MustCompile(`--d:-?[0-9.]+s`)
	dgEscScoreRe = regexp.MustCompile(`&#34;score&#34;:([0-9.]+)`)
	dgULIDRe     = regexp.MustCompile(`\b[0-9A-HJKMNP-TV-Z]{26}\b`)
	dgSessRe     = regexp.MustCompile(`\bsess/[0-9a-z]{8}\b`)
	dgTimeRe     = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:?\d{2})?`)
	dgDateRe     = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	dgHashRe     = regexp.MustCompile(`\b[0-9a-f]{64}\b`)
	dgAgeRe      = regexp.MustCompile(`\bjust now\b|\b\d+(?:\.\d+)?\s?(?:s|m|h|d|sec|secs|min|mins|seconds?|minutes?|hours?|days?) ago\b`)
	dgHourRe     = regexp.MustCompile(`\b(?:[01]\d|2[0-3]):00\b`)
)

func dgNewNormalizer(d *dgDaemon) *dgNormalizer {
	n := &dgNormalizer{ids: map[string]string{}, shorts: map[string]string{}, sess: map[string]string{}, hashes: map[string]string{}}
	add := func(old, repl string) {
		if old == "" {
			return
		}
		n.literal = append(n.literal, old, repl)
		// JSON-escaped forms appear inside text that is itself JSON.
		if esc, err := json.Marshal(old); err == nil {
			s := strings.Trim(string(esc), `"`)
			if s != old {
				n.literal = append(n.literal, s, repl)
			}
		}
	}
	for _, p := range []struct{ path, repl string }{{d.dir, "<data-dir>"}, {d.home, "<home>"}} {
		if real, err := filepath.EvalSymlinks(p.path); err == nil && real != p.path {
			add(real, p.repl)
		}
		add(p.path, p.repl)
	}
	add(d.base, "<base>")
	add(strings.TrimPrefix(d.base, "http://"), "<base-host>")
	// The console names the machine it runs on (os.Hostname, console.New):
	// it differs from machine to machine, and is no business of the golden.
	if h, err := os.Hostname(); err == nil && strings.TrimSpace(h) != "" {
		add("server "+strings.TrimSpace(h), "server <hostname>")
	}
	return n
}

func dgMapped(m map[string]string, prefix, key string) string {
	if v, ok := m[key]; ok {
		return v
	}
	v := fmt.Sprintf("<%s:%d>", prefix, len(m)+1)
	m[key] = v
	return v
}

// prime numbers every ULID in texts, in order, before anything is
// normalized: a console page names an item by its short id before (or
// without) its full one, and the short id is only recognizable as the suffix
// of a ULID already known.
func (n *dgNormalizer) prime(texts []string) {
	for _, t := range texts {
		for _, m := range dgULIDRe.FindAllString(t, -1) {
			dgMapped(n.ids, "id", m)
		}
	}
	n.shorts = map[string]string{}
	for id, ph := range n.ids {
		n.shorts[id[len(id)-8:]] = ph + "~8"
	}
}

func (n *dgNormalizer) str(s string) string {
	for i := 0; i < len(n.literal); i += 2 {
		s = strings.ReplaceAll(s, n.literal[i], n.literal[i+1])
	}
	s = dgULIDRe.ReplaceAllStringFunc(s, func(m string) string { return dgMapped(n.ids, "id", m) })
	s = dgShortRe.ReplaceAllStringFunc(s, func(m string) string {
		if ph, ok := n.shorts[m]; ok {
			return ph
		}
		return m
	})
	s = dgTsAttrRe.ReplaceAllString(s, `data-$1="<unix>"`)
	s = dgTookRe.ReplaceAllString(s, "")
	s = dgMsRe.ReplaceAllString(s, "><ms><")
	s = dgLiveVRe.ReplaceAllString(s, `data-live-v="<v>"`)
	s = dgSkyPosRe.ReplaceAllString(s, "translate(<xy>)")
	s = dgSkyXYRe.ReplaceAllString(s, `data-$1="<n>"`)
	s = dgSkyPhaseRe.ReplaceAllString(s, "--d:<phase>")
	s = dgEscScoreRe.ReplaceAllStringFunc(s, func(m string) string {
		var f float64
		if _, err := fmt.Sscanf(dgEscScoreRe.FindStringSubmatch(m)[1], "%g", &f); err != nil {
			return m
		}
		return fmt.Sprintf("&#34;score&#34;:%g", math.Round(f*1e4)/1e4)
	})
	s = dgSessRe.ReplaceAllStringFunc(s, func(m string) string { return dgMapped(n.sess, "sess", m) })
	s = dgTimeRe.ReplaceAllString(s, "<time>")
	s = dgDateRe.ReplaceAllString(s, "<date>")
	s = dgHashRe.ReplaceAllStringFunc(s, func(m string) string { return dgMapped(n.hashes, "hash", m) })
	s = dgAgeRe.ReplaceAllString(s, "<age>")
	s = dgHourRe.ReplaceAllString(s, "<hh>:00")
	return s
}

// value normalizes a decoded JSON value, keys included, walking maps in sorted
// key order so the placeholder numbering does not depend on map iteration.
//
// A multi-line string becomes the array of its lines (after normalization),
// so a golden diff points at the briefing line that changed rather than at
// one long escaped string.
func (n *dgNormalizer) value(v any) any {
	switch x := v.(type) {
	case string:
		x = n.str(x)
		if strings.Contains(x, "\n") {
			lines := strings.Split(x, "\n")
			out := make([]any, len(lines))
			for i, l := range lines {
				out[i] = l
			}
			return map[string]any{"lines": out}
		}
		return x
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = n.value(e)
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(x))
		for _, k := range keys {
			switch v := x[k].(type) {
			case string:
				if k == "age" {
					out[k] = "<age>"
					continue
				}
			case float64:
				if k == "score" {
					out[k] = math.Round(v*1e4) / 1e4
					continue
				}
			}
			out[n.str(k)] = n.value(x[k])
		}
		return out
	default:
		return v
	}
}

// encode turns any value into its normalized, indented JSON.
func (n *dgNormalizer) encode(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var generic any
	require.NoError(t, json.Unmarshal(raw, &generic))
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // placeholders and briefing tags stay readable
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(n.value(generic)))
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// Accepted differences
// ---------------------------------------------------------------------------

// dgAccepted are the differences from main that the owner accepted as
// legitimate changes for every install, not as part of the opt-in feature the
// gate guards. The golden is written from main, so it cannot carry them: each
// entry is the branch's exact text and main's, and the branch's file is mapped
// back to main's text before the comparison.
//
// An entry is strict: its branch text must occur exactly n times, or the gate
// fails, so it never hides anything beyond what was accepted and a stale entry
// shows. Nothing is mapped while writing the golden from main. Once main
// carries the change, drop the entry and regenerate the golden there.
//
// None is open: main carries every difference accepted so far, and the golden
// is written from it.
var dgAccepted = []struct {
	file, why    string
	branch, main string
	n            int
}{}

// dgAccept maps the accepted differences in file's normalized text back to
// main's text.
func dgAccept(t *testing.T, file, text string) string {
	t.Helper()
	for _, a := range dgAccepted {
		if a.file != file {
			continue
		}
		if got := strings.Count(text, a.branch); got != a.n {
			t.Errorf("%s: the accepted difference (%s) occurs %d times, want %d: update or drop the entry", file, a.why, got, a.n)
			continue
		}
		text = strings.ReplaceAll(text, a.branch, a.main)
	}
	return text
}

// ---------------------------------------------------------------------------
// Golden comparison
// ---------------------------------------------------------------------------

// dgCompare compares got with the golden file name, or writes it when
// updating. The accepted differences are mapped back to main's text first;
// DG_OUT still gets the file as this tree renders it. A mismatch reports the
// differing region, line-numbered.
func dgCompare(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join(dgGoldenDir, name)
	if out := os.Getenv("DG_OUT"); out != "" {
		require.NoError(t, os.MkdirAll(out, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(out, name), got, 0o644))
	}
	if dgUpdating() {
		require.NoError(t, os.MkdirAll(dgGoldenDir, 0o755))
		require.NoError(t, os.WriteFile(path, got, 0o644))
		return
	}
	got = []byte(dgAccept(t, name, string(got)))
	want, err := os.ReadFile(path)
	require.NoError(t, err, "golden %s missing: generate it on a main extract with -dg.update", path)
	if string(want) == string(got) {
		return
	}
	t.Errorf("%s differs from the main golden:\n%s", name, dgDiff(string(want), string(got)))
}

// dgDiff is a small line diff: the common prefix and suffix are dropped and
// the rest is aligned by an LCS, then shown as - (main) and + (this tree)
// lines with two lines of context and, for each hunk, the section it falls
// in (a console "====" heading, a transcript step, a tool name), capped so a
// wholesale change stays readable.
func dgDiff(want, got string) string {
	a := strings.Split(want, "\n")
	b := strings.Split(got, "\n")
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	am, bm := a[pre:len(a)-suf], b[pre:len(b)-suf]
	// op is one aligned line: ' ' both, '-' main only, '+' this tree only;
	// bi is its index in b (for a '-' line, where it would have been).
	type op struct {
		kind byte
		text string
		ai   int
		bi   int
	}
	var ops []op
	for k := max(0, pre-2); k < pre; k++ {
		ops = append(ops, op{' ', a[k], k, k})
	}
	if len(am)*len(bm) > 4_000_000 {
		for i, l := range am {
			ops = append(ops, op{'-', l, pre + i, pre})
		}
		for j, l := range bm {
			ops = append(ops, op{'+', l, pre, pre + j})
		}
	} else {
		lcs := make([][]int, len(am)+1)
		for i := range lcs {
			lcs[i] = make([]int, len(bm)+1)
		}
		for i := len(am) - 1; i >= 0; i-- {
			for j := len(bm) - 1; j >= 0; j-- {
				if am[i] == bm[j] {
					lcs[i][j] = lcs[i+1][j+1] + 1
				} else {
					lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
				}
			}
		}
		i, j := 0, 0
		for i < len(am) || j < len(bm) {
			switch {
			case i < len(am) && j < len(bm) && am[i] == bm[j]:
				ops = append(ops, op{' ', am[i], pre + i, pre + j})
				i++
				j++
			case i < len(am) && (j == len(bm) || lcs[i+1][j] >= lcs[i][j+1]):
				ops = append(ops, op{'-', am[i], pre + i, pre + j})
				i++
			default:
				ops = append(ops, op{'+', bm[j], pre + i, pre + j})
				j++
			}
		}
	}
	for k := 0; k < min(2, suf); k++ {
		ops = append(ops, op{' ', a[len(a)-suf+k], len(a) - suf + k, len(b) - suf + k})
	}
	near := func(k int) bool {
		for d := -2; d <= 2; d++ {
			if k+d >= 0 && k+d < len(ops) && ops[k+d].kind != ' ' {
				return true
			}
		}
		return false
	}
	// A console section heading precedes its lines; a transcript step's and a
	// tool's name follow their large fields (the keys are written sorted).
	heading := func(bi int) string {
		for k := min(bi, len(b)-1); k >= 0; k-- {
			if strings.HasPrefix(b[k], "==== ") {
				return strings.TrimSpace(b[k])
			}
		}
		for _, marker := range []string{`"step": "`, `"name": "`} {
			for k := max(bi, 0); k < len(b); k++ {
				if strings.Contains(b[k], marker) {
					return strings.TrimSpace(b[k])
				}
			}
		}
		return ""
	}
	var out strings.Builder
	const maxLines = 400
	lines, shown := 0, false
	for k, o := range ops {
		if !near(k) {
			shown = false
			continue
		}
		if !shown {
			if lines < maxLines {
				fmt.Fprintf(&out, "@@ main line %d, this tree line %d -- %s\n", o.ai+1, o.bi+1, heading(o.bi))
			}
			shown = true
		}
		if lines < maxLines {
			fmt.Fprintf(&out, "%c %s\n", o.kind, o.text)
		}
		lines++
	}
	if lines > maxLines {
		fmt.Fprintf(&out, "... %d more diff lines (set DG_OUT=<dir> for the whole file)\n", lines-maxLines)
	}
	return out.String()
}

// ---------------------------------------------------------------------------
// The test
// ---------------------------------------------------------------------------

// TestDefaultsGolden drives the scripted sequence against a default-config
// daemon and compares every surface with the golden files main wrote.
func TestDefaultsGolden(t *testing.T) {
	d := dgNewDaemon(t)
	s := &dgScript{t: t, d: d}

	// ---- handshake and tool surface --------------------------------------
	first := d.dial(t, "handshake", nil)
	tools, err := first.cli.ListTools(context.Background(), mcpgo.ListToolsRequest{})
	require.NoError(t, err)
	initRaw, err := json.Marshal(first.init)
	require.NoError(t, err)
	d.keepRaw(string(initRaw))
	toolsRaw, err := json.Marshal(tools)
	require.NoError(t, err)
	d.keepRaw(string(toolsRaw))

	dgRunScript(t, s)

	consolePages := dgConsole(t, d)
	state := dgState(t, d)
	corpus := dgCorpus(t, d.dir)

	n := dgNewNormalizer(d)
	d.rawMu.Lock()
	n.prime(d.prime)
	d.rawMu.Unlock()
	// Each golden file is its own subtest, so a failure names the surface
	// (go test -run 'TestDefaultsGolden/transcript').
	golden := []struct {
		name string
		v    any
	}{
		{"initialize.json", map[string]any{
			"protocol_version": first.init.ProtocolVersion,
			"server_info":      first.init.ServerInfo,
			"capabilities":     first.init.Capabilities,
			"instructions":     first.init.Instructions,
		}},
		{"tools_list.json", map[string]any{"count": len(tools.Tools), "next_cursor": tools.NextCursor, "tools": tools.Tools}},
		{"transcript.json", s.steps},
		{"corpus.json", corpus},
		{"console.txt", consolePages},
	}
	for _, g := range golden {
		// Encode outside the subtest, in this fixed order: the placeholder
		// numbering continues from one file into the next.
		var got []byte
		if text, ok := g.v.(string); ok {
			got = []byte(n.str(text))
		} else {
			got = n.encode(t, g.v)
		}
		t.Run(strings.TrimSuffix(strings.TrimSuffix(g.name, ".json"), ".txt"), func(t *testing.T) { dgCompare(t, g.name, got) })
	}

	// The plan's own tables (migrations 031-034) hold no row. On main they do
	// not exist; on a binary whose schema has them, every one must exist, so
	// this check is never vacuous there.
	counts := state["row_counts"].(map[string]int)
	t.Run("plan_tables_empty", func(t *testing.T) {
		if store.LatestSchemaVersion() < 31 {
			t.Skip("main: the plan's tables do not exist before migration 031")
		}
		for _, table := range dgPlanTables {
			got, exists := counts[table]
			require.True(t, exists, "table %s is missing from a schema at version %d", table, store.LatestSchemaVersion())
			require.Zero(t, got, "table %s must hold no row with peer_auth and isolation.requests off", table)
		}
	})
	// Any other table main does not have must be empty too; the golden then
	// compares the tables main has.
	if !dgUpdating() {
		var want map[string]any
		raw, err := os.ReadFile(filepath.Join(dgGoldenDir, "state.json"))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &want))
		mainCounts, _ := want["row_counts"].(map[string]any)
		t.Run("new_tables_empty", func(t *testing.T) {
			for table, got := range counts {
				if _, onMain := mainCounts[table]; !onMain {
					require.Zero(t, got, "table %s, which main does not have, holds rows with the defaults", table)
				}
			}
		})
		for table := range counts {
			if _, onMain := mainCounts[table]; !onMain {
				delete(counts, table)
			}
		}
	}
	stateJSON := n.encode(t, state)
	t.Run("state", func(t *testing.T) { dgCompare(t, "state.json", stateJSON) })

	// No elicitation, ever; no notification (the Touch ID helper is launched
	// only by core.ApproverNotification); no held row after the owner's
	// constraint write with screening off.
	t.Run("no_elicitation_no_helper_no_hold", func(t *testing.T) {
		require.Zero(t, d.elicit.count(), "the defaults never ask the client for anything")
		require.Empty(t, d.notes.sorted(), "the defaults send no notification (none launches the approver)")
		require.Zero(t, state["held_writes"], "no held row with write screening off")
	})

	// SEAL A's labels never appear with the defaults: no response key, no
	// event payload key, no frontmatter key, no "written by account" text.
	t.Run("no_account_labels", func(t *testing.T) {
		d.rawMu.Lock()
		raws := slices.Clone(d.raw)
		d.rawMu.Unlock()
		for path, body := range corpus {
			raws = append(raws, body)
			require.NotRegexp(t, `(?m)^account:`, body, "%s carries an account label", path)
		}
		for _, raw := range raws {
			require.NotContains(t, raw, "written by account")
			for _, key := range dgForbiddenKeys {
				for _, form := range []string{`"` + key + `":`, `\"` + key + `\":`, `&#34;` + key + `&#34;:`} {
					require.NotContains(t, raw, form, "a response carries the %s key", key)
				}
			}
		}
	})
}
