package mcp_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// A session_start briefing carries the repo-lock lines of the checkout the
// call names, as a SessionStart hook's briefing does, and records the
// session's lock aliases, so a client without hooks is told about the lock and
// counts as a live peer of the repository.
func TestSessionStart_BriefingShowsTheCheckoutsLocks(t *testing.T) {
	ctx := context.Background()
	url, db := newServer(t)
	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{RepoLocks: true}))
	owner := dialClient(t, ctx, url, testKey)
	callJSON(t, ctx, owner, "locks_declare", lkCheckout(nil))

	start := func(name string, checkout map[string]any) string {
		t.Helper()
		args := map[string]any{"name": name, "project": "demo"}
		for k, v := range checkout {
			args[k] = v
		}
		out := callJSON(t, ctx, dialClient(t, ctx, url, testKey), "session_start", args)
		briefing, _ := out["briefing"].(string)
		return briefing
	}

	first := start("lk-ss-a", lkCheckout(nil))
	require.Contains(t, first, "LOCK app main: free")
	require.NotContains(t, first, "Also live in this repository", "no other session is in the repository yet")

	second := start("lk-ss-b", lkCheckout(nil))
	require.Contains(t, second, "LOCK app main: free")
	require.Contains(t, second, "Also live in this repository: lk-ss-a",
		"the first session's own session_start recorded its repository")

	other := start("lk-ss-c", map[string]any{"host": "example-laptop", "repo_root": "/work/other",
		"repo_origin": "git@github.com:example/other.git"})
	require.NotContains(t, other, "LOCK", "a checkout of another repository has no lock line")

	require.NoError(t, store.SetFeaturesConfig(ctx, db, config.Features{}))
	off := start("lk-ss-d", lkCheckout(nil))
	require.NotContains(t, off, "LOCK", "repo locks off: no lock line")
}
