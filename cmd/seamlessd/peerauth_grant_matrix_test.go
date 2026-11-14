package main

// The read-grant rows of the peer_auth matrix (isolation.requests, step G of
// the locked-projects plan; step 10's acceptance: a grant only reads, and only
// for its own tree), under the clearance rule (plan fenced-sessions P2): a
// write's destination must be cleared for everything the conversation holds.
//
//   - TestPeerAuthMatrix_Grants: peer_auth on, isolation.requests on. A grant
//     approved for one agent's process tree reads its confidential target
//     through every read tool; holding that content, the tree writes only into
//     the target -- never into an open project, its own included, nor the
//     global scope, and it carries nothing out of the target; the grant never
//     reaches a sealed or a locked project; it is the tree's alone (its
//     descendants read; a sibling, an orphan, another session and the TCP key
//     do not); it ends at expiry, revoke and the session's end, and the label
//     stays with the tree. T-anchor row 5: a tmux pane files no grant row.
//   - TestPeerAuthMatrix_Advisory: peer_auth off, isolation.requests on (the
//     advisory tier): the same, held by the conversation the agent-process
//     identity names and decided by the bearer key over the console, and its
//     texts verbatim.
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

// gmHolds is the clearance rule's refusal of a write into where, by a holder
// ("agent" or "session") holding held and cleared only for cleared ("" for
// none), as internal/mcp's clearanceRefusal words it.
func gmHolds(holder, held, cleared, where string) string {
	if cleared == "" {
		return "this " + holder + " holds content from " + held + ", and no project is cleared for all of it: a write into " +
			where + " is refused"
	}
	return "this " + holder + " holds content from " + held + ", so it writes only into " + cleared + ": a write into " +
		where + " is refused"
}

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
	require.Contains(r.t, out.String(), "Approved "+short+": ")
	require.Contains(r.t, out.String(), ", working in "+gmHome+", may read "+gmConf+" (read-only) until ")
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
	require.Equal(r.t, gmHome, got.From)
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
			before := r.grantRows("")
			items, isErr := gmCall(t, cli, tool, args)
			what := who + ": " + tool + " probe " + string(rune('0'+i)) + " " + mustJSON(t, args)
			if tool == mcp.AccessRequestTool && !isErr {
				// A holder of a live grant of the target asks again: the
				// grant answers, and nothing is filed (approval.AccessLiveError).
				// For a sealed target the move its refused reads filed is
				// found waiting instead (plan fenced-sessions P4).
				var res struct {
					Status, Kind string
					Filed        bool
				}
				require.NoError(t, json.Unmarshal([]byte(items[0]), &res), what)
				if res.Kind == store.AccessKindMove {
					require.Equal(t, store.ApprovalPending, res.Status, what)
					require.False(t, res.Filed, "%s: the waiting move, found", what)
				} else {
					require.Equal(t, store.ApprovalApproved, res.Status, what)
					require.Equal(t, store.AccessKindGrant, res.Kind, what)
				}
				require.Equal(t, before, r.grantRows(""), "%s: nothing filed", what)
			} else {
				require.True(t, isErr, "%s must be refused: %v", what, items)
			}
			gmNoSecretsBut(t, gmText(items), what, target.project)
			r.unchanged(target, what)
		}
	}
}

// gmGrantRule is a read refused at target's confidential fence to a
// conversation homed in a confidential project, as internal/mcp's
// fenceReadQuiet words it for an identified conversation: the kind rule
// offers a grant.
func gmGrantRule(target string) string {
	return "project " + target + " is confidential: only a conversation working in it, or one the owner granted a read " +
		"of it, reads it -- if your task is about " + target + ", ask the owner for a grant of it (session_start project=" +
		target + " asks); otherwise leave it alone"
}

// gmMoveRule is the same refusal from an open home, or of a sealed project:
// the kind rule offers a move.
func gmMoveRule(target, state string) string {
	return "project " + target + " is " + state + ": only a conversation working in it reads it -- if your task is about " +
		target + ", ask the owner to move this conversation into it (session_start project=" + target + " asks); " +
		"otherwise leave it alone"
}

func TestPeerAuthMatrix_Grants(t *testing.T) {
	r := newGMRig(t, true, true)
	r.plantAll()
	conf, sealed, fin := r.items[gmConf], r.items[gmSealed], r.items[gmFin]
	gmCatalogCovered(t, conf)

	agentSess := r.stamp(gmHome, "cc/home-agent", gmAgent)
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
	held := strings.Join([]string{gmConf, gmHome}, ", ")

	var short string
	t.Run("before a grant: the fence refuses and files one request, anchored to the agent", func(t *testing.T) {
		r := r.at(t)
		items, isErr := gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
		require.True(t, isErr)
		ls := gmLines(t, items)
		require.Len(t, ls, 2, "the rule, then the remedy")
		require.Equal(t, "notes_read: "+gmGrantRule(gmConf), ls[0])
		short = gmShort.FindString(ls[1])
		require.NotEmpty(t, short, ls[1])
		require.Equal(t, "Remedy: read request "+short+" is waiting -- the owner approves it in their own terminal with "+
			"`seam approve "+short+"`; retry once it is approved. An approved read is read-only, and nothing read from "+
			gmConf+" may be written anywhere but "+gmHome+". "+approval.CiteByReferenceLine(gmConf, gmHome), ls[1])
		gmNoSecretsBut(t, items[0], "the refusal")
		// A repeated refusal names the same request.
		items, _ = gmCall(t, agent, "memory_read", map[string]any{"id": conf.memID})
		require.Contains(t, items[0], short)
		require.Equal(t, 1, r.grantRows(""))
		a, err := r.grants.Resolve(r.ctx, short)
		require.NoError(t, err)
		require.Equal(t, gmAgent.Key(), a.ProcessKey, "anchored to the agent, not the bridge that asked")
		require.Equal(t, agentSess, a.SessionID)
		require.Equal(t, gmHome, a.FromProject)
		require.Equal(t, gmClaude, a.Program)
		require.Equal(t, store.ApprovalPending, a.Status)
	})

	t.Run("a sealed project is reached only by moving in: a read asks for a move, never a grant", func(t *testing.T) {
		r := r.at(t)
		items, isErr := gmCall(t, agent, "memory_read", map[string]any{"id": sealed.memID})
		require.True(t, isErr)
		ls := gmLines(t, items)
		require.Len(t, ls, 2, "%v", items)
		require.Equal(t, "memory_read: "+gmMoveRule(gmSealed, "sealed"), ls[0])
		require.True(t, strings.HasPrefix(ls[1], "Remedy: move request R-"), ls[1])
		require.Contains(t, ls[1], "Record what belongs in "+gmHome+" now")
		require.NotContains(t, ls[1], "Touch ID")
		items, isErr = gmCall(t, sealedAgent, "notes_read", map[string]any{"id": conf.noteID})
		require.True(t, isErr)
		require.Len(t, gmLines(t, items), 1, "a sealed project's session asks for nothing: %v", items)
		status, body := r.consoleDo(agentSrv.URL, "", http.MethodPost, "/console/access/request", url.Values{"project": {gmSealed}},
			map[string]string{mcp.AgentProcessHeader: gmAgent.AgentID(), mcp.HostHeader: gmHost})
		require.Equal(t, http.StatusOK, status, body)
		var found struct {
			Kind  string
			Filed bool
		}
		require.NoError(t, json.Unmarshal([]byte(body), &found), body)
		require.Equal(t, store.AccessKindMove, found.Kind, "seam access request finds the waiting move")
		require.False(t, found.Filed)
		require.Zero(t, r.grantRows("project_slug = ?", gmSealed))
		require.Zero(t, r.grantRows("session_id = ?", sealedSess))
		require.Zero(t, r.moveRows("session_id = ?", sealedSess))
		require.Equal(t, 1, r.moveRows("project_slug = ? AND process_key = ?", gmSealed, gmAgent.Key()))
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
		items, isErr = gmCall(t, agent, mcp.AccessRequestTool, map[string]any{"project": gmDocs})
		require.True(t, isErr)
		require.Equal(t, mcp.AccessRequestTool+": that project is neither locked nor confidential: nothing to request", items[0])
		// A confidential project no lock covers: access_request asks for its
		// read grant, as `seam access request` does, and finds the request
		// its fence filed.
		items, isErr = gmCall(t, agent, mcp.AccessRequestTool, map[string]any{"project": gmConf})
		require.False(t, isErr, "%v", items)
		var asked struct {
			ShortID                     string `json:"short_id"`
			Kind, Project, From, Status string
		}
		require.NoError(t, json.Unmarshal([]byte(items[0]), &asked), items[0])
		require.Equal(t, short, asked.ShortID, "the fence's request, found again")
		require.Equal(t, store.AccessKindGrant, asked.Kind)
		require.Equal(t, gmConf, asked.Project)
		require.Equal(t, gmHome, asked.From)
		require.Equal(t, store.ApprovalPending, asked.Status)
		require.Equal(t, 1, r.grantRows(""))
	})

	t.Run("under peer_auth the bearer key approves nothing", func(t *testing.T) {
		r := r.at(t)
		f := r.gmFacts(r.tcp.URL, gmKey, short)
		require.Equal(t, gmHome, f.From)
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
		require.Equal(t, gmHome, grant.FromProject)
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
		require.Contains(t, items[1], "Nothing read from "+gmConf+" may be written anywhere but "+gmHome+".")
		items, _ = gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
		require.Len(t, items, 1, "announced once")
		r.reads(agent, "the grantee", conf, true)
		require.Zero(t, r.count(`SELECT COUNT(*) FROM events WHERE kind = 'access.confined'`), "a confidential home is not confined")
	})

	t.Run("the grantee's descendants read", func(t *testing.T) {
		r := r.at(t)
		r.reads(child, "a child", conf, true)
		r.reads(grand, "a grandchild", conf, true)
	})

	t.Run("outside the tree reads nothing and files nothing", func(t *testing.T) {
		r := r.at(t)
		before, moves := r.grantRows(""), r.moveRows("")
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
		require.Equal(t, moves, r.moveRows(""), "nothing outside the tree filed a request")
		// Another agent session's tree, in an open home, is refused; its own
		// refusal files its own request -- a move into the project (P4) --
		// which opens nothing until approved.
		r.reads(other, "another session", conf, false)
		require.Equal(t, 1, r.moveRows("process_key = ? AND status = 'pending' AND project_slug = ?", gmAgent2.Key(), gmConf))
		require.Zero(t, r.grantRows("process_key = ?", gmAgent2.Key()))
		require.Equal(t, 1, r.count(`SELECT COUNT(*) FROM access_grants`))
	})

	// A read-only grant clears its target's content into the home, never the
	// home's into the target: the grantee writes into its home, and into its
	// target only with read and write -- its refused write asks for that.
	// Carrying the target's content out is a write into an open project, and
	// is refused with nothing to ask.
	t.Run("the read-only grantee writes into its home, asks to write its target, and carries nothing out", func(t *testing.T) {
		r := r.at(t)
		for i, cli := range []*mcpclient.Client{agent, child} {
			got := gmOK(t, cli, "memory_write", map[string]any{"name": "grantee-home-" + string(rune('a'+i)),
				"kind": "reference", "description": "d", "body": "b\n"})
			require.Equal(t, gmHome, got["project"])
			items, isErr := gmCall(t, cli, "memory_write", map[string]any{"project": gmConf, "name": "grantee-in-" + string(rune('a'+i)),
				"kind": "reference", "description": "d", "body": "b\n"})
			require.True(t, isErr, "%v", items)
			ls := gmLines(t, items)
			require.Len(t, ls, 2, "%v", items)
			require.Equal(t, "memory_write: "+gmHolds("agent", held, gmHome, "project "+gmConf), ls[0])
			require.True(t, strings.HasPrefix(ls[1], "Remedy: read-and-write request R-"), ls[1])
			require.Contains(t, ls[1], "Approved read and write, this agent changes "+gmConf+"'s tasks, memories and notes")
			for _, c := range []struct {
				tool string
				args map[string]any
			}{
				{"notes_update", map[string]any{"id": conf.noteID, "project": gmOpen}},
				{"tasks_update", map[string]any{"id": conf.openTaskID, "project": gmOpen}},
				{"tasks_update", map[string]any{"id": conf.taskID, "project": gmOpen}},
			} {
				// Moving the target's item out is a write of the item first:
				// refused there, asking for read and write as above.
				items, isErr := gmCall(t, cli, c.tool, c.args)
				require.True(t, isErr, "%s %v: %v", c.tool, c.args, items)
				ls := gmLines(t, items)
				require.Equal(t, c.tool+": "+gmHolds("agent", held, gmHome, "project "+gmConf), ls[0])
				require.True(t, strings.HasPrefix(ls[1], "Remedy: read-and-write request R-"), ls[1])
			}
		}
		require.Equal(t, 1, r.grantRows("requested_access = 'write' AND status = 'pending' AND process_key = ?", gmAgent.Key()),
			"one waiting read-and-write request, whichever call asked")
		// Every write tool into a project its label does not clear: refused,
		// and nothing there changes.
		r.writes(agent, "the grantee", sealed, registered, true)
		r.unchanged(conf, "after the grantee's writes")
		r.unchanged(sealed, "after the grantee's writes")
		r.unchanged(fin, "after the grantee's writes")
	})

	// session_start into the target is no rebind: between confidential
	// projects it asks for a grant of read and write, and the home stays
	// (D4 of the fenced-sessions design).
	t.Run("session_start into the target binds nothing, asks for read and write, and changes nothing there", func(t *testing.T) {
		r := r.at(t)
		for _, args := range gmRebindProbes(conf) {
			cli := r.dial(agentSrv.URL, "", gmAgent)
			items, isErr := gmCall(t, cli, "session_start", args)
			require.True(t, isErr, "session_start %v: %v", args, items)
			ls := gmLines(t, items)
			require.Len(t, ls, 2, "%v", items)
			require.Equal(t, "session_start: nothing was bound: project "+gmConf+" is confidential, and this conversation "+
				"works in confidential project "+gmHome+", which stays its home: it reaches "+gmConf+" only with the owner's grant", ls[0])
			require.True(t, strings.HasPrefix(ls[1], "Remedy: read-and-write request R-"), ls[1])
			require.True(t, strings.HasSuffix(ls[1], "Once it is approved, name project="+gmConf+" on the calls that work there"), ls[1])
			r.unchanged(conf, "session_start "+mustJSON(t, args))
		}
		require.Equal(t, 1, r.grantRows("requested_access = 'write' AND status = 'pending' AND process_key = ?", gmAgent.Key()))
	})

	// The clearance rule judges the conversation -- under peer_auth the
	// agent's process tree -- never the connection's binding, and with
	// isolation.requests on a binding cannot take the tree anywhere its home
	// may not read and write: session_start into an open project or the global
	// scope is refused (there is no way back), and a session= handle naming
	// another project's session is refused at the handle. An unscoped write
	// lands in the home.
	t.Run("a rebind does not lift the tree's label", func(t *testing.T) {
		r := r.at(t)
		for i, target := range []string{gmDocs, "global"} {
			name, title := "rebind-probe-"+string(rune('a'+i)), "rebind probe "+string(rune('a'+i))
			cli := r.dial(agentSrv.URL, "", gmAgent)
			items, isErr := gmCall(t, cli, "session_start", map[string]any{"project": target})
			require.True(t, isErr, "%v", items)
			where := "project " + target
			if target == "global" {
				where = "the global scope"
			}
			require.True(t, strings.HasPrefix(items[0], "session_start: nothing was bound: this conversation holds content from "+
				held+", so it does not work in "+where+": "), items[0])
			for _, c := range []struct {
				tool string
				args map[string]any
			}{
				{"memory_write", map[string]any{"name": name, "kind": "reference", "description": "d", "body": "b\n"}},
				{"notes_create", map[string]any{"title": title, "body": "b"}},
				{"tasks_add", map[string]any{"title": title}},
			} {
				items, isErr := gmCall(t, cli, c.tool, c.args)
				assert.False(t, isErr, "nothing was bound: the tree's %s lands in its home: %v", c.tool, items)
			}
			slug := target
			if slug == "global" {
				slug = ""
			}
			r.noLanded(slug, target, name, title)
		}
		_, found, err := store.MemoryByName(r.ctx, r.db, gmHome, "rebind-probe-a")
		require.NoError(t, err)
		require.True(t, found, "the unscoped writes landed in the home")
		// The same, by naming another project's session with session= on the
		// agent's own connection.
		items, isErr := gmCall(t, agent, "memory_write", map[string]any{"session": "docs-agent-session",
			"name": "handle-probe", "kind": "reference", "description": "d", "body": "b\n"})
		assert.True(t, isErr, "speaking for another project's session, the tree's write must be refused: %v", items)
		r.noLanded(gmDocs, "session= docs-agent-session", "handle-probe", "handle probe")
	})

	t.Run("a session= handle does not lift the tree's label for an explicit project", func(t *testing.T) {
		r := r.at(t)
		items, isErr := gmCall(t, agent, "memory_write", map[string]any{"session": "docs-agent-session", "project": gmDocs,
			"name": "probe-out", "kind": "reference", "description": "d", "body": "b\n"})
		require.True(t, isErr, "%v", items)
		require.Equal(t, "memory_write: session docs-agent-session works in project "+gmDocs+", and this conversation works in "+
			"project "+gmHome+": it holds content from "+held+", which may not be written into project "+gmDocs+", so it may "+
			"not act as that session\nRemedy: leave session= off to act as your own session cc/home-agent in project "+gmHome, items[0])
		r.noProbes(gmDocs, "session= with project=")
	})

	t.Run("the grant opens its target only from the home it was approved from", func(t *testing.T) {
		for _, rd := range []struct {
			tool string
			args map[string]any
		}{
			{"notes_read", map[string]any{"id": conf.noteID}}, {"memory_read", map[string]any{"id": conf.memID}},
			{"recall", map[string]any{"query": gmSearch, "project": gmConf}}, {"tasks_list", map[string]any{"id": conf.taskID}},
		} {
			args := map[string]any{"session": "docs-agent-session"}
			for k, v := range rd.args {
				args[k] = v
			}
			items, isErr := gmCall(t, agent, rd.tool, args)
			require.True(t, isErr, "acting as %s, the tree reads nothing of %s: %s %v", gmDocs, gmConf, rd.tool, items)
			gmNoSecretsBut(t, gmText(items), rd.tool)
		}
		items, _ := gmCall(t, agent, "recall", map[string]any{"query": gmSearch, "session": "docs-agent-session"})
		gmNoSecretsBut(t, gmText(items), "recall acting as "+gmDocs)
	})

	// Leak path 2 of the plan closed: holding the target's content, the
	// grantee writes into no open project nor the global scope; its home is
	// cleared for what it read.
	t.Run("the grantee writes only into its home", func(t *testing.T) {
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
			{"memory_write", map[string]any{"project": gmOpen, "name": "probe-out", "kind": "reference", "description": "d", "body": "b\n"}, "project " + gmOpen},
		} {
			for _, cli := range []*mcpclient.Client{agent, child} {
				items, isErr := gmCall(t, cli, c.tool, c.args)
				require.True(t, isErr, "%s %v: %v", c.tool, c.args, items)
				require.Equal(t, []string{c.tool + ": " + gmHolds("agent", held, gmHome, c.where)}, items)
			}
		}
		r.noProbes(gmDocs, "the held writes")
		r.noProbes(gmOpen, "the held writes")
		r.noProbes("", "the held writes")
		// Into its home, writes land.
		gmOK(t, agent, "memory_write", map[string]any{"name": "home-own", "kind": "reference", "description": "d", "body": "b\n"})
		_, found, err := store.MemoryByName(r.ctx, r.db, gmHome, "home-own")
		require.NoError(t, err)
		require.True(t, found)
		note := gmOK(t, agent, "notes_create", map[string]any{"title": "home own note", "body": "b"})
		require.Equal(t, gmHome, note["project"])
		task := gmOK(t, agent, "tasks_add", map[string]any{"title": "home own task"})
		got, err := store.TaskByID(r.ctx, r.db, task["id"].(string))
		require.NoError(t, err)
		require.Equal(t, gmHome, got.ProjectSlug)
		// The label is the tree's: another session writes where it may.
		gmOK(t, other, "memory_write", map[string]any{"project": gmDocs, "name": "other-into-docs", "kind": "reference", "description": "d", "body": "b\n"})
	})

	t.Run("a planted grant on a sealed or a locked project opens nothing", func(t *testing.T) {
		r := r.at(t)
		now := r.clock.Now()
		for _, target := range []string{gmSealed, gmFin} {
			a, err := store.CreateGrantRequest(r.ctx, r.db, store.GrantRequest{Project: target, From: gmHome, Lease: time.Hour,
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
	// The planted grants count: the label holds every target the tree was
	// ever approved, and each one's content is cleared into the home.
	held = strings.Join([]string{gmConf, gmFin, gmHome, gmSealed}, ", ")

	t.Run("T-anchor row 5: a tmux pane's confidential read files no request", func(t *testing.T) {
		r := r.at(t)
		pane := r.dial(r.socket(gmPane, gmTmux).URL, "", gmTmux)
		before, moves := r.grantRows(""), r.moveRows("")
		items, isErr := gmCall(t, pane, "notes_read", map[string]any{"id": conf.noteID})
		require.True(t, isErr)
		ls := gmLines(t, items)
		require.Len(t, ls, 2, "%v", items)
		require.Equal(t, "notes_read: "+gmMoveRule(gmConf, "confidential")+"; "+strings.Replace(gmTmuxRule, "no access request", "no request", 1), ls[0])
		require.Equal(t, gmAnchorCure, ls[1])
		require.Equal(t, before, r.grantRows(""))
		require.Equal(t, moves, r.moveRows(""))
		require.Zero(t, r.count(`SELECT COUNT(*) FROM access_approvals WHERE process_key = ?`, gmTmux.Key()))
	})

	t.Run("expiry ends the read, never the label", func(t *testing.T) {
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
			require.True(t, strings.HasPrefix(items[0], "notes_read: "+gmGrantRule(gmConf)+"\nRemedy: read request R-"), items[0])
		}
		r.reads(agent, "after expiry", conf, false)
		items, isErr = gmCall(t, agent, "memory_write", map[string]any{"project": gmDocs, "name": "probe-out", "kind": "reference", "description": "d", "body": "b\n"})
		require.True(t, isErr, "still held: %v", items)
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

	// session_end in a confidential home writes its findings there, which the
	// home is cleared for, and ends the read; the label stays with the
	// agent's tree for its life.
	t.Run("the session's end ends the read, and the agent's label stays for its life", func(t *testing.T) {
		r := r.at(t)
		items, _ := gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
		g := r.approveDaemon(gmShort.FindString(items[0]))
		_, isErr := gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
		require.False(t, isErr)
		end := gmOK(t, agent, "session_end", map[string]any{"findings": "done, citing example-conf by reference"})
		require.Nil(t, end["findings_withheld"], "the home is cleared for what it holds")
		ended, err := store.AccessGrantByID(r.ctx, r.db, g.ID)
		require.NoError(t, err)
		require.NotNil(t, ended.RevokedAt)
		require.Equal(t, store.LeaseRevokedSessionEnded, ended.RevokedReason)
		// The agent's next session (/clear): it reads nothing of the target,
		// and its label is still the tree's.
		r.stamp(gmHome, "cc/home-agent-next", gmAgent)
		r.reads(agent, "the next session", conf, false)
		items, isErr = gmCall(t, agent, "memory_write", map[string]any{"project": gmDocs, "name": "probe-out", "kind": "reference", "description": "d", "body": "b\n"})
		require.True(t, isErr, "%v", items)
		require.Equal(t, "memory_write: "+gmHolds("agent", held, gmHome, "project "+gmDocs), items[0])
		got := gmOK(t, agent, "memory_write", map[string]any{"name": "next-home", "kind": "reference", "description": "d", "body": "b\n"})
		require.Equal(t, gmHome, got["project"])
		r.noProbes(gmDocs, "after the session's end")
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

	s1 := r.stamp(gmHome, "cc/home-1", gmAgent)
	r.stamp(gmOpen, "cc/open-2", gmAgent2)
	cli1 := r.dial(r.tcp.URL, gmKey, gmAgent)
	cli2 := r.dial(r.tcp.URL, gmKey, gmAgent2)
	unbound := r.dial(r.tcp.URL, gmKey, access.ProcessToken{})
	registered := gmRegistered(t, cli1)
	require.False(t, registered[mcp.AccessRequestTool], "nothing is locked: access_request is not registered")
	held := strings.Join([]string{gmConf, gmHome}, ", ")

	var short string
	t.Run("the refusal names the request, in the documented words", func(t *testing.T) {
		r := r.at(t)
		items, isErr := gmCall(t, cli1, "notes_read", map[string]any{"id": conf.noteID})
		require.True(t, isErr)
		ls := gmLines(t, items)
		require.Len(t, ls, 2)
		require.Equal(t, "notes_read: "+gmGrantRule(gmConf), ls[0])
		short = gmShort.FindString(ls[1])
		require.NotEmpty(t, short, ls[1])
		require.Equal(t, "Remedy: read request "+short+" is waiting -- the owner approves it in their own terminal with "+
			"`seam approve "+short+"`, or with Approve on the console's Access requests page; retry once it is approved. "+
			"An approved read is read-only, and nothing read from "+gmConf+" may be written anywhere but "+gmHome+". "+
			approval.CiteByReferenceLine(gmConf, gmHome), ls[1])
		a, err := r.grants.Resolve(r.ctx, short)
		require.NoError(t, err)
		require.Equal(t, s1, a.SessionID, "filed for the conversation's own row")
		require.Empty(t, a.ProcessKey, "the advisory tier names no process")
		require.Equal(t, gmHome, a.FromProject)
		require.Equal(t, 1, r.grantRows(""))
	})

	t.Run("a sealed project asks for a move, a sealed project's session for nothing, an open project for none", func(t *testing.T) {
		r := r.at(t)
		items, isErr := gmCall(t, cli1, "memory_read", map[string]any{"id": sealed.memID})
		require.True(t, isErr)
		ls := gmLines(t, items)
		require.Len(t, ls, 2, "%v", items)
		require.Equal(t, "memory_read: "+gmMoveRule(gmSealed, "sealed"), ls[0])
		require.True(t, strings.HasPrefix(ls[1], "Remedy: move request R-"), ls[1])
		require.Contains(t, ls[1], "until the conversation ends", "the advisory tier's conversation")
		sealedSess := r.stamp(gmSealed, "cc/sealed-agent", gmSealAgent)
		items, isErr = gmCall(t, r.dial(r.tcp.URL, gmKey, gmSealAgent), "notes_read", map[string]any{"id": conf.noteID})
		require.True(t, isErr)
		require.Len(t, gmLines(t, items), 1, "a sealed project's session asks for nothing: %v", items)
		require.Zero(t, r.grantRows("session_id = ?", sealedSess))
		require.Zero(t, r.moveRows("session_id = ?", sealedSess))
		status, body := r.consoleDo(r.tcp.URL, gmKey, http.MethodPost, "/console/access/request", url.Values{"project": {gmDocs}},
			map[string]string{mcp.AgentProcessHeader: gmAgent.AgentID(), mcp.HostHeader: gmHost})
		require.Equal(t, http.StatusBadRequest, status, body)
		require.Contains(t, body, "that project is not confidential: nothing to request")
		require.Zero(t, r.grantRows("project_slug IN (?, ?)", gmSealed, gmDocs))
		require.Equal(t, 1, r.moveRows("project_slug = ? AND session_id = ?", gmSealed, s1))
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
		var listed *approval.Facts
		for i := range list.Requests {
			if list.Requests[i].Short == short {
				listed = &list.Requests[i]
			}
		}
		require.NotNil(t, listed, "%v", list.Requests)
		require.Equal(t, gmHome, listed.From)
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
			", read-only. Nothing read from "+gmConf+" may be written anywhere but "+gmHome+". "+
			approval.CiteByReferenceLine(gmConf, gmHome), items[1])
		r.reads(cli1, "the grantee session", conf, true)
	})

	t.Run("the session writes only into its home", func(t *testing.T) {
		r := r.at(t)
		got := gmOK(t, cli1, "memory_write", map[string]any{"name": "session-home", "kind": "reference", "description": "d", "body": "b\n"})
		require.Equal(t, gmHome, got["project"])
		for where, args := range map[string]map[string]any{
			"project " + gmDocs: {"project": gmDocs, "name": "probe-out", "kind": "reference", "description": "d", "body": "b\n"},
			"the global scope":  {"project": "global", "name": "probe-out", "kind": "reference", "description": "d", "body": "b\n"},
			"project " + gmOpen: {"project": gmOpen, "name": "probe-out", "kind": "reference", "description": "d", "body": "b\n"},
		} {
			items, isErr := gmCall(t, cli1, "memory_write", args)
			require.True(t, isErr, "%s: %v", where, items)
			require.Equal(t, []string{"memory_write: " + gmHolds("session", held, gmHome, where)}, items)
		}
		items, isErr := gmCall(t, cli1, "memory_write", map[string]any{"project": gmConf, "name": "probe-in", "kind": "reference",
			"description": "d", "body": "b\n"})
		require.True(t, isErr, "%v", items)
		ls := gmLines(t, items)
		require.Equal(t, "memory_write: "+gmHolds("session", held, gmHome, "project "+gmConf), ls[0])
		require.True(t, strings.HasPrefix(ls[1], "Remedy: read-and-write request R-"), ls[1])
		items, isErr = gmCall(t, cli1, "notes_update", map[string]any{"id": conf.noteID, "project": gmOpen})
		require.True(t, isErr, "%v", items)
		require.Equal(t, "notes_update: "+gmHolds("session", held, gmHome, "project "+gmConf), gmLines(t, items)[0],
			"moving the target's note out is a write of it first")
		r.noProbes(gmDocs, "held")
		r.noProbes(gmOpen, "held")
		r.noProbes(gmConf, "held")
		r.noProbes("", "held")
		r.writes(cli1, "the grantee session", sealed, registered, false)
		r.unchanged(conf, "after the grantee's writes")
		r.unchanged(sealed, "after the grantee's writes")
		r.unchanged(fin, "after the grantee's writes")
	})

	t.Run("another session, and an unbound connection, read nothing", func(t *testing.T) {
		r := r.at(t)
		r.reads(cli2, "another session", conf, false)
		r.reads(unbound, "an unbound connection", conf, false)
		gmOK(t, cli2, "memory_write", map[string]any{"project": gmDocs, "name": "other-into-docs", "kind": "reference", "description": "d", "body": "b\n"})
	})

	// A call that names no agent process speaks for the session it names, and
	// a session that names a conversation carries that conversation's grants
	// and label, whatever connection the call arrives on.
	t.Run("the grant is the conversation's: a call naming its session with session= holds it", func(t *testing.T) {
		items, isErr := gmCall(t, unbound, "notes_read", map[string]any{"id": conf.noteID, "session": "cc/home-1"})
		require.False(t, isErr, "%v", items)
		require.Contains(t, items[0], conf.secret)
		items, isErr = gmCall(t, unbound, "memory_write", map[string]any{"session": "cc/home-1", "project": gmDocs,
			"name": "probe-out", "kind": "reference", "description": "d", "body": "b\n"})
		require.True(t, isErr, "and its label: %v", items)
		r.at(t).noProbes(gmDocs, "session= cc/home-1")
	})

	// The advisory tier's holder is the conversation the agent-process
	// identity names, never the binding: the grantee's agent cannot bind or
	// name a session where its home may not read and write (plan
	// fenced-sessions P4), and nothing it writes lands there. The limit that
	// stands: a client that names no agent process speaks for the session it
	// binds or names, self-reported like every binding without peer_auth
	// ("Honest limits"). Under peer_auth the holder is the process tree
	// (TestPeerAuthMatrix_Grants).
	t.Run("the conversation holds whatever the grantee's agent binds or names", func(t *testing.T) {
		r := r.at(t)
		items, isErr := gmCall(t, cli1, "memory_write", map[string]any{"session": "docs-agent-session", "project": gmDocs,
			"name": "advisory-handle", "kind": "reference", "description": "d", "body": "b\n"})
		require.True(t, isErr, "%v", items)
		require.True(t, strings.HasPrefix(items[0], "memory_write: session docs-agent-session works in project "+gmDocs+
			", and this conversation works in project "+gmHome+": "), items[0])
		rebound := r.dial(r.tcp.URL, gmKey, gmAgent)
		items, isErr = gmCall(t, rebound, "session_start", map[string]any{"project": gmDocs})
		require.True(t, isErr, "no way back: %v", items)
		require.Contains(t, items[0], "nothing was bound: this conversation holds content from "+held)
		got := gmOK(t, rebound, "memory_write", map[string]any{"name": "advisory-rebind", "kind": "reference", "description": "d", "body": "b\n"})
		require.Equal(t, gmHome, got["project"], "nothing was bound: it lands in the home")
		for _, name := range []string{"advisory-handle", "advisory-rebind"} {
			_, found, err := store.MemoryByName(r.ctx, r.db, gmDocs, name)
			require.NoError(t, err)
			require.False(t, found, name)
		}
		// The limit, pinned as it stands.
		gmOK(t, unbound, "memory_write", map[string]any{"session": "docs-agent-session", "project": gmDocs,
			"name": "advisory-limit", "kind": "reference", "description": "d", "body": "b\n"})
		_, found, err := store.MemoryByName(r.ctx, r.db, gmDocs, "advisory-limit")
		require.NoError(t, err)
		require.True(t, found)
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
		status, body = r.hook(r.tcp.URL, gmKey, "session-end", map[string]any{"session_id": "cc/home-1-ext", "cwd": "/work/" + gmHome, "reason": "exit"})
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
