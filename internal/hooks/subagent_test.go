package hooks

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// placeSubagentTranscript copies the fixture transcript into the verified
// layout (<main transcript minus .jsonl>/subagents/agent-<id>.jsonl) and
// returns the main transcript path the hook payload would carry.
func placeSubagentTranscript(t *testing.T, agentID string) string {
	t.Helper()
	fixture, err := os.ReadFile(filepath.Join("testdata", "agent-transcript.jsonl"))
	require.NoError(t, err)
	return placeSubagentTranscriptContent(t, agentID, fixture)
}

// placeSubagentTranscriptContent writes content as the subagent transcript in
// the verified layout and returns the main transcript path.
func placeSubagentTranscriptContent(t *testing.T, agentID string, content []byte) string {
	t.Helper()
	main := filepath.Join(t.TempDir(), "session.jsonl")
	sub := filepath.Join(strings.TrimSuffix(main, ".jsonl"), "subagents", "agent-"+agentID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sub), 0o755))
	require.NoError(t, os.WriteFile(sub, content, 0o644))
	return main
}

func (e *captureEnv) subagentStop(t *testing.T, body map[string]any) {
	t.Helper()
	e.postHook(t, "subagent-stop", body)
}

func TestSubagentCaptureDuringPlanning(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true})
	e.startSession(t)

	// An active draft plan gates the capture on.
	path := e.writePlanFile(t, "clever-stallman", "# My Plan\n\nDo it.\n")
	e.planWrite(t, "Write", path)

	main := placeSubagentTranscript(t, "abc123")
	e.subagentStop(t, map[string]any{
		"session_id": testSID, "cwd": "/work/demo", "permission_mode": "default",
		"transcript_path": main, "agent_id": "abc123", "agent_type": "Explore",
	})

	note, found := e.loadNote(t, "cc-agent-abc123")
	require.True(t, found)
	require.Equal(t, "[Explore] Explore the gardener package and report its structure", note.Title)
	require.Contains(t, note.Tags, "plan:my-plan")
	require.Contains(t, note.Tags, "agent-cache")
	require.Contains(t, note.Tags, "agent:Explore")
	require.Contains(t, note.Tags, "created-by:agent")
	require.Contains(t, note.Body,
		"> captured from "+ambientName(ClientClaudeCode, testSID)+" | agent abc123 | git ")
	require.Contains(t, note.Body, "## Prompt\n\nExplore the gardener package and report its structure")
	require.Contains(t, note.Body, "## Report\n\nFinal report: the gardener has three passes.")

	evs := eventsOfKind(t, e.rec, core.EventSubagentCaptured)
	require.Len(t, evs, 1)
	require.Equal(t, note.ID, evs[0].ItemID)
	require.Equal(t, "abc123", evs[0].Payload["agent_id"])
	require.Equal(t, "my-plan", evs[0].Payload["plan_slug"])
	require.Equal(t, "Final report: the gardener has three passes.", payloadString(evs[0].Payload["content"]))
	require.Contains(t, payloadString(evs[0].Payload["prompt"]), "Explore the gardener package")
}

func TestSubagentCaptureGatedOutsidePlanning(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true})
	e.startSession(t)

	main := placeSubagentTranscript(t, "abc123")
	e.subagentStop(t, map[string]any{
		"session_id": testSID, "cwd": "/work/demo", "permission_mode": "default",
		"transcript_path": main, "agent_id": "abc123", "agent_type": "Explore",
	})

	_, found := e.loadNote(t, "cc-agent-abc123")
	require.False(t, found)
	require.Empty(t, eventsOfKind(t, e.rec, core.EventSubagentCaptured))
}

func TestSubagentCapturePlanModeGate(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true})
	e.startSession(t)

	// permission_mode=="plan" gates the capture on without any captured plan;
	// the payload names the subagent transcript directly (the other resolution
	// branch). No plan slug is known, so no plan: tag.
	main := placeSubagentTranscript(t, "def456")
	sub := filepath.Join(strings.TrimSuffix(main, ".jsonl"), "subagents", "agent-def456.jsonl")
	e.subagentStop(t, map[string]any{
		"session_id": testSID, "cwd": "/work/demo", "permission_mode": "plan",
		"transcript_path": sub, "agent_id": "def456", "agent_type": "general-purpose",
	})

	note, found := e.loadNote(t, "cc-agent-def456")
	require.True(t, found)
	require.Contains(t, note.Tags, "agent-cache")
	require.Contains(t, note.Tags, "agent:general-purpose")
	for _, tag := range note.Tags {
		require.False(t, strings.HasPrefix(tag, "plan:"), "no plan tag without a correlated plan, got %q", tag)
	}
}

func TestSubagentBeforePlanAdoptedIntoComposition(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true})
	e.startSession(t)

	// Explore-first pattern: the subagent completes before any plan file exists,
	// so no plan slug can be minted yet.
	main := placeSubagentTranscript(t, "early99")
	e.subagentStop(t, map[string]any{
		"session_id": testSID, "cwd": "/work/demo", "permission_mode": "plan",
		"transcript_path": main, "agent_id": "early99", "agent_type": "Explore",
	})

	note, found := e.loadNote(t, "cc-agent-early99")
	require.True(t, found)
	require.Empty(t, plans.SlugFromTags(note.Tags))
	require.Equal(t, []any{"cc-agent-early99"}, e.sessionPlanMeta(t)["pending_agents"],
		"the parked note slug rides on the session until a plan exists")

	// The first plan capture mints the slug and adopts the parked agent note.
	path := e.writePlanFile(t, "clever-stallman", "# My Plan\n\nDo it.\n")
	e.planWrite(t, "Write", path)

	note, found = e.loadNote(t, "cc-agent-early99")
	require.True(t, found)
	require.Contains(t, note.Tags, "plan:my-plan")
	require.Contains(t, note.Tags, "agent-cache")
	require.Nil(t, e.sessionPlanMeta(t)["pending_agents"], "pending list drains at adoption")

	evs := eventsOfKind(t, e.rec, core.EventPlanCaptured)
	require.Len(t, evs, 1)
	require.Equal(t, float64(1), evs[0].Payload["adopted_agents"])

	// A later subagent (slug now known) is tagged directly, no pending detour.
	main2 := placeSubagentTranscript(t, "late42")
	e.subagentStop(t, map[string]any{
		"session_id": testSID, "cwd": "/work/demo", "permission_mode": "plan",
		"transcript_path": main2, "agent_id": "late42", "agent_type": "Explore",
	})
	note, found = e.loadNote(t, "cc-agent-late42")
	require.True(t, found)
	require.Contains(t, note.Tags, "plan:my-plan")
	require.Nil(t, e.sessionPlanMeta(t)["pending_agents"])
}

// Claude Code fires SubagentStop for agents that never write a transcript, at
// turn cadence while a plan is unapproved. Nothing is captured and nothing is
// warned: the 204 WARNs those stops produced were the bulk of the daemon log.
// (A transcript that exists but yields nothing is still a warning; exercising
// it here would spend the full settle wait.)
func TestSubagentCaptureMissingTranscriptFailsOpen(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true})
	e.startSession(t)

	path := e.writePlanFile(t, "clever-stallman", "# My Plan\n\nDo it.\n")
	e.planWrite(t, "Write", path)

	e.subagentStop(t, map[string]any{
		"session_id": testSID, "cwd": "/work/demo", "permission_mode": "default",
		"transcript_path": filepath.Join(t.TempDir(), "nope.jsonl"), "agent_id": "ghost1", "agent_type": "Explore",
	})

	_, found := e.loadNote(t, "cc-agent-ghost1")
	require.False(t, found)
	require.Empty(t, eventsOfKind(t, e.rec, core.EventSubagentCaptured))
	require.NotContains(t, e.logs.String(), "level=WARN", e.logs.String())
	require.Contains(t, e.logs.String(), "subagent left no transcript")
	require.Contains(t, e.logs.String(), "agent_type=Explore")
}

// Claude Code's SubagentStop names the child transcript itself
// (agent_transcript_path); that wins over the layout derived from the parent's
// transcript_path, which here points nowhere.
func TestSubagentCaptureUsesAgentTranscriptPath(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true})
	e.startSession(t)

	path := e.writePlanFile(t, "clever-stallman", "# My Plan\n\nDo it.\n")
	e.planWrite(t, "Write", path)

	main := placeSubagentTranscript(t, "named1")
	e.subagentStop(t, map[string]any{
		"session_id": testSID, "cwd": "/work/demo", "permission_mode": "default",
		"transcript_path":       filepath.Join(t.TempDir(), "elsewhere.jsonl"),
		"agent_transcript_path": filepath.Join(strings.TrimSuffix(main, ".jsonl"), "subagents", "agent-named1.jsonl"),
		"agent_id":              "named1", "agent_type": "Explore",
	})

	note, found := e.loadNote(t, "cc-agent-named1")
	require.True(t, found)
	require.Contains(t, note.Body, "## Report\n\nFinal report: the gardener has three passes.")
}

// Codex SubagentStop shares the parent session_id but is not a Claude planning
// capture. Even a plan permission mode and a readable child transcript must not
// create a cc-agent note or overwrite the main-turn findings/model.
func TestCodexSubagentStop_OnlyHeartbeatsParent(t *testing.T) {
	e := newCaptureEnv(t, config.PlanCapture{Enabled: true, AutoTask: true})
	const parentID = "019f8000-0000-7000-8000-000000000001"

	resp, _ := post(t, e.ts.URL+"/api/hooks/session-start?client=codex", testKey, map[string]any{
		"session_id": parentID, "cwd": "/work/demo", "source": "startup", "model": "gpt-parent",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp, _ = post(t, e.ts.URL+"/api/hooks/stop?client=codex", testKey, map[string]any{
		"session_id": parentID, "cwd": "/work/demo", "model": "gpt-parent",
		"last_assistant_message": "parent main-turn findings",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, store.TouchAmbientSession(context.Background(), e.db,
		ClientCodex.externalIdentity(), parentID, time.Now().UTC().Add(-time.Hour)))
	before := requireAmbientSession(t, e.db, ClientCodex, parentID)

	main := placeSubagentTranscript(t, "codex-child")
	child := filepath.Join(strings.TrimSuffix(main, ".jsonl"), "subagents", "agent-codex-child.jsonl")
	resp, out := post(t, e.ts.URL+"/api/hooks/subagent-stop?client=codex", testKey, map[string]any{
		"session_id": parentID, "turn_id": "turn-1", "cwd": "/work/demo",
		"transcript_path": main, "agent_transcript_path": child,
		"permission_mode": "plan", "hook_event_name": "SubagentStop",
		"model": "gpt-child", "agent_id": "codex-child", "agent_type": "default",
		"last_assistant_message": "child report must not become parent findings",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, true, out["continue"])
	require.Nil(t, out["hookSpecificOutput"], "SubagentStop cannot inject")

	after := requireAmbientSession(t, e.db, ClientCodex, parentID)
	require.Equal(t, before.Name, after.Name)
	require.Equal(t, before.ProjectSlug, after.ProjectSlug)
	require.Equal(t, before.Model, after.Model, "a child model must not re-attribute the parent")
	require.Equal(t, before.Findings, after.Findings)
	require.True(t, after.UpdatedAt.After(before.UpdatedAt), "SubagentStop heartbeats the proven parent id")
	require.Equal(t, "(auto-harvested) parent main-turn findings", after.Findings)
	require.NotContains(t, after.Findings, "child report")

	_, found := e.loadNote(t, "cc-agent-codex-child")
	require.False(t, found, "generic Codex workers must not create durable plan notes")
	require.Empty(t, eventsOfKind(t, e.rec, core.EventSubagentCaptured))
}

// TestSubagentTranscriptPathRejectsTraversalAgentID pins the agent-id guard:
// the id is a filename fragment, so one carrying separators or ".." must not be
// joined into the transcript path (it could point the read anywhere on disk).
func TestSubagentTranscriptPathRejectsTraversalAgentID(t *testing.T) {
	base := subagentPayload{TranscriptPath: "/tmp/sess.jsonl"}

	for _, id := range []string{"../../../etc/passwd", "a/b", `a\b`, "a..b"} {
		p := base
		p.AgentID = id
		require.Empty(t, subagentTranscriptPath(p), "agent id %q must be rejected", id)
	}

	ok := base
	ok.AgentID = "agent01"
	require.Equal(t, filepath.Join("/tmp/sess", "subagents", "agent-agent01.jsonl"),
		subagentTranscriptPath(ok))
}

// subagentSpawnPrompt (Claude Code): the child transcript's first user message
// is the spawn prompt; every failure mode -- absent, empty, malformed,
// oversized -- yields "" silently.
func TestSubagentSpawnPrompt_ClaudeCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		main func(t *testing.T) string
		want string
	}{
		{
			"prompt present",
			func(t *testing.T) string { return placeSubagentTranscript(t, "sp1") },
			"Explore the gardener package and report its structure",
		},
		{
			"transcript not yet written (file absent)",
			func(t *testing.T) string { return filepath.Join(t.TempDir(), "session.jsonl") },
			"",
		},
		{
			"file empty",
			func(t *testing.T) string { return placeSubagentTranscriptContent(t, "sp1", nil) },
			"",
		},
		{
			"malformed lines only",
			func(t *testing.T) string {
				return placeSubagentTranscriptContent(t, "sp1", []byte("{not json\n\nplain text\n"))
			},
			"",
		},
		{
			"prompt line exceeding the scanner bound",
			func(t *testing.T) string {
				oversized := `{"type":"user","message":{"role":"user","content":"` +
					strings.Repeat("x", 17*1024*1024) + `"}}` + "\n"
				return placeSubagentTranscriptContent(t, "sp1", []byte(oversized))
			},
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := subagentPayload{
				ParentSessionID: "parent", AgentID: "sp1", AgentType: "Explore",
				CWD: "/work/demo", TranscriptPath: tc.main(t),
			}
			require.Equal(t, tc.want, subagentSpawnPrompt(ClientClaudeCode, p))
		})
	}
}

// The subagent path populates BriefingInput.Prompt for both clients; a missing
// transcript leaves it empty without disturbing the rest of the input. The
// briefing matches the field against project memories to render its RELEVANT
// section (the briefing_prompt tests in internal/retrieve cover that half).
func TestSubagentBriefingInput_PopulatesPrompt(t *testing.T) {
	// Claude Code: the child transcript exists when the hook fires.
	p42 := subagentPayload{
		ParentSessionID: "parent", AgentID: "sp42", AgentType: "Explore",
		CWD: "/work/demo", TranscriptPath: placeSubagentTranscript(t, "sp42"),
	}
	in := subagentBriefingInput(p42, "", subagentSpawnPrompt(ClientClaudeCode, p42))
	require.Equal(t, "/work/demo", in.CWD)
	require.Equal(t, "Explore", in.AgentType)
	require.Equal(t, "Explore the gardener package and report its structure", in.Prompt)

	// Codex: the Start fixture's transcript_path names the CHILD rollout (its
	// basename carries the agent_id), unlike Stop where it names the parent.
	p := decodeSubagentStart(ClientCodex, codexFixture(t, "tui", "subagent-start.input.json"))
	require.Contains(t, filepath.Base(p.TranscriptPath), p.AgentID,
		"SubagentStart transcript_path must name the child rollout")
	p.TranscriptPath = rolloutPath() // stand the committed rollout fixture in as the child rollout
	in = subagentBriefingInput(p, "", subagentSpawnPrompt(ClientCodex, p))
	require.Equal(t, p.CWD, in.CWD)
	require.Equal(t, p.AgentType, in.AgentType)
	require.Equal(t, rolloutUserPrompt, in.Prompt)

	// Not-yet-flushed transcript: the prompt is empty, the input intact.
	p43 := subagentPayload{
		ParentSessionID: "parent", AgentID: "sp43", AgentType: "Explore",
		CWD: "/work/demo", TranscriptPath: filepath.Join(t.TempDir(), "missing.jsonl"),
	}
	in = subagentBriefingInput(p43, "", subagentSpawnPrompt(ClientClaudeCode, p43))
	require.Equal(t, "/work/demo", in.CWD)
	require.Empty(t, in.Prompt)
}

// The RELEVANT section end to end (Claude Code): the child transcript supplies
// the spawn prompt, the matched memory renders between the constraint tiers
// and the footer, its id joins the injected-ids telemetry, and that telemetry
// stays weight 0 for utility -- a briefing inject is exposure, never
// prompt-class demand, even though the prompt matcher produced it (the
// closed-loop-utility-signal-contract boundary).
func TestSubagentStart_RelevantInjectsAreWeightZero(t *testing.T) {
	ts, db := newHandlerServer(t)
	ctx := context.Background()
	const parent = "cc-parent-relevant-0001"

	_, _ = post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": parent, "cwd": "/work/demo", "source": "startup",
	})

	transcript := placeSubagentTranscriptContent(t, "rel1", []byte(
		`{"type":"user","message":{"role":"user","content":"why does the chroma container health check race at startup"}}`+"\n"))
	resp, out := post(t, ts.URL+"/api/hooks/subagent-start", testKey, map[string]any{
		"session_id": parent, "agent_id": "rel1", "agent_type": "Explore",
		"cwd": "/work/demo", "transcript_path": transcript,
		"permission_mode": "default", "hook_event_name": "SubagentStart",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	ac := additionalContext(t, out)
	require.Contains(t, ac, "- no-force-push")
	require.Contains(t, ac, "Relevant to this task:\n- chroma-boot-race: chroma container health check startup race")
	require.Greater(t, strings.Index(ac, "Relevant to this task:"), strings.Index(ac, "Constraints (binding for every session):"),
		"the relevant section renders after the constraint tiers")
	require.Contains(t, ac, "Recall on demand with recall; read a memory with memory_read.")
	require.True(t, strings.HasSuffix(ac, "</seam-briefing>"))

	// The RELEVANT id joins the injected-ids instrumentation on the
	// subagent-start surface, alongside the constraints'.
	var injection core.Event
	for _, event := range eventsOfKind(t, events.NewRecorder(db), core.EventInjected) {
		if event.Payload["hook"] == "subagent-start" {
			injection = event
			break
		}
	}
	require.NotEmpty(t, injection.ID, "the subagent-start injection must be recorded")
	require.Contains(t, injection.Payload["item_ids"], "01A")
	require.Contains(t, injection.Payload["item_ids"], "01B")

	// Weight 0: rebuilding retrieval stats from the event log credits the
	// RELEVANT memory with exposure only. No prompt-class (or any other)
	// demand component may appear -- demand stays the child's own subsequent
	// memory_read/recall calls.
	require.NoError(t, store.RebuildRetrievalStats(ctx, db))
	stat, ok, err := store.GetRetrievalStat(ctx, db, "01B")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 2, stat.InjectCount, "session-start index line + subagent-start RELEVANT line")
	require.Zero(t, stat.Utility, "a RELEVANT inject must not accrue utility")
	require.True(t, stat.Components.IsZero(), "no demand in any class, prompt included")
}

func TestParseSubagentTranscript(t *testing.T) {
	prompt, report, complete := parseSubagentTranscript(filepath.Join("testdata", "agent-transcript.jsonl"))
	require.Equal(t, "Explore the gardener package and report its structure", prompt,
		"prompt is the FIRST user message, not later tool feedback")
	require.Equal(t, "Final report: the gardener has three passes.", report,
		"report is the LAST assistant text")
	require.True(t, complete, "the fixture ends on a terminal assistant text line")

	prompt, report, complete = parseSubagentTranscript("")
	require.Empty(t, prompt)
	require.Empty(t, report)
	require.False(t, complete)
}
