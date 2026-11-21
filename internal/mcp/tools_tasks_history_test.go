package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// A task's body history at the tool surface: every body tasks_update replaces,
// or appends to with body_append, is kept as a revision and read back with
// tasks_list id=<id> body_revisions=true -- exactly where the task's own body
// is read, and nowhere else.

// hrAppendedLine is the line an append adds, as notes_append adds one: a
// blank line, the UTC minute, " -- " and the text.
const hrAppendedLine = `\n\n\d{4}-\d{2}-\d{2} \d{2}:\d{2} -- `

// hrBothRefusal is tasks_update's refusal of body and body_append together.
const hrBothRefusal = "tasks_update: body (or content, text) and body_append in one call: body replaces the whole body " +
	"and body_append adds a line to it; pass one of them"

// hrRevisions reads a task's earlier bodies through cli, newest first.
func hrRevisions(t *testing.T, ctx context.Context, cli *mcpclient.Client, id string) []map[string]any {
	t.Helper()
	out := callJSON(t, ctx, cli, "tasks_list", map[string]any{"id": id, "body_revisions": true})
	tasks := out["tasks"].([]any)
	require.Len(t, tasks, 1)
	raw, ok := tasks[0].(map[string]any)["body_revisions"].([]any)
	require.True(t, ok, "asked for, the revisions are there, an empty list included: %v", out)
	revs := make([]map[string]any, len(raw))
	for i, r := range raw {
		revs[i] = r.(map[string]any)
	}
	return revs
}

// hrBodies is each revision's body, in order.
func hrBodies(revs []map[string]any) []any {
	out := make([]any, len(revs))
	for i, r := range revs {
		out[i] = r["body"]
	}
	return out
}

// hrTaskBody is the task's stored body.
func hrTaskBody(t *testing.T, ctx context.Context, cli *mcpclient.Client, id string) string {
	t.Helper()
	out := callJSON(t, ctx, cli, "tasks_list", map[string]any{"id": id})
	body, _ := out["tasks"].([]any)[0].(map[string]any)["body"].(string)
	return body
}

// A body tasks_update replaces is read back, with when, how and by which
// session; the write's own answer reads as it always did.
func TestTasksHistory_AReplacedBodyIsReadBack(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	session := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})["session_id"].(string)
	id := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "ship the rollout", "body": "the acceptance criteria"})["id"].(string)
	require.Empty(t, hrRevisions(t, ctx, cli, id), "a task never changed has none")

	before := time.Now().UTC().Add(-time.Second)
	updated := callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": id, "body": "only the findings"})
	require.Equal(t, "only the findings", updated["body"])
	require.NotContains(t, updated, "body_revisions", "a write answers as it always did")

	revs := hrRevisions(t, ctx, cli, id)
	require.Len(t, revs, 1)
	at, err := core.ParseTime(revs[0]["replaced_at"].(string))
	require.NoError(t, err)
	require.False(t, at.Before(before), "replaced_at is when the change landed")
	require.Equal(t, map[string]any{"revision": float64(1), "body": "the acceptance criteria", "change": "replace",
		"replaced_at": revs[0]["replaced_at"], "replaced_by": session}, revs[0])

	callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": id, "text": "the third body"})
	require.Equal(t, []any{"only the findings", "the acceptance criteria"}, hrBodies(hrRevisions(t, ctx, cli, id)),
		"newest first; body's aliases replace it the same way")
}

// tasks_list answers exactly as before unless body_revisions=true is passed:
// false is the default, and true adds the one key to the task and nothing else.
// Without id it is refused, since it reads one task's history.
func TestTasksHistory_TheDefaultAnswerIsUnchanged(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	id := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "the deploy", "body": "stage it"})["id"].(string)
	callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": id, "body": "staged"})

	text := func(args map[string]any) string {
		t.Helper()
		isErr, txt := callErr(t, ctx, cli, "tasks_list", args)
		require.False(t, isErr, txt)
		return txt
	}
	plain := text(map[string]any{"id": id})
	require.NotContains(t, plain, "body_revisions")
	require.Equal(t, plain, text(map[string]any{"id": id, "body_revisions": false}))

	var on map[string]any
	require.NoError(t, json.Unmarshal([]byte(text(map[string]any{"id": id, "body_revisions": true})), &on))
	task := on["tasks"].([]any)[0].(map[string]any)
	require.Len(t, task["body_revisions"], 1)
	delete(task, "body_revisions")
	require.Equal(t, plain, mustJSON(t, on), "the switch adds its one key and nothing else")

	listed := text(map[string]any{})
	require.NotContains(t, listed, "body_revisions")
	require.Equal(t, listed, text(map[string]any{"body_revisions": false}))
	isErr, txt := callErr(t, ctx, cli, "tasks_list", map[string]any{"body_revisions": true})
	require.True(t, isErr)
	require.Equal(t, "tasks_list: body_revisions reads one task's earlier bodies: pass it with id=<task id>", txt)
}

// body_append adds notes_append's line to the body, keeps the body before it
// as a revision, and answers the new body; an empty body takes the line alone
// and keeps nothing.
func TestTasksHistory_AnAppendAddsALineAndKeepsTheBodyBefore(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	id := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "chase the race", "body": "the plan"})["id"].(string)

	out := callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": id, "body_append": "found the race"})
	require.Regexp(t, `\Athe plan`+hrAppendedLine+`found the race\n\z`, out["body"])
	revs := hrRevisions(t, ctx, cli, id)
	require.Len(t, revs, 1)
	require.Equal(t, "the plan", revs[0]["body"])
	require.Equal(t, "append", revs[0]["change"])

	out = callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": id, "body_append": "fixed it", "status": "done"})
	require.Equal(t, "done", out["status"], "an append rides with the other fields of one update")
	require.Regexp(t, `\Athe plan`+hrAppendedLine+`found the race`+hrAppendedLine+`fixed it\n\z`, out["body"])
	require.Len(t, hrRevisions(t, ctx, cli, id), 2)

	// The line notes_append adds to a note, so an agent appends to either the
	// same way.
	note := callJSON(t, ctx, cli, "notes_create", map[string]any{"title": "race log", "body": "the plan"})["id"].(string)
	callJSON(t, ctx, cli, "notes_append", map[string]any{"id": note, "body": "found the race"})
	read := callJSON(t, ctx, cli, "notes_read", map[string]any{"id": note})
	require.Regexp(t, `\Athe plan`+hrAppendedLine+`found the race\n\z`, read["body"])

	empty := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "no body yet"})["id"].(string)
	out = callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": empty, "body_append": "the first line"})
	require.Regexp(t, `\A\d{4}-\d{2}-\d{2} \d{2}:\d{2} -- the first line\n\z`, out["body"])
	require.Empty(t, hrRevisions(t, ctx, cli, empty), "an empty body keeps nothing")
}

// body and body_append are one or the other: together they are refused naming
// both, whichever of body's names carried it, and a blank append is refused;
// neither writes anything.
func TestTasksHistory_BodyAndAppendAreOneOrTheOther(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	id := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "keep me", "body": "the plan"})["id"].(string)

	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"id": id, "body": "x", "body_append": "y"}, hrBothRefusal},
		{map[string]any{"id": id, "content": "x", "body_append": "y"}, hrBothRefusal},
		{map[string]any{"id": id, "text": "x", "body_append": "y", "status": "done"}, hrBothRefusal},
		{map[string]any{"id": id, "body_append": " \n\t"}, "tasks_update: body_append must not be blank: pass the text to add to the body"},
		{map[string]any{"id": id, "body_append": "", "status": "done"}, "tasks_update: body_append must not be blank: pass the text to add to the body"},
	} {
		isErr, txt := callErr(t, ctx, cli, "tasks_update", tc.args)
		require.True(t, isErr, "%v", tc.args)
		require.Equal(t, tc.want, txt, "%v", tc.args)
	}
	task := callJSON(t, ctx, cli, "tasks_list", map[string]any{"id": id})["tasks"].([]any)[0].(map[string]any)
	require.Equal(t, "the plan", task["body"])
	require.Equal(t, "open", task["status"])
	require.Empty(t, hrRevisions(t, ctx, cli, id))
}

// Appends from several agents at once all land, each with its revision: the
// store reads the body inside each update's transaction.
func TestTasksHistory_ConcurrentAppendsLoseNothing(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	seed := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, seed, "session_start", map[string]any{"cwd": "/work/demo", "name": "hr-append-seed"})
	id := callJSON(t, ctx, seed, "tasks_add", map[string]any{"title": "investigation", "body": "base line"})["id"].(string)

	const appenders = 8
	clients := make([]*mcpclient.Client, appenders)
	for i := range clients {
		clients[i] = dialClient(t, ctx, url, testKey)
		callJSON(t, ctx, clients[i], "session_start", map[string]any{"cwd": "/work/demo", "name": fmt.Sprintf("hr-append-%02d", i)})
	}
	start := make(chan struct{})
	errs := make([]error, appenders)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = callToolAsync(ctx, clients[i], "tasks_update", map[string]any{
				"id": id, "body_append": fmt.Sprintf("finding-%02d", i),
			})
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "appender %d", i)
	}

	body := hrTaskBody(t, ctx, seed, id)
	require.True(t, strings.HasPrefix(body, "base line\n\n"), body)
	for i := range appenders {
		require.Equal(t, 1, strings.Count(body, fmt.Sprintf("finding-%02d", i)),
			"append %d was overwritten by a concurrent one -- the lost update is back: %s", i, body)
	}
	revs := hrRevisions(t, ctx, seed, id)
	require.Len(t, revs, appenders, "each append kept the body it extended")
	for i, r := range revs {
		require.Equal(t, float64(appenders-i), r["revision"])
	}
}

// Only a change of the stored body keeps a revision: a status, title, gate,
// dependency or project change keeps none, nor does the same body sent again.
func TestTasksHistory_NoRevisionWithoutABodyChange(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	_, err := store.EnsureProject(ctx, db, "other", "other")
	require.NoError(t, err)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	id := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "a step", "body": "the plan"})["id"].(string)
	blocker := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "a blocker"})["id"].(string)
	for _, args := range []map[string]any{
		{"gate_who": "owner", "gate_what": "sign off the rollout"},
		{"gate_clear": true, "gate_note": "signed off"},
		{"status": "in_progress"},
		{"title": "the step, renamed"},
		{"body": "the plan"},
		{"add_depends_on": []any{blocker}},
		{"remove_depends_on": []any{blocker}},
		{"project": "other"},
		{"status": "done"},
	} {
		args["id"] = id
		callJSON(t, ctx, cli, "tasks_update", args)
		require.Empty(t, hrRevisions(t, ctx, cli, id), "%v", args)
	}
	require.Equal(t, "the plan", hrTaskBody(t, ctx, cli, id))

	// A body changed after the move is kept with the task where it is now.
	callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": id, "body": "the plan, as shipped"})
	require.Equal(t, []any{"the plan"}, hrBodies(hrRevisions(t, ctx, cli, id)))
}

// An isolation fence withholds a task's revisions exactly where it withholds
// its body: a read from outside a confidential project is refused, revisions
// and all; a write from outside lands, its answer trimmed of the body and with
// no revisions; the project's own session reads every revision, the outside
// write's included. A sealed project refuses the outside append outright and
// keeps nothing.
func TestTasksHistory_AFenceWithholdsRevisionsWithTheBody(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	isolate(t, ctx, db, "client-work", core.IsolationConfidential)
	inside := bindClient(t, ctx, db, url, "client-work")
	id := callJSON(t, ctx, inside, "tasks_add", map[string]any{
		"title": "rotate the client's keys", "body": "the client's deployment topology",
	})["id"].(string)
	callJSON(t, ctx, inside, "tasks_update", map[string]any{"id": id, "body": "rotated: see the topology"})

	outside := bindClient(t, ctx, db, url, "demo")
	isErr, txt := callErr(t, ctx, outside, "tasks_list", map[string]any{"id": id, "body_revisions": true})
	require.True(t, isErr, txt)
	require.Contains(t, txt, "project client-work is confidential: reads require a session bound to it")
	require.NotContains(t, txt, "topology")

	out := callJSON(t, ctx, outside, "tasks_update", map[string]any{"id": id, "body_append": "an outside finding"})
	raw := mustJSON(t, out)
	require.NotContains(t, raw, "topology", "the write landed; its answer is trimmed")
	require.NotContains(t, raw, "body_revisions")
	require.NotContains(t, out, "body")

	revs := hrRevisions(t, ctx, inside, id)
	require.Equal(t, []any{"rotated: see the topology", "the client's deployment topology"}, hrBodies(revs))
	require.Equal(t, "append", revs[0]["change"], "the outside append's revision")
	require.Regexp(t, `\Arotated: see the topology`+hrAppendedLine+`an outside finding\n\z`, hrTaskBody(t, ctx, inside, id))

	isolate(t, ctx, db, "vault", core.IsolationSealed)
	vault := bindClient(t, ctx, db, url, "vault")
	vaultTask := callJSON(t, ctx, vault, "tasks_add", map[string]any{"title": "vault step", "body": "the vault plan"})["id"].(string)
	isErr, txt = callErr(t, ctx, outside, "tasks_update", map[string]any{"id": vaultTask, "body_append": "from outside"})
	require.True(t, isErr, txt)
	require.Contains(t, txt, "project vault is sealed: writes into it require a session bound to it")
	require.Empty(t, hrRevisions(t, ctx, vault, vaultTask))
	require.Equal(t, "the vault plan", hrTaskBody(t, ctx, vault, vaultTask))
}

// A locked project's revisions are read only where its task is: refused at the
// lock on TCP and on a socket with no lease, read by the tree the owner
// approved.
func TestTasksHistory_ALockedProjectsRevisionsNeedItsLease(t *testing.T) {
	r := newLockRig(t)
	const secret = "reconcile every account by hand"
	admin := r.dial(r.serve(r.srv, nil), false)
	created, _ := r.ok(admin, "tasks_add", map[string]any{"project": lkFin, "title": "reconcile the ledger", "body": secret})
	var task struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(created), &task))
	r.ok(admin, "tasks_update", map[string]any{"id": task.ID, "body": "reconciled the quarter"})

	tcp := r.dial(r.tcp(), false)
	r.ok(tcp, "session_start", map[string]any{"project": lkOps, "name": "cc/hr-tcp"})
	stranger := r.dial(r.socket(lkStranger), false)
	r.ok(stranger, "session_start", map[string]any{"project": lkOps, "name": "cc/hr-stranger"})
	for name, cli := range map[string]*mcpclient.Client{"tcp": tcp, "socket without a lease": stranger} {
		text := r.refused(cli, "tasks_list", map[string]any{"id": task.ID, "body_revisions": true})
		require.Contains(t, text, lkFin, name)
		require.NotContains(t, text, secret, name)
		require.NotContains(t, text, "reconciled the quarter", name)
	}

	r.session("cc/fin", lkFin)
	agent := r.dial(r.socket(lkBridge, lkAgent), true)
	r.refused(agent, "tasks_list", nil)
	pending := r.pending()
	require.Len(t, pending, 1)
	r.approve(pending[0].ID, time.Hour)
	text, _ := r.ok(agent, "tasks_list", map[string]any{"id": task.ID, "body_revisions": true})
	require.Contains(t, text, secret, "the leased tree reads the earlier body")
	require.Contains(t, text, "reconciled the quarter")
}

// A restricted account (peer_auth.restricted) reads a task's revisions where it
// reads the task, and is told a task it may not reach does not exist, in the
// words a missing id gets. Its append stamps its label on the task, and each
// revision carries the label its text had.
func TestTasksHistory_ARestrictedAccountReadsRevisionsWhereItReadsTheTask(t *testing.T) {
	r := newKARig(t, kaOpts{})
	conf := r.call(r.owner, "tasks_add", map[string]any{"project": kaConf, "title": "conf task", "body": "the confidential plan"})["id"].(string)
	r.call(r.owner, "tasks_update", map[string]any{"id": conf, "body": "the confidential plan, revised"})
	got := r.refused(r.agent, "tasks_list", map[string]any{"id": conf, "body_revisions": true})
	require.Equal(t, `tasks_list: store.TaskByID: task not found: "`+conf+`"`, got)
	const missing = "01M7Y7ETR8063NJMCQBN796D0K"
	require.Equal(t, `tasks_list: store.TaskByID: task not found: "`+missing+`"`,
		r.refused(r.agent, "tasks_list", map[string]any{"id": missing, "body_revisions": true}))

	open := r.call(r.owner, "tasks_add", map[string]any{"project": kaOpen, "title": "open task", "body": "the owner's plan"})["id"].(string)
	out := r.call(r.agent, "tasks_update", map[string]any{"id": open, "body_append": "the account's finding"})
	require.Equal(t, kaAccount, out["updated_account"])
	r.call(r.owner, "tasks_update", map[string]any{"id": open, "body": "the owner's rewrite"})

	for _, cli := range []*mcpclient.Client{r.agent, r.owner} {
		res := r.call(cli, "tasks_list", map[string]any{"id": open, "body_revisions": true})
		task := res["tasks"].([]any)[0].(map[string]any)
		require.Equal(t, kaAccount, task["updated_account"], "the owner rewrote the body, not the title")
		revs := task["body_revisions"].([]any)
		require.Len(t, revs, 2)
		newest, oldest := revs[0].(map[string]any), revs[1].(map[string]any)
		require.Regexp(t, `\Athe owner's plan`+hrAppendedLine+`the account's finding\n\z`, newest["body"])
		require.Equal(t, kaAccount, newest["updated_account"], "the text the owner replaced held the account's words")
		require.Equal(t, "the owner's plan", oldest["body"])
		require.NotContains(t, oldest, "updated_account", "the owner's text carries no label")
	}
}

// A receipt carries no text, so a receipt with an append is refused with the
// rule, the value never echoed and nothing written; a receipt sent keeps no
// revision.
func TestTasksHistory_AReceiptCarriesNoAppend(t *testing.T) {
	r := receiptRigFor(t, true)
	ctx := context.Background()
	step := r.receiptStep(gOps, "build step")
	done := r.receiptMove(gFin)
	home := gFin + ":" + done.Child.ID
	rule := "tasks_update: store.JudgeReceipt: receipt refused: a receipt carries no text: it sets the status alone, with at " +
		"most one reference to an item in the conversation's home, example-finance, so "
	for _, args := range []map[string]any{
		{"id": step, "status": "done", "body_append": "built: the quarter's ledger totals"},
		{"id": step, "status": "dropped", "body_append": "the quarter's ledger totals", "reference": home},
	} {
		text := r.refused(r.cli, "tasks_update", args)
		require.Equal(t, rule+"body_append is refused", text)
		require.NotContains(t, text, "ledger", "the value is never echoed")
	}
	require.Equal(t, rule+"title, body_append are refused", r.refused(r.cli, "tasks_update",
		map[string]any{"id": step, "status": "done", "title": "t", "body_append": "b", "reference": home}),
		"each field named, in the schema's order")
	task := r.receiptTask(step)
	require.Equal(t, core.TaskInProgress, task.Status, "nothing was written")
	require.Equal(t, "build step: the plan", task.Body)
	revs, err := store.TaskBodyRevisions(ctx, r.db, step)
	require.NoError(t, err)
	require.Empty(t, revs)

	out := r.ok(r.cli, "tasks_update", map[string]any{"id": step, "status": "done"})
	require.Equal(t, true, out["receipt"])
	revs, err = store.TaskBodyRevisions(ctx, r.db, step)
	require.NoError(t, err)
	require.Empty(t, revs, "a receipt changes the status alone")
}
