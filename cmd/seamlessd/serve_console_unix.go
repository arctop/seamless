//go:build unix

package main

import "os"

// detachTaskConsole is Windows' alone: a launchd or systemd service has no
// console to give back.
func detachTaskConsole() bool { return false }

// redirectStdio is Windows' alone; detachTaskConsole never asks for it here.
func redirectStdio(*os.File) {}
