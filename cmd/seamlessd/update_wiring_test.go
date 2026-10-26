package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
	"github.com/arctop/seamless/internal/update"
	"github.com/stretchr/testify/require"
)

// asRelease makes this test binary look like a goreleaser build of v for the
// test's duration. No test in this package runs in parallel, so swapping the
// link-time variables is safe.
func asRelease(t *testing.T, v string) {
	t.Helper()
	oldV, oldD := version, distribution
	version, distribution = v, update.DistributionRelease
	t.Cleanup(func() { version, distribution = oldV, oldD })
}

// testUpdateDeps is the daemon's checker wiring over a temp data dir, a real
// SQLite event log, and a TLS release server, with the loop's tick held. Its
// probes read only temp paths: CODEX_HOME points into the test's temp dir.
func testUpdateDeps(t *testing.T, list string) (update.Deps, *events.Recorder, *sql.DB, string) {
	t.Helper()
	w := newWiringRig(t, list)
	return w.deps, w.rec, w.db, w.dataDir
}

// wiringRig is testUpdateDeps with the parts a test drives: the held tick, the
// request tracker the deps read, and the config they were built from.
type wiringRig struct {
	t        *testing.T
	deps     update.Deps
	rec      *events.Recorder
	db       *sql.DB
	dataDir  string
	cfg      config.Config
	activity *activityTracker
	ticks    chan time.Time
}

func newWiringRig(t *testing.T, list string) *wiringRig {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `W/"1"`)
		_, _ = w.Write([]byte(list))
	}))
	t.Cleanup(srv.Close)
	client := srv.Client()
	client.CheckRedirect = update.HTTPSOnlyRedirect

	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	t.Setenv("CODEX_HOME", t.TempDir())

	w := &wiringRig{
		t: t, rec: events.NewRecorder(db), db: db, cfg: config.Defaults(),
		activity: newActivityTracker(time.Now), ticks: make(chan time.Time),
	}
	w.cfg.DataDir = t.TempDir()
	w.dataDir = w.cfg.DataDir
	w.deps = updateCheckerDeps(w.cfg, db, w.rec, "01TESTINSTANCE",
		update.Install{Kind: update.KindInstaller, Hint: update.Hint(update.KindInstaller)}, w.activity, nil)
	// The config loads no file, so no OS spawner is wired even under a release
	// stamp: a started checker here can never launch this test binary as an
	// updater. A test that needs one to apply sets a fakeSpawner.
	require.Nil(t, w.deps.Spawner)
	w.deps.Fetcher = &update.Fetcher{Client: client, URL: srv.URL, Now: time.Now}
	w.deps.Ticks = func() (<-chan time.Time, func()) { return w.ticks, func() {} }
	return w
}

// start builds and starts the checker over the rig's deps.
func (w *wiringRig) start() (*update.Checker, context.Context) {
	w.t.Helper()
	chk := update.New(w.deps)
	ctx, cancel := context.WithCancel(context.Background())
	chk.Start(ctx)
	w.t.Cleanup(func() { cancel(); chk.Wait() })
	return chk, ctx
}

// tick runs one loop tick to its end: the Refresh queues behind it on the
// loop goroutine, so it returns only once the tick has been handled.
func (w *wiringRig) tick(ctx context.Context, chk *update.Checker) update.Status {
	w.t.Helper()
	w.ticks <- time.Now()
	st, err := chk.Refresh(ctx)
	require.NoError(w.t, err)
	return st
}

// hold serves one counted request through the rig's tracker and keeps it in
// flight until the returned release is called.
func (w *wiringRig) hold() (release func()) {
	w.t.Helper()
	entered, unblock, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h := w.activity.wrap(activityHeldHandler(entered, unblock))
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/mcp", nil))
	}()
	<-entered
	var once sync.Once
	release = func() { once.Do(func() { close(unblock); <-done }) }
	w.t.Cleanup(release)
	return release
}

// fakeSpawner stands in for the OS spawner: it records what it was asked to
// start and starts nothing. Tests that start a checker able to apply use it,
// never the real one, which would launch this test binary as an updater.
type fakeSpawner struct {
	mu   sync.Mutex
	reqs []update.SpawnRequest
}

func (f *fakeSpawner) Spawn(_ context.Context, req update.SpawnRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return nil
}

func (f *fakeSpawner) spawned() []update.SpawnRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// loadedConfig is a config read from a real (empty) file under a temp dir, so
// newUpdateSpawner has the absolute config path it needs.
func loadedConfig(t *testing.T) config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seamless.yaml")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	cfg, err := config.LoadFrom(path)
	require.NoError(t, err)
	cfg.DataDir = t.TempDir()
	return cfg
}

const oneNewerRelease = `[{"tag_name":"v0.7.3","published_at":"2026-10-08T00:00:00Z","assets":[
  {"name":"checksums.txt","state":"uploaded"},{"name":"install","state":"uploaded"},
  {"name":"install.sigstore.json","state":"uploaded"},{"name":"install.ps1","state":"uploaded"},
  {"name":"install.ps1.sigstore.json","state":"uploaded"}]}]`

// TestUpdateWiring_ReleaseBuildEndToEnd runs the real checker against a real
// event log and the real fetcher: a check records update.available once,
// writes the state file, and /healthz then names the newer release -- until
// the console turns checks off.
func TestUpdateWiring_ReleaseBuildEndToEnd(t *testing.T) {
	asRelease(t, "0.7.2")
	deps, rec, db, dataDir := testUpdateDeps(t, oneNewerRelease)
	chk := update.New(deps)
	ctx, cancel := context.WithCancel(context.Background())
	chk.Start(ctx)
	t.Cleanup(func() { cancel(); chk.Wait() })

	st, err := chk.CheckNow(ctx)
	require.NoError(t, err)
	require.True(t, st.Available)
	require.Equal(t, "0.7.3", st.Newest.Version.String())

	saved, err := update.ReadState(dataDir)
	require.NoError(t, err)
	require.Equal(t, "01TESTINSTANCE", saved.Running.Instance)
	require.Equal(t, "0.7.2", saved.LastRunVersion.String())
	require.Len(t, saved.Releases, 1)

	got, err := rec.ByKinds(ctx, []core.EventKind{core.EventUpdateAvailable}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "0.7.3", got[0].ItemID)

	body := serveHealthz(t, db, deps.Instance, chk)
	require.Equal(t, "0.7.3", body["update_available"])
	require.Equal(t, "01TESTINSTANCE", body["instance"])
	require.Equal(t, "release", body["distribution"])

	// The console turns checks off: /healthz stops naming the release.
	off := false
	require.NoError(t, store.SetUpdateOverride(ctx, db, config.UpdateOverride{Check: &off}))
	_, err = chk.Refresh(ctx)
	require.NoError(t, err)
	_, has := serveHealthz(t, db, deps.Instance, chk)["update_available"]
	require.False(t, has)
}

// TestUpdateWiring_SourceBuildNeverWritesState: a source build -- which every
// test, make run, the fixtures and seambench are -- never asks GitHub by
// default and never creates <data_dir>/update.
func TestUpdateWiring_SourceBuildNeverWritesState(t *testing.T) {
	deps, rec, db, dataDir := testUpdateDeps(t, oneNewerRelease)
	chk := update.New(deps)
	ctx, cancel := context.WithCancel(context.Background())
	chk.Start(ctx)
	t.Cleanup(func() { cancel(); chk.Wait() })

	st := chk.Status()
	require.False(t, st.Release)
	require.False(t, st.Settings.Check)
	_, err := chk.CheckNow(ctx)
	require.ErrorIs(t, err, update.ErrChecksOff)

	_, err = os.Stat(update.StateDir(dataDir))
	require.True(t, os.IsNotExist(err))
	got, err := rec.ByKinds(ctx, []core.EventKind{core.EventUpdateAvailable, core.EventUpdateApplied}, "", "", 10)
	require.NoError(t, err)
	require.Empty(t, got)

	body := serveHealthz(t, db, deps.Instance, chk)
	require.Equal(t, "source", body["distribution"])
	_, has := body["update_available"]
	require.False(t, has)
}

// soakedSignedRelease is v0.7.3, long past any soak, with every asset an
// unattended update needs -- the checksums bundle included.
const soakedSignedRelease = `[{"tag_name":"v0.7.3","published_at":"2025-01-08T00:00:00Z","assets":[
  {"name":"checksums.txt","state":"uploaded"},{"name":"checksums.txt.sigstore.json","state":"uploaded"},
  {"name":"install","state":"uploaded"},{"name":"install.sigstore.json","state":"uploaded"},
  {"name":"install.ps1","state":"uploaded"},{"name":"install.ps1.sigstore.json","state":"uploaded"}]}]`

// Only a published release gets the OS spawner, and the checker reports an
// updater wired (CanApply) exactly when it has one. Nothing here starts a
// checker, so the real spawner is built and never used.
func TestUpdateWiring_SpawnerOnlyOnAReleaseBuild(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	installer := update.Install{Kind: update.KindInstaller, Hint: update.Hint(update.KindInstaller)}
	osHasSpawner := runtime.GOOS == "darwin" || runtime.GOOS == "linux" || runtime.GOOS == "windows"

	tests := []struct {
		name         string
		distribution string
		version      string
		loaded       bool // a config file is loaded
		want         bool
	}{
		{"source build", update.DistributionSource, devVersion, true, false},
		{"source build carrying a release number", update.DistributionSource, "0.7.2", true, false},
		{"goreleaser snapshot", update.DistributionRelease, "0.7.3-SNAPSHOT-1a2b3c4", true, false},
		{"release without a config file", update.DistributionRelease, "0.7.2", false, false},
		{"release", update.DistributionRelease, "0.7.2", true, osHasSpawner},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldV, oldD := version, distribution
			version, distribution = tt.version, tt.distribution
			t.Cleanup(func() { version, distribution = oldV, oldD })
			cfg := config.Defaults()
			cfg.DataDir = t.TempDir()
			if tt.loaded {
				cfg = loadedConfig(t)
			}

			deps := updateCheckerDeps(cfg, db, nil, "01TESTINSTANCE", installer, newActivityTracker(time.Now), nil)
			require.Equal(t, tt.want, deps.Spawner != nil, "Spawner wired")
			require.Equal(t, tt.want, update.New(deps).Status().CanApply, "CanApply")
			require.NoDirExists(t, update.StateDir(cfg.DataDir), "building the checker writes nothing")
		})
	}
}

// The rest of the automatic-update wiring: the live-session counter over the
// daemon's own sessions with the configured idle threshold, the snapshot of
// the tracker passed in, and the two probes, reading this data dir and the
// daemon's CODEX_HOME.
func TestUpdateWiring_AutomaticUpdateReads(t *testing.T) {
	w := newWiringRig(t, oneNewerRelease)
	ctx := context.Background()
	d := w.deps

	require.Equal(t, 45*time.Minute, d.SessionIdle, "gardener.session_idle_minutes, defaulted")
	cfg := w.cfg
	cfg.Gardener.SessionIdleMinutes = 10
	require.Equal(t, 10*time.Minute, updateCheckerDeps(cfg, w.db, w.rec, "01I", update.Install{}, nil, nil).SessionIdle)

	now := time.Now().UTC()
	beat := now.Add(-20 * time.Minute)
	for _, s := range []core.Session{
		{ID: "01SESSLIVE", Name: "cc/live", Status: core.SessionActive, CreatedAt: beat, UpdatedAt: beat},
		{ID: "01SESSDONE", Name: "cc/done", Status: core.SessionCompleted, CreatedAt: now, UpdatedAt: now},
	} {
		require.NoError(t, store.CreateSession(ctx, w.db, s))
	}
	n, err := d.LiveSessions(ctx, now.Add(-d.SessionIdle))
	require.NoError(t, err)
	require.Equal(t, 1, n, "the active session heartbeating inside the window; never the completed one")
	n, err = d.LiveSessions(ctx, now.Add(-10*time.Minute))
	require.NoError(t, err)
	require.Zero(t, n, "its heartbeat is older than a 10m cutoff")

	require.NotNil(t, d.Activity)
	inFlight, _ := d.Activity()
	require.Zero(t, inFlight)
	release := w.hold()
	inFlight, _ = d.Activity()
	require.EqualValues(t, 1, inFlight, "the snapshot is the tracker's that serves the routes")
	release()
	inFlight, last := d.Activity()
	require.Zero(t, inFlight)
	require.False(t, last.Before(now), "stamped when the request ended")
	require.Nil(t, updateCheckerDeps(w.cfg, w.db, w.rec, "01I", update.Install{}, nil, nil).Activity,
		"no tracker, no snapshot: activity is unknown")

	held, err := d.UpdaterRunning()
	require.NoError(t, err)
	require.False(t, held)
	require.NoDirExists(t, update.StateDir(w.dataDir), "the probe creates nothing")
	require.NoError(t, os.MkdirAll(update.StateDir(w.dataDir), 0o700))
	updater, err := tryLockFile(update.LockPath(w.dataDir))
	require.NoError(t, err)
	held, err = d.UpdaterRunning()
	require.NoError(t, err)
	require.True(t, held, "it probes this data dir's update lock")
	require.NoError(t, updater.Close())

	fp, err := d.CodexHooksHash()
	require.NoError(t, err)
	require.Equal(t, "absent", fp)
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "hooks.json"), []byte("{}"), 0o600))
	fp, err = d.CodexHooksHash()
	require.NoError(t, err)
	require.Equal(t, sha256Fingerprint("{}"), fp)
}

// A source build with checks turned on asks GitHub, and still never reaches
// any automatic-update read or writes anything under the data dir: the probes
// are wired on every build, and none of them runs on this one.
func TestUpdateWiring_SourceBuildNeverRunsTheAutomaticUpdateReads(t *testing.T) {
	w := newWiringRig(t, soakedSignedRelease)
	on := true
	w.deps.Base.Check = &on
	var calls atomic.Int32
	live, act, running, codex := w.deps.LiveSessions, w.deps.Activity, w.deps.UpdaterRunning, w.deps.CodexHooksHash
	w.deps.LiveSessions = func(ctx context.Context, cutoff time.Time) (int, error) {
		calls.Add(1)
		return live(ctx, cutoff)
	}
	w.deps.Activity = func() (int64, time.Time) { calls.Add(1); return act() }
	w.deps.UpdaterRunning = func() (bool, error) { calls.Add(1); return running() }
	w.deps.CodexHooksHash = func() (string, error) { calls.Add(1); return codex() }

	chk, ctx := w.start()
	st, err := chk.CheckNow(ctx)
	require.NoError(t, err)
	require.NotNil(t, st.Newest, "the check ran")
	require.False(t, st.CanApply)
	st = w.tick(ctx, chk)
	require.Nil(t, st.Applying)
	require.Zero(t, calls.Load(), "no automatic-update read on a source build")
	require.NoDirExists(t, update.StateDir(w.dataDir))
	mode, _ := st.Mode()
	require.Equal(t, update.ModeNotify, mode)
}

// End to end on a release installer install: with the daemon idle -- no live
// agent session in its database, nothing in flight in its tracker, no updater
// holding the lock -- the next tick starts the update. A request held in
// flight, then a live session, each hold it off. The spawner is the only fake.
func TestUpdateWiring_ReleaseInstallUpdatesWhenIdle(t *testing.T) {
	asRelease(t, "0.7.2")
	w := newWiringRig(t, soakedSignedRelease)
	spawner := &fakeSpawner{}
	w.deps.Spawner = spawner
	chk, ctx := w.start()

	st, err := chk.CheckNow(ctx)
	require.NoError(t, err)
	require.True(t, st.CanApply)
	mode, reason := st.Mode()
	require.Equal(t, update.ModeAuto, mode, reason)
	require.NotNil(t, st.Target)
	require.Equal(t, "0.7.3", st.Target.Version.String())

	// A request in flight holds the update off; once it ends, idle applies.
	release := w.hold()
	st = w.tick(ctx, chk)
	require.Equal(t, update.WaitInFlight, st.WaitCode, st.Waiting)
	require.Empty(t, spawner.spawned())
	release()

	// So does a live agent session.
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, w.db, core.Session{
		ID: "01SESSLIVE", Name: "cc/live", Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))
	st = w.tick(ctx, chk)
	require.Equal(t, update.WaitSessions, st.WaitCode, st.Waiting)
	require.Empty(t, spawner.spawned())

	require.NoError(t, store.UpdateSession(ctx, w.db, core.Session{
		ID: "01SESSLIVE", Name: "cc/live", Status: core.SessionCompleted, CreatedAt: now, UpdatedAt: time.Now().UTC(),
	}))
	st = w.tick(ctx, chk)
	reqs := spawner.spawned()
	require.Len(t, reqs, 1, "idle: the tick started the update (%s)", st.Waiting)
	require.Equal(t, update.WhyAuto, reqs[0].Why)
	require.Equal(t, "0.7.2", reqs[0].From.String())
	require.Equal(t, "0.7.3", reqs[0].To.String())
	require.NotNil(t, st.Applying)
	require.Equal(t, reqs[0].AttemptID, st.Applying.ID)

	saved, err := update.ReadState(w.dataDir)
	require.NoError(t, err)
	require.True(t, saved.Running.CanApply)
	require.Equal(t, "absent", saved.CodexHooks, "the daemon's Codex fingerprint, saved at start")
	require.NotNil(t, saved.Spawn)
	require.Equal(t, "absent", saved.Spawn.CodexHooks, "and again before the spawn")
	require.NoFileExists(t, update.LockPath(w.dataDir), "probing the lock left no file behind")
}

func TestHealthzBody(t *testing.T) {
	newest := &update.Release{Version: update.Version{Major: 0, Minor: 7, Patch: 3}}
	on := update.Settings{Check: true}
	tests := []struct {
		name      string
		st        *update.Status
		instance  string
		wantAvail string
	}{
		{"no update check wired", nil, "", ""},
		{"up to date", &update.Status{Settings: on, Newest: newest}, "01I", ""},
		{"available", &update.Status{Settings: on, Newest: newest, Available: true}, "01I", "0.7.3"},
		{"available but checks off", &update.Status{Newest: newest, Available: true}, "01I", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := healthzBody("ok", tt.instance, tt.st)
			require.Equal(t, "ok", body["status"])
			require.Equal(t, buildVersion(), body["version"])
			require.Equal(t, distribution, body["distribution"])
			require.Equal(t, tt.instance, body["instance"])
			require.Equal(t, tt.wantAvail, body["update_available"])
			if tt.instance == "" {
				_, has := body["instance"]
				require.False(t, has)
			}
		})
	}
}

// serveHealthz runs the real /healthz handler once against the checker.
func serveHealthz(t *testing.T, db *sql.DB, instance string, chk *update.Checker) map[string]string {
	t.Helper()
	w := httptest.NewRecorder()
	healthzHandler(db, instance, chk.Status)(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	return body
}
