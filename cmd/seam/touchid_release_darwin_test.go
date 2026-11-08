//go:build darwin

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The macOS seam carries the real verifier: a bundle that is not one is
// refused by Sigstore's parser, not waved through.
func TestTouchIDReleaseVerifier_IsSigstore(t *testing.T) {
	verify := releaseVerifier()
	for _, junk := range []string{"", "{", "not json"} {
		require.ErrorContains(t, verify([]byte(junk), []byte("checksums"), fakeRelease), "parse sigstore bundle")
	}
}
