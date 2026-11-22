package console

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// A note's place is where the Notes list draws it, for every sort, grouping
// and filter (itAgree, it_place_test.go).
func TestItPlaceNotesAgreeWithTheList(t *testing.T) {
	_, mgr, svc, mux := itFixture(t, "advanced")
	ctx := context.Background()
	base := time.Now().UTC().Add(-24 * time.Hour)
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
}
