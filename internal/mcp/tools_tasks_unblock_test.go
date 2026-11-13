package mcp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// tasks_update remove_depends_on: a blocker that turned out to be the wrong
// task is dropped from the task, not the task re-created under a new id. The
// removal is in the task's history (who and when), an id that is not a
// blocker is a no-op the answer names back, and the update's holder lock and
// fences apply to it like to any other change.

// transition is one task.transition record of a task, as its history
// holds it.
type transition struct {
	ts      time.Time
	session string
	payload map[string]any
}

// transitions returns the task's task.transition records, oldest first.
func transitions(t *testing.T, db *sql.DB, taskID string) []transition {
	t.Helper()
	rows, err := db.Query(`SELECT ts, session_id, payload FROM events WHERE kind = ? AND item_id = ? ORDER BY ts, id`,
		string(core.EventTaskTransition), taskID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []transition
	for rows.Next() {
		var ts, sess, payload string
		require.NoError(t, rows.Scan(&ts, &sess, &payload))
		at, err := core.ParseTime(ts)
		require.NoError(t, err)
		var p map[string]any
		require.NoError(t, json.Unmarshal([]byte(payload), &p))
		out = append(out, transition{ts: at, session: sess, payload: p})
	}
	require.NoError(t, rows.Err())
	return out
}

// readyIDs returns the ids tasks_ready lists as ready.
func readyIDs(t *testing.T, out map[string]any) []string {
	t.Helper()
	var ids []string
	for _, r := range out["ready"].([]any) {
		ids = append(ids, r.(map[string]any)["id"].(string))
	}
	return ids
}

func TestTasksUpdate_RemovingTheLastOpenBlockerMakesTheTaskReady(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	sess := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})["session_id"].(string)

	wrong := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "the wrong blocker"})["id"].(string)
	done := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "a finished blocker"})["id"].(string)
	callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": done, "status": "done"})
	waits := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "waits", "depends_on": []string{wrong, done}})["id"].(string)
	require.NotContains(t, readyIDs(t, callJSON(t, ctx, cli, "tasks_ready", nil)), waits)

	before := time.Now().UTC()
	out := callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": waits, "remove_depends_on": []string{wrong}})
	after := time.Now().UTC()
	require.Equal(t, []any{wrong}, out["removed_depends_on"])
	require.NotContains(t, out, "not_depends_on", "every id named was a blocker")
	require.Equal(t, []any{done}, out["depends_on"], "the finished blocker is still an edge")
	require.Equal(t, "open", out["status"])

	ready := callJSON(t, ctx, cli, "tasks_ready", nil)
	require.Contains(t, readyIDs(t, ready), waits, "its last unfinished blocker gone, the task is ready")
	require.Empty(t, ready["blocked"])

	// The history: who removed which blocker, and when.
	hist := transitions(t, db, waits)
	require.Len(t, hist, 2, "created, then the removal")
	last := hist[1]
	require.Equal(t, sess, last.session, "who: the acting session")
	require.False(t, last.ts.Before(before.Truncate(time.Millisecond)), "when")
	require.False(t, last.ts.After(after.Add(time.Millisecond)), "when")
	require.Equal(t, map[string]any{"to": "open", "from": "open", "removed_depends_on": []any{wrong}}, last.payload)

	// A removal riding a status change records both; from tells them apart.
	other := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "other", "depends_on": []string{wrong}})["id"].(string)
	out = callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": other, "status": "dropped", "remove_depends_on": wrong})
	require.Equal(t, "dropped", out["status"])
	require.Equal(t, []any{wrong}, out["removed_depends_on"], "the comma-separated form is accepted too")
	hist = transitions(t, db, other)
	require.Equal(t, map[string]any{"to": "dropped", "from": "open", "removed_depends_on": []any{wrong}}, hist[len(hist)-1].payload)
}

// An id that is not one of the task's blockers -- another task, or no task at
// all -- changes nothing: the answer names it back in not_depends_on, the row
// keeps its updated_at, and the history gains no record.
func TestTasksUpdate_RemovingANonBlockerIsANoOpThatSaysSo(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})

	blocker := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "blocker"})["id"].(string)
	stranger := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "not a blocker"})["id"].(string)
	waits := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "waits", "depends_on": []string{blocker}})["id"].(string)
	missing, err := core.NewID()
	require.NoError(t, err)
	before, err := store.TaskByID(ctx, db, waits)
	require.NoError(t, err)
	history := len(transitions(t, db, waits))

	out := callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": waits, "remove_depends_on": []string{stranger, missing, stranger}})
	require.Equal(t, []any{stranger, missing}, out["not_depends_on"])
	require.NotContains(t, out, "removed_depends_on")
	require.Equal(t, []any{blocker}, out["depends_on"])
	after, err := store.TaskByID(ctx, db, waits)
	require.NoError(t, err)
	require.Equal(t, before, after, "nothing written, updated_at included")
	require.Len(t, transitions(t, db, waits), history, "and nothing recorded")

	// One named blocker among non-blockers is removed; the rest are named back.
	out = callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": waits, "remove_depends_on": stranger + ", " + blocker})
	require.Equal(t, []any{blocker}, out["removed_depends_on"])
	require.Equal(t, []any{stranger}, out["not_depends_on"])
	require.NotContains(t, out, "depends_on")
	require.Len(t, transitions(t, db, waits), history+1)

	isErr, txt := callErr(t, ctx, cli, "tasks_update", map[string]any{"id": waits})
	require.True(t, isErr)
	require.Equal(t, "tasks_update: nothing to update: pass status, title, body, project, add_depends_on, or remove_depends_on", txt)

	isErr, txt = callErr(t, ctx, cli, "tasks_update", map[string]any{"id": waits,
		"add_depends_on": []string{stranger}, "remove_depends_on": []string{stranger}})
	require.True(t, isErr)
	require.Equal(t, `tasks_update: store.UpdateTask: dependency both added and removed: "`+stranger+`"`, txt)
}

// The holder lock covers a removal like any other change: another session is
// refused, a would-be no-op included, and the holder removes freely.
func TestTasksUpdate_RemovingABlockerIsBehindTheHolderLock(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	holder := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, holder, "session_start", map[string]any{"cwd": "/work/demo"})
	blocker := callJSON(t, ctx, holder, "tasks_add", map[string]any{"title": "blocker"})["id"].(string)
	callJSON(t, ctx, holder, "tasks_update", map[string]any{"id": blocker, "status": "done"})
	held := callJSON(t, ctx, holder, "tasks_add", map[string]any{"title": "held", "depends_on": []string{blocker}})["id"].(string)
	callJSON(t, ctx, holder, "tasks_claim", map[string]any{"id": held})

	other := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, other, "session_start", map[string]any{"cwd": "/work/demo", "name": "agent-b"})
	for _, ids := range []string{blocker, "01JZZZZZZZZZZZZZZZZZZZZZZZ"} {
		isErr, txt := callErr(t, ctx, other, "tasks_update", map[string]any{"id": held, "remove_depends_on": ids})
		require.True(t, isErr, txt)
		require.Contains(t, txt, "already claimed")
	}
	tk, err := store.TaskByID(ctx, db, held)
	require.NoError(t, err)
	require.Equal(t, []string{blocker}, tk.DependsOn)

	out := callJSON(t, ctx, holder, "tasks_update", map[string]any{"id": held, "remove_depends_on": []string{blocker}})
	require.Equal(t, []any{blocker}, out["removed_depends_on"])
	require.Equal(t, "in_progress", out["status"], "the claim stands")
}

// A restricted account (peer_auth.restricted) removes blockers only on tasks it
// may write: a task in a project it does not reach is a task that does not
// exist. A blocker it does not reach is not one it can remove, and the answer
// reads exactly as for an id that names nothing. Its removal puts no account
// label on the owner's task. The owner reaches every blocker.
func TestTasksUpdate_RestrictedAccountRemovesOnlyWhatItReaches(t *testing.T) {
	r := newKARig(t, kaOpts{})
	near := r.call(r.owner, "tasks_add", map[string]any{"project": kaOpen, "title": "near"})["id"].(string)
	far := r.call(r.owner, "tasks_add", map[string]any{"project": kaConf, "title": "far"})["id"].(string)
	waits := r.call(r.owner, "tasks_add", map[string]any{"project": kaOpen, "title": "waits",
		"depends_on": []string{near, far}})["id"].(string)
	closed := r.call(r.owner, "tasks_add", map[string]any{"project": kaConf, "title": "closed waits",
		"depends_on": []string{far}})["id"].(string)
	missing, err := core.NewID()
	require.NoError(t, err)
	sess := r.call(r.agent, "session_start", map[string]any{"project": kaOpen})["session_id"].(string)

	// The task it may not write: the words a missing task gets.
	refusedClosed := r.refused(r.agent, "tasks_update", map[string]any{"id": closed, "remove_depends_on": []string{far}})
	refusedMissing := r.refused(r.agent, "tasks_update", map[string]any{"id": missing, "remove_depends_on": []string{far}})
	require.Contains(t, refusedClosed, "task not found")
	require.Equal(t, strings.ReplaceAll(refusedMissing, missing, closed), refusedClosed)
	require.NotContains(t, refusedClosed, kaConf)

	// The blocker it may not reach: kept, and named back like a missing id.
	viaFar := r.call(r.agent, "tasks_update", map[string]any{"id": waits, "remove_depends_on": []string{far}})
	viaMissing := r.call(r.agent, "tasks_update", map[string]any{"id": waits, "remove_depends_on": []string{missing}})
	require.Equal(t, []any{far}, viaFar["not_depends_on"])
	require.Equal(t, strings.ReplaceAll(mustJSON(t, viaMissing), missing, far), mustJSON(t, viaFar))
	tk, err := store.TaskByID(r.ctx, r.db, waits)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{near, far}, tk.DependsOn)

	// The one it reaches goes, recorded under its session.
	out := r.call(r.agent, "tasks_update", map[string]any{"id": waits, "remove_depends_on": []string{near, far}})
	require.Equal(t, []any{near}, out["removed_depends_on"])
	require.Equal(t, []any{far}, out["not_depends_on"])
	require.NotContains(t, out, "updated_account", "a removal writes no text, so it labels nothing")
	hist := transitions(t, r.db, waits)
	require.Equal(t, sess, hist[len(hist)-1].session)
	require.Equal(t, []any{near}, hist[len(hist)-1].payload["removed_depends_on"])

	out = r.call(r.owner, "tasks_update", map[string]any{"id": waits, "remove_depends_on": []string{far}})
	require.Equal(t, []any{far}, out["removed_depends_on"], "the owner reaches the confidential blocker")
}

// A caller that may write a task but not read it (outside a confidential
// project) may remove its blockers, but its answer reports nothing of them:
// whether an id was a blocker is the fenced project's graph, so a removal and
// a no-op read the same. A blocker in a project the caller may not read, of a
// task it may, is reported like the add path confirms one: by the id it sent.
func TestTasksUpdate_RemovalReportFollowsTheReadFence(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	const vault = "example-vault"
	_, err := store.EnsureProject(ctx, db, vault, vault)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, db, vault, core.IsolationConfidential))
	now := time.Now().UTC()
	mk := func(project, title string, deps ...string) string {
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: id, ProjectSlug: project, Title: title,
			Status: core.TaskOpen, DependsOn: deps, CreatedAt: now, UpdatedAt: now}))
		return id
	}
	inner := mk(vault, "vault blocker")
	stranger := mk(vault, "vault stranger")
	vaultWaits := mk(vault, "vault waits", inner)
	demoWaits := mk("demo", "demo waits on the vault", inner)

	outside := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, outside, "session_start", map[string]any{"cwd": "/work/demo"})

	removed := callJSON(t, ctx, outside, "tasks_update", map[string]any{"id": vaultWaits, "remove_depends_on": []string{inner}})
	noop := callJSON(t, ctx, outside, "tasks_update", map[string]any{"id": vaultWaits, "remove_depends_on": []string{stranger}})
	require.Contains(t, removed, "withheld")
	require.Equal(t, mustJSON(t, noop), mustJSON(t, removed), "a removal and a no-op read the same from outside")
	tk, err := store.TaskByID(ctx, db, vaultWaits)
	require.NoError(t, err)
	require.Empty(t, tk.DependsOn, "the write landed")

	out := callJSON(t, ctx, outside, "tasks_update", map[string]any{"id": demoWaits, "remove_depends_on": []string{inner}})
	require.Equal(t, []any{inner}, out["removed_depends_on"])
	require.NotContains(t, out, "depends_on_withheld")
}

// Every tasks_update record carries the status before the call as from, so the
// history tells a status move from an update that moved none. A call that
// changed nothing records nothing; the other writers keep their shapes.
func TestTasksUpdate_RecordsTheStatusBeforeTheCall(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	blocker := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "blocker"})["id"].(string)
	id := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "work"})["id"].(string)

	callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": id, "title": "work, renamed"})
	callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": id, "add_depends_on": []string{blocker}})
	callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": id, "status": "done"})
	callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": id, "body": "notes after the fact"})
	callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": id, "status": "open"})

	var got []map[string]any
	for _, tr := range transitions(t, db, id) {
		got = append(got, tr.payload)
	}
	require.Equal(t, []map[string]any{
		{"to": "open", "created": true},
		{"to": "open", "from": "open"},
		{"to": "open", "from": "open"},
		{"to": "done", "from": "open"},
		{"to": "done", "from": "done"},
		{"to": "open", "from": "done"},
	}, got)
}
