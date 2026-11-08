//go:build !darwin

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Other platforms link no verifier, and refuse every asset.
func TestTouchIDReleaseVerifier_RefusesOffMacOS(t *testing.T) {
	require.ErrorContains(t, releaseVerifier()([]byte("{}"), []byte("checksums"), fakeRelease), "only on macOS")
}
