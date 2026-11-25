//go:build windows

package termvt

import (
	"os"

	"golang.org/x/sys/windows"
)

// enable sets ENABLE_VIRTUAL_TERMINAL_PROCESSING on f's console, keeping the
// rest of its mode. A handle that is not a console has no mode, so
// GetConsoleMode fails and the answer is no.
func enable(f *os.File) bool {
	h := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
