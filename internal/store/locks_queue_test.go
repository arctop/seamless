package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// lqSession creates a session named name in example-ops with status, last
// active at updated, and returns its id.
func lqSession(t *testing.T, db *sql.DB, name string, status core.SessionStatus, updated time.Time) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, CreateSession(context.Background(), db, core.Session{
		ID: id, Name: name, ProjectSlug: "example-ops", Status: status, CreatedAt: updated, UpdatedAt: updated,
	}))
	return id
}

// lqWaiters reads a lock's Waiters the way every surface does.
func lqWaiters(t *testing.T, db *sql.DB, id string) []string {
	t.Helper()
	l, ok, err := RepoLockByID(context.Background(), db, id)
	require.NoError(t, err)
	require.True(t, ok)
	return l.Waiters
}

// lqNow is a clock near the wall clock: the readers with no now of their own
// (ListRepoLocks, RepoLockByID) judge places at time.Now.
func lqNow() time.Time { return time.Now().UTC().Truncate(time.Second) }

// Two waiters are granted the lock in arrival order: while anyone waits only
// the head may take it, a later arrival is refused naming its place, and the
// grant is an ordinary hold that leaves the queue.
func TestRepoLockQueue_GrantsInArrivalOrder(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := lqNow()
	a := lqSession(t, db, "cc/lq-a", core.SessionActive, now)
	b := lqSession(t, db, "cc/lq-b", core.SessionActive, now)
	c := lqSession(t, db, "cc/lq-c", core.SessionActive, now)
	l := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/app"}, "main").Lock

	_, err := AcquireRepoLock(ctx, db, l.ID, a, time.Minute, now)
	require.NoError(t, err)

	// A refusal without wait keeps no place.
	refused, err := AcquireRepoLock(ctx, db, l.ID, b, time.Minute, now)
	require.ErrorIs(t, err, ErrRepoLockHeld)
	require.Zero(t, refused.Position)
	require.Empty(t, lqWaiters(t, db, l.ID))

	gotB, err := AcquireRepoLockWaiting(ctx, db, l.ID, b, time.Minute, now, true)
	require.NoError(t, err)
	require.True(t, gotB.Queued)
	require.Equal(t, 1, gotB.Position)
	require.Equal(t, now.Add(core.RepoLockWaitLease), *gotB.PlaceUntil)
	require.Equal(t, a, gotB.Lock.Holder, "the holder is untouched")
	gotC, err := AcquireRepoLockWaiting(ctx, db, l.ID, c, time.Minute, now.Add(time.Second), true)
	require.NoError(t, err)
	require.Equal(t, 2, gotC.Position)
	require.Equal(t, []string{b, c}, lqWaiters(t, db, l.ID), "arrival order, the head first")

	// Asking again keeps the place, and refreshes it.
	again, err := AcquireRepoLockWaiting(ctx, db, l.ID, b, time.Minute, now.Add(2*time.Second), true)
	require.NoError(t, err)
	require.Equal(t, 1, again.Position)
	require.Equal(t, now.Add(2*time.Second+core.RepoLockWaitLease), *again.PlaceUntil)

	// The holder's own heartbeat is no barge.
	beat, err := AcquireRepoLock(ctx, db, l.ID, a, time.Minute, now.Add(3*time.Second))
	require.NoError(t, err)
	require.True(t, beat.Heartbeat)

	_, err = ReleaseRepoLock(ctx, db, l.ID, a, now.Add(4*time.Second))
	require.NoError(t, err)

	// Free, but B is first: C is refused with its place, waiting or not.
	behind, err := AcquireRepoLock(ctx, db, l.ID, c, time.Minute, now.Add(5*time.Second))
	require.ErrorIs(t, err, ErrRepoLockQueued)
	require.Equal(t, 2, behind.Position)
	require.Equal(t, b, behind.Lock.NextWaiter())
	require.Empty(t, behind.Lock.Holder, "the lock stays free for the head")
	queued, err := AcquireRepoLockWaiting(ctx, db, l.ID, c, time.Minute, now.Add(5*time.Second), true)
	require.NoError(t, err)
	require.True(t, queued.Queued)
	require.Equal(t, 2, queued.Position)
	// So is a session that never queued.
	stranger := lqSession(t, db, "cc/lq-stranger", core.SessionActive, now)
	cut, err := AcquireRepoLock(ctx, db, l.ID, stranger, time.Minute, now.Add(5*time.Second))
	require.ErrorIs(t, err, ErrRepoLockQueued)
	require.Zero(t, cut.Position)

	took, err := AcquireRepoLockWaiting(ctx, db, l.ID, b, time.Minute, now.Add(6*time.Second), true)
	require.NoError(t, err)
	require.False(t, took.Queued)
	require.Equal(t, b, took.Lock.Holder, "the head takes it: an ordinary hold")
	require.Equal(t, []string{c}, took.Lock.Waiters, "and leaves the queue")

	_, err = ReleaseRepoLock(ctx, db, l.ID, b, now.Add(7*time.Second))
	require.NoError(t, err)
	tookC, err := AcquireRepoLock(ctx, db, l.ID, c, time.Minute, now.Add(8*time.Second))
	require.NoError(t, err)
	require.Equal(t, c, tookC.Lock.Holder, "then the next")
	require.Empty(t, tookC.Lock.Waiters)
}

// A waiter that stopped asking, or whose session ended, is skipped: the next
// one takes the lock, and the skipped one's place is gone when it asks again
// (it rejoins at the back).
func TestRepoLockQueue_SkipsAWaiterThatStoppedOrEnded(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := lqNow()
	a := lqSession(t, db, "cc/lq-a", core.SessionActive, now)
	b := lqSession(t, db, "cc/lq-b", core.SessionActive, now)
	c := lqSession(t, db, "cc/lq-c", core.SessionActive, now)
	d := lqSession(t, db, "cc/lq-d", core.SessionActive, now)
	l := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/app"}, "main").Lock
	_, err := AcquireRepoLock(ctx, db, l.ID, a, time.Minute, now)
	require.NoError(t, err)
	for i, s := range []string{b, c, d} {
		_, err := AcquireRepoLockWaiting(ctx, db, l.ID, s, time.Minute, now.Add(time.Duration(i)*time.Millisecond), true)
		require.NoError(t, err)
	}
	require.Equal(t, []string{b, c, d}, lqWaiters(t, db, l.ID))

	// B's session ends (a waiter killed with its agent): skipped at once.
	_, err = db.ExecContext(ctx, `UPDATE sessions SET status = 'completed' WHERE id = ?`, b)
	require.NoError(t, err)
	require.Equal(t, []string{c, d}, lqWaiters(t, db, l.ID))

	// C stops asking (a killed `--wait`): once its place lapses it is skipped,
	// while D, which kept asking, is next.
	keep := now.Add(core.RepoLockWaitLease - time.Second)
	_, err = AcquireRepoLockWaiting(ctx, db, l.ID, d, time.Minute, keep, true)
	require.NoError(t, err)
	lapsed := now.Add(core.RepoLockWaitLease + time.Second)
	_, err = ReleaseRepoLock(ctx, db, l.ID, a, lapsed)
	require.NoError(t, err)
	got, err := AcquireRepoLock(ctx, db, l.ID, d, time.Minute, lapsed)
	require.NoError(t, err)
	require.Equal(t, d, got.Lock.Holder, "the waiters ahead of it were skipped")
	var rows int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM repo_lock_waiters WHERE lock_id = ?`, l.ID).Scan(&rows))
	require.Zero(t, rows, "and pruned")

	// C asks again: a new place, at the back.
	back, err := AcquireRepoLockWaiting(ctx, db, l.ID, c, time.Minute, lapsed.Add(time.Second), true)
	require.NoError(t, err)
	require.Equal(t, 1, back.Position)
}

// A session that is active but idle past the threshold, holding nothing live,
// is no longer live (core.Session.LiveAsOf): its place is skipped even before
// the reaper expires it.
func TestRepoLockQueue_SkipsASessionThatIsNoLongerLive(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := lqNow()
	idle := lqSession(t, db, "cc/lq-idle", core.SessionActive, now.Add(-core.SessionIdleTTL-time.Minute))
	l := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/app"}, "main").Lock
	_, err := db.ExecContext(ctx, `INSERT INTO repo_lock_waiters (lock_id, session_id, queued_at, place_until) VALUES (?, ?, ?, ?)`,
		l.ID, idle, core.FormatTime(now), core.FormatTime(now.Add(time.Minute)))
	require.NoError(t, err)
	require.Empty(t, lqWaiters(t, db, l.ID), "its place is live, its session is not")

	// The negative control: the same place for a live session counts.
	live := lqSession(t, db, "cc/lq-live", core.SessionActive, now)
	_, err = db.ExecContext(ctx, `INSERT INTO repo_lock_waiters (lock_id, session_id, queued_at, place_until) VALUES (?, ?, ?, ?)`,
		l.ID, live, core.FormatTime(now), core.FormatTime(now.Add(time.Minute)))
	require.NoError(t, err)
	require.Equal(t, []string{live}, lqWaiters(t, db, l.ID))
}

// A waiting session leaves the queue on its own (a release that holds
// nothing, or a timed-out wait), at session end, and when the reaper expires
// it; a declaration waiting for the owner has no queue.
func TestRepoLockQueue_PlacesEndWithTheirSession(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := lqNow()
	a := lqSession(t, db, "cc/lq-a", core.SessionActive, now)
	b := lqSession(t, db, "cc/lq-b", core.SessionActive, now)
	c := lqSession(t, db, "cc/lq-c", core.SessionActive, now)
	l := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/app"}, "main").Lock
	other := lkTestDeclare(t, db, CWDIdentity{Host: "h", RepoRoot: "/work/app"}, "release").Lock
	_, err := AcquireRepoLock(ctx, db, l.ID, a, time.Minute, now)
	require.NoError(t, err)
	_, err = AcquireRepoLock(ctx, db, other.ID, a, time.Minute, now)
	require.NoError(t, err)
	for _, s := range []string{b, c} {
		for _, id := range []string{l.ID, other.ID} {
			_, err := AcquireRepoLockWaiting(ctx, db, id, s, time.Minute, now, true)
			require.NoError(t, err)
		}
	}

	// A release by a waiter that does not hold the lock is no release.
	_, err = ReleaseRepoLock(ctx, db, l.ID, b, now)
	require.ErrorIs(t, err, ErrRepoLockNotHolder)
	cur, left, err := LeaveRepoLockQueue(ctx, db, l.ID, b, now)
	require.NoError(t, err)
	require.True(t, left)
	require.Equal(t, []string{c}, cur.Waiters)
	_, left, err = LeaveRepoLockQueue(ctx, db, l.ID, b, now)
	require.NoError(t, err)
	require.False(t, left, "no place, nothing left")

	freed, err := ReleaseRepoLocksForSession(ctx, db, c, now)
	require.NoError(t, err)
	require.Zero(t, freed, "it held nothing")
	require.Empty(t, lqWaiters(t, db, l.ID), "session end gives up every place")
	require.Equal(t, []string{b}, lqWaiters(t, db, other.ID))

	_, err = db.ExecContext(ctx, `UPDATE sessions SET status = 'expired' WHERE id = ?`, b)
	require.NoError(t, err)
	_, err = ReleaseReapedRepoLocks(ctx, db, b, now)
	require.NoError(t, err)
	var rows int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM repo_lock_waiters WHERE session_id = ?`, b).Scan(&rows))
	require.Zero(t, rows, "the reaper's release takes the expired session's places")

	// Nobody queues for a declaration the owner has not approved.
	req, err := DeclareRepoLock(ctx, db, RepoLockDeclaration{Name: "main", Aliases: lkAliases(t, CWDIdentity{Host: "h", RepoRoot: "/work/req"}),
		State: core.RepoLockRequested, Account: "_example_agent"}, now)
	require.NoError(t, err)
	refused, err := AcquireRepoLockWaiting(ctx, db, req.Lock.ID, b, time.Minute, now, true)
	require.ErrorIs(t, err, ErrRepoLockRequested)
	require.False(t, refused.Queued)
	require.Empty(t, lqWaiters(t, db, req.Lock.ID))
}

// Every reader of a lock fills its Waiters: by id, the list, a checkout's
// locks and a resolve.
func TestRepoLockQueue_EveryReaderFillsWaiters(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	now := lqNow()
	a := lqSession(t, db, "cc/lq-a", core.SessionActive, now)
	b := lqSession(t, db, "cc/lq-b", core.SessionActive, now)
	id := CWDIdentity{Host: "h", RepoRoot: "/work/app"}
	l := lkTestDeclare(t, db, id, "main").Lock
	_, err := AcquireRepoLock(ctx, db, l.ID, a, time.Minute, now)
	require.NoError(t, err)
	_, err = AcquireRepoLockWaiting(ctx, db, l.ID, b, time.Minute, now, true)
	require.NoError(t, err)

	byID, _, err := RepoLockByID(ctx, db, l.ID)
	require.NoError(t, err)
	listed, err := ListRepoLocks(ctx, db)
	require.NoError(t, err)
	mine, err := RepoLocksFor(ctx, db, lkAliases(t, id))
	require.NoError(t, err)
	resolved, _, err := ResolveRepoLock(ctx, db, lkAliases(t, id), "main")
	require.NoError(t, err)
	for _, got := range [][]string{byID.Waiters, listed[0].Waiters, mine[0].Waiters, resolved.Waiters} {
		require.Equal(t, []string{b}, got)
	}
}
