package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A server_url that names a user (https://user:password@host) still loads:
// `seamlessd install-hooks --server-url` (EnsureClientConfig) writes one
// into a client config, and an install it wrote must keep loading. It is
// named (ServerURLNamesUser) for doctor's warning instead. A bare address, a
// socket and an unset server_url name none.
func TestFdev_ServerURLNamesUser(t *testing.T) {
	clearSeamlessEnv(t)
	for raw, want := range map[string]bool{
		"https://user:pw@example-server.example:8081": true,
		"http://user@192.0.2.10:8081":                 true,
		"https://@example-server.example:8081":        true,
		" HTTPS://user:pw@example-server.example/":    true,
		"https://example-server.example:8081":         false,
		"http://127.0.0.1:8081":                       false,
		"":                                            false,
	} {
		c := Defaults()
		c.AdvertisedURL = raw
		require.NoError(t, c.Validate(), "%q loads", raw)
		require.Equal(t, want, c.ServerURLNamesUser(), "%q", raw)
	}
	// A socket names no user. Not validated here: a Unix path is not an
	// absolute path on Windows, where this test runs too.
	require.False(t, Config{AdvertisedURL: "unix:///opt/seamless/run/seamlessd.sock"}.ServerURLNamesUser())

	path := filepath.Join(t.TempDir(), "seamless.yaml")
	require.NoError(t, os.WriteFile(path, []byte("role: client\nserver_url: \"https://user:pw@example-server.example:8081\"\n"+
		"mcp:\n  api_key: \"example-key\"\n"), 0o600))
	loaded, err := LoadFrom(path)
	require.NoError(t, err, "a client config install-hooks wrote keeps loading")
	require.True(t, loaded.ServerURLNamesUser())
	require.Equal(t, "example-server.example", loaded.ServerHost())
}
