package main

// The read-grant rows of the peer_auth matrix (isolation.requests, step G of
// the locked-projects plan; step 10's acceptance: a grant only reads, and only
// for its own tree).
//
//   - TestPeerAuthMatrix_Grants: peer_auth on, isolation.requests on. A grant
//     approved for one agent's process tree reads its confidential target
//     through every read tool and writes nothing into it through every other
//     tool; the tree is write-confined to its own project; the grant never
//     reaches a sealed or a locked project; it is the tree's alone (its
//     descendants read; a sibling, an orphan, another session and the TCP key
//     do not); it ends at expiry, revoke and session_end. T-anchor row 5: a
//     tmux pane files no grant row.
//   - TestPeerAuthMatrix_Advisory: peer_auth off, isolation.requests on (the
//     advisory tier): the same, keyed on the session and decided by the
//     bearer key over the console, and its texts verbatim.
//   - TestPeerAuthMatrix_AdvisoryOff: both off: the fence refuses word for
//     word as on main and files nothing.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/store"
)

var gmShort = regexp.MustCompile(`R-[0-9A-Z]{4}`)

// gmOffRefusal is a read of project from outside refused at its confidential
// fence: the literal main's internal/mcp/isolation.go builds (fenceReadQuiet
// and bindRemedy, `git show main:internal/mcp/isolation.go`), word for word.
func gmOffRefusal(project string) string {
	return "project " + project + " is confidential: reads require a session bound to it -- if your task is about " +
		project + ", bind this connection to it with session_start project=" + project + "; otherwise leave it alone"
}

// The anchor rule's texts (internal/approval/anchor.go), as
// internal/mcp/anchor_test.go pins them.
const (
	gmTmuxRule = "Seamless files no access request for this process: it is tmux, a terminal multiplexer, not an agent, " +
		"and an approval would open the project to every program it started, other agent sessions included."
	gmAnchorCure = "Remedy: run the work from an agent session (Claude Code or Codex), which asks for itself; for a " +
		"one-off, the owner approves that session's request with `seam approve` in their own terminal."
)

// gmLines splits a one-item refusal into its lines.
func gmLines(t *testing.T, items []string) []string {
	t.Helper()
	require.Len(t, items, 1, "a refusal is one item: %v", items)
	return strings.Split(items[0], "\n")
}

// gmFacts reads one request's facts over the console (seam approve's GET).
func (r *gmRig) gmFacts(base, key, ref string) approval.Facts {
	r.t.Helper()
	status, body := r.consoleDo(base, key, http.MethodGet, "/console/access/"+url.PathEscape(ref)+"?format=json", nil, nil)
	require.Equal(r.t, http.StatusOK, status, body)
	var one struct {
		Request  approval.Facts `json:"request"`
		Advisory bool           `json:"advisory"`
	}
	require.NoError(r.t, json.Unmarshal([]byte(body), &one), body)
	require.NotEmpty(r.t, one.Request.Digest)
	require.Equal(r.t, !r.peer, one.Advisory, "the advisory flag is the tier's")
	return one.Request
}

// approveDaemon approves a pending grant request with seamlessd approve's
// core (approveWith, the daemon account's proof) for 1h.
func (r *gmRig) approveDaemon(short string) store.AccessGrant {
	r.t.Helper()
	a, err := r.grants.Resolve(r.ctx, short)
	require.NoError(r.t, err)
	facts, err := r.grants.Facts(r.ctx, a.ID)
	require.NoError(r.t, err)
	var out bytes.Buffer
	require.NoError(r.t, approveWith(r.ctx, r.svc, r.grants, &out, []string{short, "--for", approval.For1h, "--digest", facts.Digest}))
	require.Contains(r.t, out.String(), "Approved "+short+": "+gmConf+", read-only, for "+gmOpen+", until ")
	g := r.liveGrantOf(a.ID)
	require.Equal(r.t, "daemon-account", g.DecidedBy)
	return g
}

// approveKey approves a pending grant request over the console with the
// bearer key, as seam approve's y does without peer_auth.
func (r *gmRig) approveKey(short string) store.AccessGrant {
	r.t.Helper()
	f := r.gmFacts(r.tcp.URL, gmKey, short)
	status, body := r.consoleDo(r.tcp.URL, gmKey, http.MethodPost, "/console/access/"+url.PathEscape(f.ID)+"/approve?format=json",
		url.Values{"duration": {approval.For1h}, "digest": {f.Digest}}, nil)
	require.Equal(r.t, http.StatusOK, status, body)
	var got struct{ Approved, Project, From, Grant string }
	require.NoError(r.t, json.Unmarshal([]byte(body), &got), body)
	require.Equal(r.t, short, got.Approved)
	require.Equal(r.t, gmConf, got.Project)
	require.Equal(r.t, gmOpen, got.From)
	g := r.liveGrantOf(f.ID)
	require.Equal(r.t, "bearer-key", g.DecidedBy)
	return g
}

func (r *gmRig) liveGrantOf(approvalID string) store.AccessGrant {
	r.t.Helper()
	live, err := store.LiveAccessGrants(r.ctx, r.db, r.clock.Now())
	require.NoError(r.t, err)
	for _, g := range live {
		if g.ApprovalID == approvalID {
			return g
		}
	}
	require.Failf(r.t, "no live grant", "approval %s", approvalID)
	return store.AccessGrant{}
}

// reads runs every read probe of target on cli. granted: every probe must
// succeed, each want probe show the target's secret, and none another closed
// project's; else none may show any secret.
func (r *gmRig) reads(cli *mcpclient.Client, who string, target gmItems, granted bool) {
	t := r.t
	t.Helper()
	for tool, probes := range gmReadProbes(target) {
		for i, p := range probes {
			items, isErr := gmCall(t, cli, tool, p.args)
			text := gmText(items)
			what := who + ": " + tool + " probe " + string(rune('0'+i)) + " " + mustJSON(t, p.args)
			if granted {
				require.False(t, isErr, "%s: %s", what, text)
				if p.want {
					require.Contains(t, text, target.secret, "%s: a live grant reads the target", what)
				} else {
					t.Logf("measured: %s shows the target's content: %v", what, strings.Contains(text, target.secret))
				}
				gmNoSecretsBut(t, text, what, target.project)
			} else {
				gmNoSecretsBut(t, text, what)
			}
		}
	}
	// lab_open answers with the lab's recent trials: the granted target's
	// show only to a holder, and it leaves the target unchanged.
	for tool, probes := range gmStateProbes(target) {
		for i, args := range probes {
			items, isErr := gmCall(t, cli, tool, args)
			text := gmText(items)
			what := who + ": " + tool + " probe " + string(rune('0'+i)) + " " + mustJSON(t, args)
			if granted {
				require.False(t, isErr, "%s: %s", what, text)
				require.Contains(t, text, target.secret, "%s: the granted project's trials show", what)
				gmNoSecretsBut(t, text, what, target.project)
			} else {
				gmNoSecretsBut(t, text, what)
			}
			r.unchanged(target, what)
		}
	}
}

// writes runs every write probe of target on cli: each refused, and the
// target's content unchanged after each. handles false skips the probes that
// name the target's session with session=: without peer_auth such a call
// speaks for that session (the advisory tier's holder), which
// TestPeerAuthMatrix_Advisory's limit row covers on a spare session.
func (r *gmRig) writes(cli *mcpclient.Client, who string, target gmItems, registered map[string]bool, handles bool) {
	t := r.t
	t.Helper()
	for tool, probes := range gmWriteProbes(target) {
		if !registered[tool] {
			continue
		}
		for i, args := range probes {
			if _, named := args["session"]; named && !handles {
				continue
			}
			items, isErr := gmCall(t, cli, tool, args)
			what := who + ": " + tool + " probe " + string(rune('0'+i)) + " " + mustJSON(t, args)
			require.True(t, isErr, "%s must be refused: %v", what, items)
			gmNoSecretsBut(t, gmText(items), what, target.project)
			r.unchanged(target, what)
		}
	}
}

func TestPeerAuthMatrix_Grants(t *testing.T) {
	r := newGMRig(t, true, true)
	r.plantAll()
	conf, sealed, fin := r.items[gmConf], r.items[gmSealed], r.items[gmFin]
	gmCatalogCovered(t, conf)

	agentSess := r.stamp(gmOpen, "cc/open-agent", gmAgent)
	r.stamp(gmOpen, "cc/open-agent-2", gmAgent2)
	sealedSess := r.stamp(gmSealed, "cc/sealed-agent", gmSealAgent)
	r.stamp(gmOpen, "cc/pane", gmTmux)

	agentSrv := r.socket(gmBridge, gmAgent)
	agent := r.dial(agentSrv.URL, "", gmAgent)
	child := r.dial(r.socket(gmChild, gmAgent).URL, "", gmAgent)
	grand := r.dial(r.socket(gmGrand, gmChild, gmAgent).URL, "", gmAgent)
	// Same uid, outside the tree, forging the agent's process header.
	sibling := r.dial(r.socket(gmSibling, gmSibShell).URL, "", gmAgent)
	orphan := r.dial(r.socket(gmOrphan).URL, "", gmAgent)
	other := r.dial(r.socket(gmBridge2, gmAgent2).URL, "", gmAgent2)
	tcp := r.dial(r.tcp.URL, gmKey, gmAgent)
	sealedAgent := r.dial(r.socket(gmSealBridge, gmSealAgent).URL, "", gmSealAgent)
	registered := gmRegistered(t, agent)
	require.True(t, registered[mcp.AccessRequestTool], "a locked project registers access_request")

	var short string
	t.Run("before a grant: the fence refuses and files one request, anchored to the agent", func(t *testing.T) {
		r := r.at(t)
		items, isErr := gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
		require.True(t, isErr)
		ls := gmLines(t, items)
		require.Len(t, ls, 2, "the rule, then the remedy")
		require.Equal(t, "notes_read: "+gmOffRefusal(gmConf), ls[0], "the rule reads word for word as on main")
		short = gmShort.FindString(ls[1])
		require.NotEmpty(t, short, ls[1])
		require.Equal(t, "Remedy: read request "+short+" is waiting -- the owner approves it in their own terminal with "+
			"`seam approve "+short+"`; retry once it is approved. An approved read is read-only, and confines this agent's "+
			"writes to "+gmOpen+" for the rest of its life.", ls[1])
		gmNoSecretsBut(t, items[0], "the refusal")
		// A repeated refusal names the same request.
		items, _ = gmCall(t, agent, "memory_read", map[string]any{"id": conf.memID})
		require.Contains(t, items[0], short)
		require.Equal(t, 1, r.grantRows(""))
		a, err := r.grants.Resolve(r.ctx, short)
		require.NoError(t, err)
		require.Equal(t, gmAgent.Key(), a.ProcessKey, "anchored to the agent, not the bridge that asked")
		require.Equal(t, agentSess, a.SessionID)
		require.Equal(t, gmOpen, a.FromProject)
		require.Equal(t, gmClaude, a.Program)
		require.Equal(t, store.ApprovalPending, a.Status)
	})

	t.Run("nothing involving a sealed project files a grant request", func(t *testing.T) {
		r := r.at(t)
		items, isErr := gmCall(t, agent, "memory_read", map[string]any{"id": sealed.memID})
		require.True(t, isErr)
		require.Len(t, gmLines(t, items), 1, "a sealed project's refusal is final: %v", items)
		require.NotContains(t, items[0], "R-")
		items, isErr = gmCall(t, sealedAgent, "notes_read", map[string]any{"id": conf.noteID})
		require.True(t, isErr)
		require.Len(t, gmLines(t, items), 1, "a sealed project's session asks for nothing: %v", items)
		status, body := r.consoleDo(agentSrv.URL, "", http.MethodPost, "/console/access/request", url.Values{"project": {gmSealed}},
			map[string]string{mcp.AgentProcessHeader: gmAgent.AgentID(), mcp.HostHeader: gmHost})
		require.Equal(t, http.StatusBadRequest, status, body)
		require.Contains(t, body, "a sealed project is never granted")
		require.Zero(t, r.grantRows("project_slug = ?", gmSealed))
		require.Zero(t, r.grantRows("session_id = ?", sealedSess))
		require.Equal(t, 1, r.grantRows(""))
	})

	t.Run("the locked project needs a lease, never a grant", func(t *testing.T) {
		r := r.at(t)
		items, isErr := gmCall(t, agent, "tasks_list", map[string]any{"project": gmFin})
		require.True(t, isErr)
		require.Contains(t, items[0], "is locked")
		gmNoSecretsBut(t, gmText(items), "the lock's refusal")
		status, body := r.consoleDo(agentSrv.URL, "", http.MethodPost, "/console/access/request", url.Values{"project": {gmFin}},
			map[string]string{mcp.AgentProcessHeader: gmAgent.AgentID(), mcp.HostHeader: gmHost})
		require.Equal(t, http.StatusOK, status, body)
		var filed struct{ Kind, Project string }
		require.NoError(t, json.Unmarshal([]byte(body), &filed), body)
		require.Equal(t, store.AccessKindRequest, filed.Kind, "a locked project's request is a lock's")
		require.Zero(t, r.grantRows("project_slug = ?", gmFin))
	})

	t.Run("an open target needs no grant, and asking for one files nothing", func(t *testing.T) {
		r := r.at(t)
		items, isErr := gmCall(t, agent, "memory_read", map[string]any{"project": gmDocs, "name": "docs-style"})
		require.False(t, isErr, "%v", items)
		require.Contains(t, items[0], gmDocsToken)
		status, body := r.consoleDo(agentSrv.URL, "", http.MethodPost, "/console/access/request", url.Values{"project": {gmDocs}},
			map[string]string{mcp.AgentProcessHeader: gmAgent.AgentID(), mcp.HostHeader: gmHost})
		require.Equal(t, http.StatusBadRequest, status, body)
		require.Contains(t, body, "that project is neither locked nor confidential: nothing to request")
		for _, p := range []string{gmDocs, gmConf} {
			items, isErr = gmCall(t, agent, mcp.AccessRequestTool, map[string]any{"project": p})
			require.True(t, isErr)
			require.Equal(t, mcp.AccessRequestTool+": that project is not locked: nothing to request", items[0],
				"access_request asks for a lock only: a confidential project's grant is asked at its fence")
		}
		require.Equal(t, 1, r.grantRows(""))
	})

	t.Run("under peer_auth the bearer key approves nothing", func(t *testing.T) {
		r := r.at(t)
		f := r.gmFacts(r.tcp.URL, gmKey, short)
		require.Equal(t, gmOpen, f.From)
		for _, base := range []string{r.tcp.URL, agentSrv.URL} {
			key := ""
			if base == r.tcp.URL {
				key = gmKey
			}
			status, body := r.consoleDo(base, key, http.MethodPost, "/console/access/"+url.PathEscape(f.ID)+"/approve?format=json",
				url.Values{"duration": {approval.For1h}, "digest": {f.Digest}}, nil)
			require.Equal(t, http.StatusConflict, status, body)
			require.Contains(t, body, "approving needs a passkey here, or seam approve in the owner's terminal")
		}
		require.Zero(t, r.count(`SELECT COUNT(*) FROM access_grants`))
	})

	t.Run("the daemon account's proof approves a grant, never a lease", func(t *testing.T) {
		r := r.at(t)
		leasesBefore, err := store.LiveAccessLeases(r.ctx, r.db, r.clock.Now())
		require.NoError(t, err)
		grant := r.approveDaemon(short)
		require.Equal(t, gmAgent.Key(), grant.ProcessKey)
		require.Equal(t, agentSess, grant.SessionID)
		require.Equal(t, gmConf, grant.Project)
		require.Equal(t, gmOpen, grant.FromProject)
		leases, err := store.LiveAccessLeases(r.ctx, r.db, r.clock.Now())
		require.NoError(t, err)
		require.Equal(t, leasesBefore, leases, "a grant is never a lease")
	})

	t.Run("the grantee reads its target through every read tool", func(t *testing.T) {
		r := r.at(t)
		items, isErr := gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
		require.False(t, isErr, "%v", items)
		require.Len(t, items, 2, "the result, then the grant's news once")
		require.Contains(t, items[0], conf.secret)
		require.Contains(t, items[1], "Read access to "+gmConf+" was approved until")
		require.Contains(t, items[1], "This agent now writes only into "+gmOpen+", for the rest of its life")
		items, _ = gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
		require.Len(t, items, 1, "announced once")
		r.reads(agent, "the grantee", conf, true)
		require.Equal(t, 1, r.count(`SELECT COUNT(*) FROM events WHERE kind = 'access.confined' AND project_slug = ?`, gmOpen))
	})

	t.Run("the grantee's descendants read", func(t *testing.T) {
		r := r.at(t)
		r.reads(child, "a child", conf, true)
		r.reads(grand, "a grandchild", conf, true)
	})

	t.Run("outside the tree reads nothing and files nothing", func(t *testing.T) {
		r := r.at(t)
		before := r.grantRows("")
		for _, c := range []struct {
			who string
			cli *mcpclient.Client
		}{{"a same-uid sibling forging the header", sibling}, {"an orphaned descendant", orphan}, {"the TCP key", tcp}} {
			r.reads(c.cli, c.who, conf, false)
			items, isErr := gmCall(t, c.cli, "notes_read", map[string]any{"id": conf.noteID})
			require.True(t, isErr, "%s: %v", c.who, items)
			require.Equal(t, "notes_read: "+gmOffRefusal(gmConf), items[0], "%s: refused, and nothing filed", c.who)
		}
		require.Equal(t, before, r.grantRows(""), "nothing outside the tree filed a request")
		// Another agent session's tree is refused; its own refusal files its
		// own request, which opens nothing until approved.
		r.reads(other, "another session", conf, false)
		require.Equal(t, 1, r.grantRows("process_key = ? AND status = 'pending'", gmAgent2.Key()))
		require.Equal(t, 1, r.count(`SELECT COUNT(*) FROM access_grants`))
	})

	t.Run("the grantee writes nothing into its target", func(t *testing.T) {
		r := r.at(t)
		r.writes(agent, "the grantee", conf, registered, true)
		r.writes(child, "a child", conf, registered, true)
		r.unchanged(sealed, "after the grantee's writes")
		r.unchanged(fin, "after the grantee's writes")
	})

	t.Run("a rebind is the fence's own remedy, and changes nothing in the target", func(t *testing.T) {
		r := r.at(t)
		for _, args := range gmRebindProbes(conf) {
			cli := r.dial(agentSrv.URL, "", gmAgent)
			// Each binds the connection to the confidential project, as it would
			// for any open-project session: the name resumes the owner's own
			// session there (same account), the cwd is the project's mapped repo.
			out := gmOK(t, cli, "session_start", args)
			require.Equal(t, gmConf, out["project"], "session_start %v", args)
			r.unchanged(conf, "session_start "+mustJSON(t, args))
		}
	})

	// AGENTS.md (Read grants): CanWrite "consults only access.WriteConfined,
	// the confinement every grant a holder was ever approved leaves ...
	// (writes only into those from_projects, for the holder's life)"; under
	// peer_auth the holder is the agent's process tree. A connection of the
	// tree that rebinds elsewhere is the same holder: an implicit-scope create
	// there is refused like an explicit one.
	//
	// FAILS on the branch as of 4989ebee: resolveWriteScope
	// (internal/mcp/server.go) returns the bound, ambient or home project
	// without asking CanWrite, so WriteConfined is never consulted for a create
	// that names no project -- every create lands in the rebound project,
	// the global scope included.
	t.Run("a rebind does not lift the tree's confinement", func(t *testing.T) {
		r := r.at(t)
		for _, target := range []string{gmConf, gmDocs, "global"} {
			cli := r.dial(agentSrv.URL, "", gmAgent)
			out := gmOK(t, cli, "session_start", map[string]any{"project": target})
			if target == "global" {
				require.Empty(t, out["project"])
			} else {
				require.Equal(t, target, out["project"])
			}
			for _, c := range []struct {
				tool string
				args map[string]any
			}{
				{"memory_write", map[string]any{"name": "rebind-probe", "kind": "reference", "description": "d", "body": "b\n"}},
				{"notes_create", map[string]any{"title": "rebind probe", "body": "b"}},
				{"tasks_add", map[string]any{"title": "rebind probe"}},
				{"trial_record", map[string]any{"lab": conf.lab, "title": "rebind probe"}},
			} {
				items, isErr := gmCall(t, cli, c.tool, c.args)
				assert.True(t, isErr, "bound to %s by a rebind, the confined tree's %s must be refused: %v", target, c.tool, items)
			}
			slug := target
			if slug == "global" {
				slug = ""
			}
			r.noLanded(slug, target, "rebind-probe", "rebind probe")
		}
		// The same, by naming another project's session with session= on the
		// agent's own connection.
		items, isErr := gmCall(t, agent, "memory_write", map[string]any{"session": "docs-agent-session",
			"name": "handle-probe", "kind": "reference", "description": "d", "body": "b\n"})
		assert.True(t, isErr, "speaking for another project's session, the confined tree's write must be refused: %v", items)
		r.noLanded(gmDocs, "session= docs-agent-session", "handle-probe", "handle probe")
	})

	t.Run("a session= handle does not lift the tree's confinement for an explicit project", func(t *testing.T) {
		r := r.at(t)
		items, isErr := gmCall(t, agent, "memory_write", map[string]any{"session": "docs-agent-session", "project": gmDocs,
			"name": "probe-out", "kind": "reference", "description": "d", "body": "b\n"})
		require.True(t, isErr, "%v", items)
		require.Contains(t, items[0], "writes only into "+gmOpen)
		r.noProbes(gmDocs, "session= with project=")
	})

	t.Run("the grant opens its target only to the project it was approved from", func(t *testing.T) {
		r := r.at(t)
		cli := r.dial(agentSrv.URL, "", gmAgent)
		gmOK(t, cli, "session_start", map[string]any{"project": gmDocs})
		for _, rd := range []struct {
			tool string
			args map[string]any
		}{
			{"notes_read", map[string]any{"id": conf.noteID}}, {"memory_read", map[string]any{"id": conf.memID}},
			{"recall", map[string]any{"query": gmSearch, "project": gmConf}}, {"tasks_list", map[string]any{"id": conf.taskID}},
		} {
			items, isErr := gmCall(t, cli, rd.tool, rd.args)
			require.True(t, isErr, "bound to %s, the tree reads nothing of %s: %s %v", gmDocs, gmConf, rd.tool, items)
			gmNoSecretsBut(t, gmText(items), rd.tool)
		}
		items, _ := gmCall(t, cli, "recall", map[string]any{"query": gmSearch})
		gmNoSecretsBut(t, gmText(items), "recall from "+gmDocs)
	})

	t.Run("the open-project grantee is write-confined to its own project", func(t *testing.T) {
		r := r.at(t)
		for _, c := range []struct {
			tool  string
			args  map[string]any
			where string
		}{
			{"memory_write", map[string]any{"project": gmDocs, "name": "probe-out", "kind": "reference", "description": "d", "body": "b\n"}, "project " + gmDocs},
			{"memory_write", map[string]any{"project": "global", "name": "probe-out", "kind": "reference", "description": "d", "body": "b\n"}, "the global scope"},
			{"notes_create", map[string]any{"project": gmDocs, "title": "probe out", "body": "b"}, "project " + gmDocs},
			{"tasks_add", map[string]any{"project": gmDocs, "title": "probe out"}, "project " + gmDocs},
		} {
			for _, cli := range []*mcpclient.Client{agent, child} {
				items, isErr := gmCall(t, cli, c.tool, c.args)
				require.True(t, isErr, "%s %v: %v", c.tool, c.args, items)
				require.Equal(t, c.tool+": this agent was granted a read of another project, so it writes only into "+gmOpen+
					" for the rest of its life: a write into "+c.where+" is refused", items[0])
			}
		}
		r.noProbes(gmDocs, "the confined writes")
		r.noProbes("", "the confined writes")
		// Inside its own project, writes land.
		gmOK(t, agent, "memory_write", map[string]any{"name": "open-own", "kind": "reference", "description": "d", "body": "b\n"})
		_, found, err := store.MemoryByName(r.ctx, r.db, gmOpen, "open-own")
		require.NoError(t, err)
		require.True(t, found)
		note := gmOK(t, agent, "notes_create", map[string]any{"title": "open own note", "body": "b"})
		require.Equal(t, gmOpen, note["project"])
		task := gmOK(t, agent, "tasks_add", map[string]any{"title": "open own task"})
		got, err := store.TaskByID(r.ctx, r.db, task["id"].(string))
		require.NoError(t, err)
		require.Equal(t, gmOpen, got.ProjectSlug)
		// Confinement is the tree's: another session writes where it may.
		gmOK(t, other, "memory_write", map[string]any{"project": gmDocs, "name": "other-into-docs", "kind": "reference", "description": "d", "body": "b\n"})
	})

	t.Run("a planted grant on a sealed or a locked project opens nothing", func(t *testing.T) {
		r := r.at(t)
		now := r.clock.Now()
		for _, target := range []string{gmSealed, gmFin} {
			a, err := store.CreateGrantRequest(r.ctx, r.db, store.GrantRequest{Project: target, From: gmOpen, Lease: time.Hour,
				TTL: time.Minute, PeerUID: gmUID, PeerPID: gmAgent.PID, ProcessKey: gmAgent.Key(), Program: gmClaude,
				SessionID: agentSess}, now)
			require.NoError(t, err)
			_, err = store.ApproveAccessGrant(r.ctx, r.db, a.ID, a.Challenge, "daemon-account", time.Hour, now)
			require.NoError(t, err)
		}
		for _, it := range []gmItems{sealed, fin} {
			for _, rd := range []struct {
				tool string
				args map[string]any
			}{{"memory_read", map[string]any{"id": it.memID}}, {"notes_read", map[string]any{"id": it.noteID}},
				{"tasks_list", map[string]any{"id": it.taskID}}, {"recall", map[string]any{"query": gmSearch, "project": it.project}}} {
				items, isErr := gmCall(t, agent, rd.tool, rd.args)
				require.True(t, isErr, "%s %v: %v", rd.tool, rd.args, items)
				gmNoSecretsBut(t, gmText(items), rd.tool, gmConf)
			}
		}
		items, _ := gmCall(t, agent, "recall", map[string]any{"query": gmSearch})
		gmNoSecretsBut(t, gmText(items), "recall's scope", gmConf)
		items, _ = gmCall(t, agent, "trial_query", map[string]any{})
		gmNoSecretsBut(t, gmText(items), "trial_query", gmConf)
	})

	t.Run("T-anchor row 5: a tmux pane's confidential read files no grant row", func(t *testing.T) {
		r := r.at(t)
		pane := r.dial(r.socket(gmPane, gmTmux).URL, "", gmTmux)
		before := r.grantRows("")
		items, isErr := gmCall(t, pane, "notes_read", map[string]any{"id": conf.noteID})
		require.True(t, isErr)
		ls := gmLines(t, items)
		require.Len(t, ls, 2, "%v", items)
		require.Equal(t, "notes_read: "+gmOffRefusal(gmConf)+"; "+strings.Replace(gmTmuxRule, "no access request", "no read request", 1), ls[0])
		require.Equal(t, gmAnchorCure, ls[1])
		require.Equal(t, before, r.grantRows(""))
		require.Zero(t, r.grantRows("process_key = ?", gmTmux.Key()))
	})

	t.Run("expiry ends the read, never the confinement", func(t *testing.T) {
		r := r.at(t)
		read := map[string]any{"id": conf.noteID}
		r.clock.Advance(40 * time.Minute)
		items, isErr := gmCall(t, agent, "notes_read", read)
		require.False(t, isErr, "%v", items)
		require.Contains(t, gmText(items), "Access to "+gmConf+" ends at", "from 30 minutes before its end, every result says when")
		r.clock.Advance(21 * time.Minute)
		for _, cli := range []*mcpclient.Client{agent, child, grand} {
			items, isErr = gmCall(t, cli, "notes_read", read)
			require.True(t, isErr, "%v", items)
			require.True(t, strings.HasPrefix(items[0], "notes_read: "+gmOffRefusal(gmConf)+"\nRemedy: read request R-"), items[0])
		}
		r.reads(agent, "after expiry", conf, false)
		items, isErr = gmCall(t, agent, "memory_write", map[string]any{"project": gmDocs, "name": "probe-out", "kind": "reference", "description": "d", "body": "b\n"})
		require.True(t, isErr, "still confined: %v", items)
		r.noProbes(gmDocs, "after expiry")
	})

	t.Run("revoke ends the read; an agent's socket may not revoke", func(t *testing.T) {
		r := r.at(t)
		items, _ := gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
		g := r.approveDaemon(gmShort.FindString(items[0]))
		items, isErr := gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
		require.False(t, isErr, "%v", items)
		status, body := r.consoleDo(agentSrv.URL, "", http.MethodPost, "/console/access/grants/"+g.ID+"/revoke", url.Values{}, nil)
		require.Equal(t, http.StatusForbidden, status, body)
		_, isErr = gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
		require.False(t, isErr, "the socket's revoke changed nothing")
		status, body = r.consoleDo(r.tcp.URL, gmKey, http.MethodPost, "/console/access/grants/"+g.ID+"/revoke", url.Values{}, nil)
		require.Equal(t, http.StatusOK, status, body)
		r.reads(agent, "after revoke", conf, false)
		r.reads(child, "a child after revoke", conf, false)
	})

	t.Run("session_end ends the read, and the agent stays confined for its life", func(t *testing.T) {
		r := r.at(t)
		items, _ := gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
		g := r.approveDaemon(gmShort.FindString(items[0]))
		_, isErr := gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
		require.False(t, isErr)
		gmOK(t, agent, "session_end", map[string]any{"findings": "done"})
		ended, err := store.AccessGrantByID(r.ctx, r.db, g.ID)
		require.NoError(t, err)
		require.NotNil(t, ended.RevokedAt)
		require.Equal(t, store.LeaseRevokedSessionEnded, ended.RevokedReason)
		// The agent's next session (/clear): it reads nothing of the target,
		// and is still confined -- the grant was the process's.
		r.stamp(gmOpen, "cc/open-agent-next", gmAgent)
		r.reads(agent, "the next session", conf, false)
		items, isErr = gmCall(t, agent, "memory_write", map[string]any{"project": gmDocs, "name": "probe-out", "kind": "reference", "description": "d", "body": "b\n"})
		require.True(t, isErr, "%v", items)
		require.Contains(t, items[0], "writes only into "+gmOpen)
		gmOK(t, agent, "memory_write", map[string]any{"name": "open-own-next", "kind": "reference", "description": "d", "body": "b\n"})
		r.noProbes(gmDocs, "after session_end")
	})

	// seamlessd approvers reset's core: every live grant ends with the
	// credentials (reason reset).
	t.Run("an approvers reset ends the read", func(t *testing.T) {
		r := r.at(t)
		items, _ := gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
		g := r.approveDaemon(gmShort.FindString(items[0]))
		_, isErr := gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
		require.False(t, isErr)
		_, _, err := store.ResetApprovers(r.ctx, r.db, r.clock.Now())
		require.NoError(t, err)
		ended, err := store.AccessGrantByID(r.ctx, r.db, g.ID)
		require.NoError(t, err)
		require.Equal(t, store.LeaseRevokedReset, ended.RevokedReason)
		r.reads(agent, "after a reset", conf, false)
		r.unchanged(conf, "the end")
		r.unchanged(sealed, "the end")
		r.unchanged(fin, "the end")
	})
}

func TestPeerAuthMatrix_Advisory(t *testing.T) {
	r := newGMRig(t, false, true)
	r.plantAll()
	conf, sealed, fin := r.items[gmConf], r.items[gmSealed], r.items[gmFin]
	gmCatalogCovered(t, conf)

	s1 := r.stamp(gmOpen, "cc/open-1", gmAgent)
	r.stamp(gmOpen, "cc/open-2", gmAgent2)
	cli1 := r.dial(r.tcp.URL, gmKey, gmAgent)
	cli2 := r.dial(r.tcp.URL, gmKey, gmAgent2)
	unbound := r.dial(r.tcp.URL, gmKey, access.ProcessToken{})
	registered := gmRegistered(t, cli1)
	require.False(t, registered[mcp.AccessRequestTool], "nothing is locked: access_request is not registered")

	var short string
	t.Run("the refusal names the request, in the documented words", func(t *testing.T) {
		r := r.at(t)
		items, isErr := gmCall(t, cli1, "notes_read", map[string]any{"id": conf.noteID})
		require.True(t, isErr)
		ls := gmLines(t, items)
		require.Len(t, ls, 2)
		require.Equal(t, "notes_read: "+gmOffRefusal(gmConf), ls[0], "the rule reads word for word as on main")
		short = gmShort.FindString(ls[1])
		require.NotEmpty(t, short, ls[1])
		// docs-src/concepts/project-isolation.md, "Asking for access", with
		// its example's id and asking project:
		doc := "Remedy: read request R-7F3K is waiting -- the owner approves it in their own terminal with `seam approve R-7F3K`, " +
			"or with Approve on the console's Access requests page; retry once it is approved. An approved read is read-only, " +
			"and confines this session's writes to example-ops for the rest of its life."
		require.Equal(t, strings.ReplaceAll(strings.ReplaceAll(doc, "R-7F3K", short), "example-ops", gmOpen), ls[1])
		a, err := r.grants.Resolve(r.ctx, short)
		require.NoError(t, err)
		require.Equal(t, s1, a.SessionID, "keyed on the session")
		require.Empty(t, a.ProcessKey, "the advisory tier names no process")
		require.Equal(t, gmOpen, a.FromProject)
		require.Equal(t, 1, r.grantRows(""))
	})

	t.Run("nothing involving a sealed project files a request; an open project asks for none", func(t *testing.T) {
		r := r.at(t)
		items, isErr := gmCall(t, cli1, "memory_read", map[string]any{"id": sealed.memID})
		require.True(t, isErr)
		require.Len(t, gmLines(t, items), 1, "%v", items)
		sealedSess := r.stamp(gmSealed, "cc/sealed-agent", gmSealAgent)
		items, isErr = gmCall(t, r.dial(r.tcp.URL, gmKey, gmSealAgent), "notes_read", map[string]any{"id": conf.noteID})
		require.True(t, isErr)
		require.Len(t, gmLines(t, items), 1, "a sealed project's session asks for nothing: %v", items)
		require.Zero(t, r.grantRows("session_id = ?", sealedSess))
		status, body := r.consoleDo(r.tcp.URL, gmKey, http.MethodPost, "/console/access/request", url.Values{"project": {gmDocs}},
			map[string]string{mcp.AgentProcessHeader: gmAgent.AgentID(), mcp.HostHeader: gmHost})
		require.Equal(t, http.StatusBadRequest, status, body)
		require.Contains(t, body, "that project is not confidential: nothing to request")
		require.Zero(t, r.grantRows("project_slug IN (?, ?)", gmSealed, gmDocs))
	})

	t.Run("the bearer key approves it over the console", func(t *testing.T) {
		r := r.at(t)
		status, body := r.consoleDo(r.tcp.URL, gmKey, http.MethodGet, "/console/access?format=json", nil, nil)
		require.Equal(t, http.StatusOK, status, body)
		var list struct {
			Requests []approval.Facts `json:"requests"`
			Advisory bool             `json:"advisory"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &list), body)
		require.True(t, list.Advisory)
		require.Len(t, list.Requests, 1)
		require.Equal(t, short, list.Requests[0].Short)
		require.Equal(t, gmOpen, list.Requests[0].From)
		g := r.approveKey(short)
		require.Equal(t, s1, g.SessionID)
		require.Empty(t, g.ProcessKey)
	})

	t.Run("the session reads its target through every read tool", func(t *testing.T) {
		r := r.at(t)
		items, isErr := gmCall(t, cli1, "notes_read", map[string]any{"id": conf.noteID})
		require.False(t, isErr, "%v", items)
		require.Len(t, items, 2)
		require.Equal(t, "Read access to "+gmConf+" was approved until "+access.Clock(r.liveGrantFor(s1).ExpiresAt)+
			", read-only. Content from "+gmConf+" may be copied into "+gmOpen+"'s memories, and nowhere else. This session "+
			"now writes only into "+gmOpen+", for the rest of its life: a write to another project or the global scope is refused.", items[1])
		r.reads(cli1, "the grantee session", conf, true)
	})

	t.Run("the session writes nothing into its target, and is confined", func(t *testing.T) {
		r := r.at(t)
		r.writes(cli1, "the grantee session", conf, registered, false)
		items, isErr := gmCall(t, cli1, "memory_write", map[string]any{"project": gmDocs, "name": "probe-out", "kind": "reference", "description": "d", "body": "b\n"})
		require.True(t, isErr)
		require.Equal(t, "memory_write: this session was granted a read of another project, so it writes only into "+gmOpen+
			" for the rest of its life: a write into project "+gmDocs+" is refused", items[0])
		items, isErr = gmCall(t, cli1, "memory_write", map[string]any{"project": "global", "name": "probe-out", "kind": "reference", "description": "d", "body": "b\n"})
		require.True(t, isErr)
		require.Contains(t, items[0], "a write into the global scope is refused")
		gmOK(t, cli1, "memory_write", map[string]any{"name": "open-own", "kind": "reference", "description": "d", "body": "b\n"})
		r.noProbes(gmDocs, "confined")
		r.noProbes("", "confined")
		r.unchanged(sealed, "after the grantee's writes")
		r.unchanged(fin, "after the grantee's writes")
	})

	t.Run("another session, and an unbound connection, read nothing", func(t *testing.T) {
		r := r.at(t)
		r.reads(cli2, "another session", conf, false)
		r.reads(unbound, "an unbound connection", conf, false)
		gmOK(t, cli2, "memory_write", map[string]any{"project": gmDocs, "name": "other-into-docs", "kind": "reference", "description": "d", "body": "b\n"})
	})

	// "keyed on the session" (configuration reference, isolation.requests):
	// whatever connection a call arrives on, the call speaks for the session
	// it names, and carries that session's grants and confinement.
	t.Run("the grant is the session's: a call naming it with session= holds it", func(t *testing.T) {
		items, isErr := gmCall(t, unbound, "notes_read", map[string]any{"id": conf.noteID, "session": "cc/open-1"})
		require.False(t, isErr, "%v", items)
		require.Contains(t, items[0], conf.secret)
		items, isErr = gmCall(t, unbound, "memory_write", map[string]any{"session": "cc/open-1", "project": gmDocs,
			"name": "probe-out", "kind": "reference", "description": "d", "body": "b\n"})
		require.True(t, isErr, "and its confinement: %v", items)
		r.at(t).noProbes(gmDocs, "session= cc/open-1")
	})

	// The advisory tier's limit, pinned as it stands: its holder is the
	// session a call speaks for, self-reported like every binding without
	// peer_auth ("Honest limits"), so the grantee's agent, by binding or
	// naming ANOTHER session, writes where that session may. The docs say the
	// tier is advisory and keyed on the session; they do not spell this out.
	// Under peer_auth the holder is the process tree, and the same moves are
	// refused (TestPeerAuthMatrix_Grants).
	t.Run("advisory limit: another session the grantee's agent binds or names is not confined", func(t *testing.T) {
		r := r.at(t)
		items, isErr := gmCall(t, cli1, "memory_write", map[string]any{"session": "docs-agent-session", "project": gmDocs,
			"name": "advisory-handle", "kind": "reference", "description": "d", "body": "b\n"})
		require.False(t, isErr, "%v", items)
		rebound := r.dial(r.tcp.URL, gmKey, gmAgent)
		gmOK(t, rebound, "session_start", map[string]any{"project": gmDocs})
		_, isErr = gmCall(t, rebound, "memory_write", map[string]any{"name": "advisory-rebind", "kind": "reference", "description": "d", "body": "b\n"})
		require.False(t, isErr)
		for _, name := range []string{"advisory-handle", "advisory-rebind"} {
			_, found, err := store.MemoryByName(r.ctx, r.db, gmDocs, name)
			require.NoError(t, err)
			require.True(t, found, name)
		}
	})

	t.Run("expiry, revoke and the session's end each end the read", func(t *testing.T) {
		r := r.at(t)
		read := map[string]any{"id": conf.noteID}
		r.clock.Advance(61 * time.Minute)
		items, isErr := gmCall(t, cli1, "notes_read", read)
		require.True(t, isErr, "%v", items)
		r.reads(cli1, "after expiry", conf, false)

		g := r.approveKey(gmShort.FindString(items[0]))
		_, isErr = gmCall(t, cli1, "notes_read", read)
		require.False(t, isErr)
		status, body := r.consoleDo(r.tcp.URL, gmKey, http.MethodPost, "/console/access/grants/"+g.ID+"/revoke", url.Values{}, nil)
		require.Equal(t, http.StatusOK, status, body)
		items, isErr = gmCall(t, cli1, "notes_read", read)
		require.True(t, isErr)
		r.reads(cli1, "after revoke", conf, false)

		g = r.approveKey(gmShort.FindString(items[0]))
		_, isErr = gmCall(t, cli1, "notes_read", read)
		require.False(t, isErr)
		status, body = r.hook(r.tcp.URL, gmKey, "session-end", map[string]any{"session_id": "cc/open-1-ext", "cwd": "/work/" + gmOpen, "reason": "exit"})
		require.Equal(t, http.StatusOK, status, body)
		ended, err := store.AccessGrantByID(r.ctx, r.db, g.ID)
		require.NoError(t, err)
		require.NotNil(t, ended.RevokedAt, "the SessionEnd hook ends the session's grants")
		require.Equal(t, store.LeaseRevokedSessionEnded, ended.RevokedReason)
		r.reads(cli1, "after the session's end", conf, false)
		r.unchanged(conf, "the end")
	})
}

// noLanded asserts no memory named name, and no note, task or trial titled
// title, landed in project.
func (r *gmRig) noLanded(project, what, name, title string) {
	t := r.t
	t.Helper()
	_, found, err := store.MemoryByName(r.ctx, r.db, project, name)
	require.NoError(t, err)
	assert.False(t, found, "%s: a confined tree wrote memory %s into %q", what, name, project)
	assert.Zero(t, r.count(`SELECT COUNT(*) FROM notes_index WHERE project = ? AND title = ?`, project, title), "%s: a note", what)
	assert.Zero(t, r.count(`SELECT COUNT(*) FROM tasks WHERE project_slug = ? AND title = ?`, project, title), "%s: a task", what)
	assert.Zero(t, r.count(`SELECT COUNT(*) FROM trials WHERE project_slug = ? AND title = ?`, project, title), "%s: a trial", what)
}

// liveGrantFor is the session's live grant.
func (r *gmRig) liveGrantFor(sessionID string) store.AccessGrant {
	r.t.Helper()
	live, err := store.LiveAccessGrants(r.ctx, r.db, r.clock.Now())
	require.NoError(r.t, err)
	for _, g := range live {
		if g.SessionID == sessionID {
			return g
		}
	}
	require.Failf(r.t, "no live grant", "session %s", sessionID)
	return store.AccessGrant{}
}

func TestPeerAuthMatrix_AdvisoryOff(t *testing.T) {
	r := newGMRig(t, false, false)
	r.plantAll()
	conf := r.items[gmConf]
	r.stamp(gmOpen, "cc/open-1", gmAgent)
	cli := r.dial(r.tcp.URL, gmKey, gmAgent)
	for _, rd := range []struct {
		tool string
		args map[string]any
	}{
		{"notes_read", map[string]any{"id": conf.noteID}}, {"memory_read", map[string]any{"id": conf.memID}},
		{"recall", map[string]any{"query": gmSearch, "project": gmConf}}, {"tasks_list", map[string]any{"id": conf.taskID}},
	} {
		items, isErr := gmCall(t, cli, rd.tool, rd.args)
		require.True(t, isErr)
		require.Equal(t, []string{rd.tool + ": " + gmOffRefusal(gmConf)}, items, "word for word as on main, one line")
	}
	r.reads(cli, "requests off", conf, false)
	require.Zero(t, r.count(`SELECT COUNT(*) FROM access_approvals`))
	require.Zero(t, r.count(`SELECT COUNT(*) FROM access_grants`))
	require.Zero(t, r.count(`SELECT COUNT(*) FROM events WHERE kind LIKE 'access.%'`))
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/console/access?format=json"},
		{http.MethodPost, "/console/access/request"},
	} {
		var form url.Values
		if c.method == http.MethodPost {
			form = url.Values{"project": {gmConf}}
		}
		status, body := r.consoleDo(r.tcp.URL, gmKey, c.method, c.path, form,
			map[string]string{mcp.AgentProcessHeader: gmAgent.AgentID(), mcp.HostHeader: gmHost})
		require.Equal(t, http.StatusNotFound, status, "%s %s: %s", c.method, c.path, body)
	}
	require.Zero(t, r.count(`SELECT COUNT(*) FROM access_approvals`))
}
