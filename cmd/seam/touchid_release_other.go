//go:build !darwin

package main

import (
	"errors"

	"github.com/arctop/seamless/internal/update"
)

// releaseVerifier refuses every release asset: the Touch ID approver is macOS
// only (seam touchid stops at errTouchIDNotMac before it fetches anything), so
// no other seam links the Sigstore verifier.
func releaseVerifier() func(bundle, artifact []byte, v update.Version) error {
	return func([]byte, []byte, update.Version) error {
		return errors.New("this seam carries no release signature verifier: the Touch ID approver exists only on macOS")
	}
}
