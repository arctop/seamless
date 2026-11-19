// Package mcpversion names the MCP protocol revisions Seamless speaks: the one
// its own clients ask for, and the ones its server serves by default. Both live
// here so the two cannot drift apart. It imports only mcp-go, so the seam CLI
// can use it without linking the server.
package mcpversion

import "github.com/mark3labs/mcp-go/mcp"

// Client is the protocol version every Seamless MCP client (seam, the
// seamlessd doctor and setup, seambench) puts in its initialize request.
//
// It is pinned to 2025-11-25, the newest revision that still has the
// initialize handshake and Mcp-Session-Id, rather than mcp-go's
// LATEST_PROTOCOL_VERSION (2026-07-28, the stateless core). A client that asks
// for a revision before 2026-07-28 skips mcp-go's server/discover probe and
// goes straight to the handshake. That matters because, since mcp-go v1.3.0,
// a client that asks for 2026-07-28 and is refused with a supported list
// returns the refusal instead of falling back to the handshake, and the
// server below refuses 2026-07-28 by default (Served). Every released
// seamlessd serves 2025-11-25: mcp-go v0.58.0's newest revision, and on mcp-go
// v1.1.1 the newest one its legacy-only transport serves.
const Client = mcp.ProtocolVersion20251125

// Served returns the protocol versions the streamable-HTTP transport serves
// unless the stateless revision is turned on (internal/mcp Config
// ServeStatelessMCP): the revisions that keep protocol-level sessions,
// newest first. Client is always one of them.
func Served() []string {
	return mcp.LegacyProtocolVersions()
}
