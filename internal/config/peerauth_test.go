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
		// An unknown key under peer_auth is refused (KnownFields). Top-level
		// "sealed" was this row's sample until it became the passkey design's
		// real key (TestPeerAuth_SealedList); "seal" keeps the property tested.
		{"a misspelled key", peerBase + "  seal: [x]\n", "field seal not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// peer_auth.sealed (the passkey design): projects sealed behind the owner's
// passkey, checked like the users map -- always, enabled or not -- and never
// beside it, so a 6afda40 users block cannot be read as the new design or the
// other way round.
func TestPeerAuth_SealedList(t *testing.T) {
	cfg, err := loadYAML(t, peerBase+"  sealed: [arctop-finance, \" arctop-legal\", arctop-ip]\n")
	require.NoError(t, err)
	require.Equal(t, []string{"arctop-finance", " arctop-legal", "arctop-ip"}, cfg.PeerAuth.Sealed)
	require.Equal(t, []string{"arctop-finance", "arctop-ip", "arctop-legal"}, cfg.PeerAuth.ReferencedProjects(),
		"serve and doctor check every sealed slug against the projects table")
	require.Empty(t, cfg.PeerAuth.SealedTo(), "no project is sealed to an OS account")

	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"both designs at once", peerBase + `  sealed: [arctop-finance]
  users:
    _a: {sealed: [arctop-legal]}
`, "both sealed and users"},
		{"the old design's seal moved up beside its users map", peerBase + `  sealed: [arctop-finance]
  users:
    _a: {sealed: [arctop-finance]}
`, "both sealed and users"},
		{"global", peerBase + "  sealed: [global]\n", "global scope"},
		{"the _global alias", peerBase + "  sealed: [_global]\n", "global scope"},
		{"the widening token", peerBase + "  sealed: [all]\n", "reserved"},
		{"a traversal slug", peerBase + "  sealed: [\"../notes\"]\n", "invalid project"},
		{"listed twice", peerBase + "  sealed: [arctop-finance, \"arctop-finance \"]\n", "twice"},
		{"disabled is still checked", "peer_auth:\n  enabled: false\n  sealed: [global]\n", "global scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// A config written for 6afda40 -- the users map alone -- loads exactly as it
// did: no project lands in the passkey design's sealed list, and every seal
// stays with its account. (internal/access's TestLegacyUsersConfig_KeepsItsMeaning
// carries the same file through the policy.)
func TestPeerAuth_UsersOnlyConfigKeepsItsMeaning(t *testing.T) {
	cfg, err := loadYAML(t, peerBase+`  users:
    _arctop_finance:
      sealed: [arctop-finance]
    _arctop_hq:
      projects: [arctop-hq, arctop-ops]
`)
	require.NoError(t, err)
	require.Empty(t, cfg.PeerAuth.Sealed, "the users map never fills the passkey design's list")
	require.Equal(t, map[string]string{"arctop-finance": "_arctop_finance"}, cfg.PeerAuth.SealedTo())
	require.Equal(t, []string{"arctop-finance", "arctop-hq", "arctop-ops"}, cfg.PeerAuth.ReferencedProjects())
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
