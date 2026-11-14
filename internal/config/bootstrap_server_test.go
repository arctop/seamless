package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// tlsServerEnv isolates a Server bootstrap test from the developer's own
// install: a scratch home on every OS, and none of the overrides that would
// otherwise reach the files under test.
func tlsServerEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	for _, key := range []string{"SEAMLESS_CONFIG", "SEAMLESS_MCP_API_KEY", "SEAMLESS_SERVER_URL", "SEAMLESS_ROLE", "SEAMLESS_ADDR",
		"SEAMLESS_ALLOWED_HOSTS", "SEAMLESS_TLS_CERT_FILE", "SEAMLESS_TLS_KEY_FILE", "SEAMLESS_TLS_CA_FILE"} {
		unsetenvForTest(t, key)
	}
	return home
}

// tlsServerAddress is the address a tailnet Server's setup decides.
func tlsServerAddress(home string) ServerAddress {
	tls := filepath.Join(home, ".config", "seamless", "tls")
	return ServerAddress{
		Addr:      "100.64.0.7:8081",
		ServerURL: "https://studio.example.ts.net:8081",
		CertFile:  filepath.Join(tls, "studio.example.ts.net.crt"),
		KeyFile:   filepath.Join(tls, "studio.example.ts.net.key"),
	}
}

func TestEnsureServerConfig(t *testing.T) {
	t.Run("a first install writes the address, allowed_hosts and a generated key, 0600", func(t *testing.T) {
		home := tlsServerEnv(t)
		a := tlsServerAddress(home)
		got, path, err := EnsureServerConfig(a)
		require.NoError(t, err)
		want := filepath.Join(home, ".config", "seamless", "seamless.yaml")
		require.Equal(t, want, path)
		require.Equal(t, "100.64.0.7:8081", got.Addr)
		require.Equal(t, "https://studio.example.ts.net:8081", got.ServerURL())
		require.Equal(t, a.CertFile, got.TLS.CertFile)
		require.Equal(t, a.KeyFile, got.TLS.KeyFile)
		require.True(t, got.TLSEnabled())
		require.False(t, got.IsClient())
		require.Len(t, got.MCP.APIKey, 64)
		require.Empty(t, got.AllowedHosts)
		require.True(t, got.HasServerAddress(a))
		if runtime.GOOS != "windows" {
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		}
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Contains(t, string(raw), "allowed_hosts: []\n", "allowed_hosts is written, empty: the name and the bind are all it answers to")
		// The passkey origin follows from it, as task 01M73MX5T07BC741Z6DXM6HHCP derives it.
		rp, origin, ok := got.ServedRelyingParty()
		require.True(t, ok)
		require.Equal(t, "studio.example.ts.net", rp)
		require.Equal(t, "https://studio.example.ts.net:8081", origin)

		reloaded, err := Load()
		require.NoError(t, err)
		require.Equal(t, got.MCP.APIKey, reloaded.MCP.APIKey)
		require.True(t, reloaded.HasServerAddress(a))
	})

	t.Run("allowed_hosts it names are written", func(t *testing.T) {
		home := tlsServerEnv(t)
		a := tlsServerAddress(home)
		a.AllowedHosts = []string{"studio"}
		got, _, err := EnsureServerConfig(a)
		require.NoError(t, err)
		require.Equal(t, []string{"studio"}, got.AllowedHosts)
	})

	t.Run("SEAMLESS_MCP_API_KEY is the key everything beside it presents", func(t *testing.T) {
		home := tlsServerEnv(t)
		t.Setenv("SEAMLESS_MCP_API_KEY", "owner-key")
		_, path, err := EnsureServerConfig(tlsServerAddress(home))
		require.NoError(t, err)
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Contains(t, string(raw), `api_key: "owner-key"`)
	})

	t.Run("a set-but-empty SEAMLESS_MCP_API_KEY writes nothing", func(t *testing.T) {
		home := tlsServerEnv(t)
		t.Setenv("SEAMLESS_MCP_API_KEY", "")
		_, path, err := EnsureServerConfig(tlsServerAddress(home))
		require.ErrorContains(t, err, "SEAMLESS_MCP_API_KEY is set but empty")
		require.Empty(t, path)
		require.NoFileExists(t, filepath.Join(home, ".config", "seamless", "seamless.yaml"))
	})

	t.Run("an existing config is never edited: the error names the lines", func(t *testing.T) {
		home := tlsServerEnv(t)
		path := filepath.Join(home, ".config", "seamless", "seamless.yaml")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		body := "# mine\nmcp:\n  api_key: \"k\"\n"
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		a := tlsServerAddress(home)
		_, wrote, err := EnsureServerConfig(a)
		require.Error(t, err)
		require.Empty(t, wrote)
		require.Contains(t, err.Error(), path+" already exists and is never edited on your behalf")
		require.Contains(t, err.Error(), `  addr: "100.64.0.7:8081"`)
		require.Contains(t, err.Error(), `  server_url: "https://studio.example.ts.net:8081"`)
		require.Contains(t, err.Error(), `    cert_file: "`+a.CertFile+`"`)
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, body, string(raw))
	})

	t.Run("an existing config that says exactly this is adopted, unwritten", func(t *testing.T) {
		home := tlsServerEnv(t)
		a := tlsServerAddress(home)
		_, first, err := EnsureServerConfig(a)
		require.NoError(t, err)
		before, err := os.ReadFile(first)
		require.NoError(t, err)
		got, wrote, err := EnsureServerConfig(a)
		require.NoError(t, err)
		require.Empty(t, wrote)
		require.True(t, got.HasServerAddress(a))
		after, err := os.ReadFile(first)
		require.NoError(t, err)
		require.Equal(t, before, after)
	})

	for name, mutate := range map[string]func(*ServerAddress){
		"no TLS pair":           func(a *ServerAddress) { a.CertFile, a.KeyFile = "", "" },
		"half a pair":           func(a *ServerAddress) { a.KeyFile = "" },
		"an http server_url":    func(a *ServerAddress) { a.ServerURL = "http://studio.example.ts.net:8081" },
		"a wildcard server_url": func(a *ServerAddress) { a.ServerURL = "https://0.0.0.0:8081" },
		"a socket server_url":   func(a *ServerAddress) { a.ServerURL = "unix:///opt/seamless/run/seamlessd.sock" },
		"no addr":               func(a *ServerAddress) { a.Addr = "" },
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			home := tlsServerEnv(t)
			a := tlsServerAddress(home)
			mutate(&a)
			_, wrote, err := EnsureServerConfig(a)
			require.Error(t, err)
			require.Empty(t, wrote)
			require.NoFileExists(t, filepath.Join(home, ".config", "seamless", "seamless.yaml"))
		})
	}
}

func TestServerAddressLines(t *testing.T) {
	a := ServerAddress{Addr: "0.0.0.0:8081", ServerURL: "https://studio.lan:8081", CertFile: "/c.crt", KeyFile: "/c.key"}
	require.Equal(t, "  addr: \"0.0.0.0:8081\"\n  server_url: \"https://studio.lan:8081\"\n  tls:\n"+
		"    cert_file: \"/c.crt\"\n    key_file: \"/c.key\"\n", a.Lines("  "))
	a.AllowedHosts = []string{"studio"}
	require.Contains(t, a.Lines(""), "allowed_hosts:\n  - \"studio\"\n")
	// The lines load as a config, every value as written.
	dir := t.TempDir()
	path := filepath.Join(dir, "seamless.yaml")
	require.NoError(t, os.WriteFile(path, []byte(a.Lines("")), 0o600))
	tlsServerEnv(t)
	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.True(t, cfg.HasServerAddress(a))
	require.False(t, cfg.HasServerAddress(ServerAddress{Addr: "0.0.0.0:8081", ServerURL: "https://studio.lan:8081", CertFile: "/c.crt", KeyFile: "/c.key"}),
		"allowed_hosts differ")
	require.False(t, strings.Contains(a.Lines(""), "api_key"), "the lines never carry the key")
}
