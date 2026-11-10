package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
)

// The owner-locked view at the store's one enforcement point: CanRead/CanWrite
// and the fts/cosine reach filter (accessFilter) answer for a locked project
// exactly what the request's View answers -- locked for the bearer key,
// for a socket owner without a lease, for an expired lease and for another
// process; open for the one process holding a live lease.

var (
	fenceNow   = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	fenceProc  = access.ProcessToken{PID: 4242, Version: 7, Boot: "6c2f1a3e-0b7d-4c55-9a51-3d8f00a1b2c3"}
	fenceOther = access.ProcessToken{PID: 5151, Version: 2, Boot: fenceProc.Boot}
)

func lockedPolicy(t *testing.T) *access.Policy {
	t.Helper()
	p, err := access.New(config.PeerAuth{Owner: "owner", ForceLocked: []string{"example-finance", "example-legal"}},
		func(n string) (access.Account, error) {
			if n == "owner" {
				return access.Account{UID: 501, Name: "owner"}, nil
			}
			return access.Account{}, fmt.Errorf("no %s", n)
		})
	require.NoError(t, err)
	return p
}

// fencePrincipals is every owner principal the locked view tells apart, each as
// the context a request would carry, plus the no-View context (peer_auth off).
func fencePrincipals(t *testing.T, p *access.Policy, leases []access.Lease) map[string]context.Context {
	t.Helper()
	ctx := context.Background()
	sock := func(proc access.ProcessToken, ls []access.Lease) context.Context {
		v, ok := p.PeerProcessView(501, proc)
		require.True(t, ok)
		return access.WithView(ctx, v.WithLeases(fenceNow, ls))
	}
	expired := make([]access.Lease, len(leases))
	for i, l := range leases {
		l.Expires = fenceNow.Add(-time.Second)
		expired[i] = l
	}
	return map[string]context.Context{
		"no view":                     ctx,
		"owner over TCP":              access.WithView(ctx, p.OwnerView().WithLeases(fenceNow, leases)),
		"owner socket, locked":        sock(fenceProc, nil),
		"owner socket, leased":        sock(fenceProc, leases),
		"owner socket, lease expired": sock(fenceProc, expired),
		"another process, same lease": sock(fenceOther, leases),
	}
}

func TestCanReadCanWrite_OwnerLocked(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, slug := range []string{"example-finance", "example-legal", "example-ops"} {
		_, err := EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, SetProjectIsolation(ctx, db, "example-finance", core.IsolationConfidential))
	require.NoError(t, SetProjectIsolation(ctx, db, "example-legal", core.IsolationConfidential))
	p := lockedPolicy(t)
	lease := access.Lease{ID: "l1", Project: "example-finance", Process: fenceProc.Key(), Expires: fenceNow.Add(3 * time.Hour)}
	principals := fencePrincipals(t, p, []access.Lease{lease})

	type row struct {
		caller, target string
		// want[principal] = {read, write}
		want map[string][2]bool
	}
	locked := [2]bool{false, false}
	all := func(leased, noView [2]bool) map[string][2]bool {
		return map[string][2]bool{
			"no view": noView, "owner over TCP": locked, "owner socket, locked": locked,
			"owner socket, leased": leased, "owner socket, lease expired": locked, "another process, same lease": locked,
		}
	}
	same := func(v [2]bool) map[string][2]bool {
		m := map[string][2]bool{}
		for name := range principals {
			m[name] = v
		}
		return m
	}
	for name, tc := range map[string]row{
		// A session bound INTO the locked project reads and writes it only
		// under the lease -- the binding never stands in for the unlock.
		"bound to the lock, its own project": {"example-finance", "example-finance", all([2]bool{true, true}, [2]bool{true, true})},
		// From an open project: the isolation fence keeps a confidential
		// project unreadable from outside whatever the lease, and lets a write
		// in (inbound is unchanged) -- but only for a View that may write it.
		"bound to an open project, reaching the lock": {"example-ops", "example-finance", all([2]bool{false, true}, [2]bool{false, true})},
		// A lease on one locked project never opens the other.
		"bound to the lock, reaching the other lock": {"example-finance", "example-legal", same(locked)},
		"bound to the other lock, its own project":   {"example-legal", "example-legal", all(locked, [2]bool{true, true})},
		// The owner's open project and global: untouched by the lock.
		"an open project, its own":  {"example-ops", "example-ops", same([2]bool{true, true})},
		"an open project to global": {"example-ops", "", same([2]bool{true, true})},
		// A leased locked session still never writes global (its isolation
		// fences outbound), and reads it only once it may read its own project.
		"bound to the lock, global": {"example-finance", "", all([2]bool{true, false}, [2]bool{true, false})},
	} {
		for who, pctx := range principals {
			t.Run(name+"/"+who, func(t *testing.T) {
				w, ok := tc.want[who]
				require.True(t, ok, "row %q has no expectation for %q", name, who)
				r, err := CanRead(pctx, db, tc.caller, tc.target)
				require.NoError(t, err)
				require.Equal(t, w[0], r, "read %s -> %q", tc.caller, tc.target)
				wr, err := CanWrite(pctx, db, tc.caller, tc.target)
				require.NoError(t, err)
				require.Equal(t, w[1], wr, "write %s -> %q", tc.caller, tc.target)
			})
		}
	}
}

func TestAccessFilter_OwnerLocked(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, row := range []struct{ id, project string }{
		{"m-fin", "example-finance"}, {"m-legal", "example-legal"}, {"m-ops", "example-ops"}, {"m-glob", ""},
	} {
		_, err := db.ExecContext(ctx, `INSERT INTO fts (item_id, kind, project, title, name, description, body) VALUES (?, 'memory', ?, '', 'ledger', '', 'ledger body')`, row.id, row.project)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO embeddings (item_id, kind, model, dims, vec, updated_at) VALUES (?, 'memory', 'm', 2, ?, 'x')`, row.id, EncodeVector([]float32{1, 0}))
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `INSERT INTO memories_index (id, kind, name, project, file_path, content_hash, created_at, updated_at)
			VALUES (?, 'gotcha', ?, ?, ?, 'h', 'x', 'x')`, row.id, row.id, row.project, "memory/"+row.id+".md")
		require.NoError(t, err)
	}
	p := lockedPolicy(t)
	lease := access.Lease{ID: "l1", Project: "example-finance", Process: fenceProc.Key(), Expires: fenceNow.Add(time.Hour)}
	principals := fencePrincipals(t, p, []access.Lease{lease})
	ids := func(ctx context.Context) []string {
		hits, err := FTSSearch(ctx, db, "ledger", nil, nil, 10)
		require.NoError(t, err)
		var out []string
		for _, h := range hits {
			out = append(out, h.ItemID)
		}
		cos, err := CosineSearch(ctx, db, []float32{1, 0}, "m", nil, nil, 10)
		require.NoError(t, err)
		var cout []string
		for _, h := range cos {
			cout = append(cout, h.ItemID)
		}
		require.ElementsMatch(t, out, cout, "the two legs agree on reach")
		return out
	}
	open := []string{"m-ops", "m-glob"}
	for who, want := range map[string][]string{
		"no view":                     {"m-fin", "m-legal", "m-ops", "m-glob"},
		"owner over TCP":              open,
		"owner socket, locked":        open,
		"owner socket, leased":        {"m-fin", "m-ops", "m-glob"},
		"owner socket, lease expired": open,
		"another process, same lease": open,
	} {
		t.Run(who, func(t *testing.T) {
			require.ElementsMatch(t, want, ids(principals[who]))
		})
	}
}
