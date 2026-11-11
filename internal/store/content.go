package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"hash"
	"slices"
	"strings"
)

// ProjectContent is what one project holds, counted from the index tables and
// the work record. An archive's manifest records it for the snapshot it carries,
// and the console's backup list compares it with what the live instance holds.
type ProjectContent struct {
	// Slug is the project, "" for the global scope.
	Slug string
	// Name is the registered display name; empty for the global scope and for
	// a slug that holds content but has no projects row.
	Name string
	// Memories are the active ones (invalid_at IS NULL); Retired the superseded
	// or archived ones, still on disk as tombstones.
	Memories int
	Retired  int
	Notes    int
	Tasks    int
	Sessions int
	Trials   int
}

// Total is everything the project holds.
func (p ProjectContent) Total() int {
	return p.Memories + p.Retired + p.Notes + p.Tasks + p.Sessions + p.Trials
}

// ProjectContentCounts counts every project's content, sorted by slug with the
// global scope ("") first. Every registered project is listed, an empty one
// with zeros (a restore brings it back), and so is any slug that holds content
// without a projects row. The global scope is listed only when it holds
// something.
//
// On a filtered view (OpenView) the counts are the view's: a project the view
// leaves out is not counted, which is what the console's comparisons want.
func ProjectContentCounts(ctx context.Context, db *sql.DB) ([]ProjectContent, error) {
	const op = "store.ProjectContentCounts"
	by := map[string]*ProjectContent{}
	at := func(slug string) *ProjectContent {
		p := by[slug]
		if p == nil {
			p = &ProjectContent{Slug: slug}
			by[slug] = p
		}
		return p
	}

	if err := eachRow(ctx, db, `SELECT slug, name FROM projects`, nil, func(rows *sql.Rows) error {
		var slug, name string
		if err := rows.Scan(&slug, &name); err != nil {
			return err
		}
		if p := at(slug); name != slug {
			p.Name = name
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("%s: projects: %w", op, err)
	}

	counts := []struct {
		query string
		add   func(p *ProjectContent, a, b int)
	}{
		{`SELECT project, SUM(CASE WHEN invalid_at IS NULL THEN 1 ELSE 0 END),
			SUM(CASE WHEN invalid_at IS NULL THEN 0 ELSE 1 END) FROM memories_index GROUP BY project`,
			func(p *ProjectContent, a, b int) { p.Memories, p.Retired = a, b }},
		{`SELECT project, COUNT(*), 0 FROM notes_index GROUP BY project`,
			func(p *ProjectContent, a, _ int) { p.Notes = a }},
		{`SELECT project_slug, COUNT(*), 0 FROM tasks GROUP BY project_slug`,
			func(p *ProjectContent, a, _ int) { p.Tasks = a }},
		{`SELECT project_slug, COUNT(*), 0 FROM sessions GROUP BY project_slug`,
			func(p *ProjectContent, a, _ int) { p.Sessions = a }},
		{`SELECT project_slug, COUNT(*), 0 FROM trials GROUP BY project_slug`,
			func(p *ProjectContent, a, _ int) { p.Trials = a }},
	}
	for _, c := range counts {
		if err := eachRow(ctx, db, c.query, nil, func(rows *sql.Rows) error {
			var slug string
			var a, b int
			if err := rows.Scan(&slug, &a, &b); err != nil {
				return err
			}
			c.add(at(slug), a, b)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
	}

	out := make([]ProjectContent, 0, len(by))
	for _, p := range by {
		if p.Slug == "" && p.Total() == 0 {
			continue
		}
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b ProjectContent) int { return strings.Compare(a.Slug, b.Slug) })
	return out, nil
}

// fingerprintPart is one table's contribution to ContentFingerprint.
type fingerprintPart struct {
	table string
	// cols are hashed for every row, in order; a column the table lacks (an
	// older database) is left out.
	cols  []string
	order string
	// summary hashes only the row count and the newest value of each column,
	// for tables whose every row would be too much to read on each check.
	summary bool
}

// fingerprintParts is what ContentFingerprint covers: the knowledge (by id and
// content hash), the project registry and its fences, the locks and approvers,
// the repo routes, and the size and newest change of the work record. The event
// log, the derived mirrors (fts, embeddings, retrieval stats) and the access
// state are left out on purpose: telemetry or a lease alone must not make a
// scheduled backup of otherwise unchanged data.
var fingerprintParts = []fingerprintPart{
	{table: "memories_index", cols: []string{"id", "content_hash", "invalid_at", "superseded_by"}, order: "id"},
	{table: "notes_index", cols: []string{"id", "content_hash"}, order: "id"},
	{table: "projects", cols: []string{"slug", "name", "isolation", "parent_slug", "retired_at", "updated_at"}, order: "slug"},
	{table: "console_locks", cols: []string{"project_slug", "credential", "locked_at"}, order: "project_slug"},
	{table: "repo_map", cols: []string{"host", "path", "slug", "origin"}, order: "host, path"},
	{table: "approver_credentials", cols: []string{"id", "sign_count", "last_used_at"}, order: "id"},
	{table: "sessions", cols: []string{"updated_at"}, summary: true},
	{table: "tasks", cols: []string{"updated_at"}, summary: true},
	{table: "task_deps", summary: true},
	{table: "trials", cols: []string{"created_at"}, summary: true},
	{table: "gardener_proposals", cols: []string{"created_at", "resolved_at"}, summary: true},
	{table: "commit_records", cols: []string{"recorded_at"}, summary: true},
}

// fingerprintVolatileSettings are the settings rows ContentFingerprint skips:
// caches recomputed from the data they summarize, and the drift review's
// timestamp, which moves on every review whether or not anything changed.
var fingerprintVolatileSettings = []string{
	SettingGamificationRecords, SettingProjectStages, SettingCaptureStreak, SettingCorpusReviewed,
}

// ContentFingerprint is a digest of what a backup of db would carry, so the
// backup scheduler can tell that nothing changed since the last backup and skip
// writing an identical one. Equal fingerprints mean equal knowledge (every
// memory and note by id and content hash), the same projects with the same
// fences, the same locks, approvers, repo routes and settings, and a work
// record of the same size and newest change. See fingerprintParts for what is
// left out and why.
//
// A table or column an older database lacks is skipped rather than refused, so
// the fingerprint of a snapshot an older release wrote still computes; it then
// differs from a newer database's, which costs one extra backup and nothing
// else.
func ContentFingerprint(ctx context.Context, db *sql.DB) (string, error) {
	const op = "store.ContentFingerprint"
	h := sha256.New()
	for _, part := range fingerprintParts {
		if err := fingerprintTable(ctx, db, h, part); err != nil {
			return "", fmt.Errorf("%s: %s: %w", op, part.table, err)
		}
	}
	args := make([]any, len(fingerprintVolatileSettings))
	for i, k := range fingerprintVolatileSettings {
		args[i] = k
	}
	q := `SELECT key, value FROM settings WHERE key NOT IN (?` + strings.Repeat(", ?", len(args)-1) + `) ORDER BY key`
	if err := fingerprintRows(ctx, db, h, "settings", q, 2, args...); err != nil {
		return "", fmt.Errorf("%s: settings: %w", op, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fingerprintTable writes one part into h: its rows, or its count and newest
// values, over the columns the table has.
func fingerprintTable(ctx context.Context, db *sql.DB, h hash.Hash, part fingerprintPart) error {
	have, err := tableColumns(ctx, db, part.table)
	if err != nil {
		return err
	}
	if have == nil {
		h.Write([]byte(part.table + "\x00absent\x1d"))
		return nil
	}
	var cols []string
	for _, c := range part.cols {
		if slices.Contains(have, c) {
			cols = append(cols, `"`+c+`"`)
		}
	}
	var q string
	switch {
	case part.summary:
		sel := []string{"COUNT(*)"}
		for _, c := range cols {
			sel = append(sel, "MAX("+c+")")
		}
		q = `SELECT ` + strings.Join(sel, ", ") + ` FROM "` + part.table + `"`
		return fingerprintRows(ctx, db, h, part.table, q, len(sel))
	case len(cols) == 0:
		q = `SELECT COUNT(*) FROM "` + part.table + `"`
		return fingerprintRows(ctx, db, h, part.table, q, 1)
	default:
		q = `SELECT ` + strings.Join(cols, ", ") + ` FROM "` + part.table + `" ORDER BY ` + part.order
		return fingerprintRows(ctx, db, h, part.table, q, len(cols))
	}
}

// fingerprintRows writes every row q returns into h, each value as text, with
// separators no value can produce on its own (NULL is its own marker). A
// hash.Hash never returns an error from Write.
func fingerprintRows(ctx context.Context, db *sql.DB, h hash.Hash, label, q string, n int, args ...any) error {
	h.Write([]byte(label + "\x1d"))
	vals := make([]sql.NullString, n)
	ptrs := make([]any, n)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	return eachRow(ctx, db, q, args, func(rows *sql.Rows) error {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		for _, v := range vals {
			if !v.Valid {
				h.Write([]byte("\x01\x00"))
				continue
			}
			h.Write([]byte(v.String + "\x00"))
		}
		h.Write([]byte("\x1e"))
		return nil
	})
}

// tableColumns lists table's columns, or nil when the table does not exist.
func tableColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	var cols []string
	err := eachRow(ctx, db, `SELECT name FROM pragma_table_info(?)`, []any{table}, func(rows *sql.Rows) error {
		var c string
		if err := rows.Scan(&c); err != nil {
			return err
		}
		cols = append(cols, c)
		return nil
	})
	return cols, err
}
