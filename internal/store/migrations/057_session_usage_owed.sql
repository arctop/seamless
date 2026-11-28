-- Owed usage: a Claude Code conversation whose ambient row ended without its
-- SessionEnd hook's harvest (store usage_owed.go).
--
-- Claude Code reports a conversation's whole token usage at its SessionEnd
-- (the usage ledger, migration 053), and that hook may come late or never: the
-- Claude app keeps a session open until it is archived or the app quits, and
-- one it stops runs no SessionEnd at all. Once the agent closed its session
-- with session_end, or the idle reaper expired it, the turns since the last
-- harvest are on the transcript alone. The row here says so until they are
-- read: the gardener's upkeep reads them where the daemon may read the file,
-- and under peer_auth the same caller's next hooks are asked for them.
--
-- Additive only: one new table, which the previous release never reads. Its
-- rows are bookkeeping about one session, left out of a merge
-- (internal/archive merges named tables only). IF NOT EXISTS: a database
-- whose migration record was cut back below this one still migrates forward.
--
--   session_id  the conversation's ambient row (sessions.id).
--   owed_at     when its latest end without a SessionEnd harvest came.
--   changed_at  the transcript's latest modification (its subagents'
--               included) at the last read; NULL before the first.
--   asks        under peer_auth: the asks since the last answer.
--   asked_at    under peer_auth: when last asked; NULL before the first.
CREATE TABLE IF NOT EXISTS session_usage_owed (
    session_id TEXT PRIMARY KEY,
    owed_at    TEXT NOT NULL,
    changed_at TEXT,
    asks       INTEGER NOT NULL DEFAULT 0,
    asked_at   TEXT
);
