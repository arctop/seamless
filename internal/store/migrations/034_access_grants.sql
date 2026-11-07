-- Read grants (isolation.requests, off by default): a read refused at a
-- confidential project's fence files an access request, and the owner may
-- approve it as a grant -- read-only, one project, to the one session (under
-- peer_auth, the one agent process tree) that asked, for a limited time.
--
-- Additive only: one column on access_approvals and one new table. The previous
-- release selects access_approvals' columns by name, so it reads a database
-- this one migrated exactly as before. Both stay empty while isolation.requests
-- is off. They hold metadata about a request, never corpus content: slugs,
-- ids, the kernel's facts about the caller and times. Times are
-- core.TimeFormat (fixed width, UTC), so a text comparison is a chronological
-- one.
--
-- A grant's request is an access_approvals row of kind 'grant': it shares the
-- pending -> approved | denied | expired machine, the one-time challenge and
-- the short id with a lock's request, and the owner decides it on the same
-- surfaces. Only store.ApproveAccessGrant approves one, and it writes a row
-- here, never a lease: store.ApproveAccess(For) refuses a grant request.
--
--   from_project  the project the asking session works in: the grant opens
--                 project_slug to sessions working from here, and nothing it
--                 reads may be written anywhere else ('' for a lock's request)
ALTER TABLE access_approvals ADD COLUMN from_project TEXT NOT NULL DEFAULT '';

-- access_grants holds one row per approved grant. A grant is live while
-- revoked_at IS NULL and expires_at is after now; nothing extends one in place
-- (approving the same holder again revokes the previous grant as refreshed).
-- Every row, live or ended, confines its holder's writes to from_project for
-- the rest of the holder's life:
--
--   id              ULID
--   project_slug    the confidential project it opens, read-only
--   from_project    the project it opens it to
--   process_key     access.ProcessToken.Key of the agent process it was granted
--                   to under peer_auth (its whole tree shares it); '' without
--                   peer_auth
--   session_id      the asking session: the holder without peer_auth, and the
--                   session whose end revokes a "until the session ends" grant
--   peer_uid, peer_pid
--                   the kernel's facts at grant time, for display (0 without
--                   peer_auth)
--   approval_id     the request it came from
--   decided_by      the proof that decided it: an approver credential id, the
--                   daemon's account, or bearer-key (the advisory approval
--                   without peer_auth)
--   granted_at, expires_at
--   revoked_at, revoked_reason
--                   when and why it ended early: refreshed, session_ended,
--                   revoked (by the owner) or reset
CREATE TABLE access_grants (
    id             TEXT PRIMARY KEY,
    project_slug   TEXT NOT NULL,
    from_project   TEXT NOT NULL,
    process_key    TEXT NOT NULL DEFAULT '',
    session_id     TEXT NOT NULL DEFAULT '',
    peer_uid       INTEGER NOT NULL DEFAULT 0,
    peer_pid       INTEGER NOT NULL DEFAULT 0,
    approval_id    TEXT NOT NULL REFERENCES access_approvals (id),
    decided_by     TEXT NOT NULL,
    granted_at     TEXT NOT NULL,
    expires_at     TEXT NOT NULL,
    revoked_at     TEXT,
    revoked_reason TEXT NOT NULL DEFAULT '',
    CHECK (process_key <> '' OR session_id <> ''),
    CHECK (project_slug <> '' AND from_project <> '' AND project_slug <> from_project)
);

CREATE INDEX idx_access_grants_process ON access_grants (process_key, expires_at);
CREATE INDEX idx_access_grants_session ON access_grants (session_id, expires_at);
