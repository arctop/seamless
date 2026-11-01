-- Write screening, part 2 (the screening optional feature): the corpus
-- record behind drift detection, the review's trust mark, and the drift
-- quarantine.
--
-- One new table and nothing else: no existing table changes shape, so the
-- previous release reads a database this one migrated exactly as before. It
-- stays empty until the owner first turns the feature on; that first turn-on
-- runs the review below, which is what fills it.
--
-- corpus_records holds one row per memory or note file, keyed by its path
-- under the data dir:
--
--   hash            the content hash of the file as Seamless last wrote it
--                   (files.Manager records every write while the watch is
--                   armed) or as the review last read it. A file whose bytes
--                   no longer hash to it changed outside Seamless: drift.
--   via             how hash was recorded: write, review or resync
--   review_status   '' (never reviewed), safe (the screens found nothing that
--                   holds, or the owner marked it safe) or flagged
--   review_digest   core.MemoryDigest of the memory the review judged (a
--                   note's file hash): the safe mark holds only while the
--                   item still digests the same, so any later change lapses it
--   review_verdicts JSON array of the verdict codes the screens found
--   review_by       review (the deterministic pass) or owner (marked safe)
--   drift_status    '' or drifted: the file changed outside Seamless, or
--                   appeared without Seamless writing it. A drifted item stays
--                   on disk untouched and is kept out of briefing and prompt
--                   injection until the owner accepts it.
--   drift_change    changed (the file differs from hash) or unknown (no
--                   Seamless write is on record for it)
--   drift_hash      the hash of the drifted bytes the screens last read
--   drift_base      hash as it stood when the drift was found: a file put back
--                   to exactly these bytes is no longer drifted
--   drift_verdicts  JSON array of the verdict codes the screens found in the
--                   drifted text
CREATE TABLE corpus_records (
    file_path       TEXT PRIMARY KEY,
    item_kind       TEXT NOT NULL CHECK (item_kind IN ('memory', 'note')),
    item_id         TEXT NOT NULL DEFAULT '',
    project_slug    TEXT NOT NULL DEFAULT '',
    hash            TEXT NOT NULL DEFAULT '',
    recorded_at     TEXT NOT NULL,
    via             TEXT NOT NULL DEFAULT '',
    review_status   TEXT NOT NULL DEFAULT '' CHECK (review_status IN ('', 'safe', 'flagged')),
    review_digest   TEXT NOT NULL DEFAULT '',
    review_verdicts TEXT NOT NULL DEFAULT '[]',
    reviewed_at     TEXT NOT NULL DEFAULT '',
    review_by       TEXT NOT NULL DEFAULT '',
    drift_status    TEXT NOT NULL DEFAULT '' CHECK (drift_status IN ('', 'drifted')),
    drift_change    TEXT NOT NULL DEFAULT '',
    drift_hash      TEXT NOT NULL DEFAULT '',
    drift_base      TEXT NOT NULL DEFAULT '',
    drift_verdicts  TEXT NOT NULL DEFAULT '[]',
    drifted_at      TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_corpus_records_item ON corpus_records(item_id);
CREATE INDEX idx_corpus_records_review ON corpus_records(review_status);
CREATE INDEX idx_corpus_records_drift ON corpus_records(drift_status);
