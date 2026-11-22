package retrieve

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/store"
)

// searchSourceDepth is how many candidates each leg contributes before fusion.
// It is deliberately deeper than recallSourceDepth: a human search can range
// over every project at once, so the candidate set has to be wide enough that a
// large scope does not crowd out a match that ranks mid-pack in its own project.
const searchSourceDepth = 60

// SearchInput parameterizes a human-facing search over durable knowledge.
//
// Projects nil means every project -- the difference that makes this a separate
// entry point from Recall, which is always bound to one session's scope.
// Semantic false runs the FTS leg only, for a caller that fires a query per
// keystroke and must not pay a remote embedding round-trip each time.
type SearchInput struct {
	Query    string
	Scope    string    // all | memories | notes
	Projects []string  // nil = every project
	Limit    int       // default 20
	Semantic bool      // false = FTS-only (the palette's fast path)
	Since    time.Time // zero = all time; otherwise updated at/after this instant
	// PerKind, when set, caps each knowledge kind on its own instead of Limit
	// capping one fused ranking of both: PerKind[core.ItemKindMemory] and
	// PerKind[core.ItemKindNote] (a kind absent or <= 0 takes Limit). Each kind
	// is then ranked by its own candidate legs, at a depth of at least its cap,
	// so one kind's matches never crowd the other's out and a count below the
	// cap is the whole count: a caller asking one more than it shows learns,
	// per kind, whether there are more. The hits come grouped, memories first.
	PerKind map[string]int
	// Starred is how starred items rank (store.StarredFirst, StarredOnly). Both
	// are decided before any cap: the starred items of a kind are ranked on
	// their own, ahead of the rest (StarredFirst) or alone (StarredOnly), so a
	// cap can never cut a starred match in favor of an unstarred one. It needs
	// PerKind; the shared-Limit ranking keeps its own order.
	Starred store.SearchStarred
	// Exclude are projects whose hits are left out before any cap, the way a
	// hit the caller's view may not read is (access.Visible): a caller that
	// reads with a wider view than its answer may show -- the console, for a
	// browser whose unlock opens some locked projects but not all -- names
	// the ones still hidden, so their matches never take a kind's slots and
	// leave its count short. The candidate legs still read them, so they can
	// fill a leg's depth: the caller's view is what keeps a project out of
	// the legs themselves.
	Exclude []string
}

// Search finds memories and notes for a human searching the console. It shares
// Recall's candidate queries, RRF fusion, and embedder-degradation contract (see
// candidates) but deliberately drops three Recall behaviors that are wrong for
// this caller:
//
//   - No token budgeting. Recall packs into an agent's context budget and
//     silently truncates; a search page owes the observer every hit it found, up
//     to the limit they asked for.
//   - No post-fusion scope guard. Recall re-checks each hit against its single
//     bound project, which would discard every project-scoped hit of an
//     all-projects search. Scope here is enforced only where it belongs -- in the
//     candidate queries, via Projects.
//   - No link expansion. [[name]] resolution is single-project by construction,
//     and a linked neighbor does not textually match the query, so it reads as
//     noise to someone who typed a search term.
//
// Snippet is populated for hits the FTS leg found; a semantic-only hit has none
// (there is no matched term to quote), and the caller falls back to the item's
// description.
//
// Search adds one guard Recall does not have: the semantic floor
// (search.semantic_floor). The cosine leg is pure nearest-neighbor -- there is
// always a "nearest" item, however far, so without a floor any query fills the
// page to its limit with noise. A semantic-only hit below the floor is dropped;
// a hit the lexical leg also matched earned its place regardless of distance.
// Recall keeps every neighbor on purpose: an agent can judge a weak hit, an
// observer reads "20 results" as 20 matches. Similarity is set on every hit the
// semantic leg found, so the caller can show where relevance falls off.
func (s *Service) Search(ctx context.Context, in SearchInput) ([]Hit, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if in.PerKind != nil {
		return s.searchPerKind(ctx, in, limit)
	}
	if in.Starred != store.StarredAny {
		return nil, errors.New("retrieve.Search: Starred needs PerKind")
	}

	acc, err := s.searchCandidates(ctx, in, scopeKinds(in.Scope), searchSourceDepth)
	if err != nil {
		return nil, err
	}
	out, err := s.takeHits(ctx, rankSearch(acc), acc, limit, in.Exclude)
	if err != nil {
		return nil, err
	}
	if err := s.attachStamps(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// searchCandidates runs the identifier lane and both candidate legs over kinds
// at depth, and fuses them into one accumulator.
func (s *Service) searchCandidates(ctx context.Context, in SearchInput, kinds []string, depth int) (map[string]*fusedItem, error) {
	identifierHits, err := store.SearchKnowledgeIdentifiersSince(ctx, s.db, in.Query, kinds, in.Projects, in.Since, depth)
	if err != nil {
		return nil, err
	}
	acc, err := s.candidates(ctx, in.Query, kinds, in.Projects, "", in.Since, depth, in.Semantic, "retrieve.Search")
	if err != nil {
		return nil, err
	}
	for i, hit := range identifierHits {
		f := acc[hit.ItemID]
		if f == nil {
			f = &fusedItem{kind: hit.Kind}
			acc[hit.ItemID] = f
		}
		if f.identifierMatch == "" || store.IdentifierMatchPriority(hit.Match) < store.IdentifierMatchPriority(f.identifierMatch) {
			f.identifierMatch = hit.Match
			f.identifierOrder = i
		}
	}
	return acc, nil
}

// takeHits walks ordered (rankSearch's order) and keeps up to limit hits: a
// semantic-only hit below the floor drops, and so does one the caller may not
// read or one in a project it excludes (SearchInput.Exclude) -- before it
// counts toward limit.
func (s *Service) takeHits(ctx context.Context, ordered []string, acc map[string]*fusedItem, limit int, exclude []string) ([]Hit, error) {
	hyd, err := s.hydrate(ctx, ordered, acc)
	if err != nil {
		return nil, err
	}

	// The requested limit can come from a library caller. Capacity only needs to
	// cover the finite candidate set; bounding it here avoids a disproportionate
	// allocation even when the caller supplies an unusually large limit.
	out := make([]Hit, 0, min(limit, len(ordered)))
	for _, id := range ordered {
		if len(out) >= limit {
			break
		}
		f := acc[id]
		if f.identifierMatch == "" && f.semantic && !f.fts && f.cosine < s.search.SemanticFloor {
			continue
		}
		h, ok := hyd.hit(id, f.kind)
		// peer_auth: the identifier leg reads every project, so the caller's
		// reach is applied to the hydrated hit -- the console owner's search must
		// not surface a locked project without a lease.
		if !ok || !access.Visible(ctx, h.Project) || slices.Contains(exclude, h.Project) {
			continue
		}
		if f.identifierMatch != "" {
			h.Source = "identifier"
			h.IdentifierMatch = f.identifierMatch
		} else {
			h.Source = fusedSource(f)
		}
		h.Score = f.score
		h.Snippet = f.snippet
		if f.semantic {
			h.Similarity = f.cosine
		}
		out = append(out, h)
	}
	return out, nil
}

// searchPerKind is Search with PerKind set: each knowledge kind in scope is
// ranked and capped on its own (see SearchInput.PerKind and Starred).
//
// The legs run at a depth of at least the kind's cap. That is what makes a
// short count whole: a kind with fewer matches than its cap has all of them
// inside every leg's depth -- all its lexical matches, and all its semantic
// neighbors above the floor, since the cosine leg is ordered by similarity --
// so the caller can tell "12" from "12 and the depth ran out". Without the
// semantic leg the order is the identifier lane then the FTS rank, which a
// depth of exactly the cap already decides.
func (s *Service) searchPerKind(ctx context.Context, in SearchInput, limit int) ([]Hit, error) {
	run := s
	if s.embedder != nil && in.Semantic {
		// One embedding per search, however many kinds and lanes ask for it.
		cp := *s
		cp.embedder = &onceEmbedder{inner: s.embedder, slot: &cp.embedder}
		run = &cp
	}
	var out []Hit
	for _, kind := range scopeKinds(in.Scope) {
		kindLimit := limit
		if n := in.PerKind[kind]; n > 0 {
			kindLimit = n
		}
		var hits []Hit
		var starred map[string]bool
		if in.Starred != store.StarredAny {
			var err error
			if hits, starred, err = run.searchStarred(ctx, in, kind, kindLimit); err != nil {
				return nil, err
			}
		}
		if in.Starred != store.StarredOnly && len(hits) < kindLimit {
			depth := kindLimit
			if in.Semantic {
				depth = max(searchSourceDepth, kindLimit)
			}
			acc, err := run.searchCandidates(ctx, in, []string{kind}, depth)
			if err != nil {
				return nil, err
			}
			// The starred lane already placed every starred item of this kind.
			for id := range starred {
				delete(acc, id)
			}
			rest, err := run.takeHits(ctx, rankSearch(acc), acc, kindLimit-len(hits), in.Exclude)
			if err != nil {
				return nil, err
			}
			hits = append(hits, rest...)
		}
		out = append(out, hits...)
	}
	if err := s.attachStamps(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// searchStarred ranks one kind's starred items alone: the same identifier
// lane, lexical and semantic legs, and reciprocal-rank fusion as the full
// search, over the starred set only, which is small enough to score whole --
// so no depth can cut a starred match. It returns the hits (up to limit) and
// every starred id of the kind, for the unstarred lane to leave out.
func (s *Service) searchStarred(ctx context.Context, in SearchInput, kind string, limit int) ([]Hit, map[string]bool, error) {
	items, err := store.StarredKnowledge(ctx, s.db, kind, in.Projects, in.Since)
	if err != nil || len(items) == 0 {
		return nil, nil, err
	}
	starred := make(map[string]bool, len(items))
	ids := make([]string, len(items))
	for i, it := range items {
		starred[it.ID], ids[i] = true, it.ID
	}
	acc := make(map[string]*fusedItem)
	entry := func(id string) *fusedItem {
		f := acc[id]
		if f == nil {
			f = &fusedItem{kind: kind}
			acc[id] = f
		}
		return f
	}

	// The identifier lane, in SearchKnowledgeIdentifiersSince's order:
	// strongest match first, then newest.
	type identified struct {
		item  store.StarredItem
		match store.IdentifierMatchKind
	}
	var named []identified
	for _, it := range items {
		m := store.IDIdentifierMatch(in.Query, it.ID)
		if m == "" {
			m = store.NaturalIdentifierMatch(in.Query, it.Identifier)
		}
		if m != "" {
			named = append(named, identified{it, m})
		}
	}
	sort.SliceStable(named, func(i, j int) bool {
		pi, pj := store.IdentifierMatchPriority(named[i].match), store.IdentifierMatchPriority(named[j].match)
		if pi != pj {
			return pi < pj
		}
		if !named[i].item.Updated.Equal(named[j].item.Updated) {
			return named[i].item.Updated.After(named[j].item.Updated)
		}
		return named[i].item.ID < named[j].item.ID
	})
	for i, n := range named {
		f := entry(n.item.ID)
		f.identifierMatch, f.identifierOrder = n.match, i
	}

	lexical, err := store.FTSSearchSnippetsWithin(ctx, s.db, in.Query, ids)
	if err != nil {
		return nil, nil, err
	}
	for rank, h := range lexical {
		f := entry(h.ItemID)
		f.score += 1.0 / float64(rrfK+rank+1)
		f.fts = true
		if f.snippet == "" {
			f.snippet = h.Snippet
		}
	}

	if in.Semantic && s.embedder != nil {
		qvec, err := s.searchVector(ctx, in.Query)
		if err != nil {
			return nil, nil, err
		}
		if qvec != nil {
			vecs, err := store.EmbeddingsFor(ctx, s.db, s.embedder.Model(), ids)
			if err != nil {
				return nil, nil, err
			}
			type scored struct {
				id  string
				cos float64
			}
			near := make([]scored, 0, len(vecs))
			for _, id := range ids {
				if v, ok := vecs[id]; ok && len(v) == len(qvec) {
					near = append(near, scored{id, store.Cosine(qvec, v)})
				}
			}
			sort.Slice(near, func(i, j int) bool {
				if near[i].cos != near[j].cos {
					return near[i].cos > near[j].cos
				}
				return near[i].id < near[j].id
			})
			for rank, n := range near {
				f := entry(n.id)
				f.score += 1.0 / float64(rrfK+rank+1)
				f.semantic, f.cosine = true, n.cos
			}
		}
	}

	hits, err := s.takeHits(ctx, rankSearch(acc), acc, limit, in.Exclude)
	return hits, starred, err
}

// searchVector embeds the query for the starred lane under candidates'
// degradation contract: a local configuration error surfaces, any other
// failure means a lexical-only lane (nil vector). Under searchPerKind the
// embedder is a onceEmbedder, so this asks the provider at most once per
// search, together with the unstarred lane's own call.
func (s *Service) searchVector(ctx context.Context, query string) ([]float32, error) {
	qvec, err := s.embedder.Embed(ctx, query)
	switch {
	case errors.Is(err, llm.ErrConfig):
		return nil, fmt.Errorf("retrieve.Search: %w", err)
	case err != nil:
		s.logger.Warn("retrieve.Search: embed failed, FTS only", "error", err)
		return nil, nil
	default:
		return qvec, nil
	}
}

// onceEmbedder remembers one query's embedding (or its failure), so the
// several lanes of one search cost one provider round-trip. It lives for one
// Search call, on one goroutine. slot is that search's own embedder field: a
// provider failure empties it, so the lane that asked warns about it and the
// lanes after it run lexical-only without asking again (and warning again).
type onceEmbedder struct {
	inner llm.Embedder
	slot  *llm.Embedder
	text  string
	vec   []float32
	err   error
	done  bool
}

func (e *onceEmbedder) Model() string { return e.inner.Model() }

func (e *onceEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if e.done && text == e.text {
		return e.vec, e.err
	}
	e.text, e.done = text, true
	if e.vec, e.err = e.inner.Embed(ctx, text); e.err != nil && !errors.Is(e.err, llm.ErrConfig) {
		*e.slot = nil
	}
	return e.vec, e.err
}

// rankSearch promotes stable-identifier matches ahead of the ordinary RRF
// ordering. The identifier source already sorted exact ids, exact names/slugs,
// id prefixes, and identifier substrings; its ordinal preserves deterministic
// newest/id ordering within each tier. Non-identifier candidates retain the
// exact score/id ordering used by Recall.
func rankSearch(acc map[string]*fusedItem) []string {
	ids := make([]string, 0, len(acc))
	for id := range acc {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := acc[ids[i]], acc[ids[j]]
		pa := store.IdentifierMatchPriority(a.identifierMatch)
		pb := store.IdentifierMatchPriority(b.identifierMatch)
		if pa != pb {
			return pa < pb
		}
		if a.identifierMatch != "" && a.identifierOrder != b.identifierOrder {
			return a.identifierOrder < b.identifierOrder
		}
		if a.score != b.score {
			return a.score > b.score
		}
		return ids[i] < ids[j]
	})
	return ids
}
