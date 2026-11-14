package access

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The owner-locked view (peer_auth, the 2026-11-04 design): a locked
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
// one the policy locks, it has not expired, and the view is the owner's
// socket view. A TCP request -- the bearer key, which every owner process can
// present -- never holds one, whatever it is handed.

// ProcessToken names one process as the kernel reported it: the peer of a
// socket connection or one of its ancestors (Peer.Chain). It holds the pid,
// the process's start time (Version, in internal/agentproc's unit:
// microseconds since the epoch on macOS, clock ticks since boot on Linux), and
// the boot it ran in. Together these are unique across pid reuse and reboots.
// The token is compared as a whole (Key), never by its parts: two tokens are
// equal exactly when they name one process.
//
// Relay names the computer when it is not this one (plan networked-relay): the
// paired relay, by its device id, whose own kernel reported the process and
// forwarded it with the request. "" is a process of this machine, as this
// daemon's kernel reported it. Two computers' processes never share a key,
// whatever their pid, start time and boot, so a lease, grant or request keyed
// on one relay's process never applies to another relay's or to this machine's.
type ProcessToken struct {
	PID     int32
	Version uint64
	Boot    string
	Relay   string
}

// maxRelayIDLen bounds ProcessToken.Relay: a device id is a ULID (26).
const maxRelayIDLen = 64

// Valid reports whether t names a process at all: a positive pid, a boot
// identifier of letters, digits and dashes (a UUID, as both kernels give it),
// and a relay that is "" or an id of letters and digits.
func (t ProcessToken) Valid() bool {
	if t.PID <= 0 || t.Boot == "" || len(t.Boot) > 64 || len(t.Relay) > maxRelayIDLen {
		return false
	}
	for _, r := range t.Boot {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	for _, r := range t.Relay {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// relayKeyPrefix starts the key of every process a relay reported:
// "relay/<id>/". A boot never holds a slash, so no key of this machine's
// processes starts with it.
const relayKeyPrefix = "relay/"

// Key is the token's one comparable form: what lease rows store and match on.
// An invalid token has no key (""). A process of this machine is
// "<boot>:<pid>:<version>", exactly as a relay forwards its own; one a relay
// reported is "relay/<relay id>/<boot>:<pid>:<version>" (RelayKeyPrefix).
func (t ProcessToken) Key() string {
	if !t.Valid() {
		return ""
	}
	key := t.Boot + ":" + strconv.FormatInt(int64(t.PID), 10) + ":" + strconv.FormatUint(t.Version, 10)
	if t.Relay != "" {
		return RelayKeyPrefix(t.Relay) + key
	}
	return key
}

// RelayKeyPrefix is how every key of a process relay reported starts:
// "relay/<relay>/". The store finds a relay's rows by it.
func RelayKeyPrefix(relay string) string { return relayKeyPrefix + relay + "/" }

// RelayOfKey is the relay a process key names, "" for a process of this
// machine (or a key that is not a process key at all).
func RelayOfKey(key string) string {
	rest, ok := strings.CutPrefix(key, relayKeyPrefix)
	if !ok {
		return ""
	}
	relay, _, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	return relay
}

// ErrProcessKey is a process key that is not one ProcessToken.Key renders
// for a process of the computer that sent it.
var ErrProcessKey = errors.New("access: not a process key")

// ParseProcessKey reads a key ProcessToken.Key rendered for a process of the
// computer that sent it -- "<boot>:<pid>:<version>", as a relay forwards each
// process of its caller's chain -- back into its token. It is strict: the key
// must be exactly the token's own rendering (decimal, no sign, no leading
// zero, nothing around it) and the token valid, so one process never has two
// keys. A relay-qualified key is not one: the relay a process belongs to is
// the server's to say (ProcessToken.Relay), never the sender's.
func ParseProcessKey(key string) (ProcessToken, error) {
	boot, rest, ok := strings.Cut(key, ":")
	if !ok {
		return ProcessToken{}, fmt.Errorf("%w: %q", ErrProcessKey, key)
	}
	pidText, versionText, ok := strings.Cut(rest, ":")
	if !ok {
		return ProcessToken{}, fmt.Errorf("%w: %q", ErrProcessKey, key)
	}
	pid, err := strconv.ParseInt(pidText, 10, 32)
	if err != nil {
		return ProcessToken{}, fmt.Errorf("%w: %q: the pid", ErrProcessKey, key)
	}
	version, err := strconv.ParseUint(versionText, 10, 64)
	if err != nil {
		return ProcessToken{}, fmt.Errorf("%w: %q: the start time", ErrProcessKey, key)
	}
	t := ProcessToken{PID: int32(pid), Version: version, Boot: boot}
	if !t.Valid() || t.Key() != key {
		return ProcessToken{}, fmt.Errorf("%w: %q", ErrProcessKey, key)
	}
	return t, nil
}

// ChainKeys are the keys of p's kernel-verified process chain: through a relay
// (p.Relay), keys that name the relay too (ProcessToken.Key).
func (p Principal) ChainKeys() []string {
	keys := make([]string, 0, len(p.Chain))
	for _, t := range p.Chain {
		if k := t.Key(); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
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
// which every process it started shares) and a project the policy
// lists, and it ends after now. Every other lease is dropped,
// and every other view comes back unchanged, so a lease can never reach a TCP
// request, another process, or a project that is not locked at all. The socket
// may be a paired relay's (Principal.Relay): then its chain's keys name that
// relay (ProcessToken.Relay), so only a lease granted to that relay's process
// matches, never this machine's or another relay's.
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

// Locked reports whether project is locked and v holds no
// lease on it: the owner may see that it exists, and nothing inside.
// It is false for every non-owner view (a restricted account never lists such
// a project: its reach is the open projects that are not locked).
func (v View) Locked(project string) bool {
	return v.who.Kind == KindOwner && v.policy != nil && v.policy.ownerLocked[project] && !v.leased(project)
}

// Listed reports whether v may show that project exists: its slug, name and
// item counts, never anything inside it. Every project v may read is listed;
// beyond that, the owner sees a locked project -- locked, not hidden -- so an
// agent can name it in an access request.
func (v View) Listed(project string) bool {
	return v.Visible(project) || v.Locked(project)
}

// lockedRefusal is the refusal for a locked project when no request could be
// filed for it: one line with the rule, one with the remedy, which differ
// between the TCP key, a relay that names no program (wire version 1) and a
// socket -- this machine's, or a relay's that names its programs, through
// which the agent asks as it would here.
func (v View) lockedRefusal(project string) error {
	if v.who.Relay != "" && !v.who.Named() {
		return &RefusalError{msg: fmt.Sprintf("project %s is locked: even the owner's own agents need the owner's approval, "+
			"and this connection holds none. Seamless files no access request for this agent on another computer: "+
			"its relay does not say which program is asking (it speaks wire version 1), and a request is filed only for an "+
			"agent program.\n"+
			"Remedy: update Seamless on that computer, so its relay names each program; until then work in %s from an "+
			"agent session on the server's own computer, which asks for itself, and keep to other projects here.", project, project)}
	}
	if !v.who.Socket {
		return &RefusalError{msg: fmt.Sprintf("project %s is locked: the TCP connection and its bearer key never reach a "+
			"locked project, because a shared key cannot be tied to one process.\n"+
			"Remedy: work in it from an agent session whose seam mcp-proxy dials the peer_auth Unix socket; a call "+
			"there files an access request, and the owner approves it in their own terminal.", project)}
	}
	return &RefusalError{msg: fmt.Sprintf("project %s is locked: even the owner's own agents need the owner's approval, "+
		"and this connection holds none.\n"+
		"Remedy: run `seam access request %s` from the agent's shell to file an access request, and the owner "+
		"approves it in their own terminal with `seam approve` and the request's id.", project, project)}
}
