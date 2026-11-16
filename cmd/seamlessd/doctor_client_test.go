package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/mcp"
)

// writeClientConfig writes a role: client seamless.yaml naming dataDir, points
// $SEAMLESS_CONFIG at it, and moves $HOME into the same throwaway tree so the
// hook, skill and Codex checks inspect the fixture rather than the developer's
// real client configuration.
func writeClientConfig(t *testing.T, dir, serverURL, dataDir string) {
	t.Helper()
	path := filepath.Join(dir, "seamless.yaml")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join([]string{
		"role: client",
		"server_url: " + serverURL,
		"data_dir: " + dataDir,
		"mcp:",
		"  api_key: doctor-client-test-key",
		"",
	}, "\n")), 0o600))
	t.Setenv("SEAMLESS_CONFIG", path)
	t.Setenv("HOME", dir)
	t.Setenv("CODEX_HOME", filepath.Join(dir, ".codex"))
}

// The bug this pins: doctor opened the database unconditionally, and store.Open
// CREATES and migrates. On a client that is not a useless check but a
// destructive one -- it mints the seam.db whose absence is what "role: client"
// means, after which every later run finds a database and reports it healthy,
// and an owner looking for their corpus finds an empty one on the wrong machine.
//
// data_dir is named explicitly (rather than left to the default) so the
// assertion is about the path doctor would have used, not about a directory that
// happens not to exist.
func TestDoctor_ClientRoleOpensNoDatabase(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "seamless-data")
	// Port 1 is reserved and nothing listens there: the reachability check fails
	// without a live server, which is also the state a client is most often
	// diagnosed in.
	writeClientConfig(t, dir, "http://127.0.0.1:1", dataDir)

	var runErr error
	out := captureStdout(t, func() error {
		runErr = doctor(nil)
		return nil
	})

	require.NoDirExists(t, dataDir, "a client doctor must not create the data dir")
	require.NoFileExists(t, filepath.Join(dataDir, "seam.db"))
	// The whole tree, not just the configured path: nothing anywhere may be a
	// database this run brought into existence.
	require.NoError(t, filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		require.NotContains(t, filepath.Base(path), "seam.db", "doctor created a database at %s", path)
		return nil
	}))

	// The unreachable server is the failure, and it is reported as one: on a
	// client there is no benign "the daemon is just stopped" reading.
	require.Error(t, runErr)
	require.Contains(t, out, "[info] role: client: this install runs no daemon and dials http://127.0.0.1:1")
	require.Contains(t, out, "[fail] server_url: http://127.0.0.1:1 is not answering")
}

// The skip list, asserted by absence. Each of these reads the local seam.db or
// describes daemon-side work, so a client that printed any of them would be
// reporting on a machine that is somewhere else.
func TestDoctor_ClientRoleSkipsTheServerOnlyChecks(t *testing.T) {
	dir := t.TempDir()
	writeClientConfig(t, dir, "http://127.0.0.1:1", filepath.Join(dir, "seamless-data"))

	out := captureStdout(t, func() error {
		_ = doctor(nil)
		return nil
	})

	for _, absent := range []string{
		"database:", "schema version:", "repo map:", "remote sessions:",
		"feature skills:", "gardener:", "data_dir:", "embedder:", "llm:",
		"bind:", "tls:",
	} {
		require.NotContains(t, out, absent, "a client install has no %q to report", absent)
	}

	// And the checks it DOES run, so the test cannot be satisfied by a doctor
	// that prints nothing: config, the key, reachability, the tool count, and
	// the client-side hook wiring.
	for _, present := range []string{"config:", "mcp.api_key:", "server_url:", "mcp_tools:", "hooks:"} {
		require.Contains(t, out, present)
	}
}

// clientChecks is the list, independent of config.Load: it must never reach for
// a database handle, which is what makes the skip structural rather than a guard
// each future check has to remember.
func TestClientChecks_TakesNoDatabase(t *testing.T) {
	dir := t.TempDir()
	writeClientConfig(t, dir, "http://127.0.0.1:1", filepath.Join(dir, "seamless-data"))

	cfg, err := config.Load()
	require.NoError(t, err)
	require.True(t, cfg.IsClient())

	checks := clientChecks(cfg)
	require.NotEmpty(t, checks)
	names := make([]string, 0, len(checks))
	for _, c := range checks {
		names = append(names, c.name)
	}
	require.Contains(t, names, "role")
	require.Contains(t, names, "server_url")
	require.Contains(t, names, "mcp.api_key")
	require.NotContains(t, names, "database")
	require.NotContains(t, names, "codex hook activity", "the event log lives on the server")
}

// serverFor stands up a fake Seamless server for the client-role mcp_tools
// check: the REAL MCP handler at /api/mcp (so tools/list returns the registry
// this binary would actually serve, filtered by the real feature gate) plus a
// console settings endpoint answering settingsBody. An empty settingsBody omits
// the endpoint entirely, which is the pre-features / unreadable-state case.
//
// It returns a config pointing at it, with key as mcp.api_key.
func serverFor(t *testing.T, key, settingsBody string) config.Config {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/api/mcp", mcp.New(mcp.Config{APIKey: key}).Handler())
	if settingsBody != "" {
		mux.HandleFunc("/console/settings", func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "Bearer "+key, r.Header.Get("Authorization"),
				"the feature read must present the bearer key")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(settingsBody))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := config.Defaults()
	cfg.Addr = strings.TrimPrefix(srv.URL, "http://")
	cfg.MCP.APIKey = key
	return cfg
}

// The deferred half of the task, landed: the client-role line is the count the
// SERVER exposes, obtained by dialing it, not the count this binary registers.
//
// It is also the feature-aware form. Optional features ship OFF, so the real MCP
// handler hides their tools from tools/list and the exposed count is legitimately
// lower than mcp.ToolCount -- the state a fresh install is in, which a bare
// equality against the registered count would have reported as a failure.
func TestClientMCPToolsCheck_CountsWhatTheServerExposes(t *testing.T) {
	cfg := serverFor(t, "client-doctor-key", `{"featuresConfig":{"research":false}}`)
	exposed := mcp.ToolCount - len(features.ToolOwners())

	got := clientMCPToolsCheck(cfg)
	require.Equal(t, statusOK, got.status, got.detail)
	require.Equal(t, "mcp_tools", got.name)
	require.Equal(t,
		fmt.Sprintf("%d registered, %d exposed (research, repo_locks disabled)", mcp.ToolCount, exposed),
		got.detail)
	require.NotContains(t, got.detail, "tools registered",
		"the registration-count line is the SERVER role's; a client must report the live one")
}

// With every optional feature on, the server exposes the full surface and the
// line is the single-number form. Proves the check is not a floor that passes on
// anything below the registered count.
func TestClientMCPToolsCheck_AllFeaturesOnExpectsTheFullSurface(t *testing.T) {
	// The handler's own gate reads mcp.Config.Features, so both halves have to
	// agree for this to be the all-on case rather than a mismatch.
	mux := http.NewServeMux()
	on := features.Defaults()
	for _, f := range features.Registry() {
		f.Set(&on, true)
	}
	mux.Handle("/api/mcp", mcp.New(mcp.Config{APIKey: "k", Features: on}).Handler())
	mux.HandleFunc("/console/settings", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"featuresConfig":{"research":true,"momentum":true,"gamification":true,"repo_locks":true}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cfg := config.Defaults()
	cfg.Addr = strings.TrimPrefix(srv.URL, "http://")
	cfg.MCP.APIKey = "k"

	got := clientMCPToolsCheck(cfg)
	require.Equal(t, statusOK, got.status, got.detail)
	require.Equal(t, fmt.Sprintf("%d tools (expected %d)", mcp.ToolCount, mcp.ToolCount), got.detail)
}

// A server that is not there is a FAIL with a reason a human can act on, not a
// panic and not a silent pass. On a client the server IS the install.
func TestClientMCPToolsCheck_UnreachableServerFails(t *testing.T) {
	cfg := config.Defaults()
	cfg.Addr = "127.0.0.1:1" // reserved; nothing listens there
	cfg.MCP.APIKey = "irrelevant"

	got := clientMCPToolsCheck(cfg)
	require.Equal(t, statusFail, got.status)
	require.Contains(t, got.detail, "tools/list failed against http://127.0.0.1:1")
	require.Contains(t, got.detail, "seamlessd is running there")
	require.Contains(t, got.detail, "mcp.api_key")
}

// The other failure a client cannot diagnose any other way: the server answers,
// but refuses this install's key. Nothing else in the client report presents a
// credential, so without this line a wrong key reads as a healthy machine.
func TestClientMCPToolsCheck_WrongKeyFails(t *testing.T) {
	cfg := serverFor(t, "the-real-key", `{"featuresConfig":{"research":false}}`)
	cfg.MCP.APIKey = "not-the-real-key"

	got := clientMCPToolsCheck(cfg)
	require.Equal(t, statusFail, got.status)
	require.Contains(t, got.detail, "mcp.api_key matches the server's key")
}

// Failure-soft on the SECOND dependency: the tool surface answered, only the
// feature state did not. That leaves a range rather than a number, and a healthy
// daemon must not fail because one console endpoint was unreadable.
func TestClientMCPToolsCheck_UnreadableFeatureStateFallsBackToTheRange(t *testing.T) {
	cfg := serverFor(t, "client-doctor-key", "") // no console settings endpoint
	low := mcp.ToolCount - len(features.ToolOwners())

	got := clientMCPToolsCheck(cfg)
	require.Equal(t, statusOK, got.status, got.detail)
	require.Contains(t, got.detail, "feature state unreadable")
	require.Contains(t, got.detail, fmt.Sprintf("expected %d-%d", low, mcp.ToolCount))
}

// A gap the feature arithmetic cannot explain still fails, and the line names the
// two causes that produce it -- so an operator on a mixed-version fleet is not
// left reading a bare number mismatch as a broken tool gate.
func TestClientMCPToolsCheck_UnexplainableGapFailsAndNamesVersionSkew(t *testing.T) {
	// The server hides the research tools; the console claims research is ON, so
	// the expected count is the full surface and the live one is short of it.
	cfg := serverFor(t, "k", `{"featuresConfig":{"research":true,"repo_locks":true}}`)

	got := clientMCPToolsCheck(cfg)
	require.Equal(t, statusFail, got.status)
	require.Contains(t, got.detail, fmt.Sprintf("expected %d", mcp.ToolCount))
	require.Contains(t, got.detail, "different versions")
}

// The list wiring, not the check: clientChecks must carry the LIVE line. The
// registration count is feature-independent by construction, so a client that
// still ran mcpToolsCheck would print a confident "ok" against a server it never
// contacted -- which is the narrowness this task existed to close.
func TestClientChecks_MCPToolsIsTheLiveCount(t *testing.T) {
	dir := t.TempDir()
	writeClientConfig(t, dir, "http://127.0.0.1:1", filepath.Join(dir, "seamless-data"))

	cfg, err := config.Load()
	require.NoError(t, err)
	require.True(t, cfg.IsClient())

	var tools check
	for _, c := range clientChecks(cfg) {
		if c.name == "mcp_tools" {
			tools = c
		}
	}
	require.Equal(t, "mcp_tools", tools.name, "the client report must still carry an mcp_tools line")
	require.Equal(t, statusFail, tools.status, "nothing answers at port 1, so the live count cannot be taken")
	require.NotContains(t, tools.detail, "tools registered")
}
