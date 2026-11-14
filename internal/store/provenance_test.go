package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func provenanceStamp(kind, id, project, source, approval string) ProvenanceStamp {
	return ProvenanceStamp{ItemKind: kind, ItemID: id, Project: project, Source: source, ApprovalID: approval}
}

func TestProvenance_UpsertAndLists(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	t0, t1, t2 := migration039Now, migration039Now.Add(time.Minute), migration039Now.Add(2*time.Minute)
	for _, c := range []struct {
		s  ProvenanceStamp
		at time.Time
	}{
		{provenanceStamp(core.ItemKindTask, "01TASK", "example-dataroom", "example-legal", "01APPROVALA"), t0},
		{provenanceStamp(core.ItemKindTask, "01TASK", "example-dataroom", "example-finance", "01APPROVALB"), t1},
		{provenanceStamp(core.ItemKindMemory, "01MEM", "example-dataroom", "example-legal", "01APPROVALA"), t1},
		{provenanceStamp(core.ItemKindSession, "01SESS", "example-finance", "example-legal", "01APPROVALC"), t2},
	} {
		require.NoError(t, ProvenanceUpsert(ctx, db, c.s, c.at))
	}

	got, err := ProvenanceForItem(ctx, db, core.ItemKindTask, "01TASK")
	require.NoError(t, err)
	require.Equal(t, []ProvenanceStamp{
		{ItemKind: core.ItemKindTask, ItemID: "01TASK", Project: "example-dataroom", Source: "example-finance", ApprovalID: "01APPROVALB", CreatedAt: t1},
		{ItemKind: core.ItemKindTask, ItemID: "01TASK", Project: "example-dataroom", Source: "example-legal", ApprovalID: "01APPROVALA", CreatedAt: t0},
	}, got, "by source")
	got, err = ProvenanceForItem(ctx, db, core.ItemKindNote, "01TASK")
	require.NoError(t, err)
	require.Empty(t, got, "the kind is part of the item")

	in, err := ProvenanceForProject(ctx, db, "example-dataroom", "", 0)
	require.NoError(t, err)
	require.Len(t, in, 3)
	require.Equal(t, t1, in[0].CreatedAt, "newest first")
	fromLegal, err := ProvenanceForProject(ctx, db, "example-dataroom", "example-legal", 0)
	require.NoError(t, err)
	require.Len(t, fromLegal, 2, "the came-from filter")
	one, err := ProvenanceForProject(ctx, db, "example-dataroom", "", 1)
	require.NoError(t, err)
	require.Len(t, one, 1)
	everywhere, err := ProvenanceFromSource(ctx, db, "example-legal", 0)
	require.NoError(t, err)
	require.Len(t, everywhere, 3)
	require.Equal(t, "01SESS", everywhere[0].ItemID)

	// Stamping again keeps the first approval and time, and follows the item
	// to the project it is in now.
	require.NoError(t, ProvenanceUpsert(ctx, db,
		provenanceStamp(core.ItemKindTask, "01TASK", "example-hr", "example-legal", "01APPROVALZ"), t2))
	got, err = ProvenanceForItem(ctx, db, core.ItemKindTask, "01TASK")
	require.NoError(t, err)
	require.Equal(t, ProvenanceStamp{ItemKind: core.ItemKindTask, ItemID: "01TASK", Project: "example-hr", Source: "example-legal",
		ApprovalID: "01APPROVALA", CreatedAt: t0}, got[1])
	require.Equal(t, 4, migration039Count(t, db, "item_provenance"), "an upsert adds no row")

	// Inside the transaction that writes the item.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, ProvenanceUpsert(ctx, tx, provenanceStamp(core.ItemKindNote, "01NOTE", "example-dataroom", "example-legal", "01APPROVALA"), t2))
	require.NoError(t, tx.Rollback())
	got, err = ProvenanceForItem(ctx, db, core.ItemKindNote, "01NOTE")
	require.NoError(t, err)
	require.Empty(t, got, "a rolled-back write leaves no stamp")
}

func TestProvenance_Validates(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	for name, s := range map[string]ProvenanceStamp{
		"a lab":                    provenanceStamp("lab", "01TR", "example-ops", "example-legal", "01A"),
		"no kind":                  provenanceStamp("", "01T", "example-ops", "example-legal", "01A"),
		"no item":                  provenanceStamp(core.ItemKindTask, "", "example-ops", "example-legal", "01A"),
		"a padded item":            provenanceStamp(core.ItemKindTask, " 01T", "example-ops", "example-legal", "01A"),
		"no source":                provenanceStamp(core.ItemKindTask, "01T", "example-ops", "", "01A"),
		"its own project":          provenanceStamp(core.ItemKindTask, "01T", "example-legal", "example-legal", "01A"),
		"no approval":              provenanceStamp(core.ItemKindTask, "01T", "example-ops", "example-legal", " "),
		"a padded project":         provenanceStamp(core.ItemKindTask, "01T", "example-ops ", "example-legal", "01A"),
		"a global item, no source": provenanceStamp(core.ItemKindTask, "01T", "", "", "01A"),
	} {
		require.ErrorIs(t, ProvenanceUpsert(ctx, db, s, migration039Now), ErrInvalidProvenance, name)
	}
	require.NoError(t, ProvenanceUpsert(ctx, db, provenanceStamp(core.ItemKindTask, "01T", "", "example-legal", "01A"), migration039Now),
		"a global item may carry a stamp")
	require.NoError(t, ProvenanceUpsert(ctx, db, provenanceStamp(core.ItemKindTrial, "01TR", "example-ops", "example-legal", "01A"), migration039Now),
		"a trial is the agent's own text, stamped like a task")
	require.Zero(t, migration039Count(t, db, "access_flows"), "a stamp opens and confines nothing")
}

// The console's filtered handle hides a stamp with its item and with the
// project the item is in, and keeps one that only names a hidden project as
// its source: a slug is no content.
func TestProvenance_OpenViewHidesWithTheItem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, _, hiddenTask, hiddenSession, _ := seedViewFixture(t, db)
	ctx := context.Background()
	for _, s := range []ProvenanceStamp{
		provenanceStamp(core.ItemKindTask, hiddenTask, "example-finance", "example-legal", "01A"),
		provenanceStamp(core.ItemKindSession, hiddenSession, "example-finance", "example-legal", "01A"),
		// A row naming the hidden item's id under a visible project: hidden by the item.
		provenanceStamp(core.ItemKindTask, hiddenTask, "example-ops", "example-hr", "01A"),
		provenanceStamp(core.ItemKindTask, "t-shown", "example-ops", "example-finance", "01B"),
	} {
		require.NoError(t, ProvenanceUpsert(ctx, db, s, migration039Now))
	}

	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	got, err := ProvenanceForProject(ctx, view, "example-finance", "", 0)
	require.NoError(t, err)
	require.Empty(t, got, "a hidden project's stamps")
	got, err = ProvenanceForItem(ctx, view, core.ItemKindTask, hiddenTask)
	require.NoError(t, err)
	require.Empty(t, got, "a hidden item's stamps, whatever project a row names")
	got, err = ProvenanceFromSource(ctx, view, "example-finance", 0)
	require.NoError(t, err)
	require.Len(t, got, 1, "a stamp naming the hidden project only as its source stays")
	require.Equal(t, "t-shown", got[0].ItemID)

	all, err := ProvenanceFromSource(ctx, db, "example-legal", 0)
	require.NoError(t, err)
	require.Len(t, all, 2, "the unfiltered handle sees them all")
}
