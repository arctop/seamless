package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/update"
	"github.com/stretchr/testify/require"
)

// The updater's command line is a contract between two releases: what one
// daemon builds, every later seamlessd parses back to the same request.
func TestUpdaterArgs_RoundTrip(t *testing.T) {
	req := update.SpawnRequest{
		AttemptID: testAttemptID(t), From: mustVersion(t, "0.7.2"), To: mustVersion(t, "0.10.0"), Why: update.WhyAuto,
	}
	cfg := filepath.Join(t.TempDir(), "seamless.yaml")
	args := updaterArgs(req, cfg)
	require.Equal(t, []string{
		"update", "--auto", "--attempt", req.AttemptID, "--from", "0.7.2", "--to", "0.10.0",
		"--why", "auto", "--config", cfg,
	}, args, "the frozen shape")

	f, err := parseUpdateFlags(args[1:])
	require.NoError(t, err)
	require.True(t, f.auto)
	got, gotCfg, err := f.spawnRequest()
	require.NoError(t, err)
	require.Equal(t, req, got)
	require.Equal(t, cfg, gotCfg)
}

func TestParseUpdateFlags_Refusals(t *testing.T) {
	id := testAttemptID(t)
	cfg := filepath.Join(t.TempDir(), "seamless.yaml")
	auto := []string{"--auto", "--attempt", id, "--from", "0.7.2", "--to", "0.7.3", "--why", "auto", "--config", cfg}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"--url with --auto", append(slicesClone(auto), "--url", "https://example.com/install"), "--url cannot be combined with --auto"},
		{"--check with --auto", append(slicesClone(auto), "--check"), "--check cannot be combined with --auto"},
		{"--dry-run with --auto", append(slicesClone(auto), "--dry-run"), "--dry-run cannot be combined with --auto"},
		{"--check=false with --auto is still given", append(slicesClone(auto), "--check=false"), "--check cannot be combined with --auto"},
		{"--attempt without --auto", []string{"--attempt", id}, "needs --auto"},
		{"--from without --auto", []string{"--from", "0.7.2"}, "needs --auto"},
		{"a stray argument", []string{"now"}, "unexpected argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseUpdateFlags(tt.args)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }

// Every field of the request is held before anything acts on it.
func TestSpawnRequest_Refusals(t *testing.T) {
	id := testAttemptID(t)
	cfg := filepath.Join(t.TempDir(), "seamless.yaml")
	tests := []struct {
		name                       string
		attempt, from, to, why, cf string
		want                       string
	}{
		{"not a ULID", "not-a-ulid", "0.7.2", "0.7.3", "auto", cfg, "not a ULID"},
		{"a v-prefixed version", id, "v0.7.2", "0.7.3", "auto", cfg, "want a release version"},
		{"a prerelease version", id, "0.7.2", "0.7.3-rc1", "auto", cfg, "want a release version"},
		{"a zero-padded version", id, "0.07.2", "0.7.3", "auto", cfg, "want a release version"},
		{"a downgrade", id, "0.7.3", "0.7.2", "auto", cfg, "only moves forward"},
		{"a reinstall", id, "0.7.3", "0.7.3", "auto", cfg, "only moves forward"},
		{"an unknown why", id, "0.7.2", "0.7.3", "whim", cfg, "valid values"},
		{"a manual why", id, "0.7.2", "0.7.3", "manual", cfg, "attended"},
		{"a relative config", id, "0.7.2", "0.7.3", "auto", "seamless.yaml", "absolute"},
		{"no config", id, "0.7.2", "0.7.3", "auto", "", "absolute"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"--auto", "--attempt", tt.attempt, "--from", tt.from, "--to", tt.to, "--why", tt.why}
			if tt.cf != "" {
				args = append(args, "--config", tt.cf)
			}
			f, err := parseUpdateFlags(args)
			require.NoError(t, err)
			_, _, err = f.spawnRequest()
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestInstallRunHint(t *testing.T) {
	const u = "https://github.com/arctop/seamless/releases/download/v0.7.3/install"
	require.Equal(t, "curl -fsSL "+u+" | SEAMLESS_VERSION=0.7.3 sh", installRunHint("linux", u, "0.7.3"))
	require.Equal(t, "curl -fsSL "+u+" | sh", installRunHint("darwin", u, ""))
	require.Equal(t, "$env:SEAMLESS_VERSION='0.7.3'; irm "+u+".ps1 | iex", installRunHint("windows", u+".ps1", "0.7.3"))
	require.Equal(t, "irm "+u+".ps1 | iex", installRunHint("windows", u+".ps1", ""))
}

// attendedEnv is a clean terminal environment for an attended test, plus
// the owner's own knobs.
func attendedEnv(w *world, knobs ...string) []string {
	return append([]string{"PATH=/usr/bin:/bin", "HOME=" + w.home, "LANG=C"}, knobs...)
}

func runAttended(t *testing.T, w *world, d updaterDeps, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	d.stdout = &out
	f, err := parseUpdateFlags(append([]string{"--config", w.cfgPath}, args...))
	require.NoError(t, err)
	err = runAttendedUpdate(context.Background(), f, d)
	return out.String(), err
}

func TestAttendedUpdate_InstallsTheNewestReleaseRecorded(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.environ = attendedEnv(w, "SEAMLESS_CLIENT=codex")
	w.publish("0.7.1", releaseOpts{})
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	w.publish("0.8.0", releaseOpts{prerelease: true})

	out, err := runAttended(t, w, w.deps(nil))
	require.NoError(t, err, out)
	require.Contains(t, out, "v0.7.3")
	require.Contains(t, out, "(the newest release)")
	require.NotContains(t, out, "0.8.0", "a prerelease is never the newest")
	require.Contains(t, out, "v0.7.3 installed and serving")

	a, err := update.ReadAttempt(w.dataDir)
	require.NoError(t, err)
	requireRecorded(t, w, a, update.StageDone, false)
	require.Equal(t, update.WhyManual, a.Why)

	require.Len(t, w.installs, 1)
	run := w.installs[0]
	require.False(t, run.unattended)
	env := envOf(t, run.env)
	require.Equal(t, "0.7.3", env["SEAMLESS_VERSION"], "an attended update always pins the version")
	require.Equal(t, w.pin("0.7.3"), env["SEAMLESS_CHECKSUMS_SHA256"])
	require.Equal(t, w.installDir, env["SEAMLESS_INSTALL_DIR"])
	require.Equal(t, "codex", env["SEAMLESS_CLIENT"], "the owner's knob passes through")
	require.NotContains(t, env, "SEAMLESS_NO_HOOKS", "attended, the installer detects and asks")
}

// The attended path takes the update lock the same way: it waits out the
// daemon's momentary probe, and refuses, recording nothing, while another
// update holds the lock for the whole wait.
func TestAttendedUpdate_TheUpdateLock(t *testing.T) {
	setup := func(t *testing.T) (*world, *fileLock) {
		w := newWorld(t, "linux", "amd64", "0.7.2")
		w.environ = attendedEnv(w)
		w.publish("0.7.2", releaseOpts{})
		w.publish("0.7.3", releaseOpts{})
		require.NoError(t, os.MkdirAll(update.StateDir(w.dataDir), 0o700))
		held, err := tryLockFile(update.LockPath(w.dataDir))
		require.NoError(t, err)
		return w, held
	}
	t.Run("a momentary holder is waited out", func(t *testing.T) {
		w, probe := setup(t)
		released := make(chan error, 1)
		time.AfterFunc(200*time.Millisecond, func() { released <- probe.Close() })
		d := w.deps(nil)
		d.timing.lockWait = 10 * time.Second
		out, err := runAttended(t, w, d)
		require.NoError(t, err, out)
		require.NoError(t, <-released)
		require.Contains(t, out, "waiting up to 10s")
		a, err := update.ReadAttempt(w.dataDir)
		require.NoError(t, err)
		requireRecorded(t, w, a, update.StageDone, false)
	})
	t.Run("a lock held for the whole wait is another update", func(t *testing.T) {
		w, held := setup(t)
		defer func() { _ = held.Close() }()
		_, err := runAttended(t, w, w.deps(nil))
		require.ErrorIs(t, err, errLockHeld)
		require.ErrorContains(t, err, "another update is running")
		require.NoFileExists(t, update.AttemptPath(w.dataDir))
		require.NoFileExists(t, update.AttemptHistoryPath(w.dataDir))
		require.NoDirExists(t, backupDir(w.dataDir))
		require.Empty(t, w.installs)
	})
}

func TestAttendedUpdate_UpToDateChangesNothing(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.3")
	w.environ = attendedEnv(w)
	w.publish("0.7.3", releaseOpts{})

	out, err := runAttended(t, w, w.deps(nil))
	require.NoError(t, err)
	require.Contains(t, out, "up to date")
	require.Contains(t, out, "SEAMLESS_VERSION=0.7.3 seamlessd update")
	require.Empty(t, w.installs)
	requireNothingRecorded(t, w)
}

// A pinned older release is a downgrade the owner asked for: recorded,
// backed up, and the rollback target is still the running release.
func TestAttendedUpdate_PinnedOlderReleaseDowngrades(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.environ = attendedEnv(w, "SEAMLESS_VERSION=0.7.1")
	w.publish("0.7.1", releaseOpts{})
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})

	out, err := runAttended(t, w, w.deps(nil))
	require.NoError(t, err, out)
	require.Contains(t, out, "(SEAMLESS_VERSION)")
	a, err := update.ReadAttempt(w.dataDir)
	require.NoError(t, err)
	requireRecorded(t, w, a, update.StageDone, false)
	require.Equal(t, "0.7.2", a.From.String())
	require.Equal(t, "0.7.1", a.To.String())
	require.Equal(t, "0.7.1", envOf(t, w.installs[0].env)["SEAMLESS_VERSION"])
}

// An attended rollback runs the rollback target pinned to itself, whatever
// the owner pinned for the update.
func TestAttendedUpdate_RollbackOverridesTheOwnersPin(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.environ = attendedEnv(w, "SEAMLESS_VERSION=0.7.3")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	w.installer = func(w *world, run installerRun) error {
		if installerVersion(t, run) == "0.7.3" {
			w.daemon.stop()
			return os.WriteFile(w.exe, []byte("seamlessd 0.7.3\n"), 0o755)
		}
		return installerSwaps(w, run)
	}

	out, err := runAttended(t, w, w.deps(nil))
	require.Error(t, err)
	require.Contains(t, out, "v0.7.2 serving again")
	a, rerr := update.ReadAttempt(w.dataDir)
	require.NoError(t, rerr)
	requireRecorded(t, w, a, update.StageRollback, true)
	require.Len(t, w.installs, 2)
	back := envOf(t, w.installs[1].env)
	require.Equal(t, "0.7.2", back["SEAMLESS_VERSION"])
	require.Equal(t, w.pin("0.7.2"), back["SEAMLESS_CHECKSUMS_SHA256"])
}

// A pinned release from before signed checksums (the floor) is installed by
// the newest release's installer, verified with ITS tag, with no checksums
// pin: that installer checks the old archive itself.
func TestAttendedUpdate_PinnedBelowTheFloorUsesTheNewestInstaller(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.environ = attendedEnv(w, "SEAMLESS_VERSION=0.6.0")
	w.publish("0.6.0", releaseOpts{noChecksumsSign: true, installerSigner: "https://github.com/0spoon/seamless/.github/workflows/release.yml@refs/tags/v0.6.0"})
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})

	out, err := runAttended(t, w, w.deps(nil))
	require.NoError(t, err, out)
	require.Contains(t, out, "predates signed checksums")
	require.Len(t, w.installs, 1)
	require.Equal(t, "#!/bin/sh\n# the v0.7.3 installer\n", string(w.installs[0].script))
	env := envOf(t, w.installs[0].env)
	require.Equal(t, "0.6.0", env["SEAMLESS_VERSION"])
	require.NotContains(t, env, "SEAMLESS_CHECKSUMS_SHA256")
	a, err := update.ReadAttempt(w.dataDir)
	require.NoError(t, err)
	requireRecorded(t, w, a, update.StageDone, false)
	require.Equal(t, "0.6.0", a.To.String())
}

func TestAttendedUpdate_RefusesHomebrew(t *testing.T) {
	w := newWorld(t, "darwin", "arm64", "0.7.2")
	w.environ = attendedEnv(w)
	w.probeEdits = func(p *update.Probe) { p.Exe = "/opt/homebrew/Caskroom/seamless/0.7.2/seamlessd" }
	_, err := runAttended(t, w, w.deps(nil))
	require.ErrorContains(t, err, update.Hint(update.KindHomebrew))
	require.Empty(t, w.installs)
}

// Anything the engine does not manage runs the verified installer as
// before, without a backup, a rollback or a record.
func TestAttendedUpdate_UnmanagedRunsUnrecorded(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(w *world, d *updaterDeps)
		reason string
	}{
		{"a source build (make update)", func(w *world, d *updaterDeps) {
			d.version, d.distribution, d.buildVersion = "0.7.2", update.DistributionSource, "0.7.2+dirty"
			w.probeEdits = func(p *update.Probe) { p.Distribution = update.DistributionSource }
		}, "source"},
		{"a release build run by hand next to another install", func(w *world, _ *updaterDeps) {
			w.probeEdits = func(p *update.Probe) { p.Service.RunsThisExe = false }
		}, "not this executable"},
		{"an install dir the owner moves", func(w *world, _ *updaterDeps) {
			w.environ = attendedEnv(w, "SEAMLESS_INSTALL_DIR="+filepath.Join(w.home, "elsewhere"))
		}, "moves the install"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, "linux", "amd64", "0.7.2")
			w.environ = attendedEnv(w)
			w.publish("0.7.2", releaseOpts{})
			w.publish("0.7.3", releaseOpts{})
			d := w.deps(nil)
			tt.setup(w, &d)
			d.environ = w.environ

			out, err := runAttended(t, w, d)
			require.NoError(t, err, out)
			require.Contains(t, out, tt.reason)
			require.Contains(t, out, "without a backup, a rollback or a record")
			require.Len(t, w.installs, 1)
			env := envOf(t, w.installs[0].env)
			require.Equal(t, "0.7.3", env["SEAMLESS_VERSION"])
			require.Equal(t, w.pin("0.7.3"), env["SEAMLESS_CHECKSUMS_SHA256"])
			requireNothingRecorded(t, w)
		})
	}
}

// A pin that is not a release number reaches the newest installer verbatim,
// as it always did: a release candidate is installed that way.
func TestAttendedUpdate_AReleaseCandidatePinPassesThrough(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.environ = attendedEnv(w, "SEAMLESS_VERSION=0.8.0-rc.1")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	w.installer = func(*world, installerRun) error { return nil }

	out, err := runAttended(t, w, w.deps(nil))
	require.NoError(t, err, out)
	require.Contains(t, out, "0.8.0-rc.1")
	require.Len(t, w.installs, 1)
	require.Equal(t, "#!/bin/sh\n# the v0.7.3 installer\n", string(w.installs[0].script))
	env := envOf(t, w.installs[0].env)
	require.Equal(t, "0.8.0-rc.1", env["SEAMLESS_VERSION"])
	require.NotContains(t, env, "SEAMLESS_CHECKSUMS_SHA256")
}

func TestAttendedUpdate_DryRunTouchesNothing(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.environ = attendedEnv(w)
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})

	out, err := runAttended(t, w, w.deps(nil), "--dry-run")
	require.NoError(t, err)
	for _, want := range []string{"(dry run)", "v0.7.3", "/releases/download/v0.7.3/install", "pre-update-v0.7.2-<UTC time>.tar.gz",
		"SEAMLESS_VERSION=0.7.3 sh", "no changes made"} {
		require.Contains(t, out, want)
	}
	require.Empty(t, w.installs)
	requireNothingRecorded(t, w)
}

// The plan's backup row names no stamp of its own: the backup is stamped when
// it starts, seconds after the plan prints, so a plan-time stamp named a file
// that never existed (seen live 2026-11-20: 205347Z planned, 205357Z written).
func TestBackupNamePattern_LeavesTheStampToTheBackup(t *testing.T) {
	from := mustVersion(t, "0.10.3")
	require.Equal(t, "pre-update-v0.10.3-<UTC time>.tar.gz", backupNamePattern(from))
	_, ok := backupTime(backupNamePattern(from))
	require.False(t, ok, "the pattern is never mistaken for a backup the updater wrote")
}

// --url keeps its escape hatch: TLS only, run as is, nothing recorded.
func TestAttendedUpdate_CustomURL(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.environ = attendedEnv(w)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		_, _ = rw.Write([]byte("#!/bin/sh\necho custom\n"))
	}))
	defer srv.Close()
	w.installer = func(*world, installerRun) error { return nil }
	d := w.deps(nil)
	d.releases.client = srv.Client()

	out, err := runAttended(t, w, d, "--url", srv.URL+"/install")
	require.NoError(t, err, out)
	require.Contains(t, out, "custom --url carries no sigstore bundle")
	require.Len(t, w.installs, 1)
	require.Equal(t, "#!/bin/sh\necho custom\n", string(w.installs[0].script))
	require.Equal(t, w.environ, w.installs[0].env)
	requireNothingRecorded(t, w)
}

// releaseListJSON is a release list as GitHub returns it, newest-created
// first: a backport (v0.5.9) cut after v0.6.0, plus a prerelease that must not
// count.
const releaseListJSON = `[
  {"tag_name":"v0.5.9","published_at":"2026-10-09T12:00:00Z","assets":[
    {"name":"checksums.txt","state":"uploaded"},{"name":"install","state":"uploaded"},
    {"name":"install.sigstore.json","state":"uploaded"}]},
  {"tag_name":"v0.7.0-rc1","prerelease":true,"published_at":"2026-10-09T13:00:00Z","assets":[
    {"name":"checksums.txt","state":"uploaded"},{"name":"install","state":"uploaded"},
    {"name":"install.sigstore.json","state":"uploaded"}]},
  {"tag_name":"v0.6.0","published_at":"2026-10-08T12:00:00Z","assets":[
    {"name":"checksums.txt","state":"uploaded"},{"name":"install","state":"uploaded"},
    {"name":"install.sigstore.json","state":"uploaded"}]}
]`

func TestReportUpdateCheck(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "seamlessd-update-check", r.Header.Get("User-Agent"))
		_, _ = w.Write([]byte(releaseListJSON))
	}))
	defer srv.Close()
	client := srv.Client()
	client.CheckRedirect = update.HTTPSOnlyRedirect
	f := &update.Fetcher{Client: client, URL: srv.URL, Now: time.Now}

	tests := []struct {
		name    string
		current string
		want    string
	}{
		{"older release", "0.5.4", "update available"},
		{"the newest by version, not the backport", "0.6.0", "up to date"},
		{"the backport is not newer", "0.5.9", "update available"},
		{"ahead", "0.6.1", "ahead of the newest published release"},
		{"dev build", "0.0.0-dev", "development build"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, reportUpdateCheck(context.Background(), &buf, f, "linux", tt.current, tt.current+"+abc1234", nil))
			out := buf.String()
			require.Contains(t, out, tt.want)
			require.Contains(t, out, "v0.6.0", "newest is the max version, never the prerelease")
			require.NotContains(t, out, "0.7.0")
			require.Contains(t, out, tt.current+"+abc1234")
		})
	}
}

func TestReportUpdateCheck_NoInstallableRelease(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(releaseListJSON))
	}))
	defer srv.Close()
	client := srv.Client()
	client.CheckRedirect = update.HTTPSOnlyRedirect
	f := &update.Fetcher{Client: client, URL: srv.URL, Now: time.Now}

	var buf bytes.Buffer
	// None of the releases carries the Windows installer pair.
	require.NoError(t, reportUpdateCheck(context.Background(), &buf, f, "windows", "0.6.0", "0.6.0", nil))
	require.Contains(t, buf.String(), "no installable release for windows")
}

func TestReportUpdateCheck_RateLimitHint(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	client := srv.Client()
	client.CheckRedirect = update.HTTPSOnlyRedirect
	f := &update.Fetcher{Client: client, URL: srv.URL, Now: time.Now}

	err := reportUpdateCheck(context.Background(), io.Discard, f, "linux", "0.6.0", "0.6.0", nil)
	require.ErrorIs(t, err, update.ErrRateLimited)
	require.ErrorContains(t, err, "SEAMLESS_VERSION")
}

// The daemon's rows come from its own records, so --check prints them even
// when the release list cannot be read: after a failed or paused update is
// when the owner needs them, and the network may well be why.
func TestReportUpdateCheck_DaemonRowsWithoutTheReleaseList(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	client := srv.Client()
	client.CheckRedirect = update.HTTPSOnlyRedirect
	f := &update.Fetcher{Client: client, URL: srv.URL, Now: time.Now}

	now := time.Now()
	dir := t.TempDir()
	require.NoError(t, update.SaveState(dir, autoRecord(now, func(s *update.State) {
		s.Paused = &update.Pause{Reason: update.PauseRollbacks, Versions: []update.Version{v073, v074}, At: now.Add(-time.Hour)}
	})))
	cfg := config.Defaults()
	cfg.DataDir = dir
	view := loadUpdateView(context.Background(), cfg, nil)

	var buf bytes.Buffer
	err := reportUpdateCheck(context.Background(), &buf, f, "linux", "0.7.2", "0.7.2+abc1234", &view)
	require.ErrorIs(t, err, update.ErrUnavailable)
	out := buf.String()
	for _, want := range []string{
		"current  0.7.2+abc1234",
		"newest   unknown -- the release list could not be read",
		"mode     notify -- automatic updates paused themselves",
		"paused   since ",
	} {
		require.Contains(t, out, want)
	}
}

// With the release list read, the daemon's rows follow the verdict.
func TestReportUpdateCheck_DaemonRowsFollowTheVerdict(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(releaseListJSON))
	}))
	defer srv.Close()
	client := srv.Client()
	client.CheckRedirect = update.HTTPSOnlyRedirect
	f := &update.Fetcher{Client: client, URL: srv.URL, Now: time.Now}

	now := time.Now()
	dir := t.TempDir()
	require.NoError(t, update.SaveState(dir, autoRecord(now, func(s *update.State) {
		s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
	})))
	cfg := config.Defaults()
	cfg.DataDir = dir
	view := loadUpdateView(context.Background(), cfg, nil)

	var buf bytes.Buffer
	require.NoError(t, reportUpdateCheck(context.Background(), &buf, f, "linux", "0.5.4", "0.5.4", &view))
	out := buf.String()
	status := strings.Index(out, "update available")
	target := strings.Index(out, "target   v0.7.3")
	require.Positive(t, status, out)
	require.Greater(t, target, status, "the daemon's rows come after the verdict:\n%s", out)
}

// These use TLS servers because fetchInstaller refuses plain http outright --
// its output is piped to a shell (see update.RequireHTTPS). srv.Client() trusts the
// throwaway cert; the scheme rules under test are unchanged.
func TestFetchInstaller(t *testing.T) {
	fetch := func(srv *httptest.Server) (string, error) {
		client := srv.Client()
		client.CheckRedirect = update.HTTPSOnlyRedirect
		return fetchInstallerWith(client, srv.URL)
	}

	t.Run("returns the script body", func(t *testing.T) {
		const script = "#!/bin/sh\necho hi\n"
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(script))
		}))
		defer srv.Close()
		got, err := fetch(srv)
		require.NoError(t, err)
		require.Equal(t, script, got)
	})

	t.Run("non-200 is an error", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()
		_, err := fetch(srv)
		require.Error(t, err)
		require.Contains(t, err.Error(), "404")
	})

	t.Run("empty response is an error", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("   \n"))
		}))
		defer srv.Close()
		_, err := fetch(srv)
		require.Error(t, err)
		require.Contains(t, err.Error(), "empty")
	})
}
