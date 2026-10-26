package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

func newEmbedDB(t *testing.T) *sql.DB {
	t.Helper()
	return openTestDB(t)
}

func TestEncodeDecodeVector(t *testing.T) {
	vec := []float32{0, 1, -1, 0.5, 3.14159, -2.71828}
	got := DecodeVector(EncodeVector(vec))
	require.Equal(t, vec, got)

	require.Len(t, EncodeVector(vec), len(vec)*4)
	require.Nil(t, DecodeVector([]byte{1, 2, 3})) // not a multiple of 4
	require.Empty(t, DecodeVector(nil))
}

func TestCosine(t *testing.T) {
	require.InDelta(t, 1.0, Cosine([]float32{1, 0, 0}, []float32{1, 0, 0}), 1e-6)
	require.InDelta(t, 1.0, Cosine([]float32{1, 2, 3}, []float32{2, 4, 6}), 1e-6) // scale-invariant
	require.InDelta(t, 0.0, Cosine([]float32{1, 0}, []float32{0, 1}), 1e-6)       // orthogonal
	require.InDelta(t, -1.0, Cosine([]float32{1, 0}, []float32{-1, 0}), 1e-6)     // opposite
	require.Equal(t, 0.0, Cosine([]float32{0, 0}, []float32{1, 1}))               // zero magnitude
	require.Equal(t, 0.0, Cosine([]float32{1, 0, 0}, []float32{1, 0}))            // length mismatch
}

func TestUpsertAndCosineSearch(t *testing.T) {
	db := newEmbedDB(t)
	ctx := context.Background()

	require.NoError(t, UpsertEmbedding(ctx, db, "a", "memory", "m1", []float32{1, 0, 0}))
	require.NoError(t, UpsertEmbedding(ctx, db, "b", "memory", "m1", []float32{0.9, 0.1, 0}))
	require.NoError(t, UpsertEmbedding(ctx, db, "c", "note", "m1", []float32{0, 1, 0}))

	hits, err := CosineSearch(ctx, db, []float32{1, 0, 0}, "m1", nil, nil, 10)
	require.NoError(t, err)
	require.Len(t, hits, 3)
	require.Equal(t, "a", hits[0].ItemID) // identical
	require.Equal(t, "b", hits[1].ItemID) // close
	require.Equal(t, "c", hits[2].ItemID) // orthogonal
	require.InDelta(t, 1.0, hits[0].Score, 1e-6)
	require.InDelta(t, 0.0, hits[2].Score, 1e-6)

	// limit caps the result set.
	hits, err = CosineSearch(ctx, db, []float32{1, 0, 0}, "m1", nil, nil, 2)
	require.NoError(t, err)
	require.Len(t, hits, 2)

	// kind filter restricts the search space.
	hits, err = CosineSearch(ctx, db, []float32{1, 0, 0}, "m1", []string{"note"}, nil, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "c", hits[0].ItemID)
}

// A high-scoring old vector must be removed before the top-K window is built,
// leaving room for a weaker but in-window neighbor.
func TestCosineSearchSince_FiltersBeforeTopK(t *testing.T) {
	db := newEmbedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	insertMemory(t, db, "old", "gotcha", "old", "d", "seam", "b",
		core.FormatTime(now.Add(-48*time.Hour)), "")
	insertMemory(t, db, "new", "gotcha", "new", "d", "seam", "b",
		core.FormatTime(now.Add(-time.Hour)), "")
	require.NoError(t, UpsertEmbedding(ctx, db, "old", "memory", "m1", []float32{1, 0}))
	require.NoError(t, UpsertEmbedding(ctx, db, "new", "memory", "m1", []float32{0.8, 0.2}))

	all, err := CosineSearch(ctx, db, []float32{1, 0}, "m1", nil, nil, 1)
	require.NoError(t, err)
	require.Equal(t, "old", all[0].ItemID)

	hits, err := CosineSearchSince(ctx, db, []float32{1, 0}, "m1", nil, nil, "", now.Add(-24*time.Hour), 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "new", hits[0].ItemID)
}

// A vector stored under a different model is invisible to a search for m1.
func TestCosineSearchModelScope(t *testing.T) {
	db := newEmbedDB(t)
	ctx := context.Background()
	require.NoError(t, UpsertEmbedding(ctx, db, "a", "memory", "m1", []float32{1, 0}))
	require.NoError(t, UpsertEmbedding(ctx, db, "b", "memory", "m2", []float32{1, 0}))

	hits, err := CosineSearch(ctx, db, []float32{1, 0}, "m1", nil, nil, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "a", hits[0].ItemID)
}

// Rows whose dimensionality differs from the query (e.g. after a model swap
// under the same name) are skipped rather than returned with a bogus score.
func TestCosineSearchSkipsDimMismatch(t *testing.T) {
	db := newEmbedDB(t)
	ctx := context.Background()
	require.NoError(t, UpsertEmbedding(ctx, db, "a", "memory", "m1", []float32{1, 0, 0}))
	require.NoError(t, UpsertEmbedding(ctx, db, "b", "memory", "m1", []float32{1, 0})) // 2-dim

	hits, err := CosineSearch(ctx, db, []float32{1, 0, 0}, "m1", nil, nil, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "a", hits[0].ItemID)
}

// CosineSearchScoped resolves each item's project through the index tables and
// filters at query time, so out-of-scope vectors never occupy candidate slots.
func TestCosineSearchScoped_ProjectAndGlobal(t *testing.T) {
	db := newEmbedDB(t)
	ctx := context.Background()
	now := core.FormatTime(time.Now().UTC())

	insertMemory(t, db, "a", "gotcha", "seam-mem", "d", "seam", "b", now, "")
	insertMemory(t, db, "b", "gotcha", "other-mem", "d", "other", "b", now, "")
	insertMemory(t, db, "c", "reference", "global-mem", "d", "", "b", now, "")
	insertNote(t, db, "n", "Seam Note", "seam-note", "seam", now)

	require.NoError(t, UpsertEmbedding(ctx, db, "a", "memory", "m1", []float32{1, 0, 0}))
	require.NoError(t, UpsertEmbedding(ctx, db, "b", "memory", "m1", []float32{1, 0, 0}))
	require.NoError(t, UpsertEmbedding(ctx, db, "c", "memory", "m1", []float32{0.9, 0.1, 0}))
	require.NoError(t, UpsertEmbedding(ctx, db, "n", "note", "m1", []float32{0.8, 0.2, 0}))
	// An embedding orphaned from both index tables resolves to no project.
	require.NoError(t, UpsertEmbedding(ctx, db, "z", "memory", "m1", []float32{1, 0, 0}))

	q := []float32{1, 0, 0}

	// seam + global: the other-project row and the orphan are excluded.
	hits, err := CosineSearch(ctx, db, q, "m1", nil, []string{"", "seam"}, 10)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "c", "n"}, hitIDs(hits)) // best-first

	// Global-only scope sees only the global memory.
	hits, err = CosineSearch(ctx, db, q, "m1", nil, []string{""}, 10)
	require.NoError(t, err)
	require.Equal(t, []string{"c"}, hitIDs(hits))

	// Kind and project filters compose: notes in scope only.
	hits, err = CosineSearch(ctx, db, q, "m1", []string{"note"}, []string{"", "seam"}, 10)
	require.NoError(t, err)
	require.Equal(t, []string{"n"}, hitIDs(hits))

	// An empty projects filter keeps the unscoped full-scan behavior, orphan
	// included.
	hits, err = CosineSearch(ctx, db, q, "m1", nil, nil, 10)
	require.NoError(t, err)
	require.Len(t, hits, 5)
}

func TestUpsertReplaces(t *testing.T) {
	db := newEmbedDB(t)
	ctx := context.Background()

	require.NoError(t, UpsertEmbedding(ctx, db, "a", "memory", "m1", []float32{1, 0, 0}))
	require.NoError(t, UpsertEmbedding(ctx, db, "a", "memory", "m1", []float32{0, 1, 0})) // replace

	var dims int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT dims FROM embeddings WHERE item_id='a'`).Scan(&dims))
	require.Equal(t, 3, dims)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM embeddings`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestUpsertRejectsEmpty(t *testing.T) {
	db := newEmbedDB(t)
	ctx := context.Background()
	require.Error(t, UpsertEmbedding(ctx, db, "", "memory", "m1", []float32{1}))
	require.Error(t, UpsertEmbedding(ctx, db, "a", "memory", "m1", nil))
}

// TestCosineSearchMatchesNaiveReference pins the fused scan (RawBytes columns,
// running top-limit window, precomputed query norm) to the naive
// DecodeVector+Cosine+sort implementation it replaced: same hit set, same
// order, bit-identical scores. The corpus includes exact-duplicate vectors so
// score ties cross the window boundary and must resolve by item_id, and the
// limits exercise both the bounded window and the collect-and-sort fallback
// past cosineTopKMax.
func TestCosineSearchMatchesNaiveReference(t *testing.T) {
	db := newEmbedDB(t)
	ctx := context.Background()
	const dims = 32

	rng := rand.New(rand.NewPCG(42, 7))
	randVec := func() []float32 {
		v := make([]float32, dims)
		for i := range v {
			v[i] = float32(rng.Float64()*2 - 1)
		}
		return v
	}

	stored := make(map[string][]float32)
	kinds := make(map[string]string)
	for i := range 260 {
		id := fmt.Sprintf("it%03d", i)
		kind := "memory"
		if i%3 == 0 {
			kind = "note"
		}
		stored[id], kinds[id] = randVec(), kind
	}
	// Exact duplicates of the first 40 vectors under later-sorting ids, so tie
	// groups straddle every window boundary the limits below produce.
	for i := range 40 {
		id := fmt.Sprintf("zz%03d", i)
		stored[id], kinds[id] = stored[fmt.Sprintf("it%03d", i)], "memory"
	}
	for id, v := range stored {
		require.NoError(t, UpsertEmbedding(ctx, db, id, kinds[id], "m1", v))
	}
	// A dimensionality-mismatched row must be skipped by both implementations.
	require.NoError(t, UpsertEmbedding(ctx, db, "bad", "memory", "m1", []float32{1, 0}))

	query := randVec()

	naive := func(kindFilter []string, limit int) []SearchHit {
		var hits []SearchHit
		for id, v := range stored {
			if len(kindFilter) > 0 && !slices.Contains(kindFilter, kinds[id]) {
				continue
			}
			dec := DecodeVector(EncodeVector(v))
			hits = append(hits, SearchHit{ItemID: id, Kind: kinds[id], Score: Cosine(query, dec)})
		}
		sort.Slice(hits, func(i, j int) bool {
			if hits[i].Score != hits[j].Score {
				return hits[i].Score > hits[j].Score
			}
			return hits[i].ItemID < hits[j].ItemID
		})
		if len(hits) > limit {
			hits = hits[:limit]
		}
		return hits
	}

	for _, tc := range []struct {
		name  string
		kinds []string
		limit int
	}{
		{"window-smaller-than-corpus", nil, 10},
		{"recall-depth", []string{"memory", "note"}, 24},
		{"kind-filtered", []string{"note"}, 24},
		{"window-equals-corpus", nil, 300},
		{"fallback-truncated", nil, 257},
		{"fallback-all-rows", nil, 400},
		{"no-matches", []string{"nope"}, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CosineSearch(ctx, db, query, "m1", tc.kinds, nil, tc.limit)
			require.NoError(t, err)
			want := naive(tc.kinds, tc.limit)
			require.Equal(t, want, got)
		})
	}
}

func TestUnitVectorNormIsOne(t *testing.T) {
	// Guard the norm math against regressions.
	v := []float32{3, 4}
	require.InDelta(t, 5.0, math.Sqrt(float64(v[0]*v[0]+v[1]*v[1])), 1e-6)
	require.InDelta(t, 1.0, Cosine(v, v), 1e-6)
}

// GetEmbeddingStats groups stored vectors by (model, dims) and reports the
// embeddable corpus with its not-yet-embedded remainder. Invalidated memories
// keep their vectors (they count in the model groups) but are not owed one, so
// they never count as missing.
func TestGetEmbeddingStats(t *testing.T) {
	db := newEmbedDB(t)
	ctx := context.Background()
	now := core.FormatTime(time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC))

	insertMemory(t, db, "01A", "gotcha", "alpha", "d", "seam", "b", now, "")
	insertMemory(t, db, "01B", "gotcha", "beta", "d", "seam", "b", now, "")
	insertMemory(t, db, "01C", "gotcha", "gamma", "d", "seam", "b", now, now) // invalid, unembedded
	insertNoteTags(t, db, "01N", "seam", "[]", now)
	insertNoteTags(t, db, "01M", "seam", "[]", now) // unembedded

	require.NoError(t, UpsertEmbedding(ctx, db, "01A", "memory", "new-model", []float32{1, 0, 0}))
	require.NoError(t, UpsertEmbedding(ctx, db, "01B", "memory", "old-model", []float32{1, 0}))
	require.NoError(t, UpsertEmbedding(ctx, db, "01N", "note", "new-model", []float32{0, 1, 0}))

	s, err := GetEmbeddingStats(ctx, db)
	require.NoError(t, err)
	require.Equal(t, 3, s.Total)
	require.Equal(t, 2, s.ActiveMemories)
	require.Equal(t, 2, s.Notes)
	require.Equal(t, 1, s.Missing) // 01M only; the invalid 01C is not owed a vector

	require.Len(t, s.Models, 2)
	require.Equal(t, "new-model", s.Models[0].Model) // biggest group first
	require.Equal(t, 3, s.Models[0].Dims)
	require.Equal(t, 2, s.Models[0].Count)
	require.Equal(t, 1, s.Models[0].Memories)
	require.Equal(t, 1, s.Models[0].Notes)
	require.False(t, s.Models[0].Updated.IsZero())
	require.Equal(t, "old-model", s.Models[1].Model)
	require.Equal(t, 2, s.Models[1].Dims)
	require.Equal(t, 1, s.Models[1].Count)
}

// An empty database yields zeroes, not errors.
func TestGetEmbeddingStatsEmpty(t *testing.T) {
	db := newEmbedDB(t)
	s, err := GetEmbeddingStats(context.Background(), db)
	require.NoError(t, err)
	require.Zero(t, s.Total)
	require.Empty(t, s.Models)
	require.Zero(t, s.Missing)
}
