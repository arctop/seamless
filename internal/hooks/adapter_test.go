package hooks

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

const currentCodexFixtureVersion = "v0.144.6"

// codexFixture reads a committed Codex hook-contract fixture. The adapter is
// pinned to the current live capture, not hand-written JSON; older contracts stay
// versioned beside it for traceability.
func codexFixture(t *testing.T, frontend, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "codex", currentCodexFixtureVersion, frontend, name))
	require.NoError(t, err)
	return b
}

// The Codex SessionStart payload shares every field this hook reads with Claude
// Code, so the identity decode must surface them and yield the cx/ ambient name.
func TestDecodeSessionStart_CodexFixtures(t *testing.T) {
	for frontend, wantID := range map[string]string{
		"exec": "019f8000-0000-7000-8000-000000000001",
		"tui":  "019f8000-0010-7000-8000-000000000011",
	} {
		t.Run(frontend, func(t *testing.T) {
			p := decodeSessionStart(ClientCodex, codexFixture(t, frontend, "session-start.input.json"))

			require.Equal(t, wantID, p.SessionID)
			require.Equal(t, "/Users/dev/myrepo", p.CWD)
			require.Equal(t, "startup", p.Source)
			require.Equal(t, "gpt-5.6-sol", p.Model)
			require.Contains(t, p.TranscriptPath, "rollout-2026-07-19")
			require.Empty(t, p.AgentType, "a top-level Codex session is not a subagent")
		})
	}

	require.Equal(t, "cx/019f8000-6f2e1e624e1ad9fa",
		ambientName(ClientCodex, "019f8000-0000-7000-8000-000000000001"),
		"Codex ambient display names keep a readable prefix plus a full-id digest")
}

// Codex names the submitted prompt `prompt`, as Claude Code does: decodePrompt
// must land the Codex fixture's in the internal UserPrompt field so downstream
// recall is shared code.
func TestDecodePrompt_CodexFixturesNormalizePromptField(t *testing.T) {
	for frontend, wantID := range map[string]string{
		"exec": "019f8000-0000-7000-8000-000000000001",
		"tui":  "019f8000-0010-7000-8000-000000000011",
	} {
		t.Run(frontend, func(t *testing.T) {
			p := decodePrompt(ClientCodex, codexFixture(t, frontend, "user-prompt-submit.input.json"))

			require.Contains(t, p.UserPrompt, "SEAMLESS_CONTRACT_",
				"Codex `prompt` must land in UserPrompt")
			require.Equal(t, wantID, p.SessionID)
			require.Equal(t, "/Users/dev/myrepo", p.CWD)
			require.Equal(t, "UserPromptSubmit", p.HookEventName)
			require.Equal(t, "gpt-5.6-sol", p.Model)
		})
	}
}

// Claude Code names the submitted prompt `prompt` too (its 2.1.330 builds the
// hook input with it), and decodePrompt reads it into UserPrompt; a body that
// carries the internal name, `user_prompt` -- as the repository's tests and a
// hand-written POST do -- is read as it always was.
func TestDecodePrompt_ClaudeCodeReadsItsPromptField(t *testing.T) {
	p := decodePrompt(ClientClaudeCode, []byte(`{"session_id":"abc","cwd":"/w","user_prompt":"cc words"}`))
	require.Equal(t, "cc words", p.UserPrompt)

	p = decodePrompt(ClientClaudeCode, []byte(`{"session_id":"abc","cwd":"/w","hook_event_name":"UserPromptSubmit",`+
		`"prompt":"cc words","session_title":"Example"}`))
	require.Equal(t, "cc words", p.UserPrompt)
}

// If a body somehow carries both fields, the internal user_prompt wins: the
// `prompt` fallback only fills an empty UserPrompt, so it can never clobber a
// value that decoded straight into the internal shape.
func TestDecodePrompt_PromptFallbackDoesNotClobberUserPrompt(t *testing.T) {
	for _, client := range []Client{ClientClaudeCode, ClientCodex} {
		p := decodePrompt(client, []byte(`{"user_prompt":"already","prompt":"other"}`))
		require.Equal(t, "already", p.UserPrompt, client)
	}
}

// The current Codex fixtures prove that both subagent events carry the parent
// session_id plus a distinct child agent_id. The adapter retains the turn/model/
// permission fields, the event-specific rollout paths, and Stop's stable final
// message without inferring anything from Claude Code payloads.
func TestDecodeSubagent_CodexFixtures(t *testing.T) {
	for _, tt := range []struct {
		frontend       string
		parentID       string
		turnID         string
		agentID        string
		permissionMode string
	}{
		{
			frontend: "exec", parentID: "019f8000-0000-7000-8000-000000000001",
			turnID:  "019f8000-0003-7000-8000-000000000004",
			agentID: "019f8000-0002-7000-8000-000000000003", permissionMode: "bypassPermissions",
		},
		{
			frontend: "tui", parentID: "019f8000-0010-7000-8000-000000000011",
			turnID:  "019f8000-0013-7000-8000-000000000014",
			agentID: "019f8000-0012-7000-8000-000000000013", permissionMode: "default",
		},
	} {
		t.Run(tt.frontend, func(t *testing.T) {
			start := decodeSubagentStart(ClientCodex,
				codexFixture(t, tt.frontend, "subagent-start.input.json"))
			require.Equal(t, tt.parentID, start.ParentSessionID)
			require.Equal(t, tt.turnID, start.TurnID)
			require.Equal(t, tt.agentID, start.AgentID)
			require.Equal(t, "default", start.AgentType)
			require.Equal(t, "/Users/dev/myrepo", start.CWD)
			require.Equal(t, "gpt-5.6-sol", start.Model)
			require.Equal(t, tt.permissionMode, start.PermissionMode)
			require.Equal(t, "SubagentStart", start.HookEventName)
			require.Contains(t, start.TranscriptPath, tt.agentID,
				"SubagentStart transcript_path is the child rollout")

			stop := decodeSubagentStop(ClientCodex,
				codexFixture(t, tt.frontend, "subagent-stop.input.json"))
			require.Equal(t, tt.parentID, stop.ParentSessionID)
			require.Equal(t, tt.turnID, stop.TurnID)
			require.Equal(t, tt.agentID, stop.AgentID)
			require.Equal(t, "default", stop.AgentType)
			require.Equal(t, "/Users/dev/myrepo", stop.CWD)
			require.Equal(t, "gpt-5.6-sol", stop.Model)
			require.Equal(t, tt.permissionMode, stop.PermissionMode)
			require.Equal(t, "SubagentStop", stop.HookEventName)
			require.NotEqual(t, stop.TranscriptPath, stop.AgentTranscriptPath)
			require.Equal(t, start.TranscriptPath, stop.AgentTranscriptPath,
				"SubagentStop explicitly names the child rollout")
			require.Equal(t, "SUBAGENT_CONTRACT_DONE", stop.LastAssistantMessage)
			require.False(t, stop.StopHookActive)
		})
	}
}

func TestDecodeSubagentStop_ClaudeCodeKeepsPlanCaptureFields(t *testing.T) {
	p := decodeSubagentStop(ClientClaudeCode, []byte(`{
  "session_id":"cc-parent","transcript_path":"/tmp/parent.jsonl","cwd":"/work/demo",
  "permission_mode":"plan","hook_event_name":"SubagentStop",
  "agent_id":"child-1","agent_type":"Explore"
}`))
	require.Equal(t, "cc-parent", p.ParentSessionID)
	require.Equal(t, "/tmp/parent.jsonl", p.TranscriptPath)
	require.Equal(t, "plan", p.PermissionMode)
	require.Equal(t, "child-1", p.AgentID)
	require.Equal(t, "Explore", p.AgentType)
	require.Empty(t, p.AgentTranscriptPath)
	require.Empty(t, p.LastAssistantMessage)
}

// The discriminator rides on ?client=, not the body. Absence alone defaults to
// Claude Code; every present value must be canonical.
func TestClientFromRequest(t *testing.T) {
	mk := func(query string) *http.Request {
		return httptest.NewRequest(http.MethodPost, "/api/hooks/x"+query, nil)
	}

	for _, tt := range []struct {
		name  string
		query string
		want  Client
	}{
		{name: "missing defaults to Claude Code", want: ClientClaudeCode},
		{name: "explicit Claude Code", query: "?client=claude-code", want: ClientClaudeCode},
		{name: "explicit Codex", query: "?client=codex", want: ClientCodex},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := clientFromRequest(mk(tt.query))
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}

	for _, query := range []string{
		"?client=gemini",
		"?client=",
		"?client=codex&client=claude-code",
		"?client=gemini;extra=x",
	} {
		t.Run(query, func(t *testing.T) {
			_, err := clientFromRequest(mk(query))
			require.Error(t, err)
			require.Contains(t, err.Error(), "valid values are claude-code, codex")
		})
	}
}

// Every authenticated hook route validates the discriminator before decoding
// its body. Invalid requests therefore share one 400 contract and cannot create
// the cc/ session the old fallback produced.
func TestHookRoutes_InvalidClientReturnsBadRequestWithoutMutation(t *testing.T) {
	ts, db := newHandlerServer(t)
	var changesBefore int64
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT total_changes()`).Scan(&changesBefore))
	paths := []string{
		"session-start",
		"user-prompt-submit",
		"session-end",
		"stop",
		"subagent-start",
		"post-tool-use",
		"subagent-stop",
		"permission-request",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			resp, _ := post(t, ts.URL+"/api/hooks/"+path+"?client=gemini", testKey, map[string]any{
				"session_id": "invalid-client-session", "cwd": "/work/invalid",
				"prompt": "must not be discarded", "last_assistant_message": "must not be harvested",
			})
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Contains(t, string(body), `invalid hook client "gemini"`)
			require.Contains(t, string(body), "valid values are claude-code, codex")
		})
	}

	var sessions, hookEvents int
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sessions`).Scan(&sessions))
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM events`).Scan(&hookEvents))
	require.Zero(t, sessions, "invalid SessionStart must not create a Claude ambient row")
	require.Zero(t, hookEvents, "invalid hook requests must not record events")
	var changesAfter int64
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT total_changes()`).Scan(&changesAfter))
	require.Equal(t, changesBefore, changesAfter, "invalid hook requests must not mutate any database row")
}

// A typo beside a Codex-shaped prompt must fail visibly instead of selecting
// Claude's adapter, dropping `prompt`, and returning an apparent empty success.
// Stop is included to pin the no-heartbeat/model/findings-mutation side of the
// same boundary.
func TestInvalidClient_CodexPromptAndStopDoNotMutateSession(t *testing.T) {
	ts, db := newHandlerServer(t)
	ctx := context.Background()
	const sessionID = "019f7291-40f1-7311-8997-0d497579d27b"

	resp, _ := post(t, ts.URL+"/api/hooks/session-start?client=codex", testKey, map[string]any{
		"session_id": sessionID, "cwd": "/work/demo", "source": "startup", "model": "gpt-before",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	before := requireAmbientSession(t, db, ClientCodex, sessionID)
	var eventsBefore int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&eventsBefore))

	resp, _ = post(t, ts.URL+"/api/hooks/user-prompt-submit?client=codxe", testKey, map[string]any{
		"session_id": sessionID, "cwd": "/work/demo", "prompt": "why does chroma fail",
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp, _ = post(t, ts.URL+"/api/hooks/stop?client=codxe", testKey, map[string]any{
		"session_id": sessionID, "model": "gpt-after", "last_assistant_message": "must not land",
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	after := requireAmbientSession(t, db, ClientCodex, sessionID)
	require.Equal(t, before.UpdatedAt, after.UpdatedAt, "invalid requests must not heartbeat")
	require.Equal(t, before.Model, after.Model, "invalid requests must not update model attribution")
	require.Equal(t, before.Findings, after.Findings, "invalid Stop must not harvest findings")
	var eventsAfter int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&eventsAfter))
	require.Equal(t, eventsBefore, eventsAfter, "invalid requests must not record prompt or injection events")
}

// End to end through the real handler: a Codex SessionStart names a cx/ ambient
// session and injects the briefing, and a Codex UserPromptSubmit whose prompt
// arrives under the `prompt` key still fires recall -- proving the ?client=codex
// query param selects the adapter and everything downstream is shared with CC.
func TestCodexHooks_EndToEnd(t *testing.T) {
	ts, db := newHandlerServer(t)
	ctx := context.Background()
	rec := events.NewRecorder(db)
	codexID := "019f7291-40f1-7311-8997-0d497579d27b"

	_, out := post(t, ts.URL+"/api/hooks/session-start?client=codex", testKey, map[string]any{
		"session_id": codexID, "cwd": "/work/demo", "source": "startup",
	})
	ac := additionalContext(t, out)
	require.Contains(t, ac, "<seam-briefing>")
	codexName := ambientName(ClientCodex, codexID)
	require.Contains(t, ac, "Seam session: "+codexName+" (ambient)",
		"a Codex session gets a cx/ ambient line, not cc/")

	sess, ok, err := store.AmbientSessionByExternalIdentity(ctx, db, "codex", codexID)
	require.NoError(t, err)
	require.True(t, ok, "SessionStart created the cx/ ambient session")
	require.Equal(t, codexID, sess.ExternalSessionID)

	// Codex sends the prompt as `prompt`; a matching one must still return recall.
	_, out = post(t, ts.URL+"/api/hooks/user-prompt-submit?client=codex", testKey, map[string]any{
		"session_id": codexID, "cwd": "/work/demo",
		"prompt": "why does the chroma container fail its health check",
	})
	require.Contains(t, additionalContext(t, out), "chroma-boot-race",
		"the `prompt` field was normalized, so recall fired")

	inj := eventsOfKind(t, rec, core.EventInjected)
	require.Equal(t, "user-prompt-submit", inj[0].Payload["hook"])
	require.Equal(t, sess.ID, inj[0].SessionID,
		"the prompt injection is stamped with the cx/ ambient session")
}

// The Stop lifecycle for Codex: each Stop harvests the turn's last agent message
// onto the ambient session's findings, so repeated Stops converge on the latest
// turn, an empty turn leaves the prior harvest intact, and the session stays
// active (the idle reaper -- not Stop -- ends it).
func TestCodexStopHook_ConvergesFindings(t *testing.T) {
	ts, db := newHandlerServer(t)
	ctx := context.Background()
	codexID := "019f7291-40f1-7311-8997-0d497579d27b"

	// Start the cx/ ambient session so Stop has something to harvest onto.
	_, _ = post(t, ts.URL+"/api/hooks/session-start?client=codex", testKey, map[string]any{
		"session_id": codexID, "cwd": "/work/demo", "source": "startup",
	})

	stopURL := ts.URL + "/api/hooks/stop?client=codex"
	findings := func() string {
		s, ok, err := store.AmbientSessionByExternalIdentity(ctx, db, "codex", codexID)
		require.NoError(t, err)
		require.True(t, ok)
		return s.Findings
	}

	// First turn's Stop harvests its last message; the response is a bare ack.
	resp, out := post(t, stopURL, testKey, map[string]any{
		"session_id": codexID, "last_assistant_message": "first turn summary",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, true, out["continue"])
	require.Nil(t, out["hookSpecificOutput"], "Stop cannot inject -- no hookSpecificOutput")
	require.Equal(t, "(auto-harvested) first turn summary", findings())

	// A later turn overwrites: findings converge on the most recent message.
	_, _ = post(t, stopURL, testKey, map[string]any{
		"session_id": codexID, "last_assistant_message": "second turn summary",
	})
	require.Equal(t, "(auto-harvested) second turn summary", findings())

	// A turn with nothing to harvest (no payload message, no transcript) leaves the
	// prior harvest intact rather than blanking it.
	_, _ = post(t, stopURL, testKey, map[string]any{"session_id": codexID})
	require.Equal(t, "(auto-harvested) second turn summary", findings())

	// Stop never ends the session; the reaper does. It is still active.
	s, ok, err := store.AmbientSessionByExternalIdentity(ctx, db, "codex", codexID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionActive, s.Status)
}

// A Stop for Claude Code (the default client) only heartbeats: it must not harvest
// findings, since CC ends via its own SessionEnd path. This keeps the CC end path
// untouched even though CC installs no Stop hook of its own.
func TestStopHook_ClaudeCodeOnlyHeartbeats(t *testing.T) {
	ts, db := newHandlerServer(t)
	ctx := context.Background()
	ccID := "abcdef12-3456"

	_, _ = post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": ccID, "cwd": "/work/demo", "source": "startup",
	})

	// A CC-shaped Stop carrying a last message must NOT write findings.
	_, _ = post(t, ts.URL+"/api/hooks/stop", testKey, map[string]any{
		"session_id": ccID, "last_assistant_message": "should not be harvested",
	})
	s, ok, err := store.AmbientSessionByExternalIdentity(ctx, db, "claude-code", ccID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Empty(t, s.Findings, "a Claude Code Stop only heartbeats -- CC harvests on SessionEnd")
}
