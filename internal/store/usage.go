package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// NamedCount pairs an item's name with a count, for "top N" lists.
type NamedCount struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// NamedScore pairs an item's name with a float score, for "top N" lists.
type NamedScore struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Score float64 `json:"score"`
}

// UsageSummary is a point-in-time roll-up of activity across the store, backing
// the usage_summary MCP tool and (later) the console's overview. It is derived
// entirely from the DB-of-record tables and the event log.
type UsageSummary struct {
	Memories struct {
		Active int            `json:"active"`
		ByKind map[string]int `json:"byKind"`
	} `json:"memories"`
	Notes     int            `json:"notes"`
	Sessions  map[string]int `json:"sessions"` // status -> count
	Tasks     map[string]int `json:"tasks"`    // status -> count
	Retrieval struct {
		Injections  int          `json:"injections"`
		Reads       int          `json:"reads"`
		TopInjected []NamedCount `json:"topInjected"`
		TopUtility  []NamedScore `json:"topUtility"` // highest decayed-demand scores
		// FunnelBySurface is the all-time read-after-inject funnel segmented by
		// injection surface (conversions within DefaultFunnelFollow).
		FunnelBySurface []SurfaceFunnel `json:"funnelBySurface"`
	} `json:"retrieval"`
	GardenerPending map[string]int `json:"gardenerPending"` // kind -> count
	EventsByKind    map[string]int `json:"eventsByKind"`
}

// GetUsageSummary computes the usage roll-up. It reads current retrieval_stats;
// callers wanting fresh numbers should RebuildRetrievalStats first.
func GetUsageSummary(ctx context.Context, db *sql.DB) (UsageSummary, error) {
	u, err := GetUsageCounts(ctx, db)
	if err != nil {
		return u, err
	}
	if u.EventsByKind, err = countBy(ctx, db, `SELECT kind, COUNT(*) FROM events GROUP BY kind`); err != nil {
		return u, err
	}
	if err = db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(inject_count), 0), COALESCE(SUM(read_count), 0) FROM retrieval_stats`).
		Scan(&u.Retrieval.Injections, &u.Retrieval.Reads); err != nil {
		return u, fmt.Errorf("store.GetUsageSummary: retrieval totals: %w", err)
	}
	if u.Retrieval.TopUtility, err = topUtility(ctx, db, 5); err != nil {
		return u, err
	}
	if u.Retrieval.FunnelBySurface, err = ReadAfterInjectFunnel(ctx, db, time.Time{}, 0); err != nil {
		return u, err
	}
	return u, nil
}

// GetUsageCounts is the inexpensive part of GetUsageSummary: the memory, note,
// session, task and pending-proposal counts and the most-injected memories. It
// leaves the event-log aggregates zero -- events by kind, retrieval totals, top
// utility and the all-time read-after-inject funnel, which replays every
// injection's payload -- for a caller that renders none of them on every
// refresh (the console Overview).
func GetUsageCounts(ctx context.Context, db *sql.DB) (UsageSummary, error) {
	var u UsageSummary

	var err error
	if u.Memories.ByKind, err = countBy(ctx, db,
		`SELECT kind, COUNT(*) FROM memories_index WHERE invalid_at IS NULL GROUP BY kind`); err != nil {
		return u, err
	}
	for _, n := range u.Memories.ByKind {
		u.Memories.Active += n
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes_index`).Scan(&u.Notes); err != nil {
		return u, fmt.Errorf("store.GetUsageCounts: notes: %w", err)
	}
	if u.Sessions, err = countBy(ctx, db, `SELECT status, COUNT(*) FROM sessions GROUP BY status`); err != nil {
		return u, err
	}
	if u.Tasks, err = countBy(ctx, db, `SELECT status, COUNT(*) FROM tasks GROUP BY status`); err != nil {
		return u, err
	}
	if u.GardenerPending, err = countBy(ctx, db,
		`SELECT kind, COUNT(*) FROM gardener_proposals WHERE status = 'pending' GROUP BY kind`); err != nil {
		return u, err
	}
	if u.Retrieval.TopInjected, err = topInjected(ctx, db, 5); err != nil {
		return u, err
	}
	return u, nil
}

// topUtility returns the active memories with the highest utility scores.
func topUtility(ctx context.Context, db *sql.DB, limit int) ([]NamedScore, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT rs.item_id, m.name, rs.utility
		FROM retrieval_stats rs
		JOIN memories_index m ON m.id = rs.item_id AND m.invalid_at IS NULL
		WHERE rs.utility > 0
		ORDER BY rs.utility DESC, rs.item_id ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store.topUtility: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []NamedScore
	for rows.Next() {
		var ns NamedScore
		if err := rows.Scan(&ns.ID, &ns.Name, &ns.Score); err != nil {
			return nil, fmt.Errorf("store.topUtility: scan: %w", err)
		}
		out = append(out, ns)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.topUtility: %w", err)
	}
	return out, nil
}

// countBy runs a "SELECT key, COUNT(*) ... GROUP BY key" query into a map.
func countBy(ctx context.Context, db *sql.DB, query string) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store.countBy: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]int)
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return nil, fmt.Errorf("store.countBy: scan: %w", err)
		}
		out[key] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.countBy: %w", err)
	}
	return out, nil
}

// topInjected returns the most-injected active memories, highest first.
func topInjected(ctx context.Context, db *sql.DB, limit int) ([]NamedCount, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT rs.item_id, m.name, rs.inject_count
		FROM retrieval_stats rs
		JOIN memories_index m ON m.id = rs.item_id AND m.invalid_at IS NULL
		WHERE rs.inject_count > 0
		ORDER BY rs.inject_count DESC, rs.item_id ASC
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store.topInjected: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []NamedCount
	for rows.Next() {
		var nc NamedCount
		if err := rows.Scan(&nc.ID, &nc.Name, &nc.Count); err != nil {
			return nil, fmt.Errorf("store.topInjected: scan: %w", err)
		}
		out = append(out, nc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.topInjected: %w", err)
	}
	return out, nil
}
