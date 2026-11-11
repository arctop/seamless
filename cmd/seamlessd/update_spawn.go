package main

// The daemon's updater spawner: update.Spawner, one per OS (plan 2.3, settled
// by the 2.01 spikes in lab detached-updater-os-spikes). The updater, `seamlessd
// update --auto`, runs the installer that restarts the daemon, so it must run
// where stopping the service does not reach it:
//
//   - darwin: a child in a session of its own (Setsid). launchd stops a job by
//     signalling its process group, which a session leader has left (memory
//     macos-updater-detach-setsid-outside-process-group).
//   - linux: a transient user service of its own, from systemd-run. systemd
//     stops the whole cgroup of seamless.service, Setsid children included
//     (memory linux-updater-detach-systemd-run-transient-service).
//   - windows: a detached child, else a per-user on-demand Scheduled Task.
//     Task Scheduler's stop ends only the task's own process, so both outlive
//     it (memory windows-updater-detach-under-task-scheduler).
//   - any other OS: no spawner, so the daemon only notifies.
//
// Every spawner runs the seamlessd on disk with updaterArgs, the contract
// between releases: that binary can be newer than the daemon, and the updater
// refuses unless it is the release the attempt updates from. None takes
// update.lock, which the updater waits for, and none ties the updater to the
// daemon's context. The plans are pure values here, so their tests run on any
// OS; the system calls live in update_spawn_<os>.go.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/update"
)

// spawnTarget is what a spawner starts: the seamlessd on disk and the daemon's
// config file, both absolute, plus the data dir for anything a spawner writes
// on the way (the Windows task definition).
type spawnTarget struct {
	exe, configPath, dataDir string
}

// args is the updater's command line after the executable.
func (t spawnTarget) args(req update.SpawnRequest) []string {
	return updaterArgs(req, t.configPath)
}

// newUpdateSpawner builds the daemon's update.Spawner for this OS, once, at
// startup: the executable is resolved now, while its path still names the
// running binary (an update renames a new one over it, after which Linux's
// /proc/self/exe names a deleted file). It returns nil -- the daemon then
// notifies and never applies -- on an OS with no way to detach the updater,
// when the executable cannot be located, and when no config file is loaded:
// the updater needs the daemon's config as an absolute path.
func newUpdateSpawner(cfg config.Config, logger *slog.Logger) update.Spawner {
	if logger == nil {
		logger = slog.Default()
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		logger.Warn("update: automatic updates are unavailable: the running executable could not be located", "err", err)
		return nil
	}
	configPath := absConfigPath(cfg.SourcePath())
	if !filepath.IsAbs(configPath) {
		logger.Debug("update: automatic updates are unavailable: no config file is loaded")
		return nil
	}
	return osUpdateSpawner(spawnTarget{exe: exe, configPath: configPath, dataDir: cfg.DataDir}, logger)
}

// handoffTimeout bounds one hand-off command (systemd-run, schtasks), which
// returns within milliseconds when the service manager is healthy.
const handoffTimeout = 30 * time.Second

// errHandoffTimeout is a hand-off command still running at its timeout, which
// then kills it.
var errHandoffTimeout = errors.New("timed out")

// runHandoff runs one hand-off command to its end and returns its combined
// output, buffered: the command exits once the service manager has the
// updater, which never inherits its stdio. The daemon's ctx does not kill it,
// since it may already have handed the updater over: a cancelled ctx returns
// at once and leaves the command to finish on its own. Only timeout kills it.
func runHandoff(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	return runHandoffEnv(ctx, timeout, nil, name, args...)
}

// runHandoffEnv is runHandoff with the command's environment: env, or this
// process's own when env is nil.
func runHandoffEnv(ctx context.Context, timeout time.Duration, env []string, name string, args ...string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	cmd := exec.CommandContext(runCtx, name, args...)
	cmd.Env = env
	cmd.SysProcAttr = handoffProcAttr()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if err != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("%w after %s: %w", errHandoffTimeout, timeout, err)
		}
		cancel()
		done <- err
	}()
	select {
	case err := <-done:
		return out.Bytes(), err
	case <-ctx.Done():
		return nil, fmt.Errorf("%s may still hand the updater over: %w", name, ctx.Err())
	}
}

// withFirstLine adds the first line of a command's output to its error, when
// it printed anything.
func withFirstLine(err error, out []byte) error {
	if len(bytes.TrimSpace(out)) == 0 {
		return err
	}
	return fmt.Errorf("%w: %s", err, firstLine(string(out)))
}

// Linux: the transient unit (memory linux-updater-detach-systemd-run-transient-service).
const (
	// updateUnitPrefix names an attempt's unit: seamless-update-<id>.service.
	updateUnitPrefix = "seamless-update-"
	// updateUnitMaxRuntime is the unit's RuntimeMaxSec, twice the installer's
	// timeout. systemd then stops the unit, SIGTERM then SIGKILL, taking along
	// whatever the updater left running.
	updateUnitMaxRuntime = 2 * installerTimeout
)

// forwardedEnv lists the variables an attempt's unit gets from the daemon. A
// transient unit starts from the user manager's environment, never the
// daemon's, and what the release download needs from the daemon's is its proxy
// and CA bundle settings, in both spellings (curl reads the lower-case ones).
var forwardedEnv = []string{
	"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy",
	"NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
}

// systemdRunArgv is the command that starts req's updater as a transient user
// service, environ being the daemon's:
//
//	systemd-run --user --unit=seamless-update-<id> --collect -p Type=exec -p RuntimeMaxSec=1800 [--setenv=NAME=VALUE ...] -- <exe> update --auto ...
//
// --collect drops the unit however it ends, so a failed attempt never holds
// its name. With Type=exec systemd-run returns only once the updater was
// exec'd, so a binary that cannot run fails the spawn, not the attempt. Each
// --setenv is
// NAME=VALUE for a forwardedEnv variable the daemon has set and non-empty: the
// bare --setenv=NAME fails the whole spawn on systemd 249 (memory
// systemd-run-setenv-inherit-form-fails-on-249). No --wait, --pipe, --pty or
// --no-block: those need a user bus, which Debian does not ship. req must have
// passed Validate, because systemd-run silently escapes an invalid unit name.
func systemdRunArgv(t spawnTarget, req update.SpawnRequest, environ []string) []string {
	argv := []string{
		"systemd-run", "--user",
		"--unit=" + updateUnitPrefix + req.AttemptID,
		"--collect",
		"-p", "Type=exec",
		"-p", fmt.Sprintf("RuntimeMaxSec=%d", int(updateUnitMaxRuntime/time.Second)),
	}
	for _, name := range forwardedEnv {
		if v := envValue(environ, name); v != "" {
			argv = append(argv, "--setenv="+name+"="+v)
		}
	}
	argv = append(argv, "--", t.exe)
	return append(argv, t.args(req)...)
}

// systemdRunEnv is the environment systemd-run itself runs with: the
// daemon's, plus XDG_RUNTIME_DIR=/run/user/<uid> when the daemon has none.
// systemd-run --user finds the user manager through it, and a per-user
// unit's daemon has it from its manager, but a peer_auth daemon's system unit
// starts without one: its updater runs under the daemon account's lingering
// user manager all the same. The unit itself never sees this environment.
func systemdRunEnv(environ []string, uid int) []string {
	if envValue(environ, "XDG_RUNTIME_DIR") != "" {
		return environ
	}
	out := slices.Clone(environ)
	return append(out, fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid))
}

// systemdRunFailure says why systemd-run did not start the updater, from its
// output: it exits 1 whatever went wrong, so the text is all there is to tell
// one failure from another. The phrases are systemd's own, as systemd 249-261
// print them.
func systemdRunFailure(out []byte, err error) error {
	text := string(out)
	var why string
	switch {
	case errors.Is(err, exec.ErrNotFound):
		why = "systemd-run is not installed"
	case errors.Is(err, errHandoffTimeout):
		why = "systemd-run did not return"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		why = "the daemon stopped waiting for systemd-run"
	case strings.Contains(text, "Failed to connect to"):
		why = "the systemd user manager is unreachable"
	case strings.Contains(text, "already loaded or has a fragment file"):
		why = "this attempt's unit already exists"
	case strings.Contains(text, "Failed to find executable"):
		why = "the seamlessd binary is missing or not executable"
	case strings.Contains(text, "Job for ") && strings.Contains(text, " failed"):
		why = "the updater could not be executed"
	default:
		why = "systemd-run failed"
	}
	return withFirstLine(fmt.Errorf("%s: %w", why, err), out)
}

// Windows: a detached child, else the per-user on-demand update task (memory
// windows-updater-detach-under-task-scheduler).

// childElseTask is the Windows spawn order, apart from its system calls so its
// tests run on any OS: child starts the updater as a detached process, and
// task, the update task, runs only when child fails. A start through the task
// is a warning naming why the child failed; both failing is one error naming
// both. A child that fails once ctx is done -- the daemon is shutting down --
// is final: it started nothing, and the task would start nothing either, since
// runHandoff gives up on /Create at once and /Run is never sent, while that
// /Create, left running, could still register the task. ctx never holds the
// child back, as on darwin: CreateProcess has started it or failed when it
// returns.
func childElseTask(ctx context.Context, logger *slog.Logger, child, task func() error) error {
	childErr := child()
	if childErr == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("seamlessd.spawn: a detached process: %w", childErr)
	}
	if err := task(); err != nil {
		return fmt.Errorf("seamlessd.spawn: a detached process: %w; the update task: %w", childErr, err)
	}
	logger.Warn("update: the updater could not start as a detached process, so the update task starts it", "err", childErr)
	return nil
}

// updateTaskPrefix names the update task, SeamlessUpdate-<SID>: task names are
// machine-wide, and the owner's SID keeps each user's apart.
const updateTaskPrefix = "SeamlessUpdate-"

func updateTaskName(sid string) string { return updateTaskPrefix + sid }

// updateTaskDescription is the update task's description in Task Scheduler.
// It is not the installer's marker: this task is no service definition.
const updateTaskDescription = "Runs the Seamless updater (seamlessd update --auto) when the Seamless daemon starts an automatic update. seamlessd rewrites it for every update; seamlessd uninstall removes it."

// updateTaskTemplate is the update task's definition, as 2.01 registered and
// ran it on Windows 11: the owner's own SID as an InteractiveToken principal
// at LeastPrivilege (a standard user is refused an S4U task, and the daemon's
// task runs only while the owner is signed in anyway), no trigger -- it runs
// only when started -- hidden, IgnoreNew, and an hour's limit. The values are
// the description, the SID, the executable and the argument string, each
// XML-escaped.
const updateTaskTemplate = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>%s</Description></RegistrationInfo>
  <Principals><Principal id="Author"><UserId>%s</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>true</Hidden>
    <ExecutionTimeLimit>PT1H</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author"><Exec><Command>%s</Command><Arguments>%s</Arguments></Exec></Actions>
</Task>
`

// updateTaskPlan is the Windows fallback as data: register the task from
// its definition with schtasks, CIM-free (it works from any token, an SSH
// one included), then run it.
type updateTaskPlan struct {
	name string
	// definition is the task XML, UTF-16 little endian behind a byte-order
	// mark, as schtasks /XML reads it.
	definition []byte
}

// newUpdateTaskPlan plans the update task for the user sid, running exe with
// arguments: the updater's arguments already escaped and joined into the one
// string a task action carries (taskArguments).
func newUpdateTaskPlan(sid, exe, arguments string) (updateTaskPlan, error) {
	vals := []string{updateTaskDescription, sid, exe, arguments}
	esc := make([]any, len(vals))
	for i, v := range vals {
		var b strings.Builder
		if err := xml.EscapeText(&b, []byte(v)); err != nil {
			return updateTaskPlan{}, fmt.Errorf("escape the update task definition: %w", err)
		}
		esc[i] = b.String()
	}
	return updateTaskPlan{
		name:       updateTaskName(sid),
		definition: utf16WithBOM(fmt.Sprintf(updateTaskTemplate, esc...)),
	}, nil
}

// create is the schtasks command that registers the task from the definition
// written at path, replacing the previous attempt's.
func (p updateTaskPlan) create(path string) []string {
	return []string{"schtasks", "/Create", "/TN", p.name, "/XML", path, "/F"}
}

// run is the schtasks command that starts the task.
func (p updateTaskPlan) run() []string { return []string{"schtasks", "/Run", "/TN", p.name} }

// utf16WithBOM encodes s as UTF-16 little endian behind a byte-order mark,
// which is how schtasks reads and writes task XML.
func utf16WithBOM(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2, 2+2*len(units))
	out[0], out[1] = 0xFF, 0xFE
	for _, u := range units {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

// The detached child's CreateProcess flags and the error it retries on,
// Windows' own values (update_spawn_windows_test.go holds them to
// x/sys/windows): no console window, a process group of its own, out of the
// daemon's job.
const (
	createNewProcessGroup  = 0x00000200 // CREATE_NEW_PROCESS_GROUP
	createBreakawayFromJob = 0x01000000 // CREATE_BREAKAWAY_FROM_JOB
	createNoWindow         = 0x08000000 // CREATE_NO_WINDOW

	detachedChildFlags = createNoWindow | createNewProcessGroup | createBreakawayFromJob

	errAccessDenied = syscall.Errno(5) // ERROR_ACCESS_DENIED
)

// withoutBreakaway decides the detached child's one retry. CreateProcess
// refuses CREATE_BREAKAWAY_FROM_JOB with ERROR_ACCESS_DENIED when the daemon's
// job does not allow breakaway at that moment -- Task Scheduler toggles its
// job's SILENT_BREAKAWAY_OK -- and that job has no KILL_ON_JOB_CLOSE, so a
// child left in it survives the task's stop just the same (2.01). It returns
// the flags to retry with, or false when there is no retry.
func withoutBreakaway(flags uint32, err error) (uint32, bool) {
	if flags&createBreakawayFromJob == 0 || !errors.Is(err, errAccessDenied) {
		return flags, false
	}
	return flags &^ createBreakawayFromJob, true
}
