-- The session flag: a verdict an agent (or the owner) sets on a session that
-- went unusually well, unusually badly, or is worth re-reading for another
-- reason, and the transcript the flag keeps (session_update / session_end
-- verdict and reason; internal/transcripts).
--
-- Additive only: seven nullable columns on sessions and one partial index.
-- Nothing is dropped, renamed or retyped. The previous release names its
-- columns in every statement over sessions, so it reads and writes a database
-- this one migrated exactly as before, and its inserts leave the new columns
-- NULL.
--
--   transcript_path       the agent client's transcript file, verbatim as its
--                         hooks name it (every hook payload carries
--                         transcript_path), recorded at every SessionStart.
--                         NULL when no hook named one: a session from
--                         session_start without hooks, and every row from
--                         before this migration. A path only: under peer_auth
--                         the daemon never opens it.
--   verdict               good, bad or notable; NULL when not flagged. One
--                         flag per session, the last one set wins, and
--                         clearing it sets every column below back to NULL.
--   verdict_reason        why, one line, as the flag gave it; NULL for none.
--   flagged_at            when the flag was last set.
--   transcript_copy       what became of the copy: copied, or the fixed word
--                         saying why none was made (core.TranscriptCopies).
--                         NULL while none was attempted.
--   transcript_bytes      the transcript's size when it was last copied, so a
--                         Codex rollout is copied again only once it grew.
--   transcript_copied_at  when it was last copied.
--
-- Setting or clearing a flag never bumps updated_at: a flag is metadata, like
-- a star, and must not reorder recency sorts or the briefing's findings.
--
-- RESTORE, ROLLBACK, ARCHIVES. The copies live in <data_dir>/transcripts/,
-- outside the database and outside every archive (internal/archive carries
-- memory/ and notes/ only). A restored or merged database may therefore name
-- a copy that is not on this disk; every reader checks the file, never this
-- column alone.
ALTER TABLE sessions ADD COLUMN transcript_path TEXT;
ALTER TABLE sessions ADD COLUMN verdict TEXT CHECK (verdict IS NULL OR verdict IN ('good', 'bad', 'notable'));
ALTER TABLE sessions ADD COLUMN verdict_reason TEXT;
ALTER TABLE sessions ADD COLUMN flagged_at TEXT;
ALTER TABLE sessions ADD COLUMN transcript_copy TEXT;
ALTER TABLE sessions ADD COLUMN transcript_bytes INTEGER;
ALTER TABLE sessions ADD COLUMN transcript_copied_at TEXT;

CREATE INDEX IF NOT EXISTS idx_sessions_flagged ON sessions(flagged_at) WHERE verdict IS NOT NULL;
