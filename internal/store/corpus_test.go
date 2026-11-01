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

// The write path records nothing while the watch is disarmed, and records in
// the same statement once it is armed; a Seamless write never clears a drift.
func TestCorpusRecord_ArmedGatesTheWritePath(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	path := "memory/demo/rule.md"

	require.NoError(t, store.RecordCorpusWrite(ctx, db, "memory", "01A", "demo", path, "h1", now))
	_, ok, err := store.CorpusRecordByPath(ctx, db, path)
	require.NoError(t, err)
	require.False(t, ok, "disarmed: nothing recorded")

	require.NoError(t, store.SetCorpusArmed(ctx, db, true))
	require.NoError(t, store.RecordCorpusWrite(ctx, db, "memory", "01A", "demo", path, "h1", now))
	rec, ok, err := store.CorpusRecordByPath(ctx, db, path)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "h1", rec.Hash)
	require.Equal(t, store.CorpusViaWrite, rec.Via)

	require.NoError(t, store.SetCorpusDrift(ctx, db, path, "memory", "01A", "demo", store.DriftChanged, "h2", []string{"unicode.bidi"}, now))
	require.NoError(t, store.RecordCorpusWrite(ctx, db, "memory", "01A", "demo", path, "h3", now))
	rec, _, err = store.CorpusRecordByPath(ctx, db, path)
	require.NoError(t, err)
	require.True(t, rec.Drifted(), "a Seamless write leaves the quarantine in place")
	require.Equal(t, "h1", rec.DriftBase, "the base is the hash recorded when the drift was found")
	require.Equal(t, "h3", rec.Hash)
	require.Equal(t, []string{"unicode.bidi"}, rec.DriftVerdicts)

	require.NoError(t, store.ClearCorpusDrift(ctx, db, path, "h2", store.CorpusViaAccept, now))
	rec, _, err = store.CorpusRecordByPath(ctx, db, path)
	require.NoError(t, err)
	require.False(t, rec.Drifted())
	require.Equal(t, "h2", rec.Hash)
	require.ErrorIs(t, store.ClearCorpusDrift(ctx, db, "memory/demo/none.md", "x", "accept", now), store.ErrNoCorpusRecord)

	require.NoError(t, store.SetCorpusReview(ctx, db, path, store.ReviewFlagged, "d1", []string{"secret.jwt"}, store.ReviewByReview, now))
	counts, err := store.CountCorpus(ctx, db)
	require.NoError(t, err)
	require.Equal(t, store.CorpusCounts{Items: 1, Flagged: 1}, counts)
	require.Error(t, store.SetCorpusReview(ctx, db, path, "maybe", "d1", nil, store.ReviewByReview, now))

	require.NoError(t, store.RecordCorpusRemove(ctx, db, path))
	_, ok, err = store.CorpusRecordByPath(ctx, db, path)
	require.NoError(t, err)
	require.False(t, ok)
}

// InjectedSessions finds every session an item was injected into, whether
// the event names it in item_id or in the payload's item_ids array.
func TestInjectedSessions(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i, e := range []core.Event{
		{SessionID: "S1", Payload: map[string]any{"item_ids": []string{"M1", "M2"}}},
		{SessionID: "S1", Payload: map[string]any{"item_ids": []string{"M1"}}},
		{SessionID: "S2", ItemID: "M1"},
		{SessionID: "S3", Payload: map[string]any{"item_ids": []string{"M2"}}},
	} {
		e.Kind, e.TS = core.EventInjected, base.Add(time.Duration(i)*time.Hour)
		_, err := rec.Record(ctx, e)
		require.NoError(t, err)
	}
	_, err = rec.Record(ctx, core.Event{Kind: core.EventMemoryRead, SessionID: "S9", ItemID: "M1"})
	require.NoError(t, err)

	got, err := store.InjectedSessions(ctx, db, "M1", 0)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "S2", got[0].SessionID, "most recent first")
	require.Equal(t, "S1", got[1].SessionID)
	require.Equal(t, 2, got[1].Times)
	require.Equal(t, base, got[1].First)
	none, err := store.InjectedSessions(ctx, db, "", 0)
	require.NoError(t, err)
	require.Empty(t, none)
}
