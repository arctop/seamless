package console

// The screen registry: the ONE list of console screens every navigation surface
// derives from -- the sidebar and the phone drawer (layout.html ranges over it),
// the g-chords and the ? sheet (shell.js reads the rendered links), the
// palette's Jump to (search.js, likewise), the level banner, and the docs
// matrix. A new screen or nav entry is a registry entry with a minimum level;
// nothing else hand-maintains a list of screens.
//
// Levels REMOVE; they never rename or rearrange what remains. The registry
// order is the sidebar order at every level, and a screen's label is the same
// everywhere it appears. The g-chord keys belong to screens, not to
// positions: moving a screen never moves its key.

import (
	"context"
	"net/url"
	"slices"
	"strings"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/features"
)

// screen is one console screen.
type screen struct {
	// ID is the stable identifier: the pageData.Active key that highlights the
	// nav entry, and the id features.NavIDs name.
	ID string
	// Label is the owner-facing name, identical at every level.
	Label string
	// Icon is the lucide glyph the nav entry carries.
	Icon string
	// Href is where the screen lives.
	Href string
	// Group is the sidebar section heading; empty for the entries pinned below
	// the groups (Settings) and for screens with no nav row.
	Group string
	// Key is the g-chord (g then Key) that opens the screen.
	Key string
	// Min is the lowest level whose sidebar shows the screen. A screen is
	// reachable by URL at every level regardless.
	Min level
	// Feature is the optional feature owning the screen ("" = core). A screen
	// is visible iff its feature is on AND the level is at least Min.
	Feature features.Key
	// Badge names the navCounts field the entry's count shows ("" = none:
	// an inventory total is no badge), BadgeKind says what that number is
	// (every badge has one), and BadgeTitle is the count's tooltip.
	Badge      string
	BadgeKind  badgeKind
	BadgeTitle string
	// Class is an extra class on the nav link (Settings' nav-settings).
	Class string
	// NavRow is false for screens that are real pages with no sidebar entry
	// (Search, Context): the palette and the docs matrix still know them.
	NavRow bool
	// Hint is the palette's one-line description for a screen with no nav row.
	Hint string
	// Scope is the search scope this screen owns ("" = none): the scope is
	// offered wherever the screen is visible (see searchScopesFor).
	Scope string
}

// badgeKind is what a sidebar number means (plan:console-ux-pass UX 13). A
// number that never asks for anything trains the eye to ignore the ones that
// do, so the sidebar shows two kinds and no totals.
type badgeKind string

const (
	// badgeQueue is something waiting on the owner: drawn as a pill while it
	// is non-zero and not at all at zero. Any queue the sidebar shows lights
	// the collapsed rail's dot on its entry and the phone bar's menu button
	// (navSections.Waiting).
	badgeQueue badgeKind = "queue"
	// badgePulse is a live reading (the agents live now): drawn quietly at
	// every value, dimmed at zero, and never an attention mark.
	badgePulse badgeKind = "pulse"
)

// Sidebar groups, in the owner's job order (plan:console-ux-pass UX 14, the
// owner's decision of 2026-11-21): what is happening, the work being followed
// (the owner's main job, so it comes before Knowledge), what the fleet knows
// and how it circulates, the research domain, and this machine. The Access
// group (groupAccess, below) goes ahead of all of them whenever it exists.
const (
	groupLive      = "Live"
	groupWork      = "Work"
	groupKnowledge = "Knowledge"
	groupResearch  = "Research"
	// groupServer is this machine's own picture: the parts of Seamless and
	// what agents can reach.
	groupServer = "Server"
)

// screens is the ordered registry: the sidebar order at every level.
var screens = []screen{
	{ID: "overview", Label: "Overview", Icon: "layout-dashboard", Href: "/console/", Group: groupLive, Key: "o",
		Min: levelBasic, NavRow: true},
	{ID: "now", Label: "Now", Icon: "radar", Href: "/console/now", Group: groupLive, Key: "n",
		Min: levelStandard, NavRow: true, Badge: "now", BadgeKind: badgePulse, BadgeTitle: "Agents live now"},
	{ID: "interactions", Label: "Interactions", Icon: "activity", Href: "/console/interactions", Group: groupLive, Key: "i",
		Min: levelAdvanced, NavRow: true},
	// The Mishaps screen (mishaps.go): Basic, like the Overview's mishap
	// item, and a queue of the reports since the owner last marked them
	// reviewed -- the item's own count (unreviewedMishaps).
	{ID: "mishaps", Label: "Mishaps", Icon: "triangle-alert", Href: mishapsPath, Group: groupLive, Key: "f",
		Min: levelBasic, NavRow: true, Badge: "mishaps", BadgeKind: badgeQueue,
		BadgeTitle: "Mishaps reported since you last marked them reviewed"},
	// The Diary (diary.go): one entry per day of work. Basic, and the Work
	// group's first row, because "what happened today" is the first question
	// an owner who turns it on asks.
	{ID: "diary", Label: "Diary", Icon: "book-open", Href: "/console/diary", Group: groupWork, Key: "d",
		Min: levelBasic, Feature: features.Diary, NavRow: true},
	// Projects, Plans and Tasks are Basic: following plans and tasks is the
	// owner's main job, and a new installation starts at Basic.
	{ID: "projects", Label: "Projects", Icon: "table-2", Href: "/console/projects", Group: groupWork, Key: "w",
		Min: levelBasic, NavRow: true, Scope: "projects"},
	{ID: "plans", Label: "Plans", Icon: "map", Href: "/console/plans", Group: groupWork, Key: "p",
		Min: levelBasic, NavRow: true, Badge: "plans", BadgeKind: badgeQueue, BadgeTitle: "Plans awaiting your approval",
		Scope: "plans"},
	{ID: "tasks", Label: "Tasks", Icon: "list-checks", Href: "/console/tasks", Group: groupWork, Key: "t",
		Min: levelBasic, NavRow: true, Badge: "tasks", BadgeKind: badgeQueue,
		BadgeTitle: "Gates waiting on you, lapsed claims and tasks in progress with no claim", Scope: "tasks"},
	{ID: "sessions", Label: "Sessions", Icon: "terminal", Href: "/console/sessions", Group: groupWork, Key: "s",
		Min: levelBasic, NavRow: true, Scope: "sessions"},
	// The repo locks (locks.go): every lock on a repository's refs, and the
	// declarations waiting for the owner.
	{ID: "locks", Label: "Locks", Icon: "git-branch", Href: "/console/locks", Group: groupWork, Key: "b",
		Min: levelStandard, Feature: features.RepoLocks, NavRow: true, Badge: "locks", BadgeKind: badgeQueue,
		BadgeTitle: "Lock declarations waiting for you"},
	{ID: "memories", Label: "Memories", Icon: "database", Href: "/console/memories", Group: groupKnowledge, Key: "m",
		Min: levelBasic, NavRow: true, Scope: "memories"},
	{ID: "notes", Label: "Notes", Icon: "file-text", Href: "/console/notes", Group: groupKnowledge, Key: "e",
		Min: levelBasic, NavRow: true, Scope: "notes"},
	{ID: "retrieval", Label: "Retrieval", Icon: "brain", Href: "/console/retrieval", Group: groupKnowledge, Key: "r",
		Min: levelAdvanced, NavRow: true},
	{ID: "gardener", Label: "Gardener", Icon: "sprout", Href: "/console/gardener", Group: groupKnowledge, Key: "g",
		Min: levelBasic, NavRow: true, Badge: "proposals", BadgeKind: badgeQueue, BadgeTitle: "Proposals waiting for your review"},
	// Basic, because an owner who turns write screening on at any level must
	// be able to reach what it holds.
	{ID: "held", Label: "Held", Icon: "lock", Href: "/console/held", Group: groupKnowledge, Key: "h",
		Min: levelBasic, Feature: features.Screening, NavRow: true, Badge: "held", BadgeKind: badgeQueue,
		BadgeTitle: "Writes held for your review"},
	{ID: "labs", Label: "Labs", Icon: "flask-conical", Href: "/console/labs", Group: groupResearch, Key: "l",
		Min: levelStandard, Feature: features.Research, NavRow: true},
	{ID: "trials", Label: "Trials", Icon: "test-tube", Href: "/console/trials", Group: groupResearch, Key: "x",
		Min: levelStandard, Feature: features.Research, NavRow: true, Scope: "trials"},
	{ID: "system", Label: "System", Icon: "orbit", Href: "/console/system", Group: groupServer, Key: "y",
		Min: levelStandard, NavRow: true},
	{ID: "privacy", Label: "Privacy", Icon: "shield", Href: "/console/privacy", Group: groupServer, Key: "v",
		Min: levelStandard, NavRow: true},
	{ID: "settings", Label: "Settings", Icon: "settings", Href: "/console/settings", Key: ",",
		Min: levelBasic, NavRow: true, Class: "nav-settings"},
	// Real pages with no sidebar row.
	{ID: "search", Label: "Search", Icon: "search", Href: "/console/search",
		Min: levelBasic, Hint: "every filter and sort"},
	{ID: "context", Label: "Context", Icon: "share-2", Href: "/console/context",
		Min: levelAdvanced, Hint: "briefing topology across projects"},
}

// groupAccess is the sidebar section of the lock-only screens.
const groupAccess = "Access"

// lockScreens are the screens that exist only while peer_auth locks a project
// (Config.Approvals set): the access requests agents file at the lock, with
// the live leases and console unlocks, and the approver credentials that
// decide them. They are deliberately not in screens: a console with nothing
// locked has no such pages, so its sidebar, palette and docs matrix
// (LevelMatrix) never name them. withLockScreens places them, at every level,
// as the sidebar's first group: a waiting request is a blocked agent.
var lockScreens = []screen{
	{ID: "access", Label: "Access requests", Icon: "lock", Href: "/console/access", Group: groupAccess, Key: "a",
		Min: levelBasic, NavRow: true, Badge: "access", BadgeKind: badgeQueue, BadgeTitle: "Access requests waiting for you"},
	{ID: "approvers", Label: "Approvers", Icon: "key-round", Href: "/console/approvers", Group: groupAccess, Key: "k",
		Min: levelBasic, NavRow: true},
}

// withLockScreens returns visible with the lock-only screens inserted ahead of
// the first group, so Access is the sidebar's first section, or visible
// itself when locked is false.
func withLockScreens(visible []screen, locked bool) []screen {
	if !locked {
		return visible
	}
	return slices.Concat(lockScreens, visible)
}

// withAccessScreens is withLockScreens when a project is locked; otherwise,
// with read grants on (isolation.requests) and no lock, it places only the
// Access requests screen, first as well -- there are no approver credentials
// to manage without peer_auth. Neither: visible itself.
func withAccessScreens(visible []screen, locked, grants bool) []screen {
	switch {
	case locked:
		return withLockScreens(visible, true)
	case grants:
		return slices.Concat(lockScreens[:1], visible)
	}
	return visible
}

// beforeTail inserts extra ahead of the pinned tail (the first nav row with
// no group: Settings).
func beforeTail(visible, extra []screen) []screen {
	at := len(visible)
	for i, sc := range visible {
		if sc.NavRow && sc.Group == "" {
			at = i
			break
		}
	}
	return slices.Concat(visible[:at:at], extra, visible[at:])
}

// offeredScreens is the sidebar's screen list for one request: the screens
// the feature state and level make visible, with the Held screen while
// restricted accounts' proposals wait there, the Devices screen on a server
// that pairs devices, and the access screens while a project is locked or
// read grants are on. The sidebar is drawn from it (withChrome), and the
// Overview offers a link only into a screen on it (linkOffer).
func (s *Service) offeredScreens(feats config.Features, lvl level) []screen {
	return withAccessScreens(withDevicesScreen(s.withProposalScreen(visibleScreens(feats, lvl), feats, lvl),
		s.cfg.Devices != nil), s.cfg.Approvals != nil, s.cfg.Grants != nil)
}

// linkOffer answers, for one request, whether a console link is offered: a
// link into a screen the sidebar hides at this level, or into a Settings
// section the Settings menu hides, is not (plan:console-ux-pass UX 12). Such
// a screen still opens by URL, under the level banner; a page simply does not
// send the owner there. A path no screen owns (an event page) is offered at
// every level.
type linkOffer struct {
	visible []screen
	lvl     level
}

// linkOfferFor is the request's linkOffer.
func (s *Service) linkOfferFor(ctx context.Context) linkOffer {
	lvl := s.consoleLevel(ctx).Level
	return linkOffer{visible: s.offeredScreens(s.effectiveFeatures(ctx), lvl), lvl: lvl}
}

// knownScreens is every screen a console path can belong to: the registry,
// the lock-only screens and the Devices screen.
func knownScreens() []screen { return slices.Concat(screens, lockScreens, []screen{devicesScreen}) }

// offers reports whether href is a link this request offers. Anything but a
// console path is not.
func (o linkOffer) offers(href string) bool {
	u, err := url.Parse(href)
	if err != nil || !strings.HasPrefix(u.Path, "/console/") {
		return false
	}
	sc, ok := screenForPath(knownScreens(), u.Path)
	if !ok {
		return true
	}
	if !slices.ContainsFunc(o.visible, func(v screen) bool { return v.ID == sc.ID }) {
		return false
	}
	if sc.ID == "settings" {
		if sec, found := settingsSectionByID(u.Query().Get("s")); found && o.lvl < sec.Min {
			return false
		}
	}
	return true
}

// screenRegistry returns the ordered screens. The slice is a copy, so a caller
// cannot mutate the registry.
func screenRegistry() []screen { return slices.Clone(screens) }

// screenByID returns the registry entry for id; found is false for an unknown id.
func screenByID(id string) (screen, bool) {
	for _, sc := range screens {
		if sc.ID == id {
			return sc, true
		}
	}
	return screen{}, false
}

// visibleAt reports whether the screen is offered at this level with this
// feature state: its feature is on (or it has none) AND the level reaches Min.
func (sc screen) visibleAt(feats config.Features, lvl level) bool {
	if sc.Feature != "" && !features.Enabled(feats, sc.Feature) {
		return false
	}
	return lvl >= sc.Min
}

// visibleScreens lists the screens offered at this level and feature state, in
// registry order.
func visibleScreens(feats config.Features, lvl level) []screen {
	out := make([]screen, 0, len(screens))
	for _, sc := range screens {
		if sc.visibleAt(feats, lvl) {
			out = append(out, sc)
		}
	}
	return out
}

// scopeScreen returns the screen that owns a search scope; found is false for
// "all" (every screen's) and for an unknown scope.
func scopeScreen(scope string) (screen, bool) {
	for _, sc := range screens {
		if sc.Scope != "" && sc.Scope == scope {
			return sc, true
		}
	}
	return screen{}, false
}

// navEntry is one rendered sidebar link: the screen plus this request's facts.
type navEntry struct {
	screen
	// Count is the badge number (meaningful only when Badge is set).
	Count int
	// Shown says the badge is drawn: a queue while something waits, a pulse
	// at every value.
	Shown bool
	// LinkClass is the link's full class attribute value.
	LinkClass string
}

// waiting reports a queue with something in it.
func (e navEntry) waiting() bool { return e.BadgeKind == badgeQueue && e.Count > 0 }

// navGroup is one sidebar section: a heading and its entries. The pinned tail
// (Settings) is the one group with no heading: the sidebar draws it below
// the scrolling sections, so it never scrolls away.
type navGroup struct {
	Label   string
	Pinned  bool
	Entries []navEntry
}

// navSections is the sidebar's sections, in order.
type navSections []navGroup

// Waiting counts the queues on the sidebar with something in them: non-zero,
// it marks the nav (data-waiting), which lights the phone bar's menu button.
// A queue the sidebar does not offer at this level or feature state is not
// on it, so it lights nothing.
func (ns navSections) Waiting() int {
	n := 0
	for _, g := range ns {
		for _, e := range g.Entries {
			if e.waiting() {
				n++
			}
		}
	}
	return n
}

// navGroups lays the visible screens with a nav row out as sidebar sections,
// in registry order, marking the active entry and filling each badge.
func navGroups(visible []screen, active string, counts navCounts) navSections {
	var out navSections
	for _, sc := range visible {
		if !sc.NavRow {
			continue
		}
		class := sc.Class
		if sc.ID == active {
			if class != "" {
				class += " "
			}
			class += "active"
		}
		entry := navEntry{screen: sc, LinkClass: class}
		if sc.Badge != "" {
			entry.Count, _ = counts.count(sc.Badge)
			entry.Shown = sc.BadgeKind == badgePulse || entry.Count > 0
		}
		if len(out) == 0 || out[len(out)-1].Label != sc.Group {
			out = append(out, navGroup{Label: sc.Group, Pinned: sc.Group == ""})
		}
		last := &out[len(out)-1]
		last.Entries = append(last.Entries, entry)
	}
	return out
}

// count maps a registry Badge name to its navCounts field. found is false for a
// name no field answers -- a guard test holds every registry Badge to it, so the
// template never has to know the field list.
func (n navCounts) count(badge string) (int, bool) {
	switch badge {
	case "now":
		return n.Now, true
	case "tasks":
		return n.Tasks, true
	case "proposals":
		return n.Proposals, true
	case "plans":
		return n.Plans, true
	case "held":
		return n.Held, true
	case "access":
		return n.Access, true
	case "locks":
		return n.Locks, true
	case "mishaps":
		return n.Mishaps, true
	default:
		return 0, false
	}
}

// surface is an in-page gate: an element woven into a screen a lower level
// shows, hidden below Min. Every {{if $.Level.AtLeast "..."}} in a template is
// one surface here -- register the phrase in the same change that adds the
// gate, exactly like features.Surfaces -- so the Experience cards and the docs
// matrix can say what each level adds without anyone transcribing it. A guard
// test holds each template's gates to its registered surfaces.
type surface struct {
	// Where names the page the surface lives on, as the owner reads it -- a
	// Settings surface names its section ("Settings › Features").
	Where string
	// Template is the file whose gate hides it (the guard's key).
	Template string
	// Label is the owner-facing phrase, read after Where: "Overview: the vitals".
	Label string
	// Min is the lowest level that shows it.
	Min level
}

// surfaces is the in-page gate registry, grouped by page in sidebar order.
var surfaces = []surface{
	{Where: "Overview", Template: "overview.html", Label: "the observation window", Min: levelStandard},
	{Where: "Overview", Template: "overview.html", Label: "the vitals", Min: levelStandard},
	{Where: "Overview", Template: "overview.html", Label: "the projects table and the knowledge rail", Min: levelStandard},
	{Where: "Sessions", Template: "sessions.html", Label: "the Retained filter", Min: levelStandard},
	{Where: "Sessions", Template: "sessions.html", Label: "the Sort menu", Min: levelStandard},
	{Where: "Sessions", Template: "session.html", Label: "a session's review signals", Min: levelStandard},
	{Where: "Memories", Template: "memories.html", Label: "the Reach and Utility sorts", Min: levelStandard},
	{Where: "Memories", Template: "memories.html", Label: "how often each memory surfaced", Min: levelStandard},
	{Where: "Memories", Template: "memories.html", Label: "the Never surfaced filter", Min: levelStandard},
	{Where: "Memories", Template: "memories.html", Label: "the utility score in the reader", Min: levelStandard},
	{Where: "Gardener", Template: "gardener.html", Label: "the Type and Source filters", Min: levelStandard},
	{Where: "Gardener", Template: "gardener.html", Label: "the project-split example", Min: levelStandard},
	{Where: "Gardener", Template: "gardener.html", Label: "the Hidden forever list", Min: levelStandard},
	{Where: "Gardener", Template: "gardener.html", Label: "Retarget on a proposal", Min: levelStandard},
	{Where: "Gardener", Template: "gardener.html", Label: "Hide forever on a proposal", Min: levelStandard},
	{Where: "Search", Template: "search.html", Label: "the Updated window", Min: levelStandard},
	{Where: "Search", Template: "search.html", Label: "the Sort control", Min: levelStandard},
	{Where: "Settings \u203a Features", Template: "settings.html", Label: "the precedence line", Min: levelStandard},
	{Where: "Settings \u203a Features", Template: "settings.html", Label: "the agent tool names", Min: levelStandard},
	{Where: "Settings \u203a Workspaces", Template: "settings.html", Label: "each repo route's host", Min: levelAdvanced},
	{Where: "Settings \u203a Workspaces", Template: "settings.html", Label: "the unbound repo routes", Min: levelAdvanced},
	{Where: "Event pages", Template: "peek_event.html", Label: "the decoded payload fields", Min: levelAdvanced},
	{Where: "Event pages", Template: "peek_event.html", Label: "the raw payload", Min: levelAdvanced},
}

// surfacesAt lists the surfaces whose minimum level is exactly lvl.
func surfacesAt(lvl level) []surface {
	var out []surface
	for _, sf := range surfaces {
		if sf.Min == lvl {
			out = append(out, sf)
		}
	}
	return out
}
