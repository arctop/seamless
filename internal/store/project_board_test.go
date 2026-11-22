package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func TestProjectsWithCounts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	tsAt := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }
	ts := func(min int) string { return core.FormatTime(tsAt(min)) }

	// Registered projects: seam (with data) and other (registered, no data).
	require.NoError(t, CreateProject(ctx, db, core.Project{
		ID: "01P", Slug: "seam", Name: "Seam", CreatedAt: base, UpdatedAt: base,
	}))
	require.NoError(t, CreateProject(ctx, db, core.Project{
		ID: "01Q", Slug: "other", Name: "Other", CreatedAt: base, UpdatedAt: base,
	}))

	// seam memories: 2 active, 1 inactive (excluded).
	insertMemory(t, db, "m1", "gotcha", "m1", "d", "seam", "b", ts(1), "")
	insertMemory(t, db, "m2", "gotcha", "m2", "d", "seam", "b", ts(2), "")
	insertMemory(t, db, "m3", "gotcha", "m3", "d", "seam", "b", ts(3), ts(4)) // inactive
	// Global scope memory + note (its OWN scope, not folded into seam).
	insertMemory(t, db, "g1", "gotcha", "g1", "d", "", "b", ts(1), "")
	insertNote(t, db, "gn1", "GN", "gn", "", ts(1))
	// Orphan slug "ghost": data-table rows with NO projects-table row.
	insertMemory(t, db, "gh1", "gotcha", "gh1", "d", "ghost", "b", ts(1), "")
	// seam note.
	insertNote(t, db, "n1", "N", "n", "seam", ts(1))

	// Sessions: seam (one active, one completed), global, orphan.
	mkSession := func(id, name, project string, status core.SessionStatus, updated time.Time) {
		require.NoError(t, CreateSession(ctx, db, core.Session{
			ID: id, Name: name, ProjectSlug: project, Status: status,
			CreatedAt: base, UpdatedAt: updated,
		}))
	}
	mkSession("s-seam-a", "cc/seam-a", "seam", core.SessionActive, tsAt(1))
	mkSession("s-seam-b", "cc/seam-b", "seam", core.SessionCompleted, tsAt(5)) // newest -> LastActive
	// Active but idle: last updated beyond SessionIdleTTL of the query's now, so
	// it counts as a session but never as live (matches the Sessions screen).
	mkSession("s-seam-i", "cc/seam-i", "seam", core.SessionActive, tsAt(10).Add(-core.SessionIdleTTL-time.Minute))
	mkSession("s-glob", "cc/glob", "", core.SessionActive, tsAt(2))
	mkSession("s-ghost", "cc/ghost", "ghost", core.SessionActive, tsAt(3))

	// seam tasks: A open, P in_progress, D done (excluded), B blocked by A.
	a := addTask(t, db, "seam", "A", 1)
	p := addTask(t, db, "seam", "P", 2)
	setStatus(t, db, p, core.TaskInProgress)
	d := addTask(t, db, "seam", "D", 3)
	setStatus(t, db, d, core.TaskDone)
	addTask(t, db, "seam", "B", 4, a) // depends on open A -> blocked
	// Task recency counts every status: the done task changed last.
	_, err := db.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE project_slug = 'seam'`, ts(6))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE id = ?`, ts(20), d)
	require.NoError(t, err)

	// One injection of a seam memory so reach joins in (seam: 1/2 surfaced -> 50%).
	insertRetrievalEvent(t, db, core.EventInjected, "s-seam-a", "m1", "{}", tsAt(1))

	rows, err := ProjectsWithCounts(ctx, db, ResolveRetrievalWindow("all", tsAt(10)), tsAt(10), 0)
	require.NoError(t, err)

	byProject := map[string]ProjectBoardRow{}
	for _, r := range rows {
		byProject[r.Project] = r
	}

	// Every registered project + the global scope + the orphan slug all appear.
	require.Contains(t, byProject, "seam")
	require.Contains(t, byProject, "other")
	require.Contains(t, byProject, "")
	require.Contains(t, byProject, "ghost")

	// seam counts match the single-project peek exactly.
	peek, err := GetProjectCounts(ctx, db, "seam")
	require.NoError(t, err)
	seam := byProject["seam"]
	require.Equal(t, peek.Memories, seam.Memories, "board memories == GetProjectCounts")
	require.Equal(t, peek.Sessions, seam.Sessions, "board sessions == GetProjectCounts")
	require.Equal(t, peek.OpenTasks, seam.OpenTasks, "board open tasks == GetProjectCounts")
	require.Equal(t, peek.Notes, seam.Notes, "board notes == GetProjectCounts")
	require.Equal(t, 2, seam.Memories)
	require.Equal(t, 3, seam.Sessions)
	require.Equal(t, 1, seam.LiveSessions) // active within TTL; the idle active one is excluded
	require.Equal(t, 3, seam.OpenTasks)    // A + P + B (D done excluded)
	require.Equal(t, 1, seam.Blocked)      // B blocked by open A
	require.Equal(t, 1, seam.Ready)        // A
	require.Equal(t, 1, seam.Unclaimed)    // P: in progress with no claim
	require.Zero(t, seam.InProgress, "no live claim holds P")
	require.Equal(t, 1, seam.Notes)
	require.False(t, seam.Unregistered)
	require.True(t, seam.LastActive.Equal(tsAt(5)), "LastActive == MAX(session updated_at)")
	require.True(t, seam.LastTask.Equal(tsAt(20)), "LastTask == MAX(task updated_at), any status: %v", seam.LastTask)
	require.Equal(t, 50, seam.ReachRate) // 1 of 2 active surfaced
	require.Equal(t, 1, seam.Surfaced)
	require.Equal(t, 2, seam.Active)

	// The global "" scope is its own row with the global-scope counts, never
	// folded into seam and never flagged Unregistered.
	global := byProject[""]
	require.Equal(t, 1, global.Memories)
	require.Equal(t, 1, global.Notes)
	require.Equal(t, 1, global.Sessions)
	require.Equal(t, 1, global.LiveSessions)
	require.Equal(t, 0, global.OpenTasks)
	require.False(t, global.Unregistered)
	require.True(t, global.LastActive.Equal(tsAt(2)))

	// The orphan slug appears, flagged Unregistered, with its own counts.
	ghost := byProject["ghost"]
	require.True(t, ghost.Unregistered, "a data-table slug with no projects row is flagged")
	require.Equal(t, 1, ghost.Memories)
	require.Equal(t, 1, ghost.Sessions)
	require.True(t, ghost.LastActive.Equal(tsAt(3)))

	// A registered project with no data still appears, at zero, not flagged.
	other := byProject["other"]
	require.False(t, other.Unregistered)
	require.Equal(t, 0, other.Memories)
	require.Equal(t, 0, other.Sessions)
	require.True(t, other.LastActive.IsZero())
	require.True(t, other.LastTask.IsZero(), "no tasks, no task recency")
	require.True(t, global.LastTask.IsZero())

	// Rows are ordered by slug (global "" first).
	require.Equal(t, "", rows[0].Project)
}

func TestProjectsWithCounts_Empty(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	rows, err := ProjectsWithCounts(context.Background(), db, ResolveRetrievalWindow("all", now), now, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestGetSessionCoverageForProject(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mk := func(id, name, project string) core.Session {
		return core.Session{
			ID: id, Name: name, ProjectSlug: project, Status: core.SessionCompleted,
			CreatedAt: now, UpdatedAt: now,
		}
	}
	// seam: one covered (findings), one covered (memory), one uncovered.
	seamFindings := mk("cs1", "cc/cs1", "seam")
	seamFindings.Findings = "kept a thing"
	require.NoError(t, CreateSession(ctx, db, seamFindings))
	require.NoError(t, CreateSession(ctx, db, mk("cs2", "cc/cs2", "seam")))
	insertSessionEvent(t, db, core.EventMemoryWritten, "cs2", now)
	require.NoError(t, CreateSession(ctx, db, mk("cs3", "cc/cs3", "seam"))) // uncovered
	// A global session and an other-project session, both covered -- must NOT
	// count toward seam's coverage.
	glob := mk("cg1", "cc/cg1", "")
	glob.Findings = "global thing"
	require.NoError(t, CreateSession(ctx, db, glob))
	otherS := mk("co1", "cc/co1", "other")
	otherS.Findings = "other thing"
	require.NoError(t, CreateSession(ctx, db, otherS))

	// Empty project is rejected, and the message points at GetSessionCoverage.
	_, err := GetSessionCoverageForProject(ctx, db, "", time.Time{})
	require.Error(t, err)
	require.ErrorContains(t, err, "GetSessionCoverage")
	require.ErrorContains(t, err, "ambiguous")

	// A real slug returns only that project's coverage.
	c, err := GetSessionCoverageForProject(ctx, db, "seam", time.Time{})
	require.NoError(t, err)
	require.Equal(t, 3, c.Total, "only seam's sessions")
	require.Equal(t, 2, c.Covered) // findings + memory; the third is uncovered
	require.Equal(t, 1, c.Findings)
	require.Equal(t, 1, c.Memories)

	// The since bound narrows the denominator (all seam sessions are at `now`, so
	// a future cutoff drops them all).
	future, err := GetSessionCoverageForProject(ctx, db, "seam", now.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, future.Total)
}

// TestBdProjectBoardRow_JSONKeepsItsKeys: the row's task counts sit in its own
// JSON object, under the keys a script already reads ("blocked").
func TestBdProjectBoardRow_JSONKeepsItsKeys(t *testing.T) {
	raw, err := json.Marshal(ProjectBoardRow{Project: "demo", TaskCounts: TaskCounts{Blocked: 2, Waiting: 3}})
	require.NoError(t, err)
	var keys map[string]any
	require.NoError(t, json.Unmarshal(raw, &keys))
	require.EqualValues(t, 2, keys["blocked"])
	require.EqualValues(t, 3, keys["waiting"])
	for _, k := range []string{"ready", "inProgress", "lapsed", "unclaimed", "gatesOpen", "openTasks"} {
		require.Contains(t, keys, k)
	}
	require.NotContains(t, keys, "TaskCounts")
}
