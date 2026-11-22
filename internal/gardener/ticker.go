package gardener

import (
	"context"
	"errors"
	"time"
)

const (
	// initialDelay lets the server finish starting before the first tick runs, so
	// startup latency never waits on gardening.
	initialDelay = 20 * time.Second
	// passTimeout bounds a single proposal pass; the digest step may call an
	// LLM, so it is generous relative to the cadence. Upkeep gets one of its own.
	passTimeout = 5 * time.Minute
)

// What started a proposal pass (Run.Trigger).
const (
	TriggerSchedule = "schedule"
	TriggerOwner    = "owner"
)

// RunNow's refusals.
var (
	// ErrProposingOff: gardener.enabled is false, so no proposal pass runs,
	// asked for or not.
	ErrProposingOff = errors.New("the gardener's suggestions are off (gardener.enabled is false in the config)")
	// ErrPassRunning: a proposal pass is already running or waiting to start.
	ErrPassRunning = errors.New("a gardener pass is already running")
	// ErrNotScheduled: the schedule is not running on this service (never
	// started, or stopped), so nothing would run the pass.
	ErrNotScheduled = errors.New("the gardener's schedule is not running on this server")
)

// Run is one proposal pass: what started it, when, and what it found.
type Run struct {
	// Trigger is TriggerSchedule or TriggerOwner; By is who asked for an
	// owner's run (the RunNow caller's actor), "" for the schedule's.
	Trigger string
	By      string
	// Queued is when the pass was asked for; Started and Finished are zero
	// until it gets there.
	Queued, Started, Finished time.Time
	// Result is what the passes produced; Err is why the run as a whole could
	// not proceed (the suppression set unreadable), "" when it did.
	Result PassResult
	Err    string
}

// OK reports whether the run started and every pass in it ran to completion.
func (r Run) OK() bool { return r.Err == "" && r.Result.OK() }

// Status is the pass bookkeeping the console's status line reads: whether the
// proposal passes run at all, the one running now, the last one to finish,
// when the schedule next ticks, and when upkeep last ran.
type Status struct {
	Proposing bool
	// Scheduled is whether the loop is running on this service.
	Scheduled bool
	Interval  time.Duration
	// Running is the pass running or waiting to start, nil when none is.
	Running *Run
	// Last is the latest pass to finish, nil before the first.
	Last *Run
	// Next is when the schedule next ticks (zero while it is not running);
	// Housekept is when upkeep last finished (zero before the first tick).
	Next, Housekept time.Time
	// Skips are the passes a pass will not run, and why (Service.Skips).
	Skips []SkippedPass
}

// Status reports the pass bookkeeping as of now. The runs are copies: a caller
// may keep them.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{
		Proposing: s.cfg.Propose, Scheduled: s.scheduled, Interval: s.cfg.Interval,
		Next: s.next, Housekept: s.housekept, Skips: s.Skips(),
	}
	if s.pass != nil {
		r := *s.pass
		st.Running = &r
	}
	if s.last != nil {
		r := *s.last
		st.Last = &r
	}
	return st
}

// runRequest is the owner's RunNow, handed to the loop.
type runRequest struct{ by string }

// Start launches the gardener's schedule in a background goroutine: one tick
// shortly after startup, then every Interval, until ctx is cancelled. Each tick
// runs upkeep (Housekeep) and, when Propose is on, the proposal passes; the
// owner's RunNow runs the proposal passes between ticks. It never blocks the
// caller and never panics the server (each step is best-effort). The daemon
// calls it whatever gardener.enabled says, at most once, from the goroutine
// that will later call Wait (the serve path).
func (s *Service) Start(ctx context.Context) {
	s.done = make(chan struct{})
	kick := make(chan runRequest, 1)
	s.mu.Lock()
	s.kick, s.scheduled = kick, true
	s.next = time.Now().Add(s.firstDelay)
	s.mu.Unlock()
	go func() {
		defer close(s.done)
		s.loop(ctx, kick)
	}()
}

// Wait blocks until the goroutine launched by Start has exited (its ctx must
// already be cancelled, or Wait blocks until it is). It returns immediately when
// Start was never called. The daemon calls it during shutdown so no pass is
// still touching the DB when the DB closes.
func (s *Service) Wait() {
	if s.done != nil {
		<-s.done
	}
}

func (s *Service) loop(ctx context.Context, kick <-chan runRequest) {
	timer := time.NewTimer(s.firstDelay)
	defer timer.Stop()
	defer func() {
		// Nothing reads kick from here on: refuse RunNow, and drop a request
		// that was queued but never ran.
		s.mu.Lock()
		s.scheduled, s.next, s.pass = false, time.Time{}, nil
		s.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-kick:
			s.runPass(ctx, TriggerOwner, req.by, true)
		case <-timer.C:
			// The cadence counts from each tick's start, so a slow pass does
			// not push every later one back.
			started := time.Now()
			s.tick(ctx)
			next := started.Add(s.cfg.Interval)
			s.mu.Lock()
			s.next = next
			s.mu.Unlock()
			timer.Reset(max(time.Until(next), 0))
		}
	}
}

// tick is one scheduled round: upkeep always, then the proposal passes when
// they are on and no pass the owner asked for is waiting to start (that one
// runs right after, over the same fresh stats).
func (s *Service) tick(ctx context.Context) {
	upkeep, cancel := context.WithTimeout(ctx, passTimeout)
	s.Housekeep(upkeep)
	cancel()
	if !s.cfg.Propose {
		return
	}
	s.runPass(ctx, TriggerSchedule, "", false)
}

// RunNow asks the schedule's loop for a proposal pass now, between ticks, and
// returns as soon as it is queued: the pass runs in the background under the
// loop's own context and timeout, and Status reports it. ctx names who asked
// (WithActor); the finished pass records a gardener.action event (run_now) in
// that name. It refuses with ErrProposingOff while gardener.enabled is false,
// ErrPassRunning while a pass runs or waits to start, and ErrNotScheduled while
// the schedule is not running.
func (s *Service) RunNow(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !s.cfg.Propose:
		return ErrProposingOff
	case !s.scheduled:
		return ErrNotScheduled
	case s.pass != nil:
		return ErrPassRunning
	}
	by := ActorOf(ctx)
	s.pass = &Run{Trigger: TriggerOwner, By: by, Queued: s.now().UTC()}
	// The buffer holds one request and s.pass guards it (the loop clears it
	// only once the request is done), so this never blocks.
	s.kick <- runRequest{by: by}
	return nil
}

// runPass runs one proposal pass under the pass timeout and records it for
// Status, logging a failure rather than propagating it (the loop must keep
// running). A scheduled pass is skipped while an owner's pass waits to start.
// fresh says whether the stats need refreshing first: the owner's pass does,
// the tick's just ran upkeep.
func (s *Service) runPass(ctx context.Context, trigger, by string, fresh bool) {
	now := s.now().UTC()
	s.mu.Lock()
	switch {
	case trigger == TriggerSchedule && s.pass != nil:
		s.mu.Unlock()
		return
	case s.pass == nil:
		s.pass = &Run{Trigger: trigger, By: by, Queued: now}
	}
	s.pass.Started = now
	run := *s.pass
	s.mu.Unlock()

	passCtx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()
	var (
		res PassResult
		err error
	)
	if fresh {
		res, err = s.Propose(passCtx)
	} else {
		res, err = s.propose(passCtx)
	}
	if err != nil {
		s.logger.Warn("gardener: pass failed", "trigger", trigger, "error", err)
		run.Err = err.Error()
	}
	run.Result, run.Finished = res, s.now().UTC()

	s.mu.Lock()
	s.pass, s.last = nil, &run
	s.mu.Unlock()

	if trigger == TriggerOwner {
		// The owner asked for this pass: its answer is an event, which is also
		// what refreshes the console page that asked. It carries no counts:
		// they span every project, locked ones included.
		payload := map[string]any{"action": ActionRunNow}
		if len(res.Failed) > 0 {
			payload["failed"] = res.Failed
		}
		if run.Err != "" {
			payload["error"] = "the pass could not start"
		}
		s.record(WithActor(context.WithoutCancel(ctx), by), "", payload)
	}
}
