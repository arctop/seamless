//go:build unix

package hooks

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/harvest"
)

// fifo makes a named pipe at path. Opening one for reading blocks until a
// writer opens it, so a daemon that opened it would never answer the hook.
func fifo(t *testing.T, path string) string {
	t.Helper()
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	// Release a reader the daemon should never have become, so a failing test
	// does not leave the server's handler blocked in open(2) past the test.
	t.Cleanup(func() {
		if f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	})
	return path
}

// Under peer_auth the daemon never opens a path a hook names -- with a push
// or without one (an older client on the TCP key). Every path here is a FIFO
// the daemon could read if it opened it at all: the transcript, a plan file
// inside the daemon's own plans dir (so its own plan-path check would pass),
// a subagent transcript and a Codex rollout. Each hook must answer; a daemon
// that opened one would block in open(2) until the deadline below.
func TestPushedCapture_ForgedPathsAreNeverOpened(t *testing.T) {
	e := newPushHooks(t)
	dir := t.TempDir()
	transcript := fifo(t, filepath.Join(dir, "transcript.jsonl"))
	rollout := fifo(t, filepath.Join(dir, "rollout.jsonl"))
	agent := fifo(t, filepath.Join(dir, "agent-a9.jsonl"))
	planFile := fifo(t, filepath.Join(e.plansDir, "fifo-plan.md"))
	model := map[string]any{"version": 1, "model": "claude-pushed"}
	plan := map[string]any{"version": 1, "plan": map[string]any{"basename": "fifo-plan", "content": "# Fifo plan\n\nPushed."}}

	type call struct {
		endpoint, client string
		body, push       map[string]any
	}
	calls := []call{
		{"session-start", "", map[string]any{"session_id": "fifo-1", "cwd": "/work/ops", "source": "startup", "transcript_path": transcript}, model},
		{"user-prompt-submit", "", map[string]any{"session_id": "fifo-1", "cwd": "/work/ops", "user_prompt": "deploy race", "transcript_path": transcript}, model},
		{"subagent-start", "", map[string]any{"session_id": "fifo-1", "cwd": "/work/ops", "agent_id": "a9", "agent_type": "Explore", "agent_transcript_path": agent, "transcript_path": transcript}, map[string]any{"version": 1, "prompt": "p"}},
		{"post-tool-use", "", map[string]any{"session_id": "fifo-1", "cwd": "/work/ops", "tool_name": "Write", "tool_input": map[string]any{"file_path": planFile}, "transcript_path": transcript}, plan},
		{"permission-request", "", map[string]any{"session_id": "fifo-1", "cwd": "/work/ops", "tool_name": "ExitPlanMode", "transcript_path": transcript}, map[string]any{"version": 1}},
		{"post-tool-use", "", map[string]any{"session_id": "fifo-1", "cwd": "/work/ops", "tool_name": "ExitPlanMode", "tool_response": map[string]any{"filePath": planFile}}, plan},
		{"subagent-stop", "", map[string]any{"session_id": "fifo-1", "cwd": "/work/ops", "agent_id": "a9", "agent_type": "Explore", "permission_mode": "plan", "agent_transcript_path": agent, "transcript_path": transcript}, map[string]any{"version": 1, "prompt": "p", "report": "r"}},
		{"session-end", "", map[string]any{"session_id": "fifo-1", "transcript_path": transcript}, map[string]any{"version": 1, "findings": "(auto-harvested) pushed"}},
		{"session-start", "codex", map[string]any{"session_id": "fifo-cx", "cwd": "/work/ops", "source": "startup", "transcript_path": rollout}, nil},
		{"subagent-start", "codex", map[string]any{"session_id": "fifo-cx", "cwd": "/work/ops", "agent_id": "a8", "agent_type": "worker", "transcript_path": rollout}, nil},
		{"stop", "codex", map[string]any{"session_id": "fifo-cx", "cwd": "/work/ops", "transcript_path": rollout}, map[string]any{"version": 1, "findings": "(auto-harvested) pushed"}},
	}
	for _, withPush := range []bool{true, false} {
		for _, c := range calls {
			push := c.push
			if !withPush {
				push = nil
			}
			done := make(chan int, 1)
			go func() { done <- rawHook(e, c.endpoint, c.client, c.body, push) }()
			select {
			case status := <-done:
				require.Equal(t, http.StatusOK, status, "%s (%s, push=%v)", c.endpoint, c.client, withPush)
			case <-time.After(5 * time.Second):
				t.Fatalf("%s (%s, push=%v) never answered: the daemon opened a path the hook named", c.endpoint, c.client, withPush)
			}
		}
	}
	// The pushes landed; the daemon read none of the pipes.
	require.Equal(t, "(auto-harvested) pushed", requireAmbientSession(t, e.db, ClientCodex, "fifo-cx").Findings)
	n, ok := e.note(lkOps, "cc-plan-fifo-plan")
	require.True(t, ok)
	require.Contains(t, n.Body, "Pushed.")
}

// rawHook posts one hook off the test goroutine (no require there) and
// returns its status, or 0 when the request failed.
func rawHook(e *pushHooks, endpoint, client string, body, push map[string]any) int {
	q := url.Values{"host": {localHostName}, "agent_process": {lkAgent.AgentID()}}
	if client != "" {
		q.Set("client", client)
	}
	all := map[string]any{}
	for k, v := range body {
		all[k] = v
	}
	if push != nil {
		all[harvest.Field] = push
	}
	raw, err := json.Marshal(all)
	if err != nil {
		return 0
	}
	req, err := http.NewRequest(http.MethodPost, e.url+"/api/hooks/"+endpoint+"?"+q.Encode(), bytes.NewReader(raw))
	if err != nil {
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // draining the reply; the status is the result
	return resp.StatusCode
}
