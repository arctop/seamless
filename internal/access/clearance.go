package access

import (
	"cmp"
	"context"
	"slices"
	"time"
)

// The clearance rule (isolation.requests, plan fenced-sessions; owner
// decisions in the fenced-sessions-design memory). The fence judges the
// conversation, not the connection's binding: what can leak is the agent's
// context, and that only grows.
//
//   - The conversation is the holder the transport vouches for: the agent's
//     kernel-verified process tree under peer_auth (a View in ctx), else the
//     client's own session (the agent client and the session id it issued),
//     found from the agent-process identity or named by the hook -- never the
//     connection's binding, never ambient inference. A client that sends no
//     identity is held by the one session it is bound to, as before.
//   - Its home is the project of its one ambient row: where its own record
//     lives. It does not limit reach.
//   - Its label is every fenced (confidential or sealed) project whose content
//     it may hold: a fenced home, every project its own sessions are in, every
//     grant target and every project it moved through. It only grows: expiry
//     and revocation end reads, never the label.
//   - A flow is an owner-approved permission for one project's content to be
//     written into another (an approval's carry, a grant's own, a move's own).
//
// The one rule: a write's destination must be cleared for everything the
// conversation holds. dest is cleared when every project in the label is dest
// or has an approved flow into dest. Open projects and the global scope are
// cleared only while the label is empty: no approval ever clears a flow into
// either (the store drops any such row before the rule sees it). The rule
// lives in store.CanWrite (through WriteCleared) and nowhere else.
//
// Like a lease or a grant, every input is data this layer judges, never data
// it trusts: JudgeConversation keeps a grant only when it names the holder ctx
// speaks for, and keeps nothing for a request no conversation is identified
// by. The label and the flows are the store's reading of P1's rows for exactly
// the holders it derived from the same ctx (store.ConversationClearance).

// Flow is one approved flow as the access layer judges it: content of From
// may be written into To.
type Flow struct {
	From, To string
}

// Conversation is the conversation a request speaks for as the store read it
// (store.ConversationClearance), for JudgeConversation.
type Conversation struct {
	// Key names the conversation without peer_auth: "<client>/<external id>",
	// the encoding Grant.Conversation carries. "" under peer_auth and for a
	// client without an external identity.
	Key string
	// Sessions are the sessions whose session-keyed grants are the
	// conversation's own, without peer_auth: every session of the
	// conversation (its ambient row, the explicit sessions that linked to it,
	// a move's children), since grants filed before migration 039 name the
	// session that asked; or the one session a client without an external
	// identity is bound to.
	Sessions []string
	// Home is the conversation's home: the project of its one ambient row (""
	// for the global scope, or when none was found).
	Home string
	// Label is every fenced project the conversation holds, as the store found
	// them fenced at the request.
	Label []string
	// Flows are the approved flows into fenced projects. A flow into an open
	// project or the global scope clears nothing while the label holds
	// anything, so the store never passes one.
	Flows []Flow
	// Grants are the grant rows of the conversation's holders, live or ended.
	Grants []Grant
}

// JudgeConversation judges the conversation c describes for the holder ctx
// speaks for, at now:
//
//   - under peer_auth (a View in ctx), only the owner's socket view with a
//     kernel-verified process chain holds anything, and a grant is kept only
//     when its process is a hop of that chain (a session's or a
//     conversation's grant never is). A TCP request or a restricted account's
//     holds nothing, and nothing is refused it here;
//   - without peer_auth, a grant is kept only when it names the conversation
//     (Key), or -- naming neither a process nor a conversation -- one of its
//     Sessions. A request no conversation and no session identifies holds
//     nothing.
//
// A kept grant is live when it was not ended and expires after now. The
// label and flows are taken as the store found them, sorted and deduplicated.
func JudgeConversation(ctx context.Context, now time.Time, c Conversation) Grants {
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
	} else if c.Key == "" && len(c.Sessions) == 0 {
		return Grants{}
	}
	g := Grants{judged: true, home: c.Home}
	for _, r := range c.Grants {
		if r.Project == "" || r.From == "" || r.Project == r.From {
			continue
		}
		switch {
		case hasView:
			// "" is never a chain key: a session's or a conversation's grant
			// holds nothing under peer_auth.
			if !chain[r.Process] {
				continue
			}
		case r.Process != "":
			continue
		case r.Conversation != "":
			if c.Key == "" || r.Conversation != c.Key {
				continue
			}
		case r.Session == "" || !slices.Contains(c.Sessions, r.Session):
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
	sortLive(g.live)
	for _, p := range c.Label {
		if p != "" {
			g.label = append(g.label, p)
		}
	}
	slices.Sort(g.label)
	g.label = slices.Compact(g.label)
	for _, f := range c.Flows {
		if f.From != "" && f.To != "" && f.From != f.To {
			g.flows = append(g.flows, f)
		}
	}
	slices.SortFunc(g.flows, compareFlows)
	g.flows = slices.Compact(g.flows)
	return g
}

func compareFlows(a, b Flow) int {
	return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.To, b.To))
}

// sortLive orders live grants by target project, the latest-ending first.
func sortLive(live []Grant) {
	slices.SortFunc(live, func(a, b Grant) int {
		if c := cmp.Compare(a.Project, b.Project); c != 0 {
			return c
		}
		return b.Expires.Compare(a.Expires)
	})
}

// Judged reports that g is a conversation JudgeConversation judged: its
// writes meet the clearance rule. A legacy judged state (JudgeGrants) and the
// zero value report false.
func (g Grants) Judged() bool { return g.judged }

// Home is the conversation's home ("" for the global scope or none known).
func (g Grants) Home() string { return g.home }

// Label returns the fenced projects the conversation holds, sorted.
func (g Grants) Label() []string { return slices.Clone(g.label) }

// Flows returns the approved flows into fenced projects, sorted.
func (g Grants) Flows() []Flow { return slices.Clone(g.flows) }

// Cleared reports whether dest ("" is the global scope) is cleared for
// everything g holds. For a conversation: every project in the label is dest
// or has an approved flow into dest; with an empty label everything is
// cleared, and with a non-empty one never the global scope (and never an open
// project: no flow into one reaches the rule). For a legacy judged state
// (JudgeGrants): the confinement its grants leave, dest one of the projects
// they were approved from. The zero value clears everything.
func (g Grants) Cleared(dest string) bool {
	if !g.judged {
		return len(g.confined) == 0 || (dest != "" && slices.Contains(g.confined, dest))
	}
	if len(g.label) == 0 {
		return true
	}
	if dest == "" {
		return false
	}
	for _, p := range g.label {
		if p != dest && !g.flowsInto(p, dest) {
			return false
		}
	}
	return true
}

func (g Grants) flowsInto(from, to string) bool {
	_, found := slices.BinarySearchFunc(g.flows, Flow{From: from, To: to}, compareFlows)
	return found
}

// ClearedProjects returns the fenced projects g's writes are cleared for,
// sorted: the label's projects and the destinations of its flows that pass
// Cleared. A refusal names them as where the conversation may still write.
// For a legacy judged state it is the confinement's projects; it is nil while
// nothing is held (then every destination is cleared).
func (g Grants) ClearedProjects() []string {
	if !g.judged {
		return slices.Clone(g.confined)
	}
	if len(g.label) == 0 {
		return nil
	}
	candidates := slices.Clone(g.label)
	for _, f := range g.flows {
		candidates = append(candidates, f.To)
	}
	slices.Sort(candidates)
	var out []string
	for _, p := range slices.Compact(candidates) {
		if g.Cleared(p) {
			out = append(out, p)
		}
	}
	return out
}

// WithRead returns g as it would stand once a read grant of target, asked
// from from, is approved: target joins the label, and its own flow into from
// is cleared when from is fenced. It is for what a request's text promises; it
// judges nothing. A state that is not a judged conversation comes back
// unchanged.
func (g Grants) WithRead(target, from string, fromFenced bool) Grants {
	if !g.judged || target == "" {
		return g
	}
	out := g
	out.label = append(slices.Clone(g.label), target)
	if fromFenced && from != "" {
		out.label = append(out.label, from)
	}
	slices.Sort(out.label)
	out.label = slices.Compact(out.label)
	out.flows = slices.Clone(g.flows)
	if fromFenced && from != "" && from != target {
		out.flows = append(out.flows, Flow{From: target, To: from})
		slices.SortFunc(out.flows, compareFlows)
		out.flows = slices.Compact(out.flows)
	}
	return out
}

// WriteCleared reports whether a write into dest ("" is the global scope) is
// cleared for everything the conversation ctx carries holds (Grants.Cleared).
// It is store.CanWrite's clearance step, which only refuses: with nothing in
// ctx -- isolation.requests off, or a request no conversation identifies --
// everything is cleared, and the fence is exactly what it was.
func WriteCleared(ctx context.Context, dest string) bool {
	g, ok := GrantsFrom(ctx)
	if !ok {
		return true
	}
	return g.Cleared(dest)
}

// WriteGranted reports whether ctx holds a live read-and-write grant of dest
// approved from from, and from is the conversation's home: the one thing that
// opens a fenced caller's outbound fence (store.CanWrite, the mirror of
// ReadGranted in CanRead). It never answers for the global scope, a legacy
// judged state or a conversation whose home is elsewhere.
func WriteGranted(ctx context.Context, from, dest string) bool {
	if from == "" || dest == "" || from == dest {
		return false
	}
	g, _ := GrantsFrom(ctx)
	if !g.judged || g.home != from {
		return false
	}
	for _, l := range g.live {
		if l.Write && l.From == from && l.Project == dest {
			return true
		}
	}
	return false
}
