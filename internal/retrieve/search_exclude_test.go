package retrieve

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/store"
)

// skExcludeFixture seeds 25 memories and 25 notes in a project the caller
// leaves out, each naming the term three times so every one outranks the
// visible matches, and visible matches of each kind in two open projects and
// the global scope.
func skExcludeFixture(t *testing.T, visible int) *Service {
	t.Helper()
	db := setupDB(t)
	for i := range 25 {
		insMem(t, db, fmt.Sprintf("01SKXM%02d", i), "gotcha", fmt.Sprintf("sk-left-out-%02d", i), "harbor harbor harbor", "example-sealed")
		insSearchNote(t, db, fmt.Sprintf("01SKXN%02d", i), fmt.Sprintf("sk-left-out-note-%02d", i), "harbor harbor harbor", "example-sealed")
	}
	projects := []string{"example-ops", "example-finance", ""}
	for i := range visible {
		p := projects[i%len(projects)]
		insMem(t, db, fmt.Sprintf("01SKVM%02d", i), "gotcha", fmt.Sprintf("sk-shown-%02d", i), "the harbor and the long way round it, by the old road", p)
		insSearchNote(t, db, fmt.Sprintf("01SKVN%02d", i), fmt.Sprintf("sk-shown-note-%02d", i), "the harbor and the long way round it, by the old road", p)
	}
	return New(db, nil, budgets(), nil)
}

func skProjects(hits []Hit) map[string]int {
	out := map[string]int{}
	for _, h := range hits {
		out[h.Project]++
	}
	return out
}

// Exclude leaves a project's hits out before any cap, the way a hit the
// caller's view may not read is left out: a left-out project that outranks
// everything can no longer take a kind's slots and leave its count short.
//
// These run as the console page does (Semantic, so each leg reads at least
// searchSourceDepth candidates; with no embedder that is the lexical leg
// alone). Exclude does not reach into the legs: a left-out project with more
// matches than a leg's depth can still crowd it (SearchInput.Exclude).
func TestSkSearch_ExcludeDropsBeforeTheCap(t *testing.T) {
	ctx := context.Background()
	svc := skExcludeFixture(t, 3)
	perKind := map[string]int{"memory": 21, "note": 21}

	// The fixture's premise: without Exclude the left-out project takes every
	// slot of each kind.
	hits, err := svc.Search(ctx, SearchInput{Query: "harbor", Semantic: true, PerKind: perKind, Starred: store.StarredFirst})
	require.NoError(t, err)
	require.Equal(t, map[string]int{"example-sealed": 42}, skProjects(hits))

	for _, starred := range []store.SearchStarred{store.StarredAny, store.StarredFirst} {
		hits, err = svc.Search(ctx, SearchInput{Query: "harbor", Semantic: true, PerKind: perKind, Starred: starred,
			Exclude: []string{"example-sealed"}})
		require.NoError(t, err)
		require.Equal(t, 3, countKind(hits, "memory"), "starred=%v: every visible memory, and so the whole count", starred)
		require.Equal(t, 3, countKind(hits, "note"), "starred=%v", starred)
		require.Zero(t, skProjects(hits)["example-sealed"], "starred=%v", starred)
	}

	// The shared-Limit ranking leaves it out the same way.
	shared, err := svc.Search(ctx, SearchInput{Query: "harbor", Limit: 21, Exclude: []string{"example-sealed"}})
	require.NoError(t, err)
	require.Len(t, shared, 6)
	require.Zero(t, skProjects(shared)["example-sealed"])

	// Only the projects named are left out: the global scope is not one.
	require.Equal(t, map[string]int{"example-ops": 2, "example-finance": 2, "": 2}, skProjects(shared))
}

// A kind with more visible matches than its cap still fills it, all visible,
// so a caller asking one past what it shows learns there are more.
func TestSkSearch_ExcludeStillFillsTheCap(t *testing.T) {
	ctx := context.Background()
	svc := skExcludeFixture(t, 22)
	hits, err := svc.Search(ctx, SearchInput{Query: "harbor", Semantic: true,
		PerKind: map[string]int{"memory": 21, "note": 21}, Starred: store.StarredFirst,
		Exclude: []string{"example-sealed"}})
	require.NoError(t, err)
	require.Equal(t, 21, countKind(hits, "memory"))
	require.Equal(t, 21, countKind(hits, "note"))
	require.Zero(t, skProjects(hits)["example-sealed"])
}

// The starred lane ranks the whole starred set, and leaves the excluded
// project's starred items out like the rest.
func TestSkSearch_ExcludeHoldsInTheStarredLane(t *testing.T) {
	ctx := context.Background()
	svc := skExcludeFixture(t, 3)
	starSearchItem(t, svc.db, "memories_index", "01SKXM00")
	starSearchItem(t, svc.db, "memories_index", "01SKVM01")
	in := SearchInput{Query: "harbor", Semantic: true, PerKind: map[string]int{"memory": 21}, Scope: "memories",
		Exclude: []string{"example-sealed"}}

	in.Starred = store.StarredOnly
	only, err := svc.Search(ctx, in)
	require.NoError(t, err)
	require.Equal(t, []string{"01SKVM01"}, hitIDs(only))

	in.Starred = store.StarredFirst
	first, err := svc.Search(ctx, in)
	require.NoError(t, err)
	require.Len(t, first, 3)
	require.Equal(t, "01SKVM01", first[0].ID, "the visible starred match leads")
	require.NotContains(t, hitIDs(first), "01SKXM00")
}

// A search whose provider is down says so once, not once per kind and lane:
// the lanes share one embedding (onceEmbedder), and its failure too.
func TestSkSearch_EmbedFailureWarnsOnce(t *testing.T) {
	ctx := context.Background()
	db := setupDB(t)
	insMem(t, db, "01SKEM", "gotcha", "sk-embed-memory", "embed failure", "seam")
	starSearchItem(t, db, "memories_index", "01SKEM")
	insSearchNote(t, db, "01SKEN", "sk-embed-note", "embed failure", "seam")
	starSearchItem(t, db, "notes_index", "01SKEN")
	insMem(t, db, "01SKEM2", "gotcha", "sk-embed-memory-2", "embed failure again", "seam")

	var logs bytes.Buffer
	down := fmt.Errorf("llm.OpenAI.Embed: %w: dial tcp: connection refused", llm.ErrUnavailable)
	svc := New(db, erroringEmbedder{err: down}, budgets(), slog.New(slog.NewTextHandler(&logs, nil)))
	hits, err := svc.Search(ctx, SearchInput{Query: "embed failure", Semantic: true,
		PerKind: map[string]int{"memory": 21, "note": 21}, Starred: store.StarredFirst})
	require.NoError(t, err)
	require.Equal(t, 2, countKind(hits, "memory"), "a provider failure degrades to lexical, every lane")
	require.Equal(t, 1, countKind(hits, "note"))
	require.Equal(t, 1, strings.Count(logs.String(), "embed failed"), logs.String())
}
