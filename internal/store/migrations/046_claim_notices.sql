-- Claim notices: a session that lost a claim learns it once, on its next hook
-- injection or Seamless tool result.
--
-- claim_notices holds one row per lost claim, written in the transaction that
-- took the claim away: another session claiming it after its lease lapsed
-- (store.ClaimTask's reclaim), the owner releasing it (store.ForceReleaseTask),
-- or the reaper releasing it from a session that went idle once its lease had
-- lapsed (store.ReleaseReapedClaims).
--
--   session_id        the session that held the claim, whom the notice is for
--   resource_kind     what the claim was on: 'task'
--   resource_id       its id
--   resource_label    a short identifier the holder knew it by beside its id,
--                     '' when the id is its whole name (a task); never free text
--   project_slug      its project ('' the global scope)
--   new_holder        the session holding it now, '' when none does
--   reason            how it was lost: reclaimed | released | reaped
--   lease_expires_at  the lost claim's lease, NULL when it carried none
--   lost_at           when it was lost
--   delivered_at      when a hook injection or tool result carried the notice;
--                     NULL until then. The row is the claim
--                     (store.DeliverClaimNotices), as access_moves.briefed_at is
--                     for a move's briefing: a restart does not deliver it
--                     again, and of two concurrent deliveries one carries it.
--
-- idx_claim_notices_pending: the pending notices of a session, read on every
-- tool call and every prompt and session-start hook.
--
-- Additive only: a new table nothing older reads.
CREATE TABLE claim_notices (
    id               TEXT PRIMARY KEY,
    session_id       TEXT NOT NULL,
    resource_kind    TEXT NOT NULL,
    resource_id      TEXT NOT NULL,
    resource_label   TEXT NOT NULL DEFAULT '',
    project_slug     TEXT NOT NULL DEFAULT '',
    new_holder       TEXT NOT NULL DEFAULT '',
    reason           TEXT NOT NULL,
    lease_expires_at TEXT,
    lost_at          TEXT NOT NULL,
    delivered_at     TEXT
);

CREATE INDEX idx_claim_notices_pending ON claim_notices (session_id) WHERE delivered_at IS NULL;
