package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/stretchr/testify/require"
)

func TestParseCodexMCPState_VersionedFixtures(t *testing.T) {
	for _, name := range []string{
		"stdio-enabled.json",
		"stdio-disabled.json",
		"streamable-http-enabled.json",
		"streamable-http-disabled.json",
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "hooks", "testdata", "codex", "v0.144.6", "mcp", name))
			require.NoError(t, err)

			// A new top-level field must not break a known contract.
			var fixture map[string]any
			require.NoError(t, json.Unmarshal(raw, &fixture))
			fixture["future_codex_metadata"] = map[string]any{"revision": 2}
			raw, err = json.Marshal(fixture)
			require.NoError(t, err)

			state, err := parseCodexMCPState(raw)
			require.NoError(t, err)
			require.Equal(t, seamlessMCPName, state.Name)
			require.Equal(t, strings.Contains(name, "enabled") && !strings.Contains(name, "disabled"), state.Enabled)
			require.NotNil(t, state.StartupTimeoutSec)
			require.NotNil(t, state.ToolTimeoutSec)
			if strings.HasPrefix(name, "stdio") {
				require.Equal(t, "stdio", state.Transport.Type)
				require.Equal(t, "/opt/seam/bin/seam", state.Transport.Command)
				require.Equal(t, []string{"mcp-proxy", "--config", "/Users/dev/.config/seamless/seamless.yaml"}, state.Transport.Args)
				require.Len(t, state.Transport.EnvVars, 2)
			} else {
				require.Equal(t, "streamable_http", state.Transport.Type)
				require.Equal(t, "https://mcp.example.invalid/api/mcp", state.Transport.URL)
				require.NotNil(t, state.Transport.BearerTokenEnvVar)
			}
		})
	}
}

func TestParseCodexMCPState_RejectsMissingAndWrongTypeRequiredFields(t *testing.T) {
	want, err := desiredCodexMCPState(absFixture("/opt/seam"), absFixture("/etc/seamless.yaml"))
	require.NoError(t, err)
	valid := marshalCodexMCPState(t, want)

	var base map[string]any
	require.NoError(t, json.Unmarshal(valid, &base))
	for _, tt := range []struct {
		name   string
		mutate func(map[string]any)
		match  string
	}{
		{"missing enabled", func(v map[string]any) { delete(v, "enabled") }, "missing required field"},
		{"wrong enabled type", func(v map[string]any) { v["enabled"] = "yes" }, `field "enabled"`},
		{"missing timeout", func(v map[string]any) { delete(v, "tool_timeout_sec") }, "missing required field"},
		{"null transport", func(v map[string]any) { v["transport"] = nil }, `field "transport"`},
		{"missing command", func(v map[string]any) {
			delete(v["transport"].(map[string]any), "command")
		}, "missing required field"},
		{"wrong args type", func(v map[string]any) {
			v["transport"].(map[string]any)["args"] = "mcp-proxy"
		}, `field "args"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			copyRaw, copyErr := json.Marshal(base)
			require.NoError(t, copyErr)
			var value map[string]any
			require.NoError(t, json.Unmarshal(copyRaw, &value))
			tt.mutate(value)
			raw, marshalErr := json.Marshal(value)
			require.NoError(t, marshalErr)
			_, parseErr := parseCodexMCPState(raw)
			require.ErrorContains(t, parseErr, tt.match)
		})
	}

	_, err = parseCodexMCPState([]byte(`{"name":`))
	require.ErrorContains(t, err, "parse Codex MCP JSON")
}

func TestCodexMCPComparator_ClassifiesExactOwnedDriftAndForeign(t *testing.T) {
	want, err := desiredCodexMCPState(absFixture("/new/bin/seam"), absFixture("/new/config/seamless.yaml"))
	require.NoError(t, err)
	disabledReason := "disabled in config"

	for _, tt := range []struct {
		name      string
		mutate    func(*codexMCPState)
		wantClass mcpRegClass
		wantDrift string
	}{
		{"exact", func(*codexMCPState) {}, mcpRegExact, ""},
		{"disabled", func(s *codexMCPState) {
			s.Enabled = false
			s.DisabledReason = &disabledReason
		}, mcpRegOwnedDrifted, "disabled"},
		{"old binary", func(s *codexMCPState) {
			s.Transport.Command = "/old/bin/seam"
		}, mcpRegOwnedDrifted, "command"},
		{"old config", func(s *codexMCPState) {
			s.Transport.Args = []string{"mcp-proxy", "--config", "/old/config/seamless.yaml"}
		}, mcpRegOwnedDrifted, "arguments"},
		{"wrong args", func(s *codexMCPState) {
			s.Transport.Args = []string{"mcp-proxy", "--unknown"}
		}, mcpRegOwnedDrifted, "arguments"},
		{"environment drift", func(s *codexMCPState) {
			s.Transport.Env = map[string]string{"TOKEN": "must-not-appear-in-diagnostic"}
		}, mcpRegOwnedDrifted, "environment"},
		{"timeout drift", func(s *codexMCPState) {
			seconds := 10.0
			s.ToolTimeoutSec = &seconds
		}, mcpRegOwnedDrifted, "timeouts"},
		{"direct HTTP is foreign", func(s *codexMCPState) {
			s.Transport = codexMCPTransport{Type: "streamable_http", URL: "https://example.invalid/api/mcp"}
		}, mcpRegIncompatible, "transport"},
		{"arbitrary stdio is foreign", func(s *codexMCPState) {
			s.Transport.Command = "/usr/bin/python3"
			s.Transport.Args = []string{"bridge.py"}
		}, mcpRegIncompatible, "command"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := cloneCodexMCPState(want)
			tt.mutate(&got)
			class, drift := classifyCodexMCPState(got, want)
			require.Equal(t, tt.wantClass, class)
			if tt.wantDrift == "" {
				require.Empty(t, drift)
			} else {
				require.Contains(t, strings.Join(drift, ", "), tt.wantDrift)
				require.NotContains(t, strings.Join(drift, ", "), "must-not-appear")
			}
		})
	}
}

func TestExecMCPCommandRunner_SeparatesStdoutAndStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the isolated fake MCP client is a POSIX script")
	}
	path := filepath.Join(t.TempDir(), "codex")
	require.NoError(t, os.WriteFile(path, []byte(`#!/bin/sh
printf '{"ok":true}\n'
printf 'benign warning\n' >&2
if [ "${1:-}" = fail ]; then
  exit 2
fi
`), 0o755))

	runner := execMCPCommandRunner{client: "codex", path: path, timeout: 2 * time.Second}
	out, err := runner.Run(context.Background(), "success")
	require.NoError(t, err)
	require.JSONEq(t, `{"ok":true}`, string(out),
		"successful machine-readable output must not be corrupted by stderr warnings")

	out, err = runner.Run(context.Background(), "fail")
	require.ErrorContains(t, err, "codex fail")
	require.Contains(t, string(out), `{"ok":true}`)
	require.Contains(t, string(out), "benign warning",
		"failed commands must retain stderr for actionable diagnostics")
}

func TestReconcileCodexMCP_StateMachine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the isolated fake Codex executable is a POSIX script")
	}
	want, err := desiredCodexMCPState("/new/bin/seam", "/new/config/seamless.yaml")
	require.NoError(t, err)
	disabled := cloneCodexMCPState(want)
	disabled.Enabled = false
	oldBinary := cloneCodexMCPState(want)
	oldBinary.Transport.Command = "/old/bin/seam"
	oldConfig := cloneCodexMCPState(want)
	oldConfig.Transport.Args = []string{"mcp-proxy", "--config", "/old/config/seamless.yaml"}
	wrongArgs := cloneCodexMCPState(want)
	wrongArgs.Transport.Args = []string{"mcp-proxy", "--bogus"}
	foreignHTTP := cloneCodexMCPState(want)
	foreignHTTP.Transport = codexMCPTransport{
		Type: "streamable_http", URL: "https://mcp.example.invalid/api/mcp",
		HTTPHeaders: map[string]string{"Authorization": "Bearer fixture-secret-must-stay-hidden"},
	}

	for _, tt := range []struct {
		name       string
		initial    []byte
		afterAdd   []byte
		mode       string
		timeout    time.Duration
		wantAction mcpRegAction
		wantErr    string
		wantAdds   int
		wantGets   int
	}{
		{"absent adds", nil, marshalCodexMCPState(t, want), "", 2 * time.Second, mcpRegAdded, "", 1, 2},
		{"exact reinstall is no-op", marshalCodexMCPState(t, want), nil, "", 2 * time.Second, mcpRegUnchanged, "", 0, 1},
		{"disabled repairs", marshalCodexMCPState(t, disabled), marshalCodexMCPState(t, want), "", 2 * time.Second, mcpRegRepaired, "", 1, 2},
		{"old binary repairs", marshalCodexMCPState(t, oldBinary), marshalCodexMCPState(t, want), "", 2 * time.Second, mcpRegRepaired, "", 1, 2},
		{"old config repairs", marshalCodexMCPState(t, oldConfig), marshalCodexMCPState(t, want), "", 2 * time.Second, mcpRegRepaired, "", 1, 2},
		{"wrong args repair", marshalCodexMCPState(t, wrongArgs), marshalCodexMCPState(t, want), "", 2 * time.Second, mcpRegRepaired, "", 1, 2},
		{"HTTP entry fails loudly", marshalCodexMCPState(t, foreignHTTP), nil, "", 2 * time.Second, "", "incompatible", 0, 1},
		{"malformed get JSON", []byte(`{"name":`), nil, "", 2 * time.Second, "", "parse Codex MCP JSON", 0, 1},
		{"malformed list JSON", nil, nil, "malformed-list", 2 * time.Second, "", "mcp list --json", 0, 1},
		{"old Codex without JSON", nil, nil, "no-json", 2 * time.Second, "", "absence check failed", 0, 1},
		{"timeout", nil, nil, "timeout", 3 * time.Second, "", "deadline exceeded", 0, 1},
		{"add failure", nil, marshalCodexMCPState(t, want), "add-fail", 2 * time.Second, "", "write desired", 1, 1},
		{"repair add failure preserves old entry", marshalCodexMCPState(t, oldConfig), marshalCodexMCPState(t, want), "add-fail", 2 * time.Second, "", "synthetic add failure", 1, 1},
		{"repair verifies second get", marshalCodexMCPState(t, disabled), marshalCodexMCPState(t, oldConfig), "", 2 * time.Second, "", "desired state still differs", 1, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeCodex(t, tt.initial, tt.afterAdd, tt.mode)
			result, reconcileErr := reconcileCodexMCP(context.Background(), execMCPCommandRunner{
				client: "codex", path: fake.path, timeout: tt.timeout,
			}, want.Transport.Command, mcpBridgeConfigPath(want.Transport.Args))
			if tt.wantErr != "" {
				require.ErrorContains(t, reconcileErr, tt.wantErr)
				require.NotContains(t, reconcileErr.Error(), "fixture-secret-must-stay-hidden")
			} else {
				require.NoError(t, reconcileErr)
				require.Equal(t, tt.wantAction, result.Action)
			}

			logRaw, readErr := os.ReadFile(fake.logPath)
			require.NoError(t, readErr)
			logText := string(logRaw)
			require.Equal(t, tt.wantAdds, strings.Count(logText, "CALL|mcp|add|"))
			require.Equal(t, tt.wantGets, strings.Count(logText, "CALL|mcp|get|"))
			require.NotContains(t, logText, "|remove|")
			if tt.name == "repair add failure preserves old entry" {
				stateRaw, stateErr := os.ReadFile(fake.statePath)
				require.NoError(t, stateErr)
				require.JSONEq(t, string(tt.initial), string(stateRaw))
			}
		})
	}
}

func TestReconcileCodexMCP_PathsWithSpacesAndWindowsPathsRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the isolated fake Codex executable is a POSIX script")
	}
	for _, tt := range []struct {
		name       string
		seamBin    string
		configPath string
	}{
		{"spaces", "/opt/Seamless App/bin/seam", "/Users/test/Library/Application Support/Seamless/seamless.yaml"},
		{"windows", `C:\Program Files\Seamless\seam.exe`, `C:\Users\Test User\AppData\Roaming\Seamless\seamless.yaml`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want, err := desiredCodexMCPState(tt.seamBin, tt.configPath)
			require.NoError(t, err)
			require.Equal(t, tt.seamBin, want.Transport.Command)
			require.Equal(t, []string{"mcp-proxy", "--config", tt.configPath}, want.Transport.Args)

			fake := newFakeCodex(t, nil, marshalCodexMCPState(t, want), "")
			result, err := reconcileCodexMCP(context.Background(), execMCPCommandRunner{
				client: "codex", path: fake.path, timeout: 2 * time.Second,
			}, tt.seamBin, tt.configPath)
			require.NoError(t, err)
			require.Equal(t, mcpRegAdded, result.Action)

			logRaw, readErr := os.ReadFile(fake.logPath)
			require.NoError(t, readErr)
			require.Contains(t, string(logRaw), "CALL|mcp|add|seamless|--|"+tt.seamBin+"|mcp-proxy|--config|"+tt.configPath)
		})
	}

	_, err := desiredCodexMCPState("seam", "/etc/seamless.yaml")
	require.ErrorContains(t, err, "bridge command")
	_, err = desiredCodexMCPState("/opt/seam", "relative/seamless.yaml")
	require.ErrorContains(t, err, "config path")
	require.True(t, filepath.IsAbs(resolveSeamBin(filepath.Join("relative dir", "seam"))))
}

func TestCodexMCPDoctorUsesExactComparatorAndChecksPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the isolated fake Codex executable is a POSIX script")
	}
	dir := t.TempDir()
	seamBin := filepath.Join(dir, "seam")
	configPath := filepath.Join(dir, "seamless.yaml")
	require.NoError(t, os.WriteFile(seamBin, []byte("#!/bin/sh\n"), 0o755))
	require.NoError(t, os.WriteFile(configPath, []byte("mcp: {}\n"), 0o600))
	want, err := desiredCodexMCPState(seamBin, configPath)
	require.NoError(t, err)
	fake := newFakeCodex(t, marshalCodexMCPState(t, want), nil, "")
	runner := execMCPCommandRunner{client: "codex", path: fake.path, timeout: 2 * time.Second}

	chk := codexMCPCheckWithRunner(context.Background(), runner, seamBin, configPath)
	require.Equal(t, statusOK, chk.status)
	require.Contains(t, chk.detail, "exact enabled stdio")

	stale := cloneCodexMCPState(want)
	stale.Transport.Args = []string{"mcp-proxy", "--config", filepath.Join(dir, "old.yaml")}
	require.NoError(t, os.WriteFile(fake.statePath, marshalCodexMCPState(t, stale), 0o600))
	chk = codexMCPCheckWithRunner(context.Background(), runner, seamBin, configPath)
	require.Equal(t, statusWarn, chk.status)
	require.Contains(t, chk.detail, "owned registration is stale")

	missingBin := filepath.Join(dir, "missing", "seam")
	missingWant, desiredErr := desiredCodexMCPState(missingBin, configPath)
	require.NoError(t, desiredErr)
	require.NoError(t, os.WriteFile(fake.statePath, marshalCodexMCPState(t, missingWant), 0o600))
	chk = codexMCPCheckWithRunner(context.Background(), runner, missingBin, configPath)
	require.Equal(t, statusWarn, chk.status)
	require.Contains(t, chk.detail, "bridge executable is missing")
}

func TestCodexMCPDoctorReportsOperationalFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the isolated fake Codex executable is a POSIX script")
	}
	tests := []struct {
		name          string
		mode          string
		mutate        func(*codexMCPState)
		raw           []byte
		missingBin    bool
		missingConfig bool
		wantDetail    []string
	}{
		{
			name: "disabled registration",
			mutate: func(state *codexMCPState) {
				state.Enabled = false
			},
			wantDetail: []string{"owned registration is stale", "registration is disabled", "seamlessd install-hooks --client codex"},
		},
		{
			name: "wrong transport",
			mutate: func(state *codexMCPState) {
				state.Transport = codexMCPTransport{
					Type: "streamable_http", URL: "https://example.invalid/api/mcp",
				}
			},
			wantDetail: []string{"incompatible registration", "transport type differs", "codex mcp remove seamless"},
		},
		{
			name:       "malformed get json",
			raw:        []byte(`{"name":`),
			wantDetail: []string{"parse Codex MCP JSON", "codex mcp get seamless --json"},
		},
		{
			name:       "missing target executable",
			missingBin: true,
			wantDetail: []string{"exact registration is not runnable", "bridge executable is missing", "seamlessd install-hooks --client codex"},
		},
		{
			name:          "missing target config",
			missingConfig: true,
			wantDetail:    []string{"exact registration is not runnable", "config path is missing", "seamlessd install-hooks --client codex"},
		},
		{
			name:       "subprocess timeout",
			mode:       "timeout",
			wantDetail: []string{"deadline exceeded", "codex mcp get seamless --json"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			seamBin := filepath.Join(dir, "seam")
			if tt.missingBin {
				seamBin = filepath.Join(dir, "missing", "seam")
			} else {
				require.NoError(t, os.WriteFile(seamBin, []byte("#!/bin/sh\n"), 0o755))
			}
			configPath := filepath.Join(dir, "seamless.yaml")
			if !tt.missingConfig {
				require.NoError(t, os.WriteFile(configPath, []byte("mcp: {}\n"), 0o600))
			}
			want, err := desiredCodexMCPState(seamBin, configPath)
			require.NoError(t, err)

			got := cloneCodexMCPState(want)
			if tt.mutate != nil {
				tt.mutate(&got)
			}
			raw := tt.raw
			if raw == nil {
				raw = marshalCodexMCPState(t, got)
			}
			fake := newFakeCodex(t, raw, nil, tt.mode)
			timeout := 2 * time.Second
			if tt.mode == "timeout" {
				timeout = 25 * time.Millisecond
			}
			chk := codexMCPCheckWithRunner(context.Background(), execMCPCommandRunner{
				client: "codex", path: fake.path, timeout: timeout,
			}, seamBin, configPath)
			require.Equal(t, statusWarn, chk.status)
			for _, detail := range tt.wantDetail {
				require.Contains(t, chk.detail, detail)
			}
		})
	}
}

func TestInstallCodexHooks_FailedReconciliationReturnsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the isolated fake Codex executable is a POSIX script")
	}
	fake := newFakeCodex(t, nil, nil, "add-fail")
	t.Setenv("PATH", filepath.Dir(fake.path)+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.MCP.APIKey = "test-key-that-must-not-reach-codex"
	err := installCodexHooks(cfg, filepath.Join(dir, "hooks.json"), "http://127.0.0.1:8081",
		filepath.Join(dir, "seam"), filepath.Join(dir, "seamless.yaml"), true)
	require.ErrorContains(t, err, "reconcile Codex MCP registration")

	logRaw, readErr := os.ReadFile(fake.logPath)
	require.NoError(t, readErr)
	require.NotContains(t, string(logRaw), cfg.MCP.APIKey)
	require.NotContains(t, string(logRaw), "Bearer")
}

type fakeCodexCLI struct {
	path      string
	statePath string
	logPath   string
}

func newFakeCodex(t *testing.T, initial, afterAdd []byte, mode string) fakeCodexCLI {
	t.Helper()

	// The production command bound guards against a hung real CLI; spawning even
	// this trivial script can exceed it when the whole suite runs under -race.
	// The fake always exits on its own, so tests need only a hang backstop.
	// Tests that exercise the deadline path pass their own runner timeout,
	// long enough for the script to log its call under -race (the timeout case
	// gave it 500ms, and a loaded gate once saw no call logged at all).
	prevTimeout := mcpCommandTimeout
	mcpCommandTimeout = 5 * time.Minute
	t.Cleanup(func() { mcpCommandTimeout = prevTimeout })

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	afterPath := filepath.Join(dir, "after-add.json")
	logPath := filepath.Join(dir, "calls.log")
	if initial != nil {
		require.NoError(t, os.WriteFile(statePath, initial, 0o600))
	}
	if afterAdd != nil {
		require.NoError(t, os.WriteFile(afterPath, afterAdd, 0o600))
	}
	require.NoError(t, os.WriteFile(logPath, nil, 0o600))

	path := filepath.Join(dir, "codex")
	script := `#!/bin/sh
set -eu
printf 'CALL' >> "$FAKE_CODEX_LOG"
for fake_codex_arg in "$@"; do
  printf '|%s' "$fake_codex_arg" >> "$FAKE_CODEX_LOG"
done
printf '\n' >> "$FAKE_CODEX_LOG"
if [ "$1" = --version ]; then
  printf 'codex-cli test-runtime\n'
  exit 0
fi
if [ "${FAKE_CODEX_MODE:-}" = timeout ]; then
  exec /bin/sleep 10
fi
if [ "$1" != mcp ]; then
  exit 64
fi
case "$2" in
  get)
	if [ "${FAKE_CODEX_MODE:-}" = no-json ]; then
	  exit 2
	fi
    if [ -f "$FAKE_CODEX_STATE" ]; then
      /bin/cat "$FAKE_CODEX_STATE"
      exit 0
    fi
    exit 1
    ;;
  list)
	if [ "${FAKE_CODEX_MODE:-}" = no-json ]; then
	  exit 2
	fi
	if [ "${FAKE_CODEX_MODE:-}" = malformed-list ]; then
	  printf '{\n'
	  exit 0
	fi
    if [ -f "$FAKE_CODEX_STATE" ]; then
      printf '['
      /bin/cat "$FAKE_CODEX_STATE"
      printf ']\n'
    else
      printf '[]\n'
    fi
    ;;
  add)
    if [ "${FAKE_CODEX_MODE:-}" = add-fail ]; then
      printf 'synthetic add failure\n' >&2
      exit 2
    fi
    if [ ! -f "$FAKE_CODEX_AFTER_ADD" ]; then
      exit 3
    fi
    /bin/cp "$FAKE_CODEX_AFTER_ADD" "$FAKE_CODEX_STATE"
    ;;
  *)
    exit 64
    ;;
esac
`
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	t.Setenv("CODEX_HOME", filepath.Join(dir, "isolated-codex-home"))
	t.Setenv("FAKE_CODEX_STATE", statePath)
	t.Setenv("FAKE_CODEX_AFTER_ADD", afterPath)
	t.Setenv("FAKE_CODEX_LOG", logPath)
	t.Setenv("FAKE_CODEX_MODE", mode)
	return fakeCodexCLI{path: path, statePath: statePath, logPath: logPath}
}

func marshalCodexMCPState(t *testing.T, state codexMCPState) []byte {
	t.Helper()
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	return raw
}

func cloneCodexMCPState(state codexMCPState) codexMCPState {
	clone := state
	clone.Transport.Args = append([]string(nil), state.Transport.Args...)
	clone.EnabledTools = append([]string(nil), state.EnabledTools...)
	clone.DisabledTools = append([]string(nil), state.DisabledTools...)
	return clone
}
