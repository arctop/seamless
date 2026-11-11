//go:build unix

package main

// The updater end to end over a real installer: the release's install script
// is a real POSIX sh script run by the real runner (runInstallerScript, /bin/sh
// -s, the script on stdin), and the daemon it (re)starts is a real separate
// process -- this test binary re-executed as a stand-in seamlessd
// (TestUpdaterHelperDaemon) that binds the config's address, records itself
// in the update state and answers /healthz with its version and instance. The
// confirmation therefore observes another process through cfg.HTTPClient,
// exactly as it does on a machine.
//
// Every child process runs in an environment built from scratch here: a temp
// HOME, a fixed PATH, the UPDATER_E2E_* variables that steer the stand-in, and
// the updater's own knobs. Nothing of the test process's environment -- no
// SEAMLESS_* of the developer's -- reaches them.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/update"
)

// e2eInstallerScript stands in for docs/install: it records its knobs, swaps
// the installed seamlessd by rename, stops the running stand-in daemon and
// starts a new one as SEAMLESS_VERSION -- unless that is the release the test
// marked broken, which swaps and then never comes up.
const e2eInstallerScript = `#!/bin/sh
set -eu
dir=$UPDATER_E2E_DIR
printf '%s\n' "$SEAMLESS_VERSION" >>"$dir/versions"
env >"$dir/env-$SEAMLESS_VERSION"
printf 'seamlessd %s\n' "$SEAMLESS_VERSION" >"$SEAMLESS_INSTALL_DIR/.seamlessd.new"
mv -f "$SEAMLESS_INSTALL_DIR/.seamlessd.new" "$SEAMLESS_INSTALL_DIR/seamlessd"
if [ -f "$dir/pid" ]; then
	kill "$(cat "$dir/pid")" 2>/dev/null || true
	rm -f "$dir/pid"
fi
if [ "$SEAMLESS_VERSION" = "${UPDATER_E2E_BROKEN:-}" ]; then
	exit 0
fi
UPDATER_HELPER_DAEMON=1 "$UPDATER_E2E_HELPER" -test.run='^TestUpdaterHelperDaemon$' >/dev/null 2>&1 &
echo $! >"$dir/pid"
`

// TestUpdaterHelperDaemon is not a test: the end-to-end test's installer
// script runs this binary with UPDATER_HELPER_DAEMON=1 as a stand-in
// seamlessd, and this is that daemon. It binds UPDATER_E2E_ADDR (waiting for
// the previous one to let go), records itself as running in the update state
// under UPDATER_E2E_DATA before it serves -- the order a release daemon keeps
// -- and answers /healthz as SEAMLESS_VERSION. With UPDATER_E2E_LOCK=1 it
// first takes the data dir's lock, as serve does (lockDataDir). It exits when
// killed, or after a minute regardless, so a failed test cannot leave it
// behind for long.
func TestUpdaterHelperDaemon(t *testing.T) {
	if os.Getenv("UPDATER_HELPER_DAEMON") != "1" {
		t.Skip("the stand-in daemon of TestUpdater_EndToEnd; not a test of its own")
	}
	v, ok := update.Parse(os.Getenv("SEAMLESS_VERSION"))
	if !ok {
		os.Exit(2)
	}
	if os.Getenv("UPDATER_E2E_LOCK") == "1" {
		lock, err := lockDataDir(t.Context(), os.Getenv("UPDATER_E2E_DATA"), nil)
		if err != nil {
			os.Exit(6)
		}
		defer func() { _ = lock.Close() }()
	}
	instance, err := core.NewID()
	if err != nil {
		os.Exit(3)
	}
	var ln net.Listener
	retry := time.NewTicker(20 * time.Millisecond)
	giveUp := time.After(10 * time.Second)
	for ln == nil {
		if ln, err = net.Listen("tcp", os.Getenv("UPDATER_E2E_ADDR")); err == nil {
			break
		}
		select {
		case <-retry.C:
		case <-giveUp:
			os.Exit(4)
		}
	}
	retry.Stop()
	if err := update.SaveState(os.Getenv("UPDATER_E2E_DATA"), update.State{Running: update.Running{
		Version: v, Distribution: update.DistributionRelease, Kind: update.KindInstaller,
		Instance: instance, PID: os.Getpid(), StartedAt: time.Now().UTC(),
	}}); err != nil {
		os.Exit(5)
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"status":"ok","version":%q,"instance":%q}`, v.String()+"+e2e", instance)
	})}
	go func() { _ = srv.Serve(ln) }()
	<-time.After(time.Minute)
	os.Exit(0)
}

// e2eWorld is a world whose installer is e2eInstallerScript run for real and
// whose daemon is the stand-in process.
func e2eWorld(t *testing.T, broken string) (*world, updaterDeps, string) {
	t.Helper()
	w := newWorld(t, "linux", "amd64", "0.7.2")
	helper, err := filepath.Abs(os.Args[0])
	require.NoError(t, err)
	dir := t.TempDir()
	addr := w.cfg.Addr
	// The in-process daemon steps aside: the stand-in takes its port.
	w.daemon.srv.Close()
	t.Cleanup(func() {
		if raw, err := os.ReadFile(filepath.Join(dir, "pid")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	w.environ = []string{
		"PATH=/usr/bin:/bin", "HOME=" + w.home, "TMPDIR=" + dir, "LANG=C",
		"UPDATER_E2E_DIR=" + dir, "UPDATER_E2E_HELPER=" + helper,
		"UPDATER_E2E_ADDR=" + addr, "UPDATER_E2E_DATA=" + w.dataDir, "UPDATER_E2E_BROKEN=" + broken,
		// What the daemon's own environment may carry, which must not reach
		// the installer or anything it starts.
		"SEAMLESS_CONFIG=" + w.cfgPath, "SEAMLESS_MCP_API_KEY=leaked-key", "SEAMLESS_DATA_DIR=/nowhere",
	}
	// The world publishes the same script for both releases; the comment
	// line keeps their bytes, and so their signatures, apart.
	w.publish("0.7.2", releaseOpts{script: e2eInstallerScript + "# v0.7.2\n"})
	w.publish("0.7.3", releaseOpts{script: e2eInstallerScript + "# v0.7.3\n"})

	d := w.deps(nil)
	d.environ = w.environ
	d.runInstaller = func(run installerRun) error {
		w.mu.Lock()
		w.installs = append(w.installs, run)
		w.mu.Unlock()
		return runInstallerScript(run)
	}
	d.service = func(a serviceAction) (bool, string) {
		w.mu.Lock()
		w.actions = append(w.actions, a)
		w.mu.Unlock()
		if a == actionStop {
			if raw, err := os.ReadFile(filepath.Join(dir, "pid")); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
					_ = syscall.Kill(pid, syscall.SIGTERM)
				}
			}
		}
		return true, ""
	}
	d.timing.confirm.deadline = 3 * time.Second
	d.timing.confirm.starting = 3 * time.Second
	return w, d, dir
}

func runE2E(t *testing.T, w *world, d updaterDeps) (update.Attempt, error) {
	t.Helper()
	f, err := parseUpdateFlags(updaterArgs(update.SpawnRequest{
		AttemptID: testAttemptID(t), From: mustVersion(t, "0.7.2"), To: mustVersion(t, "0.7.3"), Why: update.WhyAuto,
	}, w.cfgPath)[1:])
	require.NoError(t, err)
	runErr := runAutoUpdate(f, d)
	a, err := update.ReadAttempt(w.dataDir)
	require.NoError(t, err)
	return a, runErr
}

func TestUpdater_EndToEnd(t *testing.T) {
	w, d, dir := e2eWorld(t, "")
	a, err := runE2E(t, w, d)
	require.NoError(t, err)
	requireRecorded(t, w, a, update.StageDone, false)

	// The real installer ran with the real environment rule: inherited minus
	// SEAMLESS_*, plus the knobs. What the script itself saw:
	seen, rerr := os.ReadFile(filepath.Join(dir, "env-0.7.3"))
	require.NoError(t, rerr)
	env := string(seen)
	require.Contains(t, env, "SEAMLESS_VERSION=0.7.3\n")
	require.Contains(t, env, "SEAMLESS_CHECKSUMS_SHA256="+w.pin("0.7.3")+"\n")
	require.Contains(t, env, "SEAMLESS_INSTALL_DIR="+w.installDir+"\n")
	require.Contains(t, env, "SEAMLESS_NO_HOOKS=1\n")
	require.Contains(t, env, "HOME="+w.home+"\n")
	require.NotContains(t, env, "leaked-key")
	require.NotContains(t, env, "SEAMLESS_CONFIG=")
	require.NotContains(t, env, "SEAMLESS_DATA_DIR=")

	installed, rerr := os.ReadFile(w.exe)
	require.NoError(t, rerr)
	require.Equal(t, "seamlessd 0.7.3\n", string(installed))
	st, rerr := update.ReadState(w.dataDir)
	require.NoError(t, rerr)
	require.Equal(t, "0.7.3", st.Running.Version.String())

	logged, rerr := os.ReadFile(a.LogPath)
	require.NoError(t, rerr)
	require.Contains(t, string(logged), "installer exited: ok")
}

func TestUpdater_EndToEndRollback(t *testing.T) {
	w, d, dir := e2eWorld(t, "0.7.3")
	a, err := runE2E(t, w, d)
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageRollback, true)

	versions, rerr := os.ReadFile(filepath.Join(dir, "versions"))
	require.NoError(t, rerr)
	require.Equal(t, "0.7.3\n0.7.2\n", string(versions), "the release, then the rollback pinned to the old one")
	back, rerr := os.ReadFile(filepath.Join(dir, "env-0.7.2"))
	require.NoError(t, rerr)
	require.Contains(t, string(back), "SEAMLESS_CHECKSUMS_SHA256="+w.pin("0.7.2")+"\n")

	installed, rerr := os.ReadFile(w.exe)
	require.NoError(t, rerr)
	require.Equal(t, "seamlessd 0.7.2\n", string(installed))
	st, rerr := update.ReadState(w.dataDir)
	require.NoError(t, rerr)
	require.Equal(t, "0.7.2", st.Running.Version.String())
	require.Equal(t, []serviceAction{actionStop}, w.actions)
	require.False(t, errors.Is(err, errLockHeld))
}

// e2eSystemDaemon is a system install's seamlessd in the end-to-end test: a
// script that runs the stand-in daemon as release v, taking the data dir's
// lock -- or, broken, one that exits at once.
func e2eSystemDaemon(v string, broken bool) string {
	if broken {
		return "#!/bin/sh\nexit 3\n"
	}
	return "#!/bin/sh\nUPDATER_HELPER_DAEMON=1 UPDATER_E2E_LOCK=1 SEAMLESS_VERSION=" + v +
		" exec \"$UPDATER_E2E_HELPER\" -test.run='^TestUpdaterHelperDaemon$' >/dev/null 2>&1\n"
}

// e2eSystemWorld is a peer_auth system daemon run for real: its seamlessd is
// e2eSystemDaemon on disk, a supervisor loop plays launchd's KeepAlive (it
// runs whatever seamlessd is on disk, again whenever it exits), and the
// updater's end of the daemon is the real SIGTERM (endDaemon).
func e2eSystemWorld(t *testing.T, broken bool) (*world, updaterDeps) {
	t.Helper()
	w := newWorld(t, "linux", "amd64", "0.7.2")
	helper, err := filepath.Abs(os.Args[0])
	require.NoError(t, err)
	addr := w.cfg.Addr
	w.daemon.srv.Close()
	w.probeEdits = func(p *update.Probe) {
		p.PeerAuth = true
		p.System = update.SystemProbe{
			Account:    true,
			Service:    update.ServiceProbe{Path: "/etc/systemd/system/seamless-daemon.service", Found: true, Program: w.exe, RunsThisExe: true},
			Restarts:   true,
			Supervised: true,
		}
	}
	w.installer = func(*world, installerRun) error { return errors.New("a system install never runs the installer") }
	for _, v := range []string{"0.7.2", "0.7.3"} {
		w.publish(v, releaseOpts{archive: tarGz(t, map[string]string{
			"seamlessd": e2eSystemDaemon(v, broken && v == "0.7.3"), "seam": "seam " + v + "\n",
		})})
	}
	require.NoError(t, os.WriteFile(w.exe, []byte(e2eSystemDaemon("0.7.2", false)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(w.installDir, "seam"), []byte("seam 0.7.2\n"), 0o755))

	env := []string{
		"PATH=/usr/bin:/bin", "HOME=" + w.home, "LANG=C",
		"UPDATER_E2E_HELPER=" + helper, "UPDATER_E2E_ADDR=" + addr, "UPDATER_E2E_DATA=" + w.dataDir,
	}
	done := make(chan struct{})
	var (
		mu      sync.Mutex
		current *os.Process
	)
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			cmd := exec.Command(w.exe)
			cmd.Env = env
			if err := cmd.Start(); err == nil {
				mu.Lock()
				current = cmd.Process
				mu.Unlock()
				_ = cmd.Wait()
			}
			select {
			case <-done:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() {
		close(done)
		mu.Lock()
		defer mu.Unlock()
		if current != nil {
			_ = current.Kill()
		}
	})
	healthy := func(want string) func() bool {
		return func() bool {
			resp, err := http.Get("http://" + addr + "/healthz")
			if err != nil {
				return false
			}
			defer func() { _ = resp.Body.Close() }()
			var body struct{ Version string }
			return json.NewDecoder(resp.Body).Decode(&body) == nil && strings.HasPrefix(body.Version, want+"+")
		}
	}
	require.Eventually(t, healthy("0.7.2"), 10*time.Second, 20*time.Millisecond)

	d := w.deps(nil)
	d.environ = env
	d.stopDaemon = endDaemon
	d.timing.confirm.deadline = 3 * time.Second
	d.timing.confirm.starting = 3 * time.Second
	d.timing.daemonStop = 5 * time.Second
	return w, d
}

func TestUpdater_EndToEndSystem(t *testing.T) {
	w, d := e2eSystemWorld(t, false)
	a, err := runE2E(t, w, d)
	require.NoError(t, err)
	requireRecorded(t, w, a, update.StageDone, false)
	require.Empty(t, w.installs, "no installer runs")

	installed, err := os.ReadFile(w.exe)
	require.NoError(t, err)
	require.Equal(t, e2eSystemDaemon("0.7.3", false), string(installed))
	seam, err := os.ReadFile(filepath.Join(w.installDir, "seam"))
	require.NoError(t, err)
	require.Equal(t, "seam 0.7.3\n", string(seam))
	st, err := update.ReadState(w.dataDir)
	require.NoError(t, err)
	require.Equal(t, "0.7.3", st.Running.Version.String())
	_, err = os.Stat(rollbackName(w.installDir, "seamlessd"))
	require.ErrorIs(t, err, os.ErrNotExist, "the kept binaries go once the release confirms")
}

func TestUpdater_EndToEndSystemRollback(t *testing.T) {
	w, d := e2eSystemWorld(t, true)
	a, err := runE2E(t, w, d)
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageRollback, true)
	require.NotContains(t, a.Error, "did not let go of the data dir")

	installed, rerr := os.ReadFile(w.exe)
	require.NoError(t, rerr)
	require.Equal(t, e2eSystemDaemon("0.7.2", false), string(installed))
	st, rerr := update.ReadState(w.dataDir)
	require.NoError(t, rerr)
	require.Equal(t, "0.7.2", st.Running.Version.String())
}
