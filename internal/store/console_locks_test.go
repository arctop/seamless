package store_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

func TestConsoleLocks(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, slug := range []string{"example-conf", "example-open"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, "example-conf", core.IsolationConfidential))
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

	require.ErrorIs(t, store.AddConsoleLock(ctx, db, "example-open", "cred", now), store.ErrConsoleLockUnfenced)
	require.ErrorIs(t, store.AddConsoleLock(ctx, db, "example-gone", "cred", now), store.ErrProjectNotFound)
	require.ErrorIs(t, store.RemoveConsoleLock(ctx, db, "example-conf"), store.ErrConsoleLockNotFound)

	require.NoError(t, store.AddConsoleLock(ctx, db, "example-conf", "cred", now))
	require.ErrorIs(t, store.AddConsoleLock(ctx, db, "example-conf", "cred", now), store.ErrConsoleLockExists)
	locks, err := store.ConsoleLocks(ctx, db)
	require.NoError(t, err)
	require.Equal(t, []store.ConsoleLock{{Project: "example-conf", Credential: "cred", LockedAt: now}}, locks)
	slugs, err := store.ConsoleLockedSlugs(ctx, db)
	require.NoError(t, err)
	require.Equal(t, []string{"example-conf"}, slugs)
	has, err := store.HasConsoleLock(ctx, db, "example-conf")
	require.NoError(t, err)
	require.True(t, has)

	require.NoError(t, store.RemoveConsoleLock(ctx, db, "example-conf"))
	has, err = store.HasConsoleLock(ctx, db, "example-conf")
	require.NoError(t, err)
	require.False(t, has)
}

// Turning a lock back on ends what the project still had from before the lock
// came off: agents' leases, read grants, and browsers' unlocks, live or pending
// (a terminal pairing's code included). Other projects keep theirs, an expired
// lease is left as it ended, and a lock that is refused ends nothing.
func TestAddConsoleLock_EndsEarlierAccess(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, slug := range []string{"example-conf", "example-other", "example-open"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	for _, slug := range []string{"example-conf", "example-other"} {
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, core.IsolationConfidential))
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	at, later, earlier := core.FormatTime(now), core.FormatTime(now.Add(time.Hour)), core.FormatTime(now.Add(-time.Hour))
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := db.ExecContext(ctx, q, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO access_approvals (id, project_slug, kind, challenge, status, requested_at, expires_at, lease_seconds,
		peer_uid, peer_pid, process_key) VALUES ('A1', 'example-conf', 'request', ?, 'approved', ?, ?, 3600, 501, 1, 'k')`,
		make([]byte, 32), earlier, later)
	lease := func(id, project, expires string) {
		exec(`INSERT INTO access_leases (id, project_slug, process_key, peer_uid, peer_pid, approval_id, granted_at, expires_at)
			VALUES (?, ?, 'k', 501, 1, 'A1', ?, ?)`, id, project, earlier, expires)
	}
	lease("L-conf", "example-conf", later)
	lease("L-conf-expired", "example-conf", earlier)
	lease("L-other", "example-other", later)
	exec(`INSERT INTO access_grants (id, project_slug, from_project, session_id, approval_id, decided_by, granted_at, expires_at)
		VALUES ('G-conf', 'example-conf', 'example-open', 'S1', 'A1', 'passkey', ?, ?)`, earlier, later)
	unlock := func(id, project, status, claim string) {
		exec(`INSERT INTO console_unlocks (id, project_slug, status, token_hash, claim_hash, lease_seconds, created_at,
			pending_until, granted_at, expires_at) VALUES (?, ?, ?, 'tok', ?, 3600, ?, ?, ?, ?)`,
			id, project, status, claim, earlier, later, earlier, later)
	}
	unlock("U-live", "example-conf", store.ConsoleUnlockLive, "")
	unlock("U-pairing", "example-conf", store.ConsoleUnlockPending, "code-hash")
	unlock("U-other", "example-other", store.ConsoleUnlockLive, "")

	ended, err := store.AddConsoleLockEndingAccess(ctx, db, "example-conf", "cred", now)
	require.NoError(t, err)
	require.Equal(t, store.EndedAccess{Leases: 1, Unlocks: 2, Grants: 1}, ended)

	leases, err := store.LiveAccessLeases(ctx, db, now)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.Equal(t, "L-other", leases[0].ID, "another project's lease is untouched")
	grants, err := store.LiveAccessGrants(ctx, db, now)
	require.NoError(t, err)
	require.Empty(t, grants, "a read grant of the project ends too, so removing the lock again does not revive it")
	var reason string
	var revoked sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, `SELECT revoked_reason FROM access_leases WHERE id = 'L-conf'`).Scan(&reason))
	require.Equal(t, store.LeaseRevokedLocked, reason)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT revoked_at FROM access_leases WHERE id = 'L-conf-expired'`).Scan(&revoked))
	require.False(t, revoked.Valid, "an expired lease keeps its own end")
	for id, want := range map[string]string{"U-live": store.ConsoleUnlockEnded, "U-pairing": store.ConsoleUnlockEnded, "U-other": store.ConsoleUnlockLive} {
		var status, why, claim string
		var endedAt sql.NullString
		require.NoError(t, db.QueryRowContext(ctx, `SELECT status, ended_reason, claim_hash, ended_at FROM console_unlocks WHERE id = ?`, id).
			Scan(&status, &why, &claim, &endedAt))
		require.Equal(t, want, status, id)
		if want == store.ConsoleUnlockEnded {
			require.Equal(t, store.ConsoleUnlockEndLocked, why, id)
			require.Empty(t, claim, "%s: a pending pairing's code can no longer be approved", id)
			require.Equal(t, at, endedAt.String, id)
		}
	}

	// A refused lock ends nothing: the project is already locked.
	lease("L-conf-new", "example-conf", later)
	_, err = store.AddConsoleLockEndingAccess(ctx, db, "example-conf", "cred", now)
	require.ErrorIs(t, err, store.ErrConsoleLockExists)
	leases, err = store.LiveAccessLeases(ctx, db, now)
	require.NoError(t, err)
	require.Len(t, leases, 2, "the refused lock left the newer lease live")
}

// A fence stays up under a lock of either kind; narrowing it to another fence is
// still allowed, and an unlocked project opens as before.
func TestLoosenProjectIsolation(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, slug := range []string{"example-console", "example-file", "example-free"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, core.IsolationSealed))
	}
	require.NoError(t, store.AddConsoleLock(ctx, db, "example-console", "cred", time.Now()))
	force := []string{"example-file"}

	for _, slug := range []string{"example-console", "example-file"} {
		require.ErrorIs(t, store.LoosenProjectIsolation(ctx, db, slug, core.IsolationOpen, force), store.ErrIsolationLocked, slug)
		state, err := store.IsolationOf(ctx, db, slug)
		require.NoError(t, err)
		require.Equal(t, core.IsolationSealed, state, "%s keeps its fence", slug)

		require.NoError(t, store.LoosenProjectIsolation(ctx, db, slug, core.IsolationConfidential, force), slug)
		state, err = store.IsolationOf(ctx, db, slug)
		require.NoError(t, err)
		require.Equal(t, core.IsolationConfidential, state, slug)
	}
	require.NoError(t, store.LoosenProjectIsolation(ctx, db, "example-free", core.IsolationOpen, force))
	require.ErrorIs(t, store.LoosenProjectIsolation(ctx, db, "example-gone", core.IsolationOpen, force), store.ErrProjectNotFound)
}
