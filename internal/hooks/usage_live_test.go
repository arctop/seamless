package hooks

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// The Diary's mid-session usage harvest (usage_live.go).

// ledgerLiveTurn1 is a session's first turn: the prompt and one message,
// written as two lines, in the 09:00 quarter-hour. ledgerLiveTurn2 is the
// next turn, in the 09:30 one, and ledgerLiveTurn2Row the row it adds.
var (
	ledgerLiveTurn1 = `{"type":"user","timestamp":"2026-11-21T09:00:30Z","message":{"role":"user","content":"deploy"}}` + "\n" +
		ledgerLine("msg_A", "2026-11-21T09:01:00Z", "claude-x", false, 100, 50, 1000, 200) + "\n" +
		ledgerLine("msg_A", "2026-11-21T09:01:05Z", "claude-x", false, 100, 50, 1000, 200) + "\n"
	ledgerLiveTurn2 = ledgerLine("msg_B", "2026-11-21T09:31:00Z", "claude-y", false, 20, 10, 0, 0) + "\n"

	ledgerLiveTurn2Row = core.UsageBucket{Bucket: time.Date(2026, 11, 21, 9, 30, 0, 0, time.UTC), Model: "claude-y",
		Thread: core.UsageThreadMain, Input: 20, Output: 10, Messages: 1,
		MinuteMask: core.UsageMinuteBit(time.Date(2026, 11, 21, 9, 31, 0, 0, time.UTC))}
)

// ledgerLiveAppend adds lines to a transcript, as the agent client does.
func ledgerLiveAppend(t *testing.T, path, lines string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(lines)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// ledgerLiveRows is what a transcript's harvest reads now.
func ledgerLiveRows(t *testing.T, path string) []core.UsageBucket {
	t.Helper()
	_, rows, ok := harvest.ClaudeUsage(path)
	require.True(t, ok, path)
	return rows
}

// ledgerLiveEnv is a hook server with peer_auth off, where the daemon reads
// the agent's disk itself: the Diary as asked, and a clock of the test's for
// the throttle.
type ledgerLiveEnv struct {
	t   *testing.T
	h   *Handler
	db  *sql.DB
	url string
	mu  sync.Mutex
	now time.Time
}

func newLedgerLiveEnv(t *testing.T, diary bool) *ledgerLiveEnv {
	t.Helper()
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(ctx, db, localHostName))
	_, err = store.EnsureProject(ctx, db, "demo", "demo")
	require.NoError(t, err)
	require.NoError(t, store.AddRepoMapping(ctx, db, "/work/demo", "demo"))
	// newHandlerServer's memories: the prompt matcher weighs a term by how
	// few memories hold it, so one memory alone matches nothing.
	insertMemory(t, db, "01A", "constraint", "no-force-push", "never force push to main", "demo")
	insertMemory(t, db, "01B", "gotcha", "chroma-boot-race", "chroma container health check startup race", "demo")
	insertMemory(t, db, "01C", "decision", "ulid-over-uuid", "use ulid identifiers not uuid values", "demo")
	insertMemory(t, db, "01D", "reference", "sqlite-wal", "enable wal journal mode and busy timeout", "demo")
	e := &ledgerLiveEnv{t: t, db: db, now: time.Date(2026, 11, 21, 9, 40, 0, 0, time.UTC)}
	e.h = NewHandler(Config{DB: db, Retrieve: retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil),
		Events: events.NewRecorder(db), APIKey: testKey, LocalHost: localHostName, Features: config.Features{Diary: diary}, Now: e.clock})
	// Registered after the database's close, so it runs before it: no read
	// outlives the test. Bounded, so a read a broken test left waiting fails
	// the test instead of hanging the package.
	t.Cleanup(func() {
		if !e.h.WaitFor(10 * time.Second) {
			t.Error("a mid-session read outlived its test")
		}
	})
	mux := http.NewServeMux()
	e.h.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	e.url = ts.URL
	return e
}

func (e *ledgerLiveEnv) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *ledgerLiveEnv) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = e.now.Add(d)
}

// hook posts one hook as Claude Code's http hook (or a `seam hook` on this
// computer) would, and returns the answer with its body.
func (e *ledgerLiveEnv) hook(event, query string, body map[string]any) (*http.Response, []byte) {
	e.t.Helper()
	return ledgerLivePost(e.t, e.url+"/api/hooks/"+event+query, body)
}

// start opens conversation id's ambient session, naming transcript.
func (e *ledgerLiveEnv) start(id, transcript string) core.Session {
	e.t.Helper()
	e.hook("session-start", "", map[string]any{"session_id": id, "cwd": "/work/demo", "source": "startup", "transcript_path": transcript})
	return requireAmbientSession(e.t, e.db, ClientClaudeCode, id)
}

// prompt submits a prompt in conversation id; the answer's background read,
// if it started one, is done when it returns.
func (e *ledgerLiveEnv) prompt(id, transcript string) *http.Response {
	e.t.Helper()
	resp, _ := e.hook("user-prompt-submit", "", ledgerLivePromptBody(id, "/work/demo", transcript))
	e.h.Wait()
	return resp
}

// ledger is conversation id's ambient row's ledger.
func (e *ledgerLiveEnv) ledger(id string) []core.UsageBucket {
	e.t.Helper()
	return sessionLedger(e.t, e.db, requireAmbientSession(e.t, e.db, ClientClaudeCode, id).ID)
}

func ledgerLivePromptBody(id, cwd, transcript string) map[string]any {
	body := map[string]any{"session_id": id, "cwd": cwd, "user_prompt": "why does the chroma container fail its health check",
		"hook_event_name": "UserPromptSubmit"}
	if transcript != "" {
		body["transcript_path"] = transcript
	}
	return body
}

// ledgerLiveClaudeBody is a prompt as Claude Code itself names its fields
// (its 2.1.330): the prompt under "prompt", beside session_title. Whether
// decodePrompt reads that field for Claude Code or not, the harvest runs.
func ledgerLiveClaudeBody(id, cwd, transcript string) map[string]any {
	return map[string]any{"session_id": id, "cwd": cwd, "transcript_path": transcript, "permission_mode": "default",
		"hook_event_name": "UserPromptSubmit", "prompt": "why does the chroma container fail its health check",
		"session_title": "Fix the chroma boot race"}
}

// ledgerLiveEmptyBody is a prompt the hook reads no text from under any
// field name: the hook's early return, where the harvest runs too.
func ledgerLiveEmptyBody(id, cwd, transcript string) map[string]any {
	return map[string]any{"session_id": id, "cwd": cwd, "transcript_path": transcript, "permission_mode": "default",
		"hook_event_name": "UserPromptSubmit", "prompt": ""}
}

// ledgerLivePost posts body to target with the test key and returns the
// answer with its body read.
func ledgerLivePost(t *testing.T, target string, body map[string]any) (*http.Response, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, out
}

// With peer_auth off, on the agent's own computer, a prompt keeps the ledger
// current to the turn before it: the daemon reads the transcript itself once
// it has answered, so the answer asks for nothing. The session stays live,
// its totals the ledger's sum, and the end then changes only the last turn.
func TestPromptUsage_TheDaemonKeepsTheLedgerCurrent(t *testing.T) {
	e := newLedgerLiveEnv(t, true)
	path := writeTranscript(t, ledgerLiveTurn1)
	e.start("live-read-1", path)

	resp := e.prompt("live-read-1", path)
	require.Empty(t, resp.Header.Get(harvest.UsageWantedHeader), "the daemon reads it itself: nothing to ask for")
	mid := ledgerLiveRows(t, path)
	require.Len(t, mid, 1)
	require.Equal(t, mid, e.ledger("live-read-1"), "current to the turn before the prompt")
	sess := requireAmbientSession(t, e.db, ClientClaudeCode, "live-read-1")
	require.Equal(t, core.SessionActive, sess.Status)
	require.Equal(t, core.SumUsage(mid), sess.Tokens, "the totals are the ledger's sum")

	ledgerLiveAppend(t, path, ledgerLiveTurn2)
	e.hook("session-end", "", map[string]any{"session_id": "live-read-1", "transcript_path": path, "reason": "exit"})
	end := e.ledger("live-read-1")
	require.Equal(t, append(slices.Clone(mid), ledgerLiveTurn2Row), end, "the end changes only the last turn")
	require.Equal(t, ledgerLiveRows(t, path), end)
	require.Equal(t, core.SessionCompleted, requireAmbientSession(t, e.db, ClientClaudeCode, "live-read-1").Status)
}

// The Diary off: the prompt reads, asks and writes nothing, keeps no
// throttle state, and answers exactly as it does with the Diary on -- the
// same status, headers and body -- since on, too, this transport adds
// nothing to the answer. Every body shape: the field this package's tests
// send, Claude Code's own, and a prompt with no text (the hook's early
// return, which the harvest takes too).
func TestPromptUsage_DiaryOffReadsAsksAndWritesNothing(t *testing.T) {
	type answerIs uint8
	const (
		anyAnswer answerIs = iota // whatever the decode reads of it, on and off alike
		aRecall
		empty
	)
	for name, tc := range map[string]struct {
		body   func(id, cwd, transcript string) map[string]any
		answer answerIs
	}{
		"user_prompt": {ledgerLivePromptBody, aRecall},
		"prompt":      {ledgerLiveClaudeBody, anyAnswer},
		"no text":     {ledgerLiveEmptyBody, empty},
	} {
		t.Run(name, func(t *testing.T) {
			answer := func(diary bool) (*http.Response, []byte, *ledgerLiveEnv) {
				e := newLedgerLiveEnv(t, diary)
				path := writeTranscript(t, ledgerLiveTurn1)
				e.start("live-off-1", path)
				resp, body := e.hook("user-prompt-submit", "", tc.body("live-off-1", "/work/demo", path))
				e.h.Wait()
				return resp, body, e
			}
			offResp, offBody, off := answer(false)
			require.Empty(t, offResp.Header.Get(harvest.UsageWantedHeader))
			require.Empty(t, off.ledger("live-off-1"), "nothing read, nothing written")
			require.Zero(t, off.h.live.asked(), "no throttle state")
			require.True(t, requireAmbientSession(t, off.db, ClientClaudeCode, "live-off-1").Tokens.Empty())

			onResp, onBody, on := answer(true)
			require.Equal(t, ledgerLiveRows(t, writeTranscript(t, ledgerLiveTurn1)), on.ledger("live-off-1"),
				"the control: with the Diary on the same prompt harvests")
			require.Equal(t, onResp.StatusCode, offResp.StatusCode)
			require.Equal(t, string(onBody), string(offBody), "the same answer")
			switch tc.answer {
			case aRecall:
				require.Contains(t, string(offBody), "chroma-boot-race", "an answer with a recall in it")
			case empty:
				require.Empty(t, additionalContext(t, ledgerLiveJSON(t, offBody)), "the answer to a prompt with no text")
			}
			ledgerLiveSameHeaders(t, onResp.Header, offResp.Header)
			require.ElementsMatch(t, []string{"Content-Type", "Content-Length", "Date"}, slices.Collect(maps.Keys(offResp.Header)),
				"the headers the prompt's answer always had, and no other")
		})
	}
}

// ledgerLiveJSON decodes a hook answer's body.
func ledgerLiveJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

// Claude Code's own body, and a prompt with no text, on the peer_auth
// socket: the answer asks all the same, and the push lands. Off, the same
// body answers the same and asks nothing.
func TestPromptUsage_EveryBodyShapeOnTheSocket(t *testing.T) {
	for name, body := range map[string]func(id, cwd, transcript string) map[string]any{
		"prompt": ledgerLiveClaudeBody, "no text": ledgerLiveEmptyBody,
	} {
		t.Run(name, func(t *testing.T) {
			e := newLedgerLiveSocket(t)
			path := writeTranscript(t, ledgerLiveTurn1)
			ledgerLiveHook(e, "session-start", "", map[string]any{"session_id": "live-cc-1", "cwd": "/work/ops", "source": "startup"})
			resp, onBody := ledgerLiveHook(e, "user-prompt-submit", "", body("live-cc-1", "/work/ops", path))
			require.Equal(t, harvest.UsageWanted, resp.Header.Get(harvest.UsageWantedHeader))
			status, a := ledgerLivePush(e, "", "live-cc-1", ledgerLiveRows(t, path))
			require.Equal(t, http.StatusOK, status, a.Error)
			require.Equal(t, harvest.UsageWritten, a.Result)
			require.Equal(t, ledgerLiveRows(t, path), sessionLedger(t, e.db, requireAmbientSession(t, e.db, ClientClaudeCode, "live-cc-1").ID))

			require.NoError(t, store.ClearFeaturesConfig(context.Background(), e.db))
			e.set(e.clock().Add(usageLiveInterval))
			offResp, offBody := ledgerLiveHook(e, "user-prompt-submit", "", body("live-cc-1", "/work/ops", path))
			require.Empty(t, offResp.Header.Get(harvest.UsageWantedHeader))
			require.Equal(t, string(onBody), string(offBody))
		})
	}
}

// ledgerLiveSameHeaders holds two answers' headers equal but for Date.
func ledgerLiveSameHeaders(t *testing.T, want, got http.Header) {
	t.Helper()
	w, g := want.Clone(), got.Clone()
	w.Del("Date")
	g.Del("Date")
	require.Equal(t, w, g)
}

// The throttle: a conversation's usage is read at most once per
// usageLiveInterval, each conversation on its own clock.
func TestPromptUsage_TheThrottle(t *testing.T) {
	e := newLedgerLiveEnv(t, true)
	path := writeTranscript(t, ledgerLiveTurn1)
	e.start("live-thr-1", path)
	e.prompt("live-thr-1", path)
	require.Len(t, e.ledger("live-thr-1"), 1)

	ledgerLiveAppend(t, path, ledgerLiveTurn2)
	e.advance(usageLiveInterval - time.Second)
	e.prompt("live-thr-1", path)
	require.Len(t, e.ledger("live-thr-1"), 1, "within the interval: not read again")

	other := writeTranscript(t, ledgerLiveTurn1)
	e.start("live-thr-2", other)
	e.prompt("live-thr-2", other)
	require.Len(t, e.ledger("live-thr-2"), 1, "another conversation is not held by this one's throttle")

	e.advance(time.Second)
	e.prompt("live-thr-1", path)
	require.Equal(t, ledgerLiveRows(t, path), e.ledger("live-thr-1"), "the interval passed: read again")

	// An entry past its interval is dropped: the state holds only the
	// conversations asked within the last one.
	e.advance(usageLiveInterval)
	e.prompt("live-thr-2", other)
	require.Equal(t, 1, e.h.live.asked())
}

// The console's override turns the harvest on and off with no restart: the
// hooks read the features live.
func TestPromptUsage_TheToggleIsLive(t *testing.T) {
	ctx := context.Background()
	e := newLedgerLiveEnv(t, false)
	path := writeTranscript(t, ledgerLiveTurn1)
	e.start("live-tog-1", path)
	e.prompt("live-tog-1", path)
	require.Empty(t, e.ledger("live-tog-1"))

	require.NoError(t, store.SetFeaturesConfig(ctx, e.db, config.Features{Diary: true}))
	e.prompt("live-tog-1", path)
	require.Equal(t, ledgerLiveRows(t, path), e.ledger("live-tog-1"), "on from the console")

	require.NoError(t, store.SetFeaturesConfig(ctx, e.db, config.Features{}))
	ledgerLiveAppend(t, path, ledgerLiveTurn2)
	e.advance(usageLiveInterval)
	e.prompt("live-tog-1", path)
	require.Len(t, e.ledger("live-tog-1"), 1, "off from the console: not read")
}

// No read where the daemon may not or need not make one: a Codex
// conversation (each Stop credits its counter), a conversation on another
// computer -- whose skip the Diary never records once a prompt -- and a
// prompt in a session the hooks never started.
func TestPromptUsage_NoReadWhereNoneIsTheDaemons(t *testing.T) {
	skips := func(diary bool) (int, *ledgerLiveEnv) {
		e := newLedgerLiveEnv(t, diary)
		path := writeTranscript(t, ledgerLiveTurn1)
		e.hook("session-start", "?host=beta", map[string]any{"session_id": "live-remote-1", "cwd": "/work/demo",
			"source": "startup", "transcript_path": path})
		e.prompt("live-remote-1", path)
		require.Empty(t, e.ledger("live-remote-1"), "another computer's transcript is not read here")
		var n int
		require.NoError(t, e.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND payload LIKE '%remote-host-skip%'`,
			string(core.EventHookError)).Scan(&n))
		return n, e
	}
	off, _ := skips(false)
	on, e := skips(true)
	require.Equal(t, off, on, "the Diary records no skip of its own")
	require.Zero(t, e.h.live.asked(), "and keeps no state for it")

	path := writeTranscript(t, ledgerLiveTurn1)
	e.hook("session-start", "?client=codex", map[string]any{"session_id": "live-codex-1", "cwd": "/work/demo",
		"source": "startup", "model": "gpt-x", "transcript_path": path})
	e.hook("user-prompt-submit", "?client=codex", map[string]any{"session_id": "live-codex-1", "cwd": "/work/demo",
		"prompt": "deploy", "model": "gpt-x", "transcript_path": path})
	e.h.Wait()
	require.Empty(t, sessionLedger(t, e.db, requireAmbientSession(t, e.db, ClientCodex, "live-codex-1").ID), "a Codex prompt reads nothing")

	e.prompt("live-never-started", path)
	require.Zero(t, e.h.live.asked(), "no ambient row: nothing to keep current")
}

// The ask outlasts the push it waits for, so a sender within its own
// timeout always finds its ask open.
func TestUsageLive_TheAskOutlastsThePush(t *testing.T) {
	require.GreaterOrEqual(t, usageLiveInterval, harvest.UsagePushTimeout)
	l := newLiveUsage()
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	require.True(t, l.claim("k", liveAsk, at))
	require.False(t, l.answer("other", at))
	require.True(t, l.answer("k", at.Add(harvest.UsagePushTimeout)))
	require.False(t, l.answer("k", at.Add(harvest.UsagePushTimeout)), "one push an ask")
	require.False(t, l.claim("k", liveAsk, at.Add(usageLiveInterval-time.Nanosecond)))
	require.True(t, l.claim("k", liveAsk, at.Add(usageLiveInterval)))
	require.False(t, l.answer("k", at.Add(2*usageLiveInterval)), "an ask past its interval is closed")
}

// ledgerLiveSocket is lockHooks with the Diary on from the console: the
// daemon under peer_auth, every request the owner's socket connection.
func newLedgerLiveSocket(t *testing.T) *lockHooks {
	t.Helper()
	e := newLockHooks(t, true)
	require.NoError(t, store.SetFeaturesConfig(context.Background(), e.db, config.Features{Diary: true}))
	return e
}

// ledgerLiveHook posts a hook as the agent's `seam hook` does under
// peer_auth (the agent process and host in the query).
func ledgerLiveHook(e *lockHooks, event, client string, body map[string]any) (*http.Response, []byte) {
	e.t.Helper()
	q := url.Values{"host": {localHostName}, "agent_process": {lkAgent.AgentID()}}
	if client != "" {
		q.Set("client", client)
	}
	if cwd, _ := body["cwd"].(string); cwd != "" && event == "session-start" {
		q.Set("repo_root", cwd)
		q.Set("main_root", cwd)
	}
	return ledgerLivePost(e.t, e.url+"/api/hooks/"+event+"?"+q.Encode(), body)
}

// ledgerLivePush posts the rows as `seam hook-usage` does and returns the
// status and the answer.
func ledgerLivePush(e *lockHooks, client, id string, rows []core.UsageBucket) (int, harvest.UsagePushAnswer) {
	e.t.Helper()
	q := url.Values{"host": {localHostName}}
	if client != "" {
		q.Set("client", client)
	}
	body := map[string]any{"session_id": id}
	if rows != nil {
		tokens := core.SumUsage(rows)
		body[harvest.Field] = harvest.Push{Version: harvest.PushVersion, Tokens: &tokens, Usage: rows}
	}
	resp, out := ledgerLivePost(e.t, e.url+harvest.UsageEndpoint+"?"+q.Encode(), body)
	var a harvest.UsagePushAnswer
	require.NoError(e.t, json.Unmarshal(out, &a), "%s", out)
	return resp.StatusCode, a
}

// On the peer_auth socket the daemon reads nothing: the prompt's answer asks
// for the usage, at most once per interval, and the push it asked for lands
// once. The end then changes only the last turn, and a push the end overtook
// writes nothing.
func TestPromptUsage_TheSocketAsksAndThePushLands(t *testing.T) {
	e := newLedgerLiveSocket(t)
	path := writeTranscript(t, ledgerLiveTurn1)
	ledgerLiveHook(e, "session-start", "", map[string]any{"session_id": "live-sock-1", "cwd": "/work/ops", "source": "startup",
		"transcript_path": path})
	ledger := func() []core.UsageBucket {
		return sessionLedger(t, e.db, requireAmbientSession(t, e.db, ClientClaudeCode, "live-sock-1").ID)
	}

	resp, _ := ledgerLiveHook(e, "user-prompt-submit", "", ledgerLivePromptBody("live-sock-1", "/work/ops", path))
	require.Equal(t, harvest.UsageWanted, resp.Header.Get(harvest.UsageWantedHeader))
	require.Empty(t, ledger(), "the daemon read nothing")
	mid := ledgerLiveRows(t, path)
	status, a := ledgerLivePush(e, "", "live-sock-1", mid)
	require.Equal(t, http.StatusOK, status, a.Error)
	require.Equal(t, harvest.UsageWritten, a.Result)
	require.Equal(t, mid, ledger())
	status, a = ledgerLivePush(e, "", "live-sock-1", mid)
	require.Equal(t, http.StatusNotFound, status, "one push an ask")
	require.Equal(t, usageNotAsked, a.Error)

	resp, _ = ledgerLiveHook(e, "user-prompt-submit", "", ledgerLivePromptBody("live-sock-1", "/work/ops", path))
	require.Empty(t, resp.Header.Get(harvest.UsageWantedHeader), "within the interval: no ask")

	ledgerLiveAppend(t, path, ledgerLiveTurn2)
	e.set(e.clock().Add(usageLiveInterval))
	resp, _ = ledgerLiveHook(e, "user-prompt-submit", "", ledgerLivePromptBody("live-sock-1", "/work/ops", path))
	require.Equal(t, harvest.UsageWanted, resp.Header.Get(harvest.UsageWantedHeader), "the interval passed: asked again")

	// The end's push lands before this ask's: the ledger is the end's, and
	// the late push, which read the transcript before the last turn, is behind.
	totals, full, ok := harvest.ClaudeUsage(path)
	require.True(t, ok)
	resp, _ = ledgerLiveHook(e, "session-end", "", map[string]any{"session_id": "live-sock-1", "transcript_path": path,
		harvest.Field: map[string]any{"version": 1, "findings": "(auto-harvested) Done.", "tokens": totals, "usage": full}})
	require.Empty(t, resp.Header.Get(harvest.RefusedHeader))
	require.Equal(t, append(slices.Clone(mid), ledgerLiveTurn2Row), ledger(), "the end changes only the last turn")
	status, a = ledgerLivePush(e, "", "live-sock-1", mid)
	require.Equal(t, http.StatusOK, status, a.Error)
	require.Equal(t, harvest.UsageBehind, a.Result)
	require.Equal(t, full, ledger(), "the late push wrote nothing")
}

// The Diary turned off between an ask and its push: the push writes nothing.
func TestUsagePush_OffBetweenTheAskAndThePush(t *testing.T) {
	e := newLedgerLiveSocket(t)
	path := writeTranscript(t, ledgerLiveTurn1)
	ledgerLiveHook(e, "session-start", "", map[string]any{"session_id": "live-race-1", "cwd": "/work/ops", "source": "startup"})
	resp, _ := ledgerLiveHook(e, "user-prompt-submit", "", ledgerLivePromptBody("live-race-1", "/work/ops", path))
	require.Equal(t, harvest.UsageWanted, resp.Header.Get(harvest.UsageWantedHeader))
	require.NoError(t, store.ClearFeaturesConfig(context.Background(), e.db))
	status, a := ledgerLivePush(e, "", "live-race-1", ledgerLiveRows(t, path))
	require.Equal(t, http.StatusNotFound, status)
	require.Equal(t, usageNotAsked, a.Error)
	require.Empty(t, sessionLedger(t, e.db, requireAmbientSession(t, e.db, ClientClaudeCode, "live-race-1").ID))
}

// What the push route refuses, before anything is written: anything but the
// peer_auth socket, a Codex session, a body that carries no rows (or rows
// that do not hold), a session nobody asked, one that does not exist, and a
// session in a project locked to the caller, which is never asked either.
func TestUsagePush_Refusals(t *testing.T) {
	e := newLedgerLiveSocket(t)
	path := writeTranscript(t, ledgerLiveTurn1)
	rows := ledgerLiveRows(t, path)
	ledgerLiveHook(e, "session-start", "", map[string]any{"session_id": "live-ref-1", "cwd": "/work/ops", "source": "startup"})

	status, a := ledgerLivePush(e, "", "live-ref-1", rows)
	require.Equal(t, http.StatusNotFound, status, "never asked")
	require.Equal(t, usageNotAsked, a.Error)
	status, a = ledgerLivePush(e, "", "live-nobody", rows)
	require.Equal(t, http.StatusNotFound, status, "no such session")
	require.Equal(t, usageNotAsked, a.Error)
	status, _ = ledgerLivePush(e, "codex", "live-ref-1", rows)
	require.Equal(t, http.StatusBadRequest, status, "a Codex session's Stop credits its counter")
	status, _ = ledgerLivePush(e, "", "live-ref-1", nil)
	require.Equal(t, http.StatusBadRequest, status, "a push carries the rows")
	short := core.SumUsage(rows)
	short.Output--
	short.Normalize()
	resp, _ := ledgerLivePost(t, e.url+harvest.UsageEndpoint, map[string]any{"session_id": "live-ref-1",
		harvest.Field: harvest.Push{Version: harvest.PushVersion, Tokens: &short, Usage: rows}})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "rows that do not sum to their totals")

	// The locked project's session is recorded there, metadata only: its
	// prompt asks nothing, and a push for it is not taken.
	ledgerLiveHook(e, "session-start", "", map[string]any{"session_id": "live-ref-fin", "cwd": "/work/finance", "source": "startup"})
	fin := requireAmbientSession(t, e.db, ClientClaudeCode, "live-ref-fin")
	require.Equal(t, lkFin, fin.ProjectSlug)
	resp, _ = ledgerLiveHook(e, "user-prompt-submit", "", ledgerLivePromptBody("live-ref-fin", "/work/finance", path))
	require.Empty(t, resp.Header.Get(harvest.UsageWantedHeader), "a locked project's session takes no counts")
	status, _ = ledgerLivePush(e, "", "live-ref-fin", rows)
	require.Equal(t, http.StatusNotFound, status)
	require.Empty(t, sessionLedger(t, e.db, fin.ID))

	// The TCP key under peer_auth: its hook pushes nothing, so its prompt is
	// asked nothing, and the route is the socket's alone.
	e.mu.Lock()
	e.view = func() (access.View, bool) { return e.policy.OwnerView(), true }
	e.mu.Unlock()
	e.set(e.clock().Add(usageLiveInterval))
	resp, _ = ledgerLiveHook(e, "user-prompt-submit", "", ledgerLivePromptBody("live-ref-1", "/work/ops", path))
	require.Empty(t, resp.Header.Get(harvest.UsageWantedHeader))
	status, _ = ledgerLivePush(e, "", "live-ref-1", rows)
	require.Equal(t, http.StatusForbidden, status)
	e.mu.Lock()
	e.view = nil // peer_auth off: the daemon reads the file itself
	e.mu.Unlock()
	status, _ = ledgerLivePush(e, "", "live-ref-1", rows)
	require.Equal(t, http.StatusForbidden, status)
	require.Empty(t, sessionLedger(t, e.db, requireAmbientSession(t, e.db, ClientClaudeCode, "live-ref-1").ID))
}

// A restricted account's own session is asked like the owner's and takes its
// push; the owner, naming the same agent session id, names another session
// (the account's ids are its own) and is never asked for this one.
func TestPromptUsage_ARestrictedAccountsOwnSession(t *testing.T) {
	ctx := context.Background()
	e := newRestrictedHooks(t)
	require.NoError(t, store.SetFeaturesConfig(ctx, e.db, config.Features{Diary: true}))
	path := writeTranscript(t, ledgerLiveTurn1)
	send := func(event string, body map[string]any) *http.Response {
		t.Helper()
		q := url.Values{"host": {localHostName}, "agent_process": {rAgent.AgentID()}}
		if event == "session-start" {
			q.Set("repo_root", "/work/ops")
			q.Set("main_root", "/work/ops")
		}
		resp, _ := ledgerLivePost(t, e.url+"/api/hooks/"+event+"?"+q.Encode(), body)
		return resp
	}
	send("session-start", map[string]any{"session_id": "live-r-1", "cwd": "/work/ops", "source": "startup"})
	own := requireAmbientSession(t, e.db, ClientClaudeCode, rExt("live-r-1"))
	require.Equal(t, rAcct, own.Account)
	resp := send("user-prompt-submit", ledgerLivePromptBody("live-r-1", "/work/ops", path))
	require.Equal(t, harvest.UsageWanted, resp.Header.Get(harvest.UsageWantedHeader))

	e.asOwner()
	status, _ := ledgerLivePush(e.lockHooks, "", "live-r-1", ledgerLiveRows(t, path))
	require.Equal(t, http.StatusNotFound, status, "the owner's id names no session of the account's")
	e.asRestricted()
	status, a := ledgerLivePush(e.lockHooks, "", "live-r-1", ledgerLiveRows(t, path))
	require.Equal(t, http.StatusOK, status, a.Error)
	require.Equal(t, harvest.UsageWritten, a.Result)
	got := requireAmbientSession(t, e.db, ClientClaudeCode, rExt("live-r-1"))
	require.Equal(t, ledgerLiveRows(t, path), sessionLedger(t, e.db, got.ID))
	require.Equal(t, rAcct, got.Account, "the account's own session, still its own")
}
