package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// Tasks as a backlog you can search and triage (plan:console-ux-pass UX 08):
// the default list is the tasks outside a plan, a toggle adds the steps, the
// URL holds the filters, the rows say where each task stands and for how long,
// the reader tells the task's history, and Waits on someone sets a gate.

// bkTask creates a task in project; mut adjusts it first. It answers the id.
func bkTask(t *testing.T, db *sql.DB, project, plan, title string, mut func(*core.Task)) string {
	t.Helper()
	now := time.Now().UTC()
	task := core.Task{ID: mustID(t), ProjectSlug: project, PlanSlug: plan, Title: title, Status: core.TaskOpen,
		CreatedAt: now, UpdatedAt: now}
	if mut != nil {
		mut(&task)
	}
	require.NoError(t, store.CreateTask(context.Background(), db, task))
	return task.ID
}

// bkSession creates a session named name in project and answers its id.
func bkSession(t *testing.T, db *sql.DB, project, name string) string {
	t.Helper()
	now := time.Now().UTC()
	id := mustID(t)
	require.NoError(t, store.CreateSession(context.Background(), db, core.Session{ID: id, Name: name, ProjectSlug: project,
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now}))
	return id
}

// bkPage is the Tasks page's JSON model at path.
func bkPage(t *testing.T, mux *http.ServeMux, path string) tasksData {
	t.Helper()
	var data tasksData
	getJSON(t, mux, path, &data)
	return data
}

// bkTitles is the titles of rows, in order.
func bkTitles(rows []taskRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Title)
	}
	return out
}

// bkGet answers a GET as the CLI (asJSON) or a browser with the key.
func bkGet(mux *http.ServeMux, path string, asJSON bool) (int, string) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	if asJSON {
		req.Header.Set("Accept", "application/json")
	}
	rr := do(mux, req)
	return rr.Code, rr.Body.String()
}

// bkError reads a JSON error answer's message.
func bkError(t *testing.T, body string) string {
	t.Helper()
	var answer map[string]string
	require.NoError(t, json.Unmarshal([]byte(body), &answer), body)
	return answer["error"]
}

// The default list holds no plan step but the ones waiting on the owner,
// which Needs you draws whatever the toggle says; the toggle adds the rest,
// each with its plan chip, and the header's counts follow it.
func TestBkDefaultListIsOffPlan_StepsToggleAddsThem(t *testing.T) {
	db, mux := newConsole(t)
	bkTask(t, db, "example-ops", "", "loose task", nil)
	bkTask(t, db, "example-ops", "ship-plan", "plan step one", nil)
	gate := bkTask(t, db, "example-ops", "ship-plan", "plan gate", func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "approve"}
	})
	bkTask(t, db, "example-ops", "ship-plan", "plan step behind the gate", func(k *core.Task) { k.DependsOn = []string{gate} })

	data := bkPage(t, mux, "/console/tasks?format=json")
	require.Equal(t, []string{"loose task"}, bkTitles(data.Ready))
	require.Empty(t, data.Gates)
	require.Empty(t, data.Gated)
	require.Equal(t, taskCounts{Open: 1, Ready: 1}, data.Counts)
	page := getHTMLBody(t, mux, "/console/tasks")
	require.NotContains(t, page, "plan step one")
	require.NotContains(t, page, "plan step behind the gate")
	require.Equal(t, []string{gate}, tlGroupIDs(page, "needs"), "the plan's gate on the owner needs the owner")
	require.Contains(t, tlRow(t, page, gate), `data-plan="ship-plan"`)
	require.Contains(t, page, `href="/console/tasks?steps=1" data-task-steps`)

	data = bkPage(t, mux, "/console/tasks?format=json&steps=1")
	require.Equal(t, []string{"loose task", "plan step one"}, bkTitles(data.Ready))
	require.Equal(t, "ship-plan", data.Ready[1].Plan)
	require.Equal(t, taskCounts{Open: 4, Ready: 2, Waiting: 2}, data.Counts, "the counts follow the toggle")
	page = getHTMLBody(t, mux, "/console/tasks?steps=1")
	row := tlRowNamed(t, page, "plan step one")
	require.Contains(t, row, `<a class="tl-plan" href="/console/tasks?plan=ship-plan" data-plan="ship-plan" data-seam-query title="Show this plan's steps">plan:ship-plan</a>`)
	require.Contains(t, page, `class="lib-flag on" href="/console/tasks" data-task-steps`, "the toggle turns them off again")
	require.Contains(t, page, `>1 needs you</a>`, "the gate")
	require.Contains(t, page, `>1 blocked</a>`, "the step it holds")
	require.Contains(t, page, `href="/console/tasks/`+data.Ready[1].ID+`?steps=1"`, "each row keeps the state")

	// With only plan steps, the default list offers them, not "no tasks".
	db2, mux2 := newConsole(t)
	bkTask(t, db2, "example-ops", "ship-plan", "only a step", nil)
	page = getHTMLBody(t, mux2, "/console/tasks")
	require.Contains(t, page, "Every task is a plan step")
	require.NotContains(t, page, "No tasks yet")
}

// Each filter narrows: a project (the global scope by name), a plan (as
// copied, plan:<slug>), text in a title or body, an id or a pasted short id;
// the project menu lists the projects with open tasks and their counts.
func TestBkFiltersNarrow(t *testing.T) {
	db, mux := newConsole(t)
	widget := bkTask(t, db, "example-ops", "", "Build the widget", nil)
	bkTask(t, db, "example-ops", "", "write the docs", func(k *core.Task) { k.Body = "and the widget's manual" })
	bkTask(t, db, "example-docs", "", "docs chore", nil)
	bkTask(t, db, "example-docs", "docs-plan", "docs step", nil)
	bkTask(t, db, "", "", "global chore", nil)
	closed := time.Now().UTC()
	bkTask(t, db, "example-docs", "", "docs closed", func(k *core.Task) { k.Status, k.ClosedAt = core.TaskDone, &closed })

	for _, tc := range []struct {
		path        string
		ready, shut []string
	}{
		{"?project=example-docs", []string{"docs chore"}, []string{"docs closed"}},
		{"?project=example-docs&steps=1", []string{"docs chore", "docs step"}, []string{"docs closed"}},
		{"?project=global", []string{"global chore"}, nil},
		{"?plan=docs-plan", []string{"docs step"}, nil},
		{"?plan=plan:docs-plan", []string{"docs step"}, nil},
		{"?q=widget", []string{"Build the widget", "write the docs"}, nil},
		{"?q=WIDGET&project=example-docs", nil, nil},
		{"?q=" + widget, []string{"Build the widget"}, nil},
		{"?q=" + widget[len(widget)-8:], []string{"Build the widget"}, nil},
	} {
		data := bkPage(t, mux, "/console/tasks"+tc.path+"&format=json")
		require.Equal(t, append([]string{}, tc.ready...), bkTitles(data.Ready), tc.path)
		require.Equal(t, append([]string{}, tc.shut...), bkTitles(data.Closed), tc.path)
	}
	data := bkPage(t, mux, "/console/tasks?format=json&plan=plan:docs-plan")
	require.Equal(t, "docs-plan", data.Filter.Plan, "the pasted plan: names the slug")

	data = bkPage(t, mux, "/console/tasks?format=json&project=example-docs")
	require.Equal(t, []taskProjectCount{{"", 1}, {"example-docs", 1}, {"example-ops", 2}}, data.Projects,
		"the menu counts every project under the other filters, global first")
	page := getHTMLBody(t, mux, "/console/tasks?project=example-docs")
	menu := page[strings.Index(page, "data-scope "):]
	menu = menu[:strings.Index(menu, "</details>")]
	require.Contains(t, menu, `<strong>example-docs</strong>`)
	require.Contains(t, menu, `<a class="scope-o" id="sc-o-all" data-scope-v="" data-seam-query href="/console/tasks">All projects<span class="n">4</span></a>`)
	require.Contains(t, menu, `<a class="scope-o on" id="sc-o-example-docs" data-scope-v="example-docs" data-seam-query href="/console/tasks?project=example-docs">example-docs<span class="n">1</span>`)
	require.Contains(t, menu, `href="/console/tasks?project=global"`)
	require.Contains(t, page, `class="lib-clear" href="/console/tasks"`)
	require.Contains(t, page, `<input type="hidden" name="project" value="example-docs">`, "typing keeps the project")

	short := getHTMLBody(t, mux, "/console/tasks?q="+widget[len(widget)-8:])
	require.Contains(t, short, `id="tl-`+widget+`"`, "a pasted short id finds its task")
	require.Contains(t, tlRow(t, short, widget), `href="/console/tasks/`+widget+`?q=`+widget[len(widget)-8:]+`"`)
	require.NotContains(t, short, `data-auto-url`, "the list opens no task")

	none := getHTMLBody(t, mux, "/console/tasks?q=nothing-like-this")
	require.Contains(t, none, "No task matches &ldquo;nothing-like-this&rdquo;")
}

// Parameters are strict: an unknown value is a 400 that names the valid
// ones, a project or plan that names nothing here is a 400 too, and a closed
// page past the last is a 404 naming how many there are.
func TestBkStrictParameters(t *testing.T) {
	db, mux := newConsole(t)
	id := bkTask(t, db, "example-ops", "p", "a step", nil)
	for _, tc := range []struct{ path, says string }{
		{"?steps=yes", `invalid steps "yes": valid values are 1 (or omit)`},
		{"?closed=some", `invalid closed "some": valid values are all (or omit)`},
		{"?sort=recent", `invalid sort "recent": valid values are oldest, newest`},
		{"?page=2", `invalid page "2": page pages through the closed tasks, with closed=all`},
		{"?closed=all&page=0", `invalid page "0": a page number from 1`},
		{"?project=a/b", `invalid project "a/b": name a project's slug, or global for the global scope`},
		{"?project=example-nowhere", `invalid project "example-nowhere": no project has that slug`},
		{"?plan=nope", `invalid plan "nope": no task here is a step of that plan`},
		{"?plan=a:b", `invalid plan "a:b": name a plan's slug`},
	} {
		code, body := bkGet(mux, "/console/tasks"+tc.path, true)
		require.Equal(t, http.StatusBadRequest, code, tc.path)
		require.Contains(t, bkError(t, body), tc.says, tc.path)
		// The page and a task's page, which draws the list beside it, alike.
		for _, base := range []string{"/console/tasks", "/console/tasks/" + id} {
			code, body = bkGet(mux, base+tc.path, false)
			require.Equal(t, http.StatusBadRequest, code, base+tc.path)
			require.Contains(t, body, "Bad request", base+tc.path)
		}
	}
	code, body := bkGet(mux, "/console/tasks?closed=all&page=2", true)
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, "No page 2 of the closed tasks: they fill 1 page.", bkError(t, body))
	code, _ = bkGet(mux, "/console/tasks?closed=all&page=1&format=json", true)
	require.Equal(t, http.StatusOK, code)
	// A flash, the reader and the format are other readers' parameters.
	code, _ = bkGet(mux, "/console/tasks?notice=Done.&format=json", true)
	require.Equal(t, http.StatusOK, code)
}

// The list folds the tasks closed in the last week, the newest 25 of them,
// and links to the rest; closed=all pages through every one, 25 at a time,
// newest first.
func TestBkClosedTasksPagePast25(t *testing.T) {
	db, mux := newConsole(t)
	base := time.Now().UTC().Add(-48 * time.Hour)
	for i := range 30 {
		at := base.Add(time.Duration(i) * time.Minute)
		bkTask(t, db, "example-ops", "", "closed "+string(rune('A'+i)), func(k *core.Task) { k.Status, k.ClosedAt = core.TaskDone, &at })
	}
	data := bkPage(t, mux, "/console/tasks?format=json")
	require.Len(t, data.Closed, closedLimit)
	require.Equal(t, 30, data.ClosedTotal)
	require.Equal(t, 5, data.ClosedMore)
	require.Equal(t, "closed "+string(rune('A'+29)), data.Closed[0].Title)
	require.Equal(t, 30, data.ClosedRecent)
	page := getHTMLBody(t, mux, "/console/tasks")
	require.Contains(t, page, `<a class="ml-more" href="/console/tasks?closed=all#tl-g-closed">All 30 closed tasks`)
	require.Contains(t, tlGroup(page, "closed"), `<span class="ml-note">25 of 30</span>`)
	require.Contains(t, page, `<details class="ml-group tl-group tl-fold" id="tl-g-closed" data-group="closed">`, "folded")

	data = bkPage(t, mux, "/console/tasks?format=json&closed=all&page=2")
	require.Len(t, data.Closed, 5)
	require.Equal(t, 2, data.ClosedPage)
	require.Equal(t, 2, data.ClosedPages)
	require.Equal(t, "closed E", data.Closed[0].Title)
	require.Equal(t, "closed A", data.Closed[4].Title)
	page = getHTMLBody(t, mux, "/console/tasks?closed=all&page=2")
	require.Contains(t, page, `<details class="ml-group tl-group tl-fold" id="tl-g-closed" data-group="closed" open>`)
	require.Contains(t, page, `<a class="ml-more" href="/console/tasks?closed=all#tl-g-closed" rel="prev">`)
	require.NotContains(t, page, `rel="next"`)
	require.Contains(t, page, `closed, page 2 of 2`)
	require.Contains(t, tlGroup(page, "closed"), `<span class="ml-note">5 of 30</span>`)
	require.Contains(t, page, `href="/console/tasks/`+data.Closed[0].ID+`?closed=all&amp;page=2"`, "a row keeps its page")
	page = getHTMLBody(t, mux, "/console/tasks?closed=all")
	require.Contains(t, page, `<a class="ml-more" href="/console/tasks?closed=all&amp;page=2#tl-g-closed" rel="next">`)
}

// A row says where its task stands: the holder and the lease of a claim and
// when it started; a lapsed claim, in the lapsed tone, and whose it was; the
// first blocker's title; who a step waits on; how long a task has been
// ready; and a ready task nothing changed on for 30 days says so.
func TestBkRowsSayWhereEachTaskStands(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	sess := bkSession(t, db, "example-ops", "cc/bk-holder")
	held := bkTask(t, db, "example-ops", "", "bk held", nil)
	_, err := store.ClaimTask(ctx, db, held, sess, 15*time.Minute, now.Add(-2*time.Hour))
	require.NoError(t, err)
	_, err = store.ClaimTask(ctx, db, held, sess, 3*time.Hour+30*time.Minute, now.Add(-time.Hour))
	require.NoError(t, err)
	lapsed := bkTask(t, db, "example-ops", "", "bk lapsed", nil)
	_, err = store.ClaimTask(ctx, db, lapsed, sess, time.Minute, now.Add(-time.Hour))
	require.NoError(t, err)
	first := bkTask(t, db, "example-ops", "", "bk the first blocker", func(k *core.Task) { k.CreatedAt = now.Add(-time.Hour) })
	second := bkTask(t, db, "example-ops", "", "bk the second blocker", nil)
	bkTask(t, db, "example-ops", "", "bk blocked twice", func(k *core.Task) { k.DependsOn = []string{second, first} })
	bkTask(t, db, "example-ops", "", "bk waits on hardware", func(k *core.Task) {
		k.Gate = core.TaskGate{Who: "hardware", What: "the board arrives"}
	})
	old := now.Add(-45 * 24 * time.Hour)
	bkTask(t, db, "example-ops", "", "bk old ready", func(k *core.Task) { k.CreatedAt, k.UpdatedAt = old, old })
	rec := events.NewRecorder(db)
	_, err = rec.Record(ctx, core.Event{Kind: core.EventTaskTransition, SessionID: sess, ProjectSlug: "example-ops", ItemID: held,
		TS: now.Add(-2 * time.Hour), Payload: map[string]any{"to": "in_progress", "claimed_by": sess}})
	require.NoError(t, err)
	_, err = rec.Record(ctx, core.Event{Kind: core.EventTaskTransition, SessionID: sess, ProjectSlug: "example-ops", ItemID: held,
		TS: now.Add(-time.Hour), Payload: map[string]any{"to": "in_progress", "claimed_by": sess}})
	require.NoError(t, err)

	data := bkPage(t, mux, "/console/tasks?format=json")
	require.Equal(t, taskCounts{Open: 7, InProgress: 1, Lapsed: 1, Ready: 3, Blocked: 1, Waiting: 1}, data.Counts)
	page := getHTMLBody(t, mux, "/console/tasks")

	row := tlRow(t, page, held)
	require.Contains(t, row, `cc/bk-holder &middot; <time `)
	require.Contains(t, row, `>lease 2h left</time>`)
	require.Contains(t, row, `>started 2h ago</time>`, "in progress since its first claim, the heartbeat continuing it")
	require.Equal(t, []string{held}, tlGroupIDs(page, "inprogress"))

	row = tlRow(t, page, lapsed)
	require.Contains(t, row, `data-word="claim-lapsed"`)
	require.Contains(t, row, `data-tone="danger"`)
	require.Contains(t, row, `>claim lapsed 59m ago</time> &middot; was cc/bk-holder`)
	require.Equal(t, []string{lapsed}, tlGroupIDs(page, "needs"), "no live claim holds it: not in progress")
	require.Contains(t, page, `>1 needs you</a>`)
	require.Contains(t, page, `<span class="live-dot"></span>1 in progress</a>`, "in progress counts the live claim alone")

	row = tlRowNamed(t, page, "bk blocked twice")
	require.Contains(t, row, `blocked by <a href="/console/tasks/`+first+`" data-first-blocker="`+first+`">bk the first blocker</a> (+1)`,
		"the oldest blocker first")

	row = tlRowNamed(t, page, "bk waits on hardware")
	require.Contains(t, row, `waits on hardware: the board arrives`)
	require.Contains(t, tlGroup(page, "waiting"), "bk waits on hardware")

	row = tlRowNamed(t, page, "bk old ready")
	require.Contains(t, row, `data-tone="warn"`)
	require.Contains(t, row, `>untouched 1mo</time>`, "nothing changed on it for 45 days")
	require.NotContains(t, tlRowNamed(t, page, "bk the second blocker"), "untouched")
	require.Contains(t, tlRowNamed(t, page, "bk the second blocker"), `>ready just now</time>`)
	require.Contains(t, page, "Oldest first, as agents claim")
	require.Less(t, strings.Index(page, "bk old ready"), strings.Index(page, "bk the second blocker"), "the queue's order")
}

// The reader's history lists a task's transitions, oldest first, each with
// the session that made it (named and linked) or the owner, and when; a
// holder's renewed claim reads once.
func TestBkHistoryListsTransitionsWithSessionNames(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	maker := bkSession(t, db, "example-ops", "cc/bk-maker")
	worker := bkSession(t, db, "example-ops", "cc/bk-worker")
	id := bkTask(t, db, "example-ops", "", "bk storied task", nil)
	rec := events.NewRecorder(db)
	at := func(m int) time.Time { return now.Add(time.Duration(m-60) * time.Minute) }
	for _, e := range []struct {
		session string
		m       int
		payload map[string]any
	}{
		{maker, 1, map[string]any{"to": "open", "created": true}},
		{worker, 2, map[string]any{"to": "in_progress", "claimed_by": worker}},
		{worker, 3, map[string]any{"to": "in_progress", "claimed_by": worker}},
		{worker, 4, map[string]any{"to": "in_progress", "claimed_by": worker}},
		{worker, 5, map[string]any{"to": "open", "released": true}},
		{"", 6, map[string]any{"to": "done", "from": "open", "by": "console"}},
		{"", 7, map[string]any{"to": "open", "from": "done", "by": "console"}},
		{"cc-gone", 8, map[string]any{"to": "open", "from": "open"}},
	} {
		_, err := rec.Record(ctx, core.Event{Kind: core.EventTaskTransition, SessionID: e.session, ProjectSlug: "example-ops",
			ItemID: id, TS: at(e.m), Payload: e.payload})
		require.NoError(t, err)
	}
	// The gate set through the console writes its own.
	_, _, msg := tkFlash(t, tkPost(t, mux, "/console/tasks/"+id+"/gate", url.Values{"who": {"owner"}, "what": {"approve"}}, false))
	require.Contains(t, msg, "Waits on owner now")

	var d taskDetail
	getJSON(t, mux, "/console/tasks/"+id+"?format=json", &d)
	verbs := make([]string, 0, len(d.History))
	for _, h := range d.History {
		verbs = append(verbs, h.Verb)
	}
	require.Equal(t, []string{"created", "claimed", "released", "done", "reopened", "updated", "gate set"}, verbs)
	require.Equal(t, &taskSession{ID: maker, Name: "cc/bk-maker"}, d.History[0].Who)
	require.Equal(t, &taskSession{ID: worker, Name: "cc/bk-worker"}, d.History[1].Who)
	require.Equal(t, 3, d.History[1].Times, "a renewed claim reads once")
	require.True(t, d.History[3].Owner)
	require.Nil(t, d.History[3].Who)
	require.Equal(t, &taskSession{Name: "cc-gone"}, d.History[5].Who, "a session the console cannot read keeps its text")
	require.True(t, d.History[6].Owner)

	for _, path := range []string{"/console/tasks/" + id, "/console/tasks/" + id + "?peek=1", "/console/tasks/" + id + "?reader=1"} {
		body := getHTMLBody(t, mux, path)
		strip := body[strings.Index(body, "data-history"):]
		strip = strip[:strings.Index(strip, "</section>")]
		require.Contains(t, strip, `<span class="th-who"><a class="mono" href="/console/sessions/`+maker+`">cc/bk-maker</a></span>`, path)
		require.Contains(t, strip, `<span class="th-times" title="3 in a row">&times;3</span>`, path)
		require.Contains(t, strip, `<span class="th-who owner" title="An owner action from this console">you</span>`, path)
		require.Contains(t, strip, `<span class="th-who"><span class="mono">cc-gone</span></span>`, path)
		require.Less(t, strings.Index(strip, ">created"), strings.Index(strip, ">gate set"), "%s: oldest first", path)
	}
	// A task with no recorded history renders none.
	quiet := bkTask(t, db, "example-ops", "", "bk quiet", nil)
	require.NotContains(t, getHTMLBody(t, mux, "/console/tasks/"+quiet+"?peek=1"), "data-history")
}

// Waits on someone sets a gate through the path tasks_update takes: the
// ready task moves to the people's rails, the event says the console set it,
// and the refusals are the task writes' own.
func TestBkGateSet_MovesAReadyTaskToWaitingOnPeople(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	id := bkTask(t, db, "example-ops", "", "bk future work", nil)
	dependent := bkTask(t, db, "example-ops", "", "bk after it", func(k *core.Task) { k.DependsOn = []string{id} })
	require.Equal(t, []string{"bk future work"}, bkTitles(bkPage(t, mux, "/console/tasks?format=json").Ready))
	body := getHTMLBody(t, mux, "/console/tasks/"+id)
	form := between(t, body, "data-task-wait", "<details", "</details>")
	require.Contains(t, form, `action="/console/tasks/`+id+`/gate"`)
	require.Contains(t, form, `name="who" required`)
	require.Contains(t, form, `name="what" required`)
	require.Contains(t, form, `data-live-skip`)

	next := "/console/tasks/" + id + "?project=example-ops"
	target, param, msg := tkFlash(t, tkPost(t, mux, "/console/tasks/"+id+"/gate", url.Values{
		"who": {"  owner "}, "what": {"decide\r\n before  release"}, "decision": {"which week?"}, "next": {next},
	}, false))
	require.Equal(t, "notice", param, msg)
	require.Equal(t, "Waits on owner now: it stays out of the ready queue until its gate is marked as handled.", msg)
	require.Equal(t, "/console/tasks/"+id, target.Path)
	require.Equal(t, "example-ops", target.Query().Get("project"), "the owner's filters are kept")

	got := tkTaskByID(t, db, id)
	require.Equal(t, core.TaskGate{Who: "owner", What: "decide before release", Decision: "which week?"}, got.Gate)
	require.Equal(t, core.TaskOpen, got.Status)
	evs := tkTransitions(t, db, id)
	require.Len(t, evs, 1)
	require.Equal(t, map[string]any{"to": "open", "from": "open", "by": "console", "gate": "set"}, evs[0].Payload)
	require.Empty(t, evs[0].Session, "a console write attributes no session")

	data := bkPage(t, mux, "/console/tasks?format=json")
	require.Empty(t, data.Ready, "it left the ready queue")
	require.Equal(t, []string{"bk future work"}, bkTitles(data.Gated), "it waits on people")
	require.Equal(t, []string{"bk after it"}, bkTitles(data.Blocked), "its dependent still waits on it")
	require.Equal(t, []string{"owner"}, data.Gated[0].WaitsOn())
	require.Equal(t, 1, data.Counts.Waiting)
	require.NotContains(t, getHTMLBody(t, mux, "/console/tasks/"+id), "data-task-wait", "an open gate is marked as handled, not set again")

	// The same gate again writes nothing; JSON callers get the task.
	_, param, msg = tkFlash(t, tkPost(t, mux, "/console/tasks/"+id+"/gate", url.Values{
		"who": {"owner"}, "what": {"decide before release"}, "decision": {"which week?"}}, false))
	require.Equal(t, "notice", param)
	require.Equal(t, "The task already waits on owner.", msg)
	require.Len(t, tkTransitions(t, db, id), 1)
	rr := tkPost(t, mux, "/console/tasks/"+dependent+"/gate", url.Values{"who": {"hardware"}, "what": {"the board"}}, true)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Equal(t, dependent, out["id"])

	// Refusals: who and what are both needed, the store's limits read in the
	// form's words, a closed task reopens first, and a live claim holds it.
	for _, tc := range []struct {
		form url.Values
		code int
		says string
	}{
		{url.Values{"what": {"x"}}, http.StatusBadRequest, "Say who the task waits on and what must happen"},
		{url.Values{"who": {"owner"}, "what": {"  "}}, http.StatusBadRequest, "Say who the task waits on and what must happen"},
		{url.Values{"who": {strings.Repeat("w", 101)}, "what": {"x"}}, http.StatusBadRequest, "The gate was not set: who is 101 characters; at most 100."},
	} {
		other := bkTask(t, db, "example-ops", "", "bk refused", nil)
		_, param, msg := tkFlash(t, tkPost(t, mux, "/console/tasks/"+other+"/gate", tc.form, false))
		require.Equal(t, "error", param)
		require.Contains(t, msg, tc.says)
		rr := tkPost(t, mux, "/console/tasks/"+other+"/gate", tc.form, true)
		require.Equal(t, tc.code, rr.Code, rr.Body.String())
		require.False(t, tkTaskByID(t, db, other).Gate.Set())
	}
	done := bkTask(t, db, "example-ops", "", "bk done", func(k *core.Task) { k.Status = core.TaskDone })
	_, param, msg = tkFlash(t, tkPost(t, mux, "/console/tasks/"+done+"/gate", url.Values{"who": {"owner"}, "what": {"x"}}, false))
	require.Equal(t, "error", param)
	require.Equal(t, "The task is done: reopen it first, then say what it waits on.", msg)
	require.Equal(t, http.StatusConflict, tkPost(t, mux, "/console/tasks/"+done+"/gate", url.Values{"who": {"owner"}, "what": {"x"}}, true).Code)

	sess := bkSession(t, db, "example-ops", "cc/bk-holder")
	held := bkTask(t, db, "example-ops", "", "bk held", nil)
	_, err := store.ClaimTask(ctx, db, held, sess, 15*time.Minute, time.Now().UTC())
	require.NoError(t, err)
	require.NotContains(t, getHTMLBody(t, mux, "/console/tasks/"+held), "data-task-wait", "a live claim holds it")
	_, param, msg = tkFlash(t, tkPost(t, mux, "/console/tasks/"+held+"/gate", url.Values{"who": {"owner"}, "what": {"x"}}, false))
	require.Equal(t, "error", param)
	require.Equal(t, "cc/bk-holder holds a live claim on this task: release the claim first, then say what it waits on.", msg)
	require.Equal(t, http.StatusConflict, tkPost(t, mux, "/console/tasks/"+held+"/gate", url.Values{"who": {"owner"}, "what": {"x"}}, true).Code)
	require.False(t, tkTaskByID(t, db, held).Gate.Set())
	require.Empty(t, tkTransitions(t, db, held))

	// A cleared gate opens again as set.
	cleared := bkTask(t, db, "example-ops", "", "bk cleared", func(k *core.Task) { k.Gate = core.TaskGate{Who: "owner", What: "old"} })
	_, err = store.UpdateTask(ctx, db, cleared, store.TaskPatch{ClearGate: &store.TaskGateClear{By: "owner"}}, "", time.Now().UTC())
	require.NoError(t, err)
	require.Contains(t, getHTMLBody(t, mux, "/console/tasks/"+cleared), "data-task-wait")
	tkFlash(t, tkPost(t, mux, "/console/tasks/"+cleared+"/gate", url.Values{"who": {"vendor"}, "what": {"ships"}}, false))
	require.True(t, tkTaskByID(t, db, cleared).Gate.Open())
	require.Equal(t, "vendor", tkTaskByID(t, db, cleared).Gate.Who)
}

// The gate set rides the console's write contracts: a cookie post must come
// from the console's own origin, the in-place client's post is answered 204
// with where to go, and -- like Mark done, unlike Mark as handled -- it is
// not the owner's hands alone: it names no one as having acted, and
// tasks_update gate_who is open to the same callers.
func TestBkGateSet_OriginFollowAndCallers(t *testing.T) {
	db, mux := newConsole(t)
	id := bkTask(t, db, "example-ops", "", "bk followed", nil)
	cross := httptest.NewRequest(http.MethodPost, "http://localhost:8081/console/tasks/"+id+"/gate", strings.NewReader("who=owner&what=x"))
	cross.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	cross.Header.Set("Sec-Fetch-Site", "same-site")
	cross.AddCookie(consoleCookie())
	require.Equal(t, http.StatusForbidden, do(mux, cross).Code)
	require.False(t, tkTaskByID(t, db, id).Gate.Set())

	req := httptest.NewRequest(http.MethodPost, "http://localhost:8081/console/tasks/"+id+"/gate",
		strings.NewReader("who=owner&what=x&next="+url.QueryEscape("/console/tasks?project=example-ops")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://localhost:8081")
	req.Header.Set("X-Seam-Follow", "1")
	req.AddCookie(consoleCookie())
	rr := do(mux, req)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	require.Equal(t, "/console/tasks?project=example-ops&notice=Waits+on+owner+now%3A+it+stays+out+of+the+ready+queue+until+its+gate+is+marked+as+handled.",
		rr.Header().Get("X-Seam-Location"))
	require.True(t, tkTaskByID(t, db, id).Gate.Open())

	for _, c := range uohCallers(t) {
		t.Run(c.name, func(t *testing.T) {
			db, mux := newConsole(t)
			a := bkTask(t, db, "example-ops", "", "bk status", nil)
			b := bkTask(t, db, "example-ops", "", "bk gate", nil)
			status := uohPost(t, mux, c, "/console/tasks/"+a+"/status", "to=done")
			gate := uohPost(t, mux, c, "/console/tasks/"+b+"/gate", "who=owner&what=x")
			require.Equal(t, status.Code, gate.Code, "%s: the gate set answers as Mark done does: %s", c.name, gate.Body.String())
			require.Equal(t, tkTaskByID(t, db, a).Status == core.TaskDone, tkTaskByID(t, db, b).Gate.Set(), c.name)
		})
	}
}

// The reader's actions and the star return to the task with the page's
// filters, and the list's sort keeps them.
func TestBkReaderKeepsTheFilters(t *testing.T) {
	db, mux := newConsole(t)
	id := bkTask(t, db, "example-ops", "", "bk kept", nil)
	next := `name="next" value="/console/tasks/` + id + `?project=example-ops&amp;sort=newest"`
	for _, path := range []string{"/console/tasks/" + id + "?project=example-ops&sort=newest", "/console/tasks/" + id + "?reader=1&project=example-ops&sort=newest"} {
		body := getHTMLBody(t, mux, path)
		require.Contains(t, between(t, body, `data-task-act="done"`, "<form", "</form>"), next, path)
		require.Contains(t, between(t, body, "star-form", "<form", "</form>"), next, path)
		require.Contains(t, between(t, body, "data-task-wait", "<details", "</details>"), next, path)
	}
	// The reader's star is the one library.js follows after a swap.
	reader := getHTMLBody(t, mux, "/console/tasks/"+id+"?reader=1&project=example-ops&sort=newest")
	require.Contains(t, between(t, reader, "star-form", "<form", "</form>"), next+" data-lib-next")
	// The list's sort keeps its filters.
	list := getHTMLBody(t, mux, "/console/tasks?project=example-ops")
	require.Contains(t, list, `<a class="" href="/console/tasks?project=example-ops&amp;sort=newest">Newest first</a>`)
	require.Contains(t, list, `<a class="on" href="/console/tasks?project=example-ops">Oldest first</a>`)
	// The default list carries no state, so the actions keep their own way back.
	page := getHTMLBody(t, mux, "/console/tasks/"+id)
	require.NotContains(t, between(t, page, `data-task-act="done"`, "<form", "</form>"), `name="next"`)
}

// Every read the backlog adds goes through the request's handle: a locked
// project's tasks are absent from the filters, the project menu and the
// counts, its plan and project read as nothing here, and a history entry its
// session made is absent with that session.
func TestBkLockedProjectIsAbsentFromTheBacklog(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.CreateTask(ctx, lc.db, core.Task{ID: mustID(t), ProjectSlug: lockedSlug, PlanSlug: lc.plan,
		Title: "ledger step " + lockedSecret, Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}))
	open := mustID(t)
	require.NoError(t, store.CreateTask(ctx, lc.db, core.Task{ID: open, ProjectSlug: openSlug, Title: "ops task",
		Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}))
	for _, e := range []struct {
		session string
		payload map[string]any
	}{
		{"", map[string]any{"to": "open", "created": true}},
		{lc.sessID, map[string]any{"to": "in_progress", "claimed_by": lc.sessID}},
		{"", map[string]any{"to": "open", "released": true, "by": "console"}},
	} {
		_, err := lc.rec.Record(ctx, core.Event{Kind: core.EventTaskTransition, SessionID: e.session, ProjectSlug: openSlug,
			ItemID: open, Payload: e.payload})
		require.NoError(t, err)
	}
	get := func(path string, want int) string {
		t.Helper()
		for _, asJSON := range []bool{false, true} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer "+testKey)
			if asJSON {
				req.Header.Set("Accept", "application/json")
			}
			rr := lc.do(req)
			require.Equal(t, want, rr.Code, "%s: %s", path, rr.Body.String())
			require.NotContains(t, rr.Body.String(), lockedSecret, path)
			require.NotContains(t, rr.Body.String(), "reconcile", "%s: no locked title", path)
			require.NotContains(t, rr.Body.String(), "cc/ledger", "%s: no locked session", path)
			if !asJSON {
				return rr.Body.String()
			}
		}
		return ""
	}
	// The text filters name part of the locked title: the page echoes the
	// owner's own text, and must find nothing for it.
	for _, path := range []string{"/console/tasks?steps=1", "/console/tasks?q=reconc&steps=1", "/console/tasks?q=LEDGER-SECRET",
		"/console/tasks?closed=all&steps=1", "/console/tasks/" + open + "?steps=1"} {
		page := get(path, http.StatusOK)
		require.NotContains(t, page, `href="/console/tasks?project=`+lockedSlug, "%s: the menu leaves the locked project out", path)
	}
	var data tasksData
	req := httptest.NewRequest(http.MethodGet, "/console/tasks?format=json&steps=1", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := lc.do(req)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &data))
	require.Equal(t, []taskProjectCount{{openSlug, 1}}, data.Projects)
	require.Equal(t, 1, data.Counts.Open)

	body := get("/console/tasks?project="+lockedSlug, http.StatusBadRequest)
	require.Contains(t, body, "it is locked, and its tasks stay out of this view until this browser unlocks it")
	body = get("/console/tasks?plan="+lc.plan, http.StatusBadRequest)
	require.Contains(t, body, "no task here is a step of that plan")

	var d taskDetail
	getJSONVia(t, lc, "/console/tasks/"+open+"?format=json", &d)
	verbs := make([]string, 0, len(d.History))
	for _, h := range d.History {
		verbs = append(verbs, h.Verb)
	}
	require.Equal(t, []string{"created", "released"}, verbs, "the locked session's claim is absent with its session")
	get("/console/tasks/"+lc.taskID, http.StatusNotFound)

	// The gate set on a locked project's task is not found, and changes nothing.
	for _, asJSON := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodPost, "/console/tasks/"+lc.taskID+"/gate", strings.NewReader("who=owner&what=probe"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+testKey)
		if asJSON {
			req.Header.Set("Accept", "application/json")
		}
		rr := lc.do(req)
		require.Equal(t, http.StatusNotFound, rr.Code, rr.Body.String())
		require.NotContains(t, rr.Body.String(), lockedSecret)
	}
	locked, err := store.TaskByID(ctx, lc.db, lc.taskID)
	require.NoError(t, err)
	require.False(t, locked.Gate.Set())

	// A browser that unlocked the project reads it, filters and all.
	cookie := lc.unlockByTerminal(t)
	breq := lc.browser(http.MethodGet, "/console/tasks?format=json&project="+lockedSlug, cookie, nil)
	breq.Header.Set("Accept", "application/json")
	rr = lc.do(breq)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Contains(t, rr.Body.String(), "reconcile")
}

// getJSONVia is getJSON against a locked console.
func getJSONVia(t *testing.T, lc *lockedConsole, path string, v any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Accept", "application/json")
	rr := lc.do(req)
	require.Equal(t, http.StatusOK, rr.Code, "GET %s: %s", path, rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), v))
}

// rowSince reads each state's start from the record, and falls back on the
// row's own times.
func TestBkRowSince(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 11, 1, h, 0, 0, 0, time.UTC) }
	cleared := at(6)
	closed := at(9)
	for _, tc := range []struct {
		name string
		row  taskRow
		s    store.TaskSince
		want time.Time
	}{
		{"ready since created", taskRow{Status: "open", Created: at(1)}, store.TaskSince{}, at(1)},
		{"ready since reopened", taskRow{Status: "open", Created: at(1)}, store.TaskSince{Status: at(3)}, at(3)},
		{"ready since its blocker closed", taskRow{Status: "open", Created: at(1)}, store.TaskSince{Status: at(1), BlockersClosed: at(5)}, at(5)},
		{"ready since its gate cleared", taskRow{Status: "open", Created: at(1), Gate: &taskGateView{ClearedAt: &cleared}}, store.TaskSince{}, at(6)},
		{"blocked keeps the open run", taskRow{Status: "open", Created: at(1), Blockers: []blocker{{ID: "b"}}}, store.TaskSince{Status: at(2), BlockersClosed: at(5)}, at(2)},
		{"waiting since the gate set", taskRow{Status: "open", Created: at(1), Gate: &taskGateView{Open: true}}, store.TaskSince{Status: at(2), GateSet: at(4)}, at(4)},
		{"in progress since the run began", taskRow{Status: "in_progress", Created: at(1), Updated: at(8)}, store.TaskSince{Status: at(7)}, at(7)},
		{"in progress with no record", taskRow{Status: "in_progress", Created: at(1), Updated: at(8)}, store.TaskSince{}, at(8)},
		{"closed", taskRow{Status: "done", Closed: &closed}, store.TaskSince{Status: at(2)}, at(9)},
	} {
		require.Equal(t, tc.want, rowSince(tc.row, tc.s), tc.name)
	}
}

// The history's words for the gate's own moves, and the ledger's for the rest.
func TestBkHistoryVerb(t *testing.T) {
	for _, tc := range []struct {
		payload map[string]any
		verb    string
	}{
		{map[string]any{"to": "open", "created": true, "gate": "set"}, "created"},
		{map[string]any{"to": "open", "from": "open", "gate": "set"}, "gate set"},
		{map[string]any{"to": "done", "from": "open", "gate": "cleared", "by": "console"}, "gate cleared"},
		{map[string]any{"to": "open", "from": "open", "gate": "removed"}, "gate removed"},
		{map[string]any{"to": "in_progress", "claimed_by": "s"}, "claimed"},
		{map[string]any{"to": "open", "from": "done"}, "reopened"},
	} {
		verb, _, _ := historyVerb(tc.payload)
		require.Equal(t, tc.verb, verb, "%v", tc.payload)
	}
}
