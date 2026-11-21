package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/bench"
)

// recallServer stands in for the arm's daemon, answering the UserPromptSubmit
// hook with the given additionalContext.
func recallServer(t *testing.T, additionalContext string, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/hooks/user-prompt-submit", r.URL.Path)
		require.Equal(t, "Bearer "+fixtureKey, r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var got map[string]string
		require.NoError(t, json.Unmarshal(body, &got))
		require.NotEmpty(t, got["prompt"])
		require.NotEmpty(t, got["cwd"])

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"hookSpecificOutput": map[string]string{
				"hookEventName":     "UserPromptSubmit",
				"additionalContext": additionalContext,
			},
		})
	}))
}

// recallArm builds a Seamless-ful arm pointed at srv.
func recallArm(t *testing.T, url string) *arm {
	t.Helper()
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key.txt")
	require.NoError(t, os.WriteFile(keyFile, []byte(fixtureKey+"\n"), 0o600))
	return &arm{
		condition: bench.Condition{Name: "mechanism", Profile: bench.ProfileMechanism, Client: bench.ClientClaude},
		dir:       dir,
		repo:      dir,
		seamless:  true,
		url:       url,
		keyFile:   keyFile,
	}
}

func TestRecallCheck_PassesWhenTheInjectionLands(t *testing.T) {
	srv := recallServer(t, "<seam-recall>Seam has possibly relevant memories:\n- rate-limit-not-in-memory</seam-recall>", http.StatusOK)
	defer srv.Close()

	logPath := filepath.Join(t.TempDir(), bench.AgentLogFile)
	out := recallCheck(context.Background(), recallArm(t, srv.URL), "persist the refresh tokens", logPath)
	require.NoError(t, out.err)
	require.Equal(t, 0, out.exitCode)

	log, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Contains(t, string(log), "user-prompt-submit")
	require.Contains(t, string(log), "<seam-recall>")
}

func TestRecallCheck_FailsWhenNothingIsInjected(t *testing.T) {
	srv := recallServer(t, "", http.StatusOK)
	defer srv.Close()

	out := recallCheck(context.Background(), recallArm(t, srv.URL), "unrelated prompt",
		filepath.Join(t.TempDir(), bench.AgentLogFile))
	require.ErrorContains(t, out.err, "<seam-recall>")
	require.NotEqual(t, 0, out.exitCode)
}

func TestRecallCheck_FailsOnAnErrorStatus(t *testing.T) {
	srv := recallServer(t, "<seam-recall>x</seam-recall>", http.StatusUnauthorized)
	defer srv.Close()

	out := recallCheck(context.Background(), recallArm(t, srv.URL), "prompt",
		filepath.Join(t.TempDir(), bench.AgentLogFile))
	require.ErrorContains(t, out.err, "401")
}

func TestRecallCheck_VanillaArmHasNothingToCheck(t *testing.T) {
	dir := t.TempDir()
	a := &arm{
		condition: bench.Condition{Name: "vanilla", Profile: bench.ProfileVanilla, Client: bench.ClientClaude},
		dir:       dir,
		repo:      dir,
	}
	logPath := filepath.Join(dir, bench.AgentLogFile)
	out := recallCheck(context.Background(), a, "prompt", logPath)
	require.ErrorContains(t, out.err, "needs a Seamless daemon")
	require.FileExists(t, logPath, "the reason is recorded as the run's log, not swallowed")
}
