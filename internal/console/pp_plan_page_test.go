package console

import (
	"context"
	"database/sql"
	"html"
	"net/http"
	"net/url"
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

// A plan's own page joins the reading frame a memory, a note and a task open
// in (plan:console-ux-pass UX 32), and the Plans board draws the project
// switcher the other lists draw.

// ppFrame is a page's item frame, from its div to the end of the view.
func ppFrame(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `<div class="item-page"`)
	require.GreaterOrEqual(t, i, 0, "no item page")
	j := strings.Index(body[i:], "</main>")
	require.Positive(t, j, "unterminated item page")
	return body[i : i+j]
}

// ppBlock is the part of frame from the first match of start to the first
// end after it.
func ppBlock(t *testing.T, frame, start, end string) string {
	t.Helper()
	i := strings.Index(frame, start)
	require.GreaterOrEqual(t, i, 0, "no %q", start)
	j := strings.Index(frame[i:], end)
	require.Positive(t, j, "no %q after %q", end, start)
	return frame[i : i+j]
}

var (
	ppBackRe  = regexp.MustCompile(`<a class="item-back" href="([^"]*)" data-item-back>.*?<span[^>]*>([^<]*)</span></a>`)
	ppCrumbRe = regexp.MustCompile(`<nav class="item-crumbs" aria-label="Where this lives">(.*?)</nav>`)
	ppLinkRe  = regexp.MustCompile(`<a href="([^"]*)"[^>]*>([^<]*)</a>`)
)

// ppBack reads a frame's way back: its link and its words, unescaped.
func ppBack(t *testing.T, frame string) (href, label string) {
	t.Helper()
	m := ppBackRe.FindStringSubmatch(frame)
	require.NotNil(t, m, "no way back")
	return html.UnescapeString(m[1]), html.UnescapeString(m[2])
}

// ppCrumbs reads a frame's crumbs as label, link pairs.
func ppCrumbs(t *testing.T, frame string) [][2]string {
	t.Helper()
	m := ppCrumbRe.FindStringSubmatch(frame)
	require.NotNil(t, m, "no crumbs")
	var out [][2]string
	for _, c := range ppLinkRe.FindAllStringSubmatch(m[1], -1) {
		out = append(out, [2]string{html.UnescapeString(c[2]), html.UnescapeString(c[1])})
	}
	return out
}

// ppSeedCapture writes a presented capture of plan in project with three
// steps (the oldest waits on the two after it), a supporting note and an
// agent cache, and returns the step ids in the page's order.
func ppSeedCapture(t *testing.T, db *ppDB, project, plan string) []string {
	t.Helper()
	now := time.Now().UTC()
	pvbNote(t, db.mgr, project, plans.NotePrefix+plan, "Storage migration -- the cut-over", plan, now.Add(-time.Hour),
		plans.TagPlan, "plan-status:presented")
	pvbNote(t, db.mgr, project, plan+"-research", "Storage research", plan, now)
	pvbNote(t, db.mgr, project, "cc-agent-"+plan, "[Explore] the old store", plan, now.Add(-2*time.Hour), plans.TagAgent)
	copyData := pvbStep(t, db.db, project, plan, "copy the data", now.Add(-50*time.Minute), nil)
	verify := pvbStep(t, db.db, project, plan, "verify the copy", now.Add(-40*time.Minute), func(k *core.Task) {
		k.DependsOn = []string{copyData}
	})
	cutover := pvbStep(t, db.db, project, plan, "cut over to the new store", now.Add(-time.Hour), func(k *core.Task) {
		k.DependsOn = []string{verify}
	})
	return []string{copyData, verify, cutover}
}

// The plan's page is the frame: the way back to the project's plans, project
// then Plans, the narrative's title with copy and the star, plan:<slug> to
// copy, the status in words and Approve in the title row, the facts, the
// steps first (in dependency order) and then the narrative in the body
// column, and the narrative note, the attached notes and the project beside
// them. No place in a list, so no previous or next.
func TestPpPlanPageIsTheFrame(t *testing.T) {
	db := ppConsole(t)
	_, err := store.EnsureProject(context.Background(), db.db, "example-ops", "example-ops")
	require.NoError(t, err)
	steps := ppSeedCapture(t, db, "example-ops", "storage")

	page := "/console/plans/storage?project=example-ops"
	body := getHTMLBody(t, db.mux, page)
	require.NotContains(t, body, `class="lib-page"`, "the frame, not the board's screen")
	frame := ppFrame(t, body)
	require.True(t, strings.HasPrefix(frame, `<div class="item-page" data-item="plan" data-item-id="storage">`))

	href, label := ppBack(t, frame)
	require.Equal(t, "/console/plans?project=example-ops", href, "the way back is the project's plans")
	require.Equal(t, "example-ops’s plans", label)
	require.NotContains(t, frame, "data-item-pos", "a plan has no place in a list")
	require.NotContains(t, frame, "data-item-step")
	require.Equal(t, [][2]string{{"example-ops", "/console/projects/example-ops"}, {"Plans", "/console/plans?project=example-ops"}},
		ppCrumbs(t, frame), "project, then Plans")

	title := ppBlock(t, frame, `<h1 class="item-title">`, "</h1>")
	require.Contains(t, title, "Storage migration: the cut-over<button", "the narrative's title, its \" -- \" shown as a colon")
	require.NotContains(t, title, "migration -- the")
	require.Contains(t, title, `<form class="star-form" method="post" action="/console/favorites/plan/storage?project=example-ops">`)
	require.Contains(t, title, `<input type="hidden" name="next" value="`+strings.ReplaceAll(page, "&", "&amp;")+`">`,
		"the star returns to this page")
	actions := ppBlock(t, frame, `<div class="item-actions">`, `<div class="item-facts">`)
	require.Contains(t, actions, `plan:storage<button type="button" class="copy-btn" data-copy="plan:storage"`)
	require.Contains(t, actions, `data-plan-status="presented">awaiting approval</span>`, "the status in words")
	require.Contains(t, actions, `<form class="plan-approve" method="post" action="/console/plans/storage/approve?project=example-ops">`)
	require.Contains(t, actions, `<input type="hidden" name="next" value="`+strings.ReplaceAll(page, "&", "&amp;")+`">`,
		"Approve returns to this page")

	facts := ppBlock(t, frame, `<div class="item-facts">`, "</header>")
	require.Contains(t, facts, `data-plan-source="capture">captured from plan mode &middot; <span class="mono">storage.md</span>`)
	require.Contains(t, facts, "0/3 steps done")
	require.Contains(t, facts, `<span data-count="ready">1 ready</span>`)
	require.Contains(t, facts, `<span data-count="blocked">2 blocked</span>`)
	require.Contains(t, facts, "1 agent run")
	require.Contains(t, facts, "updated <time")

	main := ppBlock(t, frame, `<article class="item-main">`, "</article>")
	require.Contains(t, main, `<div class="plan-page" data-plan-scope data-return="`+strings.ReplaceAll(page, "&", "&amp;")+`">`,
		"the step rows' dossiers and chips work in the body column")
	at := make([]int, 0, len(steps))
	for _, id := range steps {
		i := strings.Index(main, `id="`+id+`">`)
		require.GreaterOrEqual(t, i, 0, "step %s is in the body column", id)
		at = append(at, i)
	}
	require.Less(t, at[0], at[1], "a blocker reads above what it blocks")
	require.Less(t, at[1], at[2])
	require.Less(t, at[2], strings.Index(main, "The narrative of Storage migration"), "steps first, then the narrative")
	require.NotContains(t, main, `class="plan-head"`, "the card's head is the frame's title row and facts now")
	require.Equal(t, 1, strings.Count(body, `action="/console/plans/storage/approve`), "one Approve on the page")
	require.Equal(t, 1, strings.Count(body, `action="/console/favorites/plan/storage`), "one star on the page")

	side := ppBlock(t, frame, `<aside class="item-side"`, "</aside>")
	require.Greater(t, strings.Index(frame, `<aside class="item-side"`), strings.Index(frame, "The narrative of Storage migration"),
		"the context column follows the narrative, as it stacks on a narrow screen")
	require.Contains(t, side, `data-item-card="narrative"`)
	require.Regexp(t, `<a class="item-card-title" href="/console/notes/[0-9A-Z]{26}">Storage migration: the cut-over</a>`, side)
	attached := ppBlock(t, side, `data-item-card="attached"`, "</section>")
	require.Contains(t, attached, `Attached notes <span class="count">2</span>`)
	require.Contains(t, attached, ">Storage research</a>")
	require.Contains(t, attached, `>[Explore] the old store</a> <span class="badge">agent-cache</span>`)
	project := ppBlock(t, side, `data-item-card="project"`, "</section>")
	require.Contains(t, project, `href="/console/projects/example-ops?tab=tasks#plan-storage"`, "the project opens its tab at the plan")
	require.Contains(t, project, `<a class="item-all" href="/console/plans?project=example-ops">Every plan in example-ops`)
}

// Approve on the plan's page returns to the page (the follow contract: the
// in-place client is told where, with the notice), and the page then says
// approved with no Approve.
func TestPpApproveReturnsToThePage(t *testing.T) {
	db := ppConsole(t)
	ppSeedCapture(t, db, "example-ops", "storage")
	page := "/console/plans/storage?project=example-ops"

	rr := ndPost(db.mux, "/console/plans/storage/approve?project=example-ops", url.Values{"next": {page}}, true)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	require.Equal(t, page+"&notice="+url.QueryEscape("Approved plan:storage."), rr.Header().Get("X-Seam-Location"))

	actions := ppBlock(t, ppFrame(t, getHTMLBody(t, db.mux, page)), `<div class="item-actions">`, `<div class="item-facts">`)
	require.Contains(t, actions, `data-plan-status="approved">approved</span>`)
	require.NotContains(t, actions, "plan-approve")
}

// A composed plan says the state the board puts it in, since it has no
// status of its own; a stalled one says so with how long. A plan whose steps
// exist with no narrative note has a mono title (its slug), no star, no
// narrative card and nothing to approve.
func TestPpStatusInWordsAndNoNote(t *testing.T) {
	db := ppConsole(t)
	now := time.Now().UTC()
	pvbNote(t, db.mgr, "example-ops", "plan-ready", "Ready plan", "ready-plan", now)
	pvbStep(t, db.db, "example-ops", "ready-plan", "the first step", now, nil)
	quiet := now.AddDate(0, 0, -12)
	pvbNote(t, db.mgr, "example-ops", "plan-quiet", "Quiet plan", "quiet-plan", quiet)
	pvbStep(t, db.db, "example-ops", "quiet-plan", "the quiet step", quiet, nil)
	pvbStep(t, db.db, "example-ops", "bare-plan", "a step of no narrative", now, nil)

	actions := func(slug string) string {
		frame := ppFrame(t, getHTMLBody(t, db.mux, "/console/plans/"+slug+"?project=example-ops"))
		return ppBlock(t, frame, `<div class="item-actions">`, `<div class="item-facts">`)
	}
	require.Contains(t, actions("ready-plan"), `<span class="kind " data-plan-state="ready">ready</span>`)
	quietRow := actions("quiet-plan")
	require.NotContains(t, quietRow, "data-plan-state=", "stalled says itself with its mark")
	require.Contains(t, quietRow, `data-stalled title="Open steps, none in progress, and nothing moved for 12 days">stalled 12d</span>`)
	require.Contains(t, quietRow, "ready work, nobody on it")

	frame := ppFrame(t, getHTMLBody(t, db.mux, "/console/plans/bare-plan?project=example-ops"))
	require.Contains(t, frame, `<h1 class="item-title mono">bare-plan<button`, "no narrative: the slug, in mono")
	require.NotContains(t, frame, "star-form", "nothing to star without a note")
	require.NotContains(t, frame, `data-item-card="narrative"`)
	require.Contains(t, frame, `data-no-note>No narrative note yet.`)
	require.NotContains(t, frame, "plan-approve")
}

// A plan in the global scope: the way back and the crumb are the global
// scope's plans, and the project card names the global scope without a
// workspace to open.
func TestPpGlobalPlanPage(t *testing.T) {
	db := ppConsole(t)
	pvbStep(t, db.db, "", "shared-cleanup", "tidy the shared notes", time.Now().UTC(), nil)
	frame := ppFrame(t, getHTMLBody(t, db.mux, "/console/plans/shared-cleanup?project=global"))
	href, label := ppBack(t, frame)
	require.Equal(t, "/console/plans?project=global", href)
	require.Equal(t, "Plans in the global scope", label)
	require.Equal(t, [][2]string{{"global", "/console/plans?project=global"}, {"Plans", "/console/plans?project=global"}}, ppCrumbs(t, frame))
	project := ppBlock(t, frame, `data-item-card="project"`, "</section>")
	require.Contains(t, project, `<span class="item-card-title mono" data-plan-project="global">global</span>`)
	require.Contains(t, project, `>Every plan in the global scope<`)
}

// The board's switcher keeps the text filter and the window in every row,
// names the current choice on its button, and the board under a project
// still counts every project in the switcher.
func TestPpBoardSwitcherKeepsTheFilters(t *testing.T) {
	db := ppConsole(t)
	now := time.Now().UTC()
	pvbStep(t, db.db, "example-ops", "ops-rollout", "roll out", now, nil)
	pvbStep(t, db.db, "example-web", "web-rollout", "roll out", now.Add(-time.Minute), nil)
	pvbStep(t, db.db, "example-web", "web-other", "something else", now, nil)

	v := scParse(t, getHTMLBody(t, db.mux, "/console/plans?q=rollout&w=7d"))
	require.Equal(t, 2, v.All.Count, "the text filter applies to the counts")
	require.Equal(t, "/console/plans?q=rollout&w=7d", v.All.Href)
	for _, r := range v.Active {
		require.Equal(t, 1, r.Count, r.Value)
		require.Equal(t, "/console/plans?project="+r.Value+"&q=rollout&w=7d", r.Href, "a row keeps the text filter and the window")
	}

	body := getHTMLBody(t, db.mux, "/console/plans?project=example-web&q=rollout&w=7d")
	v = scParse(t, body)
	require.Equal(t, "example-web", v.Label)
	require.Equal(t, "example-web", v.Current)
	require.Equal(t, "/console/plans?q=rollout&w=7d", v.All.Href, "All projects drops the project only")
	require.False(t, v.All.On)
	require.Contains(t, body, `<input type="hidden" name="project" value="example-web">`, "the text filter's form keeps the project")
	require.Contains(t, body, `href="/console/plans?project=example-web&amp;q=rollout&amp;w=30d#pg-shipped">30d</a>`, "the window links keep it")
	require.Contains(t, body, `id="`+planAnchor("example-web", "web-rollout")+`"`)
	require.NotContains(t, body, `id="`+planAnchor("example-ops", "ops-rollout")+`"`)
}

// Under peer_auth the board's switcher lists a locked project with a lock and
// no count, linking to its unlock page, and nothing of its plans; the open
// project keeps its count. The plan page's crumb reads its project through
// the request's handle.
func TestPpBoardSwitcherLockedProject(t *testing.T) {
	lc := newLockedConsole(t)
	now := time.Now().UTC()
	pvbStep(t, lc.db, lockedSlug, "locked-rollout", "secret step "+lockedSecret, now, nil)
	pvbStep(t, lc.db, openSlug, "open-rollout", "open step", now, nil)

	code, body := lc.get(t, "/console/plans?w=all", "")
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, body, lockedSecret)
	require.NotContains(t, body, "locked-rollout")
	require.NotContains(t, body, lc.plan, "the locked project's narrative-only plan is absent too")
	v := scParse(t, body)
	var lock, open *scRow
	for i := range v.Active {
		switch v.Active[i].Value {
		case lockedSlug:
			lock = &v.Active[i]
		case openSlug:
			open = &v.Active[i]
		}
	}
	require.NotNil(t, lock, "the locked project is listed")
	require.True(t, lock.Locked)
	require.Equal(t, -1, lock.Count, "with no count")
	require.Equal(t, "/console/unlock/"+lockedSlug, lock.Href)
	require.False(t, lock.Query)
	require.NotNil(t, open)
	require.Equal(t, 1, open.Count)
	require.Equal(t, 1, v.All.Count, "All projects counts what this request reads")

	code, body = lc.get(t, "/console/plans/open-rollout?project="+openSlug, "")
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, body, lockedSecret)
	frame := ppFrame(t, body)
	require.Equal(t, [2]string{openSlug, "/console/projects/" + openSlug}, ppCrumbs(t, frame)[0])
}

// The step dossier's "open full" link is the task's own page as a page: it
// carries data-no-panel, so the read-in-place panel leaves it to navigate.
func TestPpDossierOpenFullSkipsThePanel(t *testing.T) {
	shared, err := templateFS.ReadFile("templates/plan_shared.html")
	require.NoError(t, err)
	js := string(shared)
	at := strings.Index(js, `open.className = 'dossier-open';`)
	require.GreaterOrEqual(t, at, 0)
	require.Contains(t, js[at:at+200], `open.setAttribute('data-no-panel', '');`)
}

// ppDB is a console with the files layer, its database and its mux.
type ppDB struct {
	db  *sql.DB
	mgr *files.Manager
	mux *http.ServeMux
}

// ppConsole is a fresh console with the files layer.
func ppConsole(t *testing.T) *ppDB {
	t.Helper()
	db, mgr, mux := newConsoleWithFiles(t)
	return &ppDB{db: db, mgr: mgr, mux: mux}
}
