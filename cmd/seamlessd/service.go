package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
)

// serviceAction is one lifecycle verb for the installed per-user service.
type serviceAction string

const (
	actionStart   serviceAction = "start"
	actionStop    serviceAction = "stop"
	actionRestart serviceAction = "restart"
	actionStatus  serviceAction = "status"
)

// serviceControlPlan is the OS-specific set of steps that start, stop, restart,
// or report the per-user service. Like serviceTeardownPlan it is a pure value, so
// the argv can be asserted in tests without executing anything.
//
// These verbs control an ALREADY-installed service; they never author one (that
// stays with `make install` and the installer scripts). Installation is detected
// by DefFile (a stat) on darwin/linux, or ProbeCmd (a query) on Windows, where
// the registration lives in the Task Scheduler rather than in a file.
type serviceControlPlan struct {
	Label       string      // human label for the service kind
	DefFile     string      // service-definition file to stat for the install check ("" -> use ProbeCmd)
	ProbeCmd    *exec.Cmd   // Windows: `schtasks /Query`; nil where DefFile suffices
	Cmds        []*exec.Cmd // the action's commands, in order; the last one's exit is the verdict
	Fallback    []*exec.Cmd // tried when Cmds fail (darwin restart -> bootstrap); nil elsewhere
	InstallHint string      // shown when not installed: how to install on this OS

	// SettleAfter, when non-zero, is how many leading Cmds stop a daemon whose
	// process outlives them: run waits for that process to exit between them
	// and the rest (daemonSettle). Cmds alone, back to back, are still the
	// whole action for a caller that does not settle.
	SettleAfter int
}

// serviceControl builds the steps for action on goos. It mirrors serviceTeardown
// (same identifiers, same gui/<uid>/<label> target and unit path); the two are
// the only Go code that manages the service. The launchd/systemd/schtasks handles
// are the shared consts from uninstall.go -- no new definition site. On darwin
// the label is the installed job's (launchdJob, a stat under home), so a Mac
// still on the legacy label is controlled under that one.
//
// darwin uses launchctl because the plist declares KeepAlive: bootout truly stops
// the job (a plain kill would be resurrected), kickstart -k restarts a loaded job
// in place, and bootstrap (re)loads it. systemd and schtasks map to their verbs
// directly. Windows restart has no single verb, so it is /End then /Run. /End
// returns about a second before Task Scheduler terminates the task's process,
// which holds the port, the data dir and its executable until then, so Windows
// stop and restart settle after it (SettleAfter).
func serviceControl(action serviceAction, goos, home string, uid int) serviceControlPlan {
	switch goos {
	case "darwin":
		label := launchdJob(home, fileExists)
		plist := launchdPlist(home, label)
		domain := fmt.Sprintf("gui/%d", uid)
		target := fmt.Sprintf("gui/%d/%s", uid, label)
		p := serviceControlPlan{
			Label:       "launchd (" + label + ")",
			DefFile:     plist,
			InstallHint: "run 'make install' in the repo, or: curl -fsSL https://thereisnospoon.org/install | sh",
		}
		switch action {
		case actionStart:
			p.Cmds = []*exec.Cmd{exec.Command("launchctl", "bootstrap", domain, plist)}
		case actionStop:
			p.Cmds = []*exec.Cmd{exec.Command("launchctl", "bootout", target)}
		case actionRestart:
			p.Cmds = []*exec.Cmd{exec.Command("launchctl", "kickstart", "-k", target)}
			p.Fallback = []*exec.Cmd{exec.Command("launchctl", "bootstrap", domain, plist)}
		case actionStatus:
			p.Cmds = []*exec.Cmd{exec.Command("launchctl", "print", target)}
		}
		return p
	case "windows":
		p := serviceControlPlan{
			Label:       "Scheduled Task (" + scheduledTask + ")",
			ProbeCmd:    exec.Command("schtasks", "/Query", "/TN", scheduledTask),
			InstallHint: "run the installer: irm https://thereisnospoon.org/install.ps1 | iex",
		}
		switch action {
		case actionStart:
			p.Cmds = []*exec.Cmd{exec.Command("schtasks", "/Run", "/TN", scheduledTask)}
		case actionStop:
			p.Cmds = []*exec.Cmd{exec.Command("schtasks", "/End", "/TN", scheduledTask)}
			p.SettleAfter = 1
		case actionRestart:
			p.Cmds = []*exec.Cmd{
				exec.Command("schtasks", "/End", "/TN", scheduledTask),
				exec.Command("schtasks", "/Run", "/TN", scheduledTask),
			}
			p.SettleAfter = 1
		case actionStatus:
			p.Cmds = []*exec.Cmd{exec.Command("schtasks", "/Query", "/TN", scheduledTask, "/V", "/FO", "LIST")}
		}
		return p
	default: // linux and other unixes run the systemd --user unit
		unit := filepath.Join(home, ".config", "systemd", "user", systemdUnit)
		p := serviceControlPlan{
			Label:       "systemd --user (" + systemdUnit + ")",
			DefFile:     unit,
			InstallHint: "run the installer: curl -fsSL https://thereisnospoon.org/install | sh",
		}
		switch action {
		case actionStart:
			p.Cmds = []*exec.Cmd{exec.Command("systemctl", "--user", "start", systemdUnit)}
		case actionStop:
			p.Cmds = []*exec.Cmd{exec.Command("systemctl", "--user", "stop", systemdUnit)}
		case actionRestart:
			p.Cmds = []*exec.Cmd{exec.Command("systemctl", "--user", "restart", systemdUnit)}
		case actionStatus:
			p.Cmds = []*exec.Cmd{exec.Command("systemctl", "--user", "status", systemdUnit)}
		}
		return p
	}
}

// serviceInstalled reports whether the service is registered. On darwin/linux
// that is a stat of the definition file; on Windows there is no file, so a
// best-effort ProbeCmd (`schtasks /Query`) stands in -- a non-zero exit (task
// absent) reads as not installed. With no way to check, it does not block.
func serviceInstalled(p serviceControlPlan) bool {
	if p.DefFile != "" {
		_, err := os.Stat(p.DefFile)
		return err == nil
	}
	if p.ProbeCmd != nil {
		return p.ProbeCmd.Run() == nil
	}
	return true
}

// runServiceAction runs one lifecycle verb against the installed service. It is
// the entrypoint for `seamlessd start|stop|restart|status`. When the service is
// not installed it returns a clear install hint instead of a cryptic tool error.
func runServiceAction(action serviceAction, args []string) error {
	fs := flag.NewFlagSet(string(action), flag.ContinueOnError)
	fs.Usage = func() { serviceUsage(fs.Output(), action) }
	if err := fs.Parse(args); err != nil {
		return err
	}
	plan := serviceControl(action, runtime.GOOS, homeDir(), os.Getuid())

	if !serviceInstalled(plan) {
		where := plan.Label
		if plan.DefFile != "" {
			where = plan.Label + " [" + tildePath(plan.DefFile) + "]"
		}
		return fmt.Errorf("service not installed: %s -- %s", where, plan.InstallHint)
	}

	if action == actionStatus {
		return reportServiceStatus(plan)
	}

	fmt.Printf("\n%s %s\n", bold("Seamless"), dim(string(action)))
	fieldRow("kind", plan.Label)

	ok, out := plan.run(runControlCmds, daemonSettle(plan, os.Stdout))
	if !ok && len(plan.Fallback) > 0 {
		ok, out = runControlCmds(plan.Fallback)
	}
	if ok {
		fieldRow(string(action), green(pastTense(action)))
	} else {
		fieldRow(string(action), yellow("could not "+string(action)))
		if out != "" {
			contDim(out)
		}
	}
	return nil
}

// launchdLabelLine is the line of the service verbs' usage that names the
// LaunchAgent label this binary installs under. docs/install's launchd_label
// asks a binary it unpacked for it (`seamlessd status -h`) and installs the
// legacy label for one whose help lacks it -- a pinned pre-rename release,
// which only knows that one -- so keep this exact wording, or change both.
const launchdLabelLine = "launchd label: " + launchdLabel

// serviceUsage is the -h text of `seamlessd <action>`.
func serviceUsage(w io.Writer, action serviceAction) {
	verb := map[serviceAction]string{
		actionStart: "Starts", actionStop: "Stops", actionRestart: "Restarts", actionStatus: "Reports on",
	}[action]
	fmt.Fprintf(w, "usage: seamlessd %s\n\n%s the per-user service the installer registered:\n"+
		"  macOS    the launchd job %s, or %s\n"+
		"           on a Mac installed before the label rename\n"+
		"  Linux    the systemd --user unit %s\n"+
		"  Windows  the Scheduled Task %s\n\n%s\n",
		action, verb, launchdLabel, legacyLaunchdLabel, systemdUnit, scheduledTask, launchdLabelLine)
}

// runControlCmds runs each command, best-effort, and returns whether the LAST
// command succeeded plus its trimmed output. The last command establishes the
// desired end state (e.g. schtasks /Run after /End on restart); earlier commands
// are setup whose failure is ignored.
func runControlCmds(cmds []*exec.Cmd) (bool, string) {
	if len(cmds) == 0 {
		return true, ""
	}
	var ok bool
	var out string
	for i, c := range cmds {
		o, err := c.CombinedOutput()
		if i == len(cmds)-1 {
			ok = err == nil
			out = strings.TrimSpace(string(o))
		}
	}
	return ok, out
}

// run carries out the plan's Cmds through runCmds (runControlCmds) and returns
// the verdict. A plan that settles runs its stop half, Cmds[:SettleAfter],
// first; when that succeeded, settle waits for the old daemon to exit (a
// failed stop has nothing on its way out to wait for); then the rest of Cmds
// runs. settle reports its own outcome, and its error moves only a stop's
// verdict: a stop is done once the old daemon is gone, while a restart's
// verdict stays its start command's, because the serve that command starts
// still waits for the data dir itself (lockDataDir). A nil settle, or a plan
// that does not settle, runs Cmds back to back.
func (p serviceControlPlan) run(runCmds func([]*exec.Cmd) (bool, string), settle func() error) (bool, string) {
	n := p.SettleAfter
	if settle == nil || n <= 0 || n > len(p.Cmds) {
		return runCmds(p.Cmds)
	}
	ok, out := runCmds(p.Cmds[:n])
	if ok {
		if err := settle(); err != nil && n == len(p.Cmds) {
			return false, ""
		}
	}
	if n == len(p.Cmds) {
		return ok, out
	}
	return runCmds(p.Cmds[n:])
}

// daemonExitWait bounds how long a Windows stop or restart waits after
// schtasks /End for the old serve process to exit -- the bound install.ps1's
// Stop-Daemon gives Wait-Process. Task Scheduler terminates the task's process
// 1.06-1.18s after /End returns (lab detached-updater-os-spikes), so a holder
// still there at the deadline is not the task's own process -- a serve started
// by hand, or a daemon on another data dir that this shell's config names --
// and it is reported, never terminated: it is not the service's to stop.
const daemonExitWait = 15 * time.Second

// daemonSettle builds the settle step plan.run waits with: a daemonExit on the
// data dir of the config this command loads -- the install every seamlessd
// command acts on -- writing its outcome to w as a row. A plan that does not
// settle gets nil without loading anything. So does a config that does not
// load, after a row saying so: the commands then run back to back as before,
// and a restarted serve still waits for the old one itself (lockDataDir).
func daemonSettle(p serviceControlPlan, w io.Writer) func() error {
	if p.SettleAfter == 0 {
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		fieldRowTo(w, "daemon", yellow("not waiting for the old daemon to exit: "+err.Error()))
		return nil
	}
	exit := daemonExit{
		dataDir: cfg.DataDir, holder: dataDirLockHolder, now: time.Now,
		ticker: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
		exitWait: daemonExitWait, poll: dataDirLockPoll,
	}
	return func() error {
		note, err := exit.run()
		if err != nil {
			fieldRowTo(w, "daemon", yellow(err.Error()))
			return err
		}
		fieldRowTo(w, "daemon", dim(note))
		return nil
	}
}

// daemonExit waits for the serve process holding a data dir's one-daemon lock
// (seamlessd.lock) to exit. The lock is the signal, not a PID or a process
// list: the OS releases it only once the holder is gone, however it went; only
// serve takes it, so it never names another seamlessd command (the updater
// included); and a recycled PID cannot fake it. It waits and reports, and
// never terminates anything. The seams are injected so a test drives the lock
// and the clock.
type daemonExit struct {
	dataDir string
	holder  func(dataDir string) (held bool, pid int) // dataDirLockHolder
	now     func() time.Time
	ticker  func(time.Duration) (<-chan time.Time, func())
	// exitWait bounds the wait for the holder to exit; poll paces it.
	exitWait, poll time.Duration
}

// run waits for the lock to come free and returns a note for the output, or an
// error naming the holder when the lock is still held at the deadline. A free
// lock needs no wait, and neither does one held without a readable PID right
// after the stop: the old daemon recorded its PID when it started, so that is
// a lock this probe cannot read (a file system that refuses locking, where
// serve runs unguarded), not a daemon on its way out.
func (w daemonExit) run() (string, error) {
	held, first := w.holder(w.dataDir)
	switch {
	case !held:
		return "not running", nil
	case first == 0:
		return "cannot read its lock; not waiting for it to exit", nil
	}
	tick, stop := w.ticker(w.poll)
	defer stop()
	start := w.now()
	free, pid := w.waitFree(tick, start.Add(w.exitWait))
	if free {
		return fmt.Sprintf("pid %d exited (%s)", first, w.now().Sub(start).Round(100*time.Millisecond)), nil
	}
	dir := tildePath(w.dataDir)
	switch pid {
	case 0:
		return "", fmt.Errorf("%s is still in use %s after the task stopped, by a process that recorded no PID", dir, w.exitWait)
	case first:
		return "", fmt.Errorf("pid %d still holds %s %s after the task stopped, so it is not the task's own process "+
			"(a serve started by hand, or this shell's SEAMLESS_CONFIG or SEAMLESS_DATA_DIR names another daemon's data dir); "+
			"stop it yourself, or check those settings", pid, dir, w.exitWait)
	default:
		return "", fmt.Errorf("pid %d let go of %s, but pid %d holds it now", first, dir, pid)
	}
}

// waitFree polls the lock every tick until it is free or deadline has passed,
// and reports whether it came free and the PID the last poll read.
func (w daemonExit) waitFree(tick <-chan time.Time, deadline time.Time) (bool, int) {
	for {
		<-tick
		held, pid := w.holder(w.dataDir)
		if !held {
			return true, 0
		}
		if !w.now().Before(deadline) {
			return false, pid
		}
	}
}

// reportServiceStatus streams the platform tool's own status output verbatim
// (launchctl print / systemctl --user status / schtasks /Query). A non-zero exit
// -- an installed-but-not-loaded service -- is a dim hint, not an error, because
// status is informational.
func reportServiceStatus(p serviceControlPlan) error {
	if len(p.Cmds) == 0 {
		return nil
	}
	out, err := p.Cmds[0].CombinedOutput()
	if s := strings.TrimRight(string(out), "\n"); s != "" {
		fmt.Println(s)
	}
	if err != nil {
		fmt.Printf("%s %s\n", dim(p.Label+":"), dim("not currently loaded (run 'seamlessd start')"))
	}
	return nil
}

// pastTense is the confirmation verb printed after a completed action.
func pastTense(action serviceAction) string {
	switch action {
	case actionStart:
		return "started"
	case actionStop:
		return "stopped"
	case actionRestart:
		return "restarted"
	default:
		return string(action)
	}
}
