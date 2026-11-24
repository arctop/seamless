package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
)

// The installer's Windows fallback. When the Scheduled Task cannot be
// registered (group policy, an older release's task of another user's, a
// session that cannot reach the Task Scheduler), install.ps1 puts a shortcut
// in the user's Startup folder, Seamless.lnk, which starts the daemon at
// logon through a hidden PowerShell, and starts the daemon directly
// (Register-ServiceFallback). The service verbs and uninstall find it here, so
// they control and remove it as they do the task, instead of calling such an
// install "not installed" and leaving the shortcut and its daemon behind.

// windowsService is what runs this user's Seamless daemon on Windows, as
// resolveWindowsService found it.
type windowsService struct {
	// tasks are this user's Seamless tasks (taskLookup.own), the per-user
	// one first; perUser is the per-user task's name, for a PC with none.
	tasks   []string
	perUser string
	// shortcut is the path of the Startup shortcut when it runs this
	// seamlessd.exe's serve, "" otherwise; config and logFile are the serve
	// flags it runs with.
	shortcut, config, logFile string
	// pid is this install's running daemon: the holder of the data dir's
	// lock, when that process runs this seamlessd.exe. 0 for none.
	pid int
	// exe is this seamlessd.exe and powershell Windows PowerShell, both by
	// absolute path.
	exe, powershell string
}

// task is the task the service verbs act on: this user's, the per-user one
// first; "" for none.
func (w windowsService) task() string {
	if len(w.tasks) > 0 {
		return w.tasks[0]
	}
	return ""
}

// startCmd starts the shortcut's daemon the way the installer's fallback
// starts it: hidden, in a console of its own, detached from this command,
// with the shortcut's config and log file.
func (w windowsService) startCmd() *exec.Cmd {
	args := fmt.Sprintf(`serve --config "%s" --log-file "%s"`, w.config, w.logFile)
	script := "Start-Process -WindowStyle Hidden -FilePath " + psQuote(w.exe) + " -ArgumentList " + psQuote(args)
	return exec.Command(w.powershell, "-NoProfile", "-NonInteractive", "-Command", script)
}

// killCmd ends the running daemon: the hard stop schtasks /End gives the
// task's.
func (w windowsService) killCmd() *exec.Cmd {
	return exec.Command("taskkill", "/F", "/PID", strconv.Itoa(w.pid))
}

// psQuote quotes s as a PowerShell single-quoted string.
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// windowsServiceControl is serviceControl on Windows: this user's task when
// there is one, else the Startup shortcut when it is this install's, else the
// per-user task's name, which reads as not installed.
func windowsServiceControl(action serviceAction, w windowsService) serviceControlPlan {
	task := w.task()
	if task == "" && w.shortcut != "" {
		return shortcutControl(action, w)
	}
	shown := task
	if shown == "" {
		shown = w.perUser
	}
	if shown == "" {
		// This user's SID could not be read: the query below then fails,
		// which reads as not installed.
		shown = serviceTaskPrefix + "<your SID>"
	}
	p := serviceControlPlan{
		Label:       "Scheduled Task (" + shown + ")",
		ProbeCmd:    exec.Command("schtasks", "/Query", "/TN", task),
		InstallHint: "run the installer: irm https://thereisnospoon.org/install.ps1 | iex",
	}
	if task != "" && w.shortcut != "" {
		p.Note = "a Startup shortcut from an earlier install is there too (" + w.shortcut +
			"); the task is the service, and the next install removes the shortcut"
	}
	switch action {
	case actionStart:
		p.Cmds = []*exec.Cmd{exec.Command("schtasks", "/Run", "/TN", task)}
	case actionStop:
		p.Cmds = []*exec.Cmd{exec.Command("schtasks", "/End", "/TN", task)}
		p.SettleAfter = 1
	case actionRestart:
		p.Cmds = []*exec.Cmd{
			exec.Command("schtasks", "/End", "/TN", task),
			exec.Command("schtasks", "/Run", "/TN", task),
		}
		p.SettleAfter = 1
	case actionStatus:
		p.Cmds = []*exec.Cmd{exec.Command("schtasks", "/Query", "/TN", task, "/V", "/FO", "LIST")}
	}
	return p
}

// shortcutControl acts on the Startup shortcut's daemon. Stopping it ends the
// process that holds the data dir; nothing restarts it before the next logon,
// as with a stopped task. A start while it runs, or a stop while it does not,
// has nothing to do and says so.
func shortcutControl(action serviceAction, w windowsService) serviceControlPlan {
	p := serviceControlPlan{
		Label:       "Startup shortcut (" + w.shortcut + ")",
		DefFile:     w.shortcut,
		InstallHint: "run the installer: irm https://thereisnospoon.org/install.ps1 | iex",
	}
	running := "running (pid " + strconv.Itoa(w.pid) + ")"
	switch action {
	case actionStart:
		if w.pid != 0 {
			p.Note = "already " + running
			return p
		}
		p.Cmds = []*exec.Cmd{w.startCmd()}
	case actionStop:
		if w.pid == 0 {
			p.Note = "not running"
			return p
		}
		p.Cmds = []*exec.Cmd{w.killCmd()}
		p.SettleAfter = 1
	case actionRestart:
		if w.pid != 0 {
			p.Cmds = []*exec.Cmd{w.killCmd()}
			p.SettleAfter = 1
		}
		p.Cmds = append(p.Cmds, w.startCmd())
	case actionStatus:
		state := "not running"
		if w.pid != 0 {
			state = running
		}
		p.Report = [][2]string{
			{"kind", p.Label},
			{"daemon", state},
			{"note", "the installer could not register the Scheduled Task, so the shortcut starts the daemon at logon; " +
				"this install does not update itself (re-run the installer to register the task)"},
		}
	}
	return p
}

// windowsTeardown is serviceTeardown's Windows plan: end and delete this
// user's tasks, stop this install's running daemon however it was started,
// delete the update task, and remove the Startup shortcut.
func windowsTeardown(w windowsService, installDir, updateTask string) serviceTeardownPlan {
	var stop []*exec.Cmd
	for _, task := range w.tasks {
		stop = append(stop,
			exec.Command("schtasks", "/End", "/TN", task),
			exec.Command("schtasks", "/Delete", "/TN", task, "/F"))
	}
	if w.pid != 0 {
		stop = append(stop, w.killCmd())
	}
	if updateTask != "" {
		stop = append(stop, exec.Command("schtasks", "/Delete", "/TN", updateTask, "/F"))
	}
	var kinds []string
	if len(w.tasks) > 0 {
		kinds = append(kinds, "Scheduled Task ("+strings.Join(w.tasks, ", ")+")")
	}
	var remove []string
	if w.shortcut != "" {
		kinds = append(kinds, "Startup shortcut")
		remove = append(remove, w.shortcut)
	}
	label := strings.Join(kinds, " + ")
	if label == "" {
		label = "Scheduled Task (none of yours registered)"
	}
	return serviceTeardownPlan{
		Label:       label,
		StopCmds:    stop,
		RemoveFiles: remove,
		PathEdits:   []*exec.Cmd{exec.Command("powershell", "-NoProfile", "-Command", windowsPathRemoveScript(installDir))},
	}
}

// fallbackServe matches a Startup shortcut's arguments to the command
// install.ps1's Register-ServiceFallback writes -- `& '<seamlessd.exe>' serve
// --config '<path>' --log-file '<path>'`, inside a hidden PowerShell's
// -Command -- and returns its config and log file when the executable is exe,
// compared without case as Windows compares paths.
func fallbackServe(arguments, exe string) (config, logFile string, ok bool) {
	m := fallbackServeRe.FindStringSubmatch(arguments)
	if m == nil || exe == "" || !strings.EqualFold(psUnquote(m[1]), exe) {
		return "", "", false
	}
	return psUnquote(m[2]), psUnquote(m[3]), true
}

var fallbackServeRe = regexp.MustCompile(`&\s+'((?:[^']|'')*)'\s+serve\s+--config\s+'((?:[^']|'')*)'\s+--log-file\s+'((?:[^']|'')*)'`)

// psUnquote undoes psQuote's doubling of the quotes inside.
func psUnquote(s string) string { return strings.ReplaceAll(s, "''", "'") }

// Shell link flags ([MS-SHLLINK] 2.1.1) that decide what follows the header.
const (
	linkHasTargetIDList = 1 << 0
	linkHasLinkInfo     = 1 << 1
	linkHasName         = 1 << 2
	linkHasRelativePath = 1 << 3
	linkHasWorkingDir   = 1 << 4
	linkHasArguments    = 1 << 5
	linkIsUnicode       = 1 << 7
)

// shellLinkHeaderSize is a shell link's fixed header ([MS-SHLLINK] 2.1).
const shellLinkHeaderSize = 0x4C

var errShellLink = errors.New("not a shell link")

// shellLinkArguments reads a .lnk file's command line arguments: past the
// header, the item ID list and the link info come the string data, whose
// fourth string is the arguments, each present only when its flag says so
// ([MS-SHLLINK] 2.4). "" for a shortcut without them.
func shellLinkArguments(raw []byte) (string, error) {
	if len(raw) < shellLinkHeaderSize || binary.LittleEndian.Uint32(raw) != shellLinkHeaderSize {
		return "", errShellLink
	}
	flags := binary.LittleEndian.Uint32(raw[20:])
	off := shellLinkHeaderSize
	if flags&linkHasTargetIDList != 0 {
		if off+2 > len(raw) {
			return "", errShellLink
		}
		off += 2 + int(binary.LittleEndian.Uint16(raw[off:]))
	}
	if flags&linkHasLinkInfo != 0 {
		if off+4 > len(raw) {
			return "", errShellLink
		}
		off += int(binary.LittleEndian.Uint32(raw[off:]))
	}
	unit := 1
	if flags&linkIsUnicode != 0 {
		unit = 2
	}
	for _, f := range []uint32{linkHasName, linkHasRelativePath, linkHasWorkingDir, linkHasArguments} {
		if flags&f == 0 {
			continue
		}
		if off+2 > len(raw) {
			return "", errShellLink
		}
		n := int(binary.LittleEndian.Uint16(raw[off:]))
		off += 2
		end := off + n*unit
		if end > len(raw) {
			return "", errShellLink
		}
		if f == linkHasArguments {
			return decodeLinkString(raw[off:end], unit), nil
		}
		off = end
	}
	return "", nil
}

// decodeLinkString decodes a shell link string: UTF-16 little endian, or the
// system code page, read here as Latin-1, when the link is not Unicode.
func decodeLinkString(b []byte, unit int) string {
	if unit == 1 {
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c)
		}
		return string(r)
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}
