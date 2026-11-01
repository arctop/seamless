package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
)

// newGatedConsole builds a console with every optional feature at its shipped
// default (off) and seeds one trial, so every assertion below is about the gate
// rather than about an empty database.
func newGatedConsole(t *testing.T) (*sql.DB, *http.ServeMux, string) {
	t.Helper()
	db, mux := newConsoleFeatures(t, config.Features{})
	id := seedTrial(t, db, "cold-lab", "boot loop trial", core.OutcomeFail, "demo", "", nil, 1)
	return db, mux, id
}

// gatedPaths are the four routes the research feature owns.
func gatedPaths(trialID string) []string {
	return []string{"/console/labs", "/console/labs/cold-lab", "/console/trials", "/console/trials/" + trialID}
}

func TestGatedRoutes_RenderTheDisabledPageForBrowsers(t *testing.T) {
	_, mux, trialID := newGatedConsole(t)

	for _, path := range gatedPaths(trialID) {
		rr := getPeek(t, mux, path)
		require.Equal(t, http.StatusOK, rr.Code, "%s: a switched-off feature is an explanation, not an error", path)
		body := rr.Body.String()
		require.Contains(t, body, `data-feature-off="research"`, path)
		require.Contains(t, body, "Research labs &amp; trials is switched off", path)
		require.Contains(t, body, "Nothing was deleted", path)
		require.Contains(t, body, `href="/console/settings?s=features"`, path)
		// The gated screen's own content must not leak through.
		require.NotContains(t, body, "boot loop trial", path)
		require.NotContains(t, body, `id="lib-reader"`, path)
	}
}

func TestGatedRoutes_JSONCallersGet403(t *testing.T) {
	_, mux, trialID := newGatedConsole(t)

	for _, path := range gatedPaths(trialID) {
		req := httptest.NewRequest(http.MethodGet, path+"?format=json", nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr := do(mux, req)
		require.Equal(t, http.StatusForbidden, rr.Code, path)

		var payload map[string]string
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &payload), path)
		require.Equal(t, "the research feature is disabled", payload["error"], path)
	}
}

// A stale detail-pane fetch must get a fragment, not a whole console nested
// inside the pane.
func TestGatedRoutes_PeekFetchGetsAFragment(t *testing.T) {
	_, mux, trialID := newGatedConsole(t)

	rr := getPeek(t, mux, "/console/trials/"+trialID+"?peek=1")
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	require.Contains(t, body, `class="peek-entity"`)
	require.Contains(t, body, "switched off")
	require.NotContains(t, body, "<html")
}

// Gating is unauthenticated-safe: auth still decides first, so a disabled route
// never becomes a way to probe the console without the key.
func TestGatedRoutes_StillRequireAuth(t *testing.T) {
	_, mux, trialID := newGatedConsole(t)

	rr := do(mux, httptest.NewRequest(http.MethodGet, "/console/trials/"+trialID, nil))
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "/console/login")
}

func TestGatedRoutes_ServeNormallyWhenEnabled(t *testing.T) {
	db, mux := newConsoleFeatures(t, config.Features{Research: true})
	id := seedTrial(t, db, "cold-lab", "boot loop trial", core.OutcomeFail, "demo", "", nil, 1)

	for _, path := range gatedPaths(id) {
		rr := getPeek(t, mux, path)
		require.Equal(t, http.StatusOK, rr.Code, path)
		require.NotContains(t, rr.Body.String(), `data-feature-off`, path)
	}
}

// The stored override is what flips the gate at runtime -- no restart, no
// rebuild of the service.
func TestGatedRoutes_StoredOverrideFlipsTheGateLive(t *testing.T) {
	db, mux, trialID := newGatedConsole(t)
	ctx := context.Background()

	require.Equal(t, http.StatusOK, getPeek(t, mux, "/console/trials").Code)
	require.Contains(t, getPeek(t, mux, "/console/trials").Body.String(), "data-feature-off")

	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Research: true}))
	require.NotContains(t, getPeek(t, mux, "/console/trials").Body.String(), "data-feature-off",
		"the override applies to the very next render")
	require.Contains(t, getPeek(t, mux, "/console/trials/"+trialID).Body.String(), "boot loop trial")

	require.NoError(t, store.ClearFeaturesConfig(ctx, db))
	require.Contains(t, getPeek(t, mux, "/console/trials").Body.String(), "data-feature-off",
		"clearing the override falls back to the file/env base, which is off")
}

func TestNavEntries_HideWhileTheFeatureIsOff(t *testing.T) {
	db, mux, _ := newGatedConsole(t)

	page := getPeek(t, mux, "/console/").Body.String()
	require.NotContains(t, page, `href="/console/labs"`)
	require.NotContains(t, page, `href="/console/trials"`)

	require.NoError(t, store.SetFeaturesConfig(context.Background(), db, config.Features{Research: true}))
	page = getPeek(t, mux, "/console/").Body.String()
	require.Contains(t, page, `href="/console/labs"`)
	require.Contains(t, page, `href="/console/trials"`)
	require.Contains(t, page, `<span class="nav-label">Trials</span><span class="count">1</span>`,
		"the badge count returns with the entry")
}

// The badge counts are zeroed as well as hidden, so a stale or partially morphed
// template cannot leak a count for a screen that is switched off.
func TestNavCounts_ZeroWhileTheFeatureIsOff(t *testing.T) {
	db, _, _ := newGatedConsole(t)
	svc, err := New(Config{DB: db, APIKey: testKey})
	require.NoError(t, err)
	ctx := context.Background()

	off := svc.navCounts(ctx, config.Features{})
	require.Equal(t, 0, off.Labs)
	require.Equal(t, 0, off.Trials)

	on := svc.navCounts(ctx, config.Features{Research: true})
	require.Equal(t, 1, on.Labs)
	require.Equal(t, 1, on.Trials)
}

func TestSearch_TrialsScopeLeavesTheEnumWhileTheFeatureIsOff(t *testing.T) {
	db, mux, _ := newGatedConsole(t)
	ctx := context.Background()

	// The scope is not offered, not mentioned, and not silently searched.
	page := getPeek(t, mux, "/console/search?q=boot").Body.String()
	require.NotContains(t, page, `href="/console/search?scope=trials`)
	require.NotContains(t, page, "boot loop trial")
	require.NotContains(t, page, "tasks, plans, trials, projects")

	var data searchData
	getJSON(t, mux, "/console/search?q=boot&format=json", &data)
	for _, g := range data.Groups {
		require.NotEqual(t, "trials", g.Kind, "scope=all must not reach past the gate")
	}

	// An explicit ?scope=trials is a named 400, never a quiet "search everything".
	req := httptest.NewRequest(http.MethodGet, "/console/search?q=boot&scope=trials&format=json", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(),
		`invalid scope \"trials\": valid values are all, memories, notes, tasks, plans, projects, sessions`,
		"the named-values list must not advertise the gated scope")

	// Enabled: the scope, the copy, and the rows all come back.
	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Research: true}))
	page = getPeek(t, mux, "/console/search?q=boot").Body.String()
	require.Contains(t, page, `href="/console/search?scope=trials`)
	require.Contains(t, page, "boot loop trial")

	getJSON(t, mux, "/console/search?q=boot&scope=trials&format=json", &data)
	require.Equal(t, 1, data.Total)
	require.Equal(t, "trials", data.Groups[0].Kind)
}

// The favorites view is the ?fav=1 search filter: a starred trial disappears
// with the feature and returns with it, rather than leaving a dead link.
func TestSearchFavorites_TrialRowsFollowTheFeature(t *testing.T) {
	db, mux, trialID := newGatedConsole(t)
	ctx := context.Background()
	require.NoError(t, store.SetTrialFavorite(ctx, db, trialID, true))

	var data searchData
	getJSON(t, mux, "/console/search?q=boot&fav=1&format=json", &data)
	require.Equal(t, 0, data.Total, "a starred trial stays hidden while the feature is off")

	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Research: true}))
	getJSON(t, mux, "/console/search?q=boot&fav=1&format=json", &data)
	require.Equal(t, 1, data.Total)
	require.Equal(t, "trials", data.Groups[0].Kind)
}

func TestOverviewCoverage_DropsTheTrialsChannelWhileOff(t *testing.T) {
	db, mux, _ := newGatedConsole(t)

	var data overviewData
	getJSON(t, mux, "/console/?format=json", &data)
	for _, row := range data.CoverageRows {
		require.NotEqual(t, "Trials", row.Label, "a switched-off channel must not read as 0% retention")
	}

	require.NoError(t, store.SetFeaturesConfig(context.Background(), db, config.Features{Research: true}))
	getJSON(t, mux, "/console/?format=json", &data)
	labels := make([]string, 0, len(data.CoverageRows))
	for _, row := range data.CoverageRows {
		labels = append(labels, row.Label)
	}
	require.Equal(t, []string{"Findings", "Memories", "Notes", "Trials"}, labels)
}

// The finish-line card is a momentum surface: off (the shipped default) leaves
// the Overview without a trace of it; on, a near-done plan earns the strip's
// one positive card, naming the exact remaining steps and linking to the plan.
func TestOverviewFinishLineCard_FollowsTheMomentumFeature(t *testing.T) {
	db, mux, _ := newGatedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seed := func(plan, title string, status core.TaskStatus, i int) {
		t.Helper()
		at := now.Add(time.Duration(i) * time.Minute)
		require.NoError(t, store.CreateTask(ctx, db, core.Task{
			ID: mustID(t), ProjectSlug: "demo", Title: title, Status: status,
			PlanSlug: plan, CreatedAt: at, UpdatedAt: at,
		}))
	}
	for i := range 4 {
		seed("nearly", "closed step", core.TaskDone, i)
	}
	seed("nearly", "write the docs", core.TaskOpen, 10)

	page := getPeek(t, mux, "/console/").Body.String()
	require.NotContains(t, page, "from shipped")
	require.NotContains(t, page, `data-sev="ok"`)
	require.NotContains(t, page, "mom-fin-bar", "off leaves no trace of the progress draw either")

	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Momentum: true}))
	page = getPeek(t, mux, "/console/").Body.String()
	require.Contains(t, page, "nearly -- one step from shipped")
	require.Contains(t, page, "left: write the docs")
	require.Contains(t, page, `href="/console/plans/nearly"`)
	require.Contains(t, page, `data-sev="ok"`)
	require.Contains(t, page, `class="mom-fin-bar"`, "the card carries the progress draw")
	require.Contains(t, page, `<line class="mom-fin-fill" x1="0" x2="80" y1="2" y2="2" pathLength="1"/>`,
		"the fill's width is the rollup's real done/total share")
	require.Contains(t, page, "<title>4 of 5 steps done -- 80%</title>",
		"the percent is verifiable on the surface, done and total named verbatim")

	// A plan under the line earns nothing even with the feature on: no card is
	// the empty state.
	seed("midway", "first of many", core.TaskOpen, 20)
	seed("midway", "second of many", core.TaskDone, 21)
	page = getPeek(t, mux, "/console/").Body.String()
	require.NotContains(t, page, "midway")
}

// The maturity-stage pills are a momentum surface on the project board and
// the detail header: off (the shipped default), no computation, no latch, no
// markup; on, both surfaces show the same latched stage and the crossing is
// minted as an event exactly once.
func TestProjectStages_FollowTheMomentumFeature(t *testing.T) {
	db, mux, _ := newGatedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	projID := mustID(t)
	require.NoError(t, store.CreateProject(ctx, db, core.Project{ID: projID, Slug: "demo", Name: "demo"}))
	_, err := db.ExecContext(ctx, `UPDATE projects SET created_at = ? WHERE slug = 'demo'`,
		core.FormatTime(now.AddDate(0, 0, -10)))
	require.NoError(t, err)
	for range 30 {
		_, err = db.ExecContext(ctx, `
			INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
			VALUES (?, ?, 'tool.call', '', 'demo', '', '{}')`,
			mustID(t), core.FormatTime(now.Add(-time.Hour)))
		require.NoError(t, err)
	}
	for i := range 5 {
		stamp := core.FormatTime(now)
		_, err = db.ExecContext(ctx, `
			INSERT INTO memories_index
			    (id, kind, name, description, project, file_path, tags, valid_from,
			     invalid_at, superseded_by, source_session, content_hash, created_at, updated_at)
			VALUES (?, 'gotcha', ?, 'd', 'demo', ?, '[]', ?, NULL, NULL, '', 'h', ?, ?)`,
			mustID(t), fmt.Sprintf("mem-%d", i), fmt.Sprintf("memory/demo/mem-%d.md", i),
			stamp, stamp, stamp)
		require.NoError(t, err)
	}
	stageEvents := func() int {
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?`,
			string(core.EventProjectStage)).Scan(&n))
		return n
	}

	require.NotContains(t, getPeek(t, mux, "/console/projects").Body.String(), "mom-stage")
	require.Zero(t, stageEvents(), "off means no computation and no latch")

	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Momentum: true}))
	board := getPeek(t, mux, "/console/projects").Body.String()
	require.Contains(t, board, `data-stage="sprouting"`)
	require.Contains(t, board, "sprouting -- established at 30d, 15 memories, 250 events",
		"the pill's tooltip names the next stage's bars beside where the project stands")
	require.Equal(t, 1, stageEvents(), "the crossing is minted exactly once")

	detail := getPeek(t, mux, "/console/projects/demo").Body.String()
	require.Contains(t, detail, `data-stage="sprouting"`, "the detail header shows the board's pill")
	require.Equal(t, 1, stageEvents(), "re-rendering mints nothing new")
}

// The first-reuse ledger line is the ideation copy, composed from the event
// payload -- and it doubles as the SSE toast text, so the shape is pinned.
func TestEventSummary_FirstReuse(t *testing.T) {
	line := eventSummary(core.Event{
		Kind:    core.EventMemoryFirstReuse,
		Payload: map[string]any{"name": "chroma-boot-race", "kind": "gotcha"},
	})
	require.Equal(t, "gotcha chroma-boot-race just paid off for the first time", line)

	require.Equal(t, "a memory just paid off for the first time",
		eventSummary(core.Event{Kind: core.EventMemoryFirstReuse}),
		"a payload-less mark still reads as a sentence")
}

// The ledger glint rides the two momentum-born celebration kinds: each carries
// its own distinct icon, and the Overview ledger row wears the mom-glint class
// the live-arrival wash keys on. The wash itself is client-side (only nodes
// the SSE morph inserts carry .live-in), so what is pinned here is the server
// side: the class on the row and the icon mapping.
func TestLedgerGlint_MomentumBornKindsCarryTheHook(t *testing.T) {
	require.Equal(t, "zap", evtIcon(string(core.EventMemoryFirstReuse)))
	require.Equal(t, "trending-up", evtIcon(string(core.EventProjectStage)))
	require.NotEqual(t, evtIcon(string(core.EventMemoryFirstReuse)), evtIcon("session.started"),
		"the payoff row does not blur into the generic stream glyphs")

	db, mux, _ := newGatedConsole(t)
	ctx := context.Background()

	page := getPeek(t, mux, "/console/").Body.String()
	require.NotContains(t, page, "mom-glint", "no celebration event, no glint hook anywhere")

	// The kinds are only minted while momentum is on; the glint is presentation
	// on those rows, keyed to the kind rather than re-gated on the feature.
	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Momentum: true}))
	_, err := db.ExecContext(ctx, `
		INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES (?, ?, ?, '', 'demo', '', '{"name":"chroma-boot-race","kind":"gotcha"}')`,
		mustID(t), core.FormatTime(time.Now().UTC()), string(core.EventMemoryFirstReuse))
	require.NoError(t, err)

	page = getPeek(t, mux, "/console/").Body.String()
	require.Contains(t, page, `class="ov2-evt mom-glint"`, "the payoff row wears the glint hook")
	require.Contains(t, page, "just paid off for the first time")
}

// The milestone rows are a momentum surface in the activity ledger: off (the
// shipped default) filters even a stale milestone.reached event -- one minted
// before a later switch-off -- out of the business feeds; on, the row renders
// the payload's claim verbatim under its own award glyph, wearing the glint
// hook the live-arrival wash keys on. Milestones accumulate in the ledger
// only: no screen of their own.
func TestLedgerMilestones_FollowTheMomentumFeature(t *testing.T) {
	db, mux, _ := newGatedConsole(t)
	ctx := context.Background()

	// A milestone minted while momentum was on, surviving in the log after the
	// feature went back off (nothing is ever deleted).
	_, err := db.ExecContext(ctx, `
		INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES (?, ?, ?, '', 'demo', 'memories-100:demo',
		        '{"claim":"100 memories written in demo","project":"demo","count":103,"threshold":100}')`,
		mustID(t), core.FormatTime(time.Now().UTC()), string(store.EventMilestoneReached))
	require.NoError(t, err)

	offPage := getPeek(t, mux, "/console/").Body.String()
	require.NotContains(t, offPage, "milestone.reached", "off = no milestone rows, even stale ones")
	require.NotContains(t, offPage, "100 memories written in demo")
	require.NotContains(t, getJSON2(t, mux, "/console/?format=json"), "milestone.reached")

	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Momentum: true}))
	page := getPeek(t, mux, "/console/").Body.String()
	require.Contains(t, page, "milestone.reached")
	require.Contains(t, page, "100 memories written in demo",
		"the row's sentence is the payload's claim, verbatim")
	require.Contains(t, page, `class="ov2-evt mom-glint"`, "the milestone row wears the glint hook")

	// The distinct glyph, mirrored client-side by interactions.js.
	require.Equal(t, "award", evtIcon(string(store.EventMilestoneReached)))
	require.NotEqual(t, evtIcon(string(store.EventMilestoneReached)), evtIcon("session.started"),
		"the milestone row does not blur into the generic stream glyphs")
	require.NotEqual(t, evtIcon(string(store.EventMilestoneReached)), evtIcon(string(core.EventPlanShipped)))

	// A payload-less row still reads as a sentence rather than an empty line.
	require.Equal(t, "milestone reached", eventSummary(core.Event{Kind: store.EventMilestoneReached}))

	// Off again: the ledger is byte-identical to the pre-enable render -- the
	// stale row leaves no residue.
	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{}))
	require.Equal(t, offPage, getPeek(t, mux, "/console/").Body.String(),
		"off = byte-identical Overview")
}

// The unlocked-date line on the Settings utility-activation table is a
// momentum surface: on, a scope's stored activation timestamp reads as the
// witnessed "unlocked <date>" note; off keeps today's operational
// "armed <ago>" note -- the same timestamp, no celebration vocabulary, and a
// page byte-for-byte free of the momentum markup.
func TestSettingsUtilityUnlocked_FollowsTheMomentumFeature(t *testing.T) {
	db, mux, _ := newGatedConsole(t)
	ctx := context.Background()

	readyAt := time.Date(2026, 7, 12, 9, 30, 0, 0, time.UTC)
	require.NoError(t, store.SetUtilityActivation(ctx, db, store.UtilityActivation{
		Projects: map[string]store.UtilityProjectState{"demo": {ReadyAt: &readyAt}},
	}))

	page := getPeek(t, mux, "/console/settings?s=engine").Body.String()
	require.NotContains(t, page, "mom-unlocked", "off leaves no trace of the unlock notice")
	require.NotContains(t, page, "unlocked 2026-07-12")
	require.Contains(t, page, ">armed ", "the operational armed note is core metadata and stays")

	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Momentum: true}))
	page = getPeek(t, mux, "/console/settings?s=engine").Body.String()
	require.Contains(t, page, `class="mom-unlocked"`)
	require.Contains(t, page, "unlocked 2026-07-12",
		"the stored activation date is witnessed on the surface, verbatim")
	require.Contains(t, page, `title="2026-07-12 09:30 UTC"`, "the full timestamp stays in the tooltip")
	require.NotContains(t, page, ">armed ",
		"the note upgrades rather than duplicates -- one timestamp, one line")
}

// The memory-of-the-month spotlight is a momentum surface on the Overview
// rail: off (the shipped default), no trace in HTML or JSON; on, the window's
// top-utility memory renders with its counts, or the honest empty state when
// nothing qualifies.
func TestOverviewSpotlight_FollowsTheMomentumFeature(t *testing.T) {
	db, mux, _ := newGatedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	memID := mustID(t)
	stamp := core.FormatTime(now)
	_, err := db.ExecContext(ctx, `
		INSERT INTO memories_index
		    (id, kind, name, description, project, file_path, tags, valid_from,
		     invalid_at, superseded_by, source_session, content_hash, created_at, updated_at)
		VALUES (?, 'gotcha', 'star-memory', 'd', 'demo',
		        'memory/demo/star-memory.md', '[]', ?, NULL, NULL, '', 'h', ?, ?)`,
		memID, stamp, stamp, stamp)
	require.NoError(t, err)
	for i, sess := range []string{"s1", "s2"} {
		evID := mustID(t)
		_, err = db.ExecContext(ctx, `
			INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
			VALUES (?, ?, ?, ?, 'demo', ?, '{}')`,
			evID, core.FormatTime(now.Add(-time.Duration(i+1)*time.Hour)),
			string(core.EventMemoryRead), sess, memID)
		require.NoError(t, err)
	}

	page := getPeek(t, mux, "/console/").Body.String()
	require.NotContains(t, page, "Memory of the month")
	require.NotContains(t, getJSON2(t, mux, "/console/?format=json"), "spotlight")

	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Momentum: true}))
	page = getPeek(t, mux, "/console/").Body.String()
	require.Contains(t, page, "Memory of the month")
	require.Contains(t, page, "star-memory")
	require.Contains(t, page, "2 reads · 2 sessions · last 30 days")
	require.Contains(t, page, `href="/console/memories/`+memID+`"`)

	// The honest empty state: the feature on with nothing qualifying.
	_, err = db.ExecContext(ctx, `DELETE FROM events WHERE kind = ?`, string(core.EventMemoryRead))
	require.NoError(t, err)
	page = getPeek(t, mux, "/console/").Body.String()
	require.Contains(t, page, "Memory of the month")
	require.Contains(t, page, "No memory earned reuse in the last 30 days yet.")
}

// The capture calendar is a momentum surface on the Sessions page: off (the
// shipped default), the page carries no trace of it in HTML or JSON; on, the
// grid renders with the streak numbers, live from the stored override.
func TestSessionsCaptureCalendar_FollowsTheMomentumFeature(t *testing.T) {
	db, mux, _ := newGatedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	sessID := mustID(t)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: sessID, Name: "cc/covered", ProjectSlug: "demo", Status: core.SessionCompleted,
		Findings: "left something durable", CreatedAt: now, UpdatedAt: now,
	}))

	page := getPeek(t, mux, "/console/sessions").Body.String()
	require.NotContains(t, page, "mom-cal")
	require.NotContains(t, page, "Capture calendar")
	require.NotContains(t, getJSON2(t, mux, "/console/sessions?format=json"), "captureStreak")

	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Momentum: true}))
	page = getPeek(t, mux, "/console/sessions").Body.String()
	require.Contains(t, page, "Capture calendar")
	require.Contains(t, page, `class="mom-cal-grid"`)
	require.Contains(t, page, "mom-cal-dot", "the covered day carries its mark")
	require.Contains(t, page, "day capture streak")
	require.Contains(t, page, `class="mom-cal-col" style="--d:0ms"`, "week columns carry the staggered entry-wave delay")
	require.Contains(t, page, ` today"`, "the final bucket is today's breathing cell")
	require.Contains(t, page, "data-day=", "cells carry their dates for the ?day drill-down")
	require.Contains(t, page, "mom-cal-ramp", "the header key restates the intensity ramp")
	require.NotContains(t, page, "mom-cal-ember", "one covered day is below the ember floor")
	require.NotContains(t, page, "mom-cal-day", "no day chip while no day is focused")
	require.Contains(t, getJSON2(t, mux, "/console/sessions?format=json"), `"captureStreak"`)

	// Focusing a day (a calendar cell click) outlines its cell and raises the
	// clearable chip; today's session was created today, so its cell is both.
	day := time.Now().Local().Format("2006-01-02")
	page = getPeek(t, mux, "/console/sessions?day="+day).Body.String()
	require.Contains(t, page, ` sel`, "the focused day's cell wears the outline")
	require.Contains(t, page, `class="mom-cal-day"`, "the chip names the focused day")
	require.Contains(t, page, "Clear the day focus", "the chip carries the way back out")
}

// The streak ember is presentation on the calendar's already-judged current
// streak: at StreakEmberFloor covered days the number gains the warm pulsing
// flame (the now-hot.on pattern), below the floor there is no ember at all
// (the calendar test above pins that), and off the whole calendar -- ember
// included -- leaves no trace.
func TestSessionsStreakEmber_LightsAtTheFloor(t *testing.T) {
	db, mux, _ := newGatedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// StreakEmberFloor consecutive covered days ending today, seeded before the
	// first render so the streak cache finalizes them in one pass.
	for i := range StreakEmberFloor {
		day := now.AddDate(0, 0, -i)
		require.NoError(t, store.CreateSession(ctx, db, core.Session{
			ID: mustID(t), Name: fmt.Sprintf("cc/day-%d", i), ProjectSlug: "demo",
			Status: core.SessionCompleted, Findings: "left something durable",
			CreatedAt: day, UpdatedAt: day,
		}))
	}

	page := getPeek(t, mux, "/console/sessions").Body.String()
	require.NotContains(t, page, "mom-cal", "off keeps the whole calendar out, ember included")

	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Momentum: true}))
	page = getPeek(t, mux, "/console/sessions").Body.String()
	require.Contains(t, page, "mom-cal-ember", "at the floor the ember lights")
	require.Contains(t, page, fmt.Sprintf("<strong>%d</strong>", StreakEmberFloor),
		"the streak number stays verbatim beside the glyph")
	require.Contains(t, page, fmt.Sprintf("the ember lights at %d", StreakEmberFloor),
		"the tooltip names the floor the run cleared")
}

// The plan-shipped settle and the monthly shipped count are momentum surfaces
// on the Plans page: off (the shipped default), the page and its JSON carry no
// trace and no settlement is computed or minted -- even with the completing
// transition recorded; on, the settlement is minted exactly once across
// repeated rollup recomputes, the header gains the quiet count line, and the
// shipped plan's done row wears the settle hook the live-arrival wash keys on.
func TestPlansShipped_FollowsTheMomentumFeature(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	seeded := time.Now().UTC().Add(-2 * time.Hour) // fixed in the past so ago() stays stable across renders

	seedPlanNoteAt(t, mgr, "narrative-ship-it", "ship-it", seeded)
	step := func(status core.TaskStatus) string {
		id := mustID(t)
		require.NoError(t, store.CreateTask(ctx, db, core.Task{
			ID: id, ProjectSlug: "demo", Title: "step", Status: status,
			CreatedBy: "test", PlanSlug: "ship-it", CreatedAt: seeded, UpdatedAt: seeded,
		}))
		return id
	}
	step(core.TaskDone)
	last := step(core.TaskOpen)
	_, err := db.ExecContext(ctx, `UPDATE tasks SET status = 'done' WHERE id = ?`, last)
	require.NoError(t, err)

	// The detection chokepoint is the recorder's post-insert hook, so shipping
	// via any path that records the transition counts -- this is the exact shape
	// the MCP tasks_update tool records. Armed on the off base: the stored
	// override decides live.
	rec := events.NewRecorder(db)
	rec.SetFeatures(config.Features{})
	transition := core.Event{Kind: core.EventTaskTransition, ProjectSlug: "demo",
		ItemID: last, Payload: map[string]any{"to": "done"}}
	shippedCount := func() int {
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ?`,
			string(core.EventPlanShipped)).Scan(&n))
		return n
	}

	_, err = rec.Record(ctx, transition)
	require.NoError(t, err)
	require.Zero(t, shippedCount(), "off = not computed, no event minted")

	offPage := getPeek(t, mux, "/console/plans?w=all").Body.String()
	require.NotContains(t, offPage, "Plans shipped this month")
	require.NotContains(t, offPage, "mom-shipped")
	require.NotContains(t, offPage, "mom-settle")
	offJSON := getJSON2(t, mux, "/console/plans?format=json&w=all")
	require.NotContains(t, offJSON, "shippedThisMonth")
	require.NotContains(t, offJSON, `"shipped":true`)

	// On via the stored override: the next recompute mints, and only once.
	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Momentum: true}))
	_, err = rec.Record(ctx, transition)
	require.NoError(t, err)
	_, err = rec.Record(ctx, transition)
	require.NoError(t, err)
	require.Equal(t, 1, shippedCount(), "repeated rollup recomputes mint exactly once")

	page := getPeek(t, mux, "/console/plans?w=all").Body.String()
	require.Contains(t, page, "Plans shipped this month: <strong>1</strong>")
	require.Contains(t, page, "mom-settle", "the shipped plan's done row wears the settle hook")
	var onData plansData
	getJSON(t, mux, "/console/plans?format=json&w=all", &onData)
	require.NotNil(t, onData.ShippedMonth)
	require.Equal(t, 1, onData.ShippedMonth.Count)
	require.Len(t, onData.Rows, 1)
	require.True(t, onData.Rows[0].Shipped)

	// The settlement's ledger line and glyph: its own flag, not the capture map.
	shipped, err := rec.ByKinds(ctx, []core.EventKind{core.EventPlanShipped}, "", "", 1)
	require.NoError(t, err)
	require.Len(t, shipped, 1)
	require.Equal(t, "shipped plan ship-it (2 steps)", eventSummary(shipped[0]))
	require.Equal(t, "flag", evtIcon(string(core.EventPlanShipped)))
	require.NotEqual(t, evtIcon(string(core.EventPlanCaptured)), evtIcon(string(core.EventPlanShipped)))

	// Switching off again leaves the page byte-identical to the pre-enable
	// render: no computation, no residue, even though the settlement now exists.
	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Momentum: false}))
	require.Equal(t, offPage, getPeek(t, mux, "/console/plans?w=all").Body.String(),
		"off = byte-identical Plans page")
}

// getJSON2 fetches a path and returns the raw JSON body as a string.
func getJSON2(t *testing.T, mux *http.ServeMux, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	return rr.Body.String()
}

func TestSessionDetail_TrialsSectionFollowsTheFeature(t *testing.T) {
	db, mux, _ := newGatedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	sessID := mustID(t)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: sessID, Name: "cc/lab", ProjectSlug: "demo", Status: core.SessionActive,
		CreatedAt: now, UpdatedAt: now,
	}))
	seedTrial(t, db, "cold-lab", "session trial", core.OutcomePass, "demo", sessID, nil, 2)

	var detail sessionDetail
	getJSON(t, mux, "/console/sessions/"+sessID+"?format=json", &detail)
	require.Empty(t, detail.Trials, "the query is skipped entirely while the feature is off")

	page := getPeek(t, mux, "/console/sessions/"+sessID).Body.String()
	require.NotContains(t, page, "Trials recorded")
	require.NotContains(t, page, "session trial")
	peek := getPeek(t, mux, "/console/sessions/"+sessID+"?peek=1").Body.String()
	require.NotContains(t, peek, "trial")

	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Research: true}))
	getJSON(t, mux, "/console/sessions/"+sessID+"?format=json", &detail)
	require.Len(t, detail.Trials, 1)
	require.Contains(t, getPeek(t, mux, "/console/sessions/"+sessID).Body.String(), "Trials recorded")
	require.Contains(t, getPeek(t, mux, "/console/sessions/"+sessID+"?peek=1").Body.String(), "1 trial")
}

// The event log is an audit record, not a feature surface: a historical
// "recorded trial ..." line stays visible whether or not the feature is on. A
// straggler that links into a gated route lands on the disabled page, which is
// exactly why that page exists.
func TestEventLog_KeepsHistoricalTrialLinesWhileOff(t *testing.T) {
	db, mux, _ := newGatedConsole(t)
	ctx := context.Background()

	id, err := events.NewRecorder(db).Record(ctx, core.Event{
		Kind: core.EventTrialRecorded, ProjectSlug: "demo",
		Payload: map[string]any{"title": "boot loop trial", "lab": "cold-lab"},
	})
	require.NoError(t, err)

	var overview overviewData
	getJSON(t, mux, "/console/?format=json", &overview)
	var summaries []string
	for _, row := range overview.Recent {
		summaries = append(summaries, row.Summary)
	}
	require.Contains(t, summaries, "recorded trial boot loop trial",
		"the event log is an audit record, not a feature surface")

	// The entry is still readable, and its link into a gated screen lands on the
	// disabled page rather than a dead end -- which is why that page exists.
	require.Equal(t, http.StatusOK, getPeek(t, mux, "/console/events/"+id).Code)
}

func TestFeatureWhenOff_GeneratedFromTheRegistry(t *testing.T) {
	f, ok := features.Get(features.Research)
	require.True(t, ok)

	line := featureWhenOff(f)
	require.Equal(t,
		"Hides the Labs & Trials screens, the trials search scope, and 3 agent tools (lab_open, trial_record, trial_query).",
		line)

	// Every tool the registry claims is named, so the line cannot drift from
	// what the MCP filter actually hides.
	for _, tool := range f.Tools {
		require.Contains(t, line, tool)
	}
}

func TestFeatureCards_RenderOnePerRegistryEntry(t *testing.T) {
	cards := featureCards(config.Features{}, navCounts{Labs: 3, Trials: 12})
	require.Len(t, cards, len(features.Registry()))

	research := cards[0]
	require.Equal(t, string(features.Research), research.Key)
	require.Equal(t, "feature_research", research.Field)
	require.False(t, research.Enabled)
	require.False(t, research.Default, "optional features ship off")
	require.Equal(t, "Data kept: 12 trials across 3 labs.", research.DataKept)

	// No data yet reads as "no data yet", not as a confident zero-count line.
	empty := featureCards(config.Features{}, navCounts{})
	require.Equal(t, "No trial data yet.", empty[0].DataKept)

	on := featureCards(config.Features{Research: true}, navCounts{Labs: 1, Trials: 1})
	require.True(t, on[0].Enabled)
	require.Equal(t, "Data kept: 1 trial across 1 lab.", on[0].DataKept)

	momentum := cards[1]
	require.Equal(t, string(features.Momentum), momentum.Key)
	require.Equal(t, "feature_momentum", momentum.Field)
	require.False(t, momentum.Enabled)
	require.False(t, momentum.Default, "optional features ship off")
	require.Empty(t, momentum.DataKept, "no reassurance line until the feature owns stored data")
}

// The surfaces mechanism: a feature whose surfaces live inside existing pages
// describes them verbatim on its card, ahead of any screens or tools.
func TestFeatureWhenOff_NamesInPageSurfaces(t *testing.T) {
	line := featureWhenOff(features.Feature{
		Surfaces: []string{"the finish-line card on Overview", "the activity calendar"},
	})
	require.Equal(t, "Hides the finish-line card on Overview and the activity calendar.", line)
}

func TestSettingsFeaturesZone_MarkupContract(t *testing.T) {
	_, mux, _ := newGatedConsole(t)

	page := getPeek(t, mux, "/console/settings?s=features")
	require.Equal(t, http.StatusOK, page.Code)
	body := page.Body.String()

	require.Contains(t, body, `<section class="settings-zone settings-section" id="features">`)
	require.Contains(t, body, `href="/console/settings?s=features"`, "the sub-nav carries the section")
	require.Contains(t, body, "Optional features are off until you turn them on.")
	require.Contains(t, body, "nothing is ever deleted")
	require.Contains(t, body, "agents pick the change up on their next session")
	require.Contains(t, body, `data-feature="research"`)
	require.Contains(t, body, `data-feature="momentum"`)
	require.Contains(t, body, `action="/console/settings/features"`)
	require.Contains(t, body, `data-features-form`)
	require.Contains(t, body, `name="feature_research"`)
	require.Contains(t, body, `name="feature_momentum"`)
	require.Contains(t, body, `class="brief-switch"`, "the toggle uses the shared switch markup")
	require.Contains(t, body, "Disabled", "the state pill says which way the switch is")
	require.Contains(t, body, "3 agent tools (lab_open, trial_record, trial_query)")
	require.Contains(t, body, "Data kept: 1 trial across 1 lab.",
		"the reassurance line reports the data of a feature that is currently off")
	require.NotContains(t, body, `action="/console/settings/features/reset"`,
		"reset is offered only while a stored override exists")
	require.Contains(t, body, "Following file + env", "the precedence line says where the switches come from")

	// No numbered zones any more: the numbers implied a sequence that never existed.
	require.NotContains(t, body, `class="settings-zone-index"`)
}

func TestSettingsFeaturesZone_ShowsLiveDataAndOverrideState(t *testing.T) {
	db, mux, _ := newGatedConsole(t)
	require.NoError(t, store.SetFeaturesConfig(context.Background(), db, config.Features{Research: true}))

	body := getPeek(t, mux, "/console/settings?s=features").Body.String()
	require.Contains(t, body, "Data kept: 1 trial across 1 lab.")
	require.Contains(t, body, "Enabled")
	require.Contains(t, body, `action="/console/settings/features/reset"`)
	require.Contains(t, body, "Stored override active",
		"the precedence line says stored override -- the grandfather migration can be the writer")
	require.Contains(t, body, "Reset to file + env")
}

func TestSettingsFeaturesSaveAndReset(t *testing.T) {
	db, mux, _ := newGatedConsole(t)
	ctx := context.Background()

	// Save: the checkbox is present -> the feature is on, stored as an override.
	rr := postForm(mux, "/console/settings/features", url.Values{"feature_research": {"1"}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")
	require.Contains(t, rr.Header().Get("Location"), "#features")

	var data settingsData
	getJSON(t, mux, "/console/settings?format=json", &data)
	require.True(t, data.FeaturesOverridden)
	require.True(t, data.FeaturesConfig.Research)
	require.True(t, data.Features[0].Enabled)

	cfg, overridden, err := store.FeaturesConfig(ctx, db, config.Features{})
	require.NoError(t, err)
	require.True(t, overridden)
	require.True(t, cfg.Research)

	// The gated route follows immediately.
	require.NotContains(t, getPeek(t, mux, "/console/trials").Body.String(), "data-feature-off")

	// Save with the box absent = off. This is the full-struct rebuild: an
	// unchecked box submits nothing, so "leave it alone" must not be the answer.
	rr = postForm(mux, "/console/settings/features", "")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	getJSON(t, mux, "/console/settings?format=json", &data)
	require.True(t, data.FeaturesOverridden, "an all-off save is still a stored override")
	require.False(t, data.FeaturesConfig.Research)
	require.Contains(t, getPeek(t, mux, "/console/trials").Body.String(), "data-feature-off")

	// Reset deletes the row: back to the file/env base, which is off.
	rr = postForm(mux, "/console/settings/features/reset", "")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")
	getJSON(t, mux, "/console/settings?format=json", &data)
	require.False(t, data.FeaturesOverridden)
	require.False(t, data.FeaturesConfig.Research)

	_, found, err := store.GetSetting(ctx, db, store.SettingFeaturesConfig)
	require.NoError(t, err)
	require.False(t, found, "reset is a row deletion, not a stored all-off")
}

// The momentum toggle stores independently of research, and the resolved state
// reaches the render context (pageData.Features) on the very next request --
// which is the mechanism every in-page momentum surface gates on.
func TestSettingsFeaturesSave_MomentumTogglesIndependently(t *testing.T) {
	db, mux, _ := newGatedConsole(t)
	ctx := context.Background()

	rr := postForm(mux, "/console/settings/features", url.Values{"feature_momentum": {"1"}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)

	var data settingsData
	getJSON(t, mux, "/console/settings?format=json", &data)
	require.True(t, data.FeaturesConfig.Momentum)
	require.False(t, data.FeaturesConfig.Research, "the momentum box alone leaves research off")
	require.True(t, data.Features[1].Enabled)

	svc, err := New(Config{DB: db, APIKey: testKey})
	require.NoError(t, err)
	require.True(t, svc.effectiveFeatures(ctx).Momentum,
		"a fresh resolve sees the stored override without any restart")
}

// Reset restores whatever the file/env layer says, which is not necessarily off.
func TestSettingsFeaturesReset_FallsBackToTheFileBase(t *testing.T) {
	db, mux := newConsoleFeatures(t, config.Features{Research: true})
	ctx := context.Background()

	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{}))
	require.Contains(t, getPeek(t, mux, "/console/trials").Body.String(), "data-feature-off")

	rr := postForm(mux, "/console/settings/features/reset", "")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.NotContains(t, getPeek(t, mux, "/console/trials").Body.String(), "data-feature-off")
}

func TestSettingsFeaturesSave_RejectsUninterpretableFields(t *testing.T) {
	_, mux, _ := newGatedConsole(t)

	for _, body := range []string{
		"feature_research=maybe",                // present but not the checkbox value
		"feature_reserach=1",                    // typo'd feature name
		"feature_research=1&feature_research=1", // submitted twice
	} {
		rr := postForm(mux, "/console/settings/features", body)
		require.Equal(t, http.StatusSeeOther, rr.Code, body)
		require.Contains(t, rr.Header().Get("Location"), "error=", body)
	}

	var data settingsData
	getJSON(t, mux, "/console/settings?format=json", &data)
	require.False(t, data.FeaturesOverridden, "a rejected form stores nothing")
}

// A features save is an owner action on what agents can reach, so it lands in
// the event log with attribution like every other one.
func TestSettingsFeaturesSave_RecordsAnEvent(t *testing.T) {
	db, mux, _ := newGatedConsole(t)

	rr := postForm(mux, "/console/settings/features", url.Values{"feature_research": {"1"}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)

	evs, err := events.NewRecorder(db).RecentExcluding(context.Background(), 10)
	require.NoError(t, err)
	var found *core.Event
	for i := range evs {
		if evs[i].Kind == eventFeaturesChanged {
			found = &evs[i]
			break
		}
	}
	require.NotNil(t, found, "the toggle must be visible in Activity")
	require.Equal(t, "console", found.Payload["by"])
	require.Equal(t, "optional features changed (research on, momentum off, gamification off, screening off)", eventSummary(*found))

	rr = postForm(mux, "/console/settings/features/reset", "")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	evs, err = events.NewRecorder(db).RecentExcluding(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, "optional features reset to the file configuration", eventSummary(evs[0]))
}

// The settings JSON is the contract `seam doctor` reads to decide how many MCP
// tools a daemon should expose, so its field names are pinned here.
func TestSettingsJSON_ExposesTheFeatureContract(t *testing.T) {
	_, mux, _ := newGatedConsole(t)

	req := httptest.NewRequest(http.MethodGet, "/console/settings?format=json", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	require.Contains(t, raw, "features")
	require.Contains(t, raw, "featuresConfig")
	require.Contains(t, raw, "featuresOverridden")

	var cards []struct {
		Key     string   `json:"key"`
		Label   string   `json:"label"`
		Enabled bool     `json:"enabled"`
		Default bool     `json:"default"`
		Tools   []string `json:"tools"`
		Field   string   `json:"field"`
		WhenOff string   `json:"whenOff"`
	}
	require.NoError(t, json.Unmarshal(raw["features"], &cards))
	require.Len(t, cards, len(features.Registry()))
	require.Equal(t, "research", cards[0].Key)
	require.Equal(t, "Research labs & trials", cards[0].Label)
	require.False(t, cards[0].Enabled)
	require.Equal(t, []string{"lab_open", "trial_record", "trial_query"}, cards[0].Tools)
	require.Equal(t, "feature_research", cards[0].Field)
	require.NotEmpty(t, cards[0].WhenOff)

	require.JSONEq(t, `{"research": false, "momentum": false, "gamification": false, "screening": false}`, string(raw["featuresConfig"]))
	require.Equal(t, "false", string(raw["featuresOverridden"]))
}

func TestFeatureStyles_ZoneAndDisabledPage(t *testing.T) {
	css := string(consoleCSS)

	require.Contains(t, css, ".features-workbench")
	require.Contains(t, css, ".features-grid")
	require.Contains(t, css, ".feature-card")
	require.Contains(t, css, ".feature-off")
	require.Contains(t, css, ".settings-subnav", "Settings is one section at a time, with a sub-nav")
}

// The features form rides the same snapshot-based dirty tracker the briefing
// form uses, and the page stays opted out of the live SSE morph so a half-filled
// form is never clobbered.
func TestSettingsFeaturesForm_DirtyStateAndNoLiveRefresh(t *testing.T) {
	source, err := templateFS.ReadFile("templates/settings.html")
	require.NoError(t, err)
	page := string(source)

	require.Contains(t, page, "window.SEAM_NO_LIVE_REFRESH = true")
	require.Contains(t, page, "enhanceDirtyForm('[data-features-form]', 'features-state'")
	require.Contains(t, page, "enhanceDirtyForm('[data-briefing-form]', 'brief-state'")
	require.Contains(t, page, `id="features-state"`)
}

// The disabled page is a plain server-rendered page: no GET form, no reload, and
// the way back is an ordinary link.
func TestFeatureOffPage_StaysOnTheSharedNoReloadPath(t *testing.T) {
	source, err := templateFS.ReadFile("templates/feature_off.html")
	require.NoError(t, err)
	page := string(source)

	require.NotContains(t, page, "<form")
	require.NotContains(t, page, "location.")
	require.Contains(t, page, `href="{{.SettingsHref}}"`)
	require.Contains(t, strings.Join(pageNames, ","), "feature_off",
		"the page must be parsed with the layout like every other page")
}
