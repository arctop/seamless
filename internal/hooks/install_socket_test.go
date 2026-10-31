package hooks

import (
	"encoding/json"
	"os"
	pathpkg "path"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const socketBase = "unix:///opt/seamless/run/seamlessd.sock"

// A role account's install reaches its daemon over the peer_auth socket, which
// Claude Code's http hook cannot dial: every hook becomes a `seam hook` command,
// and no key is needed -- or written -- because the daemon names the caller by
// its uid.
func TestInstall_SocketMakesEveryHookACommandWithoutAKey(t *testing.T) {
	require.True(t, IsSocketBaseURL(socketBase))
	require.True(t, CommandOnlyBaseURL(socketBase))
	require.False(t, IsHTTPSBaseURL(socketBase))
	require.False(t, CommandOnlyBaseURL("http://127.0.0.1:8081"))

	path := filepath.Join(t.TempDir(), "settings.json")
	opts := InstallOptions{SettingsPath: path, BaseURL: socketBase, SeamBin: "/opt/seamless/bin/seam", ConfigPath: "/opt/arctop/finance/home/.config/seamless/seamless.yaml"}
	res, err := Install(opts)
	require.NoError(t, err, "a socket install needs no api key")
	require.True(t, res.Changed)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(raw, &settings))
	hooksObj := settings["hooks"].(map[string]any)
	for _, hs := range seamlessHooks {
		requireCommandHook(t, hooksObj, hs.Event, pathpkg.Base(hs.Endpoint))
	}
	require.NotContains(t, string(raw), `"http"`)
	require.NotContains(t, string(raw), "Bearer")

	status, err := InstalledStatus(opts)
	require.NoError(t, err)
	require.Equal(t, installedEvents(t, ClientClaudeCode), status.Current)

	// Without a socket URL the key is still required.
	opts.BaseURL = "http://127.0.0.1:8081"
	_, err = Install(opts)
	require.ErrorContains(t, err, "api key is required")
}
