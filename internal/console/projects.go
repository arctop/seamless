package console

import (
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// projectGroupKeys / projectSortKeys are the strict enums the board's toolbar
// GET params accept. An unknown value is a 400 (never a silent default): an
// agent driving the console by URL must see the fix, not a page that quietly
// ignored its param.
var (
	projectGroupKeys = []string{"family", "flat"}
	projectSortKeys  = []string{"recent", "coverage", "name", "favorites"}
)

// projectRowVM is one board row: a project's strict per-slug health (from
// store.ProjectsWithCounts) merged with its registry metadata (name, parent,
// retired) and a console-side family role. Global is the "" scope row; it is not
// a project and carries no detail link.
type projectRowVM struct {
	Slug         string
	Name         string
	Description  string
	ParentSlug   string
	Global       bool
	Parent       bool
	Child        bool
	Retired      bool
	Favorite     bool
	Unregistered bool
	Isolation    core.Isolation // agent-facing fence; drives the row's lock pill
	TileClass    string
	TileIcon     string
	Working      int // live (active) sessions
	Sessions     int
	OpenTasks    int
	Blocked      int
	Memories     int
	Notes        int
	Inherited    int // memories a child inherits from its parent's shared briefing
	ReachRate    int
	Surfaced     int
	Active       int
	HasReach     bool // the project has >=1 active memory (a reach denominator)
	LastActive   time.Time
	TokensTotal  int // real model tokens burned across the project's sessions
	// Stage is the momentum maturity glyph, pre-rendered ("" while the feature
	// is off -- zero trace in the row markup).
	Stage template.HTML
}

// projectGroupVM is a family band on the board: a labeled header plus its rows.
type projectGroupVM struct {
	Label        string
	Icon         string
	Count        int
	Note         string // e.g. "parent injects 6 memories into 2 children"
	Rows         []projectRowVM
	Working      int
	OpenTasks    int
	Blocked      int
	Memories     int
	Notes        int
	TokensTotal  int
	ReachRate    int
	HasReach     bool
	Unregistered int
}

// projectsData is the board page payload.
type projectsData struct {
	Group        string
	Sort         string
	Query        string
	Window       string
	WindowLabel  string           `json:"-"`
	Windows      []windowOption   `json:"-"`
	Groups       []projectGroupVM // family mode
	Flat         []projectRowVM   // flat mode
	Projects     int              // # project rows (excludes the global scope)
	Working      int              // total live sessions across visible scopes
	LiveScopes   int              // visible scopes with at least one live session
	Families     int              // # shared-briefing family bands
	Sessions     int
	OpenTasks    int
	Blocked      int
	Memories     int
	Notes        int
	TokensTotal  int
	Surfaced     int
	Active       int
	ReachRate    int
	HasReach     bool
	Attention    int // visible scopes with blocked work or registry drift
	Unregistered int
	Retired      int
	// Locked are the locked projects this request cannot read (peer_auth.
	// locked): name and counts only, aggregates taken on the main handle,
	// each linking to its unlock page. Empty while nothing is locked.
	Locked []store.ProjectTally `json:"locked,omitempty"`
}

func (s *Service) projectsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	group := r.URL.Query().Get("group")
	if group == "" {
		group = "family"
	}
	if !slices.Contains(projectGroupKeys, group) {
		s.badRequest(w, r, fmt.Sprintf("invalid group %q: valid values are %s",
			group, strings.Join(projectGroupKeys, ", ")))
		return
	}
	sortKey := r.URL.Query().Get("sort")
	if sortKey == "" {
		sortKey = "recent"
	}
	if !slices.Contains(projectSortKeys, sortKey) {
		s.badRequest(w, r, fmt.Sprintf("invalid sort %q: valid values are %s",
			sortKey, strings.Join(projectSortKeys, ", ")))
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	now := time.Now().UTC()
	win := store.ResolveRetrievalWindow(r.URL.Query().Get("w"), now)

	board, err := store.ProjectsWithCounts(ctx, s.db(ctx), win, now, s.cfg.SessionIdleTTL)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	projects, err := store.ListProjects(ctx, s.db(ctx))
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	meta := make(map[string]core.Project, len(projects))
	childrenOf := map[string][]string{}
	isParent := map[string]bool{}
	for _, p := range projects {
		meta[p.Slug] = p
		if p.ParentSlug != "" {
			childrenOf[p.ParentSlug] = append(childrenOf[p.ParentSlug], p.Slug)
			isParent[p.ParentSlug] = true
		}
	}

	// memoriesBySlug lets a child show how many memories it inherits from its
	// parent (the shared briefing injects the parent's active memories).
	memoriesBySlug := make(map[string]int, len(board))
	for _, b := range board {
		memoriesBySlug[b.Project] = b.Memories
	}

	vmBySlug := make(map[string]projectRowVM, len(board))
	var (
		rootRows       []projectRowVM // the global scope
		standaloneRows []projectRowVM
		retiredRows    []projectRowVM
		working        int
		projectCount   int
		liveScopes     int
		sessions       int
		openTasks      int
		blocked        int
		memories       int
		notes          int
		tokensTotal    int
		surfaced       int
		active         int
		attention      int
		unregistered   int
		retired        int
	)
	stages := s.projectStages(ctx, board, now)
	q := strings.ToLower(query)
	for _, b := range board {
		p := meta[b.Project]
		vm := newProjectRowVM(b, p, isParent[b.Project])
		if p.ParentSlug != "" {
			vm.Inherited = memoriesBySlug[p.ParentSlug]
		}
		if info, ok := stages[b.Project]; ok {
			vm.Stage = stageGlyph(info, now)
		}
		if !matchesQuery(vm, q) {
			continue
		}
		vmBySlug[b.Project] = vm
		working += vm.Working
		if vm.Working > 0 {
			liveScopes++
		}
		sessions += vm.Sessions
		openTasks += vm.OpenTasks
		blocked += vm.Blocked
		memories += vm.Memories
		notes += vm.Notes
		tokensTotal += vm.TokensTotal
		surfaced += vm.Surfaced
		active += vm.Active
		if projectNeedsAttention(vm) {
			attention++
		}
		if !vm.Global {
			projectCount++
			if vm.Unregistered {
				unregistered++
			}
			if vm.Retired {
				retired++
			}
		}
		switch {
		case vm.Global:
			rootRows = append(rootRows, vm)
		case vm.Retired:
			retiredRows = append(retiredRows, vm)
		case vm.Child, isParent[b.Project]:
			// placed into family bands below
		default:
			standaloneRows = append(standaloneRows, vm)
		}
	}

	less := projectLess(sortKey)
	data := projectsData{
		Group: group, Sort: sortKey, Query: query,
		Window: win.Key, WindowLabel: win.Label, Windows: windowOptions(win.Key),
		Projects: projectCount, Working: working, LiveScopes: liveScopes,
		Sessions: sessions, OpenTasks: openTasks, Blocked: blocked,
		Memories: memories, Notes: notes, TokensTotal: tokensTotal, Surfaced: surfaced, Active: active,
		HasReach: active > 0, ReachRate: percent(surfaced, active),
		Attention: attention, Unregistered: unregistered, Retired: retired,
	}
	if locked := lockedProjects(ctx); len(locked) > 0 {
		tallies, err := store.ProjectTallies(ctx, s.writeDB(), locked)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		for _, t := range tallies {
			if t.Registered && (q == "" || strings.Contains(strings.ToLower(t.Slug), q) || strings.Contains(strings.ToLower(t.Name), q)) {
				data.Locked = append(data.Locked, t)
			}
		}
	}
	for parent, children := range childrenOf {
		if _, ok := vmBySlug[parent]; ok {
			data.Families++
			continue
		}
		for _, child := range children {
			if _, ok := vmBySlug[child]; ok {
				data.Families++
				break
			}
		}
	}

	if group == "flat" {
		flat := make([]projectRowVM, 0, len(vmBySlug))
		for _, vm := range vmBySlug {
			vm.Child = false // no indentation in flat mode
			flat = append(flat, vm)
		}
		sort.SliceStable(flat, func(i, j int) bool { return less(flat[i], flat[j]) })
		data.Flat = flat
		s.render(w, r, "projects", pageData{Title: "Projects", Active: "projects", Data: data})
		return
	}

	// Family mode: Root (global) -> each shared-briefing family -> Standalone ->
	// Retired. A family band is a parent row followed by its indented children.
	var groups []projectGroupVM
	if len(rootRows) > 0 {
		sortRows(rootRows, less)
		groups = append(groups, newProjectGroupVM("Root", "server", "Shared context available to every project", rootRows))
	}

	parents := make([]string, 0, len(isParent))
	for slug := range isParent {
		parents = append(parents, slug)
	}
	sort.Strings(parents)
	// Order the family bands by their parent row under the active sort.
	sort.SliceStable(parents, func(i, j int) bool {
		return less(vmBySlug[parents[i]], vmBySlug[parents[j]])
	})
	for _, parent := range parents {
		var rows []projectRowVM
		if pv, ok := vmBySlug[parent]; ok {
			rows = append(rows, pv)
		}
		kids := make([]projectRowVM, 0, len(childrenOf[parent]))
		for _, cs := range childrenOf[parent] {
			if cv, ok := vmBySlug[cs]; ok {
				kids = append(kids, cv)
			}
		}
		sortRows(kids, less)
		rows = append(rows, kids...)
		if len(rows) == 0 {
			continue // whole family filtered out by the query
		}
		groups = append(groups, newProjectGroupVM(
			parent+" · shared briefing",
			"folder-tree",
			familyNote(memoriesBySlug[parent], len(kids)),
			rows,
		))
	}

	if len(standaloneRows) > 0 {
		sortRows(standaloneRows, less)
		groups = append(groups, newProjectGroupVM("Standalone", "box", "Independent project scopes", standaloneRows))
	}
	if len(retiredRows) > 0 {
		sortRows(retiredRows, less)
		groups = append(groups, newProjectGroupVM("Retired by split", "archive", "Historical scopes kept for lineage", retiredRows))
	}
	data.Groups = groups
	s.render(w, r, "projects", pageData{Title: "Projects", Active: "projects", Data: data})
}

// newProjectRowVM projects a board row + its registry metadata into a display
// row, assigning a family role and a role-based tile (language is not tracked,
// so the tile reflects structure: parent/global/retired/leaf).
func newProjectRowVM(b store.ProjectBoardRow, p core.Project, parent bool) projectRowVM {
	vm := projectRowVM{
		Slug:         b.Project,
		Name:         p.Name,
		Description:  p.Description,
		ParentSlug:   p.ParentSlug,
		Global:       b.Project == "",
		Parent:       parent,
		Child:        p.ParentSlug != "",
		Retired:      p.Retired(),
		Favorite:     p.Favorite,
		Unregistered: b.Unregistered,
		Isolation:    isolationOf(p),
		Working:      b.LiveSessions,
		Sessions:     b.Sessions,
		OpenTasks:    b.OpenTasks,
		Blocked:      b.Blocked,
		Memories:     b.Memories,
		Notes:        b.Notes,
		ReachRate:    b.ReachRate,
		Surfaced:     b.Surfaced,
		Active:       b.Active,
		HasReach:     b.Active > 0,
		LastActive:   b.LastActive,
		TokensTotal:  b.TokensTotal,
	}
	if vm.Name == "" {
		vm.Name = b.Project
	}
	switch {
	case vm.Global:
		vm.Name = "global"
		vm.TileClass, vm.TileIcon = "grp", "database"
	case vm.Retired:
		vm.TileClass, vm.TileIcon = "", "archive"
	case parent:
		vm.TileClass, vm.TileIcon = "grp", "folder-tree"
	default:
		vm.TileClass, vm.TileIcon = "", "box"
	}
	return vm
}

// newProjectGroupVM rolls the same strict project metrics up to a visible
// family band. Reach is recomputed from surfaced/active counts rather than
// averaging row percentages, so one tiny project cannot outweigh a large one.
func newProjectGroupVM(label, icon, note string, rows []projectRowVM) projectGroupVM {
	g := projectGroupVM{Label: label, Icon: icon, Count: len(rows), Note: note, Rows: rows}
	var surfaced, active int
	for _, row := range rows {
		g.Working += row.Working
		g.OpenTasks += row.OpenTasks
		g.Blocked += row.Blocked
		g.Memories += row.Memories
		g.Notes += row.Notes
		g.TokensTotal += row.TokensTotal
		surfaced += row.Surfaced
		active += row.Active
		if row.Unregistered {
			g.Unregistered++
		}
	}
	g.HasReach = active > 0
	g.ReachRate = percent(surfaced, active)
	return g
}

func projectNeedsAttention(row projectRowVM) bool {
	return !row.Retired && (row.Blocked > 0 || row.Unregistered)
}

// familyNote describes a shared-briefing band: the parent injects its active
// memories into each child.
func familyNote(parentMemories, children int) string {
	if children == 0 {
		return ""
	}
	return fmt.Sprintf("parent injects %s into %s",
		plural(parentMemories, "memory", "memories"), plural(children, "child", "children"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// projectLess builds the row comparator for a sort key: recent = most recent
// activity first, coverage = highest reach first, name = slug ascending,
// favorites = starred first (then most recent activity).
func projectLess(sortKey string) func(a, b projectRowVM) bool {
	switch sortKey {
	case "favorites":
		return func(a, b projectRowVM) bool {
			if a.Favorite != b.Favorite {
				return a.Favorite
			}
			if !a.LastActive.Equal(b.LastActive) {
				return a.LastActive.After(b.LastActive)
			}
			return a.Slug < b.Slug
		}
	case "coverage":
		return func(a, b projectRowVM) bool {
			if a.ReachRate != b.ReachRate {
				return a.ReachRate > b.ReachRate
			}
			return a.Slug < b.Slug
		}
	case "name":
		return func(a, b projectRowVM) bool { return a.Slug < b.Slug }
	default: // recent
		return func(a, b projectRowVM) bool {
			if !a.LastActive.Equal(b.LastActive) {
				return a.LastActive.After(b.LastActive)
			}
			return a.Slug < b.Slug
		}
	}
}

func sortRows(rows []projectRowVM, less func(a, b projectRowVM) bool) {
	sort.SliceStable(rows, func(i, j int) bool { return less(rows[i], rows[j]) })
}

// matchesQuery reports whether a row matches a lowercased free-text filter over
// its slug, name, and description. An empty query matches everything.
func matchesQuery(vm projectRowVM, q string) bool {
	if q == "" {
		return true
	}
	return strings.Contains(strings.ToLower(vm.Slug), q) ||
		strings.Contains(strings.ToLower(vm.Name), q) ||
		strings.Contains(strings.ToLower(vm.Description), q)
}
