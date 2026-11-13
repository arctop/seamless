package hooks

import (
	"context"
	"fmt"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/store"
)

// captureConfined is the refusal of a plan or subagent capture into target
// that the clearance rule refuses (isolation.requests, off by default), or ""
// when the capture may land. What the hooks record for a conversation is a
// write on its behalf, so it meets the rule its tool calls meet: the target
// must be cleared for everything the conversation holds (access.WriteCleared
// over store.ConversationClearance). The conversation is the one the hook
// names -- its client and session id -- under peer_auth held to the caller's
// kernel chain; a capture whose target is not cleared is dropped with this
// notice. State that cannot be read refuses the capture, as it refuses a tool
// call.
func (h *Handler) captureConfined(ctx context.Context, client Client, externalSessionID, target, what string) string {
	if !h.readGrants {
		return ""
	}
	ref := store.ConversationRef{}
	if externalSessionID != "" {
		ref.Client, ref.ExternalID = client.externalIdentity(), externalSessionID
	}
	g, err := store.ConversationClearance(ctx, h.db, ref, h.now())
	if err != nil {
		h.logger.Warn("hooks: load the conversation's clearance", "error", err)
		return "Seamless did not record " + what + ": the read grants of this connection could not be checked."
	}
	if access.WriteCleared(access.WithGrants(ctx, g), target) {
		return ""
	}
	holder := "session"
	if _, hasView := access.FromContext(ctx); hasView {
		holder = "agent"
	}
	where := "project " + target
	if target == "" {
		where = "the global scope"
	}
	cleared := g.ClearedProjects()
	if len(cleared) == 0 {
		return fmt.Sprintf("Seamless did not record %s: this %s holds content from %s, and no project is cleared for all of it: "+
			"this would land in %s.", what, holder, strings.Join(g.Label(), ", "), where)
	}
	return fmt.Sprintf("Seamless did not record %s: this %s holds content from %s, so it writes only into %s, and this would land in %s.",
		what, holder, strings.Join(g.Label(), ", "), strings.Join(cleared, ", "), where)
}
