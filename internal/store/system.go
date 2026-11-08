package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// The System screen's reads of the event log: how many events of each kind
// landed in a window, and the (kind, time) of each recent one for the
// per-minute activity lines. Both read through the handle they are given, so
// a console request's filtered handle counts only what that request may see.

// EventCountsByKindSince returns how many events of each kind were recorded at
// or after since.
func EventCountsByKindSince(ctx context.Context, db *sql.DB, since time.Time) (map[string]int, error) {
	const op = "store.EventCountsByKindSince"
	rows, err := db.QueryContext(ctx, `SELECT kind, COUNT(*) FROM events WHERE ts >= ? GROUP BY kind`, core.FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, fmt.Errorf("%s scan: %w", op, err)
		}
		out[kind] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// KindAt is one event's kind and time.
type KindAt struct {
	Kind string
	At   time.Time
}

// EventKindTimesSince returns the kind and time of each event recorded at or
// after since, newest first, capped at limit (a non-positive limit returns
// nil).
func EventKindTimesSince(ctx context.Context, db *sql.DB, since time.Time, limit int) ([]KindAt, error) {
	const op = "store.EventKindTimesSince"
	if limit <= 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT kind, ts FROM events WHERE ts >= ? ORDER BY ts DESC, id DESC LIMIT ?`,
		core.FormatTime(since), limit)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()
	var out []KindAt
	for rows.Next() {
		var kind, ts string
		if err := rows.Scan(&kind, &ts); err != nil {
			return nil, fmt.Errorf("%s scan: %w", op, err)
		}
		at, err := core.ParseTime(ts)
		if err != nil {
			return nil, fmt.Errorf("%s time: %w", op, err)
		}
		out = append(out, KindAt{Kind: kind, At: at})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}
