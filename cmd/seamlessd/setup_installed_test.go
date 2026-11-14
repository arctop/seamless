package main

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
)

// setupConfigCases are config files the installers' reading must agree on.
var setupConfigCases = map[string]string{
	"plain":            "role: client\nserver_url: https://box.example:8081\naddr: 0.0.0.0:8081\n",
	"double quoted":    "role: \"client\"\nserver_url: \"https://box.example:8081\"\n",
	"single quoted":    "role: 'client'\nserver_url: 'https://box.example:8081'\n",
	"comment":          "role: client # this machine dials the server\n",
	"no space":         "role:client\n",
	"tabs":             "role:\t\tclient\n",
	"crlf":             "role: client\r\nserver_url: https://box.example:8081\r\n",
	"nested only":      "peer_auth:\n  role: client\n  server_url: x\n",
	"twice":            "role: server\nrole: client\n",
	"empty first":      "role:\nrole: client\n",
	"a longer key":     "roles: client\nserver_url_x: y\n",
	"quote inside":     "server_url: \"https://a\"b\"\n",
	"no newline":       "role: client",
	"blank":            "",
	"server role":      "addr: \"127.0.0.1:9090\"\nmcp:\n  api_key: \"k\"\n",
	"trailing slash":   "role: client\nserver_url: \"https://box.example:8081/\"\n",
	"space then quote": "role:  \" client\"\n",
}

// TestInstallerConfigValue_IsTheInstallersRead runs docs/install's own
// config_value in sh over every case and holds installerConfigValue to its
// answer: setup decides what the installer acts on, so it must read the
// config exactly as the installer does.
func TestInstallerConfigValue_IsTheInstallersRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("docs/install is a POSIX sh script")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "install"))
	require.NoError(t, err)
	fn := regexp.MustCompile(`(?ms)^config_value\(\) \{\n.*?^\}\n`).Find(raw)
	require.NotEmpty(t, fn, "docs/install defines config_value")
	dir := t.TempDir()
	for name, body := range setupConfigCases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "seamless.yaml")
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			for _, key := range []string{"role", "server_url", "addr"} {
				script := string(fn) + "CONFIG=\"$1\"\nprintf '[%s]' \"$(config_value " + key + ")\"\n"
				out, err := exec.Command("/bin/sh", "-c", script, "sh", path).CombinedOutput()
				require.NoError(t, err, string(out))
				require.Equal(t, string(out), "["+installerConfigValue([]byte(body), key)+"]", "%s of %q", key, body)
			}
		})
	}
}

// setupProbeEnv is an install probe over a scratch home and fixture files:
// a path in files reads as its content; every other path is absent.
func setupProbeEnv(goos, home string, environ []string, files map[string]string) installProbeEnv {
	return installProbeEnv{
		goos: goos, home: home, environ: environ,
		readFile: func(path string) ([]byte, error) {
			if body, ok := files[path]; ok {
				if body == "\x00unreadable" {
					return nil, fs.ErrPermission
				}
				return []byte(body), nil
			}
			return nil, fs.ErrNotExist
		},
		sameFile: func(a, b string) bool { return a == b },
		taskXML:  func() ([]byte, error) { return nil, errors.New("no task") },
		resolve:  func(string) (access.Account, error) { return access.Account{}, errors.New("no account") },
	}
}

func TestSetupFactsOf(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "h")
	cfg := filepath.Join(home, ".config", "seamless", "seamless.yaml")
	exists := func(paths ...string) func(string) bool {
		return func(p string) bool {
			for _, want := range paths {
				if p == want {
					return true
				}
			}
			return false
		}
	}
	none := exists()

	t.Run("a scratch home holds nothing", func(t *testing.T) {
		f := setupFactsOf(setupHost{probe: setupProbeEnv("linux", home, nil, nil), exists: none})
		require.Equal(t, setupFacts{configPath: cfg}, f)
		require.Equal(t, setupNone, setupInstalledOf(f).kind)
	})
	t.Run("the installer's config and unit", func(t *testing.T) {
		unit := filepath.Join(home, ".config", "systemd", "user", systemdUnit)
		f := setupFactsOf(setupHost{probe: setupProbeEnv("linux", home, nil, map[string]string{
			cfg: "addr: \"0.0.0.0:8081\"\nserver_url: \"https://box.example:8081\"\n", unit: installerUnit,
		}), exists: none})
		require.True(t, f.configFound)
		require.True(t, f.userService)
		require.Equal(t, "0.0.0.0:8081", f.addr)
		require.Equal(t, setupServer, setupInstalledOf(f).kind)
	})
	t.Run("a LaunchAgent and the data, no config", func(t *testing.T) {
		plist := launchdPlist(home, launchdLabel)
		f := setupFactsOf(setupHost{probe: setupProbeEnv("darwin", home, nil, map[string]string{plist: installerPlist}),
			exists: exists(filepath.Join(home, ".seamless", "seam.db"))})
		require.False(t, f.configFound)
		require.True(t, f.userService)
		require.True(t, f.localData)
		require.Equal(t, setupStandalone, setupInstalledOf(f).kind)
	})
	t.Run("peer_auth: the owner's socket config and the LaunchDaemon", func(t *testing.T) {
		daemon := filepath.Join(systemDaemonPlistDir, systemDaemonLabel+".plist")
		f := setupFactsOf(setupHost{probe: setupProbeEnv("darwin", home, nil, map[string]string{
			cfg:    "role: client\nserver_url: \"unix:///opt/seamless/run/seamlessd.sock\"\n",
			daemon: installerPlist,
		}), exists: none})
		require.True(t, f.systemService)
		in := setupInstalledOf(f)
		require.Equal(t, setupStandalone, in.kind)
		require.True(t, in.locked)
		require.Equal(t, "client", in.role, "the installer acts on the owner's config: a client of the socket")
	})
	t.Run("SEAMLESS_CONFIG names the config", func(t *testing.T) {
		other := filepath.Join(home, "other.yaml")
		f := setupFactsOf(setupHost{probe: setupProbeEnv("linux", home, []string{"SEAMLESS_CONFIG=" + other},
			map[string]string{other: "role: client\nserver_url: https://box.example:8081\n"}), exists: none})
		require.Equal(t, other, f.configPath)
		require.Equal(t, setupClient, setupInstalledOf(f).kind)
	})
	t.Run("a config that cannot be read is there, with no values", func(t *testing.T) {
		f := setupFactsOf(setupHost{probe: setupProbeEnv("linux", home, nil, map[string]string{cfg: "\x00unreadable"}), exists: none})
		require.True(t, f.configFound)
		require.Equal(t, "", f.role)
		require.Equal(t, setupStandalone, setupInstalledOf(f).kind)
	})
	t.Run("Windows has no system service to look for", func(t *testing.T) {
		f := setupFactsOf(setupHost{probe: setupProbeEnv("windows", home, nil, map[string]string{cfg: "mcp:\n  api_key: k\n"}), exists: none})
		require.True(t, f.configFound)
		require.False(t, f.systemService)
		require.False(t, f.userService, "no Scheduled Task")
	})
	t.Run("the Scheduled Task counts only when it is this user's", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			mine bool
			err  error
			want setupKind
		}{
			{"this user's task", true, nil, setupStandalone},
			{"another user's task: this user installs fresh", false, nil, setupNone},
			{"a principal that cannot be judged", false, errors.New("no such account"), setupNone},
		} {
			env := setupProbeEnv("windows", home, nil, nil)
			env.taskXML = func() ([]byte, error) { return utf16LE(taskXML), nil }
			env.isCurrentUser = func(string) (bool, error) { return tt.mine, tt.err }
			f := setupFactsOf(setupHost{probe: env, exists: none})
			require.Equal(t, tt.mine && tt.err == nil, f.userService, tt.name)
			require.Equal(t, tt.want, setupInstalledOf(f).kind, tt.name)
		}
	})
}

func TestSetupNetworkedConfig(t *testing.T) {
	for _, tt := range []struct {
		addr, serverURL string
		want            bool
	}{
		{"", "", false},
		{"127.0.0.1:8081", "", false},
		{"localhost:8081", "", false},
		{"[::1]:8081", "", false},
		{"0.0.0.0:8081", "", true},
		{":8081", "", true},
		{"[::]:8081", "", true},
		{"10.0.0.5:8081", "", true},
		{"box.example:8081", "", true},
		{"127.0.0.1:8081", "https://box.example:8081", true},
		{"127.0.0.1:8081", "http://127.0.0.1:8081", false},
		{"127.0.0.1:8081", "unix:///opt/seamless/run/seamlessd.sock", false},
		{"not an addr", "", false},
	} {
		require.Equal(t, tt.want, setupNetworkedConfig(tt.addr, tt.serverURL), "%q %q", tt.addr, tt.serverURL)
	}
}
