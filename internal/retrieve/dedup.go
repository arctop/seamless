package retrieve

import (
	"context"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/store"
)

// dedupMinScore is the cosine-similarity floor above which a proposed memory is
// flagged as a possible duplicate. Ported from v1 (0.83).
const dedupMinScore = 0.83

// DedupHint returns the most semantically similar active memory to a proposed
// (name, description) within the project+global scope, when it clears the
// similarity threshold. It is advisory only -- memory_write always proceeds --
// and semantic-only: with no embedder, or on any embed hiccup, it returns nil so
// a write is never blocked by retrieval trouble. Ported from v1's dedupHint.
func (s *Service) DedupHint(ctx context.Context, project, name, description string) (*Hit, error) {
	if s.embedder == nil {
		return nil, nil
	}
	query := strings.TrimSpace(name + " " + description)
	if query == "" {
		return nil, nil
	}
	qvec, err := s.embedder.Embed(ctx, query)
	if err != nil {
		s.logger.Warn("retrieve.DedupHint: embed failed, no hint", "error", err)
		return nil, nil
	}
	// Same scope as recall, isolation included: a sealed project may not read
	// global, and a hint naming a global memory would leak one into it.
	globalVisible, err := s.globalReadable(ctx, project)
	if err != nil {
		return nil, err
	}
	scope := []string{project}
	if project != "" && globalVisible {
		scope = append(scope, "")
	}
	hits, err := store.CosineSearch(ctx, s.db, qvec, s.embedder.Model(), []string{"memory"}, scope, 3)
	if err != nil {
		return nil, err
	}
	for _, h := range hits {
		if h.Score < dedupMinScore {
			continue // hits are best-first, so nothing below can clear it either
		}
		m, ok, err := store.MemoryByID(ctx, s.db, h.ItemID)
		if err != nil {
			return nil, err
		}
		// CosineSearch already excludes invalidated memories and out-of-scope
		// ones (the caller's peer_auth reach included); this is the residual
		// guard for a memory superseded between that query and this read.
		if !ok || m.InvalidAt != nil || !scopeVisible(m.Project, project, globalVisible) || !access.Visible(ctx, m.Project) {
			continue
		}
		hit := memoryHit(m)
		hit.Score = h.Score
		hit.Source = "semantic"
		return &hit, nil
	}
	return nil, nil
}
