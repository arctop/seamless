package console

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// newTestService builds a console Service over a fresh DB for tests that
// exercise unexported methods directly rather than going through the mux.
func newTestService(t *testing.T) (*sql.DB, *Service) {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc, err := New(Config{
		DB: db, Events: events.NewRecorder(db), APIKey: testKey,
		Retrieve: retrieve.New(db, nil, config.Defaults().Budgets, nil),
	})
	require.NoError(t, err)
	return db, svc
}

func TestSourceSessionResolver(t *testing.T) {
	db, svc := newTestService(t)
	ctx := context.Background()
	now := time.Now().UTC()

	victimID := mustID(t)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: victimID, Name: "cc/victim", ExternalClient: "claude-code", Model: "claude-fable-5",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))
	// A session NAMED with another session's ULID: names are agent-supplied
	// and unvalidated, so this row exists to prove it cannot capture the
	// victim's provenance.
	hijackerID := mustID(t)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: hijackerID, Name: victimID, ExternalClient: "codex", Model: "gpt-5.5",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))

	resolve := svc.sourceSessionResolver(ctx)

	t.Run("ULID stamp resolves by id only, immune to name hijack", func(t *testing.T) {
		got := resolve(victimID)
		require.Equal(t, victimID, got.ID)
		require.Equal(t, "cc/victim", got.Name)
		require.Equal(t, "claude-code", harnessOfSource(resolve, victimID))
	})
	t.Run("name stamp resolves by name", func(t *testing.T) {
		require.Equal(t, victimID, resolve("cc/victim").ID)
	})
	t.Run("deleted session falls back to the ambient name prefix", func(t *testing.T) {
		require.Equal(t, "claude-code", harnessOfSource(resolve, "cc/gone"))
		require.Equal(t, "codex", harnessOfSource(resolve, "cx/gone"))
		// A deleted bound stamp has no prefix to fall back to.
		require.Empty(t, harnessOfSource(resolve, mustID(t)))
	})
	t.Run("empty stamp resolves to the zero session", func(t *testing.T) {
		require.Empty(t, resolve("").ID)
	})
	// Last: deletes the victim row, so the subtests above must already have run.
	t.Run("memoized per resolver, fresh per call", func(t *testing.T) {
		memo := svc.sourceSessionResolver(ctx)
		require.Equal(t, victimID, memo("cc/victim").ID)
		_, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, victimID)
		require.NoError(t, err)
		require.Equal(t, victimID, memo("cc/victim").ID, "second lookup is served from the memo")
		require.Empty(t, svc.sourceSessionResolver(ctx)("cc/victim").ID, "a fresh resolver sees the deletion")
	})
}
