package lifecycle_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/lifecycle"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// newManager builds a files.Manager over a temp data dir + fresh DB.
func newManager(t *testing.T) (*files.Manager, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	return mgr, db
}

func writeMemory(t *testing.T, mgr *files.Manager, m core.Memory) core.Memory {
	t.Helper()
	now := time.Now().UTC()
	id, err := core.NewID()
	require.NoError(t, err)
	m.ID, m.Created, m.Updated, m.ValidFrom = id, now, now, now
	written, err := mgr.WriteMemory(context.Background(), m)
	require.NoError(t, err)
	return written
}

func TestSupersedeMarksOldInvalidAndTombstonesFile(t *testing.T) {
	ctx := context.Background()
	mgr, db := newManager(t)

	old := writeMemory(t, mgr, core.Memory{
		Kind: core.KindGotcha, Name: "chroma-boot-race", Project: "demo",
		Description: "old take", Body: "The old understanding.\n",
	})
	repl := writeMemory(t, mgr, core.Memory{
		Kind: core.KindGotcha, Name: "chroma-readiness-gate", Project: "demo",
		Description: "new take", Body: "The readiness gate fix.\n",
	})

	// Read the full old memory (with body) and supersede it.
	full, err := mgr.Store().ReadMemory(old.FilePath)
	require.NoError(t, err)
	at := time.Now().UTC()
	updated, err := lifecycle.Supersede(ctx, mgr, full, repl, at)
	require.NoError(t, err)
	require.NotNil(t, updated.InvalidAt)
	require.Equal(t, repl.ID, updated.SupersededBy)

	// The old memory leaves the active index...
	_, found, err := store.MemoryByName(ctx, db, "demo", "chroma-boot-race")
	require.NoError(t, err)
	require.False(t, found, "superseded memory must not resolve as active")

	active, err := store.ActiveMemories(ctx, db, "demo")
	require.NoError(t, err)
	for _, m := range active {
		require.NotEqual(t, "chroma-boot-race", m.Name, "superseded memory must leave ActiveMemories")
	}

	// ...but stays readable, with invalid_at + superseded_by set.
	got, found, err := store.MemoryByNameIncludingInvalid(ctx, db, "demo", "chroma-boot-race")
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, got.InvalidAt)
	require.Equal(t, repl.ID, got.SupersededBy)

	// The on-disk file carries a truthful tombstone line.
	onDisk, err := mgr.Store().ReadMemory(got.FilePath)
	require.NoError(t, err)
	require.Contains(t, onDisk.Body, "Superseded by demo/chroma-readiness-gate")
	require.Contains(t, onDisk.Body, repl.ID)
	require.True(t, strings.Contains(onDisk.Body, "The old understanding."),
		"tombstone must append, not replace the original body")
}

func TestMemoryRef(t *testing.T) {
	require.Equal(t, "name", lifecycle.MemoryRef("", "name"))
	require.Equal(t, "proj/name", lifecycle.MemoryRef("proj", "name"))
}

// A reader who may not read the replacement gets the body without the line that
// names it, and only that line.
func TestWithoutTombstone(t *testing.T) {
	const id, other = "01K000000000000000000REPL0", "01K000000000000000000OTHR0"
	line := "> Superseded by example-conf/secret-name (" + id + ") on 2026-11-09"
	for _, tc := range []struct {
		name, body, id, want string
	}{
		{"as Supersede wrote it", "Own words.\n\n" + line + "\n", id, "Own words.\n"},
		{"followed by a hand edit", "Own words.\n\n" + line + "\nAdded later.\n", id, "Own words.\n\nAdded later.\n"},
		{"indented by a hand edit", "Own words.\n\n  " + line + "\n", id, "Own words.\n"},
		{"the whole body", line + "\n", id, ""},
		{"another replacement's line stays", "Own words.\n\n" + line + "\n", other, "Own words.\n\n" + line + "\n"},
		{"prose naming the id stays", "See (" + id + ") for why.\n", id, "See (" + id + ") for why.\n"},
		{"no replacement to look for", "Own words.\n\n" + line + "\n", "", "Own words.\n\n" + line + "\n"},
		{"no tombstone at all", "Own words.\n", id, "Own words.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, lifecycle.WithoutTombstone(tc.body, tc.id))
		})
	}
}

// It removes exactly what Supersede added, whatever Supersede's format is.
func TestWithoutTombstone_UndoesSupersede(t *testing.T) {
	mgr, _ := newManager(t)
	old := writeMemory(t, mgr, core.Memory{Kind: core.KindGotcha, Name: "old", Description: "d", Body: "The old body.\n"})
	repl := writeMemory(t, mgr, core.Memory{Kind: core.KindGotcha, Name: "new", Project: "example-conf", Description: "d", Body: "New.\n"})
	full, err := mgr.Store().ReadMemory(old.FilePath)
	require.NoError(t, err)
	updated, err := lifecycle.Supersede(context.Background(), mgr, full, repl, time.Now().UTC())
	require.NoError(t, err)
	require.Contains(t, updated.Body, repl.ID)
	require.Equal(t, "The old body.\n", lifecycle.WithoutTombstone(updated.Body, repl.ID))
}
