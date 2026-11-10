package mcp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
)

// lockedBlockerProject names a locked project only to the owner's own view
// holding no lease on it: no View (peer_auth off), a leased view and a
// restricted account all get "".
// The rig's names (locked_test.go is the external test package).
const (
	lnFin = "example-finance"
	lnOps = "example-ops"
)

var lnAgent = access.ProcessToken{PID: 200, Version: 2000, Boot: "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"}

func TestLockedBlockerProject(t *testing.T) {
	policy, err := access.New(config.PeerAuth{Owner: "owner", ForceLocked: []string{lnFin}, Restricted: []string{"_example_agent"}},
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
	ctx := context.Background()
	now := time.Now()
	owner := policy.OwnerView()
	sock, ok := policy.PeerChainView(501, []access.ProcessToken{lnAgent})
	require.True(t, ok)
	leased := sock.WithLeases(now, []access.Lease{{Project: lnFin, Process: lnAgent.Key(), Expires: now.Add(time.Hour)}})
	restricted, ok := policy.PeerView(503)
	require.True(t, ok)
	restricted = restricted.WithOpenProjects([]string{lnOps})

	for _, tc := range []struct {
		name    string
		ctx     context.Context
		project string
		want    string
	}{
		{"no view", ctx, lnFin, ""},
		{"owner on TCP", access.WithView(ctx, owner), lnFin, lnFin},
		{"owner on the socket", access.WithView(ctx, sock), lnFin, lnFin},
		{"a project that is not locked", access.WithView(ctx, owner), lnOps, ""},
		{"a leased tree", access.WithView(ctx, leased), lnFin, ""},
		{"a restricted account", access.WithView(ctx, restricted), lnFin, ""},
	} {
		require.Equal(t, tc.want, lockedBlockerProject(tc.ctx, tc.project), tc.name)
	}
}
