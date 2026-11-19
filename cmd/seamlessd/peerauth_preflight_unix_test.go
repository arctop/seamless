//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/shellcmd"
	"github.com/arctop/seamless/internal/sshsig"
	"github.com/arctop/seamless/internal/sshsig/sshsigtest"
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
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", ForceLocked: []string{"p"}}
	policy, err := access.New(cfg.PeerAuth, lockOwnerResolve)
	require.NoError(t, err)
	rt := &peerAuthRuntime{policy: policy, cfg: cfg.PeerAuth,
		env: peerAuthEnv{euid: daemon, inspect: inspect, exe: func() (string, error) { return bin, nil }, resolve: lockOwnerResolve}}

	require.Empty(t, rt.trustedFileProblem(configFileWhat, cfgFile), "the guide's 0640 passes")
	require.Contains(t, rt.trustedFileProblem(configFileWhat, worldCfg), "every account can read it")
	require.Empty(t, rt.trustedFileProblem("seamlessd binary", bin), "a binary is meant to be readable")
	require.Contains(t, rt.trustedFileProblem(configFileWhat, link), ownerDir)
}

// A config or TLS key every account can read is told to take the daemon's
// group before it is made 0640: root's file left in another group would be
// closed to the daemon at 0640, and the daemon's own file open to that group.
func TestTrustedFileProblem_WorldReadableAdviceKeepsTheFileReadable(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	etc := filepath.Join(root, "etc")
	require.NoError(t, os.MkdirAll(etc, 0o755))
	file := filepath.Join(etc, "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o640))
	bin := filepath.Join(etc, "seamlessd")
	require.NoError(t, os.WriteFile(bin, []byte("x"), 0o755))

	const daemonUID, daemonGID = 480, 480
	cfg := config.Defaults()
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", ForceLocked: []string{"p"}}
	policy, err := access.New(cfg.PeerAuth, lockOwnerResolve)
	require.NoError(t, err)
	for _, what := range []string{configFileWhat, tlsKeyWhat} {
		for _, tc := range []struct {
			name      string
			uid, gid  uint32
			wantChgrp bool
		}{
			{"root's, in another group", 0, 0, true},
			{"the daemon's own, in another group", daemonUID, 20, true},
			{"root's, in the daemon's group", 0, daemonGID, false},
		} {
			t.Run(what+"/"+tc.name, func(t *testing.T) {
				inspect := func(p string) (fileFacts, error) {
					info, err := os.Lstat(p)
					if err != nil {
						return fileFacts{}, err
					}
					f := fileFacts{uid: 0, mode: info.Mode(), isDir: info.IsDir(), symlink: info.Mode()&os.ModeSymlink != 0}
					if p == file {
						f.uid, f.gid, f.mode = tc.uid, tc.gid, 0o644
					}
					return f, nil
				}
				rt := &peerAuthRuntime{policy: policy, cfg: cfg.PeerAuth,
					env: peerAuthEnv{euid: daemonUID, egid: daemonGID, inspect: inspect, exe: func() (string, error) { return bin, nil }, resolve: lockOwnerResolve}}
				got := rt.trustedFileProblem(what, file)
				require.Contains(t, got, "every account can read")
				if tc.wantChgrp {
					require.Contains(t, got, "chgrp it to the daemon's group (480) and make it 0640")
				} else {
					require.Contains(t, got, "make it 0640, readable by the daemon's group only")
					require.NotContains(t, got, "chgrp")
				}
			})
		}
	}
}

// update.release_signers under peer_auth: serve refuses to start unless the
// file is root's -- not the daemon's account's, whose own updater it binds --
// through root's directories, and lists a key for the release namespace.
func TestFilesystemProblems_ReleaseSigners(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	etc := filepath.Join(root, "etc")
	require.NoError(t, os.MkdirAll(etc, 0o755))
	key, err := sshsigtest.NewKey(sshsig.KeySKEd25519)
	require.NoError(t, err)
	good := filepath.Join(etc, "release_signers")
	require.NoError(t, os.WriteFile(good, []byte(key.SignersLine("owner-a", "seamless-release")+"\n"), 0o644))
	daemonOwned := filepath.Join(etc, "daemon_signers")
	require.NoError(t, os.WriteFile(daemonOwned, []byte(key.SignersLine("owner-a", "seamless-release")+"\n"), 0o644))
	noKeys := filepath.Join(etc, "empty_signers")
	require.NoError(t, os.WriteFile(noKeys, []byte("# none\n"), 0o644))

	const daemon = 480
	inspect := func(p string) (fileFacts, error) {
		// The directories above the test's own are the machine's, and a CI
		// runner's /tmp is world-writable (sticky), which the pin refuses under
		// peer_auth: stand in root's 0755 for them, so the test judges only
		// the files it made.
		if rel, err := filepath.Rel(root, p); err != nil || strings.HasPrefix(rel, "..") {
			return fileFacts{uid: 0, mode: os.ModeDir | 0o755, isDir: true}, nil
		}
		info, err := os.Lstat(p)
		if err != nil {
			return fileFacts{}, err
		}
		f := fileFacts{uid: 0, mode: info.Mode(), isDir: info.IsDir(), symlink: info.Mode()&os.ModeSymlink != 0}
		if p == daemonOwned {
			f.uid = daemon
		}
		return f, nil
	}
	cfg := config.Defaults()
	cfg.PeerAuth = config.PeerAuth{Enabled: true, Owner: "owner", ForceLocked: []string{"p"}}
	policy, err := access.New(cfg.PeerAuth, lockOwnerResolve)
	require.NoError(t, err)
	rt := &peerAuthRuntime{policy: policy, cfg: cfg.PeerAuth,
		env: peerAuthEnv{euid: daemon, inspect: inspect, exe: func() (string, error) { return good, nil }, resolve: lockOwnerResolve}}
	pinProblems := func(path string) []string {
		c := cfg
		c.Update.ReleaseSigners = path
		var out []string
		for _, p := range rt.filesystemProblems(c, false) {
			if strings.Contains(p, "update.release_signers") {
				out = append(out, p)
			}
		}
		return out
	}
	require.Empty(t, pinProblems(""), "no pin, no problem")
	require.Empty(t, pinProblems(good), "root's 0644 file passes")
	require.Len(t, pinProblems(daemonOwned), 1)
	require.Contains(t, pinProblems(daemonOwned)[0], "under peer_auth it must be root's")
	// Both repairs root runs name sudo and its tool by their full paths.
	tools := shellcmd.ToolsFor(runtime.GOOS)
	require.Contains(t, pinProblems(daemonOwned)[0], "-- "+tools.Sudo+" "+tools.Chown+" root "+shellcmd.PosixArg(daemonOwned)+" --")
	require.True(t, strings.HasSuffix(pinProblems(daemonOwned)[0],
		" -- have root install it: /usr/bin/sudo /usr/bin/install -o root -m 0644 <release_signers> "+shellcmd.PosixArg(daemonOwned)), pinProblems(daemonOwned)[0])
	require.Contains(t, pinProblems(noKeys)[0], "no key for namespace seamless-release")
	require.Contains(t, pinProblems(filepath.Join(etc, "missing"))[0], "update.release_signers")
}
