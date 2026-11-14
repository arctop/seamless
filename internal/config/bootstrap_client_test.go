package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// clientHome is a scratch home with no config anywhere in the search order.
func clientHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, k := range []string{"SEAMLESS_CONFIG", envMCPAPIKey, envServerURL} {
		unsetenvForTest(t, k)
	}
	t.Chdir(t.TempDir())
	return home
}

// A key device's config: role client, the server, this computer's own key
// and, when given, the certificate it trusts the server by -- 0600, loadable,
// and written only where no config is.
func TestEnsureDeviceClientConfig(t *testing.T) {
	home := clientHome(t)
	path, err := EnsureDeviceClientConfig("https://box.example:8081/", "seamless_dk_example", "/h/server-ca.crt")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, ".config", "seamless", "seamless.yaml"), path)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.True(t, cfg.IsClient())
	require.Equal(t, "https://box.example:8081", cfg.ServerURL())
	require.Equal(t, "seamless_dk_example", cfg.MCP.APIKey)
	require.Equal(t, "/h/server-ca.crt", cfg.TLS.CAFile)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(raw), "This computer's own device key")
	require.NotContains(t, string(raw), "copied from it verbatim")

	_, err = EnsureDeviceClientConfig("https://box.example:8081", "seamless_dk_other", "")
	require.ErrorIs(t, err, ErrConfigExists, "an existing config, even of the same server, holds another key")

	clientHome(t)
	t.Setenv(envMCPAPIKey, "k")
	_, err = EnsureDeviceClientConfig("https://box.example:8081", "seamless_dk_example", "")
	require.ErrorContains(t, err, "would stand in for this computer's own key")
}

// A relay Client's config: role client and the relay's socket, no key; a run
// again finds it and writes nothing, and any other config is refused.
func TestEnsureSocketClientConfig(t *testing.T) {
	home := clientHome(t)
	path, err := EnsureSocketClientConfig()
	require.NoError(t, err)
	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.True(t, cfg.IsClient())
	require.Equal(t, "/opt/seamless/run/seamlessd.sock", cfg.SocketPath())
	require.Empty(t, cfg.MCP.APIKey)

	again, err := EnsureSocketClientConfig()
	require.NoError(t, err)
	require.Empty(t, again, "the same config is adopted, nothing written")

	require.NoError(t, os.WriteFile(path, []byte("role: client\nserver_url: \"https://box.example:8081\"\nmcp:\n  api_key: \"k\"\n"), 0o600))
	_, err = EnsureSocketClientConfig()
	require.True(t, errors.Is(err, ErrConfigExists), "%v", err)
	existing, target, err := ClientConfigTarget()
	require.NoError(t, err)
	require.Equal(t, path, existing)
	require.Equal(t, filepath.Join(home, ".config", "seamless", "seamless.yaml"), target)
}
