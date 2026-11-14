package mcp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// Provenance stamps, proved against the APIs the previous commit already has
// (plan fenced-sessions P6): nothing here names a type or a function the
// stamps added, so the same file builds before them and fails there for the
// reason it exists -- an item written while the conversation holds a grant
// carried no stamp.

type p6ProofRig struct {
	t   *testing.T
	db  *sql.DB
	mgr *files.Manager
	dir string
	url string
	now time.Time
}

func newP6ProofRig(t *testing.T) *p6ProofRig {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	for slug, state := range map[string]core.Isolation{"example-finance": core.IsolationConfidential,
		"example-legal": core.IsolationConfidential} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, state))
	}
	r := &p6ProofRig{t: t, db: db, mgr: mgr, dir: dir, now: time.Now().UTC().Truncate(time.Second)}
	rec := events.NewRecorder(db)
	ret := retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 4000}, nil)
	ret.SetBodyReader(mgr.Store())
	grants := approval.NewGrants(approval.GrantsConfig{DB: db, Events: rec, LeaseCap: 8 * time.Hour,
		Now: func() time.Time { return r.now }})
	ts := httptest.NewServer(mcpserver.New(mcpserver.Config{DB: db, Files: mgr, Retrieve: ret, Events: rec,
		APIKey: testKey, Grants: grants, Now: func() time.Time { return r.now }}).Handler())
	t.Cleanup(ts.Close)
	r.url = ts.URL
	return r
}

func (r *p6ProofRig) call(cli *mcpclient.Client, name string, args map[string]any) map[string]any {
	r.t.Helper()
	res, err := cli.CallTool(context.Background(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
	require.NoError(r.t, err)
	text := res.Content[0].(mcp.TextContent).Text
	require.False(r.t, res.IsError, "%s: %s", name, text)
	var out map[string]any
	require.NoError(r.t, json.Unmarshal([]byte(text), &out))
	return out
}

func (r *p6ProofRig) rows(kind, id string) int {
	r.t.Helper()
	var n int
	require.NoError(r.t, r.db.QueryRow(`SELECT count(*) FROM item_provenance WHERE item_kind = ? AND item_id = ?`,
		kind, id).Scan(&n))
	return n
}

// p6Grantee is a conversation homed in a confidential project holding an
// approved read of another, and the grant's approval id.
func (r *p6ProofRig) grantee() (*mcpclient.Client, string, string) {
	r.t.Helper()
	ctx := context.Background()
	amb := seedAgentAmbient(r.t, ctx, r.db, "cc/p6-proof", "example-legal", "/work/example-legal", "7171.42")
	agent := dialAgent(r.t, ctx, r.url, "7171.42")
	a, err := store.CreateGrantRequest(ctx, r.db, store.GrantRequest{Project: "example-finance", From: "example-legal",
		Lease: time.Hour, TTL: 10 * time.Minute, SessionID: amb, SessionName: "cc/p6-proof",
		ConversationClient: "claude-code", ConversationID: "cc/p6-proof-full"}, r.now)
	require.NoError(r.t, err)
	_, err = store.GrantAccessApprove(ctx, r.db, a.ID, a.Challenge, "owner", 0, store.GrantAccessRead, r.now)
	require.NoError(r.t, err)
	return agent, amb, a.ID
}

// TestP6BaseProof_AGrantHoldersWritesAreStamped: a conversation homed in a
// confidential project, holding an approved read of another, writes in its
// home. Each item records that it may contain the other project's content:
// in the file's frontmatter, in the index, in the read answers and in
// recall's hits; and a supersession keeps the union.
func TestP6BaseProof_AGrantHoldersWritesAreStamped(t *testing.T) {
	t.Run("memory frontmatter, index and read", func(t *testing.T) {
		r := newP6ProofRig(t)
		agent, _, approvalID := r.grantee()
		mem := r.call(agent, "memory_write", map[string]any{"name": "proof-rule", "kind": "decision",
			"description": "the ledger proof", "body": "Reconcile the ledger first.\n"})
		raw, err := os.ReadFile(filepath.Join(r.dir, "memory", "example-legal", "proof-rule.md"))
		require.NoError(t, err)
		require.Contains(t, string(raw), "may_contain:\n    - project: example-finance\n      grant: "+approvalID+"\n",
			"the memory's frontmatter names the grant whose content it may hold")
		require.Equal(t, 1, r.rows(core.ItemKindMemory, mem["id"].(string)), "mirrored into item_provenance")
		read := r.call(agent, "memory_read", map[string]any{"name": "proof-rule"})
		require.Equal(t, []any{map[string]any{"project": "example-finance", "grant": approvalID}}, read["may_contain"])
	})
	t.Run("note", func(t *testing.T) {
		r := newP6ProofRig(t)
		agent, _, _ := r.grantee()
		note := r.call(agent, "notes_create", map[string]any{"title": "ledger proof", "body": "the ledger proof\n"})
		require.Equal(t, 1, r.rows(core.ItemKindNote, note["id"].(string)))
	})
	t.Run("task", func(t *testing.T) {
		r := newP6ProofRig(t)
		agent, _, _ := r.grantee()
		task := r.call(agent, "tasks_add", map[string]any{"title": "reconcile the ledger proof"})
		require.Equal(t, 1, r.rows(core.ItemKindTask, task["id"].(string)))
		list := r.call(agent, "tasks_list", map[string]any{"id": task["id"]})
		require.Contains(t, list["tasks"].([]any)[0], "may_contain")
	})
	t.Run("findings", func(t *testing.T) {
		r := newP6ProofRig(t)
		agent, amb, _ := r.grantee()
		r.call(agent, "session_update", map[string]any{"findings": "the ledger proof holds"})
		require.Equal(t, 1, r.rows(core.ItemKindSession, amb))
	})
	t.Run("recall hits", func(t *testing.T) {
		r := newP6ProofRig(t)
		agent, _, _ := r.grantee()
		r.call(agent, "memory_write", map[string]any{"name": "proof-rule", "kind": "decision",
			"description": "the ledger proof", "body": "Reconcile the ledger first.\n"})
		hits := r.call(agent, "recall", map[string]any{"query": "ledger proof"})["hits"].([]any)
		require.NotEmpty(t, hits)
		for _, h := range hits {
			require.Contains(t, h.(map[string]any), "may_contain", "%v", h)
		}
	})
	t.Run("supersession unions a stamp only the old file carries", func(t *testing.T) {
		r := newP6ProofRig(t)
		ctx := context.Background()
		id, err := core.NewID()
		require.NoError(t, err)
		// The stamp as frontmatter carries it, written through the key the
		// previous commit keeps as an unknown one.
		_, err = r.mgr.WriteMemory(ctx, core.Memory{ID: id, Kind: core.KindDecision, Name: "stamped-rule",
			Description: "d", Project: "example-legal", Body: "b\n", Created: r.now, Updated: r.now, ValidFrom: r.now,
			Extra: map[string]any{"may_contain": []any{map[string]any{"project": "example-finance", "grant": "01UNIONPROOF"}}}})
		require.NoError(t, err)
		own := dialClient(t, ctx, r.url, testKey)
		ownID, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateSession(ctx, r.db, core.Session{ID: ownID, Name: "cc/p6-union", ProjectSlug: "example-legal",
			Status: core.SessionActive, CWD: "/work/example-legal", CreatedAt: r.now, UpdatedAt: r.now}))
		r.call(own, "session_start", map[string]any{"name": "cc/p6-union"})
		r.call(own, "memory_write", map[string]any{"name": "stamped-rule-v2", "kind": "decision", "description": "the new rule",
			"body": "b2\n", "supersedes": "stamped-rule"})
		raw, err := os.ReadFile(filepath.Join(r.dir, "memory", "example-legal", "stamped-rule-v2.md"))
		require.NoError(t, err)
		require.True(t, strings.Contains(string(raw), "grant: 01UNIONPROOF"), "the replacement unions the old stamp:\n%s", raw)
	})
	t.Run("supersession keeps the union", func(t *testing.T) {
		r := newP6ProofRig(t)
		ctx := context.Background()
		agent, _, approvalID := r.grantee()
		r.call(agent, "memory_write", map[string]any{"name": "proof-rule", "kind": "decision",
			"description": "the ledger proof", "body": "Reconcile the ledger first.\n"})
		// A replacement written by a connection that holds nothing more.
		own := dialClient(t, ctx, r.url, testKey)
		ownID, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateSession(ctx, r.db, core.Session{ID: ownID, Name: "cc/p6-owner", ProjectSlug: "example-legal",
			Status: core.SessionActive, CWD: "/work/example-legal", CreatedAt: r.now, UpdatedAt: r.now}))
		r.call(own, "session_start", map[string]any{"name": "cc/p6-owner"})
		r.call(own, "memory_write", map[string]any{"name": "proof-rule-v2", "kind": "decision", "description": "the new proof",
			"body": "Reconcile twice.\n", "supersedes": "proof-rule"})
		raw, err := os.ReadFile(filepath.Join(r.dir, "memory", "example-legal", "proof-rule-v2.md"))
		require.NoError(t, err)
		require.True(t, strings.Contains(string(raw), "grant: "+approvalID), "the replacement keeps the union:\n%s", raw)
	})
}
