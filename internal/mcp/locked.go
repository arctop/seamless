package mcp

// Locked projects (peer_auth.locked): a tool call refused at a lock files an
// access request automatically, and the refusal names it. The access layer
// produces every refusal (access.Refusal); this file only plants the requester
// it calls, for calls that can carry a lease at all -- the owner's socket
// connections with a kernel-verified process chain -- and only while the
// daemon locks a project (Config.Approvals set). Off, nothing here runs.

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/store"
)

// Approvals is what the MCP server needs of the daemon's approval service
// (*approval.Service).
type Approvals interface {
	Request(ctx context.Context, in approval.RequestInput) (store.AccessApproval, error)
	Page(id string) string
	RevokeSession(ctx context.Context, sessionID string) (int64, error)
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
// file again.
func (s *Server) lockRequester(ctx context.Context, project string) (access.LockRequest, error) {
	ctx = access.WithLockRequester(ctx, nil)
	v, _ := access.FromContext(ctx)
	a, err := s.cfg.Approvals.Request(ctx, approval.RequestInput{
		Project: project, View: v, SessionID: s.boundSession(ctx), AgentProcess: callerAgentProcess(ctx), Via: "refusal",
	})
	if err != nil {
		s.logger.Debug("access request not filed", "project", project, "error", err)
		return access.LockRequest{}, err
	}
	return access.LockRequest{
		ID: a.ID, Short: approval.ShortID(a.ID), Refresh: a.Kind == store.AccessKindRefresh, Page: s.cfg.Approvals.Page(a.ID),
	}, nil
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
