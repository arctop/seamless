package update

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
)

// The checker's automatic-update step (plan 2.4): fold the updater's record,
// pick the target, decide, and hand an attempt to the Spawner. Everything
// here runs on the loop goroutine, the state's only writer; ApplyNow and
// Resume reach it as commands, like CheckNow.

// Errors ApplyNow returns (besides ErrChecksOff, ErrNotRunning and a failed
// check's own error).
var (
	// ErrCannotApply: this daemon does not install updates by itself -- the
	// installer did not make this install (Install.NotifyOnly), or no
	// updater is wired. The wrapped text says which.
	ErrCannotApply = errors.New("update: this daemon does not install updates by itself")
	// ErrInProgress: an update is already under way.
	ErrInProgress = errors.New("update: an update is already in progress")
	// ErrUpToDate: no release newer than the running one is known.
	ErrUpToDate = errors.New("update: no newer release to install")
	// ErrUnsigned: the newest release carries no checksums bundle, which the
	// unattended updater verifies (Release.ChecksumsBundle).
	ErrUnsigned = errors.New("update: the newest release carries no signed checksums bundle")
	// errStateNotSaved: the state file could not be written, so no spawn may
	// start (its record would not survive a crash).
	errStateNotSaved = errors.New("update: the update state could not be saved")
	// errNoSessionCounter: Deps.LiveSessions is nil, so nothing is ever idle.
	errNoSessionCounter = errors.New("update: no live-session counter is wired")
)

// autoView is what the last automatic-update evaluation found, for snapshot.
type autoView struct {
	target   *Release
	decision Decision
	applying *Applying
}

// ApplyNow installs the newest release now: the console's "Update now". It
// skips the soak, the idle wait, blocks and the backoff, and lifts a hold. It
// still refuses when checks are off (ErrChecksOff: update.check: false means
// no update traffic, and `seamlessd update` is the owner's way past that), on
// an install that does not update itself or with no updater wired
// (ErrCannotApply), while an update is under way (ErrInProgress), when nothing
// newer is known (ErrUpToDate), and when the newest release carries no
// checksums bundle (ErrUnsigned). It re-checks the release list first unless
// one was fetched within the last minute; a failed re-check comes back as its
// error, recorded in the status, and a request that failed under a minute ago
// is ErrTooSoon, as for CheckNow. It spawns with WhyNow and returns once the
// updater is handed to the OS, not when it is done; the status then shows it
// as Applying.
func (c *Checker) ApplyNow(ctx context.Context) (Status, error) {
	return c.send(ctx, cmdApplyNow)
}

// Resume lifts what holds automatic updates back by the daemon's own choice:
// the hold a deliberate downgrade set, and the pause after rollbacks (the
// rollback count starts over). It changes no setting -- an owner's
// update.auto: false stays false -- and leaves blocks and a running backoff
// alone. With nothing to lift it changes nothing.
func (c *Checker) Resume(ctx context.Context) (Status, error) {
	return c.send(ctx, cmdResume)
}

// autoOn reports whether this daemon installs updates by itself right now:
// Status.Mode's ModeAuto conditions, from the loop's side.
func (c *Checker) autoOn(s Settings) bool {
	return c.release && s.Check && s.Auto && !c.d.Install.NotifyOnly() && c.d.Spawner != nil && c.state.Paused == nil
}

// step is one automatic-update step: fold the attempt record, drop what the
// running version made moot, evaluate, and -- when spawnOK and the decision
// says so -- start an update. It saves the state when anything changed. A
// source build has no automatic updates and never reads the data dir.
func (c *Checker) step(ctx context.Context, s Settings, spawnOK bool) {
	if !c.release {
		return
	}
	cur, ok := Parse(c.d.Version)
	if !ok {
		return
	}
	now := c.d.Now()
	dirty := c.fold(ctx, now, cur)
	dirty = c.state.tidy(cur) || dirty
	dirty = c.evaluate(ctx, now, s, cur) || dirty
	if dirty {
		c.persist()
	}
	if spawnOK && c.view.decision.Apply {
		c.applyAuto(ctx, s, cur)
	}
}

// evaluate fills c.view: the target, the pending deadline (started or
// cleared), and the decision. It reports whether the state changed.
func (c *Checker) evaluate(ctx context.Context, now time.Time, s Settings, cur Version) bool {
	c.view = autoView{applying: c.applying}
	if !c.autoOn(s) {
		if c.state.Pending != nil {
			c.state.Pending = nil
			return true
		}
		return false
	}
	clock, clockOK := serverClock(c.state.ServerDate, c.state.CheckedAt, now, scheduleSlack(s.CheckInterval))
	target, ok := Target(c.state.Releases, cur, clock, s.MinAge, c.state.Blocks, c.state.Hold)
	if !ok {
		changed := c.state.Pending != nil
		c.state.Pending = nil
		switch newest, nok := Newest(c.state.Releases); {
		case c.applying != nil:
			c.view.decision = waitFor(WaitInProgress, applyingWords(c.applying))
		case nok && newest.Version.Compare(cur) > 0:
			code, words := newestWait(newest, clock, s.MinAge, c.state.Blocks, c.state.Hold)
			c.view.decision = waitFor(code, words)
		}
		return changed
	}
	c.view.target = &target

	// The deadline runs on GitHub's clock when there is one, like the soak.
	pclock := now
	if clockOK {
		pclock = clock
	}
	changed := false
	if p := c.state.Pending; p == nil || p.Running != cur {
		c.state.Pending = &Pending{Running: cur, Since: pclock}
		changed = true
	}
	in := DecideInput{
		Now:        now,
		Pending:    max(pclock.Sub(c.state.Pending.Since), 0),
		MaxDefer:   s.MaxDefer,
		InProgress: c.applying != nil,
	}
	if b := c.state.Backoff; b != nil {
		in.BackoffUntil = b.Until
	}
	in.Sessions, in.SessionsErr = c.liveSessions(ctx, now)
	if c.d.Activity != nil {
		in.Activity = true
		in.InFlight, in.LastActivity = c.d.Activity()
	}
	c.view.decision = Decide(in)
	switch {
	case c.applying != nil:
		c.view.decision.Wait = applyingWords(c.applying)
	case !c.view.decision.Apply:
	case c.updaterRunning():
		// Not this daemon's attempt (it would be applying): a manual update
		// that has not written its record yet, or one whose heartbeat stalled.
		c.view.decision = waitFor(WaitInProgress, "an updater is already running")
	case c.stale(now) && now.Before(c.state.RecheckAt):
		c.view.decision = waitFor(WaitRecheck, fmt.Sprintf("re-checking the release list in %s before installing v%s",
			UntilWords(c.state.RecheckAt.Sub(now)), target.Version))
	}
	return changed
}

// stale reports whether the last successful fetch is too old for a spawn to
// rely on (recheckFresh), or unknown.
func (c *Checker) stale(now time.Time) bool {
	since := now.Sub(c.lastGood)
	return c.lastGood.IsZero() || since < 0 || since >= recheckFresh
}

// liveSessions counts the live agent sessions at now; no counter is an error,
// which Decide treats as not idle.
func (c *Checker) liveSessions(ctx context.Context, now time.Time) (int, error) {
	if c.d.LiveSessions == nil {
		return 0, errNoSessionCounter
	}
	idle := c.d.SessionIdle
	if idle <= 0 {
		idle = time.Duration(config.Defaults().Gardener.SessionIdleMinutes) * time.Minute
	}
	return c.d.LiveSessions(ctx, now.Add(-idle))
}

// applyAuto carries out a decision to apply (evaluate has already waited
// out an updater holding the lock and a re-check that is not due). A spawn
// relies on a release list fetched within recheckFresh: older than that, it
// re-checks first -- at most once per recheckSpacing, saved before the
// request -- and spawns only when the target is unchanged and the decision,
// taken again on the fresh list, still says apply (the yank gap, and a soak
// measured against GitHub's fresh Date).
func (c *Checker) applyAuto(ctx context.Context, s Settings, cur Version) {
	if c.view.target == nil {
		return
	}
	target, path := *c.view.target, c.view.decision.Path
	now := c.d.Now()
	if c.stale(now) {
		c.state.RecheckAt = now.Add(recheckSpacing)
		c.persist()
		if err := c.check(ctx, now, s); err != nil {
			c.view.decision = waitFor(WaitRecheck, fmt.Sprintf("the release list could not be re-checked before installing v%s; trying again later",
				target.Version))
			return
		}
		now = c.d.Now()
		if c.evaluate(ctx, now, s, cur) {
			c.persist()
		}
		switch {
		case c.view.target == nil:
			return // the evaluation says why
		case c.view.target.Version != target.Version:
			c.view.decision = waitFor(WaitRecheck, fmt.Sprintf("the release list changed on a re-check (v%s, not v%s); deciding again",
				c.view.target.Version, target.Version))
			return
		case !c.view.decision.Apply:
			return
		}
		path = c.view.decision.Path
	}
	if err := c.spawn(ctx, now, cur, target.Version, WhyAuto, path); err != nil {
		c.log.Debug("update: the automatic update did not start", "to", target.Version.String(), "err", err)
	}
}

// spawn starts one attempt: mint its id, save it as the pending spawn (with
// the Codex fingerprint), record update.started, and hand it to the Spawner.
// A Spawner error settles the attempt as failed before it started (backoff,
// update.failed); a spawn that cannot be saved is not started at all.
func (c *Checker) spawn(ctx context.Context, now time.Time, from, to Version, why, path string) error {
	id, err := core.NewID()
	if err != nil {
		c.view.decision = waitFor(WaitSave, "an attempt id could not be minted; trying again at the next tick")
		return fmt.Errorf("update.spawn: %w", err)
	}
	req := SpawnRequest{AttemptID: id, From: from, To: to, Why: why}
	if err := req.Validate(); err != nil {
		return fmt.Errorf("update.spawn: %w", err)
	}
	sp := &Spawn{ID: id, From: from, To: to, Why: why, Path: path, At: now, CodexHooks: c.codexFingerprint()}
	if sp.CodexHooks != "" {
		c.state.CodexHooks = sp.CodexHooks
	}
	c.state.Spawn = sp
	if !c.persist() {
		c.state.Spawn = nil
		c.view.decision = waitFor(WaitSave, "the update state could not be saved; installing waits until it can be")
		return errStateNotSaved
	}
	c.record(ctx, core.Event{
		Kind:   core.EventUpdateStarted,
		ItemID: to.String(),
		Payload: map[string]any{
			"from": from.String(), "to": to.String(), "why": why, "path": path, "attempt": id,
		},
	})
	if err := c.d.Spawner.Spawn(ctx, req); err != nil {
		if ctx.Err() != nil {
			// Shutting down: the updater may or may not have started. The saved
			// spawn stays for the next daemon to fold either way.
			return fmt.Errorf("update.spawn: %w", err)
		}
		c.log.Warn("update: could not start the updater; trying again after a backoff",
			"to", to.String(), "why", why, "attempt", id, "err", err)
		c.state.Spawn = nil
		res := AttemptResult{ID: id, From: from, To: to, Why: why, Outcome: OutcomeFailed, Stage: StageSpawn,
			SpawnedAt: now, FoldedAt: now}
		c.settle(ctx, res, now, from, true)
		c.persist()
		c.applying = nil
		c.view.applying = nil
		c.view.decision = waitFor(WaitBackoff, "the updater could not be started; trying again after a backoff")
		return fmt.Errorf("update.spawn: %w", err)
	}
	c.log.Info("update: started an update", "from", from.String(), "to", to.String(), "why", why, "path", path, "attempt", id)
	c.applying = &Applying{ID: id, From: from, To: to, Why: why, SpawnedAt: now}
	c.view.applying = c.applying
	c.view.decision = waitFor(WaitInProgress, applyingWords(c.applying))
	return nil
}

// applyNow serves ApplyNow.
func (c *Checker) applyNow(ctx context.Context, s Settings) error {
	switch {
	case !s.Check:
		return ErrChecksOff
	case c.d.Install.NotifyOnly():
		reason := c.d.Install.Reason
		if reason == "" {
			reason = "the installer did not make this install"
		}
		return fmt.Errorf("%w: %s", ErrCannotApply, reason)
	case c.d.Spawner == nil || !c.release:
		return fmt.Errorf("%w: no updater is wired into this daemon", ErrCannotApply)
	}
	cur, _ := Parse(c.d.Version) // a release build always parses
	now := c.d.Now()
	if c.fold(ctx, now, cur) {
		c.persist()
	}
	if c.applying != nil {
		c.step(ctx, s, false)
		return ErrInProgress
	}
	if c.stale(now) {
		if !c.lastFetch.IsZero() && now.Sub(c.lastFetch) < checkNowThrottle {
			c.step(ctx, s, false)
			return ErrTooSoon // the last request, a minute ago, failed
		}
		if err := c.check(ctx, now, s); err != nil {
			c.step(ctx, s, false)
			return err
		}
		now = c.d.Now()
	}
	newest, ok := Newest(c.state.Releases)
	switch {
	case !ok || newest.Version.Compare(cur) <= 0:
		c.step(ctx, s, false)
		return ErrUpToDate
	case !newest.ChecksumsBundle:
		c.step(ctx, s, false)
		return ErrUnsigned
	case c.updaterRunning():
		c.step(ctx, s, false)
		return ErrInProgress
	}
	if c.state.Hold != nil {
		c.log.Info("update: Update now lifts the hold", "skip_through", c.state.Hold.Through.String())
		c.state.Hold = nil
	}
	err := c.spawn(ctx, now, cur, newest.Version, WhyNow, PathNow)
	c.persist()
	c.step(ctx, s, false)
	return err
}

// resume serves Resume.
func (c *Checker) resume(ctx context.Context, s Settings) {
	if c.state.Hold == nil && c.state.Paused == nil && c.state.Rollbacks == nil {
		c.step(ctx, s, false)
		return
	}
	c.log.Info("update: automatic updates resumed", "hold", c.state.Hold != nil, "paused", c.state.Paused != nil)
	c.state.Hold, c.state.Paused, c.state.Rollbacks = nil, nil, nil
	c.persist()
	c.step(ctx, s, false)
}

// fold reads attempt.json and settles what it says: about the spawn this
// daemon (or the one before it) is waiting on, about a late record of one it
// folded as interrupted, or about an attempt it did not start (a manual
// `seamlessd update`, shown and nothing more). A spawn no record names is
// waited on for AttemptStartWindow, then looked up in the history (a later
// attempt may have replaced its record), and otherwise folded as never
// started -- unless an updater holds the lock. Nothing is folded while an
// attempt is live; c.applying names it instead. It reports whether the state
// changed.
func (c *Checker) fold(ctx context.Context, now time.Time, cur Version) bool {
	c.applying = nil
	rec, haveRec := c.readAttempt()
	sp := c.state.Spawn
	mine := haveRec && sp != nil && rec.ID == sp.ID
	dirty := false

	if haveRec {
		last := c.state.LastAttempt
		switch {
		case rec.Live(now), mine && !rec.Finished() && c.updaterRunning():
			// Under way (a stale heartbeat from this daemon's own updater, with
			// the lock held, is too): whatever else is pending waits for it.
			c.applying = applyingOf(rec, sp, mine)
			return false
		case mine:
			res := resultOf(rec, Classify(rec, cur), now)
			res.SpawnedAt = sp.At
			c.state.Spawn = nil
			c.settle(ctx, res, now, cur, true)
			return true
		case last != nil && rec.ID == last.ID:
			// Folded already. Only a record that finished after being folded as
			// interrupted (laptop sleep, a Windows rename retried) says more.
			if last.Outcome == OutcomeInterrupted && rec.Finished() {
				res := resultOf(rec, Classify(rec, cur), now)
				res.SpawnedAt = last.SpawnedAt
				c.settle(ctx, res, now, cur, false)
				dirty = true
			}
		case last == nil || rec.StartedAt.After(lastSeen(last)):
			// Not this daemon's spawn: shown as the last attempt, with no
			// consequences (a manual update, or one whose spawn record was lost).
			res := resultOf(rec, Classify(rec, cur), now)
			c.state.LastAttempt = &res
			dirty = true
		}
	}

	if sp := c.state.Spawn; sp != nil {
		if sp.At.After(now) { // the clock went back (a command, between ticks)
			sp.At, dirty = now, true
		}
		switch {
		case now.Sub(sp.At) < AttemptStartWindow:
			c.applying = applyingOfSpawn(sp)
			return dirty
		case c.updaterRunning():
			c.applying = applyingOfSpawn(sp)
			return dirty
		}
		if h, ok := c.fromHistory(sp.ID); ok {
			res := resultOf(h, Classify(h, cur), now)
			res.SpawnedAt = sp.At
			c.state.Spawn = nil
			c.settle(ctx, res, now, cur, true)
			return true
		}
		outcome := OutcomeInterrupted
		switch {
		case cur == sp.To:
			outcome = OutcomeUnverified
		case cur != sp.From:
			outcome = OutcomeSuperseded
		}
		c.state.Spawn = nil
		c.settle(ctx, AttemptResult{
			ID: sp.ID, From: sp.From, To: sp.To, Why: sp.Why, Outcome: outcome, Stage: StageSpawn,
			SpawnedAt: sp.At, FoldedAt: now,
		}, now, cur, true)
		return true
	}
	return dirty
}

// lastSeen is when the last folded attempt began, for telling a newer record
// from an older one: its record's start, else its spawn.
func lastSeen(a *AttemptResult) time.Time {
	if !a.StartedAt.IsZero() {
		return a.StartedAt
	}
	return a.SpawnedAt
}

// settle applies res (applyOutcome), records update.failed for a failure of
// an attempt a daemon started, and logs it. The payload carries versions and
// fixed words only -- a known gate refusal among them -- never the updater's
// error text.
//
// An attempt reported applied while this daemon, running cur, is still below
// its To cannot happen with one daemon per data dir -- the updater confirms
// the new daemon before it says so -- but if it ever did, the target would
// still be there and the next tick would start the same update again; a
// backoff keeps that from becoming a loop.
func (c *Checker) settle(ctx context.Context, res AttemptResult, now time.Time, cur Version, recount bool) {
	failed := applyOutcome(&c.state, res, now, recount)
	if (res.Outcome == OutcomeApplied || res.Outcome == OutcomeUnverified) && spawnedByDaemon(res.Why) &&
		cur.Compare(res.To) < 0 {
		c.log.Warn("update: an update reports applied, but this daemon still runs the version it replaced",
			"running", cur.String(), "to", res.To.String(), "attempt", res.ID)
		c.state.backoff(res.To, res.Outcome, now)
	}
	attrs := []any{"from", res.From.String(), "to", res.To.String(), "why", res.Why,
		"outcome", res.Outcome, "stage", string(res.Stage), "attempt", res.ID}
	switch {
	case failed:
		payload := map[string]any{
			"from": res.From.String(), "to": res.To.String(), "outcome": res.Outcome,
			"stage": string(res.Stage), "why": res.Why, "rolled_back": res.RolledBack, "attempt": res.ID,
		}
		// A gate refusal this release knows, as its fixed word; a newer
		// updater's word is left out, as a plain failure.
		if r := res.Refused(); r != "" {
			payload["refusal"] = r
			attrs = append(attrs, "refusal", r)
		}
		c.log.Warn("update: an update attempt did not apply", attrs...)
		c.record(ctx, core.Event{
			Kind:    core.EventUpdateFailed,
			ItemID:  res.To.String(),
			Payload: payload,
		})
	default:
		c.log.Info("update: an update attempt finished", attrs...)
	}
}

// readAttempt reads attempt.json. An unreadable record is warned about once
// per distinct error and treated as none: the daemon never repairs it, and the
// next attempt's first write replaces it.
func (c *Checker) readAttempt() (Attempt, bool) {
	a, err := ReadAttempt(c.d.DataDir)
	if err != nil {
		if msg := err.Error(); msg != c.attemptErr {
			c.log.Warn("update: the update attempt record is unreadable; treating it as absent", "err", err)
			c.attemptErr = msg
		}
		return Attempt{}, false
	}
	c.attemptErr = ""
	return a, a.ID != ""
}

// fromHistory finds attempt id in attempts.jsonl, newest line first, logging
// the lines it had to skip.
func (c *Checker) fromHistory(id string) (Attempt, bool) {
	hist, skipped, err := ReadAttemptHistory(c.d.DataDir)
	if err != nil {
		c.log.Warn("update: the update attempt history is unreadable", "err", err)
		return Attempt{}, false
	}
	if skipped > 0 {
		c.log.Warn("update: skipped unreadable lines in the update attempt history", "skipped", skipped)
	}
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].ID == id {
			return hist[i], true
		}
	}
	return Attempt{}, false
}

// updaterRunning asks Deps.UpdaterRunning whether an updater holds the lock;
// nil or an error is "no", leaving the heartbeat to decide.
func (c *Checker) updaterRunning() bool {
	if c.d.UpdaterRunning == nil {
		return false
	}
	held, err := c.d.UpdaterRunning()
	if err != nil {
		c.log.Debug("update: could not probe the updater lock", "err", err)
		return false
	}
	return held
}

// codexFingerprint is Deps.CodexHooksHash's answer, or "" when there is none
// or it fails (logged once per distinct error).
func (c *Checker) codexFingerprint() string {
	if c.d.CodexHooksHash == nil {
		return ""
	}
	h, err := c.d.CodexHooksHash()
	if err != nil {
		if msg := err.Error(); msg != c.codexErr {
			c.log.Warn("update: could not fingerprint Codex's hooks.json; no Codex banner for this update", "err", err)
			c.codexErr = msg
		}
		return ""
	}
	c.codexErr = ""
	return h
}

// applyingOf describes a record under way; sp is the spawn it belongs to when
// mine.
func applyingOf(a Attempt, sp *Spawn, mine bool) *Applying {
	ap := &Applying{ID: a.ID, From: a.From, To: a.To, Why: a.Why, Stage: a.Stage, HeartbeatAt: a.HeartbeatAt}
	if mine {
		ap.SpawnedAt = sp.At
	}
	return ap
}

// applyingOfSpawn describes a spawn no record names yet.
func applyingOfSpawn(sp *Spawn) *Applying {
	return &Applying{ID: sp.ID, From: sp.From, To: sp.To, Why: sp.Why, SpawnedAt: sp.At}
}

// applyingWords is the wait reason while an update is under way.
func applyingWords(a *Applying) string {
	if a.Stage == "" {
		return fmt.Sprintf("updating to v%s: the updater is starting", a.To)
	}
	return fmt.Sprintf("updating to v%s: %s", a.To, a.Stage)
}
