package main

// The update of a peer_auth system daemon (update.KindSystem) in the updater's
// test world: real tar.gz archives on the release server, real files swapped
// in a real bin dir, the data dir's real lock, and a fake service manager --
// KeepAlive or Restart=always -- that starts the daemon again from whatever
// seamlessd is on disk whenever it is down.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/update"
)

// tarGz builds a release archive holding files (name -> contents), each a
// regular 0755 file unless its name is in links, which become symlinks.
func tarGz(t *testing.T, files map[string]string, links ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
		for _, l := range links {
			if l == name {
				hdr = &tar.Header{Name: name, Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}
			}
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if hdr.Typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(body))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// releaseArchive is release v's archive: its seamlessd and seam, which name
// the release, and the files goreleaser adds beside them.
func releaseArchive(t *testing.T, v string) []byte {
	return tarGz(t, map[string]string{
		"seamlessd": "seamlessd " + v + "\n", "seam": "seam " + v + "\n",
		"LICENSE": "license\n", "seam-onboard/SKILL.md": "skill\n",
	})
}

// supervisor is the system world's service manager: while the daemon is down
// it takes the data dir's lock, as a starting serve does, and starts the daemon
// as the release on disk -- unless that release is broken, which exits at once
// (a crash loop), or a stranger, which serves without recording itself as the
// running daemon (so it never confirms). delay is how long it waits after a
// stop before it starts again (systemd's RestartSec, launchd's throttle), and
// paused stops it starting anything.
type supervisor struct {
	w     *world
	delay time.Duration

	mu       sync.Mutex
	paused   bool
	broken   map[string]bool
	stranger map[string]bool
	// migrate, when set, runs on the database once as release migrateAs
	// starts: the new release migrating it before it fails.
	migrate   func() error
	migrateAs string
	lock    *fileLock
	stopped time.Time
	signals []int
	stopErr error
	errs    []error
}

// installed is the release the seamlessd on disk names.
func (s *supervisor) installed() (string, error) {
	raw, err := os.ReadFile(s.w.exe)
	if err != nil {
		return "", err
	}
	v, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "seamlessd ")
	if !ok {
		return "", errors.New("not a seamlessd: " + string(raw))
	}
	return v, nil
}

// tick starts the daemon when it is down and the delay has passed.
func (s *supervisor) tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paused || s.lock != nil || time.Since(s.stopped) < s.delay {
		return
	}
	lock, err := tryLockFile(filepath.Join(s.w.dataDir, dataDirLockName))
	if err != nil {
		return // held: by the updater, while it moves the database
	}
	v, err := s.installed()
	if err != nil || s.broken[v] {
		_ = lock.Close()
		s.stopped = time.Now()
		return
	}
	if s.migrate != nil && v == s.migrateAs {
		if err := s.migrate(); err != nil {
			s.errs = append(s.errs, err)
		}
		s.migrate = nil
	}
	if s.stranger[v] {
		s.w.daemon.startUnrecorded(v)
		s.lock = lock
		return
	}
	if err := s.w.daemon.startQuiet(v); err != nil {
		s.errs = append(s.errs, err)
		_ = lock.Close()
		return
	}
	s.lock = lock
}

// stop is the updater's SIGTERM to pid: the daemon goes down and lets go of
// the data dir.
func (s *supervisor) stop(pid int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signals = append(s.signals, pid)
	if s.stopErr != nil {
		return s.stopErr
	}
	if pid != os.Getpid() || s.lock == nil {
		return nil // no such process: gone already
	}
	s.w.daemon.stop()
	_ = s.lock.Close()
	s.lock, s.stopped = nil, time.Now()
	return nil
}

// newSystemWorld stands up a peer_auth system daemon of release from: the
// probe passes every system gate, seam sits beside seamlessd, both releases
// carry real archives, and the supervisor runs the daemon. The world's
// installer must never run.
func newSystemWorld(t *testing.T, from, to string) (*world, *supervisor) {
	t.Helper()
	w := newWorld(t, "darwin", "arm64", from)
	w.probeEdits = func(p *update.Probe) {
		p.PeerAuth, p.AutoOptIn = true, true
		p.Service = update.ServiceProbe{}
		p.System = update.SystemProbe{
			Account:    true,
			Service:    update.ServiceProbe{Path: "/Library/LaunchDaemons/com.arctop.seamless.daemon.plist", Found: true, Program: w.exe, RunsThisExe: true},
			Restarts:   true,
			Supervised: true,
		}
	}
	w.installer = func(*world, installerRun) error { return errors.New("a system install never runs the installer") }
	require.NoError(t, os.WriteFile(filepath.Join(w.installDir, "seam"), []byte("seam "+from+"\n"), 0o755))
	for _, v := range []string{from, to} {
		w.publish(v, releaseOpts{archive: releaseArchive(t, v)})
	}
	// The daemon the world started holds no lock; the supervisor's does.
	w.daemon.stop()
	w.daemon.ownPID = true
	s := &supervisor{w: w, broken: map[string]bool{}, stranger: map[string]bool{}}
	done := make(chan struct{})
	ticker := time.NewTicker(2 * time.Millisecond)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				s.tick()
			}
		}
	}()
	t.Cleanup(func() {
		close(done)
		ticker.Stop()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.lock != nil {
			_ = s.lock.Close()
		}
		require.Empty(t, s.errs)
	})
	require.Eventually(t, func() bool { v, _ := w.daemon.serving(); return v == from }, 2*time.Second, time.Millisecond)
	return w, s
}

// systemDeps is the world's deps with the supervisor's stop as the daemon's
// end, and an environment without the world's SEAMLESS_* noise, which an
// attended run would read as the owner's pin.
func systemDeps(w *world, s *supervisor) updaterDeps {
	d := w.deps(nil)
	d.stopDaemon = s.stop
	d.environ = []string{"PATH=/usr/bin:/bin", "HOME=" + w.home}
	return d
}

func runSystemAuto(t *testing.T, w *world, d updaterDeps, to string) (update.Attempt, error) {
	t.Helper()
	f, err := parseUpdateFlags(updaterArgs(update.SpawnRequest{
		AttemptID: testAttemptID(t), From: mustVersion(t, w.from), To: mustVersion(t, to), Why: update.WhyAuto,
	}, w.cfgPath)[1:])
	require.NoError(t, err)
	err = runAutoUpdate(f, d)
	a, rerr := update.ReadAttempt(w.dataDir)
	require.NoError(t, rerr)
	return a, err
}

// requireBinaries checks the bin dir holds seamlessd and seam of release v,
// both 0755, and nothing the update kept or staged.
func requireBinaries(t *testing.T, w *world, v string) {
	t.Helper()
	for _, name := range systemBinaries {
		path := filepath.Join(w.installDir, name)
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, name+" "+v+"\n", string(raw))
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o755), info.Mode().Perm(), name)
	}
	entries, err := os.ReadDir(w.installDir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	require.ElementsMatch(t, []string{"seam", "seamlessd"}, names, "nothing kept or staged is left behind")
}

func TestSystemUpdate_SwapsInPlaceAndConfirms(t *testing.T) {
	w, s := newSystemWorld(t, "0.7.2", "0.7.3")
	a, err := runSystemAuto(t, w, systemDeps(w, s), "0.7.3")
	require.NoError(t, err)
	requireRecorded(t, w, a, update.StageDone, false)
	require.True(t, a.OK)
	require.Empty(t, w.installs, "no installer runs")
	require.Equal(t, []int{os.Getpid()}, s.signals, "the daemon named by the lock and the state, once")
	requireBinaries(t, w, "0.7.3")
	ver, _ := w.daemon.serving()
	require.Equal(t, "0.7.3", ver)
	require.NotEmpty(t, a.BackupPath, "the pre-update backup ran")

	logged, err := os.ReadFile(a.LogPath)
	require.NoError(t, err)
	require.Contains(t, string(logged), "a peer_auth system daemon")
	require.Contains(t, string(logged), "checksums.txt signed by the release workflow on v0.7.3")
}

func TestSystemUpdate_RollsBackWhenTheReleaseNeverComesUp(t *testing.T) {
	w, s := newSystemWorld(t, "0.7.2", "0.7.3")
	s.broken["0.7.3"] = true
	a, err := runSystemAuto(t, w, systemDeps(w, s), "0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageRollback, true)
	require.Contains(t, a.Error, "v0.7.3 did not come up")
	require.Contains(t, a.Error, "rolled back to v0.7.2")
	requireBinaries(t, w, "0.7.2")
	ver, _ := w.daemon.serving()
	require.Equal(t, "0.7.2", ver)
}

func TestSystemUpdate_RollbackEndsANewDaemonThatDoesNotConfirm(t *testing.T) {
	w, s := newSystemWorld(t, "0.7.2", "0.7.3")
	// v0.7.3 comes up and serves, but never as the daemon the update state
	// names. The restart is held back a little, as RestartSec does, so the
	// updater takes the data dir's lock before the old release starts again.
	s.stranger["0.7.3"] = true
	s.delay = 50 * time.Millisecond
	a, err := runSystemAuto(t, w, systemDeps(w, s), "0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageRollback, true)
	require.Contains(t, a.Error, "rolled back to v0.7.2")
	require.NotContains(t, a.Error, "did not let go of the data dir", "the updater held the data dir while the daemon was down")
	require.Equal(t, []int{os.Getpid(), os.Getpid()}, s.signals, "v0.7.2 ended for the update, then v0.7.3 for the rollback")
	requireBinaries(t, w, "0.7.2")
	ver, _ := w.daemon.serving()
	require.Equal(t, "0.7.2", ver)
}

func TestSystemUpdate_RollbackDrill(t *testing.T) {
	w, s := newSystemWorld(t, "0.7.2", "0.7.3")
	require.NoError(t, os.MkdirAll(update.StateDir(w.dataDir), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(update.StateDir(w.dataDir), confirmDrillName), nil, 0o600))
	a, err := runSystemAuto(t, w, systemDeps(w, s), "0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageRollback, true)
	require.Contains(t, a.Error, "the rollback drill is on")
	requireBinaries(t, w, "0.7.2")
	require.Eventually(t, func() bool { v, _ := w.daemon.serving(); return v == "0.7.2" }, 2*time.Second, time.Millisecond)
}

func TestSystemUpdate_RefusesAReleaseItCannotVerify(t *testing.T) {
	tests := []struct {
		name    string
		publish func(t *testing.T, w *world)
		stage   update.Stage
		errText string
	}{
		{"an archive that does not match checksums.txt", func(t *testing.T, w *world) {
			good := releaseArchive(t, "0.7.3")
			w.publish("0.7.3", releaseOpts{archive: good})
			w.mu.Lock()
			w.assets["v0.7.3"][platformArchive(mustVersion(t, "0.7.3"), w.goos, w.goarch)] = releaseArchive(t, "6.6.6")
			w.mu.Unlock()
		}, update.StageVerify, "does not match its checksum"},
		{"checksums.txt signed for another tag", func(t *testing.T, w *world) {
			w.publish("0.7.3", releaseOpts{archive: releaseArchive(t, "0.7.3"), checksumsSigner: signedFor("0.7.1")})
		}, update.StageVerify, "checksums.txt of v0.7.3"},
		{"no checksums bundle (no floor fallback in place)", func(t *testing.T, w *world) {
			w.publish("0.7.3", releaseOpts{archive: releaseArchive(t, "0.7.3"), noChecksumsSign: true})
		}, update.StageVerify, "has no checksums.txt.sigstore.json"},
		{"an archive without seam", func(t *testing.T, w *world) {
			w.publish("0.7.3", releaseOpts{archive: tarGz(t, map[string]string{"seamlessd": "seamlessd 0.7.3\n"})})
		}, update.StageVerify, "no seam"},
		{"a binary that is a symlink", func(t *testing.T, w *world) {
			w.publish("0.7.3", releaseOpts{archive: tarGz(t, map[string]string{"seamlessd": "", "seam": "seam 0.7.3\n"}, "seamlessd")})
		}, update.StageVerify, "seamlessd is not a regular file"},
		{"an archive that is not one", func(t *testing.T, w *world) {
			w.publish("0.7.3", releaseOpts{archive: []byte("not gzip")})
		}, update.StageVerify, "does not hold the release's binaries"},
		{"an archive still uploading", func(t *testing.T, w *world) {
			w.publish("0.7.3", releaseOpts{archive: releaseArchive(t, "0.7.3")})
			w.setAssetState("0.7.3", platformArchive(mustVersion(t, "0.7.3"), w.goos, w.goarch), "open")
		}, update.StageFetch, "still uploading"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, s := newSystemWorld(t, "0.7.2", "0.7.3")
			tt.publish(t, w)
			a, err := runSystemAuto(t, w, systemDeps(w, s), "0.7.3")
			require.Error(t, err)
			requireRecorded(t, w, a, tt.stage, false)
			require.Contains(t, a.Error, tt.errText)
			require.Empty(t, s.signals, "the daemon is left alone")
			requireBinaries(t, w, "0.7.2")
		})
	}
}

func TestSystemUpdate_ADaemonThatCannotBeEndedKeepsItsBinaries(t *testing.T) {
	w, s := newSystemWorld(t, "0.7.2", "0.7.3")
	s.stopErr = errors.New("operation not permitted")
	a, err := runSystemAuto(t, w, systemDeps(w, s), "0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageInstall, false)
	require.Contains(t, a.Error, "could not be ended to start v0.7.3: operation not permitted")
	require.Contains(t, a.Error, "v0.7.2's binaries are back")
	requireBinaries(t, w, "0.7.2")
}

func TestSystemUpdate_NoDaemonToRestartInstallsNothing(t *testing.T) {
	w, s := newSystemWorld(t, "0.7.2", "0.7.3")
	s.mu.Lock()
	s.paused = true // the daemon stays down once stopped
	s.mu.Unlock()
	require.NoError(t, s.stop(os.Getpid()))
	s.signals = nil
	a, err := runSystemAuto(t, w, systemDeps(w, s), "0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StagePreflight, false)
	require.Contains(t, a.Error, "no daemon holds the data dir")
	require.Empty(t, s.signals)
	requireBinaries(t, w, "0.7.2")
}

func TestSystemUpdate_GatesRefuseAnInstallThatNoLongerUpdatesItself(t *testing.T) {
	w, s := newSystemWorld(t, "0.7.2", "0.7.3")
	edit := w.probeEdits
	w.probeEdits = func(p *update.Probe) {
		edit(p)
		p.AutoOptIn = false
	}
	a, err := runSystemAuto(t, w, systemDeps(w, s), "0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageGates, false)
	require.Equal(t, update.RefusalNotInstaller, a.Refusal)
	require.Contains(t, a.Error, "update.auto: true")
	requireBinaries(t, w, "0.7.2")
}

func TestSystemUpdate_Attended(t *testing.T) {
	w, s := newSystemWorld(t, "0.7.2", "0.7.3")
	var out strings.Builder
	d := systemDeps(w, s)
	d.stdout = &out
	require.NoError(t, runAttendedUpdate(t.Context(), updateFlags{config: w.cfgPath}, d))
	text := out.String()
	require.Contains(t, text, platformArchive(mustVersion(t, "0.7.3"), w.goos, w.goarch))
	require.Contains(t, text, "seamlessd and seam swapped in place")
	require.Contains(t, text, "v0.7.3 installed and serving")
	require.NotContains(t, text, "curl -fsSL", "no installer one-liner for an install the installer does not make")
	require.Empty(t, w.installs)
	requireBinaries(t, w, "0.7.3")
}

func TestSystemUpdate_AttendedRefusesTheInstallerForAnyPeerAuthLayout(t *testing.T) {
	w, s := newSystemWorld(t, "0.7.2", "0.7.3")
	edit := w.probeEdits
	w.probeEdits = func(p *update.Probe) {
		edit(p)
		p.ExeDirWritable = false // the guide's root-owned bin
	}
	var out strings.Builder
	d := systemDeps(w, s)
	d.stdout = &out
	err := runAttendedUpdate(t.Context(), updateFlags{config: w.cfgPath}, d)
	require.Error(t, err)
	require.Contains(t, err.Error(), "turns peer_auth on, and the installer sets up a per-user service")
	require.Contains(t, err.Error(), "cannot write the executable's directory")
	require.Empty(t, w.installs)
	requireBinaries(t, w, "0.7.2")
}

func TestWalkArchive(t *testing.T) {
	var got []string
	require.NoError(t, walkArchive(tarGz(t, map[string]string{"./seamlessd": "d", "seam": "s", "seamlessd.1": "man"}),
		func(name string, r io.Reader) error {
			got = append(got, name)
			return nil
		}))
	require.ElementsMatch(t, []string{"seamlessd", "seam"}, got)

	nested := tarGz(t, map[string]string{"bin/seamlessd": "d", "bin/seam": "s"})
	require.ErrorIs(t, walkArchive(nested, func(string, io.Reader) error { return nil }), errArchiveLayout,
		"only the top level counts")
}

func TestBinarySwap_PutBackRemovesASeamThatWasNotThere(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "seamlessd"), []byte("seamlessd 0.7.2\n"), 0o755))
	s, err := keepBinaries(dir)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"seamlessd": true}, s.had)
	require.NoError(t, s.stageNew(releaseArchive(t, "0.7.3")))
	require.NoError(t, s.swapIn())
	raw, err := os.ReadFile(filepath.Join(dir, "seam"))
	require.NoError(t, err)
	require.Equal(t, "seam 0.7.3\n", string(raw))
	require.NoError(t, s.putBack())
	_, err = os.Stat(filepath.Join(dir, "seam"))
	require.ErrorIs(t, err, os.ErrNotExist)
	raw, err = os.ReadFile(filepath.Join(dir, "seamlessd"))
	require.NoError(t, err)
	require.Equal(t, "seamlessd 0.7.2\n", string(raw))
	_, err = os.Stat(rollbackName(dir, "seamlessd"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestBinarySwap_ARenameThatFailsPutsTheKeptOnesBack(t *testing.T) {
	dir := t.TempDir()
	for _, name := range systemBinaries {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(name+" 0.7.2\n"), 0o755))
	}
	s, err := keepBinaries(dir)
	require.NoError(t, err)
	require.NoError(t, s.stageNew(releaseArchive(t, "0.7.3")))
	// seamlessd is renamed last; make that rename fail.
	blocker := filepath.Join(dir, "seamlessd")
	require.NoError(t, os.Remove(blocker))
	require.NoError(t, os.MkdirAll(filepath.Join(blocker, "x"), 0o700))
	err = s.swapIn()
	require.Error(t, err)
	require.ErrorIs(t, err, errPutBack, "the kept seamlessd cannot replace a directory either, and the error says so")
	raw, rerr := os.ReadFile(filepath.Join(dir, "seam"))
	require.NoError(t, rerr)
	require.Equal(t, "seam 0.7.2\n", string(raw), "seam, already swapped, is back")
	require.FileExists(t, rollbackName(dir, "seamlessd"), "the only copy left of the previous seamlessd is kept")
}

func TestSystemUpdate_BackupFailureLeavesNothingBehind(t *testing.T) {
	w, s := newSystemWorld(t, "0.7.2", "0.7.3")
	w.freeSpace = 1
	a, err := runSystemAuto(t, w, systemDeps(w, s), "0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageBackup, false)
	require.Empty(t, s.signals)
	requireBinaries(t, w, "0.7.2")
}

func TestSystemUpdate_AttendedRefusesAConfigItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	w, s := newSystemWorld(t, "0.7.2", "0.7.3")
	require.NoError(t, os.Chmod(w.cfgPath, 0o000))
	t.Cleanup(func() { _ = os.Chmod(w.cfgPath, 0o600) })
	var out strings.Builder
	d := systemDeps(w, s)
	d.stdout = &out
	err := runAttendedUpdate(t.Context(), updateFlags{config: w.cfgPath}, d)
	require.Error(t, err)
	require.Contains(t, err.Error(), "this account cannot read the config, so nothing is installed")
	require.Contains(t, err.Error(), update.SystemHint)
	require.Empty(t, w.installs, "the per-user installer never runs over a config it cannot read")
}

func TestSystemUpdate_RollbackRestoresAMigratedDatabaseWhileItHoldsTheDataDir(t *testing.T) {
	w, s := newSystemWorld(t, "0.7.2", "0.7.3")
	s0 := store.LatestSchemaVersion()
	dbPath := filepath.Join(w.dataDir, "seam.db")
	s.mu.Lock()
	s.stranger["0.7.3"] = true
	s.migrateAs = "0.7.3"
	s.migrate = func() error {
		db, err := store.OpenExisting(dbPath)
		if err != nil {
			return err
		}
		_, err = db.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, s0+1)
		return errors.Join(err, db.Close())
	}
	s.delay = 50 * time.Millisecond
	s.mu.Unlock()

	a, err := runSystemAuto(t, w, systemDeps(w, s), "0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageRollback, true)
	require.Contains(t, a.Error, fmt.Sprintf("the database was restored to schema v%d from the backup", s0))
	got, err := dbSchemaVersion(dbPath)
	require.NoError(t, err)
	require.Equal(t, s0, got, "the backup's database is back")
	require.FileExists(t, filepath.Join(w.dataDir, "seam.db.pre-rollback-"+a.ID), "the migrated database is kept")
	requireBinaries(t, w, "0.7.2")
	ver, _ := w.daemon.serving()
	require.Equal(t, "0.7.2", ver)
}
