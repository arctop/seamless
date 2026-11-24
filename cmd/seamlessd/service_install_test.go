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
	"strconv"
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
			require.Equal(t, "Seamless-"+sid, p.name, "the task is this user's alone: a task name is the whole computer's")
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
	require.Equal(t, []string{"schtasks", "/Create", "/TN", "Seamless-S-1-5-21-9", "/XML", `C:\t\seamless-task-1.xml`, "/F"},
		p.create(`C:\t\seamless-task-1.xml`))
	require.Equal(t, []string{"schtasks", "/Run", "/TN", "Seamless-S-1-5-21-9"}, p.run())
	require.Equal(t, []string{"schtasks", "/End", "/TN", "Seamless-S-1-5-21-9"}, p.end())
	require.Equal(t, []string{"schtasks", "/Delete", "/TN", "Seamless-S-1-5-21-9", "/F"}, p.remove())
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

	m = regexp.MustCompile(`(?m)^\$TaskName = '([^']*)' \+ `).FindSubmatch(raw)
	require.NotNil(t, m, "docs/install.ps1 sets $TaskName to the per-user name")
	require.Equal(t, serviceTaskPrefix, string(m[1]), "install.ps1 stops and reports the task install-service registers")
	m = regexp.MustCompile(`(?m)^\$LegacyTaskName = '([^']*)'`).FindSubmatch(raw)
	require.NotNil(t, m, "docs/install.ps1 sets $LegacyTaskName")
	require.Equal(t, legacyServiceTask, string(m[1]))

	m = regexp.MustCompile(`(?m)^\$ServiceDeferredExit = (\d+)`).FindSubmatch(raw)
	require.NotNil(t, m, "docs/install.ps1 sets $ServiceDeferredExit")
	require.Equal(t, strconv.Itoa(serviceDeferredExit), string(m[1]), "install.ps1 reads install-service's exit code for a task that starts at sign-in")

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
	tasks   map[string][]byte // the registered tasks' XML this user can read, by name
	mineErr error             // isCurrentUser's error
	// runErr and runOut answer one schtasks command, keyed "/Verb name".
	runErr   map[string]error
	runOut   map[string]string
	writeErr error
	notRun   bool // Windows did not start a task /Run asked for: nobody is signed in

	ran     [][]string
	written []byte
	removed []string
}

const fakeSID = "S-1-5-21-1-2-3-1001"

func (f *fakeServiceTaskEnv) env() serviceTaskEnv {
	return serviceTaskEnv{
		sid: fakeSID, account: `DESKTOP-1\me`, exe: `C:\Users\me\.local\bin\seamlessd.exe`,
		arguments: func(args []string) string { return strings.Join(args, " ") },
		splitArgs: func(s string) ([]string, error) { return strings.Fields(s), nil },
		queryXML: func(name string) ([]byte, error) {
			if raw, ok := f.tasks[name]; ok {
				return raw, nil
			}
			return nil, errors.New("exit status 1")
		},
		isCurrentUser: func(account string) (bool, error) { return account == fakeSID, f.mineErr },
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
			key := argv[1] + " " + argv[3]
			return []byte(f.runOut[key]), f.runErr[key]
		},
		started: func(string) bool { return !f.notRun },
	}
}

// fails makes one schtasks command fail with out.
func (f *fakeServiceTaskEnv) fails(key, out string) *fakeServiceTaskEnv {
	if f.runErr == nil {
		f.runErr, f.runOut = map[string]error{}, map[string]string{}
	}
	f.runErr[key] = errors.New("exit status 1")
	f.runOut[key] = out
	return f
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
		task    = "Seamless-" + fakeSID
		denied  = "ERROR: Access is denied.\r\n"
	)
	create := []string{"schtasks", "/Create", "/TN", task, "/XML", defPath, "/F"}
	run := []string{"schtasks", "/Run", "/TN", task}
	remove := []string{"schtasks", "/Delete", "/TN", task, "/F"}
	endOld := []string{"schtasks", "/End", "/TN", "Seamless"}
	removeOld := []string{"schtasks", "/Delete", "/TN", "Seamless", "/F"}
	runOld := []string{"schtasks", "/Run", "/TN", "Seamless"}
	// same is a task of this user's that runs this exe and args, quoted the
	// way install.ps1's cmdlets quoted them; other is another user's.
	same := serviceTaskXML(fakeSID, `"C:\USERS\me\.local\bin\seamlessd.exe"`, args)
	other := serviceTaskXML("S-1-5-21-1-2-3-1002", exe, args)
	register := func(f *fakeServiceTaskEnv) (registeredService, error) {
		return registerServiceTask(f.env(), `C:\c.yaml`, `C:\l.log`)
	}

	t.Run("no task yet: register the per-user task, then start it", func(t *testing.T) {
		f := &fakeServiceTaskEnv{}
		got, err := register(f)
		require.NoError(t, err)
		require.Equal(t, registeredService{name: task}, got)
		require.Equal(t, [][]string{create, run}, f.ran)
		require.Equal(t, []string{defPath}, f.removed, "no definition is left behind")
		doc := decodeServiceTask(t, f.written)
		require.Equal(t, args, doc.Actions.Arguments)
		require.Equal(t, exe, doc.Actions.Command)
	})

	t.Run("this user's task: replaced", func(t *testing.T) {
		f := &fakeServiceTaskEnv{tasks: map[string][]byte{task: same}}
		got, err := register(f)
		require.NoError(t, err)
		require.Equal(t, registeredService{name: task}, got)
		require.Equal(t, [][]string{create, run}, f.ran)
	})

	t.Run("a task of this name with another principal: refused before schtasks", func(t *testing.T) {
		f := &fakeServiceTaskEnv{tasks: map[string][]byte{task: other}}
		_, err := register(f)
		require.ErrorContains(t, err, "belongs to S-1-5-21-1-2-3-1002")
		require.ErrorContains(t, err, "schtasks /delete /tn "+task+" /f")
		require.Empty(t, f.ran)
		require.Nil(t, f.written)
	})

	t.Run("this user's Seamless from an earlier release: moved to the per-user name", func(t *testing.T) {
		f := &fakeServiceTaskEnv{tasks: map[string][]byte{"Seamless": same}}
		got, err := register(f)
		require.NoError(t, err)
		require.Equal(t, registeredService{name: task, migrated: true}, got)
		require.Equal(t, [][]string{create, endOld, removeOld, run}, f.ran,
			"the old task goes only once the new one is registered, and before it starts")
	})

	t.Run("an old Seamless that is not running: deleted all the same", func(t *testing.T) {
		f := (&fakeServiceTaskEnv{tasks: map[string][]byte{"Seamless": same}}).
			fails("/End Seamless", "ERROR: The task is not running.\r\n")
		got, err := register(f)
		require.NoError(t, err)
		require.True(t, got.migrated)
		require.Equal(t, [][]string{create, endOld, removeOld, run}, f.ran)
	})

	for name, tasks := range map[string]map[string][]byte{
		"another user's Seamless":                   {"Seamless": other},
		"a Seamless this user may not read":         {},
		"a Seamless whose principal cannot be told": {"Seamless": serviceTaskXML("NOBODY", exe, args)},
	} {
		t.Run(name+": left alone, the per-user task registered beside it", func(t *testing.T) {
			f := &fakeServiceTaskEnv{tasks: tasks}
			if name == "a Seamless whose principal cannot be told" {
				f.mineErr = errors.New("no mapping")
			}
			got, err := register(f)
			require.NoError(t, err)
			require.Equal(t, registeredService{name: task}, got)
			require.Equal(t, [][]string{create, run}, f.ran, "never ended or deleted: it is not this user's")
		})
	}

	t.Run("an old Seamless only an admin may delete, running this exe: the new task goes, the old one is kept and started", func(t *testing.T) {
		f := (&fakeServiceTaskEnv{tasks: map[string][]byte{"Seamless": same}}).fails("/Delete Seamless", denied)
		got, err := register(f)
		require.NoError(t, err)
		require.Equal(t, registeredService{name: "Seamless", kept: true}, got)
		require.Equal(t, [][]string{create, endOld, removeOld, remove, runOld}, f.ran,
			"two tasks would start two daemons at every logon")
	})

	t.Run("kept, but the old task cannot start: the error", func(t *testing.T) {
		f := (&fakeServiceTaskEnv{tasks: map[string][]byte{"Seamless": same}}).
			fails("/Delete Seamless", denied).fails("/Run Seamless", "ERROR: The task has been disabled.\r\n")
		_, err := register(f)
		require.ErrorContains(t, err, "could not start the Seamless Scheduled Task already registered")
		require.ErrorContains(t, err, "ERROR: The task has been disabled.")
		require.Equal(t, [][]string{create, endOld, removeOld, remove, runOld}, f.ran)
	})

	for _, tt := range []struct{ name, command, arguments string }{
		{"another exe", `D:\old\seamlessd.exe`, args},
		{"other arguments", exe, `serve --config C:\other.yaml --log-file C:\l.log`},
	} {
		t.Run("an old Seamless only an admin may delete, with "+tt.name+": names the admin's fix", func(t *testing.T) {
			f := (&fakeServiceTaskEnv{tasks: map[string][]byte{"Seamless": serviceTaskXML(fakeSID, tt.command, tt.arguments)}}).
				fails("/Delete Seamless", denied)
			_, err := register(f)
			require.ErrorContains(t, err, "could not replace the Seamless Scheduled Task an earlier release registered")
			require.ErrorContains(t, err, "ERROR: Access is denied.")
			require.ErrorContains(t, err, "an elevated 'schtasks /delete /tn Seamless /f', then re-run the installer")
			require.Equal(t, [][]string{create, endOld, removeOld, remove}, f.ran,
				"neither task is started: the old one would start something else")
		})
	}

	t.Run("create refused, no task: its message, nothing started", func(t *testing.T) {
		f := (&fakeServiceTaskEnv{}).fails("/Create "+task, denied)
		_, err := register(f)
		require.ErrorContains(t, err, "could not register the "+task+" Scheduled Task: schtasks /Create: exit status 1: ERROR: Access is denied.")
		require.ErrorContains(t, err, "group policy")
		require.Equal(t, [][]string{create}, f.ran)
		require.Equal(t, []string{defPath}, f.removed)
	})

	t.Run("create refused, an old Seamless of this user's running this exe: kept and started", func(t *testing.T) {
		f := (&fakeServiceTaskEnv{tasks: map[string][]byte{"Seamless": same}}).fails("/Create "+task, denied)
		got, err := register(f)
		require.NoError(t, err)
		require.Equal(t, registeredService{name: "Seamless", kept: true}, got)
		require.Equal(t, [][]string{create, runOld}, f.ran, "it already starts the daemon at logon")
	})

	t.Run("create refused, an old Seamless running something else: not kept", func(t *testing.T) {
		f := (&fakeServiceTaskEnv{tasks: map[string][]byte{"Seamless": serviceTaskXML(fakeSID, `D:\old\seamlessd.exe`, args)}}).
			fails("/Create "+task, denied)
		_, err := register(f)
		require.ErrorContains(t, err, "group policy")
		require.Equal(t, [][]string{create}, f.ran)
	})

	t.Run("registered, but nobody is signed in to start it: kept, deferred to the next sign-in", func(t *testing.T) {
		f := &fakeServiceTaskEnv{tasks: map[string][]byte{"Seamless": same}, notRun: true}
		got, err := register(f)
		require.NoError(t, err)
		require.Equal(t, registeredService{name: task, migrated: true, deferred: true}, got)
		require.Equal(t, [][]string{create, endOld, removeOld, run}, f.ran,
			"never removed: it starts the daemon at the next sign-in, and a fallback would not start sooner")
	})

	t.Run("kept, and nobody is signed in: deferred", func(t *testing.T) {
		f := (&fakeServiceTaskEnv{tasks: map[string][]byte{"Seamless": same}, notRun: true}).fails("/Create "+task, denied)
		got, err := register(f)
		require.NoError(t, err)
		require.Equal(t, registeredService{name: "Seamless", kept: true, deferred: true}, got)
	})

	t.Run("registered but cannot start: removed again", func(t *testing.T) {
		f := (&fakeServiceTaskEnv{}).fails("/Run "+task, "ERROR: The task image is corrupt.\r\n")
		_, err := register(f)
		require.ErrorContains(t, err, "removed again")
		require.ErrorContains(t, err, "ERROR: The task image is corrupt.")
		require.Equal(t, [][]string{create, run, remove}, f.ran,
			"a task left registered would start beside the installer's Startup-folder fallback at the next logon")
	})

	t.Run("definition not written: nothing run", func(t *testing.T) {
		f := &fakeServiceTaskEnv{writeErr: errors.New("disk full")}
		_, err := register(f)
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
