package console

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// fixedEmbedder returns the same vector for every text (retrieve's tests keep a
// twin); it lets a console test drive the semantic leg without a provider.
type fixedEmbedder struct{ vec []float32 }

func (e fixedEmbedder) Model() string { return "test-embed" }

func (e fixedEmbedder) Embed(context.Context, string) ([]float32, error) { return e.vec, nil }

// newSemanticConsole is newConsole with an embedder, for the tests that need
// the semantic leg live.
func newSemanticConsole(t *testing.T, vec []float32) (*sql.DB, *http.ServeMux) {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	svc, err := New(Config{
		DB: db, Events: events.NewRecorder(db), APIKey: testKey,
		Retrieve: retrieve.New(db, fixedEmbedder{vec: vec}, config.Defaults().Budgets, nil),
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	svc.Register(mux)
	return db, mux
}

// seedSearchMemory inserts a memory index row plus its fts row.
func seedSearchMemory(t *testing.T, db *sql.DB, id, name, desc, project, body string) {
	t.Helper()
	seedSearchMemoryAt(t, db, id, name, desc, project, body, time.Now().UTC())
}

func seedSearchMemoryAt(t *testing.T, db *sql.DB, id, name, desc, project, body string, updated time.Time) {
	t.Helper()
	ctx := context.Background()
	stamp := core.FormatTime(updated.UTC())
	_, err := db.ExecContext(ctx, `
		INSERT INTO memories_index
		    (id, kind, name, description, project, file_path, tags, valid_from,
		     invalid_at, superseded_by, source_session, content_hash, created_at, updated_at)
		VALUES (?, 'gotcha', ?, ?, ?, ?, '[]', ?, NULL, NULL, '', 'h', ?, ?)`,
		id, name, desc, project, "memory/x/"+name+".md", stamp, stamp, stamp)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO fts (item_id, kind, project, title, name, description, body)
		VALUES (?, 'memory', ?, '', ?, ?, ?)`, id, project, name, desc, body)
	require.NoError(t, err)
}

func seedSearchNote(t *testing.T, db *sql.DB, id, slug, title, desc, project, body string, updated time.Time) {
	t.Helper()
	stamp := core.FormatTime(updated.UTC())
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO notes_index
		    (id, title, slug, description, project, file_path, tags, source_url,
		     content_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '[]', NULL, 'h', ?, ?)`,
		id, title, slug, desc, project, "notes/x/"+slug+".md", stamp, stamp)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO fts (item_id, kind, project, title, name, description, body)
		VALUES (?, 'note', ?, ?, '', ?, ?)`, id, project, title, desc, body)
	require.NoError(t, err)
}

func seedSearchTask(t *testing.T, db *sql.DB, id, project, title, planSlug string) {
	t.Helper()
	seedSearchTaskAt(t, db, id, project, title, planSlug, time.Now().UTC())
}

func seedSearchTaskAt(t *testing.T, db *sql.DB, id, project, title, planSlug string, updated time.Time) {
	t.Helper()
	stamp := core.FormatTime(updated.UTC())
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO tasks (id, project_slug, title, body, status, created_by,
		    plan_slug, claimed_by, lease_expires_at, created_at, updated_at, closed_at)
		VALUES (?, ?, ?, '', 'open', 'test', ?, '', NULL, ?, ?, NULL)`,
		id, project, title, planSlug, stamp, stamp)
	require.NoError(t, err)
}

// getSearch issues an authenticated HTML GET and returns the recorder.
func getSearch(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	return do(mux, req)
}

func TestSearch_UnauthenticatedRedirectsToLogin(t *testing.T) {
	mux := newTestMux(t)
	rr := do(mux, httptest.NewRequest(http.MethodGet, "/console/search?q=chroma", nil))
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "/console/login")
}

func TestSearch_EmptyQueryRendersEmptyState(t *testing.T) {
	mux := newTestMux(t)
	rr := getSearch(t, mux, "/console/search")
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	require.Contains(t, body, "Search memories, notes, tasks")
	require.Contains(t, body, "<kbd>Ctrl</kbd>+<kbd>K</kbd>")
	require.Contains(t, body, "<kbd>Cmd</kbd>+<kbd>K</kbd>")
}

// A strictly validated enum must name its valid values, so an agent driving the
// console by URL sees the fix rather than a silent default.
func TestSearch_BadScopeIsRejected(t *testing.T) {
	mux := newTestMux(t)
	rr := getSearch(t, mux, "/console/search?q=chroma&scope=bogus")
	require.Equal(t, http.StatusBadRequest, rr.Code)
	body := rr.Body.String()
	require.Contains(t, body, "invalid scope")
	require.Contains(t, body, "memories")
	require.Contains(t, body, "sessions")
}

func TestSearch_BadFastIsRejected(t *testing.T) {
	mux := newTestMux(t)
	rr := getSearch(t, mux, "/console/search?q=chroma&fast=yes")
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), "invalid fast")
}

func TestSearch_InvalidOrAmbiguousControlsAreRejected(t *testing.T) {
	mux := newTestMux(t)
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"empty scope", "/console/search?q=chroma&scope=", "invalid scope"},
		{"bad window", "/console/search?q=chroma&w=yesterday", "invalid w"},
		{"bad sort", "/console/search?q=chroma&sort=popular", "invalid sort"},
		{"empty fast", "/console/search?q=chroma&fast=", "invalid fast"},
		{"misspelled parameter", "/console/search?q=chroma&srot=newest", "invalid parameter"},
		{"duplicate sort", "/console/search?q=chroma&sort=newest&sort=oldest", "exactly once"},
		{"bad format", "/console/search?q=chroma&format=xml", "invalid format"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := getSearch(t, mux, tc.path)
			require.Equal(t, http.StatusBadRequest, rr.Code)
			require.Contains(t, rr.Body.String(), tc.want)
		})
	}
}

func TestSearch_JSONGroupsAcrossEntities(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()

	seedSearchMemory(t, db, "01MEM", "chroma-boot-race", "chroma container health check", "seam", "chroma boot race body")
	seedSearchTask(t, db, "01TSK", "seam", "fix the chroma boot race", "")
	seedSearchTask(t, db, "01PLN", "seam", "plan step", "chroma-cleanup")
	require.NoError(t, store.CreateProject(ctx, db, core.Project{
		ID: "01PRJ", Slug: "chroma-tools", Name: "Chroma Tools", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: "01SES", Name: "cc/chroma-debug", ProjectSlug: "seam",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))

	var data searchData
	getJSON(t, mux, "/console/search?format=json&q=chroma", &data)

	require.Equal(t, "chroma", data.Query)
	require.Equal(t, "all", data.Scope)
	kinds := map[string]int{}
	for _, g := range data.Groups {
		kinds[g.Kind] = g.Count
	}
	require.Equal(t, 1, kinds["memories"], "the fused leg must find the memory")
	require.Equal(t, 1, kinds["tasks"])
	require.Equal(t, 1, kinds["plans"])
	require.Equal(t, 1, kinds["projects"])
	require.Equal(t, 1, kinds["sessions"])
	require.Equal(t, 5, data.Total)
}

func TestSearch_ScopeNarrowsToOneGroup(t *testing.T) {
	db, mux := newConsole(t)
	seedSearchMemory(t, db, "01MEM", "chroma-boot-race", "chroma health", "seam", "chroma body")
	seedSearchTask(t, db, "01TSK", "seam", "fix the chroma boot race", "")

	var data searchData
	getJSON(t, mux, "/console/search?format=json&q=chroma&scope=tasks", &data)

	require.Len(t, data.Groups, 1)
	require.Equal(t, "tasks", data.Groups[0].Kind)
}

func TestSearch_TimeWindowFiltersKnowledgeAndStructuredRows(t *testing.T) {
	db, mux := newConsole(t)
	now := time.Now().UTC()
	seedSearchMemoryAt(t, db, "01MEMOLD", "window-memory-old", "window topic", "seam", "window topic", now.Add(-48*time.Hour))
	seedSearchMemoryAt(t, db, "01MEMNEW", "window-memory-new", "window topic", "seam", "window topic", now.Add(-time.Hour))
	seedSearchTaskAt(t, db, "01TASKOLD", "seam", "window task old", "", now.Add(-48*time.Hour))
	seedSearchTaskAt(t, db, "01TASKNEW", "seam", "window task new", "", now.Add(-time.Hour))

	var data searchData
	getJSON(t, mux, "/console/search?format=json&q=window&w=24h", &data)
	require.Equal(t, "24h", data.Window)
	require.Equal(t, "past 24 hours", data.WindowLabel)

	ids := map[string]bool{}
	for _, group := range data.Groups {
		for _, row := range group.Rows {
			ids[row.ID] = true
		}
	}
	require.True(t, ids["01MEMNEW"])
	require.True(t, ids["01TASKNEW"])
	require.False(t, ids["01MEMOLD"])
	require.False(t, ids["01TASKOLD"])
}

func TestSearch_PageSortsAcrossEntityGroups(t *testing.T) {
	db, mux := newConsole(t)
	now := time.Now().UTC()
	seedSearchMemoryAt(t, db, "01MEM", "chronology-memory-old", "chronology topic", "alpha", "chronology topic", now.Add(-48*time.Hour))
	seedSearchTaskAt(t, db, "01TASK", "zeta", "chronology task newest", "", now.Add(-time.Hour))

	rr := getSearch(t, mux, "/console/search?q=chronology&sort=newest")
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	taskAt, memoryAt := strings.Index(body, "chronology task newest"), strings.Index(body, "chronology-memory-old")
	require.NotEqual(t, -1, taskAt)
	require.NotEqual(t, -1, memoryAt)
	require.Less(t, taskAt, memoryAt,
		"newest sort must cross the old kind-group boundary")
	require.Contains(t, body, "search-filterbar")
	require.Contains(t, body, ">1y</a>")
	require.Contains(t, body, ">Confidence</option>")
}

// A query below the FTS floor must render the empty state, not run a query that
// cannot match.
func TestSearch_ShortQuerySkipsSearch(t *testing.T) {
	db, mux := newConsole(t)
	seedSearchTask(t, db, "01TSK", "seam", "a chroma task", "")

	var data searchData
	getJSON(t, mux, "/console/search?format=json&q=c", &data)
	require.Empty(t, data.Groups)
	require.Equal(t, 0, data.Total)
}

func TestSearch_RowsCarryPeekableHrefs(t *testing.T) {
	db, mux := newConsole(t)
	seedSearchMemory(t, db, "01MEM", "chroma-boot-race", "chroma health", "seam", "chroma body")

	var data searchData
	getJSON(t, mux, "/console/search?format=json&q=chroma&scope=memories", &data)
	require.Len(t, data.Groups, 1)
	row := data.Groups[0].Rows[0]
	require.Equal(t, "/console/memories/01MEM", row.Href)
	require.True(t, row.Peek)
}

func TestSearch_ExactNoteSlugPromotesAndExposesCanonicalReference(t *testing.T) {
	db, mux := newConsole(t)
	now := time.Now().UTC()
	const noteID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	slug := "deep-code-and-security-audit-2026-07-31"

	seedSearchNote(t, db, noteID, slug, "Deep code and security audit", "audit findings", "seam", "deep code security audit", now.Add(-time.Hour))
	seedSearchMemory(t, db, "01NOISE", "newer-audit-summary", "deep code security audit 2026 07 31", "seam", "deep code security audit 2026 07 31")

	var data searchData
	getJSON(t, mux, "/console/search?format=json&q="+slug, &data)
	require.NotEmpty(t, data.Groups)
	require.Equal(t, "notes", data.Groups[0].Kind,
		"the palette group containing the exact identifier must be promoted")
	row := data.Groups[0].Rows[0]
	require.Equal(t, noteID, row.ID)
	require.Equal(t, slug, row.Identifier)
	require.Equal(t, store.IdentifierMatchExactIdentifier, row.MatchKind)
	require.Equal(t, "/console/notes/"+noteID, row.Href)

	rr := getSearch(t, mux, "/console/search?q="+slug)
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	noteAt := strings.Index(body, "/console/notes/"+noteID)
	noiseAt := strings.Index(body, "/console/memories/01NOISE")
	require.NotEqual(t, -1, noteAt)
	require.NotEqual(t, -1, noiseAt)
	require.Less(t, noteAt, noiseAt)
	require.Contains(t, body, "Exact identifier")
	require.Contains(t, body, `class="search-identifier"`)
}

func TestSearch_KnowledgeIDAndPrefixCarryMatchMetadata(t *testing.T) {
	db, mux := newConsole(t)
	now := time.Now().UTC()
	const (
		first  = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
		second = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
	)
	seedSearchNote(t, db, first, "first-note", "First note", "d", "seam", "body", now)
	seedSearchNote(t, db, second, "second-note", "Second note", "d", "seam", "body", now.Add(-time.Minute))

	var exact searchData
	getJSON(t, mux, "/console/search?format=json&scope=notes&q="+strings.ToLower(first), &exact)
	require.Len(t, exact.Groups, 1)
	require.Len(t, exact.Groups[0].Rows, 1)
	require.Equal(t, store.IdentifierMatchExactID, exact.Groups[0].Rows[0].MatchKind)
	require.Equal(t, first, exact.Groups[0].Rows[0].MatchedID)

	var prefix searchData
	getJSON(t, mux, "/console/search?format=json&scope=notes&q=01ARZ3ND", &prefix)
	require.Len(t, prefix.Groups, 1)
	require.Len(t, prefix.Groups[0].Rows, 2)
	for _, row := range prefix.Groups[0].Rows {
		require.Equal(t, store.IdentifierMatchIDPrefix, row.MatchKind)
		require.Equal(t, row.ID, row.MatchedID)
	}

	var short searchData
	getJSON(t, mux, "/console/search?format=json&scope=notes&q=01ARZ3N", &short)
	require.Empty(t, short.Groups)
}

func TestSearch_FTSHitCarriesMarkedSnippet(t *testing.T) {
	db, mux := newConsole(t)
	seedSearchMemory(t, db, "01MEM", "boot-race", "the description", "seam",
		"the daemon hits a chroma boot race on cold start")

	var data searchData
	getJSON(t, mux, "/console/search?format=json&q=chroma&scope=memories", &data)
	require.Len(t, data.Groups, 1)
	require.Contains(t, string(data.Groups[0].Rows[0].SnippetHTML), "<mark>chroma</mark>")
}

// The one place raw item text becomes HTML. A memory body carrying markup must
// come back escaped, with only our own <mark> live.
func TestSearch_SnippetEscapesItemMarkup(t *testing.T) {
	db, mux := newConsole(t)
	seedSearchMemory(t, db, "01XSS", "xss-fixture", "d", "seam",
		"chroma <script>alert(1)</script> payload")

	rr := getSearch(t, mux, "/console/search?q=chroma&scope=memories")
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	require.NotContains(t, body, "<script>alert(1)", "item markup must never render live")
	require.Contains(t, body, "&lt;script&gt;")
	require.Contains(t, body, "<mark>chroma</mark>", "our own highlight must survive escaping")
}

// A semantic hit carries its cosine similarity as a percentage -- both in the
// JSON contract and rendered on the page -- so the observer can see where
// relevance falls off. The fixture's cosine (0.6) also sits above the default
// semantic floor, pinning that an above-floor hit survives it end to end.
func TestSearch_SemanticHitCarriesSimilarityPercent(t *testing.T) {
	db, mux := newSemanticConsole(t, []float32{1, 0, 0})
	seedSearchMemory(t, db, "01SEM", "vector-thing", "unrelated words entirely", "seam", "nothing lexical here")
	require.NoError(t, store.UpsertEmbedding(context.Background(), db, "01SEM", "memory", "test-embed",
		[]float32{0.6, 0.8, 0}))

	var data searchData
	getJSON(t, mux, "/console/search?format=json&q=quantum+flux&scope=memories", &data)
	require.Len(t, data.Groups, 1)
	require.Equal(t, 60, data.Groups[0].Rows[0].Similarity)

	rr := getSearch(t, mux, "/console/search?q=quantum+flux&scope=memories")
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()
	require.Contains(t, body, "60%")
	require.Contains(t, body, `class="search-score good"`)
	require.Contains(t, body, `<meter min="0" max="100" value="60">`)
	require.Contains(t, body, "Match")
}

// A lexical-only hit has no vector distance: its row must omit the similarity
// cell rather than render a zero.
func TestSearch_LexicalHitOmitsSimilarity(t *testing.T) {
	db, mux := newConsole(t)
	seedSearchMemory(t, db, "01MEM", "boot-race", "daemon health", "seam", "chroma body")

	var data searchData
	getJSON(t, mux, "/console/search?format=json&q=chroma&scope=memories", &data)
	require.Len(t, data.Groups, 1)
	require.Zero(t, data.Groups[0].Rows[0].Similarity)

	rr := getSearch(t, mux, "/console/search?q=chroma&scope=memories")
	require.NotContains(t, rr.Body.String(), "semantic similarity",
		"a lexical-only page must not render the similarity tooltip span")
	require.Contains(t, rr.Body.String(), "Text match")
}

func TestSortSearchRows(t *testing.T) {
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	original := []searchRow{
		{ID: "text", Title: "Text", Project: "zeta", Updated: base.Add(-2 * time.Hour), Lexical: true},
		{ID: "low", Title: "Low", Project: "alpha", Updated: base.Add(-time.Hour), Similarity: 41},
		{ID: "high", Title: "High", Project: "beta", Updated: base, Similarity: 82},
	}
	ids := func(rows []searchRow) []string {
		out := make([]string, len(rows))
		for i, row := range rows {
			out[i] = row.ID
		}
		return out
	}
	clone := func() []searchRow { return append([]searchRow(nil), original...) }

	rows := clone()
	sortSearchRows(rows, "newest")
	require.Equal(t, []string{"high", "low", "text"}, ids(rows))

	rows = clone()
	sortSearchRows(rows, "oldest")
	require.Equal(t, []string{"text", "low", "high"}, ids(rows))

	rows = clone()
	sortSearchRows(rows, "project")
	require.Equal(t, []string{"low", "high", "text"}, ids(rows))

	rows = clone()
	sortSearchRows(rows, "confidence")
	require.Equal(t, []string{"high", "low", "text"}, ids(rows))
}

func TestSortSearchRows_RelevancePromotesIdentifiersWithinFavoritePartitions(t *testing.T) {
	rows := []searchRow{
		{ID: "plain", Title: "Plain"},
		{ID: "exact", Title: "Exact", MatchKind: store.IdentifierMatchExactIdentifier},
		{ID: "starred-plain", Title: "Starred plain", Favorite: true},
		{ID: "starred-prefix", Title: "Starred prefix", Favorite: true, MatchKind: store.IdentifierMatchIDPrefix},
	}

	sortSearchRows(rows, "relevance")
	require.Equal(t, []string{"starred-prefix", "starred-plain", "exact", "plain"},
		[]string{rows[0].ID, rows[1].ID, rows[2].ID, rows[3].ID})
}

// The palette script loads on every page including the login screen, so its
// asset must be public like the stylesheet -- an auth redirect here would 303 an
// HTML login page into a <script> tag.
func TestSearchJS_IsPublicAsset(t *testing.T) {
	mux := newTestMux(t)
	rr := do(mux, httptest.NewRequest(http.MethodGet, "/console/static/search.js", nil))
	require.Equal(t, http.StatusOK, rr.Code)
	require.Contains(t, rr.Header().Get("Content-Type"), "javascript")
	require.Contains(t, rr.Body.String(), "cmdk")
}

// highlightSnippet is the audited escape-then-substitute helper. The order is
// the whole security property, so pin it directly.
func TestHighlightSnippet(t *testing.T) {
	t.Run("sentinels become marks", func(t *testing.T) {
		got := highlightSnippet("a \x01hit\x02 here")
		require.Equal(t, `a <mark>hit</mark> here`, string(got))
	})
	t.Run("item markup is escaped", func(t *testing.T) {
		got := highlightSnippet("<script>alert(1)</script> \x01x\x02")
		require.Equal(t, `&lt;script&gt;alert(1)&lt;/script&gt; <mark>x</mark>`, string(got))
	})
	t.Run("an embedded literal sentinel is inert", func(t *testing.T) {
		// A writer who plants a sentinel in their own body can only produce a
		// stray, harmless <mark> -- never an injection, because the escape
		// already neutered every angle bracket.
		got := highlightSnippet("evil \x01<img src=x onerror=alert(1)>\x02")
		require.NotContains(t, string(got), "<img")
		require.Contains(t, string(got), "&lt;img")
	})
	t.Run("empty stays empty", func(t *testing.T) {
		require.Equal(t, "", string(highlightSnippet("")))
	})
}
