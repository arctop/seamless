package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// CountStaleMemories counts what StaleMemories lists for the same cutoff: the
// active memories with no update, injection or read on or after it. It is the
// one staleness set the console counts (the Overview's "going stale" card), so
// the number on the card is the number of rows Retrieval's quiet-knowledge list
// shows, and the set the gardener's staleness pass proposes archives from.
//
// The WHERE clause is StaleMemories' own, clause for clause;
// TestCountStaleMemories_CountsWhatStaleMemoriesLists holds the two together.
func CountStaleMemories(ctx context.Context, db *sql.DB, cutoff time.Time) (int, error) {
	c := core.FormatTime(cutoff.UTC())
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM memories_index
		LEFT JOIN retrieval_stats ON retrieval_stats.item_id = memories_index.id
		WHERE invalid_at IS NULL
		  AND updated_at < ?
		  AND (last_injected_at IS NULL OR last_injected_at < ?)
		  AND (last_read_at IS NULL OR last_read_at < ?)`, c, c, c).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store.CountStaleMemories: %w", err)
	}
	return n, nil
}
