package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console/browserproof"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/passkey"
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

// sxStar stars one row of table (tasks, memories_index, notes_index, ...).
func sxStar(t *testing.T, db *sql.DB, table, id string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `UPDATE `+table+` SET favorite = 1 WHERE id = ?`, id)
	require.NoError(t, err)
}

// sxRetire supersedes a memory by successor ("" archives it).
func sxRetire(t *testing.T, db *sql.DB, id, successor string) {
	t.Helper()
	var next any
	if successor != "" {
		next = successor
	}
	_, err := db.ExecContext(context.Background(), `UPDATE memories_index SET invalid_at = ?, superseded_by = ? WHERE id = ?`,
		core.FormatTime(time.Now().UTC()), next, id)
	require.NoError(t, err)
}

// sxGroup returns the group of kind, failing when it is absent.
func sxGroup(t *testing.T, data searchData, kind string) searchGroup {
	t.Helper()
	for _, g := range data.Groups {
		if g.Kind == kind {
			return g
		}
	}
	require.FailNow(t, "no group", "kind %s in %+v", kind, data.Groups)
	return searchGroup{}
}

func sxHasGroup(data searchData, kind string) bool {
	for _, g := range data.Groups {
		if g.Kind == kind {
			return true
		}
	}
	return false
}

func sxIDs(g searchGroup) []string {
	out := make([]string, len(g.Rows))
	for i, r := range g.Rows {
		out[i] = r.ID
	}
	return out
}

// A kind with more matches than the page shows says so: its count is a floor
// ("20+"), its Show more opens the rest, and the header total carries the
// same "+". The page used to pass its per-kind cap off as the count.
func TestSxSearch_CutKindSaysPlusAndShowMoreRevealsTheRest(t *testing.T) {
	db, mux := newConsole(t)
	now := time.Now().UTC()
	for i := range 25 {
		seedSearchTaskAt(t, db, fmt.Sprintf("01SXTASK%02d", i), "seam", fmt.Sprintf("honest count task %02d", i), "", now.Add(-time.Duration(i)*time.Minute))
	}

	var data searchData
	getJSON(t, mux, "/console/search?format=json&scope=tasks&q=honest", &data)
	tasks := sxGroup(t, data, "tasks")
	require.Equal(t, 20, tasks.Count)
	require.Len(t, tasks.Rows, 20)
	require.True(t, tasks.More, "25 matches behind a 20-row page is a cut kind")
	require.True(t, data.Partial)
	require.Equal(t, "/console/search?scope=tasks&w=all&sort=relevance&more=tasks&q=honest", tasks.MoreHref)

	page := getHTMLBody(t, mux, "/console/search?scope=tasks&q=honest")
	require.Contains(t, page, "<strong>20+</strong>matches")
	require.Contains(t, page, "<b>20+</b> Tasks")
	require.Contains(t, page, `<a class="search-more" href="/console/search?scope=tasks&amp;w=all&amp;sort=relevance&amp;more=tasks&amp;q=honest">Show more</a>`)

	var more searchData
	getJSON(t, mux, tasks.MoreHref+"&format=json", &more)
	all := sxGroup(t, more, "tasks")
	require.Equal(t, 25, all.Count, "Show more reveals the rest")
	require.False(t, all.More)
	require.Empty(t, all.MoreHref, "no Show more that would change nothing")
	require.Equal(t, []string{"tasks"}, more.Expanded)
	require.False(t, more.Partial)
	page = getHTMLBody(t, mux, tasks.MoreHref)
	require.Contains(t, page, "<b>25</b> Tasks")
	require.NotContains(t, page, "Show more")
}

// Memories and notes are counted apart: a query that matches many memories no
// longer squeezes the notes out of one shared cap, and each kind says 20+ for
// itself.
func TestSxSearch_KnowledgeKindsCountApart(t *testing.T) {
	db, mux := newConsole(t)
	now := time.Now().UTC()
	for i := range 25 {
		seedSearchMemoryAt(t, db, fmt.Sprintf("01SXMEM%02d", i), fmt.Sprintf("apart-memory-%02d", i),
			"apart apart apart", "seam", "apart apart apart apart", now.Add(-time.Duration(i)*time.Minute))
	}
	for i := range 3 {
		seedSearchNote(t, db, fmt.Sprintf("01SXNOTE%02d", i), fmt.Sprintf("apart-note-%02d", i), "A note", "apart",
			"seam", "only once apart", now)
	}

	var data searchData
	getJSON(t, mux, "/console/search?format=json&q=apart", &data)
	mems := sxGroup(t, data, "memories")
	require.Equal(t, 20, mems.Count)
	require.True(t, mems.More)
	notes := sxGroup(t, data, "notes")
	require.Equal(t, 3, notes.Count, "the notes keep their own count")
	require.False(t, notes.More)

	var more searchData
	getJSON(t, mux, mems.MoreHref+"&format=json", &more)
	require.Equal(t, 25, sxGroup(t, more, "memories").Count)
	require.Equal(t, 3, sxGroup(t, more, "notes").Count)
}

// Starred only runs inside the queries, before the cap: a starred hit ranked
// 30th still appears under Starred only (it used to filter the first 20). And
// without the filter a starred match takes its kind's slots first.
func TestSxSearch_StarredHitAtRankThirtyAppearsUnderStarredOnly(t *testing.T) {
	db, mux := newConsole(t)
	now := time.Now().UTC()
	for i := range 30 {
		seedSearchTaskAt(t, db, fmt.Sprintf("01SXSTAR%02d", i), "seam", fmt.Sprintf("deep starred task %02d", i), "", now.Add(-time.Duration(i)*time.Minute))
		seedSearchMemoryAt(t, db, fmt.Sprintf("01SXSMEM%02d", i), fmt.Sprintf("deep-starred-memory-%02d", i),
			"deep starred", "seam", "deep starred", now.Add(-time.Duration(i)*time.Minute))
	}
	// The oldest of each: rank 30 under every order the page has.
	sxStar(t, db, "tasks", "01SXSTAR29")
	sxStar(t, db, "memories_index", "01SXSMEM29")

	var plain searchData
	getJSON(t, mux, "/console/search?format=json&q=deep+starred", &plain)
	require.Equal(t, "01SXSTAR29", sxGroup(t, plain, "tasks").Rows[0].ID, "a starred match takes the first slot")
	require.Equal(t, "01SXSMEM29", sxGroup(t, plain, "memories").Rows[0].ID)

	var starred searchData
	getJSON(t, mux, "/console/search?format=json&fav=1&q=deep+starred", &starred)
	require.Equal(t, []string{"01SXSTAR29"}, sxIDs(sxGroup(t, starred, "tasks")))
	require.Equal(t, []string{"01SXSMEM29"}, sxIDs(sxGroup(t, starred, "memories")))
	require.Equal(t, 2, starred.Total)
	require.False(t, starred.Partial)
}

// A one-character query has its own state: it says to type more, never
// "Nothing matches".
func TestSxSearch_OneCharacterQueryHasItsOwnState(t *testing.T) {
	db, mux := newConsole(t)
	seedSearchTask(t, db, "01TSK", "seam", "a chroma task", "")

	page := getHTMLBody(t, mux, "/console/search?q=c")
	require.Contains(t, page, "Type at least 2 characters to search.")
	require.NotContains(t, page, "Nothing matches")
	require.NotContains(t, page, "mv2-count", "no match count for a query that never ran")

	var data searchData
	getJSON(t, mux, "/console/search?format=json&q=c", &data)
	require.True(t, data.TooShort)
	require.Empty(t, data.Groups)

	var start searchData
	getJSON(t, mux, "/console/search?format=json", &start)
	require.False(t, start.TooShort, "an empty query is the start page, not a short one")
}

// A superseded memory is found by its exact name, badged with what replaced
// it; an archived one by its id. Text finds a retired memory only with Include
// archived.
func TestSxSearch_RetiredMemoryFoundByExactNameWithItsBadge(t *testing.T) {
	db, mux := newConsole(t)
	seedSearchMemory(t, db, "01SXOLD", "boot-order", "the old boot order", "seam", "start the daemon after the index")
	seedSearchMemory(t, db, "01SXNEW", "boot-order-v2", "the new boot order", "seam", "start the index after the daemon")
	sxRetire(t, db, "01SXOLD", "01SXNEW")
	seedSearchMemory(t, db, "01SXGONE", "dropped-flag", "a flag nobody sets", "seam", "the zeppelin flag")
	sxRetire(t, db, "01SXGONE", "")

	var data searchData
	getJSON(t, mux, "/console/search?format=json&q=boot-order", &data)
	retired := sxGroup(t, data, searchRetiredKind)
	require.Equal(t, "memories", retired.Scope)
	require.Equal(t, []string{"01SXOLD"}, sxIDs(retired))
	row := retired.Rows[0]
	require.Equal(t, "superseded", row.Retired)
	require.Equal(t, &searchRef{ID: "01SXNEW", Name: "boot-order-v2", Href: "/console/memories/01SXNEW"}, row.SupersededBy)
	require.Equal(t, store.IdentifierMatchExactIdentifier, row.MatchKind)
	require.Equal(t, "/console/memories/01SXOLD", row.Href)
	require.Contains(t, sxIDs(sxGroup(t, data, "memories")), "01SXNEW", "the successor still answers as live knowledge")

	page := getHTMLBody(t, mux, "/console/search?q=boot-order")
	card := between(t, page, "boot-order", `<article class="search-result`, "</article>")
	require.Contains(t, card, `data-retired="superseded"`)
	require.Contains(t, card, `<span class="badge warn search-retired">superseded</span>`)
	require.Contains(t, card, `by <a href="/console/memories/01SXNEW" data-peek>boot-order-v2</a>`)

	var byID searchData
	getJSON(t, mux, "/console/search?format=json&q=01sxgone", &byID)
	require.Equal(t, "archived", sxGroup(t, byID, searchRetiredKind).Rows[0].Retired, "an exact id finds it too")
	require.Nil(t, sxGroup(t, byID, searchRetiredKind).Rows[0].SupersededBy)

	// By text: only with Include archived.
	var text, archived searchData
	getJSON(t, mux, "/console/search?format=json&q=zeppelin", &text)
	require.Empty(t, text.Groups)
	getJSON(t, mux, "/console/search?format=json&archived=1&q=zeppelin", &archived)
	require.True(t, archived.Archived)
	require.Equal(t, []string{"01SXGONE"}, sxIDs(sxGroup(t, archived, searchRetiredKind)))
	require.Contains(t, string(sxGroup(t, archived, searchRetiredKind).Rows[0].SnippetHTML), "<mark>zeppelin</mark>")

	// Scoped to notes, no memory of any kind answers.
	var notes searchData
	getJSON(t, mux, "/console/search?format=json&scope=notes&archived=1&q=boot-order", &notes)
	require.False(t, sxHasGroup(notes, searchRetiredKind))
}

// The project fence holds for retired memories: a locked project's superseded
// memory is absent by exact name and by text, while an open project's is
// found the same way.
func TestSxSearch_LockedProjectRetiredMemoryStaysAbsent(t *testing.T) {
	lc := newLockedConsole(t)
	open := writeMemory(t, lc.mgr, core.KindGotcha, openSlug, "ops-retired", "the old ops note "+openMarker)
	sxRetire(t, lc.db, open.ID, "")
	sxRetire(t, lc.db, lc.memID, "")

	answer := func(target string) searchData {
		code, body := lc.get(t, target, "")
		require.Equal(t, http.StatusOK, code, target)
		var data searchData
		require.NoError(t, json.Unmarshal([]byte(body), &data), target)
		return data
	}
	for _, target := range []string{
		"/console/search?format=json&q=ledger-close",
		"/console/search?format=json&q=" + lc.memID,
		"/console/search?format=json&archived=1&q=" + lockedSecret,
		"/console/search?format=json&archived=1&q=ledger",
	} {
		data := answer(target)
		require.False(t, sxHasGroup(data, searchRetiredKind), target)
		require.Empty(t, data.Groups, target)
	}
	_, page := lc.get(t, "/console/search?archived=1&q=ledger", "")
	require.NotContains(t, page, "/console/memories/"+lc.memID)
	require.NotContains(t, page, "the close runs")

	require.Equal(t, []string{open.ID}, sxIDs(sxGroup(t, answer("/console/search?format=json&q=ops-retired"), searchRetiredKind)),
		"the open project's retired memory is found")
	require.Equal(t, []string{open.ID}, sxIDs(sxGroup(t, answer("/console/search?format=json&archived=1&q="+openMarker), searchRetiredKind)))
}

// skTwoLockedConsole is newLockedConsole's fixture (lockedSlug locked,
// openSlug open) with a second locked project, skSealedSlug, which no browser
// in these tests unlocks.
const (
	skSealedSlug   = "example-hr"
	skSealedSecret = "SEALED-SECRET-4K"
)

func skTwoLockedConsole(t *testing.T) *lockedConsole {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(filepath.Join(dir, "data"), db, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	locked := []string{lockedSlug, skSealedSlug}
	for _, slug := range []string{lockedSlug, skSealedSlug, openSlug} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	for _, slug := range locked {
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, core.IsolationConfidential))
	}
	key, err := browserproof.NewKey()
	require.NoError(t, err)
	lc := &lockedConsole{db: db, mgr: mgr, key: key}
	lc.policy, err = access.New(config.PeerAuth{Owner: "owner", ForceLocked: locked}, func(n string) (access.Account, error) {
		if n == "owner" {
			return access.Account{UID: 501, Name: "owner"}, nil
		}
		return access.Account{}, fmt.Errorf("no %s", n)
	})
	require.NoError(t, err)
	views, err := store.OpenViews(path, lc.policy.OwnerLocked())
	require.NoError(t, err)
	t.Cleanup(func() { _ = views.Close() })
	base, err := views.For(nil)
	require.NoError(t, err)
	v, err := passkey.New(passkey.Config{RPID: "localhost", Origins: []string{approverOrigin}})
	require.NoError(t, err)
	lc.rec = events.NewRecorder(db)
	lc.svc = approval.New(approval.Config{DB: db, Passkeys: v, Origins: []string{approverOrigin}, Events: lc.rec, ForceLocked: locked})
	lc.cs, err = New(Config{
		DB: base, WriteDB: db, Files: mgr, Events: lc.rec.ReadingFrom(base),
		Retrieve: retrieve.New(db, nil, config.Defaults().Budgets, nil), DataDir: filepath.Join(dir, "data"),
		APIKey: testKey, Level: "advanced", Approvals: lc.svc, Views: views, ForceLocked: locked,
		Terminal: &TerminalApprover{Account: "_seamless", Seamlessd: "/opt/seamless/bin/seamlessd", Config: "/opt/seamless/etc/seamless.yaml"},
	})
	require.NoError(t, err)
	mux := http.NewServeMux()
	lc.cs.Register(mux)
	owner := lc.policy.OwnerView()
	lc.mux = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := access.FromContext(r.Context()); !ok {
			r = r.WithContext(access.WithView(r.Context(), owner))
		}
		mux.ServeHTTP(w, r)
	})
	return lc
}

// A browser that unlocked one locked project counts each kind whole while
// another locked project's matches outrank everything: the knowledge search
// leaves the still-locked project out before each kind's cap. It used to drop
// those hits after the cap, so a kind came out short, lost its More, or
// vanished.
func TestSkSearch_UnlockedBrowserCountsPastAStillLockedProject(t *testing.T) {
	lc := skTwoLockedConsole(t)
	now := time.Now().UTC()
	for i := range 25 { // the still-locked project: the term three times, every one
		seedSearchMemory(t, lc.db, fmt.Sprintf("01SKLM%02d", i), fmt.Sprintf("sk-sealed-%02d", i), "harbor harbor harbor", skSealedSlug, "harbor harbor harbor "+skSealedSecret)
		seedSearchNote(t, lc.db, fmt.Sprintf("01SKLN%02d", i), fmt.Sprintf("sk-sealed-note-%02d", i), "Sealed note", "harbor harbor harbor", skSealedSlug, "harbor harbor harbor "+skSealedSecret, now)
	}
	for i := range 2 { // the unlocked project
		seedSearchMemory(t, lc.db, fmt.Sprintf("01SKUM%02d", i), fmt.Sprintf("sk-unlocked-%02d", i), "the harbor and the long way round it", lockedSlug, "the harbor and the long way round it, by the old road")
	}
	seedSearchNote(t, lc.db, "01SKUN00", "sk-unlocked-note", "Unlocked note", "d", lockedSlug, "the harbor and the long way round it, by the old road", now)
	for i := range 22 { // the open project
		seedSearchMemory(t, lc.db, fmt.Sprintf("01SKOM%02d", i), fmt.Sprintf("sk-open-%02d", i), "the harbor and the long way round it", openSlug, "the harbor and the long way round it, by the old road")
	}
	for i := range 2 {
		seedSearchNote(t, lc.db, fmt.Sprintf("01SKON%02d", i), fmt.Sprintf("sk-open-note-%02d", i), "Open note", "d", openSlug, "the harbor and the long way round it, by the old road", now)
	}
	cookie := lc.unlockByTerminal(t)
	answer := func(target, unlock string) (searchData, string) {
		t.Helper()
		code, body := lc.get(t, target, unlock)
		require.Equal(t, http.StatusOK, code, target)
		var data searchData
		require.NoError(t, json.Unmarshal([]byte(body), &data), target)
		return data, body
	}
	projects := func(g searchGroup) map[string]int {
		out := map[string]int{}
		for _, r := range g.Rows {
			out[r.Project]++
		}
		return out
	}

	data, raw := answer("/console/search?format=json&q=harbor", cookie)
	mems, notes := sxGroup(t, data, "memories"), sxGroup(t, data, "notes")
	require.Equal(t, 20, mems.Count)
	require.True(t, mems.More, "24 visible memories behind a 20-row page")
	require.Equal(t, 3, notes.Count, "every visible note, the unlocked project's included")
	require.False(t, notes.More)
	require.Equal(t, map[string]int{lockedSlug: 1, openSlug: 2}, projects(notes))
	require.True(t, data.Partial)
	require.NotContains(t, raw, skSealedSecret)
	require.NotContains(t, raw, skSealedSlug)

	more, _ := answer(mems.MoreHref+"&format=json", cookie)
	all := sxGroup(t, more, "memories")
	require.Equal(t, 24, all.Count, "Show more reveals every visible memory")
	require.False(t, all.More)
	require.Equal(t, map[string]int{lockedSlug: 2, openSlug: 22}, projects(all))

	_, page := lc.get(t, "/console/search?q=harbor", cookie)
	require.Contains(t, page, "<b>20+</b> Memories")
	require.Contains(t, page, "<b>3</b> Notes")
	require.NotContains(t, page, skSealedSecret)

	// Another browser reads neither locked project, and counts the same way.
	plain, _ := answer("/console/search?format=json&q=harbor", "")
	require.Equal(t, 20, sxGroup(t, plain, "memories").Count)
	require.Equal(t, map[string]int{openSlug: 2}, projects(sxGroup(t, plain, "notes")))

	// The retired memories read the request's handle, on which the still-locked
	// project does not exist: by name and by text, only the unlocked project's.
	sxRetire(t, lc.db, "01SKLM00", "")
	sxRetire(t, lc.db, "01SKUM00", "")
	gone, raw := answer("/console/search?format=json&q=sk-sealed-00", cookie)
	require.False(t, sxHasGroup(gone, searchRetiredKind))
	require.NotContains(t, raw, skSealedSecret)
	byName, _ := answer("/console/search?format=json&q=sk-unlocked-00", cookie)
	require.Equal(t, []string{"01SKUM00"}, sxIDs(sxGroup(t, byName, searchRetiredKind)))
	byText, raw := answer("/console/search?format=json&archived=1&q=harbor", cookie)
	require.Equal(t, []string{"01SKUM00"}, sxIDs(sxGroup(t, byText, searchRetiredKind)))
	require.NotContains(t, raw, skSealedSecret)
}

// An empty page lists each active restriction as a chip that removes it, and
// each one changes the result; with two or more, one link clears them all. A
// page with no restriction offers no link at all.
func TestSxSearch_EmptyStateChipsEachChangeTheResult(t *testing.T) {
	db, mux := newConsole(t)
	now := time.Now().UTC()
	// Each kept out by exactly one restriction of scope=notes&w=24h&fav=1:
	seedSearchTaskAt(t, db, "01SXKIND", "seam", "quokka by kind", "", now) // a task, starred, recent
	sxStar(t, db, "tasks", "01SXKIND")
	seedSearchNote(t, db, "01SXOLDN", "quokka-old", "Quokka old", "d", "seam", "quokka", now.Add(-72*time.Hour)) // old
	sxStar(t, db, "notes_index", "01SXOLDN")
	seedSearchNote(t, db, "01SXPLAIN", "quokka-plain", "Quokka plain", "d", "seam", "quokka", now) // not starred

	const base = "/console/search?scope=notes&w=24h&sort=relevance&fav=1&q=quokka"
	var data searchData
	getJSON(t, mux, base+"&format=json", &data)
	require.Empty(t, data.Groups)

	page := getHTMLBody(t, mux, base)
	require.Contains(t, page, "Nothing matches &ldquo;quokka&rdquo; with these filters.")
	require.NotContains(t, page, "Search all time")
	chips := map[string]string{
		"Notes":                        "/console/search?scope=all&w=24h&sort=relevance&fav=1&q=quokka",
		"Updated in the past 24 hours": "/console/search?scope=notes&w=all&sort=relevance&fav=1&q=quokka",
		"Starred only":                 "/console/search?scope=notes&w=24h&sort=relevance&q=quokka",
	}
	for label, href := range chips {
		escaped := strings.ReplaceAll(href, "&", "&amp;")
		require.Contains(t, page, `<a class="search-chip" href="`+escaped+`" aria-label="Remove the filter: `+label+`"`, label)
		var after searchData
		getJSON(t, mux, href+"&format=json", &after)
		require.NotEmpty(t, after.Groups, "removing %q changes the result", label)
	}
	require.Contains(t, page, `<a class="search-chip-clear" href="/console/search?scope=all&amp;w=all&amp;sort=relevance&amp;q=quokka">Clear all filters</a>`)
	var cleared searchData
	getJSON(t, mux, "/console/search?scope=all&w=all&sort=relevance&q=quokka&format=json", &cleared)
	require.Equal(t, 3, cleared.Total)

	// One restriction: its chip, no clear-all.
	page = getHTMLBody(t, mux, "/console/search?scope=sessions&q=quokka")
	require.Contains(t, page, "with this filter.")
	require.Contains(t, page, `aria-label="Remove the filter: Sessions"`)
	require.NotContains(t, page, "Clear all filters")

	// No restriction, nothing found: no link that would change nothing.
	page = getHTMLBody(t, mux, "/console/search?q=wombat")
	require.Contains(t, page, "Nothing matches &ldquo;wombat&rdquo;.")
	require.NotContains(t, page, "search-chip")
	require.NotContains(t, page, "Include archived</a></div>")

	// What only an archived memory matches: the page offers to include them.
	seedSearchMemory(t, db, "01SXWOMBAT", "wombat-notes", "old wombat notes", "seam", "wombat")
	sxRetire(t, db, "01SXWOMBAT", "")
	page = getHTMLBody(t, mux, "/console/search?q=wombat")
	require.Contains(t, page, `1 archived or superseded memory matches. <a href="/console/search?scope=all&amp;w=all&amp;sort=relevance&amp;archived=1&amp;q=wombat">Include archived</a>`)
	getJSON(t, mux, "/console/search?scope=all&w=all&sort=relevance&archived=1&q=wombat&format=json", &data)
	require.Equal(t, 1, data.Total)
}

// Show more is part of the URL, so it survives the live refresh (which
// refetches the URL) and every control that keeps the search: the type,
// window, starred and archived links and the sort form. A new query starts
// with it closed.
func TestSxSearch_ShowMoreSurvivesTheControls(t *testing.T) {
	db, mux := newConsole(t)
	for i := range 3 {
		seedSearchTask(t, db, fmt.Sprintf("01SXKEEP%02d", i), "seam", "kept open task", "")
	}
	page := getHTMLBody(t, mux, "/console/search?scope=all&w=all&sort=relevance&more=tasks,sessions&q=kept")
	for _, href := range []string{
		"/console/search?scope=tasks&amp;w=all&amp;sort=relevance&amp;more=tasks%2Csessions&amp;q=kept",              // a type pill
		"/console/search?scope=all&amp;w=7d&amp;sort=relevance&amp;more=tasks%2Csessions&amp;q=kept",                 // a window pill
		"/console/search?scope=all&amp;w=all&amp;sort=relevance&amp;fav=1&amp;more=tasks%2Csessions&amp;q=kept",      // Starred only
		"/console/search?scope=all&amp;w=all&amp;sort=relevance&amp;archived=1&amp;more=tasks%2Csessions&amp;q=kept", // Include archived
	} {
		require.Contains(t, page, `href="`+href+`"`)
	}
	sortForm := between(t, page, `<label for="search-sort">`, `<form class="search-sort"`, "</form>")
	require.Contains(t, sortForm, `<input type="hidden" name="more" value="tasks,sessions">`)
	queryForm := between(t, page, `<button class="search-submit"`, `<form class="search search-query"`, "</form>")
	require.NotContains(t, queryForm, `name="more"`)

	var data searchData
	getJSON(t, mux, "/console/search?format=json&more=sessions,tasks&q=kept", &data)
	require.Equal(t, []string{"tasks", "sessions"}, data.Expanded, "one state, one order")
}

func TestSxSearch_BadMoreAndArchivedAreRejected(t *testing.T) {
	_, mux := newConsoleFeatures(t, config.Features{})
	for _, tc := range []struct{ path, want string }{
		{"/console/search?q=x1&more=bogus", "invalid more"},
		{"/console/search?q=x1&more=", "invalid more"},
		{"/console/search?q=x1&more=tasks,tasks", "named twice"},
		{"/console/search?q=x1&more=tasks,", "invalid more"},
		{"/console/search?q=x1&more=trials", "invalid more"}, // research is off
		{"/console/search?q=x1&archived=yes", "invalid archived"},
		{"/console/search?q=x1&archived=", "invalid archived"},
		{"/console/search?q=x1&more=tasks&more=notes", "exactly once"},
	} {
		rr := getSearch(t, mux, tc.path)
		require.Equal(t, http.StatusBadRequest, rr.Code, tc.path)
		require.Contains(t, rr.Body.String(), tc.want, tc.path)
	}
	require.Equal(t, http.StatusOK, getSearch(t, mux, "/console/search?q=x1&more=retired,memories&archived=1").Code)
}

// The palette's preview is cut at five and says so: the group carries more
// and the page that shows the rest -- Search narrowed to the kind, archived
// memories included for theirs.
func TestSxSearch_PaletteGroupsSayWhenCut(t *testing.T) {
	db, mux := newConsole(t)
	for i := range 7 {
		seedSearchTask(t, db, fmt.Sprintf("01SXPAL%02d", i), "seam", "palette preview task", "")
	}
	seedSearchTask(t, db, "01SXPALS", "seam", "palette preview starred", "")
	sxStar(t, db, "tasks", "01SXPALS")

	var data searchData
	getJSON(t, mux, "/console/search?format=json&fast=1&q=palette+preview", &data)
	tasks := sxGroup(t, data, "tasks")
	require.Equal(t, 5, tasks.Count)
	require.True(t, tasks.More)
	require.Equal(t, "tasks", tasks.Scope)
	require.Equal(t, "/console/search?scope=tasks&w=all&sort=relevance&q=palette%20preview", tasks.MoreHref)
	require.Equal(t, "01SXPALS", tasks.Rows[0].ID, "the starred match leads the preview")
	require.True(t, tasks.Rows[0].Favorite)

	// The JSON keys the palette reads.
	raw := getPeek(t, mux, "/console/search?format=json&fast=1&q=palette+preview").Body.String()
	for _, key := range []string{`"more": true`, `"moreHref": `, `"scope": "tasks"`, `"favorite": true`} {
		require.Contains(t, raw, key)
	}
}

// search.js mirrors the group contract above: it shows a cut count as "5+",
// ends a cut group with "More in Search" to its moreHref, files the retired
// memories under their scope, and marks starred and retired rows.
func TestSxSearchJS_MirrorsCutGroups(t *testing.T) {
	js := string(searchJS)
	for _, want := range []string{
		"g.count + (g.more ? '+' : '')",
		"if (g.more && g.moreHref) items.push({ title: 'More in Search'",
		"scopes.indexOf(g.scope || g.kind)",
		"if (r.favorite) bits.push('starred');",
		"if (r.retired) bits.push(r.supersededBy ? r.retired + ' by ' + r.supersededBy.name : r.retired);",
		"(r.more ? ' cmdk-more' : '')",
	} {
		require.Contains(t, js, want)
	}
	require.NotContains(t, js, "scopes.indexOf(g.kind) === -1", "every group check goes through its scope")
}
