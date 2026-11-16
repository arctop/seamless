package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// drSocketBase is a peer_auth socket's base URL: the desired Claude Code
// registration is then the seam mcp-proxy stdio bridge.
const drSocketBase = "unix:///opt/seamless/run/seamlessd.sock"

func TestClaudeCodeConfigPathFor(t *testing.T) {
	home := absFixture("/Users/example-owner")
	got, err := claudeCodeConfigPathFor(home, "")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, ".claude.json"), got)

	dir := absFixture("/srv/claude-config")
	got, err = claudeCodeConfigPathFor(home, dir)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, ".claude.json"), got, "CLAUDE_CONFIG_DIR moves the file")

	_, err = claudeCodeConfigPathFor("", "")
	require.ErrorContains(t, err, "home directory is unknown")
}

func TestParseClaudeCodeMCPEntry(t *testing.T) {
	for _, tt := range []struct {
		name  string
		raw   string
		want  claudeCodeMCPEntry
		stdio bool
		err   string
	}{
		{"stdio as claude mcp add writes it", `{"type":"stdio","command":"/s/seam","args":["mcp-proxy"],"env":{}}`,
			claudeCodeMCPEntry{Type: "stdio", Command: "/s/seam", Args: []string{"mcp-proxy"}}, true, ""},
		{"stdio with no type", `{"command":"/s/seam","args":["mcp-proxy"]}`,
			claudeCodeMCPEntry{Command: "/s/seam", Args: []string{"mcp-proxy"}}, true, ""},
		{"an env with values is recorded as present only", `{"command":"/s/seam","env":{"TOKEN":"must-not-appear"}}`,
			claudeCodeMCPEntry{Command: "/s/seam", HasEnv: true}, true, ""},
		{"http", `{"type":"http","url":"http://127.0.0.1:8081/api/mcp","headersHelper":"/s/seam mcp-headers"}`,
			claudeCodeMCPEntry{Type: "http"}, false, ""},
		{"not an object", `["seam"]`, claudeCodeMCPEntry{}, false, "parse Claude Code MCP entry"},
		{"null", `null`, claudeCodeMCPEntry{}, false, "expected object"},
		{"args of another type", `{"command":"/s/seam","args":"mcp-proxy"}`, claudeCodeMCPEntry{}, false, `field "args"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseClaudeCodeMCPEntry(json.RawMessage(tt.raw))
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.stdio, got.stdio())
		})
	}
}

// The repair rule: only an owned stdio bridge that differs from the desired
// stdio bridge is registered again. Every other shape is left alone.
func TestClaudeCodeRepairDrift(t *testing.T) {
	seam, cfgPath := absFixture("/opt/seamless/bin/seam"), absFixture("/Users/example-owner/.config/seamless/seamless.yaml")
	bridge, err := desiredClaudeCodeMCPEntry(drSocketBase, seam, cfgPath)
	require.NoError(t, err)
	require.Equal(t, claudeCodeMCPEntry{Type: "stdio", Command: seam, Args: []string{"mcp-proxy", "--config", cfgPath}}, bridge)
	httpWant, err := desiredClaudeCodeMCPEntry("http://127.0.0.1:8081", seam, cfgPath)
	require.NoError(t, err)
	require.False(t, httpWant.stdio())

	oldSeam := absFixture("/Users/example-owner/.local/bin/seam")
	for _, tt := range []struct {
		name string
		got  claudeCodeMCPEntry
		want claudeCodeMCPEntry
		// drift is "" when the registration is left as it is.
		drift string
	}{
		{"exact", bridge, bridge, ""},
		{"exact with no type", claudeCodeMCPEntry{Command: seam, Args: bridge.Args}, bridge, ""},
		{"another seam", claudeCodeMCPEntry{Type: "stdio", Command: oldSeam, Args: bridge.Args}, bridge, "bridge command differs"},
		{"another config", claudeCodeMCPEntry{Type: "stdio", Command: seam, Args: []string{"mcp-proxy", "--config", absFixture("/old/seamless.yaml")}},
			bridge, "bridge arguments differ"},
		{"no config", claudeCodeMCPEntry{Command: oldSeam, Args: []string{"mcp-proxy"}}, bridge, "bridge command differs, bridge arguments differ"},
		{"an env", claudeCodeMCPEntry{Type: "stdio", Command: seam, Args: bridge.Args, HasEnv: true}, bridge, "bridge environment present"},
		{"a bridge to another program", claudeCodeMCPEntry{Type: "stdio", Command: "/usr/bin/python3", Args: []string{"bridge.py"}}, bridge, ""},
		{"an http registration", claudeCodeMCPEntry{Type: "http"}, bridge, ""},
		{"an sse registration", claudeCodeMCPEntry{Type: "sse"}, bridge, ""},
		{"a bridge where http is desired", claudeCodeMCPEntry{Type: "stdio", Command: oldSeam, Args: bridge.Args}, httpWant, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(claudeCodeRepairDrift(tt.got, tt.want), ", ")
			require.Equal(t, tt.drift, got)
		})
	}
}

// drFakeClaude is an isolated claude CLI: mcp get, remove and add-json over a
// Claude Code config file of the test's, every call logged.
type drFakeClaude struct {
	path, config, log string
}

// drNewFakeClaude writes the fake into its own directory, named claude, over
// the config file at config. mode makes it fail: remove-fail, add-fail, or
// add-other (the add stores another bridge than it was given).
func drNewFakeClaude(t *testing.T, config, mode string) drFakeClaude {
	t.Helper()
	// The fake always exits on its own; the production bound only guards a hung
	// real CLI, and a spawn can exceed it under the race detector.
	prev := mcpCommandTimeout
	mcpCommandTimeout = 5 * time.Minute
	t.Cleanup(func() { mcpCommandTimeout = prev })

	dir := t.TempDir()
	f := drFakeClaude{path: filepath.Join(dir, "claude"), config: config, log: filepath.Join(dir, "calls.log")}
	require.NoError(t, os.WriteFile(f.log, nil, 0o600))
	script := `#!/bin/sh
set -eu
printf 'CALL' >> "$DR_FAKE_CLAUDE_LOG"
for arg in "$@"; do
  printf '|%s' "$arg" >> "$DR_FAKE_CLAUDE_LOG"
done
printf '\n' >> "$DR_FAKE_CLAUDE_LOG"
cfg=$DR_FAKE_CLAUDE_CONFIG
mode=${DR_FAKE_CLAUDE_MODE:-}
[ "$1" = mcp ] || exit 64
case "$2" in
  get)
    if [ -f "$cfg" ]; then
      case "$(/bin/cat "$cfg")" in
        *'"seamless"'*)
          printf 'seamless:\n  Scope: User config (available in all your projects)\n'
          /bin/cat "$cfg"
          exit 0 ;;
      esac
    fi
    printf 'No MCP server found with name: seamless\n' >&2
    exit 1 ;;
  remove)
    if [ "$mode" = remove-fail ]; then
      printf 'synthetic remove failure\n' >&2
      exit 1
    fi
    printf '{"mcpServers":{}}\n' > "$cfg" ;;
  add-json)
    if [ "$mode" = add-fail ]; then
      printf 'synthetic add failure\n' >&2
      exit 2
    fi
    if [ "$mode" = add-other ]; then
      printf '{"mcpServers":{"seamless":{"type":"stdio","command":"/elsewhere/seam","args":["mcp-proxy"]}}}\n' > "$cfg"
      exit 0
    fi
    printf '{"mcpServers":{"seamless":%s}}\n' "$6" > "$cfg" ;;
  *)
    exit 64 ;;
esac
`
	require.NoError(t, os.WriteFile(f.path, []byte(script), 0o755))
	t.Setenv("DR_FAKE_CLAUDE_CONFIG", config)
	t.Setenv("DR_FAKE_CLAUDE_LOG", f.log)
	t.Setenv("DR_FAKE_CLAUDE_MODE", mode)
	return f
}

// calls is the logged argv of each call, by subcommand: "get", "remove", ...
func (f drFakeClaude) calls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(f.log)
	require.NoError(t, err)
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if fields := strings.Split(line, "|"); len(fields) > 2 {
			out = append(out, fields[2])
		}
	}
	return out
}

// drClaudeConfig writes a Claude Code config file holding entry as the
// user-scope "seamless" server beside another server and keys of the
// account's, or no file for a nil entry.
func drClaudeConfig(t *testing.T, path string, entry any) {
	t.Helper()
	if entry == nil {
		return
	}
	raw, err := json.Marshal(map[string]any{
		"numStartups": 7,
		"projects":    map[string]any{"/Users/example-owner/repo": map[string]any{"mcpServers": map[string]any{}}},
		"mcpServers": map[string]any{
			"seamless": entry,
			"other":    map[string]any{"type": "stdio", "command": "/usr/bin/other", "env": map[string]string{"SECRET": "must-not-appear"}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
}

// The installer's Claude Code registration end to end against the fake CLI:
// which calls it makes, what it prints, and what the config file holds after.
func TestRegisterClaudeMCPWith(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the isolated fake claude CLI is a POSIX script")
	}
	seam, cfgPath := "/opt/seamless/bin/seam", "/Users/example-owner/.config/seamless/seamless.yaml"
	oldSeam := "/Users/example-owner/.local/bin/seam"
	bridge := func(command string, args ...string) map[string]any {
		return map[string]any{"type": "stdio", "command": command, "args": args, "env": map[string]string{}}
	}
	desired := bridge(seam, "mcp-proxy", "--config", cfgPath)
	stale := bridge(oldSeam, "mcp-proxy", "--config", cfgPath)
	addLine := "claude mcp add-json --scope user seamless '{\"args\":[\"mcp-proxy\",\"--config\",\"" + cfgPath + "\"],\"command\":\"" + seam + "\",\"type\":\"stdio\"}'"
	removeLine := "claude mcp remove --scope user seamless"

	for _, tt := range []struct {
		name    string
		base    string
		initial any
		mode    string
		noFile  bool // the installer knows no config file
		calls   []string
		want    []string
		dont    []string
		// after is the command the user-scope entry holds at the end; "" leaves
		// the file as the test wrote it.
		after string
	}{
		{name: "a stale bridge command is repaired", base: drSocketBase, initial: stale,
			calls: []string{"get", "remove", "add-json", "get"},
			want:  []string{"repaired (bridge command differs; --scope user)"}, after: seam},
		{name: "a stale config argument is repaired", base: drSocketBase, initial: bridge(seam, "mcp-proxy", "--config", "/old/seamless.yaml"),
			calls: []string{"get", "remove", "add-json", "get"},
			want:  []string{"repaired (bridge arguments differ; --scope user)"}, after: seam},
		{name: "an env on the bridge is repaired, its values unprinted", base: drSocketBase,
			initial: map[string]any{"type": "stdio", "command": seam, "args": []string{"mcp-proxy", "--config", cfgPath}, "env": map[string]string{"TOKEN": "must-not-appear"}},
			calls:   []string{"get", "remove", "add-json", "get"},
			want:    []string{"repaired (bridge environment present; --scope user)"}, after: seam},
		{name: "the exact bridge is left alone", base: drSocketBase, initial: desired,
			calls: []string{"get"}, want: []string{"already registered"}},
		{name: "a bridge to another program is left alone", base: drSocketBase, initial: bridge("/usr/bin/python3", "bridge.py"),
			calls: []string{"get"}, want: []string{"already registered"}},
		{name: "an http registration is left alone where the bridge is desired", base: drSocketBase,
			initial: map[string]any{"type": "http", "url": "http://127.0.0.1:8081/api/mcp", "headersHelper": oldSeam + " mcp-headers"},
			calls:   []string{"get"}, want: []string{"already registered"}},
		{name: "a bridge is left alone where http is desired", base: "http://127.0.0.1:8081", initial: stale,
			calls: []string{"get"}, want: []string{"already registered"}},
		{name: "with no known config file a stale bridge is left alone, as before", base: drSocketBase, initial: stale, noFile: true,
			calls: []string{"get"}, want: []string{"already registered"}},
		{name: "the stored-key upgrade runs as before", base: "http://127.0.0.1:8081",
			initial: map[string]any{"type": "http", "url": "http://127.0.0.1:8081/api/mcp", "headers": map[string]string{"Authorization": "Bearer stored-key"}},
			calls:   []string{"get", "remove", "add-json", "get"},
			want:    []string{"re-registered (--scope user; bearer key moved out of ~/.claude.json)"}},
		{name: "an absent registration is added", base: drSocketBase,
			calls: []string{"get", "add-json", "get"},
			want:  []string{"registered (--scope user, stdio bridge: seam mcp-proxy)"}, after: seam},
		{name: "an absent http registration is added", base: "http://127.0.0.1:8081",
			calls: []string{"get", "add-json", "get"},
			want:  []string{"registered (--scope user, key via headersHelper)"}},
		{name: "a failed remove adds nothing and prints both commands", base: drSocketBase, initial: stale, mode: "remove-fail",
			calls: []string{"get", "remove"},
			want:  []string{"could not replace the stale bridge registration", "synthetic remove failure", removeLine, addLine},
			after: oldSeam},
		{name: "a failed add prints the command to run", base: drSocketBase, initial: stale, mode: "add-fail",
			calls: []string{"get", "remove", "add-json"},
			want:  []string{"registration failed", "synthetic add failure", addLine}, dont: []string{"repaired"}},
		{name: "a repair is verified on the config file", base: drSocketBase, initial: stale, mode: "add-other",
			calls: []string{"get", "remove", "add-json", "get"},
			want:  []string{"registration could not be verified", "bridge command differs", removeLine, addLine},
			dont:  []string{"repaired"}, after: "/elsewhere/seam"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), ".claude.json")
			drClaudeConfig(t, config, tt.initial)
			fake := drNewFakeClaude(t, config, tt.mode)
			judged := config
			if tt.noFile {
				judged = ""
			}
			out := captureStdout(t, func() error {
				registerClaudeMCPWith(context.Background(), execMCPCommandRunner{client: "claude", path: fake.path, timeout: 30 * time.Second},
					judged, tt.base, seam, cfgPath)
				return nil
			})
			require.Equal(t, tt.calls, fake.calls(t))
			for _, w := range tt.want {
				require.Contains(t, out, w)
			}
			for _, d := range tt.dont {
				require.NotContains(t, out, d)
			}
			require.NotContains(t, out, "must-not-appear")
			require.NotContains(t, out, "stored-key")
			if tt.after != "" {
				got, present, err := readClaudeCodeMCPEntry(config)
				require.NoError(t, err)
				require.True(t, present)
				require.Equal(t, tt.after, got.Command)
			}
		})
	}
}

// The incident, replayed through the command: a peer_auth client's
// install-hooks --seam naming the daemon's seam, while ~/.claude.json still
// registers the bridge with the owner's old seam. Claude Code's bridge is
// repointed, as the Claude app's and Codex's are.
func TestRunInstallHooks_RepointsAClaudeCodeBridgeToTheNewSeam(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the isolated fake claude CLI is a POSIX script")
	}
	home := t.TempDir()
	clientInstallEnv(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	require.NoError(t, os.Unsetenv("CLAUDE_CONFIG_DIR"))
	sock := filepath.Join(t.TempDir(), "run", "seamlessd.sock")
	cfgPath := filepath.Join(home, ".config", "seamless", "seamless.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(cfgPath), 0o700))
	require.NoError(t, os.WriteFile(cfgPath, []byte("role: client\nserver_url: \"unix://"+sock+"\"\n"), 0o600))
	oldSeam := filepath.Join(home, ".local", "bin", "seam")
	newSeam := filepath.Join(t.TempDir(), "opt", "seamless", "bin", "seam")
	for _, bin := range []string{oldSeam, newSeam} {
		require.NoError(t, os.MkdirAll(filepath.Dir(bin), 0o755))
		require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))
	}
	claudeJSON := filepath.Join(home, ".claude.json")
	drClaudeConfig(t, claudeJSON, map[string]any{"type": "stdio", "command": oldSeam,
		"args": []string{"mcp-proxy", "--config", cfgPath}, "env": map[string]string{}})
	fake := drNewFakeClaude(t, claudeJSON, "")
	t.Setenv("PATH", filepath.Dir(fake.path))

	out := captureStdout(t, func() error {
		return runInstallHooks([]string{"--client", "claude", "--settings", filepath.Join(home, ".claude", "settings.json"),
			"--config", cfgPath, "--seam", newSeam, "--skills=false"})
	})
	require.Contains(t, out, "repaired (bridge command differs; --scope user)")
	require.NotContains(t, out, "already registered")
	got, present, err := readClaudeCodeMCPEntry(claudeJSON)
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, claudeCodeMCPEntry{Type: "stdio", Command: newSeam, Args: []string{"mcp-proxy", "--config", cfgPath}}, got)
	require.Equal(t, []string{"get", "remove", "add-json", "get"}, fake.calls(t))

	// A second run finds the exact bridge and leaves it.
	require.NoError(t, os.WriteFile(fake.log, nil, 0o600))
	out = captureStdout(t, func() error {
		return runInstallHooks([]string{"--client", "claude", "--settings", filepath.Join(home, ".claude", "settings.json"),
			"--config", cfgPath, "--seam", newSeam, "--skills=false"})
	})
	require.Contains(t, out, "already registered")
	require.Equal(t, []string{"get"}, fake.calls(t))
}

// The manual commands the installers print are what a shell reads back as the
// argv they would have run: the Claude Code registration's JSON (quotes,
// spaces) and paths with spaces included.
func TestMCPManualCommand_ParsesBackToTheArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell quoting")
	}
	seam := "/opt/Seamless App/bin/seam"
	cfgPath := "/Users/example-owner/Library/Application Support/Seamless/owner's seamless.yaml"
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	for _, args := range [][]string{
		claudeMCPAddArgs("http://127.0.0.1:8081", seam, cfgPath),
		claudeMCPAddArgs(drSocketBase, seam, cfgPath),
		claudeMCPRemoveArgs,
		codexMCPAddArgs(seam, cfgPath),
	} {
		line := mcpManualCommand("record", args)
		require.NoError(t, os.WriteFile(logPath, nil, 0o600))
		recorder := filepath.Join(dir, "record")
		require.NoError(t, os.WriteFile(recorder, []byte("#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> \""+logPath+"\"; done\n"), 0o755))
		cmd := exec.Command("/bin/sh", "-c", line)
		cmd.Env = append(os.Environ(), "PATH="+dir)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s: %s", line, out)
		raw, err := os.ReadFile(logPath)
		require.NoError(t, err)
		require.Equal(t, strings.Join(args, "\n")+"\n", string(raw), line)
	}
	// A plain path stays bare: the Codex line most installs print is unchanged.
	require.Equal(t, "codex mcp add seamless -- /opt/seam mcp-proxy --config /etc/seamless.yaml",
		mcpManualCommand("codex", codexMCPAddArgs("/opt/seam", "/etc/seamless.yaml")))
}
