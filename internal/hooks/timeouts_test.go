package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The timeouts the seam CLI sizes its dial budgets against (cmd/seam
// hook_test.go): every command hook a client's profile wires.
func TestCommandHookTimeouts_ByClient(t *testing.T) {
	const s = time.Second
	want := map[Client]map[string]time.Duration{
		ClientClaudeCode: {
			"session-start":      10 * s,
			"user-prompt-submit": 5 * s,
			"session-end":        10 * s,
			"post-tool-use":      10 * s,
			"subagent-start":     10 * s,
			"subagent-stop":      10 * s,
			"permission-request": 10 * s,
		},
		ClientCodex: {
			"session-start":      10 * s,
			"user-prompt-submit": 5 * s,
			"stop":               10 * s,
			"subagent-start":     10 * s,
			"subagent-stop":      10 * s,
		},
	}
	require.Len(t, want, len(HookClients), "every hook client needs a row")
	for _, client := range HookClients {
		got, err := CommandHookTimeouts(client)
		require.NoError(t, err)
		require.Equal(t, want[client], got, "%s", client)
	}

	// The zero value is the documented absent/default Claude Code selection, and
	// a present-but-unknown client is refused rather than defaulted.
	got, err := CommandHookTimeouts("")
	require.NoError(t, err)
	require.Equal(t, want[ClientClaudeCode], got)
	_, err = CommandHookTimeouts("codxe")
	require.ErrorContains(t, err, "valid values are claude-code, codex")
}

// The accessor reads the tables Install builds from, so it must agree with what
// Install actually writes: install every client on both transports and read
// each command hook's event and timeout back out of the file.
func TestCommandHookTimeouts_MatchWhatInstallWrites(t *testing.T) {
	for _, client := range HookClients {
		t.Run(string(client), func(t *testing.T) {
			written := map[string]time.Duration{}
			for _, base := range []string{"http://127.0.0.1:8081", httpsBase} {
				path := filepath.Join(t.TempDir(), "hooks.json")
				_, err := Install(InstallOptions{
					Client: client, SettingsPath: path, BaseURL: base, APIKey: "k",
					SeamBin: "/opt/bin/seam", ConfigPath: "/etc/seamless.yaml",
				})
				require.NoError(t, err)
				for arg, timeout := range writtenCommandHookTimeouts(t, path) {
					if prev, ok := written[arg]; !ok || timeout < prev {
						written[arg] = timeout
					}
				}
			}
			got, err := CommandHookTimeouts(client)
			require.NoError(t, err)
			require.Equal(t, written, got)
		})
	}
}

// writtenCommandHookTimeouts reads an installed hooks file back as the timeout
// of each `seam hook <arg>` command hook, keyed by arg, in either shape the
// installer writes: exec form (an args array) or a shell string (Codex).
func writtenCommandHookTimeouts(t *testing.T, path string) map[string]time.Duration {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var settings struct {
		Hooks map[string][]struct {
			Hooks []map[string]any `json:"hooks"`
		} `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(raw, &settings))
	out := map[string]time.Duration{}
	for event, entries := range settings.Hooks {
		for _, entry := range entries {
			for _, h := range entry.Hooks {
				if h["type"] != "command" {
					continue
				}
				args, ok := hookStringArgs(h["args"])
				if !ok {
					command, isStr := h["command"].(string)
					require.True(t, isStr, "%s: command %v", event, h["command"])
					words, split := splitHookCommand(command)
					require.True(t, split, "%s: %q", event, command)
					args = words[1:]
				}
				require.GreaterOrEqual(t, len(args), 2, "%s: %v", event, args)
				require.Equal(t, "hook", args[0], "%s: %v", event, args)
				seconds, ok := h["timeout"].(float64)
				require.True(t, ok, "%s: timeout %v", event, h["timeout"])
				out[args[1]] = time.Duration(seconds) * time.Second
			}
		}
	}
	return out
}
