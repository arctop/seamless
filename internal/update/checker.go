package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
)

// Scheduling constants.
const (
	// firstCheckDelay + rand(firstCheckJitter) is when a daemon with no sane
	// saved schedule first checks: soon after it starts, never in its first
	// moments, and spread so a fleet restarted together does not ask at once.
	firstCheckDelay  = 2 * time.Minute
	firstCheckJitter = 8 * time.Minute
	firstCheckSpread = firstCheckDelay + firstCheckJitter

	// errorBackoffBase is the wait after the first failed check; each further
	// failure doubles it, up to the check interval.
	errorBackoffBase = 15 * time.Minute

	// checkNowThrottle is how soon after any check "Check now" may ask again.
	checkNowThrottle = time.Minute

	// tickInterval is how often the loop compares the clock with its saved
	// deadlines. Deadlines are wall-clock times in the state, and the tick is
	// short, so a laptop that slept through a check catches up within a
	// minute of waking -- a long monotonic timer would stall instead.
	tickInterval = time.Minute

	// pollInterval is how often the loop also re-reads the record of an
	// update under way, between ticks, so the status stops saying "updating"
	// within seconds of the updater finishing rather than at the next tick.
	pollInterval = 5 * time.Second
)

// Errors CheckNow returns.
var (
	// ErrChecksOff: update checks are off, so nothing may be asked.
	ErrChecksOff = errors.New("update: update checks are off")
	// ErrTooSoon: a check ran within the last minute.
	ErrTooSoon = errors.New("update: checked less than a minute ago")
	// ErrNotRunning: the checker is not started, or stopped after a fault.
	ErrNotRunning = errors.New("update: the update checker is not running")
)

// ReleaseFetcher reads the release list (*Fetcher in production).
type ReleaseFetcher interface {
	Fetch(ctx context.Context, etag string) (Page, error)
}

// Recorder is the slice of the event log the checker writes to
// (*events.Recorder in production).
type Recorder interface {
	Record(ctx context.Context, e core.Event) (string, error)
	RecordOnce(ctx context.Context, e core.Event) (string, bool, error)
}

// Deps is everything the checker reads or writes, injected so a test drives
// the clock, the ticks, the network and the event log.
type Deps struct {
	DataDir      string // state lives in <DataDir>/update
	Version      string // the running version, as built
	Distribution string // the build stamp (DistributionRelease or not)
	GOOS         string // which installer's assets a release must carry
	Install      Install
	Instance     string // this process's id, also served by /healthz
	PID          int
	LocalHost    string // the daemon's own host name, for the briefing notice

	// Base is the file/env config. Override reads the console's stored
	// override on every use; an error falls back to Base alone.
	Base     config.Update
	Override func(ctx context.Context) (config.UpdateOverride, error)

	Fetcher ReleaseFetcher
	Events  Recorder // nil: no events

	// Automatic updates (plan 2.4). Each is optional. Without a Spawner the
	// daemon installs nothing and never reports ModeAuto, whatever
	// Settings.Auto says; the rest only tune when it applies.

	// Spawner starts the updater for one attempt (cmd/seamlessd wires one
	// per OS). nil: never apply.
	Spawner Spawner
	// LiveSessions counts the agent sessions live at cutoff: active and
	// heartbeating on or after it (store.LiveSessionCount). The checker
	// passes now minus SessionIdle. nil, or an error, is never idle.
	LiveSessions func(ctx context.Context, cutoff time.Time) (int, error)
	// SessionIdle is gardener.session_idle_minutes, the age past which a
	// session no longer counts as live; zero falls back to config's default.
	SessionIdle time.Duration
	// Activity is the request tracker's snapshot: how many counted requests
	// are in flight, and when the last one finished (the tracker's start
	// before any has). nil: activity unknown, so only the overdue path
	// (twice max_defer) applies.
	Activity func() (inFlight int64, last time.Time)
	// UpdaterRunning probes LockPath: true when an updater holds it, which
	// a heartbeat alone cannot always tell (a stale heartbeat from a live
	// updater, one that has not written yet). An implementation that takes
	// the lock to probe releases it at once. nil, or an error: the
	// heartbeat alone decides.
	UpdaterRunning func() (bool, error)
	// CodexHooksHash fingerprints Codex's hooks.json: a hash of its bytes,
	// or a fixed token when the file does not exist; an error when it
	// cannot tell. It is read at start and before each spawn, and a
	// fingerprint that changed across a version change raises the Codex
	// banner (Updated.CodexHooks). nil: no banner.
	CodexHooksHash func() (string, error)

	Now   func() time.Time                  // nil: time.Now
	Ticks func() (<-chan time.Time, func()) // nil: a tickInterval ticker
	// Polls is the faster ticker the loop runs only while an update is under
	// way, started when one begins and stopped when it settles. nil: a
	// pollInterval ticker.
	Polls func() (<-chan time.Time, func())
	Rand  func(n int64) int64 // uniform in [0, n); nil: math/rand/v2
	Log   *slog.Logger        // nil: slog.Default()
}

// Checker is the background update check. One goroutine (the loop) owns the
// state and is its only writer: CheckNow and Refresh hand it a command and
// wait. Every reader gets a copy of the published Status.
type Checker struct {
	d       Deps
	release bool
	log     *slog.Logger

	mu     sync.RWMutex
	status Status

	cmds    chan command
	done    chan struct{}
	started atomic.Bool

	// Owned by the loop goroutine (and by Start before it launches).
	state       State
	lastFetch   time.Time // the last request, failed or not (CheckNow's throttle)
	lastGood    time.Time // the last successful request (recheckFresh)
	persistErr  string
	overrideErr string

	// Automatic updates, owned by the loop goroutine too.
	applying   *Applying // the update under way, as the last fold found it
	view       autoView  // what the last evaluation found, for snapshot
	attemptErr string    // the last attempt.json read error, logged once
	codexErr   string    // the last Codex fingerprint error, logged once
}

// commandKind is what a command asks the loop to do.
type commandKind int

const (
	cmdRefresh  commandKind = iota // re-read the settings and republish
	cmdCheck                       // Check now
	cmdApplyNow                    // Update now
	cmdResume                      // Resume automatic updates
)

type command struct {
	kind  commandKind
	reply chan result
}

type result struct {
	status Status
	err    error
}

// New builds a Checker. Nothing runs until Start.
//
// The clock it keeps is wall time only: d.Now's readings lose their monotonic
// part. Every deadline here is a wall-clock time (the state file holds no
// other kind), and the monotonic clock stops while the machine sleeps
// (mach_absolute_time on macOS, CLOCK_MONOTONIC on Linux), so a deadline set
// in memory before a sleep would otherwise come due as late as the sleep was
// long -- the next check, a backoff, an attempt's start window -- and a
// release list fetched just before the lid closed would still count as fresh
// on waking. A clock that goes back instead is clampDeadlines' job.
func New(d Deps) *Checker {
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
	if d.Rand == nil {
		d.Rand = rand.Int64N
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	c := &Checker{
		d:       d,
		release: IsReleaseBuild(d.Distribution, d.Version),
		log:     d.Log,
		cmds:    make(chan command),
		done:    make(chan struct{}),
	}
	c.status = c.snapshot(Effective(d.Base, config.UpdateOverride{}, c.release))
	return c
}

// Start runs the startup step -- load and clean the saved state, notice a
// version change, schedule the first check -- and then launches the loop,
// which stops when ctx is cancelled. Start it only once the daemon has bound
// its listener: a daemon that fails to bind must not record itself as
// running. Calling Start twice is a no-op.
func (c *Checker) Start(ctx context.Context) {
	if !c.started.CompareAndSwap(false, true) {
		return
	}
	c.boot(ctx)
	go c.loop(ctx)
}

// Wait blocks until the loop has exited. It returns at once when Start was
// never called.
func (c *Checker) Wait() {
	if !c.started.Load() {
		return
	}
	<-c.done
}

// Status returns a copy of the current snapshot.
func (c *Checker) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status.clone()
}

// Notice is the briefing line for a session on host, in the shape the
// retrieve service's update-notice hook takes.
func (c *Checker) Notice(_ context.Context, host string) string {
	return c.Status().Notice(host, c.d.LocalHost, c.d.Now())
}

// CheckNow asks GitHub right away -- the console's "Check now". It refuses
// with ErrChecksOff when checks are off and ErrTooSoon within a minute of the
// previous check. A failed request comes back as its error alongside the
// updated status (which records it).
func (c *Checker) CheckNow(ctx context.Context) (Status, error) {
	return c.send(ctx, cmdCheck)
}

// Refresh re-reads the settings and republishes the status: the console calls
// it after saving or resetting its override, so the page it redirects to
// already shows the new state.
func (c *Checker) Refresh(ctx context.Context) (Status, error) {
	return c.send(ctx, cmdRefresh)
}

func (c *Checker) send(ctx context.Context, kind commandKind) (Status, error) {
	if !c.started.Load() {
		return c.Status(), ErrNotRunning
	}
	reply := make(chan result, 1)
	select {
	case c.cmds <- command{kind: kind, reply: reply}:
	case <-c.done:
		return c.Status(), ErrNotRunning
	case <-ctx.Done():
		return c.Status(), ctx.Err()
	}
	select {
	case r := <-reply:
		return r.status, r.err
	case <-ctx.Done():
		return c.Status(), ctx.Err()
	}
}

// boot is the startup step. For a release build it loads the saved state
// (quarantining a corrupt file), clamps future timestamps, records the version
// change since the previous daemon -- with what it means for automatic
// updates: a hold after a deliberate downgrade, the Codex banner, a later
// first check -- notes this process as the running one and saves. A source
// build keeps everything in memory and never touches the data dir
// (constraint dev-and-fixture-daemons-never-self-update).
func (c *Checker) boot(ctx context.Context) {
	now := c.d.Now()
	s := c.settings(ctx)
	if !c.release {
		c.scheduleFirst(now)
		c.publish(s)
		return
	}

	st, quarantined, err := LoadState(c.d.DataDir, now)
	if err != nil {
		c.log.Warn("update: could not read the update state; starting fresh", "err", err)
		st = State{}
	}
	if quarantined != "" {
		c.log.Warn("update: the update state file was corrupt and has been moved aside", "moved_to", quarantined)
	}
	st.Clamp(now, s.CheckInterval)
	if st.ReleasesRev != filterRevision && (len(st.Releases) > 0 || st.ETag != "") {
		// The cached list came from another release's Filter (an older one
		// never recorded ChecksumsBundle): read it whole at the next check
		// rather than let a 304 keep it.
		st.ETag = ""
	}

	cur, _ := Parse(c.d.Version) // a release build always parses
	c.state = st
	codex := c.codexFingerprint()
	var changed *Updated
	if last := st.LastRunVersion; last != nil && *last != cur {
		dir := DirectionUpgrade
		if cur.Compare(*last) < 0 {
			dir = DirectionDowngrade
		}
		changed = &Updated{From: *last, To: cur, At: now, Direction: dir}
		c.state.Updated = changed
		c.versionChanged(now, cur, changed, codex)
	}
	// An unreadable hooks.json keeps the fingerprint the last daemon saw.
	if codex != "" || c.d.CodexHooksHash == nil {
		c.state.CodexHooks = codex
	}
	c.state.LastRunVersion = &cur
	c.state.Running = Running{
		Version: cur, Distribution: c.d.Distribution, Kind: c.d.Install.Kind, Reason: c.d.Install.Reason,
		Instance: c.d.Instance, PID: c.d.PID, StartedAt: now, CanApply: c.d.Spawner != nil,
	}
	c.state.tidy(cur)
	c.scheduleFirst(now)

	// The event follows the write: if the write fails, the next start sees
	// the same change again, so the event is better lost than doubled.
	if c.persist() && changed != nil {
		c.log.Info("update: running a different version than last time",
			"from", changed.From.String(), "to", changed.To.String(), "direction", changed.Direction,
			"codex_hooks_changed", changed.CodexHooks)
		payload := map[string]any{
			"from": changed.From.String(), "to": changed.To.String(), "direction": changed.Direction,
		}
		if changed.CodexHooks {
			payload["codex_hooks_changed"] = true
		}
		c.record(ctx, core.Event{Kind: core.EventUpdateApplied, ItemID: changed.To.String(), Payload: payload})
	}
	// Fold what an updater left behind (the attempt that brought this
	// version, or one a predecessor started), so the first status says so.
	c.step(ctx, s, false)
	c.publish(s)
}

// versionChanged is what a version change at startup means for automatic
// updates:
//
//   - a downgrade that no update attempt explains is the owner's own pin
//     (SEAMLESS_VERSION, a manual `seamlessd update`), and sets a hold so an
//     automatic update does not undo it; one that an attempt from cur to the
//     version left explains is a rollback, which the fold settles instead;
//   - codex, the fingerprint of Codex's hooks.json now, differs from the one
//     taken before the update (at the spawn, or at the previous daemon's
//     start): the Codex banner;
//   - the first check, and any pre-update re-check, wait
//     postUpdateCheckDelay (plus jitter) from now.
func (c *Checker) versionChanged(now time.Time, cur Version, u *Updated, codex string) {
	if u.Direction == DirectionDowngrade && !c.explainsDowngrade(cur, u.From) {
		through := u.From
		if newest, ok := Newest(c.state.Releases); ok && newest.Version.Compare(through) > 0 {
			through = newest.Version
		}
		c.state.Hold = &Hold{Through: through, From: u.From, At: now}
		c.log.Info("update: running an older version than last time; automatic updates skip releases up to it until resumed",
			"from", u.From.String(), "to", cur.String(), "skip_through", through.String())
	}

	before := c.state.CodexHooks
	if sp := c.state.Spawn; sp != nil && sp.To == cur && sp.CodexHooks != "" {
		before = sp.CodexHooks
	}
	if before != "" && codex != "" && codex != before {
		u.CodexHooks = true
	}

	first := now.Add(postUpdateCheckDelay + time.Duration(c.d.Rand(int64(firstCheckJitter))))
	if first.After(c.state.NextCheckAt) {
		c.state.NextCheckAt = first
	}
	if c.state.NextCheckAt.After(c.state.RecheckAt) {
		c.state.RecheckAt = c.state.NextCheckAt
	}
}

// explainsDowngrade reports whether an update attempt from cur to from -- the
// spawn this daemon's predecessor saved, or the newest record -- accounts for
// running cur after from: a rollback, not the owner's choice.
func (c *Checker) explainsDowngrade(cur, from Version) bool {
	if sp := c.state.Spawn; sp != nil && sp.From == cur && sp.To == from {
		return true
	}
	a, ok := c.readAttempt()
	return ok && a.From == cur && a.To == from
}

// scheduleFirst keeps a saved next-check time that is still ahead (a daemon
// that restarts mid-backoff keeps waiting), and otherwise schedules the first
// check firstCheckDelay plus jitter from now.
func (c *Checker) scheduleFirst(now time.Time) {
	if c.state.NextCheckAt.After(now) {
		return
	}
	c.state.NextCheckAt = now.Add(firstCheckDelay + time.Duration(c.d.Rand(int64(firstCheckJitter))))
}

// loop is the checker's goroutine: it takes the ticks, the commands and,
// while an update is under way, the polls. A panic in it is recovered and
// logged: the daemon keeps serving, the status reports the checker stopped,
// and Wait returns.
func (c *Checker) loop(ctx context.Context) {
	defer close(c.done)
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("update: the update checker stopped after a panic; the daemon keeps serving",
				"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			c.mu.Lock()
			c.status.Stopped = true
			c.mu.Unlock()
		}
	}()

	ticks, stop := c.d.Ticks()
	defer stop()
	var polls <-chan time.Time // nil, so never ready, while no update is under way
	stopPolls := func() {}
	defer func() { stopPolls() }()
	for {
		switch under := c.applying != nil; {
		case under && polls == nil:
			polls, stopPolls = c.d.Polls()
		case !under && polls != nil:
			stopPolls()
			polls, stopPolls = nil, func() {}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			c.tick(ctx)
		case <-polls:
			c.poll(ctx)
		case cmd := <-c.cmds:
			cmd.reply <- c.handle(ctx, cmd)
		}
	}
}

// poll re-reads the record of the update under way between ticks: the
// automatic-update step with neither a check nor a spawn, so a finished
// attempt is folded within pollInterval of the updater's last write.
func (c *Checker) poll(ctx context.Context) {
	s := c.settings(ctx)
	c.step(ctx, s, false)
	c.publish(s)
}

// tick checks when the saved deadline has passed and checks are on, then
// takes the automatic-update step: fold the attempt record, decide, and
// maybe start an update.
func (c *Checker) tick(ctx context.Context) {
	s := c.settings(ctx)
	if c.state.clampDeadlines(c.d.Now(), s.CheckInterval) {
		c.persist()
	}
	if now := c.d.Now(); s.Check && !now.Before(c.state.NextCheckAt) {
		// A scheduled check has no caller to tell: fail() logged the error and
		// the status carries it as CheckError.
		_ = c.check(ctx, now, s) //nolint:errcheck // reported through the status, see above
	}
	c.step(ctx, s, true)
	c.publish(s)
}

// handle serves a command.
func (c *Checker) handle(ctx context.Context, cmd command) result {
	s := c.settings(ctx)
	switch cmd.kind {
	case cmdCheck:
		return c.handleCheck(ctx, s)
	case cmdApplyNow:
		err := c.applyNow(ctx, s)
		c.publish(s)
		return result{status: c.Status(), err: err}
	case cmdResume:
		c.resume(ctx, s)
		c.publish(s)
		return result{status: c.Status()}
	default:
		c.step(ctx, s, false)
		c.publish(s)
		return result{status: c.Status()}
	}
}

// handleCheck serves CheckNow.
func (c *Checker) handleCheck(ctx context.Context, s Settings) result {
	now := c.d.Now()
	switch {
	case !s.Check:
		c.publish(s)
		return result{status: c.snapshot(s), err: ErrChecksOff}
	case !c.lastFetch.IsZero() && now.Sub(c.lastFetch) < checkNowThrottle:
		c.publish(s)
		return result{status: c.snapshot(s), err: ErrTooSoon}
	}
	err := c.check(ctx, now, s)
	c.step(ctx, s, false)
	c.publish(s)
	return result{status: c.Status(), err: err}
}

// check asks GitHub once and folds the answer into the state.
func (c *Checker) check(ctx context.Context, now time.Time, s Settings) error {
	// Save a pessimistic next time BEFORE asking: if this process dies
	// mid-request (or crash-loops), the next start reads it and waits
	// instead of asking again at once.
	c.state.NextCheckAt = now.Add(c.errorBackoff(s.CheckInterval))
	c.persist()

	page, err := c.d.Fetcher.Fetch(ctx, c.state.ETag)
	now = c.d.Now()
	if err != nil {
		if ctx.Err() != nil {
			return err // shutting down: not the network's fault, not a failure
		}
		c.lastFetch = now
		c.fail(now, s, err)
		c.persist()
		return err
	}
	c.lastFetch, c.lastGood = now, now

	if c.state.CheckError != nil {
		c.log.Info("update: the release check works again", "after_failures", c.state.CheckError.Count)
	}
	c.state.CheckError = nil
	c.state.CheckedAt = now
	c.state.ServerDate = page.ServerDate
	if page.ETag != "" {
		c.state.ETag = page.ETag
	}
	if !page.NotModified {
		c.state.Releases = Filter(page.Releases, c.d.GOOS)
		c.state.ReleasesRev = filterRevision
	}
	c.state.NextCheckAt = now.Add(c.jitter(s.CheckInterval))

	newest, ok := Newest(c.state.Releases)
	if ok && (c.state.Seen == nil || c.state.Seen.Version != newest.Version) {
		c.state.Seen = &Seen{Version: newest.Version, At: now}
	}
	c.persist()

	if cur, curOK := Parse(c.d.Version); ok && curOK && newest.Version.Compare(cur) > 0 {
		c.record(ctx, core.Event{
			Kind:   core.EventUpdateAvailable,
			ItemID: newest.Version.String(),
			Payload: map[string]any{
				"version": newest.Version.String(), "current": cur.String(), "kind": string(c.d.Install.Kind),
			},
		})
	}
	return nil
}

// fail records a failed check and schedules the retry: the error backoff
// (15m doubling per consecutive failure, capped at the interval), or the
// rate limit's own retry time when that is later.
func (c *Checker) fail(now time.Time, s Settings, err error) {
	kind := CheckErrorUnavailable
	var rl *RateLimitError
	if errors.As(err, &rl) {
		kind = CheckErrorRateLimited
	}
	ce := c.state.CheckError
	if ce == nil {
		ce = &CheckError{Since: now}
		c.log.Warn("update: the release check failed; retrying later", "err", err)
	} else {
		c.log.Debug("update: the release check failed again", "err", err, "failures", ce.Count+1)
	}
	ce.Kind, ce.Message, ce.Count = kind, err.Error(), ce.Count+1
	c.state.CheckError = ce

	next := now.Add(c.errorBackoff(s.CheckInterval))
	if rl != nil && rl.RetryAt.After(next) {
		next = rl.RetryAt
	}
	c.state.NextCheckAt = next
}

// errorBackoff is the wait after the current streak of failures plus one:
// errorBackoffBase doubled per failure already counted, capped at interval.
func (c *Checker) errorBackoff(interval time.Duration) time.Duration {
	n := 0
	if c.state.CheckError != nil {
		n = c.state.CheckError.Count
	}
	d := errorBackoffBase
	for i := 1; i < n && d < interval; i++ {
		d *= 2
	}
	return min(d, max(interval, errorBackoffBase))
}

// jitter spreads an interval by +-10%, so installs that started together do
// not stay in step.
func (c *Checker) jitter(d time.Duration) time.Duration {
	return d - d/10 + time.Duration(c.d.Rand(int64(d/5)+1))
}

// settings resolves the effective settings, reading the console override.
// An unreadable override falls back to the file/env values, logged once per
// distinct error.
func (c *Checker) settings(ctx context.Context) Settings {
	var o config.UpdateOverride
	if c.d.Override != nil {
		got, err := c.d.Override(ctx)
		switch {
		case err != nil:
			if msg := err.Error(); msg != c.overrideErr {
				c.log.Warn("update: the console's update settings are unreadable; using the config file's", "err", err)
				c.overrideErr = msg
			}
		default:
			c.overrideErr = ""
			o = got
		}
	}
	return Effective(c.d.Base, o, c.release)
}

// persist saves the state for a release build and reports whether it did; a
// source build never writes. A failure is logged once per distinct error:
// memory stays the source of truth and the next save tries again.
func (c *Checker) persist() bool {
	if !c.release {
		return false
	}
	if err := SaveState(c.d.DataDir, c.state); err != nil {
		if msg := err.Error(); msg != c.persistErr {
			c.log.Warn("update: could not save the update state; keeping it in memory", "err", err)
			c.persistErr = msg
		}
		return false
	}
	c.persistErr = ""
	return true
}

// record writes one event, best-effort: the event log is telemetry, and the
// check must not fail over it.
func (c *Checker) record(ctx context.Context, e core.Event) {
	if c.d.Events == nil {
		return
	}
	var err error
	if e.Kind == core.EventUpdateAvailable {
		_, _, err = c.d.Events.RecordOnce(ctx, e)
	} else {
		_, err = c.d.Events.Record(ctx, e)
	}
	if err != nil {
		c.log.Warn("update: could not record an update event", "kind", string(e.Kind), "err", err)
	}
}

// publish replaces the shared snapshot.
func (c *Checker) publish(s Settings) {
	snap := c.snapshot(s)
	c.mu.Lock()
	snap.Stopped = c.status.Stopped
	c.status = snap
	c.mu.Unlock()
}

// snapshot builds a Status from the loop's state under settings s.
func (c *Checker) snapshot(s Settings) Status {
	st := Status{
		Version:      c.d.Version,
		Distribution: c.d.Distribution,
		Release:      c.release,
		Instance:     c.d.Instance,
		StartedAt:    c.state.Running.StartedAt,
		Install:      c.d.Install,
		Settings:     s,
		CheckedAt:    c.state.CheckedAt,
		NextCheckAt:  c.state.NextCheckAt,
	}
	if ce := c.state.CheckError; ce != nil {
		cp := *ce
		st.CheckError = &cp
	}
	if u := c.state.Updated; u != nil {
		cp := *u
		st.Updated = &cp
	}
	if newest, ok := Newest(c.state.Releases); ok {
		st.Newest = &newest
		if cur, curOK := Parse(c.d.Version); curOK && newest.Version.Compare(cur) > 0 {
			st.Available = true
		}
		if c.state.Seen != nil && c.state.Seen.Version == newest.Version {
			st.SeenAt = c.state.Seen.At
		}
	}

	// Automatic updates. The state's own records come through as they are;
	// what the last evaluation found (target, wait reason, the update under
	// way) comes from the view.
	st.CanApply = c.d.Spawner != nil
	st.LastAttempt = c.state.LastAttempt
	st.Hold = c.state.Hold
	st.Paused = c.state.Paused
	if cur, ok := Parse(c.d.Version); ok {
		for _, b := range c.state.Blocks {
			if b.Version.Compare(cur) > 0 {
				st.Blocked = append(st.Blocked, b)
			}
		}
	}
	if b := c.state.Backoff; b != nil && c.d.Now().Before(b.Until) {
		st.Backoff = b
	}
	if p := c.state.Pending; p != nil {
		st.PendingSince = p.Since
	}
	st.Target = c.view.target
	st.Applying = c.view.applying
	st.Waiting, st.WaitCode = c.view.decision.Wait, c.view.decision.Code
	// The clone is what keeps a reader from aliasing the loop's state.
	return st.clone()
}

// clone deep-copies the pointer and slice fields, so a reader can never
// alias the loop's state.
func (s Status) clone() Status {
	out := s
	out.Newest = clonePtr(s.Newest)
	out.CheckError = clonePtr(s.CheckError)
	out.Updated = clonePtr(s.Updated)
	out.Target = clonePtr(s.Target)
	out.Applying = clonePtr(s.Applying)
	out.LastAttempt = clonePtr(s.LastAttempt)
	out.Hold = clonePtr(s.Hold)
	out.Backoff = clonePtr(s.Backoff)
	out.Blocked = slices.Clone(s.Blocked)
	if s.Paused != nil {
		p := *s.Paused
		p.Versions = slices.Clone(p.Versions)
		out.Paused = &p
	}
	return out
}

// clonePtr copies the value p points to, or returns nil.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
