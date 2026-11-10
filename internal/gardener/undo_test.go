package gardener

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

func TestCanUndoApply_ExemptsOnlyTheIrreversibleKinds(t *testing.T) {
	for _, kind := range store.ProposalKinds {
		want := kind != store.ProposalConsolidate && kind != store.ProposalSplit
		require.Equal(t, want, CanUndoApply(kind), "kind %q", kind)
	}
}

// Undoing a rejection is pure bookkeeping: the proposal comes back pending and
// nothing in the world was touched either way. Both tiers reopen the same way
// -- a hide that could not be taken back would be a trap, not a decision.
func TestUndo_ReopensEitherRejectionTier(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reject func(*Service, context.Context, string) error
	}{
		{"dismiss", func(g *Service, ctx context.Context, id string) error { return g.Dismiss(ctx, id) }},
		{"hide", func(g *Service, ctx context.Context, id string) error { return g.Hide(ctx, id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _, cx := newApplyFixture(t)
			ctx := cx()

			p, err := store.CreateProposal(ctx, g.db, store.ProposalArchive, map[string]any{"key": "archive:x", "id": "GONE"})
			require.NoError(t, err)
			require.NoError(t, tc.reject(g, ctx, p.ID))

			require.NoError(t, g.Undo(ctx, p.ID))
			got, _, err := store.ProposalByID(ctx, g.db, p.ID)
			require.NoError(t, err)
			require.Equal(t, store.ProposalPending, got.Status)

			// A pending proposal has nothing to undo, so a double undo is refused.
			require.ErrorContains(t, g.Undo(ctx, p.ID), "still pending")
		})
	}
}

// The console reads undo availability off CanUndoResolution, so it has to hold
// for every (status, kind) pair: only an APPLIED consolidate or split is final.
func TestCanUndoResolution_OnlyAnAppliedKindCanBeFinal(t *testing.T) {
	for _, kind := range store.ProposalKinds {
		for _, status := range []string{store.ProposalDismissed, store.ProposalHidden} {
			require.True(t, CanUndoResolution(status, kind), "%s %s", status, kind)
		}
		require.Equal(t, CanUndoApply(kind), CanUndoResolution(store.ProposalApplied, kind), "applied %s", kind)
	}
}

func TestUndo_Archive_RestoresTheMemory(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	writeMem(t, mgr, "stale-one", "demo", "1", core.KindGotcha, now.AddDate(0, 0, -120), "the body")
	mem, _, err := store.MemoryByName(ctx, g.db, "demo", "stale-one")
	require.NoError(t, err)

	p, err := store.CreateProposal(ctx, g.db, store.ProposalArchive, map[string]any{
		"key": "archive:" + mem.ID, "id": mem.ID, "name": mem.Name, "project": "demo",
	})
	require.NoError(t, err)
	_, err = g.Apply(ctx, p.ID)
	require.NoError(t, err)
	_, found, err := store.MemoryByName(ctx, g.db, "demo", "stale-one")
	require.NoError(t, err)
	require.False(t, found, "the apply took it out of the active set")

	require.NoError(t, g.Undo(ctx, p.ID))
	restored, found, err := store.MemoryByName(ctx, g.db, "demo", "stale-one")
	require.NoError(t, err)
	require.True(t, found, "the undo brought it back")
	require.Nil(t, restored.InvalidAt)

	full, err := mgr.Store().ReadMemory(restored.FilePath)
	require.NoError(t, err)
	require.NotContains(t, full.Body, "Archived", "the tombstone went with it")

	got, _, err := store.ProposalByID(ctx, g.db, p.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalPending, got.Status, "the proposal is back in the queue")
}

func TestUndo_Merge_RestoresTheDroppedMemory(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	writeMem(t, mgr, "keep-me", "", "1", core.KindGotcha, now, "newer copy")
	writeMem(t, mgr, "drop-me", "", "1", core.KindGotcha, now.Add(-time.Hour), "older copy")
	keep, _, err := store.MemoryByName(ctx, g.db, "", "keep-me")
	require.NoError(t, err)
	drop, _, err := store.MemoryByName(ctx, g.db, "", "drop-me")
	require.NoError(t, err)

	p, err := store.CreateProposal(ctx, g.db, store.ProposalMerge, map[string]any{
		"key":  mergeKey(keep.ID, drop.ID),
		"keep": map[string]any{"id": keep.ID, "name": keep.Name},
		"drop": map[string]any{"id": drop.ID, "name": drop.Name},
	})
	require.NoError(t, err)
	_, err = g.Apply(ctx, p.ID)
	require.NoError(t, err)

	require.NoError(t, g.Undo(ctx, p.ID))
	restored, found, err := store.MemoryByName(ctx, g.db, "", "drop-me")
	require.NoError(t, err)
	require.True(t, found)
	require.Empty(t, restored.SupersededBy, "the supersession edge is gone")
	_, found, err = store.MemoryByName(ctx, g.db, "", "keep-me")
	require.NoError(t, err)
	require.True(t, found, "the kept memory was never touched")
}

func TestUndo_Rekind_WritesTheOriginalKindBack(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	writeMem(t, mgr, "wordmark-rule", "demo", "1", core.KindConstraint, now, "branding fact")
	mem, _, err := store.MemoryByName(ctx, g.db, "demo", "wordmark-rule")
	require.NoError(t, err)

	p, err := store.CreateProposal(ctx, g.db, store.ProposalRekind, map[string]any{
		"key": "rekind:" + mem.ID + ":convention", "id": mem.ID, "name": mem.Name,
		"project": "demo", "from": "constraint", "to": "convention",
	})
	require.NoError(t, err)
	_, err = g.Apply(ctx, p.ID)
	require.NoError(t, err)
	got, _, err := store.MemoryByName(ctx, g.db, "demo", "wordmark-rule")
	require.NoError(t, err)
	require.Equal(t, core.KindConvention, got.Kind)

	require.NoError(t, g.Undo(ctx, p.ID))
	got, _, err = store.MemoryByName(ctx, g.db, "demo", "wordmark-rule")
	require.NoError(t, err)
	require.Equal(t, core.KindConstraint, got.Kind)
}

// A kind that changed again since the apply is not this proposal's to revert.
func TestUndo_Rekind_RefusesAfterAThirdPartyChange(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	writeMem(t, mgr, "wordmark-rule", "demo", "1", core.KindConstraint, now, "branding fact")
	mem, _, err := store.MemoryByName(ctx, g.db, "demo", "wordmark-rule")
	require.NoError(t, err)
	p, err := store.CreateProposal(ctx, g.db, store.ProposalRekind, map[string]any{
		"key": "rekind:" + mem.ID + ":convention", "id": mem.ID, "name": mem.Name,
		"project": "demo", "from": "constraint", "to": "convention",
	})
	require.NoError(t, err)
	_, err = g.Apply(ctx, p.ID)
	require.NoError(t, err)

	// Someone reclassifies it again, by hand.
	idx, _, err := store.MemoryByName(ctx, g.db, "demo", "wordmark-rule")
	require.NoError(t, err)
	full, err := mgr.Store().ReadMemory(idx.FilePath)
	require.NoError(t, err)
	full.Kind = core.KindReference
	_, err = mgr.WriteMemory(ctx, full)
	require.NoError(t, err)

	require.ErrorContains(t, g.Undo(ctx, p.ID), "is now a reference")
	got, _, err := store.ProposalByID(ctx, g.db, p.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalApplied, got.Status, "a refused undo leaves the proposal resolved")
}

func TestUndo_Reproject_MovesTheMemoryBack(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	writeMem(t, mgr, "ios-thing", "example-app", "1", core.KindGotcha, now, "body")
	mem, _, err := store.MemoryByName(ctx, g.db, "example-app", "ios-thing")
	require.NoError(t, err)

	p, err := store.CreateProposal(ctx, g.db, store.ProposalReproject, map[string]any{
		"key": "reproject:" + mem.ID, "id": mem.ID, "name": mem.Name,
		"from": "example-app", "to": "example-ios",
	})
	require.NoError(t, err)
	_, err = g.Apply(ctx, p.ID)
	require.NoError(t, err)
	_, found, err := store.MemoryByName(ctx, g.db, "example-ios", "ios-thing")
	require.NoError(t, err)
	require.True(t, found)

	require.NoError(t, g.Undo(ctx, p.ID))
	back, found, err := store.MemoryByName(ctx, g.db, "example-app", "ios-thing")
	require.NoError(t, err)
	require.True(t, found, "the memory came home")
	require.Equal(t, mem.ID, back.ID, "same memory, same identity")
	_, found, err = store.MemoryByName(ctx, g.db, "example-ios", "ios-thing")
	require.NoError(t, err)
	require.False(t, found)
}

func TestUndo_Digest_DeletesTheNoteItWrote(t *testing.T) {
	g, _, cx := newApplyFixture(t)
	ctx := cx()

	p, err := store.CreateProposal(ctx, g.db, store.ProposalDigest, map[string]any{
		"key": "digest:demo:2026-07", "project": "demo", "month": "2026-07",
		"title": "demo digest 2026-07", "body": "## Findings\n- did the thing",
	})
	require.NoError(t, err)
	res, err := g.Apply(ctx, p.ID)
	require.NoError(t, err)
	noteID, _ := res["note_id"].(string)
	require.NotEmpty(t, noteID)

	// The result was persisted, which is what makes the note findable again.
	stored, _, err := store.ProposalByID(ctx, g.db, p.ID)
	require.NoError(t, err)
	require.Equal(t, noteID, stored.Result["note_id"])

	require.NoError(t, g.Undo(ctx, p.ID))
	_, ok, err := store.NoteByID(ctx, g.db, noteID)
	require.NoError(t, err)
	require.False(t, ok, "the digest note is gone")
}

func TestUndo_MemoryWanted_DropsTheTaskItOpened(t *testing.T) {
	g, _, cx := newApplyFixture(t)
	ctx := cx()

	p, err := store.CreateProposal(ctx, g.db, store.ProposalMemoryWanted, map[string]any{
		"key": "memory_wanted:demo:chroma", "project": "demo",
		"queries": []any{"chroma boot race"}, "miss_count": 3.0, "session_count": 2.0,
		"suggested_title": "chroma boot race",
	})
	require.NoError(t, err)
	res, err := g.Apply(ctx, p.ID)
	require.NoError(t, err)
	taskID, _ := res["task_id"].(string)
	require.NotEmpty(t, taskID)

	require.NoError(t, g.Undo(ctx, p.ID))
	task, err := store.TaskByID(ctx, g.db, taskID)
	require.NoError(t, err)
	require.Equal(t, core.TaskDropped, task.Status)

	open, err := store.ListTasks(ctx, g.db, "demo", core.TaskOpen)
	require.NoError(t, err)
	require.Empty(t, open)
}

// A task someone already started is not the undo's to close.
func TestUndo_ToolError_RefusesAPickedUpTask(t *testing.T) {
	g, _, cx := newApplyFixture(t)
	ctx := cx()

	p, err := store.CreateProposal(ctx, g.db, store.ProposalToolError, map[string]any{
		"key": "tool_error:demo:abcd", "project": "demo", "surface": "tool",
		"name": "tasks_add", "signature": "unknown parameter <v>",
		"examples":    []any{`tasks_add: unknown parameter "titel"`},
		"error_count": 3.0, "session_count": 2.0,
		"suggested_title": "tasks_add: unknown parameter <v>",
	})
	require.NoError(t, err)
	res, err := g.Apply(ctx, p.ID)
	require.NoError(t, err)
	taskID, _ := res["task_id"].(string)

	inProgress := core.TaskInProgress
	_, err = store.UpdateTask(ctx, g.db, taskID, store.TaskPatch{Status: &inProgress}, "someone", time.Now().UTC())
	require.NoError(t, err)

	require.ErrorContains(t, g.Undo(ctx, p.ID), "someone picked it up")
	got, _, err := store.ProposalByID(ctx, g.db, p.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalApplied, got.Status)
}

func TestUndo_ShipPlan_RestoresThePriorStatus(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	note := writePlanNote(t, mgr, "cc-plan-landed", "landed-plan", "Landed Plan", plans.StatusPresented, now)
	p, err := store.CreateProposal(ctx, g.db, store.ProposalShipPlan, map[string]any{
		"key": "ship_plan:" + note.ID, "id": note.ID, "slug": "landed-plan",
		"title": note.Title, "project": "demo", "plan_status": plans.StatusPresented,
	})
	require.NoError(t, err)
	_, err = g.Apply(ctx, p.ID)
	require.NoError(t, err)
	idx, ok, err := store.NoteBySlug(ctx, g.db, "demo", "cc-plan-landed")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, plans.StatusShipped, plans.StatusFromTags(idx.Tags))

	require.NoError(t, g.Undo(ctx, p.ID))
	idx, _, err = store.NoteBySlug(ctx, g.db, "demo", "cc-plan-landed")
	require.NoError(t, err)
	require.Equal(t, plans.StatusPresented, plans.StatusFromTags(idx.Tags), "the capture is presented again")
}

// The two coordinated kinds refuse an apply-undo outright rather than
// half-reverting a project family or a resolve-or-create consolidation.
func TestUndo_RefusesSplitAndConsolidateApplies(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	writeMem(t, mgr, "dfu-a", "demo", "1", core.KindGotcha, now, "part a")
	a, _, err := store.MemoryByName(ctx, g.db, "demo", "dfu-a")
	require.NoError(t, err)
	consolidate, err := store.CreateProposal(ctx, g.db, store.ProposalConsolidate, map[string]any{
		"key": "consolidate:" + a.ID, "name": "dfu-unified", "kind": "runbook",
		"project": "demo", "description": "unified", "body": "# DFU",
		"sources": []any{map[string]any{"id": a.ID, "name": a.Name}},
	})
	require.NoError(t, err)
	_, err = g.Apply(ctx, consolidate.ID)
	require.NoError(t, err)

	split, err := store.CreateProposal(ctx, g.db, store.ProposalSplit, map[string]any{
		"key": "split:demo", "source_project": "demo", "plan": "split-demo",
		"children": []any{map[string]any{"slug": "demo-ios", "label": "Demo iOS"}},
	})
	require.NoError(t, err)
	_, err = g.Apply(ctx, split.ID)
	require.NoError(t, err)

	for _, id := range []string{consolidate.ID, split.ID} {
		require.ErrorContains(t, g.Undo(ctx, id), "cannot be undone from here")
		got, _, gerr := store.ProposalByID(ctx, g.db, id)
		require.NoError(t, gerr)
		require.Equal(t, store.ProposalApplied, got.Status)
	}

	// Dismissing one of these is still undoable -- a dismissal had no effect.
	dismissed, err := store.CreateProposal(ctx, g.db, store.ProposalSplit, map[string]any{
		"key": "split:demo2", "source_project": "demo2",
	})
	require.NoError(t, err)
	require.NoError(t, g.Dismiss(ctx, dismissed.ID))
	require.NoError(t, g.Undo(ctx, dismissed.ID))
}
