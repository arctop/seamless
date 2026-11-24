package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/require"
)

const (
	janeExe    = `C:\Users\Jane O'Doe\.local\bin\seamlessd.exe`
	janeConfig = `C:\Users\Jane O'Doe\.config\seamless\seamless.yaml`
	janeLog    = `C:\Users\Jane O'Doe\.seamless\seamlessd.log`
)

// TestShellLinkArguments_TheInstallersShortcut reads a shortcut Windows
// wrote with install.ps1's own Register-ServiceFallback code (WScript.Shell,
// in a Windows 11 VM), for a profile path with a space and a quote. Its
// optional extra data blocks, which carry the machine's name and the user's
// SID, were cut after the strings and the terminal block put back.
func TestShellLinkArguments_TheInstallersShortcut(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "fallback.lnk"))
	require.NoError(t, err)
	args, err := shellLinkArguments(raw)
	require.NoError(t, err)
	require.Equal(t, `-NoProfile -WindowStyle Hidden -Command "& 'C:\Users\Jane O''Doe\.local\bin\seamlessd.exe' serve `+
		`--config 'C:\Users\Jane O''Doe\.config\seamless\seamless.yaml' --log-file 'C:\Users\Jane O''Doe\.seamless\seamlessd.log'"`, args)

	cfg, logFile, ok := fallbackServe(args, janeExe)
	require.True(t, ok)
	require.Equal(t, janeConfig, cfg)
	require.Equal(t, janeLog, logFile)
}

// linkString is a shell link string: its count of UTF-16 units, then them.
func linkString(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := binary.LittleEndian.AppendUint16(nil, uint16(len(u)))
	for _, c := range u {
		b = binary.LittleEndian.AppendUint16(b, c)
	}
	return b
}

// shellLink builds a minimal Unicode shell link with the given strings, each
// present when non-empty.
func shellLink(name, workingDir, arguments string) []byte {
	flags := uint32(linkIsUnicode)
	var data []byte
	if name != "" {
		flags |= linkHasName
		data = append(data, linkString(name)...)
	}
	if workingDir != "" {
		flags |= linkHasWorkingDir
		data = append(data, linkString(workingDir)...)
	}
	if arguments != "" {
		flags |= linkHasArguments
		data = append(data, linkString(arguments)...)
	}
	header := make([]byte, shellLinkHeaderSize)
	binary.LittleEndian.PutUint32(header, shellLinkHeaderSize)
	binary.LittleEndian.PutUint32(header[20:], flags)
	return append(append(header, data...), 0, 0, 0, 0)
}

func TestShellLinkArguments(t *testing.T) {
	args, err := shellLinkArguments(shellLink("d", `C:\w`, "-x 'y'"))
	require.NoError(t, err)
	require.Equal(t, "-x 'y'", args)

	args, err = shellLinkArguments(shellLink("d", `C:\w`, ""))
	require.NoError(t, err)
	require.Empty(t, args, "a shortcut without arguments")

	for name, raw := range map[string][]byte{
		"empty":          nil,
		"not a link":     []byte("MZ\x90\x00 this is an exe, not a shortcut, and it is long enough to pass the size check......"),
		"cut in a count": shellLink("d", "", "args")[:shellLinkHeaderSize+1],
		"cut in a string": func() []byte {
			b := shellLink("", "", "a long argument string")
			return b[:len(b)-10]
		}(),
	} {
		_, err := shellLinkArguments(raw)
		require.ErrorIs(t, err, errShellLink, name)
	}
}

func TestFallbackServe(t *testing.T) {
	args := `-NoProfile -WindowStyle Hidden -Command "& 'C:\Users\me\.local\bin\seamlessd.exe' serve --config 'C:\Users\me\.config\seamless\seamless.yaml' --log-file 'C:\Users\me\.seamless\seamlessd.log'"`
	cfg, logFile, ok := fallbackServe(args, `c:\users\ME\.local\bin\seamlessd.exe`)
	require.True(t, ok, "Windows compares paths without case")
	require.Equal(t, `C:\Users\me\.config\seamless\seamless.yaml`, cfg)
	require.Equal(t, `C:\Users\me\.seamless\seamlessd.log`, logFile)

	_, _, ok = fallbackServe(args, `D:\other\seamlessd.exe`)
	require.False(t, ok, "another install's shortcut is not this one's")
	_, _, ok = fallbackServe(args, "")
	require.False(t, ok)
	_, _, ok = fallbackServe(`-NoProfile -Command "& 'C:\x\seamlessd.exe' doctor"`, `C:\x\seamlessd.exe`)
	require.False(t, ok, "only the serve the installer writes")
}

// shortcutWin is a PC on the installer's fallback: no task of this user's,
// the shortcut, and the daemon running as pid unless 0.
func shortcutWin(pid int) windowsService {
	return windowsService{
		perUser: testTask, shortcut: `C:\Users\me\Startup\Seamless.lnk`, config: `C:\c.yaml`, logFile: `C:\l.log`,
		pid: pid, exe: `C:\Users\me\.local\bin\seamlessd.exe`, powershell: `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
	}
}

func TestWindowsServiceControl_Shortcut(t *testing.T) {
	start := []string{`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, "-NoProfile", "-NonInteractive", "-Command",
		`Start-Process -WindowStyle Hidden -FilePath 'C:\Users\me\.local\bin\seamlessd.exe' -ArgumentList 'serve --config "C:\c.yaml" --log-file "C:\l.log"'`}
	kill := []string{"taskkill", "/F", "/PID", "4242"}
	tests := []struct {
		name       string
		action     serviceAction
		pid        int
		wantCmds   [][]string
		wantSettle int
		wantNote   string
	}{
		{"start while stopped: hidden, as the installer starts it", actionStart, 0, [][]string{start}, 0, ""},
		{"start while running: nothing to do", actionStart, 4242, nil, 0, "already running (pid 4242)"},
		{"stop while running: this install's daemon, then wait", actionStop, 4242, [][]string{kill}, 1, ""},
		{"stop while stopped: nothing to do", actionStop, 0, nil, 0, "not running"},
		{"restart while running", actionRestart, 4242, [][]string{kill, start}, 1, ""},
		{"restart while stopped is a start", actionRestart, 0, [][]string{start}, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := serviceControl(tt.action, "windows", `C:\Users\me`, -1, shortcutWin(tt.pid))
			require.Equal(t, `Startup shortcut (C:\Users\me\Startup\Seamless.lnk)`, p.Label)
			require.Equal(t, `C:\Users\me\Startup\Seamless.lnk`, p.DefFile, "installed while the shortcut is there")
			require.Equal(t, tt.wantCmds, argvs(p.Cmds))
			require.Equal(t, tt.wantSettle, p.SettleAfter)
			require.Equal(t, tt.wantNote, p.Note)
		})
	}

	status := serviceControl(actionStatus, "windows", `C:\Users\me`, -1, shortcutWin(4242))
	require.Nil(t, status.Cmds)
	require.Equal(t, [2]string{"daemon", "running (pid 4242)"}, status.Report[1])
	require.Contains(t, status.Report[2][1], "does not update itself")
	require.Equal(t, [2]string{"daemon", "not running"}, serviceControl(actionStatus, "windows", `C:\Users\me`, -1, shortcutWin(0)).Report[1])
}

// A task wins over a shortcut an earlier install left, and says so; with
// neither, the plan names the per-user task, whose query fails: not installed.
func TestWindowsServiceControl_TaskFirst(t *testing.T) {
	both := shortcutWin(4242)
	both.tasks = []string{testTask}
	p := serviceControl(actionStop, "windows", `C:\Users\me`, -1, both)
	require.Equal(t, "Scheduled Task ("+testTask+")", p.Label)
	require.Equal(t, [][]string{{"schtasks", "/End", "/TN", testTask}}, argvs(p.Cmds))
	require.Contains(t, p.Note, "the task is the service")

	none := serviceControl(actionStart, "windows", `C:\Users\me`, -1, windowsService{perUser: testTask})
	require.Equal(t, "Scheduled Task ("+testTask+")", none.Label)
	require.Equal(t, []string{"schtasks", "/Query", "/TN", ""}, none.ProbeCmd.Args, "no task of this user's to query")
	require.Empty(t, none.Note)

	noSID := serviceControl(actionStart, "windows", `C:\Users\me`, -1, windowsService{})
	require.Equal(t, "Scheduled Task (Seamless-<your SID>)", noSID.Label)
}

func TestWindowsTeardown(t *testing.T) {
	const updateTask = "SeamlessUpdate-S-1-5-21-1-2-3-1001"
	both := shortcutWin(4242)
	both.tasks = []string{testTask, "Seamless"}
	p := serviceTeardown("windows", `C:\Users\me`, -1, `C:\Users\me\.local\bin`, updateTask, both)
	require.Equal(t, "Scheduled Task ("+testTask+", Seamless) + Startup shortcut", p.Label)
	require.Equal(t, [][]string{
		{"schtasks", "/End", "/TN", testTask},
		{"schtasks", "/Delete", "/TN", testTask, "/F"},
		{"schtasks", "/End", "/TN", "Seamless"},
		{"schtasks", "/Delete", "/TN", "Seamless", "/F"},
		{"taskkill", "/F", "/PID", "4242"},
		{"schtasks", "/Delete", "/TN", updateTask, "/F"},
	}, argvs(p.StopCmds), "the daemon stops however it was started")
	require.Equal(t, []string{`C:\Users\me\Startup\Seamless.lnk`}, p.RemoveFiles)

	fallback := serviceTeardown("windows", `C:\Users\me`, -1, `C:\Users\me\.local\bin`, "", shortcutWin(0))
	require.Equal(t, "Startup shortcut", fallback.Label)
	require.Nil(t, fallback.StopCmds, "nothing running, no task: nothing to stop")
	require.Equal(t, []string{`C:\Users\me\Startup\Seamless.lnk`}, fallback.RemoveFiles)
}
