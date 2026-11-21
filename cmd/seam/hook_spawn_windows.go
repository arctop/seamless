//go:build windows

package main

import "errors"

// spawnDetached has no Windows form: a transcript is sent only over the
// peer_auth socket, and peer_auth serves macOS and Linux alone.
func spawnDetached(string, []string) error {
	return errors.New("a detached transcript upload is not supported on Windows")
}
