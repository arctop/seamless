package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// InjectionSurfaces are the briefing injection surfaces the read-after-inject
// funnel segments by, in display order. Each value is the hook name the hooks
// layer stamps into a retrieval.injected payload: "session-start" is the main
// SessionStart briefing, "subagent-start" the constraints-only child briefing.
var InjectionSurfaces = []string{"session-start", "subagent-start"}

// DefaultFunnelFollow is how long after an injection a query-gated pull of the
// same item still counts as that injection's funnel conversion. A session-scale
// horizon: reads driven by an injection happen within the same working day,
// while a pull weeks later says nothing about the injection that preceded it.
const DefaultFunnelFollow = 24 * time.Hour

// SurfaceFunnel is the read-after-inject funnel for one injection surface:
// how much the surface pushed, and how much of it agents pulled back.
type SurfaceFunnel struct {
	Surface    string `json:"surface"`
	Injections int    `json:"injections"` // item-level injections via this surface in the window (volume)
	Items      int    `json:"items"`      // distinct items this surface injected in the window
	ItemsRead  int    `json:"itemsRead"`  // of Items, how many were pulled within the follow window of an injection
	ReadRate   int    `json:"readRate"`   // ItemsRead / Items, rounded %
}

// ReadAfterInjectFunnel segments the read-after-inject funnel by injection
// surface over the events at or after since (zero = all time). An item
// converts for a surface when a deliberate pull -- an explicit memory.read or
// a recall-tool hit -- lands after one of that surface's injections of the
// item and within follow of it (<=0 = DefaultFunnelFollow); a pull with no
// preceding in-window injection converts nothing. Prompt-recall matches do not
// count as conversions: they are themselves automated injections, and Claude
// Code children have no UserPromptSubmit hook, so counting them would skew the
// session-start vs subagent-start comparison this funnel exists for.
//
// The aggregation is read-only over the existing event log: it records
// nothing, adds no utility inputs, and leaves RebuildRetrievalStats untouched
// (closed-loop-utility-signal-contract).
func ReadAfterInjectFunnel(ctx context.Context, db *sql.DB, since time.Time, follow time.Duration) ([]SurfaceFunnel, error) {
	if follow <= 0 {
		follow = DefaultFunnelFollow
	}

	args := []any{string(core.EventInjected), string(core.EventMemoryRead)}
	q := `SELECT ts, kind, item_id, payload FROM events WHERE kind IN (?, ?)`
	if !since.IsZero() {
		q += ` AND ts >= ?`
		args = append(args, core.FormatTime(since))
	}
	q += ` ORDER BY ts ASC, id ASC`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store.ReadAfterInjectFunnel: query events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type surfaceState struct {
		injections int
		lastInject map[string]time.Time // item id -> most recent injection ts
		converted  map[string]struct{}  // item ids pulled within follow of an injection
	}
	states := make([]*surfaceState, len(InjectionSurfaces))
	surfaceIdx := make(map[string]int, len(InjectionSurfaces))
	for i, name := range InjectionSurfaces {
		states[i] = &surfaceState{lastInject: map[string]time.Time{}, converted: map[string]struct{}{}}
		surfaceIdx[name] = i
	}
	// Events arrive oldest-first, so at pull time lastInject holds the nearest
	// preceding injection -- if any injection of the item is within the follow
	// window, the most recent one is.
	pull := func(id string, ts time.Time) {
		for _, st := range states {
			t0, ok := st.lastInject[id]
			if !ok || ts.Sub(t0) > follow {
				continue
			}
			st.converted[id] = struct{}{}
		}
	}

	for rows.Next() {
		var tsStr, kind, itemID, payload string
		if err := rows.Scan(&tsStr, &kind, &itemID, &payload); err != nil {
			return nil, fmt.Errorf("store.ReadAfterInjectFunnel: scan: %w", err)
		}
		ts, err := core.ParseTime(tsStr)
		if err != nil {
			return nil, fmt.Errorf("store.ReadAfterInjectFunnel: parse ts: %w", err)
		}
		switch core.EventKind(kind) {
		case core.EventMemoryRead:
			if itemID != "" {
				pull(itemID, ts)
			}
		case core.EventInjected:
			p, _ := decodeInjectedPayload(payload)
			if p.Source == "recall" {
				for _, id := range injectedItemIDs(itemID, p) {
					pull(id, ts)
				}
				continue
			}
			idx, ok := surfaceIdx[p.Hook]
			if !ok {
				continue // prompt-recall and other non-briefing injections
			}
			st := states[idx]
			for _, id := range injectedItemIDs(itemID, p) {
				st.injections++
				st.lastInject[id] = ts
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ReadAfterInjectFunnel: rows: %w", err)
	}

	out := make([]SurfaceFunnel, len(InjectionSurfaces))
	for i, name := range InjectionSurfaces {
		st := states[i]
		f := SurfaceFunnel{
			Surface:    name,
			Injections: st.injections,
			Items:      len(st.lastInject),
			ItemsRead:  len(st.converted),
		}
		if f.Items > 0 {
			f.ReadRate = int(float64(f.ItemsRead)/float64(f.Items)*100 + 0.5)
		}
		out[i] = f
	}
	return out, nil
}
