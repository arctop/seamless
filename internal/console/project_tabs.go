package console

// The project-detail workspace's tab panels. Each fillXTab loads exactly one
// panel into projectWorkspaceData and owns that tab's view models; the page
// assembly lives in project_detail.go.

import (
	"context"
	"encoding/json"
	"html/template"
	"slices"
	"sort"
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
// planChainDepth caps the blocker walk.
const (
	doneRunFold    = 3
	donePlansCap   = 20
	planLedgerScan = 300
	planLedgerRows = 3
	planChainDepth = 3
)

// planTimelineVM is one plan's card on the Plans & tasks tab: the rollup
// counts, the composition's narrative note (status chip + star), the last few
// transitions, and the step rows grouped so long done runs fold away.
type planTimelineVM struct {
	Slug         string
	Total        int
	Done         int
	InFlight     int
	Open         int
	Claimable    int
	DonePct      int
	DoingPct     int
	LastActivity time.Time

	// The composition's primary note. A plan whose steps exist without one
	// degrades to no chip and no star rather than to empty ones.
	HasNote   bool
	NoteID    string
	NoteTitle string
	Status    string
	Favorite  bool

	// Stale marks a plan a pending gardener settlement (ship/abandon) names.
	Stale bool

	Recent []planLedgerVM
	Groups []planStepGroup
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
}

// planStepVM is one step in a plan timeline: its status, claim/lease, and both
// dependency directions. State is the CSS row modifier (done/doing/blocked/
// open); it is derived, never stored.
type planStepVM struct {
	ID        string
	Title     string
	Project   string // the row's own actions post back to this project's tab
	Status    string
	State     string
	Claimable bool      // open and unblocked: an agent could take it right now
	Created   time.Time // the plan's own step order, oldest first
	Closed    time.Time // when it reached a terminal status (the done-run label)

	ClaimedBy   string // session name
	ClaimedByID string
	LeaseLeft   string // server-rendered remaining lease, the no-JS fallback
	LeaseExp    string // RFC3339 expiry, for the client ticker
	LeaseLapsed bool   // held by a session whose lease ran out: reclaimable

	Chain         []depChipVM // unfinished blockers, nearest first
	Unblocks      []depChipVM // steps waiting on this one
	UnblocksTitle string      // those steps named, for the chip's tooltip
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
}

// projSessionVM is one row on the Sessions tab.
type projSessionVM struct {
	ID       string
	Name     string
	Status   string
	Source   string
	Ambient  bool
	Active   bool
	Findings string
	Holds    []string // labels of tasks the session currently claims
	Updated  time.Time
}

// projMemoryVM is one row on the Memories tab, carrying a rendered lineage cell
// (provenance session, or a supersession pointer).
type projMemoryVM struct {
	ID         string
	Name       string
	Kind       string
	Desc       string
	Lineage    template.HTML
	Injects    int
	Reads      int
	Updated    time.Time
	Superseded bool
}

// projNoteVM is one row on the Notes tab.
type projNoteVM struct {
	ID      string
	Title   string
	Desc    string
	Tags    []string
	Updated time.Time
}

// fillOverviewTab loads the Overview panel: memories-by-kind (project's active
// memories), this project's injection trend (its own memories, matching the
// board reach), and the project's recent activity.
func (s *Service) fillOverviewTab(ctx context.Context, data *projectWorkspaceData, slug string, win store.RetrievalWindow) error {
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
		evs, err := s.eventLog(ctx).RecentExcluding(ctx, 120, s.ledgerExcludedKinds(ctx)...)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if e.ProjectSlug != slug {
				continue
			}
			data.Recent = append(data.Recent, toEventRow(e))
			if len(data.Recent) >= 8 {
				break
			}
		}
	}
	return nil
}

// fillTasksTab loads the Plans & tasks panel: each active plan's live card
// (rollup, narrative note, recent-transition ledger, staleness) with its step
// timeline, the completed plans as rollup lines, and the off-plan ready queue.
//
// Query budget: one rollup query, one ListTasksForPlan per ACTIVE plan (a
// completed plan renders from its rollup alone), then exactly one query each
// for the plan notes, the transition ledger and the pending settlements. No
// per-step or per-plan lookup beyond the dependency walk's cache misses.
func (s *Service) fillTasksTab(ctx context.Context, data *projectWorkspaceData, slug string) error {
	rollups, err := store.PlanRollupsForProject(ctx, s.db(ctx), slug)
	if err != nil {
		return err
	}
	var active []store.PlanRollup
	for _, pl := range rollups {
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
	onPage := make(map[string]bool, len(cache))
	for id := range cache {
		onPage[id] = true
	}
	namer := s.sessionNamer(ctx)
	// Ordering matters: both of these read the cache while it still holds only
	// this page's plan steps -- the dependency walk below adds their blockers.
	unblocks := reverseEdges(cache)
	ledger := s.planLedger(ctx, slug, cache, namer)

	notes, err := s.planNoteMeta(ctx, slug)
	if err != nil {
		return err
	}
	stale := s.stalePlanSlugs(ctx, slug)

	pc := planStepCtx{cache: cache, onPage: onPage, unblocks: unblocks, namer: namer, now: time.Now().UTC()}
	for _, pl := range active {
		tl := planTimelineVM{
			Slug: pl.Slug, Total: pl.Total, Done: pl.Done, InFlight: pl.InFlight,
			Open:      pl.Total - pl.Done - pl.InFlight,
			Claimable: pl.Claimable, LastActivity: pl.LastActivity,
			DonePct: percent(pl.Done, pl.Total), DoingPct: percent(pl.InFlight, pl.Total),
			Stale:  stale[pl.Slug],
			Recent: ledger[pl.Slug],
		}
		if n, ok := notes[pl.Slug]; ok {
			tl.HasNote, tl.NoteID, tl.NoteTitle = true, n.ID, n.Title
			tl.Status, tl.Favorite = plans.StatusFromTags(n.Tags), n.Favorite
		}
		rows := make([]planStepVM, 0, len(steps[pl.Slug]))
		for _, st := range steps[pl.Slug] {
			rows = append(rows, s.planStep(ctx, st, pc))
		}
		tl.Groups = groupSteps(rows)
		data.Plans = append(data.Plans, tl)
	}

	ready, err := store.ReadyTasks(ctx, s.db(ctx), slug)
	if err != nil {
		return err
	}
	data.Ready = taskRows(ready)
	return nil
}

// planStep projects a plan-step task into a timeline row, resolving its claim
// (holder + remaining lease, live or lapsed) and both dependency directions.
func (s *Service) planStep(ctx context.Context, t core.Task, pc planStepCtx) planStepVM {
	step := planStepVM{
		ID: t.ID, Title: t.Title, Project: t.ProjectSlug, Status: string(t.Status),
		State: taskState(t.Status), Created: t.CreatedAt,
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
		if sess := pc.namer(t.ClaimedBy); sess.ID != "" {
			step.ClaimedBy, step.ClaimedByID = sess.Name, sess.ID
		}
		if t.LeaseExpiresAt != nil {
			step.LeaseLeft = durUntil(*t.LeaseExpiresAt, pc.now)
			step.LeaseExp = t.LeaseExpiresAt.UTC().Format(time.RFC3339)
		}
		step.LeaseLapsed = !t.ClaimLive(pc.now)
	}
	if t.Status == core.TaskOpen {
		if step.Chain = s.blockingChain(ctx, t, pc); len(step.Chain) > 0 {
			step.State = "blocked"
		} else {
			step.Claimable = true
		}
	}
	// A closed step's reverse edges are spent: it already unblocked whatever was
	// waiting, so saying so on the row is noise rather than a cue.
	if step.State == "done" {
		return step
	}
	if step.Unblocks = pc.unblocks[t.ID]; len(step.Unblocks) > 0 {
		titles := make([]string, 0, len(step.Unblocks))
		for _, u := range step.Unblocks {
			titles = append(titles, u.Title)
		}
		step.UnblocksTitle = strings.Join(titles, " · ")
	}
	return step
}

// taskState is the CSS row modifier for a task status. "blocked" is not a
// status: it is derived per row from the dependency walk.
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
			chain = append(chain, depChip(dep, pc.onPage[dep.ID]))
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

// groupSteps orders a plan frontier-first: every unfinished step, in the plan's
// own step order (oldest created first, the order it was written, so a blocker
// reads above what it blocks), then ONE group holding every closed step, most
// recently closed first. Source order is creation-time descending, which
// scattered finished work above and below the frontier -- history belongs in one
// place, at the bottom. A closed block shorter than doneRunFold is not worth
// hiding, so it stays expanded. Pure, and totally ordered: a morph must not
// shuffle rows between two renders of the same data.
func groupSteps(steps []planStepVM) []planStepGroup {
	live, closed := make([]planStepVM, 0, len(steps)), make([]planStepVM, 0, len(steps))
	for _, st := range steps {
		if st.State == "done" {
			closed = append(closed, st)
			continue
		}
		live = append(live, st)
	}
	sort.Slice(live, func(i, j int) bool {
		if !live[i].Created.Equal(live[j].Created) {
			return live[i].Created.Before(live[j].Created)
		}
		return live[i].ID < live[j].ID
	})
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
// same rule composedPrimaries applies on the Plans library.
func betterPrimary(a, b core.Note) bool {
	aCap, bCap := slices.Contains(a.Tags, plans.TagPlan), slices.Contains(b.Tags, plans.TagPlan)
	if aCap != bCap {
		return aCap
	}
	return plans.EarlierPrimary(a, b)
}

// planLedger reads the recent task transitions once and buckets the newest few
// per plan. Events carry no plan slug, so the filter is Go-side against the
// page's step set -- the same shape fillOverviewTab uses for RecentExcluding.
// Best-effort: a feed error costs the ledger line, not the tab.
func (s *Service) planLedger(ctx context.Context, project string, steps map[string]core.Task, namer func(string) core.Session) map[string][]planLedgerVM {
	if s.cfg.Events == nil {
		return nil
	}
	evs, err := s.eventLog(ctx).ByKinds(ctx, []core.EventKind{core.EventTaskTransition}, "", "", planLedgerScan)
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
		if e.ProjectSlug != project || seen[e.ItemID] {
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
		return "reclaimed", "lock", "warn"
	case payloadStr(payload, "claimed_by") != "":
		return "claimed", "lock", "brand"
	case payloadBool(payload, "released"):
		return "released", "arrow-up-down", "muted"
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

// fillSessionsTab loads the Sessions panel: the project's sessions newest first,
// with each active session's held tasks resolved for an inline claim indicator.
func (s *Service) fillSessionsTab(ctx context.Context, data *projectWorkspaceData, slug string) error {
	sessions, err := store.ListSessionsForProject(ctx, s.db(ctx), slug, "", time.Time{}, 60)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, sess := range sessions {
		vm := projSessionVM{
			ID: sess.ID, Name: sess.Name, Status: string(sess.Status),
			Source: sess.Source, Ambient: sess.Ambient,
			Active:   sess.Status == core.SessionActive,
			Findings: snippet(markdown.PlainText(sess.Findings), 140), Updated: sess.UpdatedAt,
		}
		if vm.Active {
			if held, err := store.TasksClaimedBy(ctx, s.db(ctx), sess.ID); err == nil {
				for _, t := range held {
					if t.ClaimLive(now) {
						vm.Holds = append(vm.Holds, taskLabel(t))
					}
				}
			}
		}
		data.Sessions = append(data.Sessions, vm)
	}
	return nil
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
	for _, m := range strict {
		data.Memories = append(data.Memories, s.projMemory(ctx, m))
	}
	active, err := store.ActiveMemories(ctx, s.db(ctx), slug)
	if err != nil {
		return err
	}
	for _, m := range active {
		if m.Project == slug {
			continue // strict rows already shown above
		}
		data.Inherited = append(data.Inherited, s.projMemory(ctx, m))
	}
	return nil
}

// projMemory projects a memory into a Memories-tab row, building its lineage
// cell and joining its retrieval stats.
func (s *Service) projMemory(ctx context.Context, m core.Memory) projMemoryVM {
	vm := projMemoryVM{
		ID: m.ID, Name: m.Name, Kind: string(m.Kind), Desc: m.Description,
		Updated: m.Updated, Superseded: !m.Active(),
		Lineage: s.memLineage(ctx, m),
	}
	if stat, ok, err := store.GetRetrievalStat(ctx, s.db(ctx), m.ID); err == nil && ok {
		vm.Injects, vm.Reads = stat.InjectCount, stat.ReadCount
	}
	return vm
}

// memLineage renders a memory's provenance cell: a supersession pointer when the
// memory was replaced, else its source session, else nothing.
func (s *Service) memLineage(ctx context.Context, m core.Memory) template.HTML {
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
			`from ` + template.HTMLEscapeString(m.SourceSession) + `</span>`)
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
			ID: n.ID, Title: n.Title, Desc: n.Description, Tags: n.Tags, Updated: n.Updated,
		})
	}
	sort.SliceStable(data.Notes, func(i, j int) bool {
		return data.Notes[i].Updated.After(data.Notes[j].Updated)
	})
	return nil
}

// fillInteractionsTab loads the Interactions panel: the transport feed filtered
// to this project's events (interactionRow already carries the project slug), so
// no new store query is needed.
func (s *Service) fillInteractionsTab(ctx context.Context, data *projectWorkspaceData, slug string) error {
	if s.cfg.Events == nil {
		return nil
	}
	evs, err := s.eventLog(ctx).ByKinds(ctx, interactionKinds, "", "", interactionsPageLimit)
	if err != nil {
		return err
	}
	name := s.sessionNamer(ctx)
	for _, e := range evs {
		if e.ProjectSlug != slug || skipInteraction(e) {
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

// taskLabel is a short human label for a claimed task: its plan-step marker when
// it has a plan, else a truncated id.
func taskLabel(t core.Task) string {
	if t.PlanSlug != "" {
		return t.PlanSlug + " step"
	}
	return shortID(t.ID)
}
