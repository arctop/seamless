-- Device pairing (plan networked-relay): each computer that connects to a
-- networked server pairs with it through a one-time code and gets a credential
-- of its own. A relay (macOS and Linux clients) proves itself with a TLS client
-- certificate over its own key; a key device (Windows, or a machine where the
-- owner has no root, such as a container or CI) holds a bearer key of its own,
-- in place of the server's shared one.
--
-- Additive only: two new tables, and no existing table changes shape, so the
-- previous release reads a database this one migrated exactly as before. Both
-- stay empty unless this install serves other computers (a TLS listener, a
-- bind beyond loopback, or a server_url naming another host). They hold
-- metadata about a computer, never corpus content: hashes, a public key, a
-- host name and times. Times are core.TimeFormat (fixed width, UTC), so a text
-- comparison is a chronological one.
--
-- device_pairing_codes holds the one-time codes the owner mints on the
-- console's Devices page or with `seamlessd devices code`. A code pairs one
-- computer, once, within its lifetime, and is never the long-lived credential.
-- Only its hash is kept.
--
--   id          ULID
--   code_hash   hex SHA-256 of the code's canonical form under its own prefix
--   minted_by   where it was minted: console or terminal
--   created_at, expires_at
--   used_at     when a pairing took it; NULL while unused
--   used_by     the device that took it
CREATE TABLE device_pairing_codes (
    id         TEXT PRIMARY KEY,
    code_hash  TEXT NOT NULL UNIQUE CHECK (length(code_hash) = 64),
    minted_by  TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at    TEXT,
    used_by    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_device_pairing_codes_open ON device_pairing_codes (used_at, expires_at);

-- devices holds the paired computers, relays and key devices together: the
-- console's Devices page lists them as one.
--
--   id            ULID
--   kind          relay or key
--   name          the host name it paired as: every request it makes speaks
--                 for that host, whatever host the request names
--   status        pending (waiting for the owner's passkey: peer_auth only),
--                 active, denied, expired (not approved in time) or revoked
--   fingerprint   what both ends show the owner to compare, XXXX-XXXX-XXXX-XXXX:
--                 a relay's public key, or a key device's pairing secret
--   public_key    a relay's key, DER SubjectPublicKeyInfo; NULL for a key device
--   key_hash      hex SHA-256 of a relay's public key, or of a key device's own
--                 key (NULL until the device collects it): what a request is
--                 matched by. The key itself is never stored.
--   key_prefix    the first characters of a key device's key, for the owner to
--                 recognize it by; '' for a relay
--   secret_hash   hex SHA-256 of the pairing secret the device chose: the only
--                 thing that asks after a pending pairing or collects its key
--   code_id       the code it paired with
--   created_at, pending_until
--                 when it asked to pair, and the deadline for the owner's
--                 approval (its creation, for a pairing the code approved)
--   enrolled_at   when it became active
--   approved_by   the approver credential (a passkey) whose assertion approved
--                 it; '' for a pairing the code alone approved (peer_auth off)
--   last_used_at  its latest request, to the minute
--   revoked_at, revoked_by
--                 when it was revoked (or, pending, denied) and from where:
--                 console, terminal or restore
CREATE TABLE devices (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL CHECK (kind IN ('relay', 'key')),
    name          TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 253),
    status        TEXT NOT NULL CHECK (status IN ('pending', 'active', 'denied', 'expired', 'revoked')),
    fingerprint   TEXT NOT NULL,
    public_key    BLOB,
    key_hash      TEXT CHECK (key_hash IS NULL OR length(key_hash) = 64),
    key_prefix    TEXT NOT NULL DEFAULT '',
    secret_hash   TEXT NOT NULL CHECK (length(secret_hash) = 64),
    code_id       TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    pending_until TEXT NOT NULL,
    enrolled_at   TEXT,
    approved_by   TEXT NOT NULL DEFAULT '',
    last_used_at  TEXT,
    revoked_at    TEXT,
    revoked_by    TEXT NOT NULL DEFAULT ''
);

-- One live device per key and per name: a key or a name that is pending or
-- active is taken, and frees up once that device is denied, expired or
-- revoked.
CREATE UNIQUE INDEX idx_devices_key ON devices (key_hash) WHERE key_hash IS NOT NULL AND status IN ('pending', 'active');
CREATE UNIQUE INDEX idx_devices_name ON devices (name) WHERE status IN ('pending', 'active');
CREATE UNIQUE INDEX idx_devices_secret ON devices (secret_hash);
CREATE INDEX idx_devices_status ON devices (status, pending_until);
