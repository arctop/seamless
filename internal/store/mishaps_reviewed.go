package store

// The mishaps-reviewed stamp (plan:console-ux-pass UX 12): the newest
// agent-reported mishap the owner has marked reviewed on the console's
// Mishaps screen. The Overview's mishap item counts the reports recorded
// after it, so marking them reviewed clears the item until a new report
// arrives. One settings key holding a UTC time in core.TimeFormat; no
// migration. The stamp never moves back: marking reviewed through an older
// report than the stamp keeps the stamp.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// SettingMishapsReviewed is the settings key of the stamp.
const SettingMishapsReviewed = "mishaps_reviewed_at"

// EventMishapsReviewed is recorded when the owner marks the mishaps reviewed
// (payload: through, the stamp; reports, how many reports the mark covered
// that the previous stamp did not; by). Defined here beside the stamp, as
// EventMilestoneReached is beside its ledger.
const EventMishapsReviewed core.EventKind = "mishaps.reviewed"

// MishapsReviewedAt returns the stamp. found is false before the owner first
// marks the mishaps reviewed; a stored value that is not a time is an error
// naming it, never a quiet zero.
func MishapsReviewedAt(ctx context.Context, db *sql.DB) (at time.Time, found bool, err error) {
	raw, found, err := GetSetting(ctx, db, SettingMishapsReviewed)
	if err != nil || !found {
		return time.Time{}, false, err
	}
	at, err = core.ParseTime(raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store.MishapsReviewedAt: stored %s %q: %w", SettingMishapsReviewed, raw, err)
	}
	return at, true, nil
}

// MarkMishapsReviewed moves the stamp to through, unless the stamp is already
// at or past it, and returns the stamp in force afterwards. A stored value
// that is not a time is replaced.
func MarkMishapsReviewed(ctx context.Context, db *sql.DB, through time.Time) (time.Time, error) {
	through = through.UTC()
	prev, found, err := MishapsReviewedAt(ctx, db)
	if err == nil && found && !prev.Before(through) {
		return prev, nil
	}
	if err := SetSetting(ctx, db, SettingMishapsReviewed, core.FormatTime(through)); err != nil {
		return time.Time{}, fmt.Errorf("store.MarkMishapsReviewed: %w", err)
	}
	return through, nil
}

// CountMishapsAfter counts the agent.mishap reports recorded strictly after
// since (zero: every report). It reads the handle it is given: on the
// console's filtered one a locked project's reports are absent.
func CountMishapsAfter(ctx context.Context, db *sql.DB, since time.Time) (int, error) {
	query := `SELECT COUNT(*) FROM events WHERE kind = ?`
	args := []any{string(core.EventAgentMishap)}
	if !since.IsZero() {
		query += ` AND ts > ?`
		args = append(args, core.FormatTime(since))
	}
	var n int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store.CountMishapsAfter: %w", err)
	}
	return n, nil
}
