package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// The plan card tells the truth (plan:console-ux-pass UX 05): titled by its
// narrative note, plan:<slug> to copy, the status in words, the steps counted
// by their words, gates and the steps they hold as waiting, a stalled mark,
// captured plans with no steps yet as cards with Approve, and a plan with
// steps and no note opening as a page.

// pcStep creates a step of plan in project demo and returns its id; mut
// adjusts it before it is written.
func pcStep(t *testing.T, db *sql.DB, plan, title string, mut func(*core.Task)) string {
	t.Helper()
	return opTask(t, db, "demo", title, func(k *core.Task) {
		k.PlanSlug = plan
		if mut != nil {
			mut(k)
		}
	})
}

// pcCard is a plan's card on the page: from its anchor to the next card.
func pcCard(t *testing.T, page, slug string) string {
	t.Helper()
	at := strings.Index(page, `id="plan-`+slug+`"`)
	require.GreaterOrEqual(t, at, 0, "no card for plan %s", slug)
	card := page[at:]
	if end := strings.Index(card[1:], `<div class="plan-card`); end >= 0 {
		card = card[:end+1]
	}
	return card
}

// pcHead is a card's head: the plan-head holds no div of its own.
func pcHead(t *testing.T, card string) string {
	t.Helper()
	at := strings.Index(card, `<div class="plan-head">`)
	require.GreaterOrEqual(t, at, 0)
	head := card[at:]
	return head[:strings.Index(head, "</div>")]
}

// pcSummary is the text of a card's head line, tags stripped.
func pcSummary(t *testing.T, card string) string {
	t.Helper()
	head := pcHead(t, card)
	at := strings.Index(head, `data-plan-summary>`)
	require.GreaterOrEqual(t, at, 0)
	line := head[at+len(`data-plan-summary>`):]
	line = line[:strings.LastIndex(line, "</span>")]
	var b strings.Builder
	in := false
	for _, r := range line {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return strings.ReplaceAll(b.String(), "&middot;", "·")
}

// A step a gate holds is waiting, names who must act and what must happen,
// and is never claimable; the gate itself is a gate row that says so and
// what it holds. A step with its own open gate waits the same way.
func TestPcGatedStepWaitsAndIsNotClaimable(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	gate := pcStep(t, db, "pc-gated", "pc owner approves the window", func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "approve the window"}
	})
	behind := pcStep(t, db, "pc-gated", "pc run the migration", func(k *core.Task) { k.DependsOn = []string{gate} })
	own := pcStep(t, db, "pc-gated", "pc flash the boards", func(k *core.Task) {
		k.Gate = core.TaskGate{Who: "hardware", What: "boards arrive"}
	})
	free := pcStep(t, db, "pc-gated", "pc write the notes", nil)

	page := getHTMLBody(t, mux, "/console/projects/demo?tab=tasks")
	require.Contains(t, page, `id="plan-pc-gated" data-gates-open="2">`, "the rollup's open gates: the gate and the step its own gate holds")
	card := pcCard(t, page, "pc-gated")
	require.Contains(t, card, `<details class="step waiting" id="`+behind+`">`, "a step a gate holds is waiting, not ready")
	row := opRow(t, card, behind)
	require.Contains(t, row, `<span class="edge" data-gate-who="owner" title="waits on owner: approve the window">`)
	require.Contains(t, row, "waits on owner: approve the window</span>", "who and what")
	require.Contains(t, row, `<span class="edge-t">waits on owner: approve the window</span></span>`,
		"the words sit in their own span: a flex chip's own text never shows an ellipsis")
	require.Contains(t, row, `data-word="waiting"`)

	require.Contains(t, card, `<details class="step waiting" id="`+own+`">`, "its own open gate holds it")
	require.Contains(t, opRow(t, card, own), "waits on hardware: boards arrive</span>")
	require.NotContains(t, card, `class="step open ready" id="`+own+`"`, "a step with an open gate is not claimable")

	require.Contains(t, card, `<details class="step waiting gate" id="`+gate+`">`, "the gate is a gate row")
	gateRow := opRow(t, card, gate)
	label := strings.Index(gateRow, `<span class="gate-kind" data-gate`)
	waits := strings.Index(gateRow, "waits on owner: approve the window</span>")
	holds := strings.Index(gateRow, "holds 1 step</span>")
	require.True(t, label >= 0 && waits > label && holds > waits,
		"the row reads Gate, then waits on owner: approve the window, then holds 1 step:\n%s", gateRow)
	require.Contains(t, gateRow, `title="holds pc run the migration"`)
	require.Empty(t, opStrip(t, card, gate), "nobody claims a gate")

	require.Contains(t, card, `<details class="step open ready" id="`+free+`">`, "the free step is the plan's one claimable step")
	require.Contains(t, pcHead(t, card), `<b class="ready-n">1 ready</b>`)
}

// A plan gate holds every unfinished task that depends on it, wherever it
// is, in the plan's word: steps.
func TestPcGateRowCountsWhatItHolds(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	pvProject(t, db, "other", "Other")
	gate := pcStep(t, db, "pc-holds", "pc sign-off", func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "sign the release"}
	})
	on := func(k *core.Task) { k.DependsOn = []string{gate} }
	pcStep(t, db, "pc-holds", "pc cut the release", on)
	pcStep(t, db, "pc-holds", "pc tag the release", on)
	opTask(t, db, "other", "pc mirror the release", on)
	pcStep(t, db, "pc-holds", "pc dropped idea", func(k *core.Task) {
		now := time.Now().UTC()
		k.DependsOn, k.Status, k.ClosedAt = []string{gate}, core.TaskDropped, &now
	})

	page := getHTMLBody(t, mux, "/console/projects/demo?tab=tasks")
	row := opRow(t, pcCard(t, page, "pc-holds"), gate)
	require.Contains(t, row, "holds 3 steps</span>", "the other project's dependent counts; the dropped one does not")
	require.Contains(t, row, `title="holds pc cut the release · pc tag the release · pc mirror the release"`)
	require.NotContains(t, row, "</svg>unblocks", "a gate holds; it does not unblock")
}

// The head counts each unfinished step once, by its word: held, blocked, and
// waiting by who, so a plan whose last step is blocked says so.
func TestPcHeadCountsBlockedAndWaiting(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	sess := opSession(t, db, "demo", "cc/pc-worker")
	closed := func(k *core.Task) {
		now := time.Now().UTC()
		k.Status, k.ClosedAt = core.TaskDone, &now
	}
	pcStep(t, db, "pc-head", "pc one", closed)
	pcStep(t, db, "pc-head", "pc two", closed)
	held := pcStep(t, db, "pc-head", "pc three", nil)
	_, err := store.ClaimTask(context.Background(), db, held, sess, 15*time.Minute, time.Now().UTC())
	require.NoError(t, err)
	pcStep(t, db, "pc-head", "pc four", func(k *core.Task) { k.DependsOn = []string{held} })
	gate := pcStep(t, db, "pc-head", "pc owner gate", func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "pick a vendor"}
	})
	pcStep(t, db, "pc-head", "pc five", func(k *core.Task) { k.DependsOn = []string{gate} })
	pcStep(t, db, "pc-head", "pc six", func(k *core.Task) { k.Gate = core.TaskGate{Who: "hardware", What: "boards arrive"} })

	card := pcCard(t, getHTMLBody(t, mux, "/console/projects/demo?tab=tasks"), "pc-head")
	summary := pcSummary(t, card)
	require.True(t, strings.HasPrefix(summary,
		"2/7 done · 1 in progress · 1 blocked · 1 waiting on hardware · 2 waiting on owner · moved "), summary)
	head := pcHead(t, card)
	require.Contains(t, head, `<span data-count="blocked">1 blocked</span>`)
	require.Contains(t, head, `<span data-count="waiting">2 waiting on owner</span>`)
	require.NotContains(t, head, "in flight", "the status count is gone: the words say what is true")
	require.NotContains(t, head, "ready-n", "nothing is claimable, so the head offers no ready count")
}

// planCounts words every unfinished row once, held first and people last,
// whatever order the rows come in.
func TestPcPlanCountsOrder(t *testing.T) {
	row := func(w taskWord) planStepVM { return planStepVM{Word: w} }
	got := planCounts([]planStepVM{
		row(waitingWord([]string{"owner"})),
		row(wordFor(taskFacts{Status: string(core.TaskOpen), Blocked: 1})),
		row(wordFor(taskFacts{Status: string(core.TaskDone)})),
		row(wordFor(taskFacts{Status: string(core.TaskOpen)})),
		row(wordFor(taskFacts{Status: string(core.TaskInProgress), Holder: true})),
		row(wordFor(taskFacts{Status: string(core.TaskInProgress), Holder: true, Live: true})),
		row(waitingWord([]string{"owner"})),
		row(wordFor(taskFacts{Status: string(core.TaskInProgress)})),
		row(wordFor(taskFacts{Status: string(core.TaskDropped)})),
		row(waitingWord([]string{"hardware"})),
	})
	require.Equal(t, []planCountVM{
		{Key: wordInProgress, Text: "1 in progress"},
		{Key: wordLapsed, Text: "1 claim lapsed"},
		{Key: wordUnclaimed, Text: "1 unclaimed"},
		{Key: wordReady, Text: "1 ready"},
		{Key: wordBlocked, Text: "1 blocked"},
		{Key: wordWaiting, Text: "1 waiting on hardware"},
		{Key: wordWaiting, Text: "2 waiting on owner"},
	}, got)
	require.Empty(t, planCounts(nil))
}

// Stalled is open steps, none held, nothing moved for longer than seven
// days, as of a fixed clock.
func TestPcPlanStalledRule(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		last       time.Time
		open, held int
		stalled    bool
		days       int
	}{
		{"ten days, nobody on it", now.Add(-10 * 24 * time.Hour), 2, 0, true, 10},
		{"exactly seven days is still moving", now.Add(-planStalledAfter), 2, 0, false, 0},
		{"just past seven days", now.Add(-planStalledAfter - time.Second), 2, 0, true, 7},
		{"moved yesterday", now.Add(-24 * time.Hour), 2, 0, false, 0},
		{"a live claim holds a step", now.Add(-10 * 24 * time.Hour), 2, 1, false, 0},
		{"no open step", now.Add(-30 * 24 * time.Hour), 0, 0, false, 0},
		{"no activity known", time.Time{}, 2, 0, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stalled, days := planStalled(tc.last, now, tc.open, tc.held)
			require.Equal(t, tc.stalled, stalled)
			require.Equal(t, tc.days, days)
		})
	}
}

// The tab's model on a fixed clock: a plan with ready work nothing touched
// for ten days is stalled with a claimable step; the same plan a moment
// inside the seven days is not.
func TestPcStalledOnAFixedClock(t *testing.T) {
	db, _ := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	svc, err := New(Config{DB: db, APIKey: testKey})
	require.NoError(t, err)
	ctx := context.Background()
	moved := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	at := func(k *core.Task) { k.CreatedAt, k.UpdatedAt = moved, moved }
	first := pcStep(t, db, "pc-quiet", "pc measure", func(k *core.Task) {
		at(k)
		k.Status, k.ClosedAt = core.TaskDone, &moved
	})
	pcStep(t, db, "pc-quiet", "pc apply", func(k *core.Task) { at(k); k.DependsOn = []string{first} })

	fill := func(now time.Time) planTimelineVM {
		t.Helper()
		data := projectWorkspaceData{Slug: "demo"}
		require.NoError(t, svc.fillTasksTab(ctx, &data, "demo", now))
		require.Len(t, data.Plans, 1)
		return data.Plans[0]
	}
	tl := fill(moved.Add(10 * 24 * time.Hour))
	require.True(t, tl.Stalled)
	require.Equal(t, 10, tl.StalledDays)
	require.Equal(t, 1, tl.Claimable, "ready work, nobody on it")
	require.False(t, fill(moved.Add(planStalledAfter)).Stalled, "seven days to the instant still counts as moving")
}

// On the page: "stalled 10d" with "ready work, nobody on it" when a step is
// claimable; a stalled plan whose open steps all wait says only stalled; a
// plan that moved this week is not marked.
func TestPcStalledPlanIsMarked(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	old := time.Now().UTC().Add(-10*24*time.Hour - time.Hour)
	quiet := func(k *core.Task) { k.CreatedAt, k.UpdatedAt = old, old }
	pcStep(t, db, "pc-stalled", "pc apply the rules", quiet)
	gate := pcStep(t, db, "pc-stuck", "pc owner signs", func(k *core.Task) {
		quiet(k)
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "sign"}
	})
	pcStep(t, db, "pc-stuck", "pc ship", func(k *core.Task) { quiet(k); k.DependsOn = []string{gate} })
	pcStep(t, db, "pc-moving", "pc fresh work", nil)

	page := getHTMLBody(t, mux, "/console/projects/demo?tab=tasks")
	card := pcCard(t, page, "pc-stalled")
	require.Contains(t, card, `id="plan-pc-stalled" data-stalled="10"`)
	head := pcHead(t, card)
	require.Contains(t, head, `<span class="kind warn" data-stalled title="Open steps, none in progress, and nothing moved for 10 days">stalled 10d</span>`)
	require.Contains(t, head, `<span class="plan-stalled-why">ready work, nobody on it</span>`)

	stuck := pcHead(t, pcCard(t, page, "pc-stuck"))
	require.Contains(t, stuck, "stalled 10d")
	require.NotContains(t, stuck, "ready work", "nothing in it is claimable")

	require.NotContains(t, pcHead(t, pcCard(t, page, "pc-moving")), "stalled")
}

// The head: the narrative note's title, plan:<slug> to copy, the status in
// words and a narrative link; without a note, the slug and nothing empty.
func TestPcHeadTitleSlugStatusNarrative(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	noteID := mustID(t)
	insertPlanNote(t, db, noteID, "demo", "cc-plan-pc-webhooks", "Replay-safe webhook ingest",
		`["cc-plan","plan:pc-webhooks","plan-status:approved"]`, time.Now().UTC())
	pcStep(t, db, "pc-webhooks", "pc dedupe on delivery id", nil)
	pcStep(t, db, "pc-bare", "pc a step with no narrative", nil)

	page := getHTMLBody(t, mux, "/console/projects/demo?tab=tasks")
	head := pcHead(t, pcCard(t, page, "pc-webhooks"))
	require.Contains(t, head, `<a class="nm titled" href="/console/plans/pc-webhooks?project=demo">Replay-safe webhook ingest</a>`, "titled by its note")
	require.Contains(t, head, `plan:pc-webhooks<button type="button" class="copy-btn" data-copy="plan:pc-webhooks"`, "the slug chip copies")
	require.Contains(t, head, `<span class="kind ok" data-plan-status="approved">approved</span>`)
	require.Contains(t, head, `<a class="plan-narrative" href="/console/notes/`+noteID+`"`)
	require.Contains(t, head, `action="/console/favorites/plan/pc-webhooks?project=demo"`)
	require.NotContains(t, head, "plan-approve", "an approved plan has nothing to approve")

	bare := pcHead(t, pcCard(t, page, "pc-bare"))
	require.Contains(t, bare, `<a class="nm" href="/console/plans/pc-bare?project=demo">pc-bare</a>`, "no note: the slug is the title")
	require.Contains(t, bare, `data-copy="plan:pc-bare"`)
	for _, absent := range []string{"data-plan-status", "plan-narrative", "star-form", "plan-approve"} {
		require.NotContains(t, bare, absent, "no note, so no %s", absent)
	}
}

// A captured plan with no steps yet is a card: its status in words and, while
// it awaits approval, Approve, posting the existing route with a next that
// lands back on this card. Approved and not started it is a card with no
// Approve; settled, or a composed note with no steps, it is no card.
func TestPcNoteOnlyPlansAreCards(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	now := time.Now().UTC()
	capture := func(slug, title, status string, at time.Time) {
		insertPlanNote(t, db, mustID(t), "demo", "cc-plan-"+slug, title,
			`["cc-plan","plan:`+slug+`","plan-status:`+status+`"]`, at)
	}
	capture("pc-await", "Rate limits for the public API", plans.StatusPresented, now.Add(-time.Hour))
	capture("pc-draft", "Draft of the export", plans.StatusDraft, now.Add(-2*time.Hour))
	capture("pc-approved", "Approved, not started", plans.StatusApproved, now.Add(-3*time.Hour))
	capture("pc-gone", "Given up", plans.StatusAbandoned, now)
	insertPlanNote(t, db, mustID(t), "demo", "pc-composed", "A composed narrative", `["plan:pc-composed"]`, now)
	// A presented capture with steps: a plan card with rows, and Approve.
	capture("pc-active", "Already has steps", plans.StatusPresented, now)
	pcStep(t, db, "pc-active", "pc implement", nil)

	page := getHTMLBody(t, mux, "/console/projects/demo?tab=tasks")
	panel := tabPanel(t, page, "tasks")
	require.NotContains(t, panel, "No active plans")

	card := pcCard(t, panel, "pc-await")
	require.Contains(t, panel, `<div class="plan-card plan-noteonly" id="plan-pc-await">`)
	head := pcHead(t, card)
	require.Contains(t, head, ">Rate limits for the public API</a>")
	require.Contains(t, head, `data-plan-status="presented">awaiting approval</span>`)
	approve := between(t, card, `action="/console/plans/pc-await/approve?project=demo"`, "<form", "</form>")
	require.Contains(t, approve, `<form class="plan-approve" method="post" action="/console/plans/pc-await/approve?project=demo">`,
		"Approve names its project, so a slug two projects capture is never guessed")
	require.Contains(t, approve, `<input type="hidden" name="next" value="/console/projects/demo?tab=tasks#plan-pc-await">`,
		"Approve lands back on the card")
	require.Contains(t, pcSummary(t, card), "no steps yet")
	require.NotContains(t, card, `<details class="step`, "no steps yet")

	draft := pcCard(t, panel, "pc-draft")
	require.Contains(t, pcHead(t, draft), `data-plan-status="draft">draft</span>`)
	require.Contains(t, draft, `action="/console/plans/pc-draft/approve?project=demo"`)

	approved := pcCard(t, panel, "pc-approved")
	require.Contains(t, pcHead(t, approved), `data-plan-status="approved">approved</span>`)
	require.NotContains(t, approved, "/approve", "approved: nothing to approve")
	require.Contains(t, approved, "Approved. No steps yet.")

	for _, slug := range []string{"pc-gone", "pc-composed"} {
		require.NotContains(t, panel, `id="plan-`+slug+`"`, "%s is no card", slug)
	}

	active := pcCard(t, panel, "pc-active")
	require.Contains(t, active, `action="/console/plans/pc-active/approve?project=demo"`, "a presented capture with steps still offers Approve")
	require.Contains(t, active, "pc implement")

	order := []int{
		strings.Index(panel, `id="plan-pc-await"`), strings.Index(panel, `id="plan-pc-draft"`),
		strings.Index(panel, `id="plan-pc-active"`), strings.Index(panel, `id="plan-pc-approved"`),
	}
	for i := 1; i < len(order); i++ {
		require.Less(t, order[i-1], order[i], "awaiting first, newest first, then the plans in motion, then approved not started")
	}
	require.Contains(t, page, `data-open-tally>1 open: 1 in plans, 0 off-plan</span>`, "a card with no steps counts no task")
}

// A plan with steps and no note opens: its page, its reader, its peek and
// its JSON say it has no narrative note yet and list its steps in their
// words. The card links to it; a slug nothing carries is still a 404.
func TestPcPlanWithStepsAndNoNoteOpens(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	first := pcStep(t, db, "pc-steps-only", "pc first step", nil)
	next := pcStep(t, db, "pc-steps-only", "pc next step", func(k *core.Task) { k.DependsOn = []string{first} })

	require.Contains(t, pcHead(t, pcCard(t, getHTMLBody(t, mux, "/console/projects/demo?tab=tasks"), "pc-steps-only")),
		`href="/console/plans/pc-steps-only?project=demo"`)

	for _, path := range []string{"/console/plans/pc-steps-only", "/console/plans/pc-steps-only?reader=1", "/console/plans/pc-steps-only?peek=1"} {
		body := getHTMLBody(t, mux, path)
		require.Contains(t, body, "No narrative note yet.", path)
		require.Contains(t, body, `data-no-note`, path)
		require.NotContains(t, body, "No plan with slug", path)
		require.NotContains(t, body, `/console/favorites/plan/pc-steps-only`, "%s: nothing to star without a note", path)
		require.NotContains(t, body, `href="/console/notes/"`, "%s: no link to a note that does not exist", path)
		require.NotContains(t, body, "/approve", path)
		require.Contains(t, body, `data-copy="`+first+`"`, path)
		require.Contains(t, body, `data-word="blocked"`, "%s: the steps in their words", path)
	}

	var d planDetailData
	getJSON(t, mux, "/console/plans/pc-steps-only?format=json", &d)
	require.True(t, d.NoNote)
	require.Equal(t, "pc-steps-only", d.Row.Slug)
	require.Equal(t, "pc-steps-only", d.Row.Title)
	require.Equal(t, "demo", d.Row.Project)
	require.Equal(t, 2, d.Row.TasksTotal)
	require.False(t, d.BodyLoaded)
	require.False(t, d.CanApprove)
	words := map[string]string{}
	for _, st := range d.Tasks {
		words[st.ID] = st.Word.Key
	}
	require.Equal(t, map[string]string{first: wordReady, next: wordBlocked}, words)

	rr := getPeek(t, mux, "/console/plans/pc-no-such-plan?format=json")
	require.Equal(t, http.StatusNotFound, rr.Code)
	require.Contains(t, rr.Body.String(), "No plan with slug pc-no-such-plan.")
}

// Past the completed-plans cap, the fold links to Plans for this project.
func TestPcOlderPlansLink(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	now := time.Now().UTC()
	for i := range donePlansCap + 2 {
		pcStep(t, db, "pc-done-"+strconv.Itoa(i), "pc wrapped", func(k *core.Task) { k.Status, k.ClosedAt = core.TaskDone, &now })
	}
	panel := tabPanel(t, getHTMLBody(t, mux, "/console/projects/demo?tab=tasks"), "tasks")
	fold := between(t, panel, `id="plans-completed"`, "<details", "</summary>")
	require.Contains(t, fold, strconv.Itoa(donePlansCap)+" completed plans")
	require.Contains(t, fold, `<a href="/console/plans?project=demo&amp;w=all#pg-shipped" data-older-plans title="Every plan of demo on Plans, shipped ones of all time">2 older</a>`)
	var board plansData
	getJSON(t, mux, "/console/plans?project=demo&w=all&format=json", &board)
	require.Equal(t, donePlansCap+2, board.Done, "the link opens every shipped plan of the project")
	require.NotContains(t, fold, "not shown")
}

// The card and the step row live in one template file, which the project
// tab renders; the tab's own template no longer defines them.
func TestPcCardAndStepRowShareOneTemplate(t *testing.T) {
	shared, err := templateFS.ReadFile("templates/plan_shared.html")
	require.NoError(t, err)
	require.Contains(t, string(shared), `{{define "plan-card"}}`)
	require.Contains(t, string(shared), `{{define "plan-step"}}`)
	tab, err := templateFS.ReadFile("templates/projectdetail.html")
	require.NoError(t, err)
	require.NotContains(t, string(tab), `{{define "plan-step"}}`)
	require.NotContains(t, string(tab), `{{define "plan-card"}}`)
	require.Contains(t, string(tab), `{{range .Plans}}{{template "plan-card" .}}`)
	require.Contains(t, string(tab), `{{range .Rows}}{{template "plan-step" .}}{{end}}`, "the off-plan card reuses the step row")
	for _, banned := range []string{"location.reload(", "location.assign(", "location.href =", "confirm("} {
		require.NotContains(t, string(shared), banned)
	}
}

// Under peer_auth the card reads through the request's handle: a plan step a
// gate in a locked project holds reads blocked, the gate unnamed, and the
// head counts it blocked, never waiting; a plan gate's dependent in the
// locked project is not counted; a plan whose steps sit only in the locked
// project does not open, and opens once the browser unlocked it.
func TestPcLockedProjectIsAbsent(t *testing.T) {
	lc := newLockedConsole(t)
	lockedGate := opTask(t, lc.db, lockedSlug, "approve "+lockedSecret, func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "auditor " + lockedSecret, What: "sign " + lockedSecret}
	})
	inPlan := func(deps ...string) func(*core.Task) {
		return func(k *core.Task) { k.PlanSlug, k.DependsOn = "pc-ops", deps }
	}
	held := opTask(t, lc.db, openSlug, "pc waits on the audit", inPlan(lockedGate))
	gate := opTask(t, lc.db, openSlug, "pc ops sign-off", func(k *core.Task) {
		inPlan()(k)
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "sign off"}
	})
	opTask(t, lc.db, openSlug, "pc ships after sign-off", inPlan(gate))
	opTask(t, lc.db, lockedSlug, "mirror "+lockedSecret, func(k *core.Task) { k.DependsOn = []string{gate} })
	opTask(t, lc.db, lockedSlug, "secret step "+lockedSecret, func(k *core.Task) { k.PlanSlug = "pc-locked-steps" })
	_, err := lc.mgr.WriteNote(context.Background(), core.Note{ID: mustID(t), Slug: "cc-plan-pc-secret", Title: "Capture " + lockedSecret,
		Project: lockedSlug, Body: lockedSecret, Tags: []string{plans.TagPlan, plans.SlugTag("pc-secret"), "plan-status:presented"},
		Created: time.Now().UTC(), Updated: time.Now().UTC()})
	require.NoError(t, err)

	code, page := lc.get(t, "/console/projects/"+openSlug+"?tab=tasks", "")
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, page, lockedSecret)
	require.NotContains(t, page, lockedGate)
	require.NotContains(t, page, `id="plan-pc-secret"`, "a locked project's capture is no card here")
	card := pcCard(t, page, "pc-ops")
	require.Contains(t, card, `<details class="step blocked" id="`+held+`">`, "a locked gate is never read: the step is blocked")
	require.NotContains(t, opRow(t, card, held), "data-gate-who")
	require.Contains(t, opRow(t, card, held), `data-locked-blocker="`+lockedSlug+`"`)
	require.Contains(t, opRow(t, card, gate), "holds 1 step</span>", "the locked dependent is not counted")
	summary := pcSummary(t, card)
	require.True(t, strings.HasPrefix(summary, "0/3 done · 1 blocked · 2 waiting on owner · moved "),
		"the step behind the locked gate counts as blocked, the open gate and its step as waiting: %s", summary)

	bearer := httptest.NewRequest(http.MethodGet, "/console/plans/pc-locked-steps?format=json", nil)
	bearer.Header.Set("Authorization", "Bearer "+testKey)
	bearer.Header.Set("Accept", "application/json")
	rr := lc.do(bearer)
	require.Equal(t, http.StatusForbidden, rr.Code, "steps only in the locked project: not on this handle, so the lock is named")
	require.NotContains(t, rr.Body.String(), lockedSecret)
	var refused lockedAnswer
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &refused))
	require.True(t, refused.Locked)
	require.Equal(t, lockedSlug, refused.Project)

	cookie := lc.unlockByTerminal(t)
	req := lc.browser(http.MethodGet, "/console/plans/pc-locked-steps?format=json", cookie, nil)
	req.Header.Set("Accept", "application/json")
	rr = lc.do(req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var d planDetailData
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &d))
	require.True(t, d.NoNote)
	require.Equal(t, lockedSlug, d.Row.Project)
	require.Len(t, d.Tasks, 1)

	rr = lc.do(lc.browser(http.MethodGet, "/console/projects/"+openSlug+"?tab=tasks", cookie, nil))
	require.Equal(t, http.StatusOK, rr.Code)
	card = pcCard(t, rr.Body.String(), "pc-ops")
	require.Contains(t, card, `<details class="step waiting" id="`+held+`">`, "unlocked, the gate is read: the step waits on it")
	require.Contains(t, opRow(t, card, gate), "holds 2 steps</span>")
}

// A step's chips are flex boxes cut at a width, and text-overflow does not
// reach a flex box's own text: cut there, a blocker's title stopped mid-word
// with no ellipsis. The ellipsis is the words' span's.
func TestConsoleCSS_StepChipWordsEndInAnEllipsis(t *testing.T) {
	css := string(consoleCSS)
	i := strings.Index(css, ".step .meta .edge-t {")
	require.GreaterOrEqual(t, i, 0, "the chip's words have a rule")
	rule := css[i : i+strings.Index(css[i:], "}")]
	for _, want := range []string{"min-width: 0;", "overflow: hidden;", "text-overflow: ellipsis;"} {
		require.Contains(t, rule, want)
	}
}
