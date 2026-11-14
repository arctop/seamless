package lifecycle_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/lifecycle"
	"github.com/arctop/seamless/internal/store"
)

// Supersession keeps the union of both memories' provenance stamps (plan
// fenced-sessions P6): the replacement names what the memory it replaced may
// have held, its own first approvals stay, and nothing is rewritten when it
// already names them all.
func TestUnionStamps_TheReplacementCarriesBoth(t *testing.T) {
	ctx := context.Background()
	mgr, db := newManager(t)
	old := writeMemory(t, mgr, core.Memory{Kind: core.KindGotcha, Name: "stamp-old", Project: "example-data",
		Description: "old", Body: "old\n", MayContain: []core.Stamp{
			{Project: "example-legal", Via: core.StampViaGrant, Ref: "01OLDLEGAL"},
			{Project: "example-finance", Via: core.StampViaMove, Ref: "01OLDFIN"},
			{Project: "example-ops", Via: core.StampViaGrant, Ref: "01OWNPROJECT"},
		}})
	repl := writeMemory(t, mgr, core.Memory{Kind: core.KindGotcha, Name: "stamp-new", Project: "example-ops",
		Description: "new", Body: "new\n", MayContain: []core.Stamp{
			{Project: "example-legal", Via: core.StampViaGrant, Ref: "01NEWLEGAL"},
		}})

	full, err := mgr.Store().ReadMemory(old.FilePath)
	require.NoError(t, err)
	updated, err := lifecycle.Supersede(ctx, mgr, full, repl, time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, full.MayContain, updated.MayContain, "the tombstone keeps the old memory's own stamps")

	got, err := lifecycle.UnionStamps(ctx, mgr, repl, updated.MayContain)
	require.NoError(t, err)
	want := []core.Stamp{
		{Project: "example-legal", Via: core.StampViaGrant, Ref: "01NEWLEGAL"},
		{Project: "example-finance", Via: core.StampViaMove, Ref: "01OLDFIN"},
	}
	require.Equal(t, want, got.MayContain, "its own approval for a project stays; its own project is never a stamp")
	onDisk, err := mgr.Store().ReadMemory(repl.FilePath)
	require.NoError(t, err)
	require.Equal(t, want, onDisk.MayContain)
	rows, err := store.ProvenanceForItem(ctx, db, core.ItemKindMemory, repl.ID)
	require.NoError(t, err)
	require.Len(t, rows, 2, "the index mirrors the union")

	// Again: nothing to add, nothing rewritten.
	again, err := lifecycle.UnionStamps(ctx, mgr, got, updated.MayContain)
	require.NoError(t, err)
	require.Equal(t, got.ContentHash, again.ContentHash)
	// No stamps at all: the replacement is not even read.
	none, err := lifecycle.UnionStamps(ctx, mgr, core.Memory{Name: "nothing"}, nil)
	require.NoError(t, err)
	require.Equal(t, "nothing", none.Name)
}

// Archiving rewrites the file with what it carries: the stamps stay.
func TestArchive_KeepsTheStamps(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newManager(t)
	stamps := []core.Stamp{{Project: "example-legal", Via: core.StampViaGrant, Ref: "01LEGAL"}}
	mem := writeMemory(t, mgr, core.Memory{Kind: core.KindGotcha, Name: "stamp-archive", Project: "example-data",
		Description: "d", Body: "b\n", MayContain: stamps})
	full, err := mgr.Store().ReadMemory(mem.FilePath)
	require.NoError(t, err)
	archived, err := lifecycle.Archive(ctx, mgr, full, "stale", time.Now().UTC())
	require.NoError(t, err)
	onDisk, err := mgr.Store().ReadMemory(archived.FilePath)
	require.NoError(t, err)
	require.Equal(t, stamps, onDisk.MayContain)
}
