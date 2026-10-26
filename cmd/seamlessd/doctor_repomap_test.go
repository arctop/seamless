package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

func TestRepoMapCheck(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	// An empty map is the fresh-install state, not a problem.
	c := repoMapCheck(db)
	require.Equal(t, statusOK, c.status)
	require.Contains(t, c.detail, "no repos mapped")

	// Every mapped path present on disk: quiet ok with the count.
	present := t.TempDir()
	require.NoError(t, store.AddRepoMapping(ctx, db, present, "here"))
	c = repoMapCheck(db)
	require.Equal(t, statusOK, c.status)
	require.Contains(t, c.detail, "1 local mapped paths, all present")

	// A dangling entry warns, names the path and slug, and points at map-repo
	// for the moved-and-renamed case the auto-adoption cannot recognize, and at
	// unmap-repo --stale for clearing the dead entry.
	missing := filepath.Join(t.TempDir(), "gone")
	require.NoError(t, store.AddRepoMapping(ctx, db, missing, "gone-project"))
	c = repoMapCheck(db)
	require.Equal(t, statusWarn, c.status)
	require.Contains(t, c.detail, "1 of 2 local mapped paths missing")
	require.Contains(t, c.detail, missing)
	require.Contains(t, c.detail, "gone-project")
	require.Contains(t, c.detail, "map-repo --path")
	require.Contains(t, c.detail, "unmap-repo --stale")
}

// A path on another machine is missing from THIS disk by definition. Stat'ing it
// would report every remote device's repos as dangling, and a same-named
// checkout here would report a stale mapping as healthy.
func TestRepoMapCheck_RemoteRowsAreCountedNotStatted(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	present := t.TempDir()
	require.NoError(t, store.AddRepoMapping(ctx, db, present, "here"))
	// A remote client's repo root, which exists on argon and nowhere here.
	_, _, err = store.RegisterProjectForCWD(ctx, db, store.CWDIdentity{
		Host: "argon", CWD: "/srv/app/cmd", RepoRoot: "/srv/app", MainRoot: "/srv/app",
	}, config.Hostname())
	require.NoError(t, err)

	c := repoMapCheck(db)
	require.Equal(t, statusOK, c.status, "a remote path is not a dangling local one")
	require.Contains(t, c.detail, "1 local mapped paths, all present on disk")
	require.Contains(t, c.detail, "1 on argon, not verifiable from here")
}
