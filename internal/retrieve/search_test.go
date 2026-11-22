package retrieve

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/store"
)

// failingEmbedder fails the test if it is ever asked to embed. It pins the
// palette's fast path: a search fired per keystroke must not reach the provider.
type failingEmbedder struct{ t *testing.T }

func (e failingEmbedder) Model() string { return "must-not-be-called" }

func (e failingEmbedder) Embed(context.Context, string) ([]float32, error) {
	e.t.Helper()
	e.t.Fatal("Search with Semantic:false must not call the embedder")
	return nil, nil
}

// The reason Search exists rather than an extended Recall: Recall re-checks each
// fused hit against its single bound project, which would discard every
// project-scoped hit of an all-projects search. Search must return memories from
// every project when Projects is nil.
func TestSearch_CrossProjectWithNilProjects(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	insMem(t, db, "01A", "gotcha", "alpha-thing", "shared searchable topic", "alpha")
	insMem(t, db, "01B", "gotcha", "beta-thing", "shared searchable topic", "beta")
	insMem(t, db, "01G", "reference", "global-thing", "shared searchable topic", "")

	svc := New(db, nil, budgets(), nil)

	hits, err := svc.Search(ctx, SearchInput{Query: "shared searchable topic", Limit: 20})
	require.NoError(t, err)

	projects := make([]string, len(hits))
	for i, h := range hits {
		projects[i] = h.Project
	}
	require.ElementsMatch(t, []string{"alpha", "beta", ""}, projects,
		"a nil Projects scope must reach every project, not just global")
}

// Projects is the one place scope is enforced -- inside the candidate queries.
func TestSearch_ProjectsFilterScopes(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	insMem(t, db, "01A", "gotcha", "alpha-thing", "shared searchable topic", "alpha")
	insMem(t, db, "01B", "gotcha", "beta-thing", "shared searchable topic", "beta")

	svc := New(db, nil, budgets(), nil)

	hits, err := svc.Search(ctx, SearchInput{Query: "shared searchable topic", Projects: []string{"alpha"}, Limit: 20})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "alpha", hits[0].Project)
}

// Recall packs into a token budget and stops early; a search page owes the
// observer every hit up to their limit. A tiny budget must not truncate Search.
func TestSearch_IgnoresRecallTokenBudget(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	for i := range 10 {
		insMem(t, db, fmt.Sprintf("01BUD%03d", i), "gotcha", fmt.Sprintf("budget-%02d", i),
			"shared searchable topic with a fairly long description to burn tokens", "seam")
	}
	// A budget so small Recall yields exactly one hit.
	tiny := config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1}
	svc := New(db, nil, tiny, nil)

	recalled, err := svc.Recall(ctx, RecallInput{Query: "shared searchable topic", Project: "seam", Limit: 10})
	require.NoError(t, err)
	require.Len(t, recalled, 1, "fixture check: the budget must actually bind Recall")

	hits, err := svc.Search(ctx, SearchInput{Query: "shared searchable topic", Limit: 10})
	require.NoError(t, err)
	require.Len(t, hits, 10, "Search must not silently truncate on the recall token budget")
}

func TestSearch_LimitCaps(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	for i := range 10 {
		insMem(t, db, fmt.Sprintf("01LIM%03d", i), "gotcha", fmt.Sprintf("limited-%02d", i),
			"shared searchable topic", "seam")
	}
	svc := New(db, nil, budgets(), nil)

	hits, err := svc.Search(ctx, SearchInput{Query: "shared searchable topic", Limit: 3})
	require.NoError(t, err)
	require.Len(t, hits, 3)
}

// The degradation contract is shared code, so it must hold identically here: a
// remote failure degrades to lexical-only rather than failing the page.
func TestSearch_ProviderFailureDegradesToFTS(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"unavailable", fmt.Errorf("llm.OpenAI.Embed: %w: dial tcp: connection refused", llm.ErrUnavailable)},
		{"auth", fmt.Errorf("llm.OpenAI.Embed: %w: status 401", llm.ErrAuth)},
		{"rate limited", fmt.Errorf("llm.OpenAI.Embed: %w: status 429", llm.ErrRateLimited)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupDB(t)
			insMem(t, db, "01A", "gotcha", "chroma-boot-race", "chroma container health check", "seam")

			svc := New(db, erroringEmbedder{err: tc.err}, budgets(), slog.New(slog.DiscardHandler))

			hits, err := svc.Search(context.Background(), SearchInput{
				Query: "chroma health check", Semantic: true, Limit: 5,
			})
			require.NoError(t, err)
			require.NotEmpty(t, hits, "lexical results must survive a provider failure")
			require.Equal(t, "fts", hits[0].Source)
		})
	}
}

// A local config defect must surface, not hide behind quietly worse results.
func TestSearch_ConfigErrorSurfaces(t *testing.T) {
	db := setupDB(t)
	insMem(t, db, "01A", "gotcha", "chroma-boot-race", "chroma container health check", "seam")

	badReq := fmt.Errorf("llm.OpenAI.Embed: %w: new request: parse %q: missing protocol scheme", llm.ErrConfig, "://x")
	svc := New(db, erroringEmbedder{err: badReq}, budgets(), slog.New(slog.DiscardHandler))

	_, err := svc.Search(context.Background(), SearchInput{
		Query: "chroma health check", Semantic: true, Limit: 5,
	})
	require.Error(t, err)
	require.ErrorIs(t, err, llm.ErrConfig)
	require.False(t, errors.Is(err, llm.ErrUnavailable), "a config error must not masquerade as an outage")
}

// The palette fires a query per keystroke; paying a remote embedding round-trip
// each time would be latency and rate-limit pressure for nothing.
func TestSearch_FastPathSkipsEmbedder(t *testing.T) {
	db := setupDB(t)
	insMem(t, db, "01A", "gotcha", "chroma-boot-race", "chroma container health check", "seam")

	svc := New(db, failingEmbedder{t: t}, budgets(), slog.New(slog.DiscardHandler))

	hits, err := svc.Search(context.Background(), SearchInput{
		Query: "chroma health check", Semantic: false, Limit: 5,
	})
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	require.Equal(t, "fts", hits[0].Source)
}

func TestSearch_SnippetSetForFTSHits(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	insMem(t, db, "01A", "gotcha", "boot-race", "the daemon hits a chroma boot race", "seam")

	svc := New(db, nil, budgets(), nil)
	hits, err := svc.Search(ctx, SearchInput{Query: "chroma", Limit: 5})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Contains(t, hits[0].Snippet, store.SnippetStartMark+"chroma"+store.SnippetEndMark)
}

// Recall's JSON payload is an agent-facing contract; adding Snippet to the
// shared Hit type must not change what recall emits.
func TestRecall_NeverSetsSnippet(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	insMem(t, db, "01A", "gotcha", "boot-race", "the daemon hits a chroma boot race", "seam")

	svc := New(db, nil, budgets(), nil)
	hits, err := svc.Recall(ctx, RecallInput{Query: "chroma", Project: "seam", Limit: 5})
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	for _, h := range hits {
		require.Empty(t, h.Snippet, "recall must leave Snippet empty so its JSON stays byte-identical")
	}
}

// Superseded memories leave the candidate set (F20); a search must never surface
// a retired revision.
func TestSearch_ExcludesSuperseded(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	insMem(t, db, "01LIVE", "gotcha", "live-one", "shared searchable topic", "seam")
	insMem(t, db, "01DEAD", "gotcha", "dead-one", "shared searchable topic", "seam")
	_, err := db.ExecContext(ctx,
		`UPDATE memories_index SET invalid_at = updated_at, superseded_by = '01LIVE' WHERE id = '01DEAD'`)
	require.NoError(t, err)

	svc := New(db, nil, budgets(), nil)
	hits, err := svc.Search(ctx, SearchInput{Query: "shared searchable topic", Limit: 10})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "live-one", hits[0].Name)
}

func TestSearch_ScopeFiltersKind(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	insMem(t, db, "01M", "gotcha", "mem-thing", "shared searchable topic", "seam")
	insNote(t, db, "01N", "note-thing", "shared searchable topic", "seam", "[]", time.Now())
	_, err := db.ExecContext(ctx, `
		INSERT INTO fts (item_id, kind, project, title, name, description, body)
		VALUES ('01N', 'note', 'seam', 'shared searchable topic', 'note-thing', 'd', 'shared searchable topic')`)
	require.NoError(t, err)

	svc := New(db, nil, budgets(), nil)

	all, err := svc.Search(ctx, SearchInput{Query: "shared searchable topic", Scope: "all", Limit: 10})
	require.NoError(t, err)
	require.Len(t, all, 2)

	mems, err := svc.Search(ctx, SearchInput{Query: "shared searchable topic", Scope: "memories", Limit: 10})
	require.NoError(t, err)
	require.Len(t, mems, 1)
	require.Equal(t, "memory", mems[0].Kind)

	notes, err := svc.Search(ctx, SearchInput{Query: "shared searchable topic", Scope: "notes", Limit: 10})
	require.NoError(t, err)
	require.Len(t, notes, 1)
	require.Equal(t, "note", notes[0].Kind)
}

// A query with no usable term is "no results", not an error -- the console gates
// on a 2-char floor, but a punctuation-only query must still be quiet.
func TestSearch_UnusableQueryReturnsNothing(t *testing.T) {
	db := setupDB(t)
	insMem(t, db, "01A", "gotcha", "boot-race", "chroma boot race", "seam")

	svc := New(db, nil, budgets(), nil)
	hits, err := svc.Search(context.Background(), SearchInput{Query: "!!! ???", Limit: 5})
	require.NoError(t, err)
	require.Empty(t, hits)
}

// Recall's own contract must stay pinned now that it shares Search's pipeline:
// it still enforces its single-project scope after fusion.
func TestRecall_StillDropsOutOfScopeProjects(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	insMem(t, db, "01A", "gotcha", "alpha-thing", "shared searchable topic", "alpha")
	insMem(t, db, "01B", "gotcha", "beta-thing", "shared searchable topic", "beta")
	insMem(t, db, "01G", "reference", "global-thing", "shared searchable topic", "")

	svc := New(db, nil, budgets(), nil)
	hits, err := svc.Recall(ctx, RecallInput{Query: "shared searchable topic", Project: "alpha", Limit: 10})
	require.NoError(t, err)

	for _, h := range hits {
		require.True(t, h.Project == "alpha" || h.Project == "",
			"recall leaked project %q into an alpha-scoped recall", h.Project)
	}
	names := make([]string, len(hits))
	for i, h := range hits {
		names[i] = h.Name
	}
	require.ElementsMatch(t, []string{"alpha-thing", "global-thing"}, names)
}

// The cosine leg is pure nearest-neighbor: without a floor there is always a
// "nearest" item, so any query -- including nonsense -- fills the page to its
// limit. A semantic-only hit below search.semantic_floor must be dropped, and
// setting the floor to 0 must restore the keep-everything behavior.
func TestSearch_SemanticFloorDropsWeakHits(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	// Neither description shares a term with the query, isolating the cosine leg.
	insMem(t, db, "01STRONG", "gotcha", "strong-vector", "unrelated words entirely", "seam")
	require.NoError(t, store.UpsertEmbedding(ctx, db, "01STRONG", "memory", "test-embed", []float32{0.6, 0.8, 0}))
	insMem(t, db, "01WEAK", "gotcha", "weak-vector", "different unrelated words", "seam")
	require.NoError(t, store.UpsertEmbedding(ctx, db, "01WEAK", "memory", "test-embed", []float32{0.2, 0.98, 0}))

	svc := New(db, fixedEmbedder{vec: []float32{1, 0, 0}}, budgets(), nil)
	svc.SetSearchConfig(config.Search{SemanticFloor: 0.3})

	hits, err := svc.Search(ctx, SearchInput{Query: "quantum flux capacitor", Semantic: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "strong-vector", hits[0].Name)
	require.InDelta(t, 0.6, hits[0].Similarity, 0.01,
		"a semantic hit must report its raw cosine similarity")

	svc.SetSearchConfig(config.Search{SemanticFloor: 0})
	hits, err = svc.Search(ctx, SearchInput{Query: "quantum flux capacitor", Semantic: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, hits, 2, "a zero floor must keep every nearest neighbor")
}

// A hit the lexical leg also matched earned its place by keyword, regardless of
// how far its vector is; the floor applies only to semantic-only hits. Its
// similarity still surfaces so the observer sees the weak distance.
func TestSearch_SemanticFloorExemptsLexicalMatches(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	insMem(t, db, "01FUSE", "gotcha", "fused-hit", "quantum capacitor sizing notes", "seam")
	require.NoError(t, store.UpsertEmbedding(ctx, db, "01FUSE", "memory", "test-embed", []float32{0.2, 0.98, 0}))

	svc := New(db, fixedEmbedder{vec: []float32{1, 0, 0}}, budgets(), nil)
	svc.SetSearchConfig(config.Search{SemanticFloor: 0.3})

	hits, err := svc.Search(ctx, SearchInput{Query: "quantum capacitor", Semantic: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, hits, 1, "a keyword match must survive a below-floor vector")
	require.Equal(t, "fused", hits[0].Source)
	require.InDelta(t, 0.2, hits[0].Similarity, 0.01)
}

// A lexical-only hit has no vector distance to report: Similarity must stay
// zero rather than posing as a measured relevance.
func TestSearch_LexicalOnlyHitOmitsSimilarity(t *testing.T) {
	db := setupDB(t)

	insMem(t, db, "01A", "gotcha", "boot-race", "the daemon hits a chroma boot race", "seam")

	svc := New(db, nil, budgets(), nil)
	hits, err := svc.Search(context.Background(), SearchInput{Query: "chroma", Limit: 5})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Zero(t, hits[0].Similarity)
}

// Recall's JSON payload is an agent-facing contract (see TestRecall_NeverSetsSnippet),
// and the floor is Search's guard alone: recall keeps every neighbor, however
// weak, and never emits Similarity.
func TestRecall_IgnoresFloorAndNeverSetsSimilarity(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()

	insMem(t, db, "01WEAK", "gotcha", "weak-vector", "different unrelated words", "seam")
	require.NoError(t, store.UpsertEmbedding(ctx, db, "01WEAK", "memory", "test-embed", []float32{0.2, 0.98, 0}))

	svc := New(db, fixedEmbedder{vec: []float32{1, 0, 0}}, budgets(), nil)
	svc.SetSearchConfig(config.Search{SemanticFloor: 0.3})

	hits, err := svc.Recall(ctx, RecallInput{Query: "quantum flux capacitor", Project: "seam", Limit: 10})
	require.NoError(t, err)
	require.Len(t, hits, 1, "recall must keep a below-floor neighbor")
	require.Zero(t, hits[0].Similarity, "recall must leave Similarity empty so its JSON stays byte-identical")
}

// The console renders a snippet by escaping first and substituting second. Pin
// that the sentinels are what survives that order -- a literal "<mark>" in a body
// would not.
func TestSearch_SnippetCarriesSentinelsNotMarkup(t *testing.T) {
	db := setupDB(t)
	insMem(t, db, "01A", "gotcha", "xss-fixture", "payload <script>alert(1)</script> chroma", "seam")

	svc := New(db, nil, budgets(), nil)
	hits, err := svc.Search(context.Background(), SearchInput{Query: "chroma", Limit: 5})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.NotContains(t, hits[0].Snippet, "<mark>", "the store must not emit markup")
	require.True(t, strings.Contains(hits[0].Snippet, store.SnippetStartMark))
}

func TestSearch_IdentifierLanePromotesExactNoteSlug(t *testing.T) {
	db := setupDB(t)
	now := time.Now().UTC()
	const noteID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	slug := "deep-code-and-security-audit-2026-07-31"

	// The note deliberately has no FTS row. The memory supplies noisy matches
	// for the slug's common terms, reproducing the console failure where the
	// intended note was buried among audit/date hits.
	insNote(t, db, noteID, slug, "Deep code and security audit", "seam", `[]`, now.Add(-time.Hour))
	insMem(t, db, "01NOISE", "reference", "newer-audit-summary", "deep code security audit 2026 07 31", "seam")

	svc := New(db, nil, budgets(), nil)
	hits, err := svc.Search(context.Background(), SearchInput{Query: slug, Scope: "notes", Limit: 20})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, noteID, hits[0].ID)
	require.Equal(t, "identifier", hits[0].Source)
	require.Equal(t, store.IdentifierMatchExactIdentifier, hits[0].IdentifierMatch)

	// Recall intentionally remains the agent's fused text/semantic surface; it
	// must not silently acquire console-only identifier behavior.
	recalled, err := svc.Recall(context.Background(), RecallInput{Query: slug, Project: "seam", Scope: "notes", Limit: 20})
	require.NoError(t, err)
	require.Empty(t, recalled)
}

func TestSearch_IdentifierLaneFindsFullAndPrefixIDs(t *testing.T) {
	db := setupDB(t)
	now := time.Now().UTC()
	const (
		first  = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		second = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	)
	insNote(t, db, first, "first-note", "First", "seam", `[]`, now)
	insNote(t, db, second, "second-note", "Second", "seam", `[]`, now.Add(-time.Minute))

	svc := New(db, nil, budgets(), nil)
	hits, err := svc.Search(context.Background(), SearchInput{Query: strings.ToLower(first), Scope: "notes", Limit: 20})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, first, hits[0].ID)
	require.Equal(t, store.IdentifierMatchExactID, hits[0].IdentifierMatch)

	hits, err = svc.Search(context.Background(), SearchInput{Query: "01ARZ3ND", Scope: "notes", Limit: 20})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	for _, hit := range hits {
		require.Equal(t, store.IdentifierMatchIDPrefix, hit.IdentifierMatch)
	}

	hits, err = svc.Search(context.Background(), SearchInput{Query: "01ARZ3N", Scope: "notes", Limit: 20})
	require.NoError(t, err)
	require.Empty(t, hits, "short ID prefixes must not fan out")
}

// countingEmbedder counts its calls and returns a fixed vector.
type countingEmbedder struct {
	vec   []float32
	calls *int
}

func (e countingEmbedder) Model() string { return "test-embed" }

func (e countingEmbedder) Embed(context.Context, string) ([]float32, error) {
	*e.calls++
	return e.vec, nil
}

// insSearchNote is a note with its fts row, for the per-kind tests.
func insSearchNote(t *testing.T, db *sql.DB, id, slug, text, project string) {
	t.Helper()
	insNote(t, db, id, slug, text, project, "[]", time.Now())
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO fts (item_id, kind, project, title, name, description, body)
		VALUES (?, 'note', ?, ?, '', ?, ?)`, id, project, text, text, text)
	require.NoError(t, err)
}

func starSearchItem(t *testing.T, db *sql.DB, table, id string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `UPDATE `+table+` SET favorite = 1 WHERE id = ?`, id)
	require.NoError(t, err)
}

func hitIDs(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

func countKind(hits []Hit, kind string) int {
	n := 0
	for _, h := range hits {
		if h.Kind == kind {
			n++
		}
	}
	return n
}

// PerKind ranks and caps each knowledge kind on its own: many matching
// memories cannot squeeze the notes out, and each kind fills its own cap. The
// hits come grouped, memories first.
func TestSearch_PerKindCapsEachKindApart(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	for i := range 30 {
		insMem(t, db, fmt.Sprintf("01PKM%02d", i), "gotcha", fmt.Sprintf("kind-apart-%02d", i), "kind apart kind apart kind apart", "seam")
	}
	for i := range 3 {
		insSearchNote(t, db, fmt.Sprintf("01PKN%02d", i), fmt.Sprintf("kind-note-%02d", i), "one kind apart", "seam")
	}
	svc := New(db, nil, budgets(), nil)

	shared, err := svc.Search(ctx, SearchInput{Query: "kind apart", Limit: 21})
	require.NoError(t, err)
	require.Len(t, shared, 21, "the shared Limit is unchanged without PerKind")

	hits, err := svc.Search(ctx, SearchInput{Query: "kind apart", PerKind: map[string]int{"memory": 21, "note": 21}})
	require.NoError(t, err)
	require.Equal(t, 21, countKind(hits, "memory"))
	require.Equal(t, 3, countKind(hits, "note"), "every note, whatever the memories took")
	require.Equal(t, "memory", hits[0].Kind)
	require.Equal(t, "note", hits[len(hits)-1].Kind)

	hits, err = svc.Search(ctx, SearchInput{Query: "kind apart", PerKind: map[string]int{"memory": 200, "note": 21}})
	require.NoError(t, err)
	require.Equal(t, 30, countKind(hits, "memory"), "a cap past the old depth still finds every match")
}

// StarredOnly ranks the starred set on its own, so a starred match past the
// candidate depth is found; StarredFirst puts it ahead of every unstarred
// match. Neither moves a semantic-only starred item past the floor.
func TestSearch_StarredLaneReachesPastTheDepth(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	for i := range 80 {
		insMem(t, db, fmt.Sprintf("01STD%02d", i), "gotcha", fmt.Sprintf("deep-%02d", i), "deep lane deep lane", "seam")
	}
	// The weakest lexical match: one term once, after 80 stronger ones.
	insMem(t, db, "01STZ", "gotcha", "zz-starred", "lane", "seam")
	starSearchItem(t, db, "memories_index", "01STZ")
	// Starred, semantic-only and far: below the floor.
	insMem(t, db, "01STFAR", "gotcha", "far-starred", "nothing in common", "seam")
	starSearchItem(t, db, "memories_index", "01STFAR")
	require.NoError(t, store.UpsertEmbedding(ctx, db, "01STFAR", "memory", "test-embed", []float32{0.1, 0.99, 0}))
	// Starred, semantic-only and near: above it.
	insMem(t, db, "01STNEAR", "gotcha", "near-starred", "nothing in common either", "seam")
	starSearchItem(t, db, "memories_index", "01STNEAR")
	require.NoError(t, store.UpsertEmbedding(ctx, db, "01STNEAR", "memory", "test-embed", []float32{0.9, 0.1, 0}))

	lexical := New(db, nil, budgets(), nil)
	plain, err := lexical.Search(ctx, SearchInput{Query: "deep lane", PerKind: map[string]int{"memory": 21}})
	require.NoError(t, err)
	require.NotContains(t, hitIDs(plain), "01STZ", "without the starred rule the depth cuts it")

	only, err := lexical.Search(ctx, SearchInput{Query: "deep lane", PerKind: map[string]int{"memory": 21}, Starred: store.StarredOnly})
	require.NoError(t, err)
	require.Equal(t, []string{"01STZ"}, hitIDs(only))
	require.True(t, only[0].Favorite)
	require.NotEmpty(t, only[0].Snippet, "the starred lane quotes its match like the rest")

	first, err := lexical.Search(ctx, SearchInput{Query: "deep lane", PerKind: map[string]int{"memory": 21}, Starred: store.StarredFirst})
	require.NoError(t, err)
	require.Len(t, first, 21)
	require.Equal(t, "01STZ", first[0].ID)
	require.Equal(t, 1, strings.Count(strings.Join(hitIDs(first), " "), "01STZ"), "a starred item is never listed twice")

	semantic := New(db, fixedEmbedder{vec: []float32{1, 0, 0}}, budgets(), nil)
	semantic.SetSearchConfig(config.Search{SemanticFloor: 0.3})
	only, err = semantic.Search(ctx, SearchInput{Query: "deep lane", Semantic: true, PerKind: map[string]int{"memory": 21}, Starred: store.StarredOnly})
	require.NoError(t, err)
	ids := hitIDs(only)
	require.Contains(t, ids, "01STZ")
	require.Contains(t, ids, "01STNEAR", "a near starred neighbor clears the floor")
	require.NotContains(t, ids, "01STFAR", "the floor holds in the starred lane too")
	for _, h := range only {
		if h.ID == "01STNEAR" {
			require.InDelta(t, 0.99, h.Similarity, 0.01)
		}
	}
}

// One embedding per search, however many kinds and lanes rank it.
func TestSearch_PerKindEmbedsOnce(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	insMem(t, db, "01EMB", "gotcha", "embed-once", "embed once", "seam")
	starSearchItem(t, db, "memories_index", "01EMB")
	insSearchNote(t, db, "01EMN", "embed-note", "embed once", "seam")
	starSearchItem(t, db, "notes_index", "01EMN")

	calls := 0
	svc := New(db, countingEmbedder{vec: []float32{1, 0, 0}, calls: &calls}, budgets(), nil)
	_, err := svc.Search(ctx, SearchInput{Query: "embed once", Semantic: true,
		PerKind: map[string]int{"memory": 21, "note": 21}, Starred: store.StarredFirst})
	require.NoError(t, err)
	require.Equal(t, 1, calls)

	calls = 0
	_, err = svc.Search(ctx, SearchInput{Query: "embed once", Semantic: false,
		PerKind: map[string]int{"memory": 21, "note": 21}, Starred: store.StarredFirst})
	require.NoError(t, err)
	require.Zero(t, calls, "the palette's fast path never embeds")
}

// The starred lane keeps the degradation contract: with Starred only there is
// no other lane to run it, so the lane itself surfaces a local config error
// and degrades to lexical on a provider failure.
func TestSearch_StarredOnlyKeepsTheEmbedderContract(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	insMem(t, db, "01SEC", "gotcha", "contract-starred", "chroma health check", "seam")
	starSearchItem(t, db, "memories_index", "01SEC")
	in := SearchInput{Query: "chroma health check", Semantic: true, PerKind: map[string]int{"memory": 5}, Starred: store.StarredOnly}

	badReq := fmt.Errorf("llm.OpenAI.Embed: %w: new request: parse %q: missing protocol scheme", llm.ErrConfig, "://x")
	_, err := New(db, erroringEmbedder{err: badReq}, budgets(), slog.New(slog.DiscardHandler)).Search(ctx, in)
	require.ErrorIs(t, err, llm.ErrConfig)

	down := fmt.Errorf("llm.OpenAI.Embed: %w: dial tcp: connection refused", llm.ErrUnavailable)
	hits, err := New(db, erroringEmbedder{err: down}, budgets(), slog.New(slog.DiscardHandler)).Search(ctx, in)
	require.NoError(t, err)
	require.Equal(t, []string{"01SEC"}, hitIDs(hits))
	require.Equal(t, "fts", hits[0].Source)
}

// The starred rule needs per-kind ranking; asking for it on the shared-Limit
// ranking is refused rather than ignored.
func TestSearch_StarredNeedsPerKind(t *testing.T) {
	svc := New(setupDB(t), nil, budgets(), nil)
	_, err := svc.Search(context.Background(), SearchInput{Query: "anything", Starred: store.StarredOnly})
	require.ErrorContains(t, err, "Starred needs PerKind")
}
