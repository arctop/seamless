package main

// The OS reads behind update.Detect. Detect is a pure function of an
// update.Probe; this file gathers the probe -- the service definition the
// installer would own, the config in use, the environment, whether this
// process is the supervised instance -- so the classification itself stays
// testable on any OS while every system call lives here. The parsers are
// plain functions over bytes, so the darwin, linux and windows fixtures all
// run on the Linux CI.

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/update"
)

// installProbeEnv is the slice of the operating system probeInstall reads,
// injected so tests can stand up any OS's layout from fixtures.
type installProbeEnv struct {
	goos     string
	home     string
	exe      string   // this executable, symlinks resolved; "" when unknown
	environ  []string // KEY=VALUE, as os.Environ
	ppid     int
	euid     int
	readFile func(path string) ([]byte, error)
	sameFile func(a, b string) bool
	writable func(dir string) bool
	// taskXML returns the Scheduled Task's definition (`schtasks /Query /TN
	// Seamless /XML`); an error means the task is absent or unreadable.
	taskXML func() ([]byte, error)
	// parentImage names this process's parent's executable, without its
	// directory, and isCurrentUser reports whether a Scheduled Task principal
	// names the user this process runs as: the Windows half of gate 9.
	parentImage   func() (string, error)
	isCurrentUser func(account string) (bool, error)
	// resolve, daemonPin and systemAccount judge a peer_auth daemon's
	// account (access.LookupAccount, access.DaemonAccount,
	// access.SystemAccount): the system half of the probe (probeSystem).
	resolve       access.Resolver
	daemonPin     func() (access.DaemonPin, bool, error)
	systemAccount func(uid uint32) (bool, string)
}

// realInstallProbeEnv reads the live process and machine.
func realInstallProbeEnv() installProbeEnv {
	env := installProbeEnv{
		goos:     runtime.GOOS,
		environ:  os.Environ(),
		ppid:     os.Getppid(),
		euid:     os.Geteuid(),
		readFile: os.ReadFile,
		sameFile: sameFile,
		writable: dirWritable,
		// Read once: the service probe and gate 9 both parse it.
		taskXML: sync.OnceValues(func() ([]byte, error) {
			return exec.Command("schtasks", "/Query", "/TN", scheduledTask, "/XML").Output()
		}),
		parentImage:   parentImageName,
		isCurrentUser: isCurrentUser,
		resolve:       access.LookupAccount,
		daemonPin:     access.DaemonAccount,
		systemAccount: access.SystemAccount,
	}
	if home, err := os.UserHomeDir(); err == nil {
		env.home = home
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		env.exe = exe
	}
	return env
}

// installConfig is what the install probe reads of the loaded config.
type installConfig struct {
	// path is the config file this process loaded, absolute; "" for none.
	path string
	// client is role: client.
	client bool
	// peerAuth is peer_auth.enabled and owner peer_auth.owner; autoOptIn is
	// update.auto set true explicitly, which a peer_auth system daemon needs
	// to update itself.
	peerAuth  bool
	owner     string
	autoOptIn bool
}

// installConfigOf is the probe's view of cfg, loaded from path ("" for none).
func installConfigOf(cfg config.Config, path string) installConfig {
	return installConfig{
		path: path, client: cfg.IsClient(),
		peerAuth: cfg.PeerAuth.Enabled, owner: cfg.PeerAuth.Owner,
		autoOptIn: cfg.Update.Auto != nil && *cfg.Update.Auto,
	}
}

// probeInstall gathers the probe for this process, which loaded the config ic
// describes.
func probeInstall(distribution, ver string, ic installConfig) update.Probe {
	return probeInstallWith(realInstallProbeEnv(), distribution, ver, ic)
}

// probeInstallWith is probeInstall over an injected environment. A build that
// is not a release stops after the build facts: Detect decides from those
// alone, and a developer's build has no business being probed (the probe
// writes a scratch file to test the executable's directory). With peer_auth
// on, the system service is probed instead of the installer's.
func probeInstallWith(env installProbeEnv, distribution, ver string, ic installConfig) update.Probe {
	p := update.Probe{
		Distribution: distribution,
		Version:      ver,
		Client:       ic.client,
		Root:         env.euid == 0,
		Exe:          env.exe,
		ConfigPath:   ic.path,
		PeerAuth:     ic.peerAuth,
		AutoOptIn:    ic.autoOptIn,
	}
	if !update.IsReleaseBuild(distribution, ver) || ic.client {
		return p
	}
	if p.Exe != "" {
		p.ExeDirWritable = env.writable(filepath.Dir(p.Exe))
	}
	if ic.peerAuth {
		p.System = probeSystem(env, ic.owner)
		return p
	}
	if ic.path != "" && env.home != "" {
		installerConfig := filepath.Join(env.home, ".config", "seamless", "seamless.yaml")
		p.ConfigIsInstallers = env.sameFile(ic.path, installerConfig)
	}
	p.ForeignEnv = foreignSeamlessEnv(env.goos, env.environ)
	p.Service = probeService(env)
	p.Supervised, p.SupervisedReason = probeSupervised(env)
	return p
}

// The peer_auth system service (docs-src/reference/service.md, "A system
// service for peer_auth"): a LaunchDaemon under the label the reference
// names, or under the label these docs gave before the rename; on Linux the
// system unit the peer_auth guide writes.
const (
	systemDaemonLabel       = launchdLabel + ".daemon"
	legacySystemDaemonLabel = legacyLaunchdLabel + ".daemon"
	systemDaemonPlistDir    = "/Library/LaunchDaemons"
	systemDaemonUnit        = "seamless-daemon.service"
	// lingerDir holds one empty file per account whose user manager runs
	// without a login session (loginctl enable-linger).
	lingerDir = "/var/lib/systemd/linger"
)

// systemUnitDirs are where systemd reads a system unit's file, the
// administrator's directory first.
var systemUnitDirs = []string{"/etc/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"}

// probeSystem gathers what Detect judges a peer_auth system daemon by: its
// account, its system service, whether this process is that service's own
// instance, and on Linux whether its account's user manager lingers. owner
// is peer_auth.owner, whose account the daemon must not be.
func probeSystem(env installProbeEnv, owner string) update.SystemProbe {
	var sp update.SystemProbe
	sp.Account, sp.AccountReason = probeDaemonAccount(env, owner)
	sp.Service, sp.Restarts = probeSystemService(env)
	sp.Supervised, sp.SupervisedReason = probeSystemSupervised(env)
	if env.goos == "linux" {
		sp.ForeignEnv = foreignSeamlessEnv(env.goos, env.environ)
		sp.UserManagerProblem = probeLinger(env)
	}
	return sp
}

// probeDaemonAccount reports whether this process runs as the account
// peer_auth names as the daemon's -- the rule serve, seam approve and the
// Touch ID approver hold it to (access.DaemonUID): neither root nor the
// owner, and the pinned account, or with no pin a system account.
func probeDaemonAccount(env installProbeEnv, owner string) (bool, string) {
	if env.goos != "darwin" && env.goos != "linux" {
		return false, "peer_auth runs on macOS and Linux only"
	}
	if env.euid == 0 {
		return false, "running as root"
	}
	if owner != "" && env.resolve != nil {
		if a, err := env.resolve(owner); err == nil && int(a.UID) == env.euid {
			return false, "uid " + strconv.Itoa(env.euid) + " is the owner's account, " + a.Name
		}
	}
	if env.daemonPin == nil || env.systemAccount == nil {
		return false, "the daemon's account could not be checked"
	}
	pin, pinned, err := env.daemonPin()
	if err != nil {
		return false, err.Error()
	}
	if ok, rule := access.DaemonUID(uint32(env.euid), pin, pinned, env.systemAccount); !ok {
		return false, "uid " + strconv.Itoa(env.euid) + ": " + rule
	}
	return true, ""
}

// probeSystemService reads the system service definition and checks it runs
// this executable: the LaunchDaemon plist (the current label's, else the
// legacy one's) or the system unit (the first of systemUnitDirs that has it).
// restarts reports whether it starts the daemon again after a clean exit
// (plistKeepsAlive, unitRestartsOnCleanExit). A unit's drop-ins are not read.
func probeSystemService(env installProbeEnv) (sp update.ServiceProbe, restarts bool) {
	var paths []string
	switch env.goos {
	case "darwin":
		for _, label := range []string{systemDaemonLabel, legacySystemDaemonLabel} {
			paths = append(paths, filepath.Join(systemDaemonPlistDir, label+".plist"))
		}
	case "linux":
		for _, dir := range systemUnitDirs {
			paths = append(paths, filepath.Join(dir, systemDaemonUnit))
		}
	default:
		return update.ServiceProbe{Path: "a system service", Err: "peer_auth runs on macOS and Linux only"}, false
	}
	sp.Path = paths[0]
	var data []byte
	for _, path := range paths {
		raw, err := env.readFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		sp.Path = path
		if err != nil {
			return absentOrErr(sp, err), false
		}
		data = raw
		break
	}
	if data == nil {
		return sp, false
	}
	program := unitExecStart(data)
	restarts = unitRestartsOnCleanExit(data)
	if env.goos == "darwin" {
		program, restarts = plistProgram(data), plistKeepsAlive(data)
	}
	sp.Found = true
	sp.Program = program
	sp.RunsThisExe = program != "" && env.exe != "" && env.sameFile(program, env.exe)
	return sp, restarts
}

// unitRestartsOnCleanExit reports whether a unit's last Restart= starts the
// service again after it exits cleanly: always or on-success. systemd's
// default, no, and on-failure do not, and serve exits 0 on SIGTERM.
func unitRestartsOnCleanExit(data []byte) bool {
	restart := "no"
	for line := range strings.SplitSeq(string(data), "\n") {
		if val, ok := strings.CutPrefix(strings.TrimSpace(line), "Restart="); ok {
			restart = strings.TrimSpace(val)
		}
	}
	return restart == "always" || restart == "on-success"
}

// plistKeepsAlive reports whether a launchd plist's KeepAlive starts the job
// again after it exits cleanly: <true/>, or a dictionary whose SuccessfulExit
// is <true/>. Absent, <false/>, or any other condition does not.
func plistKeepsAlive(data []byte) bool {
	dec := xml.NewDecoder(bytes.NewReader(data))
	depth, lastKey := 0, ""
	keepAliveDepth := -1 // the depth of KeepAlive's dictionary, once entered
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch t.Name.Local {
			case "key":
				var k string
				if err := dec.DecodeElement(&k, &t); err != nil {
					return false
				}
				depth--
				lastKey = strings.TrimSpace(k)
				continue
			case "true":
				if lastKey == "KeepAlive" && keepAliveDepth < 0 {
					return true
				}
				if lastKey == "SuccessfulExit" && keepAliveDepth >= 0 && depth == keepAliveDepth+1 {
					return true
				}
			case "dict":
				if lastKey == "KeepAlive" && keepAliveDepth < 0 {
					keepAliveDepth = depth
				}
			}
			lastKey = ""
		case xml.EndElement:
			if t.Name.Local == "dict" && depth == keepAliveDepth {
				return false // KeepAlive's dictionary held no SuccessfulExit true
			}
			depth--
		}
	}
}

// probeSystemSupervised reports whether this process is the system service's
// own instance: launchd names the LaunchDaemon's job in XPC_SERVICE_NAME, as
// it does a LaunchAgent's, and is its parent; systemd places the system
// unit's processes in its cgroup, outside any user manager, and sets
// INVOCATION_ID.
func probeSystemSupervised(env installProbeEnv) (bool, string) {
	switch env.goos {
	case "darwin":
		if job := envValue(env.environ, "XPC_SERVICE_NAME"); job != systemDaemonLabel && job != legacySystemDaemonLabel {
			return false, "not running as the launchd job " + systemDaemonLabel
		}
		if env.ppid != 1 {
			return false, "the launchd job's process is not a child of launchd"
		}
		return true, ""
	case "linux":
		cg, err := env.readFile("/proc/self/cgroup")
		if err != nil {
			return false, "could not read /proc/self/cgroup"
		}
		if !inSystemUnit(cg, systemDaemonUnit) {
			return false, "not running in the " + systemDaemonUnit + " system unit"
		}
		if envValue(env.environ, "INVOCATION_ID") == "" {
			return false, "not started by systemd (no INVOCATION_ID)"
		}
		return true, ""
	default:
		return false, "peer_auth runs on macOS and Linux only"
	}
}

// probeLinger says why this account's systemd user manager cannot host the
// updater, "" when it lingers. A system account has no login sessions, so its
// manager runs only when lingering is on, and without it `systemd-run --user`
// has nothing to talk to.
func probeLinger(env installProbeEnv) string {
	uid := strconv.Itoa(env.euid)
	if env.resolve == nil {
		return "could not name uid " + uid + " to check that its systemd user manager lingers"
	}
	a, err := env.resolve(uid)
	if err != nil || a.Name == "" || !access.ValidDaemonAccountName(a.Name) {
		return "could not name uid " + uid + " to check that its systemd user manager lingers"
	}
	if _, err := env.readFile(filepath.Join(lingerDir, a.Name)); err != nil {
		return "the systemd user manager of " + a.Name + " does not linger, and the updater runs as a unit of it: " +
			"sudo loginctl enable-linger " + a.Name
	}
	return ""
}

// inSystemUnit reports whether a /proc/self/cgroup listing places the process
// in the system unit unit (or below it): the cgroup v2 line, or a cgroup v1
// host's name=systemd line, names the unit with no user manager
// (user@<uid>.service) above it. A user unit of the same name is not it.
func inSystemUnit(cgroup []byte, unit string) bool {
	for line := range strings.SplitSeq(string(cgroup), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 || (parts[1] != "" && parts[1] != "name=systemd") {
			continue
		}
		for seg := range strings.SplitSeq(parts[2], "/") {
			if isUserManager(seg) {
				break
			}
			if seg == unit {
				return true
			}
		}
	}
	return false
}

// foreignSeamlessEnv lists the SEAMLESS_* variables in environ other than
// SEAMLESS_CONFIG, sorted. Windows environment names are case-insensitive.
func foreignSeamlessEnv(goos string, environ []string) []string {
	var out []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		name := k
		if goos == "windows" {
			name = strings.ToUpper(k)
		}
		if strings.HasPrefix(name, "SEAMLESS_") && name != "SEAMLESS_CONFIG" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// envValue is os.Getenv over an injected environ (last value wins, as in the
// real environment).
func envValue(environ []string, key string) string {
	val := ""
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			val = v
		}
	}
	return val
}

// probeService reads the service definition the installer writes on this OS
// and checks it is the installer's and runs this executable.
func probeService(env installProbeEnv) update.ServiceProbe {
	var (
		sp      update.ServiceProbe
		data    []byte
		program string
	)
	switch env.goos {
	case "darwin":
		// The current label's plist, else the legacy one's: a Mac the
		// installer has not run on since the rename still has only that.
		sp.Path = launchdPlist(env.home, launchdLabel)
		var err error
		data, err = env.readFile(sp.Path)
		if errors.Is(err, fs.ErrNotExist) {
			legacy := launchdPlist(env.home, legacyLaunchdLabel)
			if ldata, lerr := env.readFile(legacy); !errors.Is(lerr, fs.ErrNotExist) {
				sp.Path, data, err = legacy, ldata, lerr
			}
		}
		if err != nil {
			return absentOrErr(sp, err)
		}
		program = plistProgram(data)
	case "windows":
		sp.Path = "Scheduled Task " + scheduledTask
		raw, err := env.taskXML()
		if err != nil {
			return sp // schtasks fails for a missing task; nothing to read
		}
		def, err := parseTaskXML(raw)
		if err != nil {
			sp.Err = err.Error()
			return sp
		}
		sp.Found = true
		sp.Marker = strings.Contains(def.Description, update.InstallerMarker)
		sp.Program = def.Command
		sp.RunsThisExe = def.Command != "" && env.exe != "" && env.sameFile(def.Command, env.exe)
		return sp
	default: // linux and other unixes run the systemd --user unit
		sp.Path = filepath.Join(env.home, ".config", "systemd", "user", systemdUnit)
		var err error
		if data, err = env.readFile(sp.Path); err != nil {
			return absentOrErr(sp, err)
		}
		program = unitExecStart(data)
	}
	sp.Found = true
	sp.Marker = bytes.Contains(data, []byte(update.InstallerMarker))
	sp.Program = program
	sp.RunsThisExe = program != "" && env.exe != "" && env.sameFile(program, env.exe)
	return sp
}

// absentOrErr records a failed definition read: a missing file is simply not
// found, anything else is an error worth naming.
func absentOrErr(sp update.ServiceProbe, err error) update.ServiceProbe {
	if !errors.Is(err, fs.ErrNotExist) {
		sp.Err = err.Error()
	}
	return sp
}

// taskSchedulerImage is the executable Task Scheduler runs in: a task's
// process is a child of the svchost.exe hosting the Schedule service.
const taskSchedulerImage = "svchost.exe"

// probeSupervised reports whether this process is the service's own instance,
// not a release binary someone started by hand next to it.
//
//   - darwin: launchd names the job in XPC_SERVICE_NAME (the current label
//     or the legacy one) and is the parent (ppid 1) of a job it spawned.
//   - linux: systemd places the unit's processes in its cgroup, under the
//     user's manager (inSystemdUnit), and sets INVOCATION_ID.
//   - windows: the parent is the Task Scheduler (svchost.exe) and the
//     Seamless task's principal is this user. A serve started by hand has a
//     shell or Explorer for its parent. Gate 5 has already tied the task to
//     this executable and the installer's marker.
func probeSupervised(env installProbeEnv) (bool, string) {
	switch env.goos {
	case "darwin":
		if job := envValue(env.environ, "XPC_SERVICE_NAME"); job != launchdLabel && job != legacyLaunchdLabel {
			return false, "not running as the launchd job " + launchdLabel
		}
		if env.ppid != 1 {
			return false, "the launchd job's process is not a child of launchd"
		}
		return true, ""
	case "windows":
		return windowsSupervised(env)
	default:
		cg, err := env.readFile("/proc/self/cgroup")
		if err != nil {
			return false, "could not read /proc/self/cgroup"
		}
		if !inSystemdUnit(cg, systemdUnit) {
			return false, "not running in the " + systemdUnit + " unit"
		}
		if envValue(env.environ, "INVOCATION_ID") == "" {
			return false, "not started by systemd (no INVOCATION_ID)"
		}
		return true, ""
	}
}

// windowsSupervised is gate 9 on Windows: this process is a child of the Task
// Scheduler, and the Seamless task runs as this user.
func windowsSupervised(env installProbeEnv) (bool, string) {
	parent, err := env.parentImage()
	if err != nil {
		return false, "could not identify the parent process: " + err.Error()
	}
	if !strings.EqualFold(parent, taskSchedulerImage) {
		return false, "the parent process is " + parent + ", not the Task Scheduler (" + taskSchedulerImage + ")"
	}
	raw, err := env.taskXML()
	if err != nil {
		return false, "the Scheduled Task " + scheduledTask + " could not be read"
	}
	def, err := parseTaskXML(raw)
	if err != nil {
		return false, "the Scheduled Task " + scheduledTask + " could not be parsed: " + err.Error()
	}
	if def.UserID == "" {
		return false, "the Scheduled Task " + scheduledTask + " names no user"
	}
	mine, err := env.isCurrentUser(def.UserID)
	switch {
	case err != nil:
		return false, "could not tell whether the Scheduled Task " + scheduledTask + "'s user " + def.UserID + " is this user: " + err.Error()
	case !mine:
		return false, "the Scheduled Task " + scheduledTask + " runs as " + def.UserID + ", not this user"
	}
	return true, ""
}

// inSystemdUnit reports whether a /proc/self/cgroup listing places the
// process in unit under a user's service manager: the cgroup v2 line
// ("0::/user.slice/.../user@1000.service/app.slice/seamless.service") or, on
// a cgroup v1 host, the name=systemd hierarchy's line, which a legacy host
// has instead of a "0::" line. A sub-cgroup of the unit is in it too -- the
// unit's stop kills it -- and a system unit of the same name is not the
// installer's.
func inSystemdUnit(cgroup []byte, unit string) bool {
	for line := range strings.SplitSeq(string(cgroup), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[1] != "" && parts[1] != "name=systemd" {
			continue // a v1 controller hierarchy other than systemd's
		}
		if inUserUnit(parts[2], unit) {
			return true
		}
	}
	return false
}

// inUserUnit reports whether a cgroup path lies in unit, or below it, under a
// user manager's own cgroup (user@<uid>.service).
func inUserUnit(path, unit string) bool {
	manager := false
	for seg := range strings.SplitSeq(path, "/") {
		switch {
		case !manager:
			manager = isUserManager(seg)
		case seg == unit:
			return true
		}
	}
	return false
}

// isUserManager reports whether a cgroup path segment is a user manager's
// unit, user@<uid>.service.
func isUserManager(seg string) bool {
	uid, ok := strings.CutPrefix(seg, "user@")
	if !ok {
		return false
	}
	uid, ok = strings.CutSuffix(uid, ".service")
	return ok && uid != "" && strings.Trim(uid, "0123456789") == ""
}

// plistProgram returns ProgramArguments[0] from a launchd plist, or "" when
// the plist has none or does not parse.
func plistProgram(data []byte) string {
	dec := xml.NewDecoder(bytes.NewReader(data))
	lastKey, inArgs := "", false
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "key":
				var k string
				if err := dec.DecodeElement(&k, &t); err != nil {
					return ""
				}
				lastKey = strings.TrimSpace(k)
				continue
			case "array":
				inArgs = lastKey == "ProgramArguments"
			case "string":
				if inArgs {
					var v string
					if err := dec.DecodeElement(&v, &t); err != nil {
						return ""
					}
					return strings.TrimSpace(v)
				}
			}
			lastKey = ""
		case xml.EndElement:
			if t.Name.Local == "array" && inArgs {
				return "" // an empty ProgramArguments
			}
		}
	}
}

// unitExecStart returns the executable of a systemd unit's ExecStart=, or ""
// when there is none. systemd's executable prefixes (-, @, :, +, !) are
// dropped and a quoted path is unquoted.
func unitExecStart(data []byte) string {
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		val, ok := strings.CutPrefix(line, "ExecStart=")
		if !ok {
			continue
		}
		val = strings.TrimLeft(strings.TrimSpace(val), "-@:+!")
		if val == "" {
			return ""
		}
		if q := val[0]; q == '"' || q == '\'' {
			if end := strings.IndexByte(val[1:], q); end >= 0 {
				return val[1 : end+1]
			}
			return ""
		}
		prog, _, _ := strings.Cut(val, " ")
		prog, _, _ = strings.Cut(prog, "\t")
		return prog
	}
	return ""
}

// taskDefinition is the sliver of a Task Scheduler XML definition the probe
// reads.
type taskDefinition struct {
	Description string `xml:"RegistrationInfo>Description"`
	Command     string `xml:"Actions>Exec>Command"`
	UserID      string `xml:"Principals>Principal>UserId"`
}

// parseTaskXML decodes `schtasks /Query /XML` output. schtasks writes UTF-16
// with a byte-order mark and declares encoding="UTF-16"; the decoder only
// speaks UTF-8, so the bytes are converted first and the declaration is then
// accepted as already handled.
func parseTaskXML(raw []byte) (taskDefinition, error) {
	text := decodeUTF16(raw)
	dec := xml.NewDecoder(strings.NewReader(text))
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	var def taskDefinition
	if err := dec.Decode(&def); err != nil {
		return taskDefinition{}, err
	}
	def.Command = strings.Trim(strings.TrimSpace(def.Command), `"`)
	def.Description = strings.TrimSpace(def.Description)
	def.UserID = strings.TrimSpace(def.UserID)
	return def, nil
}

// decodeUTF16 turns BOM-marked UTF-16 into a Go string; bytes without a
// UTF-16 BOM are taken as UTF-8.
func decodeUTF16(b []byte) string {
	var order binary.ByteOrder
	switch {
	case len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE:
		order = binary.LittleEndian
	case len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF:
		order = binary.BigEndian
	default:
		return strings.TrimPrefix(string(b), "\uFEFF")
	}
	b = b[2:]
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, order.Uint16(b[i:]))
	}
	return string(utf16.Decode(u))
}

// sameFile reports whether a and b name the same existing file, the way the
// filesystem decides it (case-insensitive volumes, hard links, symlinks).
func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// dirWritable reports whether dir accepts a new file, by creating and removing
// one: permission bits do not tell the whole story (ACLs, read-only mounts,
// Windows), and replacing a binary in place needs exactly this.
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".seamless-write-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return false
	}
	return os.Remove(name) == nil
}
