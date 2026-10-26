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
