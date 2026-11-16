package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// clSession creates a session in project, last updated at updated, and
// returns its id.
func clSession(t *testing.T, db *sql.DB, name, project string, status core.SessionStatus, updated time.Time) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, CreateSession(context.Background(), db, core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: status, CreatedAt: updated, UpdatedAt: updated,
	}))
	return id
}

// clClaim creates an open task in project and claims it for session at now,
// leased for lease; it returns the task id.
func clClaim(t *testing.T, db *sql.DB, project, session string, lease time.Duration, now time.Time) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, CreateTask(context.Background(), db, core.Task{
		ID: id, ProjectSlug: project, Title: "held work", Status: core.TaskOpen,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}))
	_, err = ClaimTask(context.Background(), db, id, session, lease, now)
	require.NoError(t, err)
	return id
}

// Every session read fills HeldUntil in the same statement as the row: the
// latest lease among the in_progress tasks the session claimed, and nothing
// else -- another session's claim, an open row's stale residue and a closed
// task are not holds.
func TestSessionHeldUntil_FilledByEveryRead(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: id, Name: "cc/holder", ProjectSlug: "example-ops", Status: core.SessionActive, Ambient: true,
		ExternalClient: "claude-code", ExternalSessionID: "holder-full", CreatedAt: now, UpdatedAt: now,
	}))
	other := clSession(t, db, "cc/other", "example-ops", core.SessionActive, now)

	got, ok, err := SessionByID(ctx, db, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Nil(t, got.HeldUntil, "no claim, no hold")

	clClaim(t, db, "example-ops", id, 30*time.Minute, now)
	clClaim(t, db, "example-ops", id, time.Hour, now)
	clClaim(t, db, "example-ops", other, 3*time.Hour, now)
	closed := clClaim(t, db, "example-ops", id, 2*time.Hour, now)
	done := core.TaskDone
	_, err = UpdateTask(ctx, db, closed, TaskPatch{Status: &done}, id, now)
	require.NoError(t, err)
	residue := clClaim(t, db, "example-ops", id, 2*time.Hour, now)
	_, err = db.ExecContext(ctx, `UPDATE tasks SET status = 'open' WHERE id = ?`, residue)
	require.NoError(t, err)

	want := now.Add(time.Hour)
	reads := map[string]func() core.Session{
		"SessionByID": func() core.Session {
			s, ok, err := SessionByID(ctx, db, id)
			require.NoError(t, err)
			require.True(t, ok)
			return s
		},
		"SessionByName": func() core.Session {
			s, ok, err := SessionByName(ctx, db, "cc/holder")
			require.NoError(t, err)
			require.True(t, ok)
			return s
		},
		"ListSessions": func() core.Session {
			all, err := ListSessions(ctx, db, "", time.Time{}, 0)
			require.NoError(t, err)
			for _, s := range all {
				if s.ID == id {
					return s
				}
			}
			t.Fatal("ListSessions dropped the holder")
			return core.Session{}
		},
		"ReactivateAmbientSession (UPDATE ... RETURNING)": func() core.Session {
			s, ok, err := ReactivateAmbientSession(ctx, db, "claude-code", "holder-full", "", now)
			require.NoError(t, err)
			require.True(t, ok)
			return s
		},
	}
	for name, read := range reads {
		s := read()
		require.NotNil(t, s.HeldUntil, name)
		require.True(t, want.Equal(*s.HeldUntil), "%s: the latest live claim's lease, got %s", name, s.HeldUntil)
	}
}

// The SQL form of the rule (sessionLive) answers exactly as
// core.Session.LiveAsOf, arm by arm and at both boundaries, wherever the
// database judges: the board's live count, and the reaper, which expires
// exactly the active sessions the rule calls dead.
func TestSessionLive_SQLRuleIsTheGoRule(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	ttl := 45 * time.Minute
	idle := now.Add(-2 * time.Hour)

	type row struct {
		project string
		status  core.SessionStatus
		updated time.Time
		claim   func(session string) // nil: holds nothing
		live    bool
	}
	claimUntil := func(project string, until time.Time) func(string) {
		return func(session string) { clClaim(t, db, project, session, 15*time.Minute, until.Add(-15*time.Minute)) }
	}
	rows := []row{
		{project: "example-fresh", status: core.SessionActive, updated: now.Add(-time.Minute), live: true},
		{project: "example-edge", status: core.SessionActive, updated: now.Add(-ttl), live: true},
		{project: "example-past-edge", status: core.SessionActive, updated: now.Add(-ttl - time.Nanosecond)},
		{project: "example-idle", status: core.SessionActive, updated: idle},
		{project: "example-holder", status: core.SessionActive, updated: idle, live: true},
		{project: "example-lapsing-now", status: core.SessionActive, updated: idle},
		{project: "example-lapsing-later", status: core.SessionActive, updated: idle, live: true},
		{project: "example-lapsed", status: core.SessionActive, updated: idle},
		{project: "example-completed", status: core.SessionCompleted, updated: now},
		{project: "example-expired", status: core.SessionExpired, updated: idle},
	}
	rows[4].claim = claimUntil("example-holder", now.Add(time.Hour))
	rows[5].claim = claimUntil("example-lapsing-now", now)
	rows[6].claim = claimUntil("example-lapsing-later", now.Add(time.Nanosecond))
	rows[7].claim = claimUntil("example-lapsed", now.Add(-time.Minute))
	rows[8].claim = claimUntil("example-completed", now.Add(time.Hour))
	rows[9].claim = claimUntil("example-expired", now.Add(time.Hour))

	ids := map[string]string{}
	for _, r := range rows {
		id := clSession(t, db, "sess/"+r.project, r.project, r.status, r.updated)
		ids[r.project] = id
		if r.claim != nil {
			r.claim(id)
		}
	}

	board, err := ProjectsWithCounts(ctx, db, ResolveRetrievalWindow("all", now), now, ttl)
	require.NoError(t, err)
	liveIn := map[string]int{}
	for _, b := range board {
		liveIn[b.Project] = b.LiveSessions
	}
	for _, r := range rows {
		s, ok, err := SessionByID(ctx, db, ids[r.project])
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, r.live, s.LiveAsOf(now, ttl), "%s: the Go rule", r.project)
		want := 0
		if r.live {
			want = 1
		}
		require.Equal(t, want, liveIn[r.project], "%s: the SQL rule (the board's live count)", r.project)
	}

	reaped, err := ExpireIdleSessions(ctx, db, now, ttl)
	require.NoError(t, err)
	gone := map[string]bool{}
	for _, s := range reaped {
		gone[s.ProjectSlug] = true
	}
	for _, r := range rows {
		require.Equal(t, r.status == core.SessionActive && !r.live, gone[r.project],
			"%s: the reaper expires exactly the active sessions the rule calls dead", r.project)
	}
}

// The live count behind the console's Now badge and the update gate counts a
// quiet session holding a live claim, and nothing it holds once the lease has
// lapsed.
func TestLiveSessionCount_CountsAQuietHolder(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Now().UTC()
	cutoff := now.Add(-core.SessionIdleTTL)
	idle := now.Add(-2 * time.Hour)

	clSession(t, db, "cc/fresh", "example-ops", core.SessionActive, now.Add(-time.Minute))
	quiet := clSession(t, db, "cc/quiet", "example-ops", core.SessionActive, idle)
	n, err := LiveSessionCount(ctx, db, cutoff)
	require.NoError(t, err)
	require.Equal(t, 1, n, "a quiet session that holds nothing is not live")

	clClaim(t, db, "example-ops", quiet, time.Hour, now)
	n, err = LiveSessionCount(ctx, db, cutoff)
	require.NoError(t, err)
	require.Equal(t, 2, n, "its live claim keeps the quiet session live")

	lapsed := clSession(t, db, "cc/lapsed", "example-ops", core.SessionActive, idle)
	clClaim(t, db, "example-ops", lapsed, time.Hour, now.Add(-90*time.Minute))
	n, err = LiveSessionCount(ctx, db, cutoff)
	require.NoError(t, err)
	require.Equal(t, 2, n, "a lapsed claim keeps nothing live")
}

// The live lists read ListSessions with the idle cutoff and confirm each row
// with LiveAsOf; the cutoff must keep a quiet holder of a live claim, or those
// lists drop a session the rule calls live.
func TestListSessions_SinceKeepsALiveHolder(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := time.Now().UTC()
	idle := now.Add(-2 * time.Hour)
	holder := clSession(t, db, "cc/holder", "example-ops", core.SessionActive, idle)
	clSession(t, db, "cc/idle", "example-ops", core.SessionActive, idle)
	clClaim(t, db, "example-ops", holder, time.Hour, now)

	listed, err := ListSessions(ctx, db, core.SessionActive, now.Add(-core.SessionIdleTTL), 10)
	require.NoError(t, err)
	require.Len(t, listed, 1, "the quiet holder, not the quiet session that holds nothing")
	require.Equal(t, holder, listed[0].ID)
	require.True(t, listed[0].LiveAsOf(now, core.SessionIdleTTL))
}
