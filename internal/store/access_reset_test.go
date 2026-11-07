package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
)

// A restored database's live leases, console unlocks (pending ones included)
// and read grants all end; one already revoked keeps its own end.
func TestResetRestoredAccess_EndsEveryLivePermission(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

	proc := access.ProcessToken{PID: 4242, Version: 7, Boot: "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"}
	a, err := CreateAccessApproval(ctx, db, leaseReq("example-finance", proc), now)
	require.NoError(t, err)
	live, err := ApproveAccessFor(ctx, db, a.ID, a.Challenge, "daemon-account", time.Hour, now)
	require.NoError(t, err)

	other := access.ProcessToken{PID: 4343, Version: 8, Boot: proc.Boot}
	b, err := CreateAccessApproval(ctx, db, leaseReq("example-finance", other), now)
	require.NoError(t, err)
	gone, err := ApproveAccessFor(ctx, db, b.ID, b.Challenge, "daemon-account", time.Hour, now)
	require.NoError(t, err)
	require.NoError(t, RevokeAccessLease(ctx, db, gone.ID, LeaseRevokedSessionEnded, now))

	u, err := BeginConsoleUnlock(ctx, db, "example-finance", HashConsoleToken("browser-a"), 3*time.Hour, 5*time.Minute, now)
	require.NoError(t, err)
	_, err = GrantConsoleUnlock(ctx, db, u.ID, HashConsoleToken("browser-a"), "credential-1", now)
	require.NoError(t, err)
	_, err = BeginConsoleUnlock(ctx, db, "example-finance", HashConsoleToken("browser-b"), 3*time.Hour, 5*time.Minute, now)
	require.NoError(t, err)

	g, err := CreateGrantRequest(ctx, db, grantReq("S1"), now)
	require.NoError(t, err)
	_, err = ApproveAccessGrant(ctx, db, g.ID, g.Challenge, "owner", time.Hour, now)
	require.NoError(t, err)

	later := now.Add(time.Minute)
	got, err := ResetRestoredAccess(ctx, db, later)
	require.NoError(t, err)
	require.Equal(t, RestoredAccess{Leases: 1, Unlocks: 2, Grants: 1}, got)
	require.Equal(t, int64(4), got.Total())

	leases, err := ActiveAccessLeases(ctx, db, proc.Key(), later)
	require.NoError(t, err)
	require.Empty(t, leases)
	var reason string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT revoked_reason FROM access_leases WHERE id = ?`, live.ID).Scan(&reason))
	require.Equal(t, LeaseRevokedReset, reason)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT revoked_reason FROM access_leases WHERE id = ?`, gone.ID).Scan(&reason))
	require.Equal(t, LeaseRevokedSessionEnded, reason, "a lease already revoked keeps its own end")
	grants, err := LiveAccessGrants(ctx, db, later)
	require.NoError(t, err)
	require.Empty(t, grants)
	var open int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM console_unlocks WHERE status != ?`, ConsoleUnlockEnded).Scan(&open))
	require.Zero(t, open)

	again, err := ResetRestoredAccess(ctx, db, later)
	require.NoError(t, err)
	require.Zero(t, again.Total(), "nothing live is left to end")
}

// An update rollback's snapshot may predate the access tables: they are
// skipped, not an error.
func TestResetRestoredAccess_ADatabaseWithoutTheTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seam.db")
	db, err := sql.Open("sqlite", dsn(path))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY)`)
	require.NoError(t, err)

	got, err := ResetRestoredAccess(context.Background(), db, time.Now())
	require.NoError(t, err)
	require.Zero(t, got.Total())
}
