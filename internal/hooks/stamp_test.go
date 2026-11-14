package hooks

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// Provenance stamps on what the hooks record for a conversation
// (isolation.requests, plan fenced-sessions P6; stamp.go): a conversation
// homed in a confidential project, holding an approved read of another,
// leaves every capture and harvest in its home stamped with that grant --
// the plan's note and its tracking task, a subagent's report, the SessionEnd
// harvest and Codex Stop findings -- and nothing with the switch off.

// stampGrantFor approves a read of target asked from the conversation's home
// (amb's project), held by the conversation the advisory way (its client and
// session id) or, with process set, by the agent's process tree.
func stampGrantFor(t *testing.T, db *sql.DB, client Client, amb core.Session, target string, process *access.ProcessToken) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	req := store.GrantRequest{Project: target, From: amb.ProjectSlug, Lease: time.Hour, TTL: 10 * time.Minute,
		SessionID: amb.ID, SessionName: amb.Name}
	if process != nil {
		req.ProcessKey, req.PeerPID, req.PeerUID = process.Key(), process.PID, 501
	} else {
		req.ConversationClient, req.ConversationID = client.externalIdentity(), amb.ExternalSessionID
	}
	a, err := store.CreateGrantRequest(ctx, db, req, now)
	require.NoError(t, err)
	_, err = store.GrantAccessApprove(ctx, db, a.ID, a.Challenge, "owner", 0, store.GrantAccessRead, now)
	require.NoError(t, err)
	return a.ID
}

func stampsOfItem(t *testing.T, db *sql.DB, kind, id string) []core.Stamp {
	t.Helper()
	got, err := store.ProvenanceForItems(context.Background(), db, kind, []string{id})
	require.NoError(t, err)
	return got[id]
}

// stampPlan writes a plan file and posts its capture from cwd, then its
// approval when approve is set.
func stampPlan(t *testing.T, e *routeEnv, cwd, basename string, approve bool) {
	t.Helper()
	plan := filepath.Join(e.plansDir, basename+".md")
	require.NoError(t, os.WriteFile(plan, []byte("# Example ledger review\n\n1. "+rtSecret+" step one.\n"), 0o644))
	_, refused := e.hook(ClientClaudeCode, "post-tool-use", map[string]any{
		"session_id": rtConv, "cwd": cwd, "tool_name": "Write", "tool_input": map[string]any{"file_path": plan},
	})
	require.Empty(t, refused)
	if approve {
		_, refused = e.hook(ClientClaudeCode, "post-tool-use", map[string]any{
			"session_id": rtConv, "cwd": cwd, "tool_name": "ExitPlanMode", "tool_response": map[string]any{"filePath": plan},
		})
		require.Empty(t, refused)
	}
}

func TestStampHooks_AdvisoryTierStampsEveryRecord(t *testing.T) {
	for _, on := range []bool{true, false} {
		name := "isolation.requests on"
		if !on {
			name = "off"
		}
		t.Run(name, func(t *testing.T) {
			e := homeNewEnv(t, on)
			ctx := context.Background()
			amb := e.start(ClientClaudeCode, homeLegalCWD)
			require.Equal(t, rtLegal, amb.ProjectSlug)
			approval := stampGrantFor(t, e.db, ClientClaudeCode, amb, rtFin, nil)
			want := []core.Stamp{{Project: rtFin, Via: core.StampViaGrant, Ref: approval}}
			if !on {
				want = nil
			}

			stampPlan(t, e, homeLegalCWD, "quiet-ledger", true)
			idx, ok, err := store.NoteBySlug(ctx, e.db, rtLegal, plans.NotePrefix+"quiet-ledger")
			require.NoError(t, err)
			require.True(t, ok)
			note, err := e.mgr.Store().ReadNote(idx.FilePath)
			require.NoError(t, err)
			require.Equal(t, want, note.MayContain, "the plan's note")
			tasks, err := store.ListTasksForPlan(ctx, e.db, rtLegal, "", "example-ledger-review")
			require.NoError(t, err)
			require.Len(t, tasks, 1)
			require.Equal(t, want, stampsOfItem(t, e.db, core.ItemKindTask, tasks[0].ID), "the plan's tracking task")

			_, refused := e.hook(ClientClaudeCode, "subagent-stop", map[string]any{
				"session_id": rtConv, "cwd": homeLegalCWD, "permission_mode": "plan",
				"transcript_path": placeSubagentTranscript(t, "ag51"), "agent_id": "ag51", "agent_type": "Explore",
			})
			require.Empty(t, refused)
			agentIdx, ok, err := store.NoteBySlug(ctx, e.db, rtLegal, plans.AgentNotePrefix+"ag51")
			require.NoError(t, err)
			require.True(t, ok)
			agent, err := e.mgr.Store().ReadNote(agentIdx.FilePath)
			require.NoError(t, err)
			require.Equal(t, want, agent.MayContain, "the subagent's report")

			require.Empty(t, e.end())
			require.Equal(t, "(auto-harvested) "+rtSecret+" of the review.", e.session(amb.ID).Findings)
			require.Equal(t, want, stampsOfItem(t, e.db, core.ItemKindSession, amb.ID), "the SessionEnd harvest")
			if !on {
				var n int
				require.NoError(t, e.db.QueryRow(`SELECT count(*) FROM item_provenance`).Scan(&n))
				require.Zero(t, n, "off: no index row at all")
			}
		})
	}
}

// Codex has no SessionEnd: every turn's findings land on its row (here, the
// home's ambient row) and carry the conversation's stamps.
func TestStampHooks_CodexStopFindings(t *testing.T) {
	e := homeNewEnv(t, true)
	amb := e.start(ClientCodex, homeLegalCWD)
	approval := stampGrantFor(t, e.db, ClientCodex, amb, rtFin, nil)
	_, refused := e.hook(ClientCodex, "stop", map[string]any{
		"session_id": rtConv, "cwd": homeLegalCWD, "last_assistant_message": rtSecret + " turn one.",
	})
	require.Empty(t, refused)
	require.Equal(t, "(auto-harvested) "+rtSecret+" turn one.", e.session(amb.ID).Findings)
	require.Equal(t, []core.Stamp{{Project: rtFin, Via: core.StampViaGrant, Ref: approval}},
		stampsOfItem(t, e.db, core.ItemKindSession, amb.ID))
}

// A conversation whose label is its home alone records nothing it holds from
// elsewhere: no stamp.
func TestStampHooks_TheHomeAloneStampsNothing(t *testing.T) {
	e := homeNewEnv(t, true)
	amb := e.start(ClientClaudeCode, homeLegalCWD)
	stampPlan(t, e, homeLegalCWD, "home-plan", false)
	require.Empty(t, e.end())
	require.Empty(t, stampsOfItem(t, e.db, core.ItemKindSession, amb.ID))
	var n int
	require.NoError(t, e.db.QueryRow(`SELECT count(*) FROM item_provenance`).Scan(&n))
	require.Zero(t, n)
}

// Under peer_auth the conversation is the agent's process tree: a grant keyed
// to its process stamps the pushed plan, the pushed report and the pushed
// harvest.
func TestStampHooks_PeerTierStampsThePushedRecord(t *testing.T) {
	e := homeNewPushHooks(t, true)
	const ext = "stamp-p1"
	e.send("session-start", "", map[string]any{"session_id": ext, "cwd": homeLegalCWD, "source": "startup"}, nil)
	amb := requireAmbientSession(t, e.db, ClientClaudeCode, ext)
	require.Equal(t, rtLegal, amb.ProjectSlug)
	tok := lkAgent
	approval := stampGrantFor(t, e.db, ClientClaudeCode, amb, homeDesk, &tok)
	want := []core.Stamp{{Project: homeDesk, Via: core.StampViaGrant, Ref: approval}}

	_, refused := homePeerPlan(e, ext, homeLegalCWD, "legal-plan", "# Legal plan\n\n"+rtSecret+".")
	require.Empty(t, refused)
	note, ok := e.note(rtLegal, plans.NotePrefix+"legal-plan")
	require.True(t, ok)
	require.Equal(t, want, note.MayContain)

	_, refused = e.send("subagent-stop", "", map[string]any{"session_id": ext, "cwd": homeLegalCWD,
		"agent_id": "a61", "agent_type": "Explore", "permission_mode": "plan"},
		map[string]any{"version": 1, "prompt": "Read the example memo", "report": rtSecret + " report."})
	require.Empty(t, refused)
	report, ok := e.note(rtLegal, plans.AgentNotePrefix+"a61")
	require.True(t, ok)
	require.Equal(t, want, report.MayContain)

	_, refused = e.send("session-end", "", map[string]any{"session_id": ext},
		map[string]any{"version": 1, "findings": "(auto-harvested) " + rtSecret + " pushed."})
	require.Empty(t, refused)
	require.Equal(t, want, stampsOfItem(t, e.db, core.ItemKindSession, amb.ID))
}
