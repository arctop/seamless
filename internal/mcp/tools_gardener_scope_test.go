package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/gardener"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// TestGardenerRequestScope pins the token-to-scope mapping at the boundary.
//
// It is a same-package test because the interesting part is not reachable from
// the tool surface: gardener_request needs an LLM to get past scope resolution,
// so the differences that matter here -- "global" is not "all", and an omitted
// project is not "all" either -- are invisible to a black-box call against a
// chat-less fixture.
//
// The omitted case is the decided behavior change and the reason this test
// exists. It used to scan every project on the machine with no ambiguity check;
// it now resolves like every other read, which is the "no automatic fallbacks for
// ambiguous requests" directive finally reaching the last tool that ignored it.
func TestGardenerRequestScope(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	s := New(Config{DB: db, APIKey: "test-key"})

	_, err = store.EnsureProject(ctx, db, "alpha", "Alpha")
	require.NoError(t, err)

	for _, tc := range []struct {
		name     string
		explicit string
		want     gardener.RequestScope
	}{
		{
			name:     "the widening token is the only way to reach every project",
			explicit: "all",
			want:     gardener.RequestScope{AllProjects: true},
		},
		{
			name:     "global is globals only, NOT every project",
			explicit: "global",
			want:     gardener.RequestScope{Project: ""},
		},
		{
			name:     "the on-disk global synonym normalizes the same way",
			explicit: "_global",
			want:     gardener.RequestScope{Project: ""},
		},
		{
			name:     "a registered slug scopes to that project",
			explicit: "alpha",
			want:     gardener.RequestScope{Project: "alpha"},
		},
		{
			// With nothing to infer from, a read legitimately targets global --
			// resolveReadScope's ("", nil) case. The point is what it is NOT:
			// AllProjects stays false, where an omitted project used to mean the
			// whole machine.
			name:     "omitted resolves like any other read, and never widens",
			explicit: "",
			want:     gardener.RequestScope{Project: ""},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.gardenerRequestScope(ctx, tc.explicit)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}

	t.Run("a well-formed slug that is not a project", func(t *testing.T) {
		_, err := s.gardenerRequestScope(ctx, "typoed")
		require.EqualError(t, err, `unknown project "typoed"`)
	})

	t.Run("an unsafe slug never reaches the existence check", func(t *testing.T) {
		_, err := s.gardenerRequestScope(ctx, "../notes/_global")
		require.ErrorContains(t, err, "invalid project")
	})
}
