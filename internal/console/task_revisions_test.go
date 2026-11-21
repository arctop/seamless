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

// A task's earlier bodies on its page (migration 054): the reader and the peek
// fragment fold them under Earlier versions, newest first, and the JSON
// carries them; a task whose body was never replaced renders none of it.
func TestTaskDetail_EarlierVersions(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	for _, tk := range []struct{ id, body string }{{"TR", "the first plan"}, {"TN", "the only plan"}} {
		require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: tk.id, ProjectSlug: "seamless", Title: "task " + tk.id,
			Body: tk.body, Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}))
	}
	second := "the second plan"
	_, err := store.UpdateTask(ctx, db, "TR", store.TaskPatch{Body: &second}, "01SESSIONREVISIONS00000000", now)
	require.NoError(t, err)

	var d taskDetail
	getJSON(t, mux, "/console/tasks/TR?format=json", &d)
	require.Len(t, d.Revisions, 1)
	require.Equal(t, "the first plan", d.Revisions[0].BodyText)
	require.Equal(t, "replace", d.Revisions[0].Change)
	require.Equal(t, "01SESSIONREVISIONS00000000", d.Revisions[0].ReplacedBy)
	for _, path := range []string{"/console/tasks/TR?peek=1", "/console/tasks/TR?reader=1"} {
		rr := getPeek(t, mux, path)
		require.Equal(t, http.StatusOK, rr.Code, path)
		require.Contains(t, rr.Body.String(), "Earlier versions", path)
		require.Contains(t, rr.Body.String(), "the first plan", path)
	}

	var n map[string]any
	getJSON(t, mux, "/console/tasks/TN?format=json", &n)
	require.NotContains(t, n, "revisions")
	for _, path := range []string{"/console/tasks/TN?peek=1", "/console/tasks/TN?reader=1"} {
		require.NotContains(t, getPeek(t, mux, path).Body.String(), "data-revisions", path)
	}
}

// A locked project's task takes its earlier bodies with it: the handle that
// hides the task hides them, while an open task's stay on its page.
func TestTaskDetail_EarlierVersionsHiddenWithTheirTask(t *testing.T) {
	lc := newLockedConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	first, rewritten := "the ledger, first pass "+lockedSecret, "the ledger, reconciled"
	for _, body := range []*string{&first, &rewritten} {
		_, err := store.UpdateTask(ctx, lc.db, lc.taskID, store.TaskPatch{Body: body}, "", now)
		require.NoError(t, err)
	}
	revs, err := store.TaskBodyRevisions(ctx, lc.db, lc.taskID)
	require.NoError(t, err)
	require.Len(t, revs, 1, "the main handle holds the locked task's revision")

	open := mustID(t)
	require.NoError(t, store.CreateTask(ctx, lc.db, core.Task{ID: open, ProjectSlug: openSlug, Title: "ops task",
		Body: openMarker, Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}))
	_, err = store.UpdateTask(ctx, lc.db, open, store.TaskPatch{Body: &rewritten}, "", now)
	require.NoError(t, err)

	get := func(path string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		rr := lc.do(req)
		return rr.Code, rr.Body.String()
	}
	for _, path := range []string{"/console/tasks/" + lc.taskID + "?format=json", "/console/tasks/" + lc.taskID + "?reader=1"} {
		code, body := get(path)
		require.NotEqual(t, http.StatusOK, code, path)
		require.NotContains(t, body, revs[0].Body, path)
	}
	code, body := get("/console/tasks/" + open + "?reader=1")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, body, "Earlier versions")
	require.Contains(t, body, openMarker, "the open task's earlier body")
}
