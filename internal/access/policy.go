package access

import (
	"errors"
	"fmt"
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
// (the hidden role accounts live there, not in /etc/passwd).
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

// Policy is peer_auth resolved: the owner account, each configured account with
// its reach, and which projects are sealed to whom. It is immutable once built,
// so one value serves every request without a lock.
type Policy struct {
	owner       Account
	users       map[uint32]*userRule
	sealedTo    map[string]Account // project -> the one account that may reach it
	ownerHidden []string           // every sealed project, sorted: none is the owner's
}

type userRule struct {
	acct     Account
	allowed  map[string]bool
	sealed   []string
	projects []string
	home     string
}

// ErrInvalidPolicy marks a peer_auth block whose accounts do not resolve or
// collide. serve refuses to start on it.
var ErrInvalidPolicy = errors.New("invalid peer_auth")

// New resolves a validated peer_auth block into a Policy. Shape problems were
// refused at config load (config.Config.Validate); what is checked here needs
// the OS: every account must resolve, and no two entries -- the owner included
// -- may resolve to one uid, since a uid listed twice would carry two reaches
// and only one could be enforced.
func New(cfg config.PeerAuth, resolve Resolver) (*Policy, error) {
	if resolve == nil {
		resolve = LookupAccount
	}
	owner, err := resolve(strings.TrimSpace(cfg.Owner))
	if err != nil {
		return nil, fmt.Errorf("%w: peer_auth.owner: %w", ErrInvalidPolicy, err)
	}
	p := &Policy{
		owner:    owner,
		users:    map[uint32]*userRule{},
		sealedTo: map[string]Account{},
	}
	names := make([]string, 0, len(cfg.Users))
	for name := range cfg.Users {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		acct, err := resolve(name)
		if err != nil {
			return nil, fmt.Errorf("%w: peer_auth.users.%s: %w", ErrInvalidPolicy, name, err)
		}
		if acct.UID == owner.UID {
			return nil, fmt.Errorf("%w: peer_auth.users.%s is uid %d, the owner's: the owner takes no users entry", ErrInvalidPolicy, name, acct.UID)
		}
		if prev, dup := p.users[acct.UID]; dup {
			return nil, fmt.Errorf("%w: peer_auth.users lists uid %d twice (%s and %s): give each account one entry", ErrInvalidPolicy, acct.UID, prev.acct.Name, name)
		}
		u := cfg.Users[name]
		rule := &userRule{acct: acct, allowed: map[string]bool{}}
		for _, slug := range u.Sealed {
			slug = strings.TrimSpace(slug)
			rule.sealed = append(rule.sealed, slug)
			rule.allowed[slug] = true
			p.sealedTo[slug] = acct
		}
		for _, slug := range u.Projects {
			slug = strings.TrimSpace(slug)
			rule.projects = append(rule.projects, slug)
			rule.allowed[slug] = true
		}
		switch {
		case len(rule.sealed) > 0:
			rule.home = rule.sealed[0]
		case len(rule.projects) > 0:
			rule.home = rule.projects[0]
		default:
			return nil, fmt.Errorf("%w: peer_auth.users.%s reaches no project", ErrInvalidPolicy, name)
		}
		p.users[acct.UID] = rule
	}
	for slug := range p.sealedTo {
		p.ownerHidden = append(p.ownerHidden, slug)
	}
	slices.Sort(p.ownerHidden)
	return p, nil
}

// Owner returns the owner account.
func (p *Policy) Owner() Account { return p.owner }

// OwnerView is the view of a request the TCP bearer key authenticated: the
// owner, with no kernel-reported uid.
func (p *Policy) OwnerView() View {
	return View{policy: p, who: Principal{Kind: KindOwner, User: p.owner.Name}}
}

// PeerView is the view of a socket connection whose peer the kernel reported as
// uid. ok is false for a uid the policy does not know: such a connection is
// refused before any route runs.
func (p *Policy) PeerView(uid uint32) (View, bool) {
	if uid == p.owner.UID {
		return View{policy: p, who: Principal{Kind: KindOwner, UID: uid, HasUID: true, User: p.owner.Name, Socket: true}}, true
	}
	if rule, ok := p.users[uid]; ok {
		return View{policy: p, user: rule, who: Principal{Kind: KindUser, UID: uid, HasUID: true, User: rule.acct.Name, Socket: true}}, true
	}
	return View{}, false
}

// SealedProjects returns every project sealed to a configured account, sorted.
func (p *Policy) SealedProjects() []string { return slices.Clone(p.ownerHidden) }

// Projects returns every project the policy names, sorted and de-duplicated.
func (p *Policy) Projects() []string {
	var out []string
	for _, rule := range p.users {
		for slug := range rule.allowed {
			if !slices.Contains(out, slug) {
				out = append(out, slug)
			}
		}
	}
	slices.Sort(out)
	return out
}

// AccountRow describes one configured account for doctor and the startup log.
type AccountRow struct {
	Account  Account
	Sealed   []string
	Projects []string
	Home     string
}

// Accounts returns the configured accounts (the owner excluded), by uid.
func (p *Policy) Accounts() []AccountRow {
	uids := make([]uint32, 0, len(p.users))
	for uid := range p.users {
		uids = append(uids, uid)
	}
	slices.Sort(uids)
	out := make([]AccountRow, 0, len(uids))
	for _, uid := range uids {
		r := p.users[uid]
		out = append(out, AccountRow{Account: r.acct, Sealed: slices.Clone(r.sealed), Projects: slices.Clone(r.projects), Home: r.home})
	}
	return out
}

// HomeFor returns the home directory of the account a view speaks for, as the
// directory service reported it when the policy was built ("" when unknown).
func (v View) HomeFor() string {
	switch v.who.Kind {
	case KindUser:
		if v.user != nil {
			return v.user.acct.Home
		}
	case KindOwner:
		if v.policy != nil {
			return v.policy.owner.Home
		}
	}
	return ""
}
