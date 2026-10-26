package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpen_FreshDB_PragmasAndMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "seam.db")
	db, err := Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var fk int
	require.NoError(t, db.QueryRow("PRAGMA foreign_keys").Scan(&fk))
	require.Equal(t, 1, fk, "foreign_keys must be ON")

	var jm string
	require.NoError(t, db.QueryRow("PRAGMA journal_mode").Scan(&jm))
	require.Equal(t, "wal", strings.ToLower(jm))

	v, err := SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, len(migrationList()), v, "schema version must match the number of migrations")

	for _, tbl := range []string{
		"projects", "memories_index", "notes_index", "embeddings", "sessions",
		"tasks", "task_deps", "trials", "events", "retrieval_stats",
		"gardener_proposals", "settings", "jobs",
	} {
		var name string
		err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&name)
		require.NoErrorf(t, err, "table %q should exist", tbl)
	}

	var ftsName string
	require.NoError(t, db.QueryRow("SELECT name FROM sqlite_master WHERE name='fts'").Scan(&ftsName))
	require.Equal(t, "fts", ftsName)
}

// TestTasksClaimedByIndex pins migration 008. Asserting the index row exists
// would pass even if the planner ignored it, so this checks the plan instead --
// which is what caught the partial-index form silently degrading to a scan.
func TestTasksClaimedByIndex(t *testing.T) {
	db := openTestDB(t)

	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT `+taskCols+` FROM tasks
		WHERE claimed_by = ? ORDER BY created_at ASC, id ASC`, "01KXKE2XZNQEA9R0EQEXMGP9C9")
	require.NoError(t, err)
	t.Cleanup(func() { _ = rows.Close() })

	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		plan.WriteString(detail + "\n")
	}
	require.NoError(t, rows.Err())
	require.Contains(t, plan.String(), "idx_tasks_claimed_by",
		"the claim lookup must use the index, not scan the table")
}

func TestOpen_Idempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "seam.db")

	db, err := Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db2, err := Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db2.Close() })

	var count int
	require.NoError(t, db2.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&count))
	require.Equal(t, len(migrationList()), count, "reopening must not re-apply the migration")
}

func TestForeignKeys_Enforced(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "seam.db")
	db, err := Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	// task_deps references tasks(id); a dep pointing at non-existent tasks must
	// be rejected end-to-end (proves foreign_keys is enforced, not just set).
	_, err = db.Exec("INSERT INTO task_deps(task_id, depends_on) VALUES('missing-a','missing-b')")
	require.Error(t, err)
}

func TestFTS_InsertMatchDelete(t *testing.T) {
	db := openTestDB(t)

	_, err := db.Exec(
		`INSERT INTO fts(item_id, kind, project, title, name, description, body)
		 VALUES(?,?,?,?,?,?,?)`,
		"01ABC", "memory", "seam", "", "chroma-boot-race",
		"chroma races on boot", "The chroma container loses a startup race.",
	)
	require.NoError(t, err)

	var itemID, kind string
	err = db.QueryRow("SELECT item_id, kind FROM fts WHERE fts MATCH 'chroma' LIMIT 1").Scan(&itemID, &kind)
	require.NoError(t, err)
	require.Equal(t, "01ABC", itemID)
	require.Equal(t, "memory", kind)

	// Stemming: 'race' should match 'races'/'racing' via the porter tokenizer.
	var n int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM fts WHERE fts MATCH 'race'").Scan(&n))
	require.Equal(t, 1, n)

	// A self-contained fts5 table supports plain DELETE by a stored column.
	_, err = db.Exec("DELETE FROM fts WHERE item_id = ?", "01ABC")
	require.NoError(t, err)
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM fts WHERE fts MATCH 'chroma'").Scan(&n))
	require.Equal(t, 0, n)
}

func TestTableCount(t *testing.T) {
	db := openTestDB(t)

	n, err := TableCount(db)
	require.NoError(t, err)
	// 13 domain tables + schema_migrations + the fts virtual table's own entry
	// is excluded by name filter; assert a sane lower bound instead of an exact
	// count so adding indexes/tables later does not brittle-break this.
	require.GreaterOrEqual(t, n, 14)
}

// TestLatestSchemaVersion_MatchesAFreshDB pins the compiled-vs-applied pair: a
// database this binary just created must report exactly the version this binary
// compiles, or the archive importer's newer-schema refusal would fire on
// archives it wrote itself.
func TestLatestSchemaVersion_MatchesAFreshDB(t *testing.T) {
	db := openTestDB(t)

	applied, err := SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, LatestSchemaVersion(), applied,
		"a freshly opened DB must sit at the newest compiled migration")

	ms := migrationList()
	require.Equal(t, ms[len(ms)-1].Version, LatestSchemaVersion(),
		"LatestSchemaVersion must be the last migrationList entry, not a transcribed number")
}

func TestOpenExisting_AbsentFileIsAnErrorAndCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "seam.db")

	db, err := OpenExisting(dbPath)
	require.Error(t, err)
	require.Nil(t, db)
	require.ErrorIs(t, err, os.ErrNotExist)

	_, statErr := os.Stat(dbPath)
	require.ErrorIs(t, statErr, os.ErrNotExist, "OpenExisting must never create the database")
}

// TestOpenExisting_DoesNotMigrate is the whole point of the function: a newer
// binary reading a data dir served by an older running daemon must not migrate
// the live schema out from under it. The PRAGMA assertions pin the other half
// of the contract -- same DSN as Open, only the migrate step removed.
func TestOpenExisting_DoesNotMigrate(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "seam.db")

	// Build a deliberately one-version-behind database.
	all := migrationList()
	require.Greater(t, len(all), 1)
	raw, err := sql.Open("sqlite", dsn(dbPath))
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	require.NoError(t, migrate(raw, all[:len(all)-1]))
	require.NoError(t, raw.Close())

	behind := all[len(all)-2].Version

	db, err := OpenExisting(dbPath)
	require.NoError(t, err)

	v, err := SchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, behind, v, "OpenExisting must leave the schema exactly where it found it")
	require.Less(t, v, LatestSchemaVersion(), "the fixture must actually be behind")

	var fk int
	require.NoError(t, db.QueryRow("PRAGMA foreign_keys").Scan(&fk))
	require.Equal(t, 1, fk, "foreign_keys must be ON, same DSN as Open")
	var jm string
	require.NoError(t, db.QueryRow("PRAGMA journal_mode").Scan(&jm))
	require.Equal(t, "wal", strings.ToLower(jm))
	require.NoError(t, db.Close())

	// Open on the same file is the contrast: it DOES migrate forward.
	migrated, err := Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = migrated.Close() })
	v, err = SchemaVersion(migrated)
	require.NoError(t, err)
	require.Equal(t, LatestSchemaVersion(), v)
}
