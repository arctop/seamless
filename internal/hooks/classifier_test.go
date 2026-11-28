package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func hookSpecFor(t *testing.T, client Client, event string) hookSpec {
	t.Helper()
	_, profile, err := resolveHookProfile(client)
	require.NoError(t, err)
	for _, hs := range profile {
		if hs.Event == event {
			return hs
		}
	}
	t.Fatalf("missing %s hook spec", event)
	return hookSpec{}
}

func TestClassifyHookDefinition_ExactOwnershipStates(t *testing.T) {
	hs := hookSpecFor(t, ClientCodex, "Stop")
	desired := buildEntry(ClientCodex, hs, "/opt/seam", "/etc/seamless.yaml")
	desiredURL := "http://127.0.0.1:8081/api/hooks/stop"

	currentWithoutMarker := buildEntry(ClientCodex, hs, "/opt/seam", "/etc/seamless.yaml")
	delete(currentWithoutMarker, managedMarker)
	managedStale := buildEntry(ClientCodex, hs, "/old/seam", "/old/seamless.yaml")
	legacy := map[string]any{
		"hooks": []any{map[string]any{
			"type": "command", "command": "/old/seam hook stop --config /old/seamless.yaml", "timeout": 10,
		}},
	}
	legacyWindows := map[string]any{
		"hooks": []any{map[string]any{
			"type": "command", "command": `"C:\Program Files\Seam\seam.exe" hook stop --config "C:\Users\dev\seamless.yaml"`, "timeout": 10,
		}},
	}
	legacyWindowsUNC := map[string]any{
		"hooks": []any{map[string]any{
			"type": "command", "command": `"\\server\share\seam.exe" hook stop --config "\\server\share\seamless.yaml"`, "timeout": 10,
		}},
	}
	legacyTilde := map[string]any{
		"hooks": []any{map[string]any{
			"type": "command", "command": "~/.local/bin/seam hook stop --config ~/.config/seamless/seamless.yaml --client codex", "timeout": 10,
		}},
	}
	foreignTilde := map[string]any{
		"hooks": []any{map[string]any{
			"type": "command", "command": "~/bin/guard hook stop", "timeout": 10,
		}},
	}
	foreignRelativeConfig := map[string]any{
		"hooks": []any{map[string]any{
			"type": "command", "command": "~/.local/bin/seam hook stop --config seamless.yaml", "timeout": 10,
		}},
	}
	foreignExec := map[string]any{
		"hooks": []any{map[string]any{
			"type": "command", "command": "/usr/local/bin/guard", "args": []any{"hook", "stop"}, "timeout": 10,
		}},
	}
	foreignShell := map[string]any{
		"hooks": []any{map[string]any{
			"type": "command", "command": "/usr/local/bin/guard hook stop", "timeout": 10,
		}},
	}

	tests := []struct {
		name  string
		entry any
		want  hookDefinitionClass
	}{
		{name: "marked exact current", entry: desired, want: hookDefinitionCurrent},
		{name: "marker stripped exact current", entry: currentWithoutMarker, want: hookDefinitionCurrent},
		{name: "marked drift is stale", entry: managedStale, want: hookDefinitionManagedStale},
		{name: "known marker stripped shell is legacy", entry: legacy, want: hookDefinitionLegacy},
		{name: "known Windows shell is legacy", entry: legacyWindows, want: hookDefinitionLegacy},
		{name: "known Windows UNC shell is legacy", entry: legacyWindowsUNC, want: hookDefinitionLegacy},
		{name: "hand-written tilde command is legacy", entry: legacyTilde, want: hookDefinitionLegacy},
		{name: "tilde non-seam executable is foreign", entry: foreignTilde, want: hookDefinitionForeign},
		{name: "relative config path is foreign", entry: foreignRelativeConfig, want: hookDefinitionForeign},
		{name: "foreign executable with matching args", entry: foreignExec, want: hookDefinitionForeign},
		{name: "foreign shell with matching text", entry: foreignShell, want: hookDefinitionForeign},
		{name: "wrong input type", entry: "hook stop", want: hookDefinitionForeign},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, classifyHookDefinition(ClientCodex, tt.entry, desired, hs, desiredURL))
		})
	}
}

func TestForeignHookDefinitionsSurviveInstallAndUninstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	initial := `{
  "description": "foreign definitions",
  "hooks": {
    "Stop": [
      {"hooks":[{"type":"command","command":"/usr/local/bin/guard","args":["hook","stop"],"timeout":10}]},
      {"hooks":[{"type":"command","command":"/usr/local/bin/guard hook stop","timeout":10}]}
    ]
  }
}`
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o600))

	var before map[string]any
	require.NoError(t, json.Unmarshal([]byte(initial), &before))
	beforeStop := before["hooks"].(map[string]any)["Stop"].([]any)

	res, err := Install(codexOpts(path))
	require.NoError(t, err)
	require.True(t, res.Changed)

	afterInstall := readHookSettings(t, path)
	require.Equal(t, "foreign definitions", afterInstall["description"])
	installedStop := afterInstall["hooks"].(map[string]any)["Stop"].([]any)
	require.Len(t, installedStop, 3)
	require.Equal(t, beforeStop, installedStop[:2], "install must preserve foreign definitions byte-for-byte at the JSON value level")
	require.True(t, isManaged(installedStop[2]))

	status, err := InstalledStatus(codexOpts(path))
	require.NoError(t, err)
	require.Equal(t, installedEvents(t, ClientCodex), status.Current)
	require.Empty(t, status.Stale)

	uninstall, err := Uninstall(UninstallOptions{
		Client: ClientCodex, SettingsPath: path, BaseURL: "http://127.0.0.1:8081",
	})
	require.NoError(t, err)
	require.True(t, uninstall.Changed)

	afterUninstall := readHookSettings(t, path)
	require.Equal(t, "foreign definitions", afterUninstall["description"])
	remainingStop := afterUninstall["hooks"].(map[string]any)["Stop"].([]any)
	require.Equal(t, beforeStop, remainingStop)
}

func TestInstalledStatusCodexSeparatesCurrentFromStale(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "missing client on marker stripped command",
			mutate: func(entry map[string]any) {
				delete(entry, managedMarker)
				handler := onlyHookHandler(entry)
				handler["command"] = strings.ReplaceAll(handler["command"].(string), " --client codex", "")
				handler["command_windows"] = strings.ReplaceAll(handler["command_windows"].(string), " --client codex", "")
			},
		},
		{
			name: "wrong binary",
			mutate: func(entry map[string]any) {
				handler := onlyHookHandler(entry)
				handler["command"] = strings.Replace(handler["command"].(string), "/opt/seam", "/old/seam", 1)
			},
		},
		{
			name: "wrong config",
			mutate: func(entry map[string]any) {
				handler := onlyHookHandler(entry)
				handler["command"] = strings.Replace(handler["command"].(string), "/etc/seamless.yaml", "/old/seamless.yaml", 1)
			},
		},
		{
			name: "wrong timeout",
			mutate: func(entry map[string]any) {
				onlyHookHandler(entry)["timeout"] = 99
			},
		},
		{
			name: "missing Windows command",
			mutate: func(entry map[string]any) {
				delete(onlyHookHandler(entry), "command_windows")
			},
		},
		{
			// The pre-0.4.8 emission: a leading double-quoted executable, which
			// PowerShell reads as a string expression (exit 1). It must classify
			// stale so a re-install repairs live Windows files.
			name: "legacy quoted Windows executable",
			mutate: func(entry map[string]any) {
				onlyHookHandler(entry)["command_windows"] = `"/opt/seam" hook stop --config "/etc/seamless.yaml" --client codex`
			},
		},
		{
			name: "unsupported handler type",
			mutate: func(entry map[string]any) {
				onlyHookHandler(entry)["type"] = "prompt"
			},
		},
		{
			name: "async handler",
			mutate: func(entry map[string]any) {
				onlyHookHandler(entry)["async"] = true
			},
		},
	}
	wantCurrent := make([]string, 0, len(installedEvents(t, ClientCodex))-1)
	for _, event := range installedEvents(t, ClientCodex) {
		if event != "Stop" {
			wantCurrent = append(wantCurrent, event)
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hooks.json")
			_, err := Install(codexOpts(path))
			require.NoError(t, err)

			settings := readHookSettings(t, path)
			stop := settings["hooks"].(map[string]any)["Stop"].([]any)[0].(map[string]any)
			tt.mutate(stop)
			writeHookSettings(t, path, settings)

			status, err := InstalledStatus(codexOpts(path))
			require.NoError(t, err)
			require.Equal(t, wantCurrent, status.Current)
			require.Equal(t, []string{"Stop"}, status.Stale)
			require.Equal(t, installedEvents(t, ClientCodex), status.Owned)
		})
	}
}

func TestInstallMarkerStrippedCurrentDefinitionIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	_, err := Install(codexOpts(path))
	require.NoError(t, err)

	settings := readHookSettings(t, path)
	for _, entries := range settings["hooks"].(map[string]any) {
		for _, entry := range entries.([]any) {
			delete(entry.(map[string]any), managedMarker)
		}
	}
	writeHookSettings(t, path, settings)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	res, err := Install(codexOpts(path))
	require.NoError(t, err)
	require.False(t, res.Changed)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)

	status, err := InstalledStatus(codexOpts(path))
	require.NoError(t, err)
	require.Equal(t, installedEvents(t, ClientCodex), status.Current)
	require.Empty(t, status.Stale)
}

func TestCodexWindowsPathsClassifyAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	opts := codexOpts(path)
	opts.SeamBin = `C:\Program Files\Seam\seam.exe`
	opts.ConfigPath = `C:\Users\dev\AppData\Roaming\seamless\seamless.yaml`

	_, err := Install(opts)
	require.NoError(t, err)
	status, err := InstalledStatus(opts)
	require.NoError(t, err)
	require.Equal(t, installedEvents(t, ClientCodex), status.Current)
	require.Empty(t, status.Stale)

	settings := readHookSettings(t, path)
	command := onlyHookHandler(settings["hooks"].(map[string]any)["Stop"].([]any)[0].(map[string]any))["command_windows"].(string)
	// A space forces quoting, and quoting under PowerShell requires the call
	// operator; the config path is argument position, so plain quotes suffice.
	require.True(t, strings.HasPrefix(command, `& "C:\Program Files\Seam\seam.exe" hook `), command)
	require.Contains(t, command, `"C:\Users\dev\AppData\Roaming\seamless\seamless.yaml"`)

	res, err := Uninstall(UninstallOptions{Client: ClientCodex, SettingsPath: path, BaseURL: opts.BaseURL})
	require.NoError(t, err)
	require.True(t, res.Changed)
	status, err = InstalledStatus(opts)
	require.NoError(t, err)
	require.Empty(t, status.Owned)
}

func TestInstallRejectsAmbiguousDefinitionPaths(t *testing.T) {
	base := InstallOptions{SettingsPath: filepath.Join(t.TempDir(), "hooks.json"), BaseURL: "http://127.0.0.1:8081", APIKey: "k"}

	badBinary := base
	badBinary.SeamBin = "/opt/guard"
	_, err := Install(badBinary)
	require.ErrorContains(t, err, "must be named seam or seam.exe")

	badConfig := base
	badConfig.ConfigPath = "relative/seamless.yaml"
	_, err = Install(badConfig)
	require.ErrorContains(t, err, "config path must be absolute")
}

func TestRecordedCommandPaths(t *testing.T) {
	t.Run("codex shell form", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hooks.json")
		_, err := Install(codexOpts(path))
		require.NoError(t, err)
		bin, config, ok := RecordedCommandPaths(ClientCodex, path)
		require.True(t, ok)
		require.Equal(t, "/opt/seam", bin)
		require.Equal(t, "/etc/seamless.yaml", config)
	})
	t.Run("claude exec form", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		_, err := Install(InstallOptions{
			Client: ClientClaudeCode, SettingsPath: path, BaseURL: "http://127.0.0.1:8081",
			APIKey: "k", SeamBin: "/opt/seam", ConfigPath: "/etc/seamless.yaml",
		})
		require.NoError(t, err)
		bin, config, ok := RecordedCommandPaths(ClientClaudeCode, path)
		require.True(t, ok)
		require.Equal(t, "/opt/seam", bin)
		require.Equal(t, "/etc/seamless.yaml", config)
	})
	t.Run("foreign hooks are never read", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hooks.json")
		initial := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/usr/local/bin/guard hook stop --config /etc/guard.yaml","timeout":10}]}]}}`
		require.NoError(t, os.WriteFile(path, []byte(initial), 0o600))
		_, _, ok := RecordedCommandPaths(ClientCodex, path)
		require.False(t, ok)
	})
	t.Run("missing file", func(t *testing.T) {
		_, _, ok := RecordedCommandPaths(ClientCodex, filepath.Join(t.TempDir(), "none.json"))
		require.False(t, ok)
	})
}

func onlyHookHandler(entry map[string]any) map[string]any {
	return entry["hooks"].([]any)[0].(map[string]any)
}

func readHookSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(raw, &settings))
	return settings
}

func writeHookSettings(t *testing.T, path string, settings map[string]any) {
	t.Helper()
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
}
