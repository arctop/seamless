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
	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// newRequestGardener builds a gardener over a fresh store with the given chat
// output (empty => no chat client at all), and seeds three active global
// memories in a known newest-first order: [1] keep-me, [2] drop-me,
// [3] stale-thing.
func newRequestGardener(t *testing.T, chatOut string) (context.Context, *sql.DB, *Service) {
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
	writeMem(t, mgr, "keep-me", "", "1", core.KindGotcha, now, "canonical")
	writeMem(t, mgr, "drop-me", "", "2", core.KindGotcha, now.Add(-1*time.Hour), "redundant")
	writeMem(t, mgr, "stale-thing", "", "3", core.KindReference, now.Add(-2*time.Hour), "obsolete")

	// A genuine nil llm.Chat interface (not a typed-nil pointer) when no chat.
	var chat llm.Chat
	if chatOut != "" {
		chat = fakeChat{out: chatOut}
	}
	g := New(db, mgr, nil, chat, events.NewRecorder(db), Config{}, slog.Default())
	return ctx, db, g
}

// newReorgGardener builds a gardener over a fresh, empty store (no seeded
// memories) and returns the manager too, so a test can seed project-scoped
// memories and projects itself. chatOut empty => no chat client.
func newReorgGardener(t *testing.T, chatOut string) (context.Context, *sql.DB, *files.Manager, *Service) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mgr, err := files.NewManager(dir, db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })

	var chat llm.Chat
	if chatOut != "" {
		chat = fakeChat{out: chatOut}
	}
	g := New(db, mgr, nil, chat, events.NewRecorder(db), Config{}, slog.Default())
	return ctx, db, mgr, g
}

// TestRequestCandidates_Scope pins the scope contract after the empty-string
// inversion was removed.
//
// This package used to read "" as "every project on the machine" -- the inverse
// of store.ActiveMemories, where an empty slug IS the global scope. That is why
// gardener_request could not simply be routed through the shared scope guards:
// normalizeProject maps "global" to "", so a globals-only request would have
// become a whole-machine scan. Scanning everything is now reachable only by
// asking for it.
func TestRequestCandidates_Scope(t *testing.T) {
	ctx, _, mgr, g := newReorgGardener(t, "")
	now := time.Now().UTC()
	writeMem(t, mgr, "a-global", "", "1", core.KindReference, now, "global body")
	writeMem(t, mgr, "in-alpha", "alpha", "1", core.KindReference, now, "alpha body")
	writeMem(t, mgr, "in-beta", "beta", "1", core.KindReference, now, "beta body")

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
		{"the zero value is globals only", RequestScope{}, []string{"a-global"}},
		{"a slug is that project plus globals", RequestScope{Project: "alpha"}, []string{"a-global", "in-alpha"}},
		{"AllProjects is every project", RequestScope{AllProjects: true}, []string{"a-global", "in-alpha", "in-beta"}},
		{
			"AllProjects wins over Project",
			RequestScope{Project: "alpha", AllProjects: true},
			[]string{"a-global", "in-alpha", "in-beta"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mems, err := g.requestCandidates(ctx, tc.scope)
			require.NoError(t, err)
			require.Equal(t, tc.want, names(mems))
		})
	}
}

func TestRequest_ReprojectMovesToExistingProject(t *testing.T) {
	ctx, db, mgr, g := newReorgGardener(t,
		`{"ops":[{"op":"reproject","target":1,"to":"arctop-ios","reason":"iOS-specific"}]}`)
	now := time.Now().UTC()
	writeMem(t, mgr, "ios-dfu", "arctop-app", "1", core.KindRunbook, now, "the iOS DFU flow")
	_, err := store.EnsureProject(ctx, db, "arctop-ios", "Arctop iOS")
	require.NoError(t, err)

	res, err := g.Request(ctx, "the ios-dfu memory belongs in arctop-ios", RequestScope{AllProjects: true})
	require.NoError(t, err)
	require.Equal(t, 1, res.Total)
	require.Equal(t, 1, res.ByKind[store.ProposalReproject])
	require.Empty(t, res.Skipped)

	rps, err := store.PendingProposals(ctx, db, store.ProposalReproject)
	require.NoError(t, err)
	require.Len(t, rps, 1)
	require.Equal(t, "ios-dfu", rps[0].Payload["name"])
	require.Equal(t, "arctop-app", rps[0].Payload["from"])
	require.Equal(t, "arctop-ios", rps[0].Payload["to"])
	require.Equal(t, "request", rps[0].Payload["source"], "request-originated moves are tagged")
}

func TestRequest_ReprojectToUnknownProjectIsSkipped(t *testing.T) {
	ctx, db, mgr, g := newReorgGardener(t,
		`{"ops":[{"op":"reproject","target":1,"to":"brand-new","reason":"x"}]}`)
	writeMem(t, mgr, "thing", "arctop-app", "1", core.KindGotcha, time.Now().UTC(), "x")

	res, err := g.Request(ctx, "move thing into a project that does not exist", RequestScope{AllProjects: true})
	require.NoError(t, err)
	require.Equal(t, 0, res.Total, "moving into a non-existent project is a split, not a reproject")
	require.Len(t, res.Skipped, 1)
	require.Contains(t, res.Skipped[0], "not an existing project")

	all, err := store.PendingProposals(ctx, db, "")
	require.NoError(t, err)
	require.Empty(t, all)
}

func TestRequest_RekindReclassifiesKind(t *testing.T) {
	ctx, db, mgr, g := newReorgGardener(t,
		`{"ops":[{"op":"rekind","target":1,"kind":"convention","reason":"project-local branding fact"}]}`)
	writeMem(t, mgr, "wordmark-rule", "seamless", "1", core.KindConstraint, time.Now().UTC(), "branding")

	res, err := g.Request(ctx, "the wordmark memory is a convention, not a constraint", RequestScope{AllProjects: true})
	require.NoError(t, err)
	require.Equal(t, 1, res.Total)
	require.Equal(t, 1, res.ByKind[store.ProposalRekind])
	require.Empty(t, res.Skipped)
	require.Contains(t, res.Summary, "1 rekind")

	rks, err := store.PendingProposals(ctx, db, store.ProposalRekind)
	require.NoError(t, err)
	require.Len(t, rks, 1)
	require.Equal(t, "wordmark-rule", rks[0].Payload["name"])
	require.Equal(t, "seamless", rks[0].Payload["project"])
	require.Equal(t, "constraint", rks[0].Payload["from"])
	require.Equal(t, "convention", rks[0].Payload["to"])
	require.Equal(t, "request", rks[0].Payload["source"], "request-originated rekinds are tagged")
}

func TestRequest_RekindInvalidOrSameKindIsSkipped(t *testing.T) {
	ctx, db, mgr, g := newReorgGardener(t,
		`{"ops":[{"op":"rekind","target":1,"kind":"banana","reason":"x"},{"op":"rekind","target":2,"kind":"constraint","reason":"x"}]}`)
	now := time.Now().UTC()
	writeMem(t, mgr, "newer-thing", "seamless", "1", core.KindConstraint, now, "a")
	writeMem(t, mgr, "older-thing", "seamless", "2", core.KindConstraint, now.Add(-time.Hour), "b")

	res, err := g.Request(ctx, "reclassify these", RequestScope{AllProjects: true})
	require.NoError(t, err)
	require.Equal(t, 0, res.Total)
	require.Len(t, res.Skipped, 2)
	require.Contains(t, res.Skipped[0], "unknown kind")
	require.Contains(t, res.Skipped[1], "already a constraint")

	all, err := store.PendingProposals(ctx, db, "")
	require.NoError(t, err)
	require.Empty(t, all)
}

func TestRequest_RecognizesSplitAndRoutes(t *testing.T) {
	ctx, db, mgr, g := newReorgGardener(t,
		`{"split":{"source":"arctop-app","instruction":"ios and android"}}`)
	writeMem(t, mgr, "ios-thing", "arctop-app", "1", core.KindGotcha, time.Now().UTC(), "iOS only")

	res, err := g.Request(ctx, "split arctop-app into arctop-ios and arctop-android", RequestScope{AllProjects: true})
	require.NoError(t, err)
	require.Equal(t, "arctop-app", res.SplitSource, "the source project is recognized and returned")
	require.True(t, res.SplitIntent, "a recognized split is flagged structurally")
	require.NotEmpty(t, res.Guidance)
	require.Contains(t, res.Guidance, "gardener_split", "guidance routes to the split tool")
	require.Equal(t, 0, res.Total, "routing a split creates no proposals itself")

	all, err := store.PendingProposals(ctx, db, "")
	require.NoError(t, err)
	require.Empty(t, all, "the general request never plans a split -- it only routes")
}

func TestRequest_SplitUnknownSourceGivesGuidance(t *testing.T) {
	ctx, _, mgr, g := newReorgGardener(t, `{"split":{"source":"ghost-project"}}`)
	writeMem(t, mgr, "thing", "arctop-app", "1", core.KindGotcha, time.Now().UTC(), "x")

	res, err := g.Request(ctx, "split the ghost project apart", RequestScope{AllProjects: true})
	require.NoError(t, err)
	require.Empty(t, res.SplitSource, "an unknown source is not routed")
	require.True(t, res.SplitIntent, "split intent is flagged even without a known source")
	require.Contains(t, res.Guidance, "not a known project")
}

func TestRequest_CreatesMergeAndArchive(t *testing.T) {
	ctx, db, g := newRequestGardener(t,
		`{"ops":[{"op":"merge","keep":1,"drop":2},{"op":"archive","target":3,"reason":"obsolete"}]}`)

	res, err := g.Request(ctx, "keep-me and drop-me are duplicates; retire stale-thing", RequestScope{AllProjects: true})
	require.NoError(t, err)
	require.Equal(t, 2, res.Total)
	require.Equal(t, 1, res.ByKind[store.ProposalMerge])
	require.Equal(t, 1, res.ByKind[store.ProposalArchive])
	require.Len(t, res.Created, 2)
	require.Empty(t, res.Skipped)

	merges, err := store.PendingProposals(ctx, db, store.ProposalMerge)
	require.NoError(t, err)
	require.Len(t, merges, 1)
	keep, _ := merges[0].Payload["keep"].(map[string]any)
	drop, _ := merges[0].Payload["drop"].(map[string]any)
	require.Equal(t, "keep-me", keep["name"])
	require.Equal(t, "drop-me", drop["name"])
	require.Equal(t, "request", merges[0].Payload["source"], "request-originated proposals are tagged")

	archives, err := store.PendingProposals(ctx, db, store.ProposalArchive)
	require.NoError(t, err)
	require.Len(t, archives, 1)
	require.Equal(t, "stale-thing", archives[0].Payload["name"])
	require.Equal(t, "obsolete", archives[0].Payload["reason"])
}

func TestRequest_CreatesConsolidate(t *testing.T) {
	ctx, db, g := newRequestGardener(t,
		`{"ops":[{"op":"consolidate","name":"unified","kind":"runbook","description":"one flow","body":"# Unified\ncombined","sources":[1,2,3]}]}`)

	res, err := g.Request(ctx, "the three memories are really one -- consolidate them", RequestScope{AllProjects: true})
	require.NoError(t, err)
	require.Equal(t, 1, res.Total)
	require.Equal(t, 1, res.ByKind[store.ProposalConsolidate])

	cs, err := store.PendingProposals(ctx, db, store.ProposalConsolidate)
	require.NoError(t, err)
	require.Len(t, cs, 1)
	require.Equal(t, "unified", cs[0].Payload["name"])
	require.Equal(t, "runbook", cs[0].Payload["kind"])
	require.Equal(t, "request", cs[0].Payload["source"])
	srcs, ok := cs[0].Payload["sources"].([]any)
	require.True(t, ok)
	require.Len(t, srcs, 3, "all three referenced candidates become sources")
}

func TestRequest_SkipsInvalidOps(t *testing.T) {
	ctx, db, g := newRequestGardener(t,
		`{"ops":[{"op":"merge","keep":1,"drop":1},{"op":"archive","target":99,"reason":"x"},{"op":"delete","target":2}]}`)

	res, err := g.Request(ctx, "do some questionable things", RequestScope{AllProjects: true})
	require.NoError(t, err)
	require.Equal(t, 0, res.Total, "every op is invalid")
	require.Len(t, res.Skipped, 3, "keep==drop, out-of-range, and unknown op are each skipped")

	all, err := store.PendingProposals(ctx, db, "")
	require.NoError(t, err)
	require.Empty(t, all, "no proposal is created from invalid ops")
}

func TestRequest_NoChatIsAnError(t *testing.T) {
	ctx, db, g := newRequestGardener(t, "") // no chat client

	_, err := g.Request(ctx, "merge the duplicates", RequestScope{AllProjects: true})
	require.ErrorIs(t, err, ErrNoChat)

	all, err := store.PendingProposals(ctx, db, "")
	require.NoError(t, err)
	require.Empty(t, all)
}

func TestRequest_EmptyRequestIsAnError(t *testing.T) {
	ctx, _, g := newRequestGardener(t, `{"ops":[]}`)

	_, err := g.Request(ctx, "   ", RequestScope{AllProjects: true})
	require.ErrorIs(t, err, ErrEmptyRequest)
}

func TestRequest_GarbageOutputCreatesNothing(t *testing.T) {
	ctx, db, g := newRequestGardener(t, "I'm sorry, I cannot help with that request.")

	_, err := g.Request(ctx, "merge the duplicates", RequestScope{AllProjects: true})
	require.ErrorIs(t, err, ErrUnparseable)

	all, err := store.PendingProposals(ctx, db, "")
	require.NoError(t, err)
	require.Empty(t, all, "an unparseable completion never fabricates proposals")
}

func TestRequest_EmptyOpsIsCleanNoResult(t *testing.T) {
	ctx, db, g := newRequestGardener(t, "```json\n{\"ops\":[]}\n```")

	res, err := g.Request(ctx, "nothing needs changing", RequestScope{AllProjects: true})
	require.NoError(t, err, "an empty op list is a clean outcome, not an error")
	require.Equal(t, 0, res.Total)
	require.Equal(t, "no proposals matched", res.Summary)

	all, err := store.PendingProposals(ctx, db, "")
	require.NoError(t, err)
	require.Empty(t, all)
}
