package relay

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// config names this layout's socket and data directory for the processes that
// do not import this package: seam tells a relay's socket from a peer_auth
// daemon's own, which is the same path, by the relay's data directory
// (cmd/seam onClientComputer). Both must stay this layout's.
func TestDefaultLayout_IsTheOneConfigNames(t *testing.T) {
	require.Equal(t, "unix://"+DefaultSocketPath, config.RelaySocketURL)
	require.Equal(t, DefaultDataDir, config.RelayDataDir)
}
