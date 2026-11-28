package gardener

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// owedUsageLine is one Claude Code assistant line: a message id, its
// timestamp, a model and its usage block, the shape harvest.ClaudeUsage reads.
func owedUsageLine(id, ts string, in, out, read, create int) string {
	return `{"type":"assistant","timestamp":"` + ts + `","isSidechain":false,"message":{"id":"` + id +
		`","role":"assistant","model":"claude-x","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":` +
		strconv.Itoa(in) + `,"output_tokens":` + strconv.Itoa(out) + `,"cache_read_input_tokens":` + strconv.Itoa(read) +
		`,"cache_creation_input_tokens":` + strconv.Itoa(create) + `}}}`
}

// owedEnv is a store with a gardener over it whose clock is now and whose
// computer is owedHost.
type owedEnv struct {
	t   *testing.T
	db  *sql.DB
	now time.Time
}

const owedHost = "alpha"

func newOwedEnv(t *testing.T) *owedEnv {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &owedEnv{t: t, db: db, now: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)}
}

// session records an active ambient session of client named name, last alive
// at updated, on host, with path as the transcript its SessionStart recorded
// ("" for none).
func (e *owedEnv) session(name, client, host, path string, updated time.Time) core.Session {
	e.t.Helper()
	ctx := context.Background()
	sess := core.Session{ID: mustID(e.t), Name: name, Status: core.SessionActive, Ambient: true,
		ExternalClient: client, ExternalSessionID: name, Host: host, Source: "startup",
		CreatedAt: updated, UpdatedAt: updated}
	require.NoError(e.t, store.CreateSession(ctx, e.db, sess))
	if path != "" {
		require.NoError(e.t, store.SetSessionTranscriptPath(ctx, e.db, sess.ID, path))
	}
	return sess
}

// endedBySessionEnd completes sess as the session_end tool does, and marks its
// usage owed as that tool does.
func (e *owedEnv) endedBySessionEnd(sess core.Session, at time.Time) {
	e.t.Helper()
	ctx := context.Background()
	sess.Status, sess.UpdatedAt, sess.Findings = core.SessionCompleted, at, "done"
	require.NoError(e.t, store.UpdateSession(ctx, e.db, sess))
	marked, err := store.MarkUsageOwed(ctx, e.db, sess.ID, at)
	require.NoError(e.t, err)
	require.True(e.t, marked)
}

// upkeep runs the reaper and the owed-usage pass, as Housekeep does, with
// peer_auth as peerAuth says.
func (e *owedEnv) upkeep(peerAuth bool) {
	e.t.Helper()
	g := New(e.db, nil, nil, nil, events.NewRecorder(e.db), Config{SessionIdle: 30 * time.Minute,
		LocalHost: owedHost, PeerAuth: peerAuth}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.now = func() time.Time { return e.now }
	g.reapStaleSessions(context.Background())
	g.settleOwedUsage(context.Background())
}

// usage is a session's ledger rows and its row as stored.
func (e *owedEnv) usage(id string) ([]store.UsageRow, core.Session) {
	e.t.Helper()
	ctx := context.Background()
	rows, err := store.SessionUsage(ctx, e.db, id)
	require.NoError(e.t, err)
	sess, found, err := store.SessionByID(ctx, e.db, id)
	require.NoError(e.t, err)
	require.True(e.t, found)
	return rows, sess
}

// owed reports whether sess's usage is still owed.
func (e *owedEnv) owed(sess core.Session) bool {
	e.t.Helper()
	_, found, err := store.OwedUsageOf(context.Background(), e.db, sess.ExternalClient, sess.ExternalSessionID)
	require.NoError(e.t, err)
	return found
}

// owedTranscriptLines are a transcript's lines: two messages in the 09:00
// quarter-hour, at minutes 1 and 3 (msg_A written twice, as Claude Code
// writes a message's blocks).
var owedTranscriptLines = []string{
	`{"type":"user","timestamp":"2026-11-27T09:00:30Z","message":{"role":"user","content":"go"}}`,
	owedUsageLine("msg_A", "2026-11-27T09:01:10Z", 100, 50, 1000, 200),
	owedUsageLine("msg_A", "2026-11-27T09:01:12Z", 100, 50, 1000, 200),
	owedUsageLine("msg_B", "2026-11-27T09:03:59Z", 10, 5, 0, 0),
}

// writeOwedTranscript writes lines as a transcript last changed at changed.
func writeOwedTranscript(t *testing.T, path string, lines []string, changed time.Time) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	require.NoError(t, os.Chtimes(path, changed, changed))
}

// owedWant is the one row owedTranscriptLines hold.
var owedWant = core.UsageBucket{
	Bucket: time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC), Model: "claude-x", Thread: core.UsageThreadMain,
	Input: 110, Cached: 1000, CacheCreation: 200, Output: 55, Messages: 2, MinuteMask: 1<<1 | 1<<3,
}

// The reaper's expiry is the end a Claude Code session never got from its
// client: where the daemon may read the transcript, the upkeep records the
// session's usage from it, on the expired row, which keeps the time it was
// last alive. A live session waits for its own end.
func TestOwedUsage_TheUpkeepReadsAReapedSessionsTranscript(t *testing.T) {
	e := newOwedEnv(t)
	path := filepath.Join(t.TempDir(), "conversation.jsonl")
	writeOwedTranscript(t, path, owedTranscriptLines, e.now.Add(-2*time.Hour))
	idle := e.now.Add(-2 * time.Hour)
	reaped := e.session("cc/killed", store.UsageOwedClient, owedHost, path, idle)
	older := e.session("cc/older-client", store.UsageOwedClient, "", path, idle)
	live := e.session("cc/live", store.UsageOwedClient, owedHost, path, e.now.Add(-time.Minute))

	e.upkeep(false)

	for _, sess := range []core.Session{reaped, older} {
		rows, got := e.usage(sess.ID)
		require.Equal(t, core.SessionExpired, got.Status, sess.Name)
		require.Len(t, rows, 1, sess.Name)
		require.Equal(t, sess.ID, rows[0].SessionID)
		require.Equal(t, owedWant, rows[0].UsageBucket, "%s: msg_A once, both minutes in the mask", sess.Name)
		require.Equal(t, core.SumUsage([]core.UsageBucket{owedWant}), got.Tokens, "%s: the totals are the rows' sum", sess.Name)
		require.True(t, got.UpdatedAt.Equal(idle), "%s: the read moves no expired row's updated_at: %s", sess.Name, got.UpdatedAt)
		require.True(t, e.owed(sess), "%s: still owed: the conversation may go on on its ended row", sess.Name)
	}
	rows, got := e.usage(live.ID)
	require.Equal(t, core.SessionActive, got.Status)
	require.Empty(t, rows, "a live session is not the upkeep's to read")
	require.Zero(t, got.Tokens)
	require.False(t, e.owed(live))
}

// A conversation the agent ended with session_end goes on, and its SessionEnd
// never comes. Each upkeep after a change reads what the transcript holds
// then; one with nothing new reads nothing; and once the transcript has not
// changed for the horizon the owed usage is dropped.
func TestOwedUsage_ReadsAgainOnlyWhatChanged(t *testing.T) {
	e := newOwedEnv(t)
	path := filepath.Join(t.TempDir(), "conversation.jsonl")
	writeOwedTranscript(t, path, owedTranscriptLines[:2], e.now.Add(-time.Minute))
	sess := e.session("cc/ended", store.UsageOwedClient, owedHost, path, e.now.Add(-time.Minute))
	e.endedBySessionEnd(sess, e.now)

	e.upkeep(false)
	rows, got := e.usage(sess.ID)
	require.Equal(t, core.SessionCompleted, got.Status)
	require.Len(t, rows, 1)
	require.Equal(t, 1, rows[0].Messages, "msg_A alone so far")

	// The conversation went on: the next upkeep reads the turn after the end.
	e.now = e.now.Add(time.Hour)
	writeOwedTranscript(t, path, owedTranscriptLines, e.now.Add(-10*time.Minute))
	e.upkeep(false)
	rows, _ = e.usage(sess.ID)
	require.Len(t, rows, 1)
	require.Equal(t, owedWant, rows[0].UsageBucket)

	// Nothing changed since that read: nothing is read, even what the file
	// would say now (rewritten under the same time).
	e.now = e.now.Add(time.Hour)
	writeOwedTranscript(t, path, owedTranscriptLines[:2], e.now.Add(-70*time.Minute))
	e.upkeep(false)
	rows, _ = e.usage(sess.ID)
	require.Equal(t, owedWant, rows[0].UsageBucket, "an unchanged transcript is not read again")
	require.True(t, e.owed(sess))

	// Unchanged for the horizon: the last read had everything.
	e.now = e.now.Add(store.UsageOwedHorizon)
	e.upkeep(false)
	require.False(t, e.owed(sess))
	rows, _ = e.usage(sess.ID)
	require.Equal(t, owedWant, rows[0].UsageBucket)
}

// A conversation whose row is active again (a SessionStart resumed it) is the
// reaper's and its SessionEnd's again: its owed usage is dropped unread, and
// its next end marks it again.
func TestOwedUsage_AnActiveRowOwesNothing(t *testing.T) {
	e := newOwedEnv(t)
	path := filepath.Join(t.TempDir(), "conversation.jsonl")
	writeOwedTranscript(t, path, owedTranscriptLines, e.now.Add(-time.Minute))
	sess := e.session("cc/resumed", store.UsageOwedClient, owedHost, path, e.now)
	marked, err := store.MarkUsageOwed(context.Background(), e.db, sess.ID, e.now)
	require.NoError(t, err)
	require.True(t, marked)

	e.upkeep(false)
	require.False(t, e.owed(sess))
	rows, _ := e.usage(sess.ID)
	require.Empty(t, rows)
}

// The upkeep reads no transcript the daemon may not read, nor one of a
// session whose usage another harvest owns, nor a path that is not a regular
// file: each such conversation's owed usage is dropped unread (a Codex one is
// never owed), and each session is still reaped. Under peer_auth the
// caller's later hooks are asked for it, so it stays owed, unread, until the
// horizon.
func TestOwedUsage_ReadsOnlyTranscriptsItMay(t *testing.T) {
	transcript := func(t *testing.T, at time.Time) string {
		path := filepath.Join(t.TempDir(), "conversation.jsonl")
		writeOwedTranscript(t, path, owedTranscriptLines, at)
		return path
	}
	for _, tc := range []struct {
		name     string
		peerAuth bool
		client   string
		host     string
		path     func(t *testing.T, at time.Time) string
		owed     bool
	}{
		{name: "peer_auth: the daemon opens no file a hook named", peerAuth: true,
			client: store.UsageOwedClient, host: owedHost, path: transcript, owed: true},
		{name: "another computer's session", client: store.UsageOwedClient, host: "beta", path: transcript},
		{name: "a Codex session", client: "codex", host: owedHost, path: transcript},
		{name: "no transcript recorded", client: store.UsageOwedClient, host: owedHost,
			path: func(*testing.T, time.Time) string { return "" }},
		{name: "a transcript that is gone", client: store.UsageOwedClient, host: owedHost,
			path: func(t *testing.T, _ time.Time) string { return filepath.Join(t.TempDir(), "gone.jsonl") }},
		{name: "a path that is not a regular file", client: store.UsageOwedClient, host: owedHost,
			path: func(t *testing.T, _ time.Time) string { return t.TempDir() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newOwedEnv(t)
			sess := e.session("reaped", tc.client, tc.host, tc.path(t, e.now.Add(-2*time.Hour)), e.now.Add(-2*time.Hour))

			e.upkeep(tc.peerAuth)

			rows, got := e.usage(sess.ID)
			require.Equal(t, core.SessionExpired, got.Status, "reaped all the same")
			require.Empty(t, rows)
			require.Zero(t, got.Tokens)
			require.Equal(t, tc.owed, e.owed(sess))
			if !tc.owed {
				return
			}
			e.now = e.now.Add(store.UsageOwedHorizon)
			e.upkeep(tc.peerAuth)
			require.False(t, e.owed(sess), "past the horizon nothing waits for it")
		})
	}
}

// A restricted account's transcript is in that account's home: the daemon
// does not read it.
func TestOwedUsage_ARestrictedAccountsTranscriptIsNotRead(t *testing.T) {
	e := newOwedEnv(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conversation.jsonl")
	writeOwedTranscript(t, path, owedTranscriptLines, e.now.Add(-2*time.Hour))
	sess := core.Session{ID: mustID(t), Name: "cc/ci", Status: core.SessionCompleted, Ambient: true,
		ExternalClient: store.UsageOwedClient, ExternalSessionID: "ci-1", Host: owedHost, Account: "ci",
		CreatedAt: e.now.Add(-3 * time.Hour), UpdatedAt: e.now.Add(-2 * time.Hour)}
	require.NoError(t, store.CreateSession(ctx, e.db, sess))
	require.NoError(t, store.SetSessionTranscriptPath(ctx, e.db, sess.ID, path))
	_, err := store.MarkUsageOwed(ctx, e.db, sess.ID, e.now.Add(-2*time.Hour))
	require.NoError(t, err)

	e.upkeep(false)
	rows, _ := e.usage(sess.ID)
	require.Empty(t, rows)
	require.False(t, e.owed(sess))
}

// A subagent still writing keeps the conversation changing while its main
// transcript waits: its transcript's change counts.
func TestOwedUsage_ASubagentsWriteIsAChange(t *testing.T) {
	e := newOwedEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "conv.jsonl")
	writeOwedTranscript(t, path, owedTranscriptLines[:2], e.now.Add(-time.Minute))
	sess := e.session("cc/parent", store.UsageOwedClient, owedHost, path, e.now)
	e.endedBySessionEnd(sess, e.now)
	e.upkeep(false)
	rows, _ := e.usage(sess.ID)
	require.Len(t, rows, 1)

	// Hours on, the main transcript is as it was; a subagent beside it wrote.
	e.now = e.now.Add(3 * time.Hour)
	subDir := filepath.Join(dir, "conv", "subagents")
	require.NoError(t, os.MkdirAll(subDir, 0o700))
	sub := `{"type":"assistant","timestamp":"2026-11-27T13:30:00Z","isSidechain":true,"agentId":"a1",` +
		`"message":{"id":"msg_S","role":"assistant","model":"claude-x","content":[{"type":"text","text":"x"}],` +
		`"usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`
	writeOwedTranscript(t, filepath.Join(subDir, "agent-a1.jsonl"), []string{sub}, e.now.Add(-time.Minute))
	e.upkeep(false)
	rows, _ = e.usage(sess.ID)
	require.Len(t, rows, 2, "the subagent's row was read")
	require.True(t, e.owed(sess))
}
