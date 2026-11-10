package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// writeDaemonConfig writes a daemon's config naming dataDir and locking one
// project, and points $SEAMLESS_CONFIG and $HOME at a throwaway tree, so nothing
// these commands open can be a developer's real database.
func writeDaemonConfig(t *testing.T, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "seamless.yaml")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join([]string{
		"role: server",
		"data_dir: " + dataDir,
		"mcp:",
		"  api_key: approvers-open-test-key",
		"peer_auth:",
		"  enabled: true",
		"  owner: no-such-account-seamless-test",
		"  locked: [example-finance]",
		"  socket:",
		"    path: " + filepath.Join(dir, "seamlessd.sock"),
		"",
	}, "\n")), 0o600))
	t.Setenv("SEAMLESS_CONFIG", path)
	t.Setenv("SEAMLESS_DATA_DIR", dataDir)
	t.Setenv("HOME", dir)
}

// daemonDB is a database at the applied schema, in a data dir the approval
// commands accept as the daemon's own (0700), with the config naming it.
func daemonDB(t *testing.T, applied int) string {
	t.Helper()
	dbPath := setSchemaVersion(t, applied)
	require.NoError(t, os.Chmod(filepath.Dir(dbPath), 0o700))
	writeDaemonConfig(t, filepath.Dir(dbPath))
	return dbPath
}

// appliedSchema reads a database's schema version without migrating it.
func appliedSchema(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := store.OpenExisting(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	v, err := store.SchemaVersion(db)
	require.NoError(t, err)
	return v
}

// The bug this pins: `approvers enroll-code --help`, run to read the flags, took
// "--help" for a kind only after config.Load and store.Open, so a newer binary
// migrated the live database under an older running daemon. Help and every bad
// argument are settled before anything is opened: the database stays at its
// schema, and a data dir with no database gets none.
func TestRunApprovers_HelpAndBadArgumentsNeverTouchTheDatabase(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string // empty: it printed the usage
	}{
		{name: "help after the verb", args: []string{"enroll-code", "--help"}},
		{name: "help after the kind", args: []string{"enroll-code", "passkey", "--help"}},
		{name: "short help on list", args: []string{"list", "-h"}},
		{name: "help on remove", args: []string{"remove", "--help"}},
		{name: "single-dash help on reset", args: []string{"reset", "-help"}},
		{name: "no kind", args: []string{"enroll-code"}, wantErr: "takes one kind"},
		{name: "unknown kind", args: []string{"enroll-code", "bogus"}, wantErr: "invalid kind"},
		{name: "no key", args: []string{"enroll-code", "passkey"}, wantErr: "needs --key"},
		{name: "malformed key", args: []string{"enroll-code", "passkey", "--key", "nope"}, wantErr: "invalid value"},
		{name: "unknown flag", args: []string{"enroll-code", "passkey", "--nope"}, wantErr: "not defined"},
		{name: "list with an argument", args: []string{"list", "extra"}, wantErr: "takes no arguments"},
		{name: "list with an unknown flag", args: []string{"list", "--nope"}, wantErr: "not defined"},
		{name: "remove with no id", args: []string{"remove"}, wantErr: "takes one credential id"},
		{name: "reset unconfirmed", args: []string{"reset"}, wantErr: "pass --yes"},
		{name: "unknown verb", args: []string{"bogus"}, wantErr: "unknown verb"},
	}
	check := func(t *testing.T, args []string, wantErr string) {
		t.Helper()
		err := runApprovers(args)
		if wantErr == "" {
			require.NoError(t, err)
			return
		}
		require.ErrorContains(t, err, wantErr)
	}
	for _, tt := range tests {
		t.Run(tt.name+"/older database", func(t *testing.T) {
			behind := store.LatestSchemaVersion() - 1
			dbPath := daemonDB(t, behind)
			check(t, tt.args, tt.wantErr)
			require.Equal(t, behind, appliedSchema(t, dbPath), "the schema must not move")
		})
		t.Run(tt.name+"/no database", func(t *testing.T) {
			dataDir := filepath.Join(t.TempDir(), "data")
			writeDaemonConfig(t, dataDir)
			check(t, tt.args, tt.wantErr)
			require.NoFileExists(t, filepath.Join(dataDir, "seam.db"))
		})
	}
}

// A well-formed verb against a database behind this binary is refused rather
// than migrating it: the schema is the daemon's to move, at its own startup.
func TestRunApprovers_RefusesADatabaseBehindThisBinary(t *testing.T) {
	fingerprint := store.ApproverKeyFingerprint([]byte("a public key"))
	for _, args := range [][]string{
		{"list"},
		{"enroll-code", "passkey", "--key", fingerprint},
		{"remove", "01M71EXAMPLECREDENTIAL0000"},
		{"reset", "--yes"},
	} {
		t.Run(args[0], func(t *testing.T) {
			behind := store.LatestSchemaVersion() - 1
			dbPath := daemonDB(t, behind)
			err := runApprovers(args)
			require.ErrorContains(t, err, fmt.Sprintf("schema v%d", behind))
			require.ErrorContains(t, err, "never migrates")
			require.Equal(t, behind, appliedSchema(t, dbPath))
		})
	}
}

// The daemon creates the database. A one-shot run against a data dir with none
// says so, and leaves it that way.
func TestRunApprovers_RefusesAMissingDatabaseWithoutCreatingIt(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	require.NoError(t, os.Mkdir(dataDir, 0o700))
	writeDaemonConfig(t, dataDir)
	require.ErrorContains(t, runApprovers([]string{"list"}), "no database at")
	require.NoFileExists(t, filepath.Join(dataDir, "seam.db"))
}

// The ordinary case still works, and so does a database ahead of this binary,
// which needs no migration and which serve keeps running on too.
func TestRunApprovers_RunsOnACurrentOrNewerDatabase(t *testing.T) {
	compiled := store.LatestSchemaVersion()
	for name, applied := range map[string]int{"current": compiled, "ahead": compiled + 2} {
		t.Run(name, func(t *testing.T) {
			dbPath := daemonDB(t, applied)
			out := captureStdout(t, func() error { return runApprovers([]string{"list"}) })
			require.Contains(t, out, "No approver credentials are enrolled")
			require.Equal(t, applied, appliedSchema(t, dbPath))
		})
	}
}

// A database ahead of this binary opens with serve's and doctor's own diagnosis
// on the warnings stream; a current one opens silently.
func TestOpenServedDB_WarnsOnlyOnADatabaseAheadOfThisBinary(t *testing.T) {
	compiled := store.LatestSchemaVersion()

	daemonDB(t, compiled)
	cfg, err := config.Load()
	require.NoError(t, err)
	var quiet bytes.Buffer
	db, err := openServedDB(cfg, &quiet)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.Empty(t, quiet.String())

	daemonDB(t, compiled+2)
	cfg, err = config.Load()
	require.NoError(t, err)
	var warned bytes.Buffer
	db, err = openServedDB(cfg, &warned)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.Contains(t, warned.String(), fmt.Sprintf("schema v%d, newer than this seamlessd's v%d", compiled+2, compiled))
	require.Contains(t, warned.String(), schemaAheadCause)
	require.Contains(t, warned.String(), schemaAheadFix)
}

// The other one-shot commands the approval path runs as the daemon's account
// hold the same line.
func TestApprovalCommands_NeverMigrateADatabaseBehindThisBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("peer_auth is a POSIX deployment: the account check refuses before the schema check")
	}
	behind := store.LatestSchemaVersion() - 1
	for name, run := range map[string]func(config.Config) error{
		"approve":               func(config.Config) error { return runApprove([]string{"R-ABCD", "--deny"}) },
		"console-open --unlock": func(cfg config.Config) error { return runConsoleUnlock(cfg, "example-finance", "", "") },
	} {
		t.Run(name, func(t *testing.T) {
			dbPath := daemonDB(t, behind)
			cfg, err := config.Load()
			require.NoError(t, err)
			err = run(cfg)
			require.ErrorContains(t, err, fmt.Sprintf("schema v%d", behind))
			require.ErrorContains(t, err, "never migrates")
			require.Equal(t, behind, appliedSchema(t, dbPath))
		})
	}
}
