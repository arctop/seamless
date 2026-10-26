package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// setSchemaVersion opens a fresh migrated database in a temp dir and rewrites
// schema_migrations so its newest applied version is applied, as if a newer
// binary (applied > compiled) or an older one (applied < compiled) had left it.
func setSchemaVersion(t *testing.T, applied int) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version > ?`, applied)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT OR IGNORE INTO schema_migrations (version) VALUES (?)`, applied)
	require.NoError(t, err)
	return dbPath
}

// The premise of serve's schema WARN: store.Open applies nothing to a database
// AHEAD of this binary and opens it anyway. If migrate ever starts refusing such
// a database, serve fails before it can warn, and this test says so.
func TestStoreOpen_DatabaseAheadOfTheBinaryOpensUnmigrated(t *testing.T) {
	ahead := store.LatestSchemaVersion() + 5
	dbPath := setSchemaVersion(t, ahead)

	db, err := store.Open(dbPath)
	require.NoError(t, err, "a newer schema must still open, so serve can say what it is running on")
	defer func() { require.NoError(t, db.Close()) }()
	v, err := store.SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, ahead, v, "nothing is applied to, or rolled back from, a newer schema")
}

// schemaVersionCheck is doctor's half of the schema-ahead diagnosis; serve's
// startup WARN prints the same schemaAheadCause and schemaAheadFix, so the two
// can never disagree about why it happened or what to do.
func TestSchemaVersionCheck(t *testing.T) {
	compiled := store.LatestSchemaVersion()
	tests := []struct {
		name       string
		applied    int
		wantStatus checkStatus
		wantDetail string
	}{
		{
			name: "current", applied: compiled, wantStatus: statusInfo,
			wantDetail: fmt.Sprintf("v%d applied / v%d compiled", compiled, compiled),
		},
		{
			// Unreachable through doctor itself, whose store.Open migrates first,
			// but the check reports the pair rather than guessing.
			name: "behind", applied: compiled - 3, wantStatus: statusInfo,
			wantDetail: fmt.Sprintf("v%d applied / v%d compiled", compiled-3, compiled),
		},
		{
			name: "ahead: a downgrade or a rollback that kept the newer database", applied: compiled + 2,
			wantStatus: statusWarn,
			wantDetail: fmt.Sprintf("v%d applied / v%d compiled -- %s; %s",
				compiled+2, compiled, schemaAheadCause, schemaAheadFix),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, err := store.OpenExisting(setSchemaVersion(t, tt.applied))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })

			got := schemaVersionCheck(db)
			require.Equal(t, "schema version", got.name)
			require.Equal(t, tt.wantStatus, got.status, got.detail)
			require.Equal(t, tt.wantDetail, got.detail)
		})
	}
}

// Both halves of the diagnosis name the cause and the fix the plan settled on:
// with automatic updates, a newer schema means a downgrade or a rollback that
// could not restore the database, and the way out is the newer release again or
// a backup from before it, restored with seamlessd import.
func TestSchemaAheadWording(t *testing.T) {
	require.Contains(t, schemaAheadCause, "newer seamlessd")
	require.Contains(t, schemaAheadCause, "downgrade")
	require.Contains(t, schemaAheadCause, "rollback")
	require.Contains(t, schemaAheadFix, "newer release again")
	require.Contains(t, schemaAheadFix, "seamlessd update")
	require.Contains(t, schemaAheadFix, "seamlessd import")
}

func TestSchemaVersionCheck_UnreadableSchemaWarns(t *testing.T) {
	// A SQLite file with no schema_migrations table at all.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "bare.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.Exec(`CREATE TABLE unrelated (x INTEGER)`)
	require.NoError(t, err)

	got := schemaVersionCheck(db)
	require.Equal(t, statusWarn, got.status)
	require.Contains(t, got.detail, "cannot read schema_migrations")
}
