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
	"github.com/arctop/seamless/internal/store"
)

// A task a locked project blocks (peer_auth, owner decision D7). The
// console's handle hides the locked project's tasks and their dependency
// edges, so before this the owner's Tasks screen listed such a task as READY
// -- claimable -- with no word of what blocks it. Now it sits in the blocked
// rail, its locked blockers recorded by project: the owner's surface lists
// locked projects by name, and their content stays out.
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
	item := between(t, page, "ops waits on ledger", `<a class="rail-item`, "</a>")
	require.Contains(t, item, "blocked by 1")
	require.Contains(t, item, `data-locked-blocker="`+lockedSlug+`"`)
	require.Contains(t, item, "a task in locked project "+lockedSlug+" blocks this one")
	require.Contains(t, between(t, page, "ops waits on both", `<a class="rail-item`, "</a>"), "blocked by 2")

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
	require.Contains(t, page, "blocked by 1")
	require.NotContains(t, page, "data-locked-blocker")
}

// The project workspace's Plans & tasks tab reads through the same handle:
// a plan step a locked project blocks is blocked there too, named by its
// locked project, and an off-plan task it blocks leaves the ready queue.
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
	require.NotContains(t, panel, "ops off-plan waits on ledger", "a blocked task is not claimable")
	require.Contains(t, panel, "ops off-plan free")

	// Unlocked, the browser reads the blocker itself: an ordinary chain chip.
	cookie := lc.unlockByTerminal(t)
	rr := lc.do(lc.browser(http.MethodGet, "/console/projects/"+openSlug+"?tab=tasks", cookie, nil))
	require.Equal(t, http.StatusOK, rr.Code)
	panel = tabPanel(t, rr.Body.String(), "tasks")
	require.NotContains(t, panel, "data-locked-blocker")
	require.Contains(t, between(t, panel, step, `<details class="step`, "</summary>"), `<details class="step blocked"`)
}

// The task's own reader and peek name the locked blocker under "Depends on"
// (D7), as the Tasks rail does; the rail gives it a line of its own, so the
// truncated meta line never hides it. Unlocked, the browser reads the
// blocker itself.
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

	item := between(t, get("/console/tasks"), "ops waits on ledger", `<a class="rail-item`, "</a>")
	require.Contains(t, item, `<span class="ri-desc" data-locked-blocker="`+lockedSlug+`"`, "a line of its own")

	cookie := lc.unlockByTerminal(t)
	rr := lc.do(lc.browser(http.MethodGet, "/console/tasks/"+id+"?reader=1", cookie, nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.NotContains(t, rr.Body.String(), "data-locked-blocker")
	require.Contains(t, rr.Body.String(), lc.taskID, "unlocked, the blocker itself")
}
