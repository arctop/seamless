package console

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// openFreshDB is store.Open on a path that does not exist yet, without paying
// for the migrations on every call.
//
// Under -race a fresh store.Open costs about half a second (every embedded
// migration runs through the instrumented, transpiled SQLite), against about
// 10ms for opening an already-migrated file, and nearly every console test
// opens at least one. So the package migrates one template database once and
// each caller gets its own byte copy of it at path: every test still owns a
// separate, fresh database file, and nothing is shared but the starting bytes.
// store.Open on the copy still runs its migrate step, which finds every version
// applied and returns. TestOpenFreshDB_MatchesAFreshMigration pins the copy to
// what a fresh store.Open produces.
//
// path must not exist: an existing database is refused, never overwritten.
func openFreshDB(t *testing.T, path string) (*sql.DB, error) {
	t.Helper()
	tpl, err := freshDBTemplate(t)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("openFreshDB: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("openFreshDB: %w", err)
	}
	if _, err := f.Write(tpl); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("openFreshDB: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("openFreshDB: close %s: %w", path, err)
	}
	return store.Open(path)
}

var freshDB struct {
	once sync.Once
	tpl  []byte
	err  error
}

// freshDBTemplate returns the bytes of a freshly migrated database, built on
// first use in the first caller's temp dir. The bytes are all it keeps, so
// that dir's ordinary cleanup takes the build files with it.
func freshDBTemplate(t *testing.T) ([]byte, error) {
	t.Helper()
	freshDB.once.Do(func() { freshDB.tpl, freshDB.err = buildFreshDBTemplate(t.TempDir()) })
	return freshDB.tpl, freshDB.err
}

// buildFreshDBTemplate migrates a database in dir with store.Open and
// snapshots it with VACUUM INTO, which writes one self-contained file whatever
// the source's WAL holds, so the copy never depends on a checkpoint having run.
func buildFreshDBTemplate(dir string) ([]byte, error) {
	db, err := store.Open(filepath.Join(dir, "template.db"))
	if err != nil {
		return nil, fmt.Errorf("freshDBTemplate: %w", err)
	}
	snap := filepath.Join(dir, "snapshot.db")
	if _, err := db.Exec(`VACUUM INTO ?`, snap); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("freshDBTemplate: snapshot: %w", err)
	}
	if err := db.Close(); err != nil {
		return nil, fmt.Errorf("freshDBTemplate: %w", err)
	}
	tpl, err := os.ReadFile(snap)
	if err != nil {
		return nil, fmt.Errorf("freshDBTemplate: %w", err)
	}
	return tpl, nil
}

// The copy is a fresh database: the same schema, the same rows (migrations seed
// some), every migration applied and none re-run, the connection PRAGMAs
// store.Open sets, and a file of its own.
func TestOpenFreshDB_MatchesAFreshMigration(t *testing.T) {
	want, err := store.Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = want.Close() })

	path := filepath.Join(t.TempDir(), "seam.db")
	got, err := openFreshDB(t, path)
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
	other, err := openFreshDB(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	_, err = got.Exec(`INSERT INTO settings (key, value) VALUES ('freshdb_probe', '{}')`)
	require.NoError(t, err)
	var n int
	require.NoError(t, other.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'freshdb_probe'`).Scan(&n))
	require.Zero(t, n)

	// An existing database is refused, never overwritten.
	_, err = openFreshDB(t, path)
	require.ErrorIs(t, err, os.ErrExist)
	require.NoError(t, got.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'freshdb_probe'`).Scan(&n))
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
