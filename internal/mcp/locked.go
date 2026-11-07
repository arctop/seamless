package mcp

// Locked projects (peer_auth.locked): a tool call refused at a lock files an
// access request automatically, and the refusal names it. The access layer
// produces every refusal (access.Refusal); this file only plants the requester
// it calls, for calls that can carry a lease at all -- the owner's socket
// connections with a kernel-verified process chain -- and only while the
// daemon locks a project (Config.Approvals set). Off, nothing here runs.
//
// It also carries the locked view at the tool surface:
//
//   - a connection whose own session lies in a locked project (the
//     SessionStart hook records it there, metadata only, before any approval)
//     is bound to nothing (getBinding), and a call that would resolve its
//     scope or its session through that binding is refused at the lock
//     (lockedBinding) instead of being inferred into some other project;
//   - once a lease lands, the first tool result after it carries the
//     project's briefing, once per lease (leaseNoticeMiddleware);
//   - from access.ExpiryWarning before a lease ends, every tool result says
//     when it ends and how to ask again.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// Approvals is what the MCP server needs of the daemon's approval service
// (*approval.Service). Facts, PasskeysOn and LeaseLength serve the
// in-session approval dialog (elicit.go): what it shows the owner, whether it
// may offer a passkey, and how long "until the session ends" can last. Ways
// is what every remedy offers: only the approval paths this daemon has.
type Approvals interface {
	Request(ctx context.Context, in approval.RequestInput) (store.AccessApproval, error)
	Page(id string) string
	Ways(ctx context.Context, id string) access.Ways
	RevokeSession(ctx context.Context, sessionID string) (int64, error)
	Facts(ctx context.Context, id string) (approval.Facts, error)
	PasskeysOn() bool
	LeaseLength(choice string) (time.Duration, error)
}

// now is the clock the lease notices and the ended wording read.
func (s *Server) now() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// lockRequestMiddleware plants the lock requester for one tool call. It runs
// innermost, after the session= handle resolved, so the requester sees the
// call's own binding.
func (s *Server) lockRequestMiddleware(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if s.cfg.Approvals == nil {
			return next(ctx, req)
		}
		v, ok := access.FromContext(ctx)
		if !ok || !v.Principal().HasProcess || v.Principal().Kind != access.KindOwner {
			return next(ctx, req)
		}
		return next(access.WithLockRequester(ctx, s.lockRequester), req)
	}
}

// lockRequester files (or finds) the request for a locked project on behalf
// of this call. The lookups it makes run with no requester in their context:
// resolving the bound session may meet a refusal of its own, which must not
// file again. The session it names is the call's binding, else the
// connection's own session in a locked project (lockedBinding) -- the one the
// SessionStart hook recorded there.
func (s *Server) lockRequester(ctx context.Context, project string) (access.LockRequest, error) {
	ctx = access.WithLockRequester(ctx, nil)
	v, _ := access.FromContext(ctx)
	a, err := s.fileAccessRequest(ctx, project, "", "refusal")
	if err != nil {
		s.logger.Debug("access request not filed", "project", project, "error", err)
		// A process no approval may name (approval.AnchorError): the
		// refusal says why nothing was filed, and what to do instead.
		if unfiled, ok := unfiledRequest(err); ok {
			return unfiled, nil
		}
		return access.LockRequest{}, err
	}
	// The call's refusal will name this request: the approval dialog may
	// offer it (elicit.go).
	noteDialogTarget(ctx, dialogTarget{ID: a.ID, Short: approval.ShortID(a.ID), Project: a.Project})
	ways := callWays(ctx, s.cfg.Approvals.Ways(ctx, a.ID))
	out := access.LockRequest{
		ID: a.ID, Short: approval.ShortID(a.ID), Refresh: a.Kind == store.AccessKindRefresh, Page: ways.Page, TouchID: ways.TouchID,
	}
	if !out.Refresh {
		// A lease this tree held on the project that has since ended: the
		// refusal says when, so a lapsed approval does not read like none.
		ended, ok, err := store.LastAccessLeaseEnd(ctx, s.cfg.DB, chainKeys(v), project, s.now())
		if err != nil {
			s.logger.Debug("access: last lease end", "project", project, "error", err)
		} else if ok {
			out.Ended = ended
		}
	}
	return out, nil
}

// fileAccessRequest files (or finds) the pending request for a locked project
// on behalf of the call in ctx, through the approval service's one path: the
// session is the call's binding, else the connection's own session in a
// locked project (lockedBinding) -- the one the SessionStart hook recorded
// there -- and the anchor checks are the service's. via says how it was filed
// ("refusal" or "request"); reason is the agent's own words. ctx must carry no
// lock requester (resolving the session may meet a refusal, which must not
// file again).
func (s *Server) fileAccessRequest(ctx context.Context, project, reason, via string) (store.AccessApproval, error) {
	v, _ := access.FromContext(ctx)
	sessionID := s.boundSession(ctx)
	if sessionID == "" {
		if b, ok := s.lockedBinding(ctx); ok {
			sessionID = b.sessionID
		}
	}
	return s.cfg.Approvals.Request(ctx, approval.RequestInput{
		Project: project, View: v, SessionID: sessionID, AgentProcess: callerAgentProcess(ctx),
		Host: s.callerHost(ctx), Reason: reason, Via: via,
	})
}

// unfiledRequest is the lock refusal's account of an anchor the approval
// service refused to file for (approval.AnchorError): why, and what to do.
func unfiledRequest(err error) (access.LockRequest, bool) {
	var anchor *approval.AnchorError
	if !errors.As(err, &anchor) {
		return access.LockRequest{}, false
	}
	return access.LockRequest{Unfiled: anchor.Rule(), UnfiledRemedy: anchor.Remedy()}, true
}

// chainKeys are the lease keys of the view's kernel-verified process chain.
func chainKeys(v access.View) []string {
	chain := v.Principal().Chain
	keys := make([]string, 0, len(chain))
	for _, t := range chain {
		if k := t.Key(); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// lockedBinding returns the binding a call would have but for a lock: the
// session= handle, the explicit session_start binding or the agent's own
// process-bound session, when that binding lies in a project this connection
// is locked out of (access.View.Locked). It reads the same resolution
// getBinding does, so when it answers, getBinding found nothing: a locked
// project is never Visible. It is false with no View (peer_auth off, without
// a lookup) and for every binding that is not locked, so it never changes an
// unlocked call.
func (s *Server) lockedBinding(ctx context.Context) (binding, bool) {
	if _, ok := access.FromContext(ctx); !ok {
		return binding{}, false
	}
	b, ok := s.resolveBinding(ctx)
	if !ok || !access.Locked(ctx, b.project) {
		return binding{}, false
	}
	return b, true
}

// lockedScopeRefusal is the refusal for a call whose scope or session is a
// locked project it did not name: the access layer's lock refusal (which files
// or finds the request through the planted requester), recorded like any
// fence refusal.
func (s *Server) lockedScopeRefusal(ctx context.Context, project string) error {
	err := &fenceRefusal{reason: screening.RefusedPeerUnreach, err: access.Refusal(ctx, project)}
	s.noteRefusal(ctx, err, project)
	return err
}

// lockedMayHold is the recall notice for a locked project in a recall's scope:
// its name only, never a count of what it holds (owner decision Q7).
func lockedMayHold(project string) string {
	return project + " (locked) may hold results"
}

// revokeSessionLeases ends a finished session's leases on locked projects.
// A failure is logged, never the session_end's: the session is over either
// way, and its agent process -- the lease's anchor -- is going with it.
func (s *Server) revokeSessionLeases(ctx context.Context, sessionID string) {
	if s.cfg.Approvals == nil || sessionID == "" {
		return
	}
	if _, err := s.cfg.Approvals.RevokeSession(ctx, sessionID); err != nil {
		s.logger.Warn("session_end: revoke the session's leases", "session_id", sessionID, "error", err)
	}
}

// leaseNoticeMiddleware appends the lease notices to a tool result of a
// connection that holds a lease (only ever while a project is locked):
//
//   - the project's briefing, on the first result after a lease lands. "Once"
//     is an in-memory set of lease ids on this server (leaseSeen): a lease is
//     claimed under the lock before its briefing renders, so of two concurrent
//     calls exactly one carries it. Every lease is new to it, a refresh
//     included: a refresh may follow a /clear in the same agent process, whose
//     new session has no briefing, so "same process, same project" cannot
//     tell a renewal from a fresh start. The set is not persisted: after a
//     daemon restart a live lease's next result carries the briefing once
//     more, a harmless repeat, where persisting it would need a schema or an
//     event kind for a convenience.
//   - from access.ExpiryWarning before a lease ends, the expiry notice.
//
// The notices are separate text items after the result's own, so a client
// parsing the first item as JSON reads exactly what it did before. It runs
// outside logMiddleware: the briefing is the locked project's content, and the
// tool.call event is filed under whatever project the call was bound to.
func (s *Server) leaseNoticeMiddleware(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := next(ctx, req)
		if s.cfg.Approvals == nil || err != nil || res == nil {
			return res, err
		}
		// A session_end that went through just revoked the session's leases:
		// nothing to brief or warn about any more.
		if req.Params.Name == "session_end" && !res.IsError {
			return res, err
		}
		v, ok := access.FromContext(ctx)
		if !ok {
			return res, err
		}
		leases := v.Leases()
		if len(leases) == 0 {
			return res, err
		}
		now := s.now()
		var notes []string
		for _, l := range leases {
			if text := s.leaseNews(ctx, l, now); text != "" {
				notes = append(notes, text)
			}
		}
		if text := access.ExpiryNotice(leases, now); text != "" {
			notes = append(notes, text)
		}
		for _, n := range notes {
			res.Content = append(res.Content, mcp.NewTextContent(n))
		}
		return res, err
	}
}

// leaseNews is what a result says about a lease the first time this server
// sees it: that access was approved, until when, and the project's briefing.
// "" when the lease was seen already.
func (s *Server) leaseNews(ctx context.Context, l access.Lease, now time.Time) string {
	if !s.claimLease(l, now) {
		return ""
	}
	head := "Access to " + l.Project + " was approved until " + access.Clock(l.Expires) +
		". This is its briefing, sent once:"
	briefing, _, err := s.cfg.Retrieve.ProjectBriefing(access.WithLockRequester(ctx, nil), l.Project,
		retrieve.BriefingInput{Host: s.callerHost(ctx), Source: "approval"})
	if err != nil {
		// Not sent: let the next result try again rather than claim a
		// briefing nobody received.
		s.forgetLease(l.ID)
		s.logger.Warn("mcp: briefing after approval", "project", l.Project, "error", err)
		return ""
	}
	if strings.TrimSpace(briefing) == "" {
		return "Access to " + l.Project + " was approved until " + access.Clock(l.Expires) + "."
	}
	return head + "\n" + briefing
}

// claimLease records l as seen and reports whether this call is the first to
// see it. Entries for leases that have ended are dropped on the way, so the
// set holds at most the live leases.
func (s *Server) claimLease(l access.Lease, now time.Time) bool {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if _, seen := s.leaseSeen[l.ID]; seen {
		return false
	}
	for id, expires := range s.leaseSeen {
		if !expires.After(now) {
			delete(s.leaseSeen, id)
		}
	}
	s.leaseSeen[l.ID] = l.Expires
	return true
}

// forgetLease drops a claim whose briefing could not be sent.
func (s *Server) forgetLease(id string) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	delete(s.leaseSeen, id)
}
