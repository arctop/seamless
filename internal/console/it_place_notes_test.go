package console

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// A note's place is where the Notes list draws it, for every sort, filter,
// tag chip and fold (itAgree, it_place_test.go): the list across every page
// (plan:console-ux-pass UX 29), the agent working copies folded away unless
// the list shows them.
func TestItPlaceNotesAgreeWithTheList(t *testing.T) {
	_, mgr, svc, mux := itFixture(t, "advanced")
	ctx := context.Background()
	base := time.Now().UTC().Add(-24 * time.Hour)
	for i, spec := range []struct {
		project, title string
		tags           []string
	}{
		{"example-pay", "Zeta", []string{"research"}}, {"example-pay", "alpha", []string{"plan:pay-v2", "created-by:agent"}},
		{"", "Inbox", nil}, {"example-web", "Beta", []string{"research", "meeting"}}, {"example-web", "beta two", nil},
		{"example-pay", "Planning subagent run", []string{"agent-cache", "plan:pay-v2", "created-by:agent"}},
		{"example-web", "Same time a", []string{"research"}}, {"example-web", "Same time b", []string{"plan:web-v1"}},
	} {
		at := base.Add(time.Duration(i) * time.Hour)
		if spec.title == "Same time a" || spec.title == "Same time b" {
			at = base.Add(10 * time.Hour)
		}
		n := libNote(t, mgr, spec.project, fmt.Sprintf("n-%d", i), spec.title, at, spec.tags)
		if i == 3 || i == 5 {
			require.Equal(t, 303, postFavorite(mux, "note", n.ID, url.Values{"favorite": {"1"}}).Code)
		}
	}
	// The Notes page's list, every row of it (a page as long as the list),
	// in the order it draws them.
	drawn := func(lq libQuery, v noteView) []string {
		t.Helper()
		v.Show = 1000
		data, err := svc.notesPage(ctx, lq, v)
		require.NoError(t, err)
		require.Empty(t, data.List.More, "the whole list is drawn")
		var ids []string
		for _, g := range data.List.Groups {
			for _, n := range g.Rows {
				ids = append(ids, n.ID)
			}
		}
		return ids
	}
	for _, c := range []struct {
		lq libQuery
		v  noteView
	}{
		{libQuery{Sort: "recent"}, noteView{}}, {libQuery{Sort: "name"}, noteView{}}, {libQuery{Sort: "favorites"}, noteView{}},
		{libQuery{Sort: "recent", Group: "project"}, noteView{}}, {libQuery{Sort: "name", Group: "project"}, noteView{}},
		{libQuery{Sort: "recent", Project: "example-web"}, noteView{}}, {libQuery{Sort: "recent", Project: "global"}, noteView{}},
		{libQuery{Sort: "recent", Fav: true}, noteView{}}, {libQuery{Sort: "name", Q: "BETA"}, noteView{}},
		{libQuery{Sort: "recent"}, noteView{Caches: true}}, {libQuery{Sort: "favorites"}, noteView{Caches: true}},
		{libQuery{Sort: "recent", Project: "example-pay"}, noteView{Caches: true}},
		{libQuery{Sort: "recent"}, noteView{Tag: "research"}}, {libQuery{Sort: "name", Project: "example-web"}, noteView{Tag: "research"}},
		{libQuery{Sort: "recent"}, noteView{Plans: true}}, {libQuery{Sort: "recent"}, noteView{Plans: true, Caches: true}},
		{libQuery{Sort: "recent", Q: "subagent"}, noteView{Caches: true}},
	} {
		list := notePlaceList(c.lq, c.v)
		itAgree(t, fmt.Sprintf("notes %+v %+v", c.lq, c.v), drawn(c.lq, c.v), func(id string) (store.ItemPlace, bool, error) {
			return store.NotePlace(ctx, svc.db(ctx), list, id)
		})
	}
}
