package gardener

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// A live repo lock keeps its quiet session as a live claim does; the first
// pass after the lease lapses reaps the session, frees the lock and tells the
// holder once.
func TestReapStaleSessions_ALiveRepoLockKeepsTheSessionUntilItLapses(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)

	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	idle := now.Add(-2 * time.Hour)
	holder := mustID(t)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: holder, Name: "cc/landing", ProjectSlug: "seamless", Status: core.SessionActive,
		CreatedAt: idle, UpdatedAt: idle,
	}))
	aliases, err := store.RepoLockAliasesFor(store.CWDIdentity{Host: "h", RepoRoot: "/work/app"}, "h")
	require.NoError(t, err)
	decl, err := store.DeclareRepoLock(ctx, db, store.RepoLockDeclaration{
		Name: "main", Aliases: aliases, State: core.RepoLockActive, RecordAliases: true,
	}, idle)
	require.NoError(t, err)
	lockID := decl.Lock.ID
	// Acquired at now with a 20-minute lease: live at now, quiet for two hours
	// by the session's last activity.
	_, err = store.AcquireRepoLock(ctx, db, lockID, holder, 20*time.Minute, now)
	require.NoError(t, err)

	g := New(db, nil, nil, nil, rec, Config{}, slog.Default())
	g.now = func() time.Time { return now }
	g.reapStaleSessions(ctx)
	got, _, err := store.SessionByID(ctx, db, holder)
	require.NoError(t, err)
	require.Equal(t, core.SessionActive, got.Status, "a live lock keeps its session")
	lock, _, err := store.RepoLockByID(ctx, db, lockID)
	require.NoError(t, err)
	require.Equal(t, holder, lock.Holder)

	g.now = func() time.Time { return now.Add(20 * time.Minute) }
	g.reapStaleSessions(ctx)
	got, _, err = store.SessionByID(ctx, db, holder)
	require.NoError(t, err)
	require.Equal(t, core.SessionExpired, got.Status, "the first pass after the lease lapses reaps it")
	lock, _, err = store.RepoLockByID(ctx, db, lockID)
	require.NoError(t, err)
	require.Empty(t, lock.Holder, "and frees the lapsed lock")

	pending, err := store.PendingClaimNotices(ctx, db, holder)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, store.ClaimResourceLock, pending[0].Kind)
	require.Equal(t, lockID, pending[0].ResourceID)
	require.Equal(t, store.ClaimLostReaped, pending[0].Reason)

	evs, err := rec.RecentExcluding(ctx, 20)
	require.NoError(t, err)
	var ended int
	for _, e := range evs {
		if e.Kind == core.EventSessionEnded && e.SessionID == holder {
			ended++
			require.Equal(t, "expired", e.Payload["reason"])
			require.EqualValues(t, 1, e.Payload["locks_released"])
			require.EqualValues(t, 0, e.Payload["claims_released"])
		}
	}
	require.Equal(t, 1, ended, "reaped once")
}
