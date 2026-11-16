package main

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// lqQueuedJSON is a queued answer: the lock held by another session, this
// session at position of waiting.
func lqQueuedJSON(position, waiting int) string {
	return `{"lock":{"id":"01KLOCKLOCKLOCKLOCKLOCKLOC","label":"app main","repo":"app","name":"main",` +
		`"refs":["refs/heads/main"],"state":"active","held":true,"holder_session":"cc/example-holder",` +
		`"until":"2026-11-15T18:15:00.000000000Z","aliases":[],"projects":[],"held_by_caller":false,` +
		`"waiting":` + strconv.Itoa(waiting) + `,"next_waiter_session":"cc/example-first"},` +
		`"acquired":false,"queued":true,"position":` + strconv.Itoa(position) + `,"place_until":"2026-11-15T18:02:00.000000000Z"}`
}

// lqFastRetry makes a wait ask again at once, for the test.
func lqFastRetry(t *testing.T) {
	t.Helper()
	prev := lqRetryEvery
	lqRetryEvery = time.Millisecond
	t.Cleanup(func() { lqRetryEvery = prev })
}

// lqCalls returns the tools the fixture served, in order, with their args.
func (fx *daemonFixture) lqCalls() []recordedCall {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return append([]recordedCall(nil), fx.calls...)
}

// --wait asks with wait until the lock is this session's, printing each new
// place on stderr, and then prints the acquire line and exits 0.
func TestLockWait_AsksUntilItHoldsTheLock(t *testing.T) {
	lqFastRetry(t)
	var mu sync.Mutex
	replies := []string{lqQueuedJSON(2, 2), lqQueuedJSON(2, 2), lqQueuedJSON(1, 1),
		`{"lock":` + lkLockJSON + `,"acquired":true,"heartbeat":false,"reclaimed":false}`}
	fx := newDaemonFixture(t, map[string]toolFunc{
		"locks_acquire": func(map[string]any) toolReply {
			mu.Lock()
			defer mu.Unlock()
			r := replies[0]
			if len(replies) > 1 {
				replies = replies[1:]
			}
			return toolReply{text: r}
		},
	}, nil, "")

	stdout, stderr, code := fx.run(t, "lock", "acquire", "--id", "01KLOCKLOCKLOCKLOCKLOCKLOC", "--wait", "--session", "cc/example-agent")
	require.Equal(t, 0, code, stderr)
	require.Equal(t, "acquired LOCK app main until 2026-11-15T18:15:00Z\n", stdout)
	require.Equal(t, "queued for LOCK app main: number 2 of 2, next cc/example-first; held by cc/example-holder\n"+
		"queued for LOCK app main: number 1 of 1; held by cc/example-holder\n", stderr, "a line per new place, never per ask")
	calls := fx.lqCalls()
	require.Len(t, calls, 4)
	for _, c := range calls {
		require.Equal(t, "locks_acquire", c.name)
		require.Equal(t, true, c.args["wait"])
		require.Equal(t, "cc/example-agent", c.args["session"])
		require.Equal(t, "01KLOCKLOCKLOCKLOCKLOCKLOC", c.args["id"])
	}
}

// --timeout ends a wait that never got the lock: the wait leaves the queue (a
// release naming the same lock and session) and exits 1.
func TestLockWait_TimeoutLeavesTheQueue(t *testing.T) {
	lqFastRetry(t)
	fx := newDaemonFixture(t, map[string]toolFunc{
		"locks_acquire": fixedReply(lqQueuedJSON(2, 3)),
		"locks_release": fixedReply(`{"lock":` + lkLockJSON + `,"released":false,"left_queue":true}`),
	}, nil, "")
	stdout, stderr, code := fx.run(t, "lock", "acquire", "--id", "01KLOCKLOCKLOCKLOCKLOCKLOC", "--wait", "--timeout", "40ms",
		"--lease", "600", "--session", "cc/example-agent")
	require.Equal(t, 1, code)
	require.Empty(t, stdout)
	require.Contains(t, stderr, "timed out after 40ms waiting for LOCK app main (number 2 in its queue): left the queue")
	last := fx.lastCall(t)
	require.Equal(t, "locks_release", last.name)
	require.Equal(t, map[string]any{"id": "01KLOCKLOCKLOCKLOCKLOCKLOC", "session": "cc/example-agent"}, last.args,
		"the same lock and session; no wait, no lease")
}

// A refusal that is no place in the queue ends the wait at once, with the
// server's words, and gives up nothing.
func TestLockWait_ARefusalEndsTheWait(t *testing.T) {
	lqFastRetry(t)
	fx := newDaemonFixture(t, map[string]toolFunc{
		"locks_acquire": func(map[string]any) toolReply {
			return toolReply{text: `locks_acquire: lock not found: no lock with id "01KNOPE"`, isError: true}
		},
		"locks_release": fixedReply(`{}`),
	}, nil, "")
	_, stderr, code := fx.run(t, "lock", "acquire", "--id", "01KNOPE", "--wait")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "no lock with id")
	require.Equal(t, 1, fx.callCount(), "one ask, no release")
}

func TestLockWait_FlagRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		code int
		want string
	}{
		{"a timeout with no wait", []string{"lock", "acquire", "--id", "01K", "--timeout", "1m"}, 1, "pass --wait with it"},
		{"a zero timeout", []string{"lock", "acquire", "--id", "01K", "--wait", "--timeout", "0s"}, 2, "must be a positive duration"},
		{"a timeout that is no duration", []string{"lock", "acquire", "--id", "01K", "--wait", "--timeout", "soon"}, 2,
			"must be a positive duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, errb := stubEnv()
			require.Equal(t, tc.code, dispatch(context.Background(), e, tc.argv), errb.String())
			require.Contains(t, errb.String(), tc.want)
		})
	}
}

// seam lock list shows a lock's queue: its length, and the next waiter where
// the server named it.
func TestLockWait_ListShowsTheQueue(t *testing.T) {
	require.Equal(t, "LOCK app main  held by cc/example-holder until 2026-11-15T18:15:00Z  refs/heads/main  3 waiting, next cc/example-first",
		lkLine(map[string]any{"label": "app main", "state": "active", "held": true, "holder_session": "cc/example-holder",
			"until": "2026-11-15T18:15:00.000000000Z", "refs": []any{"refs/heads/main"}, "waiting": float64(3),
			"next_waiter_session": "cc/example-first", "aliases": []any{}}))
	require.Equal(t, "LOCK app main  held until 2026-11-15T18:15:00Z  refs/heads/main  1 waiting",
		lkLine(map[string]any{"label": "app main", "state": "active", "held": true, "until": "2026-11-15T18:15:00.000000000Z",
			"refs": []any{"refs/heads/main"}, "waiting": float64(1), "aliases": []any{}}),
		"a holder and a waiter the server does not name, and no aliases at all, still read")
}
