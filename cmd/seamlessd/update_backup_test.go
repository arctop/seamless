package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

func TestBackupName_RoundTrips(t *testing.T) {
	at := time.Date(2026, 10, 9, 12, 30, 5, 0, time.FixedZone("x", 3600))
	name := backupName(mustVersion(t, "0.7.2"), at)
	require.Equal(t, "pre-update-v0.7.2-20261009T113005Z.tar.gz", name)
	got, ok := backupTime(name)
	require.True(t, ok)
	require.True(t, got.Equal(at))
	for _, other := range []string{"notes.tar.gz", "pre-update-v0.7.2.tar.gz", "pre-update-vX-20261009T113005Z.tar.gz",
		"pre-update-v0.7.2-yesterday.tar.gz", "pre-update-v0.7.2-20261009T113005Z.zip"} {
		_, ok := backupTime(other)
		require.False(t, ok, other)
	}
}

func TestPruneBackups_KeepsTheNewestTwoAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var names []string
	for i, v := range []string{"0.7.0", "0.7.2", "0.6.9", "0.7.1"} {
		name := backupName(mustVersion(t, v), base.Add(time.Duration(i)*time.Hour))
		names = append(names, name)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "my-own-export.tar.gz"), nil, 0o600))

	require.NoError(t, pruneBackups(dir, backupKeep))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	// By time, not by version or name: the two taken last stay.
	require.ElementsMatch(t, []string{names[2], names[3], "my-own-export.tar.gz"}, left)
}

// backupDataDir is a data dir with a migrated database and one memory file.
func backupDataDir(t *testing.T) (dataDir, exe string) {
	t.Helper()
	dataDir = filepath.Join(t.TempDir(), "data")
	db, err := storetest.OpenFresh(t, filepath.Join(dataDir, "seam.db"))
	require.NoError(t, err)
	require.NoError(t, db.Close())
	mem := filepath.Join(dataDir, "memory", "proj", "a.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(mem), 0o700))
	require.NoError(t, os.WriteFile(mem, []byte("---\nid: x\n---\nbody\n"), 0o600))
	exe = filepath.Join(t.TempDir(), "seamlessd")
	require.NoError(t, os.WriteFile(exe, []byte("binary"), 0o755))
	return dataDir, exe
}

func TestTakeBackup(t *testing.T) {
	dataDir, exe := backupDataDir(t)
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	res, err := takeBackup(context.Background(), backupRequest{
		dataDir: dataDir, from: mustVersion(t, "0.7.2"), attemptID: testAttemptID(t), at: at,
		exe: exe, version: "0.7.2+abc", freeBytes: func(string) (uint64, error) { return 1 << 40, nil },
	})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dataDir, "backups", "pre-update-v0.7.2-20261009T120000Z.tar.gz"), res.path)
	require.Equal(t, store.LatestSchemaVersion(), res.schema)
	want, err := fileSHA256(exe)
	require.NoError(t, err)
	require.Equal(t, want, res.binSHA)

	fi, err := os.Stat(res.path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(res.path))
	require.NoError(t, err)
	require.Len(t, entries, 1, "the staging directory and the temp file are gone")
}

func TestTakeBackup_RefusesWithoutRoom(t *testing.T) {
	dataDir, exe := backupDataDir(t)
	_, err := takeBackup(context.Background(), backupRequest{
		dataDir: dataDir, from: mustVersion(t, "0.7.2"), attemptID: testAttemptID(t), at: time.Now(),
		exe: exe, freeBytes: func(string) (uint64, error) { return backupSpare - 1, nil },
	})
	require.ErrorIs(t, err, errNoSpace)
	_, err = os.Stat(filepath.Join(dataDir, "backups", "pre-update-v0.7.2-"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestTakeBackup_FailsWithoutADatabase(t *testing.T) {
	dataDir := t.TempDir()
	exe := filepath.Join(t.TempDir(), "seamlessd")
	require.NoError(t, os.WriteFile(exe, nil, 0o755))
	_, err := takeBackup(context.Background(), backupRequest{
		dataDir: dataDir, from: mustVersion(t, "0.7.2"), attemptID: testAttemptID(t), at: time.Now(),
		exe: exe, freeBytes: func(string) (uint64, error) { return 1 << 40, nil },
	})
	require.Error(t, err)
}

func TestRestoreDatabase(t *testing.T) {
	dataDir, exe := backupDataDir(t)
	id := testAttemptID(t)
	res, err := takeBackup(context.Background(), backupRequest{
		dataDir: dataDir, from: mustVersion(t, "0.7.2"), attemptID: id, at: time.Now(),
		exe: exe, freeBytes: func(string) (uint64, error) { return 1 << 40, nil },
	})
	require.NoError(t, err)

	dbPath := filepath.Join(dataDir, "seam.db")
	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, res.schema+1)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	t.Run("a backup of another schema is refused", func(t *testing.T) {
		_, err := restoreDatabase(dataDir, res.path, id, res.schema+5, nil)
		require.ErrorContains(t, err, "schema")
		got, err := dbSchemaVersion(dbPath)
		require.NoError(t, err)
		require.Equal(t, res.schema+1, got, "nothing moved")
		_, err = os.Stat(dbPath + ".restore-" + id)
		require.ErrorIs(t, err, os.ErrNotExist)
	})

	// A WAL beside the migrated database must move with it, or it would be
	// replayed into the restored one. (Written last: opening the database
	// discards a WAL it cannot read.)
	require.NoError(t, os.WriteFile(dbPath+"-wal", []byte("stale wal"), 0o600))
	suffix, err := restoreDatabase(dataDir, res.path, id, res.schema, nil)
	require.NoError(t, err)
	_, err = os.Stat(dbPath + "-wal")
	require.ErrorIs(t, err, os.ErrNotExist, "no WAL is left beside the restored database")
	require.Equal(t, ".pre-rollback-"+id, suffix)
	got, err := dbSchemaVersion(dbPath)
	require.NoError(t, err)
	require.Equal(t, res.schema, got)
	aside, err := os.ReadFile(dbPath + "-wal" + suffix)
	require.NoError(t, err)
	require.Equal(t, "stale wal", string(aside))
	migrated, err := dbSchemaVersion(dbPath + suffix)
	require.NoError(t, err)
	require.Equal(t, res.schema+1, migrated)
}

// When the restored database's access cannot be ended, the rollback puts the
// migrated database back: unrestoreDatabase undoes restoreDatabase, keeping the
// restored files (and the WAL opening them left) aside, never deleting them.
func TestUnrestoreDatabase_PutsTheMigratedDatabaseBack(t *testing.T) {
	dataDir, exe := backupDataDir(t)
	id := testAttemptID(t)
	res, err := takeBackup(context.Background(), backupRequest{
		dataDir: dataDir, from: mustVersion(t, "0.7.2"), attemptID: id, at: time.Now(),
		exe: exe, freeBytes: func(string) (uint64, error) { return 1 << 40, nil },
	})
	require.NoError(t, err)

	dbPath := filepath.Join(dataDir, "seam.db")
	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, res.schema+1)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	suffix, err := restoreDatabase(dataDir, res.path, id, res.schema, nil)
	require.NoError(t, err)
	got, err := dbSchemaVersion(dbPath)
	require.NoError(t, err)
	require.Equal(t, res.schema, got)
	// The restored database was opened (to end its access), which can leave a WAL.
	require.NoError(t, os.WriteFile(dbPath+"-wal", []byte("restored wal"), 0o600))

	require.NoError(t, unrestoreDatabase(dataDir, suffix, id))
	got, err = dbSchemaVersion(dbPath)
	require.NoError(t, err)
	require.Equal(t, res.schema+1, got, "the migrated database is back")
	require.FileExists(t, dbPath+".unrestored-"+id, "the restored one is kept aside")
	wal, err := os.ReadFile(dbPath + "-wal.unrestored-" + id)
	require.NoError(t, err)
	require.Equal(t, "restored wal", string(wal), "its WAL moved with it")
	_, err = os.Stat(dbPath + suffix)
	require.ErrorIs(t, err, os.ErrNotExist, "nothing is left under the pre-rollback name")
}

func TestRestoredAccessWords(t *testing.T) {
	for _, tc := range []struct {
		in   store.RestoredAccess
		want string
	}{
		{store.RestoredAccess{}, "no access"},
		{store.RestoredAccess{Leases: 1}, "1 access lease"},
		{store.RestoredAccess{Leases: 2, Grants: 1}, "2 access leases and 1 read grant"},
		{store.RestoredAccess{Leases: 1, Unlocks: 3, Grants: 2}, "1 access lease, 3 console unlocks and 2 read grants"},
	} {
		require.Equal(t, tc.want, restoredAccessWords(tc.in))
	}
}

// consoleLockProjects fences each project in the database at dbPath and locks
// it from the console, the way the console's lock ceremony leaves it.
func consoleLockProjects(t *testing.T, dbPath string, projects ...string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	for _, p := range projects {
		_, err := store.EnsureProject(ctx, db, p, p)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, p, core.IsolationConfidential))
		require.NoError(t, store.AddConsoleLock(ctx, db, p, "cred", time.Now()))
	}
}

// execDB runs statements on the database at dbPath without migrating it.
func execDB(t *testing.T, dbPath string, stmts ...string) {
	t.Helper()
	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	for _, s := range stmts {
		_, err := db.Exec(s)
		require.NoError(t, err, s)
	}
}

// restoreLeftovers lists the files a restore leaves beside seam.db under a
// temp or aside name.
func restoreLeftovers(t *testing.T, dataDir string) []string {
	t.Helper()
	var out []string
	for _, pattern := range []string{"seam.db*.restore-*", "seam.db*.pre-rollback-*", "seam.db*.unrestored-*"} {
		m, err := filepath.Glob(filepath.Join(dataDir, pattern))
		require.NoError(t, err)
		out = append(out, m...)
	}
	return out
}

func TestDBConsoleLocks(t *testing.T) {
	ctx := context.Background()
	dataDir, _ := backupDataDir(t)
	dbPath := filepath.Join(dataDir, "seam.db")

	got, err := dbConsoleLocks(ctx, dbPath)
	require.NoError(t, err)
	require.Empty(t, got)

	consoleLockProjects(t, dbPath, "example-b", "example-a")
	got, err = dbConsoleLocks(ctx, dbPath)
	require.NoError(t, err)
	require.Equal(t, []string{"example-a", "example-b"}, got)

	t.Run("a database without the table has none", func(t *testing.T) {
		old := filepath.Join(t.TempDir(), "seam.db")
		db, err := storetest.OpenFresh(t, old)
		require.NoError(t, err)
		require.NoError(t, db.Close())
		execDB(t, old, `DROP TABLE console_locks`)
		got, err := dbConsoleLocks(ctx, old)
		require.NoError(t, err)
		require.Empty(t, got)
	})
	t.Run("a lock it cannot read is an error", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "seam.db")
		db, err := storetest.OpenFresh(t, bad)
		require.NoError(t, err)
		require.NoError(t, db.Close())
		execDB(t, bad, `INSERT INTO console_locks (project_slug, credential, locked_at) VALUES ('example-a', 'cred', 'not a time')`)
		_, err = dbConsoleLocks(ctx, bad)
		require.Error(t, err)
	})
	t.Run("a missing database is not created", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "seam.db")
		_, err := dbConsoleLocks(ctx, missing)
		require.ErrorIs(t, err, os.ErrNotExist)
		_, err = os.Stat(missing)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
}

func TestKeepConsoleLocks(t *testing.T) {
	ctx := context.Background()
	dataDir, _ := backupDataDir(t)
	snapshot := filepath.Join(dataDir, "seam.db")
	consoleLockProjects(t, snapshot, "example-a", "example-b")

	for _, tc := range []struct {
		name     string
		locked   []string
		missing  []string
		returned []string
		msg      string
	}{
		{name: "the same locks", locked: []string{"example-a", "example-b"}},
		{name: "none in the migrated database", returned: []string{"example-a", "example-b"}},
		{name: "one removed after the backup", locked: []string{"example-b"}, returned: []string{"example-a"}},
		{name: "one added after the backup", locked: []string{"example-a", "example-c"}, missing: []string{"example-c"},
			msg: "the backup's database does not hold the console lock on example-c"},
		{name: "two added after the backup", locked: []string{"example-c", "example-d"}, missing: []string{"example-c", "example-d"},
			msg: "the backup's database does not hold the console locks on example-c and example-d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			returned := []string{"stale"}
			err := keepConsoleLocks(ctx, tc.locked, &returned)(snapshot)
			if tc.missing == nil {
				require.NoError(t, err)
				require.Equal(t, tc.returned, returned)
				return
			}
			var missing *missingLocksError
			require.ErrorAs(t, err, &missing)
			require.Equal(t, tc.missing, missing.projects)
			require.EqualError(t, err, tc.msg)
		})
	}

	t.Run("a snapshot it cannot read fails, and not as a missing lock", func(t *testing.T) {
		var returned []string
		err := keepConsoleLocks(ctx, nil, &returned)(filepath.Join(t.TempDir(), "seam.db"))
		require.ErrorContains(t, err, "read the backup's console locks")
		var missing *missingLocksError
		require.False(t, errors.As(err, &missing))
	})
}

// The vet reads the extracted snapshot while the live database is still in
// place: a refusal moves nothing, and a vetted restore leaves nothing under
// the snapshot's temp name.
func TestRestoreDatabase_TheVetRunsBeforeAnythingMoves(t *testing.T) {
	dataDir, exe := backupDataDir(t)
	id := testAttemptID(t)
	dbPath := filepath.Join(dataDir, "seam.db")
	consoleLockProjects(t, dbPath, "example-a")
	res, err := takeBackup(context.Background(), backupRequest{
		dataDir: dataDir, from: mustVersion(t, "0.7.2"), attemptID: id, at: time.Now(),
		exe: exe, freeBytes: func(string) (uint64, error) { return 1 << 40, nil },
	})
	require.NoError(t, err)
	execDB(t, dbPath, fmt.Sprintf(`INSERT INTO schema_migrations(version) VALUES (%d)`, res.schema+1))

	refused := errors.New("refused")
	var vetted string
	_, err = restoreDatabase(dataDir, res.path, id, res.schema, func(snapshot string) error {
		vetted = snapshot
		got, err := dbSchemaVersion(snapshot)
		require.NoError(t, err)
		require.Equal(t, res.schema, got, "the vet reads the backup's database")
		live, err := dbSchemaVersion(dbPath)
		require.NoError(t, err)
		require.Equal(t, res.schema+1, live, "while the migrated one is still seam.db")
		return refused
	})
	require.ErrorIs(t, err, refused)
	require.NotEmpty(t, vetted)
	got, err := dbSchemaVersion(dbPath)
	require.NoError(t, err)
	require.Equal(t, res.schema+1, got, "nothing moved")
	require.Empty(t, restoreLeftovers(t, dataDir))

	var relocked []string
	suffix, err := restoreDatabase(dataDir, res.path, id, res.schema, keepConsoleLocks(context.Background(), []string{"example-a"}, &relocked))
	require.NoError(t, err)
	require.Empty(t, relocked)
	got, err = dbSchemaVersion(dbPath)
	require.NoError(t, err)
	require.Equal(t, res.schema, got)
	left := restoreLeftovers(t, dataDir)
	for _, p := range left {
		require.Contains(t, filepath.Base(p), suffix, "only the migrated files are left aside")
	}
}

func TestAndList(t *testing.T) {
	require.Equal(t, "", andList(nil))
	require.Equal(t, "a", andList([]string{"a"}))
	require.Equal(t, "a and b", andList([]string{"a", "b"}))
	require.Equal(t, "a, b and c", andList([]string{"a", "b", "c"}))
}

func TestRestoreDatabase_RefusesWhatIsNotABackup(t *testing.T) {
	dataDir := t.TempDir()
	notArchive := filepath.Join(t.TempDir(), "x.tar.gz")
	require.NoError(t, os.WriteFile(notArchive, []byte("hello"), 0o600))
	_, err := restoreDatabase(dataDir, notArchive, testAttemptID(t), 1, nil)
	require.Error(t, err)
	matches, err := filepath.Glob(filepath.Join(dataDir, "*"))
	require.NoError(t, err)
	require.Empty(t, matches)
}

func TestDBSchemaVersion_NeverMigrates(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "seam.db")
	_, err := dbSchemaVersion(dbPath)
	require.Error(t, err, "a missing database is not created")
	_, err = os.Stat(dbPath)
	require.ErrorIs(t, err, os.ErrNotExist)
}
