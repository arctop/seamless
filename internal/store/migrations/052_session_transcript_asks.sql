-- The session flag's pending transcripts: how often a flagged session holding
-- no transcript copy was asked for it again (internal/transcripts).
--
-- Under peer_auth the daemon reads no agent file, so a flagged session whose
-- end signal never sent its transcript (a crash, a closed terminal, a flag set
-- after the end) stays pending (sessions.transcript_copy 'pending'): a later
-- hook of the same agent client, account and computer is asked for it, with a
-- growing wait between asks, and after core.TranscriptAskLimit asks the copy
-- is given up on ('not_sent'). The asks are counted here, one row per session
-- asked about: bookkeeping only the pending logic reads, so a table of its own
-- rather than columns every session read would scan, and left out of a merge
-- (internal/archive merges named tables only).
--
-- Additive only: one new table. Nothing on sessions changes. The previous
-- release never reads it, and reads the new transcript_copy words (pending,
-- refused, not_sent) as an unknown word, which its surfaces print as
-- "no copy: <word>". IF NOT EXISTS: a database whose migration record was cut
-- back below this one still migrates forward.
--
--   session_id  the session asked about (sessions.id).
--   asks        how many times since it was last flagged; a new flag, and
--               clearing the flag, delete the row.
--   asked_at    when last.
CREATE TABLE IF NOT EXISTS session_transcript_asks (
    session_id TEXT PRIMARY KEY,
    asks       INTEGER NOT NULL,
    asked_at   TEXT NOT NULL
);
