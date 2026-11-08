//go:build windows

// Step 10's defaults row, the Windows half (plan seamless-sealed-unlock): the
// plan's code paths that need peer credentials, the process tree or the Touch
// ID helper have a build here that compiles and says "not supported", and
// enabling peer_auth still refuses serve at start, before anything opens a
// file or a port. Every top-level identifier carries the dg prefix.
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
)

// dgWinPeerAuthRefusal is serve's refusal on a platform without Unix-socket
// peer credentials (preparePeerAuth), as runServe returns it.
const dgWinPeerAuthRefusal = "seamlessd.serve: peer_auth preflight: this platform has no Unix-socket peer credentials Seamless can read (macOS and Linux only)"

// TestDefaultsWindows_ServeRefusesPeerAuth runs serve with peer_auth enabled
// in an otherwise valid config: it must refuse with the platform message and
// leave the data dir uncreated (the refusal comes before the lock, the
// database and the listener).
func TestDefaultsWindows_ServeRefusesPeerAuth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	sock := filepath.Join(root, "sock", "seamless.sock")
	cfgPath := filepath.Join(root, "seamless.yaml")
	yaml := "addr: '127.0.0.1:0'\n" +
		"data_dir: '" + filepath.ToSlash(dataDir) + "'\n" +
		"mcp:\n  api_key: 'dg-windows-key-0123456789'\n" +
		"peer_auth:\n  enabled: true\n  owner: 'example-owner'\n  socket:\n    path: '" + filepath.ToSlash(sock) + "'\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(yaml), 0o600))
	// runServe exports SEAMLESS_CONFIG for --config; t.Setenv restores it.
	t.Setenv("SEAMLESS_CONFIG", cfgPath)

	cfg, err := config.Load()
	require.NoError(t, err, "the config is valid: the refusal under test is serve's, not the config's")
	require.True(t, cfg.PeerAuth.Enabled)

	err = runServe([]string{"--config", cfgPath})
	require.EqualError(t, err, dgWinPeerAuthRefusal)
	require.ErrorIs(t, err, errPeerAuthPreflight)
	_, statErr := os.Stat(dataDir)
	require.True(t, os.IsNotExist(statErr), "serve refused before it created the data dir")
}

// TestDefaultsWindows_PeerPathsSayNotSupported pins the "other" builds the
// plan's peer code paths compile to here, and the words each answers.
func TestDefaultsWindows_PeerPathsSayNotSupported(t *testing.T) {
	require.False(t, access.PeerCredSupported)

	// internal/access/peercred_other.go and proctree_other.go.
	_, err := access.PeerUID(nil)
	require.ErrorIs(t, err, access.ErrPeerCredUnsupported)
	require.EqualError(t, access.ErrPeerCredUnsupported, "access: Unix-socket peer credentials are not supported on this platform")
	_, err = access.ReadPeer(nil)
	require.ErrorIs(t, err, access.ErrPeerCredUnsupported)
	_, err = access.Peer{}.Chain()
	require.ErrorIs(t, err, access.ErrPeerCredUnsupported)
	_, err = access.ProgramPath(1)
	require.ErrorIs(t, err, access.ErrPeerCredUnsupported)
	require.Empty(t, access.ProgramOf(access.ProcessToken{}), "no process is named here")

	// cmd/seamlessd/peerauth_other.go: the file ownership checks.
	_, err = inspectPath(t.TempDir())
	require.EqualError(t, err, "peer_auth: file ownership checks are not supported on this platform")

	// preparePeerAuth itself, and doctor's peer_auth row.
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	cfg.PeerAuth.Enabled = true
	_, err = preparePeerAuth(cfg, defaultPeerAuthEnv())
	require.ErrorIs(t, err, errPeerAuthPreflight)
	require.EqualError(t, err, "peer_auth preflight: this platform has no Unix-socket peer credentials Seamless can read (macOS and Linux only)")
	checks := peerAuthChecks(context.Background(), cfg, nil, defaultPeerAuthEnv())
	require.Len(t, checks, 1)
	require.Equal(t, statusFail, checks[0].status)
	require.Equal(t, "enabled, but this platform has no Unix-socket peer credentials Seamless can read (macOS and Linux only)", checks[0].detail)

	// seamlessd approve: the account check fails closed at the file check.
	cfg.PeerAuth.Locked = []string{"example-locked"}
	err = approverAccountCheck(cfg, defaultPeerAuthEnv())
	require.ErrorContains(t, err, "peer_auth: file ownership checks are not supported on this platform")
}
