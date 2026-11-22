package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// CountInjectedSessions counts exactly the sessions InjectedSessions lists,
// an injection in no session included, and on a filtered handle a locked
// project's sessions are absent from both.
func TestLibCountInjectedSessions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := storetest.OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, s := range []core.Session{
		{ID: "S1", Name: "cc/open0001", ProjectSlug: "example-open"},
		{ID: "S2", Name: "cc/open0002", ProjectSlug: "example-open"},
		{ID: "S3", Name: "cc/lock0003", ProjectSlug: "example-locked"},
	} {
		s.Status, s.CreatedAt, s.UpdatedAt = core.SessionCompleted, base, base
		require.NoError(t, store.CreateSession(ctx, db, s))
	}
	rec := events.NewRecorder(db)
	for i, e := range []core.Event{
		{SessionID: "S1", Payload: map[string]any{"item_ids": []string{"M1", "M2"}}},
		{SessionID: "S1", Payload: map[string]any{"item_ids": []string{"M1"}}},
		{SessionID: "S2", ItemID: "M1"},
		{SessionID: "S3", ProjectSlug: "example-locked", ItemID: "M1"},
		{Payload: map[string]any{"item_ids": []string{"M1"}}}, // in no session
		{SessionID: "S2", Payload: map[string]any{"item_ids": []string{"M2"}}},
	} {
		e.Kind, e.TS = core.EventInjected, base.Add(time.Duration(i)*time.Hour)
		_, err := rec.Record(ctx, e)
		require.NoError(t, err)
	}
	_, err = rec.Record(ctx, core.Event{Kind: core.EventMemoryRead, SessionID: "S1", ItemID: "M3"})
	require.NoError(t, err)

	for _, tc := range []struct {
		item string
		want int
	}{
		{"M1", 4}, // S1, S2, S3 and the injection in no session
		{"M2", 2},
		{"M3", 0}, // read, never injected
		{"", 0},
	} {
		n, err := store.CountInjectedSessions(ctx, db, tc.item)
		require.NoError(t, err)
		require.Equal(t, tc.want, n, tc.item)
		listed, err := store.InjectedSessions(ctx, db, tc.item, 0)
		require.NoError(t, err)
		require.Len(t, listed, n, "%s: the count is the list's length", tc.item)
	}

	view, err := store.OpenView(path, []string{"example-locked"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	n, err := store.CountInjectedSessions(ctx, view, "M1")
	require.NoError(t, err)
	require.Equal(t, 3, n, "the locked project's session is absent from the read")
	listed, err := store.InjectedSessions(ctx, view, "M1", 0)
	require.NoError(t, err)
	require.Len(t, listed, 3)
	for _, s := range listed {
		require.NotEqual(t, "S3", s.SessionID)
	}
}
