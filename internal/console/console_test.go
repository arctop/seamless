package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const testKey = "test-key-abc123"

// newConsole builds a console over a fresh DB and returns the DB (for seeding)
// and its mux.
//
// Optional features are ON here, deliberately: this helper backs the tests that
// exercise the feature's own screens (labs, trials, the trials search scope,
// session trials), which would otherwise be testing the gate instead of the
// surface. The default-OFF state is what newConsoleFeatures exists for, and
// features_test.go asserts both directions.
func newConsole(t *testing.T) (*sql.DB, *http.ServeMux) {
	t.Helper()
	return newConsoleFeatures(t, config.Features{Research: true})
}

// newConsoleFeatures builds a console over a fresh DB with an explicit
// optional-feature base (the file/env layer), for the gating tests.
//
// The console level is ADVANCED here, deliberately, for the same reason
// newConsole turns features on: these helpers back the tests of the screens and
// controls themselves, which would otherwise be testing the level gates instead
// of the surface. The level tests pick their level with newConsoleLevel.
func newConsoleFeatures(t *testing.T, feats config.Features) (*sql.DB, *http.ServeMux) {
	t.Helper()
	return newConsoleLevel(t, feats, "advanced")
}

// newConsoleLevel builds a console over a fresh DB with an explicit
// optional-feature base and file/env console level base.
func newConsoleLevel(t *testing.T, feats config.Features, lvl string) (*sql.DB, *http.ServeMux) {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	svc, err := New(Config{
		DB: db, Events: events.NewRecorder(db), APIKey: testKey,
		// A nil embedder keeps search lexical-only: no test needs a provider.
		Retrieve: retrieve.New(db, nil, config.Defaults().Budgets, nil),
		Features: feats,
		Level:    lvl,
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return db, mux
}

// newTestMux builds a console over a fresh DB and returns its mux.
func newTestMux(t *testing.T) *http.ServeMux {
	t.Helper()
	_, mux := newConsole(t)
	return mux
}

// getJSON issues an authenticated JSON GET and decodes the body into v.
func getJSON(t *testing.T, mux *http.ServeMux, path string, v any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Accept", "application/json")
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code, "GET %s -> %d: %s", path, rr.Code, rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), v))
}

func do(mux *http.ServeMux, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

func TestAuth_UnauthenticatedRedirectsToLogin(t *testing.T) {
	mux := newTestMux(t)
	rr := do(mux, httptest.NewRequest(http.MethodGet, "/console/", nil))
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "/console/login")
}

func TestAuth_JSONUnauthenticatedReturns401(t *testing.T) {
	mux := newTestMux(t)
	rr := do(mux, httptest.NewRequest(http.MethodGet, "/console/?format=json", nil))
	require.Equal(t, http.StatusUnauthorized, rr.Code)
	require.Contains(t, rr.Body.String(), "unauthorized")
}

func TestAuth_BearerKeyGrantsJSON(t *testing.T) {
	mux := newTestMux(t)
	req := httptest.NewRequest(http.MethodGet, "/console/?format=json", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Header().Get("Content-Type"), "application/json")
}

func TestLogin_WrongKeyRerendersWithError(t *testing.T) {
	mux := newTestMux(t)
	form := url.Values{"key": {"nope"}}
	req := httptest.NewRequest(http.MethodPost, "/console/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Body.String(), "invalid key")
	require.Empty(t, rr.Result().Cookies())
}

func TestLogin_LandingGuidance(t *testing.T) {
	const configPath = "/Users/owner/.config/seamless/seamless.yaml"
	svc, err := New(Config{APIKey: testKey, ConfigPath: configPath})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)

	rr := do(mux, httptest.NewRequest(http.MethodGet, "/console/login", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	require.Contains(t, body, "See what your agents <em>remember</em>.")
	require.Contains(t, body, `data-copy="seamlessd console-open"`)
	require.Contains(t, body, "opens your default browser with the console cookie already set")
	require.Contains(t, body, "https://thereisnospoon.org/docs/quickstart/")
	require.Contains(t, body, "https://thereisnospoon.org/docs/reference/console/")
	require.Contains(t, body, "https://thereisnospoon.org/docs/reference/configuration/")
	require.Contains(t, body, configPath)
	require.Contains(t, body, "vscode://file"+configPath)
	require.Contains(t, body, "mcp.api_key")
	require.Contains(t, body, "SEAMLESS_MCP_API_KEY")
	require.Contains(t, body, "true first run")
	require.Contains(t, body, "never edits an existing config automatically")
}

func TestLogin_CorrectKeySetsCookieAndGrantsAccess(t *testing.T) {
	mux := newTestMux(t)

	form := url.Values{"key": {testKey}, "next": {"/console/"}}
	req := httptest.NewRequest(http.MethodPost, "/console/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := do(mux, req)
	require.Equal(t, http.StatusSeeOther, rr.Code)

	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	require.NotNil(t, cookie, "login must set the console cookie")
	require.Equal(t, consoleToken(testKey), cookie.Value)
	require.True(t, cookie.HttpOnly)
	require.False(t, cookie.Secure, "no TLS here: a Secure cookie over http is dropped by the browser")

	// The cookie now authenticates a page load.
	req2 := httptest.NewRequest(http.MethodGet, "/console/", nil)
	req2.AddCookie(cookie)
	rr2 := do(mux, req2)
	require.Equal(t, http.StatusOK, rr2.Code)
	require.Contains(t, rr2.Body.String(), "Overview")
}

func TestLogout_ClearsCookie(t *testing.T) {
	mux := newTestMux(t)
	req := httptest.NewRequest(http.MethodPost, "/console/logout", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	var found bool
	for _, c := range rr.Result().Cookies() {
		if c.Name == cookieName {
			found = true
			require.True(t, c.MaxAge < 0, "logout must expire the cookie")
		}
	}
	require.True(t, found)
}

func TestConsoleFormBoundary(t *testing.T) {
	mux := newTestMux(t)

	t.Run("known oversized login form", func(t *testing.T) {
		body := strings.Repeat("x", int(formBodySmall)+1)
		req := httptest.NewRequest(http.MethodPost, "/console/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := do(mux, req)
		require.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)
	})

	t.Run("unknown-length oversized login form", func(t *testing.T) {
		body := "key=" + strings.Repeat("x", int(formBodySmall))
		req := httptest.NewRequest(http.MethodPost, "/console/login", strings.NewReader(body))
		req.ContentLength = -1
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := do(mux, req)
		require.Equal(t, http.StatusRequestEntityTooLarge, rr.Code)
	})

	t.Run("malformed encoding", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/console/login", strings.NewReader("key=%zz"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := do(mux, req)
		require.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("wrong content type is not an empty form", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/console/login", strings.NewReader(`{"key":"`+testKey+`"}`))
		req.Header.Set("Content-Type", "application/json")
		rr := do(mux, req)
		require.Equal(t, http.StatusBadRequest, rr.Code)
		require.Empty(t, rr.Result().Cookies())
	})

	t.Run("protected route authenticates before parsing", func(t *testing.T) {
		body := strings.Repeat("x", int(formBodySmall)+1)
		req := httptest.NewRequest(http.MethodPost, "/console/settings/briefing/reset", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rr := do(mux, req)
		require.Equal(t, http.StatusSeeOther, rr.Code)
		require.Contains(t, rr.Header().Get("Location"), "/console/login")
	})
}

func TestServeCSS(t *testing.T) {
	mux := newTestMux(t)
	rr := do(mux, httptest.NewRequest(http.MethodGet, "/console/static/console.css", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Header().Get("Content-Type"), "text/css")
	require.Contains(t, rr.Body.String(), ".sidebar")
}

func TestServeFavicon(t *testing.T) {
	mux := newTestMux(t)
	rr := do(mux, httptest.NewRequest(http.MethodGet, "/console/static/favicon.svg", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Header().Get("Content-Type"), "image/svg+xml")
	require.Contains(t, rr.Body.String(), "<svg")
}

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"/console/memories":   "/console/memories",
		"/console/":           "/console/",
		"https://evil.test/x": "/console/",
		"//evil.test":         "/console/",
		"/console//evil":      "/console/",
		"/etc/passwd":         "/console/",
		"":                    "/console/",
	}
	for in, want := range cases {
		require.Equal(t, want, safeNext(in), "safeNext(%q)", in)
	}
}

func TestGetNavCounts_EmptyDB(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	n, err := store.GetNavCounts(context.Background(), db)
	require.NoError(t, err)
	require.Equal(t, store.NavCounts{}, n)
}

func TestOverview_CoverageInPayload(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// One covered session (has findings), one uncovered (empty).
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: "cov1", Name: "cc/a", Status: core.SessionCompleted,
		Findings: "kept something", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: "cov2", Name: "cc/b", Status: core.SessionCompleted,
		CreatedAt: now, UpdatedAt: now,
	}))

	var data overviewData
	getJSON(t, mux, "/console/?w=all&format=json", &data)

	require.Equal(t, 1, data.Covered)
	require.Equal(t, 2, data.CovTotal)
	require.Equal(t, 50, data.Coverage) // 1 of 2 sessions
	require.Len(t, data.CoverageRows, 4)
	require.Equal(t, "Findings", data.CoverageRows[0].Label)
	require.Equal(t, 1, data.CoverageRows[0].Count)
	require.Equal(t, 50, data.CoverageRows[0].Pct)

	// The all-time window buckets daily, so the trend runs from today's session
	// back to itself: a single local-day bucket reflecting 1 of 2 covered.
	require.NotEmpty(t, data.CoverageTrend)
	today := data.CoverageTrend[len(data.CoverageTrend)-1]
	require.Equal(t, now.Local().Format("Jan 02"), today.Label)
	require.Equal(t, 2, today.Total)
	require.Equal(t, 1, today.Covered)
}

func TestOverview_CoverageTrendEmptyWhenNoSessions(t *testing.T) {
	_, mux := newConsole(t)
	var data overviewData
	getJSON(t, mux, "/console/?format=json", &data)
	require.Nil(t, data.CoverageTrend)
}

// SecureCookies follows config.TLSEnabled: under TLS the session cookie must be
// Secure, and the logout that deletes it must carry the same attribute or the
// browser may keep the original in place.
func TestSecureCookiesUnderTLS(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc, err := New(Config{DB: db, APIKey: testKey, SecureCookies: true})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)

	form := url.Values{"key": {testKey}, "next": {"/console/"}}
	req := httptest.NewRequest(http.MethodPost, "/console/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := do(mux, req)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	for _, c := range rr.Result().Cookies() {
		if c.Name == cookieName {
			require.True(t, c.Secure)
			require.True(t, c.HttpOnly)
		}
	}

	out := httptest.NewRequest(http.MethodPost, "/console/logout", nil)
	out.Header.Set("Authorization", "Bearer "+testKey)
	rr = do(mux, out)
	require.Equal(t, http.StatusSeeOther, rr.Code)
	var found bool
	for _, c := range rr.Result().Cookies() {
		if c.Name == cookieName {
			found = true
			require.True(t, c.Secure, "the delete must match the set, or it may not replace it")
			require.True(t, c.MaxAge < 0)
		}
	}
	require.True(t, found)
}

// The sidebar names the SERVER, not "this machine": the console may be open on
// a different device than the daemon.
func TestSidebarNamesTheServerHost(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc, err := New(Config{DB: db, APIKey: testKey})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(svc.host, "server"), "got %q", svc.host)
	require.NotContains(t, svc.host, "this machine")
}
