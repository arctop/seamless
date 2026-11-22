package console

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// A task's place is where the Tasks list draws it, for every sort and filter
// (itAgree, it_place_test.go).
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
	mk("held", "", core.TaskInProgress, 7*time.Hour, func(t *core.Task) { t.ClaimedBy, t.LeaseExpiresAt = "S1", &live })
	mk("lapsed", "", core.TaskInProgress, 8*time.Hour, func(t *core.Task) { t.ClaimedBy, t.LeaseExpiresAt = "S1", &lapsed })
	mk("claimless", "", core.TaskInProgress, 9*time.Hour, nil)
	mk("done one", "", core.TaskDone, 10*time.Hour, nil)
	mk("dropped one", "", core.TaskDropped, 11*time.Hour, nil)
	mk("a plan step", "ops-plan", core.TaskOpen, 12*time.Hour, nil)
	mk("another gate", "", core.TaskOpen, 13*time.Hour, func(t *core.Task) {
		t.Kind, t.Gate = core.TaskKindGate, core.TaskGate{Who: "bob", What: "y"}
	})
	for _, q := range []taskQuery{
		{Sort: taskSortOldest}, {Sort: taskSortNewest}, {Sort: taskSortOldest, Steps: true},
		{Sort: taskSortOldest, Plan: "ops-plan"}, {Sort: taskSortNewest, Project: "example-ops", Q: "one"},
	} {
		q.Page = 1
		data, err := svc.tasksPage(ctx, q)
		require.NoError(t, err)
		var order []string
		for _, bucket := range [][]taskRow{data.Live(), data.Unheld(), data.Ready, data.Blocked, data.Gates, data.Gated, data.Closed} {
			for _, r := range bucket {
				order = append(order, r.ID)
			}
		}
		itAgree(t, fmt.Sprintf("tasks %+v", q), order, func(id string) (store.ItemPlace, bool, error) {
			return store.TaskPlace(ctx, svc.db(ctx), q.filter(), q.Sort == taskSortNewest, time.Now().UTC(), id)
		})
	}
}
