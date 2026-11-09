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
//
// With read grants on as well (isolation.requests), a confidential project
// no lock covers is asked for as a read grant, as `seam access request` asks
// (console grantRequest): the same request a read refused at its fence files.
// With them off, such a project is "not locked", word for word as before.
//
// An asker that already holds live access to the project -- its tree's lease,
// or a read grant from its project -- is answered with it and files nothing
// until access.ExpiryWarning before it ends; from then on asking again files
// a new approval: a refresh, or a new read grant request
// (approval.AccessLiveError).

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
			"say why. Asking again finds the request already waiting rather than filing another, and a reason given then "+
			"fills in one the waiting request lacks. Once the owner denies a request, refused calls file nothing more and "+
			"say so: ask again here, with a reason, when the owner tells you to. When this agent's client "+
			"can show an approval dialog, the call shows the owner the request and waits up to 110 seconds for the decision; "+
			"otherwise it returns at once with the request's short id and how the owner approves it. An approval opens the "+
			"project to this agent's process tree until the time the result names: retry the work then. An agent that "+
			"already holds the access it asks for is answered with it, and nothing is filed, until 30 minutes before it "+
			"ends. With read grants on (isolation.requests), it also asks for a read of a confidential project no lock "+
			"covers."),
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
	// Kind is request, refresh for a tree whose lease on the project ends
	// within the expiry warning (approving replaces it), or grant for a read
	// grant of a confidential project (isolation.requests).
	Kind string `json:"kind"`
	// From is a read grant's asking project; absent for a lock's request.
	From string `json:"from,omitempty"`
	// Status is pending, approved, denied or expired. A live lease that
	// covers the ask answers approved, with the request that opened it.
	Status string `json:"status"`
	// OpenUntil is when the approved access ends (RFC 3339).
	OpenUntil string `json:"open_until,omitempty"`
	// Dialog is how the in-session approval dialog ended (accepted, declined,
	// cancelled, unoffered, unanswered, or -- accepted with Touch ID --
	// touchid_cancelled when the owner cancelled the Touch ID prompt and
	// touchid_closed when the helper closed without approving); absent when
	// none was shown.
	Dialog string `json:"dialog,omitempty"`
	// Page is the request's passkey approval page, when a passkey can
	// approve it there (passkeys set up, and one enrolled).
	Page string `json:"page,omitempty"`
	// Next is what the agent does now.
	Next string `json:"next"`
}

func (s *Server) handleAccessRequest(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	start := time.Now()
	// A restricted account (peer_auth.restricted) files nothing: a lock is
	// the owner's, and no approval opens one -- or anything else beyond the
	// open projects -- to such an account. Refused before the project is even
	// read, so the answer is the same whatever it names.
	if account := access.AccountOf(ctx); account != "" {
		return errResult(AccessRequestTool, restrictedAccessRequestRefusal(account))
	}
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
	if s.cfg.Grants != nil && !lockCovers(ctx, project) {
		return s.accessRequestGrant(ctx, start, project, reason)
	}
	a, err := s.fileAccessRequest(ctx, project, reason, approval.ViaRequest)
	var live *approval.AccessLiveError
	if errors.As(err, &live) {
		return jsonResult(s.liveAccessResult(ctx, live))
	}
	if err != nil {
		return errResult(AccessRequestTool, accessRequestError(err))
	}
	t := dialogTarget{ID: a.ID, Short: approval.ShortID(a.ID), Project: a.Project}
	ways := callWays(ctx, s.cfg.Approvals.Ways(ctx, a.ID))
	out := accessRequestResult{
		ID: a.ID, ShortID: t.Short, Project: a.Project, Kind: a.Kind, Status: s.stateOf(a), Page: ways.Page,
	}
	return s.accessRequestAnswer(ctx, start, t, ways, out)
}

// lockCovers reports whether project is a lock's (peer_auth.locked) for the
// call in ctx: locked to it, or leased by its tree. A read grant is asked
// only for a project no lock covers.
func lockCovers(ctx context.Context, project string) bool {
	v, ok := access.FromContext(ctx)
	if !ok {
		return false
	}
	_, leased := v.Lease(project)
	return leased || v.Locked(project)
}

// accessRequestGrant files (or finds) the read grant request for a
// confidential project no lock covers, as a read refused at its fence would
// (grantRequestRefusal), and answers like a lock's request: the dialog when
// it can run, else at once.
func (s *Server) accessRequestGrant(ctx context.Context, start time.Time, project, reason string) (*mcp.CallToolResult, error) {
	a, err := s.cfg.Grants.RequestGrant(ctx, approval.GrantInput{
		Project: project, SessionID: s.boundSession(ctx), AgentProcess: callerAgentProcess(ctx), Host: s.callerHost(ctx),
		Reason: reason, Via: approval.ViaRequest,
	})
	var live *approval.AccessLiveError
	if errors.As(err, &live) {
		return jsonResult(s.liveAccessResult(ctx, live))
	}
	if err != nil {
		return errResult(AccessRequestTool, grantRequestError(err))
	}
	t := dialogTarget{ID: a.ID, Short: approval.ShortID(a.ID), Project: a.Project, Grant: true}
	ways := callWays(ctx, s.cfg.Grants.Ways(ctx, a.ID))
	out := accessRequestResult{
		ID: a.ID, ShortID: t.Short, Project: a.Project, Kind: a.Kind, From: a.FromProject, Status: s.stateOf(a), Page: ways.Page,
	}
	return s.accessRequestAnswer(ctx, start, t, ways, out)
}

// liveAccessResult is the answer to an ask live access already covers -- a
// lease, or a read grant from the same project: the request that opened it,
// approved, until it ends; nothing filed.
func (s *Server) liveAccessResult(ctx context.Context, live *approval.AccessLiveError) accessRequestResult {
	out := accessRequestResult{
		Project: live.Project, From: live.From, Status: stateApproved, OpenUntil: live.Expires.UTC().Format(time.RFC3339),
		Next: live.Answer() + "; keep working there until then",
	}
	if live.ApprovalID != "" {
		out.ID, out.ShortID = live.ApprovalID, approval.ShortID(live.ApprovalID)
		if a, err := store.AccessApprovalByID(ctx, s.cfg.DB, live.ApprovalID); err == nil {
			out.Kind = a.Kind
		}
	}
	return out
}

// accessRequestAnswer runs the dialog for request t when it can and answers
// with out as the dialog left it, else at once.
func (s *Server) accessRequestAnswer(ctx context.Context, start time.Time, t dialogTarget, ways access.Ways,
	out accessRequestResult) (*mcp.CallToolResult, error) {
	if end, ran := s.dialog(ctx, start, t); ran {
		out.Status, out.Dialog = end.state, end.answer
		if out.Dialog == answerNone {
			out.Dialog = "unanswered"
		}
		if end.state == stateApproved && !end.until.IsZero() {
			out.OpenUntil = end.until.UTC().Format(time.RFC3339)
		}
		out.Next = accessRequestNext(t, ways, end)
		return jsonResult(out)
	}
	out.Next = accessRequestNext(t, ways, dialogEnd{state: out.Status})
	return jsonResult(out)
}

// restrictedAccessRequestRefusal is access_request's answer to a restricted
// account: nothing is filed, and nothing it could file would open a project.
func restrictedAccessRequestRefusal(account string) error {
	return errors.New(access.RestrictedNoRequests(account))
}

// accessRequestNext says what the agent does once the request is where end
// left it; ways are the approval paths the daemon offers for it.
func accessRequestNext(t dialogTarget, ways access.Ways, end dialogEnd) string {
	switch end.state {
	case stateApproved:
		return "the owner approved it: " + openLine(t, end) + "; retry the work that needs it"
	case stateDenied:
		return "the owner denied it: keep to other projects, and " + access.AskAgain(t.Project, true)
	case stateExpired:
		return "it expired before the owner decided: ask again only if the work still needs " + t.Project
	}
	lead := ""
	switch end.answer {
	case answerTouchIDCancelled:
		lead = "Touch ID was cancelled, so nothing was approved and " + t.what() + " is still pending: "
	case answerTouchIDClosed:
		lead = "the Touch ID approver closed without approving, so " + t.what() + " is still pending: "
	}
	return lead + ways.OwnerApproves(t.Short) + "; keep to other projects until it is approved, then retry the work that needs " + t.Project
}

// accessRequestError words the approval service's refusals for an agent, as
// the console's access request route does.
func accessRequestError(err error) error {
	var anchor *approval.AnchorError
	switch {
	case errors.As(err, &anchor):
		// Its own two lines: the rule, then the remedy. No request exists.
		return anchor
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

// grantRequestError words the read grants' refusals for an agent, as the
// console's access request route does for `seam access request`
// (console grantRequest). The reason's length is checked before filing.
func grantRequestError(err error) error {
	var anchor *approval.AnchorError
	switch {
	case errors.As(err, &anchor):
		return fmt.Errorf("%s.\nRemedy: %s", strings.Replace(anchor.Rule(), "no access request", "no read request", 1), anchor.Remedy())
	case errors.Is(err, approval.ErrSealed):
		return errors.New("a sealed project is never granted, and a sealed project's session is never granted another: nothing to request")
	case errors.Is(err, approval.ErrNotConfidential):
		return errors.New("that project is neither locked nor confidential: nothing to request")
	case errors.Is(err, approval.ErrNoProject):
		return errors.New("the asking session works in no project: a read grant opens a project to another project, never to the global scope")
	case errors.Is(err, approval.ErrNoProcess):
		return errors.New("this connection has no verified process: request access from an agent session whose seam dials the peer_auth socket")
	case errors.Is(err, approval.ErrNoSession), errors.Is(err, approval.ErrNoAnchor), errors.Is(err, approval.ErrAnchorNotInChain):
		return errors.New("no agent session names this connection's agent process: ask from the agent's own session, whose seam names the agent that started it")
	case errors.Is(err, access.ErrNotReachable):
		return errors.New("that project is not reachable from this connection")
	}
	return err
}
