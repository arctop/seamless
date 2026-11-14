package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/hooks"
)

const allClaudeEvents = "SessionStart, UserPromptSubmit, SessionEnd, PostToolUse, SubagentStart, SubagentStop, PermissionRequest"

func TestClaudeManagedSettingsDirFor(t *testing.T) {
	require.Equal(t, "/Library/Application Support/ClaudeCode", claudeManagedSettingsDirFor("darwin"))
	require.Equal(t, "/etc/claude-code", claudeManagedSettingsDirFor("linux"))
	require.Equal(t, `C:\Program Files\ClaudeCode`, claudeManagedSettingsDirFor("windows"))
	require.Empty(t, claudeManagedSettingsDirFor("plan9"))
}

// Claude Code merges managed-settings.json first, then the drop-ins in
// alphabetical order, ignoring hidden files and anything not ending in .json.
func TestClaudeManagedSettingsFilesMergeOrder(t *testing.T) {
	dir := t.TempDir()
	files, err := claudeManagedSettingsFiles(dir)
	require.NoError(t, err)
	require.Empty(t, files, "an empty directory holds no managed settings")

	dropins := filepath.Join(dir, claudeManagedSettingsDropins)
	require.NoError(t, os.MkdirAll(filepath.Join(dropins, "nested.json"), 0o755))
	for _, name := range []string{"20-security.json", "10-telemetry.json", ".hidden.json", "README.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dropins, name), []byte("{}"), 0o600))
	}
	files, err = claudeManagedSettingsFiles(dir)
	require.NoError(t, err)
	require.Equal(t, []string{
		filepath.Join(dropins, "10-telemetry.json"),
		filepath.Join(dropins, "20-security.json"),
	}, files, "drop-ins alone, without the main file")

	primary := filepath.Join(dir, claudeManagedSettingsFile)
	require.NoError(t, os.WriteFile(primary, []byte("{}"), 0o600))
	files, err = claudeManagedSettingsFiles(dir)
	require.NoError(t, err)
	require.Equal(t, primary, files[0])
	require.Len(t, files, 3)

	files, err = claudeManagedSettingsFiles("")
	require.NoError(t, err)
	require.Empty(t, files)
}

// useManagedSettingsDir points the managed settings lookup at a fresh temp dir.
func useManagedSettingsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	previous := claudeManagedSettingsDir
	claudeManagedSettingsDir = func() string { return dir }
	t.Cleanup(func() { claudeManagedSettingsDir = previous })
	return dir
}

// managedHooksWorld is a machine with a temp HOME and cwd, a seam executable
// on PATH, and a temp managed settings directory.
type managedHooksWorld struct {
	cfg        config.Config
	seamBin    string
	managedDir string
	userPath   string
}

func newManagedHooksWorld(t *testing.T) managedHooksWorld {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Chdir(t.TempDir())
	binDir := t.TempDir()
	seamBin := filepath.Join(binDir, seamBinName())
	writeTestExecutable(t, seamBin)
	t.Setenv("PATH", binDir)
	cfg := doctorTestConfig(t, t.TempDir())
	return managedHooksWorld{
		cfg: cfg, seamBin: seamBin, managedDir: useManagedSettingsDir(t),
		userPath: filepath.Join(home, ".claude", "settings.json"),
	}
}

// writeManaged writes the Claude Code profile into a managed settings file the
// way an agent guard does: the installer's definitions without its ownership
// marker, beside a foreign hook, keeping only the given events (all when none).
func (w managedHooksWorld) writeManaged(t *testing.T, name, seamBin string, keep ...string) string {
	t.Helper()
	return w.writeManagedNaming(t, name, seamBin, w.cfg.SourcePath(), keep...)
}

// writeManagedNaming is writeManaged with every hook's --config naming
// configPath rather than the config doctor runs with.
func (w managedHooksWorld) writeManagedNaming(t *testing.T, name, seamBin, configPath string, keep ...string) string {
	t.Helper()
	path := filepath.Join(w.managedDir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	_, err := hooks.Install(hooks.InstallOptions{
		Client: hooks.ClientClaudeCode, SettingsPath: path, BaseURL: w.cfg.HookBaseURL(),
		APIKey: w.cfg.MCP.APIKey, SeamBin: seamBin, ConfigPath: configPath,
	})
	require.NoError(t, err)
	settings := readDoctorHookSettings(t, path)
	hooksObj := settings["hooks"].(map[string]any)
	for event, entries := range hooksObj {
		if len(keep) > 0 && !slices.Contains(keep, event) {
			delete(hooksObj, event)
			continue
		}
		for _, entry := range entries.([]any) {
			delete(entry.(map[string]any), "seamless_managed")
		}
	}
	hooksObj["PreToolUse"] = []any{map[string]any{"hooks": []any{map[string]any{
		"type": "command", "command": "/usr/bin/env", "args": []any{"-i", "/opt/guard/bin/guard", "hook"},
	}}}}
	writeDoctorHookSettings(t, path, settings)
	return path
}

// installUser installs the profile into ~/.claude/settings.json, skipping the
// covered events.
func (w managedHooksWorld) installUser(t *testing.T, covered ...string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(w.userPath), 0o755))
	_, err := hooks.Install(hooks.InstallOptions{
		Client: hooks.ClientClaudeCode, SettingsPath: w.userPath, BaseURL: w.cfg.HookBaseURL(),
		APIKey: w.cfg.MCP.APIKey, SeamBin: w.seamBin, ConfigPath: w.cfg.SourcePath(), Covered: covered,
	})
	require.NoError(t, err)
}

// The layout an administrator who deploys the hooks chooses: managed settings
// carry all seven hooks and the user file none. Doctor used to call that "not installed".
func TestHooksCheckManagedSettingsCarryTheWholeProfile(t *testing.T) {
	w := newManagedHooksWorld(t)
	managed := w.writeManaged(t, claudeManagedSettingsFile, w.seamBin)

	chk := hooksCheck(w.cfg)
	require.Equal(t, statusOK, chk.status, chk.detail)
	require.Equal(t, "definitions in managed settings "+managed+" (current: "+allClaudeEvents+"; stale: none)", chk.detail)
}

// A user copy of a managed hook runs twice; doctor names it and the fix.
func TestHooksCheckFlagsUserCopyOfManagedHook(t *testing.T) {
	w := newManagedHooksWorld(t)
	w.writeManaged(t, claudeManagedSettingsFile, w.seamBin)
	w.installUser(t)

	chk := hooksCheck(w.cfg)
	require.Equal(t, statusWarn, chk.status)
	require.Contains(t, chk.detail, "runs twice: "+allClaudeEvents+" also in "+w.userPath)
	require.Contains(t, chk.detail, "run: seamlessd install-hooks --client claude to remove that copy")
}

// The incident that found this: the managed copy kept calling a seam that had
// been removed. The managed file is not the user's, so the fix names who can
// change it rather than a command.
func TestHooksCheckManagedHookWithMissingExecutable(t *testing.T) {
	w := newManagedHooksWorld(t)
	gone := filepath.Join(t.TempDir(), "gone", seamBinName())
	managed := w.writeManaged(t, claudeManagedSettingsFile, gone)

	chk := hooksCheck(w.cfg)
	require.Equal(t, statusWarn, chk.status)
	// UserPromptSubmit is an http hook under this http base URL: it runs no seam.
	require.Contains(t, chk.detail,
		"(current: UserPromptSubmit; stale: SessionStart, SessionEnd, PostToolUse, SubagentStart, SubagentStop, PermissionRequest)")
	require.Contains(t, chk.detail, "stale in "+managed)
	require.Contains(t, chk.detail, "an administrator must update it")
	require.Contains(t, chk.detail, "not runnable from "+managed)
	require.Contains(t, chk.detail, "is missing")
	require.NotContains(t, chk.detail, "runs twice")
}

// Managed settings may carry part of the profile; the user file carries the
// rest. Doctor reports where each part lives, and what is missing.
func TestHooksCheckSplitsProfileBetweenManagedAndUser(t *testing.T) {
	w := newManagedHooksWorld(t)
	managed := w.writeManaged(t, filepath.Join(claudeManagedSettingsDropins, "50-seamless.json"), w.seamBin,
		"SessionStart", "SessionEnd")

	chk := hooksCheck(w.cfg)
	require.Equal(t, statusWarn, chk.status)
	require.Contains(t, chk.detail, "managed settings "+managed+" (current: SessionStart, SessionEnd; stale: none)")
	require.Contains(t, chk.detail,
		"outside the managed settings, stale: none; missing: UserPromptSubmit, PostToolUse, SubagentStart, SubagentStop, PermissionRequest")

	w.installUser(t, "SessionStart", "SessionEnd")
	chk = hooksCheck(w.cfg)
	require.Equal(t, statusOK, chk.status, chk.detail)
	require.Contains(t, chk.detail, " and "+w.userPath+" (current: UserPromptSubmit, PostToolUse, SubagentStart, SubagentStop, PermissionRequest)")
}

// The same event in two managed files runs twice too.
func TestHooksCheckFlagsEventInTwoManagedFiles(t *testing.T) {
	w := newManagedHooksWorld(t)
	first := w.writeManaged(t, claudeManagedSettingsFile, w.seamBin)
	second := w.writeManaged(t, filepath.Join(claudeManagedSettingsDropins, "90-extra.json"), w.seamBin, "SessionStart")

	chk := hooksCheck(w.cfg)
	require.Equal(t, statusWarn, chk.status)
	require.Contains(t, chk.detail, "SessionStart runs twice: it is in "+first+" and "+second)
}

// A managed file Claude Code cannot parse makes it refuse to start; doctor
// says so even when the user file is otherwise fine.
func TestHooksCheckWarnsOnUnreadableManagedSettings(t *testing.T) {
	w := newManagedHooksWorld(t)
	w.installUser(t)
	managed := filepath.Join(w.managedDir, claudeManagedSettingsFile)
	require.NoError(t, os.WriteFile(managed, []byte("{"), 0o600))

	chk := hooksCheck(w.cfg)
	require.Equal(t, statusWarn, chk.status)
	require.Contains(t, chk.detail, "current: "+allClaudeEvents)
	require.Contains(t, chk.detail, "cannot inspect managed settings "+managed)
}

// install-hooks leaves managed events to the managed settings: it writes none
// of them, removes the copies an earlier install left, keeps foreign hooks, and
// says where they run from. Doctor is then clean.
func TestInstallClaudeHooksLeavesManagedEventsToManagedSettings(t *testing.T) {
	w := newManagedHooksWorld(t)
	managed := w.writeManaged(t, claudeManagedSettingsFile, w.seamBin)
	w.installUser(t)
	settings := readDoctorHookSettings(t, w.userPath)
	settings["hooks"].(map[string]any)["UserPromptSubmit"] = append(
		settings["hooks"].(map[string]any)["UserPromptSubmit"].([]any),
		map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/usr/local/bin/email-guard"}}})
	writeDoctorHookSettings(t, w.userPath, settings)

	out := captureStdout(t, func() error {
		return installClaudeHooks(w.cfg, w.userPath, w.cfg.HookBaseURL(), w.seamBin, w.cfg.SourcePath(), false)
	})
	require.Contains(t, out, "7 removed")
	require.Contains(t, out, allClaudeEvents+" run from "+tildePath(managed)+"; not written here")

	raw, err := os.ReadFile(w.userPath)
	require.NoError(t, err)
	var after map[string]any
	require.NoError(t, json.Unmarshal(raw, &after))
	require.Equal(t, map[string]any{"UserPromptSubmit": []any{map[string]any{"hooks": []any{
		map[string]any{"type": "command", "command": "/usr/local/bin/email-guard"},
	}}}}, after["hooks"], "only the foreign hook is left")

	chk := hooksCheck(w.cfg)
	require.Equal(t, statusOK, chk.status, chk.detail)

	out = captureStdout(t, func() error {
		return installClaudeHooks(w.cfg, w.userPath, w.cfg.HookBaseURL(), w.seamBin, w.cfg.SourcePath(), false)
	})
	require.Contains(t, out, "7 managed")
	require.NotContains(t, out, "removed")
}

// A managed definition this install would write differently is reported, not
// rewritten: the file is not the user's.
func TestInstallClaudeHooksReportsDriftInManagedSettings(t *testing.T) {
	w := newManagedHooksWorld(t)
	w.writeManaged(t, claudeManagedSettingsFile, filepath.Join(t.TempDir(), seamBinName()), "SessionStart")

	out := captureStdout(t, func() error {
		return installClaudeHooks(w.cfg, w.userPath, w.cfg.HookBaseURL(), w.seamBin, w.cfg.SourcePath(), false)
	})
	require.Contains(t, out, "differ from this install: SessionStart")
	require.Contains(t, out, "6 added")
	status, err := hooks.InstalledStatus(doctorInstallOptions(hooks.ClientClaudeCode, w.userPath, w.cfg))
	require.NoError(t, err)
	require.NotContains(t, status.Owned, "SessionStart")
	require.Len(t, status.Current, 6)
}

// Uninstall cannot remove managed hooks, so it names them and who can.
func TestUninstallClientHooksNamesManagedHooksLeftBehind(t *testing.T) {
	w := newManagedHooksWorld(t)
	managed := w.writeManaged(t, claudeManagedSettingsFile, w.seamBin, "SessionStart", "SessionEnd")
	w.installUser(t, "SessionStart", "SessionEnd")
	opts := hooks.InstallOptions{
		BaseURL: w.cfg.HookBaseURL(), APIKey: w.cfg.MCP.APIKey, SeamBin: w.seamBin, ConfigPath: w.cfg.SourcePath(),
	}

	for _, dryRun := range []bool{true, false} {
		out := captureStdout(t, func() error {
			return uninstallClientHooks(hooks.ClientClaudeCode, w.userPath, opts, dryRun)
		})
		require.Contains(t, out, "SessionStart, SessionEnd still run from "+tildePath(managed))
		require.Contains(t, out, "ask whoever deployed it to remove them")
	}
	status, err := hooks.InstalledStatus(doctorInstallOptions(hooks.ClientClaudeCode, w.userPath, w.cfg))
	require.NoError(t, err)
	require.Empty(t, status.Owned, "the user file's hooks are removed")
	raw, err := os.ReadFile(managed)
	require.NoError(t, err)
	require.Contains(t, string(raw), "session-start", "the managed file is never touched")
}

// The daemons the shared-config tests name.
const (
	managedDaemonSocket = "unix:///opt/seamless/run/seamlessd.sock"
	otherDaemonSocket   = "unix:///opt/other/run/seamlessd.sock"
)

// socketClientConfigFile writes a client config of serverURL in a fresh directory.
func socketClientConfigFile(t *testing.T, serverURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client.yaml")
	require.NoError(t, os.WriteFile(path, []byte("role: client\nserver_url: \""+serverURL+"\"\n"), 0o644))
	return path
}

// newSharedConfigWorld is the peer_auth layout of docs-src/guides/peer-auth.md:
// doctor runs with the owner's own client config, while the managed hooks name
// the one client config an administrator wrote for every account. Both reach
// the daemon's socket. It returns the world and the shared config's path.
func newSharedConfigWorld(t *testing.T) (managedHooksWorld, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a socket server_url names a Unix path")
	}
	w := newManagedHooksWorld(t)
	own, err := config.LoadFrom(socketClientConfigFile(t, managedDaemonSocket))
	require.NoError(t, err)
	w.cfg = own
	return w, socketClientConfigFile(t, managedDaemonSocket)
}

// The incident that found this: the managed hooks name the administrator's
// shared client config, doctor runs with the owner's own, and both reach the
// same daemon. Doctor called every event stale although every hook worked.
func TestHooksCheckManagedHooksNamingASharedConfigOfTheSameServer(t *testing.T) {
	w, shared := newSharedConfigWorld(t)
	managed := w.writeManagedNaming(t, claudeManagedSettingsFile, w.seamBin, shared)

	chk := hooksCheck(w.cfg)
	require.Equal(t, statusOK, chk.status, chk.detail)
	require.Equal(t, "definitions in managed settings "+managed+" (current: "+allClaudeEvents+
		"; stale: none; hook config "+shared+" reaches the same server)", chk.detail)
}

// A config the managed hooks name counts only when it loads and reaches
// doctor's server: anything else stays stale, and the row says why.
func TestHooksCheckManagedHooksNamingAConfigThatDoesNotCount(t *testing.T) {
	for name, tc := range map[string]struct {
		spoil  func(t *testing.T, shared string)
		reason string // after "hook config <shared> "
	}{
		"another server": {
			spoil: func(t *testing.T, shared string) {
				require.NoError(t, os.WriteFile(shared, []byte("role: client\nserver_url: \""+otherDaemonSocket+"\"\n"), 0o644))
			},
			reason: "reaches " + otherDaemonSocket + ", not " + managedDaemonSocket,
		},
		"another server, naming a user": {
			spoil: func(t *testing.T, shared string) {
				require.NoError(t, os.WriteFile(shared, []byte("role: client\nserver_url: \"https://agent:hunter2@seamless.example:8081\"\n"), 0o644))
			},
			reason: "reaches https://seamless.example:8081, not " + managedDaemonSocket,
		},
		"missing": {
			spoil:  func(t *testing.T, shared string) { require.NoError(t, os.Remove(shared)) },
			reason: "cannot be read: no such file or directory",
		},
		"does not load": {
			spoil: func(t *testing.T, shared string) {
				require.NoError(t, os.WriteFile(shared, []byte("role: client\nserver_urll: \""+managedDaemonSocket+"\"\n"), 0o644))
			},
			reason: "does not load: yaml: unmarshal errors: line 2: field server_urll not found",
		},
	} {
		t.Run(name, func(t *testing.T) {
			w, shared := newSharedConfigWorld(t)
			managed := w.writeManagedNaming(t, claudeManagedSettingsFile, w.seamBin, shared)
			tc.spoil(t, shared)

			chk := hooksCheck(w.cfg)
			require.Equal(t, statusWarn, chk.status)
			require.Contains(t, chk.detail, "managed settings "+managed+" (current: none; stale: "+allClaudeEvents+")")
			require.Contains(t, chk.detail, "stale in "+managed+": "+allClaudeEvents+
				" (hook config "+shared+" "+tc.reason)
			require.Contains(t, chk.detail, "an administrator must update it")
			require.NotContains(t, chk.detail, "\n", "the row is one line")
			require.NotContains(t, chk.detail, "hunter2")
		})
	}
}

// The same over TCP, where doctor runs with the daemon's own server config and
// the managed hooks name a client config of its address. (This one runs on
// Windows too.)
func TestHooksCheckManagedHooksNamingAClientConfigOfThisServer(t *testing.T) {
	w := newManagedHooksWorld(t)
	shared := filepath.Join(t.TempDir(), "client.yaml")
	require.NoError(t, os.WriteFile(shared, []byte("role: client\nserver_url: \""+w.cfg.ServerURL()+
		"\"\nmcp:\n  api_key: "+w.cfg.MCP.APIKey+"\n"), 0o600))
	managed := w.writeManagedNaming(t, claudeManagedSettingsFile, w.seamBin, shared)

	chk := hooksCheck(w.cfg)
	require.Equal(t, statusOK, chk.status, chk.detail)
	require.Equal(t, "definitions in managed settings "+managed+" (current: "+allClaudeEvents+
		"; stale: none; hook config "+shared+" reaches the same server)", chk.detail)
}

// Over TCP the key counts too: the shared config's seam authenticates with its
// own key, so the right address with another key fails every hook. The key is
// never printed.
func TestHooksCheckManagedHooksNamingAClientConfigOfThisServerWithAnotherKey(t *testing.T) {
	w := newManagedHooksWorld(t)
	shared := filepath.Join(t.TempDir(), "client.yaml")
	const otherKey = "sk-another-key-0123456789abcdef"
	require.NoError(t, os.WriteFile(shared, []byte("role: client\nserver_url: \""+w.cfg.ServerURL()+
		"\"\nmcp:\n  api_key: "+otherKey+"\n"), 0o600))
	managed := w.writeManagedNaming(t, claudeManagedSettingsFile, w.seamBin, shared)

	chk := hooksCheck(w.cfg)
	require.Equal(t, statusWarn, chk.status, chk.detail)
	// UserPromptSubmit is an http hook over TCP, built from this install's own
	// key and naming no config: current either way. Every command hook is not.
	require.Contains(t, chk.detail, "(current: UserPromptSubmit; stale: SessionStart, SessionEnd, PostToolUse, "+
		"SubagentStart, SubagentStop, PermissionRequest)")
	require.Contains(t, chk.detail, "stale in "+managed+": SessionStart, SessionEnd, PostToolUse, SubagentStart, "+
		"SubagentStop, PermissionRequest (hook config "+shared+" reaches the same server with another key; "+
		"not a file install-hooks writes")
	require.NotContains(t, chk.detail, otherKey)
	require.NotContains(t, chk.detail, w.cfg.MCP.APIKey)
}

// The rule is about the config alone: managed hooks running another seam
// executable stay stale even when their config reaches the same server.
func TestHooksCheckManagedHooksNamingASharedConfigButAnotherSeam(t *testing.T) {
	w, shared := newSharedConfigWorld(t)
	other := filepath.Join(t.TempDir(), seamBinName())
	writeTestExecutable(t, other)
	managed := w.writeManagedNaming(t, claudeManagedSettingsFile, other, shared)

	chk := hooksCheck(w.cfg)
	require.Equal(t, statusWarn, chk.status)
	require.Contains(t, chk.detail, "managed settings "+managed+" (current: none; stale: "+allClaudeEvents+")")
	require.Contains(t, chk.detail, "stale in "+managed+": "+allClaudeEvents+
		" (not a file install-hooks writes: an administrator must update it)")
}

// install-hooks judges the managed settings the same way: hooks naming a
// shared config of the same server do not differ from this install, and ones
// naming another server do, with the reason.
func TestInstallClaudeHooksJudgesManagedHooksNamingASharedConfig(t *testing.T) {
	w, shared := newSharedConfigWorld(t)
	w.writeManagedNaming(t, claudeManagedSettingsFile, w.seamBin, shared)
	install := func() string {
		return captureStdout(t, func() error {
			return installClaudeHooks(w.cfg, w.userPath, w.cfg.HookBaseURL(), w.seamBin, w.cfg.SourcePath(), false)
		})
	}

	out := install()
	require.Contains(t, out, "7 managed")
	require.NotContains(t, out, "differ from this install")

	require.NoError(t, os.WriteFile(shared, []byte("role: client\nserver_url: \""+otherDaemonSocket+"\"\n"), 0o644))
	out = install()
	require.Contains(t, out, "differ from this install: "+allClaudeEvents+
		" (hook config "+shared+" reaches "+otherDaemonSocket+", not "+managedDaemonSocket+")")
}
