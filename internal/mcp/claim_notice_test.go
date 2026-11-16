package mcp

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// The name the claim notices skip is the one the seam CLI gives at initialize.
// The CLI is package main, so its source is the only place to read it from.
func TestSeamCLIClient_IsTheNameTheCLIInitializesWith(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "cmd", "seam", "main.go"))
	require.NoError(t, err)
	require.Contains(t, string(raw), "mcp.Implementation{Name: "+strconv.Quote(seamCLIClient)+", Version:",
		"cmd/seam names its MCP client %q; the claim notices skip that client", seamCLIClient)
}
