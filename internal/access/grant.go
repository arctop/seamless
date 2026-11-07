package access

import (
	"context"
	"slices"
	"time"
)

// Read grants (isolation.requests, step G of the locked-projects plan): a read
// refused at a confidential project's fence can become a request the owner
// approves. The approval is a grant: read-only, of one project (the target),
// to one holder working from one project (From), for a limited time.
//
// A grant is data this layer judges, never data it trusts -- the same rule as
// a lease (lease.go). The surface that serves a request loads the grant rows
// for whoever the request speaks for and hands them to JudgeGrants, which
// keeps a row only when it names that very holder:
//
//   - under peer_auth (a View in ctx), a process of the owner's socket
//     connection's kernel-verified chain (the agent process the request was
//     anchored to, so every process it started shares the grant), and never a
//     session: a TCP request, or a configured account's, holds none;
//   - without peer_auth (no View), the session the connection is bound to --
//     identification, never ambient inference -- and never a process.
//
// What a grant opens is narrow on purpose (store.CanRead): reads of a
// CONFIDENTIAL target, from the one project the grant was approved from, after
// the View has had its word. It never opens a sealed project, never lets a
// sealed project's session read out, and never lets anything write
// (store.CanWrite consults only the confinement a grant leaves behind, which
// only ever refuses).
//
// Confinement: a holder that was ever granted a read writes only into the
// projects its grants were approved from, for the rest of its life -- a
// session's life without peer_auth, the agent process's under it -- whether
// or not a grant is still live. Content it read may be in its context, and the
// owner approved copying it into exactly those projects.

// Grant is one approved read grant as the access layer judges it. Project is
// the target it opens, From the project it was approved from. Process is the
// ProcessToken.Key of the agent process it was granted to (peer_auth), Session
// the session it was granted to (no peer_auth); exactly one is set. Ended
// reports a grant revoked before its expiry.
type Grant struct {
	ID      string
	Project string
	From    string
	Process string
	Session string
	Expires time.Time
	Ended   bool
}

// Grants is one request's judged grants: the live ones, and the projects the
// holder is confined to writing. The zero value holds nothing and confines
// nothing.
type Grants struct {
	live     []Grant
	confined []string
}

// JudgeGrants keeps the grants in rows that name the holder ctx speaks for
// (see the file comment): with a View, the owner's socket view's process
// chain; without one, sessionID. A grant is live when it was not ended and
// expires after now; every kept grant, live or not, confines the holder's
// writes to its From project.
func JudgeGrants(ctx context.Context, sessionID string, now time.Time, rows []Grant) Grants {
	v, hasView := FromContext(ctx)
	var chain map[string]bool
	if hasView {
		if v.who.Kind != KindOwner || !v.who.Socket || !v.who.HasProcess {
			return Grants{}
		}
		chain = make(map[string]bool, len(v.who.Chain))
		for _, t := range v.who.Chain {
			if k := t.Key(); k != "" {
				chain[k] = true
			}
		}
	} else if sessionID == "" {
		return Grants{}
	}
	var g Grants
	for _, r := range rows {
		if r.Project == "" || r.From == "" || r.Project == r.From {
			continue
		}
		if hasView {
			// A session-keyed grant has no process, and "" is never a chain key.
			if !chain[r.Process] {
				continue
			}
		} else if r.Process != "" || r.Session != sessionID {
			continue
		}
		if !slices.Contains(g.confined, r.From) {
			g.confined = append(g.confined, r.From)
		}
		if !r.Ended && r.Expires.After(now) {
			g.live = append(g.live, r)
		}
	}
	slices.Sort(g.confined)
	slices.SortFunc(g.live, func(a, b Grant) int {
		if a.Project != b.Project {
			if a.Project < b.Project {
				return -1
			}
			return 1
		}
		return b.Expires.Compare(a.Expires)
	})
	return g
}

// Live returns the live grants, by target project (the latest first).
func (g Grants) Live() []Grant { return slices.Clone(g.live) }

// Confined returns the projects the holder may still write, sorted, and
// whether it is confined at all.
func (g Grants) Confined() ([]string, bool) {
	return slices.Clone(g.confined), len(g.confined) > 0
}

type grantsKey struct{}

// WithGrants returns ctx carrying g. The MCP edge installs it on every tool
// call while isolation.requests is on.
func WithGrants(ctx context.Context, g Grants) context.Context {
	return context.WithValue(ctx, grantsKey{}, g)
}

// GrantsFrom returns the Grants ctx carries, and whether it carries any.
func GrantsFrom(ctx context.Context) (Grants, bool) {
	g, ok := ctx.Value(grantsKey{}).(Grants)
	return g, ok
}

// ReadGranted reports whether ctx holds a live grant of target to a caller in
// from. It is store.CanRead's one question about grants, asked only after
// the View and only for a confidential target.
func ReadGranted(ctx context.Context, from, target string) bool {
	if from == "" || target == "" || from == target {
		return false
	}
	g, _ := GrantsFrom(ctx)
	for _, l := range g.live {
		if l.From == from && l.Project == target {
			return true
		}
	}
	return false
}

// GrantedTargets returns the targets of ctx's live grants approved from
// project, sorted: the projects that join its recall scope while they live.
func GrantedTargets(ctx context.Context, from string) []string {
	if from == "" {
		return nil
	}
	g, _ := GrantsFrom(ctx)
	var out []string
	for _, l := range g.live {
		if l.From == from && !slices.Contains(out, l.Project) {
			out = append(out, l.Project)
		}
	}
	slices.Sort(out)
	return out
}

// WriteConfined reports whether a write into target is refused by the
// confinement a grant left behind: the holder was granted a read, and target
// is none of the projects its grants were approved from (the global scope
// included). With no grants in ctx nothing is confined.
func WriteConfined(ctx context.Context, target string) bool {
	g, _ := GrantsFrom(ctx)
	if len(g.confined) == 0 {
		return false
	}
	return target == "" || !slices.Contains(g.confined, target)
}
