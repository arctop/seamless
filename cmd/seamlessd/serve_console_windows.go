//go:build windows

package main

import (
	"log/slog"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// kernel32's console calls, which x/sys/windows does not wrap.
var (
	procGetConsoleProcessList = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")
	procFreeConsole           = windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole")
)

// detachTaskConsole gives back the console a Scheduled Task opened for this
// serve (serve_console.go) and reports whether it did. Standard output and
// error then point at NUL until applyServeEnv points them at the log file:
// their console handles closed with it.
func detachTaskConsole() bool {
	parent, err := parentImageName()
	if err != nil {
		return false
	}
	if !ownsTaskConsole(parent, consoleProcessCount()) {
		return false
	}
	if r, _, _ := procFreeConsole.Call(); r == 0 { //nolint:errcheck // r is the verdict: Call's error is GetLastError, which says nothing more here
		return false
	}
	if null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		redirectStdio(null)
	}
	return true
}

// consoleProcessCount is how many processes share this one's console, or 0
// when it has none or the count cannot be read. A list of two is enough: all
// that matters is whether there is more than one.
func consoleProcessCount() int {
	if err := procGetConsoleProcessList.Find(); err != nil {
		return 0
	}
	var pids [2]uint32
	// r is the count, 0 when there is none to read: the answer either way.
	r, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids))) //nolint:errcheck // r is the answer, 0 for none
	return int(r)
}

// redirectStdio points this process's standard output and error at f: the
// os package's files, and the process's standard handles, which the Go
// runtime writes a panic to. The default logger is rebuilt on the new
// standard error, as main made it.
func redirectStdio(f *os.File) {
	h := windows.Handle(f.Fd())
	_ = windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, h) //nolint:errcheck // best-effort: os.Stdout below still moves
	_ = windows.SetStdHandle(windows.STD_ERROR_HANDLE, h)  //nolint:errcheck // best-effort: os.Stderr below still moves
	os.Stdout, os.Stderr = f, f
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})))
}
