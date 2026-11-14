package main

// The read-grant matrix's daemon (isolation.requests, step G of the
// locked-projects plan): the real route table over real services, wired by
// the same decisions serve makes (main.go) from two switches -- peer_auth on
// or off, isolation.requests on or off -- and one socket server per injected
// kernel chain. Content is planted in every closed project with that
// project's secret in every content field; no probe ever sends a secret, so
// a secret in any response is a read across the fence.

import (
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
	"strings"
	"sync"
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
	// gmHome is the grantee's home: confidential, not locked. A read grant
	// is asked from a confidential home (plan fenced-sessions P4: from an
	// open home a refused read asks for a move instead).
	gmHome   = "example-home"
	gmOpen   = "example-open"    // open: another agent's home, and a move's
	gmDocs   = "example-docs"    // open: somewhere else to write
	gmConf   = "example-conf"    // confidential, not locked: the grant's target
	gmSealed = "example-sealed"  // isolation-sealed
	gmFin    = "example-finance" // confidential and locked under peer_auth

	gmKey  = "grant-matrix-key"
	gmHost = "laptop"
	gmUID  = 501
	gmBoot = "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"

	// gmSearch is a word every planted item carries, so search probes hit.
	gmSearch = "zq8grant"
	// gmDocsToken is in the open docs project's memory: any session reads it.
	gmDocsToken = "docs3token"

	gmClaude  = "/home/owner/.local/share/claude/versions/2.1.319"
	gmSeam    = "/usr/local/bin/seam"
	gmTmuxBin = "/opt/homebrew/Cellar/tmux/3.8/bin/tmux"
)

// gmSecrets is each closed project's secret: in every content field planted
// there, and in no probe.
var gmSecrets = map[string]string{gmConf: "conf5secret", gmSealed: "seal5secret", gmFin: "fin5secret"}

// The kernel chains the matrix injects, the peer first. One boot, so every
// token is valid together (access.Policy.PeerChainView).
var (
	gmAgent  = access.ProcessToken{PID: 200, Version: 2000, Boot: gmBoot} // the grantee: Claude Code
	gmBridge = access.ProcessToken{PID: 300, Version: 3000, Boot: gmBoot} // its seam mcp-proxy
	gmChild  = access.ProcessToken{PID: 320, Version: 3200, Boot: gmBoot} // seam run by its Bash tool
	gmGrand  = access.ProcessToken{PID: 330, Version: 3300, Boot: gmBoot} // a script under that

	gmSibling  = access.ProcessToken{PID: 600, Version: 6000, Boot: gmBoot} // same uid, outside the tree
	gmSibShell = access.ProcessToken{PID: 610, Version: 6100, Boot: gmBoot}
	// gmOrphan's parent link did not verify (it was re-parented, or its pid
	// was reused): Peer.Chain stops at it, so its chain is itself alone.
	gmOrphan = access.ProcessToken{PID: 700, Version: 7000, Boot: gmBoot}

	gmAgent2  = access.ProcessToken{PID: 500, Version: 5000, Boot: gmBoot} // another agent session's tree
	gmBridge2 = access.ProcessToken{PID: 510, Version: 5100, Boot: gmBoot}

	gmSealAgent  = access.ProcessToken{PID: 520, Version: 5200, Boot: gmBoot} // an agent in the sealed project
	gmSealBridge = access.ProcessToken{PID: 530, Version: 5300, Boot: gmBoot}

	gmFinAgent  = access.ProcessToken{PID: 800, Version: 8000, Boot: gmBoot} // the locked project's leased agent
	gmFinBridge = access.ProcessToken{PID: 810, Version: 8100, Boot: gmBoot}

	// T-anchor row 5: seam in a tmux pane, the tmux server above it (its
	// parent is pid 1, so the chain ends there).
	gmPane = access.ProcessToken{PID: 910, Version: 9100, Boot: gmBoot}
	gmTmux = access.ProcessToken{PID: 3698, Version: 36980, Boot: gmBoot}
)

// gmPrograms is the matrix's kernel naming each process (approval.Config
// .Programs): the agents are Claude Code, the bridges and CLIs seam, the
// shell zsh, the multiplexer tmux. Anything else the kernel no longer names.
func gmPrograms(t access.ProcessToken) string {
	switch t {
	case gmAgent, gmAgent2, gmSealAgent, gmFinAgent:
		return gmClaude
	case gmBridge, gmChild, gmBridge2, gmSealBridge, gmFinBridge, gmPane, gmSibling, gmOrphan:
		return gmSeam
	case gmSibShell:
		return "/bin/zsh"
	case gmTmux:
		return gmTmuxBin
	}
	return ""
}

func gmResolve(name string) (access.Account, error) {
	if name == "owner" {
		return access.Account{UID: gmUID, Name: "owner", Home: "/Users/owner"}, nil
	}
	return access.Account{}, fmt.Errorf("no such account %q", name)
}

// gmClock is the clock the MCP server, the approval service and the socket
// edge share, so a grant can be run past its end without sleeping.
type gmClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *gmClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *gmClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// gmItems is one project's planted content.
type gmItems struct {
	project, secret                    string
	memID, memName, noteID, noteSlug   string
	taskID, openTaskID, trialID, lab   string
	plan, sessID, sessName, proposalID string
}

// gmRig is one daemon as serve would wire it for the two switches.
type gmRig struct {
	t        *testing.T
	ctx      context.Context
	db       *sql.DB
	dir      string
	rec      *events.Recorder
	clock    *gmClock
	peer     bool
	requests bool
	policy   *access.Policy
	svc      *approval.Service
	grants   *approval.Grants
	mux      http.Handler
	tcp      *httptest.Server
	logger   *slog.Logger
	items    map[string]gmItems
}

// newGMRig builds the daemon. peer is peer_auth (the owner, and gmFin
// locked); requests is isolation.requests. Everything else follows main.go's
// own decisions: the approval service exists only under peer_auth while it
// locks a project or offers grants, the grants only with requests, the
// access_request tool only while a project is locked, the socket edge walks
// the owner's chain for grants only with requests.
func newGMRig(t *testing.T, peer, requests bool) *gmRig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(ctx, db, gmHost))
	for slug, state := range map[string]core.Isolation{
		gmHome: core.IsolationConfidential,
		gmOpen: core.IsolationOpen, gmDocs: core.IsolationOpen, gmConf: core.IsolationConfidential,
		gmSealed: core.IsolationSealed, gmFin: core.IsolationConfidential,
	} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, state))
		require.NoError(t, store.AddRepoMapping(ctx, db, "/work/"+slug, slug))
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &gmRig{t: t, ctx: ctx, db: db, dir: dir, peer: peer, requests: requests, logger: logger,
		clock: &gmClock{now: time.Now().UTC().Truncate(time.Second)}, items: map[string]gmItems{}}

	cfg := config.Defaults()
	cfg.Isolation.Requests = requests
	if peer {
		cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", ForceLocked: []string{gmFin}}
		r.policy, err = access.New(cfg.PeerAuth, gmResolve)
		require.NoError(t, err)
		problems, err := projectProblems(ctx, db, r.policy)
		require.NoError(t, err)
		require.Empty(t, problems, "the fixture passes serve's project preflight")
	}

	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	ret := retrieve.New(db, nil, config.Defaults().Budgets, nil)
	ret.SetBodyReader(mgr.Store())
	r.rec = events.NewRecorder(db)
	garden := gardener.New(db, mgr, nil, nil, r.rec, gardener.Config{}, nil)
	// Research on, so the trial tools are registered and probed; screening
	// built and off, as serve builds it.
	feats := config.Features{Research: true}
	ret.SetFeaturesConfig(feats)
	screen := screening.New(screening.Config{DB: db, Files: mgr, Events: r.rec, Gardener: garden,
		Enabled: func(context.Context) bool { return false }})

	if peer && (r.policy.HasLocked() || requests) {
		r.svc = gmApprovals(t, db, r.rec, r.clock)
	}
	if requests {
		r.grants = approval.NewGrants(approval.GrantsConfig{DB: db, Approvals: r.svc, Events: r.rec,
			Host: gmHost, LeaseCap: cfg.PeerAuth.LeaseCap(), Now: r.clock.Now, Logger: logger})
	}
	var mcpApprovals mcp.Approvals
	var hookApprovals hooks.Approvals
	var consoleApprovals console.ApprovalsView
	var revoke func(context.Context, string) (int64, error)
	if r.svc != nil {
		mcpApprovals, hookApprovals, consoleApprovals, revoke = r.svc, r.svc, r.svc, r.svc.RevokeSession
	}
	var mcpGrants mcp.Grants
	var consoleGrants console.GrantsView
	if r.grants != nil {
		mcpGrants, consoleGrants = r.grants, r.grants
		leaseRevoke := revoke
		revoke = func(ctx context.Context, sessionID string) (int64, error) {
			var n int64
			if leaseRevoke != nil {
				m, err := leaseRevoke(ctx, sessionID)
				if err != nil {
					return m, err
				}
				n = m
			}
			m, err := r.grants.RevokeSession(ctx, sessionID)
			return n + m, err
		}
	}
	mcpSrv := mcp.New(mcp.Config{
		DB: db, Files: mgr, Retrieve: ret, Events: r.rec, Gardener: garden, Screening: screen,
		APIKey: gmKey, LocalHost: gmHost, Features: feats, Now: r.clock.Now,
		Approvals: mcpApprovals, Grants: mcpGrants,
		OfferAccessRequest: peer && r.policy.HasLocked(),
	})
	a2aSrv, err := a2a.New(a2a.Config{Retrieve: ret, Events: r.rec, APIKey: gmKey, Version: "test", Endpoint: "http://127.0.0.1:8081/api/a2a"})
	require.NoError(t, err)
	hooksH := hooks.NewHandler(hooks.Config{
		DB: db, Retrieve: ret, Events: r.rec, Files: mgr, APIKey: gmKey, LocalHost: gmHost,
		Features: feats, Screening: screen, RevokeSessionLeases: revoke, Approvals: hookApprovals, ReadGrants: requests, Now: r.clock.Now,
	})
	consoleDB, consoleEvents := db, r.rec
	var consoleWriteDB *sql.DB
	var views *store.Views
	if peer && len(r.policy.ConsoleHidden()) > 0 {
		locked := r.policy.OwnerLocked()
		views, err = store.OpenViews(dbPath, locked)
		require.NoError(t, err)
		t.Cleanup(func() { _ = views.Close() })
		base, err := views.For(nil)
		require.NoError(t, err)
		consoleDB, consoleWriteDB, consoleEvents = base, db, r.rec.ReadingFrom(base)
	}
	consoleSrv, err := console.New(console.Config{
		DB: consoleDB, WriteDB: consoleWriteDB, Files: mgr, Gardener: garden, Events: consoleEvents, Retrieve: ret,
		Screening: screen, APIKey: gmKey, DataDir: dir, Features: feats,
		Approvals: consoleApprovals, Grants: consoleGrants, Views: views, Logger: logger,
	})
	require.NoError(t, err)
	r.mux = daemonRoutes{
		healthz: healthzHandler(db, "instance", nil), mcp: mcpSrv.Handler(), a2a: a2aSrv.Handler(),
		agentCard: a2aSrv.CardHandler(), hooks: hooksH, console: consoleSrv,
	}.mux()
	tcp := r.mux
	if peer {
		tcp = ownerHandler(r.policy, r.mux)
	}
	r.tcp = httptest.NewServer(tcp)
	t.Cleanup(func() { closeServer(r.tcp) })
	return r
}

// gmApprovals is the matrix's approval service: newApprovals' wiring (a
// passkey relying party, none enrolled), plus the injected kernel naming
// (Programs) and the shared clock.
func gmApprovals(t *testing.T, db *sql.DB, rec *events.Recorder, clock *gmClock) *approval.Service {
	t.Helper()
	v, err := passkey.New(passkey.Config{RPID: "localhost", Origins: []string{"http://localhost:8081"}})
	require.NoError(t, err)
	return approval.New(approval.Config{DB: db, Passkeys: v, Origins: []string{"http://localhost:8081"},
		UserName: "owner", UserDisplayName: "Seamless owner", Events: rec, Host: gmHost, LeaseCap: 8 * time.Hour,
		Programs: gmPrograms, Now: clock.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

// at is the rig as seen from subtest t: its helpers fail t, never the parent.
func (r *gmRig) at(t *testing.T) *gmRig {
	c := *r
	c.t = t
	return &c
}

// socket is a peer_auth socket server whose every connection the kernel
// reports as the owner's uid with this process chain, the peer first.
func (r *gmRig) socket(chain ...access.ProcessToken) *httptest.Server {
	r.t.Helper()
	require.True(r.t, r.peer, "a socket exists only under peer_auth")
	hops := make([]access.ChainHop, len(chain))
	for i, c := range chain {
		hops[i] = access.ChainHop{Token: c, Name: gmPrograms(c)}
	}
	cred := peerCred{uid: gmUID, chain: func() ([]access.ChainHop, error) { return hops, nil }}
	leases := storeLeases(r.db)
	leases.chains = r.requests
	leases.now = r.clock.Now
	h := peerHandler(r.policy, leases, r.logger, r.mux)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), peerCredKey{}, cred)))
	}))
	r.t.Cleanup(func() { closeServer(srv) })
	return srv
}

// dial connects an MCP client: key "" on the socket (the kernel is the
// credential), proc the agent-process header ("" for none).
func (r *gmRig) dial(base, key string, proc access.ProcessToken) *mcpclient.Client {
	r.t.Helper()
	headers := map[string]string{mcp.HostHeader: gmHost}
	if proc.Valid() {
		headers[mcp.AgentProcessHeader] = proc.AgentID()
	}
	return paClientHeaders(r.t, base, key, headers)
}

// stamp records an agent's ambient session in project, stamped with its
// process as its SessionStart hook would, and returns the session's id.
func (r *gmRig) stamp(project, name string, agent access.ProcessToken) string {
	r.t.Helper()
	id, err := core.NewID()
	require.NoError(r.t, err)
	now := time.Now().UTC()
	require.NoError(r.t, store.CreateSession(r.ctx, r.db, core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive, Ambient: true, Host: gmHost,
		ExternalSessionID: name + "-ext", ExternalClient: "claude-code", CWD: "/work/" + project, CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(r.t, store.SetSessionAgentProcess(r.ctx, r.db, id, agent.AgentID()))
	return id
}

// call calls a tool and returns its text items and whether it was an error.
func gmCall(t *testing.T, cli *mcpclient.Client, name string, args map[string]any) ([]string, bool) {
	t.Helper()
	res, err := cli.CallTool(context.Background(), mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: name, Arguments: args}})
	require.NoError(t, err, "%s transport error", name)
	var items []string
	for _, c := range res.Content {
		if tc, ok := c.(mcpgo.TextContent); ok {
			items = append(items, tc.Text)
		}
	}
	return items, res.IsError
}

func gmText(items []string) string { return strings.Join(items, "\n") }

func gmOK(t *testing.T, cli *mcpclient.Client, name string, args map[string]any) map[string]any {
	t.Helper()
	items, isErr := gmCall(t, cli, name, args)
	require.False(t, isErr, "%s failed: %v", name, items)
	require.NotEmpty(t, items, name)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(items[0]), &out), "%s: %v", name, items)
	return out
}

// plant writes one closed project's content through cli, the tools its own
// agents use, with the project's secret in every content field.
func (r *gmRig) plant(cli *mcpclient.Client, project, prefix string) gmItems {
	t := r.t
	t.Helper()
	secret := gmSecrets[project]
	it := gmItems{project: project, secret: secret, sessName: prefix + "-agent-session"}
	sess := gmOK(t, cli, "session_start", map[string]any{"project": project, "name": it.sessName})
	it.sessID = sess["session_id"].(string)
	require.Equal(t, project, sess["project"])
	it.memName = prefix + "-ledger-rule"
	it.memID = gmOK(t, cli, "memory_write", map[string]any{"name": it.memName, "kind": "gotcha",
		"description": gmSearch + " " + secret + " ledger description",
		"body":        "The " + gmSearch + " ledger rule: " + secret + " body text."})["id"].(string)
	note := gmOK(t, cli, "notes_create", map[string]any{"title": prefix + " " + gmSearch + " board memo",
		"description": secret + " memo description", "body": secret + " memo body " + gmSearch})
	it.noteID, it.noteSlug = note["id"].(string), note["slug"].(string)
	it.plan = prefix + "-close-plan"
	it.taskID = gmOK(t, cli, "tasks_add", map[string]any{"title": gmSearch + " " + secret + " reconcile",
		"body": secret + " task body", "plan": it.plan})["id"].(string)
	gmOK(t, cli, "tasks_claim", map[string]any{"id": it.taskID})
	// An open task outside the plan: the default tasks_list and tasks_ready
	// show it (plan steps are listed only with plan=).
	it.openTaskID = gmOK(t, cli, "tasks_add", map[string]any{"title": gmSearch + " " + secret + " open audit",
		"body": secret + " open task body"})["id"].(string)
	it.lab = prefix + "-forecast-lab"
	gmOK(t, cli, "lab_open", map[string]any{"lab": it.lab, "goal": secret + " goal"})
	it.trialID = gmOK(t, cli, "trial_record", map[string]any{"title": gmSearch + " " + secret + " trial",
		"changes": secret + " changes", "actual": secret})["id"].(string)
	gmOK(t, cli, "session_update", map[string]any{"findings": gmSearch + " " + secret + " findings"})
	p, err := store.CreateProposal(r.ctx, r.db, store.ProposalArchive, map[string]any{
		"id": it.memID, "project": project, "name": it.memName, "reason": secret + " stale"})
	require.NoError(t, err)
	it.proposalID = p.ID
	r.items[project] = it
	return it
}

// plantAll plants the three closed projects -- the confidential and the
// sealed one by the owner over TCP, the locked one by its own leased agent
// tree under peer_auth (the owner over TCP when there is no lock) -- and a
// memory in the open docs project.
func (r *gmRig) plantAll() {
	t := r.t
	t.Helper()
	own := paClient(t, r.tcp.URL, gmKey)
	r.plant(own, gmConf, "conf")
	r.plant(paClient(t, r.tcp.URL, gmKey), gmSealed, "seal")
	if r.peer {
		r.stamp(gmFin, "cc/fin-agent", gmFinAgent)
		now := r.clock.Now()
		a, err := store.CreateAccessApproval(r.ctx, r.db, store.AccessRequest{Project: gmFin, Kind: store.AccessKindRequest,
			Lease: 8 * time.Hour, TTL: time.Minute, PeerUID: gmUID, PeerPID: gmFinAgent.PID, ProcessKey: gmFinAgent.Key()}, now)
		require.NoError(t, err)
		_, err = store.ApproveAccessFor(r.ctx, r.db, a.ID, a.Challenge, "daemon-account", 8*time.Hour, now)
		require.NoError(t, err)
		r.plant(r.dial(r.socket(gmFinBridge, gmFinAgent).URL, "", gmFinAgent), gmFin, "fin")
	} else {
		r.plant(paClient(t, r.tcp.URL, gmKey), gmFin, "fin")
	}
	docs := paClient(t, r.tcp.URL, gmKey)
	gmOK(t, docs, "session_start", map[string]any{"project": gmDocs, "name": "docs-agent-session"})
	gmOK(t, docs, "memory_write", map[string]any{"name": "docs-style", "kind": "convention",
		"description": "docs " + gmSearch, "body": "docs body " + gmDocsToken})
}

// unchanged asserts one project's planted content survived, read straight
// from the database and the files: nothing wrote, moved, starred, claimed,
// ended or resolved any of it.
func (r *gmRig) unchanged(it gmItems, after string) {
	t := r.t
	t.Helper()
	ctx := r.ctx
	mem, ok, err := store.MemoryByID(ctx, r.db, it.memID)
	require.NoError(t, err)
	require.True(t, ok, "%s: the memory still exists", after)
	require.Nil(t, mem.InvalidAt, "%s: the memory is still active", after)
	require.False(t, mem.Favorite, "%s: nobody starred it", after)
	require.Equal(t, it.project, mem.Project, after)
	body, err := os.ReadFile(filepath.Join(r.dir, mem.FilePath))
	require.NoError(t, err)
	require.Contains(t, string(body), "ledger rule", "%s: the memory body was not edited", after)
	for _, s := range []string{"appended", "LEDGER", "probe"} {
		require.NotContains(t, string(body), s, after)
	}

	note, ok, err := store.NoteByID(ctx, r.db, it.noteID)
	require.NoError(t, err)
	require.True(t, ok, "%s: the note still exists", after)
	require.Equal(t, it.project, note.Project, after)
	require.False(t, note.Favorite, after)
	nb, err := os.ReadFile(filepath.Join(r.dir, note.FilePath))
	require.NoError(t, err)
	for _, s := range []string{"outsider", "MEMO"} {
		require.NotContains(t, string(nb), s, after)
	}

	task, err := store.TaskByID(ctx, r.db, it.taskID)
	require.NoError(t, err)
	require.Equal(t, core.TaskInProgress, task.Status, "%s: the task is still claimed", after)
	require.Equal(t, it.project, task.ProjectSlug, after)
	require.Equal(t, it.sessID, task.ClaimedBy, "%s: by its own agent", after)
	require.NotContains(t, task.Title, "outsider", after)
	require.False(t, task.Favorite, after)
	open, err := store.TaskByID(ctx, r.db, it.openTaskID)
	require.NoError(t, err)
	require.Equal(t, core.TaskOpen, open.Status, "%s: the open task is still open", after)
	require.Equal(t, it.project, open.ProjectSlug, after)
	require.Empty(t, open.ClaimedBy, after)

	trial, ok, err := store.TrialByID(ctx, r.db, it.trialID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, it.project, trial.ProjectSlug, after)
	require.False(t, trial.Favorite, after)

	sess, ok, err := store.SessionByID(ctx, r.db, it.sessID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionActive, sess.Status, "%s: nobody ended the session", after)
	require.NotContains(t, sess.Findings, "outsider", after)
	require.Equal(t, it.project, sess.ProjectSlug, after)

	p, ok, err := store.ProposalByID(ctx, r.db, it.proposalID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, store.ProposalPending, p.Status, "%s: nobody resolved the proposal", after)

	proj, ok, err := store.ProjectBySlug(ctx, r.db, it.project)
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, proj.Favorite, after)
	want := core.IsolationConfidential
	if it.project == gmSealed {
		want = core.IsolationSealed
	}
	require.Equal(t, want, proj.Isolation, "%s: isolation unchanged", after)

	r.noProbes(it.project, after)
}

// noProbes asserts no probe left an item in project.
func (r *gmRig) noProbes(project, after string) {
	t := r.t
	t.Helper()
	for _, probe := range []string{"probe-in", "probe-supersede", "probe-out"} {
		_, found, err := store.MemoryByName(r.ctx, r.db, project, probe)
		require.NoError(t, err)
		require.False(t, found, "%s: a probe wrote memory %s into %s", after, probe, project)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM notes_index WHERE project = ? AND title LIKE 'probe%'`,
		`SELECT COUNT(*) FROM tasks WHERE project_slug = ? AND title LIKE 'probe%'`,
		`SELECT COUNT(*) FROM trials WHERE project_slug = ? AND title LIKE 'probe%'`,
	} {
		require.Zero(t, r.count(q, project), "%s: %s", after, q)
	}
}

func (r *gmRig) count(query string, args ...any) int {
	r.t.Helper()
	var n int
	require.NoError(r.t, r.db.QueryRowContext(r.ctx, query, args...).Scan(&n))
	return n
}

// moveRows counts the move requests filed (access_approvals rows of kind
// move) matching where.
func (r *gmRig) moveRows(where string, args ...any) int {
	r.t.Helper()
	q := `SELECT COUNT(*) FROM access_approvals WHERE kind = 'move'`
	if where != "" {
		q += " AND " + where
	}
	return r.count(q, args...)
}

// grantRows counts the read grant requests filed (access_approvals rows of
// kind grant) matching where.
func (r *gmRig) grantRows(where string, args ...any) int {
	r.t.Helper()
	q := `SELECT COUNT(*) FROM access_approvals WHERE kind = 'grant'`
	if where != "" {
		q += " AND " + where
	}
	return r.count(q, args...)
}

// consoleDo makes one console request: over base (TCP with the key, or a
// socket with none), JSON unless asJSON is false, a form body for a POST.
func (r *gmRig) consoleDo(base, key, method, path string, form url.Values, headers map[string]string) (int, string) {
	t := r.t
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, base+path, body)
	require.NoError(t, err)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(out)
}

// hook posts one hook event over base.
func (r *gmRig) hook(base, key, event string, body map[string]any) (int, string) {
	t := r.t
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, base+"/api/hooks/"+event, strings.NewReader(string(raw)))
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

// noSecretsBut fails when text carries a closed project's secret other than
// the ones allowed.
func gmNoSecretsBut(t *testing.T, text, what string, allowed ...string) {
	t.Helper()
	for project, secret := range gmSecrets {
		ok := false
		for _, a := range allowed {
			ok = ok || a == project
		}
		if !ok {
			require.NotContains(t, text, secret, "%s leaked %s's content", what, project)
		}
	}
}

// gmProbe is one probe of a tool: its arguments, and -- for a read --
// whether a live grant must show the target's secret in its answer.
type gmProbe struct {
	args map[string]any
	want bool
}

// gmReadProbes is every read tool (ReadOnlyHint) in mcp.Catalog(), aimed at
// its project every way it can be: by project=, by id, by name, and the
// filters. want marks the reads a live grant must answer with the content.
func gmReadProbes(it gmItems) map[string][]gmProbe {
	p := it.project
	return map[string][]gmProbe{
		"memory_read": {{map[string]any{"project": p, "name": it.memName}, true}, {map[string]any{"id": it.memID}, true}},
		"notes_read":  {{map[string]any{"id": it.noteID}, true}, {map[string]any{"slug": it.noteSlug, "project": p}, true}},
		"recall": {
			{map[string]any{"query": gmSearch, "project": p}, true},
			// The granted project joins the session's own recall scope.
			{map[string]any{"query": gmSearch}, true},
			{map[string]any{"kind": "gotcha", "project": p}, true},
			{map[string]any{"query": gmSearch, "project": p, "scope": "notes"}, true},
			{map[string]any{"query": gmSearch, "project": p, "scope": "tasks"}, true},
			{map[string]any{"query": gmSearch, "project": p, "scope": "trials"}, true},
			{map[string]any{"query": gmSearch, "project": p, "scope": "sessions"}, true},
		},
		"tasks_list": {
			{map[string]any{"project": p}, true}, {map[string]any{"id": it.taskID}, true},
			{map[string]any{"project": p, "plan": it.plan}, true},
		},
		// The plan's one step is claimed (in progress): ready lists none of it.
		"tasks_ready": {{map[string]any{"project": p}, true}, {map[string]any{"project": p, "plan": it.plan}, false}},
		// "its trials show in trial_query" (Asking for access).
		"trial_query": {{map[string]any{"lab": it.lab}, true}},
		// A proposal about the target is the target's content: CanRead, and so
		// a live grant, decides it like every other read.
		"gardener_proposals": {{map[string]any{}, true}},
		"project_list":       {{map[string]any{}, false}},
		"usage_summary":      {{map[string]any{}, false}},
	}
}

// gmStateProbes are the tools that set only the caller's own session state
// while answering with what they read: lab_open names the session's lab and
// answers with the lab's recent trials, across the projects the caller may
// read (labs span projects by design). It is not refused; what it shows is a
// read, and it must leave the target unchanged.
func gmStateProbes(it gmItems) map[string][]map[string]any {
	return map[string][]map[string]any{"lab_open": {{"lab": it.lab}}}
}

// gmWriteProbes is every other tool in mcp.Catalog(), aimed at its project
// and items: each must be refused and leave them unchanged. session_start is
// separate (it rebinds the connection: gmRebindProbes).
func gmWriteProbes(it gmItems) map[string][]map[string]any {
	p := it.project
	edit := []any{map[string]any{"old_string": "ledger", "new_string": "LEDGER"}}
	return map[string][]map[string]any{
		"session_update": {{"session": it.sessName, "findings": "outsider overwrite"}, {"session": it.sessID, "findings": "outsider overwrite"}},
		"session_end":    {{"session": it.sessName, "findings": "outsider end"}, {"session": it.sessID, "findings": "outsider end"}},
		"memory_write": {
			{"project": p, "name": "probe-in", "kind": "gotcha", "description": "probe", "body": "probe"},
			{"project": p, "name": "probe-supersede", "kind": "gotcha", "description": "probe", "body": "probe", "supersedes": it.memName},
		},
		"memory_append":  {{"project": p, "name": it.memName, "body": "appended"}},
		"memory_edit":    {{"project": p, "name": it.memName, "edits": edit}},
		"memory_delete":  {{"project": p, "name": it.memName}},
		"notes_create":   {{"project": p, "title": "probe", "body": "probe"}},
		"notes_update":   {{"id": it.noteID, "body": "outsider rewrite"}, {"id": it.noteID, "project": gmOpen}},
		"notes_edit":     {{"id": it.noteID, "edits": []any{map[string]any{"old_string": "memo", "new_string": "MEMO"}}}},
		"notes_append":   {{"id": it.noteID, "body": "outsider append"}},
		"notes_delete":   {{"id": it.noteID}},
		"project_create": {{"name": "probe", "slug": p}},
		"tasks_add":      {{"project": p, "title": "probe"}},
		"tasks_update": {
			{"id": it.taskID, "status": "done"}, {"id": it.taskID, "project": gmOpen},
			{"id": it.taskID, "title": "outsider retitle"}, {"id": it.openTaskID, "status": "dropped"},
		},
		"tasks_claim":      {{"id": it.openTaskID}, {"id": it.taskID}},
		"tasks_release":    {{"id": it.taskID}},
		"trial_record":     {{"lab": it.lab, "project": p, "title": "probe"}},
		"gardener_request": {{"request": "tidy", "project": p}},
		"gardener_split":   {{"source": p}},
		"gardener_apply":   {{"id": it.proposalID}, {"id": it.proposalID, "action": "dismiss"}, {"id": it.proposalID, "action": "hide"}},
		"capture_url":      {{"url": "https://example.invalid/x", "project": p}},
		"access_request":   {{"project": p}, {"project": p, "reason": "asks to write"}},
		"favorite_set": {
			{"kind": "memory", "id": it.memID, "favorite": true}, {"kind": "note", "id": it.noteID, "favorite": true},
			{"kind": "task", "id": it.taskID, "favorite": true}, {"kind": "trial", "id": it.trialID, "favorite": true},
			{"kind": "session", "id": it.sessID, "favorite": true}, {"kind": "project", "id": p, "favorite": true},
			{"kind": "plan", "id": it.plan, "favorite": true},
		},
	}
}

// gmRebindProbes are session_start's probes: each on a connection of its own.
func gmRebindProbes(it gmItems) []map[string]any {
	return []map[string]any{{"project": it.project}, {"name": it.sessName}, {"cwd": "/work/" + it.project}}
}

// gmCatalogCovered fails when a tool in mcp.Catalog() has no probe row, or
// a read tool is probed as a write (or the reverse): a new tool is a new
// surface to prove.
func gmCatalogCovered(t *testing.T, it gmItems) {
	t.Helper()
	reads, writes, state := gmReadProbes(it), gmWriteProbes(it), gmStateProbes(it)
	for _, tool := range mcp.Catalog() {
		ro := tool.Annotations.ReadOnlyHint != nil && *tool.Annotations.ReadOnlyHint
		_, isRead := reads[tool.Name]
		_, isWrite := writes[tool.Name]
		if _, isState := state[tool.Name]; isState {
			require.False(t, ro || isRead || isWrite, "tool %s: a state probe is neither a read nor a write row", tool.Name)
			continue
		}
		if tool.Name == "session_start" {
			require.False(t, isRead || isWrite, "session_start is probed by gmRebindProbes")
			continue
		}
		require.True(t, isRead || isWrite, "mcp.Catalog() tool %s has no read-grant probe row: add one", tool.Name)
		require.Equal(t, ro, isRead, "tool %s: probed as a read %v, ReadOnlyHint %v", tool.Name, isRead, ro)
	}
}

// registered is the set of tools cli's server registers.
func gmRegistered(t *testing.T, cli *mcpclient.Client) map[string]bool {
	t.Helper()
	res, err := cli.ListTools(context.Background(), mcpgo.ListToolsRequest{})
	require.NoError(t, err)
	out := map[string]bool{}
	for _, tool := range res.Tools {
		out[tool.Name] = true
	}
	return out
}
