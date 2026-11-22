package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The Tasks page shows who and what on each task that waits on people: gates
// in their own rail (the people's queue), gated steps in theirs, never in
// ready or blocked, and the reader and the peek name the gate.
func TestTasksPage_GatesAndGatedSteps(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	mk := func(title string, kind core.TaskKind, gate core.TaskGate, deps ...string) core.Task {
		id, err := core.NewID()
		require.NoError(t, err)
		task := core.Task{
			ID: id, ProjectSlug: "seamless", Title: title, Status: core.TaskOpen,
			Kind: kind, Gate: gate, DependsOn: deps, CreatedAt: now, UpdatedAt: now,
		}
		require.NoError(t, store.CreateTask(ctx, db, task))
		return task
	}
	gate := mk("GATE sign-off", core.TaskKindGate, core.TaskGate{Who: "owner", What: "sign the release"})
	behind := mk("ship the release", "", core.TaskGate{}, gate.ID)
	own := mk("run the drill", "", core.TaskGate{Who: "sitting:security", What: "the sitting runs it", Decision: "which week?"})
	mk("free work", "", core.TaskGate{})

	var data tasksData
	getJSON(t, mux, "/console/tasks?format=json", &data)
	require.Len(t, data.Ready, 1)
	require.Equal(t, "free work", data.Ready[0].Title)
	require.Empty(t, data.Blocked)
	require.Len(t, data.Gates, 1)
	require.Equal(t, "gate", data.Gates[0].Kind)
	require.Equal(t, "owner", data.Gates[0].Gate.Who)
	require.Len(t, data.Gated, 2)
	whoWaits := map[string][]string{}
	for _, row := range data.Gated {
		whoWaits[row.Title] = row.WaitsOn()
	}
	require.Equal(t, []string{"owner"}, whoWaits["ship the release"], "a step behind a gate waits on the gate's who")
	require.Equal(t, []string{"sitting:security"}, whoWaits["run the drill"])

	list := getHTMLBody(t, mux, "/console/tasks")
	require.Contains(t, list, `id="rg-gates"`)
	require.Contains(t, list, `id="rg-gated"`)
	require.Contains(t, list, "3 waiting on people", "the header counts the gates and the steps they hold, as the board does")
	require.Contains(t, list, "waits on owner: sign the release")
	for _, path := range []string{"/console/tasks/" + behind.ID, "/console/tasks/" + behind.ID + "?reader=1"} {
		require.Contains(t, getHTMLBody(t, mux, path), "gate &middot; waits on owner: sign the release",
			"%s: the task marks a dependency that is a gate", path)
	}

	req := httptest.NewRequest(http.MethodGet, "/console/tasks/"+own.ID+"?peek=1", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	peek := rr.Body.String()
	require.Contains(t, peek, `data-gate="open"`)
	require.Contains(t, peek, "Waits on sitting:security")
	require.Contains(t, peek, "which week?")

	// Cleared, the gate is a record on the step, which is ready again.
	_, err := store.UpdateTask(ctx, db, own.ID, store.TaskPatch{ClearGate: &store.TaskGateClear{By: "owner", Note: "ran it"}}, "", now)
	require.NoError(t, err)
	var detail taskDetail
	getJSON(t, mux, "/console/tasks/"+own.ID+"?format=json", &detail)
	require.NotNil(t, detail.Gate)
	require.False(t, detail.Gate.Open)
	require.Equal(t, "ran it", detail.Gate.Note)
	getJSON(t, mux, "/console/tasks?format=json", &data)
	require.Len(t, data.Ready, 2)
	require.Len(t, data.Gated, 1)
}
