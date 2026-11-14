package hooks

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// Captures meet the clearance rule (isolation.requests): what the hooks record
// for a conversation is a write on its behalf, judged for the conversation the
// hook names -- every session of it, never only its ambient row.

// clPlan captures a plan from cwd for the fixture conversation, and returns
// the refusal notice (harvest.RefusedHeader).
func clPlan(t *testing.T, e *routeEnv, cwd, base string) string {
	t.Helper()
	plan := filepath.Join(e.plansDir, base+".md")
	require.NoError(t, os.WriteFile(plan, []byte("# Example ledger review\n\n1. "+rtSecret+" step one.\n"), 0o644))
	_, refused := e.hook(ClientClaudeCode, "post-tool-use", map[string]any{
		"session_id": rtConv, "cwd": cwd, "tool_name": "Write", "tool_input": map[string]any{"file_path": plan},
	})
	return refused
}

func clCaptured(t *testing.T, e *routeEnv, project, base string) bool {
	t.Helper()
	_, ok, err := store.NoteBySlug(context.Background(), e.db, project, plans.NotePrefix+base)
	require.NoError(t, err)
	return ok
}

// A conversation native to a confidential project whose hook fires from an
// open project's directory: with the switch on its capture goes to its home,
// as everything it records does (home.go), and nothing lands outside it; off,
// it lands by cwd, as before.
func TestClearanceCapture_NativeConversationOutsideItsHome(t *testing.T) {
	for _, on := range []bool{true, false} {
		e := newRouteEnvWith(t, func(c *Config) { c.ReadGrants = on })
		e.start(ClientClaudeCode, rtFinCWD)
		require.Empty(t, clPlan(t, e, rtOpsCWD, "away"))
		require.Equal(t, !on, clCaptured(t, e, rtOps, "away"), "off: the cwd's project")
		require.Equal(t, on, clCaptured(t, e, rtFin, "away"), "on: its home")
		require.Empty(t, clPlan(t, e, rtFinCWD, "home"))
		require.True(t, clCaptured(t, e, rtFin, "home"), "its home takes it, switch on or off")
	}
}

// P0's native-home rule holds with the switch on: a fenced home keeps the
// captures of a conversation whose own sessions reach other fenced projects.
func TestClearanceCapture_NativeHomeKeepsCaptures(t *testing.T) {
	e := newRouteEnvWith(t, func(c *Config) { c.ReadGrants = true })
	e.start(ClientClaudeCode, rtFinCWD)
	e.bind(ClientClaudeCode, rtLegal, core.SessionActive, "")
	e.bind(ClientClaudeCode, rtSealed, core.SessionCompleted, "")
	require.Empty(t, clPlan(t, e, rtFinCWD, "home"))
	require.True(t, clCaptured(t, e, rtFin, "home"))
}

// The rebind hole the P1 handoff named: a grant held by a session the
// conversation linked with session_start is the conversation's, so its
// captures obey it too -- not only the ambient row's grants.
func TestClearanceCapture_ALinksGrantIsTheConversations(t *testing.T) {
	e := newRouteEnvWith(t, func(c *Config) { c.ReadGrants = true })
	e.start(ClientClaudeCode, rtOpsCWD)
	link := e.bind(ClientClaudeCode, rtDocs, core.SessionActive, "")
	approveGrant(t, &captureEnv{db: e.db}, rtDocs, rtLegal, link.ID, "")
	require.Equal(t, "Seamless did not record this plan: this session holds content from example-legal, so it writes "+
		"only into example-legal, and this would land in project example-ops.", clPlan(t, e, rtOpsCWD, "away"))
	require.False(t, clCaptured(t, e, rtOps, "away"))
}
