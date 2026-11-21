-- Task gates: a step that waits on a person or an event outside Seamless.
--
-- A task's gate names who must act (gate_who: owner, reviewer:<name>,
-- sitting:<name>, hardware, sealed:<project>, or free text) and what must
-- happen (gate_what), optionally the open question the person answers
-- (gate_decision) and when the step is ready (gate_ready_when). The gate is
-- open while gate_who is set and gate_cleared_at is NULL; clearing it stamps
-- gate_cleared_at, gate_cleared_by and gate_note and leaves the task's status
-- alone. A step with an open gate is not ready.
--
-- kind is 'step' (work an agent claims and finishes) or 'gate' (a person's
-- action that blocks the steps depending on it: never ready, never claimed;
-- clearing it closes it). Every row written before this migration, and every
-- row a previous release inserts, is a step with no gate.
ALTER TABLE tasks ADD COLUMN kind            TEXT NOT NULL DEFAULT 'step';
ALTER TABLE tasks ADD COLUMN gate_who        TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN gate_what       TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN gate_decision   TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN gate_ready_when TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN gate_cleared_at TEXT;
ALTER TABLE tasks ADD COLUMN gate_cleared_by TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN gate_note       TEXT NOT NULL DEFAULT '';
