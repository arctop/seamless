package mcpversion

import (
	"slices"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// TestClient_IsPinnedToTheLastHandshakeRevision pins the version Seamless's
// clients send. Changing it changes what a new seam asks of every daemon
// already installed, so it moves only on purpose: 2025-11-25 is served by every
// released seamlessd (mcp-go v0.58.0 and v1.1.1 alike).
func TestClient_IsPinnedToTheLastHandshakeRevision(t *testing.T) {
	require.Equal(t, "2025-11-25", Client)
	require.False(t, mcp.IsModernProtocol(Client),
		"a stateless revision makes mcp-go's client probe server/discover, which the default server refuses")
	require.True(t, mcp.IsValidProtocolVersion(Client))
}

// TestServed_IncludesTheClientVersionAndNoStatelessRevision keeps the server's
// default list and the clients' version together: a client pinned to a
// version its own server does not serve cannot connect.
func TestServed_IncludesTheClientVersionAndNoStatelessRevision(t *testing.T) {
	served := Served()
	require.Contains(t, served, Client)
	require.Equal(t, Client, served[0], "the client asks for the newest version the server serves")
	require.False(t, slices.ContainsFunc(served, mcp.IsModernProtocol), "served: %v", served)
}
