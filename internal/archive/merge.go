package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
)

// mergeTables is the order the row pass copies tables in, and the whole
// contract for which tables a merge touches at all.
//
// It is foreign-key safe front to back. task_deps carries the only real foreign
// keys in the schema (both into tasks), and INSERT OR IGNORE does NOT swallow a
// foreign key violation the way it swallows a uniqueness one -- a dep inserted
// ahead of its task is an error that aborts the transaction, not a skip.
//
// What is deliberately absent, and why each one stays absent:
//
//   - settings: repo-to-project mappings, project families, briefing overrides
//     and the embedder choice. Every one of them describes THIS machine, and
//     importing another machine's would repoint projects at paths that do not
//     exist here.
//   - embeddings: a vector is only comparable with vectors from the same model.
//     Imported corpus files are embedded on write by whatever embedder this
//     instance has; without one the report says so rather than importing
//     numbers that cannot be compared.
//   - memories_index, notes_index, fts: rebuildable mirrors of the files, which
//     the files pass has already refreshed through the files layer.
//   - retrieval_stats: recomputed from the merged event log by
//     store.RebuildRetrievalStats.
//   - jobs: a work queue, not a record of anything.
//   - access_approvals, access_leases: unlocks of locked projects,
//     bound to processes of the exporting machine's boot. Merging one would
//     be an unlock nobody approved here.
//   - approver_credentials, approver_enroll_codes: who may approve those
//     unlocks. A credential enrolled on another machine was never shown to
//     this one's owner.
//   - console_unlocks: one browser's console unlocks, behind a cookie only
//     that machine's browser holds.
//   - access_grants: read grants of confidential projects, bound to this
//     machine's sessions and processes; merging one would be a grant nobody
//     approved here.
//   - mcp_sessions: what this machine's own connections declared, keyed by
//     connection ids that mean nothing anywhere else.
//   - schema_migrations: the destination's own migration history.
//
// A host-local table (one keyed by which machine a row came from) does not
// belong here either: its rows describe the exporting machine's filesystem, and
// merging them would hand this instance paths it cannot stat.
//
// commit_records rides with the sessions and tasks it hangs off, as task_deps
// does: it holds commit ids, no path, and a merged session or task without its
// recorded commits would read as work that recorded none.
var mergeTables = []string{
	"projects", "sessions", "tasks", "task_deps", "trials", "events", "gardener_proposals", "commit_records",
}

// uniqueNameColumns are the UNIQUE columns whose collisions are worth naming in
// the report. INSERT OR IGNORE skips such a row silently, and "one fewer
// session than the archive had" is not something an owner can act on without
// being told which name was already taken.
var uniqueNameColumns = []struct{ table, column string }{
	{"projects", "slug"},
	{"sessions", "name"},
}

// rowPass is what the row pass learned, for the steps that run after it.
type rowPass struct {
	taskIDs    []string
	trialIDs   []string
	sessionIDs []string

	// projectSlugs are the slugs present in the archive's projects table, which
	// the row pass either inserted or found already taken. The project backfill
	// consults it so a dry run predicts the same number of registrations the
	// real run performs.
	projectSlugs []string
}

// importMerge folds the archive into an instance that already has data.
//
// Unlike a fresh restore, nothing here is byte-exact: every memory and note is
// re-rendered through the files layer so it is indexed, embedded, and subject
// to the same path-occupancy rules as any other write. That is the cost of
// merging, and it is why fresh-vs-merge is decided by the destination rather
// than offered as a flag.
func importMerge(ctx context.Context, a *archiveReader, opts ImportOptions, logger *slog.Logger, rep *ImportReport) error {
	stage, err := os.MkdirTemp("", "seamless-import-")
	if err != nil {
		return fmt.Errorf("archive.Import: staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()

	ex, err := a.extractTo(ctx, stage, DBName)
	if err != nil {
		return err
	}

	if ex.dbPath != "" {
		// store.Open migrates the staged snapshot forward to this build's
		// schema. It is the whole reason the snapshot is staged rather than
		// read in place: after this the two databases have identical columns,
		// so the row pass can copy column-for-column without a translation
		// table per historical schema version.
		staged, err := store.Open(ex.dbPath)
		if err != nil {
			return fmt.Errorf("archive.Import: migrate staged snapshot: %w", err)
		}
		if err := staged.Close(); err != nil {
			return fmt.Errorf("archive.Import: close staged snapshot: %w", err)
		}
	}

	db, err := store.Open(filepath.Join(opts.DataDir, DBName))
	if err != nil {
		return fmt.Errorf("archive.Import: open destination database: %w", err)
	}
	defer func() { _ = db.Close() }()

	mgr, err := files.NewManager(opts.DataDir, db, logger)
	if err != nil {
		return fmt.Errorf("archive.Import: %w", err)
	}
	defer func() { _ = mgr.Close() }()
	if opts.Embedder != nil {
		mgr.SetEmbedder(opts.Embedder)
	}

	slugs, err := mergeCorpus(ctx, mgr, db, stage, opts.DryRun, rep)
	if err != nil {
		return err
	}

	var pass rowPass
	if ex.dbPath != "" {
		pass, err = mergeRows(ctx, db, ex.dbPath, opts.DryRun, rep)
		if err != nil {
			return err
		}
	}

	if err := ensureImportedProjects(ctx, db, slugs, pass.projectSlugs, opts.DryRun, rep); err != nil {
		return err
	}

	if !opts.DryRun {
		// The row pass INSERTs straight into the work-record tables, so nothing
		// on that path went through IndexTaskFTS and friends; without this the
		// imported work record is present and unsearchable.
		if err := store.ReindexWorkFTS(ctx, db, pass.taskIDs, pass.trialIDs, pass.sessionIDs); err != nil {
			return fmt.Errorf("archive.Import: %w", err)
		}
		// The imported events carry retrieval signal from the other instance,
		// and retrieval_stats is derived state, so it is recomputed rather than
		// merged.
		if err := store.RebuildRetrievalStats(ctx, db); err != nil {
			return fmt.Errorf("archive.Import: %w", err)
		}
	}

	rep.Warnings = append(rep.Warnings, embeddingWarnings(a.manifest, opts.Embedder, ModeMerge)...)
	return nil
}

// ---------------------------------------------------------------------------
// files pass
// ---------------------------------------------------------------------------

// mergeCorpus writes the staged markdown through the files layer and returns
// the distinct project slugs the written items referenced.
//
// Every item is first-writer-wins by ULID: an id already in the index is
// skipped outright, which is what makes re-running the same archive insert
// nothing. Frontmatter rides through the core.Memory/core.Note structs, so
// invalid_at, superseded_by, favorite and unknown keys survive the round trip
// without this package understanding any of them -- and in particular
// supersession is NOT replayed through internal/lifecycle, which would append a
// second tombstone to a body that already has one.
func mergeCorpus(ctx context.Context, mgr *files.Manager, db *sql.DB, stage string, dry bool, rep *ImportReport) ([]string, error) {
	slugs := map[string]bool{}
	// pending is the path -> id map of writes a DRY run has already promised.
	// Without it, two archive items wanting the same path would both be counted
	// as writable, and the dry run would predict one more write than the real
	// run performs.
	pending := map[string]string{}

	for _, tree := range []string{MemoryTree, NotesTree} {
		root := filepath.Join(stage, tree)
		if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("archive.Import: staged %s: %w", tree, err)
		}

		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() || filepath.Ext(p) != ".md" {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			rel, err := filepath.Rel(stage, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			content, err := os.ReadFile(p) //nolint:gosec // p comes from walking our own staging dir.
			if err != nil {
				return fmt.Errorf("archive.Import: read %s: %w", rel, err)
			}
			if tree == MemoryTree {
				return mergeMemory(ctx, mgr, db, string(content), rel, dry, slugs, pending, rep)
			}
			return mergeNote(ctx, mgr, db, string(content), rel, dry, slugs, pending, rep)
		})
		if err != nil {
			return nil, fmt.Errorf("archive.Import: walk staged %s: %w", tree, err)
		}
	}
	return slices.Sorted(maps.Keys(slugs)), nil
}

func mergeMemory(ctx context.Context, mgr *files.Manager, db *sql.DB, content, rel string,
	dry bool, slugs map[string]bool, pending map[string]string, rep *ImportReport) error {
	mem, err := files.ParseMemory(content, rel)
	if err != nil {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: parse: %v", rel, err))
		return nil
	}
	if mem.ID == "" {
		rep.Warnings = append(rep.Warnings, rel+": no id, skipped")
		return nil
	}
	exists, err := idExists(ctx, db, "memories_index", mem.ID)
	if err != nil {
		return err
	}
	if exists {
		rep.Skipped++
		return nil
	}

	target := files.MemoryRelPath(mem.Project, mem.Name)
	if dry {
		owner, occupied, err := plannedPathOwner(ctx, mgr, target, pending)
		if err != nil {
			return err
		}
		if occupied && owner != mem.ID {
			rep.PathCollisions = append(rep.PathCollisions,
				PathCollision{Path: target, IncomingID: mem.ID, ExistingID: owner})
			return nil
		}
		pending[target] = mem.ID
		rep.Memories++
		addSlug(slugs, mem.Project)
		return nil
	}

	if _, err := mgr.WriteMemory(ctx, mem); err != nil {
		if errors.Is(err, files.ErrPathOccupied) {
			owner, _, oerr := pathOwner(ctx, mgr, target)
			if oerr != nil {
				return oerr
			}
			rep.PathCollisions = append(rep.PathCollisions,
				PathCollision{Path: target, IncomingID: mem.ID, ExistingID: owner})
			return nil
		}
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: write memory %s: %v", rel, mem.ID, err))
		return nil
	}
	rep.Memories++
	addSlug(slugs, mem.Project)
	return nil
}

func mergeNote(ctx context.Context, mgr *files.Manager, db *sql.DB, content, rel string,
	dry bool, slugs map[string]bool, pending map[string]string, rep *ImportReport) error {
	note, err := files.ParseNote(content, rel)
	if err != nil {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: parse: %v", rel, err))
		return nil
	}
	if note.ID == "" {
		rep.Warnings = append(rep.Warnings, rel+": no id, skipped")
		return nil
	}
	exists, err := idExists(ctx, db, "notes_index", note.ID)
	if err != nil {
		return err
	}
	if exists {
		rep.Skipped++
		return nil
	}

	target := files.NoteRelPath(note.Project, note.Slug)
	if dry {
		owner, occupied, err := plannedPathOwner(ctx, mgr, target, pending)
		if err != nil {
			return err
		}
		if occupied && owner != note.ID {
			rep.PathCollisions = append(rep.PathCollisions,
				PathCollision{Path: target, IncomingID: note.ID, ExistingID: owner})
			return nil
		}
		pending[target] = note.ID
		rep.Notes++
		addSlug(slugs, note.Project)
		return nil
	}

	if _, err := mgr.WriteNote(ctx, note); err != nil {
		if errors.Is(err, files.ErrPathOccupied) {
			owner, _, oerr := pathOwner(ctx, mgr, target)
			if oerr != nil {
				return oerr
			}
			rep.PathCollisions = append(rep.PathCollisions,
				PathCollision{Path: target, IncomingID: note.ID, ExistingID: owner})
			return nil
		}
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: write note %s: %v", rel, note.ID, err))
		return nil
	}
	rep.Notes++
	addSlug(slugs, note.Project)
	return nil
}

// addSlug records a project slug. The empty slug is the global scope and is
// never a project row.
func addSlug(slugs map[string]bool, slug string) {
	if slug != "" {
		slugs[slug] = true
	}
}

// idExists reports whether an id is already present in a corpus index table.
// First-writer-wins is by ULID: an item whose id is already here came from the
// same lineage as the archive's copy, so it is left exactly as it is.
func idExists(ctx context.Context, db *sql.DB, table, id string) (bool, error) {
	// table is one of this file's own constants, never caller input.
	var one int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM `+table+` WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("archive.Import: lookup %s %s: %w", table, id, err)
	}
	return true, nil
}

// plannedPathOwner is pathOwner plus the writes a dry run has already promised.
func plannedPathOwner(ctx context.Context, mgr *files.Manager, relPath string, pending map[string]string) (string, bool, error) {
	if id, ok := pending[relPath]; ok {
		return id, true, nil
	}
	return pathOwner(ctx, mgr, relPath)
}

// pathOwner reports which item owns a corpus path today, mirroring the check
// files.Manager.WriteMemory makes: the index row first (file_path is UNIQUE, so
// the index is what a write actually collides with), then the file on disk,
// which can exist unindexed after an out-of-band edit. An occupied path whose
// owner cannot be established reports an empty id rather than guessing.
func pathOwner(ctx context.Context, mgr *files.Manager, relPath string) (string, bool, error) {
	id, found, err := mgr.Indexer().IDByFilePath(ctx, relPath)
	if err != nil {
		return "", false, fmt.Errorf("archive.Import: path owner %s: %w", relPath, err)
	}
	if found {
		return id, true, nil
	}

	abs := filepath.Join(mgr.Store().DataDir(), filepath.FromSlash(relPath))
	info, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("archive.Import: path owner %s: %w", relPath, err)
	}
	if !info.Mode().IsRegular() {
		return "", true, nil
	}
	content, err := os.ReadFile(abs) //nolint:gosec // relPath is a validated corpus path under the data dir.
	if err != nil {
		return "", true, nil
	}
	switch {
	case strings.HasPrefix(relPath, MemoryTree+"/"):
		mem, err := files.ParseMemory(string(content), relPath)
		if err != nil {
			return "", true, nil
		}
		return mem.ID, true, nil
	case strings.HasPrefix(relPath, NotesTree+"/"):
		note, err := files.ParseNote(string(content), relPath)
		if err != nil {
			return "", true, nil
		}
		return note.ID, true, nil
	}
	return "", true, nil
}

// ensureImportedProjects registers a projects-table row for every slug the
// imported corpus references that still has none, so project_list reflects what
// just arrived.
//
// It runs AFTER the row pass on purpose. The archive's own projects table is
// the authoritative source for a project's name, description, topology and
// isolation; registering the slug first would mint a local row with a fresh
// ULID, the archive's row would then lose its UNIQUE slug to it, and the import
// would report a collision with a project it had itself just invented.
//
// srcSlugs is the set of slugs the archive's projects table carries. In a dry
// run those rows have not been inserted, so they are treated as present anyway
// -- otherwise the dry run would predict registrations the real run never makes.
//
// Only canonical slugs are registered, mirroring importer.backfillProjects: a
// value that does not survive core.Slugify never became a project directory
// legitimately, and registering it would put the malformation in project_list.
func ensureImportedProjects(ctx context.Context, db *sql.DB, slugs, srcSlugs []string, dry bool, rep *ImportReport) error {
	fromArchive := make(map[string]bool, len(srcSlugs))
	for _, s := range srcSlugs {
		fromArchive[s] = true
	}

	for _, slug := range slugs {
		if core.Slugify(slug) != slug {
			rep.Warnings = append(rep.Warnings,
				fmt.Sprintf("skipped non-slug project %q (not registered)", slug))
			continue
		}
		if dry && fromArchive[slug] {
			continue
		}
		_, ok, err := store.ProjectBySlug(ctx, db, slug)
		if err != nil {
			return fmt.Errorf("archive.Import: lookup project %q: %w", slug, err)
		}
		if ok {
			continue
		}
		if !dry {
			if _, err := store.EnsureProject(ctx, db, slug, slug); err != nil {
				return fmt.Errorf("archive.Import: register project %q: %w", slug, err)
			}
		}
		rep.Projects++
	}
	return nil
}

// ---------------------------------------------------------------------------
// row pass
// ---------------------------------------------------------------------------

// mergeRows copies the work record from the staged snapshot into the
// destination, or (dry) counts what it would copy.
//
// The whole pass runs on ONE pinned connection. ATTACH is connection-scoped, so
// `src` is only visible to statements issued on the connection that attached
// it; the pool is capped at one connection anyway, which is also why nothing
// here may touch db directly while the connection is held.
func mergeRows(ctx context.Context, db *sql.DB, srcPath string, dry bool, rep *ImportReport) (_ rowPass, retErr error) {
	var pass rowPass

	conn, err := db.Conn(ctx)
	if err != nil {
		return pass, fmt.Errorf("archive.Import: pin connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// ATTACH cannot run inside a transaction, hence before BeginTx.
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS src`, srcPath); err != nil {
		return pass, fmt.Errorf("archive.Import: attach staged snapshot: %w", err)
	}
	defer func() {
		if _, err := conn.ExecContext(ctx, `DETACH DATABASE src`); err != nil && retErr == nil {
			retErr = fmt.Errorf("archive.Import: detach staged snapshot: %w", err)
		}
	}()

	if err := collectNameCollisions(ctx, conn, rep); err != nil {
		return pass, err
	}
	if pass.projectSlugs, err = srcColumnValues(ctx, conn, "projects", "slug"); err != nil {
		return pass, err
	}
	if rep.Rows == nil {
		rep.Rows = make(map[string]int, len(mergeTables))
	}

	if dry {
		for _, table := range mergeTables {
			keys, err := uniqueKeySets(ctx, conn, table)
			if err != nil {
				return pass, err
			}
			n, err := predictInserts(ctx, conn, table, keys)
			if err != nil {
				return pass, err
			}
			rep.Rows[table] = n
		}
		return pass, nil
	}

	// Columns are resolved before the transaction opens: PRAGMA introspection
	// inside the same transaction that is writing the tables is a needlessly
	// interesting thing to do, and the column list cannot change under us.
	cols := make(map[string][]string, len(mergeTables))
	for _, table := range mergeTables {
		c, err := mergeColumns(ctx, conn, table)
		if err != nil {
			return pass, err
		}
		cols[table] = c
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return pass, fmt.Errorf("archive.Import: begin merge: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, table := range mergeTables {
		res, err := tx.ExecContext(ctx, insertSelect(table, cols[table]))
		if err != nil {
			return pass, fmt.Errorf("archive.Import: merge %s: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return pass, fmt.Errorf("archive.Import: merge %s: rows affected: %w", table, err)
		}
		rep.Rows[table] = int(n)
	}
	if err := tx.Commit(); err != nil {
		return pass, fmt.Errorf("archive.Import: commit merge: %w", err)
	}

	// Every id the pass tried to insert, not only the ones it did: an id that
	// was already present is reindexed harmlessly (the fts row is a rebuildable
	// mirror), and telling the two apart would cost a second pass for nothing.
	for _, spec := range []struct {
		table string
		dst   *[]string
	}{
		{"tasks", &pass.taskIDs},
		{"trials", &pass.trialIDs},
		{"sessions", &pass.sessionIDs},
	} {
		ids, err := srcColumnValues(ctx, conn, spec.table, "id")
		if err != nil {
			return pass, err
		}
		*spec.dst = ids
	}
	return pass, nil
}

// collectNameCollisions reports src rows whose UNIQUE name is already held by a
// DIFFERENT row in the destination. These are exactly the rows INSERT OR IGNORE
// will drop, and the only ones it drops for a reason other than "the same id is
// already here".
func collectNameCollisions(ctx context.Context, conn *sql.Conn, rep *ImportReport) error {
	for _, spec := range uniqueNameColumns {
		found, err := nameCollisions(ctx, conn, spec.table, spec.column)
		if err != nil {
			return err
		}
		rep.NameCollisions = append(rep.NameCollisions, found...)
	}
	return nil
}

func nameCollisions(ctx context.Context, conn *sql.Conn, table, column string) ([]NameCollision, error) {
	q := fmt.Sprintf(
		`SELECT s.id, m.id, s.%[1]s FROM src.%[2]s s JOIN main.%[2]s m ON m.%[1]s = s.%[1]s
		  WHERE m.id <> s.id ORDER BY s.%[1]s`, quoteIdent(column), quoteIdent(table))
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("archive.Import: %s.%s collisions: %w", table, column, err)
	}
	defer func() { _ = rows.Close() }()

	var out []NameCollision
	for rows.Next() {
		c := NameCollision{Table: table, Column: column}
		if err := rows.Scan(&c.IncomingID, &c.ExistingID, &c.Value); err != nil {
			return nil, fmt.Errorf("archive.Import: %s.%s collisions: %w", table, column, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("archive.Import: %s.%s collisions: %w", table, column, err)
	}
	return out, nil
}

// insertSelect builds the copy statement for one table. The column list is
// written out on both sides rather than using `SELECT *`, so the copy is by
// NAME: a column that a migration appended in a different position still lands
// in the right place.
func insertSelect(table string, cols []string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = quoteIdent(c)
	}
	list := strings.Join(quoted, ", ")
	return fmt.Sprintf(`INSERT OR IGNORE INTO main.%[1]s (%[2]s) SELECT %[2]s FROM src.%[1]s`,
		quoteIdent(table), list)
}

// mergeColumns returns the destination's columns for a table, having checked
// the staged snapshot has every one of them. The two schemas are supposed to be
// identical by now (the snapshot was migrated forward on open), so a missing
// column is a broken assumption rather than a case to work around: copying the
// subset that happens to line up would import rows silently missing a field.
func mergeColumns(ctx context.Context, conn *sql.Conn, table string) ([]string, error) {
	dst, err := tableColumns(ctx, conn, "main", table)
	if err != nil {
		return nil, err
	}
	if len(dst) == 0 {
		return nil, fmt.Errorf("archive.Import: destination has no table %s", table)
	}
	src, err := tableColumns(ctx, conn, "src", table)
	if err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(src))
	for _, c := range src {
		have[c] = true
	}
	for _, c := range dst {
		if !have[c] {
			return nil, fmt.Errorf(
				"archive.Import: staged snapshot is missing %s.%s after migrating to schema v%d",
				table, c, store.LatestSchemaVersion())
		}
	}
	return dst, nil
}

// tableColumns lists a table's columns in declaration order. The pragma
// table-valued function is used rather than a bare PRAGMA so the table and
// schema are bound parameters and the result is an ordinary result set.
func tableColumns(ctx context.Context, conn *sql.Conn, schema, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx,
		`SELECT name FROM pragma_table_info(?, ?) ORDER BY cid`, table, schema)
	if err != nil {
		return nil, fmt.Errorf("archive.Import: columns of %s.%s: %w", schema, table, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("archive.Import: columns of %s.%s: %w", schema, table, err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("archive.Import: columns of %s.%s: %w", schema, table, err)
	}
	return out, nil
}

// uniqueKeySets returns every column set a row must not duplicate: each UNIQUE
// index (which includes the implicit index behind a PRIMARY KEY) plus, for
// completeness, the declared primary key, which has no index of its own when it
// is a rowid alias.
//
// The sets are derived from the destination schema rather than transcribed,
// because they exist to make the dry run's prediction equal INSERT OR IGNORE's
// behaviour -- and a UNIQUE constraint added by a later migration would
// otherwise silently split the two apart.
func uniqueKeySets(ctx context.Context, conn *sql.Conn, table string) ([][]string, error) {
	names, err := uniqueIndexNames(ctx, conn, table)
	if err != nil {
		return nil, err
	}

	var sets [][]string
	seen := map[string]bool{}
	add := func(cols []string) {
		if len(cols) == 0 {
			return
		}
		key := strings.Join(cols, "\x00")
		if seen[key] {
			return
		}
		seen[key] = true
		sets = append(sets, cols)
	}

	for _, name := range names {
		cols, ok, err := indexColumns(ctx, conn, name)
		if err != nil {
			return nil, err
		}
		if !ok {
			// An expression index has no column name to compare on; skipping it
			// can only make the prediction conservative (it never claims a row
			// will insert when it will not).
			continue
		}
		add(cols)
	}

	pk, err := primaryKeyColumns(ctx, conn, table)
	if err != nil {
		return nil, err
	}
	add(pk)
	return sets, nil
}

func uniqueIndexNames(ctx context.Context, conn *sql.Conn, table string) ([]string, error) {
	// "unique" is a keyword, hence the quoting; partial indexes are excluded
	// because a row can duplicate their columns without conflicting.
	rows, err := conn.QueryContext(ctx,
		`SELECT name FROM pragma_index_list(?, 'main') WHERE "unique" = 1 AND partial = 0`, table)
	if err != nil {
		return nil, fmt.Errorf("archive.Import: unique indexes of %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("archive.Import: unique indexes of %s: %w", table, err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("archive.Import: unique indexes of %s: %w", table, err)
	}
	return out, nil
}

// indexColumns returns an index's columns, reporting false when any of them is
// an expression rather than a plain column.
func indexColumns(ctx context.Context, conn *sql.Conn, index string) ([]string, bool, error) {
	rows, err := conn.QueryContext(ctx,
		`SELECT name FROM pragma_index_info(?, 'main') ORDER BY seqno`, index)
	if err != nil {
		return nil, false, fmt.Errorf("archive.Import: columns of index %s: %w", index, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	plain := true
	for rows.Next() {
		var name sql.NullString
		if err := rows.Scan(&name); err != nil {
			return nil, false, fmt.Errorf("archive.Import: columns of index %s: %w", index, err)
		}
		if !name.Valid {
			plain = false
			continue
		}
		out = append(out, name.String)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("archive.Import: columns of index %s: %w", index, err)
	}
	return out, plain, nil
}

func primaryKeyColumns(ctx context.Context, conn *sql.Conn, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx,
		`SELECT name FROM pragma_table_info(?, 'main') WHERE pk > 0 ORDER BY pk`, table)
	if err != nil {
		return nil, fmt.Errorf("archive.Import: primary key of %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("archive.Import: primary key of %s: %w", table, err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("archive.Import: primary key of %s: %w", table, err)
	}
	return out, nil
}

// predictInserts counts the src rows that would survive INSERT OR IGNORE: the
// ones colliding with no uniqueness constraint in the destination. It is the
// dry run's whole answer for a table, and it is written as NOT EXISTS per key
// set rather than as a count of ids so that a row dropped for a duplicate
// session name is predicted as dropped, exactly as the real pass would.
func predictInserts(ctx context.Context, conn *sql.Conn, table string, keys [][]string) (int, error) {
	q := fmt.Sprintf(`SELECT COUNT(*) FROM src.%s s`, quoteIdent(table))
	if len(keys) > 0 {
		conds := make([]string, 0, len(keys))
		for _, key := range keys {
			on := make([]string, 0, len(key))
			for _, c := range key {
				on = append(on, fmt.Sprintf("m.%[1]s = s.%[1]s", quoteIdent(c)))
			}
			conds = append(conds, fmt.Sprintf(`NOT EXISTS (SELECT 1 FROM main.%s m WHERE %s)`,
				quoteIdent(table), strings.Join(on, " AND ")))
		}
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	var n int
	if err := conn.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("archive.Import: predict %s: %w", table, err)
	}
	return n, nil
}

// srcColumnValues reads one column out of the staged snapshot.
func srcColumnValues(ctx context.Context, conn *sql.Conn, table, column string) ([]string, error) {
	rows, err := conn.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM src.%s`, quoteIdent(column), quoteIdent(table)))
	if err != nil {
		return nil, fmt.Errorf("archive.Import: read src.%s.%s: %w", table, column, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("archive.Import: read src.%s.%s: %w", table, column, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("archive.Import: read src.%s.%s: %w", table, column, err)
	}
	return out, nil
}

// quoteIdent quotes a SQL identifier. Every identifier this package interpolates
// comes from its own constants or from sqlite_master, never from a caller, but
// interpolation without quoting is the habit that stops being safe the first
// time that changes.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
