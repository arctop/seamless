package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// clearSeamlessEnv unsets every SEAMLESS_* variable the developer's shell
// carries, so a test comparing against Defaults() is not comparing against
// their environment instead.
func clearSeamlessEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "SEAMLESS_") {
			unsetEnv(t, k)
		}
	}
}

// TestExampleConfigLoads loads the shipped seamless.yaml.example through the
// real loader. `make install` seeds a fresh config from it and the docs print
// it verbatim, so it must load -- KnownFields turns a key renamed in code but
// not here into a daemon that will not start -- and the promise in its header,
// "every key, at its default", must hold.
func TestExampleConfigLoads(t *testing.T) {
	clearSeamlessEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows

	path := filepath.Join("..", "..", "seamless.yaml.example")
	cfg, err := LoadFrom(path)
	require.NoError(t, err)

	// The update block: check_interval, max_defer and min_age are spelled out
	// at their defaults, and check and auto are only comments, so each is
	// unset exactly as its default is. Every key and its env override is
	// written out, set or not.
	require.Equal(t, Defaults().Update, cfg.Update)
	require.Nil(t, cfg.Update.Check, "check is commented out in the example: unset, the build decides")
	require.Nil(t, cfg.Update.Auto, "auto is commented out in the example: unset, which means on")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	for _, line := range []string{
		"  # check: false\n", "  check_interval: 6h\n", "  # auto: false\n", "  max_defer: 24h\n", "  min_age: 24h\n",
		"  # env: SEAMLESS_UPDATE_CHECK\n", "  # env: SEAMLESS_UPDATE_CHECK_INTERVAL\n", "  # env: SEAMLESS_UPDATE_AUTO\n",
		"  # env: SEAMLESS_UPDATE_MAX_DEFER\n", "  # env: SEAMLESS_UPDATE_MIN_AGE\n",
		"  # env: SEAMLESS_UPDATE_RELEASE_SIGNERS\n", "  # release_signers: /opt/seamless/etc/release_signers\n",
	} {
		require.Contains(t, text, line)
	}

	// The backup block is documented but commented out whole: a backup: key
	// pins the file to a release that knows the block, and a rollback to an
	// older one would refuse it (AGENTS.md "Release discipline").
	require.Equal(t, Defaults().Backup, cfg.Backup)
	require.NotContains(t, "\n"+text, "\nbackup:", "the backup block stays commented out")
	for _, line := range []string{
		"# backup:\n", "#   auto: false\n", "#   every: 24h\n", "#   keep: 7\n", "#   dir: \"\"\n",
		"#   # env: SEAMLESS_BACKUP_AUTO\n", "#   # env: SEAMLESS_BACKUP_EVERY\n",
		"#   # env: SEAMLESS_BACKUP_KEEP\n", "#   # env: SEAMLESS_BACKUP_DIR\n",
	} {
		require.Contains(t, text, line)
	}

	// Every other key is at its default too, modulo what loading legitimately
	// does to a default: data_dir's leading ~ expands, and the example's
	// `allowed_hosts: []` decodes to an empty list where the default is nil --
	// both mean "no extra hosts".
	resolvedHome, err := os.UserHomeDir()
	require.NoError(t, err)
	want := Defaults()
	want.DataDir = filepath.Join(resolvedHome, ".seamless")
	want.sourcePath = path
	require.Empty(t, cfg.AllowedHosts)
	cfg.AllowedHosts = nil
	require.Equal(t, want, cfg)
}

// TestExampleConfig_BackupBlockLoadsUncommented uncomments the example's
// backup block and loads it, so the values it shows stay ones the daemon
// accepts: the defaults, with auto spelled out as the opt-out it documents.
func TestExampleConfig_BackupBlockLoadsUncommented(t *testing.T) {
	isolateUpdateEnv(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "seamless.yaml.example"))
	require.NoError(t, err)
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")

	_, block, found := strings.Cut(text, "\n# backup:\n")
	require.True(t, found, "the example documents the backup block")
	block, _, _ = strings.Cut(block, "\n\n")
	var b strings.Builder
	b.WriteString("backup:\n")
	for line := range strings.SplitSeq(block, "\n") {
		require.True(t, strings.HasPrefix(line, "#   "), "every line of the block is commented: %q", line)
		b.WriteString(strings.TrimPrefix(line, "# ") + "\n")
	}

	cfg, err := LoadFrom(writeConfig(t, b.String()))
	require.NoError(t, err)
	want := Defaults().Backup
	want.Auto = new(false)
	require.Equal(t, want, cfg.Backup)
}
