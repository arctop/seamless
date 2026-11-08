package main

// The lease principals' rows of the peer_auth matrix (plan step 10,
// peer_auth.locked): one daemon -- the real route table over real services,
// wired as serve wires them while peer_auth locks a project -- with
// example-finance (confidential) and example-legal (open) locked, open,
// confidential and sealed projects beside them, and a restricted local
// account. Every principal connects with the process chain the kernel would
// report for it (peerCred.chain), and the real peerHandler loads its leases
// from the store.
//
// Leased owner agents plant the locked projects' content through MCP, with a
// secret per project in every content field and in no probe; the owner
// plants the confidential and sealed projects. Then every tool in
// mcp.Catalog(), the hooks, A2A and the live feed run as each principal, both
// ways: no secret reaches a principal without a live lease on its project,
// and the leased agent's process tree -- the agent, a child, a grandchild --
// reads and writes it, with state checked after every write.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/store"
)

// lmWorld is the planted fixture: the item sets, the leased agents' sessions
// and leases, and one socket server per process that connects.
type lmWorld struct {
	fin, legal, conf, sealed, ghost lmItems
	sess                            map[string]core.Session      // by agent letter
	lease                           map[string]store.AccessLease // by agent letter
	srv                             map[string]*httptest.Server  // by process name
}

func (d *lmDaemon) setup(t *testing.T) *lmWorld {
	t.Helper()
	w := &lmWorld{sess: map[string]core.Session{}, lease: map[string]store.AccessLease{}, srv: map[string]*httptest.Server{}}
	chains := map[string][]access.ProcessToken{
		"hookA": {lmHookA, lmAgentA, lmTerm}, "bridgeA": {lmBridgeA, lmAgentA, lmTerm}, "agentA": {lmAgentA, lmTerm},
		"scriptA": {lmScriptA, lmShellA, lmAgentA, lmTerm},
		"hookP":   {lmHookP, lmAgentP, lmTerm}, "bridgeP": {lmBridgeP, lmAgentP, lmTerm},
		"hookB": {lmHookB, lmAgentB, lmTerm}, "bridgeB": {lmBridgeB, lmAgentB, lmTerm},
		"hookC": {lmHookC, lmAgentC, lmTerm}, "bridgeC": {lmBridgeC, lmAgentC, lmTerm},
		"hookD": {lmHookD, lmAgentD, lmTerm}, "bridgeD": {lmBridgeD, lmAgentD, lmTerm},
		"hookE": {lmHookE, lmAgentE, lmTerm}, "bridgeE": {lmBridgeE, lmAgentE, lmTerm},
		"hookU": {lmHookU, lmAgentU, lmTerm}, "bridgeU": {lmBridgeU, lmAgentU, lmTerm},
		"hookS": {lmHookS, lmAgentS, lmTerm}, "bridgeS": {lmBridgeS, lmAgentS, lmTerm},
		// The walk stops below pid 1 (access.walkChain), so an orphan's chain
		// is itself alone: no agent above it.
		"orphan": {lmOrphan},
	}
	for name, chain := range chains {
		w.srv[name] = d.peer(t, lmUIDOwner, chain...)
	}
	w.srv["bridgeR"] = d.peer(t, lmUIDAgent, lmBridgeR, lmAgentR, lmTerm)
	w.srv["hookR"] = d.peer(t, lmUIDAgent, lmHookR, lmAgentR, lmTerm)
	w.srv["stranger"] = d.peer(t, lmUIDStranger, lmTok(4000), lmTerm)

	// Agent A: SessionStart in the finance repository files the request, the
	// owner approves it for the session, and A's bridge plants finance.
	w.sess["A"], w.lease["A"] = d.startLeasedAgent(t, w.srv["hookA"].URL, lmAgentA, "claude-a", lmFin, "/work/finance", approval.ForSession)
	w.fin = d.plantItems(t, paClientHeaders(t, w.srv["bridgeA"].URL, "", lmAgentHeaders(lmAgentA)), lmFin, lmFinSecret, "fin", "/work/finance")
	w.fin.leaseSessID, w.fin.leaseSessName, w.fin.leaseID = w.sess["A"].ID, w.sess["A"].Name, w.lease["A"].ID
	// Agent P: the same in legal.
	w.sess["P"], w.lease["P"] = d.startLeasedAgent(t, w.srv["hookP"].URL, lmAgentP, "claude-p", lmLegal, "/work/legal", approval.ForSession)
	w.legal = d.plantItems(t, paClientHeaders(t, w.srv["bridgeP"].URL, "", lmAgentHeaders(lmAgentP)), lmLegal, lmLegalSecret, "legal", "/work/legal")
	w.legal.leaseSessID, w.legal.leaseSessName, w.legal.leaseID = w.sess["P"].ID, w.sess["P"].Name, w.lease["P"].ID
	// B (one hour), C, D, E (the session): finance leases that will end.
	w.sess["B"], w.lease["B"] = d.startLeasedAgent(t, w.srv["hookB"].URL, lmAgentB, "claude-b", lmFin, "/work/finance", approval.For1h)
	for _, x := range []struct {
		letter string
		agent  access.ProcessToken
	}{{"C", lmAgentC}, {"D", lmAgentD}, {"E", lmAgentE}} {
		w.sess[x.letter], w.lease[x.letter] = d.startLeasedAgent(t, w.srv["hook"+x.letter].URL, x.agent,
			"claude-"+strings.ToLower(x.letter), lmFin, "/work/finance", approval.ForSession)
	}
	// U and the sibling S: ordinary sessions in the open project's repository.
	for _, x := range []struct {
		hook  string
		agent access.ProcessToken
		id    string
	}{{"hookU", lmAgentU, "claude-u"}, {"hookS", lmAgentS, "claude-s"}} {
		out := d.hook(t, w.srv[x.hook].URL, "", "session-start", &x.agent, map[string]any{"session_id": x.id, "cwd": "/work/open", "source": "startup"})
		require.NotContains(t, out, "is locked")
	}
	// The restricted account's own session, in the open project.
	out := d.hook(t, w.srv["hookR"].URL, "", "session-start", &lmAgentR, map[string]any{"session_id": "claude-r", "cwd": "/work/open", "source": "startup"})
	require.NotContains(t, out, "maps to no project")

	// The owner, over TCP: the confidential and sealed projects, the open
	// project and a global memory.
	w.conf = d.plantItems(t, paClient(t, d.owner.URL, lmKey), lmConf, lmConfSecret, "conf", "/work/conf")
	w.sealed = d.plantItems(t, paClient(t, d.owner.URL, lmKey), lmSealed, lmSealedSecret, "sealed", "/work/sealed")
	lmCallOK(t, paClient(t, d.owner.URL, lmKey), "memory_write", map[string]any{"project": lmOpen, "name": "open-fact",
		"kind": "gotcha", "description": "open " + lmOpenToken + " " + lmSearch, "body": "open body " + lmOpenToken})
	lmCallOK(t, paClient(t, d.owner.URL, lmKey), "memory_write", map[string]any{"project": "global", "name": "house-style",
		"kind": "convention", "description": "global " + lmGlobalToken, "body": "global body " + lmGlobalToken})
	// A second session in each, as the locked projects have their agent's
	// lease session: the probes that name one have a counterpart everywhere.
	for _, it := range []*lmItems{&w.conf, &w.sealed} {
		s := lmCallOK(t, paClient(t, d.owner.URL, lmKey), "session_start", map[string]any{"project": it.project,
			"name": strings.TrimPrefix(it.project, "example-") + "-second-session"})
		it.leaseSessID, it.leaseSessName = s["session_id"].(string), s["name"].(string)
	}
	w.ghost = lmGhostItems(t, lmNone, "ghost")
	return w
}

// principal names one socket process as a matrix row.
func (w *lmWorld) principal(name, proc string, agent *access.ProcessToken, anchor string, leased ...string) lmPrincipal {
	p := lmPrincipal{name: name, base: w.srv[proc].URL, anchor: anchor, leased: leased}
	if agent != nil {
		p.headers = lmAgentHeaders(*agent)
	}
	return p
}

// lockedSecrets are the secrets of the locked projects p holds no lease on,
// plus the confidential and sealed projects' (no probe binds a session there,
// so no owner principal of the sweep may read them either); unleased are the
// locked item sets whose identifiers it may not see.
func (w *lmWorld) lockedSecrets(p lmPrincipal) (secrets []string, unleased []lmItems) {
	secrets = []string{lmConfSecret, lmSealedSecret}
	for _, it := range []lmItems{w.fin, w.legal} {
		if !slices.Contains(p.leased, it.project) {
			secrets = append(secrets, it.secret)
			unleased = append(unleased, it)
		}
	}
	return secrets, unleased
}

// pendingFor returns the pending lock requests anchored at key, by short id.
func (d *lmDaemon) pendingFor(t *testing.T, key string) map[string]store.AccessApproval {
	t.Helper()
	rows, err := store.PendingAccessApprovals(context.Background(), d.db, d.clock.now())
	require.NoError(t, err)
	out := map[string]store.AccessApproval{}
	for _, a := range rows {
		if a.ProcessKey == key && a.Kind != store.AccessKindGrant {
			out[approval.ShortID(a.ID)] = a
		}
	}
	return out
}

// clearPending denies every pending request anchored at key, so the next
// probe's refusal can only name a request it filed itself.
func (d *lmDaemon) clearPending(t *testing.T, key string) {
	t.Helper()
	for _, a := range d.pendingFor(t, key) {
		require.NoError(t, store.DenyAccess(context.Background(), d.db, a.ID, "matrix", d.clock.now()))
	}
}

// sweep drives every probe of every Catalog tool at each target as p, and
// returns which probes filed an access request ("tool#i@project"). After each
// probe: no secret of a locked project p holds no lease on, no identifier of
// the target it did not send, every locked project's planted content
// unchanged, no session started in the target, no lease created, and a
// request filed exactly when the refusal names one.
func (d *lmDaemon) sweep(t *testing.T, w *lmWorld, p lmPrincipal, targets ...lmItems) map[string]bool {
	t.Helper()
	cli := p.client(t)
	secrets, unleased := w.lockedSecrets(p)
	filed := map[string]bool{}
	for _, tool := range mcp.Catalog() {
		for _, target := range targets {
			for i, args := range lmToolProbes(target)[tool.Name] {
				what := fmt.Sprintf("%s: %s probe %d at %s (%v)", p.name, tool.Name, i, target.project, args)
				if p.anchor != "" {
					d.clearPending(t, p.anchor)
				}
				sessions := lmCount(t, d.db, `SELECT COUNT(*) FROM sessions WHERE project_slug = ?`, target.project)
				leases := lmCount(t, d.db, `SELECT COUNT(*) FROM access_leases`)
				approvals := lmCount(t, d.db, `SELECT COUNT(*) FROM access_approvals`)

				text, _ := paCall(t, cli, tool.Name, args)

				for _, s := range secrets {
					require.NotContains(t, text, s, "%s leaked locked content", what)
				}
				for _, it := range unleased {
					lmNoIdentifiers(t, it, text, mustJSON(t, args), what)
				}
				d.unchanged(t, target, what)
				require.Equal(t, sessions, lmCount(t, d.db, `SELECT COUNT(*) FROM sessions WHERE project_slug = ?`, target.project),
					"%s started a session in %s", what, target.project)
				require.Equal(t, leases, lmCount(t, d.db, `SELECT COUNT(*) FROM access_leases`), "%s created a lease", what)

				named := lmShortRe.FindAllString(text, -1)
				slices.Sort(named)
				named = slices.Compact(named)
				if p.anchor == "" {
					require.Equal(t, approvals, lmCount(t, d.db, `SELECT COUNT(*) FROM access_approvals`),
						"%s filed a request for a principal no approval may name", what)
					require.Empty(t, named, "%s named a request", what)
					continue
				}
				got := d.pendingFor(t, p.anchor)
				shorts := make([]string, 0, len(got))
				for short, a := range got {
					shorts = append(shorts, short)
					require.Equal(t, store.AccessKindRequest, a.Kind, "%s: a tree without a lease files a first request", what)
					require.Equal(t, uint32(lmUIDOwner), a.PeerUID, what)
				}
				sort.Strings(shorts)
				if len(named) == 0 {
					named = []string{}
				}
				require.Equal(t, named, shorts, "%s: a request is filed exactly when the refusal names it", what)
				filed[fmt.Sprintf("%s#%d@%s", tool.Name, i, target.project)] = len(shorts) > 0
			}
		}
	}
	// Both locked projects, once more: a probe at one never moved the other.
	d.unchanged(t, w.fin, p.name+": after the sweep")
	d.unchanged(t, w.legal, p.name+": after the sweep")
	return filed
}

// leasedReadsWrites drives a leased principal's reads (each must carry it's
// secret and never other's) and writes (each checked in the database or the
// files), naming its own items after who.
func (d *lmDaemon) leasedReadsWrites(t *testing.T, cli *mcpclient.Client, it, other lmItems, who string, anchored bool) {
	t.Helper()
	ctx := context.Background()
	x := it.project
	for _, r := range []struct {
		tool string
		args map[string]any
	}{
		{"memory_read", map[string]any{"project": x, "name": it.memName}},
		{"memory_read", map[string]any{"id": it.memID}},
		{"notes_read", map[string]any{"id": it.noteID}},
		{"notes_read", map[string]any{"slug": it.noteSlug, "project": x}},
		{"notes_read", map[string]any{"id": it.planNoteID}},
		{"tasks_list", map[string]any{"id": it.taskID}},
		{"tasks_list", map[string]any{"project": x, "plan": it.plan}},
		{"recall", map[string]any{"query": lmSearch, "project": x}},
		{"recall", map[string]any{"query": lmSearch}},
		{"recall", map[string]any{"query": lmSearch, "project": x, "scope": "sessions"}},
		{"recall", map[string]any{"kind": "gotcha", "project": x}},
		{"trial_query", map[string]any{"lab": it.lab}},
		{"lab_open", map[string]any{"lab": it.lab}},
		{"gardener_proposals", map[string]any{}},
	} {
		text, isErr := paCall(t, cli, r.tool, r.args)
		require.False(t, isErr, "%s: %s %v: %s", who, r.tool, r.args, text)
		require.Contains(t, text, it.secret, "%s: %s %v reads the leased project", who, r.tool, r.args)
		require.NotContains(t, text, other.secret, "%s: %s %v: a lease is per project", who, r.tool, r.args)
	}

	id := func(text string) string {
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(lmFirstJSON(text)), &out), text)
		v, _ := out["id"].(string)
		require.NotEmpty(t, v, text)
		return v
	}
	memFile := func(name string) string {
		m, ok, err := store.MemoryByName(ctx, d.db, x, name)
		require.NoError(t, err)
		require.True(t, ok, "%s: memory %s is in %s", who, name, x)
		require.Nil(t, m.InvalidAt)
		raw, err := os.ReadFile(filepath.Join(d.dir, m.FilePath))
		require.NoError(t, err)
		return string(raw)
	}
	noteFile := func(nid string) (core.Note, string) {
		n, ok, err := store.NoteByID(ctx, d.db, nid)
		require.NoError(t, err)
		require.True(t, ok, "%s: note %s exists", who, nid)
		require.Equal(t, x, n.Project)
		raw, err := os.ReadFile(filepath.Join(d.dir, n.FilePath))
		require.NoError(t, err)
		return n, string(raw)
	}
	call := func(tool string, args map[string]any) string {
		text, isErr := paCall(t, cli, tool, args)
		require.False(t, isErr, "%s: %s %v: %s", who, tool, args, text)
		require.NotContains(t, text, other.secret, "%s: %s: a lease is per project", who, tool)
		return text
	}

	mem := who + "-leased-fact"
	memID := id(call("memory_write", map[string]any{"project": x, "name": mem, "kind": "gotcha",
		"description": "leased " + who, "body": "leased body " + who}))
	require.Contains(t, memFile(mem), "leased body "+who)
	call("memory_append", map[string]any{"project": x, "name": mem, "body": "appended-" + who})
	require.Contains(t, memFile(mem), "appended-"+who)
	call("memory_edit", map[string]any{"project": x, "name": mem,
		"edits": []any{map[string]any{"old_string": "leased body", "new_string": "edited body"}}})
	require.Contains(t, memFile(mem), "edited body "+who)
	call("favorite_set", map[string]any{"kind": "memory", "id": mem, "project": x, "favorite": true})
	m, ok, err := store.MemoryByID(ctx, d.db, memID)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, m.Favorite, "%s: the star landed", who)

	nid := id(call("notes_create", map[string]any{"project": x, "title": who + " leased note", "body": "note body " + who}))
	_, raw := noteFile(nid)
	require.Contains(t, raw, "note body "+who)
	call("notes_append", map[string]any{"id": nid, "body": "note-append-" + who})
	_, raw = noteFile(nid)
	require.Contains(t, raw, "note-append-"+who)
	call("notes_edit", map[string]any{"id": nid, "edits": []any{map[string]any{"old_string": "note body", "new_string": "NOTE BODY"}}})
	_, raw = noteFile(nid)
	require.Contains(t, raw, "NOTE BODY "+who)
	call("notes_update", map[string]any{"id": nid, "description": "updated-" + who})
	n, _ := noteFile(nid)
	require.Equal(t, "updated-"+who, n.Description)

	tid := id(call("tasks_add", map[string]any{"project": x, "title": who + " leased task"}))
	task, err := store.TaskByID(ctx, d.db, tid)
	require.NoError(t, err)
	require.Equal(t, x, task.ProjectSlug)
	call("tasks_update", map[string]any{"id": tid, "title": who + " retitled"})
	task, err = store.TaskByID(ctx, d.db, tid)
	require.NoError(t, err)
	require.Equal(t, who+" retitled", task.Title)
	call("tasks_claim", map[string]any{"id": tid})
	task, err = store.TaskByID(ctx, d.db, tid)
	require.NoError(t, err)
	require.Equal(t, core.TaskInProgress, task.Status)
	call("tasks_release", map[string]any{"id": tid})
	task, err = store.TaskByID(ctx, d.db, tid)
	require.NoError(t, err)
	require.Equal(t, core.TaskOpen, task.Status)
	call("tasks_update", map[string]any{"id": tid, "status": "done"})
	task, err = store.TaskByID(ctx, d.db, tid)
	require.NoError(t, err)
	require.Equal(t, core.TaskDone, task.Status)

	trid := id(call("trial_record", map[string]any{"lab": it.lab, "project": x, "title": who + " leased trial",
		"changes": "c " + who, "actual": "a " + who}))
	tr, ok, err := store.TrialByID(ctx, d.db, trid)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, x, tr.ProjectSlug)
	require.Equal(t, it.lab, tr.Lab)

	findings := who + "-leased-findings"
	call("session_update", map[string]any{"findings": findings})
	require.Equal(t, 1, lmCount(t, d.db, `SELECT COUNT(*) FROM sessions WHERE project_slug = ? AND findings LIKE ?`, x, "%"+findings+"%"),
		"%s: the findings landed in a session of %s", who, x)

	// A tree that holds the lease asking again, more than the expiry warning
	// from its end: the lease answers and nothing is filed -- whether or not
	// the process names an agent, since no request (and so no anchor) is
	// involved. From the warning on it files a refresh (approval's own tests
	// and TestLeaseExpiry_WarningsRefreshAndEnd), never a lease.
	_ = anchored
	leases := lmCount(t, d.db, `SELECT COUNT(*) FROM access_leases`)
	approvals := lmCount(t, d.db, `SELECT COUNT(*) FROM access_approvals`)
	var req map[string]any
	require.NoError(t, json.Unmarshal([]byte(lmFirstJSON(call("access_request", map[string]any{"project": x}))), &req))
	require.Equal(t, "approved", req["status"], "%s: %v", who, req)
	require.NotEmpty(t, req["open_until"], "%s: %v", who, req)
	require.Contains(t, req["next"], "is already open to this agent until ", "%s: %v", who, req)
	require.Equal(t, approvals, lmCount(t, d.db, `SELECT COUNT(*) FROM access_approvals`), "%s: nothing is filed", who)
	require.Equal(t, leases, lmCount(t, d.db, `SELECT COUNT(*) FROM access_leases`))

	call("memory_delete", map[string]any{"project": x, "name": mem})
	_, found, err := store.MemoryByName(ctx, d.db, x, mem)
	require.NoError(t, err)
	require.False(t, found, "%s: the delete landed", who)
	call("notes_delete", map[string]any{"id": nid})
	_, found, err = store.NoteByID(ctx, d.db, nid)
	require.NoError(t, err)
	require.False(t, found, "%s: the note delete landed", who)
}

// lmFirstJSON is a tool result's first text item: the lease notices ride as
// later items, which paCall concatenates.
func lmFirstJSON(text string) string {
	if i := strings.Index(text, "}Access to "); i >= 0 {
		return text[:i+1]
	}
	return text
}

// lmHookEvents are the hook events Handler serves (internal/hooks/handler.go
// Register), the session end last.
var lmHookEvents = []string{"session-start", "user-prompt-submit", "stop", "post-tool-use", "subagent-start",
	"subagent-stop", "permission-request", "session-taint", "session-end"}

// lmHookBody is one hook event's body naming the agent session id.
func lmHookBody(id, cwd string) map[string]any {
	return map[string]any{"session_id": id, "cwd": cwd, "source": "resume", "prompt": "ledger reconcile memo " + lmSearch,
		"reason": "other", "agent_id": "sub-1", "agent_type": "general", "tool_name": "ExitPlanMode",
		"tool_input": map[string]any{"plan": "# Plan\n\nreconcile " + lmSearch}, "hook_event_name": "Hook"}
}

func TestPeerAuthMatrix_Leases(t *testing.T) {
	ctx := context.Background()
	d := newLMDaemon(t)
	w := d.setup(t)
	own := paClient(t, d.owner.URL, lmKey)

	for _, tool := range mcp.Catalog() {
		require.Contains(t, lmToolProbes(w.fin), tool.Name, "mcp.Catalog() tool %s has no lease-matrix probe row: add one", tool.Name)
	}
	require.Contains(t, lmToolProbes(w.fin), mcp.AccessRequestTool, "the locked-only tool is probed")

	// The leased tree: the agent itself, its bridge, a script under a shell
	// under it -- every one reads and writes finance; P's bridge, legal.
	t.Run("leased tree reads and writes its project", func(t *testing.T) {
		for _, c := range []struct {
			who, proc string
			agent     access.ProcessToken
			it, other lmItems
		}{
			{"anchor", "agentA", lmAgentA, w.fin, w.legal},
			{"child", "bridgeA", lmAgentA, w.fin, w.legal},
			{"grandchild", "scriptA", lmAgentA, w.fin, w.legal},
			{"legal-child", "bridgeP", lmAgentP, w.legal, w.fin},
		} {
			t.Run(c.who, func(t *testing.T) {
				cli := paClientHeaders(t, w.srv[c.proc].URL, "", lmAgentHeaders(c.agent))
				d.leasedReadsWrites(t, cli, c.it, c.other, c.who, true)
			})
		}
		// The lease follows the kernel's chain, not the header: a script that
		// names no agent still holds it. Isolation is unchanged -- a
		// confidential project's reads need a session bound to it -- so it
		// binds first, which the lease allows.
		t.Run("grandchild naming no agent", func(t *testing.T) {
			cli := paClient(t, w.srv["scriptA"].URL, "")
			text, isErr := paCall(t, cli, "memory_read", map[string]any{"id": w.fin.memID})
			require.True(t, isErr, text)
			require.Contains(t, text, "confidential: reads require a session bound to it", "the isolation fence, not the lock")
			require.NotContains(t, text, lmFinSecret)
			sess := lmCallOK(t, cli, "session_start", map[string]any{"project": lmFin, "name": "gc-noheader-session"})
			require.Equal(t, lmFin, sess["project"], "a leased tree may place a session in the locked project")
			d.leasedReadsWrites(t, cli, w.fin, w.legal, "gc-noheader", false)
		})
		// The other leases, before they end: each tree reads finance.
		for _, x := range []struct {
			letter string
			agent  access.ProcessToken
		}{{"B", lmAgentB}, {"C", lmAgentC}, {"D", lmAgentD}, {"E", lmAgentE}} {
			cli := paClientHeaders(t, w.srv["bridge"+x.letter].URL, "", lmAgentHeaders(x.agent))
			text, isErr := paCall(t, cli, "memory_read", map[string]any{"id": w.fin.memID})
			require.False(t, isErr, "%s: %s", x.letter, text)
			require.Contains(t, text, lmFinSecret, "agent %s's tree reads finance under its lease", x.letter)
		}
	})

	// The leases that end before their expiry: C's session ends over MCP,
	// the owner revokes D's on the console, E's own SessionEnd hook ends it.
	t.Run("revocations", func(t *testing.T) {
		cliC := paClientHeaders(t, w.srv["bridgeC"].URL, "", lmAgentHeaders(lmAgentC))
		lmCallOK(t, cliC, "session_end", map[string]any{"findings": "c done"})
		lmRequireRevoked(t, d, w.lease["C"].ID, store.LeaseRevokedSessionEnded)
		sess, ok, err := store.SessionByID(ctx, d.db, w.sess["C"].ID)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, core.SessionCompleted, sess.Status)

		// The agent's own socket may not revoke (an owner decision), and the
		// owner's TCP key may.
		revoke := func(base, key string) int {
			req, err := http.NewRequest(http.MethodPost, base+"/console/access/leases/"+w.lease["D"].ID+"/revoke", strings.NewReader(""))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Accept", "application/json")
			if key != "" {
				req.Header.Set("Authorization", "Bearer "+key)
			}
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			return resp.StatusCode
		}
		require.Equal(t, http.StatusForbidden, revoke(w.srv["bridgeD"].URL, ""))
		require.Nil(t, lmMustLease(t, d, w.lease["D"].ID).RevokedAt, "the agent's socket did not revoke")
		require.Equal(t, http.StatusOK, revoke(d.owner.URL, lmKey))
		lmRequireRevoked(t, d, w.lease["D"].ID, store.LeaseRevokedOwner)

		d.hook(t, w.srv["hookE"].URL, "", "session-end", &lmAgentE, map[string]any{"session_id": "claude-e", "reason": "exit"})
		lmRequireRevoked(t, d, w.lease["E"].ID, store.LeaseRevokedSessionEnded)

		for _, x := range []struct {
			letter string
			agent  access.ProcessToken
		}{{"C", lmAgentC}, {"D", lmAgentD}, {"E", lmAgentE}} {
			cli := paClientHeaders(t, w.srv["bridge"+x.letter].URL, "", lmAgentHeaders(x.agent))
			text, isErr := paCall(t, cli, "memory_read", map[string]any{"id": w.fin.memID})
			require.True(t, isErr, "%s: %s", x.letter, text)
			require.NotContains(t, text, lmFinSecret)
			require.Contains(t, text, "project "+lmFin+" is locked", "%s: locked again", x.letter)
		}
	})

	t.Run("live feed", func(t *testing.T) { d.liveFeedRows(t, w, own) })
	t.Run("a2a", func(t *testing.T) { d.a2aRows(t, w) })
	t.Run("hooks", func(t *testing.T) { d.hookRows(t, w) })

	// Every tool, every probe, at every locked project each principal holds
	// no live lease on.
	// lockedHome marks a principal whose own live session lies in a locked
	// project it holds no lease on (D: the owner revoked the lease, the
	// session goes on): a call that resolves its scope through that session
	// is refused at the lock and files, where for the others it only filters.
	deniedRows := []struct {
		p          lmPrincipal
		targets    []lmItems
		lockedHome bool
	}{
		{lmPrincipal{name: "owner over TCP", base: d.owner.URL, key: lmKey}, []lmItems{w.fin, w.legal}, false},
		{lmPrincipal{name: "owner over TCP naming A's agent process", base: d.owner.URL, key: lmKey, headers: lmAgentHeaders(lmAgentA)}, []lmItems{w.fin, w.legal}, false},
		{w.principal("owner's agent U on the socket, no approval", "bridgeU", &lmAgentU, lmAgentU.Key()), []lmItems{w.fin, w.legal}, false},
		{w.principal("same-uid sibling naming A's agent process", "bridgeS", &lmAgentA, ""), []lmItems{w.fin, w.legal}, false},
		{w.principal("orphaned descendant of A (reparented)", "orphan", &lmAgentA, ""), []lmItems{w.fin, w.legal}, false},
		{w.principal("A's bridge at legal (leased on finance only)", "bridgeA", &lmAgentA, lmAgentA.Key(), lmFin), []lmItems{w.legal}, false},
		{w.principal("P's bridge at finance (leased on legal only)", "bridgeP", &lmAgentP, lmAgentP.Key(), lmLegal), []lmItems{w.fin}, false},
		{w.principal("C, revoked by session_end", "bridgeC", &lmAgentC, lmAgentC.Key()), []lmItems{w.fin, w.legal}, false},
		{w.principal("D, revoked by the owner", "bridgeD", &lmAgentD, lmAgentD.Key()), []lmItems{w.fin, w.legal}, true},
		{w.principal("E, revoked by its SessionEnd hook", "bridgeE", &lmAgentE, lmAgentE.Key()), []lmItems{w.fin, w.legal}, false},
	}
	for _, row := range deniedRows {
		t.Run("mcp: "+row.p.name, func(t *testing.T) {
			filed := d.sweep(t, w, row.p, row.targets...)
			if row.p.anchor == "" {
				return
			}
			var quiet []string
			for probe, ok := range filed {
				if !ok {
					quiet = append(quiet, probe)
				}
			}
			sort.Strings(quiet)
			t.Logf("%d of %d probes filed a request; these filed none: %s", len(filed)-len(quiet), len(filed), strings.Join(quiet, " "))
			// A refusal at the lock files a request; filtering is not asking --
			// recall answers from the rest of its scope even around a locked
			// session, and names the project without filing.
			unscoped := []string{"gardener_proposals#0", "tasks_list#3", "tasks_ready#2", "trial_query#2"}
			for _, target := range row.targets {
				for _, probe := range []string{"memory_read#0", "tasks_list#0", "notes_read#1", "recall#1", "access_request#0"} {
					require.True(t, filed[probe+"@"+target.project], "%s at %s files a request", probe, target.project)
				}
				for _, probe := range []string{"project_list#0", "usage_summary#0", "recall#0", "recall#3"} {
					require.False(t, filed[probe+"@"+target.project], "%s at %s only filters: it files nothing", probe, target.project)
				}
				for _, probe := range unscoped {
					require.Equal(t, row.lockedHome, filed[probe+"@"+target.project],
						"%s at %s: refused at the lock exactly when the caller's own session is locked", probe, target.project)
				}
			}
		})
	}

	t.Run("restricted account: every tool at every closed project", func(t *testing.T) { d.restrictedSweep(t, w) })
	// After the sweep, before any lease expires: the account's hooks start
	// sessions of its own under the agent ids the owner's leased sessions
	// hold, whose handles are the owner's, qualified (SEAL A2). The sweep
	// names the owner's handles and compares them with a ghost's the account
	// never used, so it must run while the account holds none of them.
	t.Run("restricted account's hooks and a2a", func(t *testing.T) { d.restrictedHooks(t, w) })

	// The expired lease, last: the daemon's clock moves past B's hour. A's
	// lease (the session, capped at eight hours) carries on.
	t.Run("expired lease", func(t *testing.T) {
		d.clock.advance(61 * time.Minute)
		require.Nil(t, lmMustLease(t, d, w.lease["B"].ID).RevokedAt, "expired, never revoked")
		live, err := store.ActiveAccessLeases(ctx, d.db, lmAgentB.Key(), d.clock.now())
		require.NoError(t, err)
		require.Empty(t, live)
		cliB := paClientHeaders(t, w.srv["bridgeB"].URL, "", lmAgentHeaders(lmAgentB))
		text, isErr := paCall(t, cliB, "memory_read", map[string]any{"id": w.fin.memID})
		require.True(t, isErr, text)
		require.Contains(t, text, "project "+lmFin+" is locked (access ended at ", "the refusal tells a lapsed approval from none")
		require.NotContains(t, text, lmFinSecret)
		out := d.hook(t, w.srv["hookB"].URL, "", "session-start", &lmAgentB, map[string]any{"session_id": "claude-b", "cwd": "/work/finance", "source": "resume"})
		require.NotContains(t, out, lmFinSecret)
		require.Contains(t, out, "is locked")
		cliA := paClientHeaders(t, w.srv["bridgeA"].URL, "", lmAgentHeaders(lmAgentA))
		text, isErr = paCall(t, cliA, "memory_read", map[string]any{"id": w.fin.memID})
		require.False(t, isErr, text)
		require.Contains(t, text, lmFinSecret, "A's lease outlives B's")
		d.sweep(t, w, w.principal("B, past its lease's end", "bridgeB", &lmAgentB, lmAgentB.Key()), w.fin, w.legal)
	})

	t.Run("another uid is refused at the door", func(t *testing.T) {
		for _, path := range []string{"/api/mcp", "/api/hooks/session-start", "/console/", "/api/a2a", "/healthz", "/console/events"} {
			resp, err := http.Post(w.srv["stranger"].URL+path, "application/json", strings.NewReader(`{}`))
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusForbidden, resp.StatusCode, path)
		}
	})

	// Every lease is one the owner approved; nothing else created one.
	t.Run("leases are the approvals", func(t *testing.T) {
		rows, err := d.db.QueryContext(ctx, `SELECT l.process_key, a.status, a.decided_by FROM access_leases l
			JOIN access_approvals a ON a.id = l.approval_id`)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		keys := map[string]bool{}
		for rows.Next() {
			var key, status, by string
			require.NoError(t, rows.Scan(&key, &status, &by))
			require.Equal(t, store.ApprovalApproved, status)
			require.Equal(t, "daemon-account", by)
			keys[key] = true
		}
		require.NoError(t, rows.Err())
		for _, proc := range []access.ProcessToken{lmAgentU, lmAgentS, lmOrphan, lmAgentR, lmBridgeA, lmScriptA, lmHookA, lmTerm} {
			require.False(t, keys[proc.Key()], "no lease names %s", proc)
		}
		require.Equal(t, lmCount(t, d.db, `SELECT COUNT(*) FROM access_leases`),
			lmCount(t, d.db, `SELECT COUNT(*) FROM access_approvals WHERE status = 'approved'`))
		require.Zero(t, lmCount(t, d.db, `SELECT COUNT(*) FROM access_approvals WHERE peer_uid = ?`, lmUIDAgent),
			"the restricted account filed nothing")
		require.Zero(t, lmCount(t, d.db, `SELECT COUNT(*) FROM access_grants`))
	})

	// The event log files each locked project's content under that project.
	t.Run("events stay in their project", func(t *testing.T) {
		for project, secret := range map[string]string{lmFin: lmFinSecret, lmLegal: lmLegalSecret} {
			require.Zero(t, lmCount(t, d.db, `SELECT COUNT(*) FROM events WHERE payload LIKE ? AND project_slug <> ?`,
				"%"+secret+"%", project), "an event carries %s content outside it", project)
			require.Positive(t, lmCount(t, d.db, `SELECT COUNT(*) FROM events WHERE payload LIKE ?`, "%"+secret+"%"),
				"the leased agent's calls left events carrying %s content", project)
		}
	})
}

func lmRequireRevoked(t *testing.T, d *lmDaemon, id, reason string) {
	t.Helper()
	l := lmMustLease(t, d, id)
	require.NotNil(t, l.RevokedAt, "lease %s is revoked", id)
	require.Equal(t, reason, l.RevokedReason)
}

func lmMustLease(t *testing.T, d *lmDaemon, id string) store.AccessLease {
	t.Helper()
	l, err := store.AccessLeaseByID(context.Background(), d.db, id)
	require.NoError(t, err)
	return l
}

// liveFeedRows subscribes to the console's live feed as the owner (TCP, both
// feeds), as unleased owner trees on the socket, and as A's leased tree; then
// the leased agents act in both locked projects and the owner writes a
// marker. Every frame up to the marker is free of locked content for the
// unleased views; A's view carries its own project's event and not legal's.
func (d *lmDaemon) liveFeedRows(t *testing.T, w *lmWorld, own *mcpclient.Client) {
	feeds := []struct {
		name, base, key, feed string
		leased                []string
	}{
		{"owner over TCP", d.owner.URL, lmKey, "", nil},
		{"owner over TCP, interactions", d.owner.URL, lmKey, "?feed=interactions", nil},
		{"U on the socket", w.srv["bridgeU"].URL, "", "", nil},
		{"orphan on the socket", w.srv["orphan"].URL, "", "", nil},
		{"A's leased bridge", w.srv["bridgeA"].URL, "", "", []string{lmFin}},
	}
	frames := make([]<-chan string, len(feeds))
	for i, f := range feeds {
		frames[i] = lmSubscribe(t, f.base, f.key, f.feed)
	}
	cliA := paClientHeaders(t, w.srv["bridgeA"].URL, "", lmAgentHeaders(lmAgentA))
	cliP := paClientHeaders(t, w.srv["bridgeP"].URL, "", lmAgentHeaders(lmAgentP))
	lmCallOK(t, cliA, "memory_write", map[string]any{"name": "live-fin-fact", "kind": "gotcha", "description": "live " + lmFinSecret, "body": "live " + lmFinSecret})
	lmCallOK(t, cliP, "memory_write", map[string]any{"project": lmLegal, "name": "live-legal-fact", "kind": "gotcha", "description": "live " + lmLegalSecret, "body": "live " + lmLegalSecret})
	_, _ = paCall(t, cliA, "recall", map[string]any{"query": lmFinSecret + " live"})
	_, _ = paCall(t, cliP, "recall", map[string]any{"query": lmLegalSecret + " live", "project": lmLegal})
	_ = lmA2ASend(t, w.srv["bridgeP"].URL, "", lmLegalSecret+" ledger", map[string]any{"project": lmLegal})
	marker := "owner-live-marker"
	lmCallOK(t, own, "memory_write", map[string]any{"project": lmOpen, "name": marker, "kind": "gotcha", "description": marker, "body": marker})
	for i, f := range feeds {
		seen := lmFramesUntil(t, frames[i], marker, func(frame string) {
			if !slices.Contains(f.leased, lmFin) {
				require.NotContains(t, frame, lmFinSecret, "%s: the live feed carried finance content", f.name)
				require.NotContains(t, frame, "live-fin-fact", "%s: the live feed named a finance memory", f.name)
				lmNoIdentifiers(t, w.fin, frame, "", f.name+" frame")
			}
			require.NotContains(t, frame, lmLegalSecret, "%s: the live feed carried legal content", f.name)
			require.NotContains(t, frame, "live-legal-fact", "%s: the live feed named a legal memory", f.name)
			lmNoIdentifiers(t, w.legal, frame, "", f.name+" frame")
		})
		if slices.Contains(f.leased, lmFin) {
			require.True(t, slices.ContainsFunc(seen, func(s string) bool { return strings.Contains(s, "live-fin-fact") }),
				"%s: the leased tree's feed shows its own project's event", f.name)
		}
	}
}

// a2aRows: A2A recall, as each principal, at each locked project.
func (d *lmDaemon) a2aRows(t *testing.T, w *lmWorld) {
	denied := []struct {
		name, base, key string
		secrets         []string
	}{
		{"owner over TCP", d.owner.URL, lmKey, []string{lmFinSecret, lmLegalSecret}},
		{"U", w.srv["bridgeU"].URL, "", []string{lmFinSecret, lmLegalSecret}},
		{"sibling", w.srv["bridgeS"].URL, "", []string{lmFinSecret, lmLegalSecret}},
		{"orphan", w.srv["orphan"].URL, "", []string{lmFinSecret, lmLegalSecret}},
		{"C revoked", w.srv["bridgeC"].URL, "", []string{lmFinSecret, lmLegalSecret}},
		{"A at legal", w.srv["bridgeA"].URL, "", []string{lmLegalSecret}},
		{"P at finance", w.srv["bridgeP"].URL, "", []string{lmFinSecret}},
	}
	for _, who := range denied {
		for _, meta := range []map[string]any{{"project": lmFin}, {"project": lmLegal}, nil, {"scope": "all"},
			{"project": lmFin, "scope": "sessions"}, {"project": lmLegal, "scope": "tasks"}} {
			for _, q := range []string{lmSearch, "ledger reconcile memo"} {
				out := lmA2ASend(t, who.base, who.key, q, meta)
				for _, s := range who.secrets {
					require.NotContains(t, out, s, "a2a as %s (%v): leaked locked content", who.name, meta)
				}
			}
		}
	}
	// The leased trees recall their own project over A2A.
	out := lmA2ASend(t, w.srv["bridgeP"].URL, "", lmSearch, map[string]any{"project": lmLegal})
	require.Contains(t, out, lmLegalSecret, "P's tree recalls legal over A2A")
	require.NotContains(t, out, lmFinSecret)
	out = lmA2ASend(t, w.srv["bridgeA"].URL, "", lmSearch, map[string]any{"project": lmFin})
	require.Contains(t, out, lmFinSecret, "A's tree recalls finance over A2A")
	require.NotContains(t, out, lmLegalSecret)
	// The owner's open project still answers over TCP: the rows above are
	// not an empty surface.
	require.Contains(t, lmA2ASend(t, d.owner.URL, lmKey, lmSearch, map[string]any{"project": lmOpen}), lmOpenToken)
}

// lmHooker is one principal posting hook events.
type lmHooker struct {
	name, base, key string
	agent           *access.ProcessToken
}

// hookRows: every hook event, as the denied principals, in the locked
// projects' repositories and naming the leased sessions; the leased hook
// processes get their project's briefing.
func (d *lmDaemon) hookRows(t *testing.T, w *lmWorld) {
	ctx := context.Background()
	notes := func(project string) int {
		return lmCount(t, d.db, `SELECT COUNT(*) FROM notes_index WHERE project = ?`, project)
	}
	finNotes, legalNotes := notes(lmFin), notes(lmLegal)

	t.Run("leased hooks brief their own project", func(t *testing.T) {
		out := d.hook(t, w.srv["hookA"].URL, "", "session-start", &lmAgentA, map[string]any{"session_id": "claude-a", "cwd": "/work/finance", "source": "resume"})
		require.Contains(t, out, lmFinSecret, "A's hook process carries the lease: the finance briefing")
		require.NotContains(t, out, lmLegalSecret)
		require.NotContains(t, out, "is locked")
		out = d.hook(t, w.srv["hookP"].URL, "", "session-start", &lmAgentP, map[string]any{"session_id": "claude-p", "cwd": "/work/legal", "source": "resume"})
		require.Contains(t, out, lmLegalSecret)
		require.NotContains(t, out, lmFinSecret)
		out = d.hook(t, w.srv["hookA"].URL, "", "user-prompt-submit", &lmAgentA, lmHookBody("claude-a", "/work/finance"))
		require.NotContains(t, out, lmLegalSecret)
	})

	denied := []lmHooker{
		{"owner over TCP", d.owner.URL, lmKey, nil},
		{"U's hook", w.srv["hookU"].URL, "", &lmAgentU},
		{"the sibling's hook", w.srv["hookS"].URL, "", &lmAgentS},
		{"the orphan", w.srv["orphan"].URL, "", &lmAgentA},
		{"C's hook (revoked)", w.srv["hookC"].URL, "", &lmAgentC},
		{"D's hook (revoked)", w.srv["hookD"].URL, "", &lmAgentD},
	}
	for _, h := range denied {
		t.Run("in the locked repositories: "+h.name, func(t *testing.T) {
			for _, repo := range []string{"/work/finance", "/work/legal"} {
				id := "claude-" + strings.ReplaceAll(h.name, " ", "-") + strings.ReplaceAll(repo, "/", "-")
				for _, ev := range lmHookEvents {
					body := lmHookBody(id, repo)
					if ev == "session-start" {
						body["source"] = "startup"
					}
					status, out := d.hookStatus(t, h.base, h.key, ev, h.agent, body)
					require.Less(t, status, 500, "%s %s: %s", h.name, ev, out)
					require.NotContains(t, out, lmFinSecret, "%s %s in %s", h.name, ev, repo)
					require.NotContains(t, out, lmLegalSecret, "%s %s in %s", h.name, ev, repo)
					lmNoIdentifiers(t, w.fin, out, "", h.name+" "+ev)
					lmNoIdentifiers(t, w.legal, out, "", h.name+" "+ev)
					if ev == "session-start" {
						require.Contains(t, out, "is locked", "%s %s in %s: the locked notice", h.name, ev, repo)
					}
				}
			}
			require.Equal(t, finNotes, notes(lmFin), "no capture landed in finance")
			require.Equal(t, legalNotes, notes(lmLegal), "no capture landed in legal")
			d.unchanged(t, w.fin, h.name+" hooks")
			d.unchanged(t, w.legal, h.name+" hooks")
		})
	}

	// A hook from outside a leased tree naming that tree's agent session: no
	// content, and the session and its lease are the tree's alone -- the MCP
	// session_end refuses the same at the lock.
	for n, h := range []lmHooker{
		{"U's hook", w.srv["hookU"].URL, "", &lmAgentU},
		{"owner over TCP", d.owner.URL, lmKey, nil},
		{"the sibling's hook", w.srv["hookS"].URL, "", &lmAgentS},
		{"the orphan naming the victim's process", w.srv["orphan"].URL, "", nil},
	} {
		t.Run("naming another tree's leased session: "+h.name, func(t *testing.T) {
			victim, victimHook := lmVictim(n)
			claudeID := fmt.Sprintf("claude-v%d", n)
			hookSrv := d.peer(t, lmUIDOwner, victimHook, victim, lmTerm)
			sess, lease := d.startLeasedAgent(t, hookSrv.URL, victim, claudeID, lmFin, "/work/finance", approval.ForSession)
			agent := h.agent
			if agent == nil && h.key == "" {
				agent = &victim // the orphan claims the victim's identity
			}
			for _, ev := range lmHookEvents {
				status, out := d.hookStatus(t, h.base, h.key, ev, agent, lmHookBody(claudeID, "/work/finance"))
				require.Less(t, status, 500, "%s %s: %s", h.name, ev, out)
				require.NotContains(t, out, lmFinSecret, "%s %s naming %s", h.name, ev, claudeID)
				require.NotContains(t, out, lmLegalSecret)
				after, ok, err := store.SessionByID(ctx, d.db, sess.ID)
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, core.SessionActive, after.Status, "%s %s ended another tree's leased session", h.name, ev)
				require.Equal(t, lmFin, after.ProjectSlug)
				require.Nil(t, lmMustLease(t, d, lease.ID).RevokedAt, "%s %s revoked another tree's lease", h.name, ev)
			}
		})
	}
}

// restrictedSweep drives every Catalog tool's probes at every project the
// restricted account may not reach -- confidential, sealed, locked (open),
// locked (confidential) -- and at a slug nobody registered. No secret appears
// anywhere, a refusal reads the same for every one of them (only the
// identifiers the probe sent differ), and the account files nothing.
func (d *lmDaemon) restrictedSweep(t *testing.T, w *lmWorld) {
	cli := paClientHeaders(t, w.srv["bridgeR"].URL, "", lmAgentHeaders(lmAgentR))
	sets := []lmItems{w.fin, w.legal, w.conf, w.sealed, w.ghost}
	secrets := []string{lmFinSecret, lmLegalSecret, lmConfSecret, lmSealedSecret}
	counts := func() [3]int {
		return [3]int{lmCount(t, d.db, `SELECT COUNT(*) FROM access_approvals`), lmCount(t, d.db, `SELECT COUNT(*) FROM access_leases`),
			lmCount(t, d.db, `SELECT COUNT(*) FROM access_grants`)}
	}
	before := counts()
	closedSessions := func() int {
		return lmCount(t, d.db, `SELECT COUNT(*) FROM sessions WHERE project_slug IN (?, ?, ?, ?)`, lmFin, lmLegal, lmConf, lmSealed)
	}
	sessions := closedSessions()
	var oracles []string
	n := 0
	for _, tool := range mcp.Catalog() {
		for i := range lmToolProbes(w.fin)[tool.Name] {
			n++
			ghost := lmGhostItems(t, lmNone, fmt.Sprintf("ghost%d", n))
			// Session names keep one history across the sweep, as a closed
			// project's do: a probe that starts the account's own session
			// under a name (session_start {project: open, name}) leaves it,
			// inside the account's namespace, for every later probe naming it
			// -- at a closed project and at the ghost alike.
			ghost.sessName, ghost.leaseSessName = w.ghost.sessName, w.ghost.leaseSessName
			sets[len(sets)-1] = ghost
			probes := make([][]map[string]any, len(sets))
			for j, it := range sets {
				probes[j] = lmToolProbes(it)[tool.Name]
			}
			var texts []string
			var errs []bool
			for j, it := range sets {
				args := probes[j][i]
				text, isErr := paCall(t, cli, tool.Name, args)
				what := fmt.Sprintf("restricted: %s probe %d at %s (%v)", tool.Name, i, it.project, args)
				for _, s := range secrets {
					require.NotContains(t, text, s, "%s leaked", what)
				}
				for _, other := range sets {
					lmNoIdentifiers(t, other, text, mustJSON(t, args), what)
				}
				texts = append(texts, lmNormalize(text, it))
				errs = append(errs, isErr)
			}
			// The unregistered slug is the reference: every closed project
			// must answer exactly as it does.
			ref := len(sets) - 1
			for j := range sets[:ref] {
				switch {
				case errs[j] != errs[ref]:
					oracles = append(oracles, fmt.Sprintf("%s probe %d: %s answers error=%v, %s error=%v:\n  %s\n  %s",
						tool.Name, i, sets[j].project, errs[j], sets[ref].project, errs[ref], texts[j], texts[ref]))
				case errs[j] && texts[j] != texts[ref]:
					oracles = append(oracles, fmt.Sprintf("%s probe %d: the refusal at %s tells it from %s:\n  %s\n  %s",
						tool.Name, i, sets[j].project, sets[ref].project, texts[j], texts[ref]))
				}
			}
		}
	}
	require.Empty(t, oracles, "a restricted account's answer tells a closed project from a missing one")
	require.Equal(t, before, counts(), "the restricted account files no request, lease or grant")
	require.Zero(t, lmCount(t, d.db, `SELECT COUNT(*) FROM access_approvals WHERE peer_uid = ?`, lmUIDAgent))
	require.Equal(t, sessions, closedSessions(), "no session in a closed project")
	require.Zero(t, lmCount(t, d.db, `SELECT COUNT(*) FROM projects WHERE slug = ?`, lmNone), "no project minted")
	for _, it := range []lmItems{w.fin, w.legal} {
		d.unchanged(t, it, "the restricted sweep")
	}
	// Both ways: the account reads the open project and global.
	text, isErr := paCall(t, cli, "memory_read", map[string]any{"project": lmOpen, "name": "open-fact"})
	require.False(t, isErr, text)
	require.Contains(t, text, lmOpenToken)
	text, isErr = paCall(t, cli, "memory_read", map[string]any{"project": "global", "name": "house-style"})
	require.False(t, isErr, text)
	require.Contains(t, text, lmGlobalToken)
}

// lmNormalize replaces it's identifiers in text with placeholders, longest
// first, so a refusal at one project compares with the same refusal at
// another.
func lmNormalize(text string, it lmItems) string {
	pairs := [][2]string{{it.project, "<project>"}, {it.repo, "<repo>"}, {it.memID, "<mem-id>"}, {it.memName, "<mem>"},
		{it.noteID, "<note-id>"}, {it.noteSlug, "<note>"}, {it.planNoteID, "<plan-note-id>"}, {it.taskID, "<task-id>"},
		{it.trialID, "<trial-id>"}, {it.lab, "<lab>"}, {it.plan, "<plan>"}, {it.sessID, "<sess-id>"}, {it.sessName, "<sess>"},
		{it.proposalID, "<proposal-id>"}, {it.leaseSessID, "<second-sess-id>"}, {it.leaseSessName, "<second-sess>"}}
	sort.SliceStable(pairs, func(a, b int) bool { return len(pairs[a][0]) > len(pairs[b][0]) })
	for _, p := range pairs {
		if p[0] != "" {
			text = strings.ReplaceAll(text, p[0], p[1])
		}
	}
	return text
}

// restrictedHooks: the restricted account's SessionStart in every closed
// project's repository, and an unmapped one, records nothing and answers one
// text; its prompts in its own session carry no closed content; its A2A
// recall refuses every closed slug alike.
func (d *lmDaemon) restrictedHooks(t *testing.T, w *lmWorld) {
	ctx := context.Background()
	approvals := lmCount(t, d.db, `SELECT COUNT(*) FROM access_approvals`)
	secrets := []string{lmFinSecret, lmLegalSecret, lmConfSecret, lmSealedSecret}
	var notices []string
	for _, repo := range []string{"/work/finance", "/work/legal", "/work/conf", "/work/sealed", "/work/nothere"} {
		id := "claude-r" + strings.ReplaceAll(repo, "/", "-")
		out := d.hook(t, w.srv["hookR"].URL, "", "session-start", &lmAgentR, map[string]any{"session_id": id, "cwd": repo, "source": "startup"})
		for _, s := range secrets {
			require.NotContains(t, out, s, repo)
		}
		_, found, err := store.AmbientSessionByExternalIdentity(ctx, d.db, "claude-code", id)
		require.NoError(t, err)
		require.False(t, found, "%s: no session recorded", repo)
		notices = append(notices, out)
		for _, ev := range lmHookEvents[1:] {
			status, out := d.hookStatus(t, w.srv["hookR"].URL, "", ev, &lmAgentR, lmHookBody(id, repo))
			require.Less(t, status, 500, "%s %s: %s", ev, repo, out)
			for _, s := range secrets {
				require.NotContains(t, out, s, "%s in %s", ev, repo)
			}
		}
	}
	for _, n := range notices[1:] {
		require.Equal(t, notices[0], n, "the restricted notice is one text for every closed or unmapped repository")
	}
	// Its own session, in the open project, and the agent ids the owner's
	// leased sessions hold (claude-a, claude-p): nothing closed comes back,
	// nothing of theirs moves, and a taken id answers every hook exactly as a
	// free one does -- the account's agent ids are its own (SEAL A2).
	type answer struct {
		status int
		out    string
	}
	taken := map[string]string{"claude-a": "claude-free-a", "claude-p": "claude-free-p"}
	answers := map[string][]answer{}
	for _, ev := range lmHookEvents {
		for _, id := range []string{"claude-r", "claude-a", "claude-free-a", "claude-p", "claude-free-p"} {
			body := lmHookBody(id, "/work/open")
			if ev == "session-start" {
				// The agent's own start (lmHookBody's agent_type is a
				// subagent's, which starts nothing): it records the session
				// every later event acts in.
				body = map[string]any{"session_id": id, "cwd": "/work/open", "source": "startup"}
			}
			status, out := d.hookStatus(t, w.srv["hookR"].URL, "", ev, &lmAgentR, body)
			require.Less(t, status, 500, "%s naming %s: %s", ev, id, out)
			for _, s := range secrets {
				require.NotContains(t, out, s, "%s naming %s", ev, id)
			}
			answers[id] = append(answers[id], answer{status, out})
		}
	}
	ownName := func(id string) string {
		sess, found, err := store.AmbientSessionByExternalIdentity(ctx, d.db, "claude-code", access.QualifiedSessionName(lmAccount, id))
		require.NoError(t, err)
		require.True(t, found, "%s: the account's own session", id)
		require.Equal(t, lmAccount, sess.Account)
		return sess.Name
	}
	for id, free := range taken {
		takenName, freeName := ownName(id), ownName(free)
		for i, ev := range lmHookEvents {
			got, want := answers[id][i], answers[free][i]
			require.Equal(t, want.status, got.status, "%s naming %s answers apart from a free id", ev, id)
			require.Equal(t, strings.ReplaceAll(want.out, freeName, "<own>"), strings.ReplaceAll(got.out, takenName, "<own>"),
				"%s naming %s answers apart from a free id", ev, id)
		}
	}
	for _, x := range []string{"A", "P"} {
		sess, ok, err := store.SessionByID(ctx, d.db, w.sess[x].ID)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, core.SessionActive, sess.Status, "the restricted account cannot end the owner's leased session %s", x)
		require.Nil(t, lmMustLease(t, d, w.lease[x].ID).RevokedAt)
	}
	require.Equal(t, approvals, lmCount(t, d.db, `SELECT COUNT(*) FROM access_approvals`), "its hooks filed nothing")

	// A2A: every closed slug refused alike; global and open answer.
	var refusals []string
	for _, it := range []lmItems{w.fin, w.legal, w.conf, w.sealed, w.ghost} {
		out := lmA2ASend(t, w.srv["bridgeR"].URL, "", lmSearch, map[string]any{"project": it.project})
		for _, s := range secrets {
			require.NotContains(t, out, s, it.project)
		}
		require.Contains(t, out, "is not reachable from account "+lmAccount, it.project)
		refusals = append(refusals, lmNormalize(out, it))
	}
	for _, r := range refusals[1:] {
		require.Equal(t, refusals[0], r, "an A2A refusal never tells a closed project from a missing one")
	}
	for _, meta := range []map[string]any{nil, {"scope": "all"}} {
		out := lmA2ASend(t, w.srv["bridgeR"].URL, "", lmSearch+" ledger", meta)
		for _, s := range secrets {
			require.NotContains(t, out, s)
		}
	}
	require.Contains(t, lmA2ASend(t, w.srv["bridgeR"].URL, "", lmSearch, map[string]any{"project": lmOpen}), lmOpenToken)
}
