package console

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// tlRow is the Tasks list's row for task id in page: from its opening tag
// to the row's close (a row holds no <div> of its own).
func tlRow(t *testing.T, page, id string) string {
	t.Helper()
	at := strings.Index(page, `<div class="tl-row row-link" id="tl-`+id+`"`)
	require.GreaterOrEqual(t, at, 0, "no row for %s", id)
	end := strings.Index(page[at:], "</div>")
	require.Greater(t, end, 0)
	return page[at : at+end]
}

// tlRowNamed is the Tasks list's row whose title is title (a starred one's
// title follows its star).
func tlRowNamed(t *testing.T, page, title string) string {
	t.Helper()
	at := strings.Index(page, "data-list-row>"+title+"</a>")
	if at < 0 {
		at = strings.Index(page, `</span>`+title+`</a>`)
	}
	require.GreaterOrEqual(t, at, 0, "no row is titled %q", title)
	start := strings.LastIndex(page[:at], `<div class="tl-row`)
	require.GreaterOrEqual(t, start, 0)
	end := strings.Index(page[at:], "</div>")
	require.Greater(t, end, 0)
	return page[start : at+end]
}

// tlGroup is the markup of the Tasks list's group key, "" when the page
// draws none.
func tlGroup(page, key string) string {
	at := strings.Index(page, `id="tl-g-`+key+`"`)
	if at < 0 {
		return ""
	}
	rest := page[at:]
	end := len(rest)
	for _, closer := range []string{`class="ml-group tl-group`, `<footer class="ml-foot tl-foot"`} {
		if i := strings.Index(rest[1:], closer); i >= 0 && i+1 < end {
			end = i + 1
		}
	}
	return rest[:end]
}

// tlGroupIDs is the ids of the rows group key draws, in order.
func tlGroupIDs(page, key string) []string {
	var ids []string
	group := tlGroup(page, key)
	for {
		at := strings.Index(group, `<div class="tl-row row-link" id="tl-`)
		if at < 0 {
			return ids
		}
		group = group[at+len(`<div class="tl-row row-link" id="tl-`):]
		ids = append(ids, group[:strings.Index(group, `"`)])
	}
}

// Ready draws its first readyShown tasks and says how many more there are,
// with a link that draws every one, landing on the first of them; ready
// takes all or nothing else.
func TestTlReadyShowsTheFirstTenThenMore(t *testing.T) {
	db, mux := newConsole(t)
	base := time.Now().UTC().Add(-time.Hour)
	var ids []string
	for i := range readyShown + 2 {
		at := base.Add(time.Duration(i) * time.Minute)
		ids = append(ids, tlTask(t, db, "example-ops", "", fmt.Sprintf("tl ready %02d", i), func(k *core.Task) {
			k.CreatedAt, k.UpdatedAt = at, at
		}).ID)
	}
	page := getHTMLBody(t, mux, "/console/tasks?project=example-ops")
	require.Equal(t, ids[:readyShown], tlGroupIDs(page, "ready"), "the first ten, oldest first")
	require.Contains(t, tlGroup(page, "ready"), `<span class="ml-note">12</span>`)
	require.Contains(t, page, `<p class="tl-more">2 more ready &middot; <a href="/console/tasks?project=example-ops&amp;ready=all#tl-`+ids[readyShown]+`" data-seam-query data-ready-more>show them</a></p>`)
	all := getHTMLBody(t, mux, "/console/tasks?project=example-ops&ready=all")
	require.Equal(t, ids, tlGroupIDs(all, "ready"))
	require.NotContains(t, all, "tl-more")
	require.Contains(t, tlRow(t, all, ids[0]), `href="/console/tasks/`+ids[0]+`?project=example-ops&amp;ready=all"`, "a row keeps the state")
	data := bkPage(t, mux, "/console/tasks?format=json")
	require.Len(t, data.Ready, readyShown+2, "the JSON keeps every ready task")
	require.Empty(t, data.Filter.Ready)
	require.Equal(t, "all", bkPage(t, mux, "/console/tasks?format=json&ready=all").Filter.Ready)
	code, body := bkGet(mux, "/console/tasks?ready=some", true)
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, `invalid ready "some": valid values are all (or omit)`, bkError(t, body))
}

// Recently closed is the last week's, folded; an older closed task is a
// link away (All N closed tasks), and its page places it in that list. The
// JSON's closed page is the newest closed tasks, as it always was.
func TestTlRecentlyClosedIsTheLastWeek(t *testing.T) {
	db, mux := newConsole(t)
	now := time.Now().UTC()
	recent, old := now.Add(-3*24*time.Hour), now.Add(-10*24*time.Hour)
	tlTask(t, db, "example-ops", "", "tl open", nil)
	fresh := tlTask(t, db, "example-ops", "", "tl closed this week", func(k *core.Task) { k.Status, k.ClosedAt = core.TaskDone, &recent })
	stale := tlTask(t, db, "example-ops", "", "tl closed long ago", func(k *core.Task) { k.Status, k.ClosedAt = core.TaskDropped, &old })
	page := getHTMLBody(t, mux, "/console/tasks")
	require.Equal(t, []string{fresh.ID}, tlGroupIDs(page, "closed"))
	require.Contains(t, tlGroup(page, "closed"), `<h2>Recently closed</h2><span class="ml-gloss">closed in the last 7 days</span><span class="ml-note">1</span>`)
	require.Contains(t, page, `<a class="ml-more" href="/console/tasks?closed=all#tl-g-closed">All 2 closed tasks`)
	require.Contains(t, page, `>1 closed this week</a>`)
	data := bkPage(t, mux, "/console/tasks?format=json")
	require.Equal(t, 1, data.ClosedRecent)
	require.Equal(t, []string{"tl closed this week", "tl closed long ago"}, bkTitles(data.Closed))
	all := getHTMLBody(t, mux, "/console/tasks?closed=all")
	require.Equal(t, []string{fresh.ID, stale.ID}, tlGroupIDs(all, "closed"))
	require.Contains(t, all, `<a href="#tl-g-closed" data-pill="closed" title="every closed task, newest closed first">2 closed</a>`)

	// The item page: the old task is in the list that holds every closed
	// task, and in no default list.
	pos, _, _ := itPlace(t, itFrame(t, getHTMLBody(t, mux, "/console/tasks/"+stale.ID+"?closed=all")))
	require.True(t, strings.HasPrefix(pos, "3 of 3"), pos)
	pos, _, _ = itPlace(t, itFrame(t, getHTMLBody(t, mux, "/console/tasks/"+fresh.ID+"?project=example-ops")))
	require.True(t, strings.HasPrefix(pos, "2 of 2"), pos)
	require.NotContains(t, itFrame(t, getHTMLBody(t, mux, "/console/tasks/"+stale.ID+"?project=example-ops")), "data-item-pos")
}

// The rows' keys are library.js's, learned additively: the Memories and
// Notes lists keep their a.ml-row rows, the Tasks list's rows give j, k and
// Enter their title link, and an action that lands on a row in a fold opens
// it, on a mutation's landing only. The script never reloads the document.
func TestTlLibraryJSTakesTheTaskRows(t *testing.T) {
	js := string(libraryJS)
	for _, want := range []string{
		"var rows = Array.prototype.slice.call(document.querySelectorAll('.main [data-ml-list] a.ml-row'));\n    if (rows.length) return rows;",
		"document.querySelectorAll('.main [data-ml-list] a[data-list-row]')",
		"var LIST_ROW = 'a.ml-row, a[data-list-row]';",
		"if (!e.detail || e.detail.source !== 'mutation') return;",
		"row.closest('.main [data-ml-list] details:not([open])')",
	} {
		require.Contains(t, js, want)
	}
	require.NotContains(t, js, "location.href =")
	src, err := templateFS.ReadFile("templates/tasks.html")
	require.NoError(t, err)
	page := string(src)
	require.Contains(t, page, `<section class="ml-list tl-list" id="tl-list" aria-label="Tasks" data-ml-list>`)
	require.Contains(t, page, `<a class="tl-title" id="tl-a-{{.ID}}" href="{{.Href}}" data-peek data-list-row>`)
	require.Contains(t, page, `<div class="tl-row row-link" id="tl-{{.ID}}" data-href="{{.Href}}"`,
		"a click on the row reads the title's task: the panel steps through the rows")
	require.Contains(t, page, `{{template "project-switcher" $v.Scope}}`)
	require.NotContains(t, page, `lib-rail`)
	require.NotContains(t, page, `data-auto-url`)
}
