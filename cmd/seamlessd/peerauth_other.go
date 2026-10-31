//go:build !darwin && !linux

package main

import "errors"

// inspectPath is unavailable where peer_auth is: preparePeerAuth refuses these
// platforms before any path is inspected.
func inspectPath(string) (fileFacts, error) {
	return fileFacts{}, errors.New("peer_auth: file ownership checks are not supported on this platform")
}
