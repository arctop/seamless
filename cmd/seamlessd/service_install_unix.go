//go:build unix

package main

// realServiceTaskEnv is Windows' alone: runInstallService refuses every
// other OS before it asks.
func realServiceTaskEnv() (serviceTaskEnv, error) { return serviceTaskEnv{}, errWindowsOnly }
