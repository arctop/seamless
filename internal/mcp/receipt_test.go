package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// Receipts (isolation.requests, owner decision D5; plan fenced-sessions P8):
// an orchestrator in an open project claims a step, the owner moves it into a
// fenced project for good, and it closes the step with status done or
// dropped -- nothing else, at most one reference to an item in its new home
// -- although the clearance rule refuses every other write into the project
// it left.

// receiptAgent is the agent process the receipt conversation names.
const receiptAgent = "6161.88"

// receiptRig is the advisory grant rig (isolation.requests on, or off) with
// an orchestrator conversation in example-ops: the ambient row the hook left,
// and a connection naming its agent process.
type receiptRig struct {
	*grantRig
	cli     *mcpclient.Client
	ambient string
}

func receiptRigFor(t *testing.T, on bool) *receiptRig {
	t.Helper()
	r := &receiptRig{grantRig: newGrantRig(t, on)}
	ctx := context.Background()
	r.ambient = seedAgentAmbient(t, ctx, r.db, "cc/receipt-agent", gOps, "/work/"+gOps, receiptAgent)
	r.cli = dialAgent(t, ctx, r.url, receiptAgent)
	return r
}

// receiptStep adds a step to project and claims it as the conversation's own
// session.
func (r *receiptRig) receiptStep(project, title string) string {
	r.t.Helper()
	id := r.ok(r.cli, "tasks_add", map[string]any{"project": project, "title": title, "body": title + ": the plan"})["id"].(string)
	claimed := r.ok(r.cli, "tasks_claim", map[string]any{"id": id})
	require.Equal(r.t, r.ambient, claimed["claimed_by"])
	return id
}

// receiptMove has the owner move the conversation into target, and reads the
// first result after it, which announces the move.
func (r *receiptRig) receiptMove(target string) approval.MoveApproved {
	r.t.Helper()
	text := r.refused(r.cli, "session_start", map[string]any{"project": target})
	done := r.approveMove(gShort.FindString(text))
	require.Equal(r.t, r.ambient, done.Left.ID)
	items, isErr := r.call(r.cli, "project_list", nil)
	require.False(r.t, isErr, "%v", items)
	require.Len(r.t, items, 2, "%v", items)
	require.True(r.t, strings.HasPrefix(items[1], "This conversation moved into "+target+": "), items[1])
	return done
}

func (r *receiptRig) receiptTask(id string) core.Task {
	r.t.Helper()
	task, err := store.TaskByID(context.Background(), r.db, id)
	require.NoError(r.t, err)
	return task
}

// receiptTransitions are a task's task.transition records, oldest first.
func (r *receiptRig) receiptTransitions(id string) []map[string]any {
	r.t.Helper()
	var out []map[string]any
	for _, e := range eventsOf(r.t, r.db, core.EventTaskTransition) {
		if e["_item"] == id {
			out = append(out, e)
		}
	}
	return out
}

// receiptClearanceRefusal is the clearance rule's refusal of a write into
// example-ops from a conversation moved into example-finance.
const receiptClearanceRefusal = "this session holds content from example-finance, so it writes only into example-finance: " +
	"a write into project example-ops is refused"

// The typical case: the step claimed before the move closes, done or
// dropped, its claim released; the task's history records the receipt and
// its reference; nothing else of the move's home reaches example-ops, and
// every other write there is refused as before.
func TestReceipt_ClosesAStepClaimedBeforeTheMove(t *testing.T) {
	r := receiptRigFor(t, true)
	build := r.receiptStep(gOps, "build step")
	cleanup := r.receiptStep(gOps, "cleanup step")
	moved := r.receiptMove(gFin)
	results := r.ok(r.cli, "notes_create", map[string]any{"title": "build results", "body": "the quarterly ledger build passed"})
	require.Equal(t, gFin, results["project"], "the note is written in the new home")
	note := results["id"].(string)

	// Everything but a receipt is refused as before: a title, or no claim of
	// its own to close.
	require.Equal(t, "tasks_update: "+receiptClearanceRefusal,
		r.refused(r.cli, "tasks_update", map[string]any{"id": build, "title": "renamed"}))
	require.Equal(t, "tasks_add: "+receiptClearanceRefusal,
		r.refused(r.cli, "tasks_add", map[string]any{"project": gOps, "title": "one more step"}))

	// Dropped, with no reference: the plain call is the receipt.
	dropped := r.ok(r.cli, "tasks_update", map[string]any{"id": cleanup, "status": "dropped"})
	require.Equal(t, "dropped", dropped["status"])
	require.Equal(t, true, dropped["receipt"])
	require.NotContains(t, dropped, "reference")
	got := r.receiptTransitions(cleanup)
	require.NotContains(t, got[len(got)-1], "reference")
	require.Equal(t, true, got[len(got)-1]["receipt"])
	require.Equal(t, core.TaskDropped, r.receiptTask(cleanup).Status)
	require.Empty(t, r.receiptTask(cleanup).ClaimedBy)

	// Done, with a reference to the note that holds the results.
	out := r.ok(r.cli, "tasks_update", map[string]any{"id": build, "status": "done", "reference": gFin + ":" + note})
	require.Equal(t, "done", out["status"])
	require.Equal(t, true, out["receipt"])
	require.Equal(t, gFin+":"+note, out["reference"])
	require.Equal(t, "build step", out["title"], "the home reads example-ops: the answer is in full")
	require.NotContains(t, out, "claimed_by")

	task := r.receiptTask(build)
	require.Equal(t, core.TaskDone, task.Status)
	require.Empty(t, task.ClaimedBy, "the claim is released")
	require.Nil(t, task.LeaseExpiresAt)
	require.NotNil(t, task.ClosedAt)
	require.Equal(t, "build step", task.Title)
	require.Equal(t, "build step: the plan", task.Body)

	history := r.receiptTransitions(build)
	last := history[len(history)-1]
	require.Equal(t, r.ambient, last["_session"], "filed under the session whose claim it closed")
	require.Equal(t, gOps, last["_project"])
	require.Equal(t, "done", last["to"])
	require.Equal(t, "in_progress", last["from"])
	require.Equal(t, true, last["receipt"])
	require.Equal(t, gFin+":"+note, last["reference"])
	require.NotContains(t, last["_raw"], "ledger", "the reference is an id, never the home's text")
	require.NotContains(t, last["_raw"], moved.Child.ID, "nor the session the conversation works in now")

	// A receipt opens nothing else: the next write into example-ops is still
	// refused, and closed, the step is no receipt's to close again.
	require.Equal(t, "tasks_add: "+receiptClearanceRefusal,
		r.refused(r.cli, "tasks_add", map[string]any{"project": gOps, "title": "one more step"}))
	require.Equal(t, "tasks_update: "+receiptClearanceRefusal,
		r.refused(r.cli, "tasks_update", map[string]any{"id": build, "status": "dropped"}))
	require.Zero(t, r.count(`SELECT COUNT(*) FROM events WHERE project_slug = ? AND payload LIKE '%ledger%'`, gOps))
}

// A step the pre-move session claimed in what is now the conversation's home
// closes too: the claim is the pre-move session's, which the conversation's
// new session could not otherwise close while its lease lives.
func TestReceipt_AStepInTheNewHome(t *testing.T) {
	r := receiptRigFor(t, true)
	step := r.receiptStep(gFin, "reconcile")
	r.receiptMove(gFin)
	out := r.ok(r.cli, "tasks_update", map[string]any{"id": step, "status": "done"})
	require.Equal(t, "done", out["status"])
	require.Equal(t, true, out["receipt"])
	require.Empty(t, r.receiptTask(step).ClaimedBy)
}

// A task someone else claimed is no receipt's to close, whether it was theirs
// all along or they took it after the pre-move session's lease lapsed: the
// update is refused as before, and naming a reference makes it a receipt the
// rule refuses.
func TestReceipt_ATaskClaimedBySomeoneElse(t *testing.T) {
	r := receiptRigFor(t, true)
	ctx := context.Background()
	worker := r.bind(gOps, "cc/ops-worker")
	theirs := r.ok(worker, "tasks_add", map[string]any{"title": "their step"})["id"].(string)
	held := r.ok(worker, "tasks_claim", map[string]any{"id": theirs})["claimed_by"].(string)
	taken := r.receiptStep(gOps, "taken over")
	done := r.receiptMove(gFin)
	// The pre-move session's lease lapses and another session takes the step.
	_, err := store.ClaimTask(ctx, r.db, taken, held, time.Hour, time.Now().UTC().Add(time.Hour))
	require.NoError(t, err)

	for _, id := range []string{theirs, taken} {
		require.Equal(t, "tasks_update: "+receiptClearanceRefusal,
			r.refused(r.cli, "tasks_update", map[string]any{"id": id, "status": "done"}))
		text := r.refused(r.cli, "tasks_update", map[string]any{"id": id, "status": "done", "reference": gFin + ":" + done.Child.ID})
		require.Equal(t, "tasks_update: store.JudgeReceipt: receipt refused: a receipt closes only a task this conversation "+
			"claimed before it moved", text)
		task := r.receiptTask(id)
		require.Equal(t, core.TaskInProgress, task.Status)
		require.Equal(t, held, task.ClaimedBy, "the claim stays theirs")
	}
}

// A receipt carries no text: any other field is refused with the rule, the
// value never echoed and nothing written; so is a reference of any other
// shape, outside the home, or naming no item there, and a status that does
// not close the step.
func TestReceipt_ATextFieldIsRefused(t *testing.T) {
	r := receiptRigFor(t, true)
	ctx := context.Background()
	step := r.receiptStep(gOps, "build step")
	blocker := r.ok(r.cli, "tasks_add", map[string]any{"project": gOps, "title": "a blocker"})["id"].(string)
	done := r.receiptMove(gFin)
	home := gFin + ":" + done.Child.ID
	rule := "tasks_update: store.JudgeReceipt: receipt refused: a receipt carries no text: it sets the status alone, with at " +
		"most one reference to an item in the conversation's home, example-finance, so "

	for field, value := range map[string]any{
		"title":             "built: the quarter's ledger totals",
		"body":              "the quarter's ledger totals",
		"content":           "the quarter's ledger totals",
		"project":           gHR,
		"add_depends_on":    []any{blocker},
		"remove_depends_on": []any{blocker},
	} {
		text := r.refused(r.cli, "tasks_update", map[string]any{"id": step, "status": "done", field: value, "reference": home})
		name := field
		if field == "content" {
			name = "body"
		}
		require.Equal(t, rule+name+" is refused", text, field)
		require.NotContains(t, text, "ledger", "the value is never echoed")
		// The same without a reference: closing a step the pre-move session
		// claimed is a receipt either way.
		require.Equal(t, rule+name+" is refused",
			r.refused(r.cli, "tasks_update", map[string]any{"id": step, "status": "dropped", field: value}), field)
	}
	require.Equal(t, rule+"title, body are refused", r.refused(r.cli, "tasks_update",
		map[string]any{"id": step, "status": "done", "title": "t", "body": "b"}))

	prefix := "tasks_update: store.JudgeReceipt: receipt refused: "
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"id": step, "reference": home}, prefix + `a receipt sets status done or dropped, not ""`},
		{map[string]any{"id": step, "status": "open", "reference": home}, prefix + `a receipt sets status done or dropped, not "open"`},
		{map[string]any{"id": step, "status": "done", "reference": gOps + ":" + step}, prefix +
			"a receipt's reference names an item in the conversation's home, example-finance"},
		{map[string]any{"id": step, "status": "done", "reference": gFin + ":" + step}, prefix +
			"a receipt's reference names an item in the conversation's home, example-finance, and " + step + " names none there"},
		{map[string]any{"id": step, "status": "done", "reference": gFin + ":finished the review"}, prefix +
			"a receipt's reference is one item id"},
		{map[string]any{"id": step, "status": "done", "reference": "the quarter's ledger totals"}, "tasks_update: " +
			"store.ParseReceiptRef: receipt refused: a receipt's reference is one item in the conversation's home, written " +
			"<project>:<id> -- the home's slug and the item's id"},
		{map[string]any{"id": step, "status": "done", "reference": ""}, "tasks_update: store.ParseReceiptRef: receipt " +
			"refused: a receipt's reference is one item in the conversation's home, written <project>:<id> -- the home's " +
			"slug and the item's id"},
	} {
		require.Equal(t, tc.want, r.refused(r.cli, "tasks_update", tc.args), "%v", tc.args)
	}
	// Not closing it and naming no reference is no receipt: the update is the
	// clearance rule's to refuse, as before.
	require.Equal(t, "tasks_update: "+receiptClearanceRefusal,
		r.refused(r.cli, "tasks_update", map[string]any{"id": step, "status": "open"}))

	task := r.receiptTask(step)
	require.Equal(t, core.TaskInProgress, task.Status, "nothing was written")
	require.Equal(t, r.ambient, task.ClaimedBy)
	require.Equal(t, "build step", task.Title)
	deps, err := store.TaskByID(ctx, r.db, step)
	require.NoError(t, err)
	require.Empty(t, deps.DependsOn)
	require.Len(t, r.receiptTransitions(step), 2, "created and claimed, nothing since")
}

// A conversation whose home is sealed sends no receipts: the step its
// pre-move session claimed stays as it was.
func TestReceipt_ASealedHomeSendsNone(t *testing.T) {
	r := receiptRigFor(t, true)
	step := r.receiptStep(gOps, "build step")
	done := r.receiptMove(gVault)
	want := "tasks_update: store.JudgeReceipt: receipt refused: a conversation whose home is sealed project example-vault " +
		"sends no receipts"
	require.Equal(t, want, r.refused(r.cli, "tasks_update", map[string]any{"id": step, "status": "done"}))
	require.Equal(t, want, r.refused(r.cli, "tasks_update", map[string]any{"id": step, "status": "dropped",
		"reference": gVault + ":" + done.Child.ID}))
	require.Equal(t, want, r.refused(r.cli, "tasks_update", map[string]any{"id": step, "status": "done", "body": "b"}),
		"the seal is the first word")
	task := r.receiptTask(step)
	require.Equal(t, core.TaskInProgress, task.Status)
	require.Equal(t, r.ambient, task.ClaimedBy)
}

// With isolation.requests off there are no receipts: tasks_update lists no
// reference and refuses one as the unknown parameter it always was, and
// closing a claimed task is the ordinary update it always was.
func TestReceipt_OffIsThePreviousBehavior(t *testing.T) {
	r := receiptRigFor(t, false)
	ctx := context.Background()
	schema := listedSchema(t, ctx, r.cli, "tasks_update")
	require.NotContains(t, schema.Properties, "reference")
	require.Contains(t, catalogTool(t, "tasks_update").InputSchema.Properties, "reference", "the catalog documents it")

	step := r.receiptStep(gOps, "build step")
	text := r.refused(r.cli, "tasks_update", map[string]any{"id": step, "status": "done", "reference": gOps + ":" + step})
	require.Equal(t, `tasks_update: unknown parameter "reference": valid parameters are: `+
		"add_depends_on, body, content, id, project, remove_depends_on, session, session_id, status, text, title", text)
	out := r.ok(r.cli, "tasks_update", map[string]any{"id": step, "status": "done"})
	require.Equal(t, "done", out["status"])
	require.NotContains(t, out, "receipt")
	history := r.receiptTransitions(step)
	last := history[len(history)-1]
	require.NotContains(t, last, "receipt")
	require.Equal(t, r.ambient, last["_session"])
	require.Zero(t, r.count(`SELECT COUNT(*) FROM access_moves`))
	require.Zero(t, r.count(`SELECT COUNT(*) FROM access_approvals`))
}

// A refused receipt into a project the clearance rule refuses is the write
// across the fence it would have been without receipts: write screening
// records it as such. A receipt sent is no refusal.
func TestReceipt_ARefusedReceiptIsAFenceWrite(t *testing.T) {
	r := receiptRigFor(t, true)
	ctx := context.Background()
	r.cfg.Screening = screening.New(screening.Config{DB: r.db, Files: r.cfg.Files, Events: r.rec,
		Enabled: func(context.Context) bool { return true }})
	r.cfg.Features.Screening = true
	r.url = r.restart(t)
	r.cli = dialAgent(t, ctx, r.url, receiptAgent)
	step := r.receiptStep(gOps, "build step")
	r.receiptMove(gFin)

	text := r.refused(r.cli, "tasks_update", map[string]any{"id": step, "status": "done", "body": "the quarter's ledger totals"})
	require.Contains(t, text, "receipt refused: a receipt carries no text")
	refused := eventsOf(t, r.db, core.EventAccessRefused)
	require.Len(t, refused, 1, "%v", refused)
	require.Equal(t, screening.RefusedFenceWrite, refused[0]["reason"])
	require.Equal(t, "tasks_update", refused[0]["tool"])
	require.Equal(t, gOps, refused[0]["target"])
	require.NotContains(t, refused[0]["_raw"], "ledger")

	r.ok(r.cli, "tasks_update", map[string]any{"id": step, "status": "done"})
	require.Len(t, eventsOf(t, r.db, core.EventAccessRefused), 1, "a receipt sent is no refusal")
}

// Under peer_auth the conversation is the agent's process tree: its move is
// the tree's, and the step the tree's pre-move session claimed closes the
// same way, the claim released.
func TestReceipt_PeerAuthTree(t *testing.T) {
	r := newFPRig(t, "native open", nil)
	ctx := context.Background()
	out, _ := r.ok(r.agent, "tasks_add", map[string]any{"project": lkOps, "title": "build step"})
	var added map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &added), out)
	step := added["id"].(string)
	r.ok(r.agent, "tasks_claim", map[string]any{"id": step})
	before, err := store.TaskByID(ctx, r.db, step)
	require.NoError(t, err)
	require.NotEmpty(t, before.ClaimedBy)

	text := r.refused(r.agent, "session_start", map[string]any{"project": dlgLegal})
	done := r.approveMove(gShort.FindString(text), approval.For1h)
	require.Equal(t, before.ClaimedBy, done.Left.ID)
	items, _ := r.call(r.agent, "project_list", nil)
	require.Len(t, items, 2, "%v", items)

	require.Equal(t, "tasks_update: this agent holds content from example-legal, so it writes only into example-legal: "+
		"a write into project example-ops is refused", r.refused(r.agent, "tasks_update", map[string]any{"id": step, "title": "x"}))
	out, _ = r.ok(r.agent, "tasks_update", map[string]any{"id": step, "status": "done", "reference": dlgLegal + ":" + done.Child.ID})
	var closed map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &closed), out)
	require.Equal(t, true, closed["receipt"])
	require.Equal(t, "done", closed["status"])
	after, err := store.TaskByID(ctx, r.db, step)
	require.NoError(t, err)
	require.Equal(t, core.TaskDone, after.Status)
	require.Empty(t, after.ClaimedBy)
}
