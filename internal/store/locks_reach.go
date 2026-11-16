package store

import (
	"context"
	"slices"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

// RepoLockReachable reports whether the caller ctx speaks for may see and hold
// lock l. The owner principal reaches every lock, on any host, whatever project
// its conversation is bound to or confined in, since a lock's state carries no
// content. A restricted account, local or forwarded by a relay, reaches a lock
// only when one of its listed projects is writable in the account's reach
// (access.View.Writable, judged on each request). A request with no View
// (peer_auth off) is the owner's. Every surface that shows or grants a lock
// asks this one question, so the tools, the queue and the briefing cannot
// disagree about who may see a lock.
func RepoLockReachable(ctx context.Context, l core.RepoLock) bool {
	v, ok := access.FromContext(ctx)
	if !ok || v.Principal().Kind != access.KindRestricted {
		return true
	}
	return slices.ContainsFunc(l.Projects, v.Writable)
}
