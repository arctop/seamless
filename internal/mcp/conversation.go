package mcp

// The MCP edge speaks for the conversation (isolation.requests, plan
// fenced-sessions P4; owner decisions in the fenced-sessions-design memory,
// D1-D4). Off (Config.Grants nil), nothing here runs and every call is judged
// against its binding, word for word as before.
//
// On, a call that identifies its conversation -- the agent-process identity
// names the agent's own ambient row, and the store found the conversation's
// home there (store.ConversationClearance, access.Grants.HomeKnown) -- speaks
// from that home:
//
//   - the fences judge it against the home (callerScope), whatever it is
//     bound to;
//   - a session= handle or an earlier session_start binding acts as a session
//     outside the home only where the home may read and write (speakAs);
//     otherwise the call is refused, with the rule and the remedy;
//   - session_start into a fenced project files the request the home and the
//     project call for instead of binding (sessionStartReach): a move from an
//     open home (or the global scope) into a confidential project and from any
//     home into a sealed one, a grant between confidential projects; into an
//     open project or the global scope from a conversation that holds fenced
//     content it is refused, since there is no way back;
//   - a refusal at a fenced project files the same kind of request
//     (askRefusal): a refused read asks for the read (a move from an open
//     home), a refused write for read and write;
//   - the first result after an approved move announces it and carries the new
//     home's briefing, once (moveNoticeMiddleware).
//
// A client that sends no agent-process identity is not identified: it is held
// by the session it is bound to, and binds as before (the documented limit).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
)

// conversation is the conversation one call speaks for, judged once per call
// by grantsMiddleware (loadGrants) and carried in its context.
type conversation struct {
	// ref is the holder the clearance was read for (conversationRef).
	ref store.ConversationRef
	// identified reports that the agent-process identity named the
	// conversation's own ambient row (homeSession) and the store found its
	// home there, in a project this connection reaches.
	identified bool
	// home is the conversation's home ("" for the global scope) and
	// homeSession its ambient row, when identified.
	home        string
	homeSession core.Session
}

type conversationKey struct{}

func withConversation(ctx context.Context, c conversation) context.Context {
	return context.WithValue(ctx, conversationKey{}, c)
}

// conversationOf returns the conversation the call in ctx speaks for, and
// whether grantsMiddleware judged one (isolation.requests on).
func conversationOf(ctx context.Context) (conversation, bool) {
	c, ok := ctx.Value(conversationKey{}).(conversation)
	return c, ok
}

// identifiedConversation is conversationOf for an identified conversation
// only.
func identifiedConversation(ctx context.Context) (conversation, bool) {
	c, ok := conversationOf(ctx)
	return c, ok && c.identified
}

// identify settles what conversationRef found into the call's conversation.
// It is identified only when the agent-process identity found the
// conversation's own ambient row (fromProcess), the store found the home on
// that same row (g.HomeKnown) and this connection reaches it: a home in a
// project locked to it is the lock's to open (lockedBinding).
func identify(ctx context.Context, g access.Grants, ref store.ConversationRef, own core.Session, fromProcess bool) conversation {
	c := conversation{ref: ref}
	if !fromProcess || !g.HomeKnown() || own.ProjectSlug != g.Home() || !access.Visible(ctx, g.Home()) {
		return c
	}
	c.identified, c.home, c.homeSession = true, g.Home(), own
	return c
}

// bindingSource is which of getBinding's routes produced a binding.
type bindingSource int

const (
	boundByHandle   bindingSource = iota + 1 // the call's session= handle
	boundExplicitly                          // an earlier session_start on this connection
	boundByProcess                           // the agent's own session (agent_binding.go)
)

// speakAs refuses a call whose session= handle, or whose connection's
// session_start binding, would have the identified conversation c act as a
// session outside its home where the home may not both read and write. The
// fences judge every call against the home (callerScope), so the binding only
// chooses the session it is credited to and the project its unscoped calls
// land in; that is harmless exactly where the home may read and write -- an
// open project while the conversation holds nothing fenced, or a project the
// owner granted it read and write -- and nowhere else. A binding to a project
// this connection may not reach at all is the View's to judge (getBinding
// drops it, lockedBinding refuses it at the lock).
func (s *Server) speakAs(ctx context.Context, c conversation) error {
	b, how, ok := s.bindingFrom(ctx)
	if !ok || how == boundByProcess || b.project == c.home || !access.Visible(ctx, b.project) {
		return nil
	}
	readable, err := store.CanRead(ctx, s.cfg.DB, c.home, b.project)
	if err != nil {
		return err
	}
	if readable {
		writable, err := store.CanWrite(ctx, s.cfg.DB, c.home, b.project)
		if err != nil {
			return err
		}
		if writable {
			return nil
		}
	}
	name := b.sessionID
	if sess, found, err := store.SessionByID(ctx, s.cfg.DB, b.sessionID); err == nil && found {
		name = sess.Name
	}
	why, err := s.speakWhy(ctx, b.project, readable)
	if err != nil {
		return err
	}
	where, home := projectWords(b.project), projectWords(c.home)
	tail := s.askTail(ctx, c, b.project)
	if how == boundByHandle {
		return fmt.Errorf("session %s works in %s, and this conversation works in %s: %s, so it may not act as that "+
			"session\nRemedy: leave session= off to act as your own session %s in %s%s", name, where, home, why,
			c.homeSession.Name, home, tail)
	}
	return fmt.Errorf("this connection is bound to session %s in %s, and this conversation works in %s: %s, so it may "+
		"no longer act as that session\nRemedy: call session_start with no arguments to hand this connection back to your "+
		"own session %s in %s%s", name, where, home, why, c.homeSession.Name, home, tail)
}

// speakWhy says why the home may not act in project: the project's fence (a
// read the home may not make), or what the conversation holds (the clearance
// rule), or the home's own fence. Projects only, never anything they hold.
func (s *Server) speakWhy(ctx context.Context, project string, readable bool) (string, error) {
	state, err := store.IsolationOf(ctx, s.cfg.DB, project)
	if err != nil {
		return "", err
	}
	if !readable {
		if !state.FencesOutbound() {
			// Every conversation reads an open project (and the global scope)
			// but one whose home's fence keeps it in: a sealed home reads
			// nothing outside it, so the fence that refused is the home's.
			return "its home's fence keeps its reads out of " + projectWords(project), nil
		}
		return fmt.Sprintf("%s is %s, and only a conversation working in it reads it", project, state), nil
	}
	if !access.WriteCleared(ctx, project) {
		g, _ := access.GrantsFrom(ctx)
		return fmt.Sprintf("it holds content from %s, which may not be written into %s", strings.Join(g.Label(), ", "),
			projectWords(project)), nil
	}
	return "its home's fence keeps its writes out of " + projectWords(project), nil
}

// askTail completes a remedy with the way to ask for project, when the kind
// rule offers one: "; if your task is about P, session_start project=P asks
// the owner to move this conversation into it" (or for a grant of it).
func (s *Server) askTail(ctx context.Context, c conversation, project string) string {
	switch s.offeredKind(ctx, c.home, project) {
	case store.AccessKindMove:
		return "; if your task is about " + project + ", session_start project=" + project +
			" asks the owner to move this conversation into it"
	case store.AccessKindGrant:
		return "; if your task is about " + project + ", session_start project=" + project + " asks the owner for a grant of it"
	}
	return ""
}

// kindFor is the kind of request the read grants file for target from a
// conversation homed in home (approval.Grants.KindFor), "" when none.
func (s *Server) kindFor(ctx context.Context, home, target string) (string, error) {
	k, ok := s.cfg.Grants.(interface {
		KindFor(ctx context.Context, home, target string) (string, error)
	})
	if !ok {
		return "", errors.New("this server cannot tell which request a project calls for")
	}
	return k.KindFor(ctx, home, target)
}

// offeredKind is the kind of request a refusal's words offer for target from
// home: kindFor's answer, "" where the kind rule offers none. A lookup failure
// offers none too, and is logged: the words are guidance, and the refusal
// itself is already decided.
func (s *Server) offeredKind(ctx context.Context, home, target string) string {
	kind, err := s.kindFor(ctx, home, target)
	if err != nil {
		if !errors.Is(err, approval.ErrNotConfidential) && !errors.Is(err, approval.ErrSealed) && !errors.Is(err, approval.ErrIsHome) {
			s.logger.Warn("mcp: the request a fence offers", "project", target, "error", err)
		}
		return ""
	}
	return kind
}

// projectWords names a project for a sentence: "project P", or "the global
// scope".
func projectWords(project string) string {
	if project == "" {
		return "the global scope"
	}
	return "project " + project
}

// askFenced files (or finds) the request the identified conversation c's home
// and target call for (approval.Grants.Ask): a move, or a grant asking for
// read and write when write. The asking session is the conversation's own
// ambient row, never the call's binding, so a session= handle naming another
// agent's session asks nothing in that agent's name.
func (s *Server) askFenced(ctx context.Context, c conversation, target string, write bool, via string) (store.AccessApproval, error) {
	asker, ok := s.cfg.Grants.(grantAsker)
	if !ok {
		return store.AccessApproval{}, errors.New("this server cannot file a request for a fenced project")
	}
	return asker.Ask(access.WithLockRequester(ctx, nil), approval.AskInput{GrantInput: approval.GrantInput{
		Project: target, SessionID: c.homeSession.ID, AgentProcess: callerAgentProcess(ctx), Host: s.callerHost(ctx), Via: via,
	}, Write: write})
}

// askRefusal turns refusal f at fenced project target, refused to the
// identified conversation c, into the same refusal naming the request it
// filed: the rule on its first line as the fence wrote it, the remedy on the
// next. A refused read asks for the read (Ask without write: a move from an
// open home or for a sealed project, a read grant between confidential
// ones), a refused write for read and write. Where the kind rule offers
// nothing (an open project, a sealed home) f comes back unchanged.
func (s *Server) askRefusal(ctx context.Context, c conversation, f *fenceRefusal, target string, write bool) error {
	a, err := s.askFenced(ctx, c, target, write, approval.ViaRefusal)
	if err != nil {
		s.logger.Debug("fenced request not filed", "project", target, "error", err)
		if remedy, ok := s.unfiledRemedy(err, target); ok {
			return &fenceRefusal{reason: f.reason, err: fmt.Errorf("%w%s", f.err, remedy)}
		}
		return f
	}
	noteDialogTarget(ctx, targetOf(a))
	return &fenceRefusal{reason: f.reason, err: fmt.Errorf("%w\nRemedy: %s", f.err, s.requestRemedy(ctx, c, a))}
}

// unfiledRemedy is the tail of a refusal whose request was not filed for a
// reason the agent can act on: an anchor no approval may name (the rule, then
// what to do instead), or the owner's denial of its last request (when, and
// how to ask again). ok is false for every other reason, which leaves the
// refusal as it was.
func (s *Server) unfiledRemedy(err error, target string) (string, bool) {
	var anchor *approval.AnchorError
	if errors.As(err, &anchor) {
		return "; " + strings.Replace(anchor.Rule(), "no access request", "no request", 1) + ".\nRemedy: " + anchor.Remedy(), true
	}
	var denied *approval.DeniedError
	if errors.As(err, &denied) {
		return "\nRemedy: " + access.DeniedRemedy(denied.What()+" "+approval.ShortID(denied.Request.ID), denied.At(), target,
			s.cfg.OfferAccessRequest), true
	}
	return "", false
}

// targetOf is the dialog target of a request the read grants filed.
func targetOf(a store.AccessApproval) dialogTarget {
	return dialogTarget{ID: a.ID, Short: approval.ShortID(a.ID), Project: a.Project,
		Grant: a.Kind == store.AccessKindGrant, Move: a.Kind == store.AccessKindMove,
		Write: a.Kind == store.AccessKindGrant && a.RequestedAccess == store.GrantAccessWrite}
}

// requestRemedy is the remedy naming request a, filed for the identified
// conversation c: how the owner approves it, and what an approval brings.
func (s *Server) requestRemedy(ctx context.Context, c conversation, a store.AccessApproval) string {
	ways := s.requestWays(ctx, a)
	switch {
	case a.Kind == store.AccessKindMove:
		return s.moveRemedy(ctx, a, ways)
	case a.RequestedAccess == store.GrantAccessWrite:
		return s.writeGrantRemedy(a, ways)
	}
	fromFenced := false
	if iso, err := store.IsolationOf(ctx, s.cfg.DB, a.FromProject); err == nil && iso.FencesOutbound() {
		fromFenced = true
	}
	g, _ := access.GrantsFrom(ctx)
	return s.grantRemedy(a, g.WithRead(a.Project, a.FromProject, fromFenced), ways)
}

// requestWays are the approval paths a remedy offers for request a: the
// daemon's, Touch ID only for a client that can show its dialog, and never
// for a move, which the Touch ID approver does not word
// (approval.ErrTouchIDMove).
func (s *Server) requestWays(ctx context.Context, a store.AccessApproval) access.Ways {
	ways := callWays(ctx, s.cfg.Grants.Ways(ctx, a.ID))
	if a.Kind == store.AccessKindMove {
		ways.TouchID = false
	}
	return ways
}

// ownerApproves says how the owner approves request short: the paths ways
// holds, or the advisory tier's terminal and console.
func (s *Server) ownerApproves(short string, ways access.Ways) string {
	if s.cfg.Grants.Advisory() {
		return advisoryApproves(short)
	}
	return ways.OwnerApproves(short)
}

// moveUntil is how long a moved conversation stays out of the home it left:
// its process's life under peer_auth, the conversation's without it.
func (s *Server) moveUntil() string {
	if s.cfg.Grants != nil && !s.cfg.Grants.Advisory() {
		return "the agent process exits"
	}
	return "the conversation ends"
}

// moveRemedy is the remedy of a move request: how the owner approves it, and
// -- before anything else is done -- that what belongs in the home is
// recorded there now, since after the move the conversation cannot write
// there, save the receipts that close a task it claimed there
// (receiptAfterMove).
func (s *Server) moveRemedy(ctx context.Context, a store.AccessApproval, ways access.Ways) string {
	short := approval.ShortID(a.ID)
	leave := "Record what belongs in " + a.FromProject + " now: once the owner approves the move, this conversation works " +
		"in " + a.Project + " for good, and it can no longer write to " + a.FromProject + " or the global scope until " +
		s.moveUntil() + "." + s.receiptAfterMove(ctx, a.FromProject, a.Project)
	if a.FromProject == "" {
		leave = "Record what belongs in the global scope now: once the owner approves the move, this conversation works in " +
			a.Project + " for good, and it can no longer write to the global scope until " + s.moveUntil() + "."
	}
	return fmt.Sprintf("move request %s is waiting -- %s; retry once it is approved. %s Its first result after the "+
		"approval says so and carries %s's briefing.", short, s.ownerApproves(short, ways), leave, a.Project)
}

// receiptAfterMove is the sentence a move request's words add about the one
// write the move leaves the conversation in the home it leaves: a receipt
// (store.JudgeReceipt), which closes a task its pre-move session claimed and
// carries no text. "" for a move out of the global scope, and for a move into
// a sealed project, whose conversation sends no receipts -- or when the
// target's state cannot be read, since then the words promise nothing.
func (s *Server) receiptAfterMove(ctx context.Context, from, target string) string {
	if from == "" || !s.sendsReceipts(ctx, target) {
		return ""
	}
	return " After the move it can still close a task it claimed there, with a receipt: tasks_update with status done " +
		"or dropped, and no text."
}

// sendsReceipts reports whether a conversation homed in home may send
// receipts once it moved there: every home but a sealed one. A state that
// cannot be read sends none, as far as the words go.
func (s *Server) sendsReceipts(ctx context.Context, home string) bool {
	state, err := store.IsolationOf(ctx, s.cfg.DB, home)
	if err != nil {
		s.logger.Warn("mcp: the state of a move's target", "project", home, "error", err)
		return false
	}
	return state != core.IsolationSealed
}

// writeGrantRemedy is the remedy of a grant request that asks for read and
// write: how the owner approves it, and what each answer brings. The owner
// chooses; read-only is the default.
func (s *Server) writeGrantRemedy(a store.AccessApproval, ways access.Ways) string {
	short := approval.ShortID(a.ID)
	return fmt.Sprintf("read-and-write request %s is waiting -- %s; retry once it is approved. Approved read and write, "+
		"this %s changes %s's tasks, memories and notes as its own sessions do; approved read-only, it reads %s and writes "+
		"nothing there. %s", short, s.ownerApproves(short, ways), s.holderWord(), a.Project, a.Project,
		approval.CiteByReferenceLine(a.Project, a.FromProject))
}

// bindRemedyFor is bindRemedy for the call in ctx: for an identified
// conversation (isolation.requests) binding is no way into a fenced project,
// so it names the request the kind rule offers instead; for every other
// caller it is bindRemedy, word for word.
func (s *Server) bindRemedyFor(ctx context.Context, project string) string {
	c, ok := identifiedConversation(ctx)
	if !ok {
		return bindRemedy(project)
	}
	switch s.offeredKind(ctx, c.home, project) {
	case store.AccessKindMove:
		return " -- if your task is about " + project + ", ask the owner to move this conversation into it (session_start " +
			"project=" + project + " asks); otherwise leave it alone"
	case store.AccessKindGrant:
		return " -- if your task is about " + project + ", ask the owner for a grant of it (session_start project=" +
			project + " asks); otherwise leave it alone"
	}
	return "; leave it alone"
}

// sessionStartDest is the project a session_start would bind the identified
// conversation c to: its home when nothing pins another (its own session is
// adopted), the named session's project when one is resumed and no project
// pins it, else the project the argument or the cwd named.
func sessionStartDest(c conversation, name string, pinned bool, project string, existing core.Session, found bool) string {
	switch {
	case name == "" && !pinned:
		return c.home
	case name != "" && found && !pinned:
		return existing.ProjectSlug
	}
	return project
}

// sessionStartReach is session_start's word for an identified conversation c
// (D4 of the fenced-sessions design): nil to bind as before -- its home, or an
// open project (or the global scope) while it holds nothing fenced -- else
// the refusal. Into a fenced project it files the request the home and the
// project call for instead of binding (a move, or a grant asking for read and
// write; the home stays), and the refusal names it. Into an open project or
// the global scope from a conversation that holds fenced content, native ones
// included, it is refused: there is no way back.
func (s *Server) sessionStartReach(ctx context.Context, c conversation, dest string) error {
	if dest == c.home {
		return nil
	}
	state, err := store.IsolationOf(ctx, s.cfg.DB, dest)
	if err != nil {
		return err
	}
	if !state.FencesOutbound() {
		if access.WriteCleared(ctx, dest) {
			return nil
		}
		g, _ := access.GrantsFrom(ctx)
		// The reference form is core's, the one the gardener reads back from a
		// body (core.Citations): a copy citing its sources this way is one the
		// gardener can flag when a source changes, or propose moving.
		return fmt.Errorf("nothing was bound: this conversation holds content from %s, so it does not work in %s: a "+
			"conversation holding a fenced project's content never goes back to an open project or the global scope"+
			"\nRemedy: keep working in %s, your own session %s; record there what you meant to record in %s, citing other "+
			"projects' items by reference, each written %s with the item's full id", strings.Join(g.Label(), ", "),
			projectWords(dest), projectWords(c.home), c.homeSession.Name, projectWords(dest), core.CitationForm("<project>"))
	}
	a, err := s.askFenced(ctx, c, dest, true, approval.ViaRefusal)
	var live *approval.AccessLiveError
	switch {
	case errors.As(err, &live):
		return fmt.Errorf("nothing was bound: this conversation works in %s, which stays its home, and it already reaches %s "+
			"through its grant\nRemedy: %s; name project=%s on the calls that work there", projectWords(c.home), dest,
			live.Answer(), dest)
	case errors.Is(err, approval.ErrSealed):
		return fmt.Errorf("nothing was bound: this conversation works in sealed project %s, and a sealed project's "+
			"conversation reaches no confidential project\nRemedy: keep working in %s", c.home, c.home)
	case err != nil:
		rule := fmt.Errorf("nothing was bound: project %s is %s, and this conversation works in %s", dest, state,
			projectWords(c.home))
		if remedy, ok := s.unfiledRemedy(err, dest); ok {
			return fmt.Errorf("%w%s", rule, remedy)
		}
		return err
	}
	noteDialogTarget(ctx, targetOf(a))
	ways := s.requestWays(ctx, a)
	if a.Kind == store.AccessKindMove {
		how := "it works in " + dest + " only once the owner moves it there"
		if state == core.IsolationSealed {
			how = "a conversation reaches " + dest + " only by moving in, once the owner approves"
		}
		return fmt.Errorf("nothing was bound: project %s is %s, and this conversation works in %s, so %s\nRemedy: %s",
			dest, state, projectWords(c.home), how, s.moveRemedy(ctx, a, ways))
	}
	return fmt.Errorf("nothing was bound: project %s is %s, and this conversation works in confidential project %s, which "+
		"stays its home: it reaches %s only with the owner's grant\nRemedy: %s Once it is approved, name project=%s on the "+
		"calls that work there", dest, state, c.home, dest, s.writeGrantRemedy(a, ways), dest)
}

// unrecordedEnd reports whether session_end may still end sess, refused
// (err) only because its findings may not be written where it works: the
// clearance rule refuses the session's project, and sess is one of the
// conversation's own sessions. The session still ends -- its claims, leases
// and grants with it -- and line is what its findings become: why nothing
// was recorded, naming projects only. A session the conversation does not
// own, or one any other fence refuses, is refused as before.
func (s *Server) unrecordedEnd(ctx context.Context, sess core.Session, err error) (string, bool) {
	var f *fenceRefusal
	if s.cfg.Grants == nil || !errors.As(err, &f) || !access.Writable(ctx, sess.ProjectSlug) ||
		access.WriteCleared(ctx, sess.ProjectSlug) {
		return "", false
	}
	c, ok := conversationOf(ctx)
	if !ok {
		return "", false
	}
	own := c.ref.SessionID != "" && sess.ID == c.ref.SessionID
	if c.ref.Client != "" && sess.ExternalClient == c.ref.Client && sess.ExternalSessionID == c.ref.ExternalID {
		own = true
	}
	if !own {
		return "", false
	}
	g, _ := access.GrantsFrom(ctx)
	return store.UnrecordedFindings(s.holderWord(), g.Label(), projectWords(sess.ProjectSlug)), true
}

// The move notice: the first tool result after an approved move says the
// conversation moved and carries its new home's briefing, once.

// moveNoticeSlotKey keys the per-call slot moveNoticeMiddleware plants and
// grantsMiddleware fills (noteMove).
type moveNoticeSlotKey struct{}

// moveNoticeSlot carries the moved conversation's new home session out of the
// call, the same-goroutine hand-off sessionHandleSlot is.
type moveNoticeSlot struct {
	child core.Session
	ok    bool
}

// moveSeen is the set of move children this server already found briefed,
// so a moved conversation costs one lookup per daemon run, not one per call.
type moveSeen struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func (m *moveSeen) has(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.seen[id]
	return ok
}

func (m *moveSeen) add(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = map[string]struct{}{}
	}
	m.seen[id] = struct{}{}
}

// noteMove hands the identified conversation's home session out to
// moveNoticeMiddleware when it is a move's child this server has not seen
// briefed.
func (s *Server) noteMove(ctx context.Context, c conversation) {
	slot, planted := ctx.Value(moveNoticeSlotKey{}).(*moveNoticeSlot)
	if !planted || !c.identified || c.homeSession.ParentSessionID == "" || s.moves.has(c.homeSession.ID) {
		return
	}
	slot.child, slot.ok = c.homeSession, true
}

// moveNoticeMiddleware appends the move notice to the first result after an
// approved move (moveNews). It runs outside logMiddleware, as the lease
// notices do: the briefing is the new home's content, and must not be copied
// into the call's tool.call event. Off (no read grants) it is a pass-through.
func (s *Server) moveNoticeMiddleware(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if s.cfg.Grants == nil {
			return next(ctx, req)
		}
		slot := &moveNoticeSlot{}
		res, err := next(context.WithValue(ctx, moveNoticeSlotKey{}, slot), req)
		if err != nil || res == nil || !slot.ok {
			return res, err
		}
		if text := s.moveNews(ctx, slot.child); text != "" {
			res.Content = append(res.Content, mcp.NewTextContent(text))
		}
		return res, err
	}
}

// moveNews is what a result says the first time any call reaches a moved
// conversation's new home session child: the move, what it means, and the
// home's briefing. "Once" is the move row's briefed_at
// (store.ClaimMoveBriefing), the access_leases.briefed_at pattern: atomic,
// claimed before the briefing renders, kept across a restart. A move into a
// locked project carries the project's lease (store.MoveApproveWith), whose
// own briefing (leaseNoticeMiddleware) is the same one: the notice claims the
// lease's briefing too and carries it only when it wins, so whichever notice
// runs first sends it, and nothing sends it twice.
func (s *Server) moveNews(ctx context.Context, child core.Session) string {
	mi, found, err := store.MoveIntoSession(ctx, s.cfg.DB, child.ID)
	if err != nil {
		s.logger.Warn("mcp: move notice", "session_id", child.ID, "error", err)
		return ""
	}
	if !found || mi.Briefed {
		s.moves.add(child.ID)
		return ""
	}
	now := s.now()
	won, err := store.ClaimMoveBriefing(ctx, s.cfg.DB, mi.Move.ID, now)
	if err != nil {
		s.logger.Warn("mcp: claim move briefing", "session_id", child.ID, "error", err)
		return ""
	}
	s.moves.add(child.ID)
	if !won {
		return ""
	}
	project, from := mi.Move.ToProject, mi.Move.FromProject
	head := "This conversation moved into " + project + ": the owner approved move request " + approval.ShortID(mi.Move.ApprovalID) +
		". " + project + " is its home from now on -- its record goes there, and calls that name no project work there -- and " +
		"it can no longer write to " + projectWords(from) + " or the global scope until " + s.moveUntil() + "."
	if from == "" {
		head = "This conversation moved into " + project + ": the owner approved move request " + approval.ShortID(mi.Move.ApprovalID) +
			". " + project + " is its home from now on -- its record goes there, and calls that name no project work there -- and " +
			"it can no longer write to the global scope until " + s.moveUntil() + "."
	}
	brief := true
	if mi.LeaseID != "" {
		if lease, err := store.AccessLeaseByID(ctx, s.cfg.DB, mi.LeaseID); err == nil && lease.ExpiresAt.After(now) {
			head += " Its lock is open to this agent until " + access.Clock(lease.ExpiresAt) + "."
		}
		leaseWon, err := store.ClaimAccessLeaseBriefing(ctx, s.cfg.DB, mi.LeaseID, now)
		if err != nil {
			s.logger.Warn("mcp: claim the move's lease briefing", "project", project, "error", err)
		}
		brief = err == nil && leaseWon
	}
	head += s.afterMoveLines(ctx, mi.Move)
	if !brief {
		return head
	}
	briefing, _, err := s.cfg.Retrieve.ProjectBriefing(access.WithLockRequester(ctx, nil), project,
		retrieve.BriefingInput{Host: s.callerHost(ctx), Source: "approval"})
	if err != nil {
		// Not sent: let the next result try again rather than claim a
		// briefing nobody received. The call's context may be why it failed,
		// and must not stop the claims being given back.
		back := context.WithoutCancel(ctx)
		if ferr := store.ForgetMoveBriefing(back, s.cfg.DB, mi.Move.ID); ferr != nil {
			s.logger.Warn("mcp: release move briefing", "project", project, "error", ferr)
		}
		if mi.LeaseID != "" {
			if ferr := store.ForgetAccessLeaseBriefing(back, s.cfg.DB, mi.LeaseID); ferr != nil {
				s.logger.Warn("mcp: release the move's lease briefing", "project", project, "error", ferr)
			}
		}
		s.moves.forget(child.ID)
		s.logger.Warn("mcp: briefing after a move", "project", project, "error", err)
		return ""
	}
	if strings.TrimSpace(briefing) == "" {
		return head
	}
	return head + " This is its briefing, sent once:\n" + briefing
}

// afterMoveLines is what the move notice adds about the home m left. A task
// the pre-move session claimed is closed with a receipt (store.JudgeReceipt:
// status done or dropped, no text, at most one reference to an item in the new
// home), which the move's remedy promised (receiptAfterMove) -- unless the new
// home is sealed, whose conversation sends none. And a move out of a fenced
// home, which is always into a sealed project (a confidential home reaches
// another confidential project by grant, approval.KindFor), is told where what
// it writes about that home goes: its new home, citing the home's items by
// reference in the one form the gardener reads (core.CitationForm), as a
// grant's holder is asked to (approval.CiteByReferenceLine). Nothing for a
// move out of the global scope.
func (s *Server) afterMoveLines(ctx context.Context, m store.Move) string {
	if m.FromProject == "" {
		return ""
	}
	var out string
	if s.sendsReceipts(ctx, m.ToProject) {
		who := "it"
		if left, ok, err := store.SessionByID(ctx, s.cfg.DB, m.FromSessionID); err == nil && ok {
			who = "its session " + left.Name
		}
		out += " It can still close a task " + who + " claimed before the move, with a receipt: tasks_update with status " +
			"done or dropped and no text, optionally with reference=" + core.CitationForm(m.ToProject) + " naming one item in " +
			m.ToProject + "."
	}
	if from, err := store.IsolationOf(ctx, s.cfg.DB, m.FromProject); err == nil && from.FencesOutbound() {
		out += " What it writes about " + m.FromProject + " goes in " + m.ToProject + ", citing " + m.FromProject +
			"'s items by reference, each written " + core.CitationForm(m.FromProject) + " with the item's full id."
	}
	return out
}

func (m *moveSeen) forget(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.seen, id)
}
