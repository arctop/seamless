package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"math/rand/v2"
	"time"
)

//go:embed migrations/001_initial.sql
var migration001 string

//go:embed migrations/002_task_plans_claims.sql
var migration002 string

//go:embed migrations/003_gardener_consolidate.sql
var migration003 string

//go:embed migrations/004_project_topology.sql
var migration004 string

//go:embed migrations/005_abandon_plan.sql
var migration005 string

//go:embed migrations/006_session_expired.sql
var migration006 string

//go:embed migrations/007_open_claims_repair.sql
var migration007 string

//go:embed migrations/008_tasks_claimed_by_index.sql
var migration008 string

//go:embed migrations/009_model_attribution.sql
var migration009 string

//go:embed migrations/010_session_external_identity.sql
var migration010 string

//go:embed migrations/011_session_token_usage.sql
var migration011 string

//go:embed migrations/012_favorites.sql
var migration012 string

//go:embed migrations/013_retrieval_utility.sql
var migration013 string

//go:embed migrations/014_memory_wanted.sql
var migration014 string

//go:embed migrations/015_tool_error.sql
var migration015 string

//go:embed migrations/016_rekind.sql
var migration016 string

//go:embed migrations/017_ship_plan.sql
var migration017 string

//go:embed migrations/018_proposal_undo.sql
var migration018 string

//go:embed migrations/019_project_isolation.sql
var migration019 string

//go:embed migrations/020_isolation_relocate.sql
var migration020 string

//go:embed migrations/021_proposal_hidden.sql
var migration021 string

//go:embed migrations/022_features_grandfather.sql
var migration022 string

//go:embed migrations/023_merge_plans.sql
var migration023 string

//go:embed migrations/024_work_record_fts.sql
var migration024 string

//go:embed migrations/025_session_host_repo_map.sql
var migration025 string

//go:embed migrations/026_console_level_grandfather.sql
var migration026 string

//go:embed migrations/027_session_agent_process.sql
var migration027 string

//go:embed migrations/028_write_screening.sql
var migration028 string

//go:embed migrations/029_screening_corpus.sql
var migration029 string

//go:embed migrations/030_commit_records.sql
var migration030 string

//go:embed migrations/031_access_leases.sql
var migration031 string

//go:embed migrations/032_approvers.sql
var migration032 string

//go:embed migrations/033_console_unlocks.sql
var migration033 string

//go:embed migrations/034_access_grants.sql
var migration034 string

//go:embed migrations/035_restricted_accounts.sql
var migration035 string

//go:embed migrations/036_mcp_sessions.sql
var migration036 string

//go:embed migrations/037_console_locks.sql
var migration037 string

//go:embed migrations/038_approver_rp_id.sql
var migration038 string

//go:embed migrations/039_fenced_sessions.sql
var migration039 string

//go:embed migrations/040_move_briefing.sql
var migration040 string

//go:embed migrations/041_console_signins.sql
var migration041 string

//go:embed migrations/042_devices.sql
var migration042 string

//go:embed migrations/043_relay_leases.sql
var migration043 string

//go:embed migrations/044_terminal_signin_browser.sql
var migration044 string

// Migration is a single numbered schema migration.
type Migration struct {
	Version int
	SQL     string
}

// migrationList returns the ordered migrations. Append new numbered files here
// (with a matching go:embed) -- NEVER edit an already-applied migration.
func migrationList() []Migration {
	return []Migration{
		{Version: 1, SQL: migration001},
		{Version: 2, SQL: migration002},
		{Version: 3, SQL: migration003},
		{Version: 4, SQL: migration004},
		{Version: 5, SQL: migration005},
		{Version: 6, SQL: migration006},
		{Version: 7, SQL: migration007},
		{Version: 8, SQL: migration008},
		{Version: 9, SQL: migration009},
		{Version: 10, SQL: migration010},
		{Version: 11, SQL: migration011},
		{Version: 12, SQL: migration012},
		{Version: 13, SQL: migration013},
		{Version: 14, SQL: migration014},
		{Version: 15, SQL: migration015},
		{Version: 16, SQL: migration016},
		{Version: 17, SQL: migration017},
		{Version: 18, SQL: migration018},
		{Version: 19, SQL: migration019},
		{Version: 20, SQL: migration020},
		{Version: 21, SQL: migration021},
		{Version: 22, SQL: migration022},
		{Version: 23, SQL: migration023},
		{Version: 24, SQL: migration024},
		{Version: 25, SQL: migration025},
		{Version: 26, SQL: migration026},
		{Version: 27, SQL: migration027},
		{Version: 28, SQL: migration028},
		{Version: 29, SQL: migration029},
		{Version: 30, SQL: migration030},
		{Version: 31, SQL: migration031},
		{Version: 32, SQL: migration032},
		{Version: 33, SQL: migration033},
		{Version: 34, SQL: migration034},
		{Version: 35, SQL: migration035},
		{Version: 36, SQL: migration036},
		{Version: 37, SQL: migration037},
		{Version: 38, SQL: migration038},
		{Version: 39, SQL: migration039},
		{Version: 40, SQL: migration040},
		{Version: 41, SQL: migration041},
		{Version: 42, SQL: migration042},
		{Version: 43, SQL: migration043},
		{Version: 44, SQL: migration044},
	}
}

// LatestSchemaVersion returns the version of the newest COMPILED migration --
// what a fresh Open would leave in schema_migrations -- as opposed to
// SchemaVersion, which reports what a given database has actually applied.
//
// The pair is the newer-schema refusal: an archive whose schema_version exceeds
// this binary's LatestSchemaVersion was written by a newer seamlessd and cannot
// be understood by migrating forward, because the migrations that produced it
// are not in this build.
func LatestSchemaVersion() int {
	ms := migrationList()
	if len(ms) == 0 {
		return 0
	}
	return ms[len(ms)-1].Version
}

// migrate applies every migration whose version exceeds the current max, each
// inside its own transaction, recording the version in schema_migrations.
// Ported from Seam v1 (migrations/migrate.go); the rarely-used PreHook was
// dropped -- add it back only if a future migration needs pre-transaction DDL.
//
// The version read here is only a fast path that keeps the common case (nothing
// pending) lock-free: applyMigration re-reads it under the write lock, because
// this one is a snapshot that a concurrent process can invalidate.
func migrate(db *sql.DB, ms []Migration) error {
	ctx := context.Background()

	if err := retryBusy(ctx, func() error {
		_, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`)
		return err
	}); err != nil {
		return fmt.Errorf("store.migrate: create tracking table: %w", err)
	}

	var current int
	if err := retryBusy(ctx, func() error {
		const q = "SELECT COALESCE(MAX(version), 0) FROM schema_migrations"
		return db.QueryRow(q).Scan(&current)
	}); err != nil {
		return fmt.Errorf("store.migrate: read current version: %w", err)
	}

	for _, m := range ms {
		if m.Version <= current {
			continue
		}
		if err := retryBusy(ctx, func() error { return applyMigration(ctx, db, m) }); err != nil {
			return err
		}
	}
	return nil
}

// busyRetryWindow bounds how long an opener waits out a peer holding the
// database in a state SQLite's own busy handler does not cover. Generous
// because losing this race costs an install real behavior (install-hooks falls
// back to the file/env features config and silently skips the skills of every
// feature enabled only in the database), and free in the common case, which
// never retries at all.
const busyRetryWindow = 10 * time.Second

// retryBusy runs fn until it stops reporting BUSY, the window closes, or ctx
// ends, returning fn's own error in the first two cases.
//
// The DSN's busy_timeout already covers ordinary write contention, and this
// does not duplicate it. It exists for the one case that timeout cannot see: on
// a brand-new database several connections race to perform the
// journal_mode=WAL transition, which takes an exclusive lock, and SQLite hands
// everyone else SQLITE_BUSY *without consulting the busy handler*, so the
// timeout never applies. A concurrent first-ever open then failed outright --
// 2 openers in 40 at a concurrency of 12.
//
// That transition happens once in a database's lifetime, so this loop can only
// spin on a first-ever open; every later one takes the fast path on the first
// try. fn must be idempotent, which both callers are: CREATE TABLE IF NOT
// EXISTS, and a migration that re-reads its own version under the write lock.
func retryBusy(ctx context.Context, fn func() error) error {
	const (
		baseDelay = 2 * time.Millisecond
		maxDelay  = 100 * time.Millisecond
	)

	deadline := time.Now().Add(busyRetryWindow)
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil || !isBusy(err) || !time.Now().Before(deadline) {
			return err
		}

		d := baseDelay << (attempt - 1)
		if d <= 0 || d > maxDelay {
			d = maxDelay
		}
		// Equal jitter, as in internal/llm: keep half the backoff as a floor
		// and randomize the rest, so racing openers do not wake in lockstep
		// and collide again.
		d = d/2 + rand.N(d/2+1)

		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// applyMigration applies one migration under SQLite's write lock, skipping it
// when another process got there first.
//
// Two things make this safe to run from several processes at once, and both are
// load-bearing. BEGIN IMMEDIATE takes the write lock at the start of the
// transaction rather than at its first write, so competing migrators serialize
// here instead of interleaving (the busy_timeout on the DSN makes the loser wait
// rather than fail). Re-reading the applied version INSIDE that lock is what
// turns the wait into a skip: the snapshot migrate took before the lock can be
// stale by the time we hold it.
//
// Without the pair, `make install` -- which restarts the daemon and then runs
// install-hooks -- had both processes replay the same pending migration, and the
// loser died on "duplicate column name". It is a dedicated connection rather
// than db.Begin so the raw BEGIN/COMMIT cannot be interleaved with another
// caller's statements on a pooled one.
func applyMigration(ctx context.Context, db *sql.DB, m Migration) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store.migrate: connection for v%d: %w", m.Version, err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("store.migrate: begin v%d: %w", m.Version, err)
	}
	committed := false
	defer func() {
		if !committed {
			// best-effort unwind: every path here is already returning the
			// error that matters, and a ROLLBACK that fails means a connection
			// that is about to be closed anyway
			_, _ = conn.ExecContext(ctx, "ROLLBACK") //nolint:errcheck // see above
		}
	}()

	var applied int
	const q = "SELECT COALESCE(MAX(version), 0) FROM schema_migrations"
	if err := conn.QueryRowContext(ctx, q).Scan(&applied); err != nil {
		return fmt.Errorf("store.migrate: re-read version for v%d: %w", m.Version, err)
	}
	if m.Version <= applied {
		return nil // another process applied it while we waited for the lock
	}

	if _, err := conn.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("store.migrate: apply v%d: %w", m.Version, err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO schema_migrations (version) VALUES (?)", m.Version); err != nil {
		return fmt.Errorf("store.migrate: record v%d: %w", m.Version, err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("store.migrate: commit v%d: %w", m.Version, err)
	}
	committed = true
	return nil
}
