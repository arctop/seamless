package hooks

// Claim notices at the hooks: a claim the conversation's sessions lost --
// another session took it once its lease lapsed, the owner released it, or the
// reaper released it after the lease lapsed (store.RecordClaimNotice) -- leads
// the next prompt's injection or the next session start's briefing, once
// (store.DeliverClaimNotices). The MCP edge delivers the same notices in a tool
// result (internal/mcp/claim_notice.go); whichever reaches the session first
// carries each one.

import (
	"context"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/store"
)

// claimNotice is the one-time notice of the claims a conversation lost: its
// ambient session's, and those of every explicit session session_start linked
// to it, whatever their status (the reaper releases a session's lapsed claims
// as it expires it, and that session is the one its agent comes back to). It
// is "" when none is pending, when the hook names no session, or when the
// session is not this caller's own to brief (reach other than reachOK: another
// principal's, or one in a project locked to the caller).
func (h *Handler) claimNotice(ctx context.Context, client Client, externalID string, reach sessionReach) string {
	if h.db == nil || externalID == "" || reach != reachOK {
		return ""
	}
	ids := h.conversationSessionIDs(ctx, client, externalID)
	if len(ids) == 0 {
		return ""
	}
	notices, err := store.DeliverClaimNotices(ctx, h.db, ids, h.now())
	if err != nil {
		// Nothing was claimed: the next injection or tool result carries them.
		h.recordHookError(ctx, "claim-notice", client, err)
		return ""
	}
	return strings.Join(store.ClaimNoticeTexts(ctx, h.db, notices), "\n")
}

// conversationSessionIDs returns the ids of a client's conversation's sessions
// that the caller may act in (access.SessionAccountOK): the ambient row and the
// explicit sessions linked to it, in any status. A failed lookup is recorded and
// answers what it found so far.
func (h *Handler) conversationSessionIDs(ctx context.Context, client Client, externalID string) []string {
	var ids []string
	ambient, found, err := store.AmbientSessionByExternalIdentity(ctx, h.db, client.externalIdentity(), externalID)
	if err != nil {
		h.recordHookError(ctx, "claim-notice-sessions", client, err)
		return nil
	}
	if found && access.SessionAccountOK(ctx, ambient.Account) {
		ids = append(ids, ambient.ID)
	}
	linked, err := store.LinkedSessionsByExternalIdentity(ctx, h.db, client.externalIdentity(), externalID)
	if err != nil {
		h.recordHookError(ctx, "claim-notice-sessions", client, err)
		return ids
	}
	for _, sess := range linked {
		if access.SessionAccountOK(ctx, sess.Account) {
			ids = append(ids, sess.ID)
		}
	}
	return ids
}

// joinNotices joins the non-empty notices, one per line.
func joinNotices(notices ...string) string {
	var kept []string
	for _, n := range notices {
		if n != "" {
			kept = append(kept, n)
		}
	}
	return strings.Join(kept, "\n")
}
