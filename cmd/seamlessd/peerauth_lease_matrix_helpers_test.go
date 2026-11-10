package main

// Helpers for the lease principals' matrix (peerauth_lease_matrix_test.go):
// the daemon (the real route table over real services, wired as serve wires
// them while peer_auth locks a project), the kernel process chains each
// principal connects with, the planted sentinels, and the probe table that
// aims every tool in mcp.Catalog() at one project.

import (
	"bufio"
	"bytes"
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
	"sync/atomic"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
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
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const (
	lmFin    = "example-finance" // confidential and locked
	lmLegal  = "example-legal"   // open and locked
	lmOpen   = "example-open"    // open
	lmDocs   = "example-docs"    // open, nothing planted
	lmConf   = "example-conf"    // confidential, not locked
	lmSealed = "example-sealed"  // isolation-sealed
	lmNone   = "example-nothere" // never registered

	// The secrets: one per closed project, in every content field planted
	// there and in no probe. Finding one in a response to a principal that
	// may not read that project is a leak.
	lmFinSecret    = "fq3secret"
	lmLegalSecret  = "lg6secret"
	lmConfSecret   = "cf2secret"
	lmSealedSecret = "sd9secret"
	// lmOpenToken is in the open project's content and lmGlobalToken in a
	// global memory: every principal here reads both.
	lmOpenToken   = "op5token"
	lmGlobalToken = "gb7token"
	// lmSearch is a word every planted item carries, so search probes hit.
	lmSearch = "wv4probe"

	lmKey  = "lease-matrix-key"
	lmHost = "example-host"

	lmUIDOwner    = 501
	lmUIDAgent    = 502 // the restricted account
	lmUIDStranger = 4244
	lmAccount     = "_example_agent"

	lmBoot = "4b1d0c2e-7a3f-4e59-8c61-0d2e4f6a8b9c"
)

// lmCommit is the synthetic commit id the commit-record probes send.
var lmCommit = strings.Repeat("cd", 20)

var lmAccounts = map[string]access.Account{
	"owner":   {UID: lmUIDOwner, Name: "owner", Home: "/Users/owner"},
	lmAccount: {UID: lmUIDAgent, Name: lmAccount, Home: "/Users/" + lmAccount},
}

func lmResolve(name string) (access.Account, error) {
	if a, ok := lmAccounts[name]; ok {
		return a, nil
	}
	return access.Account{}, fmt.Errorf("no such account %q", name)
}

// lmTok names one process of the fixture's boot.
func lmTok(pid int32) access.ProcessToken {
	return access.ProcessToken{PID: pid, Version: uint64(pid) * 1000, Boot: lmBoot}
}

// The process table. Every agent (claude) runs under one terminal; each has a
// bridge (`seam mcp-proxy`, the `seam` CLI) and a hook process (`seam hook`),
// and agent A also a shell running a script.
var (
	lmTerm = lmTok(100) // the terminal: a broad ancestor, never an anchor

	lmAgentA  = lmTok(200) // the leased agent: finance
	lmBridgeA = lmTok(210)
	lmHookA   = lmTok(211)
	lmShellA  = lmTok(220)
	lmScriptA = lmTok(221)

	lmAgentP  = lmTok(250) // leased on legal only
	lmBridgeP = lmTok(260)
	lmHookP   = lmTok(261)

	lmAgentB  = lmTok(300) // its 1h lease expires
	lmBridgeB = lmTok(310)
	lmHookB   = lmTok(311)

	lmAgentC  = lmTok(400) // its lease revoked by its session's session_end (MCP)
	lmBridgeC = lmTok(410)
	lmHookC   = lmTok(411)

	lmAgentD  = lmTok(500) // its lease revoked by the owner on the console
	lmBridgeD = lmTok(510)
	lmHookD   = lmTok(511)

	lmAgentE  = lmTok(550) // its lease revoked by its own SessionEnd hook
	lmBridgeE = lmTok(560)
	lmHookE   = lmTok(561)

	lmAgentU  = lmTok(600) // the owner's agent with no approval at all
	lmBridgeU = lmTok(610)
	lmHookU   = lmTok(611)

	lmAgentS  = lmTok(700) // a same-uid sibling: another agent under the terminal
	lmBridgeS = lmTok(710)
	lmHookS   = lmTok(711)

	lmOrphan = lmTok(800) // A's descendant whose parent died: reparented to pid 1

	lmAgentR  = lmTok(900) // the restricted account's agent
	lmBridgeR = lmTok(910)
	lmHookR   = lmTok(911)
)

// lmVictim is the leased agent the n-th cross-tree hook row aims at, with its
// hook process: a fresh lease per row, so one row's outcome cannot decide the
// next's.
func lmVictim(n int) (agent, hook access.ProcessToken) {
	return lmTok(int32(1000 + 20*n)), lmTok(int32(1001 + 20*n))
}

// lmPrograms is the kernel's name for each process (approval.Config.Programs):
// what the anchor rule judges. A victim's agent and hook are named by their
// pids' place in lmVictim's layout.
func lmPrograms(t access.ProcessToken) string {
	const (
		claude = "/Users/owner/.local/share/claude/versions/2.1.319"
		seam   = "/Users/owner/.local/bin/seam"
	)
	switch t {
	case lmTerm:
		return "/Applications/Ghostty.app/Contents/MacOS/ghostty"
	case lmAgentA, lmAgentP, lmAgentB, lmAgentC, lmAgentD, lmAgentE, lmAgentU, lmAgentS, lmAgentR:
		return claude
	case lmShellA:
		return "/bin/zsh"
	case lmScriptA, lmOrphan:
		return "/usr/bin/python3"
	case lmBridgeA, lmBridgeP, lmBridgeB, lmBridgeC, lmBridgeD, lmBridgeE, lmBridgeU, lmBridgeS, lmBridgeR,
		lmHookA, lmHookP, lmHookB, lmHookC, lmHookD, lmHookE, lmHookU, lmHookS, lmHookR:
		return seam
	}
	if t.Boot == lmBoot && t.PID >= 1000 && t.Version == uint64(t.PID)*1000 {
		if t.PID%20 == 0 {
			return claude
		}
		return seam
	}
	return ""
}

// lmClock is the daemon's clock: real time plus an offset the expiry rows
// move. The transport's lease judgment (peerLeases.now), the MCP server, the
// hooks and the approval service all read it.
type lmClock struct{ offset atomic.Int64 }

func (c *lmClock) now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

func (c *lmClock) advance(d time.Duration) { c.offset.Add(int64(d)) }

// lmItems is one project's planted content: what a principal probes for, and
// what must be unchanged afterwards.
type lmItems struct {
	project, secret, repo                                string
	memID, memName, noteID, noteSlug, planNoteID, taskID string
	trialID, lab, plan, sessID, sessName, proposalID     string
	// leaseSessID and leaseSessName name a second session in the project: in
	// a locked one, the session its agent's lease was granted to; leaseID is
	// that lease.
	leaseSessID, leaseSessName, leaseID string
}

// lmDaemon is the matrix's daemon.
type lmDaemon struct {
	db     *sql.DB
	dir    string
	mgr    *files.Manager
	rec    *events.Recorder
	policy *access.Policy
	svc    *approval.Service
	mux    *http.ServeMux
	clock  *lmClock
	owner  *httptest.Server // TCP, the bearer key
	logger *slog.Logger

	items map[string]lmItems
}

func newLMDaemon(t *testing.T) *lmDaemon {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(ctx, db, lmHost))
	for _, slug := range []string{lmFin, lmLegal, lmOpen, lmDocs, lmConf, lmSealed} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, lmFin, core.IsolationConfidential))
	require.NoError(t, store.SetProjectIsolation(ctx, db, lmConf, core.IsolationConfidential))
	require.NoError(t, store.SetProjectIsolation(ctx, db, lmSealed, core.IsolationSealed))
	for repo, slug := range map[string]string{"/work/finance": lmFin, "/work/legal": lmLegal, "/work/open": lmOpen,
		"/work/docs": lmDocs, "/work/conf": lmConf, "/work/sealed": lmSealed} {
		require.NoError(t, store.AddRepoMapping(ctx, db, repo, slug))
	}

	cfg := config.Defaults()
	cfg.PeerAuth = config.PeerAuth{
		Enabled: true, Owner: "owner",
		Socket:      config.PeerSocket{Path: "/opt/seamless/run/seamlessd.sock"},
		ForceLocked: []string{lmFin, lmLegal},
		Restricted:  []string{lmAccount},
	}
	policy, err := access.New(cfg.PeerAuth, lmResolve)
	require.NoError(t, err)
	// example-legal is locked while its isolation is open: serve's preflight
	// refuses that at start, so the state arises only from an isolation change
	// at run time. It is kept to prove the lock alone keeps it closed.
	problems, err := projectProblems(ctx, db, policy)
	require.NoError(t, err)
	require.Len(t, problems, 1, "%v", problems)
	require.Contains(t, problems[0], `project "`+lmLegal+`" is locked (peer_auth) but its isolation is open`)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	clock := &lmClock{}
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	ret := retrieve.New(db, nil, config.Defaults().Budgets, nil)
	ret.SetBodyReader(mgr.Store())
	rec := events.NewRecorder(db)
	garden := gardener.New(db, mgr, nil, nil, rec, gardener.Config{}, nil)
	// Write screening OFF: the lease rows are about reach; the screens have
	// their own matrix rows, and a restricted account's binding rules are
	// held whether or not screening is on.
	feats := config.Features{Research: true, Momentum: true, Gamification: true}
	ret.SetFeaturesConfig(feats)
	screen := screening.New(screening.Config{DB: db, Files: mgr, Events: rec, Gardener: garden,
		Enabled: func(context.Context) bool { return false }})

	rpID, origins := cfg.RelyingParty()
	verifier, err := passkey.New(passkey.Config{RPID: rpID, Origins: origins})
	require.NoError(t, err)
	svc := approval.New(approval.Config{
		DB: db, Passkeys: verifier, Origins: origins, UserName: "owner", UserDisplayName: "Seamless owner on " + lmHost,
		Events: rec, Host: lmHost, LeaseCap: cfg.PeerAuth.LeaseCap(), Programs: lmPrograms, Now: clock.now, Logger: logger,
	})

	mcpSrv := mcp.New(mcp.Config{
		DB: db, Files: mgr, Retrieve: ret, Events: rec, Gardener: garden, Screening: screen,
		APIKey: lmKey, LocalHost: lmHost, Features: feats, Logger: logger,
		RecordCommits: true, Approvals: svc, OfferAccessRequest: policy.HasLocked(), Now: clock.now,
	})
	a2aSrv, err := a2a.New(a2a.Config{Retrieve: ret, Events: rec, APIKey: lmKey, Version: "test",
		Endpoint: "http://127.0.0.1:8081/api/a2a", Logger: logger})
	require.NoError(t, err)
	hooksH := hooks.NewHandler(hooks.Config{
		DB: db, Retrieve: ret, Events: rec, Files: mgr, APIKey: lmKey, LocalHost: lmHost, Logger: logger,
		PlanCapture: config.PlanCapture{Enabled: true, AutoTask: true, InjectRelated: true},
		Features:    feats, Screening: screen, RevokeSessionLeases: svc.RevokeSession, Approvals: svc, Now: clock.now,
	})
	// The console as serve wires it under locked projects: a view pool, the
	// base handle for unleased reads, writes to the main handle.
	views, err := store.OpenViews(dbPath, policy.OwnerLocked())
	require.NoError(t, err)
	t.Cleanup(func() { _ = views.Close() })
	base, err := views.For(nil)
	require.NoError(t, err)
	consoleSrv, err := console.New(console.Config{
		DB: base, WriteDB: db, Files: mgr, Gardener: garden, Events: rec.ReadingFrom(base), Screening: screen,
		Retrieve: ret, APIKey: lmKey, DataDir: dir, Features: feats, Level: "advanced",
		Approvals: svc, Views: views, RestrictedAccounts: policy.HasRestricted(), Logger: logger,
	})
	require.NoError(t, err)
	mux := daemonRoutes{
		healthz: healthzHandler(db, "instance", nil), mcp: mcpSrv.Handler(), a2a: a2aSrv.Handler(),
		agentCard: a2aSrv.CardHandler(), hooks: hooksH, console: consoleSrv,
	}.mux()

	d := &lmDaemon{db: db, dir: dir, mgr: mgr, rec: rec, policy: policy, svc: svc, mux: mux, clock: clock, logger: logger,
		items: map[string]lmItems{}}
	d.owner = httptest.NewServer(ownerHandler(policy, mux))
	t.Cleanup(func() { closeServer(d.owner) })
	return d
}

// peer serves the socket for one connection whose kernel peer is uid and
// whose process chain (the peer first) is hops: the real peerHandler with the
// store's leases, judged at the daemon's clock.
func (d *lmDaemon) peer(t *testing.T, uid uint32, chain ...access.ProcessToken) *httptest.Server {
	t.Helper()
	hops := make([]access.ChainHop, len(chain))
	for i, c := range chain {
		hops[i] = access.ChainHop{Token: c, Name: filepath.Base(lmPrograms(c))}
	}
	cred := peerCred{uid: uid}
	if len(chain) > 0 {
		cred.chain = func() ([]access.ChainHop, error) { return hops, nil }
	}
	leases := storeLeases(d.db)
	leases.now = d.clock.now
	h := peerHandler(d.policy, leases, d.logger, d.mux)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), peerCredKey{}, cred)))
	}))
	t.Cleanup(func() { closeServer(srv) })
	return srv
}

// lmPrincipal is one row of the matrix: who connects, how, and which agent
// process (if any) its requests would be anchored at.
type lmPrincipal struct {
	name    string
	base    string
	key     string
	headers map[string]string
	// anchor is the process key a request this principal files names, ""
	// when it can file none (TCP, no agent in its chain, restricted).
	anchor string
	// leased lists the projects it holds a live lease on.
	leased []string
}

func (p lmPrincipal) client(t *testing.T) *mcpclient.Client {
	t.Helper()
	return paClientHeaders(t, p.base, p.key, p.headers)
}

// lmAgentHeaders are the headers a `seam` process under agent sends.
func lmAgentHeaders(agent access.ProcessToken) map[string]string {
	return map[string]string{mcp.AgentProcessHeader: agent.AgentID(), mcp.HostHeader: lmHost}
}

// hook posts one hook event to base as `seam hook` would: the agent process
// and host in the query, the repository roots for a session start. It
// returns the response body.
func (d *lmDaemon) hook(t *testing.T, base, key, event string, agent *access.ProcessToken, body map[string]any) string {
	t.Helper()
	status, out := d.hookStatus(t, base, key, event, agent, body)
	require.Equal(t, http.StatusOK, status, "hook %s: %s", event, out)
	return out
}

func (d *lmDaemon) hookStatus(t *testing.T, base, key, event string, agent *access.ProcessToken, body map[string]any) (int, string) {
	t.Helper()
	q := url.Values{"host": {lmHost}}
	if agent != nil {
		q.Set("agent_process", agent.AgentID())
	}
	if cwd, _ := body["cwd"].(string); cwd != "" && event == "session-start" {
		q.Set("repo_root", cwd)
		q.Set("main_root", cwd)
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, base+"/api/hooks/"+event+"?"+q.Encode(), bytes.NewReader(raw))
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
	return resp.StatusCode, string(out)
}

var lmShortRe = regexp.MustCompile(`R-[0-9A-Z]{4}`)

// approveShort runs the terminal path's commit (seamlessd approve's core) on
// request short, for duration, with the digest of the facts as they stand.
func (d *lmDaemon) approveShort(t *testing.T, short, duration string) store.AccessLease {
	t.Helper()
	ctx := context.Background()
	a, err := d.svc.Resolve(ctx, short)
	require.NoError(t, err, short)
	facts, err := d.svc.Facts(ctx, a.ID)
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, approve(ctx, d.svc, &out, []string{short, "--for", duration, "--digest", facts.Digest}))
	require.Contains(t, out.String(), "Approved "+short)
	after, err := store.AccessApprovalByID(ctx, d.db, a.ID)
	require.NoError(t, err)
	require.Equal(t, store.ApprovalApproved, after.Status)
	lease, err := store.AccessLeaseByID(ctx, d.db, after.LeaseID)
	require.NoError(t, err)
	return lease
}

// startLeasedAgent opens an agent session in project's repository through the
// agent's SessionStart hook -- which records the session there, metadata only,
// and files the access request -- then approves that request on the terminal
// path. It returns the session and the lease.
func (d *lmDaemon) startLeasedAgent(t *testing.T, hookSrv string, agent access.ProcessToken, claudeID, project, repo, duration string) (core.Session, store.AccessLease) {
	t.Helper()
	ctx := context.Background()
	notice := d.hook(t, hookSrv, "", "session-start", &agent, map[string]any{
		"session_id": claudeID, "cwd": repo, "source": "startup", "hook_event_name": "SessionStart"})
	require.Contains(t, notice, "project "+project+" is locked", notice)
	require.Contains(t, notice, "This session is recorded in "+project)
	short := lmShortRe.FindString(notice)
	require.NotEmpty(t, short, notice)
	sess, ok, err := store.AmbientSessionByExternalIdentity(ctx, d.db, "claude-code", claudeID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, project, sess.ProjectSlug)
	lease := d.approveShort(t, short, duration)
	require.Equal(t, agent.Key(), lease.ProcessKey, "the lease names the agent process, never the hook or the terminal")
	require.Equal(t, sess.ID, lease.SessionID)
	require.Equal(t, project, lease.Project)
	return sess, lease
}

// plantItems writes one project's content through cli -- the tools any of its
// agents uses -- with secret in every content field and lmSearch beside it.
func (d *lmDaemon) plantItems(t *testing.T, cli *mcpclient.Client, project, secret, prefix, repo string) lmItems {
	t.Helper()
	ctx := context.Background()
	it := lmItems{project: project, secret: secret, repo: repo}
	it.sessName = prefix + "-agent-session"
	sess := lmCallOK(t, cli, "session_start", map[string]any{"project": project, "name": it.sessName})
	it.sessID = sess["session_id"].(string)
	require.Equal(t, project, sess["project"])

	it.memName = prefix + "-ledger-rule"
	mem := lmCallOK(t, cli, "memory_write", map[string]any{
		"name": it.memName, "kind": "gotcha",
		"description": lmSearch + " " + secret + " ledger description",
		"body":        "The " + lmSearch + " ledger rule: " + secret + " body text.",
	})
	it.memID = mem["id"].(string)
	note := lmCallOK(t, cli, "notes_create", map[string]any{
		"title": prefix + " " + lmSearch + " board memo", "description": secret + " memo description",
		"body": secret + " memo body " + lmSearch,
	})
	it.noteID, it.noteSlug = note["id"].(string), note["slug"].(string)
	it.plan = prefix + "-close-plan"
	planNote := lmCallOK(t, cli, "notes_create", map[string]any{
		"title": prefix + " close plan narrative", "description": secret + " plan narrative",
		"body": "# Close plan\n\n" + secret + " plan body " + lmSearch, "tags": []any{"plan:" + it.plan},
	})
	it.planNoteID = planNote["id"].(string)
	task := lmCallOK(t, cli, "tasks_add", map[string]any{
		"title": lmSearch + " " + secret + " reconcile", "body": secret + " task body", "plan": it.plan,
	})
	it.taskID = task["id"].(string)
	// Claimed, so a refused force-release would have something to undo.
	lmCallOK(t, cli, "tasks_claim", map[string]any{"id": it.taskID})
	it.lab = prefix + "-forecast-lab"
	lmCallOK(t, cli, "lab_open", map[string]any{"lab": it.lab, "goal": secret + " goal"})
	trial := lmCallOK(t, cli, "trial_record", map[string]any{
		"title": lmSearch + " " + secret + " trial", "changes": secret + " changes", "actual": secret,
	})
	it.trialID = trial["id"].(string)
	lmCallOK(t, cli, "session_update", map[string]any{"findings": lmSearch + " " + secret + " findings"})
	// A call carrying content in its arguments: its tool.call event is this
	// project's content too.
	_, _ = paCall(t, cli, "recall", map[string]any{"query": lmSearch + " " + secret})

	// A gardener proposal about the memory, planted directly.
	p, err := store.CreateProposal(ctx, d.db, store.ProposalArchive, map[string]any{
		"id": it.memID, "project": project, "name": it.memName, "reason": secret + " stale",
	})
	require.NoError(t, err)
	it.proposalID = p.ID

	// Every row really landed where it was aimed, with the secret.
	m, ok, err := store.MemoryByID(ctx, d.db, it.memID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, project, m.Project)
	n, ok, err := store.NoteByID(ctx, d.db, it.planNoteID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, project, n.Project)
	tk, err := store.TaskByID(ctx, d.db, it.taskID)
	require.NoError(t, err)
	require.Equal(t, project, tk.ProjectSlug)
	tr, ok, err := store.TrialByID(ctx, d.db, it.trialID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, project, tr.ProjectSlug)
	s, ok, err := store.SessionByID(ctx, d.db, it.sessID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Contains(t, s.Findings, secret)
	return it
}

// lmGhostItems is an item set for a project nobody registered: fresh ids, and
// names after tag that name nothing, for the answers a closed project must
// share with a missing one. A fresh tag per probe keeps a probe that creates
// something under a ghost name (a session the caller then owns) from deciding
// the next probe's answer.
func lmGhostItems(t *testing.T, project, tag string) lmItems {
	t.Helper()
	id := func() string {
		v, err := core.NewID()
		require.NoError(t, err)
		return v
	}
	return lmItems{project: project, repo: "/work/nothere", memID: id(), memName: tag + "-ledger-rule", noteID: id(),
		noteSlug: tag + "-wv4probe-board-memo", planNoteID: id(), taskID: id(), trialID: id(), lab: tag + "-forecast-lab",
		plan: tag + "-close-plan", sessID: id(), sessName: tag + "-agent-session", proposalID: id(),
		leaseSessID: id(), leaseSessName: tag + "-second-session"}
}

// lmToolProbes is every way a caller might reach it's project through each
// MCP tool: by slug, by id, by name, by search word, by session. The matrix
// fails when a tool in mcp.Catalog() has no row.
func lmToolProbes(it lmItems) map[string][]map[string]any {
	edit := []any{map[string]any{"old_string": "ledger", "new_string": "LEDGER"}}
	sessionStart := []map[string]any{{"project": it.project}, {"name": it.sessName}, {"cwd": it.repo},
		{"project": it.project, "name": "probe-session"}, {"project": lmOpen, "name": it.sessName}}
	sessionUpdate := []map[string]any{{"session": it.sessName, "findings": "outsider overwrite"},
		{"session_id": it.sessID, "findings": "outsider overwrite"}}
	sessionEnd := []map[string]any{{"session": it.sessName, "findings": "outsider end"},
		{"session_id": it.sessID, "findings": "outsider end"},
		{"session": it.sessName, "findings": "outsider end", "commits": []any{lmCommit}}}
	if it.leaseSessID != "" {
		sessionStart = append(sessionStart, map[string]any{"name": it.leaseSessName})
		sessionUpdate = append(sessionUpdate, map[string]any{"session": it.leaseSessName, "findings": "outsider overwrite"},
			map[string]any{"session_id": it.leaseSessID, "findings": "outsider overwrite"})
		sessionEnd = append(sessionEnd, map[string]any{"session": it.leaseSessName, "findings": "outsider end"},
			map[string]any{"session_id": it.leaseSessID, "findings": "outsider end"})
	}
	return map[string][]map[string]any{
		"session_start":  sessionStart,
		"session_update": sessionUpdate,
		"session_end":    sessionEnd,
		"memory_write": {
			{"project": it.project, "name": "probe-in", "kind": "gotcha", "description": "probe", "body": "probe"},
			{"name": "probe-supersede", "kind": "gotcha", "description": "probe", "body": "probe", "supersedes": it.memName, "project": it.project},
			{"project": it.project, "name": "probe-rule", "kind": "constraint", "description": "probe", "body": "probe"},
		},
		"memory_append": {{"project": it.project, "name": it.memName, "body": "appended"}, {"name": it.memName, "body": "appended"}},
		"memory_edit":   {{"project": it.project, "name": it.memName, "edits": edit}, {"name": it.memName, "edits": edit}},
		"memory_read": {{"project": it.project, "name": it.memName}, {"id": it.memID}, {"name": it.memName},
			{"name": it.memName, "session": it.sessName}, {"name": it.memName, "session": it.leaseSessName}},
		"memory_delete": {{"project": it.project, "name": it.memName}, {"name": it.memName}},
		"recall": {
			{"query": lmSearch}, {"query": lmSearch, "project": it.project}, {"query": "ledger reconcile memo"},
			{"kind": "gotcha"}, {"kind": "gotcha", "project": it.project}, {"query": lmSearch, "scope": "sessions"},
			{"query": lmSearch, "scope": "tasks"}, {"query": lmSearch, "scope": "trials"}, {"query": lmSearch, "scope": "notes"},
			{"query": lmSearch, "project": it.project, "scope": "sessions"}, {"query": "close plan narrative", "project": it.project},
			{"query": lmSearch, "session": it.sessName}, {"query": lmSearch, "session": it.leaseSessID},
		},
		"notes_create": {{"project": it.project, "title": "probe", "body": "probe"},
			{"project": it.project, "title": "probe", "body": "probe", "tags": []any{"plan:" + it.plan}}},
		"notes_read": {{"id": it.noteID}, {"slug": it.noteSlug, "project": it.project}, {"slug": it.noteSlug},
			{"id": it.planNoteID}, {"slug": it.plan, "project": it.project}},
		"notes_update":   {{"id": it.noteID, "body": "outsider rewrite"}, {"id": it.noteID, "project": "global"}, {"id": it.noteID, "project": lmOpen}},
		"notes_edit":     {{"id": it.noteID, "edits": []any{map[string]any{"old_string": "memo", "new_string": "MEMO"}}}},
		"notes_append":   {{"id": it.noteID, "body": "outsider append"}, {"id": it.planNoteID, "body": "outsider append"}},
		"notes_delete":   {{"id": it.noteID}, {"id": it.planNoteID}},
		"project_list":   {{}},
		"project_create": {{"name": "probe", "slug": it.project}},
		"tasks_add":      {{"project": it.project, "title": "probe"}, {"project": it.project, "title": "probe", "plan": it.plan}},
		"tasks_update": {
			{"id": it.taskID, "status": "done"}, {"id": it.taskID, "project": "global"}, {"id": it.taskID, "project": lmOpen},
			{"id": it.taskID, "title": "outsider retitle"}, {"id": it.taskID, "commits": []any{lmCommit}},
		},
		"tasks_ready": {{"project": it.project}, {"project": it.project, "plan": it.plan}, {}},
		"tasks_list": {{"project": it.project}, {"id": it.taskID}, {"project": it.project, "plan": it.plan}, {},
			{"plan": it.plan, "session": it.leaseSessName}},
		"tasks_claim":        {{"id": it.taskID}},
		"tasks_release":      {{"id": it.taskID}},
		"lab_open":           {{"lab": it.lab}, {"lab": it.lab, "session": it.sessName}},
		"trial_record":       {{"lab": it.lab, "project": it.project, "title": "probe"}},
		"trial_query":        {{"lab": it.lab}, {"lab": it.lab, "session": it.sessName}, {}},
		"gardener_proposals": {{}, {"kind": "archive"}},
		"gardener_request":   {{"request": "tidy", "project": it.project}, {"request": "tidy", "project": "all"}},
		"gardener_split":     {{"source": it.project}},
		"gardener_apply":     {{"id": it.proposalID}, {"id": it.proposalID, "action": "dismiss"}, {"id": it.proposalID, "action": "hide"}},
		"capture_url":        {{"url": "https://example.invalid/x", "project": it.project}},
		"usage_summary":      {{}, {"session": it.sessName}},
		"access_request":     {{"project": it.project}, {"project": it.project, "reason": "outsider asks"}},
		"favorite_set": {
			{"kind": "memory", "id": it.memID, "favorite": true}, {"kind": "memory", "id": it.memName, "favorite": true},
			{"kind": "memory", "id": it.memName, "project": it.project, "favorite": true},
			{"kind": "note", "id": it.noteID, "favorite": true}, {"kind": "note", "id": it.noteSlug, "project": it.project, "favorite": true},
			{"kind": "session", "id": it.sessName, "favorite": true},
			{"kind": "task", "id": it.taskID, "favorite": true}, {"kind": "trial", "id": it.trialID, "favorite": true},
			{"kind": "session", "id": it.sessID, "favorite": true}, {"kind": "project", "id": it.project, "favorite": true},
			{"kind": "plan", "id": it.plan, "favorite": true},
		},
	}
}

// unchanged asserts one project's planted content survived, read straight
// from the database and the files: no probe wrote, moved, starred, claimed,
// ended or resolved any of it.
func (d *lmDaemon) unchanged(t *testing.T, it lmItems, after string) {
	t.Helper()
	ctx := context.Background()
	mem, ok, err := store.MemoryByID(ctx, d.db, it.memID)
	require.NoError(t, err)
	require.True(t, ok, "%s: the memory still exists", after)
	require.Nil(t, mem.InvalidAt, "%s: the memory is still active", after)
	require.False(t, mem.Favorite, "%s: no one starred it", after)
	require.Equal(t, it.project, mem.Project, after)
	body, err := os.ReadFile(filepath.Join(d.dir, mem.FilePath))
	require.NoError(t, err)
	require.Contains(t, string(body), "ledger rule", "%s: the memory body was not edited", after)
	require.NotContains(t, string(body), "appended", after)
	require.NotContains(t, string(body), "LEDGER", after)

	for _, id := range []string{it.noteID, it.planNoteID} {
		note, ok, err := store.NoteByID(ctx, d.db, id)
		require.NoError(t, err)
		require.True(t, ok, "%s: the note still exists", after)
		require.Equal(t, it.project, note.Project, after)
		require.False(t, note.Favorite, after)
		nb, err := os.ReadFile(filepath.Join(d.dir, note.FilePath))
		require.NoError(t, err)
		require.NotContains(t, string(nb), "outsider", after)
		require.NotContains(t, string(nb), "MEMO", after)
	}

	task, err := store.TaskByID(ctx, d.db, it.taskID)
	require.NoError(t, err)
	require.Equal(t, core.TaskInProgress, task.Status, "%s: the task is still claimed", after)
	require.Equal(t, it.project, task.ProjectSlug, after)
	require.Equal(t, it.sessID, task.ClaimedBy, "%s: by its own agent", after)
	require.NotContains(t, task.Title, "outsider", after)
	require.False(t, task.Favorite, after)

	trial, ok, err := store.TrialByID(ctx, d.db, it.trialID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, it.project, trial.ProjectSlug, after)
	require.False(t, trial.Favorite, after)

	for _, id := range []string{it.sessID, it.leaseSessID} {
		if id == "" {
			continue
		}
		sess, ok, err := store.SessionByID(ctx, d.db, id)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, core.SessionActive, sess.Status, "%s: no one ended session %s", after, id)
		require.NotContains(t, sess.Findings, "outsider", after)
		require.Equal(t, it.project, sess.ProjectSlug, after)
		require.False(t, sess.Favorite, after)
	}
	if it.leaseID != "" {
		l, err := store.AccessLeaseByID(ctx, d.db, it.leaseID)
		require.NoError(t, err)
		require.Nil(t, l.RevokedAt, "%s: no one revoked the planting agent's lease", after)
	}

	p, ok, err := store.ProposalByID(ctx, d.db, it.proposalID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, store.ProposalPending, p.Status, "%s: no one resolved the proposal", after)

	proj, ok, err := store.ProjectBySlug(ctx, d.db, it.project)
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, proj.Favorite, after)

	for _, probe := range []string{"probe-in", "probe-supersede", "probe-rule"} {
		_, found, err := store.MemoryByName(ctx, d.db, it.project, probe)
		require.NoError(t, err)
		require.False(t, found, "%s: %s was written into %s", after, probe, it.project)
	}
	for q, what := range map[string]string{
		`SELECT COUNT(*) FROM notes_index WHERE project = ? AND title = 'probe'`:          "a note",
		`SELECT COUNT(*) FROM tasks WHERE project_slug = ? AND title = 'probe'`:           "a task",
		`SELECT COUNT(*) FROM trials WHERE project_slug = ? AND title = 'probe'`:          "a trial",
		`SELECT COUNT(*) FROM sessions WHERE project_slug = ? AND name = 'probe-session'`: "a session",
		`SELECT COUNT(*) FROM held_writes WHERE project_slug = ?`:                         "a held write",
	} {
		require.Zero(t, lmCount(t, d.db, q, it.project), "%s: %s landed in %s", after, what, it.project)
	}
	require.Zero(t, lmCount(t, d.db, `SELECT COUNT(*) FROM commit_records WHERE owner_id IN (?, ?)`, it.sessID, it.taskID),
		"%s: no commit was recorded on its session or task", after)
}

func lmCount(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query, args...).Scan(&n))
	return n
}

// lmNoIdentifiers fails when text names one of it's items, unless the caller
// supplied that identifier itself (said): an error echoing what the caller
// asked for discloses nothing it did not hold. The project's slug is not an
// identifier here: a locked project is listed, by name, on purpose.
func lmNoIdentifiers(t *testing.T, it lmItems, text, said, what string) {
	t.Helper()
	for _, ident := range []string{it.memID, it.memName, it.noteID, it.noteSlug, it.planNoteID, it.taskID, it.trialID,
		it.sessID, it.sessName, it.leaseSessID, it.leaseSessName, it.proposalID} {
		if ident == "" || strings.Contains(said, ident) {
			continue
		}
		require.NotContains(t, text, ident, "%s named an item behind the lock", what)
	}
}

// lmA2ASend posts one A2A message/send and returns the raw response.
func lmA2ASend(t *testing.T, base, key, query string, meta map[string]any) string {
	t.Helper()
	msg := map[string]any{"role": "user", "kind": "message", "messageId": "m1",
		"parts": []map[string]any{{"kind": "text", "text": query}}}
	if meta != nil {
		msg["metadata"] = meta
	}
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": map[string]any{"message": msg}})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, base+"/api/a2a", bytes.NewReader(raw))
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

// lmSubscribe opens the console's live feed at base and returns its frames,
// once the stream is live.
func lmSubscribe(t *testing.T, base, key, feed string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/console/events"+feed, nil)
	require.NoError(t, err)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	frames := make(chan string, 1024)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for sc.Scan() {
			frames <- sc.Text()
		}
	}()
	require.Eventually(t, func() bool {
		select {
		case f := <-frames:
			return strings.HasPrefix(f, ": connected")
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond)
	return frames
}

// lmFramesUntil reads frames until one contains marker, failing on any frame
// check rejects, and returns every frame read.
func lmFramesUntil(t *testing.T, frames <-chan string, marker string, check func(string)) []string {
	t.Helper()
	var seen []string
	deadline := time.After(10 * time.Second)
	for {
		select {
		case f, open := <-frames:
			require.True(t, open, "feed closed before the marker %s", marker)
			check(f)
			seen = append(seen, f)
			if strings.Contains(f, marker) {
				return seen
			}
		case <-deadline:
			t.Fatalf("the marker %s never reached the feed", marker)
		}
	}
}

// lmCallOK is paCallOK reading a result's first text item: a leased
// connection's results carry the lease notices as later items.
func lmCallOK(t *testing.T, cli *mcpclient.Client, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cli.CallTool(context.Background(), mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: name, Arguments: args}})
	require.NoError(t, err, "%s transport error", name)
	require.NotEmpty(t, res.Content, name)
	first, ok := res.Content[0].(mcpgo.TextContent)
	require.True(t, ok, "%s: the first item is text", name)
	require.False(t, res.IsError, "%s failed: %s", name, first.Text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(first.Text), &out), "%s: %s", name, first.Text)
	return out
}
