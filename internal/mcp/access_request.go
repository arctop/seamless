package mcp

// access_request: ask the owner for access to a locked project,
// explicitly, with a reason. A call refused at the lock
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
//
// write (with read grants on) asks for what the asker's home and the project
// call for (approval.Grants.Ask): between confidential projects a grant that
// asks for read and write -- the owner still chooses -- and from an open
// project, or into a sealed one, a move into the project for good. Without
// read grants it changes nothing: a lock's lease reads and writes already.
//
// In the advisory tier (read grants on without peer_auth; plan fenced-sessions
// P9) it is the explicit ask the in-session dialog answers there, registered
// with words of its own (accessRequestToolFor): nothing is locked, so every
// ask is a grant or a move, and the dialog's Approve decides it
// (advisory_dialog.go). Whether a daemon registers it is OffersAccessRequest.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
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

// accessWriteParam asks for read and write, or a move (accessRequestAsk).
const accessWriteParam = "write"

func accessRequestTool() mcp.Tool {
	return mcp.NewTool(AccessRequestTool, hintSet(),
		mcp.WithDescription("Ask the owner for access to a locked project. Present only on a server that locks projects "+
			"(peer_auth), and answered only on its peer_auth socket. A locked project is closed even to the owner's "+
			"own agents until the owner approves this agent's request with a proof (Touch ID, a passkey, or seam approve in "+
			"their own terminal); a call refused at the lock already files one, so use this to ask ahead of the work, or to "+
			"say why. Asking again finds the request already waiting rather than filing another, and a reason given then "+
			"fills in one the waiting request lacks. The answer says which: filed is true when this call filed a new "+
			"request, false when it found one already waiting or the access is already held (nothing filed); reason is "+
			"added when this call's reason was stored on the request, kept when the waiting request already had one, so "+
			"this one was not used, and none when this call gave no reason or nothing was filed. Once the owner denies a "+
			"request, refused calls file nothing more and "+
			"say so: ask again here, with a reason, when the owner tells you to. When this agent's client "+
			"can show an approval dialog, the call shows the owner the request and waits up to 110 seconds for the decision; "+
			"otherwise it returns at once with the request's short id and how the owner approves it. An approval opens the "+
			"project to this agent's process tree until the time the result names: retry the work then. An agent that "+
			"already holds the access it asks for is answered with it, and nothing is filed, until 30 minutes before it "+
			"ends. With read grants on (isolation.requests), it also asks for a read of a confidential project no lock "+
			"covers, and write asks to change the project too."),
		mcp.WithString("project", mcp.Required(), mcp.Description("the locked project's slug")),
		mcp.WithString(accessReasonParam, mcp.Description(fmt.Sprintf("optional: why the work needs the project, in at "+
			"most %d characters. The owner sees it labeled as this agent's own words.", store.MaxAccessReasonRunes))),
		mcp.WithBoolean(accessWriteParam, mcp.Description("optional, with read grants on (isolation.requests): ask to "+
			"change the project as well as read it. From a confidential project it asks for a read-and-write grant, which "+
			"the owner may still approve read-only; from an open project, or for a sealed one, it asks to move this "+
			"conversation into the project for good, after which it can no longer write where it works now, so record "+
			"what belongs there first. Without read grants a locked project's approval reads and writes already.")),
	)
}

// OffersAccessRequest reports whether a daemon registers access_request
// (Config.OfferAccessRequest): while peer_auth locks a project, and -- the
// advisory tier's explicit ask, which its in-session dialog answers (plan
// fenced-sessions P9) -- while read grants (isolation.requests) are on without
// peer_auth. Every other install registers none: its tools/list, ToolCount
// and doctor line are unchanged.
func OffersAccessRequest(locksProjects, peerAuth, readGrants bool) bool {
	return locksProjects || (!peerAuth && readGrants)
}

// accessRequestToolFor is access_request as this server registers it: in the
// advisory tier, where nothing is locked and no proof is asked, in words of
// its own (advisoryAccessRequestTool); under peer_auth with read grants on,
// in words that say what the switch asks for (grantsAccessRequestTool); on
// every other server accessRequestTool, the Catalog's, word for word.
func (s *Server) accessRequestToolFor() mcp.Tool {
	switch {
	case s.advisoryTier():
		return advisoryAccessRequestTool()
	case s.cfg.Grants != nil:
		return grantsAccessRequestTool()
	}
	return accessRequestTool()
}

// grantsAccessRequestTool is access_request on a server that locks a project
// under peer_auth with read grants on (isolation.requests): the same name,
// arguments and hints as accessRequestTool, worded for what the switch files.
// A locked project is still the lock's to open, and asking for it without
// write asks for its lease; a confidential or sealed project no lock covers is
// asked for as its fence's refusal asks (approval.Grants.RequestGrant: the
// kind the conversation's home and the project call for); and write asks for
// the move or grant the home and the project call for, which opens a locked
// target's lock too (approval.Grants.Ask).
func grantsAccessRequestTool() mcp.Tool {
	return mcp.NewTool(AccessRequestTool, hintSet(),
		mcp.WithDescription("Ask the owner for a locked, confidential or sealed project. Present only on a server that "+
			"locks projects (peer_auth), and answered only on its peer_auth socket; this server also has isolation.requests "+
			"on. A locked project is closed even to the owner's own agents until the owner approves this agent's request with "+
			"a proof (Touch ID, a passkey, or seam approve in their own terminal), and without write the ask is for its lease. "+
			"For a confidential or sealed project no lock covers, what is asked follows from the project this conversation "+
			"works in and the one it names: from a confidential project, a grant of another confidential one (read-only "+
			"unless write is set, and the owner still chooses); from an open project, or for a sealed one, a move of this "+
			"conversation into the project for good, which only a passkey or seam approve approves. With write, a locked "+
			"project is asked for the same way, and the approval opens its lock too: a lease alone opens a locked project "+
			"only to the conversation that works in it. A call refused at the lock or at a fence already files a request, "+
			"so use this to ask ahead of the work, or to say why. Asking again finds the request already waiting rather "+
			"than filing another, and a reason given then fills in one the waiting request lacks. The answer says which: "+
			"filed is true when this call filed a new request, false when it found one already waiting or the access is "+
			"already held (nothing filed); reason is added when this call's reason was stored on the request, kept when the "+
			"waiting request already had one, so this one was not used, and none when this call gave no reason or nothing "+
			"was filed. Once the owner denies a request, refused calls file nothing more and say so: ask again here, with a "+
			"reason, when the owner tells you to. When this agent's client can show an approval dialog, the call shows the "+
			"owner the request and waits up to 110 seconds for the decision; otherwise it returns at once with the "+
			"request's short id and how the owner approves it. An approved lease or grant opens the project until the time "+
			"the result names, and an approved move lasts for good: retry the work then. An agent that already holds the "+
			"access it asks for is answered with it, and nothing is filed, until 30 minutes before it ends."),
		mcp.WithString("project", mcp.Required(), mcp.Description("the locked, confidential or sealed project's slug")),
		mcp.WithString(accessReasonParam, mcp.Description(fmt.Sprintf("optional: why the work needs the project, in at "+
			"most %d characters. The owner sees it labeled as this agent's own words.", store.MaxAccessReasonRunes))),
		mcp.WithBoolean(accessWriteParam, mcp.Description("optional: ask to work in the project, not only read it. From a "+
			"confidential project it asks for a read-and-write grant, which the owner may still approve read-only; from an "+
			"open project, or for a sealed one, it asks to move this conversation into the project for good, after which "+
			"it can no longer write where it works now, so record what belongs there first. For a locked project the "+
			"approval opens the lock too; for the locked project this conversation already works in, it asks for the "+
			"lock's lease, which reads and writes already.")),
	)
}

// advisoryAccessRequestTool is access_request in the advisory tier: the same
// name, arguments and hints as accessRequestTool, worded for a daemon without
// peer_auth whose read grants are on.
func advisoryAccessRequestTool() mcp.Tool {
	return mcp.NewTool(AccessRequestTool, hintSet(),
		mcp.WithDescription("Ask the owner for a confidential or sealed project. Present with read grants on "+
			"(isolation.requests) on a server without peer_auth. A call refused at such a project's fence already files a "+
			"request, so use this to ask ahead of the work, or to say why. What is asked follows from the project this "+
			"conversation works in and the one it names: from a confidential project, a grant of another confidential one "+
			"(read-only unless write is set, and the owner still chooses); from an open project, or for a sealed one, a move "+
			"of this conversation into the project for good. Asking again finds the request already waiting rather than "+
			"filing another, and a reason given then fills in one the waiting request lacks. The answer says which: filed is "+
			"true when this call filed a new request, false when it found one already waiting or the access is already held "+
			"(nothing filed); reason is added when this call's reason was stored on the request, kept when the waiting "+
			"request already had one, so this one was not used, and none when this call gave no reason or nothing was "+
			"filed. Once the owner denies a request, refused calls file nothing more and say so: ask again here, with a "+
			"reason, when the owner tells you to. When this agent's client can show an approval dialog, the call shows the "+
			"owner the request there and waits up to 110 seconds for the decision; otherwise it returns at once with the "+
			"request's short id and how the owner approves it. An approved grant opens the project until the time the result "+
			"names, and an approved move lasts for good: retry the work then. An agent that already holds the access it asks "+
			"for is answered with it, and nothing is filed, until 30 minutes before it ends."),
		mcp.WithString("project", mcp.Required(), mcp.Description("the confidential or sealed project's slug")),
		mcp.WithString(accessReasonParam, mcp.Description(fmt.Sprintf("optional: why the work needs the project, in at "+
			"most %d characters. The owner sees it labeled as this agent's own words.", store.MaxAccessReasonRunes))),
		mcp.WithBoolean(accessWriteParam, mcp.Description("optional: from a confidential project, ask to change the other "+
			"project as well as read it -- a read-and-write grant, which the owner may still approve read-only. From an open "+
			"project, or for a sealed one, the ask is a move into the project for good either way, after which this "+
			"conversation can no longer write where it works now, so record what belongs there first.")),
	)
}

// accessRequestResult is access_request's answer.
type accessRequestResult struct {
	ID      string `json:"id"`
	ShortID string `json:"short_id"`
	Project string `json:"project"`
	// Kind is request, refresh for a tree whose lease on the project ends
	// within the expiry warning (approving replaces it), grant for a grant of
	// a confidential project, or move for a move into a fenced project
	// (isolation.requests).
	Kind string `json:"kind"`
	// From is a grant's asking project, or the home a move leaves; absent for
	// a lock's request.
	From string `json:"from,omitempty"`
	// Access is write when the request asks for read and write (a grant the
	// write argument filed); absent otherwise.
	Access string `json:"access,omitempty"`
	// Status is pending, approved, denied or expired. A live lease that
	// covers the ask answers approved, with the request that opened it.
	Status string `json:"status"`
	// Filed is true when this call filed a new request, false when it found
	// one already waiting or live access already covers the ask (nothing
	// filed).
	Filed bool `json:"filed"`
	// Reason says what became of this call's reason: approval.ReasonAdded,
	// approval.ReasonKept or approval.ReasonNone.
	Reason string `json:"reason"`
	// OpenUntil is when the approved access ends (RFC 3339).
	OpenUntil string `json:"open_until,omitempty"`
	// Dialog is how the in-session approval dialog ended (accepted, declined,
	// cancelled, unoffered, unanswered, or -- accepted with Touch ID --
	// touchid_cancelled when the owner cancelled the Touch ID prompt and
	// touchid_closed when the helper closed without approving; in the advisory
	// tier, where accepting decides, changed when the request changed while
	// the dialog showed it and failed when the decision did not go through,
	// both leaving it pending); absent when none was shown.
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
		if s.advisoryTier() {
			return s.advisoryAccessRequest(ctx, start, project, reason, argBool(req, accessWriteParam, false))
		}
		return errResult(AccessRequestTool, errors.New("this server locks no project: nothing to request"))
	}
	if s.cfg.Grants != nil && argBool(req, accessWriteParam, false) {
		if res, ok, err := s.accessRequestAsk(ctx, start, project, reason); ok {
			return res, err
		}
	} else if s.cfg.Grants != nil && !lockCovers(ctx, project) {
		return s.accessRequestGrant(ctx, start, project, reason)
	}
	var asked approval.Asked
	a, err := s.fileAccessRequest(approval.WithAsked(ctx, &asked), project, reason, approval.ViaRequest)
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
	out.Filed, out.Reason = asked.Outcome(a)
	return s.accessRequestAnswer(ctx, start, t, ways, out)
}

// advisoryAccessRequest is access_request in the advisory tier: nothing is
// locked, so the ask is what the conversation's home and the project call
// for -- with write, approval.Grants.Ask (accessRequestAsk); without it, the
// read refused at the fence would ask for (accessRequestGrant) -- and the
// dialog, when the client can show one, is the advisory tier's, whose Approve
// decides it (advisory_dialog.go).
func (s *Server) advisoryAccessRequest(ctx context.Context, start time.Time, project, reason string, write bool) (*mcp.CallToolResult, error) {
	if c, ok := identifiedConversation(ctx); ok && c.home == project {
		return errResult(AccessRequestTool, askRequestError(approval.ErrIsHome, project))
	}
	if !write {
		return s.accessRequestGrant(ctx, start, project, reason)
	}
	res, ok, err := s.accessRequestAsk(ctx, start, project, reason)
	if !ok {
		// Only a lock's lease, which this tier never has, answers nothing here.
		return errResult(AccessRequestTool, askRequestError(approval.ErrIsHome, project))
	}
	return res, err
}

// lockCovers reports whether project is a lock's for the
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
//
// For a conversation identified by its agent process it asks for what the
// conversation's home and the project call for, as a read refused at the fence
// does (approval.Grants.RequestGrant): from an open home, or for a sealed
// project, a move into it, answered like accessRequestAsk's.
func (s *Server) accessRequestGrant(ctx context.Context, start time.Time, project, reason string) (*mcp.CallToolResult, error) {
	var asked approval.Asked
	a, err := s.cfg.Grants.RequestGrant(approval.WithAsked(ctx, &asked), approval.GrantInput{
		Project: project, SessionID: s.askingSession(ctx), AgentProcess: callerAgentProcess(ctx), Host: s.callerHost(ctx),
		Reason: reason, Via: approval.ViaRequest,
	})
	var live *approval.AccessLiveError
	if errors.As(err, &live) {
		return jsonResult(s.liveAccessResult(ctx, live))
	}
	if err != nil {
		if s.advisoryTier() && errors.Is(err, approval.ErrNotConfidential) {
			// Nothing is locked in this tier: a project with no fence is all
			// that is left to name.
			return errResult(AccessRequestTool, askRequestError(err, project))
		}
		return errResult(AccessRequestTool, grantRequestError(err))
	}
	return s.askAnswer(ctx, start, a, asked)
}

// askingSession is the session an explicit ask is filed for: an identified
// conversation's own ambient row (isolation.requests), never the call's
// binding; else the session the call is bound to, as before.
func (s *Server) askingSession(ctx context.Context) string {
	if c, ok := identifiedConversation(ctx); ok {
		return c.homeSession.ID
	}
	return s.boundSession(ctx)
}

// askAnswer answers an explicit ask with the grant or move request a it filed
// or found: the dialog when it can run (a move's is the passkey page, never
// Touch ID), else at once. A move still waiting tells the agent first to
// record what belongs in its home (moveFirstLine).
func (s *Server) askAnswer(ctx context.Context, start time.Time, a store.AccessApproval, asked approval.Asked) (*mcp.CallToolResult, error) {
	t := targetOf(a)
	ways := s.requestWays(ctx, a)
	out := accessRequestResult{
		ID: a.ID, ShortID: t.Short, Project: a.Project, Kind: a.Kind, From: a.FromProject, Status: s.stateOf(a), Page: ways.Page,
	}
	if t.Write {
		out.Access = store.GrantAccessWrite
	}
	out.Filed, out.Reason = asked.Outcome(a)
	out = s.answerOut(ctx, start, t, ways, out)
	if t.Move && out.Status == store.ApprovalPending {
		out.Next = moveFirstLine(a.FromProject) + s.receiptAfterMove(ctx, a.FromProject, a.Project) + " " + sentenceStart(out.Next)
	}
	return jsonResult(out)
}

// sentenceStart is s with its first letter upper-cased, for s to follow a
// sentence ("... no longer write there. The owner approves it ...").
func sentenceStart(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if !unicode.IsLower(r) {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// grantAsker is the read grants' ask of a kind (*approval.Grants.Ask):
// asserted, so the Grants interface the rest of the server holds keeps its
// shape.
type grantAsker interface {
	Ask(ctx context.Context, in approval.AskInput) (store.AccessApproval, error)
}

// accessRequestAsk files (or finds) what write asks for: the kind the
// conversation's home and the project call for (approval.Grants.Ask) -- a
// grant asking for read and write between confidential projects, or a move.
// ok is false, with nothing answered, for the project the asker already
// works in when a lock covers it: that is the lock's lease to ask for, which
// reads and writes already. A move's dialog is its passkey page, never Touch
// ID, which does not approve one (approval.ErrTouchIDMove); without a passkey
// it is answered at once, and the owner decides it in their own terminal.
func (s *Server) accessRequestAsk(ctx context.Context, start time.Time, project, reason string) (*mcp.CallToolResult, bool, error) {
	asker, ok := s.cfg.Grants.(grantAsker)
	if !ok {
		res, err := errResult(AccessRequestTool, errors.New("this server cannot ask for write access: nothing was filed"))
		return res, true, err
	}
	var asked approval.Asked
	a, err := asker.Ask(approval.WithAsked(ctx, &asked), approval.AskInput{GrantInput: approval.GrantInput{
		Project: project, SessionID: s.askingSession(ctx), AgentProcess: callerAgentProcess(ctx), Host: s.callerHost(ctx),
		Reason: reason, Via: approval.ViaRequest,
	}, Write: true})
	if errors.Is(err, approval.ErrIsHome) && lockCovers(ctx, project) {
		return nil, false, nil
	}
	var live *approval.AccessLiveError
	if errors.As(err, &live) {
		res, err := jsonResult(s.liveAccessResult(ctx, live))
		return res, true, err
	}
	if err != nil {
		res, err := errResult(AccessRequestTool, askRequestError(err, project))
		return res, true, err
	}
	res, err := s.askAnswer(ctx, start, a, asked)
	return res, true, err
}

// moveFirstLine is what a move request tells the agent before anything else:
// after the move it cannot write where it works now.
func moveFirstLine(home string) string {
	if home == "" {
		return "Once the owner approves the move, this conversation can no longer write to the global scope."
	}
	return "Record what belongs in " + home + " now: once the owner approves the move, this conversation can no longer write there."
}

// askRequestError words the refusals of an ask of a kind for an agent, as
// the console's access request route does for `seam access request --write`.
func askRequestError(err error, project string) error {
	switch {
	case errors.Is(err, approval.ErrIsHome):
		return errors.New("this conversation already works in " + project + ": nothing to request")
	case errors.Is(err, approval.ErrNoConversation):
		return errors.New("a move needs this agent's own session, the one its SessionStart hook recorded; this call names none")
	case errors.Is(err, approval.ErrNotConfidential):
		return errors.New("that project is neither confidential nor sealed: nothing to request")
	case errors.Is(err, approval.ErrSealed):
		return errors.New("a sealed project's session asks for no other project: nothing to request")
	}
	return grantRequestError(err)
}

// liveAccessResult is the answer to an ask live access already covers -- a
// lease, or a read grant from the same project: the request that opened it,
// approved, until it ends; nothing filed.
func (s *Server) liveAccessResult(ctx context.Context, live *approval.AccessLiveError) accessRequestResult {
	out := accessRequestResult{
		Project: live.Project, From: live.From, Status: stateApproved, Reason: approval.ReasonNone,
		OpenUntil: live.Expires.UTC().Format(time.RFC3339), Next: live.Answer() + "; keep working there until then",
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
	return jsonResult(s.answerOut(ctx, start, t, ways, out))
}

// answerOut is accessRequestAnswer's result before it is encoded.
func (s *Server) answerOut(ctx context.Context, start time.Time, t dialogTarget, ways access.Ways,
	out accessRequestResult) accessRequestResult {
	if end, ran := s.dialog(ctx, start, t); ran {
		out.Status, out.Dialog = end.state, end.answer
		if out.Dialog == answerNone {
			out.Dialog = "unanswered"
		}
		if end.state == stateApproved && !end.until.IsZero() {
			out.OpenUntil = end.until.UTC().Format(time.RFC3339)
		}
		out.Next = accessRequestNext(t, ways, end)
		return out
	}
	out.Next = accessRequestNext(t, ways, dialogEnd{state: out.Status, advisory: s.advisoryCall(ctx)})
	return out
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
	case answerChanged:
		lead = "the request changed while the approval dialog showed it, so nothing was approved and " + t.what() +
			" is still pending: "
	case answerFailed:
		lead = "the decision in the approval dialog did not go through, so " + t.what() + " is still pending: "
	}
	approves := ways.OwnerApproves(t.Short)
	if end.advisory {
		approves = advisoryApproves(t.Short)
	}
	return lead + approves + "; keep to other projects until it is approved, then retry the work that needs " + t.Project
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
