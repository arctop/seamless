package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// repoMapping is one (host, path) -> project route, read from the repo_map
// table rather than the legacy flat JSON mirror: once several machines share one
// daemon, two of them can mount the same path on different repositories, so a
// route without its host names nothing.
type repoMapping struct {
	// Host is the machine the path lives on. Empty is the LEGACY bucket -- rows
	// written before host scoping, or by a seeder that never named its machine
	// -- and renders as "unknown", never as "this machine".
	Host    string `json:"host,omitempty"`
	Repo    string `json:"repo"`
	Project string `json:"project"`
}

// repoRoute is one mapped repo path under a workspace scope, with the machine
// it lives on.
type repoRoute struct {
	Host string `json:"host,omitempty"`
	Path string `json:"path"`
}

type familyGroup struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

type familyScopeRef struct {
	Slug       string `json:"slug"`
	Registered bool   `json:"registered"`
}

type workspaceFamily struct {
	Name        string `json:"name"`
	MemberCount int    `json:"memberCount"`
}

type familyProjectOption struct {
	Slug       string `json:"slug"`
	Name       string `json:"name,omitempty"`
	Registered bool   `json:"registered"`
	Retired    bool   `json:"retired"`
	Selected   bool   `json:"selected"`
}

type familyEditor struct {
	Name    string                `json:"name"`
	Members []familyScopeRef      `json:"members"`
	Options []familyProjectOption `json:"options"`
}

// workspaceScope joins the three ways Settings describes a scope: its project
// row, the repo paths that resolve to it, and the families that share context
// with it. Registered is false for slugs referenced only by routing, lineage,
// or family metadata, so the console can expose those dangling references
// instead of silently dropping them from the unified directory.
type workspaceScope struct {
	Slug             string            `json:"slug"`
	Name             string            `json:"name,omitempty"`
	Description      string            `json:"description,omitempty"`
	ParentSlug       string            `json:"parentSlug,omitempty"`
	Registered       bool              `json:"registered"`
	Retired          bool              `json:"retired"`
	ParentRegistered bool              `json:"parentRegistered"`
	Repos            []repoRoute       `json:"repos"`
	Families         []workspaceFamily `json:"families"`
}

// utilityProjectRow is one project's utility-activation status for the
// Settings table: where its demand history stands against the readiness
// thresholds, whether the gardener latch has tripped, and any owner force.
type utilityProjectRow struct {
	Project        string     `json:"project"` // "" = global scope
	Status         string     `json:"status"`  // active|armed|building|forced-off
	Forced         string     `json:"forced,omitempty"`
	ReadyAt        *time.Time `json:"readyAt,omitempty"`
	RecentEvents   int        `json:"recentEvents"`
	RecentMemories int        `json:"recentMemories"`
	AgeDays        int        `json:"ageDays"` // days since the first demand event
	EventsOK       bool       `json:"eventsOK"`
	MemoriesOK     bool       `json:"memoriesOK"`
	AgeOK          bool       `json:"ageOK"`
	// Remaining spells out what still separates this scope from arming
	// ("needs 7 more events, 3d more history") so the table answers "when
	// does auto kick in" without mental math against the threshold prose.
	Remaining string `json:"remaining,omitempty"`
}

// EmbeddingRuntime describes the embedder the daemon resolved at serve start.
// Enabled means vectors are being written and searched this process. When
// disabled, Reason says why in the owner's terms: their off switch, a missing
// credential, or a config mistake -- three states that call for three different
// next actions.
type EmbeddingRuntime struct {
	Enabled bool `json:"enabled"`
	// Provider is the configured llm.provider, shown even when disabled.
	Provider string `json:"provider"`
	// Model is the active embedding model when enabled, else the configured one.
	Model string `json:"model"`
	// Reason is the human-readable cause when disabled; empty when enabled.
	Reason string `json:"reason,omitempty"`
	// Misconfigured marks Reason as a local config error (llm.ErrConfig class)
	// rather than a deliberate opt-out, so the panel can escalate its tone.
	Misconfigured bool `json:"misconfigured,omitempty"`
	// OverriddenOff means this process started with the console off switch set.
	OverriddenOff bool `json:"overriddenOff,omitempty"`
}

// embeddingModelRow is one (model, dims) group of stored vectors, flagged
// stale when it is not the model the running embedder writes -- those vectors
// are invisible to semantic search until re-embedded.
type embeddingModelRow struct {
	Model    string    `json:"model"`
	Dims     int       `json:"dims"`
	Count    int       `json:"count"`
	Memories int       `json:"memories"`
	Notes    int       `json:"notes"`
	Updated  time.Time `json:"updated"`
	Stale    bool      `json:"stale"`
}

// embeddingsPanel is the Settings page's semantic-index section: the runtime
// embedder state, the stored corpus grouped by model, the owner's override as
// currently stored, and the re-embed pass state.
type embeddingsPanel struct {
	EmbeddingRuntime
	// ModeOff is the stored override at render time; RestartNeeded is true when
	// it disagrees with what this process resolved at startup.
	ModeOff       bool                  `json:"modeOff"`
	RestartNeeded bool                  `json:"restartNeeded"`
	Total         int                   `json:"total"`
	Missing       int                   `json:"missing"`
	Stale         int                   `json:"stale"`
	Models        []embeddingModelRow   `json:"models"`
	Reembed       files.ReembedProgress `json:"reembed"`
}

// databasePanel is the Settings page's SQLite block: where the database lives,
// how big it is on disk (main file plus WAL), and its schema version.
type databasePanel struct {
	Path          string `json:"path"`
	SizeBytes     int64  `json:"sizeBytes"`
	WalBytes      int64  `json:"walBytes"`
	SizeHuman     string `json:"sizeHuman"` // main + WAL, formatted
	SchemaVersion int    `json:"schemaVersion"`
}

// settingsData is the payload for the Settings page. Briefing carries the
// effective briefing knobs (file/env base + the console override row), while
// FamilyEditors supplies the other intentionally editable control surface.
// Runtime configuration and project routing remain read-only here.
type settingsData struct {
	DataDir            string          `json:"dataDir"`
	Budgets            config.Budgets  `json:"budgets"`
	Gardener           config.Gardener `json:"gardener"`
	Briefing           config.Briefing `json:"briefing"`
	BriefingOverridden bool            `json:"briefingOverridden"`
	// Features is the effective optional-feature state, one card per registry
	// entry in registry order. FeaturesConfig is the same state as the raw
	// config struct, and FeaturesOverridden reports whether a stored override
	// row is in force (the console save or the grandfather migration wrote it).
	// This trio is the contract `seam doctor` reads to compute how many MCP
	// tools a daemon should be exposing.
	Features           []featureCard         `json:"features"`
	FeaturesConfig     config.Features       `json:"featuresConfig"`
	FeaturesOverridden bool                  `json:"featuresOverridden"`
	Embeddings         embeddingsPanel       `json:"embeddings"`
	Database           databasePanel         `json:"database"`
	UtilityRows        []utilityProjectRow   `json:"utilityRows"`
	UtilityReady       [3]int                `json:"utilityReady"` // thresholds: events, memories, age days
	Projects           []core.Project        `json:"projects"`
	RepoMap            []repoMapping         `json:"repoMap"`
	Families           []familyGroup         `json:"families"`
	Workspaces         []workspaceScope      `json:"workspaces"`
	UnboundRepos       []repoRoute           `json:"unboundRepos"`
	FamilyEditors      []familyEditor        `json:"familyEditors"`
	FamilyOptions      []familyProjectOption `json:"familyOptions"`
	// ConsoleLevel is the level in force (presentation only), with whether a
	// stored row sets it and who wrote that row ("chosen" by the owner or
	// "seeded" by the upgrade). They are the ONLY fields that differ by level:
	// everything above is the full payload at every level and every section.
	ConsoleLevel           string `json:"consoleLevel"`
	ConsoleLevelOverridden bool   `json:"consoleLevelOverridden"`
	ConsoleLevelSource     string `json:"consoleLevelSource,omitempty"`

	// Section is the ?s= section the page shows and Subnav its menu; page
	// state only, so JSON callers get the same payload whatever s says.
	Section string             `json:"-"`
	Subnav  []settingsNavEntry `json:"-"`
	// LevelCards are the Experience section's three levels, the one in force
	// marked, each with its generated list of what it shows.
	LevelCards []levelCard `json:"-"`
	// BriefingPresets are the Briefing section's one-choice recipes, the one
	// the effective values equal marked; none marked means Custom.
	BriefingPresets []briefingPresetCard `json:"-"`
	// CustomizeOpen opens the Briefing section's full form by default: at
	// Advanced every knob is one glance away, below it the presets lead.
	CustomizeOpen bool `json:"-"`
	// Setup is the Your setup section's plain-language facts.
	Setup setupPanel `json:"-"`
	// Updates is the Updates section's view of the background update check,
	// filled only for that section. Page state like Setup: the JSON payload
	// stays the same at every section.
	Updates updatesPanel `json:"-"`
	// PreviewProjects are the projects the Briefing section's preview offers
	// (registered and live, in slug order), and PreviewProject the one it
	// opens on: where the owner's agents worked last, else the first. Filled
	// only for the Briefing section.
	PreviewProjects []string `json:"-"`
	PreviewProject  string   `json:"-"`
}

// briefingPresetCard is one preset as the Briefing section offers it.
type briefingPresetCard struct {
	Key     string
	Label   string
	Intent  string
	Summary string
	// Values is the preset as form field -> value JSON: the page fills the
	// form from it, and compares the form against it to mark the match live.
	Values string
	// Selected marks the preset the effective values equal.
	Selected bool
}

// briefingPresetCards builds the preset cards, marking the one current equals.
func briefingPresetCards(current config.Briefing) []briefingPresetCard {
	match, matched := config.MatchBriefingPreset(current)
	presets := config.BriefingPresets()
	out := make([]briefingPresetCard, 0, len(presets))
	for _, p := range presets {
		values, err := json.Marshal(briefingFormValues(p.Briefing))
		if err != nil {
			// A map of numbers, bools, and strings always encodes; keep the
			// card rather than fail the page if that ever stops being true.
			values = []byte("{}")
		}
		out = append(out, briefingPresetCard{
			Key: p.Key, Label: p.Label, Intent: p.Intent, Summary: briefingPresetSummary(p.Briefing),
			Values: string(values), Selected: matched && match.Key == p.Key,
		})
	}
	return out
}

// briefingFormValues maps a briefing onto the Briefing form's field names --
// the same names settingsBriefingSave reads, so a preset fills exactly the
// fields the save submits.
func briefingFormValues(b config.Briefing) map[string]any {
	mode := b.UtilityMode
	if mode == "" {
		mode = "auto"
	}
	return map[string]any{
		"constraint_max_full":        b.ConstraintMaxFull,
		"convention_max_full":        b.ConventionMaxFull,
		"memory_max_age_days":        b.MemoryMaxAgeDays,
		"memory_max_items":           b.MemoryMaxItems,
		"findings_count":             b.FindingsCount,
		"findings_max_age_days":      b.FindingsMaxAgeDays,
		"ready_tasks_shown":          b.ReadyTasksShown,
		"pending_plan_max_days":      b.PendingPlanMaxDays,
		"stage_unknown_max_age_days": b.StageUnknownMaxAgeDays,
		"hard_cap_multiplier":        b.HardCapMultiplier,
		"sibling_findings_count":     b.SiblingFindingsCount,
		"include_parent_memories":    b.IncludeParentMemories,
		"include_sibling_memories":   b.IncludeSiblingMemories,
		"utility_weight":             b.UtilityWeight,
		"utility_mode":               mode,
	}
}

// briefingPresetSummary names the numbers that set a preset apart, generated
// from its values so the copy can never promise a number the preset lacks.
func briefingPresetSummary(b config.Briefing) string {
	parts := []string{plural(b.ConstraintMaxFull, "full constraint", "full constraints")}
	if b.MemoryMaxItems > 0 {
		parts = append(parts, plural(b.MemoryMaxItems, "memory line", "memory lines"))
	} else {
		parts = append(parts, "memory lines up to the budget")
	}
	parts = append(parts, plural(b.FindingsCount, "recent finding", "recent findings"))
	if b.IncludeSiblingMemories {
		parts = append(parts, "family memories")
	}
	return strings.Join(parts, " \u00b7 ")
}

// setupPanel is the Your setup section: what a non-technical owner can check
// and act on -- the version, where things live, which agents are connected --
// and none of the budgets or policy numbers (those stay in Knowledge engine).
type setupPanel struct {
	Version       string
	Host          string
	ConfigPath    string
	ConfigEditURL template.URL
	Clients       []setupClient
}

// setupClient is one agent client that has recorded sessions here.
type setupClient struct {
	Name     string
	LastSeen time.Time
}

// setupData builds the Your setup facts. The clients line is best-effort: a
// read failure costs that line, never the section.
func (s *Service) setupData(ctx context.Context) setupPanel {
	p := setupPanel{Version: strings.TrimSpace(s.cfg.Version), Host: s.host, ConfigPath: strings.TrimSpace(s.cfg.ConfigPath)}
	if p.ConfigPath != "" {
		_, p.ConfigEditURL = absAndEditURL("", p.ConfigPath)
	}
	facts, err := store.GetHealthFacts(ctx, s.cfg.DB)
	if err != nil {
		s.logger.Warn("console: setup clients", "error", err)
	}
	for _, c := range facts.Clients {
		_, _, name := agentDisplay(c.Client)
		p.Clients = append(p.Clients, setupClient{Name: name, LastSeen: c.LastSeen})
	}
	return p
}

func (s *Service) settings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	section, err := settingsSectionParam(r.URL.Query())
	if err != nil {
		s.badRequest(w, r, err.Error())
		return
	}

	projects, err := store.ListProjects(ctx, s.cfg.DB)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	repoRows, err := store.RepoMapRows(ctx, s.cfg.DB)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	families, err := store.ProjectFamilies(ctx, s.cfg.DB)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	workspaces, unboundRepos := buildWorkspaceRegistry(projects, repoRows, families)
	familyEditors, familyOptions := buildFamilyEditors(workspaces, families)
	briefing, overridden, err := store.BriefingConfig(ctx, s.cfg.DB, s.cfg.BriefingCfg)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	utilityRows, err := s.utilityActivationRows(ctx, projects)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	embeddings, err := s.embeddingsPanel(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// The features zone resolves the same way the gates do, and its data line
	// needs the RAW counts: a feature that is off still holds its rows, and
	// saying so is the point of the line. s.navCounts zeroes them by design.
	featuresCfg, featuresOverridden := s.featuresConfig(ctx)
	rawCounts, cerr := store.GetNavCounts(ctx, s.cfg.DB)
	if cerr != nil {
		// Best-effort, like the sidebar badges: a counts failure costs the
		// reassurance line, not the page.
		s.logger.Warn("console: settings feature counts", "error", cerr)
	}

	var previewProjects []string
	var previewProject string
	if section.ID == "briefing" && !wantsJSON(r) {
		previewProjects, previewProject = s.previewChoices(ctx, projects)
	}
	var updates updatesPanel
	if section.ID == "updates" && !wantsJSON(r) {
		updates = s.updatesPanelAt(time.Now())
	}

	lvl := s.consoleLevel(ctx)
	pd := pageData{
		Title:       "Settings \u00b7 " + section.Label,
		Active:      "settings",
		LevelBanner: sectionBanner(section, lvl.Level),
		Data: settingsData{
			DataDir:            s.cfg.DataDir,
			Embeddings:         embeddings,
			Database:           s.databasePanel(),
			Budgets:            s.cfg.Budgets,
			Gardener:           s.cfg.GardenerCfg,
			Briefing:           briefing,
			BriefingOverridden: overridden,
			Features:           featureCards(featuresCfg, navCounts{Labs: rawCounts.Labs, Trials: rawCounts.Trials}),
			FeaturesConfig:     featuresCfg,
			FeaturesOverridden: featuresOverridden,
			UtilityRows:        utilityRows,
			UtilityReady:       [3]int{store.UtilityReadyMinEvents, store.UtilityReadyMinMemories, store.UtilityReadyMinAgeDays},
			Projects:           projects,
			RepoMap:            sortedRepoMap(repoRows),
			Families:           sortedFamilies(families),
			Workspaces:         workspaces,
			UnboundRepos:       unboundRepos,
			FamilyEditors:      familyEditors,
			FamilyOptions:      familyOptions,

			ConsoleLevel:           lvl.Level.String(),
			ConsoleLevelOverridden: lvl.Overridden,
			ConsoleLevelSource:     lvl.Source,
			Section:                section.ID,
			Subnav:                 settingsSubnav(lvl.Level, section.ID),
			LevelCards:             levelCards(featuresCfg, lvl.Level),
			BriefingPresets:        briefingPresetCards(briefing),
			CustomizeOpen:          lvl.Level >= levelAdvanced,
			Setup:                  s.setupData(ctx),
			Updates:                updates,
			PreviewProjects:        previewProjects,
			PreviewProject:         previewProject,
		},
	}
	s.render(w, r, "settings", pd)
}

// previewChoices lists the live projects the briefing preview can be for and
// picks the one it opens on: the project of the most recent session, when it
// is still live, else the first in slug order. A failed recency read costs the
// smart default, never the page.
func (s *Service) previewChoices(ctx context.Context, projects []core.Project) ([]string, string) {
	live := liveProjectSlugs(projects)
	if len(live) == 0 {
		return nil, ""
	}
	latest, err := store.LatestSessionProject(ctx, s.cfg.DB)
	if err != nil {
		s.logger.Warn("console: briefing preview default project", "error", err)
	}
	if slices.Contains(live, latest) {
		return live, latest
	}
	return live, live[0]
}

// utilityActivationRows joins the activation state with each scope's demand
// progress so the Settings table can say WHY a project is or is not ranked by
// utility yet. Every registered project appears, plus the global scope and any
// slug that exists only in the demand or activation maps.
func (s *Service) utilityActivationRows(ctx context.Context, projects []core.Project) ([]utilityProjectRow, error) {
	now := time.Now().UTC()
	activation, err := store.GetUtilityActivation(ctx, s.cfg.DB)
	if err != nil {
		return nil, err
	}
	demand, err := store.UtilityDemandByProject(ctx, s.cfg.DB, now, store.UtilityReadyWindow)
	if err != nil {
		return nil, err
	}

	slugs := map[string]struct{}{"": {}}
	for _, p := range projects {
		if !p.Retired() {
			slugs[p.Slug] = struct{}{}
		}
	}
	for slug := range demand {
		slugs[slug] = struct{}{}
	}
	for slug := range activation.Projects {
		slugs[slug] = struct{}{}
	}

	rows := make([]utilityProjectRow, 0, len(slugs))
	for slug := range slugs {
		st := activation.Projects[slug]
		d := demand[slug]
		row := utilityProjectRow{
			Project: slug, Forced: st.Forced, ReadyAt: st.ReadyAt,
			RecentEvents: d.RecentEvents, RecentMemories: d.RecentMemories,
		}
		if !d.Earliest.IsZero() {
			row.AgeDays = int(now.Sub(d.Earliest).Hours() / 24)
		}
		row.EventsOK = d.RecentEvents >= store.UtilityReadyMinEvents
		row.MemoriesOK = d.RecentMemories >= store.UtilityReadyMinMemories
		row.AgeOK = !d.Earliest.IsZero() && now.Sub(d.Earliest) >= store.UtilityReadyMinAgeDays*24*time.Hour
		switch {
		case st.Forced == "off":
			row.Status = "forced-off"
		case st.Forced == "on" || st.ReadyAt != nil:
			row.Status = "active"
		case d.Ready(now):
			row.Status = "armed" // latches on the next gardener pass
			row.Remaining = "arms on the next gardener pass"
		default:
			row.Status = "building"
			row.Remaining = utilityRemaining(row, d)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if (rows[i].Status == "active") != (rows[j].Status == "active") {
			return rows[i].Status == "active"
		}
		if rows[i].RecentEvents != rows[j].RecentEvents {
			return rows[i].RecentEvents > rows[j].RecentEvents
		}
		return rows[i].Project < rows[j].Project
	})
	return rows, nil
}

// utilityRemaining phrases the unmet readiness gates for a building scope. A
// scope with no demand at all gets nothing -- the table already says "no
// demand yet" and enumerating every gate there would just restate the header.
func utilityRemaining(row utilityProjectRow, d store.UtilityProjectDemand) string {
	if d.Earliest.IsZero() {
		return ""
	}
	var needs []string
	if !row.EventsOK {
		needs = append(needs, fmt.Sprintf("%d more events", store.UtilityReadyMinEvents-d.RecentEvents))
	}
	if !row.MemoriesOK {
		needs = append(needs, fmt.Sprintf("%d more memories", store.UtilityReadyMinMemories-d.RecentMemories))
	}
	if !row.AgeOK {
		needs = append(needs, fmt.Sprintf("%dd more history", store.UtilityReadyMinAgeDays-row.AgeDays))
	}
	if len(needs) == 0 {
		return ""
	}
	return "needs " + strings.Join(needs, ", ")
}

// settingsUtilityForce sets or clears the owner's per-project force: "on" and
// "off" win over the gardener latch; "auto" clears the force and defers to it.
func (s *Service) settingsUtilityForce(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.PostFormValue("project"))
	if project != "" {
		if err := validate.Name(project); err != nil {
			settingsUtilityFlash(w, r, "project: "+err.Error())
			return
		}
	}
	force := r.PostFormValue("force")
	switch force {
	case "on", "off", "auto":
	default:
		settingsUtilityFlash(w, r, fmt.Sprintf("force invalid %q: valid values are on, off, auto", force))
		return
	}

	ctx := r.Context()
	activation, err := store.GetUtilityActivation(ctx, s.cfg.DB)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	st := activation.Projects[project]
	if force == "auto" {
		st.Forced = ""
	} else {
		st.Forced = force
	}
	activation.Projects[project] = st
	if err := store.SetUtilityActivation(ctx, s.writeDB(), activation); err != nil {
		s.serverError(w, r, err)
		return
	}
	scope := project
	if scope == "" {
		scope = "the global scope"
	}
	settingsUtilityNotice(w, r, fmt.Sprintf("Utility ranking for %s set to %s.", scope, force))
}

// settingsBriefingSave persists the briefing form as the runtime override row
// (store.SettingBriefingConfig). It never writes the config file; the override
// layers over file/env values and takes effect on the next session start, so no
// daemon restart is needed. Redirects back with a flash either way.
func (s *Service) settingsBriefingSave(w http.ResponseWriter, r *http.Request) {
	b, err := briefingFromForm(r)
	if err != nil {
		settingsBriefingFlash(w, r, err.Error())
		return
	}
	if err := store.SetBriefingConfig(r.Context(), s.writeDB(), b); err != nil {
		s.serverError(w, r, err)
		return
	}
	settingsBriefingNotice(w, r, "Briefing settings saved -- they apply from the next session start.")
}

// briefingIntFields are the briefing form's whole-number knobs, by form field.
var briefingIntFields = []struct {
	name string
	dst  func(*config.Briefing) *int
}{
	{"constraint_max_full", func(b *config.Briefing) *int { return &b.ConstraintMaxFull }},
	{"convention_max_full", func(b *config.Briefing) *int { return &b.ConventionMaxFull }},
	{"memory_max_age_days", func(b *config.Briefing) *int { return &b.MemoryMaxAgeDays }},
	{"memory_max_items", func(b *config.Briefing) *int { return &b.MemoryMaxItems }},
	{"findings_count", func(b *config.Briefing) *int { return &b.FindingsCount }},
	{"findings_max_age_days", func(b *config.Briefing) *int { return &b.FindingsMaxAgeDays }},
	{"ready_tasks_shown", func(b *config.Briefing) *int { return &b.ReadyTasksShown }},
	{"pending_plan_max_days", func(b *config.Briefing) *int { return &b.PendingPlanMaxDays }},
	{"stage_unknown_max_age_days", func(b *config.Briefing) *int { return &b.StageUnknownMaxAgeDays }},
	{"hard_cap_multiplier", func(b *config.Briefing) *int { return &b.HardCapMultiplier }},
	{"sibling_findings_count", func(b *config.Briefing) *int { return &b.SiblingFindingsCount }},
}

// briefingFromForm reads the briefing form into a validated config.Briefing:
// the ONE parse the save and the preview share, so a preview can never accept
// knobs a save would refuse. It rebuilds the WHOLE struct -- a checkbox left
// unchecked submits nothing, and an empty number reads as 0 -- because the
// override row replaces the struct, and a field left out would silently zero
// (constraint closed-loop-utility-signal-contract). The error is the owner-facing
// message the save flashes.
func briefingFromForm(r *http.Request) (config.Briefing, error) {
	b := config.Briefing{
		IncludeParentMemories:  r.PostFormValue("include_parent_memories") != "",
		IncludeSiblingMemories: r.PostFormValue("include_sibling_memories") != "",
		UtilityMode:            r.PostFormValue("utility_mode"),
	}
	weightStr := strings.TrimSpace(r.PostFormValue("utility_weight"))
	if weightStr == "" {
		weightStr = "0"
	}
	weight, err := strconv.ParseFloat(weightStr, 64)
	if err != nil {
		return config.Briefing{}, errors.New("utility_weight must be a number between 0 and 1")
	}
	b.UtilityWeight = weight
	for _, f := range briefingIntFields {
		v := strings.TrimSpace(r.PostFormValue(f.name))
		if v == "" {
			v = "0"
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return config.Briefing{}, errors.New(f.name + " must be a whole number")
		}
		*f.dst(&b) = n
	}
	if err := b.Validate(); err != nil {
		return config.Briefing{}, err
	}
	return b, nil
}

// eventFeaturesChanged records an optional-feature toggle in the event log, so
// a change of what agents can reach shows up in Activity with the same
// attribution as every other owner action.
const eventFeaturesChanged core.EventKind = "settings.features_changed"

// eventLevelChanged records a console experience-level change in the event log
// (payload {from, to, by: "console"}, plus reset: true when the owner went back
// to the file/env level), so the change shows in Activity like a features
// toggle. The level is presentation only: nothing an agent receives changes,
// which is why this is a settings event and not a features one.
const eventLevelChanged core.EventKind = "settings.level_changed"

// settingsFeaturesSave persists the optional-features form as the stored
// override row. Like the briefing form it rebuilds the WHOLE struct from
// checkbox presence -- an unchecked box submits nothing, so reading only the
// fields that arrived would make "turn it off" indistinguishable from "leave it
// alone". It never rewrites the config file, and it applies immediately in the
// console (agents pick the change up when they next list tools).
func (s *Service) settingsFeaturesSave(w http.ResponseWriter, r *http.Request) {
	// Boundary check before anything is stored: a field that is present but not
	// the checkbox's own value, or a feature_* field naming nothing in the
	// registry, is a caller mistake and must not quietly become "off".
	known := make(map[string]bool, len(features.Registry()))
	for _, f := range features.Registry() {
		known[featureField(f.Key)] = true
	}
	for name, values := range r.PostForm {
		if !strings.HasPrefix(name, featureFieldPrefix) {
			continue
		}
		if !known[name] {
			settingsFeaturesFlash(w, r, fmt.Sprintf("unknown feature field %q", name))
			return
		}
		if len(values) != 1 || values[0] != "1" {
			settingsFeaturesFlash(w, r, fmt.Sprintf("%s must be omitted (off) or set exactly once to 1 (on)", name))
			return
		}
	}

	var cfg config.Features
	changed := make(map[string]any, len(features.Registry()))
	var on []string
	for _, f := range features.Registry() {
		enabled := r.PostFormValue(featureField(f.Key)) != ""
		f.Set(&cfg, enabled)
		changed[string(f.Key)] = enabled
		if enabled {
			on = append(on, f.Label)
		}
	}
	if err := store.SetFeaturesConfig(r.Context(), s.writeDB(), cfg); err != nil {
		s.serverError(w, r, err)
		return
	}
	if s.cfg.Events != nil {
		if _, err := s.cfg.Events.Record(r.Context(), core.Event{
			Kind:    eventFeaturesChanged,
			Payload: map[string]any{"features": changed, "by": "console"},
		}); err != nil {
			s.logger.Warn("console: record features event", "error", err)
		}
	}
	msg := "Optional features saved -- all of them are off."
	if len(on) > 0 {
		msg = "Optional features saved -- " + strings.Join(on, ", ") +
			" on. Agents pick the change up on their next session."
	}
	settingsFeaturesNotice(w, r, msg)
}

// settingsLevelSave stores the owner's console level -- presentation only, so
// nothing an agent receives changes. The boundary is strict: a missing level is
// a malformed request (400), a level outside config.ConsoleLevels flashes an
// error naming them, and neither ever falls back to a default. Choosing a level
// also answers the Home welcome card. The redirect follows
// redirectAfterLevelChange: back where the owner was when the new level shows
// it, Home (or Experience) with a note when it does not.
func (s *Service) settingsLevelSave(w http.ResponseWriter, r *http.Request) {
	values, present := r.PostForm["level"]
	if !present {
		s.formError(w, r, http.StatusBadRequest,
			"level is required: valid values are "+strings.Join(config.ConsoleLevels, ", "))
		return
	}
	if len(values) != 1 {
		settingsLevelFlash(w, r, "level must be given exactly once")
		return
	}
	next, err := parseLevel(values[0])
	if err != nil {
		settingsLevelFlash(w, r, err.Error())
		return
	}
	ctx := r.Context()
	prev := s.consoleLevel(ctx).Level
	if err := store.SetConsoleLevel(ctx, s.writeDB(), next.String(), store.ConsoleLevelChosen); err != nil {
		s.serverError(w, r, err)
		return
	}
	back := levelReturn(r)
	if next == prev {
		// The row now records the choice (and the welcome), but nothing on
		// screen changed, so there is no change to log.
		http.Redirect(w, r, withNotice(back, "Staying on "+next.Label()+"."), http.StatusSeeOther)
		return
	}
	s.recordLevelChange(ctx, prev, next, false)
	http.Redirect(w, r, redirectAfterLevelChange(screenRegistry(), next, back.RequestURI()), http.StatusSeeOther)
}

// settingsLevelReset clears the stored level so the file/env level applies
// again. The welcome is kept -- a reset is not a request to be asked again --
// and a reset with no stored level is a no-op that says so.
func (s *Service) settingsLevelReset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	before := s.consoleLevel(ctx)
	if !before.Overridden {
		http.Redirect(w, r, withNotice(levelReturn(r),
			"Already following the file and environment ("+before.Level.Label()+")."), http.StatusSeeOther)
		return
	}
	if err := store.ClearConsoleLevel(ctx, s.writeDB()); err != nil {
		s.serverError(w, r, err)
		return
	}
	after := s.baseLevel
	s.recordLevelChange(ctx, before.Level, after, true)
	http.Redirect(w, r, redirectAfterLevelChange(screenRegistry(), after, levelReturn(r).RequestURI()), http.StatusSeeOther)
}

// settingsLevelWelcome dismisses the Home welcome card without changing the
// level. It lands back where it was posted from, with nothing to announce.
func (s *Service) settingsLevelWelcome(w http.ResponseWriter, r *http.Request) {
	if err := store.MarkConsoleWelcomed(r.Context(), s.writeDB()); err != nil {
		s.serverError(w, r, err)
		return
	}
	target := levelReturn(r)
	q := target.Query()
	q.Del("notice")
	q.Del("error")
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.RequestURI(), http.StatusSeeOther)
}

// levelReturn is the level forms' optional return path, through the login
// open-redirect guard; absent means the Experience section.
func levelReturn(r *http.Request) *url.URL {
	raw := strings.TrimSpace(r.PostFormValue("return"))
	if raw == "" {
		raw = "/console/settings?s=" + defaultSettingsSection
	}
	u, err := url.Parse(safeNext(raw))
	if err != nil {
		return &url.URL{Path: "/console/"}
	}
	return u
}

// recordLevelChange logs a level change in the event log so it shows in
// Activity like a features toggle. Best-effort: the change already happened.
func (s *Service) recordLevelChange(ctx context.Context, from, to level, reset bool) {
	if s.cfg.Events == nil {
		return
	}
	payload := map[string]any{"from": from.String(), "to": to.String(), "by": "console"}
	if reset {
		payload["reset"] = true
	}
	if _, err := s.cfg.Events.Record(ctx, core.Event{Kind: eventLevelChanged, Payload: payload}); err != nil {
		s.logger.Warn("console: record level event", "error", err)
	}
}

// settingsFeaturesReset clears the stored override row, so the effective state
// falls back to the file/env configuration -- which, unless the owner set the
// keys there, means every optional feature is off again. Nothing is deleted:
// the feature's data is untouched either way.
func (s *Service) settingsFeaturesReset(w http.ResponseWriter, r *http.Request) {
	if err := store.ClearFeaturesConfig(r.Context(), s.writeDB()); err != nil {
		s.serverError(w, r, err)
		return
	}
	if s.cfg.Events != nil {
		if _, err := s.cfg.Events.Record(r.Context(), core.Event{
			Kind:    eventFeaturesChanged,
			Payload: map[string]any{"reset": true, "by": "console"},
		}); err != nil {
			s.logger.Warn("console: record features event", "error", err)
		}
	}
	settingsFeaturesNotice(w, r, "Feature override cleared -- back to the file/env configuration.")
}

// settingsBriefingReset clears the runtime override row, reverting the
// effective briefing knobs to the file/env configuration.
func (s *Service) settingsBriefingReset(w http.ResponseWriter, r *http.Request) {
	if err := store.ClearBriefingConfig(r.Context(), s.writeDB()); err != nil {
		s.serverError(w, r, err)
		return
	}
	settingsBriefingNotice(w, r, "Briefing overrides cleared -- back to the file/env configuration.")
}

// embeddingsPanel assembles the semantic-index section: the startup runtime
// state from Config, the stored override and vector stats read live, and the
// re-embed pass state from the files manager (when wired).
func (s *Service) embeddingsPanel(ctx context.Context) (embeddingsPanel, error) {
	mode, err := store.EmbedderMode(ctx, s.cfg.DB)
	if err != nil {
		return embeddingsPanel{}, err
	}
	stats, err := store.GetEmbeddingStats(ctx, s.cfg.DB)
	if err != nil {
		return embeddingsPanel{}, err
	}

	p := embeddingsPanel{
		EmbeddingRuntime: s.cfg.Embedding,
		ModeOff:          mode == store.EmbedderModeOff,
		Total:            stats.Total,
		Missing:          stats.Missing,
	}
	// The override applies at serve start, so the page owes the owner a restart
	// note exactly when the stored switch disagrees with what this process did:
	// running but switched off, or held off by an override that is now cleared.
	p.RestartNeeded = (p.Enabled && p.ModeOff) || (p.OverriddenOff && !p.ModeOff)
	for _, m := range stats.Models {
		row := embeddingModelRow{
			Model: m.Model, Dims: m.Dims, Count: m.Count,
			Memories: m.Memories, Notes: m.Notes, Updated: m.Updated,
			Stale: p.Enabled && m.Model != p.Model,
		}
		if row.Stale {
			p.Stale += m.Count
		}
		p.Models = append(p.Models, row)
	}
	if s.cfg.Files != nil {
		p.Reembed = s.cfg.Files.ReembedStatus()
	}
	return p, nil
}

// databasePanel reads the SQLite block: schema version from the live
// connection, sizes from disk. Best-effort on the filesystem side -- a missing
// WAL (checkpointed) or an unreadable stat renders as zero, not an error.
func (s *Service) databasePanel() databasePanel {
	p := databasePanel{Path: s.cfg.DBPath}
	if v, err := store.SchemaVersion(s.cfg.DB); err == nil {
		p.SchemaVersion = v
	}
	if p.Path == "" {
		return p
	}
	if info, err := os.Stat(p.Path); err == nil {
		p.SizeBytes = info.Size()
	}
	if info, err := os.Stat(p.Path + "-wal"); err == nil {
		p.WalBytes = info.Size()
	}
	p.SizeHuman = humanBytes(p.SizeBytes + p.WalBytes)
	return p
}

// humanBytes formats a byte count for display (binary units, one decimal).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// settingsEmbeddingsMode stores or clears the embedder off switch. The switch
// is read once at serve start, so the flash says when a restart is what makes
// the change real.
func (s *Service) settingsEmbeddingsMode(w http.ResponseWriter, r *http.Request) {
	mode := r.PostFormValue("mode")
	switch mode {
	case store.EmbedderModeAuto, store.EmbedderModeOff:
	default:
		settingsEmbeddingsFlash(w, r, fmt.Sprintf("mode invalid %q: valid values are %s, %s",
			mode, store.EmbedderModeAuto, store.EmbedderModeOff))
		return
	}
	if err := store.SetEmbedderMode(r.Context(), s.writeDB(), mode); err != nil {
		s.serverError(w, r, err)
		return
	}
	switch {
	case mode == store.EmbedderModeOff && s.cfg.Embedding.Enabled:
		settingsEmbeddingsNotice(w, r, "Embeddings switched off -- restart the daemon to apply.")
	case mode == store.EmbedderModeOff:
		settingsEmbeddingsNotice(w, r, "Embeddings switched off.")
	case s.cfg.Embedding.OverriddenOff:
		settingsEmbeddingsNotice(w, r, "Embeddings switch cleared -- restart the daemon to re-enable them.")
	default:
		settingsEmbeddingsNotice(w, r, "Embeddings switch cleared -- back to the configured provider.")
	}
}

// settingsEmbeddingsReembed starts the background re-embed pass. The redirect
// returns immediately; progress renders on the page and the pass survives the
// request (the files manager owns its lifecycle).
func (s *Service) settingsEmbeddingsReembed(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Files == nil {
		settingsEmbeddingsFlash(w, r, "the files subsystem is not available")
		return
	}
	total, err := s.cfg.Files.StartReembed(r.Context())
	switch {
	case errors.Is(err, files.ErrNoEmbedder):
		settingsEmbeddingsFlash(w, r, "embeddings are disabled -- configure a provider (and restart) before re-embedding")
		return
	case errors.Is(err, files.ErrReembedRunning):
		settingsEmbeddingsFlash(w, r, "a re-embed pass is already running")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	settingsEmbeddingsNotice(w, r, fmt.Sprintf("Re-embedding %d items in the background with %s.",
		total, s.cfg.Embedding.Model))
}

// settingsFamilySave creates a family or replaces one family's name and member
// set. The picker submits a closed list of known project slugs; accepting an
// arbitrary slug here would let a typo create a plausible but inert family
// member that never contributes context.
func (s *Service) settingsFamilySave(w http.ResponseWriter, r *http.Request) {
	names, ok := r.PostForm["name"]
	if !ok || len(names) != 1 {
		settingsRegistryFlash(w, r, "Enter one family name.")
		return
	}
	name := strings.TrimSpace(names[0])
	if err := validate.Name(name); err != nil {
		settingsRegistryFlash(w, r, "Family name: "+err.Error())
		return
	}

	previousName := ""
	if previous, present := r.PostForm["original_name"]; present {
		if len(previous) != 1 || strings.TrimSpace(previous[0]) == "" {
			settingsRegistryFlash(w, r, "The family being edited is missing.")
			return
		}
		previousName = strings.TrimSpace(previous[0])
		if err := validate.Name(previousName); err != nil {
			settingsRegistryFlash(w, r, "Original family name: "+err.Error())
			return
		}
	}

	members := uniqueNonEmpty(r.PostForm["members"])
	if len(members) == 0 {
		settingsRegistryFlash(w, r, "Choose at least one project for the family.")
		return
	}
	projects, err := store.ListProjects(r.Context(), s.cfg.DB)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	families, err := store.ProjectFamilies(r.Context(), s.cfg.DB)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	allowed := make(map[string]bool, len(projects)+len(members))
	for _, project := range projects {
		allowed[project.Slug] = true
	}
	if previousName != "" {
		currentMembers, exists := families[previousName]
		if !exists {
			settingsRegistryFlash(w, r, fmt.Sprintf("Family %q no longer exists.", previousName))
			return
		}
		for _, member := range currentMembers {
			allowed[member] = true
		}
	}
	for _, member := range members {
		if err := validate.Name(member); err != nil {
			settingsRegistryFlash(w, r, "Project slug: "+err.Error())
			return
		}
		if !allowed[member] {
			settingsRegistryFlash(w, r, fmt.Sprintf("Unknown project scope %q.", member))
			return
		}
	}

	// Isolation requires a standalone project, so a fenced slug may not join a
	// family. The guard lives here rather than inside SaveProjectFamily on
	// purpose: gardener.Apply calls AddFamilyMembers during a split, and a new
	// error out of the store would land in serverError's 500 branch instead of
	// this flash. The owner's other route into a family -- tightening a project
	// that is already in one -- is handled by TightenProjectIsolation, which
	// detaches it as part of the tighten.
	fenced, err := store.IsolatedSlugs(r.Context(), s.cfg.DB, members)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(fenced) > 0 {
		labels := make([]string, len(fenced))
		for i, f := range fenced {
			labels[i] = fmt.Sprintf("%s (%s)", f.Slug, f.State)
		}
		settingsRegistryFlash(w, r, fmt.Sprintf(
			"Isolated projects cannot join a family: %s. Set them back to open first.",
			strings.Join(labels, ", ")))
		return
	}

	_, err = store.SaveProjectFamily(r.Context(), s.writeDB(), previousName, name, members)
	switch {
	case errors.Is(err, store.ErrFamilyExists):
		settingsRegistryFlash(w, r, fmt.Sprintf("A family named %q already exists.", name))
		return
	case errors.Is(err, store.ErrFamilyNotFound):
		settingsRegistryFlash(w, r, fmt.Sprintf("Family %q no longer exists.", previousName))
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	verb := "created"
	if previousName != "" {
		verb = "updated"
	}
	settingsRegistryNotice(w, r, fmt.Sprintf("Family %q %s.", name, verb))
}

func (s *Service) settingsFamilyDelete(w http.ResponseWriter, r *http.Request) {
	names, ok := r.PostForm["original_name"]
	if !ok || len(names) != 1 {
		settingsRegistryFlash(w, r, "Choose one family to delete.")
		return
	}
	name := strings.TrimSpace(names[0])
	if err := validate.Name(name); err != nil {
		settingsRegistryFlash(w, r, "Family name: "+err.Error())
		return
	}
	_, err := store.RemoveFamilyMembers(r.Context(), s.writeDB(), name, nil)
	switch {
	case errors.Is(err, store.ErrFamilyNotFound):
		settingsRegistryFlash(w, r, fmt.Sprintf("Family %q no longer exists.", name))
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	settingsRegistryNotice(w, r, fmt.Sprintf("Family %q deleted.", name))
}

// settingsBack answers a Settings POST by going back to the section it came
// from, carrying its one-shot flash (param is "notice" or "error") and the
// block to land on.
func settingsBack(w http.ResponseWriter, r *http.Request, section, param, msg, anchor string) {
	target := "/console/settings?s=" + section + "&" + param + "=" + url.QueryEscape(msg)
	if anchor != "" {
		target += "#" + anchor
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func settingsLevelFlash(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "experience", "error", msg, "experience")
}

func settingsBriefingFlash(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "briefing", "error", msg, "briefing-recipe")
}

func settingsBriefingNotice(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "briefing", "notice", msg, "briefing-recipe")
}

func settingsUtilityFlash(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "engine", "error", msg, "utility-activation")
}

func settingsUtilityNotice(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "engine", "notice", msg, "utility-activation")
}

func settingsEmbeddingsFlash(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "engine", "error", msg, "semantic-index")
}

func settingsEmbeddingsNotice(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "engine", "notice", msg, "semantic-index")
}

func settingsFeaturesFlash(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "features", "error", msg, "features")
}

func settingsFeaturesNotice(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "features", "notice", msg, "features")
}

func settingsRegistryFlash(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "workspaces", "error", msg, "workspace-registry")
}

func settingsRegistryNotice(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "workspaces", "notice", msg, "workspace-registry")
}

// sortedRepoMap projects the repo_map rows for display. store.RepoMapRows
// already returns them ordered by host then path, so this preserves that order
// rather than re-sorting by path alone: grouping a machine's routes together is
// the whole point of showing the host.
func sortedRepoMap(rows []store.RepoMapRow) []repoMapping {
	out := make([]repoMapping, 0, len(rows))
	for _, r := range rows {
		out = append(out, repoMapping{Host: r.Host, Repo: r.Path, Project: r.Slug})
	}
	return out
}

func sortedFamilies(m map[string][]string) []familyGroup {
	out := make([]familyGroup, 0, len(m))
	for name, members := range m {
		sorted := append([]string(nil), members...)
		sort.Strings(sorted)
		out = append(out, familyGroup{Name: name, Members: sorted})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// lessRoute orders routes by host then path, matching store.RepoMapRows so a
// machine's checkouts stay grouped wherever they are rendered.
func lessRoute(a, b repoRoute) bool {
	if a.Host != b.Host {
		return a.Host < b.Host
	}
	return a.Path < b.Path
}

// buildWorkspaceRegistry joins the project registry, the repo_map rows and the
// family settings into the Settings/Context workspace view, returning the scopes
// plus the routes that resolve to no scope at all.
func buildWorkspaceRegistry(projects []core.Project, repoRows []store.RepoMapRow, families map[string][]string) ([]workspaceScope, []repoRoute) {
	bySlug := make(map[string]*workspaceScope, len(projects))
	ensure := func(slug string) *workspaceScope {
		if scope, ok := bySlug[slug]; ok {
			return scope
		}
		scope := &workspaceScope{Slug: slug}
		bySlug[slug] = scope
		return scope
	}

	for _, project := range projects {
		slug := strings.TrimSpace(project.Slug)
		if slug == "" {
			continue
		}
		scope := ensure(slug)
		scope.Name = project.Name
		scope.Description = project.Description
		scope.ParentSlug = strings.TrimSpace(project.ParentSlug)
		scope.Registered = true
		scope.Retired = project.Retired()
	}
	for _, project := range projects {
		if parent := strings.TrimSpace(project.ParentSlug); parent != "" {
			ensure(parent)
		}
	}

	var unboundRepos []repoRoute
	for _, row := range repoRows {
		repo := repoRoute{Host: row.Host, Path: row.Path}
		slug := strings.TrimSpace(row.Slug)
		if slug == "" {
			unboundRepos = append(unboundRepos, repo)
			continue
		}
		scope := ensure(slug)
		scope.Repos = append(scope.Repos, repo)
	}

	for _, family := range sortedFamilies(families) {
		members := uniqueNonEmpty(family.Members)
		for _, slug := range members {
			ensure(slug)
		}
		for _, slug := range members {
			bySlug[slug].Families = append(bySlug[slug].Families, workspaceFamily{
				Name:        family.Name,
				MemberCount: len(members),
			})
		}
	}

	out := make([]workspaceScope, 0, len(bySlug))
	for _, scope := range bySlug {
		sort.Slice(scope.Repos, func(i, j int) bool { return lessRoute(scope.Repos[i], scope.Repos[j]) })
		sort.Slice(scope.Families, func(i, j int) bool {
			return scope.Families[i].Name < scope.Families[j].Name
		})
		if scope.ParentSlug != "" {
			scope.ParentRegistered = bySlug[scope.ParentSlug].Registered
		}
		out = append(out, *scope)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Registered != out[j].Registered {
			return out[i].Registered
		}
		if out[i].Retired != out[j].Retired {
			return !out[i].Retired
		}
		return out[i].Slug < out[j].Slug
	})
	sort.Slice(unboundRepos, func(i, j int) bool { return lessRoute(unboundRepos[i], unboundRepos[j]) })
	return out, unboundRepos
}

func buildFamilyEditors(workspaces []workspaceScope, families map[string][]string) ([]familyEditor, []familyProjectOption) {
	bySlug := make(map[string]workspaceScope, len(workspaces))
	createOptions := make([]familyProjectOption, 0, len(workspaces))
	for _, workspace := range workspaces {
		bySlug[workspace.Slug] = workspace
		if workspace.Registered && !workspace.Retired {
			createOptions = append(createOptions, familyProjectOption{
				Slug:       workspace.Slug,
				Name:       workspace.Name,
				Registered: true,
			})
		}
	}

	groups := sortedFamilies(families)
	editors := make([]familyEditor, 0, len(groups))
	for _, group := range groups {
		members := uniqueNonEmpty(group.Members)
		selected := make(map[string]bool, len(members))
		editor := familyEditor{
			Name:    group.Name,
			Members: make([]familyScopeRef, 0, len(members)),
		}
		for _, slug := range members {
			selected[slug] = true
			editor.Members = append(editor.Members, familyScopeRef{
				Slug:       slug,
				Registered: bySlug[slug].Registered,
			})
		}
		for _, workspace := range workspaces {
			if (!workspace.Registered || workspace.Retired) && !selected[workspace.Slug] {
				continue
			}
			editor.Options = append(editor.Options, familyProjectOption{
				Slug:       workspace.Slug,
				Name:       workspace.Name,
				Registered: workspace.Registered,
				Retired:    workspace.Retired,
				Selected:   selected[workspace.Slug],
			})
		}
		editors = append(editors, editor)
	}
	return editors, createOptions
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
