package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/hooks"
	agentskills "github.com/arctop/seamless/internal/skills"
	"github.com/arctop/seamless/internal/update"
)

// shapeFixture is an installer-managed install under a temp HOME: the
// binaries in ~/.local/bin and the config at the installer's path.
type shapeFixture struct {
	env        shapeEnv
	dir, exe   string
	cfg        config.Config
	configPath string
	svc        update.ServiceProbe
}

func newShapeFixture(t *testing.T, goos string, environ ...string) shapeFixture {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "bin")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	exe := filepath.Join(dir, "seamlessd")
	require.NoError(t, os.WriteFile(exe, []byte("seamlessd"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "seam"), []byte("seam"), 0o755))
	configPath := filepath.Join(home, ".config", "seamless", "seamless.yaml")
	cfg := config.Defaults()
	cfg.MCP.APIKey = "test-key"
	return shapeFixture{
		env: shapeEnv{goos: goos, home: home, environ: append([]string{"HOME=" + home}, environ...), sameFile: sameFile},
		dir: dir, exe: exe, cfg: cfg, configPath: configPath,
		svc: update.ServiceProbe{Path: "the service", Found: true, Marker: true, Program: exe, RunsThisExe: true},
	}
}

// wire installs Seamless's hooks for client into path, the way install-hooks
// does, with seam at seamBin.
func (f shapeFixture) wire(t *testing.T, client hooks.Client, path, seamBin string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	_, err := hooks.Install(hooks.InstallOptions{
		Client: client, SettingsPath: path, BaseURL: f.cfg.ServerURL(), APIKey: f.cfg.MCP.APIKey,
		SeamBin: seamBin, ConfigPath: f.configPath,
	})
	require.NoError(t, err)
}

func TestInstallDirFor(t *testing.T) {
	t.Run("the service, the binary and the hooks agree", func(t *testing.T) {
		f := newShapeFixture(t, "linux")
		f.wire(t, hooks.ClientClaudeCode, f.env.claudeSettingsPath(), filepath.Join(f.dir, "seam"))
		dir, err := installDirFor(f.env, f.svc, f.exe)
		require.NoError(t, err)
		require.Equal(t, f.dir, dir)
	})
	t.Run("the service runs a binary in another directory", func(t *testing.T) {
		f := newShapeFixture(t, "linux")
		other := filepath.Join(f.env.home, "opt", "seamlessd")
		require.NoError(t, os.MkdirAll(filepath.Dir(other), 0o700))
		require.NoError(t, os.WriteFile(other, nil, 0o755))
		f.svc.Program = other
		_, err := installDirFor(f.env, f.svc, f.exe)
		require.ErrorIs(t, err, errShapeMismatch)
	})
	t.Run("the service names no absolute program", func(t *testing.T) {
		f := newShapeFixture(t, "linux")
		f.svc.Program = "seamlessd"
		_, err := installDirFor(f.env, f.svc, f.exe)
		require.ErrorIs(t, err, errShapeMismatch)
	})
	t.Run("the Codex hooks run seam from elsewhere", func(t *testing.T) {
		f := newShapeFixture(t, "linux")
		elsewhere := filepath.Join(f.env.home, "old", "bin")
		require.NoError(t, os.MkdirAll(elsewhere, 0o700))
		f.wire(t, hooks.ClientCodex, f.env.codexHooksPath(), filepath.Join(elsewhere, "seam"))
		_, err := installDirFor(f.env, f.svc, f.exe)
		require.ErrorIs(t, err, errShapeMismatch)
		require.Contains(t, err.Error(), "Codex")
	})
}

func TestObserveClients_ObservedNeverAdded(t *testing.T) {
	t.Run("nothing wired is SEAMLESS_NO_HOOKS", func(t *testing.T) {
		f := newShapeFixture(t, "darwin")
		// A ~/.claude directory alone is detection, not wiring: an update
		// must not add Claude Code to an install that never wired it.
		require.NoError(t, os.MkdirAll(filepath.Join(f.env.home, ".claude"), 0o700))
		s, err := observeInstallShape(f.env, f.cfg, f.configPath, f.svc, f.exe)
		require.NoError(t, err)
		require.Empty(t, s.clients)
		k, err := s.knobs()
		require.NoError(t, err)
		require.Equal(t, map[string]string{"SEAMLESS_INSTALL_DIR": f.dir, "SEAMLESS_NO_HOOKS": "1"}, k)
	})
	t.Run("every wired client, in the installer's order", func(t *testing.T) {
		f := newShapeFixture(t, "darwin")
		seam := filepath.Join(f.dir, "seam")
		f.wire(t, hooks.ClientCodex, f.env.codexHooksPath(), seam)
		f.wire(t, hooks.ClientClaudeCode, f.env.claudeSettingsPath(), seam)
		desktop, err := claudeDesktopConfigPathFor("darwin", f.env.home, "")
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(desktop), 0o700))
		_, err = reconcileClaudeDesktopMCP(desktop, seam, f.configPath)
		require.NoError(t, err)

		clients, notes := observeClients(f.env, f.cfg, f.dir, f.configPath)
		require.Empty(t, notes)
		require.Equal(t, []string{shapeClaude, shapeClaudeDesktop, shapeCodex}, clients)
	})
	t.Run("stale hooks still count as wired", func(t *testing.T) {
		f := newShapeFixture(t, "linux")
		// Wired by an older install against another base URL: owned, stale.
		require.NoError(t, os.MkdirAll(filepath.Dir(f.env.claudeSettingsPath()), 0o700))
		_, err := hooks.Install(hooks.InstallOptions{
			Client: hooks.ClientClaudeCode, SettingsPath: f.env.claudeSettingsPath(), BaseURL: "http://127.0.0.1:9999",
			APIKey: "old", SeamBin: filepath.Join(f.dir, "seam"), ConfigPath: f.configPath,
		})
		require.NoError(t, err)
		clients, _ := observeClients(f.env, f.cfg, f.dir, f.configPath)
		require.Equal(t, []string{shapeClaude}, clients)
	})
	t.Run("Codex is observed where CODEX_HOME puts it", func(t *testing.T) {
		codexHome := filepath.Join(t.TempDir(), "codex-home")
		f := newShapeFixture(t, "linux", "CODEX_HOME="+codexHome)
		require.Equal(t, filepath.Join(codexHome, "hooks.json"), f.env.codexHooksPath())
		f.wire(t, hooks.ClientCodex, filepath.Join(codexHome, "hooks.json"), filepath.Join(f.dir, "seam"))
		clients, _ := observeClients(f.env, f.cfg, f.dir, f.configPath)
		require.Equal(t, []string{shapeCodex}, clients)

		// The same hooks under ~/.codex are not this install's when
		// CODEX_HOME points elsewhere.
		g := newShapeFixture(t, "linux", "CODEX_HOME="+filepath.Join(t.TempDir(), "empty"))
		g.wire(t, hooks.ClientCodex, filepath.Join(g.env.home, ".codex", "hooks.json"), filepath.Join(g.dir, "seam"))
		clients, _ = observeClients(g.env, g.cfg, g.dir, g.configPath)
		require.Empty(t, clients)
	})
	t.Run("a foreign desktop entry is not ours", func(t *testing.T) {
		f := newShapeFixture(t, "darwin")
		desktop, err := claudeDesktopConfigPathFor("darwin", f.env.home, "")
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(desktop), 0o700))
		require.NoError(t, os.WriteFile(desktop, []byte(`{"mcpServers":{"seamless":{"command":"/usr/bin/other-tool"}}}`), 0o600))
		clients, notes := observeClients(f.env, f.cfg, f.dir, f.configPath)
		require.Empty(t, clients)
		require.Len(t, notes, 1)
	})
	t.Run("hooks in Claude Code's managed settings wire Claude Code", func(t *testing.T) {
		f := newShapeFixture(t, "darwin")
		f.env.claudeManagedDir = t.TempDir()
		f.wire(t, hooks.ClientClaudeCode, filepath.Join(f.env.claudeManagedDir, claudeManagedSettingsFile), filepath.Join(f.dir, "seam"))
		clients, notes := observeClients(f.env, f.cfg, f.dir, f.configPath)
		require.Empty(t, notes)
		require.Equal(t, []string{shapeClaude}, clients, "an update must keep refreshing Claude Code's skills and MCP")

		// An unreadable managed file wires nothing, and says why.
		require.NoError(t, os.WriteFile(filepath.Join(f.env.claudeManagedDir, claudeManagedSettingsFile), []byte("{"), 0o600))
		clients, notes = observeClients(f.env, f.cfg, f.dir, f.configPath)
		require.Empty(t, clients)
		require.Len(t, notes, 1)
		require.Contains(t, notes[0], "cannot inspect managed settings")
	})
	t.Run("an unreadable settings file leaves the client out", func(t *testing.T) {
		f := newShapeFixture(t, "linux")
		require.NoError(t, os.MkdirAll(filepath.Dir(f.env.claudeSettingsPath()), 0o700))
		require.NoError(t, os.WriteFile(f.env.claudeSettingsPath(), []byte("{not json"), 0o600))
		clients, notes := observeClients(f.env, f.cfg, f.dir, f.configPath)
		require.Empty(t, clients)
		require.Len(t, notes, 1)
		require.Contains(t, notes[0], "Claude Code left out")
	})
}

func TestSkillOptOuts_EverySkillEveryWiredClientHas(t *testing.T) {
	f := newShapeFixture(t, "linux")
	opts := agentskills.Options{HomeDir: f.env.home}
	for _, c := range []agentskills.Client{agentskills.ClientClaude, agentskills.ClientCodex} {
		_, err := agentskills.Install(c, opts)
		require.NoError(t, err)
	}
	both := []string{shapeClaude, shapeCodex}

	skip, notes := skillOptOuts(f.env, both)
	require.Empty(t, notes)
	require.Empty(t, skip, "delivered everywhere: refresh both")

	// The owner removed seam-research from Codex only. Refreshing Claude's
	// would put it back into Codex too (the opt-out is process-wide), so the
	// update leaves it alone everywhere.
	codexRoot, err := agentskills.Root(agentskills.ClientCodex, opts)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Join(codexRoot, agentskills.ResearchName)))
	skip, _ = skillOptOuts(f.env, both)
	require.Equal(t, []string{agentskills.ResearchName}, skip)

	// seam-onboard ran and removed itself, leaving its marker: still
	// delivered, since a re-run does not reinstall it.
	claudeRoot, err := agentskills.Root(agentskills.ClientClaude, opts)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Join(claudeRoot, agentskills.OnboardName)))
	require.FileExists(t, filepath.Join(claudeRoot, agentskills.OnboardMarker))
	skip, _ = skillOptOuts(f.env, both)
	require.Equal(t, []string{agentskills.ResearchName}, skip)

	// The Claude app gets no skills, and no hook client means no knobs.
	skip, _ = skillOptOuts(f.env, []string{shapeClaudeDesktop})
	require.Empty(t, skip)
}

func TestSkillOptOutKnob_DerivesFromInstallerKnobs(t *testing.T) {
	for _, name := range agentskills.Published() {
		knob, err := skillOptOutKnob(name)
		require.NoError(t, err, name)
		require.Contains(t, installerKnobs, knob)
		require.Contains(t, inheritedKnobs, knob, "install-hooks, not the installer, reads %s", knob)
	}
	_, err := skillOptOutKnob("seam-unknown")
	require.Error(t, err)
	_, err = skillOptOutKnob("onboard")
	require.Error(t, err)
}

func TestUnattendedInstallerEnv(t *testing.T) {
	inherited := []string{
		"PATH=/usr/bin:/bin", "HOME=/home/u", "TMPDIR=/tmp/u", "XDG_RUNTIME_DIR=/run/user/1000",
		"HTTPS_PROXY=http://proxy:3128", "CODEX_HOME=/home/u/cx",
		"SEAMLESS_CONFIG=/home/u/.config/seamless/seamless.yaml", "SEAMLESS_MCP_API_KEY=secret",
		"SEAMLESS_SERVER_URL=https://elsewhere", "SEAMLESS_VERSION=0.1.0", "SEAMLESS_NO_SERVICE=1",
	}
	knobs := map[string]string{
		"SEAMLESS_VERSION": "0.7.3", "SEAMLESS_INSTALL_DIR": "/home/u/.local/bin",
		"SEAMLESS_CHECKSUMS_SHA256": strings.Repeat("a", 64), "SEAMLESS_CLIENT": "claude,codex",
		"SEAMLESS_NO_RESEARCH_SKILL": "1",
	}
	env, err := unattendedInstallerEnv("linux", inherited, knobs)
	require.NoError(t, err)
	require.Equal(t, []string{
		"PATH=/usr/bin:/bin", "HOME=/home/u", "TMPDIR=/tmp/u", "XDG_RUNTIME_DIR=/run/user/1000",
		"HTTPS_PROXY=http://proxy:3128", "CODEX_HOME=/home/u/cx",
		// installerKnobs order, nothing else SEAMLESS_*.
		"SEAMLESS_VERSION=0.7.3", "SEAMLESS_INSTALL_DIR=/home/u/.local/bin",
		"SEAMLESS_CHECKSUMS_SHA256=" + strings.Repeat("a", 64), "SEAMLESS_CLIENT=claude,codex",
		"SEAMLESS_NO_RESEARCH_SKILL=1",
	}, env)

	// Windows names are case-insensitive: a lower-case one is dropped too.
	env, err = unattendedInstallerEnv("windows", []string{"Path=C:\\Windows", "seamless_config=x", "Seamless_Mcp_Api_Key=y"},
		map[string]string{"SEAMLESS_VERSION": "0.7.3"})
	require.NoError(t, err)
	require.Equal(t, []string{"Path=C:\\Windows", "SEAMLESS_VERSION=0.7.3"}, env)

	_, err = unattendedInstallerEnv("linux", nil, map[string]string{"SEAMLESS_DATA_DIR": "/x"})
	require.ErrorContains(t, err, "not in installerKnobs")
}

func TestAttendedInstallerEnv_TheOwnersKnobsWin(t *testing.T) {
	inherited := []string{"PATH=/bin", "SEAMLESS_VERSION=0.7.1", "SEAMLESS_INSTALL_DIR=", "SEAMLESS_CLIENT=codex"}
	knobs := map[string]string{"SEAMLESS_VERSION": "0.7.1", "SEAMLESS_INSTALL_DIR": "/opt/bin", "SEAMLESS_CHECKSUMS_SHA256": "pin"}

	env, err := attendedInstallerEnv("linux", inherited, knobs)
	require.NoError(t, err)
	got := envOf(t, env)
	require.Equal(t, "0.7.1", got["SEAMLESS_VERSION"])
	require.Equal(t, "/opt/bin", got["SEAMLESS_INSTALL_DIR"], "an empty value is unset, as the installers read it")
	require.Equal(t, "pin", got["SEAMLESS_CHECKSUMS_SHA256"])
	require.Equal(t, "codex", got["SEAMLESS_CLIENT"])

	// A rollback forces its own release over the owner's pin.
	env, err = attendedInstallerEnv("linux", inherited,
		map[string]string{"SEAMLESS_VERSION": "0.7.2", "SEAMLESS_CHECKSUMS_SHA256": "back"},
		"SEAMLESS_VERSION", "SEAMLESS_CHECKSUMS_SHA256")
	require.NoError(t, err)
	got = envOf(t, env)
	require.Equal(t, "0.7.2", got["SEAMLESS_VERSION"])
	require.Equal(t, "back", got["SEAMLESS_CHECKSUMS_SHA256"])
	require.True(t, slices.Contains(env, "PATH=/bin"))
}

func TestLookupEnvIn(t *testing.T) {
	env := []string{"A=1", "B=", "A=2", "c=3"}
	v, ok := lookupEnvIn("linux", env, "A")
	require.True(t, ok)
	require.Equal(t, "2", v, "the last value wins")
	_, ok = lookupEnvIn("linux", env, "B")
	require.False(t, ok, "empty is unset")
	_, ok = lookupEnvIn("linux", env, "C")
	require.False(t, ok)
	v, ok = lookupEnvIn("windows", env, "C")
	require.True(t, ok)
	require.Equal(t, "3", v)
}
