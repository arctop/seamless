package console

// The project-detail workspace's tab panels. Each fillXTab loads exactly one
// panel into projectWorkspaceData and owns that tab's view models; the page
// assembly lives in project_detail.go, and a page load calls the one fill its
// active tab needs. The panels that read the event log read the project's own
// events (events.Recorder.ProjectEvents), never the fleet's tail filtered here:
// on a busy fleet a quiet project's activity is not in the newest few hundred.

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/markdown"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// Plans & tasks tab tuning. A closed block shorter than doneRunFold is not
// worth hiding;
// donePlansCap bounds the completed-plans fold; the ledger scans
// planLedgerScan recent transitions and keeps planLedgerRows per plan;
// planChainDepth caps the blocker walk; the off-plan card's closed fold shows
// the offPlanClosedShown most recently closed tasks and links to the rest.
const (
	doneRunFold        = 3
	donePlansCap       = 20
	planLedgerScan     = 300
	planLedgerRows     = 3
	planChainDepth     = 3
	offPlanClosedShown = 10
)

// offPlanVM is the Plans & tasks tab's off-plan card: every task of the
// project outside a plan, once, in the group that says what is true of it
// (offPlanGroup), each row the plan-step row, then the most recently closed
// behind a fold. InPlans and Open are the card's tally line, which holds the
// tab's pill to the rows: the pill counts the project's open tasks, plan steps
// included, and every one of them is a row on this tab.
type offPlanVM struct {
	Groups      []offPlanGroup // the open groups that hold a row, in reading order
	Open        int            // open off-plan tasks: the rows of Groups
	InPlans     int            // open steps of the project's active plans
	Closed      []planStepVM   // the most recently closed, newest first
	ClosedTotal int            // every closed off-plan task of the project
}

// Total is the open tasks the tab lists: its plans' open steps and its
// off-plan rows.
func (o offPlanVM) Total() int { return o.InPlans + o.Open }

// offPlanGroup is one group of the off-plan card. Key is its anchor and
// data-group (in-progress, ready, blocked, waiting, gates), Hint what it
// holds, for its title, and Note what its count alone does not say.
type offPlanGroup struct {
	Key   string
	Label string
	Hint  string
	Note  string
	Rows  []planStepVM
}

// gateLineVM is one open gate a row waits on: its own, or a gate it depends
// on, by who must act and what must happen.
type gateLineVM struct {
	Who  string
	What string
}

// planTimelineVM is one plan's card (templates/plan_shared.html's plan-card):
// the rollup counts, the composition's narrative note (title, status in
// words, star, Approve while a capture awaits it), the head's count of its
// steps by their words, the stalled mark, the last few transitions, and the
// step rows grouped so long done runs fold away.
type planTimelineVM struct {
	Slug         string
	Project      string // the project whose steps the card shows
	Title        string // the narrative note's title; the slug without a note
	Total        int
	Done         int
	InFlight     int
	Open         int
	Claimable    int // steps an agent could claim now: the rows' own count
	DonePct      int
	DoingPct     int
	LastActivity time.Time
	// GatesOpen is the rollup's open gates: gate tasks, and steps whose own
	// gate is open (store.PlanRollup.GatesOpen).
	GatesOpen int

	// Counts words the unfinished steps for the head, each once by its word
	// (planCounts): "1 in progress", "2 waiting on owner".
	Counts []planCountVM
	// Stalled marks open steps, none held, nothing moved for longer than
	// planStalledAfter; StalledDays is for how long (planStalled).
	Stalled     bool
	StalledDays int

	// The composition's primary note. A plan whose steps exist without one
	// degrades to its slug as the title and no status, star or narrative link,
	// rather than to empty ones.
	HasNote    bool
	NoteID     string
	NoteTitle  string
	Status     string
	StatusText string // Status in words (planStatusText)
	Favorite   bool
	// Capture is a Claude Code plan-mode capture; CanApprove one still
	// draft or presented, which the card offers to approve.
	Capture    bool
	CanApprove bool
	// NoteOnly is a captured plan with no steps yet: a card with its status
	// and no rows.
	NoteOnly bool
	// Return is where the card's own actions land (the star, Approve): the
	// surface that shows it, without the card's anchor.
	Return string

	// Stale marks a plan a pending gardener settlement (ship/abandon) names.
	Stale bool

	Recent []planLedgerVM
	Groups []planStepGroup

	// The Plans board's and the plan page's (plans.go); zero on the project
	// tab, which renders as it did. State is the board's group; Anchor the
	// card's id, unique per project and slug (CardID); ShowProject puts the
	// project on the card as a chip, ProjectHref the project tab at it ("" for
	// a project with no workspace). Lazy marks a card that loads its steps
	// from StepsURL when it is opened. The token fields are the plan's model
	// tokens (store.PlanTokenRollup: whole-session, never summed across
	// plans). Dropped is how many of the closed steps were dropped, and
	// MomSettle marks a plan shipped this local month (momentum only).
	State           string
	Anchor          string
	ShowProject     bool
	ProjectHref     string
	Lazy            bool
	StepsURL        string
	Tokens          int
	TokenSessions   int
	TokenUnreported int
	TokenShared     int
	Dropped         int
	MomSettle       bool
}

// CardID is the card's anchor: plan-<slug> on the project tab, where a slug
// names one plan; the board's own (planAnchor) where it does not.
func (tl planTimelineVM) CardID() string {
	if tl.Anchor != "" {
		return tl.Anchor
	}
	return "plan-" + tl.Slug
}

// Href is the plan's page, its project named: one slug is a plan per project.
func (tl planTimelineVM) Href() string { return planHref(tl.Project, tl.Slug) }

// ApproveAction is the escape hatch's route for this plan, its project named,
// so Approve never has to guess between two projects' captures of one slug.
func (tl planTimelineVM) ApproveAction() string {
	return "/console/plans/" + url.PathEscape(tl.Slug) + "/approve?project=" + url.QueryEscape(planProjectParam(tl.Project))
}

// TitleHTML is the card's title for display (planTitleHTML).
func (tl planTimelineVM) TitleHTML() template.HTML { return planTitleHTML(tl.Title) }

// ProjectLabel is the card's project as the board shows it.
func (tl planTimelineVM) ProjectLabel() string { return planProjectLabel(tl.Project) }

// DoneOnly is the closed steps that were done, not dropped.
func (tl planTimelineVM) DoneOnly() int { return tl.Done - tl.Dropped }

// planCountVM is one part of a plan head's count: its word's key (a
// data-count attribute) and what it says ("2 blocked").
type planCountVM struct {
	Key  string
	Text string
}

// donePlanVM is one line in the completed-plans fold: a finished plan reduced
// to its rollup, since none of it is claimable any more.
type donePlanVM struct {
	Slug         string
	Total        int
	LastActivity time.Time
}

// planStepGroup is one run of step rows. A DoneRun group is a run of closed
// steps long enough to fold behind a summary (LastTitle/LastClosed describe the
// most recently closed step in it); every other group is a single live step the
// timeline renders in full.
type planStepGroup struct {
	DoneRun    bool
	LastTitle  string
	LastClosed time.Time
	Steps      []planStepVM
}

// planLedgerVM is one step's latest transition in a plan's recent-activity
// ledger. Tone is the verb's colour class (ok/brand/warn/muted), so the row
// scans by colour before it is read.
type planLedgerVM struct {
	Verb      string
	Icon      string
	Tone      string
	StepID    string
	StepTitle string
	Session   string // session name, when the transition had an actor
	SessionID string
	Owner     bool // the console did it, not an agent
	When      time.Time
}

// depChipVM is one dependency-edge chip on a step row: a blocker in the chain,
// or a step this one unblocks. OnPage marks a target rendered on this page, so
// the chip can jump to it instead of navigating away.
type depChipVM struct {
	ID     string
	Title  string
	Status string
	State  string
	OnPage bool
	Word   taskWord // a blocker's state in the owner's words, for its title
}

// planStepVM is one step in a plan timeline: its status, claim/lease, and both
// dependency directions. State is the CSS row modifier (done/doing/blocked/
// waiting/open); it is derived, never stored.
type planStepVM struct {
	ID        string
	Title     string
	Project   string // the row's own actions post back to this project's tab
	Plan      string // its plan's slug; "" off-plan
	Kind      string // core.TaskKind: a step or a gate
	Account   string // restricted account that created the step ("" = owner)
	Status    string
	State     string
	Word      taskWord  // its state in the owner's words (taskwords.go)
	Claimable bool      // open and unblocked: an agent could take it right now
	Created   time.Time // the plan's own step order, oldest first
	Closed    time.Time // when it reached a terminal status (the done-run label)

	ClaimedBy   string // session name; the holder's short id when it resolved to none
	ClaimedByID string // the session's id, for its link; "" when it resolved to none
	LeaseLeft   string // server-rendered remaining lease, the no-JS fallback
	LeaseExp    string // RFC3339 expiry, for the client ticker
	LeaseLapsed bool   // held by a session whose lease ran out: reclaimable
	// Claimless is in progress with no holder at all (a claimless start:
	// seam task start, tasks_update status=in_progress): claimable by any
	// agent, and nobody's to release from the row (constraint
	// claimtask-claimless-inprogress-and-lease-boundary).
	Claimless bool
	// Alert is the claim strip's word under a lapsed or claimless row, in
	// the one task vocabulary (taskwords.go): claim lapsed or unclaimed.
	Alert taskWord

	Chain         []depChipVM // unfinished blockers, nearest first
	Unblocks      []depChipVM // steps waiting on this one
	UnblocksTitle string      // those steps named, for the chip's tooltip
	// IsGate marks a task of kind gate: nobody claims it, and the tasks
	// waiting on it are what it holds. Waits are the open gates the row
	// waits on, who and what (waitsOf).
	IsGate bool
	Waits  []gateLineVM
	// LockedBlockers are the step's unfinished blockers in a locked project
	// this request does not read: their project, once per
	// blocker (markLockedBlockers' rule). nil while nothing is locked.
	LockedBlockers []string
	// Return is where the row's own actions land on the page that shows it
	// (the Plans board, a plan's page); "" on the project tab, whose rows
	// land on the tab (ReleaseNext).
	Return string
	// deps is the task's own blockers, by id: groupSteps reads them to put
	// a blocker above what it blocks.
	deps []string
}

// ReleaseNext is where the claim strip's release lands: back on this row,
// on the page that shows it.
func (st planStepVM) ReleaseNext() string {
	if st.Return != "" {
		return st.Return + "#" + st.ID
	}
	return "/console/projects/" + st.Project + "?tab=tasks#" + st.ID
}

// LockedIn is the locked projects the step's locked blockers sit in, once each.
func (st planStepVM) LockedIn() []string { return slices.Compact(slices.Clone(st.LockedBlockers)) }

// HoldsText is a gate row's count of what it holds: "holds 2 steps" for a
// plan's gate, "holds 1 task" for one outside a plan.
func (st planStepVM) HoldsText() string {
	noun := "task"
	if st.Plan != "" {
		noun = "step"
	}
	if len(st.Unblocks) != 1 {
		noun += "s"
	}
	return "holds " + strconv.Itoa(len(st.Unblocks)) + " " + noun
}

// planStepCtx is the per-page state every step projection reads: the task cache
// the dependency walk fills, which ids render on this page, the reverse-edge
// index, and the shared session namer.
type planStepCtx struct {
	cache    map[string]core.Task
	onPage   map[string]bool
	unblocks map[string][]depChipVM
	namer    func(string) core.Session
	now      time.Time
	// lockedBy is each open step's blockers in locked projects the request
	// does not read, by project (store.OpenBlockerProjects); nil while
	// nothing is locked.
	lockedBy map[string][]string
	// ret is where the rows' own actions return (planStepVM.Return); "" on
	// the project tab.
	ret string
}

// projSessionVM is one row on the Sessions tab. State is its word, the
// Sessions screen's: live, idle (active, past the idle window, holding no live
// claim), completed or expired.
type projSessionVM struct {
	ID       string
	Name     string
	Status   string
	State    string
	Source   string
	Account  string // restricted account that started it ("" = owner)
	Ambient  bool
	Live     bool // core.Session.LiveAsOf at the read
	Findings string
	Holds    []heldTaskVM // the tasks the session holds a live claim on
	Updated  time.Time
	// Headline titles the row by what the session is doing, as the Sessions
	// screen does (sessionHeadlines): the task it holds, else its first
	// prompt. "" keeps the name. HeadlineFrom is headlineTask or headlinePrompt.
	Headline     string
	HeadlineFrom string
}

// heldTaskVM is a task a live session holds, as its row names it: by title,
// linked to the task.
type heldTaskVM struct {
	ID    string
	Title string
}

// projectSessionsShown caps the Sessions tab's rows; the header counts them
// all and links to the Sessions screen filtered to the project.
const projectSessionsShown = 60

// projMemoryVM is one row on the Memories tab, carrying a rendered lineage cell
// (provenance session, or a supersession pointer).
type projMemoryVM struct {
	ID         string
	Name       string
	Kind       string
	Desc       string
	Account    string // restricted account that wrote the content ("" = owner)
	Lineage    template.HTML
	Injects    int
	Reads      int
	Updated    time.Time
	Superseded bool
	// MayContain is the memory's provenance stamps (camefrom.go): set on a
	// project whose items carry any, nil otherwise.
	MayContain []core.Stamp
}

// projNoteVM is one row on the Notes tab.
type projNoteVM struct {
	ID      string
	Title   string
	Desc    string
	Account string // restricted account that wrote the content ("" = owner)
	Tags    []string
	Updated time.Time
	// MayContain is the note's provenance stamps, as projMemoryVM's.
	MayContain []core.Stamp
}

// fillOverviewTab loads the Overview panel: the metric bar's two judged rates,
// memories-by-kind (project's active memories), this project's injection
// trend (its own memories, matching the board reach), the Isolation card (its
// lock, and the ?isolate= confirm when pending names one), and the project's
// recent activity.
func (s *Service) fillOverviewTab(r *http.Request, data *projectWorkspaceData, win store.RetrievalWindow, now time.Time, pending core.Isolation) error {
	ctx := r.Context()
	slug := data.Slug
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

	lock, err := s.lockViewFor(r, slug, data.Isolation)
	if err != nil {
		return err
	}
	data.Lock = lock
	data.LockSetup = lock == nil && (lockSetupGOOS == "darwin" || lockSetupGOOS == "linux")
	if lock.Locked() {
		disableUnfenced(data.IsolationOptions)
	}
	if pending != "" {
		confirm, err := s.isolationPendingConfirm(ctx, slug, pending)
		if err != nil {
			return err
		}
		data.IsolationConfirm = confirm
	}

	active, err := store.ActiveMemories(ctx, s.db(ctx), slug)
	if err != nil {
		return err
	}
	byKind := map[string]int{}
	for _, m := range active {
		if m.Project == slug { // strict: exclude inherited global/parent rows
			byKind[string(m.Kind)]++
		}
	}
	data.MemByKind = orderKinds(byKind)

	trend, err := store.ProjectRetrievalTrend(ctx, s.db(ctx), win, slug)
	if err != nil {
		return err
	}
	data.Trend = trend

	if s.cfg.Events != nil {
		evs, err := s.eventLog(ctx).ProjectEvents(ctx, slug, nil, s.ledgerExcludedKinds(ctx), projectRecentRows)
		if err != nil {
			return err
		}
		for _, e := range evs {
			data.Recent = append(data.Recent, toEventRow(e))
		}
	}
	return nil
}

// projectRecentRows is how many of the project's events Overview's Recent
// activity lists.
const projectRecentRows = 8

// fillTasksTab loads the Plans & tasks panel as of now: each active plan's
// live card (rollup, narrative note, the head's count of its steps by their
// words, the stalled mark, recent-transition ledger, staleness) with its step
// timeline, a card for each captured plan with no steps yet (noteOnlyCards),
// the completed plans as rollup lines, and the off-plan card: every task
// outside a plan, grouped by what is true of it (offPlanRows).
//
// Query budget: one rollup query, one ListTasksForPlan per ACTIVE plan (a
// completed plan renders from its rollup alone), five for the off-plan tasks
// (readOffPlan), then exactly one query each for the plan notes, the
// transition ledger and the pending settlements, and one per open gate, a
// plan's or an off-plan one, for what it holds (gateHolds). No per-step or
// per-plan lookup beyond the dependency walk's cache misses.
func (s *Service) fillTasksTab(ctx context.Context, data *projectWorkspaceData, slug string, now time.Time) error {
	rollups, err := store.PlanRollupsForProject(ctx, s.db(ctx), slug)
	if err != nil {
		return err
	}
	var active []store.PlanRollup
	hasSteps := make(map[string]bool, len(rollups))
	for _, pl := range rollups {
		hasSteps[pl.Slug] = true
		if pl.Done < pl.Total {
			active = append(active, pl)
			continue
		}
		if len(data.DonePlans) < donePlansCap {
			data.DonePlans = append(data.DonePlans, donePlanVM{
				Slug: pl.Slug, Total: pl.Total, LastActivity: pl.LastActivity,
			})
		} else {
			data.DonePlansMore++
		}
	}

	// Load every active plan's steps first: the reverse-edge index, the ledger
	// filter and the dependency walk all read the same in-memory set.
	steps := make(map[string][]core.Task, len(active))
	cache := map[string]core.Task{}
	for _, pl := range active {
		list, lerr := store.ListTasksForPlan(ctx, s.db(ctx), slug, "", pl.Slug)
		if lerr != nil {
			return lerr
		}
		steps[pl.Slug] = list
		for _, st := range list {
			cache[st.ID] = st
		}
	}
	// The off-plan tasks still open are rows of this page too: a chain chip
	// jumps to one, and a step it holds says so.
	off, err := s.readOffPlan(ctx, slug)
	if err != nil {
		return err
	}
	for _, t := range off.open() {
		cache[t.ID] = t
	}
	onPage := make(map[string]bool, len(cache))
	for id := range cache {
		onPage[id] = true
	}
	namer := s.sessionNamer(ctx)
	// Ordering matters: these read the cache while it still holds only this
	// page's rows -- the dependency walk below adds their blockers.
	unblocks := reverseEdges(cache)
	ledger := s.planLedger(ctx, slug, cache, namer)
	lockedBy, err := s.lockedBlockersOf(ctx, cache)
	if err != nil {
		return err
	}

	notes, err := s.planNoteMeta(ctx, slug)
	if err != nil {
		return err
	}
	stale := s.stalePlanSlugs(ctx, slug)

	pc := planStepCtx{cache: cache, onPage: onPage, unblocks: unblocks, namer: namer, now: now, lockedBy: lockedBy}
	ret := "/console/projects/" + slug + "?tab=tasks"
	inPlans := 0 // the plans' open steps: every one is a row on its card
	var cards []planTimelineVM
	for _, pl := range active {
		tl := planTimelineVM{
			Slug: pl.Slug, Project: slug, Title: pl.Slug, Return: ret,
			Total: pl.Total, Done: pl.Done, InFlight: pl.InFlight,
			Open:         pl.Total - pl.Done - pl.InFlight,
			LastActivity: pl.LastActivity, GatesOpen: pl.GatesOpen,
			DonePct: percent(pl.Done, pl.Total), DoingPct: percent(pl.InFlight, pl.Total),
			Stale:  stale[pl.Slug],
			Recent: ledger[pl.Slug],
		}
		if n, ok := notes[pl.Slug]; ok {
			tl.withNote(n)
		}
		rows := make([]planStepVM, 0, len(steps[pl.Slug]))
		for _, st := range steps[pl.Slug] {
			rows = append(rows, s.planStep(ctx, st, pc))
		}
		if err := s.gateHolds(ctx, rows, pc); err != nil {
			return err
		}
		// The head counts the rows, not the rollup: a step a locked project
		// blocks reads as ready on the request's handle, which holds neither
		// the blocker nor the edge, and its row knows better.
		tl.Counts = planCounts(rows)
		held := 0
		for _, row := range rows {
			if row.Claimable {
				tl.Claimable++
			}
			if row.Word.Key == wordInProgress {
				held++
			}
		}
		tl.Stalled, tl.StalledDays = planStalled(pl.LastActivity, now, pl.Total-pl.Done, held)
		tl.Groups = groupSteps(rows)
		cards = append(cards, tl)
		inPlans += pl.Total - pl.Done
	}
	awaiting, notStarted := noteOnlyCards(notes, hasSteps, slug, ret)
	data.Plans = append(append(awaiting, cards...), notStarted...)

	if data.OffPlan, err = s.offPlanRows(ctx, off, pc); err != nil {
		return err
	}
	data.OffPlan.InPlans = inPlans
	return nil
}

// withNote fills a card's head from its plan's narrative note: the note's
// title, the status in words, the star, and Approve while a capture is still
// a draft or presented (the reader's escape hatch, planApprove).
func (tl *planTimelineVM) withNote(n core.Note) {
	tl.HasNote, tl.NoteID, tl.NoteTitle = true, n.ID, n.Title
	if strings.TrimSpace(n.Title) != "" {
		tl.Title = n.Title
	}
	tl.Status, tl.Favorite = plans.StatusFromTags(n.Tags), n.Favorite
	tl.StatusText = planStatusText(tl.Status)
	tl.Capture = slices.Contains(n.Tags, plans.TagPlan)
	tl.CanApprove = tl.Capture && (tl.Status == plans.StatusDraft || tl.Status == plans.StatusPresented)
}

// planStatusText is a plan status in the owner's words: a presented capture
// awaits approval; every other status reads as itself (draft, approved).
func planStatusText(status string) string {
	if status == plans.StatusPresented {
		return "awaiting approval"
	}
	return status
}

// noteOnlyCards are the cards of the project's captured plans that have no
// step in it yet: those still a draft or presented (awaiting, which lead the
// tab: they wait on the owner) and those approved and not started (which
// follow the plans in motion). Each carries its status and, while it awaits
// approval, the Approve action; none has rows. A settled capture (shipped,
// abandoned, merged) is not a card. Newest note first, by slug on a tie, so a
// morph never reorders them between two renders of the same data.
func noteOnlyCards(notes map[string]core.Note, hasSteps map[string]bool, project, ret string) (awaiting, notStarted []planTimelineVM) {
	for slug, n := range notes {
		if hasSteps[slug] || !slices.Contains(n.Tags, plans.TagPlan) {
			continue
		}
		tl := planTimelineVM{Slug: slug, Project: project, Title: slug, Return: ret, NoteOnly: true, LastActivity: n.Updated}
		tl.withNote(n)
		switch tl.Status {
		case plans.StatusDraft, plans.StatusPresented:
			awaiting = append(awaiting, tl)
		case plans.StatusApproved:
			notStarted = append(notStarted, tl)
		}
	}
	for _, list := range [][]planTimelineVM{awaiting, notStarted} {
		slices.SortFunc(list, func(a, b planTimelineVM) int {
			if c := b.LastActivity.Compare(a.LastActivity); c != 0 {
				return c
			}
			return strings.Compare(a.Slug, b.Slug)
		})
	}
	return awaiting, notStarted
}

// planCounts words a plan's unfinished steps for its head, each step once by
// its word (taskwords.go), in reading order: held, then the in-progress steps
// no live claim holds, what an agent could claim now, what waits on
// unfinished work, and what waits on people, by who, verbatim and sorted. The
// closed steps are the head's own "3/5 done".
func planCounts(rows []planStepVM) []planCountVM {
	order := []string{wordInProgress, wordLapsed, wordUnclaimed, wordReady, wordBlocked, wordWaiting}
	type group struct {
		key, text string
		n         int
	}
	var groups []group
	for _, r := range rows {
		if r.Word.Key == wordDone || r.Word.Key == wordDropped {
			continue
		}
		i := slices.IndexFunc(groups, func(g group) bool { return g.key == r.Word.Key && g.text == r.Word.Text })
		if i < 0 {
			groups = append(groups, group{key: r.Word.Key, text: r.Word.Text})
			i = len(groups) - 1
		}
		groups[i].n++
	}
	rank := func(key string) int {
		if i := slices.Index(order, key); i >= 0 {
			return i
		}
		return len(order) // a word this list does not know reads last
	}
	slices.SortStableFunc(groups, func(a, b group) int {
		if c := rank(a.key) - rank(b.key); c != 0 {
			return c
		}
		return strings.Compare(a.text, b.text)
	})
	out := make([]planCountVM, 0, len(groups))
	for _, g := range groups {
		out = append(out, planCountVM{Key: g.key, Text: strconv.Itoa(g.n) + " " + g.text})
	}
	return out
}

// planStalled reports whether a plan's head marks it stalled (the plan note's
// Stalled state): it has open steps, none held by a live claim, and nothing
// moved for longer than planStalledAfter, the board's threshold, as of now.
// days is for how long, in whole days.
func planStalled(last, now time.Time, open, held int) (stalled bool, days int) {
	if open == 0 || held > 0 || last.IsZero() || now.Sub(last) <= planStalledAfter {
		return false, 0
	}
	return true, int(now.Sub(last) / (24 * time.Hour))
}

// gateHolds fills what each unfinished gate among rows holds: the unfinished
// tasks that depend on it, wherever they are, read through the request's
// handle, so a dependent in a locked project the request does not read is
// absent from the read and never counted. One query per open gate.
func (s *Service) gateHolds(ctx context.Context, rows []planStepVM, pc planStepCtx) error {
	for i := range rows {
		if !rows[i].IsGate || rows[i].State == "done" {
			continue
		}
		held, err := store.TasksBlockedBy(ctx, s.db(ctx), rows[i].ID)
		if err != nil {
			return err
		}
		rows[i].Unblocks, rows[i].UnblocksTitle = nil, ""
		titles := make([]string, 0, len(held))
		for _, d := range held {
			if d.Status.Closed() {
				continue
			}
			rows[i].Unblocks = append(rows[i].Unblocks, depChip(d, pc.onPage[d.ID]))
			titles = append(titles, d.Title)
		}
		rows[i].UnblocksTitle = strings.Join(titles, " · ")
	}
	return nil
}

// lockedBlockersOf is markLockedBlockers' lookup for the Plans & tasks tab:
// the blockers, by locked project, of the page's open rows (its plans' steps
// and its off-plan tasks) that sit in a locked project this request does not
// read. It is nil, and asks nothing, while nothing is locked.
func (s *Service) lockedBlockersOf(ctx context.Context, rows map[string]core.Task) (map[string][]string, error) {
	locked := lockedProjects(ctx)
	if len(locked) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(rows))
	for id, t := range rows {
		if t.Status == core.TaskOpen {
			ids = append(ids, id)
		}
	}
	return store.OpenBlockerProjects(ctx, s.writeDB(), ids, locked)
}

// offPlanTasks is a project's off-plan tasks as the tab reads them: the
// queue's four open groups (the partition in store's tasks_gates.go: ready,
// blocked, the steps that wait on people, and the gates, in progress or
// not), the other tasks in progress, and the most recently closed ones,
// newest closed first, with how many are closed in all.
type offPlanTasks struct {
	ready                   []core.Task
	blocked, waiting, gates []store.BlockedTask
	inProgress, closed      []core.Task
	closedTotal             int
}

// readOffPlan reads a project's off-plan tasks through the request's handle,
// so a locked project's tasks, and the edges to them, are absent from the
// read. Every read leaves plan steps out: the queue's by its scope
// (queueScope), the filtered reads by their filter (store.TaskFilter keeps
// the tasks outside a plan). The tasks in progress come oldest-created
// first, the queue's order, a gate someone started staying with the gates;
// the closed ones are read bounded, the offPlanClosedShown most recently
// closed and how many there are in all.
func (s *Service) readOffPlan(ctx context.Context, slug string) (offPlanTasks, error) {
	db := s.db(ctx)
	var (
		off offPlanTasks
		err error
	)
	if off.ready, err = store.ReadyTasks(ctx, db, slug); err != nil {
		return off, err
	}
	if off.blocked, err = store.BlockedTasks(ctx, db, slug); err != nil {
		return off, err
	}
	if off.waiting, err = store.GatedTasks(ctx, db, slug); err != nil {
		return off, err
	}
	if off.gates, err = store.GateTasks(ctx, db, slug); err != nil {
		return off, err
	}
	f := store.TaskFilter{ByProject: true, Project: slug}
	if off.inProgress, err = store.FilteredStartedTasks(ctx, db, f); err != nil {
		return off, err
	}
	if off.closed, off.closedTotal, err = store.FilteredClosedTasks(ctx, db, f, offPlanClosedShown, 0); err != nil {
		return off, err
	}
	return off, nil
}

// open is every off-plan task still open or in progress, each once.
func (o offPlanTasks) open() []core.Task {
	out := slices.Clone(o.inProgress)
	out = append(out, o.ready...)
	for _, group := range [][]store.BlockedTask{o.blocked, o.waiting, o.gates} {
		out = append(out, blockedTasksOf(group)...)
	}
	return out
}

// blockedTasksOf is the tasks of a queue group, without their blockers: the
// row reads those from the page's dependency walk, as a plan step does.
func blockedTasksOf(group []store.BlockedTask) []core.Task {
	out := make([]core.Task, len(group))
	for i, b := range group {
		out[i] = b.Task
	}
	return out
}

// offPlanRows projects the off-plan tasks into the card's groups, each row
// the plan-step row (planStep). The locked-blocker rule holds here as on the
// plan cards: a ready task a locked project blocks reads as ready on the
// request's handle, which holds neither the blocker nor the edge, so it is
// listed under Blocked, named by that project, and never offered as ready.
// A gate's row says what it holds: the unfinished tasks that depend on it,
// read through the request's handle, wherever they are.
func (s *Service) offPlanRows(ctx context.Context, off offPlanTasks, pc planStepCtx) (offPlanVM, error) {
	rows := func(tasks []core.Task) []planStepVM {
		out := make([]planStepVM, 0, len(tasks))
		for _, t := range tasks {
			out = append(out, s.planStep(ctx, t, pc))
		}
		return out
	}
	var ready, blocked []planStepVM
	for _, row := range rows(off.ready) {
		if len(row.LockedBlockers) > 0 {
			blocked = append(blocked, row)
			continue
		}
		ready = append(ready, row)
	}
	blocked = append(blocked, rows(blockedTasksOf(off.blocked))...)
	slices.SortStableFunc(blocked, func(a, b planStepVM) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	gates := rows(blockedTasksOf(off.gates))
	if err := s.gateHolds(ctx, gates, pc); err != nil {
		return offPlanVM{}, err
	}
	inProgress := rows(off.inProgress)

	var vm offPlanVM
	for _, g := range []offPlanGroup{
		{Key: "in-progress", Label: "In progress", Rows: inProgress, Note: notHeldNote(inProgress),
			Hint: "Marked in progress. Only a live claim holds a task: each row says whether one does."},
		{Key: "ready", Label: "Ready", Rows: ready,
			Hint: "Nothing unfinished before it and no open gate: an agent can claim it now."},
		{Key: "blocked", Label: "Blocked", Rows: blocked,
			Hint: "Waits on unfinished work before it."},
		{Key: "waiting", Label: "Waiting on people", Rows: rows(blockedTasksOf(off.waiting)),
			Hint: "Its own gate is open, or a gate it depends on is: who must act, and what must happen."},
		{Key: "gates", Label: "Gates", Rows: gates,
			Hint: "What a person, or an event outside Seamless, must do before the tasks that depend on it go ahead. Nobody claims a gate."},
	} {
		if len(g.Rows) == 0 {
			continue
		}
		vm.Open += len(g.Rows)
		vm.Groups = append(vm.Groups, g)
	}
	vm.ClosedTotal = off.closedTotal
	vm.Closed = rows(off.closed)
	return vm, nil
}

// notHeldNote says how many of the in-progress rows no live claim holds, by
// their word: the group is what the tasks are marked, and only
// core.Task.ClaimLive makes one held (constraint
// claimtask-claimless-inprogress-and-lease-boundary).
func notHeldNote(rows []planStepVM) string {
	var parts []string
	for _, key := range []string{wordLapsed, wordUnclaimed} {
		n, text := 0, ""
		for _, r := range rows {
			if r.Word.Key == key {
				n, text = n+1, r.Word.Text
			}
		}
		if n > 0 {
			parts = append(parts, strconv.Itoa(n)+" "+text)
		}
	}
	return strings.Join(parts, " · ")
}

// planStep projects a plan-step task into a timeline row, resolving its claim
// (holder + remaining lease, live or lapsed) and both dependency directions.
func (s *Service) planStep(ctx context.Context, t core.Task, pc planStepCtx) planStepVM {
	step := planStepVM{
		ID: t.ID, Title: t.Title, Project: t.ProjectSlug, Plan: t.PlanSlug, Kind: string(t.Kind),
		Account: t.Account, Status: string(t.Status),
		State: taskState(t.Status), Created: t.CreatedAt, IsGate: t.IsGate(), Return: pc.ret,
		deps: t.DependsOn,
	}
	if t.Status == core.TaskDone || t.Status == core.TaskDropped {
		step.Closed = closedAt(t)
	}
	// A held row still names its holder once the lease runs out: that pairing --
	// in_progress, a claimant, an expired lease -- is precisely the row an owner
	// has to intervene on. core.Task.ClaimLive stays the authority on held-ness
	// (constraint claimtask-claimless-inprogress-and-lease-boundary); this only
	// reads it.
	if t.ClaimedBy != "" && t.Status == core.TaskInProgress {
		if h := holderLink(t.ClaimedBy, pc.namer); h.ID != "" {
			step.ClaimedBy, step.ClaimedByID = h.Name, h.ID
		} else {
			step.ClaimedBy = shortID(t.ClaimedBy)
		}
		step.LeaseLapsed = !t.ClaimLive(pc.now)
		if t.LeaseExpiresAt != nil {
			step.LeaseLeft = stepLease(*t.LeaseExpiresAt, !step.LeaseLapsed, pc.now)
			step.LeaseExp = t.LeaseExpiresAt.UTC().Format(time.RFC3339)
		}
		if step.LeaseLapsed {
			step.Alert = wordFor(taskFacts{Status: string(core.TaskInProgress), Holder: true})
		}
	}
	// A gate marked in progress is no claimless start: nobody claims a gate,
	// so it reads as waiting on its people, with no claim strip.
	if t.ClaimedBy == "" && t.Status == core.TaskInProgress && !t.IsGate() {
		step.Claimless = true
		step.Alert = wordFor(taskFacts{Status: string(core.TaskInProgress)})
	}
	if t.Status == core.TaskOpen {
		step.LockedBlockers = pc.lockedBy[t.ID]
		if step.Chain = s.blockingChain(ctx, t, pc); len(step.Chain) > 0 || len(step.LockedBlockers) > 0 {
			step.State = "blocked"
		} else {
			step.Claimable = true
		}
	}
	// The walk above put an open step's blockers in the cache; a blocker it
	// never read counts as finished, as the walk does.
	step.Word = wordFor(factsOf(t, pc.lookup, len(step.LockedBlockers), pc.now))
	if step.Word.Key == wordWaiting {
		// A step a gate holds is not claimable, whatever its blockers say,
		// and its row is waiting -- on people, not on unfinished work -- as
		// is a gate, marked in progress or not. A gate in a locked project
		// is never read, so a step it holds stays blocked, its gate unnamed.
		step.Claimable = false
		step.State = "waiting"
	}
	// A closed step's reverse edges are spent: it already unblocked whatever was
	// waiting, so saying so on the row is noise rather than a cue.
	if step.State == "done" {
		return step
	}
	step.Waits = waitsOf(t, pc.lookup)
	if step.Unblocks = pc.unblocks[t.ID]; len(step.Unblocks) > 0 {
		titles := make([]string, 0, len(step.Unblocks))
		for _, u := range step.Unblocks {
			titles = append(titles, u.Title)
		}
		step.UnblocksTitle = strings.Join(titles, " · ")
	}
	return step
}

// waitsOf is who and what the open gates holding t wait on: its own gate
// first, then each unfinished gate it depends on that lookup answers (the
// gates factsOf words the row by, in its order). A gate in a locked project
// the request does not read is never answered, so never named.
func waitsOf(t core.Task, lookup func(string) (core.Task, bool)) []gateLineVM {
	var out []gateLineVM
	if t.Gate.Open() {
		out = append(out, gateLineVM{Who: t.Gate.Who, What: t.Gate.What})
	}
	for _, id := range t.DependsOn {
		b, ok := lookup(id)
		if !ok || b.Status.Closed() || !b.IsGate() || !b.Gate.Open() {
			continue
		}
		out = append(out, gateLineVM{Who: b.Gate.Who, What: b.Gate.What})
	}
	return out
}

// stepLease is a step row's lease cell: the time left while the claim is
// live ("12m left": the row's timer icon says lease), "lapsed 3m ago" once it
// ran out. The client ticker keeps it current between refreshes.
func stepLease(exp time.Time, live bool, now time.Time) string {
	if live {
		return leaseLeft(exp, now)
	}
	return "lapsed " + agoPhraseAt(exp, now)
}

// lookup answers a task the page's cache holds: its plan steps and the
// blockers the dependency walk read.
func (pc planStepCtx) lookup(id string) (core.Task, bool) {
	t, ok := pc.cache[id]
	return t, ok
}

// taskState is the CSS row modifier for a task status. "blocked" and
// "waiting" are not statuses: planStep derives them per row, from the
// dependency walk and the row's word.
func taskState(status core.TaskStatus) string {
	switch status {
	case core.TaskDone, core.TaskDropped:
		return "done"
	case core.TaskInProgress:
		return "doing"
	default:
		return "open"
	}
}

// depChip projects a task into a dependency-edge chip.
func depChip(t core.Task, onPage bool) depChipVM {
	return depChipVM{
		ID: t.ID, Title: t.Title, Status: string(t.Status),
		State: taskState(t.Status), OnPage: onPage,
	}
}

// blockingChain walks an open step's unfinished dependencies breadth-first and
// returns them as chips, nearest blocker first. The walk is depth-capped and
// cycle-guarded: nothing guarantees this graph is acyclic, and one bad edge
// must not hang a page render. A dangling edge costs its chip, not the page.
func (s *Service) blockingChain(ctx context.Context, t core.Task, pc planStepCtx) []depChipVM {
	var chain []depChipVM
	seen := map[string]bool{t.ID: true}
	frontier := t.DependsOn
	for depth := 0; depth < planChainDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, depID := range frontier {
			if seen[depID] {
				continue
			}
			seen[depID] = true
			dep, ok := pc.cache[depID]
			if !ok {
				d, err := store.TaskByID(ctx, s.db(ctx), depID)
				if err != nil {
					continue
				}
				dep, pc.cache[depID] = d, d
			}
			if dep.Status != core.TaskOpen && dep.Status != core.TaskInProgress {
				continue
			}
			chip := depChip(dep, pc.onPage[dep.ID])
			chip.Word = wordFor(factsOf(dep, pc.lookup, 0, pc.now))
			chain = append(chain, chip)
			next = append(next, dep.DependsOn...)
		}
		frontier = next
	}
	return chain
}

// reverseEdges inverts the dependency edges across a page's step set: for each
// step, the steps waiting on it. Only in-set edges count -- the point is a chip
// that jumps somewhere already on this page -- so this needs no query at all.
// Map iteration is unordered, hence the title sort: the rendered chips must not
// shuffle between two renders of the same data (a morph would rewrite them).
func reverseEdges(steps map[string]core.Task) map[string][]depChipVM {
	out := map[string][]depChipVM{}
	for _, t := range steps {
		for _, depID := range t.DependsOn {
			if _, ok := steps[depID]; !ok {
				continue
			}
			out[depID] = append(out[depID], depChip(t, true))
		}
	}
	for id := range out {
		sort.SliceStable(out[id], func(i, j int) bool { return out[id][i].Title < out[id][j].Title })
	}
	return out
}

// groupSteps orders a plan frontier-first: every unfinished step in
// dependency order -- a step below each unfinished step of the plan it
// depends on, and otherwise in the plan's own step order (oldest created
// first, the order it was written: stepOrder) -- then ONE group holding every
// closed step, most recently closed first. Source order is creation-time
// descending, which scattered finished work above and below the frontier --
// history belongs in one place, at the bottom. A closed block shorter than
// doneRunFold is not worth hiding, so it stays expanded. Pure, and totally
// ordered: a morph must not shuffle rows between two renders of the same data.
func groupSteps(steps []planStepVM) []planStepGroup {
	live, closed := make([]planStepVM, 0, len(steps)), make([]planStepVM, 0, len(steps))
	for _, st := range steps {
		if st.State == "done" {
			closed = append(closed, st)
			continue
		}
		live = append(live, st)
	}
	live = stepOrder(live)
	sort.Slice(closed, func(i, j int) bool {
		if !closed[i].Closed.Equal(closed[j].Closed) {
			return closed[i].Closed.After(closed[j].Closed)
		}
		return closed[i].ID > closed[j].ID
	})

	out := make([]planStepGroup, 0, len(live)+1)
	single := func(st planStepVM) { out = append(out, planStepGroup{Steps: []planStepVM{st}}) }
	for _, st := range live {
		single(st)
	}
	switch {
	case len(closed) == 0:
		return out
	case len(closed) < doneRunFold:
		for _, st := range closed {
			single(st)
		}
		return out
	}
	// closed[0] is the most recently closed step: the fold's label.
	return append(out, planStepGroup{
		DoneRun: true, Steps: closed,
		LastTitle: closed[0].Title, LastClosed: closed[0].Closed,
	})
}

// stepOrder puts a plan's unfinished steps in dependency order: each after
// every one of them it depends on, ties (and the order where nothing
// depends) by creation, oldest first, then id. The dependency graph is not
// guaranteed acyclic: when every step left waits on another left, the
// oldest of them goes next, so one bad edge costs the order, never a row.
func stepOrder(live []planStepVM) []planStepVM {
	sort.Slice(live, func(i, j int) bool {
		if !live[i].Created.Equal(live[j].Created) {
			return live[i].Created.Before(live[j].Created)
		}
		return live[i].ID < live[j].ID
	})
	in := make(map[string]bool, len(live))
	for _, st := range live {
		in[st.ID] = true
	}
	placed := make(map[string]bool, len(live))
	waiting := func(st planStepVM) bool {
		for _, d := range st.deps {
			if in[d] && !placed[d] && d != st.ID {
				return true
			}
		}
		return false
	}
	out := make([]planStepVM, 0, len(live))
	for len(out) < len(live) {
		next := -1
		for i, st := range live {
			if placed[st.ID] {
				continue
			}
			if next < 0 {
				next = i // the oldest left: the cycle breaker
			}
			if !waiting(st) {
				next = i
				break
			}
		}
		placed[live[next].ID] = true
		out = append(out, live[next])
	}
	return out
}

// planNoteMeta resolves each plan slug's narrative note in one query. The note
// FILE is deliberately not read: the tab needs the title, status and star, all
// of which the index row already carries, and reading N plan files per page
// load is exactly the N+1 the Plans library pays for its iteration column.
func (s *Service) planNoteMeta(ctx context.Context, project string) (map[string]core.Note, error) {
	notes, err := store.NotesByTagPrefix(ctx, s.db(ctx), project, plans.SlugTagPrefix())
	if err != nil {
		return nil, err
	}
	primary := make(map[string]core.Note, len(notes))
	for _, n := range notes {
		if slices.Contains(n.Tags, plans.TagAgent) {
			continue // an agent cache is in the composition, never its narrative
		}
		slug := plans.SlugFromTags(n.Tags)
		if slug == "" {
			continue
		}
		if cur, ok := primary[slug]; !ok || betterPrimary(n, cur) {
			primary[slug] = n
		}
	}
	return primary, nil
}

// betterPrimary reports whether note a represents a plan over b: a Claude Code
// capture is the primary of its slug, otherwise the earlier note wins -- the
// same rule the Plans board applies (planEntries).
func betterPrimary(a, b core.Note) bool {
	aCap, bCap := slices.Contains(a.Tags, plans.TagPlan), slices.Contains(b.Tags, plans.TagPlan)
	if aCap != bCap {
		return aCap
	}
	return plans.EarlierPrimary(a, b)
}

// planLedger reads the project's recent task transitions once and buckets the
// newest few per plan. Events carry no plan slug, so the plan filter is
// Go-side against the page's step set; the project filter is the read's own
// (ProjectEvents), so a quiet project's transitions are there however busy
// the fleet is. Best-effort: a feed error costs the ledger line, not the tab.
func (s *Service) planLedger(ctx context.Context, project string, steps map[string]core.Task, namer func(string) core.Session) map[string][]planLedgerVM {
	if s.cfg.Events == nil {
		return nil
	}
	evs, err := s.eventLog(ctx).ProjectEvents(ctx, project, []core.EventKind{core.EventTaskTransition}, nil, planLedgerScan)
	if err != nil {
		s.logger.Warn("console: plan ledger", "project", project, "error", err)
		return nil
	}
	out := map[string][]planLedgerVM{}
	// One entry per STEP, not per event: a step that was created, claimed and
	// finished inside the window is one thing that happened, and spending all
	// three ledger slots on its title repeated verbatim says nothing. Events
	// arrive newest first, so the first sighting of a step is its latest state.
	seen := map[string]bool{}
	for _, e := range evs {
		if seen[e.ItemID] {
			continue
		}
		step, ok := steps[e.ItemID]
		if !ok || step.PlanSlug == "" || len(out[step.PlanSlug]) >= planLedgerRows {
			continue
		}
		seen[e.ItemID] = true
		row := planLedgerVM{
			StepID: step.ID, StepTitle: step.Title, When: e.TS,
			Owner: payloadStr(e.Payload, "by") == "console",
		}
		row.Verb, row.Icon, row.Tone = transitionVerb(e.Payload)
		// A claim stamps its holder in the payload; every other transition
		// carries the acting session on the event itself.
		actor := e.SessionID
		if actor == "" {
			actor = payloadStr(e.Payload, "claimed_by")
		}
		if sess := namer(actor); sess.ID != "" {
			row.Session, row.SessionID = sess.Name, sess.ID
		}
		out[step.PlanSlug] = append(out[step.PlanSlug], row)
	}
	return out
}

// transitionVerb reads a task.transition payload as the verb the ledger shows,
// covering the shapes tasks_add / tasks_claim / tasks_release / tasks_update
// and the console's own force-release write emit.
func transitionVerb(payload map[string]any) (verb, iconName, tone string) {
	switch {
	case payloadBool(payload, "created"):
		return "created", "circle", "muted"
	case payloadBool(payload, "reclaimed"):
		return "reclaimed", "terminal", "warn"
	case payloadStr(payload, "claimed_by") != "":
		return "claimed", "terminal", "brand"
	case payloadBool(payload, "released"):
		return "released", "arrow-up-down", "muted"
	case blockersRemovedOnly(payload):
		if len(payloadStrList(payload, "removed_depends_on")) > 1 {
			return "blockers removed", "link", "muted"
		}
		return "blocker removed", "link", "muted"
	case statusUnmoved(payload):
		return "updated", "pencil", "muted"
	}
	switch payloadStr(payload, "to") {
	case string(core.TaskDone):
		return "done", "check", "ok"
	case string(core.TaskDropped):
		return "dropped", "archive", "muted"
	case string(core.TaskInProgress):
		return "started", "loader", "brand"
	default:
		return "reopened", "circle", "warn"
	}
}

// statusUnmoved reports a tasks_update that moved no status -- an edit, a new
// or removed blocker, a commit, a project move: it carries its status before
// the call as from, and from equals to, so the status words ("reopened",
// "done") would name a move that did not happen. An event without from (every
// one written before tasks_update recorded it, and every other writer's) is
// not judged here and reads as it always has.
func statusUnmoved(payload map[string]any) bool {
	from := payloadStr(payload, "from")
	return from != "" && from == payloadStr(payload, "to")
}

// blockersRemovedOnly reports a statusUnmoved update that removed blockers.
func blockersRemovedOnly(payload map[string]any) bool {
	return statusUnmoved(payload) && len(payloadStrList(payload, "removed_depends_on")) > 0
}

// stalePlanSlugs flags the plans a pending gardener settlement names -- the
// ship-plan / abandon-plan proposals /console/gardener is already showing.
// This reuses that queue rather than re-running any git stamp check per page
// load. One query; the badge is decoration, so an error costs it and nothing
// else.
func (s *Service) stalePlanSlugs(ctx context.Context, project string) map[string]bool {
	proposals, err := store.PendingProposals(ctx, s.db(ctx), "")
	if err != nil {
		s.logger.Warn("console: pending plan settlements", "project", project, "error", err)
		return nil
	}
	out := map[string]bool{}
	for _, p := range proposals {
		switch p.Kind {
		case store.ProposalShipPlan, store.ProposalAbandonPlan, store.ProposalMergePlans:
		default:
			continue
		}
		// The payload carries the captured note's project; a global note still
		// settles a plan whose steps live here.
		if proj := payloadStr(p.Payload, "project"); proj != "" && proj != project {
			continue
		}
		if slug := payloadStr(p.Payload, "slug"); slug != "" {
			out[slug] = true
		}
	}
	return out
}

// fillSessionsTab loads the Sessions panel: the project's sessions counted by
// state for the header, and the newest projectSessionsShown of them, newest
// first, each live session with the tasks it holds a live claim on, by title.
func (s *Service) fillSessionsTab(ctx context.Context, data *projectWorkspaceData, slug string) error {
	now := time.Now().UTC()
	tally, err := store.ProjectSessionTallyAt(ctx, s.db(ctx), slug, now, s.cfg.SessionIdleTTL)
	if err != nil {
		return err
	}
	data.SessionTally = tally
	sessions, err := store.ListSessionsForProject(ctx, s.db(ctx), slug, "", time.Time{}, projectSessionsShown)
	if err != nil {
		return err
	}
	// Two reads for the whole list (the held tasks and the first prompts),
	// through the request's handle.
	heads := s.sessionHeadlines(ctx, sessions, now)
	for _, sess := range sessions {
		h := heads[sess.ID]
		vm := projSessionVM{
			ID: sess.ID, Name: sessionLabel(sess), Status: string(sess.Status),
			Source: sess.Source, Account: sess.Account, Ambient: sess.Ambient,
			Live:     sess.LiveAsOf(now, s.cfg.SessionIdleTTL),
			Findings: snippet(markdown.PlainText(sess.Findings), 140), Updated: sess.UpdatedAt,
			Headline: h.Text, HeadlineFrom: h.From,
		}
		vm.State = sessionState(sess, vm.Live)
		for _, t := range h.Held {
			vm.Holds = append(vm.Holds, heldTaskVM{ID: t.ID, Title: t.Title})
		}
		data.Sessions = append(data.Sessions, vm)
	}
	return nil
}

// sessionState is a session's word on the Sessions tab, the Sessions screen's
// buckets: live, idle, completed or expired.
func sessionState(sess core.Session, live bool) string {
	switch {
	case live:
		return "live"
	case sess.Status == core.SessionActive:
		return "idle"
	default:
		return string(sess.Status)
	}
}

// fillMemoriesTab loads the Memories panel: the project's strict memories
// (active + superseded, with lineage) and, as a separate labeled group, the
// global/parent memories it inherits -- so the tab count reconciles with the
// board's strict per-slug count instead of silently disagreeing.
func (s *Service) fillMemoriesTab(ctx context.Context, data *projectWorkspaceData, slug string) error {
	strict, err := store.ProjectMemoriesIncludingInvalid(ctx, s.db(ctx), slug)
	if err != nil {
		return err
	}
	resolve := s.sourceSessionResolver(ctx)
	for _, m := range strict {
		data.Memories = append(data.Memories, s.projMemory(ctx, m, resolve))
	}
	active, err := store.ActiveMemories(ctx, s.db(ctx), slug)
	if err != nil {
		return err
	}
	for _, m := range active {
		if m.Project == slug {
			continue // strict rows already shown above
		}
		data.Inherited = append(data.Inherited, s.projMemory(ctx, m, resolve))
	}
	return nil
}

// projMemory projects a memory into a Memories-tab row, building its lineage
// cell (resolve names its source session) and joining its retrieval stats.
func (s *Service) projMemory(ctx context.Context, m core.Memory, resolve func(string) core.Session) projMemoryVM {
	vm := projMemoryVM{
		ID: m.ID, Name: m.Name, Kind: string(m.Kind), Desc: m.Description,
		Account: m.Account, Updated: m.Updated, Superseded: !m.Active(),
		Lineage: s.memLineage(ctx, m, resolve),
	}
	if stat, ok, err := store.GetRetrievalStat(ctx, s.db(ctx), m.ID); err == nil && ok {
		vm.Injects, vm.Reads = stat.InjectCount, stat.ReadCount
	}
	return vm
}

// memLineage renders a memory's provenance cell: a supersession pointer when the
// memory was replaced, else its source session by name (sourceSessionLabel),
// else nothing.
func (s *Service) memLineage(ctx context.Context, m core.Memory, resolve func(string) core.Session) template.HTML {
	if m.SupersededBy != "" {
		name := m.SupersededBy
		if by, ok, err := store.MemoryByID(ctx, s.db(ctx), m.SupersededBy); err == nil && ok {
			name = by.Name
		}
		return template.HTML(`<span class="lineage" style="color:var(--pop-strong)">` +
			string(icon("arrow-right")) + `superseded by ` + template.HTMLEscapeString(name) + `</span>`)
	}
	if m.SourceSession != "" {
		return template.HTML(`<span class="lineage">` + string(icon("git-commit-horizontal")) +
			`from ` + template.HTMLEscapeString(sourceSessionLabel(resolve, m.SourceSession)) + `</span>`)
	}
	return template.HTML(`<span class="lineage faint">&mdash;</span>`)
}

// fillNotesTab loads the Notes panel: the project's notes (a Go-side filter of
// the note index, newest first).
func (s *Service) fillNotesTab(ctx context.Context, data *projectWorkspaceData, slug string) error {
	notes, err := store.ListNotes(ctx, s.db(ctx))
	if err != nil {
		return err
	}
	for _, n := range notes {
		if n.Project != slug {
			continue
		}
		data.Notes = append(data.Notes, projNoteVM{
			ID: n.ID, Title: n.Title, Desc: n.Description, Account: n.Account, Tags: n.Tags, Updated: n.Updated,
		})
	}
	sort.SliceStable(data.Notes, func(i, j int) bool {
		return data.Notes[i].Updated.After(data.Notes[j].Updated)
	})
	return nil
}

// fillInteractionsTab loads the Interactions panel: the project's own
// transport events (ProjectEvents over interactionKinds), newest first.
func (s *Service) fillInteractionsTab(ctx context.Context, data *projectWorkspaceData, slug string) error {
	if s.cfg.Events == nil {
		return nil
	}
	evs, err := s.eventLog(ctx).ProjectEvents(ctx, slug, interactionKinds, nil, interactionsPageLimit)
	if err != nil {
		return err
	}
	name := s.sessionNamer(ctx)
	for _, e := range evs {
		if skipInteraction(e) {
			continue
		}
		data.Interactions = append(data.Interactions, toInteractionRow(e, name))
		if len(data.Interactions) >= 40 {
			break
		}
	}
	// Project-scoped volume histogram over all of the project's history; embedded
	// as compact JSON for the shared IX.renderVolume client renderer.
	if vol, err := s.interactionVolume(ctx, slug, 0); err != nil {
		return err
	} else if len(vol) > 0 {
		if b, err := json.Marshal(vol); err == nil {
			data.IxVolumeJSON = string(b)
		}
	}
	return nil
}
