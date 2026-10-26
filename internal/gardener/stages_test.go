package gardener

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

func TestProposeStaleStages(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	defer func() { _ = mgr.Close() }()

	now := time.Now().UTC()
	old := now.Add(-30 * 24 * time.Hour)

	// Old with no Status header at all: the classic milestone breadcrumb.
	writeMem(t, mgr, "landed-breadcrumb", "", "1", core.KindStage, old, "F1-F4 landed, all green")
	// Old and done: finished gate, ready to retire.
	writeMem(t, mgr, "finished-gate", "", "2", core.KindStage, old, "Status: done\nGate: ai\n\nshipped")
	// Old but a live gate: never proposed, whatever the age.
	writeMem(t, mgr, "live-gate", "", "3", core.KindStage, old, "Status: blocked\nGate: human\n\nwaiting")
	// Gateless but fresh: still inside the update window.
	writeMem(t, mgr, "fresh-breadcrumb", "", "4", core.KindStage, now.Add(-time.Hour), "landed yesterday")
	// Old and gateless but referenced: a [[link]] protects it, same as staleness.
	writeMem(t, mgr, "referenced-stage", "", "5", core.KindStage, old, "landed long ago")
	writeMem(t, mgr, "linker", "", "6", core.KindReference, now, "see [[referenced-stage]] for the story")
	// An old non-stage memory must not concern this pass.
	writeMem(t, mgr, "old-gotcha", "", "7", core.KindGotcha, old, "not a stage")

	g := New(db, mgr, nil, nil, events.NewRecorder(db), Config{StaleStageDays: 14}, slog.Default())

	created, err := g.proposeStaleStages(ctx, seenKeys{})
	require.NoError(t, err)
	require.Equal(t, 2, created)

	proposals, err := store.PendingProposals(ctx, db, store.ProposalArchive)
	require.NoError(t, err)
	names := make([]string, 0, len(proposals))
	reasons := make(map[string]string, len(proposals))
	for _, p := range proposals {
		name, _ := p.Payload["name"].(string)
		reason, _ := p.Payload["reason"].(string)
		names = append(names, name)
		reasons[name] = reason
	}
	require.ElementsMatch(t, []string{"landed-breadcrumb", "finished-gate"}, names)
	require.Contains(t, reasons["landed-breadcrumb"], "no parsable Status header")
	require.Contains(t, reasons["finished-gate"], "status done")

	// A key already proposed (by this pass or staleness) is not raised again.
	again, err := g.proposeStaleStages(ctx, loadSeenKeys(t, ctx, db))
	require.NoError(t, err)
	require.Equal(t, 0, again)
}

func TestProposeStaleStages_Disabled(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	defer func() { _ = mgr.Close() }()

	writeMem(t, mgr, "landed-breadcrumb", "", "1", core.KindStage,
		time.Now().UTC().Add(-300*24*time.Hour), "no header, ancient")

	g := New(db, mgr, nil, nil, events.NewRecorder(db), Config{}, slog.Default())
	created, err := g.proposeStaleStages(ctx, seenKeys{})
	require.NoError(t, err)
	require.Equal(t, 0, created, "StaleStageDays 0 disables the pass")
}

func loadSeenKeys(t *testing.T, ctx context.Context, db *sql.DB) seenKeys {
	t.Helper()
	seen, err := loadSeen(ctx, db)
	require.NoError(t, err)
	return seen
}
