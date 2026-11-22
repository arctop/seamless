package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// A task a locked project blocks (peer_auth, owner decision D7). The
// console's handle hides the locked project's tasks and their dependency
// edges, so before this the owner's Tasks screen listed such a task as READY
// -- claimable -- with no word of what blocks it. Now it sits in the blocked
// rail and the list's Blocked group, its locked blockers recorded by
// project: the owner's surface lists locked projects by name, and their
// content stays out.
func TestTasks_LockedBlockersNamed(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	newTask := func(title string, deps ...string) string {
		id := mustID(t)
		require.NoError(t, store.CreateTask(ctx, lc.db, core.Task{ID: id, ProjectSlug: openSlug, Title: title,
			Status: core.TaskOpen, DependsOn: deps, CreatedAt: now, UpdatedAt: now}))
		now = now.Add(time.Second)
		return id
	}
	prep := newTask("ops prep")
	waits := newTask("ops waits on ledger", lc.taskID)
	both := newTask("ops waits on both", lc.taskID, prep)
	free := newTask("ops free")

	type row struct {
		ID             string   `json:"id"`
		Blockers       []any    `json:"blockers"`
		LockedBlockers []string `json:"lockedBlockers"`
	}
	var data struct {
		Ready   []row `json:"ready"`
		Blocked []row `json:"blocked"`
	}
	req := httptest.NewRequest(http.MethodGet, "/console/tasks?format=json", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := lc.do(req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.NotContains(t, rr.Body.String(), lockedSecret)
	require.NotContains(t, rr.Body.String(), lc.taskID, "the locked blocker's id stays out")
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &data))
	ids := func(rows []row) map[string]row {
		out := map[string]row{}
		for _, r := range rows {
			out[r.ID] = r
		}
		return out
	}
	ready, blocked := ids(data.Ready), ids(data.Blocked)
	require.Contains(t, ready, prep)
	require.Contains(t, ready, free)
	require.NotContains(t, ready, waits, "a task the locked project blocks is not claimable")
	require.Equal(t, []string{lockedSlug}, blocked[waits].LockedBlockers)
	require.Empty(t, blocked[waits].Blockers)
	require.Equal(t, []string{lockedSlug}, blocked[both].LockedBlockers)
	require.Len(t, blocked[both].Blockers, 1, "the readable blocker is still named")

	hreq := httptest.NewRequest(http.MethodGet, "/console/tasks", nil)
	hreq.Header.Set("Authorization", "Bearer "+testKey)
	hrr := lc.do(hreq)
	require.Equal(t, http.StatusOK, hrr.Code)
	page := hrr.Body.String()
	require.NotContains(t, page, lockedSecret)
	require.Equal(t, []string{waits, both}, tlGroupIDs(page, "blocked"))
	item := tlRow(t, page, waits)
	require.NotContains(t, item, "blocked by <a", "no readable blocker to name")
	require.Contains(t, item, `data-locked-blocker="`+lockedSlug+`"`)
	require.Contains(t, item, "a task in locked project "+lockedSlug+" blocks this one")
	item = tlRow(t, page, both)
	require.Contains(t, item, `blocked by <a href="/console/tasks/`+prep+`" data-first-blocker="`+prep+`">ops prep</a> &middot; <span data-locked-blocker="`+lockedSlug+`"`,
		"the readable blocker first, then the locked one")
	require.NotContains(t, item, "(+", "the locked blocker is named, not counted again")

	// A browser that unlocked the project reads the blocker itself.
	cookie := lc.unlockByTerminal(t)
	breq := lc.browser(http.MethodGet, "/console/tasks?format=json", cookie, nil)
	breq.Header.Set("Accept", "application/json")
	rr = lc.do(breq)
	require.Equal(t, http.StatusOK, rr.Code)
	require.NotContains(t, rr.Body.String(), "lockedBlockers")
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &data))
	require.Len(t, ids(data.Blocked)[waits].Blockers, 1)
}

// With nothing locked the Tasks screen is as it was: no locked key, no line.
func TestTasks_NoLockedBlockersWithoutLocks(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	a := mustID(t)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: a, ProjectSlug: "example-ops", Title: "prep",
		Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: mustID(t), ProjectSlug: "example-ops", Title: "waits",
		Status: core.TaskOpen, DependsOn: []string{a}, CreatedAt: now, UpdatedAt: now}))
	require.NotContains(t, getPeek(t, mux, "/console/tasks?format=json").Body.String(), "lockedBlockers")
	page := getHTMLBody(t, mux, "/console/tasks")
	require.Contains(t, tlRowNamed(t, page, "waits"), `blocked by <a href="/console/tasks/`+a+`" data-first-blocker="`+a+`">prep</a>`)
	require.NotContains(t, page, "data-locked-blocker")
}

// The project workspace's Plans & tasks tab reads through the same handle:
// a plan step a locked project blocks is blocked there too, named by its
// locked project, and an off-plan task it blocks is listed under Blocked,
// never as ready.
func TestProjectTasksTab_LockedBlockersNamed(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	newTask := func(title, plan string, deps ...string) string {
		id := mustID(t)
		require.NoError(t, store.CreateTask(ctx, lc.db, core.Task{ID: id, ProjectSlug: openSlug, Title: title, PlanSlug: plan,
			Status: core.TaskOpen, DependsOn: deps, CreatedAt: now, UpdatedAt: now}))
		now = now.Add(time.Second)
		return id
	}
	free := newTask("ops step free", "ops-plan")
	step := newTask("ops step waits on ledger", "ops-plan", lc.taskID)
	newTask("ops off-plan waits on ledger", "", lc.taskID)
	newTask("ops off-plan free", "")

	get := func(t *testing.T) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/console/projects/"+openSlug+"?tab=tasks", nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr := lc.do(req)
		require.Equal(t, http.StatusOK, rr.Code)
		require.NotContains(t, rr.Body.String(), lockedSecret)
		require.NotContains(t, rr.Body.String(), lc.taskID)
		return tabPanel(t, rr.Body.String(), "tasks")
	}
	panel := get(t)
	blocked := between(t, panel, step, `<details class="step`, "</summary>")
	require.Contains(t, blocked, `<details class="step blocked"`)
	require.Contains(t, blocked, `data-locked-blocker="`+lockedSlug+`"`)
	require.Contains(t, blocked, "a task in locked project "+lockedSlug+" blocks this one")
	require.Contains(t, between(t, panel, free, `<details class="step`, "</summary>"), `<details class="step open ready"`)
	require.Contains(t, panel, `<b class="ready-n">1 ready</b>`, "the plan's rollup counts only the free step")
	offBlocked := between(t, panel, "ops off-plan waits on ledger", `<details class="step`, "</summary>")
	require.Contains(t, offBlocked, `<details class="step blocked"`, "a blocked task is not claimable")
	require.Contains(t, offBlocked, "a task in locked project "+lockedSlug+" blocks this one")
	ready := panel[strings.Index(panel, `id="offplan-ready"`):strings.Index(panel, `id="offplan-blocked"`)]
	require.NotContains(t, ready, "ops off-plan waits on ledger", "it is listed under Blocked, never as ready")
	require.Contains(t, ready, "ops off-plan free")

	// Unlocked, the browser reads the blocker itself: an ordinary chain chip.
	cookie := lc.unlockByTerminal(t)
	rr := lc.do(lc.browser(http.MethodGet, "/console/projects/"+openSlug+"?tab=tasks", cookie, nil))
	require.Equal(t, http.StatusOK, rr.Code)
	panel = tabPanel(t, rr.Body.String(), "tasks")
	require.NotContains(t, panel, "data-locked-blocker")
	require.Contains(t, between(t, panel, step, `<details class="step`, "</summary>"), `<details class="step blocked"`)
}

// The task's own reader and peek name the locked blocker under "Depends on"
// (D7), as the Tasks list does on the task's row. Unlocked, the browser
// reads the blocker itself.
func TestTaskDetail_LockedBlockerNamed(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id := mustID(t)
	require.NoError(t, store.CreateTask(ctx, lc.db, core.Task{ID: id, ProjectSlug: openSlug, Title: "ops waits on ledger",
		Status: core.TaskOpen, DependsOn: []string{lc.taskID}, CreatedAt: now, UpdatedAt: now}))
	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr := lc.do(req)
		require.Equal(t, http.StatusOK, rr.Code, path)
		body := rr.Body.String()
		require.NotContains(t, body, lockedSecret, path)
		require.NotContains(t, body, lc.taskID, "%s: the locked blocker's id stays out", path)
		return body
	}
	for _, path := range []string{"/console/tasks/" + id + "?reader=1", "/console/tasks/" + id + "?peek=1"} {
		body := get(path)
		at := strings.Index(body, "Depends on")
		require.GreaterOrEqual(t, at, 0, path)
		deps, _, _ := strings.Cut(body[at:], "</ul>")
		require.Contains(t, deps, `1</span>`, path)
		require.Contains(t, deps, `data-locked-blocker="`+lockedSlug+`"`, path)
		require.Contains(t, deps, `title="Unlock `+lockedSlug+` in the console to see the task"`, path)
		require.Contains(t, deps, "a task in locked project "+lockedSlug+" blocks this one", path)
	}
	var d struct {
		LockedBlockers []string `json:"lockedBlockers"`
		Deps           []any    `json:"deps"`
	}
	require.NoError(t, json.Unmarshal([]byte(get("/console/tasks/"+id+"?format=json")), &d))
	require.Equal(t, []string{lockedSlug}, d.LockedBlockers)
	require.Empty(t, d.Deps)

	item := tlRow(t, get("/console/tasks"), id)
	require.Contains(t, item, `<span data-locked-blocker="`+lockedSlug+`" title="Unlock `+lockedSlug+` in the console to see the task">`)

	cookie := lc.unlockByTerminal(t)
	rr := lc.do(lc.browser(http.MethodGet, "/console/tasks/"+id+"?reader=1", cookie, nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.NotContains(t, rr.Body.String(), "data-locked-blocker")
	require.Contains(t, rr.Body.String(), lc.taskID, "unlocked, the blocker itself")
}

// TestRwLockedProjectIsAbsentFromTaskWords covers the reads the task words
// add, each through the request's handle: a holder or creator in a locked
// project resolves to no name and no link, and a blocker there makes its
// dependent read blocked on the reader, the project tab and the Plans
// screen while its content stays out.
func TestRwLockedProjectIsAbsentFromTaskWords(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const plan = "rw-ops-plan"
	_, err := lc.mgr.WriteNote(ctx, core.Note{ID: mustID(t), Slug: "rw-ops-narrative", Title: "Ops plan", Project: openSlug,
		Description: "the ops plan", Body: "# Ops", Tags: []string{plans.SlugTag(plan), "created-by:agent"}, Created: now, Updated: now})
	require.NoError(t, err)
	held := mustID(t)
	require.NoError(t, store.CreateTask(ctx, lc.db, core.Task{ID: held, ProjectSlug: openSlug, Title: "ops held by a locked session",
		Status: core.TaskOpen, PlanSlug: plan, CreatedBy: "cc/ledger", CreatedAt: now, UpdatedAt: now}))
	_, err = store.ClaimTask(ctx, lc.db, held, lc.sessID, 15*time.Minute, now)
	require.NoError(t, err)
	waits := mustID(t)
	require.NoError(t, store.CreateTask(ctx, lc.db, core.Task{ID: waits, ProjectSlug: openSlug, Title: "ops waits on the ledger",
		Status: core.TaskOpen, PlanSlug: plan, DependsOn: []string{lc.taskID}, CreatedAt: now, UpdatedAt: now}))

	get := func(path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr := lc.do(req)
		require.Equal(t, http.StatusOK, rr.Code, path)
		body := rr.Body.String()
		require.NotContains(t, body, lockedSecret, path)
		require.NotContains(t, body, `href="/console/sessions/`+lc.sessID+`"`, "%s links to a session the request cannot read", path)
		return body
	}
	reader := get("/console/tasks/" + held)
	require.NotContains(t, reader, ">cc/ledger</a>", "the locked session's name is not resolved into a link")
	require.Contains(t, reader, `data-word="in-progress"`, "the claim is live whoever holds it")
	peek := get("/console/tasks/" + waits + "?peek=1")
	require.Contains(t, peek, `data-word="blocked"`, "a blocker in a locked project blocks")
	require.Contains(t, peek, `data-locked-blocker="`+lockedSlug+`"`)

	tab := get("/console/projects/" + openSlug + "?tab=tasks")
	row := tab[strings.Index(tab, `id="`+waits+`">`):]
	row = row[:strings.Index(row, "</summary>")]
	require.Contains(t, row, `data-word="blocked"`)
	require.NotContains(t, tab, ">cc/ledger<", "the holder of a step resolves through the request's handle")

	plansPage := get("/console/plans/" + plan + "?peek=1")
	at := strings.Index(plansPage, `href="/console/tasks/`+waits+`"`)
	require.GreaterOrEqual(t, at, 0)
	step := plansPage[at:]
	step = step[:strings.Index(step, "</tr>")]
	require.Contains(t, step, `data-word="blocked"`, "the Plans screen counts the locked blocker too")
	require.NotContains(t, plansPage, lc.taskID, "the locked blocker's id stays out")
}
