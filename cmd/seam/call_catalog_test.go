package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	// Test-only, as in identity_test.go: the seam BINARY must not import
	// internal/mcp (it drags in internal/store and the SQLite driver), but the
	// test binary may, which is what lets the CLI's literal tool list be pinned
	// against the server's catalog instead of trusted.
	"github.com/arctop/seamless/internal/mcp"
)

// callTools is the list `seam call --help` prints. It is transcribed rather than
// imported, so it is compared here, in order, with the catalog the server
// registers from: a tool added to the server and not to the help fails this
// rather than going unadvertised. The catalog is every tool any server can
// register, so the list includes the ones only a server that locks a project
// registers (mcp.LockedOnlyTools), last, as the help says.
func TestCallTools_MatchTheCatalog(t *testing.T) {
	catalog := mcp.Catalog()
	names := make([]string, 0, len(catalog))
	for _, tool := range catalog {
		names = append(names, tool.Name)
	}
	require.Equal(t, names, callTools)
	require.Len(t, callTools, mcp.LockedToolCount)
	require.Equal(t, mcp.LockedOnlyTools(), callTools[mcp.ToolCount:])
}

// seam doctor mirrors the server's tool counts without importing its package
// at run time; this pins the mirror (the test binary may import it).
func TestDoctorMirrorsTheServersToolCounts(t *testing.T) {
	require.Equal(t, mcp.ToolCount, expectedTools)
	require.Equal(t, []string{lockedOnlyTool}, mcp.LockedOnlyTools())
	require.Equal(t, mcp.LockedToolCount, expectedTools+len(mcp.LockedOnlyTools()))
}
