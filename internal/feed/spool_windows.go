//go:build windows

package feed

import "os"

// ownedByUs is always true on Windows: the spool folder inherits the data
// dir's ACL, which the installer keeps to the owner's account.
func ownedByUs(os.FileInfo) bool { return true }

// processAlive treats every recorded pid as alive: Windows offers no
// signal-0 probe, so a stale lock is removed by hand (the error names it).
func processAlive(int) bool { return true }
