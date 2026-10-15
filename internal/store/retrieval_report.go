package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// RetrievalWindowKeys are the selectable trailing windows for the retrieval-health
// views, in display order. "24h" is the default; "all" spans every recorded event.
var RetrievalWindowKeys = []string{"24h", "7d", "30d", "all"}

// RetrievalWindow is a resolved trailing time window for the retrieval-health
// views: a stable key (URL + selector), a human label, and the inclusive lower
// bound. A zero Since means "all time" (unbounded).
type RetrievalWindow struct {
	Key   string    `json:"key"`
	Label string    `json:"label"`
	Since time.Time `json:"since"`
}

// ResolveRetrievalWindow maps a selector key to a window anchored at now. Unknown
// or empty keys fall back to "24h", the default: an unscoped console page answers
// about today rather than about all history.
func ResolveRetrievalWindow(key string, now time.Time) RetrievalWindow {
	switch key {
	case "7d":
		return RetrievalWindow{Key: "7d", Label: "7d", Since: now.AddDate(0, 0, -7)}
	case "30d":
		return RetrievalWindow{Key: "30d", Label: "30d", Since: now.AddDate(0, 0, -30)}
	case "all":
		return RetrievalWindow{Key: "all", Label: "all time"}
	default:
		return RetrievalWindow{Key: "24h", Label: "24h", Since: now.Add(-24 * time.Hour)}
	}
}

// TrendBucket is one time bucket of the injection trend: a pre-formatted tick
// label and the item-level injection count that fell in it. The report's total
// Injected equals the sum of bucket counts, so the hero number and the chart
// always describe the same quantity over the same window.
type TrendBucket struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// KindReach is one memory kind's share of a window's injection activity: total
// injections (volume) and the number of distinct active memories of that kind
// that were surfaced at least once.
type KindReach struct {
	Kind     string `json:"kind"`
	Injects  int    `json:"injects"`
	Memories int    `json:"memories"`
}

// ProjectReach is one project's reach within the window: how many of its active
// memories were surfaced at least once, out of its total active memories, plus
// the injection volume attributable to them. It lets the global reach number be
// read per knowledge base -- a project whose memories never surface shows 0%
// even while the global rate looks healthy. Project "" is the global scope.
type ProjectReach struct {
	Project   string `json:"project"`
	Surfaced  int    `json:"surfaced"`  // distinct active memories of this project surfaced
	Active    int    `json:"active"`    // total active memories in this project (reach denominator)
	ReachRate int    `json:"reachRate"` // Surfaced / Active, rounded %
	Injects   int    `json:"injects"`   // injection volume attributable to this project's memories
	Created   int    `json:"created"`   // memories created within the window (incl. since-retired ones)
	Retired   int    `json:"retired"`   // memories retired (superseded/archived) within the window
}

// RetrievalReport is the retrieval-REACH rollup for a window, computed live from
// the injection event stream. It measures how far the knowledge base actually
// reaches agents -- how many distinct memories get surfaced, across how many
// sessions -- rather than a read-back rate. (Read-after-inject is not tracked
// here: agents almost never memory_read what a briefing already surfaced, so the
// only honest consumption signal for this system is reach, not re-reads.)
type RetrievalReport struct {
	Window           RetrievalWindow `json:"window"`
	Injected         int             `json:"injected"`         // item-level injections in the window (volume) == sum(Trend)
	InjectedTokens   int             `json:"injectedTokens"`   // estimated tokens of injected context in the window (reach's cost side)
	MemoriesSurfaced int             `json:"memoriesSurfaced"` // distinct active memories surfaced >=1x
	ActiveMemories   int             `json:"activeMemories"`   // total active memories (reach denominator)
	ReachRate        int             `json:"reachRate"`        // MemoriesSurfaced / ActiveMemories, rounded %
	SessionsReached  int             `json:"sessionsReached"`  // distinct sessions that received >=1 injection
	CreatedInWindow  int             `json:"createdInWindow"`  // memories created within the window (incl. since-retired ones)
	RetiredInWindow  int             `json:"retiredInWindow"`  // memories retired (superseded/archived) within the window
	ByKind           []KindReach     `json:"byKind"`
	ByProject        []ProjectReach  `json:"byProject"` // reach sliced per project (denominator = that project's active memories)
	Top              []MemoryStat    `json:"top"`       // most-injected active memories, with sessions reached
	Trend            []TrendBucket   `json:"trend"`
	Hourly           bool            `json:"hourly"` // trend granularity: hourly (24h window) vs daily

	// Distinct-count series over the same axis as Trend. Volume and reach move
	// independently -- one memory injected a thousand times is a tall Trend and a
	// flat SurfacedTrend -- so a reach sparkline has to plot its own numerator
	// rather than borrow the volume curve.
	SurfacedTrend []TrendBucket `json:"surfacedTrend"` // distinct memories surfaced per bucket
	SessionTrend  []TrendBucket `json:"sessionTrend"`  // distinct sessions reached per bucket

	// Loop health: whether what briefings push is what agents actually pull.
	// Demand = query-gated signals only (recall hits, prompt matches, explicit
	// reads); passive briefing injections are exposure, not demand.
	BriefingSurfaced   int          `json:"briefingSurfaced"`   // distinct active memories briefings surfaced in the window
	DemandedOfSurfaced int          `json:"demandedOfSurfaced"` // of those, how many also saw demand in the window
	DemandRate         int          `json:"demandRate"`         // DemandedOfSurfaced / BriefingSurfaced, rounded %
	WasteShare         int          `json:"wasteShare"`         // % of injected tokens spent on memories with no demand in 30d
	PromptMatches      int          `json:"promptMatches"`      // prompt-recall injections in the window
	RecallMisses       int          `json:"recallMisses"`       // hook.prompt (matched nothing) events in the window
	MissRate           int          `json:"missRate"`           // RecallMisses / (RecallMisses+PromptMatches), rounded %
	ToolMisses         int          `json:"toolMisses"`         // recall.miss (zero-hit recall calls) events in the window
	DeadWeight         []MemoryStat `json:"deadWeight"`         // most-briefing-injected active memories with no demand in 30d
}

// injection is one (session, memory) injection occurrence within the window.
type injection struct {
	session string // session_id, or the payload's claude_session_id when the column is empty
	item    string
	at      time.Time
}

// memMeta is the naming/classification an injected id needs to appear in the
// per-kind, most-injected, and dead-weight breakdowns.
type memMeta struct {
	kind, name, project string
	created, updated    time.Time
}

// BuildRetrievalReport computes the windowed retrieval-reach rollup in a single
// pass over the injection event stream: total injection volume, how many distinct
// active memories were surfaced (reach), across how many sessions, plus the
// per-kind and most-injected breakdowns and the injection trend. topN caps the
// most-injected list (<=0 => 12). Archived/unknown injected ids still count
// toward the volume total and the trend, but drop out of the by-kind and
// most-injected breakdowns (scoped to active memories).
func BuildRetrievalReport(ctx context.Context, db *sql.DB, w RetrievalWindow, topN int) (RetrievalReport, error) {
	if topN <= 0 {
		topN = 12
	}
	rep := RetrievalReport{Window: w, Hourly: w.Key == "24h"}

	meta, err := activeMemoryMeta(ctx, db)
	if err != nil {
		return rep, err
	}
	rep.ActiveMemories = len(meta)

	// Active-memory denominator per project (the reach denominator is per project,
	// so a project with zero surfaced memories still appears at 0%).
	activeByProject := map[string]int{}
	for _, m := range meta {
		activeByProject[m.project]++
	}

	// The denominator is "active as of now" in every window, so on its own it says
	// nothing about how the knowledge base is changing; the churn counts put it in
	// context ("152 active, +27 new / -11 retired this window").
	createdBy, retiredBy, err := memoryChurn(ctx, db, w.Since)
	if err != nil {
		return rep, err
	}
	for _, n := range createdBy {
		rep.CreatedInWindow += n
	}
	for _, n := range retiredBy {
		rep.RetiredInWindow += n
	}

	args := []any{string(core.EventInjected), string(core.EventMemoryRead)}
	q := `SELECT ts, kind, session_id, item_id, payload FROM events WHERE kind IN (?, ?)`
	if !w.Since.IsZero() {
		q += ` AND ts >= ?`
		args = append(args, core.FormatTime(w.Since))
	}
	q += ` ORDER BY ts ASC, id ASC`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return rep, fmt.Errorf("store.BuildRetrievalReport: query events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var injections []injection
	sessions := map[string]struct{}{}         // distinct sessions reached (any injection)
	briefingSurfaced := map[string]struct{}{} // active memories session-start briefings surfaced
	briefingInj := map[string]int{}           // briefing item-level inject counts (dead-weight sort)
	windowDemand := map[string]struct{}{}     // item ids with query-gated demand inside the window
	memTokens := map[string]float64{}         // per-item injected-token attribution
	var earliest time.Time
	for rows.Next() {
		var tsStr, kind, sessionID, itemID, payload string
		if err := rows.Scan(&tsStr, &kind, &sessionID, &itemID, &payload); err != nil {
			return rep, fmt.Errorf("store.BuildRetrievalReport: scan: %w", err)
		}
		ts, err := core.ParseTime(tsStr)
		if err != nil {
			return rep, fmt.Errorf("store.BuildRetrievalReport: parse ts: %w", err)
		}
		if core.EventKind(kind) == core.EventMemoryRead {
			// Reads feed the demand side only -- never injection volume, trend,
			// or session reach.
			if itemID != "" {
				windowDemand[itemID] = struct{}{}
			}
			continue
		}

		p, _ := decodeInjectedPayload(payload)
		_, class := injectedUtilityWeight(p)
		if class == "prompt" {
			rep.PromptMatches++
		}
		sess := injectedSessionKey(sessionID, p)
		if sess != "" {
			sessions[sess] = struct{}{}
		}
		ids := injectedItemIDs(itemID, p)
		cost := injectedTokenCost(payload)
		rep.InjectedTokens += cost
		for _, id := range ids {
			injections = append(injections, injection{session: sess, item: id, at: ts})
			if earliest.IsZero() || ts.Before(earliest) {
				earliest = ts
			}
			// Token attribution is approximate: an event records one cost for
			// its whole content block, so each item carries an equal share.
			memTokens[id] += float64(cost) / float64(len(ids))
			if class != "" {
				windowDemand[id] = struct{}{}
			}
			if p.Hook == "session-start" {
				if _, active := meta[id]; active {
					briefingSurfaced[id] = struct{}{}
				}
				briefingInj[id]++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return rep, fmt.Errorf("store.BuildRetrievalReport: rows: %w", err)
	}
	rep.SessionsReached = len(sessions)

	// Per-kind (distinct memories) and per-memory (distinct sessions) rollups over
	// active memories only; the volume total covers every injected id.
	kindMems := map[string]map[string]struct{}{} // kind -> set of memory ids
	kindInj := map[string]int{}
	projMems := map[string]map[string]struct{}{} // project -> set of surfaced memory ids
	projInj := map[string]int{}
	memInj := map[string]int{}
	memSess := map[string]map[string]struct{}{} // memory id -> set of sessions
	for _, in := range injections {
		rep.Injected++
		m, known := meta[in.item]
		if !known {
			continue
		}
		kindInj[m.kind]++
		if kindMems[m.kind] == nil {
			kindMems[m.kind] = map[string]struct{}{}
		}
		kindMems[m.kind][in.item] = struct{}{}
		projInj[m.project]++
		if projMems[m.project] == nil {
			projMems[m.project] = map[string]struct{}{}
		}
		projMems[m.project][in.item] = struct{}{}
		memInj[in.item]++
		if memSess[in.item] == nil {
			memSess[in.item] = map[string]struct{}{}
		}
		if in.session != "" {
			memSess[in.item][in.session] = struct{}{}
		}
	}
	rep.MemoriesSurfaced = len(memInj)
	if rep.ActiveMemories > 0 {
		rep.ReachRate = int(float64(rep.MemoriesSurfaced)/float64(rep.ActiveMemories)*100 + 0.5)
	}

	rep.ByKind = make([]KindReach, 0, len(kindInj))
	for kind, inj := range kindInj {
		rep.ByKind = append(rep.ByKind, KindReach{Kind: kind, Injects: inj, Memories: len(kindMems[kind])})
	}
	sort.Slice(rep.ByKind, func(i, j int) bool {
		if rep.ByKind[i].Injects != rep.ByKind[j].Injects {
			return rep.ByKind[i].Injects > rep.ByKind[j].Injects
		}
		return rep.ByKind[i].Kind < rep.ByKind[j].Kind
	})

	// Every project with active memories appears (largest knowledge base first),
	// including those at 0% reach -- that gap is the point of the breakdown. A
	// project whose last memory retired inside the window drops off the list; its
	// churn still counts in the global RetiredInWindow.
	rep.ByProject = make([]ProjectReach, 0, len(activeByProject))
	for proj, active := range activeByProject {
		surfaced := len(projMems[proj])
		rate := 0
		if active > 0 {
			rate = int(float64(surfaced)/float64(active)*100 + 0.5)
		}
		rep.ByProject = append(rep.ByProject, ProjectReach{
			Project: proj, Surfaced: surfaced, Active: active, ReachRate: rate, Injects: projInj[proj],
			Created: createdBy[proj], Retired: retiredBy[proj],
		})
	}
	sort.Slice(rep.ByProject, func(i, j int) bool {
		if rep.ByProject[i].Active != rep.ByProject[j].Active {
			return rep.ByProject[i].Active > rep.ByProject[j].Active
		}
		if rep.ByProject[i].Surfaced != rep.ByProject[j].Surfaced {
			return rep.ByProject[i].Surfaced > rep.ByProject[j].Surfaced
		}
		return rep.ByProject[i].Project < rep.ByProject[j].Project
	})

	rep.Top = make([]MemoryStat, 0, len(memInj))
	for id, inj := range memInj {
		m := meta[id]
		rep.Top = append(rep.Top, MemoryStat{
			ID: id, Name: m.name, Kind: m.kind, Project: m.project,
			Injects: inj, Sessions: len(memSess[id]),
		})
	}
	sort.Slice(rep.Top, func(i, j int) bool {
		if rep.Top[i].Injects != rep.Top[j].Injects {
			return rep.Top[i].Injects > rep.Top[j].Injects
		}
		return rep.Top[i].ID < rep.Top[j].ID
	})
	if len(rep.Top) > topN {
		rep.Top = rep.Top[:topN]
	}

	// Loop health. The waste and dead-weight sides judge demand over a fixed
	// trailing 30d horizon regardless of the selected window, so the 24h view
	// does not brand every quiet-today memory as waste.
	demand30, err := DemandItemIDsSince(ctx, db, time.Now().UTC().Add(-30*24*time.Hour))
	if err != nil {
		return rep, err
	}
	rep.BriefingSurfaced = len(briefingSurfaced)
	for id := range briefingSurfaced {
		if _, ok := windowDemand[id]; ok {
			rep.DemandedOfSurfaced++
		}
	}
	if rep.BriefingSurfaced > 0 {
		rep.DemandRate = int(float64(rep.DemandedOfSurfaced)/float64(rep.BriefingSurfaced)*100 + 0.5)
	}
	if rep.InjectedTokens > 0 {
		var waste float64
		for id, tok := range memTokens {
			if _, active := meta[id]; !active {
				continue
			}
			if _, ok := demand30[id]; !ok {
				waste += tok
			}
		}
		rep.WasteShare = int(waste/float64(rep.InjectedTokens)*100 + 0.5)
	}
	rep.RecallMisses, err = countEventsSince(ctx, db, core.EventHookPrompt, w.Since)
	if err != nil {
		return rep, err
	}
	if total := rep.RecallMisses + rep.PromptMatches; total > 0 {
		rep.MissRate = int(float64(rep.RecallMisses)/float64(total)*100 + 0.5)
	}
	rep.ToolMisses, err = countEventsSince(ctx, db, core.EventRecallMiss, w.Since)
	if err != nil {
		return rep, err
	}
	for id := range briefingSurfaced {
		if _, ok := demand30[id]; ok {
			continue
		}
		m := meta[id]
		// Constraints and stages are pinned into every briefing by design, so
		// exposure-without-demand is their normal state, not a finding -- left
		// in, they would permanently crowd the table (the gardener's dead-weight
		// pass protects them for the same reason).
		if m.kind == string(core.KindConstraint) || m.kind == string(core.KindStage) {
			continue
		}
		rep.DeadWeight = append(rep.DeadWeight, MemoryStat{
			ID: id, Name: m.name, Kind: m.kind, Project: m.project,
			Injects: briefingInj[id], Updated: m.updated,
		})
	}
	sort.Slice(rep.DeadWeight, func(i, j int) bool {
		if rep.DeadWeight[i].Injects != rep.DeadWeight[j].Injects {
			return rep.DeadWeight[i].Injects > rep.DeadWeight[j].Injects
		}
		return rep.DeadWeight[i].ID < rep.DeadWeight[j].ID
	})
	if len(rep.DeadWeight) > 8 {
		rep.DeadWeight = rep.DeadWeight[:8]
	}

	now := time.Now()
	rep.Trend = buildTrend(injections, w, rep.Hourly, earliest, now)
	// Only active memories count toward reach, so the surfaced series drops the
	// injected ids that no longer resolve -- exactly like MemoriesSurfaced.
	rep.SurfacedTrend = buildDistinctTrend(injections, w, rep.Hourly, earliest, now, func(in injection) string {
		if _, active := meta[in.item]; !active {
			return ""
		}
		return in.item
	})
	rep.SessionTrend = buildDistinctTrend(injections, w, rep.Hourly, earliest, now, func(in injection) string { return in.session })
	return rep, nil
}

// countEventsSince counts events of one kind at or after since (zero = all).
func countEventsSince(ctx context.Context, db *sql.DB, kind core.EventKind, since time.Time) (int, error) {
	q := `SELECT COUNT(*) FROM events WHERE kind = ?`
	args := []any{string(kind)}
	if !since.IsZero() {
		q += ` AND ts >= ?`
		args = append(args, core.FormatTime(since))
	}
	var n int
	if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store.countEventsSince: %w", err)
	}
	return n, nil
}

// ProjectRetrievalTrend builds the injection trend for a single project's active
// memories over the window: the same time-bucketed series as RetrievalReport.Trend,
// but counting only injections of memories whose project is `project`. Attribution
// matches RetrievalReport.ByProject (by the injected memory's own project, not the
// injecting session's), so a project's trend agrees with the reach numbers shown
// beside it. Injections of archived/unknown ids -- which have no active project --
// are excluded (unlike the global trend, they cannot be attributed to a slice).
func ProjectRetrievalTrend(ctx context.Context, db *sql.DB, w RetrievalWindow, project string) ([]TrendBucket, error) {
	meta, err := activeMemoryMeta(ctx, db)
	if err != nil {
		return nil, err
	}

	args := []any{string(core.EventInjected)}
	q := `SELECT ts, item_id, payload FROM events WHERE kind = ?`
	if !w.Since.IsZero() {
		q += ` AND ts >= ?`
		args = append(args, core.FormatTime(w.Since))
	}
	q += ` ORDER BY ts ASC, id ASC`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store.ProjectRetrievalTrend: query events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var injections []injection
	var earliest time.Time
	for rows.Next() {
		var tsStr, itemID, payload string
		if err := rows.Scan(&tsStr, &itemID, &payload); err != nil {
			return nil, fmt.Errorf("store.ProjectRetrievalTrend: scan: %w", err)
		}
		ts, err := core.ParseTime(tsStr)
		if err != nil {
			return nil, fmt.Errorf("store.ProjectRetrievalTrend: parse ts: %w", err)
		}
		p, _ := decodeInjectedPayload(payload)
		for _, id := range injectedItemIDs(itemID, p) {
			if m, ok := meta[id]; !ok || m.project != project {
				continue
			}
			injections = append(injections, injection{item: id, at: ts})
			if earliest.IsZero() || ts.Before(earliest) {
				earliest = ts
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ProjectRetrievalTrend: rows: %w", err)
	}

	return buildTrend(injections, w, w.Key == "24h", earliest, time.Now()), nil
}

// injectedSessionKey resolves the session an injection belongs to: the event's
// session_id column when set, else the payload's external client + historical
// claude_session_id. Older payloads without the client retain their original
// id-only fallback; new cross-client ids can no longer collapse together.
func injectedSessionKey(sessionID string, p injectedPayload) string {
	if sessionID != "" {
		return sessionID
	}
	if p.ExternalClient != "" && p.ClaudeSessionID != "" {
		return p.ExternalClient + "\x00" + p.ClaudeSessionID
	}
	return p.ClaudeSessionID
}

// injectedTokenPayload is the cost-relevant slice of a retrieval.injected
// payload: hook injections record an emitted_estimated_tokens estimate of the
// content that reached the model; older hook events carry only the verbatim
// content. Recall-tool injections carry neither -- the agent pulled ids, no
// context block was pushed -- and cost 0 here.
type injectedTokenPayload struct {
	EmittedEstimatedTokens int    `json:"emitted_estimated_tokens"`
	Content                string `json:"content"`
}

// injectedTokenCost returns the estimated token cost of one injection event.
// The content fallback must stay on the same ~4 bytes/token convention as
// retrieve.EstimateTokens (not importable here: retrieve depends on store), so
// old and new events aggregate on one scale.
func injectedTokenCost(payload string) int {
	if payload == "" || payload == "{}" {
		return 0
	}
	var p injectedTokenPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return 0
	}
	if p.EmittedEstimatedTokens > 0 {
		return p.EmittedEstimatedTokens
	}
	if p.Content != "" {
		return (len(p.Content) + 3) / 4
	}
	return 0
}

// buildTrend densifies the window into contiguous local-time buckets (so "today"
// is the local day, never a UTC-rollover day into tomorrow) and tallies
// item-level injections into them. For an all-time window the range starts at the
// earliest injection. Returns nil when the window holds no injections.
func buildTrend(injections []injection, w RetrievalWindow, hourly bool, earliest, now time.Time) []TrendBucket {
	var start time.Time
	switch {
	case !w.Since.IsZero():
		start = w.Since
	case !earliest.IsZero():
		start = earliest
	default:
		return nil
	}
	counts := make(map[string]int, len(injections))
	for _, in := range injections {
		counts[bucketKey(in.at, hourly)]++
	}
	axis := localBucketAxis(start, now, hourly)
	out := make([]TrendBucket, 0, len(axis))
	for _, t := range axis {
		out = append(out, TrendBucket{Label: t.label, Count: counts[t.key]})
	}
	return out
}

// buildDistinctTrend is buildTrend over a DISTINCT key per bucket: how many
// different things a bucket touched rather than how many injections it recorded.
// key returns "" for an injection that does not belong in the series (an
// unattributed session, an injected id that is no longer active), which is
// skipped rather than counted as one anonymous member.
func buildDistinctTrend(injections []injection, w RetrievalWindow, hourly bool, earliest, now time.Time, key func(injection) string) []TrendBucket {
	var start time.Time
	switch {
	case !w.Since.IsZero():
		start = w.Since
	case !earliest.IsZero():
		start = earliest
	default:
		return nil
	}
	seen := map[string]map[string]struct{}{}
	for _, in := range injections {
		k := key(in)
		if k == "" {
			continue
		}
		b := bucketKey(in.at, hourly)
		if seen[b] == nil {
			seen[b] = map[string]struct{}{}
		}
		seen[b][k] = struct{}{}
	}
	axis := localBucketAxis(start, now, hourly)
	out := make([]TrendBucket, 0, len(axis))
	for _, t := range axis {
		out = append(out, TrendBucket{Label: t.label, Count: len(seen[t.key])})
	}
	return out
}

// bucketTick is one contiguous trend bucket: its map key (for tallying) and its
// pre-formatted x-axis label.
type bucketTick struct{ key, label string }

// localBucketAxis returns the contiguous local-time buckets spanning [start, now],
// hourly or daily, in order. The injection and coverage trends share it so their
// x-axes line up over the same window.
func localBucketAxis(start, now time.Time, hourly bool) []bucketTick {
	start, end := start.Local(), now.Local()
	var labelFn func(time.Time) string
	var floor, next func(time.Time) time.Time
	if hourly {
		labelFn = func(t time.Time) string { return t.Format("15:04") }
		floor = func(t time.Time) time.Time {
			return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())
		}
		next = func(t time.Time) time.Time { return t.Add(time.Hour) }
	} else {
		labelFn = func(t time.Time) string { return t.Format("Jan 02") }
		floor = func(t time.Time) time.Time {
			return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		}
		next = func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
	}
	var out []bucketTick
	for cur := floor(start); !cur.After(end); cur = next(cur) {
		out = append(out, bucketTick{key: bucketKey(cur, hourly), label: labelFn(cur)})
	}
	return out
}

// bucketKey is the local-time bucket key for t at the given granularity; it must
// match the keys localBucketAxis emits so tallies land in the right bucket.
func bucketKey(t time.Time, hourly bool) string {
	if hourly {
		return t.Local().Format("2006-01-02T15")
	}
	return t.Local().Format("2006-01-02")
}

// memoryChurn counts, per project, the memories created and the memories retired
// (invalid_at stamped by supersession or archive) within the window. Counts cover
// every index row, active or not, so a memory created and retired inside the same
// window appears in both. A zero since means all time.
func memoryChurn(ctx context.Context, db *sql.DB, since time.Time) (created, retired map[string]int, err error) {
	// The empty string sorts below every stored timestamp, so the zero (all-time)
	// bound matches every row without a second query shape.
	bound := ""
	if !since.IsZero() {
		bound = core.FormatTime(since)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT project,
			SUM(CASE WHEN created_at >= ? THEN 1 ELSE 0 END),
			SUM(CASE WHEN invalid_at IS NOT NULL AND invalid_at >= ? THEN 1 ELSE 0 END)
		FROM memories_index GROUP BY project`, bound, bound)
	if err != nil {
		return nil, nil, fmt.Errorf("store.memoryChurn: %w", err)
	}
	defer func() { _ = rows.Close() }()
	created, retired = map[string]int{}, map[string]int{}
	for rows.Next() {
		var project string
		var c, r int
		if err := rows.Scan(&project, &c, &r); err != nil {
			return nil, nil, fmt.Errorf("store.memoryChurn: scan: %w", err)
		}
		created[project], retired[project] = c, r
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store.memoryChurn: %w", err)
	}
	return created, retired, nil
}

// activeMemoryMeta loads id -> (kind, name, project) for every active memory, so
// the report can name and classify injected ids without a per-row query.
func activeMemoryMeta(ctx context.Context, db *sql.DB) (map[string]memMeta, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, kind, name, project, created_at, updated_at FROM memories_index WHERE invalid_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("store.activeMemoryMeta: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]memMeta{}
	for rows.Next() {
		var id, kind, name, project, createdStr, updatedStr string
		if err := rows.Scan(&id, &kind, &name, &project, &createdStr, &updatedStr); err != nil {
			return nil, fmt.Errorf("store.activeMemoryMeta: scan: %w", err)
		}
		created, err := core.ParseTime(createdStr)
		if err != nil {
			return nil, fmt.Errorf("store.activeMemoryMeta: parse created_at: %w", err)
		}
		updated, err := core.ParseTime(updatedStr)
		if err != nil {
			return nil, fmt.Errorf("store.activeMemoryMeta: parse updated_at: %w", err)
		}
		out[id] = memMeta{kind: kind, name: name, project: project, created: created, updated: updated}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.activeMemoryMeta: %w", err)
	}
	return out, nil
}

// DemandItemIDsSince returns every item id that saw query-gated demand -- a
// recall hit, a prompt-recall match, or an explicit read -- since the cutoff.
// Passive briefing injections do not count, mirroring the utility score.
func DemandItemIDsSince(ctx context.Context, db *sql.DB, since time.Time) (map[string]struct{}, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT kind, item_id, payload FROM events
		WHERE kind IN (?, ?) AND ts >= ?`,
		string(core.EventInjected), string(core.EventMemoryRead), core.FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("store.demandItemIDsSince: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]struct{}{}
	for rows.Next() {
		var kind, itemID, payload string
		if err := rows.Scan(&kind, &itemID, &payload); err != nil {
			return nil, fmt.Errorf("store.demandItemIDsSince: scan: %w", err)
		}
		switch core.EventKind(kind) {
		case core.EventInjected:
			p, ok := decodeInjectedPayload(payload)
			if !ok {
				continue
			}
			if _, class := injectedUtilityWeight(p); class == "" {
				continue
			}
			for _, id := range injectedItemIDs(itemID, p) {
				out[id] = struct{}{}
			}
		case core.EventMemoryRead:
			if itemID != "" {
				out[itemID] = struct{}{}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.demandItemIDsSince: %w", err)
	}
	return out, nil
}

// RecallMiss is one zero-hit recall tool call read back out of the event log:
// an agent deliberately searched and found nothing. The gardener's
// memory-wanted pass clusters these into proposals.
type RecallMiss struct {
	TS        time.Time
	SessionID string
	Project   string
	Query     string
}

// RecallMissesSince returns the recall.miss events at or after the cutoff,
// oldest first. Rows whose payload carries no query text are unusable for
// clustering and are skipped.
func RecallMissesSince(ctx context.Context, db *sql.DB, since time.Time) ([]RecallMiss, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT ts, session_id, project_slug, payload FROM events
		WHERE kind = ? AND ts >= ? ORDER BY ts`,
		string(core.EventRecallMiss), core.FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("store.RecallMissesSince: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []RecallMiss
	for rows.Next() {
		var ts, sessionID, project, payload string
		if err := rows.Scan(&ts, &sessionID, &project, &payload); err != nil {
			return nil, fmt.Errorf("store.RecallMissesSince: scan: %w", err)
		}
		var p struct {
			Query string `json:"query"`
		}
		if payload == "" || json.Unmarshal([]byte(payload), &p) != nil || p.Query == "" {
			continue
		}
		at, err := core.ParseTime(ts)
		if err != nil {
			continue
		}
		out = append(out, RecallMiss{TS: at, SessionID: sessionID, Project: project, Query: p.Query})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.RecallMissesSince: %w", err)
	}
	return out, nil
}

// AgentError is one agent-facing failure read back out of the event log: an
// MCP/CLI tool call that returned an error (surface "tool") or a hook-stage
// failure swallowed fail-open (surface "hook"). The gardener's tool-error pass
// clusters these into proposals.
type AgentError struct {
	TS        time.Time
	SessionID string
	Project   string
	Surface   string         // "tool" | "hook"
	Key       string         // surface "tool": tool name; surface "hook": stage label
	Error     string         // first line of the error, as the payload recorded it
	Args      map[string]any // surface "tool" only: the call's (truncated) arguments
}

// ToolErrorsSince returns the failed tool.call events at or after the cutoff,
// oldest first. Rows without an error or tool name are unusable for clustering
// and are skipped.
func ToolErrorsSince(ctx context.Context, db *sql.DB, since time.Time) ([]AgentError, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT ts, session_id, project_slug, payload FROM events
		WHERE kind = ? AND ts >= ? ORDER BY ts`,
		string(core.EventToolCall), core.FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("store.ToolErrorsSince: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AgentError
	for rows.Next() {
		var ts, sessionID, project, payload string
		if err := rows.Scan(&ts, &sessionID, &project, &payload); err != nil {
			return nil, fmt.Errorf("store.ToolErrorsSince: scan: %w", err)
		}
		var p struct {
			Tool    string         `json:"tool"`
			IsError bool           `json:"is_error"`
			Error   string         `json:"error"`
			Args    map[string]any `json:"args"`
		}
		if payload == "" || json.Unmarshal([]byte(payload), &p) != nil ||
			!p.IsError || p.Tool == "" || p.Error == "" {
			continue
		}
		at, err := core.ParseTime(ts)
		if err != nil {
			continue
		}
		out = append(out, AgentError{
			TS: at, SessionID: sessionID, Project: project,
			Surface: "tool", Key: p.Tool, Error: p.Error, Args: p.Args,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ToolErrorsSince: %w", err)
	}
	return out, nil
}

// HookErrorsSince returns the hook.error events at or after the cutoff, oldest
// first. Rows without a stage or error are skipped.
func HookErrorsSince(ctx context.Context, db *sql.DB, since time.Time) ([]AgentError, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT ts, session_id, project_slug, payload FROM events
		WHERE kind = ? AND ts >= ? ORDER BY ts`,
		string(core.EventHookError), core.FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("store.HookErrorsSince: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AgentError
	for rows.Next() {
		var ts, sessionID, project, payload string
		if err := rows.Scan(&ts, &sessionID, &project, &payload); err != nil {
			return nil, fmt.Errorf("store.HookErrorsSince: scan: %w", err)
		}
		var p struct {
			Stage string `json:"stage"`
			Error string `json:"error"`
		}
		if payload == "" || json.Unmarshal([]byte(payload), &p) != nil ||
			p.Stage == "" || p.Error == "" {
			continue
		}
		at, err := core.ParseTime(ts)
		if err != nil {
			continue
		}
		out = append(out, AgentError{
			TS: at, SessionID: sessionID, Project: project,
			Surface: "hook", Key: p.Stage, Error: p.Error,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.HookErrorsSince: %w", err)
	}
	return out, nil
}

// RecallHitQuery is the (project, query) of one successful recall tool call.
type RecallHitQuery struct {
	Project string
	Query   string
}

// RecallHitQueriesSince returns the query text of every recall tool call that
// surfaced hits since the cutoff. The memory-wanted pass uses these to suppress
// miss groups whose query also succeeds sometimes -- an intermittent miss is a
// ranking problem, not a knowledge gap.
func RecallHitQueriesSince(ctx context.Context, db *sql.DB, since time.Time) ([]RecallHitQuery, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT project_slug, payload FROM events WHERE kind = ? AND ts >= ?`,
		string(core.EventInjected), core.FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("store.RecallHitQueriesSince: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []RecallHitQuery
	for rows.Next() {
		var project, payload string
		if err := rows.Scan(&project, &payload); err != nil {
			return nil, fmt.Errorf("store.RecallHitQueriesSince: scan: %w", err)
		}
		if payload == "" || payload == "{}" {
			continue
		}
		var p injectedPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			continue
		}
		if p.Source != "recall" || p.Query == "" {
			continue
		}
		out = append(out, RecallHitQuery{Project: project, Query: p.Query})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.RecallHitQueriesSince: %w", err)
	}
	return out, nil
}

// BriefingExposureSince counts, per item id, session-start briefing injections
// since the cutoff -- the exposure denominator for dead-weight detection.
func BriefingExposureSince(ctx context.Context, db *sql.DB, since time.Time) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT item_id, payload FROM events WHERE kind = ? AND ts >= ?`,
		string(core.EventInjected), core.FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("store.BriefingExposureSince: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var itemID, payload string
		if err := rows.Scan(&itemID, &payload); err != nil {
			return nil, fmt.Errorf("store.BriefingExposureSince: scan: %w", err)
		}
		p, ok := decodeInjectedPayload(payload)
		if !ok || p.Hook != "session-start" {
			continue
		}
		for _, id := range injectedItemIDs(itemID, p) {
			out[id]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.BriefingExposureSince: %w", err)
	}
	return out, nil
}
