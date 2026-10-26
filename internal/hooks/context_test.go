package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

func TestPrepareHookContext_ClientAwareThreshold(t *testing.T) {
	tests := []struct {
		name      string
		client    Client
		content   string
		truncated bool
	}{
		{
			name:    "codex just below",
			client:  ClientCodex,
			content: strings.Repeat("a", (codexContextMaxTokens-1)*4),
		},
		{
			// The exact boundary: 9600 bytes estimate to exactly the cap, and
			// the cap is inclusive -- only content strictly above it truncates.
			name:    "codex exactly at the cap",
			client:  ClientCodex,
			content: strings.Repeat("a", codexContextMaxTokens*4),
		},
		{
			name:      "codex just above",
			client:    ClientCodex,
			content:   strings.Repeat("a", codexContextMaxTokens*4+1),
			truncated: true,
		},
		{
			name:    "normal 1500 token briefing unchanged",
			client:  ClientCodex,
			content: strings.Repeat("b", 1500*4),
		},
		{
			name:    "claude remains uncapped",
			client:  ClientClaudeCode,
			content: strings.Repeat("c", (codexContextMaxTokens+100)*4),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := prepareHookContext(tt.client, tt.content)
			require.Equal(t, retrieve.EstimateTokens(tt.content), got.originalEstimatedTokens)
			require.Equal(t, tt.truncated, got.truncated)
			require.Equal(t, retrieve.EstimateTokens(got.content), got.emittedEstimatedTokens)
			if tt.truncated {
				require.LessOrEqual(t, got.emittedEstimatedTokens, codexContextMaxTokens)
				require.NotEqual(t, tt.content, got.content)
				return
			}
			require.Equal(t, tt.content, got.content)
		})
	}
}

func TestPrepareHookContext_PreservesPriorityUTF8AndTags(t *testing.T) {
	const identity = "Seam session: cx/019f7291-46ec71e628fd86c6 (ambient)"
	content := "<seam-briefing>\n" +
		"Seam project: demo -- pinned context.\n" +
		"CONSTRAINT: exact-telemetry: the emitted bytes are authoritative\n" +
		"PLAN: codex-hardening -- 1/11 done, 5 claimable, 1 in flight\n" +
		strings.Repeat("- optional-memory: 界界界 optional detail that drops first\n", 500) +
		identity + "\n</seam-briefing>"

	got := prepareHookContext(ClientCodex, content)
	require.True(t, got.truncated)
	require.Greater(t, got.originalEstimatedTokens, codexContextMaxTokens)
	require.LessOrEqual(t, got.emittedEstimatedTokens, codexContextMaxTokens)
	require.True(t, utf8.ValidString(got.content))
	require.True(t, strings.HasPrefix(got.content, "<seam-briefing>\n"))
	require.Contains(t, got.content, "CONSTRAINT: exact-telemetry")
	require.Contains(t, got.content, "PLAN: codex-hardening")
	require.Contains(t, got.content, contextTruncationMarker)
	require.Contains(t, got.content, identity)
	require.True(t, strings.HasSuffix(got.content, "</seam-briefing>"))
}

func TestWriteContextResponse_CodexEventsShareCapAndExactTelemetry(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		hook    string
		content string
	}{
		{
			name:  "session-start briefing",
			event: "SessionStart",
			hook:  "session-start",
			content: "<seam-briefing>\nCONSTRAINT: keep-me: pinned\n" +
				strings.Repeat("session optional detail\n", 1200) +
				"Seam session: cx/019f7291-46ec71e628fd86c6 (ambient)\n</seam-briefing>",
		},
		{
			name:  "user-prompt-submit recall",
			event: "UserPromptSubmit",
			hook:  "user-prompt-submit",
			content: "<seam-recall>possibly relevant:\n" +
				strings.Repeat("- oversized recall detail 界\n", 1200) +
				"</seam-recall>",
		},
		{
			name:  "subagent-start constraints",
			event: "SubagentStart",
			hook:  "subagent-start",
			content: "<seam-briefing>\nCONSTRAINT: child-scope: keep constraints\n" +
				strings.Repeat("CONSTRAINT: bounded-child: 界界界\n", 1200) +
				"</seam-briefing>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			h := NewHandler(Config{DB: db, Events: events.NewRecorder(db)})
			rr := httptest.NewRecorder()

			h.writeContextResponse(context.Background(), rr, tt.event, tt.hook,
				ClientCodex, "codex-session", "prompt", tt.content, true, []string{"01A"})

			require.Equal(t, http.StatusOK, rr.Code)
			var response hookResponse
			require.NoError(t, json.NewDecoder(rr.Body).Decode(&response))
			require.NotNil(t, response.HookSpecificOutput)
			emitted := response.HookSpecificOutput.AdditionalContext
			require.LessOrEqual(t, retrieve.EstimateTokens(emitted), codexContextMaxTokens)
			require.True(t, utf8.ValidString(emitted))

			injected := eventsOfKind(t, events.NewRecorder(db), core.EventInjected)
			require.Len(t, injected, 1)
			require.Equal(t, emitted, injected[0].Payload["content"],
				"telemetry content must be byte-for-byte the response content")
			require.Equal(t, tt.hook, injected[0].Payload["hook"])
			require.Equal(t, true, injected[0].Payload["truncated"])
			require.Equal(t, float64(retrieve.EstimateTokens(tt.content)),
				injected[0].Payload["original_estimated_tokens"])
			require.Equal(t, float64(retrieve.EstimateTokens(emitted)),
				injected[0].Payload["emitted_estimated_tokens"])
			require.Equal(t, false, injected[0].Payload["item_ids_exact"])
			// Truncation makes the list a superset, not a reason to drop it:
			// item_ids is the only source of last_injected_at, so discarding it
			// would make an injected memory read as stale-and-archivable.
			require.Equal(t, []any{"01A"}, injected[0].Payload["item_ids"])
			require.Equal(t, float64(1), injected[0].Payload["original_item_count"])
		})
	}
}

func TestSessionStart_CodexCapsPinnedContextAfterAmbientLine(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx := context.Background()
	require.NoError(t, store.SetSetting(ctx, db,
		store.SettingRepoProjectMap, `{"/work/demo":"demo"}`))

	for i := range 25 {
		insertMemory(t, db, fmt.Sprintf("01C%03d", i), "constraint",
			fmt.Sprintf("pinned-constraint-%03d", i), strings.Repeat("bounded pinned detail ", 12), "demo")
	}
	now := time.Now().UTC()
	for i := range 95 {
		require.NoError(t, store.CreateTask(ctx, db, core.Task{
			ID:          fmt.Sprintf("01T%03d", i),
			ProjectSlug: "demo",
			Title:       fmt.Sprintf("plan step %03d", i),
			Status:      core.TaskOpen,
			PlanSlug:    fmt.Sprintf("codex-context-plan-%03d-%s", i, strings.Repeat("x", 32)),
			CreatedAt:   now,
			UpdatedAt:   now,
		}))
	}

	ret := retrieve.New(db, nil, config.Budgets{
		MaxBriefingTokens:  1500,
		RecallBudgetTokens: 1000,
	}, nil)
	h := NewHandler(Config{
		DB: db, Retrieve: ret, Events: events.NewRecorder(db), APIKey: testKey,
	})
	mux := http.NewServeMux()
	h.Register(mux)
	body, err := json.Marshal(map[string]any{
		"session_id": "019f7291-40f1-7311-8997-0d497579d27b",
		"cwd":        "/work/demo",
		"source":     "startup",
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/hooks/session-start?client=codex", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var response hookResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&response))
	require.NotNil(t, response.HookSpecificOutput)
	emitted := response.HookSpecificOutput.AdditionalContext
	require.LessOrEqual(t, retrieve.EstimateTokens(emitted), codexContextMaxTokens)
	// The constraint head is tiered (default constraint_max_full=4): the top
	// full line and the compact "Also binding" tail -- including the oldest
	// name it carries -- are both pinned and survive the codex cap.
	require.Contains(t, emitted, "- pinned-constraint-024")
	require.Contains(t, emitted, "- +21 more, equally binding")
	require.Contains(t, emitted, "pinned-constraint-000")
	require.Contains(t, emitted, "- codex-context-plan-000")
	require.Contains(t, emitted, "Seam session: cx/019f7291-")
	require.True(t, strings.HasSuffix(emitted, "</seam-briefing>"))

	injected := eventsOfKind(t, events.NewRecorder(db), core.EventInjected)
	require.Len(t, injected, 1)
	require.Equal(t, emitted, injected[0].Payload["content"])
	require.Equal(t, true, injected[0].Payload["truncated"])
	require.Equal(t, false, injected[0].Payload["item_ids_exact"])
	// The truncated briefing still credits the constraints it injected, so they
	// do not accumulate a null last_injected_at and become archive proposals.
	require.NotEmpty(t, injected[0].Payload["item_ids"])
	require.Greater(t, injected[0].Payload["original_estimated_tokens"].(float64),
		float64(codexContextMaxTokens))
}

func TestSubagentStart_CodexCapsConstraintsAndRecordsExactTelemetry(t *testing.T) {
	ts, db := newHandlerServer(t)
	const parentID = "019f8000-0000-7000-8000-000000000001"

	for i := range 95 {
		insertMemory(t, db, fmt.Sprintf("01S%03d", i), "constraint",
			fmt.Sprintf("subagent-constraint-%03d", i), strings.Repeat("bounded child detail ", 12), "demo")
	}
	// The tiered constraint rendering keeps even 80 constraints under the codex
	// cap, so force the legacy all-full rendering (the owner's
	// constraint_max_full=0 override) to overflow it and exercise the
	// truncation + telemetry path end to end.
	legacy := config.Defaults().Briefing
	legacy.ConstraintMaxFull = 0
	require.NoError(t, store.SetBriefingConfig(context.Background(), db, legacy))
	_, _ = post(t, ts.URL+"/api/hooks/session-start?client=codex", testKey, map[string]any{
		"session_id": parentID, "cwd": "/work/demo", "source": "startup", "model": "gpt-parent",
	})

	// A child rollout whose spawn prompt matches the seeded gotcha, so every
	// section -- constraint wall, RELEVANT, footer -- participates in the
	// capped assembly.
	rollout := filepath.Join(t.TempDir(), "rollout-child-1.jsonl")
	require.NoError(t, os.WriteFile(rollout, []byte(
		`{"type":"event_msg","payload":{"type":"user_message","message":"why does the chroma container health check race at startup"}}`+"\n"), 0o644))

	resp, out := post(t, ts.URL+"/api/hooks/subagent-start?client=codex", testKey, map[string]any{
		"session_id": parentID, "turn_id": "turn-1", "agent_id": "child-1",
		"agent_type": "default", "cwd": "/work/demo", "model": "gpt-child",
		"permission_mode": "default", "hook_event_name": "SubagentStart",
		"transcript_path": rollout,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	hso := out["hookSpecificOutput"].(map[string]any)
	require.Equal(t, "SubagentStart", hso["hookEventName"])
	emitted := additionalContext(t, out)
	require.LessOrEqual(t, retrieve.EstimateTokens(emitted), codexContextMaxTokens)
	require.Contains(t, emitted, "- subagent-constraint-")
	require.Contains(t, emitted, contextTruncationMarker)
	require.True(t, strings.HasSuffix(emitted, "</seam-briefing>"))
	require.NotContains(t, emitted, "Seam session:")

	var injection core.Event
	for _, event := range eventsOfKind(t, events.NewRecorder(db), core.EventInjected) {
		if event.Payload["hook"] == "subagent-start" {
			injection = event
			break
		}
	}
	require.NotEmpty(t, injection.ID)
	require.Equal(t, emitted, injection.Payload["content"])
	require.Equal(t, string(ClientCodex), injection.Payload["external_client"])
	require.Equal(t, true, injection.Payload["truncated"])
	require.Equal(t, false, injection.Payload["item_ids_exact"])
	// Truncation cut the RELEVANT line, but its id stays in the superset list
	// (the assembleBriefing posture): item_ids is the only source of
	// last_injected_at, so dropping it would make the memory read as
	// stale-and-archivable.
	require.Contains(t, injection.Payload["item_ids"], "01B")
	require.Greater(t, injection.Payload["original_estimated_tokens"].(float64),
		float64(codexContextMaxTokens))
	require.LessOrEqual(t, injection.Payload["emitted_estimated_tokens"].(float64),
		float64(codexContextMaxTokens))
}

// A child briefing that fits under the Codex cap flows through whole: the
// constraint core, the prompt-matched RELEVANT section read from the child
// rollout's first user event, and the closing footer, with exact telemetry.
func TestSubagentStart_CodexRelevantSectionUnderCap(t *testing.T) {
	ts, db := newHandlerServer(t)
	const parentID = "019f8000-0000-7000-8000-000000000002"

	_, _ = post(t, ts.URL+"/api/hooks/session-start?client=codex", testKey, map[string]any{
		"session_id": parentID, "cwd": "/work/demo", "source": "startup", "model": "gpt-parent",
	})
	rollout := filepath.Join(t.TempDir(), "rollout-child-rel.jsonl")
	require.NoError(t, os.WriteFile(rollout, []byte(
		`{"type":"event_msg","payload":{"type":"user_message","message":"why does the chroma container health check race at startup"}}`+"\n"), 0o644))

	resp, out := post(t, ts.URL+"/api/hooks/subagent-start?client=codex", testKey, map[string]any{
		"session_id": parentID, "turn_id": "turn-1", "agent_id": "child-rel",
		"agent_type": "default", "cwd": "/work/demo", "model": "gpt-child",
		"permission_mode": "default", "hook_event_name": "SubagentStart",
		"transcript_path": rollout,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	emitted := additionalContext(t, out)
	require.LessOrEqual(t, retrieve.EstimateTokens(emitted), codexContextMaxTokens)
	require.Contains(t, emitted, "- no-force-push")
	require.Contains(t, emitted, "Relevant to this task:\n- chroma-boot-race: chroma container health check startup race")
	require.Contains(t, emitted, "Recall on demand with recall; read a memory with memory_read.")
	require.True(t, strings.HasSuffix(emitted, "</seam-briefing>"))
	require.NotContains(t, emitted, contextTruncationMarker)

	var injection core.Event
	for _, event := range eventsOfKind(t, events.NewRecorder(db), core.EventInjected) {
		if event.Payload["hook"] == "subagent-start" {
			injection = event
			break
		}
	}
	require.NotEmpty(t, injection.ID)
	require.Equal(t, emitted, injection.Payload["content"])
	require.Equal(t, false, injection.Payload["truncated"])
	require.Contains(t, injection.Payload["item_ids"], "01A")
	require.Contains(t, injection.Payload["item_ids"], "01B")
}

// The cap is Codex-only: the same oversized briefing on a Claude Code
// SessionStart must be emitted whole, byte-for-byte, with no truncation
// telemetry.
func TestSessionStart_ClaudeEmitsOversizedBriefingUncapped(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx := context.Background()
	require.NoError(t, store.SetSetting(ctx, db,
		store.SettingRepoProjectMap, `{"/work/demo":"demo"}`))

	for i := range 25 {
		insertMemory(t, db, fmt.Sprintf("01C%03d", i), "constraint",
			fmt.Sprintf("pinned-constraint-%03d", i), strings.Repeat("bounded pinned detail ", 12), "demo")
	}
	now := time.Now().UTC()
	for i := range 95 {
		require.NoError(t, store.CreateTask(ctx, db, core.Task{
			ID:          fmt.Sprintf("01T%03d", i),
			ProjectSlug: "demo",
			Title:       fmt.Sprintf("plan step %03d", i),
			Status:      core.TaskOpen,
			PlanSlug:    fmt.Sprintf("claude-context-plan-%03d-%s", i, strings.Repeat("x", 32)),
			CreatedAt:   now,
			UpdatedAt:   now,
		}))
	}

	ret := retrieve.New(db, nil, config.Budgets{
		MaxBriefingTokens:  1500,
		RecallBudgetTokens: 1000,
	}, nil)
	h := NewHandler(Config{
		DB: db, Retrieve: ret, Events: events.NewRecorder(db), APIKey: testKey,
	})
	mux := http.NewServeMux()
	h.Register(mux)
	body, err := json.Marshal(map[string]any{
		"session_id": "019f7291-40f1-7311-8997-0d497579d27b",
		"cwd":        "/work/demo",
		"source":     "startup",
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		"/api/hooks/session-start", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	var response hookResponse
	require.NoError(t, json.NewDecoder(rr.Body).Decode(&response))
	require.NotNil(t, response.HookSpecificOutput)
	emitted := response.HookSpecificOutput.AdditionalContext
	require.Greater(t, retrieve.EstimateTokens(emitted), codexContextMaxTokens,
		"the seeded briefing must exceed the Codex cap for this test to prove anything")
	require.NotContains(t, emitted, contextTruncationMarker)

	injected := eventsOfKind(t, events.NewRecorder(db), core.EventInjected)
	require.Len(t, injected, 1)
	require.Equal(t, emitted, injected[0].Payload["content"])
	require.Equal(t, false, injected[0].Payload["truncated"])
	require.Equal(t, injected[0].Payload["original_estimated_tokens"],
		injected[0].Payload["emitted_estimated_tokens"])
}
