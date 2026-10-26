package gardener

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

func writePlanNote(t *testing.T, mgr *files.Manager, slug, planSlug, title, status string, updated time.Time) core.Note {
	t.Helper()
	return writePlanNoteBody(t, mgr, slug, planSlug, title, status, "# "+title+"\n\nbody", updated)
}

func writePlanNoteBody(t *testing.T, mgr *files.Manager, slug, planSlug, title, status, body string, updated time.Time) core.Note {
	t.Helper()
	n, err := mgr.WriteNote(context.Background(), core.Note{
		ID: mustID(t), Slug: slug, Title: title, Project: "demo",
		Description: "captured plan", Body: body,
		Tags:    []string{"plan:" + planSlug, plans.TagPlan, "plan-status:" + status, "created-by:agent"},
		Created: updated, Updated: updated,
	})
	require.NoError(t, err)
	return n
}

func TestStalePlanProposeAndApply(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	defer func() { _ = mgr.Close() }()
	rec := events.NewRecorder(db)

	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	staleAt := now.AddDate(0, 0, -20)
	freshAt := now.AddDate(0, 0, -2)

	writePlanNote(t, mgr, "cc-plan-old", "old-plan", "Old Plan", plans.StatusPresented, staleAt)
	writePlanNote(t, mgr, "cc-plan-new", "new-plan", "New Plan", plans.StatusDraft, freshAt)
	writePlanNote(t, mgr, "cc-plan-done", "done-plan", "Done Plan", plans.StatusApproved, staleAt)

	g := New(db, mgr, nil, nil, rec, Config{StalePlanDays: 14}, slog.Default())
	g.now = func() time.Time { return now }

	seen, err := loadSeen(ctx, db)
	require.NoError(t, err)
	n, err := g.proposeStalePlans(ctx, seen)
	require.NoError(t, err)
	require.Equal(t, 1, n, "only the stale unapproved plan is proposed")

	props, err := store.PendingProposals(ctx, db, store.ProposalAbandonPlan)
	require.NoError(t, err)
	require.Len(t, props, 1)
	p := props[0]
	require.Equal(t, "old-plan", p.Payload["slug"])
	require.Equal(t, "cc-plan-old", p.Payload["note_slug"])
	require.Equal(t, "presented", p.Payload["plan_status"])

	// Apply retags the note abandoned.
	result, err := g.Apply(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "cc-plan-old", result["abandoned"])
	idx, ok, err := store.NoteBySlug(ctx, db, "demo", "cc-plan-old")
	require.NoError(t, err)
	require.True(t, ok)
	require.Contains(t, idx.Tags, "plan-status:abandoned")
	require.Contains(t, idx.Description, "abandoned")

	// A second pass proposes nothing new: the abandoned plan is settled and the
	// applied key stays known.
	seen, err = loadSeen(ctx, db)
	require.NoError(t, err)
	n, err = g.proposeStalePlans(ctx, seen)
	require.NoError(t, err)
	require.Zero(t, n)

	// StalePlanDays 0 disables the pass.
	g0 := New(db, mgr, nil, nil, rec, Config{}, slog.Default())
	g0.now = func() time.Time { return now }
	n, err = g0.proposeStalePlans(ctx, seenKeys{})
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestStalePlanShipEvidence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	defer func() { _ = mgr.Close() }()

	// A fake repo mapped to the notes' project: detached HEAD at the last
	// commit, the full history in the HEAD reflog.
	const (
		hashA = "aaaa000000000000000000000000000000000000"
		hashB = "bbbb000000000000000000000000000000000000"
		hashC = "cccc000000000000000000000000000000000000"
	)
	repo := t.TempDir()
	gitDir := filepath.Join(repo, ".git")
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "logs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte(hashC+"\n"), 0o644))
	reflog := "0000000000000000000000000000000000000000 " + hashA + " A <a@b.c> 1720000000 +0000\tcommit (initial): first\n" +
		hashA + " " + hashB + " A <a@b.c> 1720000100 +0000\tcommit: rework the gardener staleness pass for stale captures\n" +
		hashB + " " + hashC + " A <a@b.c> 1720000200 +0000\tcommit: bump the linter\n"
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "logs", "HEAD"), []byte(reflog), 0o644))
	require.NoError(t, store.AddRepoMapping(ctx, db, repo, "demo"))

	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	staleAt := now.AddDate(0, 0, -20)
	stampBody := func(stamp, title string) string {
		return "> captured from cc/ab12cd34 | x.md | iter 1 | git " + stamp +
			" | 2026-06-20T00:00:00Z\n\n# " + title + "\n\nbody"
	}
	writePlanNoteBody(t, mgr, "cc-plan-landed", "landed-plan", "Rework the gardener staleness pass",
		plans.StatusPresented, stampBody(hashA[:12], "Rework the gardener staleness pass"), staleAt)
	writePlanNoteBody(t, mgr, "cc-plan-unrelated", "unrelated-plan", "Console retrieval charts polish",
		plans.StatusPresented, stampBody(hashB[:12], "Console retrieval charts polish"), staleAt)
	writePlanNoteBody(t, mgr, "cc-plan-idle", "idle-plan", "Reshape briefing injection order",
		plans.StatusDraft, stampBody(hashC[:12], "Reshape briefing injection order"), staleAt)

	g := New(db, mgr, nil, nil, events.NewRecorder(db), Config{StalePlanDays: 14}, slog.Default())
	g.now = func() time.Time { return now }

	seen, err := loadSeen(ctx, db)
	require.NoError(t, err)
	n, err := g.proposeStalePlans(ctx, seen)
	require.NoError(t, err)
	require.Equal(t, 3, n, "every stale plan is settled one way or the other")

	// The plan the post-stamp commits match is proposed as shipped, evidence attached.
	ships, err := store.PendingProposals(ctx, db, store.ProposalShipPlan)
	require.NoError(t, err)
	require.Len(t, ships, 1)
	ship := ships[0]
	require.Equal(t, "landed-plan", ship.Payload["slug"])
	require.Equal(t, "cc-plan-landed", ship.Payload["note_slug"])
	require.EqualValues(t, 2, ship.Payload["commits_since"])
	require.EqualValues(t, 1, ship.Payload["matched_count"])
	require.Equal(t, []any{hashB[:12] + " commit: rework the gardener staleness pass for stale captures"},
		ship.Payload["commits"])
	require.Contains(t, ship.Payload["reason"], "appears to have shipped")

	// The others fall back to abandonment, with the checked evidence in the reason.
	abandons, err := store.PendingProposals(ctx, db, store.ProposalAbandonPlan)
	require.NoError(t, err)
	require.Len(t, abandons, 2)
	reasons := map[string]string{}
	for _, p := range abandons {
		reasons[p.Payload["note_slug"].(string)] = p.Payload["reason"].(string)
	}
	require.Contains(t, reasons["cc-plan-unrelated"], "1 commit(s) since the capture, none matching the plan")
	require.Contains(t, reasons["cc-plan-idle"], "repo unchanged since the capture")

	// Applying the ship retags the note shipped; the plan leaves the stale set.
	result, err := g.Apply(ctx, ship.ID)
	require.NoError(t, err)
	require.Equal(t, "cc-plan-landed", result["shipped"])
	idx, ok, err := store.NoteBySlug(ctx, db, "demo", "cc-plan-landed")
	require.NoError(t, err)
	require.True(t, ok)
	require.Contains(t, idx.Tags, "plan-status:shipped")
	require.Contains(t, idx.Description, "shipped")

	// A second pass proposes nothing new: shipped is settled, and the abandon
	// keys stay known.
	seen, err = loadSeen(ctx, db)
	require.NoError(t, err)
	n, err = g.proposeStalePlans(ctx, seen)
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestShipPlanApplyGuardsApprovedPlans(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	defer func() { _ = mgr.Close() }()

	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	note := writePlanNote(t, mgr, "cc-plan-raced", "raced-plan", "Raced Plan", plans.StatusPresented, now.AddDate(0, 0, -20))

	g := New(db, mgr, nil, nil, nil, Config{StalePlanDays: 14}, slog.Default())
	g.now = func() time.Time { return now }
	id, err := g.createProposal(ctx, store.ProposalShipPlan, "ship_plan:"+note.ID,
		map[string]any{"id": note.ID}, seenKeys{})
	require.NoError(t, err)

	// The plan gets approved after the proposal was raised: apply refuses.
	note.Tags = plans.SetStatusTag(note.Tags, plans.StatusApproved)
	_, err = mgr.WriteNote(ctx, note)
	require.NoError(t, err)
	_, err = g.Apply(ctx, id)
	require.ErrorContains(t, err, "approved since")
}

func TestAbandonPlanApplyGuardsApprovedPlans(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	defer func() { _ = mgr.Close() }()

	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	note := writePlanNote(t, mgr, "cc-plan-raced", "raced-plan", "Raced Plan", plans.StatusDraft, now.AddDate(0, 0, -20))

	g := New(db, mgr, nil, nil, nil, Config{StalePlanDays: 14}, slog.Default())
	g.now = func() time.Time { return now }
	seen := seenKeys{}
	n, err := g.proposeStalePlans(ctx, seen)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	props, err := store.PendingProposals(ctx, db, store.ProposalAbandonPlan)
	require.NoError(t, err)
	require.Len(t, props, 1)

	// The plan gets approved after the proposal was raised.
	note.Tags = plans.SetStatusTag(note.Tags, plans.StatusApproved)
	_, err = mgr.WriteNote(ctx, note)
	require.NoError(t, err)

	// Apply refuses and leaves the proposal pending.
	_, err = g.Apply(ctx, props[0].ID)
	require.ErrorContains(t, err, "approved since")
	p, ok, err := store.ProposalByID(ctx, db, props[0].ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, store.ProposalPending, p.Status)
}
