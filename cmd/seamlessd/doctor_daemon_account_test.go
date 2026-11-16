package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// The server report's agent client rows: with peer_auth off they are today's,
// row for row; with it on the config is the daemon's, and one row stands for
// them, naming no path.
func TestAgentClientChecks_PeerAuthDaemonConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir())
	for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "SEAMLESS_CONFIG"} {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}
	cfg := config.Defaults()
	d := doctorDB{why: "no database yet"}

	off := agentClientChecks(cfg, d)
	names := checkNames(off)
	for _, want := range []string{"hooks", "codex", "feature skills"} {
		require.Contains(t, names, want)
	}
	require.NotContains(t, names, daemonConfigClientsRow.name)
	require.Equal(t, check{statusInfo, "feature skills", "skipped: no database yet"}, findCheck(t, off, "feature skills"))

	cfg.PeerAuth.Enabled = true
	require.Equal(t, []check{daemonConfigClientsRow}, agentClientChecks(cfg, d))
	require.NotContains(t, daemonConfigClientsRow.detail, "/", "the row names no path the reader may not be able to use")
	require.NotContains(t, daemonConfigClientsRow.detail, "install-hooks", "nothing to install from the daemon's config")
}
