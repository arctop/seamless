package mcp

// The in-session approval dialog (plan step E, the daemon's half): when a call
// on the peer_auth socket files an access request -- refused at a lock
// (locked.go), refused at a confidential project's fence with a read grant
// request (grants.go), or asked for with access_request -- and the agent's
// client can show an MCP elicitation, the call shows the owner the request's
// facts in the client itself and waits, up to DialogWait from the start of
// the call, for the request to be decided.
//
// The dialog's answer grants nothing. Any process of the owner's account, or a
// client hook, can answer it, so all it ever does is start a proof: accepted
// with Touch ID, it asks the agent's `seam mcp-proxy` bridge to launch the
// Touch ID helper (core.ApproverNotification on this call's own reply); with a
// passkey, it sends the owner to the request's passkey page (a URL
// elicitation), with the duration the dialog's form chose (?for=) when it
// asked one. When the helper the bridge launched exits without an approval --
// the owner cancelled the Touch ID prompt, or it could not finish -- the
// bridge says so (core.ApproverEndedNotification) and the wait ends at once,
// the request still pending. Only the approve core (approval.Service.Approve,
// approval.Grants.Approve) decides, and the wait reads the request's row:
// approved, denied, expired, or still pending when the wait ends. A decline
// denies nothing -- an agent's connection may not deny -- the request stays
// pending for the owner.
//
// Nothing here runs unless the call came over the peer_auth socket, the
// client declared the elicitation capability, and at least one proof can
// finish in the dialog (an enrolled Touch ID key; a passkey with a URL-capable
// client). Otherwise the call answers at once, word for word as before.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

const (
	// defaultDialogWait bounds a dialog from the start of its tool call.
	defaultDialogWait = 2 * time.Minute
	// defaultDialogPoll is how often a waiting dialog reads its request.
	defaultDialogPoll = 500 * time.Millisecond
)

// How a dialog was answered.
const (
	answerNone      = ""          // no answer before the wait ended
	answerAccepted  = "accepted"  // accepted, with a duration and a proof method
	answerDeclined  = "declined"  // the dialog's own decline
	answerCancelled = "cancelled" // dismissed
	answerUnoffered = "unoffered" // accepted with something the dialog did not offer
	// Accepted with Touch ID, then the helper ended without an approval: the
	// owner cancelled its prompt, or it closed for another reason (it failed).
	answerTouchIDCancelled = "touchid_cancelled"
	answerTouchIDClosed    = "touchid_closed"
)

// Where a request stood when its dialog ended.
const (
	statePending  = store.ApprovalPending
	stateApproved = store.ApprovalApproved
	stateDenied   = store.ApprovalDenied
	stateExpired  = store.ApprovalExpired
)

func (s *Server) dialogWait() time.Duration {
	if s.cfg.DialogWait > 0 {
		return s.cfg.DialogWait
	}
	return defaultDialogWait
}

func (s *Server) dialogPoll() time.Duration {
	if s.cfg.DialogPoll > 0 {
		return s.cfg.DialogPoll
	}
	return defaultDialogPoll
}

// dialogTarget is the request a call filed, which its dialog may offer.
type dialogTarget struct {
	ID, Short, Project string
	// Grant marks a read grant request (isolation.requests), Project its
	// confidential target.
	Grant bool
}

// what names the request for the agent.
func (t dialogTarget) what() string {
	if t.Grant {
		return "read request " + t.Short
	}
	return "access request " + t.Short
}

// dialogSlotKey keys the per-call slot the requesters report into.
type dialogSlotKey struct{}

// dialogSlot carries the first request a call filed out to dialogMiddleware.
type dialogSlot struct {
	mu     sync.Mutex
	target dialogTarget
}

// noteDialogTarget records t as the call's request, unless one is recorded
// already. Outside dialogMiddleware (no slot) it does nothing.
func noteDialogTarget(ctx context.Context, t dialogTarget) {
	slot, ok := ctx.Value(dialogSlotKey{}).(*dialogSlot)
	if !ok || t.ID == "" {
		return
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.target.ID == "" {
		slot.target = t
	}
}

func (d *dialogSlot) get() (dialogTarget, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.target, d.target.ID != ""
}

// dialogMiddleware offers the approval dialog for a call refused with the
// request it filed. It plants the slot the lock requester and the read-grant
// refusal report into, and after the call, when the result is that refusal
// (an error naming the request), runs the dialog and rewrites the refusal's
// remedy with how it ended. With no approval service, or off the peer_auth
// socket, it is a pass-through.
func (s *Server) dialogMiddleware(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if s.cfg.Approvals == nil || !access.PeerAuthenticated(ctx) {
			return next(ctx, req)
		}
		start := time.Now()
		slot := &dialogSlot{}
		res, err := next(context.WithValue(ctx, dialogSlotKey{}, slot), req)
		if err != nil || res == nil || !res.IsError || len(res.Content) == 0 {
			return res, err
		}
		t, ok := slot.get()
		if !ok {
			return res, err
		}
		first, ok := res.Content[0].(mcp.TextContent)
		if !ok || !strings.Contains(first.Text, t.Short) {
			// Refused for some other reason than the request it filed.
			return res, err
		}
		end, ran := s.dialog(ctx, start, t)
		if !ran {
			return res, err
		}
		out := *res
		out.Content = slices.Clone(res.Content)
		out.Content[0] = mcp.NewTextContent(refusalAfterDialog(req.Params.Name, first.Text, t, end,
			callWays(ctx, s.cfg.Approvals.Ways(ctx, t.ID)), s.cfg.OfferAccessRequest))
		return &out, err
	}
}

// dialogEnd is how a dialog ended.
type dialogEnd struct {
	answer string
	// state is where the request stood at the end; request is its row.
	state   string
	request store.AccessApproval
	// until is when the access the approval opened ends (approved only;
	// zero when it could not be read).
	until time.Time
	// stopAt is when the dialog stopped waiting, and timedOut whether it
	// stopped there rather than at a decision.
	stopAt   time.Time
	timedOut bool
}

// dialog runs the approval dialog for request t, filed by the call in ctx that
// started at start. It reports false -- show nothing, answer as before --
// unless every precondition holds: the call came over the peer_auth socket,
// the client declared elicitation, a proof can finish in the dialog, the
// request is pending, and this connection has no dialog open for it already.
func (s *Server) dialog(ctx context.Context, start time.Time, t dialogTarget) (dialogEnd, bool) {
	if s.cfg.Approvals == nil || t.ID == "" || !access.PeerAuthenticated(ctx) {
		return dialogEnd{}, false
	}
	session := mcpserver.ClientSessionFromContext(ctx)
	if session == nil || session.SessionID() == "" {
		return dialogEnd{}, false
	}
	if _, ok := session.(mcpserver.SessionWithElicitation); !ok {
		return dialogEnd{}, false
	}
	form, urlMode := clientElicitation(session)
	methods, page := s.dialogMethods(ctx, t.ID, form, urlMode)
	if len(methods) == 0 {
		return dialogEnd{}, false
	}
	a, err := store.AccessApprovalByID(ctx, s.cfg.DB, t.ID)
	if err != nil || s.stateOf(a) != statePending {
		return dialogEnd{}, false
	}
	key := session.SessionID() + "\x00" + t.ID
	if !s.claimDialog(key) {
		return dialogEnd{}, false
	}
	defer s.releaseDialog(key)

	stopAt := start.Add(s.dialogWait())
	dctx, cancel := context.WithDeadline(ctx, stopAt)
	defer cancel()
	end := dialogEnd{stopAt: stopAt}

	if !form || !slices.Contains(methods, approval.ProofTouchID) {
		// A passkey only: its page shows the facts and asks the duration, so
		// the dialog is the page itself.
		elicitID, accepted, ok := s.passkeyDialog(dctx, session, t, page, "")
		if !ok {
			return dialogEnd{}, false
		}
		if !accepted.answered() {
			end.answer = accepted.answer()
			return s.settle(ctx, dctx, t, end), true
		}
		end.answer = answerAccepted
		s.awaitDecision(dctx, t.ID)
		return s.completeURL(ctx, session, elicitID, s.settle(ctx, dctx, t, end)), true
	}

	facts, err := s.cfg.Approvals.Facts(ctx, t.ID)
	if err != nil {
		s.logger.Debug("access dialog: facts", "request", t.Short, "error", err)
		return dialogEnd{}, false
	}
	capped, err := s.cfg.Approvals.LeaseLength(approval.ForSession)
	if err != nil {
		s.logger.Debug("access dialog: lease cap", "request", t.Short, "error", err)
		return dialogEnd{}, false
	}
	shown := dialogShown{stopAt: stopAt, capped: capped, ways: s.cfg.Approvals.Ways(ctx, t.ID)}
	res, err := s.mcp.RequestElicitation(dctx, formElicitation(facts, methods, shown))
	if err != nil {
		if dctx.Err() == nil {
			// The client did not take it: nothing was shown.
			s.logger.Debug("access dialog: not shown", "request", t.Short, "error", err)
			return dialogEnd{}, false
		}
		return s.settle(ctx, dctx, t, end), true
	}
	switch res.Action {
	case mcp.ElicitationResponseActionAccept:
	case mcp.ElicitationResponseActionDecline:
		end.answer = answerDeclined
		return s.settle(ctx, dctx, t, end), true
	case mcp.ElicitationResponseActionCancel:
		end.answer = answerCancelled
		return s.settle(ctx, dctx, t, end), true
	default:
		end.answer = answerUnoffered
		return s.settle(ctx, dctx, t, end), true
	}
	duration, method, ok := dialogChoice(res.Content, methods)
	if !ok {
		end.answer = answerUnoffered
		return s.settle(ctx, dctx, t, end), true
	}
	end.answer = answerAccepted
	s.logger.Debug("access dialog: accepted", "request", t.Short, "method", method, "duration", duration)
	if method == approval.ProofPasskey {
		// The page for the duration chosen here: it shows it read-only, and
		// the passkey signs it.
		elicitID, accepted, ok := s.passkeyDialog(dctx, session, t, pageFor(page, duration), duration)
		if !ok || !accepted.answered() {
			if ok {
				end.answer = accepted.answer()
			}
			return s.settle(ctx, dctx, t, end), true
		}
		s.awaitDecision(dctx, t.ID)
		return s.completeURL(ctx, session, elicitID, s.settle(ctx, dctx, t, end)), true
	}
	// Touch ID: the bridge launches the helper, which reads the facts from
	// the daemon, shows them in the prompt, and signs over the duration. When
	// it exits without approving, the bridge says so and the wait ends. The
	// handler is registered once, when first needed, so a daemon that never
	// shows a Touch ID dialog never has one.
	ended, unwatch := s.watchApproverEnd(key)
	defer unwatch()
	s.approverOnce.Do(func() { s.mcp.AddNotificationHandler(core.ApproverEndedNotification, s.handleApproverEnded) })
	if err := s.sendApproverLaunch(dctx, t.ID, duration); err != nil {
		// Not queued, so there is nothing to lose by returning: wait as for
		// any other proof (the owner may still approve elsewhere).
		s.logger.Debug("access dialog: approver launch not sent", "request", t.Short, "error", err)
		s.awaitDecision(dctx, t.ID)
		return s.settle(ctx, dctx, t, end), true
	}
	// The ask is queued, not yet written: the call stays open long enough for
	// it to reach the reply stream (holdOpen), past the bound if it must.
	wctx, stop := s.holdOpen(ctx, dctx)
	defer stop()
	if dl, ok := wctx.Deadline(); ok {
		end.stopAt = dl
	}
	outcome := s.awaitDecisionOrEnd(wctx, t.ID, ended)
	end = s.settle(ctx, wctx, t, end)
	if outcome != "" && end.state == statePending {
		end.answer = answerTouchIDClosed
		if outcome == core.ApproverEndedCancelled {
			end.answer = answerTouchIDCancelled
		}
	}
	return end, true
}

// watchApproverEnd opens the slot the dialog keyed key (its MCP session and
// request) waits on for its helper's end (Server.approverEnds), and returns it
// with the func that closes it.
func (s *Server) watchApproverEnd(key string) (<-chan string, func()) {
	ch := make(chan string, 1)
	s.approverMu.Lock()
	s.approverEnds[key] = ch
	s.approverMu.Unlock()
	return ch, func() {
		s.approverMu.Lock()
		defer s.approverMu.Unlock()
		if s.approverEnds[key] == ch {
			delete(s.approverEnds, key)
		}
	}
}

// handleApproverEnded takes the bridge's word that the helper it launched for
// a request has exited, and ends the wait of that request's dialog on this
// connection. It is taken only on the peer_auth socket, from the asking
// agent's own process tree (the request's process is a hop of this
// connection's kernel chain), with an outcome it knows; anything else is
// dropped. A notification has no reply, so nothing is answered either way.
func (s *Server) handleApproverEnded(ctx context.Context, n mcp.JSONRPCNotification) {
	if !access.PeerAuthenticated(ctx) {
		return
	}
	session := mcpserver.ClientSessionFromContext(ctx)
	if session == nil || session.SessionID() == "" {
		return
	}
	id, _ := n.Params.AdditionalFields["request_id"].(string)
	outcome, _ := n.Params.AdditionalFields["outcome"].(string)
	if id == "" || (outcome != core.ApproverEndedCancelled && outcome != core.ApproverEndedClosed) {
		return
	}
	s.approverMu.Lock()
	ch, waiting := s.approverEnds[session.SessionID()+"\x00"+id]
	s.approverMu.Unlock()
	if !waiting {
		return
	}
	a, err := store.AccessApprovalByID(ctx, s.cfg.DB, id)
	if err != nil {
		return
	}
	v, _ := access.FromContext(ctx)
	if a.ProcessKey == "" || !slices.Contains(chainKeys(v), a.ProcessKey) {
		s.logger.Debug("access dialog: approver end from another tree", "request", approval.ShortID(id))
		return
	}
	select {
	case ch <- outcome:
	default: // one end is enough
	}
}

// notifyHold is how long a call stays open after queueing a notification on
// its own reply: a second, or a quarter of the dialog's bound when that is
// shorter (tests). mcp-go v1.1.1 drops a notification queued just before the
// handler returns (its forwarder takes it off the session's channel, then
// loses the reply's lock to the response path, which closes the stream), so
// queueing one and returning at once would lose it most of the time. Any
// wait at all lets the forwarder write it; this one is generous.
func (s *Server) notifyHold() time.Duration {
	return min(time.Second, s.dialogWait()/4)
}

// holdOpen is the context a dialog waits on after queueing a notification:
// dctx when its bound leaves at least notifyHold, else one derived from the
// call's ctx that ends notifyHold from now. It never outlives the call.
func (s *Server) holdOpen(ctx, dctx context.Context) (context.Context, context.CancelFunc) {
	floor := time.Now().Add(s.notifyHold())
	if dl, ok := dctx.Deadline(); ok && !dl.Before(floor) {
		return dctx, func() {}
	}
	return context.WithDeadline(ctx, floor)
}

// urlAnswer is how a URL elicitation was answered.
type urlAnswer string

func (u urlAnswer) answered() bool { return u == answerAccepted }
func (u urlAnswer) answer() string { return string(u) }

// passkeyDialog sends the owner to the request's passkey page: a URL
// elicitation. duration is the one the dialog's form chose, which page
// carries (pageFor), "" when the page asks it. ok is false when the client
// did not take it (nothing shown).
func (s *Server) passkeyDialog(ctx context.Context, session mcpserver.ClientSession, t dialogTarget, page, duration string) (string, urlAnswer, bool) {
	elicitID, err := core.NewID()
	if err != nil {
		return "", "", false
	}
	// Only the approval takes the passkey: the page denies without one.
	approve, decide := "approve "+plain(t.Project), "Approve it"
	if phrase, ok := durationPhrases[duration]; ok {
		approve, decide = approve+" "+phrase, decide+" "+phrase
	}
	msg := "Seamless access request " + t.Short + ": " + approve + " with your passkey, or deny it, on the page this " +
		"opens. Opening it grants nothing by itself."
	if f, err := s.cfg.Approvals.Facts(ctx, t.ID); err == nil {
		// What is asked first, as in the form (dialogLead); the page shows
		// every fact, and asks how long unless the form did.
		msg = plain(askerName(f)) + " asks for " + plain(accessPhrase(f)) + "; " + warningCount(len(f.Warnings)) +
			". " + decide + " with your passkey, or deny it, on the page this opens, which shows the request (" +
			plain(f.Short) + ") in full. Opening it grants nothing by itself."
	}
	res, err := s.mcp.RequestURLElicitation(ctx, session, elicitID, page, msg)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Debug("access dialog: passkey page not shown", "request", t.Short, "error", err)
			return "", "", false
		}
		return elicitID, answerNone, true
	}
	switch res.Action {
	case mcp.ElicitationResponseActionAccept:
		return elicitID, answerAccepted, true
	case mcp.ElicitationResponseActionDecline:
		return elicitID, answerDeclined, true
	case mcp.ElicitationResponseActionCancel:
		return elicitID, answerCancelled, true
	default:
		return elicitID, answerUnoffered, true
	}
}

// pageFor is the passkey page with the duration the dialog's form chose
// (?for=): the page shows it read-only, and its options sign it. "" leaves
// the page as it is, and the page asks.
func pageFor(page, duration string) string {
	if duration == "" {
		return page
	}
	u, err := url.Parse(page)
	if err != nil {
		return page
	}
	q := u.Query()
	q.Set("for", duration)
	u.RawQuery = q.Encode()
	return u.String()
}

// durationPhrases word a duration choice inside a sentence.
var durationPhrases = map[string]string{
	approval.ForSession: "until its session ends",
	approval.For1h:      "for 1 hour",
	approval.For4h:      "for 4 hours",
}

// completeURL tells the client a passkey page's interaction is over once the
// request was decided.
func (s *Server) completeURL(ctx context.Context, session mcpserver.ClientSession, elicitID string, end dialogEnd) dialogEnd {
	if elicitID == "" || end.state == statePending || ctx.Err() != nil {
		return end
	}
	if err := s.mcp.SendElicitationComplete(ctx, session, elicitID); err != nil {
		s.logger.Debug("access dialog: elicitation complete not sent", "error", err)
		return end
	}
	// Queued, not yet written: hold the call open so it is not dropped
	// (notifyHold).
	hold := time.NewTimer(s.notifyHold())
	defer hold.Stop()
	select {
	case <-hold.C:
	case <-ctx.Done():
	}
	return end
}

// sendApproverLaunch asks the agent's bridge to launch the Touch ID helper for
// request id, signing over duration (core.ApproverLaunch). It rides this
// call's reply stream; the bridge swallows it.
func (s *Server) sendApproverLaunch(ctx context.Context, id, duration string) error {
	b, err := json.Marshal(core.ApproverLaunch{RequestID: id, Duration: duration})
	if err != nil {
		return err
	}
	var params map[string]any
	if err := json.Unmarshal(b, &params); err != nil {
		return err
	}
	return s.mcp.SendNotificationToClient(ctx, core.ApproverNotification, params)
}

// awaitDecision waits until request id leaves pending, or ctx ends. It polls
// the request's row: every decision writes it, whoever made it.
func (s *Server) awaitDecision(ctx context.Context, id string) {
	s.awaitDecisionOrEnd(ctx, id, nil)
}

// awaitDecisionOrEnd is awaitDecision that also ends when ended delivers how
// the Touch ID helper ended, and returns that outcome ("" when the wait ended
// otherwise). A nil ended is never ready.
func (s *Server) awaitDecisionOrEnd(ctx context.Context, id string, ended <-chan string) string {
	tick := time.NewTicker(s.dialogPoll())
	defer tick.Stop()
	for {
		a, err := store.AccessApprovalByID(ctx, s.cfg.DB, id)
		if err == nil && s.stateOf(a) != statePending {
			return ""
		}
		select {
		case <-ctx.Done():
			return ""
		case outcome := <-ended:
			return outcome
		case <-tick.C:
		}
	}
}

// settle reads where the request stands at the end of its dialog, and when
// the access it opened ends. dctx is the dialog's own bounded context: it
// stopped at the bound when it hit its deadline while the call's did not end.
func (s *Server) settle(ctx, dctx context.Context, t dialogTarget, end dialogEnd) dialogEnd {
	end.timedOut = errors.Is(dctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	rctx := context.WithoutCancel(ctx)
	a, err := store.AccessApprovalByID(rctx, s.cfg.DB, t.ID)
	if err != nil {
		end.state = statePending
		return end
	}
	end.request, end.state = a, s.stateOf(a)
	if end.state == stateApproved {
		end.until = s.accessEnd(rctx, a)
	}
	return end
}

// stateOf is where a request stands now: its status, or expired once its
// deadline passed undecided.
func (s *Server) stateOf(a store.AccessApproval) string {
	if a.Status == store.ApprovalPending && !a.ExpiresAt.After(s.now()) {
		return stateExpired
	}
	return a.Status
}

// accessEnd is when the lease or read grant an approved request opened ends;
// zero when it is not live (it may already have been revoked).
func (s *Server) accessEnd(ctx context.Context, a store.AccessApproval) time.Time {
	var keys []string
	if a.ProcessKey != "" {
		keys = []string{a.ProcessKey}
	}
	if a.Kind == store.AccessKindGrant {
		sessionID := ""
		if len(keys) == 0 {
			sessionID = a.SessionID
		}
		rows, err := store.AccessGrantsForHolder(ctx, s.cfg.DB, keys, sessionID)
		if err != nil {
			return time.Time{}
		}
		for _, g := range rows {
			if g.ApprovalID == a.ID && g.RevokedAt == nil && g.ExpiresAt.After(s.now()) {
				return g.ExpiresAt
			}
		}
		return time.Time{}
	}
	rows, err := store.ActiveAccessLeasesFor(ctx, s.cfg.DB, keys, s.now())
	if err != nil {
		return time.Time{}
	}
	for _, l := range rows {
		if l.ApprovalID == a.ID {
			return l.ExpiresAt
		}
	}
	return time.Time{}
}

// claimDialog marks a dialog open for key, reporting false when one already
// is.
func (s *Server) claimDialog(key string) bool {
	s.dialogMu.Lock()
	defer s.dialogMu.Unlock()
	if _, open := s.dialogs[key]; open {
		return false
	}
	s.dialogs[key] = struct{}{}
	return true
}

func (s *Server) releaseDialog(key string) {
	s.dialogMu.Lock()
	defer s.dialogMu.Unlock()
	delete(s.dialogs, key)
}

// clientElicitation reads the elicitation modes a client session declared at
// initialize. mcp-go never checks them for us. A bare `elicitation: {}` (the
// 2025-06-18 shape) is form mode.
func clientElicitation(session mcpserver.ClientSession) (form, urlMode bool) {
	ci, ok := session.(mcpserver.SessionWithClientInfo)
	if !ok {
		return false, false
	}
	e := ci.GetClientCapabilities().Elicitation
	if e == nil {
		return false, false
	}
	urlMode = e.URL != nil
	return e.Form != nil || !urlMode, urlMode
}

// dialogMethods are the proofs a dialog may offer for request id, in the
// order it offers them, and the passkey page when a passkey is one: Touch ID
// with an enrolled key and a form-capable client; a passkey with passkeys on,
// an enrolled one, a URL-capable client and an http(s) page.
func (s *Server) dialogMethods(ctx context.Context, id string, form, urlMode bool) ([]string, string) {
	var methods []string
	if form {
		if keys, err := store.ApproverCredentials(ctx, s.cfg.DB, store.ApproverTouchID); err == nil && len(keys) > 0 {
			methods = append(methods, approval.ProofTouchID)
		}
	}
	page := ""
	if urlMode && s.cfg.Approvals.PasskeysOn() {
		if p := s.cfg.Approvals.Page(id); webPage(p) {
			if keys, err := store.ApproverCredentials(ctx, s.cfg.DB, store.ApproverPasskey); err == nil && len(keys) > 0 {
				methods, page = append(methods, approval.ProofPasskey), p
			}
		}
	}
	return methods, page
}

// callWays are the approval paths a remedy in this call may offer the owner
// for request id: the daemon's (ways, from Approvals.Ways or Grants.Ways),
// with Touch ID only when this connection's client can show the approval
// dialog that carries it -- a form-capable client on the peer_auth socket.
// Elsewhere Touch ID is no way in for this call, and the text says nothing
// of it.
func callWays(ctx context.Context, ways access.Ways) access.Ways {
	if ways.TouchID && !formDialogCapable(ctx) {
		ways.TouchID = false
	}
	return ways
}

// formDialogCapable reports whether the call in ctx could show the approval
// dialog's form: it came over the peer_auth socket from a client that
// declared form elicitation.
func formDialogCapable(ctx context.Context) bool {
	if !access.PeerAuthenticated(ctx) {
		return false
	}
	session := mcpserver.ClientSessionFromContext(ctx)
	if session == nil {
		return false
	}
	if _, ok := session.(mcpserver.SessionWithElicitation); !ok {
		return false
	}
	form, _ := clientElicitation(session)
	return form
}

// webPage reports whether p is an absolute http(s) URL.
func webPage(p string) bool {
	u, err := url.Parse(p)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

// The words the form shows for each choice.
var (
	durationTitles = map[string]string{
		approval.ForSession: "Until this session ends",
		approval.For1h:      "1 hour",
		approval.For4h:      "4 hours",
	}
	methodTitles = map[string]string{
		approval.ProofTouchID: "Touch ID",
		approval.ProofPasskey: "Passkey (opens the approval page for the duration chosen here)",
	}
)

func titles(values []string, words map[string]string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = v
		if w, ok := words[v]; ok {
			out[i] = w
		}
	}
	return out
}

// dialogShown is what a dialog says beside the request's facts: when it
// stops waiting, how long "until the session ends" can last (the lease cap),
// and the approval paths the daemon offers besides the dialog.
type dialogShown struct {
	stopAt time.Time
	capped time.Duration
	ways   access.Ways
}

// formElicitation is the dialog: the daemon's facts about the request, the
// warnings first, the agent's reason quoted and labeled as its own; and two
// fields, how long and with which proof.
func formElicitation(f approval.Facts, methods []string, shown dialogShown) mcp.ElicitationRequest {
	return mcp.ElicitationRequest{
		Request: mcp.Request{Method: string(mcp.MethodElicitationCreate)},
		Params: mcp.ElicitationParams{
			Message: dialogMessage(f, shown),
			RequestedSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"duration": map[string]any{
						"type": "string", "title": "Approve for",
						"description": "how long the access lasts, capped by the daemon's maximum",
						"enum":        approval.Durations, "enumNames": titles(approval.Durations, durationTitles),
						"default": approval.ForSession,
					},
					"method": map[string]any{
						"type": "string", "title": "Prove it with",
						"description": "accepting here grants nothing: the approval needs this proof",
						"enum":        methods, "enumNames": titles(methods, methodTitles),
						"default": methods[0],
					},
				},
				"required": []string{"duration", "method"},
			},
		},
	}
}

// dialogChoice reads the accepted form: a duration the service offers and one
// of the offered methods (the only one, when the form came back without it).
func dialogChoice(content any, methods []string) (duration, method string, ok bool) {
	fields, _ := content.(map[string]any)
	duration, _ = fields["duration"].(string)
	method, _ = fields["method"].(string)
	if duration == "" {
		duration = approval.ForSession
	}
	if method == "" && len(methods) == 1 {
		method = methods[0]
	}
	return duration, method, slices.Contains(approval.Durations, duration) && slices.Contains(methods, method)
}

// dialogMessage renders the request's facts for the dialog. Its first line
// is one sentence that says what is asked -- who, which access to which
// project, for how long, and how many warnings -- because Claude Code
// collapses a dialog to its first line. The warnings follow, before every
// other fact. Every value is stripped of control and format characters (bidi
// overrides and isolates among them), so nothing can reorder or hide what the
// owner reads.
func dialogMessage(f approval.Facts, shown dialogShown) string {
	var b strings.Builder
	b.WriteString(dialogLead(f, shown.capped) + "\n")
	if len(f.Warnings) > 0 {
		b.WriteString("\nWarnings:\n")
		for _, w := range f.Warnings {
			b.WriteString("- " + plain(w.Text) + "\n")
		}
	}
	b.WriteString("\n")
	line := func(label, value string) {
		if value = plain(value); value != "" {
			b.WriteString(label + ": " + value + "\n")
		}
	}
	if f.Kind == store.AccessKindGrant {
		line("Project", f.Project+" ("+f.Isolation+"), read from project "+f.From)
	} else {
		line("Project", f.Project+" ("+f.Isolation+", locked)")
	}
	line("Access", f.Access)
	line("Session", f.Session)
	line("Repository", f.Repo)
	line("Working directory", f.CWD)
	line("Model", f.Model)
	line("Agent process", agentProcessName(f))
	if strings.ContainsAny(f.Program, `/\`) {
		// The full path, so a program that only borrows an agent's name
		// shows where it really runs from.
		line("Program", f.Program)
	}
	line("Request", f.Short+", "+requestKindWords(f.Kind))
	line("This dialog stops waiting at", access.Clock(shown.stopAt))
	line("The request expires at", access.Clock(f.ExpiresAt))
	if reason := plain(f.Reason); reason != "" {
		b.WriteString("\nThe agent's reason, in its own words (Seamless did not check it):\n\"" + reason + "\"\n")
	}
	pending := "`seam approve " + plain(f.Short) + "` in your own terminal"
	if shown.ways.Page != "" {
		pending += ", or a passkey at " + plain(shown.ways.Page)
	}
	b.WriteString("\nAccepting here grants nothing by itself: the approval needs the proof you choose below. " +
		"Declining leaves the request pending for " + pending + ".")
	return b.String()
}

// dialogLead is the dialog's first line: "Claude Code 2.1.319 asks for
// read-write access to demo-vault (confidential, locked) until its session
// ends (8 hours at most) unless you pick less below; 2 warnings." The
// duration is the form's default, "until the session ends", which the lease
// cap bounds; a request no session names has nothing to end it sooner.
func dialogLead(f approval.Facts, capped time.Duration) string {
	asks := "asks for"
	if f.Kind == store.AccessKindRefresh {
		asks = "asks again for"
	}
	long := "until its session ends (" + spanWords(capped) + " at most)"
	if f.SessionID == "" {
		long = "for " + spanWords(capped)
	}
	return plain(askerName(f)) + " " + asks + " " + plain(accessPhrase(f)) + " " + long + " unless you pick less below; " +
		warningCount(len(f.Warnings)) + "."
}

// accessPhrase is the access a request asks for, and where: "read-write
// access to demo-vault (confidential, locked)".
func accessPhrase(f approval.Facts) string {
	if f.Kind == store.AccessKindGrant {
		return f.Access + " access to " + f.Project + " (" + f.Isolation + ") from project " + f.From
	}
	return f.Access + " access to " + f.Project + " (" + f.Isolation + ", locked)"
}

// warningCount counts the warnings for the lead: "no warnings", "1
// warning", "2 warnings".
func warningCount(n int) string {
	switch {
	case n == 1:
		return "1 warning"
	case n > 1:
		return fmt.Sprintf("%d warnings", n)
	}
	return "no warnings"
}

// askerName names the asking process for a person: its agent's name, else
// its program's base name, else its pid; with none of them, its session.
func askerName(f approval.Facts) string {
	if n := agentProcessName(f); n != "" {
		return n
	}
	if f.Session != "" {
		return "Session " + f.Session
	}
	return "An agent"
}

// agentProcessName is the asking process as the facts name it: the agent's
// name (Facts.Agent), else the program's base name, and the pid only when no
// name is known.
func agentProcessName(f approval.Facts) string {
	switch {
	case f.Agent != "":
		return f.Agent
	case plain(f.Program) != "":
		p := plain(f.Program)
		if i := strings.LastIndexAny(p, `/\`); i >= 0 && i < len(p)-1 {
			p = p[i+1:]
		}
		return p
	case f.PID > 0:
		return fmt.Sprintf("pid %d", f.PID)
	}
	return ""
}

// requestKindWords says what kind of request it is, for the Request line.
func requestKindWords(kind string) string {
	switch kind {
	case store.AccessKindRefresh:
		return "a refresh of the lease this agent holds"
	case store.AccessKindGrant:
		return "a read request"
	}
	return "a new request"
}

// spanWords renders a lease length for a person: "8 hours", "1 hour", "90
// minutes".
func spanWords(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		if h := int(d / time.Hour); h != 1 {
			return fmt.Sprintf("%d hours", h)
		}
		return "1 hour"
	case d >= time.Minute:
		if m := int(d / time.Minute); m != 1 {
			return fmt.Sprintf("%d minutes", m)
		}
		return "1 minute"
	}
	return d.String()
}

// plain strips control and format characters from a value shown to the owner:
// line breaks and tabs become spaces; bidi embeddings, overrides and isolates,
// zero-width characters and the other format characters are dropped.
func plain(v string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || r == '\u2028' || r == '\u2029':
			return ' '
		case unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, v))
}

// refusalAfterDialog is a refused call's text once its dialog ended: the
// rule, then the remedy as the dialog left it. text is the refusal as the
// call made it (tool-prefixed, its rule and its remedy on two lines); ways
// are the approval paths still open to the owner while it is pending, and
// askTool reports that this daemon offers access_request (the way to ask
// again after a denial).
//
// A request still pending gets one remedy, not two: how the dialog ended,
// the paths left to the owner, and the original remedy's closing words (from
// "retry once it is approved" on, which for a read request say what an
// approval brings).
func refusalAfterDialog(tool, text string, t dialogTarget, end dialogEnd, ways access.Ways, askTool bool) string {
	rule, remedy, _ := strings.Cut(text, "\nRemedy: ")
	what := t.what()
	switch end.state {
	case stateApproved:
		prefix := ""
		if strings.HasPrefix(text, tool+": ") {
			prefix = tool + ": "
		}
		return prefix + "the owner approved " + what + ": " + openLine(t, end) +
			".\nRemedy: this call ran before the approval and did nothing -- retry it now."
	case stateDenied:
		return rule + "\nRemedy: the owner denied " + what + "; keep to other projects, and " +
			access.AskAgain(t.Project, askTool) + "."
	case stateExpired:
		return rule + "\nRemedy: " + what + " expired before the owner decided; the next call that needs " +
			t.Project + " files a new one."
	}
	var ended string
	switch end.answer {
	case answerDeclined, answerCancelled:
		ended = "the approval dialog was " + end.answer
	case answerTouchIDCancelled:
		ended = "Touch ID was cancelled in the approval dialog, so nothing was approved"
	case answerTouchIDClosed:
		ended = "the Touch ID approver closed without approving"
	case answerUnoffered:
		ended = "the approval dialog's answer was not one it offered"
	default:
		ended = "the approval dialog stopped waiting at " + access.Clock(end.stopAt) + " without a decision"
	}
	closing := "retry once it is approved, and keep to other projects until then."
	if i := strings.Index(remedy, "retry once it is approved"); i >= 0 {
		closing = remedy[i:]
	}
	return rule + "\nRemedy: " + ended + "; " + what + " stays pending for the owner: " + ways.StillPending(t.Short) +
		"; " + closing
}

// openLine says what an approval opened, and until when.
func openLine(t dialogTarget, end dialogEnd) string {
	open := "project " + t.Project + " is open to this agent"
	if t.Grant {
		open = "project " + t.Project + " is readable from project " + end.request.FromProject + ", nothing else"
	}
	if !end.until.IsZero() {
		open += " until " + access.Clock(end.until)
	}
	return open
}
