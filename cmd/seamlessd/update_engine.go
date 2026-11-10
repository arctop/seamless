package main

// The updater engine: plan 2.2 steps 1-9, shared by the daemon's unattended
// `seamlessd update --auto` and the owner's attended `seamlessd update` on an
// installer-managed install. It takes the update lock, records the attempt
// (update.Attempt) from its first write to its last, and moves through the
// stages in order -- gates, fetch, verify, preflight, backup, install,
// confirm, and a rollback when an installed release does not come up.
//
// The engine records; it does not judge. Whether a failure blocks a release,
// backs off, or turns automatic updates off is the daemon's call (its fold
// over attempts.jsonl), so every outcome here is written down exactly as the
// attempt contract classifies it, and nothing else.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/releasesig"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/update"
)

// updaterTiming paces the engine.
type updaterTiming struct {
	// lockWait and lockPoll pace taking the update lock (runRecorded).
	lockWait, lockPoll time.Duration
	confirm            confirmTiming
	// installer bounds one installer run.
	installer time.Duration
	// daemonStop is how long a rollback waits for the stopped daemon to let
	// go of the data dir before it leaves the database alone.
	daemonStop time.Duration
	// finalTries and finalRetry pace the final attempt.json write, which can
	// fail on Windows while the daemon has the file open.
	finalTries int
	finalRetry time.Duration
}

var defaultUpdaterTiming = updaterTiming{
	lockWait:   updateLockWait,
	lockPoll:   updateLockPoll,
	confirm:    defaultConfirmTiming,
	installer:  installerTimeout,
	daemonStop: dataDirLockWait,
	finalTries: 5,
	finalRetry: 500 * time.Millisecond,
}

// updaterDeps is everything the updater reads, runs or waits on, injected so a
// test drives the network, the machine, the clock and the installer.
type updaterDeps struct {
	goos, goarch string
	// version and distribution are this build's; buildVersion is what the
	// backup's manifest records.
	version, distribution, buildVersion string
	// environ is this process's environment, which the installer inherits
	// (less SEAMLESS_* when unattended); home is the user's home directory.
	environ []string
	home    string
	// stdout is the attended terminal; nil for an unattended run, whose only
	// output is the attempt log.
	stdout io.Writer
	now    func() time.Time
	ticker func(time.Duration) (<-chan time.Time, func())

	// releases reads one release and its assets; listReleases reads the
	// release list (the attended path's newest release).
	releases     releaseSource
	listReleases func(ctx context.Context) ([]update.APIRelease, error)
	// verify checks an asset against its Sigstore bundle with the exact
	// identity of release v's tag (releasesig.VerifyAsset).
	verify func(bundle, artifact []byte, v update.Version) error

	// probe gathers update.Detect's probe for configPath (probeInstall).
	probe func(configPath string, client bool) update.Probe
	// selfCheck refuses an unattended run inside the service's own process
	// tree (outsideServiceCheck).
	selfCheck func() error
	// autoAllowed refuses an unattended run once the owner has turned
	// automatic updates off (autoUpdateAllowed), with an error wrapping
	// errAutoOff; any other error is a failure to read the settings.
	autoAllowed func(ctx context.Context, cfg config.Config) error
	// service runs one lifecycle verb on the installed service.
	service func(action serviceAction) (bool, string)
	// runInstaller runs a release's install script (runInstallerScript).
	runInstaller func(installerRun) error
	freeBytes    func(dir string) (uint64, error)
	sameFile     func(a, b string) bool

	timing updaterTiming
}

// realUpdaterDeps reads and runs the live machine. stdout is the attended
// terminal, nil for --auto.
func realUpdaterDeps(stdout io.Writer) updaterDeps {
	home := homeDir()
	return updaterDeps{
		goos: runtime.GOOS, goarch: runtime.GOARCH,
		version: version, distribution: distribution, buildVersion: buildVersion(),
		environ: os.Environ(), home: home, stdout: stdout,
		now: time.Now,
		ticker: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
		releases: newReleaseSource(),
		listReleases: func(ctx context.Context) ([]update.APIRelease, error) {
			page, err := update.NewFetcher().Fetch(ctx, "")
			return page.Releases, err
		},
		verify: releasesig.Embedded(),
		probe: func(configPath string, client bool) update.Probe {
			return probeInstall(distribution, version, configPath, client)
		},
		selfCheck:   func() error { return outsideServiceCheck(runtime.GOOS, os.ReadFile) },
		autoAllowed: autoUpdateAllowed,
		service: func(action serviceAction) (bool, string) {
			plan := serviceControl(action, runtime.GOOS, home, os.Getuid())
			ok, out := runControlCmds(plan.Cmds)
			if !ok && len(plan.Fallback) > 0 {
				ok, out = runControlCmds(plan.Fallback)
			}
			return ok, out
		},
		runInstaller: runInstallerScript,
		freeBytes:    freeBytes,
		sameFile:     sameFile,
		timing:       defaultUpdaterTiming,
	}
}

// errInsideService is an unattended updater running where the installer's
// service restart would kill it.
var errInsideService = errors.New("the updater must run outside the service's process tree")

// jobObjectLimitKillOnJobClose is Windows' JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE:
// a job with it kills every process in it once its last handle closes.
const jobObjectLimitKillOnJobClose = 0x2000

// selfProcess is what outsideServiceCheck reads about this process beyond
// /proc: its process group (darwin) and its job (windows).
type selfProcess struct {
	pid, pgid int
	inJob     bool
	jobLimits uint32 // the job's JOB_OBJECT_LIMIT_* flags, when inJob
}

// outsideServiceCheck is the updater's self-check that it does NOT run where
// stopping the service it is about to restart would take it down too -- the
// inverse of Detect's gate 9 -- or the installer's bootout, restart or
// Stop-ScheduledTask would kill it mid-install and leave the swap half done.
// The rule per OS is how that OS stops a service, as the 2.01 spikes measured
// it, and what the daemon's spawner (update_spawn.go) does to escape it:
//
//   - linux: systemd stops the unit's whole cgroup, so /proc/self/cgroup must
//     not place this process in the seamless.service unit (inSystemdUnit). A
//     transient unit of its own passes.
//   - darwin: launchd signals the job's process group, so this process must
//     lead a group of its own (pgid == pid). A Setsid child and a launchd
//     job's main process pass; a plain child of the daemon does not. Not
//     XPC_SERVICE_NAME, nor the parent pid: an orphaned updater inherits the
//     one and shares ppid 1 with the daemon.
//   - windows: Task Scheduler's stop ends only the task's own process, which is
//     `serve`, never this `update --auto`, and leaves the rest of its job
//     running; install.ps1 stops only `serve` processes. What would still take
//     this process down is a job that kills its members when it closes, so
//     that, read from its innermost job, is what it refuses. Not "the daemon
//     is not in my job": an updater the update task starts shares the
//     daemon's job and survives.
//
// Unreadable answers fail closed, and so does every other OS, where nothing
// starts the updater.
func outsideServiceCheck(goos string, readFile func(string) ([]byte, error)) error {
	return checkOutsideService(goos, readFile, ownProcess)
}

// checkOutsideService is outsideServiceCheck over an injected reader of this
// process's group and job.
func checkOutsideService(goos string, readFile func(string) ([]byte, error), self func() (selfProcess, error)) error {
	switch goos {
	case "linux":
		cg, err := readFile("/proc/self/cgroup")
		if err != nil {
			return fmt.Errorf("%w: cannot read /proc/self/cgroup: %w", errInsideService, err)
		}
		if inSystemdUnit(cg, systemdUnit) {
			return fmt.Errorf("%w: this process is in the %s unit, which the installer restarts", errInsideService, systemdUnit)
		}
		return nil
	case "darwin":
		p, err := self()
		if err != nil {
			return fmt.Errorf("%w: cannot read this process's group: %w", errInsideService, err)
		}
		if p.pgid != p.pid {
			return fmt.Errorf("%w: this process is in process group %d, not one of its own, and launchd stops the service by signalling its job's group", errInsideService, p.pgid)
		}
		return nil
	case "windows":
		p, err := self()
		if err != nil {
			return fmt.Errorf("%w: cannot read this process's job: %w", errInsideService, err)
		}
		if p.inJob && p.jobLimits&jobObjectLimitKillOnJobClose != 0 {
			return fmt.Errorf("%w: this process is in a job that kills its processes when it closes", errInsideService)
		}
		return nil
	default:
		return fmt.Errorf("%w: on %s nothing starts the updater outside the service, so this cannot be checked", errInsideService, goos)
	}
}

// autoUpdateAllowed re-reads whether the owner still allows automatic
// updates -- the file/env update block with the console's stored override,
// merged the restrictive way (update.Effective) -- because the daemon decided
// to spawn on what it read a moment before, and check: false means no update
// traffic at all. The override is read through store.OpenExisting, never
// migrated, and an unreadable one fails closed.
func autoUpdateAllowed(ctx context.Context, cfg config.Config) error {
	db, err := store.OpenExisting(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("read the update settings: %w", err)
	}
	override, _, err := store.UpdateOverride(ctx, db)
	if cerr := db.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("read the update settings: %w", err)
	}
	if s := update.Effective(cfg.Update, override, true); !s.Auto {
		return fmt.Errorf("%w (set by %s)", errAutoOff, s.AutoSource)
	}
	return nil
}

// errAutoOff is autoUpdateAllowed finding automatic updates turned off -- as
// opposed to failing to read whether they are, which refuses too but is no
// refusal of the owner's.
var errAutoOff = errors.New("automatic updates are off")

// refusalError is a gate refusing an unattended attempt: refusal is the
// update.Refusal* word the record carries, and err the reason, which reads
// in the record's Error exactly as it did before refusals had a word.
type refusalError struct {
	refusal string
	err     error
}

func (e *refusalError) Error() string { return e.err.Error() }
func (e *refusalError) Unwrap() error { return e.err }

// refused marks err as the gates' refusal with word refusal.
func refused(refusal string, err error) error { return &refusalError{refusal: refusal, err: err} }

// updaterProbe adapts Detect's probe to the updater, which is by design not
// the service's own process: gate 9 ("this process is the service's
// supervised instance") always fails for it, so it is taken as met here, and
// replaced by the opposite check, outsideServiceCheck, for an unattended run.
// An attended run also ignores the installer's own knobs in the environment:
// gate 7 is there because the service would lose them, but an owner who sets
// SEAMLESS_VERSION for `seamlessd update` is aiming it, and the knobs reach
// the installer, not the service.
func updaterProbe(p update.Probe, attended bool) update.Probe {
	p.Supervised, p.SupervisedReason = true, ""
	if attended {
		p.ForeignEnv = slices.DeleteFunc(slices.Clone(p.ForeignEnv), func(name string) bool {
			return slices.Contains(installerKnobs, name)
		})
	}
	return p
}

// updateJob is one recorded run of the engine.
type updateJob struct {
	req        update.SpawnRequest
	cfg        config.Config
	configPath string       // absolute
	probe      update.Probe // updaterProbe'd; Detect is re-run from it
	// attended is the owner's `seamlessd update`: progress rows on the
	// terminal, the installer prompting there, no client or skill knobs, and
	// the owner's own knobs winning. installDir is then already decided
	// (installDirFor) and newest is the newest release, when known, for the
	// floor fallback.
	attended   bool
	installDir string
	newest     update.Version
}

// outcome is how an attempt ended, as the attempt record states it.
type outcome struct {
	stage      update.Stage
	ok         bool
	rolledBack bool
	err        string
	// refusal is the gates' refusal word (refusalError), "" for anything else.
	refusal string
}

// updater is one engine run.
type updater struct {
	d   updaterDeps
	job updateJob
	rec *attemptRecorder
	log io.Writer // the attempt log; nil when it could not be opened

	shape        installShape
	backup       backupResult
	prevInstance string
	prevPID      int
}

func (u *updater) dataDir() string { return u.job.cfg.DataDir }

// logf writes one line to the attempt log.
func (u *updater) logf(format string, args ...any) {
	if u.log == nil {
		return
	}
	fmt.Fprintf(u.log, "%s %s\n", u.d.now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

// note reports a step: a row on the attended terminal and a log line.
func (u *updater) note(label, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	if u.job.attended && u.d.stdout != nil {
		fieldRowTo(u.d.stdout, label, text)
	}
	u.logf("%s: %s", label, text)
}

// stage moves the attempt to s.
func (u *updater) stage(s update.Stage) {
	u.logf("stage %s", s)
	u.rec.set(func(a *update.Attempt) { a.Stage = s })
}

// fail ends the attempt at stage s.
func (u *updater) fail(s update.Stage, err error) outcome {
	u.logf("failed at %s: %v", s, err)
	return outcome{stage: s, err: fmt.Sprintf("%s: %v", s, err)}
}

// The update lock is taken with a short wait rather than a single try,
// because the daemon probes whether an updater is alive by try-locking the
// same file and releasing it at once. A probe holds it for microseconds, so a
// freshly started updater whose own try landed in that instant would
// otherwise exit unrecorded -- which the daemon then folds as an interrupted
// attempt and backs off from for an hour. A real concurrent updater holds the
// lock for minutes, so "still held after updateLockWait" keeps meaning
// "another update is running".
const (
	updateLockWait = 10 * time.Second
	updateLockPoll = 100 * time.Millisecond
)

// runRecorded takes the update lock, records the attempt from its first write
// (StageLock) to its last, runs the engine in between and releases the lock
// after the history line. Only the lock's holder writes attempt.json and
// attempts.jsonl, so a lock it cannot take -- still held after
// timing.lockWait by another updater, or on a file system that cannot lock
// -- is an error with nothing recorded.
func runRecorded(ctx context.Context, d updaterDeps, job updateJob) (update.Attempt, error) {
	dataDir := job.cfg.DataDir
	if err := os.MkdirAll(update.StateDir(dataDir), 0o700); err != nil {
		return update.Attempt{}, fmt.Errorf("create %s: %w", update.StateDir(dataDir), err)
	}
	lockPath := update.LockPath(dataDir)
	lock, err := waitLockFile(ctx, lockPath, d.timing.lockWait, d.timing.lockPoll, func(_, holder string) {
		if job.attended && d.stdout != nil {
			fieldRowTo(d.stdout, "lock", dim("held by "+holder+"; waiting up to "+d.timing.lockWait.String()))
		}
	})
	switch {
	case errors.Is(err, errLockHeld):
		return update.Attempt{}, fmt.Errorf("another update is running: %w", err)
	case errors.Is(err, errLockUnsupported):
		return update.Attempt{}, fmt.Errorf("cannot lock %s on this file system, so another update cannot be ruled out: %w", lockPath, err)
	case err != nil:
		return update.Attempt{}, err
	}
	defer func() { _ = lock.Close() }()

	u := &updater{d: d, job: job}
	logFile, logPath, logErr := openUpdateLog(dataDir, job.req.AttemptID)
	if logErr == nil {
		defer func() { _ = logFile.Close() }()
		u.log = logFile
	}
	rec, err := startAttempt(dataDir, job.req, logPath, d.now, d.ticker, u.logf)
	if err != nil {
		return update.Attempt{}, fmt.Errorf("record the attempt: %w", err)
	}
	u.rec = rec
	if logErr != nil {
		u.note("log", "%s", yellow("none")+dim(" -- "+logErr.Error()))
	}
	u.logf("attempt %s: v%s -> v%s (%s)", job.req.AttemptID, job.req.From, job.req.To, job.req.Why)

	out := u.run(ctx)
	final, err := rec.finish(out, d.timing)
	if err != nil {
		u.logf("final record: %v", err)
	}
	u.logf("finished at %s ok=%t rolled_back=%t", final.Stage, final.OK, final.RolledBack)
	return final, err
}

// run is the engine proper, from the gates to the outcome.
func (u *updater) run(ctx context.Context) outcome {
	u.stage(update.StageGates)
	if err := u.gates(ctx); err != nil {
		o := u.fail(update.StageGates, err)
		var r *refusalError
		if errors.As(err, &r) {
			o.refusal = r.refusal
		}
		return o
	}

	u.stage(update.StageFetch)
	dl, floor, err := u.fetchTarget(ctx)
	if err != nil {
		return u.fail(fetchFailureStage(err), err)
	}

	u.stage(update.StageVerify)
	target, err := verifyDownload(dl, u.d.goos, u.d.goarch, u.d.verify)
	if err != nil {
		return u.fail(update.StageVerify, err)
	}
	if floor {
		target.version = u.job.req.To
		u.note("verify", "%s", green("signature verified")+dim(fmt.Sprintf(" -- the v%s installer, which installs v%s and checks its archive itself", target.signedFor, target.version)))
	} else {
		u.note("verify", "%s", green("signature verified")+dim(fmt.Sprintf(" -- installer and checksums.txt signed by the release workflow on v%s; %s pinned", target.version, target.archive)))
	}

	u.stage(update.StagePreflight)
	back, err := u.preflight(ctx)
	if err != nil {
		return u.fail(update.StagePreflight, err)
	}
	u.note("rollback", "v%s verified and ready, should v%s not come up", back.version, target.version)

	u.stage(update.StageBackup)
	if err := u.takeBackup(ctx); err != nil {
		return u.fail(update.StageBackup, err)
	}

	u.stage(update.StageInstall)
	return u.install(ctx, target, back)
}

// fetchFailureStage is where a failed fetch stops. StageFetch for what may
// clear by itself -- the network's failure, and a release caught mid-upload
// (errReleaseInFlight) -- which the daemon backs off from. StageVerify for the
// release's own state, which it blocks the release on: gone or a draft (the
// tag re-read's 404), pulled (prerelease), or published without an asset the
// update needs.
func fetchFailureStage(err error) update.Stage {
	if errors.Is(err, errFetchNetwork) || errors.Is(err, errReleaseInFlight) {
		return update.StageFetch
	}
	return update.StageVerify
}

// gates re-checks that this update may run now. An unattended one re-runs
// Detect (with gate 9 replaced, updaterProbe), requires this binary to be the
// release the attempt updates from, runs the outside-the-service self-check,
// re-reads whether automatic updates are still allowed -- for an automatic
// attempt (WhyAuto) only: one the owner asked the daemon for runs whatever
// update.auto says -- and observes the install's shape. An attended one
// passed Detect before it took the lock and has its install dir already, and
// never refuses here.
//
// Each refusal is a refusalError carrying its update.Refusal* word, so the
// record says why in a word the surfaces can act on. Two failures here carry
// none and stay plain failures at StageGates: settings that could not be read
// (no owner's decision to report), and an install shape the updater cannot
// agree on (errShapeMismatch: what disagrees varies -- the service's program,
// a client's hooks -- and no one fixed action resolves it; the record's Error
// names it).
func (u *updater) gates(ctx context.Context) error {
	if u.job.attended {
		u.shape = installShape{dir: u.job.installDir}
		return nil
	}
	if inst := update.Detect(u.job.probe); inst.Kind != update.KindInstaller {
		return refused(update.RefusalNotInstaller, fmt.Errorf("not an installer-managed install (%s): %s", inst.Kind, inst.Reason))
	}
	if cur, ok := update.Parse(u.d.version); !ok || cur != u.job.req.From {
		return refused(update.RefusalStaleBinary, fmt.Errorf("the installed seamlessd is not v%s, the release this attempt updates from; restart the service so it runs the installed release", u.job.req.From))
	}
	if err := u.d.selfCheck(); err != nil {
		return refused(update.RefusalSelfCheck, err)
	}
	if u.job.req.Why == update.WhyAuto {
		switch err := u.d.autoAllowed(ctx, u.job.cfg); {
		case errors.Is(err, errAutoOff):
			return refused(update.RefusalAutoOff, err)
		case err != nil:
			return err
		}
	}
	shape, err := observeInstallShape(u.shapeEnv(), u.job.cfg, u.job.configPath, u.job.probe.Service, u.job.probe.Exe)
	if err != nil {
		return err
	}
	u.shape = shape
	for _, n := range shape.notes {
		u.logf("shape: %s", n)
	}
	u.logf("shape: install dir %s, clients [%s], skills left alone [%s]",
		shape.dir, strings.Join(shape.clients, ","), strings.Join(shape.skipSkills, ","))
	return nil
}

func (u *updater) shapeEnv() shapeEnv {
	return shapeEnv{goos: u.d.goos, home: u.d.home, environ: u.d.environ, sameFile: u.d.sameFile}
}

// fetchTarget re-reads the release the attempt installs by its tag and
// downloads its installer, checksums.txt and both bundles. The re-read is
// what catches a release pulled since the daemon chose it (a draft, a
// prerelease, gone), and a release published without any asset the update
// needs -- its checksums bundle included -- is refused rather than
// half-verified. A release still uploading one, or an asset the re-read lists
// as uploaded that does not download yet, is errReleaseInFlight: transient.
//
// The one exception is attended: a release the owner pinned that predates
// checksums bundles (the floor) is installed the way it always was, by the
// NEWEST release's installer, verified with that release's own tag, which
// fetches and checks the pinned release's archive itself. floor reports it.
func (u *updater) fetchTarget(ctx context.Context) (releaseDownload, bool, error) {
	to := u.job.req.To
	rel, err := u.d.releases.releaseByTag(ctx, to)
	if err != nil {
		return releaseDownload{}, false, fmt.Errorf("re-read v%s: %w", to, err)
	}
	archiveName := platformArchive(to, u.d.goos, u.d.goarch)
	err = checkPublished(rel, to, releaseAssetNames(to, u.d.goos, u.d.goarch))
	if err == nil {
		dl, err := u.d.releases.downloadRelease(ctx, to, u.d.goos, true)
		if err != nil {
			return releaseDownload{}, false, fmt.Errorf("download v%s: %w", to, listedAssetGone(err))
		}
		return dl, false, nil
	}
	if !u.job.attended || !errors.Is(err, errReleaseIncomplete) || checkPublished(rel, to, []string{archiveName}) != nil {
		return releaseDownload{}, false, err
	}
	newest := u.job.newest
	if newest.IsZero() {
		if newest, err = newestRelease(ctx, u.d); err != nil {
			return releaseDownload{}, false, fmt.Errorf("v%s predates signed checksums, and the newest release to install it with is unknown: %w", to, err)
		}
	}
	dl, err := u.d.releases.downloadRelease(ctx, newest, u.d.goos, false)
	if err != nil {
		// The release list only names a newest release whose installer and
		// bundle are uploaded (update.Filter).
		return releaseDownload{}, false, fmt.Errorf("download the v%s installer: %w", newest, listedAssetGone(err))
	}
	u.note("checksums", "%s", yellow("not pinned")+dim(fmt.Sprintf(" -- v%s predates signed checksums; the v%s installer checks its archive (and its signature, with cosign installed)", to, newest)))
	return dl, true, nil
}

// newestRelease is the highest installable release in the release list
// (update.Filter, update.Newest) -- never GitHub's "latest", which a backport
// can make an older version.
func newestRelease(ctx context.Context, d updaterDeps) (update.Version, error) {
	list, err := d.listReleases(ctx)
	if err != nil {
		return update.Version{}, err
	}
	newest, ok := update.Newest(update.Filter(list, d.goos))
	if !ok {
		return update.Version{}, fmt.Errorf("no installable release for %s in the release list", d.goos)
	}
	return newest.Version, nil
}

// preflight proves the rollback before anything changes (plan step 4): the
// release the attempt updates from must still be downloadable and verify --
// its installer, its checksums.txt and both bundles, with that release's own
// tag identity -- and list this platform's archive, which must answer a HEAD.
// The verified installer and checksums pin are kept for the rollback: no
// rollback path, no update.
func (u *updater) preflight(ctx context.Context) (releaseAssets, error) {
	from := u.job.req.From
	dl, err := u.d.releases.downloadRelease(ctx, from, u.d.goos, true)
	if err != nil {
		return releaseAssets{}, fmt.Errorf("download v%s, the rollback target: %w", from, err)
	}
	back, err := verifyDownload(dl, u.d.goos, u.d.goarch, u.d.verify)
	if err != nil {
		return releaseAssets{}, fmt.Errorf("verify v%s, the rollback target: %w", from, err)
	}
	if err := u.d.releases.headAsset(ctx, from, back.archive); err != nil {
		return releaseAssets{}, fmt.Errorf("v%s's archive, for the rollback: %w", from, err)
	}
	return back, nil
}

// takeBackup is plan step 5 (takeBackup does the work), recording the
// archive's path on the attempt.
func (u *updater) takeBackup(ctx context.Context) error {
	res, err := takeBackup(ctx, backupRequest{
		dataDir: u.dataDir(), from: u.job.req.From, attemptID: u.job.req.AttemptID,
		at: u.d.now(), exe: u.job.probe.Exe, version: u.d.buildVersion, freeBytes: u.d.freeBytes,
	})
	if err != nil && !errors.Is(err, errBackupPrune) {
		return err
	}
	if err != nil {
		u.logf("backup: %v", err)
	}
	u.backup = res
	u.rec.set(func(a *update.Attempt) { a.BackupPath = res.path })
	u.note("backup", "%s", dim(tildePath(res.path)+fmt.Sprintf(" (schema v%d)", res.schema)))
	return nil
}

// installerEnv is the environment for running r's installer. Unattended: the
// inherited environment without SEAMLESS_*, plus the observed shape and r's
// pins. Attended: the owner's environment, plus the install dir and r's pins
// where the owner set none; rollback forces r's pins over the owner's.
func (u *updater) installerEnv(r releaseAssets, rollback bool) ([]string, error) {
	if u.job.attended {
		knobs := mergeKnobs(map[string]string{"SEAMLESS_INSTALL_DIR": u.shape.dir}, releaseKnobs(r))
		var force []string
		if rollback {
			force = []string{"SEAMLESS_VERSION", "SEAMLESS_CHECKSUMS_SHA256"}
		}
		return attendedInstallerEnv(u.d.goos, u.d.environ, knobs, force...)
	}
	shape, err := u.shape.knobs()
	if err != nil {
		return nil, err
	}
	return unattendedInstallerEnv(u.d.goos, u.d.environ, mergeKnobs(shape, releaseKnobs(r)))
}

// runInstaller runs r's verified installer with env: its output to the
// attempt log, and to the terminal as well when attended.
func (u *updater) runInstaller(r releaseAssets, env []string) error {
	out := io.Discard
	switch {
	case u.job.attended && u.d.stdout != nil && u.log != nil:
		out = io.MultiWriter(u.d.stdout, u.log)
	case u.job.attended && u.d.stdout != nil:
		out = u.d.stdout
	case u.log != nil:
		out = u.log
	}
	if u.job.attended && u.d.stdout != nil {
		fmt.Fprintf(u.d.stdout, "\n%s\n", dim(fmt.Sprintf("running the v%s installer...", r.signedFor)))
	}
	u.logf("running the v%s installer for v%s (SEAMLESS_VERSION=%s)", r.signedFor, r.version, r.version)
	err := u.d.runInstaller(installerRun{
		script: r.installer, env: env, out: out,
		unattended: !u.job.attended, timeout: u.d.timing.installer,
	})
	u.logf("installer exited: %v", errText(err))
	if u.job.attended && u.d.stdout != nil {
		fmt.Fprintln(u.d.stdout)
	}
	return err
}

// errText renders an error for the log, "ok" for none.
func errText(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// swapped reports whether the installed seamlessd changed since the backup
// hashed it (H0). A binary that cannot be read is taken as changed: the
// installer replaces it by rename, so a missing or half-written one is the
// install's doing, and treating it as changed is what puts the rollback
// target back.
func (u *updater) swapped() bool {
	sum, err := fileSHA256(u.job.probe.Exe)
	if err != nil {
		u.logf("hash %s after the installer: %v", u.job.probe.Exe, err)
		return true
	}
	return sum != u.backup.binSHA
}

// confirmDrillName is the rollback drill's switch: while
// <data_dir>/update/test-fail-confirm exists, the confirmation after an
// install fails at once, as if the new release never came up, so the owner
// can exercise the whole rollback on a real install by creating the file. It
// never fails the confirmation of the rollback itself.
const confirmDrillName = "test-fail-confirm"

// confirm waits for want to serve (confirmServing). drill applies the
// rollback drill's switch.
func (u *updater) confirm(ctx context.Context, want update.Version, drill bool) confirmResult {
	if drill {
		if _, err := os.Lstat(filepath.Join(update.StateDir(u.dataDir()), confirmDrillName)); err == nil {
			return confirmResult{reason: "the rollback drill is on (" + confirmDrillName + " exists)"}
		}
	}
	client, err := u.job.cfg.HTTPClient(u.d.timing.confirm.request)
	if err != nil {
		return confirmResult{reason: err.Error()}
	}
	return confirmServing(ctx, confirmParams{
		want: want, client: client, baseURL: u.job.cfg.ServerURL(), dataDir: u.dataDir(),
		prevInstance: u.prevInstance, prevPID: u.prevPID,
		timing: u.d.timing.confirm, now: u.d.now, ticker: u.d.ticker,
	})
}

// install runs the target's installer and decides what happened (plan steps
// 6-8). The verdict rests on observation: whether the installed binary
// changed (its hash against H0), and whether the release then confirms.
//
//   - Confirmed: StageDone, OK. A non-zero installer exit with the release
//     serving is "applied with warnings" (typically the client wiring after
//     the restart failed).
//   - Not installed -- the binary unchanged after a failed run, or after a
//     "successful" one to a different release: StageInstall, nothing to roll
//     back. On Windows the installer stops the daemon before it swaps, so the
//     service is started again first.
//   - Installed and not confirmed: the rollback (StageRollback).
//
// A reinstall of the running release swaps nothing (same bytes), so there a
// clean installer exit counts as installed.
func (u *updater) install(ctx context.Context, target, back releaseAssets) outcome {
	if st, err := update.ReadState(u.dataDir()); err != nil {
		u.logf("read the update state before the install: %v", err)
	} else {
		u.prevInstance, u.prevPID = st.Running.Instance, st.Running.PID
	}
	env, err := u.installerEnv(target, false)
	if err != nil {
		return u.fail(update.StageInstall, err)
	}
	runErr := u.runInstaller(target, env)
	swapped := u.swapped()
	if !swapped && (runErr != nil || target.version != u.job.req.From) {
		reason := fmt.Sprintf("the installer exited with %s and seamlessd is unchanged", errText(runErr))
		if runErr == nil {
			reason = "the installer exited cleanly but did not replace seamlessd"
		}
		if u.d.goos == "windows" {
			if ok, out := u.d.service(actionStart); ok {
				reason += fmt.Sprintf("; v%s restarted", u.job.req.From)
			} else {
				reason += fmt.Sprintf("; starting v%s again failed (%s), so the daemon may be down: run seamlessd start", u.job.req.From, firstLine(out))
			}
		}
		u.note("install", "%s", yellow("not installed")+dim(" -- "+reason))
		return outcome{stage: update.StageInstall, err: "install: " + reason}
	}

	u.stage(update.StageConfirm)
	res := u.confirm(ctx, target.version, true)
	if res.ok {
		u.note("confirm", "%s", green(fmt.Sprintf("v%s serving", target.version))+dim(" (instance "+res.instance+")"))
		o := outcome{stage: update.StageDone, ok: true}
		if runErr != nil {
			o.err = fmt.Sprintf("applied with warnings: v%s is serving, but the installer exited with %s; run seamlessd install-hooks to finish wiring the clients", target.version, errText(runErr))
			u.note("warning", "%s", yellow(o.err))
		}
		return o
	}
	cause := res.reason
	if runErr != nil {
		cause = fmt.Sprintf("the installer exited with %s; %s", errText(runErr), res.reason)
	}
	u.note("confirm", "%s", yellow(fmt.Sprintf("v%s not confirmed", target.version))+dim(" -- "+cause))
	return u.rollback(ctx, target, back, cause)
}

// rollback restores the release the attempt updated from (plan step 8): stop
// the service; once the daemon has let go of the data dir, put the backup's
// database back if the new release migrated it past the backup's schema and
// the backup's holds every console lock the migrated one does (the migrated
// one is kept aside, never deleted); run the rollback target's
// verified installer pinned to it; and confirm it the same way. Markdown is
// never restored here.
func (u *updater) rollback(ctx context.Context, target, back releaseAssets, cause string) outcome {
	u.stage(update.StageRollback)
	u.note("rollback", "restoring v%s", back.version)
	var notes []string
	if ok, out := u.d.service(actionStop); !ok {
		notes = append(notes, "stopping the service failed: "+firstLine(out))
	}
	if u.waitDaemonGone(ctx) {
		if n := u.restoreDBIfMigrated(ctx); n != "" {
			notes = append(notes, n)
		}
	} else {
		notes = append(notes, "the daemon did not let go of the data dir, so the database was left as it is")
	}
	for _, n := range notes {
		u.note("rollback", "%s", dim(n))
	}

	env, err := u.installerEnv(back, true)
	if err != nil {
		return u.rollbackFailed(target, back, cause, err.Error(), notes)
	}
	runErr := u.runInstaller(back, env)
	res := u.confirm(ctx, back.version, false)
	if !res.ok {
		reason := res.reason
		if runErr != nil {
			reason = fmt.Sprintf("its installer exited with %s; %s", errText(runErr), res.reason)
		}
		return u.rollbackFailed(target, back, cause, reason, notes)
	}
	u.note("rollback", "%s", green(fmt.Sprintf("v%s serving again", back.version))+dim(" (instance "+res.instance+")"))
	msg := fmt.Sprintf("v%s did not come up (%s); rolled back to v%s", target.version, cause, back.version)
	if len(notes) > 0 {
		msg += "; " + strings.Join(notes, "; ")
	}
	return outcome{stage: update.StageRollback, rolledBack: true, err: msg}
}

// rollbackFailed is a rollback that could not confirm its target: the daemon
// may be down, and the record says how to recover.
func (u *updater) rollbackFailed(target, back releaseAssets, cause, reason string, notes []string) outcome {
	moved := u.dataDir() + ".broken"
	msg := fmt.Sprintf("v%s did not come up (%s), and the rollback to v%s was not confirmed (%s); the daemon may be down: "+
		"check it with seamlessd status, reinstall v%s with SEAMLESS_VERSION=%s and the installer one-liner, "+
		"and to bring the data back as it was, stop the daemon, move the data dir aside to %s and run seamlessd import --from %s",
		target.version, cause, back.version, reason, back.version, back.version, moved, movedBackupPath(u.dataDir(), moved, u.backup.path))
	if len(notes) > 0 {
		msg += "; " + strings.Join(notes, "; ")
	}
	u.note("rollback", "%s", yellow("not confirmed")+dim(" -- "+reason))
	return outcome{stage: update.StageRollback, err: msg}
}

// movedBackupPath is where the backup sits once the data dir has moved aside
// to moved: the backup lives inside the data dir (backups/), so the path the
// updater wrote it to is gone after the move. A backup outside the data dir
// keeps its own path.
func movedBackupPath(dataDir, moved, backup string) string {
	rel, err := filepath.Rel(dataDir, backup)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return backup
	}
	return filepath.Join(moved, rel)
}

// waitDaemonGone waits until no process holds the data dir's lock, up to
// timing.daemonStop: a database may only move while nothing has it open.
func (u *updater) waitDaemonGone(ctx context.Context) bool {
	tick, stop := u.d.ticker(dataDirLockPoll)
	defer stop()
	deadline := u.d.now().Add(u.d.timing.daemonStop)
	for {
		if held, _ := dataDirLockHolder(u.dataDir()); !held {
			return true
		}
		if !u.d.now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick:
		}
	}
}

// restoreDBIfMigrated puts the backup's database back when the schema moved
// past the backup's (S1 > S0), and says what it did. Wherever it keeps the
// migrated database instead, the rollback target runs on the newer schema,
// which release discipline keeps working.
//
// A restore never reduces the set of locked projects (owner decision,
// 2026-11-10). The console's locks live only in the database, so a lock the
// owner turned on after the backup is in the migrated database alone: its
// console locks are read before any file moves (its WAL is part of it), and
// the backup's database goes in place only if it holds every one. When either
// side's locks cannot be read, that cannot be shown, and the migrated database
// stays. The check runs on the extracted snapshot, before the reset below
// writes to it and before it replaces seam.db, so no moment leaves a database
// missing a lock as seam.db. A lock only the backup holds -- removed after it
// -- comes back with it, which fails closed.
//
// The backup also holds the access leases, console unlocks and read grants
// live when it was taken, including ones revoked since, so every live one in
// it is ended before anything serves it; when that cannot be done, the
// migrated database goes back instead rather than serve permissions the owner
// had closed.
func (u *updater) restoreDBIfMigrated(ctx context.Context) string {
	dbPath := filepath.Join(u.dataDir(), archive.DBName)
	s1, err := dbSchemaVersion(dbPath)
	if err != nil {
		return "could not read the database's schema, so it was left as it is: " + err.Error()
	}
	if s1 <= u.backup.schema {
		u.logf("rollback: schema v%d, not past the backup's v%d; database left as it is", s1, u.backup.schema)
		return ""
	}
	locked, err := dbConsoleLocks(ctx, dbPath)
	if err != nil {
		return fmt.Sprintf("could not read the console locks of the database (schema v%d), so it was left as it is "+
			"and the rollback target runs on a newer schema: %v", s1, err)
	}
	attempt := u.job.req.AttemptID
	var relocked []string
	suffix, err := restoreDatabase(u.dataDir(), u.backup.path, attempt, u.backup.schema, keepConsoleLocks(ctx, locked, &relocked))
	var missing *missingLocksError
	if errors.As(err, &missing) {
		return fmt.Sprintf("%v (added after the backup), so the migrated database (v%d) stays and the rollback target runs on a newer schema", missing, s1)
	}
	if err != nil {
		return fmt.Sprintf("the database (schema v%d) could not be restored to v%d, so the rollback target runs on a newer schema: %v", s1, u.backup.schema, err)
	}
	ended, err := resetRestoredAccess(ctx, u.dataDir(), u.d.now())
	if err != nil {
		if uerr := unrestoreDatabase(u.dataDir(), suffix, attempt); uerr != nil {
			return fmt.Sprintf("the database was restored to schema v%d, but its access leases could not be ended (%v), "+
				"and the migrated database could not be put back either (%v): stop the daemon and run seamlessd doctor", u.backup.schema, err, uerr)
		}
		return fmt.Sprintf("the backup's access leases could not be ended (%v), so the migrated database (v%d) was put back "+
			"and the rollback target runs on a newer schema", err, s1)
	}
	msg := fmt.Sprintf("the database was restored to schema v%d from the backup; the migrated one (v%d) is kept as %s%s", u.backup.schema, s1, archive.DBName, suffix)
	if ended.Total() > 0 {
		msg += fmt.Sprintf("; ended what the backup held live: %s (locked projects ask again)", restoredAccessWords(ended))
	}
	switch len(relocked) {
	case 0:
	case 1:
		msg += "; the backup's console lock on " + relocked[0] + ", removed after the backup, is back"
	default:
		msg += "; the backup's console locks on " + andList(relocked) + ", removed after the backup, are back"
	}
	return msg
}

// firstLine is the first line of a command's output, for a record.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if line == "" {
		return "no output"
	}
	return line
}

// attemptRecorder writes an attempt's record (update.Attempt) while the engine
// runs: at every stage change, from a heartbeat timer at least every
// update.AttemptHeartbeatInterval (one step -- the installer, the wait to
// confirm -- can take minutes), and a last time with the outcome, which also
// goes to the history. A failed write before the last is left to the next
// heartbeat; the last is retried.
type attemptRecorder struct {
	dataDir string
	now     func() time.Time
	logf    func(format string, args ...any)

	mu sync.Mutex
	a  update.Attempt

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// startAttempt writes the attempt's first record, at StageLock, and starts
// its heartbeat. An error means nothing was recorded.
func startAttempt(dataDir string, req update.SpawnRequest, logPath string, now func() time.Time,
	ticker func(time.Duration) (<-chan time.Time, func()), logf func(string, ...any)) (*attemptRecorder, error) {
	t := now().UTC()
	r := &attemptRecorder{
		dataDir: dataDir, now: now, logf: logf,
		a: update.Attempt{
			ID: req.AttemptID, From: req.From, To: req.To, Why: req.Why,
			StartedAt: t, HeartbeatAt: t, Stage: update.StageLock, LogPath: logPath,
		},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if err := update.WriteAttempt(dataDir, r.a); err != nil {
		return nil, err
	}
	tick, stopTick := ticker(update.AttemptHeartbeatInterval)
	go func() {
		defer close(r.done)
		defer stopTick()
		for {
			select {
			case <-r.stop:
				return
			case <-tick:
				r.set(func(*update.Attempt) {})
			}
		}
	}()
	return r, nil
}

// set changes the record and writes it, stamping the heartbeat.
func (r *attemptRecorder) set(change func(*update.Attempt)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	change(&r.a)
	r.a.HeartbeatAt = r.now().UTC()
	if err := update.WriteAttempt(r.dataDir, r.a); err != nil {
		r.logf("write attempt.json (left to the next write): %v", err)
	}
}

// finish stops the heartbeat, writes the outcome as the final record --
// retried, since on Windows the rename over attempt.json fails while the
// daemon reads it -- and appends that record to the history once.
func (r *attemptRecorder) finish(o outcome, timing updaterTiming) (update.Attempt, error) {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.now().UTC()
	r.a.Stage, r.a.OK, r.a.RolledBack, r.a.Error, r.a.Refusal = o.stage, o.ok, o.rolledBack, o.err, o.refusal
	r.a.FinishedAt, r.a.HeartbeatAt = t, t
	var werr error
	for try := range max(timing.finalTries, 1) {
		if werr = update.WriteAttempt(r.dataDir, r.a); werr == nil || errors.Is(werr, update.ErrInvalidAttempt) {
			break
		}
		r.logf("write the final attempt.json (try %d): %v", try+1, werr)
		retry := time.NewTimer(timing.finalRetry)
		<-retry.C
	}
	if err := update.AppendAttempt(r.dataDir, r.a); err != nil {
		werr = errors.Join(werr, err)
	}
	return r.a, werr
}
