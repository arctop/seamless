package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOwnsTaskConsole: serve gives its console back only when the Task
// Scheduler started it and nothing else shares it -- never a console the owner
// started serve from, nor one a double-click opened for it.
func TestOwnsTaskConsole(t *testing.T) {
	tests := []struct {
		name   string
		parent string
		procs  int
		want   bool
	}{
		{"the task's own console", "svchost.exe", 1, true},
		{"the image name in another case", "SVCHOST.EXE", 1, true},
		{"a console shared with the shell serve was started from", "svchost.exe", 2, false},
		{"no console at all (CREATE_NO_WINDOW, or unreadable)", "svchost.exe", 0, false},
		{"started by hand from a shell", "powershell.exe", 1, false},
		{"started by double-click", "explorer.exe", 1, false},
		{"the Startup shortcut's hidden powershell", "powershell.exe", 2, false},
		{"no parent read", "", 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, ownsTaskConsole(tt.parent, tt.procs))
		})
	}
}
