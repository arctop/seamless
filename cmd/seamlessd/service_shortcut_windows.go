//go:build windows

package main

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"

	"github.com/arctop/seamless/internal/config"
)

// startupShortcutName is the fallback shortcut's file in the Startup folder
// (install.ps1's Register-ServiceFallback).
const startupShortcutName = "Seamless.lnk"

// resolveWindowsService finds what runs this user's daemon: this user's
// tasks, this install's Startup shortcut, and the running daemon. Each part
// that cannot be read is left empty, which the verbs read as absent.
func resolveWindowsService() windowsService {
	var w windowsService
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		w.exe = exe
	}
	if sys, err := windows.GetSystemDirectory(); err == nil {
		w.powershell = filepath.Join(sys, "WindowsPowerShell", "v1.0", "powershell.exe")
	}
	if l, err := realTaskLookup(); err == nil {
		w.tasks = taskNames(l.own())
		w.perUser = serviceTaskName(l.sid)
	}
	if dir, err := windows.KnownFolderPath(windows.FOLDERID_Startup, 0); err == nil {
		path := filepath.Join(dir, startupShortcutName)
		if raw, rerr := os.ReadFile(path); rerr == nil {
			if args, perr := shellLinkArguments(raw); perr == nil {
				if cfg, logFile, ok := fallbackServe(args, w.exe); ok {
					w.shortcut, w.config, w.logFile = path, cfg, logFile
				}
			}
		}
	}
	// The daemon is the data dir lock's holder (only serve takes it), when
	// that process runs this seamlessd.exe: a recycled PID, or a serve of
	// another build on the same data dir, is not this install's to stop.
	if cfg, err := config.Load(); err == nil {
		if held, pid := dataDirLockHolder(cfg.DataDir); held && pid > 0 && pid != os.Getpid() {
			if image, ierr := processImagePath(pid); ierr == nil && w.exe != "" && sameFile(image, w.exe) {
				w.pid = pid
			}
		}
	}
	return w
}

// processImagePath is the full path of the executable process pid runs.
func processImagePath(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer func() {
		_ = windows.CloseHandle(h) //nolint:errcheck // a query handle; nothing was written through it
	}()
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}
