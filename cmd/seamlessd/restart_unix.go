//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// reexecSelf replaces this process with a fresh copy of the same command line.
// The pid does not change, so launchd and systemd see one uninterrupted service,
// and a daemon started by hand restarts just the same.
func reexecSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("seamlessd.restart: %w", err)
	}
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil { //nolint:gosec // our own binary and argv
		return fmt.Errorf("seamlessd.restart: exec %s: %w", exe, err)
	}
	return nil
}
