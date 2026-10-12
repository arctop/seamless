package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// openTestDB opens a fresh migrated database in a temp dir.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// insertMemory inserts a memories_index row plus its fts row. invalidAt "" means
// active.
func insertMemory(t *testing.T, db *sql.DB, id, kind, name, desc, project, body, updated, invalidAt string) {
	t.Helper()
	ctx := context.Background()
	var inv any
	if invalidAt != "" {
		inv = invalidAt
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO memories_index
		    (id, kind, name, description, project, file_path, tags, valid_from,
		     invalid_at, superseded_by, source_session, content_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '[]', ?, ?, NULL, '', 'h', ?, ?)`,
		id, kind, name, desc, project, "memory/"+dirOf(project)+"/"+name+".md",
		updated, inv, updated, updated)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO fts (item_id, kind, project, title, name, description, body)
		VALUES (?, 'memory', ?, '', ?, ?, ?)`, id, project, name, desc, body)
	require.NoError(t, err)
}

func dirOf(project string) string {
	if project == "" {
		return "_global"
	}
	return project
}

func TestActiveMemoriesScopeAndValidity(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	ts := func(min int) string { return core.FormatTime(base.Add(time.Duration(min) * time.Minute)) }

	insertMemory(t, db, "01A", "gotcha", "alpha", "alpha desc", "seam", "alpha body", ts(3), "")
	insertMemory(t, db, "01B", "runbook", "beta", "beta desc", "seam", "beta body", ts(2), "")
	insertMemory(t, db, "01C", "constraint", "gamma", "gamma desc", "", "gamma body", ts(1), "")
	insertMemory(t, db, "01D", "gotcha", "delta", "delta desc", "seam", "delta body", ts(4), ts(5)) // inactive

	// Project scope includes its own + global actives, newest first, excludes inactive.
	got, err := ActiveMemories(ctx, db, "seam")
	require.NoError(t, err)
	names := memNames(got)
	require.Equal(t, []string{"alpha", "beta", "gamma"}, names)

	// Global scope sees only global memories.
	g, err := ActiveMemories(ctx, db, "")
	require.NoError(t, err)
	require.Equal(t, []string{"gamma"}, memNames(g))
}

func TestMemoryByNameAndID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := core.FormatTime(time.Now().UTC())
	insertMemory(t, db, "01A", "gotcha", "alpha", "alpha desc", "seam", "body", now, "")

	m, found, err := MemoryByName(ctx, db, "seam", "alpha")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "01A", m.ID)
	require.Equal(t, core.KindGotcha, m.Kind)

	_, found, err = MemoryByName(ctx, db, "seam", "missing")
	require.NoError(t, err)
	require.False(t, found)

	byID, found, err := MemoryByID(ctx, db, "01A")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "alpha", byID.Name)

	byIDs, err := MemoriesByIDs(ctx, db, []string{"01A", "nope"})
	require.NoError(t, err)
	require.Len(t, byIDs, 1)
	require.Equal(t, "alpha", byIDs["01A"].Name)
}

func TestProjectsWithMemoryNamed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := core.FormatTime(time.Now().UTC())
	insertMemory(t, db, "01A", "gotcha", "shared", "d", "web", "body", now, "")
	insertMemory(t, db, "01B", "gotcha", "shared", "d", "", "body", now, "")
	insertMemory(t, db, "01C", "gotcha", "shared", "d", "api", "body", now, "")
	insertMemory(t, db, "01D", "gotcha", "shared", "d", "retired", "body", now, now) // inactive
	insertMemory(t, db, "01E", "gotcha", "other", "d", "ops", "body", now, "")

	got, err := ProjectsWithMemoryNamed(ctx, db, "shared")
	require.NoError(t, err)
	require.Equal(t, []string{"", "api", "web"}, got)

	none, err := ProjectsWithMemoryNamed(ctx, db, "missing")
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestProjectsWithNoteSlug(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := core.FormatTime(time.Now().UTC())
	for i, project := range []string{"web", "", "api"} {
		_, err := db.ExecContext(ctx, `
			INSERT INTO notes_index (id, title, slug, project, file_path, content_hash, created_at, updated_at)
			VALUES (?, 'Shared', 'shared', ?, ?, 'h', ?, ?)`,
			fmt.Sprintf("01N%d", i), project, "notes/"+dirOf(project)+"/shared.md", now, now)
		require.NoError(t, err)
	}

	got, err := ProjectsWithNoteSlug(ctx, db, "shared")
	require.NoError(t, err)
	require.Equal(t, []string{"", "api", "web"}, got)

	none, err := ProjectsWithNoteSlug(ctx, db, "missing")
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestFTSSearchHyphenSafe(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := core.FormatTime(time.Now().UTC())
	insertMemory(t, db, "01A", "gotcha", "chroma-boot-race",
		"the chroma container answers health checks before it can serve", "seam", "readiness gate", now, "")
	insertMemory(t, db, "01B", "decision", "ulid-over-uuid",
		"use ulid identifiers everywhere instead of uuid", "seam", "sortable ids", now, "")

	// A hyphenated query must not raise an FTS5 syntax error and should match.
	hits, err := FTSSearch(ctx, db, "chroma-boot-race", nil, nil, 5)
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	require.Equal(t, "01A", hits[0].ItemID)

	// Punctuation-only input yields no hits, not an error.
	none, err := FTSSearch(ctx, db, "!!! -", nil, nil, 5)
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestProjectsCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, CreateProject(ctx, db, core.Project{
		ID: "01P", Slug: "seam", Name: "Seam", Description: "kb", CreatedAt: now, UpdatedAt: now,
	}))

	// Duplicate slug is a typed error.
	err := CreateProject(ctx, db, core.Project{ID: "01Q", Slug: "seam", Name: "Dup", CreatedAt: now, UpdatedAt: now})
	require.ErrorIs(t, err, ErrSlugExists)

	p, found, err := ProjectBySlug(ctx, db, "seam")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "Seam", p.Name)

	all, err := ListProjects(ctx, db)
	require.NoError(t, err)
	require.Len(t, all, 1)

	_, found, err = ProjectBySlug(ctx, db, "ghost")
	require.NoError(t, err)
	require.False(t, found)
}

func TestSessionsCRUDAndFindings(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	s := core.Session{
		ID: "01S", Name: "cc/aabbccdd", ProjectSlug: "seam", Status: core.SessionActive,
		Source: "startup", Ambient: true, Metadata: map[string]any{"k": "v"},
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, CreateSession(ctx, db, s))

	got, found, err := SessionByID(ctx, db, "01S")
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, got.Ambient)
	require.Equal(t, "v", got.Metadata["k"])

	byName, found, err := SessionByName(ctx, db, "cc/aabbccdd")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "01S", byName.ID)

	// Complete it with findings; it should surface in RecentFindings.
	s.Status = core.SessionCompleted
	s.Findings = "discovered the boot race"
	s.UpdatedAt = now.Add(time.Minute)
	require.NoError(t, UpdateSession(ctx, db, s))

	rf, err := RecentFindings(ctx, db, "seam", 5)
	require.NoError(t, err)
	require.Len(t, rf, 1)
	require.Equal(t, "discovered the boot race", rf[0].Findings)

	// An active session with no findings is excluded.
	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: "01T", Name: "cc/eeff", ProjectSlug: "seam", Status: core.SessionActive,
		CreatedAt: now, UpdatedAt: now,
	}))
	rf, err = RecentFindings(ctx, db, "seam", 5)
	require.NoError(t, err)
	require.Len(t, rf, 1)

	// A completed session whose findings is the no-summary sentinel is excluded:
	// it carries no knowledge and must not spend an agent's briefing context.
	require.NoError(t, CreateSession(ctx, db, core.Session{
		ID: "01U", Name: "cc/9900", ProjectSlug: "seam", Status: core.SessionCompleted,
		Ambient: true, Findings: core.FindingNoSummary,
		CreatedAt: now, UpdatedAt: now.Add(2 * time.Minute), // newest, so it would sort first
	}))
	rf, err = RecentFindings(ctx, db, "seam", 5)
	require.NoError(t, err)
	require.Len(t, rf, 1)
	require.Equal(t, "discovered the boot race", rf[0].Findings, "sentinel finding filtered out")
}

func memNames(ms []core.Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Name
	}
	return out
}
