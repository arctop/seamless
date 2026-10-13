package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
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
	db, err := updateTestDB(t, filepath.Join(dataDir, "seam.db"))
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
		_, err := restoreDatabase(dataDir, res.path, id, res.schema+5)
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
	suffix, err := restoreDatabase(dataDir, res.path, id, res.schema)
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

func TestRestoreDatabase_RefusesWhatIsNotABackup(t *testing.T) {
	dataDir := t.TempDir()
	notArchive := filepath.Join(t.TempDir(), "x.tar.gz")
	require.NoError(t, os.WriteFile(notArchive, []byte("hello"), 0o600))
	_, err := restoreDatabase(dataDir, notArchive, testAttemptID(t), 1)
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
