-- Fenced sessions (plan fenced-sessions; isolation.requests, off by default).
-- A conversation working in an open project (or global) that reaches a
-- confidential one moves into it, with the owner's approval, for good; any
-- conversation reaches a sealed project only by moving in; between confidential
-- projects it takes grants, read-only or read-and-write. Every approval states
-- the whole carry (the home, the fenced projects the conversation holds, where
-- it may write afterwards), and approving clears each flow it lists. This
-- migration is the data only: what judges it is store.CanWrite through
-- internal/access (the one rule), and nothing in this file decides anything.
--
-- Additive only: four columns on access_approvals, three on access_grants, one
-- on sessions, three new tables and their indexes. Nothing is dropped, renamed
-- or retyped, and no existing CHECK changes. The previous release names its
-- columns in every statement over sessions, access_approvals and access_grants,
-- so it reads and writes a database this one migrated exactly as before, and
-- its inserts take the defaults below. Every new column keeps its default, and
-- every new table stays empty, while isolation.requests is off. All of it is
-- metadata, never corpus content: slugs, ids, holder keys, the kernel's facts
-- and times. Times are core.TimeFormat (fixed width, UTC), so a text comparison
-- is a chronological one.
--
-- THE HOLDER. Who an approval, a grant, a move or a flow belongs to is the
-- conversation, in the one form the request's transport vouches for. The store
-- has one encoding for it (store.Holder, internal/store/holder.go), a key in one
-- of three kinds:
--
--   process:<key>        under peer_auth: access.ProcessToken.Key of the agent
--                        process the request was anchored to; its whole
--                        kernel-verified tree shares it, for the process's life.
--                        Today's process_key holder, unchanged.
--   conversation:<client>/<external session id>
--                        without peer_auth, for a client that sends its
--                        agent-process identity: the client's own session
--                        (sessions.external_client + sessions.claude_session_id),
--                        found from the agent-process identity and never from
--                        the connection's binding, so neither session_start nor
--                        session= nor a move changes it. /clear starts a new one.
--   session:<ULID>       a client without hooks: today's session holder.
--
-- The new tables carry the key in a holder column. access_approvals and
-- access_grants keep process_key and session_id and gain the conversation's two
-- halves; a row's holder is derived from the three in that order (process_key
-- when set, else the conversation when set, else session_id), so a row the
-- previous release writes, which never names a conversation, keeps the holder it
-- meant. A conversation's grant still names its asking session: the CHECK of
-- migration 034 needs one.
--
-- RESTORE AND ROLLBACK. store.ResetRestoredAccess ends every live permission a
-- restored database brings back, because the owner may have closed it since the
-- snapshot: leases, console unlocks and grants (read and write alike). Moves,
-- flows and provenance stamps are not live permissions in that sense. They are
-- history: no owner action ends one (revocation and expiry never delete them,
-- and a label only grows), so a restore cannot reopen anything by keeping them,
-- and ending one would only shrink a label or strip a warning, the one direction
-- a restore must never move. A restore keeps them as they stood in the snapshot.
-- Rows written after the snapshot are not in it, and a restore does not carry
-- them over from the database it replaces (grants fare the same today), so a
-- conversation still running across a restore can lose label entries it earned
-- after the snapshot was taken.
-- An update rollback from this release puts back a snapshot taken before this
-- migration ran: these tables are absent there (ResetRestoredAccess skips a
-- missing table), the rows the failed release wrote go with it, and the previous
-- release judges no label at all.
--
-- ARCHIVE MERGE (internal/archive/merge.go). access_moves and access_flows are
-- not merged: like access_grants they belong to this machine's conversations
-- and approvals, and a merged one would be a clearance nobody approved here.
-- item_provenance is merged: a stamp is a warning, never a permission, and it
-- rides with the tasks and sessions it stamps as commit_records does.
--
-- sessions.parent_session_id: the session a move's child continues (the
-- session the conversation left); NULL for every other session. Set once, at
-- creation. No foreign key: a merged child whose parent the merge skipped keeps
-- a dangling pointer, which a reader treats as "parent not here".
ALTER TABLE sessions ADD COLUMN parent_session_id TEXT;

CREATE INDEX idx_sessions_parent ON sessions (parent_session_id)
    WHERE parent_session_id IS NOT NULL;

-- A conversation's sessions, every status: its ambient row, the explicit
-- sessions that linked to it (session_start) -- which the record routing reads
-- at every SessionEnd, Codex Stop with findings and plan or subagent capture,
-- newest first, to send the conversation's records where it bound itself --
-- and a move's children (store.HolderLabel). The ambient identity index of
-- migration 010 covers ambient rows only, so those lookups scanned the table.
-- It is not partial on purpose: a lookup by a bound id cannot prove a
-- "claude_session_id <> ''" condition, and SQLite then ignores the index.
-- created_at and id make the newest-first order the index's own.
CREATE INDEX idx_sessions_conversation ON sessions (external_client, claude_session_id, created_at, id);

-- access_approvals: a move request is a row of kind 'move' (store.AccessKindMove,
-- kept outside store.AccessKinds as a grant's 'grant' is), approved only by
-- store.MoveApprove, never as a lease or a grant. For a move, project_slug is the
-- fenced project to move into and from_project the home it leaves ('' for
-- global).
--
--   requested_access     a grant request's access: 'read' or 'write'. A lock's
--                        request and a move keep 'read', which they do not use.
--   carry                what the approval states, as JSON (store.Carry):
--                        {"home": ..., "holds": [...], "write_to": [...]} --
--                        the home, every fenced project the conversation holds
--                        once approved, and every project it may write to once
--                        approved. Approving records a flow from each held
--                        project into each write destination. '' for a lock's
--                        request and every row from before this migration.
--   conversation_client, conversation_id
--                        the asking conversation (see THE HOLDER), both '' or
--                        both set
ALTER TABLE access_approvals ADD COLUMN requested_access TEXT NOT NULL DEFAULT 'read'
    CHECK (requested_access IN ('read', 'write'));
ALTER TABLE access_approvals ADD COLUMN carry TEXT NOT NULL DEFAULT ''
    CHECK (carry = '' OR json_valid(carry));
ALTER TABLE access_approvals ADD COLUMN conversation_client TEXT NOT NULL DEFAULT '';
ALTER TABLE access_approvals ADD COLUMN conversation_id TEXT NOT NULL DEFAULT ''
    CHECK ((conversation_client = '') = (conversation_id = ''));

CREATE INDEX idx_access_approvals_conversation
    ON access_approvals (conversation_client, conversation_id, project_slug, status)
    WHERE conversation_id <> '';

-- access_grants:
--
--   access               what the owner approved: 'read', or 'write' (read and
--                        write, between confidential projects). Every row from
--                        before this migration, and every one the previous
--                        release writes, is 'read'.
--   conversation_client, conversation_id
--                        the conversation it was granted to (see THE HOLDER),
--                        both '' or both set
ALTER TABLE access_grants ADD COLUMN access TEXT NOT NULL DEFAULT 'read'
    CHECK (access IN ('read', 'write'));
ALTER TABLE access_grants ADD COLUMN conversation_client TEXT NOT NULL DEFAULT '';
ALTER TABLE access_grants ADD COLUMN conversation_id TEXT NOT NULL DEFAULT ''
    CHECK ((conversation_client = '') = (conversation_id = ''));

CREATE INDEX idx_access_grants_conversation
    ON access_grants (conversation_client, conversation_id, expires_at)
    WHERE conversation_id <> '';

-- access_moves holds one row per approved move: the conversation left one home
-- for a fenced project, for good. A move never ends: revocation and expiry
-- touch leases and grants, never this table.
--
--   id               ULID
--   approval_id      the move request it came from (one move per approval)
--   holder           the conversation that moved (a store.Holder key)
--   from_session_id  the session it left, which ended with a pointer
--   to_session_id    the child session it continues in (its parent_session_id
--                    is from_session_id)
--   from_project     the home it left ('' for global)
--   to_project       the fenced project it moved into, its home from then on
--   decided_by       the proof that decided it: an approver credential id, the
--                    daemon's account, or bearer-key (advisory, without
--                    peer_auth)
--   moved_at         when it was approved
CREATE TABLE access_moves (
    id              TEXT PRIMARY KEY,
    approval_id     TEXT NOT NULL REFERENCES access_approvals (id),
    holder          TEXT NOT NULL,
    from_session_id TEXT NOT NULL,
    to_session_id   TEXT NOT NULL,
    from_project    TEXT NOT NULL,
    to_project      TEXT NOT NULL,
    decided_by      TEXT NOT NULL,
    moved_at        TEXT NOT NULL,
    CHECK (holder <> '' AND decided_by <> ''),
    CHECK (to_project <> '' AND from_project <> to_project),
    CHECK (from_session_id <> '' AND to_session_id <> '' AND from_session_id <> to_session_id)
);

CREATE UNIQUE INDEX idx_access_moves_approval ON access_moves (approval_id);
CREATE INDEX idx_access_moves_holder ON access_moves (holder, moved_at);

-- access_flows holds the flows approvals cleared, one row per holder and pair:
-- content of from_project may be written into to_project. A later approval
-- that lists a flow already recorded adds nothing (the first approval stays
-- named). A grant row implies its own flows (its target into its asking
-- project, and for a write grant the asking project into the target) without a
-- row here; store.HolderFlows returns both. Never deleted.
--
--   id            ULID
--   approval_id   the approval that listed it
--   holder        the conversation it was cleared for (a store.Holder key)
--   from_project  whose content (never the global scope)
--   to_project    where it may be written (never the global scope)
--   created_at    when the approval recorded it
CREATE TABLE access_flows (
    id           TEXT PRIMARY KEY,
    approval_id  TEXT NOT NULL REFERENCES access_approvals (id),
    holder       TEXT NOT NULL,
    from_project TEXT NOT NULL,
    to_project   TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    CHECK (holder <> ''),
    CHECK (from_project <> '' AND to_project <> '' AND from_project <> to_project),
    UNIQUE (holder, from_project, to_project)
);

-- item_provenance is the index behind provenance stamps: item item_id of kind
-- item_kind, in project project_slug, may contain content from source_project,
-- cleared by approval approval_id. Memories and notes also carry their stamp in
-- frontmatter; tasks and session findings only here. A stamp is set from the
-- writing conversation's state, never from agent input, and names projects and
-- approvals, never content. One stamp per item and source: stamping again keeps
-- the first approval and refreshes the item's project. Never deleted by a
-- revocation or an expiry.
--
--   item_kind       core.ItemKind*: memory, note, task or session
--   item_id         the item's id
--   project_slug    the item's project ('' for global)
--   source_project  the fenced project whose content it may contain
--   approval_id     the grant or move approval that cleared the write (no
--                   foreign key: a merge carries stamps without approvals)
--   created_at      when it was first stamped
CREATE TABLE item_provenance (
    item_kind      TEXT NOT NULL,
    item_id        TEXT NOT NULL,
    project_slug   TEXT NOT NULL,
    source_project TEXT NOT NULL,
    approval_id    TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    PRIMARY KEY (item_kind, item_id, source_project),
    CHECK (item_kind <> '' AND item_id <> '' AND approval_id <> ''),
    CHECK (source_project <> '' AND source_project <> project_slug)
);

CREATE INDEX idx_item_provenance_project ON item_provenance (project_slug, source_project);
CREATE INDEX idx_item_provenance_source ON item_provenance (source_project);
