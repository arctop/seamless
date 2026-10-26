package retrieve

// Hot-path baseline benchmarks for the retrieval surfaces agents hit on every
// session: recall (FTS5 + brute-force cosine + RRF fusion, end to end),
// briefing assembly (SessionStart), and the prompt-context matcher
// (UserPromptSubmit). Fixtures are synthetic and deterministic; embeddings are
// pseudo-random float32 vectors from a local fake embedder -- no LLM calls, so
// the hybrid recall numbers exclude real embedding-API latency. Run with
// `make bench`.

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const (
	// benchDims matches OpenAI text-embedding-3-small, the default production
	// embedding size.
	benchDims  = 1536
	benchModel = "bench-embed"

	benchProject = "bench"
	benchCWD     = "/bench/repo"
	benchQuery   = "chroma container health check race"
	benchPrompt  = "why does the chroma container fail its health check race"
)

// benchWords is a small vocabulary used to synthesize memory names and
// descriptions with a realistic token-overlap distribution. The words at
// indices 0, 13, 26, 39, and 52 appear together in the benchmark query and
// prompt so a predictable subset of the corpus matches (see benchDescription).
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

// benchDescription builds a deterministic 8-word description for item i.
func benchDescription(i int) string {
	words := make([]string, 8)
	for k := range words {
		words[k] = benchWords[(i*7+k*13)%len(benchWords)]
	}
	return strings.Join(words, " ")
}

// benchVector returns a deterministic pseudo-random vector for seed.
func benchVector(seed uint64, dims int) []float32 {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	v := make([]float32, dims)
	for i := range v {
		v[i] = float32(rng.Float64()*2 - 1)
	}
	return v
}

// benchEmbedder derives a deterministic pseudo-random vector from the text's
// hash: same text, same vector, zero network. It stands in for the embedding
// API on the recall path, so recall numbers exclude provider latency.
type benchEmbedder struct{ dims int }

func (e benchEmbedder) Model() string { return benchModel }

func (e benchEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	return benchVector(h.Sum64(), e.dims), nil
}

func setupBenchDB(b *testing.B) *sql.DB {
	b.Helper()
	db, err := storetest.OpenFresh(b, filepath.Join(b.TempDir(), "seam.db"))
	require.NoError(b, err)
	b.Cleanup(func() { _ = db.Close() })
	return db
}

func benchBudgets() config.Budgets {
	return config.Budgets{MaxBriefingTokens: 1500, RecallBudgetTokens: 1000}
}

// seedBenchCorpus inserts n synthetic active memories (index + fts rows, and an
// embedding row each when withEmbeddings is set) in one transaction. The first
// three items are constraints so the briefing exercises its never-drop path.
func seedBenchCorpus(b *testing.B, db *sql.DB, project string, n int, withEmbeddings bool) {
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
		kind := "gotcha"
		if i < 3 {
			kind = "constraint"
		}
		desc := benchDescription(i)
		_, err = insMem.ExecContext(ctx, id, kind, name, desc, project,
			"memory/bench/"+name+".md", stamp, stamp, stamp)
		require.NoError(b, err)
		_, err = insFTS.ExecContext(ctx, id, project, name, desc, desc+" "+benchDescription(i+1))
		require.NoError(b, err)
		if withEmbeddings {
			_, err = insEmb.ExecContext(ctx, id, benchModel, benchDims,
				store.EncodeVector(benchVector(uint64(i+1), benchDims)), stamp)
			require.NoError(b, err)
		}
	}
	require.NoError(b, tx.Commit())
}

// seedBenchSessionsAndTasks adds recent completed-session findings and open
// tasks so the briefing renders its findings and ready-tasks sections.
func seedBenchSessionsAndTasks(b *testing.B, db *sql.DB, project string) {
	b.Helper()
	ctx := context.Background()
	now := time.Now()
	for i := range 10 {
		ts := now.Add(-time.Duration(i) * time.Hour)
		require.NoError(b, store.CreateSession(ctx, db, core.Session{
			ID:   fmt.Sprintf("S%025d", i),
			Name: fmt.Sprintf("cc/bench-%d", i), ProjectSlug: project,
			Status:    core.SessionCompleted,
			Findings:  "finding " + benchDescription(i),
			CreatedAt: ts, UpdatedAt: ts,
		}))
	}
	for i := range 5 {
		require.NoError(b, store.CreateTask(ctx, db, core.Task{
			ID:          fmt.Sprintf("T%025d", i),
			ProjectSlug: project,
			Title:       fmt.Sprintf("bench task %d", i),
			Status:      core.TaskOpen,
			CreatedAt:   now, UpdatedAt: now,
		}))
	}
}

// BenchmarkRecall measures the recall tool end to end at corpus scale:
// embed the query (local fake), brute-force cosine over 5k embedding BLOBs,
// FTS5 MATCH, RRF fusion, hydration, scope filter, and token-budget packing.
// fts-only is the degraded path with no embedder configured.
func BenchmarkRecall(b *testing.B) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		hybrid bool
	}{
		{"hybrid", true},
		{"fts-only", false},
	} {
		b.Run(tc.name+"/n=5000", func(b *testing.B) {
			db := setupBenchDB(b)
			seedBenchCorpus(b, db, benchProject, 5000, true)
			var emb llm.Embedder
			if tc.hybrid {
				emb = benchEmbedder{dims: benchDims}
			}
			svc := New(db, emb, benchBudgets(), nil)
			b.ReportAllocs()
			for b.Loop() {
				hits, err := svc.Recall(ctx, RecallInput{
					Query: benchQuery, Project: benchProject, Limit: 10,
				})
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

// BenchmarkBriefing measures SessionStart briefing assembly end to end:
// cwd resolution, the settings-override read, the active-memory scope load,
// kind partitioning, findings/tasks/plans reads, and budget packing.
// memories=200 is a typical mature project; memories=5000 is the stress shape
// (the scope load dominates there).
func BenchmarkBriefing(b *testing.B) {
	ctx := context.Background()
	for _, n := range []int{200, 5000} {
		b.Run(fmt.Sprintf("memories=%d", n), func(b *testing.B) {
			db := setupBenchDB(b)
			require.NoError(b, store.SetSetting(ctx, db, store.SettingRepoProjectMap,
				fmt.Sprintf(`{%q:%q}`, benchCWD, benchProject)))
			seedBenchCorpus(b, db, benchProject, n, false)
			seedBenchSessionsAndTasks(b, db, benchProject)
			svc := New(db, nil, benchBudgets(), nil)
			b.ReportAllocs()
			for b.Loop() {
				text, _, err := svc.Briefing(ctx, BriefingInput{CWD: benchCWD, Source: "startup"})
				if err != nil {
					b.Fatal(err)
				}
				if text == "" {
					b.Fatal("expected a briefing")
				}
			}
		})
	}
}

// BenchmarkPromptRecall measures the prompt-context matcher that fires on every
// UserPromptSubmit. warm is the steady state (IDF corpus cached, within its 30s
// TTL): tokenize the prompt and score it against every candidate. cold forces
// the corpus rebuild each iteration (store load + tokenize + IDF over all
// active memories) -- the cost paid once per project per TTL window.
func BenchmarkPromptRecall(b *testing.B) {
	ctx := context.Background()
	for _, n := range []int{200, 5000} {
		db := setupBenchDB(b)
		require.NoError(b, store.SetSetting(ctx, db, store.SettingRepoProjectMap,
			fmt.Sprintf(`{%q:%q}`, benchCWD, benchProject)))
		seedBenchCorpus(b, db, benchProject, n, false)
		svc := New(db, nil, benchBudgets(), nil)

		b.Run(fmt.Sprintf("warm/memories=%d", n), func(b *testing.B) {
			out, _, err := svc.PromptRecall(ctx, "", benchCWD, benchPrompt) // prime the corpus cache
			require.NoError(b, err)
			require.NotEmpty(b, out, "fixture must produce prompt-recall hits")
			b.ReportAllocs()
			for b.Loop() {
				if _, _, err := svc.PromptRecall(ctx, "", benchCWD, benchPrompt); err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(fmt.Sprintf("cold/memories=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				svc.corpus.mu.Lock()
				delete(svc.corpus.entries, benchProject)
				svc.corpus.mu.Unlock()
				if _, _, err := svc.PromptRecall(ctx, "", benchCWD, benchPrompt); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
