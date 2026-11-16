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

func TestTasksPage_ReadyBlockedClosed(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mk := func(title string, deps ...string) core.Task {
		id, err := core.NewID()
		require.NoError(t, err)
		task := core.Task{
			ID: id, ProjectSlug: "seamless", Title: title, Status: core.TaskOpen,
			DependsOn: deps, CreatedAt: now, UpdatedAt: now,
		}
		require.NoError(t, store.CreateTask(ctx, db, task))
		return task
	}

	a := mk("build the widget") // ready
	mk("ship the widget", a.ID) // blocked by a
	c := mk("write docs")       // will be marked done
	_, err := store.UpdateTask(ctx, db, c.ID, store.TaskPatch{Status: ptrStatus(core.TaskDone)}, "", now)
	require.NoError(t, err)

	var data tasksData
	getJSON(t, mux, "/console/tasks?format=json", &data)

	require.Len(t, data.Ready, 1)
	require.Equal(t, "build the widget", data.Ready[0].Title)
	require.Len(t, data.Blocked, 1)
	require.Equal(t, "ship the widget", data.Blocked[0].Title)
	require.Len(t, data.Blocked[0].Blockers, 1)
	require.Equal(t, "build the widget", data.Blocked[0].Blockers[0].Title)
	require.Len(t, data.Closed, 1)
	require.Equal(t, "write docs", data.Closed[0].Title)
	require.Equal(t, "done", data.Closed[0].Status)

	// HTML renders.
	req := httptest.NewRequest(http.MethodGet, "/console/tasks", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), "ship the widget")
}

func TestTasksPage_RailGroupsOrderNewestFirst(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	base := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)

	create := func(title string, status core.TaskStatus, created, updated time.Time, closed *time.Time, deps ...string) string {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateTask(ctx, db, core.Task{
			ID: id, ProjectSlug: "seamless", Title: title, Status: status,
			DependsOn: deps, CreatedAt: created, UpdatedAt: updated, ClosedAt: closed,
		}))
		return id
	}

	// Give every older row a star. Stars remain visible metadata, but must not
	// displace a newer row when the rail has no explicit alternate sort mode.
	oldInProgress := create("old in progress", core.TaskInProgress, base, base.Add(8*time.Hour), nil)
	create("new in progress", core.TaskInProgress, base.Add(time.Hour), base.Add(time.Hour), nil)
	oldReady := create("old ready", core.TaskOpen, base, base, nil)
	create("new ready", core.TaskOpen, base.Add(time.Hour), base.Add(time.Hour), nil)
	oldBlocked := create("old blocked", core.TaskOpen, base, base, nil, oldInProgress)
	create("new blocked", core.TaskOpen, base.Add(time.Hour), base.Add(time.Hour), nil, oldInProgress)
	closedOld, closedNew := base.Add(2*time.Hour), base.Add(3*time.Hour)
	oldClosed := create("old closed", core.TaskDone, base, closedOld, &closedOld)
	create("new closed", core.TaskDone, base.Add(time.Hour), closedNew, &closedNew)
	for _, id := range []string{oldInProgress, oldReady, oldBlocked, oldClosed} {
		require.NoError(t, store.SetTaskFavorite(ctx, db, id, true))
	}

	var data tasksData
	getJSON(t, mux, "/console/tasks?format=json", &data)
	titles := func(rows []taskRow) []string {
		out := make([]string, len(rows))
		for i := range rows {
			out[i] = rows[i].Title
		}
		return out
	}
	require.Equal(t, []string{"new in progress", "old in progress"}, titles(data.InProgress))
	require.Equal(t, []string{"new ready", "old ready"}, titles(data.Ready))
	require.Equal(t, []string{"new blocked", "old blocked"}, titles(data.Blocked))
	require.Equal(t, []string{"new closed", "old closed"}, titles(data.Closed))
	require.True(t, data.Ready[1].Favorite, "the older star remains visible without changing recency order")
}

func ptrStatus(s core.TaskStatus) *core.TaskStatus { return &s }

// TestTaskRelease_OwnerOverride covers the console release-lock button: the peek
// surfaces the holder and offers the button, and POSTing the release reopens the
// task (clearing the claim) regardless of who held it.
func TestTaskRelease_OwnerOverride(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: id, ProjectSlug: "seamless", Title: "held task", Status: core.TaskOpen,
		CreatedAt: now, UpdatedAt: now,
	}))
	_, err = store.ClaimTask(ctx, db, id, "cc/agent-x", time.Minute, now)
	require.NoError(t, err)

	// The peek surfaces the holder and the release-lock button.
	req := httptest.NewRequest(http.MethodGet, "/console/tasks/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), "cc/agent-x")
	require.Contains(t, rr.Body.String(), "Release lock")

	// POST the release: a browser gets a redirect back to the task.
	rr = do(mux, postAuthed("/console/tasks/"+id+"/release"))
	require.Equal(t, http.StatusSeeOther, rr.Code)

	// The claim is cleared and the task is open again.
	got, err := store.TaskByID(ctx, db, id)
	require.NoError(t, err)
	require.Equal(t, core.TaskOpen, got.Status)
	require.Empty(t, got.ClaimedBy)
	require.Nil(t, got.LeaseExpiresAt)

	// The button is gone now that nothing holds the task.
	req = httptest.NewRequest(http.MethodGet, "/console/tasks/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr = do(mux, req)
	require.NotContains(t, rr.Body.String(), "Release lock")

	// A JSON caller (the seam CLI --force path) gets 200 with the reopened task;
	// releasing an unclaimed task is an error, not a silent success.
	_, err = store.ClaimTask(ctx, db, id, "cc/agent-y", time.Minute, time.Now().UTC())
	require.NoError(t, err)
	req = httptest.NewRequest(http.MethodPost, "/console/tasks/"+id+"/release", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Accept", "application/json")
	rr = do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), `"status": "open"`)
}

// postAuthed builds an authenticated POST request (no body) for a console action.
func postAuthed(path string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	return req
}

// The owner's release from the console tells the session that held the claim:
// one notice waits for its next hook injection or tool result.
func TestTaskRelease_TellsTheHolder(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: id, ProjectSlug: "seamless", Title: "held task",
		Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}))
	_, err = store.ClaimTask(ctx, db, id, "01CLHOLDERAAAAAAAAAAAAAAAA", time.Hour, now)
	require.NoError(t, err)

	rr := do(mux, postAuthed("/console/tasks/"+id+"/release"))
	require.Equal(t, http.StatusSeeOther, rr.Code)

	pending, err := store.PendingClaimNotices(ctx, db, "01CLHOLDERAAAAAAAAAAAAAAAA")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, store.ClaimLostReleased, pending[0].Reason)
	require.Equal(t, id, pending[0].ResourceID)
}

// A session idle past the threshold while it holds a live claim is live on the
// console's session surfaces: the Sessions screen, its own page, and the Now
// page, where its claim rides its card rather than the loose ends. Once the
// lease lapses it reads idle again.
func TestConsole_AQuietHolderOfALiveClaimIsLive(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	idle := now.Add(-2 * time.Hour)
	mk := func(name string) string {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: id, Name: name, ProjectSlug: "seamless",
			Status: core.SessionActive, CreatedAt: idle, UpdatedAt: idle}))
		return id
	}
	holder, quiet := mk("cc/gates"), mk("cc/quiet")
	task, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: task, ProjectSlug: "seamless", Title: "run the gates",
		Status: core.TaskOpen, CreatedAt: idle, UpdatedAt: idle}))
	_, err = store.ClaimTask(ctx, db, task, holder, time.Hour, now)
	require.NoError(t, err)

	liveOf := func() (active, idleN int, live map[string]bool) {
		t.Helper()
		var list sessionsData
		getJSON(t, mux, "/console/sessions?format=json", &list)
		live = map[string]bool{}
		for _, r := range list.Sessions {
			live[r.ID] = r.Live
		}
		return list.Active, list.Idle, live
	}
	active, idleN, live := liveOf()
	require.Equal(t, 1, active)
	require.Equal(t, 1, idleN)
	require.True(t, live[holder], "its live claim keeps the quiet holder live")
	require.False(t, live[quiet])

	var detail struct {
		Live bool `json:"live"`
	}
	getJSON(t, mux, "/console/sessions/"+holder+"?format=json", &detail)
	require.True(t, detail.Live)

	var page nowData
	getJSON(t, mux, "/console/now?format=json", &page)
	require.Equal(t, 1, page.AgentsLive)
	require.Equal(t, holder, page.Agents[0].ID)
	require.Len(t, page.Agents[0].Claims, 1)
	require.Equal(t, task, page.Agents[0].Claims[0].ID)
	require.Empty(t, page.Loose, "a live holder's claim is no loose end")

	_, err = db.ExecContext(ctx, `UPDATE tasks SET lease_expires_at = ? WHERE id = ?`,
		core.FormatTime(now.Add(-time.Minute)), task)
	require.NoError(t, err)
	active, idleN, live = liveOf()
	require.Zero(t, active, "the lease lapsed: nothing keeps it live")
	require.Equal(t, 2, idleN)
	require.False(t, live[holder])
}
