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

// staleWindowDays is how long an active memory can go without being updated,
// injected, or read before the Retrieval page flags it as stale. It mirrors the
// gardener's default staleness horizon.
const staleWindowDays = 90

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
	inputs, err := store.RetrievalStatsInputs(ctx, s.cfg.DB)
	if err != nil {
		s.logger.Warn("console: retrieval stats inputs", "error", err)
	} else if inputs == s.stats.inputs && time.Since(s.stats.at) < statsDecayWindow {
		return
	}
	if err := store.RebuildRetrievalStats(ctx, s.cfg.DB); err != nil {
		s.logger.Warn("console: rebuild retrieval stats", "error", err)
		return
	}
	s.stats.inputs, s.stats.at = inputs, time.Now()
}

// retrievalData is the payload for the Retrieval page. The reach funnel, by-kind,
// trend, and top lists are all computed over the selected window; Stale is
// window-independent.
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
	Stale         []store.MemoryStat   `json:"stale"`
	StaleDays     int                  `json:"staleDays"`

	// Loop health (closed-loop retrieval): push vs pull, and its token cost.
	BriefingSurfaced   int                `json:"briefingSurfaced"`
	DemandedOfSurfaced int                `json:"demandedOfSurfaced"`
	DemandRate         int                `json:"demandRate"`
	WasteShare         int                `json:"wasteShare"`
	PromptMatches      int                `json:"promptMatches"`
	RecallMisses       int                `json:"recallMisses"`
	MissRate           int                `json:"missRate"`
	ToolMisses         int                `json:"toolMisses"`
	DeadWeight         []store.MemoryStat `json:"deadWeight"`

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
	report, err := store.BuildRetrievalReport(ctx, s.cfg.DB, win, 12)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	prior, hasPrior := s.priorVitals(ctx, win, now)
	stale, err := store.StaleMemories(ctx, s.cfg.DB, time.Now().UTC().AddDate(0, 0, -staleWindowDays))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	funnel, err := store.ReadAfterInjectFunnel(ctx, s.cfg.DB, win.Since, 0)
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
			TopInjected: report.Top, Stale: staleStats(stale), StaleDays: staleWindowDays,
			BriefingSurfaced: report.BriefingSurfaced, DemandedOfSurfaced: report.DemandedOfSurfaced,
			DemandRate: report.DemandRate, WasteShare: report.WasteShare,
			PromptMatches: report.PromptMatches, RecallMisses: report.RecallMisses, MissRate: report.MissRate,
			ToolMisses:        report.ToolMisses,
			DeadWeight:        report.DeadWeight,
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
	v, err := store.GetWindowVitals(ctx, s.cfg.DB, since, until)
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
	v, err := store.GetWindowVitalsForProject(ctx, s.cfg.DB, project, since, until)
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

// staleStats projects stale memories into MemoryStat for uniform rendering.
// Stale by definition means no recent injection/read, so only the updated age
// carries information.
func staleStats(mems []core.Memory) []store.MemoryStat {
	out := make([]store.MemoryStat, 0, len(mems))
	for _, m := range mems {
		out = append(out, store.MemoryStat{
			ID: m.ID, Name: m.Name, Kind: string(m.Kind), Project: m.Project, Updated: m.Updated,
		})
	}
	return out
}
