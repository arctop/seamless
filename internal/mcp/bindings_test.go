package mcp

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// fakeClientSession satisfies mcpserver.ClientSession so tests can stamp a
// connection identity on a context without a real streamable-HTTP transport.
type fakeClientSession struct{ id string }

func (f fakeClientSession) Initialize()                                         {}
func (f fakeClientSession) Initialized() bool                                   { return true }
func (f fakeClientSession) NotificationChannel() chan<- mcp.JSONRPCNotification { return nil }
func (f fakeClientSession) SessionID() string                                   { return f.id }

// newBindingsServer builds a Server over a fresh store, wired with just enough
// config for the binding paths (DB + key).
func newBindingsServer(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return New(Config{DB: db, APIKey: "test-key"}), db
}

// connCtx returns a context carrying a fake client session id, as the
// streamable-HTTP transport would for a stateful connection.
func connCtx(s *Server, conn string) context.Context {
	return s.mcp.WithContext(context.Background(), fakeClientSession{id: conn})
}

// seedStatusSession inserts a session with the given status and recency.
func seedStatusSession(t *testing.T, db *sql.DB, name string, status core.SessionStatus, updated time.Time) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(context.Background(), db, core.Session{
		ID: id, Name: name, ProjectSlug: "demo", Status: status,
		CreatedAt: updated, UpdatedAt: updated,
	}))
	return id
}

// forceSweep rewinds the sweep gate and runs one sweep immediately.
func forceSweep(s *Server, ctx context.Context) {
	s.mu.Lock()
	s.lastSweep = time.Time{}
	s.mu.Unlock()
	s.maybeSweepBindings(ctx)
}

// hasBinding reports whether the connection currently has a binding, without
// refreshing its touchedAt the way getBinding does.
func hasBinding(s *Server, conn string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.bindings[conn]
	return ok
}

// TestLabOnlyBindingIsNotASessionBinding pins the getBinding/rawBinding split:
// the session-less entry setBindingLab creates on an unbound connection keeps
// its lab visible through boundLab but is never reported as a session binding,
// so scope resolution falls through to the ambient path instead of reading the
// entry's empty (global) project.
func TestLabOnlyBindingIsNotASessionBinding(t *testing.T) {
	s, db := newBindingsServer(t)
	ctx := connCtx(s, "conn-lab-only")

	s.setBindingLab(ctx, "lab-1")
	require.True(t, hasBinding(s, "conn-lab-only"), "the lab-only entry must exist")
	_, ok := s.getBinding(ctx)
	require.False(t, ok, "a lab-only entry must not count as a session binding")
	require.Equal(t, "lab-1", s.boundLab(ctx), "lab affinity must survive without a session binding")

	// Once a real session binds, the connection is session-bound again.
	id := seedStatusSession(t, db, "sess/lab-bind", core.SessionActive, time.Now().UTC())
	s.setBinding(ctx, id, "demo")
	b, ok := s.getBinding(ctx)
	require.True(t, ok)
	require.Equal(t, "demo", b.project)
}

// TestSessionEnd_EvictsBindings verifies the session_end tool evicts every
// connection binding pointing at the ended session -- the graceful-end half of
// the unbounded-bindings fix.
func TestSessionEnd_EvictsBindings(t *testing.T) {
	s, db := newBindingsServer(t)
	now := time.Now().UTC()
	id := seedStatusSession(t, db, "sess/ending", core.SessionActive, now)
	other := seedStatusSession(t, db, "sess/other", core.SessionActive, now)

	ctx1 := connCtx(s, "conn-1")
	s.setBinding(ctx1, id, "demo")
	// A second connection bound to the same session (resume by name) and a
	// bystander bound to a different session.
	s.setBinding(connCtx(s, "conn-2"), id, "demo")
	s.setBinding(connCtx(s, "conn-other"), other, "demo")

	req := mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "session_end", Arguments: map[string]any{"findings": "done"},
	}}
	res, err := s.handleSessionEnd(ctx1, req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	require.False(t, hasBinding(s, "conn-1"), "the ending connection's binding must be evicted")
	require.False(t, hasBinding(s, "conn-2"), "every binding on the ended session must be evicted")
	require.True(t, hasBinding(s, "conn-other"), "bindings on other sessions must survive")
}

// TestBindingSweep_EvictsInactiveSessions verifies the opportunistic sweep: a
// binding whose session is completed or expired (ended by the SessionEnd hook or
// the idle reaper, which never see the bindings map) is evicted, an active one
// is kept, and a session-less (lab-only) binding falls back to the idle TTL.
func TestBindingSweep_EvictsInactiveSessions(t *testing.T) {
	s, db := newBindingsServer(t)
	now := time.Now().UTC()
	activeID := seedStatusSession(t, db, "sess/alive", core.SessionActive, now)
	completedID := seedStatusSession(t, db, "sess/done", core.SessionCompleted, now)
	expiredID := seedStatusSession(t, db, "sess/gone", core.SessionExpired, now)

	s.setBinding(connCtx(s, "conn-active"), activeID, "demo")
	s.setBinding(connCtx(s, "conn-completed"), completedID, "demo")
	s.setBinding(connCtx(s, "conn-expired"), expiredID, "demo")
	// Lab-only bindings: no session lifecycle to end them, so the TTL decides.
	s.setBindingLab(connCtx(s, "conn-lab-fresh"), "lab-1")
	s.setBindingLab(connCtx(s, "conn-lab-idle"), "lab-2")
	s.mu.Lock()
	b := s.bindings["conn-lab-idle"]
	b.touchedAt = time.Now().Add(-bindingIdleTTL - time.Minute)
	s.bindings["conn-lab-idle"] = b
	s.mu.Unlock()

	forceSweep(s, context.Background())

	require.True(t, hasBinding(s, "conn-active"), "an active session's binding must survive the sweep")
	require.False(t, hasBinding(s, "conn-completed"), "a completed session's binding must be evicted")
	require.False(t, hasBinding(s, "conn-expired"), "an expired session's binding must be evicted")
	require.True(t, hasBinding(s, "conn-lab-fresh"), "a fresh lab-only binding must survive")
	require.False(t, hasBinding(s, "conn-lab-idle"), "a lab-only binding idle past the TTL must be evicted")
}

// TestBindingSweep_EvictsReapedSession runs the real idle-reaper cascade: a
// session the reaper flips to expired loses its binding on the next sweep, so a
// crashed agent's connection entry does not outlive its session.
func TestBindingSweep_EvictsReapedSession(t *testing.T) {
	s, db := newBindingsServer(t)
	ctx := context.Background()
	stale := time.Now().UTC().Add(-2 * time.Hour)
	id := seedStatusSession(t, db, "sess/idle", core.SessionActive, stale)
	s.setBinding(connCtx(s, "conn-idle"), id, "demo")

	reaped, err := store.ExpireStaleSessions(ctx, db, time.Now().UTC().Add(-time.Hour))
	require.NoError(t, err)
	require.Len(t, reaped, 1)

	forceSweep(s, ctx)
	require.False(t, hasBinding(s, "conn-idle"), "the reaped session's binding must be evicted by the sweep")
}

// TestBindingSweep_RateLimited pins the gate: a second sweep inside the interval
// does not run (an ended session's binding survives until the interval elapses).
func TestBindingSweep_RateLimited(t *testing.T) {
	s, db := newBindingsServer(t)
	now := time.Now().UTC()
	completedID := seedStatusSession(t, db, "sess/done", core.SessionCompleted, now)

	// Close the gate as if a sweep just ran, then bind to an ended session: no
	// sweep attempt inside the interval may evict it.
	s.mu.Lock()
	s.lastSweep = time.Now()
	s.mu.Unlock()
	s.setBinding(connCtx(s, "conn-a"), completedID, "demo")
	s.maybeSweepBindings(context.Background())
	require.True(t, hasBinding(s, "conn-a"), "a sweep inside the interval must not run")

	// Reopening the gate sweeps it away.
	forceSweep(s, context.Background())
	require.False(t, hasBinding(s, "conn-a"))
}

// TestBindings_ConcurrentAccessAndSweep hammers set/get/lab updates on many
// connections while sweeps run concurrently -- the race-safety check for
// eviction vs. concurrent binding lookups. Run with -race. After a final sweep,
// only bindings pointing at the still-active session may remain.
func TestBindings_ConcurrentAccessAndSweep(t *testing.T) {
	s, db := newBindingsServer(t)
	now := time.Now().UTC()
	activeID := seedStatusSession(t, db, "sess/hot", core.SessionActive, now)
	endedID := seedStatusSession(t, db, "sess/cold", core.SessionCompleted, now)

	const (
		conns = 8
		iters = 200
	)
	var wg sync.WaitGroup
	for i := range conns {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := connCtx(s, fmt.Sprintf("conn-%d", i))
			for j := range iters {
				id := activeID
				if (i+j)%2 == 0 {
					id = endedID
				}
				s.setBinding(ctx, id, "demo")
				s.getBinding(ctx)
				s.setBindingLab(ctx, "lab-x")
				s.evictSessionBindings(endedID)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 50 {
			forceSweep(s, context.Background())
		}
	}()
	wg.Wait()

	forceSweep(s, context.Background())
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn, b := range s.bindings {
		// A cross-goroutine evict between a conn's setBinding and setBindingLab
		// can legitimately leave a fresh session-less lab binding, so the
		// invariant is: nothing may still point at the ended session.
		require.NotEqual(t, endedID, b.sessionID,
			"after the final sweep %s must not be bound to the ended session", conn)
	}
}
