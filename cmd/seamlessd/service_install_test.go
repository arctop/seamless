package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/update"
)

// serviceTaskDoc is the Seamless task's definition as a test reads it back.
type serviceTaskDoc struct {
	XMLName            xml.Name `xml:"http://schemas.microsoft.com/windows/2004/02/mit/task Task"`
	SecurityDescriptor string   `xml:"RegistrationInfo>SecurityDescriptor"`
	LogonTrigger       struct {
		Enabled string `xml:"Enabled"`
		UserID  string `xml:"UserId"`
	} `xml:"Triggers>LogonTrigger"`
	Principal struct {
		ID        string `xml:"id,attr"`
		UserID    string `xml:"UserId"`
		LogonType string `xml:"LogonType"`
		RunLevel  string `xml:"RunLevel"`
	} `xml:"Principals>Principal"`
	Settings struct {
		MultipleInstancesPolicy    string `xml:"MultipleInstancesPolicy"`
		DisallowStartIfOnBatteries string `xml:"DisallowStartIfOnBatteries"`
		StopIfGoingOnBatteries     string `xml:"StopIfGoingOnBatteries"`
		StartWhenAvailable         string `xml:"StartWhenAvailable"`
		AllowStartOnDemand         string `xml:"AllowStartOnDemand"`
		Enabled                    string `xml:"Enabled"`
		Hidden                     string `xml:"Hidden"`
		ExecutionTimeLimit         string `xml:"ExecutionTimeLimit"`
		RestartOnFailure           struct {
			Interval string `xml:"Interval"`
			Count    string `xml:"Count"`
		} `xml:"RestartOnFailure"`
	} `xml:"Settings"`
	Actions struct {
		Context   string `xml:"Context,attr"`
		Command   string `xml:"Exec>Command"`
		Arguments string `xml:"Exec>Arguments"`
	} `xml:"Actions"`
}

func decodeServiceTask(t *testing.T, def []byte) serviceTaskDoc {
	t.Helper()
	require.Equal(t, []byte{0xFF, 0xFE}, def[:2], "UTF-16 little endian behind a byte-order mark")
	require.Zero(t, len(def)%2)
	text := decodeUTF16(def)
	require.True(t, strings.HasPrefix(text, `<?xml version="1.0" encoding="UTF-16"?>`), text)
	dec := xml.NewDecoder(strings.NewReader(text))
	dec.CharsetReader = func(_ string, r io.Reader) (io.Reader, error) { return r, nil }
	var doc serviceTaskDoc
	require.NoError(t, dec.Decode(&doc))
	return doc
}

// TestServiceTaskPlan holds the definition to what install.ps1's
// Register-ScheduledTask call built, and to what the install probe reads back:
// Detect calls the install "installer" from the marker, the command and the
// principal (install_probe.go), so a task this verb wrote must read the same.
func TestServiceTaskPlan(t *testing.T) {
	const sid = "S-1-5-21-1-2-3-1001"
	tests := []struct {
		name, account, exe, arguments string
	}{
		{
			"the usual layout",
			`DESKTOP-1\me`,
			`C:\Users\me\.local\bin\seamlessd.exe`,
			`serve --config C:\Users\me\.config\seamless\seamless.yaml --log-file C:\Users\me\.seamless\seamlessd.log`,
		},
		{
			// taskArguments quotes a path with a space (syscall.EscapeArg); the
			// definition must carry the quotes through XML unchanged.
			"spaces in the paths",
			`AzureAD\Jane Doe`,
			`C:\Users\Jane Doe\.local\bin\seamlessd.exe`,
			`serve --config "C:\Users\Jane Doe\.config\seamless\seamless.yaml" --log-file "C:\Users\Jane Doe\.seamless\seamlessd.log"`,
		},
		{
			"characters XML escapes",
			`R&D\<me>`,
			`C:\Users\A&B <x>\seamlessd.exe`,
			`serve --config "C:\Users\A&B <x>\it's \"q\"\seamless.yaml" --log-file C:\l.log`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := newServiceTaskPlan(sid, tt.account, tt.exe, tt.arguments)
			require.NoError(t, err)
			require.Equal(t, scheduledTask, p.name)
			doc := decodeServiceTask(t, p.definition)

			require.Equal(t, "D:(A;;FA;;;"+sid+")", doc.SecurityDescriptor,
				"the user may replace the task even when an elevated PowerShell registered it")
			require.Equal(t, "true", doc.LogonTrigger.Enabled)
			require.Equal(t, tt.account, doc.LogonTrigger.UserID,
				"the trigger is this user's logon: an any-user logon trigger needs admin")
			require.Equal(t, sid, doc.Principal.UserID)
			require.Equal(t, "InteractiveToken", doc.Principal.LogonType, "no stored password, and a standard user is refused S4U")
			require.Equal(t, "LeastPrivilege", doc.Principal.RunLevel)
			require.Equal(t, doc.Principal.ID, doc.Actions.Context)
			require.Equal(t, "IgnoreNew", doc.Settings.MultipleInstancesPolicy)
			require.Equal(t, "false", doc.Settings.Hidden)
			require.Equal(t, "PT0S", doc.Settings.ExecutionTimeLimit, "a daemon has no time limit")
			require.Equal(t, "true", doc.Settings.AllowStartOnDemand, "schtasks /Run and seamlessd start need it")
			require.Equal(t, "true", doc.Settings.Enabled)
			require.Equal(t, "false", doc.Settings.DisallowStartIfOnBatteries)
			require.Equal(t, "false", doc.Settings.StopIfGoingOnBatteries)
			require.Equal(t, "true", doc.Settings.StartWhenAvailable)
			require.Equal(t, "PT1M", doc.Settings.RestartOnFailure.Interval)
			require.Equal(t, "3", doc.Settings.RestartOnFailure.Count)
			require.Equal(t, tt.exe, doc.Actions.Command)
			require.Equal(t, tt.arguments, doc.Actions.Arguments, "the argument string survives XML byte for byte")

			def, err := parseTaskXML(p.definition)
			require.NoError(t, err, "the probe's own task parser reads it")
			require.Contains(t, def.Description, update.InstallerMarker, "Detect must still call it the installer's")
			require.Equal(t, sid, def.UserID)
			require.Equal(t, tt.exe, def.Command)
		})
	}
}

// TestServiceTaskPlan_Commands pins the schtasks argv: CIM-free, so it works
// from a reduced token.
func TestServiceTaskPlan_Commands(t *testing.T) {
	p, err := newServiceTaskPlan("S-1-5-21-9", `H\me`, `C:\s.exe`, "serve")
	require.NoError(t, err)
	require.Equal(t, []string{"schtasks", "/Create", "/TN", "Seamless", "/XML", `C:\t\seamless-task-1.xml`, "/F"},
		p.create(`C:\t\seamless-task-1.xml`))
	require.Equal(t, []string{"schtasks", "/Run", "/TN", "Seamless"}, p.run())
	require.Equal(t, []string{"schtasks", "/Delete", "/TN", "Seamless", "/F"}, p.remove())
}

// TestServiceTaskDescription_IsTheInstallers: install.ps1 still registers the
// task through the cmdlets for a release without install-service, and both
// must write the description Detect reads.
func TestServiceTaskDescription_IsTheInstallers(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "install.ps1"))
	require.NoError(t, err)
	m := regexp.MustCompile(`(?m)^\$TaskDescription = '([^']*)'`).FindSubmatch(raw)
	require.NotNil(t, m, "docs/install.ps1 sets $TaskDescription")
	require.Equal(t, serviceTaskDescription, string(m[1]))
	require.Contains(t, serviceTaskDescription, update.InstallerMarker)

	m = regexp.MustCompile(`(?m)^\$ServiceProtocol = '([^']*)'`).FindSubmatch(raw)
	require.NotNil(t, m, "docs/install.ps1 sets $ServiceProtocol")
	require.Equal(t, serviceProtocolLine, string(m[1]), "install.ps1 looks for the line install-service -h prints")
}

func TestInstallServiceUsage_PrintsTheProtocolLine(t *testing.T) {
	var out bytes.Buffer
	fs, _, _ := installServiceFlags(&out)
	fs.Usage()
	require.Contains(t, out.String(), "usage: seamlessd install-service")
	require.Contains(t, out.String(), "\n"+serviceProtocolLine+"\n")
	require.Contains(t, out.String(), "-log-file")
}

func TestRunInstallService_RefusesOtherOSes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this is the other OSes' answer")
	}
	err := runInstallService([]string{"--config", "/c.yaml", "--log-file", "/l.log"})
	require.ErrorContains(t, err, "only Windows")
}

// fakeServiceTaskEnv records what registerServiceTask ran and wrote.
type fakeServiceTaskEnv struct {
	existing []byte // the registered task's XML; nil for none
	mine     bool   // whether its principal is this user
	mineErr  error  // isCurrentUser's error
	runErr   map[string]error
	runOut   map[string]string
	writeErr error

	ran     [][]string
	written []byte
	removed []string
}

func (f *fakeServiceTaskEnv) env() serviceTaskEnv {
	return serviceTaskEnv{
		sid: "S-1-5-21-1-2-3-1001", account: `DESKTOP-1\me`, exe: `C:\Users\me\.local\bin\seamlessd.exe`,
		arguments: func(args []string) string { return strings.Join(args, " ") },
		splitArgs: func(s string) ([]string, error) { return strings.Fields(s), nil },
		queryXML: func() ([]byte, error) {
			if f.existing == nil {
				return nil, errors.New("exit status 1")
			}
			return f.existing, nil
		},
		isCurrentUser: func(string) (bool, error) { return f.mine, f.mineErr },
		write: func(def []byte) (string, error) {
			if f.writeErr != nil {
				return "", f.writeErr
			}
			f.written = def
			return `C:\Users\me\AppData\Local\Temp\seamless-task-1.xml`, nil
		},
		remove: func(path string) error { f.removed = append(f.removed, path); return nil },
		run: func(argv []string) ([]byte, error) {
			f.ran = append(f.ran, argv)
			return []byte(f.runOut[argv[1]]), f.runErr[argv[1]]
		},
	}
}

// serviceTaskXML is a registered task's definition as schtasks /Query /XML
// gives it, for the principal user running command with arguments.
func serviceTaskXML(user, command, arguments string) []byte {
	return utf16WithBOM(`<?xml version="1.0" encoding="UTF-16"?><Task xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">` +
		`<RegistrationInfo><Description>x</Description></RegistrationInfo>` +
		`<Principals><Principal id="Author"><UserId>` + user + `</UserId></Principal></Principals>` +
		`<Actions Context="Author"><Exec><Command>` + command + `</Command><Arguments>` + arguments + `</Arguments></Exec></Actions></Task>`)
}

func TestRegisterServiceTask(t *testing.T) {
	const (
		defPath = `C:\Users\me\AppData\Local\Temp\seamless-task-1.xml`
		exe     = `C:\Users\me\.local\bin\seamlessd.exe`
		args    = `serve --config C:\c.yaml --log-file C:\l.log`
	)
	create := []string{"schtasks", "/Create", "/TN", "Seamless", "/XML", defPath, "/F"}
	run := []string{"schtasks", "/Run", "/TN", "Seamless"}
	remove := []string{"schtasks", "/Delete", "/TN", "Seamless", "/F"}
	other := serviceTaskXML("S-1-5-21-1-2-3-1002", exe, args)
	same := serviceTaskXML("S-1-5-21-1-2-3-1001", `"C:\USERS\me\.local\bin\seamlessd.exe"`, args)
	denied := func(f *fakeServiceTaskEnv) *fakeServiceTaskEnv {
		f.runErr = map[string]error{"/Create": errors.New("exit status 1")}
		f.runOut = map[string]string{"/Create": "ERROR: Access is denied.\r\n"}
		return f
	}

	t.Run("no task yet: register, then start", func(t *testing.T) {
		f := &fakeServiceTaskEnv{}
		kept, err := registerServiceTask(f.env(), `C:\c.yaml`, `C:\l.log`)
		require.NoError(t, err)
		require.False(t, kept)
		require.Equal(t, [][]string{create, run}, f.ran)
		require.Equal(t, []string{defPath}, f.removed, "no definition is left behind")
		doc := decodeServiceTask(t, f.written)
		require.Equal(t, args, doc.Actions.Arguments)
		require.Equal(t, exe, doc.Actions.Command)
	})

	t.Run("this user's task: replaced", func(t *testing.T) {
		f := &fakeServiceTaskEnv{existing: same, mine: true}
		kept, err := registerServiceTask(f.env(), `C:\c.yaml`, `C:\l.log`)
		require.NoError(t, err)
		require.False(t, kept, "a task schtasks replaces is registered afresh, descriptor and all")
		require.Equal(t, [][]string{create, run}, f.ran)
	})

	t.Run("another user's task: refused before schtasks, naming the fix", func(t *testing.T) {
		f := &fakeServiceTaskEnv{existing: other}
		_, err := registerServiceTask(f.env(), `C:\c.yaml`, `C:\l.log`)
		require.ErrorContains(t, err, "belongs to S-1-5-21-1-2-3-1002")
		require.ErrorContains(t, err, "schtasks /delete /tn Seamless /f")
		require.Empty(t, f.ran)
		require.Nil(t, f.written)
	})

	t.Run("an unreadable task: schtasks decides", func(t *testing.T) {
		f := &fakeServiceTaskEnv{existing: []byte("not xml")}
		_, err := registerServiceTask(f.env(), `C:\c.yaml`, `C:\l.log`)
		require.NoError(t, err)
		require.Equal(t, [][]string{create, run}, f.ran)
	})

	t.Run("create refused, no task: its message, nothing started", func(t *testing.T) {
		f := denied(&fakeServiceTaskEnv{})
		_, err := registerServiceTask(f.env(), `C:\c.yaml`, `C:\l.log`)
		require.ErrorContains(t, err, "schtasks /Create: exit status 1: ERROR: Access is denied.")
		require.ErrorContains(t, err, "group policy")
		require.ErrorContains(t, err, "another user of this computer may hold the name Seamless", "a task this user may not read looks like none")
		require.Equal(t, [][]string{create}, f.ran)
		require.Equal(t, []string{defPath}, f.removed)
	})

	t.Run("create refused over this user's task that runs this exe and args: kept and started", func(t *testing.T) {
		f := denied(&fakeServiceTaskEnv{existing: same, mine: true})
		kept, err := registerServiceTask(f.env(), `C:\c.yaml`, `C:\l.log`)
		require.NoError(t, err)
		require.True(t, kept)
		require.Equal(t, [][]string{create, run}, f.ran, "never removed: this run could not, and it already starts the daemon at logon")
	})

	t.Run("kept, but it cannot start: the error, the task left alone", func(t *testing.T) {
		f := denied(&fakeServiceTaskEnv{existing: same, mine: true})
		f.runErr["/Run"] = errors.New("exit status 1")
		f.runOut["/Run"] = "ERROR: The task has been disabled.\r\n"
		_, err := registerServiceTask(f.env(), `C:\c.yaml`, `C:\l.log`)
		require.ErrorContains(t, err, "could not start the Seamless Scheduled Task already registered")
		require.ErrorContains(t, err, "ERROR: The task has been disabled.")
		require.Equal(t, [][]string{create, run}, f.ran)
	})

	for _, tt := range []struct{ name, command, arguments string }{
		{"another exe", `D:\old\seamlessd.exe`, args},
		{"other arguments", exe, `serve --config C:\other.yaml --log-file C:\l.log`},
	} {
		t.Run("create refused over this user's task with "+tt.name+": names the admin's fix", func(t *testing.T) {
			f := denied(&fakeServiceTaskEnv{existing: serviceTaskXML("S-1-5-21-1-2-3-1001", tt.command, tt.arguments), mine: true})
			_, err := registerServiceTask(f.env(), `C:\c.yaml`, `C:\l.log`)
			require.ErrorContains(t, err, "could not replace the Seamless Scheduled Task")
			require.ErrorContains(t, err, "ERROR: Access is denied.")
			require.ErrorContains(t, err, "an elevated 'schtasks /delete /tn Seamless /f', then re-run the installer",
				"an elevated re-run would not do: replacing a task keeps its descriptor")
			require.Equal(t, [][]string{create}, f.ran, "a task that would start something else is not started")
		})
	}

	t.Run("create refused over a task whose principal cannot be told: not kept", func(t *testing.T) {
		f := denied(&fakeServiceTaskEnv{existing: same, mineErr: errors.New("no mapping")})
		_, err := registerServiceTask(f.env(), `C:\c.yaml`, `C:\l.log`)
		require.ErrorContains(t, err, "group policy")
		require.Equal(t, [][]string{create}, f.ran)
	})

	t.Run("registered but cannot start: removed again", func(t *testing.T) {
		f := &fakeServiceTaskEnv{
			runErr: map[string]error{"/Run": errors.New("exit status 1")},
			runOut: map[string]string{"/Run": "ERROR: The task image is corrupt.\r\n"},
		}
		_, err := registerServiceTask(f.env(), `C:\c.yaml`, `C:\l.log`)
		require.ErrorContains(t, err, "removed again")
		require.ErrorContains(t, err, "ERROR: The task image is corrupt.")
		require.Equal(t, [][]string{create, run, remove}, f.ran,
			"a task left registered would start beside the installer's Startup-folder fallback at the next logon")
	})

	t.Run("definition not written: nothing run", func(t *testing.T) {
		f := &fakeServiceTaskEnv{writeErr: errors.New("disk full")}
		_, err := registerServiceTask(f.env(), `C:\c.yaml`, `C:\l.log`)
		require.ErrorContains(t, err, "disk full")
		require.Empty(t, f.ran)
		require.Empty(t, f.removed)
	})
}

func TestWriteTaskFile(t *testing.T) {
	dir := t.TempDir()
	path, err := writeTaskFile(dir, "seamless-task-*.xml", []byte("def"))
	require.NoError(t, err)
	require.Equal(t, dir, filepath.Dir(path))
	require.True(t, strings.HasPrefix(filepath.Base(path), "seamless-task-"))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "def", string(got))

	_, err = writeTaskFile(filepath.Join(dir, "missing"), "x-*.xml", []byte("def"))
	require.ErrorContains(t, err, "write the task definition")
}

func TestTrimAccount(t *testing.T) {
	require.Equal(t, `DESKTOP-1\me`, trimAccount("DESKTOP-1", "me"))
	require.Equal(t, "me", trimAccount("", "me"))
	require.Equal(t, `AzureAD\Jane Doe`, trimAccount(" AzureAD ", "Jane Doe "))
}

func TestServiceTaskArgs(t *testing.T) {
	require.Equal(t, []string{"serve", "--config", `C:\c.yaml`, "--log-file", `C:\l.log`},
		serviceTaskArgs(`C:\c.yaml`, `C:\l.log`))
}
