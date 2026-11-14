//go:build darwin || linux

package relay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/devices"
)

// The pairing is one file, 0600, written by rename; it loads back whole, and
// its key is the identity the relay presents.
func TestRelayState_SavesAndLoadsItsPairing(t *testing.T) {
	l := relayLayoutAt(t)
	cert := relayNewCert(t, "example-server")
	_, err := LoadState(l.DataDir)
	require.ErrorIs(t, err, ErrNotPaired)
	st := relayWriteState(t, l, "https://example-server.example.ts.net:8081", cert.fp, cert.pem)
	require.Equal(t, os.FileMode(0o600), relayMode(t, filepath.Join(l.DataDir, StateName)))
	back, err := LoadState(l.DataDir)
	require.NoError(t, err)
	require.Equal(t, st, back)
	id, err := back.Identity()
	require.NoError(t, err)
	require.Equal(t, st.Fingerprint, id.Fingerprint())
	entries, err := os.ReadDir(l.DataDir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	require.ElementsMatch(t, []string{StateName, st.CAFile}, names, "no temp file is left behind")
}

// A pairing that is there but does not load is refused, never half-used.
func TestRelayState_RefusesAPairingThatDoesNotLoad(t *testing.T) {
	cert := relayNewCert(t, "example-server")
	other, err := devices.NewRelayIdentity("example-other")
	require.NoError(t, err)
	_, otherKey, err := other.MarshalPEM()
	require.NoError(t, err)
	for name, edit := range map[string]func(*State){
		"another version":      func(s *State) { s.Version = 2 },
		"plain http":           func(s *State) { s.ServerURL = "http://example-server:8081" },
		"no pin":               func(s *State) { s.ServerFingerprint = "" },
		"a short pin":          func(s *State) { s.ServerFingerprint = "AB:CD" },
		"a CA path":            func(s *State) { s.CAFile = "../server-ca-0123456789abcdef.pem" },
		"no device":            func(s *State) { s.DeviceID = "" },
		"another key":          func(s *State) { s.Key = string(otherKey) },
		"another fingerprint":  func(s *State) { s.Fingerprint = other.Fingerprint() },
		"a certificate as key": func(s *State) { s.Key = s.Certificate },
	} {
		l := relayLayoutAt(t)
		st := relayWriteState(t, l, "https://example-server:8081", cert.fp, cert.pem)
		edit(&st)
		data, err := json.Marshal(st)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(l.DataDir, StateName), data, 0o600))
		_, err = LoadState(l.DataDir)
		require.ErrorIs(t, err, ErrState, name)
		if name != "another version" { // SaveState writes this release's version
			require.Error(t, SaveState(l.DataDir, st), name)
		}
	}

	l := relayLayoutAt(t)
	require.NoError(t, os.WriteFile(filepath.Join(l.DataDir, StateName), []byte("{not json"), 0o600))
	_, err = LoadState(l.DataDir)
	require.ErrorIs(t, err, ErrState)

	// A symlink is not the relay's own file, wherever it points.
	l = relayLayoutAt(t)
	relayWriteState(t, l, "https://example-server:8081", cert.fp, cert.pem)
	moved := filepath.Join(filepath.Dir(l.DataDir), "elsewhere.json")
	require.NoError(t, os.Rename(filepath.Join(l.DataDir, StateName), moved))
	require.NoError(t, os.Symlink(moved, filepath.Join(l.DataDir, StateName)))
	_, err = LoadState(l.DataDir)
	require.ErrorIs(t, err, ErrState)
}

func TestRelayServerURLOf(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://example-server.example.ts.net:8081":  true,
		"https://example-server.example.ts.net:8081/": true,
		"https://192.168.1.20":                        true,
		"https://[fd00::1]:8081":                      true,
		"http://example-server:8081":                  false,
		"unix:///opt/seamless/run/seamlessd.sock":     false,
		"https://":                         false,
		"https://user@example-server:8081": false,
		"https://example-server:8081/api":  false,
		"https://example-server:8081?x=1":  false,
		"https://example-server:8081#x":    false,
		"example-server:8081":              false,
		"":                                 false,
	} {
		_, err := ServerURLOf(raw)
		require.Equal(t, ok, err == nil, "%q: %v", raw, err)
	}
}
