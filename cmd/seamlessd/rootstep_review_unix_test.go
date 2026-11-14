//go:build darwin || linux

package main

// M1 of the second review (wave nt1113f): the relay's root step had root open
// files the owner can write -- following a symlink to a file only root reads
// -- and checked root's copy only after it was in place, on a line that
// stopped nothing. Here the printed block runs for real under /bin/sh, as an
// owner pastes it, with every root tool a stand-in that runs as this user and
// every root path under a scratch directory: no sudo, nothing under /opt.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// rootstepRelayOutput is what a relay Client's setup prints for this
// computer's OS: the seamlessd it ran as is genuine, the installer's bin is
// installDir, and the Server presents cert.
func rootstepRelayOutput(t *testing.T, installDir, genuine string, cert clientServerCert) string {
	t.Helper()
	env := []string{"SEAMLESS_SETUP=client", "SEAMLESS_SERVER_URL=https://box.example:8081", "SEAMLESS_PAIR_CODE=ABCD-EFGH-JKMN-PQRS",
		"SEAMLESS_INSTALL_DIR=" + installDir}
	if !cert.trusted {
		env = append(env, "SEAMLESS_SERVER_FINGERPRINT="+cert.fingerprint)
	}
	r, out, _ := setupTestRun(t, runtime.GOOS, env, setupFactsFresh, false, "")
	clientArrange(t, r, cert)
	r.client.exe = func() (string, error) { return genuine, nil }
	st, err := clientFirstHalf(t, r)
	require.NoError(t, err, out.String())
	out.Reset()
	require.NoError(t, setupHandOff(r, &st, setupStepsLeft(st, runtime.GOOS), true))
	return out.String()
}

// rootstepBlock is block n of the printed root step, as the owner pastes it.
func rootstepBlock(t *testing.T, out string, n int) string {
	t.Helper()
	header := regexp.MustCompile(`^\s+(\d)\. `)
	var block []string
	in := false
	for _, line := range strings.Split(out, "\n") {
		if m := header.FindStringSubmatch(line); m != nil {
			in = m[1] == strconv.Itoa(n)
			continue
		}
		if !in {
			continue
		}
		if strings.TrimSpace(line) == "" {
			if len(block) > 0 {
				break
			}
			continue
		}
		block = append(block, strings.TrimPrefix(line, setupRowCont+"  "))
	}
	require.NotEmpty(t, block, "block %d in:\n%s", n, out)
	return strings.Join(block, "\n") + "\n"
}

// rootstepStandIns are the root tools a printed block may run, and what each
// stand-in does: sudo logs its words and runs its command as this user;
// install and chown leave out the ownership this user cannot give; id answers
// the relay's uid; the account and service tools only log.
var rootstepStandIns = map[string]string{
	"sudo": `printf '%s\037' "$@" >>"$ROOTSTEP_LOG"; printf '\n' >>"$ROOTSTEP_LOG"
while [ $# -gt 0 ]; do case $1 in -u) shift 2 ;; -*) shift ;; *) break ;; esac; done
exec "$@"`,
	"install": `skip=
for a in "$@"; do shift; if [ -n "$skip" ]; then skip=; continue; fi
case $a in -o|-g) skip=1; continue ;; esac; set -- "$@" "$a"; done
exec /usr/bin/install "$@"`,
	"chown":     `exit 0`,
	"id":        `echo 480`,
	"tee":       `exec /usr/bin/tee "$@"`,
	"chmod":     `exec /bin/chmod "$@"`,
	"mv":        `exec /bin/mv "$@"`,
	"shasum":    `if [ -x /usr/bin/shasum ]; then exec /usr/bin/shasum "$@"; fi; shift 2; exec sha256sum "$@"`,
	"sha256sum": `if [ -x /usr/bin/sha256sum ]; then exec /usr/bin/sha256sum "$@"; fi; exec /usr/bin/shasum -a 256 "$@"`,
	"dscl":      `exit 0`, "launchctl": `exit 0`, "useradd": `exit 0`, "groupadd": `exit 0`, "usermod": `exit 0`, "systemctl": `exit 0`,
	"env": `exec /usr/bin/env "$@"`,
}

// rootstepRunBlock runs block under /bin/sh in scratch, its root tools the
// stand-ins (by their full paths, and on PATH for a bare name) and /opt
// under scratch. It returns the run's error and sudo's log: one line of
// words, \037-separated, per sudo run.
func rootstepRunBlock(t *testing.T, block, scratch string) (string, error) {
	t.Helper()
	stub := filepath.Join(scratch, "stub")
	require.NoError(t, os.MkdirAll(stub, 0o755))
	var pairs []string
	for name, body := range rootstepStandIns {
		require.NoError(t, os.WriteFile(filepath.Join(stub, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755))
		for _, dir := range []string{"/usr/bin/", "/bin/", "/usr/sbin/"} {
			pairs = append(pairs, dir+name, filepath.Join(stub, name))
		}
	}
	pairs = append(pairs, "/opt/seamless", filepath.Join(scratch, "opt", "seamless"))
	text := strings.NewReplacer(pairs...).Replace(block)
	// No path the block names may start at this computer's own root
	// directories, or be its sudo.
	live := regexp.MustCompile(`(^|[\s'"=<>|;&(])(/opt/|/Library/|/etc/|/usr/bin/sudo|/usr/local/)`)
	require.False(t, live.MatchString(text), "the block would reach this computer's own files:\n%s", text)
	log := filepath.Join(scratch, "sudo.log")
	cmd := exec.Command("/bin/sh", "-c", text)
	cmd.Dir, cmd.Env = scratch, []string{"PATH=" + stub, "HOME=" + filepath.Join(scratch, "home"), "ROOTSTEP_LOG=" + log}
	out, err := cmd.CombinedOutput()
	t.Logf("the block ran: %v\n%s", err, out)
	data, rerr := os.ReadFile(log)
	if rerr != nil && !os.IsNotExist(rerr) {
		require.NoError(t, rerr)
	}
	return string(data), err
}

// M1: root opens no file the owner can write -- the owner's shell opens the
// seamlessd to copy, so a symlink to a file only root reads fails before root
// runs -- and a copy that is not the binary setup ran as stops the block
// before it is in place, made root's, or pinned.
func TestRootStepReview_RootOpensNoFileOfTheOwnersAndAFailedCheckStopsTheBlock(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, source, genuine, scratch string)
		placed  bool
	}{
		{name: "the binary setup ran as", placed: true, arrange: func(t *testing.T, source, genuine, _ string) {
			data, err := os.ReadFile(genuine)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(source, data, 0o755))
		}},
		{name: "a symlink to a file only root reads", arrange: func(t *testing.T, source, _, scratch string) {
			secret := filepath.Join(scratch, "secret")
			require.NoError(t, os.WriteFile(secret, []byte("ROOT-ONLY-SECRET\n"), 0o600))
			require.NoError(t, os.Chmod(secret, 0))
			t.Cleanup(func() { _ = os.Chmod(secret, 0o600) })
			require.NoError(t, os.Symlink(secret, source))
		}},
		{name: "a binary swapped after setup read it", arrange: func(t *testing.T, source, _, _ string) {
			require.NoError(t, os.WriteFile(source, []byte("#!/bin/sh\necho not the release\n"), 0o755))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratch := t.TempDir()
			genuine := filepath.Join(scratch, "release", "seamlessd")
			installDir := filepath.Join(scratch, "home", ".local", "bin")
			for _, d := range []string{filepath.Dir(genuine), installDir} {
				require.NoError(t, os.MkdirAll(d, 0o755))
			}
			require.NoError(t, os.WriteFile(genuine, []byte("#!/bin/sh\necho the release\n"), 0o755))
			source := filepath.Join(installDir, "seamlessd")
			tc.arrange(t, source, genuine, scratch)

			out := rootstepRelayOutput(t, installDir, genuine, clientTrusted)
			log, err := rootstepRunBlock(t, rootstepBlock(t, out, 2), scratch)
			require.NotContains(t, log, source, "root's commands never name the owner's seamlessd")
			placed := filepath.Join(scratch, "opt", "seamless", "bin", "seamlessd")
			pin := filepath.Join(scratch, "opt", "seamless", "etc", "daemon-account")
			if !tc.placed {
				require.Error(t, err, "the block stops")
				require.NoFileExists(t, placed, "nothing that failed the check is in place")
				require.NoFileExists(t, pin, "nor pinned")
				return
			}
			require.NoError(t, err)
			got, err := os.ReadFile(placed)
			require.NoError(t, err)
			require.Equal(t, "#!/bin/sh\necho the release\n", string(got))
			info, err := os.Stat(placed)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
			pinned, err := os.ReadFile(pin)
			require.NoError(t, err)
			require.Equal(t, "_seamless 480\n", string(pinned))
		})
	}
}

// M1: the certificate the owner compared reaches root's copy from the step
// itself; root reads no copy of the owner's for it.
func TestRootStepReview_RootReadsNoCertificateOfTheOwners(t *testing.T) {
	scratch := t.TempDir()
	genuine := filepath.Join(scratch, "release", "seamlessd")
	installDir := filepath.Join(scratch, "home", ".local", "bin")
	for _, d := range []string{filepath.Dir(genuine), installDir} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	require.NoError(t, os.WriteFile(genuine, []byte("#!/bin/sh\necho the release\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(installDir, "seamlessd"), []byte("#!/bin/sh\necho the release\n"), 0o755))
	cert := rootstepCert(t)

	out := rootstepRelayOutput(t, installDir, genuine, cert)
	log, err := rootstepRunBlock(t, rootstepBlock(t, out, 2), scratch)
	require.NoError(t, err)
	require.NotContains(t, log, "/h/.config", "root's commands never name the owner's copy")
	got, err := os.ReadFile(filepath.Join(scratch, "opt", "seamless", "etc", "server-ca.crt"))
	require.NoError(t, err)
	require.Equal(t, string(cert.pem), string(got))
}
