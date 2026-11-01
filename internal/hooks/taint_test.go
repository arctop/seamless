package hooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

func newTaintServer(t *testing.T, on bool) (*httptest.Server, func() []store.TaintedSession) {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.SetSetting(context.Background(), db, store.SettingRepoProjectMap, `{"/work/demo":"demo"}`))
	rec := events.NewRecorder(db)
	ret := retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	h := NewHandler(Config{DB: db, Retrieve: ret, Events: rec, APIKey: testKey,
		Features: config.Features{Screening: on}, Screening: screening.New(screening.Config{DB: db, Events: rec})})
	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	list := func() []store.TaintedSession {
		got, lerr := store.TaintedSessions(context.Background(), db)
		require.NoError(t, lerr)
		return got
	}
	return ts, list
}

// The agent guard marks its own session by forwarding its hook stdin: the
// client's session id names the ambient session. The first mark records the
// reason; a repeat changes nothing.
func TestSessionTaint_HookMarksItsOwnSession(t *testing.T) {
	ts, tainted := newTaintServer(t, true)
	// SessionStart creates the ambient session the mark lands on.
	resp, _ := post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": "cc-ext-0001", "cwd": "/work/demo", "source": "startup"})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, out := post(t, ts.URL+"/api/hooks/session-taint", testKey, map[string]any{
		"session_id": "cc-ext-0001", "reason": "web_fetch", "tool_name": "WebFetch"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "marked", resp.Header.Get(TaintHeader))
	// The body is the plain hook acknowledgement: `seam hook session-taint`
	// copies it to the guard hook's stdout, which its client parses.
	require.Equal(t, map[string]any{"continue": true, "suppressOutput": true}, out)
	resp, _ = post(t, ts.URL+"/api/hooks/session-taint", testKey, map[string]any{"session_id": "cc-ext-0001", "reason": "mail"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "already-marked", resp.Header.Get(TaintHeader), "the first mark stands")

	list := tainted()
	require.Len(t, list, 1)
	require.Equal(t, "web_fetch", list[0].Reason, "an explicit reason wins over the tool name")
	require.Equal(t, "demo", list[0].Project)

	// A Seamless session name works the same; an unknown session is 404.
	resp, _ = post(t, ts.URL+"/api/hooks/session-taint", testKey, map[string]any{"session": list[0].Name})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "already-marked", resp.Header.Get(TaintHeader))
	resp, _ = post(t, ts.URL+"/api/hooks/session-taint", testKey, map[string]any{"session_id": "no-such-session"})
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, "unknown-session", resp.Header.Get(TaintHeader))
}

func TestSessionTaint_Boundary(t *testing.T) {
	ts, tainted := newTaintServer(t, true)
	resp, _ := post(t, ts.URL+"/api/hooks/session-taint", "", map[string]any{"session_id": "x"})
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "the static key, like every hook")
	resp, _ = post(t, ts.URL+"/api/hooks/session-taint?client=nope", testKey, map[string]any{"session_id": "x"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp, _ = post(t, ts.URL+"/api/hooks/session-taint", testKey, map[string]any{})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "name the session")
	resp, _ = post(t, ts.URL+"/api/hooks/session-taint", testKey, map[string]any{"session_id": "a", "session": "b"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "exactly one way")
	resp, _ = post(t, ts.URL+"/api/hooks/session-taint", testKey, map[string]any{"session_id": "a", "reason": "Free Text!"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "a reason is a short code, never free text")
	require.Equal(t, "invalid", resp.Header.Get(TaintHeader))
	require.Empty(t, tainted())
}

// Off -- the default -- the endpoint marks nothing and says so.
func TestSessionTaint_OffMarksNothing(t *testing.T) {
	ts, tainted := newTaintServer(t, false)
	resp, _ := post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": "cc-ext-0002", "cwd": "/work/demo", "source": "startup"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp, _ = post(t, ts.URL+"/api/hooks/session-taint", testKey, map[string]any{"session_id": "cc-ext-0002"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "off", resp.Header.Get(TaintHeader))
	require.Empty(t, tainted())
}

// No hook clears a mark: the only way out is the owner's console.
func TestSessionTaint_NoHookClearsAMark(t *testing.T) {
	for _, hs := range seamlessHooks {
		require.NotContains(t, hs.Endpoint, "untaint")
	}
	for _, hs := range codexHooks {
		require.NotContains(t, hs.Endpoint, "untaint")
	}
	require.Equal(t, core.EventKind("session.untainted"), core.EventSessionCleared)
}

// A guard that forwards its PostToolUse stdin unchanged names the tool, which
// becomes the reason code.
func TestSessionTaint_ToolNameBecomesTheReason(t *testing.T) {
	ts, tainted := newTaintServer(t, true)
	resp, _ := post(t, ts.URL+"/api/hooks/session-start", testKey, map[string]any{
		"session_id": "cc-ext-0003", "cwd": "/work/demo", "source": "startup"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp, _ = post(t, ts.URL+"/api/hooks/session-taint", testKey, map[string]any{
		"session_id": "cc-ext-0003", "hook_event_name": "PostToolUse", "tool_name": "mcp__mail__Read Inbox",
		"tool_input": map[string]any{"q": "x"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	list := tainted()
	require.Len(t, list, 1)
	require.Equal(t, "tool:mcp__mail__read_inbox", list[0].Reason)
	require.Equal(t, "", toolReason("  "))
}
