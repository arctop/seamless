package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Commit records ship off: nothing offered, nothing recorded, no snapshot.
func TestLoadFrom_CommitsDefaultsOff(t *testing.T) {
	clearSeamlessEnv(t)
	cfg, err := LoadFrom("")
	require.NoError(t, err)
	require.False(t, cfg.Commits.Record, "recording ships off")
	require.Empty(t, cfg.Commits.SnapshotFile, "no snapshot file: no snapshot")
	require.Equal(t, DefaultCommitsSnapshotInterval, cfg.Commits.SnapshotInterval)
	require.Equal(t, DefaultCommitsSnapshotWindow, cfg.Commits.SnapshotWindow)
}

func TestLoadFrom_CommitsFileAndEnv(t *testing.T) {
	clearSeamlessEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	path := writeConfig(t, `
commits:
  record: true
  snapshot_file: ~/outbox/snapshot.json
  snapshot_interval: 2m
  snapshot_window: 168h
`)
	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.True(t, cfg.Commits.Record)
	resolvedHome, err := os.UserHomeDir()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(resolvedHome, "outbox", "snapshot.json"), cfg.Commits.SnapshotFile, "a leading ~ expands")
	require.Equal(t, Duration(2*time.Minute), cfg.Commits.SnapshotInterval)
	require.Equal(t, Duration(168*time.Hour), cfg.Commits.SnapshotWindow)

	abs := filepath.Join(t.TempDir(), "elsewhere.json")
	t.Setenv("SEAMLESS_COMMITS_RECORD", "false")
	t.Setenv("SEAMLESS_COMMITS_SNAPSHOT_FILE", abs)
	t.Setenv("SEAMLESS_COMMITS_SNAPSHOT_INTERVAL", "10m")
	t.Setenv("SEAMLESS_COMMITS_SNAPSHOT_WINDOW", "24h")
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.False(t, cfg.Commits.Record, "env wins over file")
	require.Equal(t, abs, cfg.Commits.SnapshotFile)
	require.Equal(t, Duration(10*time.Minute), cfg.Commits.SnapshotInterval)
	require.Equal(t, Duration(24*time.Hour), cfg.Commits.SnapshotWindow)

	// Present but uninterpretable is an error, never a silent default.
	t.Setenv("SEAMLESS_COMMITS_RECORD", "sometimes")
	_, err = LoadFrom("")
	require.ErrorContains(t, err, "SEAMLESS_COMMITS_RECORD")
}

func TestCommitsValidate(t *testing.T) {
	ok := Commits{SnapshotInterval: DefaultCommitsSnapshotInterval, SnapshotWindow: DefaultCommitsSnapshotWindow}
	abs, err := filepath.Abs(filepath.Join("outbox", "snapshot.json"))
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		mutate  func(*Commits)
		wantErr string
	}{
		{"defaults", func(*Commits) {}, ""},
		{"recording without a snapshot", func(c *Commits) { c.Record = true }, ""},
		{"an absolute snapshot file", func(c *Commits) { c.SnapshotFile = abs }, ""},
		{"a relative snapshot file", func(c *Commits) { c.SnapshotFile = "outbox/snapshot.json" }, "commits.snapshot_file"},
		{"a zero interval is present, not off", func(c *Commits) { c.SnapshotInterval = 0 }, "commits.snapshot_interval"},
		{"an interval under the floor", func(c *Commits) { c.SnapshotInterval = Duration(10 * time.Second) }, "commits.snapshot_interval"},
		{"an interval over the ceiling", func(c *Commits) { c.SnapshotInterval = Duration(48 * time.Hour) }, "commits.snapshot_interval"},
		{"a zero window", func(c *Commits) { c.SnapshotWindow = 0 }, "commits.snapshot_window"},
		{"a window over a year", func(c *Commits) { c.SnapshotWindow = Duration(400 * 24 * time.Hour) }, "commits.snapshot_window"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ok
			tc.mutate(&c)
			err := c.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
