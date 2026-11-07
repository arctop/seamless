package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/features"
)

// optionalTools is how many MCP tools all optional features own together -- the
// widest legitimate gap between the registered and exposed counts. Derived, so
// this file keeps saying something true when a second optional feature lands.
func optionalTools() int { return len(features.ToolOwners()) }

// settingsServer stands up a console that answers the settings JSON with body,
// and returns the config pointing at it. It asserts the request doctor makes is
// the authenticated JSON one -- the plumbing in client.go, not a hand-rolled
// request that would skip the bearer key.
func settingsServer(t *testing.T, code int, body string) config.Config {
	t.Helper()
	const key = "doctor-test-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/console/settings", r.URL.Path)
		require.Equal(t, "json", r.URL.Query().Get("format"))
		require.Equal(t, "Bearer "+key, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	cfg := config.Defaults()
	cfg.Addr = strings.TrimPrefix(srv.URL, "http://")
	cfg.MCP.APIKey = key
	return cfg
}

// The common case, not the exception: optional features ship OFF, so a fresh
// install exposes fewer tools than are registered and doctor must still pass.
// The old check asserted `n == expectedTools` and would have failed every
// default install out of the box.
func TestToolsCheck_DefaultOffPassesAndNamesTheFeature(t *testing.T) {
	cfg := settingsServer(t, http.StatusOK, `{"featuresConfig":{"research":false},"featuresOverridden":false}`)

	ok, detail := toolsCheck(cfg, expectedTools-optionalTools(), false)
	require.True(t, ok, "a default daemon must pass doctor: %s", detail)
	require.Equal(t,
		fmt.Sprintf("%d registered, %d exposed (research disabled)", expectedTools, expectedTools-optionalTools()),
		detail)
}

// The other half: with the feature on, the full surface is expected and the line
// stays the single-number form it has always been.
func TestToolsCheck_ResearchOnExpectsTheFullSurface(t *testing.T) {
	cfg := settingsServer(t, http.StatusOK, `{"featuresConfig":{"research":true}}`)

	ok, detail := toolsCheck(cfg, expectedTools, false)
	require.True(t, ok, detail)
	require.Equal(t, fmt.Sprintf("%d tools (expected %d)", expectedTools, expectedTools), detail)

	// And the reduced count is now WRONG, so the check has not been softened into
	// one that passes on anything.
	ok, detail = toolsCheck(cfg, expectedTools-optionalTools(), false)
	require.False(t, ok)
	require.Contains(t, detail, fmt.Sprintf("expected %d", expectedTools))
}

// A daemon that locks a project also serves access_request: its list is judged
// against one more registered tool, and the plain count is then one short.
func TestToolsCheck_ALockingDaemonServesAccessRequestToo(t *testing.T) {
	cfg := settingsServer(t, http.StatusOK, `{"featuresConfig":{"research":true}}`)

	ok, detail := toolsCheck(cfg, expectedTools+1, true)
	require.True(t, ok, detail)
	require.Equal(t, fmt.Sprintf("%d tools (expected %d)", expectedTools+1, expectedTools+1), detail)

	ok, _ = toolsCheck(cfg, expectedTools, true)
	require.False(t, ok, "a locking daemon missing a tool still fails")
}

// A daemon with the feature off that exposes its tools anyway is a real failure:
// the gate is not firing. The reduced expectation is an expectation, not a floor.
func TestToolsCheck_FeatureOffButToolsExposedFails(t *testing.T) {
	cfg := settingsServer(t, http.StatusOK, `{"featuresConfig":{"research":false}}`)

	ok, detail := toolsCheck(cfg, expectedTools, false)
	require.False(t, ok)
	require.Equal(t,
		fmt.Sprintf("%d registered, %d exposed, expected %d with research disabled",
			expectedTools, expectedTools, expectedTools-optionalTools()),
		detail)
}

// Failure-soft: the settings endpoint is a second dependency of what used to be
// a self-contained check, so losing it must not fail an otherwise healthy
// daemon. Without the feature state there is no single expected number, only the
// range between "everything optional off" and "everything on" -- doctor judges
// that range and says out loud that it could not read the state, rather than
// asserting a number it does not know.
func TestToolsCheck_UnreadableFeatureStateFallsBackToTheRange(t *testing.T) {
	low := expectedTools - optionalTools()

	cases := []struct {
		name string
		cfg  func(t *testing.T) config.Config
		want string // fragment naming the reason
	}{
		{
			name: "console error",
			cfg:  func(t *testing.T) config.Config { return settingsServer(t, http.StatusInternalServerError, `boom`) },
			want: "console returned 500",
		},
		{
			// The pre-features daemon: the field is simply absent. Decoding that
			// into a value would read as "every optional feature off" and quietly
			// lower the expectation on a daemon exposing all of them.
			name: "featuresConfig absent",
			cfg:  func(t *testing.T) config.Config { return settingsServer(t, http.StatusOK, `{"dataDir":"/tmp/x"}`) },
			want: "carries no featuresConfig",
		},
		{
			name: "console unreachable",
			cfg: func(t *testing.T) config.Config {
				cfg := config.Defaults()
				cfg.Addr = "127.0.0.1:1" // nothing listens on port 1
				return cfg
			},
			want: "console unreachable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg(t)

			for _, live := range []int{low, expectedTools} {
				ok, detail := toolsCheck(cfg, live, false)
				require.True(t, ok, "%d is inside the range: %s", live, detail)
				require.Contains(t, detail, "feature state unreadable")
				require.Contains(t, detail, tc.want)
				require.Contains(t, detail, fmt.Sprintf("expected %d-%d", low, expectedTools))
			}

			// Soft, not blind: a count outside the range cannot be explained by any
			// feature state, so it still fails.
			ok, detail := toolsCheck(cfg, low-1, false)
			require.False(t, ok, detail)
			ok, _ = toolsCheck(cfg, expectedTools+1, false)
			require.False(t, ok)
		})
	}
}

// The gap arithmetic itself now lives in internal/features
// (features.ToolCountVerdict, shared with the client-role `seamlessd doctor`)
// and is tested there against the registry. What stays here is the wiring: that
// this command feeds it expectedTools and the state read from the console.
func TestToolsCheck_FeedsTheRegisteredCountToTheSharedVerdict(t *testing.T) {
	cfg := settingsServer(t, http.StatusOK, `{"featuresConfig":{"research":false}}`)
	feats := config.Features{}

	ok, detail := toolsCheck(cfg, expectedTools-optionalTools(), false)
	wantOK, wantDetail := features.ToolCountVerdict(expectedTools, expectedTools-optionalTools(), &feats, "")
	require.Equal(t, wantOK, ok)
	require.Equal(t, wantDetail, detail, "toolsCheck must be the shared verdict, not a second copy of it")
}

// doctor's aggregate shape, at the one depth a unit test can reach: the checks
// that answered are printed, the one that did not is named, and the command
// exits non-zero. The tool-count check itself is covered above rather than here
// because e.dial hands back a concrete *mcpclient.Client -- faking a WORKING one
// means standing up a real MCP server, which no test in this package does (see
// status_test.go's note).
func TestDoctor_ReportsEachCheckAndFails(t *testing.T) {
	e, out, errb := healthzOnly(t, `{"status":"ok","version":"test"}`)

	require.Equal(t, 1, dispatch(context.Background(), e, []string{"doctor"}))
	require.Contains(t, out.String(), "[ok  ] server: ok")
	require.Contains(t, out.String(), "[FAIL] mcp: connect failed: connection refused")
	require.Contains(t, errb.String(), "doctor: 1 check(s) failed")

	// The server line names the URL that was actually resolved, not just the
	// status word. On a client install that URL is server_url rather than
	// anything derived from a local addr, and it is the single fact that tells
	// an operator whether this machine is pointed where they think it is.
	cfg, err := e.loadConfig()
	require.NoError(t, err)
	require.Contains(t, out.String(), "server: ok ("+cfg.ServerURL()+")")
}

// `seam doctor` takes no arguments.
func TestDoctor_TakesNoArguments(t *testing.T) {
	_, err := parse(commands(), []string{"doctor", "extra"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "takes no positional arguments")
}
