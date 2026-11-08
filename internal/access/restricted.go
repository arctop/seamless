package access

import (
	"context"
	"fmt"
	"slices"
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
