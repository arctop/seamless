package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
)

// subagentTranscript is a session transcript where Claude Code keeps one, with
// one subagent's transcript beside it in its subagents folder: the layout of
// current Claude Code, whose session transcript holds no sidechain line.
func subagentTranscript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	transcript := filepath.Join(dir, "5f0e7a2c-ledger.jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(strings.Join([]string{
		`{"type":"user","timestamp":"2026-11-20T09:00:30Z","message":{"role":"user","content":"deploy"}}`,
		ledgerLine("msg_A", "2026-11-20T09:01:00Z", "claude-x", false, 100, 50, 1000, 200),
		ledgerLine("msg_B", "2026-11-20T09:31:00Z", "claude-x", false, 20, 10, 0, 0),
	}, "\n")+"\n"), 0o600))
	subagents := filepath.Join(dir, "5f0e7a2c-ledger", "subagents")
	require.NoError(t, os.MkdirAll(subagents, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(subagents, "agent-a1.jsonl"), []byte(strings.Join([]string{
		ledgerLine("msg_S1", "2026-11-20T09:05:00Z", "claude-small", true, 7, 3, 300, 0),
		ledgerLine("msg_S2", "2026-11-20T09:18:00Z", "claude-small", true, 1, 1, 0, 0),
	}, "\n")+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(subagents, "agent-a1.meta.json"), []byte(`{"agentType":"Explore"}`), 0o600))
	return transcript
}

// subagentTotals is subagentTranscript's usage: the session's own messages and
// its subagent's, each once.
var subagentTotals = core.TokenUsage{Input: 128, Cached: 1300, CacheCreation: 200, Output: 64, Total: 1692}

// The daemon's own read at SessionEnd counts the subagents' transcripts
// against the session that launched them: the totals include their burn, and
// the ledger keeps it under the subagent thread.
func TestSessionEnd_TheDaemonsReadCountsSubagentTranscripts(t *testing.T) {
	ts, db := newHandlerServer(t)
	const ext = "ledger-subagent-read-1"
	_, _ = post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": ext, "cwd": "/work/demo", "source": "startup",
	})
	transcript := subagentTranscript(t)
	_, _ = post(t, ts.URL+"/api/hooks/session-end", testKey, map[string]any{
		"session_id": ext, "transcript_path": transcript, "reason": "exit",
	})

	sess := requireAmbientSession(t, db, ClientClaudeCode, ext)
	require.Equal(t, core.SessionCompleted, sess.Status)
	require.Equal(t, subagentTotals, sess.Tokens, "the subagent's burn is the session's")
	rows := sessionLedger(t, db, sess.ID)
	_, want, ok := harvest.ClaudeUsage(transcript)
	require.True(t, ok)
	require.Equal(t, want, rows)
	var sub core.TokenUsage
	for _, r := range rows {
		if r.Thread == core.UsageThreadSubagent {
			require.Equal(t, "claude-small", r.Model)
			sub.Input += r.Input
			sub.Cached += r.Cached
			sub.Output += r.Output
		}
	}
	sub.Normalize()
	require.Equal(t, core.TokenUsage{Input: 8, Cached: 300, Output: 4, Total: 312}, sub, "the subagent thread is its transcript's")
	require.Equal(t, core.SumUsage(rows), sess.Tokens)
}

// The agent ended its own session with session_end before Claude Code's
// SessionEnd hook ran: the end's harvest onto the ended row counts the
// subagents too, from the same parser.
func TestSessionEnd_AfterSessionEndCountsSubagentTranscripts(t *testing.T) {
	ts, db := newHandlerServer(t)
	const ext = "ledger-subagent-after-1"
	_, _ = post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": ext, "cwd": "/work/demo", "source": "startup",
	})
	ended := requireAmbientSession(t, db, ClientClaudeCode, ext)
	ended.Status = core.SessionCompleted
	ended.Findings = "Shipped the example deploy."
	require.NoError(t, store.UpdateSession(context.Background(), db, ended))

	_, _ = post(t, ts.URL+"/api/hooks/session-end", testKey, map[string]any{
		"session_id": ext, "transcript_path": subagentTranscript(t), "reason": "exit",
	})
	got := requireAmbientSession(t, db, ClientClaudeCode, ext)
	require.Equal(t, core.SessionCompleted, got.Status)
	require.Equal(t, subagentTotals, got.Tokens)
}

// Under peer_auth the daemon reads nothing: the push `seam hook` builds with
// the same parser carries the subagent rows, and they land as the daemon's own
// read would have put them.
func TestSessionEnd_APushedLedgerCarriesSubagentRows(t *testing.T) {
	e := newPushHooks(t)
	transcript := subagentTranscript(t)
	totals, rows, ok := harvest.ClaudeUsage(transcript)
	require.True(t, ok)
	require.Equal(t, subagentTotals, totals)

	e.send("session-start", "", map[string]any{"session_id": "ledger-subagent-push-1", "cwd": "/work/ops", "source": "startup"}, nil)
	_, refused := e.send("session-end", "", map[string]any{"session_id": "ledger-subagent-push-1", "transcript_path": "/elsewhere.jsonl"},
		map[string]any{"version": 1, "findings": "(auto-harvested) Done.", "tokens": totals, "usage": rows})
	require.Empty(t, refused)
	sess := requireAmbientSession(t, e.db, ClientClaudeCode, "ledger-subagent-push-1")
	require.Equal(t, subagentTotals, sess.Tokens)
	require.Equal(t, rows, sessionLedger(t, e.db, sess.ID))
}
