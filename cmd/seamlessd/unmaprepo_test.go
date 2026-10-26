package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

func TestUnmapTargets(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	require.NoError(t, store.AdoptLocalHost(ctx, db, "alpha"))

	present := t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	require.NoError(t, store.AddRepoMapping(ctx, db, present, "here"))
	require.NoError(t, store.AddRepoMapping(ctx, db, gone, "gone-project"))
	// Another host's path is missing from this disk by definition; --stale must
	// never select it.
	_, _, err = store.RegisterProjectForCWD(ctx, db, store.CWDIdentity{
		Host: "beta", CWD: "/elsewhere/app", RepoRoot: "/elsewhere/app",
	}, "alpha")
	require.NoError(t, err)

	// --stale: only the local mapping whose path is gone.
	targets, err := unmapTargets(ctx, db, "", true, "")
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, gone, targets[0].Path)
	require.Equal(t, "gone-project", targets[0].Slug)

	// --path: the exact mapping, whether or not the directory still exists.
	targets, err = unmapTargets(ctx, db, present, false, "")
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "here", targets[0].Slug)

	// A path this machine has not mapped is an error, not an empty success --
	// including a directory nested under a mapped one.
	_, err = unmapTargets(ctx, db, filepath.Join(present, "sub"), false, "")
	require.ErrorIs(t, err, store.ErrRepoMappingNotFound)
	_, err = unmapTargets(ctx, db, "/elsewhere/app", false, "")
	require.ErrorIs(t, err, store.ErrRepoMappingNotFound)

	// --host names another machine's mapping by exact absolute path; this
	// machine's own name is the local view.
	targets, err = unmapTargets(ctx, db, "/elsewhere/app", false, "beta")
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "beta", targets[0].Host)
	_, err = unmapTargets(ctx, db, "/elsewhere/other", false, "beta")
	require.ErrorIs(t, err, store.ErrRepoMappingNotFound)
	_, err = unmapTargets(ctx, db, "elsewhere/app", false, "beta")
	require.ErrorContains(t, err, "must be absolute")

	// Absolute means absolute on THAT host's OS, whatever this one runs: a
	// Windows client's drive and UNC paths are accepted and found here too, a
	// drive path in either separator spelling, since Windows takes both. A
	// driveless "\repos\app" is relative to a current drive, so it is refused.
	for _, root := range []string{`C:\repos\app`, `\\srv\share\app`} {
		_, _, err = store.RegisterProjectForCWD(ctx, db, store.CWDIdentity{
			Host: "winbox", CWD: root, RepoRoot: root,
		}, "alpha")
		require.NoError(t, err)
	}
	for _, p := range []string{
		`C:\repos\app`, `C:\repos\app\`, "C:/repos/app", "C:/repos/app/", `C:\repos/app`, `\\srv\share\app`,
	} {
		targets, err = unmapTargets(ctx, db, p, false, "winbox")
		require.NoError(t, err, p)
		require.Len(t, targets, 1, p)
		require.Equal(t, "winbox", targets[0].Host)
	}
	_, err = unmapTargets(ctx, db, `\repos\app`, false, "winbox")
	require.ErrorContains(t, err, "must be absolute")

	targets, err = unmapTargets(ctx, db, present, false, "alpha")
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "here", targets[0].Slug)

	// Removing what --stale selected leaves nothing stale behind, and the live
	// mapping stays.
	stale, err := unmapTargets(ctx, db, "", true, "")
	require.NoError(t, err)
	_, err = store.RemoveRepoMappings(ctx, db, []string{stale[0].Path})
	require.NoError(t, err)
	stale, err = unmapTargets(ctx, db, "", true, "")
	require.NoError(t, err)
	require.Empty(t, stale)
	targets, err = unmapTargets(ctx, db, present, false, "")
	require.NoError(t, err)
	require.Len(t, targets, 1)
}
