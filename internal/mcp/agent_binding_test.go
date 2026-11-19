package mcp_test

// The process binding: a connection that names the agent process (as `seam
// mcp-proxy` and `seam mcp-headers` do) is bound to the ambient session that
// process's SessionStart hook created -- no session_start, no inference -- and
// session_start itself only ever adopts the caller's OWN session.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/mcp/mcpversion"
	"github.com/arctop/seamless/internal/store"
)

// dialAgent dials the way a Claude Code or Codex agent's connection does: with
// the agent process identity its MCP bridge sends. An empty proc is a client
// that names no process.
func dialAgent(t *testing.T, ctx context.Context, url, proc string) *mcpclient.Client {
	t.Helper()
	return dialAgentAt(t, ctx, url, proc, mcpversion.Client)
}

// dialAgentAt is dialAgent asking for the given protocol version, and requires
// the connection to settle on it.
func dialAgentAt(t *testing.T, ctx context.Context, url, proc, version string) *mcpclient.Client {
	t.Helper()
	headers := map[string]string{"Authorization": "Bearer " + testKey}
	if proc != "" {
		headers[mcpserver.AgentProcessHeader] = proc
	}
	cli, err := mcpclient.NewStreamableHttpClient(url, transport.WithHTTPHeaders(headers))
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	require.NoError(t, cli.Start(ctx))
	var initReq mcp.InitializeRequest
	initReq.Params.ProtocolVersion = version
	initReq.Params.ClientInfo = mcp.Implementation{Name: "agent-test", Version: "0"}
	_, err = cli.Initialize(ctx, initReq)
	require.NoError(t, err)
	require.Equal(t, version, cli.ProtocolVersion())
	return cli
}

// seedAgentAmbient is the ambient session the SessionStart hook leaves for an
// agent that named its process.
func seedAgentAmbient(t *testing.T, ctx context.Context, db *sql.DB, name, project, cwd, proc string) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive, Ambient: true,
		ExternalSessionID: name + "-full", ExternalClient: "claude-code", CWD: cwd,
		CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.SetSessionAgentProcess(ctx, db, id, proc))
	return id
}

func writeMemory(t *testing.T, ctx context.Context, cli *mcpclient.Client, name string, extra map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{"name": name, "kind": "gotcha", "description": "d", "body": "b\n"}
	for k, v := range extra {
		args[k] = v
	}
	return callJSON(t, ctx, cli, "memory_write", args)
}

// lastToolCall returns the session the most recent tool.call event was
// attributed to.
func lastToolCall(t *testing.T, db *sql.DB, tool string) string {
	t.Helper()
	var sessionID string
	require.NoError(t, db.QueryRow(`SELECT session_id FROM events
		WHERE kind = 'tool.call' AND json_extract(payload, '$.tool') = ?
		ORDER BY ts DESC, id DESC LIMIT 1`, tool).Scan(&sessionID))
	return sessionID
}

// The headline: two agents in two projects, neither ever calling session_start.
// Each one's unscoped calls land in its own project and are attributed to its own
// session; a client naming no process is exactly as ambiguous as before, and a
// process that owns no session is not guessed into one.
func TestProcessBinding_ScopesEachAgentWithoutSessionStart(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	alpha := seedAgentAmbient(t, ctx, db, "cc/alpha", "demo", "/work/demo", "100.1")
	beta := seedAgentAmbient(t, ctx, db, "cc/beta", "other", "/work/other", "200.2")

	a := dialAgent(t, ctx, url, "100.1")
	require.Equal(t, "demo", writeMemory(t, ctx, a, "alpha-note", nil)["project"])
	require.Equal(t, alpha, lastToolCall(t, db, "memory_write"), "attributed to the agent's own session")
	callJSON(t, ctx, a, "recall", map[string]any{"query": "alpha"})

	b := dialAgent(t, ctx, url, "200.2")
	require.Equal(t, "other", writeMemory(t, ctx, b, "beta-note", nil)["project"])
	require.Equal(t, beta, lastToolCall(t, db, "memory_write"))

	// A tool call is a heartbeat for the session it is bound to.
	sess, ok, err := store.SessionByID(ctx, db, beta)
	require.NoError(t, err)
	require.True(t, ok)
	require.WithinDuration(t, time.Now(), sess.UpdatedAt, 30*time.Second)

	for _, proc := range []string{"", "999.9"} {
		isErr, txt := callErr(t, ctx, dialAgent(t, ctx, url, proc), "memory_write",
			map[string]any{"name": "stray", "kind": "gotcha", "description": "d", "body": "b"})
		require.True(t, isErr, "proc %q: two live projects with no binding stay ambiguous", proc)
		require.Contains(t, txt, "ambiguous scope")
		require.Contains(t, txt, "session_start", "the refusal names the binding remedy")
	}
}

// The binding lives in the session row and on every request, not in the
// connection: a brand-new connection from the same process -- a client
// reconnect, or the first call after a daemon restart -- is bound already.
func TestProcessBinding_NeedsNoConnectionState(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	seedAgentAmbient(t, ctx, db, "cc/alpha", "demo", "/work/demo", "100.1")
	seedAgentAmbient(t, ctx, db, "cc/beta", "other", "/work/other", "200.2")

	for i := 0; i < 3; i++ {
		require.Equal(t, "demo", writeMemory(t, ctx, dialAgent(t, ctx, url, "100.1"), "again", nil)["project"])
	}
}

// One process stamped on two live sessions is a host running several sessions
// at once: a call from it cannot be pinned to either, so it is not bound, and
// resolution falls back to exactly what it was before identities existed.
func TestProcessBinding_SharedProcessIsNotABinding(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	seedAgentAmbient(t, ctx, db, "cc/one", "demo", "/work/demo", "100.1")
	seedAgentAmbient(t, ctx, db, "cc/two", "other", "/work/other", "100.1")

	isErr, txt := callErr(t, ctx, dialAgent(t, ctx, url, "100.1"), "memory_write",
		map[string]any{"name": "x", "kind": "gotcha", "description": "d", "body": "b"})
	require.True(t, isErr)
	require.Contains(t, txt, "ambiguous scope")
}

// The process binding is a binding, so the isolation fence judges the caller by
// its own session's project: a confidential project's own agent reads it with no
// ritual and cannot write out of it, while another agent is refused -- and told
// the deliberate way in.
func TestProcessBinding_IsolationJudgesTheAgentsOwnProject(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	isolate(t, ctx, db, "secret", core.IsolationConfidential)
	seedAgentAmbient(t, ctx, db, "cc/inside", "secret", "/work/secret", "100.1")
	seedAgentAmbient(t, ctx, db, "cc/outside", "demo", "/work/demo", "200.2")

	inside := dialAgent(t, ctx, url, "100.1")
	require.Equal(t, "secret", writeMemory(t, ctx, inside, "vault-layout", nil)["project"])
	read := callJSON(t, ctx, inside, "memory_read", map[string]any{"name": "vault-layout"})
	require.Equal(t, "secret", read["project"])

	isErr, txt := callErr(t, ctx, inside, "memory_write", map[string]any{
		"name": "leak", "kind": "gotcha", "description": "d", "body": "b", "project": "global",
	})
	require.True(t, isErr, "a confidential project's own session cannot write out of it")
	require.Contains(t, txt, "writes outside it are disabled")

	isErr, txt = callErr(t, ctx, dialAgent(t, ctx, url, "200.2"), "memory_read",
		map[string]any{"name": "vault-layout", "project": "secret"})
	require.True(t, isErr)
	require.Contains(t, txt, "project secret is confidential: reads require a session bound to it")
	require.Contains(t, txt, "session_start project=secret")
}

// session_start from an agent with a process identity adopts THAT agent's
// session even when several agents share the repository -- the case that used
// to mint a duplicate sess/* row -- says the call was unnecessary, and binds
// session_end without any session argument.
func TestSessionStart_AdoptsOwnSessionAmongSameRepoPeers(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	mine := seedAgentAmbient(t, ctx, db, "cc/mine", "demo", "/work/demo", "100.1")
	seedAgentAmbient(t, ctx, db, "cc/peer", "demo", "/work/demo", "200.2")

	cli := dialAgent(t, ctx, url, "100.1")
	writeMemory(t, ctx, cli, "something-to-brief", nil)
	for _, args := range []map[string]any{{"cwd": "/work/demo"}, {}, {"project": "demo"}} {
		start := callJSON(t, ctx, cli, "session_start", args)
		require.Equal(t, mine, start["session_id"], "args %v", args)
		require.Equal(t, "demo", start["project"], "a cwd-less start is the agent's project, not global")
		require.Contains(t, start["scope"], "your own session")
		require.Contains(t, start["briefing"], "Seam project: demo")
	}

	all, err := store.ListSessions(ctx, db, "", time.Time{}, 0)
	require.NoError(t, err)
	require.Len(t, all, 2, "no duplicate session")

	end := callJSON(t, ctx, cli, "session_end", map[string]any{"findings": "done"})
	require.Equal(t, mine, end["session_id"], "the agent's own session, not an ambiguity")
}

// An agent that names its process but owns no live session (its hook has not run,
// or it ended its session) must not be handed a session just because it is the
// only one in that directory: that one belongs to another agent.
func TestSessionStart_NeverAdoptsAnotherAgentsSession(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	other := seedAgentAmbient(t, ctx, db, "cc/other", "demo", "/work/demo", "200.2")

	start := callJSON(t, ctx, dialAgent(t, ctx, url, "100.1"), "session_start", map[string]any{"cwd": "/work/demo"})
	require.NotEqual(t, other, start["session_id"])
	sess, ok, err := store.SessionByID(ctx, db, start["session_id"].(string))
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, sess.Ambient)
	require.Empty(t, sess.ExternalSessionID, "not linked to another agent's session either")
}

// Moving to another project starts a separate session there, linked to the
// agent's own so its SessionEnd closes both, and binds the connection to it; an
// argument-less session_start hands the connection back to the agent's own.
func TestSessionStart_AnotherProjectAndBack(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	mine := seedAgentAmbient(t, ctx, db, "cc/mine", "demo", "/work/demo", "100.1")
	seedAgentAmbient(t, ctx, db, "cc/peer", "elsewhere", "/work/elsewhere", "300.3")
	cli := dialAgent(t, ctx, url, "100.1")

	moved := callJSON(t, ctx, cli, "session_start", map[string]any{"project": "other"})
	require.NotEqual(t, mine, moved["session_id"])
	require.Equal(t, "other", moved["project"])
	require.Contains(t, moved["warning"], "did not exist and was created", "a new slug is said out loud")
	sess, ok, err := store.SessionByID(ctx, db, moved["session_id"].(string))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "cc/mine-full", sess.ExternalSessionID, "linked to the agent's own session")
	require.Equal(t, "other", writeMemory(t, ctx, cli, "moved-note", nil)["project"])

	back := callJSON(t, ctx, cli, "session_start", nil)
	require.Equal(t, mine, back["session_id"])
	require.Equal(t, "demo", writeMemory(t, ctx, cli, "home-note", nil)["project"])
}

// The project argument names the scope outright -- the only way for a client
// with no meaningful cwd -- and is checked against cwd rather than silently
// preferred to it.
func TestSessionStart_ProjectArgument(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)

	// A session in the repository registers its project, as a real install's
	// first session there does; it also gives the briefing something to show.
	seed := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, seed, "session_start", map[string]any{"cwd": "/work/demo"})
	writeMemory(t, ctx, seed, "something-to-brief", nil)

	cli := dialClient(t, ctx, url, testKey)
	start := callJSON(t, ctx, cli, "session_start", map[string]any{"project": "demo"})
	require.Equal(t, "demo", start["project"])
	require.Contains(t, start["scope"], "as named")
	require.Nil(t, start["warning"], "demo already exists: nothing to warn about")
	require.Contains(t, start["briefing"], "Seam project: demo", "briefed for the named project, not the empty cwd")
	require.Equal(t, "demo", writeMemory(t, ctx, cli, "named-scope", nil)["project"])

	agree := callJSON(t, ctx, dialClient(t, ctx, url, testKey), "session_start",
		map[string]any{"project": "demo", "cwd": "/work/demo"})
	require.Equal(t, "demo", agree["project"])

	isErr, txt := callErr(t, ctx, dialClient(t, ctx, url, testKey), "session_start",
		map[string]any{"project": "other", "cwd": "/work/demo"})
	require.True(t, isErr)
	require.Contains(t, txt, `project "other" conflicts with cwd "/work/demo", which is in project "demo"`)

	global := callJSON(t, ctx, dialClient(t, ctx, url, testKey), "session_start", map[string]any{"project": "global"})
	require.Equal(t, "", global["project"])
	require.Contains(t, global["scope"], "global scope, as named")

	isErr, _ = callErr(t, ctx, dialClient(t, ctx, url, testKey), "session_start", map[string]any{"project": "../notes"})
	require.True(t, isErr, "a project argument is validated like every other")
}

// Resuming a named session with no cwd briefs that session's project -- the
// briefing used to follow the (empty) cwd and come back global.
func TestSessionStart_ResumeBriefsTheSessionsProject(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	firstCli := dialClient(t, ctx, url, testKey)
	first := callJSON(t, ctx, firstCli, "session_start",
		map[string]any{"name": "sess/long-job", "cwd": "/work/demo"})
	require.Equal(t, "demo", first["project"])
	writeMemory(t, ctx, firstCli, "something-to-brief", nil)

	again := callJSON(t, ctx, dialClient(t, ctx, url, testKey), "session_start", map[string]any{"name": "sess/long-job"})
	require.Equal(t, first["session_id"], again["session_id"])
	require.Equal(t, "demo", again["project"])
	require.Contains(t, again["briefing"], "Seam project: demo")
	require.Contains(t, again["briefing"], "resumed session", "a resume carries the re-ground hint")
}

// session= takes a name or the ULID session_start returned -- passing the id was
// the common slip, and it used to be read as a name and refused as "no active
// session: call session_start first". A miss says what the argument takes.
// session_end also takes summary for findings.
func TestSessionRefs_AcceptTheULIDAndExplainMisses(t *testing.T) {
	ctx := context.Background()
	url, _ := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	id := callJSON(t, ctx, cli, "session_start", map[string]any{"name": "sess/by-id", "cwd": "/work/demo"})["session_id"].(string)

	upd := callJSON(t, ctx, dialClient(t, ctx, url, testKey), "session_update",
		map[string]any{"findings": "halfway", "session": id})
	require.Equal(t, id, upd["session_id"])

	task := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "claim me"})
	claimed := callJSON(t, ctx, dialClient(t, ctx, url, testKey), "tasks_claim",
		map[string]any{"id": task["id"], "session": id})
	require.Equal(t, id, claimed["claimed_by"], "the actor takes a ULID in session= too")

	isErr, txt := callErr(t, ctx, cli, "session_update", map[string]any{"findings": "x", "session": "sess/nope"})
	require.True(t, isErr)
	require.Contains(t, txt, `session "sess/nope" not found: session= takes a session name`)

	isErr, txt = callErr(t, ctx, cli, "session_end",
		map[string]any{"findings": "x", "session_id": "01JZZZZZZZZZZZZZZZZZZZZZZZ"})
	require.True(t, isErr)
	require.Contains(t, txt, `session_id "01JZZZZZZZZZZZZZZZZZZZZZZZ" not found`)

	end := callJSON(t, ctx, dialClient(t, ctx, url, testKey), "session_end",
		map[string]any{"summary": "finished", "session": id})
	require.Equal(t, "completed", end["status"])
}

// An agent back from a pause longer than the idle TTL finds its session reaped.
// Its next call -- from the same live process -- is proof of life: the session
// comes back and the call is bound, instead of the agent being dropped into the
// ambiguity this binding exists to remove. A session the agent ENDED stays ended.
func TestProcessBinding_RevivesWhatTheReaperExpired(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	mine := seedAgentAmbient(t, ctx, db, "cc/mine", "demo", "/work/demo", "100.1")
	seedAgentAmbient(t, ctx, db, "cc/peer", "other", "/work/other", "200.2")
	_, err := store.ExpireStaleSessions(ctx, db, time.Now().UTC().Add(time.Hour))
	require.NoError(t, err)

	cli := dialAgent(t, ctx, url, "100.1")
	require.Equal(t, "demo", writeMemory(t, ctx, cli, "after-the-break", nil)["project"])
	sess, ok, err := store.SessionByID(ctx, db, mine)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionActive, sess.Status)
	var revived int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = 'session.started'
		AND session_id = ? AND json_extract(payload, '$.revived') = 1`, mine).Scan(&revived))
	require.Equal(t, 1, revived, "the revival is on the record")

	// The peer comes back when it calls. Then this agent ends its own session,
	// which stays ended -- and with it gone the peer's session is the only live
	// ambient, the one the sole-ambient inference would pick. An identified caller
	// is never inferred into another agent's session: not for a write, and above
	// all not for a session_end, which would complete the peer's work.
	peer := dialAgent(t, ctx, url, "200.2")
	writeMemory(t, ctx, peer, "peer-back", nil)
	callJSON(t, ctx, cli, "session_end", map[string]any{"findings": "done for today"})
	isErr, txt := callErr(t, ctx, cli, "memory_write",
		map[string]any{"name": "after-end", "kind": "gotcha", "description": "d", "body": "b"})
	require.True(t, isErr, "an ended session is not reopened, and the peer's is not borrowed")
	require.Contains(t, txt, "ambiguous scope")
	isErr, _ = callErr(t, ctx, cli, "session_end", map[string]any{"findings": "again"})
	require.True(t, isErr, "a bare session_end must not complete another agent's session")
	require.Equal(t, "other", writeMemory(t, ctx, peer, "peer-still-live", nil)["project"])
}

// A caller that names a process owning no session -- `seam` run by hand in a
// terminal, an agent whose session already ended -- is not guessed into another
// agent's session, and is not quietly answered from the global scope either while
// other agents are active: that would hide the project it meant. It is told to
// name one. With no agent active at all, global is the honest answer.
func TestProcessBinding_OtherAgentsSessionsAreNotACallersScope(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	stranger := dialAgent(t, ctx, url, "999.9")

	_, err := callErrOK(t, ctx, stranger, "recall", map[string]any{"query": "anything"})
	require.NoError(t, err, "nothing active: a global read is the honest answer")

	seedAgentAmbient(t, ctx, db, "cc/busy", "demo", "/work/demo", "100.1")
	isErr, txt := callErr(t, ctx, stranger, "recall", map[string]any{"query": "anything"})
	require.True(t, isErr, "another agent's live session is not this caller's scope")
	require.Contains(t, txt, "the active ones belong to other agents")
	require.Contains(t, txt, "project=<slug>")

	got := callJSON(t, ctx, stranger, "recall", map[string]any{"query": "anything", "project": "demo"})
	require.NotNil(t, got, "naming the project is the way through")
}

// callErrOK calls a tool and returns its text, or an error carrying the text when
// the tool reported one.
func callErrOK(t *testing.T, ctx context.Context, cli *mcpclient.Client, name string, args map[string]any) (string, error) {
	t.Helper()
	isErr, txt := callErr(t, ctx, cli, name, args)
	if isErr {
		return txt, errors.New(txt)
	}
	return txt, nil
}
