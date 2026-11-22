package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// The stamp starts unset, moves forward only, and a corrupt value is an
// error to read and is replaced by the next mark.
func TestMishapsReviewed_StampMovesForwardOnly(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Now().UTC()

	_, found, err := MishapsReviewedAt(ctx, db)
	require.NoError(t, err)
	require.False(t, found, "no stamp before the first review")

	at, err := MarkMishapsReviewed(ctx, db, now.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, at.Equal(now.Add(-time.Hour)))
	got, found, err := MishapsReviewedAt(ctx, db)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, got.Equal(now.Add(-time.Hour)), "the stamp reads back at full precision")

	at, err = MarkMishapsReviewed(ctx, db, now.Add(-2*time.Hour))
	require.NoError(t, err)
	require.True(t, at.Equal(now.Add(-time.Hour)), "an older mark keeps the stamp")
	at, err = MarkMishapsReviewed(ctx, db, now)
	require.NoError(t, err)
	require.True(t, at.Equal(now))

	require.NoError(t, SetSetting(ctx, db, SettingMishapsReviewed, "not a time"))
	_, _, err = MishapsReviewedAt(ctx, db)
	require.ErrorContains(t, err, `"not a time"`)
	at, err = MarkMishapsReviewed(ctx, db, now.Add(-3*time.Hour))
	require.NoError(t, err)
	require.True(t, at.Equal(now.Add(-3*time.Hour)), "a corrupt stamp is replaced")
}

// The count is every report strictly after the stamp, of every type, and
// nothing of another kind.
func TestMishapsReviewed_CountsReportsAfterTheStamp(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Now().UTC()
	mishapFleet(t, db, now) // 12 reports, 1h to 8 days old, and one other event

	n, err := CountMishapsAfter(ctx, db, time.Time{})
	require.NoError(t, err)
	require.Equal(t, 12, n, "zero: every report")
	n, err = CountMishapsAfter(ctx, db, now.Add(-3*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 2, n, "strictly after: the 3h-old report is not counted")
	n, err = CountMishapsAfter(ctx, db, now)
	require.NoError(t, err)
	require.Zero(t, n)
}

// Through the console's filtered handle a hidden project's reports are not
// counted, and the stamp (a settings row) reads the same.
func TestMishapsReviewed_ReadThroughTheView(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	mishapAt(t, db, "01MS0000000000000000000001", "S1", "example-ops", `{"description":"shell-trap: x","type":"shell-trap"}`, now.Add(-time.Hour))
	mishapAt(t, db, "01MS0000000000000000000002", "S2", "example-finance", `{"description":"live-state: y","type":"live-state"}`, now.Add(-time.Hour))
	_, err = MarkMishapsReviewed(ctx, db, now.Add(-2*time.Hour))
	require.NoError(t, err)

	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	at, found, err := MishapsReviewedAt(ctx, view)
	require.NoError(t, err)
	require.True(t, found)
	n, err := CountMishapsAfter(ctx, view, at)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the locked project's report is absent")
	n, err = CountMishapsAfter(ctx, db, at)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.Equal(t, core.EventKind("mishaps.reviewed"), EventMishapsReviewed)
}
