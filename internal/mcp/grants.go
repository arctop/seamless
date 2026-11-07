package mcp

// Read grants at the tool surface (isolation.requests, off by default; Config
// .Grants set). Off, nothing here runs: no tool call carries grants, the fence
// refuses word for word as before, and recall's scope is the old one.
//
// On:
//
//   - every tool call carries the grants of whoever it speaks for
//     (grantsMiddleware): the kernel-verified process chain of the owner's
//     socket connection under peer_auth, else the session the connection is
//     bound to (getBinding -- identification, never ambient inference). The
//     access layer judges the rows (access.JudgeGrants) and store.CanRead /
//     CanWrite ask them, so every fence inherits them: a live grant opens one
//     confidential project, read-only, to the one project it was approved
//     from, and every grant a holder was ever approved confines its writes to
//     the projects it was approved from;
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

// loadGrants judges the grants of the holder this call speaks for: the owner's
// socket process chain under peer_auth (a TCP request or a configured account
// holds none), else the bound session.
func (s *Server) loadGrants(ctx context.Context) (access.Grants, error) {
	v, hasView := access.FromContext(ctx)
	var keys []string
	sessionID := ""
	if hasView {
		if v.Principal().Kind != access.KindOwner || !v.Principal().HasProcess {
			return access.Grants{}, nil
		}
		if keys = chainKeys(v); len(keys) == 0 {
			return access.Grants{}, nil
		}
	} else {
		b, ok := s.getBinding(ctx)
		if !ok || b.sessionID == "" {
			return access.Grants{}, nil
		}
		sessionID = b.sessionID
	}
	rows, err := store.AccessGrantsForHolder(ctx, s.cfg.DB, keys, sessionID)
	if err != nil {
		return access.Grants{}, err
	}
	if len(rows) == 0 {
		return access.Grants{}, nil
	}
	return access.JudgeGrants(ctx, sessionID, s.now(), store.AccessGrants(rows)), nil
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
		AgentProcess: callerAgentProcess(ctx), Host: s.callerHost(ctx), Via: "refusal",
	})
	if rerr != nil {
		s.logger.Debug("read grant request not filed", "project", target, "error", rerr)
		return err
	}
	// The refusal names this request: the approval dialog may offer it
	// (elicit.go; only ever on the peer_auth socket).
	noteDialogTarget(ctx, dialogTarget{ID: a.ID, Short: approval.ShortID(a.ID), Project: a.Project, Grant: true})
	confines := false
	if iso, ierr := store.IsolationOf(ctx, s.cfg.DB, b.project); ierr == nil && iso == core.IsolationOpen {
		confines = true
	}
	return &fenceRefusal{reason: f.reason, err: s.grantRefusal(f.err, a, confines)}
}

// grantRefusal is the fence's refusal with its remedy line: the request, how
// the owner approves it, and what an approval brings with it.
func (s *Server) grantRefusal(rule error, a store.AccessApproval, confines bool) error {
	short := approval.ShortID(a.ID)
	how := fmt.Sprintf("the owner approves it in their own terminal with `seam approve %s`", short)
	switch page := s.cfg.Grants.Page(a.ID); {
	case page != "":
		how += ", or with a passkey at " + page
	case s.cfg.Grants.Advisory():
		how += ", or with Approve on the console's Access requests page"
	}
	brings := "An approved read is read-only, and " + approval.CopyIntoLine(a.Project, a.FromProject) + " and nowhere else."
	if confines {
		brings = fmt.Sprintf("An approved read is read-only, and confines this %s's writes to %s for the rest of its life.",
			s.holderWord(), a.FromProject)
	}
	return fmt.Errorf("%w\nRemedy: read request %s is waiting -- %s; retry once it is approved. %s", rule, short, how, brings)
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
// sees it: what was approved, until when, and the confinement that came with
// it.
func (s *Server) grantNews(ctx context.Context, l access.Grant) string {
	head := fmt.Sprintf("Read access to %s was approved until %s, read-only. %s, and nowhere else.",
		l.Project, access.Clock(l.Expires), capitalize(approval.CopyIntoLine(l.Project, l.From)))
	iso, err := store.IsolationOf(ctx, s.cfg.DB, l.From)
	if err != nil || iso != core.IsolationOpen {
		return head
	}
	return head + fmt.Sprintf(" This %s now writes only into %s, for the rest of its life: a write to another project "+
		"or the global scope is refused.", s.holderWord(), l.From)
}

// confinedRefusal is the refusal of a write a read grant's confinement
// refuses: the rule, and where the holder may still write.
func (s *Server) confinedRefusal(ctx context.Context, target string) error {
	g, _ := access.GrantsFrom(ctx)
	confined, _ := g.Confined()
	where := "project " + target
	if target == "" {
		where = "the global scope"
	}
	return fmt.Errorf("this %s was granted a read of another project, so it writes only into %s for the rest of its life: "+
		"a write into %s is refused", s.holderWord(), strings.Join(confined, ", "), where)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// claimGrant records a grant as announced and reports whether this call is
// the first to see it. As with leases (claimLease) the set is in memory: a
// daemon restart announces a live grant once more, a harmless repeat.
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
