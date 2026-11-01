//go:build !windows

package feed

import (
	"errors"
	"os"
	"syscall"
)

// ownedByUs reports whether this process's user owns the file.
func ownedByUs(fi os.FileInfo) bool {
	sys, ok := fi.Sys().(*syscall.Stat_t)
	return !ok || int(sys.Uid) == os.Getuid()
}

// processAlive reports whether a process with this pid exists (signal 0
// probes without delivering anything; EPERM means it exists under another
// user).
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
