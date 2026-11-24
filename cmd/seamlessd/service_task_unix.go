//go:build unix

package main

// onceTaskXML is Windows' alone: the install probe reads a Scheduled Task only
// there.
func onceTaskXML() func() (string, []byte, error) {
	return func() (string, []byte, error) { return legacyServiceTask, nil, errWindowsOnly }
}
