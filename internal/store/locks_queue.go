package store

// The repo lock queue (plan repo-locks, step RL5, migration 048): the sessions
// waiting for a lock, first come, first served. While a lock has waiters, only
// the head of the queue takes it (AcquireRepoLockWaiting); everyone else is
// refused with the head and their own place. The grant is an ordinary hold, so
// every other surface sees an ordinary holder.
//
// A waiter counts only while a grant could still reach it: its place has not
// lapsed (it asked again within core.RepoLockWaitLease), and its session is
// live (core.Session.LiveAsOf, here in its SQL form, sessionLive). The rows
// that fail either are skipped by every read (core.RepoLock.Waiters) and
// pruned by the writes that read the queue.
//
// The session's liveness is judged with core.SessionIdleTTL, LiveAsOf's own
// fallback: the readers that fill Waiters take no threshold. The configured
// threshold reaches the queue through the reaper, which expires an idle
// session -- and a session that is not active is skipped at once.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// ErrRepoLockQueued is an acquire refused because the lock's queue puts
// another session first: the lock is free, or its hold lapsed, and the head
// takes it next. The refusal carries the lock (its Waiters, the head first)
// and the caller's own place (RepoLockAcquired.Position; 0 for none).
var ErrRepoLockQueued = errors.New("lock queued: another session is next")

// lqLapsedWaiter matches a repo_lock_waiters row a grant can no longer reach
// at the bound instant: a place that lapsed, or a session that ended or is not
// live. A session the reading handle does not show (the console's filtered
// view hides a locked project's sessions) is judged by its place alone. Three
// binds: now, the idle cutoff, now.
const lqLapsedWaiter = `(repo_lock_waiters.place_until <= ? OR EXISTS (SELECT 1 FROM sessions
	WHERE sessions.id = repo_lock_waiters.session_id AND NOT ` + sessionLive + `))`

// lqBinds are lqLapsedWaiter's binds at now.
func lqBinds(now time.Time) []any {
	nowStr := core.FormatTime(now.UTC())
	return []any{nowStr, core.FormatTime(now.Add(-core.SessionIdleTTL).UTC()), nowStr}
}

// lqAttachWaiters fills each lock's Waiters with the sessions a grant could
// still reach at now, head first. ids and in are lkLocks' lock ids and their
// placeholder list.
func lqAttachWaiters(ctx context.Context, q lkQuerier, out []core.RepoLock, ids []any, in string, now time.Time) error {
	at := make(map[string]int, len(out))
	for i, l := range out {
		at[l.ID] = i
	}
	args := append(append([]any{}, ids...), lqBinds(now)...)
	rows, err := q.QueryContext(ctx, `SELECT lock_id, session_id FROM repo_lock_waiters
		WHERE lock_id IN `+in+` AND NOT `+lqLapsedWaiter+`
		ORDER BY lock_id, queued_at, rowid`, args...)
	if err != nil {
		return fmt.Errorf("lock waiters: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var lockID, sessionID string
		if err := rows.Scan(&lockID, &sessionID); err != nil {
			return fmt.Errorf("lock waiters: %w", err)
		}
		if i, ok := at[lockID]; ok {
			out[i].Waiters = append(out[i].Waiters, sessionID)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("lock waiters: %w", err)
	}
	return nil
}

// lqPrune deletes the lock's waiters a grant can no longer reach at now, so a
// place that lapsed rejoins at the back when its session asks again.
func lqPrune(ctx context.Context, q lkQuerier, lockID string, now time.Time) error {
	args := append([]any{lockID}, lqBinds(now)...)
	if _, err := q.ExecContext(ctx, `DELETE FROM repo_lock_waiters WHERE lock_id = ? AND `+lqLapsedWaiter, args...); err != nil {
		return fmt.Errorf("prune lock waiters: %w", err)
	}
	return nil
}

// lqLeave deletes sessionID's place in the lock's queue, if it has one.
func lqLeave(ctx context.Context, q lkQuerier, lockID, sessionID string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM repo_lock_waiters WHERE lock_id = ? AND session_id = ?`, lockID, sessionID); err != nil {
		return fmt.Errorf("leave lock queue: %w", err)
	}
	return nil
}

// lqJoin gives sessionID a place in the lock's queue that lasts
// core.RepoLockWaitLease from now: it keeps the place it holds (queued is its
// current 1-based place, 0 for none), or joins at the back. It returns the
// place's end.
func lqJoin(ctx context.Context, q lkQuerier, lockID, sessionID string, queued int, now time.Time) (time.Time, error) {
	until := now.Add(core.RepoLockWaitLease).UTC()
	untilStr := core.FormatTime(until)
	if queued > 0 {
		res, err := q.ExecContext(ctx, `UPDATE repo_lock_waiters SET place_until = ? WHERE lock_id = ? AND session_id = ?`,
			untilStr, lockID, sessionID)
		if err != nil {
			return time.Time{}, fmt.Errorf("keep lock queue place: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return time.Time{}, fmt.Errorf("keep lock queue place: rows affected: %w", err)
		}
		if n == 1 {
			return until, nil
		}
	}
	// No place, or one pruned after the look: a new one, at the back. A row
	// that survived as unreachable (its session not live) is replaced, so the
	// insert never collides.
	if err := lqLeave(ctx, q, lockID, sessionID); err != nil {
		return time.Time{}, err
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO repo_lock_waiters (lock_id, session_id, queued_at, place_until) VALUES (?, ?, ?, ?)`,
		lockID, sessionID, core.FormatTime(now.UTC()), untilStr); err != nil {
		return time.Time{}, fmt.Errorf("join lock queue: %w", err)
	}
	return until, nil
}

// lqRefuseOrQueue answers an acquire that may not take the lock now: another
// session's live hold (held), or a queue whose head is another session. With
// wait the session keeps or takes a place and the answer is Queued; without,
// it is refused with its place as it stands (ErrRepoLockHeld,
// ErrRepoLockQueued). Either way the prune the acquire made is committed, and
// the lock is read again so its Waiters match the answer.
func lqRefuseOrQueue(ctx context.Context, tx *sql.Tx, prior core.RepoLock, sessionID string, position int, held, wait bool,
	now time.Time) (RepoLockAcquired, error) {
	out := RepoLockAcquired{Position: position}
	if wait {
		until, err := lqJoin(ctx, tx, prior.ID, sessionID, position, now)
		if err != nil {
			return RepoLockAcquired{}, fmt.Errorf("store.AcquireRepoLock: %w", err)
		}
		out.Queued, out.PlaceUntil = true, &until
	}
	cur, ok, err := lkLockByID(ctx, tx, prior.ID, now)
	if err != nil {
		return RepoLockAcquired{}, fmt.Errorf("store.AcquireRepoLock: %w", err)
	}
	if !ok {
		return RepoLockAcquired{}, fmt.Errorf("store.AcquireRepoLock: %w: %q", ErrRepoLockNotFound, prior.ID)
	}
	if err := tx.Commit(); err != nil {
		return RepoLockAcquired{}, fmt.Errorf("store.AcquireRepoLock: commit: %w", err)
	}
	out.Lock = cur
	if wait {
		// The place as the queue now reads it: a session whose own session is
		// not live holds no place a grant could reach.
		out.Position = slices.Index(cur.Waiters, sessionID) + 1
		return out, nil
	}
	if held {
		return out, fmt.Errorf("store.AcquireRepoLock: %w: lock %q held by %q", ErrRepoLockHeld, prior.ID, cur.Holder)
	}
	return out, fmt.Errorf("store.AcquireRepoLock: %w: lock %q", ErrRepoLockQueued, prior.ID)
}

// LeaveRepoLockQueue gives up sessionID's place in the lock's queue: what a
// release by a session that waits for the lock but does not hold it does, and
// what `seam lock acquire --wait` does when its timeout ends the wait. left is
// false when the session held no place a grant could still reach. It returns
// the lock as it now stands; ErrRepoLockNotFound when the id names no lock.
func LeaveRepoLockQueue(ctx context.Context, db *sql.DB, id, sessionID string, now time.Time) (core.RepoLock, bool, error) {
	const op = "store.LeaveRepoLockQueue"
	if sessionID == "" {
		return core.RepoLock{}, false, errors.New(op + ": empty session id")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return core.RepoLock{}, false, fmt.Errorf("%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lqPrune(ctx, tx, id, now); err != nil {
		return core.RepoLock{}, false, fmt.Errorf("%s: %w", op, err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM repo_lock_waiters WHERE lock_id = ? AND session_id = ?`, id, sessionID)
	if err != nil {
		return core.RepoLock{}, false, fmt.Errorf("%s: %w", op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return core.RepoLock{}, false, fmt.Errorf("%s: rows affected: %w", op, err)
	}
	cur, ok, err := lkLockByID(ctx, tx, id, now)
	if err != nil {
		return core.RepoLock{}, false, fmt.Errorf("%s: %w", op, err)
	}
	if !ok {
		return core.RepoLock{}, false, fmt.Errorf("%s: %w: %q", op, ErrRepoLockNotFound, id)
	}
	if err := tx.Commit(); err != nil {
		return core.RepoLock{}, false, fmt.Errorf("%s: commit: %w", op, err)
	}
	return cur, n > 0, nil
}
