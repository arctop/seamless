package main

import (
	"os"
	"testing"
)

// TestMain keeps every test in this package off the machine's real Claude Code
// managed settings. A developer machine can carry them (an agent guard writes
// /Library/Application Support/ClaudeCode/managed-settings.json), and doctor and
// install-hooks read them, so without this a hooks test would judge or skip
// events by what happens to be installed on the host. A test that wants managed
// settings points claudeManagedSettingsDir at a temp dir (useManagedSettingsDir).
func TestMain(m *testing.M) {
	claudeManagedSettingsDir = func() string { return "" }
	os.Exit(m.Run())
}
