package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func TestEventCountsAndTimes_Window(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// A fresh installation counts nothing.
	counts, err := EventCountsByKindSince(ctx, db, now.Add(-time.Hour))
	require.NoError(t, err)
	require.Empty(t, counts)
	times, err := EventKindTimesSince(ctx, db, now.Add(-time.Hour), 10)
	require.NoError(t, err)
	require.Empty(t, times)

	insert := func(id, kind string, ago time.Duration) {
		_, err := db.Exec(`INSERT INTO events (id, ts, kind, payload) VALUES (?, ?, ?, '{}')`,
			id, core.FormatTime(now.Add(-ago)), kind)
		require.NoError(t, err)
	}
	insert("01E1", "tool.call", 2*time.Minute)
	insert("01E2", "tool.call", 5*time.Minute)
	insert("01E3", "memory.written", 10*time.Minute)
	insert("01E4", "tool.call", 3*time.Hour) // outside the window

	counts, err = EventCountsByKindSince(ctx, db, now.Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, map[string]int{"tool.call": 2, "memory.written": 1}, counts, "only events inside the window count")

	times, err = EventKindTimesSince(ctx, db, now.Add(-time.Hour), 10)
	require.NoError(t, err)
	require.Equal(t, []KindAt{
		{Kind: "tool.call", At: now.Add(-2 * time.Minute)},
		{Kind: "tool.call", At: now.Add(-5 * time.Minute)},
		{Kind: "memory.written", At: now.Add(-10 * time.Minute)},
	}, times, "newest first")

	times, err = EventKindTimesSince(ctx, db, now.Add(-time.Hour), 1)
	require.NoError(t, err)
	require.Len(t, times, 1, "the limit caps the rows")

	times, err = EventKindTimesSince(ctx, db, now.Add(-time.Hour), 0)
	require.NoError(t, err)
	require.Nil(t, times, "a non-positive limit reads nothing")
}
