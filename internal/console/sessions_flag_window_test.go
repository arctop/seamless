package console

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The Flag filter lists flagged sessions across all time unless the owner
// chose a window, as seam sessions --flagged does: a flag moves no
// updated_at, so the default 24h window would hide every session flagged
// after its last day. A chosen window applies to it as to every filter.
func TestSessionsList_FlagFilterSpansAllTimeUnlessAWindowIsChosen(t *testing.T) {
	db, mux, svc := newFlagConsole(t)
	ctx := context.Background()
	recent := flaggedSession(t, db, svc, "cc/recent", core.VerdictGood, "")
	old := oldSession(t, db, "cc/old", 10*24*time.Hour)
	_, err := svc.Flag(ctx, old, core.VerdictBad, "a month of lessons", "owner", true)
	require.NoError(t, err)
	oldPlain := oldSession(t, db, "cc/old-plain", 10*24*time.Hour)

	for _, tc := range []struct {
		query, window string
		want          []string
	}{
		{"verdict=flagged", "all", []string{recent.ID, old.ID}},
		{"verdict=bad", "all", []string{old.ID}},
		{"verdict=flagged&w=24h", "24h", []string{recent.ID}},
		{"verdict=flagged&w=7d", "7d", []string{recent.ID}},
		{"verdict=flagged&w=30d", "30d", []string{recent.ID, old.ID}},
		{"", "24h", []string{recent.ID}},
		{"w=all", "all", []string{recent.ID, old.ID, oldPlain.ID}},
	} {
		var list sessionsData
		getJSON(t, mux, "/console/sessions?format=json&"+tc.query, &list)
		var ids []string
		for _, r := range list.Sessions {
			ids = append(ids, r.ID)
		}
		require.ElementsMatch(t, tc.want, ids, "?%s", tc.query)
		require.Equal(t, tc.window, list.Window, "?%s", tc.query)
	}

	// The page's links carry a window only once one was chosen, so a filter
	// picked from the default view does not pin its 24h.
	page := getPage(t, mux, "/console/sessions")
	require.Contains(t, page, `href="/console/sessions?status=&amp;sort=recent&amp;q=&amp;day=&amp;retained=&amp;verdict=flagged"`)
	require.NotContains(t, page, `name="w"`, "the search does not pin the default window")
	flagged := getPage(t, mux, "/console/sessions?verdict=flagged")
	require.Contains(t, flagged, "all time view")
	require.Contains(t, flagged, `href="/console/sessions?status=completed&amp;sort=recent&amp;q=&amp;day=&amp;retained=&amp;verdict=flagged"`,
		"another filter keeps the flag and leaves the window unchosen")
	require.Contains(t, flagged, `href="/console/sessions?status=&amp;sort=recent&amp;q=&amp;day=&amp;retained="`,
		"clearing the flag goes back to the default window")
	require.Contains(t, flagged, `href="/console/sessions?status=&amp;w=24h&amp;sort=recent&amp;q=&amp;retained=&amp;verdict=flagged"`,
		"the window links choose one")
	chosen := getPage(t, mux, "/console/sessions?w=7d")
	require.Contains(t, chosen, `href="/console/sessions?status=&amp;w=7d&amp;sort=recent&amp;q=&amp;day=&amp;retained=&amp;verdict=flagged"`,
		"a chosen window stays chosen through the flag filter")
	require.Contains(t, chosen, `<input type="hidden" name="w" value="7d">`)

	// Live updates re-read the page's own URL: a session flagged since, however
	// old, joins the filtered list on the next read.
	late := oldSession(t, db, "cc/late", 40*24*time.Hour)
	var before sessionsData
	getJSON(t, mux, "/console/sessions?format=json&verdict=flagged", &before)
	require.Len(t, before.Sessions, 2)
	_, err = svc.Flag(ctx, late, core.VerdictNotable, "", "agent", true)
	require.NoError(t, err)
	var after sessionsData
	getJSON(t, mux, "/console/sessions?format=json&verdict=flagged", &after)
	require.Len(t, after.Sessions, 3)

	// A window present but none of the windows is refused, never a default.
	for _, bad := range []string{"w=", "w=1y", "w=bogus&verdict=flagged"} {
		req := httptest.NewRequest(http.MethodGet, "/console/sessions?format=json&"+bad, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr := do(mux, req)
		require.Equal(t, http.StatusBadRequest, rr.Code, "?%s", bad)
		require.Contains(t, rr.Body.String(), "valid values are 24h, 7d, 30d, all", "?%s", bad)
	}
}

// oldSession creates a completed session last updated age ago.
func oldSession(t *testing.T, db *sql.DB, name string, age time.Duration) core.Session {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	at := time.Now().UTC().Add(-age)
	sess := core.Session{ID: id, Name: name, ProjectSlug: "demo", Status: core.SessionCompleted, Ambient: true,
		ExternalClient: "claude-code", ExternalSessionID: name, Source: "startup", CreatedAt: at, UpdatedAt: at}
	require.NoError(t, store.CreateSession(context.Background(), db, sess))
	return sess
}
