package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/store"
)

// Settings > Knowledge engine > Gardener: the gardener's two switches. One
// says whether agents may resolve proposals (gardener.agents_may_apply, on by
// default); the other names the kinds a pass applies by itself the moment it
// raises one (gardener.auto_apply, empty by default). Each control says where
// its value comes from and offers Reset, as Settings > Updates does. The
// console's choice wins over the file until Reset, except that
// agents_may_apply: false in the file or environment is final
// (gardener.Effective). Both routes are the owner's own decision (ownerHands):
// each changes what may alter agents' knowledge without the owner's review.
//
// The same switches guard the Gardener screen's writes (gardenerAgentsBarred)
// and feed its sentence about who may apply (gardenerSwitches), and the
// Overview counts what automatic apply did (autoAppliedCards). Every reader
// reads the stored row when it decides, so a change applies without a
// restart.

// gardenerSettingsAnchor is the Gardener group's element id on Settings >
// Knowledge engine: the fragment the Gardener screen links to and the
// switches' POSTs return to.
const gardenerSettingsAnchor = "settings-gardener"

// gardenerSwitches is the gardener's switches in force, for the Gardener
// screen and Settings. AgentsMayApply and AutoApply (in store.ProposalKinds
// order) are the frozen contract the Gardener screen reads; the rest says
// where each came from.
type gardenerSwitches struct {
	AgentsMayApply bool
	AutoApply      []string
	// AgentsSource and AutoSource are gardener.SourceDefault, SourceConfig or
	// SourceConsole. AgentsLocked marks agents_may_apply: false in the file
	// or environment, which the console cannot turn on.
	AgentsSource string
	AgentsLocked bool
	AutoSource   string
	// Unreadable marks a stored choice that could not be read: the file
	// decides, and a save replaces it. ReadFailed marks switches that could
	// not be read at all: the values are the file's, and the Gardener's
	// writes refuse an agent's process meanwhile (gardenerAgentsBarred).
	Unreadable bool
	ReadFailed bool
}

// readSwitches is the switches in force for this request, read through its
// handle (the settings row is no project's, so every handle reads it).
func (s *Service) readSwitches(ctx context.Context) (gardener.Switches, error) {
	return gardener.ReadSwitches(ctx, s.db(ctx), s.cfg.GardenerCfg, s.logger)
}

// gardenerSwitches is the gardener's switches in force. A failed read is
// logged and answered with the file/env values, marked ReadFailed; a guard
// that must fail closed reads gardenerAgentsBarred instead.
func (s *Service) gardenerSwitches(ctx context.Context) gardenerSwitches {
	sw, err := s.readSwitches(ctx)
	failed := false
	if err != nil {
		s.logger.Warn("console: gardener switches unreadable; showing the config file's", "error", err)
		sw, failed = gardener.Effective(s.cfg.GardenerCfg, config.GardenerOverride{}), true
	}
	return gardenerSwitches{
		AgentsMayApply: sw.AgentsMayApply, AutoApply: sw.AutoApply,
		AgentsSource: sw.AgentsSource, AgentsLocked: sw.AgentsLocked, AutoSource: sw.AutoSource,
		Unreadable: sw.Unreadable, ReadFailed: failed,
	}
}

// gardenerAgentsBarred reports whether the Gardener's writes are the owner's
// own decision now: the effective gardener.agents_may_apply is false, or the
// switches cannot be read (deciding nothing is the safe answer). Console.go
// wraps each Gardener write as ownerHandsIf(s.gardenerAgentsBarred, ...), so
// while it is true an agent's process on the peer_auth socket and a paired
// device are refused, and the owner's browser, terminal and bearer key are
// not. It writes nothing.
func (s *Service) gardenerAgentsBarred(r *http.Request) bool {
	sw, err := s.readSwitches(r.Context())
	if err != nil {
		s.logger.Warn("console: gardener switches unreadable; the Gardener's writes are the owner's", "error", err)
		return true
	}
	return !sw.AgentsMayApply
}

// gardenerPanel is the Settings Gardener group's view of the switches.
type gardenerPanel struct {
	gardenerSwitches
	// AgentsLabel and AgentsNote say who decided the agents switch, and what
	// that means for the console's own switch; AutoLabel and AutoNote the
	// same for automatic apply.
	AgentsLabel, AgentsNote string
	AutoLabel, AutoNote     string
	// Overridden reports a console choice in force for either switch, which
	// Reset clears.
	Overridden bool
	// LockedNote says where a locked agents switch was set; empty when not.
	LockedNote string
	// Kinds are automatic apply's checkboxes, one per AutoSafe kind, in
	// store.ProposalKinds order.
	Kinds []gardenerAutoKind
}

// gardenerAutoKind is one "Apply automatically" checkbox.
type gardenerAutoKind struct {
	Kind, Label, Sentence string
	On                    bool
}

// gardenerAutoWords are the four AutoSafe kinds in the owner's words: a label
// and one plain sentence on what applying it does and how Undo takes it back.
var gardenerAutoWords = map[string][2]string{
	store.ProposalAbandonPlan: {"Abandon a stale captured plan",
		"Retags a captured plan nobody approved as abandoned once it has sat untouched past the cutoff; Undo restores its status."},
	store.ProposalMemoryWanted: {"Open a task for a knowledge gap",
		"When agents keep searching for something recall cannot find, opens a task to write it; Undo drops the task while nobody has picked it up."},
	store.ProposalToolError: {"Open a task for a recurring error",
		"When agents keep hitting the same tool or hook error, opens a task to fix it; Undo drops the task while nobody has picked it up."},
	store.ProposalShipPlan: {"Mark a finished plan shipped",
		"Retags a captured plan whose work landed in git without an approval as shipped; Undo restores its status."},
}

// gardenerAgentsLockedText names where a locked agents switch was set.
const gardenerAgentsLockedText = "gardener.agents_may_apply: false is set in the config file or SEAMLESS_GARDENER_AGENTS_MAY_APPLY; change it there."

// gardenerPanelFor builds the Settings Gardener group.
func (s *Service) gardenerPanelFor(ctx context.Context) gardenerPanel {
	sw := s.gardenerSwitches(ctx)
	p := gardenerPanel{gardenerSwitches: sw,
		Overridden: sw.AgentsSource == gardener.SourceConsole || sw.AutoSource == gardener.SourceConsole}
	switch {
	case sw.AgentsLocked:
		p.AgentsLabel, p.AgentsNote = "Set in the config file or environment",
			"gardener.agents_may_apply: false is final there, so this console cannot turn agents' applying on."
		p.LockedNote = gardenerAgentsLockedText
	case sw.AgentsSource == gardener.SourceConsole:
		p.AgentsLabel, p.AgentsNote = "Set in this console", "It wins over the config file until you reset it."
	case sw.AgentsSource == gardener.SourceConfig:
		p.AgentsLabel, p.AgentsNote = "Set in the config file or environment",
			"gardener.agents_may_apply: true is set there; you can still turn it off here."
	default:
		p.AgentsLabel, p.AgentsNote = "Default", "Agents may apply proposals unless you turn it off here or set gardener.agents_may_apply in the config file."
	}
	switch sw.AutoSource {
	case gardener.SourceConsole:
		p.AutoLabel, p.AutoNote = "Set in this console", "It wins over the config file until you reset it."
	case gardener.SourceConfig:
		p.AutoLabel, p.AutoNote = "Set in the config file or environment", "gardener.auto_apply names these kinds there; saving here wins over it until you reset."
	default:
		p.AutoLabel, p.AutoNote = "Default", "Nothing applies by itself unless you choose kinds here or set gardener.auto_apply in the config file."
	}
	for _, k := range gardener.AutoSafeKinds() {
		w := gardenerAutoWords[k]
		p.Kinds = append(p.Kinds, gardenerAutoKind{Kind: k, Label: w[0], Sentence: w[1], On: slices.Contains(sw.AutoApply, k)})
	}
	return p
}

// settingsGardenerSave stores the console's choice for one switch or both.
// Form field agents, "on" or "off", sets whether agents may apply proposals;
// auto_set=1 with zero or more auto=<kind> fields sets the kinds automatic
// apply takes (none checked: nothing applies by itself). Each control's form
// carries its own fields, so saving one leaves the other's source as it was;
// a form carrying neither is refused, naming both. Turning agents' applying
// on while the file or environment says agents_may_apply: false is refused
// with nothing stored, and so is a kind that is not one of the four AutoSafe
// ones: "merge" from a hand-made form is refused as the config file's would
// be. The route is ownerHands: every direction is the owner's.
func (s *Service) settingsGardenerSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var agents *bool
	if _, present := r.PostForm["agents"]; present {
		on, refusal := onOffField(r.PostForm, "agents")
		if refusal != "" {
			settingsGardenerFlash(w, r, refusal)
			return
		}
		agents = &on
	}
	var auto *[]string
	switch values, present := r.PostForm["auto_set"]; {
	case present && (len(values) != 1 || values[0] != "1"):
		settingsGardenerFlash(w, r, "auto_set must be given exactly once, as 1")
		return
	case present:
		kinds, refusal := autoKindsField(r.PostForm["auto"])
		if refusal != "" {
			settingsGardenerFlash(w, r, refusal)
			return
		}
		auto = &kinds
	case len(r.PostForm["auto"]) > 0:
		settingsGardenerFlash(w, r, "auto kinds come with auto_set=1, which says this form sets automatic apply")
		return
	}
	if agents == nil && auto == nil {
		settingsGardenerFlash(w, r, "nothing to save: send agents=on or off, or auto_set=1 with the kinds to apply automatically")
		return
	}

	sw, err := s.readSwitches(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if agents != nil && *agents && sw.AgentsLocked {
		settingsGardenerFlash(w, r, "Agents stay barred from applying proposals: "+gardenerAgentsLockedText)
		return
	}
	o, replaced, err := s.gardenerOverrideForSave(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	payload := map[string]any{"action": gardener.ActionSwitches, "by": "console"}
	var said []string
	if agents != nil {
		o.AgentsMayApply = agents
		payload["agents_may_apply"] = *agents
		if *agents {
			said = append(said, "Agents may apply, dismiss and hide proposals again.")
		} else {
			said = append(said, "Agents may no longer apply, dismiss or hide proposals: gardener_apply refuses them, and so do the Gardener's writes from an agent's process.")
		}
	}
	if auto != nil {
		o.AutoApply = auto
		payload["auto_apply"] = *auto
		said = append(said, autoAppliesSentence(*auto))
	}
	if err := store.SetGardenerOverride(ctx, s.writeDB(), o); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.recordGardenerSwitches(ctx, payload)
	msg := strings.Join(said, " ")
	if replaced {
		msg += " The stored gardener setting was unreadable and has been replaced."
	}
	settingsGardenerNotice(w, r, msg)
}

// autoKindsField reads the auto=<kind> checkboxes: each an AutoSafe kind,
// none twice. It answers the kinds in store.ProposalKinds order, or the
// flash that refuses them.
func autoKindsField(values []string) (kinds []string, refusal string) {
	if err := config.ValidateGardenerAutoApply("auto", values); err != nil {
		return nil, strings.TrimPrefix(err.Error(), "config: ")
	}
	for i, v := range values {
		if slices.Contains(values[:i], v) {
			return nil, fmt.Sprintf("auto %q must be given at most once", v)
		}
	}
	out := []string{}
	for _, k := range gardener.AutoSafeKinds() {
		if slices.Contains(values, k) {
			out = append(out, k)
		}
	}
	return out, ""
}

// autoAppliesSentence says what automatic apply now takes.
func autoAppliesSentence(kinds []string) string {
	if len(kinds) == 0 {
		return "Automatic apply is off: every proposal waits for a decision."
	}
	labels := make([]string, len(kinds))
	for i, k := range kinds {
		labels[i] = strings.ReplaceAll(k, "_", " ")
	}
	return "New " + strings.Join(labels, ", ") + " proposals are applied by themselves from the next gardener pass; each can be undone from the decision history."
}

// settingsGardenerReset clears the console's gardener override -- both
// switches -- so the config file and environment decide again.
func (s *Service) settingsGardenerReset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := store.ClearGardenerOverride(ctx, s.writeDB()); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.recordGardenerSwitches(ctx, map[string]any{"action": gardener.ActionSwitchesReset, "by": "console"})
	sw := s.gardenerSwitches(ctx)
	agents := "agents may apply proposals"
	if !sw.AgentsMayApply {
		agents = "agents may not apply proposals"
	}
	settingsGardenerNotice(w, r, "The gardener's switches follow the config file again: "+agents+". "+autoAppliesSentence(sw.AutoApply))
}

// recordGardenerSwitches records a switches change as a gardener.action event.
func (s *Service) recordGardenerSwitches(ctx context.Context, payload map[string]any) {
	if s.cfg.Events == nil {
		return
	}
	if _, err := s.eventLog(ctx).Record(ctx, core.Event{Kind: core.EventGardenerAction, Payload: payload}); err != nil {
		s.logger.Warn("console: record gardener switches event", "error", err)
	}
}

// gardenerOverrideForSave reads the stored override a save is about to
// change. One that cannot be read cannot be kept, and refusing would leave the
// switches stuck: every reader already ignores it, so the save replaces it
// (replaced), and the flash says so.
func (s *Service) gardenerOverrideForSave(ctx context.Context) (o config.GardenerOverride, replaced bool, err error) {
	o, _, err = store.GardenerOverride(ctx, s.db(ctx))
	if err == nil {
		return o, false, nil
	}
	if !errors.Is(err, store.ErrGardenerOverrideInvalid) {
		return config.GardenerOverride{}, false, err
	}
	s.logger.Warn("console: replacing an unreadable gardener override", "error", err)
	return config.GardenerOverride{}, true, nil
}

func settingsGardenerFlash(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "engine", "error", msg, gardenerSettingsAnchor)
}

func settingsGardenerNotice(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "engine", "notice", msg, gardenerSettingsAnchor)
}

// autoAppliedCards is the Overview's informational card: how many proposals
// automatic apply applied since the owner last looked, which is the newest
// decision the owner made on a proposal from the console (an apply, dismiss,
// hide, unhide or undo there: any gardener.action recorded by console but
// gardener.NotDecisions -- asking for a pass, for proposals or a split, and
// changing the switches). Looking without deciding writes nothing, so it does
// not count: no GET here writes. Before any such decision, it
// counts every automatic apply. It counts the rows the decision history's
// Applied automatically section lists (status applied, resolved_by auto),
// through the request's handle, so a locked project's proposals are not
// counted. Nothing applied: no card. A failed read costs the card, never the
// page.
func (s *Service) autoAppliedCards(ctx context.Context) []attnCard {
	db := s.db(ctx)
	since, looked, err := store.LastGardenerActionBy(ctx, db, "console", gardener.NotDecisions)
	if err != nil {
		s.logger.Warn("console: last gardener decision for the attention strip", "error", err)
		return nil
	}
	n, err := store.CountProposalsAppliedSince(ctx, db, gardener.ByAuto, since)
	if err != nil {
		s.logger.Warn("console: automatic applies for the attention strip", "error", err)
		return nil
	}
	if n == 0 {
		return nil
	}
	sub := "since your last Gardener decision · review or undo them"
	if !looked {
		sub = "review or undo them in the decision history"
	}
	return []attnCard{{
		Sev: "info", Icon: "sparkles",
		Title: plural(n, "proposal applied automatically", "proposals applied automatically"),
		Sub:   sub,
		Href:  "/console/gardener/decisions#gardener-auto",
	}}
}

// SetupLine is the Your setup row's sentence about the switches.
func (p gardenerPanel) SetupLine() string {
	agents := "agents may apply proposals"
	if !p.AgentsMayApply {
		agents = "only you apply proposals"
	}
	if len(p.AutoApply) == 0 {
		return agents + "; nothing applies by itself"
	}
	labels := make([]string, len(p.AutoApply))
	for i, k := range p.AutoApply {
		labels[i] = strings.ReplaceAll(k, "_", " ")
	}
	return agents + "; " + strings.Join(labels, ", ") + " apply by themselves"
}
