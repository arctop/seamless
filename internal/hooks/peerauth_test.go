package hooks

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// Under peer_auth the daemon opens no path a hook names: the transcript here is
// perfectly readable, and session end still harvests nothing from it. The
// daemon runs as its own account under peer_auth, so a path in a hook is one
// some other account chose -- possibly a symlink into the daemon's own data
// directory, which only the daemon can read. A restricted account's hook
// grows no repo mapping either: it cannot create projects.
func TestPeerAuth_HooksReadNoAgentFiles(t *testing.T) {
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	require.NoError(t, store.AdoptLocalHost(ctx, db, localHostName))
	_, err = store.EnsureProject(ctx, db, "example-ops", "example-ops")
	require.NoError(t, err)
	require.NoError(t, store.AddRepoMapping(ctx, db, "/work/ops", "example-ops"))
	const agentAccount = "_example_agent"
	accounts := map[string]access.Account{"owner": {UID: 501, Name: "owner"}, agentAccount: {UID: 470, Name: agentAccount}}
	policy, err := access.New(config.PeerAuth{Owner: "owner", Restricted: []string{agentAccount}},
		func(n string) (access.Account, error) {
			if a, ok := accounts[n]; ok {
				return a, nil
			}
			return access.Account{}, fmt.Errorf("no %s", n)
		})
	require.NoError(t, err)
	agent, ok := policy.PeerView(470)
	require.True(t, ok)
	agent = agent.WithOpenProjects([]string{"example-ops"})

	ret := retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	h := NewHandler(Config{DB: db, Retrieve: ret, Events: events.NewRecorder(db), APIKey: testKey, LocalHost: localHostName})
	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(access.WithView(r.Context(), agent)))
	}))
	t.Cleanup(ts.Close)

	const id = "agent-1"
	transcript := writeTranscript(t, `{"type":"assistant","message":{"role":"assistant","model":"claude-x","content":[{"type":"text","text":"SECRET-FROM-A-FILE"}]}}`)
	resp, _ := post(t, ts.URL+"/api/hooks/session-start?host="+localHostName, "", map[string]any{
		"session_id": id, "cwd": "/work/ops", "source": "startup", "transcript_path": transcript,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	sess := requireAmbientSession(t, db, ClientClaudeCode, access.QualifiedSessionName(agentAccount, id))
	require.Equal(t, "example-ops", sess.ProjectSlug)

	resp, _ = post(t, ts.URL+"/api/hooks/session-end", "", map[string]any{"session_id": id, "transcript_path": transcript})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	sess = requireAmbientSession(t, db, ClientClaudeCode, access.QualifiedSessionName(agentAccount, id))
	require.Equal(t, core.SessionCompleted, sess.Status)
	require.NotContains(t, sess.Findings, "SECRET-FROM-A-FILE", "no agent file is read under peer_auth")
	require.Empty(t, sess.Model, "no transcript sniffing under peer_auth")

	// A directory nothing maps: not recorded, and the hook grew no mapping.
	resp, _ = post(t, ts.URL+"/api/hooks/session-start?host="+localHostName, "", map[string]any{
		"session_id": "agent-2", "cwd": "/opt/example/app", "source": "startup", "transcript_path": transcript,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	rows, err := store.RepoMapRows(ctx, db)
	require.NoError(t, err)
	require.Len(t, rows, 1)
}
