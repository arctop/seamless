package gardener

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// newSplitGardener builds a gardener with the given chat output and seeds three
// active memories in project "example-app" in a known newest-first order:
// [1] ios-thing, [2] android-thing, [3] shared-thing. chatOut == "" => no chat.
func newSplitGardener(t *testing.T, chatOut string) (context.Context, *sql.DB, *Service) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })

	now := time.Now().UTC()
	writeMem(t, mgr, "ios-thing", "example-app", "1", core.KindGotcha, now, "iOS only")
	writeMem(t, mgr, "android-thing", "example-app", "2", core.KindGotcha, now.Add(-time.Hour), "Android only")
	writeMem(t, mgr, "shared-thing", "example-app", "3", core.KindReference, now.Add(-2*time.Hour), "cross-platform")

	var chat llm.Chat
	if chatOut != "" {
		chat = fakeChat{out: chatOut}
	}
	g := New(db, mgr, nil, chat, events.NewRecorder(db), Config{}, slog.Default())
	return ctx, db, g
}

const splitJSON = `{"children":[{"slug":"example-ios","label":"Example iOS"},{"slug":"example-android","label":"Example Android"}],` +
	`"shared":{"slug":"example-mobile-apps","label":"Example Mobile"},` +
	`"assignments":[{"memory":1,"to":"example-ios","rationale":"iOS"},{"memory":2,"to":"example-android","rationale":"Android"},{"memory":3,"to":"example-mobile-apps","rationale":"shared"}]}`

func TestSplit_CreatesSetupAndReprojects(t *testing.T) {
	ctx, db, g := newSplitGardener(t, splitJSON)

	res, err := g.Split(ctx, "example-app", "split into ios and android, keep cross-platform shared")
	require.NoError(t, err)
	require.Equal(t, 4, res.Total, "1 setup + 3 memory moves")
	require.Equal(t, 1, res.ByKind[store.ProposalSplit])
	require.Equal(t, 3, res.ByKind[store.ProposalReproject])
	require.Empty(t, res.Skipped)

	// The setup proposal carries the topology and the plan tag.
	splits, err := store.PendingProposals(ctx, db, store.ProposalSplit)
	require.NoError(t, err)
	require.Len(t, splits, 1)
	require.Equal(t, "example-app", splits[0].Payload["source_project"])
	require.Equal(t, "split-example-app", splits[0].Payload["plan"])
	require.Equal(t, "request", splits[0].Payload["source"])

	// Every reproject is plan-tagged and moves out of the source.
	reps, err := store.PendingProposals(ctx, db, store.ProposalReproject)
	require.NoError(t, err)
	require.Len(t, reps, 3)
	targets := map[string]string{}
	for _, r := range reps {
		require.Equal(t, "split-example-app", r.Payload["plan"])
		require.Equal(t, "example-app", r.Payload["from"])
		targets[r.Payload["name"].(string)] = r.Payload["to"].(string)
	}
	require.Equal(t, "example-ios", targets["ios-thing"])
	require.Equal(t, "example-android", targets["android-thing"])
	require.Equal(t, "example-mobile-apps", targets["shared-thing"])
}

func TestSplit_SkipsInvalidTargets(t *testing.T) {
	// [3]'s target is not among the children or the shared parent -> skipped.
	badJSON := `{"children":[{"slug":"example-ios","label":"iOS"},{"slug":"example-android","label":"Android"}],` +
		`"shared":{"slug":"example-mobile-apps","label":"Mobile"},` +
		`"assignments":[{"memory":1,"to":"example-ios"},{"memory":3,"to":"nonsense-project"}]}`
	ctx, db, g := newSplitGardener(t, badJSON)

	res, err := g.Split(ctx, "example-app", "")
	require.NoError(t, err)
	require.Equal(t, 2, res.Total, "1 setup + 1 valid reproject")
	require.Equal(t, 1, res.ByKind[store.ProposalReproject])
	require.Len(t, res.Skipped, 1, "the out-of-target assignment is skipped, not fabricated")

	reps, err := store.PendingProposals(ctx, db, store.ProposalReproject)
	require.NoError(t, err)
	require.Len(t, reps, 1)
}

func TestSplit_RejectsTooFewChildren(t *testing.T) {
	oneChild := `{"children":[{"slug":"example-ios","label":"iOS"}],"shared":{},"assignments":[]}`
	ctx, db, g := newSplitGardener(t, oneChild)

	_, err := g.Split(ctx, "example-app", "")
	require.Error(t, err, "a split needs at least two children")

	all, err := store.PendingProposals(ctx, db, "")
	require.NoError(t, err)
	require.Empty(t, all, "an invalid plan creates nothing")
}

func TestSplit_NoSourceIsAnError(t *testing.T) {
	ctx, _, g := newSplitGardener(t, splitJSON)
	_, err := g.Split(ctx, "  ", "anything")
	require.ErrorIs(t, err, ErrNoSource)
}

func TestSplit_NoChatIsAnError(t *testing.T) {
	ctx, db, g := newSplitGardener(t, "") // no chat client
	_, err := g.Split(ctx, "example-app", "split it")
	require.ErrorIs(t, err, ErrNoChat)

	all, err := store.PendingProposals(ctx, db, "")
	require.NoError(t, err)
	require.Empty(t, all)
}

func TestSplit_GarbageOutputCreatesNothing(t *testing.T) {
	ctx, db, g := newSplitGardener(t, "sorry, I can't do that")
	_, err := g.Split(ctx, "example-app", "split it")
	require.ErrorIs(t, err, ErrUnparseable)

	all, err := store.PendingProposals(ctx, db, "")
	require.NoError(t, err)
	require.Empty(t, all)
}

func TestSplit_NoMemoriesIsCleanNoResult(t *testing.T) {
	ctx, _, g := newSplitGardener(t, splitJSON)
	res, err := g.Split(ctx, "empty-project", "split it")
	require.NoError(t, err, "an empty source is a clean outcome, not an error")
	require.Equal(t, 0, res.Total)
}
