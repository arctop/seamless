package store

// Hot-path baseline benchmarks for the store's search primitives: brute-force
// cosine over embedding BLOBs, FTS5 MATCH, and the vector encode/decode they
// lean on. Fixtures are synthetic and deterministic (pseudo-random float32
// vectors, a fixed vocabulary); no LLM or network is involved. Run with
// `make bench`.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

const (
	// benchDims matches OpenAI text-embedding-3-small, the default production
	// embedding size, so the cosine scan cost is realistic.
	benchDims  = 1536
	benchModel = "bench-embed"
)

// benchWords is a small vocabulary used to synthesize names and descriptions
// with a realistic token-overlap distribution for FTS. The words at indices
// 0, 13, 26, 39, and 52 appear together in benchmark queries so a predictable
// subset of the corpus matches (see benchDescription).
var benchWords = []string{
	"chroma", "sqlite", "embedding", "briefing", "session", "memory",
	"gardener", "recall", "fusion", "cosine", "vector", "index", "token",
	"container", "daemon", "hook", "prompt", "matcher", "budget", "ulid",
	"migration", "schema", "watcher", "frontmatter", "markdown", "atomic",
	"health", "lease", "claim", "queue", "task", "trial", "event", "fanout",
	"subscriber", "console", "template", "bearer", "localhost", "check",
	"project", "scope", "global", "sibling", "parent", "family", "rollup",
	"plan", "capture", "fetch", "retry", "backoff", "race", "startup", "boot",
	"readiness", "gate", "telemetry", "funnel", "injected", "retrieval",
	"stats", "reaper", "idle", "cursor",
}

// benchDescription builds a deterministic 8-word description for item i. The
// stride walks the vocabulary so different items share some tokens (posting
// lists have realistic depth) without every item matching every query.
func benchDescription(i int) string {
	words := make([]string, 8)
	for k := range words {
		words[k] = benchWords[(i*7+k*13)%len(benchWords)]
	}
	return strings.Join(words, " ")
}

// benchVector returns a deterministic pseudo-random vector for seed, in the
// same value range as real embeddings. No LLM is involved.
func benchVector(seed uint64, dims int) []float32 {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	v := make([]float32, dims)
	for i := range v {
		v[i] = float32(rng.Float64()*2 - 1)
	}
	return v
}

func openBenchStore(b *testing.B) *sql.DB {
	b.Helper()
	return openTestDB(b)
}

// seedSearchCorpus inserts n synthetic memories -- an index row, an fts row,
// and an embedding row each -- in a single transaction so fixture setup stays
// fast at 5k items.
func seedSearchCorpus(b *testing.B, db *sql.DB, project string, n, dims int) {
	b.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(b, err)

	insMem, err := tx.PrepareContext(ctx, `
		INSERT INTO memories_index
		    (id, kind, name, description, project, file_path, tags, valid_from,
		     invalid_at, superseded_by, source_session, content_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '[]', ?, NULL, NULL, '', 'h', ?, ?)`)
	require.NoError(b, err)
	defer func() { _ = insMem.Close() }()
	insFTS, err := tx.PrepareContext(ctx, `
		INSERT INTO fts (item_id, kind, project, title, name, description, body)
		VALUES (?, 'memory', ?, '', ?, ?, ?)`)
	require.NoError(b, err)
	defer func() { _ = insFTS.Close() }()
	insEmb, err := tx.PrepareContext(ctx, `
		INSERT INTO embeddings (item_id, kind, model, dims, vec, updated_at)
		VALUES (?, 'memory', ?, ?, ?, ?)`)
	require.NoError(b, err)
	defer func() { _ = insEmb.Close() }()

	stamp := core.FormatTime(time.Now().UTC())
	for i := range n {
		id := fmt.Sprintf("M%025d", i)
		name := fmt.Sprintf("bench-mem-%d", i)
		desc := benchDescription(i)
		_, err = insMem.ExecContext(ctx, id, "gotcha", name, desc, project,
			"memory/bench/"+name+".md", stamp, stamp, stamp)
		require.NoError(b, err)
		_, err = insFTS.ExecContext(ctx, id, project, name, desc, desc+" "+benchDescription(i+1))
		require.NoError(b, err)
		_, err = insEmb.ExecContext(ctx, id, benchModel, dims,
			EncodeVector(benchVector(uint64(i+1), dims)), stamp)
		require.NoError(b, err)
	}
	require.NoError(b, tx.Commit())
}

// BenchmarkCosineSearch measures the brute-force scan recall leans on: read
// every embedding row for the model, decode the BLOB, cosine against the query,
// sort, cut to the source depth recall uses (24).
func BenchmarkCosineSearch(b *testing.B) {
	ctx := context.Background()
	for _, n := range []int{1000, 5000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			db := openBenchStore(b)
			seedSearchCorpus(b, db, "bench", n, benchDims)
			query := benchVector(999_999, benchDims)
			b.ReportAllocs()
			for b.Loop() {
				hits, err := CosineSearch(ctx, db, query, benchModel, []string{"memory", "note"}, nil, 24)
				if err != nil {
					b.Fatal(err)
				}
				if len(hits) == 0 {
					b.Fatal("expected hits")
				}
			}
		})
	}
}

// BenchmarkFTSSearch measures the FTS5 MATCH leg of recall at corpus scale,
// with the same multi-term query shape agents send.
func BenchmarkFTSSearch(b *testing.B) {
	ctx := context.Background()
	db := openBenchStore(b)
	seedSearchCorpus(b, db, "bench", 5000, benchDims)
	b.ReportAllocs()
	for b.Loop() {
		hits, err := FTSSearch(ctx, db, "chroma container health check race", []string{"memory", "note"}, nil, 24)
		if err != nil {
			b.Fatal(err)
		}
		if len(hits) == 0 {
			b.Fatal("expected hits")
		}
	}
}

// supersedeFraction invalidates the first frac of an n-item seeded corpus the
// way lifecycle.Supersede does -- stamping memories_index.invalid_at and leaving
// the fts and embedding rows untouched -- so the fixture holds the retired
// revisions a long-lived corpus actually accumulates.
func supersedeFraction(b *testing.B, db *sql.DB, n int, frac float64) int {
	b.Helper()
	dead := int(float64(n) * frac)
	res, err := db.ExecContext(context.Background(), `
		UPDATE memories_index SET invalid_at = ?
		WHERE id <= ?`, core.FormatTime(time.Now().UTC()), fmt.Sprintf("M%025d", dead-1))
	require.NoError(b, err)
	got, err := res.RowsAffected()
	require.NoError(b, err)
	require.Equal(b, int64(dead), got, "fixture must invalidate exactly the intended rows")
	return dead
}

// BenchmarkSearchSupersededCorpus measures both candidate queries against the
// corpus shape they actually run on: one where a fifth of the memories have been
// superseded but keep their fts and embedding rows. seedSearchCorpus stamps
// every row valid, so the plain benchmarks above measure the one case where the
// validity predicate is pure overhead and can never pay for itself. Here the
// predicate also removes a fifth of the rows from the scan, which is the
// trade the production DB makes.
func BenchmarkSearchSupersededCorpus(b *testing.B) {
	ctx := context.Background()
	const n = 5000

	b.Run("cosine", func(b *testing.B) {
		db := openBenchStore(b)
		seedSearchCorpus(b, db, "bench", n, benchDims)
		supersedeFraction(b, db, n, 0.2)
		query := benchVector(999_999, benchDims)
		b.ReportAllocs()
		for b.Loop() {
			hits, err := CosineSearch(ctx, db, query, benchModel, []string{"memory", "note"}, []string{"bench"}, 24)
			if err != nil {
				b.Fatal(err)
			}
			if len(hits) == 0 {
				b.Fatal("expected hits")
			}
		}
	})

	b.Run("fts", func(b *testing.B) {
		db := openBenchStore(b)
		seedSearchCorpus(b, db, "bench", n, benchDims)
		supersedeFraction(b, db, n, 0.2)
		b.ReportAllocs()
		for b.Loop() {
			hits, err := FTSSearch(ctx, db, "chroma container health check race", []string{"memory", "note"}, []string{"bench"}, 24)
			if err != nil {
				b.Fatal(err)
			}
			if len(hits) == 0 {
				b.Fatal("expected hits")
			}
		}
	})
}

// BenchmarkCosine isolates the pure vector math (one similarity computation at
// production dimensionality), the inner loop of the brute-force scan.
func BenchmarkCosine(b *testing.B) {
	x := benchVector(1, benchDims)
	y := benchVector(2, benchDims)
	var acc float64
	b.ReportAllocs()
	for b.Loop() {
		acc += Cosine(x, y)
	}
	if math.IsNaN(acc) {
		b.Fatal("cosine produced NaN")
	}
}

// BenchmarkDecodeVector isolates BLOB-to-float32 decoding, paid once per stored
// row per cosine scan.
func BenchmarkDecodeVector(b *testing.B) {
	blob := EncodeVector(benchVector(7, benchDims))
	b.ReportAllocs()
	for b.Loop() {
		if v := DecodeVector(blob); len(v) != benchDims {
			b.Fatal("bad decode")
		}
	}
}
