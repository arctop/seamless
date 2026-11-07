package console

// Console experience levels (plan:console-levels).
//
// The level decides how much of the console the owner SEES: basic (the fewest
// screens and knobs), standard (adds following the work), advanced (every
// screen and knob). It is presentation only -- agents, hooks, MCP tools, the
// briefing, the gardener, and recall are identical at every level -- so it
// lives here, in the console, and nowhere an agent can reach.
//
// Three rules shape everything that reads it:
//
//   - Hidden, not locked. A screen above the level leaves the sidebar, the
//     palette, and the shortcut map, but its URL keeps working (with a soft
//     banner offering the switch), and JSON callers never see a difference.
//   - Monotonic. Each level shows everything the previous one does; the
//     registry guard tests assert the nesting.
//   - Failure-soft, like features. The stored level is resolved live per
//     request, and a corrupt row logs and degrades to the file/env base rather
//     than taking the console down over presentation state.

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// level is a console experience level. Its value is the index into
// config.ConsoleLevels, so comparison is nesting: a higher level shows
// everything a lower one does.
type level int

// The levels, in config.ConsoleLevels order (a guard test pins the two
// together).
const (
	levelBasic level = iota
	levelStandard
	levelAdvanced
)

// allLevels lists every level, fewest surfaces first.
func allLevels() []level {
	out := make([]level, len(config.ConsoleLevels))
	for i := range out {
		out[i] = level(i)
	}
	return out
}

// parseLevel reads a level name. An unknown name is an error naming the valid
// values -- never a silent default.
func parseLevel(name string) (level, error) {
	i := slices.Index(config.ConsoleLevels, name)
	if i < 0 {
		return levelBasic, fmt.Errorf("unknown console level %q: valid values are %s",
			name, strings.Join(config.ConsoleLevels, ", "))
	}
	return level(i), nil
}

// String is the level's config key ("basic").
func (l level) String() string {
	if l < 0 || int(l) >= len(config.ConsoleLevels) {
		return fmt.Sprintf("level(%d)", int(l))
	}
	return config.ConsoleLevels[l]
}

// Label is the owner-facing name ("Basic").
func (l level) Label() string { return titleWord(l.String()) }

// MarshalText makes a level encode as its name wherever it reaches JSON.
func (l level) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// AtLeast reports whether this level shows what a level named name shows. It
// is the ONE template gate for level-dependent markup ({{if $.Level.AtLeast
// "standard"}}). An unknown name is an error, which fails the render loudly
// rather than silently hiding the markup behind a typo.
func (l level) AtLeast(name string) (bool, error) {
	want, err := parseLevel(name)
	if err != nil {
		return false, err
	}
	return l >= want, nil
}

// levelState is the level in force for one request, with the facts the
// Settings precedence line and the Home welcome card read.
type levelState struct {
	Level level
	// Overridden reports a stored row setting the level (else file/env).
	Overridden bool
	// Source says who set a stored level: store.ConsoleLevelChosen or
	// store.ConsoleLevelSeeded ("" when not overridden).
	Source string
	// Welcomed reports whether the owner has seen the Home welcome card.
	Welcomed bool
}

// consoleLevel resolves the level for this request: the file/env base with the
// stored row layered over it. It returns no error on purpose, exactly like
// featuresConfig: a corrupt row is logged and degrades to the base. The
// degraded state counts as welcomed -- a row exists, so this is not a fresh
// installation, and re-asking on every Home load would nag about a fault the
// owner cannot see; choosing a level in Settings rewrites the row.
func (s *Service) consoleLevel(ctx context.Context) levelState {
	name, overridden, source, welcomed, err := store.ConsoleLevel(ctx, s.db(ctx), s.baseLevel.String())
	if err != nil {
		s.logger.Warn("console: console level", "error", err)
		return levelState{Level: s.baseLevel, Welcomed: true}
	}
	lvl, err := parseLevel(name)
	if err != nil {
		// Unreachable while the store validates stored levels; kept so a
		// future store change cannot turn into a wrong level on screen.
		s.logger.Warn("console: console level", "error", err)
		return levelState{Level: s.baseLevel, Welcomed: true}
	}
	return levelState{Level: lvl, Overridden: overridden, Source: source, Welcomed: welcomed}
}

// screenForPath finds the registry screen a console path belongs to: the one
// whose Href is the longest path-segment prefix of it, so /console/memories/X
// is Memories. "/console/" matches only itself -- every console path starts
// with it, and Overview owns nothing beneath it.
func screenForPath(reg []screen, path string) (screen, bool) {
	var best screen
	found := false
	for _, sc := range reg {
		match := path == sc.Href
		if !match && sc.Href != "/console/" {
			match = strings.HasPrefix(path, strings.TrimSuffix(sc.Href, "/")+"/")
		}
		if match && (!found || len(sc.Href) > len(best.Href)) {
			best, found = sc, true
		}
	}
	return best, found
}

// redirectAfterLevelChange decides where a level change lands, as the 303
// target with its notice. When the new level shows the return path's screen --
// or the path belongs to no level-gated screen at all (an event page) -- the
// owner goes back where they were; switching UP always does. When the new level
// hides it, standing on a bannered page would be a strange welcome to a level
// just chosen, so the owner lands on Home with a note naming what left the
// sidebar. A Settings section the new level hides lands on Experience instead,
// where the switch was most likely made. returnPath passes the login
// open-redirect guard first.
func redirectAfterLevelChange(reg []screen, newLevel level, returnPath string) string {
	switched := "Switched to " + newLevel.Label() + "."
	target, err := url.Parse(safeNext(returnPath))
	if err != nil {
		target = &url.URL{Path: "/console/"}
	}
	if sc, ok := screenForPath(reg, target.Path); ok && newLevel < sc.Min {
		target = &url.URL{Path: "/console/"}
		switched += " " + sc.Label + " is not in the " + newLevel.Label() +
			" sidebar, so you are back on Home."
	} else if ok && sc.ID == "settings" {
		if sec, found := settingsSectionByID(target.Query().Get("s")); found && newLevel < sec.Min {
			target = &url.URL{Path: "/console/settings", RawQuery: "s=" + defaultSettingsSection}
			switched += " " + sec.Label + " is not in the " + newLevel.Label() +
				" Settings menu, so you are back on Experience."
		}
	}
	return withNotice(target, switched)
}

// withNotice sets the one-shot notice on a console URL (dropping any stale
// error and fragment) and returns it as a redirect target.
func withNotice(target *url.URL, notice string) string {
	q := target.Query()
	q.Del("error")
	q.Set("notice", notice)
	target.RawQuery = q.Encode()
	target.Fragment = ""
	return target.RequestURI()
}

// levelCard is one level as the Experience section and the Home welcome card
// offer it: the pitch, and what the level shows beyond the one below it.
type levelCard struct {
	Level level
	// Line is the one-sentence pitch.
	Line string
	// Lead introduces Items: "Shows" for the first level, "Adds" after it.
	Lead string
	// Items (screens and Settings sections) and Details (in-page surfaces,
	// grouped by page) are generated from the registries, so a card can never
	// promise what the gates do not do.
	Items   []string
	Details []string
	// Current marks the level in force.
	Current bool
}

// levelPitches are the one-line pitches, by level.
var levelPitches = map[level]string{
	levelBasic:    "Just the essentials: what your agents remember and what they are doing.",
	levelStandard: "Curate knowledge and follow the work.",
	levelAdvanced: "Every screen, every knob, every number.",
}

// levelCards builds the three cards for the current feature state, marking the
// level in force.
func levelCards(feats config.Features, current level) []levelCard {
	out := make([]levelCard, 0, len(config.ConsoleLevels))
	for _, lvl := range allLevels() {
		card := levelCard{Level: lvl, Line: levelPitches[lvl], Lead: "Adds", Current: lvl == current}
		if lvl == levelBasic {
			card.Lead = "Shows"
		}
		card.Items, card.Details = levelAddsScreens(feats, lvl), levelAddsSurfaces(lvl)
		out = append(out, card)
	}
	return out
}

// levelAdds names what lvl shows that the level below it does not (for the
// first level: everything it shows): its screens, then its Settings sections,
// then the in-page surfaces registered at it.
func levelAdds(feats config.Features, lvl level) []string {
	return append(levelAddsScreens(feats, lvl), levelAddsSurfaces(lvl)...)
}

// levelAddsScreens names the screens and Settings sections lvl adds.
func levelAddsScreens(feats config.Features, lvl level) []string {
	var items []string
	for _, sc := range screens {
		if sc.Min == lvl && sc.visibleAt(feats, lvl) {
			items = append(items, sc.Label)
		}
	}
	var sections []string
	for _, sec := range settingsSections {
		if sec.Min == lvl {
			sections = append(sections, sec.Label)
		}
	}
	if len(sections) > 0 {
		items = append(items, "Settings: "+joinWithAnd(sections))
	}
	return items
}

// levelAddsSurfaces names the in-page surfaces lvl adds, grouped by page:
// "Overview: the vitals and ...".
func levelAddsSurfaces(lvl level) []string {
	var items []string
	var where []string
	byWhere := map[string][]string{}
	for _, sf := range surfacesAt(lvl) {
		if _, seen := byWhere[sf.Where]; !seen {
			where = append(where, sf.Where)
		}
		byWhere[sf.Where] = append(byWhere[sf.Where], sf.Label)
	}
	for _, w := range where {
		items = append(items, w+": "+joinWithAnd(byWhere[w]))
	}
	return items
}
