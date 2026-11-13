package mcp

// Read grants at the tool surface (isolation.requests, off by default; Config
// .Grants set). Off, nothing here runs: no tool call carries grants, the fence
// refuses word for word as before, and recall's scope is the old one.
//
// On:
//
//   - every tool call carries the judged state of the conversation it speaks
//     for (grantsMiddleware, store.ConversationClearance): under peer_auth the
//     kernel-verified process chain of the owner's socket connection, else
//     the conversation the agent-process identity names (conversationRef --
//     identification, never ambient inference; a client that sends no
//     identity is held by its bound session, as before). The access layer
//     judges the rows (access.JudgeConversation) and store.CanRead / CanWrite
//     ask the state, so every fence inherits it: a live grant opens one
//     confidential project to the one project it was approved from, and every
//     write must be cleared for everything the conversation holds
//     (access.WriteCleared: its label, the flows the owner approved, its
//     home);
//   - a read refused at a confidential project's fence files a request
//     (grantRequestRefusal) when the caller is an identified session in an
//     open or confidential project, and the refusal names it; anything that
//     involves a sealed project files nothing;
//   - the first tool result after a grant lands says so, with what it allows
//     and the confinement it brings; from access.ExpiryWarning before it ends,
//     every result says when it ends;
//   - a granted project joins recall's scope while the grant lives (the
//     briefing is unchanged);
//   - session_end revokes the session's grants ("until the session ends").

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// Grants is what the MCP server needs of the daemon's read grants
// (*approval.Grants).
type Grants interface {
	RequestGrant(ctx context.Context, in approval.GrantInput) (store.AccessApproval, error)
	Page(id string) string
	Ways(ctx context.Context, id string) access.Ways
	RevokeSession(ctx context.Context, sessionID string) (int64, error)
	Advisory() bool
}

// grantsMiddleware plants the call's judged grants and, after the call,
// appends the grant notices. It runs innermost, after the session= handle
// resolved, so the binding it reads is the call's own. The notices hold slugs
// and times only, never a granted project's content.
func (s *Server) grantsMiddleware(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if s.cfg.Grants == nil {
			return next(ctx, req)
		}
		g, err := s.loadGrants(ctx)
		if err != nil {
			// Fail closed both ways: without its grants a call could neither be
			// trusted to read nor to stay confined.
			s.logger.Warn("mcp: load read grants", "tool", req.Params.Name, "error", err)
			return errResult(req.Params.Name, errors.New("the read grants of this connection could not be checked: try again"))
		}
		ctx = access.WithGrants(ctx, g)
		res, err := next(ctx, req)
		if err != nil || res == nil {
			return res, err
		}
		if req.Params.Name == "session_end" && !res.IsError {
			// Its grants were just revoked: nothing to announce or warn about.
			return res, err
		}
		now := s.now()
		live := g.Live()
		var notes []string
		for _, l := range live {
			if s.claimGrant(l, now) {
				notes = append(notes, s.grantNews(ctx, l))
			}
		}
		ending := make([]access.Lease, len(live))
		for i, l := range live {
			ending[i] = access.Lease{ID: l.ID, Project: l.Project, Expires: l.Expires}
		}
		if text := access.ExpiryNotice(ending, now); text != "" {
			notes = append(notes, text)
		}
		for _, n := range notes {
			res.Content = append(res.Content, mcp.NewTextContent(n))
		}
		return res, err
	}
}

// loadGrants judges the conversation this call speaks for
// (store.ConversationClearance): the owner's socket process chain under
// peer_auth (a TCP request or a restricted account holds nothing), else the
// conversation conversationRef names.
func (s *Server) loadGrants(ctx context.Context) (access.Grants, error) {
	ref, err := s.conversationRef(ctx)
	if err != nil {
		return access.Grants{}, err
	}
	return store.ConversationClearance(ctx, s.cfg.DB, ref, s.now())
}

// conversationRef names the conversation this call speaks for: the client and
// session id of the agent's own session, found from the agent-process
// identity the connection named (listedProcessSession) -- never the
// connection's binding, so neither session_start, session= nor a move changes
// it. Under peer_auth that is all (the store holds it to the kernel's chain).
// Without peer_auth, a call whose process names no such session falls back to
// the session it is bound to (getBinding): that session's conversation when
// its row carries one, else the session itself -- a client that sends no
// agent-process identity, today's holder and the documented limit.
func (s *Server) conversationRef(ctx context.Context) (store.ConversationRef, error) {
	if own, ok := s.listedProcessSession(ctx); ok && own.ExternalClient != "" && own.ExternalSessionID != "" {
		return store.ConversationRef{Client: own.ExternalClient, ExternalID: own.ExternalSessionID}, nil
	}
	if _, hasView := access.FromContext(ctx); hasView {
		return store.ConversationRef{}, nil
	}
	b, ok := s.getBinding(ctx)
	if !ok || b.sessionID == "" {
		return store.ConversationRef{}, nil
	}
	sess, found, err := store.SessionByID(ctx, s.cfg.DB, b.sessionID)
	if err != nil {
		return store.ConversationRef{}, err
	}
	if found && sess.ExternalClient != "" && sess.ExternalSessionID != "" {
		return store.ConversationRef{Client: sess.ExternalClient, ExternalID: sess.ExternalSessionID}, nil
	}
	return store.ConversationRef{SessionID: b.sessionID}, nil
}

// grantRequestRefusal turns a read refused at a confidential project's fence
// into the same refusal naming the access request it filed: the rule on its
// first line, word for word as before, and the remedy on the next. It files
// only for an identified session (a binding, never ambient inference) bound to
// a project, and only what approval.Grants.RequestGrant accepts -- a
// confidential target, asked from a project that is not sealed. Anything else
// -- a sealed project on either side, a peer_auth refusal, a store failure --
// comes back unchanged and files nothing.
func (s *Server) grantRequestRefusal(ctx context.Context, err error, target string) error {
	var f *fenceRefusal
	if s.cfg.Grants == nil || !errors.As(err, &f) || f.reason != screening.RefusedFenceRead {
		return err
	}
	b, ok := s.getBinding(ctx)
	if !ok || b.sessionID == "" || b.project == "" {
		return err
	}
	a, rerr := s.cfg.Grants.RequestGrant(access.WithLockRequester(ctx, nil), approval.GrantInput{
		Project: target, From: b.project, SessionID: b.sessionID,
		AgentProcess: callerAgentProcess(ctx), Host: s.callerHost(ctx), Via: approval.ViaRefusal,
	})
	if rerr != nil {
		s.logger.Debug("read grant request not filed", "project", target, "error", rerr)
		// A process no approval may name (approval.AnchorError): the fence's
		// refusal, then why nothing was filed and what to do instead.
		var anchor *approval.AnchorError
		if errors.As(rerr, &anchor) {
			return &fenceRefusal{reason: f.reason, err: fmt.Errorf("%w; %s.\nRemedy: %s", f.err,
				strings.Replace(anchor.Rule(), "no access request", "no read request", 1), anchor.Remedy())}
		}
		// The owner denied this asker's last read request for the pair: the
		// fence's refusal, then the denial and how to ask again; nothing new
		// filed, no dialog.
		var denied *approval.DeniedError
		if errors.As(rerr, &denied) {
			return &fenceRefusal{reason: f.reason, err: fmt.Errorf("%w\nRemedy: %s", f.err,
				access.DeniedRemedy(denied.What()+" "+approval.ShortID(denied.Request.ID), denied.At(), target, s.cfg.OfferAccessRequest))}
		}
		return err
	}
	// The refusal names this request: the approval dialog may offer it
	// (elicit.go; only ever on the peer_auth socket).
	noteDialogTarget(ctx, dialogTarget{ID: a.ID, Short: approval.ShortID(a.ID), Project: a.Project, Grant: true})
	fromFenced := false
	if iso, ierr := store.IsolationOf(ctx, s.cfg.DB, b.project); ierr == nil && iso.FencesOutbound() {
		fromFenced = true
	}
	g, _ := access.GrantsFrom(ctx)
	after := g.WithRead(a.Project, a.FromProject, fromFenced)
	return &fenceRefusal{reason: f.reason, err: s.grantRefusal(f.err, a, after, callWays(ctx, s.cfg.Grants.Ways(ctx, a.ID)))}
}

// grantRefusal is the fence's refusal with its remedy line: the request, how
// the owner approves it (only the paths ways holds), and what an approval
// brings with it -- judged from after, the conversation as it would stand
// once the read is approved (access.Grants.WithRead).
func (s *Server) grantRefusal(rule error, a store.AccessApproval, after access.Grants, ways access.Ways) error {
	short := approval.ShortID(a.ID)
	how := ways.OwnerApproves(short)
	if s.cfg.Grants.Advisory() {
		how = fmt.Sprintf("the owner approves it in their own terminal with `seam approve %s`, or with Approve on the "+
			"console's Access requests page", short)
	}
	brings := "An approved read is read-only, and nothing read from " + a.Project + " may be written anywhere but " + a.FromProject +
		". " + approval.CiteByReferenceLine(a.Project, a.FromProject)
	if after.Judged() && !after.Cleared(a.FromProject) {
		// Once the read is held, the asking project is not cleared for it: an
		// open project never is, a fenced one when the conversation holds what
		// no approval let into it.
		brings = fmt.Sprintf("An approved read is read-only, and confines this %s's writes to %s for the rest of its life",
			s.holderWord(), confinedTo(after.ClearedProjects()))
		if !slices.Contains(after.Label(), a.FromProject) {
			brings += fmt.Sprintf(": once it holds %s's content, it writes into no open project, %s included, and not into "+
				"the global scope", a.Project, a.FromProject)
		}
		brings += "."
	}
	return fmt.Errorf("%w\nRemedy: read request %s is waiting -- %s; retry once it is approved. %s", rule, short, how, brings)
}

// confinedTo names the projects a conversation may still write into, for a
// sentence ending "writes ... to <this>".
func confinedTo(cleared []string) string {
	if len(cleared) == 0 {
		return "no project"
	}
	return strings.Join(cleared, ", ")
}

// holderWord names what holds a grant: a session without peer_auth, the agent
// (its whole process tree) under it.
func (s *Server) holderWord() string {
	if s.cfg.Grants != nil && !s.cfg.Grants.Advisory() {
		return "agent"
	}
	return "session"
}

// grantNews is what a result says about a grant the first time this server
// sees it: what was approved, until when, and where the conversation may now
// write (the judged state ctx carries, which holds the grant).
func (s *Server) grantNews(ctx context.Context, l access.Grant) string {
	g, _ := access.GrantsFrom(ctx)
	what := "Read access to %s was approved until %s, read-only."
	if l.Write {
		what = "Read and write access to %s was approved until %s."
	}
	head := fmt.Sprintf(what, l.Project, access.Clock(l.Expires))
	if g.Judged() && !g.Cleared(l.From) {
		// Its asking project is not cleared for what it now holds (an open
		// project): it writes only where it may, and documents nothing there.
		return head + fmt.Sprintf(" This %s now writes only into %s, for the rest of its life: a write to another project "+
			"or the global scope is refused.", s.holderWord(), confinedTo(g.ClearedProjects()))
	}
	if l.Write {
		return head + fmt.Sprintf(" This %s now writes only into %s. %s", s.holderWord(), confinedTo(g.ClearedProjects()),
			approval.CiteByReferenceLine(l.Project, l.From))
	}
	head += fmt.Sprintf(" Nothing read from %s may be written anywhere but %s. %s", l.Project, l.From,
		approval.CiteByReferenceLine(l.Project, l.From))
	if g.Judged() {
		return head
	}
	iso, err := store.IsolationOf(ctx, s.cfg.DB, l.From)
	if err != nil || iso != core.IsolationOpen {
		return head
	}
	return head + fmt.Sprintf(" This %s now writes only into %s, for the rest of its life: a write to another project "+
		"or the global scope is refused.", s.holderWord(), l.From)
}

// clearanceRefusal is the refusal of a write the clearance rule refuses
// (access.WriteCleared): what the conversation holds, and where it may still
// write. It names projects only, never anything they hold.
func (s *Server) clearanceRefusal(ctx context.Context, target string) error {
	g, _ := access.GrantsFrom(ctx)
	where := "project " + target
	if target == "" {
		where = "the global scope"
	}
	if !g.Judged() {
		confined, _ := g.Confined()
		return fmt.Errorf("this %s was granted a read of another project, so it writes only into %s for the rest of its life: "+
			"a write into %s is refused", s.holderWord(), strings.Join(confined, ", "), where)
	}
	cleared := g.ClearedProjects()
	if len(cleared) == 0 {
		return fmt.Errorf("this %s holds content from %s, and no project is cleared for all of it: a write into %s is refused",
			s.holderWord(), strings.Join(g.Label(), ", "), where)
	}
	return fmt.Errorf("this %s holds content from %s, so it writes only into %s: a write into %s is refused",
		s.holderWord(), strings.Join(g.Label(), ", "), strings.Join(cleared, ", "), where)
}

// claimGrant records a grant as announced and reports whether this call is
// the first to see it. The set is in memory: a daemon restart announces a
// live grant once more, a harmless repeat.
func (s *Server) claimGrant(l access.Grant, now time.Time) bool {
	s.grantMu.Lock()
	defer s.grantMu.Unlock()
	if _, seen := s.grantSeen[l.ID]; seen {
		return false
	}
	for id, expires := range s.grantSeen {
		if !expires.After(now) {
			delete(s.grantSeen, id)
		}
	}
	s.grantSeen[l.ID] = l.Expires
	return true
}

// grantedScope is the projects a recall in project also searches: the
// targets of the live grants approved from it, each still readable from the
// caller's own scope. A recall naming another project gets none.
func (s *Server) grantedScope(ctx context.Context, project string) ([]string, error) {
	if s.cfg.Grants == nil || project == "" || project != s.callerScope(ctx) {
		return nil, nil
	}
	var out []string
	for _, target := range access.GrantedTargets(ctx, project) {
		ok, err := store.CanRead(ctx, s.cfg.DB, project, target)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, target)
		}
	}
	return out, nil
}

// revokeSessionGrants ends a finished session's read grants. A failure is
// logged, never the session_end's.
func (s *Server) revokeSessionGrants(ctx context.Context, sessionID string) {
	if s.cfg.Grants == nil || sessionID == "" {
		return
	}
	if _, err := s.cfg.Grants.RevokeSession(ctx, sessionID); err != nil {
		s.logger.Warn("session_end: revoke the session's read grants", "session_id", sessionID, "error", err)
	}
}
