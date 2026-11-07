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

// The locked view at the tool surface (peer_auth.locked, steps 7a and 8): a
// project the owner has locked is listed with its counts, named in scope
// errors and recalls, refused at the lock with the request that would open
// it, briefed once when a lease lands, and warned about from 30 minutes
// before the lease ends. Off -- no View, or no approval service -- nothing
// changes.

const (
	lkBoot = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"
	lkFin  = "example-finance"
	lkOps  = "example-ops"
	lkHost = "laptop"
)

var (
	lkBridge   = access.ProcessToken{PID: 300, Version: 3000, Boot: lkBoot}
	lkAgent    = access.ProcessToken{PID: 200, Version: 2000, Boot: lkBoot}
	lkStranger = access.ProcessToken{PID: 400, Version: 4000, Boot: lkBoot}
	lkShortID  = regexp.MustCompile(`R-[0-9A-Z]{4}`)
)

// lkClaude is the program the kernel names for lkAgent: Claude Code's native
// install, by its path.
const lkClaude = "/home/owner/.local/share/claude/versions/2.1.319"

// lkPrograms is the rig's kernel: it names the agent process, and no other
// (a token the kernel no longer names is filed as before).
func lkPrograms(t access.ProcessToken) string {
	if t == lkAgent {
		return lkClaude
	}
	return ""
}

// lockRig is one daemon's MCP server with a locked project, a fake clock
// shared by the server and the approval service, and transports that carry
// the View cmd/seamlessd's peerHandler would build.
type lockRig struct {
	t      *testing.T
	db     *sql.DB
	mgr    *files.Manager
	ret    *retrieve.Service
	rec    *events.Recorder
	svc    *approval.Service
	policy *access.Policy
	srv    *mcpserver.Server

	mu  sync.Mutex
	now time.Time
}

func newLockRig(t *testing.T) *lockRig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	for _, slug := range []string{lkFin, lkOps} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, lkFin, core.IsolationConfidential))
	policy, err := access.New(config.PeerAuth{Owner: "owner", Locked: []string{lkFin}}, func(n string) (access.Account, error) {
		if n == "owner" {
			return access.Account{UID: 501, Name: "owner"}, nil
		}
		return access.Account{}, fmt.Errorf("no account %s", n)
	})
	require.NoError(t, err)
	r := &lockRig{t: t, db: db, mgr: mgr, rec: events.NewRecorder(db), policy: policy,
		now: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)}
	r.ret = retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	r.ret.SetBodyReader(mgr.Store())
	r.svc = approval.New(approval.Config{DB: db, Events: r.rec, Host: lkHost, LeaseCap: 8 * time.Hour, Now: r.clock,
		Programs: lkPrograms})
	r.srv = r.server(r.svc)
	return r
}

// server is an MCP server over the rig's state; approvals nil is a daemon
// that locks nothing.
func (r *lockRig) server(approvals mcpserver.Approvals) *mcpserver.Server {
	cfg := mcpserver.Config{
		DB: r.db, Files: r.mgr, Retrieve: r.ret, Events: r.rec, APIKey: testKey, LocalHost: lkHost,
		Features: config.Features{Research: true}, Now: r.clock,
	}
	if approvals != nil {
		cfg.Approvals = approvals
	}
	return mcpserver.New(cfg)
}

func (r *lockRig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *lockRig) set(t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = t
}

// serve exposes srv with view(r) attached to every request, as the transport
// edge does; view nil attaches none (peer_auth off).
func (r *lockRig) serve(srv *mcpserver.Server, view func() (access.View, bool)) string {
	h := srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if view != nil {
			if v, ok := view(); ok {
				req = req.WithContext(access.WithView(req.Context(), v))
			}
		}
		h.ServeHTTP(w, req)
	}))
	r.t.Cleanup(ts.Close)
	return ts.URL
}

// socket is the owner's socket transport for a process chain, with the leases
// live on it at the rig's clock (peerHandler's leasedView).
func (r *lockRig) socket(chain ...access.ProcessToken) string {
	return r.serve(r.srv, func() (access.View, bool) {
		v, ok := r.policy.PeerChainView(501, chain)
		require.True(r.t, ok)
		keys := make([]string, len(chain))
		for i, c := range chain {
			keys[i] = c.Key()
		}
		now := r.clock()
		rows, err := store.ActiveAccessLeasesFor(context.Background(), r.db, keys, now)
		require.NoError(r.t, err)
		return v.WithLeases(now, store.AccessLeases(rows)), true
	})
}

// tcp is the owner's TCP transport: the bearer key, no process.
func (r *lockRig) tcp() string {
	return r.serve(r.srv, func() (access.View, bool) { return r.policy.OwnerView(), true })
}

// dial connects to base, naming the agent process when agent is set.
func (r *lockRig) dial(base string, agent bool) *mcpclient.Client {
	r.t.Helper()
	headers := map[string]string{"Authorization": "Bearer " + testKey, mcpserver.HostHeader: lkHost}
	if agent {
		headers[mcpserver.AgentProcessHeader] = lkAgent.AgentID()
	}
	cli, err := mcpclient.NewStreamableHttpClient(base, transport.WithHTTPHeaders(headers))
	require.NoError(r.t, err)
	r.t.Cleanup(func() { _ = cli.Close() })
	require.NoError(r.t, cli.Start(context.Background()))
	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "lock", Version: "0"}
	_, err = cli.Initialize(context.Background(), initReq)
	require.NoError(r.t, err)
	return cli
}

// call returns every text item of a tool result and whether it is an error.
func (r *lockRig) call(cli *mcpclient.Client, name string, args map[string]any) ([]string, bool) {
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

// ok calls and requires a success, returning the first item (the tool's own
// result) and every item.
func (r *lockRig) ok(cli *mcpclient.Client, name string, args map[string]any) (string, []string) {
	r.t.Helper()
	items, isErr := r.call(cli, name, args)
	require.False(r.t, isErr, "%s: %v", name, items)
	require.NotEmpty(r.t, items)
	return items[0], items
}

// refused calls and requires an error, returning its text.
func (r *lockRig) refused(cli *mcpclient.Client, name string, args map[string]any) string {
	r.t.Helper()
	items, isErr := r.call(cli, name, args)
	require.True(r.t, isErr, "%s: %v", name, items)
	require.NotEmpty(r.t, items)
	return items[0]
}

// session records the agent's session the way the SessionStart hook does in a
// locked repository: ambient, in project, stamped with the agent process.
func (r *lockRig) session(name, project string) string {
	r.t.Helper()
	ctx := context.Background()
	id, err := core.NewID()
	require.NoError(r.t, err)
	now := r.clock()
	require.NoError(r.t, store.CreateSession(ctx, r.db, core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive, Ambient: true, Host: lkHost,
		ExternalSessionID: name + "-x", ExternalClient: "claude-code", CWD: "/work/" + project, CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(r.t, store.SetSessionAgentProcess(ctx, r.db, id, lkAgent.AgentID()))
	return id
}

func (r *lockRig) pending() []store.AccessApproval {
	r.t.Helper()
	rows, err := store.PendingAccessApprovals(context.Background(), r.db, r.clock())
	require.NoError(r.t, err)
	return rows
}

// approve grants a pending request for length at the rig's clock, through the
// store's one lease writer (the approve core's last step).
func (r *lockRig) approve(id string, length time.Duration) store.AccessLease {
	r.t.Helper()
	ctx := context.Background()
	a, err := store.AccessApprovalByID(ctx, r.db, id)
	require.NoError(r.t, err)
	lease, err := store.ApproveAccessFor(ctx, r.db, a.ID, a.Challenge, "test-owner", length, r.clock())
	require.NoError(r.t, err)
	return lease
}

// seed writes the fixture's knowledge as the owner with peer_auth off.
func (r *lockRig) seed() {
	r.t.Helper()
	admin := r.dial(r.serve(r.srv, nil), false)
	r.ok(admin, "memory_write", map[string]any{"project": lkFin, "name": "fin-ledger-rule", "kind": "constraint",
		"description": "close the ledger only after reconciliation", "body": "Reconcile every ledger before closing it.\n"})
	r.ok(admin, "tasks_add", map[string]any{"project": lkFin, "title": "reconcile the ledger"})
	r.ok(admin, "notes_create", map[string]any{"project": lkFin, "title": "ledger notes", "body": "quarterly ledger notes"})
	r.ok(admin, "memory_write", map[string]any{"project": "global", "name": "ledger-tip", "kind": "reference",
		"description": "where the shared ledger templates live", "body": "The ledger templates live in the shared drive.\n"})
}

// lines splits a refusal into its lines.
func lines(s string) []string { return strings.Split(strings.TrimSpace(s), "\n") }

func projectRow(t *testing.T, listJSON, slug string) map[string]any {
	t.Helper()
	var out struct {
		Projects []map[string]any `json:"projects"`
	}
	require.NoError(t, json.Unmarshal([]byte(listJSON), &out))
	for _, p := range out.Projects {
		if p["slug"] == slug {
			return p
		}
	}
	return nil
}

func TestLockedView_OwnerSurfaces_LockedThenLeased(t *testing.T) {
	r := newLockRig(t)
	r.seed()
	admin := r.dial(r.serve(r.srv, nil), false)
	opsTask, _ := r.ok(admin, "tasks_add", map[string]any{"project": lkOps, "title": "rotate the ops keys"})
	var task struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(opsTask), &task))
	require.NotEmpty(t, task.ID)
	sessID := r.session("cc/fin", lkFin)
	agent := r.dial(r.socket(lkBridge, lkAgent), true)

	// project_list: the locked project's identity and counts, and the remedy.
	list, items := r.ok(agent, "project_list", nil)
	require.Len(t, items, 1, "no lease, no notices")
	fin := projectRow(t, list, lkFin)
	require.NotNil(t, fin, "a locked project is listed")
	require.Equal(t, true, fin["locked"])
	require.Equal(t, "confidential", fin["isolation"])
	require.Equal(t, map[string]any{"memories": float64(1), "notes": float64(1), "open_tasks": float64(1)}, fin["counts"])
	require.Contains(t, fin["remedy"], "seam access request "+lkFin)
	ops := projectRow(t, list, lkOps)
	require.NotNil(t, ops)
	require.NotContains(t, ops, "locked", "an unlocked row is as it always was")
	require.NotContains(t, ops, "counts")

	// recall with the session's locked project in scope: global results, the
	// locked project named (name only), and no request filed.
	recall, _ := r.ok(agent, "recall", map[string]any{"query": "ledger"})
	var rec struct {
		Hits   []map[string]any `json:"hits"`
		Locked []string         `json:"locked"`
	}
	require.NoError(t, json.Unmarshal([]byte(recall), &rec))
	require.Equal(t, []string{lkFin + " (locked) may hold results"}, rec.Locked)
	names := []string{}
	for _, h := range rec.Hits {
		names = append(names, h["name"].(string))
	}
	require.Contains(t, names, "ledger-tip")
	require.NotContains(t, names, "fin-ledger-rule", "nothing from inside the locked project")
	require.NotContains(t, recall, "reconcile")
	require.Empty(t, r.pending(), "a recall is a filter, not an ask")

	// A call whose scope is the session's locked project: refused at the lock,
	// one line of rule and one of remedy naming the request it filed.
	text := r.refused(agent, "tasks_list", nil)
	ls := lines(text)
	require.Len(t, ls, 2, text)
	require.Contains(t, ls[0], "project "+lkFin+" is locked")
	short := lkShortID.FindString(ls[1])
	require.NotEmpty(t, short, text)
	require.True(t, strings.HasPrefix(ls[1], "Remedy: access request "+short), ls[1])
	require.Contains(t, ls[1], "`seam approve "+short+"`")
	pending := r.pending()
	require.Len(t, pending, 1)
	require.Equal(t, sessID, pending[0].SessionID, "the request names the session the hook recorded")

	// Writes and session updates through that binding are refused the same
	// way -- never inferred into another project -- and the request is found
	// again, not filed again.
	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"memory_write", map[string]any{"name": "x", "kind": "gotcha", "description": "d", "body": "b\n"}},
		{"session_update", map[string]any{"findings": "secret findings"}},
		{"session_end", map[string]any{"findings": "secret findings"}},
		{"session_update", map[string]any{"findings": "secret findings", "session": "cc/fin"}},
		{"recall", map[string]any{"query": "ledger", "project": lkFin}},
		{"tasks_claim", map[string]any{"id": task.ID}},
		{"session_start", map[string]any{"name": "cc/fin"}},
	} {
		got := r.refused(agent, c.name, c.args)
		require.Contains(t, got, "is locked", c.name)
		require.Contains(t, got, short, "%s: the same request", c.name)
	}
	require.Len(t, r.pending(), 1)
	sess, ok, err := store.SessionByID(context.Background(), r.db, sessID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, sess.Findings, "findings are refused until leased (Q4)")
	require.Equal(t, core.SessionActive, sess.Status)

	// Scope errors name the locked project, marked.
	stranger := r.dial(r.socket(lkStranger), false)
	text = r.refused(stranger, "memory_write", map[string]any{"name": "x", "kind": "gotcha", "description": "d", "body": "b\n"})
	require.Contains(t, text, "known projects: "+lkFin+" (locked), "+lkOps)

	// The owner approves. The first result after the lease lands carries the
	// project's briefing; the next one does not.
	lease := r.approve(pending[0].ID, time.Hour)
	first, items := r.ok(agent, "tasks_list", nil)
	require.Contains(t, first, "reconcile the ledger", "the leased binding reads its project")
	require.Len(t, items, 2)
	require.Contains(t, items[1], "Access to "+lkFin+" was approved until "+access.Clock(lease.ExpiresAt))
	require.Contains(t, items[1], "fin-ledger-rule", "the project's briefing")
	_, items = r.ok(agent, "tasks_list", nil)
	require.Len(t, items, 1, "the briefing comes once per lease")

	list, _ = r.ok(agent, "project_list", nil)
	require.NotContains(t, projectRow(t, list, lkFin), "locked", "leased: listed as any project")
	recall, _ = r.ok(agent, "recall", map[string]any{"query": "ledger"})
	require.Contains(t, recall, "fin-ledger-rule")
	require.NotContains(t, recall, "may hold results")
	r.ok(agent, "session_update", map[string]any{"findings": "leased findings"})
	finTask, _ := r.ok(agent, "tasks_add", map[string]any{"title": "close the quarter"})
	require.NoError(t, json.Unmarshal([]byte(finTask), &task))
	r.ok(agent, "tasks_claim", map[string]any{"id": task.ID})
	// Leased, the session is bound to its confidential project, whose fence
	// now applies as to any session there.
	require.Contains(t, r.refused(agent, "memory_write", map[string]any{"project": lkOps, "name": "x", "kind": "gotcha",
		"description": "d", "body": "b\n"}), "writes outside it are disabled")

	// Another process tree stays locked.
	list, _ = r.ok(stranger, "project_list", nil)
	require.Equal(t, true, projectRow(t, list, lkFin)["locked"])
}

// The briefing after an approval comes exactly once per lease, however many
// connections of the agent's tree call at once.
func TestLockedView_BriefingOncePerLease_Concurrent(t *testing.T) {
	r := newLockRig(t)
	r.seed()
	r.session("cc/fin", lkFin)
	base := r.socket(lkBridge, lkAgent)
	agent := r.dial(base, true)
	r.refused(agent, "tasks_list", nil)
	pending := r.pending()
	require.Len(t, pending, 1)
	r.approve(pending[0].ID, time.Hour)

	const n = 8
	clients := make([]*mcpclient.Client, n)
	for i := range clients {
		clients[i] = r.dial(base, true)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	briefings := 0
	for _, cli := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := cli.CallTool(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "project_list"}})
			if err != nil || res.IsError {
				return
			}
			for _, c := range res.Content {
				if tc, ok := c.(mcp.TextContent); ok && strings.Contains(tc.Text, "This is its briefing") {
					mu.Lock()
					briefings++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, briefings)
}

// Step 8 on a fake clock: no warning at T-31m, a warning in every result from
// T-29m, a refresh replaces the lease (the old one revoked as refreshed), and
// after the end the next call is locked with the ended wording and files a
// new request. A session-length lease ends at session_end, before its cap.
func TestLeaseExpiry_WarningsRefreshAndEnd(t *testing.T) {
	ctx := context.Background()
	r := newLockRig(t)
	r.seed()
	sessID := r.session("cc/fin", lkFin)
	agent := r.dial(r.socket(lkBridge, lkAgent), true)
	t0 := r.clock()

	r.refused(agent, "tasks_list", nil)
	first := r.pending()
	require.Len(t, first, 1)
	lease := r.approve(first[0].ID, time.Hour)
	require.Equal(t, t0.Add(time.Hour), lease.ExpiresAt)
	_, items := r.ok(agent, "tasks_list", nil)
	require.Len(t, items, 2, "the briefing")

	r.set(t0.Add(29 * time.Minute)) // T-31m
	_, items = r.ok(agent, "tasks_list", nil)
	require.Len(t, items, 1, "no warning before T-30m")

	r.set(t0.Add(31 * time.Minute)) // T-29m
	for _, tool := range []string{"tasks_list", "project_list", "recall"} {
		args := map[string]any{}
		if tool == "recall" {
			args["query"] = "ledger"
		}
		_, items = r.ok(agent, tool, args)
		require.Len(t, items, 2, tool)
		require.Contains(t, items[1], "Access to "+lkFin+" ends at "+access.Clock(lease.ExpiresAt)+" (29 minutes left)")
		require.Contains(t, items[1], "`seam access request "+lkFin+"`")
		require.Contains(t, items[1], "a new approval")
	}

	// A refresh: a new request from the leased tree, approved -- a new lease,
	// the old one revoked as refreshed. The warning stops.
	leased, ok := r.policy.PeerChainView(501, []access.ProcessToken{lkBridge, lkAgent})
	require.True(t, ok)
	rows, err := store.ActiveAccessLeasesFor(ctx, r.db, []string{lkBridge.Key(), lkAgent.Key()}, r.clock())
	require.NoError(t, err)
	leased = leased.WithLeases(r.clock(), store.AccessLeases(rows))
	refresh, err := r.svc.Request(ctx, approval.RequestInput{Project: lkFin, View: leased, SessionID: sessID, Via: "request"})
	require.NoError(t, err)
	require.Equal(t, store.AccessKindRefresh, refresh.Kind)
	renewed := r.approve(refresh.ID, 4*time.Hour)
	var reason string
	require.NoError(t, r.db.QueryRowContext(ctx, `SELECT revoked_reason FROM access_leases WHERE id = ?`, lease.ID).Scan(&reason))
	require.Equal(t, store.LeaseRevokedRefreshed, reason)
	_, items = r.ok(agent, "tasks_list", nil)
	require.Len(t, items, 2, "the new lease's briefing, and no warning")
	require.Contains(t, items[1], "approved until "+access.Clock(renewed.ExpiresAt))
	require.NotContains(t, items[1], "ends at")

	// T+1s past the renewed lease: locked, with the ended wording, and a new
	// request is filed.
	r.set(renewed.ExpiresAt.Add(time.Second))
	text := r.refused(agent, "tasks_list", nil)
	require.Contains(t, lines(text)[0], "project "+lkFin+" is locked (access ended at "+access.Clock(renewed.ExpiresAt))
	again := r.pending()
	require.Len(t, again, 1)
	require.NotEqual(t, first[0].ID, again[0].ID)
	require.Equal(t, store.AccessKindRequest, again[0].Kind)
	require.Contains(t, text, approval.ShortID(again[0].ID))

	// A session-length lease, ended by session_end before its cap.
	length, err := r.svc.LeaseLength(approval.ForSession)
	require.NoError(t, err)
	sessLease := r.approve(again[0].ID, length)
	require.Equal(t, r.clock().Add(8*time.Hour), sessLease.ExpiresAt, "until the session ends, capped by max_lease")
	r.ok(agent, "tasks_list", nil)
	r.set(r.clock().Add(10 * time.Minute))
	ended := r.clock()
	r.ok(agent, "session_end", map[string]any{"findings": "done"})
	text = r.refused(agent, "tasks_list", map[string]any{"project": lkFin})
	require.Contains(t, lines(text)[0], "is locked (access ended at "+access.Clock(ended), "revoked at session_end, hours before the cap")
}

// The TCP key never holds a lease and files no request: its refusal is the
// rule and the remedy, two lines.
func TestLockedView_TCPRefusal(t *testing.T) {
	r := newLockRig(t)
	cli := r.dial(r.tcp(), true)
	text := r.refused(cli, "tasks_list", map[string]any{"project": lkFin})
	ls := lines(text)
	require.Len(t, ls, 2, text)
	require.Contains(t, ls[0], "bearer key never reach")
	require.True(t, strings.HasPrefix(ls[1], "Remedy: "), ls[1])
	require.Empty(t, r.pending())
}

// Off is unchanged: with no View on the request (peer_auth off) the server
// wired with the approval service answers byte for byte what a server
// without it answers, one text item per result, with none of the lock's
// fields.
func TestLockedView_OffIsUnchanged(t *testing.T) {
	r := newLockRig(t)
	r.seed()
	wired := r.dial(r.serve(r.srv, nil), true)
	plain := r.dial(r.serve(r.server(nil), nil), true)

	// A scope error (no session anywhere yet) lists plain slugs.
	scopeArgs := map[string]any{"name": "x", "kind": "gotcha", "description": "d", "body": "b\n"}
	a := r.refused(wired, "memory_write", scopeArgs)
	require.Equal(t, r.refused(plain, "memory_write", scopeArgs), a)
	require.Contains(t, a, "known projects: "+lkFin+", "+lkOps)

	r.session("cc/fin", lkFin)

	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"project_list", nil},
		{"recall", map[string]any{"query": "ledger"}},
		{"tasks_list", map[string]any{"project": lkFin}},
		{"tasks_ready", nil},
		{"memory_read", map[string]any{"name": "fin-ledger-rule", "project": lkFin}},
		{"notes_read", map[string]any{"slug": "ledger-notes", "project": lkFin}},
	} {
		a, aErr := r.call(wired, c.name, c.args)
		b, bErr := r.call(plain, c.name, c.args)
		require.Equal(t, bErr, aErr, c.name)
		require.Equal(t, b, a, "%s: byte-identical", c.name)
		require.Len(t, a, 1, "%s: one item", c.name)
		require.NotContains(t, a[0], `"locked"`, c.name)
		require.NotContains(t, a[0], "(locked)", c.name)
	}
	list, _ := r.ok(wired, "project_list", nil)
	fin := projectRow(t, list, lkFin)
	keys := make([]string, 0, len(fin))
	for k := range fin {
		keys = append(keys, k)
	}
	require.ElementsMatch(t, []string{"id", "slug", "name", "description", "isolation"}, keys)

}

// LastAccessLeaseEnd: the newest ended lease of a chain on a project, ended at
// its revocation or else its expiry; nothing for a tree that never held one.
func TestLastAccessLeaseEnd(t *testing.T) {
	ctx := context.Background()
	r := newLockRig(t)
	r.session("cc/fin", lkFin)
	keys := []string{lkBridge.Key(), lkAgent.Key()}

	_, ok, err := store.LastAccessLeaseEnd(ctx, r.db, keys, lkFin, r.clock())
	require.NoError(t, err)
	require.False(t, ok, "never leased")

	agent := r.dial(r.socket(lkBridge, lkAgent), true)
	r.refused(agent, "tasks_list", nil)
	lease := r.approve(r.pending()[0].ID, time.Hour)
	_, ok, err = store.LastAccessLeaseEnd(ctx, r.db, keys, lkFin, r.clock())
	require.NoError(t, err)
	require.False(t, ok, "a live lease has not ended")

	end, ok, err := store.LastAccessLeaseEnd(ctx, r.db, keys, lkFin, lease.ExpiresAt)
	require.NoError(t, err)
	require.True(t, ok, "expired at its end")
	require.True(t, end.Equal(lease.ExpiresAt))

	revokedAt := r.clock().Add(5 * time.Minute)
	require.NoError(t, store.RevokeAccessLease(ctx, r.db, lease.ID, store.LeaseRevokedOwner, revokedAt))
	end, ok, err = store.LastAccessLeaseEnd(ctx, r.db, keys, lkFin, revokedAt.Add(time.Second))
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, end.Equal(revokedAt), "revoked before its expiry: ended at the revocation")

	_, ok, err = store.LastAccessLeaseEnd(ctx, r.db, []string{lkStranger.Key()}, lkFin, revokedAt.Add(time.Second))
	require.NoError(t, err)
	require.False(t, ok, "another tree's lease is not this one's")
	_, ok, err = store.LastAccessLeaseEnd(ctx, r.db, keys, lkOps, revokedAt.Add(time.Second))
	require.NoError(t, err)
	require.False(t, ok)

	tooMany := make([]string, access.MaxChainHops+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("k%d", i)
	}
	_, _, err = store.LastAccessLeaseEnd(ctx, r.db, tooMany, lkFin, r.clock())
	require.ErrorIs(t, err, store.ErrInvalidAccessRequest)
}
