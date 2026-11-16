-- The repo lock queue (plan repo-locks, step RL5): the sessions waiting for a
-- lock, first come, first served. While a lock has waiters, only the head of
-- the queue may take it (no barging); everyone else is told who is next and
-- their own place.
--
-- One row per (lock, session). queued_at is when the session joined, and the
-- queue's order (rowid breaks a tie). place_until is when the session's place
-- lapses unless it asks again: a waiter keeps its place only while it keeps
-- asking (core.RepoLockWaitLease), so a waiter that stopped -- a killed `seam
-- lock acquire --wait`, an agent that moved on -- never blocks a free lock. A
-- waiter whose place lapsed, or whose session ended or is no longer live, is
-- skipped and pruned (store.lqLapsedWaiter, locks_queue.go). No free text: a lock,
-- a session and two times (owner decision D2).
CREATE TABLE repo_lock_waiters (
    lock_id     TEXT NOT NULL,
    session_id  TEXT NOT NULL,
    queued_at   TEXT NOT NULL,
    place_until TEXT NOT NULL,
    UNIQUE (lock_id, session_id)
);

-- idx_repo_lock_waiters_session: a session's places, removed when the session
-- ends or the reaper expires it.
CREATE INDEX idx_repo_lock_waiters_session ON repo_lock_waiters(session_id);
