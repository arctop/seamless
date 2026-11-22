package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func TestListSessionsForProject(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	tsAt := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }

	mk := func(id, name, project string, status core.SessionStatus, updated time.Time) {
		require.NoError(t, CreateSession(ctx, db, core.Session{
			ID: id, Name: name, ProjectSlug: project, Status: status,
			CreatedAt: base, UpdatedAt: updated,
		}))
	}
	mk("a", "cc/a", "seam", core.SessionActive, tsAt(1))
	mk("b", "cc/b", "seam", core.SessionCompleted, tsAt(3))
	mk("g", "cc/g", "", core.SessionActive, tsAt(2)) // global -- must NOT leak in
	mk("o", "cc/o", "other", core.SessionActive, tsAt(4))

	// Strict per-slug, newest-updated first; global rows excluded.
	got, err := ListSessionsForProject(ctx, db, "seam", "", time.Time{}, 100)
	require.NoError(t, err)
	require.Equal(t, []string{"b", "a"}, sessionIDs(got))

	// Status filter.
	active, err := ListSessionsForProject(ctx, db, "seam", core.SessionActive, time.Time{}, 100)
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, sessionIDs(active))

	// Since filter (drops the tsAt(1) session).
	since, err := ListSessionsForProject(ctx, db, "seam", "", tsAt(2), 100)
	require.NoError(t, err)
	require.Equal(t, []string{"b"}, sessionIDs(since))

	// Empty project is rejected, pointing at ListSessions.
	_, err = ListSessionsForProject(ctx, db, "", "", time.Time{}, 100)
	require.Error(t, err)
	require.ErrorContains(t, err, "ListSessions")
	require.ErrorContains(t, err, "ambiguous")
}

// The project page's Sessions tab header counts each state apart: an
// expired session is not a completed one, and an active session past the
// idle window with no live claim is idle, not live.
func TestPvProjectSessionTally(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	mk := func(id, project string, status core.SessionStatus, updated time.Time) {
		require.NoError(t, CreateSession(ctx, db, core.Session{
			ID: id, Name: "cc/" + id, ProjectSlug: project, Status: status,
			CreatedAt: updated, UpdatedAt: updated,
		}))
	}
	mk("live", "seam", core.SessionActive, now.Add(-time.Minute))
	mk("idle", "seam", core.SessionActive, now.Add(-2*time.Hour))
	mk("done1", "seam", core.SessionCompleted, now.Add(-3*time.Hour))
	mk("done2", "seam", core.SessionCompleted, now.Add(-4*time.Hour))
	mk("gone", "seam", core.SessionExpired, now.Add(-5*time.Hour))
	mk("global", "", core.SessionActive, now) // the global scope: never counted
	mk("other", "other", core.SessionExpired, now)

	got, err := ProjectSessionTallyAt(ctx, db, "seam", now, 45*time.Minute)
	require.NoError(t, err)
	require.Equal(t, ProjectSessionTally{Total: 5, Live: 1, Idle: 1, Completed: 2, Expired: 1}, got)

	// A quiet session that holds a live claim stays live (core.Session.LiveAsOf).
	require.NoError(t, CreateTask(ctx, db, core.Task{ID: "t1", ProjectSlug: "seam", Title: "held",
		Status: core.TaskOpen, CreatedAt: now, UpdatedAt: now}))
	_, err = ClaimTask(ctx, db, "t1", "idle", 15*time.Minute, now)
	require.NoError(t, err)
	got, err = ProjectSessionTallyAt(ctx, db, "seam", now, 45*time.Minute)
	require.NoError(t, err)
	require.Equal(t, 2, got.Live)
	require.Equal(t, 0, got.Idle)

	_, err = ProjectSessionTallyAt(ctx, db, "", now, 0)
	require.ErrorContains(t, err, "ambiguous")
}

func TestMemoriesForSession(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	ts := func(min int) string { return core.FormatTime(base.Add(time.Duration(min) * time.Minute)) }

	sessID, err := core.NewID()
	require.NoError(t, err)
	insertMemory(t, db, "m1", "gotcha", "m1", "d", "seam", "b", ts(1), "")
	insertMemory(t, db, "m2", "gotcha", "m2", "d", "seam", "b", ts(2), "")
	insertMemory(t, db, "m3", "gotcha", "m3", "d", "seam", "b", ts(3), "")
	insertMemory(t, db, "m4", "gotcha", "m4", "d", "seam", "b", ts(4), "")
	setSourceSession(t, db, "m1", "cc/aabbccdd") // ambient stamp: session name
	setSourceSession(t, db, "m2", "cc/aabbccdd")
	setSourceSession(t, db, "m3", "cc/other")
	setSourceSession(t, db, "m4", sessID) // bound stamp: session ULID

	// Both stamp spellings belong to the one session, newest-updated first.
	got, err := MemoriesForSession(ctx, db, core.Session{ID: sessID, Name: "cc/aabbccdd"})
	require.NoError(t, err)
	require.Equal(t, []string{"m4", "m2", "m1"}, memNames(got))

	// A session that produced nothing yields none.
	nobodyID, err := core.NewID()
	require.NoError(t, err)
	none, err := MemoriesForSession(ctx, db, core.Session{ID: nobodyID, Name: "cc/nobody"})
	require.NoError(t, err)
	require.Empty(t, none)

	// A zero session matches nothing rather than matching empty stamps.
	zero, err := MemoriesForSession(ctx, db, core.Session{})
	require.NoError(t, err)
	require.Empty(t, zero)
}

func TestTasksClaimedBy(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	sess, err := core.NewID()
	require.NoError(t, err)
	other, err := core.NewID()
	require.NoError(t, err)

	a := addTask(t, db, "seam", "A", 1)
	b := addTask(t, db, "seam", "B", 2)
	c := addTask(t, db, "seam", "C", 3)
	_, err = ClaimTask(ctx, db, a, sess, time.Minute, now)
	require.NoError(t, err)
	_, err = ClaimTask(ctx, db, b, sess, time.Minute, now)
	require.NoError(t, err)
	_, err = ClaimTask(ctx, db, c, other, time.Minute, now)
	require.NoError(t, err)

	got, err := TasksClaimedBy(ctx, db, sess)
	require.NoError(t, err)
	require.Equal(t, []string{a, b}, taskIDs(got)) // created_at ASC, id ASC

	// GUARD: a name-with-slash is rejected (not ULID-shaped).
	_, err = TasksClaimedBy(ctx, db, "cc/aabbccdd")
	require.Error(t, err)
	require.ErrorContains(t, err, "expected a session ULID")
	require.ErrorContains(t, err, "SessionByName")

	// GUARD: a non-ULID string is rejected too.
	_, err = TasksClaimedBy(ctx, db, "not-a-ulid")
	require.Error(t, err)
	require.ErrorContains(t, err, "SessionByName")
}

func TestDistinctPlanSlugsForProject(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Plan "alpha" is fully completed (ActivePlans drops it); "beta" is active.
	alpha := addPlanTask(t, db, "seam", "alpha", "a-step", 1)
	setStatus(t, db, alpha, core.TaskDone)
	addPlanTask(t, db, "seam", "beta", "b-step", 2)
	// A non-plan task and another project's plan must not appear.
	addTask(t, db, "seam", "loose", 3)
	addPlanTask(t, db, "other", "gamma", "g-step", 4)

	got, err := DistinctPlanSlugsForProject(ctx, db, "seam")
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "beta"}, got, "includes the completed plan ActivePlans drops")

	// Contrast: ActivePlans omits the completed plan.
	active, err := ActivePlans(ctx, db, "seam")
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, "beta", active[0].Slug)

	// Empty project is rejected.
	_, err = DistinctPlanSlugsForProject(ctx, db, "")
	require.Error(t, err)
	require.ErrorContains(t, err, "ambiguous")
}

func TestProjectMemoriesIncludingInvalid(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	ts := func(min int) string { return core.FormatTime(base.Add(time.Duration(min) * time.Minute)) }

	insertMemory(t, db, "act", "gotcha", "act", "d", "seam", "b", ts(2), "")    // active
	insertMemory(t, db, "inv", "gotcha", "inv", "d", "seam", "b", ts(1), ts(3)) // invalid
	insertMemory(t, db, "glob", "gotcha", "glob", "d", "", "b", ts(4), "")      // global -- excluded

	got, err := ProjectMemoriesIncludingInvalid(ctx, db, "seam")
	require.NoError(t, err)
	require.Equal(t, []string{"act", "inv"}, memNames(got), "both active and invalid, newest first, no global")

	// Empty project is rejected, and the message flags the no-global-union contract.
	_, err = ProjectMemoriesIncludingInvalid(ctx, db, "")
	require.Error(t, err)
	require.ErrorContains(t, err, "ActiveMemories")
	require.ErrorContains(t, err, "ambiguous")
}

func TestProjectsByParent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	mkProject := func(id, slug string) {
		require.NoError(t, CreateProject(ctx, db, core.Project{
			ID: id, Slug: slug, Name: slug, CreatedAt: now, UpdatedAt: now,
		}))
	}
	mkProject("01R", "root")
	mkProject("01C2", "c2")
	mkProject("01C1", "c1")
	mkProject("01U", "unrelated")
	require.NoError(t, SetProjectParent(ctx, db, "c1", "root", now))
	require.NoError(t, SetProjectParent(ctx, db, "c2", "root", now))

	got, err := ProjectsByParent(ctx, db, "root")
	require.NoError(t, err)
	slugs := make([]string, len(got))
	for i, p := range got {
		slugs[i] = p.Slug
	}
	require.Equal(t, []string{"c1", "c2"}, slugs)

	// A parent with no children yields none.
	empty, err := ProjectsByParent(ctx, db, "root2")
	require.NoError(t, err)
	require.Empty(t, empty)
}

// setSourceSession stamps a memory's source_session (insertMemory leaves it blank).
func setSourceSession(t *testing.T, db *sql.DB, id, name string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`UPDATE memories_index SET source_session = ? WHERE id = ?`, name, id)
	require.NoError(t, err)
}

func sessionIDs(ss []core.Session) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}

func taskIDs(ts []core.Task) []string {
	out := make([]string, len(ts))
	for i, tk := range ts {
		out[i] = tk.ID
	}
	return out
}
