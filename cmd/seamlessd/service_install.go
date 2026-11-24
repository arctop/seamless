package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// install-service registers the Windows service: the at-logon Scheduled Task
// the installer runs the daemon from, the analog of the LaunchAgent plist and
// the systemd --user unit docs/install writes. install.ps1 used to build it
// with PowerShell's Task Scheduler cmdlets, which go through a CIM provider
// that a session with a reduced token (SSH, a management agent's run as the
// user) cannot reach; schtasks.exe works from any token. So the definition is
// written here, as the task XML, next to the verbs that already drive the task
// through schtasks (service.go, uninstall.go) and the updater's own task
// (update_spawn.go), and registered with schtasks /Create /XML.

// serviceProtocolLine is the line `seamlessd install-service -h` prints;
// install.ps1 looks for it (exactly) and registers the task through the
// PowerShell cmdlets, as before, for a release without it.
const serviceProtocolLine = "service protocol: 1"

// serviceTaskDescription is the task's description: it carries the
// installer's marker (update.InstallerMarker), which install_probe.go reads
// as "the installer wrote this", in the words install.ps1 has always written.
const serviceTaskDescription = "Written by https://thereisnospoon.org/install.ps1. Re-run it to update."

// serviceTaskTemplate is the Seamless task's definition: what install.ps1's
// Register-ScheduledTask call built. The trigger is the user's own logon --
// an any-user logon trigger needs admin -- and the principal is the user's
// SID as an InteractiveToken at LeastPrivilege: no admin and no stored
// password, so it runs as you while you are signed in. No execution time
// limit (the default is three days), a restart a minute after a failure,
// three times, as KeepAlive and Restart=always do, and IgnoreNew so a trigger
// that fires twice starts no second daemon.
//
// The security descriptor gives the user full control of the task on top of
// what the task folder hands down (Administrators and SYSTEM). A task takes
// its owner from the token that creates it: an elevated PowerShell (Run as
// administrator) makes it Administrators' and leaves the user read access
// only, after which every run without elevation -- the next install, every
// automatic update -- is refused replacing it (it may still run and end it).
// With this entry the user can replace a task whoever created it. Task
// Scheduler applies a definition's descriptor when it creates the task only:
// replacing one keeps the descriptor it has, so a task an elevated
// PowerShell created before this entry existed stays out of reach until an
// admin deletes it (registerServiceTask). The entry grants nothing the
// user's own registration does not: the principal stays the user at
// LeastPrivilege, and Task Scheduler checks a replacement's run level against
// the replacing token.
//
// The values are the user's SID (the descriptor), the description, the
// trigger's account (DOMAIN\name), the principal's SID, the executable and
// the argument string, each XML-escaped.
const serviceTaskTemplate = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><SecurityDescriptor>D:(A;;FA;;;%s)</SecurityDescriptor><Description>%s</Description></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled><UserId>%s</UserId></LogonTrigger></Triggers>
  <Principals><Principal id="Author"><UserId>%s</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure><Interval>PT1M</Interval><Count>3</Count></RestartOnFailure>
  </Settings>
  <Actions Context="Author"><Exec><Command>%s</Command><Arguments>%s</Arguments></Exec></Actions>
</Task>
`

// serviceTaskArgs is the task action's argv after the executable: serve with
// the config to pin and the log file to write, which a plist or unit would
// carry as an env prefix and a stderr redirect (applyServeEnv).
func serviceTaskArgs(config, logFile string) []string {
	return []string{"serve", "--config", config, "--log-file", logFile}
}

// serviceTaskPlan is the registration as data: the definition, and the
// schtasks commands that register and start it.
type serviceTaskPlan struct {
	name string
	// definition is the task XML, UTF-16 little endian behind a byte-order
	// mark, as schtasks /XML reads it.
	definition []byte
}

// newServiceTaskPlan plans the Seamless task for the user sid, whose account
// name the logon trigger names, running exe with arguments: serviceTaskArgs
// already escaped and joined into the one string a task action carries
// (taskArguments).
func newServiceTaskPlan(sid, account, exe, arguments string) (serviceTaskPlan, error) {
	def, err := taskDefinitionXML(serviceTaskTemplate, sid, serviceTaskDescription, account, sid, exe, arguments)
	if err != nil {
		return serviceTaskPlan{}, fmt.Errorf("escape the %s task definition: %w", scheduledTask, err)
	}
	return serviceTaskPlan{name: scheduledTask, definition: def}, nil
}

// create is the schtasks command that registers the task from the definition
// written at path, replacing the one there.
func (p serviceTaskPlan) create(path string) []string {
	return []string{"schtasks", "/Create", "/TN", p.name, "/XML", path, "/F"}
}

// run is the schtasks command that starts the task.
func (p serviceTaskPlan) run() []string { return []string{"schtasks", "/Run", "/TN", p.name} }

// remove is the schtasks command that unregisters the task.
func (p serviceTaskPlan) remove() []string {
	return []string{"schtasks", "/Delete", "/TN", p.name, "/F"}
}

// serviceTaskEnv is what registering the task reads and runs, injected so a
// test drives it on any OS.
type serviceTaskEnv struct {
	// sid and account are the user this process runs as: S-1-... and
	// DOMAIN\name.
	sid, account string
	// exe is this seamlessd.exe, which the task runs.
	exe string
	// arguments joins an argv the way a task action carries it
	// (taskArguments); splitArgs splits such a string back up the way
	// Windows does.
	arguments func(args []string) string
	splitArgs func(arguments string) ([]string, error)
	// queryXML reads the registered task's definition (schtasks /Query /XML);
	// an error means none could be read.
	queryXML func() ([]byte, error)
	// isCurrentUser reports whether a task principal names this user.
	isCurrentUser func(account string) (bool, error)
	// write writes a definition to a file of its own and returns its path;
	// remove deletes that file again.
	write  func(def []byte) (string, error)
	remove func(path string) error
	// run runs one schtasks command and returns its combined output.
	run func(argv []string) ([]byte, error)
}

// registerServiceTask registers the Seamless task for config and logFile and
// starts it. It leaves either a task that is registered and running or none
// of this run's: a task that cannot start is removed again, so the
// installer's Startup-folder fallback never runs a second daemon beside it at
// the next logon. The one exception, reported as kept: a task of this user's
// that schtasks will not replace -- one an elevated PowerShell created
// without the descriptor in serviceTaskTemplate -- but that already runs this
// seamlessd.exe with these arguments is started as it is. It still starts the
// daemon at logon, and this run cannot remove it, so the fallback would only
// add a second starter. Every error is one line for the person installing,
// which install.ps1 shows as it is.
func registerServiceTask(env serviceTaskEnv, config, logFile string) (kept bool, err error) {
	current := readRegisteredTask(env)
	if current.foreign {
		return false, fmt.Errorf("could not register the %s Scheduled Task: it belongs to %s, and replacing it needs admin "+
			"(one-time fix: an elevated 'schtasks /delete /tn %s /f', then re-run the installer)", scheduledTask, current.def.UserID, scheduledTask)
	}
	args := serviceTaskArgs(config, logFile)
	plan, err := newServiceTaskPlan(env.sid, env.account, env.exe, env.arguments(args))
	if err != nil {
		return false, err
	}
	path, err := env.write(plan.definition)
	if err != nil {
		return false, fmt.Errorf("could not register the %s Scheduled Task: %w", scheduledTask, err)
	}
	defer func() {
		_ = env.remove(path) //nolint:errcheck // a leftover definition is inert, and the next run writes its own
	}()
	if out, err := env.run(plan.create(path)); err != nil {
		createErr := withFirstLine(err, out)
		switch {
		case current.runs(env, env.exe, args):
			if out, err := env.run(plan.run()); err != nil {
				return false, fmt.Errorf("could not start the %s Scheduled Task already registered for this seamlessd.exe: schtasks /Run: %w",
					scheduledTask, withFirstLine(err, out))
			}
			return true, nil
		case current.mine:
			return false, fmt.Errorf("could not replace the %s Scheduled Task: schtasks /Create: %w "+
				"(if an elevated PowerShell created it, only an admin may replace it; one-time fix: "+
				"an elevated 'schtasks /delete /tn %s /f', then re-run the installer)", scheduledTask, createErr, scheduledTask)
		default:
			// A task this user may not read reads as none: the name may be
			// another user's, as likely as task creation is barred.
			return false, fmt.Errorf("could not register the %s Scheduled Task: schtasks /Create: %w "+
				"(another user of this computer may hold the name %s, which only an admin may delete, "+
				"or group policy bars task creation for standard users)", scheduledTask, createErr, scheduledTask)
		}
	}
	if out, err := env.run(plan.run()); err != nil {
		_, _ = env.run(plan.remove()) //nolint:errcheck // best-effort: the error below is the one to report
		return false, fmt.Errorf("registered the %s Scheduled Task, but it could not start, so it was removed again: schtasks /Run: %w",
			scheduledTask, withFirstLine(err, out))
	}
	return false, nil
}

// registeredTask is the Seamless task already registered, as far as
// install-service can read it.
type registeredTask struct {
	def taskDefinition
	// mine and foreign say whose its principal is: this user's, or another
	// account's -- the task name is the whole machine's, and only an admin may
	// replace another user's task. Neither holds when there is no task, it
	// cannot be read, or its principal cannot be matched to an account
	// (schtasks /Create then has the last word).
	mine, foreign bool
}

// readRegisteredTask reads the registered Seamless task.
func readRegisteredTask(env serviceTaskEnv) registeredTask {
	raw, err := env.queryXML()
	if err != nil {
		return registeredTask{}
	}
	def, err := parseTaskXML(raw)
	if err != nil || def.UserID == "" {
		return registeredTask{}
	}
	mine, err := env.isCurrentUser(def.UserID)
	if err != nil {
		return registeredTask{def: def}
	}
	return registeredTask{def: def, mine: mine, foreign: !mine}
}

// runs reports whether the task is this user's and already runs exe with
// args: what install-service would register, however its arguments were
// quoted (install.ps1's cmdlets quoted every path; taskArguments quotes only
// a path that needs it).
func (r registeredTask) runs(env serviceTaskEnv, exe string, args []string) bool {
	if !r.mine || !strings.EqualFold(r.def.Command, exe) {
		return false
	}
	got, err := env.splitArgs(r.def.Arguments)
	return err == nil && slices.Equal(got, args)
}

// installServiceFlags is install-service's flag set, writing its usage to out.
func installServiceFlags(out io.Writer) (*flag.FlagSet, *string, *string) {
	fs := flag.NewFlagSet("install-service", flag.ContinueOnError)
	fs.SetOutput(out)
	config := fs.String("config", "", "the seamless.yaml the task's serve pins (`PATH`, absolute)")
	logFile := fs.String("log-file", "", "the log file the task's serve writes (`PATH`, absolute)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: seamlessd install-service --config PATH --log-file PATH\n\n"+
			"Registers and starts the Windows service: the at-logon Scheduled Task %s, running\n"+
			"this seamlessd.exe as you, with no admin. The installer runs it; run it yourself\n"+
			"to put back a task that was deleted. Windows only.\n\n", scheduledTask)
		fs.PrintDefaults()
		fmt.Fprintf(fs.Output(), "\n%s\n", serviceProtocolLine)
	}
	return fs, config, logFile
}

// runInstallService is `seamlessd install-service`.
func runInstallService(args []string) error {
	fs, config, logFile := installServiceFlags(os.Stdout)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("seamlessd.install-service: unexpected argument %q", fs.Arg(0))
	}
	if runtime.GOOS != "windows" {
		return errors.New("seamlessd.install-service: only Windows registers its service this way; on macOS and Linux the installer writes the service itself")
	}
	for _, f := range []struct{ name, value string }{{"--config", *config}, {"--log-file", *logFile}} {
		if !filepath.IsAbs(f.value) {
			return fmt.Errorf("seamlessd.install-service: %s needs an absolute path, not %q", f.name, f.value)
		}
	}
	env, err := realServiceTaskEnv()
	if err != nil {
		return &ownerError{msg: "could not register the " + scheduledTask + " Scheduled Task: " + err.Error(), cause: err}
	}
	kept, err := registerServiceTask(env, *config, *logFile)
	if err != nil {
		return &ownerError{msg: err.Error(), cause: err}
	}
	if kept {
		fmt.Printf("started the %s Scheduled Task (at logon, as %s) as it was: it already runs this seamlessd.exe, "+
			"and only an admin may replace it\n", scheduledTask, env.account)
		return nil
	}
	fmt.Printf("registered and started the %s Scheduled Task (at logon, as %s)\n", scheduledTask, env.account)
	return nil
}

// writeTaskFile writes a task definition to a new file in dir (the user's
// temp dir for "") named after pattern (os.CreateTemp's) and returns its path.
// The Seamless task's goes to the temp dir; the update task's to the update
// state dir (writeTaskDefinition).
func writeTaskFile(dir, pattern string, def []byte) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("write the task definition: %w", err)
	}
	_, werr := f.Write(def)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(f.Name()) //nolint:errcheck // the write already failed; a leftover partial file is inert
		return "", fmt.Errorf("write the task definition: %w", werr)
	}
	return f.Name(), nil
}

// trimAccount drops what LookupAccount gives back around an account's parts.
func trimAccount(domain, name string) string {
	domain, name = strings.TrimSpace(domain), strings.TrimSpace(name)
	if domain == "" {
		return name
	}
	return domain + `\` + name
}
