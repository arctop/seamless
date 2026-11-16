//go:build darwin || linux

package main

// The peer_auth guide's layout through the whole doctor report, run as the
// daemon's account: the config root:_seamless 0640 under a root 0755
// /opt/seamless/etc, the TLS key root:_seamless 0640, data 0700 and the
// binaries the daemon's. A report on that layout must hold no chmod 600 or
// chmod 700 advice from any row: either one, followed, leaves the daemon's
// account unable to read its config or its key, and serve fails at its next
// start.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/relay"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// drDaemonUID plays the guide's _seamless (uid and group); drOwnerUID the owner.
const (
	drDaemonUID = 480
	drOwnerUID  = 4242
)

// drGuideLayout is the guide's /opt/seamless under a temp base.
type drGuideLayout struct {
	base, etc, cfgPath, tls, cert, key, bin, exe, data, run string
}

// drNewGuideLayout lays out the guide's table ("A layout that satisfies all of
// it") with its modes. The files belong to the test's uid; drGuideOwners
// reports the guide's owners for them.
func drNewGuideLayout(t *testing.T, peerAuthExtra string) drGuideLayout {
	t.Helper()
	// Short, as newPALayout's: the socket path sits under it.
	base, err := os.MkdirTemp("", "dr")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base, err = filepath.EvalSymlinks(base)
	require.NoError(t, err)
	root := filepath.Join(base, "opt", "seamless")
	l := drGuideLayout{base: base, etc: filepath.Join(root, "etc"), bin: filepath.Join(root, "bin"),
		data: filepath.Join(root, "data"), run: filepath.Join(root, "run")}
	l.cfgPath = filepath.Join(l.etc, "seamless.yaml")
	l.tls = filepath.Join(l.etc, "tls")
	l.cert, l.key = filepath.Join(l.tls, "cert.pem"), filepath.Join(l.tls, "key.pem")
	l.exe = filepath.Join(l.bin, "seamlessd")
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{
		{filepath.Join(base, "opt"), 0o755}, {root, 0o755}, {l.etc, 0o755}, {l.tls, 0o750},
		{l.bin, 0o755}, {l.data, 0o700}, {l.run, 0o755}, {filepath.Join(root, "home"), 0o700},
	} {
		require.NoError(t, os.Mkdir(d.path, d.mode))
		require.NoError(t, os.Chmod(d.path, d.mode)) // past the umask
	}
	certSrc, keySrc := writeCertPair(t, time.Now().Add(365*24*time.Hour), "localhost", "127.0.0.1")
	write := func(path string, data []byte, mode os.FileMode) {
		require.NoError(t, os.WriteFile(path, data, 0o600))
		require.NoError(t, os.Chmod(path, mode))
	}
	for src, dst := range map[string]string{certSrc: l.cert, keySrc: l.key} {
		data, err := os.ReadFile(src)
		require.NoError(t, err)
		mode := os.FileMode(0o644)
		if dst == l.key {
			mode = 0o640
		}
		write(dst, data, mode)
	}
	write(l.exe, []byte("#!/bin/sh\n"), 0o755)
	write(filepath.Join(l.bin, "seam"), []byte("#!/bin/sh\n"), 0o755)
	write(filepath.Join(l.etc, "daemon-account"), []byte("_seamless "+strconv.Itoa(drDaemonUID)+"\n"), 0o644)
	write(l.cfgPath, fmt.Appendf(nil, "data_dir: %s\naddr: 127.0.0.1:1\nmcp:\n  api_key: k\n"+
		"tls:\n  cert_file: %s\n  key_file: %s\n"+
		"peer_auth:\n  enabled: true\n  owner: _example_owner\n  force_locked: [example-finance]\n  socket:\n    path: %s\n%s",
		l.data, l.cert, l.key, filepath.Join(l.run, "seamlessd.sock"), peerAuthExtra), 0o640)
	return l
}

// drGuideOwners is inspectPath with the guide's owners: the daemon's account
// holds bin, data and run and what is in them; the config, the TLS directory
// and the key are root's with the daemon's group; every other path, the temp
// directories above the layout included, is root's.
func drGuideOwners(l drGuideLayout) func(string) (fileFacts, error) {
	daemonGroup := map[string]bool{l.cfgPath: true, l.tls: true, l.key: true}
	return func(p string) (fileFacts, error) {
		f, err := inspectPath(p)
		if err != nil {
			return f, err
		}
		p = filepath.Clean(p)
		f.uid, f.gid = 0, 0
		switch {
		case pathWithin(p, l.bin) || pathWithin(p, l.data) || pathWithin(p, l.run):
			f.uid, f.gid = drDaemonUID, drDaemonUID
		case daemonGroup[p]:
			f.gid = drDaemonUID
		}
		return f, nil
	}
}

// drGuideMachine is doctor run as the guide's daemon account on l: pinned by
// root, a system account, and not the owner.
func drGuideMachine(l drGuideLayout) doctorMachine {
	pin := func() (access.DaemonPin, bool, error) {
		return access.DaemonPin{Name: "_seamless", UID: drDaemonUID, Path: access.DaemonAccountPath}, true, nil
	}
	inspect := drGuideOwners(l)
	return doctorMachine{
		goos:         runtime.GOOS,
		sharedConfig: sharedConfigEnv{inspect: inspect, evalSymlinks: filepath.EvalSymlinks, daemonAccount: pin},
		// No relay on this computer.
		relay: relayDoctorEnv{layout: relay.Layout{DataDir: filepath.Join(l.base, "relay")}, env: relay.HostEnv()},
		peerAuth: peerAuthEnv{
			euid: drDaemonUID, egid: drDaemonUID,
			resolve: func(name string) (access.Account, error) {
				switch name {
				case "_example_owner":
					return access.Account{UID: drOwnerUID, Name: "_example_owner", Home: "/Users/example-owner"}, nil
				case "_seamless", strconv.Itoa(drDaemonUID):
					return access.Account{UID: drDaemonUID, Name: "_seamless", Home: "/opt/seamless/home"}, nil
				}
				return access.Account{}, fmt.Errorf("unknown account %q", name)
			},
			exe:           func() (string, error) { return l.exe, nil },
			inspect:       inspect,
			systemAccount: func(uid uint32) (bool, string) { return uid == drDaemonUID, "the test's daemon account" },
			daemonAccount: pin,
		},
	}
}

// drDoctorOutput runs the whole doctor on m against the config at path, in a
// home and a PATH that hold nothing: no agent client, no launchctl. It returns
// the report and doctor's verdict.
func drDoctorOutput(t *testing.T, path string, m doctorMachine) (string, error) {
	t.Helper()
	clearServeEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir())
	for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}
	t.Setenv("SEAMLESS_CONFIG", path)
	var runErr error
	out := captureStdout(t, func() error {
		runErr = doctorOn(nil, m)
		return nil
	})
	return out, runErr
}

// The guide's layout, run as the daemon's account, before the daemon's first
// start (no database) and after it (a database whose locked project is
// fenced): no row of the report advises chmod 600 or chmod 700, the config row
// defers to the preflight, the preflight passes, and nothing fails.
func TestDoctor_PeerAuthGuideLayoutGivesNoChmodAdvice(t *testing.T) {
	for _, tc := range []struct {
		name      string
		withDB    bool
		preflight string
	}{
		{"before the first start", false, "[info] peer_auth: the files and accounts pass serve's preflight; the projects it names were not checked"},
		{"with the daemon's database", true, "[ok  ] peer_auth: preflight passes: serve will start with the boundary up"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := drNewGuideLayout(t, "")
			if tc.withDB {
				ctx := context.Background()
				db, err := storetest.OpenFresh(t, filepath.Join(l.data, "seam.db"))
				require.NoError(t, err)
				_, err = store.EnsureProject(ctx, db, "example-finance", "example-finance")
				require.NoError(t, err)
				require.NoError(t, store.SetProjectIsolation(ctx, db, "example-finance", core.IsolationConfidential))
				require.NoError(t, db.Close())
			}

			out, err := drDoctorOutput(t, l.cfgPath, drGuideMachine(l))
			require.NoError(t, err, "no row fails on the guide's layout:\n%s", out)
			require.NotContains(t, out, "chmod 600", out)
			require.NotContains(t, out, "chmod 700", out)
			require.NotContains(t, out, "[fail]", out)
			require.Contains(t, out, "[info] config permissions: "+l.cfgPath+" is a peer_auth daemon's config", out)
			require.Contains(t, out, tc.preflight, out)
			require.Contains(t, out, "[ok  ] peer_auth.daemon_account: /opt/seamless/etc/daemon-account pins _seamless (uid 480), the account this runs as", out)
		})
	}
}

// The control for the test above: the same layout read without peer_auth --
// the rule every other server config keeps -- is the owner-only one, and its
// advice is the chmod 600 and chmod 700 that would lock the daemon out. A
// report that stopped deferring to the preflight would fail the test above.
func TestDoctor_GuideLayoutWithoutPeerAuthIsTheOwnerOnlyRule(t *testing.T) {
	l := drNewGuideLayout(t, "")
	data, err := os.ReadFile(l.cfgPath)
	require.NoError(t, err)
	off := strings.Replace(string(data), "  enabled: true\n", "  enabled: false\n", 1)
	require.NotEqual(t, string(data), off)
	require.NoError(t, os.WriteFile(l.cfgPath, []byte(off), 0o640))

	out, _ := drDoctorOutput(t, l.cfgPath, drGuideMachine(l)) //nolint:errcheck // the verdict is not the point: the config row is
	require.Contains(t, out, "[warn] config permissions: "+l.cfgPath+" mode 0640 grants group/other access; run: chmod 600", out)
	require.Contains(t, out, "directory "+l.etc+" mode 0755 grants group/other access; run: chmod 700", out)
}

// Run as the daemon's account with its config, the report holds no row about
// the owner's agent clients -- no hooks, codex, Claude app or skills row
// looking in the daemon account's home -- and one row saying who checks them.
func TestDoctor_PeerAuthDaemonConfigSkipsTheClientRows(t *testing.T) {
	l := drNewGuideLayout(t, "")
	out, err := drDoctorOutput(t, l.cfgPath, drGuideMachine(l))
	require.NoError(t, err, out)
	require.Contains(t, out, "  [info] "+daemonConfigClientsRow.name+": "+daemonConfigClientsRow.detail+"\n", out)
	for _, row := range []string{"] hooks:", "] hooks' seam:", "] codex", "] claude desktop mcp:", "] feature skills:", "runtime:"} {
		require.NotContains(t, out, row, out)
	}
	require.NotContains(t, out, "install-hooks", "no row offers an install-hooks run from the daemon's config:\n%s", out)
	require.NotContains(t, out, "/.claude", out)
}
