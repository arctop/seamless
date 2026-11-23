package console

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// projectGroupKeys / projectSortKeys / projectShowKeys are the strict enums
// the board's toolbar GET params accept. An unknown value is a 400 (never a
// silent default): an agent driving the console by URL must see the fix, not a
// page that quietly ignored its param. The first key of each is its default:
// the Activity view, and every project shown (?show=attention narrows the
// board to the projects that need attention).
var (
	projectGroupKeys = []string{"activity", "family", "flat"}
	projectSortKeys  = []string{"recent", "coverage", "name", "favorites"}
	projectShowKeys  = []string{"all", "attention"}
)

// projectQuietAfter is how long a project goes with no live agent and no
// session or task touched before the Activity view counts it Quiet.
const projectQuietAfter = 30 * 24 * time.Hour

// planStalledAfter is how long a plan with a step still open goes with no
// step changing before the board counts it stalled rather than moving (the
// plan note's Stalled state: nothing moved for 7 days). A live claim's
// heartbeat changes its step, so a held plan is moving.
const planStalledAfter = 7 * 24 * time.Hour

// projectRowVM is one board row: a project's strict per-slug health (from
// store.ProjectsWithCounts) merged with its registry metadata (name, parent,
// retired) and a console-side family role. Global is the "" scope's row: it is
// not a project, so the board draws it as the shared-context line and never as
// a row in a grouping. A Locked row is a project this request cannot read: its
// name, its counts and LastActive (store.ProjectTally) and nothing else, and it
// links to its unlock page.
type projectRowVM struct {
	Slug         string
	Name         string
	Description  string
	ParentSlug   string
	Global       bool
	Parent       bool
	Child        bool
	Indent       bool `json:"-"` // drawn under its parent in a family band
	Retired      bool
	Favorite     bool
	Unregistered bool
	Locked       bool           `json:",omitempty"`
	Isolation    core.Isolation // agent-facing fence; drives the row's lock pill
	TileClass    string
	TileIcon     string
	Working      int // live (active) sessions
	Sessions     int
	OpenTasks    int
	// The open work in the Tasks page's words (store.TaskCounts): Blocked
	// waits on unfinished work, Waiting on people; InProgress is held by a
	// live claim, Lapsed and Unclaimed are in progress with no live claim.
	Ready      int
	InProgress int
	Lapsed     int
	Unclaimed  int
	Blocked    int
	Waiting    int
	GatesOpen  int
	// PlansActive are its plans with a step still open; PlansMoving those a
	// step changed in within planStalledAfter; PlansAwaiting its captured
	// plans presented and awaiting approval.
	PlansActive   int
	PlansMoving   int
	PlansAwaiting int
	// Attention: something here waits on the owner or is stuck
	// (projectNeedsAttention). Needs says what, Work is the row's work line;
	// both are drawn from the counts above, which a JSON caller reads instead.
	Attention bool
	Needs     []projectBit `json:"-"`
	Work      []projectBit `json:"-"`
	Memories  int
	Notes     int
	Inherited int // memories a child inherits from its parent's shared briefing
	ReachRate int
	Surfaced  int
	Active    int
	HasReach  bool // the project has >=1 active memory (a reach denominator)
	// LastActive is the project's last activity: its latest session update or
	// task change, whichever is later. A locked row's is the tally's latest
	// session start or end.
	LastActive  time.Time
	TokensTotal int // real model tokens burned across the project's sessions
	// Stage is the momentum maturity glyph, pre-rendered ("" while the feature
	// is off -- zero trace in the row markup).
	Stage template.HTML
}

// projectBit is one phrase of a row's work line or of what it needs: Key names
// it for the page (data-word), Tone is a status tone class, Hint its title.
type projectBit struct {
	Key  string
	Text string
	Tone string
	Hint string
}

// projectGroupVM is a band on the board: a labeled header plus its rows. Key
// names it in the page (id="pg-<key>"), so a live refresh keeps a fold the
// owner opened. A Folded band is a <details>, closed unless Open: Quiet and
// Retired fold, and open when a filter matches a project inside them.
type projectGroupVM struct {
	Key          string
	Label        string
	Icon         string
	Count        int
	Note         string // e.g. "parent injects 6 memories into 2 children"
	Folded       bool
	Open         bool
	Rows         []projectRowVM
	Working      int
	OpenTasks    int
	Blocked      int
	Waiting      int
	PlansMoving  int
	Attention    int // rows that need attention
	Memories     int
	Notes        int
	TokensTotal  int
	ReachRate    int
	HasReach     bool
	Unregistered int
}

// projectGlobalVM is the shared-context line under the summary tiles: the
// global ("") scope's knowledge, which every project's briefing draws on.
type projectGlobalVM struct {
	Memories  int
	Notes     int
	ReachRate int
	HasReach  bool
	Line      string // "6 memories, 2 notes, reach 50%"
}

// projectsData is the board page payload.
type projectsData struct {
	Group       string
	Sort        string
	Query       string
	Show        string // all, or attention: only the projects that need attention
	Window      string
	WindowLabel string           `json:"-"`
	Windows     []windowOption   `json:"-"`
	Groups      []projectGroupVM // activity and family views
	Flat        []projectRowVM   // flat view
	Global      *projectGlobalVM `json:",omitempty"` // the shared-context line; nil while the global scope holds nothing
	Projects    int              // # project rows, locked ones included (the global scope is not one)
	Working     int              // total live sessions across visible scopes
	LiveScopes  int              // visible projects with at least one live session
	Families    int              // # shared-briefing family bands
	Sessions    int
	OpenTasks   int
	// The open work across the rows shown, in the Tasks page's words.
	Ready         int
	InProgress    int
	Lapsed        int
	Unclaimed     int
	Blocked       int
	Waiting       int
	GatesOpen     int
	PlansActive   int
	PlansMoving   int
	PlansAwaiting int
	TaskLine      string `json:"-"` // "1 in progress · 3 ready · ..." under the Open tasks tile
	PlanLine      string `json:"-"` // "of 4 with open steps · 1 stalled" under the Plans tile
	Memories      int
	Notes         int
	// LockedTasks and LockedMemories are the parts of OpenTasks and Memories
	// that sit in locked projects: counted in the totals, as their rows show
	// them, but never in the task breakdown or the reach, which read work a
	// locked project hides. The tiles name them so the parts add up.
	LockedTasks    int `json:"lockedTasks,omitempty"`
	LockedMemories int `json:"lockedMemories,omitempty"`
	TokensTotal    int
	Surfaced       int
	Active         int
	ReachRate      int
	HasReach       bool
	ReachBand      band `json:"-"`
	Attention      int  // projects shown that need attention (projectNeedsAttention)
	Unregistered   int
	Retired        int
	// Locked are the locked projects this request cannot read (peer_auth.
	// locked): name, counts and last activity only, aggregates taken on the
	// main handle. Each is also a Locked row in the view's groupings, sorted
	// with the others and linking to its unlock page; this list is the same
	// tallies for a script. Empty while nothing is locked.
	Locked []store.ProjectTally `json:"locked,omitempty"`
}

func (s *Service) projectsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	group := r.URL.Query().Get("group")
	if group == "" {
		group = projectGroupKeys[0]
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
	show := r.URL.Query().Get("show")
	if show == "" {
		show = projectShowKeys[0]
	}
	if !slices.Contains(projectShowKeys, show) {
		s.badRequest(w, r, fmt.Sprintf("invalid show %q: valid values are %s",
			show, strings.Join(projectShowKeys, ", ")))
		return
	}
	attentionOnly := show == "attention"
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
	locked := lockedProjects(ctx)
	work, err := s.projectWork(ctx, locked, now)
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

	data := projectsData{
		Group: group, Sort: sortKey, Query: query, Show: show,
		Window: win.Key, WindowLabel: win.Label, Windows: windowOptions(win.Key),
	}
	vmBySlug := make(map[string]projectRowVM, len(board))
	var rows []projectRowVM // every project row the filter keeps, locked ones included
	count := func(vm projectRowVM) {
		data.Working += vm.Working
		data.Sessions += vm.Sessions
		data.OpenTasks += vm.OpenTasks
		data.Ready += vm.Ready
		data.InProgress += vm.InProgress
		data.Lapsed += vm.Lapsed
		data.Unclaimed += vm.Unclaimed
		data.Blocked += vm.Blocked
		data.Waiting += vm.Waiting
		data.GatesOpen += vm.GatesOpen
		data.PlansActive += vm.PlansActive
		data.PlansMoving += vm.PlansMoving
		data.PlansAwaiting += vm.PlansAwaiting
		data.Memories += vm.Memories
		data.Notes += vm.Notes
		data.TokensTotal += vm.TokensTotal
		data.Surfaced += vm.Surfaced
		data.Active += vm.Active
	}
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
		work[b.Project].apply(&vm)
		if vm.Global {
			// The shared-context line stands apart from the filter and from
			// every grouping; the totals count global only when it matches,
			// as they always have.
			data.Global = newProjectGlobalVM(vm)
		}
		// Global is not a project, so it never needs attention: the
		// attention view leaves it out of the totals as it leaves out every
		// project that needs nothing.
		if !matchesQuery(vm, q) || (attentionOnly && !vm.Attention) {
			continue
		}
		count(vm)
		if vm.Global {
			continue
		}
		vmBySlug[b.Project] = vm
		rows = append(rows, vm)
		data.Projects++
		if vm.Working > 0 {
			data.LiveScopes++
		}
		if vm.Attention {
			data.Attention++
		}
		if vm.Unregistered {
			data.Unregistered++
		}
		if vm.Retired {
			data.Retired++
		}
	}
	data.HasReach, data.ReachRate = data.Active > 0, percent(data.Surfaced, data.Active)
	data.ReachBand = floorBand(data.ReachRate, reachTargetPct, data.HasReach)

	if len(locked) > 0 {
		tallies, err := store.ProjectTallies(ctx, s.writeDB(), locked)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		for _, t := range tallies {
			// A locked row is never read past its counts, so it cannot be
			// judged to need attention: the attention view leaves it out.
			if attentionOnly || !t.Registered || (q != "" && !strings.Contains(strings.ToLower(t.Slug), q) && !strings.Contains(strings.ToLower(t.Name), q)) {
				continue
			}
			data.Locked = append(data.Locked, t)
			vm := newLockedRowVM(t)
			count(vm)
			data.LockedTasks += vm.OpenTasks
			data.LockedMemories += vm.Memories
			rows = append(rows, vm)
			data.Projects++
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

	data.TaskLine, data.PlanLine = boardTaskLine(data), boardPlanLine(data)

	less := boardLess(group, sortKey)
	filtered := q != "" || attentionOnly
	switch group {
	case "flat":
		flat := make([]projectRowVM, 0, len(rows))
		flat = append(flat, rows...)
		sortRows(flat, less)
		data.Flat = flat
	case "family":
		data.Groups = familyGroups(rows, vmBySlug, childrenOf, isParent, memoriesBySlug, less, filtered)
	default:
		data.Groups = activityGroups(rows, now, less, filtered)
	}
	s.render(w, r, "projects", pageData{Title: "Projects", Active: "projects", Data: data})
}

// activityGroups is the default view: Active (a live agent, or a session or
// task touched within projectQuietAfter), then Quiet, then Retired. Quiet and
// Retired fold; a band folds open when the filter matched a project inside it,
// and Quiet stands open when nothing is Active, since then nothing waits above
// it. Locked rows sort among the others by their last activity.
func activityGroups(rows []projectRowVM, now time.Time, less func(a, b projectRowVM) bool, filtered bool) []projectGroupVM {
	var active, quiet, retired []projectRowVM
	for _, vm := range rows {
		switch {
		case vm.Retired:
			retired = append(retired, vm)
		case projectIsActive(vm, now):
			active = append(active, vm)
		default:
			quiet = append(quiet, vm)
		}
	}
	var groups []projectGroupVM
	if len(active) > 0 {
		sortRows(active, less)
		groups = append(groups, newProjectGroupVM("active", "Active", "activity", "A live agent, or a session or task in the last 30 days", active))
	}
	if len(quiet) > 0 {
		sortRows(quiet, less)
		g := newProjectGroupVM("quiet", "Quiet", "moon", "No live agent, and no session or task in the last 30 days", quiet)
		g.Folded, g.Open = true, filtered || len(active) == 0
		groups = append(groups, g)
	}
	if len(retired) > 0 {
		sortRows(retired, less)
		g := newProjectGroupVM("retired", "Retired", "archive", "Historical projects kept for lineage", retired)
		g.Folded, g.Open = true, filtered
		groups = append(groups, g)
	}
	return groups
}

// familyGroups is the Family view: each shared-briefing family (a parent row
// followed by its indented children), then Standalone, then Retired, folded.
// A locked row has no family it can show, so it is standalone.
func familyGroups(rows []projectRowVM, vmBySlug map[string]projectRowVM, childrenOf map[string][]string,
	isParent map[string]bool, memoriesBySlug map[string]int, less func(a, b projectRowVM) bool, filtered bool) []projectGroupVM {
	var standalone, retired []projectRowVM
	for _, vm := range rows {
		switch {
		case vm.Retired:
			retired = append(retired, vm)
		case !vm.Locked && (vm.Child || isParent[vm.Slug]):
			// placed into family bands below
		default:
			standalone = append(standalone, vm)
		}
	}

	var groups []projectGroupVM
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
		var band []projectRowVM
		if pv, ok := vmBySlug[parent]; ok && !pv.Retired {
			band = append(band, pv)
		}
		kids := make([]projectRowVM, 0, len(childrenOf[parent]))
		for _, cs := range childrenOf[parent] {
			if cv, ok := vmBySlug[cs]; ok && !cv.Retired {
				cv.Indent = true
				kids = append(kids, cv)
			}
		}
		sortRows(kids, less)
		band = append(band, kids...)
		if len(band) == 0 {
			continue // whole family filtered out by the query
		}
		groups = append(groups, newProjectGroupVM(
			"family-"+parent,
			parent+" · shared briefing",
			"folder-tree",
			familyNote(memoriesBySlug[parent], len(kids)),
			band,
		))
	}

	if len(standalone) > 0 {
		sortRows(standalone, less)
		groups = append(groups, newProjectGroupVM("standalone", "Standalone", "box", "Independent projects", standalone))
	}
	if len(retired) > 0 {
		sortRows(retired, less)
		g := newProjectGroupVM("retired", "Retired by split", "archive", "Historical projects kept for lineage", retired)
		g.Folded, g.Open = true, filtered
		groups = append(groups, g)
	}
	return groups
}

// projectIsActive reports whether a row belongs in the Activity view's Active
// band: a live agent, or activity within projectQuietAfter of now.
func projectIsActive(vm projectRowVM, now time.Time) bool {
	return vm.Working > 0 || (!vm.LastActive.IsZero() && now.Sub(vm.LastActive) <= projectQuietAfter)
}

// newProjectGlobalVM draws the global scope's row as the shared-context line.
func newProjectGlobalVM(vm projectRowVM) *projectGlobalVM {
	g := &projectGlobalVM{Memories: vm.Memories, Notes: vm.Notes, ReachRate: vm.ReachRate, HasReach: vm.HasReach}
	g.Line = plural(g.Memories, "memory", "memories") + ", " + plural(g.Notes, "note", "notes")
	if g.HasReach {
		g.Line += fmt.Sprintf(", reach %d%%", g.ReachRate)
	}
	return g
}

// newLockedRowVM is a locked project's row: what its tally holds and nothing
// more. No description, isolation, star, reach or stage is read for it.
func newLockedRowVM(t store.ProjectTally) projectRowVM {
	return projectRowVM{
		Slug: t.Slug, Name: t.Name, Locked: true, TileIcon: "lock",
		Sessions: t.Sessions, OpenTasks: t.OpenTasks, Memories: t.Memories, Notes: t.Notes,
		LastActive: t.LastActive,
	}
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
		Ready:        b.Ready,
		InProgress:   b.InProgress,
		Lapsed:       b.Lapsed,
		Unclaimed:    b.Unclaimed,
		Blocked:      b.Blocked,
		Waiting:      b.Waiting,
		GatesOpen:    b.GatesOpen,
		Memories:     b.Memories,
		Notes:        b.Notes,
		ReachRate:    b.ReachRate,
		Surfaced:     b.Surfaced,
		Active:       b.Active,
		HasReach:     b.Active > 0,
		LastActive:   b.LastActive,
		TokensTotal:  b.TokensTotal,
	}
	if b.LastTask.After(vm.LastActive) {
		vm.LastActive = b.LastTask
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
func newProjectGroupVM(key, label, icon, note string, rows []projectRowVM) projectGroupVM {
	g := projectGroupVM{Key: key, Label: label, Icon: icon, Count: len(rows), Note: note, Rows: rows}
	var surfaced, active int
	for _, row := range rows {
		g.Working += row.Working
		g.OpenTasks += row.OpenTasks
		g.Blocked += row.Blocked
		g.Waiting += row.Waiting
		g.PlansMoving += row.PlansMoving
		if row.Attention {
			g.Attention++
		}
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

// projectNeedsAttention is what the board's attention chip means: something
// in an open project waits on the owner or is stuck -- a claim that lapsed, a
// task marked in progress that no claim holds, an open gate, a captured plan
// awaiting approval, or data under a slug the project registry does not know.
// Work that waits on other work is ordinary plan sequencing, not a reason:
// blocked tasks never flag a project. A locked row is not read, so it is never
// judged.
func projectNeedsAttention(row projectRowVM) bool {
	if row.Retired || row.Locked || row.Global {
		return false
	}
	return row.Lapsed > 0 || row.Unclaimed > 0 || row.GatesOpen > 0 || row.PlansAwaiting > 0 || row.Unregistered
}

// projectWorkFacts is what the board reads about a project's work beyond its
// board row: its plans with a step still open and how many of those moved
// within planStalledAfter (store.AllPlanRollups), its captured plans awaiting
// approval, and lockedBlocked, the tasks ready on this request's handle that
// unfinished work in a locked project blocks.
type projectWorkFacts struct {
	plansActive   int
	plansMoving   int
	awaiting      int
	lockedBlocked int
}

// projectWork gathers projectWorkFacts for every project, through the
// request's handle: a locked project's plans and captures are absent from the
// read itself. While a project is locked, the ready tasks its unfinished work
// blocks move from ready to blocked, by the Tasks page's rule
// (markLockedBlockers): the filtered handle hides a locked project's tasks and
// their dependency edges, so the main handle is asked for those blockers'
// projects alone, never their content.
func (s *Service) projectWork(ctx context.Context, locked []string, now time.Time) (map[string]projectWorkFacts, error) {
	out := map[string]projectWorkFacts{}
	rollups, err := store.AllPlanRollups(ctx, s.db(ctx))
	if err != nil {
		return nil, err
	}
	for _, p := range rollups {
		if p.Done >= p.Total {
			continue // every step closed: shipped, not in motion
		}
		f := out[p.Project]
		f.plansActive++
		if now.Sub(p.LastActivity) <= planStalledAfter {
			f.plansMoving++
		}
		out[p.Project] = f
	}
	captures, err := store.NotesByTag(ctx, s.db(ctx), "", plans.TagPlan)
	if err != nil {
		return nil, err
	}
	for _, n := range captures {
		if plans.StatusFromTags(n.Tags) == plans.StatusPresented {
			f := out[n.Project]
			f.awaiting++
			out[n.Project] = f
		}
	}
	if len(locked) == 0 {
		return out, nil
	}
	ready, err := store.ReadyTaskProjects(ctx, s.db(ctx))
	if err != nil || len(ready) == 0 {
		return out, err
	}
	ids := make([]string, 0, len(ready))
	for id := range ready {
		ids = append(ids, id)
	}
	by, err := store.OpenBlockerProjects(ctx, s.writeDB(), ids, locked)
	if err != nil {
		return nil, err
	}
	for id := range by {
		f := out[ready[id]]
		f.lockedBlocked++
		out[ready[id]] = f
	}
	return out, nil
}

// apply folds the facts into a row and draws its work line and its needs.
func (f projectWorkFacts) apply(vm *projectRowVM) {
	moved := min(f.lockedBlocked, vm.Ready)
	vm.Ready -= moved
	vm.Blocked += moved
	vm.PlansActive, vm.PlansMoving, vm.PlansAwaiting = f.plansActive, f.plansMoving, f.awaiting
	vm.Attention = projectNeedsAttention(*vm)
	if vm.Attention {
		vm.Needs = projectNeeds(*vm)
	}
	vm.Work = projectWorkLine(*vm)
}

// projectWorkLine is a row's work line, plans first: "2 plans moving, 1 in
// progress, 3 ready". Each phrase is a word of the task vocabulary
// (taskwords.go); a row with no open work has none.
func projectWorkLine(vm projectRowVM) []projectBit {
	var bits []projectBit
	if vm.PlansMoving > 0 {
		bits = append(bits, projectBit{Key: "plans", Text: plural(vm.PlansMoving, "plan moving", "plans moving"),
			Hint: "A plan with a step still open that moved in the last 7 days"})
	}
	if stalled := vm.PlansActive - vm.PlansMoving; stalled > 0 {
		bits = append(bits, projectBit{Key: "stalled", Text: plural(stalled, "plan stalled", "plans stalled"),
			Hint: "A plan with a step still open and nothing moved in 7 days"})
	}
	return append(bits, taskBits(vm.InProgress, vm.Ready, vm.Blocked, vm.Waiting)...)
}

// taskBits words open-task counts: in progress, ready, blocked, waiting on
// people, each only when there is one.
func taskBits(inProgress, ready, blocked, waiting int) []projectBit {
	var bits []projectBit
	for _, b := range []struct {
		n               int
		key, text, hint string
	}{
		{inProgress, wordInProgress, "in progress", "Held by a live claim"},
		{ready, wordReady, "ready", "Nothing unfinished before it: an agent can claim it now"},
		{blocked, wordBlocked, "blocked", "Waits on unfinished work before it"},
		{waiting, wordWaiting, "waiting on people", "A gate, or a step an open gate holds"},
	} {
		if b.n > 0 {
			bits = append(bits, projectBit{Key: b.key, Text: fmt.Sprintf("%d %s", b.n, b.text), Hint: b.hint})
		}
	}
	return bits
}

// projectNeeds says why a row needs attention, in the task vocabulary's words
// and tones. Data the registry does not know is the row's role badge instead.
func projectNeeds(vm projectRowVM) []projectBit {
	var bits []projectBit
	if vm.Lapsed > 0 {
		bits = append(bits, projectBit{Key: wordLapsed, Tone: "danger", Text: plural(vm.Lapsed, "lapsed claim", "lapsed claims"),
			Hint: "In progress, but its holder's lease ran out: any agent can claim it"})
	}
	if vm.Unclaimed > 0 {
		bits = append(bits, projectBit{Key: wordUnclaimed, Tone: "warn", Text: plural(vm.Unclaimed, "unclaimed task", "unclaimed tasks"),
			Hint: "Marked in progress without a claim: any agent can claim it"})
	}
	if vm.GatesOpen > 0 {
		bits = append(bits, projectBit{Key: "gate", Tone: "warn", Text: plural(vm.GatesOpen, "open gate", "open gates"),
			Hint: "Waits on a person, or an event outside Seamless"})
	}
	if vm.PlansAwaiting > 0 {
		bits = append(bits, projectBit{Key: "approval", Tone: "warn", Text: plural(vm.PlansAwaiting, "plan awaiting approval", "plans awaiting approval"),
			Hint: "A captured plan, presented and awaiting your approval"})
	}
	return bits
}

// boardTaskLine is the Tasks tile's line over the rows shown.
func boardTaskLine(d projectsData) string {
	var parts []string
	for _, b := range taskBits(d.InProgress, d.Ready, d.Blocked, d.Waiting) {
		parts = append(parts, b.Text)
	}
	if d.Lapsed > 0 {
		parts = append(parts, plural(d.Lapsed, "lapsed claim", "lapsed claims"))
	}
	if d.Unclaimed > 0 {
		parts = append(parts, plural(d.Unclaimed, "unclaimed", "unclaimed"))
	}
	if d.LockedTasks > 0 {
		parts = append(parts, fmt.Sprintf("%d in locked projects", d.LockedTasks))
	}
	if len(parts) == 0 {
		return "nothing open"
	}
	return strings.Join(parts, " \u00b7 ")
}

// boardPlanLine is the Plans tile's line over the rows shown.
func boardPlanLine(d projectsData) string {
	var parts []string
	if d.PlansActive == 0 {
		parts = append(parts, "no plan has an open step")
	} else {
		parts = append(parts, fmt.Sprintf("of %d with open steps", d.PlansActive))
		if stalled := d.PlansActive - d.PlansMoving; stalled > 0 {
			parts = append(parts, fmt.Sprintf("%d stalled", stalled))
		}
	}
	if d.PlansAwaiting > 0 {
		parts = append(parts, fmt.Sprintf("%d awaiting approval", d.PlansAwaiting))
	}
	return strings.Join(parts, " \u00b7 ")
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

// boardLess is the row comparator a view sorts with: projectLess, except that
// the Activity view's default Recent puts starred projects first, then those
// with a live agent, then the most recent.
func boardLess(group, sortKey string) func(a, b projectRowVM) bool {
	less := projectLess(sortKey)
	if group != "activity" || sortKey != "recent" {
		return less
	}
	return func(a, b projectRowVM) bool {
		if a.Favorite != b.Favorite {
			return a.Favorite
		}
		if (a.Working > 0) != (b.Working > 0) {
			return a.Working > 0
		}
		return less(a, b)
	}
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
