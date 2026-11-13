package main

// The restricted-account rows of the peer_auth matrix (plan step A,
// peer_auth.restricted): one daemon -- the real route table over real
// services and the real peer handler, which loads the account's reach from the
// projects table per request -- with a project in every state (open, open and
// locked, confidential, isolation-sealed) and a restricted local account on the
// socket beside the owner. Write screening is OFF: a restricted account's
// binding rules must still wait for the owner's promotion.

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
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
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
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const (
	raOpen   = "example-open"    // open: the account reads and writes it
	raDocs   = "example-docs"    // open, nothing planted
	raLocked = "example-finance" // confidential and locked
	// raLockedOpen is locked while its isolation is open: only the lock keeps
	// the account out, not the projects table's isolation column.
	raLockedOpen = "example-legal"
	raConf       = "example-conf"    // confidential
	raSealed     = "example-sealed"  // isolation-sealed
	raNone       = "example-nothere" // never registered

	raAccount     = "_example_agent"
	raUIDOwner    = 501
	raUIDAgent    = 502
	raUIDStranger = 4243
	// raPeerAccount is a second restricted account, listed only by the
	// daemons that need one (raPolicy lists raAccount alone).
	raPeerAccount = "_example_peer"
	raUIDPeer     = 503

	// raSecret is in every content field planted in a project the account may
	// not reach, and in no probe.
	raSecret = "rq5secret"
	// raOpenToken is in the owner's open-project content, raGlobalToken in a
	// global memory: both readable by the account.
	raOpenToken   = "op3token"
	raGlobalToken = "gl8token"

	raKey = "peer-auth-restricted-key"
)

var raAccounts = map[string]access.Account{
	"_example_owner": {UID: raUIDOwner, Name: "_example_owner", Home: "/Users/_example_owner"},
	raAccount:        {UID: raUIDAgent, Name: raAccount, Home: "/Users/" + raAccount},
	raPeerAccount:    {UID: raUIDPeer, Name: raPeerAccount, Home: "/Users/" + raPeerAccount},
}

// raPolicy is the block every raDaemon policy is built from: the owner, the
// locked projects, and the restricted account.
func raPolicy() config.PeerAuth {
	return config.PeerAuth{
		Enabled: true, Owner: "_example_owner",
		Socket:      config.PeerSocket{Path: "/opt/seamless/run/seamlessd.sock"},
		ForceLocked: []string{raLocked, raLockedOpen},
		Restricted:  []string{raAccount},
	}
}

func raResolve(name string) (access.Account, error) {
	if a, ok := raAccounts[name]; ok {
		return a, nil
	}
	return access.Account{}, fmt.Errorf("no such account %q", name)
}

type raDaemon struct {
	db     *sql.DB
	mgr    *files.Manager
	screen *screening.Service
	policy *access.Policy
	mux    *http.ServeMux
	owner  *httptest.Server // TCP, the bearer key
	agent  *httptest.Server // the socket, the restricted account's uid
	// stranger is the socket for a uid nobody listed.
	stranger *httptest.Server
}

func newRADaemon(t *testing.T) *raDaemon {
	t.Helper()
	return newRADaemonFor(t, raPolicy())
}

// newRADaemonFor is newRADaemon under the peer_auth block cfg, whose accounts
// raResolve must know.
func newRADaemonFor(t *testing.T, cfg config.PeerAuth) *raDaemon {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(ctx, db, paHost))
	for _, slug := range []string{raOpen, raDocs, raLocked, raLockedOpen, raConf, raSealed} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, raLocked, core.IsolationConfidential))
	require.NoError(t, store.SetProjectIsolation(ctx, db, raConf, core.IsolationConfidential))
	require.NoError(t, store.SetProjectIsolation(ctx, db, raSealed, core.IsolationSealed))
	require.NoError(t, store.AddRepoMapping(ctx, db, "/work/open", raOpen))
	require.NoError(t, store.AddRepoMapping(ctx, db, "/Users/"+raAccount+"/repos/open", raOpen))
	require.NoError(t, store.AddRepoMapping(ctx, db, "/Users/"+raAccount+"/repos/conf", raConf))

	policy, err := access.New(cfg, raResolve)
	require.NoError(t, err)

	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	ret := retrieve.New(db, nil, config.Defaults().Budgets, nil)
	ret.SetBodyReader(mgr.Store())
	rec := events.NewRecorder(db)
	garden := gardener.New(db, mgr, nil, nil, rec, gardener.Config{}, nil)
	feats := config.Features{Research: true} // write screening OFF
	ret.SetFeaturesConfig(feats)
	screen := screening.New(screening.Config{DB: db, Files: mgr, Events: rec, Gardener: garden,
		Enabled: func(context.Context) bool { return false }})
	mcpSrv := mcp.New(mcp.Config{
		DB: db, Files: mgr, Retrieve: ret, Events: rec, Gardener: garden, Screening: screen,
		APIKey: raKey, LocalHost: paHost, Features: feats,
		OfferAccessRequest: true, Approvals: paApprovals(t, db),
	})
	a2aSrv, err := a2a.New(a2a.Config{Retrieve: ret, Events: rec, APIKey: raKey, Version: "test", Endpoint: "http://127.0.0.1:8081/api/a2a"})
	require.NoError(t, err)
	hooksH := hooks.NewHandler(hooks.Config{
		DB: db, Retrieve: ret, Events: rec, Files: mgr, APIKey: raKey, LocalHost: paHost,
		PlanCapture: config.PlanCapture{Enabled: true, AutoTask: true, InjectRelated: true},
		Features:    feats, Screening: screen,
	})
	viewDB, err := store.OpenView(dbPath, policy.ConsoleHidden())
	require.NoError(t, err)
	t.Cleanup(func() { _ = viewDB.Close() })
	consoleSrv, err := console.New(console.Config{
		DB: viewDB, WriteDB: db, Files: mgr, Gardener: garden, Events: rec.ReadingFrom(viewDB),
		Screening: screen, Retrieve: ret, APIKey: raKey, DataDir: dir, Features: feats, Level: "advanced",
		Approvals:          paApprovals(t, db),
		RestrictedAccounts: policy.HasRestricted(),
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)
	mux := daemonRoutes{
		healthz: healthzHandler(db, "instance", nil), mcp: mcpSrv.Handler(), a2a: a2aSrv.Handler(),
		agentCard: a2aSrv.CardHandler(), hooks: hooksH, console: consoleSrv,
	}.mux()

	d := &raDaemon{db: db, mgr: mgr, screen: screen, policy: policy, mux: mux}
	d.owner = httptest.NewServer(ownerHandler(policy, mux))
	t.Cleanup(func() { closeServer(d.owner) })
	d.agent, d.stranger = d.socketFor(t, raUIDAgent), d.socketFor(t, raUIDStranger)
	return d
}

// socketFor is the daemon's socket as the kernel names uid on it.
func (d *raDaemon) socketFor(t *testing.T, uid uint32) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(injectPeer(uid, peerHandler(d.policy, storeLeases(d.db), logger, d.mux)))
	t.Cleanup(func() { closeServer(srv) })
	return srv
}

// plantClosed writes a memory and a note straight through the files layer, as
// background work does (no caller, no fence): the closed projects' content the
// account must never see.
// It returns the planted ids: memory and note ids, by project.
func (d *raDaemon) plantClosed(t *testing.T) (mems, notes map[string]string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	mems, notes = map[string]string{}, map[string]string{}
	for _, p := range []string{raLocked, raLockedOpen, raConf, raSealed} {
		id, err := core.NewID()
		require.NoError(t, err)
		_, err = d.mgr.WriteMemory(ctx, core.Memory{ID: id, Kind: core.KindGotcha, Name: "closed-" + p,
			Description: "closed " + raSecret, Project: p, Body: "body " + raSecret,
			Created: now, Updated: now, ValidFrom: now})
		require.NoError(t, err)
		mems[p] = id
		nid, err := core.NewID()
		require.NoError(t, err)
		_, err = d.mgr.WriteNote(ctx, core.Note{ID: nid, Title: "Closed " + p, Slug: "closed-" + p, Project: p,
			Description: "closed " + raSecret, Body: "note " + raSecret, Created: now, Updated: now})
		require.NoError(t, err)
		notes[p] = nid
	}
	return mems, notes
}

func (d *raDaemon) ownerConsole(t *testing.T, method, path string, form url.Values) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, d.owner.URL+path, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+raKey)
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

func raCount(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(query, args...).Scan(&n))
	return n
}

func TestPeerAuthMatrix_Restricted(t *testing.T) {
	d := newRADaemon(t)
	closedMems, closedNotes := d.plantClosed(t)
	own := paClient(t, d.owner.URL, raKey)
	paCallOK(t, own, "memory_write", map[string]any{"project": raOpen, "name": "open-fact", "kind": "gotcha",
		"description": "open " + raOpenToken, "body": "open body " + raOpenToken})
	paCallOK(t, own, "memory_write", map[string]any{"project": raOpen, "name": "open-rule", "kind": "constraint",
		"description": "the owner's rule " + raOpenToken, "body": "rule body"})
	paCallOK(t, own, "memory_write", map[string]any{"project": "global", "name": "house-style", "kind": "convention",
		"description": "global " + raGlobalToken, "body": "global body " + raGlobalToken})
	agent := paClient(t, d.agent.URL, "")

	refusal := func(project string) string {
		return "project " + project + " is not reachable from account " + raAccount
	}

	t.Run("reach: open read-write, global read-only, everything else refused alike", func(t *testing.T) {
		text, isErr := paCall(t, agent, "memory_read", map[string]any{"project": raOpen, "name": "open-fact"})
		require.False(t, isErr, text)
		require.Contains(t, text, raOpenToken)
		text, isErr = paCall(t, agent, "memory_read", map[string]any{"project": "global", "name": "house-style"})
		require.False(t, isErr, text)
		require.Contains(t, text, raGlobalToken)
		text, isErr = paCall(t, agent, "memory_write", map[string]any{"project": "global", "name": "g", "kind": "gotcha",
			"description": "d", "body": "b"})
		require.True(t, isErr, "a global write is refused: %s", text)
		require.Contains(t, text, "the global scope is read-only for account "+raAccount)

		var texts []string
		for _, p := range []string{raLocked, raLockedOpen, raConf, raSealed, raNone} {
			text, isErr := paCall(t, agent, "memory_read", map[string]any{"project": p, "name": "closed-" + p})
			require.True(t, isErr, "read %s: %s", p, text)
			require.Contains(t, text, refusal(p))
			require.NotContains(t, text, raSecret)
			texts = append(texts, strings.ReplaceAll(text, p, "<p>"))
			wtext, wErr := paCall(t, agent, "memory_write", map[string]any{"project": p, "name": "probe", "kind": "gotcha",
				"description": "d", "body": "b"})
			require.True(t, wErr, "write %s: %s", p, wtext)
			require.Contains(t, wtext, refusal(p))
		}
		for _, other := range texts[1:] {
			require.Equal(t, texts[0], other, "a refusal never tells a closed project from a missing one")
		}
		require.Zero(t, raCount(t, d.db, `SELECT COUNT(*) FROM projects WHERE slug = ?`, raNone),
			"a restricted account creates no project")

		listed := paCallOK(t, agent, "project_list", map[string]any{})
		raw, _ := json.Marshal(listed)
		require.Contains(t, string(raw), raOpen)
		require.Contains(t, string(raw), raDocs)
		for _, p := range []string{raLocked, raLockedOpen, raConf, raSealed} {
			require.NotContains(t, string(raw), p, "a closed project is not listed for the account")
		}
		text, _ = paCall(t, agent, "recall", map[string]any{"query": raSecret})
		require.NotContains(t, text, raSecret)
	})

	t.Run("a closed project's ids answer exactly as missing ones", func(t *testing.T) {
		missing, _ := core.NewID()
		missMem, isErr := paCall(t, agent, "memory_read", map[string]any{"id": missing})
		require.True(t, isErr)
		missNote, isErr := paCall(t, agent, "notes_read", map[string]any{"id": missing})
		require.True(t, isErr)
		for p, id := range closedMems {
			text, isErr := paCall(t, agent, "memory_read", map[string]any{"id": id})
			require.True(t, isErr, text)
			require.Equal(t, strings.ReplaceAll(missMem, missing, id), text, "memory id in %s", p)
			require.NotContains(t, text, p)
		}
		for p, id := range closedNotes {
			for _, tool := range []string{"notes_read", "notes_delete"} {
				text, isErr := paCall(t, agent, tool, map[string]any{"id": id})
				require.True(t, isErr, text)
				require.NotContains(t, text, p, "%s of a note in %s", tool, p)
				require.NotContains(t, text, raSecret)
			}
			text, _ := paCall(t, agent, "notes_read", map[string]any{"id": id})
			require.Equal(t, strings.ReplaceAll(missNote, missing, id), text, "note id in %s", p)
			text, isErr := paCall(t, agent, "notes_edit", map[string]any{"id": id,
				"edits": []any{map[string]any{"old_string": "note", "new_string": "x"}}})
			require.True(t, isErr, text)
			require.NotContains(t, text, p)
		}
		// The owner's refusal still names the fence it met.
		text, isErr := paCall(t, own, "memory_read", map[string]any{"id": closedMems[raConf]})
		require.True(t, isErr, text)
		require.Contains(t, text, "project "+raConf+" is confidential", "the owner keeps the fence's own refusal")
	})

	t.Run("no requests, grants or leases", func(t *testing.T) {
		text, isErr := paCall(t, agent, "access_request", map[string]any{"project": raLocked, "reason": "please"})
		require.True(t, isErr, text)
		require.Zero(t, raCount(t, d.db, `SELECT COUNT(*) FROM access_approvals`))
		require.Zero(t, raCount(t, d.db, `SELECT COUNT(*) FROM access_grants`))
		require.Zero(t, raCount(t, d.db, `SELECT COUNT(*) FROM access_leases`))
	})

	t.Run("an unlisted uid is refused at the door", func(t *testing.T) {
		resp, err := http.Post(d.stranger.URL+"/api/mcp", "application/json", strings.NewReader(`{}`))
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("the account has no console", func(t *testing.T) {
		for _, path := range []string{"/console", "/console/held", "/console/memories"} {
			resp, err := http.Get(d.agent.URL + path)
			require.NoError(t, err)
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			require.NotEqual(t, http.StatusOK, resp.StatusCode, "%s: %s", path, body)
		}
	})

	var memID, noteID, taskID string
	t.Run("provenance: every write labeled, every surface shows it", func(t *testing.T) {
		sess := paCallOK(t, agent, "session_start", map[string]any{"project": raOpen, "name": "agent-s1"})
		require.Equal(t, raAccount, sess["account"], "%v", sess)
		w := paCallOK(t, agent, "memory_write", map[string]any{"session": "agent-s1", "project": raOpen,
			"name": "agent-fact", "kind": "gotcha", "description": "agent fact", "body": "b"})
		memID, _ = w["id"].(string)
		n := paCallOK(t, agent, "notes_create", map[string]any{"session": "agent-s1", "project": raOpen,
			"title": "Agent note", "body": "note body"})
		noteID, _ = n["id"].(string)
		tk := paCallOK(t, agent, "tasks_add", map[string]any{"session": "agent-s1", "project": raOpen, "title": "agent task"})
		taskID, _ = tk["id"].(string)
		require.NotEmpty(t, memID)
		require.NotEmpty(t, noteID)
		require.NotEmpty(t, taskID)
		paCallOK(t, agent, "session_end", map[string]any{"session": "agent-s1", "findings": "agent finding"})

		require.Equal(t, raAccount, paCallOK(t, own, "memory_read", map[string]any{"project": raOpen, "name": "agent-fact"})["account"])
		require.Equal(t, raAccount, paCallOK(t, own, "notes_read", map[string]any{"id": noteID})["account"])
		text, _ := paCall(t, own, "tasks_list", map[string]any{"id": taskID})
		require.Contains(t, text, `"account":"`+raAccount+`"`)
		text, _ = paCall(t, own, "recall", map[string]any{"query": "agent fact", "project": raOpen})
		require.Contains(t, text, `"account":"`+raAccount+`"`)

		// The owner's own writes carry no label anywhere.
		mine := paCallOK(t, own, "memory_read", map[string]any{"project": raOpen, "name": "open-fact"})
		require.NotContains(t, mine, "account")

		for _, path := range []string{"/console/memories/" + memID, "/console/notes/" + noteID, "/console/tasks/" + taskID} {
			req, err := http.NewRequest(http.MethodGet, d.owner.URL+path, nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+raKey)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode, path)
			require.Contains(t, string(body), "written by account "+raAccount, path)
		}

		// The owner's briefing in the open project shows the label beside the
		// account's memory.
		ownerHook := paHook(t, d.owner.URL, raKey, "session-start", map[string]any{
			"session_id": "owner-claude-1", "cwd": "/work/open", "source": "startup", "hook_event_name": "SessionStart"})
		require.Contains(t, ownerHook, "written by account "+raAccount)
	})

	t.Run("sessions belong to the principal that started them", func(t *testing.T) {
		paCallOK(t, own, "session_start", map[string]any{"project": raOpen, "name": "owner-s1"})
		text, isErr := paCall(t, agent, "session_update", map[string]any{"session": "owner-s1", "findings": "hijack"})
		require.True(t, isErr, text)
		text, isErr = paCall(t, own, "session_update", map[string]any{"session": "agent-s1", "findings": "hijack"})
		require.True(t, isErr, text)
	})

	t.Run("promotion: a binding rule waits for the owner", func(t *testing.T) {
		before := paCallOK(t, own, "memory_read", map[string]any{"project": raOpen, "name": "open-rule"})

		p := paCallOK(t, agent, "memory_write", map[string]any{"project": raOpen, "name": "agent-rule",
			"kind": "constraint", "description": "agent rule zz9prop", "body": "always zz9prop"})
		require.Equal(t, "proposed", p["status"], "%v", p)
		require.Equal(t, raAccount, p["proposed_by"])
		hold, _ := p["hold"].(map[string]any)
		holdID, _ := hold["id"].(string)
		require.NotEmpty(t, holdID)

		// A restricted edit of the owner's constraint is a proposal against it.
		e := paCallOK(t, agent, "memory_append", map[string]any{"project": raOpen, "name": "open-rule", "body": "and zz9prop"})
		require.Equal(t, "proposed", e["status"], "%v", e)
		after := paCallOK(t, own, "memory_read", map[string]any{"project": raOpen, "name": "open-rule"})
		require.Equal(t, before["content_hash"], after["content_hash"], "the owner's constraint is unchanged until promoted")

		// Hidden from binding text: absent from the corpus, recall and briefings,
		// the writer's own included.
		for _, cli := range []*mcpclient.Client{own, agent} {
			text, isErr := paCall(t, cli, "memory_read", map[string]any{"project": raOpen, "name": "agent-rule"})
			require.True(t, isErr, text)
			text, _ = paCall(t, cli, "recall", map[string]any{"query": "zz9prop", "project": raOpen})
			require.NotContains(t, text, "zz9prop")
		}
		brief := paHook(t, d.owner.URL, raKey, "session-start", map[string]any{
			"session_id": "owner-claude-2", "cwd": "/work/open", "source": "startup", "hook_event_name": "SessionStart"})
		require.NotContains(t, brief, "zz9prop")

		// The owner sees it as the account's proposal, and promotes it.
		status, body := d.ownerConsole(t, http.MethodGet, "/console/held?format=json", nil)
		require.Equal(t, http.StatusOK, status, body)
		require.Contains(t, body, raAccount)
		require.True(t, screening.IsProposal([]string{string(screening.ReasonRestrictedAccount)}))
		status, body = d.ownerConsole(t, http.MethodPost, "/console/held/"+holdID+"/release", url.Values{})
		require.Less(t, status, 400, body)
		landed := paCallOK(t, own, "memory_read", map[string]any{"project": raOpen, "name": "agent-rule"})
		require.Equal(t, "constraint", landed["kind"])
		require.Equal(t, raAccount, landed["account"], "a promoted rule keeps its writer's label")
		// The account cannot promote: it has no console.
		resp, err := http.Post(d.agent.URL+"/console/held/"+holdID+"/release", "application/x-www-form-urlencoded", nil)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.NotEqual(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("refused curation", func(t *testing.T) {
		text, isErr := paCall(t, agent, "favorite_set", map[string]any{"kind": "memory", "id": memID, "favorite": true})
		require.True(t, isErr, text)
	})

	t.Run("revival: an expired session comes back for its own principal only", func(t *testing.T) {
		raRevivalRows(t)
	})
}

// raRevivalRows: an agent whose session the idle reaper expired gets it back
// on its next call from the same live process -- a restricted account's agent
// exactly as the owner's -- and no agent-process header, the account's or the
// owner's, reopens another principal's session. A daemon of its own, with a
// second restricted account listed, so every session here starts the way an
// agent's does: from its SessionStart hook, on its own transport.
func raRevivalRows(t *testing.T) {
	cfg := raPolicy()
	cfg.Restricted = []string{raAccount, raPeerAccount}
	rv := newRADaemonFor(t, cfg)
	ctx := context.Background()
	require.NoError(t, store.AddRepoMapping(ctx, rv.db, "/work/docs", raDocs))
	peerSock := rv.socketFor(t, raUIDPeer)

	const (
		ownerProc  = "6100.61000"
		agentProc  = "6200.62000"
		agentProc2 = "6300.63000" // a second agent of the account's, in raDocs
		peerProc   = "6400.64000"
	)
	start := func(base, key, proc, sessionID, cwd string) {
		q := url.Values{"host": {paHost}, "agent_process": {proc}, "repo_root": {cwd}, "main_root": {cwd}}
		paHook(t, base+"/api/hooks/session-start?"+q.Encode(), key, "", map[string]any{
			"session_id": sessionID, "cwd": cwd, "source": "startup", "hook_event_name": "SessionStart"})
	}
	start(rv.owner.URL, raKey, ownerProc, "rv-owner", "/work/open")
	start(rv.agent.URL, "", agentProc, "rv-agent", "/work/open")
	start(rv.agent.URL, "", agentProc2, "rv-agent-docs", "/work/docs")
	start(peerSock.URL, "", peerProc, "rv-peer", "/work/open")

	type rvSess struct{ id, name, project, account, status string }
	byProc := func(proc string) rvSess {
		t.Helper()
		var s rvSess
		require.NoError(t, rv.db.QueryRow(`SELECT id, name, project_slug, account, status FROM sessions
			WHERE ambient = 1 AND agent_process = ?`, proc).Scan(&s.id, &s.name, &s.project, &s.account, &s.status))
		return s
	}
	for proc, want := range map[string][2]string{
		ownerProc: {raOpen, ""}, agentProc: {raOpen, raAccount}, agentProc2: {raDocs, raAccount}, peerProc: {raOpen, raPeerAccount},
	} {
		s := byProc(proc)
		require.Equal(t, "active", s.status, "%s: its hook started it", proc)
		require.Equal(t, want[0], s.project, proc)
		require.Equal(t, want[1], s.account, proc)
	}
	_, err := store.ExpireStaleSessions(ctx, rv.db, time.Now().UTC().Add(time.Hour))
	require.NoError(t, err)
	// Since its agent went quiet, raDocs was made confidential: the account no
	// longer reaches it.
	require.NoError(t, store.SetProjectIsolation(ctx, rv.db, raDocs, core.IsolationConfidential))

	revivals := func(id string) int {
		return raCount(t, rv.db, `SELECT COUNT(*) FROM events WHERE kind = 'session.started'
			AND session_id = ? AND json_extract(payload, '$.revived') = 1`, id)
	}
	dial := func(base, key, proc string) *mcpclient.Client {
		return paClientHeaders(t, base, key, map[string]string{mcp.AgentProcessHeader: proc, mcp.HostHeader: paHost})
	}

	// No: each call is unbound, and the session its header names stays expired.
	for _, row := range []struct {
		label, base, key, proc string
	}{
		{"the account naming the owner's agent", rv.agent.URL, "", ownerProc},
		{"the account naming another restricted account's agent", rv.agent.URL, "", peerProc},
		{"the owner naming the account's agent", rv.owner.URL, raKey, agentProc},
		{"the account's own session in a project it no longer reaches", rv.agent.URL, "", agentProc2},
	} {
		text, isErr := paCall(t, dial(row.base, row.key, row.proc), "tasks_add", map[string]any{"title": "unplaced"})
		require.True(t, isErr, "%s: %s", row.label, text)
		require.Contains(t, text, "ambiguous scope", row.label)
		s := byProc(row.proc)
		require.Equal(t, "expired", s.status, "%s: reopened nothing", row.label)
		require.Zero(t, revivals(s.id), row.label)
	}
	require.Zero(t, raCount(t, rv.db, `SELECT COUNT(*) FROM tasks`))

	// Yes: each principal's own agent revives its own session and is bound to it.
	for _, row := range []struct {
		label, base, key, proc, account string
	}{
		{"the account revives its own", rv.agent.URL, "", agentProc, raAccount},
		{"another restricted account revives its own", peerSock.URL, "", peerProc, raPeerAccount},
		{"the owner still revives its own", rv.owner.URL, raKey, ownerProc, ""},
	} {
		out := paCallOK(t, dial(row.base, row.key, row.proc), "tasks_add", map[string]any{"title": "back: " + row.label})
		s := byProc(row.proc)
		require.Equal(t, "active", s.status, row.label)
		require.Equal(t, raOpen, out["project"], row.label)
		require.Equal(t, s.name, out["created_by"], "%s: the call is credited to its session", row.label)
		if row.account == "" {
			require.NotContains(t, out, "account", row.label)
		} else {
			require.Equal(t, row.account, out["account"], row.label)
			require.True(t, strings.HasPrefix(s.name, row.account+":"), "%s: %s", row.label, s.name)
		}
		require.Equal(t, 1, revivals(s.id), "%s: the revival is on the record", row.label)
	}
	require.Equal(t, "expired", byProc(agentProc2).status, "the unreachable one stayed expired throughout")
}
