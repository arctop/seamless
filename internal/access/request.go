package access

import (
	"context"
	"fmt"
)

// Access requests at the lock (the 2026-11-06 design, step 6): a call refused
// at a locked project files a pending request automatically, and the refusal
// names it, so the agent can tell the owner exactly what to approve. Asking
// needs no privilege. Filing touches the database and knows the session, so it
// is not this package's: the surface that serves the request plants a
// LockRequester in its context, and Refusal(ctx, project) calls it. A context
// without one -- background work, the TCP key, peer_auth off -- refuses as
// before.

// LockRequest is the pending request a lock refusal names.
type LockRequest struct {
	// ID is the request's id; Short its short form (R-7F3K) for a person.
	ID, Short string
	// Refresh reports a request from a tree that already holds a lease on
	// the project: approving it replaces that lease.
	Refresh bool
	// Page is the passkey approval page, "" when passkeys are not set up.
	Page string
}

// LockRequester files, or finds, the pending access request for a locked
// project on behalf of the request in ctx.
type LockRequester func(ctx context.Context, project string) (LockRequest, error)

type lockRequesterKey struct{}

// WithLockRequester returns ctx carrying f.
func WithLockRequester(ctx context.Context, f LockRequester) context.Context {
	return context.WithValue(ctx, lockRequesterKey{}, f)
}

// lockRequest files the request for a locked project when ctx carries a
// requester and the view is the owner's socket view; false otherwise.
func lockRequest(ctx context.Context, v View, project string) (LockRequest, bool) {
	if !v.Locked(project) || !v.who.Socket {
		return LockRequest{}, false
	}
	f, ok := ctx.Value(lockRequesterKey{}).(LockRequester)
	if !ok || f == nil {
		return LockRequest{}, false
	}
	req, err := f(ctx, project)
	if err != nil || req.ID == "" {
		return LockRequest{}, false
	}
	return req, true
}

// lockedRefusalFor is the refusal for a locked project that names the request
// filed for it, and how the owner approves it.
func lockedRefusalFor(project string, req LockRequest) error {
	what := "access request " + req.Short
	if req.Refresh {
		what = "refresh request " + req.Short
	}
	how := fmt.Sprintf("the owner approves it in their own terminal with `seam approve %s`", req.Short)
	if req.Page != "" {
		how += ", or with a passkey at " + req.Page
	}
	return &RefusalError{msg: fmt.Sprintf("project %s is locked: even the owner's own agents need the owner's approval, "+
		"and this session holds none -- %s is waiting: %s. Retry once it is approved; until then, keep to other projects",
		project, what, how)}
}
