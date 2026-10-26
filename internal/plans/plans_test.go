package plans

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// The package exists so the tag spellings cannot drift apart across the four
// surfaces that speak them (hooks capture, console Plans, briefing, gardener).
// These pin the literal wire format: the strings are persisted in note
// frontmatter and matched by DB prefix queries, so changing one silently orphans
// every plan note already on disk. A change here should have to be deliberate.
func TestVocabularyWireFormat(t *testing.T) {
	require.Equal(t, "cc-plan-", NotePrefix)
	require.Equal(t, "cc-agent-", AgentNotePrefix)
	require.Equal(t, "cc-plan", TagPlan)
	require.Equal(t, "agent-cache", TagAgent)

	require.Equal(t, "draft", StatusDraft)
	require.Equal(t, "presented", StatusPresented)
	require.Equal(t, "approved", StatusApproved)
	require.Equal(t, "abandoned", StatusAbandoned)
	require.Equal(t, "shipped", StatusShipped)

	require.Equal(t, "plan:", SlugTagPrefix())
	require.Equal(t, "plan:deep-audit-refactor", SlugTag("deep-audit-refactor"))
	require.Equal(t, []string{"plan-status:draft"}, SetStatusTag(nil, StatusDraft))
}

// StampHead parses the provenance line hooks' planStamp writes; cmd/seam keeps
// a transcription of this parser, so its behavior is pinned here.
func TestStampHead(t *testing.T) {
	body := "> captured from cc/ab12cd34 | clever-stallman.md | iter 2 | git feedfacef00d | 2026-07-01T00:00:00Z\n\n# Plan\n\nbody"
	require.Equal(t, "feedfacef00d", StampHead(body))
	require.Equal(t, "unknown",
		StampHead("> captured from cc/ab12cd34 | x.md | iter 1 | git unknown | 2026-07-01T00:00:00Z"))
	require.Equal(t, "", StampHead("# Plan\n\nno stamp here"))
	require.Equal(t, "", StampHead(""))
}

// "plan:" is a prefix of nothing else, but "plan-status:" starts with "plan"
// too -- a reader that matched loosely would pull "status:draft" out of a
// status tag as if it were a composition slug. Pin that they cannot cross-talk.
func TestSlugAndStatusTagsDoNotCrossTalk(t *testing.T) {
	tags := []string{"cc-plan", "plan-status:approved", "plan:my-plan", "created-by:agent"}
	require.Equal(t, "my-plan", SlugFromTags(tags))
	require.Equal(t, "approved", StatusFromTags(tags))

	// Each reader ignores the other's tag entirely.
	require.Equal(t, "", SlugFromTags([]string{"plan-status:approved"}))
	require.Equal(t, "", StatusFromTags([]string{"plan:my-plan"}))
}

func TestSlugFromTags(t *testing.T) {
	cases := []struct {
		name string
		tags []string
		want string
	}{
		{"absent", []string{"cc-plan", "created-by:agent"}, ""},
		{"nil", nil, ""},
		{"found", []string{"cc-plan", "plan:my-plan"}, "my-plan"},
		{"first-wins", []string{"plan:first", "plan:second"}, "first"},
		{"bare-prefix-is-no-slug", []string{"plan:"}, ""},
		{"slug-keeps-inner-colons", []string{"plan:a:b"}, "a:b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, SlugFromTags(tc.tags))
		})
	}
}

func TestStatusFromTags(t *testing.T) {
	cases := []struct {
		name string
		tags []string
		want string
	}{
		{"absent", []string{"cc-plan"}, ""},
		{"nil", nil, ""},
		{"found", []string{"cc-plan", "plan-status:presented"}, "presented"},
		{"first-wins", []string{"plan-status:draft", "plan-status:approved"}, "draft"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, StatusFromTags(tc.tags))
		})
	}
}

// SetStatusTag is what F10 leaned on: the capture path rewrites the status tag
// on every plan-file save, and anything it drops is gone from the owner's note.
func TestSetStatusTag(t *testing.T) {
	t.Run("preserves every non-status tag, in order", func(t *testing.T) {
		got := SetStatusTag([]string{"cc-plan", "plan:my-plan", "owner-added"}, StatusApproved)
		require.Equal(t, []string{"cc-plan", "plan:my-plan", "owner-added", "plan-status:approved"}, got)
	})

	t.Run("replaces rather than appends", func(t *testing.T) {
		got := SetStatusTag([]string{"plan-status:draft", "cc-plan"}, StatusApproved)
		require.Equal(t, []string{"cc-plan", "plan-status:approved"}, got)
		require.Equal(t, "approved", StatusFromTags(got))
	})

	t.Run("collapses a note that somehow carries several status tags", func(t *testing.T) {
		got := SetStatusTag([]string{"plan-status:draft", "x", "plan-status:presented"}, StatusApproved)
		require.Equal(t, []string{"x", "plan-status:approved"}, got)
	})

	t.Run("does not alias the caller's slice", func(t *testing.T) {
		in := []string{"cc-plan", "plan-status:draft"}
		_ = SetStatusTag(in, StatusApproved)
		require.Equal(t, []string{"cc-plan", "plan-status:draft"}, in, "input must be untouched")
	})
}

func TestBasename(t *testing.T) {
	require.Equal(t, "my-plan", Basename(NotePrefix+"my-plan"))
	// A slug that never carried the prefix passes through unchanged.
	require.Equal(t, "already-bare", Basename("already-bare"))
	require.Equal(t, "", Basename(NotePrefix))
}

// plan_iteration survives a YAML -> frontmatter -> JSON round-trip, which is why
// the reader tolerates several numeric types. The floor matters: iteration 0
// would render "iteration 0" in the note description.
func TestNoteIteration(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want int
	}{
		{"int", 3, 3},
		{"int64", int64(4), 4},
		{"float64-from-json", float64(5), 5},
		{"string", "6", 6},
		{"unparseable-string", "many", 1},
		{"missing", nil, 1},
		{"wrong-type", []string{"7"}, 1},
		{"zero-floors-to-1", 0, 1},
		{"negative-floors-to-1", -2, 1},
		{"float-truncates", 2.9, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := core.Note{}
			if tc.v != nil {
				n.Extra = map[string]any{"plan_iteration": tc.v}
			}
			require.Equal(t, tc.want, NoteIteration(n))
		})
	}

	// No Extra map at all must not panic.
	require.Equal(t, 1, NoteIteration(core.Note{}))
}

func TestNoteDescription(t *testing.T) {
	require.Equal(t,
		"Captured Claude Code plan my-plan.md (iteration 2, approved)",
		NoteDescription("my-plan", 2, StatusApproved))
}

func newPlansDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func planNote() core.Note {
	return core.Note{
		ID: "01NOTE", Title: "Rebuild the widget", Slug: NotePrefix + "rebuild", Project: "seam",
	}
}

// addPlanStep seeds a plan-step task in a given status, bypassing EnsureTask.
func addPlanStep(t *testing.T, db *sql.DB, project, planSlug string, status core.TaskStatus) {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	require.NoError(t, store.CreateTask(context.Background(), db, core.Task{
		ID: id, ProjectSlug: project, Title: "step", Status: status,
		PlanSlug: planSlug, CreatedAt: now, UpdatedAt: now,
	}))
}

func TestEnsureTask_CreatesTrackingTask(t *testing.T) {
	db := newPlansDB(t)
	ctx := context.Background()
	note := planNote()

	task, created, err := EnsureTask(ctx, db, note, "my-plan", "cc/abc")
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, "Implement plan: Rebuild the widget", task.Title)
	require.Equal(t, "seam", task.ProjectSlug)
	require.Equal(t, "my-plan", task.PlanSlug, "the tracking task is itself a step of the plan")
	require.Equal(t, core.TaskOpen, task.Status)
	require.Equal(t, "cc/abc", task.CreatedBy)
	require.NotEmpty(t, task.ID)
	require.Contains(t, task.Body, note.Slug)
	require.Contains(t, task.Body, note.ID)

	// It is persisted, not just returned.
	stored, err := store.ListTasksForPlan(ctx, db, "seam", "", "my-plan")
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Equal(t, task.ID, stored[0].ID)
}

// Re-approval is a normal event (CC can fire the hook again), so a second call
// must not pile up duplicate tracking tasks.
func TestEnsureTask_IdempotentWhileAStepIsUnclosed(t *testing.T) {
	for _, status := range []core.TaskStatus{core.TaskOpen, core.TaskInProgress} {
		t.Run(string(status), func(t *testing.T) {
			db := newPlansDB(t)
			ctx := context.Background()
			addPlanStep(t, db, "seam", "my-plan", status)

			task, created, err := EnsureTask(ctx, db, planNote(), "my-plan", "cc/abc")
			require.NoError(t, err)
			require.False(t, created)
			require.Zero(t, task)

			stored, err := store.ListTasksForPlan(ctx, db, "seam", "", "my-plan")
			require.NoError(t, err)
			require.Len(t, stored, 1, "no duplicate tracking task")
		})
	}
}

// Only unclosed steps hold it back: once the plan's work is done or dropped, a
// fresh approval opens fresh tracking.
func TestEnsureTask_CreatesAgainWhenEveryStepIsClosed(t *testing.T) {
	db := newPlansDB(t)
	ctx := context.Background()
	addPlanStep(t, db, "seam", "my-plan", core.TaskDone)
	addPlanStep(t, db, "seam", "my-plan", core.TaskDropped)

	_, created, err := EnsureTask(ctx, db, planNote(), "my-plan", "cc/abc")
	require.NoError(t, err)
	require.True(t, created)
}

// Another plan's open step must not suppress this plan's tracking task.
func TestEnsureTask_ScopedToItsOwnPlan(t *testing.T) {
	db := newPlansDB(t)
	ctx := context.Background()
	addPlanStep(t, db, "seam", "other-plan", core.TaskOpen)

	_, created, err := EnsureTask(ctx, db, planNote(), "my-plan", "cc/abc")
	require.NoError(t, err)
	require.True(t, created)
}
