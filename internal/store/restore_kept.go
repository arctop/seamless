package store

// What a restore keeps of today. A restore puts a backup's knowledge and work
// record back, but not the state that says how this install runs and who may
// approve: those stay as they are now, so a restore can never bring back a
// passkey the owner removed, reset an override the owner chose, drop a lock,
// forget the grants, moves and flows a fenced-session conversation was given
// since the backup, or loosen a fence (owner decisions of 2026-11-10 and
// 2026-11-11). ReadKeptState reads that state from today's database before the
// restore moves anything; ApplyKeptState writes it into the restored database
// before it is put in place.

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// RestoreKeptSettings are the settings rows a restore keeps as they are today
// instead of taking the backup's: this install's identity, how it runs (the
// console's overrides), who may approve, and its security switches. Every other
// key is restored with the data (restoredSettings). A key added to the store
// must be added to one of the two lists, or
// TestSettingsKeys_EveryKeyIsClassifiedForRestore fails.
var RestoreKeptSettings = []string{
	SettingLocalHost,
	SettingInstallID,
	approverUserHandleKey,
	SettingBriefingConfig,
	SettingFeaturesConfig,
	SettingConsoleLevel,
	SettingEmbedderMode,
	SettingUpdateConfig,
	SettingBackupConfig,
	SettingUtilityActivation,
	SettingLockdown,
	// The repo map's JSON mirror follows the repo_map table, which a restore
	// keeps too (keptTables).
	SettingRepoProjectMap,
}

// restoredSettings are the settings rows a restore takes from the backup: the
// project families, the caches recomputed from the data, and the corpus watch's
// state, which describes the corpus being restored.
var restoredSettings = []string{
	SettingProjectFamilies,
	SettingGamificationRecords,
	SettingProjectStages,
	SettingCaptureStreak,
	SettingCorpusArmed,
	SettingCorpusBaseline,
	SettingCorpusReviewed,
}

// keptTables are the tables a restore replaces wholesale with today's rows: who
// may approve (an enrolled passkey removed since the backup stays removed, and
// a used enrollment code stays used), which computers are paired (a device
// revoked since the backup stays revoked, one paired since stays paired, and a
// used pairing code stays used; migration 042), and this install's repo
// routes, which say which project an agent working in a repository belongs to.
var keptTables = []string{"approver_credentials", "approver_enroll_codes", "devices", "device_pairing_codes", "repo_map"}

// keptAccessHistory are the tables of the access record that say what a
// conversation holds: the read and write grants it was given, the moves it
// made into fenced projects and the flows its approvals cleared (store.Holder
// rows, migrations 034 and 039). Revocation and expiry never delete one, so
// today's rows are the backup's plus everything recorded since. A restore takes
// the union: the restored database's rows stay, and today's are added, so a
// conversation that moved or was granted after the backup keeps that part of
// its label instead of being cleared again for open projects and the global
// scope. The access_approvals rows they name come too, today's version winning
// (a request the backup holds as pending may have been decided since), and
// every grant ends with the restore, as the backup's own already have
// (ResetRestoredAccess): what a grant implied stays in the label; its access
// does not.
var keptAccessHistory = []string{"access_grants", "access_moves", "access_flows"}

// tableRows is a table's rows as read, column by column.
type tableRows struct {
	cols []string
	rows [][]any
}

// KeptState is what a restore carries over from today's database.
type KeptState struct {
	settings map[string]string
	tables   map[string]tableRows
	locks    tableRows
	// history is today's keptAccessHistory, table by table, and approvals
	// the access_approvals rows it names.
	history   map[string]tableRows
	approvals tableRows
	// projects are today's projects: name, isolation and nothing else.
	projects map[string]keptProject
	// lockSlugs are today's console locks, sorted.
	lockSlugs []string
}

type keptProject struct {
	name      string
	isolation core.Isolation
}

// LockedProject is a project a restore keeps locked, and the fence it has
// today.
type LockedProject struct {
	Slug      string
	Isolation core.Isolation
}

// Locked returns the projects locked today, sorted: the console's locks, plus
// forceLocked (the config's, which the caller passes only while peer_auth is
// on). Each carries today's isolation; a project that does not exist (a force
// lock naming one never created) reads as open, and the restore fences it.
func (k KeptState) Locked(forceLocked []string) []LockedProject {
	slugs := slices.Clone(k.lockSlugs)
	slugs = append(slugs, forceLocked...)
	slices.Sort(slugs)
	slugs = slices.Compact(slugs)
	out := make([]LockedProject, 0, len(slugs))
	for _, s := range slugs {
		state := core.IsolationOpen
		if p, ok := k.projects[s]; ok {
			state = p.isolation
		}
		out = append(out, LockedProject{Slug: s, Isolation: state})
	}
	return out
}

// ReadKeptState reads what a restore keeps from db, today's database. Nothing
// is written.
func ReadKeptState(ctx context.Context, db *sql.DB) (KeptState, error) {
	const op = "store.ReadKeptState"
	k := KeptState{settings: map[string]string{}, tables: map[string]tableRows{}, history: map[string]tableRows{},
		projects: map[string]keptProject{}}
	for _, key := range RestoreKeptSettings {
		v, found, err := GetSetting(ctx, db, key)
		if err != nil {
			return KeptState{}, fmt.Errorf("%s: %w", op, err)
		}
		if found {
			k.settings[key] = v
		}
	}
	for _, table := range keptTables {
		t, err := readTableRows(ctx, db, table)
		if err != nil {
			return KeptState{}, fmt.Errorf("%s: %s: %w", op, table, err)
		}
		k.tables[table] = t
	}
	for _, table := range keptAccessHistory {
		t, err := readTableRows(ctx, db, table)
		if err != nil {
			return KeptState{}, fmt.Errorf("%s: %s: %w", op, table, err)
		}
		k.history[table] = t
	}
	approvals, err := readRows(ctx, db, `SELECT * FROM access_approvals WHERE id IN (
		SELECT approval_id FROM access_grants UNION SELECT approval_id FROM access_moves
		UNION SELECT approval_id FROM access_flows)`)
	if err != nil {
		return KeptState{}, fmt.Errorf("%s: access_approvals: %w", op, err)
	}
	k.approvals = approvals
	locks, err := readTableRows(ctx, db, "console_locks")
	if err != nil {
		return KeptState{}, fmt.Errorf("%s: console_locks: %w", op, err)
	}
	k.locks = locks
	if k.lockSlugs, err = ConsoleLockedSlugs(ctx, db); err != nil {
		return KeptState{}, fmt.Errorf("%s: %w", op, err)
	}
	projects, err := ListProjects(ctx, db)
	if err != nil {
		return KeptState{}, fmt.Errorf("%s: %w", op, err)
	}
	for _, p := range projects {
		state, err := IsolationOf(ctx, db, p.Slug)
		if err != nil {
			return KeptState{}, fmt.Errorf("%s: %w", op, err)
		}
		k.projects[p.Slug] = keptProject{name: p.Name, isolation: state}
	}
	return k, nil
}

// KeptReport is what ApplyKeptState changed beyond copying rows, for the
// restore's report.
type KeptReport struct {
	// Tightened are projects whose restored fence was looser than today's (or,
	// for a locked one, than a lock needs), as "slug: from -> to".
	Tightened []string
	// Detached are child projects the restored data parented to a project that
	// is fenced today, as "child (from parent)": a fenced project cannot have
	// children, so the child was made standalone.
	Detached []string
	// Registered are projects locked today that the backup did not have: they
	// come back empty, and locked.
	Registered []string
}

// ApplyKeptOptions configure ApplyKeptState.
type ApplyKeptOptions struct {
	// ForceLocked are the config's force locks, passed only while peer_auth is
	// on: then a lock holds them too, so they are fenced like the console's.
	ForceLocked []string
	Now         time.Time
}

// ApplyKeptState writes kept, read from today's database, into db, a restored
// one, before it is put in place:
//
//   - the RestoreKeptSettings rows and the keptTables become exactly today's:
//     the restored rows are deleted first, so nothing the owner removed since
//     the backup comes back;
//   - today's access history (keptAccessHistory) is added to the restored
//     one, so no grant, move or flow recorded since the backup is lost, and
//     then every live permission ends (ResetRestoredAccess), as the import
//     already ended the backup's;
//   - today's console locks are written over the restored ones verbatim (who
//     set each, and when), and a restored lock today lacks stays: a restore
//     never reduces the set of locked projects;
//   - every project in both keeps the stricter of its two fences, so a restore
//     never loosens one; a locked project is registered if the backup lacks it
//     and fenced at least confidential.
//
// db must be the restored database, never a live one: the fence pass runs as
// one transaction per project (TightenProjectIsolation opens its own, and the
// pool has one connection), so a failure part way leaves db half written, and
// the caller discards it.
func ApplyKeptState(ctx context.Context, db *sql.DB, kept KeptState, opts ApplyKeptOptions) (KeptReport, error) {
	const op = "store.ApplyKeptState"
	var rep KeptReport
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if err := applyKeptRows(ctx, db, kept); err != nil {
		return rep, fmt.Errorf("%s: %w", op, err)
	}
	// Today's grants came in with the history, some of them live: a restore
	// ends every live permission, and the agents ask again.
	if _, err := ResetRestoredAccess(ctx, db, opts.Now); err != nil {
		return rep, fmt.Errorf("%s: %w", op, err)
	}

	restoredLocks, err := ConsoleLockedSlugs(ctx, db)
	if err != nil {
		return rep, fmt.Errorf("%s: %w", op, err)
	}
	locked := map[string]bool{}
	for _, s := range restoredLocks {
		locked[s] = true
	}
	for _, p := range kept.Locked(opts.ForceLocked) {
		locked[p.Slug] = true
	}

	restored, err := ListProjects(ctx, db)
	if err != nil {
		return rep, fmt.Errorf("%s: %w", op, err)
	}
	have := map[string]bool{}
	for _, p := range restored {
		have[p.Slug] = true
	}
	// Locked projects the backup does not have come back registered and empty,
	// so the lock has a project to hold.
	for _, slug := range sortedKeys(locked) {
		if have[slug] {
			continue
		}
		name := slug
		if p, ok := kept.projects[slug]; ok && strings.TrimSpace(p.name) != "" {
			name = p.name
		}
		if _, err := EnsureProject(ctx, db, slug, name); err != nil {
			return rep, fmt.Errorf("%s: register %q: %w", op, slug, err)
		}
		have[slug] = true
		rep.Registered = append(rep.Registered, slug)
	}

	for _, slug := range sortedKeys(have) {
		current, err := IsolationOf(ctx, db, slug)
		if err != nil {
			return rep, fmt.Errorf("%s: %w", op, err)
		}
		target := current
		if p, ok := kept.projects[slug]; ok {
			target = stricterIsolation(target, p.isolation)
		}
		if locked[slug] {
			target = stricterIsolation(target, core.IsolationConfidential)
		}
		if target == current {
			continue
		}
		detached, err := detachChildren(ctx, db, slug, opts.Now)
		if err != nil {
			return rep, fmt.Errorf("%s: %w", op, err)
		}
		rep.Detached = append(rep.Detached, detached...)
		if _, err := TightenProjectIsolation(ctx, db, slug, target, opts.Now); err != nil {
			return rep, fmt.Errorf("%s: fence %q: %w", op, slug, err)
		}
		rep.Tightened = append(rep.Tightened, fmt.Sprintf("%s: %s -> %s", slug, current, target))
	}
	return rep, nil
}

// applyKeptRows writes the kept settings, tables and locks in one transaction.
func applyKeptRows(ctx context.Context, db *sql.DB, kept KeptState) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	for _, key := range RestoreKeptSettings {
		if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
			return fmt.Errorf("settings %s: %w", key, err)
		}
		if v, ok := kept.settings[key]; ok {
			if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)`, key, v); err != nil {
				return fmt.Errorf("settings %s: %w", key, err)
			}
		}
	}
	for _, table := range keptTables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM "`+table+`"`); err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		if err := insertTableRows(ctx, tx, table, kept.tables[table], "INSERT"); err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
	}
	// The access history: today's approvals they name first (the rows the
	// others' foreign keys point at), then the history itself, never replacing
	// a restored row.
	if err := upsertTableRows(ctx, tx, "access_approvals", kept.approvals); err != nil {
		return fmt.Errorf("access_approvals: %w", err)
	}
	for _, table := range keptAccessHistory {
		if err := insertTableRows(ctx, tx, table, kept.history[table], "INSERT OR IGNORE"); err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
	}
	if err := insertTableRows(ctx, tx, "console_locks", kept.locks, "INSERT OR REPLACE"); err != nil {
		return fmt.Errorf("console_locks: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// detachChildren makes every project parented to slug standalone, returning
// them as "child (from slug)", sorted.
func detachChildren(ctx context.Context, db *sql.DB, slug string, now time.Time) ([]string, error) {
	var children []string
	if err := eachRow(ctx, db, `SELECT slug FROM projects WHERE parent_slug = ? ORDER BY slug`, []any{slug}, func(rows *sql.Rows) error {
		var c string
		if err := rows.Scan(&c); err != nil {
			return err
		}
		children = append(children, c)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("children of %q: %w", slug, err)
	}
	out := make([]string, 0, len(children))
	for _, c := range children {
		if err := SetProjectParent(ctx, db, c, "", now); err != nil {
			return nil, fmt.Errorf("detach %q from %q: %w", c, slug, err)
		}
		out = append(out, c+" (from "+slug+")")
	}
	return out, nil
}

// stricterIsolation returns the stricter of two fences.
func stricterIsolation(a, b core.Isolation) core.Isolation {
	if isolationRank(b) > isolationRank(a) {
		return b
	}
	return a
}

// sortedKeys returns m's keys, sorted.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// readTableRows reads every row of table, keeping each value as the driver
// returned it.
func readTableRows(ctx context.Context, db *sql.DB, table string) (tableRows, error) {
	return readRows(ctx, db, `SELECT * FROM "`+table+`"`)
}

// readRows reads every row query returns, keeping each value as the driver
// returned it.
func readRows(ctx context.Context, db *sql.DB, query string) (tableRows, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return tableRows{}, err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return tableRows{}, err
	}
	t := tableRows{cols: cols}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		// Scanning into *any copies a []byte value, so the rows outlive the
		// cursor.
		if err := rows.Scan(ptrs...); err != nil {
			return tableRows{}, err
		}
		t.rows = append(t.rows, vals)
	}
	return t, rows.Err()
}

// insertTableRows inserts t's rows into table with verb ("INSERT" or "INSERT
// OR REPLACE"), over the columns both sides have: today's database and the
// restored one are normally at the same schema, and a column only one of them
// has is left to its default rather than refused.
func insertTableRows(ctx context.Context, tx *sql.Tx, table string, t tableRows, verb string) error {
	if len(t.rows) == 0 {
		return nil
	}
	dest, err := tableColumnsTx(ctx, tx, table)
	if err != nil {
		return err
	}
	var idx []int
	var names []string
	for i, c := range t.cols {
		if slices.Contains(dest, c) {
			idx = append(idx, i)
			names = append(names, `"`+c+`"`)
		}
	}
	if len(idx) == 0 {
		return fmt.Errorf("no column in common")
	}
	q := verb + ` INTO "` + table + `" (` + strings.Join(names, ", ") + `) VALUES (?` + strings.Repeat(", ?", len(idx)-1) + `)`
	for _, row := range t.rows {
		args := make([]any, len(idx))
		for j, i := range idx {
			args[j] = row[i]
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	return nil
}

// upsertTableRows writes t's rows into table, keyed on its id column: a row
// the table lacks is inserted, and one it has is updated in place to t's
// values, never deleted and reinserted, so the rows whose foreign keys name it
// stay valid. Columns are matched as insertTableRows matches them.
func upsertTableRows(ctx context.Context, tx *sql.Tx, table string, t tableRows) error {
	if len(t.rows) == 0 {
		return nil
	}
	dest, err := tableColumnsTx(ctx, tx, table)
	if err != nil {
		return err
	}
	var idx []int
	var names, sets []string
	for i, c := range t.cols {
		if slices.Contains(dest, c) {
			idx = append(idx, i)
			names = append(names, `"`+c+`"`)
			if c != "id" {
				sets = append(sets, `"`+c+`" = excluded."`+c+`"`)
			}
		}
	}
	if !slices.Contains(names, `"id"`) || len(sets) == 0 {
		return fmt.Errorf("no id, or no other column, in common")
	}
	q := `INSERT INTO "` + table + `" (` + strings.Join(names, ", ") + `) VALUES (?` + strings.Repeat(", ?", len(idx)-1) +
		`) ON CONFLICT (id) DO UPDATE SET ` + strings.Join(sets, ", ")
	for _, row := range t.rows {
		args := make([]any, len(idx))
		for j, i := range idx {
			args[j] = row[i]
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	return nil
}

// tableColumnsTx lists table's columns inside tx.
func tableColumnsTx(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

// ClearHashesWithoutVectors blanks the content hash of every indexed memory and
// note that has no embedding, so the next reconcile indexes it again and, when
// the daemon has an embedder, gives it a vector. A restore indexes its files
// without one, and the daemon's own startup reconcile -- which has one -- would
// otherwise skip them as unchanged; it is the retry files.Manager makes after a
// failed embed. It returns how many rows it blanked.
func ClearHashesWithoutVectors(ctx context.Context, db *sql.DB) (int64, error) {
	var total int64
	for _, table := range []string{"memories_index", "notes_index"} {
		res, err := db.ExecContext(ctx, `UPDATE `+table+` SET content_hash = ''
			WHERE content_hash <> '' AND id NOT IN (SELECT item_id FROM embeddings)`)
		if err != nil {
			return total, fmt.Errorf("store.ClearHashesWithoutVectors: %s: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("store.ClearHashesWithoutVectors: %s: %w", table, err)
		}
		total += n
	}
	return total, nil
}
