package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestServiceControl(t *testing.T) {
	const (
		home  = "/home/tester"
		uid   = 501
		label = "com.arctop.seamless"
	)
	plist := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	domain := "gui/501"
	target := "gui/501/" + label

	tests := []struct {
		name      string
		goos      string
		action    serviceAction
		wantLabel string
		wantCmds  [][]string
		wantFall  [][]string
	}{
		// darwin -- launchctl on the KeepAlive LaunchAgent
		{"darwin start", "darwin", actionStart, "launchd (" + label + ")",
			[][]string{{"launchctl", "bootstrap", domain, plist}}, nil},
		{"darwin stop", "darwin", actionStop, "launchd (" + label + ")",
			[][]string{{"launchctl", "bootout", target}}, nil},
		{"darwin restart", "darwin", actionRestart, "launchd (" + label + ")",
			[][]string{{"launchctl", "kickstart", "-k", target}},
			[][]string{{"launchctl", "bootstrap", domain, plist}}},
		{"darwin status", "darwin", actionStatus, "launchd (" + label + ")",
			[][]string{{"launchctl", "print", target}}, nil},
		// linux -- systemd --user
		{"linux start", "linux", actionStart, "systemd --user (seamless.service)",
			[][]string{{"systemctl", "--user", "start", "seamless.service"}}, nil},
		{"linux stop", "linux", actionStop, "systemd --user (seamless.service)",
			[][]string{{"systemctl", "--user", "stop", "seamless.service"}}, nil},
		{"linux restart", "linux", actionRestart, "systemd --user (seamless.service)",
			[][]string{{"systemctl", "--user", "restart", "seamless.service"}}, nil},
		{"linux status", "linux", actionStatus, "systemd --user (seamless.service)",
			[][]string{{"systemctl", "--user", "status", "seamless.service"}}, nil},
		// windows -- Scheduled Task (restart is End then Run: no single verb)
		{"windows start", "windows", actionStart, "Scheduled Task (Seamless)",
			[][]string{{"schtasks", "/Run", "/TN", "Seamless"}}, nil},
		{"windows stop", "windows", actionStop, "Scheduled Task (Seamless)",
			[][]string{{"schtasks", "/End", "/TN", "Seamless"}}, nil},
		{"windows restart", "windows", actionRestart, "Scheduled Task (Seamless)",
			[][]string{{"schtasks", "/End", "/TN", "Seamless"}, {"schtasks", "/Run", "/TN", "Seamless"}}, nil},
		{"windows status", "windows", actionStatus, "Scheduled Task (Seamless)",
			[][]string{{"schtasks", "/Query", "/TN", "Seamless", "/V", "/FO", "LIST"}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := serviceControl(tt.action, tt.goos, home, uid)
			require.Equal(t, tt.wantLabel, p.Label)
			require.Equal(t, tt.wantCmds, argvs(p.Cmds))
			require.Equal(t, tt.wantFall, argvs(p.Fallback))
		})
	}
}

// TestServiceControlProbe checks the install-detection seam: darwin/linux carry a
// definition file to stat, Windows carries a query command instead.
func TestServiceControlProbe(t *testing.T) {
	const home = "/home/tester"

	dar := serviceControl(actionStatus, "darwin", home, 501)
	require.Equal(t, filepath.Join(home, "Library", "LaunchAgents", "com.arctop.seamless.plist"), dar.DefFile)
	require.Nil(t, dar.ProbeCmd)

	lin := serviceControl(actionStatus, "linux", home, 501)
	require.Equal(t, filepath.Join(home, ".config", "systemd", "user", "seamless.service"), lin.DefFile)
	require.Nil(t, lin.ProbeCmd)

	win := serviceControl(actionStatus, "windows", home, 501)
	require.Empty(t, win.DefFile)
	require.NotNil(t, win.ProbeCmd)
	require.Equal(t, []string{"schtasks", "/Query", "/TN", "Seamless"}, win.ProbeCmd.Args)
}

// A Mac still on the pre-rename label is controlled under that label, until
// the current label's plist appears; with neither, the plan names the
// current one (the plist an install writes today).
func TestServiceControl_LegacyLaunchdLabel(t *testing.T) {
	home := t.TempDir()
	agents := filepath.Join(home, "Library", "LaunchAgents")
	require.NoError(t, os.MkdirAll(agents, 0o755))

	none := serviceControl(actionRestart, "darwin", home, 501)
	require.Equal(t, "launchd (com.arctop.seamless)", none.Label)
	require.False(t, serviceInstalled(none))

	legacy := filepath.Join(agents, "org.thereisnospoon.seamless.plist")
	require.NoError(t, os.WriteFile(legacy, []byte("x"), 0o644))
	p := serviceControl(actionRestart, "darwin", home, 501)
	require.Equal(t, "launchd (org.thereisnospoon.seamless)", p.Label)
	require.Equal(t, legacy, p.DefFile)
	require.True(t, serviceInstalled(p))
	require.Equal(t, [][]string{{"launchctl", "kickstart", "-k", "gui/501/org.thereisnospoon.seamless"}}, argvs(p.Cmds))
	require.Equal(t, [][]string{{"launchctl", "bootstrap", "gui/501", legacy}}, argvs(p.Fallback))

	current := filepath.Join(agents, "com.arctop.seamless.plist")
	require.NoError(t, os.WriteFile(current, []byte("x"), 0o644))
	both := serviceControl(actionStop, "darwin", home, 501)
	require.Equal(t, current, both.DefFile, "the current label wins once its plist exists")
	require.Equal(t, [][]string{{"launchctl", "bootout", "gui/501/com.arctop.seamless"}}, argvs(both.Cmds))
}

func TestLaunchdJob(t *testing.T) {
	const home = "/h"
	cur := "/h/Library/LaunchAgents/com.arctop.seamless.plist"
	old := "/h/Library/LaunchAgents/org.thereisnospoon.seamless.plist"
	for _, tt := range []struct {
		name    string
		present []string
		want    string
	}{
		{"neither", nil, launchdLabel},
		{"current only", []string{cur}, launchdLabel},
		{"legacy only", []string{old}, legacyLaunchdLabel},
		{"both", []string{cur, old}, launchdLabel},
	} {
		t.Run(tt.name, func(t *testing.T) {
			exists := func(p string) bool { return slices.Contains(tt.present, filepath.ToSlash(p)) }
			require.Equal(t, tt.want, launchdJob(home, exists))
		})
	}
}

// docs/install's launchd_label greps `seamlessd status -h` for this exact line
// to tell a binary that knows the current label from a pre-rename one.
func TestServiceUsageNamesTheLaunchdLabel(t *testing.T) {
	for _, action := range []serviceAction{actionStart, actionStop, actionRestart, actionStatus} {
		var buf bytes.Buffer
		serviceUsage(&buf, action)
		require.Contains(t, buf.String(), "usage: seamlessd "+string(action)+"\n")
		require.Contains(t, buf.String(), "\nlaunchd label: com.arctop.seamless\n")
	}
	err := runServiceAction(actionStatus, []string{"-h"})
	require.ErrorIs(t, err, flag.ErrHelp, "-h returns before anything is looked up or run")
}

// TestServiceInstalled covers the file-stat path (darwin/linux) and the
// no-way-to-check fallthrough.
func TestServiceInstalled(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "seamless.service")
	require.NoError(t, os.WriteFile(present, []byte("x"), 0o644))

	require.True(t, serviceInstalled(serviceControlPlan{DefFile: present}))
	require.False(t, serviceInstalled(serviceControlPlan{DefFile: filepath.Join(dir, "absent.service")}))
	// Neither a DefFile nor a ProbeCmd -> nothing to check, so it does not block.
	require.True(t, serviceInstalled(serviceControlPlan{}))
}

func TestPastTense(t *testing.T) {
	require.Equal(t, "started", pastTense(actionStart))
	require.Equal(t, "stopped", pastTense(actionStop))
	require.Equal(t, "restarted", pastTense(actionRestart))
	require.Equal(t, "status", pastTense(actionStatus))
}

// schtasks /End returns before Task Scheduler terminates the task's process, so
// a Windows stop or restart waits after it; every other plan runs its commands
// back to back, as before.
func TestServiceControl_SettlesOnlyWindowsStopAndRestart(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		for _, action := range []serviceAction{actionStart, actionStop, actionRestart, actionStatus} {
			want := 0
			if goos == "windows" && (action == actionStop || action == actionRestart) {
				want = 1
			}
			require.Equal(t, want, serviceControl(action, goos, "/home/tester", 501).SettleAfter, "%s %s", goos, action)
		}
	}
}

// run is driven with fake commands and a fake settle, so nothing executes. A
// settling plan waits between its stop and start halves, and only after a stop
// that succeeded; a failed wait fails a stop, but not a restart, whose new serve
// waits for the data dir itself.
func TestServiceControlPlanRun(t *testing.T) {
	const (
		end     = "schtasks /End /TN Seamless"
		run     = "schtasks /Run /TN Seamless"
		restart = "systemctl --user restart seamless.service"
	)
	errStuck := errors.New("pid 42 still holds the data dir")
	tests := []struct {
		name      string
		goos      string
		action    serviceAction
		endFails  bool // /End exits non-zero
		noSettle  bool // daemonSettle returned nil: the config did not load
		settleErr error
		wantSteps []string // each batch handed to runCmds, and "settle"
		wantOK    bool
		wantOut   string
	}{
		{name: "restart waits between End and Run", goos: "windows", action: actionRestart,
			wantSteps: []string{end, "settle", run}, wantOK: true, wantOut: "run output"},
		{name: "restart still runs Run when the wait fails", goos: "windows", action: actionRestart, settleErr: errStuck,
			wantSteps: []string{end, "settle", run}, wantOK: true, wantOut: "run output"},
		{name: "restart does not wait after a failed End", goos: "windows", action: actionRestart, endFails: true,
			wantSteps: []string{end, run}, wantOK: true, wantOut: "run output"},
		{name: "restart without a settle runs End and Run together", goos: "windows", action: actionRestart, noSettle: true,
			wantSteps: []string{end + " + " + run}, wantOK: true, wantOut: "run output"},
		{name: "stop is done once the wait confirms it", goos: "windows", action: actionStop,
			wantSteps: []string{end, "settle"}, wantOK: true, wantOut: "end output"},
		{name: "stop fails while the old daemon outlives the wait", goos: "windows", action: actionStop, settleErr: errStuck,
			wantSteps: []string{end, "settle"}, wantOK: false, wantOut: ""},
		{name: "stop does not wait after a failed End", goos: "windows", action: actionStop, endFails: true,
			wantSteps: []string{end}, wantOK: false, wantOut: "end output"},
		{name: "a plan that does not settle never calls settle", goos: "linux", action: actionRestart,
			wantSteps: []string{restart}, wantOK: true, wantOut: "restart output"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var steps []string
			runCmds := func(cmds []*exec.Cmd) (bool, string) {
				var batch []string
				for _, c := range cmds {
					batch = append(batch, strings.Join(c.Args, " "))
				}
				steps = append(steps, strings.Join(batch, " + "))
				switch last := cmds[len(cmds)-1].Args; {
				case slices.Contains(last, "/End"):
					return !tt.endFails, "end output"
				case slices.Contains(last, "/Run"):
					return true, "run output"
				default:
					return true, "restart output"
				}
			}
			settle := func() error {
				steps = append(steps, "settle")
				return tt.settleErr
			}
			if tt.noSettle {
				settle = nil
			}
			ok, out := serviceControl(tt.action, tt.goos, "/home/tester", 501).run(runCmds, settle)
			require.Equal(t, tt.wantSteps, steps)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.wantOut, out)
		})
	}
}

// lockScript plays dataDirLockHolder for daemonExit: each poll takes the next
// answer, and the last one repeats.
type lockScript struct {
	answers []lockAnswer
	polls   int
}

type lockAnswer struct {
	held bool
	pid  int
}

func (s *lockScript) holder(string) (bool, int) {
	s.polls++
	a := s.answers[min(s.polls, len(s.answers))-1]
	return a.held, a.pid
}

// exit is a daemonExit over the script on a fake clock: every tick is ready at
// once, and each reading of the clock advances it one poll.
func (s *lockScript) exit() daemonExit {
	const poll = 100 * time.Millisecond
	ready := make(chan time.Time)
	close(ready)
	clock := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	return daemonExit{
		dataDir: "data", holder: s.holder,
		now: func() time.Time {
			clock = clock.Add(poll)
			return clock
		},
		ticker:   func(time.Duration) (<-chan time.Time, func()) { return ready, func() {} },
		exitWait: 15 * time.Second, poll: poll,
	}
}

func TestDaemonExit(t *testing.T) {
	const deadline = 1 + 150 // the first poll, then 15s of 100ms polls
	tests := []struct {
		name      string
		answers   []lockAnswer
		wantNote  string
		wantErr   []string
		wantPolls int
	}{
		{name: "nothing holds the lock", answers: []lockAnswer{{false, 0}},
			wantNote: "not running", wantPolls: 1},
		{name: "the old daemon exits within the wait", answers: []lockAnswer{{true, 42}, {true, 42}, {true, 42}, {false, 0}},
			wantNote: "pid 42 exited (300ms)", wantPolls: 4},
		{name: "a lock read without a PID is not waited on", answers: []lockAnswer{{true, 0}},
			wantNote: "cannot read its lock; not waiting for it to exit", wantPolls: 1},
		{name: "a holder that outlives the wait is not the task's own process", answers: []lockAnswer{{true, 42}},
			wantErr: []string{
				"pid 42 still holds data 15s after the task stopped, so it is not the task's own process",
				"a serve started by hand", "SEAMLESS_CONFIG or SEAMLESS_DATA_DIR", "stop it yourself",
			}, wantPolls: deadline},
		{name: "a holder that stops naming itself", answers: []lockAnswer{{true, 42}, {true, 0}},
			wantErr:   []string{"data is still in use 15s after the task stopped, by a process that recorded no PID"},
			wantPolls: deadline},
		{name: "a serve started since is not the old daemon", answers: []lockAnswer{{true, 42}, {true, 43}},
			wantErr: []string{"pid 42 let go of data, but pid 43 holds it now"}, wantPolls: deadline},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &lockScript{answers: tt.answers}
			note, err := s.exit().run()
			if tt.wantErr == nil {
				require.NoError(t, err)
				require.Equal(t, tt.wantNote, note)
			} else {
				require.Empty(t, note)
				for _, want := range tt.wantErr {
					require.ErrorContains(t, err, want)
				}
			}
			require.Equal(t, tt.wantPolls, s.polls)
		})
	}
}

// wallTicker is the production ticker, for the tests on a real lock.
func wallTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// The real probe on a real lock: a second handle in this process stands in for
// the old daemon (filelock_test.go), and lets go on the third poll. The wait
// names the PID the holder recorded.
func TestDaemonExit_WaitsOnTheRealLock(t *testing.T) {
	dataDir := t.TempDir()
	daemon, err := lockDataDir(context.Background(), dataDir, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, daemon.Close()) })

	polls := 0
	w := daemonExit{
		dataDir: dataDir,
		holder: func(dir string) (bool, int) {
			held, pid := dataDirLockHolder(dir)
			if polls++; polls == 3 {
				require.NoError(t, daemon.Close())
			}
			return held, pid
		},
		now: time.Now, ticker: wallTicker,
		exitWait: 10 * time.Second, poll: 5 * time.Millisecond,
	}
	note, err := w.run()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(note, "pid "+strconv.Itoa(os.Getpid())+" exited ("), note)
	require.Equal(t, 4, polls, "the first poll after the release finds the lock free")
}

// Nothing is terminated at the deadline: another process holds the lock past
// the wait, and run names it while it lives on, still holding the lock. The
// filelock tests' helper plays that process: it takes the lock, reports its
// PID, and holds the lock until its stdin closes.
func TestDaemonExit_TerminatesNothingAtTheDeadline(t *testing.T) {
	dataDir := t.TempDir()
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(exe, "-test.run=^TestFileLockHelperProcess$")
	cmd.Env = append(os.Environ(), fileLockHelperEnv+"="+filepath.Join(dataDir, dataDirLockName))
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	want := fmt.Sprintf("locked %d", cmd.Process.Pid)
	scanner := bufio.NewScanner(stdout)
	reported := false
	for !reported && scanner.Scan() {
		reported = scanner.Text() == want
	}
	require.True(t, reported, "the helper never reported holding the lock (scan error: %v)", scanner.Err())

	w := daemonExit{
		dataDir: dataDir, holder: dataDirLockHolder, now: time.Now, ticker: wallTicker,
		exitWait: 50 * time.Millisecond, poll: 5 * time.Millisecond,
	}
	_, err = w.run()
	require.ErrorContains(t, err, fmt.Sprintf("pid %d still holds", cmd.Process.Pid))
	require.ErrorContains(t, err, "not the task's own process")

	held, pid := dataDirLockHolder(dataDir)
	require.True(t, held, "the holder lives on: nothing terminated it")
	require.Equal(t, cmd.Process.Pid, pid)
}

// daemonSettle's wiring: only a plan that settles loads the config; one that
// does not load leaves the commands back to back, with a row saying so; a
// loaded one probes its own data dir. The config and the data dir are both
// pinned to the test's temp dir, never the live install.
func TestDaemonSettle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SEAMLESS_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("SEAMLESS_CONFIG", filepath.Join(dir, "absent.yaml"))

	var out bytes.Buffer
	require.Nil(t, daemonSettle(serviceControl(actionRestart, "darwin", dir, 501), &out))
	require.Nil(t, daemonSettle(serviceControl(actionRestart, "linux", dir, 501), &out))
	require.Nil(t, daemonSettle(serviceControl(actionStart, "windows", dir, 501), &out))
	require.Empty(t, out.String(), "a plan that does not settle never loads the config")

	require.Nil(t, daemonSettle(serviceControl(actionStop, "windows", dir, 501), &out),
		"a config that does not load leaves the commands as they were")
	require.Contains(t, out.String(), "not waiting for the old daemon to exit")
	require.Contains(t, out.String(), "absent.yaml")

	out.Reset()
	cfgPath := filepath.Join(dir, "seamless.yaml")
	require.NoError(t, os.WriteFile(cfgPath, nil, 0o600))
	t.Setenv("SEAMLESS_CONFIG", cfgPath)
	settle := daemonSettle(serviceControl(actionRestart, "windows", dir, 501), &out)
	require.NotNil(t, settle)
	require.NoError(t, settle())
	require.Contains(t, out.String(), "not running", "nothing holds the fresh data dir's lock")
}
