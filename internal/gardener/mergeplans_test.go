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
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// mergeEnv is one gardener wired to a throwaway DB and files manager, plus the
// fixed clock the stale-plan window is measured against.
type mergeEnv struct {
	g   *Service
	mgr *files.Manager
	now time.Time
}

func newMergeEnv(t *testing.T) *mergeEnv {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })

	now := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	g := New(db, mgr, nil, nil, events.NewRecorder(db), Config{StalePlanDays: 14}, slog.Default())
	g.now = func() time.Time { return now }
	return &mergeEnv{g: g, mgr: mgr, now: now}
}

// composedPlan writes the narrative note of an agent-composed plan (no cc-plan
// tag: it never came through the capture hook) plus one step task, which is
// what makes it a merge TARGET rather than another stranded capture.
func composedPlan(t *testing.T, e *mergeEnv, slug, title string) {
	t.Helper()
	ctx := context.Background()
	_, err := e.mgr.WriteNote(ctx, core.Note{
		ID: mustID(t), Slug: "plan-" + slug, Title: title, Project: "demo",
		Description: "composed plan", Body: "# " + title + "\n\nbody",
		Tags:    []string{"plan:" + slug, "created-by:agent"},
		Created: e.now, Updated: e.now,
	})
	require.NoError(t, err)
	require.NoError(t, store.CreateTask(ctx, e.g.db, core.Task{
		ID: mustID(t), ProjectSlug: "demo", Title: "Step one", Status: core.TaskOpen,
		PlanSlug: slug, CreatedAt: e.now, UpdatedAt: e.now,
	}))
}

// A stranded capture beside a composition holding the steps for the same work
// settles as a merge, not as an abandon: the merge is the more specific answer
// and the only one that says where the work went.
func TestStalePlanProposesMergeOverAbandon(t *testing.T) {
	ctx := context.Background()
	e := newMergeEnv(t)
	stale := e.now.AddDate(0, 0, -20)

	capture := writePlanNote(t, e.mgr, "cc-plan-organic", "organic-search-ai-search-plan",
		"Organic search and AI-search visibility for thereisnospoon", plans.StatusPresented, stale)
	agentNote, err := e.mgr.WriteNote(ctx, core.Note{
		ID: mustID(t), Slug: "cc-agent-a1", Title: "[Explore] search surfaces", Project: "demo",
		Description: "cached subagent run", Body: "report",
		Tags:    []string{"plan:organic-search-ai-search-plan", plans.TagAgent, "created-by:agent"},
		Created: stale, Updated: stale,
	})
	require.NoError(t, err)
	composedPlan(t, e, "search-visibility",
		"Organic search and AI-search visibility for thereisnospoon")

	seen, err := loadSeen(ctx, e.g.db)
	require.NoError(t, err)
	n, err := e.g.proposeStalePlans(ctx, seen)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	require.Empty(t, pending(t, e, store.ProposalAbandonPlan), "merge outranks abandon")
	props := pending(t, e, store.ProposalMergePlans)
	require.Len(t, props, 1)
	p := props[0]
	require.Equal(t, "organic-search-ai-search-plan", p.Payload["slug"])
	require.Equal(t, "search-visibility", p.Payload["merge_into"])

	// Applying moves the whole composition, capture and agent cache alike.
	result, err := e.g.Apply(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "search-visibility", result["into"])
	require.Equal(t, plans.StatusPresented, result["prior_status"])

	settled := reload(t, e, capture.Slug)
	require.Equal(t, "search-visibility", plans.SlugFromTags(settled.Tags))
	require.Equal(t, plans.StatusMerged, plans.StatusFromTags(settled.Tags))
	require.Contains(t, settled.Description, "merged")
	require.Equal(t, "search-visibility", plans.SlugFromTags(reload(t, e, agentNote.Slug).Tags),
		"supporting notes follow the capture so the target inherits the narrative")

	// Undo returns every note and the capture's own status.
	require.NoError(t, e.g.Undo(ctx, p.ID))
	back := reload(t, e, capture.Slug)
	require.Equal(t, "organic-search-ai-search-plan", plans.SlugFromTags(back.Tags))
	require.Equal(t, plans.StatusPresented, plans.StatusFromTags(back.Tags))
	require.Equal(t, "organic-search-ai-search-plan", plans.SlugFromTags(reload(t, e, agentNote.Slug).Tags))
}

// The bar to move a composition is high on purpose. Each of these is a reason
// to leave a duplicate standing rather than invent a relationship.
func TestMergeTargetDeclines(t *testing.T) {
	stale := func(e *mergeEnv) time.Time { return e.now.AddDate(0, 0, -20) }

	t.Run("capture owning steps is a working plan", func(t *testing.T) {
		ctx := context.Background()
		e := newMergeEnv(t)
		n := writePlanNote(t, e.mgr, "cc-plan-a", "alpha-beta-gamma-delta",
			"Alpha beta gamma delta", plans.StatusPresented, stale(e))
		require.NoError(t, store.CreateTask(ctx, e.g.db, core.Task{
			ID: mustID(t), ProjectSlug: "demo", Title: "own step", Status: core.TaskOpen,
			PlanSlug: "alpha-beta-gamma-delta", CreatedAt: e.now, UpdatedAt: e.now,
		}))
		composedPlan(t, e, "other", "Alpha beta gamma delta")
		require.False(t, e.g.mergeTarget(ctx, n, "alpha-beta-gamma-delta").found())
	})

	t.Run("target without steps is another stranded plan", func(t *testing.T) {
		ctx := context.Background()
		e := newMergeEnv(t)
		n := writePlanNote(t, e.mgr, "cc-plan-a", "alpha-beta-gamma-delta",
			"Alpha beta gamma delta", plans.StatusPresented, stale(e))
		_, err := e.mgr.WriteNote(ctx, core.Note{
			ID: mustID(t), Slug: "plan-other", Title: "Alpha beta gamma delta", Project: "demo",
			Description: "composed plan", Body: "body",
			Tags: []string{"plan:other", "created-by:agent"}, Created: e.now, Updated: e.now,
		})
		require.NoError(t, err)
		require.False(t, e.g.mergeTarget(ctx, n, "alpha-beta-gamma-delta").found())
	})

	t.Run("too few shared title words", func(t *testing.T) {
		ctx := context.Background()
		e := newMergeEnv(t)
		n := writePlanNote(t, e.mgr, "cc-plan-a", "alpha-beta-gamma-delta",
			"Alpha beta gamma delta", plans.StatusPresented, stale(e))
		composedPlan(t, e, "other", "Alpha beta something entirely different")
		require.False(t, e.g.mergeTarget(ctx, n, "alpha-beta-gamma-delta").found())
	})

	t.Run("tied candidates are ambiguity, not a choice", func(t *testing.T) {
		ctx := context.Background()
		e := newMergeEnv(t)
		n := writePlanNote(t, e.mgr, "cc-plan-a", "alpha-beta-gamma-delta",
			"Alpha beta gamma delta", plans.StatusPresented, stale(e))
		composedPlan(t, e, "first", "Alpha beta gamma delta")
		composedPlan(t, e, "second", "Alpha beta gamma delta")
		require.False(t, e.g.mergeTarget(ctx, n, "alpha-beta-gamma-delta").found())
	})
}

// An approval landing between propose and apply makes the capture a real plan
// of its own, so the merge refuses -- and refuses before it has moved anything.
func TestApplyMergePlansRefusesApprovedSince(t *testing.T) {
	ctx := context.Background()
	e := newMergeEnv(t)
	stale := e.now.AddDate(0, 0, -20)

	capture := writePlanNote(t, e.mgr, "cc-plan-organic", "organic-search-ai-search-plan",
		"Organic search and AI-search visibility for thereisnospoon", plans.StatusPresented, stale)
	agentNote, err := e.mgr.WriteNote(ctx, core.Note{
		ID: mustID(t), Slug: "cc-agent-a1", Title: "[Explore] search surfaces", Project: "demo",
		Description: "cached subagent run", Body: "report",
		Tags:    []string{"plan:organic-search-ai-search-plan", plans.TagAgent, "created-by:agent"},
		Created: stale, Updated: stale,
	})
	require.NoError(t, err)
	composedPlan(t, e, "search-visibility",
		"Organic search and AI-search visibility for thereisnospoon")

	seen, err := loadSeen(ctx, e.g.db)
	require.NoError(t, err)
	_, err = e.g.proposeStalePlans(ctx, seen)
	require.NoError(t, err)
	p := pending(t, e, store.ProposalMergePlans)[0]

	// The owner approves it in the console before deciding the proposal.
	idx, ok, err := store.NoteBySlug(ctx, e.g.db, "demo", capture.Slug)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = e.mgr.MutateNote(ctx, idx.FilePath, func(_ context.Context, note core.Note) (core.Note, error) {
		note.Tags = plans.SetStatusTag(note.Tags, plans.StatusApproved)
		return note, nil
	})
	require.NoError(t, err)

	_, err = e.g.Apply(ctx, p.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "approved")
	require.Equal(t, "organic-search-ai-search-plan",
		plans.SlugFromTags(reload(t, e, agentNote.Slug).Tags),
		"the refusal must land before any note has moved")
}

func pending(t *testing.T, e *mergeEnv, kind string) []store.Proposal {
	t.Helper()
	props, err := store.PendingProposals(context.Background(), e.g.db, kind)
	require.NoError(t, err)
	return props
}

func reload(t *testing.T, e *mergeEnv, slug string) core.Note {
	t.Helper()
	idx, ok, err := store.NoteBySlug(context.Background(), e.g.db, "demo", slug)
	require.NoError(t, err)
	require.True(t, ok)
	return idx
}
