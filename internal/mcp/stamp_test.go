package mcp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// Provenance stamps at the tool surface (isolation.requests, plan
// fenced-sessions P6): every item a conversation writes while its label holds
// a fenced project other than the destination records that it may contain
// content from it, and what cleared the write -- memories and notes in their
// frontmatter (mirrored into item_provenance), tasks and session findings in
// the index alone. Set from the conversation's state, never from arguments;
// no write removes one; supersession keeps the union; off, nothing changes.

const (
	stOps   = "example-ops"     // open
	stFin   = "example-finance" // confidential: the grant target
	stLegal = "example-legal"   // confidential: the conversation's home
	stVault = "example-vault"   // sealed
	stProc  = "6161.42"
	stBoot  = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"
)

var stampShort = regexp.MustCompile(`R-[0-9A-Z]{4}`)

// stampRig is a daemon's MCP server over a fresh database with an open, two
// confidential and a sealed project; on wires isolation.requests (advisory
// grants), view attaches a peer_auth View to every request.
type stampRig struct {
	t      *testing.T
	db     *sql.DB
	mgr    *files.Manager
	grants *approval.Grants
	url    string
	now    time.Time
}

func newStampRig(t *testing.T, on bool, view func() access.View) *stampRig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	for slug, state := range map[string]core.Isolation{stOps: core.IsolationOpen, stFin: core.IsolationConfidential,
		stLegal: core.IsolationConfidential, stVault: core.IsolationSealed} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, state))
	}
	r := &stampRig{t: t, db: db, mgr: mgr, now: time.Now().UTC().Truncate(time.Second)}
	rec := events.NewRecorder(db)
	ret := retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 4000}, nil)
	ret.SetBodyReader(mgr.Store())
	cfg := mcpserver.Config{DB: db, Files: mgr, Retrieve: ret, Events: rec, APIKey: testKey,
		Features: config.Features{Research: true}, Now: func() time.Time { return r.now }}
	if on {
		r.grants = approval.NewGrants(approval.GrantsConfig{DB: db, Events: rec, LeaseCap: 8 * time.Hour,
			Now: func() time.Time { return r.now }})
		cfg.Grants = r.grants
	}
	h := mcpserver.New(cfg).Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if view != nil {
			req = req.WithContext(access.WithView(req.Context(), view()))
		}
		h.ServeHTTP(w, req)
	}))
	t.Cleanup(ts.Close)
	r.url = ts.URL
	return r
}

// agent is an identified conversation native to home: the ambient row the
// SessionStart hook leaves, and a connection naming its process.
func (r *stampRig) agent(name, home, proc string) (*mcpclient.Client, string) {
	r.t.Helper()
	ctx := context.Background()
	id := seedAgentAmbient(r.t, ctx, r.db, name, home, "/work/"+home, proc)
	return dialAgent(r.t, ctx, r.url, proc), id
}

// owner is a connection that names no conversation, bound to a session of its
// own in project: it holds nothing but that project.
func (r *stampRig) owner(project, name string) *mcpclient.Client {
	r.t.Helper()
	ctx := context.Background()
	id, err := core.NewID()
	require.NoError(r.t, err)
	require.NoError(r.t, store.CreateSession(ctx, r.db, core.Session{ID: id, Name: name, ProjectSlug: project,
		Status: core.SessionActive, CWD: "/work/" + project, CreatedAt: r.now, UpdatedAt: r.now}))
	cli := dialClient(r.t, ctx, r.url, testKey)
	stampOK(r.t, cli, "session_start", map[string]any{"name": name})
	return cli
}

// grant approves a grant of target asked from the conversation's home (from),
// held by the conversation (name's external id), or by the process key under
// peer_auth, as the approve core records one.
func (r *stampRig) grant(sessionID, name, target, from string, write bool, process access.ProcessToken) store.AccessGrant {
	r.t.Helper()
	ctx := context.Background()
	req := store.GrantRequest{Project: target, From: from, Lease: time.Hour, TTL: 10 * time.Minute,
		SessionID: sessionID, SessionName: name}
	if process.PID != 0 {
		req.ProcessKey, req.PeerPID, req.PeerUID = process.Key(), process.PID, 501
	} else {
		req.ConversationClient, req.ConversationID = "claude-code", name+"-full"
	}
	a, err := store.CreateGrantRequest(ctx, r.db, req, r.now)
	require.NoError(r.t, err)
	accessWord := store.GrantAccessRead
	if write {
		accessWord = store.GrantAccessWrite
	}
	g, err := store.GrantAccessApprove(ctx, r.db, a.ID, a.Challenge, "owner", 0, accessWord, r.now)
	require.NoError(r.t, err)
	return g
}

func (r *stampRig) memory(project, name string) core.Memory {
	r.t.Helper()
	m, err := r.mgr.Store().ReadMemory(files.MemoryRelPath(project, name))
	require.NoError(r.t, err)
	return m
}

func (r *stampRig) note(id string) core.Note {
	r.t.Helper()
	idx, ok, err := store.NoteByID(context.Background(), r.db, id)
	require.NoError(r.t, err)
	require.True(r.t, ok)
	n, err := r.mgr.Store().ReadNote(idx.FilePath)
	require.NoError(r.t, err)
	return n
}

func (r *stampRig) rows(kind, id string) []store.ProvenanceStamp {
	r.t.Helper()
	got, err := store.ProvenanceForItem(context.Background(), r.db, kind, id)
	require.NoError(r.t, err)
	return got
}

func (r *stampRig) count() int {
	r.t.Helper()
	var n int
	require.NoError(r.t, r.db.QueryRow(`SELECT count(*) FROM item_provenance`).Scan(&n))
	return n
}

func stampCall(t *testing.T, cli *mcpclient.Client, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cli.CallTool(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	return tc.Text, res.IsError
}

func stampOK(t *testing.T, cli *mcpclient.Client, name string, args map[string]any) map[string]any {
	t.Helper()
	text, isErr := stampCall(t, cli, name, args)
	require.False(t, isErr, "%s: %s", name, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	return out
}

// stampField is an answer's may_contain, as JSON decodes it.
func stampField(ref ...string) []any {
	var out []any
	for i := 0; i+2 < len(ref); i += 3 {
		out = append(out, map[string]any{"project": ref[i], ref[i+1]: ref[i+2]})
	}
	return out
}

// A conversation native to a confidential project, holding a read grant of
// another: every memory it writes in its home may contain the grant target's
// content; an edit by anyone keeps the stamp, and a supersession hands it on.
func TestStamp_MemoriesCarryTheConversationsStamps(t *testing.T) {
	r := newStampRig(t, true, nil)
	agent, amb := r.agent("cc/stamp-agent", stLegal, stProc)
	g := r.grant(amb, "cc/stamp-agent", stFin, stLegal, false, access.ProcessToken{})
	want := []core.Stamp{{Project: stFin, Via: core.StampViaGrant, Ref: g.ApprovalID}}

	w := stampOK(t, agent, "memory_write", map[string]any{"name": "close-rule", "kind": "decision",
		"description": "how the quarter closes", "body": "Close after reconciliation.\n"})
	require.Equal(t, stLegal, w["project"])
	require.Equal(t, want, r.memory(stLegal, "close-rule").MayContain, "frontmatter may_contain")
	rows := r.rows(core.ItemKindMemory, w["id"].(string))
	require.Len(t, rows, 1, "mirrored into item_provenance")
	require.Equal(t, core.StampViaGrant, rows[0].Via)

	read := stampOK(t, agent, "memory_read", map[string]any{"name": "close-rule"})
	require.Equal(t, stampField(stFin, core.StampViaGrant, g.ApprovalID), read["may_contain"])

	// Edits by a connection that holds nothing more keep the stamp: append,
	// search/replace, a metadata-only edit, a whole overwrite.
	own := r.owner(stLegal, "cc/legal-owner")
	stampOK(t, own, "memory_append", map[string]any{"name": "close-rule", "body": "And sign off.\n"})
	stampOK(t, own, "memory_edit", map[string]any{"name": "close-rule",
		"edits": []any{map[string]any{"old_string": "Close after", "new_string": "Close only after"}}})
	stampOK(t, own, "memory_edit", map[string]any{"name": "close-rule", "tags_remove": []any{"none"}, "tags_add": []any{"q4"}})
	stampOK(t, own, "memory_write", map[string]any{"name": "close-rule", "kind": "decision",
		"description": "how the quarter closes", "body": "Rewritten whole.\n"})
	require.Equal(t, want, r.memory(stLegal, "close-rule").MayContain, "no write removes a stamp")
	require.Len(t, r.rows(core.ItemKindMemory, w["id"].(string)), 1)

	// Supersession keeps the union: the replacement names what the old one
	// may hold, though its writer holds nothing more.
	repl := stampOK(t, own, "memory_write", map[string]any{"name": "close-rule-v2", "kind": "decision",
		"description": "the new close", "body": "Close after two reconciliations.\n", "supersedes": "close-rule"})
	require.Equal(t, stLegal+"/close-rule", repl["superseded"])
	require.Equal(t, want, r.memory(stLegal, "close-rule-v2").MayContain)
	require.Len(t, r.rows(core.ItemKindMemory, repl["id"].(string)), 1)
	require.Equal(t, want, r.memory(stLegal, "close-rule").MayContain, "the superseded memory keeps its own")

	// The writer's own home is never a stamp: the owner's memory carries none.
	stampOK(t, own, "memory_write", map[string]any{"name": "own-note", "kind": "reference", "description": "d", "body": "b\n"})
	require.Nil(t, r.memory(stLegal, "own-note").MayContain)
	plain := stampOK(t, own, "memory_read", map[string]any{"name": "own-note"})
	require.NotContains(t, plain, "may_contain")
}

// Notes: every content write stamps, a tag removal alone does not need to,
// and reads show it.
func TestStamp_NotesCarryTheConversationsStamps(t *testing.T) {
	r := newStampRig(t, true, nil)
	agent, amb := r.agent("cc/stamp-notes", stLegal, stProc)
	g := r.grant(amb, "cc/stamp-notes", stFin, stLegal, false, access.ProcessToken{})
	want := []core.Stamp{{Project: stFin, Via: core.StampViaGrant, Ref: g.ApprovalID}}
	own := r.owner(stLegal, "cc/legal-notes-owner")

	// A note the owner wrote, then the agent appended to, edited and updated.
	n := stampOK(t, own, "notes_create", map[string]any{"title": "ledger review", "body": "first pass\n"})
	id := n["id"].(string)
	require.Nil(t, r.note(id).MayContain)
	stampOK(t, own, "notes_update", map[string]any{"id": id, "tags_remove": []any{"created-by:agent"}})
	require.Nil(t, r.note(id).MayContain)
	stampOK(t, agent, "notes_append", map[string]any{"id": id, "body": "second pass"})
	require.Equal(t, want, r.note(id).MayContain)
	stampOK(t, agent, "notes_edit", map[string]any{"id": id,
		"edits": []any{map[string]any{"old_string": "first pass", "new_string": "first review"}}})
	stampOK(t, agent, "notes_update", map[string]any{"id": id, "description": "the review"})
	stampOK(t, own, "notes_update", map[string]any{"id": id, "body": "replaced by the owner\n"})
	require.Equal(t, want, r.note(id).MayContain, "stamped once, kept through every later write")

	created := stampOK(t, agent, "notes_create", map[string]any{"title": "agent findings", "body": "what it found\n"})
	require.Equal(t, want, r.note(created["id"].(string)).MayContain)
	read := stampOK(t, agent, "notes_read", map[string]any{"id": created["id"]})
	require.Equal(t, stampField(stFin, core.StampViaGrant, g.ApprovalID), read["may_contain"])
	require.Len(t, r.rows(core.ItemKindNote, created["id"].(string)), 1)
}

// Tasks and session findings carry their stamps in the index alone; reads
// show them, recall hits too, and a later status change keeps them.
func TestStamp_TasksAndFindingsAreStampedInTheIndex(t *testing.T) {
	r := newStampRig(t, true, nil)
	agent, amb := r.agent("cc/stamp-work", stLegal, stProc)
	g := r.grant(amb, "cc/stamp-work", stFin, stLegal, false, access.ProcessToken{})
	field := stampField(stFin, core.StampViaGrant, g.ApprovalID)

	task := stampOK(t, agent, "tasks_add", map[string]any{"title": "reconcile the ledger", "body": "quarterly close"})
	id := task["id"].(string)
	require.Len(t, r.rows(core.ItemKindTask, id), 1)
	list := stampOK(t, agent, "tasks_list", map[string]any{"id": id})
	require.Equal(t, field, list["tasks"].([]any)[0].(map[string]any)["may_contain"])

	own := r.owner(stLegal, "cc/legal-work-owner")
	stampOK(t, own, "tasks_update", map[string]any{"id": id, "status": "in_progress"})
	stampOK(t, own, "tasks_update", map[string]any{"id": id, "body": "the owner's body"})
	require.Len(t, r.rows(core.ItemKindTask, id), 1, "kept")
	ownTask := stampOK(t, own, "tasks_add", map[string]any{"title": "owner task"})
	require.Empty(t, r.rows(core.ItemKindTask, ownTask["id"].(string)))
	ownList := stampOK(t, own, "tasks_list", map[string]any{"id": ownTask["id"]})
	require.NotContains(t, ownList["tasks"].([]any)[0], "may_contain")

	stampOK(t, agent, "session_update", map[string]any{"findings": "reconciliation runs per ledger"})
	require.Len(t, r.rows(core.ItemKindSession, amb), 1)
	hits := stampOK(t, agent, "recall", map[string]any{"query": "reconciliation ledger"})["hits"].([]any)
	seen := map[string]any{}
	for _, h := range hits {
		hit := h.(map[string]any)
		seen[hit["kind"].(string)] = hit["may_contain"]
	}
	require.Equal(t, field, seen[core.ItemKindTask])
	require.Equal(t, field, seen[core.ItemKindSession])

	end := stampOK(t, agent, "session_end", map[string]any{"findings": "closed the quarter"})
	require.Equal(t, "completed", end["status"])
	require.Len(t, r.rows(core.ItemKindSession, amb), 1)
}

// A move from a confidential home into a sealed project: what the
// conversation writes there may contain its old home's content, cleared by
// the move.
func TestStamp_AMoveStampsWithTheMove(t *testing.T) {
	r := newStampRig(t, true, nil)
	agent, _ := r.agent("cc/stamp-mover", stLegal, stProc)
	text, isErr := stampCall(t, agent, "session_start", map[string]any{"project": stVault})
	require.True(t, isErr, text)
	ctx := context.Background()
	a, err := r.grants.Resolve(ctx, stampShort.FindString(text))
	require.NoError(t, err)
	require.Equal(t, store.AccessKindMove, a.Kind)
	facts, err := r.grants.Facts(ctx, a.ID)
	require.NoError(t, err)
	_, err = r.grants.ApproveMove(ctx, approval.Decision{ID: a.ID, Duration: approval.For1h, Digest: facts.Digest,
		Proof: approval.Proof{Kind: approval.ProofKey}})
	require.NoError(t, err)

	w := stampOK(t, agent, "memory_write", map[string]any{"name": "vault-rule", "kind": "reference",
		"description": "d", "body": "b\n"})
	require.Equal(t, stVault, w["project"])
	require.Equal(t, []core.Stamp{{Project: stLegal, Via: core.StampViaMove, Ref: a.ID}}, r.memory(stVault, "vault-rule").MayContain)
}

// The label's own project is never a stamp, and a conversation that holds
// nothing else writes none.
func TestStamp_TheHomeAloneStampsNothing(t *testing.T) {
	r := newStampRig(t, true, nil)
	agent, amb := r.agent("cc/stamp-home", stLegal, stProc)
	stampOK(t, agent, "memory_write", map[string]any{"name": "home-rule", "kind": "reference", "description": "d", "body": "b\n"})
	task := stampOK(t, agent, "tasks_add", map[string]any{"title": "home task"})
	stampOK(t, agent, "session_update", map[string]any{"findings": "nothing held"})
	require.Nil(t, r.memory(stLegal, "home-rule").MayContain)
	require.Empty(t, r.rows(core.ItemKindTask, task["id"].(string)))
	require.Empty(t, r.rows(core.ItemKindSession, amb))
	require.Zero(t, r.count())
}

// Off -- isolation.requests not set -- nothing is stamped, whatever the
// database holds: no frontmatter key, no index row, no answer field.
func TestStamp_OffWritesNoKeyAndNoRow(t *testing.T) {
	r := newStampRig(t, false, nil)
	agent, amb := r.agent("cc/stamp-off", stLegal, stProc)
	r.grant(amb, "cc/stamp-off", stFin, stLegal, true, access.ProcessToken{})
	w := stampOK(t, agent, "memory_write", map[string]any{"name": "off-rule", "kind": "reference", "description": "d", "body": "b\n"})
	n := stampOK(t, agent, "notes_create", map[string]any{"title": "off note", "body": "b\n"})
	stampOK(t, agent, "notes_append", map[string]any{"id": n["id"], "body": "more"})
	task := stampOK(t, agent, "tasks_add", map[string]any{"title": "off task"})
	stampOK(t, agent, "session_update", map[string]any{"findings": "off findings"})
	require.Nil(t, r.memory(stLegal, "off-rule").MayContain)
	require.Nil(t, r.note(n["id"].(string)).MayContain)
	require.Zero(t, r.count())
	for _, out := range []map[string]any{
		stampOK(t, agent, "memory_read", map[string]any{"id": w["id"]}),
		stampOK(t, agent, "notes_read", map[string]any{"id": n["id"]}),
		stampOK(t, agent, "tasks_list", map[string]any{"id": task["id"]})["tasks"].([]any)[0].(map[string]any),
	} {
		require.NotContains(t, out, "may_contain")
	}
}

// Under peer_auth the conversation is the agent's process tree: a grant keyed
// to a process of its chain stamps what it writes.
func TestStamp_PeerTierStampsFromTheProcessTree(t *testing.T) {
	policy, err := access.New(config.PeerAuth{Owner: "owner"}, func(string) (access.Account, error) {
		return access.Account{UID: 501, Name: "owner"}, nil
	})
	require.NoError(t, err)
	agentTok := access.ProcessToken{PID: 200, Version: 2000, Boot: stBoot}
	bridge := access.ProcessToken{PID: 300, Version: 3000, Boot: stBoot}
	r := newStampRig(t, true, func() access.View {
		v, ok := policy.PeerChainView(501, []access.ProcessToken{bridge, agentTok})
		require.True(t, ok)
		return v
	})
	agent, amb := r.agent("cc/stamp-peer", stLegal, agentTok.AgentID())
	g := r.grant(amb, "cc/stamp-peer", stFin, stLegal, true, agentTok)

	w := stampOK(t, agent, "memory_write", map[string]any{"name": "peer-rule", "kind": "reference", "description": "d", "body": "b\n"})
	require.Equal(t, stLegal, w["project"])
	require.Equal(t, []core.Stamp{{Project: stFin, Via: core.StampViaGrant, Ref: g.ApprovalID}}, r.memory(stLegal, "peer-rule").MayContain)
	// A write into the write grant's target carries the home's content.
	task := stampOK(t, agent, "tasks_add", map[string]any{"title": "visit", "project": stFin})
	rows := r.rows(core.ItemKindTask, task["id"].(string))
	require.Len(t, rows, 1)
	require.Equal(t, stLegal, rows[0].Source)
	require.Equal(t, stFin, rows[0].Project)
}
