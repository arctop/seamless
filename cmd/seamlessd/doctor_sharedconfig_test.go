package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
)

// socketClient is a peer_auth client config: no key, nothing secret.
const socketClient = "role: client\nserver_url: \"unix:///opt/seamless/run/seamlessd.sock\"\n"

// daemonPin is root's pin of the daemon's account.
var daemonPin = access.DaemonPin{Name: "_seamless", UID: 480, Path: access.DaemonAccountPath}

// rootWorld inspects the real paths but reports every one owned by root,
// then applies the per-path overrides: a test plays a root-owned tree without
// being root, and the modes stay the real ones the test set.
func rootWorld(overrides map[string]func(*fileFacts)) func(string) (fileFacts, error) {
	return func(p string) (fileFacts, error) {
		f, err := inspectPath(p)
		if err != nil {
			return f, err
		}
		f.uid = 0
		if o, ok := overrides[filepath.Clean(p)]; ok {
			o(&f)
		}
		return f, nil
	}
}

func ownedBy(uid uint32) func(*fileFacts) { return func(f *fileFacts) { f.uid = uid } }

// configAt writes body as client.yaml in a fresh directory of the given
// mode, with the file at fileMode, and loads it.
func configAt(t *testing.T, body string, dirMode, fileMode os.FileMode) (config.Config, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "etc")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.Chmod(dir, dirMode))
	path := filepath.Join(dir, "client.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	require.NoError(t, os.Chmod(path, fileMode))
	cfg, err := config.LoadFrom(path)
	require.NoError(t, err)
	return cfg, path
}

// Every ownership and content case of the config permissions row. A client
// config holding no secret, owned by root or the pinned daemon account and
// writable by no one else, is an info row; anything holding a key keeps the
// owner-only rule.
func TestConfigFileCheck_OwnershipAndContent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix ownership and permission-bit cases")
	}
	for _, key := range []string{"SEAMLESS_ROLE", "SEAMLESS_SERVER_URL", "SEAMLESS_MCP_API_KEY"} {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}
	pinned := func() (access.DaemonPin, bool, error) { return daemonPin, true, nil }
	noPin := func() (access.DaemonPin, bool, error) { return access.DaemonPin{}, false, nil }
	badPin := func() (access.DaemonPin, bool, error) {
		return access.DaemonPin{}, true, errors.New("the daemon account pin is owned by uid 501")
	}

	tests := []struct {
		name     string
		body     string
		dirMode  os.FileMode
		fileMode os.FileMode
		pin      func() (access.DaemonPin, bool, error)
		// fileUID and dirUID own the file and its directory; 0, root, is what
		// every other path reports.
		fileUID, dirUID uint32
		status          checkStatus
		want            []string
		dont            []string
	}{
		{
			name: "root-owned 0644 with no secret: shared, info", body: socketClient, dirMode: 0o755, fileMode: 0o644, pin: noPin,
			status: statusInfo, want: []string{"holds no secret", "owned by root, mode 0644", "only root can write", "no other account can redirect"},
		},
		{
			name: "owned by the pinned daemon account: shared, info", body: socketClient, dirMode: 0o755, fileMode: 0o644, pin: pinned,
			fileUID: 480,
			status:  statusInfo, want: []string{"owned by the daemon's account _seamless (uid 480)", "only root and the daemon's account can write"},
		},
		{
			name: "root-owned 0600 is shared too, if only root reads it", body: socketClient, dirMode: 0o755, fileMode: 0o600, pin: noPin,
			status: statusInfo, want: []string{"owned by root, mode 0600"},
		},
		{
			name: "root-owned but group-writable: warn", body: socketClient, dirMode: 0o755, fileMode: 0o664, pin: noPin,
			status: statusWarn, want: []string{"holds no secret", "mode 0664 lets accounts other than root rewrite it", "sudo chmod 644 '"},
		},
		{
			name: "root-owned under a directory another account owns: warn", body: socketClient, dirMode: 0o755, fileMode: 0o644, pin: noPin,
			dirUID: 501,
			status: statusWarn, want: []string{"above it is owned by uid 501", "such as /opt/seamless/etc"},
		},
		{
			name: "root-owned under a world-writable directory: warn", body: socketClient, dirMode: 0o777, fileMode: 0o644, pin: noPin,
			status: statusWarn, want: []string{"above it is mode 0777"},
		},
		{
			name: "root-owned under a sticky world-writable directory: info", body: socketClient, dirMode: 0o777 | os.ModeSticky, fileMode: 0o644, pin: noPin,
			status: statusInfo, want: []string{"owned by root"},
		},
		{
			name: "owned by another account: the owner-only rule, naming the shared shape", body: socketClient, dirMode: 0o755, fileMode: 0o644, pin: noPin,
			fileUID: 501,
			status:  statusWarn, want: []string{"mode 0644 grants group/other access", "it holds no secret, so it may instead be a shared client config"},
		},
		{
			name: "owned by uid 480 with no pin: not the daemon's account", body: socketClient, dirMode: 0o755, fileMode: 0o644, pin: noPin,
			fileUID: 480,
			status:  statusWarn, want: []string{"mode 0644 grants group/other access"},
		},
		{
			name: "owned by uid 480 under a pin that is not believed: not the daemon's account", body: socketClient, dirMode: 0o755, fileMode: 0o644, pin: badPin,
			fileUID: 480,
			status:  statusWarn, want: []string{"mode 0644 grants group/other access"},
		},
		{
			name: "the caller's own 0600 client config: the owner-only rule's ok", body: socketClient, dirMode: 0o700, fileMode: 0o600, pin: noPin,
			fileUID: 501,
			status:  statusOK, want: []string{"owner-only regular paths"},
		},
		{
			name: "root-owned 0644 holding the bearer key: the owner-only rule", body: "role: client\nserver_url: \"https://seam.example:8443\"\nmcp:\n  api_key: \"k\"\n",
			dirMode: 0o755, fileMode: 0o644, pin: noPin,
			status: statusWarn, want: []string{"mode 0644 grants group/other access", "chmod 600"}, dont: []string{"holds no secret"},
		},
		{
			name: "root-owned 0644 holding a provider key: the owner-only rule", body: socketClient + "llm:\n  openai:\n    api_key: \"o\"\n",
			dirMode: 0o755, fileMode: 0o644, pin: noPin,
			status: statusWarn, want: []string{"mode 0644 grants group/other access"}, dont: []string{"holds no secret"},
		},
		{
			name: "root-owned 0644 with credentials in server_url: the owner-only rule", body: "role: client\nserver_url: \"https://user:pw@seam.example:8443\"\n",
			dirMode: 0o755, fileMode: 0o644, pin: noPin,
			status: statusWarn, want: []string{"mode 0644 grants group/other access"}, dont: []string{"holds no secret"},
		},
		{
			name: "a server config with no secret: the owner-only rule", body: "addr: \"127.0.0.1:8081\"\n", dirMode: 0o755, fileMode: 0o644, pin: noPin,
			status: statusWarn, want: []string{"mode 0644 grants group/other access"}, dont: []string{"holds no secret"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, path := configAt(t, tt.body, tt.dirMode, tt.fileMode)
			overrides := map[string]func(*fileFacts){path: ownedBy(tt.fileUID), filepath.Dir(path): ownedBy(tt.dirUID)}
			env := sharedConfigEnv{inspect: rootWorld(overrides), evalSymlinks: filepath.EvalSymlinks, daemonAccount: tt.pin}
			got := configFileCheck(cfg, "linux", env)
			require.Equal(t, "config permissions", got.name)
			require.Equal(t, tt.status, got.status, got.detail)
			for _, w := range tt.want {
				require.Contains(t, got.detail, w)
			}
			for _, d := range tt.dont {
				require.NotContains(t, got.detail, d)
			}
		})
	}
}

// A peer_auth daemon's own config is serve's preflight's to judge, not the
// owner-only rule's: that rule would warn on the guide's root:_seamless 0640
// layout and offer a chmod that locks the daemon out.
func TestConfigFileCheck_PeerAuthDaemonConfigDefersToThePreflight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission-bit case")
	}
	t.Setenv("SEAMLESS_ROLE", "")
	cfg, path := configAt(t, "mcp:\n  api_key: \"k\"\n", 0o755, 0o640)
	blind := sharedConfigEnv{inspect: rootWorld(nil), evalSymlinks: filepath.EvalSymlinks,
		daemonAccount: func() (access.DaemonPin, bool, error) { return daemonPin, true, nil }}

	got := configFileCheck(cfg, "linux", blind)
	require.Equal(t, statusWarn, got.status, "with peer_auth off, a 0640 key-bearing file is today's warning")

	cfg.PeerAuth.Enabled = true
	got = configFileCheck(cfg, "linux", blind)
	require.Equal(t, statusInfo, got.status)
	require.Contains(t, got.detail, path+" is a peer_auth daemon's config")
	require.NotContains(t, got.detail, "chmod")
}

// A symlinked config is not judged as shared: what it points at can change
// under it. It keeps the owner-only rule, which names the symlink.
func TestConfigFileCheck_SymlinkKeepsTheOwnerOnlyRule(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix ownership and permission-bit cases")
	}
	t.Setenv("SEAMLESS_ROLE", "")
	_, path := configAt(t, socketClient, 0o755, 0o644)
	link := filepath.Join(filepath.Dir(path), "linked.yaml")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg, err := config.LoadFrom(link)
	require.NoError(t, err)
	env := sharedConfigEnv{inspect: rootWorld(nil), evalSymlinks: filepath.EvalSymlinks,
		daemonAccount: func() (access.DaemonPin, bool, error) { return access.DaemonPin{}, false, nil }}
	got := configFileCheck(cfg, "linux", env)
	require.Equal(t, statusWarn, got.status)
	require.Contains(t, got.detail, "is a symlink")
}

// Where ownership cannot be read (a platform without inspectPath), the file is
// held to the owner-only rule rather than believed shared; no file and Windows
// are that rule's own info rows.
func TestConfigFileCheck_FallsBackToTheOwnerOnlyRule(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission-bit case")
	}
	t.Setenv("SEAMLESS_ROLE", "")
	cfg, _ := configAt(t, socketClient, 0o755, 0o644)
	blind := sharedConfigEnv{
		inspect:       func(string) (fileFacts, error) { return fileFacts{}, errors.New("not supported") },
		evalSymlinks:  filepath.EvalSymlinks,
		daemonAccount: func() (access.DaemonPin, bool, error) { return access.DaemonPin{}, false, nil },
	}
	got := configFileCheck(cfg, "linux", blind)
	require.Equal(t, statusWarn, got.status)
	require.Contains(t, got.detail, "mode 0644 grants group/other access")

	got = configFileCheck(config.Defaults(), "linux", blind)
	require.Equal(t, statusInfo, got.status)
	require.Contains(t, got.detail, "no config file")

	got = configFileCheck(cfg, "windows", blind)
	require.Equal(t, statusInfo, got.status)
	require.Contains(t, got.detail, "Windows ACLs")
}

// The config row says when the file came from the working directory, which the
// seam CLI's search never reads.
func TestConfigSourceCheck(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, key := range []string{"SEAMLESS_CONFIG", "SEAMLESS_ROLE", "SEAMLESS_SERVER_URL", "SEAMLESS_MCP_API_KEY"} {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}
	cwd := t.TempDir()
	t.Chdir(cwd)

	cfg, err := config.Load()
	require.NoError(t, err)
	got := configSourceCheck(cfg)
	require.Equal(t, statusOK, got.status)
	require.Equal(t, "loaded from defaults + env (no seamless.yaml found)", got.detail)

	require.NoError(t, os.WriteFile(filepath.Join(cwd, config.WorkingDirConfig), []byte("mcp:\n  api_key: \"k\"\n"), 0o600))
	cfg, err = config.Load()
	require.NoError(t, err)
	got = configSourceCheck(cfg)
	require.Equal(t, statusInfo, got.status)
	abs := absConfigPath(config.WorkingDirConfig)
	require.Contains(t, got.detail, "loaded from "+abs+", the working directory's")
	require.Contains(t, got.detail, "SEAMLESS_CONFIG="+abs)

	t.Setenv("SEAMLESS_CONFIG", abs)
	cfg, err = config.Load()
	require.NoError(t, err)
	got = configSourceCheck(cfg)
	require.Equal(t, check{statusOK, "config", "loaded from " + abs}, got, "a file named outright is not the working directory's fallback")
}
