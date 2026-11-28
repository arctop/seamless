package hooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
)

// Owed usage at the hooks (usage_owed.go): under peer_auth the caller's next
// Claude Code start or end asks for a conversation's usage that ended without
// its SessionEnd harvest, and `seam hook-usage --owed` answers.

// owedEnd ends conversation id as the session_end tool does: the row
// completed, its usage owed.
func owedEnd(t *testing.T, e *lockHooks, id string) core.Session {
	t.Helper()
	ctx := context.Background()
	sess := requireAmbientSession(t, e.db, ClientClaudeCode, id)
	sess.Status, sess.Findings, sess.UpdatedAt = core.SessionCompleted, "done", e.clock()
	require.NoError(t, store.UpdateSession(ctx, e.db, sess))
	marked, err := store.MarkUsageOwed(ctx, e.db, sess.ID, e.clock())
	require.NoError(t, err)
	require.True(t, marked)
	return sess
}

// owedStart starts conversation id in ops over the socket, naming transcript,
// and returns the owed asks its answer carries, each parsed.
func owedStart(t *testing.T, e *lockHooks, id, transcript string) []url.Values {
	t.Helper()
	resp, _ := ledgerLiveHook(e, "session-start", "", map[string]any{"session_id": id, "cwd": "/work/ops",
		"source": "startup", "transcript_path": transcript})
	var out []url.Values
	for _, raw := range resp.Header.Values(harvest.UsageOwedHeader) {
		q, err := url.ParseQuery(raw)
		require.NoError(t, err)
		out = append(out, q)
	}
	return out
}

// owedPushBody posts an owed answer for conversation id, as `seam hook-usage
// --owed` does: extra's fields beside the owed mark, and rows when non-nil.
func owedPushBody(e *lockHooks, id string, rows []core.UsageBucket, extra map[string]any) (int, harvest.UsagePushAnswer) {
	e.t.Helper()
	body := map[string]any{"session_id": id, harvest.UsageOwedField: true}
	for k, v := range extra {
		body[k] = v
	}
	if rows != nil {
		tokens := core.SumUsage(rows)
		body[harvest.Field] = harvest.Push{Version: harvest.PushVersion, Tokens: &tokens, Usage: rows}
	}
	resp, out := ledgerLivePost(e.t, e.url+harvest.UsageEndpoint+"?"+url.Values{"host": {localHostName}}.Encode(), body)
	var a harvest.UsagePushAnswer
	require.NoError(e.t, json.Unmarshal(out, &a), "%s", out)
	return resp.StatusCode, a
}

// owedPushRows is owedPushBody for the rows, the transcript last changed at
// changed.
func owedPushRows(e *lockHooks, id string, rows []core.UsageBucket, changed time.Time) (int, harvest.UsagePushAnswer) {
	e.t.Helper()
	return owedPushBody(e, id, rows, map[string]any{harvest.UsageChangedField: changed.Format(time.RFC3339Nano)})
}

func owedRow(t *testing.T, e *lockHooks, id string) (store.OwedUsage, bool) {
	t.Helper()
	o, found, err := store.OwedUsageOf(context.Background(), e.db, string(ClientClaudeCode), id)
	require.NoError(t, err)
	return o, found
}

// The Diary off, on the socket: the next start of the same caller asks for
// the conversation the agent ended, the push lands on the ended row, and the
// asks wait after it. An answer that brought no change leaves the asks
// counting; the conversation's late SessionEnd settles it.
func TestOwedUsage_TheNextStartAsksAndThePushLands(t *testing.T) {
	e := newLockHooks(t, true)
	path := writeTranscript(t, ledgerLiveTurn1)
	require.Empty(t, owedStart(t, e, "owed-1", path), "nothing owed yet")
	sess := owedEnd(t, e, "owed-1")

	asks := owedStart(t, e, "owed-2", path)
	require.Len(t, asks, 1)
	require.Equal(t, "owed-1", asks[0].Get(harvest.TranscriptSessionParam))
	require.Equal(t, path, asks[0].Get(harvest.TranscriptPathParam))

	rows := ledgerLiveRows(t, path)
	changed := e.clock().Add(-time.Minute)
	status, a := owedPushRows(e, "owed-1", rows, changed)
	require.Equal(t, http.StatusOK, status, a.Error)
	require.Equal(t, harvest.UsageWritten, a.Result)
	require.Equal(t, rows, sessionLedger(t, e.db, sess.ID))
	got := requireAmbientSession(t, e.db, ClientClaudeCode, "owed-1")
	require.Equal(t, core.SessionCompleted, got.Status, "the push revives no row")
	o, found := owedRow(t, e, "owed-1")
	require.True(t, found, "owed still: the conversation may go on on its row")
	require.Equal(t, 0, o.Asks, "the answer brought a change")
	require.Equal(t, changed.UTC(), *o.ChangedAt)

	require.Empty(t, owedStart(t, e, "owed-3", path), "within the wait after the answer")
	e.set(e.clock().Add(usageOwedBackoff))
	asks = owedStart(t, e, "owed-4", path)
	require.Len(t, asks, 1)
	status, a = owedPushRows(e, "owed-1", rows, changed)
	require.Equal(t, http.StatusOK, status, a.Error)
	require.Equal(t, harvest.UsageUnchanged, a.Result)
	o, _ = owedRow(t, e, "owed-1")
	require.Equal(t, 1, o.Asks, "no change: the asks keep counting")

	// Its SessionEnd comes after all: the whole transcript is harvested, and
	// nothing is owed.
	totals, full, ok := harvest.ClaudeUsage(path)
	require.True(t, ok)
	ledgerLiveHook(e, "session-end", "", map[string]any{"session_id": "owed-1", "transcript_path": path,
		harvest.Field: map[string]any{"version": 1, "findings": "(auto-harvested) Done.", "tokens": totals, "usage": full}})
	_, found = owedRow(t, e, "owed-1")
	require.False(t, found)
	e.set(e.clock().Add(time.Hour))
	require.Empty(t, owedStart(t, e, "owed-5", path))
}

// An answer is taken only for an ask made within the window, once per the
// rules: a push nobody asked for, or one too late, is not; a report of a
// refused or missing path settles the conversation; a report carrying rows,
// a word this release does not know, or rows without when the transcript
// changed are refused. Past the ask limit the conversation is given up on.
func TestOwedUsage_TheAnswersTaken(t *testing.T) {
	e := newLockHooks(t, true)
	path := writeTranscript(t, ledgerLiveTurn1)
	rows := ledgerLiveRows(t, path)
	owedStart(t, e, "fence-1", path)
	owedEnd(t, e, "fence-1")

	status, a := owedPushRows(e, "fence-1", rows, e.clock())
	require.Equal(t, http.StatusNotFound, status, "never asked")
	require.Equal(t, usageNotAsked, a.Error)

	require.Len(t, owedStart(t, e, "fence-2", path), 1)
	e.set(e.clock().Add(usageOwedAnswerWindow))
	status, _ = owedPushRows(e, "fence-1", rows, e.clock())
	require.Equal(t, http.StatusNotFound, status, "an answer past the window")
	require.Empty(t, sessionLedger(t, e.db, requireAmbientSession(t, e.db, ClientClaudeCode, "fence-1").ID))

	e.set(e.clock().Add(usageOwedWait(1)))
	require.Len(t, owedStart(t, e, "fence-3", path), 1)
	status, _ = owedPushBody(e, "fence-1", rows, map[string]any{harvest.UsageRefusedField: harvest.TranscriptRefusedPath})
	require.Equal(t, http.StatusBadRequest, status, "a report carries no rows")
	status, _ = owedPushBody(e, "fence-1", nil, map[string]any{harvest.UsageRefusedField: "elsewhere"})
	require.Equal(t, http.StatusBadRequest, status, "a word this release does not know")
	status, _ = owedPushBody(e, "fence-1", rows, nil)
	require.Equal(t, http.StatusBadRequest, status, "rows without when the transcript changed")
	_, found := owedRow(t, e, "fence-1")
	require.True(t, found, "a refused answer changes nothing")
	status, a = owedPushBody(e, "fence-1", nil, map[string]any{harvest.UsageRefusedField: harvest.TranscriptRefusedMissing})
	require.Equal(t, http.StatusOK, status, a.Error)
	require.Equal(t, harvest.UsageSettled, a.Result)
	_, found = owedRow(t, e, "fence-1")
	require.False(t, found, "a missing transcript owes nothing more")

	// Nobody answers (an older seam): after the limit the asks end.
	owedStart(t, e, "quiet-1", path)
	owedEnd(t, e, "quiet-1")
	for i := range usageOwedAskLimit {
		e.set(e.clock().Add(usageOwedWait(i)))
		require.Len(t, owedStart(t, e, "quiet-ask", path), 1, "ask %d", i+1)
	}
	e.set(e.clock().Add(usageOwedWait(usageOwedAskLimit)))
	require.Empty(t, owedStart(t, e, "quiet-ask", path))
	_, found = owedRow(t, e, "quiet-1")
	require.False(t, found, "given up on")
}

// An answer asks for a few conversations at most; never the conversation
// the hook names itself, nor one in a project locked to the caller; and the
// TCP key's hook under peer_auth, which pushes nothing, is asked nothing.
func TestOwedUsage_WhatAnAnswerAsks(t *testing.T) {
	e := newLockHooks(t, true)
	path := writeTranscript(t, ledgerLiveTurn1)
	// All started before any ends, so no start asks for another.
	ids := []string{"many-1", "many-2", "many-3", "many-4"}
	for _, id := range ids {
		owedStart(t, e, id, path)
	}
	ledgerLiveHook(e, "session-start", "", map[string]any{"session_id": "many-fin", "cwd": "/work/finance",
		"source": "startup", "transcript_path": path})
	fin := requireAmbientSession(t, e.db, ClientClaudeCode, "many-fin")
	require.Equal(t, lkFin, fin.ProjectSlug)
	for i, id := range append(ids, "many-fin") {
		e.set(e.clock().Add(time.Duration(i) * time.Second)) // the longest owed first
		owedEnd(t, e, id)
	}

	// The TCP key under peer_auth: no ask.
	e.mu.Lock()
	e.view = func() (access.View, bool) { return e.policy.OwnerView(), true }
	e.mu.Unlock()
	require.Empty(t, owedStart(t, e, "tcp-1", path))
	status, _ := owedPushRows(e, "many-1", ledgerLiveRows(t, path), e.clock())
	require.Equal(t, http.StatusForbidden, status)
	e.mu.Lock()
	e.view = e.socketView
	e.mu.Unlock()

	asks := owedStart(t, e, "many-next", path)
	require.Len(t, asks, harvest.MaxPendingPerAnswer)
	var named []string
	for _, q := range asks {
		named = append(named, q.Get(harvest.TranscriptSessionParam))
	}
	require.Equal(t, []string{"many-1", "many-2", "many-3"}, named, "the longest owed first")
	asks = owedStart(t, e, "many-later", path)
	require.Len(t, asks, 1, "the rest wait their own turn; the locked project's never comes")
	require.Equal(t, "many-4", asks[0].Get(harvest.TranscriptSessionParam))

	// The conversation a SessionEnd names is its own end's to harvest.
	e.set(e.clock().Add(usageOwedWait(1)))
	resp, _ := ledgerLiveHook(e, "session-end", "", map[string]any{"session_id": "many-2", "transcript_path": path})
	for _, raw := range resp.Header.Values(harvest.UsageOwedHeader) {
		q, err := url.ParseQuery(raw)
		require.NoError(t, err)
		require.NotEqual(t, "many-2", q.Get(harvest.TranscriptSessionParam))
	}
}

// With peer_auth off nothing is asked -- the daemon's upkeep reads the
// transcript itself -- and a SessionEnd it harvests settles the conversation.
func TestOwedUsage_OffThePeerAuthSocketTheEndSettlesIt(t *testing.T) {
	e := newLedgerLiveEnv(t, false)
	path := writeTranscript(t, ledgerLiveTurn1)
	sess := e.start("local-1", path)
	sess.Status, sess.Findings = core.SessionCompleted, "done"
	require.NoError(t, store.UpdateSession(context.Background(), e.db, sess))
	_, err := store.MarkUsageOwed(context.Background(), e.db, sess.ID, e.clock())
	require.NoError(t, err)

	resp, _ := e.hook("session-start", "", map[string]any{"session_id": "local-2", "cwd": "/work/demo", "source": "startup",
		"transcript_path": path})
	require.Empty(t, resp.Header.Values(harvest.UsageOwedHeader))

	e.hook("session-end", "", map[string]any{"session_id": "local-1", "transcript_path": path})
	require.Equal(t, ledgerLiveRows(t, path), e.ledger("local-1"))
	_, found, err := store.OwedUsageOf(context.Background(), e.db, string(ClientClaudeCode), "local-1")
	require.NoError(t, err)
	require.False(t, found)
}
