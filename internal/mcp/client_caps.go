package mcp

// What an MCP client declared at initialize, kept past a daemon restart.
//
// A `seam mcp-proxy` bridge outlives the daemon (an automatic update restarts
// it) and goes on presenting the Mcp-Session-Id it was given, which the transport
// accepts by shape. After a restart every POST on that connection is served by
// a new ephemeral session whose capabilities are empty: the initialize that
// declared them lived only in the old process. Without this file a running
// agent would silently lose the in-session approval dialog (elicit.go), and
// Touch ID with it, until it restarted, and nothing would say so.
//
// So the daemon keeps the elicitation modes a client declared, by session id,
// and puts them back on the connection for every tool call that needs them.
// Only the elicitation modes: nothing else a client declares, and nothing of its
// clientInfo, is ever read after initialize.
//
// Capabilities are the client's own claim and grant nothing: a caller that names
// another connection's id gets only what it could have declared itself.
//
// Off, nothing here runs: no hook is registered, and a request that did not come
// over the peer_auth socket never reaches the store.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/store"
)

// clientCapsTouchEvery is how stale a connection's last_seen_at may get before
// a call refreshes it: often enough that one in use stays far inside
// store.MCPSessionRetention, rarely enough that a tool call does not write.
const clientCapsTouchEvery = 24 * time.Hour

// keepsClientCaps reports whether this daemon can run the approval dialog at
// all: only then is what a client declared worth keeping.
func (s *Server) keepsClientCaps() bool { return s.cfg.Approvals != nil || s.cfg.Grants != nil }

// saveClientCaps is the initialize hook: it records the elicitation modes a
// client on the peer_auth socket declared. A failure is logged: the connection
// works as it is, and only a later restart would miss the dialog.
func (s *Server) saveClientCaps(ctx context.Context, _ any, req *mcp.InitializeRequest, _ *mcp.InitializeResult) {
	if !access.PeerAuthenticated(ctx) || req == nil || req.Params.Capabilities.Elicitation == nil {
		return
	}
	session := mcpserver.ClientSessionFromContext(ctx)
	if session == nil || session.SessionID() == "" {
		return
	}
	b, err := json.Marshal(mcp.ClientCapabilities{Elicitation: req.Params.Capabilities.Elicitation})
	if err != nil {
		s.logger.Warn("mcp: encode client capabilities", "error", err)
		return
	}
	if err := store.SaveMCPSession(ctx, s.cfg.DB, session.SessionID(), string(b), s.now()); err != nil {
		s.logger.Warn("mcp: save client capabilities", "error", err)
	}
}

// clientCapsMiddleware puts the capabilities the connection declared back on
// its session before a tool call reads them (formDialogCapable, callWays,
// dialog). It runs per call because after a restart each POST is a new session
// object. The session this daemon's own initialize made already holds them, and
// is left as it is; its row is still looked up, so it stays alive for the next
// restart. A failed lookup is logged and the call goes on: no dialog is the
// answer a client that declared nothing gets.
func (s *Server) clientCapsMiddleware(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if s.keepsClientCaps() && access.PeerAuthenticated(ctx) {
			s.restoreClientCaps(ctx)
		}
		return next(ctx, req)
	}
}

func (s *Server) restoreClientCaps(ctx context.Context) {
	session := mcpserver.ClientSessionFromContext(ctx)
	if session == nil || session.SessionID() == "" {
		return
	}
	info, ok := session.(mcpserver.SessionWithClientInfo)
	if !ok {
		return
	}
	raw, lastSeen, found, err := store.MCPSessionCapabilities(ctx, s.cfg.DB, session.SessionID())
	if err != nil {
		s.logger.Warn("mcp: load client capabilities", "error", err)
		return
	}
	if !found {
		return
	}
	if info.GetClientCapabilities().Elicitation == nil {
		var caps mcp.ClientCapabilities
		if err := json.Unmarshal([]byte(raw), &caps); err != nil {
			s.logger.Warn("mcp: decode client capabilities", "error", err)
			return
		}
		info.SetClientCapabilities(caps)
	}
	if now := s.now(); now.Sub(lastSeen) >= clientCapsTouchEvery {
		if err := store.TouchMCPSession(ctx, s.cfg.DB, session.SessionID(), now); err != nil {
			s.logger.Warn("mcp: touch client capabilities", "error", err)
		}
	}
}
