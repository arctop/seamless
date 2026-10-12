package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/arctop/seamless/internal/core"
)

// memoryCols is the SELECT list for memories_index, matching scanMemory. Body is
// not in the index (it lives in the file), so a scanned memory has Body == "".
const memoryCols = `id, kind, name, description, project, file_path, tags,
	valid_from, invalid_at, superseded_by, source_session, model, favorite,
	content_hash, created_at, updated_at`

// scanMemory scans one memories_index row (memoryCols order) into a core.Memory.
func scanMemory(rows *sql.Rows) (core.Memory, error) {
	var (
		m                                  core.Memory
		kind, tags, srcSession, hash       string
		created, updated                   string
		validFrom, invalidAt, supersededBy sql.NullString
	)
	if err := rows.Scan(
		&m.ID, &kind, &m.Name, &m.Description, &m.Project, &m.FilePath, &tags,
		&validFrom, &invalidAt, &supersededBy, &srcSession, &m.Model, &m.Favorite,
		&hash, &created, &updated,
	); err != nil {
		return core.Memory{}, err
	}
	m.Kind = core.MemoryKind(kind)
	m.Tags = decodeTags(tags)
	m.SourceSession = srcSession
	m.ContentHash = hash
	m.SupersededBy = supersededBy.String

	var err error
	if m.ValidFrom, err = nullTime(validFrom); err != nil {
		return core.Memory{}, fmt.Errorf("valid_from: %w", err)
	}
	if m.InvalidAt, err = nullTimePtr(invalidAt); err != nil {
		return core.Memory{}, fmt.Errorf("invalid_at: %w", err)
	}
	if m.Created, err = core.ParseTime(created); err != nil {
		return core.Memory{}, fmt.Errorf("created_at: %w", err)
	}
	if m.Updated, err = core.ParseTime(updated); err != nil {
		return core.Memory{}, fmt.Errorf("updated_at: %w", err)
	}
	return m, nil
}

// scanMemories drains rows into a slice via scanMemory.
func scanMemories(rows *sql.Rows) ([]core.Memory, error) {
	var out []core.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ActiveMemories returns the active (not superseded/archived) memories visible to
// a project: its own plus all global memories (project == ""). Rows carry index
// metadata only (no body); newest-updated first. Passing project == "" returns
// only global memories.
func ActiveMemories(ctx context.Context, db *sql.DB, project string) ([]core.Memory, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+memoryCols+`
		FROM memories_index
		WHERE invalid_at IS NULL AND (project = ? OR project = '')
		ORDER BY updated_at DESC, id DESC`, project)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveMemories: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveMemories: %w", err)
	}
	return mems, nil
}

// ActiveMemoriesForScope returns the active memories visible to a project widened
// by extra project slugs (e.g. a shared parent whose memories a split injects into
// each child). It is ActiveMemories plus the union of extra: rows where invalid_at
// IS NULL and project is the project, global (”), or any extra slug -- deduped,
// newest-updated first. Blank/duplicate extras are ignored; with no extras it is
// exactly ActiveMemories.
func ActiveMemoriesForScope(ctx context.Context, db *sql.DB, project string, extra []string) ([]core.Memory, error) {
	// Build the deduped set of scope slugs: the project, global, and each extra.
	seen := map[string]bool{project: true, "": true}
	scopes := []string{project, ""}
	for _, s := range extra {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		scopes = append(scopes, s)
	}
	args := make([]any, len(scopes))
	for i, s := range scopes {
		args[i] = s
	}
	rows, err := db.QueryContext(ctx, `SELECT `+memoryCols+`
		FROM memories_index
		WHERE invalid_at IS NULL AND project IN (`+placeholders(len(scopes))+`)
		ORDER BY updated_at DESC, id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveMemoriesForScope: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveMemoriesForScope: %w", err)
	}
	return mems, nil
}

// ActiveMemoriesForProjects returns the active memories belonging to exactly
// the given project slugs -- no own-project or global union, unlike
// ActiveMemoriesForScope -- newest-updated first. It backs the briefing's
// opt-in sibling-memories cross-over, where the caller already holds the own
// scope and wants only the family members' rows. Blank/duplicate slugs are
// ignored; no slugs yields nil.
func ActiveMemoriesForProjects(ctx context.Context, db *sql.DB, slugs []string) ([]core.Memory, error) {
	seen := map[string]bool{}
	args := make([]any, 0, len(slugs))
	for _, s := range slugs {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		args = append(args, s)
	}
	if len(args) == 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+memoryCols+`
		FROM memories_index
		WHERE invalid_at IS NULL AND project IN (`+placeholders(len(args))+`)
		ORDER BY updated_at DESC, id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveMemoriesForProjects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveMemoriesForProjects: %w", err)
	}
	return mems, nil
}

// AllMemoriesIncludingInvalid returns every memory index row -- active and
// invalid (superseded or archived) -- newest-updated first. It backs the console
// Memories browser, which renders supersession chains and archived items.
func AllMemoriesIncludingInvalid(ctx context.Context, db *sql.DB) ([]core.Memory, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+memoryCols+`
		FROM memories_index ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store.AllMemoriesIncludingInvalid: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, fmt.Errorf("store.AllMemoriesIncludingInvalid: %w", err)
	}
	return mems, nil
}

// MemoryByName returns the active memory with an exact (project, name), most
// recently updated first. found is false when none matches. It does not fall
// back to the global scope; a caller that wants that resolves it explicitly.
func MemoryByName(ctx context.Context, db *sql.DB, project, name string) (core.Memory, bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+memoryCols+`
		FROM memories_index
		WHERE project = ? AND name = ? AND invalid_at IS NULL
		ORDER BY updated_at DESC, id DESC LIMIT 1`, project, name)
	if err != nil {
		return core.Memory{}, false, fmt.Errorf("store.MemoryByName: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return core.Memory{}, false, fmt.Errorf("store.MemoryByName: %w", err)
	}
	if len(mems) == 0 {
		return core.Memory{}, false, nil
	}
	return mems[0], true, nil
}

// MemoryByNameIncludingInvalid returns the most recently updated memory with an
// exact (project, name), whether or not it is still valid. It backs memory_read's
// warning path: a superseded memory (which MemoryByName excludes) is still
// readable, prefixed with a warning pointing at its replacement.
func MemoryByNameIncludingInvalid(ctx context.Context, db *sql.DB, project, name string) (core.Memory, bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+memoryCols+`
		FROM memories_index
		WHERE project = ? AND name = ?
		ORDER BY updated_at DESC, id DESC LIMIT 1`, project, name)
	if err != nil {
		return core.Memory{}, false, fmt.Errorf("store.MemoryByNameIncludingInvalid: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return core.Memory{}, false, fmt.Errorf("store.MemoryByNameIncludingInvalid: %w", err)
	}
	if len(mems) == 0 {
		return core.Memory{}, false, nil
	}
	return mems[0], true, nil
}

// ProjectsWithMemoryNamed returns every scope holding an active memory with
// this exact name, sorted ("" is the global scope). It backs memory_read's
// wrong-project hint, so it answers only where the name lives; the caller
// decides which of those scopes it may name.
func ProjectsWithMemoryNamed(ctx context.Context, db *sql.DB, name string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT project FROM memories_index
		WHERE name = ? AND invalid_at IS NULL ORDER BY project`, name)
	if err != nil {
		return nil, fmt.Errorf("store.ProjectsWithMemoryNamed: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("store.ProjectsWithMemoryNamed: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ProjectsWithMemoryNamed: %w", err)
	}
	return out, nil
}

// MemoryByID returns the memory with the given id. found is false when absent.
func MemoryByID(ctx context.Context, db *sql.DB, id string) (core.Memory, bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+memoryCols+`
		FROM memories_index WHERE id = ? LIMIT 1`, id)
	if err != nil {
		return core.Memory{}, false, fmt.Errorf("store.MemoryByID: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return core.Memory{}, false, fmt.Errorf("store.MemoryByID: %w", err)
	}
	if len(mems) == 0 {
		return core.Memory{}, false, nil
	}
	return mems[0], true, nil
}

// MemoriesByIDs returns the memories for the given ids in no particular order.
// Missing ids are simply absent from the result; callers key by ID.
func MemoriesByIDs(ctx context.Context, db *sql.DB, ids []string) (map[string]core.Memory, error) {
	out := make(map[string]core.Memory, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.QueryContext(ctx, `SELECT `+memoryCols+`
		FROM memories_index WHERE id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store.MemoriesByIDs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, fmt.Errorf("store.MemoriesByIDs: %w", err)
	}
	for _, m := range mems {
		out[m.ID] = m
	}
	return out, nil
}

// MemoriesSuperseding returns the memories that the given memory replaced: the
// index rows whose superseded_by points at id. It is the inverse of a memory's
// own SupersededBy edge (which the store does not otherwise materialize),
// newest-updated first, and empty when the memory superseded nothing. It backs
// the console memory peek's reverse "supersedes" section.
func MemoriesSuperseding(ctx context.Context, db *sql.DB, id string) ([]core.Memory, error) {
	if id == "" {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+memoryCols+`
		FROM memories_index WHERE superseded_by = ?
		ORDER BY updated_at DESC, id DESC`, id)
	if err != nil {
		return nil, fmt.Errorf("store.MemoriesSuperseding: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, fmt.Errorf("store.MemoriesSuperseding: %w", err)
	}
	return mems, nil
}
