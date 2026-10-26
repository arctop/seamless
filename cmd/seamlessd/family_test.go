package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// familyTestDB registers one open project and one project per fenced state.
func familyTestDB(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	for slug, state := range map[string]core.Isolation{
		"app":    core.IsolationOpen,
		"secret": core.IsolationConfidential,
		"vault":  core.IsolationSealed,
	} {
		require.NoError(t, store.CreateProject(ctx, db, core.Project{
			ID: "01" + slug, Slug: slug, Name: slug, Isolation: state,
			CreatedAt: now, UpdatedAt: now,
		}))
	}
	return ctx, db
}

// A family is a briefing cross-over surface, so an isolated project may not join
// one -- isolation requires a standalone project. The refusal is the whole
// command: no member of the batch lands.
func TestFamilyAdd_RejectsIsolatedProjects(t *testing.T) {
	ctx, db := familyTestDB(t)

	for _, tt := range []struct {
		name  string
		args  []string
		wants string
	}{
		{"confidential member", []string{"acme", "app", "secret"}, "secret is confidential"},
		{"sealed member", []string{"acme", "vault"}, "vault is sealed"},
		{"both", []string{"acme", "secret", "vault"}, "secret is confidential, vault is sealed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := familyAdd(ctx, db, tt.args)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wants)
			require.Contains(t, err.Error(), "must stand alone")

			families, err := store.ProjectFamilies(ctx, db)
			require.NoError(t, err)
			require.Empty(t, families, "a refused add writes nothing at all")
		})
	}

	// Open projects (and slugs that are not registered yet) still go through.
	require.NoError(t, familyAdd(ctx, db, []string{"acme", "app", "future-repo"}))
	families, err := store.ProjectFamilies(ctx, db)
	require.NoError(t, err)
	require.Equal(t, []string{"app", "future-repo"}, families["acme"])
}
