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

// serviceDeferredExit is install-service's exit code for a task it
// registered that Windows starts at the user's next sign-in, not now
// (errServiceDeferred); install.ps1's $ServiceDeferredExit, which a test holds
// to it, reads it as installed and skips the health check.
const serviceDeferredExit = 3

// errServiceDeferred is that outcome, after install-service said so: main
// exits serviceDeferredExit for it.
var errServiceDeferred = errors.New("the Seamless task starts at the next sign-in")

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

// newServiceTaskPlan plans the Seamless task of the user sid
// (serviceTaskName), whose account name the logon trigger names, running exe
// with arguments: serviceTaskArgs already escaped and joined into the one
// string a task action carries (taskArguments).
func newServiceTaskPlan(sid, account, exe, arguments string) (serviceTaskPlan, error) {
	name := serviceTaskName(sid)
	def, err := taskDefinitionXML(serviceTaskTemplate, sid, serviceTaskDescription, account, sid, exe, arguments)
	if err != nil {
		return serviceTaskPlan{}, fmt.Errorf("escape the %s task definition: %w", name, err)
	}
	return serviceTaskPlan{name: name, definition: def}, nil
}

// create is the schtasks command that registers the task from the definition
// written at path, replacing the one there.
func (p serviceTaskPlan) create(path string) []string {
	return []string{"schtasks", "/Create", "/TN", p.name, "/XML", path, "/F"}
}

// run is the schtasks command that starts the task, and end the one that
// stops it.
func (p serviceTaskPlan) run() []string { return []string{"schtasks", "/Run", "/TN", p.name} }
func (p serviceTaskPlan) end() []string { return []string{"schtasks", "/End", "/TN", p.name} }

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
	// queryXML and isCurrentUser read the tasks already registered
	// (taskLookup).
	queryXML      func(name string) ([]byte, error)
	isCurrentUser func(account string) (bool, error)
	// write writes a definition to a file of its own and returns its path;
	// remove deletes that file again.
	write  func(def []byte) (string, error)
	remove func(path string) error
	// run runs one schtasks command and returns its combined output.
	run func(argv []string) ([]byte, error)
	// started reports whether Windows started the task name that schtasks
	// /Run asked for (taskStarted).
	started func(name string) bool
}

// registeredService is what registerServiceTask left registered and
// running: the task's name, and whether it is a task it found and kept rather
// than its own, and whether it removed this user's \Seamless. deferred says
// Windows did not start it: nobody is signed in as this user, and it starts
// at their next sign-in.
type registeredService struct {
	name                     string
	kept, migrated, deferred bool
}

// registerServiceTask registers this user's Seamless task for config and
// logFile and starts it. It leaves either a task that is registered and
// running or none of this run's: a task that cannot start is removed again,
// so the installer's Startup-folder fallback never runs a second daemon
// beside it at the next logon. Every error is one line for the person
// installing, which install.ps1 shows as it is.
//
// A \Seamless of this user's, which a release before the per-user name
// registered, is ended and deleted once the new task is registered. Only an
// admin may delete one an elevated PowerShell created, and two tasks would
// start two daemons at every logon, so then the new task goes again: the old
// one is started as it is (kept) when it already runs this seamlessd.exe with
// these arguments, and otherwise the error names the admin's one-time fix. It
// is kept the same way when task creation is barred. Another user's
// \Seamless is left alone.
//
// A task that /Run did not start because this user is not signed in to
// Windows (an install over SSH, say) stays registered, reported as deferred:
// it starts the daemon at their next sign-in, and the fallback would not
// start sooner.
func registerServiceTask(env serviceTaskEnv, config, logFile string) (registeredService, error) {
	lookup := env.lookup()
	name := serviceTaskName(env.sid)
	if current := lookup.read(name); current.foreign {
		return registeredService{}, fmt.Errorf("could not register the %s Scheduled Task: it belongs to %s, and replacing it needs admin "+
			"(one-time fix: an elevated 'schtasks /delete /tn %s /f', then re-run the installer)", name, current.def.UserID, name)
	}
	legacy := lookup.read(legacyServiceTask)
	args := serviceTaskArgs(config, logFile)
	plan, err := newServiceTaskPlan(env.sid, env.account, env.exe, env.arguments(args))
	if err != nil {
		return registeredService{}, err
	}
	path, err := env.write(plan.definition)
	if err != nil {
		return registeredService{}, fmt.Errorf("could not register the %s Scheduled Task: %w", name, err)
	}
	defer func() {
		_ = env.remove(path) //nolint:errcheck // a leftover definition is inert, and the next run writes its own
	}()
	old := serviceTaskPlan{name: legacyServiceTask}
	if out, err := env.run(plan.create(path)); err != nil {
		if legacy.runs(env, env.exe, args) {
			return keepLegacyTask(env, old)
		}
		return registeredService{}, fmt.Errorf("could not register the %s Scheduled Task: schtasks /Create: %w "+
			"(group policy may bar task creation for standard users)", name, withFirstLine(err, out))
	}
	migrated := false
	if legacy.mine {
		_, _ = env.run(old.end()) //nolint:errcheck // best-effort: a task that is not running has nothing to end
		if out, err := env.run(old.remove()); err != nil {
			_, _ = env.run(plan.remove()) //nolint:errcheck // best-effort: the result below is the one to report
			if legacy.runs(env, env.exe, args) {
				return keepLegacyTask(env, old)
			}
			return registeredService{}, fmt.Errorf("could not replace the %s Scheduled Task an earlier release registered: schtasks /Delete: %w "+
				"(if an elevated PowerShell created it, only an admin may delete it; one-time fix: "+
				"an elevated 'schtasks /delete /tn %s /f', then re-run the installer)", legacyServiceTask, withFirstLine(err, out), legacyServiceTask)
		}
		migrated = true
	}
	if out, err := env.run(plan.run()); err != nil {
		_, _ = env.run(plan.remove()) //nolint:errcheck // best-effort: the error below is the one to report
		return registeredService{}, fmt.Errorf("registered the %s Scheduled Task, but it could not start, so it was removed again: schtasks /Run: %w",
			name, withFirstLine(err, out))
	}
	return registeredService{name: name, migrated: migrated, deferred: !env.started(name)}, nil
}

// keepLegacyTask starts this user's \Seamless as it is: it already runs this
// seamlessd.exe with these arguments and starts the daemon at logon, and this
// run can neither register a task beside it nor delete it.
func keepLegacyTask(env serviceTaskEnv, old serviceTaskPlan) (registeredService, error) {
	if out, err := env.run(old.run()); err != nil {
		return registeredService{}, fmt.Errorf("could not start the %s Scheduled Task already registered for this seamlessd.exe: schtasks /Run: %w",
			old.name, withFirstLine(err, out))
	}
	return registeredService{name: old.name, kept: true, deferred: !env.started(old.name)}, nil
}

// lookup reads registered tasks through env.
func (env serviceTaskEnv) lookup() taskLookup {
	return taskLookup{sid: env.sid, queryXML: env.queryXML, isCurrentUser: env.isCurrentUser}
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
			"Registers and starts the Windows service: the at-logon Scheduled Task %s<your SID>,\n"+
			"running this seamlessd.exe as you, with no admin, in place of the %s task an earlier\n"+
			"release registered. The installer runs it; run it yourself to put back a task that\n"+
			"was deleted. Windows only.\n\n", serviceTaskPrefix, legacyServiceTask)
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
		return &ownerError{msg: "could not register the Seamless Scheduled Task: " + err.Error(), cause: err}
	}
	got, err := registerServiceTask(env, *config, *logFile)
	if err != nil {
		return &ownerError{msg: err.Error(), cause: err}
	}
	if got.deferred {
		fmt.Printf("registered the %s Scheduled Task (at logon, as %s); Windows starts it when you sign in: "+
			"nobody is signed in to Windows as %s now, so it could not start yet\n", got.name, env.account, env.account)
		return errServiceDeferred
	}
	switch {
	case got.kept:
		fmt.Printf("started the %s Scheduled Task (at logon, as %s) as it was: it already runs this seamlessd.exe, "+
			"and only an admin may replace it\n", got.name, env.account)
	case got.migrated:
		fmt.Printf("registered and started the %s Scheduled Task (at logon, as %s) in place of the %s task an earlier release registered\n",
			got.name, env.account, legacyServiceTask)
	default:
		fmt.Printf("registered and started the %s Scheduled Task (at logon, as %s)\n", got.name, env.account)
	}
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
