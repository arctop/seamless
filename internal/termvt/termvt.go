// Package termvt answers whether a terminal shows ANSI escape sequences as
// styling rather than printing them as text. A Unix terminal always does. A
// Windows console does only in virtual-terminal mode, and conhost does not
// turn that mode on for a program: a program has to turn it on itself.
// PowerShell 7 turns it on for its own output and back off while a native
// command runs, so seamlessd setup's dim text printed as "←[2m, as
// installed←[0m" (the console draws ESC as ←) until seamlessd asked for the
// mode itself. Imports nothing internal.
package termvt

import "os"

// Enable turns on escape-sequence processing for f where that is a mode of
// the terminal (a Windows console), and reports whether f now shows escape
// sequences as styling. Outside Windows there is no mode to turn on, so it
// returns true. On Windows it returns false when f is not a console, or when
// the console refuses the mode (Windows before 10 1511). It does not check
// that f is a terminal: a caller asks that first (term.IsTerminal), so a pipe
// or a file never gets escape codes on any OS.
//
// The mode is the console's, not this process's, and outlives it. That is
// harmless: a console in virtual-terminal mode still shows plain text as it
// did, which is why Windows Terminal leaves the mode on for everything.
func Enable(f *os.File) bool {
	return enable(f)
}
