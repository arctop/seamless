package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// postFavorite issues an authenticated (bearer) favorite toggle. The
// file-backed tests reuse newConsoleWithFiles from memories_test.go.
func postFavorite(mux *http.ServeMux, kind, id string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/console/favorites/"+kind+"/"+id,
		strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return do(mux, req)
}

func TestFavoriteToggle_DBKindsAndRedirects(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.CreateProject(ctx, db, core.Project{
		ID: "01P", Slug: "seam", Name: "Seam", CreatedAt: now, UpdatedAt: now,
	}))

	// Star: 303 to the list page when next is absent.
	rr := postFavorite(mux, "project", "seam", url.Values{"favorite": {"1"}})
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/projects", rr.Header().Get("Location"))
	p, ok, err := store.ProjectBySlug(ctx, db, "seam")
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, p.Favorite)

	// Unstar with a same-origin next target.
	rr = postFavorite(mux, "project", "seam", url.Values{"favorite": {"0"}, "next": {"/console/projects/seam"}})
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/projects/seam", rr.Header().Get("Location"))
	p, _, err = store.ProjectBySlug(ctx, db, "seam")
	require.NoError(t, err)
	require.False(t, p.Favorite)

	// An off-site next cannot become an open redirect.
	rr = postFavorite(mux, "project", "seam", url.Values{"favorite": {"1"}, "next": {"https://evil.example/"}})
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/projects", rr.Header().Get("Location"))

	// Unknown kind and unknown id are 404s, not 500s.
	rr = postFavorite(mux, "widget", "x", url.Values{"favorite": {"1"}})
	require.Equal(t, http.StatusNotFound, rr.Code)
	rr = postFavorite(mux, "task", "01NOPE", url.Values{"favorite": {"1"}})
	require.Equal(t, http.StatusNotFound, rr.Code)

	// JSON mode answers with the new state instead of redirecting.
	req := httptest.NewRequest(http.MethodPost, "/console/favorites/project/seam?format=json",
		strings.NewReader(url.Values{"favorite": {"1"}}.Encode()))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), `"favorite": true`)
}

func TestFavoriteToggle_RequiresOneExplicitBooleanValue(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.CreateProject(ctx, db, core.Project{
		ID: "01P", Slug: "seam", Name: "Seam", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.SetProjectFavorite(ctx, db, "seam", true))

	for _, tt := range []struct {
		name string
		path string
		form url.Values
	}{
		{"missing", "/console/favorites/project/seam", url.Values{}},
		{"invalid", "/console/favorites/project/seam", url.Values{"favorite": {"yes"}}},
		{"multiple", "/console/favorites/project/seam", url.Values{"favorite": {"0", "1"}}},
		{"query value does not substitute for body", "/console/favorites/project/seam?favorite=0", url.Values{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.form.Encode()))
			req.Header.Set("Authorization", "Bearer "+testKey)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rr := do(mux, req)
			require.Equal(t, http.StatusBadRequest, rr.Code)
			project, found, err := store.ProjectBySlug(ctx, db, "seam")
			require.NoError(t, err)
			require.True(t, found)
			require.True(t, project.Favorite, "invalid input must not silently unstar")
		})
	}
}

func TestFavoriteToggle_FileKinds(t *testing.T) {
	db, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mem, err := mgr.WriteMemory(ctx, core.Memory{
		ID: "01M", Kind: core.KindGotcha, Name: "starrable", Description: "d",
		Project: "seam", Body: "body", Created: now, Updated: now, ValidFrom: now,
	})
	require.NoError(t, err)
	note, err := mgr.WriteNote(ctx, core.Note{
		ID: "01N", Title: "Plan narrative", Slug: "plan-narrative", Project: "seam",
		Body: "b", Tags: []string{"plan:starrable-plan"}, Created: now, Updated: now,
	})
	require.NoError(t, err)

	rr := postFavorite(mux, "memory", "01M", url.Values{"favorite": {"1"}})
	require.Equal(t, http.StatusSeeOther, rr.Code)
	onDisk, err := mgr.Store().ReadMemory(mem.FilePath)
	require.NoError(t, err)
	require.True(t, onDisk.Favorite, "the frontmatter is the source of truth")
	require.Equal(t, "body\n", onDisk.Body, "starring must not touch the body")
	idx, ok, err := store.MemoryByID(ctx, db, "01M")
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, idx.Favorite, "the index mirror follows the file")

	// A plan resolves to its primary note's file.
	rr = postFavorite(mux, "plan", "starrable-plan", url.Values{"favorite": {"1"}})
	require.Equal(t, http.StatusSeeOther, rr.Code)
	onDiskNote, err := mgr.Store().ReadNote(note.FilePath)
	require.NoError(t, err)
	require.True(t, onDiskNote.Favorite)

	// A plan with no note cannot be starred.
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: "01T", ProjectSlug: "seam", Title: "step", Status: core.TaskOpen,
		PlanSlug: "task-only", CreatedAt: now, UpdatedAt: now,
	}))
	rr = postFavorite(mux, "plan", "task-only", url.Values{"favorite": {"1"}})
	require.Equal(t, http.StatusNotFound, rr.Code)
}

func TestSortMemoryRowsFavoritesFirst(t *testing.T) {
	rows := []memoryRow{
		{Name: "alpha"}, {Name: "zeta", Favorite: true}, {Name: "mid"},
	}
	sortMemoryRows(rows, "favorites")
	require.Equal(t, "zeta", rows[0].Name)
	require.Equal(t, []string{"alpha", "mid"}, []string{rows[1].Name, rows[2].Name})
}

func TestSortSearchRowsRelevancePartitionsFavorites(t *testing.T) {
	rows := []searchRow{
		{ID: "a"}, {ID: "b", Favorite: true}, {ID: "c"}, {ID: "d", Favorite: true},
	}
	sortSearchRows(rows, "relevance")
	require.Equal(t, []string{"b", "d", "a", "c"},
		[]string{rows[0].ID, rows[1].ID, rows[2].ID, rows[3].ID},
		"stable partition: favorites first, fused order preserved within each half")
}

func TestSearchFavFilter(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: "01TA", ProjectSlug: "seam", Title: "starred needle", Status: core.TaskOpen,
		CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: "01TB", ProjectSlug: "seam", Title: "plain needle", Status: core.TaskOpen,
		CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.SetTaskFavorite(ctx, db, "01TA", true))

	var out struct {
		Total  int `json:"total"`
		Groups []struct {
			Kind  string `json:"kind"`
			Count int    `json:"count"`
		} `json:"groups"`
	}
	getJSON(t, mux, "/console/search?q=needle&format=json", &out)
	require.Equal(t, 2, out.Total)
	getJSON(t, mux, "/console/search?q=needle&fav=1&format=json", &out)
	require.Equal(t, 1, out.Total, "the fav filter keeps only starred rows")

	// A junk fav value is a 400, not a silent default.
	req := httptest.NewRequest(http.MethodGet, "/console/search?q=needle&fav=yes", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusBadRequest, rr.Code)
}

// One slug is a plan per project. Named with its project, a star lands on
// that project's narrative and leaves the other project's plan of the same
// slug alone; without one it keeps resolving the slug as before (the
// earliest note), and a project that is no slug is refused.
func TestFavoriteToggle_PlanNamesItsProject(t *testing.T) {
	_, mgr, mux := newConsoleWithFiles(t)
	ctx := context.Background()
	now := time.Now().UTC()
	first, err := mgr.WriteNote(ctx, core.Note{
		ID: "01NA", Title: "Alpha's shared plan", Slug: "alpha-shared", Project: "alpha",
		Body: "a", Tags: []string{"plan:shared"}, Created: now.Add(-time.Hour), Updated: now,
	})
	require.NoError(t, err)
	second, err := mgr.WriteNote(ctx, core.Note{
		ID: "01NB", Title: "Beta's shared plan", Slug: "beta-shared", Project: "beta",
		Body: "b", Tags: []string{"plan:shared"}, Created: now, Updated: now,
	})
	require.NoError(t, err)
	starred := func(n core.Note) bool {
		t.Helper()
		onDisk, err := mgr.Store().ReadNote(n.FilePath)
		require.NoError(t, err)
		return onDisk.Favorite
	}
	post := func(target string, fav string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(url.Values{"favorite": {fav}}.Encode()))
		req.Header.Set("Authorization", "Bearer "+testKey)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return do(mux, req).Code
	}

	require.Equal(t, http.StatusSeeOther, post("/console/favorites/plan/shared?project=beta", "1"))
	require.True(t, starred(second), "beta's card stars beta's narrative")
	require.False(t, starred(first), "and leaves alpha's plan of the same slug alone")

	require.Equal(t, http.StatusSeeOther, post("/console/favorites/plan/shared?project=beta", "0"))
	require.Equal(t, http.StatusSeeOther, post("/console/favorites/plan/shared", "1"))
	require.True(t, starred(first), "unnamed, the slug's earliest note, as before")
	require.False(t, starred(second))

	require.Equal(t, http.StatusNotFound, post("/console/favorites/plan/shared?project=gamma", "1"), "no such plan in gamma")
	require.Equal(t, http.StatusBadRequest, post("/console/favorites/plan/shared?project=a/b", "1"))
}
