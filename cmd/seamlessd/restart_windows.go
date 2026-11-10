//go:build windows

package main

import "errors"

// reexecSelf has no Windows form: only a console lock asks for a restart, and
// peer_auth, which those need, is refused on Windows.
func reexecSelf() error {
	return errors.New("seamlessd.restart: a restart in place is not available on Windows")
}
