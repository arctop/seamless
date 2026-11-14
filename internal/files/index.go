package files

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Item kinds, as stored in the fts.kind and embeddings.kind columns. Aliases
// for the canonical vocabulary in core, kept so this file reads unchanged; the
// work-record kinds (task, trial, session) live in the same fts table but are
// mirrored from the store layer, which owns the rows that have no file.
const (
	kindMemory = core.ItemKindMemory
	kindNote   = core.ItemKindNote
)

// Indexer mirrors memory/note files into the SQLite index tables and the
// unified self-contained FTS5 table. The files on disk are the source of truth;
// these mirrors are rebuildable and kept in sync by the watcher + reconciler.
type Indexer struct {
	db *sql.DB
}

// NewIndexer returns an Indexer backed by db.
func NewIndexer(db *sql.DB) *Indexer { return &Indexer{db: db} }

// tagsJSON encodes tags as a JSON array string ("[]" for none).
func tagsJSON(tags []string) (string, error) {
	if len(tags) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return "", fmt.Errorf("files: marshal tags: %w", err)
	}
	return string(b), nil
}

// IndexMemory upserts a memory into memories_index and refreshes its FTS row.
func (ix *Indexer) IndexMemory(ctx context.Context, m core.Memory) error {
	if m.ID == "" {
		return fmt.Errorf("files.IndexMemory: memory has no id (file %s)", m.FilePath)
	}
	tags, err := tagsJSON(m.Tags)
	if err != nil {
		return err
	}
	var invalidAt any
	if m.InvalidAt != nil {
		invalidAt = core.FormatTime(*m.InvalidAt)
	}

	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("files.IndexMemory: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO memories_index
		    (id, kind, name, description, project, file_path, tags,
		     valid_from, invalid_at, superseded_by, source_session, model,
		     favorite, content_hash, created_at, updated_at, account)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		    kind=excluded.kind, name=excluded.name, description=excluded.description,
		    project=excluded.project, file_path=excluded.file_path, tags=excluded.tags,
		    valid_from=excluded.valid_from, invalid_at=excluded.invalid_at,
		    superseded_by=excluded.superseded_by, source_session=excluded.source_session,
		    model=excluded.model, favorite=excluded.favorite,
		    content_hash=excluded.content_hash, updated_at=excluded.updated_at,
		    account=excluded.account`,
		m.ID, string(m.Kind), m.Name, m.Description, m.Project, m.FilePath, tags,
		core.FormatTime(m.ValidFrom), invalidAt, nullStr(m.SupersededBy), m.SourceSession,
		m.Model, m.Favorite, m.ContentHash, core.FormatTime(m.Created), core.FormatTime(m.Updated),
		m.Account,
	)
	if err != nil {
		return fmt.Errorf("files.IndexMemory: upsert: %w", err)
	}

	if err := ftsUpsert(ctx, tx, m.ID, kindMemory, m.Project, "", m.Name, m.Description, m.Body); err != nil {
		return err
	}
	// The stamps the file carries, in the same transaction: item_provenance
	// mirrors may_contain exactly (store.ProvenanceMirror).
	if err := store.ProvenanceMirror(ctx, tx, kindMemory, m.ID, m.Project, m.MayContain, time.Now().UTC()); err != nil {
		return fmt.Errorf("files.IndexMemory: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("files.IndexMemory: commit: %w", err)
	}
	return nil
}

// IndexNote upserts a note into notes_index and refreshes its FTS row.
func (ix *Indexer) IndexNote(ctx context.Context, n core.Note) error {
	if n.ID == "" {
		return fmt.Errorf("files.IndexNote: note has no id (file %s)", n.FilePath)
	}
	tags, err := tagsJSON(n.Tags)
	if err != nil {
		return err
	}

	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("files.IndexNote: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO notes_index
		    (id, title, slug, description, project, file_path, tags,
		     source_url, model, favorite, content_hash, created_at, updated_at, account)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		    title=excluded.title, slug=excluded.slug, description=excluded.description,
		    project=excluded.project, file_path=excluded.file_path, tags=excluded.tags,
		    source_url=excluded.source_url, model=excluded.model, favorite=excluded.favorite,
		    content_hash=excluded.content_hash, updated_at=excluded.updated_at,
		    account=excluded.account`,
		n.ID, n.Title, n.Slug, n.Description, n.Project, n.FilePath, tags,
		nullStr(n.SourceURL), n.Model, n.Favorite, n.ContentHash, core.FormatTime(n.Created), core.FormatTime(n.Updated),
		n.Account,
	)
	if err != nil {
		return fmt.Errorf("files.IndexNote: upsert: %w", err)
	}

	if err := ftsUpsert(ctx, tx, n.ID, kindNote, n.Project, n.Title, "", n.Description, n.Body); err != nil {
		return err
	}
	if err := store.ProvenanceMirror(ctx, tx, kindNote, n.ID, n.Project, n.MayContain, time.Now().UTC()); err != nil {
		return fmt.Errorf("files.IndexNote: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("files.IndexNote: commit: %w", err)
	}
	return nil
}

// DeleteByFilePath removes the index (and FTS) row for a data-dir-relative path.
// It is a no-op if no row references that path. Used by the watcher/reconciler
// when a file disappears from disk.
func (ix *Indexer) DeleteByFilePath(ctx context.Context, relPath string) error {
	tree, _, ok := treeAndRel(relPath)
	if !ok {
		return fmt.Errorf("files.DeleteByFilePath: %q is not under a known tree", relPath)
	}
	table, kind := "memories_index", kindMemory
	if tree == notesTree {
		table, kind = "notes_index", kindNote
	}

	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("files.DeleteByFilePath: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM `+table+` WHERE file_path = ?`, relPath).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // already absent
	}
	if err != nil {
		return fmt.Errorf("files.DeleteByFilePath: lookup: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE id = ?`, id); err != nil {
		return fmt.Errorf("files.DeleteByFilePath: delete row: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM fts WHERE item_id = ?`, id); err != nil {
		return fmt.Errorf("files.DeleteByFilePath: delete fts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM embeddings WHERE item_id = ?`, id); err != nil {
		return fmt.Errorf("files.DeleteByFilePath: delete embedding: %w", err)
	}
	// The item left the corpus, so its stamps leave the index with it.
	if err := store.ProvenanceForget(ctx, tx, kind, id); err != nil {
		return fmt.Errorf("files.DeleteByFilePath: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("files.DeleteByFilePath: commit: %w", err)
	}
	return nil
}

// IDByFilePath returns the id of the index row holding a data-dir-relative
// path, and whether such a row exists. The write guard uses it to detect a path
// already owned by a different item (the file_path column is UNIQUE).
func (ix *Indexer) IDByFilePath(ctx context.Context, relPath string) (id string, found bool, err error) {
	tree, _, ok := treeAndRel(relPath)
	if !ok {
		return "", false, fmt.Errorf("files.IDByFilePath: %q is not under a known tree", relPath)
	}
	table := "memories_index"
	if tree == notesTree {
		table = "notes_index"
	}
	err = ix.db.QueryRowContext(ctx, `SELECT id FROM `+table+` WHERE file_path = ?`, relPath).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("files.IDByFilePath: %w", err)
	}
	return id, true, nil
}

// ClearContentHash blanks the recorded content hash for the row at a path. The
// empty hash never matches a real digest, so the next reconcile (or watcher
// event) treats the file as changed and re-indexes it -- the retry mechanism
// for a failed embed. A missing row is a no-op.
func (ix *Indexer) ClearContentHash(ctx context.Context, relPath string) error {
	tree, _, ok := treeAndRel(relPath)
	if !ok {
		return fmt.Errorf("files.ClearContentHash: %q is not under a known tree", relPath)
	}
	table := "memories_index"
	if tree == notesTree {
		table = "notes_index"
	}
	if _, err := ix.db.ExecContext(ctx, `UPDATE `+table+` SET content_hash = '' WHERE file_path = ?`, relPath); err != nil {
		return fmt.Errorf("files.ClearContentHash: %w", err)
	}
	return nil
}

// ContentHashByFilePath returns the indexed content hash for a path, and whether
// a row exists. The reconciler uses it to skip unchanged files.
func (ix *Indexer) ContentHashByFilePath(ctx context.Context, relPath string) (hash string, found bool, err error) {
	tree, _, ok := treeAndRel(relPath)
	if !ok {
		return "", false, fmt.Errorf("files.ContentHashByFilePath: %q is not under a known tree", relPath)
	}
	table := "memories_index"
	if tree == notesTree {
		table = "notes_index"
	}
	err = ix.db.QueryRowContext(ctx, `SELECT content_hash FROM `+table+` WHERE file_path = ?`, relPath).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("files.ContentHashByFilePath: %w", err)
	}
	return hash, true, nil
}

// AllFilePaths returns every data-dir-relative file_path currently in the memory
// and note indexes. The reconciler uses it to find rows whose file has vanished.
func (ix *Indexer) AllFilePaths(ctx context.Context) ([]string, error) {
	rows, err := ix.db.QueryContext(ctx,
		`SELECT file_path FROM memories_index UNION ALL SELECT file_path FROM notes_index`)
	if err != nil {
		return nil, fmt.Errorf("files.AllFilePaths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("files.AllFilePaths: scan: %w", err)
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("files.AllFilePaths: %w", err)
	}
	return paths, nil
}

// ftsUpsert deletes any existing FTS row for itemID and inserts a fresh one.
// The unified fts table is self-contained (no content triggers), so the files
// layer maintains it explicitly.
func ftsUpsert(ctx context.Context, tx *sql.Tx, itemID, kind, project, title, name, description, body string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM fts WHERE item_id = ?`, itemID); err != nil {
		return fmt.Errorf("files: fts delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO fts (item_id, kind, project, title, name, description, body)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		itemID, kind, project, title, name, description, body); err != nil {
		return fmt.Errorf("files: fts insert: %w", err)
	}
	return nil
}

// nullStr maps the empty string to a NULL argument, so nullable TEXT columns
// hold NULL rather than "".
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
