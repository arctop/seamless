//go:build unix

package main

// resolveWindowsService is Windows' alone: serviceControl and
// serviceTeardown read it only there.
func resolveWindowsService() windowsService { return windowsService{} }
