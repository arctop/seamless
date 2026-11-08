//go:build darwin

package main

import (
	"github.com/arctop/seamless/internal/releasesig"
	"github.com/arctop/seamless/internal/update"
)

// releaseVerifier is the updater's Sigstore check (internal/releasesig). Only
// the macOS seam links it: it is about 17 MB, and it buys seam touchid enroll
// the proof an update gets, that the approver's checksums.txt came out of this
// repository's release workflow on this seam's own tag.
func releaseVerifier() func(bundle, artifact []byte, v update.Version) error {
	return releasesig.Embedded()
}
