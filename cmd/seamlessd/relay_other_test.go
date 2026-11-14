//go:build !darwin && !linux

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/relay"
)

// The relay runs on macOS and Linux only: here `seamlessd relay` refuses with
// the one line that says so, and doctor reports no relay.
func TestRelayCmd_RunsOnMacOSAndLinuxOnly(t *testing.T) {
	for _, args := range [][]string{nil, {"serve"}, {"pair", "--server", "https://example-server:8081", "--code", "AAAA"}} {
		err := runRelay(args)
		require.ErrorIs(t, err, relay.ErrUnsupported, "%v", args)
		require.Contains(t, err.Error(), "macOS and Linux only")
	}
	require.Nil(t, relayDoctorChecks(context.Background(), defaultRelayDoctorEnv()))
}
