package hooks

// The token harvest's writes: the usage ledger (store ledger.go) is the token
// record, and every harvest writes it, on every install, from the read the
// hook already makes -- the transcript at Claude Code's SessionEnd, the
// rollout's cumulative counter at each Codex Stop, or what `seam hook` pushed
// from the same files under peer_auth. Token counts are metadata: they land
// on the conversation's ambient row (and the sessions its moves ended)
// wherever its findings go, and on a row the agent or the reaper already
// ended, without reviving it or moving the time it ended at.

import (
	"context"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
)

// harvestEndUsage records a Claude Code end's token usage on the
// conversation: the rows `seam hook` pushed (push non-nil), or its totals
// alone when it pushed no rows (an older client, a transcript past the cap),
// credited under the model it sniffed; else, when local, the transcript's own
// rows. The caller has judged the reach and the disk (metadataOnly,
// localDiskOK). An end that recorded the usage read the whole conversation,
// so none of it is owed any more (usage_owed.go).
func (h *Handler) harvestEndUsage(ctx context.Context, client Client, externalSessionID, transcriptPath string,
	push *harvest.Push, local bool, now time.Time) {
	wrote := false
	switch {
	case push != nil && len(push.Usage) > 0:
		wrote = h.replaceUsage(ctx, client, externalSessionID, push.Usage, now)
	case push != nil && push.Tokens != nil:
		wrote = h.creditUsage(ctx, client, externalSessionID, store.UsageCredit{Cumulative: *push.Tokens, Model: push.Model, At: now})
	case push == nil && local:
		if _, rows, ok := claudeTranscriptUsage(transcriptPath); ok {
			wrote = h.replaceUsage(ctx, client, externalSessionID, rows, now)
		}
	}
	if wrote {
		if err := store.ClearConversationUsageOwed(ctx, h.db, client.externalIdentity(), externalSessionID); err != nil {
			h.recordHookError(ctx, "usage-owed-clear", client, err, "external_session_id", externalSessionID)
		}
	}
}

// usageAfterEnd is a SessionEnd's token harvest for a conversation none of
// whose sessions is active any more: the agent ended its own session with
// session_end, or the idle reaper expired it, before its client ended. The
// counts land on that ambient row as they would have on an active one. Only
// such a row is harvested for: an end that names a conversation with no
// ambient row reads nothing, and so records no skipped capture for it.
func (h *Handler) usageAfterEnd(ctx context.Context, client Client, p endPayload, metadataOnly bool) {
	if metadataOnly {
		return // a locked project's session takes no counts until the owner approves
	}
	if _, found, err := store.AmbientSessionByExternalIdentity(ctx, h.db, client.externalIdentity(), p.SessionID); err != nil {
		h.recordHookError(ctx, "session-end-usage-lookup", client, err)
		return
	} else if !found {
		return
	}
	push := p.Capture
	local := push == nil && h.localDiskOK(ctx, "session-end-transcript", client, h.hostOf(p.Identity))
	h.harvestEndUsage(ctx, client, p.SessionID, p.TranscriptPath, push, local, time.Now().UTC())
}

// creditCodexTurn credits one Codex turn's growth of the rollout's cumulative
// counter to the bucket the Stop arrived in, under the model the Stop stated
// ("" for the session's own).
func (h *Handler) creditCodexTurn(ctx context.Context, externalSessionID string, cumulative core.TokenUsage, model string, now time.Time) {
	h.creditUsage(ctx, ClientCodex, externalSessionID, store.UsageCredit{Cumulative: cumulative, Model: model, At: now, Turn: true})
}

// replaceUsage replaces the conversation's ledger rows with a transcript's,
// reporting whether they landed (the conversation has an ambient row).
func (h *Handler) replaceUsage(ctx context.Context, client Client, externalSessionID string, rows []core.UsageBucket, now time.Time) bool {
	found, err := store.ReplaceConversationUsage(ctx, h.db, client.externalIdentity(), externalSessionID, rows, now)
	if err != nil {
		h.recordHookError(ctx, "session-end-token-harvest", client, err, "external_session_id", externalSessionID)
		return false
	}
	return found
}

// creditUsage credits a running total's growth to the conversation's ledger,
// reporting whether a credit was written.
func (h *Handler) creditUsage(ctx context.Context, client Client, externalSessionID string, c store.UsageCredit) bool {
	stage := "session-end-token-harvest"
	if client == ClientCodex {
		stage = "codex-stop-token-harvest"
	}
	wrote, err := store.CreditConversationUsage(ctx, h.db, client.externalIdentity(), externalSessionID, c)
	if err != nil {
		h.recordHookError(ctx, stage, client, err, "external_session_id", externalSessionID)
		return false
	}
	return wrote
}
