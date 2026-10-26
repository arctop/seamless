package bench

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// A seeded arm is only worth anything if the agent's session BINDS to the
// project the seed filled. That binding is a textual comparison between the
// repo->project map and the cwd the agent's client reports -- and every client
// reports a symlink-resolved cwd, while a fixture computes its paths from
// $TMPDIR, which on macOS lives under /var -> /private/var.
//
// Get that wrong and nothing errors. SessionStart simply misses the mapping,
// registers the same directory a second time under a fresh slug, and hands the
// agent an empty briefing: the memories, plan, tasks and findings are all still
// there, under the first project, invisible. Every Seamless-ful arm then grades
// as "the mechanism did not fire", which is indistinguishable from Seamless
// genuinely not helping -- a zero-uplift reading produced entirely by a path
// string. It cost a full suite once (2026-07-28); this test is why it cannot
// again.
func TestScenarioSeed_BindsRepoThroughASymlinkedPath(t *testing.T) {
	ctx := context.Background()

	for _, sc := range Scenarios() {
		t.Run(sc.Name, func(t *testing.T) {
			// The shape of a real fixture: a repo reached through a symlinked
			// ancestor, exactly as $TMPDIR reaches one on macOS.
			root := t.TempDir()
			real := filepath.Join(root, "physical", "myapp")
			require.NoError(t, os.MkdirAll(real, 0o755))
			link := filepath.Join(root, "logical")
			require.NoError(t, os.Symlink(filepath.Join(root, "physical"), link))
			logical := filepath.Join(link, "myapp")

			// Seed through the LOGICAL path, which is what a harness that did
			// not resolve its base dir would hand the seeder.
			dataDir := t.TempDir()
			s := newSeeder(t, dataDir)
			require.NoError(t, sc.Seed(s, logical))
			require.NoError(t, s.Close())

			db, err := store.Open(filepath.Join(dataDir, "seam.db"))
			require.NoError(t, err)
			defer func() { require.NoError(t, db.Close()) }()

			// What the client will actually send.
			reported, err := filepath.EvalSymlinks(logical)
			require.NoError(t, err)
			require.NotEqual(t, logical, reported, "test setup: the symlink resolved to itself")

			slug, adopted, err := store.RegisterProjectForCWD(ctx, db, store.CWDIdentity{CWD: reported}, "")
			require.NoError(t, err)
			require.Nil(t, adopted)
			require.Equal(t, benchProject, slug,
				"a resolved cwd did not bind to the seeded project: the arm would brief empty")

			// The second-project mint is the failure's signature, so assert the
			// absence directly rather than trusting the slug above.
			projects, err := store.ListProjects(ctx, db)
			require.NoError(t, err)
			require.Len(t, projects, 1, "seed + session start left more than one project")
			require.Equal(t, benchProject, projects[0].Slug)
		})
	}
}
