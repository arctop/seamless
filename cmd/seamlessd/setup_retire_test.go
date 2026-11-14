package main

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
)

// setup's "Sign in with passkeys and retire the key" (setup_retire.go) over a
// fake daemon: its sign-in state as the socket answers it, and the root steps
// and the owner's sign-in arriving while setup waits. Nothing here dials,
// runs sudo, or reads the real home.

const (
	retireOldKey = "old-key-0123456789abcdef"
	retireOrigin = "http://localhost:8081"
)

// retireFake is the daemon and the people around it.
type retireFake struct {
	st      console.SignInState
	key     string      // the key the daemon holds, "" once retired
	signIns []time.Time // passkey sign-ins
	clock   time.Time
	down    bool
	v4, v6  string
	pinOK   bool
	pinErr  error
	wired   []retireWired
	// arrive runs at each wait, in order: a root step or the owner's
	// sign-in taking effect.
	arrive []func(*retireFake)
	// asked records the state queries, for the proof's since.
	minted int
}

func retireNewFake() *retireFake {
	return &retireFake{
		st:    console.SignInState{SignIn: config.ConsoleSignInKey, Key: true, RPID: "localhost", Origin: retireOrigin, Passkeys: 1},
		key:   retireOldKey,
		clock: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC),
		v4:    "inst-1", v6: "inst-1", pinOK: true,
	}
}

// signInOn, signIn and keyOut are what arrives while setup waits.
func (f *retireFake) signInOn()   { f.st.SignIn = config.ConsoleSignInPasskey }
func (f *retireFake) ownerSigns() { f.signIns = append(f.signIns, f.clock) }
func (f *retireFake) keyOut()     { f.key, f.st.Key = "", false }

func (f *retireFake) env(consolePath string) retireEnv {
	return retireEnv{
		state: func(since time.Time) (console.SignInState, error) {
			if f.down {
				return console.SignInState{}, errors.New("connect: no such file or directory")
			}
			st := f.st
			st.SignIns = 0
			if !since.IsZero() {
				for _, at := range f.signIns {
					if !at.Before(since) {
						st.SignIns++
					}
				}
			}
			return st, nil
		},
		pin: func() (access.DaemonPin, bool, error) {
			if f.pinErr != nil {
				return access.DaemonPin{}, true, f.pinErr
			}
			if !f.pinOK {
				return access.DaemonPin{}, false, nil
			}
			return access.DaemonPin{Name: "_example", UID: 480, Path: access.DaemonAccountPath}, true, nil
		},
		healthz: func(base string) string {
			if strings.Contains(base, "[::1]") {
				return f.v6
			}
			return f.v4
		},
		consolePath: consolePath,
		inventory:   func(string) []retireWired { return f.wired },
		keyAccepted: func(_, key string) bool { return f.key != "" && key == f.key },
		newKey: func() (string, error) {
			f.minted++
			return "fresh-key-from-setup", nil
		},
		now: func() time.Time { return f.clock },
		sleep: func(d time.Duration) {
			f.clock = f.clock.Add(d)
			if len(f.arrive) > 0 {
				next := f.arrive[0]
				f.arrive = f.arrive[1:]
				next(f)
			}
		},
		patience: time.Minute,
	}
}

// retireTestRun is a re-run at a terminal on the owner's socket config;
// answers are what the owner types.
func retireTestRun(t *testing.T, answers string) (*setupRun, *strings.Builder) {
	t.Helper()
	out := &strings.Builder{}
	return &setupRun{
		goos: "darwin", interactive: true, in: bufio.NewReader(strings.NewReader(answers)), out: out,
		home:  t.TempDir(),
		facts: setupFacts{configPath: "/h/.config/seamless/seamless.yaml", configFound: true, role: config.RoleClient, serverURL: "unix:///opt/seamless/run/seamlessd.sock"},
		loadConfig: func() (config.Config, error) {
			cfg := config.Defaults()
			cfg.Role, cfg.AdvertisedURL = config.RoleClient, "unix:///opt/seamless/run/seamlessd.sock"
			return cfg, nil
		},
		daemonAccount: func() string { return "_example" },
	}, out
}

// retireConsoleFile writes the guide's console.yaml, with a line the owner
// added, and returns its path.
func retireConsoleFile(t *testing.T, dir, key string) string {
	t.Helper()
	path := filepath.Join(dir, "console.yaml")
	body := "# the console's own config\nrole: client\nserver_url: \"http://127.0.0.1:8081\"\nmcp:\n  api_key: \"" + key + "\"\nconsole:\n  level: advanced\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func retireFileKey(t *testing.T, path string) string {
	t.Helper()
	cfg, err := config.LoadFrom(path)
	require.NoError(t, err)
	return cfg.MCP.APIKey
}

// The whole way: sign-in on, a passkey sign-in recorded, nothing else wired,
// root takes the key out, and console.yaml loses it -- in that order.
func TestRetire_TheWholeWay(t *testing.T) {
	r, out := retireTestRun(t, "")
	f := retireNewFake()
	f.arrive = []func(*retireFake){(*retireFake).signInOn, (*retireFake).ownerSigns, (*retireFake).keyOut}
	path := retireConsoleFile(t, t.TempDir(), retireOldKey)
	require.NoError(t, retireRun(r, &setupState{}, f.env(path)))

	got := out.String()
	for _, want := range []string{"_example (uid 480)", "127.0.0.1 and ::1 are both this daemon", "under console: add  sign_in: passkey",
		"/usr/bin/sudo /bin/launchctl kickstart -k system/" + systemDaemonLabel, retireOrigin + "/console/login", "a passkey signed in",
		"under mcp: delete the api_key line", "retired: TCP serves", "took the key out of", "/usr/bin/sudo -e /opt/seamless/etc/seamless.yaml",
		"/usr/bin/sudo -k -u _example /usr/bin/env SEAMLESS_CONFIG=/opt/seamless/etc/seamless.yaml /opt/seamless/bin/seamlessd console-open --sign-in --code <code>"} {
		require.Contains(t, got, want)
	}
	rootstepPATHFree(t, got)
	require.Less(t, strings.Index(got, "sign_in: passkey"), strings.Index(got, "a passkey signed in"))
	require.Less(t, strings.Index(got, "a passkey signed in"), strings.Index(got, "delete the api_key line"),
		"nothing is removed before the proof")
	require.NotContains(t, got, retireOldKey, "the key is never printed")

	require.Empty(t, retireFileKey(t, path), "console.yaml keeps no key")
	cfg, err := config.LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, retireOrigin, cfg.ServerURL(), "console-open opens the sign-in page where a passkey works")
	require.Equal(t, "advanced", cfg.Console.Level)
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" { // Windows keeps no Unix mode bits
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

// Every refusal stops before anything is removed: the daemon keeps its key
// and console.yaml its copy.
func TestRetire_RefusalsRemoveNothing(t *testing.T) {
	for _, tt := range []struct {
		name    string
		answers string
		arrange func(*retireFake, *setupRun)
		want    string
	}{
		{name: "the owner's config is not the socket", arrange: func(_ *retireFake, r *setupRun) {
			r.loadConfig = func() (config.Config, error) {
				cfg := config.Defaults()
				cfg.Role, cfg.AdvertisedURL, cfg.MCP.APIKey = config.RoleClient, "http://127.0.0.1:8081", retireOldKey
				return cfg, nil
			}
		}, want: "does not name the daemon's socket"},
		{name: "the daemon does not answer", arrange: func(f *retireFake, _ *setupRun) { f.down = true }, want: "did not answer on its socket"},
		{name: "no pin", arrange: func(f *retireFake, _ *setupRun) { f.pinOK = false }, want: "the daemon's account is not pinned"},
		{name: "a pin not believed", arrange: func(f *retireFake, _ *setupRun) { f.pinErr = errors.New("writable by uid 501") },
			want: "the daemon-account pin is there but not believed (writable by uid 501)"},
		{name: "no terminal to ask about what still dials", arrange: func(f *retireFake, r *setupRun) {
			r.interactive = false
			f.arrive = []func(*retireFake){(*retireFake).signInOn, (*retireFake).ownerSigns}
			f.wired = []retireWired{{"~/.codex/hooks.json", "Codex's hooks runs with console.yaml, which holds the key"}}
		}, want: "the key is kept while 1 still use it"},
		{name: "::1 is not this daemon", arrange: func(f *retireFake, _ *setupRun) { f.v6 = "" }, want: "not this daemon on both loopback addresses"},
		{name: "::1 is another daemon", arrange: func(f *retireFake, _ *setupRun) { f.v6 = "inst-other" }, want: "::1 answers instance inst-other"},
		{name: "root never turns sign-in on", want: "stopped waiting for the daemon to sign browsers in with a passkey"},
		{name: "no passkey is ever added", arrange: func(f *retireFake, _ *setupRun) {
			f.st.Passkeys = 0
			f.arrive = []func(*retireFake){(*retireFake).signInOn}
		}, want: "stopped waiting for a passkey to be added"},
		{name: "the owner never signs in", arrange: func(f *retireFake, _ *setupRun) {
			f.arrive = []func(*retireFake){(*retireFake).signInOn}
		}, want: "stopped waiting for a passkey sign-in"},
		{name: "something still dials with the key, and the owner keeps it", answers: "n\n", arrange: func(f *retireFake, _ *setupRun) {
			f.arrive = []func(*retireFake){(*retireFake).signInOn, (*retireFake).ownerSigns}
			f.wired = []retireWired{{"~/.claude.json", "Claude Code's MCP registration holds the key"}}
		}, want: "the key is kept while 1 still use it"},
		{name: "root never takes the key out", arrange: func(f *retireFake, _ *setupRun) {
			f.arrive = []func(*retireFake){(*retireFake).signInOn, (*retireFake).ownerSigns}
		}, want: "stopped waiting for the daemon to serve without the key"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, out := retireTestRun(t, tt.answers)
			f := retireNewFake()
			if tt.arrange != nil {
				tt.arrange(f, r)
			}
			path := retireConsoleFile(t, t.TempDir(), retireOldKey)
			err := retireRun(r, &setupState{}, f.env(path))
			require.Error(t, err)
			require.ErrorIs(t, err, errRetireStopped)
			require.Contains(t, err.Error(), tt.want)
			require.Contains(t, err.Error(), "run seamlessd setup again to continue from here")
			require.Equal(t, retireOldKey, retireFileKey(t, path), "console.yaml keeps the key")
			if f.key == retireOldKey {
				require.NotContains(t, out.String(), "took the key out")
			}
		})
	}
}

// A run that stopped anywhere continues from where things are: a crash after
// root took the key out finishes console.yaml without asking again, and one
// before asks for a fresh proof.
func TestRetire_ACrashBetweenStepsResumes(t *testing.T) {
	dir := t.TempDir()
	path := retireConsoleFile(t, dir, retireOldKey)
	f := retireNewFake()
	// Run 1: sign-in turned on, then the run dies waiting for the proof.
	f.arrive = []func(*retireFake){(*retireFake).signInOn}
	r, _ := retireTestRun(t, "")
	require.ErrorIs(t, retireRun(r, &setupState{}, f.env(path)), errRetireStopped)
	require.Equal(t, config.ConsoleSignInPasskey, f.st.SignIn)

	// Run 2: sign-in is on already, so it asks for the proof, and dies
	// waiting for root to take the key out.
	f.arrive = []func(*retireFake){(*retireFake).ownerSigns}
	r, out := retireTestRun(t, "")
	require.ErrorIs(t, retireRun(r, &setupState{}, f.env(path)), errRetireStopped)
	require.NotContains(t, out.String(), "add  sign_in: passkey", "a step done is not asked for again")
	require.Contains(t, out.String(), "a passkey signed in")
	require.Equal(t, retireOldKey, retireFileKey(t, path))

	// Root took the key out after run 2 gave up; run 3 finishes the file.
	f.keyOut()
	r, out = retireTestRun(t, "")
	require.NoError(t, retireRun(r, &setupState{}, f.env(path)))
	require.Contains(t, out.String(), "took the key out of")
	require.NotContains(t, out.String(), "sign in to the console with your passkey now", "no proof is asked once the key is gone")
	require.Empty(t, retireFileKey(t, path))

	// Run 4: nothing left to do but say so; no is the default.
	r, out = retireTestRun(t, "\n")
	require.NoError(t, retireRun(r, &setupState{}, f.env(path)))
	require.Contains(t, out.String(), "retired: the console signs a browser in with a passkey")
	require.Zero(t, f.minted)
}

// Bringing a key back mints a fresh one, never the old, writes it to
// console.yaml, and waits for root to give the daemon the same: the key never
// crosses the terminal.
func TestRetire_UndoMintsAFreshKey(t *testing.T) {
	dir := t.TempDir()
	path := retireConsoleFile(t, dir, retireOldKey)
	f := retireNewFake()
	f.signInOn()
	f.keyOut()
	_, err := retireDropConsoleKey(path, retireOrigin)
	require.NoError(t, err)

	f.arrive = []func(*retireFake){func(f *retireFake) { f.key, f.st.Key = "fresh-key-from-setup", true }}
	r, out := retireTestRun(t, "y\n")
	require.NoError(t, retireRun(r, &setupState{}, f.env(path)))
	require.Equal(t, 1, f.minted)
	require.Equal(t, "fresh-key-from-setup", retireFileKey(t, path))
	require.NotEqual(t, retireOldKey, retireFileKey(t, path), "the old key is never revived")
	require.NotContains(t, out.String(), "fresh-key-from-setup", "the key is never printed")
	require.Contains(t, out.String(), "under mcp: set  api_key:  to the mcp.api_key in "+path)
	cfg, err := config.LoadFrom(path)
	require.NoError(t, err)
	require.Equal(t, retireOrigin, cfg.ServerURL())
}

// Taking the key out keeps every other line as the owner wrote it, drops the
// emptied mcp block, and refuses a key it cannot find the guide's way.
func TestRetireWithoutKey(t *testing.T) {
	in := "# mine\nrole: client\nserver_url: \"https://box.example:8081\"\nmcp:\n  # the key\n  api_key: \"k\"\ntls:\n  ca_file: /h/ca.pem\nllm:\n  openai:\n    api_key: \"llm\"\n"
	got := string(retireWithoutKey([]byte(in)))
	require.Equal(t, "# mine\nrole: client\nserver_url: \"https://box.example:8081\"\ntls:\n  ca_file: /h/ca.pem\nllm:\n  openai:\n    api_key: \"llm\"\n", got,
		"the mcp block went with its key; another block's api_key stays")
	kept := string(retireWithoutKey([]byte("mcp:\n  api_key: \"k\"\n  serve_stateless: true\n")))
	require.Equal(t, "mcp:\n  serve_stateless: true\n", kept)

	path := filepath.Join(t.TempDir(), "console.yaml")
	require.NoError(t, os.WriteFile(path, []byte("role: client\nserver_url: \"http://127.0.0.1:8081\"\nmcp: {api_key: \"k\"}\n"), 0o600))
	_, err := retireDropConsoleKey(path, retireOrigin)
	require.ErrorIs(t, err, errRetireStopped, "a key written another way is the owner's to take out")
	require.Equal(t, "k", retireFileKey(t, path), "and the file is unchanged")
}

// The inventory finds the key, or the console's config, in the owner's agent
// clients and Seamless configs, and nothing in a home that holds neither.
func TestRetireScanHome(t *testing.T) {
	home := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(home, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	require.Empty(t, retireScanHome(home, "linux", retireOldKey))
	write(".claude.json", `{"mcpServers":{"seamless":{"type":"http","headers":{"Authorization":"Bearer `+retireOldKey+`"}}}}`)
	write(".codex/hooks.json", `{"hooks":{"x":[{"command":"seam hook x --config /h/.config/seamless/console.yaml"}]}}`)
	write(".config/seamless/seamless.yaml", "role: client\nserver_url: unix:///opt/seamless/run/seamlessd.sock\n")
	write(".config/seamless/console.yaml", "mcp:\n  api_key: "+retireOldKey+"\n")
	got := retireScanHome(home, "linux", retireOldKey)
	require.Len(t, got, 2, "%v", got)
	require.Contains(t, got[0].what+got[1].what, "Claude Code's MCP registration holds the key")
	require.Contains(t, got[0].what+got[1].what, "Codex's hooks runs with console.yaml")
}

// A run that stopped while root had not yet given the daemon the fresh key
// goes on waiting for it, and never takes the fresh key out as a retired
// key's copy; bringing a key back again replaces it with another fresh one.
func TestRetire_AnUndoThatStoppedResumes(t *testing.T) {
	dir := t.TempDir()
	path := retireConsoleFile(t, dir, retireOldKey)
	f := retireNewFake()
	f.signInOn()
	f.keyOut()
	_, err := retireDropConsoleKey(path, retireOrigin)
	require.NoError(t, err)

	r, _ := retireTestRun(t, "y\n")
	require.ErrorIs(t, retireRun(r, &setupState{}, f.env(path)), errRetireStopped, "root has not acted yet")
	require.Equal(t, "fresh-key-from-setup", retireFileKey(t, path))

	f.arrive = []func(*retireFake){func(f *retireFake) { f.key, f.st.Key = "fresh-key-from-setup", true }}
	r, out := retireTestRun(t, "")
	require.NoError(t, retireRun(r, &setupState{}, f.env(path)))
	require.Contains(t, out.String(), "a fresh key waits in")
	require.Equal(t, "fresh-key-from-setup", retireFileKey(t, path), "the fresh key stays")
	require.Equal(t, 1, f.minted, "no second key was minted")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(data), retireUndoMark))
}

// console.yaml moves from a loopback address to the passkey origin only when
// both name the same port on this machine; a TLS name stays as it is.
func TestRetireLoopbackOrigin(t *testing.T) {
	for _, tt := range []struct{ serverURL, origin, want string }{
		{"http://127.0.0.1:8081", "http://localhost:8081", "http://localhost:8081"},
		{"http://[::1]:8081", "http://localhost:8081/", "http://localhost:8081"},
		{"http://127.0.0.1:9000", "http://localhost:8081", ""},
		{"https://box.example:8081", "https://box.example:8081", ""},
		{"http://192.0.2.7:8081", "http://localhost:8081", ""},
		{"https://127.0.0.1:8081", "http://localhost:8081", ""},
	} {
		require.Equal(t, tt.want, retireLoopbackOrigin(tt.serverURL, tt.origin), "%s -> %s", tt.serverURL, tt.origin)
	}
}

// Doctor names a key console.yaml still keeps on the owner's socket config,
// and what else dials with it; nothing on any other config, or once retired.
func TestRetireDoctorChecks(t *testing.T) {
	home := t.TempDir()
	sock := config.Defaults()
	sock.Role, sock.AdvertisedURL = config.RoleClient, "unix:///opt/seamless/run/seamlessd.sock"
	require.Empty(t, retireDoctorChecks(sock, home, "linux"), "no console.yaml")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".config", "seamless"), 0o700))
	path := retireConsoleFile(t, filepath.Join(home, ".config", "seamless"), retireOldKey)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"k":"`+retireOldKey+`"}`), 0o600))
	got := retireDoctorChecks(sock, home, "linux")
	require.Len(t, got, 2)
	require.Equal(t, statusInfo, got[0].status)
	require.Contains(t, got[0].detail, "seamlessd setup retires it")
	require.Contains(t, got[1].detail, "Claude Code's MCP registration holds the key")
	require.Empty(t, retireDoctorChecks(config.Defaults(), home, "linux"), "a default install's report is unchanged")
	_, err := retireDropConsoleKey(path, retireOrigin)
	require.NoError(t, err)
	require.Empty(t, retireDoctorChecks(sock, home, "linux"), "nothing once retired")
}
