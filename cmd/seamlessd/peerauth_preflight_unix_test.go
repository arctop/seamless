//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
)

// The preflight refuses a config every account can read (it holds the bearer
// key), and a config reached through a symlink in a directory another account
// can rewrite (the link decides what the next start loads).
func TestTrustedFileProblem_ConfigReadabilityAndSymlink(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	etc, ownerDir := filepath.Join(root, "etc"), filepath.Join(root, "ownerdir")
	require.NoError(t, os.MkdirAll(etc, 0o755))
	require.NoError(t, os.MkdirAll(ownerDir, 0o755))
	cfgFile := filepath.Join(etc, "seamless.yaml")
	require.NoError(t, os.WriteFile(cfgFile, []byte("x"), 0o640))
	worldCfg := filepath.Join(etc, "world.yaml")
	require.NoError(t, os.WriteFile(worldCfg, []byte("x"), 0o640))
	bin := filepath.Join(etc, "seamlessd")
	require.NoError(t, os.WriteFile(bin, []byte("x"), 0o755))
	link := filepath.Join(ownerDir, "seamless.yaml")
	require.NoError(t, os.Symlink(cfgFile, link))

	const daemon = 480
	inspect := func(p string) (fileFacts, error) {
		info, err := os.Lstat(p)
		if err != nil {
			return fileFacts{}, err
		}
		f := fileFacts{uid: 0, mode: info.Mode(), isDir: info.IsDir(), symlink: info.Mode()&os.ModeSymlink != 0}
		switch p {
		case ownerDir:
			f.uid = 501
		case worldCfg:
			f.mode = 0o644
		}
		return f, nil
	}
	cfg := config.Defaults()
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", Locked: []string{"p"}}
	policy, err := access.New(cfg.PeerAuth, lockOwnerResolve)
	require.NoError(t, err)
	rt := &peerAuthRuntime{policy: policy, cfg: cfg.PeerAuth,
		env: peerAuthEnv{euid: daemon, inspect: inspect, exe: func() (string, error) { return bin, nil }, resolve: lockOwnerResolve}}

	require.Empty(t, rt.trustedFileProblem(configFileWhat, cfgFile), "the guide's 0640 passes")
	require.Contains(t, rt.trustedFileProblem(configFileWhat, worldCfg), "every account can read it")
	require.Empty(t, rt.trustedFileProblem("seamlessd binary", bin), "a binary is meant to be readable")
	require.Contains(t, rt.trustedFileProblem(configFileWhat, link), ownerDir)
}
