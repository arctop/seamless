package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// The hooks' provenance stamps, proved against the APIs the previous commit
// already has (plan fenced-sessions P6): a conversation native to a
// confidential project, holding an approved read of another, captures a plan
// and ends; its plan note's frontmatter and the index name the grant for the
// note, the tracking task and the harvest. Nothing here names what the stamps
// added, so the file builds before them and fails there for that reason.
func TestP6BaseProof_HookRecordsAreStamped(t *testing.T) {
	const legalCWD = "/work/legal-proof"
	e := newRouteEnvWith(t, func(c *Config) { c.ReadGrants = true })
	ctx := context.Background()
	require.NoError(t, store.SetSetting(ctx, e.db, store.SettingRepoProjectMap,
		`{"`+rtOpsCWD+`":"`+rtOps+`","`+legalCWD+`":"`+rtLegal+`"}`))
	amb := e.start(ClientClaudeCode, legalCWD)
	require.Equal(t, rtLegal, amb.ProjectSlug)
	now := time.Now().UTC()
	a, err := store.CreateGrantRequest(ctx, e.db, store.GrantRequest{Project: rtFin, From: rtLegal, Lease: time.Hour,
		TTL: 10 * time.Minute, SessionID: amb.ID, SessionName: amb.Name,
		ConversationClient: ClientClaudeCode.externalIdentity(), ConversationID: rtConv}, now)
	require.NoError(t, err)
	_, err = store.GrantAccessApprove(ctx, e.db, a.ID, a.Challenge, "owner", 0, store.GrantAccessRead, now)
	require.NoError(t, err)

	plan := filepath.Join(e.plansDir, "proof-ledger.md")
	require.NoError(t, os.WriteFile(plan, []byte("# Example ledger review\n\n1. "+rtSecret+" step one.\n"), 0o644))
	for _, body := range []map[string]any{
		{"session_id": rtConv, "cwd": legalCWD, "tool_name": "Write", "tool_input": map[string]any{"file_path": plan}},
		{"session_id": rtConv, "cwd": legalCWD, "tool_name": "ExitPlanMode", "tool_response": map[string]any{"filePath": plan}},
	} {
		_, refused := e.hook(ClientClaudeCode, "post-tool-use", body)
		require.Empty(t, refused)
	}
	require.Empty(t, e.end())

	rows := func(kind string) int {
		var n int
		require.NoError(t, e.db.QueryRow(`SELECT count(*) FROM item_provenance WHERE item_kind = ? AND approval_id = ?`,
			kind, a.ID).Scan(&n))
		return n
	}
	idx, ok, err := store.NoteBySlug(ctx, e.db, rtLegal, plans.NotePrefix+"proof-ledger")
	require.NoError(t, err)
	require.True(t, ok)
	raw, err := os.ReadFile(filepath.Join(e.mgr.Store().DataDir(), filepath.FromSlash(idx.FilePath)))
	require.NoError(t, err)
	require.True(t, strings.Contains(string(raw), "may_contain:\n    - project: "+rtFin+"\n      grant: "+a.ID+"\n"),
		"the plan note's frontmatter names the grant:\n%s", raw)
	require.Equal(t, 1, rows(core.ItemKindNote), "the plan note, mirrored")
	require.Equal(t, 1, rows(core.ItemKindTask), "the plan's tracking task")
	require.Equal(t, 1, rows(core.ItemKindSession), "the SessionEnd harvest")
}
