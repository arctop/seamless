package main

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/update"
	"github.com/stretchr/testify/require"
)

// installerPlist is the plist docs/install writes, rendered for a home of /h.
const installerPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by https://thereisnospoon.org/install. Re-run it to update. -->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.arctop.seamless</string>
  <key>ProgramArguments</key>
  <array>
    <string>/h/.local/bin/seamlessd</string>
    <string>serve</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>SEAMLESS_CONFIG</key>
    <string>/h/.config/seamless/seamless.yaml</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
</dict>
</plist>
`

// installerUnit is the unit docs/install writes, rendered for a home of /h.
const installerUnit = `# Written by https://thereisnospoon.org/install. Re-run it to update.
[Unit]
Description=Seamless -- local-first memory and coordination substrate for AI agents
Documentation=https://thereisnospoon.org/docs/
After=network.target
StartLimitIntervalSec=0

[Service]
Type=simple
Environment=SEAMLESS_CONFIG=/h/.config/seamless/seamless.yaml
ExecStart=/h/.local/bin/seamlessd serve
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
`

const cgroupV2 = "0::/user.slice/user-1000.slice/user@1000.service/app.slice/seamless.service\n"

// The /proc/self/cgroup listings the 2.01 spike captured verbatim (Ubuntu
// 24.04 on cgroup v2; Debian 12 on full cgroup v1, which has no "0::" line and
// names the unit in the name=systemd hierarchy alone).
const (
	cgroupV2Daemon  = "0::/user.slice/user-501.slice/user@501.service/app.slice/seamless.service\n"
	cgroupV2Updater = "0::/user.slice/user-501.slice/user@501.service/app.slice/seamless-update-01M4HFPS1DBMJXQ4CE8ZZ0JRDH.service\n"
	cgroupV2Shell   = "0::/user.slice/user-501.slice/session-3.scope\n"
	cgroupV1Daemon  = "13:blkio:/\n12:freezer:/\n11:cpuset:/\n10:devices:/user.slice\n9:net_cls,net_prio:/\n8:hugetlb:/\n" +
		"7:perf_event:/\n6:pids:/user.slice/user-501.slice/user@501.service\n5:misc:/\n" +
		"4:memory:/user.slice/user-501.slice/user@501.service\n3:rdma:/\n2:cpu,cpuacct:/\n" +
		"1:name=systemd:/user.slice/user-501.slice/user@501.service/app.slice/seamless.service\n"
	cgroupV1Updater = "13:blkio:/\n12:freezer:/\n11:cpuset:/\n10:devices:/user.slice\n9:net_cls,net_prio:/\n8:hugetlb:/\n" +
		"7:perf_event:/\n6:pids:/user.slice/user-501.slice/user@501.service\n5:misc:/\n" +
		"4:memory:/user.slice/user-501.slice/user@501.service\n3:rdma:/\n2:cpu,cpuacct:/\n" +
		"1:name=systemd:/user.slice/user-501.slice/user@501.service/app.slice/seamless-update-01M4HGX6DW35570MVCR4P4WRFZ.service\n"
)

func TestPlistProgram(t *testing.T) {
	require.Equal(t, "/h/.local/bin/seamlessd", plistProgram([]byte(installerPlist)))
	require.Equal(t, "/a&b/seamlessd", plistProgram([]byte(
		`<plist><dict><key>ProgramArguments</key><array><string>/a&amp;b/seamlessd</string></array></dict></plist>`)),
		"entities decode")
	require.Empty(t, plistProgram([]byte(`<plist><dict><key>Label</key><string>x</string></dict></plist>`)))
	require.Empty(t, plistProgram([]byte(`<plist><dict><key>ProgramArguments</key><array></array></dict></plist>`)))
	require.Empty(t, plistProgram([]byte(`not xml at all`)))
	// A string in another array is not the program.
	require.Empty(t, plistProgram([]byte(
		`<plist><dict><key>WatchPaths</key><array><string>/tmp</string></array></dict></plist>`)))
}

func TestUnitExecStart(t *testing.T) {
	tests := []struct {
		unit string
		want string
	}{
		{installerUnit, "/h/.local/bin/seamlessd"},
		{"[Service]\nExecStart=-/usr/bin/seamlessd serve\n", "/usr/bin/seamlessd"},
		{"[Service]\nExecStart=\"/opt/my apps/seamlessd\" serve\n", "/opt/my apps/seamlessd"},
		{"[Service]\nExecStart=/x/seamlessd\tserve\n", "/x/seamlessd"},
		{"[Service]\nType=simple\n", ""},
		{"[Service]\nExecStart=\n", ""},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, unitExecStart([]byte(tt.unit)), tt.unit)
	}
}

func TestInSystemdUnit(t *testing.T) {
	tests := []struct {
		name   string
		cgroup string
		in     bool
	}{
		{"cgroup v2, the daemon", cgroupV2Daemon, true},
		{"cgroup v2, uid 1000", cgroupV2, true},
		{"cgroup v1, the daemon: only the name=systemd line names the unit", cgroupV1Daemon, true},
		{"cgroup v1 without app.slice (older systemd)",
			"12:cpu,cpuacct:/user.slice\n1:name=systemd:/user.slice/user-1000.slice/user@1000.service/seamless.service\n", true},
		{"a sub-cgroup of the unit, which the unit's stop kills",
			"0::/user.slice/user-501.slice/user@501.service/app.slice/seamless.service/x\n", true},
		{"a unit in a slice of its own",
			"0::/user.slice/user-501.slice/user@501.service/background.slice/seamless.service\n", true},
		{"cgroup v2, the updater's transient unit", cgroupV2Updater, false},
		{"cgroup v1, the updater's transient unit", cgroupV1Updater, false},
		{"a login shell", cgroupV2Shell, false},
		{"a system unit of the same name is not the installer's", "0::/system.slice/seamless.service\n", false},
		{"no user manager above the unit", "0::/user.slice/user-501.slice/seamless.service\n", false},
		{"a malformed manager segment", "0::/user.slice/user-501.slice/user@x.service/app.slice/seamless.service\n", false},
		{"another unit", "0::/system.slice/not-seamless.service\n", false},
		{"a non-systemd v1 controller does not count",
			"12:cpu:/user.slice/user-501.slice/user@501.service/app.slice/seamless.service\n", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.in, inSystemdUnit([]byte(tt.cgroup), "seamless.service"))
		})
	}
}

// utf16LE renders s the way schtasks writes it: UTF-16 little endian with a BOM.
func utf16LE(s string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(s)) {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

const taskXML = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Written by https://thereisnospoon.org/install.ps1. Re-run it to update.</Description>
  </RegistrationInfo>
  <Principals>
    <Principal id="Author">
      <UserId>S-1-5-21-1-2-3-1001</UserId>
      <LogonType>InteractiveToken</LogonType>
    </Principal>
  </Principals>
  <Actions Context="Author">
    <Exec>
      <Command>"C:\Users\me\AppData\Local\Programs\Seamless\seamlessd.exe"</Command>
      <Arguments>serve --config "C:\Users\me\.config\seamless\seamless.yaml"</Arguments>
    </Exec>
  </Actions>
</Task>`

func TestParseTaskXML(t *testing.T) {
	for name, raw := range map[string][]byte{"utf16": utf16LE(taskXML), "utf8": []byte(taskXML)} {
		t.Run(name, func(t *testing.T) {
			def, err := parseTaskXML(raw)
			require.NoError(t, err)
			require.Equal(t, `C:\Users\me\AppData\Local\Programs\Seamless\seamlessd.exe`, def.Command)
			require.Contains(t, def.Description, update.InstallerMarker)
			require.Equal(t, "S-1-5-21-1-2-3-1001", def.UserID)
		})
	}
	_, err := parseTaskXML([]byte("garbage"))
	require.Error(t, err)
}

func TestForeignSeamlessEnv(t *testing.T) {
	environ := []string{"PATH=/bin", "SEAMLESS_CONFIG=/c.yaml", "SEAMLESS_DATA_DIR=/d", "SEAMLESS_ADDR=x", "XSEAMLESS_Y=1"}
	require.Equal(t, []string{"SEAMLESS_ADDR", "SEAMLESS_DATA_DIR"}, foreignSeamlessEnv("linux", environ))
	require.Equal(t, []string{"SEAMLESS_ADDR"}, foreignSeamlessEnv("windows", []string{"Seamless_Config=c", "seamless_addr=x"}),
		"Windows names are case-insensitive")
	require.Empty(t, foreignSeamlessEnv("darwin", []string{"SEAMLESS_CONFIG=/c.yaml"}))
}

// fakeProbeEnv is an installer install on goos under home /h, every gate
// passing; tests break one fact at a time.
func fakeProbeEnv(goos string) installProbeEnv {
	files := map[string]string{
		"/h/Library/LaunchAgents/com.arctop.seamless.plist": installerPlist,
		"/h/.config/systemd/user/seamless.service":          installerUnit,
		"/proc/self/cgroup":                                 cgroupV2,
	}
	env := installProbeEnv{
		goos: goos,
		home: "/h",
		exe:  "/h/.local/bin/seamlessd",
		ppid: 1,
		euid: 501,
		readFile: func(p string) ([]byte, error) {
			if s, ok := files[filepath.ToSlash(p)]; ok {
				return []byte(s), nil
			}
			return nil, fs.ErrNotExist
		},
		sameFile: func(a, b string) bool {
			norm := func(s string) string { return strings.ReplaceAll(s, `\`, "/") }
			return strings.EqualFold(norm(a), norm(b))
		},
		writable: func(string) bool { return true },
		taskXML:  func() ([]byte, error) { return nil, errors.New("not windows") },
		parentImage: func() (string, error) {
			panic("only windows asks for the parent's image")
		},
		isCurrentUser: func(string) (bool, error) {
			panic("only windows asks whose the task is")
		},
	}
	switch goos {
	case "darwin":
		env.environ = []string{"XPC_SERVICE_NAME=com.arctop.seamless", "SEAMLESS_CONFIG=/h/.config/seamless/seamless.yaml"}
	case "linux":
		env.environ = []string{"INVOCATION_ID=abc", "SEAMLESS_CONFIG=/h/.config/seamless/seamless.yaml"}
	case "windows":
		// The Task Scheduler (svchost.exe) started this process from the
		// installer's Seamless task, which belongs to this user.
		env.home = `C:\Users\me`
		env.exe = `C:\Users\me\AppData\Local\Programs\Seamless\seamlessd.exe`
		env.ppid = 2672
		env.taskXML = func() ([]byte, error) { return utf16LE(taskXML), nil }
		env.parentImage = func() (string, error) { return "svchost.exe", nil }
		env.isCurrentUser = func(account string) (bool, error) { return account == "S-1-5-21-1-2-3-1001", nil }
	}
	return env
}

// legacyLabelInstall turns a darwin fakeProbeEnv into an install made before
// the label rename: the plist exists only under the legacy label, carrying
// that label, and launchd names the job by it.
func legacyLabelInstall(e *installProbeEnv) {
	plist := strings.Replace(installerPlist, "<string>com.arctop.seamless</string>", "<string>org.thereisnospoon.seamless</string>", 1)
	e.readFile = func(p string) ([]byte, error) {
		if filepath.ToSlash(p) == "/h/Library/LaunchAgents/org.thereisnospoon.seamless.plist" {
			return []byte(plist), nil
		}
		return nil, fs.ErrNotExist
	}
	e.environ = []string{"XPC_SERVICE_NAME=org.thereisnospoon.seamless", "SEAMLESS_CONFIG=/h/.config/seamless/seamless.yaml"}
}

func TestProbeInstall(t *testing.T) {
	const cfgPath = "/h/.config/seamless/seamless.yaml"
	const winCfg = `C:\Users\me\.config\seamless\seamless.yaml`
	tests := []struct {
		name     string
		goos     string
		mutate   func(e *installProbeEnv)
		cfg      string
		dist     string
		wantKind update.Kind
		reason   string
	}{
		{"darwin installer", "darwin", nil, cfgPath, "release", update.KindInstaller, ""},
		{"linux installer", "linux", nil, cfgPath, "release", update.KindInstaller, ""},
		{"source build is never probed further", "darwin", func(e *installProbeEnv) {
			e.readFile = func(string) ([]byte, error) { panic("a source build must not read service files") }
			e.writable = func(string) bool { panic("a source build must not write a probe file") }
		}, cfgPath, "source", update.KindSource, "built from source"},
		{"make install plist (no marker)", "darwin", func(e *installProbeEnv) {
			e.readFile = func(p string) ([]byte, error) {
				return []byte(strings.Replace(installerPlist, "<!-- Written by https://thereisnospoon.org/install. Re-run it to update. -->\n", "", 1)), nil
			}
		}, cfgPath, "release", update.KindUnknown, "not written by the installer"},
		{"no unit (SEAMLESS_NO_SERVICE)", "linux", func(e *installProbeEnv) {
			e.readFile = func(string) ([]byte, error) { return nil, fs.ErrNotExist }
		}, cfgPath, "release", update.KindUnknown, "no installer-managed service"},
		{"unreadable unit", "linux", func(e *installProbeEnv) {
			e.readFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
		}, cfgPath, "release", update.KindUnknown, "could not be read"},
		{"service runs another copy", "darwin", func(e *installProbeEnv) { e.exe = "/opt/other/seamlessd" },
			cfgPath, "release", update.KindUnknown, "not this executable"},
		{"custom config", "linux", nil, "/srv/seamless.yaml", "release", update.KindUnknown, "/srv/seamless.yaml"},
		{"started by hand on darwin", "darwin", func(e *installProbeEnv) { e.environ = nil },
			cfgPath, "release", update.KindUnknown, "launchd job"},
		// A Mac the installer has not run on since the label rename: only the
		// legacy plist, and launchd runs the job under the legacy label.
		{"darwin installer on the legacy label", "darwin", legacyLabelInstall,
			cfgPath, "release", update.KindInstaller, ""},
		{"darwin legacy plist, started by hand", "darwin", func(e *installProbeEnv) {
			legacyLabelInstall(e)
			e.environ = nil
		}, cfgPath, "release", update.KindUnknown, "not running as the launchd job com.arctop.seamless"},
		{"darwin job under a label that is neither", "darwin", func(e *installProbeEnv) {
			e.environ = []string{"XPC_SERVICE_NAME=com.example.other", "SEAMLESS_CONFIG=/h/.config/seamless/seamless.yaml"}
		}, cfgPath, "release", update.KindUnknown, "launchd job"},
		// An unreadable current plist is reported, not passed over for the
		// legacy one.
		{"darwin current plist unreadable", "darwin", func(e *installProbeEnv) {
			legacy := e.readFile
			e.readFile = func(p string) ([]byte, error) {
				if filepath.ToSlash(p) == "/h/Library/LaunchAgents/com.arctop.seamless.plist" {
					return nil, fs.ErrPermission
				}
				return legacy(p)
			}
		}, cfgPath, "release", update.KindUnknown, "com.arctop.seamless.plist could not be read"},
		{"darwin parent is not launchd", "darwin", func(e *installProbeEnv) { e.ppid = 4242 },
			cfgPath, "release", update.KindUnknown, "not a child of launchd"},
		{"linux outside the unit", "linux", func(e *installProbeEnv) {
			e.readFile = func(p string) ([]byte, error) {
				if p == "/proc/self/cgroup" {
					return []byte("0::/user.slice/user-1000.slice/session-2.scope\n"), nil
				}
				return []byte(installerUnit), nil
			}
		}, cfgPath, "release", update.KindUnknown, "not running in the seamless.service unit"},
		{"linux without INVOCATION_ID", "linux", func(e *installProbeEnv) { e.environ = nil },
			cfgPath, "release", update.KindUnknown, "INVOCATION_ID"},
		{"env override the installer drops", "linux", func(e *installProbeEnv) {
			e.environ = append(e.environ, "SEAMLESS_ADDR=0.0.0.0:8081")
		}, cfgPath, "release", update.KindUnknown, "SEAMLESS_ADDR"},
		{"read-only bin dir", "darwin", func(e *installProbeEnv) { e.writable = func(string) bool { return false } },
			cfgPath, "release", update.KindUnknown, "not writable"},
		{"root", "linux", func(e *installProbeEnv) { e.euid = 0 }, cfgPath, "release", update.KindUnknown, "root"},
		{"homebrew", "darwin", func(e *installProbeEnv) { e.exe = "/opt/homebrew/Caskroom/seamless/0.7.2/seamlessd" },
			cfgPath, "release", update.KindHomebrew, "Homebrew"},
		{"windows installer", "windows", nil, winCfg, "release", update.KindInstaller, ""},
		{"windows parent's image name is matched case-insensitively", "windows", func(e *installProbeEnv) {
			e.parentImage = func() (string, error) { return "SVCHOST.EXE", nil }
		}, winCfg, "release", update.KindInstaller, ""},
		{"windows serve started by hand from a shell", "windows", func(e *installProbeEnv) {
			e.parentImage = func() (string, error) { return "cmd.exe", nil }
		}, winCfg, "release", update.KindUnknown, "the parent process is cmd.exe, not the Task Scheduler (svchost.exe)"},
		{"windows serve started from Explorer", "windows", func(e *installProbeEnv) {
			e.parentImage = func() (string, error) { return "explorer.exe", nil }
		}, winCfg, "release", update.KindUnknown, "the parent process is explorer.exe"},
		{"windows parent unknown", "windows", func(e *installProbeEnv) {
			e.parentImage = func() (string, error) { return "", errors.New("the parent process 4242 has exited") }
		}, winCfg, "release", update.KindUnknown, "could not identify the parent process: the parent process 4242 has exited"},
		{"windows task belongs to another user", "windows", func(e *installProbeEnv) {
			e.isCurrentUser = func(string) (bool, error) { return false, nil }
		}, winCfg, "release", update.KindUnknown, "runs as S-1-5-21-1-2-3-1001, not this user"},
		{"windows task user cannot be resolved", "windows", func(e *installProbeEnv) {
			e.isCurrentUser = func(string) (bool, error) { return false, errors.New("no mapping") }
		}, winCfg, "release", update.KindUnknown, "could not tell whether"},
		{"windows task names no user", "windows", func(e *installProbeEnv) {
			e.taskXML = func() ([]byte, error) {
				return utf16LE(strings.Replace(taskXML, "<UserId>S-1-5-21-1-2-3-1001</UserId>", "", 1)), nil
			}
		}, winCfg, "release", update.KindUnknown, "names no user"},
		{"windows without the task", "windows", func(e *installProbeEnv) {
			e.taskXML = func() ([]byte, error) { return nil, errors.New("exit status 1") }
		}, winCfg, "release", update.KindUnknown, "no installer-managed service"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := fakeProbeEnv(tt.goos)
			if tt.mutate != nil {
				tt.mutate(&env)
			}
			got := update.Detect(probeInstallWith(env, tt.dist, "0.7.2", installConfig{path: tt.cfg}))
			require.Equal(t, tt.wantKind, got.Kind, got.Reason)
			if tt.reason == "" {
				require.Empty(t, got.Reason)
			} else {
				require.Contains(t, got.Reason, tt.reason)
			}
		})
	}
}

func TestProbeInstall_ClientStopsEarly(t *testing.T) {
	env := fakeProbeEnv("linux")
	env.readFile = func(string) ([]byte, error) { panic("a client machine has no service to read") }
	got := update.Detect(probeInstallWith(env, "release", "0.7.2", installConfig{path: "/h/.config/seamless/seamless.yaml", client: true}))
	require.Equal(t, update.KindClient, got.Kind)
}

// TestInstallerWritesTheMarker ties Detect to the real installer: the plist and
// the unit docs/install writes both carry update.InstallerMarker, and the
// plist `make install` renders does not -- which is the whole difference
// between an installer install and a developer's deploy.
func TestInstallerWritesTheMarker(t *testing.T) {
	install, err := os.ReadFile(filepath.Join("..", "..", "docs", "install"))
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(install), update.InstallerMarker),
		"docs/install writes the marker into both the launchd plist and the systemd unit")

	makePlist, err := os.ReadFile(filepath.Join("..", "..", "deploy", "launchd", launchdLabel+".plist"))
	require.NoError(t, err)
	require.NotContains(t, string(makePlist), update.InstallerMarker,
		"the make install plist must not look like the installer's")
}

func TestDirWritable(t *testing.T) {
	dir := t.TempDir()
	require.True(t, dirWritable(dir))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "the probe file is removed")
	require.False(t, dirWritable(filepath.Join(dir, "missing")))
}

func TestSameFile(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	require.NoError(t, os.WriteFile(a, []byte("x"), 0o600))
	link := filepath.Join(dir, "link")
	if err := os.Symlink(a, link); err == nil {
		require.True(t, sameFile(a, link))
	}
	require.True(t, sameFile(a, a))
	require.False(t, sameFile(a, filepath.Join(dir, "missing")))
}

// docBlock returns the first fenced code block of language lang in the docs
// source file at rel (from the repo root) that follows the line after.
func docBlock(t *testing.T, rel, after, lang string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	require.NoError(t, err)
	text := string(raw)
	i := strings.Index(text, after)
	require.GreaterOrEqual(t, i, 0, "%s has no %q", rel, after)
	text = text[i:]
	open := "```" + lang + "\n"
	j := strings.Index(text, open)
	require.GreaterOrEqual(t, j, 0, "%s has no %s block after %q", rel, lang, after)
	text = text[j+len(open):]
	k := strings.Index(text, "```")
	require.GreaterOrEqual(t, k, 0)
	return text[:k]
}

// fakeSystemProbeEnv is a peer_auth system daemon on goos laid out as the
// docs lay it out -- the LaunchDaemon plist of the service reference, the
// system unit of the peer_auth guide, both read from the docs source -- every
// system gate passing: _seamless (uid 480) is pinned, the owner is me (uid
// 501), and on Linux the account lingers.
func fakeSystemProbeEnv(t *testing.T, goos string) installProbeEnv {
	t.Helper()
	plist := docBlock(t, "docs-src/reference/service.md", "## A system service for peer_auth", "xml")
	unit := docBlock(t, "docs-src/guides/peer-auth.md", "On Linux it is a system unit", "ini")
	files := map[string]string{
		"/Library/LaunchDaemons/com.arctop.seamless.daemon.plist": plist,
		"/etc/systemd/system/seamless-daemon.service":             unit,
		"/proc/self/cgroup":                 "0::/system.slice/seamless-daemon.service\n",
		"/var/lib/systemd/linger/_seamless": "",
	}
	accounts := map[string]access.Account{
		"_seamless": {UID: 480, Name: "_seamless"}, "480": {UID: 480, Name: "_seamless"},
		"me": {UID: 501, Name: "me"}, "501": {UID: 501, Name: "me"},
	}
	env := fakeProbeEnv(goos)
	env.home = "/opt/seamless/home"
	env.exe = "/opt/seamless/bin/seamlessd"
	env.euid = 480
	env.readFile = func(p string) ([]byte, error) {
		if s, ok := files[filepath.ToSlash(p)]; ok {
			return []byte(s), nil
		}
		return nil, fs.ErrNotExist
	}
	env.resolve = func(name string) (access.Account, error) {
		if a, ok := accounts[name]; ok {
			return a, nil
		}
		return access.Account{}, errors.New("unknown user " + name)
	}
	env.daemonPin = func() (access.DaemonPin, bool, error) {
		return access.DaemonPin{Name: "_seamless", UID: 480, Path: access.DaemonAccountPath}, true, nil
	}
	env.systemAccount = func(uid uint32) (bool, string) { return uid < 500, "a system account has a uid below 500" }
	switch goos {
	case "darwin":
		env.environ = []string{"XPC_SERVICE_NAME=com.arctop.seamless.daemon", "SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml", "HOME=/opt/seamless/home"}
	case "linux":
		env.environ = []string{"INVOCATION_ID=abc", "SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml", "HOME=/opt/seamless/home"}
	}
	return env
}

func TestProbeInstall_System(t *testing.T) {
	const cfgPath = "/opt/seamless/etc/seamless.yaml"
	sysCfg := installConfig{path: cfgPath, peerAuth: true, owner: "me"}
	tests := []struct {
		name     string
		goos     string
		mutate   func(e *installProbeEnv)
		ic       installConfig
		wantKind update.Kind
		reason   string
	}{
		{"darwin LaunchDaemon", "darwin", nil, sysCfg, update.KindSystem, ""},
		{"linux system unit", "linux", nil, sysCfg, update.KindSystem, ""},
		{"a root-owned bin", "linux", func(e *installProbeEnv) { e.writable = func(string) bool { return false } },
			sysCfg, update.KindUnknown, "cannot write the executable's directory"},
		{"running as the owner", "darwin", func(e *installProbeEnv) { e.euid = 501 },
			sysCfg, update.KindUnknown, "is the owner's account, me"},
		{"running as root", "linux", func(e *installProbeEnv) { e.euid = 0 }, sysCfg, update.KindUnknown, "root"},
		{"another account than the pinned one", "darwin", func(e *installProbeEnv) { e.euid = 481 },
			sysCfg, update.KindUnknown, "pins the daemon's account as _seamless (uid 480)"},
		{"no pin, a system account", "darwin", func(e *installProbeEnv) {
			e.daemonPin = func() (access.DaemonPin, bool, error) { return access.DaemonPin{}, false, nil }
		}, sysCfg, update.KindSystem, ""},
		{"no pin, not a system account", "darwin", func(e *installProbeEnv) {
			e.daemonPin = func() (access.DaemonPin, bool, error) { return access.DaemonPin{}, false, nil }
			e.euid = 502
		}, sysCfg, update.KindUnknown, "below 500"},
		{"a pin that is not believed", "linux", func(e *installProbeEnv) {
			e.daemonPin = func() (access.DaemonPin, bool, error) {
				return access.DaemonPin{}, true, errors.New("the daemon account pin is group-writable")
			}
		}, sysCfg, update.KindUnknown, "group-writable"},
		{"no LaunchDaemon", "darwin", func(e *installProbeEnv) {
			e.readFile = func(string) ([]byte, error) { return nil, fs.ErrNotExist }
		}, sysCfg, update.KindUnknown, "no system service found (/Library/LaunchDaemons/com.arctop.seamless.daemon.plist)"},
		{"LaunchDaemon under the legacy label", "darwin", func(e *installProbeEnv) {
			plist := docBlock(t, "docs-src/reference/service.md", "## A system service for peer_auth", "xml")
			e.readFile = func(p string) ([]byte, error) {
				if p == "/Library/LaunchDaemons/org.thereisnospoon.seamless.daemon.plist" {
					return []byte(plist), nil
				}
				return nil, fs.ErrNotExist
			}
			e.environ = []string{"XPC_SERVICE_NAME=org.thereisnospoon.seamless.daemon"}
		}, sysCfg, update.KindSystem, ""},
		{"unit in /lib/systemd/system", "linux", func(e *installProbeEnv) {
			read := e.readFile
			e.readFile = func(p string) ([]byte, error) {
				switch p {
				case "/etc/systemd/system/seamless-daemon.service":
					return nil, fs.ErrNotExist
				case "/lib/systemd/system/seamless-daemon.service":
					return read("/etc/systemd/system/seamless-daemon.service")
				}
				return read(p)
			}
		}, sysCfg, update.KindSystem, ""},
		{"unit unreadable", "linux", func(e *installProbeEnv) {
			read := e.readFile
			e.readFile = func(p string) ([]byte, error) {
				if p == "/etc/systemd/system/seamless-daemon.service" {
					return nil, fs.ErrPermission
				}
				return read(p)
			}
		}, sysCfg, update.KindUnknown, "seamless-daemon.service could not be read"},
		{"service runs another copy", "darwin", func(e *installProbeEnv) { e.exe = "/usr/local/bin/seamlessd" },
			sysCfg, update.KindUnknown, "not this executable"},
		{"started by hand on darwin", "darwin", func(e *installProbeEnv) { e.environ = nil },
			sysCfg, update.KindUnknown, "not running as the launchd job com.arctop.seamless.daemon"},
		{"the per-user LaunchAgent's label is not the daemon's", "darwin", func(e *installProbeEnv) {
			e.environ = []string{"XPC_SERVICE_NAME=com.arctop.seamless"}
		}, sysCfg, update.KindUnknown, "launchd job com.arctop.seamless.daemon"},
		{"darwin parent is not launchd", "darwin", func(e *installProbeEnv) { e.ppid = 77 },
			sysCfg, update.KindUnknown, "not a child of launchd"},
		{"linux in the per-user unit", "linux", func(e *installProbeEnv) {
			read := e.readFile
			e.readFile = func(p string) ([]byte, error) {
				if p == "/proc/self/cgroup" {
					return []byte(cgroupV2Daemon), nil
				}
				return read(p)
			}
		}, sysCfg, update.KindUnknown, "not running in the seamless-daemon.service system unit"},
		{"linux without INVOCATION_ID", "linux", func(e *installProbeEnv) { e.environ = []string{"SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml"} },
			sysCfg, update.KindUnknown, "INVOCATION_ID"},
		{"a unit that restarts only on failure", "linux", func(e *installProbeEnv) {
			read := e.readFile
			e.readFile = func(p string) ([]byte, error) {
				raw, err := read(p)
				if p == "/etc/systemd/system/seamless-daemon.service" {
					raw = []byte(strings.Replace(string(raw), "Restart=always", "Restart=on-failure", 1))
				}
				return raw, err
			}
		}, sysCfg, update.KindUnknown, "does not start the daemon again after it exits cleanly"},
		{"a LaunchDaemon that keeps alive only on failure", "darwin", func(e *installProbeEnv) {
			read := e.readFile
			e.readFile = func(p string) ([]byte, error) {
				raw, err := read(p)
				return []byte(strings.Replace(string(raw), "<key>KeepAlive</key>         <true/>",
					"<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>", 1)), err
			}
		}, sysCfg, update.KindUnknown, "does not start the daemon again after it exits cleanly"},
		{"linux: an override in the unit the updater would run without", "linux", func(e *installProbeEnv) {
			e.environ = append(e.environ, "SEAMLESS_ADDR=127.0.0.1:9090")
		}, sysCfg, update.KindUnknown, "SEAMLESS_ADDR set in the system unit's environment"},
		{"darwin: the setsid updater inherits the daemon's environment", "darwin", func(e *installProbeEnv) {
			e.environ = append(e.environ, "SEAMLESS_ADDR=127.0.0.1:9090")
		}, sysCfg, update.KindSystem, ""},
		{"linux without lingering", "linux", func(e *installProbeEnv) {
			read := e.readFile
			e.readFile = func(p string) ([]byte, error) {
				if strings.HasPrefix(p, lingerDir) {
					return nil, fs.ErrNotExist
				}
				return read(p)
			}
		}, sysCfg, update.KindUnknown, "sudo loginctl enable-linger _seamless"},
		{"windows has no peer_auth", "windows", nil, sysCfg, update.KindUnknown, "macOS and Linux only"},
		{"no config file", "darwin", nil, installConfig{peerAuth: true, owner: "me"},
			update.KindUnknown, "no config file"},
		// The installer's own layout, with peer_auth switched on, is no
		// installer install: its per-user service cannot host the boundary.
		{"installer layout with peer_auth on", "darwin", func(e *installProbeEnv) {
			*e = fakeProbeEnv("darwin")
			e.resolve = func(string) (access.Account, error) { return access.Account{UID: 501, Name: "me"}, nil }
			e.daemonPin = func() (access.DaemonPin, bool, error) { return access.DaemonPin{}, false, nil }
			e.systemAccount = func(uint32) (bool, string) { return false, "a system account has a uid below 500" }
		}, installConfig{path: "/h/.config/seamless/seamless.yaml", peerAuth: true, owner: "me"},
			update.KindUnknown, "the owner's account"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := fakeSystemProbeEnv(t, tt.goos)
			if tt.mutate != nil {
				tt.mutate(&env)
			}
			got := update.Detect(probeInstallWith(env, "release", "0.8.1", tt.ic))
			require.Equal(t, tt.wantKind, got.Kind, got.Reason)
			if tt.reason == "" {
				require.Empty(t, got.Reason)
			} else {
				require.Contains(t, got.Reason, tt.reason)
			}
		})
	}
}

func TestInSystemUnit(t *testing.T) {
	tests := []struct {
		name   string
		cgroup string
		in     bool
	}{
		{"cgroup v2", "0::/system.slice/seamless-daemon.service\n", true},
		{"a slice of its own", "0::/seamless.slice/seamless-daemon.service\n", true},
		{"a sub-cgroup", "0::/system.slice/seamless-daemon.service/x\n", true},
		{"cgroup v1", "4:memory:/system.slice\n1:name=systemd:/system.slice/seamless-daemon.service\n", true},
		{"a user unit of the same name",
			"0::/user.slice/user-480.slice/user@480.service/app.slice/seamless-daemon.service\n", false},
		{"the updater's transient user unit",
			"0::/user.slice/user-480.slice/user@480.service/app.slice/seamless-update-01M4HFPS1DBMJXQ4CE8ZZ0JRDH.service\n", false},
		{"the per-user daemon", cgroupV2Daemon, false},
		{"a non-systemd v1 controller", "12:cpu:/system.slice/seamless-daemon.service\n", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.in, inSystemUnit([]byte(tt.cgroup), systemDaemonUnit))
		})
	}
}

// TestUpdaterProbe_SystemDaemonsUpdater is the updater's own view of a system
// install, through the real probe: it is no launchd job and no unit process
// (gate 9 fails for it, by design), yet it must still find KindSystem, or it
// refuses every attempt the daemon starts.
func TestUpdaterProbe_SystemDaemonsUpdater(t *testing.T) {
	sysCfg := installConfig{path: "/opt/seamless/etc/seamless.yaml", peerAuth: true, owner: "me"}
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			env := fakeSystemProbeEnv(t, goos)
			env.ppid = 4242 // the daemon, not launchd
			env.environ = nil
			read := env.readFile
			env.readFile = func(p string) ([]byte, error) {
				if p == "/proc/self/cgroup" {
					return []byte("0::/user.slice/user-480.slice/user@480.service/app.slice/seamless-update-01M4HFPS1DBMJXQ4CE8ZZ0JRDH.service\n"), nil
				}
				return read(p)
			}
			p := probeInstallWith(env, "release", "0.8.1", sysCfg)
			require.Equal(t, update.KindUnknown, update.Detect(p).Kind, "as the service's own process, it is not")
			for _, attended := range []bool{false, true} {
				got := update.Detect(updaterProbe(p, attended))
				require.Equal(t, update.KindSystem, got.Kind, got.Reason)
			}
		})
	}
}

func TestUnitRestartsOnCleanExit(t *testing.T) {
	for unit, want := range map[string]bool{
		"[Service]\nRestart=always\n":                true,
		"[Service]\nRestart=on-success\n":            true,
		"[Service]\nRestart=on-failure\n":            false,
		"[Service]\nRestart=always\nRestart=no\n":    false,
		"[Service]\nExecStart=/opt/seamless/bin/x\n": false,
		"[Service]\n  Restart = always\n":            false, // not systemd's spelling of the key
		installerUnit:                                true,
	} {
		require.Equal(t, want, unitRestartsOnCleanExit([]byte(unit)), unit)
	}
}

func TestPlistKeepsAlive(t *testing.T) {
	wrap := func(body string) []byte {
		return []byte(`<plist version="1.0"><dict><key>Label</key><string>x</string>` + body + `</dict></plist>`)
	}
	for body, want := range map[string]bool{
		`<key>KeepAlive</key><true/>`:  true,
		`<key>KeepAlive</key><false/>`: false,
		``:                             false,
		`<key>KeepAlive</key><dict><key>SuccessfulExit</key><true/></dict>`:                           true,
		`<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>`:                          false,
		`<key>KeepAlive</key><dict><key>Crashed</key><true/></dict>`:                                  false,
		`<key>KeepAlive</key><dict><key>PathState</key><dict><key>/x</key><true/></dict></dict>`:      false,
		`<key>RunAtLoad</key><true/><key>KeepAlive</key><false/>`:                                     false,
		`<key>KeepAlive</key><dict><key>Crashed</key><false/></dict><key>SuccessfulExit</key><true/>`: false,
	} {
		require.Equal(t, want, plistKeepsAlive(wrap(body)), body)
	}
	require.False(t, plistKeepsAlive([]byte("not xml")))
}
