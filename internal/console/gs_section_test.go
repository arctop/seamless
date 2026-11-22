package console

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// Settings > Gardener (plan:console-ux-pass UX 37): the gardener's switches
// and cadence as a Basic section of their own, beside the Gardener screen.

// gsConsole is newAaConsole at a level of the test's choosing: a console with
// a real gardener over a fresh store, base as the file/env gardener config.
func gsConsole(t *testing.T, base config.Gardener, lvl string) aaConsole {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	dataDir := filepath.Join(dir, "data")
	mgr, err := files.NewManager(dataDir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	rec := events.NewRecorder(db)
	if base.IntervalMinutes == 0 {
		base.IntervalMinutes = 60
	}
	garden := gardener.New(db, mgr, nil, nil, rec, gardener.FromConfig(base), slog.Default())
	svc, err := New(Config{DB: db, Files: mgr, Gardener: garden, Events: rec, DataDir: dataDir, APIKey: testKey,
		Level: lvl, GardenerCfg: base})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return aaConsole{db: db, mux: mux, svc: svc, rec: rec}
}

// gsSection is the Gardener section of a rendered Settings page.
func gsSection(t *testing.T, mux *http.ServeMux) string {
	t.Helper()
	rr := getPeek(t, mux, "/console/settings?s=gardener")
	require.Equal(t, http.StatusOK, rr.Code)
	return ssSection(t, rr.Body.String(), gardenerSettingsAnchor)
}

// The registry has Gardener at Basic, right after Backups, with the sprout
// and the anchor the Gardener screen links to; the docs' level matrix and
// the Experience card's list are built from it.
func TestGsRegistry_GardenerAtBasicAfterBackups(t *testing.T) {
	ids := settingsSectionIDs()
	at := -1
	for i, id := range ids {
		if id == "gardener" {
			at = i
		}
	}
	require.Positive(t, at)
	require.Equal(t, "backups", ids[at-1], "placed right after backups")
	sec, ok := settingsSectionByID("gardener")
	require.True(t, ok)
	require.Equal(t, settingsSection{ID: "gardener", Label: "Gardener", Icon: "sprout", Min: levelBasic,
		Anchor: "settings-gardener", Blurb: sec.Blurb}, sec)
	require.NotContains(t, strings.ToLower(sec.Blurb), "proposal",
		"the blurb rides every page's palette, and the Held screen's empty state must not say proposal")
	require.Contains(t, visibleSettingsSections(levelBasic), sec)
	require.Equal(t, "/console/settings?s=gardener#settings-gardener", gardenerSettingsHref)

	var row *LevelMatrixRow
	for _, r := range LevelMatrix() {
		if r.Group == MatrixSections && r.Name == "Gardener" {
			row = &r
		}
	}
	require.NotNil(t, row, "the docs' level matrix lists the section")
	require.Equal(t, "basic", row.Min)
	require.Equal(t, sec.Blurb, row.Note)
	require.Contains(t, levelAddsScreens(config.Features{}, levelBasic),
		"Settings: Experience, Features, Your setup, Updates, Backups, and Gardener")
}

// At Basic the section opens in its place in the sub-nav, with no level
// banner, and draws both switch forms, saving at once, and the cadence card.
func TestGsSection_RendersAtBasic(t *testing.T) {
	c := gsConsole(t, config.Gardener{Enabled: true, DedupThreshold: 0.88, StalenessDays: 90, DigestDays: 30,
		ToolEventRetentionDays: 0, StalePlanDays: 14, SessionIdleMinutes: 50}, "basic")
	rr := getPeek(t, c.mux, "/console/settings?s=gardener")
	require.Equal(t, http.StatusOK, rr.Code)
	page := rr.Body.String()
	require.NotContains(t, page, "data-level-banner", "Gardener is a Basic section")
	require.Contains(t, ssRow(t, page, "gardener"), `aria-current="page"`)
	require.Contains(t, page, `<title>Settings · Gardener`)

	sec := ssSection(t, page, gardenerSettingsAnchor)
	head := regexp.MustCompile(`(?s)<header class="settings-section-head">.*?</header>`).FindString(sec)
	require.Contains(t, head, "<h2>Gardener<span class=\"hint\">")
	require.Contains(t, head, "<p>Who may act on its proposals, and which apply by themselves.</p>")
	require.Contains(t, head, `<span class="src-chip is-file">`, "the source chip heads the section")

	// Two forms, each saving at once, each carrying only its own fields.
	forms := regexp.MustCompile(`(?s)<form method="post" action="/console/settings/gardener" class="gs-card[^"]*" data-autosave[^>]*>.*?</form>`).FindAllString(sec, -1)
	require.Len(t, forms, 2)
	agents, auto := forms[0], forms[1]
	require.Contains(t, agents, `<input type="hidden" name="agents_set" value="1">`)
	require.Contains(t, agents, `<input type="checkbox" role="switch" name="agents" value="on" aria-labelledby="gs-agents-label" checked>`)
	require.NotContains(t, agents, `name="auto`)
	require.Contains(t, agents, "Agents apply, dismiss and hide proposals. You always can.")
	require.Contains(t, auto, `<input type="hidden" name="auto_set" value="1">`)
	require.NotContains(t, auto, `name="agents`)
	require.Contains(t, auto, `<span class="badge compact quiet gs-count">0 of 4</span>`)
	require.Equal(t, len(gardener.AutoSafeKinds()), strings.Count(auto, `<input type="checkbox" role="switch" name="auto"`))
	for _, k := range gardener.AutoSafeKinds() {
		w := gardenerAutoWords[k]
		require.Contains(t, auto, `<strong id="gs-auto-`+k+`">`+w[0]+`</strong>`, k)
		require.Contains(t, auto, `<span class="set-row-sub">`+w[2]+`</span>`, "%s: the Undo sentence under the label", k)
	}
	require.Contains(t, auto, `on the <a href="/console/gardener">Gardener screen</a>`)
	// The lockdown, screening and fenced-project paragraph moved into the
	// group's hint.
	require.Contains(t, auto, "It waits for you instead while lockdown is on, when write screening would hold it, and for a locked or fenced project.</span>")
	require.NotContains(t, sec, `class="gardener-source"`)
	require.NotContains(t, sec, "Save automatic apply", "the switches save at once")
	require.Equal(t, 2, strings.Count(sec, `<button class="btn small gs-save" type="submit">Save</button>`), "without script each form keeps a Save")

	// The cadence card: the interval, its two chips, and the tuning folded.
	require.Contains(t, sec, `Runs every <strong>60 min</strong>`)
	require.Contains(t, sec, `<span class="badge compact ok">suggestions on</span><span class="badge compact quiet">upkeep always</span>`)
	require.Contains(t, sec, `<summary>Tuning, from the config file</summary>`)
	for _, fact := range []string{
		"<dt>Dedup threshold</dt><dd>0.88</dd>", "<dt>Memory stale after</dt><dd>90 d</dd>",
		"<dt>Digest window</dt><dd>30 d</dd>", "<dt>Tool events kept</dt><dd>off</dd>",
		"<dt>Stale plan after</dt><dd>14 d</dd>", "<dt>Session idle after</dt><dd>50 m</dd>",
	} {
		require.Contains(t, sec, fact)
	}

	// Suggestions off: the card says so, and upkeep keeps the clock.
	off := gsConsole(t, config.Gardener{}, "basic")
	sec = gsSection(t, off.mux)
	require.Contains(t, sec, `<span class="badge compact quiet">suggestions off</span>`)
	require.Contains(t, sec, "gardener.enabled is false, so no pass proposes anything.")
	require.Contains(t, sec, "<dt>Session idle after</dt><dd>45 m</dd>")
}

// Knowledge engine keeps recall, ranking and storage, and draws none of the
// gardener: no switches, no cadence card.
func TestGsEngine_NoGardenerSwitches(t *testing.T) {
	c := gsConsole(t, config.Gardener{Enabled: true}, "advanced")
	rr := getPeek(t, c.mux, "/console/settings?s=engine")
	require.Equal(t, http.StatusOK, rr.Code)
	engine := ssSection(t, rr.Body.String(), "knowledge-engine")
	for _, gone := range []string{`id="settings-gardener"`, `action="/console/settings/gardener`, "gardener-runtime",
		"gardener-switches", "Agents may apply proposals", "Apply by themselves", "Dedup threshold", "suggestions on"} {
		require.NotContains(t, engine, gone)
	}
	for _, kept := range []string{`id="semantic-index"`, `id="utility-activation"`, `id="runtime-profile"`} {
		require.Contains(t, engine, kept)
	}
	src, err := templateFS.ReadFile("templates/settings_engine.html")
	require.NoError(t, err)
	require.NotContains(t, string(src), "settings-gardener", "the engine no longer calls the switches")
}

// Every server-side link to the switches points at ?s=gardener, and the
// page's client map sends an old ?s=engine#settings-gardener there.
func TestGsLinks_PointAtTheGardenerSection(t *testing.T) {
	old := regexp.MustCompile(`s=engine#settings-gardener|s=engine&[^"']*#settings-gardener`)
	err := fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		src, rerr := templateFS.ReadFile(path)
		require.NoError(t, rerr)
		require.False(t, old.Match(src), "%s links to the switches' old place", path)
		return nil
	})
	require.NoError(t, err)
	goFiles, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, name := range goFiles {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		require.False(t, old.Match(src), "%s links to the switches' old place", name)
	}

	// The Gardener screen's sentence links to the section.
	c := gsConsole(t, config.Gardener{Enabled: true}, "basic")
	screen := getPeek(t, c.mux, "/console/gardener").Body.String()
	require.Contains(t, screen, `<a href="/console/settings?s=gardener#settings-gardener">Settings &rsaquo; Gardener</a>`)

	// The legacy map: #settings-gardener opens ?s=gardener even under
	// s=engine, landing on the block; every other hash still maps only when
	// the URL names no section.
	page := settingsPageSource(t)
	require.Contains(t, page, `'#runtime-profile': 'engine', '#settings-gardener': 'gardener' };`)
	require.Contains(t, page, `var to = legacy[location.hash], at = /[?&]s=([^&#]*)/.exec(location.search);`)
	require.Contains(t, page, `if (to && (!at || (to === 'gardener' && at[1] === 'engine')) && window.SeamConsole) window.SeamConsole.load('/console/settings?s=' + to + (to === 'gardener' ? location.hash : ''), { history: 'replace', source: 'query' });`)
	require.NotContains(t, page, `!/[?&]s=/.test(location.search)`, "the one condition replaced the old one")
}

// The switches' POSTs return to ?s=gardener on the block, the reset too.
func TestGsRoutes_ReturnToTheGardenerSection(t *testing.T) {
	c := gsConsole(t, config.Gardener{}, "basic")
	for _, tc := range []struct{ path, body, kind string }{
		{"/console/settings/gardener", "agents_set=1", "notice"},
		{"/console/settings/gardener", "auto_set=1&auto=tool_error", "notice"},
		{"/console/settings/gardener", "agents=maybe", "error"},
		{"/console/settings/gardener/reset", "", "notice"},
	} {
		rr := postForm(c.mux, tc.path, tc.body)
		require.Equal(t, http.StatusSeeOther, rr.Code, tc.body)
		loc := rr.Header().Get("Location")
		require.True(t, strings.HasPrefix(loc, "/console/settings?s=gardener&"+tc.kind+"="), loc)
		require.True(t, strings.HasSuffix(loc, "#settings-gardener"), loc)
	}
}

// agents_set=1 is the switch's form: agents=on present is on, absent is off.
// agents=on|off without it is as before, and every refusal stands
// (TestAaSettingsGardenerSave's table has the marker's own).
func TestGsSave_AgentsSetMarker(t *testing.T) {
	c := gsConsole(t, config.Gardener{AutoApply: []string{"ship_plan"}}, "basic")
	ctx := context.Background()
	stored := func() config.GardenerOverride {
		o, _, err := store.GardenerOverride(ctx, c.db)
		require.NoError(t, err)
		return o
	}

	kind, msg := aaFlash(t, c, "/console/settings/gardener", "agents_set=1")
	require.Equal(t, "notice", kind)
	require.Contains(t, msg, "Agents may no longer apply, dismiss or hide proposals")
	require.Equal(t, config.GardenerOverride{AgentsMayApply: new(false)}, stored(), "the marker alone is off, and only this switch")
	require.False(t, c.svc.gardenerSwitches(ctx).AgentsMayApply)
	require.Equal(t, []string{"ship_plan"}, c.svc.gardenerSwitches(ctx).AutoApply, "the other switch keeps the file's value")

	kind, msg = aaFlash(t, c, "/console/settings/gardener", "agents_set=1&agents=on")
	require.Equal(t, "notice", kind)
	require.Contains(t, msg, "Agents may apply, dismiss and hide proposals again.")
	require.Equal(t, config.GardenerOverride{AgentsMayApply: new(true)}, stored())

	kind, _ = aaFlash(t, c, "/console/settings/gardener", "agents_set=1&agents=off")
	require.Equal(t, "notice", kind)
	require.Equal(t, config.GardenerOverride{AgentsMayApply: new(false)}, stored(), "an explicit off under the marker is off")

	kind, _ = aaFlash(t, c, "/console/settings/gardener", "agents=on")
	require.Equal(t, "notice", kind, "the old field alone still saves")
	require.Equal(t, config.GardenerOverride{AgentsMayApply: new(true)}, stored())

	// A refused marker stores nothing.
	require.NoError(t, store.ClearGardenerOverride(ctx, c.db))
	for _, body := range []string{"agents_set=0", "agents_set=1&agents_set=1&agents=on", "agents_set=1&agents=yes", "agents_set=1&agents=on&agents=off"} {
		kind, _ := aaFlash(t, c, "/console/settings/gardener", body)
		require.Equal(t, "error", kind, body)
		_, found, err := store.GardenerOverride(ctx, c.db)
		require.NoError(t, err)
		require.False(t, found, "nothing stored for %q", body)
	}
}

// The marker changes nothing about who decides: an agent's process and a
// paired device are refused in both directions with nothing stored, and
// agents_may_apply: false in the file still refuses an on.
func TestGsSave_AgentsSetStaysTheOwners(t *testing.T) {
	for _, caller := range uohCallers(t) {
		if caller.owner {
			continue
		}
		t.Run(caller.name, func(t *testing.T) {
			c := gsConsole(t, config.Gardener{}, "basic")
			for _, body := range []string{"agents_set=1", "agents_set=1&agents=on"} {
				uohRequireRefused(t, uohPost(t, c.mux, caller, "/console/settings/gardener", body), caller, body)
				_, found, err := store.GardenerOverride(context.Background(), c.db)
				require.NoError(t, err)
				require.False(t, found, "nothing is stored")
			}
		})
	}

	c := gsConsole(t, config.Gardener{AgentsMayApply: new(false)}, "basic")
	kind, msg := aaFlash(t, c, "/console/settings/gardener", "agents_set=1&agents=on")
	require.Equal(t, "error", kind)
	require.Contains(t, msg, "Agents stay barred from applying proposals: gardener.agents_may_apply: false is set in the config file")
	_, found, err := store.GardenerOverride(context.Background(), c.db)
	require.NoError(t, err)
	require.False(t, found)
	require.False(t, c.svc.gardenerSwitches(context.Background()).AgentsMayApply)
}

// The sub-nav chip: what the gardener may change without the owner, the
// problem first.
func TestGsSubnav_GardenerState(t *testing.T) {
	on := config.Gardener{Enabled: true}
	for _, tc := range []struct {
		name string
		cfg  config.Gardener
		sw   gardenerSwitches
		want sectionState
	}{
		{"default", on, gardenerSwitches{AgentsMayApply: true}, sectionState{Word: "Proposes only", Tone: "quiet"}},
		{"agents barred", on, gardenerSwitches{}, sectionState{Word: "Only you decide", Tone: "quiet"}},
		{"automatic", on, gardenerSwitches{AutoApply: []string{"ship_plan", "tool_error"}}, sectionState{Word: "2 of 4 automatic", Tone: "accent"}},
		{"suggestions off", config.Gardener{}, gardenerSwitches{AutoApply: []string{"ship_plan"}},
			sectionState{Word: "Suggestions off", Tone: "quiet", Why: "gardener.enabled is false, so no pass proposes anything."}},
		{"unreadable", config.Gardener{}, gardenerSwitches{ReadFailed: true, AgentsMayApply: true},
			sectionState{Word: "Unreadable", Tone: "warn", Why: "The stored switches could not be read: the config file's values show, and agents are barred from deciding meanwhile."}},
	} {
		require.Equal(t, tc.want, gardenerState(tc.cfg, tc.sw), tc.name)
	}

	// Drawn from the settings data at every section, and in the JSON answer.
	c := gsConsole(t, on, "basic")
	page := getPeek(t, c.mux, "/console/settings?s=features").Body.String()
	require.Contains(t, ssRow(t, page, "gardener"), ssChip("Proposes only", "quiet"))
	require.Contains(t, ssRow(t, page, "gardener"), `aria-label="Gardener, proposes only"`)
	require.NoError(t, store.SetGardenerOverride(context.Background(), c.db,
		config.GardenerOverride{AutoApply: &[]string{"memory_wanted"}}))
	require.Equal(t, sectionState{Word: "1 of 4 automatic", Tone: "accent"}, ssStatus(t, c.mux, "")["gardener"])
	page = getPeek(t, c.mux, "/console/settings?s=experience").Body.String()
	require.Contains(t, ssRow(t, page, "gardener"), ssChip("1 of 4 automatic", "accent"))
	require.Contains(t, gsSection(t, c.mux), `<span class="badge compact accent gs-count">1 of 4</span>`)
}
