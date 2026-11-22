package console

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// itKinds is core's memory kinds in their order, as the place query takes them.
func itKinds() []string {
	out := make([]string, 0, len(core.MemoryKinds))
	for _, k := range core.MemoryKinds {
		out = append(out, string(k))
	}
	return out
}

// itAgree checks that the place query puts every id of order where the list
// does: at its index, out of len(order), between its list neighbours.
func itAgree(t *testing.T, what string, order []string, place func(id string) (store.ItemPlace, bool, error)) {
	t.Helper()
	require.NotEmpty(t, order, what)
	for i, id := range order {
		p, found, err := place(id)
		require.NoError(t, err, what)
		require.True(t, found, "%s: %s is in the list", what, id)
		require.Equal(t, i+1, p.Pos, "%s: %s", what, id)
		require.Equal(t, len(order), p.Total, what)
		wantPrev, wantNext := "", ""
		if i > 0 {
			wantPrev = order[i-1]
		}
		if i+1 < len(order) {
			wantNext = order[i+1]
		}
		require.Equal(t, wantPrev, p.Prev.ID, "%s: before %s", what, id)
		require.Equal(t, wantNext, p.Next.ID, "%s: after %s", what, id)
	}
}

// An item's place is where its list draws it: the place query and the list
// builder agree on every Memories, Notes and Tasks list, for every sort and
// grouping, so k / j walk the list the owner came from.
func TestItPlaceAgreesWithTheList(t *testing.T) {
	db, mgr, svc, mux := itFixture(t, "advanced")
	ctx := context.Background()
	base := time.Now().UTC().Add(-24 * time.Hour)
	var mems []core.Memory
	for i, spec := range []struct {
		kind    core.MemoryKind
		project string
		name    string
	}{
		{core.KindGotcha, "example-pay", "zeta-pay"}, {core.KindGotcha, "example-pay", "alpha-pay"},
		{core.KindDecision, "example-pay", "mid-pay"}, {core.KindGotcha, "example-web", "beta-web"},
		{core.KindRunbook, "", "global-run"}, {core.KindConstraint, "", "global-rule"},
		{core.KindGotcha, "example-web", "same-time-a"}, {core.KindGotcha, "example-web", "same-time-b"},
	} {
		at := base.Add(time.Duration(i) * time.Hour)
		if strings.HasPrefix(spec.name, "same-time") {
			at = base.Add(10 * time.Hour)
		}
		mems = append(mems, libMemory(t, mgr, spec.kind, spec.project, spec.name, at))
	}
	require.Equal(t, 303, postFavorite(mux, "memory", mems[3].ID, url.Values{"favorite": {"1"}}).Code)
	libSession(t, db, "S1", "cc/s1", "example-pay", base)
	libInject(t, db, "S1", "example-pay", base.Add(time.Hour), mems[1].ID, mems[3].ID)
	libInject(t, db, "S1", "example-pay", base.Add(2*time.Hour), mems[3].ID)
	archived := writeMemory(t, mgr, core.KindGotcha, "example-pay", "old-pay", "old")
	archived2 := writeMemory(t, mgr, core.KindDecision, "example-web", "old-web", "old")
	for _, id := range []string{archived.ID, archived2.ID} {
		require.Equal(t, 303, postForm(mux, "/console/memories/"+id+"/archive", "").Code)
	}

	for _, lq := range []libQuery{
		{Sort: "recent"}, {Sort: "name"}, {Sort: "reach"}, {Sort: "utility"}, {Sort: "favorites"},
		{Sort: "recent", Group: "project"}, {Sort: "reach", Group: "project"},
		{Sort: "recent", Kind: "gotcha"}, {Sort: "name", Project: "example-pay"}, {Sort: "recent", Project: "global"},
		{Sort: "recent", Fav: true}, {Sort: "recent", Surfaced: "never"}, {Sort: "name", Q: "PAY"}, {Sort: "recent", Q: "global"},
	} {
		data, err := svc.memoriesPage(ctx, lq)
		require.NoError(t, err)
		var active, inactive []string
		for _, g := range data.Rail {
			for _, k := range g.Kinds {
				for _, m := range k.Memories {
					active = append(active, m.ID)
				}
			}
		}
		for _, m := range data.Inactive {
			inactive = append(inactive, m.ID)
		}
		list := store.MemoryList{ByProject: lq.Project != "", Project: lq.Project, Kind: lq.Kind, Fav: lq.Fav,
			Never: lq.Surfaced == "never", Query: strings.ToLower(lq.Q), Sort: lq.Sort, Tree: lq.tree(), Kinds: itKinds()}
		if list.Project == "global" {
			list.Project = ""
		}
		what := fmt.Sprintf("memories %+v", lq)
		itAgree(t, what, active, func(id string) (store.ItemPlace, bool, error) {
			return store.MemoryPlace(ctx, svc.db(ctx), list, id)
		})
		if len(inactive) > 0 {
			gone := list
			gone.Inactive, gone.Tree = true, false
			itAgree(t, what+" archived", inactive, func(id string) (store.ItemPlace, bool, error) {
				return store.MemoryPlace(ctx, svc.db(ctx), gone, id)
			})
		}
	}

	for i, spec := range []struct{ project, title string }{
		{"example-pay", "Zeta"}, {"example-pay", "alpha"}, {"", "Inbox"}, {"example-web", "Beta"}, {"example-web", "beta two"},
	} {
		n := itNote(t, mgr, spec.project, fmt.Sprintf("n-%d", i), spec.title, "x", base.Add(time.Duration(i)*time.Hour))
		if i == 3 {
			require.Equal(t, 303, postFavorite(mux, "note", n.ID, url.Values{"favorite": {"1"}}).Code)
		}
	}
	for _, lq := range []libQuery{
		{Sort: "recent"}, {Sort: "name"}, {Sort: "favorites"}, {Sort: "recent", Group: "project"},
		{Sort: "recent", Project: "example-web"}, {Sort: "recent", Project: "global"}, {Sort: "recent", Fav: true},
		{Sort: "name", Q: "BETA"},
	} {
		data, err := svc.notesPage(ctx, lq)
		require.NoError(t, err)
		var order []string
		for _, g := range data.Rail {
			for _, n := range g.Notes {
				order = append(order, n.ID)
			}
		}
		list := store.NoteList{ByProject: lq.Project != "", Project: lq.Project, Fav: lq.Fav,
			Query: strings.ToLower(lq.Q), Sort: lq.Sort, Tree: lq.tree()}
		if list.Project == "global" {
			list.Project = ""
		}
		itAgree(t, fmt.Sprintf("notes %+v", lq), order, func(id string) (store.ItemPlace, bool, error) {
			return store.NotePlace(ctx, svc.db(ctx), list, id)
		})
	}

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
