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
)

// settingsSection is one Settings section.
type settingsSection struct {
	// ID is the ?s= value.
	ID string
	// Label is the owner-facing name, identical at every level.
	Label string
	// Icon is the lucide glyph the sub-nav entry carries.
	Icon string
	// Blurb is one line saying what the section is for: the sub-nav's second
	// line and the palette's hint.
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

// settingsNavEntry is one sub-nav row.
type settingsNavEntry struct {
	settingsSection
	Active bool
}

// settingsSubnav lists the sections the level offers, plus the active one when
// it was reached by URL from above the level -- in its usual place, so the
// sub-nav never hides where the owner is standing.
func settingsSubnav(lvl level, active string) []settingsNavEntry {
	out := make([]settingsNavEntry, 0, len(settingsSections))
	for _, sec := range settingsSections {
		if lvl >= sec.Min || sec.ID == active {
			out = append(out, settingsNavEntry{settingsSection: sec, Active: sec.ID == active})
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
