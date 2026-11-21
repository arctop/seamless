-- Task body revisions: every body a tasks_update replaces, or appends to, is
-- kept, with when and by which session (store/task_revisions.go).
--
-- A task's body holds its notes and its acceptance criteria, and tasks_update's
-- body replaces it whole. Agents that meant to add a line wiped what was there
-- (5 times in a review of 287 sessions). Now the body a change replaces is kept
-- here, in the transaction that changes it, and tasks_update's body_append
-- adds a line in that same transaction, so two appends cannot lose one.
--
-- One row per task and revision. A row holds the body as it was, so it is the
-- task's content and is as visible as its task, and no more: the console's
-- filtered handle hides it with its task (store/view.go), and the MCP surface
-- reads it only through tasks_list id=<id>, behind the task's own fence.
--
--   task_id          the task (tasks.id). The foreign key removes a task's
--                    revisions with its row: no store API deletes a task, and
--                    a row removed by hand takes its revisions along.
--   revision         1, 2, 3 ... per task, in the order the changes landed.
--                    Never reused: a gap is a revision retention removed.
--   body             the body before the change. Never empty: replacing an
--                    empty body keeps nothing.
--   change           replace (tasks_update body) or append (body_append: the
--                    body after it starts with this one).
--   replaced_at      when the change landed.
--   replaced_by      the session ULID of the call that made the change ('' when
--                    the call had no session), as commit_records.recorded_by.
--   updated_account  the task's updated_account when the change landed: the
--                    restricted account whose words this body may carry; ''
--                    for the owner.
--
-- Retention: a task keeps at most store.TaskBodyRevisionsKept revisions. The
-- write that adds one past the cap removes, in its transaction, the oldest one
-- an append made (the body after it still holds its text), else the oldest
-- one, and never the newest.
--
-- Additive only: one new table and nothing else. No existing table changes
-- shape, so the previous release reads and writes a database this one migrated
-- exactly as before: it never reads this table, and its body changes keep no
-- revision. IF NOT EXISTS: a database whose migration record was cut back below
-- this one still migrates forward.
CREATE TABLE IF NOT EXISTS task_body_revisions (
    task_id         TEXT    NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    revision        INTEGER NOT NULL CHECK (revision > 0),
    body            TEXT    NOT NULL CHECK (body <> ''),
    change          TEXT    NOT NULL CHECK (change IN ('replace', 'append')),
    replaced_at     TEXT    NOT NULL,
    replaced_by     TEXT    NOT NULL DEFAULT '',
    updated_account TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (task_id, revision)
);
