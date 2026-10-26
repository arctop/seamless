package gardener

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// insertUtilityEvent writes a raw event row with an explicit timestamp,
// project, and session -- the shape the demand/exposure queries read.
func insertUtilityEvent(t *testing.T, db *sql.DB, kind core.EventKind, session, project, itemID, payload string, ts time.Time) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		mustID(t), core.FormatTime(ts), string(kind), session, project, itemID, payload)
	require.NoError(t, err)
}

// writeMemID is writeMem returning the generated memory id.
func writeMemID(t *testing.T, mgr *files.Manager, name, project string, kind core.MemoryKind, created time.Time) string {
	t.Helper()
	id := mustID(t)
	_, err := mgr.WriteMemory(context.Background(), core.Memory{
		ID: id, Kind: kind, Name: name, Description: name + " description",
		Project: project, Body: "body of " + name,
		Created: created, Updated: created, ValidFrom: created,
	})
	require.NoError(t, err)
	return id
}

// The dead-weight pass proposes archiving only memories with real exposure and
// zero demand: age, exposure, demand, kind, and favorite protections all gate.
func TestProposeDeadWeight(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	defer func() { _ = mgr.Close() }()

	now := time.Now().UTC()
	old := now.AddDate(0, 0, -20)
	recent := now.Add(-time.Hour)

	dead := writeMemID(t, mgr, "dead-weight", "p", core.KindGotcha, old)
	young := writeMemID(t, mgr, "too-young", "p", core.KindGotcha, now.AddDate(0, 0, -5))
	demanded := writeMemID(t, mgr, "still-wanted", "p", core.KindGotcha, old)
	unexposed := writeMemID(t, mgr, "barely-briefed", "p", core.KindGotcha, old)
	rule := writeMemID(t, mgr, "old-rule", "p", core.KindConstraint, old)
	starred := writeMemID(t, mgr, "starred-quiet", "p", core.KindGotcha, old)
	_, err = db.ExecContext(ctx, `UPDATE memories_index SET favorite = 1 WHERE id = ?`, starred)
	require.NoError(t, err)

	// Everyone but "barely-briefed" gets exposure past the floor.
	for i := 0; i < deadWeightMinInjects+5; i++ {
		payload := fmt.Sprintf(`{"item_ids":[%q,%q,%q,%q,%q],"hook":"session-start"}`,
			dead, young, demanded, rule, starred)
		insertUtilityEvent(t, db, core.EventInjected, "s"+strconv.Itoa(i), "p", "", payload, recent)
	}
	insertUtilityEvent(t, db, core.EventInjected, "sx", "p", "",
		fmt.Sprintf(`{"item_ids":[%q],"hook":"session-start"}`, unexposed), recent)
	// One recall hit keeps "still-wanted" alive.
	insertUtilityEvent(t, db, core.EventInjected, "sr", "p", "",
		fmt.Sprintf(`{"item_ids":[%q],"source":"recall"}`, demanded), recent)

	g := New(db, mgr, nil, nil, events.NewRecorder(db), Config{}, slog.Default())
	seen := seenKeys{}
	n, err := g.proposeDeadWeight(ctx, seen)
	require.NoError(t, err)
	require.Equal(t, 1, n, "only the exposed, undemanded, unprotected memory")

	proposals, err := store.PendingProposals(ctx, db, store.ProposalArchive)
	require.NoError(t, err)
	require.Len(t, proposals, 1)
	require.Equal(t, "dead-weight", proposals[0].Payload["name"])
	require.Contains(t, proposals[0].Payload["reason"], "dead weight")

	// Idempotent within the shared archive key namespace.
	n, err = g.proposeDeadWeight(ctx, seen)
	require.NoError(t, err)
	require.Zero(t, n)
}

// The readiness evaluator latches a project once its demand history clears
// every threshold, records the arming event, and never latches twice.
func TestEvaluateUtilityActivation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	// "ready": demand old enough, busy enough, broad enough. Each event is its
	// own session (dedup-proof) recalling its own memory.
	insertUtilityEvent(t, db, core.EventInjected, "s-old", "ready", "",
		`{"item_ids":["M0"],"source":"recall"}`, now.AddDate(0, 0, -store.UtilityReadyMinAgeDays-1))
	for i := 0; i < store.UtilityReadyMinEvents; i++ {
		insertUtilityEvent(t, db, core.EventInjected, "s"+strconv.Itoa(i), "ready", "",
			fmt.Sprintf(`{"item_ids":["M%d"],"source":"recall"}`, i%store.UtilityReadyMinMemories),
			now.Add(-time.Hour))
	}
	// "young": same volume, but demand history started yesterday.
	for i := 0; i < store.UtilityReadyMinEvents; i++ {
		insertUtilityEvent(t, db, core.EventInjected, "y"+strconv.Itoa(i), "young", "",
			fmt.Sprintf(`{"item_ids":["Y%d"],"source":"recall"}`, i%store.UtilityReadyMinMemories),
			now.Add(-24*time.Hour))
	}

	g := New(db, nil, nil, nil, events.NewRecorder(db), Config{}, slog.Default())
	g.evaluateUtilityActivation(ctx)

	a, err := store.GetUtilityActivation(ctx, db)
	require.NoError(t, err)
	require.NotNil(t, a.Projects["ready"].ReadyAt, "mature demand history latches")
	require.Nil(t, a.Projects["young"].ReadyAt, "yesterday's burst does not")

	// Latching is one-way and idempotent.
	first := *a.Projects["ready"].ReadyAt
	g.evaluateUtilityActivation(ctx)
	a2, err := store.GetUtilityActivation(ctx, db)
	require.NoError(t, err)
	require.Equal(t, first.Unix(), a2.Projects["ready"].ReadyAt.Unix())
}
