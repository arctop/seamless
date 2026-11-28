package hooks

// The transport shapes no hook: every Claude Code hook is a `seam hook <event>`
// command hook whether the base URL is http, https or a socket. UserPromptSubmit
// was the one exception -- an http hook on an http base URL, carrying the bearer
// key -- which the Claude app may not send (seamlessHooks). The entry such an
// install wrote must be replaced by the next install, never kept beside the
// command hook (both would fire).

import (
	"encoding/json"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const httpsBase = "https://seam.example:8443"

// Every spec's CLI event is its endpoint's last segment, and every one is an
// event the seam CLI is pinned against (CommandHookEndpoints is what
// cmd/seam/hook_test.go compares its own table to): a spec without one would be
// written as `seam hook ` and fail open on every call.
func TestHookSpecCLIArgsDeriveFromEndpoints(t *testing.T) {
	endpoints := CommandHookEndpoints()
	for _, profile := range [][]hookSpec{seamlessHooks, codexHooks} {
		for _, hs := range profile {
			require.NotEmpty(t, hs.CLIArg, "%s", hs.Event)
			require.Equal(t, pathpkg.Base(hs.Endpoint), hs.CLIArg, "%s", hs.Event)
			require.Equal(t, hs.Endpoint, endpoints[hs.CLIArg], "%s", hs.Event)
		}
	}
}

// http and https installs write the same file: exec-form command hooks, the
// bearer key nowhere in it.
func TestInstall_EveryTransportMakesEveryClaudeCodeHookExecForm(t *testing.T) {
	var files []string
	for _, base := range []string{"http://127.0.0.1:8081", httpsBase} {
		t.Run(base, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			opts := InstallOptions{
				SettingsPath: path, BaseURL: base, APIKey: "secret-key",
				SeamBin: "/opt/bin/seam", ConfigPath: "/etc/seamless.yaml",
			}
			res, err := Install(opts)
			require.NoError(t, err)
			require.True(t, res.Changed)

			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			var settings map[string]any
			require.NoError(t, json.Unmarshal(raw, &settings))
			hooksObj := settings["hooks"].(map[string]any)

			for _, hs := range seamlessHooks {
				requireCommandHook(t, hooksObj, hs.Event, hs.CLIArg)
			}
			ups := hooksObj["UserPromptSubmit"].([]any)
			require.Len(t, ups, 1)
			require.Equal(t, map[string]any{
				managedMarker: true,
				"hooks": []any{map[string]any{
					"type":    "command",
					"command": "/opt/bin/seam",
					"args":    []any{"hook", "user-prompt-submit", "--config", "/etc/seamless.yaml"},
					"timeout": float64(5),
				}},
			}, ups[0], "no matcher: every prompt")

			// `seam hook` reads the key from the 0600 config instead.
			require.NotContains(t, string(raw), "Bearer")
			require.NotContains(t, string(raw), "secret-key")
			require.NotContains(t, string(raw), `"http"`)
			files = append(files, string(raw))

			// Status agrees with what was written, and a re-install changes nothing.
			status, err := InstalledStatus(opts)
			require.NoError(t, err)
			require.Equal(t, installedEvents(t, ClientClaudeCode), status.Current)
			require.Empty(t, status.Stale)
			again, err := Install(opts)
			require.NoError(t, err)
			require.False(t, again.Changed)

			// Uninstall is still the exact inverse.
			un, err := Uninstall(UninstallOptions{SettingsPath: path, BaseURL: base})
			require.NoError(t, err)
			require.True(t, un.Changed)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NotContains(t, string(after), managedMarker)
			require.NotContains(t, string(after), "user-prompt-submit")
		})
	}
	require.Len(t, files, 2)
	require.Equal(t, files[0], files[1], "the base URL is in no hook, so the two files are the same bytes")
}

// The http UserPromptSubmit hook an earlier http install wrote, as it wrote it
// and as Claude Code leaves it after stripping the marker it does not know.
// Either way status reports the event stale (it fires, but not as it should),
// the next install puts the command hook in its place -- one entry, the key
// gone from the file -- and uninstall removes it without a reinstall.
func TestInstall_ReplacesAnEarlierHTTPUserPromptSubmitHook(t *testing.T) {
	const base = "http://127.0.0.1:8081"
	earlier := func(marked bool) map[string]any {
		entry := map[string]any{"hooks": []any{map[string]any{
			"type":    "http",
			"url":     base + "/api/hooks/user-prompt-submit",
			"timeout": 5,
			"headers": map[string]any{"Authorization": "Bearer secret-key"},
		}}}
		if marked {
			entry[managedMarker] = true
		}
		return entry
	}
	for _, tc := range []struct {
		name   string
		marked bool
		action string
	}{
		{"as written", true, "UserPromptSubmit: updated"},
		{"marker stripped", false, "UserPromptSubmit: adopted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			opts := InstallOptions{
				SettingsPath: path, BaseURL: base, APIKey: "secret-key",
				SeamBin: "/opt/bin/seam", ConfigPath: "/etc/seamless.yaml",
			}
			_, err := Install(opts)
			require.NoError(t, err)
			// Put the earlier entry back where the command hook went, as an
			// install from before the change left the file.
			var settings map[string]any
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &settings))
			settings["hooks"].(map[string]any)["UserPromptSubmit"] = []any{earlier(tc.marked)}
			raw, err = json.Marshal(settings)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, raw, 0o600))

			status, err := InstalledStatus(opts)
			require.NoError(t, err)
			require.Equal(t, []string{"UserPromptSubmit"}, status.Stale)
			require.NotContains(t, status.Current, "UserPromptSubmit")
			require.Contains(t, status.Owned, "UserPromptSubmit")

			// Uninstall recognizes it without a reinstall first.
			copyPath := filepath.Join(t.TempDir(), "settings.json")
			require.NoError(t, os.WriteFile(copyPath, raw, 0o600))
			un, err := Uninstall(UninstallOptions{SettingsPath: copyPath, BaseURL: base})
			require.NoError(t, err)
			require.True(t, un.Changed)
			left, err := os.ReadFile(copyPath)
			require.NoError(t, err)
			require.NotContains(t, string(left), "user-prompt-submit")
			require.NotContains(t, string(left), "secret-key")

			res, err := Install(opts)
			require.NoError(t, err)
			require.True(t, res.Changed)
			require.Contains(t, strings.Join(res.Actions, ","), tc.action)

			raw, err = os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &settings))
			hooksObj := settings["hooks"].(map[string]any)
			require.Len(t, hooksObj["UserPromptSubmit"].([]any), 1, "replaced in place, not kept beside the command hook")
			requireCommandHook(t, hooksObj, "UserPromptSubmit", "user-prompt-submit")
			require.NotContains(t, string(raw), "Bearer")
			require.NotContains(t, string(raw), "secret-key")

			status, err = InstalledStatus(opts)
			require.NoError(t, err)
			require.Equal(t, installedEvents(t, ClientClaudeCode), status.Current)
			require.Empty(t, status.Stale)
		})
	}
}
