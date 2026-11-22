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
	"net/http/httptest"
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

// itFixture is a console with the files layer at level, and its Service, so
// a test can render a fragment the way the handlers do.
func itFixture(t *testing.T, level string) (*sql.DB, *files.Manager, *Service, *http.ServeMux) {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	dataDir := filepath.Join(dir, "data")
	mgr, err := files.NewManager(dataDir, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	svc, err := New(Config{DB: db, Files: mgr, Events: events.NewRecorder(db), DataDir: dataDir, APIKey: testKey, Level: level})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return db, mgr, svc, mux
}

// itNote writes a note with its body, at a time, so a test can order notes.
func itNote(t *testing.T, mgr *files.Manager, project, slug, title, body string, at time.Time) core.Note {
	t.Helper()
	n, err := mgr.WriteNote(context.Background(), core.Note{ID: mustID(t), Slug: slug, Title: title, Project: project,
		Description: "about " + title, Body: body, Created: at, Updated: at})
	require.NoError(t, err)
	return n
}

// itTask makes a task created at a time.
func itTask(t *testing.T, db *sql.DB, project, plan, title string, status core.TaskStatus, at time.Time, deps ...string) core.Task {
	t.Helper()
	task := core.Task{ID: mustID(t), ProjectSlug: project, PlanSlug: plan, Title: title, Status: status,
		DependsOn: deps, CreatedAt: at, UpdatedAt: at}
	if status.Closed() {
		task.ClosedAt = &at
	}
	require.NoError(t, store.CreateTask(context.Background(), db, task))
	return task
}

// itFrame is an item page's frame: from its div to the end of the view.
func itFrame(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `<div class="item-page"`)
	require.NotEqual(t, -1, i, "no item page")
	j := strings.Index(body[i:], "</main>")
	require.NotEqual(t, -1, j, "unterminated item page")
	return body[i : i+j]
}

var (
	itPosRe   = regexp.MustCompile(`<span class="item-pos" data-item-pos>([^<]*)</span>`)
	itStepRe  = regexp.MustCompile(`<a class="item-step" href="([^"]*)" rel="(prev|next)" data-item-step="(?:prev|next)" data-seam-query`)
	itBackRe  = regexp.MustCompile(`<a class="item-back" href="([^"]*)" data-item-back>.*?<span[^>]*>([^<]*)</span></a>`)
	itCrumbRe = regexp.MustCompile(`<nav class="item-crumbs" aria-label="Where this lives">(.*?)</nav>`)
	itHrefRe  = regexp.MustCompile(`<a href="([^"]*)"[^>]*>([^<]*)</a>`)
)

// itPlace reads a frame's place: what it says and its previous and next
// links ("" at an end), unescaped.
func itPlace(t *testing.T, frame string) (pos, prev, next string) {
	t.Helper()
	m := itPosRe.FindStringSubmatch(frame)
	require.NotNil(t, m, "no place on the page")
	pos = html.UnescapeString(m[1])
	for _, s := range itStepRe.FindAllStringSubmatch(frame, -1) {
		if s[2] == "prev" {
			prev = html.UnescapeString(s[1])
		} else {
			next = html.UnescapeString(s[1])
		}
	}
	return pos, prev, next
}

// itCrumbs reads a frame's crumbs as label -> link.
func itCrumbs(t *testing.T, frame string) [][2]string {
	t.Helper()
	m := itCrumbRe.FindStringSubmatch(frame)
	require.NotNil(t, m, "no crumbs")
	var out [][2]string
	for _, c := range itHrefRe.FindAllStringSubmatch(m[1], -1) {
		out = append(out, [2]string{html.UnescapeString(c[2]), html.UnescapeString(c[1])})
	}
	return out
}

// A memory's own page is the memory: no rail, no list bar, no reader pane,
// and no memory of another project; the context column names at most five
// others of its kind in its project, with a link to them all.
func TestItMemoryPageHasNoRail(t *testing.T) {
	_, mgr, _, mux := itFixture(t, "advanced")
	base := time.Now().UTC().Add(-time.Hour)
	var pay []core.Memory
	for i := range 8 {
		pay = append(pay, libMemory(t, mgr, core.KindGotcha, "example-pay", fmt.Sprintf("pay-gotcha-%d", i), base.Add(time.Duration(i)*time.Minute)))
	}
	web := []core.Memory{
		libMemory(t, mgr, core.KindGotcha, "example-web", "web-only-gotcha", base),
		libMemory(t, mgr, core.KindDecision, "example-web", "web-only-decision", base),
	}
	body := getHTMLBody(t, mux, "/console/memories/"+pay[3].ID)
	frame := itFrame(t, body)
	for _, absent := range []string{`class="rail-item`, `id="lib-reader"`, `class="mv2-bar"`, `class="lib-layout"`} {
		require.NotContains(t, body, absent)
	}
	for _, m := range web {
		require.NotContains(t, body, m.Name, "another project's memory has no row here")
		require.NotContains(t, body, m.ID)
	}
	named := 0
	for _, m := range pay {
		if m.ID != pay[3].ID && strings.Contains(frame, "/console/memories/"+m.ID) {
			named++
		}
	}
	require.LessOrEqual(t, named, itemMoreLimit+2, "five more of its kind, and its two neighbours")
	require.Contains(t, frame, "body of pay-gotcha-3")
	require.Contains(t, frame, `<h2 class="item-card-hd">More gotchas in example-pay</h2>`)
	require.Contains(t, frame, `<a class="item-all" href="/console/memories?kind=gotcha&amp;project=example-pay">All 8 gotchas in example-pay`)
	require.Contains(t, frame, `<span class="path" title="`)
	require.Contains(t, frame, `data-copy="`+pay[3].FilePath+`"`, "the file path copies")
}

// The place and the crumbs follow the list the URL names: its order, its
// filters on the neighbours' links, its sort and grouping on the crumbs, and
// the way back to that list. Without one, the place is the memory's project
// and kind, newest first; a list that does not hold the memory reads as none.
func TestItPlaceAndCrumbsFollowTheListQuery(t *testing.T) {
	db, mgr, _, mux := itFixture(t, "advanced")
	base := time.Now().UTC().Add(-time.Hour)
	alpha := libMemory(t, mgr, core.KindGotcha, "example-pay", "pay-alpha", base)
	mike := libMemory(t, mgr, core.KindGotcha, "example-pay", "pay-mike", base.Add(time.Minute))
	zulu := libMemory(t, mgr, core.KindGotcha, "example-pay", "pay-zulu", base.Add(2*time.Minute))
	libMemory(t, mgr, core.KindDecision, "example-pay", "pay-rule", base.Add(3*time.Minute))
	web := libMemory(t, mgr, core.KindGotcha, "example-web", "web-newest", base.Add(4*time.Minute))

	// No list: its project and kind, newest first.
	frame := itFrame(t, getHTMLBody(t, mux, "/console/memories/"+zulu.ID))
	pos, prev, next := itPlace(t, frame)
	require.Equal(t, "1 of 3 gotchas in example-pay", pos)
	require.Empty(t, prev)
	require.Equal(t, "/console/memories/"+mike.ID, next)
	back := itBackRe.FindStringSubmatch(frame)
	require.Equal(t, "/console/memories?project=example-pay", html.UnescapeString(back[1]))
	require.Equal(t, "example-pay’s memories", back[2])
	require.Equal(t, [][2]string{
		{"example-pay", "/console/memories?project=example-pay"},
		{"Memories", "/console/memories?project=example-pay"},
		{"gotchas", "/console/memories?kind=gotcha&project=example-pay"},
	}, itCrumbs(t, frame))

	// The list sorted by name: zulu is last, and every link keeps the list.
	const qs = "?group=project&kind=gotcha&project=example-pay&sort=name"
	frame = itFrame(t, getHTMLBody(t, mux, "/console/memories/"+zulu.ID+qs))
	pos, prev, next = itPlace(t, frame)
	require.Equal(t, "3 of 3 gotchas in example-pay", pos)
	require.Equal(t, "/console/memories/"+mike.ID+qs, prev)
	require.Empty(t, next)
	back = itBackRe.FindStringSubmatch(frame)
	require.Equal(t, "/console/memories"+qs, html.UnescapeString(back[1]))
	require.Equal(t, "example-pay’s gotchas", back[2])
	require.Equal(t, [][2]string{
		{"example-pay", "/console/memories?group=project&project=example-pay&sort=name"},
		{"Memories", "/console/memories?group=project&project=example-pay&sort=name"},
		{"gotchas", "/console/memories?group=project&kind=gotcha&project=example-pay&sort=name"},
	}, itCrumbs(t, frame), "the crumbs keep the list's sort and grouping")
	alphaFrame := itFrame(t, getHTMLBody(t, mux, "/console/memories/"+alpha.ID+qs))
	pos, prev, next = itPlace(t, alphaFrame)
	require.Equal(t, "1 of 3 gotchas in example-pay", pos)
	require.Empty(t, prev)
	require.Equal(t, "/console/memories/"+mike.ID+qs, next)

	// Every project's gotchas, newest first: another project's memory is a
	// neighbour, and the words name no project.
	frame = itFrame(t, getHTMLBody(t, mux, "/console/memories/"+zulu.ID+"?kind=gotcha"))
	pos, prev, next = itPlace(t, frame)
	require.Equal(t, "2 of 4 gotchas", pos)
	require.Equal(t, "/console/memories/"+web.ID+"?kind=gotcha", prev)
	require.Equal(t, "/console/memories/"+mike.ID+"?kind=gotcha", next)

	// Text and starred filters narrow it, and say so.
	frame = itFrame(t, getHTMLBody(t, mux, "/console/memories/"+mike.ID+"?q=PAY-&sort=name"))
	pos, _, _ = itPlace(t, frame)
	require.Equal(t, "2 of 4 memories matching “pay-”", pos, "alpha, mike, rule, zulu by name; a substring in any case")

	// A list that does not hold the memory: its own project and kind.
	frame = itFrame(t, getHTMLBody(t, mux, "/console/memories/"+zulu.ID+"?kind=decision"))
	pos, _, next = itPlace(t, frame)
	require.Equal(t, "1 of 3 gotchas in example-pay", pos)
	require.Equal(t, "/console/memories/"+mike.ID, next)
	require.Equal(t, "/console/memories?project=example-pay", html.UnescapeString(itBackRe.FindStringSubmatch(frame)[1]))

	// A project this console knows is a crumb to its page; the global scope
	// is the collection's own word for it.
	_, err := store.EnsureProject(context.Background(), db, "example-pay", "Payments")
	require.NoError(t, err)
	shared := libMemory(t, mgr, core.KindReference, "", "shared-guide", base)
	require.Equal(t, [2]string{"example-pay", "/console/projects/example-pay"}, itCrumbs(t, itFrame(t, getHTMLBody(t, mux, "/console/memories/"+zulu.ID)))[0])
	require.Equal(t, [][2]string{
		{"Shared globally", "/console/memories?project=global"},
		{"Memories", "/console/memories?project=global"},
		{"references", "/console/memories?kind=reference&project=global"},
	}, itCrumbs(t, itFrame(t, getHTMLBody(t, mux, "/console/memories/"+shared.ID))))

	// A bad list is still a 400, as it is on the list.
	require.Equal(t, http.StatusBadRequest, getPeek(t, mux, "/console/memories/"+zulu.ID+"?sort=bogus").Code)
}

// k and j are the place's links: a note's neighbours in its project, newest
// first; a plan step's in its plan, as the plan went; a task's outside a plan
// in its project's list, oldest first. library.js presses them.
func TestItStepLinksPointAtTheNeighbours(t *testing.T) {
	db, mgr, _, mux := itFixture(t, "advanced")
	base := time.Now().UTC().Add(-time.Hour)
	oldest := itNote(t, mgr, "example-pay", "n-old", "Old note", "old", base)
	middle := itNote(t, mgr, "example-pay", "n-mid", "Middle note", "mid", base.Add(time.Minute))
	newest := itNote(t, mgr, "example-pay", "n-new", "New note", "new", base.Add(2*time.Minute))
	itNote(t, mgr, "example-web", "n-web", "Web note", "web", base.Add(3*time.Minute))
	pos, prev, next := itPlace(t, itFrame(t, getHTMLBody(t, mux, "/console/notes/"+middle.ID)))
	require.Equal(t, "2 of 3 notes in example-pay", pos)
	require.Equal(t, "/console/notes/"+newest.ID, prev)
	require.Equal(t, "/console/notes/"+oldest.ID, next)
	pos, prev, next = itPlace(t, itFrame(t, getHTMLBody(t, mux, "/console/notes/"+middle.ID+"?sort=name")))
	require.Equal(t, "1 of 4 notes", pos, "every project's notes by title: Middle, New, Old, Web")
	require.Empty(t, prev)
	require.Equal(t, "/console/notes/"+newest.ID+"?sort=name", next)

	// A plan: its closed step first, then the rest in dependency order.
	first := itTask(t, db, "example-pay", "pay-v2", "schema", core.TaskDone, base)
	second := itTask(t, db, "example-pay", "pay-v2", "dispatcher", core.TaskOpen, base.Add(time.Minute))
	third := itTask(t, db, "example-pay", "pay-v2", "load test", core.TaskOpen, base.Add(2*time.Minute), second.ID)
	frame := itFrame(t, getHTMLBody(t, mux, "/console/tasks/"+second.ID))
	pos, prev, next = itPlace(t, frame)
	require.Equal(t, "Step 2 of 3 in this plan", pos)
	require.Equal(t, "/console/tasks/"+first.ID, prev)
	require.Equal(t, "/console/tasks/"+third.ID, next)
	card := frame[strings.Index(frame, `data-item-card="plan"`):]
	require.Regexp(t, `(?s)schema</a><span[^>]*>done</span>.*<li aria-current="step"><span class="item-step-t">dispatcher</span>.*load test</a><span[^>]*>blocked</span>`, card)
	require.Contains(t, card, `<p class="item-card-sub" data-plan-summary>1/3 done &middot; <span data-count="ready">1 ready</span> &middot; <span data-count="blocked">1 blocked</span></p>`)

	// Outside a plan: its project's tasks, oldest first.
	a := itTask(t, db, "example-ops", "", "ops a", core.TaskOpen, base)
	b := itTask(t, db, "example-ops", "", "ops b", core.TaskOpen, base.Add(time.Minute))
	c := itTask(t, db, "example-ops", "", "ops c", core.TaskOpen, base.Add(2*time.Minute))
	pos, prev, next = itPlace(t, itFrame(t, getHTMLBody(t, mux, "/console/tasks/"+b.ID)))
	require.Equal(t, "2 of 3 tasks in example-ops", pos)
	require.Equal(t, "/console/tasks/"+a.ID, prev)
	require.Equal(t, "/console/tasks/"+c.ID, next)
	// The Tasks list newest first, as the URL names it.
	pos, prev, next = itPlace(t, itFrame(t, getHTMLBody(t, mux, "/console/tasks/"+b.ID+"?project=example-ops&sort=newest")))
	require.Equal(t, "2 of 3 tasks in example-ops", pos)
	require.Equal(t, "/console/tasks/"+c.ID+"?project=example-ops&sort=newest", prev)
	require.Equal(t, "/console/tasks/"+a.ID+"?project=example-ops&sort=newest", next)

	js := string(libraryJS)
	require.Contains(t, js, `'.item-page a[data-item-step="' + which + '"]'`, "k and j press the page's own links")
	require.Contains(t, js, "stepItemPage(e);")
	require.Contains(t, js, "if (e.repeat) return;\n    link.click();")
}

// A task's page leads with its whole id and Copy id: the owner hands a task
// to an agent by its id.
func TestItTaskPageShowsTheWholeIDAndCopyID(t *testing.T) {
	db, _, _, mux := itFixture(t, "advanced")
	task := itTask(t, db, "example-ops", "", "rotate the keys", core.TaskOpen, time.Now().UTC())
	frame := itFrame(t, getHTMLBody(t, mux, "/console/tasks/"+task.ID))
	facts := frame[strings.Index(frame, `<div class="item-facts">`):]
	require.True(t, strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(facts, `<div class="item-facts">`)),
		`<span class="item-id" data-item-whole-id="`+task.ID+`"><span class="mono">`+task.ID+`</span>`), "the whole id comes first")
	require.Regexp(t, `<button type="button" class="copy-btn item-copy-id" data-copy="`+task.ID+`" aria-label="Copy id"[^>]*>.*?<span>Copy id</span></button>`, facts)
	require.NotContains(t, frame, ">"+shortID(task.ID)+"<", "never the short id alone")
	require.Contains(t, frame, `<h1 class="item-title">rotate the keys`)
}

// A note's section headings are its On this page list, each a link to the
// heading's id; the body carries the ids, and the reader fragment keeps the
// body it had, with none.
func TestItNoteHeadingsUnderOnThisPage(t *testing.T) {
	_, mgr, _, mux := itFixture(t, "advanced")
	body := "# Postmortem\n\nintro\n\n## Summary\n\ntext\n\n### Root cause\n\nwhy\n\n## Summary\n\nagain\n\n" +
		"## <script>alert(1)</script> Timeline {#x onclick=alert(1)}\n\nwhen\n"
	n := itNote(t, mgr, "example-ops", "pm", "Webhook storm postmortem", body, time.Now().UTC())
	frame := itFrame(t, getHTMLBody(t, mux, "/console/notes/"+n.ID))
	toc := frame[strings.Index(frame, `<h2 class="item-card-hd">On this page</h2>`):]
	toc = toc[:strings.Index(toc, "</section>")]
	require.Equal(t, []string{"#doc-summary", "#doc-root-cause", "#doc-summary-2", "#doc-alert-1-timeline-x-onclick-alert-1"},
		itHrefs(toc), "h2 and h3, in order, each its own anchor")
	require.Contains(t, toc, `<li class="item-toc-h3"><a href="#doc-root-cause">Root cause</a></li>`)
	main := frame[strings.Index(frame, `<article class="item-main">`):strings.Index(frame, `<aside class="item-side"`)]
	for _, id := range []string{"doc-summary", "doc-root-cause", "doc-summary-2", "doc-alert-1-timeline-x-onclick-alert-1"} {
		require.Regexp(t, `<h[23] id="`+id+`">`, main)
	}
	require.NotContains(t, strings.ToLower(frame), "<script")
	require.NotRegexp(t, `<h[1-6][^>]*\son[a-z]+=`, frame)
	require.Contains(t, frame, "<h1>Postmortem</h1>", "the body's h1 is no section")

	reader := getPeek(t, mux, "/console/notes/"+n.ID+"?reader=1").Body.String()
	require.Contains(t, reader, "<h2>Summary</h2>")
	require.NotContains(t, reader, `id="doc-`)

	// One heading or none makes no list.
	short := itNote(t, mgr, "example-ops", "short", "Short", "## Only\n\nx\n", time.Now().UTC())
	require.NotContains(t, getHTMLBody(t, mux, "/console/notes/"+short.ID), "On this page")
}

// itHrefs lists the links in a stretch of markup, in order.
func itHrefs(markup string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`href="([^"]*)"`).FindAllStringSubmatch(markup, -1) {
		out = append(out, m[1])
	}
	return out
}

// itRender renders a fragment through fn on a recorder and answers its body.
func itRender(t *testing.T, fn func(http.ResponseWriter, *http.Request)) string {
	t.Helper()
	rr := httptest.NewRecorder()
	fn(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	return rr.Body.String()
}

// itJSON decodes a JSON answer, or a value as JSON would carry it.
func itJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	out := map[string]any{}
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// JSON, ?peek=1 and ?reader=1 keep their answers: the same model through the
// same template as before the item page, and none of the frame in them.
func TestItJSONPeekReaderKeepTheirAnswers(t *testing.T) {
	db, mgr, svc, mux := itFixture(t, "advanced")
	ctx := context.Background()
	m := writeMemory(t, mgr, core.KindGotcha, "example-ops", "pager-loop", "the pager loops")
	n := itNote(t, mgr, "example-ops", "ops-notes", "Ops notes", "## One\n\nx\n\n## Two\n\ny\n", time.Now().UTC())
	task := itTask(t, db, "example-ops", "", "page the oncall", core.TaskOpen, time.Now().UTC())
	const lqs = "&kind=gotcha&sort=name"

	md, ok, err := svc.memoryDetailByID(ctx, m.ID)
	require.NoError(t, err)
	require.True(t, ok)
	nd, ok, err := svc.noteDetailByID(ctx, n.ID)
	require.NoError(t, err)
	require.True(t, ok)
	td, ok, err := svc.taskDetailByID(ctx, task.ID)
	require.NoError(t, err)
	require.True(t, ok)
	lq := libQuery{Sort: "name", Kind: "gotcha"}
	tq := taskQuery{Sort: taskSortNewest, Page: 1}
	tdNext := td
	tdNext.Next = tq.listNext(task.ID)

	for _, tc := range []struct {
		path  string
		model any
		peek  func(http.ResponseWriter, *http.Request)
		read  func(http.ResponseWriter, *http.Request)
		rqs   string
	}{
		{"/console/memories/" + m.ID, md,
			func(w http.ResponseWriter, r *http.Request) { svc.renderFragment(w, r, "memory", md) },
			func(w http.ResponseWriter, r *http.Request) {
				svc.renderReader(w, r, "memories", "memory-reader", svc.memoryReaderOf(ctx, md, lq))
			}, lqs},
		{"/console/notes/" + n.ID, nd,
			func(w http.ResponseWriter, r *http.Request) { svc.renderFragment(w, r, "note", nd) },
			func(w http.ResponseWriter, r *http.Request) {
				svc.renderReader(w, r, "notes", "note-reader", svc.noteReaderOf(ctx, nd, libQuery{Sort: "name"}))
			}, "&sort=name"},
		{"/console/tasks/" + task.ID, td,
			func(w http.ResponseWriter, r *http.Request) { svc.renderFragment(w, r, "task", td) },
			func(w http.ResponseWriter, r *http.Request) { svc.renderReader(w, r, "tasks", "task-reader", tdNext) },
			"&sort=newest"},
	} {
		raw, err := json.Marshal(tc.model)
		require.NoError(t, err)
		var got map[string]any
		getJSON(t, mux, tc.path+"?format=json", &got)
		require.Equal(t, itJSON(t, raw), got, "%s: the JSON is the detail model", tc.path)

		peek := getPeek(t, mux, tc.path+"?peek=1").Body.String()
		require.Equal(t, itRender(t, tc.peek), peek, "%s: ?peek=1 is the peek fragment", tc.path)
		reader := getPeek(t, mux, tc.path+"?reader=1"+tc.rqs).Body.String()
		require.Equal(t, itRender(t, tc.read), reader, "%s: ?reader=1 is the reader fragment", tc.path)
		for _, frag := range []string{peek, reader} {
			require.NotContains(t, frag, "item-page")
			require.NotContains(t, frag, "<html")
		}
	}
	require.Contains(t, getPeek(t, mux, "/console/notes/"+n.ID+"?reader=1").Body.String(), "<h2>One</h2>")
}

// Every action on an item's page returns to it: a memory's star, archive
// and restore; a task's status; a note's delete returns to the way back,
// since the note has no page left.
func TestItActionsReturnToTheItemPage(t *testing.T) {
	db, mgr, _, mux := itFixture(t, "advanced")
	m := writeMemory(t, mgr, core.KindGotcha, "example-ops", "disk-alarm", "disk alarm")
	page := "/console/memories/" + m.ID
	frame := itFrame(t, getHTMLBody(t, mux, page))
	require.Equal(t, 2, strings.Count(frame, `<input type="hidden" name="next" value="`+page+`">`), "the star and the archive")
	rr := postForm(mux, page+"/archive", url.Values{"next": {page}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, page+"?notice=Archived+disk-alarm.", rr.Header().Get("Location"))
	frame = itFrame(t, getHTMLBody(t, mux, page))
	require.Contains(t, frame, "This memory is archived.", "the banner leads the page")
	require.Regexp(t, lrRestoreForm, frame)
	require.Contains(t, frame, `<input type="hidden" name="next" value="`+page+`">`, "Restore returns here")
	require.NotContains(t, frame, page+`/archive"`, "an archived memory offers no Archive")
	rr = postForm(mux, page+"/restore", url.Values{"next": {page}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.True(t, strings.HasPrefix(rr.Header().Get("Location"), page+"?notice="), rr.Header().Get("Location"))

	task := itTask(t, db, "example-ops", "", "close me", core.TaskOpen, time.Now().UTC())
	rr = postForm(mux, "/console/tasks/"+task.ID+"/status", url.Values{"to": {"done"}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/tasks/"+task.ID+"?notice=Marked+done.", rr.Header().Get("Location"))
	listed := itFrame(t, getHTMLBody(t, mux, "/console/tasks/"+task.ID+"?project=example-ops"))
	require.Contains(t, listed, `name="next" value="/console/tasks/`+task.ID+`?project=example-ops"`, "a list's query rides along")

	n := itNote(t, mgr, "example-ops", "scratch", "Scratch", "x", time.Now().UTC())
	frame = itFrame(t, getHTMLBody(t, mux, "/console/notes/"+n.ID))
	require.Contains(t, frame, `<form method="post" action="/console/notes/`+n.ID+`/delete">`+"\n"+
		`          <input type="hidden" name="next" value="/console/notes?project=example-ops">`)
	rr = postForm(mux, "/console/notes/"+n.ID+"/delete", url.Values{"next": {"/console/notes?project=example-ops"}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/notes?notice=Deleted+Scratch.&project=example-ops", rr.Header().Get("Location"))
}

// The utility score is a Standard-level fact on the page as in the reader.
func TestItMemoryPageKeepsTheLevelGate(t *testing.T) {
	_, mgr, _, mux := itFixture(t, "basic")
	m := writeMemory(t, mgr, core.KindGotcha, "example-ops", "basic-mem", "basic")
	frame := itFrame(t, getHTMLBody(t, mux, "/console/memories/"+m.ID))
	require.NotContains(t, frame, "<span>utility</span>")
	require.Contains(t, frame, "never<span>surfaced</span>")
	_, mgr, _, mux = itFixture(t, "standard")
	m = writeMemory(t, mgr, core.KindGotcha, "example-ops", "std-mem", "std")
	require.Contains(t, itFrame(t, getHTMLBody(t, mux, "/console/memories/"+m.ID)), "<span>utility</span>")
}

// A locked project's item is refused as it always was, and its items are
// absent from every other item's place: its rows are not in the window the
// request's handle reads, so they neither count nor neighbour.
func TestItLockedProjectIsAbsent(t *testing.T) {
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
	base := time.Now().UTC().Add(-time.Hour)
	older := libMemory(t, mgr, core.KindGotcha, "example-open", "open-older", base)
	hidden := libMemory(t, mgr, core.KindGotcha, "example-locked", "secret-middle", base.Add(time.Minute))
	newer := libMemory(t, mgr, core.KindGotcha, "example-open", "open-newer", base.Add(2*time.Minute))
	noteOld := itNote(t, mgr, "example-open", "n-old", "Open old", "x", base)
	noteHidden := itNote(t, mgr, "example-locked", "n-secret", "Secret note", "x", base.Add(time.Minute))
	itNote(t, mgr, "example-open", "n-new", "Open new", "x", base.Add(2*time.Minute))
	taskOld := itTask(t, db, "example-open", "", "open first", core.TaskOpen, base)
	taskHidden := itTask(t, db, "example-locked", "", "secret task", core.TaskOpen, base.Add(time.Minute))
	itTask(t, db, "example-open", "", "open last", core.TaskOpen, base.Add(2*time.Minute))

	for _, p := range []string{"/console/memories/" + hidden.ID, "/console/notes/" + noteHidden.ID, "/console/tasks/" + taskHidden.ID} {
		rr := lrGet(t, locked, p)
		require.Equal(t, http.StatusNotFound, rr.Code, p)
		require.NotContains(t, rr.Body.String(), "secret", p)
	}

	for _, tc := range []struct {
		path, lockedPos, openPos string
	}{
		{"/console/memories/" + newer.ID + "?kind=gotcha", "1 of 2 gotchas", "1 of 3 gotchas"},
		{"/console/memories/" + older.ID + "?kind=gotcha", "2 of 2 gotchas", "3 of 3 gotchas"},
		{"/console/notes/" + noteOld.ID + "?sort=favorites", "2 of 2 notes", "3 of 3 notes"},
		{"/console/tasks/" + taskOld.ID + "?sort=newest", "2 of 2 tasks", "3 of 3 tasks"},
	} {
		body := getHTMLBody(t, locked, tc.path)
		pos, prev, next := itPlace(t, itFrame(t, body))
		require.Equal(t, tc.lockedPos, pos, tc.path)
		for _, id := range []string{hidden.ID, noteHidden.ID, taskHidden.ID} {
			require.NotContains(t, prev+next, id, tc.path)
			require.NotContains(t, body, id, tc.path)
		}
		require.NotContains(t, body, "secret", tc.path)
		openPos, _, _ := itPlace(t, itFrame(t, getHTMLBody(t, open, tc.path)))
		require.Equal(t, tc.openPos, openPos, "%s: without the lock it counts; the absence is the view's", tc.path)
	}
}
