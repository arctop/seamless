package console

import (
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/demokit"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
)

// The review fixture (demoseed -review), walked as the panel would: on each
// page the plan's "Done when" names, the links a plain click reads in place,
// and the fragment each one loads.

// pnReview is a console over the review fixture, seeded into the test's own
// folder, research on (trials have fragments too).
func pnReview(t *testing.T) (*http.ServeMux, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	seeder, err := demokit.New(dir)
	require.NoError(t, err)
	_, err = seeder.SeedReview()
	require.NoError(t, err, "seeding the review fixture")
	require.NoError(t, seeder.Close())
	db, err := store.Open(filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	svc, err := New(Config{DB: db, Files: mgr, Events: events.NewRecorder(db), DataDir: dir, APIKey: testKey,
		Level: "advanced", Features: config.Features{Research: true}})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return mux, db
}

// pnGet is an authenticated browser GET's body, after a 200.
func pnGet(t *testing.T, mux *http.ServeMux, path string) string {
	t.Helper()
	rr := getPeek(t, mux, path)
	require.Equal(t, http.StatusOK, rr.Code, "GET %s: %s", path, rr.Body.String())
	return rr.Body.String()
}

// pnAttr is an element's attribute, and whether it has it.
func pnAttr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func pnHasClass(n *html.Node, class string) bool {
	v, _ := pnAttr(n, "class")
	return slices.Contains(strings.Fields(v), class)
}

// pnLeftAlone says whether a click on a link inside n is someone else's, as
// panel.js and the handlers before it decide: a form or a button, a link that
// asks for the full page (data-no-panel, the dossier's open full), the
// in-place parts (data-jump, data-open-step, the detail split),
// navigation.js's data-seam-query links, library.js's rail and reader, and
// the panel itself.
func pnLeftAlone(n *html.Node) bool {
	for p := n; p != nil; p = p.Parent {
		if p.Type != html.ElementNode {
			continue
		}
		switch p.Data {
		case "form", "button":
			return true
		}
		for _, key := range []string{"data-no-panel", "data-jump", "data-open-step", "data-seam-query"} {
			if _, ok := pnAttr(p, key); ok {
				return true
			}
		}
		for _, class := range []string{"dossier-open", "list-detail-split", "lib-rail"} {
			if pnHasClass(p, class) {
				return true
			}
		}
		if id, _ := pnAttr(p, "id"); id == "lib-reader" || id == "read-panel" {
			return true
		}
	}
	return false
}

// pnRead lists, in document order, the hrefs a plain click on a link inside
// .main reads in the panel, each once.
func pnRead(t *testing.T, page string) []string {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(page))
	require.NoError(t, err)
	item := pnItem(t)
	var out []string
	seen := map[string]bool{}
	inMain := func(n *html.Node) bool {
		for p := n; p != nil; p = p.Parent {
			if p.Type == html.ElementNode && p.Data == "main" {
				return true
			}
		}
		return false
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			href, ok := pnAttr(n, "href")
			_, target := pnAttr(n, "target")
			_, download := pnAttr(n, "download")
			if ok && !target && !download && !strings.HasPrefix(href, "#") && inMain(n) && !pnLeftAlone(n) {
				if u, err := url.Parse(href); err == nil && u.Host == "" && item.MatchString(u.Path) {
					u.Fragment = ""
					if h := u.String(); !seen[h] {
						seen[h] = true
						out = append(out, h)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

// pnPeek is the fragment the panel loads for href: the link's own query
// kept, peek=1 added.
func pnPeek(href string) string {
	u, _ := url.Parse(href)
	q := u.Query()
	q.Set("peek", "1")
	u.RawQuery = q.Encode()
	return u.String()
}

var pnKindOf = regexp.MustCompile(`^/console/([a-z]+)/`)

func pnKind(href string) string {
	m := pnKindOf.FindStringSubmatch(href)
	if m == nil {
		return ""
	}
	return m[1]
}

func TestPnOnTheReviewFixture(t *testing.T) {
	mux, db := pnReview(t)
	t.Run("the done walk: a plain click on each page reads in place", func(t *testing.T) { pnDoneWalk(t, mux) })
	t.Run("every fragment the panel reads draws inside it", func(t *testing.T) { pnEveryFragmentFits(t, mux) })
	t.Run("sessions, search and mishaps leave the split", func(t *testing.T) { pnScreensLeaveTheSplit(t, mux) })
	t.Run("an action in the panel answers the panel", func(t *testing.T) { pnActionInThePanel(t, mux, db) })
}

// pnWalkPages are the pages the plan's "Done when" names (the Memories list
// is UX 28's, walked by its own tests), and the screens that left the split.
var pnWalkPages = []string{
	"/console/projects/example-ops?tab=tasks", // a project's Plans & tasks tab, its plan cards
	"/console/",             // the Overview
	"/console/plans",        // the Plans board's plan cards
	"/console/search?q=ops", // Search
	"/console/sessions",     // Sessions
	"/console/mishaps",      // Mishaps
}

func pnDoneWalk(t *testing.T, mux *http.ServeMux) {
	for _, page := range pnWalkPages {
		body := pnGet(t, mux, page)
		require.Equal(t, 1, strings.Count(body, `id="read-panel"`), "%s carries the panel", page)
		read := pnRead(t, body)
		require.NotEmpty(t, read, "%s has links that read in place", page)
		kinds := map[string]bool{}
		for _, href := range read {
			k := pnKind(href)
			if kinds[k] {
				continue
			}
			kinds[k] = true
			rr := getPeek(t, mux, pnPeek(href))
			require.Equal(t, http.StatusOK, rr.Code, "%s: %s reads as %s", page, href, pnPeek(href))
			require.True(t, strings.HasPrefix(strings.TrimSpace(rr.Body.String()), `<article class="peek-entity`), "%s: %s is a fragment", page, href)
		}
	}
	// The plan links name their project, and the fragment keeps it.
	read := pnRead(t, pnGet(t, mux, "/console/projects/example-ops?tab=tasks"))
	var plan string
	for _, href := range read {
		if pnKind(href) == "plans" {
			plan = href
			break
		}
	}
	require.NotEmpty(t, plan)
	require.Contains(t, plan, "?project=example-ops")
	require.Contains(t, pnPeek(plan), "project=example-ops", "peek=1 is added to the link's own query")
	// A project link is not read in place: it goes to the project.
	for _, page := range pnWalkPages {
		for _, href := range pnRead(t, pnGet(t, mux, page)) {
			require.NotEqual(t, "projects", pnKind(href), "%s: %s", page, href)
		}
	}
}

// Every kind the panel reads has a fragment that draws inside the panel: a
// fragment alone, never a page, and its root names the item's project for the
// panel's header (a plan's link names it instead).
func pnEveryFragmentFits(t *testing.T, mux *http.ServeMux) {
	found := map[string]string{}
	for _, page := range append(slices.Clone(pnWalkPages), "/console/trials") {
		for _, href := range pnRead(t, pnGet(t, mux, page)) {
			if k := pnKind(href); found[k] == "" {
				found[k] = href
			}
		}
	}
	for _, kind := range []string{"memories", "notes", "tasks", "plans", "sessions", "events", "trials"} {
		href := found[kind]
		require.NotEmpty(t, href, "the walk finds a %s link", kind)
		rr := getPeek(t, mux, pnPeek(href))
		require.Equal(t, http.StatusOK, rr.Code, kind)
		require.True(t, strings.HasPrefix(rr.Header().Get("Content-Type"), "text/html"), kind)
		body := strings.TrimSpace(rr.Body.String())
		require.True(t, strings.HasPrefix(body, `<article class="peek-entity`), "%s: %.80s", kind, body)
		for _, page := range []string{"<html", "<main", `class="sidebar"`, `id="read-panel"`, "<script"} {
			require.NotContains(t, body, page, "%s: a fragment, never a page inside the panel", kind)
		}
		root := body[:strings.Index(body, ">")]
		if kind == "plans" {
			require.Contains(t, href, "?project=", "a plan's link names its project for the header")
			continue
		}
		require.Contains(t, root, `data-panel-project="`, "%s names its project", kind)
	}
	// A stale link answers a fragment-shaped error the panel draws as it is.
	rr := getPeek(t, mux, "/console/memories/01NOSUCHMEMORY0000000000000?peek=1")
	require.Equal(t, http.StatusNotFound, rr.Code)
	require.True(t, strings.HasPrefix(rr.Header().Get("Content-Type"), "text/html"))
	require.True(t, strings.HasPrefix(rr.Body.String(), `<article class="peek-entity">`))
}

// Sessions, Search and Mishaps read in the panel now: their lists stay full
// width, with no pane of their own. The Diary and Interactions keep theirs.
func pnScreensLeaveTheSplit(t *testing.T, mux *http.ServeMux) {
	for _, page := range []string{"/console/sessions", "/console/search?q=ops", "/console/mishaps"} {
		body := pnGet(t, mux, page)
		main := body[strings.Index(body, `<main class="main"`):strings.Index(body, "</main>")]
		require.NotContains(t, main, `id="detail-pane"`, page)
		require.NotContains(t, main, "list-detail-split", page)
		require.Contains(t, main, `<div class="ops-list`, page)
		// Their rows read in place: each row's href is an item the panel reads,
		// but for a project's (a Search result), which opens the project.
		rows := regexp.MustCompile(`class="[^"]*\brow-link\b[^"]*"[^>]*data-href="([^"]+)"`).FindAllStringSubmatch(main, -1)
		require.NotEmpty(t, rows, page)
		for _, m := range rows {
			require.True(t, pnItem(t).MatchString(m[1]) || pnKind(m[1]) == "projects", "%s: row %s reads in place", page, m[1])
		}
	}
	for _, name := range []string{"diary.html", "interactions.html"} {
		require.Contains(t, lvTemplate(t, name), "list-detail-split", "%s keeps its own pane", name)
	}
	entries, err := templateFS.ReadDir("templates")
	require.NoError(t, err)
	for _, e := range entries {
		switch e.Name() {
		case "diary.html", "interactions.html", "layout.html":
			continue
		}
		require.NotContains(t, lvTemplate(t, e.Name()), "list-detail-split", "%s: a list reads in the panel, not in a split", e.Name())
		require.NotContains(t, lvTemplate(t, e.Name()), `id="detail-pane"`, e.Name())
	}
	// The rules only the moved screens used are gone with them.
	css := string(consoleCSS)
	for _, gone := range []string{".sessions-split", ".sessions-stage .list-detail-split", ".list-detail-split .sess-row.selected",
		".ops-page .list-detail-split .sess-row.selected", ".search-result.selected"} {
		require.NotContains(t, css, gone)
	}
	for _, kept := range []string{".sessions-stage .ops-list { margin-top: 10px; }", ".ops-page .sess-row[data-panel-current],",
		".search-result[data-panel-current], .search-result[data-panel-current]:hover {", ".read-panel .prose { max-height: none; overflow: visible;",
		".read-panel {", ".read-scrim {", "@media (max-width: 720px) {\n  .read-scrim { left: 0; }\n  .read-panel { width: 100%; border-left: 0; }"} {
		require.Contains(t, css, kept)
	}
}

// pnBrowserPost is a signed-in browser's POST from a console page, as
// panel.js sends it: the cookie, same-origin, asking for the redirect to be
// named (X-Seam-Follow).
func pnBrowserPost(mux *http.ServeMux, path string, form url.Values, site string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: consoleToken(testKey)})
	req.Header.Set("Sec-Fetch-Site", site)
	req.Header.Set(followHeader, "1")
	return do(mux, req)
}

// An action taken in the panel (Mark done) is answered with the named
// redirect the panel reads its notice from; the panel then draws the item
// again from its fragment, which shows the new state. A cross-site post is
// refused (console-csrf-origin-check-contract) and changes nothing.
func pnActionInThePanel(t *testing.T, mux *http.ServeMux, db *sql.DB) {
	var task, action string
	for _, page := range pnWalkPages {
		for _, href := range pnRead(t, pnGet(t, mux, page)) {
			if pnKind(href) != "tasks" || task != "" {
				continue
			}
			frag := pnGet(t, mux, pnPeek(href))
			m := regexp.MustCompile(`<form method="post" action="(/console/tasks/[0-9A-Z]+/status)">\s*(?:<input type="hidden" name="next" value="[^"]*">)?<input type="hidden" name="to" value="done">`).FindStringSubmatch(frag)
			if m != nil {
				task, action = href, m[1]
			}
		}
	}
	require.NotEmpty(t, task, "the walk finds a task the panel offers Mark done on")
	id := strings.TrimSuffix(strings.TrimPrefix(action, "/console/tasks/"), "/status")

	refused := pnBrowserPost(mux, action, url.Values{"to": {"done"}}, "cross-site")
	require.Equal(t, http.StatusForbidden, refused.Code, "a post from another site is refused")
	before, err := store.TaskByID(t.Context(), db, id)
	require.NoError(t, err)
	require.NotEqual(t, "done", string(before.Status))

	rr := pnBrowserPost(mux, action, url.Values{"to": {"done"}}, "same-origin")
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	named, err := url.Parse(rr.Header().Get(locationHeader))
	require.NoError(t, err)
	require.NotEmpty(t, named.Query().Get("notice"), "the panel shows the action's own line: %s", named)
	after, err := store.TaskByID(t.Context(), db, id)
	require.NoError(t, err)
	require.Equal(t, "done", string(after.Status))
	again := pnGet(t, mux, pnPeek(task))
	require.NotContains(t, again, `name="to" value="done"`, "drawn again, the item offers Mark done no more")
}
