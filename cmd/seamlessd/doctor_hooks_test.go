package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/hooks"
	"github.com/arctop/seamless/internal/store/storetest"
	"github.com/stretchr/testify/require"
)

func TestCodexChecksNoCodexIsQuiet(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "absent-codex-home"))
	t.Setenv("PATH", t.TempDir())

	checks := codexChecks(config.Defaults(), nil)
	require.Equal(t, []check{{
		status: statusOK,
		name:   "codex",
		detail: "not detected (no Codex CLI, initialized home, or Seamless configuration)",
	}}, checks)
}

func TestCodexChecksAppOnlyMarksMCPSetupIncomplete(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("PATH", t.TempDir())

	checks := codexChecks(config.Defaults(), nil)
	mcpChk := findCheck(t, checks, "codex mcp")
	require.Equal(t, statusWarn, mcpChk.status)
	require.Contains(t, mcpChk.detail, "management CLI not found")
	require.Contains(t, mcpChk.detail, "automated setup is incomplete")
	require.Contains(t, mcpChk.detail, "Settings > MCP servers > Add server > STDIO")
	require.Contains(t, mcpChk.detail, `name "seamless"`)
	require.Contains(t, mcpChk.detail, `arguments "mcp-proxy"`)
}

func TestDoctorClientChecksClaudeOnlyRemainDeterministic(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "absent-codex-home"))
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	seamBin := filepath.Join(binDir, seamBinName())
	writeTestExecutable(t, seamBin)
	t.Setenv("PATH", binDir)
	cfg := doctorTestConfig(t, t.TempDir())
	installDoctorClaudeHooks(t, cfg, seamBin)

	checks := []check{hooksCheck(cfg)}
	checks = append(checks, codexChecks(cfg, nil)...)
	require.Equal(t, []string{"hooks", "codex"}, checkNames(checks))
	require.Equal(t, statusOK, checks[0].status)
	require.Contains(t, checks[0].detail,
		"current: SessionStart, UserPromptSubmit, SessionEnd, PostToolUse, SubagentStart, SubagentStop, PermissionRequest")
	require.Equal(t, statusOK, checks[1].status)
	require.Contains(t, checks[1].detail, "not detected")
}

func TestCodexChecksExactDefinitionsAndMCPStillWarnsTrust(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the isolated fake Codex executable is a POSIX script")
	}
	fake := newFakeCodex(t, nil, nil, "")
	binDir := filepath.Dir(fake.path)
	seamBin := filepath.Join(binDir, seamBinName())
	writeTestExecutable(t, seamBin)
	t.Setenv("PATH", binDir)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())

	cfg := doctorTestConfig(t, t.TempDir())
	want, err := desiredCodexMCPState(seamBin, cfg.SourcePath())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(fake.statePath, marshalCodexMCPState(t, want), 0o600))
	installDoctorCodexHooks(t, cfg, seamBin)
	installDoctorClaudeHooks(t, cfg, seamBin)

	db, err := storetest.OpenFresh(t, cfg.DBPath())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	checks := []check{hooksCheck(cfg)}
	checks = append(checks, codexChecks(cfg, db)...)
	require.Equal(t, []string{"hooks", "codex CLI runtime", "codex hooks", "codex hook trust", "codex hook activity", "codex mcp"},
		checkNames(checks))
	require.Equal(t, statusOK, findCheck(t, checks, "hooks").status)
	runtimeChk := findCheck(t, checks, "codex CLI runtime")
	require.Equal(t, statusOK, runtimeChk.status)
	require.Contains(t, runtimeChk.detail, "codex-cli test-runtime")
	require.Contains(t, runtimeChk.detail, fake.path)

	hooksChk := findCheck(t, checks, "codex hooks")
	require.Equal(t, statusOK, hooksChk.status)
	require.Contains(t, hooksChk.detail,
		"current: SessionStart, UserPromptSubmit, Stop, SubagentStart, SubagentStop")
	require.Contains(t, hooksChk.detail, "stale: none; missing: none")

	trustChk := findCheck(t, checks, "codex hook trust")
	require.Equal(t, statusWarn, trustChk.status)
	require.Contains(t, trustChk.detail, "trust unverified; inspect /hooks")
	require.Contains(t, trustChk.detail, "desktop app does not expose that command")
	require.NotContains(t, trustChk.detail, "trusted_hash")

	activityChk := findCheck(t, checks, "codex hook activity")
	require.Equal(t, statusInfo, activityChk.status)
	require.Contains(t, activityChk.detail, "trust remains unverified")

	mcpChk := findCheck(t, checks, "codex mcp")
	require.Equal(t, statusOK, mcpChk.status)
	require.Contains(t, mcpChk.detail, "exact enabled stdio bridge")
	require.True(t, hasStatus(checks, statusWarn), "unknown trust must prevent an all-healthy report")
}

func TestCodexChecksReportsHookEventsByOperationalState(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*testing.T, string, config.Config, string)
		wantDetail []string
	}{
		{
			name: "missing one event",
			mutate: func(t *testing.T, path string, _ config.Config, _ string) {
				settings := readDoctorHookSettings(t, path)
				delete(settings["hooks"].(map[string]any), "SubagentStop")
				writeDoctorHookSettings(t, path, settings)
			},
			wantDetail: []string{"stale: none", "missing: SubagentStop"},
		},
		{
			name: "malformed hooks json",
			mutate: func(t *testing.T, path string, _ config.Config, _ string) {
				require.NoError(t, os.WriteFile(path, []byte(`{"hooks":`), 0o600))
			},
			wantDetail: []string{"cannot inspect", "hooks: parse", "fix or restore the JSON"},
		},
		{
			name: "wrong client discriminator",
			mutate: func(t *testing.T, path string, _ config.Config, _ string) {
				settings := readDoctorHookSettings(t, path)
				handler := doctorHookHandler(t, settings, "Stop")
				for _, field := range []string{"command", "command_windows"} {
					handler[field] = strings.ReplaceAll(handler[field].(string),
						" --client codex", " --client claude-code")
				}
				writeDoctorHookSettings(t, path, settings)
			},
			wantDetail: []string{"stale: Stop", "missing: none"},
		},
		{
			name: "old binary and config paths",
			mutate: func(t *testing.T, path string, cfg config.Config, seamBin string) {
				oldDir := t.TempDir()
				oldSeam := filepath.Join(oldDir, seamBinName())
				oldConfig := filepath.Join(oldDir, "seamless.yaml")
				writeTestExecutable(t, oldSeam)
				require.NoError(t, os.WriteFile(oldConfig, []byte("mcp: {}\n"), 0o600))

				settings := readDoctorHookSettings(t, path)
				for event := range settings["hooks"].(map[string]any) {
					handler := doctorHookHandler(t, settings, event)
					for _, field := range []string{"command", "command_windows"} {
						command := strings.ReplaceAll(handler[field].(string), seamBin, oldSeam)
						handler[field] = strings.ReplaceAll(command, cfg.SourcePath(), oldConfig)
					}
				}
				writeDoctorHookSettings(t, path, settings)
			},
			wantDetail: []string{
				"current: none",
				"stale: SessionStart, UserPromptSubmit, Stop, SubagentStart, SubagentStop",
				"missing: none",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", home)
			binDir := t.TempDir()
			seamBin := filepath.Join(binDir, seamBinName())
			writeTestExecutable(t, seamBin)
			t.Setenv("PATH", binDir)
			cfg := doctorTestConfig(t, t.TempDir())
			path := installDoctorCodexHooks(t, cfg, seamBin)
			tt.mutate(t, path, cfg, seamBin)

			chk := findCheck(t, codexChecks(cfg, nil), "codex hooks")
			require.Equal(t, statusWarn, chk.status)
			for _, want := range tt.wantDetail {
				require.Contains(t, chk.detail, want)
			}
			require.Contains(t, chk.detail, "seamlessd install-hooks --client codex")
		})
	}
}

func TestDoctorInstallOptionsUsesCurrentDesiredPaths(t *testing.T) {
	binDir := t.TempDir()
	seamBin := filepath.Join(binDir, seamBinName())
	writeTestExecutable(t, seamBin)
	t.Setenv("PATH", binDir)
	cfg := doctorTestConfig(t, t.TempDir())

	oldDir := t.TempDir()
	oldSeam := filepath.Join(oldDir, seamBinName())
	oldConfig := filepath.Join(oldDir, "seamless.yaml")
	writeTestExecutable(t, oldSeam)
	require.NoError(t, os.WriteFile(oldConfig, []byte("mcp: {}\n"), 0o600))
	path := filepath.Join(t.TempDir(), "hooks.json")
	_, err := hooks.Install(hooks.InstallOptions{
		Client: hooks.ClientCodex, SettingsPath: path, BaseURL: cfg.ServerURL(),
		APIKey: cfg.MCP.APIKey, SeamBin: oldSeam, ConfigPath: oldConfig,
	})
	require.NoError(t, err)

	opts := doctorInstallOptions(hooks.ClientCodex, path, cfg)
	require.Equal(t, seamBin, opts.SeamBin)
	require.Equal(t, cfg.SourcePath(), opts.ConfigPath)
	status, err := hooks.InstalledStatus(opts)
	require.NoError(t, err)
	events, err := hooks.InstalledEvents(hooks.ClientCodex)
	require.NoError(t, err)
	require.Equal(t, events, status.Stale)
	require.Empty(t, status.Current)
}

func TestCodexObservedActivityCannotMaskStaleDefinition(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	binDir := t.TempDir()
	seamBin := filepath.Join(binDir, seamBinName())
	writeTestExecutable(t, seamBin)
	t.Setenv("PATH", binDir)
	cfg := doctorTestConfig(t, t.TempDir())
	path := installDoctorCodexHooks(t, cfg, seamBin)

	settings := readDoctorHookSettings(t, path)
	handler := doctorHookHandler(t, settings, "Stop")
	handler["command"] = strings.ReplaceAll(handler["command"].(string), " --client codex", "")
	handler["command_windows"] = strings.ReplaceAll(handler["command_windows"].(string), " --client codex", "")
	writeDoctorHookSettings(t, path, settings)

	db, err := storetest.OpenFresh(t, cfg.DBPath())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	observedAt := time.Date(2026, 7, 20, 7, 0, 0, 0, time.UTC)
	_, err = events.NewRecorder(db).Record(context.Background(), core.Event{
		TS:   observedAt,
		Kind: core.EventHookPrompt,
		Payload: map[string]any{
			"external_client": "codex",
			"hook":            "user-prompt-submit",
		},
	})
	require.NoError(t, err)

	checks := codexChecks(cfg, db)
	hooksChk := findCheck(t, checks, "codex hooks")
	require.Equal(t, statusWarn, hooksChk.status)
	require.Contains(t, hooksChk.detail, "stale: Stop")

	activityChk := findCheck(t, checks, "codex hook activity")
	require.Equal(t, statusInfo, activityChk.status)
	require.Contains(t, activityChk.detail, "last observed UserPromptSubmit at 2026-07-20T07:00:00Z")
	require.Contains(t, activityChk.detail, "not proof that current definitions are trusted")
	require.Equal(t, statusWarn, findCheck(t, checks, "codex hook trust").status)
}

func TestHookDefinitionDetailUsesCanonicalProfileOrder(t *testing.T) {
	want := []string{"SessionStart", "UserPromptSubmit", "Stop", "SubagentStart", "SubagentStop"}
	status := hooks.InstallStatus{
		Current: []string{"SubagentStop", "SessionStart", "Stop"},
		Stale:   []string{"SubagentStart"},
	}
	detail := hookDefinitionDetail("hooks.json", want, status)
	require.Contains(t, detail, "current: SessionStart, Stop, SubagentStop")
	require.Contains(t, detail, "stale: SubagentStart")
	require.Contains(t, detail, "missing: UserPromptSubmit")
}

func doctorTestConfig(t *testing.T, dir string) config.Config {
	t.Helper()
	dataDir := filepath.Join(dir, "data")
	t.Setenv("SEAMLESS_DATA_DIR", dataDir)
	path := filepath.Join(dir, "seamless.yaml")
	raw := fmt.Sprintf("data_dir: %q\nmcp:\n  api_key: test-key\n", dataDir)
	require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
	cfg, err := config.LoadFrom(path)
	require.NoError(t, err)
	return cfg
}

func installDoctorCodexHooks(t *testing.T, cfg config.Config, seamBin string) string {
	t.Helper()
	path, err := expandHome(defaultCodexHooksPath())
	require.NoError(t, err)
	_, err = hooks.Install(hooks.InstallOptions{
		Client: hooks.ClientCodex, SettingsPath: path, BaseURL: cfg.ServerURL(),
		APIKey: cfg.MCP.APIKey, SeamBin: seamBin, ConfigPath: cfg.SourcePath(),
	})
	require.NoError(t, err)
	return path
}

func installDoctorClaudeHooks(t *testing.T, cfg config.Config, seamBin string) string {
	t.Helper()
	path, err := expandHome("~/.claude/settings.json")
	require.NoError(t, err)
	_, err = hooks.Install(hooks.InstallOptions{
		Client: hooks.ClientClaudeCode, SettingsPath: path, BaseURL: cfg.ServerURL(),
		APIKey: cfg.MCP.APIKey, SeamBin: seamBin, ConfigPath: cfg.SourcePath(),
	})
	require.NoError(t, err)
	return path
}

func writeTestExecutable(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755))
}

func readDoctorHookSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(raw, &settings))
	return settings
}

func writeDoctorHookSettings(t *testing.T, path string, settings map[string]any) {
	t.Helper()
	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
}

func doctorHookHandler(t *testing.T, settings map[string]any, event string) map[string]any {
	t.Helper()
	hooksObject, ok := settings["hooks"].(map[string]any)
	require.True(t, ok)
	entries, ok := hooksObject[event].([]any)
	require.True(t, ok)
	require.NotEmpty(t, entries)
	entry, ok := entries[0].(map[string]any)
	require.True(t, ok)
	handlers, ok := entry["hooks"].([]any)
	require.True(t, ok)
	require.Len(t, handlers, 1)
	handler, ok := handlers[0].(map[string]any)
	require.True(t, ok)
	return handler
}

func findCheck(t *testing.T, checks []check, name string) check {
	t.Helper()
	for _, chk := range checks {
		if chk.name == name {
			return chk
		}
	}
	t.Fatalf("missing check %q in %#v", name, checks)
	return check{}
}

func checkNames(checks []check) []string {
	names := make([]string, len(checks))
	for i, chk := range checks {
		names[i] = chk.name
	}
	return names
}

func hasStatus(checks []check, status checkStatus) bool {
	for _, chk := range checks {
		if chk.status == status {
			return true
		}
	}
	return false
}
