//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// installerInterpreter runs a release's install script from stdin: Windows
// PowerShell by absolute path under the system directory
// (%SystemRoot%\System32), never a PATH lookup, with -Command - reading the
// script from stdin, as the documented `irm ... | iex` one-liner does.
func installerInterpreter() (string, []string, error) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return "", nil, fmt.Errorf("locate the system directory: %w", err)
	}
	ps := filepath.Join(sys, "WindowsPowerShell", "v1.0", "powershell.exe")
	return ps, []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", "-"}, nil
}

// installerProcAttr gives an unattended installer no console window
// (CREATE_NO_WINDOW): the updater runs with no desktop to show one on, and a
// window flashing up on the owner's screen mid-update is worse than none. An
// attended one shares the terminal it was started from.
func installerProcAttr(unattended bool) *syscall.SysProcAttr {
	if !unattended {
		return nil
	}
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

// stopInstaller kills a running installer. Windows has no process group to
// signal here, so only PowerShell itself is stopped; a child it started (a
// download in flight) ends on its own.
func stopInstaller(p *os.Process, _ bool) error {
	return p.Kill()
}

// endDaemon is a system install's restart, which Windows has none of:
// peer_auth, and so update.KindSystem, is macOS and Linux only.
func endDaemon(pid int) error {
	return fmt.Errorf("signal pid %d: a peer_auth system daemon runs on macOS and Linux only", pid)
}

// freeBytes is the space on dir's volume available to this user.
func freeBytes(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var avail, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &totalFree); err != nil {
		return 0, fmt.Errorf("GetDiskFreeSpaceEx %s: %w", dir, err)
	}
	return avail, nil
}
