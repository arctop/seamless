package hooks

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// clHookTask creates an open task in project demo and claims it for session at
// at with lease; it returns the task id.
func clHookTask(t *testing.T, db *sql.DB, session string, lease time.Duration, at time.Time) string {
	t.Helper()
	ctx := context.Background()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: id, ProjectSlug: "demo", Title: "held work",
		Status: core.TaskOpen, CreatedAt: at, UpdatedAt: at}))
	_, err = store.ClaimTask(ctx, db, id, session, lease, at)
	require.NoError(t, err)
	return id
}

// The next prompt's injection leads with the claims the conversation lost --
// its ambient session's and those of an explicit session linked to it -- and
// the prompt after that carries none of them.
func TestClaimNotice_PromptInjectionLeadsWithItOnce(t *testing.T) {
	ts, db := newHandlerServer(t)
	ctx := context.Background()
	_, _ = post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": "cl-conv-1", "cwd": "/work/demo", "source": "startup"})
	ambient := requireAmbientSession(t, db, ClientClaudeCode, "cl-conv-1")
	now := time.Now().UTC()
	linked, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: linked, Name: "sess/linked", ProjectSlug: "demo",
		Status: core.SessionActive, ExternalClient: "claude-code", ExternalSessionID: "cl-conv-1",
		CreatedAt: now, UpdatedAt: now}))

	stolen := clHookTask(t, db, ambient.ID, time.Minute, now.Add(-2*time.Hour))
	_, err = store.ClaimTask(ctx, db, stolen, "01SOMEONEELSEAAAAAAAAAAAAA", time.Hour, now)
	require.NoError(t, err)
	released := clHookTask(t, db, linked, time.Hour, now)
	_, err = store.ForceReleaseTask(ctx, db, released, now)
	require.NoError(t, err)

	url := ts.URL + "/api/hooks/user-prompt-submit"
	_, out := post(t, url, testKey, map[string]any{"session_id": "cl-conv-1", "cwd": "/work/demo",
		"user_prompt": "weather in paris"})
	ac := additionalContext(t, out)
	require.True(t, strings.HasPrefix(ac, "<seam-briefing>\nSeamless: Claim lost: "), ac)
	require.Contains(t, ac, "your claim on task "+stolen+" in project demo lapsed at ")
	require.Contains(t, ac, "and another session claimed it at ", "a holder that names no session is never named")
	require.Contains(t, ac, "the owner released your claim on task "+released+" in project demo at ")

	_, out = post(t, url, testKey, map[string]any{"session_id": "cl-conv-1", "cwd": "/work/demo",
		"user_prompt": "weather in paris"})
	require.Empty(t, additionalContext(t, out), "told once")
}

// A session start carries the notice at the head of its briefing -- the
// session the reaper expired, its lapsed claim released, coming back -- and a
// subagent's start leaves it for the main session.
func TestClaimNotice_SessionStartBriefingLeadsWithIt(t *testing.T) {
	ts, db := newHandlerServer(t)
	ctx := context.Background()
	url := ts.URL + "/api/hooks/session-start"
	_, _ = post(t, url, testKey, map[string]any{"session_id": "cl-conv-2", "cwd": "/work/demo", "source": "startup"})
	ambient := requireAmbientSession(t, db, ClientClaudeCode, "cl-conv-2")
	now := time.Now().UTC()
	task := clHookTask(t, db, ambient.ID, time.Minute, now.Add(-2*time.Hour))
	n, err := store.ReleaseReapedClaims(ctx, db, ambient.ID, now)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	_, out := post(t, url, testKey, map[string]any{"session_id": "cl-conv-2", "cwd": "/work/demo",
		"agent_type": "Explore"})
	require.NotContains(t, additionalContext(t, out), "Claim lost", "a subagent's start is not the session's")

	_, out = post(t, url, testKey, map[string]any{"session_id": "cl-conv-2", "cwd": "/work/demo", "source": "resume"})
	ac := additionalContext(t, out)
	require.True(t, strings.HasPrefix(ac, "<seam-briefing>\nSeamless: Claim lost: your claim on task "+task+
		" in project demo lapsed at "), ac)
	require.Contains(t, ac, "when this session had gone idle")
	require.Contains(t, ac, "- no-force-push", "the briefing follows")

	_, out = post(t, url, testKey, map[string]any{"session_id": "cl-conv-2", "cwd": "/work/demo", "source": "resume"})
	require.NotContains(t, additionalContext(t, out), "Claim lost")
}
