package main

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// Claude Code's own UserPromptSubmit body (its 2.1.330's fields, the prompt
// under "prompt") through `seam hook` over the peer_auth socket: the daemon
// reads the prompt -- here a miss, recorded as hook.prompt under the session
// -- and heartbeats the session.
func TestRunHook_ClaudeCodesOwnPromptReachesTheDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	f := newCaptureFixture(t)
	ctx := context.Background()
	d := newSocketDaemon(t, f, ownerSocketView(t))
	require.Empty(t, runHookCall(t, d.cfg, hookCall{event: "session-start",
		payload: map[string]any{"session_id": "cc-own", "cwd": f.cwd, "source": "startup"}}))
	sess := d.session(t, "claude-code", "cc-own")
	stale := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	sess.UpdatedAt = stale
	require.NoError(t, store.UpdateSession(ctx, d.db, sess))

	const prompt = "is the weather in paris any good"
	require.Empty(t, runHookCall(t, d.cfg, hookCall{event: "user-prompt-submit", payload: map[string]any{
		"session_id": "cc-own", "transcript_path": f.transcript, "cwd": f.cwd, "permission_mode": "default",
		"hook_event_name": "UserPromptSubmit", "prompt": prompt, "session_title": "Weather"}}))
	evs, err := events.NewRecorder(d.db).ByKinds(ctx, []core.EventKind{core.EventHookPrompt}, "", "", 50)
	require.NoError(t, err)
	read := false
	for _, ev := range evs {
		if ev.SessionID == sess.ID && ev.Payload["prompt"] == prompt {
			read = true
		}
	}
	require.True(t, read, "the daemon read the prompt")
	require.True(t, d.session(t, "claude-code", "cc-own").UpdatedAt.After(stale), "and heartbeat the session")
}
