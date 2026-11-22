package console

// The Settings section registry: Settings is one section at a time,
// GET /console/settings?s=<section>, each shaped by what the owner came to do
// rather than by subsystem, and each with a minimum level -- the same contract
// the screen registry gives screens. A section above the level keeps working by
// URL under the banner (hidden, not locked), leaves the sub-nav, and leaves the
// palette's Jump to. The registry order is the sub-nav order at every level.

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/arctop/seamless/internal/backup"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/update"
)

// settingsSection is one Settings section.
type settingsSection struct {
	// ID is the ?s= value.
	ID string
	// Label is the owner-facing name, identical at every level.
	Label string
	// Icon is the lucide glyph the sub-nav entry carries.
	Icon string
	// Blurb is one line saying what the section is for: the sub-nav row's
	// title and the palette's hint.
	Blurb string
	// Min is the lowest level whose sub-nav offers the section.
	Min level
	// Anchor is the element id the section's content carries -- the fragment
	// its POST redirects land on and the legacy #hash a bookmark may still use.
	Anchor string
}

// settingsSections is the ordered registry: the sub-nav order at every level.
var settingsSections = []settingsSection{
	{ID: "experience", Label: "Experience", Icon: "sliders", Min: levelBasic, Anchor: "experience",
		Blurb: "How much of the console you see, and how it looks"},
	{ID: "features", Label: "Features", Icon: "toggle-right", Min: levelBasic, Anchor: "features",
		Blurb: "Optional features, for the console and agents alike"},
	{ID: "setup", Label: "Your setup", Icon: "server", Min: levelBasic, Anchor: "setup",
		Blurb: "Version, files, and what is connected"},
	{ID: "updates", Label: "Updates", Icon: "refresh-cw", Min: levelBasic, Anchor: "updates",
		Blurb: "New releases, and whether this install checks for and installs them"},
	{ID: "backups", Label: "Backups", Icon: "archive", Min: levelBasic, Anchor: "backups",
		Blurb: "Automatic backups, the folder they go to, and restoring one"},
	{ID: "gardener", Label: "Gardener", Icon: "sprout", Min: levelBasic, Anchor: "settings-gardener",
		Blurb: "Who may apply what the gardener suggests, and what applies by itself"},
	{ID: "briefing", Label: "Briefing", Icon: "brain", Min: levelStandard, Anchor: "briefing-recipe",
		Blurb: "What every new agent session starts with"},
	{ID: "workspaces", Label: "Workspaces", Icon: "folder-tree", Min: levelStandard, Anchor: "workspace-registry",
		Blurb: "Projects, repo routes, and families"},
	{ID: "engine", Label: "Knowledge engine", Icon: "gauge", Min: levelAdvanced, Anchor: "knowledge-engine",
		Blurb: "Semantic index, ranking, storage, and policy"},
}

// defaultSettingsSection is the section an unqualified /console/settings opens.
const defaultSettingsSection = "experience"

// settingsSectionIDs lists the valid ?s= values, in registry order.
func settingsSectionIDs() []string {
	out := make([]string, 0, len(settingsSections))
	for _, sec := range settingsSections {
		out = append(out, sec.ID)
	}
	return out
}

// settingsSectionByID returns the section for id; found is false for an
// unknown id.
func settingsSectionByID(id string) (settingsSection, bool) {
	for _, sec := range settingsSections {
		if sec.ID == id {
			return sec, true
		}
	}
	return settingsSection{}, false
}

// settingsSectionParam reads ?s=. Absent means the default section; present
// more than once, or naming no section, is an error naming the valid values --
// never a silent fallback to the default.
func settingsSectionParam(q url.Values) (settingsSection, error) {
	values, present := q["s"]
	if !present {
		sec, _ := settingsSectionByID(defaultSettingsSection)
		return sec, nil
	}
	if len(values) != 1 {
		return settingsSection{}, fmt.Errorf("parameter \"s\" must be provided exactly once")
	}
	sec, ok := settingsSectionByID(values[0])
	if !ok {
		return settingsSection{}, fmt.Errorf("invalid s %q: valid values are %s",
			values[0], strings.Join(settingsSectionIDs(), ", "))
	}
	return sec, nil
}

// Href is the section's URL.
func (sec settingsSection) Href() string { return "/console/settings?s=" + sec.ID }

// sectionState is a section's state in a word or two and a tone: the chip
// its sub-nav row carries ("Up to date", ok), so the owner can tell whether
// backups run or an update waits without opening the section. Tone is one of
// the status chips' tones: ok, accent, quiet, warn or danger.
type sectionState struct {
	Word string `json:"word"`
	Tone string `json:"tone"`
	// Why says what a tone alone cannot (Your setup's warn: semantic recall
	// is misconfigured). Page only: it joins the link's title.
	Why string `json:"-"`
}

// settingsNavEntry is one sub-nav row.
type settingsNavEntry struct {
	settingsSection
	Active bool
	// State is the section's chip; nil draws none.
	State *sectionState
	// Rule draws a divider above the row: the first section above Basic,
	// after the Basic ones.
	Rule bool
}

// Name is the row's accessible name: the label and the state, "Updates, up
// to date", so a screen reader hears the chip with the name.
func (e settingsNavEntry) Name() string {
	if e.State == nil || e.State.Word == "" {
		return e.Label
	}
	w := e.State.Word
	if r := []rune(w); len(r) > 1 && unicode.IsUpper(r[0]) && !unicode.IsUpper(r[1]) {
		w = string(unicode.ToLower(r[0])) + string(r[1:])
	}
	return e.Label + ", " + w
}

// Title is the row's title: the section's blurb, which left the row, and the
// state's reason when it has one.
func (e settingsNavEntry) Title() string {
	if e.State != nil && e.State.Why != "" {
		return e.Blurb + ". " + e.State.Why
	}
	return e.Blurb
}

// settingsSubnav lists the sections the level offers, plus the active one when
// it was reached by URL from above the level -- in its usual place, so the
// sub-nav never hides where the owner is standing. states gives each row its
// chip (settingsSectionStates); a section with none draws no chip.
func settingsSubnav(lvl level, active string, states map[string]sectionState) []settingsNavEntry {
	out := make([]settingsNavEntry, 0, len(settingsSections))
	for _, sec := range settingsSections {
		if lvl >= sec.Min || sec.ID == active {
			e := settingsNavEntry{settingsSection: sec, Active: sec.ID == active}
			if st, ok := states[sec.ID]; ok {
				e.State = &st
			}
			e.Rule = sec.Min > levelBasic && len(out) > 0 && out[len(out)-1].Min == levelBasic
			out = append(out, e)
		}
	}
	return out
}

// visibleSettingsSections lists the sections the level offers, in order.
func visibleSettingsSections(lvl level) []settingsSection {
	return slices.DeleteFunc(slices.Clone(settingsSections), func(sec settingsSection) bool {
		return lvl < sec.Min
	})
}

// sectionBanner is the level banner for a section above the level. It is the
// screens' banner in a section's words: the section lives in the Settings
// menu, not the sidebar.
func sectionBanner(sec settingsSection, current level) *levelBanner {
	if current >= sec.Min {
		return nil
	}
	return &levelBanner{
		ID: "settings:" + sec.ID, Label: sec.Label, Min: sec.Min, Current: current,
		Kind: "Settings section", Place: "your Settings menu",
	}
}

// The sub-nav chips' words, each computed from what the section itself shows:
// settingsData, which the page builds whole at every ?s=, and the update and
// backup services' snapshots, which are cheap reads under their own locks.

// sectionNotRunning is Updates' and Backups' chip in a process that runs no
// update check or no backup service: the section says so too.
var sectionNotRunning = sectionState{Word: "Not running", Tone: "quiet"}

// settingsSectionStates is every section's chip, by section id, whatever the
// level: the JSON answer carries them all (sectionStatus), and the sub-nav
// draws the rows the level offers. Experience's is the level in force, so it
// is the one chip that differs by level, like the level fields themselves.
func (s *Service) settingsSectionStates(d *settingsData, lvl level, now time.Time) map[string]sectionState {
	short, full := s.shortVersion()
	out := map[string]sectionState{
		"experience": {Word: lvl.Label(), Tone: "quiet"},
		"features":   featuresState(d.Features),
		"setup":      setupState(short, full, d.Embeddings),
		"updates":    sectionNotRunning,
		"backups":    sectionNotRunning,
		"briefing":   briefingState(d.Briefing),
		"gardener":   gardenerState(d.Gardener, d.GardenerSwitches.gardenerSwitches),
		"workspaces": {Word: plural(len(d.Projects), "project", "projects"), Tone: "quiet"},
		"engine":     engineState(d.Embeddings),
	}
	if s.cfg.Updates != nil {
		out["updates"] = updatesState(s.cfg.Updates.Status(), now)
	}
	if s.cfg.Backups != nil {
		out["backups"] = backupsState(s.cfg.Backups.Status(), now)
	}
	return out
}

// featuresState counts the features switched on: "5 of 6 on".
func featuresState(cards []featureCard) sectionState {
	on := 0
	for _, c := range cards {
		if c.Enabled {
			on++
		}
	}
	st := sectionState{Word: fmt.Sprintf("%d of %d on", on, len(cards)), Tone: "quiet"}
	if on > 0 {
		st.Tone = "accent"
	}
	return st
}

// shortVersion is the running build's version as a chip says it: "v0.10.6"
// for a release, without the commit its build metadata names. Any other
// build ("0.0.0-dev", a Go pseudo-version) is "" and full is the whole
// string, for the title; both are "" when the build names none.
func (s *Service) shortVersion() (short, full string) {
	full = strings.TrimSpace(s.cfg.Version)
	v, _, _ := strings.Cut(full, "+")
	if parsed, ok := update.Parse(v); ok {
		return "v" + parsed.String(), full
	}
	return "", full
}

// setupState is Your setup's chip: the release's version, or "Dev build" for
// any other build (its whole version goes in the title), in warn while
// semantic recall's settings have a mistake to fix (the section's own
// warning).
func setupState(short, full string, emb embeddingsPanel) sectionState {
	st := sectionState{Word: short, Tone: "quiet"}
	var why []string
	switch {
	case short != "":
	case full != "":
		st.Word = "Dev build"
		why = append(why, "Version "+full+".")
	default:
		st.Word = "Version unknown"
	}
	if !emb.Enabled && emb.Misconfigured {
		st.Tone = "warn"
		why = append(why, "Semantic recall is misconfigured.")
	}
	st.Why = strings.Join(why, " ")
	return st
}

// gardenerState is the Gardener section's chip: what the gardener may change
// without the owner. Switches that could not be read come first (agents are
// barred meanwhile), then suggestions off (no pass proposes anything, so the
// switches decide nothing), then the kinds that apply by themselves ("2 of 4
// automatic", accent, as Features' "on" count is), then agents barred ("Only
// you decide"), else "Proposes only", the Gardener screen's own words for it.
func gardenerState(cfg config.Gardener, sw gardenerSwitches) sectionState {
	switch {
	case sw.ReadFailed:
		return sectionState{Word: "Unreadable", Tone: "warn",
			Why: "The stored switches could not be read: the config file's values show, and agents are barred from deciding meanwhile."}
	case !cfg.Enabled:
		return sectionState{Word: "Suggestions off", Tone: "quiet",
			Why: "gardener.enabled is false, so no pass proposes anything."}
	case len(sw.AutoApply) > 0:
		return sectionState{Word: fmt.Sprintf("%d of %d automatic", len(sw.AutoApply), len(gardener.AutoSafeKinds())), Tone: "accent"}
	case !sw.AgentsMayApply:
		return sectionState{Word: "Only you decide", Tone: "quiet"}
	}
	return sectionState{Word: "Proposes only", Tone: "quiet"}
}

// briefingState names the preset the briefing in force equals, else Custom.
func briefingState(b config.Briefing) sectionState {
	if p, ok := config.MatchBriefingPreset(b); ok {
		return sectionState{Word: p.Label, Tone: "quiet"}
	}
	return sectionState{Word: "Custom", Tone: "quiet"}
}

// engineState says whether recall is semantic or keywords only.
func engineState(emb embeddingsPanel) sectionState {
	if emb.Enabled {
		return sectionState{Word: "Semantic", Tone: "ok"}
	}
	return sectionState{Word: "Keywords only", Tone: "warn"}
}

// updatesState is Updates' chip, in the order the owner needs it: the
// checker stopped or an update under way, automatic updates paused, a recent
// update that failed (the every-page alert's), checks off, a newer release,
// a failing check, else up to date. Every version in it is a parsed one
// (memory update-surfaces-render-parsed-versions-only).
func updatesState(st update.Status, now time.Time) sectionState {
	switch {
	case st.Stopped:
		return sectionState{Word: "Stopped", Tone: "danger"}
	case st.Applying != nil:
		return sectionState{Word: "Updating", Tone: "accent"}
	}
	alert := updateAlertFor(st, now)
	switch {
	case st.Paused != nil && (selfUpdating(st) || alert != nil):
		if st.Paused.Reason == update.PauseBroken {
			return sectionState{Word: "Paused", Tone: "danger"}
		}
		return sectionState{Word: "Paused", Tone: "warn"}
	case alert != nil:
		return sectionState{Word: "Failed", Tone: "danger"}
	case !st.Settings.Check:
		return sectionState{Word: "Checks off", Tone: "quiet"}
	case st.Newest != nil && st.Available:
		return sectionState{Word: "v" + st.Newest.Version.String() + " ready", Tone: "accent"}
	case st.CheckError != nil:
		return sectionState{Word: "Check failed", Tone: "warn"}
	}
	if _, ok := st.Current(); ok && st.Newest != nil {
		return sectionState{Word: "Up to date", Tone: "ok"}
	}
	if st.CheckedAt.IsZero() {
		return sectionState{Word: "Not checked yet", Tone: "quiet"}
	}
	return sectionState{Word: "Checks on", Tone: "quiet"}
}

// backupsState is Backups' chip: when the last backup ran while automatic
// backups are on ("2h ago"), else what is wrong or off. A failed automatic
// run is danger from the first one (the section's card says Failing then);
// overdue is the every-page alert's other case.
func backupsState(st backup.Status, now time.Time) sectionState {
	set := st.Settings
	switch {
	case st.Stopped:
		return sectionState{Word: "Stopped", Tone: "danger"}
	case set.Auto && st.Failures > 0:
		return sectionState{Word: "Failing", Tone: "danger"}
	case st.Running != nil:
		return sectionState{Word: "Backing up", Tone: "accent"}
	case !set.Auto:
		return sectionState{Word: "Off", Tone: "quiet"}
	case st.Alert(now):
		return sectionState{Word: "Overdue", Tone: "warn"}
	case st.LastBackup == nil:
		return sectionState{Word: "None yet", Tone: "warn"}
	}
	return sectionState{Word: agoPhraseAt(st.LastBackup.At, now), Tone: "ok"}
}
