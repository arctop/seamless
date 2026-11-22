package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/arctop/seamless/internal/core"
)

// The console library's reads (the Memories and Notes screens). Each one runs
// on the handle it is given: the console passes the request's (s.db), so a
// locked project's sessions and events are absent from the read itself.

// CountInjectedSessions counts the sessions InjectedSessions would list for
// itemID with no limit: every retrieval.injected event naming the item, in
// the item_id column or the payload's item_ids array, grouped by session (an
// injection that ran in no session counts once, as ""). The memory reader lists
// the latest few and says how many there are in all; the two must agree, so
// the match below is InjectedSessions' own.
func CountInjectedSessions(ctx context.Context, db *sql.DB, itemID string) (int, error) {
	if itemID == "" {
		return 0, nil
	}
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT e.session_id) FROM events e
		WHERE e.kind = ? AND (e.item_id = ? OR EXISTS (
			SELECT 1 FROM json_each(CASE WHEN json_valid(e.payload) THEN e.payload ELSE '{}' END, '$.item_ids') j
			WHERE j.value = ?))`,
		string(core.EventInjected), itemID, itemID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store.CountInjectedSessions: %w", err)
	}
	return n, nil
}
