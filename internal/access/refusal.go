package access

import (
	"context"
	"sync"
)

// A refusal note: the daemon's outermost handler plants one in each request's
// context, and the code that refuses a caller for who it is -- an OS account
// peer_auth does not know, a restricted account at the owner's console --
// writes its reason there. The outer handler reads it back once the request
// is done and records the refusal (write screening's access.refused, while
// that feature is on). A note set by nobody records nothing.

type refusalNoteKey struct{}

// RefusalNote carries one request's refusal reason out to the handler that
// planted it, and the restricted account the refused request spoke for: the
// outer handler's own context never held the request's View (the peer_auth
// handler sets it further in), so the account travels with the reason.
type RefusalNote struct {
	mu      sync.Mutex
	reason  string
	account string
}

// Reason is the reason a handler noted, or "".
func (n *RefusalNote) Reason() string {
	if n == nil {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.reason
}

// Account is the restricted account (AccountOf) of the request that noted the
// reason, or "" -- the owner, a peer the policy does not know, no note.
func (n *RefusalNote) Account() string {
	if n == nil {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.account
}

// WithRefusalNote plants an empty note in ctx.
func WithRefusalNote(ctx context.Context) (context.Context, *RefusalNote) {
	n := &RefusalNote{}
	return context.WithValue(ctx, refusalNoteKey{}, n), n
}

// NoteRefusal writes reason into the request's note, if it carries one, with
// the restricted account ctx speaks for (AccountOf). The first reason stands,
// and with it the account that was refused.
func NoteRefusal(ctx context.Context, reason string) {
	n, ok := ctx.Value(refusalNoteKey{}).(*RefusalNote)
	if !ok {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.reason == "" {
		n.reason, n.account = reason, AccountOf(ctx)
	}
}
