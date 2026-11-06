-- Locked-project unlocks (peer_auth.locked, the approval design of
-- 2026-11-04): an owner session asks for access to a locked project, the owner
-- approves it, and the approval becomes a lease -- one project, read and
-- write, for one socket connection's process, for a limited time. internal/access judges every lease it is handed
-- (lease.go); these tables are only where they wait and where they are kept.
--
-- Two new tables and nothing else: no existing table changes shape, so the
-- previous release reads a database this one migrated exactly as before. Both
-- stay empty while peer_auth.locked is unset.
--
-- Both hold metadata about a request, never corpus content: slugs, ids, the
-- kernel's facts about the caller, and the agent's one-line reason (capped in
-- Go). Times are core.TimeFormat (fixed width, UTC), so a text comparison is a
-- chronological one.
--
-- access_approvals holds one row per request, from pending to decided:
--
--   id            ULID; the approval link names it
--   project_slug  the locked project asked for
--   kind          request (a first unlock) or refresh (the session already
--                 holds a lease; approving it retires that lease)
--   challenge     32 random bytes, the one-time WebAuthn challenge the
--                 owner's passkey signs; nothing else ever reuses it
--   status        pending, approved, denied or expired
--   requested_at  when the request was made
--   expires_at    the deadline for a decision; a pending row past it is
--                 never approved
--   lease_seconds how long the lease lasts once approved
--   peer_uid, peer_pid, process_key, program
--                 what the kernel reported about the caller's socket
--                 connection: its uid and pid, access.ProcessToken.Key (the
--                 only one of these a lease is bound to), and the program
--                 path (shown on the approval page, never a credential)
--   session_id, session_name, cwd, repo, model, reason
--                 what the request said about itself, for the approval page
--   decided_at, decided_by
--                 when it was approved or denied, and the passkey credential
--                 id that did it ('' for an expiry)
--   lease_id      the lease an approval created
CREATE TABLE access_approvals (
    id            TEXT PRIMARY KEY,
    project_slug  TEXT NOT NULL,
    kind          TEXT NOT NULL,
    challenge     BLOB NOT NULL CHECK (length(challenge) = 32),
    status        TEXT NOT NULL,
    requested_at  TEXT NOT NULL,
    expires_at    TEXT NOT NULL,
    lease_seconds INTEGER NOT NULL CHECK (lease_seconds > 0),
    peer_uid      INTEGER NOT NULL,
    peer_pid      INTEGER NOT NULL,
    process_key   TEXT NOT NULL,
    program       TEXT NOT NULL DEFAULT '',
    session_id    TEXT NOT NULL DEFAULT '',
    session_name  TEXT NOT NULL DEFAULT '',
    cwd           TEXT NOT NULL DEFAULT '',
    repo          TEXT NOT NULL DEFAULT '',
    model         TEXT NOT NULL DEFAULT '',
    reason        TEXT NOT NULL DEFAULT '',
    decided_at    TEXT,
    decided_by    TEXT NOT NULL DEFAULT '',
    lease_id      TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_access_approvals_process ON access_approvals (process_key, project_slug, status);

-- access_leases holds one row per granted lease. A lease is live while
-- revoked_at IS NULL and expires_at is after now; nothing ever extends one in
-- place (a refresh is a new approval and a new row, and it revokes this one):
--
--   id              ULID
--   project_slug    the one locked project it opens
--   process_key     access.ProcessToken.Key of the connection it was granted
--                   to; every other process, the owner's included, stays
--                   locked out
--   peer_uid, peer_pid
--                   the kernel's facts at grant time, for display
--   session_id      the requesting session, for the expiry warnings its hook
--                   injections carry ('' when the request named none)
--   approval_id     the approval it came from
--   granted_at, expires_at
--   revoked_at, revoked_reason
--                   when and why it ended early: refreshed, session_ended,
--                   revoked (by the owner) or reset (every passkey removed)
CREATE TABLE access_leases (
    id             TEXT PRIMARY KEY,
    project_slug   TEXT NOT NULL,
    process_key    TEXT NOT NULL,
    peer_uid       INTEGER NOT NULL,
    peer_pid       INTEGER NOT NULL,
    session_id     TEXT NOT NULL DEFAULT '',
    approval_id    TEXT NOT NULL REFERENCES access_approvals (id),
    granted_at     TEXT NOT NULL,
    expires_at     TEXT NOT NULL,
    revoked_at     TEXT,
    revoked_reason TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_access_leases_process ON access_leases (process_key, expires_at);
CREATE INDEX idx_access_leases_session ON access_leases (session_id, expires_at);
