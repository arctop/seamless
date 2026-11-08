package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// Restricted local accounts at the knowledge tools (peer_auth.restricted).
//
// A restricted account writes the open projects it reaches, and every memory
// or note it writes carries its account name (core.Memory.Account,
// core.Note.Account) for as long as the content it wrote is the content on
// disk. What it cannot do is change what binds every session: a write that
// lands, replaces, supersedes or deletes a memory of a briefing-injected kind
// (screening.BriefingKinds) is a PROPOSAL. It goes to write screening's held
// queue -- whether or not the screening feature is on -- and lands only when
// the owner promotes it (the held queue's Release). The held queue is outside
// the corpus, so a proposal is in no briefing, recall, injection or gardener
// pass by construction. When screening is also on and holds the same write for
// its own reasons, both reason sets ride ONE hold.
//
// Nothing here applies to the owner: access.AccountOf is "" for the owner, for
// background work and on every install without restricted accounts, and each
// check below then answers exactly as before.

// errNoProposalQueue refuses a would-be proposal on a server built without the
// held queue: a proposal that cannot be held must never be written instead.
var errNoProposalQueue = errors.New("this write is a proposal (a restricted account's constraint, convention, " +
	"protocol or stage waits for the owner's promotion), but this server has no held queue to keep it in, so it " +
	"was refused and nothing was written -- write another kind, or ask the owner to make the change")

// proposalMessage is what a proposed write tells the agent: nothing landed,
// the owner decides, and rephrasing will not get past it.
const proposalMessage = "Proposed: a restricted account's constraint, convention, protocol or stage waits for the " +
	"owner's promotion. Nothing in the corpus changed; the owner promotes or discards it (the console's Held " +
	"screen, or `seam promote <id>`). Do not retry or rephrase it to get past the proposal."

// memoryJudge is one memory write call's admission state: whether write
// screening judges it, which restricted account makes it, and the verdict
// once judged.
type memoryJudge struct {
	tool    string
	screen  bool   // the screening feature judges this call
	account string // the restricted account making it ("" = the owner)
	session string // the bound session the hold names
	tainted bool   // the session carries the outside-text mark (screening only)
	jd      judged
}

// newMemoryJudge reads a memory write's admission state from the request.
// The session lookups run only when something will need them, so an owner's
// write with screening off costs exactly what it always did.
func (s *Server) newMemoryJudge(ctx context.Context, tool string) *memoryJudge {
	j := &memoryJudge{tool: tool, screen: s.screeningOn(ctx), account: access.AccountOf(ctx)}
	switch {
	case j.screen:
		j.session = s.boundSession(ctx)
		j.tainted = s.sessionTainted(ctx, j.session)
	case j.account != "":
		j.session = s.boundSession(ctx)
	}
	return j
}

// active reports whether the call is judged at all: screened, or made by a
// restricted account whose write may be a proposal.
func (j *memoryJudge) active() bool { return j.screen || j.account != "" }

// proposes reports whether a restricted account's write touching these kinds
// is a proposal: any of them -- the kind it writes, the kind it replaces in
// place, the kind it supersedes or deletes -- is a briefing-injected kind.
func (j *memoryJudge) proposes(kinds ...core.MemoryKind) bool {
	if j.account == "" {
		return false
	}
	for _, k := range kinds {
		if screening.IsBriefingKind(k) {
			return true
		}
	}
	return false
}

// heldAnswer is the tool answer for a write that went to the held queue: a
// restricted account's proposal, or a write screening held.
func heldAnswer(h store.HeldWrite, fields map[string]any) (*mcp.CallToolResult, error) {
	if screening.IsProposal(h.Reasons) {
		return proposedResult(h, fields)
	}
	return heldResult(h, fields)
}

// proposedResult is the tool answer for a proposal: a success result (the
// proposal was accepted into the owner's queue), never an error an agent
// would retry.
func proposedResult(h store.HeldWrite, fields map[string]any) (*mcp.CallToolResult, error) {
	out := map[string]any{
		"status":      "proposed",
		"hold":        map[string]any{"id": h.ID, "reasons": h.Reasons},
		"proposed_by": h.DetailValue(screening.DetailAccount),
		"message":     proposalMessage,
	}
	for k, v := range fields {
		out[k] = v
	}
	return jsonResult(out)
}

// proposalReasons puts the restricted-account reason first, ahead of any
// reason screening found, so a proposal reads as one in every listing.
func proposalReasons(reasons []screening.Code) []screening.Code {
	return append([]screening.Code{screening.ReasonRestrictedAccount}, reasons...)
}

// errSupersedeIsProposal refuses a restricted account's supersede whose target
// turned out, after the write landed, to be a briefing-injected kind: the
// target was not one when the call was judged (it changed in between, or was
// created meanwhile), so the supersede was never filed as a proposal and must
// not run as a plain write.
func errSupersedeIsProposal(target string, kind core.MemoryKind) error {
	return fmt.Errorf("%q is now a %s, and a restricted account's supersede of one is a proposal -- "+
		"retry the memory_write to file it for the owner's promotion", target, kind)
}

// ownerCurationRefusal refuses a restricted account a tool that curates what
// every session sees: the owner's alone.
func ownerCurationRefusal(account, rule, remedy string) error {
	return fmt.Errorf("not open to account %s: %s -- %s", account, rule, remedy)
}

// noteTextChanged reports whether a notes_update patch rewrites the note's
// title or description: authored text every index line shows, so a
// restricted account's change of either carries its label (accountAfter).
func noteTextChanged(note core.Note, p noteUpdatePatch) bool {
	return (p.title != nil && *p.title != note.Title) || (p.description != nil && *p.description != note.Description)
}

// accountAfter is the account label a memory or note carries after a write:
// a body change re-stamps it with the writer (the owner's rewrite clears it,
// as a rewrite re-stamps Model), and a restricted account's change of the
// description or title stamps it too, because that text is what every index
// and briefing line shows. Anything else -- tags, a star, an owner's
// description fix -- keeps the label the content already carries, so the
// label never stops naming an account whose words are still the content.
func accountAfter(current, account string, bodyChanged, textChanged bool) string {
	if bodyChanged || (textChanged && account != "") {
		return account
	}
	return current
}
