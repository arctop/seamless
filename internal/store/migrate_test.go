package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// Migration 055: a database from before it migrates with every proposal's
// decider unknown, the previous release's statements (which name their
// columns) keep working on the new schema, and a database whose migration
// record was cut back below 055 migrates forward again rather than failing on
// a duplicate column.
func TestMigration055_ResolvedBy(t *testing.T) {
	ctx := context.Background()
	db := openPartialDB(t, 55)
	v, err := SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 54, v, "the fixture is the previous release's schema")
	at := core.FormatTime(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	_, err = db.ExecContext(ctx, `INSERT INTO gardener_proposals (id, kind, payload, status, created_at, resolved_at)
		VALUES ('01MOLDDECIDEDAAAAAAAAAAAAA', 'archive', '{"key":"archive:a"}', 'applied', ?, ?)`, at, at)
	require.NoError(t, err)

	require.NoError(t, migrate(db, migrationList()))
	p, ok, err := ProposalByID(ctx, db, "01MOLDDECIDEDAAAAAAAAAAAAA")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, ProposalApplied, p.Status)
	require.Empty(t, p.ResolvedBy, "a decision from before the column reads as unknown")

	// The previous release, on the new schema: its insert and its resolve
	// name their columns, so the default fills the new one.
	_, err = db.ExecContext(ctx, `INSERT INTO gardener_proposals (id, kind, payload, status, created_at)
		VALUES ('01MOLDRELEASEAAAAAAAAAAAAA', 'archive', '{"key":"archive:b"}', 'pending', ?)`, at)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE gardener_proposals SET status = 'dismissed', resolved_at = ? WHERE id = ? AND status = 'pending'`,
		at, "01MOLDRELEASEAAAAAAAAAAAAA")
	require.NoError(t, err)
	p, _, err = ProposalByID(ctx, db, "01MOLDRELEASEAAAAAAAAAAAAA")
	require.NoError(t, err)
	require.Equal(t, ProposalDismissed, p.Status)
	require.Empty(t, p.ResolvedBy)

	_, err = db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version >= 55`)
	require.NoError(t, err)
	require.NoError(t, migrate(db, migrationList()), "a cut-back record migrates forward again")
	v, err = SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, LatestSchemaVersion(), v)
}

// Present is asked under the migration's lock: true records the version
// without running the SQL, false runs it, and its error stops the migration.
func TestMigrate_PresentSkipsWhatIsAlreadyThere(t *testing.T) {
	for _, tc := range []struct {
		name    string
		present bool
		err     error
		sql     string
		wantErr bool
	}{
		{name: "present: recorded, the SQL never runs", present: true, sql: "THIS IS NOT SQL"},
		{name: "absent: the SQL runs", sql: "THIS IS NOT SQL", wantErr: true},
		{name: "a failing probe stops the migration", err: errors.New("probe failed"), sql: "SELECT 1", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openPartialDB(t, 55)
			var ms []Migration
			for _, m := range migrationList() {
				if m.Version == 55 {
					m.SQL = tc.sql
					m.Present = func(context.Context, *sql.Conn) (bool, error) { return tc.present, tc.err }
				}
				ms = append(ms, m)
			}
			err := migrate(db, ms)
			v, verr := SchemaVersion(db)
			require.NoError(t, verr)
			if tc.wantErr {
				require.Error(t, err)
				require.Equal(t, 54, v, "nothing recorded")
				return
			}
			require.NoError(t, err)
			require.GreaterOrEqual(t, v, 55)
		})
	}
}

// Migration 056: the events table gains its (project_slug, ts) index -- on a
// database from before it, on a fresh one, and again on one whose migration
// record was cut back below 056 (IF NOT EXISTS: a re-run is harmless). It
// adds no table, so the filtered handle's inventory is unchanged.
func TestPvMigration056_EventsProjectIndex(t *testing.T) {
	ctx := context.Background()
	hasIndex := func(db *sql.DB) bool {
		t.Helper()
		var n int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_events_project' AND tbl_name = 'events'`).Scan(&n))
		return n == 1
	}
	db := openPartialDB(t, 56)
	v, err := SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 55, v, "the fixture is the previous release's schema")
	require.False(t, hasIndex(db))
	_, err = db.ExecContext(ctx, `INSERT INTO events (id, ts, kind, project_slug) VALUES ('01M80OLDEVENTAAAAAAAAAAAAA', ?, 'memory.written', 'example-ops')`,
		core.FormatTime(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)))
	require.NoError(t, err)

	require.NoError(t, migrate(db, migrationList()))
	require.True(t, hasIndex(db))
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE project_slug = 'example-ops'`).Scan(&n))
	require.Equal(t, 1, n, "the rows are untouched")

	_, err = db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version >= 56`)
	require.NoError(t, err)
	require.NoError(t, migrate(db, migrationList()), "a cut-back record migrates forward again")
	v, err = SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, LatestSchemaVersion(), v)
	require.True(t, hasIndex(db))

	require.True(t, hasIndex(openTestDB(t)), "a fresh database has it")
}
