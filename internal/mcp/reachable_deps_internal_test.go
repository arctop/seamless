package mcp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// A dependency on a task the caller may not reach reads exactly like one on a
// missing id, so an edge can neither confirm a locked task exists nor let the
// caller watch it close. A lease opens it, and peer_auth off asks nothing.
func TestReachableDeps(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	s := &Server{cfg: Config{DB: db}}

	task := func(project string) string {
		id, err := core.NewID()
		require.NoError(t, err)
		_, err = store.EnsureProject(ctx, db, project, project)
		require.NoError(t, err)
		require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: id, ProjectSlug: project, Title: "a task", Status: core.TaskOpen}))
		return id
	}
	locked, open := task(lnFin), task(lnOps)
	missing, err := core.NewID()
	require.NoError(t, err)

	policy, err := access.New(config.PeerAuth{Owner: "owner", Locked: []string{lnFin}, Restricted: []string{"_example_agent"}},
		func(n string) (access.Account, error) {
			switch n {
			case "owner":
				return access.Account{UID: 501, Name: "owner"}, nil
			case "_example_agent":
				return access.Account{UID: 503, Name: "_example_agent"}, nil
			}
			return access.Account{}, fmt.Errorf("no account %s", n)
		})
	require.NoError(t, err)
	now := time.Now()
	sock, ok := policy.PeerChainView(501, []access.ProcessToken{lnAgent})
	require.True(t, ok)
	leased := sock.WithLeases(now, []access.Lease{{Project: lnFin, Process: lnAgent.Key(), Expires: now.Add(time.Hour)}})
	restricted, ok := policy.PeerView(503)
	require.True(t, ok)
	restricted = restricted.WithOpenProjects([]string{lnOps})

	refusalOf := func(ctx context.Context, id string) string {
		err := s.reachableDeps(ctx, []string{id})
		require.Error(t, err)
		require.True(t, errors.Is(err, store.ErrTaskNotFound), "%v", err)
		return strings.ReplaceAll(err.Error(), id, "<id>")
	}

	for _, tc := range []struct {
		name       string
		ctx        context.Context
		wantLocked bool // a dependency on the locked task passes
	}{
		{"peer_auth off", ctx, true},
		{"owner on TCP", access.WithView(ctx, policy.OwnerView()), false},
		{"owner on the socket", access.WithView(ctx, sock), false},
		{"a leased agent", access.WithView(ctx, leased), true},
		{"a restricted account", access.WithView(ctx, restricted), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, s.reachableDeps(tc.ctx, []string{open}), "an open project's task is always reachable")
			if tc.wantLocked {
				require.NoError(t, s.reachableDeps(tc.ctx, []string{locked}))
				return
			}
			require.Equal(t, refusalOf(tc.ctx, missing), refusalOf(tc.ctx, locked), "a locked task reads like a missing one")
		})
	}
}
