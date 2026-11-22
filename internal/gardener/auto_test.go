package gardener

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// aaFixture is a gardener over a fresh store with a pinned clock, cfg as its
// file/env config, and one knowledge gap seeded: two sessions in example-ops
// searched for the same thing and found nothing, so the next pass raises one
// memory_wanted proposal.
func aaFixture(t *testing.T, cfg Config) (*Service, *events.Recorder) {
	t.Helper()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	rec := events.NewRecorder(db)
	g := New(db, mgr, nil, nil, rec, cfg, slog.Default())
	g.now = func() time.Time { return now }
	recordMiss(t, rec, now.Add(-48*time.Hour), "S1", "example-ops", "zeta protocol handshake")
	recordMiss(t, rec, now.Add(-24*time.Hour), "S2", "example-ops", "handshake zeta protocol")
	return g, rec
}

// aaWanted is the one memory_wanted proposal the fixture's pass raised.
func aaWanted(t *testing.T, g *Service) store.Proposal {
	t.Helper()
	ctx := context.Background()
	pending, err := store.PendingProposals(ctx, g.db, store.ProposalMemoryWanted)
	require.NoError(t, err)
	resolved, err := store.ResolvedProposals(ctx, g.db, 0, 100)
	require.NoError(t, err)
	all := pending
	for _, p := range resolved {
		if p.Kind == store.ProposalMemoryWanted {
			all = append(all, p)
		}
	}
	require.Len(t, all, 1)
	return all[0]
}

// aaActions lists the gardener.action events about id: action, by, reason.
func aaActions(t *testing.T, rec *events.Recorder, id string) []string {
	t.Helper()
	evs, err := rec.ByKinds(context.Background(), []core.EventKind{core.EventGardenerAction}, "", "", 100)
	require.NoError(t, err)
	var out []string
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		if e.ItemID != id {
			continue
		}
		line := payloadString(e.Payload, "action") + " by=" + payloadString(e.Payload, "by")
		if r := payloadString(e.Payload, "reason"); r != "" {
			line += " reason=" + r
		}
		out = append(out, line)
	}
	return out
}

// With auto_apply empty -- the default -- a pass behaves exactly as before:
// the proposal waits, nothing opens a task, no switch is consulted.
func TestAaAutoApply_OffChangesNothing(t *testing.T) {
	g, rec := aaFixture(t, Config{})
	res, err := g.Propose(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, res.MemoryWanted)
	require.Zero(t, res.AutoApplied)
	require.Zero(t, res.KeptForReview)
	p := aaWanted(t, g)
	require.Equal(t, store.ProposalPending, p.Status)
	require.Equal(t, []string{"propose by="}, aaActions(t, rec, p.ID))
	tasks, err := store.ListTasks(context.Background(), g.db, "example-ops", core.TaskOpen)
	require.NoError(t, err)
	require.Empty(t, tasks)
}

// With memory_wanted enabled in the file, the new proposal is applied by the
// pass, recorded as auto on the proposal and its event, and opens its task;
// the owner's Undo drops that task and returns the proposal to the queue,
// where no later pass applies it by itself again.
func TestAaAutoApply_MemoryWantedAppliedAndUndone(t *testing.T) {
	g, rec := aaFixture(t, Config{AutoApply: []string{store.ProposalMemoryWanted}})
	ctx := context.Background()
	res, err := g.Propose(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, res.AutoApplied)
	p := aaWanted(t, g)
	require.Equal(t, store.ProposalApplied, p.Status)
	require.Equal(t, ByAuto, p.ResolvedBy)
	require.Equal(t, []string{"propose by=", "apply by=auto"}, aaActions(t, rec, p.ID))
	taskID := payloadString(p.Result, "task_id")
	require.NotEmpty(t, taskID)
	task, err := store.TaskByID(ctx, g.db, taskID)
	require.NoError(t, err)
	require.Equal(t, core.TaskOpen, task.Status)
	require.Equal(t, "Write a memory: handshake zeta protocol", task.Title)

	require.NoError(t, g.Undo(WithActor(ctx, ByOwner), p.ID))
	task, err = store.TaskByID(ctx, g.db, taskID)
	require.NoError(t, err)
	require.Equal(t, core.TaskDropped, task.Status, "Undo removes the task the automatic apply opened")
	require.Equal(t, store.ProposalPending, aaWanted(t, g).Status)

	res, err = g.Propose(ctx)
	require.NoError(t, err)
	require.Zero(t, res.AutoApplied, "an undone proposal is the owner's now")
	require.Equal(t, store.ProposalPending, aaWanted(t, g).Status)
}

// The console's stored choice applies from the next pass, with no restart;
// a kind it does not name waits.
func TestAaAutoApply_ConsoleChoiceAppliesAtTheNextPass(t *testing.T) {
	g, _ := aaFixture(t, Config{AutoApply: []string{store.ProposalMemoryWanted}})
	ctx := context.Background()
	require.NoError(t, store.SetGardenerOverride(ctx, g.db, config.GardenerOverride{AutoApply: &[]string{store.ProposalToolError}}))
	res, err := g.Propose(ctx)
	require.NoError(t, err)
	require.Zero(t, res.AutoApplied, "the console's choice wins over the file's")
	require.Equal(t, store.ProposalPending, aaWanted(t, g).Status)
}

func TestAaAutoApply_ConsoleChoiceTurnsItOn(t *testing.T) {
	g, _ := aaFixture(t, Config{})
	ctx := context.Background()
	require.NoError(t, store.SetGardenerOverride(ctx, g.db, config.GardenerOverride{AutoApply: &[]string{store.ProposalMemoryWanted}}))
	res, err := g.Propose(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, res.AutoApplied)
	require.Equal(t, ByAuto, aaWanted(t, g).ResolvedBy)
}

// Each guard leaves the new proposal pending for the owner, recorded with its
// reason code; nothing is applied and no task opens.
func TestAaAutoApply_KeptForReview(t *testing.T) {
	tests := []struct {
		name   string
		guard  func(t *testing.T) AutoGuard
		before func(t *testing.T, g *Service)
		reason string
	}{
		{name: "lockdown is on", reason: AutoKeptLockdown,
			guard: func(*testing.T) AutoGuard { return AutoGuard{Lockdown: func(context.Context) bool { return true }} }},
		{name: "screening would hold the task text", reason: AutoKeptScreening,
			guard: func(t *testing.T) AutoGuard {
				return AutoGuard{Holds: func(_ context.Context, text string) bool {
					require.Contains(t, text, "zeta protocol handshake", "screening reads the queries the task would carry")
					return true
				}}
			}},
		{name: "the project is locked", reason: AutoKeptLocked,
			guard: func(*testing.T) AutoGuard {
				return AutoGuard{Locked: func(project string) bool { return project == "example-ops" }}
			}},
		{name: "the project is fenced from the daemon's read", reason: AutoKeptFenced,
			before: func(t *testing.T, g *Service) {
				ctx := context.Background()
				_, err := store.EnsureProject(ctx, g.db, "example-ops", "example-ops")
				require.NoError(t, err)
				require.NoError(t, store.SetProjectIsolation(ctx, g.db, "example-ops", core.IsolationConfidential))
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g, rec := aaFixture(t, Config{AutoApply: []string{store.ProposalMemoryWanted}})
			if tt.guard != nil {
				g.SetAutoGuard(tt.guard(t))
			}
			if tt.before != nil {
				tt.before(t, g)
			}
			res, err := g.Propose(context.Background())
			require.NoError(t, err)
			require.Zero(t, res.AutoApplied)
			require.Equal(t, 1, res.KeptForReview)
			p := aaWanted(t, g)
			require.Equal(t, store.ProposalPending, p.Status)
			require.Equal(t, []string{"propose by=", "kept_for_review by=auto reason=" + tt.reason}, aaActions(t, rec, p.ID))
			tasks, err := store.ListTasks(context.Background(), g.db, "example-ops", core.TaskOpen)
			require.NoError(t, err)
			require.Empty(t, tasks)
		})
	}
}

// Only a pass's own new proposals apply by themselves: one already waiting
// when the owner turned automatic apply on stays for the owner.
func TestAaAutoApply_DoesNotSweepTheQueue(t *testing.T) {
	g, _ := aaFixture(t, Config{})
	ctx := context.Background()
	_, err := g.Propose(ctx)
	require.NoError(t, err)
	require.Equal(t, store.ProposalPending, aaWanted(t, g).Status)

	require.NoError(t, store.SetGardenerOverride(ctx, g.db, config.GardenerOverride{AutoApply: &[]string{store.ProposalMemoryWanted}}))
	res, err := g.Propose(ctx)
	require.NoError(t, err)
	require.Zero(t, res.AutoApplied)
	require.Equal(t, store.ProposalPending, aaWanted(t, g).Status)
}

// A request's proposals are never applied by themselves, whatever the switch
// says: the log is the pass's alone.
func TestAaAutoApply_TheLogIsThePassAlone(t *testing.T) {
	ctx, log := withCreatedLog(context.Background())
	noteCreated(ctx, "01A", store.ProposalMemoryWanted)
	noteCreated(context.Background(), "01B", store.ProposalMemoryWanted)
	require.Equal(t, []createdProposal{{id: "01A", kind: store.ProposalMemoryWanted}}, log.list())
	require.False(t, anyAutoSafe([]createdProposal{{kind: store.ProposalMerge}}))
	require.True(t, anyAutoSafe([]createdProposal{{kind: store.ProposalMerge}, {kind: store.ProposalShipPlan}}))
	require.True(t, strings.Contains(autoLandedText(store.Proposal{Kind: store.ProposalToolError,
		Payload: map[string]any{"suggested_title": "disk full", "surface": "tool", "name": "memory_write"}}), "disk full"))
	require.Empty(t, autoLandedText(store.Proposal{Kind: store.ProposalShipPlan}), "a retag lands no text")
}
