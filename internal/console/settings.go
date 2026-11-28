package console

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
)

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
	// GardenerSwitches is the gardener's two switches in force, with where
	// each came from (settings_gardener.go): the Gardener section edits
	// them, and Your setup states them.
	GardenerSwitches gardenerPanel `json:"gardenerSwitches"`
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
	// "seeded" by the upgrade). They and SectionStatus's "experience" entry
	// are the ONLY values that differ by level: everything above is the full
	// payload at every level and every section.
	ConsoleLevel           string `json:"consoleLevel"`
	ConsoleLevelOverridden bool   `json:"consoleLevelOverridden"`
	ConsoleLevelSource     string `json:"consoleLevelSource,omitempty"`
	// SectionStatus is each section's state in a word and a tone, by section
	// id: the sub-nav's chips (settingsSectionStates), every section at every
	// level. Its "experience" entry names the level in force, so it differs by
	// level as the three fields above do; the rest is level-blind.
	SectionStatus map[string]sectionState `json:"sectionStatus"`

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
	// BriefingGroups are the Briefing section's knobs in their groups
	// (briefingGroups), drawn from the effective values.
	BriefingGroups []briefingGroup `json:"-"`
	// Ranking is Knowledge engine's Ranking card: UtilityRows grouped into
	// the rows it lists and the ones it folds (utilityRankingOf).
	Ranking utilityRanking `json:"-"`
	// Setup is the Your setup section's plain-language facts.
	Setup setupPanel `json:"-"`
	// Updates is the Updates section's view of the background update check,
	// filled only for that section. Page state like Setup: the JSON payload
	// stays the same at every section.
	Updates updatesPanel `json:"-"`
	// Backups is the Backups section's view of the backup service, filled
	// only for that section, like Updates.
	Backups backupsPanel `json:"-"`
	// PreviewProjects are the projects the Briefing section's preview offers
	// (registered and live, in slug order), and PreviewProject the one it
	// opens on: where the owner's agents worked last, else the first. Filled
	// only for the Briefing section.
	PreviewProjects []string `json:"-"`
	PreviewProject  string   `json:"-"`
}

func (s *Service) settings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	section, err := settingsSectionParam(r.URL.Query())
	if err != nil {
		s.badRequest(w, r, err.Error())
		return
	}

	projects, err := store.ListProjects(ctx, s.db(ctx))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	repoRows, err := store.RepoMapRows(ctx, s.db(ctx))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	families, err := store.ProjectFamilies(ctx, s.db(ctx))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	workspaces, unboundRepos := buildWorkspaceRegistry(projects, repoRows, families)
	familyEditors, familyOptions := buildFamilyEditors(workspaces, families)
	briefing, overridden, err := store.BriefingConfig(ctx, s.db(ctx), s.cfg.BriefingCfg)
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
	rawCounts, cerr := store.GetNavCounts(ctx, s.db(ctx))
	if cerr != nil {
		// Best-effort, like the sidebar badges: a counts failure costs the
		// reassurance line, not the page.
		s.logger.Warn("console: settings feature counts", "error", cerr)
	}
	heldCount, herr := store.CountPendingHolds(ctx, s.db(ctx))
	if herr != nil {
		s.logger.Warn("console: settings held count", "error", herr)
	}
	// The screening card counts what screening holds: a restricted account's
	// proposals wait on the Held screen whether screening is on or off, so
	// they are not screening's data to keep or release.
	if s.cfg.RestrictedAccounts && heldCount > 0 {
		if proposals, perr := s.pendingProposals(ctx); perr != nil {
			s.logger.Warn("console: settings proposal count", "error", perr)
		} else {
			heldCount = max(0, heldCount-len(proposals))
		}
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
	var backups backupsPanel
	if section.ID == "backups" && !wantsJSON(r) {
		backups = s.backupsPanelFor(r, time.Now())
	}

	lvl := s.consoleLevel(ctx)
	data := settingsData{
		DataDir:            s.cfg.DataDir,
		Embeddings:         embeddings,
		Database:           s.databasePanel(),
		Budgets:            s.cfg.Budgets,
		Gardener:           s.cfg.GardenerCfg,
		GardenerSwitches:   s.gardenerPanelFor(ctx),
		Briefing:           briefing,
		BriefingOverridden: overridden,
		Features:           featureCardsFor(featuresCfg, navCounts{Labs: rawCounts.Labs, Trials: rawCounts.Trials, Held: heldCount}, s.cfg.RestrictedAccounts),
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
		LevelCards:             levelCards(featuresCfg, lvl.Level),
		BriefingPresets:        briefingPresetCards(briefing),
		BriefingGroups:         briefingGroups(briefing),
		Ranking:                utilityRankingOf(utilityRows, briefing.UtilityMode),
		Setup:                  s.setupData(ctx),
		Updates:                updates,
		Backups:                backups,
		PreviewProjects:        previewProjects,
		PreviewProject:         previewProject,
	}
	data.SectionStatus = s.settingsSectionStates(&data, lvl.Level, time.Now())
	data.Subnav = settingsSubnav(lvl.Level, section.ID, data.SectionStatus)
	s.render(w, r, "settings", pageData{
		Title:       "Settings \u00b7 " + section.Label,
		Active:      "settings",
		LevelBanner: sectionBanner(section, lvl.Level),
		Data:        data,
	})
}

// eventFeaturesChanged records an optional-feature toggle in the event log, so
// a change of what agents can reach shows up in Activity with the same
// attribution as every other owner action.
const eventFeaturesChanged = core.EventFeaturesChanged

// eventLevelChanged records a console experience-level change in the event log
// (payload {from, to, by: "console"}, plus reset: true when the owner went back
// to the file/env level), so the change shows in Activity like a features
// toggle. The level is presentation only: nothing an agent receives changes,
// which is why this is a settings event and not a features one.
const eventLevelChanged core.EventKind = "settings.level_changed"

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
	// The welcome card's write-screening box rides with the level choice:
	// unticked (absent) changes nothing, ticked turns the feature on.
	if ok := s.welcomeFeatureOptIn(w, r); !ok {
		return
	}
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
	if ok := s.welcomeFeatureOptIn(w, r); !ok {
		return
	}
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

// welcomeOptInFeatures are the optional features the Home welcome card may
// turn on beside the level choice. Only write screening: it changes what
// agents receive, so it is offered as its own choice there instead of hiding
// inside a level.
var welcomeOptInFeatures = []features.Key{features.Screening}

// welcomeFeatureOptIn applies the welcome card's feature boxes posted with a
// level choice or a dismissal. An absent box changes nothing, at any level; a
// ticked one turns its feature on through enableFeature -- the same store call
// and event as Settings -> Features. A box present with any value other than
// the checkbox's own is refused, never read as off. It reports whether the
// handler may go on.
func (s *Service) welcomeFeatureOptIn(w http.ResponseWriter, r *http.Request) bool {
	for _, key := range welcomeOptInFeatures {
		values, present := r.PostForm[featureField(key)]
		if !present {
			continue
		}
		if len(values) != 1 || values[0] != "1" {
			settingsLevelFlash(w, r, fmt.Sprintf("%s must be omitted (off) or set exactly once to 1 (on)", featureField(key)))
			return false
		}
		if err := s.enableFeature(r.Context(), key); err != nil {
			s.serverError(w, r, err)
			return false
		}
	}
	return true
}

// enableFeature turns one optional feature on and leaves every other one as
// it is, through the store call and the event Settings -> Features uses: the
// whole effective set stored as the override row, and settings.features_changed
// naming every feature's resulting state. Already on, it stores and logs
// nothing. Turning a feature on deletes nothing and is undone in Settings.
func (s *Service) enableFeature(ctx context.Context, key features.Key) error {
	f, ok := features.Get(key)
	if !ok {
		return fmt.Errorf("console: unknown feature %q", key)
	}
	cfg, _ := s.featuresConfig(ctx)
	if f.Enabled(cfg) {
		return nil
	}
	f.Set(&cfg, true)
	if err := store.SetFeaturesConfig(ctx, s.writeDB(), cfg); err != nil {
		return err
	}
	changed := make(map[string]any, len(features.Registry()))
	for _, g := range features.Registry() {
		changed[string(g.Key)] = g.Enabled(cfg)
	}
	if s.cfg.Events != nil {
		if _, err := s.eventLog(ctx).Record(ctx, core.Event{
			Kind:    eventFeaturesChanged,
			Payload: map[string]any{"features": changed, "by": "console"},
		}); err != nil {
			s.logger.Warn("console: record features event", "error", err)
		}
	}
	return nil
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
	if _, err := s.eventLog(ctx).Record(ctx, core.Event{Kind: eventLevelChanged, Payload: payload}); err != nil {
		s.logger.Warn("console: record level event", "error", err)
	}
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
