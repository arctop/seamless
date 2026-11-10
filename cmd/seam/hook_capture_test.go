package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/harvest"
	"github.com/arctop/seamless/internal/hooks"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// captureHead is the commit the fixture repository has checked out.
var captureHead = strings.Repeat("cd", 20)

// captureFixture is an agent's machine for the capture tests: a home with a
// Claude Code plans dir (HOME and USERPROFILE point at it), a repository whose
// HEAD is captureHead, a session transcript with its subagent's transcript
// where Claude Code puts one, and the versioned Codex rollout.
type captureFixture struct {
	home, plans, cwd, transcript, agent, rollout, plan string
}

func newCaptureFixture(t *testing.T) captureFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	f := captureFixture{home: home, plans: filepath.Join(home, ".claude", "plans"), cwd: filepath.Join(home, "work", "ops")}
	require.NoError(t, os.MkdirAll(f.plans, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(f.cwd, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(f.cwd, ".git", "HEAD"), []byte(captureHead+"\n"), 0o600))

	f.transcript = filepath.Join(home, "session.jsonl")
	require.NoError(t, os.WriteFile(f.transcript, []byte(strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"deploy the ops service"}}`,
		`{"type":"assistant","message":{"id":"m1","role":"assistant","model":"claude-early","content":[{"type":"text","text":"Looking."},{"type":"tool_use","id":"t","name":"Bash","input":{}}],"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":100,"cache_creation_input_tokens":20}}}`,
		`{"type":"assistant","message":{"id":"m1","role":"assistant","model":"claude-early","content":[{"type":"text","text":"Looking."}],"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":100,"cache_creation_input_tokens":20}}}`,
		`{"type":"assistant","isSidechain":true,"message":{"id":"s1","role":"assistant","model":"claude-sub","content":[{"type":"text","text":"sub"}],"usage":{"input_tokens":3,"output_tokens":2}}}`,
		`{"type":"assistant","message":{"id":"m2","role":"assistant","model":"claude-late","content":[{"type":"text","text":"Deployed the ops service; the health check race is fixed."}],"usage":{"input_tokens":5,"output_tokens":9}}}`,
	}, "\n")+"\n"), 0o600))

	// Where Claude Code writes the subagent's transcript, so SubagentStart
	// (which names none) and SubagentStop resolve the same file.
	f.agent = harvest.SubagentTranscriptPath("", f.transcript, "a1")
	require.NoError(t, os.MkdirAll(filepath.Dir(f.agent), 0o755))
	agent, err := os.ReadFile(filepath.Join("..", "..", "internal", "hooks", "testdata", "agent-transcript.jsonl"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.agent, agent, 0o600))

	f.rollout, err = filepath.Abs(filepath.Join("..", "..", "internal", "hooks", "testdata", "codex", "v0.144.5", "rollout.jsonl"))
	require.NoError(t, err)

	f.plan = filepath.Join(f.plans, "example-plan.md")
	require.NoError(t, os.WriteFile(f.plan, []byte("# Example plan\n\nFix the health check race."), 0o600))
	return f
}

// hookCall is one hook the agent client runs: the event, --client, and the
// payload on stdin.
type hookCall struct {
	event, client string
	payload       map[string]any
}

// calls is a Claude Code session and a Codex session on the fixture: every
// event that reads a file, and the ones that read none.
func (f captureFixture) calls() []hookCall {
	return []hookCall{
		{"session-start", "", map[string]any{"session_id": "cc-1", "cwd": f.cwd, "source": "startup", "transcript_path": f.transcript}},
		{"user-prompt-submit", "", map[string]any{"session_id": "cc-1", "cwd": f.cwd, "prompt": "health check race", "transcript_path": f.transcript}},
		{"post-tool-use", "", map[string]any{"session_id": "cc-1", "cwd": f.cwd, "tool_name": "Write",
			"tool_input": map[string]any{"file_path": f.plan}, "transcript_path": f.transcript}},
		{"subagent-start", "", map[string]any{"session_id": "cc-1", "cwd": f.cwd, "agent_id": "a1", "agent_type": "Explore",
			"transcript_path": f.transcript}},
		{"subagent-stop", "", map[string]any{"session_id": "cc-1", "cwd": f.cwd, "agent_id": "a1", "agent_type": "Explore",
			"permission_mode": "plan", "transcript_path": f.transcript, "agent_transcript_path": f.agent}},
		{"permission-request", "", map[string]any{"session_id": "cc-1", "cwd": f.cwd, "tool_name": "ExitPlanMode", "transcript_path": f.transcript}},
		{"post-tool-use", "", map[string]any{"session_id": "cc-1", "cwd": f.cwd, "tool_name": "ExitPlanMode",
			"tool_response": map[string]any{"filePath": f.plan}, "transcript_path": f.transcript}},
		{"session-end", "", map[string]any{"session_id": "cc-1", "reason": "exit", "transcript_path": f.transcript}},
		{"session-start", hookClientCodex, map[string]any{"session_id": "cx-1", "cwd": f.cwd, "source": "startup", "model": "gpt-5.5", "transcript_path": f.rollout}},
		{"user-prompt-submit", hookClientCodex, map[string]any{"session_id": "cx-1", "cwd": f.cwd, "prompt": "deploy", "model": "gpt-5.5", "transcript_path": f.rollout}},
		{"subagent-start", hookClientCodex, map[string]any{"session_id": "cx-1", "cwd": f.cwd, "agent_id": "a2", "agent_type": "worker", "transcript_path": f.rollout}},
		{"subagent-stop", hookClientCodex, map[string]any{"session_id": "cx-1", "cwd": f.cwd, "agent_id": "a2", "transcript_path": f.rollout}},
		{"stop", hookClientCodex, map[string]any{"session_id": "cx-1", "cwd": f.cwd, "transcript_path": f.rollout, "model": "gpt-5.5"}},
	}
}

// recordedHooks stands in for seamlessd: it keeps every forwarded body, by
// event and client, and answers {} (with refused on the refusal header when
// set).
type recordedHooks struct {
	mu      sync.Mutex
	bodies  map[string][]byte
	refused string
}

func (rh *recordedHooks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rh.mu.Lock()
	rh.bodies[r.URL.Path+"?"+r.URL.Query().Get("client")] = body
	refused := rh.refused
	rh.mu.Unlock()
	if refused != "" {
		w.Header().Set(harvest.RefusedHeader, refused)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func (rh *recordedHooks) body(t *testing.T, event, client string) []byte {
	t.Helper()
	rh.mu.Lock()
	defer rh.mu.Unlock()
	b, ok := rh.bodies["/api/hooks/"+event+"?"+client]
	require.True(t, ok, "%s (%s) was not forwarded", event, client)
	return b
}

// serveSocket serves h on a peer_auth-style Unix socket and returns its path.
func serveSocket(t *testing.T, h http.Handler) string {
	t.Helper()
	// A short directory: a socket path is capped near 104 bytes on macOS.
	dir, err := os.MkdirTemp("", "seam")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// runHookCall runs one hook through runHook with cfg as the loaded config.
func runHookCall(t *testing.T, cfg config.Config, c hookCall) (stderr string) {
	t.Helper()
	raw, err := json.Marshal(c.payload)
	require.NoError(t, err)
	e, _, errb := stubEnv()
	e.stdin = bytes.NewReader(raw)
	e.loadConfig = func() (config.Config, error) { return cfg, nil }
	require.NoError(t, runHook(context.Background(), e, &hookOpts{client: c.client}, []string{c.event}))
	return errb.String()
}

func tcpConfig(addr string) config.Config {
	cfg := config.Defaults()
	cfg.Addr = addr
	cfg.MCP.APIKey = "k"
	return cfg
}

func socketConfig(sock string) config.Config {
	cfg := config.Defaults()
	cfg.Addr = "127.0.0.1:1" // nothing listens: only the socket can answer
	cfg.AdvertisedURL = "unix://" + sock
	return cfg
}

// Off the peer_auth socket nothing is pushed: every hook forwards the agent
// client's payload byte for byte, although every file the captures read is
// there to be read.
func TestRunHook_OffTheSocketForwardsThePayloadUnchanged(t *testing.T) {
	f := newCaptureFixture(t)
	rh := &recordedHooks{bodies: map[string][]byte{}}
	srv := httptest.NewServer(rh)
	t.Cleanup(srv.Close)
	cfg := tcpConfig(strings.TrimPrefix(srv.URL, "http://"))
	require.False(t, pushesCaptures(cfg))
	for _, c := range f.calls() {
		raw, err := json.Marshal(c.payload)
		require.NoError(t, err)
		runHookCall(t, cfg, c)
		got := rh.body(t, c.event, c.client)
		require.Equal(t, raw, got, "%s (%s): the body is the agent client's own", c.event, c.client)
		require.NotContains(t, string(got), harvest.Field)
	}
}

// On the peer_auth socket each hook pushes the capture the daemon would have
// made from the same files, read with the same parsers, beside the agent
// client's own fields; the hooks that read nothing push nothing.
func TestRunHook_OnTheSocketPushesTheCaptures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	f := newCaptureFixture(t)
	rh := &recordedHooks{bodies: map[string][]byte{}}
	cfg := socketConfig(serveSocket(t, rh))
	require.True(t, pushesCaptures(cfg))

	ccTokens := core.TokenUsage{Input: 18, Cached: 100, CacheCreation: 20, Output: 15, Total: 153} // m1 once, s1, m2
	cxTokens := core.TokenUsage{Input: 12655 - 9600, Cached: 9600, Output: 75, Total: 12730}
	report := "Final report: the gardener has three passes."
	planText := "# Example plan\n\nFix the health check race."
	want := map[string]*harvest.Push{
		"session-start?":           {Version: 1, Model: "claude-late"},
		"user-prompt-submit?":      {Version: 1, Model: "claude-late"},
		"subagent-start?":          {Version: 1, Prompt: "Explore the gardener package and report its structure"},
		"subagent-stop?":           {Version: 1, Prompt: "Explore the gardener package and report its structure", Report: report, GitHead: captureHead},
		"permission-request?":      {Version: 1},
		"session-end?":             {Version: 1, Model: "claude-late", Findings: "(auto-harvested) Deployed the ops service; the health check race is fixed.", Tokens: &ccTokens},
		"session-start?codex":      nil, // Codex states its model
		"user-prompt-submit?codex": nil,
		"subagent-start?codex":     {Version: 1, Prompt: harvest.CodexFirstUserMessage(f.rollout)},
		"subagent-stop?codex":      nil, // a heartbeat only
		"stop?codex":               {Version: 1, Findings: harvest.CodexStopFindings("", f.rollout), Tokens: &cxTokens},
	}
	require.NotEmpty(t, want["subagent-start?codex"].Prompt, "the fixture has a user message")
	require.NotEmpty(t, want["stop?codex"].Findings, "the fixture has an agent message")
	for _, c := range f.calls() {
		require.Empty(t, runHookCall(t, cfg, c))
		raw, err := json.Marshal(c.payload)
		require.NoError(t, err)
		got := rh.body(t, c.event, c.client)
		push, ok, err := harvest.DecodePush(got)
		require.NoError(t, err)
		key := c.event + "?" + c.client
		if c.event == "post-tool-use" {
			// Write and ExitPlanMode both push the plan file, read under this
			// account's own plans dir, with the head it was written at.
			require.True(t, ok)
			require.Equal(t, harvest.Push{Version: 1, GitHead: captureHead,
				Plan: &harvest.PlanFile{Basename: "example-plan", Content: planText}}, push, c.payload["tool_name"])
		} else if w := want[key]; w == nil {
			require.False(t, ok, "%s pushes nothing", key)
			require.Equal(t, raw, got, "%s: the body is the agent client's own", key)
			continue
		} else {
			require.True(t, ok, key)
			require.Equal(t, *w, push, key)
		}
		// Everything the agent client sent rides on unchanged beside the push.
		var sent, forwarded map[string]any
		require.NoError(t, json.Unmarshal(raw, &sent))
		require.NoError(t, json.Unmarshal(got, &forwarded))
		delete(forwarded, harvest.Field)
		require.Equal(t, sent, forwarded, key)
	}
}

// A capture the daemon refused at a lock comes back as one line on a header,
// which the hook prints on stderr -- and does not retry.
func TestRunHook_PrintsTheRefusalNotice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	f := newCaptureFixture(t)
	const notice = "Seamless did not record this plan: project example-finance is locked and this session holds no approval."
	rh := &recordedHooks{bodies: map[string][]byte{}, refused: notice}
	cfg := socketConfig(serveSocket(t, rh))
	stderr := runHookCall(t, cfg, f.calls()[2])
	require.Equal(t, "seam hook: "+notice+"\n", stderr)
	require.Len(t, rh.bodies, 1, "sent once")
}

// The client side parses the same formats the daemon parsed: the same session
// run once against a daemon that reads the agent's files itself (peer_auth
// off, over TCP) and once against one that reads none and takes the pushes
// (the owner on the peer_auth socket) records the same findings, tokens and
// model, the same plan note and the same subagent note.
func TestHookCaptures_ThePushMatchesTheDaemonsOwnRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the peer_auth socket is a Unix deployment")
	}
	f := newCaptureFixture(t)
	calls := f.calls()

	off := captureDaemon(t, f)
	srv := httptest.NewServer(off.mux)
	t.Cleanup(srv.Close)
	for _, c := range calls {
		require.Empty(t, runHookCall(t, tcpConfig(strings.TrimPrefix(srv.URL, "http://")), c))
	}

	on := captureDaemon(t, f)
	policy, err := access.New(config.PeerAuth{Owner: "owner", ForceLocked: []string{"example-finance"}},
		func(string) (access.Account, error) { return access.Account{UID: 501, Name: "owner"}, nil })
	require.NoError(t, err)
	view, ok := policy.PeerView(501)
	require.True(t, ok)
	sock := serveSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		on.mux.ServeHTTP(w, r.WithContext(access.WithView(r.Context(), view)))
	}))
	for _, c := range calls {
		require.Empty(t, runHookCall(t, socketConfig(sock), c))
	}

	for _, id := range []struct{ client, external string }{{"claude-code", "cc-1"}, {"codex", "cx-1"}} {
		read, pushed := off.ambient(t, id.client, id.external), on.ambient(t, id.client, id.external)
		require.NotEmpty(t, read.Findings, "the daemon's own read found something to compare")
		require.Equal(t, read.Findings, pushed.Findings, id.external)
		require.False(t, read.Tokens.Empty())
		require.Equal(t, read.Tokens, pushed.Tokens, id.external)
		require.NotEmpty(t, read.Model)
		require.Equal(t, read.Model, pushed.Model, id.external)
		require.Equal(t, read.Status, pushed.Status, id.external)
	}
	for _, slug := range []string{"cc-plan-example-plan", "cc-agent-a1"} {
		read, pushed := off.note(t, slug), on.note(t, slug)
		require.Contains(t, read.Body, "git "+captureHead[:12], "the stamp carries the repository's head")
		require.Equal(t, read.Title, pushed.Title, slug)
		require.Equal(t, read.Tags, pushed.Tags, slug)
		require.Equal(t, read.Model, pushed.Model, slug)
		require.Equal(t, stampless(read.Body), stampless(pushed.Body), slug)
	}
}

// captureDB is one stand-in daemon's state.
type captureDB struct {
	db  *sql.DB
	mgr *files.Manager
	mux *http.ServeMux
}

func (d captureDB) ambient(t *testing.T, client, external string) core.Session {
	t.Helper()
	sess, ok, err := store.AmbientSessionByExternalIdentity(context.Background(), d.db, client, external)
	require.NoError(t, err)
	require.True(t, ok, "%s/%s", client, external)
	return sess
}

func (d captureDB) note(t *testing.T, slug string) core.Note {
	t.Helper()
	idx, ok, err := store.NoteBySlug(context.Background(), d.db, "example-ops", slug)
	require.NoError(t, err)
	require.True(t, ok, slug)
	n, err := d.mgr.Store().ReadNote(idx.FilePath)
	require.NoError(t, err)
	return n
}

// captureDaemon is a real hook handler over a fresh database, with plan
// capture on, that may read the fixture's files when nothing stops it: on
// this machine, with this home's plans dir.
func captureDaemon(t *testing.T, f captureFixture) captureDB {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(ctx, db, config.Hostname()))
	_, err = store.EnsureProject(ctx, db, "example-ops", "example-ops")
	require.NoError(t, err)
	require.NoError(t, store.AddRepoMapping(ctx, db, f.cwd, "example-ops"))
	mgr, err := files.NewManager(filepath.Join(dir, "data"), db, nil)
	require.NoError(t, err)
	h := hooks.NewHandler(hooks.Config{
		DB: db, Retrieve: retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil),
		Events: events.NewRecorder(db), Files: mgr, APIKey: "k",
		PlanCapture: config.PlanCapture{Enabled: true}, PlansDir: f.plans, LocalHost: config.Hostname(),
	})
	mux := http.NewServeMux()
	h.Register(mux)
	return captureDB{db: db, mgr: mgr, mux: mux}
}

// stampless drops the capture time from a note's provenance stamp, the one
// part of two captures of the same content that differs.
var stampTime = regexp.MustCompile(`\| \d{4}-\d\d-\d\dT[0-9:]+Z`)

func stampless(body string) string { return stampTime.ReplaceAllString(body, "| <time>") }
