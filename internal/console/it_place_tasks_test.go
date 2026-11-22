package console

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// itTaskOrder is the Tasks list q names as the page draws it, every row of
// it (the list drawn whole: each group draws all it holds), in order.
func itTaskOrder(t *testing.T, svc *Service, q taskQuery) []string {
	t.Helper()
	q.Page, q.ReadyAll = max(q.Page, 1), true
	data, err := svc.tasksPage(context.Background(), q)
	require.NoError(t, err)
	var order []string
	for _, g := range data.List.Groups {
		require.Len(t, g.Rows, g.Total, "%+v: the %s group is drawn whole", q, g.Key)
		for _, r := range g.Rows {
			order = append(order, r.ID)
		}
	}
	return order
}

// A task's place is where the Tasks list draws it, for every sort and filter
// (itAgree, it_place_test.go): Needs you (plan steps included whatever the
// steps toggle says), In progress, Ready, Blocked (a step only the owner's
// gates hold among them), Waiting on others, and the tasks closed in the
// last week, or every closed task with closed=all.
func TestItPlaceTasksAgreeWithTheList(t *testing.T) {
	db, _, svc, _ := itFixture(t, "advanced")
	ctx := context.Background()
	base := time.Now().UTC().Add(-24 * time.Hour)
	libSession(t, db, "S1", "cc/s1", "example-pay", base)
	now := time.Now().UTC()
	live, lapsed := now.Add(10*time.Minute), now.Add(-10*time.Minute)
	mk := func(title, plan string, status core.TaskStatus, at time.Duration, mut func(*core.Task)) core.Task {
		task := core.Task{ID: mustID(t), ProjectSlug: "example-ops", PlanSlug: plan, Title: title, Status: status,
			CreatedAt: base.Add(at), UpdatedAt: base.Add(at)}
		if status.Closed() {
			closed := base.Add(at + time.Minute)
			task.ClosedAt = &closed
		}
		if mut != nil {
			mut(&task)
		}
		require.NoError(t, store.CreateTask(ctx, db, task))
		return task
	}
	ready := mk("ready one", "", core.TaskOpen, 1*time.Hour, nil)
	mk("ready two", "", core.TaskOpen, 2*time.Hour, nil)
	mk("blocked", "", core.TaskOpen, 3*time.Hour, func(t *core.Task) { t.DependsOn = []string{ready.ID} })
	gate := mk("the gate", "", core.TaskOpen, 4*time.Hour, func(t *core.Task) {
		t.Kind, t.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "sign"}
	})
	mk("behind the gate", "", core.TaskOpen, 5*time.Hour, func(t *core.Task) { t.DependsOn = []string{gate.ID} })
	mk("its own gate", "", core.TaskOpen, 6*time.Hour, func(t *core.Task) { t.Gate = core.TaskGate{Who: "alice", What: "x"} })
	mk("its own gate on the owner", "", core.TaskOpen, 6*time.Hour+30*time.Minute, func(t *core.Task) {
		t.Gate = core.TaskGate{Who: "Owner: the release", What: "y"}
	})
	mk("held", "", core.TaskInProgress, 7*time.Hour, func(t *core.Task) { t.ClaimedBy, t.LeaseExpiresAt = "S1", &live })
	mk("lapsed", "", core.TaskInProgress, 8*time.Hour, func(t *core.Task) { t.ClaimedBy, t.LeaseExpiresAt = "S1", &lapsed })
	mk("claimless", "", core.TaskInProgress, 9*time.Hour, nil)
	mk("done one", "", core.TaskDone, 10*time.Hour, nil)
	mk("dropped one", "", core.TaskDropped, 11*time.Hour, nil)
	mk("closed long ago", "", core.TaskDone, -30*24*time.Hour, nil)
	mk("a plan step", "ops-plan", core.TaskOpen, 12*time.Hour, nil)
	mk("a lapsed plan step", "ops-plan", core.TaskInProgress, 12*time.Hour+30*time.Minute, func(t *core.Task) {
		t.ClaimedBy, t.LeaseExpiresAt = "S1", &lapsed
	})
	planGate := mk("a plan's gate on the owner", "ops-plan", core.TaskOpen, 12*time.Hour+45*time.Minute, func(t *core.Task) {
		t.Kind, t.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "approve"}
	})
	mk("a plan step behind it", "ops-plan", core.TaskOpen, 12*time.Hour+50*time.Minute, func(t *core.Task) {
		t.DependsOn = []string{planGate.ID}
	})
	other := mk("another gate", "", core.TaskOpen, 13*time.Hour, func(t *core.Task) {
		t.Kind, t.Gate = core.TaskKindGate, core.TaskGate{Who: "bob", What: "y"}
	})
	mk("behind both gates", "", core.TaskOpen, 14*time.Hour, func(t *core.Task) { t.DependsOn = []string{gate.ID, other.ID} })

	for _, q := range []taskQuery{
		{Sort: taskSortOldest}, {Sort: taskSortNewest}, {Sort: taskSortOldest, Steps: true},
		{Sort: taskSortNewest, Steps: true}, {Sort: taskSortOldest, Plan: "ops-plan"},
		{Sort: taskSortNewest, Project: "example-ops", Q: "one"}, {Sort: taskSortOldest, Q: "plan"},
		{Sort: taskSortOldest, ClosedAll: true}, {Sort: taskSortNewest, Steps: true, ClosedAll: true},
	} {
		order := itTaskOrder(t, svc, q)
		now := time.Now().UTC()
		itAgree(t, fmt.Sprintf("tasks %+v", q), order, func(id string) (store.ItemPlace, bool, error) {
			return store.TaskPlace(ctx, svc.db(ctx), q.placeList(now, nil), now, id)
		})
	}

	// The default list: what needs the owner first, a plan's steps among it.
	order := itTaskOrder(t, svc, taskQuery{Sort: taskSortOldest})
	titles := make([]string, 0, len(order))
	for _, id := range order {
		task, err := store.TaskByID(ctx, db, id)
		require.NoError(t, err)
		titles = append(titles, task.Title)
	}
	require.Equal(t, []string{
		"the gate", "its own gate on the owner", "a plan's gate on the owner", "lapsed", "a lapsed plan step", "claimless",
		"held", "ready one", "ready two", "blocked", "behind the gate", "another gate", "its own gate", "behind both gates",
		"dropped one", "done one",
	}, titles, "closed long ago is past the last week, and the plan steps that wait on no one stay out")
}

// Under a lock, the place puts a task a locked project's task blocks where
// the list does, among the blocked, though the request's handle reads it as
// ready: the item page's place and steps follow the list.
func TestTlPlaceUnderALock(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Hour)
	mk := func(title string, deps ...string) string {
		id := mustID(t)
		require.NoError(t, store.CreateTask(ctx, lc.db, core.Task{ID: id, ProjectSlug: openSlug, Title: title,
			Status: core.TaskOpen, DependsOn: deps, CreatedAt: now, UpdatedAt: now}))
		now = now.Add(time.Second)
		return id
	}
	first := mk("ops first")
	waits := mk("ops waits on ledger", lc.taskID)
	last := mk("ops last")
	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr := lc.do(req)
		require.Equal(t, http.StatusOK, rr.Code, path)
		require.NotContains(t, rr.Body.String(), lockedSecret, path)
		return rr.Body.String()
	}
	list := get("/console/tasks?project=" + openSlug)
	require.Equal(t, []string{first, last}, tlGroupIDs(list, "ready"))
	require.Equal(t, []string{waits}, tlGroupIDs(list, "blocked"))
	for i, step := range []struct{ id, pos, prev, next string }{
		{first, "1 of 3", "", last},
		{last, "2 of 3", first, waits},
		{waits, "3 of 3", last, ""},
	} {
		pos, prev, next := itPlace(t, itFrame(t, get("/console/tasks/"+step.id+"?project="+openSlug)))
		require.True(t, strings.HasPrefix(pos, step.pos), "%d: %s", i, pos)
		for _, link := range []struct{ got, want string }{{prev, step.prev}, {next, step.next}} {
			if link.want == "" {
				require.Empty(t, link.got, "%d", i)
				continue
			}
			require.Equal(t, "/console/tasks/"+link.want+"?project="+openSlug, link.got, "%d", i)
		}
	}
}
