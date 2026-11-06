package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

func lockOwnerResolve(name string) (access.Account, error) {
	if name == "owner" {
		return access.Account{UID: 501, Name: "owner"}, nil
	}
	return access.Account{}, fmt.Errorf("no such account %q", name)
}

// serve's project preflight names the lock as "locked", never "sealed": a
// locked project whose isolation is open is refused with the remedy, and a
// locked slug with no project behind it is refused as a typo that protects
// nothing.
func TestProjectProblems_LockedWording(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, slug := range []string{"example-finance", "example-legal"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, "example-legal", core.IsolationConfidential))

	policy, err := access.New(config.PeerAuth{Owner: "owner", Locked: []string{"example-finance", "example-legal", "example-typo"}}, lockOwnerResolve)
	require.NoError(t, err)
	problems, err := projectProblems(ctx, db, policy)
	require.NoError(t, err)
	require.Equal(t, []string{
		`project "example-finance" is locked (peer_auth) but its isolation is open: make it confidential first (seam project isolation example-finance confidential --yes)`,
		`peer_auth names project "example-typo", which does not exist: if that is a typo, the project it meant is NOT protected`,
	}, problems)
}

// doctor names the locked list under its own key, and a filled-in list with
// peer_auth off is a warning rather than silence.
func TestPeerAuthChecks_LockedRows(t *testing.T) {
	cfg := config.Defaults()
	cfg.PeerAuth.Locked = []string{"example-finance"}
	rows := peerAuthChecks(context.Background(), cfg, nil, peerAuthEnv{resolve: lockOwnerResolve})
	require.Len(t, rows, 1)
	require.Equal(t, statusWarn, rows[0].status)
	require.Contains(t, rows[0].detail, "peer_auth.locked is filled in")
	require.NotContains(t, rows[0].detail, "sealed")
}
