package console

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

func TestSettingsPage(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	_, err = store.EnsureProject(ctx, db, "seamless", "Seamless")
	require.NoError(t, err)
	_, err = store.EnsureProject(ctx, db, "seam", "Seam CLI")
	require.NoError(t, err)
	require.NoError(t, store.AddRepoMapping(ctx, db, "/Users/x/repos/seamless", "seamless"))
	require.NoError(t, store.SetProjectFamilies(ctx, db, map[string][]string{
		"seam-tools": {"seam", "seamless"},
	}))

	svc, err := New(Config{
		DB: db, APIKey: testKey, DataDir: "/home/.seamless",
		Budgets:     config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 800},
		GardenerCfg: config.Gardener{Enabled: true, IntervalMinutes: 60, DedupThreshold: 0.88, StalenessDays: 90, DigestDays: 30, SessionIdleMinutes: 45},
		BriefingCfg: config.Defaults().Briefing,
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)

	var data settingsData
	getJSON(t, mux, "/console/settings?format=json", &data)
	require.Equal(t, "/home/.seamless", data.DataDir)
	require.Equal(t, 1500, data.Budgets.MaxBriefingTokens)
	require.True(t, data.Gardener.Enabled)
	require.Equal(t, 90, data.Gardener.StalenessDays)
	require.Equal(t, 45, data.Gardener.SessionIdleMinutes)
	require.Equal(t, 3, data.Briefing.FindingsCount)
	require.False(t, data.BriefingOverridden)
	require.Len(t, data.Projects, 2)
	require.Len(t, data.RepoMap, 1)
	require.Len(t, data.Families, 1)
	require.Len(t, data.Workspaces, 2)
	require.Len(t, data.FamilyEditors, 1)
	require.Len(t, data.FamilyOptions, 2)
	require.Equal(t, "/Users/x/repos/seamless", data.RepoMap[0].Repo)
	require.Equal(t, "seam", data.Workspaces[0].Slug)
	require.Equal(t, workspaceFamily{Name: "seam-tools", MemberCount: 2}, data.Workspaces[0].Families[0])
	require.Equal(t, familyScopeRef{Slug: "seam", Registered: true}, data.FamilyEditors[0].Members[0])
	require.Equal(t, []repoRoute{{Path: "/Users/x/repos/seamless"}}, data.Workspaces[1].Repos)

	section := func(s string) string {
		req := httptest.NewRequest(http.MethodGet, "/console/settings?s="+s, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr := do(mux, req)
		require.Equal(t, http.StatusOK, rr.Code, s)
		page := rr.Body.String()
		require.Contains(t, page, `class="settings-page" data-settings data-section="`+s+`"`)
		require.Contains(t, page, `aria-label="Settings sections"`)
		require.Contains(t, page, `window.SEAM_NO_LIVE_REFRESH = true`)
		return page
	}

	page := section("briefing")
	require.Contains(t, page, "Briefing injection")
	require.Contains(t, page, `id="briefing-recipe"`)
	require.Contains(t, page, `class="brief-group memory-group"`)
	require.Contains(t, page, `class="brief-group family-group"`)
	require.Contains(t, page, `class="brief-group utility-group"`)
	require.Contains(t, page, `id="utility-mode"`)
	require.Contains(t, page, `name="constraint_max_full"`)
	require.Contains(t, page, `name="convention_max_full"`)
	require.Contains(t, page, `data-briefing-form`)
	require.NotContains(t, page, `id="workspace-registry"`, "one section at a time")

	page = section("engine")
	require.Contains(t, page, `id="runtime-profile"`)
	require.Contains(t, page, `id="semantic-index"`)
	require.Contains(t, page, `id="utility-activation"`)

	page = section("workspaces")
	require.Contains(t, page, "/Users/x/repos/seamless")
	require.Contains(t, page, `id="workspace-registry"`)
	require.Contains(t, page, `class="registry-scroll workspace-directory"`)
	require.Contains(t, page, `class="workspace-row" data-workspace-scope="seamless"`)
	require.Contains(t, page, `class="workspace-route" title="/Users/x/repos/seamless"`)
	require.Contains(t, page, `class="workspace-tag family"`)
	require.Contains(t, page, `class="family-summary"`)
	require.Contains(t, page, `data-dialog-open="family-create-dialog"`)
	require.Contains(t, page, `action="/console/settings/families/save"`)
	require.Contains(t, page, `action="/console/settings/families/delete"`)
	require.NotContains(t, page, `workspace-family-peers`)
	require.NotContains(t, page, `class="repos-panel"`)
	require.NotContains(t, page, `class="families-panel"`)
}

func TestBuildWorkspaceRegistry_JoinsSourcesAndPreservesReferences(t *testing.T) {
	retiredAt := time.Now().UTC()
	projects := []core.Project{
		{Slug: "app", Name: "App", ParentSlug: "shared"},
		{Slug: "shared", Name: "Shared"},
		{Slug: "old", Name: "Old", RetiredAt: &retiredAt},
	}
	// Two hosts, plus the legacy unnamed bucket: a route is (host, path), and
	// the machine a checkout lives on must survive into the view.
	repoRows := []store.RepoMapRow{
		{Host: "mac", Path: "/repos/z-app", Slug: "app"},
		{Host: "mac", Path: "/repos/a-app", Slug: "app"},
		{Host: "argon", Path: "/srv/app", Slug: "app"},
		{Host: "mac", Path: "/repos/future", Slug: "future"},
		{Host: "", Path: "/repos/global", Slug: ""},
	}
	families := map[string][]string{
		"solo":  {"shared"},
		"suite": {"future", "app", "app"},
	}

	workspaces, unbound := buildWorkspaceRegistry(projects, repoRows, families)

	require.Len(t, workspaces, 4)
	require.Equal(t, []string{"app", "shared", "old", "future"}, []string{
		workspaces[0].Slug,
		workspaces[1].Slug,
		workspaces[2].Slug,
		workspaces[3].Slug,
	})
	require.Equal(t, []repoRoute{
		{Host: "argon", Path: "/srv/app"},
		{Host: "mac", Path: "/repos/a-app"},
		{Host: "mac", Path: "/repos/z-app"},
	}, workspaces[0].Repos, "ordered by host then path, so a machine's checkouts stay together")
	require.True(t, workspaces[0].ParentRegistered)
	require.Equal(t, "suite", workspaces[0].Families[0].Name)
	require.Equal(t, 2, workspaces[0].Families[0].MemberCount)
	require.True(t, workspaces[2].Retired)
	require.False(t, workspaces[3].Registered)
	require.Equal(t, []repoRoute{{Host: "mac", Path: "/repos/future"}}, workspaces[3].Repos)
	require.Equal(t, []repoRoute{{Host: "", Path: "/repos/global"}}, unbound,
		"the legacy unnamed bucket is still a route, not a dropped row")

	editors, createOptions := buildFamilyEditors(workspaces, families)
	require.Len(t, editors, 2)
	require.Equal(t, "solo", editors[0].Name)
	require.Equal(t, "suite", editors[1].Name)
	require.Equal(t, []familyScopeRef{
		{Slug: "app", Registered: true},
		{Slug: "future", Registered: false},
	}, editors[1].Members)
	require.Len(t, editors[1].Options, 3)
	require.True(t, editors[1].Options[0].Selected)
	require.False(t, editors[1].Options[1].Selected)
	require.True(t, editors[1].Options[2].Selected)
	require.Equal(t, []string{"app", "shared"}, []string{createOptions[0].Slug, createOptions[1].Slug})
}

func TestSettingsFamilyCreateUpdateAndDelete(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, slug := range []string{"app", "backend", "ops"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}

	svc, err := New(Config{DB: db, APIKey: testKey, BriefingCfg: config.Defaults().Briefing})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)

	rr := postForm(mux, "/console/settings/families/save", url.Values{
		"name":    {"product"},
		"members": {"app", "backend"},
	}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")
	require.Contains(t, rr.Header().Get("Location"), "#workspace-registry")
	families, err := store.ProjectFamilies(ctx, db)
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"product": {"app", "backend"}}, families)

	// A create using an occupied name errors instead of silently replacing it.
	rr = postForm(mux, "/console/settings/families/save", url.Values{
		"name":    {"product"},
		"members": {"ops"},
	}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "error=")
	families, err = store.ProjectFamilies(ctx, db)
	require.NoError(t, err)
	require.Equal(t, []string{"app", "backend"}, families["product"])

	// Editing replaces the member set and can rename the family in one write.
	rr = postForm(mux, "/console/settings/families/save", url.Values{
		"original_name": {"product"},
		"name":          {"platform"},
		"members":       {"backend", "ops"},
	}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	families, err = store.ProjectFamilies(ctx, db)
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"platform": {"backend", "ops"}}, families)

	// The picker is a closed set: a forged or stale unknown slug is rejected.
	rr = postForm(mux, "/console/settings/families/save", url.Values{
		"original_name": {"platform"},
		"name":          {"platform"},
		"members":       {"not-a-project"},
	}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "error=")
	families, err = store.ProjectFamilies(ctx, db)
	require.NoError(t, err)
	require.Equal(t, []string{"backend", "ops"}, families["platform"])

	rr = postForm(mux, "/console/settings/families/delete", url.Values{
		"original_name": {"platform"},
	}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")
	families, err = store.ProjectFamilies(ctx, db)
	require.NoError(t, err)
	require.Empty(t, families)
}

func TestSettingsBriefingSaveAndReset(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	svc, err := New(Config{DB: db, APIKey: testKey, BriefingCfg: config.Defaults().Briefing})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)

	// Save: stores the override row and redirects back with a notice.
	// include_parent_memories is deliberately absent = unchecked = false.
	rr := postForm(mux, "/console/settings/briefing", url.Values{
		"constraint_max_full":        {"12"},
		"convention_max_full":        {"6"},
		"memory_max_age_days":        {"30"},
		"memory_max_items":           {"20"},
		"findings_count":             {"5"},
		"findings_max_age_days":      {"0"},
		"ready_tasks_shown":          {"1"},
		"pending_plan_max_days":      {"14"},
		"stage_unknown_max_age_days": {"10"},
		"hard_cap_multiplier":        {"2"},
		"sibling_findings_count":     {"0"},
		"include_sibling_memories":   {"1"},
		"utility_weight":             {"0.7"},
		"utility_mode":               {"on"},
	}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")

	var data settingsData
	getJSON(t, mux, "/console/settings?format=json", &data)
	require.True(t, data.BriefingOverridden)
	require.Equal(t, 12, data.Briefing.ConstraintMaxFull)
	require.Equal(t, 6, data.Briefing.ConventionMaxFull)
	require.Equal(t, 30, data.Briefing.MemoryMaxAgeDays)
	require.Equal(t, 20, data.Briefing.MemoryMaxItems)
	require.Equal(t, 5, data.Briefing.FindingsCount)
	require.Equal(t, 1, data.Briefing.ReadyTasksShown)
	require.Equal(t, 14, data.Briefing.PendingPlanMaxDays)
	require.Equal(t, 10, data.Briefing.StageUnknownMaxAgeDays)
	require.False(t, data.Briefing.IncludeParentMemories)
	require.True(t, data.Briefing.IncludeSiblingMemories)
	require.Equal(t, 0.7, data.Briefing.UtilityWeight, "the form round-trips the weight, never zeroes it")
	require.Equal(t, "on", data.Briefing.UtilityMode)

	// The saved override is what the retrieval service will read.
	eff, overridden, err := store.BriefingConfig(context.Background(), db, config.Defaults().Briefing)
	require.NoError(t, err)
	require.True(t, overridden)
	require.Equal(t, 5, eff.FindingsCount)

	// Invalid input flashes an error and leaves the stored override untouched.
	rr = postForm(mux, "/console/settings/briefing", "findings_count=-1")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "error=")
	rr = postForm(mux, "/console/settings/briefing", "findings_count=three")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "error=")
	rr = postForm(mux, "/console/settings/briefing", "constraint_max_full=-1")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "error=")
	rr = postForm(mux, "/console/settings/briefing", "convention_max_full=-1")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "error=")
	rr = postForm(mux, "/console/settings/briefing", "utility_weight=1.5")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "error=")
	rr = postForm(mux, "/console/settings/briefing", "utility_mode=sideways")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "error=")
	getJSON(t, mux, "/console/settings?format=json", &data)
	require.Equal(t, 5, data.Briefing.FindingsCount)

	// Reset clears the override; effective values fall back to the file/env base.
	rr = postForm(mux, "/console/settings/briefing/reset", "")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")
	getJSON(t, mux, "/console/settings?format=json", &data)
	require.False(t, data.BriefingOverridden)
	require.Equal(t, 3, data.Briefing.FindingsCount)
	require.Equal(t, 4, data.Briefing.ConstraintMaxFull)
	require.Equal(t, 4, data.Briefing.ConventionMaxFull)
	require.True(t, data.Briefing.IncludeParentMemories)
	require.Equal(t, 0.4, data.Briefing.UtilityWeight)
	require.Equal(t, "auto", data.Briefing.UtilityMode)
}

// The per-project force endpoint flips a scope's activation override and the
// Settings payload reflects it; garbage force values flash instead of saving.
func TestSettingsUtilityForce(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	svc, err := New(Config{DB: db, APIKey: testKey, BriefingCfg: config.Defaults().Briefing})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)

	rr := postForm(mux, "/console/settings/utility", url.Values{
		"project": {"seamless"}, "force": {"on"},
	}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")

	var data settingsData
	getJSON(t, mux, "/console/settings?format=json", &data)
	var row *utilityProjectRow
	for i := range data.UtilityRows {
		if data.UtilityRows[i].Project == "seamless" {
			row = &data.UtilityRows[i]
		}
	}
	require.NotNil(t, row, "the forced scope appears in the activation table")
	require.Equal(t, "active", row.Status)
	require.Equal(t, "on", row.Forced)
	require.False(t, row.EventsOK)
	require.False(t, row.MemoriesOK)
	require.False(t, row.AgeOK)
	require.Empty(t, row.Remaining, "a forced scope owes no readiness hint")

	// The HTML table renders each gate against its threshold.
	req := httptest.NewRequest(http.MethodGet, "/console/settings?s=engine", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	page := do(mux, req)
	require.Equal(t, http.StatusOK, page.Code)
	require.Contains(t, page.Body.String(), "Utility activation by scope")
	require.Contains(t, page.Body.String(), fmt.Sprintf("<i>/%d</i> events", store.UtilityReadyMinEvents))
	require.Contains(t, page.Body.String(), fmt.Sprintf("<i>/%d</i> memories", store.UtilityReadyMinMemories))

	// Clearing the force reverts to the latch (unset here -> building).
	rr = postForm(mux, "/console/settings/utility", url.Values{
		"project": {"seamless"}, "force": {"auto"},
	}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	a, err := store.GetUtilityActivation(context.Background(), db)
	require.NoError(t, err)
	require.Empty(t, a.Projects["seamless"].Forced)

	// An unrecognized force value is an error, never a silent default.
	rr = postForm(mux, "/console/settings/utility", url.Values{
		"project": {"seamless"}, "force": {"sideways"},
	}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "error=")
}

// utilityRemaining spells out exactly the unmet gates, and stays silent for
// scopes with no demand at all (the table already says "no demand yet").
func TestUtilityRemaining(t *testing.T) {
	now := time.Now().UTC()
	mk := func(d store.UtilityProjectDemand) utilityProjectRow {
		row := utilityProjectRow{
			RecentEvents: d.RecentEvents, RecentMemories: d.RecentMemories,
			EventsOK:   d.RecentEvents >= store.UtilityReadyMinEvents,
			MemoriesOK: d.RecentMemories >= store.UtilityReadyMinMemories,
			AgeOK:      !d.Earliest.IsZero() && now.Sub(d.Earliest) >= store.UtilityReadyMinAgeDays*24*time.Hour,
		}
		if !d.Earliest.IsZero() {
			row.AgeDays = int(now.Sub(d.Earliest).Hours() / 24)
		}
		return row
	}

	noDemand := store.UtilityProjectDemand{}
	require.Empty(t, utilityRemaining(mk(noDemand), noDemand))

	allShort := store.UtilityProjectDemand{
		Earliest:     now.Add(-3 * 24 * time.Hour),
		RecentEvents: store.UtilityReadyMinEvents - 7, RecentMemories: store.UtilityReadyMinMemories - 4,
	}
	require.Equal(t, "needs 7 more events, 4 more memories, 11d more history",
		utilityRemaining(mk(allShort), allShort))

	ageOnly := store.UtilityProjectDemand{
		Earliest:     now.Add(-11 * 24 * time.Hour),
		RecentEvents: store.UtilityReadyMinEvents, RecentMemories: store.UtilityReadyMinMemories,
	}
	require.Equal(t, "needs 3d more history", utilityRemaining(mk(ageOnly), ageOnly))

	ready := store.UtilityProjectDemand{
		Earliest:     now.Add(-(store.UtilityReadyMinAgeDays + 1) * 24 * time.Hour),
		RecentEvents: store.UtilityReadyMinEvents, RecentMemories: store.UtilityReadyMinMemories,
	}
	require.Empty(t, utilityRemaining(mk(ready), ready))
}

// The semantic-index panel joins the startup runtime state, the stored
// override, and the vector stats, and flags model groups the running embedder
// no longer writes.
func TestSettingsEmbeddingsPanel(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	require.NoError(t, store.UpsertEmbedding(ctx, db, "01A", "memory", "text-embedding-3-large", []float32{1, 0}))
	require.NoError(t, store.UpsertEmbedding(ctx, db, "01B", "note", "old-model", []float32{0, 1}))

	svc, err := New(Config{
		DB: db, APIKey: testKey, DBPath: dbPath, BriefingCfg: config.Defaults().Briefing,
		Embedding: EmbeddingRuntime{Enabled: true, Provider: "openai", Model: "text-embedding-3-large"},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)

	var data settingsData
	getJSON(t, mux, "/console/settings?format=json", &data)
	e := data.Embeddings
	require.True(t, e.Enabled)
	require.Equal(t, "openai", e.Provider)
	require.Equal(t, "text-embedding-3-large", e.Model)
	require.False(t, e.ModeOff)
	require.False(t, e.RestartNeeded)
	require.Equal(t, 2, e.Total)
	require.Equal(t, 1, e.Stale) // the old-model vector
	require.Len(t, e.Models, 2)
	for _, m := range e.Models {
		require.Equal(t, m.Model != "text-embedding-3-large", m.Stale)
	}
	require.Equal(t, dbPath, data.Database.Path)
	require.Positive(t, data.Database.SizeBytes)
	require.NotEmpty(t, data.Database.SizeHuman)
	require.Positive(t, data.Database.SchemaVersion)

	// The page renders the section with its controls.
	req := httptest.NewRequest(http.MethodGet, "/console/settings?s=engine", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	page := rr.Body.String()
	require.Contains(t, page, `id="semantic-index"`)
	require.Contains(t, page, `action="/console/settings/embeddings/mode"`)
	require.Contains(t, page, `action="/console/settings/embeddings/reembed"`)
	require.Contains(t, page, "text-embedding-3-large")
	require.Contains(t, page, ">stale</span>")

	// Switching off stores the override and, since this process runs enabled,
	// the panel reports a pending restart.
	rr = postForm(mux, "/console/settings/embeddings/mode", url.Values{"mode": {"off"}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")
	require.Contains(t, rr.Header().Get("Location"), "#semantic-index")
	mode, err := store.EmbedderMode(ctx, db)
	require.NoError(t, err)
	require.Equal(t, store.EmbedderModeOff, mode)
	getJSON(t, mux, "/console/settings?format=json", &data)
	require.True(t, data.Embeddings.ModeOff)
	require.True(t, data.Embeddings.RestartNeeded)

	// Clearing it goes back to auto.
	rr = postForm(mux, "/console/settings/embeddings/mode", url.Values{"mode": {"auto"}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	mode, err = store.EmbedderMode(ctx, db)
	require.NoError(t, err)
	require.Equal(t, store.EmbedderModeAuto, mode)

	// An unknown mode is refused with a flash, not stored.
	rr = postForm(mux, "/console/settings/embeddings/mode", url.Values{"mode": {"sideways"}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "error=")
}

// A daemon that started disabled by the override reports the disabled state
// and, once the switch is cleared, a pending restart to re-enable.
func TestSettingsEmbeddingsDisabledStates(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	require.NoError(t, store.SetEmbedderMode(ctx, db, store.EmbedderModeOff))

	svc, err := New(Config{
		DB: db, APIKey: testKey, BriefingCfg: config.Defaults().Briefing,
		Embedding: EmbeddingRuntime{
			Provider: "openai", Model: "text-embedding-3-large",
			Reason: "switched off in the console Settings", OverriddenOff: true,
		},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)

	var data settingsData
	getJSON(t, mux, "/console/settings?format=json", &data)
	require.False(t, data.Embeddings.Enabled)
	require.True(t, data.Embeddings.ModeOff)
	require.False(t, data.Embeddings.RestartNeeded) // stored switch matches the process

	rr := postForm(mux, "/console/settings/embeddings/mode", url.Values{"mode": {"auto"}}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	getJSON(t, mux, "/console/settings?format=json", &data)
	require.False(t, data.Embeddings.ModeOff)
	require.True(t, data.Embeddings.RestartNeeded)

	// Re-embedding is impossible without a files manager (and without an
	// embedder); the console flashes instead of erroring.
	rr = postForm(mux, "/console/settings/embeddings/reembed", "")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "error=")

	// The page names the disabled cause for the owner.
	req := httptest.NewRequest(http.MethodGet, "/console/settings?s=engine", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	page := do(mux, req).Body.String()
	require.Contains(t, page, "Embeddings off")
	require.Contains(t, page, "Switched off from this console")
}

// stubEmbedder is a minimal llm.Embedder for exercising the console's
// re-embed trigger end to end.
type stubEmbedder struct{ model string }

func (s stubEmbedder) Model() string { return s.model }
func (s stubEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{1, 0, 0}, nil
}

// POSTing the re-embed trigger starts the background pass and reports it.
func TestSettingsEmbeddingsReembedTrigger(t *testing.T) {
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	mgr.SetEmbedder(stubEmbedder{model: "new-model"})
	_, err = mgr.WriteMemory(ctx, core.Memory{
		ID: "01K0MEMORY000000000000000A", Kind: core.KindGotcha, Name: "alpha",
		Description: "d", Project: "seam", Body: "b\n",
		Created: time.Now().UTC(), Updated: time.Now().UTC(), ValidFrom: time.Now().UTC(),
	})
	require.NoError(t, err)

	svc, err := New(Config{
		DB: db, Files: mgr, APIKey: testKey, BriefingCfg: config.Defaults().Briefing,
		Embedding: EmbeddingRuntime{Enabled: true, Provider: "openai", Model: "new-model"},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)

	rr := postForm(mux, "/console/settings/embeddings/reembed", "")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	loc := rr.Header().Get("Location")
	require.Contains(t, loc, "notice=")
	require.Contains(t, loc, "#semantic-index")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p := mgr.ReembedStatus(); !p.Running && !p.StartedAt.IsZero() {
			require.Equal(t, 1, p.Done)
			require.Zero(t, p.Failed)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("re-embed pass did not finish before the deadline")
}

// A daemon whose embedder fell back to disabled (no key or model) surfaces the
// cause on the page rather than silently degrading.
func TestSettingsEmbeddingsFallbackReason(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	svc, err := New(Config{
		DB: db, APIKey: testKey, BriefingCfg: config.Defaults().Briefing,
		Embedding: EmbeddingRuntime{
			Provider: "openai", Model: "text-embedding-3-large",
			Reason: "llm.NewEmbedder: openai selected but api_key is empty",
		},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/console/settings?s=engine", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	page := do(mux, req).Body.String()
	require.Contains(t, page, "No usable embedding provider")
	require.Contains(t, page, "api_key is empty")
	require.Contains(t, page, "lexical-only recall")
}

// Settings is one section at a time. Each section renders at its level, a
// section above the level still renders by URL under the banner, and the
// sub-nav offers exactly the level's sections (plus the open one).
func TestSettingsSections_RenderAtTheirLevels(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "basic")

	subnav := regexp.MustCompile(`data-section-link="([a-z]+)"`)
	offered := func(page string) []string {
		var out []string
		for _, m := range subnav.FindAllStringSubmatch(page, -1) {
			out = append(out, m[1])
		}
		return out
	}

	// No s: the default section, experience.
	page := getPeek(t, mux, "/console/settings").Body.String()
	require.Contains(t, page, `data-section="experience"`)
	require.Contains(t, page, `id="experience"`)
	require.Equal(t, []string{"experience", "features", "setup", "updates"}, offered(page))
	require.NotContains(t, page, "data-level-banner")

	for _, tc := range []struct {
		level   string
		section string
		anchor  string
		banner  bool
		subnav  []string
	}{
		{"basic", "features", `id="features"`, false, []string{"experience", "features", "setup", "updates"}},
		{"basic", "setup", `id="setup"`, false, []string{"experience", "features", "setup", "updates"}},
		{"basic", "updates", `id="updates"`, false, []string{"experience", "features", "setup", "updates"}},
		{"basic", "briefing", `id="briefing-recipe"`, true, []string{"experience", "features", "setup", "updates", "briefing"}},
		{"basic", "engine", `id="knowledge-engine"`, true, []string{"experience", "features", "setup", "updates", "engine"}},
		{"standard", "briefing", `id="briefing-recipe"`, false,
			[]string{"experience", "features", "setup", "updates", "briefing", "workspaces"}},
		{"standard", "workspaces", `id="workspace-registry"`, false,
			[]string{"experience", "features", "setup", "updates", "briefing", "workspaces"}},
		{"standard", "engine", `id="knowledge-engine"`, true,
			[]string{"experience", "features", "setup", "updates", "briefing", "workspaces", "engine"}},
		{"advanced", "engine", `id="knowledge-engine"`, false,
			[]string{"experience", "features", "setup", "updates", "briefing", "workspaces", "engine"}},
	} {
		setLevel(t, db, tc.level)
		rr := getPeek(t, mux, "/console/settings?s="+tc.section)
		require.Equal(t, http.StatusOK, rr.Code, "%s at %s", tc.section, tc.level)
		page := rr.Body.String()
		require.Contains(t, page, tc.anchor, "%s at %s renders its content", tc.section, tc.level)
		require.Equal(t, tc.subnav, offered(page), "%s at %s: sub-nav", tc.section, tc.level)
		require.Contains(t, page, `aria-current="page"`, "the open section is marked")
		if tc.banner {
			require.Contains(t, page, `data-level-banner="settings:`+tc.section+`"`, "%s at %s", tc.section, tc.level)
			require.Contains(t, page, "Settings section. You are in "+titleWord(tc.level)+
				": it works, it just is not in your Settings menu.")
		} else {
			require.NotContains(t, page, "data-level-banner", "%s at %s", tc.section, tc.level)
		}
	}
}

// An unknown or repeated ?s= is a 400 naming the valid sections -- never a
// silent fallback to the default section.
func TestSettingsSections_UnknownSectionIsANamedBadRequest(t *testing.T) {
	_, mux := newConsoleLevel(t, config.Features{}, "advanced")
	for _, path := range []string{"/console/settings?s=runtime", "/console/settings?s=features&s=setup"} {
		rr := getPeek(t, mux, path)
		require.Equal(t, http.StatusBadRequest, rr.Code, path)
	}
	rr := getPeek(t, mux, "/console/settings?s=runtime")
	require.Contains(t, rr.Body.String(),
		"invalid s &#34;runtime&#34;: valid values are experience, features, setup, updates, briefing, workspaces, engine")

	req := httptest.NewRequest(http.MethodGet, "/console/settings?s=runtime&format=json", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr = do(mux, req)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), "valid values are experience, features, setup, updates, briefing, workspaces, engine")
}

// The JSON contract: the FULL payload at every section, plus the level fields.
// seam doctor and client_features read featuresConfig from it, unchanged.
func TestSettingsSections_JSONIsTheFullPayloadAtEverySection(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{Research: true}, "basic")
	_, err := store.EnsureProject(context.Background(), db, "seamless", "Seamless")
	require.NoError(t, err)

	want := rawJSON(t, mux, "/console/settings?format=json")
	for _, id := range settingsSectionIDs() {
		require.Equal(t, want, rawJSON(t, mux, "/console/settings?format=json&s="+id), "s=%s", id)
	}
	for _, key := range []string{"featuresConfig", "features", "featuresOverridden", "briefing", "embeddings",
		"database", "utilityRows", "projects", "workspaces", "familyEditors"} {
		require.Contains(t, want, key)
	}

	var data settingsData
	getJSON(t, mux, "/console/settings?format=json", &data)
	require.Equal(t, "basic", data.ConsoleLevel)
	require.False(t, data.ConsoleLevelOverridden)
	require.Empty(t, data.ConsoleLevelSource)
	setLevel(t, db, "standard")
	getJSON(t, mux, "/console/settings?format=json", &data)
	require.Equal(t, "standard", data.ConsoleLevel)
	require.True(t, data.ConsoleLevelOverridden)
	require.Equal(t, "chosen", data.ConsoleLevelSource)
}

// Every Settings POST lands back on the section it belongs to.
func TestSettingsSections_EveryPostReturnsToItsSection(t *testing.T) {
	db, mux := newConsoleLevel(t, config.Features{}, "advanced")
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, db, "app", "app")
	require.NoError(t, err)
	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{}))
	require.NoError(t, store.SetBriefingConfig(ctx, db, config.Defaults().Briefing))

	for _, tc := range []struct {
		path, body, want string
	}{
		{"/console/settings/features", "", "/console/settings?s=features&notice="},
		{"/console/settings/features", "feature_bogus=1", "/console/settings?s=features&error="},
		{"/console/settings/features/reset", "", "/console/settings?s=features&notice="},
		{"/console/settings/briefing", "findings_count=2", "/console/settings?s=briefing&notice="},
		{"/console/settings/briefing", "findings_count=-1", "/console/settings?s=briefing&error="},
		{"/console/settings/briefing/reset", "", "/console/settings?s=briefing&notice="},
		{"/console/settings/utility", "project=app&force=on", "/console/settings?s=engine&notice="},
		{"/console/settings/utility", "project=app&force=sideways", "/console/settings?s=engine&error="},
		{"/console/settings/embeddings/mode", "mode=off", "/console/settings?s=engine&notice="},
		{"/console/settings/embeddings/mode", "mode=sideways", "/console/settings?s=engine&error="},
		{"/console/settings/embeddings/reembed", "", "/console/settings?s=engine&error="},
		{"/console/settings/families/save", "name=fam&members=app", "/console/settings?s=workspaces&notice="},
		{"/console/settings/families/save", "name=fam&members=nope", "/console/settings?s=workspaces&error="},
		{"/console/settings/families/delete", "original_name=fam", "/console/settings?s=workspaces&notice="},
		// No update check runs in this console: each Updates POST says so.
		{"/console/settings/updates", "check=off", "/console/settings?s=updates&error="},
		{"/console/settings/updates/reset", "", "/console/settings?s=updates&error="},
		{"/console/settings/updates/check", "", "/console/settings?s=updates&error="},
		{"/console/settings/updates/auto", "auto=off", "/console/settings?s=updates&error="},
		{"/console/settings/updates/apply", "", "/console/settings?s=updates&error="},
		{"/console/settings/updates/resume", "", "/console/settings?s=updates&error="},
	} {
		rr := postForm(mux, tc.path, tc.body)
		require.Equal(t, http.StatusSeeOther, rr.Code, "%s %s", tc.path, tc.body)
		require.True(t, strings.HasPrefix(rr.Header().Get("Location"), tc.want),
			"%s %s -> %s, want prefix %s", tc.path, tc.body, rr.Header().Get("Location"), tc.want)
	}
}

// The save bar replaces the footer-only buttons: it appears once a form is
// dirty (the snapshot tracker toggles .is-dirty), Discard puts the form back to
// its server-rendered baseline, and without script the bar simply stays. Old
// #anchor bookmarks map to their section in place.
func TestSettingsSections_SaveBarAndLegacyAnchors(t *testing.T) {
	source, err := templateFS.ReadFile("templates/settings.html")
	require.NoError(t, err)
	page := string(source)
	require.Equal(t, 2, strings.Count(page, `class="brief-actions settings-savebar"`), "one bar per editable form")
	require.Equal(t, 2, strings.Count(page, `type="button" data-form-discard`))
	require.Contains(t, page, "form.reset();")
	require.NotContains(t, page, "data-filters-toggle", "no Filters toggle on a page with no filters")
	require.NotContains(t, page, "settings-mode", "the briefing-source chip gave way to per-section precedence lines")
	require.Contains(t, page, `'#features': 'features'`)
	require.Contains(t, page, `'#briefing-recipe': 'briefing'`)
	require.Contains(t, page, `'#workspace-registry': 'workspaces'`)
	require.Contains(t, page, "window.SeamConsole.load('/console/settings?s='")

	css := string(consoleCSS)
	require.Contains(t, css, ".js form:not(.is-dirty) > .settings-savebar { display: none; }")
	require.Contains(t, css, ".settings-subnav")
	require.Contains(t, css, "@media (max-width: 720px)")
}

// Briefing presets: one choice for Standard, every knob a disclosure away. The
// server marks the preset the effective values equal (else Custom), and the
// Customize form is open by default only at Advanced.
func TestBriefingSection_PresetsAndCustomize(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc, err := New(Config{DB: db, APIKey: testKey, BriefingCfg: config.Defaults().Briefing, Level: "standard"})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	ctx := context.Background()

	checked := regexp.MustCompile(`name="briefing-preset" value="([a-z]+)" form="briefing-preset-picker" data-preset-values="[^"]*" checked`)
	selected := func(page string) []string {
		var out []string
		for _, m := range checked.FindAllStringSubmatch(page, -1) {
			out = append(out, m[1])
		}
		return out
	}

	page := getPeek(t, mux, "/console/settings?s=briefing").Body.String()
	require.Equal(t, []string{"balanced"}, selected(page), "the defaults are the Balanced preset")
	for _, key := range []string{"lean", "balanced", "rich"} {
		require.Contains(t, page, `value="`+key+`" form="briefing-preset-picker"`)
	}
	require.Contains(t, page, `<p class="brief-preset-custom" data-preset-custom hidden>`)
	require.Contains(t, page, `<details class="brief-customize">`, "closed at Standard")
	require.Contains(t, page, "Protected context stays protected.")
	require.Contains(t, page, "2 full constraints · 12 memory lines · 2 recent findings", "Lean's numbers are generated")
	require.Contains(t, page, "6 full constraints · memory lines up to the budget · 5 recent findings · family memories")
	for _, field := range []string{"constraint_max_full", "convention_max_full", "memory_max_age_days", "memory_max_items",
		"findings_count", "findings_max_age_days", "ready_tasks_shown", "pending_plan_max_days",
		"stage_unknown_max_age_days", "hard_cap_multiplier", "sibling_findings_count", "include_parent_memories",
		"include_sibling_memories", "utility_weight", "utility_mode"} {
		require.Contains(t, page, `name="`+field+`"`, "the form still carries every knob")
		require.Contains(t, page, `&#34;`+field+`&#34;:`, "each preset carries every knob the form submits")
	}

	// Saving Lean's values through the one route marks Lean.
	lean := config.BriefingPresets()[0].Briefing
	form := url.Values{}
	for name, v := range briefingFormValues(lean) {
		switch x := v.(type) {
		case bool:
			if x {
				form.Set(name, "1")
			}
		default:
			form.Set(name, fmt.Sprint(x))
		}
	}
	rr := postForm(mux, "/console/settings/briefing", form.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	eff, _, err := store.BriefingConfig(ctx, db, config.Defaults().Briefing)
	require.NoError(t, err)
	require.Equal(t, lean, eff, "the form round-trips the preset exactly")
	page = getPeek(t, mux, "/console/settings?s=briefing").Body.String()
	require.Equal(t, []string{"lean"}, selected(page))

	// One knob off is Custom: no preset marked, the Custom note shown.
	lean.FindingsCount = 9
	require.NoError(t, store.SetBriefingConfig(ctx, db, lean))
	page = getPeek(t, mux, "/console/settings?s=briefing").Body.String()
	require.Empty(t, selected(page))
	require.Contains(t, page, `<p class="brief-preset-custom" data-preset-custom>`)

	// At Advanced the full form is open by default.
	require.NoError(t, store.SetConsoleLevel(ctx, db, "advanced", store.ConsoleLevelChosen))
	page = getPeek(t, mux, "/console/settings?s=briefing").Body.String()
	require.Contains(t, page, `<details class="brief-customize" open>`)

	source, err := templateFS.ReadFile("templates/settings.html")
	require.NoError(t, err)
	require.Contains(t, string(source), "function syncPresets()", "an edit re-marks the matching preset")
	require.Contains(t, string(source), `<form id="briefing-preset-picker" hidden></form>`,
		"the radios never submit with the briefing form")
}

func TestBriefingPresetCards_MatchTheConfigTable(t *testing.T) {
	cards := briefingPresetCards(config.Defaults().Briefing)
	require.Len(t, cards, len(config.BriefingPresets()))
	for i, p := range config.BriefingPresets() {
		require.Equal(t, p.Key, cards[i].Key)
		var values map[string]any
		require.NoError(t, json.Unmarshal([]byte(cards[i].Values), &values))
		require.Len(t, values, 15, "every knob the form submits")
	}
	require.True(t, cards[1].Selected)
	require.False(t, cards[0].Selected)
	require.False(t, cards[2].Selected)
}
