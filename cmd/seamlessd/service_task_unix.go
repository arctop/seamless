//go:build unix

package main

// ownServiceTasks is Windows' alone: a launchd job or systemd unit is the
// user's own by where its file lives.
func ownServiceTasks() []string { return nil }

// serviceTaskForControl is Windows' alone; serviceControl reads it only there.
func serviceTaskForControl() string { return "" }

// onceTaskXML is Windows' alone: the install probe reads a Scheduled Task only
// there.
func onceTaskXML() func() (string, []byte, error) {
	return func() (string, []byte, error) { return legacyServiceTask, nil, errWindowsOnly }
}
