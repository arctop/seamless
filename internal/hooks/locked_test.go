package hooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// A SessionEnd revokes the session's leases on locked projects through the
// revoker serve wires in while a project is locked; with none wired (the
// default), nothing is called.
func TestSessionEnd_RevokesTheSessionsLeases(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap, `{"/work/demo":"demo"}`))
	require.NoError(t, store.AdoptLocalHost(ctx, db, localHostName))

	var mu sync.Mutex
	var revoked []string
	h := NewHandler(Config{
		DB: db, Retrieve: retrieve.New(db, nil, config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}, nil),
		Events: events.NewRecorder(db), APIKey: testKey, LocalHost: localHostName,
		RevokeSessionLeases: func(_ context.Context, id string) (int64, error) {
			mu.Lock()
			defer mu.Unlock()
			revoked = append(revoked, id)
			return 1, nil
		},
	})
	mux := http.NewServeMux()
	h.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	e := &hostEnv{ts: ts, db: db}

	e.postFrom(t, "session-start", map[string]string{"host": localHostName, "repo_root": "/work/demo", "main_root": "/work/demo"},
		map[string]any{"session_id": "lock-1", "cwd": "/work/demo", "source": "startup"})
	sess := e.session(t, "lock-1")
	e.postFrom(t, "session-end", map[string]string{"host": localHostName}, map[string]any{"session_id": "lock-1", "reason": "exit"})

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{sess.ID}, revoked)
}
