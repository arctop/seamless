package gardener

import (
	"context"
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

func newApplyFixture(t *testing.T) (*Service, *files.Manager, func() context.Context) {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	g := New(db, mgr, nil, nil, events.NewRecorder(db), Config{}, slog.Default())
	return g, mgr, func() context.Context { return context.Background() }
}

func TestApplyMerge_SupersedesDropByKeep(t *testing.T) {
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

	res, err := g.Apply(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "applied", res["status"])

	// drop is superseded (leaves the active index); keep remains active.
	_, found, err := store.MemoryByName(ctx, g.db, "", "drop-me")
	require.NoError(t, err)
	require.False(t, found)
	_, found, err = store.MemoryByName(ctx, g.db, "", "keep-me")
	require.NoError(t, err)
	require.True(t, found)

	// The dropped memory records its successor.
	dropIdx, _, err := store.MemoryByNameIncludingInvalid(ctx, g.db, "", "drop-me")
	require.NoError(t, err)
	require.Equal(t, keep.ID, dropIdx.SupersededBy)
}

func TestApplyConsolidate_WritesUnifiedAndSupersedesSources(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	writeMem(t, mgr, "dfu-a", "seamless", "1", core.KindGotcha, now, "part a")
	writeMem(t, mgr, "dfu-b", "seamless", "2", core.KindGotcha, now.Add(-time.Hour), "part b")
	a, _, err := store.MemoryByName(ctx, g.db, "seamless", "dfu-a")
	require.NoError(t, err)
	b, _, err := store.MemoryByName(ctx, g.db, "seamless", "dfu-b")
	require.NoError(t, err)

	p, err := store.CreateProposal(ctx, g.db, store.ProposalConsolidate, map[string]any{
		"key": "consolidate:" + a.ID + "|" + b.ID, "name": "dfu-unified", "kind": "runbook",
		"project": "seamless", "description": "the unified DFU flow", "body": "# DFU\ncombined steps",
		"sources": []any{
			map[string]any{"id": a.ID, "name": a.Name},
			map[string]any{"id": b.ID, "name": b.Name},
		},
	})
	require.NoError(t, err)

	res, err := g.Apply(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "applied", res["status"])

	// The new unified memory is active with the requested kind.
	unified, found, err := store.MemoryByName(ctx, g.db, "seamless", "dfu-unified")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, core.MemoryKind("runbook"), unified.Kind)

	// Both sources are superseded by it (leave the active index, point at unified).
	for _, name := range []string{"dfu-a", "dfu-b"} {
		_, active, err := store.MemoryByName(ctx, g.db, "seamless", name)
		require.NoError(t, err)
		require.False(t, active, name+" should be superseded")
		idx, _, err := store.MemoryByNameIncludingInvalid(ctx, g.db, "seamless", name)
		require.NoError(t, err)
		require.Equal(t, unified.ID, idx.SupersededBy)
	}
}

// TestApplyConsolidate_RetryIsIdempotent drives applyConsolidate twice (as a
// retry after Apply left the proposal pending on a partial failure would): it
// must converge -- reuse the already-written unified memory and skip
// already-superseded sources -- not write a second copy.
func TestApplyConsolidate_RetryIsIdempotent(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	writeMem(t, mgr, "src-a", "", "1", core.KindGotcha, now, "a")
	writeMem(t, mgr, "src-b", "", "2", core.KindGotcha, now, "b")
	a, _, err := store.MemoryByName(ctx, g.db, "", "src-a")
	require.NoError(t, err)
	b, _, err := store.MemoryByName(ctx, g.db, "", "src-b")
	require.NoError(t, err)

	p := store.Proposal{Payload: map[string]any{
		"name": "unified", "kind": "reference", "project": "", "description": "u", "body": "combined",
		"sources": []any{
			map[string]any{"id": a.ID, "name": "src-a"},
			map[string]any{"id": b.ID, "name": "src-b"},
		},
	}}

	_, err = g.applyConsolidate(ctx, p, now)
	require.NoError(t, err)
	_, err = g.applyConsolidate(ctx, p, now.Add(time.Minute))
	require.NoError(t, err, "a second apply converges rather than erroring")

	// Exactly one active "unified" memory exists -- no duplicate from the retry.
	all, err := store.AllActiveMemories(ctx, g.db)
	require.NoError(t, err)
	n := 0
	for _, m := range all {
		if m.Name == "unified" {
			n++
		}
	}
	require.Equal(t, 1, n, "retry must not create a duplicate unified memory")
}

func TestApplyReproject_MovesMemoryToNewProject(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	writeMem(t, mgr, "ios-dfu", "arctop-app", "1", core.KindRunbook, now, "the iOS DFU flow")
	mem, _, err := store.MemoryByName(ctx, g.db, "arctop-app", "ios-dfu")
	require.NoError(t, err)

	p, err := store.CreateProposal(ctx, g.db, store.ProposalReproject, map[string]any{
		"key": "reproject:" + mem.ID, "id": mem.ID, "name": "ios-dfu",
		"from": "arctop-app", "to": "arctop-ios", "rationale": "iOS-specific",
	})
	require.NoError(t, err)

	res, err := g.Apply(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "applied", res["status"])

	// The memory now lives in arctop-ios (same id), and is gone from arctop-app.
	moved, found, err := store.MemoryByName(ctx, g.db, "arctop-ios", "ios-dfu")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, mem.ID, moved.ID, "reproject keeps the ULID")
	require.True(t, moved.Active(), "a move is a relocation, not an invalidation")
	_, found, err = store.MemoryByName(ctx, g.db, "arctop-app", "ios-dfu")
	require.NoError(t, err)
	require.False(t, found, "the memory left the source project")

	// The file moved on disk under the new project directory.
	require.NoError(t, err)
	idx, _, err := store.MemoryByID(ctx, g.db, mem.ID)
	require.NoError(t, err)
	require.Equal(t, "memory/arctop-ios/ios-dfu.md", idx.FilePath)
}

// TestApplyReproject_RetryIsIdempotent drives applyReproject twice: a memory
// already at the target is a success (no-op), so a retry after a partial apply
// converges rather than erroring.
func TestApplyReproject_RetryIsIdempotent(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	writeMem(t, mgr, "thing", "arctop-app", "1", core.KindGotcha, now, "x")
	mem, _, err := store.MemoryByName(ctx, g.db, "arctop-app", "thing")
	require.NoError(t, err)
	p := store.Proposal{Payload: map[string]any{"id": mem.ID, "name": "thing", "from": "arctop-app", "to": "arctop-ios"}}

	_, err = g.applyReproject(ctx, p, now)
	require.NoError(t, err)
	res, err := g.applyReproject(ctx, p, now.Add(time.Minute))
	require.NoError(t, err, "a second apply converges rather than erroring")
	require.Equal(t, true, res["noop"], "the retry is a no-op")

	all, err := store.AllActiveMemories(ctx, g.db)
	require.NoError(t, err)
	n := 0
	for _, m := range all {
		if m.Name == "thing" {
			n++
		}
	}
	require.Equal(t, 1, n, "retry must not duplicate the memory")
}

func TestApplyReproject_NameClashInTargetErrors(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	writeMem(t, mgr, "dupe", "arctop-app", "1", core.KindGotcha, now, "source copy")
	writeMem(t, mgr, "dupe", "arctop-ios", "2", core.KindGotcha, now, "target already has this name")
	mem, _, err := store.MemoryByName(ctx, g.db, "arctop-app", "dupe")
	require.NoError(t, err)

	p, err := store.CreateProposal(ctx, g.db, store.ProposalReproject, map[string]any{
		"key": "reproject:" + mem.ID, "id": mem.ID, "name": "dupe", "from": "arctop-app", "to": "arctop-ios",
	})
	require.NoError(t, err)

	_, err = g.Apply(ctx, p.ID)
	require.Error(t, err, "a name clash in the target project blocks the move")
	got, _, err := store.ProposalByID(ctx, g.db, p.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalPending, got.Status, "the proposal stays pending for the owner to resolve")
}

func TestApplyRekind_ChangesKindInPlace(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	writeMem(t, mgr, "wordmark-rule", "seamless", "1", core.KindConstraint, now.Add(-time.Hour), "branding fact")
	mem, _, err := store.MemoryByName(ctx, g.db, "seamless", "wordmark-rule")
	require.NoError(t, err)

	p, err := store.CreateProposal(ctx, g.db, store.ProposalRekind, map[string]any{
		"key": "rekind:" + mem.ID + ":convention", "id": mem.ID, "name": "wordmark-rule",
		"project": "seamless", "from": "constraint", "to": "convention", "rationale": "project-local branding",
	})
	require.NoError(t, err)

	res, err := g.Apply(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "applied", res["status"])
	require.Equal(t, "constraint", res["from"])
	require.Equal(t, "convention", res["to"])

	// Same identity and project, new kind, bumped updated stamp.
	got, found, err := store.MemoryByName(ctx, g.db, "seamless", "wordmark-rule")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, mem.ID, got.ID, "rekind keeps the ULID")
	require.Equal(t, core.KindConvention, got.Kind)
	require.True(t, got.Active(), "a rekind is a reclassification, not an invalidation")
	require.True(t, got.Updated.After(mem.Updated), "the rewrite bumps updated")

	// The file on disk carries the new kind too (files are the source of truth).
	idx, _, err := store.MemoryByID(ctx, g.db, mem.ID)
	require.NoError(t, err)
	onDisk, err := mgr.Store().ReadMemory(idx.FilePath)
	require.NoError(t, err)
	require.Equal(t, core.KindConvention, onDisk.Kind)
}

// TestApplyRekind_RetryIsIdempotent drives applyRekind twice: a memory already
// at the target kind is a no-op success, so a retry after a partial apply
// converges rather than erroring.
func TestApplyRekind_RetryIsIdempotent(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	writeMem(t, mgr, "thing", "seamless", "1", core.KindConstraint, now, "x")
	mem, _, err := store.MemoryByName(ctx, g.db, "seamless", "thing")
	require.NoError(t, err)
	p := store.Proposal{Payload: map[string]any{"id": mem.ID, "name": "thing", "from": "constraint", "to": "convention"}}

	_, err = g.applyRekind(ctx, p, now)
	require.NoError(t, err)
	res, err := g.applyRekind(ctx, p, now.Add(time.Minute))
	require.NoError(t, err, "a second apply converges rather than erroring")
	require.Equal(t, true, res["noop"], "the retry is a no-op")
}

func TestApplyRekind_UnknownKindStaysPending(t *testing.T) {
	g, mgr, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	writeMem(t, mgr, "thing", "seamless", "1", core.KindConstraint, now, "x")
	mem, _, err := store.MemoryByName(ctx, g.db, "seamless", "thing")
	require.NoError(t, err)

	p, err := store.CreateProposal(ctx, g.db, store.ProposalRekind, map[string]any{
		"key": "rekind:" + mem.ID + ":no-such-kind", "id": mem.ID, "name": "thing",
		"from": "constraint", "to": "no-such-kind",
	})
	require.NoError(t, err)

	_, err = g.Apply(ctx, p.ID)
	require.Error(t, err, "an unknown target kind blocks the apply")
	got, _, err := store.ProposalByID(ctx, g.db, p.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalPending, got.Status, "the proposal stays pending for the owner to resolve")

	unchanged, _, err := store.MemoryByName(ctx, g.db, "seamless", "thing")
	require.NoError(t, err)
	require.Equal(t, core.KindConstraint, unchanged.Kind, "the memory is untouched")
}

func TestApplySplit_CreatesProjectsFamilyAndParents(t *testing.T) {
	g, _, cx := newApplyFixture(t)
	ctx := cx()
	now := time.Now().UTC()

	// The source needs a projects row for retirement to have a target.
	_, err := store.EnsureProject(ctx, g.db, "arctop-app", "Arctop App")
	require.NoError(t, err)

	p, err := store.CreateProposal(ctx, g.db, store.ProposalSplit, map[string]any{
		"key": "split:arctop-app", "source_project": "arctop-app", "retire_source": true,
		"family": "arctop-mobile-apps", "plan": "split-arctop-app",
		"children": []any{
			map[string]any{"slug": "arctop-ios", "label": "Arctop iOS"},
			map[string]any{"slug": "arctop-android", "label": "Arctop Android"},
		},
		"shared": map[string]any{"slug": "arctop-mobile-apps", "label": "Arctop Mobile"},
	})
	require.NoError(t, err)

	res, err := g.Apply(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "applied", res["status"])

	// All three projects exist; children point at the shared parent.
	for _, slug := range []string{"arctop-ios", "arctop-android", "arctop-mobile-apps"} {
		_, found, err := store.ProjectBySlug(ctx, g.db, slug)
		require.NoError(t, err)
		require.True(t, found, slug+" should have been created")
	}
	for _, child := range []string{"arctop-ios", "arctop-android"} {
		pr, _, err := store.ProjectBySlug(ctx, g.db, child)
		require.NoError(t, err)
		require.Equal(t, "arctop-mobile-apps", pr.ParentSlug, child+" is parented to the shared project")
	}

	// The three are linked as a family (siblings surface each other's findings).
	sibs, err := store.SiblingProjects(ctx, g.db, "arctop-ios")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"arctop-android", "arctop-mobile-apps"}, sibs)

	// The source is retired (flagged, not deleted).
	src, found, err := store.ProjectBySlug(ctx, g.db, "arctop-app")
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, src.Retired(), "the emptied source is retired")

	// A second apply of the same setup converges (all steps are idempotent).
	_, err = g.applySplit(ctx, p, now.Add(time.Minute))
	require.NoError(t, err, "re-running the split setup is idempotent")
}

func TestApplyDigest_WritesNote(t *testing.T) {
	g, _, cx := newApplyFixture(t)
	ctx := cx()

	p, err := store.CreateProposal(ctx, g.db, store.ProposalDigest, map[string]any{
		"key": "digest::2026-07", "project": "", "title": "Session digest -- 2026-07",
		"body": "## July\n- shipped the gardener",
	})
	require.NoError(t, err)

	res, err := g.Apply(ctx, p.ID)
	require.NoError(t, err)
	noteID, _ := res["note_id"].(string)
	require.NotEmpty(t, noteID)

	note, ok, err := store.NoteByID(ctx, g.db, noteID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "Session digest -- 2026-07", note.Title)
}

func TestApplyDismiss(t *testing.T) {
	g, _, cx := newApplyFixture(t)
	ctx := cx()

	p, err := store.CreateProposal(ctx, g.db, store.ProposalArchive, map[string]any{
		"key": "archive:gone", "id": "gone", "name": "gone",
	})
	require.NoError(t, err)

	require.NoError(t, g.Dismiss(ctx, p.ID))
	got, ok, err := store.ProposalByID(ctx, g.db, p.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, store.ProposalDismissed, got.Status)

	// Applying a proposal whose target memory does not exist errors and leaves it
	// pending (here the proposal is already dismissed, so this also covers the
	// "not pending" guard).
	_, err = g.Apply(ctx, p.ID)
	require.Error(t, err)
}

func TestApplyArchive_MissingMemoryErrors(t *testing.T) {
	g, _, cx := newApplyFixture(t)
	ctx := cx()
	p, err := store.CreateProposal(ctx, g.db, store.ProposalArchive, map[string]any{
		"key": "archive:ghost", "id": "ghost", "name": "ghost",
	})
	require.NoError(t, err)

	_, err = g.Apply(ctx, p.ID)
	require.Error(t, err)
	// The proposal stays pending so the owner can dismiss it.
	got, _, err := store.ProposalByID(ctx, g.db, p.ID)
	require.NoError(t, err)
	require.Equal(t, store.ProposalPending, got.Status)
}
