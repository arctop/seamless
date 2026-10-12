package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/arctop/seamless/internal/core"
)

// noteCols is the SELECT list for notes_index, matching scanNote. Body is not in
// the index (it lives in the file), so a scanned note has Body == "".
const noteCols = `id, title, slug, description, project, file_path, tags,
	source_url, model, favorite, content_hash, created_at, updated_at`

// scanNote scans one notes_index row (noteCols order) into a core.Note.
func scanNote(rows *sql.Rows) (core.Note, error) {
	var (
		n                core.Note
		tags, hash       string
		created, updated string
		sourceURL        sql.NullString
	)
	if err := rows.Scan(
		&n.ID, &n.Title, &n.Slug, &n.Description, &n.Project, &n.FilePath, &tags,
		&sourceURL, &n.Model, &n.Favorite, &hash, &created, &updated,
	); err != nil {
		return core.Note{}, err
	}
	n.Tags = decodeTags(tags)
	n.SourceURL = sourceURL.String
	n.ContentHash = hash

	var err error
	if n.Created, err = core.ParseTime(created); err != nil {
		return core.Note{}, fmt.Errorf("created_at: %w", err)
	}
	if n.Updated, err = core.ParseTime(updated); err != nil {
		return core.Note{}, fmt.Errorf("updated_at: %w", err)
	}
	return n, nil
}

// scanNotes drains rows into a slice via scanNote.
func scanNotes(rows *sql.Rows) ([]core.Note, error) {
	var out []core.Note
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListNotes returns every note, newest-updated first. Rows carry index metadata
// only (no body, which lives in the file). It backs the console Notes browser.
func ListNotes(ctx context.Context, db *sql.DB) ([]core.Note, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+noteCols+`
		FROM notes_index ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store.ListNotes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	notes, err := scanNotes(rows)
	if err != nil {
		return nil, fmt.Errorf("store.ListNotes: %w", err)
	}
	return notes, nil
}

// NoteByID returns the note with the given id. found is false when absent.
func NoteByID(ctx context.Context, db *sql.DB, id string) (core.Note, bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+noteCols+`
		FROM notes_index WHERE id = ? LIMIT 1`, id)
	if err != nil {
		return core.Note{}, false, fmt.Errorf("store.NoteByID: %w", err)
	}
	defer func() { _ = rows.Close() }()
	notes, err := scanNotes(rows)
	if err != nil {
		return core.Note{}, false, fmt.Errorf("store.NoteByID: %w", err)
	}
	if len(notes) == 0 {
		return core.Note{}, false, nil
	}
	return notes[0], true, nil
}

// NoteBySlug returns the note with an exact (project, slug). found is false when
// none matches.
func NoteBySlug(ctx context.Context, db *sql.DB, project, slug string) (core.Note, bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+noteCols+`
		FROM notes_index WHERE project = ? AND slug = ? LIMIT 1`, project, slug)
	if err != nil {
		return core.Note{}, false, fmt.Errorf("store.NoteBySlug: %w", err)
	}
	defer func() { _ = rows.Close() }()
	notes, err := scanNotes(rows)
	if err != nil {
		return core.Note{}, false, fmt.Errorf("store.NoteBySlug: %w", err)
	}
	if len(notes) == 0 {
		return core.Note{}, false, nil
	}
	return notes[0], true, nil
}

// ProjectsWithNoteSlug returns every scope holding a note with this exact slug,
// sorted ("" is the global scope). It backs notes_read's wrong-project hint, the
// twin of ProjectsWithMemoryNamed: it answers only where the slug lives, and the
// caller decides which of those scopes it may name.
func ProjectsWithNoteSlug(ctx context.Context, db *sql.DB, slug string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT project FROM notes_index
		WHERE slug = ? ORDER BY project`, slug)
	if err != nil {
		return nil, fmt.Errorf("store.ProjectsWithNoteSlug: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("store.ProjectsWithNoteSlug: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ProjectsWithNoteSlug: %w", err)
	}
	return out, nil
}

// NotesByTag returns the notes carrying an exact tag, newest-updated first,
// optionally scoped to a project ("" = every project). Tags are stored as a
// JSON array, so the match runs through json_each rather than LIKE.
func NotesByTag(ctx context.Context, db *sql.DB, project, tag string) ([]core.Note, error) {
	q := `SELECT ` + noteCols + ` FROM notes_index
		WHERE EXISTS (SELECT 1 FROM json_each(notes_index.tags) je WHERE je.value = ?)`
	args := []any{tag}
	if project != "" {
		q += ` AND project = ?`
		args = append(args, project)
	}
	q += ` ORDER BY updated_at DESC, id DESC`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store.NotesByTag: %w", err)
	}
	defer func() { _ = rows.Close() }()
	notes, err := scanNotes(rows)
	if err != nil {
		return nil, fmt.Errorf("store.NotesByTag: %w", err)
	}
	return notes, nil
}

// NotesByTagPrefix returns the notes carrying at least one tag that begins with
// prefix (e.g. "plan:" for every plan composition), newest-updated first,
// optionally scoped to a project ("" = every project). It backs surfaces that
// enumerate a tag family without knowing the concrete values up front. prefix
// is treated literally: its LIKE metacharacters are escaped.
func NotesByTagPrefix(ctx context.Context, db *sql.DB, project, prefix string) ([]core.Note, error) {
	q := `SELECT ` + noteCols + ` FROM notes_index
		WHERE EXISTS (SELECT 1 FROM json_each(notes_index.tags) je WHERE je.value LIKE ? ESCAPE '\')`
	args := []any{escapeLikePrefix(prefix) + "%"}
	if project != "" {
		q += ` AND project = ?`
		args = append(args, project)
	}
	q += ` ORDER BY updated_at DESC, id DESC`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store.NotesByTagPrefix: %w", err)
	}
	defer func() { _ = rows.Close() }()
	notes, err := scanNotes(rows)
	if err != nil {
		return nil, fmt.Errorf("store.NotesByTagPrefix: %w", err)
	}
	return notes, nil
}

// escapeLikePrefix escapes the LIKE metacharacters (\ % _) in a literal prefix
// so it matches as plain text under `ESCAPE '\'`.
func escapeLikePrefix(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// NotesByIDs returns the notes for the given ids keyed by ID; missing ids are
// simply absent.
func NotesByIDs(ctx context.Context, db *sql.DB, ids []string) (map[string]core.Note, error) {
	out := make(map[string]core.Note, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.QueryContext(ctx, `SELECT `+noteCols+`
		FROM notes_index WHERE id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store.NotesByIDs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	notes, err := scanNotes(rows)
	if err != nil {
		return nil, fmt.Errorf("store.NotesByIDs: %w", err)
	}
	for _, n := range notes {
		out[n.ID] = n
	}
	return out, nil
}
