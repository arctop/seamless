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

// The Notes list (plan:console-ux-pass UX 29): one full-width list, no rail
// and no reader, the Memories list's shape. A page of the matches, newest
// first in local-day groups, across every project or in the one chosen; tag
// chips filter it; the agent working copies fold away until asked for.

// nlRowRe finds the Notes list's rows, in order, by note id, with their links.
var nlRowRe = regexp.MustCompile(`<a class="ml-row nl-row[^"]*" id="ml-([0-9A-Z]+)" href="([^"]+)"`)

// nlRows is a page's row ids in the order it draws them.
func nlRows(body string) []string {
	var ids []string
	for _, m := range nlRowRe.FindAllStringSubmatch(body, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// nlRow is the markup of the row for note id, from its anchor to its end.
func nlRow(t *testing.T, body, id string) string {
	t.Helper()
	start := strings.Index(body, `id="ml-`+id+`"`)
	require.NotEqual(t, -1, start, "no row for %s", id)
	end := strings.Index(body[start:], "</a>\n")
	require.NotEqual(t, -1, end)
	return body[start : start+end]
}

// nlChips is the tag chips' nav of a page.
func nlChips(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<nav class="ml-kinds nl-chips"`)
	require.NotEqual(t, -1, start, "no tag chips")
	end := strings.Index(body[start:], "</nav>")
	require.NotEqual(t, -1, end)
	return body[start : start+end]
}

// nlHasChip checks that chips hold a chip linking to href, labelled label,
// counting n.
func nlHasChip(t *testing.T, chips, href, label string, n int) {
	t.Helper()
	re := regexp.MustCompile(`<a class="ml-chip[^"]*" href="` + regexp.QuoteMeta(html.EscapeString(href)) + `"[^>]*>` +
		regexp.QuoteMeta(label) + fmt.Sprintf(`<span class="n">%d</span>`, n))
	require.Regexp(t, re, chips, "chip %s %d at %s", label, n, href)
}

// nlChipAt is where the chip labelled label sits in chips, -1 when absent.
func nlChipAt(chips, label string) int {
	return strings.Index(chips, ">"+label+`<span class="n">`)
}

// The chips are the tags written for people, most notes first, each counting
// what choosing it would show: the provenance stamp and a captured plan's
// lifecycle and marker tags never become chips, and plan:<slug> is the one
// "plan narratives" chip.
func TestNlTagChipsLeaveMachineTagsOut(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	at := time.Now().UTC().Add(-time.Hour)
	libNote(t, mgr, "example-api", "cc-plan-limits", "API limits", at,
		[]string{"cc-plan", "plan:api-limits", "plan-status:presented", "created-by:agent", "research"})
	libNote(t, mgr, "example-api", "survey", "Survey", at, []string{"research", "meeting", "created-by:gardener"})
	libNote(t, mgr, "example-api", "retro", "Retro", at, []string{"retro"})

	chips := nlChips(t, getHTMLBody(t, mux, "/console/notes"))
	for _, machine := range []string{"cc-plan", "plan-status", "created-by", "plan:api-limits", "agent-cache"} {
		require.NotContains(t, chips, machine, "a tag software stamps is no chip")
	}
	nlHasChip(t, chips, "/console/notes", "Everything", 3)
	nlHasChip(t, chips, "/console/notes?plans=1", "plan narratives", 1)
	nlHasChip(t, chips, "/console/notes?tag=research", "research", 2)
	nlHasChip(t, chips, "/console/notes?tag=meeting", "meeting", 1)
	nlHasChip(t, chips, "/console/notes?tag=retro", "retro", 1)
	research, meeting, retro := nlChipAt(chips, "research"), nlChipAt(chips, "meeting"), nlChipAt(chips, "retro")
	require.True(t, research > 0 && research < meeting && meeting < retro, "most notes first, then A to Z:\n%s", chips)
	require.Contains(t, chips, `<a class="ml-chip on" href="/console/notes" aria-current="true">Everything`)

	// A chip is a filter: on, counted in the header, cleared by Clear, and
	// the others keep counting what choosing them would show.
	page := getHTMLBody(t, mux, "/console/notes?tag=research")
	require.Len(t, nlRows(page), 2)
	require.Contains(t, page, "<strong>2</strong>of 3 notes")
	require.Contains(t, page, `class="lib-clear" href="/console/notes"`)
	chips = nlChips(t, page)
	require.Contains(t, chips, `<a class="ml-chip on" href="/console/notes?tag=research" aria-current="true">research`)
	nlHasChip(t, chips, "/console/notes?tag=retro", "retro", 1)

	// A tag the chips do not offer can still be asked for; it stays listed.
	asked := nlChips(t, getHTMLBody(t, mux, "/console/notes?tag=created-by:gardener"))
	require.Contains(t, asked, `<a class="ml-chip on" href="/console/notes?tag=created-by%3Agardener" aria-current="true">created-by:gardener<span class="n">1</span>`)

	// Past noteChipLimit tags, the ones most notes carry stay.
	for i := range noteChipLimit + 2 {
		tags := []string{fmt.Sprintf("tag-%02d", i)}
		if i < noteChipLimit {
			tags = append(tags, "common")
		}
		libNote(t, mgr, "", fmt.Sprintf("many-%d", i), fmt.Sprintf("Many %d", i), at, tags)
	}
	chips = nlChips(t, getHTMLBody(t, mux, "/console/notes"))
	require.Equal(t, noteChipLimit+2, strings.Count(chips, `class="ml-chip`), "Everything, plan narratives and %d tags", noteChipLimit)
	nlHasChip(t, chips, "/console/notes?tag=common", "common", noteChipLimit)
}

// "plan narratives" counts and lists the notes tagged plan:<slug>, a
// working copy only once the list shows the working copies.
func TestNlPlanNarrativesCountsPlanNotes(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	at := time.Now().UTC().Add(-time.Hour)
	a := libNote(t, mgr, "example-ops", "plan-dns", "Plan: DNS cutover", at, []string{"plan:dns-cutover", "created-by:agent"})
	b := libNote(t, mgr, "example-web", "web-support", "Web support", at.Add(time.Minute), []string{"plan:web-v1", "research"})
	libNote(t, mgr, "example-ops", "cc-agent-1", "Planning subagent", at.Add(2*time.Minute), []string{"agent-cache", "plan:dns-cutover"})
	libNote(t, mgr, "example-ops", "loose", "Loose note", at, []string{"plan:", "research"})

	chips := nlChips(t, getHTMLBody(t, mux, "/console/notes"))
	nlHasChip(t, chips, "/console/notes?plans=1", "plan narratives", 2)
	page := getHTMLBody(t, mux, "/console/notes?plans=1")
	require.Equal(t, []string{b.ID, a.ID}, nlRows(page))
	require.Contains(t, nlChips(t, page), `<a class="ml-chip on" href="/console/notes?plans=1" title=`)

	shown := getHTMLBody(t, mux, "/console/notes?caches=1&plans=1")
	require.Len(t, nlRows(shown), 3)
	nlHasChip(t, nlChips(t, shown), "/console/notes?caches=1&plans=1", "plan narratives", 3)

	var d notesData
	getJSON(t, mux, "/console/notes?format=json&plans=1", &d)
	require.Equal(t, 3, d.Count, "the JSON answer keeps the working copies")
	require.True(t, d.Plans)
}

// Agent working copies (agent-cache) are folded by default: out of the list,
// its counts and its chips, and counted on one line that shows them; with
// ?caches=1 they are listed, marked, and the line folds them again. Every
// control keeps the choice.
func TestNlWorkingCopiesFoldedByDefault(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	at := time.Now().UTC().Add(-time.Hour)
	plan := libNote(t, mgr, "example-ops", "plan-dns", "Plan: DNS cutover", at, []string{"plan:dns-cutover", "research"})
	c1 := libNote(t, mgr, "example-ops", "cc-agent-1", "Planning subagent one", at.Add(time.Minute), []string{"agent-cache", "plan:dns-cutover", "agent:scout"})
	c2 := libNote(t, mgr, "example-web", "cc-agent-2", "Planning subagent two", at.Add(2*time.Minute), []string{"agent-cache"})

	page := getHTMLBody(t, mux, "/console/notes")
	require.Equal(t, []string{plan.ID}, nlRows(page))
	require.Contains(t, page, "<strong>1</strong>note\n")
	chips := nlChips(t, page)
	require.Contains(t, chips, `<span class="nl-fold" data-nl-fold>2 agent working copies folded &middot; <a href="/console/notes?caches=1"`)
	require.NotContains(t, chips, "agent:scout", "a folded copy's tags are no chips")
	nlHasChip(t, chips, "/console/notes", "Everything", 1)

	shown := getHTMLBody(t, mux, "/console/notes?caches=1")
	require.Equal(t, []string{c2.ID, c1.ID, plan.ID}, nlRows(shown))
	require.Contains(t, nlRow(t, shown, c1.ID), `<span class="nl-copy"`)
	require.NotContains(t, nlRow(t, shown, plan.ID), `<span class="nl-copy"`)
	require.Contains(t, nlChips(t, shown), `2 agent working copies shown &middot; <a href="/console/notes"`)
	nlHasChip(t, nlChips(t, shown), "/console/notes?caches=1&tag=agent%3Ascout", "agent:scout", 1)
	main := libMain(t, shown)
	require.Contains(t, main, `<input type="hidden" name="caches" value="1">`, "typing keeps them shown")
	require.Contains(t, main, `href="/console/notes?caches=1&amp;sort=name" title="A to Z">Name</a>`, "so does a sort")
	require.Contains(t, main, `href="/console/notes?caches=1&amp;fav=1"`, "and Starred")
	require.Contains(t, main, `id="ml-`+c1.ID+`" href="/console/notes/`+c1.ID+`?caches=1" data-peek>`, "and a row's page")

	// Filtered to one project, the line counts that project's copies.
	ops := nlChips(t, getHTMLBody(t, mux, "/console/notes?project=example-ops"))
	require.Contains(t, ops, "1 agent working copy folded")

	// A copy's own page places it among its project's notes with the copies
	// shown; its project's other notes are counted without them.
	frame := getHTMLBody(t, mux, "/console/notes/"+c1.ID)
	require.Contains(t, frame, "1 of 2 notes in example-ops")
	planPage := getHTMLBody(t, mux, "/console/notes/"+plan.ID)
	require.Contains(t, planPage, "1 of 1 note in example-ops")
	require.NotContains(t, planPage, "/console/notes/"+c1.ID, "its More card leaves the copy out")

	// Nothing but copies: the list says so, and shows them on request.
	_, mgr2, mux2 := newConsoleWithFiles(t)
	libNote(t, mgr2, "", "cc-agent-3", "Only a copy", at, []string{"agent-cache"})
	only := libMain(t, getHTMLBody(t, mux2, "/console/notes"))
	require.Contains(t, only, "Every note here is an agent working copy, folded away.")
	require.Contains(t, only, `<a href="/console/notes?caches=1" data-seam-query>Show them</a>`)
}

// A row: the title, the description, each plan chip with the plan's status,
// up to two tags written for people and how many more, the word count and
// the age; the project chip across every project only. It links to the
// note's own page with the list's query and opens in place (data-peek).
func TestNlRowShowsPlanStatusTagsWordsAndAge(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	now := time.Now().UTC()
	capture := libNote(t, mgr, "example-api", "cc-plan-limits", "API rate limiting", now,
		[]string{"cc-plan", "plan:api-rate-limiting", "plan-status:presented", "created-by:agent"})
	id, err := core.NewID()
	require.NoError(t, err)
	long, err := mgr.WriteNote(ctx, core.Note{ID: id, Slug: "pm", Title: "Webhook postmortem", Description: "The timeline.",
		Project: "example-api", Tags: []string{"plan:search-v2", "postmortem", "incident", "ops", "created-by:agent"},
		Body: strings.Repeat("word ", 1180), Created: now.Add(-time.Hour), Updated: now.Add(-time.Hour)})
	require.NoError(t, err)
	for _, task := range []core.Task{{ID: "T1", Title: "index", Status: core.TaskInProgress}, {ID: "T2", Title: "schema", Status: core.TaskDone}} {
		task.ProjectSlug, task.PlanSlug, task.CreatedAt, task.UpdatedAt = "example-api", "search-v2", now, now
		require.NoError(t, store.CreateTask(ctx, db, task))
	}

	page := getHTMLBody(t, mux, "/console/notes")
	row := nlRow(t, page, long.ID)
	require.Contains(t, row, `href="/console/notes/`+long.ID+`?sort=recent" data-peek>`, "the default list names itself, so the page knows its place")
	require.Contains(t, row, `<span class="nl-title">Webhook postmortem</span><span class="ml-desc">The timeline.</span>`)
	require.Contains(t, row, `<span class="lib-plan-chip" title="Webhook postmortem: 1 of 2 steps done">`)
	require.Contains(t, row, `<span class="mono">plan:search-v2</span> <span class="badge accent">in progress</span></span>`)
	require.Contains(t, row, `<span class="badge">postmortem</span>`)
	require.Contains(t, row, `<span class="badge">incident</span>`)
	require.NotContains(t, row, `<span class="badge">ops</span>`, "two tags, then a count")
	require.Contains(t, row, `>+1</span>`)
	require.NotContains(t, row, "created-by")
	require.Contains(t, row, `<span class="ml-proj">example-api</span>`)
	require.Contains(t, row, `<span class="nl-when">1,180 words &middot; <time`)
	require.NotContains(t, row, "<a ", "a row is one link: its chips are no links of their own")
	capRow := nlRow(t, page, capture.ID)
	require.Contains(t, capRow, `<span class="mono">plan:api-rate-limiting</span> <span class="badge accent">presented</span>`)
	require.Contains(t, capRow, `<span class="nl-when">5 words &middot; `)

	one := getHTMLBody(t, mux, "/console/notes?project=example-api")
	require.NotContains(t, nlRow(t, one, long.ID), "ml-proj", "one project's rows name no project")
	require.Contains(t, one, `<a class="ml-row nl-row" id="ml-`+capture.ID+`" href="/console/notes/`+capture.ID+`?project=example-api" data-peek>`)
}

// Recent groups the rows by local day, every group counting all its rows;
// 50 rows to a page, and Show more adds the next ones, the last time naming
// how many are left. The other sorts are one group in their order.
func TestNlDayGroupsAndPages(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	now := time.Now()
	today := dyMidnight(now).Add(time.Minute)
	libNote(t, mgr, "example-ops", "fresh", "Fresh", today.UTC(), nil)
	for i := range notePageSize + 10 {
		libNote(t, mgr, "example-ops", fmt.Sprintf("old-%02d", i), fmt.Sprintf("Old %02d", i), now.AddDate(0, -3, 0).Add(time.Duration(i)*time.Minute).UTC(), nil)
	}
	page := libMain(t, getHTMLBody(t, mux, "/console/notes"))
	require.Len(t, nlRows(page), notePageSize)
	require.Contains(t, page, `<h2 id="ml-gh-today">Today</h2><span class="ml-note">1 changed</span>`)
	require.Contains(t, page, `<h2 id="ml-gh-older">Older</h2><span class="ml-note">60 changed</span>`, "a group counts its rows past the page")
	require.Contains(t, page, "Showing <strong>50</strong> of 61 &middot; Newest first")
	require.Contains(t, page, `<a class="ml-more" href="/console/notes?show=100" data-seam-query>Show the other 11</a>`)
	all := libMain(t, getHTMLBody(t, mux, "/console/notes?show=100"))
	require.Len(t, nlRows(all), notePageSize+11)
	require.NotContains(t, all, `class="ml-more"`)
	require.Contains(t, all, "61 notes &middot; Newest first")

	for i := range notePageSize {
		libNote(t, mgr, "", fmt.Sprintf("more-%02d", i), fmt.Sprintf("More %02d", i), now.AddDate(0, -4, 0).UTC(), nil)
	}
	require.Contains(t, getHTMLBody(t, mux, "/console/notes"), `href="/console/notes?show=100" data-seam-query>Show 50 more</a>`)
	byName := libMain(t, getHTMLBody(t, mux, "/console/notes?sort=name&project=example-ops&show=100"))
	require.Contains(t, byName, `<h2 id="ml-gh-all">A to Z</h2><span class="ml-note">61</span>`)
	require.Equal(t, "Fresh", strings.TrimSpace(html.UnescapeString(regexp.MustCompile(`<span class="nl-title">([^<]+)</span>`).FindAllStringSubmatch(byName, -1)[0][1])))
	require.NotContains(t, byName, "ml-proj")
}

// A note's own page, opened from a row, is placed in that list: the chip,
// the fold and the filters all carried by its neighbours and its way back.
func TestNlItemPageFollowsTheChip(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	at := time.Now().UTC().Add(-time.Hour)
	a := libNote(t, mgr, "example-ops", "a", "Alpha", at, []string{"research"})
	libNote(t, mgr, "example-ops", "b", "Between", at.Add(time.Minute), []string{"meeting"})
	c := libNote(t, mgr, "example-web", "c", "Gamma", at.Add(2*time.Minute), []string{"research"})

	list := getHTMLBody(t, mux, "/console/notes?tag=research")
	require.Equal(t, []string{c.ID, a.ID}, nlRows(list))
	href := regexp.MustCompile(`href="([^"]+)"`).FindStringSubmatch(nlRow(t, list, c.ID))
	require.NotNil(t, href)
	require.Equal(t, "/console/notes/"+c.ID+"?tag=research", html.UnescapeString(href[1]))
	frame := itFrame(t, getHTMLBody(t, mux, html.UnescapeString(href[1])))
	pos, prev, next := itPlace(t, frame)
	require.Equal(t, "1 of 2 notes tagged research", pos)
	require.Empty(t, prev)
	require.Equal(t, "/console/notes/"+a.ID+"?tag=research", next)
	require.Contains(t, frame, `href="/console/notes?tag=research"`, "the way back is the chip's list")

	plansPage := itFrame(t, getHTMLBody(t, mux, "/console/notes/"+a.ID+"?plans=1"))
	require.Contains(t, plansPage, "2 of 2 notes in example-ops", "a list that does not hold it falls back to its project's")
}

// The list's own values are strict, as the library's are: a value it cannot
// read is a 400 naming what it takes, on the list, the note's page and its
// reader fragment.
func TestNlBadViewValuesAre400(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	n := libNote(t, mgr, "example-ops", "a", "Alpha", time.Now().UTC(), nil)
	for query, want := range map[string]string{
		"caches=yes": `invalid caches "yes": valid values are 1 (or omit)`,
		"plans=0":    `invalid plans "0": valid values are 1 (or omit)`,
		"show=0":     `invalid show "0": a number of rows, 1 or more`,
		"show=many":  `invalid show "many"`,
	} {
		for _, path := range []string{"/console/notes?", "/console/notes/" + n.ID + "?", "/console/notes/" + n.ID + "?reader=1&"} {
			rr := getPeek(t, mux, path+query)
			require.Equal(t, http.StatusBadRequest, rr.Code, path+query)
			require.Contains(t, html.UnescapeString(rr.Body.String()), want, path+query)
		}
	}
	require.Equal(t, http.StatusOK, getPeek(t, mux, "/console/notes/"+n.ID+"?peek=1&caches=1").Code, "the panel's fragment reads no list")
}

// The JSON answer is the one it was before the list (plan:console-ux-pass
// UX 29): every match of the filters, unpaged, in project groups, the
// working copies included, field for field. Both bodies below are the base's
// (676bca29) answers for this fixture.
func TestNlJSONKeepsItsShape(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	at := func(d int) time.Time { return time.Date(2026, 11, d, 10, 0, 0, 0, time.UTC) }
	for _, n := range []core.Note{
		{ID: "01M8NL00000000000000000001", Slug: "ops-runbook", Title: "Ops runbook", Description: "How to page", Project: "example-ops", Tags: []string{"research", "ops"}, Favorite: true, Created: at(1), Updated: at(1)},
		{ID: "01M8NL00000000000000000002", Slug: "cc-agent-1", Title: "Planning subagent", Project: "example-ops", Tags: []string{"agent-cache", "plan:ops-v2", "created-by:agent"}, Created: at(2), Updated: at(2)},
		{ID: "01M8NL00000000000000000003", Slug: "inbox", Title: "Inbox", Model: "claude-x", Created: at(3), Updated: at(3)},
		{ID: "01M8NL00000000000000000004", Slug: "web-plan", Title: "Web plan", Project: "example-web", Tags: []string{"plan:web-v1", "created-by:agent"}, Created: at(4), Updated: at(4)},
	} {
		n.Body = "body of " + n.Title
		_, err := mgr.WriteNote(context.Background(), n)
		require.NoError(t, err)
	}
	for path, want := range map[string]string{
		"/console/notes?format=json": nlBaseJSONAll,
		"/console/notes?format=json&project=example-ops&sort=name&q=o&fav=1&group=project": nlBaseJSONFiltered,
	} {
		rr := getPeek(t, mux, path)
		require.Equal(t, http.StatusOK, rr.Code)
		require.Equal(t, want, rr.Body.String(), path)
	}
	// The chips' filters are new fields, absent at their defaults.
	var d map[string]any
	getJSON(t, mux, "/console/notes?format=json&tag=research&plans=1&caches=1&show=1", &d)
	require.Equal(t, "research", d["tag"])
	require.Equal(t, true, d["plans"])
	require.EqualValues(t, 0, d["count"], "research and in a plan: none")
	getJSON(t, mux, "/console/notes?format=json&tag=research", &d)
	require.EqualValues(t, 1, d["count"])
}

const nlBaseJSONAll = `{
  "groups": [
    {
      "project": "",
      "count": 1,
      "notes": [
        {
          "id": "01M8NL00000000000000000003",
          "title": "Inbox",
          "description": "",
          "project": "",
          "model": "claude-x",
          "updated": "2026-11-03T10:00:00Z"
        }
      ]
    },
    {
      "project": "example-ops",
      "count": 2,
      "notes": [
        {
          "id": "01M8NL00000000000000000002",
          "title": "Planning subagent",
          "description": "",
          "project": "example-ops",
          "tags": [
            "agent-cache",
            "plan:ops-v2",
            "created-by:agent"
          ],
          "updated": "2026-11-02T10:00:00Z"
        },
        {
          "id": "01M8NL00000000000000000001",
          "title": "Ops runbook",
          "description": "How to page",
          "project": "example-ops",
          "tags": [
            "research",
            "ops"
          ],
          "favorite": true,
          "updated": "2026-11-01T10:00:00Z"
        }
      ]
    },
    {
      "project": "example-web",
      "count": 1,
      "notes": [
        {
          "id": "01M8NL00000000000000000004",
          "title": "Web plan",
          "description": "",
          "project": "example-web",
          "tags": [
            "plan:web-v1",
            "created-by:agent"
          ],
          "updated": "2026-11-04T10:00:00Z"
        }
      ]
    }
  ],
  "count": 4,
  "total": 4,
  "sort": "recent"
}
`

const nlBaseJSONFiltered = `{
  "groups": [
    {
      "project": "example-ops",
      "count": 1,
      "notes": [
        {
          "id": "01M8NL00000000000000000001",
          "title": "Ops runbook",
          "description": "How to page",
          "project": "example-ops",
          "tags": [
            "research",
            "ops"
          ],
          "favorite": true,
          "updated": "2026-11-01T10:00:00Z"
        }
      ]
    }
  ],
  "count": 1,
  "total": 4,
  "query": "o",
  "sort": "name",
  "group": "project",
  "project": "example-ops",
  "fav": true
}
`

// The list reads through the request's handle: a locked project's notes are
// absent from its rows, its chips, its counts, its fold and its JSON, and
// their plans' statuses come from nothing they hold. Without the lock the
// same pages list them.
func TestNlLockedProjectIsAbsent(t *testing.T) {
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
	at := time.Now().UTC().Add(-time.Hour)
	open := libNote(t, mgr, "example-open", "open", "Open note", at, []string{"research", "plan:shared-plan"})
	hidden := libNote(t, mgr, "example-locked", "secret", "Secret note", at.Add(time.Minute), []string{"research", "classified", "plan:shared-plan"})
	cache := libNote(t, mgr, "example-locked", "cc-agent-x", "Secret copy", at.Add(2*time.Minute), []string{"agent-cache"})
	require.NoError(t, store.CreateTask(context.Background(), db, core.Task{ID: "TL", Title: "locked step", Status: core.TaskInProgress,
		ProjectSlug: "example-locked", PlanSlug: "shared-plan", CreatedAt: at, UpdatedAt: at}))

	locked, unlocked := console(view), console(db)
	for _, p := range []string{"/console/notes", "/console/notes?tag=research", "/console/notes?caches=1",
		"/console/notes?plans=1", "/console/notes?q=secret", "/console/notes?format=json", "/console/notes?format=json&caches=1"} {
		rr := getPeek(t, locked, p)
		require.Equal(t, http.StatusOK, rr.Code, p)
		for _, gone := range []string{hidden.ID, cache.ID, "Secret note", "Secret copy", "classified", "in progress"} {
			require.NotContains(t, rr.Body.String(), gone, "%s names the locked project's %s", p, gone)
		}
	}
	page := getHTMLBody(t, locked, "/console/notes?caches=1")
	require.Equal(t, []string{open.ID}, nlRows(page))
	chips := nlChips(t, page)
	nlHasChip(t, chips, "/console/notes?caches=1&tag=research", "research", 1)
	require.Contains(t, chips, "0 agent working copies shown")

	all := getHTMLBody(t, unlocked, "/console/notes?caches=1")
	require.Equal(t, []string{cache.ID, hidden.ID, open.ID}, nlRows(all), "without the lock the list holds them: the absence is the view's")
	nlHasChip(t, nlChips(t, all), "/console/notes?caches=1&tag=research", "research", 2)
}

// The project switcher, Starred and Clear keep the chip and the fold, and
// each counts what its choice would show under them.
func TestNlControlsKeepTheView(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	at := time.Now().UTC().Add(-time.Hour)
	a := libNote(t, mgr, "example-ops", "a", "Alpha", at, []string{"research"})
	libNote(t, mgr, "example-web", "b", "Beta", at, []string{"research"})
	libNote(t, mgr, "example-web", "c", "Gamma", at, nil)
	require.Equal(t, http.StatusSeeOther, postFavorite(mux, "note", a.ID, url.Values{"favorite": {"1"}}).Code)

	main := libMain(t, getHTMLBody(t, mux, "/console/notes?tag=research&caches=1&q=a"))
	require.Contains(t, main, `href="/console/notes?caches=1&amp;fav=1&amp;q=a&amp;tag=research"`, "Starred keeps them")
	require.Contains(t, main, `Starred<span class="n">1</span>`)
	require.Contains(t, main, `class="lib-clear" href="/console/notes?caches=1"`, "Clear drops the chip, keeps the fold")
	for _, keep := range []string{`name="caches" value="1"`, `name="tag" value="research"`} {
		require.Contains(t, main, `<input type="hidden" `+keep+`>`, "typing keeps them")
	}
	require.Contains(t, main, `href="/console/notes?caches=1&amp;project=example-web&amp;q=a&amp;tag=research"`, "a project keeps them")
	require.Contains(t, main, `href="/console/notes?caches=1&amp;q=a&amp;sort=favorites&amp;tag=research">Starred</a>`, "a sort keeps them")
}

// A plan chip's status is the plan its link opens: the slug in the note's
// own project (plans are keyed by project and slug), on the list's row, the
// note's page and its reader alike, even when another project's composition
// of the same slug would win across every project.
func TestNlPlanChipIsTheNotesProjectsPlan(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	at := time.Now().UTC().Add(-time.Hour)
	libNote(t, mgr, "example-web", "cc-plan-shared", "Shared capture", at.Add(-time.Hour),
		[]string{"cc-plan", "plan:shared", "plan-status:approved"})
	a := libNote(t, mgr, "example-ops", "shared-narrative", "Shared narrative", at, []string{"plan:shared"})

	want := `<span class="mono">plan:shared</span> <span class="badge ">ready</span>`
	require.Contains(t, nlRow(t, getHTMLBody(t, mux, "/console/notes"), a.ID), want)
	for _, path := range []string{"/console/notes/" + a.ID, "/console/notes/" + a.ID + "?reader=1"} {
		body := getPeek(t, mux, path).Body.String()
		require.Contains(t, body, `href="/console/plans/shared?project=example-ops"`, path)
		require.Contains(t, body, want, path)
		require.NotContains(t, body, ">approved</span>", "%s: the other project's plan is not this one", path)
	}
}
