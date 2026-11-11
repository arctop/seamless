//go:build windows

package backup

import (
	"errors"

	"golang.org/x/sys/windows"
)

// canWrite has no stat-only answer on Windows: the ACL decides at the write,
// so the probe a save runs is what tells.
func canWrite(string) error { return nil }

// lstatFacts is peer_auth's, which runs on macOS and Linux only.
func lstatFacts(path string) (FileFacts, error) {
	return FileFacts{}, errors.New(path + ": file ownership is not checked on Windows")
}

// sameDevice is never asked on Windows: a missing drive letter is the
// disconnected disk there, and a drive root is always a mount point.
func sameDevice(string, string) (bool, error) { return false, nil }

func isReadOnlyFS(err error) bool { return errors.Is(err, windows.ERROR_WRITE_PROTECT) }

func isNotDir(err error) bool { return errors.Is(err, windows.ERROR_DIRECTORY) }

// syncDir has no Windows equivalent: NTFS journals the rename itself.
func syncDir(string) {}
