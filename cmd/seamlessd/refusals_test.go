package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store/storetest"
)

// refusalWatch records access.refused for a 401 on any surface and for a
// reason a handler noted, while write screening is on -- and nothing while it
// is off, or for an answer that is not a refusal.
func TestRefusalWatch(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)
	on := &atomic.Bool{}
	screen := screening.New(screening.Config{DB: db, Events: rec, Enabled: func(context.Context) bool { return on.Load() }})
	h := refusalWatch(screen, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/mcp":
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case "/api/hooks/session-start":
			access.NoteRefusal(r.Context(), screening.RefusedPeerUnknown)
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	hit := func(path string) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, nil))
	}
	count := func() []core.Event {
		evs, err := rec.ByKinds(ctx, []core.EventKind{core.EventAccessRefused}, "", "", 100)
		require.NoError(t, err)
		return evs
	}

	hit("/api/mcp")
	hit("/api/hooks/session-start")
	require.Empty(t, count(), "off: no refusal events")

	on.Store(true)
	hit("/api/mcp")
	hit("/api/hooks/session-start")
	hit("/console/")
	evs := count()
	require.Len(t, evs, 2)
	reasons := map[string]string{}
	for _, e := range evs {
		reasons[e.Payload["reason"].(string)] = e.Payload["surface"].(string)
	}
	require.Equal(t, map[string]string{"auth.failed": "mcp", "peer.unknown": "hooks"}, reasons)
}

func TestSurfaceOf(t *testing.T) {
	for path, want := range map[string]string{
		"/api/mcp": "mcp", "/api/hooks/session-taint": "hooks", "/api/a2a": "a2a",
		"/.well-known/agent-card.json": "a2a", "/console/held": "console", "/healthz": "api",
	} {
		require.Equal(t, want, surfaceOf(path), path)
	}
}
