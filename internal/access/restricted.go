package access

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// Restricted local accounts (peer_auth.restricted, 2026-11-07).
//
// A restricted account is a second OS account on the machine whose agents work
// beside the owner's. The kernel names it on the Unix socket, as it names the
// owner; it never holds the bearer key, so a TCP request is never one. Its
// reach is read from the projects table for every request, not written in the
// config: every project that is open (the isolation state) and not locked,
// reading and writing, plus the global scope read-only. A confidential,
// isolation-sealed or locked project is outside it, and so is a project that
// does not exist yet: the account cannot create one (the owner creates and maps
// projects), which is what lets its refusal read the same for a project it may
// not reach and a slug nobody registered. Such a project is not listed for it
// either.
//
// It files no access requests (View.Locked is the owner's alone), gets no read
// grants (JudgeGrants keeps owner grants only) and holds no leases
// (WithLeases), so nothing it does can open a project for it. Its writes are
// labeled with its account name (Account), and a constraint, convention or
// stage it writes waits for the owner's promotion (the MCP write tools hold it
// as a proposal).

// WithOpenProjects returns a restricted account's view with its reach for this
// request: the projects among open that peer_auth does not lock (or seal to a
// users account). The transport passes every project whose isolation is open;
// the policy, not the caller, decides what of it the account may reach. Every
// other view comes back unchanged. A restricted view never given its projects
// reaches none of them.
func (v View) WithOpenProjects(open []string) View {
	if v.who.Kind != KindRestricted {
		return v
	}
	reach := make(map[string]bool, len(open))
	for _, slug := range open {
		if slug == "" || v.policy == nil || v.policy.ownerLocked[slug] {
			continue
		}
		if _, sealed := v.policy.sealedTo[slug]; sealed {
			continue
		}
		reach[slug] = true
	}
	v.open = reach
	return v
}

// Account returns the restricted account this view speaks for -- the name every
// write it makes is labeled with -- and "" for every other view: the owner's
// own writes carry no label, exactly as before.
func (v View) Account() string {
	if v.who.Kind != KindRestricted {
		return ""
	}
	return v.who.User
}

// AccountOf is View.Account for the View in ctx: the restricted account a
// request speaks for, or "" (the owner, background work, peer_auth off).
func AccountOf(ctx context.Context) string {
	v, _ := FromContext(ctx)
	return v.Account()
}

// SessionAccountOK reports whether the request in ctx may act in a session
// labeled with account (core.Session.Account): resume it, bind to it, write its
// findings, claim work under its name. A session belongs to the principal that
// started it, so a restricted account acts only in its own sessions and every
// other caller only in sessions no restricted account started. With peer_auth
// off no session carries an account and every call passes.
func SessionAccountOK(ctx context.Context, account string) bool {
	return AccountOf(ctx) == account
}

// SessionAccountRefusal is the error a surface returns when SessionAccountOK
// says no. It names the session the caller named and nothing about its owner.
func SessionAccountRefusal(session string) error {
	return &RefusalError{msg: fmt.Sprintf("session %s is not reachable from this connection: a session belongs to the "+
		"account that started it -- start your own with session_start", session)}
}

// A restricted account's sessions live in a namespace of their own (SEAL A2).
// Session names are unique across the instance, so a name another principal
// holds -- in a project the account may not reach as much as in one it does --
// would otherwise answer differently from a free one: an oracle for which
// names exist where it cannot look. Every session such an account starts is
// stored under "<account>:<name>" (its name and the agent's external session
// id both), and every name it gives is read inside that namespace, so nothing
// it names can reach a session it did not start. The owner names one by that
// qualified name; console and CLI show it. The owner's names are stored and
// read exactly as given, so with peer_auth off -- and for every owner surface
// -- nothing changes.
//
// An OS account name never contains ":" (config.validateAccount refuses it, and
// a passwd entry cannot hold one), so the first ":" always ends the account.
const sessionNamespaceSep = ":"

// QualifiedSessionName returns name inside account's session namespace:
// "<account>:<name>", or name itself when it already carries that prefix, when
// it is empty, or when account is "" (the owner, background work, peer_auth
// off). Applying it twice changes nothing, so a restricted account may name
// its own session either way: by the name it gave, or by the qualified name
// the session reports.
func QualifiedSessionName(account, name string) string {
	if account == "" || name == "" {
		return name
	}
	prefix := account + sessionNamespaceSep
	if strings.HasPrefix(name, prefix) {
		return name
	}
	return prefix + name
}

// QualifySessionName is QualifiedSessionName for the caller in ctx: a
// restricted account's name, or agent session id, inside its namespace; every
// other caller's unchanged.
func QualifySessionName(ctx context.Context, name string) string {
	return QualifiedSessionName(AccountOf(ctx), name)
}

// SessionNamespace returns the prefix the caller in ctx's sessions carry --
// "<account>:" for a restricted account -- and "" for every other caller.
func SessionNamespace(ctx context.Context) string {
	if account := AccountOf(ctx); account != "" {
		return account + sessionNamespaceSep
	}
	return ""
}

// ReservedSessionName reports the restricted account whose namespace name lies
// in, when the caller in ctx is not that account: a name beginning
// "<account>:" for an account peer_auth.restricted lists. A session the owner
// started under such a name would sit in the account's namespace, where the
// account's own name would collide with it, so the owner may name one there
// (to be refused it, SessionAccountRefusal) but never start one. A restricted
// caller's names are always qualified with its own account first, so they are
// never reserved. With peer_auth off, or no restricted account listed, no name
// is.
func ReservedSessionName(ctx context.Context, name string) (account string, reserved bool) {
	v, ok := FromContext(ctx)
	if !ok || v.policy == nil || v.who.Kind == KindRestricted {
		return "", false
	}
	for _, acct := range v.policy.restricted {
		if acct.Name != "" && strings.HasPrefix(name, acct.Name+sessionNamespaceSep) {
			return acct.Name, true
		}
	}
	return "", false
}

// ReservedSessionNameRefusal is the error for a session name another
// account's namespace holds (ReservedSessionName).
func ReservedSessionNameRefusal(name, account string) error {
	return &RefusalError{msg: fmt.Sprintf("session name %s is in restricted account %s's namespace: names beginning %s%s "+
		"are its sessions -- choose another name", name, account, account, sessionNamespaceSep)}
}

// RestrictedNoRequests is the answer to a restricted account that asks for
// access (access_request, or seam access request from its shell): nothing
// is filed, and nothing it could file would open a project.
func RestrictedNoRequests(account string) string {
	return fmt.Sprintf("account %s files no access requests: a restricted account reaches the open projects that "+
		"are not locked, and reads the global scope; the owner's approval never opens anything else to it, so "+
		"there is nothing to request -- keep to the projects project_list shows", account)
}

// restrictedRefusal is a restricted account's refusal. It reads the same for a
// confidential, sealed or locked project and for a slug nobody registered, so a
// refusal never tells the account which projects exist beyond its reach.
func restrictedRefusal(account, project string) error {
	if project == "" {
		return &RefusalError{msg: fmt.Sprintf("the global scope is read-only for account %s: a restricted account reads "+
			"it and writes only into open projects -- name one with project=<slug>", account)}
	}
	return &RefusalError{msg: fmt.Sprintf("project %s is not reachable from account %s: a restricted account reaches "+
		"open projects that are not locked, and reads the global scope; it cannot create a project either -- the "+
		"owner creates and maps them", project, account)}
}

// HasRestricted reports whether the policy lists any restricted account: only
// then does a socket request need the open projects looked up, and only then
// does the owner's console offer the proposals restricted accounts file.
func (p *Policy) HasRestricted() bool { return len(p.restricted) > 0 }

// RestrictedAccounts returns the restricted accounts, by uid, for doctor and
// the startup log.
func (p *Policy) RestrictedAccounts() []Account {
	uids := make([]uint32, 0, len(p.restricted))
	for uid := range p.restricted {
		uids = append(uids, uid)
	}
	slices.Sort(uids)
	out := make([]Account, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.restricted[uid])
	}
	return out
}
