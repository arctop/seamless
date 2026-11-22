package console

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The Plans & tasks tab's off-plan card (plan:console-ux-pass UX 04): every
// task of the project outside a plan, once, in the group that says what is
// true of it, each row the plan-step row; and a tally that holds the tab's
// pill to the rows.

// opTask creates an open task in project and returns its id; mut adjusts it
// before it is written.
func opTask(t *testing.T, db *sql.DB, project, title string, mut func(*core.Task)) string {
	t.Helper()
	now := time.Now().UTC()
	task := core.Task{ID: mustID(t), ProjectSlug: project, Title: title, Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}
	if mut != nil {
		mut(&task)
	}
	require.NoError(t, store.CreateTask(context.Background(), db, task))
	return task.ID
}

// opSession creates an active session and returns its id.
func opSession(t *testing.T, db *sql.DB, project, name string) string {
	t.Helper()
	now := time.Now().UTC()
	id := mustID(t)
	require.NoError(t, store.CreateSession(context.Background(), db, core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))
	return id
}

// opPlanSteps seeds a plan in project demo: a ready step, one it blocks,
// one held by sess, a claimless one and a done one (four open), and returns
// their ids.
func opPlanSteps(t *testing.T, db *sql.DB, sess string) []string {
	t.Helper()
	inPlan := func(mut func(*core.Task)) func(*core.Task) {
		return func(k *core.Task) {
			k.PlanSlug = "op-plan"
			if mut != nil {
				mut(k)
			}
		}
	}
	first := opTask(t, db, "demo", "op step first", inPlan(nil))
	next := opTask(t, db, "demo", "op step next", inPlan(func(k *core.Task) { k.DependsOn = []string{first} }))
	held := opTask(t, db, "demo", "op step held", inPlan(nil))
	_, err := store.ClaimTask(context.Background(), db, held, sess, 15*time.Minute, time.Now().UTC())
	require.NoError(t, err)
	started := opTask(t, db, "demo", "op step started", inPlan(func(k *core.Task) { k.Status = core.TaskInProgress }))
	done := opTask(t, db, "demo", "op step done", inPlan(func(k *core.Task) {
		now := time.Now().UTC()
		k.Status, k.ClosedAt = core.TaskDone, &now
	}))
	return []string{first, next, held, started, done}
}

// opRowRE matches a step row's opening tag, and an off-plan group's key.
var opRowRE = regexp.MustCompile(`data-group="([a-z-]+)"|<details class="step [^"]*" id="([0-9A-Z]{26})">`)

// opCard is the page's off-plan card: from its id to the end of the tasks
// panel (it is the panel's last card).
func opCard(t *testing.T, page string) string {
	t.Helper()
	panel := tabPanel(t, page, "tasks")
	at := strings.Index(panel, `id="offplan"`)
	require.GreaterOrEqual(t, at, 0, "no off-plan card")
	return panel[at:]
}

// opGroups maps each row of the off-plan card to the group it sits in
// (closed for the fold). A row listed twice fails the test.
func opGroups(t *testing.T, page string) map[string]string {
	t.Helper()
	out := map[string]string{}
	group := ""
	for _, m := range opRowRE.FindAllStringSubmatch(opCard(t, page), -1) {
		if m[1] != "" {
			group = m[1]
			continue
		}
		_, dup := out[m[2]]
		require.False(t, dup, "row %s is listed twice", m[2])
		out[m[2]] = group
	}
	return out
}

// opRow is a step row's summary.
func opRow(t *testing.T, page, id string) string {
	t.Helper()
	at := strings.Index(page, `id="`+id+`">`)
	require.GreaterOrEqual(t, at, 0, "no row %s", id)
	row := page[at:]
	return row[:strings.Index(row, "</summary>")]
}

// opStrip is the claim strip under a row, "" when it has none.
func opStrip(t *testing.T, page, id string) string {
	t.Helper()
	at := strings.Index(page, `id="`+id+`">`)
	require.GreaterOrEqual(t, at, 0, "no row %s", id)
	rest := page[at:]
	rest = rest[strings.Index(rest, "</details>")+len("</details>"):]
	if !strings.HasPrefix(strings.TrimLeft(rest, " \n"), `<div class="step-alert"`) {
		return ""
	}
	return rest[:strings.Index(rest, "</div>")]
}

// Claiming a ready off-plan task moves it to In progress on the project tab,
// with its holder and lease: it does not disappear.
func TestOpOffPlan_ClaimMovesToInProgress(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	sess := opSession(t, db, "demo", "cc/op-worker")
	id := opTask(t, db, "demo", "op ship the report", nil)

	page := getHTMLBody(t, mux, "/console/projects/demo?tab=tasks")
	require.Equal(t, "ready", opGroups(t, page)[id])
	require.Contains(t, opRow(t, page, id), `data-word="ready"`)

	_, err := store.ClaimTask(context.Background(), db, id, sess, 15*time.Minute, time.Now().UTC())
	require.NoError(t, err)
	page = getHTMLBody(t, mux, "/console/projects/demo?tab=tasks")
	require.Equal(t, map[string]string{id: "in-progress"}, opGroups(t, page), "claimed, it is still on the tab, in progress")
	require.Contains(t, page, `<details class="step doing" id="`+id+`">`)
	row := opRow(t, page, id)
	require.Contains(t, row, `data-word="in-progress"`)
	require.Contains(t, row, `<a href="/console/sessions/`+sess+`">cc/op-worker</a>`, "the holder, linked")
	require.Contains(t, row, `data-lease-exp=`, "the lease, ticking")
	require.Empty(t, opStrip(t, page, id), "a live claim needs no strip")
	require.Contains(t, page, `data-open-tally>1 open: 0 in plans, 1 off-plan</span>`)
}

// On a project with every state, each open off-plan task appears once, under
// its group; the closed ones sit in the fold; plan steps are on their cards
// only.
func TestOpOffPlan_EveryOpenTaskOnceInItsGroup(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	f := rwSeedFleet(t, db, "")
	steps := opPlanSteps(t, db, f.sess)
	own := opTask(t, db, "demo", "op own gate", func(k *core.Task) {
		k.Gate = core.TaskGate{Who: "hardware", What: "boards arrive"}
	})
	started := opTask(t, db, "demo", "op started gate", func(k *core.Task) {
		k.Kind, k.Gate, k.Status = core.TaskKindGate, core.TaskGate{Who: "owner", What: "pick a vendor"}, core.TaskInProgress
	})

	page := getHTMLBody(t, mux, "/console/projects/demo?tab=tasks")
	groups := opGroups(t, page)
	require.Equal(t, map[string]string{
		f.ready: "ready", f.blocker: "ready",
		f.blocked: "blocked",
		f.gated:   "waiting", own: "waiting",
		f.gate: "gates", started: "gates",
		f.held: "in-progress", f.lapsed: "in-progress", f.claimless: "in-progress",
		f.done: "closed", f.dropped: "closed",
	}, groups, "every off-plan task once, in the group that says what is true of it")
	for _, id := range steps {
		require.NotContains(t, groups, id, "a plan step is on its plan's card, not here")
		require.Equal(t, 1, strings.Count(page, `id="`+id+`">`), "step %s is on the page once", id)
	}

	card := opCard(t, page)
	require.Contains(t, card, `<div class="offplan-hd" title="Marked in progress. Only a live claim holds a task: each row says whether one does.">In progress <span class="n">3</span><span class="offplan-note">1 claim lapsed · 1 unclaimed</span></div>`,
		"the group says which of its rows no live claim holds")
	require.Contains(t, opRow(t, page, f.held), `data-word="in-progress"`)
	require.Contains(t, opRow(t, page, f.lapsed), `data-word="claim-lapsed"`)
	require.Contains(t, opRow(t, page, f.claimless), `data-word="unclaimed"`)
	require.Contains(t, opStrip(t, page, f.lapsed), `action="/console/tasks/`+f.lapsed+`/release"`, "a lapsed claim is released from its row")
	require.Contains(t, opStrip(t, page, f.claimless), `data-open-step="`+f.claimless+`"`)
	blocked := opRow(t, page, f.blocked)
	require.Contains(t, blocked, `<a class="edge" href="#`+f.blocker+`" data-jump title="blocked by rw blocker &middot; ready">`,
		"a blocked row names its first blocker and links to it")
	require.Contains(t, opRow(t, page, f.gate), `data-word="waiting"`)
	require.Contains(t, opRow(t, page, started), `data-word="waiting"`, "a gate marked in progress still waits on its people")
	require.Empty(t, opStrip(t, page, started), "nobody claims a gate: no claim strip")
	require.Contains(t, page, `data-open-tally>14 open: 4 in plans, 10 off-plan</span>`)
	fold := card[strings.Index(card, `<details class="step-run offplan-closed"`):]
	require.Contains(t, fold, "2 recently closed &middot; ", "the fold says how many it holds")
	require.NotContains(t, fold[:strings.Index(fold, "</summary>")], " of ", "and no more exist")

	panel := tabPanel(t, page, "tasks")
	require.NotContains(t, panel, ">in_progress<")
	require.NotContains(t, panel, ">open</span>", "no row prints its stored status")
}

// The tab's pill counts the project's open tasks; the rows the tab lists are
// exactly those, plan steps and off-plan tasks, and the tally line says so.
func TestOpOffPlan_PillEqualsTheOpenRows(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	f := rwSeedFleet(t, db, "")
	opPlanSteps(t, db, f.sess)
	// A finished plan: its steps are closed, so none of them is open.
	for _, title := range []string{"op wrapped one", "op wrapped two"} {
		opTask(t, db, "demo", title, func(k *core.Task) {
			now := time.Now().UTC()
			k.PlanSlug, k.Status, k.ClosedAt = "op-done", core.TaskDone, &now
		})
	}
	// Another project's work counts on its own page only.
	pvProject(t, db, "other", "Other")
	opTask(t, db, "other", "op elsewhere", nil)

	page := getHTMLBody(t, mux, "/console/projects/demo?tab=tasks")
	panel := tabPanel(t, page, "tasks")
	plansPart := panel[:strings.Index(panel, `id="offplan"`)]
	planOpen := len(regexp.MustCompile(`<details class="step (open|doing|blocked|waiting)[^"]*" id=`).FindAllString(plansPart, -1))
	offOpen := 0
	for _, group := range opGroups(t, page) {
		if group != "closed" {
			offOpen++
		}
	}
	require.Equal(t, 4, planOpen)
	require.Equal(t, 8, offOpen)
	pill, err := strconv.Atoi(pvPill(t, page, "Plans &amp; tasks"))
	require.NoError(t, err)
	require.Equal(t, planOpen+offOpen, pill, "the pill equals the open rows listed")
	require.Equal(t, pvPill(t, getHTMLBody(t, mux, "/console/projects/demo?tab=overview"), "Plans &amp; tasks"), strconv.Itoa(pill),
		"every tab's pill is the same count")
	require.Contains(t, panel, `data-open-tally>`+strconv.Itoa(pill)+` open: `+strconv.Itoa(planOpen)+` in plans, `+strconv.Itoa(offOpen)+` off-plan</span>`)
}

// The fold holds the ten most recently closed off-plan tasks, newest closed
// first, and links to all of them on the Tasks screen.
func TestOpOffPlan_ClosedFoldShowsTheLastTen(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)
	ids := make([]string, 12)
	for i := range ids {
		// Created newest first, closed oldest first: the fold must read the
		// close, not the creation.
		created := base.Add(-time.Duration(i) * time.Minute)
		ids[i] = opTask(t, db, "demo", "op closed "+strconv.Itoa(i), func(k *core.Task) { k.CreatedAt, k.UpdatedAt = created, created })
		status := core.TaskDone
		if i%3 == 0 {
			status = core.TaskDropped
		}
		_, err := store.UpdateTask(ctx, db, ids[i], store.TaskPatch{Status: &status}, "", base.Add(time.Duration(i)*time.Minute))
		require.NoError(t, err)
	}

	page := getHTMLBody(t, mux, "/console/projects/demo?tab=tasks")
	card := opCard(t, page)
	require.Contains(t, card, `<div class="empty">No open off-plan tasks</div>`)
	fold := card[strings.Index(card, `<details class="step-run offplan-closed" id="offplan-closed" data-group="closed">`):]
	summary := fold[:strings.Index(fold, "</summary>")]
	require.Contains(t, summary, "10 recently closed of 12 &middot; ")
	require.Contains(t, summary, `<a href="/console/tasks?project=demo&amp;closed=all">all closed</a>`)
	var order []string
	for _, m := range opRowRE.FindAllStringSubmatch(fold, -1) {
		if m[2] != "" {
			order = append(order, m[2])
		}
	}
	want := make([]string, 0, 10)
	for i := 11; i >= 2; i-- {
		want = append(want, ids[i])
	}
	require.Equal(t, want, order, "the ten most recently closed, newest closed first")
	require.Contains(t, opRow(t, page, ids[3]), `data-word="dropped"`)
	require.Contains(t, opRow(t, page, ids[4]), `data-word="done"`)
}

// Waiting on people says who and what; a gate says what it holds: the
// unfinished tasks that depend on it, wherever they are.
func TestOpOffPlan_GatesSayWhatTheyHold(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	pvProject(t, db, "other", "Other")
	gate := opTask(t, db, "demo", "op sign-off", func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "sign the release"}
	})
	dependsOnGate := func(k *core.Task) { k.DependsOn = []string{gate} }
	waiting := opTask(t, db, "demo", "op cut the release", dependsOnGate)
	opTask(t, db, "demo", "op announce", func(k *core.Task) { k.DependsOn, k.PlanSlug = []string{gate}, "op-launch" })
	opTask(t, db, "other", "op mirror the release", dependsOnGate)
	opTask(t, db, "demo", "op dropped follow-up", func(k *core.Task) {
		now := time.Now().UTC()
		k.DependsOn, k.Status, k.ClosedAt = []string{gate}, core.TaskDropped, &now
	})

	page := getHTMLBody(t, mux, "/console/projects/demo?tab=tasks")
	require.Equal(t, "gates", opGroups(t, page)[gate])
	row := opRow(t, page, gate)
	require.Contains(t, row, `<span class="edge" data-gate-who="owner" title="waits on owner: sign the release">`)
	require.Contains(t, row, `<span class="edge ok" data-unblocks title="holds op cut the release · op announce · op mirror the release">`,
		"what the gate holds, by title, the other project's included and the dropped one not")
	require.Contains(t, row, "holds 3 tasks</span>", "counted, in the backlog's word")
	require.NotContains(t, row, "</svg>unblocks", "a gate holds; it does not unblock")

	require.Equal(t, "waiting", opGroups(t, page)[waiting])
	row = opRow(t, page, waiting)
	require.Contains(t, row, "waits on owner: sign the release</span>", "who and what")
	require.Contains(t, row, `href="#`+gate+`" data-jump`, "the gate it waits on, a jump on this page")
	require.Contains(t, row, `data-word="waiting"`)
}

// A lapsed claim's Release claim posts the existing release route and lands
// back on the tab at the row, which is ready again.
func TestOpOffPlan_ReleaseLandsOnTheRow(t *testing.T) {
	db, mux := newConsole(t)
	pvProject(t, db, "demo", "Demo")
	sess := opSession(t, db, "demo", "cc/op-ghost")
	id := opTask(t, db, "demo", "op stranded", nil)
	_, err := store.ClaimTask(context.Background(), db, id, sess, time.Minute, time.Now().UTC().Add(-time.Hour))
	require.NoError(t, err)

	page := getHTMLBody(t, mux, "/console/projects/demo?tab=tasks")
	require.Equal(t, "in-progress", opGroups(t, page)[id])
	strip := opStrip(t, page, id)
	require.Contains(t, strip, `data-claim-strip="claim-lapsed"`)
	next := "/console/projects/demo?tab=tasks#" + id
	require.Contains(t, strip, `<input type="hidden" name="next" value="`+next+`">`)

	req := httptest.NewRequest(http.MethodPost, "http://localhost:8081/console/tasks/"+id+"/release",
		strings.NewReader("next="+url.QueryEscape(next)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("X-Seam-Follow", "1")
	req.AddCookie(consoleCookie())
	rr := do(mux, req)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	require.Equal(t, "/console/projects/demo?tab=tasks&notice=Released+the+claim.#"+id, rr.Header().Get("X-Seam-Location"),
		"the anchor survives the redirect")
	require.Equal(t, "ready", opGroups(t, getHTMLBody(t, mux, "/console/projects/demo?tab=tasks"))[id])
}

// Under peer_auth the card reads through the request's handle. A task a
// locked project blocks is listed under Blocked, named only by that project,
// and is never offered as ready; a locked gate is never named; what an open
// gate holds leaves the locked project's tasks out. Unlocked, the browser
// reads them all.
func TestOpOffPlan_LockedBlockersUnderBlocked(t *testing.T) {
	lc := newLockedConsole(t)
	lockedGate := opTask(t, lc.db, lockedSlug, "approve "+lockedSecret, func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "auditor " + lockedSecret, What: "sign " + lockedSecret}
	})
	prep := opTask(t, lc.db, openSlug, "op prep", nil)
	waits := opTask(t, lc.db, openSlug, "op waits on the ledger", func(k *core.Task) { k.DependsOn = []string{lc.taskID} })
	both := opTask(t, lc.db, openSlug, "op waits on both", func(k *core.Task) { k.DependsOn = []string{lc.taskID, prep} })
	gated := opTask(t, lc.db, openSlug, "op waits on the audit", func(k *core.Task) { k.DependsOn = []string{lockedGate} })
	openGate := opTask(t, lc.db, openSlug, "op ops sign-off", func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "sign off"}
	})
	opTask(t, lc.db, lockedSlug, "mirror "+lockedSecret, func(k *core.Task) { k.DependsOn = []string{openGate} })
	opTask(t, lc.db, openSlug, "op ships after sign-off", func(k *core.Task) { k.DependsOn = []string{openGate} })
	opTask(t, lc.db, lockedSlug, "closed "+lockedSecret, func(k *core.Task) {
		now := time.Now().UTC()
		k.Status, k.ClosedAt = core.TaskDone, &now
	})

	code, page := lc.get(t, "/console/projects/"+openSlug+"?tab=tasks", "")
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, page, lockedSecret, "no locked content: not a title, not a gate's who or what")
	require.NotContains(t, page, lc.taskID)
	require.NotContains(t, page, lockedGate)
	groups := opGroups(t, page)
	require.Equal(t, "ready", groups[prep])
	for _, id := range []string{waits, both, gated} {
		require.Equal(t, "blocked", groups[id], "%s: a task a locked project blocks is never ready", id)
		require.Contains(t, page, `<details class="step blocked" id="`+id+`">`)
		row := opRow(t, page, id)
		require.Contains(t, row, `data-locked-blocker="`+lockedSlug+`"`)
		require.Contains(t, row, "a task in locked project "+lockedSlug+" blocks this one")
		require.Contains(t, row, `data-word="blocked"`)
		require.NotContains(t, row, "data-gate-who", "%s: a locked gate is never named", id)
	}
	require.Contains(t, opRow(t, page, both), `href="#`+prep+`" data-jump`, "the readable blocker is still named")
	require.Contains(t, opRow(t, page, openGate), "holds 1 task</span>", "the locked project's dependent is not counted")
	require.Contains(t, page, `data-open-tally>6 open: 0 in plans, 6 off-plan</span>`)

	// A browser that unlocked the project reads the blockers themselves.
	cookie := lc.unlockByTerminal(t)
	rr := lc.do(lc.browser(http.MethodGet, "/console/projects/"+openSlug+"?tab=tasks", cookie, nil))
	require.Equal(t, http.StatusOK, rr.Code)
	page = rr.Body.String()
	require.NotContains(t, opCard(t, page), "data-locked-blocker")
	groups = opGroups(t, page)
	require.Equal(t, "blocked", groups[waits])
	require.Contains(t, opRow(t, page, waits), `href="/console/tasks/`+lc.taskID+`"`, "a blocker off this page links to its task")
	require.Equal(t, "waiting", groups[gated])
	require.Contains(t, opRow(t, page, gated), `data-gate-who="auditor `+lockedSecret+`"`)
	require.Contains(t, opRow(t, page, openGate), "holds 2 tasks</span>")
}
