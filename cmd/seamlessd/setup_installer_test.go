//go:build unix

package main

// The installer's half of the setup hand-off, run for real: docs/install in
// /bin/sh, reading itself from stdin as `curl | sh` does, against a release
// that a stand-in curl serves from a local directory, with a stand-in
// seamlessd that records every call and plays `seamlessd setup` from a canned
// answer. Every child runs in a new session (no controlling terminal, so no
// /dev/tty) with an environment built from scratch: a temp HOME, TMPDIR and
// install dir, and stand-ins for curl, launchctl, systemctl and loginctl
// first on PATH. Nothing reaches the network, the real home, launchd or
// systemd, and no port is dialed: the stand-in curl answers /healthz itself.
//
// This file uses nothing but what the package had before setup existed, so it
// also runs against the installer as it was: the runs without a setup knob
// must pass there too -- that is "unattended means unchanged" -- and the runs
// with one must fail there.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// setupE2EVersion is the release the stand-in serves: one at the owner's
// signature floor, so the installer installs it with no checksums.txt.sshsig.
const setupE2EVersion = "0.8.3"

// setupE2ECurl stands in for curl: release assets from $SETUP_E2E_RELEASE,
// the latest-release API, and /healthz answered as the release -- anything
// else fails as curl -f does.
const setupE2ECurl = `#!/bin/sh
out=
url=
code=
while [ $# -gt 0 ]; do
	case $1 in
	-o) out=$2; shift ;;
	-w) code=1; shift ;;
	--max-time) shift ;;
	-*) ;;
	*) url=$1 ;;
	esac
	shift
done
printf '%s\n' "$url" >>"$SETUP_E2E_DIR/curl.log"
emit() {
	if [ -n "$out" ]; then cat >"$out"; else cat; fi
}
case $url in
https://github.com/arctop/seamless/releases/download/v*/*)
	f=$SETUP_E2E_RELEASE/${url##*/}
	if [ ! -f "$f" ]; then
		[ -z "$code" ] || printf 404
		exit 22
	fi
	emit <"$f"
	;;
https://api.github.com/repos/arctop/seamless/releases/latest)
	printf '{"tag_name": "v%s"}\n' "$SETUP_E2E_VERSION" | emit
	;;
*/healthz)
	printf '{"status":"ok","version":"%s+e2e"}\n' "$SETUP_E2E_VERSION" | emit
	;;
*) exit 22 ;;
esac
`

// setupE2EService stands in for launchctl, systemctl and loginctl: it records
// the call. launchctl reports the current label's job loaded (so a bootstrap
// "worked") and any other not; systemctl has no user session, so the
// installer skips the service there, as on a box without one.
const setupE2EService = `#!/bin/sh
printf '%s %s\n' "${0##*/}" "$*" >>"$SETUP_E2E_DIR/service.log"
case ${0##*/} in
launchctl)
	[ "$1" = print ] || exit 0
	case $2 in gui/*/com.arctop.seamless) exit 0 ;; esac
	exit 1
	;;
esac
exit 1
`

// setupE2ESeamlessd stands in for the release's seamlessd. Every call is one
// line of $SETUP_E2E_DIR/calls ("seamlessd [arg] [arg]..."). It answers the
// installer's probes as the current release does; install-hooks writes a
// first config as the real bootstrap would; setup -h prints the protocol line
// unless SETUP_E2E_SETUP=old (a release from before setup), and setup itself
// writes $SETUP_E2E_STATE to its --state file, or exits with
// $SETUP_E2E_SETUP_EXIT. With SETUP_E2E_SETUP=real every setup call runs the
// real one in this test binary instead (TestSetupHelperSeamlessd).
const setupE2ESeamlessd = `#!/bin/sh
{
	printf 'seamlessd'
	for a in "$@"; do printf ' [%s]' "$a"; done
	printf '\n'
} >>"$SETUP_E2E_DIR/calls"
case $1 in
setup)
	if [ -t 0 ]; then echo tty >>"$SETUP_E2E_DIR/setup-stdin"; else echo none >>"$SETUP_E2E_DIR/setup-stdin"; fi
	case ${SETUP_E2E_SETUP:-} in
	old)
		printf 'unknown command "setup"\n' >&2
		exit 2
		;;
	real)
		SETUP_E2E_HELPER_RUN=1 exec "$SETUP_E2E_HELPER" -test.run='^TestSetupHelperSeamlessd$' -- "$@"
		;;
	esac
	if [ "${2:-}" = -h ]; then
		printf 'usage: seamlessd setup\n\ninstaller protocol: 1\n'
		exit 0
	fi
	state=
	finish=
	while [ $# -gt 0 ]; do
		case $1 in
		--state) state=$2; shift ;;
		--finish) finish=1 ;;
		esac
		shift
	done
	[ -z "$finish" ] || exit 0
	[ "${SETUP_E2E_SETUP_EXIT:-0}" = 0 ] || exit "$SETUP_E2E_SETUP_EXIT"
	printf '%s' "$SETUP_E2E_STATE" >"$state"
	exit 0
	;;
install-hooks)
	if [ "${2:-}" = -h ]; then
		printf '%s\n' '  -client string' '  -server-url URL' '  -skills' '    claude-desktop'
		exit 0
	fi
	cfg=${SEAMLESS_CONFIG:-$HOME/.config/seamless/seamless.yaml}
	if [ ! -f "$cfg" ]; then
		mkdir -p "${cfg%/*}"
		printf 'mcp:\n  api_key: "generated"\n' >"$cfg"
	fi
	;;
status)
	printf 'usage: seamlessd status\n\nlaunchd label: com.arctop.seamless\n'
	;;
esac
exit 0
`

// setupInstallerWorld is one installer run's scratch machine, run by shell.
type setupInstallerWorld struct {
	dir, home, bin, release, installDir, tmp string
	shell                                    string
	script                                   []byte
	checksumsSHA                             string
}

// setupInstallerShells is every POSIX shell the installer is held to here:
// /bin/sh, and dash where it is not /bin/sh already -- macOS's /bin/sh is
// bash, Debian's and Ubuntu's is dash, and the two part ways on what a failed
// redirection does to a non-interactive shell.
func setupInstallerShells() []string {
	shells := []string{"/bin/sh"}
	if _, err := os.Stat("/bin/dash"); err == nil {
		if same, _ := filepath.EvalSymlinks("/bin/sh"); same != "/bin/dash" {
			shells = append(shells, "/bin/dash")
		}
	}
	return shells
}

// setupEachShell runs body once per setupInstallerShells, as a subtest.
func setupEachShell(t *testing.T, body func(t *testing.T, shell string)) {
	for _, shell := range setupInstallerShells() {
		t.Run(filepath.Base(shell), func(t *testing.T) { body(t, shell) })
	}
}

// setupE2ESupported skips where the installer does not run.
func setupE2ESupported(t *testing.T) {
	t.Helper()
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("the installer ships amd64 and arm64 only")
	}
	for _, tool := range []string{"/bin/sh", "/usr/bin/env"} {
		if _, err := os.Stat(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
}

// newSetupInstallerWorld stands the scratch machine up around the installer
// at script, which shell runs.
func newSetupInstallerWorld(t *testing.T, shell, script string) *setupInstallerWorld {
	t.Helper()
	setupE2ESupported(t)
	dir := t.TempDir()
	w := &setupInstallerWorld{
		shell: shell,
		dir:   dir, home: filepath.Join(dir, "home"), bin: filepath.Join(dir, "bin"),
		release: filepath.Join(dir, "release"), installDir: filepath.Join(dir, "home", ".local", "bin"),
		tmp: filepath.Join(dir, "tmp"),
	}
	for _, d := range []string{w.home, w.bin, w.release, w.tmp, filepath.Join(w.home, ".claude")} {
		require.NoError(t, os.MkdirAll(d, 0o700))
	}
	raw, err := os.ReadFile(script)
	require.NoError(t, err)
	w.script = raw
	require.NoError(t, os.WriteFile(filepath.Join(w.bin, "curl"), []byte(setupE2ECurl), 0o755))
	for _, tool := range []string{"launchctl", "systemctl", "loginctl"} {
		require.NoError(t, os.WriteFile(filepath.Join(w.bin, tool), []byte(setupE2EService), 0o755))
	}

	tarball := "seamless_" + setupE2EVersion + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{
		{"seamlessd", setupE2ESeamlessd},
		{"seam", "#!/bin/sh\nexit 0\n"},
	} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(f.body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	require.NoError(t, os.WriteFile(filepath.Join(w.release, tarball), buf.Bytes(), 0o644))
	sum := sha256.Sum256(buf.Bytes())
	checksums := hex.EncodeToString(sum[:]) + "  " + tarball + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(w.release, "checksums.txt"), []byte(checksums), 0o644))
	manifest := sha256.Sum256([]byte(checksums))
	w.checksumsSHA = hex.EncodeToString(manifest[:])
	return w
}

// config writes the installer's config, as an earlier install left it.
func (w *setupInstallerWorld) config(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(w.home, ".config", "seamless", "seamless.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

// run runs the installer as `curl | sh` runs it, in a session of its own,
// with knobs (KEY=VALUE) and the stand-ins' steering (SETUP_E2E_*) added to
// an environment built from scratch. It returns the combined output and the
// run's error.
func (w *setupInstallerWorld) run(t *testing.T, knobs ...string) (string, error) {
	t.Helper()
	helper, err := filepath.Abs(os.Args[0])
	require.NoError(t, err)
	cmd := exec.Command(w.shell, "-s")
	cmd.Stdin = bytes.NewReader(w.script)
	cmd.Dir = w.tmp
	cmd.Env = append([]string{
		"PATH=" + w.bin + ":/usr/bin:/bin", "HOME=" + w.home, "TMPDIR=" + w.tmp, "LANG=C",
		"SETUP_E2E_DIR=" + w.dir, "SETUP_E2E_RELEASE=" + w.release, "SETUP_E2E_VERSION=" + setupE2EVersion,
		"SETUP_E2E_HELPER=" + helper,
	}, knobs...)
	if os.Geteuid() == 0 {
		// The installer refuses root without it; a CI container may run as root.
		cmd.Env = append(cmd.Env, "SEAMLESS_ALLOW_ROOT=1")
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// calls is every seamlessd call the run made, probes (a lone -h) left out.
func (w *setupInstallerWorld) calls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(w.dir, "calls"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if line != "" && !strings.HasSuffix(line, " [-h]") {
			out = append(out, line)
		}
	}
	return out
}

// setupCalled reports whether any of calls ran setup (its probe aside).
func setupCalled(calls []string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, "seamlessd [setup]") {
			return true
		}
	}
	return false
}

// hooksCall is the install-hooks call the installer makes for client (the
// --client value) and extra (the client pair, when it passes one).
func (w *setupInstallerWorld) hooksCall(client string, extra ...string) string {
	line := "seamlessd [install-hooks] [--client] [" + client + "]"
	for _, a := range extra {
		line += " [" + a + "]"
	}
	return line + " [--seam] [" + filepath.Join(w.installDir, "seam") + "]"
}

// setupDetectedClients is what the installer's detection wires in the world:
// Claude Code (the world's ~/.claude), and on a Mac with the Claude app the
// chat surface too.
func setupDetectedClients() string {
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/Applications/Claude.app"); err == nil {
			return "claude,claude-desktop"
		}
	}
	return "claude"
}

// TestSetupInstaller_UnattendedRunsAreUnchanged: every run without a setup
// knob and without a person at a terminal -- a script's, an update's -- never
// starts seamlessd setup and makes exactly the calls it always made.
func TestSetupInstaller_UnattendedRunsAreUnchanged(t *testing.T) {
	setupEachShell(t, func(t *testing.T, shell string) {
		installer := filepath.Join("..", "..", "docs", "install")

		t.Run("nothing set, first install", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			out, err := w.run(t)
			require.NoError(t, err, out)
			calls := w.calls(t)
			require.False(t, setupCalled(calls), "%v", calls)
			require.Equal(t, []string{w.hooksCall(setupDetectedClients())}, calls, out)
			require.FileExists(t, filepath.Join(w.installDir, "seamlessd"))
		})

		t.Run("an update's run", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			w.config(t, "mcp:\n  api_key: \"k\"\n")
			out, err := w.run(t, "SEAMLESS_VERSION="+setupE2EVersion, "SEAMLESS_INSTALL_DIR="+w.installDir,
				"SEAMLESS_CHECKSUMS_SHA256="+w.checksumsSHA, "SEAMLESS_CLIENT=claude")
			require.NoError(t, err, out)
			calls := w.calls(t)
			require.False(t, setupCalled(calls), "%v", calls)
			require.Equal(t, []string{w.hooksCall("claude")}, calls, out)
			require.Contains(t, out, "verified by seamlessd")
		})

		t.Run("a client by SEAMLESS_SERVER_URL", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			out, err := w.run(t, "SEAMLESS_VERSION="+setupE2EVersion, "SEAMLESS_CLIENT=claude",
				"SEAMLESS_SERVER_URL=https://box.example:8081/", "SEAMLESS_MCP_API_KEY=k1")
			require.NoError(t, err, out)
			calls := w.calls(t)
			require.False(t, setupCalled(calls), "%v", calls)
			require.Equal(t, []string{w.hooksCall("claude", "--server-url", "https://box.example:8081", "--api-key", "k1")}, calls, out)
			require.Contains(t, out, "skipped (client of https://box.example:8081)")
		})

		t.Run("a client by its config", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			w.config(t, "role: client\nserver_url: \"https://box.example:8081\"\nmcp:\n  api_key: \"k\"\n")
			out, err := w.run(t, "SEAMLESS_VERSION="+setupE2EVersion, "SEAMLESS_CLIENT=claude")
			require.NoError(t, err, out)
			calls := w.calls(t)
			require.False(t, setupCalled(calls), "%v", calls)
			require.Equal(t, []string{w.hooksCall("claude")}, calls, out)
			require.Contains(t, out, "skipped (client of https://box.example:8081)")
		})

		t.Run("a release from before setup, nothing set", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			out, err := w.run(t, "SETUP_E2E_SETUP=old", "SEAMLESS_VERSION="+setupE2EVersion, "SEAMLESS_CLIENT=claude",
				"SEAMLESS_NO_SERVICE=1")
			require.NoError(t, err, out)
			calls := w.calls(t)
			require.False(t, setupCalled(calls), "%v", calls)
			require.Equal(t, []string{w.hooksCall("claude")}, calls, out)
			require.Contains(t, out, "skipped (SEAMLESS_NO_SERVICE)")
		})
	})
}

// setupStateServer and setupStateClient are answers a stand-in setup writes:
// the daemon runs here, or this computer is a client of box.example.
const (
	setupStateServer = "seamless-setup 1\nrole=server\nkind=standalone\ninstalled=none\nlocked=false\nanswer=env\n"
	setupStateClient = "seamless-setup 1\nrole=client\nserver_url=https://box.example:8081\nkind=client\ninstalled=none\nlocked=false\nanswer=asked\n"
)

// TestSetupInstaller_SetupKnobsRunSetup: a setup knob runs seamlessd setup,
// with no terminal for its stdin, before anything is installed, and the
// installer acts on its answer; --finish follows the installer's own steps.
func TestSetupInstaller_SetupKnobsRunSetup(t *testing.T) {
	setupEachShell(t, func(t *testing.T, shell string) {
		installer := filepath.Join("..", "..", "docs", "install")
		scripted := []string{"SEAMLESS_VERSION=" + setupE2EVersion, "SEAMLESS_CLIENT=claude", "SEAMLESS_NO_SERVICE=1"}

		t.Run("the daemon here", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			out, err := w.run(t, append(scripted, "SEAMLESS_SETUP=standalone", "SETUP_E2E_STATE="+setupStateServer)...)
			require.NoError(t, err, out)
			require.Equal(t, []string{
				"seamlessd [setup] [--state] [<state>]",
				w.hooksCall("claude"),
				"seamlessd [setup] [--state] [<state>] [--finish]",
			}, setupCallsWithin(t, w), out)
			require.Contains(t, out, "skipped (SEAMLESS_NO_SERVICE)")
			stdin, err := os.ReadFile(filepath.Join(w.dir, "setup-stdin"))
			require.NoError(t, err)
			require.NotContains(t, string(stdin), "tty", "a knob-driven setup never gets a terminal to read")
		})

		t.Run("a client setup answered", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			out, err := w.run(t, append(scripted, "SEAMLESS_SETUP=client", "SETUP_E2E_STATE="+setupStateClient)...)
			require.NoError(t, err, out)
			calls := w.calls(t)
			require.Contains(t, calls, w.hooksCall("claude"), "the config setup wrote is read back, so no key rides in argv")
			require.Contains(t, out, "skipped (client of https://box.example:8081)")
		})

		t.Run("a release from before setup cannot take a setup knob", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			out, err := w.run(t, append(scripted, "SEAMLESS_SETUP=server", "SETUP_E2E_SETUP=old")...)
			require.Error(t, err, out)
			require.Contains(t, out, "predates seamlessd setup")
			require.NoFileExists(t, filepath.Join(w.installDir, "seamlessd"), "nothing is installed")
			require.Empty(t, setupCallsWithin(t, w))
		})

		t.Run("a setup that stops stops the install before anything is installed", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			out, err := w.run(t, append(scripted, "SEAMLESS_SETUP=standalone", "SETUP_E2E_SETUP_EXIT=1")...)
			require.Error(t, err, out)
			require.Contains(t, out, "seamlessd setup stopped the install; nothing was installed")
			require.NoFileExists(t, filepath.Join(w.installDir, "seamlessd"))
			require.Equal(t, []string{"seamlessd [setup] [--state] [<state>]"}, setupCallsWithin(t, w))
		})

		t.Run("setup may not move what SEAMLESS_SERVER_URL answered", func(t *testing.T) {
			w := newSetupInstallerWorld(t, shell, installer)
			out, err := w.run(t, "SEAMLESS_VERSION="+setupE2EVersion, "SEAMLESS_CLIENT=claude", "SEAMLESS_SETUP=client",
				"SEAMLESS_SERVER_URL=https://box.example:8081", "SEAMLESS_MCP_API_KEY=k1", "SETUP_E2E_STATE="+setupStateServer)
			require.Error(t, err, out)
			require.Contains(t, out, "please report this")
			require.NoFileExists(t, filepath.Join(w.installDir, "seamlessd"))
		})
	})
}

// setupStatePath matches the state path in a recorded call: the installer's
// $TMP is a mktemp -d, so its name is the one part of a call no test knows
// (macOS's mktemp does not even put it under TMPDIR).
var setupStatePath = regexp.MustCompile(`\[[^]]*/setup\.state\]`)

// setupCallsWithin is the run's calls with the state path written <state>.
func setupCallsWithin(t *testing.T, w *setupInstallerWorld) []string {
	t.Helper()
	var out []string
	for _, c := range w.calls(t) {
		out = append(out, setupStatePath.ReplaceAllString(c, "[<state>]"))
	}
	return out
}

// TestSetupInstaller_DispatchRule runs docs/install's own setup block in sh,
// with tty_available stubbed to say whether a person is at a terminal and
// call_setup stubbed to record what setup would read: setup runs only for a
// setup knob, or for a person at a terminal with no SEAMLESS_* set, and only
// that person's setup reads the terminal.
func TestSetupInstaller_DispatchRule(t *testing.T) {
	setupE2ESupported(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "install"))
	require.NoError(t, err)
	block := regexp.MustCompile(`(?ms)^# BEGIN setup\n.*?^# END setup\n`).Find(raw)
	require.NotEmpty(t, block, "docs/install carries its setup block between # BEGIN setup and # END setup")
	consts := regexp.MustCompile(`(?m)^SETUP_PROTOCOL=.*\nSETUP_HEADER=.*\nSETUP_STATE=\n`).Find(raw)
	require.NotEmpty(t, consts, "docs/install defines SETUP_PROTOCOL, SETUP_HEADER and SETUP_STATE")
	for _, tt := range []struct {
		name  string
		tty   bool
		env   []string
		stdin string // "" when setup must not run at all
	}{
		{name: "no terminal, nothing set", tty: false},
		{name: "a terminal, nothing set", tty: true, stdin: "/dev/tty"},
		{name: "a terminal, SEAMLESS_CONFIG alone", tty: true, env: []string{"SEAMLESS_CONFIG=/h/c.yaml"}, stdin: "/dev/tty"},
		{name: "a terminal and an update's knobs", tty: true, env: []string{"SEAMLESS_VERSION=0.9.0", "SEAMLESS_INSTALL_DIR=/h/bin"}},
		{name: "a terminal and the client knobs", tty: true, env: []string{"SEAMLESS_SERVER_URL=https://b", "SEAMLESS_MCP_API_KEY=k"}},
		{name: "a terminal and an empty knob", tty: true, env: []string{"SEAMLESS_NO_SERVICE="}},
		{name: "no terminal, SEAMLESS_SETUP", tty: false, env: []string{"SEAMLESS_SETUP=server"}, stdin: "/dev/null"},
		{name: "a terminal, SEAMLESS_SETUP", tty: true, env: []string{"SEAMLESS_SETUP=server"}, stdin: "/dev/null"},
		{name: "no terminal, SEAMLESS_IMPORT", tty: false, env: []string{"SEAMLESS_IMPORT=/b.tar.gz"}, stdin: "/dev/null"},
		{name: "no terminal, SEAMLESS_PAIR_CODE", tty: false, env: []string{"SEAMLESS_PAIR_CODE=1"}, stdin: "/dev/null"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "log")
			tty := "1"
			if tt.tty {
				tty = "0"
			}
			script := "set -eu\n" + string(consts) + string(block) + `
tty_available() { return ` + tty + `; }
die() { printf 'DIE %s\n' "$*" >>"$LOG"; exit 1; }
call_setup() {
	printf 'setup reads %s\n' "$(setup_stdin)" >>"$LOG"
	printf 'seamless-setup 1\nrole=server\n' >"$SETUP_STATE"
}
TMP=$DIR
VERSION=9.9.9
CONFIG=$DIR/seamless.yaml
SERVER_URL=
printf '#!/bin/sh\nprintf "installer protocol: 1\\n"\n' >"$TMP/seamlessd"
chmod 0755 "$TMP/seamlessd"
run_setup
printf 'state %s\n' "${SETUP_STATE:-none}" >>"$LOG"
`
			for _, shell := range setupInstallerShells() {
				require.NoError(t, os.RemoveAll(log))
				cmd := exec.Command(shell, "-c", script)
				cmd.Env = append([]string{"PATH=/usr/bin:/bin", "LOG=" + log, "DIR=" + dir}, tt.env...)
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s: %s", shell, out)
				got, err := os.ReadFile(log)
				require.NoError(t, err)
				if tt.stdin == "" {
					require.Equal(t, "state none\n", string(got), "%s: setup must not run", shell)
					continue
				}
				require.Equal(t, "setup reads "+tt.stdin+"\nstate "+filepath.Join(dir, "setup.state")+"\n", string(got), shell)
			}
		})
	}
}

// TestSetupInstaller_TTYProbeAnswersNoWithoutATerminal runs docs/install's
// tty_available in every shell, in a session with no controlling terminal:
// it must answer no and let the installer go on. A failed redirection on the
// special built-in : ends a POSIX shell, so dash -- Debian's and Ubuntu's
// /bin/sh -- left the installer with status 2 and no message when the probe
// was a brace group; the installer now asks it on every run.
func TestSetupInstaller_TTYProbeAnswersNoWithoutATerminal(t *testing.T) {
	setupE2ESupported(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "install"))
	require.NoError(t, err)
	fn := regexp.MustCompile(`(?m)^tty_available\(\) \{.*\}\n`).Find(raw)
	require.NotEmpty(t, fn, "docs/install defines tty_available on one line")
	for _, shell := range setupInstallerShells() {
		cmd := exec.Command(shell, "-c", "set -eu\n"+string(fn)+"if tty_available; then echo yes; else echo no; fi\necho survived\n")
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s: %s", shell, out)
		require.Equal(t, "no\nsurvived\n", string(out), shell)
	}
}
