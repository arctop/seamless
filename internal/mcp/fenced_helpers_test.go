package mcp_test

import (
	"context"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// confidential registers slug as a confidential project no lock covers.
func (r *lockRig) confidential(slug string) {
	r.t.Helper()
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, r.db, slug, slug)
	require.NoError(r.t, err)
	require.NoError(r.t, store.SetProjectIsolation(ctx, r.db, slug, core.IsolationConfidential))
}
