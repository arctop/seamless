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
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/demokit"
	"github.com/arctop/seamless/internal/diary"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
)

// dyFixture is the Diary fixture (internal/demokit: three weeks around now,
// in local days, with every link case) seeded into a fresh data dir.
type dyFixture struct {
	dir string
	fx  demokit.DiaryFixture
}

// dyShared is the Diary fixture, seeded once for the package's run: seeding
// three weeks replays every store call those days made, which the race
// detector makes slow, so each test works on a copy of it instead.
var dyShared struct {
	once sync.Once
	dir  string
	fx   demokit.DiaryFixture
	err  error
}

// TestMain removes the shared fixture once every test has run.
func TestMain(m *testing.M) {
	code := m.Run()
	if dyShared.dir != "" {
		_ = os.RemoveAll(dyShared.dir)
	}
	os.Exit(code)
}

// dySeed is a fresh copy of the seeded fixture, the test's own to read and
// write: a data dir (the database, memories and notes) and the fixture's map.
func dySeed(t *testing.T) dyFixture {
	t.Helper()
	dyShared.once.Do(func() {
		dyShared.dir, dyShared.err = os.MkdirTemp("", "seamless-console-diary-")
		if dyShared.err != nil {
			return
		}
		seeder, err := demokit.New(dyShared.dir)
		if err != nil {
			dyShared.err = err
			return
		}
		dyShared.fx, dyShared.err = seeder.SeedDiary(time.Local)
		if err := seeder.Close(); dyShared.err == nil {
			dyShared.err = err
		}
	})
	require.NoError(t, dyShared.err, "seeding the Diary fixture")
	dir := t.TempDir()
	require.NoError(t, os.CopyFS(dir, os.DirFS(dyShared.dir)))
	return dyFixture{dir: dir, fx: dyShared.fx}
}

// dyConsole builds a console over the seeded data dir, read through db (the
// database itself, or a filtered handle on it), with the features given.
func dyConsole(t *testing.T, f dyFixture, db, write *sql.DB, feats config.Features) *http.ServeMux {
	t.Helper()
	mgr, err := files.NewManager(f.dir, write, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	svc, err := New(Config{
		DB: db, WriteDB: write, Events: events.NewRecorder(write).ReadingFrom(db), Files: mgr, APIKey: testKey,
		DataDir: f.dir, Retrieve: retrieve.New(db, nil, config.Defaults().Budgets, nil),
		Features: feats, Level: "advanced",
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return mux
}

// dyOpen opens the fixture's database.
func dyOpen(t *testing.T, f dyFixture) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(f.dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// dySeeded seeds the fixture and builds a console over it.
func dySeeded(t *testing.T, feats config.Features) (*sql.DB, *http.ServeMux, demokit.DiaryFixture) {
	t.Helper()
	f := dySeed(t)
	db := dyOpen(t, f)
	return db, dyConsole(t, f, db, db, feats), f.fx
}

// dyDayPath is the Diary's page of a fixture day.
func dyDayPath(day time.Time) string { return "/console/diary?day=" + day.Format("2006-01-02") }

// dyPage GETs a page and requires a 200.
func dyPage(t *testing.T, mux *http.ServeMux, path string) string {
	t.Helper()
	rr := getPeek(t, mux, path)
	require.Equal(t, http.StatusOK, rr.Code, "GET %s: %s", path, firstOf(rr.Body.String(), 400))
	return rr.Body.String()
}

// dyAnswer GETs a day's JSON answer, raw and decoded.
func dyAnswer(t *testing.T, mux *http.ServeMux, path string) ([]byte, diaryData) {
	t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	req := httptest.NewRequest(http.MethodGet, path+sep+"format=json", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code, "GET %s (json): %s", path, firstOf(rr.Body.String(), 400))
	var d diaryData
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &d))
	return rr.Body.Bytes(), d
}

func firstOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

var dyHrefRe = regexp.MustCompile(`href="(/console/[^"]*)"`)

// dyMain is the page's main content: the view, without the sidebar (which
// links every screen) or the scripts.
func dyMain(t *testing.T, page string) string {
	t.Helper()
	start, end := strings.Index(page, `<main class="main"`), strings.Index(page, `</main>`)
	require.True(t, start >= 0 && end > start, "the page has its main view")
	return page[start:end]
}

// dyHTMLHrefs is every /console/ link in a page's main view.
func dyHTMLHrefs(t *testing.T, page string) []string {
	t.Helper()
	var out []string
	for _, m := range dyHrefRe.FindAllStringSubmatch(dyMain(t, page), -1) {
		out = append(out, html.UnescapeString(m[1]))
	}
	return out
}

// dyJSONHrefs is every href a JSON answer carries, empty ones included, and
// how many refs (objects with a kind and an id) carry no href key at all.
func dyJSONHrefs(t *testing.T, raw []byte) (hrefs []string, missing int) {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal(raw, &v))
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case map[string]any:
			_, kind := x["kind"]
			_, id := x["id"]
			h, has := x["href"]
			if kind && id {
				if !has {
					missing++
				} else {
					hrefs = append(hrefs, h.(string))
				}
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(v)
	return hrefs, missing
}

// dyRefs is every Ref an entry names, as linkEntry walks them.
func dyRefs(e diary.Entry) []diary.Ref {
	var out []diary.Ref
	add := func(r diary.Ref) {
		out = append(out, r)
		if r.Owner != nil {
			out = append(out, *r.Owner)
		}
	}
	out = append(out, e.Projects...)
	for _, ts := range [][]diary.Thread{e.Spotlight, e.Also} {
		for _, th := range ts {
			if th.Plan != nil {
				add(*th.Plan)
			}
			for _, m := range th.Moved {
				add(m.Task)
			}
			for _, sd := range th.Sessions {
				add(sd.Session)
			}
			for _, r := range th.Kept {
				add(r)
			}
			for _, r := range th.Commits {
				add(r)
			}
		}
	}
	for _, sd := range e.Short.Sessions {
		add(sd.Session)
	}
	for _, items := range [][]diary.KeptItem{e.Kept.Memories, e.Kept.Notes, e.Kept.Trials} {
		for _, it := range items {
			add(it.Item)
		}
	}
	for _, r := range e.Moments.Shipped {
		add(r)
	}
	for _, r := range e.Moments.Commits {
		add(r)
	}
	for _, f := range e.Moments.Flags {
		add(f.Session)
	}
	for _, m := range e.Moments.Mishaps {
		add(m.Event)
		add(m.Session)
	}
	for _, r := range e.Usage.Unreported {
		add(r)
	}
	return out
}

// dyThreads is every thread of a day, the spotlight's first.
func dyThreads(e diary.Entry) []diary.Thread { return slices.Concat(e.Spotlight, e.Also) }

// dyDays is every fixture day's page and answer.
type dyDay struct {
	day  demokit.DiaryDay
	page string
	raw  []byte
	data diaryData
}

func dyAllDays(t *testing.T, mux *http.ServeMux, fx demokit.DiaryFixture) []dyDay {
	t.Helper()
	var out []dyDay
	for _, d := range fx.Days {
		path := dyDayPath(d.Date)
		raw, data := dyAnswer(t, mux, path)
		out = append(out, dyDay{day: d, page: dyPage(t, mux, path), raw: raw, data: data})
	}
	return out
}

// The links: every href the Diary renders, in the page or the JSON, answers
// through the same handler stack -- no 404, no 400, no switched-off page --
// and each link case of the fixture links where the plan says.
func TestDiary_LinkIntegrity(t *testing.T) {
	for _, research := range []bool{true, false} {
		name := map[bool]string{true: "research on", false: "research off"}[research]
		t.Run(name, func(t *testing.T) {
			db, mux, fx := dySeeded(t, config.Features{Diary: true, Research: research})
			days := dyAllDays(t, mux, fx)

			where := map[string]string{} // href -> the day that rendered it
			for _, d := range days {
				key := d.day.Date.Format("2006-01-02")
				jsonHrefs, missing := dyJSONHrefs(t, d.raw)
				require.Zero(t, missing, "%s: every ref in the JSON carries an href, empty when it has no page", key)
				fromJSON := map[string]bool{}
				for _, h := range jsonHrefs {
					if h != "" {
						fromJSON[h] = true
						where[h] = key
						require.True(t, strings.HasPrefix(h, "/console/"), "%s: %q", key, h)
					}
				}
				for _, h := range dyHTMLHrefs(t, d.page) {
					where[h] = key
					switch {
					case strings.HasPrefix(h, diaryPath), strings.HasPrefix(h, "/console/sessions?day="), strings.HasPrefix(h, "#"):
						continue // the day's own navigation
					}
					require.True(t, fromJSON[h], "%s: the page links %s, which no ref of its JSON carries: the page and the "+
						"answer must build one href per item", key, h)
				}
			}
			hrefs := make([]string, 0, len(where))
			for h := range where {
				hrefs = append(hrefs, h)
			}
			sort.Strings(hrefs)
			require.NotEmpty(t, hrefs)
			// Every link is fetched as the page draws it: an entity link as
			// the detail pane loads it (?peek=1), which resolves the item the
			// way its full page does, and the full page for the first two of
			// each kind; every other link (a day, a day view, a project tab)
			// in full.
			full := map[string]int{}
			for _, h := range hrefs {
				kind := dyEntityPrefix(h)
				var targets []string
				if kind == "" || full[kind] < 2 {
					targets = append(targets, h)
					full[kind]++
				}
				if kind != "" {
					targets = append(targets, dyWithPeek(h))
				}
				for _, target := range targets {
					rr := getPeek(t, mux, target)
					require.NotContains(t, []int{http.StatusNotFound, http.StatusBadRequest}, rr.Code,
						"%s (rendered on %s) answers %d", target, where[h], rr.Code)
					require.Less(t, rr.Code, 500, "%s (rendered on %s)", target, where[h])
					body := rr.Body.String()
					require.NotContains(t, body, "data-feature-off", "%s links into a switched-off feature", target)
					require.NotContains(t, body, "is switched off", "%s links into a switched-off feature", target)
				}
			}
			for _, kind := range []string{"/console/tasks/", "/console/sessions/", "/console/memories/", "/console/notes/",
				"/console/projects/", "/console/plans/", "/console/events/"} {
				require.Positive(t, full[kind], "the fixture's days link at least one %s page", kind)
			}

			l := fx.Links
			all := func(fn func(diary.Ref) bool) []diary.Ref {
				var out []diary.Ref
				for _, d := range days {
					for _, r := range dyRefs(d.data.Entry) {
						if fn(r) {
							out = append(out, r)
						}
					}
				}
				return out
			}
			// A deleted task is text marked as gone, never a link.
			deleted := all(func(r diary.Ref) bool { return r.Kind == diary.RefTask && r.ID == l.DeletedTask })
			require.NotEmpty(t, deleted, "the deleted task's history names it")
			for _, r := range deleted {
				require.False(t, r.Resolved)
				require.Empty(t, r.Href)
			}
			require.NotContains(t, where, "/console/tasks/"+l.DeletedTask)
			gone := false
			for _, d := range days {
				if strings.Contains(d.page, `title="No longer exists">`+shortID(l.DeletedTask)+`<`) {
					gone = true
				}
			}
			require.True(t, gone, "the deleted task shows its short id, marked as no longer existing")

			// A task-only plan links to its project's Plans & tasks tab.
			require.Contains(t, where, "/console/projects/"+l.TaskOnlyPlanProject+"?tab=tasks")
			require.NotContains(t, where, "/console/plans/"+l.TaskOnlyPlan)

			// One slug in two projects: each thread links to its own.
			var withPage, withTab int
			for _, d := range days {
				for _, th := range dyThreads(d.data.Entry) {
					if th.Plan == nil || th.Plan.ID != l.SharedSlug {
						continue
					}
					switch th.Project {
					case l.SharedSlugProjects[0]:
						require.Equal(t, "/console/plans/"+l.SharedSlug, th.Plan.Href)
						withPage++
					case l.SharedSlugProjects[1]:
						require.Equal(t, "/console/projects/"+l.SharedSlugProjects[1]+"?tab=tasks", th.Plan.Href)
						withTab++
					}
				}
			}
			require.Positive(t, withPage, "the slug's thread in the project holding its note links the plan page")
			require.Positive(t, withTab, "the slug's thread in the project with steps only links its project's tab")

			// A superseded memory keeps its page and is marked.
			sup := all(func(r diary.Ref) bool { return r.Kind == diary.RefMemory && r.ID == l.SupersededMemory })
			require.NotEmpty(t, sup)
			for _, r := range sup {
				require.Equal(t, diary.MemorySuperseded, r.Status)
				require.Equal(t, "/console/memories/"+l.SupersededMemory, r.Href)
			}
			marked := false
			for _, d := range days {
				if regexp.MustCompile(`href="/console/memories/` + l.SupersededMemory + `" data-peek>[^<]*</a><span class="badge warn">superseded</span>`).
					MatchString(d.page) {
					marked = true
				}
			}
			require.True(t, marked, "the superseded memory is marked where it is kept")

			// A session in the global scope links to its page; its project is text.
			glob := all(func(r diary.Ref) bool { return r.Kind == diary.RefSession && r.ID == l.GlobalSession })
			require.NotEmpty(t, glob)
			for _, r := range glob {
				require.Equal(t, "/console/sessions/"+l.GlobalSession, r.Href)
			}
			globalThreads := 0
			for _, d := range days {
				for _, th := range dyThreads(d.data.Entry) {
					for _, sd := range th.Sessions {
						if sd.Session.ID != l.GlobalSession {
							continue
						}
						globalThreads++
						require.Empty(t, th.Project, "the global session's thread is in no project")
						// Its name is text, the global scope or its working directory: no project page.
						require.Regexp(t, `id="`+dyElementID(th.Key)+`">[\s\S]*?<span class="dy-card-name dy-text" title="(The global scope|Sessions in no project, named by their working directory)">`+
							regexp.QuoteMeta(th.Label)+`</span>`, d.page)
					}
				}
			}
			require.Positive(t, globalThreads, "the global session works a thread of its own")

			// A commit links to the page of what recorded it.
			var byTask, bySession bool
			for _, r := range all(func(r diary.Ref) bool { return r.Kind == diary.RefCommit }) {
				require.NotNil(t, r.Owner)
				switch r.Owner.ID {
				case l.CommitTask:
					require.Equal(t, "/console/tasks/"+l.CommitTask, r.Href)
					byTask = true
				case l.CommitSession:
					require.Equal(t, "/console/sessions/"+l.CommitSession, r.Href)
					bySession = true
				}
			}
			require.True(t, byTask && bySession, "commits recorded by a task and by a session")

			// A self-reported mishap links to its event; an unreported session to its page.
			mishap := all(func(r diary.Ref) bool { return r.Kind == diary.RefEvent && r.ID == l.Mishap })
			require.NotEmpty(t, mishap)
			require.Equal(t, "/console/events/"+l.Mishap, mishap[0].Href)
			unreported := false
			for _, d := range days {
				for _, r := range d.data.Usage.Unreported {
					if r.ID == l.UnreportedSession {
						unreported = true
						require.Equal(t, "/console/sessions/"+l.UnreportedSession, r.Href)
					}
				}
			}
			require.True(t, unreported, "the session from a client without hooks is named unreported")

			// Research: a trial links while it is on, and is nowhere while it is off.
			trialHref := "/console/trials/" + l.Trial
			if research {
				require.Contains(t, where, trialHref)
				return
			}
			for h := range where {
				require.False(t, strings.HasPrefix(h, "/console/trials"), "research is off: %s", h)
			}
			titles := slices.Concat(dyStrings(t, db, `SELECT id FROM trials`), dyStrings(t, db, `SELECT title FROM trials`))
			require.NotEmpty(t, titles, "the fixture recorded trials")
			for _, d := range days {
				key := d.day.Date.Format("2006-01-02")
				require.Empty(t, d.data.Kept.Trials, key)
				main := dyMain(t, d.page)
				for _, s := range titles {
					require.NotContains(t, string(d.raw), s, "%s: the JSON names no trial", key)
					require.NotContains(t, main, s, "%s: the page names no trial", key)
				}
				require.NotContains(t, main, "ico-test-tube", "%s: no trial line", key)
				require.NotContains(t, main, "memories, notes or trials", key)
				for _, th := range dyThreads(d.data.Entry) {
					for _, r := range th.Kept {
						require.NotEqual(t, diary.RefTrial, r.Kind, key)
					}
				}
			}
		})
	}
}

// dyStrings runs a query of one text column.
func dyStrings(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query, args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		require.NoError(t, rows.Scan(&v))
		out = append(out, v)
	}
	require.NoError(t, rows.Err())
	return out
}

// dyEntityPrefix is the route of an entity link (one item's page, which a
// peek can show), "" for any other link.
func dyEntityPrefix(href string) string {
	if strings.Contains(href, "?") {
		return ""
	}
	for _, p := range []string{"/console/tasks/", "/console/sessions/", "/console/memories/", "/console/notes/",
		"/console/trials/", "/console/events/", "/console/plans/", "/console/projects/"} {
		if strings.HasPrefix(href, p) {
			return p
		}
	}
	return ""
}

// dyWithPeek is a link's detail-pane fetch.
func dyWithPeek(href string) string {
	if strings.Contains(href, "?") {
		return href + "&peek=1"
	}
	return href + "?peek=1"
}

// "Sessions started this day (N)": N is the count the Sessions day view
// shows, through the same handle.
func TestDiary_StartedLinkCountsWhatSessionsShows(t *testing.T) {
	_, mux, fx := dySeeded(t, config.Features{Diary: true})
	checked := 0
	for _, d := range fx.Days {
		_, data := dyAnswer(t, mux, dyDayPath(d.Date))
		key := d.Date.Format("2006-01-02")
		var list sessionsData
		getJSON(t, mux, "/console/sessions?day="+key+"&format=json", &list)
		require.Equal(t, data.SessionsStarted, len(list.Sessions), key)
		require.Len(t, d.Sessions, data.SessionsStarted, "%s: the fixture's sessions started that day", key)
		page := dyPage(t, mux, dyDayPath(d.Date))
		if data.SessionsStarted > 0 {
			require.Contains(t, page, `href="/console/sessions?day=`+key+`">`)
			require.Contains(t, page, "Sessions started this day ("+strconv.Itoa(data.SessionsStarted)+")")
			checked++
		} else {
			require.NotContains(t, page, "Sessions started this day")
		}
	}
	require.Greater(t, checked, 10)
}

// The rendered day: the spotlight's cards in the day model's order, every
// short session folded into the collapsed row and nowhere else, the tiles
// and the usage panel, and today's entry live.
func TestDiary_RenderedDay(t *testing.T) {
	_, mux, fx := dySeeded(t, config.Features{Diary: true, Research: true})
	cardID := regexp.MustCompile(`<article class="dy-card[^"]*" id="([^"]+)"`)
	var busiest, short *dyDay
	days := dyAllDays(t, mux, fx)
	// The fixture anchors on the clock: a run that straddles local midnight
	// sees "today" move, and only the today-specific checks need it to hold.
	sameDay := dyMidnight(time.Now()).Equal(fx.Days[0].Date)
	for _, d := range days {
		d := d
		key := d.day.Date.Format("2006-01-02")
		main := dyMain(t, d.page)
		// The cards are the spotlight, in its order.
		var want, got []string
		for _, th := range d.data.Spotlight {
			want = append(want, dyElementID(th.Key))
		}
		for _, m := range cardID.FindAllStringSubmatch(main, -1) {
			got = append(got, m[1])
		}
		require.Equal(t, want, got, "%s: one card per spotlight thread, most significant first", key)
		for _, th := range d.data.Also {
			require.Contains(t, main, `<li class="dy-also-row" id="`+dyElementID(th.Key)+`">`, key)
		}
		// Short sessions fold into one collapsed row, one line each.
		if n := d.data.Short.Count; n > 0 {
			fold := main[strings.Index(main, `<details class="dy-short" id="dy-short">`):]
			fold = fold[:strings.Index(fold, `</details>`)]
			require.Equal(t, n, strings.Count(fold, `<li class="dy-sess">`), key)
			// The work column above the fold holds no line of a short session
			// (the note naming the sessions with no token record may).
			work := main[strings.Index(main, `<div class="dy-spot">`):strings.Index(main, `<details class="dy-short"`)]
			for _, sd := range d.data.Short.Sessions {
				require.NotContains(t, work, sd.Session.Href+`"`, "%s: a short session is in the folded row only", key)
			}
		} else {
			require.NotContains(t, main, `class="dy-short"`, key)
		}
		require.NotContains(t, main, `<details class="dy-short" id="dy-short" open`, "the fold starts closed")
		if d.day.Role == demokit.DiaryToday && sameDay {
			require.Contains(t, main, `data-diary-live="15000"`, "today refreshes itself")
			require.Contains(t, main, "today, so far")
			require.NotContains(t, main, "data-diary-next", "no day after today is offered")
		} else if d.day.Role != demokit.DiaryToday {
			require.NotContains(t, main, "data-diary-live", "%s: a past day does not refresh", key)
		}
		if busiest == nil || len(d.data.Spotlight) > len(busiest.data.Spotlight) {
			busiest = &d
		}
		if d.day.Role == demokit.DiaryShortDay {
			short = &d
		}
	}
	require.NotNil(t, busiest)
	require.GreaterOrEqual(t, len(busiest.data.Spotlight), 2, "a day with a spotlight of several threads")
	main := dyMain(t, busiest.page)
	for _, part := range []string{`class="ov2-vitals dy-tiles"`, `id="diary-rhythm"`, `id="diary-usage"`, `id="diary-closed"`,
		`id="diary-kept"`, `class="chart chart-ring"`, `class="chart chart-mix"`, `class="chart chart-rank"`,
		`class="chart chart-presence"`, `class="chart chart-month"`, "Why this is here", `id="detail-pane"`,
		`class="dy-entry list-detail-split dy-split"`} {
		require.Contains(t, main, part)
	}
	require.Contains(t, busiest.page, `<script src="`+staticLink(t, "diary.js")+`"></script>`)
	// The agents legend, the rhythm's columns and the token mix answer on
	// hover and link nowhere: no page lists "this agent's sessions that day".
	day, err := diary.ParseDay(busiest.data.Day)
	require.NoError(t, err)
	v := buildDiaryView(busiest.data, busiest.data.Month, dyViewContext{now: time.Now(), today: dyMidnight(time.Now()),
		day: day, railMonth: dyMonthStart(day), research: true})
	for name, chart := range map[string]string{"ring": string(v.Ring), "mix": string(v.Mix), "rhythm": string(v.Rhythm)} {
		require.NotEmpty(t, chart, name)
		require.Contains(t, chart, "data-hover=", "%s answers on hover", name)
		require.NotContains(t, chart, "<a ", "%s is not a link", name)
	}

	require.NotNil(t, short, "the fixture has a day of short sessions only")
	main = dyMain(t, short.page)
	require.Empty(t, short.data.Spotlight)
	require.Empty(t, short.data.Also)
	require.Positive(t, short.data.Short.Count)
	require.Contains(t, main, "Only short sessions")
	require.NotContains(t, main, `<article class="dy-card`, "no card for a short session")
}

// A locked project's work is simply absent from every day: read through a
// filtered handle that hides example-ops, no page or answer names it or any
// of its sessions, tasks, memories or notes.
func TestDiary_LockedProjectIsAbsent(t *testing.T) {
	f := dySeed(t)
	db := dyOpen(t, f)
	const hidden = "example-ops"
	var ids []string
	for _, q := range []string{
		`SELECT id FROM sessions WHERE project_slug = ?`, `SELECT id FROM tasks WHERE project_slug = ?`,
		`SELECT id FROM memories_index WHERE project = ?`, `SELECT id FROM notes_index WHERE project = ?`,
	} {
		ids = append(ids, dyStrings(t, db, q, hidden)...)
	}
	require.Greater(t, len(ids), 10, "the hidden project holds sessions, tasks and items")

	view, err := store.OpenView(filepath.Join(f.dir, "seam.db"), []string{hidden})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	mux := dyConsole(t, f, view, db, config.Features{Diary: true, Research: true})
	openMux := dyConsole(t, f, db, db, config.Features{Diary: true, Research: true})
	sawOps := false
	for _, d := range f.fx.Days {
		path := dyDayPath(d.Date)
		raw, _ := dyAnswer(t, mux, path)
		for _, body := range []string{dyMain(t, dyPage(t, mux, path)), string(raw)} {
			require.NotContains(t, body, hidden, "%s names the locked project", path)
			for _, id := range ids {
				require.NotContains(t, body, id, "%s names an item of the locked project", path)
			}
		}
		if strings.Contains(dyPage(t, openMux, path), hidden) {
			sawOps = true
		}
	}
	require.True(t, sawOps, "without the lock the same days show the project: the absence is the view's")
}

// The handler's answers: a malformed day or month is a 400 that names the
// format, a day still to come is an honest empty entry, and the JSON answer
// is the day model plus hrefs, every list a list.
func TestDiary_HandlerAnswers(t *testing.T) {
	_, mux := newConsoleFeatures(t, config.Features{Diary: true})

	for path, want := range map[string]string{
		"/console/diary?day=2026-13-01": `invalid day "2026-13-01": expected YYYY-MM-DD`,
		"/console/diary?day=yesterday":  `invalid day "yesterday": expected YYYY-MM-DD`,
		"/console/diary?m=2026-1":       `invalid m "2026-1": expected YYYY-MM`,
		"/console/diary?m=November":     `invalid m "November": expected YYYY-MM`,
	} {
		rr := getPeek(t, mux, path)
		require.Equal(t, http.StatusBadRequest, rr.Code, path)
		require.Contains(t, html.UnescapeString(rr.Body.String()), want, path)
		req := httptest.NewRequest(http.MethodGet, path+"&format=json", nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr = do(mux, req)
		require.Equal(t, http.StatusBadRequest, rr.Code, path)
		var e map[string]string
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &e))
		require.Equal(t, want, e["error"], path)
	}

	// An empty day is absent: today.
	today := dyMidnight(time.Now()).Format("2006-01-02")
	_, d := dyAnswer(t, mux, "/console/diary?day=")
	require.Equal(t, today, d.Day)
	_, d = dyAnswer(t, mux, "/console/diary")
	require.Equal(t, today, d.Day)
	require.True(t, d.Today)

	// A day still to come: an honest empty entry, never an error.
	future := dyMidnight(time.Now()).AddDate(0, 0, 3)
	page := dyPage(t, mux, dyDayPath(future))
	require.Contains(t, page, "This day has not begun.")
	require.Contains(t, page, "<strong>Still to come.</strong>", "the card does not repeat the headline")
	require.Contains(t, page, `<p class="dy-headline">This day has not begun.</p>`, "with research off too")
	require.NotContains(t, page, `class="dy-tag"`, "a day still to come has no character yet")
	require.NotContains(t, page, `class="ov2-vitals dy-tiles"`)
	raw, d := dyAnswer(t, mux, dyDayPath(future))
	require.True(t, d.Future)
	require.Equal(t, future.Format("2006-01-02"), d.Day)
	require.NotContains(t, string(raw), "null", "every list a list, on a day still to come too")
	// A month still to come names itself and offers no next month.
	nextMonth := dyMonthStart(time.Now()).AddDate(0, 2, 0)
	page = dyPage(t, mux, "/console/diary?m="+nextMonth.Format("2006-01"))
	require.Contains(t, page, "This month has not begun.")

	// The JSON is the day model's entry, plus the rail's month and the usual day.
	raw, _ = dyAnswer(t, mux, "/console/diary")
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &top))
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	require.Equal(t, []string{"also", "character", "day", "end", "facts", "headline", "hours", "kept", "moments",
		"month", "projects", "sessionsStarted", "short", "spotlight", "start", "today", "usage", "usual"}, keys)
	require.NotContains(t, string(raw), "null")
	require.Contains(t, string(top["month"]), `"agents":`)
}

// The quiet day and the first run read as what they are, never as an error.
func TestDiary_EmptyStates(t *testing.T) {
	_, mux := newConsoleFeatures(t, config.Features{Diary: true})
	page := dyMain(t, dyPage(t, mux, "/console/diary"))
	require.Contains(t, page, "This is the Diary's first page.", "a first run says what will appear here")
	require.NotContains(t, page, `class="dy-tag"`, "a first run is not a quiet day: no character stamp")
	require.NotContains(t, page, `class="ov2-vitals dy-tiles"`)

	_, mux, fx := dySeeded(t, config.Features{Diary: true})
	for _, d := range fx.Days {
		if d.Role != demokit.DiaryQuietWeekend {
			continue
		}
		page := dyMain(t, dyPage(t, mux, dyDayPath(d.Date)))
		require.Contains(t, page, "A quiet day.")
		require.Contains(t, page, `title="No session, task or item recorded">Quiet</span>`)
		require.NotContains(t, page, "first page", "a quiet day in a history is not a first run")
	}
	for _, d := range fx.Days {
		if d.Role != demokit.DiaryPreLedger {
			continue
		}
		page := dyMain(t, dyPage(t, mux, dyDayPath(d.Date)))
		require.Contains(t, page, "Token totals are approximate", "a day older than the ledger says its totals are approximate")
		require.Contains(t, page, "not recorded before the usage ledger", "its agent time is unknown, not zero")
		require.NotContains(t, page, `href="#diary-rhythm"`, "no jump to a rhythm the day cannot draw")
	}
}

// The day's agents keep one color across the month: the ring's and the
// rhythm's series classes follow the month's order, not the day's.
func TestDiary_AgentColorsFollowTheMonth(t *testing.T) {
	month := []diary.AgentUsage{
		{Key: "claude-code/a", Minutes: 500}, {Key: "codex/b", Minutes: 300}, {Key: "claude-code/c", Minutes: 100},
	}
	day := []diary.AgentUsage{{Key: "claude-code/c", Minutes: 90}, {Key: "codex/b", Minutes: 10}}
	slots := agentSlots(month, day)
	require.Equal(t, 3, slots["claude-code/c"], "the month's third agent wears the third color, whatever its rank today")
	require.Equal(t, 2, slots["codex/b"])
	require.Equal(t, map[string]int{"claude-code/c": 1, "codex/b": 2}, agentSlots(nil, day),
		"a month that names no agent leaves the day's own order to pick")
	require.Greater(t, agentWeight(diary.AgentUsage{Minutes: 1}), agentWeight(diary.AgentUsage{Tokens: core.TokenUsage{Total: 900_000_000_000}}),
		"agent time ranks before tokens")
}

// The link rules, item by item: built from what the day model resolved,
// empty for what it did not, a trial only while research is on, a plan's
// page only when it has one, else its project's tab, a commit its owner's.
func TestDiary_RefHref(t *testing.T) {
	projects := map[string]bool{"example-api": true, "example-gone": false}
	for _, tc := range []struct {
		name     string
		ref      diary.Ref
		research bool
		want     string
	}{
		{"task", diary.Ref{Kind: diary.RefTask, ID: "01TASK", Resolved: true}, false, "/console/tasks/01TASK"},
		{"a task that is gone", diary.Ref{Kind: diary.RefTask, ID: "01TASK"}, false, ""},
		{"session", diary.Ref{Kind: diary.RefSession, ID: "01SESS", Resolved: true}, false, "/console/sessions/01SESS"},
		{"memory", diary.Ref{Kind: diary.RefMemory, ID: "01MEM", Resolved: true, Status: diary.MemoryArchived}, false,
			"/console/memories/01MEM"},
		{"note", diary.Ref{Kind: diary.RefNote, ID: "01NOTE", Resolved: true}, false, "/console/notes/01NOTE"},
		{"trial, research on", diary.Ref{Kind: diary.RefTrial, ID: "01TR", Resolved: true}, true, "/console/trials/01TR"},
		{"trial, research off", diary.Ref{Kind: diary.RefTrial, ID: "01TR", Resolved: true}, false, ""},
		{"event", diary.Ref{Kind: diary.RefEvent, ID: "01EV", Resolved: true}, false, "/console/events/01EV"},
		{"project", diary.Ref{Kind: diary.RefProject, ID: "example-api", Resolved: true}, false, "/console/projects/example-api"},
		{"a project that is gone", diary.Ref{Kind: diary.RefProject, ID: "example-gone"}, false, ""},
		{"plan with a page", diary.Ref{Kind: diary.RefPlan, ID: "rollout", Project: "example-api", Resolved: true, HasPage: true}, false,
			"/console/plans/rollout"},
		{"task-only plan", diary.Ref{Kind: diary.RefPlan, ID: "rollout", Project: "example-api", Resolved: true}, false,
			"/console/projects/example-api?tab=tasks"},
		{"task-only plan, its project gone", diary.Ref{Kind: diary.RefPlan, ID: "rollout", Project: "example-gone", Resolved: true}, false, ""},
		{"global plan with no page", diary.Ref{Kind: diary.RefPlan, ID: "rollout", Resolved: true}, false, ""},
		{"commit by a task", diary.Ref{Kind: diary.RefCommit, ID: "abc", Resolved: true,
			Owner: &diary.Ref{Kind: diary.RefTask, ID: "01TASK", Resolved: true}}, false, "/console/tasks/01TASK"},
		{"commit by a task that is gone", diary.Ref{Kind: diary.RefCommit, ID: "abc", Resolved: true,
			Owner: &diary.Ref{Kind: diary.RefTask, ID: "01TASK"}}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, refHref(tc.ref, projects, tc.research))
		})
	}

	// What a page shows for each: a link that opens beside the page, the
	// plan's fallback a plain link, and a gone item its name as text.
	link := refLink(diary.Ref{Kind: diary.RefTask, ID: "01ABCDEFGHJKMNPQRSTVWXYZ0", Title: `<b>x</b>`, Resolved: true,
		Href: "/console/tasks/01ABCDEFGHJKMNPQRSTVWXYZ0"}, "")
	require.Equal(t, `<a href="/console/tasks/01ABCDEFGHJKMNPQRSTVWXYZ0" data-peek>&lt;b&gt;x&lt;/b&gt;</a>`, string(link))
	fallback := refLink(diary.Ref{Kind: diary.RefPlan, ID: "rollout", Title: "rollout", Project: "example-api", Resolved: true,
		Href: "/console/projects/example-api?tab=tasks"}, "")
	require.NotContains(t, string(fallback), "data-peek", "the project tab is no peek")
	gone := refLink(diary.Ref{Kind: diary.RefTask, ID: "01ABCDEFGHJKMNPQRSTVWXYZ0"}, "")
	require.Equal(t, `<span class="dy-text dy-gone" title="No longer exists">STVWXYZ0</span>`, string(gone),
		"a task that is gone shows its short id, as text")
	evil := refLink(diary.Ref{Kind: diary.RefTask, ID: "x", Title: "x", Resolved: true, Href: "javascript:alert(1)"}, "")
	require.NotContains(t, string(evil), "javascript:", "an href that is no console path renders as text")
}

// With research off, a trial leaves the entry whole: its kept list, the
// threads' kept lists and their reasons, and the counts that held it.
func TestDiary_WithoutTrials(t *testing.T) {
	trial := diary.Ref{Kind: diary.RefTrial, ID: "01TR", Title: "A trial", Resolved: true}
	memory := diary.Ref{Kind: diary.RefMemory, ID: "01ME", Title: "a-memory", Resolved: true}
	e := dyEmptyEntry(time.Now())
	e.Facts = diary.Facts{Sessions: 1, Kept: 2}
	e.Kept = diary.Kept{Memories: []diary.KeptItem{{Item: memory, New: true}}, Notes: []diary.KeptItem{},
		Trials: []diary.KeptItem{{Item: trial, New: true}}, New: 2}
	e.Spotlight = []diary.Thread{{Key: "project:p", Kind: diary.ThreadProject, Label: "p", Project: "p",
		Kept: []diary.Ref{memory, trial}, Score: 9, Moved: []diary.Move{}, Sessions: []diary.SessionDay{}, Commits: []diary.Ref{},
		Reasons: []diary.Reason{{Kind: diary.ReasonKept, Count: 2, Points: 2}, {Kind: diary.ReasonMinutes, Count: 70, Points: 7}}}}
	withoutTrials(&e)
	require.Empty(t, e.Kept.Trials)
	require.Equal(t, 1, e.Facts.Kept)
	require.Equal(t, 1, e.Kept.New)
	th := e.Spotlight[0]
	require.Equal(t, []diary.Ref{memory}, th.Kept)
	require.Equal(t, []diary.Reason{{Kind: diary.ReasonKept, Count: 1, Points: 1}, {Kind: diary.ReasonMinutes, Count: 70, Points: 7}},
		th.Reasons)
	require.InDelta(t, 8, th.Score, 1e-9, "the thread loses the trial's point")
	require.NotContains(t, strings.ToLower(e.Headline), "trial")
}

// The page script stays on the shared paths: day walking clicks the page's
// own in-place links, today refreshes from the one stream and morphs in
// place, and an open peek survives the morph.
func TestDiaryJS_StaysOnTheSharedPaths(t *testing.T) {
	js := string(diaryJS)
	for _, banned := range []string{"location.reload(", "new EventSource", "location.href ="} {
		require.NotContains(t, js, banned)
	}
	for _, need := range []string{
		"window.SEAM_NO_LIVE_REFRESH = true",         // the layout's per-event refresh stands down
		"document.addEventListener('seam:event'",     // the one stream
		"'X-Seam-Live': '1'",                         // answered with the patch layout
		"window.SeamConsole.patchPage(doc, 'diary')", // the shared morph
		"setAttribute('data-live-skip', '')",         // an open peek is held out of the morph
		"removeAttribute('data-live-skip')",          // and released after it
		"document.addEventListener('seam:content-updated'",
		"document.querySelector('.chord-hint')", // a g-chord's next key is shell.js's
		"follow('[data-diary-prev]')", "follow('[data-diary-next]')", "follow('[data-diary-today]')",
		// A peek keeps the clicked link in place: noted in the capture phase,
		// put back when the split opens.
		"closest('.main .dy-page [data-peek]')", "}, true);",
		"observer.observe(split, { attributes: true, attributeFilter: ['class'] });",
		"lp.scrollTop += r.top - Math.max(lo, Math.min(k.top, hi));",
	} {
		require.Contains(t, js, need)
	}
	mux := newTestMux(t)
	rr := do(mux, httptest.NewRequest(http.MethodGet, "/console/static/diary.js", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Header().Get("Content-Type"), "text/javascript")
	require.Equal(t, js, rr.Body.String())

	// The links the keys click change the day in place.
	_, mux = newConsoleFeatures(t, config.Features{Diary: true})
	page := dyPage(t, mux, dyDayPath(dyMidnight(time.Now()).AddDate(0, 0, -1)))
	require.Regexp(t, `<nav class="dy-nav" aria-label="Days" data-seam-query>\s*<a class="dy-nav-step" href="[^"]+" data-diary-prev`, page)
	require.Contains(t, page, `data-diary-today>Today</a>`)
	require.Contains(t, page, ` data-diary-next aria-label=`)
}

// The rail names a run of quiet days by its oldest and its newest day, however
// long the run, and a day from before the usage ledger shows its agent time
// as not recorded, never as 0m.
func TestDiaryRail_QuietRunsAndDaysBeforeTheLedger(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
	var days []diary.MonthDay
	for d := start; d.Month() == start.Month(); d = d.AddDate(0, 0, 1) {
		md := diary.MonthDay{Day: d.Format("2006-01-02"), Character: diary.Tag{Word: diary.WordQuiet},
			Projects: []diary.ProjectUsage{}}
		if d.Day() == 30 {
			md.Facts = diary.Facts{Sessions: 3, Tokens: 8_900_000, Approximate: true}
			md.Character = diary.Tag{Word: diary.WordLight}
		}
		days = append(days, md)
	}
	month := diary.MonthView{Month: "2026-10", Start: start, End: start.AddDate(0, 1, 0), Days: days, Agents: []diary.AgentUsage{}}
	today := time.Date(2026, 11, 21, 0, 0, 0, 0, time.Local)
	day := time.Date(2026, 10, 30, 0, 0, 0, 0, time.Local)
	rail := buildRail(month, dyViewContext{now: today, today: today, day: day, railMonth: start})
	var labels []string
	for _, r := range rail.Rows {
		labels = append(labels, r.Label)
	}
	require.Equal(t, []string{"Sat 31", "Fri 30", "Thu 1 – Thu 29"}, labels,
		"a quiet run of 29 days reads as its oldest and its newest day")
	require.True(t, rail.Rows[2].Quiet)
	require.Equal(t, "—", rail.Rows[1].Minutes, "agent time before the ledger is not recorded, which is not 0m")
	require.Contains(t, rail.Rows[1].Title, "agent time not recorded before the usage ledger")
	require.True(t, rail.Rows[1].Selected)
}

// The moments name a session by what it was about, as its card does, not by
// its generated name: the commits it recorded, its flag, a mishap it reported.
func TestDiary_MomentsNameSessionsByWhatTheyWereAbout(t *testing.T) {
	_, mux, fx := dySeeded(t, config.Features{Diary: true, Research: true})
	var commits, flags int
	for _, d := range dyAllDays(t, mux, fx) {
		abouts := sessionAbouts(d.data.Entry)
		main := dyMain(t, d.page)
		at := strings.Index(main, `class="dy-panel dy-moments"`)
		if at < 0 {
			continue
		}
		moments := main[at:]
		for _, c := range d.data.Moments.Commits {
			if c.Owner == nil || c.Owner.Kind != diary.RefSession {
				continue
			}
			about := abouts[c.Owner.ID]
			require.NotEmpty(t, about, "%s: the session that recorded the commit is on the day", d.data.Day)
			require.Contains(t, moments, `recorded by <a href="`+c.Owner.Href+`" data-peek title="`+
				html.EscapeString(c.Owner.Title)+`">`+html.EscapeString(about)+`</a>`)
			commits++
		}
		for _, f := range d.data.Moments.Flags {
			about := abouts[f.Session.ID]
			require.NotEmpty(t, about)
			// The flag's own line: its main line, the session then its verdict
			// (the same session's commit line names it too, as its recorder).
			require.Contains(t, moments, `<span class="dy-item-main"><a href="`+f.Session.Href+`" data-peek title="`+
				html.EscapeString(f.Session.Title)+`">`+html.EscapeString(about)+`</a><span class="badge `)
			flags++
		}
	}
	require.Positive(t, commits, "the fixture has a commit a session recorded")
	require.Positive(t, flags, "the fixture has a flagged session")
}

// The day comes before its month in the document: the keyboard and a screen
// reader reach the entry (its h1 first) before the rail's month and its days,
// and a phone, which shows the rail after the entry, tabs in the order it
// shows. The stylesheet draws the rail in the left column on wider screens.
func TestDiary_TheDayComesBeforeItsMonth(t *testing.T) {
	_, mux := newConsoleFeatures(t, config.Features{Diary: true})
	main := dyMain(t, dyPage(t, mux, dyDayPath(dyMidnight(time.Now()).AddDate(0, 0, -1))))
	entry, h1, rail := strings.Index(main, `<section class="dy-entry`), strings.Index(main, `<h1 class="dy-date">`),
		strings.Index(main, `<aside class="dy-rail"`)
	require.Positive(t, entry)
	require.Greater(t, h1, entry)
	require.Greater(t, rail, h1, "the rail follows the entry")
	css := string(consoleCSS)
	require.Contains(t, css, ".dy-rail { grid-column: 1; grid-row: 1; }\n.dy-entry { grid-column: 2; grid-row: 1; }")
	require.NotContains(t, css, "order: 2; max-height: none;", "no visual reordering that the tab order does not follow")
}
