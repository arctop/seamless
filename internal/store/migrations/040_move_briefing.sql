-- Fenced sessions, the MCP edge (plan fenced-sessions P4; isolation.requests,
-- off by default): the first tool result after an approved move announces it
-- and carries the new home's briefing, once.
--
-- access_moves.briefed_at: when a tool result first carried the briefing of the
-- project a move entered; NULL until then. It is access_leases.briefed_at's
-- pattern (migration 036): the row is the claim (store.ClaimMoveBriefing), so a
-- daemon restart does not send the one-time briefing again, and of two
-- concurrent calls one carries it. A restore keeps it with the move it belongs
-- to.
--
-- idx_access_moves_child: the move that opened a session (store.MoveIntoSession),
-- read once per moved conversation and daemon run.
--
-- Additive only: one nullable column nothing older reads (the previous release
-- names access_moves' columns in every statement) and one index. The table and
-- the column stay empty while isolation.requests is off.
ALTER TABLE access_moves ADD COLUMN briefed_at TEXT;

CREATE INDEX idx_access_moves_child ON access_moves (to_session_id);
