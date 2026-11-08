package hooks

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// pushHooks is lockHooks (example-finance locked, the owner's socket View
// for the `seam hook` process under the agent) with the files layer and plan
// capture on, for the captures `seam hook` pushes under peer_auth. The
// daemon's own plans dir is empty: a capture that lands came from the push.
type pushHooks struct {
	*lockHooks
	mgr      *files.Manager
	plansDir string
}

func newPushHooks(t *testing.T) *pushHooks {
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
	})
	return e
}

// off serves every later request with no View: peer_auth off.
func (e *pushHooks) off() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.view = nil
}

// fromChain serves every later request as the owner's socket connection from
// a process tree of chain (the hook first), with the leases live on it.
func (e *pushHooks) fromChain(chain ...access.ProcessToken) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.view = func() (access.View, bool) {
		v, ok := e.policy.PeerChainView(501, chain)
		require.True(e.t, ok)
		now := e.clock()
		keys := make([]string, 0, len(chain))
		for _, c := range chain {
			keys = append(keys, c.Key())
		}
		rows, err := store.ActiveAccessLeasesFor(context.Background(), e.db, keys, now)
		require.NoError(e.t, err)
		return v.WithLeases(now, store.AccessLeases(rows)), true
	}
}

// send posts a hook as the agent's `seam hook` would, with capture pushed
// under harvest.Field (nil: no push), and returns the hook's context ("" for
// an ack) and the refusal header.
func (e *pushHooks) send(endpoint, client string, body, capture map[string]any) (hookContext, refused string) {
	e.t.Helper()
	q := url.Values{"host": {localHostName}, "agent_process": {lkAgent.AgentID()}}
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
	resp, out := post(e.t, e.url+"/api/hooks/"+endpoint+"?"+q.Encode(), testKey, body)
	require.Equal(e.t, http.StatusOK, resp.StatusCode)
	if hso, ok := out["hookSpecificOutput"].(map[string]any); ok {
		hookContext, _ = hso["additionalContext"].(string)
	}
	return hookContext, resp.Header.Get(harvest.RefusedHeader)
}

// note loads a note by project and slug.
func (e *pushHooks) note(project, slug string) (core.Note, bool) {
	e.t.Helper()
	idx, ok, err := store.NoteBySlug(context.Background(), e.db, project, slug)
	require.NoError(e.t, err)
	if !ok {
		return core.Note{}, false
	}
	n, err := e.mgr.Store().ReadNote(idx.FilePath)
	require.NoError(e.t, err)
	return n, true
}

var (
	pushHead   = strings.Repeat("ab", 20)
	pushTokens = map[string]any{"input": 7, "cached": 100, "cacheCreation": 20, "output": 3, "total": 130}
)

// diskTranscript is a Claude Code transcript the daemon would harvest from:
// its findings, model and tokens differ from every pushed value.
func diskTranscript(t *testing.T, text string) string {
	t.Helper()
	return writeTranscript(t, `{"type":"assistant","message":{"id":"m1","role":"assistant","model":"claude-disk",`+
		`"content":[{"type":"text","text":"`+text+`"}],"usage":{"input_tokens":10,"output_tokens":5}}}`+"\n")
}

func planPush(content string) map[string]any {
	return map[string]any{"version": 1, "git_head": pushHead,
		"plan": map[string]any{"basename": "example-plan", "content": content}}
}

// With peer_auth off nothing reads the pushed field: a body carrying one --
// forged, since no `seam hook` pushes off the socket -- is harvested from the
// agent's disk exactly as before, and no response carries the refusal header.
func TestPushedCapture_OffIsIgnored(t *testing.T) {
	e := newPushHooks(t)
	e.off()
	transcript := diskTranscript(t, "DISK-FINDINGS")
	forged := map[string]any{"version": 1, "findings": "(auto-harvested) FORGED", "model": "claude-forged",
		"tokens": pushTokens, "git_head": pushHead}

	_, refused := e.send("session-start", "", map[string]any{"session_id": "off-1", "cwd": "/work/ops",
		"source": "startup", "transcript_path": transcript}, forged)
	require.Empty(t, refused)
	require.Equal(t, "claude-disk", requireAmbientSession(t, e.db, ClientClaudeCode, "off-1").Model)

	planFile := filepath.Join(e.plansDir, "example-plan.md")
	require.NoError(t, os.WriteFile(planFile, []byte("# Example plan\n\nFrom the disk."), 0o600))
	_, refused = e.send("post-tool-use", "", map[string]any{"session_id": "off-1", "cwd": "/work/ops",
		"tool_name": "Write", "tool_input": map[string]any{"file_path": planFile}},
		planPush("# Forged plan\n\nFORGED"))
	require.Empty(t, refused)
	n, ok := e.note(lkOps, plans.NotePrefix+"example-plan")
	require.True(t, ok)
	require.Contains(t, n.Body, "From the disk.")
	require.NotContains(t, n.Body, "FORGED")
	require.NotContains(t, n.Body, "git "+pushHead[:12], "the stamp is the daemon's own read")

	_, refused = e.send("session-end", "", map[string]any{"session_id": "off-1", "transcript_path": transcript}, forged)
	require.Empty(t, refused)
	sess := requireAmbientSession(t, e.db, ClientClaudeCode, "off-1")
	require.Equal(t, "(auto-harvested) DISK-FINDINGS", sess.Findings)
	require.Equal(t, 10, sess.Tokens.Input)
	require.Equal(t, 5, sess.Tokens.Output)

	// Codex's Stop: the rollout and the payload, never the push.
	e.send("session-start", "codex", map[string]any{"session_id": "cx-off", "cwd": "/work/ops", "source": "startup"}, nil)
	e.send("stop", "codex", map[string]any{"session_id": "cx-off", "cwd": "/work/ops",
		"last_assistant_message": "CODEX-PAYLOAD"}, forged)
	cx := requireAmbientSession(t, e.db, ClientCodex, "cx-off")
	require.Equal(t, "(auto-harvested) CODEX-PAYLOAD", cx.Findings)
	require.True(t, cx.Tokens.Empty(), "no rollout: no tokens, whatever the body says")
}

// Under peer_auth a push into an open project lands where the daemon's own
// read would have put it, and the daemon reads nothing: every path the hooks
// name here holds other text, and none of it arrives.
func TestPushedCapture_LandsInAnOpenProject(t *testing.T) {
	e := newPushHooks(t)
	transcript := diskTranscript(t, "DISK-SECRET")

	e.send("session-start", "", map[string]any{"session_id": "ops-1", "cwd": "/work/ops", "source": "startup",
		"transcript_path": transcript}, map[string]any{"version": 1, "model": "claude-pushed"})
	sess := requireAmbientSession(t, e.db, ClientClaudeCode, "ops-1")
	require.Equal(t, lkOps, sess.ProjectSlug)
	require.Equal(t, "claude-pushed", sess.Model, "the pushed sniff, not the transcript")

	// A plan iteration: the note, with the pushed git head in its stamp.
	ctxText, refused := e.send("post-tool-use", "", map[string]any{"session_id": "ops-1", "cwd": "/work/ops",
		"tool_name": "Write", "tool_input": map[string]any{"file_path": "/home/owner/.claude/plans/example-plan.md"}},
		planPush("# Example plan\n\nStep one."))
	require.Empty(t, refused)
	require.Contains(t, ctxText, "composition plan:example-plan")
	n, ok := e.note(lkOps, plans.NotePrefix+"example-plan")
	require.True(t, ok)
	require.Contains(t, n.Body, "Step one.")
	require.Contains(t, n.Body, "git "+pushHead[:12])
	require.Equal(t, "claude-pushed", n.Model)

	// The approval: the pushed final text, approved.
	e.send("post-tool-use", "", map[string]any{"session_id": "ops-1", "cwd": "/work/ops",
		"tool_name": "ExitPlanMode", "tool_response": map[string]any{"filePath": "/home/owner/.claude/plans/example-plan.md"}},
		planPush("# Example plan\n\nStep one. Step two."))
	n, _ = e.note(lkOps, plans.NotePrefix+"example-plan")
	require.Contains(t, n.Body, "Step two.")
	require.Equal(t, plans.StatusApproved, plans.StatusFromTags(n.Tags))

	// A planning subagent's report.
	_, refused = e.send("subagent-stop", "", map[string]any{"session_id": "ops-1", "cwd": "/work/ops",
		"agent_id": "a1", "agent_type": "Explore", "permission_mode": "plan", "agent_transcript_path": transcript},
		map[string]any{"version": 1, "prompt": "Find the deploy race", "report": "The race is in the health check.",
			"git_head": pushHead})
	require.Empty(t, refused)
	agent, ok := e.note(lkOps, plans.AgentNotePrefix+"a1")
	require.True(t, ok)
	require.Contains(t, agent.Body, "## Prompt\n\nFind the deploy race")
	require.Contains(t, agent.Body, "## Report\n\nThe race is in the health check.")
	require.Contains(t, agent.Body, "git "+pushHead[:12])

	// The end: the pushed findings, tokens and model.
	_, refused = e.send("session-end", "", map[string]any{"session_id": "ops-1", "transcript_path": transcript},
		map[string]any{"version": 1, "findings": "(auto-harvested) PUSHED-FINDINGS", "tokens": pushTokens,
			"model": "claude-final"})
	require.Empty(t, refused)
	sess = requireAmbientSession(t, e.db, ClientClaudeCode, "ops-1")
	require.Equal(t, core.SessionCompleted, sess.Status)
	require.Equal(t, "(auto-harvested) PUSHED-FINDINGS", sess.Findings)
	require.Equal(t, core.TokenUsage{Input: 7, Cached: 100, CacheCreation: 20, Output: 3, Total: 130}, sess.Tokens)
	require.Equal(t, "claude-final", sess.Model)

	// Codex's Stop.
	e.send("session-start", "codex", map[string]any{"session_id": "cx-1", "cwd": "/work/ops", "source": "startup"}, nil)
	e.send("stop", "codex", map[string]any{"session_id": "cx-1", "cwd": "/work/ops", "transcript_path": transcript,
		"last_assistant_message": "PAYLOAD-TEXT"},
		map[string]any{"version": 1, "findings": "(auto-harvested) CODEX-PUSHED", "tokens": pushTokens})
	cx := requireAmbientSession(t, e.db, ClientCodex, "cx-1")
	require.Equal(t, "(auto-harvested) CODEX-PUSHED", cx.Findings)
	require.Equal(t, 130, cx.Tokens.Total)

	// Nothing from the files the hooks named.
	rows, err := e.db.QueryContext(context.Background(), `SELECT count(*) FROM events WHERE payload LIKE '%DISK-SECRET%'`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next())
	var leaked int
	require.NoError(t, rows.Scan(&leaked))
	require.NoError(t, rows.Err())
	require.Zero(t, leaked)
}

// In a locked project a push is refused before the lease -- the one-line
// notice, and nothing recorded but the session's metadata (the pushed model
// among it) -- and accepted after it, with the current state.
func TestPushedCapture_LockedRefusedBeforeTheLeaseAcceptedAfter(t *testing.T) {
	e := newPushHooks(t)
	notice, _ := e.send("session-start", "", map[string]any{"session_id": "fin-1", "cwd": "/work/finance",
		"source": "startup"}, map[string]any{"version": 1, "model": "claude-pushed"})
	require.Contains(t, notice, "is locked")
	sess := requireAmbientSession(t, e.db, ClientClaudeCode, "fin-1")
	require.Equal(t, lkFin, sess.ProjectSlug)
	require.Equal(t, "claude-pushed", sess.Model, "the model is metadata: a locked session takes it")

	plan := map[string]any{"session_id": "fin-1", "cwd": "/work/finance", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": "/home/owner/.claude/plans/fin-plan.md"}}
	finPlan := func(content string) map[string]any {
		return map[string]any{"version": 1, "plan": map[string]any{"basename": "fin-plan", "content": content}}
	}
	want := "Seamless did not record this plan: project " + lkFin + " is locked and this session holds no approval; " +
		"the next capture after the owner approves carries the current state (to ask: `seam access request " + lkFin + "`)."
	ctxText, refused := e.send("post-tool-use", "", plan, finPlan("# Fin plan\n\nFirst draft."))
	require.Equal(t, want, refused, "one line, on the header seam hook prints")
	require.Equal(t, "<seam-plan-context>\n"+want+"\n</seam-plan-context>", ctxText)
	_, ok := e.note(lkFin, plans.NotePrefix+"fin-plan")
	require.False(t, ok, "nothing lands in the locked project")
	_, refused = e.send("permission-request", "", map[string]any{"session_id": "fin-1", "cwd": "/work/finance",
		"tool_name": "ExitPlanMode"}, map[string]any{"version": 1})
	require.Empty(t, refused, "presenting records no content: nothing to say")

	_, refused = e.send("subagent-stop", "", map[string]any{"session_id": "fin-1", "cwd": "/work/finance",
		"agent_id": "a2", "agent_type": "Explore", "permission_mode": "plan"},
		map[string]any{"version": 1, "prompt": "p", "report": "the finance report"})
	require.Contains(t, refused, "Seamless did not record this subagent's report: project "+lkFin+" is locked")
	_, ok = e.note(lkFin, plans.AgentNotePrefix+"a2")
	require.False(t, ok)

	// The owner approves: the next capture carries the current state.
	e.grant(sess.ID, lkFin, time.Hour)
	ctxText, refused = e.send("post-tool-use", "", plan, finPlan("# Fin plan\n\nSecond draft."))
	require.Empty(t, refused)
	require.Contains(t, ctxText, "composition plan:fin-plan")
	n, ok := e.note(lkFin, plans.NotePrefix+"fin-plan")
	require.True(t, ok)
	require.Contains(t, n.Body, "Second draft.")
	require.Equal(t, 1, plans.NoteIteration(n), "the refused draft was never recorded, and is not retried")

	_, refused = e.send("session-end", "", map[string]any{"session_id": "fin-1"},
		map[string]any{"version": 1, "findings": "(auto-harvested) FIN-FINDINGS", "tokens": pushTokens})
	require.Empty(t, refused)
	sess = requireAmbientSession(t, e.db, ClientClaudeCode, "fin-1")
	require.Equal(t, "(auto-harvested) FIN-FINDINGS", sess.Findings)
	require.Equal(t, 130, sess.Tokens.Total)
}

// The end of a locked session before any lease: completed, metadata only, the
// pushed findings refused with the notice. Codex's Stop likewise.
func TestPushedCapture_LockedEndAndStopRefused(t *testing.T) {
	e := newPushHooks(t)
	e.send("session-start", "", map[string]any{"session_id": "fin-2", "cwd": "/work/finance", "source": "startup"}, nil)
	_, refused := e.send("session-end", "", map[string]any{"session_id": "fin-2"},
		map[string]any{"version": 1, "findings": "(auto-harvested) FIN-SECRET", "tokens": pushTokens, "model": "claude-x"})
	require.Contains(t, refused, "Seamless did not record this session's findings and token totals: project "+lkFin+" is locked")
	sess := requireAmbientSession(t, e.db, ClientClaudeCode, "fin-2")
	require.Equal(t, core.SessionCompleted, sess.Status)
	require.Empty(t, sess.Findings)
	require.True(t, sess.Tokens.Empty())

	e.send("session-start", "codex", map[string]any{"session_id": "cx-fin", "cwd": "/work/finance", "source": "startup"}, nil)
	_, refused = e.send("stop", "codex", map[string]any{"session_id": "cx-fin", "cwd": "/work/finance"},
		map[string]any{"version": 1, "findings": "(auto-harvested) FIN-SECRET", "tokens": pushTokens})
	require.Contains(t, refused, "Seamless did not record this turn's findings and token totals")
	cx := requireAmbientSession(t, e.db, ClientCodex, "cx-fin")
	require.Empty(t, cx.Findings)
	require.True(t, cx.Tokens.Empty())

	// A heartbeat-only Stop pushes nothing a lock refuses, and says nothing.
	_, refused = e.send("stop", "codex", map[string]any{"session_id": "cx-fin", "cwd": "/work/finance"}, nil)
	require.Empty(t, refused)
}

// A lease covers the agent's process tree as the kernel reports it, so a push
// from another tree is refused -- even one whose self-reported agent process
// names the leased agent (send always does).
func TestPushedCapture_OutsideTheLeasedTreeRefused(t *testing.T) {
	e := newPushHooks(t)
	e.send("session-start", "", map[string]any{"session_id": "fin-3", "cwd": "/work/finance", "source": "startup"}, nil)
	e.grant(requireAmbientSession(t, e.db, ClientClaudeCode, "fin-3").ID, lkFin, time.Hour)

	plan := map[string]any{"session_id": "fin-3", "cwd": "/work/finance", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": "/home/owner/.claude/plans/fin-plan.md"}}
	other := []access.ProcessToken{{PID: 900, Version: 9000, Boot: lkBoot}, {PID: 800, Version: 8000, Boot: lkBoot}}
	e.fromChain(other...)
	_, refused := e.send("post-tool-use", "", plan, map[string]any{"version": 1,
		"plan": map[string]any{"basename": "fin-plan", "content": "# Fin plan\n\nFrom another tree."}})
	// The session the hook names is another tree's (its stamped agent process
	// is not in this chain), so it is foreign: refused silently, as another
	// account's session is -- nothing about it is this tree's to learn.
	require.Empty(t, refused)
	_, ok := e.note(lkFin, plans.NotePrefix+"fin-plan")
	require.False(t, ok)

	e.fromChain(lkHookProc, lkAgent)
	_, refused = e.send("post-tool-use", "", plan, map[string]any{"version": 1,
		"plan": map[string]any{"basename": "fin-plan", "content": "# Fin plan\n\nFrom the leased tree."}})
	require.Empty(t, refused)
	n, ok := e.note(lkFin, plans.NotePrefix+"fin-plan")
	require.True(t, ok)
	require.Contains(t, n.Body, "From the leased tree.")
}

// A note's events are filed under the session's project, so a leased locked
// project takes a pushed note only from a session recorded in it: never its
// content in another project's records.
func TestPushedCapture_LeasedProjectFromAnotherProjectsSessionRefused(t *testing.T) {
	e := newPushHooks(t)
	e.send("session-start", "", map[string]any{"session_id": "ops-2", "cwd": "/work/ops", "source": "startup"}, nil)
	e.grant("", lkFin, time.Hour)
	_, refused := e.send("post-tool-use", "", map[string]any{"session_id": "ops-2", "cwd": "/work/finance",
		"tool_name": "Write", "tool_input": map[string]any{"file_path": "/home/owner/.claude/plans/fin-plan.md"}},
		map[string]any{"version": 1, "plan": map[string]any{"basename": "fin-plan", "content": "# Fin plan\n\nx"}})
	require.Equal(t, "Seamless did not record this plan: it would land in locked project "+lkFin+
		" from a session recorded in another project; capture it from a session started in "+lkFin+".", refused)
	_, ok := e.note(lkFin, plans.NotePrefix+"fin-plan")
	require.False(t, ok)
}

// A push that does not validate is dropped whole and recorded, never applied
// in part, and the hook still answers.
func TestPushedCapture_InvalidPushDroppedWhole(t *testing.T) {
	e := newPushHooks(t)
	e.send("session-start", "", map[string]any{"session_id": "ops-3", "cwd": "/work/ops", "source": "startup"}, nil)
	for _, bad := range []map[string]any{
		{"version": 2, "plan": map[string]any{"basename": "bad-plan", "content": "# Bad\n\nx"}},
		{"version": 1, "git_head": "HEAD", "plan": map[string]any{"basename": "bad-plan", "content": "# Bad\n\nx"}},
		{"version": 1, "plan": map[string]any{"basename": "../escape", "content": "# Bad\n\nx"}},
		{"version": "1"},
	} {
		e.send("post-tool-use", "", map[string]any{"session_id": "ops-3", "cwd": "/work/ops", "tool_name": "Write",
			"tool_input": map[string]any{"file_path": "/home/owner/.claude/plans/bad-plan.md"}}, bad)
	}
	_, ok := e.note(lkOps, plans.NotePrefix+"bad-plan")
	require.False(t, ok)
	_, refused := e.send("session-end", "", map[string]any{"session_id": "ops-3"},
		map[string]any{"version": 1, "findings": "(auto-harvested) x", "tokens": map[string]any{"input": 1, "total": 5}})
	require.Empty(t, refused)
	require.Empty(t, requireAmbientSession(t, e.db, ClientClaudeCode, "ops-3").Findings,
		"a token total that is not its parts' sum refuses the whole push")

	rows, err := e.db.QueryContext(context.Background(),
		`SELECT count(*) FROM events WHERE kind = ? AND payload LIKE '%capture-push-invalid%'`, string(core.EventHookError))
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next())
	var n int
	require.NoError(t, rows.Scan(&n))
	require.NoError(t, rows.Err())
	require.Equal(t, 5, n)
}

// The sealed-account design (peer_auth.users): a configured account's pushed
// capture lands in its home project when its cwd maps to none of its own --
// as its sessions do, never in the global scope -- and the owner's push into a
// project sealed to that account is refused without a word about it.
func TestPushedCapture_SealedAccountLandsHomeOwnerRefusedSilently(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(ctx, db, localHostName))
	for slug, repo := range map[string]string{lkFin: "/work/finance", lkOps: "/work/ops"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.AddRepoMapping(ctx, db, repo, slug))
	}
	accounts := map[string]access.Account{"owner": {UID: 501, Name: "owner"}, "_example_finance": {UID: 470, Name: "_example_finance"}}
	policy, err := access.New(config.PeerAuth{Owner: "owner", Users: map[string]config.PeerUser{
		"_example_finance": {Sealed: []string{lkFin}},
	}}, func(n string) (access.Account, error) {
		if a, ok := accounts[n]; ok {
			return a, nil
		}
		return access.Account{}, fmt.Errorf("no account %s", n)
	})
	require.NoError(t, err)
	mgr, err := files.NewManager(filepath.Join(t.TempDir(), "data"), db, nil)
	require.NoError(t, err)
	h := NewHandler(Config{
		DB: db, Retrieve: retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil),
		Events: events.NewRecorder(db), Files: mgr, APIKey: testKey, LocalHost: localHostName,
		PlanCapture: config.PlanCapture{Enabled: true}, PlansDir: t.TempDir(),
	})
	mux := http.NewServeMux()
	h.Register(mux)
	var uid atomic.Uint32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, ok := policy.PeerView(uid.Load())
		if !ok {
			http.Error(w, "unknown uid", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r.WithContext(access.WithView(r.Context(), v)))
	}))
	t.Cleanup(ts.Close)
	hook := func(endpoint string, body map[string]any) string {
		q := url.Values{"host": {localHostName}}
		resp, _ := post(t, ts.URL+"/api/hooks/"+endpoint+"?"+q.Encode(), "", body)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return resp.Header.Get(harvest.RefusedHeader)
	}
	planWrite := func(sid, cwd, basename string) string {
		return hook("post-tool-use", map[string]any{"session_id": sid, "cwd": cwd, "tool_name": "Write",
			"tool_input":  map[string]any{"file_path": "/home/x/.claude/plans/" + basename + ".md"},
			harvest.Field: map[string]any{"version": 1, "plan": map[string]any{"basename": basename, "content": "# " + basename + "\n\nx"}}})
	}
	noteIn := func(project, slug string) bool {
		_, ok, err := store.NoteBySlug(ctx, db, project, slug)
		require.NoError(t, err)
		return ok
	}

	uid.Store(470)
	hook("session-start", map[string]any{"session_id": "acct-1", "cwd": "/opt/example/app", "source": "startup"})
	require.Equal(t, lkFin, requireAmbientSession(t, db, ClientClaudeCode, "acct-1").ProjectSlug)
	require.Empty(t, planWrite("acct-1", "/opt/example/app", "acct-plan"))
	require.True(t, noteIn(lkFin, plans.NotePrefix+"acct-plan"), "the account's home project")
	require.False(t, noteIn("", plans.NotePrefix+"acct-plan"), "never the global scope")

	uid.Store(501)
	hook("session-start", map[string]any{"session_id": "own-1", "cwd": "/work/ops", "source": "startup"})
	require.Empty(t, planWrite("own-1", "/work/finance", "owner-plan"), "nothing to say about a sealed project")
	require.False(t, noteIn(lkFin, plans.NotePrefix+"owner-plan"))
	require.Empty(t, planWrite("own-1", "/work/ops", "ops-plan"))
	require.True(t, noteIn(lkOps, plans.NotePrefix+"ops-plan"), "the owner's own project takes it")
}
