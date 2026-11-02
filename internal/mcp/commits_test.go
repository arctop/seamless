package mcp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/commitfeed"
	"github.com/arctop/seamless/internal/core"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/store"
)

// Synthetic commit ids.
var (
	commitOne   = strings.Repeat("1", 40)
	commitTwo   = strings.Repeat("2", 64)
	commitThree = strings.Repeat("3", 40)
)

func recordingServer(t *testing.T) (string, *sql.DB) {
	return newServerCfg(t, func(c *mcpserver.Config) { c.RecordCommits = true })
}

func commitRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM commit_records`).Scan(&n))
	return n
}

func listedSchema(t *testing.T, ctx context.Context, cli *mcpclient.Client, name string) mcp.ToolInputSchema {
	t.Helper()
	listed, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)
	for _, tool := range listed.Tools {
		if tool.Name == name {
			return tool.InputSchema
		}
	}
	require.Failf(t, "tool not listed", "%s", name)
	return mcp.ToolInputSchema{}
}

func catalogTool(t *testing.T, name string) mcp.Tool {
	t.Helper()
	for _, tool := range mcpserver.Catalog() {
		if tool.Name == name {
			return tool
		}
	}
	require.Failf(t, "tool not in the catalog", "%s", name)
	return mcp.Tool{}
}

// bindAs dials a connection bound to a fresh named session in project.
func bindAs(t *testing.T, ctx context.Context, db *sql.DB, url, name, project string) (*mcpclient.Client, string) {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: id, Name: name, ProjectSlug: project, Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))
	cli := dialClient(t, ctx, url, testKey)
	start := callJSON(t, ctx, cli, "session_start", map[string]any{"name": name})
	require.Equal(t, project, start["project"])
	return cli, id
}

// Off -- the default -- the argument does not exist: neither tool lists it, a
// call that sends it is refused exactly as an unknown parameter always was,
// tasks_update's nothing-to-update text is unchanged, and no row is written.
func TestCommits_OffIsExactlyThePreviousBehavior(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	cli := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})

	for _, name := range []string{"session_end", "tasks_update"} {
		got := listedSchema(t, ctx, cli, name)
		require.NotContains(t, got.Properties, "commits", "%s must not offer commits while recording is off", name)
		full := catalogTool(t, name).InputSchema
		require.Contains(t, full.Properties, "commits", "the catalog documents it")
		delete(full.Properties, "commits")
		want, err := json.Marshal(full)
		require.NoError(t, err)
		have, err := json.Marshal(got)
		require.NoError(t, err)
		require.JSONEq(t, string(want), string(have), "%s: the listed schema is the catalog's, commits alone removed", name)
	}

	task := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "a task"})["id"].(string)
	isErr, txt := callErr(t, ctx, cli, "tasks_update", map[string]any{"id": task, "commits": []any{commitOne}})
	require.True(t, isErr)
	require.Contains(t, txt, `tasks_update: unknown parameter "commits": valid parameters are: `+
		"add_depends_on, body, content, id, project, session, session_id, status, text, title")
	isErr, txt = callErr(t, ctx, cli, "tasks_update", map[string]any{"id": task})
	require.True(t, isErr)
	require.Contains(t, txt, "nothing to update: pass status, title, body, project, or add_depends_on")
	require.NotContains(t, txt, "commits")

	isErr, txt = callErr(t, ctx, cli, "session_end", map[string]any{"findings": "done", "commits": []any{commitOne}})
	require.True(t, isErr)
	require.Contains(t, txt, `session_end: unknown parameter "commits": valid parameters are: `+
		"findings, mishaps, session, session_id, summary")
	end := callJSON(t, ctx, cli, "session_end", map[string]any{"findings": "done"})
	require.Equal(t, "completed", end["status"], "the session was not ended by the refused call, and ends now")
	require.NotContains(t, end, "commits_recorded", "the result keeps its old shape")
	require.Zero(t, commitRows(t, db))
}

// On, both tools offer the argument, read it leniently where that loses
// nothing (case, repeats, the comma-separated form) and refuse the whole call
// on an id that is not a full commit id or a list past the per-call bound.
func TestCommits_RecordedOnTheSessionAndTheTask(t *testing.T) {
	ctx := context.Background()
	url, db := recordingServer(t)
	cli := dialClient(t, ctx, url, testKey)
	start := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	sessionID := start["session_id"].(string)

	for _, name := range []string{"session_end", "tasks_update"} {
		prop, ok := listedSchema(t, ctx, cli, name).Properties["commits"].(map[string]any)
		require.True(t, ok, "%s offers commits while recording is on", name)
		require.Equal(t, "array", prop["type"])
		require.Contains(t, prop["description"], "never an abbreviation")
	}

	task := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "a task"})["id"].(string)
	out := callJSON(t, ctx, cli, "tasks_update", map[string]any{
		"id": task, "commits": strings.ToUpper(commitOne) + ", " + commitOne + "," + commitTwo,
	})
	require.EqualValues(t, 2, out["commits_recorded"], "uppercase is the same commit, repeats count once")
	require.Equal(t, "open", out["status"], "recording commits changes nothing else")
	got, err := store.RecordedCommits(ctx, db, store.CommitOwnerTask, []string{task})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{commitOne, commitTwo}, got[task])

	isErr, txt := callErr(t, ctx, cli, "tasks_update", map[string]any{"id": task})
	require.True(t, isErr)
	require.Contains(t, txt, "nothing to update: pass status, title, body, project, add_depends_on, or commits")

	isErr, txt = callErr(t, ctx, cli, "tasks_update", map[string]any{"id": task, "status": "done", "commits": []any{commitThree, "abc1234"}})
	require.True(t, isErr)
	require.Contains(t, txt, `tasks_update: invalid commits: entry 1 ("abc1234") is not a full commit id`)
	same := callJSON(t, ctx, cli, "tasks_list", map[string]any{"id": task})["tasks"].([]any)[0].(map[string]any)
	require.Equal(t, "open", same["status"], "a refused call changes nothing")

	many := make([]any, 0, store.MaxCommitsPerCall+1)
	for i := range store.MaxCommitsPerCall + 1 {
		many = append(many, fmt.Sprintf("%040x", i))
	}
	isErr, txt = callErr(t, ctx, cli, "tasks_update", map[string]any{"id": task, "commits": many})
	require.True(t, isErr)
	require.Contains(t, txt, fmt.Sprintf("%d distinct commit ids: one call records at most %d",
		store.MaxCommitsPerCall+1, store.MaxCommitsPerCall))
	require.Equal(t, 2, commitRows(t, db), "nothing more was recorded")

	isErr, txt = callErr(t, ctx, cli, "session_end", map[string]any{"findings": "done", "commits": []any{"not-a-commit"}})
	require.True(t, isErr)
	require.Contains(t, txt, "session_end: invalid commits: entry 0")
	sess, ok, err := store.SessionByID(ctx, db, sessionID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionActive, sess.Status, "a refused end leaves the session open")

	end := callJSON(t, ctx, cli, "session_end", map[string]any{"findings": "done", "commits": []any{commitThree, commitThree}})
	require.Equal(t, "completed", end["status"])
	require.EqualValues(t, 1, end["commits_recorded"])
	got, err = store.RecordedCommits(ctx, db, store.CommitOwnerSession, []string{sessionID})
	require.NoError(t, err)
	require.Equal(t, []string{commitThree}, got[sessionID])
	var payload string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT payload FROM events WHERE kind = ? AND session_id = ?`,
		string(core.EventSessionEnded), sessionID).Scan(&payload))
	require.Contains(t, payload, `"commits_recorded":1`, "the end's event counts them; the ids live in the record")
	require.NotContains(t, payload, commitThree)
}

// A task's commits need the right to update the task: a task another session
// holds refuses them, and so does a sealed project for a caller outside it,
// on session_end as much as tasks_update.
func TestCommits_NeedTheRightToUpdate(t *testing.T) {
	ctx := context.Background()
	url, db := recordingServer(t)
	holder, holderID := bindAs(t, ctx, db, url, "sess/holder", "demo")
	other, _ := bindAs(t, ctx, db, url, "sess/other", "demo")

	task := callJSON(t, ctx, holder, "tasks_add", map[string]any{"title": "held work"})["id"].(string)
	callJSON(t, ctx, holder, "tasks_claim", map[string]any{"id": task})
	isErr, txt := callErr(t, ctx, other, "tasks_update", map[string]any{"id": task, "commits": []any{commitOne}})
	require.True(t, isErr)
	require.Contains(t, txt, "already claimed")
	require.Zero(t, commitRows(t, db), "a held task refuses another session's commits")

	callJSON(t, ctx, holder, "tasks_update", map[string]any{"id": task, "status": "done", "commits": []any{commitOne}})
	var by string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT recorded_by FROM commit_records WHERE owner_id = ?`, task).Scan(&by))
	require.Equal(t, holderID, by, "the record names the session that made it")

	isolate(t, ctx, db, "vault", core.IsolationSealed)
	vault, vaultSession := bindAs(t, ctx, db, url, "sess/vault", "vault")
	vaultTask := callJSON(t, ctx, vault, "tasks_add", map[string]any{"title": "sealed work"})["id"].(string)
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"tasks_update", map[string]any{"id": vaultTask, "commits": []any{commitTwo}}},
		{"session_end", map[string]any{"findings": "x", "session_id": vaultSession, "commits": []any{commitTwo}}},
	} {
		isErr, txt := callErr(t, ctx, other, call.tool, call.args)
		require.True(t, isErr, "%s into a sealed project from outside", call.tool)
		require.Contains(t, txt, "project vault is sealed", "%s: %s", call.tool, txt)
	}
	require.Equal(t, 1, commitRows(t, db), "nothing reached the sealed project's record")
	sess, ok, err := store.SessionByID(ctx, db, vaultSession)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionActive, sess.Status)
}

// What agents record through MCP is what the snapshot carries: the session
// under its name with the commits it ended with, the task with its own.
func TestCommits_RecordedThroughMCPReachTheSnapshot(t *testing.T) {
	ctx := context.Background()
	url, db := recordingServer(t)
	cli := dialClient(t, ctx, url, testKey)
	start := callJSON(t, ctx, cli, "session_start", map[string]any{"cwd": "/work/demo"})
	name := start["name"].(string)

	task := callJSON(t, ctx, cli, "tasks_add", map[string]any{"title": "ship the change"})["id"].(string)
	callJSON(t, ctx, cli, "tasks_update", map[string]any{"id": task, "status": "done", "commits": []any{commitTwo}})
	callJSON(t, ctx, cli, "session_end", map[string]any{"findings": "shipped", "commits": []any{commitOne, commitThree}})
	// A second agent, still at work and in no repository.
	openID, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: openID, Name: "cx/0f0f0f0f-1e1e1e1e", Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))

	file := filepath.Join(t.TempDir(), "snapshot-seamless_feed.json")
	now = time.Now().UTC()
	w := commitfeed.NewWriter(commitfeed.WriterConfig{
		DB: db, File: file, Every: time.Minute, Window: time.Hour, Host: "build-host",
		Now: func() time.Time { return now },
	})
	require.NoError(t, w.WriteOnce(ctx))
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	require.NoError(t, commitfeed.CheckJSON(raw, now))
	t.Logf("snapshot: %s", strings.TrimSpace(string(raw)))

	var snap commitfeed.Snapshot
	require.NoError(t, json.Unmarshal(raw, &snap))
	require.Equal(t, "seamless_feed", snap.Kind)
	require.Equal(t, "build-host", snap.Host)
	require.Len(t, snap.Sessions, 2)
	s := snap.Sessions[0]
	require.Equal(t, name, s.ID)
	require.NotNil(t, s.Repo)
	require.Equal(t, "demo", *s.Repo, "the repository its working directory is in")
	require.NotNil(t, s.EndedAt, "ended")
	require.ElementsMatch(t, []string{commitOne, commitThree}, s.Commits)
	o := snap.Sessions[1]
	require.Equal(t, "cx/0f0f0f0f-1e1e1e1e", o.ID)
	require.Nil(t, o.Repo, "no working directory, no repository")
	require.Nil(t, o.EndedAt, "still open")
	require.Empty(t, o.Commits)
	require.Contains(t, string(raw), `"repo":null`, "repo is present and null, never left out")
	require.Contains(t, string(raw), `"ended_at":null`)
	require.Len(t, snap.Tasks, 1)
	require.Equal(t, task, snap.Tasks[0].ID)
	require.Equal(t, []string{commitTwo}, snap.Tasks[0].Commits)
	require.NotContains(t, string(raw), "ship the change", "never a title")
	require.NotContains(t, string(raw), "shipped", "never a finding")
}
