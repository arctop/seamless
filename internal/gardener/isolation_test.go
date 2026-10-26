package gardener

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// isolate registers a project and fences it. The row has to exist before a
// project can be isolated at all (an unregistered slug is open by construction).
func isolate(t *testing.T, db *sql.DB, slug string, state core.Isolation) {
	t.Helper()
	ctx := context.Background()
	_, err := store.EnsureProject(ctx, db, slug, slug)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, db, slug, state))
}

// briefName pulls a memory name out of a proposal payload sub-object
// ("keep"/"drop"), so a test can assert which pair was proposed.
func briefName(t *testing.T, payload map[string]any, field string) string {
	t.Helper()
	obj, ok := payload[field].(map[string]any)
	require.True(t, ok, "payload[%q] is a memory brief", field)
	name, ok := obj["name"].(string)
	require.True(t, ok, "payload[%q].name is a string", field)
	return name
}

// A merge proposal names both memories and, applied, supersedes one with the
// other -- so a pair spanning a fence is a leak in the review surface before it
// is one in the store. An isolated memory only ever dedups within its own
// project.
func TestProposeMerges_SkipsPairsSpanningAnIsolatedProject(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	mgr.SetEmbedder(fakeEmbedder{})
	defer func() { _ = mgr.Close() }()

	// Every memory sits on the same embedding axis, so every pair clears the
	// dedup threshold and only the isolation fence can separate them.
	now := time.Now().UTC()
	writeMem(t, mgr, "open-a", "alpha", "1", core.KindGotcha, now, "first copy")
	writeMem(t, mgr, "open-b", "beta", "1", core.KindGotcha, now.Add(-1*time.Hour), "second copy")
	writeMem(t, mgr, "conf-a", "conf", "1", core.KindGotcha, now.Add(-2*time.Hour), "third copy")
	writeMem(t, mgr, "conf-b", "conf", "1", core.KindGotcha, now.Add(-3*time.Hour), "fourth copy")
	writeMem(t, mgr, "sealed-one", "vault", "1", core.KindGotcha, now.Add(-4*time.Hour), "fifth copy")

	isolate(t, db, "conf", core.IsolationConfidential)
	isolate(t, db, "vault", core.IsolationSealed)

	g := New(db, mgr, fakeEmbedder{}, nil, events.NewRecorder(db),
		Config{DedupThreshold: 0.88}, slog.Default())

	created, err := g.proposeMerges(ctx, seenKeys{})
	require.NoError(t, err)
	require.Equal(t, 2, created, "the two open projects pair, and conf pairs within itself")

	merges, err := store.PendingProposals(ctx, db, store.ProposalMerge)
	require.NoError(t, err)
	var pairs []string
	for _, m := range merges {
		pair := []string{briefName(t, m.Payload, "keep"), briefName(t, m.Payload, "drop")}
		slices.Sort(pair)
		pairs = append(pairs, pair[0]+"|"+pair[1])
	}
	slices.Sort(pairs)
	require.Equal(t, []string{"conf-a|conf-b", "open-a|open-b"}, pairs)
}

// The all-projects scan runs unbound, so it reads as the global scope: fenced
// projects drop out of it entirely. A scan bound to a project reads as that
// project, which is what drops the globals for a sealed one.
func TestRequestCandidates_IsolationFence(t *testing.T) {
	ctx, db, mgr, g := newReorgGardener(t, "")
	now := time.Now().UTC()
	writeMem(t, mgr, "a-global", "", "1", core.KindReference, now, "global body")
	writeMem(t, mgr, "in-alpha", "alpha", "1", core.KindReference, now, "alpha body")
	writeMem(t, mgr, "in-conf", "conf", "1", core.KindReference, now, "confidential body")
	writeMem(t, mgr, "in-vault", "vault", "1", core.KindReference, now, "sealed body")

	isolate(t, db, "conf", core.IsolationConfidential)
	isolate(t, db, "vault", core.IsolationSealed)

	names := func(mems []core.Memory) []string {
		out := make([]string, 0, len(mems))
		for _, m := range mems {
			out = append(out, m.Name)
		}
		slices.Sort(out)
		return out
	}

	for _, tc := range []struct {
		name  string
		scope RequestScope
		want  []string
	}{
		{
			"all projects excludes every fenced project",
			RequestScope{AllProjects: true},
			[]string{"a-global", "in-alpha"},
		},
		{
			"a confidential scope still reads its own plus the globals",
			RequestScope{Project: "conf"},
			[]string{"a-global", "in-conf"},
		},
		{
			"a sealed scope reads only itself",
			RequestScope{Project: "vault"},
			[]string{"in-vault"},
		},
		{
			"an open scope is unaffected",
			RequestScope{Project: "alpha"},
			[]string{"a-global", "in-alpha"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mems, err := g.requestCandidates(ctx, tc.scope)
			require.NoError(t, err)
			require.Equal(t, tc.want, names(mems))
		})
	}
}

// The destination half of the fence: what a request may propose moving a memory
// INTO. Inbound to a confidential project is allowed (relocating knowledge into
// the fence is how it gets there); a sealed project admits nothing, and a pass
// running as a fenced project may write nowhere but itself.
func TestWritableProjects_IsolationFence(t *testing.T) {
	ctx, db, _, g := newReorgGardener(t, "")
	for _, slug := range []string{"alpha", "conf", "vault"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	isolate(t, db, "conf", core.IsolationConfidential)
	isolate(t, db, "vault", core.IsolationSealed)

	projects, err := store.ListProjects(ctx, db)
	require.NoError(t, err)

	slugs := func(ps []core.Project) []string {
		out := make([]string, 0, len(ps))
		for _, p := range ps {
			out = append(out, p.Slug)
		}
		slices.Sort(out)
		return out
	}

	for _, tc := range []struct {
		name  string
		scope RequestScope
		want  []string
	}{
		{"unbound: everything but the sealed project", RequestScope{AllProjects: true}, []string{"alpha", "conf"}},
		{"open scope: same", RequestScope{Project: "alpha"}, []string{"alpha", "conf"}},
		{"confidential scope writes only itself", RequestScope{Project: "conf"}, []string{"conf"}},
		{"sealed scope writes only itself", RequestScope{Project: "vault"}, []string{"vault"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := g.writableProjects(ctx, tc.scope, projects)
			require.NoError(t, err)
			require.Equal(t, tc.want, slugs(got))
		})
	}
}

// End to end through Request: the fenced memory is the newest, so without the
// fence it would be candidate [1] and the archive would land on it.
func TestRequest_AllProjectsNeverProposesAgainstAFencedMemory(t *testing.T) {
	ctx, db, mgr, g := newReorgGardener(t, `{"ops":[{"op":"archive","target":1,"reason":"obsolete"}]}`)
	now := time.Now().UTC()
	writeMem(t, mgr, "secret-thing", "conf", "1", core.KindGotcha, now, "fenced body")
	writeMem(t, mgr, "public-thing", "alpha", "2", core.KindGotcha, now.Add(-time.Hour), "open body")
	isolate(t, db, "conf", core.IsolationConfidential)

	res, err := g.Request(ctx, "clean up what is obsolete", RequestScope{AllProjects: true})
	require.NoError(t, err)
	require.Equal(t, 1, res.Total)

	archives, err := store.PendingProposals(ctx, db, store.ProposalArchive)
	require.NoError(t, err)
	require.Len(t, archives, 1)
	require.Equal(t, "public-thing", archives[0].Payload["name"],
		"the fenced memory was never a candidate")
}

// A split relocates the source's memories into new projects and leaves it with
// children, so an isolated source is refused outright rather than partly planned.
func TestSplit_RefusesAnIsolatedSource(t *testing.T) {
	for _, state := range []core.Isolation{core.IsolationConfidential, core.IsolationSealed} {
		t.Run(string(state), func(t *testing.T) {
			ctx, db, g := newSplitGardener(t, splitJSON)
			isolate(t, db, "arctop-app", state)

			_, err := g.Split(ctx, "arctop-app", "split into ios and android")
			require.ErrorIs(t, err, ErrIsolatedProject)
			require.Contains(t, err.Error(), string(state))

			all, err := store.PendingProposals(ctx, db, "")
			require.NoError(t, err)
			require.Empty(t, all, "a refused split plans nothing")
		})
	}

	// The same source splits fine once it is open again.
	ctx, db, g := newSplitGardener(t, splitJSON)
	isolate(t, db, "arctop-app", core.IsolationConfidential)
	require.NoError(t, store.SetProjectIsolation(ctx, db, "arctop-app", core.IsolationOpen))
	res, err := g.Split(ctx, "arctop-app", "split into ios and android")
	require.NoError(t, err)
	require.Equal(t, 4, res.Total)
}
