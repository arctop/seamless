package main

// The blocker fence end to end: one daemon -- the real route table over real
// services, the owner on TCP (ownerHandler) and on the socket (peerHandler,
// with the kernel's chain injected), a restricted local account beside it --
// and a task in a locked, confidential project that blocks the open
// project's work. Every principal that may not read the locked project is
// told something blocks the open project's tasks, never what: no title, no
// id, in tasks_ready, tasks_list (by project and by id) and the claim's
// "task blocked" refusal. The agent tree the owner approved reads all of it.
// The console and the SessionStart briefing name no blocker either.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/a2a"
	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
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
	bfOps     = "example-ops"     // open
	bfFin     = "example-finance" // confidential and locked
	bfAccount = "_example_agent"  // restricted
	bfKey     = "blockers-fence-key"
	bfHost    = "laptop"
	bfBoot    = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"

	bfUIDOwner   = 501
	bfUIDAccount = 502

	// bfSecret is the locked task's title; no answer to an outsider holds it.
	bfSecret = "close the bf7secret ledger"
)

var (
	bfBridge   = access.ProcessToken{PID: 300, Version: 3000, Boot: bfBoot}
	bfAgent    = access.ProcessToken{PID: 200, Version: 2000, Boot: bfBoot}
	bfStranger = access.ProcessToken{PID: 400, Version: 4000, Boot: bfBoot}
	bfShortID  = regexp.MustCompile(`R-[0-9A-Z]{4}`)
)

func bfResolve(name string) (access.Account, error) {
	switch name {
	case "owner":
		return access.Account{UID: bfUIDOwner, Name: "owner", Home: "/Users/owner"}, nil
	case bfAccount:
		return access.Account{UID: bfUIDAccount, Name: bfAccount, Home: "/Users/" + bfAccount}, nil
	}
	return access.Account{}, fmt.Errorf("no such account %q", name)
}

type bfDaemon struct {
	db     *sql.DB
	svc    *approval.Service
	policy *access.Policy
	mux    http.Handler
	owner  *httptest.Server // TCP, the bearer key
	// tasks the fixture planted
	fin, prep, waits, both, finWaits string
}

func bfNewDaemon(t *testing.T) *bfDaemon {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(ctx, db, bfHost))
	for _, slug := range []string{bfOps, bfFin} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, bfFin, core.IsolationConfidential))
	require.NoError(t, store.AddRepoMapping(ctx, db, "/work/ops", bfOps))

	cfg := config.Defaults()
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", Locked: []string{bfFin}, Restricted: []string{bfAccount}}
	policy, err := access.New(cfg.PeerAuth, bfResolve)
	require.NoError(t, err)
	problems, err := projectProblems(ctx, db, policy)
	require.NoError(t, err)
	require.Empty(t, problems, "the fixture is a daemon serve would start")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	rec := events.NewRecorder(db)
	ret := retrieve.New(db, nil, config.Defaults().Budgets, nil)
	ret.SetBodyReader(mgr.Store())
	garden := gardener.New(db, mgr, nil, nil, rec, gardener.Config{}, nil)
	svc, err := newApprovals(cfg, db, rec, logger)
	require.NoError(t, err)
	mcpSrv := mcp.New(mcp.Config{DB: db, Files: mgr, Retrieve: ret, Events: rec, Gardener: garden,
		APIKey: bfKey, LocalHost: bfHost, Approvals: svc, OfferAccessRequest: true})
	a2aSrv, err := a2a.New(a2a.Config{Retrieve: ret, Events: rec, APIKey: bfKey, Version: "test", Endpoint: "http://127.0.0.1:8081/api/a2a"})
	require.NoError(t, err)
	hooksH := hooks.NewHandler(hooks.Config{DB: db, Retrieve: ret, Events: rec, Files: mgr, APIKey: bfKey, LocalHost: bfHost})
	// The console's handles as serve opens them while a project is locked.
	views, err := store.OpenViews(dbPath, nil, policy.OwnerLocked())
	require.NoError(t, err)
	t.Cleanup(func() { _ = views.Close() })
	viewDB, err := views.For(nil)
	require.NoError(t, err)
	consoleSrv, err := console.New(console.Config{
		DB: viewDB, WriteDB: db, Files: mgr, Gardener: garden, Events: rec.ReadingFrom(viewDB), Retrieve: ret,
		APIKey: bfKey, DataDir: dir, Level: "advanced", Approvals: svc, Views: views, Logger: logger,
		RestrictedAccounts: policy.HasRestricted(),
	})
	require.NoError(t, err)
	mux := daemonRoutes{
		healthz: healthzHandler(db, "instance", nil), mcp: mcpSrv.Handler(), a2a: a2aSrv.Handler(),
		agentCard: a2aSrv.CardHandler(), hooks: hooksH, console: consoleSrv,
	}.mux()

	d := &bfDaemon{db: db, svc: svc, policy: policy, mux: mux}
	d.owner = httptest.NewServer(ownerHandler(policy, mux))
	t.Cleanup(func() { closeServer(d.owner) })

	now := time.Now().UTC()
	task := func(project, title string, deps ...string) string {
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: id, ProjectSlug: project, Title: title,
			Status: core.TaskOpen, DependsOn: deps, CreatedAt: now, UpdatedAt: now}))
		now = now.Add(time.Second)
		return id
	}
	d.fin = task(bfFin, bfSecret)
	d.prep = task(bfOps, "ops prep")
	d.waits = task(bfOps, "ops waits on finance", d.fin)
	d.both = task(bfOps, "ops waits on both", d.fin, d.prep)
	d.finWaits = task(bfFin, "finance waits on both", d.fin, d.prep)
	return d
}

// socket is the peer_auth socket for uid, the kernel naming chain as the
// connecting process tree (none for an account whose process is not read).
func (d *bfDaemon) socket(t *testing.T, uid uint32, chain ...access.ProcessToken) *httptest.Server {
	t.Helper()
	cred := peerCred{uid: uid}
	if len(chain) > 0 {
		hops := make([]access.ChainHop, len(chain))
		for i, c := range chain {
			hops[i] = access.ChainHop{Token: c}
		}
		cred.chain = func() ([]access.ChainHop, error) { return hops, nil }
	}
	h := peerHandler(d.policy, storeLeases(d.db), slog.New(slog.NewTextHandler(io.Discard, nil)), d.mux)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerCredKey{}, cred)))
	}))
	t.Cleanup(func() { closeServer(srv) })
	return srv
}

// noBlocker fails when text names the locked task by title or id.
func (d *bfDaemon) noBlocker(t *testing.T, text, what string) {
	t.Helper()
	require.NotContains(t, text, bfSecret, "%s names the locked task's title", what)
	require.NotContains(t, text, d.fin, "%s names the locked task's id", what)
}

// bfBlockedRow returns tasks_ready's blocked entry for id.
func bfBlockedRow(t *testing.T, text, id string) map[string]any {
	t.Helper()
	var out struct {
		Blocked []map[string]any `json:"blocked"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &out), text)
	for _, b := range out.Blocked {
		if b["id"] == id {
			return b
		}
	}
	t.Fatalf("task %s not in the blocked list: %s", id, text)
	return nil
}

// bfHookPost posts a hook event as the owner (the bearer key) and returns the
// answer.
func bfHookPost(t *testing.T, base, event string, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, base+"/api/hooks/"+event, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bfKey)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "hook %s: %s", event, out)
	return string(out)
}

const bfWithheld = "a task in a project this connection may not reach blocks this one"

// bfWithheldFor is what each outsider is told in a withheld blocker's place:
// the owner's own connections name the locked project and the remedy for
// their transport (D7, 2026-11-08); a restricted account keeps the generic
// words.
var bfWithheldFor = map[string]string{
	"owner on TCP": "a task in locked project " + bfFin + " blocks this one: the TCP connection never reaches a locked project, " +
		"so work in it from an agent session on the peer_auth Unix socket",
	"owner socket without a lease": "a task in locked project " + bfFin + " blocks this one: bind to " + bfFin + ", or approve a request for it",
	"agent before approval":        "a task in locked project " + bfFin + " blocks this one: bind to " + bfFin + ", or approve a request for it",
	"restricted account":           bfWithheld,
}

func TestBlockerFence_Daemon(t *testing.T) {
	d := bfNewDaemon(t)
	ctx := context.Background()

	tcp := paClient(t, d.owner.URL, bfKey)
	paCallOK(t, tcp, "session_start", map[string]any{"project": bfOps, "name": "cc/bf-tcp"})
	stranger := paClient(t, d.socket(t, bfUIDOwner, bfStranger).URL, "")
	paCallOK(t, stranger, "session_start", map[string]any{"project": bfOps, "name": "cc/bf-stranger"})
	account := paClient(t, d.socket(t, bfUIDAccount).URL, "")
	paCallOK(t, account, "session_start", map[string]any{"project": bfOps, "name": "cc/bf-account"})

	// The agent's session, in the open project, stamped with its process by
	// the SessionStart hook; its tree is the one the owner approves below.
	sessID, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, d.db, core.Session{
		ID: sessID, Name: "cc/bf-agent", ProjectSlug: bfOps, Status: core.SessionActive, Ambient: true, Host: bfHost,
		ExternalSessionID: "bf-agent-x", ExternalClient: "claude-code", CWD: "/work/ops", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.SetSessionAgentProcess(ctx, d.db, sessID, bfAgent.AgentID()))
	agent := paClientHeaders(t, d.socket(t, bfUIDOwner, bfBridge, bfAgent).URL, "",
		map[string]string{mcp.AgentProcessHeader: bfAgent.AgentID(), mcp.HostHeader: bfHost})

	outsiders := map[string]*mcpclient.Client{
		"owner on TCP": tcp, "owner socket without a lease": stranger, "restricted account": account,
		"agent before approval": agent,
	}
	for name, cli := range outsiders {
		text, isErr := paCall(t, cli, "tasks_ready", map[string]any{"project": bfOps})
		require.False(t, isErr, "%s: %s", name, text)
		d.noBlocker(t, text, name+" tasks_ready")
		waits := bfBlockedRow(t, text, d.waits)
		require.Equal(t, []any{map[string]any{"withheld": bfWithheldFor[name]}}, waits["blockers"], name)
		require.NotContains(t, waits, "depends_on", name)
		require.Equal(t, float64(1), waits["depends_on_withheld"], name)
		both := bfBlockedRow(t, text, d.both)
		require.Len(t, both["blockers"], 2, "%s: the count is kept", name)
		require.Equal(t, []any{d.prep}, both["depends_on"], name)

		text, isErr = paCall(t, cli, "tasks_list", map[string]any{"project": bfOps})
		require.False(t, isErr, "%s: %s", name, text)
		d.noBlocker(t, text, name+" tasks_list")
		text, isErr = paCall(t, cli, "tasks_list", map[string]any{"id": d.both})
		require.False(t, isErr, "%s: %s", name, text)
		d.noBlocker(t, text, name+" tasks_list id=")
	}
	for name, cli := range map[string]*mcpclient.Client{
		"owner on TCP": tcp, "owner socket without a lease": stranger, "restricted account": account,
	} {
		text, isErr := paCall(t, cli, "tasks_claim", map[string]any{"id": d.both})
		require.True(t, isErr, "%s: %s", name, text)
		d.noBlocker(t, text, name+" tasks_claim")
		require.Contains(t, text, "task blocked", name)
		if name == "restricted account" {
			require.Contains(t, text, "waits on "+d.prep+" (open), a task in a project this connection may not reach", name)
		} else {
			require.Contains(t, text, "waits on "+d.prep+" (open), a task in locked project "+bfFin+"\nRemedy: ", name)
		}
	}
	pending, err := d.svc.Pending(ctx)
	require.NoError(t, err)
	require.Empty(t, pending, "withholding a blocker files no request")

	// The console names no locked task: its handle does not hold one.
	req, err := http.NewRequest(http.MethodGet, d.owner.URL+"/console/tasks", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bfKey)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	page, err := io.ReadAll(resp.Body)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(page))
	require.Contains(t, string(page), "ops waits on both")
	d.noBlocker(t, string(page), "console /console/tasks")

	// The SessionStart briefing lists the ready work, names no blocker.
	brief := bfHookPost(t, d.owner.URL, "session-start", map[string]any{
		"session_id": "bf-owner-claude-0001", "cwd": "/work/ops", "source": "startup",
	})
	require.Contains(t, brief, "ops prep")
	d.noBlocker(t, brief, "SessionStart briefing")

	// The owner approves the agent's request from the terminal; bound to the
	// locked project (its confidential isolation requires it for reads), the
	// approved tree reads the blocker in every answer.
	text, isErr := paCall(t, agent, "tasks_list", map[string]any{"project": bfFin})
	require.True(t, isErr, text)
	short := bfShortID.FindString(text)
	require.NotEmpty(t, short, text)
	pending, err = d.svc.Pending(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	var out bytes.Buffer
	require.NoError(t, approve(ctx, d.svc, &out, []string{short, "--for", "1h", "--digest", pending[0].Digest}))
	// The first answer after the approval also carries the project's briefing
	// (a second text item), so it is read as text.
	text, isErr = paCall(t, agent, "session_start", map[string]any{"project": bfFin, "cwd": "/work/finance"})
	require.False(t, isErr, text)

	text, isErr = paCall(t, agent, "tasks_ready", map[string]any{"project": bfOps})
	require.False(t, isErr, text)
	waits := bfBlockedRow(t, text, d.waits)
	require.Equal(t, []any{map[string]any{"id": d.fin, "status": "open", "title": bfSecret}}, waits["blockers"])
	require.Equal(t, []any{d.fin}, waits["depends_on"])
	require.NotContains(t, waits, "depends_on_withheld")
	text, isErr = paCall(t, agent, "tasks_claim", map[string]any{"id": d.finWaits})
	require.True(t, isErr, text)
	require.Contains(t, text, "waits on "+d.fin+" (open), "+d.prep+" (open)")

	// Every other principal is as before the approval.
	for name, cli := range map[string]*mcpclient.Client{
		"owner on TCP": tcp, "owner socket without a lease": stranger, "restricted account": account,
	} {
		text, isErr := paCall(t, cli, "tasks_ready", map[string]any{"project": bfOps})
		require.False(t, isErr, "%s: %s", name, text)
		d.noBlocker(t, text, name+" after the approval")
		require.True(t, strings.Contains(text, bfWithheldFor[name]), name)
	}
}
