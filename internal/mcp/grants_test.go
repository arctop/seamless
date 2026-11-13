package mcp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
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

// Read grants at the tool surface (isolation.requests). Off -- the default --
// a confidential project's refusal reads word for word as it always has, and
// nothing is filed or recorded. On, the refusal names the request it filed,
// and an approved grant lets that one session (or agent tree) read that one
// project, read-only, while its writes stay confined.

const (
	gOps   = "example-ops"     // open: the asker
	gHR    = "example-hr"      // open: somewhere else to write
	gFin   = "example-finance" // confidential: the target
	gLegal = "example-legal"   // confidential: a confidential asker
	gVault = "example-vault"   // sealed
)

var gShort = regexp.MustCompile(`R-[0-9A-Z]{4}`)

// offRefusal is today's refusal of a read of gFin from outside, word for word.
const offRefusal = "project example-finance is confidential: reads require a session bound to it -- if your task " +
	"is about example-finance, bind this connection to it with session_start project=example-finance; otherwise leave it alone"

type grantRig struct {
	t      *testing.T
	db     *sql.DB
	rec    *events.Recorder
	grants *approval.Grants
	url    string

	mu  sync.Mutex
	now time.Time

	finNote, finMem string
}

// newGrantRig is a daemon's MCP server over a fresh database with the five
// projects; on wires the advisory read grants (isolation.requests, no
// peer_auth).
func newGrantRig(t *testing.T, on bool) *grantRig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	for slug, state := range map[string]core.Isolation{gOps: core.IsolationOpen, gHR: core.IsolationOpen,
		gFin: core.IsolationConfidential, gLegal: core.IsolationConfidential, gVault: core.IsolationSealed} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, state))
	}
	r := &grantRig{t: t, db: db, rec: events.NewRecorder(db), now: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)}
	ret := retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	ret.SetBodyReader(mgr.Store())
	cfg := mcpserver.Config{DB: db, Files: mgr, Retrieve: ret, Events: r.rec, APIKey: testKey,
		Features: config.Features{Research: true}, Now: r.clock}
	if on {
		r.grants = approval.NewGrants(approval.GrantsConfig{DB: db, Events: r.rec, LeaseCap: 8 * time.Hour, Now: r.clock})
		cfg.Grants = r.grants
	}
	ts := httptest.NewServer(mcpserver.New(cfg).Handler())
	t.Cleanup(ts.Close)
	r.url = ts.URL

	fin := r.bind(gFin, "cc/fin")
	r.finNote = r.ok(fin, "notes_create", map[string]any{"title": "ledger notes", "body": "quarterly ledger reconciliation notes"})["id"].(string)
	r.finMem = r.ok(fin, "memory_write", map[string]any{"name": "ledger-close-rule", "kind": "constraint",
		"description": "close the ledger only after reconciliation", "body": "Reconcile every ledger before closing it.\n"})["id"].(string)
	return r
}

func (r *grantRig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *grantRig) advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = r.now.Add(d)
}

// bind dials a connection bound to a new session in project.
func (r *grantRig) bind(project, name string) *mcpclient.Client {
	r.t.Helper()
	ctx := context.Background()
	id, err := core.NewID()
	require.NoError(r.t, err)
	now := r.clock()
	require.NoError(r.t, store.CreateSession(ctx, r.db, core.Session{ID: id, Name: name, ProjectSlug: project,
		Status: core.SessionActive, CWD: "/work/" + project, CreatedAt: now, UpdatedAt: now}))
	cli := dialClient(r.t, ctx, r.url, testKey)
	start := r.ok(cli, "session_start", map[string]any{"name": name})
	require.Equal(r.t, project, start["project"])
	return cli
}

func (r *grantRig) call(cli *mcpclient.Client, name string, args map[string]any) ([]string, bool) {
	r.t.Helper()
	res, err := cli.CallTool(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
	require.NoError(r.t, err)
	var items []string
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			items = append(items, tc.Text)
		}
	}
	return items, res.IsError
}

func (r *grantRig) ok(cli *mcpclient.Client, name string, args map[string]any) map[string]any {
	r.t.Helper()
	items, isErr := r.call(cli, name, args)
	require.False(r.t, isErr, "%s: %v", name, items)
	var out map[string]any
	require.NoError(r.t, json.Unmarshal([]byte(items[0]), &out))
	return out
}

func (r *grantRig) refused(cli *mcpclient.Client, name string, args map[string]any) string {
	r.t.Helper()
	items, isErr := r.call(cli, name, args)
	require.True(r.t, isErr, "%s: %v", name, items)
	require.Len(r.t, items, 1)
	return items[0]
}

func (r *grantRig) count(query string, args ...any) int {
	r.t.Helper()
	var n int
	require.NoError(r.t, r.db.QueryRowContext(context.Background(), query, args...).Scan(&n))
	return n
}

// approveKey approves a pending request as seam approve does without
// peer_auth: the facts' digest, the bearer key's decision.
func (r *grantRig) approveKey(ref, duration string) store.AccessGrant {
	r.t.Helper()
	ctx := context.Background()
	a, err := r.grants.Resolve(ctx, ref)
	require.NoError(r.t, err)
	facts, err := r.grants.Facts(ctx, a.ID)
	require.NoError(r.t, err)
	g, err := r.grants.Approve(ctx, approval.Decision{ID: a.ID, Duration: duration, Digest: facts.Digest,
		Proof: approval.Proof{Kind: approval.ProofKey}})
	require.NoError(r.t, err)
	return g
}

// reads are the ways a session reads another project: by project=, and by id.
func (r *grantRig) reads() []struct {
	tool string
	args map[string]any
} {
	return []struct {
		tool string
		args map[string]any
	}{
		{"recall", map[string]any{"query": "ledger reconciliation", "project": gFin}},
		{"notes_read", map[string]any{"id": r.finNote}},
		{"memory_read", map[string]any{"id": r.finMem}},
	}
}

func TestGrants_OffIsByteIdentical(t *testing.T) {
	r := newGrantRig(t, false)
	ops := r.bind(gOps, "cc/ops")
	for _, rd := range r.reads() {
		require.Equal(t, rd.tool+": "+offRefusal, r.refused(ops, rd.tool, rd.args), rd.tool)
	}
	// Nothing filed, nothing recorded, and recall's scope is the old one.
	require.Zero(t, r.count(`SELECT COUNT(*) FROM access_approvals`))
	require.Zero(t, r.count(`SELECT COUNT(*) FROM access_grants`))
	require.Zero(t, r.count(`SELECT COUNT(*) FROM events WHERE kind LIKE 'access.%'`))
	hits := r.ok(ops, "recall", map[string]any{"query": "ledger reconciliation"})["hits"].([]any)
	require.Empty(t, hits)
}

func TestGrants_OnRefusalNamesTheRequest(t *testing.T) {
	r := newGrantRig(t, true)
	ops := r.bind(gOps, "cc/ops")
	var id string
	for _, rd := range r.reads() {
		text := r.refused(ops, rd.tool, rd.args)
		rule, remedy, found := strings.Cut(text, "\n")
		require.True(t, found, "%s: two lines, the rule then the remedy: %s", rd.tool, text)
		require.Equal(t, rd.tool+": "+offRefusal, rule, "the rule reads word for word as before")
		short := gShort.FindString(remedy)
		require.NotEmpty(t, short, remedy)
		if id == "" {
			id = short
		}
		require.Equal(t, id, short, "a repeated refusal names the same request")
		require.Contains(t, remedy, "seam approve "+short)
		require.Contains(t, remedy, "the console's Access requests page")
		require.Contains(t, remedy, "confines this session's writes to example-finance for the rest of its life: once it "+
			"holds example-finance's content, it writes into no open project, example-ops included, and not into the global scope.")
		require.NotContains(t, text, "reconciliation notes", "a refusal never echoes the content it withholds")
	}
	require.Equal(t, 1, r.count(`SELECT COUNT(*) FROM access_approvals WHERE kind = 'grant' AND status = 'pending'`))
	require.Equal(t, 1, r.count(`SELECT COUNT(*) FROM events WHERE kind = 'access.requested'`))

	// A confidential asker may ask too; its remedy says where content may go.
	legal := r.bind(gLegal, "cc/legal")
	text := r.refused(legal, "recall", map[string]any{"query": "ledger", "project": gFin})
	require.Contains(t, text, "nothing read from example-finance may be written anywhere but example-legal")

	// Never offered: a sealed target, a sealed asker. The refusal stays final.
	before := r.count(`SELECT COUNT(*) FROM access_approvals`)
	vaultText := r.refused(ops, "recall", map[string]any{"query": "x", "project": gVault})
	require.NotContains(t, vaultText, "\n")
	require.NotContains(t, vaultText, "R-")
	vault := r.bind(gVault, "cc/vault")
	sealedText := r.refused(vault, "recall", map[string]any{"query": "x", "project": gFin})
	require.NotContains(t, sealedText, "\n")
	require.Equal(t, before, r.count(`SELECT COUNT(*) FROM access_approvals`), "nothing involving a sealed project files a request")
}

func TestGrants_OnApprovedGrantReadsOneProjectAndConfinesWrites(t *testing.T) {
	r := newGrantRig(t, true)
	ops := r.bind(gOps, "cc/ops")
	other := r.bind(gHR, "cc/hr")
	text := r.refused(ops, "recall", map[string]any{"query": "ledger reconciliation", "project": gFin})
	r.refused(other, "recall", map[string]any{"query": "ledger reconciliation", "project": gFin})
	grant := r.approveKey(gShort.FindString(text), approval.For1h)
	require.Equal(t, gOps, grant.FromProject)

	// The first result after the grant lands announces it, once.
	items, isErr := r.call(ops, "notes_read", map[string]any{"id": r.finNote})
	require.False(t, isErr, "%v", items)
	require.Len(t, items, 2)
	require.Contains(t, items[0], "quarterly ledger reconciliation notes", "the grant reads the target")
	require.Contains(t, items[1], "Read access to example-finance was approved until")
	require.NotContains(t, items[1], "may be copied")
	require.Contains(t, items[1], "This session now writes only into example-finance, for the rest of its life")
	items, _ = r.call(ops, "memory_read", map[string]any{"id": r.finMem})
	require.Len(t, items, 1, "announced once")

	// recall: by project=, and the target joins the session's own scope.
	hits := r.ok(ops, "recall", map[string]any{"query": "ledger reconciliation", "project": gFin})["hits"].([]any)
	require.NotEmpty(t, hits)
	hits = r.ok(ops, "recall", map[string]any{"query": "ledger reconciliation"})["hits"].([]any)
	require.NotEmpty(t, hits, "a granted project joins the recall scope while the grant lives")
	for _, h := range hits {
		require.Contains(t, []string{gOps, gFin, ""}, h.(map[string]any)["project"])
	}

	// That one project only: another confidential project stays fenced.
	legal := r.bind(gLegal, "cc/legal")
	r.ok(legal, "notes_create", map[string]any{"title": "contract notes", "body": "contract terms"})
	refusal := r.refused(ops, "recall", map[string]any{"query": "contract", "project": gLegal})
	require.True(t, strings.HasPrefix(refusal, "recall: project example-legal is confidential"))

	// Another session is still refused.
	text = r.refused(other, "notes_read", map[string]any{"id": r.finNote})
	require.True(t, strings.HasPrefix(text, "notes_read: "+offRefusal+"\n"), text)

	// Writes (the clearance rule): holding the target's content, the open
	// asker writes into the target alone -- never into an open project, its
	// own included, nor the global scope.
	for where, w := range map[string]map[string]any{
		"project example-hr":  {"name": "copied-into-hr", "kind": "reference", "description": "d", "body": "b\n", "project": gHR},
		"the global scope":    {"name": "copied-into-global", "kind": "reference", "description": "d", "body": "b\n", "project": "global"},
		"project example-ops": {"name": "ops-own", "kind": "reference", "description": "d", "body": "b\n"},
	} {
		require.Equal(t, "memory_write: this session holds content from example-finance, so it writes only into "+
			"example-finance: a write into "+where+" is refused", r.refused(ops, "memory_write", w))
	}
	r.ok(ops, "memory_write", map[string]any{"name": "into-fin", "kind": "reference", "description": "d", "body": "b\n", "project": gFin})
	r.ok(ops, "notes_append", map[string]any{"id": r.finNote, "body": "appended from outside"})
	require.Equal(t, 1, r.count(`SELECT COUNT(*) FROM events WHERE kind = 'access.granted'`))
	require.Equal(t, 1, r.count(`SELECT COUNT(*) FROM events WHERE kind = 'access.confined' AND project_slug = ?`, gOps))

	// The session that was refused but never granted writes as before.
	r.ok(other, "memory_write", map[string]any{"name": "hr-into-fin", "kind": "reference", "description": "d", "body": "b\n", "project": gFin})
}

func TestGrants_OnExpiryRevokeAndSessionEnd(t *testing.T) {
	r := newGrantRig(t, true)
	ops := r.bind(gOps, "cc/ops")
	read := map[string]any{"id": r.finNote}
	text := r.refused(ops, "notes_read", read)
	grant := r.approveKey(gShort.FindString(text), approval.For1h)

	_, isErr := r.call(ops, "notes_read", read)
	require.False(t, isErr)
	// From 30 minutes before its end, every result says when it ends.
	r.advance(40 * time.Minute)
	items, isErr := r.call(ops, "notes_read", read)
	require.False(t, isErr)
	require.Len(t, items, 2)
	require.Contains(t, items[1], "Access to example-finance ends at")
	// Past its end: refused again (and asked again), and still confined.
	r.advance(30 * time.Minute)
	text = r.refused(ops, "notes_read", read)
	require.True(t, strings.HasPrefix(text, "notes_read: "+offRefusal+"\nRemedy: read request R-"), text)
	r.refused(ops, "memory_write", map[string]any{"name": "after-expiry", "kind": "reference", "description": "d", "body": "b\n", "project": gHR})

	// Revoke: a second grant, revoked by the owner.
	second := r.approveKey(gShort.FindString(text), approval.For4h)
	require.NotEqual(t, grant.ID, second.ID)
	_, isErr = r.call(ops, "notes_read", read)
	require.False(t, isErr)
	_, err := r.grants.RevokeGrant(context.Background(), second.ID)
	require.NoError(t, err)
	r.refused(ops, "notes_read", read)
	require.Equal(t, 1, r.count(`SELECT COUNT(*) FROM events WHERE kind = 'access.revoked'`))

	// The open asker's session_end would leave its findings in the open
	// project: the clearance rule refuses it like any write there.
	require.Equal(t, "session_end: this session holds content from example-finance, so it writes only into example-finance: "+
		"a write into project example-ops is refused", r.refused(ops, "session_end", map[string]any{"findings": "done"}))

	// session_end revokes what the session holds: a confidential asker's
	// home is cleared for what it read.
	legal := r.bind(gLegal, "cc/legal")
	text = r.refused(legal, "notes_read", read)
	third := r.approveKey(gShort.FindString(text), approval.ForSession)
	_, isErr = r.call(legal, "notes_read", read)
	require.False(t, isErr)
	r.ok(legal, "session_end", map[string]any{"findings": "done"})
	g, err := store.AccessGrantByID(context.Background(), r.db, third.ID)
	require.NoError(t, err)
	require.Equal(t, store.LeaseRevokedSessionEnded, g.RevokedReason)
}

// The peer tier: under peer_auth a grant is anchored to the agent process in
// the socket connection's kernel chain and decided by the approve core's
// proofs (here the daemon's account, as seamlessd approve commits it). It
// opens reads for that agent's tree and nothing else; the TCP key holds none.
func TestGrants_PeerTierDaemonProof(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	for slug, state := range map[string]core.Isolation{gOps: core.IsolationOpen, gFin: core.IsolationConfidential} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, state))
	}
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	policy, err := access.New(config.PeerAuth{Owner: "owner"}, func(n string) (access.Account, error) {
		if n == "owner" {
			return access.Account{UID: 501, Name: "owner"}, nil
		}
		return access.Account{}, fmt.Errorf("no account %s", n)
	})
	require.NoError(t, err)
	rec := events.NewRecorder(db)
	svc := approval.New(approval.Config{DB: db, Events: rec, LeaseCap: 8 * time.Hour, Now: clock,
		Programs: func(t access.ProcessToken) string {
			if t == lkTmux {
				return "/opt/homebrew/Cellar/tmux/3.8/bin/tmux"
			}
			return ""
		}})
	grants := approval.NewGrants(approval.GrantsConfig{Approvals: svc})
	ret := retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	srv := mcpserver.New(mcpserver.Config{DB: db, Files: mgr, Retrieve: ret, Events: rec, APIKey: testKey, LocalHost: lkHost,
		Features: config.Features{Research: true}, Now: clock, Approvals: svc, Grants: grants})
	serve := func(view func() access.View) string {
		h := srv.Handler()
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			h.ServeHTTP(w, req.WithContext(access.WithView(req.Context(), view())))
		}))
		t.Cleanup(ts.Close)
		return ts.URL
	}
	chainView := func(chain ...access.ProcessToken) func() access.View {
		return func() access.View {
			v, ok := policy.PeerChainView(501, chain)
			require.True(t, ok)
			return v
		}
	}
	dial := func(base string, agent access.ProcessToken) *mcpclient.Client {
		cli, err := mcpclient.NewStreamableHttpClient(base, transport.WithHTTPHeaders(map[string]string{
			"Authorization": "Bearer " + testKey, mcpserver.HostHeader: lkHost, mcpserver.AgentProcessHeader: agent.AgentID()}))
		require.NoError(t, err)
		t.Cleanup(func() { _ = cli.Close() })
		require.NoError(t, cli.Start(ctx))
		var initReq mcp.InitializeRequest
		initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
		initReq.Params.ClientInfo = mcp.Implementation{Name: "grant", Version: "0"}
		_, err = cli.Initialize(ctx, initReq)
		require.NoError(t, err)
		return cli
	}
	call := func(cli *mcpclient.Client, name string, args map[string]any) (string, bool) {
		res, err := cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
		require.NoError(t, err)
		return resultText(t, res), res.IsError
	}
	stamp := func(project, name string, agent access.ProcessToken) {
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive,
			Ambient: true, Host: lkHost, ExternalSessionID: name + "-x", ExternalClient: "claude-code", CWD: "/work/" + project,
			CreatedAt: now, UpdatedAt: now}))
		require.NoError(t, store.SetSessionAgentProcess(ctx, db, id, agent.AgentID()))
	}
	// The target's content, written by its own agent.
	stamp(gFin, "cc/fin", lkStranger)
	fin := dial(serve(chainView(lkStranger)), lkStranger)
	out, isErr := call(fin, "notes_create", map[string]any{"title": "ledger notes", "body": "quarterly ledger reconciliation notes"})
	require.False(t, isErr, out)
	var note map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &note))
	read := map[string]any{"id": note["id"]}

	stamp(gOps, "cc/ops", lkAgent)
	ops := dial(serve(chainView(lkBridge, lkAgent)), lkAgent)
	text, isErr := call(ops, "notes_read", read)
	require.True(t, isErr)
	require.True(t, strings.HasPrefix(text, "notes_read: "+offRefusal+"\nRemedy: read request R-"), text)
	require.Contains(t, text, "confines this agent's writes to example-finance for the rest of its life: once it holds "+
		"example-finance's content, it writes into no open project, example-ops included")
	a, err := grants.Resolve(ctx, gShort.FindString(text))
	require.NoError(t, err)
	require.Equal(t, lkAgent.Key(), a.ProcessKey, "the request is anchored to the agent, not the bridge that asked")

	// The bearer key proves nothing here; the daemon's account does.
	facts, err := grants.Facts(ctx, a.ID)
	require.NoError(t, err)
	_, err = grants.Approve(ctx, approval.Decision{ID: a.ID, Duration: approval.For1h, Digest: facts.Digest, Proof: approval.Proof{Kind: approval.ProofKey}})
	require.ErrorIs(t, err, approval.ErrPeerProof)
	_, err = grants.Approve(ctx, approval.Decision{ID: a.ID, Duration: approval.For1h, Digest: facts.Digest, Proof: approval.Proof{Kind: approval.ProofDaemon}})
	require.NoError(t, err)

	out, isErr = call(ops, "notes_read", read)
	require.False(t, isErr, out)
	require.Contains(t, out, "quarterly ledger reconciliation notes")

	// Another agent tree, and the TCP key, hold nothing.
	stamp(gOps, "cc/ops-2", lkOther)
	strangerOps := dial(serve(chainView(lkOther)), lkOther)
	_, isErr = call(strangerOps, "notes_read", read)
	require.True(t, isErr)
	tcp := dial(serve(func() access.View { return policy.OwnerView() }), lkAgent)
	text, isErr = call(tcp, "notes_read", read)
	require.True(t, isErr)
	require.Equal(t, "notes_read: "+offRefusal, strings.SplitN(text, "\n", 2)[0])
	// Still never a lease.
	leases, err := store.LiveAccessLeases(ctx, db, now)
	require.NoError(t, err)
	require.Empty(t, leases)

	// A tmux pane's call -- its session stamped there by hand -- files no
	// read request (the anchor rule): the fence's refusal says why, and what
	// to do instead.
	before, err := grants.Pending(ctx)
	require.NoError(t, err)
	stamp(gOps, "cc/pane", lkTmux)
	pane := dial(serve(chainView(lkPane, lkTmux)), lkTmux)
	text, isErr = call(pane, "notes_read", read)
	require.True(t, isErr)
	ls := strings.Split(text, "\n")
	require.Len(t, ls, 2, text)
	require.Equal(t, "notes_read: "+offRefusal+"; "+strings.Replace(tmuxRule, "no access request", "no read request", 1), ls[0])
	require.Equal(t, anchorCure, ls[1])
	after, err := grants.Pending(ctx)
	require.NoError(t, err)
	require.Len(t, after, len(before), "nothing was filed")
}

var lkOther = access.ProcessToken{PID: 500, Version: 5000, Boot: lkBoot}

// The case a read grant exists for: an agent working a task in its own
// confidential project needs another confidential project's knowledge, asks,
// the owner approves, and the agent writes up the work in that task. The
// write-up stays open to it after the grant ends, the grant tells it to cite
// the project by reference, and nothing else opens. (From an open project a
// read leaves the agent writing only into the target: the clearance rule.)
func TestGrants_HolderDocumentsItsWorkInTheAskingProject(t *testing.T) {
	r := newGrantRig(t, true)
	legal := r.bind(gLegal, "cc/legal")
	hr := r.bind(gHR, "cc/hr")
	task := r.ok(legal, "tasks_add", map[string]any{"title": "reconcile the quarter", "plan": "close-the-books"})["id"].(string)
	elsewhere := r.ok(hr, "tasks_add", map[string]any{"title": "an hr task"})["id"].(string)

	const cite = "When you document this work in example-legal (task details, memories, notes, findings), refer to " +
		"example-finance's items by reference -- project slug plus the item's name or id -- and leave out " +
		"example-finance's confidential details."
	text := r.refused(legal, "recall", map[string]any{"query": "ledger reconciliation", "project": gFin})
	require.Contains(t, text, cite, "the instruction arrives with the request")
	r.approveKey(gShort.FindString(text), approval.For1h)

	items, isErr := r.call(legal, "notes_read", map[string]any{"id": r.finNote})
	require.False(t, isErr, "%v", items)
	require.Len(t, items, 2)
	require.Contains(t, items[1], cite, "and with the approval")

	const writeUp = "Reconciled against example-finance:quarterly-ledger-notes."
	require.Equal(t, writeUp, r.ok(legal, "tasks_update", map[string]any{"id": task, "body": writeUp})["body"])
	r.ok(legal, "memory_write", map[string]any{"name": "quarter-closed", "kind": "reference", "description": "d", "body": writeUp + "\n"})
	r.ok(legal, "notes_create", map[string]any{"title": "close notes", "body": writeUp})

	require.Contains(t, r.refused(legal, "tasks_update", map[string]any{"id": elsewhere, "body": "x"}),
		"so it writes only into example-legal", "another project's task stays closed to it")

	r.advance(2 * time.Hour)
	r.ok(legal, "tasks_update", map[string]any{"id": task, "body": writeUp + " Closed."})
}
