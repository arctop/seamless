package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func coveredTestOptions(path string) InstallOptions {
	return InstallOptions{
		SettingsPath: path, BaseURL: socketBase,
		SeamBin: "/opt/seamless/bin/seam", ConfigPath: "/Users/owner/.config/seamless/seamless.yaml",
	}
}

func readCoveredSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(raw, &settings))
	return settings
}

// An event the managed settings already run is never written, and the copy an
// earlier install left is removed -- the foreign hook beside it stays, an
// emptied event array goes, and every other event is installed as usual.
func TestInstallCoveredEventsRemoveOwnedCopiesAndKeepForeignHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	opts := coveredTestOptions(path)
	_, err := Install(opts)
	require.NoError(t, err)

	settings := readCoveredSettings(t, path)
	hooksObj := settings["hooks"].(map[string]any)
	foreign := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/usr/local/bin/email-guard"}}}
	hooksObj["SessionStart"] = append(hooksObj["SessionStart"].([]any), foreign)
	settings["theme"] = "dark"
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	opts.Covered = []string{"SessionStart", "UserPromptSubmit"}
	res, err := Install(opts)
	require.NoError(t, err)
	require.True(t, res.Changed)
	require.Contains(t, res.Actions, "SessionStart: removed")
	require.Contains(t, res.Actions, "UserPromptSubmit: removed")
	require.Contains(t, res.Actions, "SessionEnd: unchanged")

	settings = readCoveredSettings(t, path)
	require.Equal(t, "dark", settings["theme"])
	hooksObj = settings["hooks"].(map[string]any)
	require.Equal(t, []any{foreign}, hooksObj["SessionStart"], "only the foreign hook remains")
	require.NotContains(t, hooksObj, "UserPromptSubmit", "an emptied event array is dropped")

	status, err := InstalledStatus(opts)
	require.NoError(t, err)
	require.Equal(t, []string{"SessionEnd", "PostToolUse", "SubagentStart", "SubagentStop", "PermissionRequest"}, status.Current)

	// Idempotent: the covered events now report managed and nothing is written.
	res, err = Install(opts)
	require.NoError(t, err)
	require.False(t, res.Changed)
	require.Contains(t, res.Actions, "SessionStart: managed")
	require.Contains(t, res.Actions, "UserPromptSubmit: managed")
}

// With every event covered, a missing file is not created, and a file holding
// only Seamless hooks loses its hooks key while keeping everything else.
func TestInstallAllCoveredWritesNothingAndEmptiesHooks(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent", "settings.json")
	opts := coveredTestOptions(missing)
	opts.Covered = installedEvents(t, ClientClaudeCode)
	res, err := Install(opts)
	require.NoError(t, err)
	require.False(t, res.Changed)
	require.Len(t, res.Actions, len(opts.Covered))
	for _, action := range res.Actions {
		require.Contains(t, action, ": managed")
	}
	require.NoFileExists(t, missing)

	path := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"model":"opus"}`), 0o600))
	full := coveredTestOptions(path)
	_, err = Install(full)
	require.NoError(t, err)
	full.Covered = opts.Covered
	res, err = Install(full)
	require.NoError(t, err)
	require.True(t, res.Changed)
	settings := readCoveredSettings(t, path)
	require.Equal(t, map[string]any{"model": "opus"}, settings)
}

// A covered name outside the profile would skip nothing and leave the
// duplicate it was meant to prevent, so it is refused before any write.
func TestInstallRefusesUnknownCoveredEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	opts := coveredTestOptions(path)
	opts.Covered = []string{"SessionStrat"}
	_, err := Install(opts)
	require.ErrorContains(t, err, `covered event "SessionStrat" is not in the client's profile`)
	require.NoFileExists(t, path)

	opts.Client = ClientCodex
	opts.Covered = []string{"PermissionRequest"}
	_, err = Install(opts)
	require.ErrorContains(t, err, "not in the client's profile")
}
