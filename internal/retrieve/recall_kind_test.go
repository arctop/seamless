package retrieve

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRecallLimitServiceBoundary(t *testing.T) {
	db := setupDB(t)
	svc := New(db, nil, budgets(), nil)
	ctx := context.Background()

	for _, limit := range []int{-1, MaxRecallLimit + 1} {
		_, err := svc.Recall(ctx, RecallInput{Query: "anything", Limit: limit})
		require.ErrorIs(t, err, ErrInvalidRecallLimit)
	}

	// Zero is the Go API's explicit zero-value default; transport boundaries
	// distinguish an absent key from a caller-sent zero and reject the latter.
	_, err := svc.Recall(ctx, RecallInput{Query: "anything", Limit: 0})
	require.NoError(t, err)
	_, err = svc.Recall(ctx, RecallInput{Query: "anything", Limit: MaxRecallLimit})
	require.NoError(t, err)
}

// The kind filter is applied inside the candidate queries: only memories of
// that frontmatter kind return, and notes are excluded even under the default
// all scope -- a kind names a memory attribute, so it implies memories-only.
func TestRecallKindFilterMemoriesOnly(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	insMem(t, db, "01CON", "convention", "wordmark-markup", "wordmark markup sync rule", "seam")
	insMem(t, db, "01GOT", "gotcha", "wordmark-cache", "wordmark cache surprise", "seam")
	insNote(t, db, "01NOT", "wordmark-note", "Wordmark research", "seam", "[]", time.Now())
	_, err := db.ExecContext(ctx, `
		INSERT INTO fts (item_id, kind, project, title, name, description, body)
		VALUES ('01NOT', 'note', 'seam', 'Wordmark research', '', 'wordmark findings', 'wordmark findings')`)
	require.NoError(t, err)

	svc := New(db, nil, budgets(), nil) // nil embedder => FTS-only

	hits, err := svc.Recall(ctx, RecallInput{Query: "wordmark", Project: "seam", Kind: "convention", Limit: 5})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "wordmark-markup", hits[0].Name)

	// The same query unfiltered fuses all three items.
	all, err := svc.Recall(ctx, RecallInput{Query: "wordmark", Project: "seam", Limit: 5})
	require.NoError(t, err)
	require.Len(t, all, 3)
}

// scope=notes plus a kind filter is a contradiction: nothing could ever match,
// so it is rejected loudly instead of returning a misleading empty result. An
// unknown kind is rejected on the same grounds.
func TestRecallKindScopeAndValidity(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	svc := New(db, nil, budgets(), nil)

	_, err := svc.Recall(ctx, RecallInput{Query: "anything", Scope: "notes", Kind: "convention"})
	require.ErrorContains(t, err, "scope=notes")

	_, err = svc.Recall(ctx, RecallInput{Query: "anything", Kind: "conventionz"})
	require.ErrorContains(t, err, "unknown kind")

	// scope=memories plus kind is redundant but coherent, so it works.
	insMem(t, db, "01CON", "convention", "layout-fact", "where things deploy", "seam")
	hits, err := svc.Recall(ctx, RecallInput{Query: "deploy layout", Project: "seam", Scope: "memories", Kind: "convention"})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "layout-fact", hits[0].Name)
}

// A kind with no query is the browse mode: the scope's active memories of that
// kind list newest-first (project plus global, invalidated ones excluded),
// with no fusion source or score attached -- and no query with no kind is an
// error, since there is nothing to search or list.
func TestRecallKindBrowseListsNewestFirst(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	now := time.Now()
	insMemAt(t, db, "01OLD", "convention", "old-fact", "the older layout fact", "seam", now.Add(-3*time.Hour))
	insMemAt(t, db, "01NEW", "convention", "new-fact", "the newer layout fact", "seam", now.Add(-1*time.Hour))
	insMemAt(t, db, "01GLO", "convention", "global-fact", "a global layout fact", "", now.Add(-2*time.Hour))
	insMemAt(t, db, "01OTH", "convention", "other-scope", "another project's fact", "elsewhere", now)
	insMemAt(t, db, "01GOT", "gotcha", "some-gotcha", "a different kind entirely", "seam", now)

	svc := New(db, nil, budgets(), nil)

	hits, err := svc.Recall(ctx, RecallInput{Kind: "convention", Project: "seam", Limit: 10})
	require.NoError(t, err)
	names := make([]string, len(hits))
	for i, h := range hits {
		names[i] = h.Name
		require.Equal(t, "browse", h.Source)
		require.Zero(t, h.Score, "a listing has no fused score")
	}
	require.Equal(t, []string{"new-fact", "global-fact", "old-fact"}, names,
		"newest-first across the project and global scopes, other kinds and projects excluded")

	// The limit caps the listing like a searching recall.
	hits, err = svc.Recall(ctx, RecallInput{Kind: "convention", Project: "seam", Limit: 2})
	require.NoError(t, err)
	require.Len(t, hits, 2)

	// Neither query nor kind: nothing to search, nothing to list.
	_, err = svc.Recall(ctx, RecallInput{Project: "seam"})
	require.ErrorContains(t, err, "query is required unless kind is set")

	// The scope/validity guards run before the browse branch, so a browse with
	// scope=notes or an unknown kind fails the same way a search does.
	_, err = svc.Recall(ctx, RecallInput{Kind: "convention", Scope: "notes"})
	require.ErrorContains(t, err, "scope=notes")
	_, err = svc.Recall(ctx, RecallInput{Kind: "conventionz"})
	require.ErrorContains(t, err, "unknown kind")
}

// Browse excludes superseded/archived memories: the candidate listing is the
// active scope, exactly what the briefing index draws from.
func TestRecallKindBrowseSkipsInvalidated(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	now := time.Now()
	insMemAt(t, db, "01LIV", "convention", "live-fact", "still standing", "seam", now)
	insMemAt(t, db, "01DEA", "convention", "dead-fact", "superseded long ago", "seam", now)
	_, err := db.ExecContext(ctx, `UPDATE memories_index SET invalid_at = ? WHERE id = '01DEA'`, now.UTC().Format(time.RFC3339))
	require.NoError(t, err)

	svc := New(db, nil, budgets(), nil)
	hits, err := svc.Recall(ctx, RecallInput{Kind: "convention", Project: "seam", Limit: 10})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "live-fact", hits[0].Name)
}

// A listing is bounded by its limit alone, never by the search's token budget:
// cut at the budget it returned part of a kind as if it were all of it (21 of
// 39 constraints at limit=100), so agents asked for limits the schema refuses.
// Past the limit it counts what it left out.
func TestRecallKindBrowseListsUpToLimitAndCountsTheRest(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	now := time.Now()
	long := strings.Repeat("a description long enough to weigh on any token budget ", 4)
	for i := range 40 {
		insMemAt(t, db, fmt.Sprintf("01C%02d", i), "constraint", fmt.Sprintf("rule-%02d", i), long, "seam", now.Add(-time.Duration(i)*time.Minute))
	}
	insMemAt(t, db, "01GOT", "gotcha", "some-gotcha", long, "seam", now)
	svc := New(db, nil, budgets(), nil)

	hits, more, err := svc.RecallCounted(ctx, RecallInput{Kind: "constraint", Project: "seam", Limit: 100})
	require.NoError(t, err)
	require.Len(t, hits, 40, "the whole kind, though it weighs far more than RecallBudgetTokens")
	require.Zero(t, more)
	require.Equal(t, "rule-00", hits[0].Name, "still newest-first")

	hits, more, err = svc.RecallCounted(ctx, RecallInput{Kind: "constraint", Project: "seam", Limit: 15})
	require.NoError(t, err)
	require.Len(t, hits, 15)
	require.Equal(t, 25, more, "the matches past the limit, of this kind only")

	// A search counts nothing: its ranking has no fixed tail.
	_, more, err = svc.RecallCounted(ctx, RecallInput{Query: "description", Kind: "constraint", Project: "seam", Limit: 5})
	require.NoError(t, err)
	require.Zero(t, more)
}
