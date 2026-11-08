// Package access is peer-user authentication's policy: which projects the caller
// behind one request may reach.
//
// With peer_auth off (the default) nothing here runs: no request carries a View,
// and every check below answers "yes", which is exactly the behavior every
// earlier release had. With it on, the daemon learns who is calling from the
// transport, never from anything the client sends:
//
//   - a Unix-socket connection names its peer's uid through the kernel
//     (LOCAL_PEERCRED on macOS, SO_PEERCRED on Linux; peercred_*.go). The owner's
//     uid is the owner; a uid configured under peer_auth.users is that user; any
//     other uid is refused at the door.
//   - a TCP connection carrying the static bearer key is the owner. The key
//     lives in the owner's account, so whoever holds it acts as the owner.
//
// The policy is two rules, and this file is the only place they are written:
//
//  1. A project sealed to a user is reachable only from that user's socket
//     connections. The owner -- TCP key and socket alike -- never reaches it.
//  2. A configured user reaches only its own projects (sealed plus shared) for
//     reads and writes, plus the global scope for reads. It never writes the
//     global scope: a global item is injected into every project's briefing, the
//     owner's included, so a global write from a sealed system is the loudest
//     possible way for its content to leave.
//
// peer_auth.restricted adds a third principal (KindRestricted, 2026-11-07): a
// restricted local account, a second OS account on the machine whose agents
// work beside the owner's. It reaches every project that is open (isolation)
// and not locked, reading and writing, and the global scope read-only -- the
// same rule 2 above, over a reach the transport loads from the projects table
// for every request (View.WithOpenProjects) instead of an allow-list in the
// config. It files no access requests, gets no read grants and holds no leases,
// and every write it makes is labeled with its name (View.Account).
//
// peer_auth.locked is the other design (2026-11-04; lease.go). There, a project
// is locked rather than sealed to another account: closed to every owner
// request -- listed, never read, never written -- unless the request rides a
// socket connection whose process holds a live lease the owner approved. The
// lock is not an isolation state (open, confidential, sealed): isolation says
// what flows in and out of a project, the lock whether even its own agents need
// a fresh approval. A config uses one design or the other, never both
// (config.validatePeerLocked).
//
// Everything else -- the store's isolation fence (store.CanRead/CanWrite), the
// MCP scope funnels, the hooks, the retrieval surfaces, the console's filtered
// database handle, the event log -- asks a View from the request context. Code
// that runs for no caller (the gardener ticker, the files watcher, startup
// reconciliation) carries no View and is not restricted: it serves no one, so
// there is no one to leak to.
package access

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrNotReachable is the sentinel behind every peer_auth refusal. Surfaces wrap
// it with View.Refusal so the agent reads the rule and the remedy; callers that
// branch on it use errors.Is.
var ErrNotReachable = errors.New("not reachable from this connection")

// Kind says which side of the policy a principal is on.
type Kind uint8

const (
	// KindNone is no principal at all: background work, or peer_auth off. It is
	// the zero value on purpose -- a View nobody set restricts nothing.
	KindNone Kind = iota
	// KindOwner is the owner account: the TCP bearer key, or the owner's uid on
	// the socket.
	KindOwner
	// KindUser is an OS user configured under peer_auth.users, on the socket.
	KindUser
	// KindRestricted is a restricted local account (peer_auth.restricted), on
	// the socket: open projects read-write, global read-only, writes labeled.
	KindRestricted
)

// String names the kind for logs and doctor rows.
func (k Kind) String() string {
	switch k {
	case KindOwner:
		return "owner"
	case KindUser:
		return "user"
	case KindRestricted:
		return "restricted"
	default:
		return "none"
	}
}

// Principal is who one request speaks for, as the transport established it.
type Principal struct {
	Kind Kind
	// UID is the peer's uid. HasUID is false on TCP, where the kernel names no
	// peer and the bearer key stands in for the owner.
	UID    uint32
	HasUID bool
	// User is the OS account name ("katata", "_arctop_finance").
	User string
	// Socket reports that the kernel authenticated this request on the Unix
	// socket. Only such a request may skip the bearer key.
	Socket bool
	// Process is the kernel-reported process at the other end of the socket
	// connection, when the transport could read one (HasProcess), and Chain
	// is that process followed by its verified ancestors (proctree.go). A
	// lease is bound to one process of the chain -- the agent's -- so every
	// process in the agent's tree shares it. A TCP request never has either.
	Process    ProcessToken
	HasProcess bool
	Chain      []ProcessToken
}

// View is one request's reach under the policy. The zero View (no policy, no
// principal) reaches everything.
type View struct {
	policy *Policy
	who    Principal
	user   *userRule
	// leases are the owner's live unlocks of locked projects, by project, as
	// WithLeases judged them. Nil for every other view.
	leases map[string]Lease
	// acct is a restricted account's own account (KindRestricted only), and
	// open its reach: the projects that were open and not locked when the
	// transport built this request's view (WithOpenProjects). Nil open reaches
	// no project at all -- a restricted view the transport never filled fails
	// closed to the global scope, read-only.
	acct Account
	open map[string]bool
}

// Principal returns who the view speaks for.
func (v View) Principal() Principal { return v.who }

// Restricted reports whether this view can be refused anything at all: a
// configured user always can, the owner only while some project is sealed to
// another user or locked (peer_auth.locked).
func (v View) Restricted() bool {
	switch v.who.Kind {
	case KindUser, KindRestricted:
		return true
	case KindOwner:
		return len(v.Hidden()) > 0
	default:
		return false
	}
}

// Visible reports whether this view may read project's content. "" is the
// global scope, which every view reads.
func (v View) Visible(project string) bool {
	if project == "" {
		return true
	}
	switch v.who.Kind {
	case KindOwner:
		if v.policy == nil {
			return true
		}
		if _, sealed := v.policy.sealedTo[project]; sealed {
			return false
		}
		// Locked: readable only under a lease.
		return !v.policy.ownerLocked[project] || v.leased(project)
	case KindUser:
		return v.user != nil && v.user.allowed[project]
	case KindRestricted:
		return v.open[project]
	default:
		return true
	}
}

// Writable reports whether this view may write into project. The owner writes
// wherever it reads; a configured user writes only its own projects, and a
// restricted account only the open projects it reaches -- neither ever writes
// the global scope (see the package comment, rule 2).
func (v View) Writable(project string) bool {
	switch v.who.Kind {
	case KindUser:
		return project != "" && v.user != nil && v.user.allowed[project]
	case KindRestricted:
		return project != "" && v.open[project]
	}
	return v.Visible(project)
}

// Hidden returns the projects this view may not read, sorted, when that set is
// finite: for the owner, the projects sealed to other users plus every
// locked project it holds no lease on. A locked project is hidden here
// although it is Listed: Hidden is about content. A configured user's reach is
// an allow-list instead (Allowed), and the zero View hides nothing.
func (v View) Hidden() []string {
	if v.who.Kind != KindOwner || v.policy == nil {
		return nil
	}
	out := slices.Clone(v.policy.ownerHidden)
	for _, slug := range v.policy.ownerLockedList {
		if !v.leased(slug) {
			out = append(out, slug)
		}
	}
	slices.Sort(out)
	return out
}

// Allowed returns a configured user's projects, or a restricted account's
// reach (the open projects WithOpenProjects kept), sorted, and true. Any other
// view returns false: it is not bounded by an allow-list.
func (v View) Allowed() ([]string, bool) {
	if v.who.Kind == KindRestricted {
		out := make([]string, 0, len(v.open))
		for p := range v.open {
			out = append(out, p)
		}
		slices.Sort(out)
		return out, true
	}
	if v.who.Kind != KindUser {
		return nil, false
	}
	if v.user == nil {
		return []string{}, true // fail closed: bounded, by nothing
	}
	out := make([]string, 0, len(v.user.allowed))
	for p := range v.user.allowed {
		out = append(out, p)
	}
	slices.Sort(out)
	return out, true
}

// Home is the project a configured user's work lands in when nothing else
// places it: its first sealed project, else its first shared one. It is "" for
// every other view. A configured user always has one -- the policy refuses a
// user that reaches no project.
func (v View) Home() string {
	if v.who.Kind != KindUser || v.user == nil {
		return ""
	}
	return v.user.home
}

// SessionScope places a session whose cwd (or argument) resolved to project.
// A project this view may write is kept. A configured user's session never lands
// outside its own projects, so anything else -- global included -- becomes its
// home project. The owner cannot be placed in a project sealed to another user
// at all, and gets the refusal. A restricted account has no home: a session it
// starts anywhere but an open project it reaches -- the global scope, an
// unmapped directory, a project it may not reach -- is refused, in words that
// never tell a project it may not reach from one that does not exist.
func (v View) SessionScope(project string) (string, error) {
	if v.Writable(project) {
		return project, nil
	}
	if home := v.Home(); v.who.Kind == KindUser && home != "" {
		return home, nil
	}
	return "", v.Refusal(project)
}

// Refusal is the error a surface returns when this view may not reach project.
// It states the rule and the remedy, and it never echoes content. It names no
// other user: the seal is the owner's configuration, not something a refused
// caller needs to learn more of.
func (v View) Refusal(project string) error {
	if v.who.Kind == KindRestricted {
		return restrictedRefusal(v.who.User, project)
	}
	if v.who.Kind == KindUser {
		allowed, _ := v.Allowed()
		if project == "" {
			return &RefusalError{msg: fmt.Sprintf("the global scope is read-only from this connection: "+
				"OS user %s writes only into %s", v.who.User, strings.Join(allowed, ", "))}
		}
		return &RefusalError{msg: fmt.Sprintf("project %s is not reachable from this connection: "+
			"OS user %s may reach only %s (and read the global scope)", project, v.who.User, strings.Join(allowed, ", "))}
	}
	if v.Locked(project) {
		return v.lockedRefusal(project)
	}
	return &RefusalError{msg: fmt.Sprintf("project %s is not reachable from this connection: peer_auth "+
		"reserves it for its own system's OS user -- work in it from an agent running as that user "+
		"(sudo -u), not from this account", project)}
}

// RefusalError is a peer_auth refusal. Its text is the rule and the remedy;
// errors.Is matches it against ErrNotReachable.
type RefusalError struct{ msg string }

func (e *RefusalError) Error() string { return e.msg }

// Is makes every refusal match ErrNotReachable.
func (e *RefusalError) Is(target error) bool { return target == ErrNotReachable }

type viewKey struct{}

// WithView returns ctx carrying v. The transport edge (cmd/seamlessd) installs
// one on every request while peer_auth is on.
func WithView(ctx context.Context, v View) context.Context {
	return context.WithValue(ctx, viewKey{}, v)
}

// FromContext returns the View ctx carries, and whether it carries one.
func FromContext(ctx context.Context) (View, bool) {
	v, ok := ctx.Value(viewKey{}).(View)
	return v, ok
}

// Visible is View.Visible for the View in ctx; no View reaches everything.
func Visible(ctx context.Context, project string) bool {
	v, _ := FromContext(ctx)
	return v.Visible(project)
}

// Writable is View.Writable for the View in ctx; no View reaches everything.
func Writable(ctx context.Context, project string) bool {
	v, _ := FromContext(ctx)
	return v.Writable(project)
}

// Refusal is View.Refusal for the View in ctx. At a locked project, when ctx
// carries a LockRequester, it files (or finds) the pending access request and
// the refusal names it.
func Refusal(ctx context.Context, project string) error {
	v, _ := FromContext(ctx)
	if req, ok := lockRequest(ctx, v, project); ok {
		return lockedRefusalFor(project, req)
	}
	return v.Refusal(project)
}

// SessionScope is View.SessionScope for the View in ctx; with no View every
// project is kept. A refusal at a lock files its request, as Refusal does.
func SessionScope(ctx context.Context, project string) (string, error) {
	v, _ := FromContext(ctx)
	got, err := v.SessionScope(project)
	if err != nil {
		if req, ok := lockRequest(ctx, v, project); ok {
			return "", lockedRefusalFor(project, req)
		}
	}
	return got, err
}

// PeerAuthenticated reports whether the kernel authenticated this request on the
// Unix socket. The bearer-key checks accept it in place of the key; nothing
// else does.
func PeerAuthenticated(ctx context.Context) bool {
	v, ok := FromContext(ctx)
	return ok && v.who.Socket
}

// OwnerOnly reports whether ctx may use an owner-only surface (the console): no
// View at all (peer_auth off), or the owner's. A configured user and a
// restricted account never may.
func OwnerOnly(ctx context.Context) bool {
	v, ok := FromContext(ctx)
	return !ok || v.who.Kind == KindOwner || v.who.Kind == KindNone
}
