-- Restricted local accounts (peer_auth.restricted): which account wrote what.
--
-- A restricted account is a second OS account whose agents work beside the
-- owner's, named on the peer_auth socket by the kernel. Every write it makes is
-- labeled with its account name, so the owner (and every agent) can tell its
-- knowledge from the owner's: a session records the account that started it
-- (immutable, like its id), a memory or note the account that wrote its
-- current content (frontmatter `account`, mirrored here), a task the account
-- that created it and the account that last changed it, a trial the account
-- that recorded it.
--
-- '' is the owner, background work, or peer_auth off: every row written before
-- this column, and every row a default install ever writes, carries ''.
ALTER TABLE sessions       ADD COLUMN account TEXT NOT NULL DEFAULT '';
ALTER TABLE memories_index ADD COLUMN account TEXT NOT NULL DEFAULT '';
ALTER TABLE notes_index    ADD COLUMN account TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks          ADD COLUMN account TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks          ADD COLUMN updated_account TEXT NOT NULL DEFAULT '';
ALTER TABLE trials         ADD COLUMN account TEXT NOT NULL DEFAULT '';
