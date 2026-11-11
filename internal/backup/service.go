package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Scheduling constants.
const (
	// firstRunDelay is how long after the daemon starts the first scheduled
	// run may come, however overdue: a daemon that just started (an update, a
	// restart after a crash) serves first.
	firstRunDelay = 10 * time.Minute

	// tickInterval is how often the loop compares the wall clock with the
	// schedule. The schedule is wall-clock times in the record and the tick is
	// short, so a laptop that slept through a run catches up within a minute
	// of waking.
	tickInterval = time.Minute

	// pollInterval is how often the loop looks for a restore the CLI staged
	// (restore/pending.json): there is no signal and no PID to reach the
	// daemon with, so it notices the file.
	pollInterval = 2 * time.Second

	// waitBound caps Wait: a run stalled writing to a disconnected network or
	// cloud disk must not hold the daemon's shutdown. A run holds no database
	// handle while it writes to the destination, so abandoning it is safe;
	// its temp file is swept at the next run.
	waitBound = 20 * time.Second

	// alertFailures is how many scheduled runs in a row must fail before the
	// every-page alert shows, and alertOverdue how many intervals may pass
	// without a confirmation before it shows anyway.
	alertFailures = 2
	alertOverdue  = 3
)

// The restore records' names under <data_dir>/restore (the restore step owns
// the files; the service only notices a staged one).
const (
	RestoreDirName = "restore"
	PendingName    = "pending.json"
)

// PendingPath is <dataDir>/restore/pending.json.
func PendingPath(dataDir string) string { return filepath.Join(dataDir, RestoreDirName, PendingName) }

// StagePending is the stage a pending restore is ready to apply at.
const StagePending = "staged"

// Errors the commands return.
var (
	// ErrNotRunning: the service is not started, or stopped after a fault.
	ErrNotRunning = errors.New("backup: the backup service is not running")
	// ErrBusy: a backup is already running.
	ErrBusy = errors.New("backup: a backup is already running")
	// ErrNoSuchBackup: the ref names no backup in the current listing.
	ErrNoSuchBackup = errors.New("backup: no such backup")
)

// Recorder is the slice of the event log the service writes to
// (*events.Recorder in production).
type Recorder interface {
	Record(ctx context.Context, e core.Event) (string, error)
}

// Deps is everything the service reads or writes, injected so a test drives
// the clock, the ticks and the disks.
type Deps struct {
	// DataDir is the instance root.
	DataDir string
	// DB is the live database: the install id, the content counts and the
	// fingerprint a scheduled run compares. A run reads it only before it
	// writes.
	DB *sql.DB

	// Base is the file/env config. Override reads the console's stored
	// override on every use; an error falls back to Base alone and is shown.
	Base     config.Backup
	Override func(ctx context.Context) (config.BackupOverride, error)
	// Release is whether this is a release build: unset backup.auto is on for
	// one, off for a build from source.
	Release bool

	// Version is recorded in every manifest; Host names the backups and is
	// recorded as their source host (config.Hostname).
	Version string
	Host    string
	// ForceLocked and PasskeyRPID go into every manifest, as `seamlessd
	// export` records them (peer_auth only).
	ForceLocked []string
	PasskeyRPID string

	// Guard holds folders and files to peer_auth's rule; nil off.
	Guard *Guard
	// FreeBytes reads the space available on a directory's disk. nil: no
	// free-space guard and no free space shown.
	FreeBytes func(dir string) (uint64, error)
	// Events receives backup.* events; nil records none.
	Events Recorder
	// RequestRestart asks the daemon for a clean restart, so a restore the
	// CLI staged applies at startup. nil (Windows, which has no in-place
	// restart): a staged restore waits for the next start.
	RequestRestart func()
	// Validate checks a restore's prepared database the way serve's
	// preflight would (peer_auth's projectProblems); nil checks nothing more.
	Validate func(ctx context.Context, db *sql.DB) error
	// UpdateBusy reports an update under way, which refuses a restore; nil
	// never refuses.
	UpdateBusy func() bool

	// GOOS picks the folder rules and suggestions; "" is runtime.GOOS. Home,
	// User and Getenv feed the suggestions; zero values read the process's.
	GOOS   string
	Home   string
	User   string
	Getenv func(string) string

	Now   func() time.Time                  // nil: time.Now
	Ticks func() (<-chan time.Time, func()) // nil: a tickInterval ticker
	Polls func() (<-chan time.Time, func()) // nil: a pollInterval ticker
	// WaitBound caps Wait; zero is waitBound.
	WaitBound time.Duration
	Log       *slog.Logger // nil: slog.Default()

	// export writes an archive; nil is archive.Export. A test's stand-in
	// stalls or fails on purpose.
	export func(ctx context.Context, opts archive.ExportOptions) (archive.Manifest, error)
}

// Running is the run under way.
type Running struct {
	Kind      string    `json:"kind"`
	Scheduled bool      `json:"scheduled"`
	StartedAt time.Time `json:"started_at"`
}

// LastRun is the newest finished run of any kind, so the page the owner lands
// on after Back up now can say how it went.
type LastRun struct {
	Kind      string    `json:"kind"`
	Scheduled bool      `json:"scheduled"`
	Outcome   string    `json:"outcome"`
	At        time.Time `json:"at"`
	Name      string    `json:"name,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// Status is the service's snapshot, which the console, doctor and the alert
// render.
type Status struct {
	Settings Settings `json:"settings"`
	// SettingsError says why the console's override could not be read (the
	// file/env settings apply meanwhile).
	SettingsError string `json:"settings_error,omitempty"`

	Running *Running `json:"running,omitempty"`
	LastRun *LastRun `json:"last_run,omitempty"`

	LastAttempt   time.Time `json:"last_attempt,omitzero"`
	LastConfirmed time.Time `json:"last_confirmed,omitzero"`
	LastSuccess   time.Time `json:"last_success,omitzero"`
	LastBackup    *Record   `json:"last_backup,omitempty"`
	// UpToDate is a last scheduled run that found nothing changed.
	UpToDate bool `json:"up_to_date,omitempty"`
	// NextAt is when the next scheduled run is due (zero while off).
	NextAt time.Time `json:"next_at,omitzero"`

	// Failures counts the scheduled runs that failed in a row, the first at
	// FailingSince; LastReason and LastError describe the newest.
	Failures     int       `json:"failures,omitempty"`
	FailingSince time.Time `json:"failing_since,omitzero"`
	LastReason   string    `json:"last_reason,omitempty"`
	LastError    string    `json:"last_error,omitempty"`

	// Since is when automatic backups were first seen on: the overdue
	// alert's baseline before any confirmation.
	Since time.Time `json:"since,omitzero"`

	// Restore is the restore side: one being prepared, one staged, the last
	// one's result.
	Restore RestoreStatus `json:"restore"`

	// StateError says why the schedule's record could not be saved.
	StateError string `json:"state_error,omitempty"`
	// Stopped is a service whose loop ended after a fault.
	Stopped bool `json:"stopped,omitempty"`

	// written is the record's trace, for List off the loop.
	written []written
}

// Alert reports whether the every-page alert shows at now: automatic backups
// are on, and either the last alertFailures scheduled runs failed or nothing
// has confirmed the backups current for alertOverdue intervals.
func (st Status) Alert(now time.Time) bool {
	if !st.Settings.Auto || st.Stopped {
		return false
	}
	if st.Failures >= alertFailures {
		return true
	}
	since := st.AlertSince()
	return !since.IsZero() && now.Sub(since) > alertOverdue*st.Settings.Every
}

// AlertSince is when the condition behind the alert began: the failure
// streak's first run, else the last confirmation (or, before any, when
// automatic backups were first on). The console keys "Got it" by it, so a new
// streak shows the alert again.
func (st Status) AlertSince() time.Time {
	if st.Failures >= alertFailures {
		return st.FailingSince
	}
	if !st.LastConfirmed.IsZero() {
		return st.LastConfirmed
	}
	return st.Since
}

func (st Status) clone() Status {
	c := st
	if st.Running != nil {
		r := *st.Running
		c.Running = &r
	}
	if st.LastRun != nil {
		r := *st.LastRun
		c.LastRun = &r
	}
	if st.LastBackup != nil {
		b := *st.LastBackup
		c.LastBackup = &b
	}
	c.written = append([]written(nil), st.written...)
	c.Restore = st.Restore.clone()
	return c
}

// Service is the backup scheduler and the commands the console calls. One
// goroutine (the loop) owns the schedule's record and is its only writer;
// a run goes off the loop as a job and reports back to it. Every reader gets
// a copy of the published Status.
type Service struct {
	d       Deps
	log     *slog.Logger
	folders folders
	lister  lister
	export  func(ctx context.Context, opts archive.ExportOptions) (archive.Manifest, error)

	mu     sync.RWMutex
	status Status

	cmds        chan command
	jobDone     chan jobResult
	restoreDone chan restoreJobResult
	done        chan struct{}
	jobs        sync.WaitGroup
	started     atomic.Bool

	idMu sync.Mutex
	id   string

	// Owned by the loop goroutine (and by Start before it launches).
	state       state
	bootAt      time.Time
	running     *Running
	cancelJob   context.CancelFunc
	lastRun     *LastRun
	stateErr    string
	overrideErr string
	pendingSeen pendingMark

	// The restore side, owned by the loop.
	staging      *Staging
	stageStep    atomic.Pointer[string]
	cancelStage  context.CancelFunc
	archiving    bool
	pending      *Pending
	last         *Result
	stageFail    *StageFailure
	restartAsked bool
}

type commandKind int

const (
	cmdRefresh commandKind = iota
	cmdBackUpNow
	cmdRetryNow
	cmdForget
	cmdStageRestore
	cmdCancelRestore
	cmdDismissRestore
)

type command struct {
	kind    commandKind
	entry   Entry // cmdForget, cmdStageRestore
	restore RestoreRequest
	reply   chan result
}

type result struct {
	status Status
	err    error
}

// New builds a Service. Nothing runs until Start.
//
// Its clock is wall time only (d.Now's readings lose their monotonic part):
// the schedule is wall-clock times, and the monotonic clock stops while the
// machine sleeps.
func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	wall := d.Now
	d.Now = func() time.Time { return wall().Round(0) }
	if d.Ticks == nil {
		d.Ticks = func() (<-chan time.Time, func()) {
			t := time.NewTicker(tickInterval)
			return t.C, t.Stop
		}
	}
	if d.Polls == nil {
		d.Polls = func() (<-chan time.Time, func()) {
			t := time.NewTicker(pollInterval)
			return t.C, t.Stop
		}
	}
	if d.WaitBound <= 0 {
		d.WaitBound = waitBound
	}
	if d.GOOS == "" {
		d.GOOS = runtime.GOOS
	}
	if d.Getenv == nil {
		d.Getenv = os.Getenv
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	f := folders{dataDir: d.DataDir, goos: d.GOOS, guard: d.Guard, freeBytes: d.FreeBytes}
	s := &Service{
		d:           d,
		log:         d.Log,
		folders:     f,
		lister:      lister{folders: f, cache: &manifestCache{}},
		export:      d.export,
		cmds:        make(chan command),
		jobDone:     make(chan jobResult, 1),
		restoreDone: make(chan restoreJobResult, 1),
		done:        make(chan struct{}),
	}
	if s.export == nil {
		s.export = archive.Export
	}
	s.status = Status{Settings: Effective(d.Base, config.BackupOverride{}, d.Release, d.DataDir)}
	return s
}

// Start loads the schedule's record, sweeps the staging a previous daemon
// left, and launches the loop, which stops when ctx is cancelled. Start it
// once the daemon has bound its listener. Calling it twice is a no-op.
func (s *Service) Start(ctx context.Context) {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	now := s.d.Now()
	s.bootAt = now
	st, quarantined, err := loadState(s.d.DataDir, now)
	if err != nil {
		s.log.Warn("backup: could not read the schedule's record; starting fresh", "err", err)
	}
	if quarantined != "" {
		s.log.Warn("backup: the schedule's record was corrupt and has been moved aside", "moved_to", quarantined)
	}
	st.clamp(now)
	s.state = st
	sweepStaging(s.d.DataDir)
	s.pendingSeen = s.readPending()
	set := s.settings(ctx)
	if set.Auto && s.state.Since.IsZero() {
		s.state.Since = now
	}
	s.noteFolder(ctx, set)
	s.loadRestore(ctx)
	s.publish(set)
	go s.loop(ctx)
}

// Wait blocks until the loop has exited and any run has ended, or the bound
// has passed (a run stalled on a slow disk is then abandoned). It reports
// whether everything ended. It returns at once when Start was never called.
func (s *Service) Wait() bool {
	if !s.started.Load() {
		return true
	}
	all := make(chan struct{})
	go func() {
		<-s.done
		s.jobs.Wait()
		close(all)
	}()
	t := time.NewTimer(s.d.WaitBound)
	defer t.Stop()
	select {
	case <-all:
		return true
	case <-t.C:
		s.log.Warn("backup: a backup was still writing at shutdown; it is abandoned, and its temp file is removed by the next run",
			"waited", s.d.WaitBound)
		return false
	}
}

// Status returns a copy of the current snapshot.
func (s *Service) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status.clone()
}

// Refresh re-reads the settings and republishes the status: the console calls
// it after saving or resetting its override.
func (s *Service) Refresh(ctx context.Context) (Status, error) {
	return s.send(ctx, cmdRefresh, Entry{})
}

// BackUpNow starts a backup the owner asked for (KindManual): never skipped,
// never pruned. It answers at once with the run under way, or ErrBusy.
func (s *Service) BackUpNow(ctx context.Context) (Status, error) {
	return s.send(ctx, cmdBackUpNow, Entry{})
}

// RetryNow starts the schedule's run now (the failing card's Try again):
// skipped when nothing changed, and pruning after itself, like any scheduled
// run. It answers at once, or ErrBusy.
func (s *Service) RetryNow(ctx context.Context) (Status, error) {
	return s.send(ctx, cmdRetryNow, Entry{})
}

func (s *Service) send(ctx context.Context, kind commandKind, e Entry) (Status, error) {
	if !s.started.Load() {
		return s.Status(), ErrNotRunning
	}
	reply := make(chan result, 1)
	select {
	case s.cmds <- command{kind: kind, entry: e, reply: reply}:
	case <-s.done:
		return s.Status(), ErrNotRunning
	case <-ctx.Done():
		return s.Status(), ctx.Err()
	}
	select {
	case r := <-reply:
		return r.status, r.err
	case <-ctx.Done():
		return s.Status(), ctx.Err()
	}
}

// Settings is the backup behavior in force now.
func (s *Service) Settings(ctx context.Context) Settings {
	set, _ := s.effective(ctx)
	return set
}

// List lists the backups in the chosen folder and in LocalDir, newest first.
func (s *Service) List(ctx context.Context) (Listing, error) {
	set, _ := s.effective(ctx)
	id, err := s.installID(ctx)
	if err != nil {
		return Listing{}, fmt.Errorf("backup.Service.List: %w", err)
	}
	s.mu.RLock()
	wrote := s.wroteSnapshot()
	s.mu.RUnlock()
	return s.lister.list(set.Dir, id, wrote), nil
}

// wroteSnapshot is a copy of the record's trace for a reader off the loop;
// the caller holds s.mu for reading.
func (s *Service) wroteSnapshot() func(dir, name string) bool {
	st := state{Written: append([]written(nil), s.status.written...)}
	return st.wrote
}

// CheckFolder is the look the console takes while the owner types a folder:
// stats only, never a write.
func (s *Service) CheckFolder(dir string) FolderCheck { return s.folders.check(dir) }

// PrepareFolder is what saving a folder does before the override is stored:
// the check, then the folder made (0700) and a file created and removed in
// it.
func (s *Service) PrepareFolder(dir string) FolderCheck { return s.folders.prepare(dir, true) }

// Suggestions lists the places on this machine a backup folder could go.
// Under peer_auth only those the daemon's account could keep private are
// offered.
func (s *Service) Suggestions() []Suggestion {
	e := suggestEnv{goos: s.d.GOOS, home: s.d.Home, user: s.d.User, dataDir: s.d.DataDir, getenv: s.d.Getenv}
	if e.home == "" {
		if home, err := os.UserHomeDir(); err == nil {
			e.home = home // no home: no home-based suggestions
		}
	}
	if e.user == "" {
		e.user = s.d.Getenv("USER")
	}
	all := suggestions(e)
	if s.d.Guard == nil {
		return all
	}
	out := all[:0]
	for _, sg := range all {
		if s.folders.check(sg.Dir).Usable() {
			out = append(out, sg)
		}
	}
	return out
}

// Delete removes the backup ref names in the current listing, and records
// backup.deleted. by names who asked ("owner", or the console's credential
// word).
func (s *Service) Delete(ctx context.Context, ref, by string) error {
	if _, _, err := parseRef(ref); err != nil {
		return err
	}
	l, err := s.List(ctx)
	if err != nil {
		return err
	}
	e, ok := l.Find(ref)
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoSuchBackup, ref)
	}
	if err := os.Remove(e.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("backup.Service.Delete: %w", err)
	}
	s.lister.cache.forget(e.Path)
	s.record(ctx, core.Event{Kind: core.EventBackupDeleted, ItemID: e.Name, Payload: map[string]any{
		"kind": e.Kind, "where": e.Where, "by": by,
	}})
	// The record's trace of it goes too; a stopped loop just keeps it.
	_, _ = s.send(ctx, cmdForget, e) //nolint:errcheck // the file is gone either way; the trace is advisory
	return nil
}

// installID is this install's id, minted on first use and kept.
func (s *Service) installID(ctx context.Context) (string, error) {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	if s.id != "" {
		return s.id, nil
	}
	if s.d.DB == nil {
		return "", errors.New("no database")
	}
	id, err := store.InstallID(ctx, s.d.DB)
	if err != nil {
		return "", err
	}
	s.id = id
	return id, nil
}

// effective reads the override and merges it with the base.
func (s *Service) effective(ctx context.Context) (Settings, string) {
	var o config.BackupOverride
	var errText string
	if s.d.Override != nil {
		got, err := s.d.Override(ctx)
		if err != nil {
			errText = err.Error()
		} else {
			o = got
		}
	}
	return Effective(s.d.Base, o, s.d.Release, s.d.DataDir), errText
}

// settings is effective for the loop, logging a new override error once.
func (s *Service) settings(ctx context.Context) Settings {
	set, errText := s.effective(ctx)
	if errText != "" && errText != s.overrideErr {
		s.log.Warn("backup: the console's backup settings could not be read; using the config file's", "err", errText)
	}
	s.overrideErr = errText
	return set
}

// loop is the service's goroutine. A panic in it is recovered and logged: the
// daemon keeps serving, the status reports the service stopped, and Wait
// returns.
func (s *Service) loop(ctx context.Context) {
	defer close(s.done)
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("backup: the backup service stopped after a panic; the daemon keeps serving",
				"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			if s.cancelJob != nil {
				s.cancelJob()
			}
			if s.cancelStage != nil {
				s.cancelStage()
			}
			s.mu.Lock()
			s.status.Stopped = true
			s.mu.Unlock()
		}
	}()

	ticks, stopTicks := s.d.Ticks()
	defer stopTicks()
	polls, stopPolls := s.d.Polls()
	defer stopPolls()
	for {
		select {
		case <-ctx.Done():
			if s.cancelJob != nil {
				s.cancelJob()
			}
			if s.cancelStage != nil {
				s.cancelStage()
			}
			return
		case <-ticks:
			s.tick(ctx)
		case <-polls:
			s.poll()
		case r := <-s.jobDone:
			s.finish(ctx, r)
		case r := <-s.restoreDone:
			s.finishRestore(ctx, r)
		case cmd := <-s.cmds:
			cmd.reply <- s.handle(ctx, cmd)
		}
	}
}

// tick starts the scheduled run when it is due.
func (s *Service) tick(ctx context.Context) {
	now := s.d.Now()
	set := s.settings(ctx)
	if s.state.clamp(now) {
		s.persist()
	}
	if set.Auto && s.state.Since.IsZero() {
		s.state.Since = now
	}
	s.noteFolder(ctx, set)
	if s.running == nil && !s.restoreBusy() && s.pending == nil && s.due(set, now) {
		s.startJob(ctx, set, now, archive.KindScheduled, true)
	}
	s.publish(set)
}

// due reports whether a scheduled run should start at now.
func (s *Service) due(set Settings, now time.Time) bool {
	if !set.Auto || now.Before(s.bootAt.Add(firstRunDelay)) {
		return false
	}
	return s.state.LastAttempt.IsZero() || !now.Before(s.state.LastAttempt.Add(set.Every))
}

// nextAt is when the next scheduled run is due, zero while off.
func (s *Service) nextAt(set Settings) time.Time {
	if !set.Auto {
		return time.Time{}
	}
	first := s.bootAt.Add(firstRunDelay)
	if s.state.LastAttempt.IsZero() {
		return first
	}
	next := s.state.LastAttempt.Add(set.Every)
	if next.Before(first) {
		return first
	}
	return next
}

func (s *Service) handle(ctx context.Context, cmd command) result {
	set := s.settings(ctx)
	var err error
	switch cmd.kind {
	case cmdBackUpNow, cmdRetryNow:
		if s.running != nil || s.restoreBusy() {
			err = ErrBusy
			break
		}
		if cmd.kind == cmdBackUpNow {
			s.startJob(ctx, set, s.d.Now(), archive.KindManual, false)
		} else {
			s.startJob(ctx, set, s.d.Now(), archive.KindScheduled, true)
		}
	case cmdForget:
		s.state.forgetWritten(filepath.Dir(cmd.entry.Path), cmd.entry.Name)
		s.persist()
	case cmdStageRestore:
		err = s.stageRestore(ctx, cmd.restore, cmd.entry)
	case cmdCancelRestore:
		err = s.cancelRestore()
	case cmdDismissRestore:
		err = s.dismissRestore()
	}
	s.publish(set)
	return result{status: s.Status(), err: err}
}

// startJob launches one run off the loop. A scheduled run's attempt is
// counted from its start, but saved only when it ends, so a daemon stopped
// mid-run runs it again on its next start.
func (s *Service) startJob(ctx context.Context, set Settings, now time.Time, kind string, scheduled bool) {
	id, err := core.NewID()
	if err != nil {
		s.log.Error("backup: could not mint a run id", "err", err)
		return
	}
	jctx, cancel := context.WithCancel(ctx)
	spec := jobSpec{id: id, kind: kind, scheduled: scheduled, settings: set, at: now, state: s.state.clone()}
	s.running = &Running{Kind: kind, Scheduled: scheduled, StartedAt: now}
	s.cancelJob = cancel
	s.jobs.Add(1)
	go func() {
		defer s.jobs.Done()
		defer cancel()
		var res jobResult
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("backup: a backup run panicked", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
					res = failed(jobResult{spec: spec}, failf(ReasonInternal, "the backup stopped on an internal error"), nil)
				}
			}()
			res = s.run(jctx, spec)
		}()
		// Buffered for the one run there can be; a loop that has exited
		// never reads it, and nothing then waits for it.
		s.jobDone <- res
	}()
}

// finish folds a run's result into the record, saves it, and records events.
func (s *Service) finish(ctx context.Context, r jobResult) {
	s.running, s.cancelJob = nil, nil
	now := s.d.Now()
	spec := r.spec
	where := WhereFolder
	run := &LastRun{Kind: spec.kind, Scheduled: spec.scheduled, Outcome: r.outcome, At: now}

	switch r.outcome {
	case OutcomeCancelled:
		s.lastRun = run
		s.publish(s.settings(ctx))
		return
	case OutcomeCreated:
		rec := r.record
		run.Name = rec.Name
		s.state.LastConfirmed, s.state.LastSuccess, s.state.LastBackup = now, now, rec
		s.state.Failures, s.state.FailingSince, s.state.LastReason, s.state.LastError = 0, time.Time{}, "", ""
		s.state.addWritten(written{Name: rec.Name, Dir: rec.Dir, At: rec.At, Fingerprint: rec.Fingerprint})
		for _, p := range r.pruned {
			s.state.forgetWritten(filepath.Dir(p.Path), p.Name)
		}
		s.log.Info("backup: wrote a backup", "name", rec.Name, "kind", rec.Kind, "bytes", rec.Bytes)
		s.record(ctx, createdEvent(rec, where))
		for _, p := range r.pruned {
			s.record(ctx, core.Event{Kind: core.EventBackupDeleted, ItemID: p.Name, Payload: map[string]any{
				"kind": archive.KindScheduled, "where": where, "by": "prune",
			}})
		}
	case OutcomeUnchanged, OutcomeEmpty:
		s.state.LastConfirmed = now
		s.state.Failures, s.state.FailingSince, s.state.LastReason, s.state.LastError = 0, time.Time{}, "", ""
	case OutcomeFailed:
		f := r.failure
		run.Reason, run.Error = f.Reason, f.Message
		if spec.scheduled {
			if s.state.Failures == 0 {
				s.state.FailingSince = spec.at
			}
			s.state.Failures++
			s.state.LastReason, s.state.LastError = f.Reason, f.Message
		}
		s.log.Warn("backup: a backup failed", "kind", spec.kind, "reason", f.Reason, "err", f.Message, "cause", r.err)
		payload := map[string]any{"kind": spec.kind, "where": where, "reason": f.Reason}
		if spec.scheduled {
			payload["failures"] = s.state.Failures
		}
		s.record(ctx, core.Event{Kind: core.EventBackupFailed, Payload: payload})
	}
	if spec.scheduled {
		s.state.LastAttempt = spec.at
		s.state.LastOutcome = r.outcome
	}
	s.lastRun = run
	s.persist()
	s.publish(s.settings(ctx))
}

// persist saves the record, keeping the error for the status.
func (s *Service) persist() {
	if err := s.state.save(s.d.DataDir); err != nil {
		if s.stateErr != err.Error() {
			s.log.Warn("backup: could not save the schedule's record", "err", err)
		}
		s.stateErr = err.Error()
		return
	}
	s.stateErr = ""
}

// publish rebuilds the status from the loop's state.
func (s *Service) publish(set Settings) {
	st := Status{
		Settings: set, SettingsError: s.overrideErr,
		LastAttempt: s.state.LastAttempt, LastConfirmed: s.state.LastConfirmed, LastSuccess: s.state.LastSuccess,
		UpToDate: s.state.LastOutcome == OutcomeUnchanged,
		NextAt:   s.nextAt(set),
		Failures: s.state.Failures, FailingSince: s.state.FailingSince,
		LastReason: s.state.LastReason, LastError: s.state.LastError,
		Since: s.state.Since, StateError: s.stateErr,
		written: append([]written(nil), s.state.Written...),
	}
	if s.state.LastBackup != nil {
		b := *s.state.LastBackup
		st.LastBackup = &b
	}
	if s.running != nil {
		r := *s.running
		st.Running = &r
	}
	if s.lastRun != nil {
		r := *s.lastRun
		st.LastRun = &r
	}
	st.Restore = s.restoreStatus()
	s.mu.Lock()
	st.Stopped = s.status.Stopped
	s.status = st
	s.mu.Unlock()
}

func (s *Service) record(ctx context.Context, e core.Event) {
	if s.d.Events == nil {
		return
	}
	if _, err := s.d.Events.Record(context.WithoutCancel(ctx), e); err != nil {
		s.log.Warn("backup: could not record an event", "kind", e.Kind, "err", err)
	}
}

// pendingMark identifies one restore/pending.json the loop has seen.
type pendingMark struct {
	exists bool
	size   int64
	mod    time.Time
}

// readPending stats restore/pending.json.
func (s *Service) readPending() pendingMark {
	fi, err := os.Stat(PendingPath(s.d.DataDir))
	if err != nil {
		return pendingMark{}
	}
	return pendingMark{exists: true, size: fi.Size(), mod: fi.ModTime()}
}

// poll notices a restore staged while the daemon runs (the CLI's, which has
// no other way to reach it) and asks for the restart that applies it. A file
// already there at start was the startup step's to apply; only one that
// appears or changes afterwards, and reads as staged, asks. One the CLI is
// still writing is looked at again at the next poll.
func (s *Service) poll() {
	m := s.readPending()
	if m == s.pendingSeen {
		return
	}
	if !m.exists {
		s.pendingSeen = m
		s.pending = nil
		s.publish(s.settings(context.Background()))
		return
	}
	stage, ok := pendingStage(PendingPath(s.d.DataDir))
	if !ok {
		return // not complete yet
	}
	s.pendingSeen = m
	if p, err := ReadPending(s.d.DataDir); err == nil && p != nil {
		s.pending = p
		s.publish(s.settings(context.Background()))
	}
	if stage != StagePending {
		return
	}
	s.askRestart("backup: a restore was staged; restarting to apply it")
}

// askRestart asks the daemon, once, for the restart that applies a staged
// restore; without a restart (Windows) it waits for the next start.
func (s *Service) askRestart(msg string) {
	if s.d.RequestRestart == nil {
		s.log.Info("backup: a restore is staged; it applies when Seamless next starts")
		return
	}
	if s.restartAsked {
		return
	}
	s.restartAsked = true
	s.log.Info(msg)
	s.d.RequestRestart()
}

// maxPendingBytes caps how much of pending.json the poll reads.
const maxPendingBytes = 1 << 20

// pendingStage reads pending.json's stage, ok false while it does not parse.
func pendingStage(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxPendingBytes))
	if err != nil {
		return "", false
	}
	var p struct {
		Stage string `json:"stage"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return "", false
	}
	return p.Stage, true
}
