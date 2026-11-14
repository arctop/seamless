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
//     session: a TCP request, or a restricted account's, holds none;
//   - without peer_auth (no View), the session the connection is bound to --
//     identification, never ambient inference -- and never a process.
//
// What a grant opens is narrow on purpose (store.CanRead): reads of a
// CONFIDENTIAL target, from the one project the grant was approved from, after
// the View has had its word. It never opens a sealed project and never lets a
// sealed project's session read out. A read-and-write grant (migration 039)
// reads the same way, and also opens its holder's home's outbound fence for
// its target (WriteGranted, clearance.go).
//
// JudgeGrants is the judge of the previous release: it keeps the rows of a
// holder ctx speaks for and confines that holder's writes to the projects its
// grants were approved from (WriteConfined's meaning before the clearance
// rule). The MCP edge and the hooks now judge the whole conversation instead
// (JudgeConversation, clearance.go): its grants, its label, its approved
// flows and its home, so that every write meets the one clearance rule.

// Grant is one approved grant as the access layer judges it. Project is the
// target it opens, From the project it was approved from. Its holder is
// exactly one of: Process, the ProcessToken.Key of the agent process it was
// granted to (peer_auth); Conversation, the conversation it was granted to
// without peer_auth (migration 039: the agent client and the session id it
// issued, "<client>/<external id>"); Session, the session it was granted to
// (no peer_auth, a grant from before migration 039 or of a client without
// hooks). Ended reports a grant revoked before its expiry. Write reports a
// read-and-write grant (migration 039): it reads like any grant, and also
// opens its holder's home's outbound fence for its target (WriteGranted).
type Grant struct {
	ID           string
	Project      string
	From         string
	Process      string
	Session      string
	Conversation string
	Expires      time.Time
	Ended        bool
	Write        bool
}

// Grants is one request's judged state: the live grants and the projects a
// legacy judge confines the holder to writing (JudgeGrants), or -- judged by
// JudgeConversation -- the conversation's label, approved flows and home as
// well. The zero value holds nothing and refuses nothing.
type Grants struct {
	live     []Grant
	confined []string

	// judged reports a conversation judged by JudgeConversation: then the
	// clearance rule (Cleared) decides writes, and confined is only the
	// From projects of its grants, kept for Confined's readers.
	judged bool
	home   string
	// homeKnown reports that home is the project of the conversation's own
	// ambient row (Conversation.HomeKnown), not a stand-in.
	homeKnown bool
	label     []string
	flows     []Flow
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
// the View and only for a confidential target. A live read-and-write grant
// is a live grant, so it reads too.
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

// WriteConfined reports whether a write into target is refused: it is
// !WriteCleared, kept for the previous release's callers. For a legacy
// judged state (JudgeGrants) that is the confinement a grant left behind --
// the holder was granted a read, and target is none of the projects its
// grants were approved from (the global scope included); for a conversation
// (JudgeConversation) it is the clearance rule. With nothing in ctx nothing is
// refused.
func WriteConfined(ctx context.Context, target string) bool {
	return !WriteCleared(ctx, target)
}
