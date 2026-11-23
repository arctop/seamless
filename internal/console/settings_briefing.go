package console

// Settings > Briefing: the presets, the knobs and their one save route. The
// preview is briefing_preview.go; the section's markup is
// templates/settings_briefing.html.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

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

// briefingGroup is one of the Briefing form's compact groups: an icon, a name
// and its knobs. The groups are drawn from briefingGroups, so the template, the
// zero words and the tests read one table.
type briefingGroup struct {
	Key   string
	Icon  string
	Label string
	Knobs []briefingKnob
}

// briefingKnob is one field of the Briefing form: a label, its hint (the why,
// one hover away) and its control. Kind says which control: "number" (a
// value with its unit), "switch" (a checkbox the form submits as 1) or
// "choice" (radios as segments).
type briefingKnob struct {
	// Name is the form field, as briefingFromForm reads it; ID the control's
	// element id.
	Name, ID string
	Label    string
	Hint     string
	Kind     string
	// Value is a number's value as its input holds it, and Step and Max its
	// bounds when it is not a whole number (the use weight).
	Value     string
	Step, Max string
	// Unit is what a number counts ("lines", "days"); Zero what a 0 means,
	// drawn in the unit's place ("any age", "off"). The page's script swaps
	// them as the value changes.
	Unit, Zero string
	IsZero     bool
	// On is a switch's state.
	On bool
	// Choices are a choice's segments, the checked one marked.
	Choices []briefingChoice
}

// briefingChoice is one segment of a choice knob.
type briefingChoice struct {
	Value, Label string
	On           bool
}

// UnitNow is the word the unit slot shows for the value drawn: the zero
// meaning at 0, the unit otherwise.
func (k briefingKnob) UnitNow() string {
	if k.IsZero {
		return k.Zero
	}
	return k.Unit
}

// briefingInt is a whole-number knob.
func briefingInt(name, id, label, unit, zero, hint string, v int) briefingKnob {
	return briefingKnob{Name: name, ID: id, Label: label, Hint: hint, Kind: "number",
		Value: strconv.Itoa(v), Unit: unit, Zero: zero, IsZero: v == 0}
}

// briefingGroups lays the briefing b out as the form's groups, in the order
// the section draws them. Every field briefingFromForm reads is here exactly
// once (TestBriefingGroups_CarryEveryField), so a save never drops a knob.
func briefingGroups(b config.Briefing) []briefingGroup {
	mode := b.UtilityMode
	if mode == "" {
		mode = "auto"
	}
	choice := func(value, label string) briefingChoice {
		return briefingChoice{Value: value, Label: label, On: mode == value}
	}
	return []briefingGroup{
		{Key: "memory", Icon: "database", Label: "Memories", Knobs: []briefingKnob{
			briefingInt("constraint_max_full", "constraint-max-full", "Constraints in full", "lines", "all in full",
				"The top-ranked constraints are written out in full; the rest share one compact line that still names every one. 0 writes every constraint in full.",
				b.ConstraintMaxFull),
			briefingInt("convention_max_full", "convention-max-full", "Conventions in full", "lines", "all in full",
				"The top-ranked conventions are written out in full; the rest wait behind a count line. 0 writes every convention in full.",
				b.ConventionMaxFull),
			briefingInt("memory_max_items", "memory-max-items", "Memory lines", "lines", "budget",
				"How many lines the memory index lists. 0 lists as many as the token budget holds.",
				b.MemoryMaxItems),
			briefingInt("memory_max_age_days", "memory-max-age", "Recency", "days", "any age",
				"Memories not updated for longer than this leave the index. 0 keeps memories of every age.",
				b.MemoryMaxAgeDays),
		}},
		{Key: "recent", Icon: "activity", Label: "Recent work", Knobs: []briefingKnob{
			briefingInt("findings_count", "findings-count", "Findings", "items", "off",
				"How many recent session findings the briefing shows, newest first. 0 leaves the section out.",
				b.FindingsCount),
			briefingInt("findings_max_age_days", "findings-max-age", "Finding window", "days", "any age",
				"Findings older than this are left out. 0 has no age limit.",
				b.FindingsMaxAgeDays),
			briefingInt("ready_tasks_shown", "ready-tasks-shown", "Ready task titles", "titles", "off",
				"How many ready tasks the ready line names. 0 leaves the line out.",
				b.ReadyTasksShown),
		}},
		{Key: "plans", Icon: "map", Label: "Plans and stages", Knobs: []briefingKnob{
			briefingInt("pending_plan_max_days", "pending-plan-window", "Plans awaiting approval", "days", "any age",
				"A captured plan still awaiting approval is listed for this long. 0 lists every one, however old.",
				b.PendingPlanMaxDays),
			briefingInt("stage_unknown_max_age_days", "stage-unknown-window", "Stages without a gate", "days", "always",
				"A stage whose Status header names no gate stays pinned for this long. 0 pins it for good.",
				b.StageUnknownMaxAgeDays),
		}},
		{Key: "family", Icon: "git-fork", Label: "Project family", Knobs: []briefingKnob{
			briefingInt("sibling_findings_count", "sibling-findings-count", "Sibling findings", "items", "off",
				"Recent findings from the project's family, ranked below its own. 0 leaves the section out.",
				b.SiblingFindingsCount),
			{Name: "include_parent_memories", ID: "include-parent-memories", Label: "Parent\u2019s memories", Kind: "switch",
				Hint: "A child project's briefing includes its shared parent's memory index.", On: b.IncludeParentMemories},
			{Name: "include_sibling_memories", ID: "include-sibling-memories", Label: "Siblings\u2019 memories", Kind: "switch",
				Hint: "Adds a low-priority section of memories from the project's family.", On: b.IncludeSiblingMemories},
		}},
		{Key: "ranking", Icon: "trending-up", Label: "Ranking", Knobs: []briefingKnob{
			{Name: "utility_weight", ID: "utility-weight", Label: "Use weight", Kind: "number",
				Hint:  "How much a memory's recent use by agents counts beside its age, so the memories agents keep using outlast dead weight. 0 orders by recency alone.",
				Value: strconv.FormatFloat(b.UtilityWeight, 'f', -1, 64), Step: "0.1", Max: "1",
				Unit: "of the order", Zero: "recency only", IsZero: b.UtilityWeight == 0},
			{Name: "utility_mode", ID: "utility-mode", Label: "Turns on", Kind: "choice",
				Hint:    "Per project: a project's briefing ranks by use once its history is long enough, which Knowledge engine shows for each. Everywhere: every project, now. Never: no project.",
				Choices: []briefingChoice{choice("auto", "Per project"), choice("on", "Everywhere"), choice("off", "Never")}},
		}},
		{Key: "ceiling", Icon: "gauge", Label: "Ceiling", Knobs: []briefingKnob{
			briefingInt("hard_cap_multiplier", "hard-cap-multiplier", "Hard cap", "\u00d7 budget", "default 2\u00d7",
				"A briefing is cut at this many times the token budget. Constraints and active-plan rollups are never filtered out, nor is a pinned stage while its Status header marks a live gate. 0 uses 2\u00d7.",
				b.HardCapMultiplier),
		}},
	}
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
	latest, err := store.LatestSessionProject(ctx, s.db(ctx))
	if err != nil {
		s.logger.Warn("console: briefing preview default project", "error", err)
	}
	if slices.Contains(live, latest) {
		return live, latest
	}
	return live, live[0]
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
	settingsBriefingNotice(w, r, "Briefing settings saved. They apply from the next session start.")
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

// settingsBriefingReset clears the runtime override row, reverting the
// effective briefing knobs to the file/env configuration.
func (s *Service) settingsBriefingReset(w http.ResponseWriter, r *http.Request) {
	if err := store.ClearBriefingConfig(r.Context(), s.writeDB()); err != nil {
		s.serverError(w, r, err)
		return
	}
	settingsBriefingNotice(w, r, "Briefing overrides cleared: back to the file/env configuration.")
}

func settingsBriefingFlash(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "briefing", "error", msg, "briefing-recipe")
}

func settingsBriefingNotice(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "briefing", "notice", msg, "briefing-recipe")
}
