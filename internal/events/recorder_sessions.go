package events

import (
	"context"
	"fmt"
)

// CountBySession counts a session's events: everything BySession and
// LatestBySession page through, so a timeline that holds only their limit can
// say how many it left out. It reads the same handle they do (the console's
// filtered one, through ReadingFrom), so an event a locked project hides is
// not counted either. An empty id counts none.
func (r *Recorder) CountBySession(ctx context.Context, sessionID string) (int, error) {
	if sessionID == "" {
		return 0, nil
	}
	var n int
	if err := r.rdb().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		return 0, fmt.Errorf("events.CountBySession: %w", err)
	}
	return n, nil
}
