package gardener

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

func TestNormalizeErrorSignature(t *testing.T) {
	cases := []struct {
		name, tool, in, want string
	}{
		{"tool prefix strips", "tasks_add", `tasks_add: title is required`, "title is required"},
		{"quoted literal masks", "tasks_add", `tasks_add: unknown parameter "plam"`, "unknown parameter <v>"},
		{"different literals collapse", "tasks_add", `tasks_add: unknown parameter "titel"`, "unknown parameter <v>"},
		{"ulid masks", "tasks_list", "no task with id 01jm7q3v9k5r8w2x4y6z0a1b2c", "no task with id <id>"},
		{"digit runs mask", "recall", "limit must be between 1 and 500", "limit must be between <n> and <n>"},
		{"digits inside quotes stay one value", "recall", `invalid limit "500"`, "invalid limit <v>"},
		{"case and whitespace wash out", "recall", "Invalid   Scope  here", "invalid scope here"},
		{"empty in empty out", "recall", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, normalizeErrorSignature(tc.tool, tc.in))
		})
	}
}

func recordToolErr(t *testing.T, rec *events.Recorder, ts time.Time, session, project, tool, msg string) {
	t.Helper()
	_, err := rec.Record(context.Background(), core.Event{
		TS: ts, Kind: core.EventToolCall, SessionID: session, ProjectSlug: project,
		Payload: map[string]any{
			"tool": tool, "is_error": true, "error": msg,
			"args": map[string]any{"plam": "x"},
		},
	})
	require.NoError(t, err)
}

func recordHookErr(t *testing.T, rec *events.Recorder, ts time.Time, project, stage, msg string) {
	t.Helper()
	_, err := rec.Record(context.Background(), core.Event{
		TS: ts, Kind: core.EventHookError, ProjectSlug: project,
		Payload: map[string]any{"stage": stage, "error": msg, "client": "claude-code"},
	})
	require.NoError(t, err)
}

func pendingToolErrors(t *testing.T, g *Service) []store.Proposal {
	t.Helper()
	props, err := store.PendingProposals(context.Background(), g.db, store.ProposalToolError)
	require.NoError(t, err)
	return props
}

func TestProposeToolError_FloorsAndPayload(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	g, _, rec := newWantedFixture(t, now)

	// Same template, different literals, two sessions, three hits -> fires.
	recordToolErr(t, rec, now.Add(-48*time.Hour), "S1", "proj", "tasks_add", `tasks_add: unknown parameter "plam"`)
	recordToolErr(t, rec, now.Add(-24*time.Hour), "S2", "proj", "tasks_add", `tasks_add: unknown parameter "titel"`)
	recordToolErr(t, rec, now.Add(-2*time.Hour), "S1", "proj", "tasks_add", `tasks_add: unknown parameter "boddy"`)
	// One session hammering another template -> session floor holds.
	recordToolErr(t, rec, now.Add(-3*time.Hour), "S1", "proj", "recall", `recall: invalid scope "memoires"`)
	recordToolErr(t, rec, now.Add(-2*time.Hour), "S1", "proj", "recall", `recall: invalid scope "notas"`)
	recordToolErr(t, rec, now.Add(-time.Hour), "S1", "proj", "recall", `recall: invalid scope "memz"`)
	// Two sessions but only two hits -> count floor holds.
	recordToolErr(t, rec, now.Add(-3*time.Hour), "S1", "proj", "notes_read", "notes_read: id is required")
	recordToolErr(t, rec, now.Add(-2*time.Hour), "S2", "proj", "notes_read", "notes_read: id is required")

	ctx := context.Background()
	n, err := g.proposeToolError(ctx, loadSeenKeys(t, ctx, g.db))
	require.NoError(t, err)
	require.Equal(t, 1, n)

	props := pendingToolErrors(t, g)
	require.Len(t, props, 1)
	p := props[0].Payload
	require.True(t, strings.HasPrefix(p["key"].(string), "tool_error:proj:"))
	require.Equal(t, "proj", p["project"])
	require.Equal(t, "tool", p["surface"])
	require.Equal(t, "tasks_add", p["name"])
	require.Equal(t, "unknown parameter <v>", p["signature"])
	require.Equal(t, "tasks_add: unknown parameter <v>", p["suggested_title"])
	require.Equal(t, float64(3), p["error_count"])
	require.Equal(t, float64(2), p["session_count"])
	examples := payloadStrings(p, "examples")
	require.Equal(t, []string{
		`tasks_add: unknown parameter "boddy"`,
		`tasks_add: unknown parameter "titel"`,
		`tasks_add: unknown parameter "plam"`,
	}, examples, "recent first")
	require.NotEmpty(t, payloadList(p, "example_args"))
}

func TestProposeToolError_HookWaivesSessionFloor(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	g, _, rec := newWantedFixture(t, now)

	// Recurring unattributed hook failures -> fires despite one (empty) session.
	recordHookErr(t, rec, now.Add(-30*time.Hour), "proj", "ambient-create", "store: session name already exists")
	recordHookErr(t, rec, now.Add(-20*time.Hour), "proj", "ambient-create", "store: session name already exists")
	recordHookErr(t, rec, now.Add(-2*time.Hour), "proj", "ambient-create", "store: session name already exists")
	// Below the count floor -> quiet.
	recordHookErr(t, rec, now.Add(-3*time.Hour), "proj", "prompt-recall", "context deadline exceeded")
	recordHookErr(t, rec, now.Add(-2*time.Hour), "proj", "prompt-recall", "context deadline exceeded")

	ctx := context.Background()
	n, err := g.proposeToolError(ctx, loadSeenKeys(t, ctx, g.db))
	require.NoError(t, err)
	require.Equal(t, 1, n)

	p := pendingToolErrors(t, g)[0].Payload
	require.Equal(t, "hook", p["surface"])
	require.Equal(t, "ambient-create", p["name"])
	require.Equal(t, "hook: ambient-create", p["suggested_title"])
	require.Equal(t, float64(3), p["error_count"])
}

func TestProposeToolError_LivenessTail(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	g, _, rec := newWantedFixture(t, now)

	// Recurred properly, but stopped four days ago -> plausibly fixed, quiet.
	recordToolErr(t, rec, now.Add(-10*24*time.Hour), "S1", "proj", "tasks_add", "tasks_add: title is required")
	recordToolErr(t, rec, now.Add(-6*24*time.Hour), "S2", "proj", "tasks_add", "tasks_add: title is required")
	recordToolErr(t, rec, now.Add(-4*24*time.Hour), "S1", "proj", "tasks_add", "tasks_add: title is required")

	ctx := context.Background()
	n, err := g.proposeToolError(ctx, loadSeenKeys(t, ctx, g.db))
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestProposeToolError_BenignSuppressed(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	g, _, rec := newWantedFixture(t, now)

	// Claim races and not-found probes are the contract working, not a defect.
	for i, msg := range []string{
		`tasks_claim: task already claimed by "cc/aa"`,
		`tasks_claim: task already claimed by "cc/bb"`,
		`tasks_claim: task already claimed by "cc/cc"`,
		`memory_read: memory "nope" not found`,
		`memory_read: memory "nada" not found`,
		`memory_read: memory "zip" not found`,
	} {
		session := "S1"
		if i%2 == 0 {
			session = "S2"
		}
		tool, _, _ := strings.Cut(msg, ":")
		recordToolErr(t, rec, now.Add(-time.Duration(i+1)*time.Hour), session, "proj", tool, msg)
	}

	ctx := context.Background()
	n, err := g.proposeToolError(ctx, loadSeenKeys(t, ctx, g.db))
	require.NoError(t, err)
	require.Zero(t, n)
}

// seedToolErrPattern records the three occurrences that clear both floors for
// one masked template, and returns the proposal they produce.
func seedToolErrPattern(t *testing.T, g *Service, rec *events.Recorder, now time.Time) store.Proposal {
	t.Helper()
	ctx := context.Background()
	recordToolErr(t, rec, now.Add(-48*time.Hour), "S1", "proj", "tasks_add", `tasks_add: unknown parameter "plam"`)
	recordToolErr(t, rec, now.Add(-24*time.Hour), "S2", "proj", "tasks_add", `tasks_add: unknown parameter "titel"`)
	recordToolErr(t, rec, now.Add(-2*time.Hour), "S1", "proj", "tasks_add", `tasks_add: unknown parameter "boddy"`)

	n, err := g.proposeToolError(ctx, loadSeenKeys(t, ctx, g.db))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	props := pendingToolErrors(t, g)
	require.Len(t, props, 1)
	return props[0]
}

// A regular dismissal answers the evidence it was shown, not the pattern for
// all time. Errors that had already happened stay settled; the same error
// happening AGAIN after the decision is a recurrence, and gets asked about
// again -- under the very same masked key, since the key is the pattern's
// identity and never moves.
func TestProposeToolError_DismissLapsesOnRecurrence(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	g, _, rec := newWantedFixture(t, now)
	ctx := context.Background()

	first := seedToolErrPattern(t, g, rec, now)
	require.NoError(t, g.Dismiss(ctx, first.ID))

	// Another literal of the same template, but from before the decision: the
	// dismissal already covered it, so nothing is raised.
	recordToolErr(t, rec, now.Add(-time.Hour), "S3", "proj", "tasks_add", `tasks_add: unknown parameter "urgncy"`)
	n, err := g.proposeToolError(ctx, loadSeenKeys(t, ctx, g.db))
	require.NoError(t, err)
	require.Zero(t, n)
	require.Empty(t, pendingToolErrors(t, g))

	// The same error, still happening a day later. That is new evidence the
	// owner has not answered.
	later := now.Add(24 * time.Hour)
	g.now = func() time.Time { return later }
	recordToolErr(t, rec, later.Add(-time.Hour), "S3", "proj", "tasks_add", `tasks_add: unknown parameter "ttle"`)
	n, err = g.proposeToolError(ctx, loadSeenKeys(t, ctx, g.db))
	require.NoError(t, err)
	require.Equal(t, 1, n)

	again := pendingToolErrors(t, g)
	require.Len(t, again, 1)
	require.Equal(t, first.Payload["key"], again[0].Payload["key"],
		"the same pattern, re-raised under its stable key")
}

// Hiding is the tier that does mean forever: no recurrence gets past it.
// Unhiding lifts the block without returning the proposal to the queue -- the
// next recurrence is what brings the pattern back.
func TestProposeToolError_HideHoldsThroughRecurrence(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	g, _, rec := newWantedFixture(t, now)
	ctx := context.Background()

	first := seedToolErrPattern(t, g, rec, now)
	require.NoError(t, g.Hide(ctx, first.ID))

	later := now.Add(24 * time.Hour)
	g.now = func() time.Time { return later }
	recordToolErr(t, rec, later.Add(-time.Hour), "S3", "proj", "tasks_add", `tasks_add: unknown parameter "ttle"`)
	n, err := g.proposeToolError(ctx, loadSeenKeys(t, ctx, g.db))
	require.NoError(t, err)
	require.Zero(t, n, "a hidden pattern is not re-raised by recurrence")
	require.Empty(t, pendingToolErrors(t, g))

	// Unhide does not itself propose anything...
	require.NoError(t, g.Unhide(ctx, first.ID))
	require.Empty(t, pendingToolErrors(t, g))
	hidden, err := g.Hidden(ctx)
	require.NoError(t, err)
	require.Empty(t, hidden, "the block is gone from the hidden list")

	// ...but the evidence that accrued while it was hidden now counts as the
	// recurrence a regular dismissal lapses on.
	n, err = g.proposeToolError(ctx, loadSeenKeys(t, ctx, g.db))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, pendingToolErrors(t, g), 1)
}

func TestProposeToolError_PerRunCap(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	g, _, rec := newWantedFixture(t, now)

	for _, tool := range []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf"} {
		recordToolErr(t, rec, now.Add(-48*time.Hour), "S1", "proj", tool, tool+": broke badly")
		recordToolErr(t, rec, now.Add(-24*time.Hour), "S2", "proj", tool, tool+": broke badly")
		recordToolErr(t, rec, now.Add(-2*time.Hour), "S1", "proj", tool, tool+": broke badly")
	}

	ctx := context.Background()
	n, err := g.proposeToolError(ctx, loadSeenKeys(t, ctx, g.db))
	require.NoError(t, err)
	require.Equal(t, toolErrorMaxPerRun, n)
	require.Len(t, pendingToolErrors(t, g), toolErrorMaxPerRun)
}

func TestPropose_WiresToolErrorPass(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	g, _, rec := newWantedFixture(t, now)

	recordToolErr(t, rec, now.Add(-48*time.Hour), "S1", "proj", "tasks_add", "tasks_add: title is required")
	recordToolErr(t, rec, now.Add(-24*time.Hour), "S2", "proj", "tasks_add", "tasks_add: title is required")
	recordToolErr(t, rec, now.Add(-2*time.Hour), "S1", "proj", "tasks_add", "tasks_add: title is required")

	res, err := g.Propose(context.Background())
	require.NoError(t, err)
	require.True(t, res.OK())
	require.Equal(t, 1, res.ToolError)
	require.Equal(t, 1, res.Total())
}

func TestApplyToolError_OpensTaskOnce(t *testing.T) {
	g, _, cx := newApplyFixture(t)
	ctx := cx()

	payload := map[string]any{
		"key": "tool_error:proj:abcd1234abcd1234", "project": "proj",
		"surface": "tool", "name": "tasks_add", "signature": "unknown parameter <v>",
		"examples":    []string{`tasks_add: unknown parameter "titel"`, `tasks_add: unknown parameter "plam"`},
		"error_count": 3, "session_count": 2,
		"suggested_title": "tasks_add: unknown parameter <v>",
		"reason":          "recurring tool error: tasks_add returned this error 3x across 2 sessions in 14d",
	}
	p, err := store.CreateProposal(ctx, g.db, store.ProposalToolError, payload)
	require.NoError(t, err)

	res, err := g.Apply(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "applied", res["status"])
	taskID, ok := res["task_id"].(string)
	require.True(t, ok)

	task, err := store.TaskByID(ctx, g.db, taskID)
	require.NoError(t, err)
	require.Equal(t, core.TaskOpen, task.Status)
	require.Equal(t, "gardener", task.CreatedBy)
	require.Equal(t, "proj", task.ProjectSlug)
	require.Equal(t, "Fix recurring error: tasks_add: unknown parameter <v>", task.Title)
	require.Contains(t, task.Body, `"tasks_add: unknown parameter \"titel\""`)
	require.Contains(t, task.Body, "recurring tool error")
	require.Contains(t, task.Body, "tool-description or alias change")

	got, ok, err := store.ProposalByID(ctx, g.db, p.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, store.ProposalApplied, got.Status)

	// A retry reuses the identically-titled open task.
	p2, err := store.CreateProposal(ctx, g.db, store.ProposalToolError, payload)
	require.NoError(t, err)
	res2, err := g.Apply(ctx, p2.ID)
	require.NoError(t, err)
	require.Equal(t, taskID, res2["task_id"])
	require.Equal(t, true, res2["reused"])
}

func TestApplyToolError_HookFraming(t *testing.T) {
	g, _, cx := newApplyFixture(t)
	ctx := cx()

	p, err := store.CreateProposal(ctx, g.db, store.ProposalToolError, map[string]any{
		"key": "tool_error:proj:ffff0000ffff0000", "project": "proj",
		"surface": "hook", "name": "ambient-create", "signature": "ambient-create",
		"examples":    []string{"store: session name already exists"},
		"error_count": 3, "session_count": 1,
		"suggested_title": "hook: ambient-create",
		"reason":          `recurring hook failure: stage "ambient-create" failed 3x in 14d, swallowed fail-open`,
	})
	require.NoError(t, err)

	res, err := g.Apply(ctx, p.ID)
	require.NoError(t, err)
	task, err := store.TaskByID(ctx, g.db, res["task_id"].(string))
	require.NoError(t, err)
	require.Equal(t, "Fix recurring error: hook: ambient-create", task.Title)
	require.Contains(t, task.Body, "hook stage keeps failing")
	require.Contains(t, task.Body, "swallows it")
}

func TestApplyToolError_LongTitleStaysSingleLine(t *testing.T) {
	g, _, cx := newApplyFixture(t)
	ctx := cx()

	long := "tasks_add: unknown parameter <v>: did you mean <v>? valid parameters are: body, content, depends_on, plan, project, text, title"
	p, err := store.CreateProposal(ctx, g.db, store.ProposalToolError, map[string]any{
		"key": "tool_error:proj:0123456789abcdef", "project": "proj",
		"surface": "tool", "name": "tasks_add", "suggested_title": long,
	})
	require.NoError(t, err)

	res, err := g.Apply(ctx, p.ID)
	require.NoError(t, err)
	task, err := store.TaskByID(ctx, g.db, res["task_id"].(string))
	require.NoError(t, err)
	require.NotContains(t, task.Title, "\n", "a truncated title must stay single-line")
	require.True(t, strings.HasSuffix(task.Title, "..."))
	require.Len(t, []rune(task.Title), len([]rune("Fix recurring error: "))+toolErrorTitleRunes+3)
}

func TestApplyToolError_MissingTitleFails(t *testing.T) {
	g, _, cx := newApplyFixture(t)
	ctx := cx()
	p, err := store.CreateProposal(ctx, g.db, store.ProposalToolError, map[string]any{
		"key": "tool_error:proj:x", "project": "proj",
	})
	require.NoError(t, err)
	_, err = g.Apply(ctx, p.ID)
	require.Error(t, err)
	got, ok, err := store.ProposalByID(ctx, g.db, p.ID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, store.ProposalPending, got.Status, "failed apply leaves the proposal pending")
}
