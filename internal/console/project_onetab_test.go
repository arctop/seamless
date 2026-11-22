package console

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// The project page opens on the work and loads one tab at a time
// (plan:console-ux-pass UX 03).

// pvProject registers a project for a workspace test.
func pvProject(t *testing.T, db *sql.DB, slug, name string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, store.CreateProject(context.Background(), db, core.Project{
		ID: mustID(t), Slug: slug, Name: name, CreatedAt: now, UpdatedAt: now,
	}))
}

// pvEvent writes one event row straight into the log and returns its id.
func pvEvent(t *testing.T, db *sql.DB, kind core.EventKind, project, session, item, payload string, at time.Time) string {
	t.Helper()
	id := mustID(t)
	if payload == "" {
		payload = "{}"
	}
	_, err := db.ExecContext(context.Background(), `INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, core.FormatTime(at), string(kind), session, project, item, payload)
	require.NoError(t, err)
	return id
}

// pvTabOrder is the data-tab attributes of the page's tab bar, in order.
func pvTabOrder(t *testing.T, page string) []string {
	t.Helper()
	at := strings.Index(page, `<div class="tabs" id="tabs"`)
	require.GreaterOrEqual(t, at, 0, "no tab bar")
	bar := page[at:]
	bar = bar[:strings.Index(bar, "</div>")]
	var keys []string
	for _, m := range regexp.MustCompile(`data-tab="([a-z]+)"`).FindAllStringSubmatch(bar, -1) {
		keys = append(keys, m[1])
	}
	return keys
}

// pvPill is the count pill on the tab labelled label.
func pvPill(t *testing.T, page, label string) string {
	t.Helper()
	at := strings.Index(page, `<div class="tabs" id="tabs"`)
	require.GreaterOrEqual(t, at, 0, "no tab bar")
	bar := page[at:]
	bar = bar[:strings.Index(bar, "</div>")]
	link := between(t, bar, label, "<a", "</a>")
	m := regexp.MustCompile(`<span class="pill">(\d+)</span>`).FindStringSubmatch(link)
	require.NotNil(t, m, "the %s tab has no pill: %s", label, link)
	return m[1]
}

func TestPvWorkspace_OpensOnPlansAndTasks(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")

	page := getHTMLBody(t, mux, "/console/projects/demo")
	require.Contains(t, page, `<div class="tabpanel on" data-panel="tasks" id="tabpanel-tasks">`, "the page opens on Plans & tasks")
	require.Equal(t, 1, strings.Count(page, `<div class="tabpanel`), "one panel per load")
	require.Equal(t, projectTabKeys, pvTabOrder(t, page), "Plans & tasks first, then Overview")
	require.Equal(t, []string{"tasks", "overview", "sessions", "memories", "notes", "interactions", "context"}, projectTabKeys)
	require.Contains(t, page, `<a data-tab="tasks" href="?tab=tasks" class="on">`)

	// Every tab deep-links: its panel alone, its link marked.
	for _, tab := range projectTabKeys {
		page := getHTMLBody(t, mux, "/console/projects/demo?tab="+tab)
		require.Contains(t, page, `<div class="tabpanel on" data-panel="`+tab+`" id="tabpanel-`+tab+`"`, tab)
		require.Equal(t, 1, strings.Count(page, `<div class="tabpanel`), tab)
		require.Contains(t, page, `<a data-tab="`+tab+`" href="?tab=`+tab+`" class="on">`, tab)
	}

	// ?tab=plans is Plans & tasks, by its own key, the rest of the query kept.
	for alias, key := range map[string]string{"plans": "tasks", "relations": "context"} {
		req := httptest.NewRequest(http.MethodGet, "/console/projects/demo?tab="+alias+"&w=7d", nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr := do(mux, req)
		require.Equal(t, http.StatusPermanentRedirect, rr.Code, alias)
		require.Equal(t, "/console/projects/demo?tab="+key+"&w=7d", rr.Header().Get("Location"), alias)
	}

	// A pending tighten with no tab named opens the card it confirms on.
	page = getHTMLBody(t, mux, "/console/projects/demo?isolate=confidential")
	require.Contains(t, page, `<div class="tabpanel on" data-panel="overview"`)
	require.Contains(t, page, `id="isolation-confirm"`)

	// The refusal names the valid tabs in bar order.
	req := httptest.NewRequest(http.MethodGet, "/console/projects/demo?tab=bogus", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), "valid values are tasks, overview, sessions, memories, notes, interactions, context")
}

// The Plans & tasks tab's answer carries no other tab's rows, and the tab
// pills still count every tab: they come from the board row.
func TestPvWorkspace_TasksTabDrawsNoOtherTabsRows(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	pvProject(t, db, "demo", "Demo")
	memID, noteID, sessID := mustID(t), mustID(t), mustID(t)
	insertRawMemory(t, db, memID, "demo", "", now)
	insertPlanNote(t, db, noteID, "demo", "demo-notes", "Demo notes", `["demo"]`, now)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: sessID, Name: "cc/pv-reader", ProjectSlug: "demo", Status: core.SessionCompleted, CreatedAt: now, UpdatedAt: now,
	}))
	callID := pvEvent(t, db, core.EventToolCall, "demo", sessID, "", `{"tool":"recall"}`, now)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: mustID(t), ProjectSlug: "demo", Title: "pv step", Status: core.TaskOpen, PlanSlug: "pv-plan", CreatedAt: now, UpdatedAt: now,
	}))

	page := getHTMLBody(t, mux, "/console/projects/demo")
	require.Contains(t, page, "pv step", "the work is on the page")
	for name, marker := range map[string]string{
		"memory row":      `href="/console/memories/` + memID + `"`,
		"note row":        `href="/console/notes/` + noteID + `"`,
		"session row":     `href="/console/sessions/` + sessID + `"`,
		"interaction row": `data-id="` + callID + `"`,
		"interactions":    `class="ix-row`,
		"memory table":    `class="mem-name"`,
		"session list":    `<div class="sess">`,
		"overview panel":  `class="metricbar"`,
	} {
		require.NotContains(t, page, marker, "the Plans & tasks answer carries no %s", name)
	}
	require.Equal(t, "1", pvPill(t, page, "Memories"))
	require.Equal(t, "1", pvPill(t, page, "Notes"))
	require.Equal(t, "1", pvPill(t, page, "Sessions"))
	require.Equal(t, "1", pvPill(t, page, "Plans &amp; tasks"))

	// Each of those rows is on its own tab.
	require.Contains(t, getHTMLBody(t, mux, "/console/projects/demo?tab=memories"), `href="/console/memories/`+memID+`"`)
	require.Contains(t, getHTMLBody(t, mux, "/console/projects/demo?tab=notes"), `href="/console/notes/`+noteID+`"`)
	require.Contains(t, getHTMLBody(t, mux, "/console/projects/demo?tab=sessions"), `href="/console/sessions/`+sessID+`"`)
	require.Contains(t, getHTMLBody(t, mux, "/console/projects/demo?tab=interactions"), `data-id="`+callID+`"`)
}

// A tab switch is an in-place load of the tab's link: the bar hands its links
// to the console's shared client, the answer to that load is the patch layout
// holding the one panel, and no page script switches panels or rewrites the
// address itself.
func TestPvWorkspace_TabSwitchStaysInTheDocument(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")

	source, err := templateFS.ReadFile("templates/projectdetail.html")
	require.NoError(t, err)
	tmpl := string(source)
	require.Contains(t, tmpl, `<div class="tabs" id="tabs" data-seam-query>`, "the tab links load in place")
	require.Contains(t, tmpl, `href="?tab={{.Key}}`, "a tab is a real link")
	for _, banned := range []string{"history.replaceState", "history.pushState", "location.reload(", "location.assign(", "location.href ="} {
		require.NotContains(t, tmpl, banned, "the page's own script neither rewrites the address nor navigates")
	}
	require.Contains(t, tmpl, "document.addEventListener('seam:content-updated', mount)",
		"the Interactions panel a switch brings in is mounted then")
	nav := string(navigationJS)
	require.Contains(t, nav, "history.pushState(state, '', nextURL)", "a switch is a history entry")
	require.Contains(t, nav, "window.addEventListener('popstate'", "back and forward walk the tabs in place")

	req := httptest.NewRequest(http.MethodGet, "/console/projects/demo?tab=sessions", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("X-Seam-Partial", "1")
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "no-store", rr.Header().Get("Cache-Control"), "a patch answer is never cached")
	body := rr.Body.String()
	require.Contains(t, body, `<div class="tabpanel on" data-panel="sessions" id="tabpanel-sessions">`)
	require.Equal(t, 1, strings.Count(body, `<div class="tabpanel`))
	require.Contains(t, body, `<a data-tab="sessions" href="?tab=sessions" class="on">`)
	require.NotContains(t, body, "<script", "the patch layout carries the view, not the page's scripts")
}

// A project absent from the fleet's newest events still shows its own: its
// Recent activity, its plan ledger and its Interactions read the project's
// events, not the fleet's tail.
func TestPvWorkspace_QuietProjectShowsItsOwnActivity(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	pvProject(t, db, "quiet", "Quiet")
	pvProject(t, db, "busy", "Busy")
	stepID := mustID(t)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: stepID, ProjectSlug: "quiet", Title: "quiet step", Status: core.TaskOpen, PlanSlug: "quiet-plan", CreatedAt: now, UpdatedAt: now,
	}))
	old := now.Add(-time.Hour)
	written := pvEvent(t, db, core.EventMemoryWritten, "quiet", "", "", `{"name":"quiet-memory"}`, old)
	created := pvEvent(t, db, core.EventTaskTransition, "quiet", "", stepID, `{"to":"open","created":true}`, old)
	call := pvEvent(t, db, core.EventToolCall, "quiet", "", "", `{"tool":"recall"}`, old)

	// The busy project fills every window the panels used to read: the last
	// 120 events, 300 transitions, 200 interactions.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	for i := 0; i < 320; i++ {
		at := core.FormatTime(now.Add(-time.Duration(i) * time.Second))
		for _, kind := range []core.EventKind{core.EventMemoryWritten, core.EventTaskTransition, core.EventToolCall} {
			_, err := tx.ExecContext(ctx, `INSERT INTO events (id, ts, kind, project_slug, item_id, payload) VALUES (?, ?, ?, 'busy', ?, '{"to":"open"}')`,
				mustID(t), at, string(kind), mustID(t))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tx.Commit())
	tail, err := events.NewRecorder(db).RecentExcluding(ctx, 120)
	require.NoError(t, err)
	for _, e := range tail {
		require.NotEqual(t, "quiet", e.ProjectSlug, "the fixture's fleet tail holds none of the quiet project's events")
	}

	overview := tabPanel(t, getHTMLBody(t, mux, "/console/projects/quiet?tab=overview"), "overview")
	require.Contains(t, overview, `href="/console/events/`+written+`"`, "Recent activity lists the project's own event")
	require.NotContains(t, overview, "No recent activity")
	tasks := getHTMLBody(t, mux, "/console/projects/quiet?tab=tasks")
	ledger := tasks[strings.Index(tasks, `<div class="plan-ledger">`):]
	ledger = ledger[:strings.Index(ledger, "</div>")]
	require.Contains(t, ledger, `href="#`+stepID+`"`, "the plan ledger reads the project's transitions")
	require.Contains(t, ledger, "created")
	interactions := tabPanel(t, getHTMLBody(t, mux, "/console/projects/quiet?tab=interactions"), "interactions")
	require.Contains(t, interactions, `data-id="`+call+`"`, "Interactions lists the project's own call")
	require.NotContains(t, interactions, `data-id="`+created+`"`, "a transition is not an interaction")
}

// Under peer_auth the project page's event reads go through the request's
// handle: an open project's events that ran in a locked project's session, or
// are about its items, are absent from Recent activity, the plan ledger and
// Interactions; and a locked project's claimless step never shows on an open
// project's tab, though both sit in plans of one name.
func TestPvWorkspace_LockedProjectRowsAreAbsent(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	at := now.Add(-time.Minute)

	openStep, lockedStep := mustID(t), mustID(t)
	require.NoError(t, store.CreateTask(ctx, lc.db, core.Task{ID: openStep, ProjectSlug: openSlug, Title: "open claimless step",
		Status: core.TaskInProgress, PlanSlug: "shared-plan", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, store.CreateTask(ctx, lc.db, core.Task{ID: lockedStep, ProjectSlug: lockedSlug, Title: "claimless " + lockedSecret,
		Status: core.TaskInProgress, PlanSlug: "shared-plan", CreatedAt: now, UpdatedAt: now}))

	shown := map[string]string{
		"recent":      pvEvent(t, lc.db, core.EventMemoryWritten, openSlug, "", "", "", at),
		"ledger":      pvEvent(t, lc.db, core.EventTaskTransition, openSlug, "", openStep, `{"to":"in_progress","from":"open"}`, at),
		"interaction": pvEvent(t, lc.db, core.EventToolCall, openSlug, "", "", `{"tool":"recall"}`, at),
	}
	hidden := map[string]string{
		"recent by session":      pvEvent(t, lc.db, core.EventMemoryWritten, openSlug, lc.sessID, "", "", now),
		"recent by item":         pvEvent(t, lc.db, core.EventTaskTransition, openSlug, "", lc.taskID, "", now),
		"ledger by session":      pvEvent(t, lc.db, core.EventTaskTransition, openSlug, lc.sessID, openStep, `{"to":"done"}`, now),
		"interaction by session": pvEvent(t, lc.db, core.EventToolCall, openSlug, lc.sessID, "", `{"tool":"recall"}`, now),
		"locked project's own":   pvEvent(t, lc.db, core.EventToolCall, lockedSlug, "", "", `{"tool":"recall"}`, now),
	}

	pages := map[string]string{}
	for _, tab := range []string{"overview", "tasks", "interactions"} {
		code, page := lc.get(t, "/console/projects/"+openSlug+"?tab="+tab, "")
		require.Equal(t, http.StatusOK, code, tab)
		require.NotContains(t, page, lockedSecret, tab)
		for name, id := range hidden {
			require.NotContains(t, page, id, "%s: the %s event is absent", tab, name)
		}
		pages[tab] = page
	}
	require.Contains(t, pages["overview"], `href="/console/events/`+shown["recent"]+`"`)
	require.Contains(t, pages["interactions"], `data-id="`+shown["interaction"]+`"`)
	ledger := pages["tasks"][strings.Index(pages["tasks"], `<div class="plan-ledger">`):]
	ledger = ledger[:strings.Index(ledger, "</div>")]
	require.Contains(t, ledger, "started", "the visible transition, not the hidden newer one")
	require.NotContains(t, ledger, "done")

	// The claimless strip: the open project's step has one, the locked
	// project's step is not on the page at all.
	require.Contains(t, pages["tasks"], `data-open-step="`+openStep+`"`)
	require.NotContains(t, pages["tasks"], lockedStep)

	// The hidden rows are there, on the main handle: the pages leave them
	// out because the request's handle does.
	all, err := events.NewRecorder(lc.db).ProjectEvents(ctx, openSlug, []core.EventKind{core.EventToolCall}, nil, 0)
	require.NoError(t, err)
	require.Len(t, all, 2, "the main handle reads both of the open project's calls")
}

// The Sessions tab counts each state apart, shows the newest 60 and links to
// every one on the Sessions screen, words ended rows by their state (never
// "startup"), and names a held task by title, linked, as a claim.
func TestPvSessionsTab_CountsEachStateAndLinksAll(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	pvProject(t, db, "demo", "Demo")
	mk := func(name string, status core.SessionStatus, source string, updated time.Time) string {
		id := mustID(t)
		require.NoError(t, store.CreateSession(ctx, db, core.Session{
			ID: id, Name: name, ProjectSlug: "demo", Status: status, Source: source, CreatedAt: updated, UpdatedAt: updated,
		}))
		return id
	}
	live := mk("cc/pv-live", core.SessionActive, "startup", now)
	idle := mk("cc/pv-idle", core.SessionActive, "startup", now.Add(-2*time.Hour))
	expired := mk("cc/pv-expired", core.SessionExpired, "startup", now.Add(-time.Minute))
	resumed := mk("cc/pv-resumed", core.SessionCompleted, "resume", now.Add(-2*time.Minute))
	for i := 0; i < 61; i++ {
		mk("cc/pv-done-"+string(rune('a'+i%26))+string(rune('a'+i/26)), core.SessionCompleted, "startup", now.Add(-time.Duration(3+i)*time.Hour))
	}
	held := mustID(t)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: held, ProjectSlug: "demo", Title: "Ship the pv report",
		Status: core.TaskOpen, PlanSlug: "pv-plan", CreatedAt: now, UpdatedAt: now}))
	_, err := store.ClaimTask(ctx, db, held, live, 15*time.Minute, now)
	require.NoError(t, err)

	page := getHTMLBody(t, mux, "/console/projects/demo?tab=sessions")
	panel := tabPanel(t, page, "sessions")
	require.Contains(t, panel, `<span class="h2-meta" data-session-tally>1 live &middot; 1 idle &middot; 62 completed &middot; 1 expired</span>`,
		"expired is not completed, and idle is not live")
	require.Contains(t, panel, `<p class="sess-more">60 of 65 &middot; <a href="/console/sessions?project=demo">all sessions</a></p>`)
	require.Len(t, regexp.MustCompile(`<div id="[0-9A-Z]{26}" class="r">`).FindAllString(panel, -1), 60, "the newest 60 rows")
	require.Equal(t, "65", pvPill(t, page, "Sessions"))

	row := func(id string) string {
		at := strings.Index(panel, `<div id="`+id+`"`)
		require.GreaterOrEqual(t, at, 0, "no row %s", id)
		r := panel[at:]
		return r[:strings.Index(r, `<span class="when">`)]
	}
	require.Contains(t, row(live), `data-session-state="live"`)
	require.Contains(t, row(idle), `data-session-state="idle"`)
	require.Contains(t, row(expired), `data-session-state="expired"`)
	require.Contains(t, row(resumed), `data-session-state="completed"`)
	require.Contains(t, row(resumed), `title="How this session began">resume</span>`, "a source worth saying is said")
	require.NotContains(t, panel, ">startup<", "an ended row is badged by its state, not its start")

	claim := row(live)[strings.Index(row(live), `<span class="claim"`):]
	require.Contains(t, claim, `<a href="/console/tasks/`+held+`">Ship the pv report</a>`, "the held task by title, linked")
	require.Contains(t, claim, "ico-terminal")
	require.NotContains(t, panel, "ico-lock", "a claim is never drawn as a lock")
	require.NotContains(t, row(idle), `class="claim"`)
}

// A step in progress that no live claim holds carries a strip worded in the
// task vocabulary: a lapsed claim offers its release, a claimless step (no
// holder to release) opens the task under its row. A live claim has none.
func TestPvClaimStrip_LapsedAndClaimlessSteps(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	f := rwSeedFleet(t, db, "rw-plan")

	page := getHTMLBody(t, mux, "/console/projects/demo?tab=tasks")
	strip := func(id string) string {
		t.Helper()
		at := strings.Index(page, `<details class="step `)
		require.GreaterOrEqual(t, at, 0)
		at = strings.Index(page, `id="`+id+`">`)
		require.GreaterOrEqual(t, at, 0, "no step %s", id)
		rest := page[at:]
		rest = rest[strings.Index(rest, "</details>")+len("</details>"):]
		if s := strings.TrimLeft(rest, " \n"); !strings.HasPrefix(s, `<div class="step-alert"`) {
			return ""
		}
		return rest[:strings.Index(rest, "</div>")]
	}
	claimless := strip(f.claimless)
	require.Contains(t, claimless, `data-claim-strip="unclaimed"`)
	require.Contains(t, claimless, "<b>unclaimed</b><span>Marked in progress without a claim: any agent can claim it.</span>")
	require.Contains(t, claimless, `<a class="link-act" href="/console/tasks/`+f.claimless+`" data-open-step="`+f.claimless+`"`,
		"it opens the task's peek under its row; without script, the task's page")
	require.NotContains(t, claimless, "<form", "a claimless step names no holder to release from the row")

	lapsed := strip(f.lapsed)
	require.Contains(t, lapsed, `data-claim-strip="claim-lapsed"`)
	require.Contains(t, lapsed, "<b>claim lapsed</b><span>Its holder&#39;s lease ran out: any agent can claim it.</span>")
	require.Contains(t, lapsed, `action="/console/tasks/`+f.lapsed+`/release"`)

	for _, id := range []string{f.held, f.ready, f.blocked, f.done} {
		require.Empty(t, strip(id), "step %s carries no claim strip", id)
	}
	source, err := templateFS.ReadFile("templates/plan_shared.html")
	require.NoError(t, err)
	require.Contains(t, string(source), "closest('[data-open-step]')", "the strip's link opens the row in place (the cards' shared script)")
}

// An error page under /console/projects/<slug> offers the way back to that
// project, and only to a project this request can read.
func TestPvErrorPage_BackToTheProject(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")

	req := httptest.NewRequest(http.MethodGet, "/console/projects/demo?tab=bogus", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), `<a class="btn primary" href="/console/projects/demo" data-error-back>`)
	require.Contains(t, rr.Body.String(), "Back to Demo</a>")

	for _, path := range []string{"/console/projects/nope", "/console/nowhere", "/console/projects/nope?tab=bogus"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr := do(mux, req)
		require.GreaterOrEqual(t, rr.Code, 400, path)
		require.NotContains(t, rr.Body.String(), "data-error-back", "%s: no project to go back to", path)
	}

	lc := newLockedConsole(t)
	back := func(path string) *errorBack {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r = r.WithContext(access.WithView(r.Context(), lc.policy.OwnerView()))
		return lc.cs.errorBackFor(r)
	}
	require.Equal(t, &errorBack{Href: "/console/projects/" + openSlug, Label: openSlug}, back("/console/projects/"+openSlug+"/isolation"))
	require.Nil(t, back("/console/projects/"+lockedSlug+"/isolation"), "a locked project is not offered")
	require.Nil(t, back("/console/projects/"+lockedSlug))
	require.Nil(t, back("/console/projects/"))
	require.Nil(t, back("/console/memories/"+openSlug))
}

// The project peek's counts open the project's own tabs, never the
// unfiltered lists.
func TestPvProjectPeek_LinksToTheProjectsTabs(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	body := getPeek(t, mux, "/console/projects/demo?peek=1").Body.String()
	for _, tab := range []string{"memories", "sessions", "tasks", "notes"} {
		require.Contains(t, body, `href="/console/projects/demo?tab=`+tab+`"`, tab)
	}
	for _, list := range []string{`href="/console/sessions"`, `href="/console/tasks"`, `href="/console/notes"`, `href="/console/memories#`} {
		require.NotContains(t, body, list)
	}
}

// Starring the project returns to the tab it was starred from.
func TestPvWorkspace_StarReturnsToTheTabShown(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	for _, tab := range []string{"tasks", "sessions"} {
		page := getHTMLBody(t, mux, "/console/projects/demo?tab="+tab)
		form := between(t, page, `action="/console/favorites/project/demo"`, "<form", "</form>")
		require.Contains(t, form, `name="next" value="/console/projects/demo?tab=`+tab+`"`, tab)
	}
}
