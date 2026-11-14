//go:build !darwin && !linux

package relay

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// No relay runs here: nothing is inspected, and a layout check names why.
func TestRelay_UnsupportedHere(t *testing.T) {
	require.False(t, Supported)
	_, err := HostEnv().Inspect(DefaultDataDir)
	require.ErrorIs(t, err, ErrUnsupported)
}
