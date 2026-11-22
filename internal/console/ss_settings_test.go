package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/backup"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
	"github.com/arctop/seamless/internal/update"
)

// UX 34 (plan:console-ux-pass): Settings' sub-nav carries each section's
// state, every section's head is one line (a title with a hint, one sentence,
// and the source chip where values can be set), and the source chip replaces
// the precedence sentence.

// ssConsole builds an Advanced console whose update check and backup service
// are fakes (a nil one: none in this process).
func ssConsole(t *testing.T, lvl string, st *update.Status, fb *fakeBackups) (*sql.DB, *http.ServeMux) {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	cfg := Config{DB: db, Events: events.NewRecorder(db), APIKey: testKey, Level: lvl, Version: "0.7.2+1a2b3c4",
		DataDir: t.TempDir(), BriefingCfg: config.Defaults().Briefing}
	if st != nil {
		cfg.Updates = &fakeUpdates{db: db, status: *st}
	}
	if fb != nil {
		fb.db, fb.dataDir = db, cfg.DataDir
		fb.status.Settings = backup.Effective(fb.base, config.BackupOverride{}, fb.release, cfg.DataDir)
		if fb.digest == "" {
			fb.digest = strings.Repeat("ab", 32)
		}
		cfg.Backups = fb
	}
	svc, err := New(cfg)
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return db, mux
}

// ssRow cuts a section's sub-nav row out of a page.
func ssRow(t *testing.T, page, id string) string {
	t.Helper()
	row := regexp.MustCompile(`(?s)<a href="/console/settings\?s=` + id + `" data-section-link="` + id + `"[^>]*>.*?</a>`).FindString(page)
	require.NotEmpty(t, row, "the sub-nav has a %s row", id)
	return row
}

// ssChip is the state chip a row draws for word in tone.
func ssChip(word, tone string) string {
	return `<span class="badge compact ` + tone + ` settings-subnav-state" data-section-state>` + template.HTMLEscapeString(word) + `</span>`
}

// ssStatus reads the JSON answer's sectionStatus.
func ssStatus(t *testing.T, mux *http.ServeMux, query string) map[string]sectionState {
	t.Helper()
	var data struct {
		SectionStatus map[string]sectionState `json:"sectionStatus"`
	}
	getJSON(t, mux, "/console/settings?format=json"+query, &data)
	return data.SectionStatus
}

// Each visible row carries its section's state word and tone, the same the
// JSON answer gives, over backups off, failing and fine and updates off and
// available.
func TestSsSubnav_UpdatesAndBackupsStates(t *testing.T) {
	now := time.Now()
	on := true
	upToDate := releaseStatus(now)
	checksOff := releaseStatus(now)
	checksOff.Settings = update.Settings{Check: false, CheckSource: update.SourceConsole, CheckInterval: 6 * time.Hour}
	available := releaseStatus(now)
	available.Newest = &update.Release{Version: ver(0, 7, 3), PublishedAt: now.Add(-24 * time.Hour)}
	available.Available = true

	for _, tc := range []struct {
		name               string
		st                 update.Status
		fb                 *fakeBackups
		upWord, upTone     string
		bkWord, bkTone     string
		upName, bkNameWord string
	}{
		{"backups off, updates off", checksOff, &fakeBackups{}, "Checks off", "quiet", "Off", "quiet", "Updates, checks off", "off"},
		{"backups failing, update available", available, &fakeBackups{base: config.Backup{Auto: &on},
			status: backup.Status{Failures: 2, FailingSince: now.Add(-26 * time.Hour), LastError: "disk full"}},
			"v0.7.3 ready", "accent", "Failing", "danger", "Updates, v0.7.3 ready", "failing"},
		{"backups fine, up to date", upToDate, &fakeBackups{base: config.Backup{Auto: &on},
			status: backup.Status{LastBackup: &backup.Record{Name: "b.tar.gz", At: now.Add(-2*time.Hour - time.Minute)}}},
			"Up to date", "ok", "2h ago", "ok", "Updates, up to date", "2h ago"},
		{"backups on, none yet", upToDate, &fakeBackups{base: config.Backup{Auto: &on}},
			"Up to date", "ok", "None yet", "warn", "Updates, up to date", "none yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.st
			_, mux := ssConsole(t, "advanced", &st, tc.fb)
			for _, section := range settingsSectionIDs() {
				page := getPeek(t, mux, "/console/settings?s="+section).Body.String()
				up, bk := ssRow(t, page, "updates"), ssRow(t, page, "backups")
				require.Contains(t, up, ssChip(tc.upWord, tc.upTone), "s=%s", section)
				require.Contains(t, bk, ssChip(tc.bkWord, tc.bkTone), "s=%s", section)
				require.Contains(t, up, `aria-label="`+template.HTMLEscapeString(tc.upName)+`"`, "the name carries the state")
				require.Contains(t, bk, `aria-label="Backups, `+tc.bkNameWord+`"`)
			}
			status := ssStatus(t, mux, "")
			require.Equal(t, sectionState{Word: tc.upWord, Tone: tc.upTone}, status["updates"])
			require.Equal(t, sectionState{Word: tc.bkWord, Tone: tc.bkTone}, status["backups"])
		})
	}

	// No update check and no backup service in this process: the rows say
	// so, quietly, as the sections do.
	_, mux := ssConsole(t, "advanced", nil, nil)
	page := getPeek(t, mux, "/console/settings").Body.String()
	require.Contains(t, ssRow(t, page, "updates"), ssChip("Not running", "quiet"))
	require.Contains(t, ssRow(t, page, "backups"), ssChip("Not running", "quiet"))
}

// The other sections' states come from the settings data the page already
// builds: the level, the features on, the short version, the preset, the
// project count and semantic recall. Each row reads icon, label and chip,
// its blurb is the link's title, and a rule follows the Basic sections.
func TestSsSubnav_StatesFromTheSettingsData(t *testing.T) {
	db, mux := ssConsole(t, "standard", nil, nil)
	ctx := context.Background()
	for _, slug := range []string{"app", "docs"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Research: true}))

	page := getPeek(t, mux, "/console/settings?s=features").Body.String()
	all := len(features.Registry())
	preset, ok := config.MatchBriefingPreset(config.Defaults().Briefing)
	require.True(t, ok, "the default briefing is a preset")
	for _, tc := range []struct{ id, word, tone string }{
		{"experience", "Standard", "quiet"},
		{"features", fmt.Sprintf("1 of %d on", all), "accent"},
		{"setup", "v0.7.2", "quiet"},
		{"briefing", preset.Label, "quiet"},
		{"workspaces", "2 projects", "quiet"},
	} {
		row := ssRow(t, page, tc.id)
		require.Contains(t, row, ssChip(tc.word, tc.tone), tc.id)
		sec, _ := settingsSectionByID(tc.id)
		require.Contains(t, row, `title="`+template.HTMLEscapeString(sec.Blurb)+`"`, "the blurb is the link's title")
		require.Contains(t, row, `<span class="settings-subnav-label">`+sec.Label+`</span>`)
		require.NotContains(t, row, "<small>", "the blurb left the row")
	}
	require.Contains(t, ssRow(t, page, "features"), `aria-current="page"`)
	require.NotContains(t, page, `data-section-link="engine"`, "Knowledge engine is Advanced")
	require.Equal(t, 1, strings.Count(page, `<span class="settings-subnav-rule" aria-hidden="true"></span>`))
	require.Less(t, strings.Index(page, `data-section-link="backups"`), strings.Index(page, `settings-subnav-rule`))
	require.Less(t, strings.Index(page, `settings-subnav-rule`), strings.Index(page, `data-section-link="briefing"`))

	// Every section has a state in the JSON answer, the hidden ones too.
	status := ssStatus(t, mux, "")
	require.Len(t, status, len(settingsSections))
	require.Equal(t, sectionState{Word: "Keywords only", Tone: "warn"}, status["engine"])

	// A briefing that matches no preset is Custom; none on is quiet.
	custom := config.Defaults().Briefing
	custom.FindingsCount = 17
	require.NoError(t, store.SetBriefingConfig(ctx, db, custom))
	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{}))
	status = ssStatus(t, mux, "")
	require.Equal(t, sectionState{Word: "Custom", Tone: "quiet"}, status["briefing"])
	require.Equal(t, sectionState{Word: fmt.Sprintf("0 of %d on", all), Tone: "quiet"}, status["features"])

	// At Basic only the Basic rows show, with no rule.
	setLevel(t, db, "basic")
	page = getPeek(t, mux, "/console/settings").Body.String()
	require.NotContains(t, page, "settings-subnav-rule")
	require.Contains(t, ssRow(t, page, "experience"), ssChip("Basic", "quiet"))
}

// Your setup warns when semantic recall's settings have a mistake, and says
// why in the link's title; Knowledge engine says Semantic once recall is.
func TestSsSubnav_SetupAndEngineStates(t *testing.T) {
	require.Equal(t, sectionState{Word: "v0.7.2", Tone: "quiet"}, setupState("v0.7.2", "0.7.2+1a2b3c4", embeddingsPanel{}))
	bad := setupState("v0.7.2", "0.7.2", embeddingsPanel{EmbeddingRuntime: EmbeddingRuntime{Misconfigured: true, Reason: "x"}})
	require.Equal(t, "warn", bad.Tone)
	require.Equal(t, "v0.7.2", bad.Word)
	e := settingsNavEntry{settingsSection: settingsSections[2], State: &bad}
	require.Equal(t, "Version, files, and what is connected. Semantic recall is misconfigured.", e.Title())
	require.Equal(t, "Your setup, v0.7.2", e.Name())
	dev := setupState("", "0.10.7-0.20261122162319-676bca296c1c-dirty", embeddingsPanel{})
	require.Equal(t, sectionState{Word: "Dev build", Tone: "quiet", Why: "Version 0.10.7-0.20261122162319-676bca296c1c-dirty."}, dev,
		"a build that is not a release says so, with its whole version in the title")
	require.Equal(t, "Version unknown", setupState("", "", embeddingsPanel{}).Word)
	require.Equal(t, sectionState{Word: "Semantic", Tone: "ok"}, engineState(embeddingsPanel{EmbeddingRuntime: EmbeddingRuntime{Enabled: true}}))

	svc := &Service{cfg: Config{Version: "0.10.6+1a2b3c4"}}
	short, full := svc.shortVersion()
	require.Equal(t, "v0.10.6", short)
	require.Equal(t, "0.10.6+1a2b3c4", full)
	svc.cfg.Version = "0.0.0-dev+1a2b3c4"
	short, full = svc.shortVersion()
	require.Empty(t, short)
	require.Equal(t, "0.0.0-dev+1a2b3c4", full)
}

// Updates' word in every state, from update.Status, with parsed versions only.
func TestSsUpdatesState_Words(t *testing.T) {
	now := time.Now()
	auto := func() update.Status { return autoStatus(now) }
	for _, tc := range []struct {
		name       string
		st         func() update.Status
		word, tone string
	}{
		{"up to date", auto, "Up to date", "ok"},
		{"stopped", func() update.Status { st := auto(); st.Stopped = true; return st }, "Stopped", "danger"},
		{"updating", func() update.Status {
			st := auto()
			st.Applying = &update.Applying{From: ver(0, 7, 2), To: ver(0, 7, 3), Stage: update.StageInstall}
			return st
		}, "Updating", "accent"},
		{"paused", func() update.Status {
			st := auto()
			st.Paused = &update.Pause{Reason: update.PauseRollbacks, Versions: []update.Version{ver(0, 7, 3)}, At: now}
			return st
		}, "Paused", "warn"},
		{"paused, broken", func() update.Status {
			st := auto()
			st.Paused = &update.Pause{Reason: update.PauseBroken, At: now}
			return st
		}, "Paused", "danger"},
		{"a recent update failed", func() update.Status {
			st := auto()
			st.LastAttempt = &update.AttemptResult{From: ver(0, 7, 2), To: ver(0, 7, 3), Why: update.WhyAuto,
				Outcome: update.OutcomeRolledBack, FoldedAt: now.Add(-time.Hour)}
			return st
		}, "Failed", "danger"},
		{"checks off", func() update.Status {
			st := auto()
			st.Settings = update.Effective(autoBase, config.UpdateOverride{Check: boolPtr(false)}, true)
			return st
		}, "Checks off", "quiet"},
		{"a newer release", func() update.Status {
			st := auto()
			st.Newest = &update.Release{Version: ver(0, 8, 0), PublishedAt: now}
			st.Available = true
			return st
		}, "v0.8.0 ready", "accent"},
		{"checks failing", func() update.Status {
			st := auto()
			st.CheckError = &update.CheckError{Kind: update.CheckErrorUnavailable, Count: 2, Since: now.Add(-time.Hour)}
			return st
		}, "Check failed", "warn"},
		{"not checked yet", func() update.Status {
			st := auto()
			st.Newest, st.CheckedAt = nil, time.Time{}
			return st
		}, "Not checked yet", "quiet"},
		{"a source build that checks", func() update.Status {
			st := auto()
			st.Version, st.Release = "0.0.0-dev", false
			return st
		}, "Checks on", "quiet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := updatesState(tc.st(), now)
			require.Equal(t, sectionState{Word: tc.word, Tone: tc.tone}, got)
		})
	}
}

// Backups' word in every state, from the backup service's status.
func TestSsBackupsState_Words(t *testing.T) {
	now := time.Now()
	on := backup.Settings{Auto: true, Every: 24 * time.Hour, Keep: 7}
	last := &backup.Record{Name: "b.tar.gz", At: now.Add(-3 * time.Hour)}
	for _, tc := range []struct {
		name       string
		st         backup.Status
		word, tone string
	}{
		{"fine", backup.Status{Settings: on, LastBackup: last}, "3h ago", "ok"},
		{"off", backup.Status{Settings: backup.Settings{Every: 24 * time.Hour}, LastBackup: last}, "Off", "quiet"},
		{"none yet", backup.Status{Settings: on}, "None yet", "warn"},
		{"one failed", backup.Status{Settings: on, LastBackup: last, Failures: 1, FailingSince: now.Add(-time.Hour)}, "Failing", "danger"},
		{"stopped", backup.Status{Settings: on, Stopped: true}, "Stopped", "danger"},
		{"running", backup.Status{Settings: on, Running: &backup.Running{StartedAt: now}}, "Backing up", "accent"},
		{"overdue", backup.Status{Settings: on, LastBackup: last, LastConfirmed: now.Add(-30 * 24 * time.Hour)}, "Overdue", "warn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, sectionState{Word: tc.word, Tone: tc.tone}, backupsState(tc.st, now))
		})
	}
}

// ssSection cuts the open section out of a Settings page: from its <section
// to the page's script.
func ssSection(t *testing.T, page, anchor string) string {
	t.Helper()
	i := strings.Index(page, `id="`+anchor+`"`)
	require.GreaterOrEqual(t, i, 0, "the %s section renders", anchor)
	j := strings.Index(page[i:], `window.SEAM_NO_LIVE_REFRESH`)
	require.Greater(t, j, 0)
	return page[i : i+j]
}

// Every section's head is one line: no kicker, no second header, no Editable
// here or Read only chip, no precedence line. Its title carries a hint where
// the old paragraph said more, and the source chip sits in the head of every
// section whose values can be set here.
func TestSsHeads_OneLineAtEverySection(t *testing.T) {
	st := releaseStatus(time.Now())
	_, mux := ssConsole(t, "advanced", &st, &fakeBackups{})
	for _, tc := range []struct {
		id       string
		hint     bool
		chip     bool
		sentence string
	}{
		{"experience", true, true, "How much of the console you see."},
		{"features", true, true, "Optional parts of Seamless, for you and your agents alike."},
		{"setup", false, false, "What is running, where it keeps things, and which agents are connected."},
		{"updates", true, true, "New releases, and whether this install takes them by itself."},
		{"backups", true, true, "Copies of everything, on a schedule."},
		{"briefing", true, true, "What every new agent session starts with."},
		{"workspaces", false, false, "See which project each repository resolves to"},
		{"engine", true, false, "How recall finds knowledge, how the briefing ranks it, and where it all lives."},
	} {
		sec, _ := settingsSectionByID(tc.id)
		page := getPeek(t, mux, "/console/settings?s="+tc.id).Body.String()
		body := ssSection(t, page, sec.Anchor)
		for _, gone := range []string{"settings-precedence", "settings-zone-kicker", "settings-access", " Editable here</span>", " Read only</span>",
			"features-workbench-head", "briefing-workbench-head", "Following file + env", "override active", "Reset to file + env",
			"Reset to the config file", "Set in this console", "Chosen in the console"} {
			require.NotContains(t, body, gone, "s=%s", tc.id)
		}
		head := regexp.MustCompile(`(?s)<header class="settings-section-head[^"]*">.*?</header>`).FindString(body)
		require.NotEmpty(t, head, "s=%s has a head", tc.id)
		require.Equal(t, 1, strings.Count(head, "<h2>"), "s=%s: one title", tc.id)
		require.Equal(t, 1, strings.Count(head, "<p>"), "s=%s: one sentence", tc.id)
		require.Contains(t, head, tc.sentence, "s=%s", tc.id)
		require.Equal(t, tc.hint, strings.Contains(head, `<span class="hint">`), "s=%s: a hint in the title", tc.id)
		require.Equal(t, tc.chip, strings.Contains(head, `<span class="src-chip`), "s=%s: the source chip", tc.id)
		if !tc.chip && tc.id != "engine" {
			require.NotContains(t, body, "src-chip", "s=%s: nothing here is set in the console", tc.id)
		}
	}

	// Knowledge engine: the Gardener group's head carries the chip, and the
	// read-only storage group a lock where its values are shown.
	engine := ssSection(t, getPeek(t, mux, "/console/settings?s=engine").Body.String(), "knowledge-engine")
	require.Equal(t, 1, strings.Count(engine, `<span class="src-chip is-`))
	gardener := regexp.MustCompile(`(?s)<header class="engine-group-head">\s*<h3 id="engine-gardener-title">.*?</header>`).FindString(engine)
	require.Contains(t, gardener, `<span class="src-chip is-file">`)
	require.Contains(t, engine, `<h3 id="engine-storage-title"><span class="settings-readonly" role="img" aria-label="Read only" title="Read only">`+string(icon("lock"))+`</span>Storage &amp; policy</h3>`)
	require.NotContains(t, engine, `class="gardener-source"><strong>`, "the per-switch source lines gave way to the chip")
}

// A value the config file makes final reads "Config file decides" once in
// its section: in the head when it decides the whole section (update.check
// false), beside its switch when it decides one of several (update.auto
// false, gardener.agents_may_apply false).
func TestSsConfigFileDecides_SaidOnce(t *testing.T) {
	now := time.Now()
	sectionOf := func(page, anchor string) string {
		t.Helper()
		return ssSection(t, page, anchor)
	}

	locked := config.Update{Check: boolPtr(false)}
	st := releaseStatus(now)
	st.Settings = update.Effective(locked, config.UpdateOverride{}, true)
	_, mux, _ := newUpdatesConsole(t, locked, &st)
	sec := sectionOf(getPeek(t, mux, "/console/settings?s=updates").Body.String(), "updates")
	require.Equal(t, 1, strings.Count(sec, "Config file decides"))
	head := regexp.MustCompile(`(?s)<header class="settings-section-head">.*?</header>`).FindString(sec)
	require.Contains(t, head, `<span class="src-chip is-locked">`, "the file decides the whole section: the head says so")
	require.Contains(t, sec, `type="submit" disabled>Turn update checks on</button>`)

	noAuto := autoBase
	noAuto.Auto = boolPtr(false)
	st = autoStatus(now)
	st.Settings = update.Effective(noAuto, config.UpdateOverride{}, true)
	_, mux, _ = newUpdatesConsole(t, noAuto, &st)
	sec = sectionOf(getPeek(t, mux, "/console/settings?s=updates").Body.String(), "updates")
	require.Equal(t, 1, strings.Count(sec, "Config file decides"))
	auto := formFor(sec, "/console/settings/updates/auto")
	require.Contains(t, auto, `disabled>Turn automatic updates on</button>`)
	require.Contains(t, auto, `<span class="src-chip is-locked">`, "beside the switch it decides")
	head = regexp.MustCompile(`(?s)<header class="settings-section-head">.*?</header>`).FindString(sec)
	require.Contains(t, head, `<span class="src-chip is-file">`, "the head says where the rest comes from")

	c := newAaConsole(t, config.Gardener{AgentsMayApply: new(false)})
	page := getPeek(t, c.mux, "/console/settings?s=engine").Body.String()
	sec = sectionOf(page, "knowledge-engine")
	require.Equal(t, 1, strings.Count(sec, "Config file decides"))
	agents := regexp.MustCompile(`(?s)<form method="post" action="/console/settings/gardener" aria-labelledby="gardener-agents-title">.*?</form>`).FindString(sec)
	require.Contains(t, agents, `disabled>Let agents apply proposals</button>`)
	require.Contains(t, agents, `<span class="src-chip is-locked">`)
}

// Features' and Briefing's Reset keep the confirm they asked with, word for
// word, on the source chip's submit button; Experience's never asked.
func TestSsResetConfirmsRideTheSourceChip(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "advanced")
	ctx := context.Background()
	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{Research: true}))
	require.NoError(t, store.SetBriefingConfig(ctx, db, config.Defaults().Briefing))
	for _, tc := range []struct{ section, form, sentence string }{
		{"features", "features-reset", "Clear the stored override and go back to the file/env configuration? No data is deleted."},
		{"briefing", "brief-reset", "Clear the console override and go back to the file/env configuration?"},
	} {
		page := getPeek(t, mux, "/console/settings?s="+tc.section).Body.String()
		require.Contains(t, page, `<button class="btn small src-chip-reset" type="submit" form="`+tc.form+`" data-confirm="`+
			template.HTMLEscapeString(tc.sentence)+`" data-confirm-label="Reset it">Reset</button>`)
		require.Contains(t, page, `<form id="`+tc.form+`" method="post"`)
	}
	setLevel(t, db, "standard")
	page := getPeek(t, mux, "/console/settings?s=experience").Body.String()
	require.Contains(t, page, `<button class="btn small src-chip-reset" type="submit" form="level-reset">Reset</button>`)
}

// The JSON answer keeps every field it had and adds only sectionStatus: one
// {word, tone} per section, the same at every ?s=. cmd/seam doctor reads
// featuresConfig from it.
func TestSsJSON_KeepsEveryFieldAndAddsSectionStatus(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{Research: true}, "standard")
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, db, "app", "app")
	require.NoError(t, err)
	setLevel(t, db, "standard") // a stored level, so consoleLevelSource (omitempty) is there

	keys := func(m map[string]any) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	raw := func(query string) map[string]any {
		var m map[string]any
		getJSON(t, mux, "/console/settings?format=json"+query, &m)
		return m
	}
	want := []string{"briefing", "briefingOverridden", "budgets", "consoleLevel", "consoleLevelOverridden", "consoleLevelSource",
		"dataDir", "database", "embeddings", "families", "familyEditors", "familyOptions", "features", "featuresConfig",
		"featuresOverridden", "gardener", "gardenerSwitches", "projects", "repoMap", "sectionStatus", "unboundRepos",
		"utilityReady", "utilityRows", "workspaces"}
	first := raw("")
	require.Equal(t, want, keys(first))
	gardenerKeys := []string{"AgentsLabel", "AgentsLocked", "AgentsMayApply", "AgentsNote", "AgentsSource", "AutoApply",
		"AutoLabel", "AutoNote", "AutoSource", "Kinds", "LockedNote", "Overridden", "ReadFailed", "Unreadable"}
	require.Equal(t, gardenerKeys, keys(first["gardenerSwitches"].(map[string]any)), "the gardener switches keep their fields")

	status := first["sectionStatus"].(map[string]any)
	require.Equal(t, settingsSectionIDs(), func() []string {
		ids := keys(status)
		order := settingsSectionIDs()
		slices.SortFunc(ids, func(a, b string) int { return slices.Index(order, a) - slices.Index(order, b) })
		return ids
	}())
	for id, v := range status {
		require.Equal(t, []string{"tone", "word"}, keys(v.(map[string]any)), id)
	}
	b, err := json.Marshal(status["workspaces"])
	require.NoError(t, err)
	require.JSONEq(t, `{"word":"1 project","tone":"quiet"}`, string(b))
	for _, id := range settingsSectionIDs() {
		require.Equal(t, first, raw("&s="+id), "s=%s", id)
	}
}

// Under peer_auth the project count is the request's own read: a locked
// project the caller cannot see is not counted.
func TestSsSubnav_LockedProjectIsNotCounted(t *testing.T) {
	lc := newLockedConsole(t)
	code, body := lc.get(t, "/console/settings?format=json", "")
	require.Equal(t, http.StatusOK, code, body)
	var data settingsData
	require.NoError(t, json.Unmarshal([]byte(body), &data))
	var slugs []string
	for _, p := range data.Projects {
		slugs = append(slugs, p.Slug)
	}
	require.Equal(t, plural(len(slugs), "project", "projects"), data.SectionStatus["workspaces"].Word,
		"the chip counts what the section lists")
	require.NotContains(t, slugs, lockedSlug, "the locked project is absent from the read")
	require.Contains(t, slugs, openSlug)
}

// console.css: the UX 34 section styles the rows and heads, and the rules
// nothing uses any more are gone.
func TestSsStyles_RowsHeadsAndNoDeadRules(t *testing.T) {
	css := string(consoleCSS)
	require.Contains(t, css, "/* UX 34: Settings' sub-nav states and one-line heads.")
	for _, rule := range []string{".settings-subnav-state {", ".settings-subnav-label {", ".settings-subnav-rule {",
		".settings-section-head > .src-chip {", ".engine-group-head > .src-chip {"} {
		require.Contains(t, css, rule)
	}
	for _, gone := range []string{".settings-zone-kicker", ".settings-access", ".settings-precedence", ".settings-subnav-copy",
		".features-workbench-head", ".briefing-workbench-head"} {
		require.NotContains(t, css, gone)
	}
	for _, name := range []string{"templates/settings.html", "templates/settings_backups.html"} {
		src, err := templateFS.ReadFile(name)
		require.NoError(t, err)
		for _, gone := range []string{"settings-zone-kicker", "settings-access", "settings-precedence", "settings-subnav-copy"} {
			require.NotContains(t, string(src), gone, name)
		}
	}
}
