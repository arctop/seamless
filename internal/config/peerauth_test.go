package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeConfig writes body as a seamless.yaml in a temp dir and loads it.
func loadYAML(t *testing.T, body string) (Config, error) {
	t.Helper()
	clearSeamlessEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := filepath.Join(t.TempDir(), "seamless.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return LoadFrom(path)
}

const peerBase = `
data_dir: /opt/seamless/data
peer_auth:
  enabled: true
  owner: katata
  socket:
    path: /opt/seamless/run/seamlessd.sock
`

func TestPeerAuth_DefaultIsOff(t *testing.T) {
	require.Equal(t, PeerAuth{}, Defaults().PeerAuth)
	cfg, err := loadYAML(t, "addr: 127.0.0.1:8081\n")
	require.NoError(t, err)
	require.False(t, cfg.PeerAuth.Enabled)
}

func TestPeerAuth_LoadsAFullBlock(t *testing.T) {
	cfg, err := loadYAML(t, peerBase+`  users:
    _arctop_finance:
      sealed: [arctop-finance]
    _arctop_hq:
      projects: [arctop-hq, arctop-ops]
`)
	require.NoError(t, err)
	pa := cfg.PeerAuth
	require.True(t, pa.Enabled)
	require.Equal(t, "katata", pa.Owner)
	require.Equal(t, map[string]string{"arctop-finance": "_arctop_finance"}, pa.SealedTo())
	require.Equal(t, []string{"arctop-finance", "arctop-hq", "arctop-ops"}, pa.ReferencedProjects())
	mode, err := pa.Socket.SocketFileMode()
	require.NoError(t, err)
	require.Equal(t, uint32(0o666), mode, "an unset mode is the default")
}

func TestPeerAuth_ShapeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{"owner missing", `
data_dir: /opt/seamless/data
peer_auth:
  enabled: true
  socket: {path: /opt/seamless/run/s.sock}
`, "peer_auth.owner is required"},
		{"socket path missing", `
data_dir: /opt/seamless/data
peer_auth: {enabled: true, owner: katata}
`, "peer_auth.socket.path is required"},
		{"socket path relative", `
data_dir: /opt/seamless/data
peer_auth: {enabled: true, owner: katata, socket: {path: run/s.sock}}
`, "must be absolute"},
		{"socket inside the data dir", `
data_dir: /opt/seamless/data
peer_auth: {enabled: true, owner: katata, socket: {path: /opt/seamless/data/run/s.sock}}
`, "inside data_dir"},
		{"mode not octal", peerBase + "    mode: rw-rw-rw-\n", "not an octal mode"},
		{"mode beyond 0777", peerBase + "    mode: \"4777\"\n", "beyond 0777"},
		{"mode without owner rw", peerBase + "    mode: \"0066\"\n", "owner read and write"},
		{"owner listed as a user", peerBase + `  users:
    katata: {sealed: [x]}
`, "the owner"},
		{"a project sealed to two users", peerBase + `  users:
    _a: {sealed: [arctop-finance]}
    _b: {sealed: [arctop-finance]}
`, "sealed to two accounts"},
		{"sealed to one user and shared with another", peerBase + `  users:
    _a: {sealed: [arctop-finance]}
    _b: {projects: [arctop-finance]}
`, "also listed under"},
		{"listed twice for one user", peerBase + `  users:
    _a: {sealed: [arctop-finance], projects: [arctop-finance]}
`, "twice"},
		{"global listed", peerBase + `  users:
    _a: {projects: [global]}
`, "global scope"},
		{"the widening token listed", peerBase + `  users:
    _a: {sealed: [all]}
`, "reserved"},
		{"a traversal slug", peerBase + `  users:
    _a: {sealed: ["../notes"]}
`, "invalid project"},
		{"a user reaching nothing", peerBase + `  users:
    _a: {}
`, "reaches no project"},
		{"an account name with a space", peerBase + `  users:
    "_a b": {sealed: [x]}
`, "not an OS account name"},
		{"a misspelled key", peerBase + "  sealed: [x]\n", "field sealed not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// A disabled block is still checked: a typo is caught the day it is written.
func TestPeerAuth_DisabledUsersStillValidated(t *testing.T) {
	_, err := loadYAML(t, `
peer_auth:
  enabled: false
  users:
    _a: {sealed: [arctop-finance]}
    _b: {sealed: [arctop-finance]}
`)
	require.ErrorContains(t, err, "sealed to two accounts")
}

func TestPeerAuth_EnvOverrides(t *testing.T) {
	clearSeamlessEnv(t)
	t.Setenv("SEAMLESS_DATA_DIR", "/opt/seamless/data")
	t.Setenv("SEAMLESS_PEER_AUTH_ENABLED", "true")
	t.Setenv("SEAMLESS_PEER_AUTH_OWNER", "katata")
	t.Setenv("SEAMLESS_PEER_AUTH_SOCKET_PATH", "/opt/seamless/run/s.sock")
	t.Setenv("SEAMLESS_PEER_AUTH_SOCKET_MODE", "0660")
	t.Setenv("SEAMLESS_PEER_AUTH_SOCKET_GROUP", "_seamless_clients")
	cfg, err := LoadFrom("")
	require.NoError(t, err)
	require.True(t, cfg.PeerAuth.Enabled)
	require.Equal(t, "katata", cfg.PeerAuth.Owner)
	require.Equal(t, "/opt/seamless/run/s.sock", cfg.PeerAuth.Socket.Path)
	require.Equal(t, "_seamless_clients", cfg.PeerAuth.Socket.Group)
	mode, err := cfg.PeerAuth.Socket.SocketFileMode()
	require.NoError(t, err)
	require.Equal(t, uint32(0o660), mode)

	t.Setenv("SEAMLESS_PEER_AUTH_ENABLED", "yes please")
	_, err = LoadFrom("")
	require.ErrorContains(t, err, "SEAMLESS_PEER_AUTH_ENABLED")
}

func TestServerURL_Socket(t *testing.T) {
	cfg, err := loadYAML(t, "role: client\nserver_url: unix:///opt/seamless/run/seamlessd.sock\n")
	require.NoError(t, err)
	require.Equal(t, "/opt/seamless/run/seamlessd.sock", cfg.SocketPath())
	require.Equal(t, SocketBaseURL, cfg.ServerURL(), "every dial builds on an http URL the socket transport ignores")
	require.Equal(t, "unix:///opt/seamless/run/seamlessd.sock", cfg.HookBaseURL())
	require.Equal(t, "unix:/opt/seamless/run/seamlessd.sock", cfg.DaemonAddress())
	require.Empty(t, cfg.AllowedHostsEffective(), "a socket names no host to allow")

	cfg, err = loadYAML(t, "role: client\nserver_url: unix:/opt/seamless/run/seamlessd.sock\n")
	require.NoError(t, err, "the one-slash spelling")
	require.Equal(t, "/opt/seamless/run/seamlessd.sock", cfg.SocketPath())

	for _, bad := range []string{
		"unix://host/opt/s.sock", "unix:relative.sock", "unix:///opt/../etc/s.sock", "unix:///opt/s.sock?x=1", "unix:///opt/s.sock#f",
	} {
		_, err := loadYAML(t, "role: client\nserver_url: \""+bad+"\"\n")
		require.Error(t, err, bad)
	}

	cfg, err = loadYAML(t, "server_url: http://studio.local:8081\n")
	require.NoError(t, err)
	require.Empty(t, cfg.SocketPath())
	require.Equal(t, cfg.ServerURL(), cfg.HookBaseURL())
}
