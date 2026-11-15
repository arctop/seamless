//go:build darwin || linux

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// Doctor's rows for shaped restricted entries (decision B-D8): a plain entry's
// row word for word as before, a shaped one's with its shape and what it
// reaches now, a FAIL for an entry an exclude typo disables (serve still
// starts), a WARN for an only slug it cannot reach, and INFO for an excluded
// project that is not open now.
func TestPeerAuthChecks_RestrictedReachRows(t *testing.T) {
	l, cfg := newPALayout(t, `  restricted:
    - _example_agent
    - account: _example_excl
      exclude: [example-docs, example-typo]
    - account: _example_desk
      only: [example-desk]
    - account: _example_mixed
      only: [example-desk, example-ops]
    - account: _example_later
      only: [example-later]
`)
	env := paTestEnv(l, uint32(os.Geteuid())+1000)
	base := env.resolve
	uids := map[string]uint32{"_example_agent": 4702, "_example_excl": 4703, "_example_desk": 4704, "_example_mixed": 4705, "_example_later": 4706}
	env.resolve = func(name string) (access.Account, error) {
		if uid, ok := uids[name]; ok {
			return access.Account{UID: uid, Name: name}, nil
		}
		return base(name)
	}
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for slug, iso := range map[string]core.Isolation{"example-ops": core.IsolationOpen, "example-docs": core.IsolationSealed,
		"example-desk": core.IsolationConfidential} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, iso))
	}

	var b strings.Builder
	for _, r := range peerAuthChecks(ctx, cfg, db, env) {
		fmt.Fprintf(&b, "%v %s %s\n", r.status, r.name, r.detail)
	}
	out := b.String()
	row := func(status checkStatus, detail string) string {
		return fmt.Sprintf("%v peer_auth.restricted %s", status, detail)
	}
	tail := "its writes are labeled with its name, and its constraints, conventions, protocols and stages wait for the owner's promotion"
	require.Contains(t, out, row(statusOK, "_example_agent (uid 4702): restricted -- the socket only; reads and writes every open "+
		"project that is not locked, reads global; "+tail), "the plain row is unchanged")
	require.Contains(t, out, row(statusOK, "_example_excl (uid 4703): restricted, exclude [example-docs, example-typo] -- the socket "+
		"only; reads and writes every open project that is not locked but those, reads global (now 1 project); "+tail))
	require.Contains(t, out, row(statusOK, "_example_desk (uid 4704): restricted, only [example-desk] -- the socket only; reads and "+
		"writes that project while it is open, confidential or sealed and not locked, and reads global unless it is sealed "+
		"(now example-desk (confidential)); "+tail))
	require.Contains(t, out, row(statusOK, "_example_mixed (uid 4705): restricted, only [example-desk, example-ops] -- the socket "+
		"only; reads and writes those of them that are open and not locked, reads global (now example-ops (open)); "+tail))
	require.Contains(t, out, row(statusOK, "_example_later (uid 4706): restricted, only [example-later]"))
	require.Contains(t, out, "(now none)")

	require.Contains(t, out, row(statusFail, `peer_auth.restricted _example_excl excludes project "example-typo", which does not exist`))
	require.Contains(t, out, row(statusInfo, `peer_auth.restricted _example_excl excludes project "example-docs", which is sealed now`))
	require.Contains(t, out, row(statusWarn, `peer_auth.restricted _example_mixed names project "example-desk" under only beside other projects`))
	require.Contains(t, out, row(statusWarn, `peer_auth.restricted _example_later names project "example-later" under only, which does not exist`))
	require.Contains(t, out, "preflight passes: serve will start with the boundary up", "a disabled entry never stops serve")

	// Without a database doctor names the shapes and checks no slug.
	b.Reset()
	for _, r := range peerAuthChecks(ctx, cfg, nil, env) {
		fmt.Fprintf(&b, "%v %s %s\n", r.status, r.name, r.detail)
	}
	out = b.String()
	require.Contains(t, out, "_example_desk (uid 4704): restricted, only [example-desk] -- the socket only; reads and writes that "+
		"project while it is open, confidential or sealed and not locked, and reads global unless it is sealed; "+tail)
	require.NotContains(t, out, "example-typo\", which does not exist")
}
