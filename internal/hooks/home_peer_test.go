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
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// Records follow the home (isolation.requests on, home.go) under peer_auth:
// the conversation is the agent's kernel-verified process tree, and `seam
// hook` pushes what it read (pushHooks: example-finance locked, example-ops
// open, the owner's socket view for the hook under the agent).

const homeDesk = "example-desk" // confidential, no lock

// homeDeskPrompt matches the one memory homeNewPushHooks plants in
// example-desk.
const homeDeskPrompt = "who owns the intake queue triage rotation"

// homeNewPushHooks is newPushHooks with isolation.requests as on says,
// example-legal (confidential, its own repository) and example-desk
// (confidential) beside its projects, and homeSeed's memories plus one in
// example-desk.
func homeNewPushHooks(t *testing.T, on bool) *pushHooks {
	t.Helper()
	e := &pushHooks{plansDir: filepath.Join(t.TempDir(), "daemon-plans")}
	require.NoError(t, os.MkdirAll(e.plansDir, 0o755))
	e.lockHooks = newLockHooksWith(t, true, func(db *sql.DB, cfg *Config) {
		mgr, err := files.NewManager(filepath.Join(t.TempDir(), "data"), db, nil)
		require.NoError(t, err)
		e.mgr = mgr
		cfg.Files = mgr
		cfg.PlanCapture = config.PlanCapture{Enabled: true}
		cfg.PlansDir = e.plansDir
		cfg.ReadGrants = on
	})
	ctx := context.Background()
	require.NoError(t, store.AddRepoMapping(ctx, e.db, homeLegalCWD, rtLegal))
	for _, slug := range []string{rtLegal, homeDesk} {
		_, err := store.EnsureProject(ctx, e.db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, e.db, slug, core.IsolationConfidential))
	}
	homeSeed(t, e.db)
	insertMemory(t, e.db, "01HDSK1", "gotcha", "desk-intake-queue", "intake queue triage rotation schedule", homeDesk)
	return e
}

// homePeerMove approves a move of the tree's conversation whose ambient row is
// amb into target, held by the agent process (peer_auth), with the target's
// lease for lease when the target is locked: the approve core's
// store.MoveApproveWith, bound to the child.
func homePeerMove(t *testing.T, e *pushHooks, amb core.Session, target string, lease time.Duration) store.MoveApproved {
	t.Helper()
	ctx := context.Background()
	carry, err := store.CarryOf(amb.ProjectSlug, []string{target}, []string{target})
	require.NoError(t, err)
	a, err := store.MoveRequestCreate(ctx, e.db, store.MoveRequest{
		Project: target, From: amb.ProjectSlug, Carry: carry, Lease: time.Hour, TTL: 15 * time.Minute,
		PeerUID: 501, PeerPID: lkAgent.PID, ProcessKey: lkAgent.Key(), Program: "/usr/local/bin/claude",
		SessionID: amb.ID, SessionName: amb.Name,
	}, e.clock())
	require.NoError(t, err)
	done, err := store.MoveApproveWith(ctx, e.db,
		store.MoveDecision{ID: a.ID, Challenge: a.Challenge, DecidedBy: "owner", Lease: lease}, e.clock())
	require.NoError(t, err)
	return done
}

// homePeerSession reads a session by id.
func homePeerSession(t *testing.T, e *pushHooks, id string) core.Session {
	t.Helper()
	s, ok, err := store.SessionByID(context.Background(), e.db, id)
	require.NoError(t, err)
	require.True(t, ok, "session %s", id)
	return s
}

// homePeerPlan pushes a plan capture from cwd for the conversation ext.
func homePeerPlan(e *pushHooks, ext, cwd, basename, content string) (hookContext, refused string) {
	return e.send("post-tool-use", "", map[string]any{"session_id": ext, "cwd": cwd, "tool_name": "Write",
		"tool_input": map[string]any{"file_path": "/home/owner/.claude/plans/" + basename + ".md"}},
		map[string]any{"version": 1, "plan": map[string]any{"basename": basename, "content": content}})
}

// homePeerTokens is a pushed token count: its total is the sum of its parts.
func homePeerTokens(input, cached, creation, output int) map[string]any {
	return map[string]any{"input": input, "cached": cached, "cacheCreation": creation, "output": output,
		"total": input + cached + creation + output}
}

// Under peer_auth a move into a locked confidential project carries the
// lease, and the tree's pushed record lands in the project it moved into,
// wherever its hooks fire from: the plan and the subagent's report, filed
// under the child; its prompts recall the new home; the end's findings are the
// child's, and its token totals are counted once -- the session the move ended
// keeps what it had harvested, the child what came after. A compact while the
// lease lives is briefed from the home; once the lease has ended (the end
// revoked it) a resume is answered with the locked notice and a request filed
// for the home -- never the cwd's briefing -- and the child stays where it is.
func TestHomePeer_MovedConversation(t *testing.T) {
	e := homeNewPushHooks(t, true)
	const ext = "home-p1"
	e.send("session-start", "", map[string]any{"session_id": ext, "cwd": "/work/ops", "source": "startup"}, nil)
	// An earlier stretch of the conversation ended and was resumed: the row
	// holds the totals it harvested then.
	_, refused := e.send("session-end", "", map[string]any{"session_id": ext},
		map[string]any{"version": 1, "findings": "(auto-harvested) Opened the repository.", "tokens": homePeerTokens(7, 100, 20, 3)})
	require.Empty(t, refused)
	e.send("session-start", "", map[string]any{"session_id": ext, "cwd": "/work/ops", "source": "resume"}, nil)
	amb := requireAmbientSession(t, e.db, ClientClaudeCode, ext)
	require.Equal(t, lkOps, amb.ProjectSlug)
	require.Equal(t, 130, amb.Tokens.Total)
	m := homePeerMove(t, e, amb, lkFin, time.Hour)
	child := m.Child

	ctxText, refused := homePeerPlan(e, ext, "/work/ops", "fin-plan", "# Fin plan\n\n"+rtSecret+".")
	require.Empty(t, refused)
	require.Contains(t, ctxText, "composition plan:fin-plan")
	_, ok := e.note(lkFin, plans.NotePrefix+"fin-plan")
	require.True(t, ok, "the plan lands in the project the conversation moved into")
	_, ok = e.note(lkOps, plans.NotePrefix+"fin-plan")
	require.False(t, ok)

	_, refused = e.send("subagent-stop", "", map[string]any{"session_id": ext, "cwd": "/work/ops",
		"agent_id": "a51", "agent_type": "Explore", "permission_mode": "plan"},
		map[string]any{"version": 1, "prompt": "Read the example ledger", "report": rtSecret + " report."})
	require.Empty(t, refused)
	_, ok = e.note(lkFin, plans.AgentNotePrefix+"a51")
	require.True(t, ok)

	ctxText, _ = e.send("user-prompt-submit", "", map[string]any{"session_id": ext, "cwd": "/work/ops", "user_prompt": homeFinPrompt}, nil)
	require.Contains(t, ctxText, "fin-ledger-close")
	ctxText, _ = e.send("user-prompt-submit", "", map[string]any{"session_id": ext, "cwd": "/work/ops", "user_prompt": homeOpsPrompt}, nil)
	require.Empty(t, ctxText)

	ctxText, _ = e.send("session-start", "", map[string]any{"session_id": ext, "cwd": "/work/ops", "source": "compact"}, nil)
	require.Contains(t, ctxText, "Seam project: "+lkFin, "briefed from its home while the lease lives")
	require.Contains(t, ctxText, "fin-secret-rule")
	require.NotContains(t, ctxText, "ops-rule")

	_, refused = e.send("session-end", "", map[string]any{"session_id": ext},
		map[string]any{"version": 1, "findings": "(auto-harvested) " + rtSecret + " pushed.", "tokens": homePeerTokens(17, 150, 20, 13)})
	require.Empty(t, refused)
	got := homePeerSession(t, e, child.ID)
	require.Equal(t, core.SessionCompleted, got.Status)
	require.Equal(t, "(auto-harvested) "+rtSecret+" pushed.", got.Findings)
	require.Equal(t, core.TokenUsage{Input: 10, Cached: 50, Output: 10, Total: 70}, got.Tokens, "what came after the move")
	left := homePeerSession(t, e, m.Left.ID)
	require.Equal(t, 130, left.Tokens.Total, "the session the move ended keeps what it harvested")
	require.Equal(t, "(auto-harvested) Opened the repository.\n\n"+store.MovePointer(lkFin, child.Name), left.Findings)

	// The end revoked the lease the move bound to the child.
	before := len(e.pending())
	ctxText, _ = e.send("session-start", "", map[string]any{"session_id": ext, "cwd": "/work/ops", "source": "resume"}, nil)
	require.Contains(t, ctxText, "Seamless: project "+lkFin+" is locked")
	require.Contains(t, ctxText, "This session is recorded in "+lkFin)
	require.NotContains(t, ctxText, "ops-rule", "never the cwd's briefing")
	require.NotContains(t, ctxText, "fin-secret-rule", "nor the locked home's")
	got = homePeerSession(t, e, child.ID)
	require.Equal(t, lkFin, got.ProjectSlug)
	require.Equal(t, core.SessionActive, got.Status)
	pending := e.pending()
	require.Len(t, pending, before+1)
	require.Equal(t, lkFin, pending[0].Project)
	require.Equal(t, child.ID, pending[0].SessionID)

	rows, err := e.db.QueryContext(context.Background(),
		`SELECT DISTINCT project_slug FROM events WHERE payload LIKE ?`, "%"+rtSecret+"%")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		require.Equal(t, lkFin, p)
	}
	require.NoError(t, rows.Err())
}

// Under peer_auth /clear starts a new conversation in the same process tree,
// and the label stays with the tree: after a move into a confidential project
// the new conversation's home is its cwd's open project, which is not cleared
// for what the tree holds, so its captures and its harvest are refused with
// the notice and nothing of them lands in the open project.
func TestHomePeer_ClearKeepsTheTreesLabel(t *testing.T) {
	e := homeNewPushHooks(t, true)
	e.send("session-start", "", map[string]any{"session_id": "home-p2", "cwd": "/work/ops", "source": "startup"}, nil)
	homePeerMove(t, e, requireAmbientSession(t, e.db, ClientClaudeCode, "home-p2"), lkFin, time.Hour)
	_, refused := e.send("session-end", "", map[string]any{"session_id": "home-p2", "reason": "clear"},
		map[string]any{"version": 1, "findings": "(auto-harvested) Cleared."})
	require.Empty(t, refused)

	e.send("session-start", "", map[string]any{"session_id": "home-p3", "cwd": "/work/ops", "source": "clear"}, nil)
	fresh := requireAmbientSession(t, e.db, ClientClaudeCode, "home-p3")
	require.Equal(t, lkOps, fresh.ProjectSlug)
	notice := func(what string) string {
		return "Seamless did not record " + what + ": this agent holds content from " + lkFin +
			", so it writes only into " + lkFin + ", and this would land in project " + lkOps + "."
	}
	_, refused = homePeerPlan(e, "home-p3", "/work/ops", "after-clear", "# After clear\n\n"+rtSecret+".")
	require.Equal(t, notice("this plan"), refused)
	for _, p := range []string{lkOps, lkFin} {
		_, ok := e.note(p, plans.NotePrefix+"after-clear")
		require.False(t, ok, p)
	}
	_, refused = e.send("session-end", "", map[string]any{"session_id": "home-p3"},
		map[string]any{"version": 1, "findings": "(auto-harvested) " + rtSecret + "."})
	require.Equal(t, notice("this conversation's findings"), refused)
	require.Equal(t, "Not recorded: this agent holds content from "+lkFin+
		", and its findings may not be written into project "+lkOps+".",
		requireAmbientSession(t, e.db, ClientClaudeCode, "home-p3").Findings)
}

// Under peer_auth a conversation native to a confidential project holding a
// read grant of another (held by its process tree) keeps its record at home,
// wherever its hooks fire from: its pushed plan and its harvest land in its
// home; its prompts recall the home and the granted project; a subagent's
// briefing is the home's with the granted project's matches; a compact from
// an open project's repository stays home.
func TestHomePeer_GrantHolderInAConfidentialHome(t *testing.T) {
	e := homeNewPushHooks(t, true)
	const ext = "home-g1"
	e.send("session-start", "", map[string]any{"session_id": ext, "cwd": homeLegalCWD, "source": "startup"}, nil)
	amb := requireAmbientSession(t, e.db, ClientClaudeCode, ext)
	require.Equal(t, rtLegal, amb.ProjectSlug)
	approveGrant(t, &captureEnv{db: e.db}, rtLegal, homeDesk, amb.ID, lkAgent.Key())

	ctxText, _ := e.send("user-prompt-submit", "", map[string]any{"session_id": ext, "cwd": "/work/ops", "user_prompt": homeDeskPrompt}, nil)
	require.Contains(t, ctxText, "desk-intake-queue", "the granted project joins the recall")
	ctxText, _ = e.send("user-prompt-submit", "", map[string]any{"session_id": ext, "cwd": "/work/ops", "user_prompt": homeOpsPrompt}, nil)
	require.Empty(t, ctxText)

	_, refused := homePeerPlan(e, ext, "/work/ops", "legal-plan", "# Legal plan\n\n"+rtSecret+".")
	require.Empty(t, refused)
	_, ok := e.note(rtLegal, plans.NotePrefix+"legal-plan")
	require.True(t, ok)
	for _, p := range []string{lkOps, homeDesk} {
		_, ok = e.note(p, plans.NotePrefix+"legal-plan")
		require.False(t, ok, p)
	}

	ctxText, _ = e.send("subagent-start", "", map[string]any{"session_id": ext, "cwd": "/work/ops",
		"agent_id": "a61", "agent_type": "Explore"}, map[string]any{"version": 1, "prompt": homeDeskPrompt})
	require.Contains(t, ctxText, "Seam project: "+rtLegal)
	require.Contains(t, ctxText, "legal-privilege-rule")
	require.Contains(t, ctxText, "desk-intake-queue")
	require.NotContains(t, ctxText, "ops-rule")

	ctxText, _ = e.send("session-start", "", map[string]any{"session_id": ext, "cwd": "/work/ops", "source": "compact"}, nil)
	require.Contains(t, ctxText, "Seam project: "+rtLegal)
	require.Equal(t, rtLegal, requireAmbientSession(t, e.db, ClientClaudeCode, ext).ProjectSlug)

	_, refused = e.send("session-end", "", map[string]any{"session_id": ext},
		map[string]any{"version": 1, "findings": "(auto-harvested) " + rtSecret + " pushed."})
	require.Empty(t, refused)
	require.Equal(t, "(auto-harvested) "+rtSecret+" pushed.", requireAmbientSession(t, e.db, ClientClaudeCode, ext).Findings)
}

// Under peer_auth an explicit session the conversation linked with
// session_start holds a lease on a locked project. Nothing stamps an explicit
// session with an agent process, so it read as another tree's and blanked
// every hook of its conversation: the end completed nothing. With the switch
// on it is the tree's own when the conversation's ambient row is -- the end
// completes both and the harvest goes to the link (P0's routing) -- and a
// hook from another tree naming the conversation still ends nothing. Off it
// is as before.
func TestHomePeer_ALinkHoldingALeaseIsTheTreesOwn(t *testing.T) {
	for _, on := range []bool{true, false} {
		e := homeNewPushHooks(t, on)
		const ext = "home-l1"
		e.send("session-start", "", map[string]any{"session_id": ext, "cwd": "/work/ops", "source": "startup"}, nil)
		amb := requireAmbientSession(t, e.db, ClientClaudeCode, ext)
		link := pushLink(t, e, ext, lkFin, core.SessionActive)
		e.grant(link.ID, lkFin, time.Hour)

		stranger := access.ProcessToken{PID: 300, Version: 3000, Boot: lkBoot}
		e.fromChain(lkHookProc, stranger)
		e.send("session-end", "", map[string]any{"session_id": ext},
			map[string]any{"version": 1, "findings": "(auto-harvested) " + rtSecret + "."})
		require.Equal(t, core.SessionActive, homePeerSession(t, e, amb.ID).Status, "another tree ends nothing")
		require.Equal(t, core.SessionActive, homePeerSession(t, e, link.ID).Status)

		e.fromChain(lkHookProc, lkAgent)
		_, refused := e.send("session-end", "", map[string]any{"session_id": ext},
			map[string]any{"version": 1, "findings": "(auto-harvested) " + rtSecret + "."})
		require.Empty(t, refused)
		if !on {
			require.Equal(t, core.SessionActive, homePeerSession(t, e, amb.ID).Status, "off: blanked, as before")
			continue
		}
		got := homePeerSession(t, e, link.ID)
		require.Equal(t, core.SessionCompleted, got.Status)
		require.Equal(t, "(auto-harvested) "+rtSecret+".", got.Findings)
		require.Equal(t, core.SessionCompleted, homePeerSession(t, e, amb.ID).Status)
		require.Equal(t, "Continued in project "+lkFin+"; this conversation's findings go there, not here.",
			homePeerSession(t, e, amb.ID).Findings)
	}
}

// Under peer_auth a conversation that moved into a locked project and whose
// lease has expired keeps to its home, metadata only: a compact from the open
// project's repository is answered with the locked notice and a request filed
// for the child; a pushed plan is refused with the lock's notice; a prompt
// recalls nothing -- neither the locked home nor the cwd's project; and the end
// completes the child with nothing harvested.
func TestHomePeer_LockedHomeWhoseLeaseExpired(t *testing.T) {
	e := homeNewPushHooks(t, true)
	const ext = "home-p4"
	e.send("session-start", "", map[string]any{"session_id": ext, "cwd": "/work/ops", "source": "startup"}, nil)
	m := homePeerMove(t, e, requireAmbientSession(t, e.db, ClientClaudeCode, ext), lkFin, time.Hour)
	e.set(e.clock().Add(2 * time.Hour))

	before := len(e.pending())
	ctxText, _ := e.send("session-start", "", map[string]any{"session_id": ext, "cwd": "/work/ops", "source": "compact"}, nil)
	require.Contains(t, ctxText, "Seamless: project "+lkFin+" is locked")
	require.NotContains(t, ctxText, "ops-rule")
	require.Len(t, e.pending(), before+1)
	require.Equal(t, m.Child.ID, e.pending()[0].SessionID)
	require.Equal(t, lkFin, homePeerSession(t, e, m.Child.ID).ProjectSlug)

	locked := func(what string) string {
		return "Seamless did not record " + what + ": project " + lkFin + " is locked and this session holds no " +
			"approval; the next capture after the owner approves carries the current state " +
			"(to ask: `seam access request " + lkFin + "`)."
	}
	_, refused := homePeerPlan(e, ext, "/work/ops", "fin-plan", "# Fin plan\n\n"+rtSecret+".")
	require.Equal(t, locked("this plan"), refused)
	for _, p := range []string{lkOps, lkFin} {
		_, ok := e.note(p, plans.NotePrefix+"fin-plan")
		require.False(t, ok, p)
	}
	for _, prompt := range []string{homeFinPrompt, homeOpsPrompt} {
		ctxText, _ = e.send("user-prompt-submit", "", map[string]any{"session_id": ext, "cwd": "/work/ops", "user_prompt": prompt}, nil)
		require.Empty(t, ctxText, prompt)
	}

	_, refused = e.send("session-end", "", map[string]any{"session_id": ext},
		map[string]any{"version": 1, "findings": "(auto-harvested) " + rtSecret + "."})
	require.Equal(t, locked("this session's findings and token totals"), refused)
	got := homePeerSession(t, e, m.Child.ID)
	require.Equal(t, core.SessionCompleted, got.Status)
	require.Empty(t, got.Findings)
}
