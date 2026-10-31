package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// SearchHit is one result of a cosine similarity search.
type SearchHit struct {
	ItemID string
	Kind   string
	Score  float64
}

// EncodeVector serializes a float32 vector to a little-endian byte slice, the
// on-disk form of the embeddings.vec BLOB column.
func EncodeVector(vec []float32) []byte {
	b := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(v))
	}
	return b
}

// DecodeVector reverses EncodeVector. A byte slice whose length is not a
// multiple of four yields nil (corrupt row; skip it).
func DecodeVector(b []byte) []float32 {
	if len(b)%4 != 0 {
		return nil
	}
	vec := make([]float32, len(b)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return vec
}

// Cosine returns the cosine similarity of two equal-length vectors, in [-1, 1].
// Mismatched lengths or a zero-magnitude vector yield 0.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// UpsertEmbedding stores (or replaces) the vector for an item. dims is recorded
// from the vector length so a later model change is detectable.
func UpsertEmbedding(ctx context.Context, db *sql.DB, itemID, kind, model string, vec []float32) error {
	if itemID == "" {
		return fmt.Errorf("store.UpsertEmbedding: empty item_id")
	}
	if len(vec) == 0 {
		return fmt.Errorf("store.UpsertEmbedding: empty vector for %s", itemID)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO embeddings (item_id, kind, model, dims, vec, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(item_id) DO UPDATE SET
		    kind=excluded.kind, model=excluded.model, dims=excluded.dims,
		    vec=excluded.vec, updated_at=excluded.updated_at`,
		itemID, kind, model, len(vec), EncodeVector(vec), core.FormatTime(time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("store.UpsertEmbedding: %w", err)
	}
	return nil
}

// CosineSearch brute-force-scans stored vectors for the given model and returns
// the top-limit most similar items, highest score first. An empty kinds filter
// searches all kinds. At the corpus scale this system targets (thousands of
// items) a full scan is milliseconds, which is why there is no vector index.
//
// projects restricts hits to items whose project is in the list (recall passes
// the bound project plus "" for global); an empty filter searches all projects.
// Filtering inside the candidate query keeps the whole candidate depth in scope,
// so a corpus dominated by out-of-scope vectors cannot starve in-scope results
// out of the top-limit window. Embedding rows carry no project, so the scope is
// resolved by joining the index tables; an embedding orphaned from both indexes
// matches no scope.
//
// Superseded and archived memories are excluded on the same grounds: a memory
// keeps its vector after it is invalidated, and a retired revision sits close to
// the replacement that superseded it, so filtering validity after the LIMIT let
// dead revisions eat the candidate depth. An embedding with no memories_index
// row (a note, or an orphan) has nothing to invalidate it and survives this
// predicate -- the scope filter is what drops orphans.
func CosineSearch(ctx context.Context, db *sql.DB, query []float32, model string, kinds, projects []string, limit int) ([]SearchHit, error) {
	return cosineSearch(ctx, db, query, model, kinds, projects, "", time.Time{}, limit)
}

// CosineSearchSince is CosineSearch restricted to knowledge updated at or
// after since. A zero since is unbounded. Filtering before the top-K scan keeps
// old neighbors from occupying the entire candidate window of a recent search.
// memKind, when non-empty, restricts hits to memories of that frontmatter kind
// (memories_index.kind); notes have no memories_index row, so a memKind filter
// excludes them regardless of the kinds (item-type) filter.
func CosineSearchSince(ctx context.Context, db *sql.DB, query []float32, model string, kinds, projects []string, memKind string, since time.Time, limit int) ([]SearchHit, error) {
	return cosineSearch(ctx, db, query, model, kinds, projects, memKind, since, limit)
}

func cosineSearch(ctx context.Context, db *sql.DB, query []float32, model string, kinds, projects []string, memKind string, since time.Time, limit int) ([]SearchHit, error) {
	if len(query) == 0 {
		return nil, fmt.Errorf("store.CosineSearch: empty query vector")
	}
	if limit <= 0 {
		limit = 10
	}

	// One query shape: the index joins resolve validity always and scope when a
	// project filter is given, so there is no branch that can search the corpus
	// without a validity predicate. Both are LEFT joins -- an embedding missing
	// from an index must be dropped by an explicit predicate, not by the join.
	sqlStr := `SELECT e.item_id, e.kind, e.vec FROM embeddings e
		LEFT JOIN memories_index mi ON mi.id = e.item_id
		LEFT JOIN notes_index ni ON ni.id = e.item_id
		WHERE e.model = ? AND (mi.id IS NULL OR mi.invalid_at IS NULL)`
	args := []any{model}
	if len(kinds) > 0 {
		sqlStr += ` AND e.kind IN (` + placeholders(len(kinds)) + `)`
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	if len(projects) > 0 {
		sqlStr += ` AND COALESCE(mi.project, ni.project) IN (` + placeholders(len(projects)) + `)`
		for _, p := range projects {
			args = append(args, p)
		}
	}
	// The caller's peer_auth reach, applied before the top-K scan like scope, so
	// a sealed project's vectors never compete for (or fill) the depth.
	if clause, extra := accessFilter(ctx, "COALESCE(mi.project, ni.project)"); clause != "" {
		sqlStr += clause
		args = append(args, extra...)
	}
	// A NULL mi.kind (a note, or an orphan) fails the predicate, so a memKind
	// filter is memories-only by construction, applied before the top-K scan
	// like every other scope predicate.
	if memKind != "" {
		sqlStr += ` AND mi.kind = ?`
		args = append(args, memKind)
	}
	if !since.IsZero() {
		sqlStr += ` AND COALESCE(mi.updated_at, ni.updated_at) >= ?`
		args = append(args, core.FormatTime(since.UTC()))
	}

	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("store.CosineSearch: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// The scan is allocation-hostile at corpus scale (thousands of ~6KB BLOBs
	// per query), so score each row without materializing it: columns land in
	// sql.RawBytes (database/sql reuses its buffers instead of cloning every
	// row), the dot product and row norm accumulate straight off the BLOB
	// bytes, and only rows that enter the running top-limit window allocate
	// their id/kind strings. The accumulation order matches Cosine exactly, so
	// scores are bit-identical to the DecodeVector+Cosine equivalent.
	var qq float64
	for _, v := range query {
		f := float64(v)
		qq += f * f
	}
	qnorm := math.Sqrt(qq)
	wantBytes := len(query) * 4

	// better reports whether (score, id) outranks h under the result order:
	// highest score first, ties broken by item_id for deterministic output.
	better := func(score float64, id sql.RawBytes, h SearchHit) bool {
		if score != h.Score {
			return score > h.Score
		}
		return string(id) < h.ItemID
	}

	// Insertion into a sorted window costs O(limit) per admitted row, which is
	// fine at recall's depths (<= 24) but not for an unbounded caller; past the
	// threshold, fall back to collect-and-sort.
	bounded := limit <= cosineTopKMax
	var (
		idRaw, kindRaw, blob sql.RawBytes
		hits                 []SearchHit
	)
	if bounded {
		hits = make([]SearchHit, 0, limit+1)
	}
	for rows.Next() {
		if err := rows.Scan(&idRaw, &kindRaw, &blob); err != nil {
			return nil, fmt.Errorf("store.CosineSearch: scan: %w", err)
		}
		if len(blob) != wantBytes {
			continue // corrupt row or a different model's dimensionality
		}
		var dot, nn float64
		for i, av := range query {
			bv := float64(math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:])))
			dot += float64(av) * bv
			nn += bv * bv
		}
		var score float64
		if qq != 0 && nn != 0 {
			score = dot / (qnorm * math.Sqrt(nn))
		}
		if !bounded {
			hits = append(hits, SearchHit{ItemID: string(idRaw), Kind: string(kindRaw), Score: score})
			continue
		}
		if len(hits) == limit && !better(score, idRaw, hits[limit-1]) {
			continue
		}
		idx := sort.Search(len(hits), func(i int) bool { return better(score, idRaw, hits[i]) })
		hits = append(hits, SearchHit{})
		copy(hits[idx+1:], hits[idx:])
		hits[idx] = SearchHit{ItemID: string(idRaw), Kind: string(kindRaw), Score: score}
		if len(hits) > limit {
			hits = hits[:limit]
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.CosineSearch: %w", err)
	}

	if !bounded {
		// Highest score first; ties broken by item_id for deterministic output.
		sort.Slice(hits, func(i, j int) bool {
			if hits[i].Score != hits[j].Score {
				return hits[i].Score > hits[j].Score
			}
			return hits[i].ItemID < hits[j].ItemID
		})
		if len(hits) > limit {
			hits = hits[:limit]
		}
	}
	if len(hits) == 0 {
		return nil, nil
	}
	return hits, nil
}

// cosineTopKMax bounds the sorted-window insertion path in cosineSearch; a
// larger limit falls back to collecting every row and sorting once.
const cosineTopKMax = 256

// EmbeddingModelStat is one (model, dims) group of stored vectors: how many
// items it covers, split by kind, and when a vector in it was last written.
type EmbeddingModelStat struct {
	Model    string    `json:"model"`
	Dims     int       `json:"dims"`
	Count    int       `json:"count"`
	Memories int       `json:"memories"`
	Notes    int       `json:"notes"`
	Updated  time.Time `json:"updated"`
}

// EmbeddingStats summarizes the vector index for the console's Settings page:
// what is stored (grouped by model), how big the embeddable corpus is, and how
// much of it has no vector at all. Missing counts active memories and notes
// without an embeddings row; invalidated memories keep their vectors but are
// not owed one, so they never count as missing.
type EmbeddingStats struct {
	Total          int                  `json:"total"`
	Models         []EmbeddingModelStat `json:"models"`
	ActiveMemories int                  `json:"activeMemories"`
	Notes          int                  `json:"notes"`
	Missing        int                  `json:"missing"`
}

// GetEmbeddingStats reads the vector-index summary. It is a set of plain
// aggregate queries -- no BLOB is materialized.
func GetEmbeddingStats(ctx context.Context, db *sql.DB) (EmbeddingStats, error) {
	var s EmbeddingStats
	rows, err := db.QueryContext(ctx, `
		SELECT model, dims, COUNT(*),
		       SUM(CASE WHEN kind = 'memory' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN kind = 'note' THEN 1 ELSE 0 END),
		       MAX(updated_at)
		FROM embeddings
		GROUP BY model, dims
		ORDER BY COUNT(*) DESC, model, dims`)
	if err != nil {
		return EmbeddingStats{}, fmt.Errorf("store.GetEmbeddingStats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var m EmbeddingModelStat
		var updated string
		if err := rows.Scan(&m.Model, &m.Dims, &m.Count, &m.Memories, &m.Notes, &updated); err != nil {
			return EmbeddingStats{}, fmt.Errorf("store.GetEmbeddingStats: scan: %w", err)
		}
		if t, terr := core.ParseTime(updated); terr == nil {
			m.Updated = t
		}
		s.Total += m.Count
		s.Models = append(s.Models, m)
	}
	if err := rows.Err(); err != nil {
		return EmbeddingStats{}, fmt.Errorf("store.GetEmbeddingStats: %w", err)
	}

	err = db.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM memories_index WHERE invalid_at IS NULL),
		  (SELECT COUNT(*) FROM notes_index),
		  (SELECT COUNT(*) FROM memories_index mi WHERE mi.invalid_at IS NULL
		     AND NOT EXISTS (SELECT 1 FROM embeddings e WHERE e.item_id = mi.id))
		+ (SELECT COUNT(*) FROM notes_index ni
		     WHERE NOT EXISTS (SELECT 1 FROM embeddings e WHERE e.item_id = ni.id))`).
		Scan(&s.ActiveMemories, &s.Notes, &s.Missing)
	if err != nil {
		return EmbeddingStats{}, fmt.Errorf("store.GetEmbeddingStats: corpus: %w", err)
	}
	return s, nil
}

// placeholders returns "?, ?, ..." with n placeholders for an IN clause.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
