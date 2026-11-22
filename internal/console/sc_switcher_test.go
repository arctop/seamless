package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/demokit"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
)

// scReview is a console over the review fixture (demoseed -review), seeded
// into the test's own folder, with its database for the test's own reads.
func scReview(t *testing.T) (*http.ServeMux, *sql.DB, demokit.ReviewFixture) {
	t.Helper()
	dir := t.TempDir()
	seeder, err := demokit.New(dir)
	require.NoError(t, err)
	fx, err := seeder.SeedReview()
	require.NoError(t, err, "seeding the review fixture")
	require.NoError(t, seeder.Close())
	db, err := store.Open(filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	svc, err := New(Config{DB: db, Files: mgr, Events: events.NewRecorder(db), DataDir: dir, APIKey: testKey, Level: "advanced"})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return mux, db, fx
}

// The switcher on the review fixture, seeded once for these reads.
func TestScOnTheReviewFixture(t *testing.T) {
	mux, db, fx := scReview(t)
	t.Run("order is most active first", func(t *testing.T) { scOrderMostActiveFirst(t, mux, db, fx) })
	t.Run("no-script markup is a list of links", func(t *testing.T) { scNoScriptMarkupIsAListOfLinks(t, mux) })
	t.Run("choosing keeps the other filters", func(t *testing.T) { scChoosingKeepsTheOtherFilters(t, mux) })
	t.Run("g p stays Plans", func(t *testing.T) { scGPStaysPlans(t, mux) })
}

// scGet is an authenticated browser GET's body, after a 200.
func scGet(t *testing.T, mux *http.ServeMux, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code, "GET %s: %s", path, rr.Body.String())
	return rr.Body.String()
}

// scRow is one switcher row as the page draws it.
type scRow struct {
	ID, Value, Href, Label, Name, Hint string
	Count                              int // -1: no count drawn
	On, Starred, Locked, Query         bool
}

// scView is a page's switcher, section by section.
type scView struct {
	Markup, Label, Current string
	All                    scRow
	Starred, Active, Quiet []scRow
	QuietHead              string
}

var (
	scRowRe   = regexp.MustCompile(`<a class="scope-o( on)?" id="([^"]*)" data-scope-v="([^"]*)"([^>]*) href="([^"]*)">(.*?)</a>`)
	scCountRe = regexp.MustCompile(`<span class="n">(\d+)</span>`)
	scHintRe  = regexp.MustCompile(`<span class="scope-hint">(?:<time[^>]*>)?([^<]*)`)
	scNameRe  = regexp.MustCompile(` data-scope-name="([^"]*)"`)
	scTagRe   = regexp.MustCompile(`<[^>]*>`)
)

// scParse reads the switcher a page draws. Rows are placed by where they
// stand: before Starred is All projects, then Starred, the main list, and
// the quiet list.
func scParse(t *testing.T, body string) scView {
	t.Helper()
	start := strings.Index(body, `<details class="menu scope"`)
	require.GreaterOrEqual(t, start, 0, "the page draws the switcher")
	end := strings.Index(body[start:], "</details>")
	require.Positive(t, end)
	markup := body[start : start+end+len("</details>")]
	v := scView{Markup: markup}
	v.Label = html.UnescapeString(regexp.MustCompile(`aria-label="Project: ([^"]*)"`).FindStringSubmatch(markup)[1])
	v.Current = regexp.MustCompile(`data-scope-current="([^"]*)"`).FindStringSubmatch(markup)[1]
	starredAt := strings.Index(markup, "data-scope-starred>")
	activeAt := strings.Index(markup, "All, most active first")
	quietAt := strings.Index(markup, "data-scope-quiet>")
	if m := regexp.MustCompile(`data-scope-head>([^<]*)<span class="scope-type">`).FindStringSubmatch(markup); m != nil {
		v.QuietHead = m[1]
	}
	for _, at := range scRowRe.FindAllStringSubmatchIndex(markup, -1) {
		g := func(i int) string { return markup[at[2*i]:at[2*i+1]] }
		r := scRow{On: at[2] >= 0, ID: g(2), Value: g(3), Href: html.UnescapeString(g(5)), Count: -1}
		attrs, inner := g(4), g(6)
		r.Query = strings.Contains(attrs, " data-seam-query")
		r.Locked = strings.Contains(attrs, " data-scope-locked")
		if m := scNameRe.FindStringSubmatch(attrs); m != nil {
			r.Name = html.UnescapeString(m[1])
		}
		r.Starred = strings.Contains(inner, "ico-star")
		if m := scCountRe.FindStringSubmatch(inner); m != nil {
			r.Count, _ = strconv.Atoi(m[1])
		}
		if m := scHintRe.FindStringSubmatch(inner); m != nil {
			r.Hint = html.UnescapeString(m[1])
		}
		label := scCountRe.ReplaceAllString(inner, "")
		if i := strings.Index(label, `<span class="scope-hint">`); i >= 0 {
			label = label[:i]
		}
		r.Label = html.UnescapeString(scTagRe.ReplaceAllString(label, ""))
		pos := at[0]
		switch {
		case r.ID == "sc-o-all":
			v.All = r
		case starredAt >= 0 && pos > starredAt && (activeAt < 0 || pos < activeAt) && (quietAt < 0 || pos < quietAt):
			v.Starred = append(v.Starred, r)
		case quietAt >= 0 && pos > quietAt:
			v.Quiet = append(v.Quiet, r)
		default:
			v.Active = append(v.Active, r)
		}
	}
	return v
}

// The review fixture's Memories: All projects first, counting every active
// memory; Starred above the main list; the main list most active first by
// the store's last activity, the global scope last as "Shared globally";
// the projects quiet for 30 days in their own list, the line above it
// saying how many. Recent is the browser's: drawn empty, for scope.js.
func scOrderMostActiveFirst(t *testing.T, mux *http.ServeMux, db *sql.DB, fx demokit.ReviewFixture) {
	ctx := context.Background()
	body := scGet(t, mux, "/console/memories")
	v := scParse(t, body)
	var page memoriesData
	getJSON(t, mux, "/console/memories?format=json", &page)

	require.Equal(t, "All projects", v.Label)
	require.Empty(t, v.Current)
	require.True(t, v.All.On)
	require.Equal(t, page.ActiveCount, v.All.Count, "All projects counts every active memory")
	require.Contains(t, v.Markup, `<div class="scope-sec" data-scope-recent hidden><p class="scope-head">Recent</p></div>`)
	require.Less(t, strings.Index(v.Markup, "data-scope-recent"), strings.Index(v.Markup, "data-scope-starred"))
	require.Less(t, strings.Index(v.Markup, "data-scope-starred"), strings.Index(v.Markup, "All, most active first"))

	facts, err := store.ProjectActivityBySlug(ctx, db)
	require.NoError(t, err)
	now := time.Now()
	var last time.Time
	sum := 0
	require.NotEmpty(t, v.Active)
	global := v.Active[len(v.Active)-1]
	require.Equal(t, "global", global.Value, "the global scope is last")
	require.Equal(t, "Shared globally", global.Label)
	require.Equal(t, scopeGlobalHint, global.Hint)
	require.Positive(t, global.Count)
	sum += global.Count
	for i, r := range v.Active[:len(v.Active)-1] {
		at := facts[r.Value].LastActive
		require.False(t, scopeQuiet(at, now), "%s is active", r.Value)
		if i > 0 {
			require.False(t, at.After(last), "%s comes after a project less active than it", r.Value)
		}
		require.Equal(t, facts[r.Value].Favorite, r.Starred, r.Value)
		require.Positive(t, r.Count)
		last, sum = at, sum+r.Count
	}
	quiet, ok := fx.Item(demokit.ReviewProjectsQuiet)
	require.True(t, ok)
	require.NotEmpty(t, v.Quiet, "the fixture's quiet projects that hold memories")
	require.Equal(t, strconv.Itoa(len(v.Quiet))+" quiet projects, no activity in 30 days", v.QuietHead)
	last = time.Time{}
	for i, r := range v.Quiet {
		at := facts[r.Value].LastActive
		require.True(t, scopeQuiet(at, now), "%s is quiet", r.Value)
		require.Contains(t, quiet.IDs, r.Value, "a quiet row is one of the fixture's quiet projects")
		if i > 0 && !at.IsZero() {
			require.False(t, at.After(last), "%s comes after a project less recently active", r.Value)
		}
		last, sum = at, sum+r.Count
	}
	require.Equal(t, v.All.Count, sum, "every scope counted once, and All projects is their sum")

	starred, ok := fx.Item(demokit.ReviewProjectsStarred)
	require.True(t, ok)
	require.NotEmpty(t, v.Starred)
	var want []string
	for _, r := range append(append([]scRow{}, v.Active...), v.Quiet...) {
		if r.Starred {
			want = append(want, r.Value)
		}
	}
	var got []string
	for _, r := range v.Starred {
		require.True(t, r.Starred)
		require.Contains(t, starred.IDs, r.Value)
		require.Equal(t, "sc-s-"+r.Value, r.ID, "a Starred copy has an id of its own")
		got = append(got, r.Value)
	}
	require.Equal(t, want, got, "Starred lists the starred rows in the list's own order")
}

// A unit of the order on a fixed clock: activity first, then the label; the
// current choice stays in the main list even when quiet; global is last; a
// locked project joins by its activity with a lock and no count, and stands
// for the current choice when the request names it; a locked slug with no
// project is left out, as the board leaves it out.
func TestScBuildOnAFixedClock(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	menu := []libLink{
		{Label: "All projects", Href: "/l"},
		{Label: "Shared globally", Href: "/l?project=global", Count: 4, Value: "global"},
		{Label: "alpha", Href: "/l?project=alpha", Count: 3, Value: "alpha"},
		{Label: "bravo", Href: "/l?project=bravo", Count: 2, Value: "bravo", Title: "Bravo Team"},
		{Label: "charlie", Href: "/l?project=charlie", Count: 1, Value: "charlie"},
		{Label: "delta", Href: "/l?project=delta", Count: 5, Value: "delta"},
		{Label: "echo", Href: "/l?project=echo", Count: 6, Value: "echo"},
		{Label: "secret", Href: "/l?project=secret", Value: "secret", On: true},
	}
	facts := map[string]store.ProjectActivity{
		"alpha":   {LastActive: now.Add(-3 * time.Hour)},
		"bravo":   {LastActive: now.Add(-10 * time.Minute), Favorite: true},
		"charlie": {LastActive: now.Add(-3 * time.Hour)},
		"delta":   {LastActive: now.Add(-40 * 24 * time.Hour), Favorite: true},
		// echo: no session or task ever.
	}
	locked := []store.ProjectTally{
		{Slug: "secret", Name: "Secret Ledger", Registered: true, Memories: 9, LastActive: now.Add(-time.Hour)},
		{Slug: "ghost", Name: "ghost", Memories: 2, LastActive: now},
	}
	sw := buildProjectSwitcher(menu, facts, locked, now)

	values := func(rows []scopeChoice) []string {
		out := []string{}
		for _, r := range rows {
			out = append(out, r.Value)
		}
		return out
	}
	require.Equal(t, []string{"bravo", "secret", "alpha", "charlie", "global"}, values(sw.Active),
		"most active first, a tie by label, global last")
	require.Equal(t, []string{"delta", "echo"}, values(sw.Quiet), "quiet, most recently active first")
	require.Equal(t, []string{"bravo", "delta"}, values(sw.Starred))
	require.Equal(t, "sc-s-bravo", sw.Starred[0].ID)
	require.Equal(t, scopeChoice{ID: "sc-o-all", Label: "All projects", Href: "/l", Count: 21, Counted: true}, sw.All,
		"All projects counts every scope's own count; a locked project adds nothing")

	lock := sw.Active[1]
	require.True(t, lock.Locked)
	require.False(t, lock.Counted, "a locked project has no count")
	require.Zero(t, lock.Count)
	require.Equal(t, "/console/unlock/secret", lock.Href)
	require.Equal(t, "Secret Ledger", lock.Name)
	require.Equal(t, scopeLockedHint, lock.Hint)
	require.True(t, lock.On, "the request names it: the lock row is the current choice")
	require.Equal(t, "secret", sw.Label)
	require.Equal(t, "secret", sw.Value)

	require.Equal(t, "active 10m ago", sw.Active[0].Hint)
	require.Equal(t, "Bravo Team", sw.Active[0].Name)
	require.Empty(t, sw.Quiet[1].Hint, "no activity, no hint")

	// The current choice stays in the main list, wherever its activity is.
	menu[len(menu)-1].On = false
	menu[5].On = true
	sw = buildProjectSwitcher(menu, facts, nil, now)
	require.Equal(t, []string{"bravo", "alpha", "charlie", "delta", "global"}, values(sw.Active))
	require.Equal(t, []string{"echo", "secret"}, values(sw.Quiet))
	require.Equal(t, "delta", sw.Label)
	require.False(t, sw.Quiet[1].Locked, "nothing locked: the request reads it, with its count")
	require.True(t, sw.Quiet[1].Counted)

	// The global scope as the current choice.
	menu[5].On, menu[1].On = false, true
	sw = buildProjectSwitcher(menu, facts, nil, now)
	require.Equal(t, "Shared globally", sw.Label)
	require.Equal(t, "global", sw.Value)
}

// A locked project this request does not read: listed by name with a lock,
// no count and no star (its star is the hidden projects row's), linking to
// its unlock page, never patched in place; naming it in the URL makes it the
// current choice. An open project keeps its count.
func TestScLockedProjectHasNoCount(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	require.NoError(t, store.SetProjectFavorite(ctx, lc.db, lockedSlug, true))
	require.NoError(t, store.SetProjectFavorite(ctx, lc.db, openSlug, true))

	for _, path := range []string{"/console/memories", "/console/notes", "/console/memories?project=" + lockedSlug} {
		code, body := lc.get(t, path, "")
		require.Equal(t, http.StatusOK, code, path)
		require.NotContains(t, body, lockedSecret, path)
		v := scParse(t, body)
		var lock *scRow
		for i := range v.Active {
			if v.Active[i].Value == lockedSlug {
				lock = &v.Active[i]
			}
		}
		require.NotNil(t, lock, "%s: the locked project is listed, by its activity", path)
		require.True(t, lock.Locked, path)
		require.Equal(t, -1, lock.Count, "%s: a locked project has no count", path)
		require.False(t, lock.Starred, "%s: its star is not this request's to read", path)
		require.False(t, lock.Query, "%s: it opens its unlock page, never patched in place", path)
		require.Equal(t, "/console/unlock/"+lockedSlug, lock.Href, path)
		require.Equal(t, scopeLockedHint, lock.Hint, path)
		for _, r := range v.Starred {
			require.NotEqual(t, lockedSlug, r.Value, "%s: no Starred copy of a locked project", path)
		}
		if strings.Contains(path, "project=") {
			require.Equal(t, lockedSlug, v.Label)
			require.True(t, lock.On)
			continue
		}
		if !strings.HasPrefix(path, "/console/memories") {
			continue
		}
		var open *scRow
		for _, r := range append(append([]scRow{}, v.Active...), v.Quiet...) {
			if r.Value == openSlug {
				open = &r
			}
		}
		require.NotNil(t, open, path)
		require.Equal(t, 1, open.Count, "%s: the open project's count is the screen's own", path)
		require.True(t, open.Starred, path)
	}
}

// Without script the switcher is a <details> menu of links: every choice an
// <a href>, the field hidden until scope.js shows it, the quiet projects
// listed under their heading, and nothing to submit.
func scNoScriptMarkupIsAListOfLinks(t *testing.T, mux *http.ServeMux) {
	v := scParse(t, scGet(t, mux, "/console/memories"))
	m := v.Markup
	require.True(t, strings.HasPrefix(m, `<details class="menu scope" data-scope data-scope-current="">`))
	require.Contains(t, m, `<summary class="menu-btn scope-btn" aria-label="Project: All projects" aria-keyshortcuts="p"`)
	require.Contains(t, m, `<span class="menu-k">In</span><strong>All projects</strong>`)
	require.Contains(t, m, `<div class="menu-list scope-pop">`, "shell.js closes a .menu-list on a choice")
	require.Contains(t, m, `<label class="scope-field" data-live-skip hidden>`,
		"the field is script's, and a live refresh keeps what was typed")
	for _, banned := range []string{"<form", "<button", "<select", "onclick", "javascript:"} {
		require.NotContains(t, m, banned)
	}
	rows := 1 + len(v.Starred) + len(v.Active) + len(v.Quiet)
	require.Equal(t, rows, strings.Count(m, "<a "), "every choice is a link")
	require.NotEmpty(t, v.Quiet, "the quiet projects are links too, for a page with no field")
	for _, r := range append(append([]scRow{v.All}, v.Active...), v.Quiet...) {
		require.True(t, r.Query, "%s patches the view in place", r.Value)
		require.True(t, strings.HasPrefix(r.Href, "/console/memories"), r.Href)
	}
}

// Every choice keeps the screen's other filters, and changes only the
// project: All projects drops it, a scope sets it, the sort and the grouping
// stay.
func scChoosingKeepsTheOtherFilters(t *testing.T, mux *http.ServeMux) {
	for _, tc := range []struct{ list, query string }{
		{"/console/memories", "group=project&kind=gotcha&q=e&sort=reach"},
		{"/console/notes", "group=project&q=e&sort=name"},
		{"/console/notes", "project=global&q=e"},
	} {
		want, err := url.ParseQuery(tc.query)
		require.NoError(t, err)
		v := scParse(t, scGet(t, mux, tc.list+"?"+tc.query))
		rows := append(append(append([]scRow{v.All}, v.Starred...), v.Active...), v.Quiet...)
		require.Greater(t, len(rows), 1, tc.query)
		for _, r := range rows {
			u, err := url.Parse(r.Href)
			require.NoError(t, err)
			require.Equal(t, tc.list, u.Path, r.Href)
			got := u.Query()
			require.Equal(t, r.Value, got.Get("project"), "%s: the row sets its own project", r.Href)
			got.Del("project")
			keep := url.Values{}
			for k, vs := range want {
				if k != "project" {
					keep[k] = vs
				}
			}
			require.Equal(t, keep, got, "%s keeps every other filter", r.Href)
		}
	}
	v := scParse(t, scGet(t, mux, "/console/notes?project=global"))
	require.Equal(t, "Global inbox", v.Label, "Notes names the global scope its own way")
	require.Equal(t, "global", v.Current)
}

// scope.js filters the rows the page holds: it never fetches, never
// navigates, reloads or scrolls the page by itself (a choice is the row's
// own link; a highlight scrolls the list alone), touches
// storage only inside try, steps aside for a g-chord and an open palette
// before claiming p, and redraws after every live refresh. The layout loads
// it before shell.js, which is why it must step aside itself, and the
// shell's own keys leave a bare p free.
func TestScScriptFiltersInThePage(t *testing.T) {
	js := string(scopeJS)
	for _, banned := range []string{"fetch(", "XMLHttpRequest", "location.href =", "location.assign", "location.reload", ".submit(",
		"scrollIntoView(", "window.scrollTo("} {
		require.NotContains(t, js, banned)
	}
	require.Equal(t, 2, strings.Count(js, "localStorage."), "one read, one write")
	require.Contains(t, js, "try {\n      var list = JSON.parse(localStorage.getItem(RECENT_KEY)")
	require.Contains(t, js, "try { localStorage.setItem(RECENT_KEY,")
	require.Contains(t, js, "var RECENT_KEY = 'seamless-scope-recent';")
	require.Contains(t, js, "if (document.querySelector('.chord-hint')) return true;")
	require.Contains(t, js, "if (typing(e.target) || typing(document.activeElement) || shellHoldsKey()) return;")
	require.Contains(t, js, "if (e.key !== 'p' || e.metaKey || e.ctrlKey || e.altKey || e.defaultPrevented) return;")
	require.Contains(t, js, "document.addEventListener('seam:content-updated', init);")

	layout, err := templateFS.ReadFile("templates/layout.html")
	require.NoError(t, err)
	page := string(layout)
	scope, shell := strings.Index(page, `{{static "scope.js"}}`), strings.Index(page, `{{static "shell.js"}}`)
	require.Positive(t, scope)
	require.Less(t, scope, shell, "scope.js's key listener runs before the shell's")
	require.NotContains(t, string(shellJS), "e.key === 'p'", "the shell binds no bare p")
	require.Contains(t, string(shellJS), "if (e.defaultPrevented) return;", "a p scope.js took does not reach the chord")
}

// g p is Plans: the chord's second key is read from the nav, and Plans
// carries p. With the hint up, scope.js leaves p to the shell (above).
func scGPStaysPlans(t *testing.T, mux *http.ServeMux) {
	body := scGet(t, mux, "/console/memories")
	require.Regexp(t, `<a href="/console/plans" data-key="p"`, body)
	var raw json.RawMessage
	getJSON(t, mux, "/console/memories?format=json", &raw)
	require.NotContains(t, string(raw), "sc-o-", "the switcher is the page's, not the JSON answer's")
}
