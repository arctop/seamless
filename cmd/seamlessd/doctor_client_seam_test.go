package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/sshsig"
	"github.com/arctop/seamless/internal/sshsig/sshsigtest"
	"github.com/arctop/seamless/internal/update"
)

// seamTree is a fake file system of the guide's layout: /opt/seamless/bin
// the daemon's (uid 480), /opt/seamless/client root's, ~/.local/bin the
// owner's (uid 501).
func seamTree(over map[string]fileFacts) clientSeamEnv {
	dir := func(uid uint32, mode os.FileMode) fileFacts {
		return fileFacts{uid: uid, mode: mode | os.ModeDir, isDir: true}
	}
	nodes := map[string]fileFacts{
		"/": dir(0, 0o755), "/opt": dir(0, 0o755), "/opt/seamless": dir(0, 0o755),
		"/opt/seamless/bin": dir(480, 0o755), "/opt/seamless/bin/seam": {uid: 480, mode: 0o755},
		"/opt/seamless/client": dir(0, 0o755), "/opt/seamless/client/seam": {uid: 0, mode: 0o755},
		"/Users": dir(0, 0o755), "/Users/owner": dir(501, 0o750), "/Users/owner/.local": dir(501, 0o755),
		"/Users/owner/.local/bin": dir(501, 0o755), "/Users/owner/.local/bin/seam": {uid: 501, mode: 0o755},
	}
	for k, v := range over {
		nodes[k] = v
	}
	return clientSeamEnv{
		euid:    501,
		resolve: func(p string) (string, error) { return p, nil },
		inspect: func(p string) (fileFacts, error) {
			if f, ok := nodes[p]; ok {
				return f, nil
			}
			return fileFacts{}, os.ErrNotExist
		},
		daemonPin: func() (access.DaemonPin, bool, error) {
			return access.DaemonPin{Name: "_seamless", UID: 480}, true, nil
		},
		revision: func(string) (string, bool) { return "", false },
		daemonCommit: func() (string, string, bool) {
			return "", "", false
		},
	}
}

func TestClientSeamRows(t *testing.T) {
	if filepath.Separator != '/' {
		t.Skip("POSIX paths")
	}
	t.Run("the owner's own copy", func(t *testing.T) {
		env := seamTree(nil)
		env.seams = []string{"/Users/owner/.local/bin/seam"}
		rows := clientSeamRows(env)
		require.Len(t, rows, 1)
		require.Equal(t, statusInfo, rows[0].status)
		require.Contains(t, rows[0].detail, "your account can replace, and so can every agent running as you")
	})
	t.Run("the daemon's copy", func(t *testing.T) {
		env := seamTree(nil)
		env.seams = []string{"/opt/seamless/bin/seam"}
		rows := clientSeamRows(env)
		require.Equal(t, statusInfo, rows[0].status)
		require.Contains(t, rows[0].detail, "_seamless (the daemon's account) can replace")
		require.Contains(t, rows[0].detail, guideClientSection)
	})
	t.Run("root's copy, release unknown", func(t *testing.T) {
		env := seamTree(nil)
		env.seams = []string{"/opt/seamless/client/seam"}
		rows := clientSeamRows(env)
		require.Equal(t, statusOK, rows[0].status)
		require.Contains(t, rows[0].detail, "only root can replace")
		require.Contains(t, rows[0].detail, "refresh it as root after each update")
	})
	t.Run("root's copy of the daemon's release", func(t *testing.T) {
		env := seamTree(nil)
		env.seams = []string{"/opt/seamless/client/seam"}
		env.revision = func(string) (string, bool) { return "abc1234def5678abc1234def5678abc1234def56", true }
		env.daemonCommit = func() (string, string, bool) { return "0.9.1", "abc1234", true }
		rows := clientSeamRows(env)
		require.Equal(t, statusOK, rows[0].status)
		require.Contains(t, rows[0].detail, "it is the daemon's release (v0.9.1)")
	})
	t.Run("root's copy a release behind", func(t *testing.T) {
		env := seamTree(nil)
		env.seams = []string{"/opt/seamless/client/seam"}
		env.revision = func(string) (string, bool) { return "0000000def5678abc1234def5678abc1234def56", true }
		env.daemonCommit = func() (string, string, bool) { return "0.9.1", "abc1234", true }
		rows := clientSeamRows(env)
		require.Equal(t, statusWarn, rows[0].status)
		require.Contains(t, rows[0].detail, "built from commit 0000000, but the daemon runs v0.9.1 (commit abc1234)")
	})
	t.Run("a root-owned file in a directory every account can write", func(t *testing.T) {
		env := seamTree(map[string]fileFacts{"/opt/seamless/client": {uid: 0, mode: 0o777 | os.ModeDir, isDir: true}})
		env.seams = []string{"/opt/seamless/client/seam"}
		rows := clientSeamRows(env)
		require.Equal(t, statusWarn, rows[0].status)
		require.Contains(t, rows[0].detail, "every account can replace")
	})
	t.Run("a seam that is not there says nothing (the hooks row does)", func(t *testing.T) {
		env := seamTree(nil)
		env.seams = []string{"/nowhere/seam"}
		require.Empty(t, clientSeamRows(env))
	})
}

// Off peer_auth's socket there is no row at all: the report is as it was.
func TestClientSeamChecks_OnlyOnASocketClient(t *testing.T) {
	cfg := config.Defaults()
	cfg.Role = "client"
	require.Nil(t, clientSeamChecks(cfg))
}

func TestReleaseSignersCheck(t *testing.T) {
	require.NotPanics(t, func() {
		_, ok := releaseSignersCheck(config.Defaults(), hostSignersFS())
		require.False(t, ok, "no pin, no row")
	})
	dir := t.TempDir()
	key, err := sshsigtest.NewKey(sshsig.KeySKEd25519)
	require.NoError(t, err)
	path := filepath.Join(dir, "release_signers")
	require.NoError(t, os.WriteFile(path, []byte(key.SignersLine("owner-a", "seamless-release")+"\n"), 0o644))
	cfg := config.Defaults()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.Update.ReleaseSigners = path

	c, ok := releaseSignersCheck(cfg, hostSignersFS())
	require.True(t, ok)
	require.Equal(t, statusOK, c.status, c.detail)
	require.Contains(t, c.detail, "owner-a "+key.Fingerprint())
	require.NotContains(t, c.detail, "last refused")

	// The daemon's record of the last refusal is named.
	require.NoError(t, os.MkdirAll(update.StateDir(cfg.DataDir), 0o700))
	st := update.State{SignatureRefusal: &update.SignatureRefusal{
		Version: mustVersion(t, "0.9.1"), From: mustVersion(t, "0.9.0"), Signature: update.SignatureUnlisted, At: time.Now()}}
	require.NoError(t, update.SaveState(cfg.DataDir, st))
	c, _ = releaseSignersCheck(cfg, hostSignersFS())
	require.Contains(t, c.detail, "last refused v0.9.1")
	require.Contains(t, c.detail, "a key update.release_signers does not list")

	cfg.Update.ReleaseSigners = filepath.Join(dir, "missing")
	c, _ = releaseSignersCheck(cfg, hostSignersFS())
	require.Equal(t, statusFail, c.status)
	require.True(t, strings.Contains(c.detail, "every update is refused"), c.detail)
}
