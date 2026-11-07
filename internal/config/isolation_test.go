package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Access requests at a confidential fence ship off: off is the previous
// behavior, and it is the zero value, so a config that never names the block
// is off.
func TestLoadFrom_IsolationRequestsDefaultOff(t *testing.T) {
	clearSeamlessEnv(t)
	cfg, err := LoadFrom("")
	require.NoError(t, err)
	require.False(t, cfg.Isolation.Requests, "requests ship off")
	require.Equal(t, Isolation{}, Defaults().Isolation)
}

func TestLoadFrom_IsolationRequestsFileAndEnv(t *testing.T) {
	clearSeamlessEnv(t)
	path := writeConfig(t, "isolation:\n  requests: true\n")
	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.True(t, cfg.Isolation.Requests)

	t.Setenv("SEAMLESS_ISOLATION_REQUESTS", "false")
	cfg, err = LoadFrom(path)
	require.NoError(t, err)
	require.False(t, cfg.Isolation.Requests, "env wins over file")

	t.Setenv("SEAMLESS_ISOLATION_REQUESTS", "true")
	cfg, err = LoadFrom("")
	require.NoError(t, err)
	require.True(t, cfg.Isolation.Requests)

	// Present but uninterpretable is an error, never a silent default.
	t.Setenv("SEAMLESS_ISOLATION_REQUESTS", "sometimes")
	_, err = LoadFrom("")
	require.ErrorContains(t, err, "SEAMLESS_ISOLATION_REQUESTS")
}

// An unknown key under the block is refused like any other (KnownFields): a
// typo must not read as the feature being off.
func TestLoadFrom_IsolationUnknownKeyRefused(t *testing.T) {
	clearSeamlessEnv(t)
	path := writeConfig(t, "isolation:\n  request: true\n")
	_, err := LoadFrom(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "request")
}

// The shipped example documents the block, commented out (a release before it
// refuses the key), with its env override.
func TestExampleConfig_DocumentsIsolationRequests(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "seamless.yaml.example"))
	require.NoError(t, err)
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	for _, line := range []string{"# isolation:\n", "#   # env: SEAMLESS_ISOLATION_REQUESTS\n", "#   requests: false\n"} {
		require.Contains(t, text, line)
	}
}
