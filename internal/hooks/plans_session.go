package hooks

// Session correlation for capture. A Claude session's in-flight plan is tracked
// in the ambient session's Metadata["plan_capture"], which is how an approval
// (whose payload carries no plan content) finds the draft it belongs to. Shared
// with subagent.go, which parks pre-plan agent notes on the same metadata. A
// capture routed to the isolated session its conversation bound itself to
// (route.go) tracks its plan on that session instead (planSession).

import (
	"context"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// planCaptureMeta mirrors core.Session.Metadata["plan_capture"]: the session's
// active plan capture, keyed by the CC plan file basename. PendingAgents holds
// the slugs of agent-cache notes captured before any plan file existed; they
// are adopted into the composition (and the list cleared) at the next capture.
type planCaptureMeta struct {
	Basename      string
	PlanSlug      string
	Status        string
	PendingAgents []string
}

// sessionPlanMeta returns the plan_capture metadata of the session a capture
// correlates with (planSession): the ambient session, or a routed capture's.
func (h *Handler) sessionPlanMeta(ctx context.Context, claudeSessionID string, route captureRoute) (planCaptureMeta, bool) {
	sess, ok := h.planSession(ctx, claudeSessionID, route)
	if !ok {
		return planCaptureMeta{}, false
	}
	m, ok := sess.Metadata["plan_capture"].(map[string]any)
	if !ok {
		return planCaptureMeta{}, false
	}
	get := func(k string) string {
		s, _ := m[k].(string)
		return s
	}
	meta := planCaptureMeta{Basename: get("basename"), PlanSlug: get("plan_slug"), Status: get("status")}
	if raw, ok := m["pending_agents"].([]any); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok && s != "" {
				meta.PendingAgents = append(meta.PendingAgents, s)
			}
		}
	}
	return meta, true
}

// setSessionPlanMeta stores meta on the session a capture correlates with
// (planSession; best-effort).
func (h *Handler) setSessionPlanMeta(ctx context.Context, claudeSessionID string, route captureRoute, meta planCaptureMeta) {
	sess, ok := h.planSession(ctx, claudeSessionID, route)
	if !ok {
		return
	}
	if sess.Metadata == nil {
		sess.Metadata = map[string]any{}
	}
	m := map[string]any{
		"basename": meta.Basename, "plan_slug": meta.PlanSlug, "status": meta.Status,
	}
	if len(meta.PendingAgents) > 0 {
		m["pending_agents"] = meta.PendingAgents
	}
	sess.Metadata["plan_capture"] = m
	// A routed capture's session may have ended (the agent's session_end, the
	// reaper) while the conversation goes on: it keeps the time it was last
	// alive, which is when it ended.
	if !route.routed || sess.Status == core.SessionActive {
		sess.UpdatedAt = time.Now().UTC()
	}
	if err := store.UpdateSession(ctx, h.db, sess); err != nil {
		h.logger.Warn("hooks: session plan meta", "error", err)
	}
}

// ambientSession looks up the ambient session for a Claude session id.
func (h *Handler) ambientSession(ctx context.Context, claudeSessionID string) (core.Session, bool) {
	if h.db == nil || claudeSessionID == "" {
		return core.Session{}, false
	}
	// Plan capture is Claude Code-only (Codex registers no plan-capture hooks).
	sess, ok, err := store.AmbientSessionByExternalIdentity(
		ctx, h.db, ClientClaudeCode.externalIdentity(), claudeSessionID)
	if err != nil {
		h.logger.Warn("hooks: ambient session lookup", "error", err)
		return core.Session{}, false
	}
	return sess, ok
}
