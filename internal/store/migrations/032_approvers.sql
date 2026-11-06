-- Approver credentials (peer_auth.locked, the approval design): what can sign
-- an access approval's challenge with the owner present. Two kinds:
--
--   passkey  a WebAuthn credential (internal/passkey): a hardware key, a
--            phone, a platform authenticator or a password manager
--   touchid  the macOS Touch ID helper's Secure Enclave key
--
-- The terminal approval path needs neither: its proof is a sudo to the
-- daemon's own account.
--
-- Two new tables and nothing else: no existing table changes shape, so the
-- previous release reads a database this one migrated exactly as before. Both
-- stay empty until the owner enrols a credential. Neither holds corpus
-- content, and neither is ever merged by an archive import: a credential
-- enrolled on another machine was never shown to this one's owner.
--
-- approver_credentials holds one row per enrolled credential:
--
--   id               ULID
--   kind             passkey or touchid
--   credential_id    the WebAuthn credential id, or the Touch ID key's id (a
--                    hash of its public key); unique per kind
--   public_key       a COSE key for a passkey, the raw uncompressed P-256
--                    point (65 bytes) for a Touch ID key
--   alg              the COSE algorithm (-7 ES256, -8 EdDSA, -257 RS256)
--   sign_count       the highest signature counter seen (0 for synced
--                    passkeys and Touch ID keys)
--   backup_eligible, backup_state
--                    the WebAuthn BE and BS flags as last seen
--   aaguid, transports, attachment, attestation_format, user_handle
--                    what the authenticator reported at registration, kept
--                    so the credential round-trips through internal/passkey
--   name             the owner's label for it
--   added_at         when it was enrolled
--   added_by         how: 'code' (a one-time enrolment code) or the id of the
--                    existing credential that approved it
--   last_used_at     the last approval it signed
CREATE TABLE approver_credentials (
    id                 TEXT PRIMARY KEY,
    kind               TEXT NOT NULL CHECK (kind IN ('passkey', 'touchid')),
    credential_id      BLOB NOT NULL CHECK (length(credential_id) BETWEEN 1 AND 1023),
    public_key         BLOB NOT NULL CHECK (length(public_key) > 0),
    alg                INTEGER NOT NULL,
    sign_count         INTEGER NOT NULL DEFAULT 0 CHECK (sign_count >= 0),
    backup_eligible    INTEGER NOT NULL DEFAULT 0,
    backup_state       INTEGER NOT NULL DEFAULT 0,
    aaguid             BLOB,
    transports         TEXT NOT NULL DEFAULT '',
    attachment         TEXT NOT NULL DEFAULT '',
    attestation_format TEXT NOT NULL DEFAULT '',
    user_handle        BLOB,
    name               TEXT NOT NULL,
    added_at           TEXT NOT NULL,
    added_by           TEXT NOT NULL,
    last_used_at       TEXT,
    UNIQUE (kind, credential_id)
);

-- approver_enrol_codes holds the one-time enrolment codes, by hash. Only
-- `seamlessd approvers enroll-code`, run as the daemon's own account, mints
-- one; no route does. A code is the secret (never a URL), lives ten minutes,
-- and is consumed in the same transaction as the credential it enrols:
--
--   hash        hex SHA-256 of the normalized code; the code itself is never
--               stored
--   kind        the credential kind it may enrol (passkey or touchid)
--   created_at, expires_at
--   used_at     when it was consumed (NULL while unused)
--   used_for    the approver_credentials.id it enrolled
CREATE TABLE approver_enrol_codes (
    hash       TEXT PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('passkey', 'touchid')),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at    TEXT,
    used_for   TEXT NOT NULL DEFAULT ''
);
