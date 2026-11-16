package mcp

// Claim notices at the MCP edge: a session that lost a claim -- another
// session took it once its lease lapsed, the owner released it, or the reaper
// released it after the lease lapsed (store.RecordClaimNotice) -- learns it in
// the next tool result credited to it, once (store.DeliverClaimNotices). The
// hooks deliver the same notices in a prompt's or a session start's injection
// (internal/hooks/claim_notice.go); whichever reaches the session first carries
// each one.

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/arctop/seamless/internal/store"
)

// seamCLIClient is the client name the seam CLI initializes with
// (cmd/seam/main.go, dialWith). The CLI prints a result's first text item and
// nothing after it.
const seamCLIClient = "seam-cli"

// claimNoticeMiddleware appends the notices of the claims the call's session
// lost to its result, one text item each, after the handler's own. The session
// is the one the call is credited to (attribution: a session the call started
// or ended, a session= handle, the binding, the ambient fallback), which is why
// it runs just inside logMiddleware, whose slots attribution reads. The
// notices carry ids, a slug, times and at most a session name the caller may
// see (store.ClaimNoticeTexts), the kind of text the grant notices carry, so
// the tool.call event may keep them. A call from the seam CLI gets none: the
// CLI shows only a result's first text item, so a notice there would be spent
// unread, and the agent's next prompt or tool call carries it instead. With
// none pending it costs one indexed read.
func (s *Server) claimNoticeMiddleware(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := next(ctx, req)
		if err != nil || res == nil || s.cfg.DB == nil || fromSeamCLI(ctx) {
			return res, err
		}
		sessionID, _ := s.attribution(ctx)
		if sessionID == "" {
			return res, err
		}
		notices, derr := store.DeliverClaimNotices(ctx, s.cfg.DB, []string{sessionID}, s.now())
		if derr != nil {
			// Nothing was claimed, so the next result carries them.
			s.logger.Warn("mcp: claim notices", "session", sessionID, "error", derr)
			return res, err
		}
		for _, text := range store.ClaimNoticeTexts(ctx, s.cfg.DB, notices) {
			res.Content = append(res.Content, mcp.NewTextContent(text))
		}
		return res, err
	}
}

// fromSeamCLI reports whether the call's connection is the seam CLI's, by the
// name its client gave at initialize. A connection that names none -- one a
// daemon restart left without its initialize -- is not.
func fromSeamCLI(ctx context.Context) bool {
	info, ok := mcpserver.ClientSessionFromContext(ctx).(mcpserver.SessionWithClientInfo)
	return ok && info.GetClientInfo().Name == seamCLIClient
}
