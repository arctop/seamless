package retrieve

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// insScopedMem is insMem for a name that lives in more than one scope: each
// row gets its own file path, and the account the file's frontmatter carries.
func insScopedMem(t *testing.T, db *sql.DB, id, kind, name, desc, project, account string) {
	t.Helper()
	ctx := context.Background()
	stamp := core.FormatTime(time.Now().UTC())
	scope := project
	if scope == "" {
		scope = "_global"
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO memories_index
		    (id, kind, name, description, project, file_path, tags, valid_from,
		     invalid_at, superseded_by, source_session, content_hash, created_at, updated_at, account)
		VALUES (?, ?, ?, ?, ?, ?, '[]', ?, NULL, NULL, '', 'h', ?, ?, ?)`,
		id, kind, name, desc, project, "memory/"+scope+"/"+name+".md", stamp, stamp, stamp, account)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO fts (item_id, kind, project, title, name, description, body)
		VALUES (?, 'memory', ?, '', ?, ?, ?)`, id, project, name, desc, desc)
	require.NoError(t, err)
}

// A restricted account's project memory steps aside for the live global rule
// of its name; nothing else changes how a by-name read resolves.
func TestMemoryByNameRead(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	const acct = "_example_agent"
	// The account's memory came first; the owner's global rule arrived after.
	insScopedMem(t, db, "01ACCTRULE", "gotcha", "deploy-rule", "the account's", "seam", acct)
	insScopedMem(t, db, "01GLOBRULE", "constraint", "deploy-rule", "the owner's rule", "", "")
	// The owner's own project-local override.
	insScopedMem(t, db, "01OWNRULE", "gotcha", "own-rule", "the owner's override", "seam", "")
	insScopedMem(t, db, "01GLOBOWN", "constraint", "own-rule", "the owner's rule", "", "")
	// A global memory of a kind no briefing injects.
	insScopedMem(t, db, "01ACCTNOTE", "gotcha", "cache-note", "the account's", "seam", acct)
	insScopedMem(t, db, "01GLOBNOTE", "gotcha", "cache-note", "a global gotcha", "", "")
	// A global rule that has since been superseded is no rule.
	insScopedMem(t, db, "01ACCTOLD", "gotcha", "old-rule", "the account's", "seam", acct)
	insScopedMem(t, db, "01GLOBOLD", "constraint", "old-rule", "retired", "", "")
	_, err := db.ExecContext(ctx, `UPDATE memories_index SET invalid_at = ? WHERE id = '01GLOBOLD'`,
		core.FormatTime(time.Now().UTC()))
	require.NoError(t, err)
	// Only the global scope holds this one.
	insScopedMem(t, db, "01GLOBONLY", "constraint", "only-global", "the owner's rule", "", "")

	for _, kind := range []string{"constraint", "convention", "protocol", "stage"} {
		t.Run("briefing kind "+kind, func(t *testing.T) {
			_, err := db.ExecContext(ctx, `UPDATE memories_index SET kind = ? WHERE id = '01GLOBRULE'`, kind)
			require.NoError(t, err)
			r, err := MemoryByNameRead(ctx, db, "seam", "deploy-rule", true)
			require.NoError(t, err)
			require.True(t, r.Found)
			require.Equal(t, "01GLOBRULE", r.Memory.ID, "the global rule answers")
			require.Equal(t, "01ACCTRULE", r.Passed.ID, "the account's memory is named as passed over")
		})
	}

	for _, tc := range []struct {
		name, project, lookup string
		globalVisible         bool
		want                  string
	}{
		{"the owner's project memory still shadows", "seam", "own-rule", true, "01OWNRULE"},
		{"a non-briefing global kind does not trigger it", "seam", "cache-note", true, "01ACCTNOTE"},
		{"an inactive global rule does not trigger it", "seam", "old-rule", true, "01ACCTOLD"},
		{"a reader that may not read global keeps the project memory", "seam", "deploy-rule", false, "01ACCTRULE"},
		{"a miss in the project falls back to global", "seam", "only-global", true, "01GLOBONLY"},
		{"the global scope read alone", "", "deploy-rule", true, "01GLOBRULE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := MemoryByNameRead(ctx, db, tc.project, tc.lookup, tc.globalVisible)
			require.NoError(t, err)
			require.True(t, r.Found)
			require.Equal(t, tc.want, r.Memory.ID)
			require.Empty(t, r.Passed.ID, "nothing passed over")
		})
	}

	r, err := MemoryByNameRead(ctx, db, "seam", "only-global", false)
	require.NoError(t, err)
	require.False(t, r.Found, "no global fallback for a reader that may not read it")

	g, ok, err := ShadowedRule(ctx, db, core.Memory{ID: "01GLOBRULE", Name: "deploy-rule", Account: acct})
	require.NoError(t, err)
	require.False(t, ok, "a global memory shadows nothing")
	require.Empty(t, g.ID)
}

// Recall's [[name]] link expansion is a by-name read: a link to a rule's name
// pulls in the rule, never a restricted account's project memory in its place.
func TestRecall_LinkExpansion_RestrictedShadow(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		account, globalKind    string
		wantLinked, wantAbsent string
	}{
		{"a restricted account's memory steps aside", "_example_agent", "constraint", "01GLOBNB", "01PROJNB"},
		{"the owner's project memory still answers", "", "constraint", "01PROJNB", "01GLOBNB"},
		{"a non-briefing global kind does not trigger it", "_example_agent", "gotcha", "01PROJNB", "01GLOBNB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupDB(t)
			ctx := context.Background()
			insScopedMem(t, db, "01HIT", "gotcha", "readiness-gate", "readiness gate fix", "seam", "")
			insScopedMem(t, db, "01PROJNB", "gotcha", "chroma-boot-race", "chroma boot race", "seam", tc.account)
			insScopedMem(t, db, "01GLOBNB", tc.globalKind, "chroma-boot-race", "chroma boot race", "", "")
			svc := New(db, nil, budgets(), nil)
			svc.SetBodyReader(fakeBodyReader{
				"memory/seam/readiness-gate.md": "The fix: see [[chroma-boot-race]] for details.",
			})
			hits, err := svc.Recall(ctx, RecallInput{Query: "readiness gate", Project: "seam", Limit: 10})
			require.NoError(t, err)
			source := map[string]string{}
			for _, h := range hits {
				source[h.ID] = h.Source
			}
			require.Equal(t, "link", source[tc.wantLinked])
			require.NotContains(t, source, tc.wantAbsent)
		})
	}
}
