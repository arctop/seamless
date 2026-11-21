-- The usage ledger: a session's model token usage by quarter-hour, model and
-- thread, so the record can say which day, hour or model a session's tokens
-- went to (store/usage.go).
--
-- One row per session x bucket x model x thread. The harvests that exist
-- write it on every install (Claude Code's SessionEnd, each Codex Stop), from
-- the pass over the agent's files they already make: a Claude Code harvest
-- replaces the session's rows with its whole transcript's buckets, a Codex
-- harvest credits what the cumulative counter grew by to the bucket of the
-- Stop. In the same transaction the session's token totals (migration 011's
-- columns) become the sum of its rows, so every surface that reads them reads
-- the ledger's sum.
--
-- A row holds ids, a model id, counts and a time: no text.
--
--   session_id            the session (sessions.id).
--   bucket                the quarter-hour's start, UTC (core.TimeFormat). Every
--                         real UTC offset is a whole number of quarter-hours,
--                         so a local day is an exact union of buckets.
--   model                 the model id verbatim as the provider names it; ''
--                         when the harvest could not name one.
--   thread                main (the agent's own turns) or subagent (the
--                         subagents it launched).
--   input_tokens ...      the four counts, as the sessions columns define them.
--   messages              the model messages counted (a Codex row: its turns).
--   minute_mask           bit i set when a message landed in minute i of the
--                         bucket (15 bits): its population count is the
--                         bucket's active minutes.
--
-- Additive only: one new table and its index. Nothing on sessions changes.
-- The previous release never reads it, and keeps writing the totals columns
-- as it always did. IF NOT EXISTS: a database whose migration record was cut
-- back below this one still migrates forward.
CREATE TABLE IF NOT EXISTS session_usage (
    session_id            TEXT    NOT NULL,
    bucket                TEXT    NOT NULL,
    model                 TEXT    NOT NULL DEFAULT '',
    thread                TEXT    NOT NULL CHECK (thread IN ('main', 'subagent')),
    input_tokens          INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    cached_input_tokens   INTEGER NOT NULL DEFAULT 0 CHECK (cached_input_tokens >= 0),
    cache_creation_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_creation_tokens >= 0),
    output_tokens         INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    messages              INTEGER NOT NULL DEFAULT 0 CHECK (messages >= 0),
    minute_mask           INTEGER NOT NULL DEFAULT 0 CHECK (minute_mask >= 0 AND minute_mask < 32768),
    PRIMARY KEY (session_id, bucket, model, thread)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_session_usage_bucket ON session_usage(bucket);
