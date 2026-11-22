package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// insertSessionEvent writes a raw event row tied to a session, for coverage
// tests (insertEvent hardcodes an empty session_id).
func insertSessionEvent(t *testing.T, db *sql.DB, kind core.EventKind, sessionID string, ts time.Time) {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES (?, ?, ?, ?, '', '', '{}')`,
		id, core.FormatTime(ts), string(kind), sessionID)
	require.NoError(t, err)
}

func TestGetSessionCoverage(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mk := func(id, name string) core.Session {
		return core.Session{ID: id, Name: name, Status: core.SessionCompleted, CreatedAt: now, UpdatedAt: now}
	}

	// s1: covered by findings only.
	s1 := mk("s1", "cc/a")
	s1.Findings = "learned a thing"
	require.NoError(t, CreateSession(ctx, db, s1))
	// s2: covered by a written memory only.
	require.NoError(t, CreateSession(ctx, db, mk("s2", "cc/b")))
	insertSessionEvent(t, db, core.EventMemoryWritten, "s2", now)
	// s3: covered by a note only.
	require.NoError(t, CreateSession(ctx, db, mk("s3", "cc/c")))
	insertSessionEvent(t, db, core.EventNoteWritten, "s3", now)
	// s4: covered by a trial only.
	require.NoError(t, CreateSession(ctx, db, mk("s4", "cc/d")))
	insertSessionEvent(t, db, core.EventTrialRecorded, "s4", now)
	// s5: covered several ways (findings + memory) -- must not double-count.
	s5 := mk("s5", "cc/e")
	s5.Findings = "big session"
	require.NoError(t, CreateSession(ctx, db, s5))
	insertSessionEvent(t, db, core.EventMemoryWritten, "s5", now)
	// s6: uncovered -- only a read and an injection, no durable artifact.
	require.NoError(t, CreateSession(ctx, db, mk("s6", "cc/f")))
	insertSessionEvent(t, db, core.EventMemoryRead, "s6", now)
	insertSessionEvent(t, db, core.EventInjected, "s6", now)
	// s7: uncovered -- started and ended empty.
	require.NoError(t, CreateSession(ctx, db, mk("s7", "cc/g")))

	c, err := GetSessionCoverage(ctx, db, time.Time{})
	require.NoError(t, err)
	require.Equal(t, 7, c.Total)
	require.Equal(t, 5, c.Covered)  // s1..s5; s6,s7 uncovered
	require.Equal(t, 2, c.Findings) // s1, s5
	require.Equal(t, 2, c.Memories) // s2, s5
	require.Equal(t, 1, c.Notes)    // s3
	require.Equal(t, 1, c.Trials)   // s4
}

func TestGetSessionCoverage_Empty(t *testing.T) {
	db := openTestDB(t)
	c, err := GetSessionCoverage(context.Background(), db, time.Time{})
	require.NoError(t, err)
	require.Equal(t, SessionCoverage{}, c)
}

func TestGetSessionCoverage_Windowed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mk := func(id string, created time.Time) core.Session {
		return core.Session{ID: id, Name: "cc/" + id, Status: core.SessionCompleted, CreatedAt: created, UpdatedAt: created}
	}
	// In-window: one covered (findings), one uncovered.
	recent := mk("recent-a", now.Add(-time.Hour))
	recent.Findings = "kept something"
	require.NoError(t, CreateSession(ctx, db, recent))
	require.NoError(t, CreateSession(ctx, db, mk("recent-b", now.Add(-2*time.Hour))))
	// Out of the 24h window: a covered session two days ago.
	old := mk("old-a", now.Add(-48*time.Hour))
	old.Findings = "long ago"
	require.NoError(t, CreateSession(ctx, db, old))

	win := ResolveRetrievalWindow("24h", now)
	c, err := GetSessionCoverage(ctx, db, win.Since)
	require.NoError(t, err)
	require.Equal(t, 2, c.Total, "the 48h-old session is outside the 24h window")
	require.Equal(t, 1, c.Covered)

	all, err := GetSessionCoverage(ctx, db, time.Time{})
	require.NoError(t, err)
	require.Equal(t, 3, all.Total)
	require.Equal(t, 2, all.Covered)
}

func TestSessionCoverageBuckets(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// Buckets floor to the local hour, so now is pinned mid-hour in local time:
	// a wall-clock now in the first minutes of an hour puts the sessions below
	// into the previous bucket. Local, not UTC -- zones offset by :30/:45 shift
	// where the hour boundary falls.
	now := time.Date(2026, 7, 14, 12, 30, 0, 0, time.Local).UTC()

	mk := func(id string, created time.Time) core.Session {
		return core.Session{ID: id, Name: "cc/" + id, Status: core.SessionCompleted, CreatedAt: created, UpdatedAt: created}
	}
	// Two sessions this hour (one covered), one an hour earlier (uncovered).
	s1 := mk("h0a", now.Add(-time.Minute))
	s1.Findings = "kept something"
	require.NoError(t, CreateSession(ctx, db, s1))
	require.NoError(t, CreateSession(ctx, db, mk("h0b", now.Add(-2*time.Minute))))
	require.NoError(t, CreateSession(ctx, db, mk("h1a", now.Add(-70*time.Minute))))

	buckets, err := SessionCoverageBuckets(ctx, db, ResolveRetrievalWindow("24h", now), now)
	require.NoError(t, err)
	require.NotEmpty(t, buckets)

	// The final (current-hour) bucket holds the two recent sessions, one covered.
	last := buckets[len(buckets)-1]
	require.Equal(t, 2, last.Total)
	require.Equal(t, 1, last.Covered)

	total, covered := 0, 0
	for _, b := range buckets {
		total += b.Total
		covered += b.Covered
	}
	require.Equal(t, 3, total, "all three in-window sessions land in some bucket")
	require.Equal(t, 1, covered)
}

func TestSessionCoverageBuckets_Empty(t *testing.T) {
	db := openTestDB(t)
	got, err := SessionCoverageBuckets(context.Background(), db, ResolveRetrievalWindow("all", time.Now().UTC()), time.Now().UTC())
	require.NoError(t, err)
	require.Nil(t, got)
}
