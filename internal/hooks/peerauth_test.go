package hooks

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

// Under peer_auth a configured account's session lands in its own project
// wherever it opens, and the daemon opens no path a hook names: the transcript
// here is perfectly readable, and session end still harvests nothing from it.
// The daemon runs as its own account under peer_auth, so a path in a hook is
// one some other account chose -- possibly a symlink into the daemon's own
// data directory, which only the daemon can read.
func TestPeerAuth_AccountSessionsLandHomeAndHarvestNothing(t *testing.T) {
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, slug := range []string{"arctop-finance", "demo"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.AddRepoMapping(ctx, db, "/work/demo", "demo"))
	accounts := map[string]access.Account{"katata": {UID: 501, Name: "katata"}, "_arctop_finance": {UID: 470, Name: "_arctop_finance"}}
	policy, err := access.New(config.PeerAuth{Owner: "katata", Users: map[string]config.PeerUser{
		"_arctop_finance": {Sealed: []string{"arctop-finance"}},
	}}, func(n string) (access.Account, error) {
		if a, ok := accounts[n]; ok {
			return a, nil
		}
		return access.Account{}, fmt.Errorf("no %s", n)
	})
	require.NoError(t, err)
	fin, _ := policy.PeerView(470)

	ret := retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil)
	h := NewHandler(Config{DB: db, Retrieve: ret, Events: events.NewRecorder(db), APIKey: testKey})
	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(access.WithView(r.Context(), fin)))
	}))
	t.Cleanup(ts.Close)

	transcript := writeTranscript(t, `{"type":"assistant","message":{"role":"assistant","model":"claude-x","content":[{"type":"text","text":"SECRET-FROM-A-FILE"}]}}`)
	for _, cwd := range []string{"/work/demo", "/opt/arctop/finance/app", ""} {
		id := "fin-" + strings.ReplaceAll(cwd, "/", "-")
		resp, _ := post(t, ts.URL+"/api/hooks/session-start", "", map[string]any{
			"session_id": id, "cwd": cwd, "source": "startup", "transcript_path": transcript,
		})
		require.Equal(t, http.StatusOK, resp.StatusCode)
		sess := requireAmbientSession(t, db, ClientClaudeCode, id)
		require.Equal(t, "arctop-finance", sess.ProjectSlug, "cwd %q: the account's session lands in its own project", cwd)

		resp, _ = post(t, ts.URL+"/api/hooks/session-end", "", map[string]any{"session_id": id, "transcript_path": transcript})
		require.Equal(t, http.StatusOK, resp.StatusCode)
		sess = requireAmbientSession(t, db, ClientClaudeCode, id)
		require.Equal(t, core.SessionCompleted, sess.Status)
		require.NotContains(t, sess.Findings, "SECRET-FROM-A-FILE", "no agent file is read under peer_auth")
		require.Empty(t, sess.Model, "no transcript sniffing under peer_auth")
	}
	// The account's hook grew no repo mapping: it works only inside its own projects.
	rows, err := store.RepoMapRows(ctx, db)
	require.NoError(t, err)
	require.Len(t, rows, 1)
}
