//go:build darwin || linux

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
)

// Doctor names each restricted account and warns about a socket the account
// could not reach: one under the owner's home, or behind a directory other
// accounts cannot search.
func TestPeerAuthChecks_RestrictedRows(t *testing.T) {
	l, cfg := newPALayout(t, "  restricted: [_example_agent]\n")
	env := paTestEnv(l, uint32(os.Geteuid())+1000)
	base := env.resolve
	env.resolve = func(name string) (access.Account, error) {
		if name == "_example_agent" {
			return access.Account{UID: 4702, Name: "_example_agent"}, nil
		}
		return base(name)
	}
	text := func() string {
		var b strings.Builder
		for _, r := range peerAuthChecks(context.Background(), cfg, nil, env) {
			require.NotEqual(t, statusFail, r.status, "%s: %s", r.name, r.detail)
			fmt.Fprintf(&b, "%v %s %s\n", r.status, r.name, r.detail)
		}
		return b.String()
	}
	out := text()
	require.Contains(t, out, "peer_auth.restricted _example_agent (uid 4702): restricted -- the socket only")
	runWarning := "directory " + l.run + " (mode 0750) is not searchable by other accounts"
	require.NotContains(t, out, "directory "+l.run+" (mode", "the runbook layout's run dir is 0755")

	require.NoError(t, os.Chmod(l.run, 0o750))
	t.Cleanup(func() { _ = os.Chmod(l.run, 0o755) })
	out = text()
	require.Contains(t, out, runWarning)

	cfg.PeerAuth.Socket.Group = "staff"
	out = text()
	require.NotContains(t, out, runWarning, "a socket group is the owner's to check")
}

func TestPathWithin(t *testing.T) {
	require.True(t, pathWithin("/Users/alice/run/s.sock", "/Users/alice"))
	require.True(t, pathWithin("/Users/alice", "/Users/alice"))
	require.False(t, pathWithin("/Users/alicex/s.sock", "/Users/alice"))
	require.False(t, pathWithin("/opt/seamless/run/s.sock", "/Users/alice"))
}
