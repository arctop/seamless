package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/arctop/seamless/internal/core"
)

// Snippet marks wrap the matched terms inside a SnippetHit.Snippet. They are
// control characters rather than "<mark>" because a snippet is raw item text:
// handing HTML to the caller would make every consumer responsible for telling
// our markup from the item's own. A control char cannot survive HTML escaping as
// markup, so a consumer escapes first and substitutes second (see the console's
// highlightSnippet), and a sentinel a writer embedded in their own body can only
// ever produce a stray inert <mark>, never an injection.
const (
	SnippetStartMark = "\x01"
	SnippetEndMark   = "\x02"
)

// snippetTokens is the target width, in tokens, of a generated snippet: wide
// enough to read the match in context, short enough for a one-line result row.
const snippetTokens = 12

// SnippetHit is an FTS hit carrying the matched text in context. Snippet is the
// raw item text with the matched terms wrapped in SnippetStartMark/SnippetEndMark.
type SnippetHit struct {
	SearchHit
	Snippet string
}

// FTSSearch runs a full-text query over the unified fts table and returns hits
// ordered best-first. Scores are the negated bm25 rank (higher = better) so they
// share the "bigger is better" convention with CosineSearch. An empty kinds
// filter searches all kinds. A query with no usable terms yields no hits (not an
// error), so recall degrades quietly on punctuation-only input.
//
// projects restricts hits to items whose project is in the list (recall passes
// the bound project plus "" for global); an empty filter searches all projects.
// Filtering inside the candidate query keeps the whole candidate depth in scope,
// so a corpus dominated by out-of-scope matches cannot starve in-scope results
// out of the top-limit window.
//
// Superseded and archived memories are excluded for the same reason: the fts
// table is self-contained and keeps a full row for a memory that is no longer
// valid, so validity is resolved by joining memories_index. Filtering it after
// the LIMIT (as callers used to) let retired revisions of a name -- which match
// their replacement's queries almost as well, by construction -- eat the
// candidate depth and starve the live memory that replaced them. An fts row
// with no index row (notes, or an orphan) has nothing to invalidate it and is
// kept.
func FTSSearch(ctx context.Context, db *sql.DB, query string, kinds, projects []string, limit int) ([]SearchHit, error) {
	hits, err := ftsSearch(ctx, db, query, kinds, projects, "", time.Time{}, limit, false)
	if err != nil {
		return nil, err
	}
	if hits == nil {
		return nil, nil
	}
	out := make([]SearchHit, len(hits))
	for i, h := range hits {
		out[i] = h.SearchHit
	}
	return out, nil
}

// FTSSearchSince is FTSSearch restricted to index rows updated at or after
// since. A zero since keeps the unbounded behavior. The predicate lives before
// LIMIT so old, highly-ranked rows cannot crowd in-window matches out of the
// candidate set.
func FTSSearchSince(ctx context.Context, db *sql.DB, query string, kinds, projects []string, since time.Time, limit int) ([]SearchHit, error) {
	hits, err := ftsSearch(ctx, db, query, kinds, projects, "", since, limit, false)
	if err != nil {
		return nil, err
	}
	if hits == nil {
		return nil, nil
	}
	out := make([]SearchHit, len(hits))
	for i, h := range hits {
		out[i] = h.SearchHit
	}
	return out, nil
}

// FTSSearchSnippets is FTSSearch plus a generated snippet per hit: the same
// query, filters, and ordering, with the matched terms marked in context. The
// two share ftsSearch so the validity predicate, the scope filter, and the
// ordering cannot drift between the snippet and no-snippet paths -- callers rely
// on both returning identical hits for identical inputs.
func FTSSearchSnippets(ctx context.Context, db *sql.DB, query string, kinds, projects []string, limit int) ([]SnippetHit, error) {
	return ftsSearch(ctx, db, query, kinds, projects, "", time.Time{}, limit, true)
}

// FTSSearchSnippetsSince is the windowed form of FTSSearchSnippets. See
// FTSSearchSince for why the time predicate is part of the candidate query.
// memKind, when non-empty, restricts hits to memories of that frontmatter kind
// (memories_index.kind); notes have no memories_index row, so a memKind filter
// excludes them regardless of the kinds (item-type) filter.
func FTSSearchSnippetsSince(ctx context.Context, db *sql.DB, query string, kinds, projects []string, memKind string, since time.Time, limit int) ([]SnippetHit, error) {
	return ftsSearch(ctx, db, query, kinds, projects, memKind, since, limit, true)
}

// FTSSearchAllTerms is FTSSearch with AND semantics: a hit must contain every
// term. The OR of ftsQuery maximizes recall for ranking pipelines, but a
// presence probe -- "does knowledge covering all of this exist?" -- needs
// precision instead: with OR, any item sharing one common word would count.
// The gardener's memory-wanted liveness guard is the canonical caller. Terms
// are sanitized exactly like ftsQuery's tokens (split, short tokens dropped,
// quoted); unusable input yields no hits, not an error.
func FTSSearchAllTerms(ctx context.Context, db *sql.DB, terms []string, kinds, projects []string, limit int) ([]SearchHit, error) {
	quoted := make([]string, 0, len(terms))
	for _, term := range terms {
		fields := strings.FieldsFunc(term, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		})
		for _, f := range fields {
			if len([]rune(f)) < 2 {
				continue
			}
			quoted = append(quoted, `"`+f+`"`)
		}
	}
	if len(quoted) == 0 {
		return nil, nil
	}
	// Adjacent quoted terms are an implicit AND in FTS5.
	hits, err := ftsSearchMatch(ctx, db, strings.Join(quoted, " "), kinds, projects, "", time.Time{}, limit, false)
	if err != nil {
		return nil, err
	}
	out := make([]SearchHit, len(hits))
	for i, h := range hits {
		out[i] = h.SearchHit
	}
	return out, nil
}

// ftsSearch is the shared body of the free-text FTS entry points. withSnippet
// adds the snippet() projection; everything else about the query is identical.
func ftsSearch(ctx context.Context, db *sql.DB, query string, kinds, projects []string, memKind string, since time.Time, limit int, withSnippet bool) ([]SnippetHit, error) {
	return ftsSearchMatch(ctx, db, ftsQuery(query), kinds, projects, memKind, since, limit, withSnippet)
}

// ftsSearchMatch runs a pre-built FTS5 MATCH expression. match must already be
// sanitized (quoted terms only); an empty match yields no hits.
func ftsSearchMatch(ctx context.Context, db *sql.DB, match string, kinds, projects []string, memKind string, since time.Time, limit int, withSnippet bool) ([]SnippetHit, error) {
	if limit <= 0 {
		limit = 10
	}
	if match == "" {
		return nil, nil
	}

	// Column -1 lets FTS5 pick the best-matching column for the snippet, so a
	// body hit quotes the body and a name hit quotes the name.
	sel := `fts.item_id, fts.kind, bm25(fts) AS rank`
	var args []any
	if withSnippet {
		sel += `, snippet(fts, -1, ?, ?, ' ... ', ?)`
		args = append(args, SnippetStartMark, SnippetEndMark, snippetTokens)
	}
	sqlStr := `SELECT ` + sel + ` FROM fts
		LEFT JOIN memories_index mi ON mi.id = fts.item_id
		LEFT JOIN notes_index ni ON ni.id = fts.item_id
		WHERE fts MATCH ? AND (mi.id IS NULL OR mi.invalid_at IS NULL)`
	args = append(args, match)
	if len(kinds) > 0 {
		sqlStr += ` AND fts.kind IN (` + placeholders(len(kinds)) + `)`
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	if len(projects) > 0 {
		sqlStr += ` AND fts.project IN (` + placeholders(len(projects)) + `)`
		for _, p := range projects {
			args = append(args, p)
		}
	}
	// The caller's peer_auth reach, whatever scope the caller asked for: this is
	// the one read of the fts table, and it serves the owner's console search as
	// well as agent recall.
	if clause, extra := accessFilter(ctx, "fts.project"); clause != "" {
		sqlStr += clause
		args = append(args, extra...)
	}
	// A NULL mi.kind (a note, or an orphan) fails the predicate, so a memKind
	// filter is memories-only by construction -- applied inside the candidate
	// query, like scope, so the whole depth stays in-kind.
	if memKind != "" {
		sqlStr += ` AND mi.kind = ?`
		args = append(args, memKind)
	}
	if !since.IsZero() {
		sqlStr += ` AND COALESCE(mi.updated_at, ni.updated_at) >= ?`
		args = append(args, core.FormatTime(since.UTC()))
	}
	// item_id is a deterministic tiebreak: equal-bm25 rows would otherwise come
	// back in an undefined order that can flip between runs and destabilize
	// downstream rank fusion.
	sqlStr += ` ORDER BY rank, fts.item_id LIMIT ?`
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("store.ftsSearch: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var hits []SnippetHit
	for rows.Next() {
		var (
			itemID, kind string
			rank         float64
			snippet      string
		)
		dest := []any{&itemID, &kind, &rank}
		if withSnippet {
			dest = append(dest, &snippet)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("store.ftsSearch: scan: %w", err)
		}
		// bm25 returns lower (more negative) for better matches; negate so
		// higher is better, matching SearchHit's convention.
		hits = append(hits, SnippetHit{
			SearchHit: SearchHit{ItemID: itemID, Kind: kind, Score: -rank},
			Snippet:   snippet,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ftsSearch: %w", err)
	}
	return hits, nil
}

// ftsQuery builds a safe FTS5 MATCH expression from free user text: it splits on
// non-alphanumeric runes, drops single-character tokens, double-quotes each
// remaining token (so FTS5 treats it as a literal term, never an operator or a
// column filter), and ORs them. Returns "" when no usable token remains, which
// callers treat as "no results" rather than a syntax error. This is what keeps a
// query like "chroma-boot-race" from being parsed as a subtraction.
func ftsQuery(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	terms := make([]string, 0, len(fields))
	for _, f := range fields {
		if len([]rune(f)) < 2 {
			continue
		}
		terms = append(terms, `"`+f+`"`)
	}
	return strings.Join(terms, " OR ")
}
