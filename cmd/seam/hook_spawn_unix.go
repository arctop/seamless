//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// spawnDetached starts exe with args as a process of its own, detached from
// the hook that starts it: a new session (so the agent client killing the
// hook's process group at its timeout does not reach it), its stdio the null
// device, never waited for. The hook exits at once, and the child is the
// init process's to reap.
func spawnDetached(exe string, args []string) error {
	cmd := exec.Command(exe, args...) //nolint:gosec // this seam binary itself, with arguments it built
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
