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
	// relays is peer_auth.relays: each mapped relay's computer, by the relay's
	// enrolled name (RelayChainView).
	relays map[string]relayPolicy
}

// relayPolicy is one mapped relay's computer: the owner's uid there, and the
// restricted accounts there (peer_auth.relays' restricted), by uid, each
// labeled with its name and the relay's (RelayAccountLabel).
type relayPolicy struct {
	owner      uint32
	restricted map[uint32]Account
}

// RelayAccountLabel is the label a restricted account on a relay's computer
// carries -- on every write it makes, and as its session namespace -- "<its
// name there>@<the relay's enrolled name>": the account and the host, so two
// computers' accounts of one name never share a label, and none shares one
// with an account on this machine.
func RelayAccountLabel(account, relay string) string { return account + "@" + relay }

// validRelayAccount accepts a restricted account's name on a relay's
// computer, as config.validatePeerRelays does at load: an OS account name
// with no space, ':', '/' or '@' (the label's separators), not starting with
// '-'.
func validRelayAccount(name string) bool {
	return name != "" && len(name) <= 64 && !strings.HasPrefix(name, "-") && !strings.ContainsAny(name, " \t\r\n:/@")
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
		relays:      map[string]relayPolicy{},
	}
	for name, r := range cfg.Relays {
		// The shape (a relay's name, an owner that is not root) was checked at
		// load; a block that did not come through one is held to it here.
		if name == "" || name != strings.ToLower(strings.TrimSpace(name)) || strings.ContainsAny(name, "@:") || r.OwnerUID == 0 {
			return nil, fmt.Errorf("%w: peer_auth.relays %q: a relay's enrolled name, and the owner's uid there (never root)", ErrInvalidPolicy, name)
		}
		rp := relayPolicy{owner: r.OwnerUID, restricted: map[uint32]Account{}}
		for acct, uid := range r.Restricted {
			switch prev, dup := rp.restricted[uid]; {
			case !validRelayAccount(acct):
				return nil, fmt.Errorf("%w: peer_auth.relays %q restricted %q is not an OS account name", ErrInvalidPolicy, name, acct)
			case uid == 0:
				return nil, fmt.Errorf("%w: peer_auth.relays %q restricted %s is uid 0, root, which reads the relay's key: "+
					"root is never a restricted account", ErrInvalidPolicy, name, acct)
			case uid == r.OwnerUID:
				return nil, fmt.Errorf("%w: peer_auth.relays %q restricted %s is uid %d, the owner's there: the owner is never "+
					"restricted", ErrInvalidPolicy, name, acct, uid)
			case dup:
				return nil, fmt.Errorf("%w: peer_auth.relays %q restricted lists uid %d twice (%s and %s): name each account once",
					ErrInvalidPolicy, name, uid, prev.Name, RelayAccountLabel(acct, name))
			}
			rp.restricted[uid] = Account{UID: uid, Name: RelayAccountLabel(acct, name)}
		}
		p.relays[name] = rp
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
	// One label, one account: a restricted account here named like a relay's
	// account label would share its sessions and its writes' label.
	for _, local := range p.restricted {
		for name, r := range p.relays {
			for _, acct := range r.restricted {
				if acct.Name == local.Name {
					return nil, fmt.Errorf("%w: peer_auth.restricted %s and peer_auth.relays %q restricted %s carry one label: "+
						"rename one", ErrInvalidPolicy, local.Name, name, acct.Name)
				}
			}
		}
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
// (RelayChainView). The transport refuses any other paired relay's requests
// under peer_auth: every account on its computer reaches its socket, and none
// is known to be the owner.
func (p *Policy) RelayMapped(name string) bool {
	_, ok := p.relays[name]
	return ok
}

// RelayChainView is RelayChainViewNamed for a relay that names no program
// (wire version 1): the view's chain carries no program, so no request is
// filed for its processes.
func (p *Policy) RelayChainView(relay, name string, uid uint32, chain []ProcessToken) (View, bool) {
	return p.RelayChainViewNamed(relay, name, uid, chain, nil)
}

// RelayChainViewNamed is the view of a request a paired relay forwarded from
// another computer (plan networked-relay), as PeerChainView is this machine's
// socket's: relay is the relay's device id and name its enrolled name, which
// the server matched from the relay's TLS certificate; uid and chain are what
// the relay's own kernel reported of the local caller on the relay's socket,
// the peer first, as the relay forwarded them, and programs (wire version 2;
// nil for version 1) the program each process of the chain runs, as that
// kernel named it.
//
// The owner's uid on that computer (peer_auth.relays' owner_uid) is the owner,
// on a socket (Principal.Socket) a relay authenticated (Principal.Relay). A
// uid the relay's restricted list names is that restricted account, labeled
// with its name and the relay's (RelayAccountLabel), with no reach until the
// transport fills it per request (WithOpenProjects), and no chain: like a
// restricted account on this machine's socket, it holds no lease, grant or
// request. ok is false for a relay the policy does not map and for any other
// uid: such a request is refused before any route runs, never served as the
// owner.
//
// Every token of the owner's chain is the relay's (ProcessToken.Relay is set
// here), so a lease, grant or request keyed on one of its processes names the
// relay too; a chain that would not pass PeerChainView, or names another
// relay, is not attached, and the view then holds no lease. programs ride the
// chain (Principal.Programs) only when there is one per process.
func (p *Policy) RelayChainViewNamed(relay, name string, uid uint32, chain []ProcessToken, programs []string) (View, bool) {
	r, mapped := p.relays[name]
	if !mapped || relay == "" || !(ProcessToken{PID: 1, Boot: "b", Relay: relay}).Valid() {
		return View{}, false
	}
	if acct, ok := r.restricted[uid]; ok {
		return View{policy: p, acct: acct, who: Principal{Kind: KindRestricted, UID: uid, HasUID: true, User: acct.Name,
			Socket: true, Relay: relay}}, true
	}
	if uid != r.owner {
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
	if len(programs) == len(tokens) {
		v.who.Programs = slices.Clone(programs)
	}
	return v, true
}

// RelayRestrictedAccounts returns the restricted accounts peer_auth.relays
// maps on the relay enrolled as name, by uid there, for doctor and the
// startup log; each Name is its label (RelayAccountLabel).
func (p *Policy) RelayRestrictedAccounts(name string) []Account {
	r := p.relays[name]
	uids := slices.Sorted(maps.Keys(r.restricted))
	out := make([]Account, 0, len(uids))
	for _, uid := range uids {
		out = append(out, r.restricted[uid])
	}
	return out
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
