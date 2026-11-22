package events

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// CountBySession counts every event of one session, past the limit a
// timeline reads, and none of another session's.
func TestSnCountBySession_CountsPastTheTimelineLimit(t *testing.T) {
	rec := newRecorder(t)
	ctx := context.Background()
	sess, other := snID(t), snID(t)
	base := time.Now().UTC().Add(-time.Hour)
	for i := range 7 {
		_, err := rec.Record(ctx, core.Event{Kind: core.EventToolCall, SessionID: sess, TS: base.Add(time.Duration(i) * time.Second)})
		require.NoError(t, err)
	}
	_, err := rec.Record(ctx, core.Event{Kind: core.EventToolCall, SessionID: other})
	require.NoError(t, err)

	first, err := rec.BySession(ctx, sess, 5)
	require.NoError(t, err)
	require.Len(t, first, 5, "the timeline's read stops at its limit")
	n, err := rec.CountBySession(ctx, sess)
	require.NoError(t, err)
	require.Equal(t, 7, n, "the count does not")
	n, err = rec.CountBySession(ctx, "")
	require.NoError(t, err)
	require.Zero(t, n, "an empty id counts none")
}

// Through the console's filtered handle, a locked project's events are not
// counted: the count reads what the timeline reads.
func TestSnCountBySession_LockedProjectIsAbsent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	view, err := store.OpenView(dbPath, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })

	rec := NewRecorder(db)
	reading := rec.ReadingFrom(view)
	ctx := context.Background()
	sess := snID(t)
	for _, project := range []string{"example-finance", "example-finance", "example-ops"} {
		_, err := rec.Record(ctx, core.Event{Kind: core.EventToolCall, SessionID: sess, ProjectSlug: project})
		require.NoError(t, err)
	}
	n, err := rec.CountBySession(ctx, sess)
	require.NoError(t, err)
	require.Equal(t, 3, n, "the main handle counts everything")
	n, err = reading.CountBySession(ctx, sess)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the view counts only what it shows")
}

// snID mints a ULID for a test session.
func snID(t *testing.T) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	return id
}
