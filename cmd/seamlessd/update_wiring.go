package main

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"runtime"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/update"
)

// updateCheckerDeps assembles the background update check's dependencies for
// this daemon: the build stamp, how it was installed (update.Detect over the
// probe in install_probe.go), the file/env update config with the console's
// override read live from the settings table, the GitHub fetcher and the
// event log. It is split from runServe so a test can build the same checker
// over a fake fetcher and probe.
//
// It also wires automatic updates:
//
//   - Spawner, the updater spawner for this OS (updateSpawnerFor): nil on a
//     build that is not a published release, so a source build never reports
//     an updater wired (Status.CanApply) and never installs anything;
//   - LiveSessions and SessionIdle, the sessions store.LiveSessionCount finds
//     heartbeating within gardener.session_idle_minutes -- the threshold the
//     console's live badge and the session reaper use;
//   - Activity, the snapshot of the request tracker that wraps the daemon's
//     routes (daemonHandler), so pass the same tracker; nil leaves activity
//     unknown, and only the overdue deadline applies;
//   - UpdaterRunning and CodexHooksHash, the probes in update_probes.go.
//
// The checker reads none of these on a source build (its automatic-update
// step returns first, and only a release build fingerprints at startup), and
// no probe creates anything when it is read, so wiring them is harmless there
// (constraint dev-and-fixture-daemons-never-self-update).
func updateCheckerDeps(cfg config.Config, db *sql.DB, rec update.Recorder, instance string, install update.Install, activity *activityTracker, logger *slog.Logger) update.Deps {
	d := update.Deps{
		DataDir:      cfg.DataDir,
		Version:      version,
		Distribution: distribution,
		GOOS:         runtime.GOOS,
		Install:      install,
		Instance:     instance,
		PID:          os.Getpid(),
		LocalHost:    config.Hostname(),
		Base:         cfg.Update,
		Override: func(ctx context.Context) (config.UpdateOverride, error) {
			o, _, err := store.UpdateOverride(ctx, db)
			return o, err
		},
		Fetcher: update.NewFetcher(),
		Events:  rec,
		Log:     logger,

		Spawner: updateSpawnerFor(cfg, logger),
		LiveSessions: func(ctx context.Context, cutoff time.Time) (int, error) {
			return store.LiveSessionCount(ctx, db, cutoff)
		},
		SessionIdle:    time.Duration(cfg.Gardener.SessionIdleMinutes) * time.Minute,
		UpdaterRunning: func() (bool, error) { return updaterLockHeld(cfg.DataDir) },
		CodexHooksHash: codexHooksFingerprint,
		// A staged restore holds automatic updates (and Update now) off: the
		// update would back up and replace the binary under the swap.
		Holding: restoreHolding(cfg.DataDir),
	}
	if activity != nil {
		d.Activity = activity.snapshot
	}
	return d
}

// updateSpawnerFor is the daemon's update.Spawner: newUpdateSpawner's, on a
// published release build only (update.IsReleaseBuild). newUpdateSpawner
// checks neither the build nor the install kind, and the checker reports an
// updater wired whenever it has a Spawner, so without this gate doctor and the
// console would offer automatic updates on a developer's daemon. The install
// kind stays the checker's call: a release that the installer did not make
// has a Spawner and is still never updated unattended (Install.NotifyOnly).
// nil also when newUpdateSpawner has none (no way to detach the updater on
// this OS, no executable path, no config file loaded); the install then only
// notifies.
func updateSpawnerFor(cfg config.Config, logger *slog.Logger) update.Spawner {
	if !update.IsReleaseBuild(distribution, version) {
		return nil
	}
	return newUpdateSpawner(cfg, logger)
}

// newUpdateChecker builds the daemon's update checker over activity, the
// tracker that wraps its routes. Nothing runs until Start, which runServe
// calls only after the listener is bound.
func newUpdateChecker(cfg config.Config, db *sql.DB, rec update.Recorder, instance string, activity *activityTracker, logger *slog.Logger) *update.Checker {
	install := update.Detect(probeInstall(distribution, version, installConfigOf(cfg, absConfigPath(cfg.SourcePath()))))
	chk := update.New(updateCheckerDeps(cfg, db, rec, instance, install, activity, logger))
	st := chk.Status()
	mode, reason := st.Mode()
	slog.Info("update check", "mode", string(mode), "install", string(install.Kind), "reason", reason,
		"distribution", distribution, "can_apply", st.CanApply)
	return chk
}

// healthzBody is the /healthz JSON: liveness, the build, and -- with an
// update check wired -- this process's instance id and the newest release
// when the check found one newer than this build. The instance lets a caller
// tell this daemon from another answering on the same port (another user's
// on a shared Windows box), and from the daemon it replaced.
func healthzBody(status string, instance string, st *update.Status) map[string]string {
	body := map[string]string{
		"status": status, "version": buildVersion(), "commit": commit, "built": buildDate,
		"distribution": distribution,
	}
	if instance != "" {
		body["instance"] = instance
	}
	if st != nil && st.Settings.Check && st.Available && st.Newest != nil {
		body["update_available"] = st.Newest.Version.String()
	}
	return body
}
