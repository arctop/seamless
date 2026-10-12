package console

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/store"
)

// errNoGardener is returned when a gardener action is requested but the console
// was built without a gardener service.
var errNoGardener = errors.New("console: gardener unavailable")

// memBrief is a compact memory descriptor inside a proposal card.
type memBrief struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Project     string `json:"project"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// reprojectView is the reproject-specific projection of a proposal card: a memory
// moved from one project to another.
type reprojectView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	From      string `json:"from"`
	To        string `json:"to"`
	Rationale string `json:"rationale,omitempty"`
}

// relocateView is the relocate-specific projection: a GLOBAL memory pulled back
// inside the project whose sessions wrote it, after that project tightened its
// isolation. The move is a reproject's; the evidence is provenance, which is why
// it renders on its own terms -- Session names the stamp that traced it, and
// Isolation the fence it escaped.
type relocateView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind,omitempty"`
	Description string `json:"description,omitempty"`
	To          string `json:"to"`
	Session     string `json:"session,omitempty"`
	Isolation   string `json:"isolation,omitempty"`
}

// rekindView is the rekind-specific projection of a proposal card: a memory
// reclassified from one kind to another, in place.
type rekindView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Project   string `json:"project,omitempty"`
	From      string `json:"from"`
	To        string `json:"to"`
	Rationale string `json:"rationale,omitempty"`
}

// splitView is the split-setup projection: the source project and the child /
// shared projects it will be split into.
type splitView struct {
	Source       string       `json:"source"`
	Children     []projectOpt `json:"children"`
	Shared       *projectOpt  `json:"shared,omitempty"`
	Family       string       `json:"family,omitempty"`
	RetireSource bool         `json:"retireSource"`
}

// proposalCard is a display projection of one gardener proposal.
type proposalCard struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Label       string    `json:"label"`
	Eyebrow     string    `json:"eyebrow"`
	Icon        string    `json:"icon"`
	Tone        string    `json:"tone"`
	Source      string    `json:"source,omitempty"`      // "request" => owner asked for it
	RequestText string    `json:"requestText,omitempty"` // the owner ask that produced the proposal
	Plan        string    `json:"plan,omitempty"`        // plan slug => part of a reviewed batch (e.g. a split)
	CreatedAt   time.Time `json:"createdAt"`

	// Archive
	Archive *memBrief `json:"archive,omitempty"`
	Reason  string    `json:"reason,omitempty"`

	// Merge
	Score        float64   `json:"score,omitempty"`
	ScorePercent int       `json:"scorePercent,omitempty"`
	Keep         *memBrief `json:"keep,omitempty"`
	Drop         *memBrief `json:"drop,omitempty"`

	// Digest. Preview is the raw body text (JSON); Body is the full rendered
	// markdown the card shows.
	Project      string        `json:"project,omitempty"`
	Month        string        `json:"month,omitempty"`
	SessionCount int           `json:"sessionCount,omitempty"`
	Title        string        `json:"title,omitempty"`
	Preview      string        `json:"preview,omitempty"`
	Body         template.HTML `json:"-"`

	// Consolidate: a new unified memory (NewName/NewKind/NewDesc + rendered Body,
	// which reuses the digest's Body field) that supersedes Sources.
	NewName string     `json:"newName,omitempty"`
	NewKind string     `json:"newKind,omitempty"`
	NewDesc string     `json:"newDesc,omitempty"`
	Sources []memBrief `json:"sources,omitempty"`

	// Reproject (move a memory across projects) and Split (set up a project split).
	Reproject *reprojectView `json:"reproject,omitempty"`
	Split     *splitView     `json:"split,omitempty"`

	// Relocate (pull a leaked global memory back inside a newly fenced project).
	Relocate *relocateView `json:"relocate,omitempty"`

	// Rekind (reclassify a memory's kind in place).
	Rekind *rekindView `json:"rekind,omitempty"`

	// AbandonPlan (retag a never-approved captured plan).
	AbandonPlan *abandonPlanView `json:"abandonPlan,omitempty"`

	// ShipPlan (retag an implemented-but-unapproved captured plan).
	ShipPlan *shipPlanView `json:"shipPlan,omitempty"`
	// MergePlans (fold a stranded capture into the plan holding its steps).
	MergePlans *mergePlansView `json:"mergePlans,omitempty"`

	// MemoryWanted (open a task to write knowledge agents searched for in vain).
	MemoryWanted *memoryWantedView `json:"memoryWanted,omitempty"`

	// ToolError (open a task to fix an error agents keep hitting).
	ToolError *toolErrorView `json:"toolError,omitempty"`

	// Inbox projections. These drive the rail row and the reader's action
	// footer only; every one is json:"-" because ?format=json is the seam CLI's
	// contract and its shape (groups/cards/counts) must not move.
	//
	// RowTitle/RowDetail are the one-line summary: the outcome, then the
	// concrete objects it touches. GroupKey/GroupLabel place the row in the
	// taxonomy. Targets are the retarget options for an in-plan reproject.
	// NextID is the row the queue advances to after this one is decided, so an
	// apply/dismiss lands on the next proposal instead of a blank pane.
	RowTitle   string       `json:"-"`
	RowDetail  string       `json:"-"`
	GroupKey   string       `json:"-"`
	GroupLabel string       `json:"-"`
	Targets    []projectOpt `json:"-"`
	NextID     string       `json:"-"`
	// Href is the row's canonical URL with the active filter suffix, and
	// Selected marks the row the reader is showing. Both are resolved here
	// rather than in the template so the rail row renders from the card alone.
	Href     string `json:"-"`
	Selected bool   `json:"-"`
	// CanUndoApply drives both the reader's assurance copy and whether Apply
	// asks for a confirm: an undoable change needs no interstitial, an
	// irreversible one does. CanAct mirrors gardenerData's, because the reader
	// also renders standalone as a ?reader=1 fragment with no page around it.
	CanUndoApply bool `json:"-"`
	CanAct       bool `json:"-"`
	// Level is the console level, for the reader's gates (Retarget, Hide
	// forever), for the same reason as CanAct: the reader renders standalone.
	Level level `json:"-"`
	// Fenced is the projects this proposal touches that are isolated NOW,
	// whether or not they were when it was proposed. The console is exempt from
	// the isolation fence by design -- the owner sees everything -- so this
	// never hides or blocks anything; it marks a proposal whose evidence
	// predates a tighten, so applying it is an informed choice rather than a
	// silent move across a fence raised after the fact. Agent callers are
	// refused outright at the MCP surface (mcp.fenceProposal).
	Fenced []store.ProjectIsolation `json:"-"`
}

// queueSection is one taxonomy group in the review rail: the pending proposals
// that call for the same kind of decision, newest first.
type queueSection struct {
	Key   string         `json:"-"`
	Label string         `json:"-"`
	Icon  string         `json:"-"`
	Rows  []proposalCard `json:"-"`
}

// queueTaxonomy is the single source for how the review queue is grouped: the
// section order, its label, its icon, and which proposal kinds land in it.
// Adding a proposal kind without listing it here is caught by a test rather
// than silently dropping its rows out of the rail.
var queueTaxonomy = []queueSection{
	{Key: "errors", Label: "Errors & gaps", Icon: "triangle-alert"},
	{Key: "cleanup", Label: "Memory cleanup", Icon: "brain"},
	{Key: "plans", Label: "Plan settlements", Icon: "map"},
	{Key: "digests", Label: "Digests", Icon: "file-text"},
}

// sectionOfKind maps a proposal kind to its taxonomy section key.
var sectionOfKind = map[string]string{
	store.ProposalToolError:    "errors",
	store.ProposalMemoryWanted: "errors",
	store.ProposalMerge:        "cleanup",
	store.ProposalConsolidate:  "cleanup",
	store.ProposalArchive:      "cleanup",
	store.ProposalRekind:       "cleanup",
	store.ProposalReproject:    "cleanup",
	store.ProposalRelocate:     "cleanup",
	store.ProposalAbandonPlan:  "plans",
	store.ProposalShipPlan:     "plans",
	store.ProposalMergePlans:   "plans",
	store.ProposalDigest:       "digests",
}

// planSectionKey is the ?group= value selecting the split-plan batches, which
// are their own ordered groups rather than a taxonomy section.
const planSectionKey = "split"

// recentRow is one entry in the rail's "Recently decided" section: a resolved
// proposal reduced to what an undo decision needs.
type recentRow struct {
	ID         string
	Kind       string
	Icon       string
	Tone       string
	RowTitle   string
	Status     string // applied | dismissed | hidden
	ResolvedAt *time.Time
	CanUndo    bool
}

// hiddenRow is one entry in the rail's "Hidden forever" section: a pattern the
// owner blocked permanently, and the handle to lift it. It is deliberately not
// a recentRow -- these two lists answer different questions ("what did I just
// do" vs "what am I still blocking") and a hide falls off the first within a
// handful of decisions while staying in force indefinitely.
type hiddenRow struct {
	ID        string
	Kind      string
	Icon      string
	Tone      string
	RowTitle  string
	RowDetail string
	HiddenAt  *time.Time
}

// memoryWantedView is the memory_wanted projection: the recurring zero-hit
// recall queries that evidence a knowledge gap.
type memoryWantedView struct {
	Project        string   `json:"project,omitempty"`
	Queries        []string `json:"queries"`
	MissCount      int      `json:"missCount"`
	SessionCount   int      `json:"sessionCount"`
	LastMissedAt   string   `json:"lastMissedAt,omitempty"`
	SuggestedTitle string   `json:"suggestedTitle"`
}

// toolErrorView is the tool_error projection: the recurring agent-facing error
// pattern, badged by surface (a failing tool call vs a fail-open hook stage).
type toolErrorView struct {
	Project        string   `json:"project,omitempty"`
	Surface        string   `json:"surface"` // "tool" | "hook"
	Key            string   `json:"key"`     // tool name or hook stage label
	Signature      string   `json:"signature,omitempty"`
	Examples       []string `json:"examples"`
	ErrorCount     int      `json:"errorCount"`
	SessionCount   int      `json:"sessionCount"`
	LastSeenAt     string   `json:"lastSeenAt,omitempty"`
	SuggestedTitle string   `json:"suggestedTitle"`
}

// abandonPlanView is the abandon_plan projection: the stale captured plan.
type abandonPlanView struct {
	NoteID  string `json:"noteId"`
	Slug    string `json:"slug"` // plan:<slug> composition key
	Title   string `json:"title"`
	Project string `json:"project,omitempty"`
	Status  string `json:"status"` // draft | presented
}

// shipPlanView is the ship_plan projection: the stale captured plan whose work
// the repo's history shows landed, with the matching commits as evidence.
type shipPlanView struct {
	NoteID       string   `json:"noteId"`
	Slug         string   `json:"slug"` // plan:<slug> composition key
	Title        string   `json:"title"`
	Project      string   `json:"project,omitempty"`
	Status       string   `json:"status"` // draft | presented
	Repo         string   `json:"repo,omitempty"`
	Stamp        string   `json:"stamp,omitempty"`
	CommitsSince int      `json:"commitsSince"`
	MatchedCount int      `json:"matchedCount"`
	Commits      []string `json:"commits"` // "sha message" lines, capped by the gardener
}

// mergePlansView is the merge_plans projection: the stranded capture on the
// left, the composition that holds the steps on the right, and how many
// significant title words tied them together -- the evidence the owner is
// judging, since applying moves the whole composition.
type mergePlansView struct {
	NoteID       string `json:"noteId"`
	Slug         string `json:"slug"` // the capture's plan:<slug>, the one being retired
	Title        string `json:"title"`
	Project      string `json:"project,omitempty"`
	Status       string `json:"status"` // draft | presented
	IntoSlug     string `json:"intoSlug"`
	IntoTitle    string `json:"intoTitle"`
	SharedTokens int    `json:"sharedTokens"`
}

// planGroup collects the pending proposals of one plan (a split batch) so the
// console reviews them together: the setup card first, then the per-memory
// reproject cards, with Targets listing the projects a reproject may retarget to.
type planGroup struct {
	Slug      string         `json:"slug"`
	Request   string         `json:"request,omitempty"`
	MoveCount int            `json:"moveCount"`
	Cards     []proposalCard `json:"cards"`
	Targets   []projectOpt   `json:"targets"`
}

// projectOpt is one entry in the request-scope selector.
type projectOpt struct {
	Slug  string `json:"slug"`
	Label string `json:"label"`
}

// gardenerData is the payload for the Gardener page. Groups holds plan-batched
// proposals (a split's setup + reproject cards); Cards holds the rest.
type gardenerData struct {
	Groups          []planGroup    `json:"groups,omitempty"`
	Cards           []proposalCard `json:"cards"`
	PendingCount    int            `json:"pendingCount"`
	RequestedCount  int            `json:"requestedCount"`
	BackgroundCount int            `json:"backgroundCount"`
	CanAct          bool           `json:"-"`
	CanRequest      bool           `json:"canRequest"` // an LLM chat client is configured
	Projects        []projectOpt   `json:"projects,omitempty"`
	Scope           string         `json:"scope,omitempty"`    // selected request scope (project slug)
	SplitReq        string         `json:"splitReq,omitempty"` // split request awaiting a source project (renders the picker follow-up)
	Notice          string         `json:"notice,omitempty"`   // positive flash
	Error           string         `json:"error,omitempty"`    // failure flash

	// Inbox state for the HTML rail + reader. All json:"-": the CLI reads
	// Groups/Cards/counts and that shape is fixed.
	Sections []queueSection `json:"-"`
	Recent   []recentRow    `json:"-"`
	Hidden   []hiddenRow    `json:"-"`
	// Selected is the proposal open in the reader: the requested one on a
	// /console/gardener/{id} page, or the first row of the queue on the list URL
	// (SelectedAuto, which the client pins into the URL).
	Selected     *proposalCard `json:"-"`
	SelectedAuto bool          `json:"-"`
	// QS is the ?group=&src= suffix rail links carry so the active filter
	// survives a selection change ("" when both are at their defaults).
	QS         string `json:"-"`
	TypeFilter string `json:"-"` // "all" | planSectionKey | a queueTaxonomy key
	SrcFilter  string `json:"-"` // "all" | "you" | "auto"
	// TotalPending is the size of the whole queue, unfiltered -- what separates
	// "nothing matches this filter" from "the garden is tidy".
	TotalPending int `json:"-"`
}

// gardenerPageData assembles the review queue for the given request: every
// pending proposal projected into a card, filtered by ?group=/?src=, then split
// into plan batches and taxonomy sections in rail order. ok=false means the
// 400/500 response was already written.
//
// It also returns the unfiltered card set, so a deep link to a proposal the
// active filter hides can still resolve it (a shared URL must open its
// proposal, filter or no filter).
func (s *Service) gardenerPageData(w http.ResponseWriter, r *http.Request) (data gardenerData, all []proposalCard, ok bool) {
	ctx := r.Context()
	typeFilter, srcFilter, ok := s.parseGardenerFilters(w, r)
	if !ok {
		return gardenerData{}, nil, false
	}
	proposals, err := store.PendingProposals(ctx, s.cfg.DB, "")
	if err != nil {
		s.serverError(w, r, err)
		return gardenerData{}, nil, false
	}
	all = make([]proposalCard, 0, len(proposals))
	for _, p := range proposals {
		all = append(all, s.toProposalCard(ctx, p))
	}

	visible := filterCards(all, typeFilter, srcFilter)
	groups, sections := buildQueue(visible, gardenerQS(typeFilter, srcFilter))
	requested := 0
	for _, c := range visible {
		if c.Source == "request" {
			requested++
		}
	}
	return gardenerData{
		Groups:          groups,
		Cards:           ungroupedRows(sections),
		Sections:        sections,
		Recent:          s.recentDecisions(ctx),
		Hidden:          s.hiddenPatterns(ctx),
		PendingCount:    len(visible),
		RequestedCount:  requested,
		BackgroundCount: len(visible) - requested,
		TotalPending:    len(all),
		CanAct:          s.cfg.Gardener != nil,
		CanRequest:      s.cfg.Gardener != nil && s.cfg.Gardener.CanRequest(),
		Projects:        s.projectOptions(ctx),
		Scope:           r.URL.Query().Get("project"),
		SplitReq:        r.URL.Query().Get("split"),
		Notice:          r.URL.Query().Get("notice"),
		Error:           r.URL.Query().Get("error"),
		QS:              gardenerQS(typeFilter, srcFilter),
		TypeFilter:      typeFilter,
		SrcFilter:       srcFilter,
	}, all, true
}

func (s *Service) gardenerPage(w http.ResponseWriter, r *http.Request) {
	data, _, ok := s.gardenerPageData(w, r)
	if !ok {
		return
	}
	// The HTML inbox opens the top of the queue, like every library screen; the
	// client pins the resulting id into the URL so Back works through triage.
	if !wantsJSON(r) {
		if first := firstRow(data); first != nil {
			first.Selected = true
			first.Level = s.consoleLevel(r.Context()).Level
			data.Selected = first
			data.SelectedAuto = true
		}
	}
	s.render(w, r, "gardener", pageData{Title: "Gardener", Active: "gardener", Data: data})
}

// gardenerDetail serves one proposal: JSON for the CLI, the reader fragment for
// an in-place swap (?reader=1), or the full queue page with that proposal
// selected. A missing or already-resolved id bounces back to the queue with an
// error flash rather than 404ing -- the usual way to reach one is a stale link
// from a decision another tab already made.
func (s *Service) gardenerDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	p, found, err := store.ProposalByID(ctx, s.cfg.DB, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !found || p.Status != store.ProposalPending {
		gardenerRedirect(w, r, "", "error", "That proposal was already resolved.")
		return
	}
	data, all, ok := s.gardenerPageData(w, r)
	if !ok {
		return
	}
	card := findCard(data.Groups, data.Sections, id)
	if card == nil {
		// Hidden by the active filter: rebuild the rail unfiltered so the row
		// still carries its plan context, just without a next-in-queue.
		g, sec := buildQueue(all, data.QS)
		card = findCard(g, sec, id)
		if card == nil { // belt and braces; the proposal is pending, so it is in there
			c := s.toProposalCard(ctx, p)
			card = &c
		}
		card.NextID = ""
	}
	card.Selected = true
	card.Level = s.consoleLevel(ctx).Level
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, card)
		return
	}
	if r.URL.Query().Get("reader") == "1" {
		s.renderReader(w, r, "gardener", "gardener-reader", card)
		return
	}
	data.Selected = card
	s.render(w, r, "gardener", pageData{Title: "Proposal " + card.Label, Active: "gardener", Data: data})
}

// parseGardenerFilters validates ?group= and ?src=, defaulting both to "all".
// An unknown value is a 400 naming the valid ones rather than a silent default,
// so an agent driving the console by URL sees the fix. ok=false means the
// response was already written.
func (s *Service) parseGardenerFilters(w http.ResponseWriter, r *http.Request) (typeFilter, srcFilter string, ok bool) {
	q := r.URL.Query()
	typeFilter = q.Get("group")
	if typeFilter == "" {
		typeFilter = "all"
	}
	if !slices.Contains(groupFilterKeys(), typeFilter) {
		s.badRequest(w, r, fmt.Sprintf("invalid group %q: valid values are %s",
			typeFilter, strings.Join(groupFilterKeys(), ", ")))
		return "", "", false
	}
	srcFilter = q.Get("src")
	if srcFilter == "" {
		srcFilter = "all"
	}
	if !slices.Contains(srcFilterKeys, srcFilter) {
		s.badRequest(w, r, fmt.Sprintf("invalid src %q: valid values are %s",
			srcFilter, strings.Join(srcFilterKeys, ", ")))
		return "", "", false
	}
	return typeFilter, srcFilter, true
}

// srcFilterKeys are the accepted ?src values: everything, the proposals the
// owner asked for, or the ones a background pass found.
var srcFilterKeys = []string{"all", "you", "auto"}

// groupFilterKeys are the accepted ?group values: everything, the split-plan
// batches, or one taxonomy section.
func groupFilterKeys() []string {
	out := make([]string, 0, len(queueTaxonomy)+2)
	out = append(out, "all", planSectionKey)
	for _, sec := range queueTaxonomy {
		out = append(out, sec.Key)
	}
	return out
}

// gardenerQS renders the ?group=&src= suffix rail links carry ("" at defaults),
// so changing the selection keeps the active filter.
func gardenerQS(typeFilter, srcFilter string) string {
	v := url.Values{}
	if typeFilter != "all" {
		v.Set("group", typeFilter)
	}
	if srcFilter != "all" {
		v.Set("src", srcFilter)
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// filterCards narrows the queue to the selected type and source. A plan card is
// kept only by "all" or the split filter; every other card by its taxonomy
// section.
func filterCards(cards []proposalCard, typeFilter, srcFilter string) []proposalCard {
	out := make([]proposalCard, 0, len(cards))
	for _, c := range cards {
		switch srcFilter {
		case "you":
			if c.Source != "request" {
				continue
			}
		case "auto":
			if c.Source == "request" {
				continue
			}
		}
		switch typeFilter {
		case "all":
		case planSectionKey:
			if c.Plan == "" {
				continue
			}
		default:
			if c.Plan != "" || sectionOfKind[c.Kind] != typeFilter {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// buildQueue turns projected cards into the rail: plan batches first (a split
// setup plus its moves, reviewed together), then the taxonomy sections. Rows
// stay in store order (newest first) within each group, and every row learns
// its own link plus which row the queue advances to when it is decided.
func buildQueue(cards []proposalCard, qs string) ([]planGroup, []queueSection) {
	groups, ungrouped := groupByPlan(cards)
	sections := sectionize(ungrouped)
	for _, c := range railOrder(groups, sections) {
		c.Href = "/console/gardener/" + c.ID + qs
	}
	assignNextIDs(groups, sections)
	return groups, sections
}

// railOrder returns pointers to every row in display order: plan batches lead,
// then the taxonomy sections. The pointers are into the groups/sections
// themselves, so writing through them updates what the rail renders.
func railOrder(groups []planGroup, sections []queueSection) []*proposalCard {
	var order []*proposalCard
	for gi := range groups {
		for ci := range groups[gi].Cards {
			order = append(order, &groups[gi].Cards[ci])
		}
	}
	for si := range sections {
		for ri := range sections[si].Rows {
			order = append(order, &sections[si].Rows[ri])
		}
	}
	return order
}

// sectionize files the non-plan cards into the taxonomy sections, dropping
// empty ones. A kind absent from sectionOfKind would vanish here, which is
// exactly what TestQueueTaxonomy_CoversEveryKind exists to prevent.
func sectionize(cards []proposalCard) []queueSection {
	byKey := map[string][]proposalCard{}
	for _, c := range cards {
		key := sectionOfKind[c.Kind]
		if key == "" {
			continue
		}
		byKey[key] = append(byKey[key], c)
	}
	out := make([]queueSection, 0, len(queueTaxonomy))
	for _, sec := range queueTaxonomy {
		rows := byKey[sec.Key]
		if len(rows) == 0 {
			continue
		}
		for i := range rows {
			rows[i].GroupKey, rows[i].GroupLabel = sec.Key, sec.Label
		}
		sec.Rows = rows
		out = append(out, sec)
	}
	return out
}

// assignNextIDs walks the rail in display order and points each row at the one
// after it. Apply and Dismiss post that id back, so a decision advances the
// reader to the next proposal instead of leaving it blank. The last row's
// NextID stays empty, which sends the owner to the bare list (auto-selecting
// whatever is left).
func assignNextIDs(groups []planGroup, sections []queueSection) {
	order := railOrder(groups, sections)
	for i, c := range order {
		if i+1 < len(order) {
			c.NextID = order[i+1].ID
		}
	}
}

// ungroupedRows flattens the sections back into the flat Cards list the
// ?format=json CLI contract exposes, in rail order.
func ungroupedRows(sections []queueSection) []proposalCard {
	var out []proposalCard
	for _, sec := range sections {
		out = append(out, sec.Rows...)
	}
	return out
}

// firstRow is the reader's default selection on the list URL: the first row of
// the rail (plan batches lead, then the taxonomy sections).
func firstRow(data gardenerData) *proposalCard {
	return findCard(data.Groups, data.Sections, "")
}

// findCard returns the rail row with this id, or -- for an empty id -- the
// first row. nil means the rail has no such row.
func findCard(groups []planGroup, sections []queueSection, id string) *proposalCard {
	for gi := range groups {
		for ci := range groups[gi].Cards {
			if c := &groups[gi].Cards[ci]; id == "" || c.ID == id {
				return c
			}
		}
	}
	for si := range sections {
		for ri := range sections[si].Rows {
			if c := &sections[si].Rows[ri]; id == "" || c.ID == id {
				return c
			}
		}
	}
	return nil
}

// recentDecisions projects the last handful of resolved proposals into the
// rail's undo affordance. Best-effort: a query error costs the section, not the
// page.
func (s *Service) recentDecisions(ctx context.Context) []recentRow {
	ps, err := store.RecentResolvedProposals(ctx, s.cfg.DB, recentDecisionLimit)
	if err != nil {
		s.logger.Warn("console: recent gardener decisions", "error", err)
		return nil
	}
	out := make([]recentRow, 0, len(ps))
	for _, p := range ps {
		_, _, iconName, tone := proposalPresentation(p.Kind)
		title, _ := rowSummary(p.Kind, s.toProposalCard(ctx, p))
		out = append(out, recentRow{
			ID: p.ID, Kind: p.Kind, Icon: iconName, Tone: tone, RowTitle: title,
			Status: p.Status, ResolvedAt: p.ResolvedAt,
			CanUndo: gardener.CanUndoResolution(p.Status, p.Kind),
		})
	}
	return out
}

// recentDecisionLimit caps the "Recently decided" section. Undo is a
// second-thought affordance, not a history browser: the events feed keeps the
// full record.
const recentDecisionLimit = 10

// hiddenPatterns projects the forever-blocked proposals into the rail's unhide
// list. It is uncapped on purpose: a hide is an explicit owner decision, so the
// list is short by construction, and truncating the one surface that says what
// is being suppressed would leave a block nobody can find to lift. Best-effort:
// a query error costs the section, not the page.
func (s *Service) hiddenPatterns(ctx context.Context) []hiddenRow {
	if s.cfg.Gardener == nil {
		return nil
	}
	ps, err := s.cfg.Gardener.Hidden(ctx)
	if err != nil {
		s.logger.Warn("console: hidden gardener proposals", "error", err)
		return nil
	}
	out := make([]hiddenRow, 0, len(ps))
	for _, p := range ps {
		_, _, iconName, tone := proposalPresentation(p.Kind)
		title, detail := rowSummary(p.Kind, s.toProposalCard(ctx, p))
		out = append(out, hiddenRow{
			ID: p.ID, Kind: p.Kind, Icon: iconName, Tone: tone,
			RowTitle: title, RowDetail: detail, HiddenAt: p.ResolvedAt,
		})
	}
	return out
}

// groupByPlan partitions cards into plan groups (those carrying a plan slug) and
// the ungrouped remainder. Within a group the split setup card sorts first, then
// reprojects in creation order; Targets lists the projects a reproject may
// retarget to (the split's children + shared parent, or the distinct targets
// already chosen when the setup card has been applied). Group order follows first
// appearance so the newest plan (newest proposals first) leads.
func groupByPlan(cards []proposalCard) (groups []planGroup, ungrouped []proposalCard) {
	idx := map[string]int{} // plan slug -> position in groups
	for _, c := range cards {
		if c.Plan == "" {
			ungrouped = append(ungrouped, c)
			continue
		}
		i, ok := idx[c.Plan]
		if !ok {
			idx[c.Plan] = len(groups)
			groups = append(groups, planGroup{Slug: c.Plan})
			i = idx[c.Plan]
		}
		groups[i].Cards = append(groups[i].Cards, c)
	}
	for i := range groups {
		sortGroupCards(groups[i].Cards)
		groups[i].Targets = planTargets(groups[i].Cards)
		for _, c := range groups[i].Cards {
			if groups[i].Request == "" && c.RequestText != "" {
				groups[i].Request = c.RequestText
			}
			if c.Reproject != nil {
				groups[i].MoveCount++
			}
		}
		// Each card carries its batch's retarget options, so the reader can
		// offer the correction without the group around it.
		for j := range groups[i].Cards {
			groups[i].Cards[j].Targets = groups[i].Targets
			groups[i].Cards[j].GroupKey = planSectionKey
			groups[i].Cards[j].GroupLabel = "Split plan: " + groups[i].Slug
		}
	}
	return groups, ungrouped
}

// rowSummary is the one-line inbox projection of a proposal: the outcome, then
// the concrete objects it touches. It is what the rail row shows and what the
// "Recently decided" section reads back, so a decision is recognizable from a
// single line without opening the reader.
func rowSummary(kind string, c proposalCard) (title, detail string) {
	switch kind {
	case store.ProposalMerge:
		if c.Keep != nil && c.Drop != nil {
			return "Merge duplicates", "keep " + c.Keep.Name + " · retire " + c.Drop.Name
		}
		return "Merge duplicates", ""
	case store.ProposalArchive:
		if c.Archive != nil {
			return "Archive memory", c.Archive.Name + " · " + projectLabel(c.Archive.Project)
		}
		return "Archive memory", ""
	case store.ProposalDigest:
		return "Publish digest", c.Title
	case store.ProposalConsolidate:
		return fmt.Sprintf("Consolidate %d memories", len(c.Sources)), "→ " + c.NewName
	case store.ProposalReproject:
		if c.Reproject != nil {
			return "Move memory", fmt.Sprintf("%s: %s → %s",
				c.Reproject.Name, projectLabel(c.Reproject.From), projectLabel(c.Reproject.To))
		}
		return "Move memory", ""
	case store.ProposalRelocate:
		if c.Relocate != nil {
			return "Relocate leaked memory", fmt.Sprintf("%s: global → %s", c.Relocate.Name, c.Relocate.To)
		}
		return "Relocate leaked memory", ""
	case store.ProposalRekind:
		if c.Rekind != nil {
			return "Reclassify memory", fmt.Sprintf("%s: %s → %s", c.Rekind.Name, c.Rekind.From, c.Rekind.To)
		}
		return "Reclassify memory", ""
	case store.ProposalSplit:
		if c.Split != nil {
			return "Set up project split", fmt.Sprintf("%s → %d children", c.Split.Source, len(c.Split.Children))
		}
		return "Set up project split", ""
	case store.ProposalAbandonPlan:
		if c.AbandonPlan != nil {
			return "Retire stale plan", c.AbandonPlan.Title
		}
		return "Retire stale plan", ""
	case store.ProposalShipPlan:
		if c.ShipPlan != nil {
			return "Mark plan shipped", c.ShipPlan.Title + " · " +
				plural(c.ShipPlan.MatchedCount, "matching commit", "matching commits")
		}
		return "Mark plan shipped", ""
	case store.ProposalMergePlans:
		if c.MergePlans != nil {
			return "Fold plan into another", c.MergePlans.Title + " → " + c.MergePlans.IntoSlug
		}
		return "Fold plan into another", ""
	case store.ProposalMemoryWanted:
		if c.MemoryWanted != nil {
			return "Write missing memory", c.MemoryWanted.SuggestedTitle + " · " +
				plural(c.MemoryWanted.MissCount, "miss", "misses")
		}
		return "Write missing memory", ""
	case store.ProposalToolError:
		if c.ToolError != nil {
			return "Fix recurring error", fmt.Sprintf("%s · %d×", c.ToolError.Key, c.ToolError.ErrorCount)
		}
		return "Fix recurring error", ""
	default:
		return c.Label, ""
	}
}

// projectLabel renders a project slug for a one-line summary, naming the global
// scope rather than leaving a gap where a project would be.
func projectLabel(slug string) string {
	if slug == "" {
		return "global"
	}
	return slug
}

// sortGroupCards stable-orders a group's cards: the split setup first, then the
// rest in their existing (newest-first) order.
func sortGroupCards(cards []proposalCard) {
	sort.SliceStable(cards, func(i, j int) bool {
		return cards[i].Kind == store.ProposalSplit && cards[j].Kind != store.ProposalSplit
	})
}

// planTargets derives the retarget options for a plan group: the split setup's
// children + shared parent when present, else the distinct targets already chosen
// by the reproject cards (so retargeting still works after the setup is applied).
func planTargets(cards []proposalCard) []projectOpt {
	for _, c := range cards {
		if c.Split != nil {
			out := append([]projectOpt{}, c.Split.Children...)
			if c.Split.Shared != nil {
				out = append(out, *c.Split.Shared)
			}
			return out
		}
	}
	seen := map[string]bool{}
	var out []projectOpt
	for _, c := range cards {
		if c.Reproject == nil || c.Reproject.To == "" || seen[c.Reproject.To] {
			continue
		}
		seen[c.Reproject.To] = true
		out = append(out, projectOpt{Slug: c.Reproject.To, Label: c.Reproject.To})
	}
	return out
}

// projectOptions lists projects for the request-scope selector, best-effort: a
// query error yields no options rather than failing the page.
func (s *Service) projectOptions(ctx context.Context) []projectOpt {
	projs, err := store.ListProjects(ctx, s.cfg.DB)
	if err != nil {
		s.logger.Warn("console: list projects", "error", err)
		return nil
	}
	out := make([]projectOpt, 0, len(projs))
	for _, p := range projs {
		label := p.Name
		if label == "" {
			label = p.Slug
		}
		out = append(out, projectOpt{Slug: p.Slug, Label: label})
	}
	return out
}

func (s *Service) toProposalCard(ctx context.Context, p store.Proposal) proposalCard {
	label, eyebrow, iconName, tone := proposalPresentation(p.Kind)
	c := proposalCard{
		ID: p.ID, Kind: p.Kind, Label: label, Eyebrow: eyebrow, Icon: iconName, Tone: tone,
		CreatedAt: p.CreatedAt, Source: payloadStr(p.Payload, "source"),
		RequestText: snippet(payloadStr(p.Payload, "request_text"), 260), Plan: payloadStr(p.Payload, "plan"),
	}
	switch p.Kind {
	case store.ProposalArchive:
		c.Archive = &memBrief{
			ID: payloadStr(p.Payload, "id"), Name: payloadStr(p.Payload, "name"),
			Project: payloadStr(p.Payload, "project"), Kind: payloadStr(p.Payload, "kind"),
			Description: payloadStr(p.Payload, "description"),
		}
		c.Reason = payloadStr(p.Payload, "reason")
	case store.ProposalMerge:
		c.Score = payloadFloat(p.Payload, "score")
		c.ScorePercent = int(c.Score*100 + 0.5)
		c.Keep = briefFrom(payloadMap(p.Payload, "keep"))
		c.Drop = briefFrom(payloadMap(p.Payload, "drop"))
	case store.ProposalDigest:
		c.Project = payloadStr(p.Payload, "project")
		c.Month = payloadStr(p.Payload, "month")
		c.SessionCount = int(payloadFloat(p.Payload, "session_count"))
		c.Title = payloadStr(p.Payload, "title")
		body := payloadStr(p.Payload, "body")
		c.Preview = snippet(body, 600) // raw, for JSON
		c.Body = s.renderBody(ctx, body, c.Project)
	case store.ProposalConsolidate:
		c.NewName = payloadStr(p.Payload, "name")
		c.NewKind = payloadStr(p.Payload, "kind")
		c.NewDesc = payloadStr(p.Payload, "description")
		c.Project = payloadStr(p.Payload, "project")
		body := payloadStr(p.Payload, "body")
		c.Preview = snippet(body, 600) // raw, for JSON
		c.Body = s.renderBody(ctx, body, c.Project)
		for _, src := range payloadList(p.Payload, "sources") {
			c.Sources = append(c.Sources, memBrief{ID: payloadStr(src, "id"), Name: payloadStr(src, "name")})
		}
	case store.ProposalReproject:
		c.Reproject = &reprojectView{
			ID: payloadStr(p.Payload, "id"), Name: payloadStr(p.Payload, "name"),
			From: payloadStr(p.Payload, "from"), To: payloadStr(p.Payload, "to"),
			Rationale: payloadStr(p.Payload, "rationale"),
		}
	case store.ProposalRelocate:
		c.Relocate = &relocateView{
			ID: payloadStr(p.Payload, "id"), Name: payloadStr(p.Payload, "name"),
			Kind: payloadStr(p.Payload, "kind"), Description: payloadStr(p.Payload, "description"),
			To:      payloadStr(p.Payload, "to"),
			Session: payloadStr(p.Payload, "session"), Isolation: payloadStr(p.Payload, "isolation"),
		}
		c.Reason = payloadStr(p.Payload, "reason")
	case store.ProposalRekind:
		c.Rekind = &rekindView{
			ID: payloadStr(p.Payload, "id"), Name: payloadStr(p.Payload, "name"),
			Project: payloadStr(p.Payload, "project"),
			From:    payloadStr(p.Payload, "from"), To: payloadStr(p.Payload, "to"),
			Rationale: payloadStr(p.Payload, "rationale"),
		}
	case store.ProposalAbandonPlan:
		c.AbandonPlan = &abandonPlanView{
			NoteID: payloadStr(p.Payload, "id"), Slug: payloadStr(p.Payload, "slug"),
			Title: payloadStr(p.Payload, "title"), Project: payloadStr(p.Payload, "project"),
			Status: payloadStr(p.Payload, "plan_status"),
		}
		c.Reason = payloadStr(p.Payload, "reason")
	case store.ProposalShipPlan:
		c.ShipPlan = &shipPlanView{
			NoteID: payloadStr(p.Payload, "id"), Slug: payloadStr(p.Payload, "slug"),
			Title: payloadStr(p.Payload, "title"), Project: payloadStr(p.Payload, "project"),
			Status: payloadStr(p.Payload, "plan_status"),
			Repo:   payloadStr(p.Payload, "repo"), Stamp: payloadStr(p.Payload, "stamp"),
			CommitsSince: int(payloadFloat(p.Payload, "commits_since")),
			MatchedCount: int(payloadFloat(p.Payload, "matched_count")),
			Commits:      payloadStrList(p.Payload, "commits"),
		}
		c.Reason = payloadStr(p.Payload, "reason")
	case store.ProposalMergePlans:
		c.MergePlans = &mergePlansView{
			NoteID: payloadStr(p.Payload, "id"), Slug: payloadStr(p.Payload, "slug"),
			Title: payloadStr(p.Payload, "title"), Project: payloadStr(p.Payload, "project"),
			Status:    payloadStr(p.Payload, "plan_status"),
			IntoSlug:  payloadStr(p.Payload, "merge_into"),
			IntoTitle: payloadStr(p.Payload, "merge_into_title"),

			SharedTokens: int(payloadFloat(p.Payload, "shared_tokens")),
		}
		c.Reason = payloadStr(p.Payload, "reason")
	case store.ProposalMemoryWanted:
		c.MemoryWanted = &memoryWantedView{
			Project:        payloadStr(p.Payload, "project"),
			Queries:        payloadStrList(p.Payload, "queries"),
			MissCount:      int(payloadFloat(p.Payload, "miss_count")),
			SessionCount:   int(payloadFloat(p.Payload, "session_count")),
			LastMissedAt:   payloadStr(p.Payload, "last_missed_at"),
			SuggestedTitle: payloadStr(p.Payload, "suggested_title"),
		}
		c.Reason = payloadStr(p.Payload, "reason")
	case store.ProposalToolError:
		c.ToolError = &toolErrorView{
			Project:        payloadStr(p.Payload, "project"),
			Surface:        payloadStr(p.Payload, "surface"),
			Key:            payloadStr(p.Payload, "name"),
			Signature:      payloadStr(p.Payload, "signature"),
			Examples:       payloadStrList(p.Payload, "examples"),
			ErrorCount:     int(payloadFloat(p.Payload, "error_count")),
			SessionCount:   int(payloadFloat(p.Payload, "session_count")),
			LastSeenAt:     payloadStr(p.Payload, "last_seen_at"),
			SuggestedTitle: payloadStr(p.Payload, "suggested_title"),
		}
		c.Reason = payloadStr(p.Payload, "reason")
	case store.ProposalSplit:
		sv := &splitView{
			Source:       payloadStr(p.Payload, "source_project"),
			Family:       payloadStr(p.Payload, "family"),
			RetireSource: payloadBool(p.Payload, "retire_source"),
		}
		for _, ch := range payloadList(p.Payload, "children") {
			sv.Children = append(sv.Children, projectOpt{Slug: payloadStr(ch, "slug"), Label: payloadStr(ch, "label")})
		}
		if sh := payloadMap(p.Payload, "shared"); sh != nil {
			if slug := payloadStr(sh, "slug"); slug != "" {
				sv.Shared = &projectOpt{Slug: slug, Label: payloadStr(sh, "label")}
			}
		}
		c.Split = sv
	}
	c.RowTitle, c.RowDetail = rowSummary(p.Kind, c)
	c.CanUndoApply = gardener.CanUndoApply(p.Kind)
	c.CanAct = s.cfg.Gardener != nil
	c.Fenced = s.fencedTargets(ctx, p)
	return c
}

// fencedTargets reports which of a proposal's projects are isolated now. A
// resolution failure is not an error the owner should see: the console renders
// the proposal either way, so a card that cannot be attributed simply carries no
// mark. That is the opposite of the tool surface, where an unattributable
// payload fails closed -- there, silence would leak; here, it would only nag.
//
// Relocate is exempt, and it is the one kind that must be. A relocate is the
// tighten's OWN repair -- it pulls a leaked global memory back inside the fence
// -- so it is created by the tighten and its evidence can never predate it,
// which is the only thing this mark exists to warn about. Marking it would also
// be the majority of the queue right after a tighten (one row per leaked
// memory), and a warning on every row is a warning on none.
func (s *Service) fencedTargets(ctx context.Context, p store.Proposal) []store.ProjectIsolation {
	if p.Kind == store.ProposalRelocate {
		return nil
	}
	fenced, err := gardener.FencedProjects(ctx, s.cfg.DB, p)
	if err != nil {
		s.logger.Warn("console: resolve proposal isolation", "proposal", p.ID, "kind", p.Kind, "error", err)
		return nil
	}
	return fenced
}

// proposalPresentation turns store-facing proposal kinds into the outcome-first
// language used at the review gate. The raw kind still renders as compact
// provenance, while these fields tell the owner what approving the card means.
func proposalPresentation(kind string) (label, eyebrow, iconName, tone string) {
	switch kind {
	case store.ProposalArchive:
		return "Archive a memory", "Active context", "archive", "warn"
	case store.ProposalMerge:
		return "Merge duplicate memories", "Duplicate signal", "git-merge", "brand"
	case store.ProposalDigest:
		return "Publish an activity digest", "New note", "file-text", "ok"
	case store.ProposalConsolidate:
		return "Consolidate into one memory", "Synthesis", "database", "brand"
	case store.ProposalReproject:
		return "Move a memory", "Scope correction", "folder-tree", "pop"
	case store.ProposalRelocate:
		return "Relocate a memory behind the fence", "Isolation repair", "lock", "warn"
	case store.ProposalRekind:
		return "Reclassify a memory", "Kind correction", "arrow-up-down", "pop"
	case store.ProposalSplit:
		return "Restructure a project", "Project topology", "split", "pop"
	case store.ProposalAbandonPlan:
		return "Retire a stale plan", "Planning hygiene", "archive", "warn"
	case store.ProposalShipPlan:
		return "Mark a plan shipped", "Planning hygiene", "git-commit-horizontal", "ok"
	case store.ProposalMergePlans:
		return "Fold a plan into another", "Planning hygiene", "git-merge", "pop"
	case store.ProposalMemoryWanted:
		return "Write a missing memory", "Knowledge gap", "search", "pop"
	case store.ProposalToolError:
		return "Fix a recurring error", "Error pattern", "triangle-alert", "danger"
	default:
		return "Review a knowledge change", "Proposal", "sprout", "brand"
	}
}

// payloadStrList reads a string-array field from a payload map (a JSON
// round-trip delivers it as []any; a same-process payload may still hold the
// original []string).
func payloadStrList(p map[string]any, key string) []string {
	if p == nil {
		return nil
	}
	if ss, ok := p[key].([]string); ok {
		return ss
	}
	raw, ok := p[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// payloadBool reads a boolean field from a payload map (false if absent).
func payloadBool(p map[string]any, key string) bool {
	if p == nil {
		return false
	}
	v, _ := p[key].(bool)
	return v
}

func briefFrom(m map[string]any) *memBrief {
	if m == nil {
		return nil
	}
	return &memBrief{
		ID: payloadStr(m, "id"), Name: payloadStr(m, "name"), Project: payloadStr(m, "project"),
		Kind: payloadStr(m, "kind"), Description: payloadStr(m, "description"),
	}
}

// gardenerApply carries out a proposal and redirects to the next one in the
// queue, surfacing an error via a flash query param when the effect could not
// be applied. A failure keeps the decided proposal selected, since it is still
// pending and the owner has to deal with it.
func (s *Service) gardenerApply(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Gardener == nil {
		s.serverError(w, r, errNoGardener)
		return
	}
	id := r.PathValue("id")
	next := s.nextSelection(r)
	res, err := s.cfg.Gardener.Apply(r.Context(), id)
	if err != nil {
		s.logger.Warn("console: gardener apply", "id", id, "error", err)
		gardenerRedirect(w, r, id, "error", err.Error())
		return
	}
	if settled := payloadStr(res, "settled"); settled != "" {
		gardenerRedirect(w, r, next, "notice", "Settled the proposal: "+settled+".")
		return
	}
	gardenerRedirect(w, r, next, "notice", "Applied the proposal.")
}

func (s *Service) gardenerDismiss(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Gardener == nil {
		s.serverError(w, r, errNoGardener)
		return
	}
	id := r.PathValue("id")
	next := s.nextSelection(r)
	if err := s.cfg.Gardener.Dismiss(r.Context(), id); err != nil {
		s.logger.Warn("console: gardener dismiss", "id", id, "error", err)
		gardenerRedirect(w, r, id, "error", err.Error())
		return
	}
	gardenerRedirect(w, r, next, "notice", "Dismissed the proposal. Undo it from Recently decided.")
}

// gardenerHide is the strong rejection: the proposal is resolved like a
// dismissal and its pattern is blocked permanently, so no recurrence brings it
// back. Both exits stay open, and the flash names them: Undo returns this
// proposal to the queue, Unhide lifts the block without doing so.
func (s *Service) gardenerHide(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Gardener == nil {
		s.serverError(w, r, errNoGardener)
		return
	}
	id := r.PathValue("id")
	next := s.nextSelection(r)
	if err := s.cfg.Gardener.Hide(r.Context(), id); err != nil {
		s.logger.Warn("console: gardener hide", "id", id, "error", err)
		gardenerRedirect(w, r, id, "error", err.Error())
		return
	}
	gardenerRedirect(w, r, next, "notice", "Hidden for good. Lift it from Hidden forever, or undo it from Recently decided.")
}

// gardenerUnhide lifts a forever block without returning its proposal to the
// queue. The next gardener pass may raise the pattern again once evidence for
// it recurs -- and if it has stopped recurring, nothing comes back, which the
// flash says rather than promising a proposal that will not arrive.
func (s *Service) gardenerUnhide(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Gardener == nil {
		s.serverError(w, r, errNoGardener)
		return
	}
	id := r.PathValue("id")
	if err := s.cfg.Gardener.Unhide(r.Context(), id); err != nil {
		s.logger.Warn("console: gardener unhide", "id", id, "error", err)
		gardenerRedirect(w, r, "", "error", err.Error())
		return
	}
	gardenerRedirect(w, r, "", "notice", "Unhidden. The gardener may propose this again if the pattern recurs.")
}

// gardenerUndo returns a resolved proposal to the queue, inverting whatever its
// apply did. On success the proposal is pending again, so the redirect selects
// it -- the owner lands on exactly what they took back.
func (s *Service) gardenerUndo(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Gardener == nil {
		s.serverError(w, r, errNoGardener)
		return
	}
	id := r.PathValue("id")
	if err := s.cfg.Gardener.Undo(r.Context(), id); err != nil {
		s.logger.Warn("console: gardener undo", "id", id, "error", err)
		gardenerRedirect(w, r, "", "error", err.Error())
		return
	}
	gardenerRedirect(w, r, id, "notice", "Restored the proposal to the queue.")
}

// gardenerGroupDismiss closes every pending proposal in one rail group -- a
// taxonomy section (?group=) or a split plan (?plan=) -- one at a time, so each
// keeps its own event and its own row in Recently decided and stays
// individually undoable. It is best-effort: it dismisses what it can and
// reports how many landed.
//
// Only Dismiss is offered in bulk. Apply stays per-proposal (a plan's
// Apply-whole-plan aside): dismissing costs nothing but the suggestion, while
// applying changes the knowledge agents receive.
func (s *Service) gardenerGroupDismiss(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Gardener == nil {
		s.serverError(w, r, errNoGardener)
		return
	}
	ctx := r.Context()
	group := strings.TrimSpace(r.PostFormValue("group"))
	plan := strings.TrimSpace(r.PostFormValue("plan"))
	if (group == "") == (plan == "") {
		s.badRequest(w, r, "dismiss a group by exactly one of group=<section> or plan=<slug>")
		return
	}
	label := plan
	if group != "" {
		sec, ok := taxonomySection(group)
		if !ok {
			s.badRequest(w, r, fmt.Sprintf("invalid group %q: valid values are %s",
				group, strings.Join(groupFilterKeys(), ", ")))
			return
		}
		label = sec.Label
	}

	proposals, err := store.PendingProposals(ctx, s.cfg.DB, "")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var batch []string
	for _, p := range proposals {
		inPlan := payloadStr(p.Payload, "plan")
		if plan != "" {
			if inPlan == plan {
				batch = append(batch, p.ID)
			}
			continue
		}
		if inPlan == "" && sectionOfKind[p.Kind] == group {
			batch = append(batch, p.ID)
		}
	}
	if len(batch) == 0 {
		gardenerRedirect(w, r, "", "error", "no pending proposals in "+label)
		return
	}
	dismissed, firstErr := 0, ""
	for _, id := range batch {
		if derr := s.cfg.Gardener.Dismiss(ctx, id); derr != nil {
			if firstErr == "" {
				firstErr = derr.Error()
			}
			continue
		}
		dismissed++
	}
	if firstErr != "" {
		gardenerRedirect(w, r, "", "error",
			fmt.Sprintf("dismissed %d of %d -- first error: %s", dismissed, len(batch), firstErr))
		return
	}
	gardenerRedirect(w, r, "", "notice",
		fmt.Sprintf("Dismissed %s from %s. Undo any of them from Recently decided.",
			plural(dismissed, "proposal", "proposals"), label))
}

// taxonomySection looks up a section by its ?group= key.
func taxonomySection(key string) (queueSection, bool) {
	for _, sec := range queueTaxonomy {
		if sec.Key == key {
			return sec, true
		}
	}
	return queueSection{}, false
}

// nextSelection reads the queue position a decision form posted back and keeps
// it only while it is still pending -- another tab may have decided it in the
// meantime, and redirecting to a resolved proposal would bounce straight back
// out with an error. An empty result means the bare list, which auto-selects.
func (s *Service) nextSelection(r *http.Request) string {
	next := strings.TrimSpace(r.PostFormValue("next"))
	if next == "" {
		return ""
	}
	p, ok, err := store.ProposalByID(r.Context(), s.cfg.DB, next)
	if err != nil || !ok || p.Status != store.ProposalPending {
		return ""
	}
	return next
}

// gardenerRequest interprets a natural-language maintenance request into pending
// proposals and redirects back to the page, flashing the outcome. The request
// never mutates a memory -- it only ever creates proposals for review.
func (s *Service) gardenerRequest(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Gardener == nil {
		s.serverError(w, r, errNoGardener)
		return
	}
	text := strings.TrimSpace(r.PostFormValue("request"))
	if text == "" {
		redirectFlash(w, r, "enter a request")
		return
	}
	// The scope select offers "All projects" as its empty option, so an empty
	// project here is a choice the owner made from a closed list, not an absent
	// argument -- the console has no session to infer from and nothing to be
	// ambiguous about. Saying so explicitly is what lets the gardener stop reading
	// "" as "everything", which on the agent surface was a silent whole-machine
	// scan rather than a deliberate one.
	scope := gardener.RequestScope{Project: r.PostFormValue("project")}
	scope.AllProjects = scope.Project == ""

	res, err := s.cfg.Gardener.Request(r.Context(), text, scope)
	if err != nil {
		s.logger.Warn("console: gardener request", "error", err)
		redirectFlash(w, r, err.Error())
		return
	}
	// A recognized split of a known project chains straight into split planning:
	// both steps only ever create reviewable proposals, so there is nothing to
	// confirm in between and no reason to make the owner retype the request.
	if res.SplitSource != "" {
		sres, err := s.cfg.Gardener.Split(r.Context(), res.SplitSource, text)
		if err != nil {
			s.logger.Warn("console: gardener request split", "source", res.SplitSource, "error", err)
			redirectFlash(w, r, err.Error())
			return
		}
		if sres.Total == 0 {
			redirectNotice(w, r, fmt.Sprintf("Recognized a split of %s, but no proposals matched -- try naming the child projects.", res.SplitSource))
			return
		}
		redirectNotice(w, r, fmt.Sprintf("Recognized a split of %s: %d proposal(s) below.", res.SplitSource, sres.Total))
		return
	}
	// Split intent without a known source: bounce the request text back so the
	// page renders the inline project picker follow-up under the ask box.
	if res.SplitIntent {
		http.Redirect(w, r, "/console/gardener?split="+url.QueryEscape(text), http.StatusSeeOther)
		return
	}
	if res.Total == 0 {
		redirectNotice(w, r, "No proposals matched that request -- try rephrasing.")
		return
	}
	redirectNotice(w, r, fmt.Sprintf("Generated %d proposal(s) -- review below.", res.Total))
}

// gardenerSplit interprets a project-split request into a plan batch of pending
// proposals (one split setup + one reproject per memory) and redirects back with
// the outcome. Like every request it only ever creates proposals.
func (s *Service) gardenerSplit(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Gardener == nil {
		s.serverError(w, r, errNoGardener)
		return
	}
	source := strings.TrimSpace(r.PostFormValue("source"))
	if source == "" {
		redirectFlash(w, r, "choose a project to split")
		return
	}
	res, err := s.cfg.Gardener.Split(r.Context(), source, r.PostFormValue("instruction"))
	if err != nil {
		s.logger.Warn("console: gardener split", "source", source, "error", err)
		redirectFlash(w, r, err.Error())
		return
	}
	if res.Total == 0 {
		redirectNotice(w, r, "No split proposals matched -- try naming the child projects.")
		return
	}
	redirectNotice(w, r, fmt.Sprintf("Planned split of %s: %d proposal(s) below.", source, res.Total))
}

// gardenerRetarget rewrites a pending reproject proposal's destination project
// before it is applied, so the owner can correct a mis-classified memory without
// dismissing and re-asking. It only touches reproject proposals.
func (s *Service) gardenerRetarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	to := strings.TrimSpace(r.PostFormValue("to"))
	if to == "" {
		redirectFlash(w, r, "choose a target project")
		return
	}
	p, ok, err := store.ProposalByID(r.Context(), s.cfg.DB, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok || p.Kind != store.ProposalReproject {
		redirectFlash(w, r, "not a reproject proposal")
		return
	}
	p.Payload["to"] = to
	if err := store.UpdateProposalPayload(r.Context(), s.cfg.DB, id, p.Payload); err != nil {
		s.logger.Warn("console: gardener retarget", "id", id, "error", err)
		redirectFlash(w, r, err.Error())
		return
	}
	redirectNotice(w, r, fmt.Sprintf("Retargeted %s to %s.", payloadStr(p.Payload, "name"), to))
}

// gardenerApplyPlan applies every pending proposal in a plan, setup (split) first
// so the child/shared projects and family exist before the memories move. It is
// best-effort: it applies what it can and reports how many landed, leaving any
// that error still pending.
func (s *Service) gardenerApplyPlan(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Gardener == nil {
		s.serverError(w, r, errNoGardener)
		return
	}
	slug := r.PathValue("slug")
	proposals, err := store.PendingProposals(r.Context(), s.cfg.DB, "")
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// Setup (split) proposals first, then the rest, so targets exist before moves.
	var batch []store.Proposal
	for _, p := range proposals {
		if payloadStr(p.Payload, "plan") == slug && p.Kind == store.ProposalSplit {
			batch = append(batch, p)
		}
	}
	for _, p := range proposals {
		if payloadStr(p.Payload, "plan") == slug && p.Kind != store.ProposalSplit {
			batch = append(batch, p)
		}
	}
	if len(batch) == 0 {
		redirectFlash(w, r, "no pending proposals in that plan")
		return
	}
	applied, settled, firstErr := 0, 0, ""
	for _, p := range batch {
		res, aerr := s.cfg.Gardener.Apply(r.Context(), p.ID)
		switch {
		case aerr != nil:
			if firstErr == "" {
				firstErr = aerr.Error()
			}
		case payloadStr(res, "settled") != "":
			settled++
		default:
			applied++
		}
	}
	if firstErr != "" {
		redirectFlash(w, r, fmt.Sprintf("applied %d of %d -- first error: %s", applied, len(batch), firstErr))
		return
	}
	if settled > 0 {
		redirectNotice(w, r, fmt.Sprintf("Applied %d proposal(s) in %s; settled %d whose memory had already gone inactive.",
			applied, slug, settled))
		return
	}
	redirectNotice(w, r, fmt.Sprintf("Applied all %d proposal(s) in %s.", applied, slug))
}

// gardenerRedirect completes a mutation the PRG way, sending the owner back to
// the queue with a flash. sel names the proposal the reader should open (empty
// = the bare list, which auto-selects the top of the queue); param is "notice"
// or "error".
//
// Carrying the selection in the redirect is what keeps triage moving: the next
// page render decides what is selected, so the morph after a decision lands on
// a real proposal rather than an empty pane.
func gardenerRedirect(w http.ResponseWriter, r *http.Request, sel, param, msg string) {
	path := "/console/gardener"
	if sel != "" {
		path += "/" + sel
	}
	http.Redirect(w, r, path+"?"+param+"="+url.QueryEscape(msg), http.StatusSeeOther)
}

func redirectFlash(w http.ResponseWriter, r *http.Request, msg string) {
	gardenerRedirect(w, r, "", "error", msg)
}

func redirectNotice(w http.ResponseWriter, r *http.Request, msg string) {
	gardenerRedirect(w, r, "", "notice", msg)
}

// payloadFloat reads a numeric field from a payload map (0 if absent).
func payloadFloat(p map[string]any, key string) float64 {
	if p == nil {
		return 0
	}
	if v, ok := p[key].(float64); ok {
		return v
	}
	return 0
}
