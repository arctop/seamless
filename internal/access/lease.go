package access

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The owner-locked view (peer_auth.locked, the 2026-11-04 design): a locked
// project is closed to every owner request. Its name shows (View.Listed) and
// its content does not (View.Visible), and writes into it are refused. It
// opens only for one request at a time, and only when the transport has
// attached a live lease the owner approved for the agent process at the top of
// that connection's process chain (View.WithLeases; proctree.go).
//
// A lease is data the access layer judges, never data it trusts. The transport
// edge walks the kernel's process chain of a socket connection, loads the
// lease rows for its processes and hands both in. The View keeps a lease only
// when all four hold: it names a process of that very chain, the project is
// one peer_auth.locked lists, it has not expired, and the view is the owner's
// socket view. A TCP request -- the bearer key, which every owner process can
// present -- never holds one, whatever it is handed.

// ProcessToken names one process as the kernel reported it: the peer of a
// socket connection or one of its ancestors (Peer.Chain). It holds the pid,
// the process's start time (Version, in internal/agentproc's unit:
// microseconds since the epoch on macOS, clock ticks since boot on Linux), and
// the boot it ran in. Together these are unique across pid reuse and reboots.
// The token is compared as a whole (Key), never by its parts: two tokens are
// equal exactly when they name one process.
type ProcessToken struct {
	PID     int32
	Version uint64
	Boot    string
}

// Valid reports whether t names a process at all: a positive pid and a boot
// identifier of letters, digits and dashes (a UUID, as both kernels give it).
func (t ProcessToken) Valid() bool {
	if t.PID <= 0 || t.Boot == "" || len(t.Boot) > 64 {
		return false
	}
	for _, r := range t.Boot {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// Key is the token's one comparable form: what lease rows store and match on.
// An invalid token has no key ("").
func (t ProcessToken) Key() string {
	if !t.Valid() {
		return ""
	}
	return t.Boot + ":" + strconv.FormatInt(int64(t.PID), 10) + ":" + strconv.FormatUint(t.Version, 10)
}

// String renders the token for logs: the key.
func (t ProcessToken) String() string { return t.Key() }

// Lease is one owner approval as the access layer judges it. Process is the
// ProcessToken.Key of the agent process the lease was granted to (its whole
// tree shares it), Expires the instant it ends.
type Lease struct {
	ID      string
	Project string
	Process string
	Expires time.Time
}

// WithLeases returns v with the leases that apply to it at now attached. It
// keeps a lease only when v is the owner's view on the Unix socket and carries
// a process chain, and the lease names a process of that chain (the agent's,
// which every process it started shares) and a project peer_auth.locked
// lists, and it ends after now. Every other lease is dropped,
// and every other view comes back unchanged, so a lease can never reach a TCP
// request, another process, a project sealed to an OS account, or a project
// that is not locked at all.
//
// The leases are judged once, at now: a request that began before a lease
// ended finishes under it. The transport builds a View per request, so the
// next request is judged afresh.
func (v View) WithLeases(now time.Time, leases []Lease) View {
	if v.who.Kind != KindOwner || !v.who.Socket || !v.who.HasProcess || v.policy == nil || len(v.policy.ownerLocked) == 0 {
		return v
	}
	inChain := make(map[string]bool, len(v.who.Chain))
	for _, t := range v.who.Chain {
		if k := t.Key(); k != "" {
			inChain[k] = true
		}
	}
	var kept map[string]Lease
	for _, l := range leases {
		if !inChain[l.Process] || !v.policy.ownerLocked[l.Project] || !l.Expires.After(now) {
			continue
		}
		if prev, dup := kept[l.Project]; dup && !l.Expires.After(prev.Expires) {
			continue
		}
		if kept == nil {
			kept = map[string]Lease{}
		}
		kept[l.Project] = l
	}
	v.leases = kept
	return v
}

// leased reports whether v holds a lease on project.
func (v View) leased(project string) bool {
	_, ok := v.leases[project]
	return ok
}

// Lease returns the lease v holds on project, if any: the expiry the
// warnings count down to.
func (v View) Lease(project string) (Lease, bool) {
	l, ok := v.leases[project]
	return l, ok
}

// Leases returns every lease v holds, by project.
func (v View) Leases() []Lease {
	out := make([]Lease, 0, len(v.leases))
	for _, l := range v.leases {
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b Lease) int { return strings.Compare(a.Project, b.Project) })
	return out
}

// Locked reports whether project is locked (peer_auth.locked) and v holds no
// lease on it: the owner may see that it exists, and nothing inside.
// It is false for every non-owner view (they never list such a project: a
// configured account reaches only its own projects).
func (v View) Locked(project string) bool {
	return v.who.Kind == KindOwner && v.policy != nil && v.policy.ownerLocked[project] && !v.leased(project)
}

// Listed reports whether v may show that project exists: its slug, name and
// item counts, never anything inside it. Every project v may read is listed;
// beyond that, the owner sees a locked project -- locked, not hidden -- so an
// agent can name it in an access request. A project sealed to another OS
// account stays unlisted, as before.
func (v View) Listed(project string) bool {
	return v.Visible(project) || v.Locked(project)
}

// lockedRefusal is the refusal for a locked project: the rule and the remedy,
// which differ between the TCP key and the socket.
func (v View) lockedRefusal(project string) error {
	if !v.who.Socket {
		return &RefusalError{msg: fmt.Sprintf("project %s is locked: the TCP connection and its bearer key never reach a "+
			"locked project, because a shared key cannot be tied to one process -- work in it from an agent session "+
			"whose seam mcp-proxy dials the peer_auth Unix socket, once the owner has approved that session's access "+
			"request", project)}
	}
	return &RefusalError{msg: fmt.Sprintf("project %s is locked: even the owner's own agents need the owner's approval, "+
		"and this connection holds no unlock for it -- the owner unlocks it for one session's seam mcp-proxy process, "+
		"for a limited time, by approving that session's access request", project)}
}
