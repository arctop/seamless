package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// scSeed writes a session and a task at the given times (zero: none) in
// project, so a test can say exactly when work last moved there.
func scSeed(t *testing.T, db *sql.DB, project string, session, task time.Time) {
	t.Helper()
	ctx := context.Background()
	if !session.IsZero() {
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, CreateSession(ctx, db, core.Session{ID: id, Name: "cc/" + id[len(id)-10:], ProjectSlug: project,
			Status: core.SessionCompleted, CreatedAt: session.Add(-time.Hour), UpdatedAt: session}))
	}
	if !task.IsZero() {
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, CreateTask(ctx, db, core.Task{ID: id, ProjectSlug: project, Title: "work in " + project,
			Status: core.TaskOpen, CreatedBy: "test", CreatedAt: task.Add(-time.Hour), UpdatedAt: task}))
	}
}

// A project's last activity is the Projects board's: its latest session
// update or task change, whichever is later, for every scope the board has
// a row for; a registered project with neither has a zero time, and the star
// is the project's own.
func TestScActivityIsTheBoardsLastActivity(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for _, slug := range []string{"example-ops", "example-web", "example-cli", "example-idle"} {
		_, err := EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	scSeed(t, db, "example-ops", now.Add(-2*time.Hour), now.Add(-5*time.Hour))     // the session is later
	scSeed(t, db, "example-web", now.Add(-48*time.Hour), now.Add(-10*time.Minute)) // the task is later
	scSeed(t, db, "example-cli", now.Add(-40*24*time.Hour), time.Time{})           // quiet: one old session
	scSeed(t, db, "", now.Add(-3*time.Hour), time.Time{})                          // the global scope
	require.NoError(t, SetProjectFavorite(ctx, db, "example-cli", true))

	got, err := ProjectActivityBySlug(ctx, db)
	require.NoError(t, err)
	board, err := ProjectsWithCounts(ctx, db, ResolveRetrievalWindow("", now), now, 0)
	require.NoError(t, err)
	require.Len(t, got, len(board), "one entry per scope the board has a row for")
	for _, b := range board {
		want := b.LastActive
		if b.LastTask.After(want) {
			want = b.LastTask
		}
		a, ok := got[b.Project]
		require.True(t, ok, "scope %q", b.Project)
		require.True(t, want.Equal(a.LastActive), "scope %q: the board says %s, the switcher %s", b.Project, want, a.LastActive)
	}
	require.True(t, got["example-ops"].LastActive.Equal(now.Add(-2*time.Hour)))
	require.True(t, got["example-web"].LastActive.Equal(now.Add(-10*time.Minute)))
	require.True(t, got["example-idle"].LastActive.IsZero(), "a registered project with no session or task")
	require.True(t, got["example-cli"].Favorite)
	require.False(t, got["example-ops"].Favorite)
	require.False(t, got[""].Favorite, "the global scope has no star")
}

// Through a handle that hides a project, that project has no entry: its
// activity and its star are absent from the read itself.
func TestScActivityHiddenProjectIsAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	now := time.Now().UTC()
	for _, slug := range []string{"example-finance", "example-ops"} {
		_, err := EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		scSeed(t, db, slug, now.Add(-time.Hour), now.Add(-time.Minute))
	}
	require.NoError(t, SetProjectFavorite(ctx, db, "example-finance", true))

	open, err := ProjectActivityBySlug(ctx, db)
	require.NoError(t, err)
	require.Contains(t, open, "example-finance", "the main handle reads it")

	view, err := OpenView(path, []string{"example-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })
	got, err := ProjectActivityBySlug(ctx, view)
	require.NoError(t, err)
	require.NotContains(t, got, "example-finance")
	require.Contains(t, got, "example-ops")
}
