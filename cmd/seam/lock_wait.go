package main

// seam lock acquire --wait -- queue for a repo lock and hold it once it is
// yours. Each attempt is locks_acquire with wait: the server takes the lock for
// this session when it is free and the queue puts this session first, and
// otherwise keeps this session's place in the queue for a little while
// (core.RepoLockWaitLease). So the loop asks again every few seconds, well
// inside that lease: a wait that stops asking -- killed, or interrupted --
// loses its place within minutes and never blocks the sessions behind it. It
// is made for an agent harness's background mode: the command exits once the
// lock is held, and the agent is re-invoked then.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
)

// lqRetryEvery is how often a wait asks for the lock again. A variable so a
// test can ask without waiting; it stays far inside core.RepoLockWaitLease.
var lqRetryEvery = 3 * time.Second

// lqLeaveTimeout bounds the release that gives up a place when a wait ends
// unheld, so a daemon that stopped answering cannot hold the command open.
const lqLeaveTimeout = 10 * time.Second

// lqDurationValue is a duration flag that accepts only a positive value, so 0
// can mean absent (no limit) as posIntValue's 0 does.
type lqDurationValue struct{ val *time.Duration }

func (d *lqDurationValue) String() string {
	if d.val == nil {
		return time.Duration(0).String() // the zero value's own text, so PrintDefaults omits it
	}
	return d.val.String()
}

func (d *lqDurationValue) Set(s string) error {
	v, err := time.ParseDuration(s)
	if err != nil || v <= 0 {
		return errors.New("must be a positive duration, such as 90s or 10m")
	}
	*d.val = v
	return nil
}

// lqDurationFlag registers a duration flag that only accepts positive values.
func lqDurationFlag(fs *flag.FlagSet, name, usage string) *time.Duration {
	p := new(time.Duration)
	fs.Var(&lqDurationValue{val: p}, name, usage)
	return p
}

// lqWait asks for the lock with wait until this session holds it, the timeout
// ends the wait (0: no limit), or the command is interrupted. Holding it, it
// prints the acquire line (the answer itself with asJSON) and returns nil. A
// refusal that is no place in the queue -- no such lock, a declaration the
// owner has not approved -- ends the wait at once with that refusal. A wait
// that ends unheld gives up its place (locks_release from a waiting session
// leaves the queue) and returns an error, so the command exits 1.
func lqWait(ctx context.Context, e *env, cli *mcpclient.Client, args map[string]any, timeout time.Duration, asJSON bool) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	ask := map[string]any{"wait": true}
	for k, v := range args {
		ask[k] = v
	}
	label, position := "", 0
	for {
		out, text, err := lkCall(ctx, cli, "locks_acquire", ask)
		if err != nil {
			if ctx.Err() != nil {
				lqLeave(ctx, e, cli, args, position)
				return fmt.Errorf("the wait for the lock was interrupted: %w", ctx.Err())
			}
			return err
		}
		l, _ := out["lock"].(map[string]any)
		label = str(l["label"])
		if out["queued"] != true {
			if asJSON {
				return writeRaw(e.stdout, []byte(text))
			}
			fmt.Fprintln(e.stdout, lkAcquiredLine(out))
			return nil
		}
		if p := int(num(out["position"])); p != position {
			position = p
			fmt.Fprintln(e.stderr, lqQueuedLine(l, position))
		}
		next := time.NewTimer(lqRetryEvery)
		select {
		case <-next.C:
		case <-deadline:
			next.Stop()
			lqLeave(ctx, e, cli, args, position)
			return fmt.Errorf("timed out after %s waiting for LOCK %s (number %d in its queue): left the queue", timeout, label, position)
		case <-ctx.Done():
			next.Stop()
			lqLeave(ctx, e, cli, args, position)
			return fmt.Errorf("the wait for LOCK %s was interrupted (number %d in its queue): left the queue", label, position)
		}
	}
}

// lqQueuedLine is the progress line a wait prints when its place changes:
// the lock, the place, and the next waiter where the server named it.
func lqQueuedLine(l map[string]any, position int) string {
	line := fmt.Sprintf("queued for LOCK %s: number %d of %d", str(l["label"]), position, int(num(l["waiting"])))
	if next := str(l["next_waiter_session"]); next != "" && position > 1 {
		line += ", next " + next
	}
	if who := str(l["holder_session"]); who != "" && l["held"] == true {
		line += "; held by " + who
	}
	return line
}

// lqLeave gives up the session's place: locks_release names the same lock and
// session, and from a session that waits and does not hold it, leaves the
// queue. It runs on a context of its own, since the wait's may be the one that
// ended. A failure is said, never fatal: an unrenewed place lapses by itself
// within core.RepoLockWaitLease.
func lqLeave(ctx context.Context, e *env, cli *mcpclient.Client, args map[string]any, position int) {
	if position == 0 {
		return
	}
	leaveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lqLeaveTimeout)
	defer cancel()
	release := map[string]any{}
	for k, v := range args {
		if k != "lease_seconds" {
			release[k] = v
		}
	}
	if _, _, err := lkCall(leaveCtx, cli, "locks_release", release); err != nil {
		fmt.Fprintf(e.stderr, "could not leave the queue (the place lapses by itself): %v\n", err)
	}
}
