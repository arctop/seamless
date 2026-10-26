package storetest

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// The copy is a fresh database: the same schema, the same rows (migrations seed
// some), every migration applied and none re-run, the connection PRAGMAs
// store.Open sets, and a file of its own. This is the one guard for every
// package that opens its fresh databases through OpenFresh.
func TestOpenFresh_MatchesAFreshMigration(t *testing.T) {
	want, err := store.Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = want.Close() })

	path := filepath.Join(t.TempDir(), "seam.db")
	got, err := OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = got.Close() })

	v, err := store.SchemaVersion(got)
	require.NoError(t, err)
	require.Equal(t, store.LatestSchemaVersion(), v)

	require.Equal(t, dumpDB(t, want), dumpDB(t, got))

	for pragma, value := range map[string]string{"journal_mode": "wal", "foreign_keys": "1", "busy_timeout": "5000"} {
		var wantV, gotV string
		require.NoError(t, want.QueryRow("PRAGMA "+pragma).Scan(&wantV))
		require.NoError(t, got.QueryRow("PRAGMA "+pragma).Scan(&gotV))
		require.Equal(t, value, wantV, pragma)
		require.Equal(t, wantV, gotV, pragma)
	}

	// A second copy is a separate database: a write to one is not in the other.
	other, err := OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	_, err = got.Exec(`INSERT INTO settings (key, value) VALUES ('storetest_probe', '{}')`)
	require.NoError(t, err)
	var n int
	require.NoError(t, other.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'storetest_probe'`).Scan(&n))
	require.Zero(t, n)

	// An existing database is refused, never overwritten.
	_, err = OpenFresh(t, path)
	require.ErrorIs(t, err, os.ErrExist)
	require.NoError(t, got.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'storetest_probe'`).Scan(&n))
	require.Equal(t, 1, n)
}

// dumpDB renders a database's schema and every table's rows, in a stable
// order. schema_migrations contributes its versions only: applied_at is the
// wall clock of the run that applied them.
func dumpDB(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder
	var tables []string
	for _, r := range dumpRows(t, db, `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`) {
		fmt.Fprintf(&b, "%v %v %v\n%v\n", r...)
		if fmt.Sprint(r[0]) == "table" {
			tables = append(tables, fmt.Sprint(r[1]))
		}
	}
	require.NotEmpty(t, tables)

	for _, table := range tables {
		q := `SELECT * FROM "` + table + `"`
		if table == "schema_migrations" {
			q = `SELECT version FROM schema_migrations`
		}
		var lines []string
		for _, r := range dumpRows(t, db, q) {
			lines = append(lines, fmt.Sprintf("%q", r))
		}
		sort.Strings(lines)
		fmt.Fprintf(&b, "rows %s %d\n%s\n", table, len(lines), strings.Join(lines, "\n"))
	}
	return b.String()
}

// dumpRows returns every row q yields, each column as the driver scanned it.
func dumpRows(t *testing.T, db *sql.DB, q string) [][]any {
	t.Helper()
	rows, err := db.Query(q)
	require.NoError(t, err, q)
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		out = append(out, vals)
	}
	require.NoError(t, rows.Err())
	return out
}
