package console

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// The library screens (Memories / Notes / Tasks / Plans / Labs / Trials) render
// one unified page: orientation chrome, a grouped rail, and a reader. These
// tests cover the reader-specific behavior; the JSON list payloads and ?peek=1
// fragments are covered in the entity and peek tests.

func TestLibraryScreens_ShareOrientationChrome(t *testing.T) {
	_, mux := newConsole(t)
	for _, tc := range []struct {
		path string
		kind string
	}{
		{path: "/console/memories", kind: "memories"},
		{path: "/console/notes", kind: "notes"},
		{path: "/console/tasks", kind: "tasks"},
		{path: "/console/plans", kind: "plans"},
		{path: "/console/labs", kind: "labs"},
		{path: "/console/trials", kind: "trials"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			html := getPeek(t, mux, tc.path)
			require.Equal(t, http.StatusOK, html.Code)
			body := html.Body.String()
			require.Contains(t, body, `class="lib-page" data-library="`+tc.kind+`"`)
			// One compact title row per screen, and its count is labeled --
			// never a bare total the reader has to reconcile with the rail.
			require.Contains(t, body, `class="mv2-bar"`)
			require.Contains(t, body, `class="mv2-count"`)
			require.NotContains(t, body, `class="lib-hero"`)
			require.NotContains(t, body, `class="lib-controlbar"`)
		})
	}
}

func TestLibraryReaderStyles_FillAvailableColumn(t *testing.T) {
	css := string(consoleCSS)
	rule := func(selector string) string {
		t.Helper()
		start := strings.Index(css, selector+" {")
		require.NotEqual(t, -1, start, "missing %s rule", selector)
		end := strings.Index(css[start:], "}")
		require.NotEqual(t, -1, end, "unterminated %s rule", selector)
		return css[start : start+end]
	}

	// The shared reader contract keeps the navigation, sheet, and rendered
	// document aligned to the right edge of every library page. In particular,
	// none of these may regress to the legacy 900px / 72ch caps.
	for _, selector := range []string{".lib-reader", ".reader-nav", ".reader-sheet"} {
		got := rule(selector)
		require.Contains(t, got, `width: 100%`)
		require.NotContains(t, got, `max-width`)
	}
	doc := rule(".prose.doc")
	require.Contains(t, doc, `width: 100%`)
	require.Contains(t, doc, `max-width: none`)
}

// The Notes list opens no note (plan:console-ux-pass UX 29): no reader, no
// auto-open, the rows newest first, each a link to the note's own page.
func TestNotesLibrary_ListOpensNoNote(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	older := writeNote(t, mgr, "seamless", "n-old", "Older Note", "the older body")
	newer := writeNote(t, mgr, "seamless", "n-new", "Newer Note", "the newer body")

	html := getPeek(t, mux, "/console/notes")
	require.Equal(t, http.StatusOK, html.Code)
	body := html.Body.String()
	require.Equal(t, []string{newer.ID, older.ID}, nlRows(body))
	for _, gone := range []string{`id="lib-reader"`, `class="lib-rail"`, "data-auto-url=", "the newer body", `aria-current="page"`} {
		require.NotContains(t, body, gone)
	}
}

func TestBuildNoteGroups_RecentOrdersNewestFirst(t *testing.T) {
	base := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	groups := buildNoteGroups(map[string][]noteRow{
		"seamless": {
			{ID: "OLD", Title: "Old", Updated: base},
			{ID: "NEW", Title: "New", Updated: base.Add(time.Hour)},
		},
	}, "recent")

	require.Len(t, groups, 1)
	require.Equal(t, []string{"NEW", "OLD"}, []string{groups[0].Notes[0].ID, groups[0].Notes[1].ID})
}

func TestNotesLibrary_DetailPageAndReaderFragment(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	n := writeNote(t, mgr, "seamless", "n-doc", "Doc Note", "full document body")
	writeNote(t, mgr, "seamless", "n-other", "Other Note", "other body")

	// The detail URL renders the note's own page: the note alone, no library
	// beside it (plan:console-ux-pass UX 26).
	page := getPeek(t, mux, "/console/notes/"+n.ID)
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()
	require.Contains(t, body, `<div class="item-page" data-item="note" data-item-id="`+n.ID+`"`)
	require.Contains(t, body, "full document body")
	require.NotContains(t, body, `id="lib-reader"`)
	require.NotContains(t, body, `class="rail-item`)
	require.NotContains(t, body, "data-auto-url=", "an explicit selection is not client-pinned")

	// ?reader=1 returns just the reader fragment for the in-place swap.
	frag := getPeek(t, mux, "/console/notes/"+n.ID+"?reader=1")
	require.Equal(t, http.StatusOK, frag.Code)
	require.NotContains(t, frag.Body.String(), "<html")
	require.Contains(t, frag.Body.String(), "reader-sheet")
	require.Contains(t, frag.Body.String(), "full document body")

	// The list filter survives on the detail URL: a bogus sort is still a 400.
	bad := getPeek(t, mux, "/console/notes/"+n.ID+"?sort=bogus")
	require.Equal(t, http.StatusBadRequest, bad.Code)
}

func TestMemoriesLibrary_DetailAndReaderFragment(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	m := writeMemory(t, mgr, core.KindGotcha, "seamless", "lib-mem", "a memory description")

	// The detail URL renders the memory's own page: the memory alone, in its
	// kind's color, no library beside it (plan:console-ux-pass UX 26).
	page := getPeek(t, mux, "/console/memories/"+m.ID)
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()
	require.Contains(t, body, `<div class="item-page" data-item="memory" data-item-id="`+m.ID+`" data-memory-kind="gotcha">`)
	require.Contains(t, body, "body of lib-mem")
	require.NotContains(t, body, `id="lib-reader"`)
	require.NotContains(t, body, `class="rail-item`)
	require.NotContains(t, body, `class="mv2-bar"`, "nor the list's bar")
	require.Contains(t, getPeek(t, mux, "/console/memories/"+m.ID+"?group=project").Body.String(), `<div class="item-page"`,
		"a list's grouping on the URL draws the same page")

	// The list is one list, no reader (plan:console-ux-pass UX 28): each row
	// names its kind in its color, and its project.
	list := getPeek(t, mux, "/console/memories").Body.String()
	require.NotContains(t, list, `id="lib-reader"`)
	require.NotContains(t, list, "body of lib-mem", "the list opens no memory")
	require.Contains(t, list, `<a class="ml-row" id="ml-`+m.ID+`" href="/console/memories/`+m.ID+`?sort=recent" data-peek data-memory-kind="gotcha">`)
	require.Contains(t, list, `<span class="ml-kind" title="`+kindGloss("gotcha")+`">gotcha</span>`, "each row names its kind")
	require.Contains(t, list, `<span class="ml-proj">seamless</span>`)
	require.Contains(t, list, `class="menu-k">Sort<`, "the compact bar still names its sort control")
	require.Contains(t, list, `<strong>Recent</strong>`, "the sort menu shows the active choice without opening")

	frag := getPeek(t, mux, "/console/memories/"+m.ID+"?reader=1")
	require.Equal(t, http.StatusOK, frag.Code)
	require.NotContains(t, frag.Body.String(), "<html")
	require.Contains(t, frag.Body.String(), "body of lib-mem")
	require.Contains(t, frag.Body.String(), "archive", "the reader carries the archive action")
}

// The Tasks screen is one list (plan:console-ux-pass UX 30): no reader
// beside it and no task opened on the list URL. A task's ?reader=1 fragment
// still answers.
func TestTlTasksScreenHasNoReader(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	mk := func(id, title string, status core.TaskStatus) {
		require.NoError(t, store.CreateTask(ctx, db, core.Task{
			ID: id, ProjectSlug: "seamless", Title: title, Body: "do " + title,
			Status: status, CreatedAt: base, UpdatedAt: base,
		}))
	}
	mk("TR", "ready-task", core.TaskOpen)
	mk("TP", "active-task", core.TaskInProgress)

	html := getPeek(t, mux, "/console/tasks")
	require.Equal(t, http.StatusOK, html.Code)
	body := html.Body.String()
	require.NotContains(t, body, `id="lib-reader"`)
	require.Contains(t, body, "data-ml-list")
	require.Contains(t, body, ">active-task<")
	require.NotContains(t, body, "do active-task", "the list draws no task's body")

	frag := getPeek(t, mux, "/console/tasks/TR?reader=1")
	require.Equal(t, http.StatusOK, frag.Code)
	require.NotContains(t, frag.Body.String(), "<html")
	require.Contains(t, frag.Body.String(), "do ready-task")
}

func TestListQS(t *testing.T) {
	require.Equal(t, "", libQuery{Sort: "recent"}.QS())
	require.Equal(t, "?q=x", libQuery{Sort: "recent", Q: "x"}.QS())
	require.Equal(t, "?sort=name", libQuery{Sort: "name"}.QS())
	require.Equal(t, "?q=a+b&sort=name", libQuery{Sort: "name", Q: "a b"}.QS())
}

// libMemory writes a memory with explicit times, so a test can order rows.
func libMemory(t *testing.T, mgr *files.Manager, kind core.MemoryKind, project, name string, at time.Time) core.Memory {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	m, err := mgr.WriteMemory(context.Background(), core.Memory{
		ID: id, Kind: kind, Name: name, Description: "about " + name, Project: project,
		Body: "body of " + name, Created: at, Updated: at, ValidFrom: at,
	})
	require.NoError(t, err)
	return m
}

// libSessionLink finds the session links in a stretch of markup, in order.
var libSessionLink = regexp.MustCompile(`href="/console/sessions/([^"]+)"`)

func libSessionIDs(markup string) []string {
	var ids []string
	for _, m := range libSessionLink.FindAllStringSubmatch(markup, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// libSession registers a session, so the reader can name it.
func libSession(t *testing.T, db *sql.DB, id, name, project string, at time.Time) {
	t.Helper()
	require.NoError(t, store.CreateSession(context.Background(), db, core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: core.SessionCompleted, CreatedAt: at, UpdatedAt: at,
	}))
}

// libInject records one retrieval.injected event naming items, as a
// briefing does: they surfaced to session (none for "").
func libInject(t *testing.T, db *sql.DB, session, project string, at time.Time, items ...string) {
	t.Helper()
	_, err := events.NewRecorder(db).Record(context.Background(), core.Event{
		Kind: core.EventInjected, SessionID: session, ProjectSlug: project, TS: at,
		Payload: map[string]any{"item_ids": items},
	})
	require.NoError(t, err)
}

// libMain is a page's library markup, or an item's own page, without the
// layout's chrome.
func libMain(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<div class="lib-page"`)
	if start == -1 {
		start = strings.Index(body, `<div class="item-page"`)
	}
	require.NotEqual(t, -1, start, "no library page")
	end := strings.Index(body[start:], "</main>")
	require.NotEqual(t, -1, end, "unterminated library page")
	return body[start : start+end]
}

// With Recent the list is one list, newest first, whatever the project or
// kind, each row naming its project and kind; the list opens no memory. The
// grouping by project is gone from the page (choosing a project replaces
// it): ?group=project and Name draw the same one list, in their order. The
// JSON answer keeps its groups either way.
func TestLibRecentIsOneListNewestFirst(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	old := libMemory(t, mgr, core.KindConstraint, "", "aa-global-old", base)
	mid := libMemory(t, mgr, core.KindReference, "alpha", "mm-middle", base.Add(time.Hour))
	newest := libMemory(t, mgr, core.KindGotcha, "zeta", "zz-newest", base.Add(2*time.Hour))

	body := getHTMLBody(t, mux, "/console/memories")
	require.Equal(t, []string{newest.ID, mid.ID, old.ID}, mlRows(body),
		"with Recent, the newest memory is the first row, whatever its project or kind")
	require.NotContains(t, body, "data-auto-url", "and the list opens none")
	require.Contains(t, body, `<span class="ml-proj">zeta</span>`, "a row of the one list names its project")
	require.Contains(t, body, `<span class="ml-proj">Shared globally</span>`)
	require.Contains(t, body, `&middot; Newest first`)
	require.NotContains(t, body, `class="lib-group"`, "no Group control")

	grouped := getHTMLBody(t, mux, "/console/memories?group=project")
	require.Equal(t, []string{newest.ID, mid.ID, old.ID}, mlRows(grouped), "an old grouping on the URL draws the same list")
	byName := getHTMLBody(t, mux, "/console/memories?sort=name&group=none")
	require.Equal(t, []string{old.ID, mid.ID, newest.ID}, mlRows(byName), "Name: one list, A to Z")

	var data memoriesData
	getJSON(t, mux, "/console/memories?format=json", &data)
	require.Len(t, data.Groups, 3)
	require.Equal(t, "", data.Groups[0].Project)
	require.Equal(t, 3, data.Total)
	require.Equal(t, 3, data.ActiveCount)

	n1 := libNote(t, mgr, "", "inbox-old", "Inbox old", base, nil)
	n2 := libNote(t, mgr, "zeta", "zeta-new", "Zeta new", base.Add(time.Hour), nil)
	require.Equal(t, []string{n2.ID, n1.ID}, nlRows(getHTMLBody(t, mux, "/console/notes")),
		"with Recent, the newest note is the first row")
	require.Equal(t, []string{n2.ID, n1.ID}, nlRows(getHTMLBody(t, mux, "/console/notes?group=project")),
		"an old grouping on the URL draws the same list")
}

// The filters combine, in the URL, on active and inactive memories alike; each
// menu counts what its own choice would show; a value the page cannot read is
// a 400 that names what it takes.
func TestLibFiltersCombine(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	base := time.Now().UTC().Add(-time.Hour)
	never := libMemory(t, mgr, core.KindGotcha, "pay", "pay-never", base)
	seen := libMemory(t, mgr, core.KindGotcha, "pay", "pay-seen", base.Add(time.Minute))
	rule := libMemory(t, mgr, core.KindConstraint, "pay", "pay-rule", base.Add(2*time.Minute))
	web := libMemory(t, mgr, core.KindGotcha, "web", "web-trap", base.Add(3*time.Minute))
	shared := libMemory(t, mgr, core.KindGotcha, "", "shared-trap", base.Add(4*time.Minute))
	gone := libMemory(t, mgr, core.KindGotcha, "pay", "pay-gone", base.Add(5*time.Minute))
	for _, m := range []core.Memory{never, rule, web, shared} {
		require.Equal(t, http.StatusSeeOther, postFavorite(mux, "memory", m.ID, url.Values{"favorite": {"1"}}).Code)
	}
	libInject(t, db, "S1", "pay", base, seen.ID)
	require.Equal(t, http.StatusSeeOther, postForm(mux, "/console/memories/"+gone.ID+"/archive", "").Code)

	names := func(path string) []string {
		t.Helper()
		var d memoriesData
		getJSON(t, mux, path, &d)
		require.Equal(t, 5, d.Total, "%s: the total is every active memory", path)
		var out []string
		for _, g := range d.Groups {
			for _, k := range g.Kinds {
				for _, m := range k.Memories {
					out = append(out, m.Name)
				}
			}
		}
		require.Len(t, out, d.ActiveCount, path)
		for _, m := range d.Inactive {
			out = append(out, m.Name+" (inactive)")
		}
		slices.Sort(out)
		return out
	}
	for path, want := range map[string][]string{
		"/console/memories?format=json&kind=gotcha":                   {"pay-gone (inactive)", "pay-never", "pay-seen", "shared-trap", "web-trap"},
		"/console/memories?format=json&kind=gotcha&project=pay":       {"pay-gone (inactive)", "pay-never", "pay-seen"},
		"/console/memories?format=json&kind=gotcha&project=pay&fav=1": {"pay-never"},
		"/console/memories?format=json&project=pay&surfaced=never":    {"pay-gone (inactive)", "pay-never", "pay-rule"},
		"/console/memories?format=json&project=global":                {"shared-trap"},
		"/console/memories?format=json&project=_global&fav=1":         {"shared-trap"},
		"/console/memories?format=json&fav=1&q=trap":                  {"shared-trap", "web-trap"},
	} {
		require.Equal(t, want, names(path), path)
	}

	main := libMain(t, getHTMLBody(t, mux, "/console/memories?kind=gotcha&project=pay&fav=1"))
	require.Contains(t, main, "<strong>1</strong>of 5 active", "the header says how many of all")
	require.Contains(t, main, `<a class="ml-chip on" href="/console/memories?fav=1&amp;kind=gotcha&amp;project=pay"`, "the gotcha chip is the choice")
	require.Contains(t, main, `aria-label="Project: pay"`)
	require.Contains(t, main, `class="lib-flag on" href="/console/memories?kind=gotcha&amp;project=pay"`,
		"the Starred toggle is on, and its link turns it off and keeps the rest")
	require.Contains(t, main, `class="lib-clear" href="/console/memories"`)
	for _, keep := range []string{`name="fav" value="1"`, `name="kind" value="gotcha"`, `name="project" value="pay"`} {
		require.Contains(t, main, `<input type="hidden" `+keep+`>`, "typing keeps the other filters")
	}
	require.Contains(t, main, `href="/console/memories?fav=1&amp;kind=constraint&amp;project=pay" title="`+kindGloss("constraint")+`"><i class="kdot" data-kind="constraint"></i>constraint<span class="n">1</span>`,
		"the constraint chip counts what choosing it would show")
	require.Contains(t, main, `href="/console/memories?fav=1&amp;kind=gotcha&amp;project=web">web<span class="n">1</span>`)
	require.Contains(t, main, `Never surfaced<span class="n">1</span>`)

	for query, want := range map[string]string{
		"kind=bogus":     `invalid kind "bogus": valid values are constraint, convention`,
		"group=flat":     `invalid group "flat": valid values are none, project`,
		"fav=yes":        `invalid fav "yes": valid values are 1 (or omit)`,
		"surfaced=stale": `invalid surfaced "stale": valid values are never (or omit)`,
		"project=all":    `invalid project "all": name a project's slug, or global for the global scope`,
		"sort=bogus":     `invalid sort "bogus": valid values are recent, name, reach, utility, favorites`,
	} {
		for _, path := range []string{"/console/memories?", "/console/memories/" + never.ID + "?", "/console/memories/" + never.ID + "?reader=1&"} {
			rr := getPeek(t, mux, path+query)
			require.Equal(t, http.StatusBadRequest, rr.Code, path+query)
			require.Contains(t, html.UnescapeString(rr.Body.String()), want, path+query)
		}
	}
}

// Star, archive and sort keep the library's state: the rail rows, the star's
// and the archive's way back, the sort links and the reader fragment all carry
// it, and the two writes return where they were sent. A memory's own page
// keeps it too: its next link, its star and its archive.
func TestLibStateSurvivesStarArchiveSort(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	base := time.Now().UTC().Add(-time.Hour)
	a := libMemory(t, mgr, core.KindGotcha, "pay", "pay-alpha", base)
	b := libMemory(t, mgr, core.KindGotcha, "pay", "pay-beta", base.Add(time.Minute))
	libMemory(t, mgr, core.KindGotcha, "web", "web-gamma", base.Add(2*time.Minute))
	const qs = "?kind=gotcha&project=pay&q=pay&sort=name"
	esc := strings.ReplaceAll(qs, "&", "&amp;")

	// A row keeps the state, and so does a sort; the list opens no memory.
	list := libMain(t, getHTMLBody(t, mux, "/console/memories"+qs))
	require.Contains(t, list, `id="ml-`+a.ID+`" href="/console/memories/`+a.ID+esc+`"`, "a row keeps the state")
	require.Contains(t, list, `href="/console/memories?kind=gotcha&amp;project=pay&amp;q=pay&amp;sort=recent">Recent</a>`,
		"a sort keeps the filters")
	require.NotContains(t, list, `name="next"`, "no reader, no star or archive on the list")
	frag := getPeek(t, mux, "/console/memories/"+b.ID+qs+"&reader=1").Body.String()
	require.Contains(t, frag, `value="/console/memories/`+b.ID+esc+`" data-lib-next>`, "so does the fragment the rail swaps in")

	main := libMain(t, getHTMLBody(t, mux, "/console/memories/"+a.ID+qs))
	require.Contains(t, main, `href="/console/memories/`+b.ID+esc+`" rel="next" data-item-step="next"`,
		"the page's next memory keeps the state")
	require.Contains(t, main, `<input type="hidden" name="next" value="/console/memories/`+a.ID+esc+`">`,
		"the page's star and archive return to this page with every filter")
	require.Equal(t, 2, strings.Count(main, `<input type="hidden" name="next" value="/console/memories/`+a.ID+esc+`">`))

	rr := postFavorite(mux, "memory", a.ID, url.Values{"favorite": {"1"}, "next": {"/console/memories/" + a.ID + qs}})
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/memories/"+a.ID+qs, rr.Header().Get("Location"))

	rr = postForm(mux, "/console/memories/"+a.ID+"/archive", url.Values{"next": {"/console/memories" + qs + "&notice=old"}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	loc, err := url.Parse(rr.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "/console/memories", loc.Path)
	require.Equal(t, url.Values{"kind": {"gotcha"}, "project": {"pay"}, "q": {"pay"}, "sort": {"name"},
		"notice": {"Archived pay-alpha."}}, loc.Query())
	// Archiving it again (it is inactive now) returns there too.
	rr = postForm(mux, "/console/memories/"+a.ID+"/archive", url.Values{"next": {"/console/memories" + qs}}.Encode())
	require.Equal(t, "/console/memories"+qs, rr.Header().Get("Location"))

	// An archive from the memory's own page returns there, its state kept:
	// the page now shows it archived, with Restore.
	c := libMemory(t, mgr, core.KindGotcha, "pay", "pay-gamma", base)
	rr = postForm(mux, "/console/memories/"+c.ID+"/archive", url.Values{"next": {"/console/memories/" + c.ID + qs + "&notice=old"}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	loc, err = url.Parse(rr.Header().Get("Location"))
	require.NoError(t, err)
	require.Equal(t, "/console/memories/"+c.ID, loc.Path)
	require.Equal(t, url.Values{"kind": {"gotcha"}, "project": {"pay"}, "q": {"pay"}, "sort": {"name"},
		"notice": {"Archived pay-gamma."}}, loc.Query())

	// Anywhere else, an archive goes back to the Memories list: another
	// memory's page is not where the owner archived from.
	for i, next := range []string{"https://example.com/console/memories?q=x", "//example.com/console/memories",
		"/console/settings", "/console/memories/" + b.ID, "/console/memories/../settings",
		"//example.com/console/memories/" + c.ID} {
		m := libMemory(t, mgr, core.KindGotcha, "pay", "pay-victim-"+string(rune('a'+i)), base)
		rr := postForm(mux, "/console/memories/"+m.ID+"/archive", url.Values{"next": {next}}.Encode())
		require.Equal(t, http.StatusSeeOther, rr.Code)
		require.True(t, strings.HasPrefix(rr.Header().Get("Location"), "/console/memories?notice="),
			"%s led to %s", next, rr.Header().Get("Location"))
	}
}

// After a reader swap, the page's sort and Group links follow the reader and
// the star's next is the URL the reader is on (library.js), for the screens
// that keep the rail. Memories and Notes have no reader beside their lists
// (plan:console-ux-pass UX 28, UX 29): their sorts are plain list links, and
// only the reader fragment (?reader=1) still marks its star's next.
func TestLibReaderSwapKeepsThePageState(t *testing.T) {
	js := string(libraryJS)
	require.Contains(t, js, "function followReader(href)")
	require.Contains(t, js, "'.lib-page a[data-lib-here]'")
	require.Contains(t, js, "'input[data-lib-next]'")
	require.Contains(t, js, "followReader(href);")
	// A copy button inside a rail row copies and leaves the reader alone: the
	// button is inside the row's anchor, so the check asks the click's target.
	require.Contains(t, js, "if (e.target.closest('.copy-btn')) return;")
	require.NotContains(t, js, "a.closest('.copy-btn')", "the anchor's ancestors never include its own button")
	for _, name := range []string{"memories", "notes"} {
		src, err := templateFS.ReadFile("templates/" + name + ".html")
		require.NoError(t, err)
		require.NotContains(t, string(src), "data-lib-here", "%s: a sort reorders the list, with no reader to stay on", name)
		require.Equal(t, 1, strings.Count(string(src), "data-lib-next"), "%s: the reader fragment's star", name)
	}
}

// Typing a project's slug or name narrows to that project, the header says how
// many of all, and a miss offers Clear and the wider search.
func TestLibTextFilterMatchesProject(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, db, "example-pay", "Payments Service")
	require.NoError(t, err)
	base := time.Now().UTC().Add(-time.Hour)
	libMemory(t, mgr, core.KindGotcha, "example-pay", "ledger-rounding", base)
	libMemory(t, mgr, core.KindDecision, "example-pay", "retry-budget", base)
	libMemory(t, mgr, core.KindGotcha, "example-web", "css-cache", base)
	libMemory(t, mgr, core.KindReference, "", "style-guide", base)

	for q, want := range map[string]int{"example-pay": 2, "payments service": 2, "PAYMENTS": 2, "global": 1, "example-": 3} {
		var d memoriesData
		getJSON(t, mux, "/console/memories?format=json&q="+url.QueryEscape(q), &d)
		require.Equal(t, want, d.ActiveCount, q)
		require.Equal(t, 4, d.Total, q)
	}
	main := libMain(t, getHTMLBody(t, mux, "/console/memories?q=payments"))
	require.Contains(t, main, "<strong>2</strong>of 4 active")

	none := libMain(t, getHTMLBody(t, mux, "/console/memories?q=zebra&sort=name"))
	require.Contains(t, none, "No memories match &ldquo;zebra&rdquo;.")
	require.Contains(t, none, `<a href="/console/memories?sort=name" data-seam-query>`, "Clear keeps the sort")
	require.Contains(t, none, `<a href="/console/search?q=zebra">`, "and Search looks everywhere")
	narrowed := libMain(t, getHTMLBody(t, mux, "/console/memories?q=zebra&kind=stage"))
	require.Contains(t, narrowed, "No memories match &ldquo;zebra&rdquo; with these filters.")
	unnamed := libMain(t, getHTMLBody(t, mux, "/console/memories?kind=stage"))
	require.Contains(t, unnamed, "No memories match these filters.")
	require.NotContains(t, unnamed, "/console/search?q=")

	libNote(t, mgr, "example-pay", "pay-runbook", "Runbook", base, nil)
	libNote(t, mgr, "example-web", "web-notes", "Web notes", base, nil)
	var nd notesData
	getJSON(t, mux, "/console/notes?format=json&q=payments", &nd)
	require.Equal(t, 1, nd.Count)
	require.Equal(t, 2, nd.Total)
	notes := libMain(t, getHTMLBody(t, mux, "/console/notes?q=payments"))
	require.Contains(t, notes, "<strong>1</strong>of 2 notes")
}

// One word for what reaches an agent's context on these pages: "surfaced",
// the word the console docs define (reach, the knowledge sky). Never
// "injected" or "delivered"; a memory that never surfaced says so.
func TestLibOneWordForSurfacing(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	now := time.Now().UTC()
	seen := libMemory(t, mgr, core.KindGotcha, "pay", "pay-seen", now.Add(-time.Hour))
	quiet := libMemory(t, mgr, core.KindGotcha, "pay", "pay-quiet", now.Add(-2*time.Hour))
	libSession(t, db, "S1", "cc/pay00001", "pay", now)
	libInject(t, db, "S1", "pay", now.Add(-time.Minute), seen.ID)

	words := func(where, markup string) {
		t.Helper()
		lower := strings.ToLower(markup)
		require.Contains(t, lower, "surfaced", where)
		for _, other := range []string{"inject", "deliver"} {
			require.NotContains(t, lower, other, "%s: one word for what reaches an agent", where)
		}
	}
	for _, path := range []string{"/console/memories", "/console/memories/" + seen.ID, "/console/memories/" + quiet.ID,
		"/console/memories?sort=reach", "/console/memories?surfaced=never"} {
		words(path, libMain(t, getHTMLBody(t, mux, path)))
	}
	for _, path := range []string{"/console/memories/" + seen.ID + "?reader=1", "/console/memories/" + quiet.ID + "?reader=1",
		"/console/memories/" + seen.ID + "?peek=1", "/console/memories/" + quiet.ID + "?peek=1"} {
		words(path, getPeek(t, mux, path).Body.String())
	}

	quietReader := getPeek(t, mux, "/console/memories/"+quiet.ID+"?reader=1").Body.String()
	require.Contains(t, quietReader, "never<span>surfaced</span>", "a memory that never surfaced says so")
	require.NotContains(t, quietReader, "never<span>since")
	require.NotContains(t, quietReader, "Surfaced to")
	seenReader := getPeek(t, mux, "/console/memories/"+seen.ID+"?reader=1").Body.String()
	require.Contains(t, seenReader, "<span>since surfaced</span>")
	require.Contains(t, seenReader, "surfaced 1&times;")
	require.Contains(t, seenReader, `<span class="lib-surfaced-k">Surfaced to</span>`)
	require.Contains(t, seenReader, `<a class="mono" href="/console/sessions/S1"`)
	require.Contains(t, seenReader, `>cc/pay00001</a>`)
	require.NotContains(t, seenReader, "<summary>all", "one session needs no list behind a fold")
}

// The reader names the five latest sessions a memory surfaced to, newest
// first, and the rest behind "all N"; past the list's cap it says how many
// are older, from the count the store keeps in step with the list.
func TestLibSurfacedToNamesTheLatestAndAll(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	now := time.Now().UTC()
	m := libMemory(t, mgr, core.KindConstraint, "", "shared-rule", now.Add(-2*time.Hour))
	for i := range 7 {
		id := fmt.Sprintf("S%d", i)
		libSession(t, db, id, fmt.Sprintf("cc/sess000%d", i), "pay", now)
		libInject(t, db, id, "pay", now.Add(time.Duration(i-20)*time.Minute), m.ID)
	}
	libInject(t, db, "", "", now.Add(-time.Hour), m.ID) // an injection that ran in no session

	reader := getPeek(t, mux, "/console/memories/"+m.ID+"?reader=1").Body.String()
	start := strings.Index(reader, `<div class="lib-surfaced-to"`)
	require.NotEqual(t, -1, start)
	block := reader[start:]
	block = block[:strings.Index(block, "</div>")]
	fold := strings.Index(block, "<details")
	require.NotEqual(t, -1, fold, "more than five: the rest fold behind all N")
	require.Equal(t, []string{"S6", "S5", "S4", "S3", "S2"}, libSessionIDs(block[:fold]), "the five latest, newest first")
	require.Contains(t, block, "<summary>all 8</summary>")
	require.Equal(t, []string{"S1", "S0"}, libSessionIDs(block[fold:]))
	require.Contains(t, block[fold:], ">no session</span>")
	require.NotContains(t, block, "earlier")

	many := libMemory(t, mgr, core.KindConstraint, "", "busy-rule", now.Add(-2*time.Hour))
	for i := range surfacedToListed + 2 {
		id := fmt.Sprintf("B%02d", i)
		libSession(t, db, id, "cc/busy"+id, "pay", now)
		libInject(t, db, id, "pay", now.Add(time.Duration(i-200)*time.Minute), many.ID)
	}
	busy := getPeek(t, mux, "/console/memories/"+many.ID+"?reader=1").Body.String()
	require.Contains(t, busy, fmt.Sprintf("<summary>all %d</summary>", surfacedToListed+2))
	require.Contains(t, busy, "and 2 earlier")
}

// The reader's sessions come through the request's handle: a locked
// project's sessions are absent from the read, and so are its memories from
// the filtered list. Without the lock the same pages name them.
func TestLibLockedProjectIsAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	dataDir := filepath.Join(dir, "data")
	mgr, err := files.NewManager(dataDir, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	view, err := store.OpenView(path, []string{"example-locked"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	console := func(read *sql.DB) *http.ServeMux {
		svc, err := New(Config{DB: read, WriteDB: db, Events: events.NewRecorder(db).ReadingFrom(read), Files: mgr,
			DataDir: dataDir, APIKey: testKey, Level: "advanced"})
		require.NoError(t, err)
		mux := http.NewServeMux()
		svc.Register(mux)
		return mux
	}

	now := time.Now().UTC()
	shared := libMemory(t, mgr, core.KindConstraint, "", "shared-rule", now.Add(-time.Hour))
	hidden := libMemory(t, mgr, core.KindGotcha, "example-locked", "locked-trap", now.Add(-time.Hour))
	libSession(t, db, "SOPEN", "cc/open0001", "example-open", now)
	libSession(t, db, "SLOCK", "cc/lock0001", "example-locked", now)
	libInject(t, db, "SOPEN", "example-open", now.Add(-2*time.Minute), shared.ID)
	libInject(t, db, "SLOCK", "example-locked", now.Add(-time.Minute), shared.ID, hidden.ID)

	locked, open := console(view), console(db)
	paths := []string{"/console/memories/" + shared.ID, "/console/memories/" + shared.ID + "?reader=1",
		"/console/memories?project=example-locked", "/console/memories?q=locked", "/console/memories?format=json"}
	for _, p := range paths {
		rr := getPeek(t, locked, p)
		require.Equal(t, http.StatusOK, rr.Code, p)
		for _, gone := range []string{"SLOCK", "cc/lock0001", hidden.ID, "locked-trap"} {
			require.NotContains(t, rr.Body.String(), gone, "%s names the locked project's %s", p, gone)
		}
	}
	require.Contains(t, getPeek(t, locked, paths[1]).Body.String(), ">cc/open0001</a>")
	reader := getPeek(t, open, paths[1]).Body.String()
	require.Contains(t, reader, ">cc/lock0001</a>", "without the lock the reader names it: the absence is the view's")
	require.Contains(t, getPeek(t, open, paths[2]).Body.String(), "locked-trap")
}
