// Package features is the registry of OPTIONAL features: the parts of Seamless
// the owner can switch on and off from the console without losing any data.
//
// It is the single source of truth every surface derives from -- the MCP tool
// filter, the console route gate and nav, the Settings rows, the docs, and the
// guard tests. Adding a future optional feature is one registry entry plus one
// config.Features field plus tagging its surfaces; nothing else hand-maintains a
// list of what a feature covers.
//
// The package is a leaf: it imports config and nothing else, so mcp, console,
// store, skills, and cmd/ can all import it without a cycle. The
// cross-consistency tests (registry tools exist in the MCP catalog, route
// prefixes are registered routes, nav ids exist in the layout) therefore live in
// those packages, where the imports are legal.
//
// Optional features default to OFF: a fresh installation exposes none of them
// until the owner enables it. Disabling never deletes anything -- it gates
// exposure only, and re-enabling restores every surface instantly.
package features

import (
	"slices"

	"github.com/arctop/seamless/internal/config"
)

// Key identifies an optional feature. It is the stable identifier used by the
// config field, the settings form, and the URL fragment on the Settings page.
type Key string

// Research covers the whole research-lab domain: both console screens (Labs,
// Trials) and all three MCP tools. Labs and trials are one feature because a lab
// has no independent existence -- there is no labs table and no Lab core type, a
// lab is only the label its trials carry -- so splitting them would leave either
// a fully usable feature or an empty screen.
const Research Key = "research"

// Momentum (the finish lines, the capture calendar, payoffs, maturity stages
// and milestones) and gamification (the Now screen's arcade) were optional
// features here until they became core: they are always on and have no key.
// A config file may still name them (config.Features' retired fields).

// Screening covers write screening and the held queue: every agent write to a
// memory or note is screened, and a suspicious write -- or a change to a
// binding rule by a session marked as having read outside text, or the
// retirement of a constraint -- is held out of briefings, recall and the
// gardener until the owner releases it on the Held screen. Injected memories
// carry an owner-confirmed or agent-written mark while it is on. It is one
// feature because the parts only mean something together: a screen with no
// hold only counts, a hold with no screen has nothing to catch, and the marks
// are how an agent tells released knowledge from the rest.
//
// Part 2 joins the same toggle rather than adding one: the review of the
// existing corpus at the first turn-on, the drift quarantine of files changed
// outside Seamless, the lockdown switch, access.refused events and the
// optional metadata feed. They extend the same promise -- what agents receive
// has passed the screens or the owner -- and mean nothing without the screens.
//
// Unlike a feature that only shows the owner more, it changes what agents
// receive, so it is a feature toggle and never part of a console level. Turning it off
// stops screening new writes; a write already held stays held -- out of the
// corpus -- until the feature is back on and the owner decides.
const Screening Key = "screening"

// RepoLocks covers the repo locks: locks on a repository's refs (its main
// branch, a tag pattern) that agents declare once and hold while they land
// changes, so landings happen one at a time across machines -- the Locks
// console screen, the four locks_* MCP tools, and `seam lock`. They are one
// feature because a lock nobody can see or hold means nothing, and a screen
// with no tools to fill it would stay empty. Turning it off deletes no lock.
const RepoLocks Key = "repo_locks"

// Diary covers the Diary console screen -- one entry per local day saying
// what was worked on, which agents did it, and what it took in agent time and
// model tokens -- and the in-page link into it from a Sessions day view. It
// owns no MCP tools, and the usage ledger and the event log it reads are
// recorded whether it is on or off, so turning it on shows the history at
// once. The one thing it changes beyond what the owner sees: while it is on,
// a Claude Code prompt keeps its session's usage ledger current, read after
// the prompt is answered, at most once every three minutes a conversation
// (internal/hooks usage_live.go).
const Diary Key = "diary"

// Feature describes one optional feature and every surface it owns.
type Feature struct {
	// Key is the stable identifier (also the settings anchor and form field).
	Key Key
	// Label is the owner-facing name, shown in Settings.
	Label string
	// Blurb is one sentence explaining what the feature is. The docs and the
	// doctor read it, and the Settings row keeps it in its hint.
	Blurb string
	// Line is the feature in one short sentence (at most lineMax runes),
	// the description on its Settings row.
	Line string
	// Tools are the MCP tool names hidden from tools/list and rejected by
	// tools/call while the feature is off. Every name must exist in
	// mcp.Catalog() (asserted by a guard test in internal/mcp).
	Tools []string
	// NavIDs are the console sidebar nav entries to hide.
	NavIDs []string
	// RoutePrefixes are the console route prefixes to gate.
	RoutePrefixes []string
	// Surfaces are owner-facing phrases for the feature's IN-PAGE surfaces --
	// elements woven into existing screens (a briefing line, a day-view link)
	// rather than whole screens, gated where they render via the console's
	// per-request features state rather than by a route prefix. Each phrase
	// joins the Settings card's "what disappears" line verbatim, so register a
	// surface's phrase in the same change that adds the surface.
	Surfaces []string
	// Skill is the client-side skill that documents the feature's tools, or ""
	// when it has none. install-hooks skips installing it while the feature is
	// off, so agents never read about tools they cannot call.
	Skill string
	// Default is the built-in value, mirroring config.Defaults(). Optional
	// features ship off.
	Default bool

	// get and set are the accessors for this feature's config.Features field.
	// They keep the registry entry the only place a new feature is wired up:
	// every consumer goes through Feature.Enabled / Feature.Set rather than a
	// switch that would drift as features are added.
	get func(config.Features) bool
	set func(*config.Features, bool)
}

// Enabled reports whether this feature is on in the given resolved features
// config.
func (f Feature) Enabled(c config.Features) bool {
	if f.get == nil {
		return f.Default
	}
	return f.get(c)
}

// Set writes this feature's state into c. The console settings form rebuilds the
// whole config.Features from checkbox presence through it.
func (f Feature) Set(c *config.Features, on bool) {
	if f.set == nil || c == nil {
		return
	}
	f.set(c, on)
}

// lineMax is the most runes a Feature.Line may hold: one line on a Settings
// row.
const lineMax = 80

// registry is the ordered feature list. Order is stable and is the order the
// Settings rows render in.
var registry = []Feature{
	{
		Key:           Research,
		Label:         "Research labs & trials",
		Blurb:         "Systematic investigations recorded as trials (expected vs actual), shared across agents working the same lab.",
		Line:          "Experiments with expected and actual results, shared across agents.",
		Tools:         []string{"lab_open", "trial_record", "trial_query"},
		NavIDs:        []string{"labs", "trials"},
		RoutePrefixes: []string{"/console/labs", "/console/trials"},
		Skill:         "seam-research",
		Default:       false,
		get:           func(c config.Features) bool { return c.Research },
		set:           func(c *config.Features, on bool) { c.Research = on },
	},
	{
		Key:           Screening,
		Label:         "Write screening",
		Blurb:         "Screens every agent write to memories and notes for secrets, injection markers, hidden characters, encoded blobs and unlisted links, and holds suspicious ones (with any change to a binding rule by a session that read outside text) out of briefings and recall until you release them. It also reviews the existing corpus on this machine, keeps files changed outside Seamless out of briefings until you accept them, and adds a lockdown switch.",
		Line:          "Holds a suspicious agent write until you release it.",
		NavIDs:        []string{"held"},
		RoutePrefixes: []string{"/console/held"},
		Surfaces: []string{
			"the owner-confirmed and agent-written marks on briefing and recall lines",
		},
		Default: false,
		get:     func(c config.Features) bool { return c.Screening },
		set:     func(c *config.Features, on bool) { c.Screening = on },
	},
	{
		Key:           RepoLocks,
		Label:         "Repo locks",
		Blurb:         "Locks on a repository's refs (its main branch, or a tag pattern) that agents declare once and hold while they land changes, so landings happen one at a time across machines.",
		Line:          "Agents take turns landing on a repository branch.",
		Tools:         []string{"locks_list", "locks_declare", "locks_acquire", "locks_release"},
		NavIDs:        []string{"locks"},
		RoutePrefixes: []string{"/console/locks"},
		Surfaces: []string{
			"the LOCK and live-peer lines in session-start briefings, and the prompt's lock-holder notices",
		},
		Default: false,
		get:     func(c config.Features) bool { return c.RepoLocks },
		set:     func(c *config.Features, on bool) { c.RepoLocks = on },
	},
	{
		Key:           Diary,
		Label:         "Diary",
		Blurb:         "A page for each day of work: what was worked on, which agents did it, and what it took in agent time and model tokens, with the meaningful work in the spotlight and short question-and-answer sessions folded away. While it is on, a Claude Code session's token usage is kept current as the session runs, read after a prompt is answered, at most once every three minutes.",
		Line:          "A page for each day: what was worked on, by whom, and what it cost.",
		NavIDs:        []string{"diary"},
		RoutePrefixes: []string{"/console/diary"},
		Surfaces: []string{
			"the Open in Diary link on a Sessions day view",
		},
		Default: false,
		get:     func(c config.Features) bool { return c.Diary },
		set:     func(c *config.Features, on bool) { c.Diary = on },
	},
}

// Registry returns the ordered optional features. The slice is a copy, so a
// caller cannot mutate the registry.
func Registry() []Feature { return slices.Clone(registry) }

// Get returns the feature with the given key. found is false for an unknown key.
func Get(key Key) (Feature, bool) {
	for _, f := range registry {
		if f.Key == key {
			return f, true
		}
	}
	return Feature{}, false
}

// Enabled reports whether the named feature is on. An unknown key is off: a
// caller asking about a feature that does not exist must not be told it is
// available.
func Enabled(c config.Features, key Key) bool {
	f, ok := Get(key)
	if !ok {
		return false
	}
	return f.Enabled(c)
}

// ToolOwners maps every registered optional tool name to the feature that owns
// it, regardless of whether that feature is on. Guard tests use it to assert no
// tool is claimed twice and that every name exists in the MCP catalog.
func ToolOwners() map[string]Key {
	owners := make(map[string]Key)
	for _, f := range registry {
		for _, tool := range f.Tools {
			owners[tool] = f.Key
		}
	}
	return owners
}

// HiddenTools returns the set of MCP tool names hidden by the currently disabled
// features. An empty result means every registered tool is exposed.
func HiddenTools(c config.Features) map[string]bool {
	hidden := make(map[string]bool)
	for _, f := range registry {
		if f.Enabled(c) {
			continue
		}
		for _, tool := range f.Tools {
			hidden[tool] = true
		}
	}
	return hidden
}

// Defaults returns the features config with every feature at its registry
// default. It mirrors config.Defaults().Features; a test asserts they agree.
func Defaults() config.Features {
	var c config.Features
	for _, f := range registry {
		f.Set(&c, f.Default)
	}
	return c
}
