package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		// "sealed" was this row's sample until it became the lock's pre-release
		// name, which now has its own refusal (TestPeerAuth_SealedKeyRenamed);
		// "seal" keeps the property tested.
		{"a misspelled key", peerBase + "  seal: [x]\n", "field seal not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// peer_auth.locked (the approval design): locked projects, checked like the
// users map -- always, enabled or not -- and never beside it, so a 6afda40 users
// block cannot be read as the new design or the other way round.
func TestPeerAuth_LockedList(t *testing.T) {
	cfg, err := loadYAML(t, peerBase+"  locked: [example-finance, \" example-legal\", example-ip]\n")
	require.NoError(t, err)
	require.Equal(t, []string{"example-finance", " example-legal", "example-ip"}, cfg.PeerAuth.Locked)
	require.Equal(t, []string{"example-finance", "example-ip", "example-legal"}, cfg.PeerAuth.ReferencedProjects(),
		"serve and doctor check every locked slug against the projects table")
	require.Empty(t, cfg.PeerAuth.SealedTo(), "no project is sealed to an OS account")

	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"both designs at once", peerBase + `  locked: [example-finance]
  users:
    _a: {sealed: [example-legal]}
`, "both locked and users"},
		{"the old design's seal moved up beside its users map", peerBase + `  locked: [example-finance]
  users:
    _a: {sealed: [example-finance]}
`, "both locked and users"},
		{"global", peerBase + "  locked: [global]\n", "global scope"},
		{"the _global alias", peerBase + "  locked: [_global]\n", "global scope"},
		{"the widening token", peerBase + "  locked: [all]\n", "reserved"},
		{"a traversal slug", peerBase + "  locked: [\"../notes\"]\n", "invalid project"},
		{"listed twice", peerBase + "  locked: [example-finance, \"example-finance \"]\n", "twice"},
		{"disabled is still checked", "peer_auth:\n  enabled: false\n  locked: [global]\n", "global scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// The lock was spelled peer_auth.sealed before any release shipped it. A config
// that still says so is refused with the new name rather than a bare "field
// sealed not found", enabled or not, whatever the list holds. The earlier
// design's users.<account>.sealed is a different key and still loads.
func TestPeerAuth_SealedKeyRenamed(t *testing.T) {
	for name, body := range map[string]string{
		"enabled":                 peerBase + "  sealed: [example-finance]\n",
		"disabled":                "peer_auth:\n  enabled: false\n  sealed: [example-finance]\n",
		"an empty list":           "peer_auth:\n  sealed: []\n",
		"beside the new key":      peerBase + "  locked: [example-finance]\n  sealed: [example-finance]\n",
		"the block from an alias": "base: &pa\n  sealed: [example-finance]\npeer_auth: *pa\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadYAML(t, body)
			require.Error(t, err)
			require.Contains(t, err.Error(), "peer_auth.sealed was renamed to peer_auth.locked")
		})
	}

	cfg, err := loadYAML(t, peerBase+"  users:\n    _example_finance:\n      sealed: [example-finance]\n")
	require.NoError(t, err, "users.<account>.sealed is the earlier design's key, not the renamed one")
	require.Equal(t, map[string]string{"example-finance": "_example_finance"}, cfg.PeerAuth.SealedTo())
}

// A config written for 6afda40 -- the users map alone -- loads exactly as it
// did: no project lands in the approval design's locked list, and every seal
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
	require.Empty(t, cfg.PeerAuth.Locked, "the users map never fills the approval design's list")
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

// peer_auth.passkeys: the WebAuthn relying party of the approver passkeys.
// Checked always, enabled or not; unset, it is the console on localhost.
func TestPeerAuth_Passkeys(t *testing.T) {
	cfg, err := loadYAML(t, "addr: 127.0.0.1:9123\n"+strings.TrimPrefix(peerBase, "\n"))
	require.NoError(t, err)
	rp, origins := cfg.RelyingParty()
	require.Equal(t, "localhost", rp, "an IP is not a valid RP ID, so the default names localhost")
	require.Equal(t, []string{"http://localhost:9123"}, origins, "the console's own port")

	cfg, err = loadYAML(t, peerBase+`  passkeys:
    rp_id: seamless.example.com
    origins: ["https://seamless.example.com", "https://console.seamless.example.com:8443/"]
`)
	require.NoError(t, err)
	rp, origins = cfg.RelyingParty()
	require.Equal(t, "seamless.example.com", rp)
	require.Equal(t, []string{"https://seamless.example.com", "https://console.seamless.example.com:8443"}, origins)

	cfg, err = loadYAML(t, peerBase+"  passkeys:\n    rp_id: localhost\n    origins: [\"http://localhost:8081\", \"http://app.localhost:8081\"]\n")
	require.NoError(t, err, "http is fine on localhost and its subdomains")
	_, origins = cfg.RelyingParty()
	require.Equal(t, []string{"http://localhost:8081", "http://app.localhost:8081"}, origins)

	for _, tc := range []struct{ name, block, want string }{
		{"an IP literal as rp_id", "    rp_id: 127.0.0.1\n", "IP address"},
		{"an IPv6 rp_id", "    rp_id: \"::1\"\n", "IP address"},
		{"uppercase", "    rp_id: Example.com\n", "host name"},
		{"a port in rp_id", "    rp_id: localhost:8081\n", "host name"},
		{"a scheme in rp_id", "    rp_id: https://example.com\n", "host name"},
		{"origins without rp_id", "    origins: [\"http://localhost:8081\"]\n", "without rp_id"},
		{"http off localhost", "    rp_id: example.com\n    origins: [\"http://example.com\"]\n", "only on localhost"},
		{"a mismatched host", "    rp_id: example.com\n    origins: [\"https://example.org\"]\n", "subdomain"},
		{"a suffix that is not a subdomain", "    rp_id: example.com\n    origins: [\"https://badexample.com\"]\n", "subdomain"},
		{"an IP origin", "    rp_id: localhost\n    origins: [\"http://127.0.0.1:8081\"]\n", "IP address"},
		{"a path", "    rp_id: localhost\n    origins: [\"http://localhost:8081/console\"]\n", "nothing else"},
		{"another scheme", "    rp_id: localhost\n    origins: [\"ftp://localhost\"]\n", "scheme"},
		{"listed twice", "    rp_id: localhost\n    origins: [\"http://localhost:1\", \"http://localhost:1/\"]\n", "twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, peerBase+"  passkeys:\n"+tc.block)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
			_, err = loadYAML(t, "peer_auth:\n  enabled: false\n  passkeys:\n"+tc.block)
			require.Error(t, err, "checked while disabled too")
		})
	}
}

// peer_auth.max_lease caps every approved lease: unset is 8h, set is at most
// 24h.
func TestPeerAuth_MaxLease(t *testing.T) {
	cfg, err := loadYAML(t, peerBase)
	require.NoError(t, err)
	require.Equal(t, 8*time.Hour, cfg.PeerAuth.LeaseCap(), "the owner's default")
	cfg, err = loadYAML(t, peerBase+"  max_lease: 2h\n")
	require.NoError(t, err)
	require.Equal(t, 2*time.Hour, cfg.PeerAuth.LeaseCap())
	for _, bad := range []string{"25h", "-1h", "90"} {
		_, err := loadYAML(t, peerBase+"  max_lease: "+bad+"\n")
		require.Error(t, err, bad)
	}
}
