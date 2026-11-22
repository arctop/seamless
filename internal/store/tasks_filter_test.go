package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// bkBase is the instant the filter tests' tasks are created from.
var bkBase = time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

// bkTask inserts a task in project under plan ("" for none); mut adjusts it
// first. It answers the id.
func bkTask(t *testing.T, db *sql.DB, project, plan, title string, seq int, mut func(*core.Task)) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	at := bkBase.Add(time.Duration(seq) * time.Minute)
	task := core.Task{ID: id, ProjectSlug: project, PlanSlug: plan, Title: title, Status: core.TaskOpen,
		CreatedAt: at, UpdatedAt: at}
	if mut != nil {
		mut(&task)
	}
	require.NoError(t, CreateTask(context.Background(), db, task))
	return id
}

// bkEvent records a task.transition for item at ts, by session, with payload.
func bkEvent(t *testing.T, db *sql.DB, item, session string, ts time.Time, payload map[string]any) {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload) VALUES (?, ?, ?, ?, '', ?, ?)`,
		id, core.FormatTime(ts), string(core.EventTaskTransition), session, item, string(raw))
	require.NoError(t, err)
}

func bkTitles(tasks []core.Task) []string {
	out := make([]string, len(tasks))
	for i, task := range tasks {
		out[i] = task.Title
	}
	return out
}

// The default filter is the tasks outside a plan; Steps adds the plan steps
// and Plan keeps one plan's; a project narrows, the global scope included.
func TestBkTaskFilter_OffPlanByDefault(t *testing.T) {
	ctx := context.Background()
	db := newTaskDB(t)
	bkTask(t, db, "example-ops", "", "ops loose", 1, nil)
	bkTask(t, db, "example-ops", "ops-plan", "ops step", 2, nil)
	bkTask(t, db, "example-docs", "", "docs loose", 3, nil)
	bkTask(t, db, "example-docs", "docs-plan", "docs step", 4, nil)
	bkTask(t, db, "", "", "global loose", 5, nil)

	for _, tc := range []struct {
		name string
		f    TaskFilter
		want []string
	}{
		{"default", TaskFilter{}, []string{"ops loose", "docs loose", "global loose"}},
		{"steps", TaskFilter{Steps: true}, []string{"ops loose", "ops step", "docs loose", "docs step", "global loose"}},
		{"one plan", TaskFilter{Plan: "ops-plan"}, []string{"ops step"}},
		{"one plan, steps on", TaskFilter{Plan: "ops-plan", Steps: true}, []string{"ops step"}},
		{"a project", TaskFilter{ByProject: true, Project: "example-docs"}, []string{"docs loose"}},
		{"a project with steps", TaskFilter{ByProject: true, Project: "example-docs", Steps: true}, []string{"docs loose", "docs step"}},
		{"the global scope", TaskFilter{ByProject: true}, []string{"global loose"}},
		{"a plan elsewhere", TaskFilter{ByProject: true, Project: "example-docs", Plan: "ops-plan"}, []string{}},
	} {
		got, err := FilteredReadyTasks(ctx, db, tc.f)
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.want, bkTitles(got), tc.name)
	}
}

// The text filter reads title and body in any case, a whole id, and the
// start or end of an id: the 8 characters the console prints find their
// task. LIKE's wildcards in the text match only themselves.
func TestBkTaskFilter_TextAndIDs(t *testing.T) {
	ctx := context.Background()
	db := newTaskDB(t)
	widget := bkTask(t, db, "example-ops", "", "Build the WIDGET", 1, nil)
	bkTask(t, db, "example-ops", "", "write docs", 2, func(k *core.Task) { k.Body = "covers the widget too, 100% of it" })
	bkTask(t, db, "example-ops", "", "unrelated", 3, nil)

	find := func(q string) []string {
		t.Helper()
		got, err := FilteredReadyTasks(ctx, db, TaskFilter{Query: q})
		require.NoError(t, err, q)
		return bkTitles(got)
	}
	require.Equal(t, []string{"Build the WIDGET", "write docs"}, find("widget"))
	require.Equal(t, []string{"write docs"}, find("100%"))
	require.Equal(t, []string{}, find("10_%"), "an underscore is a letter, not a wildcard")
	require.Equal(t, []string{"Build the WIDGET"}, find(widget))
	require.Equal(t, []string{"Build the WIDGET"}, find(widget[len(widget)-8:]), "a pasted short id")
	require.Equal(t, []string{"Build the WIDGET"}, find(" "+strings.ToLower(widget[len(widget)-8:])+" "), "any case, padded")
	require.Equal(t, []string{"Build the WIDGET"}, find(widget[:12]), "the start of an id")
	_, ok := taskIDPart(widget[len(widget)-7:])
	require.False(t, ok, "seven characters match ids by chance")
}

// Every group reads under the filter, and the closed tasks page newest
// first with their total.
func TestBkTaskFilter_GroupsAndClosedPages(t *testing.T) {
	ctx := context.Background()
	db := newTaskDB(t)
	free := bkTask(t, db, "example-ops", "", "free", 1, nil)
	bkTask(t, db, "example-ops", "", "after free", 2, func(k *core.Task) { k.DependsOn = []string{free} })
	bkTask(t, db, "example-ops", "p", "step after free", 3, func(k *core.Task) { k.DependsOn = []string{free} })
	gate := bkTask(t, db, "example-ops", "", "GATE", 4, func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "approve"}
	})
	bkTask(t, db, "example-ops", "", "behind the gate", 5, func(k *core.Task) { k.DependsOn = []string{gate} })
	bkTask(t, db, "example-ops", "", "started", 6, func(k *core.Task) { k.Status = core.TaskInProgress })
	bkTask(t, db, "example-ops", "", "started gate", 7, func(k *core.Task) {
		k.Status, k.Kind, k.Gate = core.TaskInProgress, core.TaskKindGate, core.TaskGate{Who: "vendor", What: "ships"}
	})
	for i := range 30 {
		closed := bkBase.Add(time.Duration(i) * time.Hour)
		bkTask(t, db, "example-ops", "", "closed "+string(rune('a'+i%26))+string(rune('0'+i/26)), 10+i, func(k *core.Task) {
			k.Status, k.ClosedAt = core.TaskDone, &closed
		})
	}

	f := TaskFilter{}
	blocked, err := FilteredBlockedTasks(ctx, db, f)
	require.NoError(t, err)
	require.Equal(t, []string{"after free"}, blockedTitles(t, blocked))
	blocked, err = FilteredBlockedTasks(ctx, db, TaskFilter{Steps: true})
	require.NoError(t, err)
	require.Equal(t, []string{"after free", "step after free"}, blockedTitles(t, blocked))
	gated, err := FilteredGatedTasks(ctx, db, f)
	require.NoError(t, err)
	require.Equal(t, []string{"behind the gate"}, blockedTitles(t, gated))
	gates, err := FilteredGateTasks(ctx, db, f)
	require.NoError(t, err)
	require.Equal(t, []string{"GATE", "started gate"}, blockedTitles(t, gates))
	started, err := FilteredStartedTasks(ctx, db, f)
	require.NoError(t, err)
	require.Equal(t, []string{"started"}, bkTitles(started), "a gate someone started stays with the gates")

	page1, total, err := FilteredClosedTasks(ctx, db, f, 25, 0)
	require.NoError(t, err)
	require.Equal(t, 30, total)
	require.Len(t, page1, 25)
	require.Equal(t, "closed d1", page1[0].Title, "the newest closed first")
	page2, total, err := FilteredClosedTasks(ctx, db, f, 25, 25)
	require.NoError(t, err)
	require.Equal(t, 30, total)
	require.Len(t, page2, 5)
	require.Equal(t, "closed a0", page2[4].Title)
	none, total, err := FilteredClosedTasks(ctx, db, TaskFilter{Query: "nothing like this"}, 25, 0)
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, none)

	counts, err := OpenTaskCountsByProject(ctx, db, TaskFilter{ByProject: true, Project: "elsewhere"})
	require.NoError(t, err)
	require.Equal(t, map[string]int{"example-ops": 6}, counts, "the menu's counts ignore the project and follow the rest")
	counts, err = OpenTaskCountsByProject(ctx, db, TaskFilter{Steps: true})
	require.NoError(t, err)
	require.Equal(t, map[string]int{"example-ops": 7}, counts)

	for _, tc := range []struct {
		plan string
		want bool
	}{{"p", true}, {"q", false}, {"", false}} {
		found, err := TaskPlanExists(ctx, db, tc.plan)
		require.NoError(t, err)
		require.Equal(t, tc.want, found, tc.plan)
	}
	found, err := TaskProjectExists(ctx, db, "example-ops")
	require.NoError(t, err)
	require.True(t, found)
	found, err = TaskProjectExists(ctx, db, "")
	require.NoError(t, err)
	require.False(t, found)
	steps, err := AnyPlanStep(ctx, db)
	require.NoError(t, err)
	require.True(t, steps)
}

// TaskStateSince: a status's latest run of events starts its time, a
// heartbeat's re-claim and a gate change continue it, the gate's set time and
// the last blocker's close are read beside it, and a status the events do not
// end on reads as unknown.
func TestBkTaskStateSince(t *testing.T) {
	ctx := context.Background()
	db := newTaskDB(t)
	at := func(m int) time.Time { return bkBase.Add(time.Duration(m) * time.Minute) }
	dep := bkTask(t, db, "example-ops", "", "dep", 1, nil)
	task := bkTask(t, db, "example-ops", "", "task", 2, func(k *core.Task) { k.DependsOn = []string{dep} })
	bkEvent(t, db, task, "s1", at(2), map[string]any{"to": "open", "created": true})
	bkEvent(t, db, task, "s1", at(10), map[string]any{"to": "in_progress", "claimed_by": "s1"})
	bkEvent(t, db, task, "s1", at(20), map[string]any{"to": "in_progress", "claimed_by": "s1"})
	bkEvent(t, db, task, "s1", at(30), map[string]any{"to": "open", "released": true})
	bkEvent(t, db, task, "", at(40), map[string]any{"to": "open", "from": "open", "gate": "set", "by": "console"})
	bkEvent(t, db, task, "", at(50), map[string]any{"to": "open", "from": "open", "gate": "cleared", "by": "console"})
	closed := at(45)
	_, err := db.Exec(`UPDATE tasks SET status = 'done', closed_at = ? WHERE id = ?`, core.FormatTime(closed), dep)
	require.NoError(t, err)
	held := bkTask(t, db, "example-ops", "", "held", 3, nil)
	bkEvent(t, db, held, "s2", at(5), map[string]any{"to": "open", "created": true})
	bkEvent(t, db, held, "s2", at(15), map[string]any{"to": "in_progress", "claimed_by": "s2"})
	bkEvent(t, db, held, "s2", at(25), map[string]any{"to": "in_progress", "claimed_by": "s2"})
	quiet := bkTask(t, db, "example-ops", "", "quiet", 4, nil)
	// A status the record does not end on (a row changed outside the writers).
	odd := bkTask(t, db, "example-ops", "", "odd", 5, nil)
	bkEvent(t, db, odd, "", at(5), map[string]any{"to": "in_progress"})

	got, err := TaskStateSince(ctx, db, []core.Task{
		{ID: task, Status: core.TaskOpen}, {ID: held, Status: core.TaskInProgress},
		{ID: quiet, Status: core.TaskOpen}, {ID: odd, Status: core.TaskOpen},
	})
	require.NoError(t, err)
	require.Equal(t, at(30), got[task].Status, "open again since the release; a gate change continues the run")
	require.Equal(t, at(40), got[task].GateSet)
	require.Equal(t, closed, got[task].BlockersClosed)
	require.Equal(t, at(15), got[held].Status, "in progress since the first claim: a heartbeat continues it")
	require.Equal(t, TaskSince{}, got[quiet])
	require.True(t, got[odd].Status.IsZero())
	empty, err := TaskStateSince(ctx, db, nil)
	require.NoError(t, err)
	require.Empty(t, empty)
}

// TaskTransitions answers a task's newest events oldest first, and an event
// whose payload is not JSON reads as empty.
func TestBkTaskTransitions(t *testing.T) {
	ctx := context.Background()
	db := newTaskDB(t)
	id := bkTask(t, db, "example-ops", "", "task", 1, nil)
	for i := range 5 {
		bkEvent(t, db, id, "s", bkBase.Add(time.Duration(i)*time.Minute), map[string]any{"to": "open", "n": i})
	}
	_, err := db.Exec(`INSERT INTO events (id, ts, kind, item_id, payload) VALUES ('01BADPAYLOAD0000000000000X', ?, ?, ?, 'not json')`,
		core.FormatTime(bkBase.Add(time.Hour)), string(core.EventTaskTransition), id)
	require.NoError(t, err)
	evs, err := TaskTransitions(ctx, db, id, 3)
	require.NoError(t, err)
	require.Len(t, evs, 3)
	require.Equal(t, float64(3), evs[0].Payload["n"])
	require.Equal(t, float64(4), evs[1].Payload["n"])
	require.Empty(t, evs[2].Payload)
	require.Equal(t, core.EventTaskTransition, evs[2].Kind)
	none, err := TaskTransitions(ctx, db, "", 3)
	require.NoError(t, err)
	require.Empty(t, none)
}

// Through a view that hides a project, every filtered read leaves its tasks
// out, its events with them: the read itself, not a filter afterwards.
func TestBkTaskFilter_HiddenProjectIsAbsent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	open := bkTask(t, db, "example-ops", "", "ops task", 1, nil)
	hidden := bkTask(t, db, "example-finance", "", "finance secret", 2, nil)
	bkTask(t, db, "example-finance", "ledger", "finance step", 3, nil)
	bkTask(t, db, "example-finance", "", "finance closed", 4, func(k *core.Task) { k.Status = core.TaskDone })
	bkEvent(t, db, hidden, "", bkBase, map[string]any{"to": "open", "created": true})
	bkEvent(t, db, open, "", bkBase, map[string]any{"to": "open", "created": true})

	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	ready, err := FilteredReadyTasks(ctx, view, TaskFilter{Steps: true, Query: "task"})
	require.NoError(t, err)
	require.Equal(t, []string{"ops task"}, bkTitles(ready))
	ready, err = FilteredReadyTasks(ctx, view, TaskFilter{Query: "finance"})
	require.NoError(t, err)
	require.Empty(t, ready)
	_, total, err := FilteredClosedTasks(ctx, view, TaskFilter{}, 25, 0)
	require.NoError(t, err)
	require.Zero(t, total)
	counts, err := OpenTaskCountsByProject(ctx, view, TaskFilter{Steps: true})
	require.NoError(t, err)
	require.Equal(t, map[string]int{"example-ops": 1}, counts)
	found, err := TaskPlanExists(ctx, view, "ledger")
	require.NoError(t, err)
	require.False(t, found)
	found, err = TaskProjectExists(ctx, view, "example-finance")
	require.NoError(t, err)
	require.False(t, found)
	evs, err := TaskTransitions(ctx, view, hidden, 10)
	require.NoError(t, err)
	require.Empty(t, evs)
	since, err := TaskStateSince(ctx, view, []core.Task{{ID: hidden, Status: core.TaskOpen}})
	require.NoError(t, err)
	require.Equal(t, TaskSince{}, since[hidden])
	evs, err = TaskTransitions(ctx, view, open, 10)
	require.NoError(t, err)
	require.Len(t, evs, 1)
}

// The Tasks list's reads (plan:console-ux-pass UX 30): the week's closes
// under a filter, the open tasks a locked project blocks (on the main
// handle), what each gate holds, and the filter widened to the owner's
// queue; through a view that hides a project, its tasks are absent from
// each read.
func TestTlTaskListReads(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	recent, old := bkBase.Add(-24*time.Hour), bkBase.Add(-9*24*time.Hour)
	bkTask(t, db, "example-ops", "", "closed recently", 1, func(k *core.Task) { k.Status, k.ClosedAt = core.TaskDone, &recent })
	bkTask(t, db, "example-ops", "", "closed long ago", 2, func(k *core.Task) { k.Status, k.ClosedAt = core.TaskDone, &old })
	bkTask(t, db, "example-ops", "ops-plan", "closed step", 3, func(k *core.Task) { k.Status, k.ClosedAt = core.TaskDropped, &recent })
	bkTask(t, db, "example-finance", "", "finance closed", 4, func(k *core.Task) { k.Status, k.ClosedAt = core.TaskDone, &recent })
	secret := bkTask(t, db, "example-finance", "", "finance secret", 5, nil)
	gate := bkTask(t, db, "example-ops", "", "ops gate", 6, func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "sign"}
	})
	behind := bkTask(t, db, "example-ops", "", "ops behind the gate", 7, func(k *core.Task) { k.DependsOn = []string{gate} })
	bkTask(t, db, "example-finance", "", "finance behind the gate", 8, func(k *core.Task) { k.DependsOn = []string{gate} })
	bkTask(t, db, "example-ops", "", "ops done behind the gate", 9, func(k *core.Task) {
		k.DependsOn, k.Status, k.ClosedAt = []string{gate}, core.TaskDone, &old
	})
	waits := bkTask(t, db, "example-ops", "", "ops waits on finance", 10, func(k *core.Task) { k.DependsOn = []string{secret} })
	bkTask(t, db, "example-ops", "", "ops started, waits on finance", 11, func(k *core.Task) {
		k.DependsOn, k.Status = []string{secret}, core.TaskInProgress
	})

	since := bkBase.Add(-7 * 24 * time.Hour)
	for _, tc := range []struct {
		f    TaskFilter
		want int
	}{
		{TaskFilter{}, 2}, {TaskFilter{Steps: true}, 3}, {TaskFilter{Plan: "ops-plan"}, 1},
		{TaskFilter{ByProject: true, Project: "example-ops"}, 1}, {TaskFilter{Query: "recently"}, 1},
	} {
		n, err := CountClosedSince(ctx, db, tc.f, since)
		require.NoError(t, err)
		require.Equal(t, tc.want, n, "%+v", tc.f)
	}
	locked, err := LockedBlockedTasks(ctx, db, []string{"example-finance", "example-finance"})
	require.NoError(t, err)
	require.Equal(t, []string{waits}, locked, "open tasks only, each once")
	none, err := LockedBlockedTasks(ctx, db, nil)
	require.NoError(t, err)
	require.Empty(t, none)
	holds, err := OpenDependentCounts(ctx, db, []string{gate, behind, gate})
	require.NoError(t, err)
	require.Equal(t, map[string]int{gate: 2}, holds, "the unfinished tasks behind the gate")

	require.Equal(t, TaskFilter{Steps: true}, TaskFilter{}.WithOwnerQueue())
	require.Equal(t, TaskFilter{Plan: "p"}, TaskFilter{Plan: "p"}.WithOwnerQueue())
	require.Equal(t, TaskFilter{ByProject: true, Project: "x", Query: "q", Steps: true},
		TaskFilter{ByProject: true, Project: "x", Query: "q"}.WithOwnerQueue())

	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	n, err := CountClosedSince(ctx, view, TaskFilter{}, since)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the hidden project's close is absent")
	holds, err = OpenDependentCounts(ctx, view, []string{gate})
	require.NoError(t, err)
	require.Equal(t, map[string]int{gate: 1}, holds, "the hidden project's dependent is absent")
}

// The place query's owner predicate is core.GateWhoMatches(who, "owner"):
// the whole value or its class before a colon, trimmed, in any case; and a
// step waits only on the owner's gates when no unfinished gate on anyone
// else blocks it.
func TestTlOwnerGateSQLIsGateWhoMatches(t *testing.T) {
	ctx := context.Background()
	db := newTaskDB(t)
	for i, who := range []string{"owner", "OWNER", " Owner ", "owner:legal", "Owner : the release", "owner\t",
		"ownerx", "owners:x", "alice:owner", "co-owner", "own", "reviewer:owner"} {
		id := bkTask(t, db, "example-ops", "", "gate "+who, i, func(k *core.Task) {
			k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: who, What: "x"}
		})
		var got bool
		require.NoError(t, db.QueryRowContext(ctx, `SELECT `+sqlGateOnOwner("t")+` FROM tasks t WHERE t.id = ?`, id).Scan(&got))
		task, err := TaskByID(ctx, db, id)
		require.NoError(t, err)
		require.Equal(t, core.GateWhoMatches(task.Gate.Who, "owner"), got, "who %q (stored %q)", who, task.Gate.Who)
	}
	owner := bkTask(t, db, "example-ops", "", "the owner's gate", 20, func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "sign"}
	})
	bob := bkTask(t, db, "example-ops", "", "bob's gate", 21, func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "bob", What: "sign"}
	})
	for _, tc := range []struct {
		deps   []string
		others bool
	}{{[]string{owner}, false}, {[]string{bob}, true}, {[]string{owner, bob}, true}} {
		id := bkTask(t, db, "example-ops", "", "behind", 30, func(k *core.Task) { k.DependsOn = tc.deps })
		var got bool
		require.NoError(t, db.QueryRowContext(ctx, `SELECT `+sqlGateBlockerOnOthers("t")+` FROM tasks t WHERE t.id = ?`, id).Scan(&got))
		require.Equal(t, tc.others, got, "%v", tc.deps)
	}
}
