package gardener

import (
	"context"
	"slices"
	"strings"
)

// Who decided a proposal. The value lands in gardener_proposals.resolved_by
// (migration 055) when a proposal is applied, dismissed, hidden or unhidden,
// and in the by field of the gardener.action event every decision records.
// An agent is named by its session's name (AgentActor); these are the rest.
const (
	// ByOwner is the owner, on the console. A held agent apply the owner
	// releases from Held is the owner's decision too.
	ByOwner = "owner"
	// ByAgent is an agent call that no session names.
	ByAgent = "agent"
	// ByGardener is the gardener's own upkeep: a pending proposal whose memory
	// went inactive by another route is settled (moot.go).
	ByGardener = "gardener"
	// ByAuto is automatic apply (gardener.auto_apply, auto.go): a new
	// proposal of a kind the owner opted in, applied by the pass that raised
	// it, and the kept_for_review events of those it left for the owner.
	ByAuto = "auto"
)

// reservedActors are the names no session may stand for.
var reservedActors = []string{ByOwner, ByAgent, ByGardener, ByAuto}

// AgentActor names an agent's decision by the session the call is bound to:
// its name, or ByAgent when there is none. A session's name is the agent's to
// choose (session_start name=...), so one that spells a reserved value is
// recorded as "session <name>": no agent is recorded as the owner.
func AgentActor(sessionName string) string {
	if sessionName == "" {
		return ByAgent
	}
	if slices.Contains(reservedActors, sessionName) {
		return "session " + sessionName
	}
	return sessionName
}

// SessionNameOf is the session an agent decider names (AgentActor's
// inverse): ok is false for every other value, ByAgent included.
func SessionNameOf(by string) (name string, ok bool) {
	if by == "" || slices.Contains(reservedActors, by) {
		return "", false
	}
	if rest, cut := strings.CutPrefix(by, "session "); cut && slices.Contains(reservedActors, rest) {
		return rest, true
	}
	return by, true
}

// IsAgentActor reports whether by names an agent: a session, or ByAgent.
// Everything else is the owner, the gardener, automatic apply, or unknown ("").
func IsAgentActor(by string) bool {
	return by == ByAgent || (by != "" && !slices.Contains(reservedActors, by))
}

type actorKey struct{}

// WithActor returns ctx carrying who is deciding. The console and the MCP
// tools set it before they call Apply, Dismiss, Hide, Unhide, Undo, Request,
// Split or RunNow; a call without one records no decider ("").
func WithActor(ctx context.Context, by string) context.Context {
	return context.WithValue(ctx, actorKey{}, by)
}

// ActorOf is who ctx says is deciding, "" when nobody was named.
func ActorOf(ctx context.Context) string {
	by, _ := ctx.Value(actorKey{}).(string)
	return by
}

// eventBy is the by field a decision's gardener.action event carries. It is
// resolved_by's value, with one exception: the owner's decisions are made on
// the console, and every console write records by: console, so these do too.
func eventBy(by string) string {
	if by == ByOwner {
		return "console"
	}
	return by
}
