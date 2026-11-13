package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// searchWorld is a machine with a throwaway home and working directory and none
// of the SEAMLESS_* variables that decide the search or override what it finds.
// It returns the home config's path and the working directory.
func searchWorld(t *testing.T) (homeConfig, cwd string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, key := range []string{"SEAMLESS_CONFIG", "SEAMLESS_MCP_API_KEY", "SEAMLESS_SERVER_URL", "SEAMLESS_ROLE", "SEAMLESS_ADDR"} {
		unsetenvForTest(t, key)
	}
	cwd = t.TempDir()
	t.Chdir(cwd)
	return filepath.Join(home, ".config", "seamless", "seamless.yaml"), cwd
}

func writeSearchFile(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

// hostileWorkingDirConfig is a seamless.yaml committed to a repository: it
// names another server and another key.
const hostileWorkingDirConfig = "role: client\nserver_url: \"https://elsewhere.example:8443\"\nmcp:\n  api_key: \"planted\"\n"

// The two searches differ in exactly one step: the daemon's reads the working
// directory last, the client's never does.
func TestLoadAndLoadClient_SearchOrder(t *testing.T) {
	tests := []struct {
		name string
		// home and env say which of the other two places hold a file.
		home, env bool
		// envNamesWorkingDir points $SEAMLESS_CONFIG at ./seamless.yaml itself.
		envNamesWorkingDir bool
		wantLoad           string // "home", "env", "cwd" or "" (defaults)
		wantClient         string
	}{
		{name: "only the working directory: the daemon reads it, a client does not", wantLoad: "cwd", wantClient: ""},
		{name: "the home config wins over the working directory for both", home: true, wantLoad: "home", wantClient: "home"},
		{name: "$SEAMLESS_CONFIG wins for both", home: true, env: true, wantLoad: "env", wantClient: "env"},
		{name: "$SEAMLESS_CONFIG may name the working directory's file", envNamesWorkingDir: true, wantLoad: "named", wantClient: "named"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			homeConfig, cwd := searchWorld(t)
			writeSearchFile(t, filepath.Join(cwd, WorkingDirConfig), hostileWorkingDirConfig)
			if tt.home {
				writeSearchFile(t, homeConfig, "mcp:\n  api_key: \"home-key\"\n")
			}
			envConfig := filepath.Join(t.TempDir(), "elsewhere.yaml")
			if tt.env {
				writeSearchFile(t, envConfig, "mcp:\n  api_key: \"env-key\"\n")
				t.Setenv("SEAMLESS_CONFIG", envConfig)
			}
			if tt.envNamesWorkingDir {
				t.Setenv("SEAMLESS_CONFIG", WorkingDirConfig)
			}
			where := map[string]string{"home": homeConfig, "env": envConfig, "cwd": WorkingDirConfig, "named": WorkingDirConfig, "": ""}

			daemon, err := Load()
			require.NoError(t, err)
			require.Equal(t, where[tt.wantLoad], daemon.SourcePath())
			require.Equal(t, tt.wantLoad == "cwd", daemon.FromWorkingDir(),
				"only the search's own last step is the working directory's file; a file named outright is not")

			client, err := LoadClient()
			require.NoError(t, err)
			require.Equal(t, where[tt.wantClient], client.SourcePath())
			require.False(t, client.FromWorkingDir())
			if tt.wantClient == "" {
				require.Equal(t, Defaults().ServerURL(), client.ServerURL(), "nothing found is the defaults, never the planted server")
				require.Empty(t, client.MCP.APIKey)
				require.False(t, client.IsClient())
			}
		})
	}
}

// A $SEAMLESS_CONFIG that names nothing is an error for both searches: the
// caller said where the file is, so falling through to another one would be a
// guess.
func TestLoadAndLoadClient_MissingNamedFileIsAnError(t *testing.T) {
	_, cwd := searchWorld(t)
	writeSearchFile(t, filepath.Join(cwd, WorkingDirConfig), hostileWorkingDirConfig)
	t.Setenv("SEAMLESS_CONFIG", filepath.Join(t.TempDir(), "missing.yaml"))

	_, err := Load()
	require.ErrorContains(t, err, "SEAMLESS_CONFIG=")
	_, err = LoadClient()
	require.ErrorContains(t, err, "SEAMLESS_CONFIG=")
}

// The first-run bootstraps run in seamlessd and keep the daemon's search: a
// ./seamless.yaml is still a config file in the search order, so neither one
// writes a home config around it (memory first-run-key-bootstrap-rules).
func TestBootstraps_StillSeeTheWorkingDirectoryConfig(t *testing.T) {
	homeConfig, cwd := searchWorld(t)
	writeSearchFile(t, filepath.Join(cwd, WorkingDirConfig), "mcp:\n  api_key: \"\"\n")

	cfg, err := Load()
	require.NoError(t, err)
	_, wrote, err := EnsureAPIKey(cfg)
	require.NoError(t, err)
	require.Empty(t, wrote, "an existing ./seamless.yaml is an owner-authored file: no key is generated around it")
	require.NoFileExists(t, homeConfig)

	_, wrote, err = EnsureClientConfig("https://server.example:8081", "server-key")
	require.ErrorContains(t, err, WorkingDirConfig+" already exists and is never edited")
	require.Empty(t, wrote)
	require.NoFileExists(t, homeConfig)
}
