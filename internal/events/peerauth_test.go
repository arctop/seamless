package events

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// ReadingFrom answers reads from the filtered handle and shares everything else:
// an event recorded through either reaches both's subscribers, and the reads
// see only what the filtered handle shows.
func TestReadingFrom_ReadsThroughTheViewAndSharesTheHub(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	view, err := store.OpenView(dbPath, []string{"arctop-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })

	main := NewRecorder(db)
	reading := main.ReadingFrom(view)
	ch, unsubscribe := reading.Subscribe()
	defer unsubscribe()

	ctx := context.Background()
	hidden, err := main.Record(ctx, core.Event{Kind: core.EventToolCall, ProjectSlug: "arctop-finance"})
	require.NoError(t, err)
	shown, err := reading.Record(ctx, core.Event{Kind: core.EventToolCall, ProjectSlug: "arctop-ops"})
	require.NoError(t, err)

	got := []string{(<-ch).ID, (<-ch).ID}
	require.Equal(t, []string{hidden, shown}, got, "both recorders publish to one hub")

	_, found, err := reading.ByID(ctx, hidden)
	require.NoError(t, err)
	require.False(t, found, "a read through the view does not see the hidden project")
	_, found, err = reading.ByID(ctx, shown)
	require.NoError(t, err)
	require.True(t, found)
	_, found, err = main.ByID(ctx, hidden)
	require.NoError(t, err)
	require.True(t, found, "the main recorder still reads everything")
	recent, err := reading.RecentExcluding(ctx, 10)
	require.NoError(t, err)
	require.Len(t, recent, 1)
}
