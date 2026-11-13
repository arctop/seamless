package console

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// tasks_update records the status before the call as from. An update that
// moved no status (from == to) reads as what it was in the plan ledger and the
// activity feed -- blockers removed, or a plain update -- never as a move to the
// status the task already had. An event without from (every one written
// before, and the other writers: creation, claims, releases, the console's own)
// reads exactly as it always has.
func TestTaskTransition_ReadsTheStatusBeforeTheCall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload map[string]any
		verb    string
		summary string
	}{
		{"one blocker removed", map[string]any{"to": "open", "from": "open", "removed_depends_on": []any{"01KA"}},
			"blocker removed", "task: removed 1 blocker"},
		{"two, from the event log's JSON", map[string]any{"to": "in_progress", "from": "in_progress", "removed_depends_on": []any{"01KA", "01KB"}},
			"blockers removed", "task: removed 2 blockers"},
		{"a same-process payload", map[string]any{"to": "done", "from": "done", "removed_depends_on": []string{"01KA"}},
			"blocker removed", "task: removed 1 blocker"},
		{"removed with a status change", map[string]any{"to": "done", "from": "open", "removed_depends_on": []any{"01KA"}},
			"done", "task -> done"},
		{"an edit of an open task", map[string]any{"to": "open", "from": "open"}, "updated", "task updated"},
		{"an edit of a done task", map[string]any{"to": "done", "from": "done"}, "updated", "task updated"},
		{"a commit recorded", map[string]any{"to": "in_progress", "from": "in_progress", "commits_recorded": float64(1)},
			"updated", "task updated"},
		{"a status move", map[string]any{"to": "done", "from": "in_progress"}, "done", "task -> done"},
		{"a reopen", map[string]any{"to": "open", "from": "done"}, "reopened", "task -> open"},
		{"a start", map[string]any{"to": "in_progress", "from": "open"}, "started", "task -> in_progress"},

		// Without from: exactly as before.
		{"old: an update of an open task", map[string]any{"to": "open"}, "reopened", "task -> open"},
		{"old: a status move", map[string]any{"to": "done"}, "done", "task -> done"},
		{"old: removed_depends_on without from", map[string]any{"to": "open", "removed_depends_on": []any{"01KA"}},
			"reopened", "task -> open"},
		{"created", map[string]any{"to": "open", "created": true}, "created", "task -> open"},
		{"claimed", map[string]any{"to": "in_progress", "claimed_by": "01KS"}, "claimed", "task -> in_progress"},
		{"released", map[string]any{"to": "open", "released": true, "by": "console"}, "released", "task -> open"},
		{"no payload", nil, "reopened", "task transition"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verb, _, _ := transitionVerb(tc.payload)
			require.Equal(t, tc.verb, verb)
			require.Equal(t, tc.summary, eventSummary(core.Event{Kind: core.EventTaskTransition, Payload: tc.payload}))
		})
	}
}

// The Now page's "closed today" counts tasks that reached done: an update of a
// task already done (from == to) closed nothing, while a move to done, and an
// old event that carries only to, count as they always have.
func TestNowContribution_AnEditOfADoneTaskIsNotAClose(t *testing.T) {
	dayStart := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	at := dayStart.Add(time.Hour)
	ev := func(payload map[string]any) core.Event {
		return core.Event{Kind: core.EventTaskTransition, TS: at, Payload: payload}
	}
	c := nowContribution([]core.Event{
		ev(map[string]any{"to": "done", "from": "in_progress"}),
		ev(map[string]any{"to": "done"}),
		ev(map[string]any{"to": "done", "from": "done"}),
		ev(map[string]any{"to": "done", "from": "done", "removed_depends_on": []any{"01KA"}}),
		ev(map[string]any{"to": "open", "from": "done"}),
	}, dayStart)
	require.Equal(t, 2, c.Closed, "the move to done and the old event; neither edit of a done task")
}
