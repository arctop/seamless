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
// builder agree on every Memories list, for every sort and grouping, so k / j
// walk the list the owner came from. The Notes and Tasks lists have their own
// tests (it_place_notes_test.go, it_place_tasks_test.go).
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

	// The Memories page's list (plan:console-ux-pass UX 28), every row of it
	// (a page as long as the list), in the order it draws them: a project's
	// by kind, every project's in the sort's order; the archived and
	// superseded list the same way.
	drawn := func(lq libQuery, inactive bool) []string {
		t.Helper()
		data, err := svc.memoriesPage(ctx, lq, memoryView{Inactive: inactive, Show: 1000})
		require.NoError(t, err)
		require.Empty(t, data.List.More, "the whole list is drawn")
		var ids []string
		for _, g := range data.List.Groups {
			for _, m := range g.Rows {
				ids = append(ids, m.ID)
			}
		}
		return ids
	}
	inactiveLists := 0
	for _, lq := range []libQuery{
		{Sort: "recent"}, {Sort: "name"}, {Sort: "reach"}, {Sort: "utility"}, {Sort: "favorites"},
		{Sort: "recent", Group: "project"}, {Sort: "reach", Group: "project"},
		{Sort: "recent", Kind: "gotcha"}, {Sort: "name", Project: "example-pay"}, {Sort: "recent", Project: "global"},
		{Sort: "reach", Project: "example-pay"}, {Sort: "favorites", Project: "example-web"},
		{Sort: "recent", Fav: true}, {Sort: "recent", Surfaced: "never"}, {Sort: "name", Q: "PAY"}, {Sort: "recent", Q: "global"},
	} {
		// The item page's own list (memoryPlaceList), as memoryItemPage reads it.
		list := memoryPlaceList(lq, false)
		require.Equal(t, itKinds(), list.Kinds)
		what := fmt.Sprintf("memories %+v", lq)
		itAgree(t, what, drawn(lq, false), func(id string) (store.ItemPlace, bool, error) {
			return store.MemoryPlace(ctx, svc.db(ctx), list, id)
		})
		if inactive := drawn(lq, true); len(inactive) > 0 {
			inactiveLists++
			itAgree(t, what+" archived", inactive, func(id string) (store.ItemPlace, bool, error) {
				return store.MemoryPlace(ctx, svc.db(ctx), memoryPlaceList(lq, true), id)
			})
		}
	}
	require.Positive(t, inactiveLists, "the archived lists are checked too")
}
