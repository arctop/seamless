package mcp

import (
	"context"
	"fmt"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/store"
)

// Project isolation at the tool surface. The policy itself lives in
// internal/store (CanRead/CanWrite over core.Isolation) and is never restated
// here -- this file is only about WHERE the fence is applied and WHAT a refusal
// tells the agent. store.IsolationOf appears solely to pick the state word for
// the copy, never to make the decision.
//
// Refusals state the rule and the remedy and never echo content: an error that
// quoted the memory it was withholding would leak the very thing the fence
// exists to keep in. They also do not hide that the project EXISTS -- every
// scope error already lists the machine's projects, so pretending otherwise
// would buy nothing and cost the agent its next move.
//
// The same contract binds a PERMITTED write's success payload (withholdContent):
// a confidential project takes writes from outside, and several of those writes
// answer with the item they just touched, so the response was a read across the
// fence even though the mutation was sanctioned.

// callerScope is the project a fence judges this connection's call against: its
// bound session's project, else a single unambiguous ambient project, else the
// global scope.
//
// It never errors, and it never guesses into an isolated project (ambientFallback
// refuses those outright), so a connection whose scope cannot be pinned is judged
// as global -- which owns nothing an isolated project fences, making the unpinned
// case fail closed against isolated targets rather than inheriting their access.
func (s *Server) callerScope(ctx context.Context) string {
	if b, ok := s.getBinding(ctx); ok {
		return b.project
	}
	if sess, ok, _, _ := s.ambientFallback(ctx); ok { //nolint:errcheck // a fenced refusal already forces ok=false, and "" is the fail-closed scope this returns anyway
		return sess.ProjectSlug
	}
	// A configured peer_auth account is in its own project (peerHome); everyone
	// else unpinned is judged as global.
	return peerHome(ctx)
}

// fenceRead refuses a read that would cross an isolation fence. It backs the
// read funnel (an explicit project=) and the by-id post-load checks -- those
// resolve no scope at all, so without it any caller holding a ULID reads any
// project's item.
func (s *Server) fenceRead(ctx context.Context, target string) error {
	// peer_auth first: a project this connection's OS account may not reach is
	// refused with ITS rule and remedy. The isolation copy below would advise
	// binding to the project with session_start, which peer_auth refuses too.
	if !access.Visible(ctx, target) {
		return access.Refusal(ctx, target)
	}
	caller := s.callerScope(ctx)
	allowed, err := store.CanRead(ctx, s.cfg.DB, caller, target)
	if err != nil {
		return err
	}
	if allowed {
		return nil
	}
	// CanRead judges the target's outbound fence first, so the refusal names the
	// same side it refused on. Only a sealed caller can be the other side --
	// nothing else fences inbound -- so that branch needs no second lookup.
	state, err := store.IsolationOf(ctx, s.cfg.DB, target)
	if err != nil {
		return err
	}
	if state.FencesOutbound() {
		return fmt.Errorf("project %s is %s: reads require a session bound to it%s", target, state, bindRemedy(target))
	}
	return fmt.Errorf("this session is bound to %s project %s: reads outside it are disabled",
		core.IsolationSealed, caller)
}

// fenceWrite refuses a write that would cross an isolation fence. It backs the
// write funnel (a durable create's explicit project=) and every by-id or
// by-name MUTATION -- notes_update/append/delete, tasks_update/claim/release,
// favorite_set, the memory_append/delete/supersede targets. Those resolve no
// write scope at all, so without it any caller holding a ULID edits or deletes
// a fenced project's item.
//
// The outbound half is the interesting one: an agent-initiated write OUT of a
// fenced project is a leak in its own right, and project=global is the loudest
// form of it -- a global memory is injected into every project's briefing
// indefinitely.
//
// The inbound half is deliberately asymmetric, and the asymmetry is the point: a
// sealed target admits nothing from outside, while a confidential target stays
// writable from outside, because relocating knowledge INTO the fence is how it
// gets there. store.CanWrite owns that matrix; nothing here restates it.
func (s *Server) fenceWrite(ctx context.Context, target string) error {
	if !access.Writable(ctx, target) {
		return access.Refusal(ctx, target)
	}
	caller := s.callerScope(ctx)
	allowed, err := store.CanWrite(ctx, s.cfg.DB, caller, target)
	if err != nil {
		return err
	}
	if allowed {
		return nil
	}
	// CanWrite judges the caller's outbound fence first (the mirror of fenceRead),
	// and only a sealed target can be the other side.
	state, err := store.IsolationOf(ctx, s.cfg.DB, caller)
	if err != nil {
		return err
	}
	if state.FencesOutbound() {
		return fmt.Errorf("this session is bound to %s project %s: writes outside it are disabled",
			state, caller)
	}
	return fmt.Errorf("project %s is %s: writes into it require a session bound to it%s",
		target, core.IsolationSealed, bindRemedy(target))
}

// bindRemedy completes a fence refusal with the way through it and the reason
// not to take it lightly. The fence exists so that an agent working on one
// project does not wander into another's isolated knowledge; an agent whose task
// IS that project rebinds explicitly, which is a deliberate act the call log
// records. Before this, the refusal said only "a session bound to it", and agents
// guessed at session_start(project=...) -- which did not exist -- or gave up.
func bindRemedy(project string) string {
	return " -- if your task is about " + project + ", bind this connection to it with session_start project=" +
		project + "; otherwise leave it alone"
}

// fenceProposal refuses a gardener_apply that would resolve a proposal across an
// isolation fence -- in either direction, and for a dismissal as much as an
// apply: dismissing settles a fenced project's review queue, and the call's own
// success or refusal already reports whether the proposal exists.
//
// It asks BOTH halves of the matrix about every project the proposal spans. The
// read half covers the disclosure: the apply result echoes what it touched, so a
// permitted apply would be a read across the fence. The write half covers the
// mutation, and is what stops a session bound to a confidential project from
// reaching out and reorganizing an open one -- an agent-initiated write OUT of a
// fenced project is a leak in its own right. Neither restates the policy; both go
// through fenceRead/fenceWrite, so a refusal here is the same sentence, rule and
// remedy the rest of the surface uses, with no proposal content echoed.
//
// This is the MCP-side apply-time re-check for a proposal raised BEFORE its
// project was tightened: the scope is resolved and the fence consulted now, so a
// stale reproject can no longer carry a memory out of a project that has since
// become confidential. The owner's console path is deliberately NOT gated this
// way (see gardener.FencedProjects) -- the threat model is agent-to-agent
// leakage, not hiding from the owner.
//
// A proposal whose project cannot be resolved is refused too. Fail closed: an
// unattributable row cannot be checked against any fence, and the console is
// where it gets settled.
func (s *Server) fenceProposal(ctx context.Context, p store.Proposal) error {
	scope, err := gardener.ResolveProposalScope(ctx, s.cfg.DB, p)
	if err != nil {
		return err
	}
	if !scope.Resolved {
		return fmt.Errorf("proposal %s names no project to check an isolation fence against: resolve it from the console", p.ID)
	}
	for _, project := range scope.Projects {
		if err := s.fenceRead(ctx, project); err != nil {
			return err
		}
		if err := s.fenceWrite(ctx, project); err != nil {
			return err
		}
	}
	return nil
}

// withheldKey is the field a trimmed success payload carries. Its presence is
// the whole marker: a caller can tell "policy removed this" from "the item is
// empty", which an omitted field alone could never say.
const withheldKey = "withheld"

// withholdContent trims a SUCCESSFUL write's response when the caller may write
// the target project but may not READ it -- the confidential asymmetry seen from
// the response side. fenceWrite lets the write land (relocating knowledge INTO
// the fence is how it gets there); this is what stops the answer from becoming
// the read that same fence refuses. Without it a ULID is enough to pull a
// confidential task's body out through an edit the caller is entitled to make.
//
// The write is not undone and no error is raised: it genuinely happened, and
// reporting failure for work that landed would be a fake result. The payload is
// trimmed to identity and to the caller's own operational state instead.
//
// fields is a FIXED per-tool list, not the keys that happened to be populated:
// deleting only the present ones would make the marker's contents a probe for
// which fields are set, which is one bit of the content it exists to withhold.
// The reason says so, so an absent title never reads as an empty title.
func (s *Server) withholdContent(ctx context.Context, project string, payload map[string]any, fields []string) map[string]any {
	readable, err := store.CanRead(ctx, s.cfg.DB, s.callerScope(ctx), project)
	if err != nil {
		// The answer is unknown and the write already landed, so this fails CLOSED:
		// content this call cannot vouch for must not go back, and the caller is
		// told it was withheld rather than handed a silently thinner payload.
		s.logger.Warn("mcp: response fence state", "project", project, "error", err)
		readable = false
	}
	if readable {
		return payload
	}
	for _, f := range fields {
		delete(payload, f)
	}
	payload[withheldKey] = map[string]any{
		"fields": fields,
		"reason": s.withheldReason(ctx, project),
	}
	return payload
}

// withheldReason renders the copy for a trimmed payload: the rule, the fact that
// the write itself landed (so the agent does not retry a write that succeeded),
// and the remedy. It degrades to "isolated" on a lookup failure for the same
// reason ambientFenceErr does -- the trim is already decided, and a wrong state
// word would be a fake result.
func (s *Server) withheldReason(ctx context.Context, project string) string {
	word := "isolated"
	state, err := store.IsolationOf(ctx, s.cfg.DB, project)
	if err != nil {
		s.logger.Warn("mcp: withheld state", "project", project, "error", err)
	} else {
		word = string(state)
	}
	return fmt.Sprintf("project %s is %s: the write landed, but this project's content is not returned "+
		"to a session bound elsewhere. The listed fields are omitted whether or not they are set; "+
		"read them from a session bound to it.", project, word)
}

// canReadGlobal reports whether this connection's caller may read the global
// scope. Only a sealed caller may not, and the answer is CanRead's rather than a
// second rule.
//
// It gates the by-name global FALLBACK: memory_read/append/delete,
// notes_read slug=, and favorite_set all retry the global scope when the
// project scope misses. That retry resolves no scope of its own, so it is the
// implicit twin of the project=global the fence already refuses -- left open, a
// sealed session reads the global memories its fence exists to remove from its
// world through the back door.
func (s *Server) canReadGlobal(ctx context.Context) (bool, error) {
	return store.CanRead(ctx, s.cfg.DB, s.callerScope(ctx), "")
}

// ambientFenceErr renders ambientFallback's refusal to resolve an unbound
// connection into an isolated project. It names the one remedy that applies --
// an explicit binding -- rather than the scope errors' ambiguity help, which
// would tell an agent to pass project= for a project no argument can unlock.
//
// The state word degrades to "isolated" if the lookup fails: the refusal is
// already decided by then, and a wrong word would be a fake result, so it says
// less rather than something untrue.
func (s *Server) ambientFenceErr(ctx context.Context, project string) error {
	word := "isolated"
	state, err := store.IsolationOf(ctx, s.cfg.DB, project)
	if err != nil {
		s.logger.Warn("mcp: ambient fence state", "project", project, "error", err)
	} else {
		word = string(state)
	}
	return fmt.Errorf("project %s is %s: start a session bound to it%s", project, word, bindRemedy(project))
}

// trialsForCaller runs a trial query under the isolation fence. Trials were the
// one read that saw no project at all: the query is by lab, and a lab spans
// projects on purpose so parallel agents can share one investigation -- which is
// also why the fence is a per-row check rather than a project filter, since
// pinning the query would cut the open projects the caller is entitled to see.
//
// TrialFilter.Project is still set for a caller that may read nothing outside
// its own project (the inbound fence): the rows beyond it would all be dropped,
// so narrowing spends the query's LIMIT on rows the caller can actually get.
// CanRead makes both decisions; nothing here restates the policy.
func (s *Server) trialsForCaller(ctx context.Context, f store.TrialFilter) ([]core.Trial, error) {
	caller := s.callerScope(ctx)
	allowed := make(map[string]bool, 2)
	readable := func(project string) (bool, error) {
		if ok, seen := allowed[project]; seen {
			return ok, nil
		}
		ok, err := store.CanRead(ctx, s.cfg.DB, caller, project)
		if err != nil {
			return false, err
		}
		allowed[project] = ok
		return ok, nil
	}
	if f.Project == "" && caller != "" {
		// "May this caller read the global scope?" is the cheapest probe for an
		// inbound fence, and it is a CanRead answer rather than a second rule.
		outward, err := readable("")
		if err != nil {
			return nil, err
		}
		if !outward {
			f.Project = caller
		}
	}
	trials, err := store.QueryTrials(ctx, s.cfg.DB, f)
	if err != nil {
		return nil, err
	}
	out := make([]core.Trial, 0, len(trials))
	for _, tr := range trials {
		ok, err := readable(tr.ProjectSlug)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, tr)
		}
	}
	return out, nil
}
