package hooks

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// Restricted local accounts (peer_auth.restricted) at the hooks: a session
// only in an open project the account reaches, a notice that names no project
// everywhere else, never a session another principal started, and the
// captures `seam hook` pushes landing in its own sessions, labeled.

const (
	rAcct  = "_example_agent"
	rUID   = 502
	rVault = "example-vault" // isolation sealed
	rLegal = "example-legal" // open, but locked by peer_auth
)

var (
	rHookProc = access.ProcessToken{PID: 700, Version: 7000, Boot: lkBoot}
	rAgent    = access.ProcessToken{PID: 710, Version: 7100, Boot: lkBoot}
)

// restrictedHooks is pushHooks (example-ops open, example-finance
// confidential and locked; the files layer and plan capture on, with the
// approval's tracking task) with two more closed projects -- example-vault
// sealed, example-legal open but locked -- and a policy that names
// _example_agent a restricted account. Requests carry its socket View, its
// reach filled the way the transport fills it, until asOwner.
type restrictedHooks struct {
	*pushHooks
}

func newRestrictedHooks(t *testing.T) *restrictedHooks {
	t.Helper()
	push := &pushHooks{plansDir: filepath.Join(t.TempDir(), "daemon-plans")}
	require.NoError(t, os.MkdirAll(push.plansDir, 0o755))
	push.lockHooks = newLockHooksWith(t, true, func(db *sql.DB, cfg *Config) {
		mgr, err := files.NewManager(filepath.Join(t.TempDir(), "data"), db, nil)
		require.NoError(t, err)
		push.mgr = mgr
		cfg.Files = mgr
		cfg.PlanCapture = config.PlanCapture{Enabled: true, AutoTask: true}
		cfg.PlansDir = push.plansDir
	})
	e := &restrictedHooks{pushHooks: push}
	ctx := context.Background()
	for slug, repo := range map[string]string{rVault: "/work/vault", rLegal: "/work/legal"} {
		_, err := store.EnsureProject(ctx, e.db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.AddRepoMapping(ctx, e.db, repo, slug))
	}
	require.NoError(t, store.SetProjectIsolation(ctx, e.db, rVault, core.IsolationSealed))
	insertMemory(t, e.db, "01V", "constraint", "vault-secret-rule", "the vault team's secret rule", rVault)
	insertMemory(t, e.db, "01L", "constraint", "legal-secret-rule", "the legal team's secret rule", rLegal)
	policy, err := access.New(config.PeerAuth{Owner: "owner", Locked: []string{lkFin, rLegal}, Restricted: []string{rAcct}},
		func(n string) (access.Account, error) {
			switch n {
			case "owner":
				return access.Account{UID: 501, Name: "owner"}, nil
			case rAcct:
				return access.Account{UID: rUID, Name: rAcct}, nil
			}
			return access.Account{}, fmt.Errorf("no account %s", n)
		})
	require.NoError(t, err)
	e.mu.Lock()
	e.policy = policy
	e.mu.Unlock()
	e.asRestricted()
	return e
}

// restrictedView is the restricted account's socket view, its reach loaded
// from the projects table as peerHandler loads it.
func (e *restrictedHooks) restrictedView() access.View {
	v, ok := e.policy.PeerChainView(rUID, []access.ProcessToken{rHookProc, rAgent})
	require.True(e.t, ok)
	open, err := store.OpenProjectSlugs(context.Background(), e.db)
	require.NoError(e.t, err)
	return v.WithOpenProjects(open)
}

// rExt is the restricted account's agent session id, or session name, as the
// hooks store it: inside the account's namespace (SEAL A2).
func rExt(id string) string { return access.QualifiedSessionName(rAcct, id) }

// asRestricted serves every later request as the restricted account.
func (e *restrictedHooks) asRestricted() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.view = func() (access.View, bool) { return e.restrictedView(), true }
}

// asOwner serves every later request as the owner's socket connection.
func (e *restrictedHooks) asOwner() { e.fromChain(lkHookProc, lkAgent) }

// sendAs is send for the agent process proc.
func (e *restrictedHooks) sendAs(proc access.ProcessToken, endpoint, client string, body, capture map[string]any) (hookContext, refused string) {
	e.t.Helper()
	q := url.Values{"host": {localHostName}, "agent_process": {proc.AgentID()}}
	if client != "" {
		q.Set("client", client)
	}
	if cwd, _ := body["cwd"].(string); cwd != "" && endpoint == "session-start" {
		q.Set("repo_root", cwd)
		q.Set("main_root", cwd)
	}
	if capture != nil {
		withPush := map[string]any{harvest.Field: capture}
		for k, v := range body {
			withPush[k] = v
		}
		body = withPush
	}
	// The socket needs no key; with peer_auth off (no View) the bearer key is
	// the only way in.
	e.mu.Lock()
	key := ""
	if e.view == nil {
		key = testKey
	}
	e.mu.Unlock()
	resp, out := post(e.t, e.url+"/api/hooks/"+endpoint+"?"+q.Encode(), key, body)
	require.Equal(e.t, http.StatusOK, resp.StatusCode)
	if hso, ok := out["hookSpecificOutput"].(map[string]any); ok {
		hookContext, _ = hso["additionalContext"].(string)
	}
	return hookContext, resp.Header.Get(harvest.RefusedHeader)
}

func (e *restrictedHooks) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	require.NoError(e.t, e.db.QueryRowContext(context.Background(), query, args...).Scan(&n))
	return n
}

// agentProcess is the agent process a session is stamped with.
func (e *restrictedHooks) agentProcess(id string) string {
	e.t.Helper()
	got, err := store.SessionAgentProcesses(context.Background(), e.db, []string{id})
	require.NoError(e.t, err)
	return got[id].AgentProcess
}

// age sets a session's updated_at far back, so any heartbeat, model stamp,
// reactivation or completion moves it.
func (e *restrictedHooks) age(id string) {
	e.t.Helper()
	_, err := e.db.ExecContext(context.Background(), `UPDATE sessions SET updated_at = ? WHERE id = ?`,
		core.FormatTime(e.clock().AddDate(-1, 0, 0)), id)
	require.NoError(e.t, err)
}

// Anywhere but an open project it reaches -- a confidential, sealed or locked
// project's repository, a directory nothing maps, no cwd at all -- a
// restricted account's SessionStart records no session, grows no map, mints
// no project, and answers one notice that names no project: the same text in
// a closed project's repository as in an unmapped directory.
func TestRestrictedSessionStart_OutsideItsReachRecordsNothing(t *testing.T) {
	e := newRestrictedHooks(t)
	mapRows := e.count(`SELECT count(*) FROM repo_map`)
	projects := e.count(`SELECT count(*) FROM projects`)

	var notices []string
	for i, c := range []struct{ cwd, client string }{
		{"/work/finance", ""},                // confidential and locked
		{"/work/vault", ""},                  // sealed
		{"/work/legal", ""},                  // open, but locked
		{"/work/elsewhere", ""},              // unmapped: the global scope
		{"/work/finance/sub/dir", ""},        // inside a closed repository
		{"", ""},                             // no cwd
		{"/work/vault", string(ClientCodex)}, // another client
		{"/work/elsewhere", string(ClientCodex)},
	} {
		id := fmt.Sprintf("r-out-%d", i)
		notice, refused := e.sendAs(rAgent, "session-start", c.client, map[string]any{"session_id": id, "cwd": c.cwd, "source": "startup"}, nil)
		require.Empty(t, refused)
		notices = append(notices, notice)
		client := ClientClaudeCode
		if c.client != "" {
			client = ClientCodex
		}
		_, found, err := store.AmbientSessionByExternalIdentity(context.Background(), e.db, client.externalIdentity(), id)
		require.NoError(t, err)
		require.False(t, found, "cwd %q: no session is recorded", c.cwd)
	}
	for i, n := range notices {
		require.Equal(t, restrictedNotice(rAcct), n, "notice %d is the one text", i)
	}
	notice := notices[0]
	require.Contains(t, notice, "maps to no project account "+rAcct+" may reach")
	require.Contains(t, notice, "this session is not recorded")
	require.Contains(t, notice, "refuses the writes")
	require.Contains(t, notice, "`seamlessd map-repo --path <repo> --project <slug>`")
	for _, never := range []string{lkFin, rVault, rLegal, "fin-secret-rule", "vault-secret-rule", "legal-secret-rule",
		"confidential", "sealed", "locked", "Seam session:"} {
		require.NotContains(t, notice, never)
	}

	require.Zero(t, e.count(`SELECT count(*) FROM sessions`), "no session row anywhere")
	require.Equal(t, mapRows, e.count(`SELECT count(*) FROM repo_map`), "the repo map is read, never grown")
	require.Equal(t, projects, e.count(`SELECT count(*) FROM projects`), "no project minted")
	require.Zero(t, e.count(`SELECT count(*) FROM access_approvals`), "a restricted account files no access request")
	require.Zero(t, e.count(`SELECT count(*) FROM events WHERE kind = ?`, string(core.EventSessionStarted)))

	// In the open project it reaches: the project's briefing and its own
	// session, labeled with its account, stamped with its agent process.
	briefing, _ := e.sendAs(rAgent, "session-start", "", map[string]any{"session_id": "r-ops", "cwd": "/work/ops", "source": "startup"}, nil)
	require.Contains(t, briefing, "ops-rule")
	require.Contains(t, briefing, "Seam session: "+rAcct+":cc/r-ops-", "its session is named inside its namespace")
	require.NotContains(t, briefing, "maps to no project")
	sess := requireAmbientSession(t, e.db, ClientClaudeCode, rExt("r-ops"))
	require.Equal(t, lkOps, sess.ProjectSlug)
	require.Equal(t, rAcct, sess.Account)
	require.Equal(t, rAgent.AgentID(), e.agentProcess(sess.ID))
	require.Equal(t, mapRows, e.count(`SELECT count(*) FROM repo_map`))

	// A resume from a closed repository does not move it there: the notice,
	// and the session stays where it was.
	e.age(sess.ID)
	before := requireAmbientSession(t, e.db, ClientClaudeCode, rExt("r-ops"))
	notice, _ = e.sendAs(rAgent, "session-start", "", map[string]any{"session_id": "r-ops", "cwd": "/work/finance", "source": "resume"}, nil)
	require.Equal(t, restrictedNotice(rAcct), notice)
	require.Equal(t, before, requireAmbientSession(t, e.db, ClientClaudeCode, rExt("r-ops")))
}

// A session belongs to the principal that started it, and a restricted
// account's agent session ids live in a namespace of its own (SEAL A2): its
// hooks naming the id of the owner's session work in the account's OWN session
// under that id -- they never resume, reactivate, heartbeat, stamp, capture
// into or complete the owner's, though both work in the same open project --
// and the owner's hooks never touch a restricted account's.
func TestRestrictedHooks_NeverActInAnotherPrincipalsSession(t *testing.T) {
	e := newRestrictedHooks(t)

	// The owner's session, idle since long ago.
	e.asOwner()
	e.sendAs(lkAgent, "session-start", "", map[string]any{"session_id": "shared-1", "cwd": "/work/ops", "source": "startup"}, nil)
	owner := requireAmbientSession(t, e.db, ClientClaudeCode, "shared-1")
	require.Empty(t, owner.Account)
	e.age(owner.ID)
	owner = requireAmbientSession(t, e.db, ClientClaudeCode, "shared-1")

	// The restricted account's hooks name its id: they get a session of the
	// account's own, exactly as a free id would.
	e.asRestricted()
	out, _ := e.sendAs(rAgent, "session-start", "", map[string]any{"session_id": "shared-1", "cwd": "/work/ops", "source": "resume"},
		map[string]any{"version": 1, "model": "claude-restricted"})
	require.Contains(t, out, "Seam session: "+rAcct+":cc/shared-1-", "the account's own session, under the same id")
	require.Contains(t, out, "ops-rule", "the project's briefing comes: the account reaches the project")
	e.sendAs(rAgent, "user-prompt-submit", "", map[string]any{"session_id": "shared-1", "cwd": "/work/ops",
		"prompt": "ops deploy pipeline health check"}, map[string]any{"version": 1, "model": "claude-restricted"})
	_, refused := e.sendAs(rAgent, "post-tool-use", "", map[string]any{"session_id": "shared-1", "cwd": "/work/ops",
		"tool_name": "Write", "tool_input": map[string]any{"file_path": "/home/agent/.claude/plans/example-plan.md"}},
		planPush("# Example plan\n\nTHE ACCOUNT'S OWN"))
	require.Empty(t, refused)
	e.sendAs(rAgent, "session-end", "", map[string]any{"session_id": "shared-1"},
		map[string]any{"version": 1, "findings": "(auto-harvested) THE ACCOUNT'S OWN", "tokens": pushTokens, "model": "claude-restricted"})

	require.Equal(t, owner, requireAmbientSession(t, e.db, ClientClaudeCode, "shared-1"),
		"the owner's session is exactly as it was: not resumed, touched, re-modeled, harvested or completed")
	require.Equal(t, lkAgent.AgentID(), e.agentProcess(owner.ID), "never stamped with the restricted agent")
	theirs := requireAmbientSession(t, e.db, ClientClaudeCode, rExt("shared-1"))
	require.NotEqual(t, owner.ID, theirs.ID)
	require.Equal(t, rAcct+":"+owner.Name, theirs.Name, "the same handle, inside the account's namespace")
	require.Equal(t, rAcct, theirs.Account)
	require.Equal(t, core.SessionCompleted, theirs.Status)
	require.Equal(t, "(auto-harvested) THE ACCOUNT'S OWN", theirs.Findings)
	require.Equal(t, "claude-restricted", theirs.Model)
	require.Equal(t, rAgent.AgentID(), e.agentProcess(theirs.ID))
	n, found := e.note(lkOps, plans.NotePrefix+"example-plan")
	require.True(t, found, "the capture went through the account's own session")
	require.Equal(t, rAcct, n.Account)

	// Ended, the owner's stays ended: the restricted hook resumes its own.
	e.asOwner()
	e.sendAs(lkAgent, "session-end", "", map[string]any{"session_id": "shared-1"}, nil)
	ended := requireAmbientSession(t, e.db, ClientClaudeCode, "shared-1")
	require.Equal(t, core.SessionCompleted, ended.Status)
	e.asRestricted()
	e.sendAs(rAgent, "session-start", "", map[string]any{"session_id": "shared-1", "cwd": "/work/ops", "source": "resume"}, nil)
	require.Equal(t, ended, requireAmbientSession(t, e.db, ClientClaudeCode, "shared-1"))
	require.Equal(t, lkAgent.AgentID(), e.agentProcess(owner.ID))
	require.Equal(t, core.SessionActive, requireAmbientSession(t, e.db, ClientClaudeCode, rExt("shared-1")).Status)

	// And the other way round: the restricted account's session, which the
	// owner's hooks cannot touch.
	e.sendAs(rAgent, "session-start", "", map[string]any{"session_id": "mine-1", "cwd": "/work/ops", "source": "startup"}, nil)
	mine := requireAmbientSession(t, e.db, ClientClaudeCode, rExt("mine-1"))
	require.Equal(t, rAcct, mine.Account)
	e.age(mine.ID)
	mine = requireAmbientSession(t, e.db, ClientClaudeCode, rExt("mine-1"))
	e.asOwner()
	// Naming it by its qualified id -- the one way the owner can -- reaches
	// nothing.
	out, _ = e.sendAs(lkAgent, "session-start", "", map[string]any{"session_id": rExt("mine-1"), "cwd": "/work/ops", "source": "resume"},
		map[string]any{"version": 1, "model": "claude-owner"})
	require.NotContains(t, out, "Seam session:")
	e.sendAs(lkAgent, "user-prompt-submit", "", map[string]any{"session_id": rExt("mine-1"), "cwd": "/work/ops", "prompt": "ops deploy race"}, nil)
	e.sendAs(lkAgent, "session-end", "", map[string]any{"session_id": rExt("mine-1")},
		map[string]any{"version": 1, "findings": "(auto-harvested) OWNER", "model": "claude-owner"})
	// Naming the bare id is the owner's own id: a session of its own.
	out, _ = e.sendAs(lkAgent, "session-start", "", map[string]any{"session_id": "mine-1", "cwd": "/work/ops", "source": "startup"}, nil)
	require.Contains(t, out, "Seam session: cc/mine-1-")
	ownersMine := requireAmbientSession(t, e.db, ClientClaudeCode, "mine-1")
	require.Empty(t, ownersMine.Account)
	require.NotEqual(t, mine.ID, ownersMine.ID)
	e.sendAs(lkAgent, "session-end", "", map[string]any{"session_id": "mine-1"},
		map[string]any{"version": 1, "findings": "(auto-harvested) OWNER", "model": "claude-owner"})
	require.Equal(t, mine, requireAmbientSession(t, e.db, ClientClaudeCode, rExt("mine-1")))
	require.Equal(t, rAgent.AgentID(), e.agentProcess(mine.ID))

	// Its own hooks still end it.
	e.asRestricted()
	e.sendAs(rAgent, "session-end", "", map[string]any{"session_id": "mine-1"},
		map[string]any{"version": 1, "findings": "(auto-harvested) DONE"})
	done := requireAmbientSession(t, e.db, ClientClaudeCode, rExt("mine-1"))
	require.Equal(t, core.SessionCompleted, done.Status)
	require.Equal(t, "(auto-harvested) DONE", done.Findings)
	require.Equal(t, rAcct, done.Account)
}

// The resume itself checks the session's principal, whatever the reach check
// before it said (a race, or a caller that skipped it): ensureAmbientSessionIn
// neither reactivates, re-scopes nor stamps another principal's session.
func TestEnsureAmbientSession_ResumesOnlyItsOwn(t *testing.T) {
	e := newRestrictedHooks(t)
	h := NewHandler(Config{DB: e.db})
	ctx := context.Background()
	rctx := access.WithView(ctx, e.restrictedView())
	octx := access.WithView(ctx, e.policy.OwnerView())

	e.asOwner()
	e.sendAs(lkAgent, "session-start", "", map[string]any{"session_id": "o-1", "cwd": "/work/ops", "source": "startup"}, nil)
	e.sendAs(lkAgent, "session-end", "", map[string]any{"session_id": "o-1"}, nil)
	ownerRow := requireAmbientSession(t, e.db, ClientClaudeCode, "o-1")
	e.asRestricted()
	e.sendAs(rAgent, "session-start", "", map[string]any{"session_id": "r-1", "cwd": "/work/ops", "source": "startup"}, nil)
	e.sendAs(rAgent, "session-end", "", map[string]any{"session_id": "r-1"}, nil)
	restrictedRow := requireAmbientSession(t, e.db, ClientClaudeCode, rExt("r-1"))

	for _, tc := range []struct {
		name string
		ctx  context.Context
		id   string
		row  core.Session
	}{
		// The raw id, as a caller that skipped namespacedExternalID would
		// pass it: the resume's own check still refuses.
		{"a restricted account, the owner's session", rctx, "o-1", ownerRow},
		{"the owner, a restricted account's session", octx, rExt("r-1"), restrictedRow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, name := h.ensureAmbientSessionIn(tc.ctx, ClientClaudeCode, localHostName, hookPayload{
				SessionID: tc.id, CWD: "/work/ops", Source: "resume", Identity: hookIdentity{AgentProcess: "999.9"},
			}, "")
			require.Empty(t, id)
			require.Empty(t, name)
			require.Equal(t, tc.row, requireAmbientSession(t, e.db, ClientClaudeCode, tc.id))
			require.NotEqual(t, "999.9", e.agentProcess(tc.row.ID))
		})
	}

	// Its own resumes as always.
	id, name := h.ensureAmbientSessionIn(rctx, ClientClaudeCode, localHostName, hookPayload{
		SessionID: rExt("r-1"), CWD: "/work/ops", Source: "resume", Identity: hookIdentity{AgentProcess: rAgent.AgentID()},
	}, "")
	require.Equal(t, restrictedRow.ID, id)
	require.Equal(t, restrictedRow.Name, name)
	require.Equal(t, core.SessionActive, requireAmbientSession(t, e.db, ClientClaudeCode, rExt("r-1")).Status)

	// The session-taint target is resolved the same way: by name too.
	_, found, err := h.taintTarget(rctx, ClientClaudeCode, "", ownerRow.Name)
	require.NoError(t, err)
	require.False(t, found, "the owner's session is not found from a restricted account")
	_, found, err = h.taintTarget(octx, ClientClaudeCode, "", restrictedRow.ID)
	require.NoError(t, err)
	require.False(t, found, "nor a restricted account's from the owner")
	got, found, err := h.taintTarget(rctx, ClientClaudeCode, "", restrictedRow.Name)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, restrictedRow.ID, got.ID)
}

// The captures `seam hook` pushes over the socket land in the restricted
// account's own session, and every note the hooks write for it carries its
// account (frontmatter and index); the owner's capture over the same content
// clears the label, and a capture whose cwd is a closed project's repository
// is dropped without a word.
func TestRestrictedCaptures_LandInItsOwnSessionLabeled(t *testing.T) {
	e := newRestrictedHooks(t)
	planFile := "/home/agent/.claude/plans/example-plan.md"

	e.sendAs(rAgent, "session-start", "", map[string]any{"session_id": "r-cap", "cwd": "/work/ops", "source": "startup"},
		map[string]any{"version": 1, "model": "claude-pushed"})
	sess := requireAmbientSession(t, e.db, ClientClaudeCode, rExt("r-cap"))
	require.Equal(t, "claude-pushed", sess.Model)
	require.Equal(t, rAcct, sess.Account)

	ctxText, refused := e.sendAs(rAgent, "post-tool-use", "", map[string]any{"session_id": "r-cap", "cwd": "/work/ops",
		"tool_name": "Write", "tool_input": map[string]any{"file_path": planFile}}, planPush("# Example plan\n\nStep one."))
	require.Empty(t, refused)
	require.Contains(t, ctxText, "composition plan:example-plan")
	n, ok := e.note(lkOps, plans.NotePrefix+"example-plan")
	require.True(t, ok)
	require.Equal(t, rAcct, n.Account)
	require.Contains(t, n.Body, "git "+pushHead[:12])
	require.Equal(t, "claude-pushed", n.Model)
	e.requireNoteAccount(lkOps, plans.NotePrefix+"example-plan", rAcct)

	// The approval with no readable text flips the status and keeps the label.
	e.sendAs(rAgent, "post-tool-use", "", map[string]any{"session_id": "r-cap", "cwd": "/work/ops",
		"tool_name": "ExitPlanMode", "tool_response": map[string]any{}}, map[string]any{"version": 1})
	n, _ = e.note(lkOps, plans.NotePrefix+"example-plan")
	require.Equal(t, plans.StatusApproved, plans.StatusFromTags(n.Tags))
	require.Equal(t, rAcct, n.Account)
	// The approval's tracking task is the account's too (the store stamps the
	// request's account on every task it creates).
	tasks, err := store.ListTasksForPlan(context.Background(), e.db, lkOps, "", "example-plan")
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, rAcct, tasks[0].Account)

	_, refused = e.sendAs(rAgent, "subagent-stop", "", map[string]any{"session_id": "r-cap", "cwd": "/work/ops",
		"agent_id": "a1", "agent_type": "Explore", "permission_mode": "plan"},
		map[string]any{"version": 1, "prompt": "Find the deploy race", "report": "The race is in the health check.", "git_head": pushHead})
	require.Empty(t, refused)
	agent, ok := e.note(lkOps, plans.AgentNotePrefix+"a1")
	require.True(t, ok)
	require.Equal(t, rAcct, agent.Account)
	require.Contains(t, agent.Body, "The race is in the health check.")
	e.requireNoteAccount(lkOps, plans.AgentNotePrefix+"a1", rAcct)

	// From its own session, a capture whose cwd is a closed project's
	// repository lands nowhere, and says nothing about why.
	for _, cwd := range []string{"/work/finance", "/work/vault", "/work/legal", "/work/elsewhere"} {
		ctxText, refused := e.sendAs(rAgent, "post-tool-use", "", map[string]any{"session_id": "r-cap", "cwd": cwd,
			"tool_name": "Write", "tool_input": map[string]any{"file_path": "/home/agent/.claude/plans/closed-plan.md"}},
			map[string]any{"version": 1, "plan": map[string]any{"basename": "closed-plan", "content": "# Closed\n\nX"}})
		require.Empty(t, refused, cwd)
		require.Empty(t, ctxText, cwd)
	}
	require.Zero(t, e.count(`SELECT count(*) FROM notes_index WHERE slug = ?`, plans.NotePrefix+"closed-plan"))

	_, refused = e.sendAs(rAgent, "session-end", "", map[string]any{"session_id": "r-cap"},
		map[string]any{"version": 1, "findings": "(auto-harvested) PUSHED-FINDINGS", "tokens": pushTokens, "model": "claude-final"})
	require.Empty(t, refused)
	sess = requireAmbientSession(t, e.db, ClientClaudeCode, rExt("r-cap"))
	require.Equal(t, core.SessionCompleted, sess.Status)
	require.Equal(t, "(auto-harvested) PUSHED-FINDINGS", sess.Findings)
	require.Equal(t, core.TokenUsage{Input: 7, Cached: 100, CacheCreation: 20, Output: 3, Total: 130}, sess.Tokens)
	require.Equal(t, "claude-final", sess.Model)
	require.Equal(t, rAcct, sess.Account)

	// Codex: the Stop push lands on its own session.
	e.sendAs(rAgent, "session-start", "codex", map[string]any{"session_id": "r-cx", "cwd": "/work/ops", "source": "startup"}, nil)
	e.sendAs(rAgent, "stop", "codex", map[string]any{"session_id": "r-cx", "cwd": "/work/ops", "last_assistant_message": "PAYLOAD"},
		map[string]any{"version": 1, "findings": "(auto-harvested) CODEX-PUSHED", "tokens": pushTokens})
	cx := requireAmbientSession(t, e.db, ClientCodex, rExt("r-cx"))
	require.Equal(t, "(auto-harvested) CODEX-PUSHED", cx.Findings)
	require.Equal(t, rAcct, cx.Account)

	// The owner's own capture of the same plan file re-stamps the content, and
	// with it the label: the owner's writes carry none.
	e.asOwner()
	e.sendAs(lkAgent, "session-start", "", map[string]any{"session_id": "o-cap", "cwd": "/work/ops", "source": "startup"}, nil)
	e.sendAs(lkAgent, "post-tool-use", "", map[string]any{"session_id": "o-cap", "cwd": "/work/ops",
		"tool_name": "Write", "tool_input": map[string]any{"file_path": planFile}}, planPush("# Example plan\n\nThe owner's rewrite."))
	n, _ = e.note(lkOps, plans.NotePrefix+"example-plan")
	require.Contains(t, n.Body, "The owner's rewrite.")
	require.Empty(t, n.Account)
	e.requireNoteAccount(lkOps, plans.NotePrefix+"example-plan", "")

	// peer_auth off: the daemon's own read of the plan file, exactly as
	// before -- no account on the session, the note or its frontmatter.
	e.off()
	offPlan := filepath.Join(e.plansDir, "off-plan.md")
	require.NoError(t, os.WriteFile(offPlan, []byte("# Off plan\n\nFrom the disk."), 0o600))
	e.sendAs(lkAgent, "session-start", "", map[string]any{"session_id": "off-cap", "cwd": "/work/ops", "source": "startup"}, nil)
	require.Empty(t, requireAmbientSession(t, e.db, ClientClaudeCode, "off-cap").Account)
	e.sendAs(lkAgent, "post-tool-use", "", map[string]any{"session_id": "off-cap", "cwd": "/work/ops",
		"tool_name": "Write", "tool_input": map[string]any{"file_path": offPlan}}, nil)
	n, ok = e.note(lkOps, plans.NotePrefix+"off-plan")
	require.True(t, ok)
	require.Contains(t, n.Body, "From the disk.")
	e.requireNoteAccount(lkOps, plans.NotePrefix+"off-plan", "")
}

// requireNoteAccount checks a note's account in its index row and in its
// file's frontmatter, where an owner's note carries no account key at all.
func (e *restrictedHooks) requireNoteAccount(project, slug, account string) {
	e.t.Helper()
	idx, ok, err := store.NoteBySlug(context.Background(), e.db, project, slug)
	require.NoError(e.t, err)
	require.True(e.t, ok, slug)
	require.Equal(e.t, account, idx.Account, "%s: the index mirrors the label", slug)
	raw, err := e.mgr.Store().ReadFile(idx.FilePath)
	require.NoError(e.t, err)
	front := strings.SplitN(raw, "\n---", 2)[0]
	if account == "" {
		require.NotContains(e.t, front, "account:", "%s: an owner's note has no account key", slug)
		return
	}
	require.Contains(e.t, front, "account: "+account, slug)
}
