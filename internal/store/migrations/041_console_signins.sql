-- Console sign-in with a passkey (console.sign_in: passkey, off by default): a
-- browser signs in to the console with an enrolled passkey instead of the
-- bearer key, and a device adds its passkey through an enrollment that waits
-- for the owner's approval on the server.
--
-- Additive only: two new tables, and no existing table changes shape, so the
-- previous release reads a database this one migrated exactly as before. Both
-- stay empty while console.sign_in is key, the default. They hold metadata
-- about a browser or a key, never corpus content: hashes, public keys, a label
-- and times. Times are core.TimeFormat (fixed width, UTC), so a text
-- comparison is a chronological one.
--
-- console_signins holds one row per sign-in. The browser keeps a random
-- seamless_console cookie and a key only the console's origin can use
-- (internal/console/browserproof). A browser sends its cookies to every port
-- of its host, so the cookie never names a browser alone: a request is signed
-- in only when its Seamless-Browser-Proof verifies under a key that, hashed
-- with the cookie, gives a live row's token_hash. A row is live while ended_at
-- IS NULL and expires_at is after now; nothing extends one in place.
--
--   id            ULID
--   token_hash    store.HashConsoleSignIn of the cookie and the browser key;
--                 neither is stored
--   cookie_hash   hex SHA-256 of the cookie alone, under its own prefix: what
--                 a sign-out finds the row by, since signing out needs no proof
--   credential    the approver credential (a passkey) whose assertion signed
--                 the browser in
--   created_at, expires_at
--   ended_at, ended_reason
--                 when and why it ended early: signed-out, removed (its passkey
--                 was removed) or reset (a restore, or every credential reset)
CREATE TABLE console_signins (
    id           TEXT PRIMARY KEY,
    token_hash   TEXT NOT NULL CHECK (length(token_hash) = 64),
    cookie_hash  TEXT NOT NULL CHECK (length(cookie_hash) = 64),
    credential   TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    ended_at     TEXT,
    ended_reason TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_console_signins_token ON console_signins (token_hash, expires_at);
CREATE INDEX idx_console_signins_cookie ON console_signins (cookie_hash);
CREATE INDEX idx_console_signins_credential ON console_signins (credential);

-- passkey_enrollments holds a passkey a device created on the console's Add a
-- passkey page, waiting for the owner. The page shows a short code that names
-- the row; the owner approves that code on the server (`seamlessd approvers
-- approve --code`, as the daemon's account under peer_auth), and the approval
-- copies this row's credential, unchanged, into approver_credentials. Nothing
-- the approval carries names a key, so a code cannot enroll any other one.
--
--   id                 ULID
--   status             pending, enrolled or ended
--   code_hash          hex SHA-256 of the code under its own prefix; '' once
--                      the row is no longer pending
--   name               the label the device gave the passkey
--   credential_id ... user_handle, rp_id
--                      the credential as approver_credentials will hold it
--                      (kind passkey), and the relying party it was made for
--   created_at, pending_until
--                      when the device created it, and the deadline for its
--                      approval
--   enrolled_as        the approver_credentials id once approved
--   ended_at, ended_reason
--                      when and why it ended unapproved: reset
CREATE TABLE passkey_enrollments (
    id                 TEXT PRIMARY KEY,
    status             TEXT NOT NULL CHECK (status IN ('pending', 'enrolled', 'ended')),
    code_hash          TEXT NOT NULL DEFAULT '',
    name               TEXT NOT NULL,
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
    rp_id              TEXT NOT NULL DEFAULT '',
    created_at         TEXT NOT NULL,
    pending_until      TEXT NOT NULL,
    enrolled_as        TEXT NOT NULL DEFAULT '',
    ended_at           TEXT,
    ended_reason       TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_passkey_enrollments_code ON passkey_enrollments (code_hash, status);
-- One credential waits once: a second enrollment of the same passkey while the
-- first still waits is refused, so one approval can never enroll it twice.
CREATE UNIQUE INDEX idx_passkey_enrollments_pending ON passkey_enrollments (credential_id) WHERE status = 'pending';
