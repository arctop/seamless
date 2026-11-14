package hooks

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// Records follow the home (isolation.requests on, home.go), in the advisory
// tier: no peer_auth, so the conversation is the client's own session (its
// client and the session id it issued). Generic projects from route_test.go:
// example-ops and example-docs open, example-finance and example-legal
// confidential, example-sealed sealed.

const (
	homeLegalCWD = "/work/legal"
	// The prompts each match one memory homeSeed plants, and nothing else.
	homeFinPrompt   = "check the ledger reconciliation for the quarter close"
	homeOpsPrompt   = "the deploy pipeline health probe has a startup race"
	homeLegalPrompt = "review the indemnity clause redline"
)

// homeNewEnv is routeEnv with isolation.requests as on says, example-legal
// mapped to a repository of its own, and memories planted for the briefings
// and the prompt matcher (homeSeed).
func homeNewEnv(t *testing.T, on bool) *routeEnv {
	t.Helper()
	e := newRouteEnvWith(t, func(c *Config) { c.ReadGrants = on })
	require.NoError(t, store.SetSetting(context.Background(), e.db, store.SettingRepoProjectMap,
		`{"`+rtOpsCWD+`":"`+rtOps+`","`+rtFinCWD+`":"`+rtFin+`","`+rtSealedCWD+`":"`+rtSealed+
			`","`+homeLegalCWD+`":"`+rtLegal+`"}`))
	homeSeed(t, e.db)
	return e
}

// homeSeed plants four memories in each of example-ops, example-finance and
// example-legal -- a constraint the briefing pins, and three the prompt
// matcher scores, one per home prompt -- and a constraint in example-sealed.
func homeSeed(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, m := range []struct{ id, kind, name, desc, project string }{
		{"01HOPS1", "constraint", "ops-rollback-rule", "roll back a release within one hour", rtOps},
		{"01HOPS2", "gotcha", "ops-deploy-probe-race", "deploy pipeline health probe startup race", rtOps},
		{"01HOPS3", "reference", "ops-cache-ttl", "edge cache entries live five minutes", rtOps},
		{"01HOPS4", "decision", "ops-paging-tiers", "paging escalates primary then secondary", rtOps},
		{"01HFIN1", "constraint", "fin-approval-rule", "two people approve every wire transfer", rtFin},
		{"01HFIN2", "gotcha", "fin-ledger-close", "ledger reconciliation quarter close checklist", rtFin},
		{"01HFIN3", "reference", "fin-vendor-terms", "vendor payment terms net thirty", rtFin},
		{"01HFIN4", "decision", "fin-audit-trail", "audit trail retention seven years", rtFin},
		{"01HLEG1", "constraint", "legal-privilege-rule", "privileged memos stay with counsel", rtLegal},
		{"01HLEG2", "gotcha", "legal-indemnity-clause", "indemnity clause redline negotiation playbook", rtLegal},
		{"01HLEG3", "reference", "legal-nda-template", "mutual nda template version three", rtLegal},
		{"01HLEG4", "decision", "legal-filing-calendar", "court filing calendar deadlines", rtLegal},
		{"01HSEA1", "constraint", "sealed-vault-rule", "vault keys never leave the room", rtSealed},
	} {
		insertMemory(t, db, m.id, m.kind, m.name, m.desc, m.project)
	}
}

// homeMove approves a move of the conversation whose ambient row is amb into
// target, held by the conversation (the advisory tier), as approval's
// ApproveMove records one: store.MoveApproveWith ends amb with its pointer and
// opens the child as the conversation's ambient row.
func homeMove(t *testing.T, db *sql.DB, client Client, amb core.Session, target string) store.MoveApproved {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	carry, err := store.CarryOf(amb.ProjectSlug, []string{target}, []string{target})
	require.NoError(t, err)
	a, err := store.MoveRequestCreate(ctx, db, store.MoveRequest{
		Project: target, From: amb.ProjectSlug, Carry: carry, Lease: time.Hour, TTL: 15 * time.Minute,
		ConversationClient: client.externalIdentity(), ConversationID: amb.ExternalSessionID,
		SessionID: amb.ID, SessionName: amb.Name,
	}, now)
	require.NoError(t, err)
	done, err := store.MoveApproveWith(ctx, db, store.MoveDecision{ID: a.ID, Challenge: a.Challenge, DecidedBy: "owner"}, now)
	require.NoError(t, err)
	return done
}

// homeRecall posts the conversation's UserPromptSubmit from cwd and returns
// what was injected.
func homeRecall(t *testing.T, e *routeEnv, client Client, cwd, prompt string) string {
	t.Helper()
	out, _ := e.hook(client, "user-prompt-submit", map[string]any{"session_id": rtConv, "cwd": cwd, "user_prompt": prompt})
	return additionalContext(t, out)
}

// homeSubagentStop posts a planning subagent's SubagentStop from cwd for the
// conversation, returning the refusal notice.
func homeSubagentStop(t *testing.T, e *routeEnv, cwd, agentID string) string {
	t.Helper()
	_, refused := e.hook(ClientClaudeCode, "subagent-stop", map[string]any{
		"session_id": rtConv, "cwd": cwd, "permission_mode": "plan",
		"transcript_path": placeSubagentTranscript(t, agentID), "agent_id": agentID, "agent_type": "Explore",
	})
	return refused
}

// homeSubagentStart posts a subagent's SubagentStart from cwd whose spawn
// prompt is prompt, returning its briefing.
func homeSubagentStart(t *testing.T, e *routeEnv, cwd, agentID, prompt string) string {
	t.Helper()
	main := placeSubagentTranscriptContent(t, agentID,
		[]byte(`{"type":"user","message":{"role":"user","content":"`+prompt+`"}}`+"\n"))
	out, _ := e.hook(ClientClaudeCode, "subagent-start", map[string]any{
		"session_id": rtConv, "cwd": cwd, "agent_id": agentID, "agent_type": "Explore", "transcript_path": main,
	})
	return additionalContext(t, out)
}

// homeAgentNote reports whether the subagent's capture landed in project.
func homeAgentNote(t *testing.T, e *routeEnv, project, agentID string) bool {
	t.Helper()
	_, ok, err := store.NoteBySlug(context.Background(), e.db, project, plans.AgentNotePrefix+agentID)
	require.NoError(t, err)
	return ok
}

// homeOnlyIn requires that text is held in project and in no other: no event,
// session or search row anywhere else names it.
func homeOnlyIn(t *testing.T, e *routeEnv, text, project string) {
	t.Helper()
	for kind, by := range e.whereIs(text) {
		for p := range by {
			require.Equal(t, project, p, "%s rows hold %q", kind, text)
		}
	}
}

// homeHookEvents returns the events of kind a hook recorded, by hook name.
func homeHookEvents(t *testing.T, e *routeEnv, kind core.EventKind, hook string) []core.Event {
	t.Helper()
	var out []core.Event
	for _, ev := range eventsOfKind(t, e.rec, kind) {
		if ev.Payload["hook"] == hook {
			out = append(out, ev)
		}
	}
	return out
}

// A conversation that moved from an open project into a confidential one
// keeps its whole record in the project it moved into, wherever its hooks fire
// from (the agent still works in the open project's repository): the plan and
// the planning subagent's report land there, filed under the child session; a
// prompt recalls the new home's memories and not the old one's, its events
// filed under the child; the end's harvest is the child's findings; and the
// session the move ended keeps its pointer and nothing more.
func TestHome_MovedConversationRecordsGoHome(t *testing.T) {
	e := homeNewEnv(t, true)
	amb := e.start(ClientClaudeCode, rtOpsCWD)
	m := homeMove(t, e.db, ClientClaudeCode, amb, rtFin)
	child := m.Child
	require.Equal(t, rtFin, child.ProjectSlug)

	require.Empty(t, clPlan(t, e, rtOpsCWD, "quiet-ledger"))
	require.True(t, clCaptured(t, e, rtFin, "quiet-ledger"))
	require.False(t, clCaptured(t, e, rtOps, "quiet-ledger"))
	captured := eventsOfKind(t, e.rec, core.EventPlanCaptured)
	require.Len(t, captured, 1)
	require.Equal(t, child.ID, captured[0].SessionID)
	require.Equal(t, rtFin, captured[0].ProjectSlug)
	_, onChild := e.session(child.ID).Metadata["plan_capture"].(map[string]any)
	require.True(t, onChild, "the plan's tracking metadata is the child's")

	require.Empty(t, homeSubagentStop(t, e, rtOpsCWD, "ag51"))
	require.True(t, homeAgentNote(t, e, rtFin, "ag51"))
	require.False(t, homeAgentNote(t, e, rtOps, "ag51"))
	subs := eventsOfKind(t, e.rec, core.EventSubagentCaptured)
	require.Len(t, subs, 1)
	require.Equal(t, child.ID, subs[0].SessionID)

	require.Contains(t, homeRecall(t, e, ClientClaudeCode, rtOpsCWD, homeFinPrompt), "fin-ledger-close")
	require.Empty(t, homeRecall(t, e, ClientClaudeCode, rtOpsCWD, homeOpsPrompt),
		"the project it left is not the conversation's scope")
	injected := homeHookEvents(t, e, core.EventInjected, "user-prompt-submit")
	require.Len(t, injected, 1)
	require.Equal(t, child.ID, injected[0].SessionID)
	require.Equal(t, rtFin, injected[0].ProjectSlug)
	require.Equal(t, homeFinPrompt, injected[0].Payload["prompt"])
	missed := eventsOfKind(t, e.rec, core.EventHookPrompt)
	require.Len(t, missed, 1)
	require.Equal(t, child.ID, missed[0].SessionID)
	require.Equal(t, rtFin, missed[0].ProjectSlug)

	require.Empty(t, e.end())
	got := e.session(child.ID)
	require.Equal(t, core.SessionCompleted, got.Status)
	require.Equal(t, rtHarvest, got.Findings)
	left := e.session(m.Left.ID)
	require.Equal(t, store.MovePointer(rtFin, child.Name), left.Findings, "the session the move ended keeps its pointer")
	require.Empty(t, e.endedEvents(left.ID), "and the end leaves it ended as the move left it")
	homeOnlyIn(t, e, rtSecret, rtFin)
}

// Resuming a moved conversation resumes the child in its home and briefs it
// from there, keyed on the client's session id, wherever the resume runs from
// -- after the conversation ended (resume) or within it (compact). The
// session the move ended stays ended.
func TestHome_MovedConversationResumesAtHome(t *testing.T) {
	for _, source := range []string{"resume", "compact"} {
		t.Run(source, func(t *testing.T) {
			e := homeNewEnv(t, true)
			amb := e.start(ClientClaudeCode, rtOpsCWD)
			m := homeMove(t, e.db, ClientClaudeCode, amb, rtFin)
			if source == "resume" {
				require.Empty(t, e.end())
			}
			out, _ := e.hook(ClientClaudeCode, "session-start", map[string]any{"session_id": rtConv, "cwd": rtOpsCWD, "source": source})
			briefing := additionalContext(t, out)
			require.Contains(t, briefing, "Seam project: "+rtFin)
			require.Contains(t, briefing, "fin-approval-rule")
			require.NotContains(t, briefing, "ops-rollback-rule")
			require.NotContains(t, briefing, "Moved to project", "the pointer it left behind is not news here")
			require.Contains(t, briefing, "Seam session: "+m.Child.Name+" (ambient)")
			child := e.session(m.Child.ID)
			require.Equal(t, core.SessionActive, child.Status)
			require.Equal(t, rtFin, child.ProjectSlug)
			require.Equal(t, core.SessionCompleted, e.session(m.Left.ID).Status)
			starts := homeHookEvents(t, e, core.EventInjected, "session-start")
			last := starts[0] // newest first
			require.Equal(t, child.ID, last.SessionID)
			require.Equal(t, rtFin, last.ProjectSlug)
		})
	}
}

// A subagent shares its parent conversation's home: after a move its briefing
// is the new home's -- its constraint, and the memory its spawn prompt matches
// there -- and its report lands there (TestHome_MovedConversationRecordsGoHome).
func TestHome_SubagentSharesTheHome(t *testing.T) {
	e := homeNewEnv(t, true)
	amb := e.start(ClientClaudeCode, rtOpsCWD)
	m := homeMove(t, e.db, ClientClaudeCode, amb, rtFin)
	b := homeSubagentStart(t, e, rtOpsCWD, "ag52", homeFinPrompt)
	require.Contains(t, b, "Seam project: "+rtFin)
	require.Contains(t, b, "fin-approval-rule")
	require.Contains(t, b, "fin-ledger-close", "the RELEVANT section reads the home")
	require.NotContains(t, b, "ops-rollback-rule")
	starts := homeHookEvents(t, e, core.EventInjected, "subagent-start")
	require.Len(t, starts, 1)
	require.Equal(t, m.Child.ID, starts[0].SessionID)
	require.Equal(t, rtFin, starts[0].ProjectSlug)
}

// A conversation native to a confidential project that holds a read grant of
// another keeps its record at home, wherever its hooks fire from: its plan,
// its subagent's report and its harvest land in its home; its prompts recall
// the home and the granted project; a subagent's briefing is the home's, its
// RELEVANT section reading the granted project too; and a resume from an open
// project's repository stays home and is briefed from there.
func TestHome_GrantHolderInAConfidentialHome(t *testing.T) {
	e := homeNewEnv(t, true)
	amb := e.start(ClientClaudeCode, homeLegalCWD)
	require.Equal(t, rtLegal, amb.ProjectSlug)
	approveGrant(t, &captureEnv{db: e.db}, rtLegal, rtFin, amb.ID, "")

	require.Contains(t, homeRecall(t, e, ClientClaudeCode, rtOpsCWD, homeFinPrompt), "fin-ledger-close",
		"the granted project joins the recall")
	require.Contains(t, homeRecall(t, e, ClientClaudeCode, rtOpsCWD, homeLegalPrompt), "legal-indemnity-clause")
	require.Empty(t, homeRecall(t, e, ClientClaudeCode, rtOpsCWD, homeOpsPrompt))

	require.Empty(t, clPlan(t, e, rtOpsCWD, "legal-plan"))
	require.True(t, clCaptured(t, e, rtLegal, "legal-plan"))
	for _, p := range []string{rtOps, rtFin} {
		require.False(t, clCaptured(t, e, p, "legal-plan"), p)
	}
	require.Empty(t, homeSubagentStop(t, e, rtOpsCWD, "ag61"))
	require.True(t, homeAgentNote(t, e, rtLegal, "ag61"))
	require.False(t, homeAgentNote(t, e, rtOps, "ag61"))

	b := homeSubagentStart(t, e, rtOpsCWD, "ag62", homeFinPrompt)
	require.Contains(t, b, "Seam project: "+rtLegal)
	require.Contains(t, b, "legal-privilege-rule")
	require.Contains(t, b, "fin-ledger-close", "the RELEVANT section reads the granted project")
	require.NotContains(t, b, "fin-approval-rule", "the granted project's constraints bind its own sessions")
	require.NotContains(t, b, "ops-rollback-rule")

	out, _ := e.hook(ClientClaudeCode, "session-start", map[string]any{"session_id": rtConv, "cwd": rtOpsCWD, "source": "compact"})
	briefing := additionalContext(t, out)
	require.Contains(t, briefing, "Seam project: "+rtLegal)
	require.NotContains(t, briefing, "ops-rollback-rule")
	require.Equal(t, rtLegal, e.session(amb.ID).ProjectSlug, "a resume carries nothing into the open project")

	require.Empty(t, e.end())
	require.Equal(t, rtHarvest, e.session(amb.ID).Findings)
	homeOnlyIn(t, e, rtSecret, rtLegal)
}

// /clear ends the conversation and starts a new one (a new client session id).
// Without peer_auth the new conversation holds nothing, so its home is the
// cwd's project again and its record lands there -- after a move and after a
// grant alike.
func TestHome_ClearStartsANewConversation(t *testing.T) {
	for _, tc := range []struct {
		name, cwd string
		reach     func(e *routeEnv, amb core.Session)
	}{
		{"after a move", rtOpsCWD, func(e *routeEnv, amb core.Session) { homeMove(t, e.db, ClientClaudeCode, amb, rtFin) }},
		{"after a grant", rtOpsCWD, func(e *routeEnv, amb core.Session) {
			approveGrant(t, &captureEnv{db: e.db}, rtOps, rtLegal, amb.ID, "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := homeNewEnv(t, true)
			tc.reach(e, e.start(ClientClaudeCode, tc.cwd))
			transcript := writeTranscript(t,
				`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Cleared."}]}}`)
			e.hook(ClientClaudeCode, "session-end", map[string]any{"session_id": rtConv, "transcript_path": transcript, "reason": "clear"})

			const next = "conv-5a1d-0002"
			out, _ := e.hook(ClientClaudeCode, "session-start", map[string]any{"session_id": next, "cwd": rtOpsCWD, "source": "clear"})
			require.Contains(t, additionalContext(t, out), "Seam project: "+rtOps)
			fresh := requireAmbientSession(t, e.db, ClientClaudeCode, next)
			require.Equal(t, rtOps, fresh.ProjectSlug)

			_, refused := e.hook(ClientClaudeCode, "post-tool-use", map[string]any{
				"session_id": next, "cwd": rtOpsCWD, "tool_name": "Write",
				"tool_input": map[string]any{"file_path": homePlanFile(t, e, "fresh-plan")},
			})
			require.Empty(t, refused)
			require.True(t, clCaptured(t, e, rtOps, "fresh-plan"), "the new conversation's home takes it")

			out, _ = e.hook(ClientClaudeCode, "user-prompt-submit", map[string]any{"session_id": next, "cwd": rtOpsCWD, "user_prompt": homeOpsPrompt})
			require.Contains(t, additionalContext(t, out), "ops-deploy-probe-race")

			_, refused = e.hook(ClientClaudeCode, "session-end", map[string]any{
				"session_id": next, "transcript_path": writeTranscript(t,
					`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Fresh start."}]}}`),
				"reason": "logout",
			})
			require.Empty(t, refused)
			require.Equal(t, "(auto-harvested) Fresh start.", requireAmbientSession(t, e.db, ClientClaudeCode, next).Findings)
		})
	}
}

// homePlanFile writes a plan file under the env's plans dir and returns its
// path.
func homePlanFile(t *testing.T, e *routeEnv, base string) string {
	t.Helper()
	path := filepath.Join(e.plansDir, base+".md")
	require.NoError(t, os.WriteFile(path, []byte("# Fresh plan\n\n1. Step one.\n"), 0o644))
	return path
}

// A conversation whose home is not cleared for what it holds -- an open home
// holding a confidential project's content by a read grant from it --
// records nothing there and nothing anywhere: the end's harvest leaves the
// not-recorded line, a Codex turn's findings the same line once, a capture is
// dropped with the notice, and its prompt events carry none of its text.
func TestHome_UnclearedHomeRecordsNothing(t *testing.T) {
	notice := func(what string) string {
		return "Seamless did not record " + what + ": this session holds content from " + rtLegal +
			", so it writes only into " + rtLegal + ", and this would land in project " + rtOps + "."
	}
	line := "Not recorded: this session holds content from " + rtLegal +
		", and its findings may not be written into project " + rtOps + "."

	t.Run("claude code", func(t *testing.T) {
		e := homeNewEnv(t, true)
		amb := e.start(ClientClaudeCode, rtOpsCWD)
		approveGrant(t, &captureEnv{db: e.db}, rtOps, rtLegal, amb.ID, "")

		require.Equal(t, notice("this plan"), clPlan(t, e, rtOpsCWD, "away"))
		require.Equal(t, notice("this subagent's report"), homeSubagentStop(t, e, rtOpsCWD, "ag71"))
		for _, p := range []string{rtOps, rtLegal} {
			require.False(t, clCaptured(t, e, p, "away"), p)
			require.False(t, homeAgentNote(t, e, p, "ag71"), p)
		}

		// A read is no write: the grant's target still joins the recall, but
		// what was injected is not copied where the record may not go.
		require.Contains(t, homeRecall(t, e, ClientClaudeCode, rtOpsCWD, homeLegalPrompt+" "+rtSecret), "legal-indemnity-clause")
		injected := homeHookEvents(t, e, core.EventInjected, "user-prompt-submit")
		require.Len(t, injected, 1)
		require.Equal(t, amb.ID, injected[0].SessionID)
		require.Equal(t, "", injected[0].Payload["content"])
		require.NotContains(t, injected[0].Payload, "prompt")
		require.Equal(t, true, injected[0].Payload["text_withheld"])
		require.NotEmpty(t, injected[0].Payload["item_ids"], "what was injected still counts")
		require.Empty(t, homeRecall(t, e, ClientClaudeCode, rtOpsCWD, "weather report "+rtSecret))
		require.Empty(t, eventsOfKind(t, e.rec, core.EventHookPrompt), "a miss is the prompt alone: not recorded")

		require.Equal(t, notice("this conversation's findings"), e.end())
		got := e.session(amb.ID)
		require.Equal(t, core.SessionCompleted, got.Status)
		require.Equal(t, line, got.Findings)
		ended := e.endedEvents(amb.ID)
		require.Len(t, ended, 1)
		require.Equal(t, false, ended[0].Payload["harvested"])
		require.Equal(t, line, ended[0].Payload["findings"])
		require.Equal(t, map[string]map[string]int{"events": {}, "sessions": {}, "fts": {}}, e.whereIs(rtSecret))
	})

	t.Run("codex", func(t *testing.T) {
		e := homeNewEnv(t, true)
		amb := e.start(ClientCodex, rtOpsCWD)
		stop := func(msg string) string {
			_, refused := e.hook(ClientCodex, "stop", map[string]any{"session_id": rtConv, "cwd": rtOpsCWD, "last_assistant_message": msg})
			return refused
		}
		require.Empty(t, stop("Opened the repository."))
		require.Equal(t, "(auto-harvested) Opened the repository.", e.session(amb.ID).Findings, "before the grant, as always")
		approveGrant(t, &captureEnv{db: e.db}, rtOps, rtLegal, amb.ID, "")
		require.Equal(t, notice("this turn's findings"), stop(rtSecret+" turn one."))
		require.Equal(t, line, e.session(amb.ID).Findings)
		require.Equal(t, notice("this turn's findings"), stop(rtSecret+" turn two."))
		require.Equal(t, line, e.session(amb.ID).Findings, "one line, however many turns")
		require.Equal(t, map[string]map[string]int{"events": {}, "sessions": {}, "fts": {}}, e.whereIs(rtSecret))
	})
}

// After a move a Codex conversation's Stop findings are the child's, every
// turn, while the session the move ended keeps its pointer.
func TestHome_MovedCodexConversation(t *testing.T) {
	e := homeNewEnv(t, true)
	amb := e.start(ClientCodex, rtOpsCWD)
	stop := func(msg string) string {
		_, refused := e.hook(ClientCodex, "stop", map[string]any{"session_id": rtConv, "cwd": rtOpsCWD, "last_assistant_message": msg})
		return refused
	}
	require.Empty(t, stop("Opened the repository."))
	m := homeMove(t, e.db, ClientCodex, e.session(amb.ID), rtFin)
	require.Equal(t, "(auto-harvested) Opened the repository.\n\n"+store.MovePointer(rtFin, m.Child.Name),
		e.session(m.Left.ID).Findings, "the move keeps the turn it ended on, with its pointer")
	require.Empty(t, stop(rtSecret+" turn one."))
	require.Equal(t, "(auto-harvested) "+rtSecret+" turn one.", e.session(m.Child.ID).Findings)
	require.Empty(t, stop(rtSecret+" turn two."))
	require.Equal(t, "(auto-harvested) "+rtSecret+" turn two.", e.session(m.Child.ID).Findings)
	homeOnlyIn(t, e, rtSecret, rtFin)
}

// A resume carries the conversation, and everything it holds, into the
// project its cwd resolves to. With the switch on it does so only where the
// home may write (store.CanWrite over the clearance): a conversation holding
// fenced content stays home, and a conversation from an open home reaches a
// sealed project only by moving in. One that holds nothing resumes into
// another open or a confidential project as always. Off, every resume
// re-scopes, as before.
func TestHome_ResumeNeverCarriesTheConversationAcrossTheFence(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
		on             bool
		want           string
	}{
		{"native confidential into an open project", rtFinCWD, rtOpsCWD, true, rtFin},
		{"native confidential into another confidential project", rtFinCWD, homeLegalCWD, true, rtFin},
		{"open into a sealed project", rtOpsCWD, rtSealedCWD, true, rtOps},
		{"open into a confidential project", rtOpsCWD, rtFinCWD, true, rtFin},
		{"native sealed into an open project", rtSealedCWD, rtOpsCWD, true, rtSealed},
		{"off: native confidential into an open project", rtFinCWD, rtOpsCWD, false, rtOps},
		{"off: open into a sealed project", rtOpsCWD, rtSealedCWD, false, rtSealed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := homeNewEnv(t, tc.on)
			amb := e.start(ClientClaudeCode, tc.from)
			out, _ := e.hook(ClientClaudeCode, "session-start", map[string]any{"session_id": rtConv, "cwd": tc.to, "source": "resume"})
			require.Equal(t, tc.want, e.session(amb.ID).ProjectSlug)
			require.Contains(t, additionalContext(t, out), "Seam project: "+tc.want, "briefed from where it works")
		})
	}
}

// With the switch off every hook is P0's, word for word, whatever the rows
// hold -- a move recorded while the switch was on, a grant: captures and
// recall follow the cwd, a resume is briefed from the cwd, a subagent from
// the cwd, and the harvest goes where RecordDestinationOf says with no
// clearance asked.
func TestHome_SwitchOffIsP0(t *testing.T) {
	t.Run("a moved conversation", func(t *testing.T) {
		e := homeNewEnv(t, false)
		amb := e.start(ClientClaudeCode, rtOpsCWD)
		m := homeMove(t, e.db, ClientClaudeCode, amb, rtFin)
		require.Empty(t, clPlan(t, e, rtOpsCWD, "quiet-ledger"))
		require.True(t, clCaptured(t, e, rtOps, "quiet-ledger"), "by cwd")
		require.False(t, clCaptured(t, e, rtFin, "quiet-ledger"))
		require.Contains(t, homeRecall(t, e, ClientClaudeCode, rtOpsCWD, homeOpsPrompt), "ops-deploy-probe-race")
		require.Empty(t, homeRecall(t, e, ClientClaudeCode, rtOpsCWD, homeFinPrompt))
		require.Contains(t, homeSubagentStart(t, e, rtOpsCWD, "ag81", homeOpsPrompt), "Seam project: "+rtOps)
		out, _ := e.hook(ClientClaudeCode, "session-start", map[string]any{"session_id": rtConv, "cwd": rtOpsCWD, "source": "compact"})
		require.Contains(t, additionalContext(t, out), "Seam project: "+rtOps)
		require.Empty(t, e.end())
		require.Equal(t, rtHarvest, e.session(m.Child.ID).Findings, "P0: the conversation's ambient row in a fenced project")
	})

	t.Run("an open home holding a grant", func(t *testing.T) {
		e := homeNewEnv(t, false)
		amb := e.start(ClientClaudeCode, rtOpsCWD)
		approveGrant(t, &captureEnv{db: e.db}, rtOps, rtLegal, amb.ID, "")
		require.Empty(t, clPlan(t, e, rtOpsCWD, "away"))
		require.True(t, clCaptured(t, e, rtOps, "away"))
		require.Empty(t, homeRecall(t, e, ClientClaudeCode, rtOpsCWD, homeLegalPrompt), "no grant joins a cwd's recall")
		require.Empty(t, e.end())
		require.Equal(t, rtHarvest, e.session(amb.ID).Findings)
		require.Empty(t, homeHookEvents(t, e, core.EventInjected, "user-prompt-submit"))
	})
}

// A conversation native to a confidential project whose link -- made with
// session_start before the switch, or by a client that sends no identity --
// holds a read grant from the link's project: RecordDestinationOf keeps its
// record at home, but the grant clears a flow into the link's project only,
// so its home is not cleared for the granted project's content and the end's
// harvest is recorded nowhere.
func TestHome_ANativeHomeIsNotClearedByALinksGrant(t *testing.T) {
	e := homeNewEnv(t, true)
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, e.db, homeDesk, homeDesk)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, e.db, homeDesk, core.IsolationConfidential))
	amb := e.start(ClientClaudeCode, rtFinCWD)
	link := e.bind(ClientClaudeCode, rtLegal, core.SessionActive, "")
	approveGrant(t, &captureEnv{db: e.db}, rtLegal, homeDesk, link.ID, "")
	dest, err := store.RecordDestinationOf(ctx, e.db, ClientClaudeCode.externalIdentity(), rtConv)
	require.NoError(t, err)
	require.Equal(t, store.RecordDestAmbient, dest.Kind, "P0's decision alone: its home")

	held := homeDesk + ", " + rtFin + ", " + rtLegal
	require.Equal(t, "Seamless did not record this conversation's findings: this session holds content from "+held+
		", and no project is cleared for all of it: this would land in project "+rtFin+".", e.end())
	require.Equal(t, "Not recorded: this session holds content from "+held+
		", and its findings may not be written into project "+rtFin+".", e.session(amb.ID).Findings)
	require.Empty(t, e.session(link.ID).Findings, "the link keeps its own (none)")
	require.Equal(t, map[string]map[string]int{"events": {}, "sessions": {}, "fts": {}}, e.whereIs(rtSecret))
}
