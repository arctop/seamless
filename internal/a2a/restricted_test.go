package a2a

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// A restricted local account (peer_auth.restricted) over A2A: with no project
// named it recalls the global scope (it has no home project), it may name an
// open project it reaches, and naming any other -- a confidential one, or a
// slug nobody registered -- is the one refusal, whose words never tell the
// two apart.

// insertRecallMemory indexes a gotcha written by account ("" for the owner).
func insertRecallMemory(t *testing.T, db *sql.DB, id, name, desc, project, account string) {
	t.Helper()
	now := core.FormatTime(time.Now().UTC())
	_, err := db.Exec(`INSERT INTO memories_index
		    (id, kind, name, description, project, file_path, tags, valid_from,
		     invalid_at, superseded_by, source_session, content_hash, created_at, updated_at, account)
		VALUES (?, 'gotcha', ?, ?, ?, ?, '[]', ?, NULL, NULL, '', 'h', ?, ?, ?)`,
		id, name, desc, project, "memory/x/"+name+".md", now, now, now, account)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO fts (item_id, kind, project, title, name, description, body)
		VALUES (?, 'memory', ?, '', ?, ?, ?)`, id, project, name, desc, desc)
	require.NoError(t, err)
}

func TestMessageSend_RestrictedAccountRecallsTheGlobalScope(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for slug, iso := range map[string]core.Isolation{"example-ops": core.IsolationOpen, "example-finance": core.IsolationConfidential} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, iso))
	}
	insertRecallMemory(t, db, "01G", "global-ledger-note", "ledger reconcile shared note", "", "")
	insertRecallMemory(t, db, "01O", "ops-ledger-note", "ledger reconcile ops note", "example-ops", "_example_agent")
	insertRecallMemory(t, db, "01F", "finance-ledger-secret", "ledger reconcile finance secret", "example-finance", "")

	policy, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", Restricted: []string{"_example_agent"}},
		func(n string) (access.Account, error) {
			switch n {
			case "owner":
				return access.Account{UID: 501, Name: "owner"}, nil
			case "_example_agent":
				return access.Account{UID: 502, Name: "_example_agent"}, nil
			}
			return access.Account{}, fmt.Errorf("no %s", n)
		})
	require.NoError(t, err)
	srv, err := New(Config{Retrieve: retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil),
		Events: events.NewRecorder(db), Version: "1.2.3", Endpoint: "http://127.0.0.1:8081/api/a2a"})
	require.NoError(t, err)
	h := srv.Handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, ok := policy.PeerView(502)
		require.True(t, ok)
		open, err := store.OpenProjectSlugs(r.Context(), db)
		require.NoError(t, err)
		h.ServeHTTP(w, r.WithContext(access.WithView(r.Context(), v.WithOpenProjects(open))))
	}))
	t.Cleanup(ts.Close)

	names := func(result json.RawMessage) []string {
		var msg Message
		require.NoError(t, json.Unmarshal(result, &msg))
		var out []string
		for _, p := range msg.Parts {
			if p.Kind != "data" {
				continue
			}
			raw, err := json.Marshal(p.Data["hits"])
			require.NoError(t, err)
			var hits []retrieve.Hit
			require.NoError(t, json.Unmarshal(raw, &hits))
			for _, h := range hits {
				out = append(out, h.Name)
			}
		}
		return out
	}

	// No project named, no bearer key: the global scope, and only it.
	result, rpcErr := rpc(t, ts.URL, "", sendBody("ledger reconcile", nil))
	require.Nil(t, rpcErr)
	require.Equal(t, []string{"global-ledger-note"}, names(result))
	injected := eventRows(t, db, core.EventInjected)
	require.Len(t, injected, 1)
	require.Equal(t, "", injected[0]["_project"], "its recall's event keeps the global scope it recalled")

	// An open project it reaches: that project plus global.
	result, rpcErr = rpc(t, ts.URL, "", sendBody("ledger reconcile", map[string]any{"project": "example-ops"}))
	require.Nil(t, rpcErr)
	require.ElementsMatch(t, []string{"global-ledger-note", "ops-ledger-note"}, names(result))
	// The account's own memory says who wrote it, in the text part and the
	// data part; the owner's line carries no label.
	var msg Message
	require.NoError(t, json.Unmarshal(result, &msg))
	require.Contains(t, msg.Parts[0].Text, "ops-ledger-note -- ledger reconcile ops note (example-ops) [written by account _example_agent]")
	for _, line := range strings.Split(msg.Parts[0].Text, "\n") {
		if strings.Contains(line, "global-ledger-note") {
			require.True(t, strings.HasSuffix(line, "global-ledger-note -- ledger reconcile shared note"),
				"the owner's hit is unlabeled: %q", line)
		}
	}
	raw, err := json.Marshal(msg.Parts[1].Data["hits"])
	require.NoError(t, err)
	require.Contains(t, string(raw), `"account":"_example_agent"`)

	// A confidential project and a slug nobody registered: one refusal, the
	// slug the only difference, and nothing from inside.
	var refusals []string
	for _, slug := range []string{"example-finance", "example-nosuch"} {
		result, rpcErr = rpc(t, ts.URL, "", sendBody("ledger reconcile", map[string]any{"project": slug}))
		require.Nil(t, result)
		require.NotNil(t, rpcErr, slug)
		require.NotContains(t, rpcErr.Message, "finance-ledger-secret")
		require.Contains(t, rpcErr.Message, "is not reachable from account _example_agent")
		refusals = append(refusals, strings.ReplaceAll(rpcErr.Message, slug, "<slug>"))
	}
	require.Equal(t, refusals[0], refusals[1], "a closed project reads like a missing one")

	// The widest scope reads no closed project either.
	result, rpcErr = rpc(t, ts.URL, "", sendBody("ledger reconcile finance secret", map[string]any{"scope": "all"}))
	require.Nil(t, rpcErr)
	require.NotContains(t, names(result), "finance-ledger-secret")
}

// The text part labels a restricted account's hit after its other suffixes,
// bracketed exactly as the briefing and prompt recall print the label
// (retrieve.AccountMark); the owner's hits render byte-identically to before.
func TestRenderHits_LabelsARestrictedAccountsHits(t *testing.T) {
	owner := []retrieve.Hit{
		{Kind: "memory", Name: "ops-rule", Description: "the ops rule", Project: "example-ops"},
		{Kind: "note", Name: "shared-note", Description: "a shared note"},
	}
	require.Equal(t, "2 hits for \"q\":\n1. [memory] ops-rule -- the ops rule (example-ops)\n2. [note] shared-note -- a shared note",
		renderHits("q", owner), "the owner's hits are unchanged")

	labeled := []retrieve.Hit{
		{Kind: "memory", Name: "ops-tip", Description: "a tip", Project: "example-ops", Account: "_example_agent"},
		{Kind: "task", Name: "t1", Description: "a task", Account: "_example_agent"},
		{Kind: "note", Name: "shared-note", Description: "a shared note"},
	}
	require.Equal(t, "3 hits for \"q\":\n"+
		"1. [memory] ops-tip -- a tip (example-ops) [written by account _example_agent]\n"+
		"2. [task] t1 -- a task [written by account _example_agent]\n"+
		"3. [note] shared-note -- a shared note", renderHits("q", labeled))
}
