-- Requests through a relay (plan networked-relay): a relay on another computer
-- forwards the uid and the process chain its own kernel reported for the
-- local caller, and under peer_auth a relay peer_auth.relays maps is judged as
-- this daemon's own socket judges a request. Its leases and grants are bound
-- to (the relay, the process): every process key a relay's request carries
-- names the relay ("relay/<relay id>/<boot>:<pid>:<version>",
-- access.ProcessToken.Key), so a lease, grant or request of one relay's
-- process never matches another relay's or this machine's, in any lookup,
-- however its pid, start time and boot compare.
--
-- Additive only: one column on access_leases and one on access_grants, and an
-- index on each. Nothing is dropped, renamed or retyped, and no CHECK changes.
-- The previous release names its columns in every statement over both tables,
-- so it reads and writes a database this one migrated exactly as before, and
-- its inserts take the default. Every row keeps the default while no relay is
-- mapped. Metadata only: a device id.
--
--   relay_id   the relay whose process the lease or grant was granted to:
--              the device id (devices.id) its process key names, written by
--              the table's one writer (store.leaseInsertTx, the grant approve
--              transaction) from that key. '' is this machine's socket, and
--              every row from before this migration. Revoking the relay ends
--              its live leases and grants in the revocation's transaction
--              (store.RevokeDeviceEndingAccess), by this column.
--
-- RESTORE AND ROLLBACK. Nothing new: store.ResetRestoredAccess ends every
-- live lease and grant a restored database brings back, a relay's included,
-- and a backup restore keeps today's devices (keptTables). An update rollback
-- to the previous release leaves the column in place, unread.
--
-- ARCHIVE MERGE. access_leases and access_grants are never merged
-- (internal/archive/merge.go): a relay's are bound to a device the exporting
-- server paired, which a merge does not carry either.
ALTER TABLE access_leases ADD COLUMN relay_id TEXT NOT NULL DEFAULT '';
ALTER TABLE access_grants ADD COLUMN relay_id TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_access_leases_relay ON access_leases (relay_id, expires_at) WHERE relay_id <> '';
CREATE INDEX idx_access_grants_relay ON access_grants (relay_id, expires_at) WHERE relay_id <> '';
