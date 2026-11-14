package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// peer_auth.relays (plan networked-relay): file only, refused while peer_auth
// is off, each entry a relay's enrolled name and the owner's uid there.
func TestPeerAuth_Relays(t *testing.T) {
	cfg, err := loadYAML(t, peerBase+`  relays:
    example-laptop:
      owner_uid: 501
    build-01.example.lan:
      owner_uid: 1000
`)
	require.NoError(t, err)
	require.Equal(t, map[string]PeerRelay{"example-laptop": {OwnerUID: 501}, "build-01.example.lan": {OwnerUID: 1000}},
		cfg.PeerAuth.Relays)

	cfg, err = loadYAML(t, "addr: 127.0.0.1:8081\n")
	require.NoError(t, err)
	require.Nil(t, cfg.PeerAuth.Relays, "absent, nothing is mapped")

	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"peer_auth off", `
peer_auth:
  relays:
    example-laptop: {owner_uid: 501}
`, "while peer_auth is disabled"},
		{"no owner_uid", peerBase + "  relays:\n    example-laptop: {}\n", "owner_uid is required"},
		{"root", peerBase + "  relays:\n    example-laptop: {owner_uid: 0}\n", "0 is root"},
		{"a negative uid", peerBase + "  relays:\n    example-laptop: {owner_uid: -1}\n", "cannot unmarshal"},
		{"a uid past 2^32-1", peerBase + "  relays:\n    example-laptop: {owner_uid: 4294967296}\n", "cannot unmarshal"},
		{"an upper-case name", peerBase + "  relays:\n    Example-Laptop: {owner_uid: 501}\n", "not a relay's name"},
		{"a name with a space", peerBase + "  relays:\n    \"example laptop\": {owner_uid: 501}\n", "not a relay's name"},
		{"a dash at a label's end", peerBase + "  relays:\n    example-.lan: {owner_uid: 501}\n", "not a relay's name"},
		{"an empty label", peerBase + "  relays:\n    example..lan: {owner_uid: 501}\n", "not a relay's name"},
		{"a name too long", peerBase + "  relays:\n    " + strings.Repeat("a", 254) + ": {owner_uid: 501}\n", "1 to 253"},
		{"a misspelled key", peerBase + "  relays:\n    example-laptop: {owner: 501}\n", "field owner not found"},
		{"a list, not a map", peerBase + "  relays:\n    - {name: example-laptop, owner_uid: 501}\n", "cannot unmarshal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadYAML(t, tc.yaml)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// The environment never maps a relay: a variable of the key's name changes
// nothing.
func TestPeerAuth_RelaysAreFileOnly(t *testing.T) {
	clearSeamlessEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := filepath.Join(t.TempDir(), "seamless.yaml")
	require.NoError(t, os.WriteFile(path, []byte(peerBase), 0o600))
	for _, name := range []string{"SEAMLESS_PEER_AUTH_RELAYS", "SEAMLESS_PEER_AUTH_RELAYS_EXAMPLE_LAPTOP_OWNER_UID"} {
		t.Setenv(name, "501")
	}
	cfg, err := LoadFrom(path)
	require.NoError(t, err)
	require.Empty(t, cfg.PeerAuth.Relays)
}
