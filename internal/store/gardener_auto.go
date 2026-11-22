package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// CountProposalsAppliedSince counts the applied proposals resolved_by names
// by whose resolved_at is after since; a zero since counts all of them. The
// rows are the ones ProposalsAppliedBy(ctx, db, by) lists (status applied,
// resolved_by = by), windowed by when they were decided, so the Overview's
// "applied automatically" card and the decision history's section count the
// same proposals. An undone apply is pending again and leaves both.
func CountProposalsAppliedSince(ctx context.Context, db *sql.DB, by string, since time.Time) (int, error) {
	q := `SELECT COUNT(*) FROM gardener_proposals WHERE status = ? AND resolved_by = ?`
	args := []any{ProposalApplied, by}
	if !since.IsZero() {
		q += ` AND resolved_at > ?`
		args = append(args, core.FormatTime(since))
	}
	var n int
	if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store.CountProposalsAppliedSince: %w", err)
	}
	return n, nil
}

// LastGardenerActionBy is when the newest gardener.action event whose payload
// by names by was recorded, leaving out the actions except lists. found is
// false when there is none. The console reads it with by "console" for when
// the owner last decided on the Gardener, leaving out a change of the
// gardener's switches, which is made on Settings.
func LastGardenerActionBy(ctx context.Context, db *sql.DB, by string, except []string) (at time.Time, found bool, err error) {
	q := `SELECT ts FROM events
		WHERE kind = ? AND json_valid(payload) AND json_extract(payload, '$.by') = ?`
	args := []any{string(core.EventGardenerAction), by}
	if len(except) > 0 {
		q += ` AND COALESCE(json_extract(payload, '$.action'), '') NOT IN (?` + strings.Repeat(`, ?`, len(except)-1) + `)`
		for _, a := range except {
			args = append(args, a)
		}
	}
	q += ` ORDER BY ts DESC LIMIT 1`
	var ts string
	err = db.QueryRowContext(ctx, q, args...).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store.LastGardenerActionBy: %w", err)
	}
	at, err = core.ParseTime(ts)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store.LastGardenerActionBy: ts: %w", err)
	}
	return at, true, nil
}
