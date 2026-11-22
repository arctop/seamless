package console

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// statsDecayWindow is how long the console lets retrieval stats stand when no
// injection or read has been recorded since the last rebuild. Only the utility
// decay's anchor moves in that time, and with a 14-day half-life ten minutes
// shifts a score by well under a thousandth.
const statsDecayWindow = 10 * time.Minute

// statsGate remembers what the console's last retrieval-stats rebuild read.
type statsGate struct {
	mu     sync.Mutex
	inputs string    // store.RetrievalStatsInputs at that rebuild
	at     time.Time // when it ran
}

// freshRetrievalStats brings retrieval_stats up to date with the event log
// for a screen about to read it. A rebuild replays every injection and read
// ever recorded, and the screens that read the stats re-render on every live
// event, so it runs only when the events it reads have changed since the last
// one here, or statsDecayWindow has passed. The lock makes concurrent renders
// wait for one rebuild instead of each running their own. A failure is logged
// and the screen renders the stats as they stand.
func (s *Service) freshRetrievalStats(ctx context.Context) {
	s.stats.mu.Lock()
	defer s.stats.mu.Unlock()
	// The gate reads the handle the rebuild writes, so it is one key for
	// every request: under locked projects requests read different handles,
	// and keying on theirs would rebuild on every switch between them.
	inputs, err := store.RetrievalStatsInputs(ctx, s.writeDB())
	if err != nil {
		s.logger.Warn("console: retrieval stats inputs", "error", err)
	} else if inputs == s.stats.inputs && time.Since(s.stats.at) < statsDecayWindow {
		return
	}
	if err := store.RebuildRetrievalStats(ctx, s.writeDB()); err != nil {
		s.logger.Warn("console: rebuild retrieval stats", "error", err)
		return
	}
	s.stats.inputs, s.stats.at = inputs, time.Now()
}

// retrievalData is the payload for the Retrieval page. The reach funnel, by-kind,
// trend, and top lists are all computed over the selected window; Stale is
// window-independent, judged against the configured staleness horizon
// (StaleDays, Service.staleDays).
type retrievalData struct {
	Injections       int                  `json:"injections"`
	InjectedTokens   int                  `json:"injectedTokens"`
	MemoriesSurfaced int                  `json:"memoriesSurfaced"`
	ActiveMemories   int                  `json:"activeMemories"`
	ReachRate        int                  `json:"reachRate"`
	SessionsReached  int                  `json:"sessionsReached"`
	CreatedInWindow  int                  `json:"createdInWindow"`
	RetiredInWindow  int                  `json:"retiredInWindow"`
	Window           string               `json:"window"`
	WindowLabel      string               `json:"windowLabel"`
	Windows          []windowOption       `json:"-"`
	ByKind           []store.KindReach    `json:"byKind"`
	ByProject        []store.ProjectReach `json:"byProject"`
	// The scope list, partitioned for rendering: scopes the window actually
	// touched (sorted by injection volume) as full rows, and the untouched rest
	// collapsed into a compact strip -- fifteen 0%-reach rows say less than one
	// line saying "fifteen scopes saw nothing". JSON keeps the flat ByProject.
	ScopesTouched []store.ProjectReach `json:"-"`
	ScopesQuiet   []store.ProjectReach `json:"-"`
	Trend         []store.TrendBucket  `json:"trend"`
	TrendMax      int                  `json:"trendMax"`
	TopInjected   []store.MemoryStat   `json:"topInjected"`
	Stale         []retrievalMemRow    `json:"stale"`
	StaleDays     int                  `json:"staleDays"`

	// Loop health (closed-loop retrieval): push vs pull, and its token cost.
	BriefingSurfaced   int               `json:"briefingSurfaced"`
	DemandedOfSurfaced int               `json:"demandedOfSurfaced"`
	DemandRate         int               `json:"demandRate"`
	WasteShare         int               `json:"wasteShare"`
	PromptMatches      int               `json:"promptMatches"`
	RecallMisses       int               `json:"recallMisses"`
	MissRate           int               `json:"missRate"`
	ToolMisses         int               `json:"toolMisses"`
	DeadWeight         []retrievalMemRow `json:"deadWeight"`

	// Read-after-inject funnel segmented by injection surface: window-scoped
	// injections, conversions within FunnelFollowHours of each injection.
	FunnelBySurface   []store.SurfaceFunnel `json:"funnelBySurface"`
	FunnelFollowHours int                   `json:"funnelFollowHours"`

	// Judgment: what each headline number did against the equally long window
	// before it, and the threshold it is measured against. The loop-health rates
	// carry a band without a delta -- their prior-window comparative is not
	// derivable from the bounded rollup, and a stated ceiling judges them
	// honestly where an invented delta would not.
	PriorLabel      string `json:"-"` // "vs prior 7d"; empty when there is no prior window
	ReachDelta      delta  `json:"-"`
	ReachBand       band   `json:"-"`
	SurfacedOf      delta  `json:"-"` // "of N active" -- a denominator, not a movement
	InjectionsDelta delta  `json:"-"`
	SessionsDelta   delta  `json:"-"`
	TokensDelta     delta  `json:"-"`
	VolumeBand      band   `json:"-"` // what the volume chips compare against
	DemandBand      band   `json:"-"`
	WasteBand       band   `json:"-"`
	MissBand        band   `json:"-"`
}

func (s *Service) retrieval(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s.freshRetrievalStats(ctx)

	now := time.Now()
	win := store.ResolveRetrievalWindow(r.URL.Query().Get("w"), now)
	report, err := store.BuildRetrievalReport(ctx, s.db(ctx), win, 12)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	prior, hasPrior := s.priorVitals(ctx, win, now)
	staleDays := s.staleDays()
	stale, err := store.StaleMemories(ctx, s.db(ctx), staleCutoff(now, staleDays))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	waiting := s.proposalsWaiting(ctx)
	funnel, err := store.ReadAfterInjectFunnel(ctx, s.db(ctx), win.Since, 0)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	trendMax := 0
	for _, b := range report.Trend {
		if b.Count > trendMax {
			trendMax = b.Count
		}
	}
	touched, quiet := splitScopeReach(report.ByProject)

	s.render(w, r, "retrieval", pageData{
		Title:  "Retrieval",
		Active: "retrieval",
		Data: retrievalData{
			Injections: report.Injected, InjectedTokens: report.InjectedTokens,
			MemoriesSurfaced: report.MemoriesSurfaced,
			ActiveMemories:   report.ActiveMemories, ReachRate: report.ReachRate,
			SessionsReached: report.SessionsReached,
			CreatedInWindow: report.CreatedInWindow, RetiredInWindow: report.RetiredInWindow,
			Window: win.Key, WindowLabel: win.Label, Windows: windowOptions(win.Key),
			ByKind: report.ByKind, ByProject: report.ByProject,
			ScopesTouched: touched, ScopesQuiet: quiet,
			Trend: report.Trend, TrendMax: trendMax,
			TopInjected: report.Top, Stale: staleRows(stale, waiting), StaleDays: staleDays,
			BriefingSurfaced: report.BriefingSurfaced, DemandedOfSurfaced: report.DemandedOfSurfaced,
			DemandRate: report.DemandRate, WasteShare: report.WasteShare,
			PromptMatches: report.PromptMatches, RecallMisses: report.RecallMisses, MissRate: report.MissRate,
			ToolMisses:        report.ToolMisses,
			DeadWeight:        memRows(report.DeadWeight, waiting),
			FunnelBySurface:   funnel,
			FunnelFollowHours: int(store.DefaultFunnelFollow.Hours()),

			PriorLabel:      priorLabel(win, hasPrior),
			ReachDelta:      pointDelta(report.ReachRate, prior.ReachRate, hasPrior && report.ActiveMemories > 0, riseGood),
			ReachBand:       floorBand(report.ReachRate, reachTargetPct, report.ActiveMemories > 0),
			SurfacedOf:      ofDelta(report.ActiveMemories),
			InjectionsDelta: volumeDelta(report.Injected, prior.Injections, hasPrior, noJudgment),
			SessionsDelta:   volumeDelta(report.SessionsReached, prior.SessionsReached, hasPrior, noJudgment),
			TokensDelta:     volumeDelta(report.InjectedTokens, prior.InjectedTokens, hasPrior && report.Injected > 0, noJudgment),
			VolumeBand:      noteBand(priorLabel(win, hasPrior)),
			DemandBand:      floorBand(report.DemandRate, demandTargetPct, report.BriefingSurfaced > 0),
			WasteBand:       ceilingBand(report.WasteShare, deadWeightCeilingPct, report.InjectedTokens > 0),
			MissBand:        ceilingBand(report.MissRate, missCeilingPct, report.RecallMisses+report.PromptMatches > 0),
		},
	})
}

// priorVitals loads the equally long window before win, for the delta chips. A
// missing or failed prior window degrades to "no comparison" -- an absent chip
// is honest, a zeroed one is not -- so a query failure warns and continues
// rather than failing the page.
func (s *Service) priorVitals(ctx context.Context, win store.RetrievalWindow, now time.Time) (store.WindowVitals, bool) {
	since, until, ok := store.PriorWindow(win, now)
	if !ok {
		return store.WindowVitals{}, false
	}
	v, err := store.GetWindowVitals(ctx, s.db(ctx), since, until)
	if err != nil {
		s.logger.Warn("console: prior-window vitals", "window", win.Key, "error", err)
		return store.WindowVitals{}, false
	}
	return v, true
}

// priorProjectVitals is priorVitals scoped to one project, for the workspace's
// reach and continuity chips. Same degradation: no prior window, or a failed
// query, means no chip rather than a zeroed one.
func (s *Service) priorProjectVitals(ctx context.Context, project string, win store.RetrievalWindow, now time.Time) (store.WindowVitals, bool) {
	since, until, ok := store.PriorWindow(win, now)
	if !ok {
		return store.WindowVitals{}, false
	}
	v, err := store.GetWindowVitalsForProject(ctx, s.db(ctx), project, since, until)
	if err != nil {
		s.logger.Warn("console: prior-window vitals", "project", project, "window", win.Key, "error", err)
		return store.WindowVitals{}, false
	}
	return v, true
}

// priorLabel names the comparison a delta chip is against ("vs prior 7d"), or
// returns "" when the selected window has none to compare with.
func priorLabel(win store.RetrievalWindow, has bool) string {
	if !has {
		return ""
	}
	return "vs prior " + win.Label
}

// splitScopeReach partitions the per-project reach rows for rendering: scopes
// the window touched (an injection, a surfaced memory, or knowledge churn)
// keep their full row, re-sorted so actual traffic leads instead of knowledge-
// base size; the rest collapse into the quiet strip. Quiet rows keep the
// store's active-desc order, so the biggest silent knowledge base is named
// first.
func splitScopeReach(rows []store.ProjectReach) (touched, quiet []store.ProjectReach) {
	for _, r := range rows {
		if r.Injects > 0 || r.Surfaced > 0 || r.Created > 0 || r.Retired > 0 {
			touched = append(touched, r)
		} else {
			quiet = append(quiet, r)
		}
	}
	sort.SliceStable(touched, func(i, j int) bool {
		if touched[i].Injects != touched[j].Injects {
			return touched[i].Injects > touched[j].Injects
		}
		return touched[i].Surfaced > touched[j].Surfaced
	})
	return touched, quiet
}

// retrievalMemRow is one row of Retrieval's quiet-knowledge and dead-weight
// lists: the memory (its fields keep their JSON keys), and the gardener
// proposal already waiting on it, when there is one.
type retrievalMemRow struct {
	store.MemoryStat
	Proposal *waitingProposal `json:"proposal,omitempty"`
}

// waitingProposal is a pending gardener proposal that acts on a memory.
type waitingProposal struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Href  string `json:"href"`
}

// staleRows projects stale memories into rows for uniform rendering. Stale by
// definition means no recent injection or read, so only the updated age
// carries information.
func staleRows(mems []core.Memory, waiting map[string]waitingProposal) []retrievalMemRow {
	stats := make([]store.MemoryStat, 0, len(mems))
	for _, m := range mems {
		stats = append(stats, store.MemoryStat{
			ID: m.ID, Name: m.Name, Kind: string(m.Kind), Project: m.Project, Updated: m.Updated,
		})
	}
	return memRows(stats, waiting)
}

// memRows pairs each memory with the proposal waiting on it.
func memRows(stats []store.MemoryStat, waiting map[string]waitingProposal) []retrievalMemRow {
	out := make([]retrievalMemRow, 0, len(stats))
	for _, st := range stats {
		row := retrievalMemRow{MemoryStat: st}
		if p, ok := waiting[st.ID]; ok {
			row.Proposal = &p
		}
		out = append(out, row)
	}
	return out
}

// proposalsWaiting maps each memory a pending gardener proposal acts on to that
// proposal, so a Retrieval row can say a decision is already waiting for it. It
// reads through the request's handle, on which a proposal that names a locked
// project's item does not exist. When several proposals act on one memory, an
// archive (what the staleness and dead-weight passes file) wins, then the
// newest. It is decoration over the lists: a failed read costs the links, not
// the page.
func (s *Service) proposalsWaiting(ctx context.Context) map[string]waitingProposal {
	pending, err := store.PendingProposals(ctx, s.db(ctx), "")
	if err != nil {
		s.logger.Warn("console: proposals waiting on retrieval rows", "error", err)
		return nil
	}
	out := map[string]waitingProposal{}
	for _, p := range pending { // newest first
		label, _, _, _ := proposalPresentation(p.Kind)
		w := waitingProposal{ID: p.ID, Kind: p.Kind, Label: label, Href: "/console/gardener/" + p.ID}
		for _, id := range proposalMemoryIDs(p) {
			if have, ok := out[id]; !ok || (have.Kind != store.ProposalArchive && p.Kind == store.ProposalArchive) {
				out[id] = w
			}
		}
	}
	return out
}

// proposalMemoryIDs lists the memories a proposal acts on, by kind: the one
// memory an archive, rekind, reproject or relocate names, both sides of a
// merge, and every source a consolidate supersedes. The other kinds act on no
// existing memory.
func proposalMemoryIDs(p store.Proposal) []string {
	var ids []string
	add := func(m map[string]any) {
		if id := payloadStr(m, "id"); id != "" {
			ids = append(ids, id)
		}
	}
	switch p.Kind {
	case store.ProposalArchive, store.ProposalRekind, store.ProposalReproject, store.ProposalRelocate:
		add(p.Payload)
	case store.ProposalMerge:
		for _, side := range []string{"keep", "drop"} {
			if m, ok := p.Payload[side].(map[string]any); ok {
				add(m)
			}
		}
	case store.ProposalConsolidate:
		if srcs, ok := p.Payload["sources"].([]any); ok {
			for _, src := range srcs {
				if m, ok := src.(map[string]any); ok {
					add(m)
				}
			}
		}
	}
	return ids
}
