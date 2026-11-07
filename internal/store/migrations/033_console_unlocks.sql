-- Console unlocks (peer_auth.locked, the approval design): the owner opens a
-- locked project in ONE browser's console, for a limited time. An unlock is a
-- console cookie backed by a row here, never a lease: no agent process, the
-- owner's included, reads a locked project through it.
--
-- One new table and nothing else: no existing table changes shape, so the
-- previous release reads a database this one migrated exactly as before. It
-- stays empty while peer_auth.locked is unset. It holds metadata about an
-- unlock, never corpus content: a slug, hashes, a credential id and times.
-- Times are core.TimeFormat (fixed width, UTC), so a text comparison is a
-- chronological one.
--
-- A row goes pending -> live -> ended:
--
--   pending  a passkey ceremony in flight (challenge set, token_hash naming the
--            browser that began it) or a one-time unlock link that
--            `seamlessd console-open --unlock` wrote, run as the daemon's own
--            account (claim_hash set, token_hash empty). Either is void past
--            pending_until.
--   live     the browser whose seamless_unlock cookie hashes to token_hash
--            reads project_slug until expires_at.
--   ended    the owner locked it again, a restore reset it, or its ceremony or
--            link was used up.
--
--   id            ULID
--   project_slug  the one locked project it opens
--   status        pending, live or ended
--   token_hash    hex SHA-256 of the browser's seamless_unlock cookie; the
--                 cookie itself is never stored
--   claim_hash    hex SHA-256 of a one-time link's secret; '' once claimed,
--                 and for a passkey unlock
--   challenge     the passkey ceremony's 32-byte challenge (a passkey unlock)
--   credential    the approver credential that proved it, or daemon-account
--                 for an unlock link
--   lease_seconds how long it stays live once granted
--   created_at, pending_until
--                 when it was begun, and the deadline for its ceremony or link
--   granted_at, expires_at
--                 when it went live, and when it ends
--   ended_at, ended_reason
--                 when and why it ended early: locked, used or reset
CREATE TABLE console_unlocks (
    id            TEXT PRIMARY KEY,
    project_slug  TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('pending', 'live', 'ended')),
    token_hash    TEXT NOT NULL DEFAULT '',
    claim_hash    TEXT NOT NULL DEFAULT '',
    challenge     BLOB CHECK (challenge IS NULL OR length(challenge) = 32),
    credential    TEXT NOT NULL DEFAULT '',
    lease_seconds INTEGER NOT NULL CHECK (lease_seconds > 0),
    created_at    TEXT NOT NULL,
    pending_until TEXT NOT NULL,
    granted_at    TEXT,
    expires_at    TEXT,
    ended_at      TEXT,
    ended_reason  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_console_unlocks_token ON console_unlocks (token_hash, status, expires_at);
CREATE INDEX idx_console_unlocks_claim ON console_unlocks (claim_hash, status);
