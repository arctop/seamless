package console

// The project-detail workspace turns the thin project peek into a full tabbed
// page: the peek fragment stays the at-a-glance summary, while the full page
// (the "open" target) is a 7-tab workspace over the project's health,
// plans/tasks, sessions, memories, notes, interactions, and context flow.
//
// This file is the route dispatch and page assembly. The tab panels live in
// project_tabs.go; the Context panel reuses the same durable topology builder
// as /console/context.

import (
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// projectTabKeys are the workspace tabs in bar order. A ?tab= deep-link outside
// this set is a 400 (never a silent fallback to Overview), matching the board's
// strict-param policy so an agent driving the console by URL sees the fix.
var projectTabKeys = []string{"overview", "tasks", "sessions", "memories", "notes", "interactions", "context"}

// projectWorkspaceData is the full project-detail page payload: the header, the
// tab bar (with counts), and every tab's server-rendered panel.
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

	Metrics   projectMetrics
	Trend     []store.TrendBucket // this project's injection trend (its own memories)
	TrendWin  string              // window label the trend covers (e.g. "24h")
	MemByKind []kindCount
	Recent    []eventRow

	Plans         []planTimelineVM
	DonePlans     []donePlanVM // finished plans, rollup lines only
	DonePlansMore int          // finished plans past the fold's cap
	Ready         []taskRow
	Blocked       int // open tasks blocked by an unfinished dependency

	Sessions []projSessionVM

	Memories  []projMemoryVM
	Inherited []projMemoryVM // global/parent memories a strict project count excludes

	Notes []projNoteVM

	Interactions []interactionRow
	IxVolumeJSON string // compact JSON of the project's volume buckets, for IX.renderVolume

	Context contextData
}

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

// projectWorkspace builds and renders the full tabbed workspace page.
func (s *Service) projectWorkspace(w http.ResponseWriter, r *http.Request, p core.Project) {
	ctx := r.Context()
	slug := p.Slug

	query := r.URL.Query()
	tab := "overview"
	if values, ok := query["tab"]; ok {
		if len(values) != 1 {
			s.badRequest(w, r, "parameter \"tab\" must be provided exactly once")
			return
		}
		tab = values[0]
	}
	if tab == "relations" {
		query.Set("tab", "context")
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
	}
	now := time.Now().UTC()
	win := store.ResolveRetrievalWindow(r.URL.Query().Get("w"), now)

	data := projectWorkspaceData{
		Slug: slug, Name: p.Name, Description: p.Description,
		Parent: p.ParentSlug, Retired: p.Retired(), Favorite: p.Favorite, ActiveTab: tab,
		IsRoot: p.ParentSlug == "", TrendWin: win.Label,
		Isolation: isolationOf(p), IsolationOptions: isolationOptions(isolationOf(p)),
	}
	if pending != "" {
		confirm, err := s.isolationPendingConfirm(ctx, slug, pending)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		data.IsolationConfirm = confirm
	}
	if data.Name == "" {
		data.Name = slug
	}
	data.TileClass, data.TileIcon = "grp", "server"

	// Header + metric bar: strict per-slug health from the same batched query the
	// board uses, so a single row equals the board row exactly.
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
		OpenTasks: row.OpenTasks, Blocked: row.Blocked,
		HasReach: row.Active > 0, ReachRate: row.ReachRate,
	}
	data.Blocked = row.Blocked
	if cov, cerr := store.GetSessionCoverageForProject(ctx, s.db(ctx), slug, win.Since); cerr == nil && cov.Total > 0 {
		data.Metrics.HasCoverage = true
		data.Metrics.Coverage = percent(cov.Covered, cov.Total)
	}
	prior, hasPrior := s.priorProjectVitals(ctx, slug, win, now)
	data.Metrics.PriorLabel = priorLabel(win, hasPrior)
	data.Metrics.ReachDelta = pointDelta(data.Metrics.ReachRate, prior.ReachRate, hasPrior && data.Metrics.HasReach, riseGood)
	data.Metrics.ReachBand = floorBand(data.Metrics.ReachRate, reachTargetPct, data.Metrics.HasReach)
	data.Metrics.CoverageDelta = pointDelta(data.Metrics.Coverage, prior.Coverage, hasPrior && data.Metrics.HasCoverage && prior.CovTotal > 0, riseGood)
	data.Metrics.CoverageBand = floorBand(data.Metrics.Coverage, continuityTargetPct, data.Metrics.HasCoverage)

	if err := s.fillOverviewTab(ctx, &data, slug, win); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.fillTasksTab(ctx, &data, slug); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.fillSessionsTab(ctx, &data, slug); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.fillMemoriesTab(ctx, &data, slug); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.fillNotesTab(ctx, &data, slug); err != nil {
		s.serverError(w, r, err)
		return
	}
	if err := s.fillInteractionsTab(ctx, &data, slug); err != nil {
		s.serverError(w, r, err)
		return
	}
	contextData, found, err := s.buildContextData(ctx, slug)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !found {
		s.serverError(w, r, fmt.Errorf("console.projectWorkspace: registered scope %q missing from context topology", slug))
		return
	}
	contextData.Scope = "project"
	contextData.Project = slug
	data.Context = contextData

	data.Tabs = []projectTabVM{
		{Key: "overview", Label: "Overview", Icon: "gauge"},
		{Key: "tasks", Label: "Plans & tasks", Icon: "list-checks", Count: data.Metrics.OpenTasks, HasCount: true},
		{Key: "sessions", Label: "Sessions", Icon: "terminal", Count: data.Metrics.Sessions, HasCount: true},
		{Key: "memories", Label: "Memories", Icon: "brain", Count: data.Metrics.Memories, HasCount: true},
		{Key: "notes", Label: "Notes", Icon: "file-text", Count: len(data.Notes), HasCount: true},
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

// projectTabFollowsScreen reports whether a workspace tab is a whole console
// screen scoped to one project, and so takes that screen's level. The other
// tabs share a key with a screen (memories, sessions, notes) but are the
// project's own lists, visible wherever the workspace is.
func projectTabFollowsScreen(tab string) bool { return tab == "interactions" || tab == "context" }
