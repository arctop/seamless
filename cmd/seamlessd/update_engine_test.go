package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/backup"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
	"github.com/arctop/seamless/internal/update"
)

// requireRecorded holds a finished attempt to the contract the daemon's fold
// reads: finished, the stage given, OK exactly at done, and the same record
// appended to the history exactly once.
func requireRecorded(t *testing.T, w *world, a update.Attempt, stage update.Stage, rolledBack bool) {
	t.Helper()
	require.True(t, a.Finished(), "the attempt must be finished")
	require.Equal(t, stage, a.Stage, "error: %s", a.Error)
	require.Equal(t, stage == update.StageDone, a.OK)
	require.Equal(t, rolledBack, a.RolledBack)
	hist, skipped, err := update.ReadAttemptHistory(w.dataDir)
	require.NoError(t, err)
	require.Zero(t, skipped)
	require.Len(t, hist, 1)
	require.Equal(t, a.ID, hist[0].ID)
	require.Equal(t, a.Stage, hist[0].Stage)
	require.Equal(t, a.OK, hist[0].OK)
	require.Equal(t, a.RolledBack, hist[0].RolledBack)
	require.NotEmpty(t, a.LogPath)
	require.FileExists(t, a.LogPath)
	if !a.OK {
		require.NotEmpty(t, a.Error)
	}
	require.NotContains(t, a.Error, "test-key", "the API key must stay out of the record")
}

func TestAutoUpdate_InstallsConfirmsAndRecords(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})

	a, err := w.runAuto("0.7.3")
	require.NoError(t, err)
	requireRecorded(t, w, a, update.StageDone, false)
	require.Empty(t, a.Error)
	require.Equal(t, update.WhyAuto, a.Why)
	require.Equal(t, "0.7.2", a.From.String())
	require.Equal(t, "0.7.3", a.To.String())

	require.Len(t, w.installs, 1)
	run := w.installs[0]
	require.True(t, run.unattended)
	require.Equal(t, "#!/bin/sh\n# the v0.7.3 installer\n", string(run.script))
	env := envOf(t, run.env)
	require.Equal(t, "0.7.3", env["SEAMLESS_VERSION"])
	require.Equal(t, w.pin("0.7.3"), env["SEAMLESS_CHECKSUMS_SHA256"])
	require.Equal(t, w.installDir, env["SEAMLESS_INSTALL_DIR"])
	require.Equal(t, "1", env["SEAMLESS_NO_HOOKS"], "no client is wired in the temp HOME, so none is added")
	for name := range env {
		if strings.HasPrefix(name, "SEAMLESS_") {
			require.Contains(t, installerKnobs, name, "%s reached the installer", name)
		}
	}
	require.Equal(t, "/usr/bin:/bin", env["PATH"])
	require.Equal(t, w.home, env["HOME"])
	require.Contains(t, env, "TMPDIR")

	// The backup: 0600 in a 0700 directory, named for the release it replaces.
	require.FileExists(t, a.BackupPath)
	require.True(t, strings.HasPrefix(filepath.Base(a.BackupPath), "pre-update-v0.7.2-"))
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(a.BackupPath)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
		di, err := os.Stat(filepath.Dir(a.BackupPath))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o700), di.Mode().Perm())
	}

	logged, err := os.ReadFile(a.LogPath)
	require.NoError(t, err)
	for _, s := range update.Stages {
		switch s {
		case update.StageLock, update.StageRollback, update.StageDone:
			// Lock is the record's first write, before the log's first stage
			// line; done is the outcome ("finished at done"); no rollback ran.
			continue
		}
		require.Contains(t, string(logged), "stage "+string(s))
	}
	require.Contains(t, string(logged), "finished at done")
	require.NotContains(t, string(logged), "test-key")
	require.NotContains(t, string(logged), "leak", "the inherited SEAMLESS_MCP_API_KEY must not be logged")

	ver, inst := w.daemon.serving()
	require.Equal(t, "0.7.3", ver)
	require.NotEqual(t, "instance-1", inst)
	require.Empty(t, w.actions, "a confirmed update touches no service")
}

// Every failure the updater can meet before it installs anything is recorded
// at the stage the daemon's fold tells them apart by, and the installer never
// runs.
func TestAutoUpdate_RecordsEachPreInstallFailureAtItsStage(t *testing.T) {
	foreign := "https://github.com/evil/seamless/.github/workflows/release.yml@refs/tags/v0.7.3"
	tests := []struct {
		name  string
		setup func(w *world)
		stage update.Stage
	}{
		{"the tag re-read finds a prerelease (the yank)", func(w *world) {
			w.publish("0.7.3", releaseOpts{prerelease: true})
		}, update.StageVerify},
		{"the tag re-read finds no release", func(*world) {}, update.StageVerify},
		{"the API is unavailable", func(w *world) {
			w.publish("0.7.3", releaseOpts{})
			w.setStatus("v0.7.3", http.StatusServiceUnavailable)
		}, update.StageFetch},
		{"the API rate limits", func(w *world) {
			w.publish("0.7.3", releaseOpts{})
			w.setStatus("v0.7.3", http.StatusTooManyRequests)
		}, update.StageFetch},
		{"an asset download fails", func(w *world) {
			w.publish("0.7.3", releaseOpts{})
			w.setStatus("v0.7.3/install", http.StatusBadGateway)
		}, update.StageFetch},
		// Caught mid-upload: transient, so the daemon backs off rather than
		// blocking a release that is about to be complete.
		{"an asset is still uploading", func(w *world) {
			w.publish("0.7.3", releaseOpts{})
			w.setAssetState("0.7.3", "checksums.txt.sigstore.json", "open")
		}, update.StageFetch},
		{"an asset the release lists as uploaded does not download yet", func(w *world) {
			w.publish("0.7.3", releaseOpts{})
			w.setStatus("v0.7.3/checksums.txt", http.StatusNotFound)
		}, update.StageFetch},
		// Published without it: the release's own state, so a block.
		{"an asset was removed from the release", func(w *world) {
			w.publish("0.7.3", releaseOpts{})
			w.removeAsset("0.7.3", "install.sigstore.json")
		}, update.StageVerify},
		{"the installer is signed for another tag", func(w *world) {
			w.publish("0.7.3", releaseOpts{installerSigner: signedFor("0.7.2")})
		}, update.StageVerify},
		{"checksums.txt is signed by another repository", func(w *world) {
			w.publish("0.7.3", releaseOpts{checksumsSigner: foreign})
		}, update.StageVerify},
		{"the release ships no checksums bundle", func(w *world) {
			w.publish("0.7.3", releaseOpts{noChecksumsSign: true})
		}, update.StageVerify},
		{"checksums.txt lists the archive twice", func(w *world) {
			name := platformArchive(mustVersion(t, "0.7.3"), "linux", "amd64")
			w.publish("0.7.3", releaseOpts{checksums: strings.Repeat("b", 64) + "  " + name + "\n" + strings.Repeat("c", 64) + "  " + name + "\n"})
		}, update.StageVerify},
		{"checksums.txt does not list this platform", func(w *world) {
			w.publish("0.7.3", releaseOpts{checksums: strings.Repeat("b", 64) + "  seamless_0.7.3_linux_arm64.tar.gz\n"})
			w.removeAsset("0.7.3", platformArchive(mustVersion(t, "0.7.3"), "linux", "amd64"))
		}, update.StageVerify},
		{"the rollback target does not verify", func(w *world) {
			w.publish("0.7.3", releaseOpts{})
			w.publish("0.7.2", releaseOpts{installerSigner: signedFor("0.7.1")})
		}, update.StagePreflight},
		{"the rollback target ships no checksums bundle", func(w *world) {
			w.publish("0.7.3", releaseOpts{})
			w.publish("0.7.2", releaseOpts{noChecksumsSign: true})
		}, update.StagePreflight},
		{"the rollback target's archive is gone", func(w *world) {
			w.publish("0.7.3", releaseOpts{})
			w.publish("0.7.2", releaseOpts{})
			w.removeAsset("0.7.2", platformArchive(mustVersion(t, "0.7.2"), "linux", "amd64"))
		}, update.StagePreflight},
		{"no room for the backup", func(w *world) {
			w.publish("0.7.3", releaseOpts{})
			w.freeSpace = 1
		}, update.StageBackup},
		{"not an installer install", func(w *world) {
			w.publish("0.7.3", releaseOpts{})
			w.probeEdits = func(p *update.Probe) { p.Service.Marker = false }
		}, update.StageGates},
		{"inside the service's process tree", func(w *world) {
			w.publish("0.7.3", releaseOpts{})
			w.selfErr = errInsideService
		}, update.StageGates},
		{"automatic updates were turned off", func(w *world) {
			w.publish("0.7.3", releaseOpts{})
			w.autoErr = errors.New("automatic updates are off (set by console)")
		}, update.StageGates},
		{"the service runs a binary elsewhere", func(w *world) {
			w.publish("0.7.3", releaseOpts{})
			other := filepath.Join(w.home, "elsewhere", "seamlessd")
			require.NoError(t, os.MkdirAll(filepath.Dir(other), 0o700))
			require.NoError(t, os.WriteFile(other, []byte("x"), 0o755))
			w.probeEdits = func(p *update.Probe) { p.Service.Program = other }
		}, update.StageGates},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, "linux", "amd64", "0.7.2")
			w.publish("0.7.2", releaseOpts{}) // a setup may publish it again, its own way
			tt.setup(w)
			a, err := w.runAuto("0.7.3")
			require.Error(t, err)
			requireRecorded(t, w, a, tt.stage, false)
			require.Empty(t, w.installs, "the installer must never run")
			require.Empty(t, w.actions)
			ver, _ := w.daemon.serving()
			require.Equal(t, "0.7.2", ver)
		})
	}
}

// The binary the updater runs must be the release the attempt updates from:
// otherwise the rollback would restore a release that was not installed.
func TestAutoUpdate_RefusesWhenTheInstalledBinaryIsNotFrom(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	d := w.deps(nil)
	d.version = "0.7.1"
	f, err := parseUpdateFlags(updaterArgs(update.SpawnRequest{
		AttemptID: testAttemptID(t), From: mustVersion(t, "0.7.2"), To: mustVersion(t, "0.7.3"), Why: update.WhyAuto,
	}, w.cfgPath)[1:])
	require.NoError(t, err)
	require.Error(t, runAutoUpdate(f, d))
	a, err := update.ReadAttempt(w.dataDir)
	require.NoError(t, err)
	requireRecorded(t, w, a, update.StageGates, false)
	require.Contains(t, a.Error, "not v0.7.2")
	require.Equal(t, update.RefusalStaleBinary, a.Refusal, "the word the surfaces key the owner's restart on")
	hist, _, err := update.ReadAttemptHistory(w.dataDir)
	require.NoError(t, err)
	require.Equal(t, update.RefusalStaleBinary, hist[0].Refusal)
}

// Each gate that refuses an unattended attempt records its refusal word next
// to the error, which reads as it always did; a failure in the gates that is
// no refusal -- settings that could not be read, an install shape the
// updater cannot agree on -- records none, and every other stage none either.
func TestAutoUpdate_GateRefusalsRecordTheirWord(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(w *world)
		stage   update.Stage
		refusal string
		errText string
	}{
		{"not an installer install", func(w *world) {
			w.probeEdits = func(p *update.Probe) { p.Service.Marker = false }
		}, update.StageGates, update.RefusalNotInstaller, "gates: not an install that updates itself"},
		{"inside the service's process tree", func(w *world) {
			w.selfErr = fmt.Errorf("%w: this process is in the seamless.service unit", errInsideService)
		}, update.StageGates, update.RefusalSelfCheck, "gates: the updater must run outside the service's process tree: this process is in the seamless.service unit"},
		{"a self-check that cannot read its answer fails closed, refused the same", func(w *world) {
			w.selfErr = fmt.Errorf("%w: cannot read /proc/self/cgroup: no such file", errInsideService)
		}, update.StageGates, update.RefusalSelfCheck, "gates: the updater must run outside the service's process tree: cannot read"},
		{"automatic updates were turned off", func(w *world) {
			w.autoErr = fmt.Errorf("%w (set by console)", errAutoOff)
		}, update.StageGates, update.RefusalAutoOff, "gates: automatic updates are off (set by console)"},
		{"a restore is waiting to finish", func(w *world) {
			require.NoError(t, os.MkdirAll(backup.RestoreRoot(w.dataDir), 0o700))
			require.NoError(t, os.WriteFile(backup.PendingPath(w.dataDir), []byte(`{"stage":"staged"}`), 0o600))
		}, update.StageGates, update.RefusalRestorePending, "gates: a restore is waiting to finish"},
		{"the settings could not be read", func(w *world) {
			w.autoErr = errors.New("read the update settings: database is locked")
		}, update.StageGates, "", "gates: read the update settings: database is locked"},
		{"the service runs a binary elsewhere (the install's shape)", func(w *world) {
			other := filepath.Join(w.home, "elsewhere", "seamlessd")
			require.NoError(t, os.MkdirAll(filepath.Dir(other), 0o700))
			require.NoError(t, os.WriteFile(other, []byte("x"), 0o755))
			w.probeEdits = func(p *update.Probe) { p.Service.Program = other }
		}, update.StageGates, "", "gates: "},
		{"a later stage", func(w *world) { w.freeSpace = 1 }, update.StageBackup, "", "backup: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, "linux", "amd64", "0.7.2")
			w.publish("0.7.2", releaseOpts{})
			w.publish("0.7.3", releaseOpts{})
			tt.setup(w)
			a, err := w.runAuto("0.7.3")
			require.Error(t, err)
			requireRecorded(t, w, a, tt.stage, false)
			require.Equal(t, tt.refusal, a.Refusal)
			require.True(t, strings.HasPrefix(a.Error, tt.errText), "error: %s", a.Error)
			hist, _, err := update.ReadAttemptHistory(w.dataDir)
			require.NoError(t, err)
			require.Equal(t, tt.refusal, hist[0].Refusal, "the history line carries the same word")
			require.Empty(t, w.installs)
		})
	}
}

func TestAutoUpdate_InstallerFailureBeforeTheSwapIsInstallNotRollback(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			w := newWorld(t, goos, "amd64", "0.7.2")
			w.publish("0.7.2", releaseOpts{})
			w.publish("0.7.3", releaseOpts{})
			w.installer = func(*world, installerRun) error { return errors.New("exit status 1") }

			a, err := w.runAuto("0.7.3")
			require.Error(t, err)
			requireRecorded(t, w, a, update.StageInstall, false)
			require.Len(t, w.installs, 1)
			require.Contains(t, a.Error, "unchanged")
			if goos == "windows" {
				// install.ps1 stops the daemon before it swaps, so the
				// updater starts it again.
				require.Equal(t, []serviceAction{actionStart}, w.actions)
				require.Equal(t, "install.ps1", installerAsset(goos))
			} else {
				require.Empty(t, w.actions)
			}
		})
	}
}

// An installer that exits 0 without replacing seamlessd installed nothing:
// that is the installer's failure, not a release to roll back.
func TestAutoUpdate_CleanExitWithoutASwapIsAnInstallFailure(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	w.installer = func(*world, installerRun) error { return nil }

	a, err := w.runAuto("0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageInstall, false)
	require.Contains(t, a.Error, "did not replace seamlessd")
}

func TestAutoUpdate_RollsBackAReleaseThatDoesNotComeUp(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	w.installer = func(w *world, run installerRun) error {
		if installerVersion(t, run) == "0.7.3" {
			// Swapped, and the new daemon never answers.
			w.daemon.stop()
			return os.WriteFile(w.exe, []byte("seamlessd 0.7.3\n"), 0o755)
		}
		return installerSwaps(w, run)
	}

	a, err := w.runAuto("0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageRollback, true)
	require.Contains(t, a.Error, "rolled back to v0.7.2")

	require.Len(t, w.installs, 2)
	back := envOf(t, w.installs[1].env)
	require.Equal(t, "0.7.2", back["SEAMLESS_VERSION"])
	require.Equal(t, w.pin("0.7.2"), back["SEAMLESS_CHECKSUMS_SHA256"])
	require.Equal(t, w.installDir, back["SEAMLESS_INSTALL_DIR"])
	require.Equal(t, "#!/bin/sh\n# the v0.7.2 installer\n", string(w.installs[1].script))
	require.Equal(t, []serviceAction{actionStop}, w.actions)

	ver, inst := w.daemon.serving()
	require.Equal(t, "0.7.2", ver)
	require.NotEqual(t, "instance-1", inst)

	// The schema did not move, so the database was left exactly where it is.
	matches, err := filepath.Glob(filepath.Join(w.dataDir, "seam.db*.pre-rollback-*"))
	require.NoError(t, err)
	require.Empty(t, matches)
}

func TestAutoUpdate_RollbackRestoresTheDatabaseOnlyWhenTheSchemaAdvanced(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	s0 := store.LatestSchemaVersion()
	w.installer = func(w *world, run installerRun) error {
		if installerVersion(t, run) == "0.7.3" {
			// The new release swaps in, migrates the database one step,
			// and then fails to come up.
			db, err := store.OpenExisting(filepath.Join(w.dataDir, "seam.db"))
			require.NoError(t, err)
			_, err = db.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, s0+1)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			w.daemon.stop()
			return os.WriteFile(w.exe, []byte("seamlessd 0.7.3\n"), 0o755)
		}
		return installerSwaps(w, run)
	}

	a, err := w.runAuto("0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageRollback, true)
	require.Contains(t, a.Error, "restored to schema")

	got, err := dbSchemaVersion(filepath.Join(w.dataDir, "seam.db"))
	require.NoError(t, err)
	require.Equal(t, s0, got, "the backup's database is back")
	aside := filepath.Join(w.dataDir, "seam.db.pre-rollback-"+a.ID)
	require.FileExists(t, aside, "the migrated database is kept, never deleted")
	migrated, err := dbSchemaVersion(aside)
	require.NoError(t, err)
	require.Equal(t, s0+1, migrated)
}

// The backup holds the access leases live when it was taken. One revoked after
// the backup (its session ended while the new release installed) must not come
// back with the restored database: every live one in it ends, and the
// rollback's record says so.
func TestAutoUpdate_ARollbackRestoreEndsTheBackupsAccessLeases(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	s0 := store.LatestSchemaVersion()
	dbPath := filepath.Join(w.dataDir, "seam.db")

	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	proc := access.ProcessToken{PID: 4242, Version: 7, Boot: "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"}
	req, err := store.CreateAccessApproval(ctx, db, store.AccessRequest{
		Project: "example-finance", Kind: store.AccessKindRequest, Lease: time.Hour, TTL: 10 * time.Minute,
		PeerUID: 501, PeerPID: proc.PID, ProcessKey: proc.Key(),
	}, time.Now())
	require.NoError(t, err)
	lease, err := store.ApproveAccessFor(ctx, db, req.ID, req.Challenge, "daemon-account", time.Hour, time.Now())
	require.NoError(t, err)
	require.NoError(t, db.Close())

	w.installer = func(w *world, run installerRun) error {
		if installerVersion(t, run) == "0.7.3" {
			// After the backup: the session ends and its lease is revoked;
			// then the new release migrates one step and fails to come up.
			db, err := store.OpenExisting(dbPath)
			require.NoError(t, err)
			require.NoError(t, store.RevokeAccessLease(ctx, db, lease.ID, store.LeaseRevokedSessionEnded, time.Now()))
			_, err = db.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, s0+1)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			w.daemon.stop()
			return os.WriteFile(w.exe, []byte("seamlessd 0.7.3\n"), 0o755)
		}
		return installerSwaps(w, run)
	}

	a, err := w.runAuto("0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageRollback, true)
	require.Contains(t, a.Error, "restored to schema")
	require.Contains(t, a.Error, "ended what the backup held live: 1 access lease")

	db, err = store.OpenExisting(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	live, err := store.ActiveAccessLeases(ctx, db, proc.Key(), time.Now())
	require.NoError(t, err)
	require.Empty(t, live, "the revoked lease did not come back with the backup")
	var reason string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT revoked_reason FROM access_leases WHERE id = ?`, lease.ID).Scan(&reason))
	require.Equal(t, store.LeaseRevokedReset, reason)
}

// rollBackAfter runs an update whose new release, once the backup is taken,
// runs during on the database, migrates it one step and fails to come up. It
// returns the rolled-back attempt.
func rollBackAfter(t *testing.T, w *world, during func(dbPath string)) update.Attempt {
	t.Helper()
	dbPath := filepath.Join(w.dataDir, "seam.db")
	w.installer = func(w *world, run installerRun) error {
		if installerVersion(t, run) == "0.7.3" {
			during(dbPath)
			execDB(t, dbPath, `INSERT INTO schema_migrations(version) SELECT MAX(version) + 1 FROM schema_migrations`)
			w.daemon.stop()
			return os.WriteFile(w.exe, []byte("seamlessd 0.7.3\n"), 0o755)
		}
		return installerSwaps(w, run)
	}
	a, err := w.runAuto("0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageRollback, true)
	ver, _ := w.daemon.serving()
	require.Equal(t, "0.7.2", ver, "the rollback target serves either way")
	return a
}

// requireDB holds the data dir's seam.db to a schema and a set of console
// locks.
func requireDB(t *testing.T, w *world, schema int, locks ...string) {
	t.Helper()
	dbPath := filepath.Join(w.dataDir, "seam.db")
	got, err := dbSchemaVersion(dbPath)
	require.NoError(t, err)
	require.Equal(t, schema, got)
	held, err := dbConsoleLocks(context.Background(), dbPath)
	require.NoError(t, err)
	require.ElementsMatch(t, locks, held)
}

// A restore never reduces the set of locked projects (owner decision,
// 2026-11-10): a lock the owner turned on while the new release ran is only in
// the migrated database, so that database stays, and the record names the
// project.
func TestAutoUpdate_ARollbackKeepsTheMigratedDatabaseForALockAddedAfterTheBackup(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	s0 := store.LatestSchemaVersion()
	consoleLockProjects(t, filepath.Join(w.dataDir, "seam.db"), "example-b")

	a := rollBackAfter(t, w, func(dbPath string) { consoleLockProjects(t, dbPath, "example-a") })
	require.Contains(t, a.Error, fmt.Sprintf("the backup's database does not hold the console lock on example-a (added after the backup), "+
		"so the migrated database (v%d) stays and the rollback target runs on a newer schema", s0+1))
	require.NotContains(t, a.Error, "restored to schema")
	requireDB(t, w, s0+1, "example-a", "example-b")
	require.Empty(t, restoreLeftovers(t, w.dataDir), "nothing was moved, so nothing is left aside")
}

func TestAutoUpdate_ARollbackRestoresABackupHoldingTheSameLocks(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	s0 := store.LatestSchemaVersion()
	consoleLockProjects(t, filepath.Join(w.dataDir, "seam.db"), "example-a", "example-b")

	a := rollBackAfter(t, w, func(string) {})
	require.Contains(t, a.Error, fmt.Sprintf("the database was restored to schema v%d from the backup", s0))
	require.NotContains(t, a.Error, "console lock")
	requireDB(t, w, s0, "example-a", "example-b")
	require.FileExists(t, filepath.Join(w.dataDir, "seam.db.pre-rollback-"+a.ID))
}

// A lock the owner removed while the new release ran comes back with the
// backup's database: that fails closed, and the record says so.
func TestAutoUpdate_ARollbackRestoresALockOnlyTheBackupHolds(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	s0 := store.LatestSchemaVersion()
	consoleLockProjects(t, filepath.Join(w.dataDir, "seam.db"), "example-a")

	a := rollBackAfter(t, w, func(dbPath string) {
		db, err := store.OpenExisting(dbPath)
		require.NoError(t, err)
		require.NoError(t, store.RemoveConsoleLock(ctx, db, "example-a"))
		require.NoError(t, db.Close())
	})
	require.Contains(t, a.Error, fmt.Sprintf("the database was restored to schema v%d from the backup", s0))
	require.Contains(t, a.Error, "; the backup's console lock on example-a, removed after the backup, is back")
	requireDB(t, w, s0, "example-a")
}

// A backup whose database predates console locks (migration 037) holds none,
// so a lock in the migrated database keeps the migrated database in place.
func TestAutoUpdate_ARollbackKeepsTheLocksABackupBeforeConsoleLocksLacks(t *testing.T) {
	const consoleLocksMigration = 37 // internal/store/migrations/037_console_locks.sql
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	dbPath := filepath.Join(w.dataDir, "seam.db")
	execDB(t, dbPath, `DROP TABLE console_locks`,
		fmt.Sprintf(`DELETE FROM schema_migrations WHERE version >= %d`, consoleLocksMigration))
	s0, err := dbSchemaVersion(dbPath)
	require.NoError(t, err)
	require.Less(t, s0, consoleLocksMigration)

	a := rollBackAfter(t, w, func(dbPath string) {
		// The new release creates the table, and the owner locks a project.
		execDB(t, dbPath, `CREATE TABLE console_locks (project_slug TEXT PRIMARY KEY, credential TEXT NOT NULL, locked_at TEXT NOT NULL)`)
		consoleLockProjects(t, dbPath, "example-a")
	})
	require.Contains(t, a.Error, fmt.Sprintf("the backup's database does not hold the console lock on example-a (added after the backup), "+
		"so the migrated database (v%d) stays", s0+1))
	requireDB(t, w, s0+1, "example-a")
	require.Empty(t, restoreLeftovers(t, w.dataDir))
}

// Console locks that cannot be read cannot be shown to survive the restore,
// so the database is left exactly as it is.
func TestAutoUpdate_ARollbackLeavesADatabaseWhoseLocksCannotBeRead(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	s0 := store.LatestSchemaVersion()

	a := rollBackAfter(t, w, func(dbPath string) {
		execDB(t, dbPath, `INSERT INTO console_locks (project_slug, credential, locked_at) VALUES ('example-a', 'cred', 'not a time')`)
	})
	require.Contains(t, a.Error, fmt.Sprintf("could not read the console locks of the database (schema v%d), so it was left as it is "+
		"and the rollback target runs on a newer schema", s0+1))
	got, err := dbSchemaVersion(filepath.Join(w.dataDir, "seam.db"))
	require.NoError(t, err)
	require.Equal(t, s0+1, got)
	require.Empty(t, restoreLeftovers(t, w.dataDir))
}

func TestAutoUpdate_ARollbackThatFailsSaysTheDaemonMayBeDown(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	w.installer = func(w *world, run installerRun) error {
		w.daemon.stop()
		return os.WriteFile(w.exe, []byte("seamlessd "+installerVersion(t, run)+"\n"), 0o755)
	}

	a, err := w.runAuto("0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageRollback, false)
	require.Contains(t, a.Error, "may be down")
	// The backup lives in <data>/backups, so the recovery text names it where
	// it sits after the data dir moves aside.
	data := filepath.Dir(filepath.Dir(a.BackupPath))
	require.Contains(t, a.Error, "move the data dir aside to "+data+".broken")
	require.Contains(t, a.Error, "seamlessd import --from "+filepath.Join(data+".broken", "backups", filepath.Base(a.BackupPath)))
}

func TestAutoUpdate_ANonZeroExitWithTheReleaseServingIsAppliedWithWarnings(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	w.installer = func(w *world, run installerRun) error {
		require.NoError(t, installerSwaps(w, run))
		return errors.New("exit status 1")
	}

	a, err := w.runAuto("0.7.3")
	require.NoError(t, err)
	requireRecorded(t, w, a, update.StageDone, false)
	require.True(t, strings.HasPrefix(a.Error, "applied with warnings: "), a.Error)
	require.Len(t, w.installs, 1)
}

// The rollback drill: a file under the data dir fails the confirmation of the
// update, never of the rollback.
func TestAutoUpdate_TheRollbackDrillFailsTheConfirmation(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	require.NoError(t, os.MkdirAll(update.StateDir(w.dataDir), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(update.StateDir(w.dataDir), confirmDrillName), nil, 0o600))

	a, err := w.runAuto("0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageRollback, true)
	require.Contains(t, a.Error, "rollback drill")
}

// A daemon answering as the new release that the update state does not name
// -- another user's daemon on the port, say -- does not confirm it.
func TestAutoUpdate_AnUnrecordedDaemonDoesNotConfirm(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	w.installer = func(w *world, run installerRun) error {
		if installerVersion(t, run) == "0.7.3" {
			w.daemon.startUnrecorded("0.7.3")
			return os.WriteFile(w.exe, []byte("seamlessd 0.7.3\n"), 0o755)
		}
		return installerSwaps(w, run)
	}
	a, err := w.runAuto("0.7.3")
	require.Error(t, err)
	requireRecorded(t, w, a, update.StageRollback, true)
	require.Contains(t, a.Error, "update state does not name")
}

// A lock held for the whole wait is another updater: nothing is recorded,
// because only the lock's holder writes attempt.json.
func TestAutoUpdate_ALockHeldByAnotherUpdaterRecordsNothing(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	require.NoError(t, os.MkdirAll(update.StateDir(w.dataDir), 0o700))
	held, err := tryLockFile(update.LockPath(w.dataDir))
	require.NoError(t, err)
	defer func() { _ = held.Close() }()

	start := time.Now()
	a, err := w.runAuto("0.7.3")
	require.ErrorIs(t, err, errLockHeld)
	require.Contains(t, err.Error(), "another update is running")
	require.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond, "it waited out the lock wait first")
	require.Empty(t, a.ID, "only the lock's holder writes attempt.json")
	require.NoFileExists(t, update.AttemptPath(w.dataDir))
	require.NoFileExists(t, update.AttemptHistoryPath(w.dataDir))
	require.NoDirExists(t, updateLogDir(w.dataDir))
	require.Empty(t, w.installs)
}

// The daemon probes for a live updater by try-locking update.lock and
// letting go at once. An updater starting in that instant waits it out
// instead of exiting unrecorded.
func TestAutoUpdate_WaitsOutAMomentaryHolder(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.publish("0.7.2", releaseOpts{})
	w.publish("0.7.3", releaseOpts{})
	require.NoError(t, os.MkdirAll(update.StateDir(w.dataDir), 0o700))
	probe, err := tryLockFile(update.LockPath(w.dataDir))
	require.NoError(t, err)
	released := make(chan error, 1)
	time.AfterFunc(200*time.Millisecond, func() { released <- probe.Close() })

	d := w.deps(nil)
	d.timing.lockWait = 10 * time.Second
	f, err := parseUpdateFlags(updaterArgs(update.SpawnRequest{
		AttemptID: testAttemptID(t), From: mustVersion(t, "0.7.2"), To: mustVersion(t, "0.7.3"), Why: update.WhyAuto,
	}, w.cfgPath)[1:])
	require.NoError(t, err)
	require.NoError(t, runAutoUpdate(f, d))
	require.NoError(t, <-released)
	a, err := update.ReadAttempt(w.dataDir)
	require.NoError(t, err)
	requireRecorded(t, w, a, update.StageDone, false)
}

// Before it holds the lock the updater refuses what must never write update
// state: a source build, a client machine, a command line that does not
// validate.
func TestAutoUpdate_RefusesBeforeRecording(t *testing.T) {
	t.Run("a source build", func(t *testing.T) {
		w := newWorld(t, "linux", "amd64", "0.7.2")
		d := w.deps(nil)
		d.distribution = update.DistributionSource
		f, err := parseUpdateFlags(updaterArgs(update.SpawnRequest{
			AttemptID: testAttemptID(t), From: mustVersion(t, "0.7.2"), To: mustVersion(t, "0.7.3"), Why: update.WhyAuto,
		}, w.cfgPath)[1:])
		require.NoError(t, err)
		require.ErrorContains(t, runAutoUpdate(f, d), "release build")
		requireNothingRecorded(t, w)
	})
	t.Run("a client machine", func(t *testing.T) {
		w := newWorld(t, "linux", "amd64", "0.7.2")
		require.NoError(t, os.WriteFile(w.cfgPath, []byte("role: client\nserver_url: https://server.example:8081\nmcp:\n  api_key: k\ndata_dir: "+w.dataDir+"\n"), 0o600))
		f, err := parseUpdateFlags(updaterArgs(update.SpawnRequest{
			AttemptID: testAttemptID(t), From: mustVersion(t, "0.7.2"), To: mustVersion(t, "0.7.3"), Why: update.WhyAuto,
		}, w.cfgPath)[1:])
		require.NoError(t, err)
		require.ErrorContains(t, runAutoUpdate(f, w.deps(nil)), "role: client")
		requireNothingRecorded(t, w)
	})
	t.Run("a downgrade", func(t *testing.T) {
		w := newWorld(t, "linux", "amd64", "0.7.2")
		f, err := parseUpdateFlags([]string{"--auto", "--attempt", testAttemptID(t), "--from", "0.7.2", "--to", "0.7.1",
			"--why", "auto", "--config", w.cfgPath})
		require.NoError(t, err)
		require.ErrorIs(t, runAutoUpdate(f, w.deps(nil)), update.ErrInvalidAttempt)
		requireNothingRecorded(t, w)
	})
}

// requireNothingRecorded holds that no updater lock, attempt, history or log
// was written in w's data dir. (The update dir itself may exist: the fake
// daemon writes state.json there, as a release daemon does.)
func requireNothingRecorded(t *testing.T, w *world) {
	t.Helper()
	for _, path := range []string{
		update.LockPath(w.dataDir), update.AttemptPath(w.dataDir),
		update.AttemptHistoryPath(w.dataDir), updateLogDir(w.dataDir), backupDir(w.dataDir),
	} {
		_, err := os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist, "%s must not exist", path)
	}
}

// The settings gate re-reads update.auto for an automatic attempt only: an
// attempt the owner asked the daemon for (the console's "Update now", a why
// task 2.11 adds) runs whatever update.auto says.
func TestGates_TheSettingsGateIsForAutomaticAttemptsOnly(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.autoErr = fmt.Errorf("%w (set by console)", errAutoOff)
	u := &updater{d: w.deps(nil), job: updateJob{
		req: update.SpawnRequest{AttemptID: testAttemptID(t), From: mustVersion(t, "0.7.2"), To: mustVersion(t, "0.7.3"), Why: "now"},
		cfg: w.cfg, configPath: w.cfgPath, probe: updaterProbe(w.probe(installConfig{path: w.cfgPath}), false),
	}}
	require.NoError(t, u.gates(context.Background()))
	u.job.req.Why = update.WhyAuto
	err := u.gates(context.Background())
	require.ErrorContains(t, err, "automatic updates are off")
	var r *refusalError
	require.ErrorAs(t, err, &r)
	require.Equal(t, update.RefusalAutoOff, r.refusal)
	require.ErrorIs(t, err, errAutoOff, "the refusal wraps the gate's own error")
}

// An attended `seamlessd update` passed Detect before it took the lock: its
// gates never refuse, so it never records a refusal -- whatever every gate an
// unattended run checks would say.
func TestGates_AttendedNeverRefuses(t *testing.T) {
	w := newWorld(t, "linux", "amd64", "0.7.2")
	w.selfErr = errInsideService
	w.autoErr = errAutoOff
	d := w.deps(nil)
	d.version = "0.7.1"
	p := w.probe(installConfig{path: w.cfgPath})
	p.Service.Marker = false
	u := &updater{d: d, job: updateJob{
		req: update.SpawnRequest{AttemptID: testAttemptID(t), From: mustVersion(t, "0.7.2"), To: mustVersion(t, "0.7.3"), Why: update.WhyManual},
		cfg: w.cfg, configPath: w.cfgPath, probe: updaterProbe(p, true), attended: true, installDir: w.installDir,
	}}
	require.NoError(t, u.gates(context.Background()))
	require.Equal(t, w.installDir, u.shape.dir)
}

func TestUpdaterProbe(t *testing.T) {
	p := update.Probe{
		Supervised: false, SupervisedReason: "started by hand",
		ForeignEnv: []string{"SEAMLESS_DATA_DIR", "SEAMLESS_INSTALL_DIR", "SEAMLESS_VERSION"},
	}
	auto := updaterProbe(p, false)
	require.True(t, auto.Supervised, "the updater is never the service's process; gate 9 is replaced, not failed")
	require.Empty(t, auto.SupervisedReason)
	require.Equal(t, p.ForeignEnv, auto.ForeignEnv, "an unattended run keeps gate 7 whole")

	attended := updaterProbe(p, true)
	require.Equal(t, []string{"SEAMLESS_DATA_DIR"}, attended.ForeignEnv, "the owner's installer knobs aim the update")
	require.Len(t, p.ForeignEnv, 3, "the probe passed in is not modified")
}

// TestOutsideServiceCheck runs the self-check per OS over fakes. The cgroup
// listings are the spike's verbatim samples (install_probe_test.go).
func TestOutsideServiceCheck(t *testing.T) {
	cgroup := func(content string, err error) func(string) ([]byte, error) {
		return func(path string) ([]byte, error) {
			require.Equal(t, "/proc/self/cgroup", path)
			return []byte(content), err
		}
	}
	proc := func(p selfProcess, err error) func() (selfProcess, error) {
		return func() (selfProcess, error) { return p, err }
	}
	noCgroup := cgroup("", errors.New("only linux reads /proc"))
	noProc := proc(selfProcess{}, errors.New("linux reads only /proc"))
	tests := []struct {
		name    string
		goos    string
		read    func(string) ([]byte, error)
		self    func() (selfProcess, error)
		outside bool
		reason  string
	}{
		{"linux: the updater's transient unit", "linux", cgroup(cgroupV2Updater, nil), noProc, true, ""},
		{"linux: a login shell", "linux", cgroup(cgroupV2Shell, nil), noProc, true, ""},
		{"linux: inside seamless.service", "linux", cgroup(cgroupV2Daemon, nil), noProc, false, "seamless.service unit"},
		{"linux v1: the updater's transient unit", "linux", cgroup(cgroupV1Updater, nil), noProc, true, ""},
		{"linux v1: inside seamless.service", "linux", cgroup(cgroupV1Daemon, nil), noProc, false, "seamless.service unit"},
		{"linux: a sub-cgroup of the unit, which its stop kills", "linux",
			cgroup("0::/user.slice/user-501.slice/user@501.service/app.slice/seamless.service/x\n", nil), noProc, false, "seamless.service unit"},
		{"linux: a system unit of the same name, which the installer never restarts", "linux",
			cgroup("0::/system.slice/seamless.service\n", nil), noProc, true, ""},
		{"linux: inside a peer_auth daemon's system unit", "linux",
			cgroup("0::/system.slice/seamless-daemon.service\n", nil), noProc, false, "seamless-daemon.service system unit"},
		{"linux: a peer_auth daemon's updater, in its account's user manager", "linux",
			cgroup("0::/user.slice/user-480.slice/user@480.service/app.slice/seamless-update-01M4HFPS1DBMJXQ4CE8ZZ0JRDH.service\n", nil), noProc, true, ""},
		{"linux: unreadable", "linux", cgroup("", os.ErrPermission), noProc, false, "/proc/self/cgroup"},

		{"darwin: a Setsid child leads its own group", "darwin", noCgroup, proc(selfProcess{pid: 4242, pgid: 4242}, nil), true, ""},
		{"darwin: a launchd job's main process leads its own group", "darwin", noCgroup, proc(selfProcess{pid: 900, pgid: 900}, nil), true, ""},
		{"darwin: a plain child shares the service's group", "darwin", noCgroup, proc(selfProcess{pid: 4243, pgid: 900}, nil), false, "process group 900"},
		{"darwin: unreadable", "darwin", noCgroup, proc(selfProcess{}, errors.New("getpgid: EPERM")), false, "process's group"},

		{"windows: in no job", "windows", noCgroup, proc(selfProcess{pid: 7}, nil), true, ""},
		{"windows: the task's shared job, SILENT_BREAKAWAY_OK only", "windows", noCgroup,
			proc(selfProcess{pid: 7, inJob: true, jobLimits: 0x1000}, nil), true, ""},
		{"windows: the task's job with that flag toggled off", "windows", noCgroup,
			proc(selfProcess{pid: 7, inJob: true}, nil), true, ""},
		{"windows: a job that kills its processes when it closes", "windows", noCgroup,
			proc(selfProcess{pid: 7, inJob: true, jobLimits: jobObjectLimitKillOnJobClose}, nil), false, "kills its processes"},
		{"windows: KILL_ON_JOB_CLOSE among other limits", "windows", noCgroup,
			proc(selfProcess{pid: 7, inJob: true, jobLimits: 0x3000}, nil), false, "kills its processes"},
		{"windows: unreadable", "windows", noCgroup, proc(selfProcess{}, errors.New("IsProcessInJob: denied")), false, "process's job"},

		{"another OS starts no updater", "freebsd", noCgroup, noProc, false, "on freebsd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkOutsideService(tt.goos, tt.read, tt.self)
			if tt.outside {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, errInsideService)
			require.ErrorContains(t, err, tt.reason)
		})
	}
}

func TestAutoUpdateAllowed(t *testing.T) {
	dataDir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dataDir, "seam.db"))
	require.NoError(t, err)
	off := false
	cfg := config.Defaults()
	cfg.DataDir = dataDir

	require.NoError(t, autoUpdateAllowed(context.Background(), cfg), "unset auto is on")

	require.NoError(t, store.SetUpdateOverride(context.Background(), db, config.UpdateOverride{Auto: &off}))
	err = autoUpdateAllowed(context.Background(), cfg)
	require.ErrorContains(t, err, "automatic updates are off (set by console)")
	require.ErrorIs(t, err, errAutoOff)
	require.NoError(t, db.Close())

	fileOff := cfg
	fileOff.Update.Check = &off
	err = autoUpdateAllowed(context.Background(), fileOff)
	require.ErrorContains(t, err, "automatic updates are off", "check: false means no update traffic, so no update")
	require.ErrorIs(t, err, errAutoOff)

	missing := cfg
	missing.DataDir = filepath.Join(dataDir, "absent")
	err = autoUpdateAllowed(context.Background(), missing)
	require.Error(t, err, "an unreadable setting fails closed")
	require.NotErrorIs(t, err, errAutoOff, "but is no owner's decision: a plain failure, not a refusal")
}

// The heartbeat rewrites attempt.json from its timer, not only between steps.
func TestAttemptRecorder_HeartbeatsFromItsTimer(t *testing.T) {
	dataDir := t.TempDir()
	base := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var clock int64
	now := func() time.Time { clock++; return base.Add(time.Duration(clock) * time.Second) }
	beats := make(chan time.Time)
	ticker := func(d time.Duration) (<-chan time.Time, func()) {
		require.Equal(t, update.AttemptHeartbeatInterval, d)
		return beats, func() {}
	}
	req := update.SpawnRequest{AttemptID: testAttemptID(t), From: mustVersion(t, "0.7.2"), To: mustVersion(t, "0.7.3"), Why: update.WhyAuto}
	rec, err := startAttempt(dataDir, req, "", now, ticker, func(string, ...any) {})
	require.NoError(t, err)
	first, err := update.ReadAttempt(dataDir)
	require.NoError(t, err)
	require.Equal(t, update.StageLock, first.Stage)

	beats <- time.Now()
	require.Eventually(t, func() bool {
		a, err := update.ReadAttempt(dataDir)
		return err == nil && a.HeartbeatAt.After(first.HeartbeatAt)
	}, 5*time.Second, 5*time.Millisecond)

	final, err := rec.finish(outcome{stage: update.StageGates, err: "gates: no", refusal: update.RefusalSelfCheck}, updaterTiming{finalTries: 1})
	require.NoError(t, err)
	require.True(t, final.Finished())
	require.Equal(t, update.RefusalSelfCheck, final.Refusal)
	read, err := update.ReadAttempt(dataDir)
	require.NoError(t, err)
	require.Equal(t, update.StageGates, read.Stage)
	require.True(t, read.Finished())
	require.Equal(t, update.RefusalSelfCheck, read.Refusal)
}

// TestMovedBackupPath: the recovery text names the backup where it sits once
// the data dir has moved aside, since backups/ lives inside the data dir.
func TestMovedBackupPath(t *testing.T) {
	data := filepath.Join(t.TempDir(), "seamless")
	moved := data + ".broken"
	for _, tc := range []struct {
		name, backup, want string
	}{
		{"inside the data dir", filepath.Join(data, "backups", "pre-update-v0.7.2.tar.gz"), filepath.Join(moved, "backups", "pre-update-v0.7.2.tar.gz")},
		{"outside it", filepath.Join(filepath.Dir(data), "elsewhere", "b.tar.gz"), filepath.Join(filepath.Dir(data), "elsewhere", "b.tar.gz")},
		{"a sibling sharing the prefix", data + "-old" + string(filepath.Separator) + "b.tar.gz", data + "-old" + string(filepath.Separator) + "b.tar.gz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, movedBackupPath(data, moved, tc.backup))
		})
	}
}
