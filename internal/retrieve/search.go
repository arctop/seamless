package retrieve

import (
	"context"
	"sort"
	"time"

	"github.com/arctop/seamless/internal/access"
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
	kinds := scopeKinds(in.Scope)
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}

	identifierHits, err := store.SearchKnowledgeIdentifiersSince(ctx, s.db, in.Query, kinds, in.Projects, in.Since, searchSourceDepth)
	if err != nil {
		return nil, err
	}
	acc, err := s.candidates(ctx, in.Query, kinds, in.Projects, "", in.Since, searchSourceDepth, in.Semantic, "retrieve.Search")
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
	ordered := rankSearch(acc)

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
		// not surface a project sealed to another OS account.
		if !ok || !access.Visible(ctx, h.Project) {
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
