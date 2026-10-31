package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/store"
)

func gardenerProposalsTool() mcp.Tool {
	return mcp.NewTool("gardener_proposals", hintRead(),
		mcp.WithDescription("List pending gardener proposals (merge/consolidate duplicate memories, archive stale memories, write a monthly session digest, reproject a memory to another project, rekind a memory to a different kind, set up a project split, abandon a never-approved captured plan, fold a stranded captured plan into the composition that carries its steps, write a memory agents keep searching for in vain, or fix an error agents keep hitting). Review, then apply, dismiss or hide each with gardener_apply. Read-only."),
		mcp.WithString("kind", enumOf(store.ProposalKinds), mcp.Description("filter by proposal kind (default: all pending)")),
	)
}

func (s *Server) handleGardenerProposals(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	kind := argString(req, "kind")
	proposals, err := store.PendingProposals(ctx, s.cfg.DB, kind)
	if err != nil {
		return errResult("gardener_proposals", err)
	}
	// The listing is the leak surface: a proposal row has no project column, but
	// its payload carries names, descriptions and similarity scores out of every
	// project it touches. Drop what this caller may not read -- silently, since a
	// withheld count would itself report how much a fenced project has going on.
	visible, err := gardener.NewProposalFence(s.cfg.DB, s.callerScope(ctx)).Filter(ctx, proposals)
	if err != nil {
		return errResult("gardener_proposals", err)
	}
	return jsonResult(map[string]any{"proposals": visible, "count": len(visible)})
}

func gardenerRequestTool() mcp.Tool {
	return mcp.NewTool("gardener_request", hintAdd(),
		mcp.WithDescription("The natural-language entry point for REORGANIZING memory. Describe the change in plain language and it returns reviewable pending proposals -- fold duplicates together (\"these two memories are duplicates -- keep the newer\"), retire stale memories (\"archive anything about the old port 8080\"), synthesize several into one (\"combine the three auth-flow notes\"), move a mis-filed memory to another EXISTING project (\"the iOS DFU memory belongs in arctop-ios\"), or reclassify a memory's kind (\"the wordmark memory is a convention, not a constraint\"). Use this whenever the user describes how they want their knowledge organized; if the intended change is ambiguous, ask them a clarifying question first. It NEVER mutates memories: it only creates pending proposals -- review with gardener_proposals, resolve with gardener_apply. If the request is to split one project into NEW child projects, it recognizes that and returns guidance (splitSource) pointing you at gardener_split instead. Needs an LLM chat client."),
		mcp.WithString("request", mcp.Required(), mcp.Description("the reorganization request in plain language")),
		mcp.WithString("project", mcp.Description("scope candidate memories: a project slug (its memories + globals), \"global\" for globals only, or \"all\" for every project on the machine. Omit to use the session's project.")),
	)
}

func (s *Server) handleGardenerRequest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.cfg.Gardener == nil {
		return errResult("gardener_request", errors.New("gardener is not configured on this server"))
	}
	text := argString(req, "request")
	if text == "" {
		return errResult("gardener_request", errors.New("request is required"))
	}
	scope, err := s.gardenerRequestScope(ctx, argString(req, "project"))
	if err != nil {
		return errResult("gardener_request", err)
	}
	res, err := s.cfg.Gardener.Request(ctx, text, scope)
	if err != nil {
		return errResult("gardener_request", err)
	}
	return jsonResult(res)
}

// gardenerRequestScope resolves gardener_request's project argument, the last
// tool argument that reached a service without passing the scope guards.
//
// Every failure it now reports used to be a success. project="_global" was not
// normalized, so ActiveMemories("_global") matched no rows and answered "no
// active memories in scope"; a typo'd slug said the same thing, indistinguishable
// from a genuinely empty project; and an omitted project scanned every project on
// the machine with no ambiguity check at all -- the standing "no automatic
// fallbacks for ambiguous requests" directive, violated in the one place left
// that could still violate it.
func (s *Server) gardenerRequestScope(ctx context.Context, explicit string) (gardener.RequestScope, error) {
	// The widening token is read before the guards: it is not a slug, and
	// normalizeProject would hand it through as one.
	if explicit == allProjectsToken {
		return gardener.RequestScope{AllProjects: true}, nil
	}
	// A read, so resolveReadScope, not Write: a genuine global read is ("", nil)
	// and only ambiguity errors.
	project, err := s.resolveReadScope(ctx, explicit)
	if err != nil {
		return gardener.RequestScope{}, err
	}
	// Existence is checked only for a slug the caller named, and only here.
	//
	// validateProjectArg checks slug SHAPE, so "typoed" passes it and lands on the
	// same silent empty success this tool exists to stop returning. But an
	// inferred project comes from a live session rather than a typo, and a
	// candidate memory may legitimately reference a project that was never
	// registered (see requestCandidates' callers) -- so this stays a
	// gardener_request rule and does not move into the shared guards, where
	// memory_read and tasks_list would start rejecting unregistered-but-real slugs.
	if explicit != "" && project != "" {
		_, found, err := store.ProjectBySlug(ctx, s.cfg.DB, project)
		if err != nil {
			return gardener.RequestScope{}, err
		}
		if !found {
			return gardener.RequestScope{}, fmt.Errorf("unknown project %q", project)
		}
	}
	return gardener.RequestScope{Project: project}, nil
}

func gardenerSplitTool() mcp.Tool {
	return mcp.NewTool("gardener_split", hintAdd(),
		mcp.WithDescription("Plan a project SPLIT: divide one existing project into two or more NEW child projects, keeping cross-platform memories in a shared parent (e.g. split arctop-app into arctop-ios + arctop-android with shared arctop-mobile-apps). Use this when the user wants to break one project into several -- gardener_request points you here (via splitSource) when it detects that intent. It NEVER creates a project or moves a memory: it only creates reviewable pending proposals -- one 'split' setup proposal plus one 'reproject' per memory, all under plan 'split-<source>'. Review with gardener_proposals, then apply each with gardener_apply (or retarget a memory first in the console). Needs an LLM chat client and a known source project slug (see project_list)."),
		mcp.WithString("source", mcp.Required(), mcp.Description("the project slug to split (its own memories are classified into the children/shared parent)")),
		mcp.WithString("instruction", mcp.Description("optional guidance: which children, what stays shared")),
	)
}

func (s *Server) handleGardenerSplit(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if s.cfg.Gardener == nil {
		return errResult("gardener_split", errors.New("gardener is not configured on this server"))
	}
	source := argString(req, "source")
	if source == "" {
		return errResult("gardener_split", errors.New("source is required"))
	}
	// peer_auth: planning a split reads the source's memories into proposals and
	// writes a plan against it, so the caller's OS account must reach it both
	// ways. (The isolation fence's own refusal of a fenced source stays in
	// gardener.Split; this is the account rule, which isolation cannot see.)
	if !access.Visible(ctx, source) || !access.Writable(ctx, source) {
		return errResult("gardener_split", access.Refusal(ctx, source))
	}
	res, err := s.cfg.Gardener.Split(ctx, source, argString(req, "instruction"))
	if err != nil {
		return errResult("gardener_split", err)
	}
	return jsonResult(res)
}

func gardenerApplyTool() mcp.Tool {
	return mcp.NewTool("gardener_apply", hintOverwrite(),
		mcp.WithDescription("Resolve a gardener proposal. action=apply carries out the effect (archive -> retire the memory; merge -> supersede the older by the newer; consolidate -> write a unified memory superseding its sources; digest -> save the summary as a note; reproject -> move the memory to another project; rekind -> reclassify the memory's kind in place; split -> create the child/shared projects, link the family, parent the children, retire the source; memory_wanted -> open a task to write the missing memory; tool_error -> open a task to fix the recurring error; merge_plans -> retag a stranded captured plan's notes onto the plan that holds the steps and settle the capture as merged). A proposal whose memory has gone inactive since it was raised is settled as dismissed instead, with the reason under settled. Rejecting has two strengths: action=dismiss discards this proposal and the evidence behind it, so a pattern that keeps recurring is raised again later; action=hide blocks the pattern permanently, so no recurrence re-raises it. Prefer dismiss unless the suggestion is wrong in principle rather than wrong for now. Both are reversible by the owner from the console."),
		mcp.WithString("id", mcp.Required(), mcp.Description("proposal id (ULID)")),
		mcp.WithString("action", enumOf(gardener.Decisions), mcp.Description("apply (default), dismiss (until it recurs), or hide (forever)")),
	)
}

func (s *Server) handleGardenerApply(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := argString(req, "id")
	if id == "" {
		return errResult("gardener_apply", errors.New("id is required"))
	}
	if s.cfg.Gardener == nil {
		return errResult("gardener_apply", errors.New("gardener is not configured on this server"))
	}
	// The proposal is loaded here rather than left to Apply/Dismiss because the
	// fence needs the payload to know which projects the row is about at all.
	p, ok, err := store.ProposalByID(ctx, s.cfg.DB, id)
	if err != nil {
		return errResult("gardener_apply", err)
	}
	if !ok {
		return errResult("gardener_apply", fmt.Errorf("no proposal with id %q", id))
	}
	if err := s.fenceProposal(ctx, p); err != nil {
		return errResult("gardener_apply", err)
	}
	action := argString(req, "action")
	if action == "" {
		action = gardener.DecisionApply
	}
	switch action {
	case gardener.DecisionApply:
		result, err := s.cfg.Gardener.Apply(ctx, id)
		if err != nil {
			return errResult("gardener_apply", err)
		}
		return jsonResult(result)
	case gardener.DecisionDismiss:
		if err := s.cfg.Gardener.Dismiss(ctx, id); err != nil {
			return errResult("gardener_apply", err)
		}
		return jsonResult(map[string]any{"id": id, "status": store.ProposalDismissed})
	case gardener.DecisionHide:
		if err := s.cfg.Gardener.Hide(ctx, id); err != nil {
			return errResult("gardener_apply", err)
		}
		return jsonResult(map[string]any{"id": id, "status": store.ProposalHidden})
	default:
		return errResult("gardener_apply", fmt.Errorf("invalid action %q: valid values are %s",
			action, strings.Join(gardener.Decisions, ", ")))
	}
}
