-- Commit records (the commits.record option, off by default): the commit ids
-- a session named at session_end and a task at tasks_update, kept for the
-- commit-matching snapshot (internal/commitfeed) an integrity monitor reads to
-- tell a branch's commits made in tracked work from the ones nothing accounts
-- for.
--
-- One new table and nothing else: no existing table changes shape, so the
-- previous release reads a database this one migrated exactly as before. It
-- stays empty while the option is off -- the MCP argument that fills it is not
-- even offered then.
--
-- commit_records holds one row per (owner, commit), so recording a commit
-- twice is a no-op:
--
--   owner_kind   session or task
--   owner_id     the session's or the task's ULID; no foreign key, because
--                the owner is one of two tables and neither is ever deleted
--                from (the peer_auth view hides a row by its owner instead)
--   commit_id    the full commit id, lowercase hex: 40 digits (SHA-1) or 64
--                (SHA-256), never an abbreviation
--   recorded_by  the session ULID of the call that recorded it ('' when the
--                caller had no session): for a session's own records it is
--                the session itself, for a task's the acting agent
--   recorded_at  when it was recorded
CREATE TABLE commit_records (
    owner_kind  TEXT NOT NULL CHECK (owner_kind IN ('session', 'task')),
    owner_id    TEXT NOT NULL,
    commit_id   TEXT NOT NULL CHECK (length(commit_id) IN (40, 64)),
    recorded_by TEXT NOT NULL DEFAULT '',
    recorded_at TEXT NOT NULL,
    PRIMARY KEY (owner_kind, owner_id, commit_id)
);
