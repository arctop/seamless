package files

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// TestEmbedderSemanticSearch is an opt-in end-to-end check that the files + llm +
// store pipeline returns semantically sane cosine results with a real embedder.
// It uses the configured provider (config.Load, honoring seamless.yaml via
// SEAMLESS_CONFIG), so it exercises whatever provider is set — OpenAI or Ollama.
// Skipped unless SEAMLESS_EMBED_TEST=1 (so `make test` stays hermetic). Run with:
//
//	SEAMLESS_EMBED_TEST=1 SEAMLESS_CONFIG=$PWD/seamless.yaml \
//	  go test ./internal/files/ -run TestEmbedderSemanticSearch -v
func TestEmbedderSemanticSearch(t *testing.T) {
	if os.Getenv("SEAMLESS_EMBED_TEST") == "" {
		t.Skip("set SEAMLESS_EMBED_TEST=1 to run the live embedding test")
	}

	cfg, err := config.Load()
	require.NoError(t, err)
	embedder, err := llm.NewEmbedder(cfg.LLM)
	require.NoError(t, err)
	t.Logf("embedding provider=%s model=%s", cfg.LLM.Provider, embedder.Model())

	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	m, err := NewManager(dir, db, nil)
	require.NoError(t, err)
	m.SetEmbedder(embedder)
	ctx := context.Background()

	mems := []struct{ name, desc, body string }{
		{"chroma-boot-race", "the chroma vector-database container answers its health check before it can serve queries, causing boot failures",
			"Docker starts the sidecar and the daemon together; add a readiness gate so the daemon waits for chroma to actually serve.\n"},
		{"ulid-over-uuid", "use ULID identifiers everywhere instead of UUID so ids sort lexicographically by creation time",
			"ULIDs embed a millisecond timestamp prefix; never use UUID v4 for primary keys here.\n"},
		{"sqlite-wal-mode", "enable WAL journal mode and a busy_timeout on every sqlite connection",
			"Set journal_mode=WAL and foreign_keys=on via the DSN so every pooled connection inherits them.\n"},
	}
	for _, mm := range mems {
		id, err := core.NewID()
		require.NoError(t, err)
		_, err = m.WriteMemory(ctx, core.Memory{
			ID: id, Kind: core.KindGotcha, Name: mm.name, Description: mm.desc,
			Project: "seam", Body: mm.body,
			Created: time.Now().UTC(), Updated: time.Now().UTC(), ValidFrom: time.Now().UTC(),
		})
		require.NoError(t, err)
	}

	// A query about a container failing to start should rank the boot-race memory first.
	qvec, err := embedder.Embed(ctx, "the database sidecar container fails to start up in time")
	require.NoError(t, err)
	hits, err := store.CosineSearch(ctx, db, qvec, embedder.Model(), nil, nil, 3)
	require.NoError(t, err)
	require.NotEmpty(t, hits)

	var topName string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT name FROM memories_index WHERE id = ?`, hits[0].ItemID).Scan(&topName))
	require.Equal(t, "chroma-boot-race", topName, "boot-race memory should be the closest match; ranking=%v", hits)
	t.Logf("ranking: %+v", hits)
}

// TestQueryExistingDB runs a cosine query against an already-populated seam.db
// (e.g. a validation import), to confirm semantic ranking on real data. Opt-in:
//
//	SEAMLESS_EMBED_TEST=1 SEAMLESS_CONFIG=$PWD/seamless.yaml \
//	  SEAMLESS_QUERY_DB=/path/seam.db SEAMLESS_QUERY="back up the database" \
//	  go test ./internal/files/ -run TestQueryExistingDB -v
func TestQueryExistingDB(t *testing.T) {
	if os.Getenv("SEAMLESS_EMBED_TEST") == "" || os.Getenv("SEAMLESS_QUERY_DB") == "" {
		t.Skip("set SEAMLESS_EMBED_TEST=1 and SEAMLESS_QUERY_DB to run")
	}
	cfg, err := config.Load()
	require.NoError(t, err)
	embedder, err := llm.NewEmbedder(cfg.LLM)
	require.NoError(t, err)

	db, err := sql.Open("sqlite", "file:"+os.Getenv("SEAMLESS_QUERY_DB")+"?mode=ro")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	query := os.Getenv("SEAMLESS_QUERY")
	qvec, err := embedder.Embed(context.Background(), query)
	require.NoError(t, err)
	hits, err := store.CosineSearch(context.Background(), db, qvec, embedder.Model(), nil, nil, 5)
	require.NoError(t, err)

	t.Logf("query: %q", query)
	for i, h := range hits {
		var name string
		_ = db.QueryRowContext(context.Background(),
			`SELECT COALESCE((SELECT name FROM memories_index WHERE id=?),
			                 (SELECT title FROM notes_index WHERE id=?))`, h.ItemID, h.ItemID).Scan(&name)
		t.Logf("  #%d  %.4f  %s", i+1, h.Score, name)
	}
	require.NotEmpty(t, hits)
}
