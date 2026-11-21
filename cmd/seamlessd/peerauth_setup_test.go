package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/releasesig"
	"github.com/arctop/seamless/internal/shellcmd"
	"github.com/arctop/seamless/internal/update"
)

// pasFake is a machine for seamlessd peer-auth setup: nothing it is asked to
// run runs, but every block is kept, the state file's appends land in
// memory, and the world it reports is the test's to change.
type pasFake struct {
	t       *testing.T
	h       *pasHost
	scripts []string
	state   []string
	files   map[string]fileFacts
	users   map[string][2]int
	groups  map[string]int
	loaded  map[string]bool
	owner   [][]string
	// ownerEnv is the environment each runOwner call added.
	ownerEnv [][]string
	failOn   string
	pin      access.DaemonPin
	pinned   bool
	fetchErr error
	// ownerSig is the release's checksums.txt.sshsig, nil for none;
	// fetchedOwnerSig records whether setup asked for it.
	ownerSig        []byte
	fetchedOwnerSig bool
	team            string
	cfgPath         string
	dataDir         string
}

// pasTestTeam is a made-up Developer ID team.
const pasTestTeam = "ABCDE12345"

// pasTestChecksums is the checksums.txt the fake release's binaries were
// checked against, which the owner's signature covers.
const pasTestChecksums = "0123abcd  seamless_0.10.4_darwin_arm64.tar.gz\n"

var pasAppendRe = regexp.MustCompile(`^printf '%s\\n' (.*) \| /usr/bin/sudo /usr/bin/tee -a ` + regexp.QuoteMeta(pasStatePath) + ` >/dev/null( &&)?$`)

func newPasFake(t *testing.T, goos string) *pasFake {
	t.Helper()
	if runtime.GOOS == "windows" {
		// The fake is a darwin or linux host: its /opt/seamless paths and
		// unix:// socket are not absolute to the config's checks on Windows.
		t.Skip("peer_auth is a POSIX deployment: the fake host's /opt/seamless paths do not validate on Windows")
	}
	home := t.TempDir()
	cfgDir := filepath.Join(home, ".config", "seamless")
	require.NoError(t, os.MkdirAll(cfgDir, 0o700))
	f := &pasFake{t: t, files: map[string]fileFacts{}, users: map[string][2]int{}, groups: map[string]int{},
		loaded: map[string]bool{}, team: pasTestTeam, cfgPath: filepath.Join(cfgDir, "seamless.yaml"),
		dataDir: filepath.Join(home, ".seamless")}
	require.NoError(t, os.MkdirAll(f.dataDir, 0o700))
	require.NoError(t, os.WriteFile(f.cfgPath, []byte("# my server\ndata_dir: "+f.dataDir+"\naddr: \"127.0.0.1:8081\"\n"+
		"mcp:\n  api_key: \"k-123\"\nllm:\n  provider: \"openai\"\n  openai:\n    api_key: \"sk-test\" # my key\n"), 0o600))
	f.files[f.dataDir] = fileFacts{uid: 501, isDir: true, mode: fs.ModeDir | 0o700}
	if goos == "darwin" {
		f.files[launchdPlist(home, launchdLabel)] = fileFacts{uid: 501, mode: 0o644}
		f.loaded["gui/501/"+launchdLabel] = true
	} else {
		f.files[filepath.Join(home, ".config", "systemd", "user", systemdUnit)] = fileFacts{uid: 501, mode: 0o644}
	}
	h := &pasHost{goos: goos, goarch: "arm64", euid: 501,
		owner: pasHostOwner{name: "alice", uid: 501, group: "staff", home: home},
		in:    bufio.NewReader(strings.NewReader("")), out: &strings.Builder{},
		now: func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) },
		exe: "/Users/alice/.local/bin/seamlessd", version: "0.10.4", distribution: update.DistributionRelease,
		wait: time.Millisecond, reachable: func(string) bool { return false },
		tempDir: func() (string, error) { return os.MkdirTemp(t.TempDir(), "run-") },
	}
	h.world = pasWorld{
		config: func() (config.Config, []byte, error) {
			cfg, err := config.LoadFrom(f.cfgPath)
			if err != nil {
				return cfg, nil, err
			}
			raw, err := os.ReadFile(f.cfgPath)
			return cfg, raw, err
		},
		inspect: func(p string) (fileFacts, error) {
			if ff, ok := f.files[p]; ok {
				return ff, nil
			}
			return fileFacts{}, fs.ErrNotExist
		},
		lookupUser: func(name string) (int, int, bool) {
			u, ok := f.users[name]
			return u[0], u[1], ok
		},
		lookupGroup: func(name string) (int, bool) {
			g, ok := f.groups[name]
			return g, ok
		},
		takenIDs:      func() (map[int]bool, error) { return map[int]bool{480: true, 478: true}, nil },
		systemAccount: func(uid uint32) (bool, string) { return uid < 500, "uid 500 and up are people's" },
		pin:           func() (access.DaemonPin, bool, error) { return f.pin, f.pinned, nil },
		launchdLoaded: func(target string) bool { return f.loaded[target] },
	}
	h.shell = func(_ context.Context, script string) error {
		f.scripts = append(f.scripts, script)
		if f.failOn != "" && strings.Contains(script, f.failOn) {
			return errors.New("exit status 1")
		}
		for _, line := range strings.Split(script, "\n") {
			if m := pasAppendRe.FindStringSubmatch(line); m != nil {
				out, err := exec.Command("/bin/sh", "-c", "printf %s "+m[1]).Output()
				require.NoError(t, err)
				f.state = append(f.state, string(out))
			}
		}
		return nil
	}
	h.readFile = func(p string) ([]byte, error) {
		if p == pasStatePath {
			if len(f.state) == 0 {
				return nil, fs.ErrNotExist
			}
			return []byte(strings.Join(f.state, "\n") + "\n"), nil
		}
		return os.ReadFile(p)
	}
	h.fetch = func(_ context.Context, _ update.Version, ownerSig bool) (pasRelease, error) {
		if f.fetchErr != nil {
			return pasRelease{}, f.fetchErr
		}
		f.fetchedOwnerSig = ownerSig
		rel := pasRelease{bins: map[string][]byte{"seam": []byte("seam of the release"), "seamlessd": []byte("seamlessd of the release")},
			checksums: []byte(pasTestChecksums)}
		if ownerSig {
			rel.ownerSig = f.ownerSig
		}
		return rel, nil
	}
	h.readSigners = func(p string) ([]byte, error) {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if _, err := releasesig.ParseOwnerSigners(data); err != nil {
			return nil, err
		}
		return data, nil
	}
	h.team = func(context.Context, string) string { return f.team }
	h.health = func(context.Context) error { return nil }
	h.runOwner = func(_ context.Context, argv []string, env []string) error {
		f.owner = append(f.owner, argv)
		f.ownerEnv = append(f.ownerEnv, env)
		return nil
	}
	// The owner's Claude Code is wired; nothing else is.
	h.wiredClients = func(config.Config, string, string) ([]string, []string, []string) {
		return []string{shapeClaude}, nil, nil
	}
	f.h = h
	return f
}

// out is everything the run printed.
func (f *pasFake) out() string { return f.h.out.(*strings.Builder).String() }

// all is every block run, joined.
func (f *pasFake) all() string { return strings.Join(f.scripts, "\n---\n") }

// st reads the fake state file.
func (f *pasFake) st() pasState {
	st, err := f.h.state()
	require.NoError(f.t, err)
	return st
}

// moved makes the world look as it does once every step of the move ran.
func (f *pasFake) moved() {
	delete(f.files, f.dataDir)
	f.files[pasData] = fileFacts{uid: 479, isDir: true, mode: fs.ModeDir | 0o700}
	home := f.h.owner.home
	if f.h.goos == "darwin" {
		plist := launchdPlist(home, launchdLabel)
		delete(f.files, plist)
		f.files[plist+pasKeptSuffix] = fileFacts{uid: 501, mode: 0o644}
		delete(f.loaded, "gui/501/"+launchdLabel)
	} else {
		f.files[pasDaemonUnitPath()] = fileFacts{mode: 0o644}
	}
}

func TestPeerAuthSetup_RefusesWindowsAndRoot(t *testing.T) {
	f := newPasFake(t, "darwin")
	f.h.goos = "windows"
	err := pasSetup(context.Background(), f.h, pasOptions{yes: true})
	require.ErrorIs(t, err, errPasRefused)
	require.ErrorContains(t, err, "Windows has no Unix-socket peer credentials")
	require.ErrorContains(t, pasUndo(context.Background(), f.h, true, false), "Windows")

	f = newPasFake(t, "linux")
	f.h.euid = 0
	require.ErrorContains(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}), "not as root")
	require.Empty(t, f.scripts)
}

// The whole move, on each system: every step in the guide's order, each
// recorded as done, and the owner's side written.
func TestPeerAuthSetup_RunsEveryStepInOrder(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			f := newPasFake(t, goos)
			require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
			st := f.st()
			require.Equal(t, pasDone, st.phase(), f.out())
			require.Equal(t, "alice", st.plan.Owner)
			require.Equal(t, "0.10.4", st.plan.Release)
			require.Equal(t, []string{pasServiceNames(goos)[0]}, st.plan.Services)
			if goos == "darwin" {
				require.Equal(t, 479, st.plan.UID, "480 and 478 are taken: the first id free as a user id and a group id")
				require.Equal(t, pasTestTeam, st.plan.Team)
			} else {
				require.Zero(t, st.plan.UID)
			}

			// Every block, in order: the first carries the plan.
			markers := []string{"tee -a " + pasStatePath, "export -o -", "/opt/seamless/stage", "seamless.yaml.new",
				"--now", "/opt/seamless/data", systemDaemonLabel, "seamlessd doctor"}
			if goos == "linux" {
				markers[4], markers[6] = "systemctl --user disable --now", "enable --now seamless-daemon"
			} else {
				markers[4] = "bootout gui/501/" + launchdLabel
			}
			all := f.all()
			at := -1
			for _, m := range markers {
				i := strings.Index(all, m)
				require.Greater(t, i, at, "%q comes after the step before it:\n%s", m, all)
				at = i
			}
			require.Contains(t, f.scripts[0], `"owner":"alice"`)
			if goos == "darwin" {
				require.Contains(t, all, "/usr/bin/dscl . -create /Users/_seamless UniqueID 479")
				require.Contains(t, all, "/usr/bin/codesign --verify --strict --check-notarization")
				require.Contains(t, all, `leaf[subject.OU] = "`+pasTestTeam+`" and notarized`)
				require.Contains(t, all, "/usr/bin/sudo /bin/launchctl bootstrap system "+pasDaemonPlistPath())
				require.Contains(t, all, "/bin/mv "+launchdPlist(f.h.owner.home, launchdLabel)+" "+launchdPlist(f.h.owner.home, launchdLabel)+pasKeptSuffix)
			} else {
				require.Contains(t, all, "/usr/sbin/useradd --system --user-group --shell /usr/sbin/nologin --home-dir /opt/seamless/home _seamless")
				require.Contains(t, all, "/bin/loginctl enable-linger _seamless", "the updater runs in the account's lingering user manager")
				require.NotContains(t, all, "codesign")
			}
			require.Contains(t, all, "-o _seamless -g _seamless -m 0755 "+pasBin, "the daemon's account holds the bin directory: it updates itself")

			// The owner's side: the server config kept whole, the client's in its place.
			kept, err := os.ReadFile(f.cfgPath + pasKeptSuffix)
			require.NoError(t, err)
			require.Contains(t, string(kept), "# my key")
			cli, err := config.LoadFrom(f.cfgPath)
			require.NoError(t, err)
			require.True(t, cli.IsClient())
			require.Equal(t, pasSocket, cli.SocketPath())
			require.Empty(t, cli.MCP.APIKey)
			con, err := config.LoadFrom(filepath.Join(filepath.Dir(f.cfgPath), "console.yaml"))
			require.NoError(t, err)
			require.Equal(t, "k-123", con.MCP.APIKey)
			require.Equal(t, "http://127.0.0.1:8081", strings.TrimSuffix(con.AdvertisedURL, "/"))
			info, err := os.Stat(filepath.Join(filepath.Dir(f.cfgPath), "console.yaml"))
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			require.Equal(t, [][]string{{f.h.exe, "install-hooks", "--config", f.cfgPath, "--client", "claude", "--seam", pasBin + "/seam"}}, f.owner)

			// A run again has nothing left to do.
			n := len(f.scripts)
			require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
			require.Len(t, f.scripts, n)
			require.Contains(t, f.out(), "Locked projects are on")
		})
	}
}

// A run that stops continues at the step that stopped: no new plan, nothing
// done before run again.
func TestPeerAuthSetup_ResumesAtTheStepThatStopped(t *testing.T) {
	f := newPasFake(t, "darwin")
	f.failOn = "/usr/bin/sudo /bin/mv " + f.dataDir
	err := pasSetup(context.Background(), f.h, pasOptions{yes: true})
	require.ErrorIs(t, err, errPasRefused)
	require.ErrorContains(t, err, "step 6 (data) stopped")
	require.ErrorContains(t, err, "continues from this step")
	st := f.st()
	require.Equal(t, pasInProgress, st.phase())
	require.Equal(t, "data", st.next().id)
	require.Contains(t, pasStatusLine(st), "5 of 11 steps done, next data")

	f.failOn = ""
	n := len(f.scripts)
	require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
	again := strings.Join(f.scripts[n:], "\n")
	require.NotContains(t, again, "export -o -", "the backup ran already")
	require.NotContains(t, again, "dscl", "the account is there")
	require.Contains(t, f.scripts[n], "/usr/bin/sudo /bin/mv "+f.dataDir+" "+pasData, "the stopped step is the first to run")
	plans := 0
	for _, l := range f.state {
		if strings.HasPrefix(l, pasLinePlan+" ") {
			plans++
		}
	}
	require.Equal(t, 1, plans, "a resumed run plans nothing again")
	require.Equal(t, pasDone, f.st().phase())
}

// A move under way continues with the plan it began with: a flag that would
// change it is refused, not silently dropped.
func TestPeerAuthSetup_ResumeRefusesANewPlansFlags(t *testing.T) {
	f := newPasFake(t, "darwin")
	f.failOn = "export -o -"
	require.Error(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
	f.failOn = ""
	for _, o := range []pasOptions{{yes: true, release: "0.10.3"}, {yes: true, rootOwned: true}, {yes: true, sourceBuild: true}} {
		require.ErrorContains(t, pasSetup(context.Background(), f.h, o), "continues with the plan it began with")
	}
}

// Status prints each step, and where the move stands.
func TestPeerAuthStatus_PrintsEachStep(t *testing.T) {
	f := newPasFake(t, "linux")
	require.NoError(t, pasStatusCmd(f.h, nil))
	require.Contains(t, f.out(), "no move to peer_auth recorded")
	f.failOn = "/usr/sbin/useradd"
	require.Error(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
	f.h.out = &strings.Builder{}
	require.NoError(t, pasStatusCmd(f.h, nil))
	out := f.out()
	require.Contains(t, out, "in progress: 1 of 11 steps done, next account")
	require.Regexp(t, `1\. backup\s+done 2026-11-20T12:00:00Z`, out)
	require.Regexp(t, `2\. account\s+pending`, out)
}

// An undo that stopped after the data moved back and before the chown hands
// the data to the owner when it runs again: the data is back already, and
// still the daemon's account's.
func TestPeerAuthUndo_RerunAfterTheMoveStillHandsTheDataBack(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			f := newPasFake(t, goos)
			require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
			f.moved()
			f.h.health = func(context.Context) error { return errors.New("connection refused") }
			f.failOn = "chown -R alice:staff " + f.dataDir
			require.Error(t, pasUndo(context.Background(), f.h, true, false))

			// The move itself happened: the data is at the old path, the account's.
			delete(f.files, pasData)
			f.files[f.dataDir] = fileFacts{uid: 479, isDir: true, mode: fs.ModeDir | 0o700}
			f.failOn = ""
			n := len(f.scripts)
			require.NoError(t, pasUndo(context.Background(), f.h, true, false))
			again := strings.Join(f.scripts[n:], "\n---\n")
			require.NotContains(t, again, "/bin/mv "+pasData, "nothing left to move")
			require.Contains(t, again, "-f "+filepath.Join(f.dataDir, "seam.db"))
			require.Contains(t, again, "chown -R alice:staff "+f.dataDir, "the data is handed back all the same")
		})
	}
}

// Undo puts the hooks back on the seam they ran before the move, wherever the
// seamlessd that runs undo lives.
func TestPeerAuthUndo_HooksGoBackToTheirOldSeam(t *testing.T) {
	f := newPasFake(t, "darwin")
	old := filepath.Join(f.h.owner.home, ".local", "bin", "seam")
	f.h.hookSeam = func() string { return old }
	require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
	require.Equal(t, old, f.st().plan.OldSeam)
	f.moved()
	f.h.health = func(context.Context) error { return errors.New("connection refused") }
	f.owner = nil
	require.NoError(t, pasUndo(context.Background(), f.h, true, false))
	require.Equal(t, [][]string{{f.h.exe, "install-hooks", "--config", f.cfgPath, "--client", "claude", "--seam", old}}, f.owner)

	// A seam under the layout already (a move made by hand) is not recorded.
	g := newPasFake(t, "linux")
	g.h.hookSeam = func() string { return pasBin + "/seam" }
	require.NoError(t, pasSetup(context.Background(), g.h, pasOptions{yes: true}))
	require.Empty(t, g.st().plan.OldSeam)
}

// The hooks step re-points the clients wired before the move and no other,
// keeps out the skills they lacked, and undo does the same; with no client
// wired it reinstalls nothing.
func TestPeerAuthSetup_HooksKeepTheWiredClients(t *testing.T) {
	f := newPasFake(t, "linux")
	var gotSeamDir, gotConfig string
	f.h.wiredClients = func(_ config.Config, configPath, seamDir string) ([]string, []string, []string) {
		gotConfig, gotSeamDir = configPath, seamDir
		return []string{shapeClaude, shapeCodex}, []string{"seam-research"}, []string{"Claude app left out: a note"}
	}
	require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
	require.Equal(t, f.cfgPath, gotConfig)
	require.Equal(t, filepath.Dir(f.h.exe), gotSeamDir, "with no recorded seam, the one beside this seamlessd")
	plan := f.st().plan
	require.Equal(t, []string{shapeClaude, shapeCodex}, plan.Clients)
	require.Equal(t, []string{"seam-research"}, plan.SkipSkills)
	require.Contains(t, f.out(), "Claude app left out: a note")
	want := []string{f.h.exe, "install-hooks", "--config", f.cfgPath, "--client", "claude,codex", "--seam", pasBin + "/seam"}
	require.Equal(t, [][]string{want}, f.owner)
	require.Equal(t, [][]string{{"SEAMLESS_NO_RESEARCH_SKILL=1"}}, f.ownerEnv)

	f.moved()
	f.h.health = func(context.Context) error { return errors.New("connection refused") }
	f.owner, f.ownerEnv = nil, nil
	require.NoError(t, pasUndo(context.Background(), f.h, true, false))
	require.Equal(t, [][]string{{f.h.exe, "install-hooks", "--config", f.cfgPath, "--client", "claude,codex"}}, f.owner)
	require.Equal(t, [][]string{{"SEAMLESS_NO_RESEARCH_SKILL=1"}}, f.ownerEnv)

	g := newPasFake(t, "darwin")
	g.h.wiredClients = func(config.Config, string, string) ([]string, []string, []string) { return nil, nil, nil }
	require.NoError(t, pasSetup(context.Background(), g.h, pasOptions{dryRun: true}))
	require.Contains(t, g.out(), "no agent client is wired to Seamless here, so the move re-points none")
	require.Contains(t, g.out(), "# then, in this process: nothing: no agent client was wired to Seamless before the move")
	require.NoError(t, pasSetup(context.Background(), g.h, pasOptions{yes: true}))
	require.Empty(t, g.owner, "no client was wired, so none is")
	require.Contains(t, g.out(), "nothing to reinstall")
}

// A plan naming a client install-hooks does not wire, or a skill it cannot
// leave out, is refused.
func TestPasPlanProblem_ClientsAndSkills(t *testing.T) {
	f := newPasFake(t, "linux")
	require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{dryRun: true}))
	plan, _, err := pasPreflight(context.Background(), f.h, pasOptions{})
	require.NoError(t, err)
	plan.Release = "0.10.4"
	require.Empty(t, plan.problem())
	bad := plan
	bad.Clients = []string{"claude", "detect"}
	require.Contains(t, bad.problem(), `"detect" is not an agent client install-hooks wires`)
	bad = plan
	bad.SkipSkills = []string{"seam-nonesuch"}
	require.Contains(t, bad.problem(), "seam-nonesuch")
}

// The per-user install's own update is waited out before its service stops:
// an update running while the data moves failed its backup, re-created the
// data directory for its record, and its rollback would bring the per-user
// daemon back (caught live on 2026-11-20).
func TestPeerAuthSetup_WaitsOutAPerUserUpdate(t *testing.T) {
	f := newPasFake(t, "darwin")
	polls := 0
	f.h.updaterBusy = func(dir string) (bool, error) {
		require.Equal(t, f.dataDir, dir)
		polls++
		return polls <= 3, nil // an update finishing
	}
	require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
	require.Greater(t, polls, 3, "the retire step polled the lock until it was free")
	require.Equal(t, 1, strings.Count(f.out(), "the per-user install is updating itself; waiting"), "said once, not per poll")
	require.Equal(t, pasLineDone, f.st().status["retire"])

	// One that never finishes stops the move before the service stops.
	g := newPasFake(t, "darwin")
	g.h.updaterBusy = func(string) (bool, error) { return true, nil }
	err := pasSetup(context.Background(), g.h, pasOptions{yes: true})
	require.ErrorContains(t, err, "an update of the per-user install is still running")
	require.NotContains(t, g.all(), "bootout gui/501/"+launchdLabel)
	require.NotEqual(t, pasLineDone, g.st().status["retire"])
	require.NotEqual(t, pasLineDone, g.st().status["data"])

	// The dry run says so.
	h := newPasFake(t, "darwin")
	require.NoError(t, pasSetup(context.Background(), h.h, pasOptions{dryRun: true}))
	require.Contains(t, h.out(), "# first, in this process: wait until no update of the per-user install runs")
}

// A plist the per-user install's update wrote back beside the one an earlier
// run set aside is the one kept: it matches the binaries now on disk.
func TestPeerAuthSetup_RetireKeepsAPlistAnUpdateWroteBack(t *testing.T) {
	f := newPasFake(t, "darwin")
	plist := launchdPlist(f.h.owner.home, launchdLabel)
	f.files[plist+pasKeptSuffix] = fileFacts{uid: 501, mode: 0o644}
	require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
	require.Contains(t, f.all(), "/bin/mv -f "+plist+" "+plist+pasKeptSuffix)
}

// Undo waits out an update the daemon runs before its service stops.
func TestPeerAuthUndo_WaitsOutTheDaemonsUpdate(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			f := newPasFake(t, goos)
			require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
			f.moved()
			f.h.health = func(context.Context) error { return errors.New("connection refused") }
			n := len(f.scripts)
			require.NoError(t, pasUndo(context.Background(), f.h, true, false))
			undo := strings.Join(f.scripts[n:], "\n---\n")
			wait := strings.Index(undo, "/usr/bin/pgrep -u _seamless -f 'seamlessd update'")
			stop := strings.Index(undo, "bootout system/"+systemDaemonLabel)
			if goos == "linux" {
				stop = strings.Index(undo, "disable --now seamless-daemon")
			}
			require.GreaterOrEqual(t, wait, 0, undo)
			require.Less(t, wait, stop, "the wait comes before the service stops")
		})
	}
}

// Undo waits for the per-user daemon it started again, and says where to look
// when it does not answer. On macOS the address stays the other account's for
// up to a minute after the system daemon stops (TIME_WAIT is kept per
// account), so the restored daemon's first binds fail (seen live 2026-11-20).
func TestPeerAuthUndo_WaitsForThePerUserDaemonToAnswer(t *testing.T) {
	f := newPasFake(t, "darwin")
	require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
	f.moved()
	f.h.health = func(context.Context) error { return errors.New("connection refused") }
	probes := 0
	f.h.reachable = func(string) bool { probes++; return probes > 7 }
	require.NoError(t, pasUndo(context.Background(), f.h, true, false))
	require.Equal(t, 1, strings.Count(f.out(), "macOS keeps 127.0.0.1:8081 for the connections the other account's daemon had"), f.out())
	require.NotContains(t, f.out(), "does not answer at")

	g := newPasFake(t, "linux")
	require.NoError(t, pasSetup(context.Background(), g.h, pasOptions{yes: true}))
	g.moved()
	g.h.health = func(context.Context) error { return errors.New("connection refused") }
	require.NoError(t, pasUndo(context.Background(), g.h, true, false), "the undo ran; a daemon slow to answer does not fail it")
	require.Contains(t, g.out(), "the per-user daemon does not answer at 127.0.0.1:8081 yet: systemctl --user status seamless.service")
}

// The per-user service starts again only once the server config is back: on
// the client config the daemon refuses to serve and crash-loops.
func TestPeerAuthUndo_RestoresTheServerConfigBeforeThePerUserService(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			f := newPasFake(t, goos)
			require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
			f.moved()
			f.h.health = func(context.Context) error { return errors.New("connection refused") }
			start := "/bin/launchctl bootstrap gui/501 "
			if goos == "linux" {
				start = "systemctl --user enable --now " + systemdUnit
			}
			shell, checked := f.h.shell, false
			f.h.shell = func(ctx context.Context, script string) error {
				if strings.Contains(script, start) {
					cfg, err := config.LoadFrom(f.cfgPath)
					require.NoError(t, err)
					require.False(t, cfg.IsClient(), "the per-user service starts on the server config")
					checked = true
				}
				return shell(ctx, script)
			}
			require.NoError(t, pasUndo(context.Background(), f.h, true, false))
			require.True(t, checked, "the per-user service was started")
		})
	}
}

// Another account cannot continue the owner's move.
func TestPeerAuthSetup_ResumeIsTheOwnersOnly(t *testing.T) {
	f := newPasFake(t, "linux")
	f.failOn = "export -o -"
	require.Error(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
	f.failOn = ""
	f.h.owner = pasHostOwner{name: "bob", uid: 502, group: "staff", home: f.h.owner.home}
	require.ErrorContains(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}), "the move is alice's (uid 501)")
}

// Undo reverses what ran, in the guide's order, and keeps the rest.
func TestPeerAuthUndo_ReversesInTheGuidesOrder(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			f := newPasFake(t, goos)
			require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
			f.moved()
			calls := 0
			f.h.health = func(context.Context) error {
				calls++
				return errors.New("connection refused") // the daemon stopped
			}
			n := len(f.scripts)
			f.owner = nil
			require.NoError(t, pasUndo(context.Background(), f.h, true, false))
			undo := strings.Join(f.scripts[n:], "\n---\n")
			order := []string{"bootout system/" + systemDaemonLabel, "/usr/bin/sudo /bin/mv " + pasData + " " + f.dataDir}
			if goos == "linux" {
				order = []string{"disable --now seamless-daemon", "/usr/bin/sudo /bin/mv " + pasData + " " + f.dataDir,
					"systemctl --user enable --now " + systemdUnit}
			} else {
				order = append(order, "/bin/launchctl bootstrap gui/501 "+launchdPlist(f.h.owner.home, launchdLabel))
			}
			at := -1
			for _, m := range order {
				i := strings.Index(undo, m)
				require.Greater(t, i, at, "%q:\n%s", m, undo)
				at = i
			}
			// Root makes the check: until the chown the directory is still the
			// daemon's account's, 0700, and the owner's shell cannot see into
			// it (caught live on 2026-11-20, macOS 26).
			require.Contains(t, undo, "/usr/bin/sudo "+shellcmd.ToolsFor(goos).Test+" -f "+filepath.Join(f.dataDir, "seam.db"),
				"the data came back whole, not into a directory made meanwhile")
			require.NotContains(t, undo, "[ -e "+filepath.Join(f.dataDir, "seam.db")+" ]")
			require.Contains(t, undo, "chown -R alice:staff "+f.dataDir)
			require.NotContains(t, undo, "userdel")
			require.NotContains(t, undo, "dscl . -delete")

			// The server config is back; the hooks run against it.
			cfg, err := config.LoadFrom(f.cfgPath)
			require.NoError(t, err)
			require.False(t, cfg.IsClient())
			require.Equal(t, [][]string{{f.h.exe, "install-hooks", "--config", f.cfgPath, "--client", "claude"}}, f.owner)
			st := f.st()
			require.Equal(t, pasRolledBack, st.phase())
			require.Contains(t, pasStatusLine(st), "rolled back")

			// A setup after the undo plans afresh.
			require.NoError(t, pasUndo(context.Background(), f.h, true, false))
			require.Contains(t, f.out(), "Nothing to undo")
		})
	}
}

// The trust anchor: a source build installs nothing unless asked, a release
// that does not check out stops the binaries step, and a macOS release whose
// team cannot be named is refused.
func TestPeerAuthSetup_TrustAnchor(t *testing.T) {
	f := newPasFake(t, "darwin")
	f.h.distribution, f.h.version = update.DistributionSource, "0.0.0-dev"
	err := pasSetup(context.Background(), f.h, pasOptions{yes: true})
	require.ErrorContains(t, err, "this seamlessd is a source build")
	require.ErrorContains(t, err, "--release X.Y.Z")
	require.ErrorContains(t, err, "--source-build")
	require.Empty(t, f.scripts, "nothing runs")

	// A release download that does not verify stops the step before root
	// copies anything.
	f = newPasFake(t, "linux")
	f.fetchErr = errors.New("checksums.txt of v0.10.4: signature does not verify")
	err = pasSetup(context.Background(), f.h, pasOptions{yes: true})
	require.ErrorContains(t, err, "step 3 (binaries) stopped")
	require.ErrorContains(t, err, "signature does not verify")
	require.NotContains(t, f.all(), pasStage+"/seam")
	require.Equal(t, "binaries", f.st().next().id)

	// A source build installing a release: held to the team that signed it.
	f = newPasFake(t, "darwin")
	f.h.distribution, f.h.version = update.DistributionSource, "0.0.0-dev"
	f.team = ""
	err = pasSetup(context.Background(), f.h, pasOptions{yes: true, release: "0.10.4"})
	require.ErrorContains(t, err, "release v0.10.4's seamlessd names no Developer ID team")

	// The check comes after the hash and before the copy is anyone's but
	// root's, for each binary, seam first.
	f = newPasFake(t, "darwin")
	x := &pasRun{ctx: context.Background(), h: f.h, plan: pasTestPlan(t, f), t: shellcmd.ToolsFor("darwin")}
	x.tmp = t.TempDir()
	cmds, err := pasBinaryCommands(x)
	require.NoError(t, err)
	var seen []string
	for _, c := range cmds {
		switch {
		case strings.Contains(c, "shasum -a 256 -c"):
			seen = append(seen, "hash")
		case strings.Contains(c, "codesign"):
			seen = append(seen, "codesign")
		case strings.Contains(c, "/usr/sbin/chown"):
			seen = append(seen, "chown")
		case strings.Contains(c, "/bin/mv -f "+pasStage+"/seam "+pasBin+"/seam"):
			seen = append(seen, "seam in place")
		case strings.Contains(c, "/bin/mv -f "+pasStage+"/seamlessd "+pasBin+"/seamlessd"):
			seen = append(seen, "seamlessd in place")
		}
	}
	require.Equal(t, []string{"hash", "codesign", "chown", "seam in place", "hash", "codesign", "chown", "seamlessd in place"}, seen)

	// A source build, asked for: the hash alone, and said so.
	f = newPasFake(t, "darwin")
	src := t.TempDir()
	for _, name := range systemBinaries {
		require.NoError(t, os.WriteFile(filepath.Join(src, name), []byte(name+" built here"), 0o755))
		f.files[filepath.Join(src, name)] = fileFacts{uid: 501, mode: 0o755}
	}
	f.h.exe = filepath.Join(src, "seamlessd")
	f.h.distribution, f.h.version = update.DistributionSource, "0.0.0-dev"
	require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{yes: true, sourceBuild: true}))
	require.NotContains(t, f.all(), "codesign")
	require.NotContains(t, f.all(), "enable-linger")
	require.Contains(t, f.out(), "no signature checks them, and the daemon never updates itself")
	require.Equal(t, src, f.st().plan.Source)
}

// pasTestPlan is the plan the fake machine's preflight makes.
func pasTestPlan(t *testing.T, f *pasFake) pasPlan {
	t.Helper()
	p, _, err := pasPreflight(context.Background(), f.h, pasOptions{})
	require.NoError(t, err)
	return p
}

// What setup cannot move it refuses before anything runs, saying why.
func TestPeerAuthSetup_RefusesWhatItCannotMove(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*pasFake)
		want  string
	}{
		{"client config", func(f *pasFake) {
			require.NoError(t, os.WriteFile(f.cfgPath, []byte("role: client\nserver_url: \"http://box:8081\"\nmcp:\n  api_key: \"k\"\n"), 0o600))
		}, "is a client's config"},
		{"moved by hand", func(f *pasFake) {
			require.NoError(t, os.WriteFile(f.cfgPath, []byte("role: client\nserver_url: \"unix://"+pasSocket+"\"\n"), 0o600))
			f.files[pasConfig] = fileFacts{mode: 0o640}
		}, "peer_auth is on here already"},
		{"peer_auth on", func(f *pasFake) {
			raw, _ := os.ReadFile(f.cfgPath)
			require.NoError(t, os.WriteFile(f.cfgPath, append(raw, []byte("peer_auth:\n  enabled: true\n  owner: alice\n  socket:\n    path: /tmp/x.sock\n")...), 0o600))
		}, "peer_auth is on"},
		{"key only in the environment", func(f *pasFake) {
			require.NoError(t, os.WriteFile(f.cfgPath, []byte("data_dir: "+f.dataDir+"\n"), 0o600))
		}, "holds no mcp.api_key"},
		{"data not the owner's", func(f *pasFake) { f.files[f.dataDir] = fileFacts{uid: 502, isDir: true} }, "is not yours"},
		{"root not root's", func(f *pasFake) { f.files[pasRoot] = fileFacts{uid: 501, isDir: true, mode: fs.ModeDir | 0o755} }, "only root can write"},
		{"root writable by others", func(f *pasFake) { f.files[pasRoot] = fileFacts{uid: 0, isDir: true, mode: fs.ModeDir | 0o777} }, "only root can write"},
		{"a relay's computer", func(f *pasFake) { f.files[pasRoot+"/relay"] = fileFacts{uid: 479, isDir: true} }, "runs the relay"},
		{"data there", func(f *pasFake) { f.files[pasData] = fileFacts{uid: 479, isDir: true} }, pasData + " is there already"},
		{"a LaunchDaemon there", func(f *pasFake) { f.files[pasDaemonPlistPath()] = fileFacts{} }, "a system service for the daemon is installed"},
		{"a person's account", func(f *pasFake) { f.users[pasAccount] = [2]int{503, 20}; f.groups[pasAccount] = 503 }, "is not a system account"},
		{"a group alone", func(f *pasFake) { f.groups[pasAccount] = 479 }, "without its account"},
		{"another account pinned", func(f *pasFake) { f.pin, f.pinned = access.DaemonPin{Name: "_other", UID: 470}, true }, "pins _other"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPasFake(t, "darwin")
			c.setup(f)
			err := pasSetup(context.Background(), f.h, pasOptions{yes: true})
			require.ErrorIs(t, err, errPasRefused)
			require.ErrorContains(t, err, c.want)
			require.Empty(t, f.scripts, "nothing runs")
		})
	}
}

// A dry run shows every command and runs none.
func TestPeerAuthSetup_DryRunChangesNothing(t *testing.T) {
	f := newPasFake(t, "darwin")
	require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{dryRun: true}))
	require.Empty(t, f.scripts)
	require.Empty(t, f.state)
	require.Empty(t, f.owner)
	out := f.out()
	require.Contains(t, out, "passkeys  approvers enroll under rp_id localhost, on http://localhost:8081")
	for _, want := range []string{"# start the state file", "# 1. export everything", "export -o -", "sha256 of v0.10.4",
		"# 11. reinstall the hooks", "# then, in this process: seamlessd install-hooks --config " + f.cfgPath + " --client claude --seam /opt/seamless/bin/seam"} {
		require.Contains(t, out, want)
	}
	_, err := os.Stat(f.cfgPath + pasKeptSuffix)
	require.ErrorIs(t, err, fs.ErrNotExist, "the owner's config is untouched")

	// Without a terminal and without --yes, nothing is asked and nothing runs.
	require.ErrorContains(t, pasSetup(context.Background(), f.h, pasOptions{}), "pass --yes")
	require.Empty(t, f.scripts)
	// Asked, a no changes nothing.
	f.h.interactive, f.h.in = true, bufio.NewReader(strings.NewReader("n\n"))
	require.ErrorContains(t, pasSetup(context.Background(), f.h, pasOptions{}), "not confirmed")
	require.Empty(t, f.scripts)
}

// The state file is root's: a line setup did not write refuses it whole.
func TestPeerAuthState_RefusesWhatSetupDidNotWrite(t *testing.T) {
	f := newPasFake(t, "linux")
	plan := pasTestPlan(t, f)
	line, err := pasPlanLine(plan, f.h.now())
	require.NoError(t, err)
	st, err := pasParseState([]byte(line + "\ndone backup 2026-11-20T12:00:00Z\n"))
	require.NoError(t, err)
	require.Equal(t, pasInProgress, st.phase())
	require.Equal(t, "account", st.next().id)

	for name, data := range map[string]string{
		"garbage":        line + "\nrm -rf /\n",
		"unknown step":   line + "\ndone everything 2026-11-20T12:00:00Z\n",
		"a step no plan": "done backup 2026-11-20T12:00:00Z\n",
		"bad time":       line + "\ndone backup yesterday\n",
		"unknown key":    strings.Replace(line, `{"v":1`, `{"v":1,"run":"x"`, 1),
		"bad owner":      strings.Replace(line, `"owner":"alice"`, `"owner":"-alice"`, 1),
		"bad account":    strings.Replace(line, `"account":"_seamless"`, `"account":"root"`, 1),
	} {
		_, err := pasParseState([]byte(data))
		require.Error(t, err, name)
	}

	// A plan line after a rolled-back move starts a new one.
	data := line + "\ndone backup 2026-11-20T12:00:00Z\nundone backup 2026-11-20T12:01:00Z\n" + line + "\n"
	st, err = pasParseState([]byte(data))
	require.NoError(t, err)
	require.Equal(t, pasInProgress, st.phase())
	require.Equal(t, "backup", st.next().id)
}

// The daemon's config is the owner's with four things changed, and loads.
func TestPeerAuthSetup_DaemonConfigKeepsTheRest(t *testing.T) {
	f := newPasFake(t, "linux")
	p := pasTestPlan(t, f)
	p.TLSCert, p.TLSKey = "/home/alice/certs/box.crt", "/home/alice/certs/box.key"
	raw := []byte("# my server\ndata_dir: ~/.seamless\naddr: \"0.0.0.0:8081\" # widened\ntls:\n  cert_file: ~/certs/box.crt\n  key_file: ~/certs/box.key\n" +
		"mcp:\n  api_key: \"k-123\"\nllm:\n  openai:\n    api_key: \"sk-test\"\npeer_auth:\n  max_lease: 8h\n")
	out, err := pasDaemonConfig(raw, p, f.h.now())
	require.NoError(t, err)
	s := string(out)
	for _, want := range []string{"# my server", "# widened", "data_dir: " + pasData, "cert_file: " + pasTLSDir + "/box.crt",
		"key_file: " + pasTLSDir + "/box.key", "api_key: \"k-123\"", "api_key: \"sk-test\"", "max_lease: 8h", "enabled: true",
		"owner: alice", "path: " + pasSocket, `mode: "0666"`, "Written by seamlessd peer-auth setup"} {
		require.Contains(t, s, want)
	}
	path := filepath.Join(t.TempDir(), "seamless.yaml")
	require.NoError(t, os.WriteFile(path, out, 0o600))
	cfg, err := config.LoadFrom(path)
	require.NoError(t, err)
	require.True(t, cfg.PeerAuth.Enabled)
	require.Equal(t, "alice", cfg.PeerAuth.Owner)
	require.Equal(t, pasData, cfg.DataDir)
	require.Equal(t, "0.0.0.0:8081", cfg.Addr)
	require.Equal(t, "sk-test", cfg.LLM.OpenAI.APIKey)
}

// Doctor shows a move only where one was begun.
func TestPeerAuthSetup_DoctorRow(t *testing.T) {
	require.Nil(t, pasStatusDoctor(func(string) ([]byte, error) { return nil, fs.ErrNotExist }))
	f := newPasFake(t, "linux")
	line, err := pasPlanLine(pasTestPlan(t, f), f.h.now())
	require.NoError(t, err)
	rows := pasStatusDoctor(func(string) ([]byte, error) { return []byte(line + "\n"), nil })
	require.Len(t, rows, 1)
	require.Equal(t, statusWarn, rows[0].status)
	require.Contains(t, rows[0].detail, "in progress: 0 of 11 steps done")
	var done strings.Builder
	done.WriteString(line + "\n")
	for _, s := range pasSteps {
		fmt.Fprintf(&done, "done %s 2026-11-20T12:00:00Z\n", s.id)
	}
	rows = pasStatusDoctor(func(string) ([]byte, error) { return []byte(done.String()), nil })
	require.Equal(t, statusOK, rows[0].status)
	rows = pasStatusDoctor(func(string) ([]byte, error) { return []byte("junk\n"), nil })
	require.Equal(t, statusWarn, rows[0].status)
}

// The move ends with the command that opens the console on its Approvers
// page, through the console's own config: the owner's is a socket client's now,
// which console-open refuses.
func TestPeerAuthSetup_EndsWithTheApproversPageCommand(t *testing.T) {
	f := newPasFake(t, "darwin")
	require.NoError(t, pasSetup(context.Background(), f.h, pasOptions{yes: true}))
	want := "SEAMLESS_CONFIG=" + shellcmd.PosixArg(pasConsoleConfig(*f.st().plan)) + " seamlessd console-open --path /console/approvers"
	require.Contains(t, f.out(), want)
	_, err := consoleOpenPath("/console/approvers")
	require.NoError(t, err, "the page it names is one console-open opens")
}
