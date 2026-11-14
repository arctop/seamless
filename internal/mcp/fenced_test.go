package mcp_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The MCP edge speaks for the conversation (isolation.requests, plan
// fenced-sessions P4): an identified conversation is judged from its home,
// a binding acts as a session elsewhere only where the home may read and
// write, session_start into a fenced project files the move or grant request
// the home and the project call for (D4), and a refusal at a fenced project
// files the same.

const fsAgent = "5151.77"

// fsOrigin is where a conversation starts: native to its home, or moved into
// it by an approved move.
type fsOrigin struct {
	name string
	// home is the conversation's home once set up; conf the confidential
	// project it is not in.
	home, conf string
	moved      bool
}

var fsOrigins = []fsOrigin{
	{name: "native open", home: gOps, conf: gFin},
	{name: "native confidential", home: gLegal, conf: gFin},
	{name: "moved", home: gFin, conf: gLegal, moved: true},
}

// fsRig is the advisory grant rig with an agent conversation set up for o:
// its connection names its process; content planted in the confidential and
// the sealed project, each with a session another agent may name.
type fsRig struct {
	*grantRig
	cli               *mcpclient.Client
	home              string
	notes             map[string]string
	sessions          map[string]string
	homeName, ambient string
}

func newFSRig(t *testing.T, o fsOrigin) *fsRig {
	t.Helper()
	r := &fsRig{grantRig: newGrantRig(t, true), notes: map[string]string{gFin: ""}, sessions: map[string]string{gFin: "cc/fin"}}
	r.notes[gFin] = r.finNote
	for _, p := range []string{gLegal, gVault} {
		name := "cc/" + p + "-owner"
		cli := r.bind(p, name)
		r.notes[p] = r.ok(cli, "notes_create", map[string]any{"title": p + " notes", "body": p + " planted notes"})["id"].(string)
		r.sessions[p] = name
	}
	ctx := context.Background()
	start := o.home
	if o.moved {
		start = gOps
	}
	r.ambient = seedAgentAmbient(t, ctx, r.db, "cc/fs-agent", start, "/work/"+start, fsAgent)
	r.homeName = "cc/fs-agent"
	r.cli = dialAgent(t, ctx, r.url, fsAgent)
	r.home = start
	if o.moved {
		text := r.refused(r.cli, "session_start", map[string]any{"project": o.home})
		r.approveMove(gShort.FindString(text))
		r.home, r.homeName = o.home, "cc/fs-agent@"+o.home
		// The first result after the move announces it.
		items, _ := r.call(r.cli, "project_list", nil)
		require.Len(t, items, 2, "%v", items)
		require.True(t, strings.HasPrefix(items[1], "This conversation moved into "+o.home+": "), items[1])
	}
	return r
}

// approveMove approves a pending move request as seam approve does without
// peer_auth: the facts' digest, the bearer key's decision.
func (r *grantRig) approveMove(ref string) approval.MoveApproved {
	r.t.Helper()
	ctx := context.Background()
	a, err := r.grants.Resolve(ctx, ref)
	require.NoError(r.t, err)
	require.Equal(r.t, store.AccessKindMove, a.Kind)
	facts, err := r.grants.Facts(ctx, a.ID)
	require.NoError(r.t, err)
	done, err := r.grants.ApproveMove(ctx, approval.Decision{ID: a.ID, Duration: approval.For1h, Digest: facts.Digest,
		Proof: approval.Proof{Kind: approval.ProofKey}})
	require.NoError(r.t, err)
	return done
}

func (r *fsRig) pending() []store.AccessApproval {
	r.t.Helper()
	rows, err := store.PendingAccessApprovals(context.Background(), r.db, r.clock())
	require.NoError(r.t, err)
	return rows
}

// kindOf is the request the kind rule files for target from home.
func kindOf(home, target string) string {
	if target == gVault || home == gOps {
		return store.AccessKindMove
	}
	return store.AccessKindGrant
}

// TestFenced_AdvisoryMatrix: the advisory tier (no peer_auth) x {confidential,
// sealed} x {native open, native confidential, moved} x {rebind, session=,
// refusal}. Each case files exactly the request the conversation's home and
// the target call for, binds nothing, and lets nothing of the target through.
func TestFenced_AdvisoryMatrix(t *testing.T) {
	for _, o := range fsOrigins {
		for _, target := range []string{o.conf, gVault} {
			for _, action := range []string{"rebind", "session=", "refusal"} {
				t.Run(fmt.Sprintf("%s/%s/%s", o.name, target, action), func(t *testing.T) {
					r := newFSRig(t, o)
					before := len(r.pending())
					kind := kindOf(r.home, target)
					state, err := store.IsolationOf(context.Background(), r.db, target)
					require.NoError(t, err)
					var text string
					switch action {
					case "rebind":
						text = r.refused(r.cli, "session_start", map[string]any{"project": target})
						require.True(t, strings.HasPrefix(text, fmt.Sprintf("session_start: nothing was bound: project %s is %s, "+
							"and this conversation works in ", target, state)), text)
					case "session=":
						text = r.refused(r.cli, "notes_read", map[string]any{"id": r.notes[target], "session": r.sessions[target]})
						require.True(t, strings.HasPrefix(text, "notes_read: session "+r.sessions[target]+" works in project "+target+
							", and this conversation works in project "+r.home+": "+target+" is "+string(state)+
							", and only a conversation working in it reads it, so it may not act as that session\n"+
							"Remedy: leave session= off to act as your own session "+r.homeName+" in project "+r.home), text)
						require.Len(t, r.pending(), before, "a handle files nothing")
					case "refusal":
						text = r.refused(r.cli, "notes_read", map[string]any{"id": r.notes[target]})
						rule, _, _ := strings.Cut(text, "\n")
						require.True(t, strings.HasPrefix(rule, "notes_read: project "+target+" is "+string(state)+": only a conversation working in it"), rule)
					}
					require.NotContains(t, text, target+" planted notes", "a refusal never echoes what it withholds")
					if action != "session=" {
						words := map[string]string{store.AccessKindMove: "move request R-", store.AccessKindGrant: "request R-"}[kind]
						require.Contains(t, text, "\nRemedy: ")
						require.Contains(t, text, words)
						pending := r.pending()
						require.Len(t, pending, before+1, "%v", text)
						got := pending[len(pending)-1]
						for _, p := range pending {
							if p.Project == target {
								got = p
							}
						}
						require.Equal(t, kind, got.Kind)
						require.Equal(t, target, got.Project)
						require.Equal(t, r.home, got.FromProject)
						if kind == store.AccessKindMove {
							require.Contains(t, text, "Record what belongs in "+r.home+" now")
						}
					}
					// Nothing was bound: an unscoped write lands in the home.
					mem := r.ok(r.cli, "memory_write", map[string]any{"name": "after-" + strings.ReplaceAll(action, "=", ""),
						"kind": "reference", "description": "d", "body": "b\n"})
					require.Equal(t, r.home, mem["project"])
				})
			}
		}
	}
}

// From a home holding fenced content there is no way back: session_start into
// an open project or the global scope is refused, and so is a session= handle
// naming a session there.
func TestFenced_NoWayBack(t *testing.T) {
	for _, o := range fsOrigins[1:] {
		t.Run(o.name, func(t *testing.T) {
			r := newFSRig(t, o)
			hr := r.bind(gHR, "cc/hr")
			r.ok(hr, "memory_write", map[string]any{"name": "hr-note", "kind": "reference", "description": "d", "body": "b\n"})
			for _, dest := range []string{gHR, "global"} {
				text := r.refused(r.cli, "session_start", map[string]any{"project": dest})
				require.Contains(t, text, "nothing was bound: this conversation holds content from ")
				require.Contains(t, text, "never goes back to an open project or the global scope\nRemedy: keep working in project "+r.home)
			}
			text := r.refused(r.cli, "memory_write", map[string]any{"name": "x", "kind": "reference", "description": "d",
				"body": "b\n", "session": "cc/hr"})
			require.Contains(t, text, "session cc/hr works in project "+gHR+", and this conversation works in project "+r.home)
			require.Empty(t, r.pending(), "nothing to ask for an open project")
		})
	}
}

// Unchanged with the switch on: a native session in its own project, an open
// home with an empty label moving between open projects, and a client that
// sends no agent-process identity.
func TestFenced_SessionStartUnchanged(t *testing.T) {
	r := newFSRig(t, fsOrigins[0])
	own := r.ok(r.cli, "session_start", map[string]any{"project": gOps})
	require.Equal(t, r.ambient, own["session_id"], "its own session, adopted")
	open := r.ok(r.cli, "session_start", map[string]any{"project": gHR})
	require.Equal(t, gHR, open["project"], "open to open with an empty label binds as before")
	mem := r.ok(r.cli, "memory_write", map[string]any{"name": "hr-bound", "kind": "reference", "description": "d", "body": "b\n"})
	require.Equal(t, gHR, mem["project"], "the binding places unscoped calls")
	back := r.ok(r.cli, "session_start", map[string]any{})
	require.Equal(t, r.ambient, back["session_id"])

	// No identity: binds into the confidential project, as before.
	plain := dialClient(t, context.Background(), r.url, testKey)
	start := r.ok(plain, "session_start", map[string]any{"project": gFin, "name": "sess/plain"})
	require.Equal(t, gFin, start["project"])
	items, isErr := r.call(plain, "notes_read", map[string]any{"id": r.finNote})
	require.False(t, isErr, "%v", items)
	require.Empty(t, r.pending())
}

// After an approved move: the first tool result announces it and carries the
// new home's briefing, once -- a daemon restart does not send it again -- and
// a binding made from the old home no longer speaks for the conversation.
func TestFenced_MoveNoticeOnce(t *testing.T) {
	r := newFSRig(t, fsOrigins[0])
	ctx := context.Background()
	open := r.ok(r.cli, "session_start", map[string]any{"project": gHR})
	require.Equal(t, gHR, open["project"])
	text := r.refused(r.cli, "notes_read", map[string]any{"id": r.finNote})
	require.Contains(t, text, "\nRemedy: move request R-")
	require.Contains(t, text, "Record what belongs in example-ops now: once the owner approves the move, this conversation "+
		"works in example-finance for good, and it can no longer write to example-ops or the global scope until the "+
		"conversation ends. After the move it can still close a task it claimed there, with a receipt: tasks_update with "+
		"status done or dropped, and no text. Its first result after the approval says so and carries example-finance's "+
		"briefing.")
	done := r.approveMove(gShort.FindString(text))
	require.Equal(t, gFin, done.Child.ProjectSlug)

	// The first call: refused for the stale binding, and carrying the notice.
	items, isErr := r.call(r.cli, "memory_write", map[string]any{"name": "stale", "kind": "reference", "description": "d", "body": "b\n"})
	require.True(t, isErr)
	require.Len(t, items, 2, "%v", items)
	require.True(t, strings.HasPrefix(items[0], "memory_write: this connection is bound to session "), items[0])
	require.Contains(t, items[0], "\nRemedy: call session_start with no arguments to hand this connection back to your own session "+
		done.Child.Name+" in project "+gFin)
	require.True(t, strings.HasPrefix(items[1], "This conversation moved into example-finance: the owner approved move request "+
		approval.ShortID(done.Move.ApprovalID)+". example-finance is its home from now on -- its record goes there, and calls "+
		"that name no project work there -- and it can no longer write to project example-ops or the global scope until the "+
		"conversation ends. It can still close a task its session cc/fs-agent claimed before the move, with a receipt: "+
		"tasks_update with status done or dropped and no text, optionally with reference=example-finance:<id> naming one "+
		"item in example-finance. This is its briefing, sent once:\n"), items[1])
	require.Contains(t, items[1], "ledger-close-rule", "the new home's briefing")

	// Handed back, it works in its new home; the notice is not repeated.
	back := r.ok(r.cli, "session_start", map[string]any{})
	require.Equal(t, done.Child.ID, back["session_id"])
	items, isErr = r.call(r.cli, "notes_read", map[string]any{"id": r.finNote})
	require.False(t, isErr, "%v", items)
	require.Len(t, items, 1, "announced once")
	require.Contains(t, items[0], "quarterly ledger reconciliation notes")

	// A restart: a new server over the same database sends nothing again.
	restarted := r.restart(t)
	cli := dialAgent(t, ctx, restarted, fsAgent)
	items, isErr = r.call(cli, "notes_read", map[string]any{"id": r.finNote})
	require.False(t, isErr, "%v", items)
	require.Len(t, items, 1, "the claim is the move row's: %v", items)

	// session_end: its findings go to the new home.
	end := r.ok(cli, "session_end", map[string]any{"findings": "reconciled the ledger"})
	require.Nil(t, end["findings_withheld"])
	child, ok, err := store.SessionByID(ctx, r.db, done.Child.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionCompleted, child.Status)
	require.Equal(t, "reconciled the ledger", child.Findings)
	left, _, err := store.SessionByID(ctx, r.db, done.Left.ID)
	require.NoError(t, err)
	require.NotContains(t, left.Findings, "reconciled", "the old home keeps only the pointer")
}

// Write answers and recall, judged from the home: a write into a project the
// home may read answers in full; one into a project it may not read is
// trimmed (withholdContent); recall's scope is the home plus its live grant
// targets.
func TestFenced_WriteAnswersAndRecall(t *testing.T) {
	r := newFSRig(t, fsOrigins[0])
	fin := r.bind(gFin, "cc/fin-tasks")
	finTask := r.ok(fin, "tasks_add", map[string]any{"title": "reconcile", "body": "the finance team's task body"})["id"].(string)
	task := r.ok(r.cli, "tasks_update", map[string]any{"id": finTask, "status": "in_progress"})
	require.NotNil(t, task["withheld"], "an open home writes into a confidential project and reads nothing back: %v", task)
	require.Nil(t, task["body"])

	c := newFSRig(t, fsOrigins[1])
	text := c.refused(c.cli, "recall", map[string]any{"query": "ledger reconciliation", "project": gFin})
	short := gShort.FindString(text)
	require.NotEmpty(t, short, text)
	c.approveKey(short, approval.For1h)
	hits := c.ok(c.cli, "recall", map[string]any{"query": "ledger reconciliation"})["hits"].([]any)
	require.NotEmpty(t, hits, "the granted target joins the home's recall scope")
	projects := map[string]bool{}
	for _, h := range hits {
		projects[h.(map[string]any)["project"].(string)] = true
	}
	require.True(t, projects[gFin], "%v", projects)

	// A read-and-write grant: the home writes into its target and reads the
	// answer in full.
	w := newFSRig(t, fsOrigins[1])
	text = w.refused(w.cli, "tasks_add", map[string]any{"project": gFin, "title": "x"})
	require.Contains(t, text, "\nRemedy: read-and-write request R-")
	a, err := w.grants.Resolve(context.Background(), gShort.FindString(text))
	require.NoError(t, err)
	require.Equal(t, store.GrantAccessWrite, a.RequestedAccess)
	facts, err := w.grants.Facts(context.Background(), a.ID)
	require.NoError(t, err)
	_, err = w.grants.Approve(context.Background(), approval.Decision{ID: a.ID, Duration: approval.For1h, Digest: facts.Digest,
		Proof: approval.Proof{Kind: approval.ProofKey}, Access: "read-write"})
	require.NoError(t, err)
	got := w.ok(w.cli, "tasks_add", map[string]any{"project": gFin, "title": "reconcile", "body": "the write grant's task body"})
	require.Nil(t, got["withheld"], "%v", got)
	require.Equal(t, "the write grant's task body", got["body"])
	wfin := w.bind(gFin, "cc/fin-tasks")
	wTask := w.ok(wfin, "tasks_add", map[string]any{"title": "audit", "body": "the finance team's audit body"})["id"].(string)
	upd := w.ok(w.cli, "tasks_update", map[string]any{"id": wTask, "status": "in_progress"})
	require.Nil(t, upd["withheld"], "a read-and-write grant reads what it writes: %v", upd)
	require.Equal(t, "the finance team's audit body", upd["body"])
}

// The session_end of an open home that holds fenced content (a grant from
// before migration 039 planted in the open home) still ends the session and
// its grants; its findings are not recorded.
func TestFenced_SessionEndUnrecorded(t *testing.T) {
	r := newFSRig(t, fsOrigins[0])
	ctx := context.Background()
	now := r.clock()
	a, err := store.CreateGrantRequest(ctx, r.db, store.GrantRequest{Project: gFin, From: gOps, Lease: time.Hour, TTL: time.Minute,
		SessionID: r.ambient, ConversationClient: "claude-code", ConversationID: "cc/fs-agent-full"}, now)
	require.NoError(t, err)
	g, err := store.ApproveAccessGrant(ctx, r.db, a.ID, a.Challenge, "bearer-key", time.Hour, now)
	require.NoError(t, err)
	end := r.ok(r.cli, "session_end", map[string]any{"findings": "copied the ledger totals", "mishaps": []any{"read it twice"}})
	require.Equal(t, "Not recorded: this session holds content from example-finance, and its findings may not be written "+
		"into project example-ops.", end["findings_withheld"])
	require.EqualValues(t, 1, end["mishaps_withheld"])
	ended, err := store.AccessGrantByID(ctx, r.db, g.ID)
	require.NoError(t, err)
	require.Equal(t, store.LeaseRevokedSessionEnded, ended.RevokedReason)
	sess, _, err := store.SessionByID(ctx, r.db, r.ambient)
	require.NoError(t, err)
	require.Equal(t, core.SessionCompleted, sess.Status)
	require.NotContains(t, sess.Findings, "ledger")
	require.Equal(t, 0, r.count(`SELECT COUNT(*) FROM events WHERE kind = 'agent.mishap'`))
	require.Zero(t, r.count(`SELECT COUNT(*) FROM events WHERE payload LIKE '%ledger%' AND project_slug = ?`, gOps),
		"nothing the call carried lands in the open project's record, its tool.call event included")
	require.Equal(t, 1, r.count(`SELECT COUNT(*) FROM events WHERE kind = 'tool.call' AND project_slug = ?
		AND json_extract(payload, '$.tool') = 'session_end' AND json_extract(payload, '$.withheld') = 1`, gOps))
}

// A client that names no agent process but binds by name to a conversation's
// own row asks as that conversation (approval.Grants.RequestGrant): from an
// open home its refused read files a move, and its remedy says so.
func TestFenced_BoundByNameAsksAsTheConversation(t *testing.T) {
	r := newFSRig(t, fsOrigins[0])
	plain := dialClient(t, context.Background(), r.url, testKey)
	r.ok(plain, "session_start", map[string]any{"name": "cc/fs-agent"})
	text := r.refused(plain, "notes_read", map[string]any{"id": r.finNote})
	require.True(t, strings.HasPrefix(text, "notes_read: "+offRefusal+"\nRemedy: move request R-"), text)
	pending := r.pending()
	require.Len(t, pending, 1)
	require.Equal(t, store.AccessKindMove, pending[0].Kind)
	require.Equal(t, r.ambient, pending[0].SessionID)
}
