package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// tlTask creates a task in project; mut adjusts it first. It answers the task.
func tlTask(t *testing.T, db *sql.DB, project, plan, title string, mut func(*core.Task)) core.Task {
	t.Helper()
	now := time.Now().UTC()
	task := core.Task{ID: mustID(t), ProjectSlug: project, PlanSlug: plan, Title: title, Status: core.TaskOpen,
		CreatedAt: now, UpdatedAt: now}
	if mut != nil {
		mut(&task)
	}
	require.NoError(t, store.CreateTask(context.Background(), db, task))
	return task
}

// tlKeys is the sorted keys of a JSON object.
func tlKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// tlRawJSON is the Tasks page's JSON model at path, undecoded into a struct.
func tlRawJSON(t *testing.T, mux *http.ServeMux, path string) map[string]any {
	t.Helper()
	var raw map[string]any
	getJSON(t, mux, path, &raw)
	return raw
}

// The Tasks page's JSON keeps every field it had before the page became one
// list (plan:console-ux-pass UX 30): the rails, the closed page, the counts,
// the project menu and the filter, each row with every field it carried.
// The list only adds fields.
func TestTlJSONKeepsItsShape(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	holder := bkSession(t, db, "example-ops", "cc/holder")
	live, lapsed := now.Add(20*time.Minute), now.Add(-20*time.Minute)
	ready := tlTask(t, db, "example-ops", "", "ready one", func(k *core.Task) { k.Account = "agent-acct" })
	require.NoError(t, store.SetTaskFavorite(ctx, db, ready.ID, true))
	old := now.Add(-60 * 24 * time.Hour)
	tlTask(t, db, "example-ops", "", "untouched one", func(k *core.Task) { k.CreatedAt, k.UpdatedAt = old, old })
	tlTask(t, db, "example-ops", "", "blocked one", func(k *core.Task) { k.DependsOn = []string{ready.ID} })
	gate := tlTask(t, db, "example-ops", "", "the gate", func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "sign"}
	})
	tlTask(t, db, "example-ops", "", "behind the gate", func(k *core.Task) { k.DependsOn = []string{gate.ID} })
	tlTask(t, db, "example-ops", "", "held", func(k *core.Task) {
		k.Status, k.ClaimedBy, k.LeaseExpiresAt = core.TaskInProgress, holder, &live
	})
	tlTask(t, db, "example-ops", "", "lapsed", func(k *core.Task) {
		k.Status, k.ClaimedBy, k.LeaseExpiresAt = core.TaskInProgress, holder, &lapsed
	})
	tlTask(t, db, "", "", "claimless", func(k *core.Task) { k.Status = core.TaskInProgress })
	tlTask(t, db, "example-ops", "ship-plan", "a step", nil)
	closed := now.Add(-time.Hour)
	tlTask(t, db, "example-ops", "", "done one", func(k *core.Task) { k.Status, k.ClosedAt = core.TaskDone, &closed })

	raw := tlRawJSON(t, mux, "/console/tasks?format=json&steps=1&closed=all")
	for _, key := range []string{
		"ready", "inProgress", "blocked", "gates", "gated", "closed", "closedMore", "closedTotal",
		"closedPage", "closedPages", "counts", "projects", "filter",
	} {
		require.Contains(t, raw, key, "the JSON keeps %s", key)
	}
	require.Equal(t, []string{"blocked", "inProgress", "lapsed", "open", "ready", "unclaimed", "waiting"},
		tlKeys(raw["counts"].(map[string]any)))
	require.Equal(t, []string{"closed", "page", "sort", "steps"}, tlKeys(raw["filter"].(map[string]any)))
	require.Equal(t, []string{"open", "project"}, tlKeys(raw["projects"].([]any)[0].(map[string]any)))

	// Every row field, each where a row carries it.
	seen := map[string]bool{}
	for _, bucket := range []string{"ready", "inProgress", "blocked", "gates", "gated", "closed"} {
		for _, row := range raw[bucket].([]any) {
			for k := range row.(map[string]any) {
				seen[k] = true
			}
		}
	}
	for _, key := range []string{
		"id", "title", "project", "status", "favorite", "deps", "account", "created", "closed", "blockers",
		"kind", "gate", "claimedBy", "claimLive", "holder", "plan", "updated", "untouched", "lease", "leaseExpiry", "since",
	} {
		require.True(t, seen[key], "a row keeps %s", key)
	}
	first := raw["ready"].([]any)[0].(map[string]any)
	for _, key := range []string{"id", "title", "project", "status", "deps", "created", "updated"} {
		require.Contains(t, first, key, "every row carries %s", key)
	}

	// The same page as a struct: the rails keep their meaning.
	data := bkPage(t, mux, "/console/tasks?format=json")
	require.Equal(t, []string{"untouched one", "ready one"}, bkTitles(data.Ready))
	require.Equal(t, []string{"blocked one"}, bkTitles(data.Blocked))
	require.Equal(t, []string{"held", "lapsed", "claimless"}, bkTitles(data.InProgress))
	require.Equal(t, []string{"the gate"}, bkTitles(data.Gates))
	require.Equal(t, []string{"behind the gate"}, bkTitles(data.Gated))
	require.Equal(t, []string{"done one"}, bkTitles(data.Closed))
	require.Equal(t, taskCounts{Open: 8, InProgress: 1, Lapsed: 1, Unclaimed: 1, Ready: 2, Blocked: 1, Waiting: 2}, data.Counts)
	require.Equal(t, 1, data.ClosedTotal)
	require.True(t, slices.ContainsFunc(data.Projects, func(p taskProjectCount) bool { return p.Project == "" && p.Open == 1 }))
	var bytes json.RawMessage
	getJSON(t, mux, "/console/tasks?format=json", &bytes)
	require.NotContains(t, string(bytes), "<", "the JSON carries no markup")

	// What the list adds: the Needs you group, in its order (the owner's
	// gate, the lapsed claim, the claimless start), and the week's closes.
	require.Equal(t, []string{"the gate", "lapsed", "claimless"}, bkTitles(data.NeedsYou))
	require.Equal(t, 1, data.ClosedRecent)
	plain := tlRawJSON(t, mux, "/console/tasks?format=json")
	require.Contains(t, plain, "needsYou")
	require.Contains(t, plain, "closedRecent")
	require.NotContains(t, plain["filter"], "ready", "ready is named only when the list draws every ready task")
}
