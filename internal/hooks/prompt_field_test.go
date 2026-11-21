package hooks

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// claudeCodePromptBody is a UserPromptSubmit body with the fields Claude Code
// 2.1.330 sends: the common hook fields, hook_event_name, the prompt under
// "prompt", and session_title.
func claudeCodePromptBody(id, prompt string) map[string]any {
	return map[string]any{"session_id": id, "transcript_path": "", "cwd": "/work/demo", "permission_mode": "default",
		"hook_event_name": "UserPromptSubmit", "prompt": prompt, "session_title": "Fix the chroma boot race"}
}

// A prompt in Claude Code's own body reaches the prompt path: a matching one
// injects the recall block, recorded with its prompt under the session; a
// miss is recorded as hook.prompt; and each heartbeats the ambient session.
func TestUserPromptSubmit_ClaudeCodesOwnBody(t *testing.T) {
	ts, db := newHandlerServer(t)
	ctx := context.Background()
	rec := events.NewRecorder(db)
	_, _ = post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": "cc-own-1", "cwd": "/work/demo", "source": "startup",
	})
	sess := requireAmbientSession(t, db, ClientClaudeCode, "cc-own-1")
	stale := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	sess.UpdatedAt = stale
	require.NoError(t, store.UpdateSession(ctx, db, sess))

	const match = "why does the chroma container fail its health check"
	_, out := post(t, ts.URL+"/api/hooks/user-prompt-submit", testKey, claudeCodePromptBody("cc-own-1", match))
	ac := additionalContext(t, out)
	require.Contains(t, ac, "<seam-recall>")
	require.Contains(t, ac, "chroma-boot-race")
	injected := false
	for _, ev := range eventsOfKind(t, rec, core.EventInjected) {
		if ev.SessionID == sess.ID && payloadString(ev.Payload["hook"]) == "user-prompt-submit" {
			require.Equal(t, match, payloadString(ev.Payload["prompt"]))
			injected = true
		}
	}
	require.True(t, injected, "the injection is recorded with its prompt, under the session")
	require.True(t, requireAmbientSession(t, db, ClientClaudeCode, "cc-own-1").UpdatedAt.After(stale),
		"the prompt heartbeats the session")

	_, out = post(t, ts.URL+"/api/hooks/user-prompt-submit", testKey, claudeCodePromptBody("cc-own-1", "weather in paris"))
	require.Empty(t, additionalContext(t, out))
	missed := false
	for _, ev := range eventsOfKind(t, rec, core.EventHookPrompt) {
		if ev.SessionID == sess.ID && payloadString(ev.Payload["prompt"]) == "weather in paris" {
			missed = true
		}
	}
	require.True(t, missed, "a miss is recorded as hook.prompt")
}
