package access

import (
	"errors"
	"fmt"
	"maps"
	"os/user"
	"slices"
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/config"
)

// Account is a resolved OS account.
type Account struct {
	UID  uint32
	Name string
	// Home is the account's home directory as the directory service reports it,
	// "" when unknown. The hooks read an agent's plan files under it.
	Home string
}

// Resolver turns a configured account -- an OS user name or a numeric uid --
// into an Account. LookupAccount is the real one; tests inject their own.
type Resolver func(name string) (Account, error)

// LookupAccount resolves name through the OS user database: a numeric value is
// a uid, anything else a user name. On macOS this reaches the directory service
// (hidden service accounts live there, not in /etc/passwd).
func LookupAccount(name string) (Account, error) {
	var (
		u   *user.User
		err error
	)
	if isNumeric(name) {
		u, err = user.LookupId(name)
	} else {
		u, err = user.Lookup(name)
	}
	if err != nil {
		return Account{}, fmt.Errorf("access: resolve OS account %q: %w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return Account{}, fmt.Errorf("access: OS account %q has a non-numeric uid %q", name, u.Uid)
	}
	return Account{UID: uint32(uid), Name: u.Username, Home: u.HomeDir}, nil
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Policy is peer_auth resolved: the owner account, the locked projects and the
// restricted accounts. It is immutable once built, so one value serves every
// request without a lock.
type Policy struct {
	owner Account
	// ownerLocked is every locked project, closed to the owner except under a
	// lease (lease.go): peer_auth.force_locked and the console's own locks.
	ownerLocked     map[string]bool
	ownerLockedList []string // the same, sorted
	// forceLocked is peer_auth.force_locked alone, sorted: the locks only the
	// config file can remove.
	forceLocked []string
	// restricted is peer_auth.restricted: the restricted local accounts, by
	// uid (restricted.go).
	restricted map[uint32]Account
	// relays is peer_auth.relays: the owner's uid on each mapped relay's
	// computer, by the relay's enrolled name (RelayChainView).
	relays map[string]uint32
}

// ErrInvalidPolicy marks a peer_auth block whose accounts do not resolve or
// collide. serve refuses to start on it.
var ErrInvalidPolicy = errors.New("invalid peer_auth")

// New resolves a validated peer_auth block into a Policy. Shape problems were
// refused at config load (config.Config.Validate); what is checked here needs
// the OS: every account must resolve, and no two entries -- the owner included
// -- may resolve to one uid, since a uid listed twice would carry two reaches
// and only one could be enforced. A filled-in users key (the retired design) is
// refused here too, for a block that did not come through a load: there is
// nothing left to enforce it with, and leaving it out would quietly open what
// it meant to close.
func New(cfg config.PeerAuth, resolve Resolver) (*Policy, error) {
	if err := cfg.RetiredUsersRefusal(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPolicy, err)
	}
	if resolve == nil {
		resolve = LookupAccount
	}
	owner, err := resolve(strings.TrimSpace(cfg.Owner))
	if err != nil {
		return nil, fmt.Errorf("%w: peer_auth.owner: %w", ErrInvalidPolicy, err)
	}
	p := &Policy{
		owner:       owner,
		ownerLocked: map[string]bool{},
		restricted:  map[uint32]Account{},
		relays:      map[string]uint32{},
	}
	for name, r := range cfg.Relays {
		// The shape (a relay's name, an owner that is not root) was checked at
		// load; a block that did not come through one is held to it here.
		if name == "" || name != strings.ToLower(strings.TrimSpace(name)) || r.OwnerUID == 0 {
			return nil, fmt.Errorf("%w: peer_auth.relays %q: a relay's enrolled name, and the owner's uid there (never root)", ErrInvalidPolicy, name)
		}
		p.relays[name] = r.OwnerUID
	}
	for _, slug := range cfg.ForceLocked {
		slug = strings.TrimSpace(slug)
		if slug == "" || p.ownerLocked[slug] {
			return nil, fmt.Errorf("%w: peer_auth.force_locked lists %q: every entry must be a distinct project", ErrInvalidPolicy, slug)
		}
		p.ownerLocked[slug] = true
		p.ownerLockedList = append(p.ownerLockedList, slug)
	}
	slices.Sort(p.ownerLockedList)
	p.forceLocked = slices.Clone(p.ownerLockedList)
	if err := p.resolveRestricted(cfg, resolve); err != nil {
		return nil, err
	}
	return p, nil
}

// resolveRestricted resolves peer_auth.restricted. Every entry must resolve, and
// none may be the owner's uid or a uid listed twice (a numeric entry and a name
// can name one account): one uid carries one reach.
func (p *Policy) resolveRestricted(cfg config.PeerAuth, resolve Resolver) error {
	for _, name := range cfg.Restricted {
		acct, err := resolve(strings.TrimSpace(name))
		if err != nil {
			return fmt.Errorf("%w: peer_auth.restricted %s: %w", ErrInvalidPolicy, name, err)
		}
		if acct.UID == p.owner.UID {
			return fmt.Errorf("%w: peer_auth.restricted %s is uid %d, the owner's: the owner is never restricted", ErrInvalidPolicy, name, acct.UID)
		}
		if prev, dup := p.restricted[acct.UID]; dup {
			return fmt.Errorf("%w: peer_auth.restricted lists uid %d twice (%s and %s): name each account once", ErrInvalidPolicy, acct.UID, prev.Name, name)
		}
		p.restricted[acct.UID] = acct
	}
	return nil
}

// Owner returns the owner account.
func (p *Policy) Owner() Account { return p.owner }

// OwnerView is the view of a request the TCP bearer key authenticated: the
// owner, with no kernel-reported uid.
func (p *Policy) OwnerView() View {
	return View{policy: p, who: Principal{Kind: KindOwner, User: p.owner.Name}}
}

// PeerProcessView is PeerView for a connection whose peer process the kernel
// also reported, with no ancestors: PeerChainView of that one process.
func (p *Policy) PeerProcessView(uid uint32, proc ProcessToken) (View, bool) {
	return p.PeerChainView(uid, []ProcessToken{proc})
}

// PeerChainView is PeerView for a connection whose peer process, and that
// process's verified ancestors, the kernel reported (Peer.Chain, the peer
// first). The view carries the chain a lease is matched against (WithLeases).
// A chain that is empty, longer than MaxChainHops, or holds an invalid token,
// two boots or a relay's process (this daemon's kernel never reports one) is
// not attached at all -- the view then holds no lease, ever.
func (p *Policy) PeerChainView(uid uint32, chain []ProcessToken) (View, bool) {
	v, ok := p.PeerView(uid)
	if !ok || !chainAttachable(chain, "") {
		return v, ok
	}
	v.who.Process, v.who.HasProcess = chain[0], true
	v.who.Chain = slices.Clone(chain)
	return v, ok
}

// chainAttachable reports whether chain is one a view may carry: one to
// MaxChainHops valid tokens, of one boot, every one reported by relay ("" for
// this machine's kernel).
func chainAttachable(chain []ProcessToken, relay string) bool {
	if len(chain) == 0 || len(chain) > MaxChainHops {
		return false
	}
	for _, t := range chain {
		if !t.Valid() || t.Boot != chain[0].Boot || t.Relay != relay {
			return false
		}
	}
	return true
}

// RelayMapped reports whether peer_auth.relays maps the relay enrolled as
// name: only then are its requests judged by what it forwards
// (RelayChainView). Any other paired relay is the owner over TCP, as a bearer
// request is, and holds no lease.
func (p *Policy) RelayMapped(name string) bool {
	_, ok := p.relays[name]
	return ok
}

// RelayChainView is the view of a request a paired relay forwarded from
// another computer (plan networked-relay), as PeerChainView is this machine's
// socket's: relay is the relay's device id and name its enrolled name, which
// the server matched from the relay's TLS certificate; uid and chain are what
// the relay's own kernel reported of the local caller on the relay's socket,
// the peer first, as the relay forwarded them.
//
// The owner's uid on that computer (peer_auth.relays' owner_uid) is the owner,
// on a socket (Principal.Socket) a relay authenticated (Principal.Relay). ok
// is false for a relay the policy does not map and for any other uid: such a
// request is refused before any route runs, never served as the owner. Every
// token of the chain is the relay's (ProcessToken.Relay is set here), so a
// lease, grant or request keyed on one of its processes names the relay too;
// a chain that would not pass PeerChainView, or names another relay, is not
// attached, and the view then holds no lease.
func (p *Policy) RelayChainView(relay, name string, uid uint32, chain []ProcessToken) (View, bool) {
	owner, mapped := p.relays[name]
	if !mapped || relay == "" || uid != owner || !(ProcessToken{PID: 1, Boot: "b", Relay: relay}).Valid() {
		return View{}, false
	}
	v := View{policy: p, who: Principal{Kind: KindOwner, UID: uid, HasUID: true, User: p.owner.Name, Socket: true, Relay: relay}}
	tokens := make([]ProcessToken, len(chain))
	for i, t := range chain {
		if t.Relay != "" && t.Relay != relay {
			return v, true
		}
		t.Relay = relay
		tokens[i] = t
	}
	if !chainAttachable(tokens, relay) {
		return v, true
	}
	v.who.Process, v.who.HasProcess = tokens[0], true
	v.who.Chain = tokens
	return v, true
}

// HasLocked reports whether the policy locks any project: only then does a
// socket request of the owner need its process chain and leases looked up.
func (p *Policy) HasLocked() bool { return len(p.ownerLockedList) > 0 }

// PeerView is the view of a socket connection whose peer the kernel reported as
// uid. ok is false for a uid the policy does not know: such a connection is
// refused before any route runs.
func (p *Policy) PeerView(uid uint32) (View, bool) {
	if uid == p.owner.UID {
		return View{policy: p, who: Principal{Kind: KindOwner, UID: uid, HasUID: true, User: p.owner.Name, Socket: true}}, true
	}
	if acct, ok := p.restricted[uid]; ok {
		// No reach yet: the transport fills it per request (WithOpenProjects).
		return View{policy: p, acct: acct, who: Principal{Kind: KindRestricted, UID: uid, HasUID: true, User: acct.Name, Socket: true}}, true
	}
	return View{}, false
}

// ConsoleHidden returns every project whose content no owner surface shows
// without a lease, sorted: the locked projects. It is the console's hidden set
// (store.OpenView), what the background surfaces that serve no caller (the
// metadata feed, the commit snapshot) leave out, and what serve's preflight
// requires to be fenced.
func (p *Policy) ConsoleHidden() []string { return slices.Clone(p.ownerLockedList) }

// OwnerLocked returns every locked project, force-locked or locked from the
// console, sorted.
func (p *Policy) OwnerLocked() []string { return slices.Clone(p.ownerLockedList) }

// ForceLocked returns the projects peer_auth.force_locked lists, sorted.
func (p *Policy) ForceLocked() []string { return slices.Clone(p.forceLocked) }

// WithConsoleLocks returns a copy of p that also locks slugs, the locks the
// console set (store.ConsoleLockedSlugs). serve calls it once, before any
// request, so a Policy stays immutable while it serves. A slug the file already
// force-locks counts once, and stays force-locked.
func (p *Policy) WithConsoleLocks(slugs []string) *Policy {
	out := *p
	out.ownerLocked = make(map[string]bool, len(p.ownerLocked)+len(slugs))
	maps.Copy(out.ownerLocked, p.ownerLocked)
	out.ownerLockedList = slices.Clone(p.ownerLockedList)
	for _, slug := range slugs {
		if slug != "" && !out.ownerLocked[slug] {
			out.ownerLocked[slug] = true
			out.ownerLockedList = append(out.ownerLockedList, slug)
		}
	}
	slices.Sort(out.ownerLockedList)
	return &out
}

// HomeFor returns the home directory of the account a view speaks for, as the
// directory service reported it when the policy was built ("" when unknown).
func (v View) HomeFor() string {
	switch v.who.Kind {
	case KindRestricted:
		return v.acct.Home
	case KindOwner:
		if v.policy != nil {
			return v.policy.owner.Home
		}
	}
	return ""
}
