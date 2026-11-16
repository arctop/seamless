-- Repo locks (plan repo-locks, step RL2): a lock guards a repository's refs --
-- its main branch, or a tag pattern -- so agents on several machines land
-- changes one at a time.
--
-- A lock belongs to a repository, not to a project: project mappings are path
-- prefixes, sessions of several projects work in one repository, and a lock a
-- project owned could not be unique. So there is no project column, and no free
-- text: a lock's state is its resource, its holder, since and until. The label
-- every surface shows ("LOCK seamless main") is derived from the repository's
-- basename (its first alias) and the name.

-- One row per lock. refs is a JSON array of the guarded refs, each a full ref
-- name (refs/heads/main) or a pattern with one '*' (refs/tags/v*), validated by
-- core.ValidRepoLockRef. state is 'requested' (a restricted account's
-- declaration, waiting for the owner) or 'active'. holder is the holding
-- session's ULID, '' when free; held_since and lease_expires_at are NULL when
-- free. A hold is live only while lease_expires_at > now (core.RepoLock.HoldLive,
-- Task.ClaimLive's rule). created_by is the declaring session's ULID ('' when
-- the declaration named none) and account the restricted account that declared
-- it ('' for the owner principal).
CREATE TABLE repo_locks (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL,
    refs             TEXT NOT NULL,
    state            TEXT NOT NULL CHECK (state IN ('requested', 'active')),
    holder           TEXT NOT NULL DEFAULT '',
    held_since       TEXT,
    lease_expires_at TEXT,
    created_by       TEXT NOT NULL DEFAULT '',
    account          TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

-- idx_repo_locks_holder: a session's holds, read with every session row (the
-- liveness rule's holds, store sessionHolds) and by the releases at session
-- end and by the reaper.
CREATE INDEX idx_repo_locks_holder ON repo_locks(holder);

-- The repository identities that resolve to a lock. kind 'origin' is the
-- normalized origin URL (store.normalizeOrigin), so the ssh and https forms of
-- one repository are one alias; kind 'path' is "<host>:<main checkout root>",
-- the fallback for a repository with no origin, recorded for every checkout
-- seen too, so a repository that gains or changes its origin keeps its lock.
-- name is the lock's own, copied, so that UNIQUE (alias, name) makes one lock
-- per repository identity and name: a second declaration of the same origin
-- and name resolves to the first lock instead of making another.
CREATE TABLE repo_lock_aliases (
    lock_id    TEXT NOT NULL,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('origin', 'path')),
    alias      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (alias, name)
);

CREATE INDEX idx_repo_lock_aliases_lock ON repo_lock_aliases(lock_id);

-- The projects whose restricted accounts a declaration lists as holders of
-- the lock (by default the project the declaring checkout maps to). A table
-- rather than a column on repo_locks, so the console's filtered view can hide a
-- hidden project's row by its slug, as it hides repo_map's.
CREATE TABLE repo_lock_projects (
    lock_id      TEXT NOT NULL,
    project_slug TEXT NOT NULL,
    PRIMARY KEY (lock_id, project_slug)
) WITHOUT ROWID;
