package console

import (
	"context"
	"database/sql"
	"encoding/json"
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

// The Memories list (plan:console-ux-pass UX 28): one full-width list, no
// rail and no reader. Across every project a page of the matches, newest
// first in local-day groups; with a project chosen, all of that project's,
// by kind.

// mlRowRe finds the list's rows, in order, by memory id, with their links.
var mlRowRe = regexp.MustCompile(`<a class="ml-row[^"]*" id="ml-([0-9A-Z]+)" href="([^"]+)"`)

// mlRows is a page's row ids in the order it draws them.
func mlRows(body string) []string {
	var ids []string
	for _, m := range mlRowRe.FindAllStringSubmatch(body, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// mlRowHref is the link of the row for memory id.
func mlRowHref(t *testing.T, body, id string) string {
	t.Helper()
	for _, m := range mlRowRe.FindAllStringSubmatch(body, -1) {
		if m[1] == id {
			return html.UnescapeString(m[2])
		}
	}
	require.FailNow(t, "no row for "+id)
	return ""
}

// mlGroupRe finds the list's groups: their key, label and note.
var mlGroupRe = regexp.MustCompile(`<section class="ml-group" id="ml-g-([a-z]+)"[^>]*>\s*<header class="ml-group-hd"><h2 id="ml-gh-[a-z]+">([^<]+)</h2>(?:<p class="ml-gloss">([^<]*)</p>)?<span class="ml-note">([^<]*)</span>`)

// mlGroup is one group as the page draws it.
type mlGroup struct{ Key, Label, Gloss, Note string }

func mlGroups(body string) []mlGroup {
	var out []mlGroup
	for _, m := range mlGroupRe.FindAllStringSubmatch(body, -1) {
		out = append(out, mlGroup{Key: m[1], Label: html.UnescapeString(m[2]), Gloss: html.UnescapeString(m[3]), Note: m[4]})
	}
	return out
}

// mlMemoryAt writes a memory updated at a time, of a kind, in a project.
func mlMemoryAt(t *testing.T, mgr *files.Manager, kind core.MemoryKind, project, name string, at time.Time) core.Memory {
	t.Helper()
	return libMemory(t, mgr, kind, project, name, at)
}

// mlChip finds a kind chip by its label: its link, its count, whether it is
// the current choice.
var mlChipRe = regexp.MustCompile(`<a class="ml-chip( on)?" href="([^"]+)"(?: title="[^"]*")?(?: aria-current="true")?>(?:<i class="kdot" data-kind="[a-z]+"></i>)?([A-Za-z ]+)<span class="n">(\d+)</span></a>`)

type mlChipRow struct {
	Href  string
	Count string
	On    bool
}

func mlChips(body string) map[string]mlChipRow {
	out := map[string]mlChipRow{}
	for _, m := range mlChipRe.FindAllStringSubmatch(body, -1) {
		out[m[3]] = mlChipRow{Href: html.UnescapeString(m[2]), Count: m[4], On: m[1] != ""}
	}
	return out
}

// A day is counted as the Diary counts it: from the daemon's local midnight,
// a day back with AddDate. This week is the seven days up to today, this
// month the calendar month; a time after now is today.
func TestMlDayGroups(t *testing.T) {
	now := time.Date(2026, 11, 18, 15, 0, 0, 0, time.Local)
	midnight := time.Date(2026, 11, 18, 0, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		at   time.Time
		want int
	}{
		{now.Add(time.Hour), dayToday},
		{now.Add(-time.Hour), dayToday},
		{midnight, dayToday},
		{midnight.Add(-time.Second), dayYesterday},
		{midnight.AddDate(0, 0, -1), dayYesterday},
		{midnight.AddDate(0, 0, -1).Add(-time.Second), dayThisWeek},
		{midnight.AddDate(0, 0, -6), dayThisWeek},
		{midnight.AddDate(0, 0, -6).Add(-time.Second), dayThisMonth},
		{time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local), dayThisMonth},
		{time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local).Add(-time.Second), dayOlder},
		{now.AddDate(-2, 0, 0), dayOlder},
	} {
		require.Equal(t, tc.want, memoryDay(tc.at, now), "%s at %s", tc.at, now)
		require.Equal(t, tc.want, memoryDay(tc.at.UTC(), now.UTC()), "the zone a time is written in does not move its day")
	}
	// Early in a month the week reaches back into the last one, and nothing
	// is earlier this month.
	early := time.Date(2026, 11, 3, 9, 0, 0, 0, time.Local)
	require.Equal(t, dayThisWeek, memoryDay(time.Date(2026, 10, 29, 12, 0, 0, 0, time.Local), early))
	require.Equal(t, dayOlder, memoryDay(time.Date(2026, 10, 27, 12, 0, 0, 0, time.Local), early))

	// The page: newest first, each day in its group, each group counting
	// what it holds.
	_, mgr, svc, _ := itFixture(t, "advanced")
	ctx := context.Background()
	at := map[string]time.Time{
		"m-today-late": now.Add(-time.Minute), "m-today-early": midnight.Add(time.Minute),
		"m-yesterday": midnight.Add(-time.Hour), "m-week": midnight.AddDate(0, 0, -3),
		"m-month": midnight.AddDate(0, 0, -10), "m-older": midnight.AddDate(0, -3, 0),
	}
	ids := map[string]string{}
	for name, when := range at {
		ids[name] = mlMemoryAt(t, mgr, core.KindGotcha, "example-ops", name, when).ID
	}
	data, err := svc.memoriesPageAt(ctx, libQuery{Sort: "recent"}, memoryView{}, now)
	require.NoError(t, err)
	var got [][]string
	for _, g := range data.List.Groups {
		row := []string{g.Key, g.Label, g.Note}
		for _, r := range g.Rows {
			row = append(row, r.Name)
		}
		got = append(got, row)
	}
	require.Equal(t, [][]string{
		{"today", "Today", "2 changed", "m-today-late", "m-today-early"},
		{"yesterday", "Yesterday", "1 changed", "m-yesterday"},
		{"week", "Earlier this week", "1 changed", "m-week"},
		{"month", "Earlier this month", "1 changed", "m-month"},
		{"older", "Older", "1 changed", "m-older"},
	}, got)
	require.False(t, data.List.ByKind)
	require.Equal(t, "Newest first", data.List.Order)

	// Another sort draws one group in its order, without days.
	data, err = svc.memoriesPageAt(ctx, libQuery{Sort: "name"}, memoryView{}, now)
	require.NoError(t, err)
	require.Len(t, data.List.Groups, 1)
	require.Equal(t, "all", data.List.Groups[0].Key)
	require.Equal(t, "A to Z", data.List.Groups[0].Label)
	require.Equal(t, "m-month", data.List.Groups[0].Rows[0].Name)
}

// The list across every project draws 50 rows; Show 50 more is a link to the
// same list, every filter kept, with more rows, which the page patches in
// place (data-seam-query). A group counts all its matches, the ones past the
// page too. A new choice starts at the first rows again, and a row's link
// carries the list's state but not its length.
func TestMlPagingKeepsEveryFilter(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	_, err := store.EnsureProject(context.Background(), db, "example-ops", "Ops")
	require.NoError(t, err)
	// Four days back: every row is in Earlier this week, whatever the hour.
	base := time.Now().UTC().Add(-96 * time.Hour)
	var names []string
	for i := range memoryPageSize + 12 {
		m := mlMemoryAt(t, mgr, core.KindGotcha, "example-ops", fmt.Sprintf("ops-trap-%02d", i), base.Add(time.Duration(i)*time.Minute))
		names = append(names, m.Name)
		if i%2 == 0 {
			require.Equal(t, http.StatusSeeOther, postFavorite(mux, "memory", m.ID, url.Values{"favorite": {"1"}}).Code)
		}
	}
	mlMemoryAt(t, mgr, core.KindDecision, "example-web", "web-choice", base)

	// Unfiltered: 50 of 63, a Show 50 more that adds the 13 left.
	first := getHTMLBody(t, mux, "/console/memories")
	require.Len(t, mlRows(first), memoryPageSize)
	require.Contains(t, first, `Showing <strong>50</strong> of 63 &middot; Newest first`)
	require.Contains(t, first, `<a class="ml-more" href="/console/memories?show=100" data-seam-query>Show 13 more</a>`)
	require.Equal(t, []mlGroup{{Key: "week", Label: "Earlier this week", Note: "63 changed"}}, mlGroups(first),
		"the group counts every match, the rows past this page too")

	// Filtered: the link keeps every filter and the sort.
	const qs = "?fav=1&kind=gotcha&q=trap&sort=name"
	filtered := getHTMLBody(t, mux, "/console/memories"+qs+"&show=20")
	require.Len(t, mlRows(filtered), 20)
	require.Contains(t, filtered, `Showing <strong>20</strong> of 31 &middot; A to Z`)
	more := regexp.MustCompile(`<a class="ml-more" href="([^"]+)" data-seam-query>Show (\d+) more</a>`).FindStringSubmatch(filtered)
	require.NotNil(t, more)
	require.Equal(t, "11", more[2], "the next page holds the 11 left")
	u, err := url.Parse(html.UnescapeString(more[1]))
	require.NoError(t, err)
	require.Equal(t, memoriesListPath, u.Path)
	require.Equal(t, url.Values{"fav": {"1"}, "kind": {"gotcha"}, "q": {"trap"}, "sort": {"name"}, "show": {"70"}}, u.Query())
	all := getHTMLBody(t, mux, u.String())
	rows := mlRows(all)
	require.Len(t, rows, 31, "every starred gotcha, A to Z")
	require.NotContains(t, all, `class="ml-more"`, "nothing is left to show")
	require.Contains(t, all, `31 memories &middot; A to Z`)
	var drawn []string
	for _, m := range regexp.MustCompile(`<span class="ml-name">(?:<span class="ml-star"[^>]*>.*?</span>)?([a-z0-9-]+)</span>`).FindAllStringSubmatch(all, -1) {
		drawn = append(drawn, m[1])
	}
	var starred []string
	for i, n := range names {
		if i%2 == 0 {
			starred = append(starred, n)
		}
	}
	require.Equal(t, starred, drawn, "the longer page is the same list, in the same order")

	// The row's link keeps the list, not its length; a new choice starts over.
	require.Equal(t, "/console/memories/"+rows[0]+qs, mlRowHref(t, all, rows[0]))
	main := libMain(t, all)
	for _, href := range regexp.MustCompile(`href="(/console/memories\?[^"]*)"`).FindAllStringSubmatch(main, -1) {
		require.NotContains(t, href[1], "show=", "%s: a filter, a sort or a project starts at the first rows", href[1])
	}

	// A project's list is drawn whole: no page, no Show more.
	whole := getHTMLBody(t, mux, "/console/memories?project=example-ops")
	require.Len(t, mlRows(whole), memoryPageSize+12)
	require.NotContains(t, whole, `class="ml-more"`)
	require.Contains(t, whole, `62 memories &middot; By kind, then newest first`)
}

// Each kind chip counts what choosing it would show, the other filters
// applied: the text, the project, Starred and Never surfaced, never its own
// kind. All kinds counts them all; a kind with nothing is left out unless it
// is the one chosen; the chosen chip is marked. Each keeps every other
// filter and the list's status.
func TestMlKindChipsCountWhatTheyWouldShow(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	base := time.Now().UTC().Add(-time.Hour)
	a := mlMemoryAt(t, mgr, core.KindGotcha, "pay", "pay-trap", base)
	mlMemoryAt(t, mgr, core.KindGotcha, "pay", "pay-trap-two", base.Add(time.Minute))
	rule := mlMemoryAt(t, mgr, core.KindConstraint, "pay", "pay-rule", base.Add(2*time.Minute))
	mlMemoryAt(t, mgr, core.KindDecision, "web", "web-choice", base.Add(3*time.Minute))
	mlMemoryAt(t, mgr, core.KindGotcha, "web", "web-trap", base.Add(4*time.Minute))
	gone := mlMemoryAt(t, mgr, core.KindRunbook, "pay", "pay-old-run", base)
	require.Equal(t, http.StatusSeeOther, postForm(mux, "/console/memories/"+gone.ID+"/archive", "").Code)
	libInject(t, db, "S1", "pay", base, a.ID)
	require.Equal(t, http.StatusSeeOther, postFavorite(mux, "memory", rule.ID, url.Values{"favorite": {"1"}}).Code)

	chips := mlChips(getHTMLBody(t, mux, "/console/memories"))
	require.Equal(t, map[string]mlChipRow{
		"All kinds":  {Href: "/console/memories", Count: "5", On: true},
		"constraint": {Href: "/console/memories?kind=constraint", Count: "1"},
		"gotcha":     {Href: "/console/memories?kind=gotcha", Count: "3"},
		"decision":   {Href: "/console/memories?kind=decision", Count: "1"},
	}, chips, "the active memories by kind; the archived runbook is not here")

	chips = mlChips(getHTMLBody(t, mux, "/console/memories?project=pay&kind=gotcha&surfaced=never"))
	require.Equal(t, map[string]mlChipRow{
		"All kinds":  {Href: "/console/memories?project=pay&surfaced=never", Count: "2"},
		"constraint": {Href: "/console/memories?kind=constraint&project=pay&surfaced=never", Count: "1"},
		"gotcha":     {Href: "/console/memories?kind=gotcha&project=pay&surfaced=never", Count: "1", On: true},
	}, chips, "in pay, never surfaced: the surfaced gotcha is not counted, and web is not here")

	chips = mlChips(getHTMLBody(t, mux, "/console/memories?kind=stage&q=trap"))
	require.Equal(t, mlChipRow{Href: "/console/memories?kind=stage&q=trap", Count: "0", On: true}, chips["stage"],
		"the chosen kind stays listed with nothing in it")
	require.Equal(t, "3", chips["gotcha"].Count)
	require.NotContains(t, chips, "constraint", "a kind with nothing to show is left out")

	chips = mlChips(getHTMLBody(t, mux, "/console/memories?status=inactive"))
	require.Equal(t, map[string]mlChipRow{
		"All kinds": {Href: "/console/memories?status=inactive", Count: "1", On: true},
		"runbook":   {Href: "/console/memories?kind=runbook&status=inactive", Count: "1"},
	}, chips, "the archived list's chips count the archived memories, and keep the status")
}

// "N archived and superseded" switches the list to the memories that were
// archived or replaced (?status=inactive): each row says which, and names
// what replaced it when this request reads it. Every filter, the switcher
// and the sort keep the status; the rows' links carry it, so the memory's
// page places it among them. The archived list is laid out as the active one.
func TestMlStatusListsArchivedAndSupersededWithSuccessors(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	_, err := store.EnsureProject(context.Background(), db, "example-ops", "Ops")
	require.NoError(t, err)
	old := writeMemory(t, mgr, core.KindDecision, "example-ops", "dns-ttl-300", "keep the TTL at 300")
	head := writeMemory(t, mgr, core.KindDecision, "example-ops", "dns-ttl-60", "lower the TTL to 60")
	lrSupersede(t, mgr, old, head)
	dropped := writeMemory(t, mgr, core.KindGotcha, "example-ops", "cron-drift", "cron drifts")
	require.Equal(t, http.StatusSeeOther, postForm(mux, "/console/memories/"+dropped.ID+"/archive", "").Code)
	writeMemory(t, mgr, core.KindGotcha, "example-web", "css-cache", "the css cache")

	active := getHTMLBody(t, mux, "/console/memories")
	require.ElementsMatch(t, []string{head.ID, mlMemoryID(t, db, "css-cache")}, mlRows(active), "the active list holds no retired memory")
	require.Contains(t, active, `<a class="lib-flag ml-status" href="/console/memories?status=inactive" title="Show the memories that were archived or replaced: they no longer reach agents">`)
	require.Contains(t, active, `2 archived and superseded</a>`)

	page := getHTMLBody(t, mux, "/console/memories?status=inactive")
	require.ElementsMatch(t, []string{old.ID, dropped.ID}, mlRows(page))
	require.Contains(t, page, `<a class="lib-flag ml-status on" href="/console/memories" title="Show the active memories again">`)
	require.Contains(t, page, `<strong>2</strong>archived &amp; superseded`)
	require.Contains(t, page, `<span class="ml-status">superseded by <span class="ml-successor">dns-ttl-60</span></span>`)
	require.Contains(t, page, `<span class="ml-status">archived</span>`)
	require.Contains(t, page, `class="ml-row ml-gone" id="ml-`+old.ID+`"`)
	require.Equal(t, "/console/memories/"+old.ID+"?status=inactive", mlRowHref(t, page, old.ID))
	main := libMain(t, page)
	require.Contains(t, main, `<input type="hidden" name="status" value="inactive">`, "typing keeps the status")
	require.Contains(t, main, `href="/console/memories?sort=name&amp;status=inactive"`, "so does a sort")
	require.Contains(t, main, `href="/console/memories?fav=1&amp;status=inactive"`, "and Starred")
	v := scParse(t, page)
	for _, r := range append(append([]scRow{v.All}, v.Active...), v.Quiet...) {
		require.Contains(t, r.Href, "status=inactive", "%s: choosing a project keeps the status", r.Value)
	}

	// A project's archived list, by kind, and a filter that matches nothing
	// says so in the archived words, its search reaching the archived too.
	byKind := getHTMLBody(t, mux, "/console/memories?status=inactive&project=example-ops")
	require.Equal(t, []string{"gotcha", "decision"}, mlGroupKeys(byKind), "by kind, in core's order")
	none := libMain(t, getHTMLBody(t, mux, "/console/memories?status=inactive&q=zebra"))
	require.Contains(t, none, "No archived or superseded memories match &ldquo;zebra&rdquo;.")
	require.Contains(t, none, `<a href="/console/search?q=zebra&amp;archived=1">`)
	require.Contains(t, none, `<a href="/console/memories?status=inactive" data-seam-query>`, "Clear keeps the status")

	// The memory's page places it among the archived ones, and keeps the
	// status on its neighbours and its way back.
	frame := itFrame(t, getHTMLBody(t, mux, "/console/memories/"+old.ID+"?status=inactive&sort=name"))
	pos, prev, next := itPlace(t, frame)
	require.Equal(t, "2 of 2 archived or superseded memories", pos)
	require.Equal(t, "/console/memories/"+dropped.ID+"?sort=name&status=inactive", prev)
	require.Empty(t, next)
	back := itBackRe.FindStringSubmatch(frame)
	require.Equal(t, "/console/memories?sort=name&status=inactive", html.UnescapeString(back[1]))
	require.Equal(t, "Archived and superseded memories", back[2])
}

// mlMemoryID is the id of the memory named name.
func mlMemoryID(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var id string
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT id FROM memories_index WHERE name = ?`, name).Scan(&id))
	return id
}

// mlGroupKeys is a page's group keys, in order.
func mlGroupKeys(body string) []string {
	var keys []string
	for _, g := range mlGroups(body) {
		keys = append(keys, g.Key)
	}
	return keys
}

// Choosing a project in the switcher shows all of that project's memories,
// by kind in core's order, each kind's group headed by its kind and its one
// line, each kind in the sort's order; the rows carry no project chip.
// Choosing keeps the other filters and drops the page size.
func TestMlProjectListGroupsByKindAndTheSwitcherKeepsFilters(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	base := time.Now().UTC().Add(-time.Hour)
	g1 := mlMemoryAt(t, mgr, core.KindGotcha, "example-pay", "pay-gotcha-old", base)
	g2 := mlMemoryAt(t, mgr, core.KindGotcha, "example-pay", "pay-gotcha-new", base.Add(time.Minute))
	c := mlMemoryAt(t, mgr, core.KindConstraint, "example-pay", "pay-rule", base.Add(2*time.Minute))
	r := mlMemoryAt(t, mgr, core.KindRefuted, "example-pay", "pay-myth", base.Add(3*time.Minute))
	mlMemoryAt(t, mgr, core.KindGotcha, "example-web", "web-gotcha", base.Add(4*time.Minute))
	shared := mlMemoryAt(t, mgr, core.KindReference, "", "shared-guide", base.Add(5*time.Minute))

	page := getHTMLBody(t, mux, "/console/memories?project=example-pay")
	require.Equal(t, []string{c.ID, g2.ID, g1.ID, r.ID}, mlRows(page), "constraints first, then gotchas newest first, refuted last")
	require.Equal(t, []mlGroup{
		{Key: "constraint", Label: "constraints", Gloss: kindGloss("constraint"), Note: "1"},
		{Key: "gotcha", Label: "gotchas", Gloss: kindGloss("gotcha"), Note: "2"},
		{Key: "refuted", Label: "refuted memories", Gloss: kindGloss("refuted"), Note: "1"},
	}, mlGroups(page))
	require.Contains(t, page, `<section class="ml-group" id="ml-g-gotcha" aria-labelledby="ml-gh-gotcha" data-memory-kind="gotcha">`)
	require.NotContains(t, page, `class="ml-proj"`, "one project: no project chip")
	require.Contains(t, page, `aria-label="Project: example-pay"`)
	require.Equal(t, "/console/memories/"+g2.ID+"?project=example-pay", mlRowHref(t, page, g2.ID))

	byName := getHTMLBody(t, mux, "/console/memories?project=example-pay&sort=name")
	require.Equal(t, []string{c.ID, g2.ID, g1.ID, r.ID}, mlRows(byName), "by name within each kind")
	global := getHTMLBody(t, mux, "/console/memories?project=global")
	require.Equal(t, []string{shared.ID}, mlRows(global))
	require.Contains(t, global, `aria-label="Project: Shared globally"`)

	// Across every project, the row names its project.
	all := getHTMLBody(t, mux, "/console/memories")
	require.Contains(t, all, `<span class="ml-proj">example-pay</span>`)
	require.Contains(t, all, `<span class="ml-proj">Shared globally</span>`)

	// The switcher keeps every other filter, the status included, and drops
	// the page size: a project's list is drawn whole.
	const qs = "fav=1&kind=gotcha&q=e&show=100&sort=reach&status=inactive&surfaced=never"
	v := scParse(t, getHTMLBody(t, mux, "/console/memories?"+qs))
	want, err := url.ParseQuery(qs)
	require.NoError(t, err)
	want.Del("show")
	rows := append(append([]scRow{v.All}, v.Active...), v.Quiet...)
	require.NotEmpty(t, rows)
	for _, row := range rows {
		u, err := url.Parse(row.Href)
		require.NoError(t, err)
		got := u.Query()
		require.Equal(t, row.Value, got.Get("project"), row.Href)
		got.Del("project")
		require.Equal(t, want, got, "%s keeps every other filter", row.Href)
	}
}

// A row is one link to the memory's own page, carrying the list's state: on
// the default list its sort, so the page knows it came from this list and
// places the memory in it, every project's, newest first; its k / j walk
// this list. A link from anywhere else carries nothing and keeps the page's
// own place, the memory's project and kind.
func TestMlRowsOpenTheirPageInThisList(t *testing.T) {
	_, mgr, _, mux := itFixture(t, "advanced")
	base := time.Now().UTC().Add(-time.Hour)
	older := mlMemoryAt(t, mgr, core.KindGotcha, "example-pay", "pay-older", base)
	mid := mlMemoryAt(t, mgr, core.KindDecision, "example-web", "web-middle", base.Add(time.Minute))
	newer := mlMemoryAt(t, mgr, core.KindGotcha, "example-pay", "pay-newer", base.Add(2*time.Minute))

	list := getHTMLBody(t, mux, "/console/memories")
	require.Equal(t, []string{newer.ID, mid.ID, older.ID}, mlRows(list))
	href := mlRowHref(t, list, mid.ID)
	require.Equal(t, "/console/memories/"+mid.ID+"?sort=recent", href)
	require.Regexp(t, `<a class="ml-row" id="ml-`+mid.ID+`" href="[^"]+" data-peek data-memory-kind="decision">`, list,
		"the row reads in place where the console does, by its path (data-peek is a hint)")

	pos, prev, next := itPlace(t, itFrame(t, getHTMLBody(t, mux, href)))
	require.Equal(t, "2 of 3 memories", pos)
	require.Equal(t, "/console/memories/"+newer.ID+"?sort=recent", prev)
	require.Equal(t, "/console/memories/"+older.ID+"?sort=recent", next)

	pos, _, next = itPlace(t, itFrame(t, getHTMLBody(t, mux, "/console/memories/"+newer.ID)))
	require.Equal(t, "1 of 2 gotchas in example-pay", pos, "no list on the link: its project and kind")
	require.Equal(t, "/console/memories/"+older.ID, next)
}

// The list opens nothing by itself: no reader, no rail, no auto-opened
// memory pinned into the URL, no Group control and no Kind menu.
func TestMlListOpensNothingByItself(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	writeMemory(t, mgr, core.KindConstraint, "", "solo-mem", "only memory")
	page := getHTMLBody(t, mux, "/console/memories?group=project")
	main := libMain(t, page)
	for _, gone := range []string{`id="lib-reader"`, `class="lib-layout"`, `class="lib-rail"`, "data-auto-url", "body of solo-mem",
		`class="lib-group"`, `aria-label="Kind:`, "data-lib-here", "reader-sheet"} {
		require.NotContains(t, main, gone)
	}
	require.Len(t, mlRows(page), 1)
	require.Contains(t, main, `<form class="search lib-query" method="get" action="/console/memories" data-seam-query>`,
		"the filter is the one / focuses (search.js pageFilter)")
	require.Contains(t, main, `<input type="hidden" name="group" value="project">`, "an old grouping on the URL is kept, and changes nothing")

	// No memory at all: the page says where they come from.
	_, _, empty := newConsoleWithFiles(t)
	require.Contains(t, getHTMLBody(t, empty, "/console/memories"), "No active memories yet")
}

// The list's own parameters are as strict as the library's: a value the page
// cannot read is a 400 naming what it takes, on the list and on a memory's
// page alike.
func TestMlBadStatusOrShowIs400(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	m := writeMemory(t, mgr, core.KindGotcha, "pay", "pay-trap", "a trap")
	for query, want := range map[string]string{
		"status=active":   `invalid status "active": valid values are inactive (or omit)`,
		"status=archived": `invalid status "archived": valid values are inactive (or omit)`,
		"show=0":          `invalid show "0": a number of rows, 1 or more`,
		"show=ten":        `invalid show "ten": a number of rows, 1 or more`,
		"show=-50":        `invalid show "-50": a number of rows, 1 or more`,
	} {
		for _, path := range []string{"/console/memories?", "/console/memories/" + m.ID + "?"} {
			rr := getPeek(t, mux, path+query)
			require.Equal(t, http.StatusBadRequest, rr.Code, path+query)
			require.Contains(t, html.UnescapeString(rr.Body.String()), want, path+query)
		}
		rr := getPeek(t, mux, "/console/memories?format=json&"+query)
		require.Equal(t, http.StatusBadRequest, rr.Code, query)
		var answer map[string]string
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &answer))
		require.Equal(t, want, answer["error"], "the JSON answer is as strict")
	}
}

// The JSON answer keeps its shape and content (seam reads no Memories JSON,
// but tests and scripts do): every match, unpaged, in its project and kind
// groups, with the archived and superseded ones beside them, the counts and
// the filters as the URL gave them. The page's own parameters, the status
// and the page size, change nothing in it. This test passes unchanged on the
// tree before UX 28.
func TestMlJSONKeepsItsShape(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for i := range 55 {
		libMemory(t, mgr, core.KindGotcha, "example-ops", fmt.Sprintf("ops-%02d", i), base.Add(time.Duration(i)*time.Minute))
	}
	rule := libMemory(t, mgr, core.KindConstraint, "example-ops", "ops-rule", base)
	libMemory(t, mgr, core.KindReference, "", "shared-guide", base.Add(time.Hour))
	gone := libMemory(t, mgr, core.KindDecision, "example-web", "web-old", base)
	require.Equal(t, http.StatusSeeOther, postForm(mux, "/console/memories/"+gone.ID+"/archive", "").Code)

	raw := func(path string) string {
		t.Helper()
		rr := getPeek(t, mux, path)
		require.Equal(t, http.StatusOK, rr.Code, path)
		return rr.Body.String()
	}
	plain := raw("/console/memories?format=json")
	for _, same := range []string{"/console/memories?format=json&show=10", "/console/memories?format=json&status=inactive",
		"/console/memories?format=json&status=inactive&show=200"} {
		require.Equal(t, plain, raw(same), "%s: the page's own parameters change nothing in the JSON", same)
	}

	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(plain), &top))
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	require.Equal(t, []string{"activeCount", "groups", "inactive", "inactiveCount", "sort", "total"}, keys)

	var d struct {
		Groups []struct {
			Project string `json:"project"`
			Count   int    `json:"count"`
			Kinds   []struct {
				Kind     string           `json:"kind"`
				Memories []map[string]any `json:"memories"`
			} `json:"kinds"`
		} `json:"groups"`
		Inactive      []map[string]any `json:"inactive"`
		ActiveCount   int              `json:"activeCount"`
		InactiveCount int              `json:"inactiveCount"`
		Total         int              `json:"total"`
		Sort          string           `json:"sort"`
	}
	require.NoError(t, json.Unmarshal([]byte(plain), &d))
	require.Equal(t, 57, d.ActiveCount, "every match, unpaged")
	require.Equal(t, 57, d.Total)
	require.Equal(t, 1, d.InactiveCount)
	require.Equal(t, "recent", d.Sort)
	require.Len(t, d.Groups, 2)
	require.Equal(t, "", d.Groups[0].Project, "the global scope first")
	require.Equal(t, "example-ops", d.Groups[1].Project)
	require.Equal(t, 56, d.Groups[1].Count)
	require.Equal(t, "constraint", d.Groups[1].Kinds[0].Kind, "kinds in core's order")
	require.Equal(t, rule.ID, d.Groups[1].Kinds[0].Memories[0]["id"])
	gotchas := d.Groups[1].Kinds[1].Memories
	require.Len(t, gotchas, 55)
	require.Equal(t, "ops-54", gotchas[0]["name"], "newest first within a kind")
	require.Equal(t, "ops-00", gotchas[54]["name"])
	rowKeys := make([]string, 0, len(gotchas[0]))
	for k := range gotchas[0] {
		rowKeys = append(rowKeys, k)
	}
	slices.Sort(rowKeys)
	require.Equal(t, []string{"absPath", "description", "filePath", "id", "injects", "kind", "name", "project", "reads", "status", "updated"}, rowKeys)
	require.Len(t, d.Inactive, 1)
	require.Equal(t, "web-old", d.Inactive[0]["name"])
	require.Equal(t, "archived", d.Inactive[0]["status"])

	var filtered map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw("/console/memories?format=json&kind=gotcha&project=example-ops&q=ops&fav=1&surfaced=never&group=project&sort=name")), &filtered))
	for k, want := range map[string]any{"kind": "gotcha", "project": "example-ops", "query": "ops", "fav": true,
		"surfaced": "never", "group": "project", "sort": "name"} {
		require.Equal(t, want, filtered[k], k)
	}
}

// A locked project's memories are absent from every part of the list: its
// rows, the kind chips' and the switcher's counts, the archived list and
// its toggle, a page past the first. Without the lock the same pages name
// them.
func TestMlLockedProjectIsAbsent(t *testing.T) {
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
	locked, open := console(view), console(db)

	now := time.Now().UTC()
	mlMemoryAt(t, mgr, core.KindGotcha, "example-open", "open-trap", now.Add(-time.Hour))
	mlMemoryAt(t, mgr, core.KindGotcha, "example-locked", "secret-trap", now.Add(-time.Minute))
	mlMemoryAt(t, mgr, core.KindStage, "example-locked", "secret-stage", now.Add(-2*time.Minute))
	gone := mlMemoryAt(t, mgr, core.KindGotcha, "example-locked", "secret-gone", now.Add(-3*time.Minute))
	require.Equal(t, http.StatusSeeOther, postForm(open, "/console/memories/"+gone.ID+"/archive", "").Code)

	for _, p := range []string{"/console/memories", "/console/memories?status=inactive", "/console/memories?q=secret",
		"/console/memories?project=example-locked", "/console/memories?kind=stage", "/console/memories?show=1&sort=name"} {
		body := getHTMLBody(t, locked, p)
		for _, name := range []string{"secret-trap", "secret-stage", "secret-gone"} {
			require.NotContains(t, body, name, p)
		}
		require.Empty(t, mlRows(body)[min(len(mlRows(body)), 1):], "%s: one open row at most", p)
		if !strings.Contains(p, "example-locked") {
			require.NotContains(t, body, "example-locked", "%s names the locked project", p)
		}
		if !strings.Contains(p, "kind=stage") {
			require.NotContains(t, body, `data-kind="stage"`, "%s: no chip counts the locked stage", p)
		}
	}
	page := getHTMLBody(t, locked, "/console/memories")
	require.Equal(t, "1", mlChips(page)["All kinds"].Count)
	require.NotContains(t, page, "archived and superseded</a>", "nothing archived this request reads: no toggle")
	require.Equal(t, 1, scParse(t, page).All.Count)

	openPage := getHTMLBody(t, open, "/console/memories")
	require.Contains(t, openPage, "secret-trap", "without the lock the list names it: the absence is the view's")
	require.Equal(t, "3", mlChips(openPage)["All kinds"].Count)
	require.Contains(t, openPage, "1 archived and superseded</a>")
	require.Contains(t, getHTMLBody(t, open, "/console/memories?status=inactive"), "secret-gone")
}

// The rows survive a live refresh: each row and group is keyed by its id, so
// the morph keeps the rows it has (and the focus ring on one), adds a new
// memory to its day group, and nothing in the list is skipped by the morph.
func TestMlRowsAreKeyedForTheLiveMorph(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	now := time.Now().UTC()
	a := mlMemoryAt(t, mgr, core.KindGotcha, "example-ops", "ops-a", now.Add(-time.Minute))
	b := mlMemoryAt(t, mgr, core.KindDecision, "example-ops", "ops-b", now.Add(-72*time.Hour))
	page := getHTMLBody(t, mux, "/console/memories")
	list := page[strings.Index(page, `<section class="ml-list"`):]
	list = list[:strings.Index(list, `<p class="ml-keys"`)]
	require.NotContains(t, list, "data-live-skip", "the list is the morph's to patch")
	for _, id := range []string{a.ID, b.ID} {
		require.Equal(t, 1, strings.Count(page, `id="ml-`+id+`"`))
	}
	require.Equal(t, len(mlGroups(page)), strings.Count(list, `<section class="ml-group" id="ml-g-`), "every group has its id")

	c := mlMemoryAt(t, mgr, core.KindGotcha, "example-ops", "ops-c", now)
	after := getHTMLBody(t, mux, "/console/memories")
	require.Equal(t, []string{c.ID, a.ID, b.ID}, mlRows(after), "a memory written elsewhere joins its day group, newest first")
}

// The list's keys (library.js): j and k move a focus ring through the rows,
// Enter opens the focused row's own page through SeamPanel.leave when the
// panel is there (asked at the moment of the key), and they are bubble
// listeners that leave a key the panel or a menu already took. / and p stay
// search.js's and scope.js's: the list adds no key of its own for them.
func TestMlListKeys(t *testing.T) {
	js := string(libraryJS)
	for _, want := range []string{
		"document.querySelectorAll('.main [data-ml-list] a.ml-row')",
		"var panel = window.SeamPanel;\n    if (panel && panel.leave) panel.leave(href);\n    else location.assign(href);",
		"if (e.metaKey || e.ctrlKey || e.altKey || e.defaultPrevented) return;",
		"if (e.key === 'Enter') {\n      openListRow(e);",
		"if (stepList(e)) return;",
		"row.focus({ preventScroll: true });\n    row.scrollIntoView({ block: 'nearest' });",
		"e.detail.source !== 'live'",
	} {
		require.Contains(t, js, want)
	}
	require.NotContains(t, js, "addEventListener('keydown', function (e) {\n    if (e.key === '/'", "no / of its own")
	require.Equal(t, 1, strings.Count(js, "addEventListener('keydown'"), "one key listener, in the bubble phase")
	require.NotContains(t, js, "addEventListener('keydown', function (e) {}, true)")

	src, err := templateFS.ReadFile("templates/memories.html")
	require.NoError(t, err)
	page := string(src)
	require.Contains(t, page, `{{template "project-switcher" .Scope}}`, "p opens the switcher (scope.js)")
	require.Contains(t, page, `data-ml-list`)
	require.Contains(t, string(searchJS), `.main .lib-query input[name="q"]`, "/ focuses the list's filter")
}
