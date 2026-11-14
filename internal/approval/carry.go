package approval

// The kind of a request for a fenced project, and the carry it states
// (isolation.requests, plan fenced-sessions; migration 039).
//
// The kind follows from the conversation's home and the target, never from
// the agent's choice (KindFor):
//
//   - an open home (or the global scope) and a confidential target: a move;
//   - a confidential home and a confidential target: a grant, read-only or,
//     when the owner chooses, read and write;
//   - any home and a sealed target: a move.
//
// No grant ever involves a sealed project, and a sealed home asks for no
// confidential project.
//
// Every grant or move states the whole carry (store.Carry): the home, every
// fenced project the conversation holds once approved (its label,
// store.HolderLabel, plus the target), and every project it may write to once
// approved (the home, every project a live write grant from the home opens,
// and for a move only the target). Approving clears each flow the carry lists
// (store.Carry.Flows), so the facts the owner approves over name all of them.

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// KindFor is the kind of request a conversation homed in home ("" for the
// global scope) files for target: store.AccessKindMove or
// store.AccessKindGrant. ErrSealed is a confidential target asked from a
// sealed home; ErrNotConfidential a target that is neither confidential nor
// sealed (or no project at all); ErrIsHome the home itself.
func (g *Grants) KindFor(ctx context.Context, home, target string) (string, error) {
	return kindFor(ctx, g.svc, home, target)
}

func kindFor(ctx context.Context, s *Service, home, target string) (string, error) {
	if home == target {
		return "", fmt.Errorf("%w: %s", ErrIsHome, target)
	}
	ti, err := store.IsolationOf(ctx, s.cfg.DB, target)
	if err != nil {
		return "", err
	}
	hi := core.IsolationOpen
	if home != "" {
		if hi, err = store.IsolationOf(ctx, s.cfg.DB, home); err != nil {
			return "", err
		}
	}
	switch {
	case ti == core.IsolationSealed:
		return store.AccessKindMove, nil
	case ti != core.IsolationConfidential:
		return "", fmt.Errorf("%w: %s", ErrNotConfidential, target)
	case hi == core.IsolationSealed:
		return "", ErrSealed
	case hi == core.IsolationConfidential:
		return store.AccessKindGrant, nil
	}
	return store.AccessKindMove, nil
}

// carryFor is the carry a request of kind for target states, for the
// conversation who asks for, homed in home: what it holds once approved (its
// label and the target) and where it may write once approved -- for a move
// the target alone; for a grant the home, every project a live write grant
// from the home opens to it, and the target too when write is asked for.
//
// The label is the one the clearance judges (store.ConversationClearance),
// read for the holder ctx speaks for -- under peer_auth that adds the fenced
// projects of the process tree's other conversations, which the asker's own
// holders do not name -- together with every holder's own label
// (store.HolderLabel), so neither reading can understate what is held.
func (s *Service) carryFor(ctx context.Context, who asker, home, target, kind string, write bool, now time.Time) (store.Carry, error) {
	holders := who.held
	holds := []string{target}
	for chunk := range slices.Chunk(holders, holderChunk) {
		label, err := store.HolderLabel(ctx, s.cfg.DB, chunk, home)
		if err != nil {
			return store.Carry{}, err
		}
		for _, e := range label {
			holds = append(holds, e.Project)
		}
	}
	ref := store.ConversationRef{SessionID: who.sess.ID}
	if who.home.ExternalClient != "" && who.home.ExternalSessionID != "" {
		ref = store.ConversationRef{Client: who.home.ExternalClient, ExternalID: who.home.ExternalSessionID}
	}
	judged, err := store.ConversationClearance(ctx, s.cfg.DB, ref, now)
	if err != nil {
		return store.Carry{}, err
	}
	holds = append(holds, judged.Label()...)
	if kind == store.AccessKindMove {
		return store.CarryOf(home, holds, []string{target})
	}
	writeTo := []string{home}
	grants, err := heldGrants(ctx, s.cfg.DB, holders)
	if err != nil {
		return store.Carry{}, err
	}
	for _, g := range grants {
		if g.Access == store.GrantAccessWrite && g.FromProject == home && g.Project != target &&
			g.RevokedAt == nil && g.ExpiresAt.After(now) {
			writeTo = append(writeTo, g.Project)
		}
	}
	if write {
		writeTo = append(writeTo, target)
	}
	return store.CarryOf(home, holds, writeTo)
}

// holderChunk is how many holders one store lookup names (the store refuses
// more than a process chain's hops): a conversation may have linked more
// sessions than that.
const holderChunk = access.MaxChainHops

// heldGrants is every grant any of holders was ever approved, each once,
// read holderChunk holders at a time.
func heldGrants(ctx context.Context, db *sql.DB, holders []store.Holder) ([]store.AccessGrant, error) {
	var out []store.AccessGrant
	seen := map[string]bool{}
	for chunk := range slices.Chunk(holders, holderChunk) {
		rows, err := store.HolderAccessGrants(ctx, db, chunk)
		if err != nil {
			return nil, err
		}
		for _, g := range rows {
			if !seen[g.ID] {
				seen[g.ID] = true
				out = append(out, g)
			}
		}
	}
	return out, nil
}

// CarryFacts is a request's carry as its facts state it (Facts.Carry): the
// home ("" for the global scope), every fenced project the conversation
// holds once approved, and every project it may write to once approved.
type CarryFacts struct {
	Home    string   `json:"home"`
	Holds   []string `json:"holds"`
	WriteTo []string `json:"write_to"`
}

// carryFacts is c as the facts state it, the target a write destination
// exactly when write: a grant's facts state its default (read-only)
// approval, and the read-and-write line says what the other adds.
func carryFacts(c store.Carry, target string, write bool) *CarryFacts {
	out := &CarryFacts{Home: c.Home, Holds: slices.Clone(c.Holds), WriteTo: []string{}}
	for _, d := range c.WriteTo {
		if d != target {
			out.WriteTo = append(out.WriteTo, d)
		}
	}
	if write {
		out.WriteTo = append(out.WriteTo, target)
		slices.Sort(out.WriteTo)
	}
	return out
}

// GrantLine is the line every surface shows for a grant whose request states
// its carry: where the target's content may go once approved -- the home
// first, then every other project the conversation may write to.
func GrantLine(c CarryFacts, target string) string {
	return "content from " + target + " may be copied into " + JoinAnd(grantDestinations(c, target))
}

// ReadWriteLine is what approving a grant as read and write lets happen, as
// one line: the grant's line, then that the agent may change the target and
// that everything it holds may be copied into it.
func ReadWriteLine(c CarryFacts, target string) string {
	s := GrantLine(c, target) + ", and it may change " + target + "'s tasks, memories and notes"
	if held := without(c.Holds, target); len(held) > 0 {
		s += "; content from " + JoinAnd(held) + " may be copied into " + target
	}
	return s
}

// grantDestinations are a grant's write destinations other than its target,
// the home first.
func grantDestinations(c CarryFacts, target string) []string {
	out := []string{c.Home}
	for _, d := range c.WriteTo {
		if d != c.Home && d != target {
			out = append(out, d)
		}
	}
	return out
}

// MoveLine is the line every surface shows for a move into target: where the
// agent worked and what it read, that it works in target from now on and what
// it saw may be written there, and that it can write nowhere it was before.
// A sealed target's line also names what the move carries in, which the seal
// otherwise keeps out. process says how long that lasts: until the agent
// process exits under peer_auth, until the conversation ends without it.
func MoveLine(c CarryFacts, target string, sealed, process bool) string {
	home := c.Home
	if home == "" {
		home = "the global scope"
	}
	read := without(c.Holds, target, c.Home)
	s := "This agent has worked in " + home
	if len(read) > 0 {
		s += " and read " + JoinAnd(read)
	}
	s += ". If you approve, it works in " + target + " from now on, and what it saw may be written into " + target + "."
	if sealed {
		s += " " + target + " is sealed, so nothing enters it otherwise: approving lets in what this agent carries from " +
			JoinAnd(append([]string{home}, read...)) + "."
	}
	until := "the conversation ends"
	if process {
		until = "the agent process exits"
	}
	if c.Home == "" {
		return s + " It can no longer write to the global scope until " + until + "."
	}
	return s + " It can no longer write to " + c.Home + " or the global scope until " + until + "."
}

// JoinAnd joins names as a sentence lists them: "a", "a and b", "a, b and c".
func JoinAnd(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// without is list less every one of drop, in order.
func without(list []string, drop ...string) []string {
	var out []string
	for _, v := range list {
		if !slices.Contains(drop, v) {
			out = append(out, v)
		}
	}
	return out
}

// kindFacts fills in what a grant's or a move's facts say beyond a lock's:
// its carry and the line saying what approving it lets happen (a grant's
// copy line, a move's line), the accesses the owner may choose between with
// what read and write adds, and whether its target is locked -- an approval
// then opens the lock too. targetIso is the target's isolation now. A grant
// that states no carry (an open project's read grant, or one filed before
// migration 039) keeps the copy line it always had.
func (s *Service) kindFacts(ctx context.Context, a store.AccessApproval, targetIso core.Isolation, f *Facts) error {
	warn := func(code, text string) { f.Warnings = append(f.Warnings, Warning{Code: code, Text: text}) }
	switch a.Kind {
	case store.AccessKindGrant:
		if a.Carry.IsZero() {
			warn(WarnCopyInto, CopyIntoLine(a.Project, a.FromProject))
			break
		}
		f.Carry = carryFacts(a.Carry, a.Project, false)
		warn(WarnCopyInto, GrantLine(*f.Carry, a.Project))
		homeIso, err := store.IsolationOf(ctx, s.cfg.DB, a.FromProject)
		if err != nil {
			return err
		}
		if homeIso == core.IsolationConfidential && targetIso == core.IsolationConfidential {
			// Read and write, between confidential projects only: never from
			// an open project (that is a move) and never with a sealed one.
			f.AccessChoices = []string{accessReadOnly, accessReadWrite}
			f.AskedAccess = accessReadOnly
			if a.RequestedAccess == store.GrantAccessWrite {
				f.AskedAccess = accessReadWrite
			}
			f.ReadWrite = ReadWriteLine(*carryFacts(a.Carry, a.Project, true), a.Project)
		}
	case store.AccessKindMove:
		f.Carry = carryFacts(a.Carry, a.Project, true)
		warn(WarnMove, MoveLine(*f.Carry, a.Project, targetIso == core.IsolationSealed, a.ProcessKey != ""))
	}
	locked, err := s.lockedNow(ctx, a.Project)
	if err != nil {
		return err
	}
	f.Locked = locked
	return nil
}
