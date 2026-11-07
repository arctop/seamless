package access

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Access requests at the lock (the 2026-11-06 design, step 6): a call refused
// at a locked project files a pending request automatically, and the refusal
// names it, so the agent can tell the owner exactly what to approve. Asking
// needs no privilege. Filing touches the database and knows the session, so it
// is not this package's: the surface that serves the request plants a
// LockRequester in its context, and Refusal(ctx, project) calls it. A context
// without one -- background work, the TCP key, peer_auth off -- refuses as
// before.
//
// Every refusal at a lock is two lines: the rule, then the remedy. When a
// request was filed, the remedy names it and how the owner approves it.

// LockRequest is the pending request a lock refusal names.
type LockRequest struct {
	// ID is the request's id; Short its short form (R-7F3K) for a person.
	ID, Short string
	// Refresh reports a request from a tree that already holds a lease on
	// the project: approving it replaces that lease.
	Refresh bool
	// Page is the request's passkey approval page, "" unless a passkey can
	// approve it there (passkeys set up, one enrolled: approval.Service.Ways).
	Page string
	// TouchID reports an enrolled Touch ID key: the approval dialog can
	// approve the request with it.
	TouchID bool
	// Ended is when this tree's last lease on the project ended (expired or
	// revoked), zero when it never held one: the refusal then says when
	// access ended, so the agent can tell a lapsed approval from none.
	Ended time.Time
	// Unfiled, set instead of ID, is why no request was filed for this
	// process: its anchor is one no approval may name (approval.AnchorError),
	// so the refusal says why and what to do, on its two lines, and names no
	// request. UnfiledRemedy is that remedy.
	Unfiled, UnfiledRemedy string
}

// Ways are the approval paths a daemon offers the owner for one request, as
// every remedy names them; approval.Service.Ways fills them in. The owner's
// own terminal (`seam approve`) is always one and is not listed.
type Ways struct {
	// TouchID reports an enrolled Touch ID key. It approves only in the
	// approval dialog Seamless shows in an agent's session (the agent's MCP
	// bridge launches the helper), so the words say where.
	TouchID bool
	// Passkey reports that passkeys are set up and one is enrolled; Page is
	// the request's passkey page ("" before a request exists).
	Passkey bool
	Page    string
}

// ways is the request's approval paths.
func (r LockRequest) ways() Ways {
	return Ways{TouchID: r.TouchID, Passkey: r.Page != "", Page: r.Page}
}

// OwnerApproves says how the owner approves request short, offering only the
// paths w holds: Touch ID in the approval dialog, their own terminal (always),
// a passkey. With short "" -- no request filed yet -- the words name "the
// request's id" instead.
func (w Ways) OwnerApproves(short string) string {
	terminal := "in their own terminal with `seam approve " + short + "`"
	if short == "" {
		terminal = "in their own terminal with `seam approve` and the request's id"
	}
	var ways []string
	if w.TouchID {
		ways = append(ways, "with Touch ID in the approval dialog (where this agent's client shows one)")
	}
	ways = append(ways, terminal)
	switch {
	case w.Page != "":
		ways = append(ways, "with a passkey at "+w.Page)
	case w.Passkey:
		ways = append(ways, "with a passkey on the request's console page")
	}
	return "the owner approves it " + orList(ways)
}

// StillPending says how the owner may still decide request short once an
// approval dialog ended without a decision: their own terminal (always), a
// passkey, Touch ID in the next dialog.
func (w Ways) StillPending(short string) string {
	ways := []string{"`seam approve " + short + "` in their own terminal"}
	if w.Page != "" {
		ways = append(ways, "a passkey at "+w.Page)
	}
	if w.TouchID {
		ways = append(ways, "Touch ID in the next approval dialog")
	}
	return orList(ways)
}

// orList joins choices as prose: "a", "a, or b", "a, b, or c".
func orList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + ", or " + items[1]
	}
	return strings.Join(items[:len(items)-1], ", ") + ", or " + items[len(items)-1]
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
	if err != nil || (req.ID == "" && req.Unfiled == "") {
		return LockRequest{}, false
	}
	return req, true
}

// Locked is View.Locked for the View in ctx; with no View nothing is locked.
func Locked(ctx context.Context, project string) bool {
	v, _ := FromContext(ctx)
	return v.Locked(project)
}

// RefusalFor is the refusal for a locked project that names req, the request
// a surface filed for it itself (the SessionStart hook files one for the
// session it is creating). A request with no id -- none could be filed -- is
// the plain refusal (View.Refusal).
func (v View) RefusalFor(project string, req LockRequest) error {
	if (req.ID == "" && req.Unfiled == "") || !v.Locked(project) {
		return v.Refusal(project)
	}
	return lockedRefusalFor(project, req)
}

// lockedRefusalFor is the refusal for a locked project that names the request
// filed for it, and how the owner approves it: the rule on one line, the
// remedy on the next.
func lockedRefusalFor(project string, req LockRequest) error {
	if req.Unfiled != "" {
		return &RefusalError{msg: fmt.Sprintf("project %s is locked: even the owner's own agents need the owner's "+
			"approval, and %s.", project, req.Unfiled) + "\nRemedy: " + req.UnfiledRemedy}
	}
	rule := fmt.Sprintf("project %s is locked: even the owner's own agents need the owner's approval, "+
		"and this session holds none.", project)
	if !req.Ended.IsZero() {
		rule = fmt.Sprintf("project %s is locked (access ended at %s): even the owner's own agents need the owner's "+
			"approval, and this session's approval has ended.", project, clockDay(req.Ended))
	}
	what := "access request " + req.Short
	if req.Refresh {
		what = "refresh request " + req.Short
	}
	how := req.ways().OwnerApproves(req.Short)
	return &RefusalError{msg: rule + "\nRemedy: " + what + " is waiting -- " + how +
		"; retry once it is approved, and keep to other projects until then."}
}
