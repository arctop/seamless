-- Console locks: locks the owner turned on from the browser, each behind a
-- fresh passkey assertion. The console removes only these; a project listed
-- under peer_auth.force_locked stays locked until the file changes.
--
-- A row closes the project from the next daemon start (the policy is read once
-- at startup). The set of locked projects is this table's rows plus the file's
-- list.
--
-- One new table and nothing else: no existing table changes shape, so the
-- previous release reads a database this one migrated exactly as before. It
-- stays empty while peer_auth is off. It holds no corpus content: a slug, the
-- approver credential that proved the change, and a time (core.TimeFormat).
CREATE TABLE console_locks (
    project_slug TEXT PRIMARY KEY,
    credential   TEXT NOT NULL,
    locked_at    TEXT NOT NULL
);
