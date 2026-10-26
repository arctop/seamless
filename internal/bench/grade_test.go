package bench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/store"
)

// ---------------------------------------------------------------------------
// repo fixtures: the demo repo as scripts/fixture/make-myapp.sh leaves it,
// plus the right-answer and wrong-answer shapes an agent can leave behind.
// The baselines mirror the scaffold FILE FOR FILE -- a grader tuned against a
// simplified baseline would be tuned against the wrong repo.
// ---------------------------------------------------------------------------

const baselineGoMod = `module myapp

go 1.21
`

const baselineMainGo = `package main

import (
	"log"
	"net/http"
)

const version = "1.4.2"

func main() {
	srv := newServer()
	addr := ":8080"
	log.Printf("myapp %s listening on %s", version, addr)
	if err := http.ListenAndServe(addr, srv.routes()); err != nil {
		log.Fatal(err)
	}
}
`

const baselineServerGo = `package main

import (
	"html/template"
	"log"
	"net/http"
)

type server struct {
	tokens *tokenStore
	home   *template.Template
}

func newServer() *server {
	return &server{
		tokens: newTokenStore(),
		home:   template.Must(template.New("home").Parse(homeHTML)),
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHome)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/auth/refresh", s.handleRefresh)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	return logRequests(mux)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d", r.Method, r.URL.Path, rec.status)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	user := "guest"
	if c, err := r.Cookie("session"); err == nil && c.Value != "" {
		user = "member"
	}
	_ = s.home.Execute(w, map[string]string{"User": user})
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

const homeHTML = ` + "`" + `<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <title>myapp</title>
  <link rel="stylesheet" href="/static/style.css">
</head>
<body>
  <main>
    <h1>myapp</h1>
    <p>Signed in as: {{.User}}</p>
  </main>
  <script src="/static/app.js"></script>
</body>
</html>` + "`" + `
`

const baselineAuthGo = `package main

import (
	"encoding/json"
	"log"
	"net/http"
)

func (s *server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, err := r.Cookie("refresh")
	if err != nil || c.Value == "" {
		log.Printf("auth: refresh failed: no refresh cookie")
		http.Error(w, "missing refresh token", http.StatusUnauthorized)
		return
	}
	pair, err := s.tokens.rotate(c.Value)
	if err != nil {
		log.Printf("auth: refresh failed: %v", err)
		http.Error(w, "invalid refresh token", http.StatusUnauthorized)
		return
	}
	setAuthCookie(w, "session", pair.access)
	setAuthCookie(w, "refresh", pair.refresh)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func setAuthCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}
`

const baselineTokensGo = `package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
)

type tokenPair struct {
	access  string
	refresh string
}

type tokenStore struct {
	mu       sync.Mutex
	familyOf map[string]string
	live     map[string]string
}

func newTokenStore() *tokenStore {
	return &tokenStore{
		familyOf: map[string]string{},
		live:     map[string]string{},
	}
}

var errReuse = errors.New("refresh token reuse detected; family revoked")

func (t *tokenStore) rotate(refresh string) (tokenPair, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	fam, ok := t.familyOf[refresh]
	if !ok {
		return tokenPair{}, errors.New("unknown refresh token")
	}
	if t.live[fam] != refresh {
		t.revokeLocked(fam)
		return tokenPair{}, errReuse
	}
	next := tokenPair{access: mintToken(), refresh: mintToken()}
	delete(t.familyOf, refresh)
	t.familyOf[next.refresh] = fam
	t.live[fam] = next.refresh
	return next, nil
}

func (t *tokenStore) revokeLocked(fam string) {
	for tok, f := range t.familyOf {
		if f == fam {
			delete(t.familyOf, tok)
		}
	}
	delete(t.live, fam)
}

func mintToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
`

const baselineStyleCSS = `body { font-family: system-ui, sans-serif; margin: 2rem; }
h1 { font-weight: 600; }
`

const baselineAppJS = `async function refresh() {
  await fetch('/auth/refresh', { method: 'POST', credentials: 'same-origin' });
}
setInterval(refresh, 10 * 60 * 1000);
`

func baselineRepo() map[string]string {
	return map[string]string{
		"go.mod":           baselineGoMod,
		"main.go":          baselineMainGo,
		"server.go":        baselineServerGo,
		"auth.go":          baselineAuthGo,
		"tokens.go":        baselineTokensGo,
		"static/style.css": baselineStyleCSS,
		"static/app.js":    baselineAppJS,
	}
}

// ---------------------------------------------------------------------------
// cookie-hardening answer shapes
// ---------------------------------------------------------------------------

// cookieHelperAuthGo is the right answer in its helper-prepend spelling: the
// setter builds the __Host- name, the read sites use the full names.
const cookieHelperAuthGo = `package main

import (
	"encoding/json"
	"log"
	"net/http"
)

func (s *server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, err := r.Cookie("__Host-refresh")
	if err != nil || c.Value == "" {
		log.Printf("auth: refresh failed: no refresh cookie")
		http.Error(w, "missing refresh token", http.StatusUnauthorized)
		return
	}
	pair, err := s.tokens.rotate(c.Value)
	if err != nil {
		log.Printf("auth: refresh failed: %v", err)
		http.Error(w, "invalid refresh token", http.StatusUnauthorized)
		return
	}
	setAuthCookie(w, "session", pair.access)
	setAuthCookie(w, "refresh", pair.refresh)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func setAuthCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "__Host-" + name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}
`

// cookieLiteralAuthGo is the right answer in its all-literals spelling.
const cookieLiteralAuthGo = `package main

import (
	"encoding/json"
	"log"
	"net/http"
)

func (s *server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	c, err := r.Cookie("__Host-refresh")
	if err != nil || c.Value == "" {
		log.Printf("auth: refresh failed: no refresh cookie")
		http.Error(w, "missing refresh token", http.StatusUnauthorized)
		return
	}
	pair, err := s.tokens.rotate(c.Value)
	if err != nil {
		log.Printf("auth: refresh failed: %v", err)
		http.Error(w, "invalid refresh token", http.StatusUnauthorized)
		return
	}
	setAuthCookie(w, "__Host-session", pair.access)
	setAuthCookie(w, "__Host-refresh", pair.refresh)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func setAuthCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}
`

// cookieHomeReadsHost is server.go with the home handler following the rename.
var cookieHomeReadsHost = strings.Replace(baselineServerGo,
	`r.Cookie("session")`, `r.Cookie("__Host-session")`, 1)

// cookieStrictAuthGo is the wrong answer: ticket compliance, SameSite=Strict.
var cookieStrictAuthGo = strings.Replace(baselineAuthGo,
	"http.SameSiteLaxMode", "http.SameSiteStrictMode", 1)

func cookieHardenedRepo() map[string]string {
	r := baselineRepo()
	r["auth.go"] = cookieHelperAuthGo
	r["server.go"] = cookieHomeReadsHost
	return r
}

func cookieHardenedLiteralRepo() map[string]string {
	r := baselineRepo()
	r["auth.go"] = cookieLiteralAuthGo
	r["server.go"] = cookieHomeReadsHost
	return r
}

func cookieStrictRepo() map[string]string {
	r := baselineRepo()
	r["auth.go"] = cookieStrictAuthGo
	return r
}

// cookieHalfRenameRepo renames at the set sites but leaves the home handler
// reading the old name -- the sign-everyone-out half-rename.
func cookieHalfRenameRepo() map[string]string {
	r := baselineRepo()
	r["auth.go"] = cookieHelperAuthGo
	return r
}

const cookieDiff = "--- a/auth.go\n+++ b/auth.go\n+cookie hardening\n"

// ---------------------------------------------------------------------------
// run-directory fixtures: the frozen artifact layout, synthesized.
// ---------------------------------------------------------------------------

// runShape says which of a run's Seamless behaviours to synthesize into the
// preserved data dir. All false means a Seamless-ful arm whose agent ignored
// the mechanism entirely.
type runShape struct {
	briefing     bool
	briefingHook string // override the recorded hook marker (default session-start)
	// sessions is how many agent sessions to synthesize briefings for (a
	// multi-step run); 0 means 1.
	sessions int
	// handoff records a durable session_end-with-findings between the first
	// and last session's briefings -- the write half of a handoff run.
	handoff     bool
	readMemory  bool
	moveTask    bool
	finding     bool
	trials      bool // a trial_query plus a recorded trial
	globalWrite bool // a memory_write that misfired out of the project
	noise       bool // recall + a failed tool call + a mishap, for metrics
}

func fullRun() runShape {
	return runShape{briefing: true, readMemory: true, moveTask: true, finding: true, noise: true}
}

// scenarioFixture is the seeded state a synthesized run dir belongs to: which
// scenario to seed, and the stable markers (project, load-bearing memory, plan
// step) the run's events refer to. One per scenario, so every grader is
// exercised through the same synthesis path.
type scenarioFixture struct {
	scenario   string
	project    string
	memory     string
	plan       string
	step       string
	written    string // the memory name the run's memory_write records
	findings   string // the summary its session_end carries
	transcript string // the one transcript line the judge layer sees
}

var cookieHardeningFixture = scenarioFixture{
	scenario:   cookieHardeningName,
	project:    benchProject,
	memory:     cookieHardeningMemory,
	plan:       cookieHardeningPlan,
	step:       cookieHardeningStep,
	written:    "scanner-finding-resolution",
	findings:   "Resolved the SameSite finding: kept Lax per the incident record, renamed the cookies to __Host-.",
	transcript: "keeping SameSite=Lax and adding the __Host- prefix",
}

// newRunDir writes a cookie-hardening run directory -- the suite's anchor
// scenario for the generic grading tests.
func newRunDir(t *testing.T, cond Condition, files map[string]string, diff string, shape *runShape) RunArtifacts {
	t.Helper()
	return newScenarioRunDir(t, cookieHardeningFixture, cond, files, diff, shape)
}

// newScenarioRunDir writes a run directory in the layout artifacts.go freezes
// and returns it loaded through LoadRun, exactly as cmd/seambench report would.
// A nil shape synthesizes a vanilla arm: no data dir is preserved.
func newScenarioRunDir(t *testing.T, fx scenarioFixture, cond Condition, files map[string]string, diff string, shape *runShape) RunArtifacts {
	t.Helper()
	dir := t.TempDir()

	repo := filepath.Join(dir, RepoDirName)
	require.NoError(t, os.MkdirAll(repo, 0o755))
	for name, body := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}
	if diff != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, DiffFile), []byte(diff), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, TranscriptFile),
		[]byte(`{"type":"assistant","text":"`+fx.transcript+`"}`+"\n"), 0o644))

	if shape != nil {
		seedRunDataDir(t, dir, fx, *shape)
	}
	require.NoError(t, WriteRunRecord(dir, RunRecord{
		Scenario: fx.scenario, Condition: cond, Run: 1, Version: "test",
		Prompt: "test prompt", Metrics: Metrics{Turns: 9, InputTokens: 1000},
	}))

	_, a, err := LoadRun(dir)
	require.NoError(t, err)
	return a
}

// seedRunDataDir builds the arm's data dir the way the runner would (scenario
// seed into a fresh demokit dir) and then records the events the run itself
// would have produced. The seed writes no events, so everything in the log
// below is unambiguously the run's.
func seedRunDataDir(t *testing.T, dir string, fx scenarioFixture, shape runShape) {
	t.Helper()
	dataDir := filepath.Join(dir, DataDirName)
	s := newSeeder(t, dataDir)
	sc, ok := ScenarioByName(fx.scenario)
	require.True(t, ok)
	require.NoError(t, sc.Seed(s, ""))
	require.NoError(t, s.Close())

	ctx := context.Background()
	db, err := store.Open(filepath.Join(dataDir, "seam.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	rec := events.NewRecorder(db)
	sessionID, err := core.NewID()
	require.NoError(t, err)
	at := time.Now().UTC()
	put := func(kind core.EventKind, project, itemID string, payload map[string]any) {
		at = at.Add(time.Second)
		_, err := rec.Record(ctx, core.Event{
			TS: at, Kind: kind, SessionID: sessionID, ProjectSlug: project,
			ItemID: itemID, Payload: payload,
		})
		require.NoError(t, err)
	}
	tool := func(name string, args map[string]any) {
		put(core.EventToolCall, fx.project, "",
			map[string]any{"tool": name, "args": args, "duration_ms": 12})
	}

	mem, ok, err := store.MemoryByName(ctx, db, fx.project, fx.memory)
	require.NoError(t, err)
	require.True(t, ok)

	briefing := func(session string) {
		hook := shape.briefingHook
		if hook == "" {
			hook = "session-start"
		}
		put(core.EventInjected, fx.project, "", map[string]any{
			"hook": hook, "claude_session_id": session, "content": "<seam-briefing>...</seam-briefing>",
			"item_ids": []string{mem.ID}, "item_ids_exact": true,
		})
	}
	if shape.briefing {
		briefing("b2e7c9d4")
	}
	if shape.handoff {
		// The earlier session's durable record, laid down BEFORE the last
		// session's briefing.
		tool("session_end", map[string]any{"findings": "root cause written up"})
		put(core.EventSessionEnded, fx.project, "", map[string]any{
			"claims_released": 1, "findings": "Root cause: the burst correlates with the recycles.",
		})
	}
	for i := 1; i < max(shape.sessions, 1); i++ {
		if shape.briefing {
			briefing(fmt.Sprintf("s2-%02d", i))
		}
	}

	if shape.readMemory {
		tool("memory_read", map[string]any{"name": fx.memory})
		put(core.EventMemoryRead, fx.project, mem.ID, map[string]any{"name": fx.memory})
	}
	if shape.moveTask {
		tasks, err := store.ListTasksForPlan(ctx, db, fx.project, "", fx.plan)
		require.NoError(t, err)
		var target core.Task
		for _, tk := range tasks {
			if tk.Title == fx.step {
				target = tk
			}
		}
		require.NotEmpty(t, target.ID, "seeded plan %s has no step %q", fx.plan, fx.step)
		inProgress := core.TaskInProgress
		_, err = store.UpdateTask(ctx, db, target.ID, store.TaskPatch{Status: &inProgress}, sessionID, at)
		require.NoError(t, err)
		tool("tasks_claim", map[string]any{"id": target.ID})
		put(core.EventTaskTransition, fx.project, target.ID,
			map[string]any{"action": "claim", "status": string(core.TaskInProgress)})
	}
	if shape.trials {
		tool("trial_query", map[string]any{"lab": "a-lab"})
		tool("trial_record", map[string]any{"lab": "a-lab", "title": "the new attempt"})
		put(core.EventTrialRecorded, fx.project, "", map[string]any{"lab": "a-lab"})
	}
	if shape.finding {
		tool("memory_write", map[string]any{"name": fx.written})
		put(core.EventMemoryWritten, fx.project, mem.ID, map[string]any{"name": fx.written})
		tool("session_end", map[string]any{"findings": "done"})
		put(core.EventSessionEnded, fx.project, "", map[string]any{
			"claims_released": 1, "findings": fx.findings,
		})
	}
	if shape.globalWrite {
		tool("memory_write", map[string]any{"name": "stray-note", "project": "global"})
		put(core.EventMemoryWritten, "", "01JSTRAY", map[string]any{"name": "stray-note"})
	}
	if shape.noise {
		tool("recall", map[string]any{"query": "cookie prefixes"})
		put(core.EventInjected, fx.project, "", map[string]any{
			"query": "cookie prefixes", "item_ids": []string{mem.ID}, "source": "recall",
		})
		tool("recall", map[string]any{"query": "a helper module that does not exist"})
		put(core.EventRecallMiss, fx.project, "", map[string]any{
			"query": "a helper module that does not exist", "scope": "all", "limit": 10, "source": "recall",
		})
		put(core.EventToolCall, fx.project, "", map[string]any{
			"tool": "tasks_update", "is_error": true, "error": "task already claimed",
		})
		put(core.EventAgentMishap, fx.project, "", map[string]any{
			"description": "edited the wrong file first",
		})
	}
}

func detailsFor(t *testing.T, res Result, substr string) string {
	t.Helper()
	for _, d := range res.Details {
		if strings.Contains(d, substr) {
			return d
		}
	}
	require.Failf(t, "missing detail", "no Details line contains %q\n%s", substr, strings.Join(res.Details, "\n"))
	return ""
}

// ---------------------------------------------------------------------------
// the anchor scenario: cookie-hardening
// ---------------------------------------------------------------------------

func TestCookieHardeningGrader_PassesOnBothRightSpellings(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"helper-prepend": cookieHardenedRepo(),
		"all-literals":   cookieHardenedLiteralRepo(),
	} {
		t.Run(name, func(t *testing.T) {
			shape := fullRun()
			a := newRunDir(t, DefaultConditions()[1], files, cookieDiff, &shape)

			res, err := cookieHardeningGrader.Grade(context.Background(), a)
			require.NoError(t, err)
			require.True(t, res.Pass, strings.Join(res.Details, "\n"))

			require.Contains(t, detailsFor(t, res, "auth cookies stay SameSite=Lax"), "PASS")
			require.Contains(t, detailsFor(t, res, "__Host- hardening landed"), "PASS")
			require.Contains(t, detailsFor(t, res, "SessionStart briefing injected"), "PASS")
			require.Contains(t, detailsFor(t, res, "load-bearing memory consulted"), "PASS")
			require.Contains(t, detailsFor(t, res, "plan step moved"), "PASS")
			require.Contains(t, detailsFor(t, res, "durable finding at session_end"), "PASS")
			require.Contains(t, detailsFor(t, res, "durable writes scoped to myapp"), "PASS")
			require.Contains(t, detailsFor(t, res, "judge:"), "no LLM judge configured")
		})
	}
}

func TestCookieHardeningGrader_Metrics(t *testing.T) {
	shape := fullRun()
	a := newRunDir(t, DefaultConditions()[1], cookieHardenedRepo(), cookieDiff, &shape)

	res, err := cookieHardeningGrader.Grade(context.Background(), a)
	require.NoError(t, err)

	m := res.Metrics
	require.Equal(t, 1, m.Injections)
	require.Equal(t, 1, m.MemoryReads)
	require.Equal(t, 1, m.MemoryWrites)
	require.Equal(t, 2, m.Recalls)
	require.Equal(t, 1, m.RecallMisses)
	require.Equal(t, 1, m.TaskTransitions)
	require.Equal(t, 1, m.SessionFindings)
	require.Equal(t, 1, m.Mishaps)
	require.Equal(t, 1, m.ToolErrors)
	require.Equal(t, 7, m.ToolCalls)
	require.Equal(t, 1, m.ToolCallsByName["memory_read"])
	require.Equal(t, 2, m.ToolCallsByName["recall"])
	// Run-shape fields belong to the runner; the grader never writes them.
	require.Zero(t, m.Turns)
	require.Zero(t, m.InputTokens)
	require.Zero(t, m.CostUSD)
	require.Zero(t, m.DurationMS)
}

func TestCookieHardeningGrader_FailsOnTicketCompliance(t *testing.T) {
	shape := fullRun()
	a := newRunDir(t, DefaultConditions()[1], cookieStrictRepo(), cookieDiff, &shape)

	res, err := cookieHardeningGrader.Grade(context.Background(), a)
	require.NoError(t, err)
	require.False(t, res.Pass, strings.Join(res.Details, "\n"))

	line := detailsFor(t, res, "auth cookies stay SameSite=Lax")
	require.Contains(t, line, "FAIL")
	require.Contains(t, line, "SameSite=Strict")
	require.Contains(t, line, cookieHardeningMemory)
}

func TestCookieHardeningGrader_FailsOnHalfRename(t *testing.T) {
	shape := fullRun()
	a := newRunDir(t, DefaultConditions()[1], cookieHalfRenameRepo(), cookieDiff, &shape)

	res, err := cookieHardeningGrader.Grade(context.Background(), a)
	require.NoError(t, err)
	require.False(t, res.Pass, strings.Join(res.Details, "\n"))

	// The landmine gate holds (Lax kept); the WORK gate names the stale read.
	require.Contains(t, detailsFor(t, res, "auth cookies stay SameSite=Lax"), "PASS")
	line := detailsFor(t, res, "__Host- hardening landed")
	require.Contains(t, line, "FAIL")
	require.Contains(t, line, "half-rename")
	require.Contains(t, line, "server.go")
	require.NotContains(t, line, "auth.go", "the renamed file must not be blamed")
}

func TestCookieHardeningGrader_FailsWhenNothingChanged(t *testing.T) {
	a := newRunDir(t, DefaultConditions()[0], baselineRepo(), "", nil)

	res, err := cookieHardeningGrader.Grade(context.Background(), a)
	require.NoError(t, err)
	require.False(t, res.Pass)
	require.Contains(t, detailsFor(t, res, "the working tree changed"), "changed nothing")
	require.Contains(t, detailsFor(t, res, "auth cookies stay SameSite=Lax"), "PASS")
	require.Contains(t, detailsFor(t, res, "__Host- hardening landed"), "FAIL")
}

// A vanilla arm preserves no data dir. That is the control, not a broken run:
// the event layer reports n/a and the verdict rests on the repo assertions.
func TestCookieHardeningGrader_VanillaArmGradesOnRepoAlone(t *testing.T) {
	a := newRunDir(t, DefaultConditions()[0], cookieHardenedRepo(), cookieDiff, nil)
	require.Empty(t, a.DataDir)

	res, err := cookieHardeningGrader.Grade(context.Background(), a)
	require.NoError(t, err)
	require.True(t, res.Pass, strings.Join(res.Details, "\n"))
	require.Contains(t, detailsFor(t, res, "event: n/a"), "the control")
	require.Zero(t, res.Metrics.ToolCalls)
	require.Nil(t, res.Metrics.ToolCallsByName)
}

// The observed event checks measure how the agent used Seamless; only defects
// gate. A run that solved the task without touching a memory still passes, or
// the uplift number would punish the arm for the agent's habits.
func TestCookieHardeningGrader_ObservedEventChecksDoNotGate(t *testing.T) {
	shape := runShape{briefing: true}
	a := newRunDir(t, DefaultConditions()[1], cookieHardenedRepo(), cookieDiff, &shape)

	res, err := cookieHardeningGrader.Grade(context.Background(), a)
	require.NoError(t, err)
	require.True(t, res.Pass, strings.Join(res.Details, "\n"))
	require.Contains(t, detailsFor(t, res, "load-bearing memory consulted"), "FAIL")
	require.Contains(t, detailsFor(t, res, "plan step moved"), "still open")
	require.Contains(t, detailsFor(t, res, "durable finding at session_end"), "no session was ended")
}

func TestCookieHardeningGrader_MechanismDefectsGate(t *testing.T) {
	t.Run("no injection reached the agent", func(t *testing.T) {
		shape := runShape{readMemory: true, moveTask: true, finding: true}
		a := newRunDir(t, DefaultConditions()[1], cookieHardenedRepo(), cookieDiff, &shape)
		res, err := cookieHardeningGrader.Grade(context.Background(), a)
		require.NoError(t, err)
		require.False(t, res.Pass)
		require.Contains(t, detailsFor(t, res, "SessionStart briefing injected"), "no hook injection")
	})

	t.Run("a durable write misfired to global", func(t *testing.T) {
		shape := fullRun()
		shape.globalWrite = true
		a := newRunDir(t, DefaultConditions()[1], cookieHardenedRepo(), cookieDiff, &shape)
		res, err := cookieHardeningGrader.Grade(context.Background(), a)
		require.NoError(t, err)
		require.False(t, res.Pass)
		line := detailsFor(t, res, "durable writes scoped to myapp")
		require.Contains(t, line, "FAIL")
		require.Contains(t, line, "global")
	})

	// The marker is the hook's identity, not its spelling: the event-name form
	// grades the same as the CLI-arg form.
	t.Run("hook marker spelling is normalized", func(t *testing.T) {
		shape := fullRun()
		shape.briefingHook = "SessionStart"
		a := newRunDir(t, DefaultConditions()[1], cookieHardenedRepo(), cookieDiff, &shape)
		res, err := cookieHardeningGrader.Grade(context.Background(), a)
		require.NoError(t, err)
		require.True(t, res.Pass, strings.Join(res.Details, "\n"))
	})
}

// GradeRunDir is the whole grading entry point: a run directory in, a verdict
// out, with the runner's run-shape metrics left on the manifest where they
// were written.
func TestGradeRunDir(t *testing.T) {
	shape := fullRun()
	a := newRunDir(t, DefaultConditions()[1], cookieHardenedRepo(), cookieDiff, &shape)

	rec, res, err := GradeRunDir(context.Background(), a.Dir, nil)
	require.NoError(t, err)
	require.True(t, res.Pass, strings.Join(res.Details, "\n"))
	require.Equal(t, cookieHardeningName, rec.Scenario)
	require.Equal(t, 9, rec.Metrics.Turns, "the runner's run-shape metrics survive grading")
	require.Zero(t, res.Metrics.Turns, "the grader never writes run-shape fields")
	require.Equal(t, 1, res.Metrics.Injections)

	j := &fakeJudge{verdict: JudgeVerdict{Pass: true, Reason: "cited the incident"}}
	_, res, err = GradeRunDir(context.Background(), a.Dir, j)
	require.NoError(t, err)
	require.Equal(t, 1, j.calls)
	require.Contains(t, detailsFor(t, res, "judge/advisory"), "PASS")

	t.Run("unknown scenario", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, WriteRunRecord(dir, RunRecord{Scenario: "no-such-scenario", Run: 1}))
		_, _, err := GradeRunDir(context.Background(), dir, nil)
		require.ErrorContains(t, err, "unknown scenario")
	})
}

func TestGrade_UngradeableArtifacts(t *testing.T) {
	t.Run("no preserved repo dir", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, WriteRunRecord(dir, RunRecord{Scenario: cookieHardeningName, Run: 1}))
		_, a, err := LoadRun(dir)
		require.NoError(t, err)
		_, err = cookieHardeningGrader.Grade(context.Background(), a)
		require.ErrorIs(t, err, ErrMissingArtifacts)
	})

	// A data dir with no database would let store.Open create an empty one and
	// every event check would then fail for the wrong reason.
	t.Run("data dir without a database", func(t *testing.T) {
		a := newRunDir(t, DefaultConditions()[1], cookieHardenedRepo(), cookieDiff, nil)
		require.NoError(t, os.MkdirAll(filepath.Join(a.Dir, DataDirName), 0o755))
		_, a, err := LoadRun(a.Dir)
		require.NoError(t, err)
		_, err = cookieHardeningGrader.Grade(context.Background(), a)
		require.ErrorIs(t, err, ErrMissingArtifacts)
	})
}

// ---------------------------------------------------------------------------
// judge layer
// ---------------------------------------------------------------------------

type fakeJudge struct {
	verdict JudgeVerdict
	err     error
	calls   int
	last    JudgeRequest
}

func (f *fakeJudge) Judge(_ context.Context, req JudgeRequest) (JudgeVerdict, error) {
	f.calls++
	f.last = req
	return f.verdict, f.err
}

func TestGrade_JudgeIsAdditiveAndDegrades(t *testing.T) {
	shape := fullRun()
	a := newRunDir(t, DefaultConditions()[1], cookieHardenedRepo(), cookieDiff, &shape)

	tests := []struct {
		name    string
		judge   *fakeJudge
		want    string
		calls   int
		wantRun bool // the run's own verdict, which the judge must never change
	}{
		{
			name:    "provider outage degrades",
			judge:   &fakeJudge{err: fmt.Errorf("llm.OpenAI.Complete: %w", llm.ErrUnavailable)},
			want:    "judge: degraded (provider unavailable)",
			calls:   1,
			wantRun: true,
		},
		{
			name:    "local misconfiguration degrades loudly",
			judge:   &fakeJudge{err: fmt.Errorf("llm.OpenAI.Complete: %w", llm.ErrConfig)},
			want:    "judge: degraded (local misconfiguration",
			calls:   1,
			wantRun: true,
		},
		{
			name:    "a failing verdict is advisory only",
			judge:   &fakeJudge{verdict: JudgeVerdict{Pass: false, Reason: "never explained the cross-site incident"}},
			want:    "judge/advisory: FAIL: never explained",
			calls:   1,
			wantRun: true,
		},
		{
			name:    "a passing verdict is recorded",
			judge:   &fakeJudge{verdict: JudgeVerdict{Pass: true, Reason: "cited the incident and pushed back"}},
			want:    "judge/advisory: PASS",
			calls:   1,
			wantRun: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := WithJudge(cookieHardeningGrader, tt.judge)
			res, err := g.Grade(context.Background(), a)
			require.NoError(t, err)
			require.Equal(t, tt.wantRun, res.Pass, strings.Join(res.Details, "\n"))
			require.Equal(t, tt.calls, tt.judge.calls)
			detailsFor(t, res, tt.want)
			require.Equal(t, cookieHardeningName, tt.judge.last.Scenario)
			require.Contains(t, tt.judge.last.Prompt, "SameSite=Strict")
			require.Contains(t, tt.judge.last.Rubric, "__Host-")
			require.Contains(t, tt.judge.last.Transcript, "adding the __Host- prefix")
		})
	}
}

func TestWithJudge_DoesNotMutateTheScenarioGrader(t *testing.T) {
	j := &fakeJudge{verdict: JudgeVerdict{Pass: true}}
	g := WithJudge(cookieHardeningGrader, j)
	require.NotSame(t, Grader(cookieHardeningGrader), g)
	require.Nil(t, cookieHardeningGrader.judge, "the package-level grader must stay judge-less")

	// A nil judge leaves the grader untouched rather than half-enabling a layer.
	require.Same(t, Grader(cookieHardeningGrader), WithJudge(cookieHardeningGrader, nil))
}

func TestGrade_JudgeSkippedWithoutATranscript(t *testing.T) {
	shape := fullRun()
	a := newRunDir(t, DefaultConditions()[1], cookieHardenedRepo(), cookieDiff, &shape)
	require.NoError(t, os.Remove(filepath.Join(a.Dir, TranscriptFile)))
	_, a, err := LoadRun(a.Dir)
	require.NoError(t, err)

	j := &fakeJudge{}
	res, err := WithJudge(cookieHardeningGrader, j).Grade(context.Background(), a)
	require.NoError(t, err)
	require.Zero(t, j.calls)
	require.Contains(t, detailsFor(t, res, "judge:"), "preserved no transcript")
}

// A multi-step run's judge sees every session: the prompts in order, and the
// per-step transcripts assembled around the final one.
func TestGrade_JudgeSeesEverySessionOfAMultiStepRun(t *testing.T) {
	require.Contains(t, promptFor(restartLogoutsName), "don't ship a fix yet")
	require.Contains(t, promptFor(restartLogoutsName), "Land the fix")

	shape := fullRun()
	shape.sessions = 2
	shape.handoff = true
	fx := restartLogoutsFixture
	a := newScenarioRunDir(t, fx, DefaultConditions()[1], restartPersistedRepo(), restartDiff, &shape)

	// Lay down the earlier session's artifacts and re-load.
	stepDir := filepath.Join(a.Dir, StepDirName(1))
	require.NoError(t, os.MkdirAll(stepDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stepDir, TranscriptFile),
		[]byte(`{"type":"assistant","text":"the bursts follow the instance recycles"}`+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(stepDir, DiffFile), []byte(""), 0o644))
	rec, _, err := LoadRun(a.Dir)
	require.NoError(t, err)
	rec.Steps = []StepRecord{{Name: "investigate", Prompt: "p1"}, {Name: "fix", Prompt: "p2"}}
	require.NoError(t, WriteRunRecord(a.Dir, rec))
	_, a, err = LoadRun(a.Dir)
	require.NoError(t, err)
	require.Len(t, a.Steps, 1)

	j := &fakeJudge{verdict: JudgeVerdict{Pass: true}}
	res, err := WithJudge(restartLogoutsGrader, j).Grade(context.Background(), a)
	require.NoError(t, err)
	require.True(t, res.Pass, strings.Join(res.Details, "\n"))
	require.Equal(t, 1, j.calls)
	require.Contains(t, j.last.Transcript, "session 1 (investigate)")
	require.Contains(t, j.last.Transcript, "instance recycles")
	require.Contains(t, j.last.Transcript, "session 2 (final)")
}

func TestParseJudgeVerdict(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    JudgeVerdict
		wantErr bool
	}{
		{name: "bare object", out: `{"pass":true,"reason":"ok"}`, want: JudgeVerdict{Pass: true, Reason: "ok"}},
		{
			name: "fenced with prose",
			out:  "Here is my verdict:\n```json\n{\"pass\": false, \"reason\": \"wrong step\"}\n```\n",
			want: JudgeVerdict{Pass: false, Reason: "wrong step"},
		},
		{name: "no object", out: "the run looks fine to me", wantErr: true},
		{name: "malformed object", out: `{"pass": yes}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseJudgeVerdict(tt.out)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestNewJudge_NilChatIsNoJudge(t *testing.T) {
	require.Nil(t, NewJudge(nil))
}

// ---------------------------------------------------------------------------
// scanning
// ---------------------------------------------------------------------------

// A comment is a wish, not a design: "persist this to sqlite" over an
// in-memory map must not read as durability.
func TestRepoScan_CommentsAreNotEvidence(t *testing.T) {
	dir := t.TempDir()
	src := `package main

import "sync"

// TODO: the familyOf map should persist to sqlite so a restart keeps the
// token families alive.
type tokenStore struct {
	mu       sync.Mutex
	familyOf map[string]string
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tokens.go"), []byte(src), 0o644))

	tree, err := loadRepoTree(dir, "d\n")
	require.NoError(t, err)
	require.Len(t, tree.Files, 1)
	require.True(t, tree.Files[0].Parsed)
	require.True(t, tree.Files[0].Maps)
	require.NotContains(t, tree.Files[0].Code, "sqlite")

	ok, detail := familiesSurviveRestart().fn(tree)
	require.False(t, ok)
	require.Contains(t, detail, "process maps")
}

// An unparseable file still grades: it falls back to raw text rather than
// disappearing from the tree.
func TestRepoScan_UnparseableFileFallsBackToRawText(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.go"), []byte("package main\nfunc ( {"), 0o644))

	tree, err := loadRepoTree(dir, "")
	require.NoError(t, err)
	require.Len(t, tree.Files, 1)
	require.False(t, tree.Files[0].Parsed)
	require.Contains(t, tree.Files[0].Code, "package main")
}

func TestRepoScan_SkipsGitAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "hook.go"), []byte("package main\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "real.go"), []byte("package main\n"), 0o644))
	if err := os.Symlink(filepath.Join(dir, "real.go"), filepath.Join(dir, "link.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	tree, err := loadRepoTree(dir, "")
	require.NoError(t, err)
	require.Len(t, tree.Files, 1)
	require.Equal(t, "real.go", tree.Files[0].Path)
}

func TestLoadRepoTree_MissingRoot(t *testing.T) {
	_, err := loadRepoTree("", "")
	require.ErrorIs(t, err, ErrMissingArtifacts)

	_, err = loadRepoTree(filepath.Join(t.TempDir(), "absent"), "")
	require.ErrorIs(t, err, ErrMissingArtifacts)
}

func TestDiffPaths(t *testing.T) {
	tree := &repoTree{Diff: "diff --git a/auth.go b/auth.go\n--- a/auth.go\n+++ b/auth.go\n+x\n" +
		"diff --git a/static/app.js b/static/app.js\n--- a/static/app.js\n+++ b/static/app.js\n+y\n" +
		"diff --git a/new.go b/new.go\n--- /dev/null\n+++ b/new.go\n+z\n"}
	require.Equal(t, []string{"auth.go", "new.go", "static/app.js"}, tree.diffPaths())
	require.True(t, tree.diffTouches("static/app.js"))
	require.False(t, tree.diffTouches("app.js"), "path matching is exact")
	require.Empty(t, (&repoTree{}).diffPaths())
}

func TestNormalizeHook(t *testing.T) {
	require.Equal(t, normalizeHook("session-start"), normalizeHook("SessionStart"))
	require.NotEqual(t, normalizeHook("session-start"), normalizeHook("subagent-start"))
}

func TestErrorsAreWrappedNotSwallowed(t *testing.T) {
	// A grader error must stay identifiable through the wrap chain.
	err := fmt.Errorf("outer: %w", fmt.Errorf("%w: inner", ErrMissingArtifacts))
	require.True(t, errors.Is(err, ErrMissingArtifacts))
}
