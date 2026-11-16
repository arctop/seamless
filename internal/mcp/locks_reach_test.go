package mcp_test

// Who may see and hold a repo lock (plan repo-locks, step RL3): the owner
// principal every lock, from any conversation; a restricted account only a
// lock one of whose listed projects it may write, and an unreachable lock
// reads exactly like a missing one. A lock's state carries no content, so a
// confined owner conversation still holds locks, but a declaration and an
// alias record are the caller's text and meet the clearance rule.

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// lqGhostLock is a lock id no lock has.
const lqGhostLock = "01JZZZZZZZZZZZZZZZZZZZZZZZ"

// lqEventAccount reads the account an event of the lock's action carries, ""
// for none; ok is false when there is no such event.
func lqEventAccount(t *testing.T, db *sql.DB, lockID, action string) (string, bool) {
	t.Helper()
	var account sql.NullString
	err := db.QueryRow(`SELECT json_extract(payload, '$.account') FROM events WHERE kind = ? AND item_id = ?
		AND json_extract(payload, '$.action') = ? ORDER BY ts DESC, id DESC LIMIT 1`,
		string(core.EventRepoLockChanged), lockID, action).Scan(&account)
	if err == sql.ErrNoRows {
		return "", false
	}
	require.NoError(t, err)
	return account.String, true
}

// lqLock is one lock object of an answer.
func lqLock(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	require.True(t, ok, "%v", v)
	return m
}

// A restricted account sees and holds a lock only through a listed project it
// may write. Outside that reach the lock is listed nowhere, and by id, by
// checkout or through a declaration the refusal is word for word the one a
// missing lock gets: it never confirms the lock exists.
func TestLocksReach_RestrictedReachesALockOnlyThroughAListedProject(t *testing.T) {
	ctx := context.Background()
	r := lkNewRig(t)
	lkSession(t, r.db, "_example_agent:cc/lq-agent", "example-open", "_example_agent")
	openOrigin, opsOrigin := "https://example.com/team/open.git", "https://example.com/team/ops.git"
	open := lqLock(t, callJSON(t, ctx, r.owner, "locks_declare", map[string]any{"repo_root": "/work/open", "repo_origin": openOrigin})["lock"])
	ops := lqLock(t, callJSON(t, ctx, r.owner, "locks_declare", map[string]any{"repo_root": "/work/ops", "repo_origin": opsOrigin})["lock"])
	require.Equal(t, []any{"example-open"}, open["projects"])
	require.Equal(t, []any{"example-ops"}, ops["projects"])
	opsID := ops["id"].(string)

	require.Len(t, callJSON(t, ctx, r.owner, "locks_list", map[string]any{})["locks"], 2, "the owner principal sees every lock")
	require.Len(t, callJSON(t, ctx, r.none, "locks_list", map[string]any{})["locks"], 2, "so does a request with no View")
	listed := callJSON(t, ctx, r.agent, "locks_list", map[string]any{})["locks"].([]any)
	require.Len(t, listed, 1)
	require.Equal(t, open["id"], lqLock(t, listed[0])["id"])
	require.Empty(t, callJSON(t, ctx, r.agent, "locks_list", map[string]any{"repo_root": "/work/ops", "repo_origin": opsOrigin})["locks"],
		"its repository lists none")

	for _, tool := range []string{"locks_acquire", "locks_release"} {
		isErr, real := callErr(t, ctx, r.agent, tool, map[string]any{"id": opsID, "session": "cc/lq-agent"})
		require.True(t, isErr)
		_, none := callErr(t, ctx, r.agent, tool, map[string]any{"id": lqGhostLock, "session": "cc/lq-agent"})
		require.Equal(t, strings.ReplaceAll(none, lqGhostLock, opsID), real, "%s by id reads as a missing id", tool)

		checkout := map[string]any{"repo_root": "/work/ops", "repo_origin": opsOrigin, "session": "cc/lq-agent"}
		isErr, real = callErr(t, ctx, r.agent, tool, checkout)
		require.True(t, isErr)
		checkout["name"] = "release"
		_, none = callErr(t, ctx, r.agent, tool, checkout)
		require.Equal(t, strings.ReplaceAll(none, "named release", "named main"), real, "%s by checkout reads as a missing lock", tool)
	}

	// A declaration resolving to it changes nothing, and is refused in the one
	// text a declaration listing no project the account may write gets.
	isErr, resolved := callErr(t, ctx, r.agent, "locks_declare", map[string]any{"repo_root": "/work/ops", "repo_origin": opsOrigin,
		"projects": []any{"example-open"}})
	require.True(t, isErr)
	_, noProject := callErr(t, ctx, r.agent, "locks_declare", map[string]any{"repo_root": "/work/nothere"})
	require.Equal(t, noProject, resolved)
	require.NotContains(t, resolved, opsID)
	require.NotContains(t, resolved, "example-ops")
	l, ok, err := store.RepoLockByID(ctx, r.db, opsID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []string{"example-ops"}, l.Projects, "its listed projects are the owner's alone")
	n, _, err := store.CountRepoLocks(ctx, r.db)
	require.NoError(t, err)
	require.Equal(t, 2, n)

	// Within reach it holds the lock, and every acquire and release names the
	// account; the owner principal's carry none.
	got := callJSON(t, ctx, r.agent, "locks_acquire", map[string]any{"id": open["id"], "session": "cc/lq-agent"})
	require.Equal(t, true, got["acquired"])
	account, ok := lqEventAccount(t, r.db, open["id"].(string), core.RepoLockActionAcquired)
	require.True(t, ok)
	require.Equal(t, "_example_agent", account)
	callJSON(t, ctx, r.agent, "locks_release", map[string]any{"id": open["id"], "session": "cc/lq-agent"})
	account, ok = lqEventAccount(t, r.db, open["id"].(string), core.RepoLockActionReleased)
	require.True(t, ok)
	require.Equal(t, "_example_agent", account)

	lkSession(t, r.db, "cc/lq-owner", "example-ops", "")
	callJSON(t, ctx, r.owner, "locks_acquire", map[string]any{"id": opsID, "session": "cc/lq-owner"})
	account, ok = lqEventAccount(t, r.db, opsID, core.RepoLockActionAcquired)
	require.True(t, ok)
	require.Empty(t, account, "the owner principal's events name no account")
}

// An alias is a caller's free text. A restricted reader sees none at all, and
// no reader sees the aliases of a lock a restricted account declared; the
// owner's own locks keep theirs for the owner. The store keeps every alias (the
// console shows them to the owner).
func TestLocksReach_AliasesShowOnlyToTheOwnerOnTheOwnersLocks(t *testing.T) {
	ctx := context.Background()
	r := lkNewRig(t)
	owned := lqLock(t, callJSON(t, ctx, r.owner, "locks_declare", map[string]any{"repo_root": "/work/open",
		"repo_origin": "https://example.com/team/open.git"})["lock"])
	require.NotEmpty(t, owned["aliases"])

	seen := lqLock(t, callJSON(t, ctx, r.agent, "locks_list", map[string]any{})["locks"].([]any)[0])
	require.Equal(t, owned["id"], seen["id"])
	require.Equal(t, []any{}, seen["aliases"], "a restricted reader sees no alias")
	require.Equal(t, "open main", seen["label"], "the label stays")

	asked := callJSON(t, ctx, r.agent, "locks_declare", map[string]any{"repo_root": "/work/open", "name": "release",
		"repo_origin": "https://example.com/team/open.git"})
	require.Equal(t, []any{}, lqLock(t, asked["lock"])["aliases"])
	require.Equal(t, []any{}, asked["recorded_aliases"])
	reqID := lqLock(t, asked["lock"])["id"].(string)

	for _, cli := range []struct {
		name string
		list map[string]any
	}{{"owner", callJSON(t, ctx, r.owner, "locks_list", map[string]any{})}, {"no view", callJSON(t, ctx, r.none, "locks_list", map[string]any{})}} {
		for _, raw := range cli.list["locks"].([]any) {
			l := lqLock(t, raw)
			if l["id"] == reqID {
				require.Equal(t, []any{}, l["aliases"], "%s: a restricted account's declaration shows no alias", cli.name)
			} else {
				require.NotEmpty(t, l["aliases"], "%s: the owner's own lock keeps its aliases", cli.name)
			}
		}
	}
	stored, ok, err := store.RepoLockByID(ctx, r.db, reqID)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotEmpty(t, stored.Aliases, "the store keeps them, for the console")
}

// lqConfined is a server with a confidential project and an owner agent bound
// to a session there by its agent process, peer_auth off: the conversation may
// not write the global scope.
type lqConfined struct {
	db                    *sql.DB
	owner, confined, open lqClient
}

// lqClient is one connection and the session it is bound to.
type lqClient struct {
	cli     *mcpclient.Client
	session string
}

func lqNewConfined(t *testing.T) *lqConfined {
	t.Helper()
	ctx := context.Background()
	url, db := newServer(t)
	_, err := store.EnsureProject(ctx, db, "example-conf", "example-conf")
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, db, "example-conf", core.IsolationConfidential))
	conf := seedAgentAmbient(t, ctx, db, "cc/lq-conf", "example-conf", "/work/conf", "400.4")
	open := seedAgentAmbient(t, ctx, db, "cc/lq-open", "demo", "/work/demo", "500.5")
	return &lqConfined{db: db,
		owner:    lqClient{cli: dialClient(t, ctx, url, testKey)},
		confined: lqClient{cli: dialAgent(t, ctx, url, "400.4"), session: conf},
		open:     lqClient{cli: dialAgent(t, ctx, url, "500.5"), session: open},
	}
}

// lqAliasCount counts a lock's stored aliases.
func lqAliasCount(t *testing.T, db *sql.DB, id string) int {
	t.Helper()
	l, ok, err := store.RepoLockByID(context.Background(), db, id)
	require.NoError(t, err)
	require.True(t, ok)
	return len(l.Aliases)
}

// An owner conversation that may not write the global scope (bound to a
// confidential project) acquires, heartbeats and releases a lock -- a lock's
// state carries no content -- but declares none and records no alias: both are
// its own text, written where every caller reads it. The open conversation
// beside it is the negative control.
func TestLocksReach_AConfinedConversationHoldsButWritesNoText(t *testing.T) {
	ctx := context.Background()
	c := lqNewConfined(t)
	origin := "https://example.com/team/app.git"

	isErr, text := callErr(t, ctx, c.confined.cli, "locks_declare", map[string]any{"host": "example-laptop", "repo_root": "/work/app",
		"repo_origin": origin})
	require.True(t, isErr)
	require.Contains(t, text, "this session is bound to confidential project example-conf: writes outside it are disabled",
		"the fence's own refusal")
	n, _, err := store.CountRepoLocks(ctx, c.db)
	require.NoError(t, err)
	require.Zero(t, n, "nothing declared")

	declared := lqLock(t, callJSON(t, ctx, c.open.cli, "locks_declare", map[string]any{"host": "example-laptop",
		"repo_root": "/work/app", "repo_origin": origin})["lock"])
	id := declared["id"].(string)
	before := lqAliasCount(t, c.db, id)

	elsewhere := map[string]any{"host": "example-desktop", "repo_root": "/srv/app", "repo_origin": origin}
	got := callJSON(t, ctx, c.confined.cli, "locks_acquire", elsewhere)
	require.Equal(t, true, got["acquired"])
	lock := lqLock(t, got["lock"])
	require.Equal(t, true, lock["held_by_caller"])
	require.Equal(t, before, lqAliasCount(t, c.db, id), "a confined conversation records no alias")
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "example-conf", "no project's content in the answer")
	beat := callJSON(t, ctx, c.confined.cli, "locks_acquire", map[string]any{"id": id})
	require.Equal(t, true, beat["heartbeat"])
	rel := callJSON(t, ctx, c.confined.cli, "locks_release", map[string]any{"id": id})
	require.Equal(t, true, rel["released"])

	// The negative control: the open conversation's acquire from that same
	// new checkout records it.
	callJSON(t, ctx, c.open.cli, "locks_acquire", elsewhere)
	require.Greater(t, lqAliasCount(t, c.db, id), before, "an open conversation records the checkout")
}

// held_by_caller and caller_bound read the connection's own binding -- the
// agent-process identity, or a session_start -- never the sole-active-ambient
// fallback an actor may resolve through.
func TestLocksBinding_HeldByCallerAndCallerBound(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	sole := seedAgentAmbient(t, ctx, db, "cc/lq-sole", "demo", "/work/demo", "600.6")
	unbound := dialClient(t, ctx, url, testKey)
	bound := dialAgent(t, ctx, url, "600.6")
	id := lqLock(t, callJSON(t, ctx, unbound, "locks_declare", lkCheckout(nil))["lock"])["id"].(string)

	// The unbound connection acts as the one active ambient (resolveActor's
	// fallback), and still reads as bound to nothing.
	got := callJSON(t, ctx, unbound, "locks_acquire", map[string]any{"id": id})
	require.Equal(t, sole, lqLock(t, got["lock"])["holder"])
	require.Equal(t, false, lqLock(t, got["lock"])["held_by_caller"], "the fallback is no binding")
	list := callJSON(t, ctx, unbound, "locks_list", map[string]any{})
	require.Equal(t, false, list["caller_bound"])
	require.Equal(t, false, lqLock(t, list["locks"].([]any)[0])["held_by_caller"])

	// The agent's own connection is bound to that session by its process.
	list = callJSON(t, ctx, bound, "locks_list", map[string]any{})
	require.Equal(t, true, list["caller_bound"])
	l := lqLock(t, list["locks"].([]any)[0])
	require.Equal(t, true, l["held_by_caller"])
	require.EqualValues(t, 0, l["waiting"])
	require.Nil(t, l["next_waiter_session"])

	// A session_start binds a connection too.
	other := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, other, "session_start", map[string]any{"name": "lq-explicit", "project": "demo"})
	list = callJSON(t, ctx, other, "locks_list", map[string]any{})
	require.Equal(t, true, list["caller_bound"])
	require.Equal(t, false, lqLock(t, list["locks"].([]any)[0])["held_by_caller"], "bound, but not the holder")

	// Released, nobody holds it.
	callJSON(t, ctx, bound, "locks_release", map[string]any{"id": id})
	require.Equal(t, false, lqLock(t, callJSON(t, ctx, bound, "locks_list", map[string]any{})["locks"].([]any)[0])["held_by_caller"])
}
