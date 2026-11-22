package console

// Global search: one query across every entity the console can link to.
//
// Two surfaces share this handler. The page (GET /console/search) runs the full
// fused semantic+FTS retrieval; the command palette fetches the same route with
// ?format=json&fast=1, which drops the semantic leg so a query per keystroke
// never costs a remote embedding round-trip. The JSON shape is the palette's
// contract and is CLI-visible, so its field names are stable surface.
//
// Coverage is deliberately partial. Memories and notes come through
// retrieve.Search (fused, snippeted); tasks, plans, trials, projects, and
// sessions have no FTS mirror and match by LIKE (store.Search*). Events are
// excluded because the telemetry stream has its own Interactions surface and
// would flood results. Superseded and archived memories, which every other
// search leaves out, come from their own console-only lookup
// (store.SearchRetiredMemories): by exact name or id always, by text with
// "Include archived".
//
// Counts are honest. Each kind is fetched one past what it shows, so a cut
// kind says "20+" and offers Show more (?more=<kind>) instead of passing the
// cap off as its total; the starred rule and the window run inside every
// query, before its cap, so "Starred only" cannot lose a starred match ranked
// past the cut, and under every view a starred match takes a slot before an
// unstarred one.

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
)

// searchScopes are the ?scope values in selector order: "all", the two
// knowledge kinds retrieve.Search understands, and one per structured entity.
// It is the full set; which of them a request may use comes from the screen
// registry, where each scope is owned by the screen that lists its entities
// (screen.Scope).
var searchScopes = []string{"all", "memories", "notes", "tasks", "plans", "trials", "projects", "sessions"}

// searchScopesFor returns the scopes offered at this level and feature state:
// "all" plus every scope whose owning screen is visible, in selector order.
//
// Two callers, two answers. At the top level it is the ACCEPTED enum, which is
// level-blind on purpose (hidden, not locked): a scope belonging to a disabled
// optional feature leaves it entirely, so ?scope=trials gets the same
// named-values 400 as any misspelling rather than silently searching
// everything, while a scope merely above the level still works by URL. At the
// request's own level it is what the page OFFERS: the selector's choices and
// what "all" covers on the HTML page. JSON callers always get the level-blind
// set, so a level never changes a JSON answer.
func searchScopesFor(cfg config.Features, lvl level) []string {
	out := make([]string, 0, len(searchScopes))
	for _, scope := range searchScopes {
		if scope == "all" {
			out = append(out, scope)
			continue
		}
		if sc, ok := scopeScreen(scope); ok && sc.visibleAt(cfg, lvl) {
			out = append(out, scope)
		}
	}
	return out
}

// searchWindowKeys / searchSortKeys are the strict URL enums for the search
// controls. Search defaults to all time so adding the window control does not
// silently hide results that the page exposed before it existed.
var (
	searchWindowKeys = []string{"24h", "7d", "30d", "1y", "all"}
	searchSortKeys   = []string{"relevance", "newest", "oldest", "project", "confidence"}
)

// searchQueryMax caps the query length. Longer input is truncated rather than
// rejected: a paste is a clumsy search, not an error.
const searchQueryMax = 200

// searchQueryMin is the shortest query worth running. It matches the floor in
// store's ftsQuery, which drops single-character tokens -- below this the
// lexical leg has nothing to match and the page would promise results it cannot
// deliver.
const searchQueryMin = 2

// Result depth per group: the page shows a full page of hits, the palette shows
// a preview it can render in a dropdown, and a kind whose Show more is open
// (?more=) shows up to searchMoreLimit. Each is fetched one past itself, which
// is how a group knows it was cut.
const (
	searchPageLimit = 20
	searchFastLimit = 5
	searchMoreLimit = 200
)

// searchRetiredKind is the group of superseded and archived memories: part of
// the memories scope, not a scope of its own, but a group with its own count
// and its own Show more.
const searchRetiredKind = "retired"

// searchAccents is the accent each group kind (and the scope that names it)
// wears: the singular kind its rows carry, which is what console.css keys
// --search-accent on (data-search-kind). A breakdown chip, a type pill, the
// palette's group head and every row of the kind then wear one color, so a
// chip reads as the legend for its rows. The retired memories wear the memory
// accent, muted by their data-retired mark: retired knowledge, not a new kind.
var searchAccents = map[string]string{
	"memories": "memory", searchRetiredKind: "memory", "notes": "note", "tasks": "task",
	"plans": "plan", "trials": "trial", "projects": "project", "sessions": "session",
}

// searchRow is one result, already resolved to the URL it links to. Href is
// always an in-console path; Peek marks the rows the detail pane can load as a
// fragment (every kind here can, but the flag keeps the template honest).
type searchRow struct {
	Kind        string                    `json:"kind"`
	ID          string                    `json:"id"`
	Title       string                    `json:"title"`
	Identifier  string                    `json:"identifier,omitempty"`
	MatchedID   string                    `json:"matchedId,omitempty"`
	MatchKind   store.IdentifierMatchKind `json:"matchKind,omitempty"`
	Project     string                    `json:"project,omitempty"`
	Age         string                    `json:"age"`
	Href        string                    `json:"href"`
	Description string                    `json:"description,omitempty"`
	SnippetHTML template.HTML             `json:"snippetHtml,omitempty"`
	Updated     time.Time                 `json:"updated"`
	Lexical     bool                      `json:"lexical,omitempty"`
	// Similarity is the semantic leg's cosine similarity as a percentage
	// (1-100), so the observer can see where relevance falls off. Zero for a
	// lexical-only hit -- an FTS match has a highlighted snippet instead of a
	// distance -- and for the structured-entity groups, which match by LIKE.
	Similarity int    `json:"similarity,omitempty"`
	ScoreTone  string `json:"-"`
	Favorite   bool   `json:"favorite,omitempty"`
	// Account is the restricted local account (peer_auth.restricted) that
	// wrote the item -- a memory or note's current content, a task it
	// created, a trial it recorded, a session it started -- for the "written
	// by account" chip; "" for the owner's items, which render as before.
	Account string `json:"account,omitempty"`
	// MayContain is the item's provenance stamps (console stamp.go): the
	// hit's for a memory or a note, the index's for a task or a session.
	MayContain []core.Stamp `json:"mayContain,omitempty"`
	Peek       bool         `json:"peek"`
	// Retired is a superseded or archived memory's status ("superseded",
	// "archived"); "" for everything live.
	Retired string `json:"retired,omitempty"`
	// SupersededBy is the memory that replaced a superseded one, when this
	// request may read it.
	SupersededBy *searchRef `json:"supersededBy,omitempty"`
}

// searchRef is a link to another item.
type searchRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Href string `json:"href"`
}

// searchGroup is one entity kind's results.
type searchGroup struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	// Accent is the singular kind the group's rows carry (searchAccents): the
	// data-search-kind its chip and the palette's group head wear.
	Accent string `json:"accent"`
	// Retired marks the superseded and archived memories' group, which wears
	// the memory accent muted.
	Retired bool `json:"retired,omitempty"`
	// Scope is the search scope the group belongs to: its own kind, except
	// the retired memories, which are memories.
	Scope string `json:"scope"`
	// Count is the number of Rows. With More set it is a floor, never the
	// total: the page shows "20+".
	Count int `json:"count"`
	// More reports that the kind has more matches than it shows.
	More bool `json:"more,omitempty"`
	// MoreHref is the console page that shows more of the kind: for the
	// palette, Search narrowed to it; on the page, this page with the kind's
	// Show more open. "" when no page would show more.
	MoreHref string      `json:"moreHref,omitempty"`
	Rows     []searchRow `json:"rows"`
	// Capped says, on the page, that the kind's Show more is open and still
	// cut: how many it shows and that a narrower search shows the rest.
	Capped string `json:"-"`
}

// searchData is the page/JSON payload.
type searchData struct {
	Query       string `json:"query"`
	Scope       string `json:"scope"`
	Sort        string `json:"sort"`
	Window      string `json:"window"`
	WindowLabel string `json:"windowLabel"`
	Fast        bool   `json:"fast"`
	Fav         bool   `json:"favoritesOnly"`
	// Archived is the "Include archived" toggle (?archived=1): superseded and
	// archived memories found by text, not only by exact name.
	Archived bool `json:"includeArchived"`
	// Expanded are the kinds whose Show more is open (?more=), in group order.
	Expanded []string      `json:"expanded,omitempty"`
	Groups   []searchGroup `json:"groups"`
	Total    int           `json:"total"`
	// Partial reports that some group was cut: Total is then a floor.
	Partial bool `json:"partial,omitempty"`
	// TooShort reports a query below searchQueryMin: nothing was searched.
	TooShort bool        `json:"tooShort,omitempty"`
	Rows     []searchRow `json:"-"` // unified, globally sorted page projection
	Since    time.Time   `json:"-"`
	// TrialsScope reports whether the research feature exposes the trials
	// scope: while it is off, not even ?scope=all may reach past it.
	TrialsScope bool `json:"-"`
	// Offered is what "all" covers for this response: every accepted scope for
	// a JSON caller (level-blind), the level's visible scopes on the page.
	Offered []string `json:"-"`
	// ScopeNoun names the offered kinds as prose ("memories, notes, and
	// sessions") for the page copy.
	ScopeNoun string               `json:"-"`
	Scopes    []searchScope        `json:"-"`
	Windows   []searchWindowOption `json:"-"`
	Sorts     []searchSortOption   `json:"-"`
	// The page's toggles: each href is this page with the toggle flipped.
	StarredHref     string `json:"-"`
	ArchivedHref    string `json:"-"`
	ArchivedOffered bool   `json:"-"` // memories are in scope, so the toggle changes something
	MoreParam       string `json:"-"` // Expanded as the ?more= value
	// The empty state: each active restriction as a chip that removes it,
	// one link that removes them all (with two or more), and, when archived
	// memories match what nothing live does, a line that includes them.
	Chips       []searchChip `json:"-"`
	ClearHref   string       `json:"-"`
	RetiredHint string       `json:"-"`
}

// searchChip is one active restriction on an empty page: what it is, and the
// page without it. Accent is the kind's accent for a scope chip, "" otherwise.
type searchChip struct {
	Label  string
	Href   string
	Accent string
}

// searchLink is the state a search URL carries. href writes it in one fixed
// order -- scope, w, sort, the toggles, more, then q -- so every control on
// the page builds the same URL for the same state.
type searchLink struct {
	q, scope, window, sort string
	fav, archived          bool
	more                   []string
}

func (l searchLink) href() string {
	var b strings.Builder
	b.WriteString("/console/search?scope=" + searchEscape(l.scope))
	b.WriteString("&w=" + searchEscape(l.window))
	b.WriteString("&sort=" + searchEscape(l.sort))
	if l.fav {
		b.WriteString("&fav=1")
	}
	if l.archived {
		b.WriteString("&archived=1")
	}
	if len(l.more) > 0 {
		b.WriteString("&more=" + searchEscape(strings.Join(l.more, ",")))
	}
	b.WriteString("&q=" + searchEscape(l.q))
	return b.String()
}

// searchEscape escapes a query value with a space as %20, the way the page's
// links always wrote it (QueryEscape's "+" renders as &#43; in an attribute).
// A literal "+" is already %2B by then.
func searchEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// link is the state this page carries.
func (d searchData) link() searchLink {
	return searchLink{q: d.Query, scope: d.Scope, window: d.Window, sort: d.Sort,
		fav: d.Fav, archived: d.Archived, more: d.Expanded}
}

// limitFor is a group's cap: searchMoreLimit once its Show more is open,
// base otherwise.
func (d searchData) limitFor(kind string, base int) int {
	if slices.Contains(d.Expanded, kind) {
		return searchMoreLimit
	}
	return base
}

// starred is the starred rule every query of this search runs under.
func (d searchData) starred() store.SearchStarred {
	if d.Fav {
		return store.StarredOnly
	}
	return store.StarredFirst
}

// highlightSnippet renders an FTS snippet as safe HTML with the matched terms
// wrapped in <mark>.
//
// The order below is the whole security argument and must not be reversed:
// escape the raw item text FIRST, so any markup a writer put in their own body
// becomes inert entities, and only THEN substitute the sentinels for real tags.
// Escaping second would escape our own <mark>; substituting first would let an
// item's body smuggle live markup through. The sentinels are control characters
// (store.SnippetStartMark/EndMark) precisely so this substitution cannot collide
// with anything HTMLEscapeString emits -- an attacker who embeds a literal
// sentinel in their body gets a stray, inert <mark>, never an injection.
func highlightSnippet(raw string) template.HTML {
	esc := template.HTMLEscapeString(raw)
	esc = strings.ReplaceAll(esc, store.SnippetStartMark, "<mark>")
	esc = strings.ReplaceAll(esc, store.SnippetEndMark, "</mark>")
	return template.HTML(esc)
}

// wants reports whether scope selects the given kind: "all" selects every
// offered kind, a named scope selects itself.
func (d searchData) wants(kind string) bool {
	if d.Scope == "all" {
		return slices.Contains(d.Offered, kind)
	}
	return d.Scope == kind
}

var searchParamNames = []string{"q", "scope", "w", "sort", "fast", "fav", "archived", "more", "format"}

// searchGroupKinds are the kinds a ?more= may name: every accepted scope but
// "all", with the retired memories beside memories, in group order.
func searchGroupKinds(accepted []string) []string {
	var out []string
	for _, k := range accepted {
		if k == "all" {
			continue
		}
		out = append(out, k)
		if k == "memories" {
			out = append(out, searchRetiredKind)
		}
	}
	return out
}

// searchMoreParam parses ?more=, the kinds whose Show more is open: a
// comma-separated list of group kinds. Absent is none; an empty, unknown, or
// repeated kind is refused like every other control. The answer is in group
// order, so one state has one URL.
func searchMoreParam(values url.Values, kinds []string) ([]string, error) {
	vals, ok := values["more"]
	if !ok {
		return nil, nil
	}
	var out []string
	for _, k := range strings.Split(vals[0], ",") {
		if !slices.Contains(kinds, k) {
			return nil, fmt.Errorf("invalid more %q: valid values are %s, comma-separated", vals[0], strings.Join(kinds, ", "))
		}
		if slices.Contains(out, k) {
			return nil, fmt.Errorf("invalid more %q: %s is named twice", vals[0], k)
		}
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b string) int { return slices.Index(kinds, a) - slices.Index(kinds, b) })
	return out, nil
}

// searchFlagParam reads a toggle that is "1" or absent.
func searchFlagParam(values url.Values, key string) (bool, error) {
	vals, ok := values[key]
	if !ok {
		return false, nil
	}
	if vals[0] != "1" {
		return false, fmt.Errorf("invalid %s %q: valid values are 1 (or omit)", key, vals[0])
	}
	return true, nil
}

// validateSearchParams rejects misspelled and ambiguous query parameters. The
// search controls are bookmarkable API inputs: silently ignoring ?srot=project
// would render a plausible relevance-ordered page that the caller cannot tell
// from success.
func validateSearchParams(values url.Values) error {
	for key, vals := range values {
		if !slices.Contains(searchParamNames, key) {
			return fmt.Errorf("invalid parameter %q: valid parameters are %s", key, strings.Join(searchParamNames, ", "))
		}
		if len(vals) != 1 {
			return fmt.Errorf("parameter %q must be provided exactly once", key)
		}
	}
	if format, ok := values["format"]; ok && format[0] != "json" {
		return fmt.Errorf("invalid format %q: valid value is json (or omit)", format[0])
	}
	return nil
}

// searchEnumParam applies a default only when the key is absent. An explicitly
// empty or unknown value is an error, matching the console's no-silent-fallback
// boundary contract.
func searchEnumParam(values url.Values, key, fallback string, valid []string) (string, error) {
	vals, ok := values[key]
	if !ok {
		return fallback, nil
	}
	value := vals[0]
	if !slices.Contains(valid, value) {
		return "", fmt.Errorf("invalid %s %q: valid values are %s", key, value, strings.Join(valid, ", "))
	}
	return value, nil
}

func (s *Service) searchPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	values := r.URL.Query()
	if err := validateSearchParams(values); err != nil {
		s.badRequest(w, r, err.Error())
		return
	}
	q := strings.TrimSpace(values.Get("q"))
	if n := []rune(q); len(n) > searchQueryMax {
		q = strings.TrimSpace(string(n[:searchQueryMax]))
	}
	feats := s.effectiveFeatures(ctx)
	// The accepted enum is level-blind (see searchScopesFor); the level only
	// decides what the page offers and what "all" covers there.
	accepted := searchScopesFor(feats, levelAdvanced)
	scope, err := searchEnumParam(values, "scope", "all", accepted)
	if err != nil {
		s.badRequest(w, r, err.Error())
		return
	}
	offered, lvl := accepted, levelAdvanced
	if !wantsJSON(r) {
		lvl = s.consoleLevel(ctx).Level
		offered = searchScopesFor(feats, lvl)
	}
	sortKey, err := searchEnumParam(values, "sort", "relevance", searchSortKeys)
	if err != nil {
		s.badRequest(w, r, err.Error())
		return
	}
	windowKey, err := searchEnumParam(values, "w", "all", searchWindowKeys)
	if err != nil {
		s.badRequest(w, r, err.Error())
		return
	}
	fast, err := searchFlagParam(values, "fast")
	if err != nil {
		s.badRequest(w, r, err.Error())
		return
	}
	fav, err := searchFlagParam(values, "fav")
	if err != nil {
		s.badRequest(w, r, err.Error())
		return
	}
	archived, err := searchFlagParam(values, "archived")
	if err != nil {
		s.badRequest(w, r, err.Error())
		return
	}
	expanded, err := searchMoreParam(values, searchGroupKinds(accepted))
	if err != nil {
		s.badRequest(w, r, err.Error())
		return
	}
	window := resolveSearchWindow(windowKey, time.Now().UTC())

	data := searchData{
		Query: q, Scope: scope, Sort: sortKey,
		Window: window.Key, WindowLabel: window.Label, Since: window.Since,
		Fast: fast, Fav: fav, Archived: archived, Expanded: expanded,
		TrialsScope: slices.Contains(accepted, "trials"),
		Offered:     offered, ScopeNoun: searchScopeNoun(offered),
		Sorts: searchSortOptions(sortKey),
	}
	data.decorate()
	// A scope above the level still searches (hidden, not locked), under the
	// banner that names its screen.
	pd := pageData{Title: "Search", Active: "search", Data: data}
	if sc, ok := scopeScreen(scope); ok {
		pd.LevelBanner = levelBannerFor(sc.ID, lvl)
	}
	if q == "" {
		s.render(w, r, "search", pd)
		return
	}
	if len([]rune(q)) < searchQueryMin {
		// Too short to match anything: say so, rather than run a query that
		// would promise results it cannot find, or report "nothing matches".
		data.TooShort = true
		pd.Data = data
		s.render(w, r, "search", pd)
		return
	}

	limit := searchPageLimit
	if fast {
		limit = searchFastLimit
	}
	// The queries run sequentially on purpose: the pool is SetMaxOpenConns(1),
	// so fanning out over goroutines would serialize on the connection anyway
	// while adding failure modes.
	groups, err := s.searchGroups(ctx, data, limit)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data.Groups = groups
	for i := range data.Groups {
		g := &data.Groups[i]
		data.Total += g.Count
		data.Partial = data.Partial || g.More
		g.MoreHref = data.moreHref(*g)
		if g.More && g.MoreHref == "" && !data.Fast {
			g.Capped = fmt.Sprintf("%s: showing the first %d. Narrow the search to see the rest.", g.Label, g.Count)
		}
		data.Rows = append(data.Rows, g.Rows...)
	}
	sortSearchRows(data.Rows, data.Sort)
	if len(data.Rows) == 0 && !data.Fast && !wantsJSON(r) {
		if err := s.searchEmptyState(ctx, &data); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	pd.Data = data
	s.render(w, r, "search", pd)
}

// decorate fills in the page's control hrefs from its state.
func (d *searchData) decorate() {
	d.Scopes = searchScopeOptions(d.Scope, d.Offered)
	for i := range d.Scopes {
		l := d.link()
		l.scope = d.Scopes[i].Key
		d.Scopes[i].Href = l.href()
	}
	d.Windows = searchWindowOptions(d.Window)
	for i := range d.Windows {
		l := d.link()
		l.window = d.Windows[i].Key
		d.Windows[i].Href = l.href()
	}
	l := d.link()
	l.fav = !d.Fav
	d.StarredHref = l.href()
	l = d.link()
	l.archived = !d.Archived
	d.ArchivedHref = l.href()
	d.ArchivedOffered = d.wants("memories")
	d.MoreParam = strings.Join(d.Expanded, ",")
}

// moreHref is where a cut group's "more" goes: for the palette (fast), Search
// narrowed to the group's scope -- archived memories included, for theirs --
// and on the page, this page with the group's Show more opened too. "" when
// the group was not cut, or its Show more is already open: no link that
// changes nothing.
func (d searchData) moreHref(g searchGroup) string {
	if !g.More {
		return ""
	}
	if d.Fast {
		return searchLink{q: d.Query, scope: g.Scope, window: d.Window, sort: d.Sort,
			fav: d.Fav, archived: d.Archived || g.Kind == searchRetiredKind}.href()
	}
	if slices.Contains(d.Expanded, g.Kind) {
		return ""
	}
	l := d.link()
	kinds := searchGroupKinds(searchScopes)
	l.more = append(slices.Clone(d.Expanded), g.Kind)
	slices.SortFunc(l.more, func(a, b string) int { return slices.Index(kinds, a) - slices.Index(kinds, b) })
	return l.href()
}

// searchEmptyState explains a page with no results: each active restriction
// (the scope, the window, Starred only) as a chip that removes it, one link
// that removes them all, and -- when archived memories match where nothing
// live does -- a line that includes them. Every link changes the search.
func (s *Service) searchEmptyState(ctx context.Context, d *searchData) error {
	all := d.link()
	all.scope, all.window, all.fav = "all", "all", false
	if d.Scope != "all" {
		l := d.link()
		l.scope = "all"
		d.Chips = append(d.Chips, searchChip{Label: searchScopeLabel(d.Scope), Href: l.href(), Accent: searchAccents[d.Scope]})
	}
	if d.Window != "all" {
		l := d.link()
		l.window = "all"
		d.Chips = append(d.Chips, searchChip{Label: "Updated in the " + d.WindowLabel, Href: l.href()})
	}
	if d.Fav {
		l := d.link()
		l.fav = false
		d.Chips = append(d.Chips, searchChip{Label: "Starred only", Href: l.href()})
	}
	if len(d.Chips) > 1 {
		d.ClearHref = all.href()
	}
	if d.Archived || !d.wants("memories") {
		return nil
	}
	searchCtx, _ := s.searchReach(ctx)
	limit := d.limitFor(searchRetiredKind, searchPageLimit)
	retired, err := store.SearchRetiredMemories(searchCtx, s.db(ctx), d.Query,
		store.SearchFilter{Since: d.Since, Starred: d.starred()}, false, limit+1)
	if err != nil {
		return err
	}
	switch n := len(retired); {
	case n > limit:
		d.RetiredHint = fmt.Sprintf("%d+ archived or superseded memories match.", limit)
	case n > 1:
		d.RetiredHint = fmt.Sprintf("%d archived or superseded memories match.", n)
	case n == 1:
		d.RetiredHint = "1 archived or superseded memory matches."
	}
	return nil
}

// searchReach is the context the knowledge reads run under, and the projects
// their answers must still drop. The fused search reads the main handle and
// filters it by the request's View, which hides every locked project. A
// browser's console unlock opens some (it is a console cookie, never part of
// the View), so for that browser the reads run with no View, and their hits
// are filtered by exactly what this request's handle hides: inside the fused
// search, before each kind's cap (retrieve.SearchInput.Exclude), so a locked
// project's matches no longer take a kind's slots. They can still fill the
// candidate legs' depth: at least 60 per kind on the page, the cap itself in
// the palette (see Exclude). The retired memories need no list: they read the
// request's handle, where the hidden projects do not exist.
func (s *Service) searchReach(ctx context.Context) (context.Context, []string) {
	if rc := reachOf(ctx); rc != nil && len(rc.unlocked) > 0 && s.cfg.Views != nil {
		return access.WithView(ctx, access.View{}), s.cfg.Views.Hidden(rc.open)
	}
	return ctx, nil
}

// searchGroups runs each in-scope entity query and returns the non-empty groups
// in display order. Each kind is fetched one past its cap, and cut there.
func (s *Service) searchGroups(ctx context.Context, data searchData, limit int) ([]searchGroup, error) {
	var groups []searchGroup
	add := func(kind, label, scope string, rows []searchRow, shown int) {
		if len(rows) == 0 {
			return
		}
		g := searchGroup{Kind: kind, Label: label, Scope: scope, Accent: searchAccents[kind], Retired: kind == searchRetiredKind}
		if len(rows) > shown {
			rows, g.More = rows[:shown], true
		}
		g.Rows, g.Count = rows, len(rows)
		groups = append(groups, g)
	}
	filter := store.SearchFilter{Since: data.Since, Starred: data.starred()}
	searchCtx, hidden := s.searchReach(ctx)

	// Memories + notes: one retrieval, each kind ranked and capped on its own
	// so neither crowds the other out of its count.
	var notes []searchRow
	noteLimit := data.limitFor("notes", limit)
	if s.cfg.Retrieve != nil && (data.wants("memories") || data.wants("notes")) {
		scope := "all"
		if data.Scope == "memories" || data.Scope == "notes" {
			scope = data.Scope
		}
		memLimit := data.limitFor("memories", limit)
		hits, err := s.cfg.Retrieve.Search(searchCtx, retrieve.SearchInput{
			Query: data.Query, Scope: scope, Semantic: !data.Fast, Since: data.Since,
			PerKind: map[string]int{core.ItemKindMemory: memLimit + 1, core.ItemKindNote: noteLimit + 1},
			Starred: data.starred(), Exclude: hidden,
		})
		if err != nil {
			return nil, err
		}
		var mems []searchRow
		for _, h := range hits {
			// A belt: Exclude already left the hidden projects out, before
			// the cap.
			if slices.Contains(hidden, h.Project) {
				continue
			}
			row := hitRow(h)
			if h.Kind == "note" {
				notes = append(notes, row)
			} else {
				mems = append(mems, row)
			}
		}
		add("memories", "Memories", "memories", mems, memLimit)
	}

	// Superseded and archived memories: by exact name or id on every search,
	// by text too with Include archived. The request's handle leaves out what
	// it may not read.
	if data.wants("memories") {
		retiredLimit := data.limitFor(searchRetiredKind, limit)
		retired, err := store.SearchRetiredMemories(searchCtx, s.db(ctx), data.Query, filter, !data.Archived, retiredLimit+1)
		if err != nil {
			return nil, err
		}
		rows, err := s.retiredRows(ctx, retired)
		if err != nil {
			return nil, err
		}
		add(searchRetiredKind, "Archived & superseded", "memories", rows, retiredLimit)
	}
	add("notes", "Notes", "notes", notes, noteLimit)

	if data.wants("tasks") {
		taskLimit := data.limitFor("tasks", limit)
		tasks, err := store.SearchTasksFiltered(ctx, s.db(ctx), data.Query, filter, taskLimit+1)
		if err != nil {
			return nil, err
		}
		rows := make([]searchRow, 0, len(tasks))
		ids := make([]string, len(tasks))
		for i, t := range tasks {
			ids[i] = t.ID
		}
		stamps := s.stampsByID(ctx, core.ItemKindTask, ids)
		for _, t := range tasks {
			row := searchRow{
				Kind: "task", ID: t.ID, Title: t.Title, Project: t.ProjectSlug,
				Age: ago(t.UpdatedAt), Href: "/console/tasks/" + t.ID,
				Description: string(t.Status), Updated: t.UpdatedAt,
				Favorite: t.Favorite, Account: t.Account, Lexical: true, Peek: true,
				MayContain: core.StampsShown(stamps[t.ID], t.ProjectSlug),
			}
			row.MatchKind = store.IDIdentifierMatch(data.Query, t.ID)
			if row.MatchKind != "" {
				row.MatchedID = t.ID
			}
			rows = append(rows, row)
		}
		add("tasks", "Tasks", "tasks", rows, taskLimit)
	}

	if data.wants("plans") {
		planLimit := data.limitFor("plans", limit)
		plans, err := store.SearchPlansFiltered(ctx, s.db(ctx), data.Query, filter, planLimit+1)
		if err != nil {
			return nil, err
		}
		rows := make([]searchRow, 0, len(plans))
		for _, p := range plans {
			row := searchRow{
				Kind: "plan", ID: p.Slug, Title: p.Title, Project: p.Project,
				Identifier: p.Slug,
				Age:        ago(p.Updated), Updated: p.Updated, Href: "/console/plans/" + p.Slug,
				Favorite: p.Favorite, Account: p.Account, Lexical: true, Peek: true,
			}
			row.MatchKind = store.NaturalIdentifierMatch(data.Query, p.Slug)
			rows = append(rows, row)
		}
		add("plans", "Plans", "plans", rows, planLimit)
	}

	// Trials belong to the research feature: while it is off the scope is not
	// even in the accepted enum, and ?scope=all must not reach past it either.
	if data.TrialsScope && data.wants("trials") {
		trialLimit := data.limitFor("trials", limit)
		trials, err := store.SearchTrialsFiltered(ctx, s.db(ctx), data.Query, filter, trialLimit+1)
		if err != nil {
			return nil, err
		}
		rows := make([]searchRow, 0, len(trials))
		for _, tr := range trials {
			desc := "lab " + tr.Lab
			if tr.Outcome != "" {
				desc += " · " + string(tr.Outcome)
			}
			row := searchRow{
				Kind: "trial", ID: tr.ID, Title: tr.Title, Project: tr.ProjectSlug,
				Age: ago(tr.CreatedAt), Href: "/console/trials/" + tr.ID,
				Description: desc, Updated: tr.CreatedAt,
				Favorite: tr.Favorite, Account: tr.Account, Lexical: true, Peek: true,
			}
			row.MatchKind = store.IDIdentifierMatch(data.Query, tr.ID)
			if row.MatchKind != "" {
				row.MatchedID = tr.ID
			}
			rows = append(rows, row)
		}
		add("trials", "Trials", "trials", rows, trialLimit)
	}

	if data.wants("projects") {
		projectLimit := data.limitFor("projects", limit)
		projects, err := store.SearchProjectsFiltered(ctx, s.db(ctx), data.Query, filter, projectLimit+1)
		if err != nil {
			return nil, err
		}
		rows := make([]searchRow, 0, len(projects))
		for _, p := range projects {
			row := searchRow{
				Kind: "project", ID: p.Slug, Title: p.Slug, Project: p.Slug, Age: ago(p.UpdatedAt),
				Identifier: p.Slug,
				Href:       "/console/projects/" + p.Slug, Description: p.Name,
				Updated: p.UpdatedAt, Favorite: p.Favorite, Lexical: true, Peek: true,
			}
			row.MatchKind = store.NaturalIdentifierMatch(data.Query, p.Slug)
			rows = append(rows, row)
		}
		add("projects", "Projects", "projects", rows, projectLimit)
	}

	if data.wants("sessions") {
		sessionLimit := data.limitFor("sessions", limit)
		sessions, err := store.SearchSessionsFiltered(ctx, s.db(ctx), data.Query, filter, sessionLimit+1)
		if err != nil {
			return nil, err
		}
		rows := make([]searchRow, 0, len(sessions))
		ids := make([]string, len(sessions))
		for i, sess := range sessions {
			ids[i] = sess.ID
		}
		stamps := s.stampsByID(ctx, core.ItemKindSession, ids)
		for _, sess := range sessions {
			row := searchRow{
				Kind: "session", ID: sess.ID, Title: sess.Name, Project: sess.ProjectSlug,
				Age: ago(sess.UpdatedAt), Href: "/console/sessions/" + sess.ID,
				Description: string(sess.Status), Updated: sess.UpdatedAt,
				Favorite: sess.Favorite, Account: sess.Account, Lexical: true, Peek: true,
				MayContain: core.StampsShown(stamps[sess.ID], sess.ProjectSlug),
			}
			row.MatchKind = store.IDIdentifierMatch(data.Query, sess.ID)
			if row.MatchKind != "" {
				row.MatchedID = sess.ID
			}
			rows = append(rows, row)
		}
		add("sessions", "Sessions", "sessions", rows, sessionLimit)
	}

	promoteIdentifierGroups(groups)
	return groups, nil
}

// retiredRows projects retired memories into result rows, each with its
// status and, for a superseded one, the memory that replaced it when this
// request may read it.
func (s *Service) retiredRows(ctx context.Context, hits []store.RetiredMemoryHit) ([]searchRow, error) {
	if len(hits) == 0 {
		return nil, nil
	}
	ids := make([]string, len(hits))
	var successors []string
	for i, h := range hits {
		ids[i] = h.Memory.ID
		if h.Memory.SupersededBy != "" {
			successors = append(successors, h.Memory.SupersededBy)
		}
	}
	next, err := store.MemoriesByIDs(ctx, s.db(ctx), successors)
	if err != nil {
		return nil, err
	}
	stamps := s.stampsByID(ctx, core.ItemKindMemory, ids)
	rows := make([]searchRow, 0, len(hits))
	for _, h := range hits {
		m := h.Memory
		row := searchRow{
			Kind: "memory", ID: m.ID, Title: m.Name, Identifier: m.Name, Project: m.Project,
			Age: ago(m.Updated), Href: "/console/memories/" + m.ID, Description: m.Description,
			Updated: m.Updated, Favorite: m.Favorite, Account: m.Account, Peek: true,
			MatchKind:  h.Match,
			MayContain: core.StampsShown(stamps[m.ID], m.Project),
			Retired:    "archived",
		}
		if m.SupersededBy != "" {
			row.Retired = "superseded"
			if succ, ok := next[m.SupersededBy]; ok {
				row.SupersededBy = &searchRef{ID: succ.ID, Name: succ.Name, Href: "/console/memories/" + succ.ID}
			}
		}
		if h.Match == store.IdentifierMatchExactID || h.Match == store.IdentifierMatchIDPrefix {
			row.MatchedID = m.ID
		}
		if h.Snippet != "" {
			row.SnippetHTML = highlightSnippet(h.Snippet)
		}
		row.Lexical = h.Snippet != "" || h.Match != ""
		rows = append(rows, row)
	}
	return rows, nil
}

// hitRow projects a retrieval hit into a result row: the snippet when the
// lexical leg quoted a match, the item's own description otherwise (a
// semantic-only hit has no matched term to quote).
func hitRow(h retrieve.Hit) searchRow {
	row := searchRow{
		Kind: h.Kind, ID: h.ID, Title: h.Title, Project: h.Project,
		Identifier: h.Name, MatchKind: h.IdentifierMatch,
		Age: h.Age, Description: h.Description, Updated: h.Updated,
		Favorite: h.Favorite, Account: h.Account, MayContain: h.MayContain, Peek: true,
	}
	if h.IdentifierMatch == store.IdentifierMatchExactID || h.IdentifierMatch == store.IdentifierMatchIDPrefix {
		row.MatchedID = h.ID
	}
	if h.Similarity > 0 {
		row.Similarity = min(int(h.Similarity*100+0.5), 100)
		row.ScoreTone = similarityTone(row.Similarity)
	}
	if h.Kind == "note" {
		row.Href = "/console/notes/" + h.ID
	} else {
		row.Href = "/console/memories/" + h.ID
	}
	if h.Snippet != "" {
		row.SnippetHTML = highlightSnippet(h.Snippet)
		row.Lexical = true
	}
	if h.IdentifierMatch != "" {
		row.Lexical = true
	}
	return row
}

// promoteIdentifierGroups keeps the palette's grouped JSON shape while moving
// the group containing the strongest identifier match first. Rows within every
// group are already identifier-ranked by their store/retrieval source.
func promoteIdentifierGroups(groups []searchGroup) {
	priority := func(group searchGroup) int {
		best := store.IdentifierMatchPriority("")
		for _, row := range group.Rows {
			best = min(best, store.IdentifierMatchPriority(row.MatchKind))
		}
		return best
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return priority(groups[i]) < priority(groups[j])
	})
}

func similarityTone(score int) string {
	switch {
	case score >= 70:
		return "strong"
	case score >= 45:
		return "good"
	default:
		return "related"
	}
}

// sortSearchRows orders the unified page projection. Relevance preserves the
// grouped retrieval order -- knowledge is fused-ranked, the structured stores
// provide their own deterministic defaults -- except that starred rows float
// first (a stable partition, so fused order holds within both halves).
func sortSearchRows(rows []searchRow, sortKey string) {
	if sortKey == "relevance" {
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Favorite != rows[j].Favorite {
				return rows[i].Favorite
			}
			return store.IdentifierMatchPriority(rows[i].MatchKind) < store.IdentifierMatchPriority(rows[j].MatchKind)
		})
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch sortKey {
		case "newest":
			return a.Updated.After(b.Updated)
		case "oldest":
			return a.Updated.Before(b.Updated)
		case "project":
			ap, bp := strings.ToLower(a.Project), strings.ToLower(b.Project)
			if ap != bp {
				return ap < bp // global (empty) sorts first
			}
			if !a.Updated.Equal(b.Updated) {
				return a.Updated.After(b.Updated)
			}
			return strings.ToLower(a.Title) < strings.ToLower(b.Title)
		case "confidence":
			aScored, bScored := a.Similarity > 0, b.Similarity > 0
			if aScored != bScored {
				return aScored
			}
			if a.Similarity != b.Similarity {
				return a.Similarity > b.Similarity
			}
			if a.Lexical != b.Lexical {
				return a.Lexical
			}
			return false
		default:
			return false // validated at the HTTP boundary
		}
	})
}

type searchWindow struct {
	Key   string
	Label string
	Since time.Time
}

type searchWindowOption struct {
	Key    string
	Label  string
	Active bool
	Href   string // this page in that window (searchData.decorate)
}

func resolveSearchWindow(key string, now time.Time) searchWindow {
	switch key {
	case "24h":
		return searchWindow{Key: key, Label: "past 24 hours", Since: now.Add(-24 * time.Hour)}
	case "7d":
		return searchWindow{Key: key, Label: "past 7 days", Since: now.AddDate(0, 0, -7)}
	case "30d":
		return searchWindow{Key: key, Label: "past 30 days", Since: now.AddDate(0, 0, -30)}
	case "1y":
		return searchWindow{Key: key, Label: "past year", Since: now.AddDate(-1, 0, 0)}
	default:
		return searchWindow{Key: "all", Label: "all time"}
	}
}

func searchWindowOptions(active string) []searchWindowOption {
	labels := map[string]string{"24h": "24h", "7d": "7d", "30d": "30d", "1y": "1y", "all": "All time"}
	out := make([]searchWindowOption, 0, len(searchWindowKeys))
	for _, key := range searchWindowKeys {
		out = append(out, searchWindowOption{Key: key, Label: labels[key], Active: key == active})
	}
	return out
}

type searchSortOption struct {
	Key      string
	Label    string
	Selected bool
}

func searchSortOptions(active string) []searchSortOption {
	labels := map[string]string{
		"relevance": "Relevance", "newest": "Newest first", "oldest": "Oldest first",
		"project": "Project", "confidence": "Confidence",
	}
	out := make([]searchSortOption, 0, len(searchSortKeys))
	for _, key := range searchSortKeys {
		out = append(out, searchSortOption{Key: key, Label: labels[key], Selected: key == active})
	}
	return out
}

// searchScope is one entry in the page's scope selector.
type searchScope struct {
	Key    string
	Label  string
	Accent string // the kind's accent (searchAccents); "" for "all"
	Active bool
	Href   string // this page in that scope (searchData.decorate)
}

// searchScopeOptions builds the ordered selector entries from the scopes the
// page offers (searchScopesFor), flagging the active one. Labels are the owning
// screens' names, except "all".
func searchScopeOptions(active string, scopes []string) []searchScope {
	// A scope requested by URL that the level does not offer still shows as the
	// active choice, in its usual place, so the selector never hides what the
	// page is searching.
	if !slices.Contains(scopes, active) {
		scopes = slices.DeleteFunc(slices.Clone(searchScopes), func(k string) bool {
			return k != active && !slices.Contains(scopes, k)
		})
	}
	out := make([]searchScope, 0, len(scopes))
	for _, k := range scopes {
		out = append(out, searchScope{Key: k, Label: searchScopeLabel(k), Accent: searchAccents[k], Active: k == active})
	}
	return out
}

// searchScopeLabel names a scope the way its screen is named.
func searchScopeLabel(scope string) string {
	if sc, ok := scopeScreen(scope); ok {
		return sc.Label
	}
	if scope == "all" {
		return "All"
	}
	return scope
}

// searchScopeNoun renders the offered kinds as lowercase prose for the page
// copy: "memories, notes, and sessions".
func searchScopeNoun(scopes []string) string {
	var kinds []string
	for _, k := range scopes {
		if k != "all" {
			kinds = append(kinds, strings.ToLower(searchScopeLabel(k)))
		}
	}
	return joinWithAnd(kinds)
}
