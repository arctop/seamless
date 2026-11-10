package store_test

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

func TestConsoleLocks(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, slug := range []string{"example-conf", "example-open"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, "example-conf", core.IsolationConfidential))
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

	require.ErrorIs(t, store.AddConsoleLock(ctx, db, "example-open", "cred", now), store.ErrConsoleLockUnfenced)
	require.ErrorIs(t, store.AddConsoleLock(ctx, db, "example-gone", "cred", now), store.ErrProjectNotFound)
	require.ErrorIs(t, store.RemoveConsoleLock(ctx, db, "example-conf"), store.ErrConsoleLockNotFound)

	require.NoError(t, store.AddConsoleLock(ctx, db, "example-conf", "cred", now))
	require.ErrorIs(t, store.AddConsoleLock(ctx, db, "example-conf", "cred", now), store.ErrConsoleLockExists)
	locks, err := store.ConsoleLocks(ctx, db)
	require.NoError(t, err)
	require.Equal(t, []store.ConsoleLock{{Project: "example-conf", Credential: "cred", LockedAt: now}}, locks)
	slugs, err := store.ConsoleLockedSlugs(ctx, db)
	require.NoError(t, err)
	require.Equal(t, []string{"example-conf"}, slugs)
	has, err := store.HasConsoleLock(ctx, db, "example-conf")
	require.NoError(t, err)
	require.True(t, has)

	require.NoError(t, store.RemoveConsoleLock(ctx, db, "example-conf"))
	has, err = store.HasConsoleLock(ctx, db, "example-conf")
	require.NoError(t, err)
	require.False(t, has)
}

// A fence stays up under a lock of either kind; narrowing it to another fence is
// still allowed, and an unlocked project opens as before.
func TestLoosenProjectIsolation(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, slug := range []string{"example-console", "example-file", "example-free"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, core.IsolationSealed))
	}
	require.NoError(t, store.AddConsoleLock(ctx, db, "example-console", "cred", time.Now()))
	force := []string{"example-file"}

	for _, slug := range []string{"example-console", "example-file"} {
		require.ErrorIs(t, store.LoosenProjectIsolation(ctx, db, slug, core.IsolationOpen, force), store.ErrIsolationLocked, slug)
		state, err := store.IsolationOf(ctx, db, slug)
		require.NoError(t, err)
		require.Equal(t, core.IsolationSealed, state, "%s keeps its fence", slug)

		require.NoError(t, store.LoosenProjectIsolation(ctx, db, slug, core.IsolationConfidential, force), slug)
		state, err = store.IsolationOf(ctx, db, slug)
		require.NoError(t, err)
		require.Equal(t, core.IsolationConfidential, state, slug)
	}
	require.NoError(t, store.LoosenProjectIsolation(ctx, db, "example-free", core.IsolationOpen, force))
	require.ErrorIs(t, store.LoosenProjectIsolation(ctx, db, "example-gone", core.IsolationOpen, force), store.ErrProjectNotFound)
}
