package console

// The project-detail workspace turns the thin project peek into a full tabbed
// page: the peek fragment stays the at-a-glance summary, while the full page
// (the "open" target) is a 7-tab workspace over the project's plans/tasks,
// health, sessions, memories, notes, interactions, and context flow.
//
// It opens on Plans & tasks, the tab the owner follows plans from
// (plan:console-ux-pass D1), and a load reads and renders one tab: the active
// one. The tab bar's links are real ?tab= navigations that the console's
// in-place client takes (static/navigation.js), so a switch keeps the document
// and morphs only the view, and a live refresh re-reads the tab on screen. The
// header and the tab pills come from the board's one batched row.
//
// This file is the route dispatch and page assembly. The tab panels live in
// project_tabs.go; the Context panel reuses the same durable topology builder
// as /console/context.

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// projectTabKeys are the workspace tabs in bar order, the work first. A ?tab=
// deep-link outside this set (and its aliases) is a 400, never a silent
// fallback to the default tab, matching the board's strict-param policy so an
// agent driving the console by URL sees the fix.
var projectTabKeys = []string{"tasks", "overview", "sessions", "memories", "notes", "interactions", "context"}

// projectDefaultTab is the tab a link that names none opens: Plans & tasks.
// Two parameters choose another when no tab is named: ?from= opens Memories
// (the came-from filter's bar lives on the tabs it narrows) and ?isolate=
// opens Overview (the tighten confirm lives on its Isolation card).
const projectDefaultTab = "tasks"

// projectTabAliases are names a link may use for a tab, answered with a
// permanent redirect to the tab's key: "relations" is the Context tab's old
// name, "plans" the word the owner uses for Plans & tasks.
var projectTabAliases = map[string]string{"relations": "context", "plans": "tasks"}

// projectWorkspaceData is the full project-detail page payload: the header, the
// tab bar (with counts), and the active tab's server-rendered panel. Only the
// active tab's fields are filled; the others stay empty.
type projectWorkspaceData struct {
	Slug        string
	Name        string
	Description string
	TileClass   string
	TileIcon    string
	Live        int  // active sessions ("N agents working")
	IsRoot      bool // no parent (a root project injects only its own memories)
	Parent      string
	Retired     bool
	Favorite    bool
	ActiveTab   string
	Tabs        []projectTabVM
	// Stage is the momentum maturity glyph, pre-rendered ("" while the
	// feature is off); the same glyph the board row shows, by construction.
	Stage template.HTML

	// The agent-facing fence: the title-row pill, the Overview control, and the
	// pending confirm panel a ?isolate= tighten renders (nil the rest of the time).
	Isolation        core.Isolation
	IsolationOptions []isolationOptionVM
	IsolationConfirm *isolationConfirmVM
	// Lock is the project's lock under peer_auth; nil with peer_auth off, so
	// nothing renders. LockSetup is then set where peer_auth runs (macOS and
	// Linux): the card names the command that turns it on.
	Lock      *lockView
	LockSetup bool

	Metrics   projectMetrics
	Trend     []store.TrendBucket // this project's injection trend (its own memories)
	TrendWin  string              // window label the trend covers (e.g. "24h")
	MemByKind []kindCount
	Recent    []eventRow

	Plans         []planTimelineVM
	DonePlans     []donePlanVM // finished plans, rollup lines only
	DonePlansMore int          // finished plans past the fold's cap
	// OffPlan is the project's work outside any plan, grouped by what is true
	// of it (offPlanVM), and the tab's open tally.
	OffPlan offPlanVM
	Blocked int // open tasks blocked by an unfinished dependency

	Sessions     []projSessionVM
	SessionTally store.ProjectSessionTally // the project's sessions by state, the tab's header

	Memories  []projMemoryVM
	Inherited []projMemoryVM // global/parent memories a strict project count excludes

	Notes []projNoteVM

	Interactions []interactionRow
	IxVolumeJSON string // compact JSON of the project's volume buckets, for IX.renderVolume

	Context contextData

	// CameFrom is the came-from filter (camefrom.go): nil -- no filter, no
	// stamps on the rows -- for a project none of whose items carries a stamp.
	CameFrom *cameFromView
}

// CameFromOn reports that the came-from filter narrows the tabs.
func (d projectWorkspaceData) CameFromOn() bool { return d.CameFrom != nil && d.CameFrom.Active != "" }

// projectTabVM is one entry in the workspace tab bar.
type projectTabVM struct {
	Key      string
	Label    string
	Icon     string
	Count    int
	HasCount bool
	Active   bool
}

// projectMetrics is the workspace header metric bar: strict per-slug health
// (matching the board row) plus windowed coverage.
type projectMetrics struct {
	Memories    int
	Sessions    int
	Live        int
	OpenTasks   int
	Blocked     int
	Waiting     int // gates and the steps they hold (store.TaskCounts.Waiting)
	HasReach    bool
	ReachRate   int
	HasCoverage bool
	Coverage    int

	// Judgment for the two rates, against this project's own prior equal-length
	// window and the console-wide targets.
	PriorLabel    string
	ReachDelta    delta
	ReachBand     band
	CoverageDelta delta
	CoverageBand  band
}

// projectDetail dispatches the /console/projects/{slug} route: the peek fragment
// (?peek=1) and the CLI (JSON) get the thin summary; a browser hitting the full
// page gets the tabbed workspace. A retired project still renders (with its
// banner), only an unknown slug 404s.
func (s *Service) projectDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	p, ok, err := store.ProjectBySlug(ctx, s.db(ctx), slug)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok && lockedHere(ctx, slug) {
		s.lockedProject(w, r, slug)
		return
	}
	if !ok {
		s.notFound(w, r, "No project with slug "+slug+".")
		return
	}
	if r.URL.Query().Get("peek") == "1" || wantsJSON(r) {
		s.projectSummary(w, r, p)
		return
	}
	s.projectWorkspace(w, r, p)
}

// lockSetupGOOS is the platform the daemon runs on, which decides whether the
// Isolation card offers seamlessd peer-auth setup: peer_auth runs on macOS and
// Linux only.
var lockSetupGOOS = runtime.GOOS

// projectSummary renders the thin project peek: metadata + per-channel counts,
// served as the peek fragment (?peek=1) or CLI JSON.
func (s *Service) projectSummary(w http.ResponseWriter, r *http.Request, p core.Project) {
	ctx := r.Context()
	counts, err := store.GetProjectCounts(ctx, s.db(ctx), p.Slug)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d := projectDetail{
		Slug: p.Slug, Name: p.Name, Description: p.Description,
		Memories: counts.Memories, Sessions: counts.Sessions,
		OpenTasks: counts.OpenTasks, Notes: counts.Notes,
		Created: p.CreatedAt, Updated: p.UpdatedAt,
		Isolation: isolationOf(p), IsolationPromise: isolationPromise(isolationOf(p)),
	}
	s.renderDetail(w, r, "project", pageData{Title: "Project " + p.Slug, Active: "projects", Data: d})
}

// projectWorkspace builds and renders the full tabbed workspace page: the
// header and tab bar, and the one panel ?tab= names.
func (s *Service) projectWorkspace(w http.ResponseWriter, r *http.Request, p core.Project) {
	ctx := r.Context()
	slug := p.Slug

	query := r.URL.Query()
	tab, chosen := projectDefaultTab, false
	if values, ok := query["tab"]; ok {
		if len(values) != 1 {
			s.badRequest(w, r, "parameter \"tab\" must be provided exactly once")
			return
		}
		tab, chosen = values[0], true
	}
	if key, ok := projectTabAliases[tab]; ok {
		query.Set("tab", key)
		http.Redirect(w, r, r.URL.Path+"?"+query.Encode(), http.StatusPermanentRedirect)
		return
	}
	if !slices.Contains(projectTabKeys, tab) {
		s.badRequest(w, r, fmt.Sprintf("invalid tab %q: valid values are %s",
			tab, strings.Join(projectTabKeys, ", ")))
		return
	}
	// The pending-tighten page state. Strict like ?tab=: a value outside the
	// isolation enum is a 400, never a silently dropped panel.
	var pending core.Isolation
	if values, ok := query["isolate"]; ok {
		if len(values) != 1 {
			s.badRequest(w, r, "parameter \"isolate\" must be provided exactly once")
			return
		}
		state, err := validate.Isolation(values[0])
		if err != nil {
			s.badRequest(w, r, err.Error())
			return
		}
		pending = state
		if !chosen {
			// The confirm panel lives on Overview's Isolation card.
			tab = "overview"
		}
	}
	// The came-from filter (?from=<project>): strict like ?tab=. A project
	// no item names is a filter that matches nothing, not an error.
	var from string
	if values, ok := query["from"]; ok {
		if len(values) != 1 {
			s.badRequest(w, r, "parameter \"from\" must be provided exactly once")
			return
		}
		if err := validate.Name(values[0]); err != nil {
			s.badRequest(w, r, fmt.Sprintf("invalid from %q: a project slug", values[0]))
			return
		}
		from = values[0]
		if !chosen {
			// The bar lives on the tabs it narrows: a link that names only
			// the filter opens Memories, not a tab that shows no bar while
			// its tab counts are already narrowed (P13, 2026-11-14).
			tab = "memories"
		}
	}
	now := time.Now().UTC()
	win := store.ResolveRetrievalWindow(r.URL.Query().Get("w"), now)

	data := projectWorkspaceData{
		Slug: slug, Name: p.Name, Description: p.Description,
		Parent: p.ParentSlug, Retired: p.Retired(), Favorite: p.Favorite, ActiveTab: tab,
		IsRoot: p.ParentSlug == "", TrendWin: win.Label,
		Isolation: isolationOf(p), IsolationOptions: isolationOptions(isolationOf(p)),
	}
	if data.Name == "" {
		data.Name = slug
	}
	data.TileClass, data.TileIcon = "grp", "server"

	// Header, metric bar and tab pills: strict per-slug health from the same
	// batched query the board uses, so a single row equals the board row
	// exactly, and no tab's own rows are read to count them.
	board, err := store.ProjectsWithCounts(ctx, s.db(ctx), win, now, s.cfg.SessionIdleTTL)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var row store.ProjectBoardRow
	for _, b := range board {
		if b.Project == slug {
			row = b
			break
		}
	}
	if info, ok := s.projectStages(ctx, board, now)[slug]; ok {
		data.Stage = stageGlyph(info, now)
	}
	data.Live = row.LiveSessions
	data.Metrics = projectMetrics{
		Memories: row.Memories, Sessions: row.Sessions, Live: row.LiveSessions,
		OpenTasks: row.OpenTasks, Blocked: row.Blocked, Waiting: row.Waiting,
		HasReach: row.Active > 0, ReachRate: row.ReachRate,
	}
	data.Blocked = row.Blocked

	// The came-from filter reads its sources for the bars of the tabs it
	// narrows, and its items whenever one is chosen: the tab links carry it
	// and the narrowed pills count what those tabs list.
	var keep map[string]map[string]bool
	if _, narrows := cameFromTabs[tab]; narrows || from != "" {
		cameFrom, k, err := s.cameFromFor(ctx, slug, from)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		data.CameFrom, keep = cameFrom, k
	}

	if err := s.fillProjectTab(r, &data, tab, win, now, pending); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.applyCameFrom(ctx, &data, keep)

	taskCount, memCount, noteCount := data.Metrics.OpenTasks, data.Metrics.Memories, row.Notes
	if data.CameFromOn() {
		// Narrowed, a tab counts what it lists.
		taskCount, memCount, noteCount = len(data.CameFrom.Tasks), len(keep[core.ItemKindMemory]), len(keep[core.ItemKindNote])
	}
	data.Tabs = []projectTabVM{
		{Key: "tasks", Label: "Plans & tasks", Icon: "list-checks", Count: taskCount, HasCount: true},
		{Key: "overview", Label: "Overview", Icon: "gauge"},
		{Key: "sessions", Label: "Sessions", Icon: "terminal", Count: data.Metrics.Sessions, HasCount: true},
		{Key: "memories", Label: "Memories", Icon: "brain", Count: memCount, HasCount: true},
		{Key: "notes", Label: "Notes", Icon: "file-text", Count: noteCount, HasCount: true},
		{Key: "interactions", Label: "Interactions", Icon: "activity"},
		{Key: "context", Label: "Context", Icon: "share-2"},
	}
	// The Interactions and Context tabs are those screens scoped to one
	// project, so they follow the screens' levels: below it the tab leaves the
	// bar, and a direct ?tab= request still renders the panel (hidden, not
	// locked) under the banner naming that screen. The workspace is HTML-only
	// (JSON callers get projectSummary), so no JSON answer changes.
	pd := pageData{Title: "Project " + slug, Active: "projects", Data: data}
	lvl := s.consoleLevel(ctx).Level
	data.Tabs = slices.DeleteFunc(data.Tabs, func(t projectTabVM) bool {
		sc, ok := screenByID(t.Key)
		return ok && projectTabFollowsScreen(t.Key) && lvl < sc.Min && t.Key != tab
	})
	if projectTabFollowsScreen(tab) {
		pd.LevelBanner = levelBannerFor(tab, lvl)
	}
	for i := range data.Tabs {
		data.Tabs[i].Active = data.Tabs[i].Key == tab
	}

	pd.Data = data
	s.render(w, r, "projectdetail", pd)
}

// fillProjectTab reads and fills the one panel the page renders. pending is
// the ?isolate= state Overview's confirm panel renders ("" for none).
func (s *Service) fillProjectTab(r *http.Request, data *projectWorkspaceData, tab string, win store.RetrievalWindow, now time.Time, pending core.Isolation) error {
	ctx := r.Context()
	slug := data.Slug
	switch tab {
	case "tasks":
		if data.CameFromOn() {
			// The filter lists the tasks themselves, read by id without
			// their blockers: read them, so each row's word tells ready
			// from blocked. The plans are not shown, so not read.
			return s.readRowBlockers(ctx, data.CameFrom.Tasks)
		}
		return s.fillTasksTab(ctx, data, slug, now)
	case "overview":
		return s.fillOverviewTab(r, data, win, now, pending)
	case "sessions":
		return s.fillSessionsTab(ctx, data, slug)
	case "memories":
		return s.fillMemoriesTab(ctx, data, slug)
	case "notes":
		return s.fillNotesTab(ctx, data, slug)
	case "interactions":
		return s.fillInteractionsTab(ctx, data, slug)
	case "context":
		contextData, found, err := s.buildContextData(ctx, slug)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("console.projectWorkspace: registered scope %q missing from context topology", slug)
		}
		contextData.Scope = "project"
		contextData.Project = slug
		data.Context = contextData
		return nil
	}
	return fmt.Errorf("console.fillProjectTab: tab %q has no panel", tab)
}

// errorBack is the styled error page's way back to the project a console URL
// under /console/projects/<slug> names.
type errorBack struct {
	Href  string
	Label string
}

// errorBackFor names the project r's path is under, for the error page's
// "Back to <project>": nil for any other path, and for a slug this request
// does not read (unknown, or a locked project its handle hides), so the page
// never links to a project it cannot open.
func (s *Service) errorBackFor(r *http.Request) *errorBack {
	rest, ok := strings.CutPrefix(r.URL.Path, "/console/projects/")
	if !ok {
		return nil
	}
	slug, _, _ := strings.Cut(rest, "/")
	if slug == "" || validate.Name(slug) != nil {
		return nil
	}
	ctx := r.Context()
	p, found, err := store.ProjectBySlug(ctx, s.db(ctx), slug)
	if err != nil || !found {
		return nil
	}
	label := p.Name
	if label == "" {
		label = p.Slug
	}
	return &errorBack{Href: "/console/projects/" + url.PathEscape(p.Slug), Label: label}
}

// projectTabFollowsScreen reports whether a workspace tab is a whole console
// screen scoped to one project, and so takes that screen's level. The other
// tabs share a key with a screen (memories, sessions, notes) but are the
// project's own lists, visible wherever the workspace is.
func projectTabFollowsScreen(tab string) bool { return tab == "interactions" || tab == "context" }
