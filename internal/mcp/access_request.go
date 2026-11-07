package mcp

// access_request: ask the owner for access to a locked project
// (peer_auth.locked), explicitly, with a reason. A call refused at the lock
// already files a request (locked.go); this is the agent's way to ask before
// it needs the project, or to say why. It files through the approval
// service's one path, as the console's POST /console/access/request does, so
// the anchor and process checks, and the errors, are the same.
//
// It is registered only while the daemon locks a project
// (Config.OfferAccessRequest): every other install's tools/list and ToolCount
// are unchanged. Catalog lists it, and the docs say when it is present.

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/store"
)

// AccessRequestTool is the access_request tool's name: the one tool only a
// server that locks a project registers (LockedOnlyTools).
const AccessRequestTool = "access_request"

// accessReasonParam is the agent's own words, shown to the owner labeled as
// such and never recorded in a tool.call event.
const accessReasonParam = "reason"

func accessRequestTool() mcp.Tool {
	return mcp.NewTool(AccessRequestTool, hintSet(),
		mcp.WithDescription("Ask the owner for access to a locked project. Present only on a server that locks projects "+
			"(peer_auth.locked), and answered only on its peer_auth socket. A locked project is closed even to the owner's "+
			"own agents until the owner approves this agent's request with a proof (Touch ID, a passkey, or seam approve in "+
			"their own terminal); a call refused at the lock already files one, so use this to ask ahead of the work, or to "+
			"say why. Asking again finds the request already waiting rather than filing another. When this agent's client "+
			"can show an approval dialog, the call shows the owner the request and waits up to two minutes for the decision; "+
			"otherwise it returns at once with the request's short id and how the owner approves it. An approval opens the "+
			"project to this agent's process tree until the time the result names: retry the work then."),
		mcp.WithString("project", mcp.Required(), mcp.Description("the locked project's slug")),
		mcp.WithString(accessReasonParam, mcp.Description(fmt.Sprintf("optional: why the work needs the project, in at "+
			"most %d characters. The owner sees it labeled as this agent's own words.", store.MaxAccessReasonRunes))),
	)
}

// accessRequestResult is access_request's answer.
type accessRequestResult struct {
	ID      string `json:"id"`
	ShortID string `json:"short_id"`
	Project string `json:"project"`
	// Kind is request, or refresh for a tree that already holds a lease on
	// the project (approving replaces it).
	Kind string `json:"kind"`
	// Status is pending, approved, denied or expired.
	Status string `json:"status"`
	// OpenUntil is when the approved access ends (RFC 3339).
	OpenUntil string `json:"open_until,omitempty"`
	// Dialog is how the in-session approval dialog ended (accepted, declined,
	// cancelled, unoffered, or unanswered); absent when none was shown.
	Dialog string `json:"dialog,omitempty"`
	// Page is the request's passkey approval page, when passkeys are set up.
	Page string `json:"page,omitempty"`
	// Next is what the agent does now.
	Next string `json:"next"`
}

func (s *Server) handleAccessRequest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start := time.Now()
	// Resolving the session may meet a refusal at the lock: it must not file
	// a second request, nor offer a dialog of its own.
	ctx = access.WithLockRequester(ctx, nil)
	project, err := validateProjectArg(argString(req, "project"))
	if err != nil {
		return errResult(AccessRequestTool, err)
	}
	if project = normalizeProject(project); project == "" {
		return errResult(AccessRequestTool, errors.New("name a project: the global scope is never locked"))
	}
	reason := argString(req, accessReasonParam)
	if utf8.RuneCountInString(reason) > store.MaxAccessReasonRunes {
		return errResult(AccessRequestTool, fmt.Errorf("the reason is limited to %d characters", store.MaxAccessReasonRunes))
	}
	if s.cfg.Approvals == nil {
		return errResult(AccessRequestTool, errors.New("this server locks no project: nothing to request"))
	}
	a, err := s.fileAccessRequest(ctx, project, reason, "request")
	if err != nil {
		return errResult(AccessRequestTool, accessRequestError(err))
	}
	t := dialogTarget{ID: a.ID, Short: approval.ShortID(a.ID), Project: a.Project}
	out := accessRequestResult{
		ID: a.ID, ShortID: t.Short, Project: a.Project, Kind: a.Kind, Status: s.stateOf(a), Page: s.cfg.Approvals.Page(a.ID),
	}
	if end, ran := s.dialog(ctx, start, t); ran {
		out.Status, out.Dialog = end.state, end.answer
		if out.Dialog == answerNone {
			out.Dialog = "unanswered"
		}
		if end.state == stateApproved && !end.until.IsZero() {
			out.OpenUntil = end.until.UTC().Format(time.RFC3339)
		}
		out.Next = accessRequestNext(t, out.Page, end)
		return jsonResult(out)
	}
	out.Next = accessRequestNext(t, out.Page, dialogEnd{state: out.Status})
	return jsonResult(out)
}

// accessRequestNext says what the agent does once the request is where end
// left it.
func accessRequestNext(t dialogTarget, page string, end dialogEnd) string {
	switch end.state {
	case stateApproved:
		return "the owner approved it: " + openLine(t, end) + "; retry the work that needs it"
	case stateDenied:
		return "the owner denied it: keep to other projects"
	case stateExpired:
		return "it expired before the owner decided: ask again only if the work still needs " + t.Project
	}
	how := fmt.Sprintf("the owner approves it in their own terminal with `seam approve %s`", t.Short)
	if page != "" {
		how += ", or with a passkey at " + page
	}
	return how + "; keep to other projects until it is approved, then retry the work that needs " + t.Project
}

// accessRequestError words the approval service's refusals for an agent, as
// the console's access request route does.
func accessRequestError(err error) error {
	switch {
	case errors.Is(err, approval.ErrNoProcess):
		return errors.New("this connection has no verified process: request access from an agent session whose seam dials the peer_auth socket")
	case errors.Is(err, approval.ErrNoAnchor), errors.Is(err, approval.ErrAnchorNotInChain):
		return errors.New("no agent session names this connection's agent process: ask from the agent's own session, whose seam names the agent that started it")
	case errors.Is(err, approval.ErrNotLocked):
		return errors.New("that project is not locked: nothing to request")
	case errors.Is(err, store.ErrInvalidAccessRequest):
		return fmt.Errorf("the reason is limited to %d characters", store.MaxAccessReasonRunes)
	}
	return err
}
