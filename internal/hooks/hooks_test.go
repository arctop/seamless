package hooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const testKey = "hook-test-key"

func newHandlerServer(t *testing.T) (*httptest.Server, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	require.NoError(t, store.SetSetting(context.Background(), db,
		store.SettingRepoProjectMap, `{"/work/demo":"demo"}`))
	// A constraint and a gotcha in project "demo" for the briefing/prompt matcher.
	insertMemory(t, db, "01A", "constraint", "no-force-push", "never force push to main", "demo")
	insertMemory(t, db, "01B", "gotcha", "chroma-boot-race", "chroma container health check startup race", "demo")
	insertMemory(t, db, "01C", "decision", "ulid-over-uuid", "use ulid identifiers not uuid values", "demo")
	insertMemory(t, db, "01D", "reference", "sqlite-wal", "enable wal journal mode and busy timeout", "demo")

	ret := retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	h := NewHandler(Config{DB: db, Retrieve: ret, Events: events.NewRecorder(db), APIKey: testKey})
	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, db
}

func insertMemory(t *testing.T, db *sql.DB, id, kind, name, desc, project string) {
	t.Helper()
	ctx := context.Background()
	now := core.FormatTime(time.Now().UTC())
	_, err := db.ExecContext(ctx, `
		INSERT INTO memories_index
		    (id, kind, name, description, project, file_path, tags, valid_from,
		     invalid_at, superseded_by, source_session, content_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '[]', ?, NULL, NULL, '', 'h', ?, ?)`,
		id, kind, name, desc, project, "memory/x/"+name+".md", now, now, now)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO fts (item_id, kind, project, title, name, description, body)
		VALUES (?, 'memory', ?, '', ?, ?, ?)`, id, project, name, desc, desc)
	require.NoError(t, err)
}

func post(t *testing.T, url, key string, body any) (*http.Response, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(b)))
	require.NoError(t, err)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	var out map[string]any
	if resp.StatusCode == http.StatusOK {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	}
	return resp, out
}

func additionalContext(t *testing.T, out map[string]any) string {
	t.Helper()
	hso, ok := out["hookSpecificOutput"].(map[string]any)
	require.True(t, ok, "missing hookSpecificOutput: %v", out)
	s, _ := hso["additionalContext"].(string)
	return s
}

func requireAmbientSession(t *testing.T, db *sql.DB, client Client, externalSessionID string) core.Session {
	t.Helper()
	sess, ok, err := store.AmbientSessionByExternalIdentity(
		context.Background(), db, client.externalIdentity(), externalSessionID)
	require.NoError(t, err)
	require.True(t, ok, "ambient session %s/%s must exist", client, externalSessionID)
	return sess
}

func installedEvents(t *testing.T, client Client) []string {
	t.Helper()
	events, err := InstalledEvents(client)
	require.NoError(t, err)
	return events
}

func TestSessionStartHook(t *testing.T) {
	ts, db := newHandlerServer(t)
	url := ts.URL + "/api/hooks/session-start"

	// Bad key -> 401.
	resp, _ := post(t, url, "nope", map[string]any{"cwd": "/work/demo"})
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Good key -> briefing in additionalContext.
	resp, out := post(t, url, testKey, map[string]any{"cwd": "/work/demo/sub", "source": "startup"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	ac := additionalContext(t, out)
	require.Contains(t, ac, "<seam-briefing>")
	require.Contains(t, ac, "- no-force-push")
	require.Contains(t, ac, "chroma-boot-race")

	// Subagent -> constraints only.
	_, out = post(t, url, testKey, map[string]any{"cwd": "/work/demo", "agent_type": "Explore"})
	ac = additionalContext(t, out)
	require.Contains(t, ac, "- no-force-push")
	require.NotContains(t, ac, "chroma-boot-race")

	// End to end: the auto-briefing recorded item_ids, so rebuilding the funnel
	// credits each surfaced memory. The full briefing and the subagent briefing
	// both surface the constraint (01A); only the full one surfaces the gotcha
	// (01B).
	ctx := context.Background()
	require.NoError(t, store.RebuildRetrievalStats(ctx, db))
	constraintStat, ok, err := store.GetRetrievalStat(ctx, db, "01A")
	require.NoError(t, err)
	require.True(t, ok, "constraint surfaced by the briefing should have a stats row")
	require.Equal(t, 2, constraintStat.InjectCount)
	gotchaStat, ok, err := store.GetRetrievalStat(ctx, db, "01B")
	require.NoError(t, err)
	require.True(t, ok, "gotcha surfaced by the briefing should have a stats row")
	require.Equal(t, 1, gotchaStat.InjectCount)
}

// A SessionStart from a moved repo's new location adopts the existing project
// and records the remap as a repo.moved event -- the console's notice that the
// map healed itself instead of minting a duplicate -N project.
func TestSessionStartHook_MovedRepoAdoptsAndRecordsEvent(t *testing.T) {
	ts, db := newHandlerServer(t)
	ctx := context.Background()

	// The fixture maps /work/demo -> demo, and /work/demo does not exist on
	// disk; a real repo named demo elsewhere is therefore a move, not a
	// same-name collision.
	newRoot := filepath.Join(t.TempDir(), "demo")
	require.NoError(t, os.MkdirAll(filepath.Join(newRoot, ".git"), 0o755))

	resp, _ := post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"cwd": newRoot, "source": "startup"})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	m, err := store.RepoProjectMap(ctx, db)
	require.NoError(t, err)
	require.Equal(t, map[string]string{newRoot: "demo"}, m)

	var payload string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT payload FROM events WHERE kind = 'repo.moved'`).Scan(&payload))
	require.Contains(t, payload, newRoot)
	require.Contains(t, payload, "/work/demo")
}

// Codex has a distinct SubagentStart event. It reuses the constraints-only
// briefing path but the event's session_id is the parent identity, so it must
// never run SessionStart's create/reactivate/re-scope behavior.
func TestCodexSubagentStart_ConstraintsOnlyAndParentIdentityIsReadOnly(t *testing.T) {
	ts, db := newHandlerServer(t)
	ctx := context.Background()
	const (
		parentA = "019f8000-0000-7000-8000-000000000001"
		parentB = "019f8000-0010-7000-8000-000000000011"
		unknown = "019f8000-0020-7000-8000-000000000021"
	)

	_, _ = post(t, ts.URL+"/api/hooks/session-start?client=codex", testKey, map[string]any{
		"session_id": parentA, "cwd": "/work/demo", "source": "startup", "model": "gpt-parent-a",
	})
	_, _ = post(t, ts.URL+"/api/hooks/session-start?client=codex", testKey, map[string]any{
		"session_id": parentB, "cwd": "/work/other", "source": "startup", "model": "gpt-parent-b",
	})
	require.NoError(t, store.TouchAmbientSession(ctx, db, ClientCodex.externalIdentity(), parentA,
		time.Now().UTC().Add(-time.Hour)))
	beforeA := requireAmbientSession(t, db, ClientCodex, parentA)
	beforeB := requireAmbientSession(t, db, ClientCodex, parentB)

	resp, out := post(t, ts.URL+"/api/hooks/subagent-start?client=codex", testKey, map[string]any{
		"session_id": parentA, "turn_id": "turn-1", "agent_id": "child-1",
		"agent_type": "default", "cwd": "/work/demo", "model": "gpt-child",
		"permission_mode": "default", "hook_event_name": "SubagentStart",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	hso, ok := out["hookSpecificOutput"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "SubagentStart", hso["hookEventName"])
	ac := additionalContext(t, out)
	require.Contains(t, ac, "- no-force-push")
	require.NotContains(t, ac, "chroma-boot-race", "children receive constraints, not the full memory index")
	require.NotContains(t, ac, "Seam session:", "a child gets no independent ambient identity")

	// A malformed event missing a required identity field is acknowledged with
	// empty context; it cannot fall back to a full parent/global briefing.
	_, out = post(t, ts.URL+"/api/hooks/subagent-start?client=codex", testKey, map[string]any{
		"session_id": parentA, "agent_id": "child-malformed", "cwd": "/work/demo",
		"hook_event_name": "SubagentStart",
	})
	require.Empty(t, additionalContext(t, out))

	// A child CWD that differs from its parent may affect retrieval scope, but it
	// cannot re-scope either ambient row. An unknown parent may receive scoped
	// constraints, but it must not create a row.
	_, _ = post(t, ts.URL+"/api/hooks/subagent-start?client=codex", testKey, map[string]any{
		"session_id": parentA, "agent_id": "child-2", "agent_type": "default", "cwd": "/work/other",
	})
	_, _ = post(t, ts.URL+"/api/hooks/subagent-start?client=codex", testKey, map[string]any{
		"session_id": unknown, "agent_id": "child-3", "agent_type": "default", "cwd": "/work/demo",
	})
	afterA := requireAmbientSession(t, db, ClientCodex, parentA)
	afterB := requireAmbientSession(t, db, ClientCodex, parentB)
	require.Equal(t, beforeA.Name, afterA.Name)
	require.Equal(t, beforeA.ProjectSlug, afterA.ProjectSlug)
	require.Equal(t, beforeA.Model, afterA.Model, "the child model must not re-attribute its parent")
	require.True(t, afterA.UpdatedAt.After(beforeA.UpdatedAt), "SubagentStart heartbeats the proven parent id")
	require.Equal(t, beforeB.Name, afterB.Name)
	require.Equal(t, beforeB.ProjectSlug, afterB.ProjectSlug)
	require.Equal(t, beforeB.Model, afterB.Model)
	require.Equal(t, beforeB.UpdatedAt, afterB.UpdatedAt, "another parent must not be heartbeated")
	_, found, err := store.AmbientSessionByExternalIdentity(
		ctx, db, ClientCodex.externalIdentity(), unknown)
	require.NoError(t, err)
	require.False(t, found, "SubagentStart must not create an ambient row")
	var sessionCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&sessionCount))
	require.Equal(t, 2, sessionCount)

	// The emitted child context is ordinary injection telemetry, stamped on the
	// parent with the correct client and hook event.
	var foundParentInjection bool
	for _, event := range eventsOfKind(t, events.NewRecorder(db), core.EventInjected) {
		if event.Payload["hook"] != "subagent-start" || event.SessionID != afterA.ID {
			continue
		}
		foundParentInjection = true
		require.Equal(t, string(ClientCodex), event.Payload["external_client"])
		require.Equal(t, parentA, event.Payload["claude_session_id"])
	}
	require.True(t, foundParentInjection, "SubagentStart injection must be attributed to its parent")
}

// Claude Code wires the same SubagentStart command hook as Codex (a CC Task
// subagent gets no SessionStart of its own), so the default claude client must
// take the identical constraints-only path: same briefing shape, a parent-only
// heartbeat, and no ambient row for the child.
func TestClaudeSubagentStart_ConstraintsOnly(t *testing.T) {
	ts, db := newHandlerServer(t)
	ctx := context.Background()
	const parent = "cc-parent-0000-0001"

	_, _ = post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": parent, "cwd": "/work/demo", "source": "startup",
	})
	require.NoError(t, store.TouchAmbientSession(ctx, db, ClientClaudeCode.externalIdentity(), parent,
		time.Now().UTC().Add(-time.Hour)))
	before := requireAmbientSession(t, db, ClientClaudeCode, parent)

	// The documented CC payload: session_id is the parent, the child carries
	// agent_id/agent_type, and no ?client= param means the claude client.
	resp, out := post(t, ts.URL+"/api/hooks/subagent-start", testKey, map[string]any{
		"session_id": parent, "agent_id": "cc-child-1", "agent_type": "Explore",
		"cwd": "/work/demo", "permission_mode": "default", "hook_event_name": "SubagentStart",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	hso, ok := out["hookSpecificOutput"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "SubagentStart", hso["hookEventName"])
	ac := additionalContext(t, out)
	require.Contains(t, ac, "- no-force-push")
	require.NotContains(t, ac, "chroma-boot-race", "children receive constraints, not the full memory index")
	require.NotContains(t, ac, "Seam session:", "a child gets no independent ambient identity")

	after := requireAmbientSession(t, db, ClientClaudeCode, parent)
	require.Equal(t, before.Name, after.Name)
	require.Equal(t, before.ProjectSlug, after.ProjectSlug)
	require.True(t, after.UpdatedAt.After(before.UpdatedAt), "SubagentStart heartbeats the parent")
	_, found, err := store.AmbientSessionByExternalIdentity(
		ctx, db, ClientClaudeCode.externalIdentity(), "cc-child-1")
	require.NoError(t, err)
	require.False(t, found, "SubagentStart must not create an ambient row for the child")
}

func TestUserPromptSubmitHook(t *testing.T) {
	ts, _ := newHandlerServer(t)
	url := ts.URL + "/api/hooks/user-prompt-submit"

	// A matching prompt returns a recall block.
	_, out := post(t, url, testKey, map[string]any{
		"cwd": "/work/demo", "user_prompt": "why does the chroma container fail its health check",
	})
	ac := additionalContext(t, out)
	require.Contains(t, ac, "<seam-recall>")
	require.Contains(t, ac, "chroma-boot-race")

	// A non-matching prompt returns empty additionalContext (never blocks).
	_, out = post(t, url, testKey, map[string]any{"cwd": "/work/demo", "user_prompt": "weather in paris"})
	require.Empty(t, additionalContext(t, out))

	// Empty body is tolerated (200, empty context).
	resp, out := post(t, url, testKey, map[string]any{})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Empty(t, additionalContext(t, out))
}

func eventsOfKind(t *testing.T, rec *events.Recorder, kind core.EventKind) []core.Event {
	t.Helper()
	evs, err := rec.ByKinds(context.Background(), []core.EventKind{kind}, "", "", 200)
	require.NoError(t, err)
	return evs
}

func payloadString(v any) string {
	s, _ := v.(string)
	return s
}

func TestHookEventCapture(t *testing.T) {
	ts, db := newHandlerServer(t)
	rec := events.NewRecorder(db)

	// SessionStart creates the ambient session and records an injection stamped
	// with that session's ULID + project.
	_, out := post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": "abcdef12-3456", "cwd": "/work/demo", "source": "startup",
	})
	require.Contains(t, additionalContext(t, out), "<seam-briefing>")

	sess := requireAmbientSession(t, db, ClientClaudeCode, "abcdef12-3456")

	inj := eventsOfKind(t, rec, core.EventInjected)
	require.Len(t, inj, 1)
	require.Equal(t, sess.ID, inj[0].SessionID, "injection stamped with the ambient session ULID")
	require.Equal(t, "demo", inj[0].ProjectSlug)
	require.Equal(t, "session-start", inj[0].Payload["hook"])
	require.Nil(t, inj[0].Payload["prompt"], "SessionStart carries no user prompt")

	// A matching prompt records an injection carrying the prompt text.
	_, out = post(t, ts.URL+"/api/hooks/user-prompt-submit", testKey, map[string]any{
		"session_id": "abcdef12-3456", "cwd": "/work/demo",
		"user_prompt": "why does the chroma container fail its health check",
	})
	require.Contains(t, additionalContext(t, out), "chroma-boot-race")
	inj = eventsOfKind(t, rec, core.EventInjected)
	require.Len(t, inj, 2) // newest first
	require.Equal(t, "user-prompt-submit", inj[0].Payload["hook"])
	require.Contains(t, payloadString(inj[0].Payload["prompt"]), "chroma container")
	require.Equal(t, sess.ID, inj[0].SessionID)

	// A non-matching prompt records a hook.prompt (recall miss) instead of nothing.
	_, _ = post(t, ts.URL+"/api/hooks/user-prompt-submit", testKey, map[string]any{
		"session_id": "abcdef12-3456", "cwd": "/work/demo", "user_prompt": "weather in paris",
	})
	hp := eventsOfKind(t, rec, core.EventHookPrompt)
	require.Len(t, hp, 1)
	require.Equal(t, false, hp[0].Payload["matched"])
	require.Contains(t, payloadString(hp[0].Payload["prompt"]), "weather in paris")
	require.Equal(t, sess.ID, hp[0].SessionID)
	// Injected count is unchanged by the miss (InjectionsByDay stays accurate).
	require.Len(t, eventsOfKind(t, rec, core.EventInjected), 2)

	// SessionEnd records session.ended with reason + harvested findings.
	transcript := writeTranscript(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Did the thing."}]}}`)
	_, _ = post(t, ts.URL+"/api/hooks/session-end", testKey, map[string]any{
		"session_id": "abcdef12-3456", "transcript_path": transcript, "reason": "clear",
	})
	ended := eventsOfKind(t, rec, core.EventSessionEnded)
	require.Len(t, ended, 1)
	require.Equal(t, "clear", ended[0].Payload["reason"])
	require.Contains(t, payloadString(ended[0].Payload["findings"]), "Did the thing.")
}

// TestSessionEndCascade_ClosesLinkedExplicitSession verifies the known-end
// principle: a graceful SessionEnd closes not just the ambient cc/* session but any
// explicit session_start linked to the same Claude session -- immediately, without
// waiting out the idle TTL -- releasing its task claims and keeping its own findings.
func TestSessionEndCascade_ClosesLinkedExplicitSession(t *testing.T) {
	ts, db := newHandlerServer(t)
	ctx := context.Background()
	rec := events.NewRecorder(db)
	claudeID := "abcdef12-9999"

	// SessionStart creates the ambient cc/abcdef12 stamped with claude_session_id.
	_, _ = post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": claudeID, "cwd": "/work/demo", "source": "startup",
	})
	amb := requireAmbientSession(t, db, ClientClaudeCode, claudeID)
	require.Equal(t, claudeID, amb.ExternalSessionID)

	// An explicit session_start that linked to the same Claude session (ambient=0,
	// claude_session_id set), carrying its own interim findings and holding a claim.
	explID, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: explID, Name: "sess/work", ProjectSlug: "demo", Status: core.SessionActive,
		ExternalSessionID: claudeID, ExternalClient: "claude-code",
		CWD: "/work/demo", Findings: "interim progress",
		CreatedAt: amb.CreatedAt, UpdatedAt: amb.CreatedAt,
	}))
	taskID, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateTask(ctx, db, core.Task{
		ID: taskID, ProjectSlug: "demo", Title: "held", Status: core.TaskOpen,
		CreatedAt: amb.CreatedAt, UpdatedAt: amb.CreatedAt,
	}))
	_, err = store.ClaimTask(ctx, db, taskID, explID, 30*time.Minute, amb.CreatedAt)
	require.NoError(t, err)

	// SessionEnd (a known end) closes BOTH immediately.
	transcript := writeTranscript(t,
		`{"type":"assistant","isSidechain":false,"message":{"role":"assistant","model":"claude-fable-5","content":[{"type":"text","text":"All done."}]}}`)
	_, _ = post(t, ts.URL+"/api/hooks/session-end", testKey, map[string]any{
		"session_id": claudeID, "transcript_path": transcript, "reason": "logout",
	})

	gotAmb, _, err := store.SessionByID(ctx, db, amb.ID)
	require.NoError(t, err)
	require.Equal(t, core.SessionCompleted, gotAmb.Status)
	require.Contains(t, gotAmb.Findings, "All done.", "ambient harvested from transcript")
	require.Equal(t, "claude-fable-5", gotAmb.Model, "one-turn ambient session gets its final transcript model")

	gotExpl, _, err := store.SessionByID(ctx, db, explID)
	require.NoError(t, err)
	require.Equal(t, core.SessionCompleted, gotExpl.Status, "linked explicit session closed immediately")
	require.Equal(t, "interim progress", gotExpl.Findings, "explicit session keeps its own findings")
	require.Equal(t, "claude-fable-5", gotExpl.Model, "linked session tracks the same final agent model")

	task, err := store.TaskByID(ctx, db, taskID)
	require.NoError(t, err)
	require.Equal(t, core.TaskOpen, task.Status, "the explicit session's claim was released")

	require.Len(t, eventsOfKind(t, rec, core.EventSessionEnded), 2, "one session.ended per closed session")
}

func TestAmbientSessionLifecycle(t *testing.T) {
	ts, db := newHandlerServer(t)
	ctx := context.Background()
	startURL := ts.URL + "/api/hooks/session-start"
	endURL := ts.URL + "/api/hooks/session-end"

	// SessionStart creates an ambient session and appends its line to the briefing.
	_, out := post(t, startURL, testKey, map[string]any{
		"session_id": "abcdef12-3456", "cwd": "/work/demo", "source": "startup",
	})
	require.Contains(t, additionalContext(t, out),
		"Seam session: "+ambientName(ClientClaudeCode, "abcdef12-3456")+" (ambient)")

	sess := requireAmbientSession(t, db, ClientClaudeCode, "abcdef12-3456")
	require.Equal(t, core.SessionActive, sess.Status)
	require.True(t, sess.Ambient)
	require.Equal(t, "demo", sess.ProjectSlug)
	require.Equal(t, "abcdef12-3456", sess.Metadata["claude_session_id"])

	// A subagent SessionStart gets no ambient session of its own.
	_, out = post(t, startURL, testKey, map[string]any{
		"session_id": "sub00000-0000", "cwd": "/work/demo", "agent_type": "Explore",
	})
	require.NotContains(t, additionalContext(t, out), "(ambient)")
	_, ok, err := store.AmbientSessionByExternalIdentity(
		ctx, db, ClientClaudeCode.externalIdentity(), "sub00000-0000")
	require.NoError(t, err)
	require.False(t, ok, "subagents share the parent session, no ambient row")

	// SessionEnd harvests the transcript's final assistant message and completes.
	transcript := writeTranscript(t,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Shipped the ready-queue."}]}}`)
	resp, endOut := post(t, endURL, testKey, map[string]any{
		"session_id": "abcdef12-3456", "transcript_path": transcript, "reason": "clear",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	// SessionEnd has no hookSpecificOutput variant in Claude Code's schema;
	// including one fails root validation, so the ack must omit it.
	require.NotContains(t, endOut, "hookSpecificOutput")
	require.Equal(t, true, endOut["continue"])

	sess = requireAmbientSession(t, db, ClientClaudeCode, "abcdef12-3456")
	require.Equal(t, core.SessionCompleted, sess.Status)
	require.Equal(t, "(auto-harvested) Shipped the ready-queue.", sess.Findings)

	// Re-delivering SessionEnd is a no-op (findings are not clobbered).
	resp, _ = post(t, endURL, testKey, map[string]any{
		"session_id": "abcdef12-3456", "transcript_path": "", "reason": "other",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	sess = requireAmbientSession(t, db, ClientClaudeCode, "abcdef12-3456")
	require.Equal(t, "(auto-harvested) Shipped the ready-queue.", sess.Findings)

	// SessionEnd for an unknown session is a tolerated no-op (still 200).
	resp, _ = post(t, endURL, testKey, map[string]any{"session_id": "unknown0-0000"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestSessionEndRejectsBadKey(t *testing.T) {
	ts, _ := newHandlerServer(t)
	resp, _ := post(t, ts.URL+"/api/hooks/session-end", "nope", map[string]any{"session_id": "x"})
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

// requireCommandHook asserts the event's first entry is an exec-form command
// hook running `seam hook <cliArg>`: type command, a bare-binary command string,
// and args carrying "hook" then the event -- no legacy shell string.
func requireCommandHook(t *testing.T, hooksObj map[string]any, event, cliArg string) {
	t.Helper()
	arr := entryArray(hooksObj, event)
	require.NotEmpty(t, arr, "%s should be installed", event)
	// Find the Seamless-owned command entry: a preserved foreign hook (e.g. a v1
	// http entry) may sit ahead of ours, so position is not assumed.
	for _, e := range arr {
		if !isManaged(e) {
			continue
		}
		h0 := e.(map[string]any)["hooks"].([]any)[0].(map[string]any)
		require.Equal(t, "command", h0["type"], "%s should be a command hook", event)
		require.NotEmpty(t, h0["command"], "%s command should be the seam binary", event)
		args, ok := hookStringArgs(h0["args"])
		require.True(t, ok, "%s args should be strings", event)
		require.GreaterOrEqual(t, len(args), 2, "%s args should include the hook event", event)
		require.Equal(t, []string{"hook", cliArg}, args[:2], "%s args should run `hook %s`", event, cliArg)
		return
	}
	t.Fatalf("no exec-form command hook running `hook %s` found for %s", cliArg, event)
}

func TestInstallIdempotentAndPreservesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	// Pre-existing settings with an unrelated key and a foreign hook.
	require.NoError(t, os.WriteFile(path, []byte(`{
  "model": "opus",
  "hooks": {
    "SessionStart": [
      {"seam_managed": true, "hooks": [{"type": "http", "url": "http://127.0.0.1:8080/api/hooks/session-start"}]}
    ]
  }
}`), 0o600))

	opts := InstallOptions{SettingsPath: path, BaseURL: "http://127.0.0.1:8081", APIKey: "secret-key"}

	res, err := Install(opts)
	require.NoError(t, err)
	require.True(t, res.Changed)
	require.NotEmpty(t, res.BackupPath, "the pre-existing file should be backed up on first change")

	// A backup was written (first change).
	baks, _ := filepath.Glob(path + ".seamless-bak-*")
	require.Len(t, baks, 1)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, "opus", got["model"], "unknown top-level key preserved")

	hooksObj := got["hooks"].(map[string]any)
	ss := hooksObj["SessionStart"].([]any)
	// The foreign seam_managed entry survives alongside the new seamless entry.
	require.Len(t, ss, 2)
	require.Contains(t, string(raw), "seamless_managed")
	// SessionStart and SessionEnd are exec-form command hooks; only
	// UserPromptSubmit stays http, so the bearer key reaches settings via that
	// lone http hook.
	requireCommandHook(t, hooksObj, "SessionStart", "session-start")
	requireCommandHook(t, hooksObj, "SessionEnd", "session-end")
	require.Contains(t, string(raw), "8081/api/hooks/user-prompt-submit")
	require.Contains(t, string(raw), "Bearer secret-key")
	// UserPromptSubmit and SessionEnd were added too.
	require.Len(t, hooksObj["UserPromptSubmit"].([]any), 1)
	require.Len(t, hooksObj["SessionEnd"].([]any), 1)
	// The plan-capture hooks are exec-form command hooks: ONE PostToolUse entry
	// with the joined matcher, plus SubagentStop and PermissionRequest.
	requireCommandHook(t, hooksObj, "PostToolUse", "post-tool-use")
	requireCommandHook(t, hooksObj, "SubagentStop", "subagent-stop")
	requireCommandHook(t, hooksObj, "PermissionRequest", "permission-request")
	// The child-briefing hook mirrors the Codex profile: matcher-free, so every
	// Task subagent gets the constraints-only briefing.
	requireCommandHook(t, hooksObj, "SubagentStart", "subagent-start")
	sas := hooksObj["SubagentStart"].([]any)
	require.Len(t, sas, 1)
	require.NotContains(t, sas[0].(map[string]any), "matcher")
	ptu := hooksObj["PostToolUse"].([]any)
	require.Len(t, ptu, 1)
	require.Equal(t, "Write|Edit|MultiEdit|ExitPlanMode", ptu[0].(map[string]any)["matcher"])
	require.Len(t, hooksObj["SubagentStop"].([]any), 1)
	pr := hooksObj["PermissionRequest"].([]any)
	require.Len(t, pr, 1)
	require.Equal(t, "ExitPlanMode", pr[0].(map[string]any)["matcher"])

	// Re-install is a no-op.
	res2, err := Install(opts)
	require.NoError(t, err)
	require.False(t, res2.Changed)
	for _, a := range res2.Actions {
		require.Contains(t, a, "unchanged")
	}
}

func TestInstalledStatus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")

	// Missing file -> nothing installed, no error.
	statusOpts := InstallOptions{Client: ClientClaudeCode, SettingsPath: path, BaseURL: "http://127.0.0.1:8081", APIKey: "k"}
	status, err := InstalledStatus(statusOpts)
	require.NoError(t, err)
	require.Empty(t, status.Owned)

	// After a full install, every event is reported.
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	_, err = Install(InstallOptions{SettingsPath: path, BaseURL: "http://127.0.0.1:8081", APIKey: "k"})
	require.NoError(t, err)

	status, err = InstalledStatus(statusOpts)
	require.NoError(t, err)
	require.Equal(t, installedEvents(t, ClientClaudeCode), status.Current)
	require.Empty(t, status.Stale)
	require.Len(t, status.Current, 7)
}

// Claude Code re-serializes settings.json through its own schema when the
// owner edits config or permissions, dropping keys it does not know --
// including the seamless_managed marker -- while keeping the functional hook
// entries. Those still-firing hooks must count as installed, matched by their
// seam-CLI command (command hooks) or hook URL (http hooks).
func TestInstalledStatusSurvivesMarkerStripping(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	_, err := Install(InstallOptions{SettingsPath: path, BaseURL: "http://127.0.0.1:8081", APIKey: "k"})
	require.NoError(t, err)

	// Simulate the Claude Code rewrite: strip the marker from every entry.
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(raw, &settings))
	for _, arr := range settings["hooks"].(map[string]any) {
		for _, e := range arr.([]any) {
			delete(e.(map[string]any), managedMarker)
		}
	}
	raw, err = json.Marshal(settings)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	status, err := InstalledStatus(InstallOptions{
		Client: ClientClaudeCode, SettingsPath: path, BaseURL: "http://127.0.0.1:8081", APIKey: "k",
	})
	require.NoError(t, err)
	require.Equal(t, installedEvents(t, ClientClaudeCode), status.Current)
	require.Empty(t, status.Stale)

	// An unmarked http entry at a different base URL is not ours (e.g. a v1
	// leftover): the http hook must drop out while the seam-CLI command hooks
	// still match by their `hook <event>` command.
	status, err = InstalledStatus(InstallOptions{
		Client: ClientClaudeCode, SettingsPath: path, BaseURL: "http://127.0.0.1:9999", APIKey: "k",
	})
	require.NoError(t, err)
	require.NotContains(t, status.Current, "UserPromptSubmit")
	require.Len(t, status.Current, 6)
}

// Mirrors the plan-capture dogfood state: an older installer left UNMARKED
// command hooks (`... seam hook session-start`), which URL-matching alone
// cannot adopt (command hooks have no URL). Install must adopt them by their
// `hook <event>` command instead of appending a duplicate.
func TestInstallAdoptsUnmarkedCommandHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
  "hooks": {
    "SessionStart": [
      {"matcher": "startup|resume|clear|compact", "hooks": [{"type": "command", "command": "SEAMLESS_CONFIG=/old/seamless.yaml /old/bin/seam hook session-start", "timeout": 10}]}
    ],
    "SessionEnd": [
      {"hooks": [{"type": "command", "command": "/old/bin/seam hook session-end", "timeout": 10}]},
      {"seamless_managed": true, "hooks": [{"type": "command", "command": "/old/bin/seam hook session-end", "timeout": 10}]}
    ]
  }
}`), 0o600))

	opts := InstallOptions{SettingsPath: path, BaseURL: "http://127.0.0.1:8081", APIKey: "k", SeamBin: "/new/seam", ConfigPath: "/new/seamless.yaml"}
	res, err := Install(opts)
	require.NoError(t, err)
	require.True(t, res.Changed)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	hooksObj := got["hooks"].(map[string]any)

	// The unmarked command entry is adopted in place, not duplicated, and
	// rewritten into the canonical exec form (bare binary + args, --config flag).
	ss := hooksObj["SessionStart"].([]any)
	require.Len(t, ss, 1)
	require.Equal(t, true, ss[0].(map[string]any)["seamless_managed"])
	h0 := ss[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	require.Equal(t, "command", h0["type"])
	require.Equal(t, "/new/seam", h0["command"])
	require.Equal(t, []any{"hook", "session-start", "--config", "/new/seamless.yaml"}, h0["args"])

	// Unmarked + marked command entries for one event collapse to one.
	se := hooksObj["SessionEnd"].([]any)
	require.Len(t, se, 1)

	joined := strings.Join(res.Actions, ",")
	require.Contains(t, joined, "SessionStart: adopted")
	require.Contains(t, joined, "SessionEnd: deduped")

	// Re-install is a clean no-op.
	res2, err := Install(opts)
	require.NoError(t, err)
	require.False(t, res2.Changed)
}

// Mirrors the P6 dogfood state: hand-written UNMARKED Seamless hooks (which an
// older installer duplicated) plus a v1 seam_managed entry at :8080. Install
// must adopt/dedupe the seamless-URL entries in place while leaving v1 alone.
func TestInstallAdoptsAndDedupesUnmarkedHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
  "hooks": {
    "SessionStart": [
      {"matcher": "startup|resume|clear|compact", "hooks": [{"type": "http", "url": "http://127.0.0.1:8081/api/hooks/session-start", "timeout": 10}]},
      {"seamless_managed": true, "matcher": "startup|resume|clear|compact", "hooks": [{"type": "http", "url": "http://127.0.0.1:8081/api/hooks/session-start", "timeout": 10}]},
      {"seam_managed": true, "matcher": "startup|resume|clear|compact", "hooks": [{"type": "http", "url": "http://127.0.0.1:8080/api/hooks/session-start", "timeout": 10}]}
    ],
    "UserPromptSubmit": [
      {"hooks": [{"type": "http", "url": "http://127.0.0.1:8081/api/hooks/user-prompt-submit", "timeout": 5}]}
    ]
  }
}`), 0o600))

	opts := InstallOptions{SettingsPath: path, BaseURL: "http://127.0.0.1:8081", APIKey: "k", SeamBin: "/opt/seam", ConfigPath: "/etc/seamless.yaml"}
	res, err := Install(opts)
	require.NoError(t, err)
	require.True(t, res.Changed)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	hooksObj := got["hooks"].(map[string]any)

	// SessionStart: the two seamless-URL entries collapse to one marked entry;
	// the v1 :8080 seam_managed entry is preserved -> 2 entries total.
	ss := hooksObj["SessionStart"].([]any)
	require.Len(t, ss, 2)
	var marked, v1 int
	for _, e := range ss {
		m := e.(map[string]any)
		if m["seamless_managed"] == true {
			marked++
			// SessionStart is now an exec-form command hook running the seam CLI,
			// not http, with the config path passed via --config so it resolves
			// from any cwd.
			h0 := m["hooks"].([]any)[0].(map[string]any)
			require.Equal(t, "command", h0["type"])
			require.Equal(t, "/opt/seam", h0["command"])
			require.Equal(t, []any{"hook", "session-start", "--config", "/etc/seamless.yaml"}, h0["args"])
		}
		if m["seam_managed"] == true {
			v1++
			require.Contains(t, m["hooks"].([]any)[0].(map[string]any)["url"], "8080")
		}
	}
	require.Equal(t, 1, marked, "seamless-URL duplicates collapse to one marked entry")
	require.Equal(t, 1, v1, "v1 seam_managed :8080 entry preserved")

	// UserPromptSubmit: the lone unmarked entry is adopted in place (now marked).
	ups := hooksObj["UserPromptSubmit"].([]any)
	require.Len(t, ups, 1)
	require.Equal(t, true, ups[0].(map[string]any)["seamless_managed"])

	// SessionEnd was absent -> added, as a command hook (not http) with the
	// config path baked in so the harvest resolves config from any cwd.
	se := hooksObj["SessionEnd"].([]any)
	require.Len(t, se, 1)
	seHook := se[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	require.Equal(t, "command", seHook["type"])
	require.Equal(t, "/opt/seam", seHook["command"])
	require.Equal(t, []any{"hook", "session-end", "--config", "/etc/seamless.yaml"}, seHook["args"])

	joined := strings.Join(res.Actions, ",")
	require.Contains(t, joined, "SessionStart: deduped")
	require.Contains(t, joined, "UserPromptSubmit: adopted")
	require.Contains(t, joined, "SessionEnd: added")

	// Re-install is now a clean no-op (all entries carry the marker).
	res2, err := Install(opts)
	require.NoError(t, err)
	require.False(t, res2.Changed)
	for _, a := range res2.Actions {
		require.Contains(t, a, "unchanged")
	}
}

// TestHookErrorEventOnSwallowedFailure forces a swallowed hook-stage failure --
// an ambient display-name collision owned by a different identity, which
// ensureAmbientSession cannot resolve by resuming -- and asserts the fail-open
// contract holds (200) while exactly one hook.error event records the stage.
func TestHookErrorEventOnSwallowedFailure(t *testing.T) {
	ts, db := newHandlerServer(t)

	name := ambientName(ClientClaudeCode, "sess-collide")
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(context.Background(), db, core.Session{
		ID: id, Name: name, Status: core.SessionActive,
		ExternalSessionID: "someone-else", ExternalClient: "codex",
		Ambient: true, CreatedAt: now, UpdatedAt: now,
	}))

	resp, _ := post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": "sess-collide", "cwd": "/work/demo", "source": "startup"})
	require.Equal(t, http.StatusOK, resp.StatusCode, "fail-open: the hook still answers 200")

	rows, err := db.Query(`SELECT payload FROM events WHERE kind = 'hook.error'`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var payloads []map[string]any
	for rows.Next() {
		var raw string
		require.NoError(t, rows.Scan(&raw))
		var p map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &p))
		payloads = append(payloads, p)
	}
	require.NoError(t, rows.Err())
	require.Len(t, payloads, 1, "exactly one hook.error for the one swallowed failure")
	require.Equal(t, "ambient-create", payloads[0]["stage"])
	require.Equal(t, "claude-code", payloads[0]["client"])
	errText, _ := payloads[0]["error"].(string)
	require.NotEmpty(t, errText)
	require.NotContains(t, errText, "\n", "payload error is first-line only")
}
