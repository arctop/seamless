package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RestoredAccess counts what ResetRestoredAccess ended.
type RestoredAccess struct {
	Leases, Unlocks, Grants int64
}

// Total is how many permissions were ended in all.
func (r RestoredAccess) Total() int64 { return r.Leases + r.Unlocks + r.Grants }

// ResetRestoredAccess ends every live permission a restored database brings
// back: an agent tree's access leases, a browser's console unlocks (and every
// pending unlock, passkey or terminal) and the read grants. A snapshot holds them
// as they stood when it was taken, including ones revoked since, so a restore
// that kept them would reopen what the owner had closed. It fails closed:
// locked projects' agents and browsers ask again.
//
// Both restores call it: seamlessd import (internal/archive), on a database it
// has migrated forward, and an update rollback that puts the pre-update
// database back (cmd/seamlessd), on one it opened without migrating. The
// rollback's snapshot may predate the access tables, so a table the database
// does not have is skipped, not an error.
func ResetRestoredAccess(ctx context.Context, db *sql.DB, now time.Time) (RestoredAccess, error) {
	const op = "store.ResetRestoredAccess"
	var got RestoredAccess
	steps := []struct {
		table string
		n     *int64
		end   func() (int64, error)
	}{
		{"access_leases", &got.Leases, func() (int64, error) {
			return RevokeAllAccessLeases(ctx, db, LeaseRevokedReset, now)
		}},
		{"console_unlocks", &got.Unlocks, func() (int64, error) {
			return EndAllConsoleUnlocks(ctx, db, ConsoleUnlockEndReset, now)
		}},
		{"access_grants", &got.Grants, func() (int64, error) {
			return RevokeAllAccessGrants(ctx, db, LeaseRevokedReset, now)
		}},
	}
	for _, s := range steps {
		ok, err := tableExists(ctx, db, s.table)
		if err != nil {
			return got, fmt.Errorf("%s: %w", op, err)
		}
		if !ok {
			continue
		}
		n, err := s.end()
		if err != nil {
			return got, fmt.Errorf("%s: %w", op, err)
		}
		*s.n = n
	}
	return got, nil
}

// tableExists reports whether the database has a table named name.
func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		return false, fmt.Errorf("look for table %s: %w", name, err)
	}
	return n > 0, nil
}
