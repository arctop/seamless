package console

// The library screens' shared state and controls (Memories and Notes): the
// URL state every link carries (libQuery), the choices a control offers
// (libLink), what a page draws beyond its list (libView), and the Project
// menu, whose choices the project switcher (plan:console-ux-pass UX 27,
// scope.go) orders and draws. Moved here from notes.go so the switcher and
// the item pages (UX 26) are written in separate files.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// libDefaultSort is both library screens' default ?sort.
const libDefaultSort = "recent"

// libQuery is a library screen's state as its URL carries it: the sort, the
// text filter, the grouping and the filters. Every link the page draws -- a
// rail item, a sort, a filter, the star's and the archive's way back -- is
// this state with at most one part changed, so no choice drops another.
type libQuery struct {
	Sort string // a sort key the screen accepts
	Q    string // the text filter, trimmed, as typed
	// Group is "project" for the project tree, "" for one list in the sort's
	// order (the default). Sorting by name always draws the tree.
	Group string
	// Kind is a core memory kind, "" for every kind (Memories only).
	Kind string
	// Project is "", "global" (the shared scope), or a project slug.
	Project string
	// Fav keeps only starred items.
	Fav bool
	// Surfaced is "never" to keep only memories that never reached an
	// agent, "" for all (Memories only).
	Surfaced string
}

// parseLibQuery reads a library screen's state from its URL. Every part is
// strict, as ?sort always was: a value the screen cannot read is an error
// naming the values it takes, never a quiet default. memories adds the two
// filters only the Memories screen has (kind, surfaced); Notes ignores them.
func parseLibQuery(v url.Values, sortKeys []string, memories bool) (libQuery, error) {
	q := libQuery{Sort: v.Get("sort"), Q: strings.TrimSpace(v.Get("q"))}
	if q.Sort == "" {
		q.Sort = libDefaultSort
	}
	if !slices.Contains(sortKeys, q.Sort) {
		return q, fmt.Errorf("invalid sort %q: valid values are %s", q.Sort, strings.Join(sortKeys, ", "))
	}
	switch g := v.Get("group"); g {
	case "", "none":
	case "project":
		q.Group = g
	default:
		return q, fmt.Errorf("invalid group %q: valid values are none, project", g)
	}
	if p := strings.TrimSpace(v.Get("project")); p != "" {
		slug, err := validate.Project(p)
		if err != nil {
			return q, fmt.Errorf("invalid project %q: name a project's slug, or global for the global scope", p)
		}
		q.Project = slug
		if slug == "" {
			q.Project = validate.GlobalProject
		}
	}
	switch f := v.Get("fav"); f {
	case "":
	case "1":
		q.Fav = true
	default:
		return q, fmt.Errorf("invalid fav %q: valid values are 1 (or omit)", f)
	}
	if !memories {
		return q, nil
	}
	if k := v.Get("kind"); k != "" {
		if !core.MemoryKind(k).Valid() {
			kinds := make([]string, 0, len(core.MemoryKinds))
			for _, mk := range core.MemoryKinds {
				kinds = append(kinds, string(mk))
			}
			return q, fmt.Errorf("invalid kind %q: valid values are %s", k, strings.Join(kinds, ", "))
		}
		q.Kind = k
	}
	switch sf := v.Get("surfaced"); sf {
	case "":
	case "never":
		q.Surfaced = sf
	default:
		return q, fmt.Errorf("invalid surfaced %q: valid values are never (or omit)", sf)
	}
	return q, nil
}

// parseLibRequest is parseLibQuery for a request; ok=false means the 400 was
// already written.
func (s *Service) parseLibRequest(w http.ResponseWriter, r *http.Request, sortKeys []string, memories bool) (libQuery, bool) {
	q, err := parseLibQuery(r.URL.Query(), sortKeys, memories)
	if err != nil {
		s.badRequest(w, r, err.Error())
		return libQuery{}, false
	}
	return q, true
}

// values is the state's non-default parts; forceSort names the sort even at
// its default (the sort menu's links say which sort they are).
func (q libQuery) values(forceSort bool) url.Values {
	v := url.Values{}
	if q.Q != "" {
		v.Set("q", q.Q)
	}
	if forceSort || q.Sort != libDefaultSort {
		v.Set("sort", q.Sort)
	}
	if q.Group != "" {
		v.Set("group", q.Group)
	}
	if q.Kind != "" {
		v.Set("kind", q.Kind)
	}
	if q.Project != "" {
		v.Set("project", q.Project)
	}
	if q.Fav {
		v.Set("fav", "1")
	}
	if q.Surfaced != "" {
		v.Set("surfaced", q.Surfaced)
	}
	return v
}

// QS is the state as a ?query suffix, "" when every part is at its default.
func (q libQuery) QS() string { return encodeQS(q.values(false)) }

// href is path with the state's query.
func (q libQuery) href(path string) string { return path + q.QS() }

func encodeQS(v url.Values) string {
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// tree reports whether the rail groups by project: asked for, or the Name
// sort, whose A to Z reads best inside each project.
func (q libQuery) tree() bool { return q.Sort == "name" || q.Group == "project" }

// narrowed reports a filter beyond the text one.
func (q libQuery) narrowed() bool {
	return q.Kind != "" || q.Project != "" || q.Fav || q.Surfaced != ""
}

// inScope reports whether an item in project passes the Project filter.
func (q libQuery) inScope(project string) bool {
	switch q.Project {
	case "":
		return true
	case validate.GlobalProject:
		return project == ""
	default:
		return project == q.Project
	}
}

// libLink is one choice a library control offers: a menu entry, a toggle.
type libLink struct {
	Label string
	Href  string
	Title string
	Count int
	On    bool
	// Value is the choice's own query value where the control sets one: the
	// Project menu's ?project= ("" for All projects, "global", or a slug).
	Value string
}

// libField is one hidden input the text filter's form carries, so typing
// keeps every other part of the state.
type libField struct{ Name, Value string }

// libView is what a library page draws beyond its list: its controls' links,
// the Project menu, and the words its header and rail use. HTML only.
type libView struct {
	// Flat is one list in the sort's order; else the project tree.
	Flat bool
	// AlwaysTree is the Name sort, which keeps the tree whatever the
	// grouping, so the Group control is not offered.
	AlwaysTree bool
	// Filtered is any filter, the text one included; Narrowed one beyond it.
	Filtered, Narrowed bool
	// OrderHint names the one list's order ("Newest first").
	OrderHint string
	// Sorts maps each sort key to its link, on the reader the page shows.
	Sorts map[string]string
	// GroupNone and GroupProject are the Group control's two links.
	GroupNone, GroupProject string
	// Projects is the Project menu, ProjectLabel its current choice.
	Projects     []libLink
	ProjectLabel string
	// Scope is the project switcher built from the Project menu
	// (projectSwitcher, scope.go); a page draws it in the menu's place.
	Scope projectSwitcher
	// FavLink is the Starred toggle.
	FavLink libLink
	// ClearHref drops every filter and keeps the sort and the grouping;
	// SearchHref looks for the text across everything, on Search.
	ClearHref, SearchHref string
	// Keep is the text filter's hidden inputs.
	Keep []libField
}

// libOrderHints name a flat rail's order, by sort key.
var libOrderHints = map[string]string{
	"recent": "Newest first", "reach": "Most surfaced first", "utility": "Highest utility first",
	"favorites": "Starred first",
}

// newLibView draws the controls for state q on a page whose list lives at
// list and whose reader shows base (list itself when nothing is open). A
// choice that changes what the list holds (a filter, Clear) goes to the list,
// which opens its first match; a choice that only reorders it (a sort, the
// grouping) stays on the item being read.
func newLibView(q libQuery, list, base string, sortKeys []string) libView {
	v := libView{
		Flat: !q.tree(), AlwaysTree: q.Sort == "name",
		Filtered: q.Q != "" || q.narrowed(), Narrowed: q.narrowed(),
		OrderHint: libOrderHints[q.Sort], Sorts: map[string]string{},
	}
	for _, key := range sortKeys {
		o := q
		o.Sort = key
		v.Sorts[key] = base + encodeQS(o.values(true))
	}
	none, proj := q, q
	none.Group, proj.Group = "", "project"
	v.GroupNone, v.GroupProject = none.href(base), proj.href(base)
	fav := q
	fav.Fav = !q.Fav
	v.FavLink = libLink{Label: "Starred", Href: fav.href(list), On: q.Fav}
	v.ClearHref = libQuery{Sort: q.Sort, Group: q.Group}.href(list)
	if q.Q != "" {
		v.SearchHref = "/console/search?q=" + url.QueryEscape(q.Q)
	}
	keep := q.values(false)
	keep.Del("q")
	names := make([]string, 0, len(keep))
	for name := range keep {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v.Keep = append(v.Keep, libField{Name: name, Value: keep.Get(name)})
	}
	return v
}

// libProjectMenu lists the Project filter's choices: every project, then each
// scope the screen holds (global first), with how many items match the other
// filters there. The chosen scope stays listed when nothing matches in it, so
// the menu always names the current choice. It returns the menu and that
// choice's label. Each entry's Value is its ?project= value; the project
// switcher (scope.go) orders and marks these same choices.
func libProjectMenu(q libQuery, counts map[string]int, names map[string]string, list, globalLabel string) ([]libLink, string) {
	all := q
	all.Project = ""
	menu := []libLink{{Label: "All projects", Href: all.href(list), On: q.Project == ""}}
	label := "All"
	chosen := q.Project
	if chosen == validate.GlobalProject {
		chosen = ""
	}
	scopes := make([]string, 0, len(counts)+1)
	for p := range counts {
		scopes = append(scopes, p)
	}
	if q.Project != "" && !slices.Contains(scopes, chosen) {
		scopes = append(scopes, chosen)
	}
	sortScopes(scopes)
	for _, p := range scopes {
		o := q
		o.Project = p
		lbl, title := p, names[p]
		if p == "" {
			o.Project, lbl, title = validate.GlobalProject, globalLabel, ""
		}
		if title == p {
			title = ""
		}
		on := q.Project != "" && p == chosen
		if on {
			label = lbl
		}
		menu = append(menu, libLink{Label: lbl, Href: o.href(list), Title: title, Count: counts[p], On: on, Value: o.Project})
	}
	return menu, label
}

// sortScopes orders project scopes: global ("") first, then by slug.
func sortScopes(scopes []string) {
	sort.Slice(scopes, func(i, j int) bool {
		if (scopes[i] == "") != (scopes[j] == "") {
			return scopes[i] == ""
		}
		return scopes[i] < scopes[j]
	})
}

// projectNames maps each project slug the request reads to its display name,
// for the text filter: typing a project's name narrows to it.
func (s *Service) projectNames(ctx context.Context) (map[string]string, error) {
	projects, err := store.ListProjects(ctx, s.db(ctx))
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(projects))
	for _, p := range projects {
		names[p.Slug] = p.Name
	}
	return names, nil
}

// scopeMatches reports whether a lowercased text filter names an item's
// project: its slug, its display name, or "global" for the shared scope.
func scopeMatches(project, name, q string) bool {
	if project == "" {
		return strings.Contains(validate.GlobalProject, q)
	}
	return strings.Contains(strings.ToLower(project), q) || (name != "" && strings.Contains(strings.ToLower(name), q))
}
