package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// A level change reads as one ledger line in Activity, like a features toggle;
// a reset names the level it fell back to, and a thin payload says less rather
// than inventing a level.
func TestEventSummary_LevelChanged(t *testing.T) {
	for _, tc := range []struct {
		payload map[string]any
		want    string
	}{
		{map[string]any{"from": "basic", "to": "standard", "by": "console"}, "console level basic -> standard"},
		{map[string]any{"to": "advanced", "by": "console"}, "console level set to advanced"},
		{map[string]any{"from": "advanced", "to": "basic", "reset": true}, "console level reset to the file configuration (basic)"},
		{map[string]any{"reset": true}, "console level reset to the file configuration"},
		{nil, "console level changed"},
	} {
		require.Equal(t, tc.want, eventSummary(core.Event{Kind: eventLevelChanged, Payload: tc.payload}))
	}
	require.Equal(t, "Console level changed", evtLabel(string(eventLevelChanged)))
}

// The typed levels are config.ConsoleLevels by index: the constants, the
// parse, and the names must never drift apart.
func TestLevels_MatchConfig(t *testing.T) {
	require.Equal(t, []string{"basic", "standard", "advanced"}, config.ConsoleLevels)
	require.Len(t, allLevels(), len(config.ConsoleLevels))
	for i, name := range config.ConsoleLevels {
		lvl, err := parseLevel(name)
		require.NoError(t, err)
		require.Equal(t, level(i), lvl)
		require.Equal(t, name, lvl.String())
	}
	require.Equal(t, levelBasic, mustLevel(t, "basic"))
	require.Equal(t, levelStandard, mustLevel(t, "standard"))
	require.Equal(t, levelAdvanced, mustLevel(t, "advanced"))
	require.Equal(t, "Standard", levelStandard.Label())

	_, err := parseLevel("expert")
	require.ErrorContains(t, err, "valid values are basic, standard, advanced")

	ok, err := levelStandard.AtLeast("basic")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = levelStandard.AtLeast("advanced")
	require.NoError(t, err)
	require.False(t, ok)
	_, err = levelAdvanced.AtLeast("standrd")
	require.Error(t, err, "a typo'd template gate must fail the render, not hide the markup")
}

func mustLevel(t *testing.T, name string) level {
	t.Helper()
	lvl, err := parseLevel(name)
	require.NoError(t, err)
	return lvl
}

// The registry is well formed: unique ids and chords, known levels and badges,
// and a nav row for everything but the palette-only pages.
func TestScreenRegistry_WellFormed(t *testing.T) {
	ids, keys := map[string]bool{}, map[string]bool{}
	for _, sc := range screenRegistry() {
		require.NotEmpty(t, sc.ID)
		require.False(t, ids[sc.ID], "duplicate screen id %q", sc.ID)
		ids[sc.ID] = true
		require.NotEmpty(t, sc.Label, "%s needs a label", sc.ID)
		require.NotEmpty(t, string(icon(sc.Icon)), "%s names icon %q, which the icon set does not have", sc.ID, sc.Icon)
		require.True(t, strings.HasPrefix(sc.Href, "/console/"), "%s href %q", sc.ID, sc.Href)
		require.Contains(t, allLevels(), sc.Min, "%s has an unknown minimum level", sc.ID)
		if sc.Badge != "" {
			_, ok := navCounts{}.count(sc.Badge)
			require.True(t, ok, "%s shows badge %q, which no navCounts field answers", sc.ID, sc.Badge)
		}
		if sc.NavRow {
			require.NotEmpty(t, sc.Key, "nav entry %s needs a g-chord", sc.ID)
			require.False(t, keys[sc.Key], "g-chord %q is bound twice", sc.Key)
			keys[sc.Key] = true
		} else {
			require.NotEmpty(t, sc.Hint, "palette-only page %s needs a one-line hint", sc.ID)
			require.Empty(t, sc.Key, "palette-only page %s has no nav link to carry a chord", sc.ID)
		}
	}
	// The existing chords survive the move to the registry.
	require.Equal(t, map[string]bool{"o": true, "n": true, "i": true, "m": true, "e": true, "r": true, "g": true,
		"w": true, "p": true, "t": true, "s": true, "l": true, "x": true, ",": true}, keys)
}

// Monotonic: each level shows everything the previous one does, with every
// feature state -- a Basic user who steps up finds everything where it was.
func TestScreenRegistry_VisibleSetsNest(t *testing.T) {
	for _, feats := range []config.Features{featuresAll(false), featuresAll(true)} {
		var prev []screen
		for _, lvl := range allLevels() {
			cur := visibleScreens(feats, lvl)
			for _, sc := range prev {
				require.Contains(t, cur, sc, "%s shows %s but %s does not", lvl-1, sc.ID, lvl)
			}
			prev = cur
		}
	}
	// And the matrix's anchors: basic is the six essentials, advanced is all.
	var basic []string
	for _, sc := range visibleScreens(featuresAll(true), levelBasic) {
		basic = append(basic, sc.ID)
	}
	require.Equal(t, []string{"overview", "memories", "notes", "gardener", "sessions", "settings", "search"}, basic)
	require.Len(t, visibleScreens(featuresAll(true), levelAdvanced), len(screenRegistry()))
}

// The banner names a screen above the level, and nothing else.
func TestLevelBannerFor(t *testing.T) {
	b := levelBannerFor("retrieval", levelBasic)
	require.NotNil(t, b)
	require.Equal(t, "Retrieval", b.Label)
	require.Equal(t, levelAdvanced, b.Min)
	require.Equal(t, levelBasic, b.Current)

	require.Nil(t, levelBannerFor("retrieval", levelAdvanced))
	require.Nil(t, levelBannerFor("memories", levelBasic))
	require.Nil(t, levelBannerFor("no-such-screen", levelBasic))
	require.NotNil(t, levelBannerFor("context", levelStandard), "Context is its own advanced screen")
}

// The level POST's landing rule (T5 wires it): back where the owner was when
// the new level shows that screen, Home with a note when it hides it, and never
// anywhere off-console.
func TestRedirectAfterLevelChange(t *testing.T) {
	reg := screenRegistry()
	home := func(label, lvl string) string {
		return "/console/?notice=" + url.QueryEscape("Switched to "+lvl+". "+label+" is not in the "+lvl+
			" sidebar, so you are back on Home.")
	}
	for _, tc := range []struct {
		name, from string
		to         level
		want       string
	}{
		{"down past a hidden screen lands home", "/console/retrieval?w=7d", levelBasic, home("Retrieval", "Basic")},
		{"up keeps the page and its query", "/console/retrieval?w=7d", levelAdvanced,
			"/console/retrieval?notice=Switched+to+Advanced.&w=7d"},
		{"a detail page follows its screen", "/console/memories/01ABC", levelBasic,
			"/console/memories/01ABC?notice=Switched+to+Basic."},
		{"a detail page under a hidden screen lands home", "/console/tasks/01TASK", levelBasic, home("Tasks", "Basic")},
		{"no gated screen: stay", "/console/events/01XYZ", levelBasic, "/console/events/01XYZ?notice=Switched+to+Basic."},
		{"context is its own advanced screen", "/console/context", levelStandard, home("Context", "Standard")},
		{"standard keeps now", "/console/now", levelStandard, "/console/now?notice=Switched+to+Standard."},
		{"a stale flash never replays", "/console/sessions?error=old&notice=old", levelBasic,
			"/console/sessions?notice=Switched+to+Basic."},
		{"home stays home", "/console/", levelBasic, "/console/?notice=Switched+to+Basic."},
		{"off-console is refused", "https://evil.example/console/", levelBasic, "/console/?notice=Switched+to+Basic."},
		{"protocol-relative is refused", "//evil.example/console/", levelBasic, "/console/?notice=Switched+to+Basic."},
		{"empty means home", "", levelStandard, "/console/?notice=Switched+to+Standard."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, redirectAfterLevelChange(reg, tc.to, tc.from))
		})
	}
}

func TestScreenForPath(t *testing.T) {
	reg := screenRegistry()
	for path, want := range map[string]string{
		"/console/":                "overview",
		"/console/memories":        "memories",
		"/console/memories/01A":    "memories",
		"/console/labs/a/b":        "labs",
		"/console/settings":        "settings",
		"/console/interactionsX":   "",
		"/console/events/01E":      "",
		"/console/sessions/01S/x/": "sessions",
	} {
		sc, ok := screenForPath(reg, path)
		require.Equal(t, want != "", ok, path)
		require.Equal(t, want, sc.ID, path)
	}
}

// seedLevelFleet gives a console enough rows that a level-dependent answer
// would show: a project, a session, and a task and plan whose titles match
// "xylophone" -- work that only Standard's screens list.
func seedLevelFleet(t *testing.T, db *sql.DB) (taskID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.CreateProject(ctx, db, core.Project{
		ID: mustID(t), Slug: "orchestra", Name: "orchestra", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: mustID(t), Name: "cc/xylophone", ProjectSlug: "orchestra", Status: core.SessionActive,
		CreatedAt: now, UpdatedAt: now,
	}))
	taskID = mustID(t)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: taskID, ProjectSlug: "orchestra", Title: "tune the xylophone", Status: core.TaskOpen,
		CreatedAt: now, UpdatedAt: now,
	}))
	return taskID
}

// setLevel stores level as the owner's choice, the way the Settings picker does.
func setLevel(t *testing.T, db *sql.DB, lvl string) {
	t.Helper()
	require.NoError(t, store.SetConsoleLevel(context.Background(), db, lvl, store.ConsoleLevelChosen))
}

// levelFields are the JSON keys that report the level itself: the one part of a
// JSON answer allowed to differ by level.
var levelFields = []string{"consoleLevel", "consoleLevelOverridden", "consoleLevelSource"}

// rawJSON fetches path as a bearer JSON caller and returns the decoded body
// minus the level-reporting fields.
func rawJSON(t *testing.T, mux *http.ServeMux, path string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Accept", "application/json")
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code, "GET %s: %s", path, rr.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	for _, k := range levelFields {
		delete(out, k)
	}
	return out
}

// Principle 2: JSON callers are level-blind. The CLI, `seam doctor`, and
// client_features read these payloads, and a presentation choice in the console
// must never change what they get -- including the search groups for work only
// Standard's screens list.
func TestLevels_JSONAnswersAreIdenticalAcrossLevels(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{Research: true}, "basic")
	seedLevelFleet(t, db)

	for _, path := range []string{
		"/console/?format=json",
		"/console/settings?format=json",
		"/console/search?q=xylophone&format=json",
		"/console/retrieval?format=json",
		"/console/sessions?format=json",
		"/console/tasks?format=json",
		"/console/memories?format=json",
		"/console/notes?format=json",
		"/console/gardener?format=json",
		"/console/search?q=xylophone&w=7d&sort=newest&format=json",
	} {
		var answers []map[string]any
		for _, lvl := range config.ConsoleLevels {
			setLevel(t, db, lvl)
			answers = append(answers, rawJSON(t, mux, path))
		}
		require.Equal(t, answers[0], answers[1], "%s: basic and standard JSON must be identical", path)
		require.Equal(t, answers[0], answers[2], "%s: basic and advanced JSON must be identical", path)
	}

	// The search answer really does carry the hidden scope's group at basic.
	setLevel(t, db, "basic")
	var data searchData
	getJSON(t, mux, "/console/search?q=xylophone&format=json", &data)
	var kinds []string
	for _, g := range data.Groups {
		kinds = append(kinds, g.Kind)
	}
	require.Contains(t, kinds, "tasks", "JSON search is level-blind: basic still gets the tasks group")
}

// Principle 2, the page half: a screen above the level renders in full at its
// URL, under a banner offering the switch -- never a 403, 404, or redirect.
func TestLevels_HiddenScreenRendersWithTheBanner(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "basic")

	rr := getPeek(t, mux, "/console/retrieval?w=7d")
	require.Equal(t, http.StatusOK, rr.Code)
	page := rr.Body.String()
	require.Contains(t, page, `data-level-banner="retrieval"`)
	require.Contains(t, page, "<strong>Retrieval</strong> is an Advanced screen. You are in Basic: it works, it just is not in your sidebar.")
	require.Contains(t, page, `<form method="post" action="/console/settings/level">`)
	require.Contains(t, page, `<input type="hidden" name="level" value="advanced">`)
	require.Contains(t, page, `<input type="hidden" name="return" value="/console/retrieval?w=7d">`)
	require.Contains(t, page, "Switch to Advanced")
	require.Contains(t, page, "data-level-banner-dismiss")
	require.Contains(t, page, `class="retrieval-page`, "the full screen renders beneath the banner")
	require.NotContains(t, page, `href="/console/retrieval" data-key`, "the screen is still not in the sidebar")

	// The banner is information, not an error: no role=alert, no flash hook.
	banner := regexp.MustCompile(`(?s)<div class="section notice level-banner"[^>]*>`).FindString(page)
	require.NotContains(t, banner, "alert")
	require.NotContains(t, banner, "data-flash")

	// A Standard screen at basic says "a Standard screen".
	require.Contains(t, getPeek(t, mux, "/console/tasks").Body.String(),
		"<strong>Tasks</strong> is a Standard screen. You are in Basic")

	// Context highlights Projects but is its own Advanced screen.
	setLevel(t, db, "standard")
	require.Contains(t, getPeek(t, mux, "/console/context").Body.String(), `data-level-banner="context"`)

	// At the screen's level there is nothing to say.
	setLevel(t, db, "advanced")
	for _, path := range []string{"/console/retrieval", "/console/context", "/console/tasks"} {
		require.NotContains(t, getPeek(t, mux, path).Body.String(), "data-level-banner", path)
	}
	// A screen every level shows never carries it.
	setLevel(t, db, "basic")
	require.NotContains(t, getPeek(t, mux, "/console/memories").Body.String(), "data-level-banner")
}

// A fragment is injected into a pane, never a page: it never carries the banner.
func TestLevels_FragmentsNeverCarryTheBanner(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "basic")
	taskID := seedLevelFleet(t, db)
	for _, path := range []string{
		"/console/tasks/" + taskID + "?peek=1",
		"/console/tasks/" + taskID + "?reader=1",
		"/console/projects/orchestra?peek=1",
	} {
		rr := getPeek(t, mux, path)
		require.Equal(t, http.StatusOK, rr.Code, path)
		require.NotContains(t, rr.Body.String(), "level-banner", path)
	}
	// The full task page does.
	require.Contains(t, getPeek(t, mux, "/console/tasks/"+taskID).Body.String(), `data-level-banner="tasks"`)
}

// Search offers only the level's scopes on the page, while a hidden scope still
// works by URL (with the banner) and a misspelling is still a named 400 naming
// the level-blind enum.
func TestLevels_SearchScopesFollowTheLevel(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{Research: true}, "basic")
	seedLevelFleet(t, db)

	page := getPeek(t, mux, "/console/search?q=xylophone").Body.String()
	for _, scope := range []string{"all", "memories", "notes", "sessions"} {
		require.Contains(t, page, `href="/console/search?scope=`+scope+`&amp;`, "basic offers %s", scope)
	}
	for _, scope := range []string{"tasks", "plans", "trials", "projects"} {
		require.NotContains(t, page, `href="/console/search?scope=`+scope+`&amp;`, "basic hides %s", scope)
	}
	require.Contains(t, page, "cc/xylophone", "the session matches under all")
	require.NotContains(t, page, "tune the xylophone", `"all" on the basic page covers the offered scopes only`)
	require.Contains(t, page, `placeholder="Search memories, notes, and sessions&hellip;"`)
	require.Contains(t, page, `data-scopes="memories notes sessions"`, "the palette narrows to the same scopes")

	// Hidden, not locked: an explicit hidden scope still searches, under the banner.
	page = getPeek(t, mux, "/console/search?q=xylophone&scope=tasks").Body.String()
	require.Contains(t, page, "tune the xylophone")
	require.Contains(t, page, `data-level-banner="tasks"`)
	require.Contains(t, page, `href="/console/search?scope=tasks&amp;`, "the active scope stays visible in the selector")

	// A misspelling is refused with the level-blind enum, as before.
	rr := getPeek(t, mux, "/console/search?q=xylophone&scope=task")
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), "valid values are all, memories, notes, tasks, plans, trials, projects, sessions")

	// Standard offers the work scopes; the research scope follows its feature.
	setLevel(t, db, "standard")
	page = getPeek(t, mux, "/console/search?q=xylophone").Body.String()
	require.Contains(t, page, "tune the xylophone")
	for _, scope := range []string{"tasks", "plans", "trials", "projects"} {
		require.Contains(t, page, `href="/console/search?scope=`+scope+`&amp;`, "standard offers %s", scope)
	}
	require.NotContains(t, page, "data-level-banner")
}

// The project workspace's Interactions and Context tabs are those screens,
// scoped: they follow the screens' level, and a direct ?tab= still renders.
func TestLevels_ProjectTabsFollowTheirScreens(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "standard")
	seedLevelFleet(t, db)

	page := getPeek(t, mux, "/console/projects/orchestra").Body.String()
	require.Contains(t, page, `data-tab="memories"`)
	require.NotContains(t, page, `data-tab="interactions"`)
	require.NotContains(t, page, `data-tab="context"`)
	require.NotContains(t, page, "data-level-banner", "projects is a standard screen")

	page = getPeek(t, mux, "/console/projects/orchestra?tab=interactions").Body.String()
	require.Contains(t, page, `data-level-banner="interactions"`)
	require.Contains(t, page, `data-tab="interactions"`, "the requested tab stays in the bar, marked on")
	require.Contains(t, page, `data-panel="interactions"`)

	setLevel(t, db, "advanced")
	page = getPeek(t, mux, "/console/projects/orchestra").Body.String()
	require.Contains(t, page, `data-tab="interactions"`)
	require.Contains(t, page, `data-tab="context"`)
	require.NotContains(t, page, "data-level-banner")
}

// The palette's Jump to follows the level: the rows with no sidebar link (the
// pages without a nav row, the Settings sections) are rendered inside the nav
// from the registries, so a hidden screen or section is not offered there.
func TestLevels_PaletteJumpsFollowTheLevel(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "basic")
	jumpRe := regexp.MustCompile(`<a href="([^"]+)" data-jump="([^"]+)"`)
	jumps := func() []string {
		var out []string
		for _, m := range jumpRe.FindAllStringSubmatch(getPeek(t, mux, "/console/memories").Body.String(), -1) {
			out = append(out, m[1])
		}
		return out
	}

	require.Equal(t, []string{
		"/console/search",
		"/console/settings?s=experience", "/console/settings?s=features", "/console/settings?s=setup",
		"/console/settings?s=updates",
	}, jumps(), "basic: no Context, no Standard or Advanced sections")

	setLevel(t, db, "advanced")
	require.Equal(t, []string{
		"/console/search", "/console/context",
		"/console/settings?s=experience", "/console/settings?s=features", "/console/settings?s=setup",
		"/console/settings?s=updates",
		"/console/settings?s=briefing", "/console/settings?s=workspaces", "/console/settings?s=engine",
	}, jumps())

	page := getPeek(t, mux, "/console/memories").Body.String()
	require.Contains(t, page, `data-jump="Settings › Briefing" data-hint="What every new agent session starts with"`)
	require.Contains(t, string(searchJS), "a.getAttribute('data-jump')", "the palette reads the jump rows")
	require.NotContains(t, string(searchJS), "title: 'Context'", "no hand-listed pages left in the palette")
}

// levelEvents returns the recorded level-change payloads, oldest first.
func levelEvents(t *testing.T, db *sql.DB) []map[string]any {
	t.Helper()
	evs, err := events.NewRecorder(db).ByKinds(context.Background(), []core.EventKind{eventLevelChanged}, "", "", 50)
	require.NoError(t, err)
	out := make([]map[string]any, 0, len(evs))
	for i := len(evs) - 1; i >= 0; i-- {
		out = append(out, evs[i].Payload)
	}
	return out
}

// The level POST: strict at the boundary, logged like a features toggle, and
// landing where the redirect rule says.
func TestLevelPost_ValidatesRecordsAndRedirects(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "basic")
	ctx := context.Background()

	// Absent is a malformed request; present-but-unknown flashes the values.
	rr := postForm(mux, "/console/settings/level", "")
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), "valid values are basic, standard, advanced")
	rr = postForm(mux, "/console/settings/level", "level=expert")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.True(t, strings.HasPrefix(rr.Header().Get("Location"), "/console/settings?s=experience&error="))
	require.Contains(t, rr.Header().Get("Location"), url.QueryEscape("valid values are basic, standard, advanced"))
	rr = postForm(mux, "/console/settings/level", "level=standard&level=advanced")
	require.Contains(t, rr.Header().Get("Location"), "error=")
	_, found, err := store.GetSetting(ctx, db, store.SettingConsoleLevel)
	require.NoError(t, err)
	require.False(t, found, "a refused level never reaches the row")

	// Up from Basic while standing on a hidden screen: back to it.
	rr = postForm(mux, "/console/settings/level", "level=advanced&return="+url.QueryEscape("/console/retrieval?w=7d"))
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/retrieval?notice=Switched+to+Advanced.&w=7d", rr.Header().Get("Location"))
	level, overridden, source, welcomed, err := store.ConsoleLevel(ctx, db, "basic")
	require.NoError(t, err)
	require.Equal(t, "advanced", level)
	require.True(t, overridden)
	require.Equal(t, store.ConsoleLevelChosen, source)
	require.True(t, welcomed, "a choice anywhere answers the welcome card")

	// Down while standing on a screen the new level hides: Home, with the note.
	rr = postForm(mux, "/console/settings/level", "level=basic&return="+url.QueryEscape("/console/retrieval"))
	require.Equal(t, "/console/?notice="+url.QueryEscape("Switched to Basic. Retrieval is not in the Basic sidebar, so you are back on Home."),
		rr.Header().Get("Location"))

	// Down from a Settings section the new level hides: Experience.
	setLevel(t, db, "advanced")
	rr = postForm(mux, "/console/settings/level", "level=standard&return="+url.QueryEscape("/console/settings?s=engine"))
	require.True(t, strings.HasPrefix(rr.Header().Get("Location"), "/console/settings?notice="), rr.Header().Get("Location"))
	require.Contains(t, rr.Header().Get("Location"), "s=experience")

	// No return: the Experience section. An off-console return: Home.
	rr = postForm(mux, "/console/settings/level", "level=basic")
	require.True(t, strings.HasPrefix(rr.Header().Get("Location"), "/console/settings?notice=Switched+to+Basic."))
	rr = postForm(mux, "/console/settings/level", "level=standard&return="+url.QueryEscape("https://evil.example/"))
	require.Equal(t, "/console/?notice=Switched+to+Standard.", rr.Header().Get("Location"))

	// The same level again records the choice but logs no change.
	rr = postForm(mux, "/console/settings/level", "level=standard&return=/console/")
	require.Equal(t, "/console/?notice=Staying+on+Standard.", rr.Header().Get("Location"))

	evs := levelEvents(t, db)
	require.Len(t, evs, 5, "one event per real change; the refused posts and the no-op log nothing")
	require.Equal(t, map[string]any{"from": "basic", "to": "advanced", "by": "console"}, evs[0])
	require.Equal(t, map[string]any{"from": "advanced", "to": "basic", "by": "console"}, evs[1])
}

// Reset goes back to the file/env level, keeps the welcome, logs the change,
// and says so plainly when there was nothing to reset.
func TestLevelReset_BackToFileAndEnv(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "standard")
	ctx := context.Background()

	rr := postForm(mux, "/console/settings/level/reset", "")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), url.QueryEscape("Already following the file and environment (Standard)."))
	require.Empty(t, levelEvents(t, db))

	setLevel(t, db, "advanced")
	rr = postForm(mux, "/console/settings/level/reset", "return="+url.QueryEscape("/console/retrieval"))
	require.Equal(t, "/console/?notice="+url.QueryEscape("Switched to Standard. Retrieval is not in the Standard sidebar, so you are back on Home."),
		rr.Header().Get("Location"))
	level, overridden, _, welcomed, err := store.ConsoleLevel(ctx, db, "standard")
	require.NoError(t, err)
	require.Equal(t, "standard", level)
	require.False(t, overridden)
	require.True(t, welcomed, "a reset keeps the welcome")
	evs := levelEvents(t, db)
	require.Len(t, evs, 1)
	require.Equal(t, map[string]any{"from": "advanced", "to": "standard", "by": "console", "reset": true}, evs[0])
}

// The welcome card shows on Home until the owner picks a level or dismisses
// it -- on any device, since the flag is server-side.
func TestLevelWelcomeCard_UntilChosenOrDismissed(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "basic")
	ctx := context.Background()

	page := getPeek(t, mux, "/console/").Body.String()
	require.Contains(t, page, `<section class="level-welcome" id="level-welcome" data-live-skip`)
	require.Contains(t, page, "Choose how much of the console you want to see")
	for _, lvl := range config.ConsoleLevels {
		require.Contains(t, page, `data-level-card="`+lvl+`"`)
	}
	require.Contains(t, page, `action="/console/settings/level/welcome"`)
	require.Contains(t, page, "You can change this any time in")
	require.NotContains(t, getPeek(t, mux, "/console/memories").Body.String(), "level-welcome", "Home only")

	// Dismiss: gone, level unchanged, and nothing to announce.
	rr := postForm(mux, "/console/settings/level/welcome", "return=/console/")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/", rr.Header().Get("Location"))
	require.NotContains(t, getPeek(t, mux, "/console/").Body.String(), "level-welcome")
	level, overridden, _, _, err := store.ConsoleLevel(ctx, db, "basic")
	require.NoError(t, err)
	require.Equal(t, "basic", level)
	require.False(t, overridden, "dismissing is not choosing")

	// An upgraded installation (the migration's seeded row) is told what is new.
	require.NoError(t, store.SetSetting(ctx, db, store.SettingConsoleLevel,
		`{"level":"advanced","source":"seeded","welcomed":false}`))
	page = getPeek(t, mux, "/console/").Body.String()
	require.Contains(t, page, "New: choose how much of the console you see")
	// Choosing any level from the card answers it.
	rr = postForm(mux, "/console/settings/level", "level=standard&return=/console/")
	require.Equal(t, "/console/?notice=Switched+to+Standard.", rr.Header().Get("Location"))
	require.NotContains(t, getPeek(t, mux, "/console/").Body.String(), "level-welcome")

	// JSON never carries the card.
	require.NotContains(t, rawJSONString(t, mux, "/console/?format=json"), "welcome")
}

func rawJSONString(t *testing.T, mux *http.ServeMux, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	return rr.Body.String()
}

// Settings -> Experience: three cards, the current one marked, each with a
// list GENERATED from the registries; the precedence line names who set it.
func TestExperienceSection_CardsAreGenerated(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{Research: true}, "basic")
	page := getPeek(t, mux, "/console/settings?s=experience").Body.String()

	card := func(lvl string) string {
		re := regexp.MustCompile(`(?s)<form class="level-card[^"]*" method="post" action="/console/settings/level" data-level-card="` + lvl + `">.*?</form>`)
		m := re.FindString(page)
		require.NotEmpty(t, m, "card %s", lvl)
		return m
	}
	basic, standard, advanced := card("basic"), card("standard"), card("advanced")
	require.Contains(t, basic, "is-current")
	require.Contains(t, basic, "In use")
	require.Contains(t, standard, "Use Standard")
	for _, item := range []string{"Overview", "Memories", "Notes", "Gardener", "Sessions", "Settings", "Search",
		"Settings: Experience, Features, Your setup, and Updates"} {
		require.Contains(t, basic, "<li>"+item+"</li>")
	}
	for _, item := range []string{"Now", "Projects", "Plans", "Tasks", "Labs", "Trials", "Settings: Briefing and Workspaces"} {
		require.Contains(t, standard, "<li>"+item+"</li>")
	}
	for _, item := range []string{"Interactions", "Retrieval", "Context", "Settings: Knowledge engine"} {
		require.Contains(t, advanced, "<li>"+item+"</li>")
	}
	require.NotContains(t, standard, "<li>Interactions</li>", "each card lists what IT adds")
	require.Contains(t, page, "Following file + env")
	require.Contains(t, page, `data-theme-choice`)
	require.Contains(t, page, `data-keys-open`)

	// Research off: its screens leave the lists, like they leave the sidebar.
	_, muxOff := newConsoleLevel(t, config.Features{}, "basic")
	offPage := getPeek(t, muxOff, "/console/settings?s=experience").Body.String()
	require.NotContains(t, offPage, "<li>Labs</li>")

	setLevel(t, db, "standard")
	page = getPeek(t, mux, "/console/settings?s=experience").Body.String()
	require.Contains(t, card("standard"), "is-current")
	require.Contains(t, page, "Chosen in the console")
	require.Contains(t, page, `action="/console/settings/level/reset"`)

	require.NoError(t, store.SetSetting(context.Background(), db, store.SettingConsoleLevel,
		`{"level":"advanced","source":"seeded","welcomed":true}`))
	page = getPeek(t, mux, "/console/settings?s=experience").Body.String()
	require.Contains(t, page, "Set by the upgrade")
}

// The sidebar's account row names the level as a link to the picker, stays one
// row, and the level switch reaches it on the no-reload path.
func TestSidebar_NamesTheLevel(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "basic")
	page := getPeek(t, mux, "/console/").Body.String()
	require.Contains(t, page, `<a class="account-level" href="/console/settings?s=experience" title="Console level: Basic. Change it in Settings." aria-label="Console level: Basic">Basic</a>`)
	setLevel(t, db, "advanced")
	require.Contains(t, getPeek(t, mux, "/console/").Body.String(), `aria-label="Console level: Advanced">Advanced</a>`)

	require.Contains(t, string(navigationJS), "morphNode(currentAccount, freshAccount)",
		"a level switch must re-render the account row in place")
	require.Contains(t, string(searchJS), "title: 'Change experience level', href: '/console/settings?s=experience'")
	require.Contains(t, string(shellJS), "[data-keys-open]")

	layout, err := templateFS.ReadFile("templates/layout.html")
	require.NoError(t, err)
	require.Contains(t, string(layout), "window.SeamTheme = { set: set, choice: choice, effective: effective };",
		"one theme setter, shared by the sidebar toggle and Settings > Experience")
	require.Contains(t, string(layout), "'seam:theme'")
}

// The level POSTs ride the console's write guard: a cookie write must prove
// same-origin like every other.
func TestLevelPost_RequiresSameOriginForCookies(t *testing.T) {
	_, mux := newConsoleLevel(t, config.Features{}, "basic")
	for _, path := range []string{"/console/settings/level", "/console/settings/level/reset", "/console/settings/level/welcome"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("level=advanced"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(consoleCookie())
		req.Header.Set("Sec-Fetch-Site", "same-site")
		require.Equal(t, http.StatusForbidden, do(mux, req).Code, path)
	}
}

// Settings -> Your setup: the facts a Basic owner can check and act on.
func TestSetupSection_Facts(t *testing.T) {
	db, err := openFreshDB(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(context.Background(), db, core.Session{
		ID: mustID(t), Name: "cc/s1", ExternalClient: "claude-code", Status: core.SessionActive,
		CreatedAt: now.Add(-10 * time.Minute), UpdatedAt: now.Add(-10 * time.Minute),
	}))
	svc, err := New(Config{
		DB: db, APIKey: testKey, DataDir: "/home/owner/.seamless", DBPath: "/home/owner/.seamless/seam.db",
		ConfigPath: "/home/owner/.config/seamless/seamless.yaml", Version: "0.5.3",
		GardenerCfg: config.Gardener{Enabled: true, IntervalMinutes: 60},
		Embedding:   EmbeddingRuntime{Provider: "openai", Reason: "llm.NewEmbedder: openai selected but api_key is empty"},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)

	page := getPeek(t, mux, "/console/settings?s=setup").Body.String()
	for _, want := range []string{
		"<dt>Version</dt>", "0.5.3",
		"<dt>Running on</dt>",
		"/home/owner/.config/seamless/seamless.yaml", `href="vscode://file/home/owner/.config/seamless/seamless.yaml"`,
		"<dt>Data folder</dt>", "/home/owner/.seamless",
		"<dt>Agents connected</dt>", "Claude Code", "last active 10m ago",
		"<dt>Semantic recall</dt>", "Add an OpenAI key", "recall-returns-junk-or-misses-what-you-just-wrote",
		"<dt>Gardener</dt>", "suggests cleanups every 60 minutes",
		"https://thereisnospoon.org/docs/quickstart/",
	} {
		require.Contains(t, page, want)
	}
	for _, advanced := range []string{"max_briefing_tokens", "Dedup threshold", "Schema version"} {
		require.NotContains(t, page, advanced, "budgets and policy stay in Knowledge engine")
	}
}

// The styled 404 offers the places a lost reader most often means -- drawn from
// the screens the level shows, so Basic is never sent to a screen its sidebar
// lacks -- and its search box names what the level searches.
func TestErrorPage_DestinationsFollowTheLevel(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "basic")
	destinations := regexp.MustCompile(`(?s)<nav class="err-actions" aria-label="Go somewhere else">(.*?)</nav>`)
	hrefs := regexp.MustCompile(`href="([^"]+)"`)
	links := func() []string {
		rr := getPeek(t, mux, "/console/no-such-screen")
		require.Equal(t, http.StatusNotFound, rr.Code)
		nav := destinations.FindStringSubmatch(rr.Body.String())
		require.NotNil(t, nav)
		var out []string
		for _, m := range hrefs.FindAllStringSubmatch(nav[1], -1) {
			out = append(out, m[1])
		}
		return out
	}
	require.Equal(t, []string{"/console/", "/console/memories", "/console/sessions", "/console/notes"}, links())
	require.Contains(t, getPeek(t, mux, "/console/no-such-screen").Body.String(),
		`placeholder="Search memories, notes, and sessions&hellip;"`)

	setLevel(t, db, "standard")
	require.Equal(t, []string{"/console/", "/console/memories", "/console/sessions", "/console/projects"}, links())
}
