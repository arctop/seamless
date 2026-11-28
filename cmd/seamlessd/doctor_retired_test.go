package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// doctor names the retired feature keys a config still carries -- in the file
// and in the environment -- as info, and says nothing when none is set.
func TestRetiredKeysCheck(t *testing.T) {
	noEnv := func(string) (string, bool) { return "", false }

	_, ok := retiredKeysCheck(config.Defaults(), noEnv)
	require.False(t, ok, "nothing retired named: no row")

	path := filepath.Join(t.TempDir(), "seamless.yaml")
	require.NoError(t, os.WriteFile(path, []byte("features:\n  momentum: true\n  gamification: false\n"), 0o600))
	cfg, err := config.LoadFrom(path)
	require.NoError(t, err)
	row, ok := retiredKeysCheck(cfg, noEnv)
	require.True(t, ok)
	require.Equal(t, statusInfo, row.status)
	require.Equal(t, "retired keys", row.name)
	require.Equal(t, "features.momentum and features.gamification in "+path+
		": momentum and gamification are always on, so these switch nothing -- remove them", row.detail)

	env := func(name string) (string, bool) { return "1", name == "SEAMLESS_FEATURES_GAMIFICATION" }
	row, ok = retiredKeysCheck(config.Defaults(), env)
	require.True(t, ok)
	require.Equal(t, "SEAMLESS_FEATURES_GAMIFICATION in the environment"+
		": momentum and gamification are always on, so these switch nothing -- remove them", row.detail)
}
