package main

// The peer_auth matrix: one daemon (the real route table, every service), a
// project sealed to one OS account, content planted in it BY that account, and
// then every MCP tool in mcp.Catalog(), every console route, the hooks, A2A and
// the live feed driven as the owner (TCP + bearer key) and as a different
// configured account (the socket, peer identity injected). The planted content
// carries paSecret in every content field and no probe ever sends paSecret, so
// any occurrence of it in a response is a leak, whatever the route. The sealed
// account itself must see it.

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/a2a"
	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/hooks"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const (
	// paSecret is in every content field planted in the sealed project, and in
	// no probe: finding it in a response to anyone but the sealed account is a
	// leak, with no echo of the caller's own input to explain it away.
	paSecret = "kx9secret"
	// paSearch is a word every planted item also carries, so search probes have
	// something to hit.
	paSearch = "zq7probe"

	// paOwnSecret is the same for the owner's own project, which no sealed
	// account may reach; paGlobalToken is in a global memory every account reads.
	paOwnSecret   = "own7secret"
	paGlobalToken = "glob4token"

	paSealed = "arctop-finance" // sealed to _arctop_finance
	paLegal  = "arctop-legal"   // sealed to _arctop_legal
	paOpen   = "arctop-ops"     // the owner's, open

	paKey = "peer-auth-matrix-key"

	paUIDOwner    = 501
	paUIDFinance  = 470
	paUIDLegal    = 471
	paUIDStranger = 4242

	paHost = "boron"

	// paFinanceProc is the agent-process identity the sealed account's agent
	// stamps on its session (pid.start), which an outsider can try to forge.
	paFinanceProc = "4711.1730000000"
)

// paAccounts is the fake directory service behind the matrix's policy.
var paAccounts = map[string]access.Account{
	"katata":          {UID: paUIDOwner, Name: "katata", Home: "/Users/katata"},
	"_arctop_finance": {UID: paUIDFinance, Name: "_arctop_finance", Home: "/opt/arctop/finance/home"},
	"_arctop_legal":   {UID: paUIDLegal, Name: "_arctop_legal", Home: "/opt/arctop/legal/home"},
}

func paResolve(name string) (access.Account, error) {
	if a, ok := paAccounts[name]; ok {
		return a, nil
	}
	return access.Account{}, fmt.Errorf("no such account %q", name)
}

func paPolicyConfig() config.PeerAuth {
	return config.PeerAuth{
		Enabled: true,
		Owner:   "katata",
		Socket:  config.PeerSocket{Path: "/opt/seamless/run/seamlessd.sock"},
		Users: map[string]config.PeerUser{
			"_arctop_finance": {Sealed: []string{paSealed}},
			"_arctop_legal":   {Sealed: []string{paLegal}},
		},
	}
}

// paItems is one project's planted content: what an outsider probes for, and
// what must be unchanged afterwards. secret is in every content field and in no
// probe.
type paItems struct {
	project, secret                                        string
	memID, memName, noteID, noteSlug, taskID, trialID, lab string
	plan, sessID, sessName, proposalID                     string
}

// paDaemon is the matrix's daemon: the real route table over real services, one
// TCP server for the owner and one server per socket principal.
type paDaemon struct {
	db     *sql.DB
	rec    *events.Recorder
	dir    string
	policy *access.Policy
	mux    *http.ServeMux
	owner  *httptest.Server
	peers  map[uint32]*httptest.Server

	// sealed is what _arctop_finance planted in arctop-finance; ops is what the
	// owner planted in arctop-ops, which no sealed account may reach.
	sealed, ops       paItems
	claudeID, eventID string
	// endedClaudeID names an agent session of the sealed account that has ended.
	endedClaudeID string
	// gardenerEventID is an event the gardener recorded, for no caller, about a
	// sealed proposal.
	gardenerEventID string
}

func newPADaemon(t *testing.T) *paDaemon {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(ctx, db, paHost))

	for _, slug := range []string{paSealed, paLegal, paOpen} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, paSealed, core.IsolationConfidential))
	require.NoError(t, store.SetProjectIsolation(ctx, db, paLegal, core.IsolationConfidential))
	require.NoError(t, store.AddRepoMapping(ctx, db, "/work/finance-os", paSealed))
	require.NoError(t, store.AddRepoMapping(ctx, db, "/work/ops", paOpen))

	policy, err := access.New(paPolicyConfig(), paResolve)
	require.NoError(t, err)
	problems, err := projectProblems(ctx, db, policy)
	require.NoError(t, err)
	require.Empty(t, problems, "the fixture must pass serve's project preflight")

	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	ret := retrieve.New(db, nil, config.Defaults().Budgets, nil)
	ret.SetBodyReader(mgr.Store())
	rec := events.NewRecorder(db)
	garden := gardener.New(db, mgr, nil, nil, rec, gardener.Config{}, nil)
	feats := config.Features{Research: true, Momentum: true, Gamification: true}

	mcpSrv := mcp.New(mcp.Config{
		DB: db, Files: mgr, Retrieve: ret, Events: rec, Gardener: garden,
		APIKey: paKey, LocalHost: paHost, Features: feats,
	})
	a2aSrv, err := a2a.New(a2a.Config{Retrieve: ret, Events: rec, APIKey: paKey, Version: "test", Endpoint: "http://127.0.0.1:8081/api/a2a"})
	require.NoError(t, err)
	hooksH := hooks.NewHandler(hooks.Config{
		DB: db, Retrieve: ret, Events: rec, Files: mgr, APIKey: paKey, LocalHost: paHost,
		PlanCapture: config.PlanCapture{Enabled: true, AutoTask: true, InjectRelated: true},
	})
	viewDB, err := store.OpenView(dbPath, policy.SealedProjects())
	require.NoError(t, err)
	t.Cleanup(func() { _ = viewDB.Close() })
	// Every console write must reach the main handle: one that reaches the
	// read-only filtered handle fails with SQLite's "readonly database", which
	// the console logs (several are degradations, not 500s), so the log is
	// where the matrix looks.
	consoleLog := &paLogSink{}
	t.Cleanup(func() {
		require.NotContains(t, consoleLog.String(), "readonly database",
			"a console write reached the read-only filtered handle")
	})
	consoleSrv, err := console.New(console.Config{
		DB: viewDB, WriteDB: db, Files: mgr, Gardener: garden, Events: rec.ReadingFrom(viewDB),
		Retrieve: ret, APIKey: paKey, DataDir: dir, Features: feats, Level: "advanced",
		Logger: slog.New(slog.NewTextHandler(consoleLog, nil)),
	})
	require.NoError(t, err)
	mux := daemonRoutes{
		healthz:   healthzHandler(db, "instance", nil),
		mcp:       mcpSrv.Handler(),
		a2a:       a2aSrv.Handler(),
		agentCard: a2aSrv.CardHandler(),
		hooks:     hooksH,
		console:   consoleSrv,
	}.mux()

	d := &paDaemon{db: db, rec: rec, dir: dir, policy: policy, mux: mux, peers: map[uint32]*httptest.Server{}}
	d.owner = httptest.NewServer(ownerHandler(policy, mux))
	t.Cleanup(func() { closeServer(d.owner) })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, uid := range []uint32{paUIDOwner, paUIDFinance, paUIDLegal, paUIDStranger} {
		srv := httptest.NewServer(injectPeer(uid, peerHandler(policy, logger, mux)))
		t.Cleanup(func() { closeServer(srv) })
		d.peers[uid] = srv
	}
	return d
}

// paLogSink collects log output for assertions.
type paLogSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *paLogSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *paLogSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// closeServer drops any connection still open (a live-feed stream a failed
// assertion left behind) before Close, which would otherwise wait on it.
func closeServer(srv *httptest.Server) {
	srv.CloseClientConnections()
	srv.Close()
}

// injectPeer stands in for peerConnContext: the kernel's answer for the
// connection, here a fixed uid. Real-socket tests run the kernel path as the
// test's own uid (peerauth_socket_test.go).
func injectPeer(uid uint32, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), peerCredKey{}, peerCred{uid: uid})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// paClient is an MCP client against base: the owner presents the key, a socket
// principal presents nothing (the kernel is its credential).
func paClient(t *testing.T, base, key string) *mcpclient.Client {
	t.Helper()
	return paClientHeaders(t, base, key, nil)
}

// paClientHeaders is paClient with extra request headers (a forged identity).
func paClientHeaders(t *testing.T, base, key string, extra map[string]string) *mcpclient.Client {
	t.Helper()
	ctx := context.Background()
	headers := map[string]string{}
	if key != "" {
		headers["Authorization"] = "Bearer " + key
	}
	for k, v := range extra {
		headers[k] = v
	}
	cli, err := mcpclient.NewStreamableHttpClient(base+"/api/mcp", transport.WithHTTPHeaders(headers))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	require.NoError(t, cli.Start(ctx))
	var init mcpgo.InitializeRequest
	init.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcpgo.Implementation{Name: "matrix", Version: "0"}
	_, err = cli.Initialize(ctx, init)
	require.NoError(t, err)
	return cli
}

// paCall calls a tool and returns its text and whether it was an error result.
func paCall(t *testing.T, cli *mcpclient.Client, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cli.CallTool(context.Background(), mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: name, Arguments: args}})
	require.NoError(t, err, "%s transport error", name)
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcpgo.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func paCallOK(t *testing.T, cli *mcpclient.Client, name string, args map[string]any) map[string]any {
	t.Helper()
	text, isErr := paCall(t, cli, name, args)
	require.False(t, isErr, "%s failed: %s", name, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out), "%s: %s", name, text)
	return out
}

// plantItems writes one project's content through cli -- the same tools any of
// its agents uses -- with secret in every content field.
func (d *paDaemon) plantItems(t *testing.T, cli *mcpclient.Client, project, secret, prefix string) paItems {
	t.Helper()
	ctx := context.Background()
	it := paItems{project: project, secret: secret}
	it.sessName = prefix + "-agent-session"
	sess := paCallOK(t, cli, "session_start", map[string]any{"project": project, "name": it.sessName})
	it.sessID = sess["session_id"].(string)
	require.Equal(t, project, sess["project"])

	it.memName = prefix + "-ledger-rule"
	mem := paCallOK(t, cli, "memory_write", map[string]any{
		"name": it.memName, "kind": "gotcha",
		"description": paSearch + " " + secret + " ledger description",
		"body":        "The " + paSearch + " ledger rule: " + secret + " body text.",
	})
	it.memID = mem["id"].(string)
	// The title becomes the slug, and a slug is an identifier probes send; the
	// secret rides in the description and the body instead.
	note := paCallOK(t, cli, "notes_create", map[string]any{
		"title": prefix + " " + paSearch + " board memo", "description": secret + " memo description",
		"body": secret + " memo body " + paSearch,
	})
	it.noteID, it.noteSlug = note["id"].(string), note["slug"].(string)
	it.plan = prefix + "-close-plan"
	task := paCallOK(t, cli, "tasks_add", map[string]any{
		"title": paSearch + " " + secret + " reconcile", "body": secret + " task body", "plan": it.plan,
	})
	it.taskID = task["id"].(string)
	// Claimed, so an outsider's force-release would have something to undo.
	paCallOK(t, cli, "tasks_claim", map[string]any{"id": it.taskID})
	it.lab = prefix + "-forecast-lab"
	paCallOK(t, cli, "lab_open", map[string]any{"lab": it.lab, "goal": secret + " goal"})
	trial := paCallOK(t, cli, "trial_record", map[string]any{
		"title": paSearch + " " + secret + " trial", "changes": secret + " changes", "actual": secret,
	})
	it.trialID = trial["id"].(string)
	paCallOK(t, cli, "session_update", map[string]any{"findings": paSearch + " " + secret + " findings"})
	// A call carrying content in its arguments: the tool.call event it records
	// is this project's content too.
	_, _ = paCall(t, cli, "recall", map[string]any{"query": paSearch + " " + secret})

	// A gardener proposal about the memory (the passes would raise one; the
	// matrix plants it directly).
	p, err := store.CreateProposal(ctx, d.db, store.ProposalArchive, map[string]any{
		"id": it.memID, "project": project, "name": it.memName, "reason": secret + " stale",
	})
	require.NoError(t, err)
	it.proposalID = p.ID
	return it
}

// sessionsIn counts the session rows in project: an outsider must not be able
// to start one there either.
func (d *paDaemon) sessionsIn(t *testing.T, project string) int {
	t.Helper()
	var n int
	require.NoError(t, d.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sessions WHERE project_slug = ?`, project).Scan(&n))
	return n
}

// plant writes the sealed project's content as its own OS account, over the
// socket, and the owner's own project and global content over TCP.
func (d *paDaemon) plant(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	fin := paClient(t, d.peers[paUIDFinance].URL, "")
	d.sealed = d.plantItems(t, fin, paSealed, paSecret, "fin")

	// The account's own agent session, through its hook: an unmapped cwd lands
	// in its own project.
	d.claudeID = "fin-claude-0001"
	out := d.hook(t, d.peers[paUIDFinance].URL+"/api/hooks/session-start?agent_process="+paFinanceProc, "", "", map[string]any{
		"session_id": d.claudeID, "cwd": "/opt/arctop/finance/app", "source": "startup",
	})
	require.Contains(t, out, paSecret, "the sealed account's own briefing carries its own content")
	amb, ok, err := store.AmbientSessionByExternalIdentity(ctx, d.db, "claude-code", d.claudeID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, paSealed, amb.ProjectSlug, "the account's session lands in its own project")
	stamped, err := store.ActiveAmbientByAgentProcess(ctx, d.db, paHost, paFinanceProc)
	require.NoError(t, err)
	require.Len(t, stamped, 1, "the session carries the agent process an outsider will try to forge")
	// The identity works for its owner: the account's own agent binds to it.
	finBound := paClientHeaders(t, d.peers[paUIDFinance].URL, "", map[string]string{mcp.AgentProcessHeader: paFinanceProc, mcp.HostHeader: paHost})
	text, isErr := paCall(t, finBound, "memory_read", map[string]any{"name": d.sealed.memName})
	require.False(t, isErr, text)
	require.Contains(t, text, paSecret)
	d.hook(t, d.peers[paUIDFinance].URL, "", "user-prompt-submit", map[string]any{
		"session_id": d.claudeID, "cwd": "/opt/arctop/finance/app", "prompt": "check the " + paSecret + " ledger",
	})

	// A second agent session of the account that has ENDED, with findings: an
	// outsider's hook naming it must not reactivate or re-scope it (which would
	// carry its findings out of the seal).
	d.endedClaudeID = "fin-claude-0002"
	d.hook(t, d.peers[paUIDFinance].URL, "", "session-start", map[string]any{
		"session_id": d.endedClaudeID, "cwd": "/opt/arctop/finance/app", "source": "startup",
	})
	ended, ok, err := store.AmbientSessionByExternalIdentity(ctx, d.db, "claude-code", d.endedClaudeID)
	require.NoError(t, err)
	require.True(t, ok)
	paCallOK(t, fin, "session_end", map[string]any{"session_id": ended.ID, "findings": paSearch + " " + paSecret + " closing findings"})

	// The gardener runs for no caller, and the event it records when it raises
	// a proposal carries the proposal's key -- a memory-wanted key embeds the
	// recall query an agent missed on, so it is the sealed account's content.
	// Recorded here exactly as gardener.Service.record does it.
	wanted, err := store.CreateProposal(ctx, d.db, store.ProposalMemoryWanted, map[string]any{
		"project": paSealed, "query": paSecret + " ledger", "sessions": 3,
	})
	require.NoError(t, err)
	d.gardenerEventID, err = d.rec.Record(ctx, core.Event{Kind: core.EventGardenerAction, ItemID: wanted.ID,
		Payload: map[string]any{"action": "propose", "kind": store.ProposalMemoryWanted, "key": "memory_wanted:" + paSealed + ":" + paSecret + "-ledger"}})
	require.NoError(t, err)

	// Every event the sealed account caused is filed under its project.
	rows, err := d.db.QueryContext(ctx, `SELECT id, project_slug, kind FROM events WHERE payload LIKE ?`, "%"+paSecret+"%")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var id, project, kind string
		require.NoError(t, rows.Scan(&id, &project, &kind))
		if kind == string(core.EventGardenerAction) {
			continue // recorded for no caller; the console hides it by the proposal it is about
		}
		require.Equal(t, paSealed, project, "event %s (%s) carries sealed content outside the sealed project", id, kind)
		d.eventID = id
		n++
	}
	require.NoError(t, rows.Err())
	require.Positive(t, n, "the sealed account's calls left events")

	// The owner's own project, which no sealed account may reach, and a global
	// memory, which every account reads.
	own := paClient(t, d.owner.URL, paKey)
	d.ops = d.plantItems(t, own, paOpen, paOwnSecret, "ops")
	paCallOK(t, own, "memory_write", map[string]any{"project": "global", "name": "house-style", "kind": "convention",
		"description": "global " + paSearch, "body": "global body " + paGlobalToken})
}

func (d *paDaemon) hook(t *testing.T, base, key, event string, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	target := base + "/api/hooks/" + event
	if event == "" {
		target = base // a full URL with its own query
	}
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(string(raw)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "hook %s: %s", event, out)
	return string(out)
}

// paToolProbes is every way a caller outside it might try to reach it's
// project through each MCP tool. TestPeerAuthMatrix fails when a tool in
// mcp.Catalog() has no row: a new tool is a new surface to prove.
func paToolProbes(it paItems) map[string][]map[string]any {
	edit := []any{map[string]any{"old_string": "ledger", "new_string": "LEDGER"}}
	return map[string][]map[string]any{
		"session_start":  {{"project": it.project}, {"name": it.sessName}, {"cwd": "/work/finance-os"}, {"cwd": "/work/ops"}},
		"session_update": {{"session": it.sessName, "findings": "outsider overwrite"}, {"session_id": it.sessID, "findings": "outsider overwrite"}},
		"session_end":    {{"session": it.sessName, "findings": "outsider end"}, {"session_id": it.sessID, "findings": "outsider end"}},
		"memory_write": {
			{"project": it.project, "name": "probe-in", "kind": "gotcha", "description": "probe", "body": "probe"},
			{"name": "probe-supersede", "kind": "gotcha", "description": "probe", "body": "probe", "supersedes": it.memName, "project": it.project},
		},
		"memory_append": {{"project": it.project, "name": it.memName, "body": "appended"}, {"name": it.memName, "body": "appended"}},
		"memory_edit":   {{"project": it.project, "name": it.memName, "edits": edit}, {"name": it.memName, "edits": edit}},
		"memory_read":   {{"project": it.project, "name": it.memName}, {"id": it.memID}, {"name": it.memName}},
		"memory_delete": {{"project": it.project, "name": it.memName}, {"name": it.memName}},
		"recall": {
			{"query": paSearch}, {"query": paSearch, "project": it.project}, {"query": "ledger reconcile memo"},
			{"kind": "gotcha"}, {"kind": "gotcha", "project": it.project}, {"query": paSearch, "scope": "sessions"},
			{"query": paSearch, "scope": "tasks"}, {"query": paSearch, "scope": "trials"}, {"query": paSearch, "scope": "notes"},
		},
		"notes_create":   {{"project": it.project, "title": "probe", "body": "probe"}},
		"notes_read":     {{"id": it.noteID}, {"slug": it.noteSlug, "project": it.project}, {"slug": it.noteSlug}},
		"notes_update":   {{"id": it.noteID, "body": "outsider rewrite"}, {"id": it.noteID, "project": "global"}},
		"notes_edit":     {{"id": it.noteID, "edits": []any{map[string]any{"old_string": "memo", "new_string": "MEMO"}}}},
		"notes_append":   {{"id": it.noteID, "body": "outsider append"}},
		"notes_delete":   {{"id": it.noteID}},
		"project_list":   {{}},
		"project_create": {{"name": "probe", "slug": it.project}},
		"tasks_add":      {{"project": it.project, "title": "probe"}},
		"tasks_update": {
			{"id": it.taskID, "status": "done"}, {"id": it.taskID, "project": "global"},
			{"id": it.taskID, "title": "outsider retitle"},
		},
		"tasks_ready":        {{"project": it.project}, {"project": it.project, "plan": it.plan}, {}},
		"tasks_list":         {{"project": it.project}, {"id": it.taskID}, {"project": it.project, "plan": it.plan}, {}},
		"tasks_claim":        {{"id": it.taskID}},
		"tasks_release":      {{"id": it.taskID}},
		"lab_open":           {{"lab": it.lab}},
		"trial_record":       {{"lab": it.lab, "project": it.project, "title": "probe"}},
		"trial_query":        {{"lab": it.lab}, {}},
		"gardener_proposals": {{}, {"kind": "archive"}},
		"gardener_request":   {{"request": "tidy", "project": it.project}, {"request": "tidy", "project": "all"}},
		"gardener_split":     {{"source": it.project}},
		"gardener_apply":     {{"id": it.proposalID}, {"id": it.proposalID, "action": "dismiss"}, {"id": it.proposalID, "action": "hide"}},
		"capture_url":        {{"url": "https://example.invalid/x", "project": it.project}},
		"usage_summary":      {{}},
		"favorite_set": {
			{"kind": "memory", "id": it.memID, "favorite": true}, {"kind": "note", "id": it.noteID, "favorite": true},
			{"kind": "task", "id": it.taskID, "favorite": true}, {"kind": "trial", "id": it.trialID, "favorite": true},
			{"kind": "session", "id": it.sessID, "favorite": true}, {"kind": "project", "id": it.project, "favorite": true},
			{"kind": "plan", "id": it.plan, "favorite": true},
		},
	}
}

// unchanged asserts one project's planted content survived, read straight from
// the database: no outsider wrote, moved, starred, claimed, ended or resolved
// any of it.
func (d *paDaemon) unchanged(t *testing.T, it paItems, after string) {
	t.Helper()
	ctx := context.Background()
	mem, ok, err := store.MemoryByID(ctx, d.db, it.memID)
	require.NoError(t, err)
	require.True(t, ok, "%s: the memory still exists", after)
	require.Nil(t, mem.InvalidAt, "%s: the memory is still active", after)
	require.False(t, mem.Favorite, "%s: no outsider starred it", after)
	require.Equal(t, it.project, mem.Project, after)
	body, err := os.ReadFile(filepath.Join(d.dir, mem.FilePath))
	require.NoError(t, err)
	require.Contains(t, string(body), "ledger rule", "%s: the memory body was not edited", after)
	require.NotContains(t, string(body), "appended", after)
	require.NotContains(t, string(body), "LEDGER", after)

	note, ok, err := store.NoteByID(ctx, d.db, it.noteID)
	require.NoError(t, err)
	require.True(t, ok, "%s: the note still exists", after)
	require.Equal(t, it.project, note.Project, after)
	require.False(t, note.Favorite, after)
	nb, err := os.ReadFile(filepath.Join(d.dir, note.FilePath))
	require.NoError(t, err)
	require.NotContains(t, string(nb), "outsider", after)
	require.NotContains(t, string(nb), "MEMO", after)

	task, err := store.TaskByID(ctx, d.db, it.taskID)
	require.NoError(t, err)
	require.Equal(t, core.TaskInProgress, task.Status, "%s: the task is still claimed", after)
	require.Equal(t, it.project, task.ProjectSlug, after)
	require.Equal(t, it.sessID, task.ClaimedBy, "%s: by its own agent", after)
	require.NotContains(t, task.Title, "outsider", after)
	require.False(t, task.Favorite, after)

	sess, ok, err := store.SessionByID(ctx, d.db, it.sessID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionActive, sess.Status, "%s: no outsider ended the session", after)
	require.NotContains(t, sess.Findings, "outsider", after)
	require.Equal(t, it.project, sess.ProjectSlug, after)

	p, ok, err := store.ProposalByID(ctx, d.db, it.proposalID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, store.ProposalPending, p.Status, "%s: no outsider resolved the proposal", after)

	proj, ok, err := store.ProjectBySlug(ctx, d.db, it.project)
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, proj.Favorite, after)

	for _, probe := range []string{"probe-in", "probe-supersede"} {
		_, found, err := store.MemoryByName(ctx, d.db, it.project, probe)
		require.NoError(t, err)
		require.False(t, found, "%s: an outsider wrote %s into %s", after, probe, it.project)
	}
	var n int
	require.NoError(t, d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes_index WHERE project = ? AND title = 'probe'`, it.project).Scan(&n))
	require.Zero(t, n, after)
	require.NoError(t, d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE project_slug = ? AND title = 'probe'`, it.project).Scan(&n))
	require.Zero(t, n, after)
	require.NoError(t, d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM trials WHERE project_slug = ? AND title = 'probe'`, it.project).Scan(&n))
	require.Zero(t, n, after)
}

// sealedIsolation is the isolation the fixture gave the sealed project; a
// probe that loosened it is a write across the seal.
func (d *paDaemon) sealedIsolation(t *testing.T) {
	t.Helper()
	proj, ok, err := store.ProjectBySlug(context.Background(), d.db, paSealed)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.IsolationConfidential, proj.Isolation)
}

func TestPeerAuthMatrix(t *testing.T) {
	d := newPADaemon(t)
	d.plant(t)

	// Console and live feed first: the owner's own MCP probes below put the
	// sealed items' identifiers in the owner's own event log (they are in the
	// probes' arguments), which is the owner's data -- but paSecret never is.
	t.Run("console GET routes as the owner", func(t *testing.T) { d.crawlConsole(t, true) })
	t.Run("console writes as the owner", func(t *testing.T) { d.consoleWrites(t) })
	t.Run("live feed as the owner", func(t *testing.T) { d.liveFeed(t) })
	t.Run("a2a", func(t *testing.T) { d.a2aProbes(t) })
	t.Run("hooks", func(t *testing.T) { d.hookProbes(t) })

	for _, tool := range mcp.Catalog() {
		require.Contains(t, paToolProbes(d.sealed), tool.Name, "mcp.Catalog() tool %s has no peer_auth probe row: add one", tool.Name)
	}
	forged := map[string]string{mcp.AgentProcessHeader: paFinanceProc, mcp.HostHeader: paHost}
	for _, who := range []struct {
		name   string
		base   string
		key    string
		target paItems
		extra  map[string]string
	}{
		{"owner over TCP, probing the sealed project", d.owner.URL, paKey, d.sealed, nil},
		{"owner over TCP forging the sealed agent's process identity", d.owner.URL, paKey, d.sealed, forged},
		{"owner over the socket, probing the sealed project", d.peers[paUIDOwner].URL, "", d.sealed, nil},
		{"another sealed account, probing the sealed project", d.peers[paUIDLegal].URL, "", d.sealed, nil},
		{"another sealed account forging the sealed agent's process identity", d.peers[paUIDLegal].URL, "", d.sealed, forged},
		{"the sealed account, probing the owner's project", d.peers[paUIDFinance].URL, "", d.ops, nil},
		{"another sealed account, probing the owner's project", d.peers[paUIDLegal].URL, "", d.ops, nil},
	} {
		t.Run("mcp: "+who.name, func(t *testing.T) {
			cli := paClientHeaders(t, who.base, who.key, who.extra)
			probes := paToolProbes(who.target)
			for _, tool := range mcp.Catalog() {
				for i, args := range probes[tool.Name] {
					sessions := d.sessionsIn(t, who.target.project)
					text, _ := paCall(t, cli, tool.Name, args)
					what := fmt.Sprintf("%s probe %d (%v)", tool.Name, i, args)
					require.Equal(t, sessions, d.sessionsIn(t, who.target.project), "%s started a session in %s", what, who.target.project)
					require.NotContains(t, text, who.target.secret, "%s leaked content", what)
					d.noIdentifiers(t, who.target, text, mustJSON(t, args), "%s", what)
					d.unchanged(t, who.target, what)
				}
			}
			// Listings name nothing behind the boundary, not even the project.
			text, _ := paCall(t, cli, "project_list", nil)
			require.NotContains(t, text, `"slug":"`+who.target.project+`"`)
			text, _ = paCall(t, cli, "recall", map[string]any{"query": paSearch})
			require.NotContains(t, text, who.target.memName)
			d.sealedIsolation(t)
		})
	}

	t.Run("the sealed account sees its own project and global", func(t *testing.T) {
		fin := paClient(t, d.peers[paUIDFinance].URL, "")
		for _, c := range []struct {
			tool string
			args map[string]any
		}{
			{"memory_read", map[string]any{"name": d.sealed.memName}},
			{"memory_read", map[string]any{"id": d.sealed.memID}},
			{"recall", map[string]any{"query": paSearch}},
			{"notes_read", map[string]any{"id": d.sealed.noteID}},
			{"tasks_list", map[string]any{"id": d.sealed.taskID}},
			{"trial_query", map[string]any{"lab": d.sealed.lab}},
			{"lab_open", map[string]any{"lab": d.sealed.lab}},
			{"gardener_proposals", map[string]any{}},
		} {
			text, isErr := paCall(t, fin, c.tool, c.args)
			require.False(t, isErr, "%s: %s", c.tool, text)
			require.Contains(t, text, paSecret, "%s %v: the sealed account must reach its own content", c.tool, c.args)
		}
		text, _ := paCall(t, fin, "project_list", nil)
		require.Contains(t, text, `"slug":"`+paSealed+`"`)
		require.NotContains(t, text, `"slug":"`+paOpen+`"`, "a sealed account lists only its own projects")
		require.NotContains(t, text, `"slug":"`+paLegal+`"`)
		text, isErr := paCall(t, fin, "memory_write", map[string]any{"project": "global", "name": "fin-global", "kind": "gotcha", "description": "x", "body": "x"})
		require.True(t, isErr, "a sealed account never writes the global scope: %s", text)
		text, _ = paCall(t, fin, "recall", map[string]any{"query": paGlobalToken})
		require.Contains(t, text, "house-style", "global items stay visible to every account")
		text, _ = paCall(t, fin, "memory_read", map[string]any{"name": "house-style"})
		require.Contains(t, text, paGlobalToken, "a global memory reads in full")
	})

	t.Run("an unconfigured uid is refused everywhere", func(t *testing.T) {
		base := d.peers[paUIDStranger].URL
		for _, path := range []string{"/api/mcp", "/api/hooks/session-start", "/console/", "/api/a2a", "/healthz"} {
			resp, err := http.Post(base+path, "application/json", strings.NewReader(`{}`))
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusForbidden, resp.StatusCode, path)
		}
	})

	t.Run("a configured account has no console", func(t *testing.T) {
		resp, err := http.Get(d.peers[paUIDFinance].URL + "/console/")
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	// Every route the owner exercised, once more, after the MCP probes: the
	// sealed content is still absent from every console page.
	t.Run("console GET routes after the probes", func(t *testing.T) { d.crawlConsole(t, false) })
}

// noIdentifiers fails when text names one of it's items, unless the caller
// supplied that identifier itself (said, the probe's own arguments or URL): an
// error that echoes what the caller asked for discloses nothing it did not
// already hold.
func (d *paDaemon) noIdentifiers(t *testing.T, it paItems, text, said string, format string, args ...any) {
	t.Helper()
	for _, ident := range []string{it.memID, it.memName, it.noteID, it.noteSlug, it.taskID, it.trialID, it.sessID, it.sessName} {
		if ident == "" || strings.Contains(said, ident) {
			continue
		}
		require.NotContains(t, text, ident, "%s named an item behind the boundary", fmt.Sprintf(format, args...))
	}
	if it.project == paSealed && !strings.Contains(said, d.claudeID) {
		require.NotContains(t, text, d.claudeID, "%s named the sealed agent session", fmt.Sprintf(format, args...))
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}

// consoleRoutes reads the console's route table from its source, so the crawl
// below cannot fall behind a route added later.
func consoleRoutes(t *testing.T) (gets, posts []string) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "internal", "console", "console.go"))
	require.NoError(t, err)
	re := regexp.MustCompile(`(?:handle|post)\("(GET|POST) (/console/[^"]*)"`)
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		if m[1] == "GET" {
			gets = append(gets, m[2])
		} else {
			posts = append(posts, m[2])
		}
	}
	require.NotEmpty(t, gets)
	require.NotEmpty(t, posts)
	return gets, posts
}

// fill substitutes the sealed project's identifiers into a route pattern.
func (d *paDaemon) fill(pattern string) string {
	it := d.sealed
	p := strings.TrimSuffix(pattern, "{$}")
	switch {
	case strings.HasPrefix(p, "/console/sessions/"):
		p = strings.Replace(p, "{id}", it.sessID, 1)
	case strings.HasPrefix(p, "/console/memories/"):
		p = strings.Replace(p, "{id}", it.memID, 1)
	case strings.HasPrefix(p, "/console/notes/"):
		p = strings.Replace(p, "{id}", it.noteID, 1)
	case strings.HasPrefix(p, "/console/tasks/"):
		p = strings.Replace(p, "{id}", it.taskID, 1)
	case strings.HasPrefix(p, "/console/trials/"):
		p = strings.Replace(p, "{id}", it.trialID, 1)
	case strings.HasPrefix(p, "/console/gardener/"):
		p = strings.Replace(p, "{id}", it.proposalID, 1)
		p = strings.Replace(p, "{slug}", it.plan, 1)
	case strings.HasPrefix(p, "/console/events/"):
		p = strings.Replace(p, "{id}", d.eventID, 1)
	case strings.HasPrefix(p, "/console/plans/"):
		p = strings.Replace(p, "{slug}", it.plan, 1)
	case strings.HasPrefix(p, "/console/projects/"):
		p = strings.Replace(p, "{slug}", paSealed, 1)
	case strings.HasPrefix(p, "/console/labs/"):
		p = strings.Replace(p, "{name...}", it.lab, 1)
	case strings.HasPrefix(p, "/console/favorites/"):
		p = strings.Replace(strings.Replace(p, "{kind}", "memory", 1), "{id}", it.memID, 1)
	}
	return p
}

func (d *paDaemon) ownerGet(t *testing.T, target string, asJSON bool) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, d.owner.URL+target, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+paKey)
	if asJSON {
		req.Header.Set("Accept", "application/json")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// crawlConsole fetches every console GET route, HTML and JSON, as the owner.
// identifiers also checks for the sealed items' names and ids; the second crawl
// runs after the owner's own MCP probes, whose arguments (in the owner's own
// Interactions feed) name them, so it checks content alone.
func (d *paDaemon) crawlConsole(t *testing.T, identifiers bool) {
	gets, _ := consoleRoutes(t)
	it := d.sealed
	var targets []string
	for _, pattern := range gets {
		if pattern == "/console/events" {
			continue // the live feed streams; liveFeed covers it
		}
		if pattern == "/console/" {
			pattern = "/console/no-such-page"
		}
		targets = append(targets, d.fill(pattern))
	}
	q := url.QueryEscape
	targets = append(targets,
		"/console/search?q="+q(paSearch), "/console/search?q="+q(it.memName), "/console/search?q="+q(it.memID),
		"/console/search?q="+q(paSearch)+"&scope=sessions", "/console/search?q="+q(paSearch)+"&scope=tasks",
		"/console/memories?project="+paSealed, "/console/notes?project="+paSealed, "/console/sessions?project="+paSealed,
		"/console/tasks?project="+paSealed, "/console/interactions?project="+paSealed, "/console/interactions",
		"/console/context?project="+paSealed, "/console/settings/briefing/preview?project="+paSealed,
		"/console/retrieval?project="+paSealed, "/console/now?project="+paSealed, "/console/plans?project="+paSealed,
		"/console/projects/"+paSealed+"?tab=memories", "/console/projects/"+paSealed+"?tab=plans",
		"/console/labs/"+it.lab, "/console/trials?lab="+it.lab, "/console/gardener?kind=archive",
		"/console/memories?q="+q(paSearch), "/console/notes?q="+q(paSearch), "/console/since?days=30",
		"/console/events/"+d.gardenerEventID,
	)
	sawOps := false
	for _, target := range targets {
		for _, asJSON := range []bool{false, true} {
			status, body := d.ownerGet(t, target, asJSON)
			require.Less(t, status, 500, "GET %s (json=%v) failed: %s", target, asJSON, firstN(body, 300))
			require.NotContains(t, body, paSecret, "GET %s (json=%v) leaked sealed content", target, asJSON)
			if identifiers {
				d.noIdentifiers(t, it, body, target, "GET %s (json=%v)", target, asJSON)
			}
			sawOps = sawOps || strings.Contains(body, paOwnSecret)
		}
	}
	require.True(t, sawOps, "the crawl reached the owner's own content somewhere: it is not an empty console")
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (d *paDaemon) consoleWrites(t *testing.T) {
	_, posts := consoleRoutes(t)
	it := d.sealed
	forms := map[string]url.Values{
		"/console/projects/{slug}/isolation":   {"state": {"open"}, "confirm": {"1"}},
		"/console/favorites/{kind}/{id}":       {"favorite": {"1"}},
		"/console/gardener/{id}/retarget":      {"to": {paOpen}},
		"/console/gardener/request":            {"request": {"tidy " + paSealed}, "project": {paSealed}},
		"/console/gardener/split":              {"source": {paSealed}},
		"/console/settings/families/save":      {"name": {"fam"}, "members": {paSealed + "," + paOpen}},
		"/console/settings/briefing/preview":   {"project": {paSealed}},
		"/console/gardener/group/dismiss":      {"kind": {"archive"}},
		"/console/settings/level":              {"level": {"advanced"}},
		"/console/settings/features":           {"feature_research": {"1"}, "feature_momentum": {"1"}, "feature_gamification": {"1"}},
		"/console/settings/embeddings/mode":    {"mode": {"auto"}},
		"/console/settings/embeddings/reembed": {},
	}
	// No redirect following: a 303 to a page is not the write's outcome, and
	// the state check after each POST is what decides.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, pattern := range posts {
		if pattern == "/console/login" || strings.HasPrefix(pattern, "/console/settings/updates") {
			continue
		}
		targets := []string{d.fill(pattern)}
		if pattern == "/console/favorites/{kind}/{id}" {
			for kind, id := range map[string]string{"note": it.noteID, "task": it.taskID, "session": it.sessID, "trial": it.trialID, "project": paSealed, "plan": it.plan} {
				targets = append(targets, "/console/favorites/"+kind+"/"+id)
			}
		}
		for _, target := range targets {
			for _, asJSON := range []bool{false, true} {
				form := forms[pattern]
				sessions := d.sessionsIn(t, paSealed)
				req, err := http.NewRequest(http.MethodPost, d.owner.URL+target, strings.NewReader(form.Encode()))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer "+paKey)
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if asJSON {
					req.Header.Set("Accept", "application/json")
				}
				resp, err := client.Do(req)
				require.NoError(t, err)
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				require.NoError(t, err)
				require.NotContains(t, string(body), paSecret, "POST %s leaked sealed content", target)
				require.NotContains(t, resp.Header.Get("Location"), paSecret, "POST %s redirected with sealed content", target)
				d.unchanged(t, it, "POST "+target)
				d.sealedIsolation(t)
				require.Equal(t, sessions, d.sessionsIn(t, paSealed), "POST %s started a session in the sealed project", target)
			}
		}
	}
	fams, err := store.ProjectFamilies(context.Background(), d.db)
	require.NoError(t, err)
	for name, members := range fams {
		require.NotContains(t, members, paSealed, "family %s took in the sealed project", name)
	}
}

// liveFeed subscribes to the console's live feed as the owner, has the sealed
// account act, then has the owner act as a marker: every frame up to the
// marker must be free of sealed content (events reach a subscriber in order).
func (d *paDaemon) liveFeed(t *testing.T) {
	for _, feed := range []string{"", "?feed=interactions"} {
		ctx, cancel := context.WithCancel(context.Background())
		// Registered before anything can fail: an open stream left behind would
		// hold httptest.Server.Close forever.
		t.Cleanup(cancel)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.owner.URL+"/console/events"+feed, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+paKey)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		require.Equal(t, http.StatusOK, resp.StatusCode)
		frames := make(chan string, 256)
		go func() {
			defer close(frames)
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
			for sc.Scan() {
				frames <- sc.Text()
			}
		}()
		// Wait for the stream to be live before anyone acts.
		require.Eventually(t, func() bool {
			select {
			case f := <-frames:
				return strings.HasPrefix(f, ": connected")
			default:
				return false
			}
		}, 5*time.Second, 10*time.Millisecond)

		fin := paClient(t, d.peers[paUIDFinance].URL, "")
		_, _ = paCall(t, fin, "recall", map[string]any{"query": paSecret + " live", "project": paSealed})
		paCallOK(t, fin, "memory_write", map[string]any{"name": "fin-live" + strings.TrimPrefix(feed, "?feed="), "kind": "gotcha",
			"description": "live " + paSecret, "body": "live " + paSecret})
		// The gardener, for no caller, about a sealed proposal.
		_, err = d.rec.Record(context.Background(), core.Event{Kind: core.EventGardenerAction, ItemID: d.sealed.proposalID,
			Payload: map[string]any{"action": "dismiss", "kind": "archive", "key": "archive:" + d.sealed.memID + ":" + paSecret}})
		require.NoError(t, err)
		marker := "owner-marker-summary"
		if feed != "" {
			marker = "owner-marker-interactions"
		}
		own := paClient(t, d.owner.URL, paKey)
		paCallOK(t, own, "memory_write", map[string]any{"project": paOpen, "name": marker, "kind": "gotcha", "description": marker, "body": marker})

		deadline := time.After(10 * time.Second)
	read:
		for {
			select {
			case f, open := <-frames:
				require.True(t, open, "feed closed before the marker")
				require.NotContains(t, f, paSecret, "the live feed carried sealed content")
				require.NotContains(t, f, "fin-live", "the live feed named a sealed memory")
				d.noIdentifiers(t, d.sealed, f, "", "live feed frame")
				if strings.Contains(f, marker) {
					break read
				}
			case <-deadline:
				t.Fatalf("the owner's marker event never reached the feed (%s)", feed)
			}
		}
		cancel()
	}
}

func (d *paDaemon) a2aProbes(t *testing.T) {
	send := func(base, key, query string, meta map[string]any) string {
		msg := map[string]any{"role": "user", "kind": "message", "messageId": "m1",
			"parts": []map[string]any{{"kind": "text", "text": query}}}
		if meta != nil {
			msg["metadata"] = meta
		}
		raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": map[string]any{"message": msg}})
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost, base+"/api/a2a", strings.NewReader(string(raw)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return string(body)
	}
	for _, who := range []struct{ base, key string }{{d.owner.URL, paKey}, {d.peers[paUIDLegal].URL, ""}} {
		require.NotContains(t, send(who.base, who.key, paSearch, map[string]any{"project": paSealed}), paSecret)
		require.NotContains(t, send(who.base, who.key, paSearch, nil), paSecret)
		require.NotContains(t, send(who.base, who.key, "ledger memo reconcile", map[string]any{"scope": "all"}), paSecret)
	}
	fin := d.peers[paUIDFinance].URL
	require.Contains(t, send(fin, "", paSearch, nil), paSecret, "the sealed account recalls its own project over A2A")
	require.NotContains(t, send(fin, "", paSearch, map[string]any{"project": paOpen}), paOwnSecret)
	require.NotContains(t, send(fin, "", paSearch, nil), paOwnSecret)
	require.Contains(t, send(d.owner.URL, paKey, paSearch, map[string]any{"project": paOpen}), paOwnSecret, "the owner recalls its own project")
}

func (d *paDaemon) hookProbes(t *testing.T) {
	ctx := context.Background()
	// The owner opening the sealed project's repository: a notice, no content,
	// no session.
	sealedSessions := d.sessionsIn(t, paSealed)
	defer func() {
		require.Equal(t, sealedSessions, d.sessionsIn(t, paSealed), "no outsider's hook started a session in the sealed project")
	}()
	out := d.hook(t, d.owner.URL, paKey, "session-start", map[string]any{
		"session_id": "owner-claude-0001", "cwd": "/work/finance-os", "source": "startup",
	})
	require.NotContains(t, out, paSecret)
	require.Contains(t, out, "sealed")
	_, found, err := store.AmbientSessionByExternalIdentity(ctx, d.db, "claude-code", "owner-claude-0001")
	require.NoError(t, err)
	require.False(t, found, "the owner gets no session inside the seal")
	out = d.hook(t, d.owner.URL, paKey, "user-prompt-submit", map[string]any{
		"session_id": "owner-claude-0001", "cwd": "/work/finance-os", "prompt": "ledger memo reconcile " + paSearch,
	})
	require.NotContains(t, out, paSecret)

	// The owner naming the sealed account's own agent session: nothing moves.
	before, _, err := store.AmbientSessionByExternalIdentity(ctx, d.db, "claude-code", d.claudeID)
	require.NoError(t, err)
	for _, ev := range []string{"session-start", "user-prompt-submit", "stop", "session-end", "post-tool-use", "permission-request", "subagent-start", "subagent-stop"} {
		out := d.hook(t, d.owner.URL, paKey, ev, map[string]any{
			"session_id": d.claudeID, "cwd": "/work/ops", "source": "resume", "prompt": "ledger " + paSearch,
			"reason": "other", "agent_id": "a1", "agent_type": "general", "tool_name": "ExitPlanMode",
		})
		require.NotContains(t, out, paSecret, "hook %s", ev)
		after, _, err := store.AmbientSessionByExternalIdentity(ctx, d.db, "claude-code", d.claudeID)
		require.NoError(t, err)
		require.Equal(t, paSealed, after.ProjectSlug, "hook %s: the owner cannot re-scope the sealed account's session", ev)
		require.Equal(t, core.SessionActive, after.Status, "hook %s: the owner cannot end the sealed account's session", ev)
		require.Equal(t, before.Findings, after.Findings, "hook %s", ev)
	}

	// The owner naming the sealed account's ENDED session: it stays ended, in
	// its project, with its findings.
	endedBefore, _, err := store.AmbientSessionByExternalIdentity(ctx, d.db, "claude-code", d.endedClaudeID)
	require.NoError(t, err)
	require.Equal(t, core.SessionCompleted, endedBefore.Status)
	for _, ev := range []string{"session-start", "user-prompt-submit", "stop", "session-end"} {
		out := d.hook(t, d.owner.URL, paKey, ev, map[string]any{
			"session_id": d.endedClaudeID, "cwd": "/work/ops", "source": "resume", "prompt": "ledger " + paSearch,
		})
		require.NotContains(t, out, paSecret, "hook %s", ev)
		after, _, err := store.AmbientSessionByExternalIdentity(ctx, d.db, "claude-code", d.endedClaudeID)
		require.NoError(t, err)
		require.Equal(t, paSealed, after.ProjectSlug, "hook %s: an ended sealed session cannot be re-scoped", ev)
		require.Equal(t, core.SessionCompleted, after.Status, "hook %s: an ended sealed session cannot be revived by an outsider", ev)
		require.Equal(t, endedBefore.Findings, after.Findings, "hook %s", ev)
	}

	// Another sealed account in the owner's repository lands in its OWN project
	// and sees none of the owner's.
	out = d.hook(t, d.peers[paUIDLegal].URL, "", "session-start", map[string]any{
		"session_id": "legal-claude-0001", "cwd": "/work/ops", "source": "startup",
	})
	require.NotContains(t, out, paSecret)
	require.NotContains(t, out, paOwnSecret)
	legal, found, err := store.AmbientSessionByExternalIdentity(ctx, d.db, "claude-code", "legal-claude-0001")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, paLegal, legal.ProjectSlug)
	out = d.hook(t, d.peers[paUIDLegal].URL, "", "user-prompt-submit", map[string]any{
		"session_id": "legal-claude-0001", "cwd": "/work/ops", "prompt": "ledger memo reconcile " + paSearch,
	})
	require.NotContains(t, out, paOwnSecret)
	require.NotContains(t, out, paSecret)
}
