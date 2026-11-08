package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServiceTeardown(t *testing.T) {
	home := "/home/tester"
	const updateTask = "SeamlessUpdate-S-1-5-21-1-2-3-1001"
	tests := []struct {
		name         string
		goos         string
		updateTask   string
		wantLabel    string
		wantStop     [][]string
		wantRemove   []string
		wantReload   [][]string
		wantPathEdit bool
	}{
		{
			// The darwin updater is a process in a session of its own: there
			// is no launchd job or plist of its own to remove. Both labels
			// go: a Mac not migrated since the rename has only the legacy one.
			name:      "darwin launchd, current and legacy label",
			goos:      "darwin",
			wantLabel: "launchd (com.arctop.seamless)",
			wantStop: [][]string{
				{"launchctl", "bootout", "gui/501/com.arctop.seamless"},
				{"launchctl", "bootout", "gui/501/org.thereisnospoon.seamless"},
			},
			wantRemove: []string{
				filepath.Join(home, "Library", "LaunchAgents", "com.arctop.seamless.plist"),
				filepath.Join(home, "Library", "LaunchAgents", "org.thereisnospoon.seamless.plist"),
			},
		},
		{
			// Leftover attempt units stop first, so an updater caught
			// mid-update cannot restart the service this removes.
			name:      "linux systemd --user",
			goos:      "linux",
			wantLabel: "systemd --user (seamless.service)",
			wantStop: [][]string{
				{"systemctl", "--user", "stop", "seamless-update-*.service"},
				{"systemctl", "--user", "disable", "--now", "seamless.service"},
			},
			wantRemove: []string{
				filepath.Join(home, ".config", "systemd", "user", "seamless.service"),
			},
			wantReload: [][]string{{"systemctl", "--user", "daemon-reload"}},
		},
		{
			name:       "windows scheduled task and the update task",
			goos:       "windows",
			updateTask: updateTask,
			wantLabel:  "Scheduled Task (Seamless)",
			wantStop: [][]string{
				{"schtasks", "/End", "/TN", "Seamless"},
				{"schtasks", "/Delete", "/TN", "Seamless", "/F"},
				{"schtasks", "/Delete", "/TN", updateTask, "/F"},
			},
			wantPathEdit: true,
		},
		{
			name:      "windows without the user's SID leaves the update task alone",
			goos:      "windows",
			wantLabel: "Scheduled Task (Seamless)",
			wantStop: [][]string{
				{"schtasks", "/End", "/TN", "Seamless"},
				{"schtasks", "/Delete", "/TN", "Seamless", "/F"},
			},
			wantPathEdit: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := serviceTeardown(tt.goos, home, 501, "/opt/bin", tt.updateTask)
			require.Equal(t, tt.wantLabel, p.Label)
			require.Equal(t, tt.wantStop, argvs(p.StopCmds))
			require.Equal(t, tt.wantRemove, p.RemoveFiles)
			require.Equal(t, tt.wantReload, argvs(p.ReloadCmds))
			require.Equal(t, tt.wantPathEdit, len(p.PathEdits) == 1)
		})
	}
}

// argvs flattens a slice of commands to their argv, for assertion. nil in -> nil
// out, so an absent phase compares cleanly against a nil expectation.
func argvs(cmds []*exec.Cmd) [][]string {
	if len(cmds) == 0 {
		return nil
	}
	out := make([][]string, len(cmds))
	for i, c := range cmds {
		out[i] = c.Args
	}
	return out
}

func TestWindowsPathRemoveScript(t *testing.T) {
	s := windowsPathRemoveScript(`C:\Users\me\.local\bin`)
	require.Contains(t, s, `$d='C:\Users\me\.local\bin'`)
	require.Contains(t, s, "GetEnvironmentVariable('Path','User')")
	require.Contains(t, s, "SetEnvironmentVariable('Path',$n,'User')")
	// A single quote in the path is escaped so it cannot break the script.
	require.Contains(t, windowsPathRemoveScript(`C:\a'b`), `$d='C:\a''b'`)
}

func TestBinFileNames(t *testing.T) {
	require.Equal(t, []string{"seamlessd.exe", "seam.exe"}, binFileNames("windows"))
	require.Equal(t, []string{"seamlessd", "seam"}, binFileNames("darwin"))
	require.Equal(t, []string{"seamlessd", "seam"}, binFileNames("linux"))
}

func TestMCPArgs(t *testing.T) {
	require.Equal(t, []string{"mcp", "get", "seamless"}, mcpGetArgs())
	require.Equal(t, []string{"mcp", "remove", "seamless"}, mcpRemoveArgs())
}

func TestResolveInstallDir(t *testing.T) {
	t.Setenv("SEAMLESS_INSTALL_DIR", "")
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	// Explicit flag wins and expands ~.
	require.Equal(t, "/custom/bin", resolveInstallDir("/custom/bin"))
	require.Equal(t, filepath.Join(home, "x", "bin"), resolveInstallDir("~/x/bin"))

	// Env is next.
	t.Setenv("SEAMLESS_INSTALL_DIR", "/env/bin")
	require.Equal(t, "/env/bin", resolveInstallDir(""))

	// Default is ~/.local/bin.
	t.Setenv("SEAMLESS_INSTALL_DIR", "")
	require.Equal(t, filepath.Join(home, ".local", "bin"), resolveInstallDir(""))
}

// --purge deletes the markdown corpus, so the preamble points at the archive
// command above the confirmation. Without --purge there is nothing to warn
// about and the line must not appear.
func TestPrintUninstallPreamble_PurgeOffersAnExportFirst(t *testing.T) {
	withPurge := capturePreamble(t, "/tmp/data", true)
	require.Contains(t, withPurge, purgeExportAdvice)
	require.Contains(t, withPurge, "seamlessd export")

	require.NotContains(t, capturePreamble(t, "/tmp/data", false), "seamlessd export")
}

// A client install owns no data dir, so it is neither listed as a purge target
// nor offered the export advice: there is no corpus here to lose, and the
// ~/.seamless a client's config resolves to may be a SERVER's on a box that once
// ran one. Promising to delete it would be both a lie and a data-loss hazard.
func TestPrintUninstallPreamble_ClientHasNoDataDir(t *testing.T) {
	purge := capturePreamble(t, "", true)
	require.Contains(t, purge, tildePath("/tmp/cfg"))
	require.NotContains(t, purge, "/tmp/data")
	require.NotContains(t, purge, purgeExportAdvice,
		"there is no local corpus to export before deleting")

	keep := capturePreamble(t, "", false)
	require.Contains(t, keep, tildePath("/tmp/cfg"))
	require.NotContains(t, keep, "/tmp/data")
}

// capturePreamble returns what printUninstallPreamble printed. An empty dataDir
// is the client role: no data dir in scope.
func capturePreamble(t *testing.T, dataDir string, purge bool) string {
	t.Helper()
	return captureStdout(t, func() error {
		printUninstallPreamble([]string{"claude"}, "/opt/bin", "/tmp/cfg", dataDir, purge, false)
		return nil
	})
}

// purgePaths is the single derivation the preamble, the purge and the "kept"
// footer all read, so a client can never be told it will delete a directory the
// purge then leaves alone.
func TestPurgePaths(t *testing.T) {
	require.Equal(t, []string{"/tmp/cfg", "/tmp/data"}, purgePaths("/tmp/cfg", "/tmp/data"))
	require.Equal(t, []string{"/tmp/cfg"}, purgePaths("/tmp/cfg", ""), "client role")
	require.Equal(t, []string{"/tmp/cfg"}, purgePaths("/tmp/cfg", "   "))
}

// purgeData follows purgePaths: with no data dir in scope the block reports the
// config dir and nothing else, and the data dir is left on disk.
func TestPurgeData_ClientTouchesOnlyTheConfigDir(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataDir := filepath.Join(root, "data")
	for _, d := range []string{configDir, dataDir} {
		require.NoError(t, os.MkdirAll(d, 0o700))
	}

	out := captureStdout(t, func() error {
		purgeData(configDir, "", false)
		return nil
	})

	require.NoDirExists(t, configDir)
	require.DirExists(t, dataDir, "a client uninstall must not delete a corpus it does not own")
	require.NotContains(t, out, dataDir)
}

// A client registers no launchd job, systemd unit or Scheduled Task. Running the
// teardown anyway is not a harmless no-op on the one box where it matters: a
// machine hosting both a server and a second user's client would have its
// running daemon stopped and deregistered by that client's uninstall.
func TestUninstallService_ClientReportsNotInstalled(t *testing.T) {
	client := captureStdout(t, func() error {
		uninstallService(true, "/opt/bin", false)
		return nil
	})
	require.Contains(t, client, "not installed")
	require.Contains(t, client, "role: client")
	for _, cmd := range []string{"launchctl", "systemctl", "schtasks"} {
		require.NotContains(t, client, cmd, "no service manager is named, let alone run")
	}

	// The server role still describes the real teardown (dry run, so nothing
	// is executed), which is what keeps the assertion above from passing on a
	// function that went silent for everyone.
	server := captureStdout(t, func() error {
		uninstallService(false, "/opt/bin", true)
		return nil
	})
	require.Contains(t, server, "would run")
	require.NotContains(t, server, "not installed")
}

func TestPurgeGuard(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	for _, bad := range []string{"", "   ", "/", ".", "~", home} {
		require.Error(t, purgeGuard(bad), "purgeGuard(%q) must refuse", bad)
	}
	require.NoError(t, purgeGuard(filepath.Join(home, ".seamless")))
	require.NoError(t, purgeGuard("/opt/seamless-data"))
}

func TestRemovedEvents(t *testing.T) {
	got := removedEvents([]string{"SessionStart: removed", "SessionEnd: absent", "PostToolUse: removed"})
	require.Equal(t, []string{"SessionStart", "PostToolUse"}, got)
	require.Empty(t, removedEvents([]string{"SessionStart: absent"}))
}

// removeBinary unlinks an existing file and is a no-op when it is already gone.
func TestRemoveBinary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seamlessd")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o755))
	require.NoError(t, removeBinary(path))
	_, err := os.Stat(path)
	require.True(t, os.IsNotExist(err))
	// Second call (already gone) is a clean no-op.
	require.NoError(t, removeBinary(path))
}
