package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/store"
)

// ledgerPromptPayload is a Claude Code prompt as Claude Code names its
// fields (the prompt under "prompt").
func ledgerPromptPayload(f captureFixture, id string) map[string]any {
	return map[string]any{"session_id": id, "cwd": f.cwd, "transcript_path": f.transcript, "permission_mode": "default",
		"hook_event_name": "UserPromptSubmit", "prompt": "is the health check race fixed?"}
}

// ledgerLedger is the ledger of conversation id's ambient row.
func (d *socketDaemon) ledgerLedger(t *testing.T, id string) []core.UsageBucket {
	t.Helper()
	rows, err := store.SessionUsage(context.Background(), d.db, d.session(t, "claude-code", id).ID)
	require.NoError(t, err)
	out := make([]core.UsageBucket, len(rows))
	for i, r := range rows {
		out[i] = r.UsageBucket
	}
	return out
}

// A live Claude Code session's usage over a real peer_auth socket, end to
// end. With the Diary off the prompt's answer asks nothing, and nothing
// starts. On, the answer asks; `seam hook` hands the read to a detached
// `seam hook-usage` (run here in-process), which pushes the transcript's
// rows, its subagents' included; and the daemon's ledger holds them while
// the session is still live. The daemon's throttle then holds the next
// prompt.
func TestRunHook_KeepsALiveSessionsUsageOverTheSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	f := newCaptureFixture(t)
	ctx := context.Background()
	d := newSocketDaemon(t, f, ownerSocketView(t))
	var jobs [][]string
	spawn := inProcess(t, d.cfg, &jobs)
	start := map[string]any{"session_id": "cc-live", "cwd": f.cwd, "source": "startup", "transcript_path": f.transcript}
	require.Empty(t, runHookSpawning(t, d.cfg, hookCall{event: "session-start", payload: start}, spawn))
	prompt := hookCall{event: "user-prompt-submit", payload: ledgerPromptPayload(f, "cc-live")}

	require.Empty(t, runHookSpawning(t, d.cfg, prompt, spawn))
	require.Empty(t, jobs, "the Diary off: nothing asked, nothing started")
	require.Zero(t, d.pushes.Load())
	require.Empty(t, d.ledgerLedger(t, "cc-live"))

	require.NoError(t, store.SetFeaturesConfig(ctx, d.db, config.Features{Diary: true}))
	require.Empty(t, runHookSpawning(t, d.cfg, prompt, spawn))
	require.Equal(t, [][]string{{"hook-usage", "--session-id", "cc-live", "--path", f.transcript}}, jobs)
	require.Equal(t, int32(1), d.pushes.Load())
	totals, rows, ok := harvest.ClaudeUsage(f.transcript)
	require.True(t, ok)
	require.True(t, slices.ContainsFunc(rows, func(r core.UsageBucket) bool { return r.Thread == core.UsageThreadSubagent }),
		"the fixture's subagents worked")
	require.Equal(t, rows, d.ledgerLedger(t, "cc-live"))
	sess := d.session(t, "claude-code", "cc-live")
	require.Equal(t, core.SessionActive, sess.Status, "still live")
	require.Equal(t, totals, sess.Tokens)

	require.Empty(t, runHookSpawning(t, d.cfg, prompt, spawn))
	require.Len(t, jobs, 1, "within the daemon's interval the next prompt asks nothing")
	require.Equal(t, int32(1), d.pushes.Load())
}

// The hook returns before the usage is read: it starts `seam hook-usage` as
// a detached process of its own (here a fake that only records), with the
// hook's --config, and reads and sends nothing itself. A spawner that fails
// costs the usage, never the hook.
func TestRunHook_StartsTheUsageReadDetached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	f := newCaptureFixture(t)
	ctx := context.Background()
	d := newSocketDaemon(t, f, ownerSocketView(t))
	require.NoError(t, store.SetFeaturesConfig(ctx, d.db, config.Features{Diary: true}))
	prompt := func(id string, spawn func([]string) error) (stdout, stderr string) {
		t.Helper()
		require.Empty(t, runHookCall(t, d.cfg, hookCall{event: "session-start",
			payload: map[string]any{"session_id": id, "cwd": f.cwd, "source": "startup"}}))
		raw, err := json.Marshal(ledgerPromptPayload(f, id))
		require.NoError(t, err)
		e, out, errb := stubEnv()
		e.stdin = bytes.NewReader(raw)
		e.loadConfig = func() (config.Config, error) { return d.cfg, nil }
		require.NoError(t, runHook(ctx, e, &hookOpts{config: "/etc/seamless/seamless.yaml", spawn: spawn}, []string{"user-prompt-submit"}))
		return out.String(), errb.String()
	}
	var started [][]string
	out, errOut := prompt("cc-detach", func(args []string) error {
		started = append(started, args)
		return nil
	})
	require.Empty(t, errOut)
	require.NotEmpty(t, out, "the reply went out")
	require.Equal(t, [][]string{{"hook-usage", "--config", "/etc/seamless/seamless.yaml",
		"--session-id", "cc-detach", "--path", f.transcript}}, started)
	require.Zero(t, d.pushes.Load(), "the hook itself sends nothing")

	out, errOut = prompt("cc-detach-2", func([]string) error { return os.ErrPermission })
	require.NotEmpty(t, out)
	require.Contains(t, errOut, "seam hook: usage not sent: ")
}

// usageJobs answers only a Claude Code prompt's ask, and only with a session
// and a transcript to read.
func TestUsageJobs(t *testing.T) {
	payload := []byte(`{"session_id":"cc-a","transcript_path":"/t/a.jsonl","prompt":"go"}`)
	for _, tc := range []struct {
		name    string
		o       hookOpts
		event   string
		payload []byte
		wanted  string
		want    [][]string
	}{
		{"nothing asked", hookOpts{}, "user-prompt-submit", payload, "", nil},
		{"asked", hookOpts{}, "user-prompt-submit", payload, harvest.UsageWanted,
			[][]string{{"hook-usage", "--session-id", "cc-a", "--path", "/t/a.jsonl"}}},
		{"with the hook's flags", hookOpts{config: "/c.yaml", client: hookClientClaudeCode}, "user-prompt-submit", payload, harvest.UsageWanted,
			[][]string{{"hook-usage", "--config", "/c.yaml", "--client", "claude-code", "--session-id", "cc-a", "--path", "/t/a.jsonl"}}},
		{"a value this release does not know", hookOpts{}, "user-prompt-submit", payload, "2", nil},
		{"another event", hookOpts{}, "session-start", payload, harvest.UsageWanted, nil},
		{"a Codex prompt", hookOpts{client: hookClientCodex}, "user-prompt-submit", payload, harvest.UsageWanted, nil},
		{"no transcript named", hookOpts{}, "user-prompt-submit", []byte(`{"session_id":"cc-a"}`), harvest.UsageWanted, nil},
		{"no session named", hookOpts{}, "user-prompt-submit", []byte(`{"transcript_path":"/t/a.jsonl"}`), harvest.UsageWanted, nil},
		{"not a JSON object", hookOpts{}, "user-prompt-submit", []byte(`[1]`), harvest.UsageWanted, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, usageJobs(&tc.o, tc.event, tc.payload, tc.wanted))
		})
	}
}

// Off the socket the client reads and sends no usage, whatever an answer
// says, and starts nothing; the detached command itself sends nothing off
// the socket, for a Codex session, or for a transcript with no usage in it.
func TestRunHook_SendsNoUsageOffTheSocket(t *testing.T) {
	f := newCaptureFixture(t)
	pushes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == harvest.UsageEndpoint {
			pushes++
		}
		w.Header().Set(harvest.UsageWantedHeader, harvest.UsageWanted)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"continue":true,"suppressOutput":true}`)
	}))
	t.Cleanup(srv.Close)
	cfg := tcpConfig(strings.TrimPrefix(srv.URL, "http://"))
	started := 0
	spawn := func([]string) error { started++; return nil }
	require.Empty(t, runHookSpawning(t, cfg, hookCall{event: "user-prompt-submit", payload: ledgerPromptPayload(f, "cc-x")}, spawn))
	require.Zero(t, started)

	run := func(cfg config.Config, o *hookUsageOpts) {
		t.Helper()
		e, _, _ := stubEnv()
		e.loadConfig = func() (config.Config, error) { return cfg, nil }
		require.NoError(t, runHookUsage(context.Background(), e, o, nil))
	}
	run(cfg, &hookUsageOpts{sessionID: "cc-x", path: f.transcript})
	require.Zero(t, pushes, "off the socket")

	if runtime.GOOS == "windows" {
		return // the peer_auth socket is a Unix deployment
	}
	d := newSocketDaemon(t, f, ownerSocketView(t))
	run(d.cfg, &hookUsageOpts{client: hookClientCodex, sessionID: "cx-x", path: f.rollout})
	empty := filepath.Join(t.TempDir(), "empty.jsonl")
	require.NoError(t, os.WriteFile(empty, []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0o600))
	run(d.cfg, &hookUsageOpts{sessionID: "cc-x", path: empty})
	run(d.cfg, &hookUsageOpts{sessionID: "cc-x", path: filepath.Join(t.TempDir(), "gone.jsonl")})
	require.Zero(t, d.pushes.Load(), "a Codex session, or a transcript with no usage yet: nothing sent")
}
