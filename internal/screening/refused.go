package screening

import (
	"context"
	"sync"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
)

// Refusal events (write screening, part 2): every refused access becomes an
// access.refused event while the feature is on -- an isolation fence, a
// peer_auth account reaching past its projects, a caller with no valid
// credential, an OS account peer_auth does not know. After the peer_auth
// cutover they are the clearest misuse signal Seamless can give.
//
// The event carries codes and names only: the reason, the surface, the tool,
// the target project, and the restricted account that was refused
// (events.PayloadAccount). Never the arguments, a key, or what was asked for.

// Refusal reasons.
const (
	RefusedFenceRead      = "fence.read"       // a read across a project isolation fence
	RefusedFenceWrite     = "fence.write"      // a write across a project isolation fence (a cross-project write attempt)
	RefusedPeerUnreach    = "peer.unreachable" // a peer_auth account reaching a project it may not
	RefusedPeerUnknown    = "peer.unknown"     // a socket caller whose OS account peer_auth does not know, or no peer credential
	RefusedOwnerOnly      = "peer.owner_only"  // a peer_auth account reaching the owner's console
	RefusedAuthFailed     = "auth.failed"      // a request with no valid bearer key
	refusalWindow         = 10 * time.Second   // one event per key per window; the rest are counted
	refusalLimiterMaxKeys = 4096               // past this many live keys the limiter starts over
)

// Refusal is one refused access.
type Refusal struct {
	Reason    string // one of the Refused* codes
	Surface   string // mcp, hooks, a2a, console, socket
	Tool      string // the MCP tool, when there is one
	Target    string // the project refused, when there is one ("" also means global)
	SessionID string // the session the call ran in, when bound
	Project   string // the caller's own scope, when known
	// Account is the restricted account refused, for a caller whose ctx no
	// longer carries its View (refusalWatch, which records outside the
	// handler that refused, from access.RefusalNote); "" takes it from ctx
	// (access.AccountOf).
	Account string
}

// refusalLimiter bounds access.refused: a caller hammering the daemon with a
// bad key must not be able to fill the event log. Per key (reason, surface,
// tool, target, session, account) at most one event per refusalWindow; the
// refusals in between are counted and ride on the next event as suppressed --
// never on another account's.
type refusalLimiter struct {
	mu   sync.Mutex
	keys map[string]*refusalKey
}

type refusalKey struct {
	last       time.Time
	suppressed int
}

// allow reports whether a refusal with this key is recorded now, and how many
// were suppressed before it.
func (l *refusalLimiter) allow(key string, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.keys == nil || len(l.keys) > refusalLimiterMaxKeys {
		l.keys = make(map[string]*refusalKey)
	}
	k, ok := l.keys[key]
	if !ok {
		l.keys[key] = &refusalKey{last: now}
		return true, 0
	}
	if now.Sub(k.last) < refusalWindow {
		k.suppressed++
		return false, 0
	}
	n := k.suppressed
	k.last, k.suppressed = now, 0
	return true, n
}

// RecordRefused records access.refused, rate-limited, while the feature is
// on. Off, it records nothing.
func (s *Service) RecordRefused(ctx context.Context, r Refusal) {
	if s == nil || !s.On(ctx) {
		return
	}
	if r.Account == "" {
		r.Account = access.AccountOf(ctx)
	}
	key := r.Reason + "\x00" + r.Surface + "\x00" + r.Tool + "\x00" + r.Target + "\x00" + r.SessionID + "\x00" + r.Account
	ok, suppressed := s.refusals.allow(key, s.now())
	if !ok {
		return
	}
	payload := map[string]any{"reason": r.Reason, "surface": r.Surface}
	if r.Tool != "" {
		payload["tool"] = r.Tool
	}
	if r.Target != "" {
		payload["target"] = r.Target
	}
	if suppressed > 0 {
		payload["suppressed"] = suppressed
	}
	if r.Account != "" {
		payload[events.PayloadAccount] = r.Account
	}
	s.record(ctx, core.Event{Kind: core.EventAccessRefused, SessionID: r.SessionID, ProjectSlug: r.Project, Payload: payload})
}
