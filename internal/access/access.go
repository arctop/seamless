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
//     uid is the owner; a uid peer_auth.restricted lists is that restricted
//     account; any other uid is refused at the door.
//   - a TCP connection carrying the static bearer key is the owner. The key
//     lives in the owner's account, so whoever holds it acts as the owner.
//   - a request a paired relay forwarded from another computer (plan
//     networked-relay) is judged as this machine's socket judges one, when
//     peer_auth.relays maps that relay: the relay's own kernel named the
//     caller's uid, process chain and (wire version 2) each process's
//     program on the relay's socket, the relay's TLS certificate names the
//     relay, and the mapping says which uid there is the owner and which are
//     restricted accounts (Policy.RelayChainViewNamed). The transport refuses
//     an unmapped relay's requests: no uid there is known to be the owner.
//
// The policy is two rules, and this file and restricted.go are the only places
// they are written:
//
//  1. The owner reaches every project but the locked ones (peer_auth,
//     2026-11-04; lease.go): a locked project is closed to every owner request
//     -- listed, never read, never written -- unless the request rides a socket
//     connection whose process holds a live lease the owner approved. The lock
//     is not an isolation state (open, confidential, sealed): isolation says
//     what flows in and out of a project, the lock whether even its own agents
//     need a fresh approval.
//  2. A restricted local account (peer_auth.restricted, KindRestricted,
//     2026-11-07), a second OS account on the machine whose agents work beside
//     the owner's, reaches every project that is open (isolation) and not
//     locked, reading and writing, and the global scope read-only: a global
//     item is injected into every project's briefing, the owner's included. Its
//     reach is loaded from the projects table for every request
//     (View.WithOpenProjects). It files no access requests, gets no read grants
//     and holds no leases, and every write it makes is labeled with its name
//     (View.Account).
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
	// KindRestricted is a restricted local account (peer_auth.restricted), on
	// the socket: open projects read-write, global read-only, writes labeled.
	KindRestricted
)

// String names the kind for logs and doctor rows.
func (k Kind) String() string {
	switch k {
	case KindOwner:
		return "owner"
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
	// User is the OS account name ("owner", "_example_agent").
	User string
	// Socket reports that the kernel authenticated this request on the Unix
	// socket: this daemon's own, or a paired relay's (Relay). Only such a
	// request may skip the bearer key.
	Socket bool
	// Process is the kernel-reported process at the other end of the socket
	// connection, when the transport could read one (HasProcess), and Chain
	// is that process followed by its verified ancestors (proctree.go). A
	// lease is bound to one process of the chain -- the agent's -- so every
	// process in the agent's tree shares it. A TCP request never has either.
	Process    ProcessToken
	HasProcess bool
	Chain      []ProcessToken
	// Relay is the paired relay (its device id) that forwarded this request
	// from another computer (plan networked-relay; Policy.RelayChainView):
	// that computer's kernel named the caller on the relay's own Unix socket,
	// and UID and Chain are its report, each process of the chain naming the
	// relay (ProcessToken.Relay). "" for this machine's socket and for TCP.
	Relay string
	// Programs is, for a relay that names them (wire version 2), the program
	// each process of Chain runs as that computer's kernel named it:
	// Programs[i] for Chain[i], "" where it named none. Nil for a relay of
	// wire version 1 and for this machine's socket, whose programs the
	// daemon's own kernel names (ProgramOf). ReportedProgram reads it.
	Programs []string
}

// Named reports whether the transport named the programs of p's chain: a
// relay of wire version 2. This machine's socket is not "named" here: the
// daemon asks its own kernel (ProgramOf).
func (p Principal) Named() bool {
	return len(p.Chain) > 0 && len(p.Programs) == len(p.Chain)
}

// ReportedProgram is the program the transport reported for t, a process of
// p's chain (Named): its entry in Programs. ok is false when the transport
// reported none for t: this machine's socket, a relay of wire version 1, or a
// token that is not one of the chain's.
func (p Principal) ReportedProgram(t ProcessToken) (string, bool) {
	k := t.Key()
	if k == "" || !p.Named() {
		return "", false
	}
	for i, c := range p.Chain {
		if c.Key() == k {
			return p.Programs[i], true
		}
	}
	return "", false
}

// View is one request's reach under the policy. The zero View (no policy, no
// principal) reaches everything.
type View struct {
	policy *Policy
	who    Principal
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
// restricted account always can, the owner only while some project is locked.
func (v View) Restricted() bool {
	switch v.who.Kind {
	case KindRestricted:
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
		// Locked: readable only under a lease.
		return !v.policy.ownerLocked[project] || v.leased(project)
	case KindRestricted:
		return v.open[project]
	default:
		return true
	}
}

// Writable reports whether this view may write into project. The owner writes
// wherever it reads; a restricted account writes only the open projects it
// reaches, never the global scope (see the package comment, rule 2).
func (v View) Writable(project string) bool {
	if v.who.Kind == KindRestricted {
		return project != "" && v.open[project]
	}
	return v.Visible(project)
}

// Hidden returns the projects this view may not read, sorted, when that set is
// finite: for the owner, every locked project it holds no lease on. A locked
// project is hidden here although it is Listed: Hidden is about content. A
// restricted account's reach is an allow-list instead (Allowed), and the zero
// View hides nothing.
func (v View) Hidden() []string {
	if v.who.Kind != KindOwner || v.policy == nil {
		return nil
	}
	var out []string
	for _, slug := range v.policy.ownerLockedList {
		if !v.leased(slug) {
			out = append(out, slug)
		}
	}
	return out
}

// Allowed returns a restricted account's reach (the open projects
// WithOpenProjects kept), sorted, and true. Any other view returns false: it is
// not bounded by an allow-list.
func (v View) Allowed() ([]string, bool) {
	if v.who.Kind != KindRestricted {
		return nil, false
	}
	out := make([]string, 0, len(v.open))
	for p := range v.open {
		out = append(out, p)
	}
	slices.Sort(out)
	return out, true
}

// SessionScope places a session whose cwd (or argument) resolved to project.
// A project this view may write is kept. Anything else is refused: the owner
// at a locked project it holds no lease on, and a restricted account anywhere
// but an open project it reaches -- the global scope, an unmapped directory, a
// project it may not reach -- in words that never tell a project it may not
// reach from one that does not exist.
func (v View) SessionScope(project string) (string, error) {
	if v.Writable(project) {
		return project, nil
	}
	return "", v.Refusal(project)
}

// Refusal is the error a surface returns when this view may not reach project.
// It states the rule and the remedy, and it never echoes content. The owner is
// refused only at a lock; the plain last line is what a view the policy never
// built gets, failing closed.
func (v View) Refusal(project string) error {
	if v.who.Kind == KindRestricted {
		return restrictedRefusal(v.who.User, project)
	}
	if v.Locked(project) {
		return v.lockedRefusal(project)
	}
	return &RefusalError{msg: fmt.Sprintf("project %s is not reachable from this connection", project)}
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
// Unix socket: this daemon's own, or, through a paired relay peer_auth.relays
// maps, the relay's on its own computer (Principal.Relay). The bearer-key
// checks accept it in place of the key; nothing else does.
func PeerAuthenticated(ctx context.Context) bool {
	v, ok := FromContext(ctx)
	return ok && v.who.Socket
}

// agentChainKey carries a request's FromAgent answer.
type agentChainKey struct{}

// WithAgentChain records how to tell whether this socket request came from a
// process an agent started: fromAgent asks the kernel's process chain.
func WithAgentChain(ctx context.Context, fromAgent func() bool) context.Context {
	return context.WithValue(ctx, agentChainKey{}, fromAgent)
}

// FromAgent reports whether this socket request came from a process an agent
// started, by the kernel's process chain. It answers true when the chain cannot
// be read, and false for a request that did not arrive on the socket. It guards
// the owner's own decisions against an agent's ordinary tool calls, and it is
// not a wall: a process that detaches from its agent has no agent above it, and
// whoever holds the bearer key does not come through here. The lock is what
// holds against an agent set on getting in.
func FromAgent(ctx context.Context) bool {
	if !PeerAuthenticated(ctx) {
		return false
	}
	fromAgent, _ := ctx.Value(agentChainKey{}).(func() bool)
	return fromAgent == nil || fromAgent()
}

// OwnerOnly reports whether ctx may use an owner-only surface (the console): no
// View at all (peer_auth off), or the owner's. A restricted account never may.
func OwnerOnly(ctx context.Context) bool {
	v, ok := FromContext(ctx)
	return !ok || v.who.Kind == KindOwner || v.who.Kind == KindNone
}
