//go:build darwin || linux

package main

// L4: every command Seamless prints for root, or for the relay's or the
// daemon's account, to run through sudo names sudo by its full path, and the
// env such a sudo runs too (shellcmd.Sudo, shellcmd.Env): the owner's shell
// finds a bare name on the owner's PATH, and so does sudo wherever sudoers
// sets no secure_path, and that PATH can lead through a directory the owner's
// processes write. A fixed tool root runs is named from shellcmd.ToolsFor; a
// diagnostic the owner runs to look stays bare (owner decision 2026-11-18).

import (
	"io/fs"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/shellcmd"
	"github.com/arctop/seamless/internal/update"
)

var (
	// l4BareSudo is the word sudo not preceded by a directory: found on PATH.
	l4BareSudo = regexp.MustCompile(`(?:^|[^/\w.-])sudo(?:\s|$)`)
	// l4BareEnv is the word env, before an assignment, not by its path.
	l4BareEnv = regexp.MustCompile(`(?:^|[^/\w.-])env\s+[A-Za-z_][A-Za-z0-9_]*=`)
)

// l4SudoRuns is, for each sudo in text, the command it runs and the word
// after: its flags, -u's account and an env's assignments skipped.
func l4SudoRuns(text string) [][2]string {
	var out [][2]string
	for rest := text; ; {
		i := strings.Index(rest, shellcmd.Sudo+" ")
		if i < 0 {
			return out
		}
		rest = rest[i+len(shellcmd.Sudo)+1:]
		words := strings.Fields(rest)
		j := 0
		for j < len(words) && strings.HasPrefix(words[j], "-") {
			if words[j] == "-u" {
				j++
			}
			j++
		}
		if j < len(words) && words[j] == shellcmd.Env {
			for j++; j < len(words) && strings.Contains(words[j], "="); j++ {
			}
		}
		var run [2]string
		if j < len(words) {
			run[0] = strings.TrimRight(words[j], "),;")
		}
		if j+1 < len(words) {
			run[1] = words[j+1]
		}
		out = append(out, run)
	}
}

// l4BareOK is what a sudo may run by a bare name: a diagnostic the owner runs
// to look, and the tools shellcmd's table does not hold yet.
func l4BareOK(run [2]string) bool {
	switch run[0] {
	case "lsof":
		return true
	case "launchctl":
		return run[1] == "print"
	case "seamlessd":
		// A template's stand-in for the daemon's binary (the restore hint).
		return true
	}
	return false
}

// l4RequireNoBareRootCommand fails for a sudo or an env a printed command
// names by a bare name, and for a fixed tool a sudo runs by one.
func l4RequireNoBareRootCommand(t *testing.T, name, text string) {
	t.Helper()
	require.NotRegexp(t, l4BareSudo, text, "%s: a bare sudo", name)
	require.NotRegexp(t, l4BareEnv, text, "%s: a bare env", name)
	runs := l4SudoRuns(text)
	require.NotEmpty(t, runs, "%s: no sudo at all: %s", name, text)
	for _, run := range runs {
		require.True(t, strings.HasPrefix(run[0], "/") || l4BareOK(run), "%s: sudo runs %q by a bare name in: %s", name, run[0], text)
	}
}

// l4Renderers is every printed command family L4 touched, rendered.
func l4Renderers(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, goos := range []string{"darwin", "linux"} {
		out["relay restart "+goos] = relayRestartCommand(goos)
		out["relay socket repair "+goos] = relaySocketRepair(goos, true)
		out["retire restart "+goos] = retireRestartCommand(goos)

		// The shared client config a mode lets another account rewrite.
		c, ok := sharedClientConfigCheck("/opt/seamless/etc/client.yaml", goos, sharedConfigEnv{
			inspect: func(string) (fileFacts, error) { return fileFacts{mode: 0o664}, nil },
			daemonAccount: func() (access.DaemonPin, bool, error) {
				return access.DaemonPin{}, false, nil
			},
		})
		require.True(t, ok)
		require.Equal(t, statusWarn, c.status, c.detail)
		out["shared config chmod "+goos] = c.detail
		out["release signers install "+goos] = releaseSignersInstallCommand(goos, "/opt/seamless/etc/release_signers")
	}
	out["retire daemon command"] = retireDaemonCommand("_seamless", "approvers approve --code <code>")
	out["relay renewal"] = relayRenewalWords(devices.Renewal{NotAfter: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), Days: 13,
		Relays: []string{"example-laptop"}}, "https://box.example:8081")
	out["listen hint"] = listenHint("8081")
	out["pin"] = pinCommand("_seamless")

	// The updater's account under systemd, not lingering.
	out["linger"] = probeLinger(installProbeEnv{euid: 480,
		resolve:  func(string) (access.Account, error) { return access.Account{UID: 480, Name: "_seamless"}, nil },
		readFile: func(string) ([]byte, error) { return nil, fs.ErrNotExist }})

	// The release signers' file the daemon's account owns, under peer_auth.
	const daemon = 480
	signers := "/opt/seamless/etc/release_signers"
	out["release signers chown"] = releaseSignersFileProblem(signers, true, signersFS{euid: daemon,
		resolve: func(p string) (string, error) { return p, nil },
		inspect: func(p string) (fileFacts, error) {
			if p == signers {
				return fileFacts{uid: daemon, mode: 0o644}, nil
			}
			return fileFacts{uid: 0, mode: 0o755 | os.ModeDir, isDir: true}, nil
		}})

	// Root's group-writable directory above the file, under peer_auth.
	out["release signers chmod"] = releaseSignersFileProblem(signers, true, signersFS{euid: daemon,
		resolve: func(p string) (string, error) { return p, nil },
		inspect: func(p string) (fileFacts, error) {
			if p == signers {
				return fileFacts{uid: 0, mode: 0o644}, nil
			}
			if p == "/opt/seamless/etc" {
				return fileFacts{uid: 0, mode: 0o775 | os.ModeDir, isDir: true}, nil
			}
			return fileFacts{uid: 0, mode: 0o755 | os.ModeDir, isDir: true}, nil
		}})

	// A restore as root, and as an account that is not the daemon's.
	cfg := config.Defaults()
	cfg.DataDir = "/opt/seamless/data"
	err := restoreAccountCheck(cfg, restoreDeps{euid: 0})
	require.Error(t, err)
	out["restore as root"] = err.Error()
	err = restoreAccountCheck(cfg, restoreDeps{euid: 501, inspect: func(string) (fileFacts, error) {
		return fileFacts{uid: daemon, mode: 0o700 | os.ModeDir, isDir: true}, nil
	}})
	require.Error(t, err)
	out["restore as another account"] = err.Error()
	return out
}

func TestL4PrintedRootCommandsNameSudoAndEnvByPath(t *testing.T) {
	for name, text := range l4Renderers(t) {
		t.Run(name, func(t *testing.T) { l4RequireNoBareRootCommand(t, name, text) })
	}
}

// The texts themselves, word for word where the family is one command.
func TestL4PrintedRootCommandTexts(t *testing.T) {
	r := l4Renderers(t)
	for name, want := range map[string]string{
		"relay restart darwin":           "/usr/bin/sudo /bin/launchctl kickstart -k system/com.arctop.seamless.relay",
		"relay restart linux":            "/usr/bin/sudo /bin/systemctl restart seamless-relay",
		"release signers install darwin": "/usr/bin/sudo /usr/bin/install -o root -m 0644 <release_signers> /opt/seamless/etc/release_signers",
		"release signers install linux":  "/usr/bin/sudo /usr/bin/install -o root -m 0644 <release_signers> /opt/seamless/etc/release_signers",
		"pin": `echo "_seamless $(id -u _seamless)" | /usr/bin/sudo /usr/bin/tee /opt/seamless/etc/daemon-account >/dev/null && ` +
			"/usr/bin/sudo /bin/chmod 0644 /opt/seamless/etc/daemon-account",
		"listen hint": "/usr/bin/sudo lsof -nP -iTCP:8081 -sTCP:LISTEN",
	} {
		require.Equal(t, want, r[name], name)
	}
	for name, want := range map[string]string{
		"relay socket repair darwin": "start it (/usr/bin/sudo /bin/launchctl kickstart -k system/com.arctop.seamless.relay), and if it stops " +
			"again, doctor run as the relay's account says why (/usr/bin/sudo -u _seamless -H /opt/seamless/bin/seamlessd doctor)",
		"relay socket repair linux":  "start it (/usr/bin/sudo /bin/systemctl restart seamless-relay)",
		"shared config chmod darwin": "; run: /usr/bin/sudo /bin/chmod 644 '/opt/seamless/etc/client.yaml'",
		"shared config chmod linux":  "; run: /usr/bin/sudo /bin/chmod 644 '/opt/seamless/etc/client.yaml'",
		"relay renewal":              ": /usr/bin/sudo -u _seamless -H /opt/seamless/bin/seamlessd relay pair --server https://box.example:8081 ",
		"linger":                     "does not linger, and the updater runs as a unit of it: /usr/bin/sudo /bin/loginctl enable-linger _seamless",
		"restore as root":            "(/usr/bin/sudo -u <account> seamlessd restore ...), never as root",
		"restore as another account": "run it as the daemon's account (/usr/bin/sudo -u <account> seamlessd restore ...)",
	} {
		require.Contains(t, r[name], want, name)
	}
	chown := map[string]string{"darwin": "/usr/sbin/chown", "linux": "/bin/chown"}[runtime.GOOS]
	require.True(t, strings.HasSuffix(r["release signers chown"], " -- /usr/bin/sudo "+chown+" root /opt/seamless/etc/release_signers"),
		r["release signers chown"])
}

// The updater's record of a rollback that did not come up either names the
// sudo before its launchctl print by its full path; the print stays bare.
func TestL4SystemRollbackFailedNamesSudoByPath(t *testing.T) {
	w, s := newSystemWorld(t, "0.7.2", "0.7.3")
	s.broken["0.7.3"] = true
	s.broken["0.7.2"] = true
	a, err := runSystemAuto(t, w, systemDeps(w, s), "0.7.3")
	require.Error(t, err)
	require.Equal(t, update.StageRollback, a.Stage, a.Error)
	require.Contains(t, a.Error, "the rollback to v0.7.2 was not confirmed")
	require.Contains(t, a.Error, "check it with /usr/bin/sudo launchctl print system/"+systemDaemonLabel+" on macOS or systemctl status "+
		systemDaemonUnit+" on Linux")
	l4RequireNoBareRootCommand(t, "system rollback failed", a.Error)
}

// The scanner itself: it finds what it is for, and passes what it is not.
func TestL4ScannerCatchesBareWords(t *testing.T) {
	for _, bad := range []string{
		"sudo /bin/chmod 0644 /x",
		"run: sudo -u _seamless /opt/seamless/bin/seamlessd doctor",
		"echo x | sudo /usr/bin/tee /x",
		"(sudo lsof -nP -iTCP:1 -sTCP:LISTEN)",
		"/usr/bin/sudo -k -u _seamless env SEAMLESS_CONFIG=/x /opt/seamless/bin/seamlessd approve",
	} {
		ok := !l4BareSudo.MatchString(bad) && !l4BareEnv.MatchString(bad)
		require.False(t, ok, bad)
	}
	for _, run := range [][2]string{{"launchctl", "kickstart"}, {"systemctl", "restart"}, {"chmod", "0644"}, {"tee", "/x"}} {
		require.False(t, l4BareOK(run), "%v", run)
	}
	require.Equal(t, [][2]string{{"/usr/bin/tee", "/x"}, {"/opt/seamless/bin/seamlessd", "approve"}, {"/opt/x", ""}},
		l4SudoRuns("echo a | /usr/bin/sudo /usr/bin/tee /x && /usr/bin/sudo -k -u _seamless /usr/bin/env SEAMLESS_CONFIG=/c "+
			"/opt/seamless/bin/seamlessd approve && /usr/bin/sudo -e /opt/x"))
	require.NotRegexp(t, l4BareSudo, "SUDO_UID, sudoers, pseudo code")
}
