package mcp_test

// The repo lock queue at the tool surface (plan repo-locks, step RL5):
// locks_acquire with wait joins the queue or keeps a place, anyone the queue
// does not put first is refused with the next waiter and their own place, and
// locks_release from a waiting session leaves the queue.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// lqCountActions counts a lock's events of action.
func lqCountActions(t *testing.T, db *sql.DB, lockID, action string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND item_id = ? AND json_extract(payload, '$.action') = ?`,
		string(core.EventRepoLockChanged), lockID, action).Scan(&n))
	return n
}

// Waiters take the lock in arrival order, and while the queue has a live head
// a request from anyone else is refused, waiting or not.
func TestLocksQueue_WaitersTakeTheLockInArrivalOrder(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	for _, name := range []string{"cc/lq-a", "cc/lq-b", "cc/lq-c"} {
		seedAmbient(t, ctx, db, name, "demo")
	}
	cli := dialClient(t, ctx, url, testKey)
	id := lqLock(t, callJSON(t, ctx, cli, "locks_declare", map[string]any{"repo_root": "/work/demo",
		"repo_origin": "git@github.com:example/demo.git"})["lock"])["id"].(string)
	as := func(session string, extra map[string]any) map[string]any {
		args := map[string]any{"id": id, "session": session}
		for k, v := range extra {
			args[k] = v
		}
		return args
	}
	callJSON(t, ctx, cli, "locks_acquire", as("cc/lq-a", nil))

	b := callJSON(t, ctx, cli, "locks_acquire", as("cc/lq-b", map[string]any{"wait": true}))
	require.Equal(t, false, b["acquired"])
	require.Equal(t, true, b["queued"])
	require.EqualValues(t, 1, b["position"])
	require.NotEmpty(t, b["place_until"])
	c := callJSON(t, ctx, cli, "locks_acquire", as("cc/lq-c", map[string]any{"wait": true}))
	require.EqualValues(t, 2, c["position"])
	l := lqLock(t, c["lock"])
	require.EqualValues(t, 2, l["waiting"])
	require.Equal(t, "cc/lq-b", l["next_waiter_session"])
	require.Equal(t, "cc/lq-a", l["holder_session"], "an ordinary holder")

	isErr, text := callErr(t, ctx, cli, "locks_acquire", as("cc/lq-c", nil))
	require.True(t, isErr)
	require.Contains(t, text, "LOCK demo main is held by session cc/lq-a")
	require.Contains(t, text, "2 sessions wait for it and you are number 2 in its queue")

	callJSON(t, ctx, cli, "locks_release", as("cc/lq-a", nil))
	isErr, text = callErr(t, ctx, cli, "locks_acquire", as("cc/lq-c", nil))
	require.True(t, isErr, "a free lock goes to the head")
	require.Contains(t, text, "LOCK demo main is queued: 2 sessions wait for it, session cc/lq-b is next, and you are number 2")
	isErr, text = callErr(t, ctx, cli, "locks_acquire", as("cc/lq-a", nil))
	require.True(t, isErr, "its last holder too")
	require.Contains(t, text, "you are not in its queue")
	again := callJSON(t, ctx, cli, "locks_acquire", as("cc/lq-c", map[string]any{"wait": true}))
	require.EqualValues(t, 2, again["position"], "asking again keeps the place")

	took := callJSON(t, ctx, cli, "locks_acquire", as("cc/lq-b", nil))
	require.Equal(t, true, took["acquired"])
	l = lqLock(t, took["lock"])
	require.Equal(t, "cc/lq-b", l["holder_session"])
	require.EqualValues(t, 1, l["waiting"])
	require.Equal(t, "cc/lq-c", l["next_waiter_session"])

	callJSON(t, ctx, cli, "locks_release", as("cc/lq-b", nil))
	require.Equal(t, true, callJSON(t, ctx, cli, "locks_acquire", as("cc/lq-c", nil))["acquired"])
	require.Equal(t, 2, lqCountActions(t, db, id, core.RepoLockActionQueued), "joining is an event, keeping a place is not")
}

// A release from a session that waits and does not hold leaves the queue;
// one that does neither is refused.
func TestLocksQueue_AReleaseFromAWaiterLeavesTheQueue(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	seedAmbient(t, ctx, db, "cc/lq-a", "demo")
	seedAmbient(t, ctx, db, "cc/lq-b", "demo")
	cli := dialClient(t, ctx, url, testKey)
	id := lqLock(t, callJSON(t, ctx, cli, "locks_declare", lkCheckout(nil))["lock"])["id"].(string)
	callJSON(t, ctx, cli, "locks_acquire", map[string]any{"id": id, "session": "cc/lq-a"})
	callJSON(t, ctx, cli, "locks_acquire", map[string]any{"id": id, "session": "cc/lq-b", "wait": true})

	left := callJSON(t, ctx, cli, "locks_release", map[string]any{"id": id, "session": "cc/lq-b"})
	require.Equal(t, false, left["released"])
	require.Equal(t, true, left["left_queue"])
	l := lqLock(t, left["lock"])
	require.EqualValues(t, 0, l["waiting"])
	require.Equal(t, "cc/lq-a", l["holder_session"], "the hold is untouched")
	require.Equal(t, 1, lqCountActions(t, db, id, core.RepoLockActionLeftQueue))

	isErr, text := callErr(t, ctx, cli, "locks_release", map[string]any{"id": id, "session": "cc/lq-b"})
	require.True(t, isErr)
	require.Contains(t, text, "is not held by this session, which does not wait for it either")
}

// Who may wait is who may hold: a restricted account outside the lock's reach
// is refused as at a missing lock and takes no place; within reach it waits,
// and its place names it only to a reader who may see its session.
func TestLocksQueue_ARestrictedWaiterIsJudgedAsAHolder(t *testing.T) {
	ctx := context.Background()
	r := lkNewRig(t)
	lkSession(t, r.db, "_example_agent:cc/lq-agent", "example-open", "_example_agent")
	lkSession(t, r.db, "cc/lq-owner", "example-open", "")
	open := lqLock(t, callJSON(t, ctx, r.owner, "locks_declare", map[string]any{"repo_root": "/work/open",
		"repo_origin": "https://example.com/team/open.git"})["lock"])["id"].(string)
	ops := lqLock(t, callJSON(t, ctx, r.owner, "locks_declare", map[string]any{"repo_root": "/work/ops",
		"repo_origin": "https://example.com/team/ops.git"})["lock"])["id"].(string)
	for _, id := range []string{open, ops} {
		callJSON(t, ctx, r.owner, "locks_acquire", map[string]any{"id": id, "session": "cc/lq-owner"})
	}

	isErr, text := callErr(t, ctx, r.agent, "locks_acquire", map[string]any{"id": ops, "session": "cc/lq-agent", "wait": true})
	require.True(t, isErr)
	require.Contains(t, text, "no lock with id")
	var places int
	require.NoError(t, r.db.QueryRow(`SELECT COUNT(*) FROM repo_lock_waiters`).Scan(&places))
	require.Zero(t, places, "outside its reach it takes no place")

	got := callJSON(t, ctx, r.agent, "locks_acquire", map[string]any{"id": open, "session": "cc/lq-agent", "wait": true})
	require.Equal(t, true, got["queued"])
	l := lqLock(t, got["lock"])
	require.NotNil(t, l["next_waiter_session"], "its own session is named to it")
	require.Nil(t, l["holder_session"], "the owner's session is not")
	account, ok := lqEventAccount(t, r.db, open, core.RepoLockActionQueued)
	require.True(t, ok)
	require.Equal(t, "_example_agent", account)

	for _, raw := range callJSON(t, ctx, r.owner, "locks_list", map[string]any{})["locks"].([]any) {
		l := lqLock(t, raw)
		if l["id"] != open {
			continue
		}
		require.EqualValues(t, 1, l["waiting"], "the owner sees the queue's length")
		require.Nil(t, l["next_waiter_session"], "and not a restricted account's session")
	}
}
