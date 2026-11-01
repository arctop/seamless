-- Write screening (the screening optional feature, internal/screening).
--
-- Three new tables and nothing else: no existing table changes shape, so the
-- previous release reads a database this one migrated exactly as before. All
-- three stay empty while the feature is off.

-- held_writes is the owner's queue of writes kept OUT of the corpus: a write
-- that failed a screen, a write of a briefing-injected kind by a session marked
-- as having read outside text, and any supersede or delete of a constraint.
-- The held text lives here and not under memory/ or notes/, which is what
-- keeps it out of every briefing, recall, injection and gardener pass by
-- construction: a reconcile or watcher rebuild indexes only those two trees,
-- so no rebuild can un-hold an item. Releasing applies the write through the
-- files layer; discarding erases the text and keeps the row as the record.
--
--   op        write (a memory or note lands at detail.path), delete (a memory
--             leaves the disk), gardener (a gardener proposal is applied)
--   item_id   the memory or note the write lands on (minted for a create)
--   base_hash the content hash of the file the write replaces, '' when it
--             would create one; a release refuses when the file has moved on
--   content   the full markdown file the write would land ('' for delete and
--             gardener ops, and after the row is resolved)
--   reasons   JSON array of the verdict and reason codes that held it
--   detail    JSON object, op-specific (path, from_path, supersedes_id, proposal)
CREATE TABLE held_writes (
    id           TEXT PRIMARY KEY,
    created_at   TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'held' CHECK (status IN ('held', 'released', 'discarded')),
    resolved_at  TEXT NOT NULL DEFAULT '',
    op           TEXT NOT NULL CHECK (op IN ('write', 'delete', 'gardener')),
    tool         TEXT NOT NULL DEFAULT '',
    item_kind    TEXT NOT NULL CHECK (item_kind IN ('memory', 'note')),
    item_id      TEXT NOT NULL DEFAULT '',
    project_slug TEXT NOT NULL DEFAULT '',
    name         TEXT NOT NULL DEFAULT '',
    memory_kind  TEXT NOT NULL DEFAULT '',
    session_id   TEXT NOT NULL DEFAULT '',
    reasons      TEXT NOT NULL DEFAULT '[]',
    base_hash    TEXT NOT NULL DEFAULT '',
    content      TEXT NOT NULL DEFAULT '',
    detail       TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX idx_held_writes_status ON held_writes(status, created_at);
CREATE INDEX idx_held_writes_item ON held_writes(item_id, status);

-- owner_confirmations records the memories whose content the owner confirmed
-- (today by releasing a held write of it; via names how). digest is
-- core.MemoryDigest of the content confirmed: the mark holds only while the
-- memory still digests the same, so any later change lapses it on its own.
CREATE TABLE owner_confirmations (
    item_id      TEXT PRIMARY KEY,
    digest       TEXT NOT NULL,
    confirmed_at TEXT NOT NULL,
    via          TEXT NOT NULL DEFAULT ''
);

-- session_taints marks the sessions an agent guard reported as having read
-- outside text (a web page, a mail, a fetched file). Setting the mark only
-- lowers trust, so the session's own hook may set it; only the owner clears it.
CREATE TABLE session_taints (
    session_id TEXT PRIMARY KEY,
    tainted_at TEXT NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    via        TEXT NOT NULL DEFAULT ''
);
