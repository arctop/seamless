//go:build darwin || linux

package main

// The Server's address step under locked projects (wave nt1113g): its root
// step had root copy the owner's TLS certificate and key by the paths the
// owner gave (`sudo install ... <cert> ...`), following a symlink to a file
// only root reads into a world-readable copy -- M1 of the second review, in
// another file -- and named its tools by the owner's PATH (its L4). Written
// against what setup prints, not its insides: the printed step runs for real
// under /bin/sh, as an owner pastes it, with every root tool a stand-in that
// runs as this user and /opt under a scratch directory (rootstepRunBlock):
// no sudo, nothing under /opt.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// addrstepOutput is what a locked-projects Server's setup prints on goos with
// the owner's TLS pair at cert and key for setupAddrDomain.
func addrstepOutput(t *testing.T, goos, cert, key string) string {
	t.Helper()
	r, _, _ := setupTestRun(t, goos, []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=" + setupAddrDomain,
		"SEAMLESS_TLS_CERT=" + cert, "SEAMLESS_TLS_KEY=" + key}, setupFactsLocked, false, "")
	setupAddrArrange(t, r, map[string][]string{setupAddrDomain: {setupAddrLANv4}}, setupAddrIfaces, tailscaleStatus{}, errNoTailscale)
	r.facts.configFound = true
	out, _, err := setupAddrFirstHalf(t, r)
	require.NoError(t, err, out)
	return out
}

// addrstepTailnetOutput is the same for this computer's tailnet name, whose
// certificate root asks the tailnet for.
func addrstepTailnetOutput(t *testing.T, goos string) string {
	t.Helper()
	r, _, _ := setupTestRun(t, goos, []string{"SEAMLESS_SETUP=server", "SEAMLESS_SERVER_NAME=" + setupAddrTSName}, setupFactsLocked, false, "")
	setupAddrArrange(t, r, map[string][]string{setupAddrTSName: {setupAddrTailv4, setupAddrTailv6}}, setupAddrIfaces, setupAddrTailnetUp, nil)
	r.facts.configFound = true
	out, _, err := setupAddrFirstHalf(t, r)
	require.NoError(t, err, out)
	return out
}

// addrstepRootLines is the root step's command lines as the owner pastes
// them: those under the "root steps" row, up to the daemon's config.
func addrstepRootLines(t *testing.T, out string) []string {
	t.Helper()
	var lines []string
	in := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "  root steps ") {
			in = true
			continue
		}
		if !in {
			continue
		}
		if strings.Contains(line, "then put these in the daemon's config") {
			break
		}
		if cmd, ok := strings.CutPrefix(line, setupRowCont+"  "); ok {
			lines = append(lines, cmd)
		}
	}
	require.NotEmpty(t, lines, "the root step in:\n%s", out)
	return lines
}

// addrstepScratchPair writes a pair for setupAddrDomain into dir, the key
// this account's alone, and returns the paths and their bytes.
func addrstepScratchPair(t *testing.T, dir, certName, keyName string) (cert, key string, certPEM, keyPEM []byte) {
	t.Helper()
	cert, key = filepath.Join(dir, certName), filepath.Join(dir, keyName)
	setupAddrWritePair(t, cert, key, time.Now().Add(365*24*time.Hour), setupAddrDomain)
	var err error
	certPEM, err = os.ReadFile(cert)
	require.NoError(t, err)
	keyPEM, err = os.ReadFile(key)
	require.NoError(t, err)
	return cert, key, certPEM, keyPEM
}

// addrstepTLSDir is the guide's TLS directory under scratch, as the stand-ins
// see /opt.
func addrstepTLSDir(scratch string) string {
	return filepath.Join(scratch, "opt", "seamless", "etc", "tls")
}

// Root opens no file the owner can write -- the owner's shell opens the
// certificate and the key, so a symlink to a file only root reads fails
// before root reads anything -- and a copy that is not what setup read and
// checked stops the block before it is in place. What failed is root's alone.
func TestAddrStepReview_RootOpensNoFileOfTheOwnersAndAFailedCheckStopsTheBlock(t *testing.T) {
	const secret = "ROOT-ONLY-SECRET"
	onlyRoot := func(t *testing.T, scratch string) string {
		p := filepath.Join(scratch, "secret")
		require.NoError(t, os.WriteFile(p, []byte(secret+"\n"), 0o600))
		require.NoError(t, os.Chmod(p, 0))
		t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
		return p
	}
	relink := func(t *testing.T, path, to string) {
		require.NoError(t, os.Remove(path))
		require.NoError(t, os.Symlink(to, path))
	}
	for _, tc := range []struct {
		name string
		// paste changes the owner's files after setup printed the step and
		// before the owner pastes it, as a process of the owner's could.
		paste func(t *testing.T, cert, key, scratch string)
		// stops names the copy the block stops at; "" when both go in place.
		stops string
	}{
		{name: "the pair setup read", paste: func(*testing.T, string, string, string) {}},
		{name: "the key turned into a symlink to a file only root reads", stops: "key", paste: func(t *testing.T, _, key, scratch string) {
			relink(t, key, onlyRoot(t, scratch))
		}},
		{name: "the certificate turned into a symlink to a file only root reads", stops: "crt", paste: func(t *testing.T, cert, _, scratch string) {
			relink(t, cert, onlyRoot(t, scratch))
		}},
		{name: "the key swapped after setup read it", stops: "key", paste: func(t *testing.T, _, key, scratch string) {
			_, other, _, _ := addrstepScratchPair(t, filepath.Join(scratch, "other"), "o.crt", "o.key")
			data, err := os.ReadFile(other)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(key, data, 0o600))
		}},
		{name: "the certificate turned into a symlink to another file of the owner's", stops: "crt", paste: func(t *testing.T, cert, _, scratch string) {
			notes := filepath.Join(scratch, "owner", "notes.txt")
			require.NoError(t, os.WriteFile(notes, []byte("the owner's private notes\n"), 0o600))
			relink(t, cert, notes)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratch := t.TempDir()
			cert, key, certPEM, keyPEM := addrstepScratchPair(t, filepath.Join(scratch, "owner"), "server.crt", "server.key")
			lines := addrstepRootLines(t, addrstepOutput(t, runtime.GOOS, cert, key))
			tc.paste(t, cert, key, scratch)

			log, err := rootstepRunBlock(t, strings.Join(lines, "\n")+"\n", scratch)
			require.NotContains(t, log, cert, "root's commands never name the owner's certificate")
			require.NotContains(t, log, key, "root's commands never name the owner's key")
			tls := addrstepTLSDir(scratch)
			placed := map[string]string{"crt": filepath.Join(tls, setupAddrDomain+".crt"), "key": filepath.Join(tls, setupAddrDomain+".key")}
			if tc.stops != "" {
				require.Error(t, err, "the block stops")
				require.NoFileExists(t, placed[tc.stops], "nothing that failed is in place")
				require.NoFileExists(t, placed["key"], "and nothing after it")
				entries, rerr := os.ReadDir(tls)
				require.NoError(t, rerr)
				for _, e := range entries {
					info, ierr := os.Lstat(filepath.Join(tls, e.Name()))
					require.NoError(t, ierr)
					if !info.Mode().IsRegular() {
						continue
					}
					got, rerr := os.ReadFile(filepath.Join(tls, e.Name()))
					require.NoError(t, rerr)
					require.NotContains(t, string(got), secret, "root read nothing of a file only root reads: %s", e.Name())
					if e.Name() != setupAddrDomain+".crt" {
						require.Zero(t, info.Mode().Perm()&0o077, "%s, which failed or never ran its check, is root's alone: %v", e.Name(), info.Mode())
					}
				}
				return
			}
			require.NoError(t, err)
			for _, f := range []struct {
				path string
				want []byte
				mode os.FileMode
			}{{placed["crt"], certPEM, 0o644}, {placed["key"], keyPEM, 0o640}} {
				got, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.Equal(t, string(f.want), string(got), "what setup read is what is in place")
				info, err := os.Stat(f.path)
				require.NoError(t, err)
				require.Equal(t, f.mode, info.Mode().Perm(), f.path)
			}
			entries, err := os.ReadDir(tls)
			require.NoError(t, err)
			require.Len(t, entries, 2, "no staged copy is left behind")
		})
	}
}

// addrstepKeyExposures runs lines one command at a time, as the AND-list
// would, under a umask that hides nothing, and reports every moment a file
// of the key's (the staged copy, then the key in place) was readable by an
// account other than root's -- by any account at all, or by a group before a
// command has made it the daemon's group's.
func addrstepKeyExposures(t *testing.T, lines []string, scratch string) []string {
	t.Helper()
	tls := addrstepTLSDir(scratch)
	var exposed []string
	groupMade := false
	seen := false
	for i, line := range lines {
		cmd := strings.TrimSuffix(line, " &&")
		_, err := rootstepRunBlock(t, "umask 000\n"+cmd+"\n", scratch)
		require.NoError(t, err, "command %d: %s", i+1, cmd)
		if strings.Contains(cmd, ".key") && (strings.Contains(cmd, "root:_seamless ") || strings.Contains(cmd, "-g _seamless ")) {
			groupMade = true
		}
		entries, err := os.ReadDir(tls)
		if os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err)
		for _, e := range entries {
			if !strings.Contains(e.Name(), ".key") {
				continue
			}
			seen = true
			info, err := os.Lstat(filepath.Join(tls, e.Name()))
			require.NoError(t, err)
			perm := info.Mode().Perm()
			switch {
			case perm&0o007 != 0:
				exposed = append(exposed, fmt.Sprintf("after %q, %s is mode %04o: every account reads it", cmd, e.Name(), perm))
			case perm&0o070 != 0 && !groupMade:
				exposed = append(exposed, fmt.Sprintf("after %q, %s is mode %04o before it is the daemon's group's", cmd, e.Name(), perm))
			}
		}
	}
	require.True(t, seen, "the key was never copied")
	return exposed
}

// The key is secret: at no step is root's copy of it readable by another
// account, whatever umask sudo runs tee with -- root's copy is made 0600
// before tee fills it, and its group may read it only once it is the
// daemon's.
func TestAddrStepReview_TheKeyIsNeverReadableByAnotherAccount(t *testing.T) {
	scratch := t.TempDir()
	cert, key, _, keyPEM := addrstepScratchPair(t, filepath.Join(scratch, "owner"), "server.crt", "server.key")
	lines := addrstepRootLines(t, addrstepOutput(t, runtime.GOOS, cert, key))
	require.Empty(t, addrstepKeyExposures(t, lines, scratch))
	got, err := os.ReadFile(filepath.Join(addrstepTLSDir(scratch), setupAddrDomain+".key"))
	require.NoError(t, err)
	require.Equal(t, string(keyPEM), string(got))
}

// Hostile paths -- the owner's own, which setup takes as the full paths they
// are -- are opened as the one word each is, and nothing they spell runs:
// the printed step parses in every shell an owner pastes into and runs
// under /bin/sh with the stand-ins.
func TestAddrStepReview_HostilePathsAreInert(t *testing.T) {
	scratch := t.TempDir()
	marker := filepath.Join(scratch, "ran")
	inject := "$(touch ran);touch ran`touch ran`'\"*!&|<x>"
	cert, key, certPEM, keyPEM := addrstepScratchPair(t, filepath.Join(scratch, "my pair", inject), "server "+inject+".crt", inject+".key")
	lines := addrstepRootLines(t, addrstepOutput(t, runtime.GOOS, cert, key))
	text := strings.Join(lines, "\n") + "\n"
	for _, sh := range rootstepShells(t) {
		out, err := exec.Command(sh, "-n", "-c", text).CombinedOutput()
		require.NoError(t, err, "%s:\n%s\n%s", sh, out, text)
	}
	log, err := rootstepRunBlock(t, text, scratch)
	require.NoError(t, err)
	require.NoFileExists(t, marker, "the step ran what a path spells:\n%s", text)
	require.NotContains(t, log, inject, "root's commands carry no path of the owner's")
	for name, want := range map[string][]byte{setupAddrDomain + ".crt": certPEM, setupAddrDomain + ".key": keyPEM} {
		got, err := os.ReadFile(filepath.Join(addrstepTLSDir(scratch), name))
		require.NoError(t, err)
		require.Equal(t, string(want), string(got), name)
	}
}

// Every command the address step prints for root, for macOS and Linux, from
// the owner's files and from the tailnet, parses in every shell an owner
// pastes into, and names each tool by its full path.
func TestAddrStepReview_EveryRootCommandParsesAndNamesItsTools(t *testing.T) {
	scratch := t.TempDir()
	cert, key, _, _ := addrstepScratchPair(t, filepath.Join(scratch, "owner"), "server.crt", "server.key")
	for _, goos := range []string{"darwin", "linux"} {
		for name, out := range map[string]string{"files": addrstepOutput(t, goos, cert, key), "tailnet": addrstepTailnetOutput(t, goos)} {
			text := strings.Join(addrstepRootLines(t, out), "\n") + "\n"
			for _, sh := range rootstepShells(t) {
				said, err := exec.Command(sh, "-n", "-c", text).CombinedOutput()
				require.NoError(t, err, "%s, %s, %s:\n%s\n%s", sh, goos, name, said, text)
			}
			rootstepPATHFree(t, out)
			require.NotContains(t, out, "\n"+setupRowCont+"  sudo ", "%s %s: no bare sudo", goos, name)
		}
	}
}

// The Server's closing under locked projects names sudo and env by their
// full paths: the command runs as the daemon's account, which reaches every
// locked project.
func TestAddrStepReview_TheDevicesCodeCommandNamesItsTools(t *testing.T) {
	r, out, _ := setupTestRun(t, "linux", nil, setupFactsLocked, false, "")
	r.loadConfig = func() (config.Config, error) {
		cfg := config.Defaults()
		cfg.Role, cfg.AdvertisedURL = config.RoleClient, "unix:///opt/seamless/run/seamlessd.sock"
		return cfg, nil
	}
	require.NoError(t, setupClientsStep(r, &setupState{}))
	require.Contains(t, out.String(), "seamlessd devices code")
	rootstepPATHFree(t, out.String())
}
