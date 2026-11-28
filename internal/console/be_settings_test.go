package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// UX 38 (plan:console-ux-pass): Settings > Briefing beside its preview, and
// Knowledge engine as three cards, Ranking by use without the wall of rows.

// beSection cuts one Settings section out of a page: from its id to the page
// script that follows the sections.
func beSection(t *testing.T, page, id string) string {
	t.Helper()
	i := strings.Index(page, `id="`+id+`"`)
	require.GreaterOrEqual(t, i, 0, id)
	j := strings.Index(page[i:], "<script>")
	require.Greater(t, j, 0, id)
	return page[i : i+j]
}

// Every field the save reads is a knob exactly once, every knob says why in
// its hint, and a number says what its 0 means.
func TestBeBriefingGroups_CarryEveryField(t *testing.T) {
	want := []string{"include_parent_memories", "include_sibling_memories", "utility_weight", "utility_mode"}
	for _, f := range briefingIntFields {
		want = append(want, f.name)
	}
	var got []string
	for _, g := range briefingGroups(config.Defaults().Briefing) {
		require.NotEmpty(t, g.Icon, g.Key)
		require.NotEmpty(t, g.Label, g.Key)
		for _, k := range g.Knobs {
			got = append(got, k.Name)
			require.NotEmpty(t, k.Hint, k.Name)
			require.NotEmpty(t, k.Label, k.Name)
			require.Contains(t, []string{"number", "switch", "choice"}, k.Kind, k.Name)
			if k.Kind == "number" {
				require.NotEmpty(t, k.Unit, k.Name)
				require.NotEmpty(t, k.Zero, k.Name)
				require.Contains(t, k.Hint, "0 ", "%s: the hint says what 0 does", k.Name)
			}
		}
	}
	slices.Sort(want)
	slices.Sort(got)
	require.Equal(t, want, got)
	require.Len(t, briefingFormValues(config.Defaults().Briefing), len(got), "the presets fill the same fields")
}

// A 0 draws its meaning in the unit slot, any other value its unit; the
// choice marks the mode in force, and an empty mode reads as auto.
func TestBeBriefingGroups_ZeroWordsAndTheMode(t *testing.T) {
	b := config.Defaults().Briefing
	b.MemoryMaxAgeDays, b.FindingsCount, b.UtilityWeight, b.UtilityMode = 0, 3, 0, ""
	knob := func(name string) briefingKnob {
		for _, g := range briefingGroups(b) {
			for _, k := range g.Knobs {
				if k.Name == name {
					return k
				}
			}
		}
		t.Fatalf("no knob %s", name)
		return briefingKnob{}
	}
	require.Equal(t, "any age", knob("memory_max_age_days").UnitNow())
	require.Equal(t, "items", knob("findings_count").UnitNow())
	require.Equal(t, "3", knob("findings_count").Value)
	w := knob("utility_weight")
	require.True(t, w.IsZero)
	require.Equal(t, "recency only", w.UnitNow())
	require.Equal(t, "0.1", w.Step)
	var on []string
	for _, c := range knob("utility_mode").Choices {
		if c.On {
			on = append(on, c.Value)
		}
	}
	require.Equal(t, []string{"auto"}, on)
}

// The section: presets, Fine-tune open at every level with its groups (no
// kicker, no paragraph, no per-field <small> help: the why is in the hints),
// the zero words drawn by the server, the preview after the form in the
// markup (below it where the section is narrow, beside it where wide).
func TestBeBriefingSection_KnobsHintsAndThePreviewAfterTheForm(t *testing.T) {
	db, mux := newConsole(t)
	seedPreviewProject(t, db)
	rich := config.BriefingPresets()[2].Briefing
	require.NoError(t, store.SetBriefingConfig(context.Background(), db, rich))

	sec := beSection(t, getPeek(t, mux, "/console/settings?s=briefing").Body.String(), "briefing-recipe")
	require.NotContains(t, sec, "<small", "the per-field help lives in the hints")
	for _, gone := range []string{"brief-group-icon", "brief-field", "brief-number", "brief-guardrail", "utility-scopes", " title=\""} {
		require.NotContains(t, sec, gone)
	}
	require.Contains(t, sec, `<details class="brief-customize" open>`)
	require.Contains(t, sec, `<h3 class="brief-set-h" id="brief-set-memory">`)
	require.Contains(t, sec, `<span class="set-unit is-zero" data-knob-unit data-unit="days" data-zero="any age"><input id="memory-max-age" type="number" name="memory_max_age_days" min="0" value="0" aria-describedby="memory-max-age-unit"><span id="memory-max-age-unit">any age</span></span>`)
	require.Contains(t, sec, `<span class="set-unit" data-knob-unit data-unit="items" data-zero="off"><input id="findings-count" type="number" name="findings_count" min="0" value="5" aria-describedby="findings-count-unit"><span id="findings-count-unit">items</span></span>`)
	require.Contains(t, sec, `<input type="checkbox" role="switch" name="include_sibling_memories" value="1" aria-labelledby="include-sibling-memories-label" checked>`)
	require.Contains(t, sec, `<label><input type="radio" name="utility_mode" value="auto" checked><span>Per project</span></label>`)
	require.Contains(t, sec, `aria-label="About Constraints in full"`)
	require.Contains(t, sec, `<span class="brief-preset-name">Rich<span class="badge compact accent">in use</span></span>`)
	require.Contains(t, sec, `<span class="brief-preset-summary" data-preset-estimate="lean">`)

	form := strings.Index(sec, `data-briefing-form>`)
	formEnd := strings.Index(sec, `</form>`+"\n\n        ")
	preview := strings.Index(sec, `<aside class="brief-preview" data-brief-preview`)
	require.Greater(t, form, 0)
	require.Greater(t, formEnd, form)
	require.Greater(t, preview, formEnd, "the preview follows the form: below it when narrow")
	require.Contains(t, sec, `<span class="badge compact quiet brief-preview-dirty">unsaved values</span>`)

	source := string(settingsPageSource(t))
	require.Contains(t, source, "function syncKnobUnits()", "a 0's meaning follows the value as it changes")
	require.Contains(t, source, "params.set('presets', '1')")

	css := string(consoleCSS)
	require.Contains(t, css, "/* UX 38: Settings > Briefing beside its preview")
	require.Contains(t, css, "@container briefing (min-width: 780px) {\n  .js .briefing-workbench { grid-template-columns: minmax(0, 1fr) minmax(290px, 0.55fr);")
	require.Contains(t, css, ".brief-section { container: briefing / inline-size; }")
	require.Contains(t, css, `[data-theme="light"] { color-scheme: light; }`, "a light select draws dark text on a dark system")
	require.Contains(t, css, "[data-theme=\"dark\"] {\n  color-scheme: dark;")
}

// presets=1 prices each preset for the previewed project, in the JSON and in
// the fragment's hidden list the page moves into the cards; without it there
// is nothing extra. A preview records nothing, priced or not, and a presets
// value other than 1 is refused.
func TestBePreview_PricesThePresetsWhenAsked(t *testing.T) {
	db, mux := newConsole(t)
	seedPreviewProject(t, db)
	events := countRows(t, db, "events")
	settings := countRows(t, db, "settings")

	form := briefingValues(config.Defaults().Briefing)
	form.Set("project", "orbital")
	plain := postForm(mux, "/console/settings/briefing/preview", form.Encode())
	require.Equal(t, http.StatusOK, plain.Code)
	require.NotContains(t, plain.Body.String(), "data-preset-estimates")

	form.Set("presets", "1")
	rr := postForm(mux, "/console/settings/briefing/preview?format=json", form.Encode())
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var got briefingPreview
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	require.Len(t, got.Presets, len(config.BriefingPresets()))
	for i, p := range config.BriefingPresets() {
		require.Equal(t, p.Key, got.Presets[i].Key)
		require.Positive(t, got.Presets[i].Tokens, p.Key)
	}
	frag := postForm(mux, "/console/settings/briefing/preview", form.Encode()).Body.String()
	require.Contains(t, frag, `<ul class="brief-preview-presets" hidden data-preset-estimates>`)
	require.Regexp(t, `<li data-preset="lean">~[0-9,]+ tokens</li>`, frag)

	var saved briefingPreview
	getJSON(t, mux, "/console/settings/briefing/preview?project=orbital&presets=1&format=json", &saved)
	require.Len(t, saved.Presets, len(config.BriefingPresets()), "GET prices them too")

	form.Set("presets", "2")
	bad := postForm(mux, "/console/settings/briefing/preview?format=json", form.Encode())
	require.Equal(t, http.StatusBadRequest, bad.Code)
	require.Contains(t, bad.Body.String(), "presets must be omitted, or given once as 1")

	require.Equal(t, events, countRows(t, db, "events"), "a preview records no event")
	require.Equal(t, settings, countRows(t, db, "settings"), "a preview writes no settings row")
}

// beRankingConsole seeds 4 projects that rank by use, 1 forced off and 40
// still building, and returns the console.
func beRankingConsole(t *testing.T) (*http.ServeMux, map[string]store.UtilityProjectState) {
	t.Helper()
	db, mux := newConsole(t)
	ctx := context.Background()
	at := time.Now().UTC().Add(-48 * time.Hour)
	states := map[string]store.UtilityProjectState{}
	for i := range 45 {
		slug := fmt.Sprintf("be-p%02d", i)
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		switch {
		case i < 4:
			states[slug] = store.UtilityProjectState{ReadyAt: &at}
		case i == 4:
			states[slug] = store.UtilityProjectState{Forced: "off"}
		}
	}
	require.NoError(t, store.SetUtilityActivation(ctx, db, store.UtilityActivation{Projects: states}))
	return mux, states
}

// Ranking lists only the projects that rank or are forced, folds the 40
// still building under one row, and counts both in its chips. The global
// scope, with no history, is not among them.
func TestBeRanking_ListsRankingAndForcedFoldsTheRest(t *testing.T) {
	mux, _ := beRankingConsole(t)
	sec := beSection(t, getPeek(t, mux, "/console/settings?s=engine").Body.String(), "utility-activation")
	fold := strings.Index(sec, `<details class="ur-fold">`)
	require.Greater(t, fold, 0)
	listed, folded := sec[:fold], sec[fold:]

	row := regexp.MustCompile(`<form method="post" action="/console/settings/utility" class="set-row ur-row" id="(ur-[0-9a-f]{8})" data-autosave>\s*<input type="hidden" name="project" value="([^"]+)">`)
	var shown []string
	for _, m := range row.FindAllStringSubmatch(listed, -1) {
		shown = append(shown, m[2])
		require.Equal(t, utilityProjectRow{Project: m[2]}.Anchor(), m[1])
	}
	require.Equal(t, []string{"be-p00", "be-p01", "be-p02", "be-p03", "be-p04"}, shown, "the four ranking, then the forced one")
	require.Len(t, row.FindAllStringSubmatch(folded, -1), 40)
	require.Contains(t, folded, `<span class="ur-fold-count">40 projects still building their history</span><span class="ur-fold-show">Show them</span>`)
	require.NotContains(t, sec, "Global context")

	require.Contains(t, listed, `<span class="badge compact ok">4 ranking</span>`)
	require.Contains(t, listed, `<span class="badge compact accent">1 forced</span>`)
	require.Contains(t, listed, `<span class="badge compact quiet">40 building</span>`)
	require.Contains(t, listed, "forced off · no use yet")
	require.Contains(t, listed, `<input type="radio" name="force" value="off" checked><span>Off</span>`)
	require.Contains(t, listed, `aria-label="Ranking by use in be-p00"`)
	require.Contains(t, listed, `class="ur-meter" role="img" aria-label="0 of `)
	require.NotContains(t, sec, "Force on", "one segment, not a button per force")
}

// A segment posts force= to the one route, and the answer goes back to its
// row; setting a forced project back to Auto folds it with the rest, where
// the same anchor still finds it.
func TestBeRanking_SegmentPostsForceAndReturnsToItsRow(t *testing.T) {
	mux, _ := beRankingConsole(t)
	anchor := utilityProjectRow{Project: "be-p10"}.Anchor()

	rr := postForm(mux, "/console/settings/utility", url.Values{"project": {"be-p10"}, "force": {"on"}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	loc := rr.Header().Get("Location")
	require.True(t, strings.HasPrefix(loc, "/console/settings?s=engine&notice="), loc)
	require.True(t, strings.HasSuffix(loc, "#"+anchor), loc)

	sec := beSection(t, getPeek(t, mux, "/console/settings?s=engine").Body.String(), "utility-activation")
	fold := strings.Index(sec, `<details class="ur-fold">`)
	i := strings.Index(sec, `id="`+anchor+`"`)
	require.Greater(t, i, 0)
	require.Less(t, i, fold, "forced on, it is listed")
	require.Contains(t, sec[i:], `<input type="radio" name="force" value="on" checked><span>On</span>`)
	require.Contains(t, sec, `<span class="badge compact ok">5 ranking</span>`)
	require.Contains(t, sec, `<span class="badge compact accent">2 forced</span>`)

	rr = postForm(mux, "/console/settings/utility", url.Values{"project": {"be-p10"}, "force": {"auto"}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.True(t, strings.HasSuffix(rr.Header().Get("Location"), "#"+anchor))
	sec = beSection(t, getPeek(t, mux, "/console/settings?s=engine").Body.String(), "utility-activation")
	require.Greater(t, strings.Index(sec, `id="`+anchor+`"`), strings.Index(sec, `<details class="ur-fold">`), "back on Auto, it folds")
}

// The grouping itself: forced rows stay listed whatever their history, a
// ready project is listed and counted apart, the global scope folds only
// with some history (and then the fold says scopes), and a mode other than
// auto is carried for the card's note.
func TestBeUtilityRankingOf(t *testing.T) {
	rows := []utilityProjectRow{
		{Project: "a", Status: "active"},
		{Project: "b", Status: "armed"},
		{Project: "c", Status: "forced-off", Forced: "off"},
		{Project: "d", Status: "active", Forced: "on"},
		{Project: "e", Status: "building"},
		{Project: "", Status: "building"},
	}
	u := utilityRankingOf(rows, "")
	require.Equal(t, "auto", u.Mode)
	require.Equal(t, 2, u.Ranking)
	require.Equal(t, 1, u.Ready)
	require.Equal(t, 2, u.Forced)
	require.Len(t, u.Rows, 4)
	require.Len(t, u.Building, 1)
	require.Equal(t, "projects", u.BuildingNoun())

	rows[5].RecentEvents = 3
	u = utilityRankingOf(rows, "off")
	require.Len(t, u.Building, 2)
	require.Equal(t, "scopes", u.BuildingNoun())
	require.Equal(t, "off", u.Mode)
	require.Equal(t, "Global context", u.Building[1].Label())
	require.Equal(t, "ur-global", u.Building[1].Anchor())
}

// A row's sub line and meter: counts against nothing yet, a force first, a
// ready project's next step, and what a building one still needs; the meter
// is full at each threshold and green once met.
func TestBeUtilityRow_WhyAndMeter(t *testing.T) {
	r := utilityProjectRow{Project: "p", Status: "active", RecentEvents: 1, RecentMemories: 1, AgeDays: 40,
		AgeOK: true}
	require.Equal(t, "1 use · 1 memory · 40 days", r.Why())
	m := r.Meter()
	require.Len(t, m, 3)
	require.Equal(t, utilityBar{Pct: 100, Met: true}, m[2], "past the threshold the bar stays full")
	require.False(t, m[0].Met)
	require.Equal(t, percent(1, store.UtilityReadyMinEvents), m[0].Pct)

	require.Equal(t, "no use yet", utilityProjectRow{Status: "building"}.Why())
	require.Equal(t, "forced off · no use yet", utilityProjectRow{Status: "forced-off", Forced: "off"}.Why())
	require.Equal(t, "needs 3d more history", utilityProjectRow{Status: "building", AgeDays: 11, Remaining: "needs 3d more history"}.Why())
	require.True(t, strings.HasPrefix(utilityProjectRow{Status: "armed", RecentEvents: 30, AgeDays: 20}.Why(), "ready: ranks by use from the gardener’s next pass"))
	require.Equal(t, "auto", utilityProjectRow{}.Choice())
	require.Equal(t, "on", utilityProjectRow{Forced: "on"}.Choice())
}

// Turns on, Everywhere or Never, decides for every project: the card says
// so, and that the per-project choices wait.
func TestBeRanking_ModeNote(t *testing.T) {
	db, mux := newConsole(t)
	b := config.Defaults().Briefing
	b.UtilityMode = "off"
	require.NoError(t, store.SetBriefingConfig(context.Background(), db, b))
	sec := beSection(t, getPeek(t, mux, "/console/settings?s=engine").Body.String(), "utility-activation")
	require.Contains(t, sec, `<strong>Turns on</strong> is Never in <a href="/console/settings?s=briefing">Briefing</a>, so no project ranks by use.`)
	require.Contains(t, sec, "No project ranks by use yet.")
}

// Recall: the provider's error sits in the row's sub line, as code, and
// Storage is chips with its explanation in a hint.
func TestBeEngine_RecallSubLineAndStorageChips(t *testing.T) {
	page := swEmbedPage(t, EmbeddingRuntime{Provider: "openai", Reason: "llm.NewEmbedder: base_url is not a URL", Misconfigured: true}, false)
	sec := beSection(t, page, "knowledge-engine")
	require.Contains(t, sec, `<span class="set-row-sub">The LLM configuration needs a fix in the config file: <span class="mono">llm.NewEmbedder: base_url is not a URL</span></span>`)
	require.Contains(t, sec, `<span class="badge compact danger">misconfigured</span>`)
	for _, gone := range []string{"runtime-card", "budget-stat", "embedder-facts", "embedding-models-table", "engine-group"} {
		require.NotContains(t, sec, gone)
	}
	require.Contains(t, sec, `<span class="badge compact quiet eng-chip">briefing `)
	require.Contains(t, sec, `<span class="badge compact quiet eng-chip">recall `)
	require.Contains(t, sec, `aria-label="About storage"`)
	require.Contains(t, sec, "Tool events are kept in full.")

	d := settingsData{Budgets: config.Budgets{ToolEventMaxChars: 4000}, Database: databasePanel{Path: "/data/seam.db"}}
	require.Contains(t, d.StorageHint(), "the database (/data/seam.db), which holds")
	require.Contains(t, d.StorageHint(), "A tool event keeps 4,000 characters of each field.")
}

// The vectors line groups its digits, as the rest of the section does.
func TestBeEngine_VectorsLine(t *testing.T) {
	require.Equal(t, "2,208 vectors · 1 item not embedded · 0 on an old model",
		embeddingsPanel{Total: 2208, Missing: 1}.VectorsLine())
	require.Equal(t, "1 vector · 1,500 items not embedded · 1,024 on an old model",
		embeddingsPanel{Total: 1, Missing: 1500, Stale: 1024}.VectorsLine())
}
