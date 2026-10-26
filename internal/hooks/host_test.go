package hooks

// Host-scoped hook behaviour: which machine a hook speaks for, and what the
// daemon refuses to read off its own disk on that machine's behalf.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const (
	localHostName  = "alpha"
	remoteHostName = "beta"
)

// hostEnv is a hook server that has NAMED its machine, which is what makes a
// hook from any other machine remote.
type hostEnv struct {
	ts       *httptest.Server
	db       *sql.DB
	mgr      *files.Manager
	plansDir string
}

func newHostEnv(t *testing.T, pc config.PlanCapture) *hostEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap, `{"/work/demo":"demo"}`))
	require.NoError(t, store.AdoptLocalHost(ctx, db, localHostName))
	insertMemory(t, db, "01A", "constraint", "no-force-push", "never force push to main", "demo")

	mgr, err := files.NewManager(filepath.Join(dir, "data"), db, nil)
	require.NoError(t, err)
	plansDir := filepath.Join(dir, "plans")
	require.NoError(t, os.MkdirAll(plansDir, 0o755))

	ret := retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	h := NewHandler(Config{
		DB: db, Retrieve: ret, Events: events.NewRecorder(db), Files: mgr,
		APIKey: testKey, PlanCapture: pc, PlansDir: plansDir, LocalHost: localHostName,
	})
	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &hostEnv{ts: ts, db: db, mgr: mgr, plansDir: plansDir}
}

// postFrom posts a hook body with the machine identity a `seam hook` on that
// host would append.
func (e *hostEnv) postFrom(t *testing.T, endpoint string, id map[string]string, body map[string]any) map[string]any {
	t.Helper()
	q := url.Values{}
	for k, v := range id {
		q.Set(k, v)
	}
	u := e.ts.URL + "/api/hooks/" + endpoint
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	resp, out := post(t, u, testKey, body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return out
}

// hookErrors returns every hook.error payload recorded so far.
func (e *hostEnv) hookErrors(t *testing.T) []map[string]any {
	t.Helper()
	rows, err := e.db.Query(`SELECT payload FROM events WHERE kind = 'hook.error'`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var raw string
		require.NoError(t, rows.Scan(&raw))
		var p map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &p))
		out = append(out, p)
	}
	require.NoError(t, rows.Err())
	return out
}

// skipEvents returns the remote-host-skip hook.error payloads recorded so far.
func (e *hostEnv) skipEvents(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, p := range e.hookErrors(t) {
		if stage, _ := p["stage"].(string); stage == "remote-host-skip" {
			out = append(out, p)
		}
	}
	return out
}

func (e *hostEnv) skippedWhat(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, p := range e.skipEvents(t) {
		what, _ := p["what"].(string)
		out = append(out, what)
	}
	return out
}

func (e *hostEnv) session(t *testing.T, externalID string) core.Session {
	t.Helper()
	sess, ok, err := store.AmbientSessionByExternalIdentity(
		context.Background(), e.db, ClientClaudeCode.externalIdentity(), externalID)
	require.NoError(t, err)
	require.True(t, ok)
	return sess
}

// The identity is read off the query string, every field optional, and the host
// is lower-cased because it arrives over the wire where case is not preserved.
func TestIdentityFromRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want hookIdentity
	}{
		{"absent", "", hookIdentity{}},
		{"client only", "client=codex", hookIdentity{}},
		{"host only", "host=Beta", hookIdentity{Host: "beta"}},
		{"full", "host=beta&repo_root=%2Fsrv%2Fapp&main_root=%2Fsrv%2Fapp&origin=git%40h%3Aa%2Fb.git",
			hookIdentity{Host: "beta", RepoRoot: "/srv/app", MainRoot: "/srv/app", Origin: "git@h:a/b.git"}},
		{"windows root", `host=beta&repo_root=C%3A%5Crepos%5Capp`,
			hookIdentity{Host: "beta", RepoRoot: `C:\repos\app`}},
		{"agent process", "agent_process=12706.1759850172000000",
			hookIdentity{AgentProcess: "12706.1759850172000000"}},
		// Anything that is not an identity is dropped rather than stored: it
		// could only ever fail to match, and the hook must not fail over it.
		{"malformed agent process", "agent_process=claude-code", hookIdentity{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/hooks/session-start?"+tc.raw, nil)
			require.Equal(t, tc.want, identityFromRequest(r))
		})
	}
}

// The CLI's copy of the identity keys must be the daemon's, or the params are
// appended under names nothing reads -- a silent no-op, because hooks fail open.
func TestIdentityQueryParams_AreStable(t *testing.T) {
	require.Equal(t, []string{"host", "repo_root", "main_root", "origin", "agent_process"}, IdentityQueryParams())
}

// A hook that sends no host is an older `seam`, which can only be talking to a
// daemon on its own machine.
func TestSameHost(t *testing.T) {
	h := NewHandler(Config{APIKey: testKey, LocalHost: localHostName})
	require.True(t, h.sameHost(""), "absent host = an older client on this machine")
	require.True(t, h.sameHost(localHostName))
	require.True(t, h.sameHost(strings.ToUpper(localHostName)))
	require.False(t, h.sameHost(remoteHostName))
}

// SessionStart stamps the machine on the ambient session and places the remote
// cwd using the roots the client resolved -- never the daemon's own disk.
func TestSessionStart_StampsHostAndPlacesRemoteRepo(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})

	e.postFrom(t, "session-start", map[string]string{
		"host": remoteHostName, "repo_root": `C:\repos\myapp`, "main_root": `C:\repos\myapp`,
		"origin": "https://github.com/acme/myapp.git",
	}, map[string]any{"session_id": "remote-1", "cwd": `C:\repos\myapp`, "source": "startup"})

	sess := e.session(t, "remote-1")
	require.Equal(t, remoteHostName, sess.Host)
	require.Equal(t, "myapp", sess.ProjectSlug)

	// The placement is scoped to the remote machine: the daemon's own map is
	// untouched.
	local, err := store.ResolveProjectForCWD(context.Background(), e.db, localHostName, `C:\repos\myapp`)
	require.NoError(t, err)
	require.Empty(t, local)

	// A local session still stamps the daemon's host, with no identity sent at all.
	e.postFrom(t, "session-start", nil,
		map[string]any{"session_id": "local-1", "cwd": "/work/demo", "source": "startup"})
	require.Equal(t, localHostName, e.session(t, "local-1").Host)
	require.Equal(t, "demo", e.session(t, "local-1").ProjectSlug)
}

// A remote session that named no repo root cannot be placed, and that is
// recorded under its own stage rather than silently resolving to global.
func TestSessionStart_RemoteWithoutRootRecordsItsOwnStage(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})

	e.postFrom(t, "session-start", map[string]string{"host": remoteHostName},
		map[string]any{"session_id": "remote-2", "cwd": "/work/demo", "source": "startup"})

	sess := e.session(t, "remote-2")
	require.Equal(t, remoteHostName, sess.Host)
	require.Empty(t, sess.ProjectSlug,
		"a remote cwd that happens to match a LOCAL mapped path must not be placed there")

	found := false
	for _, p := range e.hookErrors(t) {
		if stage, _ := p["stage"].(string); stage == "register-project-remote-root" {
			found = true
		}
	}
	require.True(t, found, "the unplaceable remote cwd must be recorded, not swallowed")
}

// UserPromptSubmit is an http hook with no identity of its own; the host comes
// from the ambient session SessionStart already created.
func TestUserPromptSubmit_AttributesHostViaTheAmbientSession(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})
	ctx := context.Background()

	// Two machines, the same absolute path, two genuinely different repositories
	// (their origins are both known and disagree), so the same cwd is two
	// projects. This is the case host scoping exists for.
	const shared = "/srv/shared"
	localSlug, _, err := store.RegisterProjectForCWD(ctx, e.db, store.CWDIdentity{
		Host: localHostName, CWD: shared, RepoRoot: shared,
		Origin: "https://github.com/acme/shared.git",
	}, localHostName)
	require.NoError(t, err)
	require.Equal(t, "shared", localSlug)
	remoteSlug, _, err := store.RegisterProjectForCWD(ctx, e.db, store.CWDIdentity{
		Host: remoteHostName, CWD: shared, RepoRoot: shared,
		Origin: "https://github.com/other/shared.git",
	}, localHostName)
	require.NoError(t, err)
	require.Equal(t, "shared-2", remoteSlug)

	// The prompt matcher is IDF-weighted, so a one-document corpus scores zero:
	// each project needs a few memories before a match can clear the floor.
	insertMemory(t, e.db, "02A", "constraint", "local-only-rule", "force push is banned on the local clone", "shared")
	insertMemory(t, e.db, "02B", "constraint", "remote-only-rule", "force push is welcome on the remote fork", "shared-2")
	for i, filler := range []struct{ name, desc string }{
		{"sqlite-wal-mode", "enable wal journal mode and busy timeout"},
		{"ulid-over-uuid", "use ulid identifiers never uuid values"},
		{"chroma-boot-race", "chroma container health check startup race"},
	} {
		insertMemory(t, e.db, "03"+string(rune('A'+i)), "reference", filler.name, filler.desc, "shared")
		insertMemory(t, e.db, "04"+string(rune('A'+i)), "reference", filler.name+"-r", filler.desc, "shared-2")
	}

	e.postFrom(t, "session-start", map[string]string{
		"host": remoteHostName, "repo_root": shared, "main_root": shared,
		"origin": "https://github.com/other/shared.git",
	}, map[string]any{"session_id": "remote-3", "cwd": shared, "source": "startup"})
	require.Equal(t, "shared-2", e.session(t, "remote-3").ProjectSlug)

	// The prompt hook carries NO identity of its own -- the host has to come from
	// the ambient row above.
	out := e.postFrom(t, "user-prompt-submit", nil, map[string]any{
		"session_id": "remote-3", "cwd": shared,
		"user_prompt": "can I force push on the remote fork",
	})
	ctxText := additionalContext(t, out)
	require.Contains(t, ctxText, "remote-only-rule",
		"the prompt must be recalled against the REMOTE machine's project")
	require.NotContains(t, ctxText, "local-only-rule",
		"the local machine's project for the same path must not leak in")
}

// Plan capture re-reads the plan file from disk, so it runs for the daemon's own
// machine and is recorded as skipped for any other.
func TestPlanCapture_SkipsARemoteHostAndRunsLocally(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{Enabled: true})
	path := filepath.Join(e.plansDir, "shared-plan.md")
	require.NoError(t, os.WriteFile(path, []byte("# Shared Plan\n\nStep one.\n"), 0o644))

	e.postFrom(t, "session-start", nil,
		map[string]any{"session_id": testSID, "cwd": "/work/demo", "source": "startup"})

	// Remote: nothing captured, one recorded skip.
	e.postFrom(t, "post-tool-use", map[string]string{"host": remoteHostName}, map[string]any{
		"session_id": testSID, "cwd": "/work/demo", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": path},
	})
	_, ok, err := store.NoteBySlug(context.Background(), e.db, "demo", "cc-plan-shared-plan")
	require.NoError(t, err)
	require.False(t, ok, "a remote agent's plan file is not on this disk")
	require.Contains(t, e.skippedWhat(t), "plan-capture")

	// Local: the same call captures.
	e.postFrom(t, "post-tool-use", map[string]string{"host": localHostName}, map[string]any{
		"session_id": testSID, "cwd": "/work/demo", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": path},
	})
	_, ok, err = store.NoteBySlug(context.Background(), e.db, "demo", "cc-plan-shared-plan")
	require.NoError(t, err)
	require.True(t, ok, "the daemon's own machine still captures")
}

// SessionEnd harvests findings, the final model and the token totals out of the
// transcript. All three are one disk read, and none of them may happen for an
// agent on another machine -- whose transcript path may well name a real file
// here, belonging to someone else.
func TestSessionEnd_RemoteHostHarvestsNothing(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})
	transcript := writeTranscript(t, strings.Join([]string{
		`{"type":"assistant","isSidechain":false,"message":{"id":"m1","model":"claude-fable-5","usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Final summary."}]}}`,
	}, "\n"))

	e.postFrom(t, "session-start", map[string]string{
		"host": remoteHostName, "repo_root": "/srv/app", "main_root": "/srv/app",
	}, map[string]any{"session_id": "remote-4", "cwd": "/srv/app", "source": "startup"})

	e.postFrom(t, "session-end", map[string]string{"host": remoteHostName},
		map[string]any{"session_id": "remote-4", "transcript_path": transcript, "reason": "other"})

	sess := e.session(t, "remote-4")
	require.Equal(t, core.SessionCompleted, sess.Status, "the session still ends")
	require.Empty(t, sess.Findings, "no transcript harvest for another machine")
	require.Empty(t, sess.Model, "no model sniff for another machine")
	require.True(t, sess.Tokens.Empty(), "no token harvest for another machine")
	require.Contains(t, e.skippedWhat(t), "session-end-transcript")
}

// The same SessionEnd on the daemon's own machine harvests everything, which is
// what keeps the gate from being a quiet regression for a loopback install.
func TestSessionEnd_LocalHostStillHarvests(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})
	transcript := writeTranscript(t, strings.Join([]string{
		`{"type":"assistant","isSidechain":false,"message":{"id":"m1","model":"claude-fable-5","usage":{"input_tokens":10,"output_tokens":5}}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Final summary."}]}}`,
	}, "\n"))

	e.postFrom(t, "session-start", map[string]string{"host": localHostName},
		map[string]any{"session_id": "local-4", "cwd": "/work/demo", "source": "startup"})
	e.postFrom(t, "session-end", map[string]string{"host": localHostName},
		map[string]any{"session_id": "local-4", "transcript_path": transcript, "reason": "other"})

	sess := e.session(t, "local-4")
	require.Equal(t, "(auto-harvested) Final summary.", sess.Findings)
	require.Equal(t, "claude-fable-5", sess.Model)
	require.False(t, sess.Tokens.Empty())
	require.Empty(t, e.skipEvents(t), "a local hook records no skips at all")
}

// The model sniff is the transcript half of setAmbientModel; a model the client
// STATED still lands from any machine.
func TestSetAmbientModel_SniffIsHostGatedButAStatedModelIsNot(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})
	transcript := writeModelTranscript(t,
		`{"type":"assistant","isSidechain":false,"message":{"model":"claude-fable-5"}}`)

	e.postFrom(t, "session-start", map[string]string{
		"host": remoteHostName, "repo_root": "/srv/app", "main_root": "/srv/app",
	}, map[string]any{
		"session_id": "remote-5", "cwd": "/srv/app", "source": "startup",
		"transcript_path": transcript,
	})
	require.Empty(t, e.session(t, "remote-5").Model, "no sniff off another machine's transcript")
	require.Contains(t, e.skippedWhat(t), "model-sniff")

	// Codex states its model in the payload: that needs no disk at all.
	e.postFrom(t, "session-start", map[string]string{
		"host": remoteHostName, "repo_root": "/srv/app", "main_root": "/srv/app",
	}, map[string]any{
		"session_id": "remote-6", "cwd": "/srv/app", "source": "startup", "model": "gpt-5.5",
	})
	sess, ok, err := store.AmbientSessionByExternalIdentity(
		context.Background(), e.db, ClientClaudeCode.externalIdentity(), "remote-6")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "gpt-5.5", sess.Model, "a stated model needs no filesystem")
}

// The Codex Stop harvest reads the rollout file, so it is gated the same way --
// the heartbeat is not.
func TestCodexStop_RolloutHarvestIsHostGated(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})
	rollout := writeTranscript(t,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"Did the thing."}}`)

	e.postFrom(t, "session-start", map[string]string{
		"client": "codex", "host": remoteHostName,
		"repo_root": "/srv/app", "main_root": "/srv/app",
	}, map[string]any{"session_id": "cx-1", "cwd": "/srv/app", "source": "startup"})

	e.postFrom(t, "stop", map[string]string{"host": remoteHostName, "client": "codex"}, map[string]any{
		"session_id": "cx-1", "cwd": "/srv/app", "transcript_path": rollout,
		"last_assistant_message": "",
	})
	require.Contains(t, e.skippedWhat(t), "codex-rollout")
}

// A subagent transcript is the child's file on the child's machine.
func TestSubagentStop_TranscriptCaptureIsHostGated(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{Enabled: true})
	e.postFrom(t, "session-start", nil,
		map[string]any{"session_id": testSID, "cwd": "/work/demo", "source": "startup"})

	e.postFrom(t, "subagent-stop", map[string]string{"host": remoteHostName}, map[string]any{
		"session_id": testSID, "agent_id": "sp99", "agent_type": "Explore",
		"cwd": "/work/demo", "permission_mode": "plan",
		"transcript_path": filepath.Join(t.TempDir(), "t.jsonl"),
	})
	require.Contains(t, e.skippedWhat(t), "subagent-transcript")
}

// SessionStart stamps the agent process on the ambient session, which is what
// lets the agent's MCP connection -- naming the same process -- bind to it. The
// stamp follows the session: a resume in another process moves it, and a resume
// from a client that names no process clears it, so a dead process never keeps
// a claim on a live session.
func TestSessionStart_StampsAgentProcess(t *testing.T) {
	ctx := context.Background()
	e := newHostEnv(t, config.PlanCapture{})
	owned := func(proc string) []core.Session {
		t.Helper()
		got, err := store.ActiveAmbientByAgentProcess(ctx, e.db, localHostName, proc)
		require.NoError(t, err)
		return got
	}

	out := e.postFrom(t, "session-start", map[string]string{"host": localHostName, "agent_process": "100.1"},
		map[string]any{"session_id": "proc-1", "cwd": "/work/demo", "source": "startup"})
	ac := additionalContext(t, out)
	require.Contains(t, ac, "bind to it automatically", "a named process binds without session_start")
	require.Len(t, owned("100.1"), 1)
	require.Equal(t, e.session(t, "proc-1").ID, owned("100.1")[0].ID)

	// Resumed in another process: the stamp moves with it.
	e.postFrom(t, "session-start", map[string]string{"host": localHostName, "agent_process": "200.2"},
		map[string]any{"session_id": "proc-1", "cwd": "/work/demo", "source": "resume"})
	require.Empty(t, owned("100.1"))
	require.Len(t, owned("200.2"), 1)

	// Resumed by a client that names no process: the old claim is dropped, and
	// the line falls back to the binding remedy.
	out = e.postFrom(t, "session-start", map[string]string{"host": localHostName},
		map[string]any{"session_id": "proc-1", "cwd": "/work/demo", "source": "resume"})
	require.Empty(t, owned("200.2"))
	require.Contains(t, additionalContext(t, out), "session_start name="+e.session(t, "proc-1").Name)
	require.Empty(t, e.hookErrors(t))
}
