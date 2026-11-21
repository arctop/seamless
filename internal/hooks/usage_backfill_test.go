package hooks

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
)

// bfDay is when the backfill fixtures' sessions ran.
var bfDay = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

// bfSession records one conversation's ambient row as its hooks would have:
// on host, in project, ended at bfDay plus an hour unless status is active,
// with the transcript path its SessionStart recorded ("" for none) and the
// account that started it ("" for the owner).
func bfSession(t *testing.T, db *sql.DB, id, client, ext, project, host string, status core.SessionStatus, path, account string) core.Session {
	t.Helper()
	ctx := context.Background()
	s := core.Session{ID: id, Name: "cc/" + ext, ProjectSlug: project, Status: status, Ambient: true,
		ExternalClient: client, ExternalSessionID: ext, Host: host, Account: account,
		CreatedAt: bfDay, UpdatedAt: bfDay.Add(time.Hour)}
	require.NoError(t, store.CreateSession(ctx, db, s))
	require.NoError(t, store.SetSessionTranscriptPath(ctx, db, id, path))
	got, ok, err := store.SessionByID(ctx, db, id)
	require.NoError(t, err)
	require.True(t, ok)
	return got
}

// bfRows is a transcript's usage over two quarter-hours of bfDay.
func bfRows() []core.UsageBucket {
	return []core.UsageBucket{
		{Bucket: bfDay, Model: "claude-x", Thread: core.UsageThreadMain, Input: 100, Cached: 1000, Output: 50, Messages: 2, MinuteMask: 0b11},
		{Bucket: bfDay.Add(15 * time.Minute), Model: "claude-small", Thread: core.UsageThreadSubagent, Input: 7, Output: 3,
			Messages: 1, MinuteMask: 1 << 4},
	}
}

// bfPush is the capture a fill carries: rows and the totals they sum to.
func bfPush(rows []core.UsageBucket) map[string]any {
	tokens := core.SumUsage(rows)
	return map[string]any{"version": harvest.PushVersion, "tokens": tokens, "usage": rows}
}

// bfDo sends one backfill request to base with key ("" for none): the
// status and the raw answer.
func bfDo(t *testing.T, method, base, key, query string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+harvest.UsageBackfillEndpoint+"?"+query, reader)
	require.NoError(t, err)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

// bfList is the listing for host.
func bfList(t *testing.T, base, key, host string) (int, harvest.BackfillList, []byte) {
	t.Helper()
	q := url.Values{}
	if host != "" {
		q.Set("host", host)
	}
	status, raw := bfDo(t, http.MethodGet, base, key, q.Encode(), nil)
	var list harvest.BackfillList
	if status == http.StatusOK {
		require.NoError(t, json.Unmarshal(raw, &list))
	}
	return status, list, raw
}

// bfFill is one session's fill, as `seam usage backfill` sends it.
func bfFill(t *testing.T, base, key, client, host, ext string, capture map[string]any, dryRun bool) (int, harvest.BackfillResult) {
	t.Helper()
	q := url.Values{"host": {host}}
	if client != "" {
		q.Set("client", client)
	}
	body := map[string]any{"session_id": ext, "dry_run": dryRun}
	if capture != nil {
		body[harvest.Field] = capture
	}
	status, raw := bfDo(t, http.MethodPost, base, key, q.Encode(), body)
	var res harvest.BackfillResult
	require.NoError(t, json.Unmarshal(raw, &res), string(raw))
	return status, res
}

// bfIDs is the listed sessions' ids.
func bfIDs(list harvest.BackfillList) []string {
	out := []string{}
	for _, s := range list.Sessions {
		out = append(out, s.ID)
	}
	return out
}

// The listing is the caller's conversations recorded on the computer the
// request speaks for, oldest first, each with what a fill needs: the agent
// client's session id, the transcript its hooks recorded, its totals now.
// Explicit sessions are no conversation of their own, another computer's are
// only counted, and a session another principal started is not there at all.
func TestUsageBackfill_ListsThisComputersOwnConversations(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})
	db := e.db
	ended := bfSession(t, db, "01BFLIST000000000000000001", "claude-code", "bf-ended", "demo", localHostName, core.SessionCompleted,
		"/home/owner/.claude/projects/-work-demo/bf-ended.jsonl", "")
	live := bfSession(t, db, "01BFLIST000000000000000002", "claude-code", "bf-live", "demo", localHostName, core.SessionActive, "", "")
	codex := bfSession(t, db, "01BFLIST000000000000000003", "codex", "bf-codex", "demo", localHostName, core.SessionExpired,
		"/home/owner/.codex/sessions/2026/11/18/rollout-bf-codex.jsonl", "")
	remote := bfSession(t, db, "01BFLIST000000000000000004", "claude-code", "bf-remote", "demo", remoteHostName, core.SessionCompleted, "", "")
	bfSession(t, db, "01BFLIST000000000000000005", "claude-code", "bf-agent", "demo", localHostName, core.SessionCompleted,
		"/home/agent/.claude/projects/-work-demo/bf-agent.jsonl", "example-agent")
	require.NoError(t, store.CreateSession(context.Background(), db, core.Session{ID: "01BFLIST000000000000000006",
		Name: "explicit-bf", ProjectSlug: "demo", Status: core.SessionCompleted, ExternalClient: "claude-code",
		ExternalSessionID: "bf-ended", Host: localHostName, CreatedAt: bfDay, UpdatedAt: bfDay}))

	status, list, raw := bfList(t, e.ts.URL, testKey, localHostName)
	require.Equal(t, http.StatusOK, status, string(raw))
	require.Equal(t, localHostName, list.Host)
	require.Equal(t, []string{ended.ID, live.ID, codex.ID}, bfIDs(list))
	require.Equal(t, 1, list.OtherHosts)
	require.Empty(t, list.Locked)
	require.Equal(t, harvest.BackfillSession{ID: ended.ID, Name: ended.Name, Project: "demo", Client: "claude-code",
		SessionID: "bf-ended", Status: "completed", TranscriptPath: "/home/owner/.claude/projects/-work-demo/bf-ended.jsonl",
		Created: bfDay}, list.Sessions[0])
	require.NotContains(t, string(raw), "bf-agent", "another principal's session is not listed, nor counted")
	require.NotContains(t, string(raw), "/home/agent")

	_, unnamed, _ := bfList(t, e.ts.URL, testKey, "")
	require.Equal(t, list, unnamed, "a request that names no computer speaks for the daemon's own")
	_, beta, _ := bfList(t, e.ts.URL, testKey, remoteHostName)
	require.Equal(t, remoteHostName, beta.Host)
	require.Equal(t, []string{remote.ID}, bfIDs(beta))
	require.Equal(t, 3, beta.OtherHosts)

	status, _, _ = bfList(t, e.ts.URL, "wrong-key", localHostName)
	require.Equal(t, http.StatusUnauthorized, status)
}

// A fill writes the transcript's rows onto an ended session without moving it:
// it stays ended, at the time it ended. A dry run writes nothing and says what
// a run would, and a second run of the same rows writes nothing either. A
// grown transcript's rows replace the earlier ones.
func TestUsageBackfill_FillsAnEndedSessionOnce(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})
	ctx := context.Background()
	sess := bfSession(t, e.db, "01BFFILL000000000000000001", "claude-code", "bf-fill", "demo", localHostName, core.SessionCompleted, "", "")
	rows := bfRows()
	want := core.SumUsage(rows)

	status, res := bfFill(t, e.ts.URL, testKey, "", localHostName, "bf-fill", bfPush(rows), true)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, harvest.BackfillResult{Result: harvest.BackfillWouldWrite, After: want}, res)
	require.Empty(t, sessionLedger(t, e.db, sess.ID), "a dry run writes nothing")

	status, res = bfFill(t, e.ts.URL, testKey, "claude-code", localHostName, "bf-fill", bfPush(rows), false)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, harvest.BackfillResult{Result: harvest.BackfillWritten, After: want}, res)
	require.Equal(t, rows, sessionLedger(t, e.db, sess.ID))
	got, _, err := store.SessionByID(ctx, e.db, sess.ID)
	require.NoError(t, err)
	require.Equal(t, want, got.Tokens, "the totals are the ledger's sum")
	require.Equal(t, core.SessionCompleted, got.Status, "never revived")
	require.True(t, sess.UpdatedAt.Equal(got.UpdatedAt), "the time it ended does not move")

	for _, dry := range []bool{true, false} {
		status, res = bfFill(t, e.ts.URL, testKey, "", localHostName, "bf-fill", bfPush(rows), dry)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, harvest.BackfillResult{Result: harvest.BackfillUnchanged, Before: want, After: want}, res,
			"a second run writes nothing (dry run %v)", dry)
	}

	grown := append(bfRows(), core.UsageBucket{Bucket: bfDay.Add(30 * time.Minute), Model: "claude-x",
		Thread: core.UsageThreadMain, Input: 1, Output: 1, Messages: 1, MinuteMask: 1})
	status, res = bfFill(t, e.ts.URL, testKey, "", localHostName, "bf-fill", bfPush(grown), false)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, harvest.BackfillResult{Result: harvest.BackfillWritten, Before: want, After: core.SumUsage(grown)}, res)
	require.Equal(t, grown, sessionLedger(t, e.db, sess.ID))
}

// A live session is its own end's to harvest: a fill writes nothing into it
// and does not heartbeat it.
func TestUsageBackfill_WritesNothingIntoALiveSession(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})
	sess := bfSession(t, e.db, "01BFLIVE000000000000000001", "claude-code", "bf-live", "demo", localHostName, core.SessionActive, "", "")
	for _, dry := range []bool{true, false} {
		status, res := bfFill(t, e.ts.URL, testKey, "", localHostName, "bf-live", bfPush(bfRows()), dry)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, harvest.BackfillActive, res.Result)
	}
	got := requireAmbientSession(t, e.db, ClientClaudeCode, "bf-live")
	require.Empty(t, sessionLedger(t, e.db, sess.ID))
	require.True(t, got.Tokens.Empty())
	require.True(t, sess.UpdatedAt.Equal(got.UpdatedAt), "no heartbeat")
}

// What a fill refuses, and writes nothing for: another computer's session
// (its transcript is there), a Codex session, a body that carries no rows or
// rows that do not sum to their totals, a session nobody recorded, and a
// request that is not one.
func TestUsageBackfill_RefusesWhatItCannotFill(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})
	remote := bfSession(t, e.db, "01BFREFUSE0000000000000001", "claude-code", "bf-remote", "demo", remoteHostName, core.SessionCompleted, "", "")
	codex := bfSession(t, e.db, "01BFREFUSE0000000000000002", "codex", "bf-codex", "demo", localHostName, core.SessionExpired, "", "")
	ours := bfSession(t, e.db, "01BFREFUSE0000000000000003", "claude-code", "bf-ours", "demo", localHostName, core.SessionCompleted, "", "")

	status, res := bfFill(t, e.ts.URL, testKey, "", localHostName, "bf-remote", bfPush(bfRows()), false)
	require.Equal(t, http.StatusConflict, status)
	require.Contains(t, res.Error, "recorded on "+remoteHostName)
	status, res = bfFill(t, e.ts.URL, testKey, "codex", localHostName, "bf-codex", bfPush(bfRows()), false)
	require.Equal(t, http.StatusBadRequest, status)
	require.Contains(t, res.Error, "running total")

	short := bfPush(bfRows())
	short["tokens"] = core.TokenUsage{Input: 1, Total: 1}
	for name, capture := range map[string]map[string]any{
		"no capture":           nil,
		"no rows":              {"version": harvest.PushVersion, "tokens": core.SumUsage(bfRows())},
		"rows past the totals": short,
		"another version":      {"version": 99, "tokens": core.SumUsage(bfRows()), "usage": bfRows()},
	} {
		status, res = bfFill(t, e.ts.URL, testKey, "", localHostName, "bf-ours", capture, false)
		require.Equal(t, http.StatusBadRequest, status, name)
		require.NotEmpty(t, res.Error, name)
	}
	status, res = bfFill(t, e.ts.URL, testKey, "", localHostName, "bf-nobody", bfPush(bfRows()), false)
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, "no session of this client's session id is this caller's to fill", res.Error)
	status, _ = bfFill(t, e.ts.URL, testKey, "", localHostName, "", bfPush(bfRows()), false)
	require.Equal(t, http.StatusBadRequest, status)
	status, raw := bfDo(t, http.MethodPost, e.ts.URL, testKey, "client=claude-code", "not an object")
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	status, raw = bfDo(t, http.MethodPost, e.ts.URL, testKey, "client=bogus", map[string]any{"session_id": "bf-ours"})
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	status, _ = bfDo(t, http.MethodPost, e.ts.URL, "wrong-key", "", map[string]any{"session_id": "bf-ours"})
	require.Equal(t, http.StatusUnauthorized, status)

	for _, s := range []core.Session{remote, codex, ours} {
		require.Empty(t, sessionLedger(t, e.db, s.ID), s.ExternalSessionID)
	}
}

// Under peer_auth the backfill is the owner's alone. A restricted local
// account is refused at both halves with one text, which names no session
// and no path -- its own sessions included -- and nothing of anyone's is
// written. The owner's listing, on the socket or with the key over TCP, holds
// only the owner's own sessions in projects it reads: a locked project is
// named, never counted, and a restricted account's session is not there. A
// fill lands where the SessionEnd push would: the owner's open-project
// session takes it, the locked project's session and the account's session
// read as not there.
func TestUsageBackfill_UnderPeerAuthIsTheOwners(t *testing.T) {
	e := newRestrictedHooks(t)
	db := e.db
	open := bfSession(t, db, "01BFPEER000000000000000001", "claude-code", "bf-open", lkOps, localHostName, core.SessionCompleted,
		"/home/owner/.claude/projects/-work-ops/bf-open.jsonl", "")
	locked := bfSession(t, db, "01BFPEER000000000000000002", "claude-code", "bf-locked", lkFin, localHostName, core.SessionCompleted,
		"/home/owner/.claude/projects/-work-finance/bf-locked.jsonl", "")
	agents := bfSession(t, db, "01BFPEER000000000000000003", "claude-code", "bf-agent", lkOps, localHostName, core.SessionCompleted,
		"/home/agent/.claude/projects/-work-ops/bf-agent.jsonl", rAcct)
	sessions := []core.Session{open, locked, agents}

	// The restricted account: refused before anything is read, both halves,
	// whatever it names -- its own session too.
	e.asRestricted()
	status, _, raw := bfList(t, e.url, "", localHostName)
	require.Equal(t, http.StatusForbidden, status)
	var refused harvest.BackfillResult
	require.NoError(t, json.Unmarshal(raw, &refused))
	require.Equal(t, usageBackfillOwnerRefusal, refused.Error)
	for _, s := range sessions {
		require.NotContains(t, string(raw), s.ID)
		require.NotContains(t, string(raw), s.TranscriptPath)
	}
	for _, ext := range []string{"bf-agent", rExt("bf-agent"), "bf-open", "bf-locked", "bf-nobody"} {
		status, res := bfFill(t, e.url, "", "", localHostName, ext, bfPush(bfRows()), false)
		require.Equal(t, http.StatusForbidden, status, ext)
		require.Equal(t, harvest.BackfillResult{Error: usageBackfillOwnerRefusal}, res, "%s: one answer for every name", ext)
	}
	for _, s := range sessions {
		require.Empty(t, sessionLedger(t, db, s.ID), "%s: nothing written", s.ExternalSessionID)
	}

	// The owner on the socket, then with the key over TCP.
	for _, owner := range []struct {
		name string
		key  string
		view func()
	}{
		{"socket", "", e.asOwner},
		{"tcp", testKey, func() {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.view = func() (access.View, bool) { return e.policy.OwnerView(), true }
		}},
	} {
		t.Run(owner.name, func(t *testing.T) {
			owner.view()
			status, list, raw := bfList(t, e.url, owner.key, localHostName)
			require.Equal(t, http.StatusOK, status, string(raw))
			require.Equal(t, []string{open.ID}, bfIDs(list))
			require.Equal(t, []string{lkFin}, list.Locked, "the locked project is named")
			require.Zero(t, list.OtherHosts)
			for _, s := range []core.Session{locked, agents} {
				require.NotContains(t, string(raw), s.ID)
				require.NotContains(t, string(raw), s.TranscriptPath)
			}

			status, res := bfFill(t, e.url, owner.key, "", localHostName, "bf-open", bfPush(bfRows()), false)
			require.Equal(t, http.StatusOK, status)
			require.Contains(t, []string{harvest.BackfillWritten, harvest.BackfillUnchanged}, res.Result)
			require.Equal(t, bfRows(), sessionLedger(t, db, open.ID))
			for _, ext := range []string{"bf-locked", "bf-agent", rExt("bf-agent")} {
				status, res := bfFill(t, e.url, owner.key, "", localHostName, ext, bfPush(bfRows()), false)
				require.Equal(t, http.StatusNotFound, status, ext)
				require.Equal(t, "no session of this client's session id is this caller's to fill", res.Error, ext)
			}
			require.Empty(t, sessionLedger(t, db, locked.ID), "the locked project's session takes nothing")
			require.Empty(t, sessionLedger(t, db, agents.ID), "the account's session takes nothing")
		})
	}
}
