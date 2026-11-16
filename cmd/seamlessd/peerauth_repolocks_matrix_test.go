package main

// The repo locks' rows of the peer_auth matrix (plan repo-locks, step RL3):
// who may see and hold a lock, on the real route table over real services.
//
//   - The owner principal holds every lock from any conversation: one bound to
//     a confidential project, one in a project locked to it, and one confined
//     by isolation.requests acquire, heartbeat and release, read no other
//     project's content, and write no text into the registry (no declaration,
//     no alias). A lock's state carries no content (owner decision D2).
//   - A restricted local account holds a lock one of whose listed projects it
//     may write; outside that reach the lock reads exactly like a missing one,
//     by list, by id, by checkout and through a declaration. Its declaration
//     waits for the owner.
//   - A relay's restricted account is judged the same way, with the open-only
//     reach decision B-D11 keeps it to.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// The repo-lock rows' processes, in the lease matrix's boot: lmPrograms names
// a pid at or above 1000 claude when it is a multiple of 20 and seam
// otherwise.
var (
	lkAgentK  = lmTok(5000) // the owner's agent in the confidential project
	lkBridgeK = lmTok(5010)
	lkHookK   = lmTok(5011)
	lkAgentL  = lmTok(5040) // the owner's agent in a project locked to it, no lease
	lkBridgeL = lmTok(5050)
	lkHookL   = lmTok(5051)
	lkAgentR  = lmTok(5080) // the restricted account's agent
	lkBridgeR = lmTok(5090)
	lkHookR   = lmTok(5091)
)

// lkGhostID is a lock id no lock has.
const lkGhostID = "01JZZZZZZZZZZZZZZZZZZZZZZZ"

// lkLockOf is the lock object of an answer.
func lkLockOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	l, ok := out["lock"].(map[string]any)
	require.True(t, ok, "%v", out)
	return l
}

// lkLocksOf is the lock objects of a locks_list answer, by id.
func lkLocksOf(t *testing.T, out map[string]any) map[string]map[string]any {
	t.Helper()
	raw, ok := out["locks"].([]any)
	require.True(t, ok, "%v", out)
	got := map[string]map[string]any{}
	for _, r := range raw {
		l := r.(map[string]any)
		got[l["id"].(string)] = l
	}
	return got
}

// lkNoSecret fails when any text holds one of the secrets.
func lkNoSecret(t *testing.T, text, what string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		require.NotContains(t, text, s, "%s: a secret in a lock answer", what)
	}
}

func TestPeerAuthMatrix_RepoLocks(t *testing.T) {
	ctx := context.Background()
	d := newLMDaemon(t)
	owner := paClient(t, d.owner.URL, lmKey)
	// A secret in the confidential project; no lock answer may carry it.
	lmCallOK(t, owner, "memory_write", map[string]any{"project": lmConf, "name": "conf-ledger", "kind": "gotcha",
		"description": "conf " + lmConfSecret, "body": "conf body " + lmConfSecret})

	docsOrigin, confOrigin := "https://example.com/team/docs.git", "https://example.com/team/conf.git"
	docs := lkLockOf(t, lmCallOK(t, owner, "locks_declare", map[string]any{"repo_root": "/work/docs", "repo_origin": docsOrigin}))
	conf := lkLockOf(t, lmCallOK(t, owner, "locks_declare", map[string]any{"repo_root": "/work/conf", "repo_origin": confOrigin}))
	require.Equal(t, []any{lmDocs}, docs["projects"])
	require.Equal(t, []any{lmConf}, conf["projects"])
	docsID, confID := docs["id"].(string), conf["id"].(string)
	eventAccount := func(id, action string) string {
		t.Helper()
		var account *string
		require.NoError(t, d.db.QueryRowContext(ctx, `SELECT json_extract(payload, '$.account') FROM events WHERE kind = ?
			AND item_id = ? AND json_extract(payload, '$.action') = ? ORDER BY ts DESC, id DESC LIMIT 1`,
			string(core.EventRepoLockChanged), id, action).Scan(&account))
		if account == nil {
			return ""
		}
		return *account
	}
	aliasCount := func(id string) int {
		t.Helper()
		l, ok, err := store.RepoLockByID(ctx, d.db, id)
		require.NoError(t, err)
		require.True(t, ok)
		return len(l.Aliases)
	}
	requests := func() int { return lmCount(t, d.db, `SELECT COUNT(*) FROM access_approvals`) }

	t.Run("an owner conversation bound to a confidential project holds a lock and writes no text", func(t *testing.T) {
		out := d.hook(t, d.peer(t, lmUIDOwner, lkHookK, lkAgentK, lmTerm).URL, "", "session-start", &lkAgentK,
			map[string]any{"session_id": "claude-lk-k", "cwd": "/work/conf", "source": "startup"})
		require.NotContains(t, out, "is locked")
		k := paClientHeaders(t, d.peer(t, lmUIDOwner, lkBridgeK, lkAgentK, lmTerm).URL, "", lmAgentHeaders(lkAgentK))

		list := lmCallOK(t, k, "locks_list", map[string]any{})
		require.Equal(t, true, list["caller_bound"])
		require.Len(t, lkLocksOf(t, list), 2, "the owner principal sees every lock")

		text, refused := paCall(t, k, "locks_declare", map[string]any{"repo_root": "/work/open", "name": "lk-k",
			"repo_origin": "https://example.com/team/open.git"})
		require.True(t, refused)
		require.Contains(t, text, "this session is bound to confidential project "+lmConf+": writes outside it are disabled")

		before := aliasCount(docsID)
		elsewhere := map[string]any{"host": "example-desktop", "repo_root": "/srv/docs", "repo_origin": docsOrigin}
		for _, step := range []struct {
			tool string
			args map[string]any
			key  string
		}{
			{"locks_acquire", elsewhere, "acquired"}, {"locks_acquire", map[string]any{"id": docsID}, "heartbeat"},
			{"locks_release", map[string]any{"id": docsID}, "released"},
		} {
			text, refused := paCall(t, k, step.tool, step.args)
			require.False(t, refused, "%s: %s", step.tool, text)
			lkNoSecret(t, text, step.tool, lmConfSecret, lmFinSecret, lmLegalSecret)
			var got map[string]any
			require.NoError(t, json.Unmarshal([]byte(text), &got))
			require.Equal(t, true, got[step.key], "%s %s", step.tool, text)
		}
		require.Equal(t, before, aliasCount(docsID), "a confined conversation records no alias")
		require.Empty(t, eventAccount(docsID, core.RepoLockActionAcquired), "the owner principal's events name no account")
	})

	t.Run("an owner conversation in a project locked to it holds a lock, files nothing, and is bound", func(t *testing.T) {
		out := d.hook(t, d.peer(t, lmUIDOwner, lkHookL, lkAgentL, lmTerm).URL, "", "session-start", &lkAgentL,
			map[string]any{"session_id": "claude-lk-l", "cwd": "/work/legal", "source": "startup"})
		require.Contains(t, out, "project "+lmLegal+" is locked")
		filed := requests()
		l := paClientHeaders(t, d.peer(t, lmUIDOwner, lkBridgeL, lkAgentL, lmTerm).URL, "", lmAgentHeaders(lkAgentL))

		list := lmCallOK(t, l, "locks_list", map[string]any{})
		require.Equal(t, true, list["caller_bound"], "a locked binding is a binding")
		got := lmCallOK(t, l, "locks_acquire", map[string]any{"id": docsID})
		require.Equal(t, true, got["acquired"])
		require.Equal(t, true, lkLockOf(t, got)["held_by_caller"])
		require.Nil(t, lkLockOf(t, got)["holder_session"], "its own locked session is not named, even to it")
		require.Equal(t, true, lmCallOK(t, l, "locks_acquire", map[string]any{"id": docsID})["heartbeat"])
		require.Equal(t, true, lmCallOK(t, l, "locks_release", map[string]any{"id": docsID})["released"])
		require.Equal(t, filed, requests(), "holding a lock asks the owner for nothing")

		// A declaration is still the lock's refusal.
		text, refused := paCall(t, l, "locks_declare", map[string]any{"repo_root": "/work/open", "name": "lk-l"})
		require.True(t, refused)
		require.Contains(t, text, lmLegal)
		lkNoSecret(t, text, "the declaration's refusal", lmLegalSecret, lmFinSecret, lmConfSecret)
	})

	t.Run("a restricted account holds a lock within its reach, and outside it the lock is missing", func(t *testing.T) {
		out := d.hook(t, d.peer(t, lmUIDAgent, lkHookR, lkAgentR, lmTerm).URL, "", "session-start", &lkAgentR,
			map[string]any{"session_id": "claude-lk-r", "cwd": "/work/open", "source": "startup"})
		require.NotContains(t, out, "maps to no project")
		r := paClientHeaders(t, d.peer(t, lmUIDAgent, lkBridgeR, lkAgentR, lmTerm).URL, "", lmAgentHeaders(lkAgentR))

		seen := lkLocksOf(t, lmCallOK(t, r, "locks_list", map[string]any{}))
		require.Len(t, seen, 1, "only the lock listing a project it may write")
		require.Contains(t, seen, docsID)
		require.Equal(t, []any{}, seen[docsID]["aliases"], "a restricted reader sees no alias")

		for _, tool := range []string{"locks_acquire", "locks_release"} {
			real, refused := paCall(t, r, tool, map[string]any{"id": confID})
			require.True(t, refused)
			ghost, _ := paCall(t, r, tool, map[string]any{"id": lkGhostID})
			require.Equal(t, strings.ReplaceAll(ghost, lkGhostID, confID), real, "%s by id", tool)
			byCheckout, _ := paCall(t, r, tool, map[string]any{"repo_root": "/work/conf", "repo_origin": confOrigin})
			missing, _ := paCall(t, r, tool, map[string]any{"repo_root": "/work/conf", "repo_origin": confOrigin, "name": "lk-none"})
			require.Equal(t, strings.ReplaceAll(missing, "named lk-none", "named main"), byCheckout, "%s by checkout", tool)
		}
		resolved, refused := paCall(t, r, "locks_declare", map[string]any{"repo_root": "/work/open", "repo_origin": confOrigin})
		require.True(t, refused)
		noProject, _ := paCall(t, r, "locks_declare", map[string]any{"repo_root": "/work/nowhere"})
		require.Equal(t, noProject, resolved, "a declaration resolving to it confirms nothing")
		require.NotContains(t, resolved, confID)

		got := lmCallOK(t, r, "locks_acquire", map[string]any{"id": docsID})
		require.Equal(t, true, got["acquired"])
		require.Equal(t, lmAccount, eventAccount(docsID, core.RepoLockActionAcquired), "the event names the account")
		// The owner sees the hold, and not the restricted account's session.
		ownerView := lkLocksOf(t, lmCallOK(t, owner, "locks_list", map[string]any{}))[docsID]
		require.Equal(t, true, ownerView["held"])
		require.Nil(t, ownerView["holder_session"])
		lmCallOK(t, r, "locks_release", map[string]any{"id": docsID})
		require.Equal(t, lmAccount, eventAccount(docsID, core.RepoLockActionReleased))
	})

	t.Run("a restricted account's declaration waits for the owner", func(t *testing.T) {
		r := paClientHeaders(t, d.peer(t, lmUIDAgent, lkBridgeR, lkAgentR, lmTerm).URL, "", lmAgentHeaders(lkAgentR))
		asked := lmCallOK(t, r, "locks_declare", map[string]any{"repo_root": "/work/open", "name": "release",
			"repo_origin": "https://example.com/team/open.git"})
		l := lkLockOf(t, asked)
		require.Equal(t, "requested", l["state"])
		require.Equal(t, []any{lmOpen}, l["projects"])
		id := l["id"].(string)
		text, refused := paCall(t, r, "locks_acquire", map[string]any{"id": id, "wait": true})
		require.True(t, refused)
		require.Contains(t, text, "has not approved yet")
		require.Zero(t, lmCount(t, d.db, `SELECT COUNT(*) FROM repo_lock_waiters WHERE lock_id = ?`, id), "nobody queues for it")

		// The owner approves it on the console (the bearer key over TCP).
		req, err := http.NewRequest(http.MethodPost, d.owner.URL+"/console/locks/"+id+"/approve?format=json", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+lmKey)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, true, lmCallOK(t, r, "locks_acquire", map[string]any{"id": id})["acquired"])
	})
}

// isolation.requests: a conversation homed in a confidential project, granted
// a read of another, holds a lock -- before the grant and after it -- and
// never writes the registry's text, nor reads the granted project's content
// through a lock answer.
func TestPeerAuthMatrix_RepoLocksConfinedConversation(t *testing.T) {
	r := newGMRig(t, true, true)
	r.plantAll()
	conf := r.items[gmConf]
	r.stamp(gmHome, "cc/lk-home-agent", gmAgent)
	agent := r.dial(r.socket(gmBridge, gmAgent).URL, "", gmAgent)
	owner := r.dial(r.tcp.URL, gmKey, access.ProcessToken{})
	origin := "https://example.com/team/docs.git"
	id := lkLockOf(t, gmOK(t, owner, "locks_declare", map[string]any{"repo_root": "/work/" + gmDocs, "repo_origin": origin}))["id"].(string)
	aliases := func() int {
		l, ok, err := store.RepoLockByID(r.ctx, r.db, id)
		require.NoError(t, err)
		require.True(t, ok)
		return len(l.Aliases)
	}

	holds := func(t *testing.T, when string) {
		t.Helper()
		before := aliases()
		items, refused := gmCall(t, agent, "locks_declare", map[string]any{"repo_root": "/work/" + gmOpen, "name": "lk-" + when})
		require.True(t, refused, "%s: %v", when, items)
		for _, step := range []struct {
			tool string
			args map[string]any
			key  string
		}{
			{"locks_acquire", map[string]any{"host": "example-desktop", "repo_root": "/srv/docs", "repo_origin": origin}, "acquired"},
			{"locks_acquire", map[string]any{"id": id}, "heartbeat"},
			{"locks_list", map[string]any{}, "caller_bound"},
			{"locks_release", map[string]any{"id": id}, "released"},
		} {
			items, refused := gmCall(t, agent, step.tool, step.args)
			require.False(t, refused, "%s %s: %v", when, step.tool, items)
			text := gmText(items)
			gmNoSecretsBut(t, text, when+" "+step.tool)
			var got map[string]any
			require.NoError(t, json.Unmarshal([]byte(items[0]), &got))
			require.Equal(t, true, got[step.key], "%s %s: %s", when, step.tool, text)
		}
		require.Equal(t, before, aliases(), "%s: a confined conversation records no alias", when)
	}
	t.Run("homed in a confidential project", func(t *testing.T) { holds(t, "home") })

	t.Run("granted a read of another confidential project", func(t *testing.T) {
		items, refused := gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
		require.True(t, refused)
		short := gmShort.FindString(gmText(items))
		require.NotEmpty(t, short, "%v", items)
		r.at(t).approveDaemon(short)
		items, refused = gmCall(t, agent, "notes_read", map[string]any{"id": conf.noteID})
		require.False(t, refused, "%v", items)
		require.Contains(t, gmText(items), conf.secret, "the grant reads it")
		holds(t, "granted")
		items, _ = gmCall(t, agent, "locks_declare", map[string]any{"repo_root": "/work/" + gmOpen, "name": "lk-after"})
		require.Contains(t, gmText(items), "holds content from", "the clearance rule's own refusal")
	})
}
