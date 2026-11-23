package console

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/devices"
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
//
// The copy-relocate pass files the same kind for a stamped memory whose
// sources all lie in one other project (Basis gardener.BasisCitedSources):
// From is then the project it is in, Cited the references its body makes, and
// Isolation the destination's.
type relocateView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind,omitempty"`
	Description string   `json:"description,omitempty"`
	To          string   `json:"to"`
	Session     string   `json:"session,omitempty"`
	Isolation   string   `json:"isolation,omitempty"`
	From        string   `json:"from,omitempty"`
	Basis       string   `json:"basis,omitempty"`
	Cited       []string `json:"cited,omitempty"`
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
	// Basis is which pass filed a proposal of a kind several passes share:
	// gardener.BasisSourceChanged on a stale copy's archive,
	// gardener.BasisCitedSources on a stamped copy's relocate, "" otherwise.
	Basis string `json:"-"`
	// StaleSources are a stale copy's cited sources that changed after it was
	// last written (Basis gardener.BasisSourceChanged).
	StaleSources []staleSourceView `json:"-"`
	// MoveWarning is set on a reproject or relocate of a stamped memory into
	// a project less fenced than one it may hold content from: the apply
	// step warns before the owner moves it (moveWarning). It marks only.
	MoveWarning *moveWarningView `json:"-"`
	// MayContain is a digest's provenance line: what the note it writes will
	// record it may contain content from, "" for none.
	MayContain string `json:"-"`
	// Why is why the proposal was raised, as one plain sentence
	// (proposalWhy). The payload's reason is the pass's own shorthand ("no
	// activity in 90d"), which ?format=json keeps as it is.
	Why string `json:"-"`
	// KeepEv and DropEv (a merge) and ArchiveEv (an archive) are the evidence
	// to decide on: each memory as it is now, with its body and how agents use
	// it, read through the request's handle (memoryEvidence). Only the
	// proposal open in the reader carries them (withEvidence); nil when the
	// memory is gone or this request may not read it.
	KeepEv    *memEvidence `json:"-"`
	DropEv    *memEvidence `json:"-"`
	ArchiveEv *memEvidence `json:"-"`
	// BulkBlock says why Apply all may not take this proposal, "" when it may
	// (applyAllBlock).
	BulkBlock string `json:"-"`
}

// memEvidence is one memory as a merge or an archive would leave it: what it
// says and how agents use it now, so the decision rests on more than a name
// and a description.
type memEvidence struct {
	ID          string
	Name        string
	Project     string
	Kind        string
	Description string
	// Status is active, superseded or archived: a memory that left active
	// context since the proposal makes the proposal moot.
	Status  string
	Updated time.Time
	// Source is who wrote it: the writing session's name (SourceHref its page,
	// when this request can show it), else the stamp as stored; Model the
	// model that produced it. Both "" when nothing was recorded.
	Source     string
	SourceHref string
	Model      string
	// Injects is how many times briefings and recall delivered it, and
	// LastInjected the latest; Delivered says both in words.
	Injects      int
	LastInjected *time.Time
	Delivered    string
	Body         template.HTML
	BodyLoaded   bool
}

// queueSection is one taxonomy group in the review rail: the pending proposals
// that call for the same kind of decision, newest first.
type queueSection struct {
	Key   string         `json:"-"`
	Label string         `json:"-"`
	Icon  string         `json:"-"`
	Rows  []proposalCard `json:"-"`
	// ApplyAll offers Apply all: every row can be undone and asks for no
	// read first (applyAllBlock). A section holding a consolidation never
	// offers it.
	ApplyAll bool `json:"-"`
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

// decisionRow is one decided proposal: a row of the decision history and of
// the rail's "Recently decided" section. It says what was decided, the
// outcome, who decided it, what an apply produced, and whether it can be
// undone.
type decisionRow struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Icon       string     `json:"-"`
	Tone       string     `json:"-"`
	RowTitle   string     `json:"title"`
	RowDetail  string     `json:"detail,omitempty"`
	Status     string     `json:"status"`  // applied | dismissed | hidden
	Outcome    string     `json:"outcome"` // the status in words; "settled" for a moot proposal
	Settled    string     `json:"settled,omitempty"`
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
	// By is who decided, in words (decider); ByHref links the deciding
	// session when this console can show it.
	By      string `json:"by"`
	ByTitle string `json:"-"`
	ByHref  string `json:"byHref,omitempty"`
	// Produced links what an apply made: the note a digest wrote, the task a
	// knowledge gap or an error pattern opened, when this console can show it.
	Produced []producedLink `json:"produced,omitempty"`
	CanUndo  bool           `json:"canUndo"`
	// CanAct mirrors gardenerData's, so a row renders its Undo from the row
	// alone wherever it is listed.
	CanAct bool `json:"-"`
}

// producedLink is one thing an apply made, linked.
type producedLink struct {
	Label string `json:"label"` // note | task
	Title string `json:"title"`
	Href  string `json:"href"`
}

// decisionsView is one page of the decision history (gardenerDecisions).
type decisionsView struct {
	// Auto is the "Applied automatically" section, on page 1 only: every
	// proposal automatic apply (gardener.ByAuto) applied that is still
	// applied, uncapped, each with its Undo. AutoCount is its length on every
	// page. Rows, Pages and Total page through every other decision, so each
	// decision is listed once.
	Auto      []decisionRow `json:"auto,omitempty"`
	AutoCount int           `json:"autoCount"`
	Rows      []decisionRow `json:"rows"`
	Page      int           `json:"page"`
	Pages     int           `json:"pages"`
	PerPage   int           `json:"perPage"`
	Total     int           `json:"total"`
	// From and To number this page's rows within the whole history.
	From, To int    `json:"-"`
	Prev     string `json:"-"`
	Next     string `json:"-"`
	CanAct   bool   `json:"-"`
}

// statusPiece is one phrase of the Gardener's status line.
type statusPiece struct {
	Text  string `json:"text"`
	Title string `json:"title,omitempty"`
	Tone  string `json:"tone,omitempty"` // "" or "warn"
}

// gardenerStatusView is the status line under the Gardener's head: whether
// background suggestions run, the last pass and the next, the checks a pass
// skips and those that failed, and whether the owner may run one now. It
// carries no counts: a pass spans every project, a locked one included.
type gardenerStatusView struct {
	Proposing bool          `json:"proposing"`
	Running   bool          `json:"running"`
	Line      []statusPiece `json:"line"`
	// Note explains the off state: what still runs, and how to turn it on.
	Note   string `json:"note,omitempty"`
	CanRun bool   `json:"canRun"`
	// Empty is what an empty queue says, and Tidy whether it may call the
	// garden tidy: only after a pass that ran every check.
	Empty string `json:"empty"`
	Tidy  bool   `json:"tidy"`
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

	// Status is the status line: background passes, last and next.
	Status *gardenerStatusView `json:"status,omitempty"`

	// Contract is the screen's own sentence: who may apply a proposal, and
	// which kinds apply automatically (gardenerContract).
	Contract gardenerContractView `json:"-"`

	// Inbox state for the HTML rail + reader. All json:"-": the CLI reads
	// Groups/Cards/counts and that shape is fixed.
	Sections []queueSection `json:"-"`
	Recent   []decisionRow  `json:"-"`
	// Decided is how many proposals have been decided in all: Recent shows
	// the newest, the decision history pages through the rest.
	Decided int         `json:"-"`
	Hidden  []hiddenRow `json:"-"`
	// History is the decision history page (gardenerDecisions); nil on the
	// queue.
	History *decisionsView `json:"-"`
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
	proposals, err := store.PendingProposals(ctx, s.db(ctx), "")
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
	recent, decided := s.recentDecisions(ctx)
	return gardenerData{
		Groups:          groups,
		Cards:           ungroupedRows(sections),
		Sections:        sections,
		Status:          s.gardenerStatus(),
		Contract:        gardenerContract(s.gardenerSwitches(ctx)),
		Recent:          recent,
		Decided:         decided,
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
			s.withEvidence(r.Context(), first)
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
	p, found, err := store.ProposalByID(ctx, s.db(ctx), id)
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
	s.withEvidence(ctx, card)
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
		sec.ApplyAll = true
		for i := range rows {
			rows[i].GroupKey, rows[i].GroupLabel = sec.Key, sec.Label
			if rows[i].BulkBlock != "" {
				sec.ApplyAll = false
			}
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

// recentDecisions projects the newest decided proposals into the rail's undo
// affordance, and counts every decision for its link to the history.
// Best-effort: a query error costs the section, not the page.
func (s *Service) recentDecisions(ctx context.Context) ([]decisionRow, int) {
	total, err := store.CountResolvedProposals(ctx, s.db(ctx))
	if err != nil {
		s.logger.Warn("console: count gardener decisions", "error", err)
		return nil, 0
	}
	if total == 0 {
		return nil, 0
	}
	ps, err := store.ResolvedProposals(ctx, s.db(ctx), 0, recentDecisionLimit)
	if err != nil {
		s.logger.Warn("console: recent gardener decisions", "error", err)
		return nil, 0
	}
	return s.decisionRows(ctx, ps), total
}

// recentDecisionLimit caps the rail's "Recently decided" section, a
// second-thought affordance; the decision history (decisionsPerPage a page)
// holds every decision.
const (
	recentDecisionLimit = 10
	decisionsPerPage    = 20
)

// decisionRows projects decided proposals into history rows, through the
// request's handle: the session that decided and what an apply produced are
// named only when this request may read them.
func (s *Service) decisionRows(ctx context.Context, ps []store.Proposal) []decisionRow {
	deciders := map[string]decider{}
	out := make([]decisionRow, 0, len(ps))
	for _, p := range ps {
		_, _, iconName, tone := proposalPresentation(p.Kind)
		title, detail := rowSummary(p.Kind, s.toProposalCard(ctx, p))
		row := decisionRow{
			ID: p.ID, Kind: p.Kind, Icon: iconName, Tone: tone, RowTitle: title, RowDetail: detail,
			Status: p.Status, Outcome: outcomeWord(p.Status), ResolvedAt: p.ResolvedAt,
			Settled: payloadStr(p.Result, "settled"),
			CanUndo: gardener.CanUndoResolution(p.Status, p.Kind),
			CanAct:  s.cfg.Gardener != nil,
		}
		if row.Settled != "" {
			row.Outcome = "settled"
		}
		d, ok := deciders[p.ResolvedBy]
		if !ok {
			d = s.decider(ctx, p.ResolvedBy)
			deciders[p.ResolvedBy] = d
		}
		row.By, row.ByTitle, row.ByHref = d.label, d.title, d.href
		if p.Status == store.ProposalApplied {
			row.Produced = s.produced(ctx, p.Result)
		}
		out = append(out, row)
	}
	return out
}

// outcomeWord is a decided proposal's status in words.
func outcomeWord(status string) string {
	switch status {
	case store.ProposalHidden:
		return "hidden forever"
	default:
		return status
	}
}

// decider is who decided a proposal, in words: label for the row, title for
// its tooltip, href to the deciding session's page.
type decider struct{ label, title, href string }

// decider turns resolved_by (gardener.By*, or a session's name) into words. A
// session is named only when it resolves through the request's handle, so a
// browser that may not read a locked project's sessions never reads their
// names here either; such a session is "an agent session".
func (s *Service) decider(ctx context.Context, by string) decider {
	switch by {
	case "":
		return decider{"unknown", "Decided before Seamless recorded who decides, or by a release that does not record it.", ""}
	case gardener.ByOwner:
		return decider{"you", "You, on the console.", ""}
	case gardener.ByGardener:
		return decider{"the gardener", "Settled by the gardener's upkeep: what it was about had already gone inactive.", ""}
	case gardener.ByAuto:
		return decider{"automatic apply", "Applied automatically.", ""}
	case gardener.ByAgent:
		return decider{"an agent", "An agent whose call no session named.", ""}
	}
	name, ok := gardener.SessionNameOf(by)
	if !ok {
		return decider{"unknown", "", ""}
	}
	sess, found, err := store.SessionByName(ctx, s.db(ctx), name)
	if err != nil {
		s.logger.Warn("console: gardener decider", "error", err)
	}
	if err != nil || !found {
		return decider{"an agent session", "An agent session this console cannot show.", ""}
	}
	return decider{name, "An agent, in session " + name + ".", "/console/sessions/" + sess.ID}
}

// produced links what an apply made, read through the request's handle: the
// note a digest wrote and the task a knowledge gap or an error pattern opened,
// each only while it is there to open.
func (s *Service) produced(ctx context.Context, result map[string]any) []producedLink {
	var out []producedLink
	if id := payloadStr(result, "note_id"); id != "" {
		n, ok, err := store.NoteByID(ctx, s.db(ctx), id)
		if err != nil {
			s.logger.Warn("console: gardener produced note", "id", id, "error", err)
		}
		if err == nil && ok {
			out = append(out, producedLink{Label: "note", Title: n.Title, Href: "/console/notes/" + id})
		}
	}
	if id := payloadStr(result, "task_id"); id != "" {
		t, err := store.TaskByID(ctx, s.db(ctx), id)
		if err != nil && !errors.Is(err, store.ErrTaskNotFound) {
			s.logger.Warn("console: gardener produced task", "id", id, "error", err)
		}
		if err == nil {
			out = append(out, producedLink{Label: "task", Title: t.Title, Href: "/console/tasks/" + id})
		}
	}
	return out
}

// gardenerStatus builds the status line from the gardener's bookkeeping.
func (s *Service) gardenerStatus() *gardenerStatusView {
	if s.cfg.Gardener == nil {
		return &gardenerStatusView{Empty: "The garden is tidy. Nothing is waiting on you.", Tidy: true}
	}
	return statusView(s.cfg.Gardener.Status(), time.Now())
}

// statusView is the status line for st as of now.
func statusView(st gardener.Status, now time.Time) *gardenerStatusView {
	v := &gardenerStatusView{Proposing: st.Proposing, Running: st.Running != nil}
	every := plural(int(st.Interval.Minutes()), "minute", "minutes")
	if !st.Proposing {
		v.Line = append(v.Line, statusPiece{Text: "Suggestions are off"})
		if !st.Housekept.IsZero() {
			v.Line = append(v.Line, statusPiece{Text: "upkeep ran " + agoPhrase(st.Housekept), Title: ts(st.Housekept)})
		}
		v.Note = "gardener.enabled is false in the config, so no background pass looks for duplicates, stale memories " +
			"or finished plans. Upkeep still runs every " + every + ": idle sessions are reaped and their claims " +
			"returned, old tool events are pruned, retrieval stats are refreshed, and proposals whose memory is gone " +
			"are settled. Requests you make here still propose. Set enabled: true under gardener: and restart the " +
			"daemon to turn suggestions on."
		v.Empty = "Nothing is waiting on you. Background suggestions are off, so only requests bring proposals here."
		return v
	}
	switch {
	case st.Running != nil && st.Running.Started.IsZero():
		v.Line = append(v.Line, statusPiece{Text: "A pass is starting"})
	case st.Running != nil:
		v.Line = append(v.Line, statusPiece{Text: "A pass is running", Title: "started " + ts(st.Running.Started)})
	case st.Last != nil:
		v.Line = append(v.Line, statusPiece{Text: "Last pass " + agoPhrase(st.Last.Finished), Title: ts(st.Last.Finished)})
	default:
		v.Line = append(v.Line, statusPiece{Text: "No pass yet since the daemon started"})
	}
	if st.Scheduled && !st.Next.IsZero() {
		next := "next " + untilPhrase(st.Next, now)
		if st.Last == nil && st.Running == nil {
			next = "first pass " + untilPhrase(st.Next, now)
		}
		v.Line = append(v.Line, statusPiece{Text: next, Title: "every " + every + "; " + ts(st.Next)})
	}
	skips := st.Skips
	if st.Last != nil {
		skips = st.Last.Result.Skipped
	}
	var skipped []string
	for _, sk := range skips {
		v.Line = append(v.Line, statusPiece{Text: sk.Pass + " skipped: " + sk.Reason})
		skipped = append(skipped, sk.Pass+" ("+sk.Reason+")")
	}
	var failed []string
	if st.Last != nil {
		failed = st.Last.Result.Failed
		for _, f := range failed {
			v.Line = append(v.Line, statusPiece{Text: f + " failed", Tone: "warn", Title: "The daemon log says why."})
		}
		if st.Last.Err != "" {
			v.Line = append(v.Line, statusPiece{Text: "the last pass could not start", Tone: "warn", Title: st.Last.Err})
		}
	}
	v.CanRun = st.Running == nil
	switch {
	case st.Running != nil:
		v.Empty = "Nothing is waiting on you yet: a pass is running now."
	case st.Last == nil:
		v.Empty = "Nothing is waiting on you yet: no background pass has run since the daemon started."
	case st.Last.Err != "":
		v.Empty = "Nothing is waiting on you, but the last pass could not start."
	case len(failed) > 0:
		v.Empty = "Nothing is waiting on you, but the last pass did not finish every check: " +
			approval.JoinAnd(failed) + " failed."
	case len(skipped) > 0:
		v.Empty = "Nothing is waiting on you, though some checks did not run: " + approval.JoinAnd(skipped) + "."
	default:
		v.Empty, v.Tidy = "The garden is tidy. Nothing is waiting on you.", true
	}
	return v
}

// hiddenPatterns projects the forever-blocked proposals into the rail's unhide
// list, read through the request's handle like every console read. It is
// uncapped on purpose: a hide is an explicit decision, so the list is short by
// construction, and truncating the one surface that says what is being
// suppressed would leave a block nobody can find to lift. Best-effort: a query
// error costs the section, not the page.
func (s *Service) hiddenPatterns(ctx context.Context) []hiddenRow {
	if s.cfg.Gardener == nil {
		return nil
	}
	ps, err := store.HiddenProposals(ctx, s.db(ctx))
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
		title := "Archive memory"
		if c.Basis == gardener.BasisSourceChanged {
			title = "Archive stale copy"
		}
		if c.Archive != nil {
			return title, c.Archive.Name + " · " + projectLabel(c.Archive.Project)
		}
		return title, ""
	case store.ProposalDigest:
		return "Publish digest", c.Title
	case store.ProposalConsolidate:
		return "Consolidate " + plural(len(c.Sources), "memory", "memories"), "→ " + c.NewName
	case store.ProposalReproject:
		if c.Reproject != nil {
			return "Move memory", fmt.Sprintf("%s: %s → %s",
				c.Reproject.Name, projectLabel(c.Reproject.From), projectLabel(c.Reproject.To))
		}
		return "Move memory", ""
	case store.ProposalRelocate:
		if c.Basis == gardener.BasisCitedSources {
			if c.Relocate != nil {
				return "Relocate stamped copy", fmt.Sprintf("%s: %s → %s",
					c.Relocate.Name, projectLabel(c.Relocate.From), c.Relocate.To)
			}
			return "Relocate stamped copy", ""
		}
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
			return "Mark plan shipped", shownTitle(c.ShipPlan.Title) + " · " +
				plural(c.ShipPlan.MatchedCount, "matching commit", "matching commits")
		}
		return "Mark plan shipped", ""
	case store.ProposalMergePlans:
		if c.MergePlans != nil {
			return "Fold plan into another", shownTitle(c.MergePlans.Title) + " → " + c.MergePlans.IntoSlug
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
	projs, err := store.ListProjects(ctx, s.db(ctx))
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
		c.MayContain = digestMayContain(p.Payload, c.Project)
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
			From: payloadStr(p.Payload, "from"), Basis: payloadStr(p.Payload, "basis"),
			Cited: payloadStrList(p.Payload, "cited"),
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
	stampedCard(&c, p)
	c.RowTitle, c.RowDetail = rowSummary(p.Kind, c)
	c.CanUndoApply = gardener.CanUndoApply(p.Kind)
	c.CanAct = s.cfg.Gardener != nil
	c.Fenced = s.fencedTargets(ctx, p)
	c.MoveWarning = s.moveWarning(ctx, p)
	c.Why = proposalWhy(p.Kind, p.Payload, c)
	c.BulkBlock = applyAllBlock(c)
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
//
// The stamped-copy passes' proposals are about fenced projects by
// construction -- their stamps name only fenced ones -- so the mark would say
// nothing about evidence predating a fence there either. A stamped copy's
// relocate carries the move warning instead, read now (moveWarning); a stale
// copy's archive is marked as any archive of its memory would be, by the
// copy's own project alone, its sources' projects aside.
func (s *Service) fencedTargets(ctx context.Context, p store.Proposal) []store.ProjectIsolation {
	if p.Kind == store.ProposalRelocate {
		return nil
	}
	if p.Kind == store.ProposalArchive && payloadStr(p.Payload, "basis") == gardener.BasisSourceChanged {
		copyOnly := make(map[string]any, len(p.Payload))
		for k, v := range p.Payload {
			if k != "changed_sources" && k != "basis" {
				copyOnly[k] = v
			}
		}
		p.Payload = copyOnly
	}
	fenced, err := gardener.FencedProjects(ctx, s.db(ctx), p)
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

// withEvidence loads what the reader needs to decide on a merge or an
// archive: each memory as it is now (memoryEvidence). Only the proposal the
// reader shows pays for it: the rail renders every pending proposal and reads
// none of their bodies.
func (s *Service) withEvidence(ctx context.Context, c *proposalCard) {
	switch c.Kind {
	case store.ProposalMerge:
		if c.Keep != nil {
			c.KeepEv = s.memoryEvidence(ctx, c.Keep.ID)
		}
		if c.Drop != nil {
			c.DropEv = s.memoryEvidence(ctx, c.Drop.ID)
		}
	case store.ProposalArchive:
		if c.Archive != nil {
			c.ArchiveEv = s.memoryEvidence(ctx, c.Archive.ID)
		}
	}
}

// memoryEvidence reads one memory for the reader through the request's
// handle: its body, who wrote it, when it last changed, and how often agents
// were given it. A memory the request may not read -- a locked project's,
// under peer_auth -- does not exist on that handle, so nothing of it is
// shown; nor is a memory that is gone. Best-effort: a failed read costs the
// evidence, never the page.
func (s *Service) memoryEvidence(ctx context.Context, id string) *memEvidence {
	if id == "" {
		return nil
	}
	m, ok, err := store.MemoryByID(ctx, s.db(ctx), id)
	if err != nil {
		s.logger.Warn("console: gardener evidence memory", "id", id, "error", err)
		return nil
	}
	if !ok {
		return nil
	}
	ev := &memEvidence{
		ID: m.ID, Name: m.Name, Project: m.Project, Kind: string(m.Kind), Description: m.Description,
		Status: "active", Updated: m.Updated, Model: m.Model, Delivered: "never",
	}
	if !m.Active() {
		ev.Status = "archived"
		if m.SupersededBy != "" {
			ev.Status = "superseded"
		}
	}
	if s.cfg.Files != nil {
		if full, ferr := s.cfg.Files.Store().ReadMemory(m.FilePath); ferr != nil {
			s.logger.Warn("console: gardener evidence body", "id", m.ID, "error", ferr)
		} else if strings.TrimSpace(full.Body) != "" {
			ev.Body = s.renderBody(ctx, full.Body, m.Project)
			ev.BodyLoaded = true
		}
	}
	if st, found, serr := store.GetRetrievalStat(ctx, s.db(ctx), m.ID); serr != nil {
		s.logger.Warn("console: gardener evidence stat", "id", m.ID, "error", serr)
	} else if found {
		ev.Injects, ev.LastInjected = st.InjectCount, st.LastInjectedAt
	}
	switch ev.Injects {
	case 0:
	case 1:
		ev.Delivered = "once"
	default:
		ev.Delivered = strconv.Itoa(ev.Injects) + " times"
	}
	if m.SourceSession != "" {
		resolve := s.sourceSessionResolver(ctx)
		ev.Source = sourceSessionLabel(resolve, m.SourceSession)
		if sess := resolve(m.SourceSession); sess.ID != "" {
			ev.SourceHref = "/console/sessions/" + sess.ID
		}
	}
	return ev
}

// applyAllBlock says why Apply all may not take a proposal, "" when it may.
// Apply all is for the decisions that cost nothing to take back: an apply
// that cannot be undone (consolidate, split) is never one of them, and
// neither is a proposal whose reader asks the owner to read it first -- one
// touching a project fenced since it was raised, or a move of a stamped
// memory into a less fenced project. Both marks block nothing on their own;
// they keep the proposal out of a decision made without opening it.
func applyAllBlock(c proposalCard) string {
	switch {
	case !c.CanUndoApply:
		return "applying it cannot be undone"
	case len(c.Fenced) > 0:
		return "it touches a project fenced since it was proposed"
	case c.MoveWarning != nil:
		return "it moves a stamped memory into a less fenced project"
	}
	return ""
}

// gardenerContractView is the Gardener screen's own sentence: what a
// proposal changes, who may apply one, and which kinds apply on their own,
// with the way to Settings, where both are chosen.
type gardenerContractView struct {
	// Auto is true when some kinds apply automatically; Badge is the pill.
	Auto  bool
	Badge string
	Copy  string
	Href  string
}

// gardenerSettingsHref is where the owner chooses who may apply proposals and
// which kinds apply automatically: Settings > Gardener, a Basic section like
// this screen.
const gardenerSettingsHref = "/console/settings?s=" + gardenerSettingsSection + "#" + gardenerSettingsAnchor

// contractKindWords names an automatically applied kind in the screen's sentence.
var contractKindWords = map[string]string{
	store.ProposalMemoryWanted: "missing memories",
	store.ProposalToolError:    "recurring errors",
	store.ProposalShipPlan:     "shipped plans",
	store.ProposalAbandonPlan:  "stale plans",
}

// gardenerContract says what is true for these switches: whether agents may
// apply a proposal, and which kinds apply as soon as a pass finds them.
func gardenerContract(sw gardenerSwitches) gardenerContractView {
	v := gardenerContractView{Badge: "Proposes only", Href: gardenerSettingsHref}
	who := "You and your agents may apply one, and the decision history shows who did."
	if !sw.AgentsMayApply {
		who = "Only you may apply one; agents are refused, and the decision history shows each decision."
	}
	auto := "None applies automatically."
	if len(sw.AutoApply) > 0 {
		words := make([]string, 0, len(sw.AutoApply))
		for _, kind := range sw.AutoApply {
			w, ok := contractKindWords[kind]
			if !ok {
				w = strings.ReplaceAll(kind, "_", " ") + " proposals"
			}
			words = append(words, w)
		}
		auto = whySentence(approval.JoinAnd(words)) + " apply automatically, as soon as a pass finds them."
		v.Auto, v.Badge = true, "Some apply automatically"
	}
	v.Copy = "A proposal changes nothing until it is applied. " + who + " " + auto
	return v
}

// proposalWhy is why a proposal was raised, as one plain sentence. Most
// passes write their reason as shorthand ("stage with no parsable Status
// header, no update in 14d"), which plainReason puts into words; a request's
// reason is the model's own wording, kept; and a kind whose payload carries no
// reason says why from its own evidence. Every kind says something
// (TestGtProposalWhy_EveryKindReadsAsASentence).
func proposalWhy(kind string, p map[string]any, c proposalCard) string {
	request := c.Source == "request"
	if r := strings.TrimSpace(payloadStr(p, "reason")); r != "" && (!request || r != "requested") {
		return plainReason(r)
	}
	switch kind {
	case store.ProposalArchive:
		if request {
			return "You asked for it to be archived."
		}
		return "A background pass found it no longer used."
	case store.ProposalMerge:
		if request {
			return "You asked for these two to be merged."
		}
		return fmt.Sprintf("A background pass found these two memories %d%% alike, so one of them may be enough.", c.ScorePercent)
	case store.ProposalDigest:
		return fmt.Sprintf("%s in %s recorded findings during %s.",
			whySentence(plural(c.SessionCount, "session", "sessions")), projectLabel(c.Project), c.Month)
	case store.ProposalConsolidate:
		if request {
			return "You asked for these memories to be combined into one."
		}
		return fmt.Sprintf("These %d memories cover the same ground.", len(c.Sources))
	case store.ProposalReproject:
		if c.Plan != "" {
			return "It is one of the moves in a project split you asked for."
		}
		if request {
			return "You asked for it to move to another project."
		}
		return "It looks filed under the wrong project."
	case store.ProposalRekind:
		if request {
			return "You asked for it to be reclassified."
		}
		return "It looks filed under the wrong kind."
	case store.ProposalRelocate:
		return "It sits outside the fence of the project it belongs to."
	case store.ProposalSplit:
		if c.Split != nil && c.Split.Source != "" {
			return "You asked for " + c.Split.Source + " to be split."
		}
		return "You asked for a project to be split."
	case store.ProposalAbandonPlan, store.ProposalShipPlan, store.ProposalMergePlans:
		return "Nobody approved this captured plan."
	case store.ProposalMemoryWanted:
		if w := c.MemoryWanted; w != nil && w.MissCount > 0 {
			return fmt.Sprintf("Agents searched for this %s across %s, and recall found nothing.",
				plural(w.MissCount, "time", "times"), plural(w.SessionCount, "session", "sessions"))
		}
		return "Agents searched for this, and recall found nothing."
	case store.ProposalToolError:
		if e := c.ToolError; e != nil && e.ErrorCount > 0 && e.Key != "" {
			return fmt.Sprintf("Agents' calls to %s returned this error %s across %s.",
				e.Key, plural(e.ErrorCount, "time", "times"), plural(e.SessionCount, "session", "sessions"))
		}
		return "Agents keep hitting this error."
	}
	return "The gardener raised it for your review."
}

// reasonShapes are the reasons the background passes write, each with its
// plain sentence. A reason that matches none -- a request's, a pass's new
// wording -- is kept, sentence-cased (plainReason): the words are still the
// pass's own, never a guess.
var reasonShapes = []struct {
	re  *regexp.Regexp
	say func(m []string) string
}{
	{regexp.MustCompile(`^no activity in (\d+)d$`), func(m []string) string {
		return "Nothing has updated, delivered or read it in " + reasonCount(m[1], "day", "days") +
			", and no other memory links to it."
	}},
	{regexp.MustCompile(`^stage with no parsable Status header, no update in (\d+)d$`), func(m []string) string {
		return "It is a stage with no Status line Seamless can read, so it gates nothing, and it has not changed in " +
			reasonCount(m[1], "day", "days") + "."
	}},
	{regexp.MustCompile(`^stage with status (.+), no update in (\d+)d$`), func(m []string) string {
		return "It is a stage whose Status line reads " + m[1] + ", so it gates nothing, and it has not changed in " +
			reasonCount(m[2], "day", "days") + "."
	}},
	{regexp.MustCompile(`^dead weight: briefing-injected (\d+)x in (\d+)d, zero recall/prompt/read demand$`), func(m []string) string {
		return "Briefings gave it to agents " + reasonCount(m[1], "time", "times") + " in " + reasonCount(m[2], "day", "days") +
			", and not one search, prompt or read asked for it."
	}},
	{regexp.MustCompile(`^knowledge gap: recall found nothing (\d+)x across (\d+) sessions? in (\d+)d$`), func(m []string) string {
		return "Agents searched for this " + reasonCount(m[1], "time", "times") + " across " + reasonCount(m[2], "session", "sessions") +
			" in " + reasonCount(m[3], "day", "days") + ", and recall found nothing."
	}},
	{regexp.MustCompile(`^recurring tool error: (.+) returned this error (\d+)x across (\d+) sessions? in (\d+)d$`), func(m []string) string {
		return "Agents' calls to " + m[1] + " returned this error " + reasonCount(m[2], "time", "times") + " across " +
			reasonCount(m[3], "session", "sessions") + " in " + reasonCount(m[4], "day", "days") + "."
	}},
	{regexp.MustCompile(`^recurring hook failure: stage "(.+)" failed (\d+)x in (\d+)d, swallowed fail-open$`), func(m []string) string {
		return "The " + m[1] + " hook stage failed " + reasonCount(m[2], "time", "times") + " in " + reasonCount(m[3], "day", "days") +
			"; a hook carries on past a failure, so no agent saw it."
	}},
	{regexp.MustCompile(`^unapproved for (\d+)d, but (\d+) of (\d+) commit\(s\) since its capture match the plan -- the work appears to have shipped$`), func(m []string) string {
		return "Nobody approved it in " + reasonCount(m[1], "day", "days") + ", but " + m[2] + " of the " +
			reasonCount(m[3], "commit", "commits") + " since it was captured match the plan, so the work looks shipped."
	}},
	{regexp.MustCompile(`^unapproved for (\d+)d and carries no steps of its own, while plan "(.+)" in the same project holds the steps for what looks like the same work \((\d+) shared title words\)$`), func(m []string) string {
		return "Nobody approved it in " + reasonCount(m[1], "day", "days") + " and it has no steps of its own, while plan " + m[2] +
			" in the same project holds the steps for what looks like the same work: their titles share " +
			reasonCount(m[3], "word", "words") + "."
	}},
	{regexp.MustCompile(`^never approved in (\d+)d; repo unchanged since the capture$`), func(m []string) string {
		return "Nobody approved it in " + reasonCount(m[1], "day", "days") + ", and its repository has not changed since it was captured."
	}},
	{regexp.MustCompile(`^never approved in (\d+)d; (\d+) commit\(s\) since the capture, none matching the plan$`), func(m []string) string {
		return "Nobody approved it in " + reasonCount(m[1], "day", "days") + ", and none of the " +
			reasonCount(m[2], "commit", "commits") + " since it was captured match the plan."
	}},
	{regexp.MustCompile(`^never approved in (\d+)d$`), func(m []string) string {
		return "Nobody approved it in " + reasonCount(m[1], "day", "days") + "."
	}},
	{regexp.MustCompile(`^written by a session bound to (.+), which is now (\S+) -- this memory stayed in the global scope, outside the fence$`), func(m []string) string {
		return "A session bound to " + m[1] + " wrote it, and " + m[1] + " is now " + m[2] +
			", but the memory stayed in the global scope, outside the fence."
	}},
}

// plainReason puts a pass's reason into a plain sentence (reasonShapes), or
// keeps one it does not know as a sentence of its own.
func plainReason(reason string) string {
	for _, sh := range reasonShapes {
		if m := sh.re.FindStringSubmatch(reason); m != nil {
			return sh.say(m)
		}
	}
	out := whySentence(strings.ReplaceAll(reason, " -- ", "; "))
	if !strings.HasSuffix(out, ".") && !strings.HasSuffix(out, "?") && !strings.HasSuffix(out, "!") {
		out += "."
	}
	return out
}

// reasonCount is n (a pass's decimal count) with its noun: "1 day", "14 days".
func reasonCount(n, one, many string) string {
	if n == "1" {
		return n + " " + one
	}
	return n + " " + many
}

// whySentence upper-cases the first letter, leaving the rest as written.
func whySentence(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
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
	if !s.proposalReachable(w, r, id) {
		return
	}
	res, err := s.cfg.Gardener.Apply(deciding(r), id)
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
	if !s.proposalReachable(w, r, id) {
		return
	}
	if err := s.cfg.Gardener.Dismiss(deciding(r), id); err != nil {
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
	if !s.proposalReachable(w, r, id) {
		return
	}
	if err := s.cfg.Gardener.Hide(deciding(r), id); err != nil {
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
	if !s.proposalReachable(w, r, id) {
		return
	}
	if err := s.cfg.Gardener.Unhide(deciding(r), id); err != nil {
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
	if !s.proposalReachable(w, r, id) {
		return
	}
	if err := s.cfg.Gardener.Undo(deciding(r), id); err != nil {
		s.logger.Warn("console: gardener undo", "id", id, "error", err)
		gardenerRedirect(w, r, "", "error", err.Error())
		return
	}
	gardenerRedirect(w, r, id, "notice", "Restored the proposal to the queue.")
}

// gardenerGroupDismiss closes every pending proposal in one rail group -- a
// taxonomy section (?group=) or a split plan (?plan=) -- one at a time, so each
// keeps its own event and its own row in the decision history and stays
// individually undoable. It is best-effort: it dismisses what it can and
// reports how many landed. src= (you or auto, as the Source filter names them)
// narrows a section to the rows the filtered rail shows, so "Dismiss all N"
// dismisses those N and no more.
//
// Dismissing costs nothing but the suggestion, so any group may be dismissed
// at once. Applying changes the knowledge agents receive: in bulk it is
// gardenerGroupApply's, for a section whose every proposal can be undone, and
// a split plan's Apply whole plan.
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
	src := strings.TrimSpace(r.PostFormValue("src"))
	if src == "" {
		src = "all"
	}
	if !slices.Contains(srcFilterKeys, src) {
		s.badRequest(w, r, fmt.Sprintf("invalid src %q: valid values are %s", src, strings.Join(srcFilterKeys, ", ")))
		return
	}

	proposals, err := store.PendingProposals(ctx, s.db(ctx), "")
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
		asked := payloadStr(p.Payload, "source") == "request"
		if (src == "you" && !asked) || (src == "auto" && asked) {
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
	decide := deciding(r)
	for _, id := range batch {
		if derr := s.cfg.Gardener.Dismiss(decide, id); derr != nil {
			if firstErr == "" {
				firstErr = derr.Error()
			}
			continue
		}
		dismissed++
	}
	if firstErr != "" {
		gardenerRedirect(w, r, "", "error",
			fmt.Sprintf("dismissed %d of %d; first error: %s", dismissed, len(batch), firstErr))
		return
	}
	gardenerRedirect(w, r, "", "notice",
		fmt.Sprintf("Dismissed %s from %s. Undo any of them from the decision history.",
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

// gardenerGroupApply applies the proposals one rail section shows, one at a
// time, so each keeps its own event and its own row in the decision history,
// where Undo takes it back. The form names the section (group=) and each
// proposal it showed (id=, repeated): a proposal a pass raised after the page
// rendered is never applied unseen, and one decided meanwhile in another tab
// is skipped and counted.
//
// Only a section every proposal of which may be applied in bulk offers it
// (applyAllBlock): applying one that cannot be undone -- consolidate, split --
// or one whose reader asks to be read first is refused for the whole batch,
// before anything is applied. Split plans keep Apply whole plan. Past that
// check it is best-effort, like Apply whole plan: it applies what it can and
// reports how many landed, leaving any that error pending.
func (s *Service) gardenerGroupApply(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Gardener == nil {
		s.serverError(w, r, errNoGardener)
		return
	}
	ctx := r.Context()
	group := strings.TrimSpace(r.PostFormValue("group"))
	sec, ok := taxonomySection(group)
	if !ok {
		keys := make([]string, 0, len(queueTaxonomy))
		for _, t := range queueTaxonomy {
			keys = append(keys, t.Key)
		}
		s.badRequest(w, r, fmt.Sprintf("apply a section by group=<section>: valid values are %s", strings.Join(keys, ", ")))
		return
	}
	var batch []string
	seen := map[string]bool{}
	decided := 0
	for _, raw := range r.PostForm["id"] {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		p, found, err := store.ProposalByID(ctx, s.db(ctx), id)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if !found {
			s.notFound(w, r, "No proposal with id "+id+".")
			return
		}
		if p.Status != store.ProposalPending {
			decided++
			continue
		}
		if payloadStr(p.Payload, "plan") != "" || sectionOfKind[p.Kind] != group {
			s.badRequest(w, r, fmt.Sprintf("proposal %s is not in %s", id, sec.Label))
			return
		}
		c := s.toProposalCard(ctx, p)
		if why := applyAllBlock(c); why != "" {
			gardenerRedirect(w, r, id, "error", fmt.Sprintf("Nothing was applied: %s (%s) is not applied in bulk, because %s. Open it and decide it on its own.",
				c.RowTitle, c.RowDetail, why))
			return
		}
		batch = append(batch, id)
	}
	if len(seen) == 0 {
		s.badRequest(w, r, "name each proposal to apply with id=<proposal id>, as the section shows them")
		return
	}
	if len(batch) == 0 {
		gardenerRedirect(w, r, "", "error", "Every proposal in "+sec.Label+" was already decided.")
		return
	}
	applied, settled, firstErr := 0, 0, ""
	decide := deciding(r)
	for _, id := range batch {
		res, aerr := s.cfg.Gardener.Apply(decide, id)
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
		gardenerRedirect(w, r, "", "error",
			fmt.Sprintf("applied %d of %d in %s; first error: %s", applied, len(batch), sec.Label, firstErr))
		return
	}
	msg := fmt.Sprintf("Applied %s from %s.", plural(applied, "proposal", "proposals"), sec.Label)
	if settled > 0 {
		msg += fmt.Sprintf(" Settled %d whose memory had already gone inactive.", settled)
	}
	if decided > 0 {
		msg += fmt.Sprintf(" Skipped %d already decided.", decided)
	}
	gardenerRedirect(w, r, "", "notice", msg+" Undo any of them from the decision history.")
}

// gardenerSwap changes which memory a pending merge keeps. Apply reads the
// payload's keep and drop (gardener's applyMerge), so the swap rewrites the
// payload and nothing else changes until the merge is applied. The form names
// the memory to keep (keep=<id>), one of the two the merge names, rather than
// toggling: a repeated or crossed post lands on what the owner chose. The
// merge's key is the pair, whichever way round, so dedup is unchanged. It
// records a gardener.action event, as every Gardener decision does.
func (s *Service) gardenerSwap(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Gardener == nil {
		s.serverError(w, r, errNoGardener)
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	if !s.proposalReachable(w, r, id) {
		return
	}
	keepID := strings.TrimSpace(r.PostFormValue("keep"))
	if keepID == "" {
		s.badRequest(w, r, "swap a merge by keep=<the id of the memory to keep>")
		return
	}
	p, found, err := store.ProposalByID(ctx, s.db(ctx), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !found || p.Status != store.ProposalPending {
		gardenerRedirect(w, r, "", "error", "That proposal was already resolved.")
		return
	}
	if p.Kind != store.ProposalMerge {
		s.badRequest(w, r, "only a merge proposal can swap which memory it keeps")
		return
	}
	keep, drop := payloadMap(p.Payload, "keep"), payloadMap(p.Payload, "drop")
	switch keepID {
	case payloadStr(keep, "id"):
		gardenerRedirect(w, r, id, "notice", "This merge already keeps "+payloadStr(keep, "name")+".")
		return
	case payloadStr(drop, "id"):
	default:
		s.badRequest(w, r, fmt.Sprintf("memory %s is not one of the two this merge names", keepID))
		return
	}
	p.Payload["keep"], p.Payload["drop"] = drop, keep
	if err := store.UpdateProposalPayload(ctx, s.writeDB(), id, p.Payload); err != nil {
		s.logger.Warn("console: gardener swap", "id", id, "error", err)
		gardenerRedirect(w, r, id, "error", err.Error())
		return
	}
	s.recordDecision(r, id, map[string]any{"action": "swap", "kind": p.Kind,
		"keep": payloadStr(drop, "id"), "drop": payloadStr(keep, "id")})
	gardenerRedirect(w, r, id, "notice", fmt.Sprintf("Swapped. Applying now keeps %s and retires %s.",
		payloadStr(drop, "name"), payloadStr(keep, "name")))
}

// recordDecision appends the gardener.action event for a console decision the
// gardener service does not make itself -- a retarget, a swap -- in the shape
// the service's own decisions carry (gardener's record): the proposal as the
// item, the action, and by, who decided. The owner's is "console", as on
// every console write and as gardener's eventBy writes it; an agent's process
// is "agent" (consoleActor). Best-effort, as the service's is: the change
// already landed.
func (s *Service) recordDecision(r *http.Request, id string, payload map[string]any) {
	if s.cfg.Events == nil {
		return
	}
	by := consoleActor(r)
	if by == gardener.ByOwner {
		by = "console"
	}
	payload["by"] = by
	ctx := r.Context()
	if _, err := s.eventLog(ctx).Record(ctx, core.Event{
		Kind: core.EventGardenerAction, ItemID: id, Payload: payload,
	}); err != nil {
		s.logger.Warn("console: record gardener decision", "id", id, "action", payload["action"], "error", err)
	}
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
	p, ok, err := store.ProposalByID(r.Context(), s.db(r.Context()), next)
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

	res, err := s.cfg.Gardener.Request(deciding(r), text, scope)
	if err != nil {
		s.logger.Warn("console: gardener request", "error", err)
		redirectFlash(w, r, err.Error())
		return
	}
	// A recognized split of a known project chains straight into split planning:
	// both steps only ever create reviewable proposals, so there is nothing to
	// confirm in between and no reason to make the owner retype the request.
	if res.SplitSource != "" {
		sres, err := s.cfg.Gardener.Split(deciding(r), res.SplitSource, text)
		if err != nil {
			s.logger.Warn("console: gardener request split", "source", res.SplitSource, "error", err)
			redirectFlash(w, r, err.Error())
			return
		}
		if sres.Total == 0 {
			redirectNotice(w, r, fmt.Sprintf("Recognized a split of %s, but no proposals matched; try naming the child projects.", res.SplitSource))
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
		redirectNotice(w, r, "No proposals matched that request; try rephrasing.")
		return
	}
	redirectNotice(w, r, fmt.Sprintf("Generated %d proposal(s); review them below.", res.Total))
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
	res, err := s.cfg.Gardener.Split(deciding(r), source, r.PostFormValue("instruction"))
	if err != nil {
		s.logger.Warn("console: gardener split", "source", source, "error", err)
		redirectFlash(w, r, err.Error())
		return
	}
	if res.Total == 0 {
		redirectNotice(w, r, "No split proposals matched; try naming the child projects.")
		return
	}
	redirectNotice(w, r, fmt.Sprintf("Planned split of %s: %d proposal(s) below.", source, res.Total))
}

// gardenerRetarget rewrites a pending reproject proposal's destination project
// before it is applied, so the owner can correct a mis-classified memory without
// dismissing and re-asking. It only touches reproject proposals. Like every
// Gardener decision it records a gardener.action event (recordDecision), with
// the destination it had (from) and the one it has now (to).
func (s *Service) gardenerRetarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	to := strings.TrimSpace(r.PostFormValue("to"))
	if to == "" {
		redirectFlash(w, r, "choose a target project")
		return
	}
	p, ok, err := store.ProposalByID(r.Context(), s.db(r.Context()), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !ok || p.Kind != store.ProposalReproject {
		redirectFlash(w, r, "not a reproject proposal")
		return
	}
	from := payloadStr(p.Payload, "to")
	if from == to {
		redirectNotice(w, r, fmt.Sprintf("%s already moves to %s.", payloadStr(p.Payload, "name"), to))
		return
	}
	p.Payload["to"] = to
	if err := store.UpdateProposalPayload(r.Context(), s.writeDB(), id, p.Payload); err != nil {
		s.logger.Warn("console: gardener retarget", "id", id, "error", err)
		redirectFlash(w, r, err.Error())
		return
	}
	s.recordDecision(r, id, map[string]any{"action": "retarget", "kind": p.Kind, "from": from, "to": to})
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
	proposals, err := store.PendingProposals(r.Context(), s.db(r.Context()), "")
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
	decide := deciding(r)
	for _, p := range batch {
		res, aerr := s.cfg.Gardener.Apply(decide, p.ID)
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
		redirectFlash(w, r, fmt.Sprintf("applied %d of %d; first error: %s", applied, len(batch), firstErr))
		return
	}
	if settled > 0 {
		redirectNotice(w, r, fmt.Sprintf("Applied %d proposal(s) in %s; settled %d whose memory had already gone inactive.",
			applied, slug, settled))
		return
	}
	redirectNotice(w, r, fmt.Sprintf("Applied all %d proposal(s) in %s.", applied, slug))
}

// deciding is the request's context naming who decides a gardener action from
// the console (consoleActor): it is recorded with the decision and on its
// event.
func deciding(r *http.Request) context.Context {
	return gardener.WithActor(r.Context(), consoleActor(r))
}

// consoleActor is who a console request decides as: the owner, unless it came
// from a process an agent started (access.FromAgent) or with a paired device's
// own credential that is not the owner's hands (refusedFromDevice says when a
// relay's is). Those are an agent's, unnamed: the console has no session to
// name it by.
func consoleActor(r *http.Request) string {
	ctx := r.Context()
	if access.FromAgent(ctx) {
		return gardener.ByAgent
	}
	if cred, ok := devices.CredentialOf(ctx); ok && (cred.Kind != devices.KindRelay || !relayOwnerHands(r)) {
		return gardener.ByAgent
	}
	return gardener.ByOwner
}

// gardenerRun asks for a proposal pass now instead of at the next tick
// (gardener.Service.RunNow). The pass runs in the background; the page shows
// it running and, once it ends, when it ran and what it skipped or failed. It
// is refused, with why, while suggestions are off, while a pass runs, and
// where the schedule is not running.
func (s *Service) gardenerRun(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Gardener == nil {
		s.serverError(w, r, errNoGardener)
		return
	}
	err := s.cfg.Gardener.RunNow(deciding(r))
	switch {
	case errors.Is(err, gardener.ErrProposingOff):
		redirectFlash(w, r, "No pass runs: background suggestions are off (gardener.enabled is false in the config). "+
			"Turn them on in the config file and restart the daemon.")
	case errors.Is(err, gardener.ErrPassRunning):
		redirectFlash(w, r, "A pass is already running. Its proposals appear here when it finishes.")
	case errors.Is(err, gardener.ErrNotScheduled):
		redirectFlash(w, r, "No pass runs: the gardener's schedule is not running on this server.")
	case err != nil:
		s.serverError(w, r, err)
	default:
		redirectNotice(w, r, "A pass has started. Its proposals appear here when it finishes.")
	}
}

// gardenerDecisions is the decision history: every decided proposal, newest
// decision first, decisionsPerPage a page (?page=N, from 1), each with its
// outcome, who decided, what an apply produced, and Undo wherever
// gardener.CanUndoResolution allows. JSON for a script, the page otherwise.
//
// What automatic apply applied comes first, on page 1, as a section of its
// own: every one still applied, uncapped, each with its Undo, because nobody
// reviewed it before it landed. The pages below list every other decision, so
// each decision appears once.
func (s *Service) gardenerDecisions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			s.badRequest(w, r, fmt.Sprintf("invalid page %q: a page number from 1", raw))
			return
		}
		page = n
	}
	auto, err := store.ProposalsAppliedBy(ctx, s.db(ctx), gardener.ByAuto)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	total, err := store.CountResolvedProposalsOmitting(ctx, s.db(ctx), gardener.ByAuto)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	pages := max(1, (total+decisionsPerPage-1)/decisionsPerPage)
	if page > pages {
		s.notFound(w, r, fmt.Sprintf("No page %d: the decision history has %s.", page, plural(pages, "page", "pages")))
		return
	}
	ps, err := store.ResolvedProposalsOmitting(ctx, s.db(ctx), gardener.ByAuto, (page-1)*decisionsPerPage, decisionsPerPage)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	v := &decisionsView{
		Rows: s.decisionRows(ctx, ps), Page: page, Pages: pages, PerPage: decisionsPerPage, Total: total,
		AutoCount: len(auto), CanAct: s.cfg.Gardener != nil,
	}
	if page == 1 {
		v.Auto = s.decisionRows(ctx, auto)
	}
	if len(v.Rows) > 0 {
		v.From = (page-1)*decisionsPerPage + 1
		v.To = v.From + len(v.Rows) - 1
	}
	if page > 1 {
		v.Prev = decisionsHref(page - 1)
	}
	if page < pages {
		v.Next = decisionsHref(page + 1)
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, v)
		return
	}
	title := "Gardener decisions"
	if page > 1 {
		title += fmt.Sprintf(", page %d", page)
	}
	s.render(w, r, "gardener", pageData{Title: title, Active: "gardener", Data: gardenerData{History: v}})
}

// decisionsHref is the decision history's page n.
func decisionsHref(n int) string {
	if n <= 1 {
		return "/console/gardener/decisions"
	}
	return "/console/gardener/decisions?page=" + strconv.Itoa(n)
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

// proposalReachable reports whether the console's own (read) handle can see the
// proposal a gardener action names, answering 404 when it cannot. The gardener
// service loads proposals from the unfiltered store, so under peer_auth -- where
// the console reads through a handle on which locked projects do not exist
// -- an id is the one thing that could reach past the
// filter, and this is the check that stops it. With nothing filtered (peer_auth
// off) there is nothing to check, and the action behaves exactly as it did.
func (s *Service) proposalReachable(w http.ResponseWriter, r *http.Request, id string) bool {
	if s.cfg.WriteDB == nil {
		return true
	}
	_, ok, err := store.ProposalByID(r.Context(), s.db(r.Context()), id)
	if err != nil {
		s.serverError(w, r, err)
		return false
	}
	if !ok {
		s.notFound(w, r, "No proposal with id "+id+".")
		return false
	}
	return true
}
