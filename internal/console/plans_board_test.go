package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// Plans is one board of plan cards for every project (plan:console-ux-pass
// UX 07): the card the project tab shows, grouped by state (Needs you, In
// progress, Stalled, Ready, Shipped), only Shipped windowed; a project menu
// and a text filter; and a plan's page, steps first in dependency order,
// keyed by project and slug.

// pvbNote writes a note tagged plan:<plan> (and tags) in project, at at.
func pvbNote(t *testing.T, mgr *files.Manager, project, noteSlug, title, plan string, at time.Time, tags ...string) core.Note {
	t.Helper()
	n, err := mgr.WriteNote(context.Background(), core.Note{
		ID: mustID(t), Slug: noteSlug, Title: title, Project: project,
		Description: "about " + title, Body: "# " + title + "\n\nThe narrative of " + title + ".",
		Tags: append([]string{plans.SlugTag(plan), "created-by:agent"}, tags...), Created: at, Updated: at,
	})
	require.NoError(t, err)
	return n
}

// pvbStep creates a step of plan in project at at and returns its id; mut
// adjusts it before it is written.
func pvbStep(t *testing.T, db *sql.DB, project, plan, title string, at time.Time, mut func(*core.Task)) string {
	t.Helper()
	k := core.Task{ID: mustID(t), ProjectSlug: project, Title: title, Status: core.TaskOpen, PlanSlug: plan,
		CreatedBy: "test", CreatedAt: at, UpdatedAt: at}
	if mut != nil {
		mut(&k)
	}
	require.NoError(t, store.CreateTask(context.Background(), db, k))
	return k.ID
}

// pvbCard is a card on the board: from its anchor to the next card or group.
func pvbCard(t *testing.T, page, project, slug string) string {
	t.Helper()
	at := strings.Index(page, `id="`+planAnchor(project, slug)+`"`)
	require.GreaterOrEqual(t, at, 0, "no card for %s/%s", project, slug)
	card := page[at:]
	for _, end := range []string{`<div class="plan-card`, `<section class="plan-group`, `<details class="plan-done-group`} {
		if i := strings.Index(card[1:], end); i >= 0 {
			card = card[:i+1]
		}
	}
	return card
}

// pvbGroupOf is the board group a card sits in.
func pvbGroupOf(t *testing.T, page, project, slug string) string {
	t.Helper()
	at := strings.Index(page, `id="`+planAnchor(project, slug)+`"`)
	require.GreaterOrEqual(t, at, 0, "no card for %s/%s", project, slug)
	open := strings.LastIndex(page[:at], `data-group="`)
	require.GreaterOrEqual(t, open, 0)
	rest := page[open+len(`data-group="`):]
	return rest[:strings.Index(rest, `"`)]
}

// The review fixture's stalled plan: nothing moved for 14 days, a step
// claimable. It is on the board under the default window, under Stalled,
// marked, with its head line and its steps loading when opened.
func TestPvbBoard_StalledPlanIsVisibleWithoutAWindow(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	quiet := time.Now().UTC().AddDate(0, 0, -14).Add(-time.Hour)
	pvbNote(t, mgr, "example-ops", "plan-metrics", "Metrics exporter", "metrics", quiet)
	first := pvbStep(t, db, "example-ops", "metrics", "export the counters", quiet, nil)
	pvbStep(t, db, "example-ops", "metrics", "graph the counters", quiet.Add(time.Second), func(k *core.Task) { k.DependsOn = []string{first} })

	page := getHTMLBody(t, mux, "/console/plans")
	require.Equal(t, planStateStalled, pvbGroupOf(t, page, "example-ops", "metrics"))
	card := pvbCard(t, page, "example-ops", "metrics")
	require.Contains(t, card, `data-stalled="14"`)
	require.Contains(t, card, "stalled 14d")
	require.Contains(t, card, "ready work, nobody on it")
	require.Contains(t, card, "0/2 done")
	require.Contains(t, card, `<details class="plan-lazy"`, "a stalled card loads its steps when opened")
	require.NotContains(t, card, `<details class="step`, "and renders none until then")
	stepsURL := "/console/plans/metrics?project=example-ops&amp;steps=1"
	require.Contains(t, card, `data-plan-steps="`+stepsURL+`"`)

	frag := getPeek(t, mux, strings.ReplaceAll(stepsURL, "&amp;", "&"))
	require.Equal(t, http.StatusOK, frag.Code)
	require.NotContains(t, frag.Body.String(), "<html", "the card's steps, a fragment")
	require.Contains(t, frag.Body.String(), `id="`+first+`">`)
	require.Contains(t, frag.Body.String(), `data-word="blocked"`)

	var data plansData
	getJSON(t, mux, "/console/plans?format=json", &data)
	require.Equal(t, "24h", data.Window)
	require.Len(t, data.Rows, 1)
	require.Equal(t, planStateStalled, data.Rows[0].State)
	require.Equal(t, 14, data.Rows[0].StalledDays)
	require.Equal(t, 1, data.Stalled)
}

// The plan awaiting approval is under Needs you with Approve, which posts
// the escape hatch with its project named and returns to the card on the
// board, filters kept (console-mutation-follow-contract).
func TestPvbBoard_AwaitingApprovalNeedsYouAndApproveKeepsItsPlace(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	now := time.Now().UTC()
	pvbNote(t, mgr, "example-api", plans.NotePrefix+"api-rate-limiting", "API rate limiting", "api-rate-limiting", now,
		plans.TagPlan, "plan-status:presented")
	pvbNote(t, mgr, "example-api", plans.NotePrefix+"api-draft", "API draft", "api-draft", now.Add(-time.Hour),
		plans.TagPlan, "plan-status:draft")

	board := "/console/plans?project=example-api&q=" + url.QueryEscape("rate")
	page := getHTMLBody(t, mux, board)
	require.Equal(t, planStateNeeds, pvbGroupOf(t, page, "example-api", "api-rate-limiting"))
	card := pvbCard(t, page, "example-api", "api-rate-limiting")
	require.Contains(t, card, `data-plan-status="presented">awaiting approval</span>`)
	require.Contains(t, card, `action="/console/plans/api-rate-limiting/approve?project=example-api"`)
	next := "/console/plans?project=example-api&q=rate#" + planAnchor("example-api", "api-rate-limiting")
	require.Contains(t, card, `<input type="hidden" name="next" value="`+strings.ReplaceAll(next, "&", "&amp;")+`">`,
		"Approve lands back on the card, the board's filters kept")
	require.NotContains(t, page, "api-draft", "the text filter keeps the draft out")

	rr := ndPost(mux, "/console/plans/api-rate-limiting/approve?project=example-api", url.Values{"next": {next}}, true)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	require.Equal(t, "/console/plans?project=example-api&q=rate&notice="+url.QueryEscape("Approved plan:api-rate-limiting.")+
		"#"+planAnchor("example-api", "api-rate-limiting"), rr.Header().Get("X-Seam-Location"))

	page = getHTMLBody(t, mux, "/console/plans?project=example-api")
	require.Equal(t, planStateReady, pvbGroupOf(t, page, "example-api", "api-rate-limiting"), "approved: its tracking task is ready")
	require.Equal(t, planStateNeeds, pvbGroupOf(t, page, "example-api", "api-draft"), "a draft the owner can approve needs you too")
}

// A plan's page reads its steps top to bottom in dependency order -- a step
// created first that waits on one created after it reads below it -- each
// with its holder, its blockers and its gates, then the narrative, then the
// attached notes.
func TestPvbPlanPage_StepsInDependencyOrderThenNarrativeThenNotes(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	now := time.Now().UTC()
	_, err := store.EnsureProject(ctx, db, "example-ops", "example-ops")
	require.NoError(t, err)
	pvbNote(t, mgr, "example-ops", "plan-storage", "Storage migration", "storage", now.Add(-time.Hour))
	pvbNote(t, mgr, "example-ops", "storage-research", "Storage research", "storage", now)
	sess := mustID(t)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: sess, Name: "cc/pvb-holder", ProjectSlug: "example-ops",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now}))
	// Written out of order: the cut-over is the oldest step, yet it waits on
	// the two written after it.
	copyData := pvbStep(t, db, "example-ops", "storage", "copy the data", now.Add(-50*time.Minute), nil)
	gate := pvbStep(t, db, "example-ops", "storage", "approve the window", now.Add(-40*time.Minute), func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "pick the window"}
	})
	cutover := pvbStep(t, db, "example-ops", "storage", "cut over to the new store", now.Add(-time.Hour), func(k *core.Task) {
		k.DependsOn = []string{copyData, gate}
	})
	_, err = store.ClaimTask(ctx, db, copyData, sess, 15*time.Minute, now)
	require.NoError(t, err)

	page := getHTMLBody(t, mux, "/console/plans/storage?project=example-ops")
	order := []int{strings.Index(page, `id="`+copyData+`">`), strings.Index(page, `id="`+gate+`">`), strings.Index(page, `id="`+cutover+`">`)}
	for i, at := range order {
		require.GreaterOrEqual(t, at, 0, "step %d renders", i)
	}
	require.Less(t, order[0], order[2], "a blocker reads above what it blocks, whatever the creation order")
	require.Less(t, order[1], order[2])
	row := func(id string) string {
		r := page[strings.Index(page, `id="`+id+`">`):]
		return r[:strings.Index(r, "</summary>")]
	}
	require.Contains(t, row(copyData), "cc/pvb-holder</a>", "the holder, by name")
	require.Contains(t, row(copyData), `data-word="in-progress"`)
	require.Contains(t, row(gate), `data-gate`, "the gate is marked as one")
	require.Contains(t, row(gate), "holds 1 step")
	require.Contains(t, row(cutover), `data-gate-who="owner"`, "the step names the gate it waits on")
	require.Contains(t, row(cutover), `title="blocked by copy the data`, "and its blocker")
	steps, narrative, attached := strings.Index(page, `id="`+copyData+`">`), strings.Index(page, "The narrative of Storage migration."),
		strings.Index(page, "Attached notes")
	require.Less(t, steps, narrative, "steps first, then the narrative")
	require.Less(t, narrative, attached, "then the attached notes")
	require.Contains(t, page, `href="/console/projects/example-ops?tab=tasks#plan-storage"`, "the project chip opens the tab at the plan")

	var d planDetailData
	getJSON(t, mux, "/console/plans/storage?project=example-ops&format=json", &d)
	ids := make([]string, 0, len(d.Tasks))
	for _, st := range d.Tasks {
		ids = append(ids, st.ID)
	}
	require.Equal(t, []string{copyData, gate, cutover}, ids, "the JSON's steps read in the page's order")
	require.Equal(t, planStateNeeds, d.Row.State, "a step waits on a gate: the plan needs you")
}

// The plan with a dropped step reads complete: closed is done plus dropped.
func TestPvbBoard_DroppedStepReadsComplete(t *testing.T) {
	db, mux := newConsole(t)
	now := time.Now().UTC()
	for i, st := range []core.TaskStatus{core.TaskDone, core.TaskDone, core.TaskDone, core.TaskDropped} {
		pvbStep(t, db, "example-ops", "dns", fmt.Sprintf("dns step %d", i), now, func(k *core.Task) { k.Status, k.ClosedAt = st, &now })
	}
	page := getHTMLBody(t, mux, "/console/plans")
	line := page[strings.Index(page, `id="`+planAnchor("example-ops", "dns")+`"`):]
	line = line[:strings.Index(line, "</div>")]
	require.Contains(t, line, `data-plan-state="shipped"`)
	require.Contains(t, line, "4/4 closed: 3 done, 1 dropped", "the plan reads complete, and says what was dropped")

	var data plansData
	getJSON(t, mux, "/console/plans?format=json", &data)
	require.Len(t, data.Rows, 1)
	row := data.Rows[0]
	require.Equal(t, 4, row.TasksDone, "closed is done plus dropped")
	require.Equal(t, 1, row.TasksDropped)
	require.Equal(t, planPhaseDone, row.Phase)
	require.Equal(t, planStateShipped, row.State)
}

// One slug in two projects is two plans: each opens on its own with its own
// steps and notes; without ?project= the page is a chooser (JSON: a 409
// naming both), never a merge; Approve without a project refuses to guess
// between two captures.
func TestPvbPlans_SameSlugInTwoProjectsOpensSeparately(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	now := time.Now().UTC()
	pvbNote(t, mgr, "example-ops", plans.NotePrefix+"shared-ops", "Shared plan in ops", "shared", now, plans.TagPlan, "plan-status:presented")
	opsStep := pvbStep(t, db, "example-ops", "shared", "the ops step", now, nil)
	pvbNote(t, mgr, "example-web", plans.NotePrefix+"shared-web", "Shared plan in web", "shared", now, plans.TagPlan, "plan-status:presented")
	webStep := pvbStep(t, db, "example-web", "shared", "the web step", now, nil)

	ops := getHTMLBody(t, mux, "/console/plans/shared?project=example-ops")
	require.Contains(t, ops, "Shared plan in ops")
	require.Contains(t, ops, `id="`+opsStep+`">`)
	require.NotContains(t, ops, "Shared plan in web", "the other project's note is not this plan's")
	require.NotContains(t, ops, webStep)
	web := getHTMLBody(t, mux, "/console/plans/shared?project=example-web")
	require.Contains(t, web, "Shared plan in web")
	require.NotContains(t, web, "Shared plan in ops")
	require.NotContains(t, web, opsStep)

	chooser := getHTMLBody(t, mux, "/console/plans/shared")
	require.Contains(t, chooser, `data-plan-chooser="shared"`)
	require.Contains(t, chooser, `href="/console/plans/shared?project=example-ops" data-plan-choice="example-ops">Shared plan in ops</a>`)
	require.Contains(t, chooser, `href="/console/plans/shared?project=example-web" data-plan-choice="example-web">Shared plan in web</a>`)
	require.NotContains(t, chooser, opsStep, "a chooser, never a merge")
	peek := getPeek(t, mux, "/console/plans/shared?peek=1")
	require.Equal(t, http.StatusOK, peek.Code)
	require.Contains(t, peek.Body.String(), `data-plan-chooser="shared"`)
	require.NotContains(t, peek.Body.String(), "<html")

	rr := getPeek(t, mux, "/console/plans/shared?format=json")
	require.Equal(t, http.StatusConflict, rr.Code)
	var ambiguous struct {
		Error     string         `json:"error"`
		Ambiguous bool           `json:"ambiguous"`
		Choices   []planChoiceVM `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &ambiguous))
	require.True(t, ambiguous.Ambiguous)
	require.Contains(t, ambiguous.Error, "plan:shared is a plan in 2 projects (example-ops, example-web)")
	require.Contains(t, ambiguous.Error, "seam plan show shared --project <slug>")
	require.Len(t, ambiguous.Choices, 2)

	board := getHTMLBody(t, mux, "/console/plans")
	require.Contains(t, board, `id="plan-example-ops:shared"`)
	require.Contains(t, board, `id="plan-example-web:shared"`, "one card per project, each its own anchor")

	rr = ndPost(mux, "/console/plans/shared/approve", url.Values{}, false)
	require.Equal(t, http.StatusConflict, rr.Code, "two captures of one slug: Approve never guesses")
	require.Contains(t, rr.Body.String(), "captured in 2 projects")
	rr = ndPost(mux, "/console/plans/shared/approve?project=example-web", url.Values{}, false)
	require.Equal(t, http.StatusSeeOther, rr.Code, rr.Body.String())
	require.Equal(t, "/console/plans/shared?project=example-web", rr.Header().Get("Location"),
		"no next: the plan's page, its project named")
	var after plansData
	getJSON(t, mux, "/console/plans?format=json", &after)
	status := map[string]string{}
	for _, r := range after.Rows {
		status[r.Project] = r.Status
	}
	require.Equal(t, map[string]string{"example-ops": plans.StatusPresented, "example-web": plans.StatusApproved}, status,
		"only the named project's capture was approved")
}

// The project switcher (UX 32: the one Memories draws) counts each
// project's plans on the board, the starred ones also under Starred, the
// rest most active first and the global scope last, and ?project= narrows
// the board to one (global names the global scope); a project that is not
// there is a 400.
func TestPvbBoard_ProjectFilterAndMenu(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, p := range []string{"example-api", "example-ops", "example-web"} {
		_, err := store.EnsureProject(ctx, db, p, p)
		require.NoError(t, err)
	}
	_, err := db.ExecContext(ctx, `UPDATE projects SET favorite = 1 WHERE slug = 'example-web'`)
	require.NoError(t, err)
	pvbStep(t, db, "example-ops", "ops-one", "ops one", now, nil)
	pvbStep(t, db, "example-ops", "ops-two", "ops two", now, nil)
	pvbStep(t, db, "example-web", "web-one", "web one", now.Add(-time.Hour), nil)
	pvbStep(t, db, "", "global-one", "global one", now, nil)

	var all plansData
	getJSON(t, mux, "/console/plans?format=json", &all)
	require.Equal(t, []planProjectCount{{Project: "", Plans: 1}, {Project: "example-ops", Plans: 2}, {Project: "example-web", Plans: 1}},
		all.Projects, "every project with a plan on the board, counted; example-api has none")

	page := getHTMLBody(t, mux, "/console/plans")
	require.NotContains(t, page, "data-plan-projects", "the Project menu gave way to the switcher")
	v := scParse(t, page)
	require.Equal(t, "All projects", v.Label)
	require.True(t, v.All.On)
	require.Equal(t, 4, v.All.Count, "All projects counts every plan on the board")
	values := func(rows []scRow) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Value)
		}
		return out
	}
	require.Equal(t, []string{"example-web"}, values(v.Starred), "the starred project, under Starred")
	require.Equal(t, []string{"example-ops", "example-web", "global"}, values(v.Active),
		"most active first, the global scope last; example-api holds no plan")
	counts := map[string]int{}
	for _, r := range v.Active {
		counts[r.Value] = r.Count
		require.True(t, r.Query, "%s patches the board in place", r.Value)
	}
	require.Equal(t, map[string]int{"example-ops": 2, "example-web": 1, "global": 1}, counts)
	require.Equal(t, "global", v.Active[2].Label, "the global scope in the board's own word")
	require.Equal(t, "/console/plans?project=example-ops", v.Active[0].Href)

	var ops plansData
	getJSON(t, mux, "/console/plans?project=example-ops&format=json", &ops)
	require.Len(t, ops.Rows, 2)
	for _, r := range ops.Rows {
		require.Equal(t, "example-ops", r.Project)
	}
	require.Len(t, ops.Projects, 3, "the menu still offers every project")
	var global plansData
	getJSON(t, mux, "/console/plans?project=global&format=json", &global)
	require.Len(t, global.Rows, 1)
	require.Equal(t, "global-one", global.Rows[0].Slug)
	var none plansData
	getJSON(t, mux, "/console/plans?project=example-api&format=json", &none)
	require.Empty(t, none.Rows, "a project with no plan: an empty board, not an error")

	rr := getPeek(t, mux, "/console/plans?project=example-nope&format=json")
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), `invalid project \"example-nope\": no project has that slug`)
	require.Equal(t, http.StatusBadRequest, getPeek(t, mux, "/console/plans?project=a/b").Code)
	require.Equal(t, http.StatusBadRequest, getPeek(t, mux, "/console/plans?project=all").Code)
}

// The window applies to Shipped only: an active plan untouched for a month
// is on the board in every window; a plan shipped ten days ago only from
// 30d, with the shipped fold saying how many shipped before the window.
func TestPvbBoard_WindowAppliesToShippedOnly(t *testing.T) {
	db, mux := newConsole(t)
	month := time.Now().UTC().AddDate(0, 0, -30)
	tenDays := time.Now().UTC().AddDate(0, 0, -10)
	pvbStep(t, db, "example-ops", "slow", "slow work", month, nil)
	pvbStep(t, db, "example-ops", "landed", "landed work", tenDays, func(k *core.Task) { k.Status, k.ClosedAt = core.TaskDone, &tenDays })

	for _, w := range []string{"24h", "7d", "30d", "all"} {
		var data plansData
		getJSON(t, mux, "/console/plans?format=json&w="+w, &data)
		slugs := map[string]bool{}
		for _, r := range data.Rows {
			slugs[r.Slug] = true
		}
		require.True(t, slugs["slow"], "w=%s: an active plan is never windowed", w)
		require.Equal(t, w == "30d" || w == "all", slugs["landed"], "w=%s: the shipped plan follows the window", w)
		require.Equal(t, 1, data.ShippedAll, w)
	}
	page := getHTMLBody(t, mux, "/console/plans?w=7d")
	fold := page[strings.Index(page, `id="pg-shipped"`):]
	fold = fold[:strings.Index(fold, "</details>")]
	require.Contains(t, fold, "Nothing shipped in the last 7d")
	require.Contains(t, fold, "1 more plan shipped before this window.")
	require.Contains(t, fold, `href="/console/plans?w=30d#pg-shipped">30d</a>`, "widen, to the shipped fold")
	require.NotContains(t, fold, `id="`+planAnchor("example-ops", "landed")+`"`)
	require.Contains(t, page, `id="`+planAnchor("example-ops", "slow")+`"`)
}

// The text filter keeps the plans whose title or slug holds it, in any case,
// plan:<slug> included; one that matches nothing says so, with a way back.
func TestPvbBoard_TextFilterOverTitleAndSlug(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	now := time.Now().UTC()
	pvbNote(t, mgr, "example-ops", "plan-tokens", "Design tokens", "theme", now)
	pvbStep(t, db, "example-ops", "theme", "emit the tokens", now, nil)
	pvbStep(t, db, "example-ops", "search-v2", "index the docs", now, nil)

	for q, want := range map[string]string{"TOKENS": "theme", "theme": "theme", "plan:search": "search-v2", "v2": "search-v2"} {
		var data plansData
		getJSON(t, mux, "/console/plans?format=json&q="+url.QueryEscape(q), &data)
		require.Len(t, data.Rows, 1, q)
		require.Equal(t, want, data.Rows[0].Slug, q)
		require.Equal(t, q, data.Filter.Q)
	}
	var stepTitle plansData
	getJSON(t, mux, "/console/plans?format=json&q=emit", &stepTitle)
	require.Empty(t, stepTitle.Rows, "a step's title is not the plan's")
	page := getHTMLBody(t, mux, "/console/plans?q=nothing-matches")
	require.Contains(t, page, "data-plans-none")
	require.Contains(t, page, `<a href="/console/plans" data-seam-query>Show every plan</a>`)
}

// Under peer_auth a locked project's plans are absent from the board, its
// counts and its project menu, and ?project= naming it is a 400 that says it
// is locked; a plan page scoped to it answers the lock.
func TestPvbBoard_LockedProjectIsAbsent(t *testing.T) {
	lc := newLockedConsole(t)
	now := time.Now().UTC()
	pvbStep(t, lc.db, lockedSlug, "locked-plan", "secret step "+lockedSecret, now, nil)
	pvbStep(t, lc.db, openSlug, "open-plan", "open step", now, nil)

	for _, asJSON := range []bool{false, true} {
		req := lc.browser(http.MethodGet, "/console/plans?w=all", "", nil)
		if asJSON {
			req.Header.Set("Accept", "application/json")
		}
		rr := lc.do(req)
		require.Equal(t, http.StatusOK, rr.Code)
		body := rr.Body.String()
		require.NotContains(t, body, lockedSecret)
		require.NotContains(t, body, "locked-plan", "the locked project's plan is absent, its slug included")
		require.Contains(t, body, "open-plan")
		require.NotContains(t, body, `href="/console/plans?project=`+lockedSlug+`"`, "and absent from the menu")
	}
	var data plansData
	getJSONVia(t, lc, "/console/plans?format=json&w=all", &data)
	require.Equal(t, []planProjectCount{{Project: openSlug, Plans: 1}}, data.Projects)
	require.Equal(t, 1, data.Total)
	require.Equal(t, []string{lockedSlug}, data.Locked)

	code, body := lc.get(t, "/console/plans?project="+lockedSlug, "")
	require.Equal(t, http.StatusBadRequest, code)
	require.Contains(t, body, "it is locked")
	require.NotContains(t, body, lockedSecret)

	rr := lc.do(lc.browser(http.MethodGet, "/console/plans/locked-plan?project="+lockedSlug, "", nil))
	require.Equal(t, http.StatusSeeOther, rr.Code, "a plan page in the locked project goes to its unlock page")
	require.Equal(t, "/console/unlock/"+lockedSlug+"?item=plan&next="+url.QueryEscape("/console/plans/locked-plan"), rr.Header().Get("Location"))
	rr = lc.do(lc.browser(http.MethodGet, "/console/plans/locked-plan?project="+openSlug, "", nil))
	require.Equal(t, http.StatusNotFound, rr.Code, "scoped to a project without it, a plain miss")
}

// With 20 active plans of 10 steps each, in every open state, the board
// stays under about 300 KB: Stalled and Ready cards render no step rows.
func TestPvbBoard_PageWeightWithTwentyActivePlans(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	sess := mustID(t)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: sess, Name: "cc/pvb-weight", ProjectSlug: "example-ops",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now}))
	states := map[string]int{}
	for p := range 20 {
		slug := fmt.Sprintf("weight-%02d", p)
		at := now
		if p%4 == 2 {
			at = now.AddDate(0, 0, -12) // stalled
		}
		var prev string
		for i := range 10 {
			id := pvbStep(t, db, "example-ops", slug, fmt.Sprintf("step %d of a plan with a title of ordinary length", i), at.Add(time.Duration(i)*time.Second),
				func(k *core.Task) {
					if prev != "" {
						k.DependsOn = []string{prev}
					}
					if i < 3 {
						k.Status, k.ClosedAt = core.TaskDone, &at
					}
					if p%4 == 0 && i == 5 {
						k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "sign off"}
					}
				})
			if p%4 == 1 && i == 3 {
				_, err := store.ClaimTask(ctx, db, id, sess, 15*time.Minute, now)
				require.NoError(t, err)
			}
			prev = id
		}
	}
	var data plansData
	getJSON(t, mux, "/console/plans?format=json", &data)
	require.Equal(t, 20, data.Active)
	for _, r := range data.Rows {
		states[r.State]++
	}
	require.Equal(t, map[string]int{planStateNeeds: 5, planStateInProgress: 5, planStateStalled: 5, planStateReady: 5}, states)
	page := getHTMLBody(t, mux, "/console/plans")
	t.Logf("board with 20 active plans of 10 steps: %d bytes", len(page))
	require.Less(t, len(page), 300*1024, "the board stays under about 300 KB")
	require.Equal(t, 10, strings.Count(page, `<details class="plan-lazy"`), "the stalled and ready cards load their steps when opened")
}

// A " -- " in a title is shown as a colon (the owner wants neither the double
// hyphen nor an en or em dash on screen); the stored title, and the JSON, are
// untouched.
func TestPvbPlanTitle_DashNeverSplits(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	now := time.Now().UTC()
	pvbNote(t, mgr, "example-web", "plan-css", "Generate CSS vars -- tokens.json", "css-vars", now)
	pvbStep(t, db, "example-web", "css-vars", "emit", now, nil)
	page := getHTMLBody(t, mux, "/console/plans")
	require.Contains(t, page, ">Generate CSS vars: tokens.json</a>")
	require.NotContains(t, page, "vars -- tokens")
	var data plansData
	getJSON(t, mux, "/console/plans?format=json", &data)
	require.Equal(t, "Generate CSS vars -- tokens.json", data.Rows[0].Title)
	require.Equal(t, `a &lt;b&gt;: c`, string(planTitleHTML("a <b> -- c")), "escaped first")
}

// A plan says where it came from in words: captured from plan mode, or from
// tasks. "composed" is the JSON's machine value, never a label.
func TestPvbPlans_SourceInWords(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	now := time.Now().UTC()
	pvbNote(t, mgr, "example-ops", plans.NotePrefix+"cap", "A capture", "cap", now, plans.TagPlan, "plan-status:approved")
	pvbStep(t, db, "example-ops", "steps-only", "a step", now, nil)
	for path, want := range map[string]string{
		"/console/plans/cap?project=example-ops":        "captured from plan mode &middot; <span class=\"mono\">cap.md</span>",
		"/console/plans/steps-only?project=example-ops": ">from tasks</span>",
		"/console/plans/cap?peek=1":                     "<dd>captured from plan mode",
		"/console/plans/steps-only?peek=1":              "<dd>from tasks</dd>",
	} {
		body := getHTMLBody(t, mux, path)
		require.Contains(t, body, want, path)
		require.NotContains(t, body, ">composed<", path)
	}
}

// The step dossier's actions return to the row they were taken on: the
// script builds the reader's next from the page's own URL (a scope's
// data-return; the project tab's Plans & tasks) and the row's id.
func TestPvbDossierReturnsToItsRow(t *testing.T) {
	shared, err := templateFS.ReadFile("templates/plan_shared.html")
	require.NoError(t, err)
	js := string(shared)
	require.Contains(t, js, `var row = box.closest('details.step');`)
	require.Contains(t, js, `(scope && scope.getAttribute('data-return')) || (location.pathname + '?tab=tasks')`)
	require.Contains(t, js, `encodeURIComponent(back + (row && row.id ? '#' + row.id : ''))`)
	require.NotContains(t, js, `encodeURIComponent(location.pathname + '?tab=tasks')`, "no longer the top of the tab")
	for _, page := range []string{"templates/projectdetail.html", "templates/plans.html"} {
		src, err := templateFS.ReadFile(page)
		require.NoError(t, err)
		require.Contains(t, string(src), `{{template "plan-steps-js"}}`, page)
	}
	plansPage, err := templateFS.ReadFile("templates/plans.html")
	require.NoError(t, err)
	require.Contains(t, string(plansPage), `data-plan-scope data-return="{{.Return}}"`)
}

// stepOrder puts a blocker above what it blocks, creation order breaking
// ties, and survives a cycle.
func TestPvbGroupSteps_DependencyOrder(t *testing.T) {
	at := func(min int) time.Time { return time.Date(2026, 11, 21, 9, min, 0, 0, time.UTC) }
	step := func(id string, min int, deps ...string) planStepVM {
		return planStepVM{ID: id, Title: id, State: "open", Created: at(min), deps: deps}
	}
	ids := func(steps []planStepVM) []string {
		out := make([]string, len(steps))
		for i, st := range steps {
			out[i] = st.ID
		}
		return out
	}
	require.Equal(t, []string{"B", "A", "C"}, ids(stepOrder([]planStepVM{step("A", 1, "B"), step("B", 2), step("C", 3)})),
		"A waits on B, written after it")
	require.Equal(t, []string{"A", "B", "C"}, ids(stepOrder([]planStepVM{step("C", 3), step("B", 2), step("A", 1)})),
		"no edges: creation order")
	require.Equal(t, []string{"C", "A", "B"}, ids(stepOrder([]planStepVM{step("A", 1, "B"), step("B", 2, "A"), step("C", 3)})),
		"a cycle: the free step first, then the oldest of the cycle")
	require.Equal(t, []string{"A", "B"}, ids(stepOrder([]planStepVM{step("A", 1, "A", "closed-elsewhere"), step("B", 2)})),
		"a self edge or a blocker off the frontier holds nothing")
	groups := groupSteps([]planStepVM{step("A", 1, "B"), step("B", 2)})
	require.Equal(t, "B", groups[0].Steps[0].ID, "groupSteps uses it")
}

// Every plan link the console renders names its project (planHref,
// planApproveHref): one slug is a plan per project, so a bare
// /console/plans/<slug> sends a shared slug to the chooser, or Approve to a
// 409. The one bare link left is Approve's own fallback redirect, kept
// byte for byte for a request that named no project.
func TestPvbEveryPlanLinkNamesItsProject(t *testing.T) {
	bare := regexp.MustCompile(`"/console/plans/"\s*\+|href="/console/plans/\{\{`)
	goFiles, err := filepath.Glob("*.go")
	require.NoError(t, err)
	tmpls, err := filepath.Glob(filepath.Join("templates", "*.html"))
	require.NoError(t, err)
	for _, f := range append(goFiles, tmpls...) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		for i, line := range strings.Split(string(src), "\n") {
			if !bare.MatchString(line) || strings.Contains(line, "project=") || strings.Contains(line, "fallback := ") {
				continue
			}
			t.Errorf("%s:%d links a plan by its slug alone; use planHref: %s", f, i+1, strings.TrimSpace(line))
		}
	}
}
