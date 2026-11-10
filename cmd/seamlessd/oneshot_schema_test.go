package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// The bug these pin: doctor, family, map-repo, unmap-repo and retire-project
// opened the configured database with store.Open, which creates and migrates. A
// newer build run by hand (make doctor above all, which passes no
// SEAMLESS_CONFIG) migrated the live database under the running daemon. The
// schema is the daemon's to move, so an existing database is never changed here.

// writeDoctorConfig writes a server config naming dataDir and points doctor at a
// throwaway tree: port 1 answers nothing, no LLM key means no probe, and the
// $HOME and $CODEX_HOME it inspects are empty.
func writeDoctorConfig(t *testing.T, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "seamless.yaml")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join([]string{
		"role: server",
		"addr: 127.0.0.1:1",
		"data_dir: " + dataDir,
		"mcp:",
		"  api_key: oneshot-doctor-test-key",
		"",
	}, "\n")), 0o600))
	t.Setenv("SEAMLESS_CONFIG", path)
	t.Setenv("SEAMLESS_DATA_DIR", dataDir)
	t.Setenv("SEAMLESS_ADDR", "127.0.0.1:1")
	t.Setenv("SEAMLESS_OPENAI_API_KEY", "")
	t.Setenv("HOME", dir)
	t.Setenv("CODEX_HOME", filepath.Join(dir, ".codex"))
}

// schemaSnapshot is a database's whole schema and migration record, without
// migrating it: every object's definition and every applied version.
func schemaSnapshot(t *testing.T, dbPath string) string {
	t.Helper()
	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	var b strings.Builder
	dump := func(q string) {
		rows, err := db.Query(q)
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()
		for rows.Next() {
			var line string
			require.NoError(t, rows.Scan(&line))
			b.WriteString(line + "\n")
		}
		require.NoError(t, rows.Err())
	}
	dump(`SELECT type || ' ' || name || ' ' || COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	dump(`SELECT version || ' ' || applied_at FROM schema_migrations ORDER BY version`)
	return b.String()
}

// doctorReport runs doctor and returns its report and verdict.
func doctorReport(t *testing.T) (string, error) {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() error {
		runErr = doctor(nil)
		return nil
	})
	return out, runErr
}

// refusing runs fn on a stdout that is not the test's; its error is the point.
func refusing(t *testing.T, fn func() error) error {
	t.Helper()
	var runErr error
	captureStdout(t, func() error {
		runErr = fn()
		return nil
	})
	return runErr
}

// Every state doctor can find, and the database left exactly as it was found.
func TestDoctor_ReportsTheDatabaseItFindsWithoutChangingIt(t *testing.T) {
	compiled := store.LatestSchemaVersion()
	behind := compiled - 1
	ahead := compiled + 2
	behindWhy := fmt.Sprintf("the database is at schema v%d, behind this binary's v%d", behind, compiled)
	tests := []struct {
		name    string
		applied int
		want    []string
		absent  []string
	}{
		{
			name: "behind this binary", applied: behind,
			want: []string{
				fmt.Sprintf("schema v%d,", behind),
				fmt.Sprintf("[warn] schema version: v%d applied / v%d compiled -- %s", behind, compiled, schemaBehindFinding),
				"[info] repo map: skipped: " + behindWhy,
				"[info] remote sessions: skipped: " + behindWhy,
				"[info] feature skills: skipped: " + behindWhy,
				"[ok  ] mcp_tools:",
				"updates:",
			},
			absent: []string{"no database yet"},
		},
		{
			name: "current", applied: compiled,
			want: []string{
				fmt.Sprintf("[info] schema version: v%d applied / v%d compiled", compiled, compiled),
				"[ok  ] repo map: no repos mapped yet",
				"remote sessions: none in 24h",
				"[ok  ] feature skills:",
			},
			absent: []string{"skipped:", "no database yet"},
		},
		{
			name: "ahead of this binary", applied: ahead,
			want: []string{
				fmt.Sprintf("[warn] schema version: v%d applied / v%d compiled -- %s; %s", ahead, compiled, schemaAheadCause, schemaAheadFix),
				"[ok  ] repo map: no repos mapped yet",
			},
			absent: []string{"skipped:", "no database yet"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dbPath := setSchemaVersion(t, tt.applied)
			writeDoctorConfig(t, filepath.Dir(dbPath))
			before := schemaSnapshot(t, dbPath)

			out, err := doctorReport(t)

			require.NoError(t, err, "only a failing check fails doctor; %s", out)
			for _, want := range tt.want {
				require.Contains(t, out, want)
			}
			for _, absent := range tt.absent {
				require.NotContains(t, out, absent)
			}
			require.Equal(t, before, schemaSnapshot(t, dbPath), "doctor changed the schema")
		})
	}
}

// A fresh install has no database until the daemon creates one: doctor says so
// and creates neither it nor the data dir.
func TestDoctor_AbsentDatabaseIsAFreshInstall(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	writeDoctorConfig(t, dataDir)

	out, err := doctorReport(t)

	require.NoError(t, err, "a fresh install is not a failure: %s", out)
	require.Contains(t, out, "database: "+filepath.Join(dataDir, "seam.db")+" does not exist yet")
	require.Contains(t, out, "the daemon creates and migrates it at its first start")
	require.Contains(t, out, fmt.Sprintf("schema version: no database yet / v%d compiled", store.LatestSchemaVersion()))
	require.Contains(t, out, "[info] repo map: skipped: no database yet")
	require.Contains(t, out, "[info] remote sessions: skipped: no database yet")
	require.Contains(t, out, "[info] feature skills: skipped: no database yet")
	require.Contains(t, out, "[ok  ] mcp_tools:", "tool registration needs no database")
	require.NoDirExists(t, dataDir, "doctor must not create the data dir")
	require.NoFileExists(t, filepath.Join(dataDir, "seam.db"))
}

// An empty file or someone else's SQLite file is no Seamless schema to read: a
// warning (serve can migrate it) that does not fail doctor or mint a schema.
func TestDoctor_UnreadableSchemaWarnsAndSkips(t *testing.T) {
	tests := map[string]func(t *testing.T, path string){
		"zero bytes": func(t *testing.T, path string) {
			require.NoError(t, os.WriteFile(path, nil, 0o600))
		},
		"another program's database": func(t *testing.T, path string) {
			db, err := sql.Open("sqlite", "file:"+path)
			require.NoError(t, err)
			defer func() { require.NoError(t, db.Close()) }()
			_, err = db.Exec(`CREATE TABLE unrelated (x INTEGER)`)
			require.NoError(t, err)
		},
	}
	for name, seed := range tests {
		t.Run(name, func(t *testing.T) {
			dataDir := t.TempDir()
			dbPath := filepath.Join(dataDir, "seam.db")
			seed(t, dbPath)
			writeDoctorConfig(t, dataDir)

			out, err := doctorReport(t)

			require.NoError(t, err, out)
			require.Contains(t, out, "its schema could not be read")
			require.Contains(t, out, "[info] repo map: skipped: its schema could not be read")

			db, err := sql.Open("sqlite", "file:"+dbPath)
			require.NoError(t, err)
			defer func() { require.NoError(t, db.Close()) }()
			var migrated int
			require.NoError(t, db.QueryRow(
				`SELECT COUNT(*) FROM sqlite_master WHERE name = 'schema_migrations'`).Scan(&migrated))
			require.Zero(t, migrated, "doctor must not start migrating it")
		})
	}
}

// oneShotWriters are the one-shot commands that open the daemon's database.
var oneShotWriters = []struct {
	name string
	run  func() error
}{
	{"family list", func() error { return runFamily([]string{"list"}) }},
	{"family add", func() error { return runFamily([]string{"add", "acme", "app"}) }},
	{"family remove", func() error { return runFamily([]string{"remove", "acme"}) }},
	{"map-repo", func() error { return runMapRepo([]string{"--path", "/some/repo", "--project", "app"}) }},
	{"unmap-repo --path", func() error { return runUnmapRepo([]string{"--path", "/some/repo"}) }},
	{"unmap-repo --stale", func() error { return runUnmapRepo([]string{"--stale"}) }},
	{"retire-project", func() error { return runRetireProject([]string{"--project", "app"}) }},
}

// A database behind this binary is refused, in the same words as the approval
// commands, and left byte-for-byte as it was found.
func TestOneShotCommands_NeverMigrateADatabaseBehindThisBinary(t *testing.T) {
	behind := store.LatestSchemaVersion() - 1
	for _, tt := range oneShotWriters {
		t.Run(tt.name, func(t *testing.T) {
			dbPath := daemonDB(t, behind)
			before := schemaSnapshot(t, dbPath)

			err := refusing(t, tt.run)

			require.ErrorContains(t, err, fmt.Sprintf("schema v%d", behind))
			require.ErrorContains(t, err, "never migrates")
			require.Equal(t, before, schemaSnapshot(t, dbPath))
			require.Equal(t, behind, appliedSchema(t, dbPath))
		})
	}
}

// The commands with no use before a daemon exists say to start one, and create
// nothing.
func TestOneShotCommands_RefuseAnAbsentDatabaseWithoutCreatingIt(t *testing.T) {
	for _, tt := range oneShotWriters {
		if tt.name == "map-repo" {
			continue // the one exception, pinned below
		}
		t.Run(tt.name, func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "data")
			writeDaemonConfig(t, dataDir)

			err := refusing(t, tt.run)

			require.ErrorContains(t, err, "no database at")
			require.ErrorContains(t, err, "start the daemon")
			require.NoDirExists(t, dataDir)
		})
	}
}

// map-repo works before any daemon ever ran (the installers can leave none), so
// an absent database is created, at this binary's schema, with the row serve's
// AdoptLocalHost later re-homes.
func TestMapRepo_CreatesAnAbsentDatabaseOnAServerInstall(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	writeDaemonConfig(t, dataDir)
	repo := t.TempDir()

	out := captureStdout(t, func() error { return runMapRepo([]string{"--path", repo, "--project", "app"}) })

	require.Contains(t, out, "mapped ")
	dbPath := filepath.Join(dataDir, "seam.db")
	require.Equal(t, store.LatestSchemaVersion(), appliedSchema(t, dbPath))
	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	rows, err := store.RepoMapRows(t.Context(), db)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "app", rows[0].Slug)
}

// A client install never gets a database of its own: nothing would ever read it.
func TestMapRepo_RefusesToCreateADatabaseOnAClientInstall(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "client-data")
	writeClientConfig(t, dir, "http://127.0.0.1:1", dataDir)

	err := refusing(t, func() error { return runMapRepo([]string{"--path", dir, "--project", "app"}) })

	require.ErrorContains(t, err, "no database at")
	require.NoDirExists(t, dataDir)
}

// The ordinary case still works, and so does a database ahead of this binary,
// which needs no migration and which serve keeps running on too.
func TestOneShotCommands_RunOnACurrentOrNewerDatabase(t *testing.T) {
	compiled := store.LatestSchemaVersion()
	for name, applied := range map[string]int{"current": compiled, "ahead": compiled + 2} {
		t.Run(name, func(t *testing.T) {
			dbPath := daemonDB(t, applied)
			repo := t.TempDir()
			before := schemaSnapshot(t, dbPath)

			for _, step := range []func() error{
				func() error { return runMapRepo([]string{"--path", repo, "--project", "app"}) },
				func() error { return runFamily([]string{"add", "acme", "app"}) },
				func() error { return runFamily([]string{"list"}) },
				func() error { return runUnmapRepo([]string{"--path", repo}) },
				func() error { return runRetireProject([]string{"--project", "app"}) },
			} {
				captureStdout(t, step)
			}

			require.Equal(t, before, schemaSnapshot(t, dbPath))
		})
	}
}

// A typo or --help is settled before anything is opened, as under approvers.
func TestRunFamily_BadSubcommandTouchesNoDatabase(t *testing.T) {
	for _, sub := range []string{"--help", "bogus"} {
		t.Run(sub+"/older database", func(t *testing.T) {
			behind := store.LatestSchemaVersion() - 1
			dbPath := daemonDB(t, behind)
			require.ErrorContains(t, runFamily([]string{sub}), "unknown subcommand")
			require.Equal(t, behind, appliedSchema(t, dbPath))
		})
		t.Run(sub+"/no database", func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "data")
			writeDaemonConfig(t, dataDir)
			require.ErrorContains(t, runFamily([]string{sub}), "unknown subcommand")
			require.NoDirExists(t, dataDir)
		})
	}
}
