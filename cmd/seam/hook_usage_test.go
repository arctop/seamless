package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

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
// and a transcript to read; and each owed value, at any Claude Code event,
// read only inside the client's own folder (--owed).
func TestUsageJobs(t *testing.T) {
	payload := []byte(`{"session_id":"cc-a","transcript_path":"/t/a.jsonl","prompt":"go"}`)
	owed := func(id, path string) string {
		return url.Values{harvest.TranscriptSessionParam: {id}, harvest.TranscriptPathParam: {path}}.Encode()
	}
	for _, tc := range []struct {
		name    string
		o       hookOpts
		event   string
		payload []byte
		wanted  string
		owed    []string
		want    [][]string
	}{
		{"nothing asked", hookOpts{}, "user-prompt-submit", payload, "", nil, nil},
		{"asked", hookOpts{}, "user-prompt-submit", payload, harvest.UsageWanted, nil,
			[][]string{{"hook-usage", "--session-id", "cc-a", "--path", "/t/a.jsonl"}}},
		{"with the hook's flags", hookOpts{config: "/c.yaml", client: hookClientClaudeCode}, "user-prompt-submit", payload, harvest.UsageWanted, nil,
			[][]string{{"hook-usage", "--config", "/c.yaml", "--client", "claude-code", "--session-id", "cc-a", "--path", "/t/a.jsonl"}}},
		{"a value this release does not know", hookOpts{}, "user-prompt-submit", payload, "2", nil, nil},
		{"another event", hookOpts{}, "session-start", payload, harvest.UsageWanted, nil, nil},
		{"a Codex prompt", hookOpts{client: hookClientCodex}, "user-prompt-submit", payload, harvest.UsageWanted, nil, nil},
		{"no transcript named", hookOpts{}, "user-prompt-submit", []byte(`{"session_id":"cc-a"}`), harvest.UsageWanted, nil, nil},
		{"no session named", hookOpts{}, "user-prompt-submit", []byte(`{"transcript_path":"/t/a.jsonl"}`), harvest.UsageWanted, nil, nil},
		{"not a JSON object", hookOpts{}, "user-prompt-submit", []byte(`[1]`), harvest.UsageWanted, nil, nil},
		{"owed, at a start", hookOpts{config: "/c.yaml"}, "session-start", payload, "", []string{owed("cc-old", "/p/old.jsonl")},
			[][]string{{"hook-usage", "--config", "/c.yaml", "--owed", "--session-id", "cc-old", "--path", "/p/old.jsonl"}}},
		{"a prompt's ask beside the owed", hookOpts{}, "user-prompt-submit", payload, harvest.UsageWanted,
			[]string{owed("cc-old", "/p/old.jsonl")},
			[][]string{
				{"hook-usage", "--session-id", "cc-a", "--path", "/t/a.jsonl"},
				{"hook-usage", "--owed", "--session-id", "cc-old", "--path", "/p/old.jsonl"},
			}},
		{"at most a few owed an answer", hookOpts{}, "session-end", payload, "",
			[]string{owed("1", "/p/1.jsonl"), owed("2", "/p/2.jsonl"), owed("3", "/p/3.jsonl"), owed("4", "/p/4.jsonl")},
			[][]string{
				{"hook-usage", "--owed", "--session-id", "1", "--path", "/p/1.jsonl"},
				{"hook-usage", "--owed", "--session-id", "2", "--path", "/p/2.jsonl"},
				{"hook-usage", "--owed", "--session-id", "3", "--path", "/p/3.jsonl"},
			}},
		{"owed values that do not hold", hookOpts{}, "session-start", payload, "",
			[]string{"%zz", owed("", "/p/x.jsonl"), owed("cc-x", "")}, nil},
		{"owed, to a Codex hook", hookOpts{client: hookClientCodex}, "session-start", payload, "",
			[]string{owed("cc-old", "/p/old.jsonl")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, usageJobs(&tc.o, tc.event, tc.payload, tc.wanted, tc.owed))
		})
	}
}

// A conversation the agent ended with session_end, whose SessionEnd never
// came, over a real peer_auth socket: the same caller's next start asks for
// its usage, the detached `seam hook-usage --owed` (run here in-process)
// reads the recorded path inside the client's own folder, and the ledger
// holds its rows -- the Diary off. A path outside that folder is reported,
// never read, and the conversation is settled.
func TestRunHook_SendsOwedUsageAtTheNextStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	f := newCaptureFixture(t)
	ctx := context.Background()
	d := newSocketDaemon(t, f, ownerSocketView(t))
	var jobs [][]string
	spawn := inProcess(t, d.cfg, &jobs)
	proj := filepath.Join(f.home, ".claude", "projects", "-work-ops")
	require.NoError(t, os.MkdirAll(proj, 0o700))
	ended := filepath.Join(proj, "ended.jsonl")
	raw, err := os.ReadFile(f.transcript)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ended, raw, 0o600))
	start := func(id, transcript string) {
		t.Helper()
		require.Empty(t, runHookSpawning(t, d.cfg, hookCall{event: "session-start",
			payload: map[string]any{"session_id": id, "cwd": f.cwd, "source": "startup", "transcript_path": transcript}}, spawn))
	}
	end := func(id string) core.Session {
		t.Helper()
		sess := d.session(t, "claude-code", id)
		sess.Status, sess.Findings, sess.UpdatedAt = core.SessionCompleted, "done", time.Now().UTC()
		require.NoError(t, store.UpdateSession(ctx, d.db, sess))
		marked, err := store.MarkUsageOwed(ctx, d.db, sess.ID, time.Now().UTC())
		require.NoError(t, err)
		require.True(t, marked)
		return sess
	}
	owed := func(id string) bool {
		t.Helper()
		_, found, err := store.OwedUsageOf(ctx, d.db, "claude-code", id)
		require.NoError(t, err)
		return found
	}

	start("cc-ended", ended)
	start("cc-out", f.transcript) // its recorded path lies outside the client's folder
	sess := end("cc-ended")
	require.Empty(t, jobs)

	start("cc-next", f.transcript)
	require.Equal(t, [][]string{{"hook-usage", "--owed", "--session-id", "cc-ended", "--path", ended}}, jobs)
	_, rows, ok := harvest.ClaudeUsage(ended)
	require.True(t, ok)
	require.Equal(t, rows, d.ledgerLedger(t, "cc-ended"))
	require.Equal(t, core.SessionCompleted, d.reload(t, sess.ID).Status, "the push revives no row")
	require.True(t, owed("cc-ended"), "owed still: the conversation may go on on its row")

	jobs = nil
	start("cc-third", f.transcript)
	require.Empty(t, jobs, "within the wait after the answer")

	out := end("cc-out")
	pushes := d.pushes.Load()
	start("cc-fourth", f.transcript)
	require.Equal(t, [][]string{{"hook-usage", "--owed", "--session-id", "cc-out", "--path", f.transcript}}, jobs)
	require.Equal(t, pushes+1, d.pushes.Load(), "one report, no rows")
	require.False(t, owed("cc-out"), "a refused path is settled")
	require.Empty(t, d.ledgerLedger(t, "cc-out"))
	require.Equal(t, core.SessionCompleted, d.reload(t, out.ID).Status)
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
