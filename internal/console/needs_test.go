package console

// Tests of the needs-you model (needs.go), the Overview it draws, Work in
// motion and the live strip from Now's model, the links a level offers, and
// Mark reviewed on the Mishaps screen (plan:console-ux-pass UX 12).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/backup"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
	"github.com/arctop/seamless/internal/update"
)

// ndConsole is a console with a files layer, at a chosen level and feature
// state, with test views of the backup service and the update check.
type ndConsole struct {
	db      *sql.DB
	mgr     *files.Manager
	mux     *http.ServeMux
	backups *fakeBackups
	updates *fakeUpdates
}

func ndNew(t *testing.T, lvl string, feats config.Features) *ndConsole {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	dataDir := filepath.Join(dir, "data")
	mgr, err := files.NewManager(dataDir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	c := &ndConsole{db: db, mgr: mgr,
		backups: &fakeBackups{db: db, status: backup.Status{LastBackup: &backup.Record{At: time.Now().Add(-time.Hour)}}},
		updates: &fakeUpdates{db: db, status: update.Status{Settings: update.Settings{Check: true}}}}
	svc, err := New(Config{DB: db, Files: mgr, Events: events.NewRecorder(db), APIKey: testKey, DataDir: dataDir,
		Level: lvl, Features: feats, Backups: c.backups, Updates: c.updates})
	require.NoError(t, err)
	c.mux = http.NewServeMux()
	svc.Register(c.mux)
	return c
}

// ndSession creates a session; live sessions heartbeat now.
func ndSession(t *testing.T, db *sql.DB, name, project string, live bool) string {
	t.Helper()
	id := mustID(t)
	at := time.Now().UTC()
	status := core.SessionActive
	if !live {
		at, status = at.Add(-48*time.Hour), core.SessionCompleted
	}
	require.NoError(t, store.CreateSession(context.Background(), db, core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: status, CreatedAt: at.Add(-time.Hour), UpdatedAt: at,
	}))
	return id
}

// ndTask creates a task, filling its id, creator and times.
func ndTask(t *testing.T, db *sql.DB, task core.Task) string {
	t.Helper()
	if task.ID == "" {
		task.ID = mustID(t)
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
	}
	if task.UpdatedAt.IsZero() {
		task.UpdatedAt = task.CreatedAt
	}
	if task.Status == "" {
		task.Status = core.TaskOpen
	}
	task.CreatedBy = "test"
	require.NoError(t, store.CreateTask(context.Background(), db, task))
	return task.ID
}

// ndCapture writes a captured Claude Code plan in a status.
func ndCapture(t *testing.T, mgr *files.Manager, project, slug, status string) core.Note {
	t.Helper()
	now := time.Now().UTC().Add(-time.Hour)
	n, err := mgr.WriteNote(context.Background(), core.Note{
		ID: mustID(t), Slug: "cc-plan-" + slug, Title: "Plan " + slug, Project: project, Description: "captured " + slug,
		Body: "# " + slug, Tags: plans.SetStatusTag([]string{plans.TagPlan, plans.SlugTag(slug)}, status),
		Created: now, Updated: now,
	})
	require.NoError(t, err)
	return n
}

// ndMishap records one agent.mishap report at ts.
func ndMishap(t *testing.T, db *sql.DB, project, desc string, ts time.Time) string {
	t.Helper()
	p := map[string]any{"description": desc}
	if typ, _ := core.ParseMishap(desc); typ != "" {
		p["type"] = string(typ)
	}
	id, err := events.NewRecorder(db).Record(context.Background(), core.Event{
		Kind: core.EventAgentMishap, ProjectSlug: project, TS: ts, Payload: p,
	})
	require.NoError(t, err)
	return id
}

// ndHeld holds one write for the owner.
func ndHeld(t *testing.T, db *sql.DB, project string) {
	t.Helper()
	require.NoError(t, store.InsertHeldWrite(context.Background(), db, store.HeldWrite{
		ID: mustID(t), Op: store.HeldOpWrite, Tool: "memory_write", ItemKind: core.ItemKindMemory, ItemID: mustID(t),
		Project: project, Name: "held-rule", MemoryKind: core.KindConstraint, Reasons: []string{"outside-text"},
		Content: "a held body",
	}))
}

// ndOverview is the part of the Overview's JSON answer these tests read.
type ndOverview struct {
	Attention []attnCard
	Motion    *workMotion
	Live      []liveSessionRow
	LiveTotal int
	LiveMore  int
}

func ndRead(t *testing.T, mux *http.ServeMux) ndOverview {
	t.Helper()
	var ov ndOverview
	getJSON(t, mux, "/console/?format=json", &ov)
	return ov
}

func ndKinds(cards []attnCard) []string {
	out := make([]string, 0, len(cards))
	for _, c := range cards {
		out = append(out, c.Kind)
	}
	return out
}

func ndCard(t *testing.T, cards []attnCard, kind string) attnCard {
	t.Helper()
	i := slices.IndexFunc(cards, func(c attnCard) bool { return c.Kind == kind })
	require.GreaterOrEqual(t, i, 0, "no %s item in %v", kind, ndKinds(cards))
	return cards[i]
}

// ndPost posts a form as the owner's browser does: the cookie, the
// same-origin fetch header, and X-Seam-Follow when follow (navigation.js).
func ndPost(mux *http.ServeMux, path string, form url.Values, follow bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8081"+path, strings.NewReader(form.Encode()))
	req.AddCookie(consoleCookie())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if follow {
		req.Header.Set("X-Seam-Follow", "1")
	}
	return do(mux, req)
}

// ndAct takes an item's inline action as its form posts it.
func ndAct(t *testing.T, mux *http.ServeMux, card attnCard) *httptest.ResponseRecorder {
	t.Helper()
	require.NotNil(t, card.Action, "%s has an inline action", card.Kind)
	form := url.Values{}
	for _, f := range card.Action.Fields {
		form.Set(f.Name, f.Value)
	}
	return ndPost(mux, card.Action.Post, form, true)
}

// ndMain is the page's <main> element: what the page itself links, apart
// from the sidebar.
func ndMain(t *testing.T, body string) string {
	t.Helper()
	start, end := strings.Index(body, "<main"), strings.Index(body, "</main>")
	require.True(t, start >= 0 && end > start)
	return body[start:end]
}

// ndFixture seeds one of every item the strip has while every feature it
// reads is on: a held write, a lock declaration, gates on the owner and on a
// reviewer, a presented plan, a lapsed and a claimless claim, mishaps, a
// proposal and a near-done plan; with a live agent holding a plan step.
type ndSeeded struct {
	gateID, otherGateID, lapsedID, claimlessID, heldStepID, readyID, liveID string
	lock                                                                    core.RepoLock
}

func ndFixture(t *testing.T, c *ndConsole) ndSeeded {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	var s ndSeeded
	ndHeld(t, c.db, "example-ops")
	s.lock = lkSeedLock(t, c.db, "main", "/work/app", core.RepoLockRequested, "example-ops")
	s.gateID = ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Sign the vendor renewal", Kind: core.TaskKindGate,
		Gate: core.TaskGate{Who: "owner", What: "sign the renewal"}})
	s.otherGateID = ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Review the firewall change", Kind: core.TaskKindGate,
		Gate: core.TaskGate{Who: "reviewer:dana", What: "read the change"}})
	ndCapture(t, c.mgr, "example-ops", "rate-limits", plans.StatusPresented)
	gone := ndSession(t, c.db, "cc/nd-gone", "example-ops", false)
	expired := now.Add(-20 * time.Minute)
	s.lapsedID = ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Move the exporter", Status: core.TaskInProgress,
		ClaimedBy: gone, LeaseExpiresAt: &expired})
	s.claimlessID = ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Tidy the routing table", Status: core.TaskInProgress})
	ndMishap(t, c.db, "example-ops", "shell-trap: a bare == died", now.Add(-time.Hour))
	_, err := store.CreateProposal(ctx, c.db, store.ProposalArchive, map[string]any{"key": "nd-k1"})
	require.NoError(t, err)
	s.liveID = ndSession(t, c.db, "cc/nd-live", "example-ops", true)
	lease := now.Add(time.Hour)
	s.heldStepID = ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Write the migration", PlanSlug: "storage",
		Status: core.TaskInProgress, ClaimedBy: s.liveID, LeaseExpiresAt: &lease, UpdatedAt: now})
	s.readyID = ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "Run the dry run", PlanSlug: "storage"})
	for i := range 4 {
		ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "closed step", PlanSlug: "nearly", Status: core.TaskDone,
			CreatedAt: now.Add(-time.Duration(10+i) * time.Hour)})
	}
	ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "write the docs", PlanSlug: "nearly"})
	return s
}

// ndAllOn is every feature the strip reads.
var ndAllOn = config.Features{Screening: true, RepoLocks: true}

// The strip lists every kind of item in its one order -- what an agent is
// blocked on first, then the owner's queues, then the knowledge queue -- each
// a sentence with its true count, a link and, where one step settles it, that
// step.
func TestNdStrip_OrdersWhatNeedsYou(t *testing.T) {
	c := ndNew(t, "advanced", ndAllOn)
	s := ndFixture(t, c)
	// An update that rolled back, and no backup at all.
	c.updates.status = update.Status{Settings: update.Settings{Check: true, Auto: true},
		Paused: &update.Pause{At: time.Now().Add(-time.Hour), Reason: update.PauseBroken}}
	c.backups.status = backup.Status{}

	ov := ndRead(t, c.mux)
	require.Equal(t, []string{needHeld, needLocks, needGates, needGatesOthers, needPlans, needLapsed, needUnclaimed,
		needMishaps, needUpdates, needBackups, needProposals, needFinishLine}, ndKinds(ov.Attention))

	gate := ndCard(t, ov.Attention, needGates)
	require.Equal(t, "1 gate waits on you", gate.Title)
	require.Equal(t, "Sign the vendor renewal: sign the renewal", gate.Sub)
	require.Equal(t, "/console/tasks/"+s.gateID, gate.Href)
	require.Equal(t, "Mark as handled", gate.Action.Label)
	require.Equal(t, "/console/tasks/"+s.gateID+"/gate/clear", gate.Action.Post)
	require.True(t, gate.Blocking)
	others := ndCard(t, ov.Attention, needGatesOthers)
	require.Equal(t, "1 gate waits on reviewer:dana", others.Title)
	require.Nil(t, others.Action, "a gate on someone else is theirs to clear")

	plan := ndCard(t, ov.Attention, needPlans)
	require.Equal(t, "1 plan awaits your approval", plan.Title)
	require.Equal(t, "/console/plans/rate-limits/approve?project=example-ops", plan.Action.Post, "Approve names the capture's project")
	require.Equal(t, "/console/plans/rate-limits?project=example-ops", plan.Href)
	lapsed := ndCard(t, ov.Attention, needLapsed)
	require.Equal(t, "1 claim lapsed", lapsed.Title)
	require.Equal(t, "danger", lapsed.Sev)
	require.Contains(t, lapsed.Sub, "Move the exporter · lapsed 20m ago · claimed by cc/nd-gone")
	require.Equal(t, "/console/tasks/"+s.lapsedID+"/release", lapsed.Action.Post)
	require.Empty(t, lapsed.Action.Confirm, "no live holder to interrupt: no confirm, Now's rule")
	claimless := ndCard(t, ov.Attention, needUnclaimed)
	require.Equal(t, "/console/tasks/"+s.claimlessID+"/release", claimless.Action.Post)
	lock := ndCard(t, ov.Attention, needLocks)
	require.Equal(t, "/console/locks/"+s.lock.ID+"/approve", lock.Action.Post)
	require.Nil(t, ndCard(t, ov.Attention, needHeld).Action, "a held write is read before it is released")

	mishaps := ndCard(t, ov.Attention, needMishaps)
	require.Equal(t, "1 mishap to review", mishaps.Title)
	require.Equal(t, mishapsPath, mishaps.Href)
	up := ndCard(t, ov.Attention, needUpdates)
	require.Equal(t, "danger", up.Sev)
	require.Equal(t, "Automatic updates paused themselves", up.Title)
	require.Equal(t, updatesSectionHref, up.Href)
	bk := ndCard(t, ov.Attention, needBackups)
	require.Equal(t, "No backup yet", bk.Title)
	require.Equal(t, backupsSectionHref, bk.Href)

	// Every inline action returns here, at the strip.
	for _, card := range ov.Attention {
		if card.Action != nil {
			require.Equal(t, []needField{{"next", "/console/#needs"}}, card.Action.Fields, card.Kind)
		}
	}

	body := ndMain(t, getHTMLBody(t, c.mux, "/console/"))
	require.Contains(t, body, `<div class="ov2-attn-card" data-sev="warn" data-need="gates" data-blocking>`)
	require.Contains(t, body, `<form class="ov2-attn-act" method="post" action="/console/tasks/`+s.gateID+`/gate/clear">`)
	require.Contains(t, body, `<input type="hidden" name="next" value="/console/#needs">`)
	require.Contains(t, body, `Needs you<strong title="11 items on 11 cards; plans near the finish line are not counted">11</strong>`, "the finish line is not counted")
}

// An item whose optional feature is off does not exist, and a strip with
// nothing in it is no strip at all.
func TestNdStrip_FeatureOffItemsDoNotExist(t *testing.T) {
	c := ndNew(t, "advanced", config.Features{})
	ndFixture(t, c)
	kinds := ndKinds(ndRead(t, c.mux).Attention)
	for _, off := range []string{needHeld, needLocks} {
		require.NotContains(t, kinds, off, "its feature is off")
	}
	require.Contains(t, kinds, needGates)
	require.Contains(t, kinds, needFinishLine, "the finish line is momentum's, which is always on")

	empty := ndNew(t, "advanced", ndAllOn)
	require.Empty(t, ndRead(t, empty.mux).Attention)
	require.NotContains(t, getHTMLBody(t, empty.mux, "/console/"), `id="needs"`)
}

// Several of a kind are one item with their count and a link to the list;
// only one alone carries an inline action.
func TestNdStrip_SeveralOfAKindIsOneItem(t *testing.T) {
	c := ndNew(t, "advanced", config.Features{})
	for _, title := range []string{"first gate", "second gate"} {
		ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: title, Kind: core.TaskKindGate, Gate: core.TaskGate{Who: "owner", What: "x"}})
	}
	// A step whose own gate waits on the owner counts; one that waits only
	// on a gate it depends on is counted through that gate.
	step := ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "gated step", Gate: core.TaskGate{Who: "owner", What: "pick a night"}})
	gate := ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "blocking gate", Kind: core.TaskKindGate, Gate: core.TaskGate{Who: "hardware", What: "disks"}})
	ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: "waits on the gate", DependsOn: []string{gate}})
	ndCapture(t, c.mgr, "example-ops", "one", plans.StatusPresented)
	ndCapture(t, c.mgr, "example-ops", "two", plans.StatusPresented)
	ndCapture(t, c.mgr, "example-ops", "draft-only", plans.StatusDraft)

	ov := ndRead(t, c.mux)
	gates := ndCard(t, ov.Attention, needGates)
	require.Equal(t, 3, gates.Count)
	require.Equal(t, "3 gates wait on you", gates.Title)
	require.Equal(t, "/console/tasks", gates.Href)
	require.Nil(t, gates.Action)
	others := ndCard(t, ov.Attention, needGatesOthers)
	require.Equal(t, "1 gate waits on hardware", others.Title, "the step behind it is not counted again")
	plansCard := ndCard(t, ov.Attention, needPlans)
	require.Equal(t, "2 plans await your approval", plansCard.Title, "a draft is not awaiting approval")
	require.Equal(t, "/console/plans", plansCard.Href)
	require.Nil(t, plansCard.Action)
	_ = step
}

// An inline action settles its item: it posts to the item's own route with
// next back to the strip, the in-place client is told to follow to the
// Overview at #needs (no reload), and the item is gone from the page it
// lands on.
func TestNdInlineActions_SettleTheirItem(t *testing.T) {
	c := ndNew(t, "advanced", ndAllOn)
	s := ndFixture(t, c)
	for _, tc := range []struct {
		kind, notice string
		settled      func() bool
	}{
		{needGates, "Marked as handled", func() bool {
			got, err := store.TaskByID(context.Background(), c.db, s.gateID)
			require.NoError(t, err)
			return got.Status == core.TaskDone
		}},
		{needLapsed, "Released the claim", func() bool {
			got, err := store.TaskByID(context.Background(), c.db, s.lapsedID)
			require.NoError(t, err)
			return got.Status == core.TaskOpen
		}},
		{needUnclaimed, "Released the claim", func() bool {
			got, err := store.TaskByID(context.Background(), c.db, s.claimlessID)
			require.NoError(t, err)
			return got.Status == core.TaskOpen
		}},
		{needPlans, "Approved plan:rate-limits", func() bool {
			return !slices.Contains(ndKinds(ndRead(t, c.mux).Attention), needPlans)
		}},
		{needLocks, "Approved LOCK", func() bool {
			l, ok, err := store.RepoLockByID(context.Background(), c.db, s.lock.ID)
			require.NoError(t, err)
			require.True(t, ok)
			return l.State == core.RepoLockActive
		}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			card := ndCard(t, ndRead(t, c.mux).Attention, tc.kind)
			rr := ndAct(t, c.mux, card)
			require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
			loc := rr.Header().Get("X-Seam-Location")
			require.True(t, strings.HasPrefix(loc, "/console/?notice="), loc)
			require.True(t, strings.HasSuffix(loc, "#needs"), loc)
			msg, err := url.QueryUnescape(strings.TrimSuffix(strings.TrimPrefix(loc, "/console/?notice="), "#needs"))
			require.NoError(t, err)
			require.Contains(t, msg, tc.notice)
			require.True(t, tc.settled())
			require.NotContains(t, ndKinds(ndRead(t, c.mux).Attention), tc.kind, "the item is settled")
			// The page the client lands on draws the strip without it.
			landed := getHTMLBody(t, c.mux, loc)
			require.NotContains(t, landed, `data-need="`+tc.kind+`"`)
		})
	}
}

// The plan and lock decisions take an optional next on the task actions'
// terms: an accepted one returns there with a notice, a refused or absent
// one answers exactly as before, and a JSON caller's answer is unchanged.
func TestNdNext_PlanAndLockDecisions(t *testing.T) {
	plan := func(t *testing.T, form url.Values) *httptest.ResponseRecorder {
		c := ndNew(t, "advanced", ndAllOn)
		ndCapture(t, c.mgr, "example-ops", "rate-limits", plans.StatusPresented)
		return ndPost(c.mux, "/console/plans/rate-limits/approve", form, false)
	}
	rr := plan(t, url.Values{"next": {"/console/?w=7d#needs"}})
	require.Equal(t, http.StatusSeeOther, rr.Code, rr.Body.String())
	require.Equal(t, "/console/?w=7d&notice="+url.QueryEscape("Approved plan:rate-limits.")+"#needs", rr.Header().Get("Location"))
	// The project tab's Approve sends a project page with a query and an anchor.
	rr = plan(t, url.Values{"next": {"/console/projects/example-ops?tab=tasks#plan-rate-limits"}})
	require.Equal(t, "/console/projects/example-ops?tab=tasks&notice="+url.QueryEscape("Approved plan:rate-limits.")+"#plan-rate-limits",
		rr.Header().Get("Location"))
	for _, refused := range []url.Values{nil, {"next": {""}}, {"next": {"https://example.com/x"}}, {"next": {"/console/"}}, {"next": {"//example.com"}}} {
		rr = plan(t, refused)
		require.Equal(t, http.StatusSeeOther, rr.Code)
		require.Equal(t, "/console/plans/rate-limits", rr.Header().Get("Location"), "next %v: the plan's page, as before", refused)
	}
	c := ndNew(t, "advanced", ndAllOn)
	ndCapture(t, c.mgr, "example-ops", "rate-limits", plans.StatusPresented)
	req := httptest.NewRequest(http.MethodPost, "/console/plans/rate-limits/approve", strings.NewReader("next=%2Fconsole%2F%23needs"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rr = do(c.mux, authedReq(req))
	require.Equal(t, http.StatusOK, rr.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Equal(t, plans.StatusApproved, out["status"], "the JSON answer is unchanged")

	lock := func(t *testing.T, form url.Values) (*httptest.ResponseRecorder, core.RepoLock) {
		c := ndNew(t, "advanced", ndAllOn)
		l := lkSeedLock(t, c.db, "main", "/work/app", core.RepoLockRequested, "example-ops")
		return ndPost(c.mux, "/console/locks/"+l.ID+"/approve", form, false), l
	}
	rr, l := lock(t, url.Values{"next": {"/console/#needs"}})
	require.Equal(t, http.StatusSeeOther, rr.Code, rr.Body.String())
	require.Equal(t, "/console/?notice="+url.QueryEscape("Approved LOCK "+l.Label()+": agents may hold it now.")+"#needs",
		rr.Header().Get("Location"))
	for _, refused := range []url.Values{nil, {"next": {"https://example.com/x"}}} {
		rr, l = lock(t, refused)
		require.Equal(t, "/console/locks?notice="+url.QueryEscape("Approved LOCK "+l.Label()+": agents may hold it now."),
			rr.Header().Get("Location"), "next %v: Locks, as before", refused)
	}
}

// Marking the mishaps reviewed stores a stamp; the item then counts only the
// reports after it, at their true count, and is gone at zero. A mark through
// an older report writes nothing; the stamp never moves back.
func TestNdMishaps_MarkReviewedClearsTheItem(t *testing.T) {
	c := ndNew(t, "basic", config.Features{})
	now := time.Now().UTC()
	for i := range 8 {
		ndMishap(t, c.db, "example-ops", "other: report", now.Add(-time.Duration(10-i)*time.Minute))
	}
	card := ndCard(t, ndRead(t, c.mux).Attention, needMishaps)
	require.Equal(t, 8, card.Count, "the true count, not the newest six")
	require.Equal(t, "8 mishaps to review", card.Title)

	var page mishapsData
	getJSON(t, c.mux, "/console/mishaps?format=json", &page)
	require.NotNil(t, page.Review)
	require.Nil(t, page.Review.At)
	require.Equal(t, 8, page.Review.New)
	html := getHTMLBody(t, c.mux, "/console/mishaps")
	require.Contains(t, html, `<form method="post" action="/console/mishaps/reviewed">`)
	through := regexp.MustCompile(`name="through" value="([^"]+)"`).FindStringSubmatch(html)
	require.NotNil(t, through)

	rr := ndPost(c.mux, "/console/mishaps/reviewed", url.Values{"through": {through[1]}, "return": {"/console/mishaps?w=7d"}}, true)
	require.Equal(t, http.StatusNoContent, rr.Code, rr.Body.String())
	require.Equal(t, "/console/mishaps?notice="+url.QueryEscape("Marked 8 reports reviewed.")+"&w=7d", rr.Header().Get("X-Seam-Location"))
	require.NotContains(t, ndKinds(ndRead(t, c.mux).Attention), needMishaps, "nothing new since: no item")
	require.NotContains(t, getHTMLBody(t, c.mux, "/console/"), `data-need="mishaps"`)
	evs, err := events.NewRecorder(c.db).ByKinds(context.Background(), []core.EventKind{store.EventMishapsReviewed}, "", "", 5)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, "console", evs[0].Payload["by"])
	require.EqualValues(t, 8, evs[0].Payload["reports"])
	require.Equal(t, "marked 8 mishap reports reviewed", eventSummary(evs[0]))
	// The stamp is the newest report's time (three minutes old here), not
	// the mark's, so the strip dates the report rather than claiming the
	// mark was made then.
	html = getHTMLBody(t, c.mux, "/console/mishaps")
	require.Contains(t, html, "Reviewed through the newest report, from ")
	require.Contains(t, html, "3m ago</time>: nothing new since.")
	require.NotContains(t, html, "Marked reviewed")

	// A new report brings the item back, counted since the stamp.
	newID := ndMishap(t, c.db, "example-ops", "live-state: wrote to the live table", time.Now().UTC())
	card = ndCard(t, ndRead(t, c.mux).Attention, needMishaps)
	require.Equal(t, "1 mishap to review", card.Title)
	require.Contains(t, card.Sub, "latest live-state ")
	require.Contains(t, card.Sub, "since you marked them reviewed")
	html = getHTMLBody(t, c.mux, "/console/mishaps")
	require.Contains(t, html, `<span class="badge warn mishap-new"`, "the new report is marked")
	require.Equal(t, 1, strings.Count(html, "mishap-new\""), "only the new one")
	require.Contains(t, html, "new since the last one you marked reviewed, from ")
	_ = newID

	// A stale page's mark through an older report changes nothing.
	rr = ndPost(c.mux, "/console/mishaps/reviewed", url.Values{"through": {through[1]}}, false)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), url.QueryEscape("Already reviewed through that report."))
	require.Equal(t, 1, ndCard(t, ndRead(t, c.mux).Attention, needMishaps).Count)
	evs, err = events.NewRecorder(c.db).ByKinds(context.Background(), []core.EventKind{store.EventMishapsReviewed}, "", "", 5)
	require.NoError(t, err)
	require.Len(t, evs, 1, "a no-op records nothing")

	// A through the page could not have sent is refused, and a JSON caller
	// marking with no through marks everything up to now.
	rr = ndPost(c.mux, "/console/mishaps/reviewed", url.Values{"through": {"yesterday"}}, false)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	req := httptest.NewRequest(http.MethodPost, "/console/mishaps/reviewed", nil)
	req.Header.Set("Accept", "application/json")
	rr = do(c.mux, authedReq(req))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var out struct{ Reviewed, New int }
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Equal(t, 1, out.Reviewed)
	require.Zero(t, out.New)
	require.NotContains(t, ndKinds(ndRead(t, c.mux).Attention), needMishaps)
}

// Marking the mishaps reviewed is the owner's acknowledgement: an agent's
// process and a paired device are refused, and nothing is stamped.
func TestNdMishaps_MarkReviewedIsTheOwnersHands(t *testing.T) {
	c := ndNew(t, "advanced", config.Features{})
	ndMishap(t, c.db, "example-ops", "other: report", time.Now().UTC().Add(-time.Minute))
	for _, caller := range uohCallers(t) {
		if caller.owner {
			continue
		}
		rr := uohPost(t, c.mux, caller, "/console/mishaps/reviewed", "")
		uohRequireRefused(t, rr, caller, caller.name)
		_, found, err := store.MishapsReviewedAt(context.Background(), c.db)
		require.NoError(t, err)
		require.False(t, found, caller.name)
	}
}

// Work in motion is Now's model: the plans a live claim holds a step of with
// who holds them, the loose ends, and what is ready next, each with its true
// count; the live strip leads each chip with the task the agent holds.
func TestNdWorkInMotion_IsNowsModel(t *testing.T) {
	c := ndNew(t, "advanced", ndAllOn)
	s := ndFixture(t, c)
	idle := ndSession(t, c.db, "cc/nd-idle", "example-web", true)

	ov := ndRead(t, c.mux)
	m := ov.Motion
	require.NotNil(t, m)
	require.Equal(t, 1, m.PlansHeld)
	require.Equal(t, "storage", m.Plans[0].Slug)
	require.Equal(t, []nowHolderVM{{ID: s.liveID, Name: "cc/nd-live"}}, m.Plans[0].Holders)
	require.Equal(t, 2, m.LooseN, "the lapsed claim and the claimless start")
	require.GreaterOrEqual(t, m.ReadyN, 2)
	require.Equal(t, 2, m.Agents)

	var now nowData
	getJSON(t, c.mux, "/console/now?format=json", &now)
	require.Equal(t, len(now.Loose), m.LooseN, "the same model as Now")
	require.Equal(t, now.ReadyTotal, m.ReadyN)

	require.Equal(t, 2, ov.LiveTotal)
	byID := map[string]liveSessionRow{}
	for _, l := range ov.Live {
		byID[l.ID] = l
	}
	require.Equal(t, "Write the migration", byID[s.liveID].Lead, "led by the task it holds")
	require.True(t, byID[s.liveID].Held)
	require.Equal(t, "example-web", byID[idle].Lead, "else by its project")

	body := ndMain(t, getHTMLBody(t, c.mux, "/console/"))
	require.Contains(t, body, `<h2 id="motion-title">Work in motion</h2>`)
	require.Contains(t, body, `held by <a href="/console/sessions/`+s.liveID+`">cc/nd-live</a>`)
	require.Contains(t, body, `<a href="/console/tasks/`+s.lapsedID+`">Move the exporter</a>`)
	require.Contains(t, body, `<a class="ov2-live-all" href="/console/now">All 2 live`)
	// Page order: needs you, work in motion, live, vitals, projects, sky.
	order := []string{`id="needs"`, `id="motion"`, `aria-label="Sessions live now"`, `class="ov2-vitals"`,
		`aria-label="Recently active projects"`, `aria-label="Recent activity"`}
	at := -1
	for _, mark := range order {
		i := strings.Index(body, mark)
		require.Greater(t, i, at, "%s comes after the one before it", mark)
		at = i
	}
}

// The live strip names six agents and counts the rest.
func TestNdLiveStrip_CountsTheRest(t *testing.T) {
	c := ndNew(t, "advanced", config.Features{})
	for i := range 8 {
		ndSession(t, c.db, "cc/nd-agent-"+string(rune('a'+i)), "example-ops", true)
	}
	ov := ndRead(t, c.mux)
	require.Len(t, ov.Live, liveChipsShown)
	require.Equal(t, 8, ov.LiveTotal)
	require.Equal(t, 2, ov.LiveMore)
	body := getHTMLBody(t, c.mux, "/console/")
	require.Contains(t, body, `<span class="ov2-live-more">+2 more</span>`)
	require.Contains(t, body, "All 8 live")
	require.NotContains(t, body, "Watch interactions")
}

// ndHrefRe reads every link a page draws: href and data-href values.
var ndHrefRe = regexp.MustCompile(`(?:href|data-href)="([^"#?][^"]*|\?[^"]*)"`)

// At Basic, and at Standard, no link the Overview draws leads to a screen the
// sidebar hides at that level, nor to a Settings section the menu hides: the
// items, Work in motion, the live strip, the vitals, the sky's scopes and the
// activity panel name such a screen without linking to it, and the check-in
// offers only the counts its level shows.
func TestNdLinks_NoneLeadsAboveTheLevel(t *testing.T) {
	for _, lvl := range []level{levelBasic, levelStandard} {
		t.Run(lvl.String(), func(t *testing.T) {
			c := ndNew(t, lvl.String(), ndAllOn)
			s := ndFixture(t, c)
			writeMemory(t, c.mgr, core.KindGotcha, "example-ops", "nd-sky-star", "a star for the sky")
			ndMishap(t, c.db, "example-ops", "other: another", time.Now().UTC())
			c.backups.status = backup.Status{}
			body := ndMain(t, getHTMLBody(t, c.mux, "/console/"))
			links := ndHrefRe.FindAllStringSubmatch(body, -1)
			require.NotEmpty(t, links)
			for _, m := range links {
				href := strings.ReplaceAll(m[1], "&amp;", "&")
				if strings.HasPrefix(href, "?") {
					continue // the window tabs: this page
				}
				u, err := url.Parse(href)
				require.NoError(t, err)
				if sc, ok := screenForPath(knownScreens(), u.Path); ok {
					require.True(t, sc.visibleAt(ndAllOn, lvl), "%s links %s, the %s screen the %s sidebar hides", lvl, href, sc.Label, lvl)
					if sc.ID == "settings" {
						sec, found := settingsSectionByID(u.Query().Get("s"))
						require.True(t, !found || lvl >= sec.Min, "%s links %s", lvl, href)
					}
				}
			}
			// The items still say what waits; at Basic, where Locks is not
			// in the sidebar, without the link.
			if lvl == levelBasic {
				require.Contains(t, body, `<span class="ov2-attn-main">`)
			}
			require.Contains(t, body, "1 gate waits on you")
			require.Contains(t, body, `action="/console/tasks/`+s.gateID+`/gate/clear"`, "the inline action stays")
			offer := regexp.MustCompile(`data-checkin-offer="([^"]*)"`).FindStringSubmatch(body)
			require.NotNil(t, offer)
			require.Contains(t, strings.Fields(offer[1]), "mishaps")
			// Tasks is a Basic screen (plan:console-ux-pass UX 14): the
			// check-in links its count at every level.
			require.Contains(t, strings.Fields(offer[1]), "tasks")
			if lvl == levelStandard {
				require.Contains(t, body, `<div class="ov2-vital">`, "Retrieval is Advanced: the vitals that open it are plain cards")
				require.Contains(t, body, `<a class="ov2-vital" href="/console/sessions?w=24h&amp;retained=no"`)
			}
		})
	}
}

// Every new read goes through the request's handle: a locked project's gate,
// claim, plan, mishap and live agent are absent from the strip, Work in
// motion and the live strip, in HTML and JSON alike; its access request is
// named by its project only, as the Access page names it.
func TestNdLockedProjectIsAbsent(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	gate := ndTask(t, lc.db, core.Task{ProjectSlug: lockedSlug, Title: "sign " + lockedSecret, Kind: core.TaskKindGate,
		Gate: core.TaskGate{Who: "owner", What: "sign " + lockedSecret}})
	expired := now.Add(-10 * time.Minute)
	lapsed := ndTask(t, lc.db, core.Task{ProjectSlug: lockedSlug, Title: "close " + lockedSecret, Status: core.TaskInProgress,
		ClaimedBy: lc.sessID, LeaseExpiresAt: &expired})
	ndCapture(t, lc.mgr, lockedSlug, "ledger-capture", plans.StatusPresented)
	ndMishap(t, lc.db, lockedSlug, "live-state: touched "+lockedSecret, now.Add(-time.Minute))
	openGate := ndTask(t, lc.db, core.Task{ProjectSlug: openSlug, Title: "open gate", Kind: core.TaskKindGate,
		Gate: core.TaskGate{Who: "owner", What: "approve"}})
	_, err := store.CreateAccessApproval(ctx, lc.db, store.AccessRequest{Project: lockedSlug, Kind: store.AccessKindRequest,
		Lease: time.Hour, TTL: 15 * time.Minute, PeerUID: 501, PeerPID: 4242, ProcessKey: "nd-proc"}, now)
	require.NoError(t, err)

	get := func(path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr := lc.do(req)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		return rr.Body.String()
	}
	for _, path := range []string{"/console/", "/console/?format=json", "/console/mishaps", "/console/mishaps?format=json"} {
		body := get(path)
		for _, hidden := range []string{lockedSecret, gate, lapsed, lc.sessID, "cc/ledger", "ledger-capture"} {
			require.NotContains(t, body, hidden, "%s names %q", path, hidden)
		}
	}
	var ov ndOverview
	require.NoError(t, json.Unmarshal([]byte(get("/console/?format=json")), &ov))
	require.Equal(t, "/console/tasks/"+openGate, ndCard(t, ov.Attention, needGates).Href, "the open project's gate alone")
	require.Equal(t, 1, ndCard(t, ov.Attention, needGates).Count)
	require.NotContains(t, ndKinds(ov.Attention), needMishaps, "the only mishap is the locked project's")
	require.NotContains(t, ndKinds(ov.Attention), needPlans)
	require.NotContains(t, ndKinds(ov.Attention), needLapsed)
	access := ndCard(t, ov.Attention, needAccess)
	require.Equal(t, "1 access request waits for you", access.Title)
	require.Contains(t, access.Sub, "for "+lockedSlug)
}

// The health line warns while there is no backup or the newest is older
// than a week, links Settings > Backups, and says nothing while they are
// current.
func TestNdHealth_WarnsOnMissingOrOldBackups(t *testing.T) {
	c := ndNew(t, "basic", config.Features{})
	fact := func() string {
		body := getHTMLBody(t, c.mux, "/console/")
		m := regexp.MustCompile(`<a class="ov-health-fact" data-tone="warn" href="/console/settings\?s=backups"[^>]*>.*?<span>([^<]*)</span>`).FindStringSubmatch(body)
		if m == nil {
			return ""
		}
		return m[1]
	}
	require.Empty(t, fact(), "a backup an hour old is current")
	c.backups.status = backup.Status{}
	require.Equal(t, "no backup yet", fact())
	c.backups.status = backup.Status{LastBackup: &backup.Record{At: time.Now().Add(-9 * 24 * time.Hour)}}
	require.Equal(t, "newest backup 9d old", fact())
	card := ndCard(t, ndRead(t, c.mux).Attention, needBackups)
	require.Equal(t, "The newest backup is 9 days old", card.Title)
	c.backups.status = backup.Status{Settings: backup.Settings{Auto: true, Every: time.Hour}, Failures: 2,
		FailingSince: time.Now().Add(-3 * time.Hour), LastBackup: &backup.Record{At: time.Now().Add(-4 * time.Hour)}}
	card = ndCard(t, ndRead(t, c.mux).Attention, needBackups)
	require.Equal(t, "Automatic backups are failing", card.Title)
	require.Equal(t, "danger", card.Sev)
}

// The check-in's mishap chip links the Mishaps screen, and a chip links
// only a screen its level shows (shell.js reads data-checkin-offer).
func TestNdCheckin_MishapChipLinksTheMishapsScreen(t *testing.T) {
	js := string(shellJS)
	require.Contains(t, js, `[d.mishaps, 'mishap reported', 'mishaps reported', '/console/mishaps', 'mishaps']`)
	require.Contains(t, js, `box.getAttribute('data-checkin-offer')`)
	require.Contains(t, js, `offered(it[4]) ? it[3] : ''`)
}

// ownerNext keeps a next on the console, carries the flash, drops a stale
// one and keeps the fragment last; anything else is the fallback, unchanged.
func TestNdOwnerNext(t *testing.T) {
	require.Equal(t, "/console/?w=7d&notice=Done.#needs", ownerNext("/console/?w=7d&notice=old#needs", "/x", "notice", "Done."))
	require.Equal(t, "/console/mishaps?error=No.", ownerNext(" /console/mishaps ", "/x", "error", "No."))
	for _, raw := range []string{"", "   ", "/console/", "https://example.com/", "//example.com/a", "/elsewhere"} {
		require.Equal(t, "/fallback?notice=a", ownerNext(raw, "/fallback?notice=a", "notice", "b"), raw)
	}
}

// The sky sits below the projects grid and folds, remembered per browser:
// the head script applies the fold before paint, and the page script keeps
// the button's state in step after every live morph.
func TestNdSky_FoldsAndSitsBelowTheGrid(t *testing.T) {
	c := ndNew(t, "advanced", config.Features{})
	writeMemory(t, c.mgr, core.KindGotcha, "example-ops", "nd-star", "a star")
	body := getHTMLBody(t, c.mux, "/console/")
	require.Contains(t, body, `<button type="button" class="sky-fold" data-sky-fold aria-expanded="true" aria-controls="sky-body"`)
	require.Contains(t, body, `<div class="sky-body" id="sky-body">`)
	require.Less(t, strings.Index(body, `class="ov2-grid"`), strings.Index(body, `id="sky"`), "the sky is below the grid")
	require.Contains(t, body, `if(localStorage.getItem('seamless-sky')==='folded')d.setAttribute('data-sky','folded');`)
	require.Contains(t, body, "document.addEventListener('seam:content-updated', syncSky);")
	require.Contains(t, string(consoleCSS), `:root[data-sky="folded"] .sky-panel .sky-body`)
}

// The model's Kind, Count and Href are the sidebar's to read: every item
// carries a kind from the one list and a count.
func TestNdItems_CarryAKindAndACount(t *testing.T) {
	c := ndNew(t, "advanced", ndAllOn)
	ndFixture(t, c)
	known := []string{needAccess, needHeld, needLocks, needGates, needGatesOthers, needPlans, needLapsed, needUnclaimed,
		needMishaps, needUpdates, needBackups, needProposals, needAutoApplied, needStale, needFinishLine}
	for _, card := range ndRead(t, c.mux).Attention {
		require.Contains(t, known, card.Kind)
		require.Positive(t, card.Count, card.Kind)
		require.True(t, strings.HasPrefix(card.Href, "/console/"), card.Kind)
	}
}

// The others card's title names at most three holders, most gates first,
// counts a name spelled two ways once, and counts the rest: a deployment with
// gates on fourteen holders once listed all of them, and the card grew to
// twenty lines and stretched every card in the strip with it.
func TestGateHolders_NamesAFewAndCountsTheRest(t *testing.T) {
	gate := func(who string) core.Task { return core.Task{Gate: core.TaskGate{Who: who}} }
	require.Equal(t, "dana", gateHolders([]core.Task{gate("dana")}))
	require.Equal(t, "people", gateHolders([]core.Task{gate("")}), "a gate with no holder waits on people")
	require.Equal(t, "dana and lee", gateHolders([]core.Task{gate("dana"), gate("lee"), gate("Dana")}),
		"a name spelled two ways is one holder, under its first spelling")
	require.Equal(t, "lee, dana and kim", gateHolders([]core.Task{gate("dana"), gate("lee"), gate("kim"), gate("lee")}),
		"the holder with the most gates comes first; ties keep their order")
	require.Equal(t, "lee, dana and 3 others", gateHolders([]core.Task{
		gate("dana"), gate("lee"), gate("kim"), gate("lee"), gate("ash"), gate("bo"),
	}))
}

// Ready next and Up next send what they have no room for to the Tasks list
// that holds every ready task: the Overview said "+222 more on Now" while
// Now drew twelve under "Up next 12", so the promised rest was nowhere on it.
func TestNdReadyMore_SendsTheRestToTasks(t *testing.T) {
	c := ndNew(t, "advanced", ndAllOn)
	for i := range 14 {
		ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: fmt.Sprintf("Ready chore %02d", i)})
	}
	var now nowData
	getJSON(t, c.mux, "/console/now?format=json", &now)
	require.Equal(t, 14, now.ReadyTotal)
	require.Len(t, now.UpNext, 12)

	href := `/console/tasks?ready=all&amp;steps=1`
	ov := ndMain(t, getHTMLBody(t, c.mux, "/console/"))
	require.Contains(t, ov, `+11 more on <a href="`+href+`">Tasks</a>`, "three drawn of fourteen")
	nowBody := getHTMLBody(t, c.mux, "/console/now")
	require.Contains(t, nowBody, `+2 more ready &middot; <a href="`+href+`">every ready task on Tasks</a>`)
	scoped := getHTMLBody(t, c.mux, "/console/now?scope=example-ops")
	require.Contains(t, scoped, `<a href="/console/tasks?project=example-ops&amp;ready=all&amp;steps=1">`, "a scoped Now keeps its project")
}

// The strip's head counts the items its cards stand for, not the cards: three
// gates on the owner are three. A finish line is good news rather than
// something waiting on the owner, so it is not counted, and a strip of finish
// lines alone shows no number.
func TestNdStrip_HeadCountsItemsNotCards(t *testing.T) {
	c := ndNew(t, "advanced", ndAllOn)
	ndFixture(t, c)
	for _, title := range []string{"Approve the budget", "Renew the domain"} {
		ndTask(t, c.db, core.Task{ProjectSlug: "example-ops", Title: title, Kind: core.TaskKindGate,
			Gate: core.TaskGate{Who: "owner", What: "decide"}})
	}
	var ov struct {
		Attention  []attnCard
		NeedsTotal int
	}
	getJSON(t, c.mux, "/console/?format=json", &ov)
	require.Equal(t, 3, ndCard(t, ov.Attention, needGates).Count)
	ndCard(t, ov.Attention, needFinishLine)
	want, cards := 0, 0
	for _, card := range ov.Attention {
		if card.Kind != needFinishLine {
			want += card.Count
			cards++
		}
	}
	require.Equal(t, want, ov.NeedsTotal)
	require.NotEqual(t, len(ov.Attention), ov.NeedsTotal, "the fixture tells items from cards")

	body := getHTMLBody(t, c.mux, "/console/")
	require.Contains(t, body, `Needs you<strong title="`+strconv.Itoa(want)+` items on `+strconv.Itoa(cards)+
		` cards; plans near the finish line are not counted">`+strconv.Itoa(want)+`</strong>`)

	n, tip := needsTotal([]attnCard{{Kind: needFinishLine, Count: 2}})
	require.Zero(t, n)
	require.Empty(t, tip)
	n, tip = needsTotal([]attnCard{{Kind: needMishaps, Count: 1}})
	require.Equal(t, 1, n)
	require.Equal(t, "1 item on 1 card", tip)
}
