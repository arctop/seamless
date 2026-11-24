package main

import "strings"

// A Scheduled Task runs the console seamlessd.exe, and a console program
// started with no console to share gets one of its own: an InteractiveToken
// task opens a window (conhost, or Windows Terminal where it is the default)
// on the signed-in desktop at every logon, and closing that window ends the
// daemon. So serve gives that console back (detachTaskConsole) before it
// does anything else: the window closes, and the daemon is out of its reach.
// Its output goes on to the log file, where the task's serve already wrote
// (--log-file, applyServeEnv), and a Go panic, which the runtime writes to the
// standard error handle, lands there too.
//
// A console serve shares with another program -- a shell the owner started it
// from by hand -- stays, and so does one whose parent is not the Task
// Scheduler: a serve started by double-click owns its console too, and its
// owner is looking at it. Without a console a sign-out ends the daemon
// without the console's logoff signal, the hard stop schtasks /End has always
// given it: SQLite's WAL and the atomic file writes are built for that.

// ownsTaskConsole reports whether serve should give its console back: the
// Task Scheduler started it (parent, the parent's image name) and no other
// process shares its console (consoleProcs, GetConsoleProcessList's count).
func ownsTaskConsole(parent string, consoleProcs int) bool {
	return consoleProcs == 1 && strings.EqualFold(parent, taskSchedulerImage)
}
