package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// stSeedStaleness writes one memory per way a memory can stand against a
// staleness cutoff of now minus 90 days: each kind of activity before and after
// it, exactly on it, and an archived memory.
func stSeedStaleness(t *testing.T, db *sql.DB, now time.Time) {
	t.Helper()
	ctx := context.Background()
	day := 24 * time.Hour
	cutoff := now.Add(-90 * day)
	old := now.Add(-120 * day)

	seedMemoryRow(t, db, "untouched", "untouched", old)
	seedMemoryRow(t, db, "updated-recently", "updated-recently", now.Add(-day))
	seedMemoryRow(t, db, "updated-on-the-cutoff", "updated-on-the-cutoff", cutoff)
	seedMemoryRow(t, db, "injected-recently", "injected-recently", old)
	insertEvent(t, db, core.EventInjected, "", `{"item_ids":["injected-recently"]}`, now.Add(-2*day))
	seedMemoryRow(t, db, "injected-long-ago", "injected-long-ago", old)
	insertEvent(t, db, core.EventInjected, "", `{"item_ids":["injected-long-ago"]}`, now.Add(-100*day))
	seedMemoryRow(t, db, "injected-on-the-cutoff", "injected-on-the-cutoff", old)
	insertEvent(t, db, core.EventInjected, "", `{"item_ids":["injected-on-the-cutoff"]}`, cutoff)
	seedMemoryRow(t, db, "read-recently", "read-recently", old)
	insertEvent(t, db, core.EventMemoryRead, "read-recently", "{}", now.Add(-3*day))
	seedMemoryRow(t, db, "read-long-ago", "read-long-ago", old)
	insertEvent(t, db, core.EventMemoryRead, "read-long-ago", "{}", now.Add(-95*day))
	seedMemoryRow(t, db, "archived", "archived", old)
	_, err := db.ExecContext(ctx, `UPDATE memories_index SET invalid_at = ? WHERE id = 'archived'`,
		core.FormatTime(now.Add(-110*day)))
	require.NoError(t, err)
	require.NoError(t, RebuildRetrievalStats(ctx, db))
}

// The count is StaleMemories counted: one definition (no update, injection or
// read on or after the cutoff), for every horizon, so a card that counts and a
// list that shows can never disagree.
func TestCountStaleMemories_CountsWhatStaleMemoriesLists(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	stSeedStaleness(t, db, now)

	stale, err := StaleMemories(ctx, db, now.AddDate(0, 0, -90))
	require.NoError(t, err)
	var names []string
	for _, m := range stale {
		names = append(names, m.Name)
	}
	require.ElementsMatch(t, []string{"untouched", "injected-long-ago", "read-long-ago"}, names,
		"activity on the cutoff counts as activity, and an archived memory is never stale")

	for _, days := range []int{1, 2, 30, 89, 90, 91, 96, 101, 119, 121, 400} {
		cutoff := now.AddDate(0, 0, -days)
		list, err := StaleMemories(ctx, db, cutoff)
		require.NoError(t, err)
		n, err := CountStaleMemories(ctx, db, cutoff)
		require.NoError(t, err)
		require.Equal(t, len(list), n, "horizon %dd", days)
	}
}

// Through a filtered handle, a locked project's memories are absent from the
// count itself.
func TestCountStaleMemories_LockedProjectIsAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	old := time.Now().UTC().AddDate(0, 0, -200)
	for _, slug := range []string{"example-finance", "example-ops"} {
		_, err := EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		for _, name := range []string{"a", "b"} {
			_, err := db.ExecContext(ctx, `INSERT INTO memories_index
				(id, kind, name, description, project, file_path, tags, content_hash, created_at, updated_at)
				VALUES (?, 'gotcha', ?, '', ?, ?, '[]', 'h', ?, ?)`,
				slug+"-"+name, name, slug, "memory/"+slug+"/"+name+".md", core.FormatTime(old), core.FormatTime(old))
			require.NoError(t, err)
		}
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -90)
	all, err := CountStaleMemories(ctx, db, cutoff)
	require.NoError(t, err)
	require.Equal(t, 4, all)

	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	shown, err := CountStaleMemories(ctx, view, cutoff)
	require.NoError(t, err)
	require.Equal(t, 2, shown, "only the open project's two")
}
