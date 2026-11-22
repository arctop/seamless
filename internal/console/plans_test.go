package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// seedPlanComposition writes a presented cc-plan note (iteration 3) plus one
// attached agent-cache note under plan:my-plan in project demo.
func seedPlanComposition(t *testing.T, mgr *files.Manager) core.Note {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	planID, err := core.NewID()
	require.NoError(t, err)
	planNote, err := mgr.WriteNote(ctx, core.Note{
		ID: planID, Slug: "cc-plan-clever-stallman", Title: "My Plan", Project: "demo",
		Description: plans.NoteDescription("clever-stallman", 3, plans.StatusPresented),
		Body:        "> captured from cc/abcdef12 | clever-stallman.md | iter 3 | git feedface | " + now.Format(time.RFC3339) + "\n\n# My Plan\n\nDo the thing.",
		Tags:        []string{"plan:my-plan", plans.TagPlan, "plan-status:presented", "created-by:agent"},
		Extra:       map[string]any{"plan_iteration": 3},
		Created:     now, Updated: now,
	})
	require.NoError(t, err)

	agentID, err := core.NewID()
	require.NoError(t, err)
	_, err = mgr.WriteNote(ctx, core.Note{
		ID: agentID, Slug: "cc-agent-abc123", Title: "[Explore] Explore the gardener", Project: "demo",
		Description: "Cached planning-subagent run (Explore) -- prompt + final report",
		Body:        "> captured from cc/abcdef12 | agent abc123 | git feedface | " + now.Format(time.RFC3339) + "\n\n## Prompt\n\nExplore\n\n## Report\n\nDone.",
		Tags:        []string{"plan:my-plan", plans.TagAgent, "agent:Explore", "created-by:agent"},
		Created:     now, Updated: now,
	})
	require.NoError(t, err)
	return planNote
}

func TestPlans_ListAndDetail(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	planNote := seedPlanComposition(t, mgr)

	var list plansData
	getJSON(t, mux, "/console/plans?format=json&w=all", &list)
	require.Equal(t, 1, list.Count)
	row := list.Rows[0]
	require.Equal(t, "my-plan", row.Slug)
	require.Equal(t, "clever-stallman", row.Basename)
	require.Equal(t, "My Plan", row.Title)
	require.Equal(t, "demo", row.Project)
	require.Equal(t, plans.StatusPresented, row.Status)
	require.Equal(t, 3, row.Iteration)
	require.Equal(t, 1, row.Agents)
	require.Equal(t, planNote.ID, row.NoteID)

	var d planDetailData
	getJSON(t, mux, "/console/plans/my-plan?format=json", &d)
	require.Equal(t, "my-plan", d.Row.Slug)
	require.True(t, d.BodyLoaded)
	require.Contains(t, d.BodyText, "Do the thing.")
	require.True(t, d.CanApprove)
	require.Len(t, d.Attached, 1)
	require.True(t, d.Attached[0].IsAgent)
	require.Equal(t, "cc-agent-abc123", d.Attached[0].Slug)

	// Unknown slug -> 404.
	req := httptest.NewRequest(http.MethodGet, "/console/plans/no-such-plan?format=json", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	require.Equal(t, http.StatusNotFound, do(mux, req).Code)
}

func TestPlansPage_PhaseRowsOrderNewestFirst(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	base := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	write := func(slug, title string, updated time.Time, favorite bool) {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		_, err = mgr.WriteNote(context.Background(), core.Note{
			ID: id, Slug: "narrative-" + slug, Title: title, Project: "demo",
			Body: "# " + title, Tags: []string{plans.SlugTag(slug), "created-by:agent"},
			Favorite: favorite, Created: updated, Updated: updated,
		})
		require.NoError(t, err)
	}

	write("old-plan", "Old favorite plan", base, true)
	write("new-plan", "New plan", base.Add(time.Hour), false)

	var data plansData
	getJSON(t, mux, "/console/plans?format=json&w=all", &data)
	require.Len(t, data.Rows, 2)
	require.Equal(t, "new-plan", data.Rows[0].Slug)
	require.Equal(t, "old-plan", data.Rows[1].Slug)
	require.True(t, data.Rows[1].Favorite, "an older star stays marked without jumping ahead of newer activity")
}

// seedComposedPlan writes a plain plans-as-composition plan: a narrative note
// tagged plan:<slug> (no cc-plan capture) plus a supporting note, in project demo.
func seedComposedPlan(t *testing.T, mgr *files.Manager, slug string) core.Note {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	primaryID, err := core.NewID()
	require.NoError(t, err)
	primary, err := mgr.WriteNote(ctx, core.Note{
		ID: primaryID, Slug: "composed-narrative", Title: "Composed Plan", Project: "demo",
		Description: "the composed plan narrative",
		Body:        "# Composed Plan\n\nShip the composed surface.",
		Tags:        []string{plans.SlugTag(slug), "created-by:agent"},
		Created:     now.Add(-time.Hour), Updated: now,
	})
	require.NoError(t, err)

	supportID, err := core.NewID()
	require.NoError(t, err)
	_, err = mgr.WriteNote(ctx, core.Note{
		ID: supportID, Slug: "composed-support", Title: "Supporting Research", Project: "demo",
		Description: "supporting context for the composed plan",
		Body:        "Some supporting findings.",
		Tags:        []string{plans.SlugTag(slug), "created-by:agent"},
		Created:     now, Updated: now,
	})
	require.NoError(t, err)
	return primary
}

func TestPlans_ComposedPlanSurfaces(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	seedPlanComposition(t, mgr)                         // a cc-plan capture (slug my-plan)
	primary := seedComposedPlan(t, mgr, "composed-one") // a composed plan

	var list plansData
	getJSON(t, mux, "/console/plans?format=json&w=all", &list)
	require.Equal(t, 2, list.Count)

	bySlug := map[string]planRow{}
	for _, row := range list.Rows {
		bySlug[row.Slug] = row
	}
	capRow := bySlug["my-plan"]
	require.Equal(t, "capture", capRow.Source)
	require.Equal(t, "clever-stallman", capRow.Basename)

	comp, ok := bySlug["composed-one"]
	require.True(t, ok)
	require.Equal(t, "composed", comp.Source)
	require.Equal(t, "Composed Plan", comp.Title)
	require.Equal(t, primary.ID, comp.NoteID) // earliest-created note is the primary
	require.Empty(t, comp.Basename)
	require.Empty(t, comp.Status)

	// Detail resolves the composed primary and lists the supporting note; the
	// capture-only approve action is withheld.
	var d planDetailData
	getJSON(t, mux, "/console/plans/composed-one?format=json", &d)
	require.Equal(t, primary.ID, d.Row.NoteID)
	require.Equal(t, "composed", d.Row.Source)
	require.False(t, d.CanApprove)
	require.Contains(t, d.BodyText, "Ship the composed surface.")
	require.Len(t, d.Attached, 1)
	require.Equal(t, "composed-support", d.Attached[0].Slug)

	// The escape-hatch POST is refused for a composed plan.
	req := httptest.NewRequest(http.MethodPost, "/console/plans/composed-one/approve?format=json", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	require.Equal(t, http.StatusNotFound, do(mux, req).Code)
}

func TestPlans_ApproveEscapeHatch(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	seedPlanComposition(t, mgr)
	ctx := context.Background()
	rec := events.NewRecorder(db)

	approve := func() map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/console/plans/my-plan/approve?format=json", nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		req.Header.Set("Accept", "application/json")
		rr := do(mux, req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var out map[string]any
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
		return out
	}

	out := approve()
	require.Equal(t, "approved", out["status"])
	require.Equal(t, true, out["taskCreated"])

	// The note flipped and the tracking task exists.
	idx, ok, err := store.NoteBySlug(ctx, db, "demo", "cc-plan-clever-stallman")
	require.NoError(t, err)
	require.True(t, ok)
	require.Contains(t, idx.Tags, "plan-status:approved")
	tasks, err := store.ListTasksForPlan(ctx, db, "demo", "", "my-plan")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, "Implement plan: My Plan", tasks[0].Title)
	require.Equal(t, "console", tasks[0].CreatedBy)

	evs, err := rec.ByKinds(ctx, []core.EventKind{core.EventPlanApproved}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, "console", evs[0].Payload["by"])

	// Re-approval is idempotent: no second task, no second approved event.
	out = approve()
	require.Equal(t, false, out["taskCreated"])
	tasks, err = store.ListTasksForPlan(ctx, db, "demo", "", "my-plan")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	evs, err = rec.ByKinds(ctx, []core.EventKind{core.EventPlanApproved}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, evs, 1)

	// Detail no longer offers the approve action.
	var d planDetailData
	getJSON(t, mux, "/console/plans/my-plan?format=json", &d)
	require.False(t, d.CanApprove)
}

// seedPlanNarrative writes a single composed-plan narrative note tagged
// plan:<slug> in project demo, with a note slug unique to the plan.
func seedPlanNarrative(t *testing.T, mgr *files.Manager, slug string) {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = mgr.WriteNote(context.Background(), core.Note{
		ID: id, Slug: "narrative-" + slug, Title: "Plan " + slug, Project: "demo",
		Description: "narrative for " + slug, Body: "# " + slug,
		Tags:    []string{plans.SlugTag(slug), "created-by:agent"},
		Created: now, Updated: now,
	})
	require.NoError(t, err)
}

// addPlanTask inserts a step task under plan:<slug> in project demo with the
// given status.
func addPlanTask(t *testing.T, db *sql.DB, slug string, status core.TaskStatus) {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateTask(context.Background(), db, core.Task{
		ID: id, ProjectSlug: "demo", Title: "step for " + slug, Status: status,
		CreatedBy: "test", PlanSlug: slug, CreatedAt: now, UpdatedAt: now,
	}))
}

// seedPlanNoteAt writes a note tagged plan:<planSlug> in project demo, stamped
// at ts, so a test can age one part of a composition independently of the rest.
func seedPlanNoteAt(t *testing.T, mgr *files.Manager, noteSlug, planSlug string, ts time.Time) {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	_, err = mgr.WriteNote(context.Background(), core.Note{
		ID: id, Slug: noteSlug, Title: "Plan " + planSlug, Project: "demo",
		Description: "note for " + planSlug, Body: "# " + planSlug,
		Tags:    []string{plans.SlugTag(planSlug), "created-by:agent"},
		Created: ts, Updated: ts,
	})
	require.NoError(t, err)
}

// addPlanTaskAt inserts a step task under plan:<slug> stamped at ts.
func addPlanTaskAt(t *testing.T, db *sql.DB, slug string, status core.TaskStatus, ts time.Time) {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateTask(context.Background(), db, core.Task{
		ID: id, ProjectSlug: "demo", Title: "step for " + slug, Status: status,
		CreatedBy: "test", PlanSlug: slug, CreatedAt: ts, UpdatedAt: ts,
	}))
}

// TestPlans_WindowUsesCompositionActivity pins the Shipped window to the
// whole composition rather than to whichever note or step represents it: a
// shipped plan whose last step closed days ago but whose supporting note
// moved an hour ago is inside 24h. An active plan is never windowed, however
// long nothing moved.
func TestPlans_WindowUsesCompositionActivity(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	old := time.Now().UTC().AddDate(0, 0, -3)
	recent := time.Now().UTC().Add(-time.Hour)

	// Shipped 3 days ago, a supporting note an hour ago: inside the window.
	seedPlanNoteAt(t, mgr, "narrative-live-plan", "live-plan", old)
	seedPlanNoteAt(t, mgr, "support-live-plan", "live-plan", recent)
	addPlanTaskAt(t, db, "live-plan", core.TaskDone, old)
	// Shipped 3 days ago, nothing since: outside.
	seedPlanNoteAt(t, mgr, "narrative-dormant-plan", "dormant-plan", old)
	addPlanTaskAt(t, db, "dormant-plan", core.TaskDone, old)
	// Active, untouched for 3 days: on the board whatever the window.
	seedPlanNoteAt(t, mgr, "narrative-task-plan", "task-plan", old)
	addPlanTaskAt(t, db, "task-plan", core.TaskOpen, old)

	var list plansData
	getJSON(t, mux, "/console/plans?format=json&w=24h", &list)
	slugs := map[string]string{}
	for _, row := range list.Rows {
		slugs[row.Slug] = row.State
	}
	require.Equal(t, planStateShipped, slugs["live-plan"], "a recent supporting note keeps the shipped plan in the 24h window")
	require.NotContains(t, slugs, "dormant-plan", "a plan shipped before the window is folded out of it")
	require.Equal(t, planStateReady, slugs["task-plan"], "an active plan is never windowed")
	require.Equal(t, 2, list.Count)
	require.Equal(t, 3, list.Total, "Total is all-time and never windowed")
	require.Equal(t, 1, list.Done)
	require.Equal(t, 2, list.ShippedAll)
}

// TestPlans_TotalIsUnwindowed: the window narrows the shipped plans alone;
// Total, and every active plan, are the same in any window.
func TestPlans_TotalIsUnwindowed(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	seedPlanNoteAt(t, mgr, "narrative-fresh", "fresh-plan", time.Now().UTC())
	seedPlanNoteAt(t, mgr, "narrative-stale", "stale-plan", time.Now().UTC().AddDate(0, 0, -10))
	addPlanTaskAt(t, db, "stale-plan", core.TaskDone, time.Now().UTC().AddDate(0, 0, -10))

	var win24 plansData
	getJSON(t, mux, "/console/plans?format=json&w=24h", &win24)
	require.Equal(t, 1, win24.Count, "only the active plan is listed inside 24h")
	require.Equal(t, 2, win24.Total)

	var all plansData
	getJSON(t, mux, "/console/plans?format=json&w=all", &all)
	require.Equal(t, 2, all.Count)
	require.Equal(t, 2, all.Total)

	rr := getPeek(t, mux, "/console/plans?w=bogus&format=json")
	require.Equal(t, http.StatusBadRequest, rr.Code, "a window outside the four is a 400, never a silent 24h")
	require.Contains(t, rr.Body.String(), "valid values are 24h, 7d, 30d, all")
}

func TestPlans_PhaseGrouping(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)

	// A plan with an in-progress step (claimless: it needs you), one with only
	// an open step, one whose single step is done, and one with no steps.
	seedPlanNarrative(t, mgr, "wip-plan")
	addPlanTask(t, db, "wip-plan", core.TaskInProgress)
	seedPlanNarrative(t, mgr, "ready-plan")
	addPlanTask(t, db, "ready-plan", core.TaskOpen)
	seedPlanNarrative(t, mgr, "done-plan")
	addPlanTask(t, db, "done-plan", core.TaskDone)
	seedPlanNarrative(t, mgr, "fresh-plan") // no steps -> ready

	var list plansData
	getJSON(t, mux, "/console/plans?format=json&w=all", &list)
	require.Equal(t, 4, list.Count)
	require.Equal(t, 1, list.NeedsYou, "a step marked in progress with no claim needs you")
	require.Equal(t, 2, list.Ready)
	require.Equal(t, 1, list.Done)
	require.Equal(t, 3, list.Active)

	phase, state := map[string]string{}, map[string]string{}
	for _, row := range list.Rows {
		phase[row.Slug], state[row.Slug] = row.Phase, row.State
	}
	require.Equal(t, planPhaseInProgress, phase["wip-plan"], "the JSON's phase keeps its meaning")
	require.Equal(t, planPhaseReady, phase["ready-plan"])
	require.Equal(t, planPhaseDone, phase["done-plan"])
	require.Equal(t, planPhaseReady, phase["fresh-plan"])
	require.Equal(t, map[string]string{"wip-plan": planStateNeeds, "ready-plan": planStateReady,
		"done-plan": planStateShipped, "fresh-plan": planStateReady}, state)
}

func TestPlanPhase(t *testing.T) {
	cases := []struct {
		name string
		row  planRow
		want string
	}{
		{"wip wins over everything", planRow{TasksWIP: 1, TasksOpen: 2, TasksTotal: 3}, planPhaseInProgress},
		{"abandoned capture is done", planRow{Status: plans.StatusAbandoned}, planPhaseDone},
		{"shipped capture is done", planRow{Status: plans.StatusShipped}, planPhaseDone},
		{"all steps closed is done", planRow{TasksDone: 2, TasksTotal: 2}, planPhaseDone},
		{"a dropped step is closed", planRow{TasksDone: 2, TasksDropped: 1, TasksTotal: 2}, planPhaseDone},
		{"open steps remain is ready", planRow{TasksOpen: 1, TasksTotal: 2, TasksDone: 1}, planPhaseReady},
		{"no steps is ready", planRow{}, planPhaseReady},
		{"presented capture with no steps is ready", planRow{Status: plans.StatusPresented}, planPhaseReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, planPhase(tc.row))
		})
	}
}

func TestPlans_HTMLShellRenders(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	seedPlanComposition(t, mgr)

	req := httptest.NewRequest(http.MethodGet, "/console/plans?w=all", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), "My Plan")
	require.Contains(t, rr.Body.String(), "plan:my-plan")

	req = httptest.NewRequest(http.MethodGet, "/console/plans/my-plan", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr = do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), "Do the thing.")
	require.Contains(t, rr.Body.String(), "agent-cache")
}

// TestPlanPeek_FragmentAndLibraryPage covers the HTML shapes of a plan
// detail: the ?peek=1 fragment kept for detail-pane surfaces (search
// results), the ?reader=1 fragment, and the default page, the plan's own
// page with its project named.
func TestPlanPeek_FragmentAndLibraryPage(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	seedPlanComposition(t, mgr)

	// ?peek=1 -> a standalone fragment (no layout) with the plan body + refs.
	frag := getPeek(t, mux, "/console/plans/my-plan?peek=1")
	require.Equal(t, http.StatusOK, frag.Code)
	fb := frag.Body.String()
	require.NotContains(t, fb, "<html", "fragment must not carry the page layout")
	require.Contains(t, fb, "plan:my-plan")
	require.Contains(t, fb, "Do the thing.")   // rendered body
	require.Contains(t, fb, "agent-cache")     // attached agent note ref
	require.Contains(t, fb, "/console/notes/") // note refs are peek links
	require.Contains(t, fb, `action="/console/plans/my-plan/approve?project=demo"`, "its Approve names the project")

	// Default (no peek): the plan's own page in the item frame (UX 32), not
	// a library reader and not the board.
	full := getPeek(t, mux, "/console/plans/my-plan?project=demo")
	require.Equal(t, http.StatusOK, full.Code)
	body := full.Body.String()
	require.Contains(t, body, "<html")
	require.Contains(t, body, `<div class="item-page" data-item="plan" data-item-id="my-plan">`)
	require.NotContains(t, body, `class="lib-page"`)
	require.Contains(t, body, `data-plan-scope data-return="/console/plans/my-plan?project=demo"`)
	require.Contains(t, body, "Do the thing.")
	require.NotContains(t, body, `id="lib-reader"`)
	require.NotContains(t, body, "data-auto-url=")

	frag = getPeek(t, mux, "/console/plans/my-plan?project=demo&reader=1")
	require.Equal(t, http.StatusOK, frag.Code)
	require.NotContains(t, frag.Body.String(), "<html")
	require.Contains(t, frag.Body.String(), "reader-sheet")
	require.Contains(t, frag.Body.String(), "plan:my-plan")

	// The board's card links to the plan's page with its project named.
	list := getPeek(t, mux, "/console/plans?w=all")
	require.Equal(t, http.StatusOK, list.Code)
	require.Contains(t, list.Body.String(), `href="/console/plans/my-plan?project=demo"`)

	// Unknown slug still 404s through the styled path.
	require.Equal(t, http.StatusNotFound, getPeek(t, mux, "/console/plans/no-such-plan?peek=1").Code)
	require.Equal(t, http.StatusNotFound, getPeek(t, mux, "/console/plans/my-plan?project=elsewhere").Code,
		"a plan is looked up in the project named, never in another")
}

// TestPlans_HTMLGroupsInOrder renders the board with a plan in each state
// and asserts the groups appear in the plan note's order: Needs you, In
// progress, Stalled, Ready, then Shipped.
func TestPlans_HTMLGroupsInOrder(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedPlanComposition(t, mgr) // presented: needs you
	seedPlanNarrative(t, mgr, "wip-plan")
	wip := mustID(t)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: wip, ProjectSlug: "demo", Title: "held step",
		Status: core.TaskOpen, PlanSlug: "wip-plan", CreatedAt: now, UpdatedAt: now}))
	_, err := store.ClaimTask(ctx, db, wip, "01SESSIONHOLDER00000000000", 15*time.Minute, now)
	require.NoError(t, err)
	seedPlanNarrative(t, mgr, "quiet-plan")
	addPlanTaskAt(t, db, "quiet-plan", core.TaskOpen, now.AddDate(0, 0, -9))
	seedPlanNarrative(t, mgr, "ready-plan")
	addPlanTask(t, db, "ready-plan", core.TaskOpen)
	seedPlanNarrative(t, mgr, "done-plan")
	addPlanTask(t, db, "done-plan", core.TaskDone)

	body := getHTMLBody(t, mux, "/console/plans")
	at := func(id string) int {
		t.Helper()
		i := strings.Index(body, `id="`+id+`"`)
		require.GreaterOrEqual(t, i, 0, "group %s present", id)
		return i
	}
	order := []int{at("pg-needs"), at("pg-inprogress"), at("pg-stalled"), at("pg-ready"), at("pg-shipped")}
	for i := 1; i < len(order); i++ {
		require.Less(t, order[i-1], order[i], "the groups read in the plan note's order")
	}
	for slug, group := range map[string]string{"my-plan": "pg-needs", "wip-plan": "pg-inprogress",
		"quiet-plan": "pg-stalled", "ready-plan": "pg-ready"} {
		card := strings.Index(body, `id="plan-demo:`+slug+`"`)
		require.Greater(t, card, at(group), "%s is in %s", slug, group)
	}
}

// seedPlanTransition inserts a task.transition event attributing a session to
// the task's plan, the durable link PlanTokenRollups reads.
func seedPlanTransition(t *testing.T, db *sql.DB, sessionID, taskID string) {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES (?, ?, ?, ?, 'demo', ?, '{}')`,
		id, core.FormatTime(time.Now().UTC()), string(core.EventTaskTransition), sessionID, taskID)
	require.NoError(t, err)
}

// TestPlans_TokenRollupSurfaces covers the plan-level model-token attribution
// end to end: a reporting session and a live (unreported) one both moved the
// plan's step, and the rollup lands on the JSON row, the rail meta, the reader
// fact, and the peek fragment.
func TestPlans_TokenRollupSurfaces(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	seedPlanComposition(t, mgr)
	ctx := context.Background()
	now := time.Now().UTC()

	taskID, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: taskID, ProjectSlug: "demo", Title: "step", Status: core.TaskDone,
		PlanSlug: "my-plan", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: "aaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "cc/worked", Status: core.SessionCompleted,
		CreatedAt: now, UpdatedAt: now,
	}))
	_, err = db.ExecContext(ctx,
		`UPDATE sessions SET total_tokens = 483000 WHERE id = ?`, "aaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: "bbbbbbbbbbbbbbbbbbbbbbbbbb", Name: "cc/live", Status: core.SessionActive,
		CreatedAt: now, UpdatedAt: now,
	}))
	seedPlanTransition(t, db, "aaaaaaaaaaaaaaaaaaaaaaaaaa", taskID)
	seedPlanTransition(t, db, "bbbbbbbbbbbbbbbbbbbbbbbbbb", taskID)

	var list plansData
	getJSON(t, mux, "/console/plans?format=json&w=all", &list)
	require.Equal(t, 1, list.Count)
	row := list.Rows[0]
	require.Equal(t, 483000, row.Tokens)
	require.Equal(t, 2, row.TokenSessions)
	require.Equal(t, 1, row.TokenUnreported)
	require.Zero(t, row.TokenShared)

	var d planDetailData
	getJSON(t, mux, "/console/plans/my-plan?format=json", &d)
	require.Equal(t, 483000, d.Row.Tokens)
	require.Equal(t, 2, d.Row.TokenSessions)

	board := getHTMLBody(t, mux, "/console/plans?w=all")
	require.Contains(t, board, "~483k tok (1 unreported)</span>",
		"the card carries the compact total, the live session disclosed, not silently dropped")
	page := getHTMLBody(t, mux, "/console/plans/my-plan?project=demo")
	require.Contains(t, page, "~483k model tokens", "the page's fact carries the qualified total")
	require.Contains(t, page, "(1 unreported)", "the live session is disclosed, not silently dropped")

	frag := getPeek(t, mux, "/console/plans/my-plan?peek=1")
	require.Equal(t, http.StatusOK, frag.Code)
	require.Contains(t, frag.Body.String(), "Model tokens")
	require.Contains(t, frag.Body.String(), "~483k")
}

// TestPlans_NoTokenAttributionRendersNothing pins the absence contract: a plan
// nothing has worked shows no token fact anywhere -- "no attribution" and
// "zero tokens" are different answers.
func TestPlans_NoTokenAttributionRendersNothing(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	seedPlanComposition(t, mgr)

	var list plansData
	getJSON(t, mux, "/console/plans?format=json&w=all", &list)
	require.Zero(t, list.Rows[0].Tokens)
	require.Zero(t, list.Rows[0].TokenSessions)

	for _, path := range []string{"/console/plans?w=all", "/console/plans/my-plan?project=demo"} {
		body := getHTMLBody(t, mux, path)
		require.NotContains(t, body, "model tokens", path)
		require.NotContains(t, body, " tok<", path)
		require.NotContains(t, body, "plan-tok", path)
	}
}

// TestRwPlansReader_StepsWordAndCopy covers the plan's page and its peek:
// each step reads its word and copies its whole id, and the plan copies as
// plan:<slug>, the form an agent is handed. On the page each step is the
// shared step row.
func TestRwPlansReader_StepsWordAndCopy(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	seedComposedPlan(t, mgr, "rw-plan")
	f := rwSeedFleet(t, db, "rw-plan")
	words := map[string]string{f.ready: "ready", f.blocked: "blocked", f.gated: "waiting",
		f.held: "in-progress", f.lapsed: "claim-lapsed", f.done: "done"}
	page := getHTMLBody(t, mux, "/console/plans/rw-plan?project=demo")
	peek := getHTMLBody(t, mux, "/console/plans/rw-plan?peek=1")
	for path, body := range map[string]string{"page": page, "peek": peek} {
		require.Contains(t, body, `data-copy="plan:rw-plan"`, path)
		require.NotContains(t, body, `data-copy="rw-plan"`, "%s: the plan copies with its plan: prefix", path)
		require.NotContains(t, body, ">in_progress<", path)
	}
	for id, word := range words {
		row := page[strings.Index(page, `id="`+id+`">`):]
		row = row[:strings.Index(row, "</summary>")]
		require.Contains(t, row, `data-copy="`+id+`"`, "page: step %s copies its id", id)
		require.Contains(t, row, `data-word="`+word+`"`, "page: step %s reads %s", id, word)

		at := strings.Index(peek, `href="/console/tasks/`+id+`"`)
		require.GreaterOrEqual(t, at, 0)
		near := peek[at:]
		near = near[:strings.Index(near, "</tr>")]
		require.Contains(t, near, `data-copy="`+id+`"`)
		require.Contains(t, near, `data-word="`+word+`"`, "peek: step %s reads %s", id, word)
	}
	held := peek[strings.Index(peek, `href="/console/tasks/`+f.held+`"`):]
	require.Contains(t, held[:strings.Index(held, "</tr>")], "held by "+f.sessName, "the peek names the holder")
	gated := peek[strings.Index(peek, `href="/console/tasks/`+f.gated+`"`):]
	require.Contains(t, gated[:strings.Index(gated, "</tr>")], "waits on owner: sign off", "and the gate a step waits on")

	var d planDetailData
	getJSON(t, mux, "/console/plans/rw-plan?format=json", &d)
	byID := map[string]planTaskRef{}
	for _, st := range d.Tasks {
		byID[st.ID] = st
	}
	require.Equal(t, wordLapsed, byID[f.lapsed].Word.Key)
	require.Equal(t, wordWaiting, byID[f.gate].Word.Key)
	require.Equal(t, f.sessName, byID[f.held].Holder)
	require.Equal(t, []planWaitRef{{Who: "owner", What: "sign off"}}, byID[f.gated].Waits)
}
