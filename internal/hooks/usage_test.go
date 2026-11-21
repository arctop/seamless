package hooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
)

// ledgerLine is one synthetic Claude Code assistant line: a message id, a
// timestamp, a model, the sidechain flag and a usage block.
func ledgerLine(id, ts, model string, sidechain bool, in, out, read, create int) string {
	return `{"type":"assistant","timestamp":"` + ts + `","isSidechain":` + strconv.FormatBool(sidechain) +
		`,"message":{"id":"` + id + `","role":"assistant","model":"` + model + `","content":[{"type":"text","text":"Done."}],` +
		`"usage":{"input_tokens":` + strconv.Itoa(in) + `,"output_tokens":` + strconv.Itoa(out) +
		`,"cache_read_input_tokens":` + strconv.Itoa(read) + `,"cache_creation_input_tokens":` + strconv.Itoa(create) + `}}}`
}

// ledgerTranscript is a session over two quarter-hours: one message written as
// two lines, a subagent's turn and a second model.
func ledgerTranscript(t *testing.T) string {
	t.Helper()
	return writeTranscript(t, strings.Join([]string{
		`{"type":"user","timestamp":"2026-11-20T09:00:30Z","message":{"role":"user","content":"deploy"}}`,
		ledgerLine("msg_A", "2026-11-20T09:01:00Z", "claude-x", false, 100, 50, 1000, 200),
		ledgerLine("msg_A", "2026-11-20T09:01:05Z", "claude-x", false, 100, 50, 1000, 200),
		ledgerLine("msg_S", "2026-11-20T09:20:00Z", "claude-x", true, 7, 3, 0, 0),
		ledgerLine("msg_B", "2026-11-20T09:31:00Z", "claude-y", false, 20, 10, 0, 0),
	}, "\n")+"\n")
}

// ledgerTotals is what the token harvest has always made of ledgerTranscript:
// every message once, the subagent's included.
var ledgerTotals = core.TokenUsage{Input: 127, Cached: 1000, CacheCreation: 200, Output: 63, Total: 1390}

// sessionLedger is a session's ledger rows.
func sessionLedger(t *testing.T, db *sql.DB, id string) []core.UsageBucket {
	t.Helper()
	rows, err := store.SessionUsage(context.Background(), db, id)
	require.NoError(t, err)
	out := make([]core.UsageBucket, len(rows))
	for i, r := range rows {
		out[i] = r.UsageBucket
	}
	return out
}

// A SessionEnd the daemon harvests from the agent's own disk writes the
// ledger from the same pass: the transcript's rows, and the totals the
// harvest always gave, which are their sum.
func TestSessionEnd_TheDaemonsReadWritesTheLedger(t *testing.T) {
	ts, db := newHandlerServer(t)
	const ext = "ledger-read-1"
	_, _ = post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": ext, "cwd": "/work/demo", "source": "startup",
	})
	transcript := ledgerTranscript(t)
	_, _ = post(t, ts.URL+"/api/hooks/session-end", testKey, map[string]any{
		"session_id": ext, "transcript_path": transcript, "reason": "exit",
	})

	sess := requireAmbientSession(t, db, ClientClaudeCode, ext)
	require.Equal(t, core.SessionCompleted, sess.Status)
	require.Equal(t, ledgerTotals, sess.Tokens, "the totals the harvest always gave")
	_, want, ok := harvest.ClaudeUsage(transcript)
	require.True(t, ok)
	require.Len(t, want, 3)
	require.Equal(t, want, sessionLedger(t, db, sess.ID))
	require.Equal(t, core.SumUsage(want), sess.Tokens, "the totals are the ledger's sum")
}

// Under peer_auth `seam hook` pushes the rows beside the totals, and they land
// exactly as the daemon's own read would have put them. A push whose rows do
// not sum to its totals is refused whole: nothing of it lands.
func TestSessionEnd_APushedLedgerLandsAsTheDaemonsReadWould(t *testing.T) {
	e := newPushHooks(t)
	transcript := ledgerTranscript(t)
	totals, rows, ok := harvest.ClaudeUsage(transcript)
	require.True(t, ok)

	e.send("session-start", "", map[string]any{"session_id": "ledger-push-1", "cwd": "/work/ops", "source": "startup"}, nil)
	_, refused := e.send("session-end", "", map[string]any{"session_id": "ledger-push-1", "transcript_path": transcript},
		map[string]any{"version": 1, "findings": "(auto-harvested) Done.", "tokens": totals, "usage": rows})
	require.Empty(t, refused)
	sess := requireAmbientSession(t, e.db, ClientClaudeCode, "ledger-push-1")
	require.Equal(t, ledgerTotals, sess.Tokens)
	require.Equal(t, rows, sessionLedger(t, e.db, sess.ID))

	e.send("session-start", "", map[string]any{"session_id": "ledger-push-2", "cwd": "/work/ops", "source": "startup"}, nil)
	short := totals
	short.Output--
	short.Normalize()
	e.send("session-end", "", map[string]any{"session_id": "ledger-push-2"},
		map[string]any{"version": 1, "findings": "(auto-harvested) Done.", "tokens": short, "usage": rows})
	bad := requireAmbientSession(t, e.db, ClientClaudeCode, "ledger-push-2")
	require.True(t, bad.Tokens.Empty(), "a push that does not validate lands nothing")
	require.Empty(t, sessionLedger(t, e.db, bad.ID))
	require.Empty(t, bad.Findings)
}

// An older `seam` pushes the totals and no rows: the daemon credits them to
// the bucket the end arrived in, under the model the client sniffed, with no
// message or minute it could not know, and the totals land as they always
// did.
func TestSessionEnd_AnOlderClientsTotalsAreCredited(t *testing.T) {
	e := newPushHooks(t)
	e.send("session-start", "", map[string]any{"session_id": "ledger-old-1", "cwd": "/work/ops", "source": "startup"}, nil)
	before := time.Now().UTC()
	e.send("session-end", "", map[string]any{"session_id": "ledger-old-1"},
		map[string]any{"version": 1, "findings": "(auto-harvested) Done.", "tokens": pushTokens, "model": "claude-final"})
	after := time.Now().UTC()

	sess := requireAmbientSession(t, e.db, ClientClaudeCode, "ledger-old-1")
	require.Equal(t, core.TokenUsage{Input: 7, Cached: 100, CacheCreation: 20, Output: 3, Total: 130}, sess.Tokens)
	rows := sessionLedger(t, e.db, sess.ID)
	require.Len(t, rows, 1)
	r := rows[0]
	require.Contains(t, []time.Time{core.UsageBucketStart(before), core.UsageBucketStart(after)}, r.Bucket)
	require.Equal(t, core.UsageBucket{Bucket: r.Bucket, Model: "claude-final", Thread: core.UsageThreadMain,
		Input: 7, Cached: 100, CacheCreation: 20, Output: 3}, r)
}

// Each Codex Stop credits what the rollout's cumulative counter grew by to the
// bucket of the Stop, under the model the Stop stated, as one turn; the same
// counter read again credits nothing, and the totals are the latest counter
// as they always were.
func TestCodexStop_CreditsEachTurnsGrowth(t *testing.T) {
	ts, db := newHandlerServer(t)
	const codexID = "01999999-1111-2222-3333-555555555555"
	_, _ = post(t, ts.URL+"/api/hooks/session-start?client=codex", testKey, map[string]any{
		"session_id": codexID, "cwd": "/work/demo", "source": "startup", "model": "gpt-x",
	})
	stop := func(rollout string) {
		t.Helper()
		_, _ = post(t, ts.URL+"/api/hooks/stop?client=codex", testKey, map[string]any{
			"session_id": codexID, "transcript_path": rollout, "model": "gpt-x", "last_assistant_message": "turn",
		})
	}
	stop(writeRollout(t, tokenCountLine(5000, 4000, 30)))
	grown := writeRollout(t, tokenCountLine(5000, 4000, 30)+"\n"+tokenCountLine(12655, 9600, 75))
	stop(grown)
	stop(grown)

	sess := requireAmbientSession(t, db, ClientCodex, codexID)
	require.Equal(t, core.TokenUsage{Input: 12655 - 9600, Cached: 9600, Output: 75, Total: 12730}, sess.Tokens)
	rows := sessionLedger(t, db, sess.ID)
	require.NotEmpty(t, rows)
	messages := 0
	for _, r := range rows {
		require.Equal(t, "gpt-x", r.Model)
		require.Equal(t, core.UsageThreadMain, r.Thread)
		require.GreaterOrEqual(t, r.ActiveMinutes(), 1, "a turn's row takes its minute")
		require.LessOrEqual(t, r.ActiveMinutes(), r.Messages)
		messages += r.Messages
	}
	require.Equal(t, 2, messages, "two turns grew the counter; the third read nothing new")
	require.Equal(t, sess.Tokens, core.SumUsage(rows))
}

// A moved conversation (isolation.requests) under peer_auth, pushing rows:
// the session the move ended keeps the buckets that ended by the move, the
// child takes the rest, and together they are the transcript's whole usage.
func TestHomePeer_AMovedConversationsLedgerIsSplitAtTheMove(t *testing.T) {
	e := homeNewPushHooks(t, true)
	const ext = "ledger-move-1"
	move := e.clock() // the move is approved at the fake clock's time
	row := func(offset time.Duration, in int) core.UsageBucket {
		return core.UsageBucket{Bucket: core.UsageBucketStart(move.Add(offset)), Model: "claude-x",
			Thread: core.UsageThreadMain, Input: in, Messages: 1, MinuteMask: 1}
	}
	push := func(rows ...core.UsageBucket) map[string]any {
		tok := core.SumUsage(rows)
		return map[string]any{"version": 1, "findings": "(auto-harvested) Done.", "tokens": tok, "usage": rows}
	}
	e.send("session-start", "", map[string]any{"session_id": ext, "cwd": "/work/ops", "source": "startup"}, nil)
	early := row(-30*time.Minute, 100)
	_, refused := e.send("session-end", "", map[string]any{"session_id": ext}, push(early))
	require.Empty(t, refused)
	e.send("session-start", "", map[string]any{"session_id": ext, "cwd": "/work/ops", "source": "resume"}, nil)
	amb := requireAmbientSession(t, e.db, ClientClaudeCode, ext)
	m := homePeerMove(t, e, amb, lkFin, time.Hour)

	before := []core.UsageBucket{early, row(-15*time.Minute, 50)}
	after := []core.UsageBucket{row(0, 20), row(15*time.Minute, 9)}
	_, refused = e.send("session-end", "", map[string]any{"session_id": ext}, push(append(before, after...)...))
	require.Empty(t, refused)

	left := homePeerSession(t, e, m.Left.ID)
	child := homePeerSession(t, e, m.Child.ID)
	require.Equal(t, before, sessionLedger(t, e.db, left.ID), "the parent keeps the buckets before the move")
	require.Equal(t, after, sessionLedger(t, e.db, child.ID), "the child takes those after")
	require.Equal(t, 150, left.Tokens.Total)
	require.Equal(t, 29, child.Tokens.Total)
	require.Equal(t, core.SessionCompleted, left.Status)
	require.True(t, m.Left.UpdatedAt.Equal(left.UpdatedAt), "the parent keeps the time the move ended it at")
}

// The hook body's cap holds a session's rows at the widest a push accepts:
// MaxUsageRows of them, each with the longest model id a push takes and
// counts no transcript reaches, beside the longest findings.
func TestPushedUsage_FitsTheHookBodyCap(t *testing.T) {
	model := strings.Repeat("m", 200)
	require.True(t, harvest.ValidModel(model))
	require.False(t, harvest.ValidModel(model+"m"), "200 bytes is the longest model id a push takes")
	start := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	const big = 1 << 40
	rows := make([]core.UsageBucket, harvest.MaxUsageRows)
	for i := range rows {
		rows[i] = core.UsageBucket{Bucket: start.Add(time.Duration(i) * core.UsageBucketWidth), Model: model,
			Thread: core.UsageThreadSubagent, Input: big, Cached: big, CacheCreation: big, Output: big,
			Messages: big, MinuteMask: core.UsageMinuteMaskAll}
	}
	totals := core.SumUsage(rows)
	wide := string(rune(0x10348)) // a letter outside the BMP: four bytes in UTF-8
	require.Len(t, wide, 4)
	findings := harvest.Prefix + strings.Repeat(wide, harvest.MaxRunes)
	p := harvest.Push{Version: harvest.PushVersion, Model: model, Findings: findings, Tokens: &totals, Usage: rows}
	require.NoError(t, p.Validate())
	body, err := json.Marshal(map[string]any{
		"session_id": strings.Repeat("s", 64), "transcript_path": "/" + strings.Repeat("p", 4096),
		"cwd": "/" + strings.Repeat("c", 4096), "hook_event_name": "SessionEnd", "reason": "prompt_input_exit",
		harvest.Field: p,
	})
	require.NoError(t, err)
	t.Logf("the widest session-end push is %d bytes, the cap %d", len(body), maxHookBody)
	require.Less(t, len(body), maxHookBody*3/4, "room to spare under the hook body's cap")
}

// A session harvested both ways -- read by the daemon, or pushed by `seam
// hook` -- reads the same on every surface the token totals reach: the
// session (the Sessions pill and the session card), the project board and the
// plan rollup, and every one of them reads what the harvest gave before the
// ledger.
func TestUsage_TheTokenSurfacesReadTheSameBothWays(t *testing.T) {
	transcript := ledgerTranscript(t)
	ctx := context.Background()
	surfaces := func(t *testing.T, db *sql.DB, sess core.Session) (core.TokenUsage, int, int) {
		t.Helper()
		_, err := db.ExecContext(ctx, `INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
			VALUES ('01LEDGEREVENT', ?, ?, ?, ?, '', '{"plan_slug":"example-plan"}')`,
			core.FormatTime(time.Now().UTC()), string(core.EventPlanCaptured), sess.ID, sess.ProjectSlug)
		require.NoError(t, err)
		got, _, err := store.SessionByID(ctx, db, sess.ID)
		require.NoError(t, err)
		now := time.Now().UTC()
		board, err := store.ProjectsWithCounts(ctx, db, store.ResolveRetrievalWindow("all", now), now, 0)
		require.NoError(t, err)
		project := 0
		for _, p := range board {
			if p.Project == sess.ProjectSlug {
				project = p.TokensTotal
			}
		}
		plans, err := store.PlanTokenRollups(ctx, db)
		require.NoError(t, err)
		return got.Tokens, project, plans[store.PlanRef{Project: sess.ProjectSlug, Slug: "example-plan"}].Tokens
	}

	read := newPushHooks(t)
	read.off()
	read.send("session-start", "", map[string]any{"session_id": "both-1", "cwd": "/work/ops", "source": "startup"}, nil)
	read.send("session-end", "", map[string]any{"session_id": "both-1", "transcript_path": transcript}, nil)
	readTokens, readBoard, readPlan := surfaces(t, read.db, requireAmbientSession(t, read.db, ClientClaudeCode, "both-1"))

	pushed := newPushHooks(t)
	totals, rows, ok := harvest.ClaudeUsage(transcript)
	require.True(t, ok)
	pushed.send("session-start", "", map[string]any{"session_id": "both-1", "cwd": "/work/ops", "source": "startup"}, nil)
	pushed.send("session-end", "", map[string]any{"session_id": "both-1", "transcript_path": transcript},
		map[string]any{"version": 1, "findings": "(auto-harvested) Done.", "tokens": totals, "usage": rows})
	pushTokens, pushBoard, pushPlan := surfaces(t, pushed.db, requireAmbientSession(t, pushed.db, ClientClaudeCode, "both-1"))

	require.Equal(t, ledgerTotals, readTokens, "the session card's figures")
	require.Equal(t, readTokens, pushTokens)
	require.Equal(t, ledgerTotals.Total, readBoard, "the project board's")
	require.Equal(t, readBoard, pushBoard)
	require.Equal(t, ledgerTotals.Total, readPlan, "the plan rollup's")
	require.Equal(t, readPlan, pushPlan)
}

// The agent closed its own session with session_end before Claude Code's
// SessionEnd hook ran. The hook's token harvest still lands on that completed
// row: it is not revived, the agent's own findings stand, and its end time
// does not move.
func TestSessionEnd_AfterSessionEndStillHarvestsTokens(t *testing.T) {
	ts, db := newHandlerServer(t)
	ctx := context.Background()
	const ext = "ended-first-1"
	_, _ = post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": ext, "cwd": "/work/demo", "source": "startup",
	})

	// session_end, as the MCP tool writes it.
	ended := requireAmbientSession(t, db, ClientClaudeCode, ext)
	ended.Status = core.SessionCompleted
	ended.Findings = "Shipped the example deploy."
	ended.UpdatedAt = time.Now().UTC().Add(-time.Minute)
	require.NoError(t, store.UpdateSession(ctx, db, ended))
	endedAt := requireAmbientSession(t, db, ClientClaudeCode, ext).UpdatedAt

	transcript := writeTranscript(t, strings.Join([]string{
		aLine("msg_A", 100, 50, 1000, 200),
		aLine("msg_B", 20, 10, 0, 0),
	}, "\n"))
	_, _ = post(t, ts.URL+"/api/hooks/session-end", testKey, map[string]any{
		"session_id": ext, "transcript_path": transcript, "reason": "exit",
	})

	got := requireAmbientSession(t, db, ClientClaudeCode, ext)
	require.Equal(t, core.TokenUsage{Input: 120, Cached: 1000, CacheCreation: 200, Output: 60, Total: 1380}, got.Tokens,
		"the end's harvest lands on the row the agent completed")
	require.Equal(t, core.SessionCompleted, got.Status, "never revived")
	require.Equal(t, "Shipped the example deploy.", got.Findings, "the agent's own findings stand")
	require.True(t, endedAt.Equal(got.UpdatedAt), "the end time does not move: %s, then %s", endedAt, got.UpdatedAt)
}

// The idle reaper expired the session while its conversation stayed open (the
// owner stepped away); a later prompt heartbeats nothing, so the row is still
// expired when SessionEnd runs. The harvest lands there too, without reviving
// it.
func TestSessionEnd_AfterTheReaperStillHarvestsTokens(t *testing.T) {
	ts, db := newHandlerServer(t)
	ctx := context.Background()
	const ext = "reaped-first-1"
	_, _ = post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": ext, "cwd": "/work/demo", "source": "startup",
	})
	reaped, err := store.ExpireIdleSessions(ctx, db, time.Now().UTC().Add(time.Hour), time.Minute)
	require.NoError(t, err)
	require.Len(t, reaped, 1)
	expiredAt := requireAmbientSession(t, db, ClientClaudeCode, ext).UpdatedAt

	transcript := writeTranscript(t, aLine("msg_A", 100, 50, 1000, 200))
	_, _ = post(t, ts.URL+"/api/hooks/session-end", testKey, map[string]any{
		"session_id": ext, "transcript_path": transcript, "reason": "exit",
	})

	got := requireAmbientSession(t, db, ClientClaudeCode, ext)
	require.Equal(t, 1350, got.Tokens.Total, "the end's harvest lands on the row the reaper expired")
	require.Equal(t, core.SessionExpired, got.Status, "never revived")
	require.True(t, expiredAt.Equal(got.UpdatedAt), "the row's time does not move")
}
